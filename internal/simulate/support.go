package simulate

import (
	"math"
	"time"

	"github.com/mfranzon/tare/internal/metrics"
)

// Helpers the tests of other packages use to set up a simulation. They are
// exported because those tests live outside this package, not because the CLI
// needs them.

// Without removes signals from every observation, so a test can ask what the
// analyzer does on a cluster that does not export them.
func Without(obs map[metrics.Key]*metrics.Observation, drop ...metrics.Signal) map[metrics.Key]*metrics.Observation {
	for _, o := range obs {
		for _, s := range drop {
			delete(o.Have, s)
		}
	}
	return obs
}

// PeakCPU is the ground truth: the smallest CPU allocation that never starves
// this workload, computed by replaying demand with no allocation limit.
// Tests assert recommendations against this rather than against a hard-coded
// number, so changing the demand model cannot silently invalidate them.
func PeakCPU(d Demand, from time.Time, dur time.Duration, seed int64) float64 {
	e := New(d, Allocation{}, from, seed)
	e.Run(dur)
	var peak float64
	for _, s := range e.samples {
		peak = math.Max(peak, s.CPUDemand)
	}
	return peak
}

// PeakMem is the ground truth for memory: the largest demand observed with no
// allocation limit, which is the smallest limit that avoids an OOMKill.
func PeakMem(d Demand, from time.Time, dur time.Duration, seed int64) int64 {
	e := New(d, Allocation{}, from, seed)
	e.Run(dur)
	var peak int64
	for _, s := range e.samples {
		if s.MemDemand > peak {
			peak = s.MemDemand
		}
	}
	return peak
}
