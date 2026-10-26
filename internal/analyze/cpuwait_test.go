package analyze_test

import (
	"testing"
	"time"

	"github.com/mfranzon/tare/internal/analyze"
	"github.com/mfranzon/tare/internal/discover"
	"github.com/mfranzon/tare/internal/metrics"
)

// waitContainer is a 1-core container on a node without PSI, busy enough to
// use its allocation (peak 0.9 cores), with a request far above its p95 so that
// without a saturation verdict it would be WASTE.
func waitContainer(w metrics.CPUWait, extra ...metrics.Signal) ([]discover.Container, map[metrics.Key]*metrics.Observation) {
	key := metrics.Key{Namespace: "shop", Pod: "api-0", Container: "api"}
	c := discover.Container{
		Namespace: key.Namespace, Pod: key.Pod, Name: key.Container,
		Workload:   "StatefulSet/api",
		CPURequest: 1000, MemRequest: 1024 * mib,
	}
	o := &metrics.Observation{
		Key:           key,
		CPUUsageP95:   0.2,
		CPUUsageMax:   0.9,
		CPUSteadyMax:  0.9,
		MemWorkingSet: 100 * mib,
		MemP95:        100 * mib,
		CPUWait:       w,
		Samples:       40000,
	}
	for _, s := range append([]metrics.Signal{
		metrics.SigCPUUsage, metrics.SigCPUPeak, metrics.SigMemory, metrics.SigMemP95,
		metrics.SigOOM, metrics.SigRestarts, metrics.SigCPUWait,
	}, extra...) {
		o.Mark(s)
	}
	return []discover.Container{c}, map[metrics.Key]*metrics.Observation{key: o}
}

// judgeWait runs one waitContainer.
func judgeWait(t *testing.T, w metrics.CPUWait, extra ...metrics.Signal) analyze.Finding {
	t.Helper()
	cs, obs := waitContainer(w, extra...)
	return judge(t, cs, obs)
}

func judge(t *testing.T, cs []discover.Container, obs map[metrics.Key]*metrics.Observation) analyze.Finding {
	t.Helper()
	fs := analyze.Run(cs, obs, 7*24*time.Hour, analyze.DefaultThresholds())
	if len(fs) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(fs))
	}
	return fs[0]
}

var starvedWait = metrics.CPUWait{Stretches: 2016, Stalled: 0.2, Normal: 0.05, HasNormal: true, Mean: 0.3, Coverage: 0.85, Busy: true}

func TestCPUWaitSaturates(t *testing.T) {
	f := judgeWait(t, starvedWait)
	if f.Verdict != analyze.Saturated || !hasCode(f, analyze.CPUWaitRelative) {
		t.Fatalf("starved by CPU wait: verdict %s, reasons %v", f.Verdict, analyze.ReasonTexts(f.Reasons))
	}
	if f.RecCPU < f.CPURequest {
		t.Errorf("CPU of a starved container reduced to %dm", f.RecCPU)
	}
}

// Below the stalled share the container has a usable saturation signal and no
// starvation, so it is judged on usage like any container with PSI.
func TestCPUWaitQuietAllowsShrink(t *testing.T) {
	w := starvedWait
	w.Stalled = 0.01
	f := judgeWait(t, w)
	if f.Verdict != analyze.Waste {
		t.Fatalf("quiet container with CPU wait: verdict %s, want WASTE; %v", f.Verdict, analyze.ReasonTexts(f.Reasons))
	}
	if hasCode(f, analyze.NoSaturationSignal) {
		t.Error("CPU wait present and usable, yet reported as no saturation signal")
	}
}

// PSI decides wherever it exists. The agent's opinion beside it is not read.
func TestPSIWinsOverCPUWait(t *testing.T) {
	f := judgeWait(t, starvedWait, metrics.SigPSI, metrics.SigPSIAvg)
	if f.Verdict == analyze.Saturated || hasCode(f, analyze.CPUWaitRelative) {
		t.Fatalf("CPU wait overrode a calm PSI: verdict %s", f.Verdict)
	}
}

// Agent data that cannot be believed is withdrawn, with the reason, and the
// container is as unverifiable as without the agent.
func TestUnusableCPUWaitIsWithdrawn(t *testing.T) {
	short, blind := starvedWait, starvedWait
	short.Stretches, short.Stalled = 100, 0
	blind.Coverage, blind.Stalled = 0.05, 0
	for name, c := range map[string]struct {
		w    metrics.CPUWait
		code analyze.Code
	}{
		"short history": {short, analyze.CPUWaitShortHistory},
		"low coverage":  {blind, analyze.CPUWaitLowCoverage},
	} {
		t.Run(name, func(t *testing.T) {
			f := judgeWait(t, c.w)
			if f.Verdict != analyze.Unverified {
				t.Errorf("verdict %s, want UNVERIFIED", f.Verdict)
			}
			if !hasCode(f, c.code) || !hasCode(f, analyze.NoSaturationSignal) {
				t.Errorf("withdrawn without saying why: %v", analyze.ReasonTexts(f.Reasons))
			}
		})
	}
}

// An idle container has no coverage to check, and its CPU wait still counts.
func TestIdleContainerCoverageNotRequired(t *testing.T) {
	w := starvedWait
	w.Stalled, w.Busy, w.Coverage = 0, false, 0
	f := judgeWait(t, w)
	if hasCode(f, analyze.CPUWaitLowCoverage) || hasCode(f, analyze.NoSaturationSignal) {
		t.Errorf("idle container lost its CPU wait: %v", analyze.ReasonTexts(f.Reasons))
	}
}

// Stalled while using a fraction of its allocation is the node's contention,
// as for PSI: a bigger request would not help.
func TestCPUWaitNodeContention(t *testing.T) {
	cs, obs := waitContainer(starvedWait)
	for _, o := range obs {
		o.CPUUsageMax, o.CPUSteadyMax = 0.1, 0.1
	}
	f := judge(t, cs, obs)
	if f.Verdict == analyze.Saturated || !hasCode(f, analyze.NodeContention) {
		t.Fatalf("verdict %s, reasons %v; want NODE_CONTENTION and no SATURATED", f.Verdict, analyze.ReasonTexts(f.Reasons))
	}
}

// One replica the agent cannot see is enough to doubt the workload.
func TestCPUWaitAcrossPodsTakesWorstCoverage(t *testing.T) {
	cs, obs := waitContainer(starvedWait)
	c2 := cs[0]
	c2.Pod = "api-1"
	o2 := *obs[metrics.Key{Namespace: "shop", Pod: "api-0", Container: "api"}]
	o2.Key.Pod = "api-1"
	o2.CPUWait.Coverage = 0.1
	obs[o2.Key] = &o2
	f := judge(t, append(cs, c2), obs)
	if !hasCode(f, analyze.CPUWaitLowCoverage) {
		t.Errorf("low coverage on one replica ignored: %v", analyze.ReasonTexts(f.Reasons))
	}
}
