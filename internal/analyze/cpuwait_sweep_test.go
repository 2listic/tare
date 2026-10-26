package analyze_test

import (
	"testing"

	"github.com/mfranzon/tare/internal/analyze"
	"github.com/mfranzon/tare/internal/metrics"
	"github.com/mfranzon/tare/internal/simulate"
)

// verdictWithout runs one archetype under one scenario, with the given signals
// removed, and returns the verdict.
func verdictWithout(d simulate.Demand, sc scenario, seed int64, drop ...metrics.Signal) analyze.Verdict {
	c := simulate.NewCluster("sim", origin, seed, d)
	c.SetAllocation(d.Name, sc.alloc(simulate.PeakCPU(d, origin, sweepWindow, seed)))
	c.Run(sweepWindow)
	obs, _ := c.Observe(nil, sweepWindow)
	return analyze.Run(c.Containers(), simulate.Without(obs, drop...), sweepWindow, analyze.DefaultThresholds())[0].Verdict
}

// TestCPUWaitMatchesPSIOnCatalog runs the catalog the threshold sweep uses on
// a cluster without PSI, where CPU wait decides, and holds it to the verdicts
// a correct analyzer gives.
//
// What it does not exercise: the simulator has one task per container and no
// neighbours, so no container waits when it is comfortable and none has a
// quiet normal of the kind real containers have. The relative test is
// therefore mostly idle here and the starved scenarios are caught by the
// ceiling or by a normal made of near-quota queueing. The rule's calibration
// is the harness's (metrics/cpuwait.go), not this catalog's.
func TestCPUWaitMatchesPSIOnCatalog(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the catalog; run without -short")
	}
	var total, correct, agree int
	for i, d := range simulate.Catalog() {
		for _, sc := range scenarios {
			seed := int64(500 + i)
			withPSI := verdictWithout(d, sc, seed)
			withWait := verdictWithout(d, sc, seed, metrics.SigPSI, metrics.SigPSIAvg)
			total++
			if withWait == sc.want(d) {
				correct++
			} else {
				t.Logf("CPU wait: %s/%s = %s, want %s (PSI says %s)", d.Name, sc.name, withWait, sc.want(d), withPSI)
			}
			if withWait == withPSI {
				agree++
			}
		}
	}
	t.Logf("CPU wait without PSI: %d/%d correct, same verdict as PSI on %d/%d", correct, total, agree, total)
	if correct < total {
		t.Errorf("CPU wait misjudged %d of %d scenarios the catalog decides", total-correct, total)
	}
}
