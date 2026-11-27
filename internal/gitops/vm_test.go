package gitops

import (
	"fmt"
	"os"
	"strings"
	"testing"

	yaml "go.yaml.in/yaml/v3"

	"github.com/2listic/tare/internal/analyze"
	"github.com/2listic/tare/internal/discover"
)

const vmYAML = `apiVersion: kubevirt.io/v1
kind: VirtualMachine
metadata:
  name: db
  namespace: vms
spec:
  runStrategy: Always
  template:
    spec:
      domain:
        cpu:
          cores: 4   # sized for the 2024 migration
        memory:
          guest: 8Gi
        resources:
          requests:
            cpu: "1"
        devices:
          disks:
            - name: root
              disk: {bus: virtio}
      volumes:
        - name: root
          dataVolume: {name: db-root}
`

const vmBareYAML = `apiVersion: kubevirt.io/v1
kind: VirtualMachine
metadata:
  name: db
  namespace: vms
spec:
  template:
    spec:
      domain:
        cpu:
          cores: 4
        devices: {}
`

func vmChangeFor(cpu, guest, memReq int64) Change {
	c := Change{Namespace: "vms", Kind: "VirtualMachine", Name: "db", Container: "compute", VM: true}
	if cpu > 0 {
		c.CPURequest = ptr(cpu)
	}
	if guest > 0 {
		c.GuestMemory = ptr(guest)
	}
	if memReq > 0 {
		c.MemRequest = ptr(memReq)
	}
	return c
}

// domainOf decodes the edited VM back, the way the API server will.
func domainOf(t *testing.T, out string) map[string]any {
	t.Helper()
	var obj map[string]any
	if err := yaml.Unmarshal([]byte(out), &obj); err != nil {
		t.Fatalf("edited VM does not parse: %v\n%s", err, out)
	}
	spec := obj["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
	return spec["domain"].(map[string]any)
}

// A VM's knobs are its domain's, not a container's: the CPU request under
// domain.resources and the guest memory under domain.memory. Everything else
// in the file, comments included, is left as it was.
func TestVMEditWritesTheDomain(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "db.yaml", vmYAML)
	if _, skipped, err := ApplyTree(dir, []Change{vmChangeFor(300, 3<<30, 0)}, false); err != nil || len(skipped) > 0 {
		t.Fatalf("err %v, skipped %v", err, skipped)
	}
	raw, _ := os.ReadFile(p)
	out := string(raw)
	d := domainOf(t, out)
	if got := d["memory"].(map[string]any)["guest"]; got != "3Gi" {
		t.Errorf("guest = %v\n%s", got, out)
	}
	if got := d["resources"].(map[string]any)["requests"].(map[string]any)["cpu"]; got != "300m" {
		t.Errorf("cpu = %v\n%s", got, out)
	}
	if !strings.Contains(out, "sized for the 2024 migration") || !strings.Contains(out, "disk: {bus: virtio}") {
		t.Errorf("unrelated text changed:\n%s", out)
	}
	if n := strings.Count(out, "\n") - strings.Count(vmYAML, "\n"); n != 0 {
		t.Errorf("an in-place edit changed the line count by %d:\n%s", n, out)
	}
}

// A VM that sets neither gains both, as block YAML beside its other keys.
func TestVMEditCreatesMissingKeys(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "db.yaml", vmBareYAML)
	if _, skipped, err := ApplyTree(dir, []Change{vmChangeFor(250, 2<<30, 2<<30)}, false); err != nil || len(skipped) > 0 {
		t.Fatalf("err %v, skipped %v", err, skipped)
	}
	raw, _ := os.ReadFile(p)
	d := domainOf(t, string(raw))
	req := d["resources"].(map[string]any)["requests"].(map[string]any)
	if req["cpu"] != "250m" || req["memory"] != "2Gi" || d["memory"].(map[string]any)["guest"] != "2Gi" {
		t.Errorf("got %v\n%s", d, raw)
	}
}

// A flow-style domain is refused with a reason, as a container's would be.
func TestVMEditRefusesFlowStyle(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "db.yaml", strings.Replace(vmBareYAML,
		"        cpu:\n          cores: 4\n        devices: {}\n", "        {cpu: {cores: 4}}\n", 1))
	_, skipped, err := ApplyTree(dir, []Change{vmChangeFor(250, 2<<30, 0)}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped) != 1 || !strings.Contains(skipped[0].Why, "flow style") {
		t.Errorf("want a flow-style refusal, got %v", skipped)
	}
}

func TestVMKustomizePatch(t *testing.T) {
	patches, list := Kustomize([]Change{vmChangeFor(300, 3<<30, 0)})
	if len(patches) != 1 || !strings.Contains(list, "tare-vms-virtualmachine-db.yaml") {
		t.Fatalf("got %v %s", patches, list)
	}
	body := patches[0].Body
	if !strings.Contains(body, "apiVersion: kubevirt.io/v1") || strings.Contains(body, "containers:") {
		t.Errorf("not a VM patch:\n%s", body)
	}
	d := domainOf(t, body)
	if d["memory"].(map[string]any)["guest"] != "3Gi" ||
		d["resources"].(map[string]any)["requests"].(map[string]any)["cpu"] != "300m" {
		t.Errorf("got %v\n%s", d, body)
	}
}

// The plan writes only what changes, and lowers an authored memory request
// with the guest so the shrink reaches the pod.
func TestPlanForAVM(t *testing.T) {
	f := analyze.Finding{
		Namespace: "vms", Workload: "VirtualMachine/db", Container: "compute", Verdict: analyze.Waste,
		CPURequest: 400, RecCPU: 200, MemRequest: 8 << 30, RecMem: 2 << 30, RecGuestMem: 2 << 30,
		VM: &discover.VMSpec{VM: "db", VMI: "db", VCPUs: 4, GuestMemory: 8 << 30, AuthoredMemRequest: 8 << 30},
	}
	changes, skipped := Plan([]analyze.Finding{f})
	if len(changes) != 1 || len(skipped) != 0 {
		t.Fatalf("changes %v skipped %v", changes, skipped)
	}
	c := changes[0]
	if !c.VM || deref(c.CPURequest) != 200 || deref(c.GuestMemory) != 2<<30 || deref(c.MemRequest) != 2<<30 ||
		c.CPULimit != nil || c.MemLimit != nil {
		t.Errorf("got %+v", c)
	}

	// Nothing to change is not a skip: the report is full of VMs like it.
	f.RecCPU, f.RecGuestMem = f.CPURequest, 0
	if changes, skipped := Plan([]analyze.Finding{f}); len(changes) != 0 || len(skipped) != 0 {
		t.Errorf("an unchanged VM produced %v / %v", changes, skipped)
	}
}

func TestPullRequestBodyNamesTheDomainFields(t *testing.T) {
	body := PullRequestBody([]Change{vmChangeFor(300, 3<<30, 0)}, nil, "prom", "7d")
	if !strings.Contains(body, "domain.memory.guest") || !strings.Contains(body, "domain.resources.requests.cpu") ||
		strings.Contains(body, "| requests.cpu") {
		t.Errorf("got:\n%s", body)
	}
}

// A VM that authors limits keeps request <= limit after the change, or the API
// server rejects it; a limit pinned to its request (Guaranteed) moves with it,
// or a shrink silently makes the VM Burstable. A guest raised while the
// request equalled the old guest takes the request with it, or the raise is
// an overcommit nobody asked for.
func TestPlanForAVMKeepsItsLimitsValid(t *testing.T) {
	const gi = int64(1) << 30
	base := func(mod func(f *analyze.Finding)) analyze.Finding {
		f := analyze.Finding{
			Namespace: "vms", Workload: "VirtualMachine/db", Container: "compute", Verdict: analyze.Saturated,
			CPURequest: 500, RecCPU: 500, MemRequest: 4 * gi, RecMem: 4 * gi,
			VM: &discover.VMSpec{VM: "db", VMI: "db", VCPUs: 2, GuestMemory: 4 * gi, AuthoredKnown: true},
		}
		mod(&f)
		return f
	}
	for _, tc := range []struct {
		name                     string
		f                        analyze.Finding
		cpuLim, memReq, memLimit int64 // 0: not written
	}{
		{"cpu raised past its limit", base(func(f *analyze.Finding) {
			f.RecCPU = 2000
			f.VM.AuthoredCPURequest, f.VM.AuthoredCPULimit = 500, 1000
		}), 2000, 0, 0},
		{"cpu raised under its limit", base(func(f *analyze.Finding) {
			f.RecCPU = 800
			f.VM.AuthoredCPURequest, f.VM.AuthoredCPULimit = 500, 1000
		}), 0, 0, 0},
		{"guaranteed shrink", base(func(f *analyze.Finding) {
			f.Verdict, f.CPURequest, f.RecCPU, f.RecGuestMem = analyze.Waste, 2000, 1000, 2*gi
			f.VM.GuestMemory = 8 * gi
			f.VM.AuthoredCPURequest, f.VM.AuthoredCPULimit = 2000, 2000
			f.VM.AuthoredMemRequest, f.VM.AuthoredMemLimit = 8*gi, 8*gi
		}), 1000, 2 * gi, 2 * gi},
		{"guest raised past its limit", base(func(f *analyze.Finding) {
			f.RecGuestMem = 6 * gi
			f.VM.AuthoredMemRequest, f.VM.AuthoredMemLimit = 4*gi, 5*gi
		}), 0, 6 * gi, 6 * gi},
		{"guest raised, request was the guest", base(func(f *analyze.Finding) {
			f.RecGuestMem = 6 * gi
			f.VM.AuthoredMemRequest = 4 * gi
		}), 0, 6 * gi, 0},
		{"guest raised, deliberate overcommit", base(func(f *analyze.Finding) {
			f.RecGuestMem = 6 * gi
			f.VM.AuthoredMemRequest = 2 * gi
		}), 0, 0, 0},
	} {
		changes, skipped := Plan([]analyze.Finding{tc.f})
		if len(changes) != 1 || len(skipped) != 0 {
			t.Errorf("%s: changes %v skipped %v", tc.name, changes, skipped)
			continue
		}
		c := changes[0]
		if deref(c.CPULimit) != tc.cpuLim || deref(c.MemRequest) != tc.memReq || deref(c.MemLimit) != tc.memLimit {
			t.Errorf("%s: cpu limit %d, mem request %d, mem limit %d; want %d, %d, %d", tc.name,
				deref(c.CPULimit), deref(c.MemRequest), deref(c.MemLimit), tc.cpuLim, tc.memReq, tc.memLimit)
		}
	}
}

// Every writer puts a VM's limits under domain.resources.limits.
func TestVMWritersWriteLimits(t *testing.T) {
	c := vmChangeFor(2000, 6<<30, 6<<30)
	c.CPULimit, c.MemLimit = ptr(2000), ptr(6<<30)

	dir := t.TempDir()
	p := write(t, dir, "db.yaml", vmYAML)
	if _, skipped, err := ApplyTree(dir, []Change{c}, false); err != nil || len(skipped) > 0 {
		t.Fatalf("err %v, skipped %v", err, skipped)
	}
	raw, _ := os.ReadFile(p)
	lim, _ := domainOf(t, string(raw))["resources"].(map[string]any)["limits"].(map[string]any)
	if fmt.Sprint(lim["cpu"]) != "2" || lim["memory"] != "6Gi" {
		t.Errorf("tree edit limits = %v\n%s", lim, raw)
	}

	// With no resources at all, requests and limits land under one key: a
	// second resources: would be a duplicate key the API server rejects.
	p = write(t, dir, "bare.yaml", vmBareYAML)
	if _, skipped, err := ApplyTree(dir, []Change{c}, false); err != nil || len(skipped) > 0 {
		t.Fatalf("err %v, skipped %v", err, skipped)
	}
	raw, _ = os.ReadFile(p)
	if n := strings.Count(string(raw), "resources:"); n != 1 {
		t.Errorf("resources written %d times:\n%s", n, raw)
	}
	res, _ := domainOf(t, string(raw))["resources"].(map[string]any)
	req, _ := res["requests"].(map[string]any)
	lim, _ = res["limits"].(map[string]any)
	if fmt.Sprint(req["cpu"]) != "2" || fmt.Sprint(lim["cpu"]) != "2" || lim["memory"] != "6Gi" {
		t.Errorf("bare VM resources = %v\n%s", res, raw)
	}

	// An empty resources: {} is filled once, not twice.
	p = write(t, dir, "empty.yaml", strings.Replace(vmBareYAML, "        devices: {}\n", "        resources: {}\n        devices: {}\n", 1))
	if _, skipped, err := ApplyTree(dir, []Change{c}, false); err != nil || len(skipped) > 0 {
		t.Fatalf("err %v, skipped %v", err, skipped)
	}
	raw, _ = os.ReadFile(p)
	res, _ = domainOf(t, string(raw))["resources"].(map[string]any)
	req, _ = res["requests"].(map[string]any)
	lim, _ = res["limits"].(map[string]any)
	if strings.Count(string(raw), "resources:") != 1 || fmt.Sprint(req["cpu"]) != "2" || lim["memory"] != "6Gi" {
		t.Errorf("empty resources filled as %v\n%s", res, raw)
	}

	patches, _ := Kustomize([]Change{c})
	lim, _ = domainOf(t, patches[0].Body)["resources"].(map[string]any)["limits"].(map[string]any)
	if lim["cpu"] != "2" || lim["memory"] != "6Gi" {
		t.Errorf("kustomize limits = %v\n%s", lim, patches[0].Body)
	}

	body := PullRequestBody([]Change{c}, nil, "prom", "7d")
	if !strings.Contains(body, "domain.resources.limits.cpu") || !strings.Contains(body, "domain.resources.limits.memory") {
		t.Errorf("PR body does not name the limits:\n%s", body)
	}
}
