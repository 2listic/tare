package analyze

import (
	"fmt"
	"strings"

	"github.com/mfranzon/tare/internal/discover"
)

// Patchable reports whether tare will stand behind writing this finding's
// numbers into a manifest, and why not when it will not.
//
// It lives here, beside the verdicts it reasons about, because two different
// outputs now depend on it: the kubectl patch script and the reviewable diff.
// Two copies of this rule would drift, and the direction they would drift is
// towards emitting a number the report disclaimed one screen earlier.
func Patchable(f Finding) (ok bool, why string) {
	kind, name, found := strings.Cut(f.Workload, "/")
	if !found || name == "" {
		return false, "no controller owns this pod, so there is no manifest to change"
	}
	// IDLE is excluded: its own evidence says IDLE_NOT_RULED_OUT and its
	// action is deletion, not the floor-clamped number it carries.
	switch f.Verdict {
	case Saturated, Waste, Healthy:
	default:
		return false, fmt.Sprintf("verdict %s, so tare does not stand behind a number for it", f.Verdict)
	}
	// A Job's spec.template is immutable: the patch is rejected with "field is
	// immutable", and writing it into a repo produces a diff that cannot be
	// applied to the running object either.
	switch kind {
	case "Deployment", "StatefulSet", "DaemonSet", "ReplicaSet", "ReplicationController":
	default:
		return false, fmt.Sprintf("a %s pod template is immutable; the change cannot be applied", kind)
	}
	if f.RecCPU == 0 && f.RecMem == 0 {
		return false, "no recommendation was produced, and a partial change is worse than none"
	}
	return true, ""
}

// WorkloadParts splits a finding's "Kind/name" workload identifier.
func WorkloadParts(f Finding) (kind, name string, ok bool) {
	return discover.SplitWorkload(f.Workload)
}
