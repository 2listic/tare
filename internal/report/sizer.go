package report

import (
	"github.com/mfranzon/tare/internal/analyze"
	"github.com/mfranzon/tare/internal/discover"
	"github.com/mfranzon/tare/internal/packing"
)

// Sizer turns per-container recommendations into per-pod requests.
//
// A pod reserves the sum of its containers, so a workload is only usable here
// if every one of its containers has a recommendation. Summing a partial set
// would hand the packer a pod smaller than anything that could exist, and the
// node count that fell out of it would be fiction. A workload with any
// container the analyzer declined to size keeps its current requests, and so
// does an IDLE one: the totals leave it out because idleness is not ruled out,
// and a node count built on it would count the same unconfirmed saving twice.
func Sizer(fs []analyze.Finding) packing.Sizer {
	type agg struct {
		cpu, mem int64
		usable   bool
	}
	byWorkload := map[string]*agg{}
	for _, f := range fs {
		key := f.Namespace + "/" + f.Workload
		a, ok := byWorkload[key]
		if !ok {
			a = &agg{usable: true}
			byWorkload[key] = a
		}
		if f.RecCPU <= 0 || f.RecMem <= 0 || f.Verdict == analyze.Idle {
			a.usable = false
			continue
		}
		a.cpu += f.RecCPU
		a.mem += f.RecMem
	}
	return func(p discover.Pod) (int64, int64, bool) {
		a, ok := byWorkload[p.Namespace+"/"+p.Workload]
		if !ok || !a.usable {
			return 0, 0, false
		}
		return a.cpu, a.mem, true
	}
}
