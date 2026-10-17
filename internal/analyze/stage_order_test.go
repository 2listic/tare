package analyze_test

import (
	"testing"
	"time"

	"github.com/mfranzon/tare/internal/analyze"
	"github.com/mfranzon/tare/internal/discover"
	"github.com/mfranzon/tare/internal/metrics"
)

// runOne analyzes a single container with every signal present.
func runOne(t *testing.T, c discover.Container, o metrics.Observation, cl analyze.Cluster) analyze.Finding {
	t.Helper()
	o.Key = metrics.Key{Namespace: c.Namespace, Pod: c.Pod, Container: c.Name}
	if o.Samples == 0 {
		o.Samples = 1024
	}
	for _, s := range []metrics.Signal{
		metrics.SigCPUUsage, metrics.SigCPUPeak, metrics.SigMemory, metrics.SigMemP95,
		metrics.SigPSI, metrics.SigThrottle, metrics.SigThrottleAvg, metrics.SigOOM, metrics.SigRestarts,
	} {
		o.Mark(s)
	}
	fs := analyze.RunWithCluster([]discover.Container{c}, map[metrics.Key]*metrics.Observation{o.Key: &o}, cl, 14*24*time.Hour, analyze.DefaultThresholds())
	if len(fs) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(fs))
	}
	return fs[0]
}

func container(qos string, cpuReq, cpuLim int64, memReq, memLim int64) discover.Container {
	return discover.Container{
		Namespace: "lab", Pod: "app-7c9d-x2k", Name: "app", Workload: "Deployment/app",
		CPURequest: cpuReq, CPULimit: cpuLim, MemRequest: memReq, MemLimit: memLim, QoS: qos,
	}
}

// The Guaranteed hold raises the memory request to the limit. It ran after the
// node-size check, so a request no node could hold shipped without the
// UNSCHEDULABLE refusal: the check had passed the value before the raise.
func TestGuaranteedHoldIsCheckedAgainstTheLargestNode(t *testing.T) {
	th := analyze.DefaultThresholds()
	usage := 700.0 * mib
	request := int64(usage * (1 + th.Margin))
	limit := int64(float64(request) * (1 + th.MemLimitHeadroom))
	node := discover.Node{Name: "n1", Cores: 4, AllocCPU: 4000, AllocMem: (request + limit) / 2, Schedulable: true}

	f := runOne(t, container("Guaranteed", 1000, 1000, 4096*mib, 4096*mib), metrics.Observation{
		CPUUsageP95: 0.2, CPUUsageMax: 0.3, MemWorkingSet: usage, MemP95: usage,
	}, analyze.Cluster{Nodes: discover.Inventory{Nodes: []discover.Node{node}}})

	if f.RecMem != f.RecMemLimit || f.RecMem <= node.AllocMem {
		t.Fatalf("fixture drifted: RecMem=%d RecMemLimit=%d node=%d", f.RecMem, f.RecMemLimit, node.AllocMem)
	}
	if !hasCode(f, analyze.UnschedulableMem) {
		t.Errorf("held request %s exceeds the %s node and was not refused: %v",
			analyze.HumanBytes(f.RecMem), analyze.HumanBytes(node.AllocMem), analyze.ReasonTexts(f.Reasons))
	}
}

// The floor flags say a number is a constant rather than a measurement. The
// Guaranteed hold raised a floored memory request to its limit, and a VPA
// zeroed a floored CPU request, and both flags stayed set.
func TestFloorFlagsFollowTheFinalNumbers(t *testing.T) {
	tiny := metrics.Observation{CPUUsageP95: 0.001, CPUUsageMax: 0.002, MemWorkingSet: 1 * mib, MemP95: 1 * mib}

	t.Run("guaranteed hold", func(t *testing.T) {
		f := runOne(t, container("Guaranteed", 1000, 1000, 1024*mib, 1024*mib), tiny, analyze.Cluster{})
		if f.RecMem == analyze.DefaultThresholds().MinMemBytes {
			t.Fatalf("fixture drifted: the hold did not raise memory off the floor")
		}
		if f.MemFloored {
			t.Errorf("MemFloored set on %s, which the Guaranteed hold raised above the floor", analyze.HumanBytes(f.RecMem))
		}
	})
	t.Run("vpa", func(t *testing.T) {
		c := container("Burstable", 1000, 2000, 1024*mib, 2048*mib)
		cl := analyze.Cluster{Constraints: map[string]discover.Constraints{
			discover.ConstraintKey(c.Namespace, c.Workload): {VPAName: "app", VPAMode: "Auto"},
		}}
		f := runOne(t, c, tiny, cl)
		if f.RecCPU != 0 {
			t.Fatalf("fixture drifted: VPA did not withhold, RecCPU=%d", f.RecCPU)
		}
		if f.CPUFloored || f.MemFloored {
			t.Errorf("floor flags set on a withheld recommendation: cpu=%v mem=%v", f.CPUFloored, f.MemFloored)
		}
	})
}

// Starvation holds only the starved resource, and that skipped the restart
// hold entirely: a CPU-throttled container restarting every few hours was told
// to cut memory sized from the samples of a container that kept dying.
func TestRestartChurnHoldsWhatStarvationDoesNot(t *testing.T) {
	f := runOne(t, container("Burstable", 500, 500, 2048*mib, 2048*mib), metrics.Observation{
		CPUUsageP95: 0.49, CPUUsageMax: 0.5,
		MemWorkingSet: 100 * mib, MemP95: 100 * mib,
		ThrottleRatio: 0.6, ThrottleAvg: 0.3,
		Restarts: 12,
	}, analyze.Cluster{})

	if f.Verdict != analyze.Saturated {
		t.Fatalf("fixture drifted: want SATURATED on CPU, got %s", f.Verdict)
	}
	if f.RecMem < 2048*mib {
		t.Errorf("memory cut to %s on a container with 12 restarts", analyze.HumanBytes(f.RecMem))
	}
	if !hasCode(f, analyze.RestartChurn) {
		t.Errorf("memory held without saying why: no %s reason", analyze.RestartChurn)
	}
}

// "Raise the limit" on a container starved only of memory contradicts a
// recommendation that raises nothing on CPU.
func TestPinnedAtLimitNeedsCPUStarvation(t *testing.T) {
	f := runOne(t, container("Burstable", 500, 500, 256*mib, 256*mib), metrics.Observation{
		CPUUsageP95: 0.49, CPUUsageMax: 0.5,
		MemWorkingSet: 250 * mib, MemP95: 250 * mib,
		OOMEvents: 1,
	}, analyze.Cluster{})

	if f.Verdict != analyze.Saturated || !hasCode(f, analyze.OOMKilled) {
		t.Fatalf("fixture drifted: want SATURATED on memory, got %s %v", f.Verdict, f.Reasons)
	}
	if hasCode(f, analyze.CPUAtLimit) {
		t.Errorf("%s on a container with no CPU stall", analyze.CPUAtLimit)
	}
}

// Lowered limits must carry a reason just as lowered requests do, including on
// IDLE containers where classifyWaste does not run.
func TestLoweredLimitsCarryReasons(t *testing.T) {
	t.Run("waste container", func(t *testing.T) {
		f := runOne(t, container("Burstable", 100, 500, 256*mib, 1024*mib), metrics.Observation{
			CPUUsageP95: 0.001, CPUUsageMax: 0.005,
			MemWorkingSet: 63 * mib, MemP95: 60 * mib,
		}, analyze.Cluster{})
		if f.RecCPULimit >= 500 || f.RecMemLimit >= 1024*mib {
			t.Fatalf("fixture drifted: limits not lowered (%dm, %s)", f.RecCPULimit, analyze.HumanBytes(f.RecMemLimit))
		}
		if !hasCode(f, analyze.CPULimitUnderused) {
			t.Errorf("lowered CPU limit has no %s reason: %v", analyze.CPULimitUnderused, f.Reasons)
		}
		if !hasCode(f, analyze.MemLimitUnderused) {
			t.Errorf("lowered memory limit has no %s reason: %v", analyze.MemLimitUnderused, f.Reasons)
		}
	})
	t.Run("idle container", func(t *testing.T) {
		obs := metrics.Observation{
			CPUUsageP95: 0.0002, CPUUsageMax: 0.0004,
			MemWorkingSet: 10 * mib, MemP95: 10 * mib,
			MemEarly: 10 * mib, MemLate: 10 * mib,
			Samples: 60000,
		}
		obs.Mark(metrics.SigMemTrend)
		f := runOne(t, container("Burstable", 100, 100, 64*mib, 64*mib), obs, analyze.Cluster{})
		if f.Verdict != analyze.Idle {
			t.Fatalf("fixture drifted: want IDLE, got %s", f.Verdict)
		}
		if !hasCode(f, analyze.CPULimitUnderused) || !hasCode(f, analyze.MemLimitUnderused) {
			t.Errorf("IDLE container with lowered limits is missing limit reasons: %v", f.Reasons)
		}
	})
}
