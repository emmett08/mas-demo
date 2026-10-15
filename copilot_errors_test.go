package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestClassifyCopilotFailure(t *testing.T) {
	exitErr := exec.Command(fakeCopilot(t, "exit 7")).Run()
	if exitErr == nil {
		t.Fatal("failure fixture unexpectedly succeeded")
	}
	for _, tt := range []struct {
		name   string
		stderr string
		code   string
	}{
		{"classic token", "Error: Classic Personal Access Tokens (ghp_) are not supported by Copilot.", "unsupported_credential"},
		{"unrecognised token", "Error: No authentication information found.", "authentication_missing"},
		{"validation network failure", "Error: Authentication token found but could not be validated.\nFailed to fetch PAT user login: network fetch failed: request failed", "authentication_network"},
		{"invalid credential", "Error: Authentication failed\nYour GitHub token may be invalid, expired, or lacking the required permissions.", "authentication_failed"},
		{"policy denial", "Error: Access denied by policy settings", "access_denied"},
		{"unknown CLI flag", "error: unknown option '--untrusted-option'", "cli_arguments"},
		{"invalid CLI argument", "error: invalid argument '--untrusted-argument'", "cli_arguments"},
		{"unclassified failure", "An otherwise unrecognised failure occurred.", "process_failed"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			const secret = "test-only-private-token"
			const hostile = "::error::untrusted-stderr-marker"
			plain := classifyCopilotFailure(exitErr, tt.stderr)
			got := classifyCopilotFailure(exitErr, tt.stderr+"\n"+secret+"\n"+hostile)
			if got.Code != tt.code || got.ExitCode != 7 {
				t.Fatalf("classification = %s, exit = %d; want %s, exit 7", got.Code, got.ExitCode, tt.code)
			}
			if got.Error() == "" || got.Error() != plain.Error() {
				t.Fatal("diagnostic must be non-empty and independent of untrusted appended output")
			}
			for _, forbidden := range []string{secret, hostile, tt.stderr, "--untrusted-option", "--untrusted-argument"} {
				if strings.Contains(got.Error(), forbidden) {
					t.Fatalf("diagnostic contains subprocess output: %q", got.Error())
				}
			}
		})
	}

	t.Run("wrapped exit status", func(t *testing.T) {
		got := classifyCopilotFailure(fmt.Errorf("wrapped process error: %w", exitErr), "")
		if got.Code != "process_failed" || got.ExitCode != 7 {
			t.Fatalf("wrapped exit error classified as %s, exit %d", got.Code, got.ExitCode)
		}
	})

	t.Run("start error", func(t *testing.T) {
		const privatePath = "/private/untrusted-process-path"
		got := classifyCopilotFailure(&os.PathError{Op: "fork/exec", Path: privatePath, Err: os.ErrNotExist}, "")
		if got.Code != "process_start" || got.ExitCode != -1 {
			t.Fatalf("start error classified as %s, exit %d", got.Code, got.ExitCode)
		}
		if strings.Contains(got.Error(), privatePath) {
			t.Fatal("process start diagnostic exposed the raw process error")
		}
	})

	t.Run("output pipe wait expired after process exit", func(t *testing.T) {
		got := classifyCopilotFailure(exec.ErrWaitDelay, "")
		if got.Code != "process_failed" || got.ExitCode != -1 {
			t.Fatalf("pipe wait error classified as %s, exit %d", got.Code, got.ExitCode)
		}
	})
}

func TestCopilotInvocationClassifiesFailureWithoutExposingOutput(t *testing.T) {
	const token = "test-only-subprocess-token"
	const hostile = "::error::untrusted-stderr-marker"
	t.Setenv("COPILOT_GITHUB_TOKEN", token)
	config := sampleConfig(t, "copilot")
	for _, tt := range []struct {
		name       string
		diagnostic string
		code       string
	}{
		{"classic token", "Error: Classic Personal Access Tokens (ghp_) are not supported by Copilot.", "unsupported_credential"},
		{"missing authentication", "Error: No authentication information found.", "authentication_missing"},
		{"network validation", "Error: Authentication token found but could not be validated. Failed to fetch PAT user login: network fetch failed", "authentication_network"},
		{"unclassified failure", "Unexpected private provider message", "process_failed"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			body := "printf '%s\\n' '" + tt.diagnostic + "' >&2\n" +
				"printf '%s\\n' \"$COPILOT_GITHUB_TOKEN\" '" + hostile + "' >&2\n" +
				"printf '%s' 'untrusted partial answer'\nexit 7"
			a, err := newAdvisor(config, fakeCopilot(t, body), true)
			if err != nil {
				t.Fatal(err)
			}
			answer, err := a.invoke(context.Background(), config.Roles["capacity"], "capacity", region{}, peerEvidence{}, check{})
			var failure *copilotFailure
			if !errors.As(err, &failure) {
				t.Fatalf("expected typed Copilot failure, got %T", err)
			}
			if failure.Code != tt.code || failure.ExitCode != 7 || answer != "" {
				t.Fatalf("got code %s, exit %d, answer %q", failure.Code, failure.ExitCode, answer)
			}
			for _, forbidden := range []string{token, hostile, tt.diagnostic, "untrusted partial answer"} {
				if strings.Contains(err.Error(), forbidden) {
					t.Fatal("invocation failure exposed raw subprocess output")
				}
			}
		})
	}
}

// This exercises the pinned real executable without a valid credential or paid
// request. Classic tokens are rejected locally before network authentication.
func TestCopilotCLIRejectsSyntheticClassicToken(t *testing.T) {
	binary := os.Getenv("COPILOT_TEST_BIN")
	if binary == "" {
		t.Skip("set COPILOT_TEST_BIN to run the real CLI compatibility check")
	}
	token := "ghp_" + strings.Repeat("0", 36)
	t.Setenv("COPILOT_GITHUB_TOKEN", token)
	config := sampleConfig(t, "copilot")
	a, err := newAdvisor(config, binary, true)
	if err != nil {
		t.Fatal(err)
	}
	answer, err := a.invoke(context.Background(), config.Roles["capacity"], "capacity", region{}, peerEvidence{}, check{})
	var failure *copilotFailure
	if !errors.As(err, &failure) {
		t.Fatalf("expected typed classic-token rejection, got %T", err)
	}
	if failure.Code != "unsupported_credential" || failure.ExitCode != 1 || answer != "" {
		t.Fatalf("got code %s, exit %d, answer %q", failure.Code, failure.ExitCode, answer)
	}
	if strings.Contains(err.Error(), token) {
		t.Fatal("real CLI failure exposed the synthetic token")
	}
}
