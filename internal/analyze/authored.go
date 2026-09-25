package analyze

import (
	"fmt"

	"github.com/mfranzon/tare/internal/discover"
)

// The gap between what a manifest says and what Kubernetes did with it.
//
// This is the only finding in tare that cannot be derived from a running pod
// at all. Kubernetes copies an omitted request from the limit, so by the time
// a pod exists the two are indistinguishable from values someone chose. The
// controller's pod template still holds the omission, and the difference
// between the two is usually a surprise to the person who wrote it.

// applyAuthored adds the findings that compare the authored manifest with the
// admitted pod. A workload whose template could not be read gets nothing.
func applyAuthored(f *Finding, a discover.Authored) {
	if !a.Known {
		return
	}

	// A limit with no request: Kubernetes reserves the whole limit. The author
	// wrote a cap and bought a reservation.
	if f.CPULimit > 0 && !a.CPURequest && a.CPULimit {
		f.Reasons = append(f.Reasons, measured(CPULimitCopied, float64(f.CPULimit), 0, Millicores, fmt.Sprintf(
			"the manifest sets a CPU limit of %dm and no CPU request, so Kubernetes copied the limit into the request: this workload RESERVES %dm on a node, it does not merely cap at it. "+
				"Each replica holds that reservation whether or not it uses any of it, and the pod's QoS class was decided by the copy rather than by anything the author wrote. "+
				"Setting an explicit request is usually a pure saving with no behavioural change.",
			f.CPULimit, f.CPULimit)))
	}
	// Reported separately from CPU: the two are independent omissions and a
	// manifest often makes only one of them.
	if f.MemLimit > 0 && !a.MemRequest && a.MemLimit {
		f.Reasons = append(f.Reasons, measured(MemLimitCopied, float64(f.MemLimit), 0, Bytes, fmt.Sprintf(
			"the manifest sets a memory limit of %s and no memory request, so Kubernetes copied it into the request: %s is reserved per replica rather than capped.",
			humanBytes(f.MemLimit), humanBytes(f.MemLimit))))
	}

	// Proposing a limit where the manifest set none is a behavioural change,
	// not a resize, and it is the direction nobody is watching: the reader is
	// looking at a report about reclaiming waste.
	//
	// A CPU limit throttles at quota even when the node is idle, which is why
	// omitting it is the common recommendation and what most charts ship. A
	// memory limit is worse in kind: it makes the container OOMKillable at that
	// cap rather than evicted under node pressure, so the failure changes from
	// "rescheduled elsewhere" to "killed in place".
	//
	// The numbers are kept, because they are useful and a reader who wants
	// limits should have them. What is added is that applying them changes how
	// the container fails.
	if a.CPURequest && !a.CPULimit && f.RecCPULimit > 0 {
		f.Reasons = append(f.Reasons, measured(CPULimitAdded, float64(f.RecCPULimit), 0, Millicores, fmt.Sprintf(
			"the manifest sets no CPU limit, so this recommendation would ADD one at %dm: that introduces CPU throttling at quota even when the node is idle, which the workload does not have today. Apply the request on its own if the omission was deliberate.",
			f.RecCPULimit)))
	}
	if a.MemRequest && !a.MemLimit && f.RecMemLimit > 0 {
		f.Reasons = append(f.Reasons, measured(MemLimitAdded, float64(f.RecMemLimit), 0, Bytes, fmt.Sprintf(
			"the manifest sets no memory limit, so this recommendation would ADD one at %s: the container becomes OOMKillable at that cap rather than evicted under node pressure. That is usually what you want, but it changes how this workload fails.",
			humanBytes(f.RecMemLimit))))
	}

	// Nothing authored at all is BestEffort: first evicted under node pressure,
	// and invisible to the scheduler when it decides what fits.
	if !a.CPURequest && !a.MemRequest && !a.CPULimit && !a.MemLimit {
		f.Reasons = append(f.Reasons, note(BestEffort, "the manifest sets no requests and no limits, so this pod is BestEffort: the scheduler treats it as free, it is the first thing evicted when a node runs short, and it can be OOMKilled while the node still has memory it was never promised. "+
			"tare's recommendation here is a starting point rather than a reduction, because there is nothing to reduce."))
	}
}

// AuthoredQoS explains how the QoS class was arrived at when the author did not
// choose it directly. A Guaranteed pod that nobody meant to create is a
// recommendation hazard: its requests may not be split from its limits without
// changing its class, which the constraint handling already refuses to do.
func authoredQoSNote(f *Finding, a discover.Authored) {
	if !a.Known || f.QoS != "Guaranteed" {
		return
	}
	if a.CPURequest && a.MemRequest {
		return // deliberately Guaranteed
	}
	f.Reasons = append(f.Reasons, note(QoSAccidental, "this pod is Guaranteed by accident rather than by choice: its requests were copied from its limits because the manifest omitted them. "+
		"That matters for what tare may recommend - requests must stay equal to limits to preserve the class - so the reduction available here is larger than the report shows, and taking it means deciding whether Guaranteed was ever wanted."))
}
