package discover

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func vmi(ns, name, owner string, spec map[string]any, migratable string) *unstructured.Unstructured {
	meta := map[string]any{"namespace": ns, "name": name}
	if owner != "" {
		meta["ownerReferences"] = []any{map[string]any{
			"apiVersion": "kubevirt.io/v1", "kind": "VirtualMachine", "name": owner, "uid": "u1", "controller": true,
		}}
	}
	obj := map[string]any{
		"apiVersion": "kubevirt.io/v1", "kind": "VirtualMachineInstance",
		"metadata": meta, "spec": spec,
		"status": map[string]any{"guestOSInfo": map[string]any{"id": "mswindows"}},
	}
	if migratable != "" {
		obj["status"].(map[string]any)["conditions"] = []any{
			map[string]any{"type": "LiveMigratable", "status": migratable},
		}
	}
	return &unstructured.Unstructured{Object: obj}
}

func vmObj(ns, name string, domain map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kubevirt.io/v1", "kind": "VirtualMachine",
		"metadata": map[string]any{"namespace": ns, "name": name},
		"spec":     map[string]any{"template": map[string]any{"spec": map[string]any{"domain": domain}}},
	}}
}

func kubevirtCR(conf map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kubevirt.io/v1", "kind": "KubeVirt",
		"metadata": map[string]any{"namespace": "kubevirt", "name": "kubevirt"},
		"spec":     map[string]any{"configuration": conf},
	}}
}

func kvClient(objs ...runtime.Object) *dynfake.FakeDynamicClient {
	return dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			vmiGVR: "VirtualMachineInstanceList", vmGVR: "VirtualMachineList",
			kubevirtGVR: "KubeVirtList", vpaGVR: "VerticalPodAutoscalerList",
		}, objs...)
}

func launcher(ns, name, vmiName string, containers ...string) *corev1.Pod {
	yes := true
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns, Name: name, Labels: map[string]string{"kubevirt.io": "virt-launcher"},
			OwnerReferences: []metav1.OwnerReference{{Kind: "VirtualMachineInstance", Name: vmiName, Controller: &yes}},
		},
		Spec: corev1.PodSpec{NodeName: "n1"},
	}
	for _, c := range containers {
		p.Spec.Containers = append(p.Spec.Containers, corev1.Container{Name: c, Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("400m"), corev1.ResourceMemory: resource.MustParse("8442Mi")},
		}})
	}
	return p
}

var standardDomain = map[string]any{
	"cpu":       map[string]any{"sockets": int64(2), "cores": int64(2), "threads": int64(1), "maxSockets": int64(8)},
	"memory":    map[string]any{"guest": "8Gi", "maxGuest": "32Gi"},
	"resources": map[string]any{"requests": map[string]any{"memory": "8Gi", "cpu": int64(1)}},
}

// A virt-launcher pod belongs to a VMI, which belongs to the VirtualMachine
// people edit. Stopping at the VMI attributes the pod to an object that a
// restart replaces and that has no spec anyone writes.
func TestVirtLauncherPodsResolveToTheirVirtualMachine(t *testing.T) {
	dyn := kvClient(
		vmi("vms", "db", "db", map[string]any{"domain": standardDomain, "evictionStrategy": "LiveMigrate"}, "True"),
		vmObj("vms", "db", map[string]any{"memory": map[string]any{"guest": "8Gi"}}),
		kubevirtCR(map[string]any{
			"developerConfiguration": map[string]any{"cpuAllocationRatio": int64(5), "memoryOvercommit": int64(150)},
			"vmRolloutStrategy":      "LiveUpdate",
		}),
	)
	c := &Client{cs: fake.NewSimpleClientset(launcher("vms", "virt-launcher-db-abcde", "db", "compute", "guest-console-log")), dyn: dyn}

	cs, err := c.Containers(context.Background(), []string{"vms"})
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 2 {
		t.Fatalf("want 2 containers, got %d", len(cs))
	}
	for _, ct := range cs {
		if ct.Workload != "VirtualMachine/db" || ct.WorkloadKind != "VirtualMachine" {
			t.Errorf("%s attributed to %q (%q)", ct.Name, ct.Workload, ct.WorkloadKind)
		}
		if (ct.Name == VMComputeContainer) != (ct.VM != nil) {
			t.Errorf("VM spec on %s = %v: only compute carries it", ct.Name, ct.VM)
		}
	}
	var vm *VMSpec
	for _, ct := range cs {
		if ct.VM != nil {
			vm = ct.VM
		}
	}
	if vm == nil {
		t.Fatal("no VM spec")
	}
	if vm.VCPUs != 4 || vm.GuestMemory != 8<<30 || vm.CPURequest != 1000 || vm.MaxGuest != 32<<30 || vm.MaxSockets != 8 {
		t.Errorf("spec misread: %+v", vm)
	}
	if !vm.AuthoredKnown || !vm.AuthoredGuest || vm.AuthoredMemRequest != 0 {
		t.Errorf("authored fields misread: %+v", vm)
	}
	if vm.CPUAllocationRatio != 5 || vm.MemoryOvercommit != 150 || vm.RolloutStrategy != "LiveUpdate" {
		t.Errorf("KubeVirt CR misread: %+v", vm)
	}
	if !vm.Migratable() || !vm.Windows() {
		t.Errorf("migratable=%v windows=%v, want both", vm.Migratable(), vm.Windows())
	}
	if len(c.Warnings()) != 0 {
		t.Errorf("unexpected warnings: %q", c.Warnings())
	}
}

// Most clusters have no KubeVirt. Its absence is not worth a word.
func TestNoKubeVirtIsSilent(t *testing.T) {
	dyn := kvClient()
	for _, r := range []string{"virtualmachineinstances", "virtualmachines", "kubevirts"} {
		gr := schema.GroupResource{Group: "kubevirt.io", Resource: r}
		dyn.PrependReactor("list", r, func(a k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.NewNotFound(gr, "")
		})
	}
	c := &Client{cs: fake.NewSimpleClientset(launcher("vms", "virt-launcher-db-abcde", "db", "compute")), dyn: dyn}
	cs, err := c.Containers(context.Background(), []string{"vms"})
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Warnings()) != 0 {
		t.Errorf("missing CRDs produced warnings: %q", c.Warnings())
	}
	if cs[0].VM != nil || cs[0].Workload != "VirtualMachineInstance/db" {
		t.Errorf("without KubeVirt the pod stays with its VMI and no spec: %+v", cs[0])
	}
}

// A refused read is a finding lost, and must say which.
func TestForbiddenVMIsWarn(t *testing.T) {
	dyn := kvClient()
	dyn.PrependReactor("list", "virtualmachineinstances", func(a k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.NewForbidden(vmiGVR.GroupResource(), "", nil)
	})
	c := &Client{cs: fake.NewSimpleClientset(launcher("vms", "virt-launcher-db-abcde", "db", "compute")), dyn: dyn}
	if _, err := c.Containers(context.Background(), []string{"vms"}); err != nil {
		t.Fatal(err)
	}
	ws := c.Warnings()
	if len(ws) != 1 || !strings.Contains(ws[0], "QEMU's resident memory") || !c.Denied(ws[0]) {
		t.Errorf("want one denied warning about VM sizing, got %q", ws)
	}
}

// KubeVirt gives every virt-launcher pod a nodeSelector, the kvm device and
// usually a PVC. None of them stops a live-migratable VM from moving; the VM
// itself is what decides.
func TestVMPinRules(t *testing.T) {
	base := func() *corev1.Pod {
		p := launcher("vms", "virt-launcher-db-abcde", "db", "compute")
		p.Spec.NodeSelector = map[string]string{"kubevirt.io/schedulable": "true", "cpu-model.node.kubevirt.io/Skylake": "true"}
		p.Spec.Containers[0].Resources.Limits = corev1.ResourceList{"devices.kubevirt.io/kvm": resource.MustParse("1")}
		p.Spec.Volumes = []corev1.Volume{{Name: "disk", VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "db-disk"}}}}
		return p
	}
	migratable := VMSpec{LiveMigratable: true, LiveMigratableKnown: true, EvictionStrategy: "LiveMigrate"}
	for _, tc := range []struct {
		name string
		vm   *VMSpec
		edit func(*corev1.Pod)
		want string // substring of the pin reason, "" for movable
	}{
		{"migratable", &migratable, nil, ""},
		{"unreadable", nil, nil, "could not be read"},
		{"not migratable", &VMSpec{LiveMigratableKnown: true, EvictionStrategy: "LiveMigrate"}, nil, "VM is not live-migratable"},
		{"no eviction strategy", &VMSpec{LiveMigratable: true, LiveMigratableKnown: true}, nil, "eviction strategy"},
		{"dedicated", &VMSpec{LiveMigratable: true, LiveMigratableKnown: true, EvictionStrategy: "LiveMigrate", DedicatedCPU: true}, nil, "dedicated CPUs"},
		{"hugepages", &VMSpec{LiveMigratable: true, LiveMigratableKnown: true, EvictionStrategy: "LiveMigrate", HugepageSize: "1Gi"}, nil, "hugepages"},
		{"host devices", &VMSpec{LiveMigratable: true, LiveMigratableKnown: true, EvictionStrategy: "LiveMigrate", HostDevices: true}, nil, "host devices"},
		{"user nodeSelector", &migratable, func(p *corev1.Pod) { p.Spec.NodeSelector["zone"] = "a" }, "nodeSelector"},
		{"a GPU", &migratable, func(p *corev1.Pod) {
			p.Spec.Containers[0].Resources.Limits["nvidia.com/gpu"] = resource.MustParse("1")
		}, "extended resource"},
	} {
		p := base()
		if tc.edit != nil {
			tc.edit(p)
		}
		got := podFromVM(p, "VirtualMachine/db", nil, tc.vm)
		if tc.want == "" && got.Pinned {
			t.Errorf("%s: pinned (%s), want movable", tc.name, got.PinReason)
		}
		if tc.want != "" && !strings.Contains(got.PinReason, tc.want) {
			t.Errorf("%s: pin reason %q, want one mentioning %q", tc.name, got.PinReason, tc.want)
		}
	}
}

// A migrated VM's pods are found through the same index, so the replay pins
// or moves them from the VM's own facts.
func TestPodsCarryTheVMIntoTheReplay(t *testing.T) {
	dyn := kvClient(vmi("vms", "db", "db", map[string]any{"domain": standardDomain}, "False"))
	c := &Client{cs: fake.NewSimpleClientset(launcher("vms", "virt-launcher-db-abcde", "db", "compute")), dyn: dyn}
	pods, err := c.Pods(context.Background(), []string{"vms"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(pods) != 1 || pods[0].PinReason != "VM is not live-migratable" || pods[0].Workload != "VirtualMachine/db" {
		t.Errorf("got %+v", pods)
	}
}

// RuntimeClass overhead belongs to no container, and a sizer that replaces
// the containers' requests must be able to add it back.
func TestPodOverheadIsKept(t *testing.T) {
	p := launcher("vms", "virt-launcher-db-abcde", "db", "compute")
	p.Spec.Overhead = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m"), corev1.ResourceMemory: resource.MustParse("64Mi")}
	got := podFromVM(p, "VirtualMachine/db", nil, nil)
	if got.OverheadCPU != 100 || got.OverheadMem != 64<<20 || got.CPU != 500 {
		t.Errorf("overhead not carried: %+v", got)
	}
}

func TestNumericQuantitiesAreRead(t *testing.T) {
	v := vmSpecFrom(map[string]any{"domain": map[string]any{
		"resources": map[string]any{"requests": map[string]any{"cpu": float64(2), "memory": "2Gi"}},
	}}, kubevirtConfig{})
	if v.CPURequest != 2000 || v.GuestMemory != 2<<30 || v.VCPUs != 1 {
		t.Errorf("got %+v", v)
	}
}

// On a cluster without KubeVirt, a refused read of virtualmachines is noise
// that sends the reader to grant something useless.
func TestKubeVirtPermissionsOnlyWhereInstalled(t *testing.T) {
	cs := fake.NewSimpleClientset()
	c := &Client{cs: cs}
	for _, p := range c.CheckPermissions(context.Background(), "") {
		if p.KubeVirt {
			t.Errorf("asked %s on a cluster without KubeVirt", p)
		}
	}
	cs.Resources = []*metav1.APIResourceList{{GroupVersion: "kubevirt.io/v1"}}
	var n int
	for _, p := range c.CheckPermissions(context.Background(), "") {
		if p.KubeVirt {
			n++
		}
	}
	if n != 3 {
		t.Errorf("want the three KubeVirt reads where it is installed, got %d", n)
	}
}
