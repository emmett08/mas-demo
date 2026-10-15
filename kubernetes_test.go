package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestResourceQuantitiesAreConservative(t *testing.T) {
	for _, tt := range []struct {
		value string
		cpu   bool
		up    bool
		want  int
	}{
		{"2", true, false, 2000},
		{"10m", true, true, 10},
		{"100001n", true, true, 1},
		{"100001n", true, false, 0},
		{"16Mi", false, true, 16777216},
		{"1.5Gi", false, false, 1610612736},
		{"400m", false, true, 1},
		{"110", false, false, 110},
	} {
		got, err := quantity(tt.value, tt.cpu, tt.up)
		if err != nil || got != tt.want {
			t.Errorf("quantity(%s, %t, %t) = %d, %v; want %d", tt.value, tt.cpu, tt.up, got, err, tt.want)
		}
	}
	for _, bad := range []string{"", "-1", "not-a-quantity", "1e99", "2Pi", "999999999999999999999999999"} {
		if _, err := quantity(bad, false, false); err == nil {
			t.Errorf("accepted unsupported resource %q", bad)
		}
	}
}

func TestPodRequestAccounting(t *testing.T) {
	var spec podSpec
	if err := json.Unmarshal([]byte(`{"containers":[{"resources":{"requests":{"cpu":"10m","memory":"16Mi"}}},{"resources":{"requests":{"cpu":"20m","memory":"8Mi"}}}],"overhead":{"cpu":"5m","memory":"1Mi"}}`), &spec); err != nil {
		t.Fatal(err)
	}
	cpu, memory, err := podRequests(spec)
	if err != nil || cpu != 35 || memory != 25*1024*1024 {
		t.Fatalf("incorrect accounting: %d %d %v", cpu, memory, err)
	}
	spec.InitContainers = []kubeContainer{{}}
	if _, _, err := podRequests(spec); err == nil {
		t.Fatal("unsupported init-container scheduling was silently approximated")
	}
}

const nodeFixture = `{"items":[{"spec":{},"status":{"allocatable":{"cpu":"2","memory":"2Gi","pods":"20"},"conditions":[{"type":"Ready","status":"True"}]}}]}`
const podsFixture = `{"items":[{"spec":{"containers":[{"resources":{"requests":{"cpu":"200m","memory":"128Mi"}}}]},"status":{"phase":"Running"}},{"spec":{"containers":[{"resources":{"requests":{"cpu":"100m","memory":"128Mi"}}}]},"status":{"phase":"Pending"}},{"spec":{"containers":[{"resources":{"requests":{"cpu":"999","memory":"999Gi"}}}]},"status":{"phase":"Succeeded"}}]}`
const deploymentFixture = `{"metadata":{"generation":1},"spec":{"replicas":2,"template":{"spec":{"containers":[{"resources":{"requests":{"cpu":"100m","memory":"32Mi"}},"securityContext":{"privileged":false}}]}}},"status":{"observedGeneration":1,"replicas":2,"updatedReplicas":2,"readyReplicas":2,"availableReplicas":2}}`

func fakeKubectl(t *testing.T, nodes, pods, deployment string) string {
	t.Helper()
	return fakeCopilot(t, `
test "$1" = --context
test "$2" = test-cluster
case "$*" in
  *"get nodes -o json") printf '%s' '`+nodes+`' ;;
  *"get pods --all-namespaces -o json") printf '%s' '`+pods+`' ;;
  *"get deployment sample -o json") printf '%s' '`+deployment+`' ;;
  *) exit 9 ;;
esac`)
}

func TestReadOnlyClusterObservationsAndBlockedProposal(t *testing.T) {
	binary := fakeKubectl(t, nodeFixture, podsFixture, deploymentFixture)
	for _, name := range []string{"healthy", "capacity"} {
		regions, err := observeCluster(context.Background(), binary, "test-cluster", name, 3)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range regions {
			if r.Current != 2 || r.Cluster.ReadyReplicas != 2 || !r.Cluster.RolloutComplete ||
				r.Cluster.AvailableCPUMillicores != 1700 || r.Cluster.AvailableMemoryBytes != 1792*1024*1024 ||
				r.Cluster.AvailablePodSlots != 18 || r.FreeSlots != 8 {
				t.Fatalf("wrong observed capacity/readiness: %+v %+v", r, r.Cluster)
			}
			if !strings.Contains(r.Cluster.ErrorBudgetSource, "synthetic") ||
				!strings.Contains(r.Cluster.SignatureSource, "no cryptographic") {
				t.Fatal("synthetic evidence lacks provenance")
			}
			peer, _ := peerPolicy("surge-budget", r)
			got := evaluate("capacity", r, peer)
			want := "ready"
			if name == "capacity" {
				want = "blocked"
			}
			if got.State != want {
				t.Fatalf("%s: %+v", name, got)
			}
		}
	}
}

func TestClusterReadinessAndUnsupportedInputsFailClosed(t *testing.T) {
	unready := strings.ReplaceAll(deploymentFixture, `"readyReplicas":2`, `"readyReplicas":1`)
	regions, err := observeCluster(context.Background(), fakeKubectl(t, nodeFixture, podsFixture, unready), "test-cluster", "healthy", 3)
	if err != nil {
		t.Fatal(err)
	}
	peer, _ := peerPolicy("surge-budget", regions[0])
	if evaluate("capacity", regions[0], peer).State != "blocked" {
		t.Fatal("unready rollout approved")
	}
	for _, nodes := range []string{
		`{"items":[]}`,
		strings.ReplaceAll(nodeFixture, `"status":"True"`, `"status":"False"`),
		strings.ReplaceAll(nodeFixture, `"spec":{}`, `"spec":{"unschedulable":true}`),
		strings.ReplaceAll(nodeFixture, `"cpu":"2"`, `"cpu":"invalid"`),
	} {
		if _, err := observeCluster(context.Background(), fakeKubectl(t, nodes, podsFixture, deploymentFixture), "test-cluster", "healthy", 3); err == nil {
			t.Fatal("unsupported node accepted")
		}
	}
	if _, err := observeCluster(context.Background(), "must-not-run", "test-cluster", "unsafe-image", 3); err == nil {
		t.Fatal("synthetic scenario accepted in cluster mode")
	}
	if _, err := observeCluster(context.Background(), "must-not-run", "test-cluster", "healthy", 0); err == nil {
		t.Fatal("invalid proposed replicas accepted")
	}
}
