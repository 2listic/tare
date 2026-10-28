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

// A starvation the stalled share dilutes: serious, and short against the
// window. The rule cannot tell it from a load peak its own normal does not
// cover, so a reduction is held, and nothing is called SATURATED.
func TestCPUWaitStalledRunHoldsReduction(t *testing.T) {
	for name, c := range map[string]struct {
		stalled float64
		run     int
		want    analyze.Verdict
	}{
		"nightly batch, an hour a night": {0.04, 12, analyze.Unverified},
		"20 minutes once":                {0.002, 4, analyze.Unverified},
		"15 minutes once":                {0.002, 3, analyze.Waste},
	} {
		t.Run(name, func(t *testing.T) {
			w := starvedWait
			w.Stalled, w.LongestRun = c.stalled, c.run
			f := judgeWait(t, w)
			if f.Verdict != c.want {
				t.Fatalf("verdict %s, want %s: %v", f.Verdict, c.want, analyze.ReasonTexts(f.Reasons))
			}
			if c.want == analyze.Unverified {
				if !hasCode(f, analyze.CPUWaitStalledRun) {
					t.Errorf("held without saying why: %v", analyze.ReasonTexts(f.Reasons))
				}
				// The signal is sound: it is not reported as missing.
				if !f.HasCPUWait() || hasCode(f, analyze.NoSaturationSignal) {
					t.Errorf("CPU wait withdrawn for a stalled run: %v", analyze.ReasonTexts(f.Reasons))
				}
			}
		})
	}
}

// Starved by the share, a run is not news.
func TestCPUWaitStalledRunOnSaturated(t *testing.T) {
	w := starvedWait
	w.LongestRun = 50
	f := judgeWait(t, w)
	if f.Verdict != analyze.Saturated || hasCode(f, analyze.CPUWaitStalledRun) {
		t.Fatalf("verdict %s, reasons %v", f.Verdict, analyze.ReasonTexts(f.Reasons))
	}
}

// A container contended all the time has its contention for a normal, and
// nothing is "twice normal". Its CPU wait is not used, unless it already
// shows starvation.
func TestCPUWaitNeverQuiet(t *testing.T) {
	w := starvedWait
	w.Normal, w.Stalled = 0.95, 0
	f := judgeWait(t, w)
	if f.Verdict != analyze.Unverified || !hasCode(f, analyze.CPUWaitNeverQuiet) {
		t.Fatalf("always contended: verdict %s, reasons %v", f.Verdict, analyze.ReasonTexts(f.Reasons))
	}

	w.Stalled = 0.2
	if f := judgeWait(t, w); f.Verdict != analyze.Saturated || hasCode(f, analyze.CPUWaitNeverQuiet) {
		t.Fatalf("always contended and over the ceiling: verdict %s, reasons %v", f.Verdict, analyze.ReasonTexts(f.Reasons))
	}
}

// With node context, the node decides the two cases the relative rule cannot.
// A run its node confirmed is starvation however short against the window; a
// run the relative test alone found still only holds a reduction. A container
// with no quiet normal is starved if its node was contended (its stretches are
// confirmed, so its share is high) and judged by its normal if not.
func TestCPUWaitNodeContextVerdicts(t *testing.T) {
	for name, c := range map[string]struct {
		w    func(metrics.CPUWait) metrics.CPUWait
		want analyze.Verdict
		code analyze.Code
	}{
		"20 minutes in a week, node confirmed": {func(w metrics.CPUWait) metrics.CPUWait {
			w.Stalled, w.LongestRun, w.LongestConfirmedRun, w.NodeKnown = 0.002, 4, 4, 1
			return w
		}, analyze.Saturated, analyze.CPUWaitNodeConfirmed},
		"20 minutes in a week, relative only": {func(w metrics.CPUWait) metrics.CPUWait {
			w.Stalled, w.LongestRun, w.LongestConfirmedRun, w.NodeKnown = 0.002, 4, 0, 1
			return w
		}, analyze.Unverified, analyze.CPUWaitStalledRun},
		"15 minutes confirmed: under the run": {func(w metrics.CPUWait) metrics.CPUWait {
			w.Stalled, w.LongestRun, w.LongestConfirmedRun, w.NodeKnown = 0.0015, 3, 3, 1
			return w
		}, analyze.Waste, ""},
		"never quiet, node contended all along": {func(w metrics.CPUWait) metrics.CPUWait {
			w.Normal, w.Stalled, w.LongestConfirmedRun, w.NodeKnown, w.NodeContended = 0.95, 1, 2016, 1, 1
			return w
		}, analyze.Saturated, analyze.CPUWaitRelative},
		"never quiet, node not contended (k6, chatty)": {func(w metrics.CPUWait) metrics.CPUWait {
			w.Normal, w.Stalled, w.NodeKnown, w.NodeContended = 0.95, 0, 1, 0
			return w
		}, analyze.Waste, ""},
		"never quiet, node unknown": {func(w metrics.CPUWait) metrics.CPUWait {
			w.Normal, w.Stalled, w.NodeKnown = 0.95, 0, 0
			return w
		}, analyze.Unverified, analyze.CPUWaitNeverQuiet},
		"never quiet, node known for too little of the window": {func(w metrics.CPUWait) metrics.CPUWait {
			w.Normal, w.Stalled, w.NodeKnown = 0.95, 0, 0.3
			return w
		}, analyze.Unverified, analyze.CPUWaitNeverQuiet},
	} {
		t.Run(name, func(t *testing.T) {
			f := judgeWait(t, c.w(starvedWait))
			if f.Verdict != c.want {
				t.Fatalf("verdict %s, want %s: %v", f.Verdict, c.want, analyze.ReasonTexts(f.Reasons))
			}
			if c.code != "" && !hasCode(f, c.code) {
				t.Errorf("missing %s: %v", c.code, analyze.ReasonTexts(f.Reasons))
			}
			if c.want == analyze.Waste && (hasCode(f, analyze.CPUWaitNeverQuiet) || hasCode(f, analyze.CPUWaitStalledRun)) {
				t.Errorf("held without cause: %v", analyze.ReasonTexts(f.Reasons))
			}
		})
	}
}

// Confirmed starvation on a container using little of its allocation is the
// node's contention, not a shortage of its own: a reduction is allowed.
func TestCPUWaitNodeConfirmedBelowAllocation(t *testing.T) {
	w := starvedWait
	w.Stalled, w.LongestRun, w.LongestConfirmedRun, w.NodeKnown = 0.002, 4, 4, 1
	cs, obs := waitContainer(w)
	for _, o := range obs {
		o.CPUUsageMax, o.CPUSteadyMax = 0.1, 0.1
	}
	f := judge(t, cs, obs)
	if f.Verdict == analyze.Saturated || !hasCode(f, analyze.NodeContention) {
		t.Fatalf("verdict %s, reasons %v", f.Verdict, analyze.ReasonTexts(f.Reasons))
	}
}

// PSI decides wherever it exists: node context changes nothing there.
func TestCPUWaitNodeContextIgnoredWithPSI(t *testing.T) {
	w := starvedWait
	w.Stalled, w.LongestRun, w.LongestConfirmedRun, w.NodeKnown = 0.002, 4, 4, 1
	f := judgeWait(t, w, metrics.SigPSI)
	if hasCode(f, analyze.CPUWaitNodeConfirmed) {
		t.Fatalf("CPU wait read beside PSI: %v", analyze.ReasonTexts(f.Reasons))
	}
}
