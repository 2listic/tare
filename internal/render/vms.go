package render

import (
	"fmt"
	"sort"
	"strconv"

	"github.com/2listic/tare/internal/analyze"
	"github.com/2listic/tare/internal/metrics"
)

// printVMs is a KubeVirt VM in its own terms: vCPUs and guest memory, which are
// the knobs, beside QEMU's resident memory and the guest's own report, which
// are the two numbers that look alike and mean different things. Printed only
// when the report holds a VM.
func (p *printer) printVMs(fs []analyze.Finding) {
	var vms []analyze.Finding
	for _, f := range fs {
		if f.IsVM() {
			vms = append(vms, f)
		}
	}
	if len(vms) == 0 {
		return
	}
	w := p.section("vms",
		"NAMESPACE/VM", "VCPUS>REC", "GUEST>REC", "VCPU_P95", "OVERHEAD_CPU", "VCPU_DELAY",
		"QEMU_RSS", "GUEST_USED", "OVERHEAD_MEM", "MIGRATABLE")
	for _, f := range vms {
		o := f.VMObs
		row(w, shortWorkload(f.Namespace, f.Workload, 48),
			vcpuCell(f), guestCell(f),
			vmValue(o, metrics.SigVMVCPU, func() string { return fmt.Sprintf("%.2f", o.VCPUUsageP95) }),
			vmValue(o, metrics.SigVMOverheadCPU, func() string { return fmt.Sprintf("%.0fm", o.OverheadCPUP95*1000) }),
			vmValue(o, metrics.SigVMVCPUDelay, func() string { return fmt.Sprintf("%.4f", o.VCPUDelayAvg) }),
			analyze.HumanBytes(vmRSS(f)),
			vmValue(o, metrics.SigVMGuestMem, func() string { return Human(int64(o.GuestMemUsedMax)) }),
			Human(f.VMOverhead),
			migratableCell(f))
	}
	w.Flush()
	p.printVMDensity(vms)
}

// printVMDensity sums, per node, what the VMs on it reserve, what QEMU holds
// resident, and what their guests were promised. The gaps are the two sides
// of memory overcommit: reserved above resident is room the scheduler will
// not hand out, and promised above reserved is memory the node has
// guaranteed nobody.
func (p *printer) printVMDensity(vms []analyze.Finding) {
	type agg struct {
		n                  int
		req, rss, promised int64
	}
	by := map[string]*agg{}
	for _, f := range vms {
		node := "-"
		if len(f.Nodes) > 0 {
			node = f.Nodes[0]
		}
		a := by[node]
		if a == nil {
			a = &agg{}
			by[node] = a
		}
		a.n++
		a.req += f.MemRequest
		a.rss += vmRSS(f)
		// A hugepage-backed guest is requested as hugepages, apart from
		// memory, so it is neither requested nor promised here.
		if f.VM != nil && f.VM.HugepageSize == "" {
			a.promised += f.VM.GuestMemory
		}
		a.promised += f.VMOverhead
	}
	nodes := make([]string, 0, len(by))
	for n := range by {
		nodes = append(nodes, n)
	}
	sort.Strings(nodes)
	w := p.section("vm-density", "NODE", "VMS", "MEM_REQUESTED", "QEMU_RSS", "GUEST+OVERHEAD",
		"REQUESTED-RSS", "PROMISED-REQUESTED")
	for _, n := range nodes {
		a := by[n]
		row(w, n, strconv.Itoa(a.n), Human(a.req), Human(a.rss), Human(a.promised),
			MemDelta(a.req-a.rss), MemDelta(a.promised-a.req))
	}
	w.Flush()
	fmt.Fprintln(p.out, "# REQUESTED-RSS is reserved and untouched; PROMISED-REQUESTED is promised memory no request backs (overcommit)")
}

func vcpuCell(f analyze.Finding) string {
	if f.VM == nil || f.VM.VCPUs == 0 {
		return ""
	}
	if f.RecVCPUs == 0 || f.RecVCPUs == f.VM.VCPUs {
		return strconv.FormatInt(f.VM.VCPUs, 10)
	}
	return fmt.Sprintf("%d>%d", f.VM.VCPUs, f.RecVCPUs)
}

func guestCell(f analyze.Finding) string {
	if f.VM == nil || f.VM.GuestMemory == 0 {
		return ""
	}
	if g, ok := analyze.VMGuestChange(f); ok {
		return Human(f.VM.GuestMemory) + ">" + Human(g)
	}
	return Human(f.VM.GuestMemory)
}

func vmRSS(f analyze.Finding) int64 {
	if f.VMObs.Has(metrics.SigVMMemResident) {
		return int64(f.VMObs.MemResident)
	}
	return int64(f.MemUsageMax)
}

// vmValue prints a VM measurement, or "unmeasured": a missing guest report is
// not a guest using nothing.
func vmValue(o *metrics.VMObservation, s metrics.Signal, v func() string) string {
	if !o.Has(s) {
		return "unmeasured"
	}
	return v()
}

func migratableCell(f analyze.Finding) string {
	switch {
	case f.VM == nil || !f.VM.LiveMigratableKnown:
		return "unknown"
	case f.VM.Migratable():
		return "yes"
	case f.VM.LiveMigratable:
		return "no (eviction " + orDash(f.VM.EvictionStrategy) + ")"
	}
	return "no"
}

func orDash(s string) string {
	if s == "" {
		return "unset"
	}
	return s
}
