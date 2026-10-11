package discover

import (
	"context"
	"fmt"
	"sort"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Node is the scheduling-relevant view of a cluster node.
//
// It exists for two jobs that the container inventory cannot do alone. The
// first is arithmetic: reclaimed millicores are not a saving until a node goes
// away, so answering "does applying this do anything" needs allocatable
// capacity and current placement. The second is refusal: a recommendation
// larger than any node can hold makes the pod Pending, and a runtime that
// sized its thread pool from the node rather than from its quota is a
// configuration defect that resizing cannot fix.
type Node struct {
	Name string

	// Cores is capacity.cpu rounded to whole cores: what an unconfigured
	// runtime sees through sysconf(_SC_NPROCESSORS_ONLN) and sizes its thread
	// pool from, regardless of the cgroup quota it actually has.
	Cores int64

	// AllocCPU and AllocMem are status.allocatable, which is capacity minus
	// kube-reserved, system-reserved and eviction thresholds. Allocatable, not
	// capacity, is what the scheduler fits pods into.
	AllocCPU int64 // millicores
	AllocMem int64 // bytes

	Arch         string
	InstanceType string

	// InternalIP is how node-level metrics identify this machine. node-exporter
	// labels its series with an `instance` of host:port and carries no node
	// name, so this address is the only join between what Kubernetes calls a
	// node and what Prometheus knows about it.
	InternalIP string

	// GPUs is the count of whole accelerator devices the node advertises, and
	// GPUResource the extended resource name they are advertised under
	// (nvidia.com/gpu, amd.com/gpu). Kubernetes allocates these as integers:
	// there is no fractional request, so this is never something to right-size
	// and always something to check a configuration against.
	GPUs        int64
	GPUResource string

	// GPUProduct and GPUMemBytes come from the labels the NVIDIA GPU Operator
	// and node-feature-discovery apply. They are frequently absent - a
	// hand-installed device plugin sets neither - and absence must read as
	// "unknown", never as zero.
	GPUProduct  string
	GPUMemBytes int64
	MIGCapable  bool

	// Schedulable is false for a cordoned node. Such a node still runs its
	// pods, so it counts for current placement and must not count as a
	// destination for anything.
	Schedulable bool

	// Ready is the kubelet's Ready condition. A node that has stopped
	// reporting keeps every pod object the API server already had, so the pod
	// list still shows them Running long after the machine is gone. Reading
	// placement without reading this counts a dead node's phantom pods as
	// live and offers its capacity to the packer.
	Ready bool

	// Labels are the node's labels, kept so that a nodeSelector can be
	// evaluated against them. A DaemonSet that selects part of the cluster is
	// per-node overhead only on the nodes it selects, and charging it to the
	// rest understates their capacity.
	Labels map[string]string
	Taints []corev1.Taint
}

// Matches reports whether the node satisfies a nodeSelector, which selects a
// node when every key is present with the same value. An empty selector
// selects every node.
//
// A node whose labels were not read matches nothing but the empty selector, so
// a caller that must not overstate capacity cannot rely on this alone: a pod
// demonstrably running on a node runs there whatever the inventory says.
func (n Node) Matches(sel map[string]string) bool {
	for k, v := range sel {
		if n.Labels[k] != v {
			return false
		}
	}
	return true
}

// Inventory is every node in the cluster.
type Inventory struct {
	Nodes []Node

	// Partial records that the node list could not be read. Every consumer
	// must degrade rather than compute a confident wrong answer from an empty
	// inventory: zero nodes is indistinguishable from "no permission to list
	// nodes", and the second must never render as "0 nodes can be removed".
	Partial bool
	Reason  string
}

// Nodes lists the cluster's nodes. A failure is not fatal: listing nodes needs
// a cluster-scoped read that a namespace-scoped user may not have, and the
// sizing report is still worth printing without it.
func (c *Client) Nodes(ctx context.Context) Inventory {
	list, err := c.cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return Inventory{Partial: true, Reason: fmt.Sprintf("cannot list nodes (%v): "+
			"per-node findings and the node-count delta are omitted", err)}
	}
	inv := Inventory{}
	for i := range list.Items {
		n := &list.Items[i]
		node := Node{
			Name:         n.Name,
			Cores:        n.Status.Capacity.Cpu().Value(),
			AllocCPU:     n.Status.Allocatable.Cpu().MilliValue(),
			AllocMem:     n.Status.Allocatable.Memory().Value(),
			Arch:         n.Labels["kubernetes.io/arch"],
			InstanceType: n.Labels["node.kubernetes.io/instance-type"],
			Schedulable:  !n.Spec.Unschedulable,
			Ready:        isReady(n),
			Taints:       n.Spec.Taints,
			InternalIP:   internalIP(n),
		}
		readGPU(n, &node)
		node.Labels = n.Labels
		inv.Nodes = append(inv.Nodes, node)
	}
	sort.Slice(inv.Nodes, func(a, b int) bool { return inv.Nodes[a].Name < inv.Nodes[b].Name })
	return inv
}

// isReady reports the kubelet's Ready condition.
//
// Unknown is the important value and the reason this is a function rather than
// a comparison at the call site: when a kubelet stops reporting, the condition
// goes to Unknown rather than False, which reads as true to anything testing
// for "not False". Only an explicit True means the node is live.
func isReady(n *corev1.Node) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// BlockingTaint returns the reason this node refuses ordinary pods, or "".
//
// Every pod the replay is willing to move tolerates nothing beyond the two
// tolerations the kubelet adds to all pods, because any other toleration is
// itself a pin reason. So a node carrying a NoSchedule or NoExecute taint can
// receive none of them. The taints were collected from the first version and
// never read by anything, which is how the replay came to offer a tainted node
// as free space.
func (n Node) BlockingTaint() string {
	for _, t := range n.Taints {
		if t.Effect != corev1.TaintEffectNoSchedule && t.Effect != corev1.TaintEffectNoExecute {
			continue // PreferNoSchedule is a preference, not a refusal
		}
		switch t.Key {
		case corev1.TaintNodeNotReady, corev1.TaintNodeUnreachable:
			continue // the kubelet's own doing, and reported as unreadiness instead
		}
		return fmt.Sprintf("carries the taint %s=%s:%s, which no pod the replay can move tolerates",
			t.Key, t.Value, t.Effect)
	}
	return ""
}

// internalIP returns the node's cluster-internal address, or "".
func internalIP(n *corev1.Node) string {
	for _, a := range n.Status.Addresses {
		if a.Type == corev1.NodeInternalIP {
			return a.Address
		}
	}
	return ""
}

// IPs lists the internal addresses of every node, for a metrics backend that
// keys by address rather than by name.
func (inv Inventory) IPs() []string {
	var out []string
	for _, n := range inv.Nodes {
		if n.InternalIP != "" {
			out = append(out, n.InternalIP)
		}
	}
	return out
}

// ByIP finds the node with the given internal address.
func (inv Inventory) ByIP(ip string) (Node, bool) {
	for _, n := range inv.Nodes {
		if n.InternalIP != "" && n.InternalIP == ip {
			return n, true
		}
	}
	return Node{}, false
}

// gpuResources are the extended resources that mean "a whole accelerator".
var gpuResources = []corev1.ResourceName{"nvidia.com/gpu", "amd.com/gpu", "intel.com/gpu"}

// readGPU fills in what the node says about its accelerators.
//
// Every field here is optional in practice. A cluster running the NVIDIA GPU
// Operator labels its nodes richly; a cluster with a hand-installed device
// plugin advertises the count and nothing else. Consumers must degrade on each
// field independently rather than assuming that a GPU count implies a known
// device memory size.
func readGPU(n *corev1.Node, out *Node) {
	for _, name := range gpuResources {
		if q, ok := n.Status.Capacity[name]; ok && q.Value() > 0 {
			out.GPUs, out.GPUResource = q.Value(), string(name)
			break
		}
	}
	out.GPUProduct = n.Labels["nvidia.com/gpu.product"]
	out.MIGCapable = n.Labels["nvidia.com/mig.capable"] == "true"
	// nvidia.com/gpu.memory is in MiB, as a plain integer label value.
	if v := n.Labels["nvidia.com/gpu.memory"]; v != "" {
		if mib, err := strconv.ParseInt(v, 10, 64); err == nil && mib > 0 {
			out.GPUMemBytes = mib << 20
		}
	}
}

// Get returns the node with the given name.
func (inv Inventory) Get(name string) (Node, bool) {
	for _, n := range inv.Nodes {
		if n.Name == name {
			return n, true
		}
	}
	return Node{}, false
}

// Largest returns the node with the most allocatable CPU and the one with the
// most allocatable memory. They may be the same node and usually are, but on a
// cluster with memory-optimised and compute-optimised pools they are not, and
// a feasibility check that assumes one node holds both maxima will pass a
// recommendation that nothing can actually schedule.
func (inv Inventory) Largest() (cpu, mem int64) {
	for _, n := range inv.Nodes {
		if !n.Schedulable {
			continue
		}
		cpu = max(cpu, n.AllocCPU)
		mem = max(mem, n.AllocMem)
	}
	return cpu, mem
}

// Classes groups nodes by the pair a workload's performance actually varies
// with. A millicore on one architecture is not a millicore on another, and
// there is no way to recover the ratio without running a benchmark, which this
// tool will not do. The honest use of this is to say the workload spans
// classes and lower the confidence, never to invent a scaling factor.
func (inv Inventory) Classes(names []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, name := range names {
		n, ok := inv.Get(name)
		if !ok {
			continue
		}
		cls := n.Arch
		if n.InstanceType != "" {
			cls += "/" + n.InstanceType
		}
		if cls == "" || seen[cls] {
			continue
		}
		seen[cls] = true
		out = append(out, cls)
	}
	sort.Strings(out)
	return out
}
