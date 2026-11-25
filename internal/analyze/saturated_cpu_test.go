package analyze_test

import (
	"testing"

	"github.com/2listic/tare/internal/analyze"
	"github.com/2listic/tare/internal/metrics"
)

// TestCPUStarvedAtLimitIsCensored: CFS never lets usage exceed the quota, so a
// starved container's usage at its limit is the limit echoed back. starved-api
// in the demo wants ~350m against a 200m limit and was told 229m request /
// 250m limit, still starving, printed as the fix. The memory side had the
// same shape and the same answer: say it is unknown.
func TestCPUStarvedAtLimitIsCensored(t *testing.T) {
	cases := []struct {
		name     string
		qos      string
		p95, max float64 // cores
	}{
		{"pinned", "Burstable", 0.200, 0.200},
		{"pinned guaranteed", "Guaranteed", 0.200, 0.200},
		// The limit is sized from the peak, so a peak at the limit censors it
		// even when p95 sits well below.
		{"spikes reach the limit", "Burstable", 0.060, 0.199},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := int64(100)
			if tc.qos == "Guaranteed" {
				req = 200
			}
			f := runOne(t, container(tc.qos, req, 200, 256*mib, 256*mib), metrics.Observation{
				CPUUsageP95: tc.p95, CPUUsageMax: tc.max,
				PSIWaitingMax: 0.6, PSIWaitingAvg: 0.3,
				MemWorkingSet: 150 * mib, MemP95: 140 * mib,
			}, analyze.Cluster{})
			if f.Verdict != analyze.Saturated {
				t.Fatalf("precondition: want SATURATED, got %s %v", f.Verdict, f.Reasons)
			}
			if !f.CPUCensored {
				t.Error("CPUCensored is false for a container starved with usage at its limit")
			}
			if f.RecCPU != req || f.RecCPULimit != 200 {
				t.Errorf("CPU rec = %dm request / %dm limit, want %dm / 200m held: any other number is invented",
					f.RecCPU, f.RecCPULimit, req)
			}
			if !hasCode(f, analyze.CPUDemandCensored) {
				t.Errorf("no %s reason; the reader has no way to know the CPU figure is not an answer", analyze.CPUDemandCensored)
			}
		})
	}
}

// Without a limit, usage is not capped, so a starved container's usage is a
// measurement and the raise derived from it stands.
func TestCPUStarvedWithoutLimitIsNotCensored(t *testing.T) {
	f := runOne(t, container("Burstable", 100, 0, 256*mib, 256*mib), metrics.Observation{
		CPUUsageP95: 0.300, CPUUsageMax: 0.350,
		PSIWaitingMax: 0.6, PSIWaitingAvg: 0.3,
		MemWorkingSet: 150 * mib, MemP95: 140 * mib,
	}, analyze.Cluster{})
	if f.Verdict != analyze.Saturated {
		t.Fatalf("precondition: want SATURATED, got %s %v", f.Verdict, f.Reasons)
	}
	if f.CPUCensored || hasCode(f, analyze.CPUDemandCensored) {
		t.Error("a container with no CPU limit was marked censored: nothing capped its usage")
	}
	if f.RecCPU <= 100 {
		t.Errorf("RecCPU = %dm, want the measured raise above 100m", f.RecCPU)
	}
}
