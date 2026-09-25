package analyze

import (
	"fmt"
	"strings"
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
	switch f.Verdict {
	case Saturated, Waste, Healthy, Idle:
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
	kind, name, ok = strings.Cut(f.Workload, "/")
	return kind, name, ok && kind != "" && name != ""
}
