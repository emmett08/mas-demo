package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// copilotFailure exposes only fixed diagnostic codes and a process exit status.
// Subprocess output is untrusted and must never be retained in this error.
type copilotFailure struct {
	Code     string
	ExitCode int // -1 means no provider exit status applies (local rejection) or is available.
}

func (e *copilotFailure) Error() string {
	hint := "check the configured model, account access, and pinned CLI installation"
	switch e.Code {
	case "unsupported_credential":
		hint = "classic PATs (ghp_) are unsupported; replace COPILOT_GITHUB_TOKEN with a fine-grained PAT with Copilot Requests permission"
	case "authentication_missing":
		hint = "the CLI did not recognise a supported credential; check COPILOT_GITHUB_TOKEN in the copilot-demo environment"
	case "authentication_network":
		hint = "credential validation could not reach GitHub; check network connectivity before replacing the credential"
	case "authentication_failed":
		hint = "check token expiry, revocation, and Copilot Requests permission"
	case "access_denied":
		hint = "check the account's Copilot subscription and organisation policies"
	case "cli_arguments":
		hint = "the pinned CLI rejected an argument; check CLI version and invocation compatibility"
	case "process_start":
		hint = "the CLI could not start; check its executable, Node.js, and PATH"
	case "output_budget_exceeded":
		hint = "Copilot output exceeded budget; partial output was discarded"
	case "invocation_timeout":
		hint = "Copilot invocation timed out; completion is unknown"
	case "invocation_cancelled":
		hint = "Copilot invocation was cancelled; completion is unknown"
	case "invalid_output":
		hint = "Copilot returned empty, invalid, or sensitive output; the answer was discarded"
	}
	return fmt.Sprintf("Copilot invocation failed [%s, exit_code=%d]: %s", e.Code, e.ExitCode, hint)
}

// classifyCopilotFailure recognises documented/reproduced CLI diagnostics, not
// arbitrary provider prose. Unknown failures retain an exit status and a generic
// hint. A classification is diagnostic evidence, never a rollout decision.
func classifyCopilotFailure(err error, stderr string) *copilotFailure {
	failure := &copilotFailure{Code: "process_failed", ExitCode: -1}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		var pathErr *os.PathError
		var lookupErr *exec.Error
		if errors.As(err, &pathErr) || errors.As(err, &lookupErr) {
			failure.Code = "process_start"
		}
		return failure
	}
	failure.ExitCode = exitErr.ExitCode()
	diagnostic := strings.ToLower(stderr)
	switch {
	case strings.Contains(diagnostic, "classic personal access tokens (ghp_) are not supported"):
		failure.Code = "unsupported_credential"
	case strings.Contains(diagnostic, "no authentication information found"):
		failure.Code = "authentication_missing"
	case strings.Contains(diagnostic, "authentication token found but could not be validated") &&
		(strings.Contains(diagnostic, "network fetch failed") || strings.Contains(diagnostic, "fetch failed")):
		failure.Code = "authentication_network"
	case strings.Contains(diagnostic, "authentication failed") || strings.Contains(diagnostic, "401 unauthorized"):
		failure.Code = "authentication_failed"
	case strings.Contains(diagnostic, "access denied by policy settings") || strings.Contains(diagnostic, "403 forbidden"):
		failure.Code = "access_denied"
	case strings.Contains(diagnostic, "error: unknown option") || strings.Contains(diagnostic, "error: invalid argument"):
		failure.Code = "cli_arguments"
	}
	return failure
}
