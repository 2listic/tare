package report

import (
	"slices"
	"testing"
	"time"

	"github.com/2listic/tare/internal/analyze"
)

// The demo is what a reader sees before installing anything, and pressure is
// the first thing the README claims. A demo where every row is a shrink shows
// none of it, which is how the demo stood until starved-api was added.
func TestDemoShowsCPUPressure(t *testing.T) {
	r := Demo(14*24*time.Hour, "2w")
	for _, f := range r.Findings {
		if f.Container != "starved-api" {
			continue
		}
		if f.Verdict != analyze.Saturated {
			t.Fatalf("starved-api verdict = %s, want %s: %v", f.Verdict, analyze.Saturated, f.Reasons)
		}
		if !slices.ContainsFunc(f.Reasons, func(r analyze.Reason) bool { return r.Code == analyze.CPUPressure }) {
			t.Fatalf("starved-api has no %s reason: %v", analyze.CPUPressure, f.Reasons)
		}
		// Its usage is pinned at the limit, so tare must not pretend to know
		// how much more it needs.
		if !f.CPUCensored {
			t.Errorf("starved-api CPU %dm -> %dm is not marked censored", f.CPURequest, f.RecCPU)
		}
		return
	}
	t.Fatal("starved-api is missing from the demo")
}
