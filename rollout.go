package main

import (
	"encoding/json"
	"fmt"
)

type region struct {
	Name           string `json:"name"`
	Current        int    `json:"current_replicas"`
	Desired        int    `json:"desired_replicas"`
	Surge          int    `json:"surge_replicas"`
	FreeSlots      int    `json:"free_pod_slots"`
	Budget         int    `json:"error_budget_percent"`
	Telemetry      bool   `json:"telemetry_available"`
	Signed         bool   `json:"image_signed"`
	Privileged     bool   `json:"privileged"`
	PolicyMaxSurge int    `json:"policy_max_surge"`
}

type check struct {
	Role     string `json:"role"`
	State    string `json:"state"`
	Evidence string `json:"evidence"`
}

type regionalReport struct {
	Region string  `json:"region"`
	State  string  `json:"state"`
	Checks []check `json:"checks"`
}

type report struct {
	Scenario string           `json:"scenario"`
	Decision string           `json:"decision"`
	Regions  []regionalReport `json:"regions"`
	Database string           `json:"database"`
	Usage    json.RawMessage  `json:"mas_usage"`
}

type topology struct {
	Name        string      `json:"name"`
	Agents      []string    `json:"agents"`
	Edges       [][2]string `json:"edges"`
	Capacity    int         `json:"mailbox_capacity"`
	Application string      `json:"application"`
	MaxMessages int         `json:"max_messages"`
	MaxCalls    int         `json:"max_calls"`
	MaxBytes    int         `json:"max_bytes"`
	WallSeconds int         `json:"wall_seconds"`
}

func rolloutTopology() topology {
	t := topology{
		Name: "platform-rollout", Agents: []string{"release"}, Capacity: 8,
		Application: "agent-task/1", MaxMessages: 100, MaxCalls: 1,
		MaxBytes: 262144, WallSeconds: 120,
	}
	link := func(a, b string) {
		t.Edges = append(t.Edges, [2]string{a, b}, [2]string{b, a})
	}
	for _, name := range []string{"east", "west"} {
		lead := name + "-lead"
		t.Agents = append(t.Agents, lead)
		link("release", lead)
		roles := []string{name + "-capacity", name + "-reliability", name + "-security"}
		for _, role := range roles {
			t.Agents = append(t.Agents, role)
			link(lead, role)
		}
		for i, a := range roles {
			for _, b := range roles[i+1:] {
				link(a, b)
			}
		}
	}
	return t
}

func scenario(name string) ([]region, error) {
	regions := []region{
		{Name: "east", Current: 6, Desired: 8, Surge: 2, FreeSlots: 5, Budget: 60, Telemetry: true, Signed: true, PolicyMaxSurge: 2},
		{Name: "west", Current: 6, Desired: 8, Surge: 2, FreeSlots: 4, Budget: 35, Telemetry: true, Signed: true, PolicyMaxSurge: 2},
	}
	switch name {
	case "healthy":
	case "capacity":
		regions[1].FreeSlots = 3
	case "unsafe-image":
		regions[0].Signed = false
	case "missing-telemetry":
		regions[1].Telemetry = false
	default:
		return nil, fmt.Errorf("unknown scenario %q (choose healthy, capacity, unsafe-image, missing-telemetry)", name)
	}
	return regions, nil
}

type peerEvidence struct {
	Known bool `json:"known"`
	Limit int  `json:"limit"`
	Safe  bool `json:"safe"`
}

func peerPolicy(operation string, r region) (peerEvidence, error) {
	switch operation {
	case "surge-budget":
		limit := 0
		if r.Budget >= 20 {
			limit = 2
		}
		return peerEvidence{Known: r.Telemetry, Limit: limit}, nil
	case "image-attestation":
		return peerEvidence{Known: true, Safe: r.Signed && !r.Privileged}, nil
	case "surge-size":
		return peerEvidence{Known: true, Limit: r.Surge}, nil
	default:
		return peerEvidence{}, fmt.Errorf("unknown peer operation %q", operation)
	}
}

func evaluate(role string, r region, peer peerEvidence) check {
	result := check{Role: role, State: "blocked"}
	switch role {
	case "capacity":
		needed := r.Desired - r.Current + r.Surge
		result.Evidence = fmt.Sprintf("need %d pod slots, have %d; reliability permits surge %d", needed, r.FreeSlots, peer.Limit)
		if !peer.Known {
			result.State = "unknown"
			result.Evidence = "surge allowance unavailable: missing reliability telemetry"
		} else if r.FreeSlots >= needed && r.Surge <= peer.Limit {
			result.State = "ready"
		}
	case "reliability":
		result.Evidence = fmt.Sprintf("error budget %d%% (minimum 20%%); peer image policy safe=%t", r.Budget, peer.Safe)
		if !r.Telemetry {
			result.State = "unknown"
			result.Evidence = "error-budget telemetry unavailable"
		} else if r.Budget >= 20 && peer.Known && peer.Safe {
			result.State = "ready"
		}
	case "security":
		result.Evidence = fmt.Sprintf("signed=%t privileged=%t; peer surge %d (policy maximum %d)", r.Signed, r.Privileged, peer.Limit, r.PolicyMaxSurge)
		if peer.Known && r.Signed && !r.Privileged && peer.Limit <= r.PolicyMaxSurge {
			result.State = "ready"
		}
	default:
		result.State = "unknown"
		result.Evidence = "unrecognized specialist"
	}
	return result
}

// Blockers take precedence over missing evidence; neither permits a rollout.
func gate(states []string) string {
	if len(states) == 0 {
		return "unknown"
	}
	state := "ready"
	for _, s := range states {
		if s == "blocked" {
			return "blocked"
		}
		if s != "ready" {
			state = "unknown"
		}
	}
	return state
}

func assessRegion(b *bus, r region) (regionalReport, error) {
	result := regionalReport{Region: r.Name}
	roles := []string{"capacity", "reliability", "security"}
	peerRoles := []string{"reliability", "security", "capacity"}
	operations := []string{"surge-budget", "image-attestation", "surge-size"}
	requests := make([]message, len(roles))
	// Queue every star branch before collecting results. Execution is deliberately serial.
	for i, role := range roles {
		request, err := b.begin(r.Name+"-lead", r.Name+"-"+role, "readiness", r)
		if err != nil {
			return result, err
		}
		requests[i] = request
	}
	inputs := make([]region, len(requests))
	for i, request := range requests {
		payload, err := b.consume(request)
		if err != nil {
			return result, err
		}
		inputs[i], err = decodeInput[region](payload)
		if err != nil {
			return result, err
		}
	}
	var states []string
	for i, request := range requests {
		input := inputs[i]
		operation := operations[i]
		peer, err := exchange(b, request.Recipient, r.Name+"-"+peerRoles[i], operation, input,
			func(received region) (peerEvidence, error) { return peerPolicy(operation, received) })
		if err != nil {
			return result, err
		}
		raw, err := b.finish(request, evaluate(roles[i], input, peer))
		if err != nil {
			return result, err
		}
		var delivered check
		if err := json.Unmarshal(raw, &delivered); err != nil {
			return result, err
		}
		result.Checks = append(result.Checks, delivered)
		states = append(states, delivered.State)
	}
	result.State = gate(states)
	return result, nil
}

func runRollout(b *bus, name string, regions []region) (report, error) {
	result := report{Scenario: name, Database: b.database}
	var states []string
	for _, r := range regions {
		regional, err := exchange(b, "release", r.Name+"-lead", "regional-readiness", r,
			func(received region) (regionalReport, error) { return assessRegion(b, received) })
		if err != nil {
			return result, err
		}
		result.Regions = append(result.Regions, regional)
		states = append(states, regional.State)
	}
	switch gate(states) {
	case "ready":
		result.Decision = "proceed"
	case "blocked":
		result.Decision = "hold"
	default:
		result.Decision = "inconclusive"
	}
	var status struct {
		Expired bool            `json:"expired"`
		Usage   json.RawMessage `json:"usage"`
		Tasks   []struct {
			State string `json:"state"`
		} `json:"tasks"`
	}
	if err := b.command(nil, &status, "status", b.database, "rollout"); err != nil {
		return result, err
	}
	if status.Expired || len(status.Tasks) != b.next {
		return result, fmt.Errorf("MAS run expired or task evidence is incomplete")
	}
	for _, task := range status.Tasks {
		if task.State != "completed" {
			return result, fmt.Errorf("MAS task is not completed: %s", task.State)
		}
	}
	result.Usage = status.Usage
	return result, nil
}
