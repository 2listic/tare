package metrics

import (
	"fmt"
	"strings"
	"time"
)

// KubeVirt runs each virtual machine as a virt-launcher pod whose `compute`
// container holds QEMU, libvirt and virt-launcher. cAdvisor sees that
// container like any other, and what it sees is misleading for a VM: its
// working set is every page the guest has ever touched, not what the guest
// uses now, and its CPU is guest and hypervisor together.
//
// virt-handler exports what cAdvisor cannot: the guest's own CPU time, how long
// its vCPUs waited for a host CPU, and - through the balloon driver and
// qemu-guest-agent - the guest's view of its memory. Those series are keyed by
// the VirtualMachineInstance (labels namespace, name), not by pod, so they
// survive a live migration that replaces the pod underneath them.
//
// Two classes are kept apart, because they carry different trust:
//
//   - host-side signals come from the hypervisor and are present whenever
//     virt-handler is scraped. They may block a change or allow one.
//   - guest-reported signals come from inside the guest, through a driver or
//     agent that may be missing, stale or simply lying. They can be evidence
//     for a shrink; they are never the only safety check.

const (
	// Host-side.
	SigVMVCPU        Signal = "vm-vcpu-usage"
	SigVMOverheadCPU Signal = "vm-overhead-cpu"
	SigVMVCPUDelay   Signal = "vm-vcpu-delay"
	SigVMMemResident Signal = "vm-mem-resident"
	SigVMMemDomain   Signal = "vm-mem-domain"
	SigVMMemOverhead Signal = "vm-mem-overhead"
	SigVMNonEvict    Signal = "vm-non-evictable"
	SigVMMigrations  Signal = "vm-migrations"

	// Guest-reported.
	SigVMGuestMem      Signal = "vm-guest-mem-used"
	SigVMBalloon       Signal = "vm-balloon"
	SigVMGuestPressure Signal = "vm-guest-mem-pressure"
	SigVMGuestLoad     Signal = "vm-guest-load"
)

// GuestReported reports whether a signal comes from inside the guest rather
// than from the hypervisor. A guest-reported signal may support a reduction
// but must never be the only thing standing between a VM and a smaller size.
func GuestReported(s Signal) bool {
	switch s {
	case SigVMGuestMem, SigVMBalloon, SigVMGuestPressure, SigVMGuestLoad:
		return true
	}
	return false
}

// VMSignals is every KubeVirt signal, host-side first.
var VMSignals = []Signal{
	SigVMVCPU, SigVMOverheadCPU, SigVMVCPUDelay, SigVMMemResident, SigVMMemDomain,
	SigVMMemOverhead, SigVMNonEvict, SigVMMigrations,
	SigVMGuestMem, SigVMBalloon, SigVMGuestPressure, SigVMGuestLoad,
}

// VMKey identifies one VirtualMachineInstance.
type VMKey struct {
	Namespace string
	Name      string
}

// VMObservation is what virt-handler reports about one VMI over the window.
// Check Has before trusting any field that could legitimately be zero.
type VMObservation struct {
	VMKey

	// VCPUUsageP95 and VCPUUsageMax are guest CPU time summed over vCPUs, in
	// cores: the running state only, so a halted or blocked vCPU counts zero.
	VCPUUsageP95 float64
	VCPUUsageMax float64
	// OverheadCPUP95 is QEMU, libvirt and I/O threads: total VMI CPU minus
	// vCPU running time, in cores.
	OverheadCPUP95 float64
	// VCPUDelayAvg and VCPUDelayMax are the host run-queue wait of the vCPU
	// threads, seconds per second per vCPU (the mean over vCPUs), so they
	// read on the same 0-1 scale as PSI whatever the VM's size. Sustained is the window
	// mean; the peak is the worst 1m bucket. Same quantity tare-agent's CPU
	// wait measures for a container.
	VCPUDelayAvg float64
	VCPUDelayMax float64

	// MemResident is QEMU's RSS: pages the guest has touched, never read as
	// guest use. MemDomain is the guest memory size; MemOverhead is KubeVirt's
	// own estimate of virt-launcher overhead.
	MemResident float64
	MemDomain   float64
	MemOverhead float64

	// GuestMemUsedMax is the guest's own used memory (available - usable,
	// cache excluded), the worst 5m bucket. GuestMemCoverage is the share of
	// the window it was reported for: a guest that reported for a day of a
	// fortnight has said nothing about the other thirteen.
	GuestMemUsedMax  float64
	GuestMemCoverage float64
	// BalloonMin is the smallest memory the balloon left the guest.
	BalloonMin float64
	// GuestSwapMax and GuestSwapAvg are swap traffic inside the guest, bytes
	// per second; GuestMajFaultMax and GuestMajFaultAvg are major page faults
	// per second.
	GuestSwapMax     float64
	GuestSwapAvg     float64
	GuestMajFaultMax float64
	GuestMajFaultAvg float64
	// GuestLoadP95 is the guest's own 5-minute load average, p95 over the
	// window: run-queue demand as the guest sees it, needs qemu-guest-agent.
	GuestLoadP95 float64

	// NonEvictable is virt-handler's flag for a VMI whose eviction strategy
	// asks for live migration while the VMI is not migratable.
	NonEvictable bool
	// LastMigrationStart and LastMigrationEnd are unix seconds, zero when the
	// VMI did not migrate in the window.
	LastMigrationStart float64
	LastMigrationEnd   float64

	// Samples is how many scrapes of the domain size were seen, which is the
	// VMI's age in the window, whatever happened to its pods.
	Samples int

	Have map[Signal]bool
}

// Has reports whether a signal was obtained.
func (o *VMObservation) Has(s Signal) bool {
	if o == nil || o.Have == nil {
		return false
	}
	return o.Have[s]
}

// Mark records that s backed this observation.
func (o *VMObservation) Mark(s Signal) {
	if o.Have == nil {
		o.Have = map[Signal]bool{}
	}
	o.Have[s] = true
}

// VMObserver is an optional Source capability: reading KubeVirt's per-VMI
// series. Like NodeObserver it is not part of Source, and it is asked only
// when the inventory holds a VM, so a cluster without KubeVirt never sees a
// row of absent kubevirt_* signals in its report header.
type VMObserver interface {
	ObserveVMs(namespaces []string, window time.Duration) (map[VMKey]*VMObservation, error)
}

// vmQuery is one expression of the VM set. Every one is reduced by
// (namespace, name) before it is sent: virt-handler exports per vCPU, per
// interface and per migration, and the join below wants one series per VMI.
type vmQuery struct {
	signal Signal
	expr   string
	assign func(o *VMObservation, v float64)
}

// vmRunning matches the running state of kubevirt_vmi_vcpu_seconds_total. The
// label value's case has not been the same in every KubeVirt release, so the
// match ignores it rather than silently matching nothing.
const vmRunning = `state=~"(?i)running"`

// vmQueries builds the VM query set. Selectors carry the namespace only: these
// series have no container label, and container!="" would exclude all of them.
func vmQueries(namespaces []string, window time.Duration) []vmQuery {
	ns := nsSelector(namespaces)
	sel := func(extra ...string) string {
		parts := append([]string{}, extra...)
		if ns != "" {
			parts = append([]string{ns}, parts...)
		}
		return strings.Join(parts, ",")
	}
	w := durationString(window)
	const step, peak = "5m", "1m"
	by := "sum by (namespace,name)"
	perVCPU := "avg by (namespace,name)"
	vcpu := func(r string) string {
		return fmt.Sprintf(`%s (rate(kubevirt_vmi_vcpu_seconds_total{%s}[%s]))`, by, sel(vmRunning), r)
	}
	q := []vmQuery{
		{SigVMVCPU, fmt.Sprintf(`quantile_over_time(0.95, (%s)[%s:%s])`, vcpu(step), w, step),
			func(o *VMObservation, v float64) { o.VCPUUsageP95 = v }},
		{SigVMVCPU, fmt.Sprintf(`max_over_time((%s)[%s:%s])`, vcpu(peak), w, peak),
			func(o *VMObservation, v float64) { o.VCPUUsageMax = v }},
		{SigVMOverheadCPU, fmt.Sprintf(`quantile_over_time(0.95, clamp_min(%s (rate(kubevirt_vmi_cpu_usage_seconds_total{%s}[%s])) - %s, 0)[%s:%s])`,
			by, sel(), step, vcpu(step), w, step),
			func(o *VMObservation, v float64) { o.OverheadCPUP95 = v }},
		// Averaged, not summed: the series are per vCPU, and the threshold
		// they meet is a fraction of one CPU. Summed, a 16-vCPU VM read 16
		// times the wait of a 1-vCPU VM with the same per-vCPU stall.
		{SigVMVCPUDelay, fmt.Sprintf(`avg_over_time((%s (rate(kubevirt_vmi_vcpu_delay_seconds_total{%s}[%s])))[%s:%s])`, perVCPU, sel(), step, w, step),
			func(o *VMObservation, v float64) { o.VCPUDelayAvg = v }},
		{SigVMVCPUDelay, fmt.Sprintf(`max_over_time((%s (rate(kubevirt_vmi_vcpu_delay_seconds_total{%s}[%s])))[%s:%s])`, perVCPU, sel(), peak, w, peak),
			func(o *VMObservation, v float64) { o.VCPUDelayMax = v }},
		{SigVMMemResident, fmt.Sprintf(`max by (namespace,name) (max_over_time(kubevirt_vmi_memory_resident_bytes{%s}[%s]))`, sel(), w),
			func(o *VMObservation, v float64) { o.MemResident = v }},
		{SigVMMemDomain, fmt.Sprintf(`max by (namespace,name) (max_over_time(kubevirt_vmi_memory_domain_bytes{%s}[%s]))`, sel(), w),
			func(o *VMObservation, v float64) { o.MemDomain = v }},
		// Summed: virt-handler reports a VMI from the node it runs on, so a
		// migration continues it as another series, and the series partition
		// the window between them (KNOWN-FAILURES 61). The max was one node's
		// share. Only the seconds of a copy are counted twice.
		{SigVMMemDomain, fmt.Sprintf(`sum by (namespace,name) (count_over_time(kubevirt_vmi_memory_domain_bytes{%s}[%s]))`, sel(), w),
			func(o *VMObservation, v float64) { o.Samples = int(v) }},
		{SigVMMemOverhead, fmt.Sprintf(`max by (namespace,name) (max_over_time(kubevirt_vmi_launcher_memory_overhead_bytes{%s}[%s]))`, sel(), w),
			func(o *VMObservation, v float64) { o.MemOverhead = v }},
		{SigVMNonEvict, fmt.Sprintf(`max by (namespace,name) (max_over_time(kubevirt_vmi_non_evictable{%s}[%s]))`, sel(), w),
			func(o *VMObservation, v float64) { o.NonEvictable = v > 0 }},
		// Guest-reported. available - usable is what the guest itself counts
		// as used, page cache excluded; domain - usable would also count
		// whatever an inflated balloon took away from it.
		{SigVMGuestMem, fmt.Sprintf(`max_over_time((max by (namespace,name) (kubevirt_vmi_memory_available_bytes{%s}) - max by (namespace,name) (kubevirt_vmi_memory_usable_bytes{%s}))[%s:%s])`,
			sel(), sel(), w, step),
			func(o *VMObservation, v float64) { o.GuestMemUsedMax = v }},
		{SigVMGuestMem, fmt.Sprintf(`sum by (namespace,name) (count_over_time(kubevirt_vmi_memory_usable_bytes{%s}[%s])) / sum by (namespace,name) (count_over_time(kubevirt_vmi_memory_domain_bytes{%s}[%s]))`,
			sel(), w, sel(), w),
			func(o *VMObservation, v float64) { o.GuestMemCoverage = min(v, 1) }},
		{SigVMBalloon, fmt.Sprintf(`min by (namespace,name) (min_over_time(kubevirt_vmi_memory_actual_balloon_bytes{%s}[%s]))`, sel(), w),
			func(o *VMObservation, v float64) { o.BalloonMin = v }},
		{SigVMGuestPressure, fmt.Sprintf(`max_over_time((%s (rate(kubevirt_vmi_memory_swap_in_traffic_bytes{%s}[%s]) + rate(kubevirt_vmi_memory_swap_out_traffic_bytes{%s}[%s])))[%s:%s])`,
			by, sel(), step, sel(), step, w, step),
			func(o *VMObservation, v float64) { o.GuestSwapMax = v }},
		{SigVMGuestPressure, fmt.Sprintf(`avg_over_time((%s (rate(kubevirt_vmi_memory_swap_in_traffic_bytes{%s}[%s]) + rate(kubevirt_vmi_memory_swap_out_traffic_bytes{%s}[%s])))[%s:%s])`,
			by, sel(), step, sel(), step, w, step),
			func(o *VMObservation, v float64) { o.GuestSwapAvg = v }},
		{SigVMGuestPressure, fmt.Sprintf(`max_over_time((%s (rate(kubevirt_vmi_memory_pgmajfault_total{%s}[%s])))[%s:%s])`, by, sel(), step, w, step),
			func(o *VMObservation, v float64) { o.GuestMajFaultMax = v }},
		{SigVMGuestPressure, fmt.Sprintf(`avg_over_time((%s (rate(kubevirt_vmi_memory_pgmajfault_total{%s}[%s])))[%s:%s])`, by, sel(), step, w, step),
			func(o *VMObservation, v float64) { o.GuestMajFaultAvg = v }},
		{SigVMGuestLoad, fmt.Sprintf(`quantile_over_time(0.95, (max by (namespace,name) (kubevirt_vmi_guest_load_5m{%s}))[%s:%s])`, sel(), w, step),
			func(o *VMObservation, v float64) { o.GuestLoadP95 = v }},
		// Migration series name the VMI as `name` on some releases and `vmi`
		// on others; grouping by both and reading either keeps the join.
		{SigVMMigrations, fmt.Sprintf(`max by (namespace,name,vmi) (max_over_time(kubevirt_vmi_migration_start_time_seconds{%s}[%s]))`, sel(), w),
			func(o *VMObservation, v float64) { o.LastMigrationStart = v }},
		{SigVMMigrations, fmt.Sprintf(`max by (namespace,name,vmi) (max_over_time(kubevirt_vmi_migration_end_time_seconds{%s}[%s]))`, sel(), w),
			func(o *VMObservation, v float64) { o.LastMigrationEnd = v }},
	}
	return q
}

// ObserveVMs runs the VM query set and joins it by VMI. Every signal is
// optional: a cluster whose Prometheus does not scrape virt-handler gets VM
// findings sized from cAdvisor alone, with the gap stated.
func (p *Prom) ObserveVMs(namespaces []string, window time.Duration) (map[VMKey]*VMObservation, error) {
	out := map[VMKey]*VMObservation{}
	for _, q := range vmQueries(namespaces, window) {
		samples, err := p.query(q.expr)
		p.health = append(p.health, SignalHealth{Signal: q.signal, Query: q.expr, Err: err, Series: len(samples)})
		if err != nil {
			continue
		}
		for _, s := range samples {
			name := s.labels["name"]
			if name == "" {
				name = s.labels["vmi"]
			}
			if name == "" || s.labels["namespace"] == "" {
				continue
			}
			k := VMKey{Namespace: s.labels["namespace"], Name: name}
			o, ok := out[k]
			if !ok {
				o = &VMObservation{VMKey: k}
				out[k] = o
			}
			q.assign(o, s.value)
			o.Mark(q.signal)
		}
	}
	return out, nil
}

// PreflightVMs is Preflight for virt-handler's series. It is separate because
// it only means something on a cluster that runs VMs: on any other, every row
// would read "absent" and send the reader looking for a component they do not
// need.
func (p *Prom) PreflightVMs(namespaces []string) []SeriesCheck {
	checks := []SeriesCheck{
		{Metric: "kubevirt_vmi_cpu_usage_seconds_total",
			Buys: "VM CPU split into guest and QEMU/libvirt overhead (virt-handler is not scraped without it)"},
		{Metric: "kubevirt_vmi_vcpu_seconds_total",
			Buys: "guest vCPU use, behind the vCPU-count advice"},
		{Metric: "kubevirt_vmi_vcpu_delay_seconds_total",
			Buys: "vCPU run-queue wait: the host-side saturation signal for a VM, so it is not UNVERIFIED without PSI"},
		{Metric: "kubevirt_vmi_memory_domain_bytes",
			Buys: "the guest memory size and the VM's age across live migrations"},
		{Metric: "kubevirt_vmi_launcher_memory_overhead_bytes",
			Buys: "KubeVirt's own overhead estimate; without it the overhead is derived from the pod request"},
		{Metric: "kubevirt_vmi_memory_usable_bytes",
			Buys: "guest-reported used memory (balloon driver), the only evidence a guest memory reduction can rest on"},
		{Metric: "kubevirt_vmi_memory_swap_in_traffic_bytes",
			Buys: "swapping inside the guest, which blocks a guest memory reduction"},
		{Metric: "kubevirt_vmi_guest_load_5m",
			Buys: "guest load (qemu-guest-agent), which separates a VM short of vCPUs from one short of host CPU"},
	}
	ns := nsSelector(namespaces)
	for i := range checks {
		samples, err := p.query(fmt.Sprintf("count(%s{%s})", checks[i].Metric, ns))
		checks[i].Err = err
		for _, s := range samples {
			checks[i].Series += int(s.value)
		}
	}
	return checks
}
