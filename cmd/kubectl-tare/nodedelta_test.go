package main

import (
	"testing"

	"github.com/mfranzon/tare/internal/analyze"
	"github.com/mfranzon/tare/internal/discover"
)

func TestSizerSumsContainersOfAWorkload(t *testing.T) {
	size := sizerFor([]analyze.Finding{
		{Namespace: "web", Workload: "Deployment/api", Container: "app", RecCPU: 300, RecMem: 512 << 20},
		{Namespace: "web", Workload: "Deployment/api", Container: "sidecar", RecCPU: 50, RecMem: 64 << 20},
	})
	cpu, mem, ok := size(discover.Pod{Namespace: "web", Workload: "Deployment/api"})
	if !ok {
		t.Fatal("a fully sized workload must be usable")
	}
	if cpu != 350 || mem != (512+64)<<20 {
		t.Errorf("got %dm / %d bytes, want the sum of both containers", cpu, mem)
	}
}

// The trap: a pod reserves the sum of its containers, so a workload where the
// analyzer declined to size one container cannot be summed. Doing so hands the
// packer a pod smaller than any that could exist, and the node count that
// falls out is fiction.
func TestSizerRefusesPartiallySizedWorkloads(t *testing.T) {
	size := sizerFor([]analyze.Finding{
		{Namespace: "web", Workload: "Deployment/api", Container: "app", RecCPU: 300, RecMem: 512 << 20},
		{Namespace: "web", Workload: "Deployment/api", Container: "leaky", Verdict: analyze.Leaking},
	})
	if _, _, ok := size(discover.Pod{Namespace: "web", Workload: "Deployment/api"}); ok {
		t.Error("a workload with an unsized container must keep its current requests")
	}
}

func TestSizerIgnoresUnknownWorkloads(t *testing.T) {
	size := sizerFor(nil)
	if _, _, ok := size(discover.Pod{Namespace: "web", Workload: "Deployment/ghost"}); ok {
		t.Error("a workload with no findings has no recommendation to apply")
	}
}
