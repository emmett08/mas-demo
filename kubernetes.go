package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

type resourceRequests struct {
	CPU    string `json:"cpu"`
	Memory string `json:"memory"`
	Pods   string `json:"pods"`
}

type kubeContainer struct {
	Resources struct {
		Requests resourceRequests `json:"requests"`
	} `json:"resources"`
	SecurityContext struct {
		Privileged bool `json:"privileged"`
	} `json:"securityContext"`
}

type podSpec struct {
	NodeName       string           `json:"nodeName"`
	Containers     []kubeContainer  `json:"containers"`
	InitContainers []kubeContainer  `json:"initContainers"`
	Overhead       resourceRequests `json:"overhead"`
}

type kubeNode struct {
	Spec struct {
		Unschedulable bool `json:"unschedulable"`
		Taints        []struct {
			Effect string `json:"effect"`
		} `json:"taints"`
	} `json:"spec"`
	Status struct {
		Allocatable resourceRequests `json:"allocatable"`
		Conditions  []struct {
			Type   string `json:"type"`
			Status string `json:"status"`
		} `json:"conditions"`
	} `json:"status"`
}

type kubePod struct {
	Spec   podSpec `json:"spec"`
	Status struct {
		Phase string `json:"phase"`
	} `json:"status"`
}

type kubeDeployment struct {
	Metadata struct {
		Generation int `json:"generation"`
	} `json:"metadata"`
	Spec struct {
		Replicas int `json:"replicas"`
		Template struct {
			Spec podSpec `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
	Status struct {
		ObservedGeneration int `json:"observedGeneration"`
		Replicas           int `json:"replicas"`
		UpdatedReplicas    int `json:"updatedReplicas"`
		ReadyReplicas      int `json:"readyReplicas"`
		AvailableReplicas  int `json:"availableReplicas"`
	} `json:"status"`
}

type clusterObservation struct {
	Context                string `json:"context"`
	Namespace              string `json:"namespace"`
	Deployment             string `json:"deployment"`
	ReadyReplicas          int    `json:"ready_replicas"`
	AvailableReplicas      int    `json:"available_replicas"`
	RolloutComplete        bool   `json:"rollout_complete"`
	CPURequestMillicores   int    `json:"cpu_request_millicores"`
	MemoryRequestBytes     int    `json:"memory_request_bytes"`
	AvailableCPUMillicores int    `json:"available_cpu_millicores"`
	AvailableMemoryBytes   int    `json:"available_memory_bytes"`
	AvailablePodSlots      int    `json:"available_pod_slots"`
	CapacityBasis          string `json:"capacity_basis"`
	ErrorBudgetSource      string `json:"error_budget_source"`
	SignatureSource        string `json:"signature_source"`
}

var quantityPattern = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)([a-zA-Z]*)$`)

// Round requests upward and allocatable downward, never inventing capacity.
func quantity(value string, cpu, roundUp bool) (int, error) {
	match := quantityPattern.FindStringSubmatch(value)
	if match == nil {
		return 0, fmt.Errorf("unsupported resource quantity %q", value)
	}
	scales := map[string]string{"": "1", "Ki": "1024", "Mi": "1048576", "Gi": "1073741824", "Ti": "1099511627776", "k": "1000", "M": "1000000", "G": "1000000000", "m": "1/1000", "u": "1/1000000", "n": "1/1000000000"}
	scale, ok := new(big.Rat).SetString(scales[match[2]])
	if !ok {
		return 0, fmt.Errorf("unsupported resource suffix %q", match[2])
	}
	number, _ := new(big.Rat).SetString(match[1])
	number.Mul(number, scale)
	if cpu {
		number.Mul(number, big.NewRat(1000, 1))
	}
	whole, remainder := new(big.Int), new(big.Int)
	whole.QuoRem(number.Num(), number.Denom(), remainder)
	if roundUp && remainder.Sign() != 0 {
		whole.Add(whole, big.NewInt(1))
	}
	if !whole.IsInt64() || whole.Int64() > 1<<50 {
		return 0, fmt.Errorf("resource quantity exceeds demo bounds")
	}
	return int(whole.Int64()), nil
}

func podRequests(spec podSpec) (int, int, error) {
	if len(spec.InitContainers) != 0 {
		return 0, 0, fmt.Errorf("demo capacity estimator does not support init containers")
	}
	cpu, memory := 0, 0
	add := func(requests resourceRequests) error {
		for _, resource := range []struct {
			value  string
			cpu    bool
			target *int
		}{
			{requests.CPU, true, &cpu}, {requests.Memory, false, &memory},
		} {
			if resource.value == "" {
				continue
			}
			value, err := quantity(resource.value, resource.cpu, true)
			if err != nil {
				return err
			}
			*resource.target += value
			if *resource.target > 1<<50 {
				return fmt.Errorf("pod resource requests exceed demo bounds")
			}
		}
		return nil
	}
	for _, container := range spec.Containers {
		if err := add(container.Resources.Requests); err != nil {
			return 0, 0, err
		}
	}
	if err := add(spec.Overhead); err != nil {
		return 0, 0, err
	}
	return cpu, memory, nil
}

func kubectlJSON(ctx context.Context, binary, kubeContext string, output any, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, binary, append([]string{"--context", kubeContext, "--request-timeout=10s"}, args...)...)
	stdout := &boundedOutput{limit: 16 << 20, cancel: cancel}
	command.Stdout = stdout
	command.WaitDelay = time.Second
	if err := command.Run(); err != nil {
		return fmt.Errorf("read-only kubectl query failed: %w", err)
	}
	if stdout.overflow {
		return fmt.Errorf("kubectl response exceeded 16 MiB")
	}
	return json.Unmarshal(stdout.buffer.Bytes(), output)
}

func observeCluster(ctx context.Context, binary, kubeContext, scenarioName string, desired int) ([]region, error) {
	if scenarioName != "healthy" && scenarioName != "capacity" {
		return nil, fmt.Errorf("cluster mode supports healthy and capacity scenarios")
	}
	if desired < 1 || desired > 100 {
		return nil, fmt.Errorf("cluster-desired must be 1..100")
	}
	var nodes struct {
		Items []kubeNode `json:"items"`
	}
	var pods struct {
		Items []kubePod `json:"items"`
	}
	if err := kubectlJSON(ctx, binary, kubeContext, &nodes, "get", "nodes", "-o", "json"); err != nil {
		return nil, err
	}
	if len(nodes.Items) != 1 {
		return nil, fmt.Errorf("demo estimator requires exactly one node, not a production scheduler")
	}
	node := nodes.Items[0]
	ready := false
	for _, condition := range node.Status.Conditions {
		ready = ready || (condition.Type == "Ready" && condition.Status == "True")
	}
	if !ready || node.Spec.Unschedulable || len(node.Spec.Taints) != 0 {
		return nil, fmt.Errorf("demo node must be Ready, schedulable, and untainted")
	}
	if err := kubectlJSON(ctx, binary, kubeContext, &pods, "get", "pods", "--all-namespaces", "-o", "json"); err != nil {
		return nil, err
	}
	availableCPU, err := quantity(node.Status.Allocatable.CPU, true, false)
	if err != nil {
		return nil, err
	}
	availableMemory, err := quantity(node.Status.Allocatable.Memory, false, false)
	if err != nil {
		return nil, err
	}
	availablePods, err := quantity(node.Status.Allocatable.Pods, false, false)
	if err != nil {
		return nil, err
	}
	for _, pod := range pods.Items {
		if pod.Status.Phase == "Succeeded" || pod.Status.Phase == "Failed" {
			continue
		}
		// Include Pending pods as reserved demand, even before node assignment.
		cpu, memory, err := podRequests(pod.Spec)
		if err != nil {
			return nil, err
		}
		availableCPU = max(0, availableCPU-cpu)
		availableMemory = max(0, availableMemory-memory)
		availablePods = max(0, availablePods-1)
	}
	var regions []region
	for _, name := range []string{"east", "west"} {
		namespace := "mas-" + name
		var deployment kubeDeployment
		if err := kubectlJSON(ctx, binary, kubeContext, &deployment, "-n", namespace, "get", "deployment", "sample", "-o", "json"); err != nil {
			return nil, err
		}
		cpu, memory, err := podRequests(deployment.Spec.Template.Spec)
		if err != nil || cpu <= 0 || memory <= 0 {
			return nil, fmt.Errorf("sample deployment needs positive CPU/memory requests and no init containers")
		}
		for _, container := range deployment.Spec.Template.Spec.Containers {
			if container.Resources.Requests.CPU == "" || container.Resources.Requests.Memory == "" {
				return nil, fmt.Errorf("each sample container needs CPU and memory requests")
			}
		}
		if desired < deployment.Spec.Replicas {
			return nil, fmt.Errorf("demo supports scale-up only")
		}
		// Split the shared single-node headroom so regions cannot double-spend it.
		free := min(availableCPU/cpu, availableMemory/memory, availablePods) / 2
		observation := &clusterObservation{
			Context: kubeContext, Namespace: namespace, Deployment: "sample",
			ReadyReplicas: deployment.Status.ReadyReplicas, AvailableReplicas: deployment.Status.AvailableReplicas,
			RolloutComplete: deployment.Status.ObservedGeneration >= deployment.Metadata.Generation &&
				deployment.Status.Replicas == deployment.Spec.Replicas &&
				deployment.Status.UpdatedReplicas == deployment.Spec.Replicas &&
				deployment.Status.ReadyReplicas == deployment.Spec.Replicas &&
				deployment.Status.AvailableReplicas == deployment.Spec.Replicas,
			CPURequestMillicores: cpu, MemoryRequestBytes: memory,
			AvailableCPUMillicores: availableCPU, AvailableMemoryBytes: availableMemory, AvailablePodSlots: availablePods,
			CapacityBasis:     "single-node allocatable minus nonterminal pod requests, overhead and pending demand; half the pod-equivalent headroom per region; not scheduler feasibility",
			ErrorBudgetSource: "synthetic demo policy: 60%; not measured SLO telemetry",
			SignatureSource:   "synthetic demo attestation: accepted; no cryptographic signature verification",
		}
		proposed := desired
		if scenarioName == "capacity" {
			proposed = deployment.Spec.Replicas + free + 1
		}
		privileged := false
		for _, container := range deployment.Spec.Template.Spec.Containers {
			privileged = privileged || container.SecurityContext.Privileged
		}
		regions = append(regions, region{
			Name: name, Current: deployment.Spec.Replicas, Desired: proposed, Surge: 1,
			FreeSlots: free, Budget: 60, Telemetry: true, Signed: true,
			Privileged: privileged, PolicyMaxSurge: 2, Cluster: observation,
		})
	}
	return regions, nil
}

func clusterDescription(observation *clusterObservation) string {
	if observation == nil {
		return ""
	}
	return strings.Join([]string{"Kubernetes observations", observation.ErrorBudgetSource, observation.SignatureSource}, "; ")
}
