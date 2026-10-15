package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

type agentConfig struct {
	Provider       string `json:"provider"`
	Model          string `json:"model"`
	Instructions   string `json:"instructions"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	MaxAICredits   int    `json:"max_ai_credits"`
}

type executionConfig struct {
	CredentialEnv  string                 `json:"credential_env"`
	MaxCalls       int                    `json:"max_calls"`
	MaxOutputBytes int                    `json:"max_output_bytes"`
	Roles          map[string]agentConfig `json:"roles"`
}

func loadExecutionConfig(path string) (*executionConfig, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 16385))
	if err != nil || len(data) > 16384 {
		return nil, fmt.Errorf("agent configuration must be at most 16 KiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var config executionConfig
	if err := decoder.Decode(&config); err != nil {
		return nil, fmt.Errorf("agent configuration: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("agent configuration must contain one JSON object")
	}
	return &config, config.validate()
}

func (c *executionConfig) validate() error {
	if c.CredentialEnv != "COPILOT_GITHUB_TOKEN" {
		return fmt.Errorf("credential_env must be COPILOT_GITHUB_TOKEN (a name, never a token)")
	}
	if c.MaxCalls < 0 || c.MaxCalls > 6 || c.MaxOutputBytes < 64 || c.MaxOutputBytes > 2048 {
		return fmt.Errorf("agent budgets require max_calls 0..6 and max_output_bytes 64..2048")
	}
	if len(c.Roles) != 3 {
		return fmt.Errorf("configure exactly capacity, reliability, and security")
	}
	liveRoles := 0
	for _, role := range []string{"capacity", "reliability", "security"} {
		agent, ok := c.Roles[role]
		if !ok || len(agent.Instructions) == 0 || len(agent.Instructions) > 2048 ||
			agent.TimeoutSeconds < 1 || agent.TimeoutSeconds > 15 {
			return fmt.Errorf("role %s needs instructions (1..2048 bytes) and timeout_seconds 1..15", role)
		}
		switch agent.Provider {
		case "deterministic":
			if agent.Model != "" || agent.MaxAICredits != 0 {
				return fmt.Errorf("deterministic role %s must not configure model or AI credits", role)
			}
		case "copilot":
			if !regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,79}$`).MatchString(agent.Model) ||
				agent.MaxAICredits < 30 || agent.MaxAICredits > 300 {
				return fmt.Errorf("Copilot role %s needs a model identifier and max_ai_credits 30..300", role)
			}
			liveRoles++
		default:
			return fmt.Errorf("unknown provider for role %s", role)
		}
	}
	if c.MaxCalls < 2*liveRoles {
		return fmt.Errorf("max_calls must cover both regions for each Copilot role")
	}
	return nil
}

type advisor struct {
	config executionConfig
	binary string
	token  string
	calls  int
}

func newAdvisor(config *executionConfig, binary string, allowLive bool) (*advisor, error) {
	if config == nil {
		return nil, nil
	}
	if err := config.validate(); err != nil {
		return nil, err
	}
	a := &advisor{config: *config}
	live := false
	for _, role := range config.Roles {
		live = live || role.Provider == "copilot"
	}
	if !live {
		return a, nil
	}
	if !allowLive {
		return nil, fmt.Errorf("Copilot requires explicit -allow-live")
	}
	a.token = os.Getenv(config.CredentialEnv)
	if strings.TrimSpace(a.token) == "" {
		return nil, fmt.Errorf("Copilot requires %s", config.CredentialEnv)
	}
	resolved, err := exec.LookPath(binary)
	if err != nil {
		return nil, fmt.Errorf("Copilot CLI not found; install @github/copilot@1.0.98")
	}
	a.binary, err = filepath.Abs(resolved)
	return a, err
}

// boundedOutput cancels the process on overflow without retaining unbounded output.
type boundedOutput struct {
	buffer   bytes.Buffer
	limit    int
	overflow bool
	cancel   context.CancelFunc
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	size := len(p)
	remaining := b.limit - b.buffer.Len()
	if size > remaining {
		b.buffer.Write(p[:remaining])
		b.overflow = true
		b.cancel()
		return size, nil
	}
	b.buffer.Write(p)
	return size, nil
}

func (a *advisor) invoke(ctx context.Context, agent agentConfig, role string, r region, peer peerEvidence, verdict check) (string, error) {
	// A fresh home and working directory avoid ambient repository instructions,
	// hooks, plugins, MCP configuration, sessions, and credential discovery.
	private, err := os.MkdirTemp("", "mas-copilot-")
	if err != nil {
		return "", fmt.Errorf("create Copilot workspace")
	}
	defer os.RemoveAll(private)
	input, err := json.Marshal(struct {
		Region   region       `json:"region"`
		Peer     peerEvidence `json:"peer"`
		Decision check        `json:"authoritative_check"`
	}{r, peer, verdict})
	if err != nil {
		return "", err
	}
	prompt := "You are a read-only platform engineering advisor for the " + role + " role. " +
		"Explain the supplied synthetic evidence and suggest a next step in at most 100 words. " +
		"The deterministic decision is authoritative; do not claim to execute tools or deployments. " +
		"Return plain text only.\nRole instructions:\n" + agent.Instructions + "\nEvidence:\n" + string(input)
	ctx, cancel := context.WithTimeout(ctx, time.Duration(agent.TimeoutSeconds)*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, a.binary,
		"--prompt", prompt, "--model", agent.Model, "--silent", "--output-format", "text",
		"--stream", "off", "--available-tools=",
		"--disable-builtin-mcps", "--no-custom-instructions", "--no-ask-user",
		"--no-auto-update", "--no-color", "--no-bash-env", "--no-remote-export",
		"--log-level", "none", "--max-ai-credits", strconv.Itoa(agent.MaxAICredits))
	cmd.Dir = private
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + private,
		"XDG_CONFIG_HOME=" + private, "XDG_CACHE_HOME=" + private,
		"COPILOT_HOME=" + private, "TMPDIR=" + private,
		"COPILOT_GITHUB_TOKEN=" + a.token, "CI=true", "NO_COLOR=1",
	}
	stdout := &boundedOutput{limit: a.config.MaxOutputBytes, cancel: cancel}
	stderr := &boundedOutput{limit: 4096, cancel: cancel}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.WaitDelay = time.Second
	err = cmd.Run()
	// Never include subprocess diagnostics or partial output in an error: these
	// can contain credentials or untrusted model text.
	switch {
	case stdout.overflow || stderr.overflow:
		return "", fmt.Errorf("Copilot output exceeded budget")
	case ctx.Err() != nil:
		return "", fmt.Errorf("Copilot invocation timed out or was cancelled")
	case err != nil:
		return "", fmt.Errorf("Copilot invocation failed")
	}
	text := strings.TrimSpace(stdout.buffer.String())
	if text == "" || !utf8.ValidString(text) || strings.Contains(text, a.token) {
		return "", fmt.Errorf("Copilot returned empty, invalid, or sensitive output")
	}
	return text, nil
}

func (a *advisor) advise(b *bus, role string, r region, peer peerEvidence, verdict *check) error {
	if a == nil || a.config.Roles[role].Provider == "deterministic" {
		return nil
	}
	if a.calls >= a.config.MaxCalls {
		return fmt.Errorf("Copilot invocation budget exhausted")
	}
	a.calls++
	actor := r.Name + "-" + role
	callID := fmt.Sprintf("advice-%d", a.calls)
	if err := b.command(nil, nil, "reserve", b.database, "rollout", actor, callID); err != nil {
		return err
	}
	text, invokeErr := a.invoke(b.ctx, a.config.Roles[role], role, r, peer, *verdict)
	evidence := map[string]any{
		"provider": "copilot", "model": a.config.Roles[role].Model,
		"status": "completed", "output_bytes": len(text),
	}
	if invokeErr != nil {
		evidence["status"] = "outcome_unknown"
	}
	if err := b.command(evidence, nil, "finish", b.database, "rollout", actor, callID, "-"); err != nil {
		return err
	}
	if invokeErr != nil {
		return invokeErr
	}
	verdict.Advice = text
	return nil
}
