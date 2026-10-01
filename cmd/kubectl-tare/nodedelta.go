package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/mfranzon/tare/internal/analyze"
	"github.com/mfranzon/tare/internal/discover"
	"github.com/mfranzon/tare/internal/packing"
)

// sizerFor turns per-container recommendations into per-pod requests.
//
// A pod reserves the sum of its containers, so a workload is only usable here
// if every one of its containers has a recommendation. Summing a partial set
// would hand the packer a pod smaller than anything that could exist, and the
// node count that fell out of it would be fiction. A workload with any
// container the analyzer declined to size keeps its current requests, and so
// does an IDLE one: the totals leave it out because idleness is not ruled out,
// and a node count built on it would count the same unconfirmed saving twice.
func sizerFor(fs []analyze.Finding) packing.Sizer {
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

// nodeLines is the node block as text, so the table and the HTML report cannot
// drift apart on the one figure in this tool that corresponds to a bill.
func nodeLines(r packing.Result) []string {
	var out []string
	pf := func(format string, args ...any) { out = append(out, fmt.Sprintf(format, args...)) }

	if !r.Trustworthy {
		pf("node-count delta: not computed. %s", r.SelfCheck)
		return out
	}

	pf("nodes: %d running, %d needed for today's requests, %d needed after these recommendations",
		r.NodesRunning, r.NodesPackedNow, r.NodesPackedRecommended)

	// The two savings are reported apart because only one of them is tare's.
	// Folding the cluster's existing slack into the headline would be taking
	// credit for nodes a consolidation pass frees without changing a request.
	if c := r.FromConsolidation(); c > 0 {
		pf("  %d node(s) are already free without resizing anything: the cluster is not tightly packed. That saving is not tare's.", c)
	}
	switch d := r.FromResizing(); {
	case d > 0:
		pf("  %d node(s) come from these recommendations.", d)
	case d < 0:
		pf("  these recommendations need %d MORE node(s): the starved containers in this report want more than the reclaimed containers give back", -d)
	default:
		pf("  0 nodes come from these recommendations. The requests get smaller and the bill does not: a request is a reservation, and the invoice moves when a node goes away.")
	}
	// Which nodes are empty, per pass, and not which pass emptied which node:
	// the passes are packed independently, so the sets do not subtract. Listing
	// every empty node under "come from these recommendations" once named the
	// cluster's own slack as tare's saving.
	if len(r.EmptyToday) > 0 {
		pf("  empty after repacking today's requests: %s", strings.Join(r.EmptyToday, ", "))
	}
	if len(r.Removable) > 0 {
		pf("  empty after applying these recommendations: %s", strings.Join(r.Removable, ", "))
	}

	if r.Pinned > 0 {
		pf("  %d of %d pods could not be modelled as movable and held their node, so this is a LOWER bound:",
			r.Pinned, r.Pinned+r.Movable)
		reasons := make([]string, 0, len(r.PinReasons))
		for reason := range r.PinReasons {
			reasons = append(reasons, reason)
		}
		sort.Slice(reasons, func(i, j int) bool {
			if r.PinReasons[reasons[i]] != r.PinReasons[reasons[j]] {
				return r.PinReasons[reasons[i]] > r.PinReasons[reasons[j]]
			}
			return reasons[i] < reasons[j]
		})
		for _, reason := range reasons {
			pf("    %4d  %s", r.PinReasons[reason], reason)
		}
	}
	if r.ExcludedNode > 0 {
		pf("  %d cordoned node(s) were excluded as destinations", r.ExcludedNode)
	}
	// A node that cannot receive anything changes what every figure above
	// means, so it is named rather than counted.
	if len(r.Unusable) > 0 {
		pf("  %d node(s) could not be used as destinations at all:", len(r.Unusable))
		for _, u := range r.Unusable {
			pf("    - %s", u)
		}
	}
	pf("  tare reports nodes, not currency: only you know what your nodes cost.")
	return out
}
