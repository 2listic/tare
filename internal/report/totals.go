package report

import (
	"sort"

	"github.com/mfranzon/tare/internal/analyze"
)

// Savings is what a reader actually wants from a cluster-wide run: not three
// hundred rows, but one number and the handful of workloads that make it up.
type Savings struct {
	CPUMillicores int64
	MemBytes      int64
	Workloads     int

	// A container that needs MORE than it has is not a rounding error in the
	// total, it is the finding that matters most. Summing only the positive
	// side produced a headline number that was optimistic by exactly the amount
	// a reader most needed to see.
	AddCPUMillicores int64
	AddMemBytes      int64
	AddWorkloads     int

	// IDLE containers are counted apart and never summed into the reduction.
	// Their own evidence says IDLE_NOT_RULED_OUT, so folding their requests
	// into the headline put a number there the report told the reader not to
	// act on. The larger saving there is deletion, not resizing.
	IdleCPUMillicores int64
	IdleMemBytes      int64
	IdleWorkloads     int

	// MemUnknown counts containers whose memory tare does not know
	// (MemCensored). They contribute nothing to either direction, and a total
	// that silently left them out would read as "nothing needs more memory".
	MemUnknown int
}

func Totals(fs []analyze.Finding) Savings {
	var s Savings
	for _, f := range fs {
		c, m := Reclaimable(f)
		if f.MemCensored {
			s.MemUnknown++
		}
		if f.Verdict == analyze.Idle {
			s.IdleWorkloads++
			s.IdleCPUMillicores += max(c, 0)
			s.IdleMemBytes += max(m, 0)
			continue
		}
		if c > 0 || m > 0 {
			s.Workloads++
		}
		if c < 0 || m < 0 {
			s.AddWorkloads++
		}
		switch {
		case c > 0:
			s.CPUMillicores += c
		case c < 0:
			s.AddCPUMillicores -= c
		}
		switch {
		case m > 0:
			s.MemBytes += m
		case m < 0:
			s.AddMemBytes -= m
		}
	}
	return s
}

// Reclaimable is per-workload, multiplied by replica count: a 100m saving on a
// twenty-replica deployment is two cores, and reporting it as 100m
// understates it by the factor that matters most.
func Reclaimable(f analyze.Finding) (cpu, mem int64) {
	pods := int64(f.Pods)
	if pods < 1 {
		pods = 1
	}
	if f.RecCPU > 0 && f.CPURequest > 0 {
		cpu = (f.CPURequest - f.RecCPU) * pods
	}
	if f.RecMem > 0 && f.MemRequest > 0 {
		mem = (f.MemRequest - f.RecMem) * pods
	}
	return cpu, mem
}

// SortBySavings puts the biggest wins first, across every verdict. On a
// cluster with hundreds of containers, severity ordering alone still leaves an
// unreadable wall of rows.
func SortBySavings(fs []analyze.Finding) {
	sort.SliceStable(fs, func(i, j int) bool {
		return SavingsWeight(fs[i]) > SavingsWeight(fs[j])
	})
}

// SavingsWeight ranks a finding by what it reclaims, weighing a core against a
// gibibyte so one ranking covers both.
func SavingsWeight(f analyze.Finding) float64 {
	c, m := Reclaimable(f)
	return float64(c)/1000 + float64(m)/float64(1<<30)
}

// VerdictCount is one verdict and how many findings carry it.
type VerdictCount struct {
	Verdict analyze.Verdict
	N       int
}

func Tally(fs []analyze.Finding) map[analyze.Verdict]int {
	n := map[analyze.Verdict]int{}
	for _, f := range fs {
		n[f.Verdict]++
	}
	return n
}

// CountVerdicts is every verdict present, in report order.
func CountVerdicts(fs []analyze.Finding) []VerdictCount {
	n := Tally(fs)
	var out []VerdictCount
	for _, v := range analyze.ReportOrder() {
		if n[v] > 0 {
			out = append(out, VerdictCount{v, n[v]})
		}
	}
	return out
}
