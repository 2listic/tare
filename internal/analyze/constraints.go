package analyze

import (
	"fmt"

	"github.com/2listic/tare/internal/discover"
)

// applyConstraints adjusts a recommendation to respect facts about the
// workload that have nothing to do with its resource usage. A number that is
// correct in isolation and breaks something when applied is not a useful
// recommendation.
func applyConstraints(f *Finding, c discover.Constraints) {
	c.Guaranteed = f.QoS == "Guaranteed"
	f.Constraints = c

	// Another controller is already setting these fields. Two controllers
	// writing the same field is a fight, not a merge, and the loser is
	// whichever wrote last.
	if c.Managed() {
		f.Reasons = append(f.Reasons, about(VPAManaged, c.VPAName, fmt.Sprintf(
			"VerticalPodAutoscaler %q already manages this workload in %s mode. "+
				"Applying these values would put two controllers in conflict; "+
				"set the VPA to updateMode: Off first, or exclude this workload.",
			c.VPAName, c.VPAMode)))
		f.RecCPU, f.RecCPULimit, f.RecMem, f.RecMemLimit = 0, 0, 0, 0
		if f.Verdict != Saturated && f.Verdict != Leaking {
			f.Verdict = Conflicted
		}
		return
	}

	if f.RecMem == 0 && f.RecCPU == 0 {
		return
	}

	// An HPA scaling on utilisation measures against the request, so the
	// request is the setpoint. Halving it doubles measured utilisation and the
	// workload scales out: a memory saving paid for with replicas.
	if c.HPAOnCPU && f.RecCPU > 0 && f.CPURequest > 0 && f.RecCPU != f.CPURequest {
		ratio := float64(f.CPURequest) / float64(f.RecCPU)
		f.Reasons = append(f.Reasons, measuredAbout(HPAOnCPUCode, c.HPAName, float64(f.RecCPU), float64(f.CPURequest), Millicores, fmt.Sprintf(
			"HorizontalPodAutoscaler %q scales this workload on CPU utilisation, which is "+
				"measured against the request. Changing %dm to %dm multiplies observed "+
				"utilisation by %.1fx, so the HPA will scale out unless its target is "+
				"raised in the same change.",
			c.HPAName, f.CPURequest, f.RecCPU, ratio)))
	}
	if c.HPAOnMemory && f.RecMem > 0 && f.MemRequest > 0 && f.RecMem != f.MemRequest {
		ratio := float64(f.MemRequest) / float64(f.RecMem)
		f.Reasons = append(f.Reasons, measuredAbout(HPAOnMemCode, c.HPAName, float64(f.RecMem), float64(f.MemRequest), Bytes, fmt.Sprintf(
			"HorizontalPodAutoscaler %q scales on memory utilisation; changing the request "+
				"multiplies observed utilisation by %.1fx and will trigger scale-out.",
			c.HPAName, ratio)))
	}
}

// holdGuaranteed keeps a Guaranteed pod's requests equal to its limits.
// Recommending different values silently demotes the pod to Burstable, costing
// it eviction priority, and on clusters with in-place resize the change is
// rejected outright because QoS may not change.
//
// It runs straight after decide, before anything reads the numbers. Run last,
// it raised the memory request after the node-size check had passed the lower
// value, so a request no node could hold shipped without UNSCHEDULABLE, and the
// HPA and authored-limit notes quoted values that were no longer the
// recommendation. QoS comes from the pod, not from an HPA or VPA lookup, so it
// is derived here rather than in the cluster scan.
func holdGuaranteed(f *Finding, c discover.Constraints) {
	if f.QoS != "Guaranteed" || c.Managed() || (f.RecMem == 0 && f.RecCPU == 0) {
		return
	}
	f.RecCPU = max(f.RecCPU, f.RecCPULimit)
	f.RecCPULimit = f.RecCPU
	f.RecMem = max(f.RecMem, f.RecMemLimit)
	f.RecMemLimit = f.RecMem
	f.Reasons = append(f.Reasons, note(QoSHeld, fmt.Sprintf(
		"this pod is Guaranteed, so requests are held equal to limits (%dm CPU, %s memory). "+
			"Splitting them would demote it to Burstable and lose eviction priority.",
		f.RecCPU, HumanBytes(f.RecMem))))
}
