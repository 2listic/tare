package discover

import (
	"context"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// KubeVirt runs a VM as a virt-launcher pod owned by a VirtualMachineInstance,
// which is in turn owned by the VirtualMachine people actually edit. The pod's
// resources are not anyone's manifest: KubeVirt derives them from the VM spec
// (CPU request = vCPUs / cpuAllocationRatio, memory = guest + overhead), so a
// recommendation written against the pod has nowhere to land. Everything here
// exists to read the VM in its own terms.

var (
	vmiGVR      = schema.GroupVersionResource{Group: "kubevirt.io", Version: "v1", Resource: "virtualmachineinstances"}
	vmGVR       = schema.GroupVersionResource{Group: "kubevirt.io", Version: "v1", Resource: "virtualmachines"}
	kubevirtGVR = schema.GroupVersionResource{Group: "kubevirt.io", Version: "v1", Resource: "kubevirts"}
)

// VMComputeContainer is the virt-launcher container that holds QEMU. The VM's
// sizing is attached to it alone; the pod's other containers (hotplug disks,
// containerDisk) are ordinary containers.
const VMComputeContainer = "compute"

// VMSpec is a VirtualMachine as tare needs it: what the guest was promised,
// what was authored, and what decides whether the VM can move.
type VMSpec struct {
	// VM is the VirtualMachine's name, empty for a VMI nobody owns. VMI is
	// the instance the pod belongs to; metrics are keyed by it.
	VM  string
	VMI string

	// VCPUs is sockets x cores x threads, what the guest sees.
	VCPUs int64
	// GuestMemory is the memory the guest was promised, in bytes.
	GuestMemory int64

	// The domain's explicit resources, as the running VMI has them. Zero when
	// unset; KubeVirt then derives the pod's from the topology.
	CPURequest int64 // millicores
	CPULimit   int64
	MemRequest int64 // bytes
	MemLimit   int64

	// AuthoredGuest, AuthoredMemRequest and AuthoredCPURequest are what the
	// VirtualMachine's own template set, which is what a patch must change.
	// AuthoredKnown is false when the VirtualMachine could not be read.
	AuthoredKnown      bool
	AuthoredGuest      bool
	AuthoredMemRequest int64
	AuthoredCPURequest int64

	// Instancetype names a VirtualMachineInstancetype the VM takes its CPU
	// and memory from. KubeVirt rejects a VM that sets either beside one, so
	// such a VM is resized by changing the instancetype, never the domain.
	Instancetype string

	DedicatedCPU          bool
	IsolateEmulatorThread bool
	// HugepageSize is set when the guest memory is backed by hugepages, which
	// are reserved whole on the node however little the guest uses.
	HugepageSize string
	// HostDevices is set when the VM uses GPUs, host devices or SR-IOV
	// interfaces: hardware on one node it cannot take with it.
	HostDevices bool
	// NodeSelector is the VMI's own spec.nodeSelector: what its author asked
	// for. KubeVirt copies it into the virt-launcher pod beside the selectors
	// it adds itself, and only these can pin a migratable VM.
	NodeSelector map[string]string
	// Affinity is set when the VMI's own spec.affinity is. KubeVirt copies it
	// into the virt-launcher pod and adds node-affinity terms of its own; only
	// the author's can pin a migratable VM.
	Affinity bool

	// EvictionStrategy is the VMI's own, or the cluster default when it sets
	// none. LiveMigratable is the VMI's condition; LiveMigratableKnown is
	// false when the condition was not reported.
	EvictionStrategy    string
	LiveMigratable      bool
	LiveMigratableKnown bool

	// MaxGuest and MaxSockets bound what a LiveUpdate rollout can hotplug.
	MaxGuest   int64
	MaxSockets int64
	// RolloutStrategy is the cluster's vmRolloutStrategy: "LiveUpdate" lets
	// some changes reach a running VM, anything else needs a restart.
	RolloutStrategy string

	// GuestOS is the guest agent's OS id, empty without an agent. Windows
	// touches every page at boot, so its RSS says nothing about use.
	GuestOS string

	// CPUAllocationRatio and MemoryOvercommit are the cluster's: a vCPU
	// costs 1/ratio of a core in the pod request, and the pod requests
	// guest x 100/overcommit of memory.
	CPUAllocationRatio int64
	MemoryOvercommit   int64
}

// Windows reports a Windows guest.
func (v VMSpec) Windows() bool {
	os := strings.ToLower(v.GuestOS)
	return strings.Contains(os, "windows") || strings.Contains(os, "mswindows")
}

// Migratable reports whether draining the VM's node would move it rather than
// shut it down: it must be able to live-migrate and be asked to.
func (v VMSpec) Migratable() bool {
	if !v.LiveMigratableKnown || !v.LiveMigratable {
		return false
	}
	switch v.EvictionStrategy {
	case "LiveMigrate", "LiveMigrateIfPossible":
		return true
	}
	return false
}

// GuestRequestShare is the part of the pod's memory request the guest
// accounts for: all of it, or less under memory overcommit.
func (v VMSpec) GuestRequestShare(guest int64) int64 {
	oc := v.MemoryOvercommit
	if oc <= 0 {
		oc = 100
	}
	return guest * 100 / oc
}

// kubevirtConfig is the cluster-wide KubeVirt CR's settings that change what a
// VM's pod requests.
type kubevirtConfig struct {
	cpuAllocationRatio int64
	memoryOvercommit   int64
	rolloutStrategy    string
	evictionStrategy   string
	read               bool
}

// loadKubeVirtConfig reads the KubeVirt CR, once. Missing means defaults.
func (x *workloadIndex) loadKubeVirtConfig(ctx context.Context) kubevirtConfig {
	if x.kvConf.read || x.c.dyn == nil {
		return x.kvConf
	}
	x.kvConf = kubevirtConfig{read: true, cpuAllocationRatio: 10, memoryOvercommit: 100}
	l, err := x.c.dyn.Resource(kubevirtGVR).Namespace(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		if !errors.IsNotFound(err) {
			x.c.warn("the KubeVirt configuration could not be read (%v): tare assumes the defaults "+
				"(cpuAllocationRatio 10, no memory overcommit, VM changes need a restart)", err)
		}
		return x.kvConf
	}
	for _, item := range l.Items {
		conf := mapAt(item.Object, "spec", "configuration")
		dev := mapAt(conf, "developerConfiguration")
		if n := intAt(dev, "cpuAllocationRatio"); n > 0 {
			x.kvConf.cpuAllocationRatio = n
		}
		if n := intAt(dev, "memoryOvercommit"); n > 0 {
			x.kvConf.memoryOvercommit = n
		}
		x.kvConf.rolloutStrategy = unstructuredString(conf, "vmRolloutStrategy")
		x.kvConf.evictionStrategy = unstructuredString(conf, "evictionStrategy")
		break // one KubeVirt per cluster
	}
	return x.kvConf
}

// listVMIs indexes VirtualMachineInstances in one namespace scope.
func (x *workloadIndex) listVMIs(ctx context.Context, ns string) error {
	if x.c.dyn == nil {
		return nil
	}
	l, err := x.c.dyn.Resource(vmiGVR).Namespace(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return nil // no KubeVirt on this cluster: nothing to say
		}
		return err
	}
	conf := x.loadKubeVirtConfig(ctx)
	for _, item := range l.Items {
		key := item.GetNamespace() + "/" + item.GetName()
		for _, ref := range item.GetOwnerReferences() {
			if ref.Controller != nil && *ref.Controller && ref.Kind == "VirtualMachine" {
				x.vmiOwner[key] = "VirtualMachine/" + ref.Name
			}
		}
		spec := vmSpecFrom(mapAt(item.Object, "spec"), conf)
		spec.VMI = item.GetName()
		if owner, ok := x.vmiOwner[key]; ok {
			spec.VM = strings.TrimPrefix(owner, "VirtualMachine/")
		}
		status := mapAt(item.Object, "status")
		if conds, ok := status["conditions"].([]any); ok {
			for _, c := range conds {
				m, _ := c.(map[string]any)
				if unstructuredString(m, "type") == "LiveMigratable" {
					spec.LiveMigratableKnown = true
					spec.LiveMigratable = unstructuredString(m, "status") == "True"
				}
			}
		}
		spec.GuestOS = unstructuredString(status, "guestOSInfo", "id")
		if spec.GuestOS == "" {
			spec.GuestOS = unstructuredString(status, "guestOSInfo", "name")
		}
		x.vmis[key] = spec
	}
	return nil
}

// listVMs indexes what each VirtualMachine's template authored.
func (x *workloadIndex) listVMs(ctx context.Context, ns string) error {
	if x.c.dyn == nil {
		return nil
	}
	l, err := x.c.dyn.Resource(vmGVR).Namespace(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return err
	}
	for _, item := range l.Items {
		spec := mapAt(item.Object, "spec")
		domain := mapAt(spec, "template", "spec", "domain")
		a := vmAuthored{
			guest:        qty(domain, "memory", "guest") != "",
			memRequest:   quantityBytes(qty(domain, "resources", "requests", "memory")),
			cpuRequest:   quantityMilli(qty(domain, "resources", "requests", "cpu")),
			instancetype: unstructuredString(spec, "instancetype", "name"),
		}
		x.vms[item.GetNamespace()+"/"+item.GetName()] = a
	}
	return nil
}

type vmAuthored struct {
	guest        bool
	memRequest   int64
	cpuRequest   int64
	instancetype string
}

// vmFor returns the VM behind a virt-launcher pod, or nil when the pod is not
// one or its VMI could not be read.
func (x *workloadIndex) vmFor(ctx context.Context, pod *corev1.Pod) *VMSpec {
	ref := metav1.GetControllerOf(pod)
	if ref == nil || ref.Kind != "VirtualMachineInstance" {
		return nil
	}
	x.load(ctx, "VirtualMachineInstance")
	spec, ok := x.vmis[pod.Namespace+"/"+ref.Name]
	if !ok {
		return nil
	}
	if spec.VM != "" {
		x.load(ctx, "VirtualMachine")
		if a, ok := x.vms[pod.Namespace+"/"+spec.VM]; ok {
			spec.AuthoredKnown = true
			spec.AuthoredGuest = a.guest
			spec.AuthoredMemRequest = a.memRequest
			spec.AuthoredCPURequest = a.cpuRequest
			spec.Instancetype = a.instancetype
		}
	}
	return &spec
}

// vmSpecFrom reads a VMI (or VM template) spec.
func vmSpecFrom(spec map[string]any, conf kubevirtConfig) VMSpec {
	domain := mapAt(spec, "domain")
	cpu := mapAt(domain, "cpu")
	mem := mapAt(domain, "memory")
	res := mapAt(domain, "resources")
	v := VMSpec{
		CPURequest:            quantityMilli(qty(res, "requests", "cpu")),
		CPULimit:              quantityMilli(qty(res, "limits", "cpu")),
		MemRequest:            quantityBytes(qty(res, "requests", "memory")),
		MemLimit:              quantityBytes(qty(res, "limits", "memory")),
		DedicatedCPU:          boolAt(cpu, "dedicatedCpuPlacement"),
		IsolateEmulatorThread: boolAt(cpu, "isolateEmulatorThread"),
		HugepageSize:          unstructuredString(mem, "hugepages", "pageSize"),
		MaxGuest:              quantityBytes(qty(mem, "maxGuest")),
		MaxSockets:            intAt(cpu, "maxSockets"),
		EvictionStrategy:      unstructuredString(spec, "evictionStrategy"),
		RolloutStrategy:       conf.rolloutStrategy,
		CPUAllocationRatio:    conf.cpuAllocationRatio,
		MemoryOvercommit:      conf.memoryOvercommit,
	}
	if v.EvictionStrategy == "" {
		v.EvictionStrategy = conf.evictionStrategy
	}
	v.Affinity = len(mapAt(spec, "affinity")) > 0
	if sel := mapAt(spec, "nodeSelector"); len(sel) > 0 {
		v.NodeSelector = map[string]string{}
		for k, val := range sel {
			v.NodeSelector[k], _ = val.(string)
		}
	}
	sockets, cores, threads := max(intAt(cpu, "sockets"), 1), max(intAt(cpu, "cores"), 1), max(intAt(cpu, "threads"), 1)
	v.VCPUs = sockets * cores * threads
	v.GuestMemory = quantityBytes(qty(mem, "guest"))
	if v.GuestMemory == 0 {
		// KubeVirt's own default: the guest gets what the domain requests.
		v.GuestMemory = v.MemRequest
	}
	devices := mapAt(domain, "devices")
	for _, k := range []string{"gpus", "hostDevices"} {
		if l, ok := devices[k].([]any); ok && len(l) > 0 {
			v.HostDevices = true
		}
	}
	if ifs, ok := devices["interfaces"].([]any); ok {
		for _, i := range ifs {
			if m, _ := i.(map[string]any); m != nil && m["sriov"] != nil {
				v.HostDevices = true
			}
		}
	}
	return v
}

// mapAt walks nested maps, returning nil when any step is missing.
func mapAt(obj map[string]any, path ...string) map[string]any {
	cur := obj
	for _, p := range path {
		next, ok := cur[p].(map[string]any)
		if !ok {
			return nil
		}
		cur = next
	}
	return cur
}

// intAt reads an integer field that JSON may have decoded as int64 or float64.
func intAt(obj map[string]any, key string) int64 {
	switch v := obj[key].(type) {
	case int64:
		return v
	case float64:
		return int64(v)
	case int:
		return int64(v)
	case string:
		n, _ := strconv.ParseInt(v, 10, 64)
		return n
	}
	return 0
}

// qty reads a resource quantity, which a CRD declared int-or-string stores as
// either: `cpu: 2` arrives as a number, `cpu: "2"` as a string.
func qty(obj map[string]any, path ...string) string {
	if len(path) == 0 {
		return ""
	}
	m := mapAt(obj, path[:len(path)-1]...)
	switch v := m[path[len(path)-1]].(type) {
	case string:
		return v
	case int64:
		return strconv.FormatInt(v, 10)
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case int:
		return strconv.Itoa(v)
	}
	return ""
}

func boolAt(obj map[string]any, key string) bool {
	b, _ := obj[key].(bool)
	return b
}

func quantityBytes(s string) int64 {
	if s == "" {
		return 0
	}
	q, err := resource.ParseQuantity(s)
	if err != nil {
		return 0
	}
	return q.Value()
}

func quantityMilli(s string) int64 {
	if s == "" {
		return 0
	}
	q, err := resource.ParseQuantity(s)
	if err != nil {
		return 0
	}
	return q.MilliValue()
}
