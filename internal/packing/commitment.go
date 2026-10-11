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
	return ratio(c.LimCPU, c.AllocCPU)
}

// MemRatio is the same for memory, where the consequence is an OOMKill rather
// than a slowdown, so the tolerable value is much closer to 1.
func (c Commitment) MemRatio() float64 {
	return ratio(c.LimMem, c.AllocMem)
}

// ReqCPUFrac is how much of the node the scheduler considers spoken for.
func (c Commitment) ReqCPUFrac() float64 {
	return ratio(c.ReqCPU, c.AllocCPU)
}

func (c Commitment) ReqMemFrac() float64 {
	return ratio(c.ReqMem, c.AllocMem)
}

// Commitments summarises every node that hosts at least one pod.
func Commitments(inv discover.Inventory, pods []discover.Pod) []Commitment {
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
		// Counted per container. Testing the pod's summed limit counted only
		// pods whose every container was unlimited, so an app container with
		// a limit hid the unlimited sidecar beside it.
		c.UnlimitedCPU += p.UnlimitedCPU
		c.UnlimitedMem += p.UnlimitedMem
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

// ratio is a/b, or 0 for a node with nothing allocatable.
func ratio(a, b int64) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b)
}
