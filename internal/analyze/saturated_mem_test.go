package analyze_test

import (
	"strings"
	"testing"
	"time"

	"github.com/mfranzon/tare/internal/analyze"
	"github.com/mfranzon/tare/internal/discover"
	"github.com/mfranzon/tare/internal/metrics"
)

// resizeTarget reproduces the long-run cluster's resize-target container as
// observed on 2026-09-15: a JVM reported SATURATED on PSI (0.0343, just over
// the 0.03 threshold) while drawing 1.5m of CPU against a 100m request, and
// whose memory is entirely comfortable — 65MiB working set against 512Mi.
func resizeTarget() ([]discover.Container, map[metrics.Key]*metrics.Observation) {
	key := metrics.Key{Namespace: "tare-lab", Pod: "resize-target-bc487b989-bmbqs", Container: "app"}
	c := discover.Container{
		Namespace: key.Namespace, Pod: key.Pod, Name: key.Container,
		Workload:   "Deployment/resize-target",
		CPURequest: 100, CPULimit: 500,
		MemRequest: 512 * mib, MemLimit: 1 * gib,
		QoS: "Burstable",
	}
	o := &metrics.Observation{
		Key:           key,
		CPUUsageP95:   0.00147,
		CPUUsageMax:   0.0021,
		CPUSteadyMax:  0.0021,
		MemWorkingSet: 65.3 * mib,
		MemP95:        65.1 * mib,
		MemEarly:      64.0 * mib,
		MemLate:       64.4 * mib,
		MemLimitEarly: 512 * mib,
		MemLimitLate:  1 * gib,
		PSIWaitingMax: 0.0343,
		Samples:       1024,
	}
	for _, s := range []metrics.Signal{
		metrics.SigCPUUsage, metrics.SigCPUPeak, metrics.SigMemory, metrics.SigMemP95,
		metrics.SigPSI, metrics.SigOOM, metrics.SigRestarts, metrics.SigMemTrend,
		metrics.SigLimitTrend,
	} {
		o.Have = markSignal(o.Have, s)
	}
	return []discover.Container{c}, map[metrics.Key]*metrics.Observation{key: o}
}

// starved returns the same container actually using what it was given, which
// is what separates a starved workload from one merely watching its neighbours
// fight over the node.
func starved() ([]discover.Container, map[metrics.Key]*metrics.Observation) {
	cs, obs := resizeTarget()
	for _, o := range obs {
		o.CPUUsageP95, o.CPUUsageMax, o.CPUSteadyMax = 0.48, 0.50, 0.50
	}
	return cs, obs
}

func markSignal(m map[metrics.Signal]bool, s metrics.Signal) map[metrics.Signal]bool {
	if m == nil {
		m = map[metrics.Signal]bool{}
	}
	m[s] = true
	return m
}

// TestCPUSaturationDoesNotInflateMemory is the bug from the long run: a
// container saturated on CPU has its memory recommendation ratcheted up to the
// current request, discarding the memory measurement entirely.
func TestCPUSaturationDoesNotInflateMemory(t *testing.T) {
	cs, obs := starved()
	fs := analyze.Run(cs, obs, 14*24*time.Hour, analyze.DefaultThresholds())
	if len(fs) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(fs))
	}
	f := fs[0]
	t.Logf("verdict=%s recCPU=%d recCPULimit=%d recMem=%d (%.0fMiB) recMemLimit=%d (%.0fMiB) cpuFloored=%v memFloored=%v",
		f.Verdict, f.RecCPU, f.RecCPULimit, f.RecMem, float64(f.RecMem)/mib,
		f.RecMemLimit, float64(f.RecMemLimit)/mib, f.CPUFloored, f.MemFloored)

	if f.Verdict != analyze.Saturated {
		t.Fatalf("precondition: expected SATURATED, got %s", f.Verdict)
	}
	// 65MiB observed. Anything at or above the 512Mi request is the request
	// echoed back, not a measurement.
	if f.RecMem >= 512*mib {
		t.Errorf("memory recommendation is %.0fMiB for a container using 65MiB; it is the %dMiB request echoed back",
			float64(f.RecMem)/mib, 512)
	}
	if f.CPUFloored && f.RecCPU > analyze.DefaultThresholds().MinCPUMillicores {
		t.Errorf("CPUFloored is set but RecCPU is %dm, above the %dm floor: the flag is stale",
			f.RecCPU, analyze.DefaultThresholds().MinCPUMillicores)
	}
}

// TestOOMKillDoesNotInflateCPU is the same defect seen from the other side: an
// OOMKill is evidence about memory, and must not freeze the CPU request of a
// container drawing 1.5m against 100m.
func TestOOMKillDoesNotInflateCPU(t *testing.T) {
	cs, obs := starved()
	for _, o := range obs {
		o.PSIWaitingMax, o.PSIWaitingAvg = 0, 0 // no CPU pressure at all
		o.CPUUsageP95, o.CPUUsageMax, o.CPUSteadyMax = 0.00147, 0.0021, 0.0021
		o.OOMEvents = 3
	}
	f := analyze.Run(cs, obs, 14*24*time.Hour, analyze.DefaultThresholds())[0]
	if f.Verdict != analyze.Saturated {
		t.Fatalf("precondition: expected SATURATED, got %s", f.Verdict)
	}
	if f.RecMem < 512*mib {
		t.Errorf("memory was ratcheted down to %.0fMiB after an OOMKill", float64(f.RecMem)/mib)
	}
	if f.RecCPU >= 100 {
		t.Errorf("CPU recommendation is %dm for a container using 1.5m; an OOMKill says nothing about CPU", f.RecCPU)
	}
}

// TestBothStarvedRatchetsBoth: a container that is OOMKilled *and* CPU-starved
// must keep both floors. Splitting the ratchet must not lose this case.
func TestBothStarvedRatchetsBoth(t *testing.T) {
	cs, obs := starved()
	for _, o := range obs {
		o.PSIWaitingMax = 0.5
		o.OOMEvents = 3
	}
	f := analyze.Run(cs, obs, 14*24*time.Hour, analyze.DefaultThresholds())[0]
	if f.RecCPU < 100 || f.RecMem < 512*mib {
		t.Errorf("starved on both, but got recCPU=%dm recMem=%.0fMiB; neither may shrink",
			f.RecCPU, float64(f.RecMem)/mib)
	}
}

// TestTrendPairsStayWithinOnePod: a workload with one leaking replica and one
// freshly-rolled replica must still be reported LEAKING, whichever order the
// pods arrive in. Before the fix the trend pair was overwritten by the last pod
// seen, so a rollout could hide a leak, and a leak could be manufactured by
// pairing one pod's early average with another's late one.
func TestTrendPairsStayWithinOnePod(t *testing.T) {
	mk := func(pod string, early, late float64) (discover.Container, *metrics.Observation) {
		k := metrics.Key{Namespace: "ns", Pod: pod, Container: "app"}
		c := discover.Container{
			Namespace: "ns", Pod: pod, Name: "app", Workload: "Deployment/w",
			CPURequest: 100, CPULimit: 200, MemRequest: 512 * mib, MemLimit: 1 * gib,
		}
		o := &metrics.Observation{
			Key: k, CPUUsageP95: 0.05, CPUUsageMax: 0.06,
			MemWorkingSet: late, MemP95: late * 0.98,
			MemEarly: early, MemLate: late, Samples: 1024,
		}
		for _, s := range []metrics.Signal{
			metrics.SigCPUUsage, metrics.SigCPUPeak, metrics.SigMemory,
			metrics.SigMemP95, metrics.SigPSI, metrics.SigMemTrend,
		} {
			o.Have = markSignal(o.Have, s)
		}
		return c, o
	}

	// leaky: 100MiB -> 400MiB. fresh: no early observation at all.
	leakyC, leakyO := mk("w-leaky", 100*mib, 400*mib)
	freshC, freshO := mk("w-fresh", 0, 90*mib)

	for _, order := range [][]discover.Container{
		{leakyC, freshC},
		{freshC, leakyC}, // the order that used to lose the leak
	} {
		obs := map[metrics.Key]*metrics.Observation{
			leakyO.Key: leakyO, freshO.Key: freshO,
		}
		f := analyze.Run(order, obs, 14*24*time.Hour, analyze.DefaultThresholds())[0]
		if f.Verdict != analyze.Leaking {
			t.Errorf("pods in order %s,%s: got %s, want LEAKING (early=%.0fMiB late=%.0fMiB)",
				order[0].Pod, order[1].Pod, f.Verdict,
				f.MemEarly/mib, f.MemLate/mib)
		}
	}
}

// TestPressureWithoutDemandIsNotStarvation is the long run's resize-target,
// measured on the cluster 2026-09-15: a JVM stalling about one minute an hour
// while drawing 1.5m of CPU against a 100m request and a 500m limit. Its worst
// bucket clears the 0.03 threshold, so pressure alone called it starved, and
// the saturation ratchet then pinned its recommendation to its current request
// for as long as the run lasted.
//
// No pressure statistic separates it from a genuinely mildly-starved container:
// the simulator's own "mildly-starved" scenario stalls for a near-identical
// fraction of the window. What separates them is whether the container was
// using what it was given.
func TestPressureWithoutDemandIsNotStarvation(t *testing.T) {
	cases := []struct {
		name  string
		usage float64 // cores, peak
		psi   float64
		want  analyze.Verdict
	}{
		// 0.5m..2m against a 500m limit: cannot be short of CPU it never asked for.
		{"idle jvm, gc pause", 0.0021, 0.0309, analyze.Waste},
		// Using its whole allocation and stalling: genuinely starved.
		{"at its allocation", 0.50, 0.0309, analyze.Saturated},
		// Severe.
		{"badly starved", 0.50, 0.8999, analyze.Saturated},
		// Using its allocation but not stalling: merely busy, not starved.
		{"busy but unstalled", 0.50, 0.0010, analyze.Waste},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cs, obs := resizeTarget()
			for _, o := range obs {
				o.CPUUsageP95 = tc.usage * 0.96
				o.CPUUsageMax, o.CPUSteadyMax = tc.usage, tc.usage
				o.PSIWaitingMax = tc.psi
			}
			f := analyze.Run(cs, obs, 14*24*time.Hour, analyze.DefaultThresholds())[0]
			if f.Verdict != tc.want {
				t.Errorf("usage %.4f cores of a 500m limit, psi %.4f: verdict %s, want %s",
					tc.usage, tc.psi, f.Verdict, tc.want)
			}
		})
	}
}

// TestSuppressedPressureIsExplained: a pressure reading that did not become a
// verdict must still reach the reader. Dropping it silently would hide real
// node contention behind a clean WASTE finding.
func TestSuppressedPressureIsExplained(t *testing.T) {
	cs, obs := resizeTarget()
	f := analyze.Run(cs, obs, 14*24*time.Hour, analyze.DefaultThresholds())[0]
	var explained bool
	for _, r := range analyze.ReasonTexts(f.Reasons) {
		if strings.Contains(r, "contention on the node") {
			explained = true
		}
	}
	if !explained {
		t.Errorf("a suppressed pressure reading must be explained; reasons were: %v", f.Reasons)
	}
}
