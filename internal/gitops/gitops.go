// Package gitops turns findings into a change a human can review in git.
//
// The report tells a platform engineer what is wrong. It does not help them,
// because the manifests belong to other teams: the person who runs tare is
// almost never the person who can merge the fix. A table cannot be reviewed,
// commented on or approved, and `kubectl patch` commands leave no trace in the
// repository that is the source of truth. A diff can be all three.
//
// The known hard problem in this area is mapping a rendered Kubernetes object
// back to the Helm values that produced it, which is why no open source tool
// does it cleanly. This package sidesteps it rather than solving it: plain
// manifests are edited where they sit, and Kustomize takes a strategic-merge
// patch addressed by kind and name, which is exactly what a finding already
// carries. Helm is reported as unsupported rather than guessed at.
package gitops

import (
	"fmt"
	"strings"

	"github.com/2listic/tare/internal/analyze"
)

// Change is one container's resource edit, already filtered to what tare will
// defend. Nil fields are left alone rather than written as zero.
type Change struct {
	Namespace string
	Kind      string
	Name      string
	Container string

	CPURequest *int64 // millicores
	MemRequest *int64 // bytes
	CPULimit   *int64
	MemLimit   *int64

	// VM marks a KubeVirt VirtualMachine. Its fields then address the VM's
	// domain, not a container: CPURequest is domain.resources.requests.cpu,
	// MemRequest domain.resources.requests.memory, GuestMemory
	// domain.memory.guest, and the limits domain.resources.limits: those are
	// written only where the VM authors them and the change would otherwise
	// leave a request above its limit or break a limit pinned to its request.
	VM          bool
	GuestMemory *int64

	// Reasons is the evidence, carried through so it can land in the pull
	// request body. It is the only part of this output a reviewer cannot get
	// from the diff itself, and the reason they might say yes.
	Reasons []string
}

// Skipped records a finding deliberately left out, so the omission is visible
// rather than silent. A diff that quietly covers half the report is worse than
// one that says what it left behind.
type Skipped struct {
	Workload  string
	Container string
	Why       string
}

// Plan converts findings into changes, dropping everything tare will not
// defend and saying why.
func Plan(fs []analyze.Finding) ([]Change, []Skipped) {
	var out []Change
	var skipped []Skipped
	for _, f := range fs {
		if f.IsVM() {
			_, cpu := analyze.VMCPUChange(f)
			_, guest := analyze.VMGuestChange(f)
			if !cpu && !guest {
				continue // a VM with nothing to change is not a skip
			}
		}
		if ok, why := analyze.Patchable(f); !ok {
			// A finding with nothing to say is not a skip worth reporting: the
			// report is full of healthy containers that need no change.
			if f.RecCPU != 0 || f.RecMem != 0 {
				skipped = append(skipped, Skipped{f.Namespace + "/" + f.Workload, f.Container, why})
			}
			continue
		}
		kind, name, _ := analyze.WorkloadParts(f)
		if f.IsVM() {
			out = append(out, vmChange(f, kind, name))
			continue
		}
		c := Change{
			Namespace: f.Namespace, Kind: kind, Name: name, Container: f.Container,
			Reasons: analyze.ReasonTexts(f.Reasons),
		}
		if f.RecCPU > 0 {
			c.CPURequest = ptr(f.RecCPU)
		}
		if f.RecCPULimit > 0 {
			c.CPULimit = ptr(f.RecCPULimit)
		}
		// A censored memory is unknown, not "keep this value": leave the
		// manifest's own fields alone rather than rewrite them.
		if f.RecMem > 0 && !f.MemCensored {
			c.MemRequest = ptr(f.RecMem)
		}
		if f.RecMemLimit > 0 && !f.MemCensored {
			c.MemLimit = ptr(f.RecMemLimit)
		}
		// A Guaranteed pod must keep requests equal to limits; the analyzer has
		// already done that, and this is a guard against a future change to it
		// silently demoting a pod's QoS through the repo instead of the API.
		if f.QoS == "Guaranteed" && (deref(c.CPURequest) != deref(c.CPULimit) || deref(c.MemRequest) != deref(c.MemLimit)) {
			skipped = append(skipped, Skipped{f.Namespace + "/" + f.Workload, f.Container,
				"Guaranteed pod whose recommended requests and limits differ: applying this would demote it to Burstable"})
			continue
		}
		out = append(out, c)
	}
	return out, skipped
}

// vmChange is a VirtualMachine's edit: its CPU request and guest memory.
// When the domain authors a memory request above the new guest size, it is
// lowered with it, or the pod would keep requesting the old size and the
// shrink would save nothing.
func vmChange(f analyze.Finding, kind, name string) Change {
	c := Change{
		Namespace: f.Namespace, Kind: kind, Name: name, Container: f.Container, VM: true,
		Reasons: analyze.ReasonTexts(f.Reasons),
	}
	vm := f.VM
	if cpu, ok := analyze.VMCPUChange(f); ok {
		c.CPURequest = ptr(cpu)
		c.CPULimit = vmLimit(vm.AuthoredCPURequest, vm.AuthoredCPULimit, cpu)
	}
	if g, ok := analyze.VMGuestChange(f); ok {
		c.GuestMemory = ptr(g)
		// The request follows the guest down, so the shrink reaches the
		// pod, and up when it was the guest: left behind, a raise is an
		// overcommit nobody asked for. A request authored below the guest
		// is a deliberate overcommit and stays.
		req := vm.AuthoredMemRequest
		if req > g || (req > 0 && req == vm.GuestMemory) {
			c.MemRequest = ptr(g)
			req = g
		}
		c.MemLimit = vmLimit(vm.AuthoredMemRequest, vm.AuthoredMemLimit, max(req, g))
	}
	return c
}

// vmLimit is the limit a VM change must write, nil when the authored one
// stays valid. A limit equal to its request (Guaranteed) moves with it, or a
// shrink silently makes the VM Burstable; any other limit is raised only when
// the new value would exceed it, which KubeVirt rejects.
func vmLimit(authoredReq, authoredLim, want int64) *int64 {
	switch {
	case authoredLim <= 0:
		return nil
	case authoredLim == authoredReq && want != authoredLim:
		return ptr(want)
	case want > authoredLim:
		return ptr(want)
	}
	return nil
}

// workload is the change's namespace/Kind/name, as a skip names it.
func (c Change) workload() string { return c.Namespace + "/" + c.Kind + "/" + c.Name }

// id identifies one container of one workload.
func (c Change) id() string { return c.workload() + "/" + c.Container }

func ptr(v int64) *int64 { return &v }
func deref(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// CPUQuantity renders millicores the way a manifest spells them. It is the one
// spelling for every output: -o patch and diff --write printed the same value
// as 1000m and 1, which reads as two different recommendations.
func CPUQuantity(m int64) string {
	if m <= 0 {
		return "0"
	}
	if m%1000 == 0 {
		return fmt.Sprintf("%d", m/1000)
	}
	return fmt.Sprintf("%dm", m)
}

// MemQuantity renders bytes as whole mebibytes, or gibibytes when exact. A
// Kubernetes quantity has to round-trip exactly, and a decimal gibibyte
// invites a rounding surprise.
func MemQuantity(b int64) string {
	if b <= 0 {
		return "0"
	}
	mi := b >> 20
	if mi < 1 {
		mi = 1
	}
	if mi%1024 == 0 {
		return fmt.Sprintf("%dGi", mi/1024)
	}
	return fmt.Sprintf("%dMi", mi)
}

// PullRequestBody renders the evidence a reviewer needs to approve the diff.
//
// This is the part that does not exist anywhere else. Any tool can compute a
// smaller number; the reason a change gets merged is that the person reviewing
// it can see why, what it is based on, and what would reverse it.
func PullRequestBody(changes []Change, skipped []Skipped, source, window string) string {
	var b strings.Builder
	b.WriteString("## Resource right-sizing\n\n")
	fmt.Fprintf(&b, "Generated by `tare` from %s over a %s window.\n\n", source, window)
	b.WriteString("Every number below is derived from measured saturation evidence, not from a target utilisation figure. ")
	b.WriteString("Containers tare could not justify changing are listed at the end with the reason.\n")

	for _, c := range changes {
		fmt.Fprintf(&b, "\n### %s/%s %s [%s]\n\n", c.Namespace, c.Kind, c.Name, c.Container)
		b.WriteString("| field | new value |\n| --- | --- |\n")
		if c.VM {
			if c.CPURequest != nil {
				fmt.Fprintf(&b, "| domain.resources.requests.cpu | `%s` |\n", CPUQuantity(*c.CPURequest))
			}
			if c.MemRequest != nil {
				fmt.Fprintf(&b, "| domain.resources.requests.memory | `%s` |\n", MemQuantity(*c.MemRequest))
			}
			if c.GuestMemory != nil {
				fmt.Fprintf(&b, "| domain.memory.guest | `%s` |\n", MemQuantity(*c.GuestMemory))
			}
			if c.CPULimit != nil {
				fmt.Fprintf(&b, "| domain.resources.limits.cpu | `%s` |\n", CPUQuantity(*c.CPULimit))
			}
			if c.MemLimit != nil {
				fmt.Fprintf(&b, "| domain.resources.limits.memory | `%s` |\n", MemQuantity(*c.MemLimit))
			}
		} else {
			if c.CPURequest != nil {
				fmt.Fprintf(&b, "| requests.cpu | `%s` |\n", CPUQuantity(*c.CPURequest))
			}
			if c.CPULimit != nil {
				fmt.Fprintf(&b, "| limits.cpu | `%s` |\n", CPUQuantity(*c.CPULimit))
			}
			if c.MemRequest != nil {
				fmt.Fprintf(&b, "| requests.memory | `%s` |\n", MemQuantity(*c.MemRequest))
			}
			if c.MemLimit != nil {
				fmt.Fprintf(&b, "| limits.memory | `%s` |\n", MemQuantity(*c.MemLimit))
			}
		}
		if len(c.Reasons) > 0 {
			b.WriteString("\nEvidence:\n\n")
			for _, r := range c.Reasons {
				fmt.Fprintf(&b, "- %s\n", r)
			}
		}
	}

	if len(skipped) > 0 {
		b.WriteString("\n### Deliberately not changed\n\n")
		for _, s := range skipped {
			fmt.Fprintf(&b, "- **%s [%s]**: %s\n", s.Workload, s.Container, s.Why)
		}
	}
	return b.String()
}
