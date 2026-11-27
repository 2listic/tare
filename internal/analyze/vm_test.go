package analyze_test

import (
	"strings"
	"testing"
	"time"

	"github.com/2listic/tare/internal/analyze"
	"github.com/2listic/tare/internal/discover"
	"github.com/2listic/tare/internal/metrics"
	"github.com/2listic/tare/internal/simulate"
)

// runVMCatalog runs every VM archetype the way the demo does, over a week.
func runVMCatalog(t *testing.T) map[string]analyze.Finding {
	t.Helper()
	const window = 7 * 24 * time.Hour
	origin := time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)
	c := simulate.NewCluster("vms", origin, 21, simulate.VMCatalog()...)
	for _, d := range simulate.VMCatalog() {
		c.SetAllocation(d.Name, simulate.VMAllocation(d))
	}
	c.Run(window)
	obs, _ := c.Observe(nil, window)
	vms, _ := c.ObserveVMs(nil, window)
	fs := analyze.RunWithCluster(c.Containers(), obs, analyze.Cluster{VMs: vms}, window, analyze.DefaultThresholds())
	out := map[string]analyze.Finding{}
	for _, f := range fs {
		_, name, _ := analyze.WorkloadParts(f)
		out[name] = f
	}
	if len(out) != len(simulate.VMCatalog()) {
		t.Fatalf("want one finding per VM, got %d", len(out))
	}
	return out
}

func TestVMArchetypes(t *testing.T) {
	fs := runVMCatalog(t)
	th := analyze.DefaultThresholds()
	must := func(name string, codes ...analyze.Code) analyze.Finding {
		t.Helper()
		f := fs[name]
		for _, c := range codes {
			if !hasCode(f, c) {
				t.Errorf("%s: missing %s; verdict %s, reasons %v", name, c, f.Verdict, analyze.ReasonTexts(f.Reasons))
			}
		}
		return f
	}
	mustNot := func(name string, codes ...analyze.Code) {
		t.Helper()
		for _, c := range codes {
			if hasCode(fs[name], c) {
				t.Errorf("%s: unexpected %s", name, c)
			}
		}
	}

	// QEMU's RSS quadruples as the page cache fills; that is not a leak,
	// and the guest's own 1.2Gi is what sizes it.
	f := must("idle-linux-vm", analyze.VMRSSTouched, analyze.VMGuestMemOversized, analyze.VMVCPUsUnused, analyze.VMChangeNeedsRestart)
	if f.Verdict == analyze.Leaking || f.Verdict != analyze.Waste {
		t.Errorf("idle-linux-vm: verdict %s, want WASTE", f.Verdict)
	}
	if f.RecGuestMem == 0 || f.RecGuestMem >= 8*gib || f.RecVCPUs == 0 || f.RecVCPUs >= 4 {
		t.Errorf("idle-linux-vm: guest %d vCPUs %d", f.RecGuestMem, f.RecVCPUs)
	}
	if f.RecMem < f.RecGuestMem+f.VMOverhead {
		t.Errorf("idle-linux-vm: pod memory %d below new guest %d + overhead %d", f.RecMem, f.RecGuestMem, f.VMOverhead)
	}
	if f.Confidence == analyze.High {
		t.Errorf("idle-linux-vm: a guest-reported shrink cannot be high confidence")
	}

	// A Windows guest's RSS is its whole guest from boot; nothing says shrink.
	must("windows-vm", analyze.VMRSSTouched)
	mustNot("windows-vm", analyze.VMGuestMemOversized, analyze.VMOverheadExceeded)

	f = must("busy-vm", analyze.VMVCPUDelay)
	if f.Verdict != analyze.Saturated || f.RecCPU < f.CPURequest {
		t.Errorf("busy-vm: verdict %s, CPU %d -> %d", f.Verdict, f.CPURequest, f.RecCPU)
	}

	f = must("load-bound-vm", analyze.VMGuestNeedsVCPUs)
	if f.RecVCPUs <= f.VM.VCPUs {
		t.Errorf("load-bound-vm: vCPU advice %d for %d", f.RecVCPUs, f.VM.VCPUs)
	}
	mustNot("load-bound-vm", analyze.VMVCPUDelay, analyze.VMVCPUsUnused)

	f = must("swapping-vm", analyze.VMGuestMemPressure)
	if f.Verdict != analyze.Saturated || f.RecGuestMem <= f.VM.GuestMemory {
		t.Errorf("swapping-vm: verdict %s, guest %d -> %d", f.Verdict, f.VM.GuestMemory, f.RecGuestMem)
	}
	mustNot("swapping-vm", analyze.VMGuestMemOversized)

	f = must("no-balloon-vm", analyze.VMGuestMemUnreported)
	if f.RecGuestMem != 0 || f.RecMem < f.VM.GuestMemory {
		t.Errorf("no-balloon-vm: guest %d, pod memory %d below the %d guest", f.RecGuestMem, f.RecMem, f.VM.GuestMemory)
	}

	f = must("dedicated-idle-vm", analyze.VMPinnedCoresIdle)
	if f.RecCPU != f.CPURequest || f.RecCPU != f.RecCPULimit {
		t.Errorf("dedicated-idle-vm: CPU %d/%d -> %d/%d, dedicated cores follow the vCPU count",
			f.CPURequest, f.CPULimit, f.RecCPU, f.RecCPULimit)
	}
	if f.RecMem != f.RecMemLimit {
		t.Errorf("dedicated-idle-vm: Guaranteed memory split %d/%d", f.RecMem, f.RecMemLimit)
	}

	f = must("hugepages-vm", analyze.VMHugepages)
	if f.RecGuestMem != 0 {
		t.Errorf("hugepages-vm: guest memory changed to %d", f.RecGuestMem)
	}

	// Migrated half an hour ago: a young pod, an old VM.
	f = fs["migrated-vm"]
	mustNot("migrated-vm", analyze.TooFewSamples)
	if f.Pods != 1 || f.Samples < th.MinSamplesForSizing {
		t.Errorf("migrated-vm: %d pods, %d samples", f.Pods, f.Samples)
	}

	for name, f := range fs {
		if f.Verdict == analyze.Leaking {
			t.Errorf("%s: a VM is never LEAKING from QEMU's RSS", name)
		}
		if hasCode(f, analyze.NoSaturationSignal) {
			t.Errorf("%s: vCPU delay is a saturation signal", name)
		}
	}
}

// vmContainer is a VM's compute container with every pod-level signal.
func vmContainer(pod string) discover.Container {
	return discover.Container{
		Namespace: "vms", Pod: pod, Name: "compute", Workload: "VirtualMachine/db", WorkloadKind: "VirtualMachine",
		CPURequest: 400, MemRequest: 8*gib + 250<<20, QoS: "Burstable",
		VM: &discover.VMSpec{VM: "db", VMI: "db", VCPUs: 4, GuestMemory: 8 * gib,
			LiveMigratable: true, LiveMigratableKnown: true, EvictionStrategy: "LiveMigrate",
			CPUAllocationRatio: 10, MemoryOvercommit: 100},
	}
}

func vmObs(c discover.Container, sigs ...metrics.Signal) *metrics.Observation {
	o := &metrics.Observation{
		Key:         metrics.Key{Namespace: c.Namespace, Pod: c.Pod, Container: c.Name},
		CPUUsageP95: 0.3, CPUUsageMax: 0.5, MemWorkingSet: 2 * gib, MemP95: 2 * gib, Samples: 20000,
	}
	for _, s := range append([]metrics.Signal{metrics.SigCPUUsage, metrics.SigCPUPeak, metrics.SigMemory, metrics.SigOOM}, sigs...) {
		o.Mark(s)
	}
	return o
}

func guestObs(mod func(o *metrics.VMObservation)) map[metrics.VMKey]*metrics.VMObservation {
	o := &metrics.VMObservation{
		VMKey:        metrics.VMKey{Namespace: "vms", Name: "db"},
		VCPUUsageP95: 0.25, VCPUUsageMax: 0.4, OverheadCPUP95: 0.03, VCPUDelayAvg: 0.001,
		MemResident: 2 * gib, MemDomain: 8 * gib, MemOverhead: 250 << 20,
		GuestMemUsedMax: 1 * gib, GuestMemCoverage: 1, Samples: 20000,
	}
	for _, s := range []metrics.Signal{metrics.SigVMVCPU, metrics.SigVMOverheadCPU, metrics.SigVMVCPUDelay,
		metrics.SigVMMemResident, metrics.SigVMMemDomain, metrics.SigVMMemOverhead, metrics.SigVMGuestMem, metrics.SigVMGuestPressure} {
		o.Mark(s)
	}
	if mod != nil {
		mod(o)
	}
	return map[metrics.VMKey]*metrics.VMObservation{o.VMKey: o}
}

func judgeVM(t *testing.T, cs []discover.Container, obs []*metrics.Observation, vms map[metrics.VMKey]*metrics.VMObservation) analyze.Finding {
	t.Helper()
	m := map[metrics.Key]*metrics.Observation{}
	for _, o := range obs {
		m[o.Key] = o
	}
	fs := analyze.RunWithCluster(cs, m, analyze.Cluster{VMs: vms}, 7*24*time.Hour, analyze.DefaultThresholds())
	if len(fs) != 1 {
		t.Fatalf("want one finding, got %d", len(fs))
	}
	return fs[0]
}

// Mid-migration a VMI has two virt-launcher pods. They are one guest, not two
// replicas: counting two would double the VM in every total.
func TestMigratingVMIsOneVM(t *testing.T) {
	src, dst := vmContainer("virt-launcher-db-aaaaa"), vmContainer("virt-launcher-db-bbbbb")
	f := judgeVM(t, []discover.Container{src, dst}, []*metrics.Observation{vmObs(src), vmObs(dst)}, guestObs(nil))
	if f.Pods != 1 {
		t.Errorf("a migrating VM counted as %d pods", f.Pods)
	}
}

// A pod gone with an earlier migration is attributed by kube-state-metrics to
// its VMI; its history is the VM's.
func TestPodsReplacedByMigrationAreCreditedToTheVM(t *testing.T) {
	live := vmContainer("virt-launcher-db-bbbbb")
	lo := vmObs(live)
	lo.Samples = 100
	gone := vmContainer("virt-launcher-db-aaaaa")
	g := vmObs(gone)
	g.Owner = "VirtualMachineInstance/db"
	g.CPUUsageMax = 3
	f := judgeVM(t, []discover.Container{live}, []*metrics.Observation{lo, g}, nil)
	if f.CPUUsageMax != 3 || f.Samples < 20000 {
		t.Errorf("the replaced pod's history was dropped: peak %.1f, samples %d", f.CPUUsageMax, f.Samples)
	}
}

// QEMU's RSS is what the guest touched. A pod memory request sized from it
// would undercut the memory the guest was promised.
func TestVMPodMemoryNeverBelowTheGuest(t *testing.T) {
	c := vmContainer("virt-launcher-db-aaaaa")
	o := vmObs(c, metrics.SigPSI, metrics.SigMemP95)
	o.MemWorkingSet, o.MemP95 = 600<<20, 600<<20
	f := judgeVM(t, []discover.Container{c}, []*metrics.Observation{o}, nil)
	if f.RecMem < 8*gib {
		t.Errorf("pod memory %s below the 8Gi guest", analyze.HumanBytes(f.RecMem))
	}
	if !hasCode(f, analyze.VMGuestMemUnreported) || f.RecGuestMem != 0 {
		t.Errorf("without guest stats the guest is not changed: %v", analyze.ReasonTexts(f.Reasons))
	}
}

// A VM whose only saturation signal is vCPU delay is not blind.
func TestVCPUDelayIsASaturationSignal(t *testing.T) {
	c := vmContainer("virt-launcher-db-aaaaa")
	f := judgeVM(t, []discover.Container{c}, []*metrics.Observation{vmObs(c)}, guestObs(nil))
	if hasCode(f, analyze.NoSaturationSignal) || f.Verdict == analyze.Unverified {
		t.Errorf("verdict %s, reasons %v", f.Verdict, analyze.ReasonTexts(f.Reasons))
	}
	f = judgeVM(t, []discover.Container{c}, []*metrics.Observation{vmObs(c)}, nil)
	if !hasCode(f, analyze.NoSaturationSignal) {
		t.Errorf("without virt-handler or PSI the VM is blind: %v", analyze.ReasonTexts(f.Reasons))
	}
}

// vCPU delay blocks a CPU reduction even where the VM is far below its
// allocation: the host is short of CPU, and fewer shares make it worse.
func TestVCPUDelayBlocksACPUShrink(t *testing.T) {
	c := vmContainer("virt-launcher-db-aaaaa")
	c.CPURequest = 2000
	f := judgeVM(t, []discover.Container{c}, []*metrics.Observation{vmObs(c)},
		guestObs(func(o *metrics.VMObservation) { o.VCPUDelayAvg = 0.2 }))
	if f.RecCPU < c.CPURequest || !hasCode(f, analyze.VMVCPUDelay) {
		t.Errorf("CPU %d -> %d under vCPU delay: %v", c.CPURequest, f.RecCPU, analyze.ReasonTexts(f.Reasons))
	}
}

// Guest memory comes down only on a full window of the guest's word and no
// sign of pressure; each missing piece keeps it where it is.
func TestGuestMemoryNeedsTheWholeStory(t *testing.T) {
	c := vmContainer("virt-launcher-db-aaaaa")
	for _, tc := range []struct {
		name string
		mod  func(o *metrics.VMObservation)
		code analyze.Code
		down bool
	}{
		{"reported, calm", nil, analyze.VMGuestMemOversized, true},
		{"no pressure signal", func(o *metrics.VMObservation) { delete(o.Have, metrics.SigVMGuestPressure) }, analyze.VMGuestMemUnreported, false},
		{"partial window", func(o *metrics.VMObservation) { o.GuestMemCoverage = 0.3 }, analyze.VMGuestMemUnreported, false},
		{"a swap burst", func(o *metrics.VMObservation) { o.GuestSwapMax = 1 << 20 }, analyze.VMGuestMemPressure, false},
		{"hugepages", nil, analyze.VMHugepages, false},
	} {
		cc := c
		vm := *c.VM
		if tc.name == "hugepages" {
			vm.HugepageSize = "1Gi"
		}
		cc.VM = &vm
		f := judgeVM(t, []discover.Container{cc}, []*metrics.Observation{vmObs(cc, metrics.SigPSI)}, guestObs(tc.mod))
		if !hasCode(f, tc.code) {
			t.Errorf("%s: missing %s: %v", tc.name, tc.code, analyze.ReasonTexts(f.Reasons))
		}
		if down := f.RecGuestMem > 0 && f.RecGuestMem < 8*gib; down != tc.down {
			t.Errorf("%s: guest -> %d", tc.name, f.RecGuestMem)
		}
		if tc.name == "a swap burst" && f.Verdict == analyze.Saturated {
			t.Errorf("a burst is not sustained pressure")
		}
	}
}

// Under LiveUpdate a guest memory raise within maxGuest reaches the running
// VM; a reduction never does, since memory cannot be unplugged.
func TestVMChangeRolloutNotes(t *testing.T) {
	c := vmContainer("virt-launcher-db-aaaaa")
	vm := *c.VM
	vm.RolloutStrategy, vm.MaxGuest = "LiveUpdate", 32*gib
	c.VM = &vm
	c.CPURequest = 2000 // no CPU change below: usage sizes to it
	o := vmObs(c, metrics.SigPSI)
	o.CPUUsageP95, o.CPUUsageMax = 1.74, 1.8

	sustained := func(o *metrics.VMObservation) { o.GuestSwapMax, o.GuestSwapAvg = 1<<20, 1<<20 }
	f := judgeVM(t, []discover.Container{c}, []*metrics.Observation{o}, guestObs(sustained))
	if !hasCode(f, analyze.VMLiveUpdate) || hasCode(f, analyze.VMChangeNeedsRestart) {
		t.Errorf("raise under LiveUpdate: %v", analyze.ReasonTexts(f.Reasons))
	}
	f = judgeVM(t, []discover.Container{c}, []*metrics.Observation{o}, guestObs(nil))
	if !hasCode(f, analyze.VMChangeNeedsRestart) || hasCode(f, analyze.VMLiveUpdate) {
		t.Errorf("shrink under LiveUpdate: %v", analyze.ReasonTexts(f.Reasons))
	}
}

func TestVMOOMPointsAtTheOverhead(t *testing.T) {
	c := vmContainer("virt-launcher-db-aaaaa")
	c.LastTermError = "OOMKilled"
	f := judgeVM(t, []discover.Container{c}, []*metrics.Observation{vmObs(c, metrics.SigPSI)}, guestObs(nil))
	if !hasCode(f, analyze.VMOOMOverhead) || f.Verdict != analyze.Saturated {
		t.Errorf("verdict %s: %v", f.Verdict, analyze.ReasonTexts(f.Reasons))
	}
}

func TestVMPatchable(t *testing.T) {
	c := vmContainer("virt-launcher-db-aaaaa")
	f := judgeVM(t, []discover.Container{c}, []*metrics.Observation{vmObs(c, metrics.SigPSI)}, guestObs(nil))
	if ok, why := analyze.Patchable(f); !ok {
		t.Fatalf("an oversized VM is patchable: %s", why)
	}
	it := f
	vm := *f.VM
	vm.Instancetype = "u1.large"
	it.VM = &vm
	if ok, _ := analyze.Patchable(it); ok {
		t.Errorf("a VM sized by an instancetype is not patched")
	}
	vmi := f
	vmi.Workload = "VirtualMachineInstance/db"
	if ok, _ := analyze.Patchable(vmi); ok {
		t.Errorf("a bare VMI is not patched")
	}
}

func reasonText(f analyze.Finding, c analyze.Code) string {
	for _, r := range f.Reasons {
		if r.Code == c {
			return r.Text
		}
	}
	return ""
}

// On the lab cluster (2026-11-24) a lowered guest, with a CPU change too, said
// "memory cannot be hot-unplugged", which KubeVirt v1.9 can do for hotplugged
// memory, and left out the CPU request, which alone forces the restart. A
// raised guest said only "a CPU request change is not applied live".
func TestVMChangeNotesNameEveryCause(t *testing.T) {
	c := vmContainer("virt-launcher-db-aaaaa")
	vm := *c.VM
	vm.RolloutStrategy, vm.MaxGuest = "LiveUpdate", 32*gib
	c.VM = &vm

	// 8Gi guest using 1Gi -> 1280Mi; CPU 400m -> less.
	f := judgeVM(t, []discover.Container{c}, []*metrics.Observation{vmObs(c, metrics.SigPSI)}, guestObs(nil))
	txt := reasonText(f, analyze.VMChangeNeedsRestart)
	for _, want := range []string{"guest memory 1280Mi", "CPU request change only at a restart", "booted with is removed only at a restart"} {
		if !strings.Contains(txt, want) {
			t.Errorf("shrink and CPU: no %q in %q", want, txt)
		}
	}
	if strings.Contains(txt, "hot-unplugged") {
		t.Errorf("shrink: %q", txt)
	}

	// A raise within maxGuest with a CPU change: the raise waits for the CPU.
	o := vmObs(c, metrics.SigPSI)
	o.CPUUsageP95, o.CPUUsageMax = 1.74, 1.8
	sustained := func(o *metrics.VMObservation) { o.GuestSwapMax, o.GuestSwapAvg = 1<<20, 1<<20 }
	f = judgeVM(t, []discover.Container{c}, []*metrics.Observation{o}, guestObs(sustained))
	txt = reasonText(f, analyze.VMChangeNeedsRestart)
	if !strings.Contains(txt, "guest memory 10Gi") || !strings.Contains(txt, "waits for it") {
		t.Errorf("raise and CPU: %q", txt)
	}
}

// A VMI nobody owns was told its change "takes effect only when the VM
// restarts" beside the skip that says it cannot be changed at all.
func TestBareVMIHasNoRestartNote(t *testing.T) {
	c := vmContainer("virt-launcher-db-aaaaa")
	c.Workload, c.WorkloadKind = "VirtualMachineInstance/db", "VirtualMachineInstance"
	vm := *c.VM
	vm.VM = ""
	c.VM = &vm
	f := judgeVM(t, []discover.Container{c}, []*metrics.Observation{vmObs(c, metrics.SigPSI)}, guestObs(nil))
	if hasCode(f, analyze.VMChangeNeedsRestart) || hasCode(f, analyze.VMLiveUpdate) {
		t.Errorf("a bare VMI: %v", analyze.ReasonTexts(f.Reasons))
	}
	if ok, why := analyze.Patchable(f); ok || !strings.Contains(why, "nobody owns") {
		t.Errorf("a bare VMI patchable %v: %s", ok, why)
	}
}

// MEM_RAISED_BY_CEILING named the current guest's ceiling (1.3Gi) on a pod
// whose request follows a raised guest (1548Mi), and said "raised to 1.6Gi" on
// a pod going from 1.6Gi to 1.3Gi with its guest lowered.
func TestCeilingRestatedForTheNewGuest(t *testing.T) {
	c := vmContainer("virt-launcher-db-aaaaa")
	o := vmObs(c, metrics.SigPSI)
	o.MemWorkingSet, o.MemP95 = 1*gib, 1*gib // usage sizes below the new guest's pod
	f := judgeVM(t, []discover.Container{c}, []*metrics.Observation{o}, guestObs(nil))
	if f.RecGuestMem != 1280<<20 {
		t.Fatalf("guest -> %d", f.RecGuestMem)
	}
	txt := reasonText(f, analyze.MemRaised)
	if !strings.Contains(txt, analyze.HumanBytes(f.RecMem)) || !strings.Contains(txt, "new 1280Mi guest") {
		t.Errorf("RecMem %s: %q", analyze.HumanBytes(f.RecMem), txt)
	}
	if strings.Contains(txt, analyze.HumanBytes(8*gib+250<<20)) {
		t.Errorf("the old ceiling: %q", txt)
	}
}

// A swapping guest had load 1.7 on 1 vCPU and was told that raising the CPU
// request "will not help" on the row that raised it.
func TestNeedsVCPUsSaysWhatTheRequestIsFor(t *testing.T) {
	c := vmContainer("virt-launcher-db-aaaaa")
	mod := func(o *metrics.VMObservation) {
		o.GuestLoadP95 = 5.5
		o.Mark(metrics.SigVMGuestLoad)
		o.GuestSwapMax, o.GuestSwapAvg = 1<<20, 1<<20
	}
	f := judgeVM(t, []discover.Container{c}, []*metrics.Observation{vmObs(c, metrics.SigPSI)}, guestObs(mod))
	txt := reasonText(f, analyze.VMGuestNeedsVCPUs)
	if strings.Contains(txt, "will not help") || !strings.Contains(txt, "reserves host CPU") ||
		!strings.Contains(txt, "also swapping") {
		t.Errorf("%q", txt)
	}
}

// A guest memory marked reported with no used value is not a guest using
// nothing: it is shrunk to the floor only on a number, never on a zero.
func TestGuestMemZeroUsedIsNotEvidence(t *testing.T) {
	c := vmContainer("virt-launcher-db-aaaaa")
	f := judgeVM(t, []discover.Container{c}, []*metrics.Observation{vmObs(c, metrics.SigPSI)},
		guestObs(func(o *metrics.VMObservation) { o.GuestMemUsedMax = 0 }))
	if f.RecGuestMem != 0 || hasCode(f, analyze.VMGuestMemOversized) {
		t.Errorf("guest shrunk to %s on a zero used: %v", analyze.HumanBytes(f.RecGuestMem), analyze.ReasonTexts(f.Reasons))
	}
	if !hasCode(f, analyze.VMGuestMemUnreported) {
		t.Errorf("missing %s: %v", analyze.VMGuestMemUnreported, analyze.ReasonTexts(f.Reasons))
	}
}

// A virt-launcher pod carries KubeVirt's own containers beside compute
// (guest-console-log, container disks, sidecar hooks). They belong to the VM
// but are not in its spec: the skip says what they are, not that the VM
// could not be read.
func TestVMSidecarSkipSaysWhatItIs(t *testing.T) {
	for _, wl := range []string{"VirtualMachine/db", "VirtualMachineInstance/db"} {
		f := analyze.Finding{Namespace: "vms", Workload: wl, Container: "guest-console-log",
			Verdict: analyze.Waste, CPURequest: 100, RecCPU: 10}
		ok, why := analyze.Patchable(f)
		if ok || strings.Contains(why, "could not be read") || strings.Contains(why, "nobody owns") ||
			!strings.Contains(why, "guest-console-log") || !strings.Contains(why, "KubeVirt") {
			t.Errorf("%s sidecar: ok=%v, why %q", wl, ok, why)
		}
	}
}

// A pool or a VMI ReplicaSet rewrites what it manages from its own template:
// a change to the VM is reverted, and the refusal says so.
func TestVMUnderAControllerIsNotPatched(t *testing.T) {
	c := vmContainer("virt-launcher-db-aaaaa")
	f := judgeVM(t, []discover.Container{c}, []*metrics.Observation{vmObs(c, metrics.SigPSI)}, guestObs(nil))
	for _, tc := range []struct{ workload, managedBy string }{
		{"VirtualMachine/db", "VirtualMachinePool/db-pool"},
		{"VirtualMachineInstance/db", "VirtualMachineInstanceReplicaSet/db-rs"},
	} {
		g := f
		vm := *f.VM
		vm.ManagedBy = tc.managedBy
		g.VM, g.Workload = &vm, tc.workload
		ok, why := analyze.Patchable(g)
		if ok || !strings.Contains(why, tc.managedBy) || !strings.Contains(why, "revert") {
			t.Errorf("%s under %s: ok=%v, why %q", tc.workload, tc.managedBy, ok, why)
		}
	}
	// The restart note would tell the reader the change lands on restart.
	cc := c
	vm := *c.VM
	vm.ManagedBy = "VirtualMachinePool/db-pool"
	cc.VM = &vm
	g := judgeVM(t, []discover.Container{cc}, []*metrics.Observation{vmObs(cc, metrics.SigPSI)}, guestObs(nil))
	for _, r := range g.Reasons {
		if strings.Contains(r.Text, "restart") {
			t.Errorf("a pool-managed VM got a restart note: %s", r.Text)
		}
	}
}
