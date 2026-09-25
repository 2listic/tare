package discover

import (
	corev1 "k8s.io/api/core/v1"
)

// Authored records which resource fields the workload's own manifest actually
// set, as opposed to which ones the API server filled in.
//
// This distinction is invisible in a running pod and it matters more than
// almost anything else in a sizing report. Kubernetes copies a limit into the
// request when the request is omitted, so a manifest that says
//
//	resources:
//	  limits: {cpu: "4", memory: 8Gi}
//
// does not cap a burst at four cores. It permanently reserves four cores and
// 8Gi on a node, and makes the pod Guaranteed. The author usually believes
// they asked for nothing. Verified against a live API server: the Deployment's
// pod template keeps the omission, and the pod it creates does not.
type Authored struct {
	// Known is false when the controller's pod template could not be read, in
	// which case every other field here is meaningless and must not be used.
	// A workload with no controller (a bare pod) has no authored template at
	// all, and a pod defaulted by a LimitRange looks identical to one whose
	// author wrote the values by hand.
	Known bool

	CPURequest bool
	MemRequest bool
	CPULimit   bool
	MemLimit   bool
}

// authoredFromSpec reads a controller's pod template into the per-container
// record of what its author actually wrote.
func authoredFromSpec(spec *corev1.PodSpec) map[string]Authored {
	out := map[string]Authored{}
	all := append(append([]corev1.Container{}, spec.Containers...), spec.InitContainers...)
	for i := range all {
		r := all[i].Resources
		out[all[i].Name] = Authored{
			Known:      true,
			CPURequest: has(r.Requests, corev1.ResourceCPU),
			MemRequest: has(r.Requests, corev1.ResourceMemory),
			CPULimit:   has(r.Limits, corev1.ResourceCPU),
			MemLimit:   has(r.Limits, corev1.ResourceMemory),
		}
	}
	return out
}

func has(rl corev1.ResourceList, name corev1.ResourceName) bool {
	_, ok := rl[name]
	return ok
}
