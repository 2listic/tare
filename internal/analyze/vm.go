package analyze

import (
	"fmt"
	"math"
	"strings"

	"github.com/2listic/tare/internal/ceilings"
	"github.com/2listic/tare/internal/metrics"
)

// A KubeVirt VM's compute container is QEMU, and the numbers cAdvisor reports
// for it mean something different from an application's:
//
//   - its working set is QEMU's resident memory, every page the guest has ever
//     touched. A Linux guest's page cache fills it, a Windows guest touches all
//     of it at boot, and it shrinks only through the balloon. It is an upper
//     bound on what the guest uses, never a measurement of it.
//   - its CPU is guest and hypervisor together.
//   - its requests are not anyone's manifest: KubeVirt derives them from the
//     VirtualMachine (CPU = vCPUs / cpuAllocationRatio unless the domain sets
//     one; memory = guest + overhead), so the knobs are VM fields.
//
// So a VM's pod memory is never sized below what its guest was promised, and
// the guest memory itself is only lowered on the guest's own word, reported
// for the whole window with no sign of pressure. The guest's word is evidence
// for a shrink and never the only safety check: the host-side signals can
// block a change the guest says is fine, never the other way round.

// VMGuestMemory is the ceiling kind that stands for the guest's promised
// memory. Like a JVM heap, it is memory the container must hold whatever its
// usage says.
const VMGuestMemory ceilings.Kind = "vm-guest-memory"

// attachVM joins virt-handler's view to a VM's finding and adds the ceiling
// that keeps its pod memory at or above what the guest was promised.
func attachVM(f *Finding, o *metrics.VMObservation) {
	f.VMObs = o
	if o != nil {
		for sig, ok := range o.Have {
			if !ok {
				continue
			}
			if f.Have == nil {
				f.Have = map[metrics.Signal]bool{}
			}
			f.Have[sig] = true
		}
		// The VMI's own age, across every pod it has had: a live migration
		// replaces the pod, not the VM's history.
		f.Samples = max(f.Samples, o.Samples)
	}
	guest := vmGuest(f)
	if guest <= 0 {
		return
	}
	share := vmGuestShare(f, guest)
	f.VMOverhead = max(f.MemRequest-share, 0)
	src := "derived from the pod request"
	if o.Has(metrics.SigVMMemOverhead) && o.MemOverhead > 0 {
		f.VMOverhead, src = int64(o.MemOverhead), "KubeVirt's estimate"
	}
	note := fmt.Sprintf("the guest was promised %s and virt-launcher needs about %s beside it (%s), so the compute "+
		"container is never sized below %s. QEMU's resident memory is the pages the guest has touched, not what it uses.",
		guestSize(guest), HumanBytes(f.VMOverhead), src, HumanBytes(share+f.VMOverhead))
	if f.VM.HugepageSize != "" {
		note = fmt.Sprintf("the guest's %s is backed by %s hugepages, reserved apart from the pod's memory request; "+
			"the compute container holds virt-launcher's overhead of about %s (%s).",
			guestSize(guest), f.VM.HugepageSize, HumanBytes(f.VMOverhead), src)
	}
	f.Ceilings = append(f.Ceilings, ceilings.Ceiling{
		Kind: VMGuestMemory, Resource: ceilings.Memory, Source: "VirtualMachine",
		Bytes: guest, MinContainerBytes: share + f.VMOverhead, Note: note,
	})
}

// vmGuest is the guest memory size: the spec's, or the hypervisor's report of
// it when the spec could not be read.
func vmGuest(f *Finding) int64 {
	if f.VM != nil && f.VM.GuestMemory > 0 {
		return f.VM.GuestMemory
	}
	if f.VMObs.Has(metrics.SigVMMemDomain) {
		return int64(f.VMObs.MemDomain)
	}
	return 0
}

// vmGuestShare is the part of the pod's memory request that a guest of this
// size accounts for. Hugepages are requested separately and count nothing
// here. Under overcommit, KubeVirt sets the domain's memory request below the
// guest, and the share scales with it.
func vmGuestShare(f *Finding, guest int64) int64 {
	vm := f.VM
	if vm == nil || vm.HugepageSize != "" {
		return 0
	}
	if cur := vm.GuestMemory; vm.MemRequest > 0 && cur > 0 && vm.MemRequest < cur {
		return int64(float64(guest) * float64(vm.MemRequest) / float64(cur))
	}
	return vm.GuestRequestShare(guest)
}

// decideVM runs after decide on a VM's compute container. It explains the
// CPU in guest terms, advises on the vCPU count, decides the guest memory,
// and puts the pod's memory where KubeVirt will derive it.
func decideVM(f *Finding, th Thresholds) {
	// decide withholds the numbers of a finding it could not size; adding a
	// guest memory to one would hand a patch writer a number nothing backs.
	if f.VM == nil || f.Verdict == NoData || f.Verdict == Leaking || f.Verdict == Blocked {
		return
	}
	o := f.VMObs
	if o.Has(metrics.SigVMVCPU) && o.Has(metrics.SigVMOverheadCPU) {
		f.Reasons = append(f.Reasons, measured(VMCPUSplit, o.VCPUUsageP95*1000, o.OverheadCPUP95*1000, Millicores, fmt.Sprintf(
			"of the compute container's CPU, the guest's %d vCPUs ran %.0fm at p95 and QEMU/libvirt overhead took %.0fm",
			f.VM.VCPUs, o.VCPUUsageP95*1000, o.OverheadCPUP95*1000)))
	}
	if f.VM.Windows() {
		f.Reasons = append(f.Reasons, note(VMRSSTouched, "a Windows guest zeroes all of its memory at boot, so QEMU's "+
			"resident memory equals the guest size from then on and says nothing about what the guest uses"))
	}

	// vCPU delay blocks a CPU reduction even where the VM is not using its
	// allocation: the guest is short of host CPU, and a smaller request
	// gives it fewer CFS shares on a node that already cannot serve it.
	if f.vcpuDelayStalled(th) && f.RecCPU > 0 && f.RecCPU < f.CPURequest {
		f.RecCPU = f.CPURequest
		f.RecCPULimit = max(f.RecCPULimit, f.CPULimit, f.RecCPU)
		if !f.Has(VMVCPUDelay) {
			f.Reasons = append(f.Reasons, measured(VMVCPUDelay, o.VCPUDelayAvg, th.PSIWaiting, SecPerSec, fmt.Sprintf(
				"the VM's vCPUs waited %.3f s/s for a host CPU on average against a %.3f threshold, so its CPU request is not reduced",
				o.VCPUDelayAvg, th.PSIWaiting)))
		}
	}

	if f.VM.DedicatedCPU {
		// Dedicated CPUs are whole cores sized by the vCPU count; the pod
		// request follows it and there is nothing else to change.
		f.RecCPU, f.RecCPULimit = f.CPURequest, f.CPULimit
		if o.Has(metrics.SigVMVCPU) && f.VM.VCPUs > 0 && o.VCPUUsageP95 < float64(f.VM.VCPUs)*th.VMPinnedIdleShare {
			f.Reasons = append(f.Reasons, measured(VMPinnedCoresIdle, o.VCPUUsageP95, float64(f.VM.VCPUs), CoreCount, fmt.Sprintf(
				"its %d dedicated cores ran vCPU work %.2f cores at p95. Low confidence: halt-polling, hint-dedicated "+
					"or cpu-pm=on keep a vCPU thread on its core while the guest is idle, which makes thread time unreliable "+
					"in both directions; check the guest's own CPU use before giving cores back",
				f.VM.VCPUs, o.VCPUUsageP95)))
		}
	} else {
		adviseVCPUs(f, th)
	}

	decideGuestMemory(f, th)
	placeVMMemory(f)

	if f.OOMKilled {
		f.Reasons = append(f.Reasons, note(VMOOMOverhead, "a VM's compute container is OOMKilled when QEMU and "+
			"virt-launcher outgrow KubeVirt's overhead estimate, not when the guest uses its memory: the guest cannot "+
			"exceed what it was given. Raise the overhead with additionalGuestMemoryOverheadRatio in the KubeVirt CR "+
			"rather than the guest memory"))
	}
}

// adviseVCPUs compares guest vCPU use and guest load with the vCPU count. It
// is advice only: the count is what the guest sees, and changing it is a
// decision about the guest, not about the node.
func adviseVCPUs(f *Finding, th Thresholds) {
	o, n := f.VMObs, f.VM.VCPUs
	if n <= 0 || !o.Has(metrics.SigVMVCPU) {
		return
	}
	hasLoad := o.Has(metrics.SigVMGuestLoad)
	delayLow := !o.Has(metrics.SigVMVCPUDelay) || o.VCPUDelayAvg <= th.PSIWaiting
	switch {
	case hasLoad && o.GuestLoadP95 > float64(n) && delayLow:
		f.RecVCPUs = int64(math.Ceil(o.GuestLoadP95))
		text := fmt.Sprintf("the guest's 5-minute load reached %.1f at p95 against %d vCPUs while its vCPUs rarely "+
			"waited for the host: it is short of vCPUs, not of host CPU. More vCPUs would help it (advice only: tare "+
			"never changes the vCPU count); the pod's CPU request would not, because it reserves host CPU for the vCPUs "+
			"the guest already has, and any change tare makes to it sizes that reservation, not the guest's CPU "+
			"(guest-reported; a Linux load average also counts tasks waiting on I/O)", o.GuestLoadP95, n)
		if o.Has(metrics.SigVMGuestPressure) && (o.GuestSwapMax > th.VMGuestSwapRate || o.GuestMajFaultMax > th.VMGuestMajFaults) {
			text += ". The guest is also swapping, and a task waiting for a swap-in counts in the load: part of this " +
				"load is waiting for memory, not for a CPU. Give it the memory first, then look at the vCPUs again"
		}
		f.Reasons = append(f.Reasons, measured(VMGuestNeedsVCPUs, o.GuestLoadP95, float64(n), CoreCount, text))
	case n > 1 && o.VCPUUsageP95 < float64(n-1) && (!hasLoad || o.GuestLoadP95 < float64(n-1)):
		want := max(int64(math.Ceil(o.VCPUUsageP95*(1+th.Margin))), 1)
		if hasLoad {
			want = max(want, int64(math.Ceil(o.GuestLoadP95)))
		}
		if want >= n {
			return
		}
		f.RecVCPUs = want
		conf := "the guest's own load agrees"
		if !hasLoad {
			conf = "Low confidence: without guest load (qemu-guest-agent) a guest whose threads wait on locks or I/O looks the same"
		}
		f.Reasons = append(f.Reasons, measured(VMVCPUsUnused, o.VCPUUsageP95, float64(n), CoreCount, fmt.Sprintf(
			"the guest ran %.2f vCPUs at p95 of the %d it has; %d would cover it. %s. Advice only: tare never changes the vCPU count",
			o.VCPUUsageP95, n, want, conf)))
	}
}

// decideGuestMemory decides domain.memory.guest. It is lowered only on the
// guest's own report, present for the whole window, with no swapping or
// major-fault storm inside the guest; a guest under pressure is offered more.
func decideGuestMemory(f *Finding, th Thresholds) {
	guest := vmGuest(f)
	if guest <= 0 || f.MemCensored {
		return
	}
	o := f.VMObs
	if f.VM.HugepageSize != "" {
		f.Reasons = append(f.Reasons, note(VMHugepages, fmt.Sprintf(
			"the guest's %s is backed by %s hugepages, which the node reserves whole whatever the guest uses; "+
				"guest memory is not changed", guestSize(guest), f.VM.HugepageSize)))
		return
	}
	if !o.Has(metrics.SigVMGuestMem) {
		f.Reasons = append(f.Reasons, measured(VMGuestMemUnreported, 0, float64(guest), Bytes, fmt.Sprintf(
			"the guest does not report its memory (no balloon driver statistics), and QEMU's resident memory is what it "+
				"touched, not what it uses: the %s guest memory is not changed", guestSize(guest))))
		return
	}
	used := o.GuestMemUsedMax
	if o.Has(metrics.SigVMGuestPressure) && (o.GuestSwapMax > th.VMGuestSwapRate || o.GuestMajFaultMax > th.VMGuestMajFaults) {
		sustained := o.GuestSwapAvg > th.VMGuestSwapRate || o.GuestMajFaultAvg > th.VMGuestMajFaults
		text := fmt.Sprintf("the guest swapped up to %s/s and took up to %.0f major faults/s (thresholds %s/s, %.0f/s) "+
			"with %s used of %s: it is short of memory, so guest memory is not reduced",
			HumanBytes(int64(o.GuestSwapMax)), o.GuestMajFaultMax, HumanBytes(int64(th.VMGuestSwapRate)), th.VMGuestMajFaults,
			HumanBytes(int64(used)), guestSize(guest))
		if sustained {
			f.RecGuestMem = roundGuest(int64(float64(guest) * (1 + th.MemLimitHeadroom)))
			text += fmt.Sprintf(". It did so on average across the window, not in a burst: %s is suggested", guestSize(f.RecGuestMem))
			switch f.Verdict {
			case Leaking, Blocked, Conflicted, Saturated:
			default:
				f.Verdict = Saturated
			}
		}
		f.Reasons = append(f.Reasons, measured(VMGuestMemPressure, o.GuestSwapMax, th.VMGuestSwapRate, Bytes, text))
		return
	}
	switch {
	case !o.Has(metrics.SigVMGuestPressure):
		f.Reasons = append(f.Reasons, measured(VMGuestMemUnreported, used, float64(guest), Bytes, fmt.Sprintf(
			"the guest reports %s used of %s but not its swap or major faults, so a reduction cannot be shown not to "+
				"push it into swap: guest memory is not changed", HumanBytes(int64(used)), guestSize(guest))))
		return
	case o.GuestMemCoverage < th.VMGuestMinCoverage:
		f.Reasons = append(f.Reasons, measured(VMGuestMemUnreported, o.GuestMemCoverage, th.VMGuestMinCoverage, Ratio, fmt.Sprintf(
			"the guest reported its memory for %.0f%% of the window (needs %.0f%%), and its silence for the rest is not "+
				"evidence: guest memory is not changed", o.GuestMemCoverage*100, th.VMGuestMinCoverage*100)))
		return
	}
	rec := roundGuest(max(int64(used*(1+th.Margin)), th.VMMinGuestMem))
	if float64(rec) >= float64(guest)*th.VMGuestShrinkRatio {
		return
	}
	f.RecGuestMem = rec
	balloon := ""
	if o.Has(metrics.SigVMBalloon) && o.BalloonMin > 0 && o.BalloonMin < float64(guest) {
		balloon = fmt.Sprintf(" The balloon held the guest at %s at its smallest, and it still fit.", HumanBytes(int64(o.BalloonMin)))
	}
	f.Reasons = append(f.Reasons, measured(VMGuestMemOversized, used, float64(guest), Bytes, fmt.Sprintf(
		"the guest itself reports at most %s in use (page cache excluded) of the %s it was given, across the whole window "+
			"and without swapping: %s would cover it.%s Guest-reported, through the balloon driver, so it is medium "+
			"confidence at best; the pod's memory request follows the guest's",
		HumanBytes(int64(used)), guestSize(guest), guestSize(rec), balloon)))
	if f.Confidence == High {
		f.Confidence = Medium
	}
	// The pod's own numbers looked fine because they are QEMU's; the waste
	// is in the guest.
	if f.Verdict == Healthy {
		f.Verdict = Waste
	}
}

// guestSize spells a guest memory the way the patch writes it, whole Gi or
// Mi: one decimal of Gi turned 1280Mi into "1.2Gi" in a sentence beside a
// patch that writes 1280Mi.
func guestSize(b int64) string {
	switch {
	case b > 0 && b%(1<<30) == 0:
		return fmt.Sprintf("%dGi", b>>30)
	case b > 0 && b%(1<<20) == 0:
		return fmt.Sprintf("%dMi", b>>20)
	}
	return HumanBytes(b)
}

// roundGuest rounds a guest size up to 128Mi, the granularity people write.
func roundGuest(b int64) int64 {
	const step = 128 << 20
	return (b + step - 1) / step * step
}

// placeVMMemory sets the pod's memory to what KubeVirt will derive. It is not
// a knob: the guest memory is, and the request follows it.
func placeVMMemory(f *Finding) {
	if f.MemCensored || f.RecMem == 0 {
		return
	}
	guest := vmGuest(f)
	if guest <= 0 {
		return
	}
	target := guest
	if f.RecGuestMem > 0 {
		target = f.RecGuestMem
	}
	derived := vmGuestShare(f, target) + f.VMOverhead
	// Above the guest and its overhead, QEMU is using more than KubeVirt
	// accounted for: a request cut to the derived value would be too small,
	// and the cause is the overhead estimate, not the guest.
	// The estimate is not byte-exact, so a peak within a few percent of it is
	// not evidence of anything.
	const slack = 1.05
	if peak := int64(f.MemUsageMax); f.RecGuestMem == 0 && float64(peak) > float64(derived)*slack {
		f.Reasons = append(f.Reasons, measured(VMOverheadExceeded, f.MemUsageMax, float64(derived), Bytes, fmt.Sprintf(
			"the compute container peaked at %s, above the %s of guest and estimated overhead: QEMU, virt-launcher "+
				"or its I/O threads use more than KubeVirt accounted for. Raise additionalGuestMemoryOverheadRatio "+
				"in the KubeVirt CR; the guest memory is not the lever", HumanBytes(peak), HumanBytes(derived))))
		derived = peak
	}
	f.RecMem = max(derived, 1)
	if f.RecGuestMem > 0 && f.RecGuestMem != guest {
		restateGuestCeiling(f, target)
	}
	// A VM's pod limits are KubeVirt's to set; tare does not propose one. A
	// Guaranteed VM's limit is its request, and moves with it.
	f.RecMemLimit = f.MemLimit
	if f.MemLimit > 0 && f.MemLimit == f.MemRequest {
		f.RecMemLimit = f.RecMem
	}
	f.RecCPULimit = 0
	if f.CPULimit > 0 {
		f.RecCPULimit = max(f.CPULimit, f.RecCPU)
	}
}

// restateGuestCeiling rewrites MEM_RAISED_BY_CEILING for a VM whose guest
// changes. decide raised the usage-sized request to the current guest's
// ceiling, and said so; the request now follows the new guest, and the old
// sentence would name a number the patch does not write ("raised to 1.6Gi" on
// a pod that goes from 1.6Gi to 1.3Gi).
func restateGuestCeiling(f *Finding, guest int64) {
	out := f.Reasons[:0]
	for _, r := range f.Reasons {
		if r.Code == MemRaised {
			if float64(f.RecMem) <= r.Observed {
				continue // usage alone sizes it at least as high: nothing was raised
			}
			r = measured(MemRaised, r.Observed, float64(f.RecMem), Bytes, fmt.Sprintf(
				"memory recommendation raised from %s to %s, the new %s guest plus %s of virt-launcher overhead: "+
					"the pod must hold the whole guest, whatever QEMU's resident memory says",
				HumanBytes(int64(r.Observed)), HumanBytes(f.RecMem), guestSize(guest), HumanBytes(f.VMOverhead)))
		}
		out = append(out, r)
	}
	f.Reasons = out
}

// VMCPUChange is the domain.resources.requests.cpu a finding proposes, if it
// proposes one. Dedicated CPUs follow the vCPU count and are never changed.
func VMCPUChange(f Finding) (int64, bool) {
	if f.VM == nil || f.VM.DedicatedCPU || f.RecCPU <= 0 || f.RecCPU == f.CPURequest {
		return 0, false
	}
	return f.RecCPU, true
}

// VMGuestChange is the domain.memory.guest a finding proposes, if any.
func VMGuestChange(f Finding) (int64, bool) {
	if f.VM == nil || f.RecGuestMem <= 0 || f.RecGuestMem == vmGuest(&f) {
		return 0, false
	}
	return f.RecGuestMem, true
}

// noteVMChange says how a VM change reaches the running guest. Nothing about
// a VirtualMachine's spec reaches its pod until KubeVirt builds a new one, so
// a change that looks applied is not, until a restart or a live update.
func noteVMChange(f *Finding) {
	// A VMI nobody owns has no spec a restart would apply: deleted, it is
	// gone. Patchable says so; a restart note beside it would contradict it.
	if f.VM == nil || strings.HasPrefix(f.Workload, "VirtualMachineInstance/") {
		return
	}
	switch f.Verdict {
	case Saturated, Waste, Healthy:
	default:
		return
	}
	cpu, cpuOK := VMCPUChange(*f)
	guest, guestOK := VMGuestChange(*f)
	if !cpuOK && !guestOK {
		return
	}
	cur := vmGuest(f)
	maxGuest := f.VM.MaxGuest
	if maxGuest == 0 {
		// KubeVirt's default hotplug ceiling under LiveUpdate.
		maxGuest = cur * 4
	}
	if f.VM.RolloutStrategy == "LiveUpdate" && !cpuOK && guestOK && guest > cur && guest <= maxGuest {
		f.Reasons = append(f.Reasons, measured(VMLiveUpdate, float64(guest), float64(maxGuest), Bytes, fmt.Sprintf(
			"the cluster rolls VM changes out live and %s is within the %s maxGuest, so KubeVirt hotplugs the memory "+
				"and live-migrates the VM to a pod of the new size, without a restart", guestSize(guest), guestSize(maxGuest))))
		return
	}
	what := []string{}
	if cpuOK {
		what = append(what, fmt.Sprintf("CPU request %s", milliString(cpu)))
	}
	if guestOK {
		what = append(what, "guest memory "+guestSize(guest))
	}
	// Every reason that applies, not the first: a lowered guest whose CPU
	// request also changes needs the restart for the CPU whatever happens to
	// the memory, and a raise that LiveUpdate could add on its own waits too.
	why := "the cluster's vmRolloutStrategy is not LiveUpdate"
	if f.VM.RolloutStrategy == "LiveUpdate" {
		whys := []string{}
		if cpuOK {
			w := "KubeVirt applies a CPU request change only at a restart"
			if guestOK && guest > cur && guest <= maxGuest {
				w += ", and the memory raise, which it would add live on its own, waits for it"
			}
			whys = append(whys, w)
		}
		if guestOK && guest < cur {
			// KubeVirt v1.9 removed hotplugged memory from a running guest on
			// the lab cluster; what it booted with is the domain's base
			// memory, which only a new domain changes.
			whys = append(whys, "memory the guest booted with is removed only at a restart (live, KubeVirt removes only memory hotplugged after boot)")
		}
		if guestOK && guest > maxGuest {
			whys = append(whys, fmt.Sprintf("%s is above the %s maxGuest that memory can be hotplugged up to",
				guestSize(guest), guestSize(maxGuest)))
		}
		if len(whys) > 0 {
			why = strings.Join(whys, "; ")
		}
	}
	f.Reasons = append(f.Reasons, note(VMChangeNeedsRestart, fmt.Sprintf(
		"the VirtualMachine change (%s) takes effect only when the VM restarts: %s. Until then the running pod "+
			"keeps its current size", joinAnd(what), why)))
}

func milliString(m int64) string { return fmt.Sprintf("%dm", m) }

func joinAnd(parts []string) string {
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0]
	}
	return parts[0] + " and " + parts[1]
}
