package packing

import (
	"testing"

	"github.com/mfranzon/tare/internal/discover"
)

func limited(name, on string, req, lim int64, memReq, memLim int64) discover.Pod {
	p := pod(name, on, req, memReq)
	p.CPULimit, p.MemLimit = lim, memLim
	return p
}

// The case the whole view exists for: a node the scheduler considers nearly
// empty, which will stall the moment its pods get busy together.
func TestUnderRequestedAndOverLimitedIsVisible(t *testing.T) {
	inv := discover.Inventory{Nodes: []discover.Node{node("n1", 8)}}
	var pods []discover.Pod
	for _, n := range []string{"a", "b", "c", "d"} {
		// 500m requested, 8 cores allowed, each.
		pods = append(pods, limited(n, "n1", 500, 8000, gib, 4*gib))
	}
	cs := Commitments(inv, pods, UnlimitedCounts(pods))
	if len(cs) != 1 {
		t.Fatalf("expected one node, got %d", len(cs))
	}
	c := cs[0]
	if got := c.ReqCPUFrac(); got > 0.30 {
		t.Errorf("requested fraction = %.2f, want about 0.25: the scheduler sees this node as mostly free", got)
	}
	if got := c.CPURatio(); got < 4 {
		t.Errorf("limit ratio = %.1f, want 4: four pods may each take the whole node", got)
	}
}

func TestMemoryOvercommitIsSeparateFromCPU(t *testing.T) {
	// Memory overcommit kills; CPU overcommit throttles. They must not be
	// collapsed into one ratio.
	inv := discover.Inventory{Nodes: []discover.Node{node("n1", 8)}} // 32Gi allocatable
	pods := []discover.Pod{
		limited("a", "n1", 100, 200, gib, 20*gib),
		limited("b", "n1", 100, 200, gib, 20*gib),
	}
	c := Commitments(inv, pods, nil)[0]
	if c.MemRatio() <= 1 {
		t.Errorf("MemRatio = %.2f, want above 1: 40Gi of limits on a 32Gi node", c.MemRatio())
	}
	if c.CPURatio() > 1 {
		t.Errorf("CPURatio = %.2f, want below 1: CPU is not overcommitted here", c.CPURatio())
	}
}

// A container with no limit is unbounded, so the limit total understates the
// node. Presenting the ratio as complete would be a confident wrong answer.
func TestUnlimitedContainersAreCounted(t *testing.T) {
	inv := discover.Inventory{Nodes: []discover.Node{node("n1", 8)}}
	pods := []discover.Pod{
		limited("capped", "n1", 100, 500, gib, gib),
		pod("uncapped", "n1", 100, gib), // no limits at all
	}
	c := Commitments(inv, pods, UnlimitedCounts(pods))[0]
	if c.UnlimitedCPU != 1 || c.UnlimitedMem != 1 {
		t.Errorf("UnlimitedCPU=%d UnlimitedMem=%d, want 1 and 1", c.UnlimitedCPU, c.UnlimitedMem)
	}
}

func TestEmptyNodesAreOmitted(t *testing.T) {
	inv := discover.Inventory{Nodes: []discover.Node{node("n1", 8), node("empty", 8)}}
	cs := Commitments(inv, []discover.Pod{pod("a", "n1", 100, gib)}, nil)
	for _, c := range cs {
		if c.Node == "empty" {
			t.Error("a node hosting nothing has committed nothing and is noise in this table")
		}
	}
}

func TestMostOvercommittedFirst(t *testing.T) {
	inv := discover.Inventory{Nodes: []discover.Node{node("calm", 8), node("hot", 8)}}
	pods := []discover.Pod{
		limited("a", "calm", 100, 500, gib, gib),
		limited("b", "hot", 100, 32000, gib, gib),
	}
	cs := Commitments(inv, pods, nil)
	if cs[0].Node != "hot" {
		t.Errorf("first node = %q, want the most overcommitted one", cs[0].Node)
	}
}

func TestNoInventoryProducesNoTable(t *testing.T) {
	if cs := Commitments(discover.Inventory{Partial: true}, []discover.Pod{pod("a", "n1", 1, 1)}, nil); cs != nil {
		t.Errorf("a partial inventory must produce no commitment table, got %d rows", len(cs))
	}
}
