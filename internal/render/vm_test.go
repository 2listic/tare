package render

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/2listic/tare/internal/analyze"
	"github.com/2listic/tare/internal/discover"
)

func vmFinding() analyze.Finding {
	return analyze.Finding{
		Namespace: "vms", Workload: "VirtualMachine/db", Container: "compute", Verdict: analyze.Waste,
		Nodes: []string{"n1"}, Pods: 1,
		CPURequest: 400, RecCPU: 200, MemRequest: 8<<30 + 250<<20, RecMem: 2<<30 + 250<<20,
		RecGuestMem: 2 << 30, VMOverhead: 250 << 20, RecVCPUs: 1, MemUsageMax: 6 << 30,
		VM: &discover.VMSpec{VM: "db", VMI: "db", VCPUs: 4, GuestMemory: 8 << 30,
			LiveMigratable: true, LiveMigratableKnown: true, EvictionStrategy: "LiveMigrate"},
		Reasons: []analyze.Reason{{Code: analyze.VMGuestMemOversized, Text: "the guest uses 1Gi"}},
	}
}

// A VM is patched on its domain with a merge patch: the pod's resources are
// KubeVirt's, and a strategic merge patch on a CRD is rejected.
func TestPatchTargetsTheVirtualMachineDomain(t *testing.T) {
	out := capture(t, func(p *printer) { p.printPatches([]analyze.Finding{vmFinding()}, "lab", "") })
	i := strings.Index(out, "patch vm 'db' --type=merge -p '")
	if i < 0 {
		t.Fatalf("no VM patch:\n%s", out)
	}
	doc := strings.TrimSuffix(strings.TrimSpace(out[i+len("patch vm 'db' --type=merge -p '"):]), "'")
	var p struct {
		Spec struct {
			Template struct {
				Spec struct {
					Domain struct {
						Memory    map[string]string `json:"memory"`
						Resources struct {
							Requests map[string]string `json:"requests"`
							Limits   map[string]string `json:"limits"`
						} `json:"resources"`
					} `json:"domain"`
					Containers any `json:"containers"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	if err := json.Unmarshal([]byte(doc), &p); err != nil {
		t.Fatalf("%v: %s", err, doc)
	}
	d := p.Spec.Template.Spec.Domain
	if d.Memory["guest"] != "2Gi" || d.Resources.Requests["cpu"] != "200m" || d.Resources.Limits != nil ||
		p.Spec.Template.Spec.Containers != nil {
		t.Errorf("got %s", doc)
	}
	if !strings.Contains(out, "VM_GUEST_MEMORY_OVERSIZED") || !strings.Contains(out, "--context 'lab'") {
		t.Errorf("evidence or context missing:\n%s", out)
	}
}

func TestWideShowsVMsOnlyWhenThereAreSome(t *testing.T) {
	r := sampleReport()
	out := capture(t, func(p *printer) { p.printVMs(r.Findings) })
	if out != "" {
		t.Errorf("a report without VMs printed a VM section:\n%s", out)
	}
	out = capture(t, func(p *printer) { p.printVMs([]analyze.Finding{vmFinding()}) })
	for _, want := range []string{"# section=vms", "vms/vm/db", "4>1", "8.0Gi>2.0Gi", "unmeasured", "# section=vm-density", "n1"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
}

func TestVMKindsAreAbbreviated(t *testing.T) {
	if got := WorkloadCell("VirtualMachine/db", 40); got != "vm/db" {
		t.Errorf("WorkloadCell = %q", got)
	}
	if got := shortWorkload("vms", "VirtualMachineInstance/db", 40); got != "vms/vmi/db" {
		t.Errorf("shortWorkload = %q", got)
	}
}
