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

// TestCPUWaitOnCatalog runs the catalog the threshold sweep uses on a cluster
// without PSI, where CPU wait decides. It holds CPU wait to one property, that
// it never calls a container starved that is not, and records the rest.
//
// The rest is a known limit of the simulator, not a score of the rule. The
// simulator has one task per container, no neighbours and no CFS throttling,
// and it starves a container by a constant cap on every tick. A starvation
// that is the same all window long is the container's normal, so the relative
// rule cannot see it: mildly-starved scenarios come back WASTE, and the
// ceiling or CPU_WAIT_NEVER_QUIET turns the harder ones into SATURATED or
// UNVERIFIED. On a real node the same container is limited, and throttling
// catches it. The rule's calibration is the harness's (metrics/cpuwait.go);
// at the time of writing this catalog scores 28/54 with CPU wait alone.
func TestCPUWaitOnCatalog(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the catalog; run without -short")
	}
	var total, correct, agree int
	for i, d := range simulate.Catalog() {
		for _, sc := range scenarios {
			seed := int64(500 + i)
			withPSI := verdictWithout(d, sc, seed)
			withWait := verdictWithout(d, sc, seed, metrics.SigPSI, metrics.SigPSIAvg)
			want := sc.want(d)
			total++
			if withWait == want {
				correct++
			} else {
				t.Logf("CPU wait: %s/%s = %s, want %s (PSI says %s)", d.Name, sc.name, withWait, want, withPSI)
			}
			if withWait == withPSI {
				agree++
			}
			if withWait == analyze.Saturated && want != analyze.Saturated {
				t.Errorf("CPU wait invented starvation: %s/%s = SATURATED, want %s", d.Name, sc.name, want)
			}
		}
	}
	t.Logf("CPU wait without PSI: %d/%d correct, same verdict as PSI on %d/%d", correct, total, agree, total)
}
