package analyze_test

import (
	"strings"
	"testing"
	"time"

	"github.com/2listic/tare/internal/analyze"
	"github.com/2listic/tare/internal/discover"
	"github.com/2listic/tare/internal/metrics"
)

// wasteful is a comfortable, over-provisioned container whose newest sample
// is at lastSeen: the shape that reads as a confident WASTE whatever its age.
func wasteful(lastSeen time.Time, pod string) (discover.Container, *metrics.Observation) {
	c := container("Burstable", 1000, 0, 1024*mib, 0)
	c.Pod = pod
	o := &metrics.Observation{
		Key:         metrics.Key{Namespace: c.Namespace, Pod: pod, Container: c.Name},
		CPUUsageP95: 0.05, CPUUsageMax: 0.08,
		MemWorkingSet: 100 * mib, MemP95: 95 * mib,
		Samples:   80000,
		FirstSeen: float64(lastSeen.Add(-14 * 24 * time.Hour).Unix()),
		LastSeen:  float64(lastSeen.Unix()),
	}
	for _, s := range []metrics.Signal{
		metrics.SigCPUUsage, metrics.SigCPUPeak, metrics.SigMemory, metrics.SigMemP95,
		metrics.SigPSI, metrics.SigOOM, metrics.SigRestarts,
	} {
		o.Mark(s)
	}
	return c, o
}

func runAt(now time.Time, pairs ...func() (discover.Container, *metrics.Observation)) analyze.Finding {
	var cs []discover.Container
	obs := map[metrics.Key]*metrics.Observation{}
	for _, p := range pairs {
		c, o := p()
		cs = append(cs, c)
		obs[o.Key] = o
	}
	fs := analyze.RunWithCluster(cs, obs, analyze.Cluster{Now: now}, 14*24*time.Hour, analyze.DefaultThresholds())
	return fs[0]
}

// A kubelet that stopped reporting five days ago leaves nine days of history
// in a fortnight's window, and the container was sized from it as if it were
// current. Only the sample count fell, and only by accident.
func TestStaleMetricsDoNotSizeAReduction(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	f := runAt(now, func() (discover.Container, *metrics.Observation) { return wasteful(now.Add(-5*24*time.Hour), "a") })
	if !hasCode(f, analyze.MetricsStale) {
		t.Fatalf("no %s on a container silent for 5 days: %s %v", analyze.MetricsStale, f.Verdict, f.Reasons)
	}
	if f.Verdict != analyze.Unverified {
		t.Errorf("verdict = %s, want %s: a reduction from five-day-old history was proposed", f.Verdict, analyze.Unverified)
	}
	if f.Confidence != analyze.Low {
		t.Errorf("confidence = %s, want low: every live replica is silent", f.Confidence)
	}
}

// One silent replica of two: the other still reports, so confidence stands,
// but the reduction applies to the silent one too and cannot be shown safe.
func TestOneStaleReplicaHoldsTheReduction(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	f := runAt(now,
		func() (discover.Container, *metrics.Observation) { return wasteful(now.Add(-3*time.Minute), "a") },
		func() (discover.Container, *metrics.Observation) { return wasteful(now.Add(-2*24*time.Hour), "b") },
	)
	if !hasCode(f, analyze.MetricsStale) || f.Verdict != analyze.Unverified {
		t.Fatalf("one replica silent for 2 days: want %s and %s, got %s %v", analyze.MetricsStale, analyze.Unverified, f.Verdict, f.Reasons)
	}
	if f.Confidence == analyze.Low {
		t.Error("confidence fell to low with a replica still reporting")
	}
}

// The negative control: a reporting container, and the scrape step's lag.
func TestFreshMetricsAreNotStale(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	for _, lag := range []time.Duration{0, 10 * time.Minute, 59 * time.Minute} {
		f := runAt(now, func() (discover.Container, *metrics.Observation) { return wasteful(now.Add(-lag), "a") })
		if hasCode(f, analyze.MetricsStale) || f.Verdict != analyze.Waste {
			t.Errorf("newest sample %s old: got %s %v, want WASTE and no %s", lag, f.Verdict, f.Reasons, analyze.MetricsStale)
		}
	}
}

// Without a clock (the simulator) or without a known newest sample, nothing
// can be said, and nothing is.
func TestStalenessUnknownIsNotStale(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	old := func() (discover.Container, *metrics.Observation) { return wasteful(now.Add(-5*24*time.Hour), "a") }
	if f := runAt(time.Time{}, old); hasCode(f, analyze.MetricsStale) {
		t.Error("flagged stale with no clock")
	}
	unknown := func() (discover.Container, *metrics.Observation) {
		c, o := wasteful(now, "a")
		o.LastSeen, o.FirstSeen = 0, 0
		return c, o
	}
	if f := runAt(now, unknown); hasCode(f, analyze.MetricsStale) {
		t.Error("flagged stale with no newest sample known")
	}
}

// Starvation measured before the silence was still measured: it stands.
func TestStaleStarvationStands(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	f := runAt(now, func() (discover.Container, *metrics.Observation) {
		c, o := wasteful(now.Add(-5*24*time.Hour), "a")
		o.OOMEvents = 1
		return c, o
	})
	if f.Verdict != analyze.Saturated || !hasCode(f, analyze.MetricsStale) {
		t.Errorf("got %s %v, want SATURATED with %s beside it", f.Verdict, f.Reasons, analyze.MetricsStale)
	}
}

// A replica with no known newest sample still counts as a replica: the
// message said "1 of its 1" beside two pods, and the lone stale one dropped
// confidence to low as if nothing were still reporting.
func TestStaleCountsReplicasWithUnknownAge(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	f := runAt(now,
		func() (discover.Container, *metrics.Observation) {
			c, o := wasteful(now, "a")
			o.LastSeen, o.FirstSeen = 0, 0
			return c, o
		},
		func() (discover.Container, *metrics.Observation) { return wasteful(now.Add(-2*24*time.Hour), "b") },
	)
	var text string
	for _, r := range f.Reasons {
		if r.Code == analyze.MetricsStale {
			text = r.Text
		}
	}
	if text == "" {
		t.Fatalf("no %s: %s %v", analyze.MetricsStale, f.Verdict, f.Reasons)
	}
	if !strings.Contains(text, "1 of its 2 live replicas") {
		t.Errorf("wrong denominator: %q", text)
	}
	if f.Confidence == analyze.Low {
		t.Error("confidence fell to low though one replica's age is unknown, not stale")
	}
}
