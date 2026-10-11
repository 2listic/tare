// Package packing answers the only question that turns a right-sizing report
// into money: if these recommendations were applied, how many nodes would the
// cluster stop needing?
//
// Reclaimed millicores are not a saving. A request is a scheduling
// reservation, and lowering one frees allocatable capacity without changing
// any invoice: the bill moves when a node goes away. On a fixed node pool the
// answer is often zero, and saying so is the honest output.
//
// The replay is deliberately pessimistic. Anything it cannot model holds its
// current node, so the figure it produces is a lower bound on what a real
// consolidation would achieve. The alternative - assuming a pod can move
// because no reason not to was found - produces a larger number that the
// scheduler then refuses to honour, which is worse than no number at all.
package packing

import (
	"fmt"
	"sort"

	"github.com/mfranzon/tare/internal/discover"
)

// Result is the outcome of a replay.
//
// Three node counts, because conflating them is how a right-sizer takes credit
// for a saving it did not produce. A typical cluster is not tightly packed, so
// some nodes could be freed today by consolidation alone with no resizing at
// all. That saving is real and it is not tare's; only the difference between
// packing today's requests and packing the recommendations is.
type Result struct {
	// NodesRunning is how many nodes currently host at least one pod.
	NodesRunning int
	// NodesPackedNow is what today's requests would fit onto. The gap to
	// NodesRunning is available without changing a single request.
	NodesPackedNow int
	// NodesPackedRecommended is what the recommendations would fit onto.
	NodesPackedRecommended int

	// Removable names every node left empty after packing the
	// recommendations, INCLUDING the ones repacking today's requests already
	// empties. It is not the list of nodes tare freed and must never be
	// printed beside FromResizing as if it were: that credited the cluster's
	// own slack to the recommendations. EmptyToday is the other half.
	// Named rather than counted, because "3 nodes" invites the reader to
	// check, and the check is the point.
	Removable []string
	// EmptyToday names the nodes left empty by repacking today's requests,
	// before any recommendation is applied.
	EmptyToday []string

	// Pinned counts pods held on their current node, with the reasons and how
	// many pods each accounted for. A replay that pinned most of the cluster
	// has not measured much, and the reader has to be able to see that.
	Pinned     int
	PinReasons map[string]int
	Movable    int
	// ExcludedNode counts cordoned nodes, which host pods but can receive
	// none.
	ExcludedNode int

	// Unusable names the nodes that can receive nothing although they are not
	// cordoned - a kubelet that has stopped reporting, or a taint no movable
	// pod tolerates - with the reason. They still host whatever they host;
	// they are simply not destinations, and a reader who is told the cluster
	// packs onto N nodes has to know which of them are alive.
	Unusable []string

	// Trustworthy is false when the self-check failed. Consumers must report
	// the failure instead of the numbers.
	Trustworthy bool
	SelfCheck   string
}

// FromResizing is the node saving the recommendations are responsible for.
func (r Result) FromResizing() int { return r.NodesPackedNow - r.NodesPackedRecommended }

// FromConsolidation is the node saving already available without tare, by
// packing the cluster as it stands. Reporting it separately is the difference
// between an honest tool and one that claims the cluster's existing slack.
func (r Result) FromConsolidation() int { return r.NodesRunning - r.NodesPackedNow }

// Sizer supplies the recommended requests for a pod's workload, and reports
// false when there is no recommendation to apply, in which case the pod keeps
// what it has.
type Sizer func(pod discover.Pod) (cpu, mem int64, ok bool)

// Replay packs the cluster twice: once with today's requests and once with the
// recommendations. The difference between the two is the answer; the gap
// between the first and what is actually running is reported separately.
func Replay(inv discover.Inventory, pods []discover.Pod, size Sizer) Result {
	r := Result{PinReasons: map[string]int{}}
	if inv.Partial || len(inv.Nodes) == 0 {
		r.SelfCheck = "no node inventory: the node-count delta cannot be computed"
		return r
	}

	occupied := map[string]bool{}
	for _, p := range pods {
		occupied[p.Node] = true
	}
	r.NodesRunning = len(occupied)

	// Self-check: every pod must fit on the node it is demonstrably running
	// on. This is the claim the model can actually be held to. Checking
	// instead that a replay reproduces the current NODE COUNT would fail on
	// every cluster that is merely not tightly packed, which is most of them,
	// and that gap is the cluster's existing slack rather than a model error.
	//
	// A failure here means the capacity accounting is wrong - effective
	// requests, pod overhead, or allocatable read incorrectly - and a model
	// that cannot fit the present has no business predicting a future.
	if bad, detail := overcommitted(inv, pods); bad {
		r.SelfCheck = "self-check failed: " + detail +
			". tare's capacity accounting disagrees with a placement the scheduler actually made, so the node-count delta is withheld."
		return r
	}

	now := pack(inv, pods, nil)
	rec := pack(inv, pods, size)
	r.NodesPackedNow = now.used
	r.NodesPackedRecommended = rec.used
	r.Pinned, r.Movable = rec.pinned, rec.movable
	r.PinReasons = rec.pinReasons
	r.ExcludedNode = rec.excluded
	r.Unusable = rec.unusable
	r.Removable = rec.empty
	r.EmptyToday = now.empty
	r.Trustworthy = true
	r.SelfCheck = fmt.Sprintf(
		"self-check passed: every one of %d pods fits the node it is running on, so the capacity model agrees with the scheduler",
		len(pods))
	return r
}

// overcommitted reports a node whose pods request more than it has allocatable.
func overcommitted(inv discover.Inventory, pods []discover.Pod) (bool, string) {
	cpu := map[string]int64{}
	mem := map[string]int64{}
	for _, p := range pods {
		cpu[p.Node] += p.CPU
		mem[p.Node] += p.Mem
	}
	names := make([]string, 0, len(cpu))
	for n := range cpu {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		n, ok := inv.Get(name)
		if !ok {
			return true, fmt.Sprintf("pods are running on node %q, which is not in the node inventory", name)
		}
		if cpu[name] > n.AllocCPU {
			return true, fmt.Sprintf("node %q hosts pods requesting %dm CPU against %dm allocatable",
				name, cpu[name], n.AllocCPU)
		}
		if mem[name] > n.AllocMem {
			return true, fmt.Sprintf("node %q hosts pods requesting %d bytes of memory against %d allocatable",
				name, mem[name], n.AllocMem)
		}
	}
	return false, ""
}

type packResult struct {
	used       int
	empty      []string
	pinned     int
	movable    int
	excluded   int
	unusable   []string
	pinReasons map[string]int
}

// pack is first-fit-decreasing over the schedulable nodes, with pinned pods
// placed first on the nodes they already occupy.
//
// First-fit-decreasing is not optimal and is not meant to be. An optimal
// packing would promise a consolidation no real cluster performs, because
// Karpenter and the cluster autoscaler are themselves greedy and because pods
// move one disruption budget at a time. A greedy answer is the closer model.
func pack(inv discover.Inventory, pods []discover.Pod, size Sizer) packResult {
	out := packResult{pinReasons: map[string]int{}}

	type bin struct {
		node     discover.Node
		freeCPU  int64
		freeMem  int64
		occupied bool
	}
	bins := map[string]*bin{}
	var order []*bin
	for _, n := range inv.Nodes {
		if !n.Schedulable {
			out.excluded++
			continue
		}
		b := &bin{node: n, freeCPU: n.AllocCPU, freeMem: n.AllocMem}
		bins[n.Name] = b
		order = append(order, b)
	}
	requests := func(p discover.Pod) (int64, int64) {
		if size == nil {
			return p.CPU, p.Mem
		}
		if cpu, mem, ok := size(p); ok {
			return cpu, mem
		}
		return p.CPU, p.Mem
	}

	// DaemonSet pods are per-node overhead, not cargo: one copy runs on every
	// node the set selects, and none on the nodes it does not. They are
	// charged to those bins up front, so a node whose remaining space is
	// entirely taken by agents is correctly seen as unable to host a workload.
	//
	// The charge is per DaemonSet, not per DaemonSet pod. Charging each pod to
	// every bin multiplied the overhead by the number of nodes: on the
	// three-node lab every node was billed three node-exporters, and the
	// replay believed the cluster had less room than it has.
	//
	// Where a set runs is the union of the nodes its nodeSelector matches and
	// the nodes its pods are observed on. The second is not redundant - a node
	// whose labels were not read matches no selector at all - and without it
	// the model would hand out capacity an agent is demonstrably occupying.
	type charge struct {
		cpu, mem int64
		sel      map[string]string
		on       map[string]bool
	}
	agents := map[string]*charge{}
	for _, p := range pods {
		if p.WorkloadKind != "DaemonSet" {
			continue
		}
		key := p.Namespace + "/" + p.Workload
		c, ok := agents[key]
		if !ok {
			c = &charge{sel: p.NodeSelector, on: map[string]bool{}}
			agents[key] = c
		}
		// The largest pod of the set: a set whose pods differ is one caught
		// mid-rollout, and the bigger of the two is what a node must hold.
		cpu, mem := requests(p)
		c.cpu, c.mem = max(c.cpu, cpu), max(c.mem, mem)
		c.on[p.Node] = true
	}
	for _, c := range agents {
		for _, b := range order {
			if !c.on[b.node.Name] && !b.node.Matches(c.sel) {
				continue
			}
			b.freeCPU -= c.cpu
			b.freeMem -= c.mem
		}
	}

	// A pod on a node that has stopped reporting is not evidence of anything.
	// The API server keeps every pod object a vanished kubelet last owned, so
	// the pod list goes on showing them Running for as long as the node is
	// gone - on the lab cluster, seventeen of them for an hour and a half.
	// Packing those as cargo would fold a dead node's phantoms onto a live one
	// and report space they never occupied as a saving.
	held := func(p discover.Pod) string {
		if p.Pinned {
			return p.PinReason
		}
		if n, ok := inv.Get(p.Node); ok && !n.Ready {
			return "its node has stopped reporting, so the pod may not be running at all"
		}
		return ""
	}

	// Pinned pods hold their current node; the rest are placed below.
	var movable []discover.Pod
	for _, p := range pods {
		if p.WorkloadKind == "DaemonSet" {
			continue
		}
		reason := held(p)
		if reason == "" {
			out.movable++
			movable = append(movable, p)
			continue
		}
		out.pinned++
		out.pinReasons[reason]++
		b, ok := bins[p.Node]
		if !ok {
			continue // pinned to a cordoned or vanished node; it stays there
		}
		cpu, mem := requests(p)
		b.freeCPU -= cpu
		b.freeMem -= mem
		b.occupied = true
	}

	// A node held by a pinned pod can never be freed, so whatever space is
	// left on it is the only free real estate in the replay: filling it costs
	// nothing, while filling any other node keeps that node alive.
	// First-fit-decreasing over the largest node first leaves the pinned node
	// half empty and reports one node more than the cluster needs. Both
	// orderings produce a packing that demonstrably fits, so this is a
	// stronger claim rather than a looser one - and on a cluster where most
	// pods are pinned, which the breadth of the pin list makes common, it is
	// most of the answer.
	sort.SliceStable(order, func(i, j int) bool {
		if order[i].occupied != order[j].occupied {
			return order[i].occupied
		}
		if order[i].freeCPU != order[j].freeCPU {
			return order[i].freeCPU > order[j].freeCPU
		}
		return order[i].node.Name < order[j].node.Name
	})

	// Destinations are the bins that can actually receive a pod. A node whose
	// kubelet has stopped reporting will not start a container, and a tainted
	// node refuses every pod the replay is willing to move - by construction,
	// since tolerating a taint is itself a pin reason. Both were previously
	// offered as free space, the second despite the taints having been read
	// from the cluster since the first version and used by nothing.
	blocked := map[string]bool{}
	var dest []*bin
	for _, b := range order {
		switch {
		case !b.node.Ready:
			blocked[b.node.Name] = true
			out.unusable = append(out.unusable,
				b.node.Name+": the kubelet has stopped reporting, so nothing can start there")
		case b.node.BlockingTaint() != "":
			blocked[b.node.Name] = true
			out.unusable = append(out.unusable, b.node.Name+": "+b.node.BlockingTaint())
		default:
			dest = append(dest, b)
		}
	}
	sort.Strings(out.unusable)

	// Movable pods, biggest first.
	sort.Slice(movable, func(i, j int) bool {
		ci, _ := requests(movable[i])
		cj, _ := requests(movable[j])
		if ci != cj {
			return ci > cj
		}
		if movable[i].Namespace != movable[j].Namespace {
			return movable[i].Namespace < movable[j].Namespace
		}
		return movable[i].Name < movable[j].Name
	})
	for _, p := range movable {
		cpu, mem := requests(p)
		placed := false
		for _, b := range dest {
			if b.freeCPU >= cpu && b.freeMem >= mem {
				b.freeCPU -= cpu
				b.freeMem -= mem
				b.occupied = true
				placed = true
				break
			}
		}
		if !placed {
			// Nothing fits it. Leave it on its own node and count that node as
			// used: a pod the model cannot place is not a pod that disappears.
			if b, ok := bins[p.Node]; ok {
				b.occupied = true
			}
		}
	}

	for _, b := range order {
		switch {
		case b.occupied:
			out.used++
		case blocked[b.node.Name]:
			// Never a destination, so not a node these recommendations freed.
		default:
			out.empty = append(out.empty, b.node.Name)
		}
	}
	sort.Strings(out.empty)
	return out
}

// SortedPinReasons is the pin reasons most common first, then by name, so
// every renderer lists them in the same order.
func (r Result) SortedPinReasons() []string {
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
	return reasons
}
