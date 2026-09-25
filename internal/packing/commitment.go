package packing

import (
	"sort"

	"github.com/mfranzon/tare/internal/discover"
)

// Commitment is what a node has promised, against what it has.
//
// Two different numbers, and the gap between them is the thing a cluster
// owner cannot see from any standard tool. Requests are what the scheduler
// reserved and what it uses to decide what else fits. Limits are what the pods
// may actually consume, and their sum routinely exceeds allocatable several
// times over, which is legal and usually deliberate.
//
// The risk lives in the combination. A node at 25% requested looks empty, so
// the scheduler keeps filling it; at 400% limited, everything on it stalls the
// moment enough of those pods get busy together. Neither number alone says
// that, and a right-sizing report that only shrinks requests makes it worse by
// inviting more pods onto the same node.
type Commitment struct {
	Node        string
	Pods        int
	AllocCPU    int64
	AllocMem    int64
	ReqCPU      int64
	ReqMem      int64
	LimCPU      int64
	LimMem      int64
	Schedulable bool

	// UnlimitedCPU and UnlimitedMem count containers with no limit at all.
	// Their consumption is unbounded, so the limit totals above understate
	// what the node could be asked for, and the ratio must not be presented
	// as complete when these are non-zero.
	UnlimitedCPU int
	UnlimitedMem int
}

// CPURatio is committed limits as a multiple of allocatable. Above 1 the node
// is overcommitted on CPU, which throttles rather than kills.
func (c Commitment) CPURatio() float64 {
	if c.AllocCPU == 0 {
		return 0
	}
	return float64(c.LimCPU) / float64(c.AllocCPU)
}

// MemRatio is the same for memory, where the consequence is an OOMKill rather
// than a slowdown, so the tolerable value is much closer to 1.
func (c Commitment) MemRatio() float64 {
	if c.AllocMem == 0 {
		return 0
	}
	return float64(c.LimMem) / float64(c.AllocMem)
}

// ReqCPUFrac is how much of the node the scheduler considers spoken for.
func (c Commitment) ReqCPUFrac() float64 {
	if c.AllocCPU == 0 {
		return 0
	}
	return float64(c.ReqCPU) / float64(c.AllocCPU)
}

func (c Commitment) ReqMemFrac() float64 {
	if c.AllocMem == 0 {
		return 0
	}
	return float64(c.ReqMem) / float64(c.AllocMem)
}

// Commitments summarises every node that hosts at least one pod.
func Commitments(inv discover.Inventory, pods []discover.Pod, unlimited map[string][2]int) []Commitment {
	if inv.Partial || len(inv.Nodes) == 0 {
		return nil
	}
	byNode := map[string]*Commitment{}
	for _, n := range inv.Nodes {
		byNode[n.Name] = &Commitment{
			Node: n.Name, AllocCPU: n.AllocCPU, AllocMem: n.AllocMem,
			Schedulable: n.Schedulable,
		}
	}
	for _, p := range pods {
		c, ok := byNode[p.Node]
		if !ok {
			continue
		}
		c.Pods++
		c.ReqCPU += p.CPU
		c.ReqMem += p.Mem
		c.LimCPU += p.CPULimit
		c.LimMem += p.MemLimit
	}
	for node, counts := range unlimited {
		if c, ok := byNode[node]; ok {
			c.UnlimitedCPU, c.UnlimitedMem = counts[0], counts[1]
		}
	}

	var out []Commitment
	for _, c := range byNode {
		if c.Pods > 0 {
			out = append(out, *c)
		}
	}
	// Most overcommitted first: that is the node a reader should look at.
	sort.Slice(out, func(i, j int) bool {
		if out[i].CPURatio() != out[j].CPURatio() {
			return out[i].CPURatio() > out[j].CPURatio()
		}
		return out[i].Node < out[j].Node
	})
	return out
}

// UnlimitedCounts counts, per node, the containers with no CPU and no memory
// limit. It is computed from the pod list rather than folded into Commitments
// so that the caller can pass whatever container view it already has.
func UnlimitedCounts(pods []discover.Pod) map[string][2]int {
	out := map[string][2]int{}
	for _, p := range pods {
		c := out[p.Node]
		if p.CPULimit == 0 {
			c[0]++
		}
		if p.MemLimit == 0 {
			c[1]++
		}
		out[p.Node] = c
	}
	return out
}
