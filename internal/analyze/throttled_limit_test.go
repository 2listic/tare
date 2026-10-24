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

// The control: the same container without throttling has its limit sized from
// usage as before, so the hold is not simply freezing every limit.
func TestUnthrottledLimitIsStillSized(t *testing.T) {
	cs, obs := guaranteedCache("Burstable", 0.01)
	f := analyze.Run(cs, obs, 14*24*time.Hour, analyze.DefaultThresholds())[0]
	if f.RecCPULimit >= 100 {
		t.Errorf("unthrottled limit held at %dm; the hold must need throttling", f.RecCPULimit)
	}
	if hasCode(f, analyze.CPULimitThrottled) {
		t.Errorf("%s fired below the throttle threshold", analyze.CPULimitThrottled)
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
