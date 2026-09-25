package analyze_test

import (
	"strings"
	"testing"
	"time"

	"github.com/mfranzon/tare/internal/analyze"
	"github.com/mfranzon/tare/internal/discover"
	"github.com/mfranzon/tare/internal/metrics"
)

// slowLeaker reproduces tare-lab's slow-leaker as the long run actually
// observed it: a container that grows without bound, OOMKills, restarts, and
// grows again. The lab's cycle is about 41 hours, so a 7-day window contains
// four of them.
//
// The trend pair is the defect under test. MemEarly is an average over the
// hour at window-1h and MemLate over the last hour, and both are keyed by
// (namespace, pod, container) - not by container id. They therefore straddle
// every restart in between. Whether late/early clears the 1.5x leak ratio is
// decided by where in its sawtooth each end of the window happened to land,
// which is a fact about the sampling phase and not about the workload.
//
// cycle picks that phase: early is taken near the top of one tooth, late near
// the bottom of the next.
func slowLeaker(earlyMiB, lateMiB, restarts float64) ([]discover.Container, map[metrics.Key]*metrics.Observation) {
	key := metrics.Key{Namespace: "tare-lab", Pod: "slow-leaker-6d4f97c4b-x2tqp", Container: "leaker"}
	c := discover.Container{
		Namespace: key.Namespace, Pod: key.Pod, Name: key.Container,
		Workload:   "Deployment/slow-leaker",
		CPURequest: 50, CPULimit: 200,
		MemRequest: 256 * mib, MemLimit: 512 * mib,
		QoS: "Burstable",
	}
	o := &metrics.Observation{
		Key:           key,
		CPUUsageP95:   0.004,
		CPUUsageMax:   0.011,
		CPUSteadyMax:  0.011,
		MemWorkingSet: 509 * mib, // it reaches the limit, by construction
		MemP95:        480 * mib,
		MemEarly:      earlyMiB * mib,
		MemLate:       lateMiB * mib,
		MemLimitEarly: 512 * mib,
		MemLimitLate:  512 * mib,
		PSIWaitingAvg: 0.001,
		PSIWaitingMax: 0.004,
		OOMEvents:     restarts,
		Restarts:      restarts,
		Samples:       10096,
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

func leakerFinding(t *testing.T, early, late, restarts float64) analyze.Finding {
	t.Helper()
	cs, obs := slowLeaker(early, late, restarts)
	fs := analyze.Run(cs, obs, 7*24*time.Hour, analyze.DefaultThresholds())
	if len(fs) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(fs))
	}
	return fs[0]
}

// TestCyclicLeakIsNotReportedAsMerelySaturated is the open defect from the
// long run, in executable form.
//
// A leaker that OOMKills inside the window has its memory reset by the kill.
// Late over early then falls below the leak ratio, the LEAKING branch does not
// fire, and the OOMKilled branch below it reports SATURATED with "memory must
// not be reduced" - which an operator reads as "give it more memory". More
// memory buys a leak time and nothing else, and the report says nothing about
// growth at all.
//
// The verdict is the safe one, so nothing crashes and no test goes red. It is
// also the wrong diagnosis, delivered with no caveat: the same shape as every
// other defect in KNOWN-FAILURES.md.
func TestCyclicLeakIsNotReportedAsMerelySaturated(t *testing.T) {
	// 4 OOMKills in 7 days. Early lands high on one tooth (470MiB), late lands
	// low on the next (180MiB), so growth reads as 0.38x: shrinkage.
	f := leakerFinding(t, 470, 180, 4)
	t.Logf("verdict=%s recMem=%.0fMiB reasons=%v", f.Verdict, float64(f.RecMem)/mib, f.Reasons)

	if f.Verdict == analyze.Saturated && !mentionsGrowth(f) {
		t.Errorf("a container that OOMKilled %d times is reported %s with no mention of growth: %v",
			4, f.Verdict, f.Reasons)
	}
}

// TestNoSamplingPhaseYieldsACleanBill is the property the fix actually
// guarantees, and it is weaker than the one worth wanting.
//
// The obvious demand is that the verdict not depend on sampling phase at all.
// It is not reachable: with a pod-keyed trend pair, the two phases genuinely
// carry different information, and forcing them to agree means throwing away
// the one that is informative. Suppressing the trend on every restart was
// tried first and it silenced the simulator's leaky-service, which OOMKills
// once and is the case the leak test exists for.
//
// What must hold instead is that neither phase produces a confident "not
// leaking". The rising phase reports LEAKING and withholds a recommendation;
// the falling phase reports SATURATED and says growth cannot be ruled out. An
// operator reading either one is sent to look at the working set, and neither
// is told to simply raise memory.
func TestNoSamplingPhaseYieldsACleanBill(t *testing.T) {
	for _, tc := range []struct {
		name        string
		early, late float64
	}{
		{"rising", 110, 450},
		{"falling", 470, 180},
		{"flat", 300, 310},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := leakerFinding(t, tc.early, tc.late, 4)
			t.Logf("verdict=%s recMem=%.0fMiB", f.Verdict, float64(f.RecMem)/mib)
			if f.Verdict == analyze.Leaking {
				if f.RecMem != 0 {
					t.Errorf("LEAKING with a %.0fMiB recommendation", float64(f.RecMem)/mib)
				}
				return
			}
			if f.RecMem > f.MemRequest && !mentionsGrowth(f) {
				t.Errorf("raises memory from %.0fMiB to %.0fMiB for a container that OOMKilled 4 times, "+
					"with nothing said about growth: %v",
					float64(f.MemRequest)/mib, float64(f.RecMem)/mib, f.Reasons)
			}
		})
	}
}

// TestOrdinaryRestartsKeepTheTrend keeps the suppression narrow. A container
// that restarts without ever being OOMKilled - a rollout, a failing probe, a
// crash on bad input - has a trend worth reading, and blanketing every restart
// with a growth caveat would make the caveat mean nothing.
func TestOrdinaryRestartsKeepTheTrend(t *testing.T) {
	cs, obs := slowLeaker(300, 310, 13)
	for _, o := range obs {
		o.OOMEvents = 0 // restarted, never OOMKilled
	}
	fs := analyze.Run(cs, obs, 7*24*time.Hour, analyze.DefaultThresholds())
	f := fs[0]
	if mentionsGrowth(f) {
		t.Errorf("a container that restarted without OOMKilling got a growth caveat: %v", f.Reasons)
	}
}

func mentionsGrowth(f analyze.Finding) bool {
	for _, r := range analyze.ReasonTexts(f.Reasons) {
		if strings.Contains(r, "grew") || strings.Contains(r, "growth") || strings.Contains(r, "leak") {
			return true
		}
	}
	return false
}

// TestLeakWithoutRestartsStillFires is the negative control for the fix above.
// Withholding the trend across restarts is only defensible if the trend still
// works everywhere else: a container that grew 4x and has not yet been killed
// must still be LEAKING, and must still have its recommendation withheld.
func TestLeakWithoutRestartsStillFires(t *testing.T) {
	f := leakerFinding(t, 110, 450, 0)
	if f.Verdict != analyze.Leaking {
		t.Fatalf("4.1x growth with no restarts: expected LEAKING, got %s (%v)", f.Verdict, f.Reasons)
	}
	if f.RecMem != 0 {
		t.Errorf("a leak was given a memory recommendation of %.0fMiB; sizing cannot fix a leak",
			float64(f.RecMem)/mib)
	}
}

// TestCleanReplicaStillSuppliesTheTrend keeps the suppression per-pod. One
// replica restarting must not blind the workload: the other replica's pair is
// intact and is the one the leak test should read.
func TestCleanReplicaStillSuppliesTheTrend(t *testing.T) {
	cs, obs := slowLeaker(470, 180, 4) // restarted, trend unusable
	clean := metrics.Key{Namespace: "tare-lab", Pod: "slow-leaker-6d4f97c4b-qqq99", Container: "leaker"}
	c2 := cs[0]
	c2.Pod = clean.Pod
	cs = append(cs, c2)

	var o2 metrics.Observation
	for _, o := range obs {
		o2 = *o
	}
	o2.Key = clean
	o2.MemEarly, o2.MemLate = 110*mib, 450*mib // 4.1x, within one incarnation
	o2.Restarts, o2.OOMEvents = 0, 0
	obs[clean] = &o2

	fs := analyze.Run(cs, obs, 7*24*time.Hour, analyze.DefaultThresholds())
	if len(fs) != 1 {
		t.Fatalf("expected 1 finding for the workload, got %d", len(fs))
	}
	if fs[0].Verdict != analyze.Leaking {
		t.Errorf("the clean replica grew 4.1x and was ignored: verdict %s (%v)", fs[0].Verdict, fs[0].Reasons)
	}
}
