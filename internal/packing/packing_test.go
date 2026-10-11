package packing

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/mfranzon/tare/internal/discover"
)

const gib = 1 << 30

func node(name string, cores int64) discover.Node {
	return discover.Node{
		Name: name, Cores: cores,
		AllocCPU: cores * 1000, AllocMem: cores * 4 * gib,
		Schedulable: true, Ready: true, Arch: "amd64",
	}
}

func pod(name, on string, cpu int64, mem int64) discover.Pod {
	return discover.Pod{
		Namespace: "default", Name: name, Node: on,
		Workload: "Deployment/" + name, WorkloadKind: "Deployment",
		CPU: cpu, Mem: mem,
		UnlimitedCPU: 1, UnlimitedMem: 1, // one container, no limits
	}
}

func halve(p discover.Pod) (int64, int64, bool) { return p.CPU / 2, p.Mem / 2, true }

func TestNoInventoryProducesNoNumber(t *testing.T) {
	r := Replay(discover.Inventory{Partial: true}, []discover.Pod{pod("a", "n1", 500, gib)}, halve)
	if r.Trustworthy {
		t.Fatal("a missing inventory must not yield a trustworthy result")
	}
	if r.FromResizing() != 0 {
		t.Errorf("no inventory must mean no delta, got %d", r.FromResizing())
	}
}

// The failure this guards: an unconsolidated cluster is the normal case, and a
// self-check that demanded the replay reproduce the current node count would
// reject every one of them.
func TestLooselyPackedClusterStillSelfChecks(t *testing.T) {
	inv := discover.Inventory{Nodes: []discover.Node{node("n1", 8), node("n2", 8), node("n3", 8)}}
	pods := []discover.Pod{
		pod("a", "n1", 500, gib), pod("b", "n2", 500, gib), pod("c", "n3", 500, gib),
	}
	r := Replay(inv, pods, nil)
	if !r.Trustworthy {
		t.Fatalf("a loosely packed cluster is not a model failure: %s", r.SelfCheck)
	}
	if r.NodesRunning != 3 {
		t.Errorf("NodesRunning = %d, want 3", r.NodesRunning)
	}
	// All three tiny pods fit on one node, and that saving needs no resizing.
	if r.FromConsolidation() != 2 {
		t.Errorf("FromConsolidation = %d, want 2", r.FromConsolidation())
	}
	if r.FromResizing() != 0 {
		t.Errorf("resizing nothing must free nothing, got %d", r.FromResizing())
	}
}

// The credit-taking failure: tare must not report the cluster's pre-existing
// slack as a saving its recommendations produced.
func TestConsolidationSavingIsNotCreditedToResizing(t *testing.T) {
	inv := discover.Inventory{Nodes: []discover.Node{node("n1", 8), node("n2", 8)}}
	pods := []discover.Pod{pod("a", "n1", 100, gib), pod("b", "n2", 100, gib)}
	r := Replay(inv, pods, halve)
	if r.FromConsolidation() != 1 {
		t.Errorf("FromConsolidation = %d, want 1", r.FromConsolidation())
	}
	if r.FromResizing() != 0 {
		t.Errorf("halving two pods that already share one node frees no further node, got %d", r.FromResizing())
	}
	// The count was right and the names were not: Removable held n2 while
	// FromResizing was 0, and the report printed it as a node tare freed.
	if len(r.EmptyToday) != 1 {
		t.Errorf("EmptyToday = %v, want the one node repacking alone empties", r.EmptyToday)
	}
}

func TestResizingFreesANode(t *testing.T) {
	// Four pods at 6 cores each need four 8-core nodes. At 3 cores each, two.
	inv := discover.Inventory{Nodes: []discover.Node{node("n1", 8), node("n2", 8), node("n3", 8), node("n4", 8)}}
	var pods []discover.Pod
	for _, n := range []string{"n1", "n2", "n3", "n4"} {
		pods = append(pods, pod("p-"+n, n, 6000, 4*gib))
	}
	r := Replay(inv, pods, halve)
	if !r.Trustworthy {
		t.Fatalf("self-check failed: %s", r.SelfCheck)
	}
	if r.NodesPackedNow != 4 {
		t.Errorf("NodesPackedNow = %d, want 4: 6-core pods do not share an 8-core node", r.NodesPackedNow)
	}
	if r.FromResizing() != 2 {
		t.Errorf("FromResizing = %d, want 2", r.FromResizing())
	}
	if len(r.Removable) != 2 {
		t.Errorf("Removable = %v, want two nodes named", r.Removable)
	}
}

func TestPinnedPodsBlockConsolidation(t *testing.T) {
	inv := discover.Inventory{Nodes: []discover.Node{node("n1", 8), node("n2", 8), node("n3", 8)}}
	var pods []discover.Pod
	for _, n := range []string{"n1", "n2", "n3"} {
		p := pod("p-"+n, n, 500, gib)
		p.Pinned, p.PinReason = true, "has a PersistentVolumeClaim, which may bind it to a zone or a node"
		pods = append(pods, p)
	}
	r := Replay(inv, pods, halve)
	if r.FromResizing() != 0 || r.FromConsolidation() != 0 {
		t.Errorf("pinned pods cannot be consolidated: resizing=%d consolidation=%d",
			r.FromResizing(), r.FromConsolidation())
	}
	if r.Pinned != 3 || r.Movable != 0 {
		t.Errorf("Pinned=%d Movable=%d, want 3 and 0", r.Pinned, r.Movable)
	}
	if r.PinReasons["has a PersistentVolumeClaim, which may bind it to a zone or a node"] != 3 {
		t.Errorf("the pin reasons must be countable by a reader: %v", r.PinReasons)
	}
}

// A DaemonSet is charged to every node rather than packed. Without that, the
// replay believes a node has capacity that its agents have already taken, and
// promises a consolidation that would evict them.
func TestDaemonSetIsPerNodeOverhead(t *testing.T) {
	inv := discover.Inventory{Nodes: []discover.Node{node("n1", 8), node("n2", 8)}}
	pods := []discover.Pod{
		pod("app-a", "n1", 3000, gib),
		pod("app-b", "n2", 3000, gib),
	}
	bare := Replay(inv, pods, nil)
	if bare.NodesPackedNow != 1 {
		t.Fatalf("two 3-core pods fit one 8-core node: got %d", bare.NodesPackedNow)
	}

	// Add an agent taking 3 cores on every node. 8 - 3 = 5 allocatable, so the
	// two 3-core pods no longer share.
	for _, n := range []string{"n1", "n2"} {
		ds := pod("agent-"+n, n, 3000, gib)
		ds.Workload, ds.WorkloadKind = "DaemonSet/agent", "DaemonSet"
		pods = append(pods, ds)
	}
	withAgents := Replay(inv, pods, nil)
	if withAgents.NodesPackedNow != 2 {
		t.Errorf("NodesPackedNow = %d, want 2: the agent's request is not free space",
			withAgents.NodesPackedNow)
	}
}

func TestOvercommittedNodeFailsTheSelfCheck(t *testing.T) {
	// A pod demonstrably running on a node that tare believes is too small
	// means tare's accounting is wrong, not that the cluster is.
	inv := discover.Inventory{Nodes: []discover.Node{node("n1", 2)}}
	pods := []discover.Pod{pod("a", "n1", 9000, gib)}
	r := Replay(inv, pods, halve)
	if r.Trustworthy {
		t.Fatal("an impossible current placement must fail the self-check")
	}
	if !strings.Contains(r.SelfCheck, "allocatable") {
		t.Errorf("the self-check must say what disagreed: %s", r.SelfCheck)
	}
	if r.FromResizing() != 0 {
		t.Errorf("a failed self-check must withhold the delta, got %d", r.FromResizing())
	}
}

func TestPodsOnUnknownNodeFailTheSelfCheck(t *testing.T) {
	inv := discover.Inventory{Nodes: []discover.Node{node("n1", 8)}}
	pods := []discover.Pod{pod("a", "ghost", 100, gib)}
	r := Replay(inv, pods, halve)
	if r.Trustworthy {
		t.Fatal("a pod on a node not in the inventory means the inventory is incomplete")
	}
}

func TestCordonedNodeIsNotADestination(t *testing.T) {
	cordoned := node("n2", 64)
	cordoned.Schedulable = false
	inv := discover.Inventory{Nodes: []discover.Node{node("n1", 8), cordoned}}
	pods := []discover.Pod{pod("a", "n1", 1000, gib)}
	r := Replay(inv, pods, nil)
	if r.ExcludedNode != 1 {
		t.Errorf("ExcludedNode = %d, want 1", r.ExcludedNode)
	}
	for _, n := range r.Removable {
		if n == "n2" {
			t.Error("a cordoned node must not be reported as newly removable: it was never a destination")
		}
	}
}

// A sizer that raises requests must be able to make things worse. A model that
// can only ever report a saving is not a model.
func TestRaisingRequestsCanCostANode(t *testing.T) {
	inv := discover.Inventory{Nodes: []discover.Node{node("n1", 8), node("n2", 8)}}
	pods := []discover.Pod{pod("a", "n1", 3000, gib), pod("b", "n1", 3000, gib)}
	double := func(p discover.Pod) (int64, int64, bool) { return p.CPU * 2, p.Mem, true }
	r := Replay(inv, pods, double)
	if r.FromResizing() >= 0 {
		t.Errorf("doubling two 3-core pods past one node must cost a node, got %d", r.FromResizing())
	}
}

// labelled gives a node the labels a nodeSelector is evaluated against.
func labelled(n discover.Node, kv ...string) discover.Node {
	n.Labels = map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		n.Labels[kv[i]] = kv[i+1]
	}
	return n
}

// agent is one pod of the DaemonSet named set, running on node on.
func agent(set, on string, cpu, mem int64) discover.Pod {
	p := pod(set+"-"+on, on, cpu, mem)
	p.Workload, p.WorkloadKind = "DaemonSet/"+set, "DaemonSet"
	return p
}

// A DaemonSet is one reservation per node, not one per pod of the set.
//
// The replay charged every DaemonSet POD to every bin, so the overhead a node
// appeared to carry grew with the size of the cluster: on the three-node lab
// each node was billed three node-exporters, on a thirty-node cluster thirty.
// The error is pessimistic, which is why nothing caught it - the reported
// saving was merely too small, and a too-small saving reads as caution.
//
// The numbers below are chosen to discriminate. Charged once, each node keeps
// 7000m and all three app pods fit on one; charged three times, each node keeps
// 5000m, the third pod finds no room beside the other two and the replay
// reports a node that cannot be freed.
func TestDaemonSetIsChargedOncePerNode(t *testing.T) {
	inv := discover.Inventory{Nodes: []discover.Node{node("n1", 8), node("n2", 8), node("n3", 8)}}
	var pods []discover.Pod
	for _, n := range []string{"n1", "n2", "n3"} {
		pods = append(pods, agent("node-exporter", n, 1000, gib), pod("app-"+n, n, 2000, gib))
	}
	r := Replay(inv, pods, nil)
	if !r.Trustworthy {
		t.Fatalf("self-check failed: %s", r.SelfCheck)
	}
	if r.NodesPackedNow != 1 {
		t.Errorf("NodesPackedNow = %d, want 1: one agent per node costs 1000m there, not 3000m",
			r.NodesPackedNow)
	}
}

// A DaemonSet with a nodeSelector is overhead only where it runs. Charging it
// to the whole cluster takes capacity away from nodes that will never host it,
// which is how a GPU device plugin reserving 5 cores makes every CPU node in
// the fleet look nearly full.
//
// Discriminating numbers again: charged to gpu-0 alone, a CPU node keeps its
// 8000m and both 3500m apps share it; charged everywhere, a CPU node keeps
// 3000m and neither app can move at all.
func TestNodeSelectorDaemonSetIsChargedOnlyWhereItRuns(t *testing.T) {
	inv := discover.Inventory{Nodes: []discover.Node{
		labelled(node("gpu-0", 8), "accelerator", "nvidia"),
		labelled(node("cpu-0", 8), "accelerator", "none"),
		labelled(node("cpu-1", 8), "accelerator", "none"),
	}}
	dev := agent("device-plugin", "gpu-0", 5000, gib)
	dev.NodeSelector = map[string]string{"accelerator": "nvidia"}
	pods := []discover.Pod{
		dev,
		pod("app-a", "cpu-0", 3500, gib),
		pod("app-b", "cpu-1", 3500, gib),
	}
	r := Replay(inv, pods, nil)
	if !r.Trustworthy {
		t.Fatalf("self-check failed: %s", r.SelfCheck)
	}
	if r.NodesPackedNow != 1 {
		t.Errorf("NodesPackedNow = %d, want 1: both apps fit one untouched CPU node, "+
			"because the device plugin is gpu-0's reservation and nobody else's", r.NodesPackedNow)
	}
}

// The negative control for the rule above. A selector is matched against the
// node's labels, and an inventory that could not read them matches nothing -
// so a set demonstrably running on a node must still be charged there. Getting
// this wrong is the expensive direction: the replay hands out capacity an
// agent is currently occupying and promises a consolidation onto a node with
// no room for it.
//
// n1 is held by a pinned pod so that it cannot simply be emptied, which is
// what makes the agent's charge on it observable at all.
func TestDaemonSetIsChargedWhereItRunsEvenWithoutNodeLabels(t *testing.T) {
	inv := discover.Inventory{Nodes: []discover.Node{node("n1", 8), node("n2", 8)}}
	ds := agent("agent", "n1", 5000, gib)
	ds.NodeSelector = map[string]string{"role": "infra"} // no node in this inventory carries labels
	held := pod("held", "n1", 2000, gib)
	held.Pinned, held.PinReason = true, "has a hostPath"
	pods := []discover.Pod{ds, held, pod("app", "n2", 2000, gib)}

	r := Replay(inv, pods, nil)
	if !r.Trustworthy {
		t.Fatalf("self-check failed: %s", r.SelfCheck)
	}
	if r.NodesPackedNow != 2 {
		t.Errorf("NodesPackedNow = %d, want 2: n1 has 1000m left beside an agent it is "+
			"demonstrably running, so the 2000m app cannot join it there", r.NodesPackedNow)
	}
}

// The shape the lab cluster actually has, and the third of the live
// observation the declaration shapes could not carry: k3s pins
// local-path-provisioner and metrics-server to the server node with a
// nodeSelector. Such a pod holds its node for the whole replay, and the floor
// on the node count is that pinning rather than any capacity.
func TestNodeSelectorPinsThePodAndItsNode(t *testing.T) {
	inv := discover.Inventory{Nodes: []discover.Node{
		labelled(node("server-0", 16), "node-role.kubernetes.io/control-plane", "true"),
		node("agent-0", 16), node("agent-1", 16),
	}}
	const reason = "has a nodeSelector"
	var pods []discover.Pod
	for _, n := range []string{"local-path-provisioner", "metrics-server"} {
		p := pod(n, "server-0", 100, 128<<20)
		p.NodeSelector = map[string]string{"node-role.kubernetes.io/control-plane": "true"}
		p.Pinned, p.PinReason = true, reason
		pods = append(pods, p)
	}
	pods = append(pods, pod("web", "agent-0", 500, gib), pod("api", "agent-1", 500, gib))

	r := Replay(inv, pods, halve)
	if !r.Trustworthy {
		t.Fatalf("self-check failed: %s", r.SelfCheck)
	}
	if r.Pinned != 2 || r.PinReasons[reason] != 2 {
		t.Errorf("Pinned=%d reasons=%v, want two pods held by their nodeSelector", r.Pinned, r.PinReasons)
	}
	// server-0 cannot be emptied, and everything else folds onto it.
	if r.NodesPackedNow != 1 {
		t.Errorf("NodesPackedNow = %d, want 1: the movable pods fit beside the pinned ones", r.NodesPackedNow)
	}
	for _, n := range r.Removable {
		if n == "server-0" {
			t.Error("a node held by a nodeSelector-pinned pod was reported removable")
		}
	}
	if r.FromResizing() != 0 {
		t.Errorf("FromResizing = %d: the floor here is pinning, not capacity, and resizing cannot lower it",
			r.FromResizing())
	}
}

// A pinned pod does not move, but it is still resized. Skipping that would
// leave the replay packing a future where the pinned half of the cluster kept
// its old requests, and on a cluster where most pods are pinned - which the
// pin list makes common - the recommendations would appear to change nothing.
func TestPinnedPodIsResizedWhereItSits(t *testing.T) {
	inv := discover.Inventory{Nodes: []discover.Node{node("n1", 8), node("n2", 8)}}
	held := pod("infra", "n1", 7500, gib)
	held.NodeSelector = map[string]string{"role": "infra"}
	held.Pinned, held.PinReason = true, "has a nodeSelector"
	pods := []discover.Pod{held, pod("app", "n2", 2000, gib)}

	if now := Replay(inv, pods, nil); now.NodesPackedNow != 2 {
		t.Fatalf("precondition: NodesPackedNow = %d, want 2 - 500m is left beside the pinned pod", now.NodesPackedNow)
	}
	r := Replay(inv, pods, halve)
	if r.FromResizing() != 1 {
		t.Errorf("FromResizing = %d, want 1: shrinking the pinned pod to 3750m is what opens "+
			"room on n1 for the app, and nothing else can", r.FromResizing())
	}
}

// A node whose kubelet has stopped reporting keeps every pod object it owned,
// so the pod list shows them Running indefinitely. The lab cluster produced
// exactly this on 2026-09-21: two of three nodes unreachable since 08:03, and
// seventeen phantom pods still listed as Running an hour and a half later.
//
// Treating those as cargo is the expensive direction twice over - it packs
// pods that may not exist onto live nodes, and it offers a dead node's
// capacity to everything else.
func TestDeadNodeIsNotADestinationAndItsPodsAreHeld(t *testing.T) {
	dead := node("n2", 8)
	dead.Ready = false
	inv := discover.Inventory{Nodes: []discover.Node{node("n1", 8), dead}}
	pods := []discover.Pod{pod("live", "n1", 7000, gib), pod("phantom", "n2", 1000, gib)}

	r := Replay(inv, pods, nil)
	if !r.Trustworthy {
		t.Fatalf("an unreachable node is a cluster condition, not a model failure: %s", r.SelfCheck)
	}
	const reason = "its node has stopped reporting, so the pod may not be running at all"
	if r.PinReasons[reason] != 1 {
		t.Errorf("pin reasons = %v, want the phantom held where it is", r.PinReasons)
	}
	if r.NodesPackedNow != 2 {
		t.Errorf("NodesPackedNow = %d, want 2: the live pod cannot be sent to a node "+
			"that will not start a container", r.NodesPackedNow)
	}
	if len(r.Removable) != 0 {
		t.Errorf("Removable = %v, want none: a dead node is not a node these recommendations freed",
			r.Removable)
	}
	if len(r.Unusable) != 1 || !strings.Contains(r.Unusable[0], "n2") {
		t.Errorf("Unusable = %v, want n2 named with its reason", r.Unusable)
	}
}

// Taints were read from the cluster by the first version of the inventory and
// used by nothing. Every pod the replay will move tolerates nothing beyond the
// kubelet's two defaults - any other toleration is itself a pin reason - so a
// tainted node can receive none of them, and offering it as free space
// promises a consolidation the scheduler refuses outright.
func TestTaintedNodeIsNotFreeSpace(t *testing.T) {
	tainted := node("big-tainted", 64)
	tainted.Taints = []corev1.Taint{{Key: "dedicated", Value: "gpu", Effect: corev1.TaintEffectNoSchedule}}
	inv := discover.Inventory{Nodes: []discover.Node{node("n1", 8), tainted, node("n3", 8)}}
	pods := []discover.Pod{pod("app-a", "n1", 5000, gib), pod("app-b", "n3", 4000, gib)}

	r := Replay(inv, pods, nil)
	if !r.Trustworthy {
		t.Fatalf("self-check failed: %s", r.SelfCheck)
	}
	if r.NodesPackedNow != 2 {
		t.Errorf("NodesPackedNow = %d, want 2: 9000m of pods do not fit one 8-core node, "+
			"and the 64-core node refuses both", r.NodesPackedNow)
	}
	if len(r.Unusable) != 1 || !strings.Contains(r.Unusable[0], "dedicated") {
		t.Errorf("Unusable = %v, want the taint named", r.Unusable)
	}
}
