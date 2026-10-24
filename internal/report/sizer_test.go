package report

import (
	"testing"

	"github.com/2listic/tare/internal/analyze"
	"github.com/2listic/tare/internal/discover"
)

func TestSizerSumsContainersOfAWorkload(t *testing.T) {
	size := Sizer([]analyze.Finding{
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
	size := Sizer([]analyze.Finding{
		{Namespace: "web", Workload: "Deployment/api", Container: "app", RecCPU: 300, RecMem: 512 << 20},
		{Namespace: "web", Workload: "Deployment/api", Container: "leaky", Verdict: analyze.Leaking},
	})
	if _, _, ok := size(discover.Pod{Namespace: "web", Workload: "Deployment/api"}); ok {
		t.Error("a workload with an unsized container must keep its current requests")
	}
}

func TestSizerIgnoresUnknownWorkloads(t *testing.T) {
	size := Sizer(nil)
	if _, _, ok := size(discover.Pod{Namespace: "web", Workload: "Deployment/ghost"}); ok {
		t.Error("a workload with no findings has no recommendation to apply")
	}
}

// IDLE is excluded from the totals because its idleness is not ruled out. The
// replay used to apply it anyway, so the node count rested on the very saving
// the totals refused to claim.
func TestSizerKeepsIdleWorkloadsAsTheyAre(t *testing.T) {
	size := Sizer([]analyze.Finding{
		{Namespace: "web", Workload: "Deployment/sync", Container: "worker", Verdict: analyze.Idle,
			CPURequest: 500, RecCPU: 10, MemRequest: 256 << 20, RecMem: 16 << 20},
	})
	if _, _, ok := size(discover.Pod{Namespace: "web", Workload: "Deployment/sync"}); ok {
		t.Error("an IDLE workload must keep its current requests in the replay")
	}
}
