package analyze

import (
	"fmt"
	"strings"

	"github.com/2listic/tare/internal/discover"
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
	// A virt-launcher pod carries KubeVirt's own containers beside compute:
	// the console logger, container disks, sidecar hooks. KubeVirt sizes
	// them; the VM spec has no field for any of them.
	if (kind == "VirtualMachine" || kind == "VirtualMachineInstance") && f.Container != discover.VMComputeContainer {
		return false, fmt.Sprintf("%s is a container KubeVirt adds to the VM's virt-launcher pod and sizes itself; "+
			"the VM spec has no field for it", f.Container)
	}
	// A Job's spec.template is immutable: the patch is rejected with "field is
	// immutable", and writing it into a repo produces a diff that cannot be
	// applied to the running object either.
	switch kind {
	case "Deployment", "StatefulSet", "DaemonSet", "ReplicaSet", "ReplicationController":
	case "VirtualMachine":
		return vmPatchable(f)
	case "VirtualMachineInstance":
		return false, "a VirtualMachineInstance nobody owns cannot be resized in place; only a VirtualMachine's spec can be changed"
	default:
		return false, fmt.Sprintf("a %s pod template is immutable; the change cannot be applied", kind)
	}
	if f.RecCPU == 0 && f.RecMem == 0 {
		return false, "no recommendation was produced, and a partial change is worse than none"
	}
	return true, ""
}

// vmPatchable is Patchable for a VirtualMachine, whose knobs are
// domain.resources.requests.cpu and domain.memory.guest: the pod's own
// resources are derived from them and are nobody's to write.
func vmPatchable(f Finding) (bool, string) {
	if f.VM == nil {
		return false, "the VirtualMachine behind this pod could not be read, so there is no VM spec to change"
	}
	if f.VM.Instancetype != "" {
		return false, fmt.Sprintf("its CPU and memory come from instancetype %s, and KubeVirt rejects a VM that sets them beside one: change the instancetype", f.VM.Instancetype)
	}
	_, cpu := VMCPUChange(f)
	_, guest := VMGuestChange(f)
	if !cpu && !guest {
		return false, "no change to the VirtualMachine's CPU request or guest memory; its pod resources are derived from them"
	}
	return true, ""
}

// WorkloadParts splits a finding's "Kind/name" workload identifier.
func WorkloadParts(f Finding) (kind, name string, ok bool) {
	return discover.SplitWorkload(f.Workload)
}
