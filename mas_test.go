package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// MAS_BIN opts into tests against the actual native runtime, never a substitute bus.
func testBus(t *testing.T, callBudget ...int) *bus {
	t.Helper()
	binary := os.Getenv("MAS_BIN")
	if binary == "" {
		t.Skip("set MAS_BIN to the pinned native mas binary to run integration tests")
	}
	resolved, err := exec.LookPath(binary)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	topology := rolloutTopology()
	if len(callBudget) > 0 {
		topology.MaxCalls = callBudget[0]
	}
	data, err := json.Marshal(topology)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "topology.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	b := &bus{ctx: ctx, binary: resolved, database: filepath.Join(dir, "state.sqlite")}
	if err := b.command(nil, nil, "init", b.database, "rollout", path); err != nil {
		t.Fatal(err)
	}
	return b
}

func TestMASScenarios(t *testing.T) {
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
			b := testBus(t)
			regions, err := scenario(tt.name)
			if err != nil {
				t.Fatal(err)
			}
			got, err := runRollout(b, tt.name, regions)
			if err != nil {
				t.Fatal(err)
			}
			if got.Decision != tt.want || len(got.Regions) != 2 {
				t.Fatalf("unexpected report: %+v", got)
			}
			for _, region := range got.Regions {
				if len(region.Checks) != 3 {
					t.Fatalf("missing checks: %+v", region)
				}
			}
			var usage struct {
				Messages int `json:"messages"`
			}
			if err := json.Unmarshal(got.Usage, &usage); err != nil {
				t.Fatal(err)
			}
			if b.next != 14 || usage.Messages != 28 {
				t.Fatalf("expected 14 tasks and 28 messages, got %d tasks, %+v", b.next, usage)
			}
			for _, actor := range rolloutTopology().Agents {
				var pending []json.RawMessage
				if err := b.command(nil, &pending, "receive", b.database, "rollout", actor); err != nil {
					t.Fatal(err)
				}
				if len(pending) != 0 {
					t.Fatalf("unacknowledged messages for %s", actor)
				}
			}
		})
	}
}

func TestMASRejectsBypassAndPreservesDelivery(t *testing.T) {
	b := testBus(t)
	if _, err := b.begin("release", "east-capacity", "bypass", map[string]any{}); err == nil {
		t.Fatal("runtime accepted a forbidden route")
	}
	request, err := b.begin("release", "east-lead", "readiness", map[string]any{"note": "quotes \" and\nnewline"})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.send(request); err != nil {
		t.Fatalf("identical resend rejected: %v", err)
	}
	var deliveries []struct {
		Redelivered bool `json:"redelivered"`
	}
	for i := 0; i < 2; i++ {
		if err := b.command(nil, &deliveries, "receive", b.database, "rollout", "east-lead"); err != nil {
			t.Fatal(err)
		}
		if len(deliveries) != 1 || deliveries[0].Redelivered != (i == 1) {
			t.Fatalf("unexpected redelivery on receive %d: %+v", i, deliveries)
		}
	}
	wrong := request
	wrong.ID = "wrong"
	if _, err := b.consume(wrong); err == nil {
		t.Fatal("unexpected envelope was acknowledged")
	}
	if _, err := b.consume(request); err != nil {
		t.Fatal(err)
	}
	if _, err := b.finish(request, map[string]any{"state": "ready"}); err != nil {
		t.Fatal(err)
	}
}

func TestMASDoesNotExecuteClosedRequest(t *testing.T) {
	b := testBus(t)
	request, err := b.begin("release", "east-lead", "readiness", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	cancel := message{
		ID: "cancel", Run: request.Run, Sender: request.Sender, Recipient: request.Recipient,
		Version: "1", Kind: "Cancel",
		Payload: map[string]any{"task_id": request.Payload["task_id"], "reply_to": request.ID, "reason": "test"},
	}
	if err := b.send(cancel); err != nil {
		t.Fatal(err)
	}
	if _, err := b.consume(request); err == nil {
		t.Fatal("closed request accepted for execution")
	}
}
