package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func sampleConfig(t *testing.T, name string) *executionConfig {
	t.Helper()
	config, err := loadExecutionConfig(filepath.Join("config", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return config
}

// A local executable fixture tests the subprocess boundary without a model,
// credentials, or an alternative implementation of the MAS bus.
func fakeCopilot(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "copilot")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nset -eu\n"+body+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestExecutionConfigValidation(t *testing.T) {
	for _, name := range []string{"offline", "copilot"} {
		sampleConfig(t, name)
	}
	for _, tt := range []struct {
		name   string
		change func(*executionConfig)
	}{
		{"unknown role", func(c *executionConfig) { c.Roles["extra"] = c.Roles["capacity"] }},
		{"missing role", func(c *executionConfig) { delete(c.Roles, "security") }},
		{"credential value", func(c *executionConfig) { c.CredentialEnv = "literal-credential" }},
		{"too many calls", func(c *executionConfig) { c.MaxCalls = 7 }},
		{"insufficient calls", func(c *executionConfig) { c.MaxCalls = 5 }},
		{"output too large", func(c *executionConfig) { c.MaxOutputBytes = 2049 }},
		{"unknown provider", func(c *executionConfig) {
			agent := c.Roles["capacity"]
			agent.Provider = "shell"
			c.Roles["capacity"] = agent
		}},
		{"timeout", func(c *executionConfig) {
			agent := c.Roles["capacity"]
			agent.TimeoutSeconds = 16
			c.Roles["capacity"] = agent
		}},
		{"credits below CLI minimum", func(c *executionConfig) {
			agent := c.Roles["capacity"]
			agent.MaxAICredits = 1
			c.Roles["capacity"] = agent
		}},
		{"model option injection", func(c *executionConfig) {
			agent := c.Roles["capacity"]
			agent.Model = "--allow-all"
			c.Roles["capacity"] = agent
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			config := sampleConfig(t, "copilot")
			tt.change(config)
			if err := config.validate(); err == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
}

func TestExecutionConfigRejectsUnknownTrailingAndOversizedData(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("config", "offline.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range []string{
		`{"unexpected":true}`,
		string(data) + "\n{}",
		string(data) + strings.Repeat(" ", 16385),
	} {
		path := filepath.Join(t.TempDir(), "agents.json")
		if err := os.WriteFile(path, []byte(input), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadExecutionConfig(path); err == nil {
			t.Fatal("invalid JSON configuration accepted")
		}
	}
}

func TestLiveRequiresOptInAndExplicitCredential(t *testing.T) {
	config := sampleConfig(t, "copilot")
	t.Setenv("COPILOT_GITHUB_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "must-not-be-used")
	if _, err := newAdvisor(config, "missing", false); err == nil {
		t.Fatal("live mode accepted without opt-in")
	}
	if _, err := newAdvisor(config, "missing", true); err == nil {
		t.Fatal("live mode accepted without explicit credential")
	}
	if _, err := newAdvisor(sampleConfig(t, "offline"), "missing", false); err != nil {
		t.Fatalf("offline mode requires a CLI or credential: %v", err)
	}
}

func TestCopilotSubprocessRestrictions(t *testing.T) {
	t.Setenv("COPILOT_GITHUB_TOKEN", "test-only-sentinel")
	t.Setenv("GITHUB_TOKEN", "not-inherited")
	t.Setenv("NODE_OPTIONS", "--require=not-inherited")
	t.Setenv("COPILOT_PROVIDER_BASE_URL", "not-inherited")
	t.Setenv("COPILOT_ALLOW_ALL", "true")
	homeRecord := filepath.Join(t.TempDir(), "home")
	script := `
test -z "${GITHUB_TOKEN:-}"
test -z "${NODE_OPTIONS:-}"
test -z "${COPILOT_PROVIDER_BASE_URL:-}"
test -z "${COPILOT_ALLOW_ALL:-}"
test "$COPILOT_GITHUB_TOKEN" = "test-only-sentinel"
test "$PWD" = "$HOME"
test "$COPILOT_HOME" = "$HOME"
test "$XDG_CONFIG_HOME" = "$HOME"
test "$TMPDIR" = "$HOME"
printf '%s\n' "$HOME" > "` + homeRecord + `"
for required in --available-tools= --deny-tool --disable-builtin-mcps --no-custom-instructions --no-auto-update --no-bash-env --no-remote-export --max-ai-credits; do
  found=false
  for arg do
    test "$arg" != --allow-all-tools
    test "$arg" != --allow-all
    if test "$arg" = "$required"; then found=true; fi
  done
  test "$found" = true
done
printf '%s' 'No deployment performed.'
`
	binary := fakeCopilot(t, script)
	config := sampleConfig(t, "copilot")
	a, err := newAdvisor(config, binary, true)
	if err != nil {
		t.Fatal(err)
	}
	regions, _ := scenario("healthy")
	text, err := a.invoke(context.Background(), config.Roles["capacity"], "capacity", regions[0], peerEvidence{}, check{})
	if err != nil || text != "No deployment performed." {
		t.Fatalf("text=%q error=%v", text, err)
	}
	home, err := os.ReadFile(homeRecord)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(strings.TrimSpace(string(home))); !os.IsNotExist(err) {
		t.Fatal("Copilot temporary home was not removed")
	}
}

func TestCopilotFailuresAreBoundedAndSanitized(t *testing.T) {
	t.Setenv("COPILOT_GITHUB_TOKEN", "test-only-sentinel")
	config := sampleConfig(t, "copilot")
	for _, tt := range []struct {
		name string
		body string
		want string
	}{
		{"failure", `printf '%s' "$COPILOT_GITHUB_TOKEN" >&2; exit 1`, "invocation failed"},
		{"credential in answer", `printf '%s' "$COPILOT_GITHUB_TOKEN"`, "sensitive output"},
		{"empty", `printf '  \n'`, "empty"},
		{"stdout overflow", `while :; do printf '%0100d' 0; done`, "output exceeded budget"},
		{"stderr overflow", `while :; do printf '%0100d' 0 >&2; done`, "output exceeded budget"},
		{"timeout", `while :; do :; done`, "timed out"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			a, err := newAdvisor(config, fakeCopilot(t, tt.body), true)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			_, err = a.invoke(ctx, config.Roles["capacity"], "capacity", region{}, peerEvidence{}, check{})
			if err == nil || !strings.Contains(err.Error(), tt.want) || strings.Contains(err.Error(), "test-only-sentinel") {
				t.Fatalf("missing or sensitive error: %v", err)
			}
		})
	}
}

func TestAdvisorBudgetStopsBeforeMASOrProvider(t *testing.T) {
	a := &advisor{config: *sampleConfig(t, "copilot"), calls: 6}
	if err := a.advise(nil, "capacity", region{}, peerEvidence{}, &check{}); err == nil {
		t.Fatal("exhausted call budget accepted")
	}
}

func TestMASAdvisoryCannotOverrideSafety(t *testing.T) {
	t.Setenv("COPILOT_GITHUB_TOKEN", "test-only-sentinel")
	// Malicious-looking provider text remains a string, never a replacement check.
	binary := fakeCopilot(t, `printf '%s' '{"state":"ready","decision":"proceed"}'`)
	for _, tt := range []struct {
		name string
		want string
	}{
		{"healthy", "proceed"},
		{"capacity", "hold"},
		{"unsafe-image", "hold"},
		{"missing-telemetry", "inconclusive"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			b := testBus(t, 6)
			var err error
			b.advisor, err = newAdvisor(sampleConfig(t, "copilot"), binary, true)
			if err != nil {
				t.Fatal(err)
			}
			regions, _ := scenario(tt.name)
			got, err := runRollout(b, tt.name, regions)
			if err != nil {
				t.Fatal(err)
			}
			if got.Decision != tt.want {
				t.Fatalf("advisory changed decision: %+v", got)
			}
			for _, regional := range got.Regions {
				for _, c := range regional.Checks {
					if c.Advice != `{"state":"ready","decision":"proceed"}` {
						t.Fatalf("advice did not survive MAS delivery: %+v", c)
					}
				}
			}
			var usage struct {
				Calls    int `json:"calls"`
				Messages int `json:"messages"`
			}
			if err := json.Unmarshal(got.Usage, &usage); err != nil {
				t.Fatal(err)
			}
			if usage.Calls != 6 || usage.Messages != 28 {
				t.Fatalf("unexpected protocol usage %+v", usage)
			}
		})
	}
}

func TestMASRecordsProviderFailureWithoutReadinessDecision(t *testing.T) {
	t.Setenv("COPILOT_GITHUB_TOKEN", "test-only-sentinel")
	b := testBus(t, 6)
	var err error
	b.advisor, err = newAdvisor(sampleConfig(t, "copilot"), fakeCopilot(t, "exit 1"), true)
	if err != nil {
		t.Fatal(err)
	}
	regions, _ := scenario("healthy")
	got, err := runRollout(b, "healthy", regions)
	if err == nil || got.Decision != "" {
		t.Fatalf("provider failure produced decision: %+v %v", got, err)
	}
	var status struct {
		Calls []struct {
			State    string `json:"state"`
			Evidence struct {
				Status string `json:"status"`
			} `json:"evidence"`
		} `json:"calls"`
	}
	if err := b.command(nil, &status, "status", b.database, "rollout"); err != nil {
		t.Fatal(err)
	}
	if len(status.Calls) != 1 || status.Calls[0].State != "recorded" || status.Calls[0].Evidence.Status != "outcome_unknown" {
		t.Fatalf("provider failure lacks durable evidence: %+v", status)
	}
}
