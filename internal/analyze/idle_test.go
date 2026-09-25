package analyze_test

import (
	"strings"
	"testing"

	"github.com/mfranzon/tare/internal/analyze"
	"github.com/mfranzon/tare/internal/simulate"
)

// The idle verdict is the only one whose action is deletion, so it is tested
// by discrimination rather than by assertion: two workloads whose percentiles
// are indistinguishable, one of which must be called idle and one of which
// must not. A test that only checked the positive case would pass just as
// happily on a rule that called everything quiet idle, which is the rule that
// deletes a nightly batch job.

func TestDormantWorkloadIsReportedAsIdle(t *testing.T) {
	f := observe(t, simulate.DormantSidecar, 11)
	if f.Verdict != analyze.Idle {
		t.Fatalf("verdict = %s, want IDLE (CPU peak %.1fm)", f.Verdict, f.CPUUsageMax*1000)
	}
	joined := strings.Join(analyze.ReasonTexts(f.Reasons), " ")
	if !strings.Contains(joined, "deleting it") {
		t.Errorf("an idle finding must name deletion as the larger saving; reasons: %v", f.Reasons)
	}
	// The limits of the evidence have to travel with it. tare sees CPU and
	// memory; it cannot see that a workload is a standby kept deliberately
	// warm, and a verdict recommending deletion that does not say so is
	// exactly the confident wrong answer this project keeps producing.
	if !strings.Contains(joined, "does NOT rule out") {
		t.Errorf("an idle finding must state what it cannot rule out; reasons: %v", f.Reasons)
	}
	// Idle is not a refusal: the smaller allocation is still correct if the
	// workload turns out to be needed.
	if f.RecCPU == 0 || f.RecMem == 0 {
		t.Errorf("idle must still size the container: cpu=%dm mem=%d", f.RecCPU, f.RecMem)
	}
}

// RareBatch is quiet at every percentile and busy for thirty ticks at a time.
// Deleting it would break a nightly job. This is the test that forces the idle
// rule to read the peak.
func TestRareBatchIsNotIdle(t *testing.T) {
	f := observe(t, simulate.RareBatch, 11)
	if f.Verdict == analyze.Idle {
		t.Fatalf("a workload that peaks at %.0fm must not be called idle (p95 %.2fm)",
			f.CPUUsageMax*1000, f.CPUUsageP95*1000)
	}
	// Guard the guard: if the archetype stopped spiking, the negative result
	// above would be meaningless and would pass for the wrong reason.
	if f.CPUUsageMax*1000 < 50 {
		t.Fatalf("rare-batch peaked at only %.1fm, so this test no longer discriminates anything",
			f.CPUUsageMax*1000)
	}
	if f.CPUUsageP95*1000 > 5 {
		t.Fatalf("rare-batch p95 is %.2fm, too busy to be confusable with an idle workload",
			f.CPUUsageP95*1000)
	}
}

// IdleHoarder draws 5m: little, but not nothing. It is the boundary case
// between the two verdicts, and it belongs on the WASTE side because there is
// a real process doing real work.
func TestSmallButWorkingIsWasteNotIdle(t *testing.T) {
	f := observe(t, simulate.IdleHoarder, 11)
	if f.Verdict == analyze.Idle {
		t.Errorf("a container drawing %.1fm is working, not idle", f.CPUUsageMax*1000)
	}
}

// Silence over a window that was never observed is not evidence of idleness.
// A container deployed an hour before the report runs must not be described as
// having done nothing for a fortnight.
func TestShortObservationIsNotIdle(t *testing.T) {
	short := 90 * 60 // 90 minutes at a 15s scrape ≈ 360 samples
	f := observe(t, simulate.DormantSidecar, 11)
	f.Samples = short / 15
	th := analyze.DefaultThresholds()
	if analyze.IsIdleForTest(&f, window, th) != "" {
		t.Errorf("%d samples over a %s window is not enough to call anything idle", f.Samples, window)
	}
}

// Measured on the lab cluster: node-exporter, kube-state-metrics, two Grafana
// sidecars and a config-reloader are all below the CPU threshold, all have
// unset requests, and all are load-bearing. Without this guard the report told
// the operator the larger saving was to delete their metrics pipeline.
func TestUnreservedContainersAreNotIdle(t *testing.T) {
	f := observe(t, simulate.DormantSidecar, 11)
	if f.Verdict != analyze.Idle {
		t.Fatalf("precondition: this archetype must be idle when it reserves something, got %s", f.Verdict)
	}
	th := analyze.DefaultThresholds()

	bare := f
	bare.CPURequest, bare.MemRequest = 0, 0
	if r := analyze.IsIdleForTest(&bare, window, th); r != "" {
		t.Errorf("a container reserving nothing frees nothing by being deleted: %s", r)
	}

	// A request at the recommendation floor is not a reservation worth
	// reporting either: the recommendation cannot go below it.
	floored := f
	floored.CPURequest, floored.MemRequest = th.MinCPUMillicores, th.MinMemBytes
	if r := analyze.IsIdleForTest(&floored, window, th); r != "" {
		t.Errorf("a request already at the floor has nothing to reclaim: %s", r)
	}

	// Memory alone is enough: a container reserving a gigabyte and doing
	// nothing is the case the verdict exists for, whatever its CPU request.
	memOnly := f
	memOnly.CPURequest, memOnly.MemRequest = 0, 1<<30
	if analyze.IsIdleForTest(&memOnly, window, th) == "" {
		t.Error("a container reserving 1Gi and doing nothing is idle")
	}
}
