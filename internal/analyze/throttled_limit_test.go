package analyze_test

import (
	"testing"
	"time"

	"github.com/2listic/tare/internal/analyze"
	"github.com/2listic/tare/internal/discover"
	"github.com/2listic/tare/internal/metrics"
)

// guaranteedCache reproduces tare-lab's guaranteed-cache as measured on msi on
// 2026-10-11: a 100m limit throttling it in 82% of periods at its worst, while
// its averaged CPU peaked at 25m. Its pressure was low enough relative to its
// allocation that it read as node contention, so it was never SATURATED, and
// the WASTE path sized the limit from the 25m peak: 30m.
func guaranteedCache(qos string, throttlePeak float64) ([]discover.Container, map[metrics.Key]*metrics.Observation) {
	key := metrics.Key{Namespace: "tare-lab", Pod: "guaranteed-cache-7c9d-x2k", Container: "cache"}
	c := discover.Container{
		Namespace: key.Namespace, Pod: key.Pod, Name: key.Container,
		Workload:   "Deployment/guaranteed-cache",
		CPURequest: 100, CPULimit: 100,
		MemRequest: 256 * mib, MemLimit: 256 * mib,
		QoS: qos,
	}
	if qos == "Burstable" {
		c.CPURequest = 50
	}
	o := &metrics.Observation{
		Key:           key,
		CPUUsageP95:   0.023,
		CPUUsageMax:   0.025,
		CPUSteadyMax:  0.025,
		CPUStartupMax: 0.025, // booted inside the window, no burst
		MemWorkingSet: 4 * mib,
		MemP95:        4 * mib,
		PSIWaitingMax: 0.205,
		PSIWaitingAvg: 0.109,
		ThrottleRatio: throttlePeak,
		ThrottleAvg:   throttlePeak / 2,
		Samples:       1024,
	}
	for _, s := range []metrics.Signal{
		metrics.SigCPUUsage, metrics.SigCPUPeak, metrics.SigMemory, metrics.SigMemP95,
		metrics.SigPSI, metrics.SigThrottle, metrics.SigThrottleAvg, metrics.SigOOM, metrics.SigRestarts,
	} {
		o.Mark(s)
	}
	return []discover.Container{c}, map[metrics.Key]*metrics.Observation{key: o}
}

func TestThrottledLimitIsNeverLowered(t *testing.T) {
	for _, qos := range []string{"Guaranteed", "Burstable"} {
		t.Run(qos, func(t *testing.T) {
			cs, obs := guaranteedCache(qos, 0.821)
			fs := analyze.Run(cs, obs, 14*24*time.Hour, analyze.DefaultThresholds())
			if len(fs) != 1 {
				t.Fatalf("expected 1 finding, got %d", len(fs))
			}
			f := fs[0]
			t.Logf("verdict=%s recCPU=%d recCPULimit=%d", f.Verdict, f.RecCPU, f.RecCPULimit)
			if f.Verdict == analyze.Saturated {
				t.Fatalf("fixture drifted: the case is the non-starved path, got %s", f.Verdict)
			}
			if f.RecCPULimit < 100 {
				t.Errorf("CPU limit lowered from 100m to %dm while it throttled 82%% of periods", f.RecCPULimit)
			}
			if !hasCode(f, analyze.CPULimitThrottled) {
				t.Errorf("limit held without saying why: no %s reason", analyze.CPULimitThrottled)
			}
			if qos == "Burstable" && f.RecCPU >= 50 {
				t.Errorf("request should still be sized from usage, got %dm", f.RecCPU)
			}
		})
	}
}

// The control: below the throttle threshold the limit is still held, but by
// the general rule, so the throttle reason is not claimed without throttling.
func TestUnthrottledLimitIsNotCalledThrottled(t *testing.T) {
	cs, obs := guaranteedCache("Burstable", 0.01)
	f := analyze.Run(cs, obs, 14*24*time.Hour, analyze.DefaultThresholds())[0]
	if hasCode(f, analyze.CPULimitThrottled) || !hasCode(f, analyze.CPULimitHeld) {
		t.Errorf("want %s and not %s below the throttle threshold: %v", analyze.CPULimitHeld, analyze.CPULimitThrottled, f.Reasons)
	}
}

func hasCode(f analyze.Finding, c analyze.Code) bool {
	for _, r := range f.Reasons {
		if r.Code == c {
			return true
		}
	}
	return false
}

// guaranteed-cache on msi carried NODE_CONTENTION ("contention on the node
// rather than a shortage of its own") and LIMIT_NOT_NODE ("the constraint is
// this container's own limit, not the machine") on the same row. The pressure
// is the quota's: only an unthrottled container's stalls can be the node's.
func TestThrottledPressureIsNotBlamedOnTheNode(t *testing.T) {
	for _, tc := range []struct {
		throttle   float64
		contention bool
	}{{0.821, false}, {0.01, true}} {
		cs, obs := guaranteedCache("Burstable", tc.throttle)
		f := analyze.Run(cs, obs, 14*24*time.Hour, analyze.DefaultThresholds())[0]
		if got := hasCode(f, analyze.NodeContention); got != tc.contention {
			t.Errorf("throttled %.0f%% of periods: %s = %v, want %v", tc.throttle*100, analyze.NodeContention, got, tc.contention)
		}
	}
}

// boutique/frontend on msi, 3.5 minutes old: 14 samples, p95 0m while drawing
// 50m, and WASTE 100m→10m that -o patch and diff --write both acted on.
func TestTooFewSamplesProposesNoReduction(t *testing.T) {
	for _, tc := range []struct {
		samples int
		want    analyze.Verdict
	}{{14, analyze.Unverified}, {1024, analyze.Waste}} {
		cs, obs := guaranteedCache("Burstable", 0.01)
		for _, o := range obs {
			o.Samples = tc.samples
		}
		f := analyze.Run(cs, obs, 14*24*time.Hour, analyze.DefaultThresholds())[0]
		if f.Verdict != tc.want || hasCode(f, analyze.TooFewSamples) != (tc.samples == 14) {
			t.Errorf("%d samples: verdict %s, %s %v; want %s", tc.samples, f.Verdict,
				analyze.TooFewSamples, hasCode(f, analyze.TooFewSamples), tc.want)
		}
		if ok, _ := analyze.Patchable(f); ok != (tc.want == analyze.Waste) {
			t.Errorf("%d samples: patchable %v", tc.samples, ok)
		}
	}
}

// Online Boutique on msi, twice. Limits sized from steady state with no boot
// in the window crash-looped cartservice; limits sized from a measured boot
// raised p95 latency 4x with no error and no restart. Averaged usage cannot
// show a lower limit is enough, whether or not a boot was seen, so a limit is
// held, none is added, and only a measured need moves one up.
func TestCPULimitIsNeverLowered(t *testing.T) {
	for _, tc := range []struct {
		name           string
		limit, startup int64
		wantLimit      int64
		wantCode       bool
	}{
		{"limit, boot unseen", 100, 0, 100, true},
		{"limit, boot seen", 100, 25, 100, true},
		{"no limit", 0, 25, 0, false},
		{"boot needs more than the limit", 100, 300, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cs, obs := guaranteedCache("Burstable", 0.01)
			cs[0].CPULimit = tc.limit
			if tc.limit == 0 {
				// Against 50m with no limit its 25m peak reads as starved.
				cs[0].CPURequest = 100
			}
			for _, o := range obs {
				o.CPUStartupMax = float64(tc.startup) / 1000
				if tc.startup > 0 {
					o.Mark(metrics.SigStartup)
				}
			}
			f := analyze.Run(cs, obs, 14*24*time.Hour, analyze.DefaultThresholds())[0]
			if tc.wantLimit > 0 || tc.limit == 0 {
				if f.RecCPULimit != tc.wantLimit {
					t.Errorf("limit %dm became %dm, want %dm", tc.limit, f.RecCPULimit, tc.wantLimit)
				}
			} else if f.RecCPULimit <= tc.limit {
				t.Errorf("a boot drawing %dm should raise the %dm limit, got %dm", tc.startup, tc.limit, f.RecCPULimit)
			}
			if hasCode(f, analyze.CPULimitHeld) != tc.wantCode {
				t.Errorf("%s present = %v, want %v", analyze.CPULimitHeld, !tc.wantCode, tc.wantCode)
			}
			if f.RecCPU >= 50 {
				t.Errorf("the request must still be sized from usage, got %dm", f.RecCPU)
			}
		})
	}
}
