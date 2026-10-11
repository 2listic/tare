package discover

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Pod is the scheduling-relevant view of a running pod: what it reserves, where
// it is, and whether tare is able to reason about moving it.
type Pod struct {
	Namespace string
	Name      string
	Node      string

	// Workload and WorkloadKind identify the controller, so a recommendation
	// made per workload can be applied to every pod of it.
	Workload     string
	WorkloadKind string

	// CPU and Mem are the pod's effective requests: the sum across regular
	// containers, floored by the largest single init container, which is how
	// the scheduler computes what a pod reserves.
	CPU int64 // millicores
	Mem int64 // bytes

	// CPULimit and MemLimit are what the pod may consume, summed the same way.
	// A node's limits can exceed its allocatable many times over: that is
	// legal, invisible to the scheduler, and the reason a node with plenty of
	// unreserved capacity can still collapse when everything on it gets busy
	// at once.
	CPULimit int64
	MemLimit int64

	// UnlimitedCPU and UnlimitedMem count the pod's containers with no limit
	// on that resource. The sums above cannot say this: one limited container
	// makes the pod's total non-zero and hides every unlimited one beside it.
	UnlimitedCPU int
	UnlimitedMem int

	// NodeSelector is the pod's own nodeSelector. It is a pin reason for a
	// workload pod, and for a DaemonSet - which is charged as per-node
	// overhead rather than packed - it is the set of nodes that overhead
	// belongs to. A DaemonSet selecting three of thirty nodes costs the other
	// twenty-seven nothing.
	NodeSelector map[string]string

	// Pinned marks a pod tare cannot model moving, and PinReason says why.
	// A pinned pod holds its current node for the whole replay. This is the
	// conservative direction on purpose: pinning a pod that could actually
	// have moved understates the saving, while moving one that could not
	// overstates it, and an overstated saving is the failure this tool exists
	// not to produce.
	Pinned    bool
	PinReason string
}

// Pods lists every pod with the fields the bin-packing replay needs.
//
// pdbs may be nil. When supplied, a pod covered by a budget that allows no
// disruption is pinned: a node it sits on cannot be drained, so counting that
// node as removable would promise a consolidation the cluster will refuse.
func (c *Client) Pods(ctx context.Context, namespaces []string, pdbs []PDB) ([]Pod, error) {
	targets := scopeOf(namespaces)
	idx := c.index(namespaces)
	var out []Pod
	for _, ns := range targets {
		list, err := c.cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("listing pods in %s: %w", describeScope(ns), err)
		}
		for i := range list.Items {
			p := &list.Items[i]
			if p.Spec.NodeName == "" || p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
				continue // not occupying a node
			}
			out = append(out, podFrom(p, idx.ownerOf(ctx, p), pdbs))
		}
	}
	return out, nil
}

// podFrom is the whole translation from a Kubernetes pod to the scheduling
// view the replay packs. It is a function of its arguments so that it can be
// tested without a cluster: every field here is one the node-count delta is
// computed from, and a field that is declared and never assigned is a defect
// this project has shipped before.
func podFrom(p *corev1.Pod, workload string, pdbs []PDB) Pod {
	cpu, mem := effectiveRequests(p)
	cpuLim, memLim, noCPULim, noMemLim := effectiveLimits(p)
	pod := Pod{
		Namespace: p.Namespace, Name: p.Name, Node: p.Spec.NodeName,
		Workload: workload, WorkloadKind: kindOf(workload),
		CPU: cpu, Mem: mem,
		CPULimit: cpuLim, MemLimit: memLim,
		UnlimitedCPU: noCPULim, UnlimitedMem: noMemLim,
		NodeSelector: p.Spec.NodeSelector,
	}
	pod.PinReason = pinReason(p, pod.WorkloadKind)
	if pod.PinReason == "" {
		pod.PinReason = blockedByPDB(pdbs, p.Namespace, p.Labels)
	}
	pod.Pinned = pod.PinReason != ""
	return pod
}

// effectiveRequests computes what the scheduler reserves for a pod.
//
// Init containers run one at a time and before the others, so the pod must fit
// the largest of them; regular containers run together, so their requests add.
// Native sidecars (an init container with restartPolicy: Always) run alongside
// the regular containers instead, so they add rather than max. Getting this
// wrong in either direction makes the replay disagree with the scheduler,
// which the self-check then catches.
func effectiveRequests(p *corev1.Pod) (cpu, mem int64) {
	for i := range p.Spec.Containers {
		r := p.Spec.Containers[i].Resources.Requests
		cpu += milli(r, corev1.ResourceCPU)
		mem += bytes(r, corev1.ResourceMemory)
	}
	var initCPU, initMem int64
	for i := range p.Spec.InitContainers {
		ic := &p.Spec.InitContainers[i]
		r := ic.Resources.Requests
		if ic.RestartPolicy != nil && *ic.RestartPolicy == corev1.ContainerRestartPolicyAlways {
			cpu += milli(r, corev1.ResourceCPU)
			mem += bytes(r, corev1.ResourceMemory)
			continue
		}
		initCPU = max(initCPU, milli(r, corev1.ResourceCPU))
		initMem = max(initMem, bytes(r, corev1.ResourceMemory))
	}
	cpu = max(cpu, initCPU)
	mem = max(mem, initMem)
	if o := p.Spec.Overhead; o != nil {
		cpu += milli(o, corev1.ResourceCPU)
		mem += bytes(o, corev1.ResourceMemory)
	}
	return cpu, mem
}

// blockedByPDB returns the reason a disruption budget prevents this pod from
// being evicted, or "".
func blockedByPDB(pdbs []PDB, ns string, podLabels map[string]string) string {
	for _, p := range pdbs {
		if p.Namespace != ns || !p.Blocking || !p.Covers(podLabels) {
			continue
		}
		return fmt.Sprintf("PodDisruptionBudget %s/%s allows no eviction, so its node cannot be drained", p.Namespace, p.Name)
	}
	return ""
}

// effectiveLimits sums what the pod may consume. A container with no limit is
// unbounded, which cannot be summed: the total is then reported as unbounded
// by the caller rather than understated by treating the gap as zero, which is
// what the unlimited counts are for.
func effectiveLimits(p *corev1.Pod) (cpu, mem int64, unlimitedCPU, unlimitedMem int) {
	for i := range p.Spec.Containers {
		l := p.Spec.Containers[i].Resources.Limits
		c, m := milli(l, corev1.ResourceCPU), bytes(l, corev1.ResourceMemory)
		cpu += c
		mem += m
		if c == 0 {
			unlimitedCPU++
		}
		if m == 0 {
			unlimitedMem++
		}
	}
	return cpu, mem, unlimitedCPU, unlimitedMem
}

// pinReason returns why a pod cannot be modelled as movable, or "".
//
// Every branch here is a constraint tare does not evaluate. The list is
// deliberately broad: the cost of pinning a movable pod is a smaller reported
// saving, and the cost of moving a pinned one is a promise the scheduler will
// not keep.
func pinReason(p *corev1.Pod, kind string) string {
	switch kind {
	case "DaemonSet":
		return "" // handled as per-node overhead, never packed
	case "":
		return "not owned by a controller, so nothing would recreate it elsewhere"
	}
	if len(p.Spec.NodeSelector) > 0 {
		return "has a nodeSelector"
	}
	if p.Spec.Affinity != nil {
		return "has affinity or anti-affinity rules"
	}
	if len(p.Spec.TopologySpreadConstraints) > 0 {
		return "has topology spread constraints"
	}
	if hasNonDefaultToleration(p.Spec.Tolerations) {
		return "tolerates a taint, so it may be placed where other pods cannot follow"
	}
	if usesExtendedResources(p) {
		return "requests an extended resource such as a GPU"
	}
	for _, v := range p.Spec.Volumes {
		switch {
		case v.PersistentVolumeClaim != nil:
			return "has a PersistentVolumeClaim, which may bind it to a zone or a node"
		case v.HostPath != nil:
			return "mounts a hostPath"
		case v.Ephemeral != nil:
			return "has an ephemeral volume claim"
		}
	}
	if p.Spec.PriorityClassName != "" {
		return "has a priority class, so eviction ordering is not tare's to predict"
	}
	return ""
}

// hasNonDefaultToleration ignores the two tolerations the kubelet adds to
// every pod. Treating those as a reason to pin would pin the whole cluster.
func hasNonDefaultToleration(ts []corev1.Toleration) bool {
	for _, t := range ts {
		switch t.Key {
		case corev1.TaintNodeNotReady, corev1.TaintNodeUnreachable:
			continue
		}
		return true
	}
	return false
}

func usesExtendedResources(p *corev1.Pod) bool {
	std := map[corev1.ResourceName]bool{
		corev1.ResourceCPU: true, corev1.ResourceMemory: true,
		corev1.ResourceEphemeralStorage: true,
	}
	check := func(rl corev1.ResourceList) bool {
		for name, q := range rl {
			if !std[name] && !q.IsZero() {
				return true
			}
		}
		return false
	}
	for i := range p.Spec.Containers {
		if check(p.Spec.Containers[i].Resources.Requests) || check(p.Spec.Containers[i].Resources.Limits) {
			return true
		}
	}
	for i := range p.Spec.InitContainers {
		if check(p.Spec.InitContainers[i].Resources.Requests) {
			return true
		}
	}
	return false
}
