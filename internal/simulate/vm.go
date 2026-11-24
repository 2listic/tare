package simulate

import (
	"fmt"
	"time"

	"github.com/2listic/tare/internal/ceilings"
	"github.com/2listic/tare/internal/discover"
	"github.com/2listic/tare/internal/metrics"
)

// VMDemand is what a KubeVirt guest does, beside what QEMU's container shows.
// The two disagree on purpose: that disagreement is what a right-sizer has to
// get right for a VM.
type VMDemand struct {
	VCPUs int64
	Guest int64 // memory the guest was promised
	// GuestUsed is what the guest itself reports in use, page cache excluded.
	GuestUsed int64
	// NoGuestStats models a guest without the balloon driver: no guest-side
	// memory report at all.
	NoGuestStats bool
	Windows      bool
	// VCPUDelay is the vCPUs' host run-queue wait, s/s summed over vCPUs.
	VCPUDelay float64
	// GuestLoad is the guest's own 5-minute load at p95; zero means no
	// qemu-guest-agent and no load reported.
	GuestLoad float64
	// GuestSwap is swap traffic inside the guest, bytes per second.
	GuestSwap float64
	Dedicated bool
	Hugepages bool
	// MigratedBefore is how long before the end of the window the VM
	// live-migrated to a new pod; zero means it never did.
	MigratedBefore time.Duration
}

// vmOverhead is virt-launcher's memory beside the guest, as KubeVirt
// estimates it for a small VM.
const vmOverhead = 250 * mib

const vmGiB = 1024 * mib

// The VM archetypes. Each is a way a VM's pod numbers mislead a sizer built
// for applications.
var (
	// IdleLinuxVM's QEMU resident memory climbs towards its 8Gi guest as the
	// page cache fills, while the guest itself uses 1.2Gi: RSS growth that is
	// not a leak, beside a guest that is genuinely oversized.
	IdleLinuxVM = Demand{
		Name: "idle-linux-vm", BaseCPU: 0.15, NoiseFraction: 0.1,
		BaseMem: 2 * vmGiB, MemGrowthPerHour: 30 * mib, MemCeiling: 8*vmGiB + vmOverhead,
		VM: &VMDemand{VCPUs: 4, Guest: 8 * vmGiB, GuestUsed: 1200 * mib, VCPUDelay: 0.002},
	}
	// WindowsVM zeroes its memory at boot, so RSS equals the guest from the
	// first minute, and uses most of it.
	WindowsVM = Demand{
		Name: "windows-vm", BaseCPU: 0.4, NoiseFraction: 0.1,
		BaseMem: 4*vmGiB + 180*mib, MemNoiseFraction: 0.004,
		VM: &VMDemand{VCPUs: 2, Guest: 4 * vmGiB, GuestUsed: 3300 * mib, Windows: true, VCPUDelay: 0.003},
	}
	// BusyVM runs hot on a contended node: its vCPUs wait for host CPU.
	BusyVM = Demand{
		Name: "busy-vm", BaseCPU: 1.6, NoiseFraction: 0.08,
		BaseMem: 3 * vmGiB, MemNoiseFraction: 0.02,
		VM: &VMDemand{VCPUs: 2, Guest: 4 * vmGiB, GuestUsed: 2 * vmGiB, VCPUDelay: 0.12, GuestLoad: 2.2},
	}
	// LoadBoundVM's guest queues more work than it has vCPUs while the host
	// serves every vCPU promptly: short of vCPUs, not of host CPU.
	LoadBoundVM = Demand{
		Name: "load-bound-vm", BaseCPU: 1.9, NoiseFraction: 0.04,
		BaseMem: 3 * vmGiB, MemNoiseFraction: 0.02,
		VM: &VMDemand{VCPUs: 2, Guest: 4 * vmGiB, GuestUsed: 2500 * mib, VCPUDelay: 0.004, GuestLoad: 4.5},
	}
	// SwappingVM's guest is short of memory and swaps all the time.
	SwappingVM = Demand{
		Name: "swapping-vm", BaseCPU: 0.5, NoiseFraction: 0.1,
		BaseMem: 4*vmGiB + vmOverhead, MemNoiseFraction: 0.01,
		VM: &VMDemand{VCPUs: 2, Guest: 4 * vmGiB, GuestUsed: 3900 * mib, GuestSwap: 2 << 20, VCPUDelay: 0.003},
	}
	// NoBalloonVM reports nothing from inside: its guest memory cannot be
	// judged, however idle QEMU looks.
	NoBalloonVM = Demand{
		Name: "no-balloon-vm", BaseCPU: 0.1, NoiseFraction: 0.1,
		BaseMem: 3 * vmGiB, MemNoiseFraction: 0.02,
		VM: &VMDemand{VCPUs: 2, Guest: 8 * vmGiB, NoGuestStats: true, VCPUDelay: 0.002},
	}
	// DedicatedIdleVM holds four whole cores and barely runs on them.
	DedicatedIdleVM = Demand{
		Name: "dedicated-idle-vm", BaseCPU: 0.25, NoiseFraction: 0.1, Shape: Guaranteed,
		BaseMem: 3 * vmGiB, MemNoiseFraction: 0.02,
		VM: &VMDemand{VCPUs: 4, Guest: 4 * vmGiB, GuestUsed: 1500 * mib, Dedicated: true, VCPUDelay: 0.001},
	}
	// HugepagesVM's guest memory is reserved on the node as hugepages.
	HugepagesVM = Demand{
		Name: "hugepages-vm", BaseCPU: 0.3, NoiseFraction: 0.1,
		BaseMem: 200 * mib, MemNoiseFraction: 0.02,
		VM: &VMDemand{VCPUs: 2, Guest: 8 * vmGiB, GuestUsed: 2 * vmGiB, Hugepages: true, VCPUDelay: 0.002},
	}
	// MigratedVM live-migrated half an hour before the window closed: its pod
	// is young and its VM is not.
	MigratedVM = Demand{
		Name: "migrated-vm", BaseCPU: 0.3, NoiseFraction: 0.1,
		BaseMem: 3 * vmGiB, MemNoiseFraction: 0.02,
		VM: &VMDemand{VCPUs: 2, Guest: 4 * vmGiB, GuestUsed: 2600 * mib, VCPUDelay: 0.002, MigratedBefore: 30 * time.Minute},
	}
)

// VMCatalog is every VM archetype. It is kept apart from Catalog: the sweeps
// over Catalog assert application verdicts, and a VM's are decided by
// different rules.
func VMCatalog() []Demand {
	return []Demand{IdleLinuxVM, WindowsVM, BusyVM, LoadBoundVM, SwappingVM,
		NoBalloonVM, DedicatedIdleVM, HugepagesVM, MigratedVM}
}

// VMAllocation is what KubeVirt derives for a VM's compute container under
// the defaults: CPU = vCPUs / 10, memory = guest + overhead, no limits; or
// whole cores and Guaranteed for dedicated CPUs.
func VMAllocation(d Demand) Allocation {
	v := d.VM
	mem := v.Guest + vmOverhead
	if v.Hugepages {
		mem = vmOverhead
	}
	if v.Dedicated {
		return Allocation{CPURequest: float64(v.VCPUs), CPULimit: float64(v.VCPUs), MemRequest: mem, MemLimit: mem}
	}
	return Allocation{CPURequest: float64(v.VCPUs) / 10, MemRequest: mem}
}

// vmPod is the live virt-launcher pod's name.
func vmPod(name string) string { return "virt-launcher-" + name + "-x7k2p" }

// renderVM is a VM's compute container as discover would report it.
func (c *Cluster) renderVM(name string, e *Env) discover.Container {
	d := e.Demand
	qos := "Burstable"
	if d.VM.Dedicated {
		qos = "Guaranteed"
	}
	dc := c.render(vmPod(name), discover.VMComputeContainer, "VirtualMachine/"+name, e.Alloc, qos, Burstable)
	dc.WorkloadKind = "VirtualMachine"
	dc.Image = "quay.io/kubevirt/virt-launcher:v1.4.0"
	dc.Spec = ceilings.Container{Name: dc.Name, Image: dc.Image, Env: map[string]string{}}
	dc.Authored = discover.Authored{}
	v := d.VM
	spec := &discover.VMSpec{
		VM: name, VMI: name, VCPUs: v.VCPUs, GuestMemory: v.Guest,
		AuthoredKnown: true, AuthoredGuest: true,
		DedicatedCPU: v.Dedicated, EvictionStrategy: "LiveMigrate",
		LiveMigratable: !v.Hugepages && !v.Dedicated, LiveMigratableKnown: true,
		CPUAllocationRatio: 10, MemoryOvercommit: 100,
	}
	if v.Hugepages {
		spec.HugepageSize = "1Gi"
	}
	if v.Windows {
		spec.GuestOS = "mswindows"
	}
	dc.VM = spec
	return dc
}

// observeVM is cAdvisor's view of a VM's pod or pods. A VM that migrated has
// two: the source, gone, which kube-state-metrics credits to its VMI, and the
// target, alive since the migration.
func (e *Env) observeVM(ns, name string, window time.Duration) map[metrics.Key]*metrics.Observation {
	live := metrics.Key{Namespace: ns, Pod: vmPod(name), Container: discover.VMComputeContainer}
	out := map[metrics.Key]*metrics.Observation{}
	at := e.Demand.VM.MigratedBefore
	if at <= 0 {
		out[live] = e.observe(live, window)
		return out
	}
	moved := e.now.Add(-at)
	src := metrics.Key{Namespace: ns, Pod: "virt-launcher-" + name + "-src01", Container: discover.VMComputeContainer}
	so := e.observeRange(src, e.now.Add(-window), moved, window)
	so.Owner = "VirtualMachineInstance/" + name
	delete(so.Have, metrics.SigMemTrend)
	out[src] = so
	out[live] = e.observeRange(live, moved, e.now, window)
	delete(out[live].Have, metrics.SigMemTrend)
	return out
}

// ObserveVMs is virt-handler's view of each VM, the same shape metrics.Prom
// returns. It derives the host-side series from the container's own samples
// and the guest-reported ones from VMDemand.
func (c *Cluster) ObserveVMs(namespaces []string, window time.Duration) (map[metrics.VMKey]*metrics.VMObservation, error) {
	out := map[metrics.VMKey]*metrics.VMObservation{}
	for name, e := range c.Envs {
		v := e.Demand.VM
		if v == nil {
			continue
		}
		k := metrics.VMKey{Namespace: c.Namespace, Name: name}
		whole := e.observe(metrics.Key{Namespace: c.Namespace, Pod: name, Container: discover.VMComputeContainer}, window)
		o := &metrics.VMObservation{VMKey: k, Samples: whole.Samples}
		// QEMU and its threads take a slice of the container's CPU; the
		// guest's vCPUs the rest.
		const overheadShare = 0.08
		o.VCPUUsageP95 = whole.CPUUsageP95 * (1 - overheadShare)
		o.VCPUUsageMax = whole.CPUUsageMax * (1 - overheadShare)
		o.OverheadCPUP95 = whole.CPUUsageP95 * overheadShare
		o.VCPUDelayAvg, o.VCPUDelayMax = v.VCPUDelay, v.VCPUDelay*2
		o.MemResident = max(whole.MemWorkingSet-float64(vmOverhead), 0)
		o.MemDomain = float64(v.Guest)
		o.MemOverhead = float64(vmOverhead)
		for _, s := range []metrics.Signal{metrics.SigVMVCPU, metrics.SigVMOverheadCPU, metrics.SigVMVCPUDelay,
			metrics.SigVMMemResident, metrics.SigVMMemDomain, metrics.SigVMMemOverhead, metrics.SigVMNonEvict} {
			o.Mark(s)
		}
		if !v.NoGuestStats {
			o.GuestMemUsedMax, o.GuestMemCoverage = float64(v.GuestUsed), 1
			o.BalloonMin = float64(v.Guest)
			o.GuestSwapAvg, o.GuestSwapMax = v.GuestSwap, v.GuestSwap*1.5
			o.GuestMajFaultAvg, o.GuestMajFaultMax = 2, 6
			if v.GuestSwap > 0 {
				o.GuestMajFaultAvg, o.GuestMajFaultMax = 200, 400
			}
			o.Mark(metrics.SigVMGuestMem)
			o.Mark(metrics.SigVMBalloon)
			o.Mark(metrics.SigVMGuestPressure)
		}
		if v.GuestLoad > 0 {
			o.GuestLoadP95 = v.GuestLoad
			o.Mark(metrics.SigVMGuestLoad)
		}
		if v.MigratedBefore > 0 {
			o.LastMigrationStart = float64(e.now.Add(-v.MigratedBefore).Unix())
			o.LastMigrationEnd = o.LastMigrationStart + 20
			o.Mark(metrics.SigVMMigrations)
		}
		out[k] = o
	}
	return out, nil
}

// String names a VM archetype for test output.
func (v VMDemand) String() string {
	return fmt.Sprintf("%d vCPU, %dMi guest", v.VCPUs, v.Guest/mib)
}
