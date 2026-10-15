package main

import (
	"strings"
	"testing"
)

func TestReadinessPolicies(t *testing.T) {
	regions, err := scenario("healthy")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		role   string
		change func(*region)
		want   string
	}{
		{"capacity exact fit", "capacity", func(r *region) { r.FreeSlots = 4 }, "ready"},
		{"capacity shortfall", "capacity", func(r *region) { r.FreeSlots = 3 }, "blocked"},
		{"low error budget prevents surge", "capacity", func(r *region) { r.Budget = 19 }, "blocked"},
		{"capacity needs telemetry", "capacity", func(r *region) { r.Telemetry = false }, "unknown"},
		{"budget threshold", "reliability", func(r *region) { r.Budget = 20 }, "ready"},
		{"exhausted budget", "reliability", func(r *region) { r.Budget = 0 }, "blocked"},
		{"missing telemetry", "reliability", func(r *region) { r.Telemetry = false }, "unknown"},
		{"image peer veto", "reliability", func(r *region) { r.Signed = false }, "blocked"},
		{"unsigned image", "security", func(r *region) { r.Signed = false }, "blocked"},
		{"privileged workload", "security", func(r *region) { r.Privileged = true }, "blocked"},
		{"surge exceeds policy", "security", func(r *region) { r.PolicyMaxSurge = 1 }, "blocked"},
	}
	operations := map[string]string{"capacity": "surge-budget", "reliability": "image-attestation", "security": "surge-size"}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := regions[0]
			tt.change(&r)
			peer, err := peerPolicy(operations[tt.role], r)
			if err != nil {
				t.Fatal(err)
			}
			got := evaluate(tt.role, r, peer)
			if got.State != tt.want || strings.TrimSpace(got.Evidence) == "" {
				t.Fatalf("got %+v, want %s with evidence", got, tt.want)
			}
		})
	}
}

func TestGateFailsClosed(t *testing.T) {
	for _, tt := range []struct {
		states []string
		want   string
	}{
		{nil, "unknown"},
		{[]string{"ready", "ready"}, "ready"},
		{[]string{"ready", "unknown"}, "unknown"},
		{[]string{"ready", "unexpected"}, "unknown"},
		{[]string{"unknown", "blocked"}, "blocked"},
		{[]string{"blocked", "unknown"}, "blocked"},
	} {
		if got := gate(tt.states); got != tt.want {
			t.Errorf("gate(%v) = %s, want %s", tt.states, got, tt.want)
		}
	}
}

func TestTopologyBoundaries(t *testing.T) {
	topo := rolloutTopology()
	agents := make(map[string]bool)
	for _, actor := range topo.Agents {
		if agents[actor] {
			t.Fatalf("duplicate actor %s", actor)
		}
		agents[actor] = true
	}
	edges := make(map[[2]string]bool)
	for _, edge := range topo.Edges {
		if edges[edge] || !agents[edge[0]] || !agents[edge[1]] {
			t.Fatalf("invalid or duplicate edge %v", edge)
		}
		edges[edge] = true
	}
	for edge := range edges {
		if !edges[[2]string{edge[1], edge[0]}] {
			t.Errorf("no reply route for %v", edge)
		}
	}
	for _, edge := range [][2]string{
		{"release", "east-lead"},
		{"east-lead", "east-capacity"},
		{"east-capacity", "east-reliability"},
		{"east-reliability", "east-security"},
		{"east-security", "east-capacity"},
	} {
		if !edges[edge] {
			t.Errorf("missing topology route %v", edge)
		}
	}
	for _, edge := range [][2]string{
		{"release", "east-capacity"},
		{"east-capacity", "west-capacity"},
		{"east-lead", "west-lead"},
	} {
		if edges[edge] {
			t.Errorf("unexpected boundary crossing %v", edge)
		}
	}
}

func TestUnknownScenario(t *testing.T) {
	if _, err := scenario("typo"); err == nil {
		t.Fatal("unknown scenario accepted")
	}
}
