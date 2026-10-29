package metrics

import (
	"fmt"
	"slices"
	"time"
)

// GPU signals, from NVIDIA's dcgm-exporter.
const (
	// SigGPUBusy is DCGM_FI_DEV_GPU_UTIL: the share of time any kernel was
	// running. It bounds use from ABOVE and nothing more. Zero proves the
	// device was idle; 100% proves nothing, because a single thread spinning
	// in one block reads 100% (measured on the lab's RTX 3060).
	SigGPUBusy Signal = "gpu-busy"
	// SigGPUSMActive is DCGM_FI_PROF_SM_ACTIVE: the fraction of the device's
	// SMs doing work, the only signal that says how MUCH of a GPU is used. It
	// is a profiling metric, which GeForce cards do not expose at all.
	SigGPUSMActive Signal = "gpu-sm-active"
	// SigGPUMem is DCGM_FI_DEV_FB_USED. It is what the allocator reserved
	// (vLLM pre-allocates, PyTorch caches its high-water mark), so it is a
	// floor for fit checks and never evidence that less would do.
	SigGPUMem Signal = "gpu-memory"
)

// GPUObservation is what the devices attributed to one container did.
type GPUObservation struct {
	// BusyMax and BusyAvg are SigGPUBusy as fractions, the worst of the
	// container's devices.
	BusyMax float64
	BusyAvg float64
	HasBusy bool

	// SMActiveAvg and SMActiveP95 are SigGPUSMActive, the highest of the
	// container's devices.
	SMActiveAvg float64
	SMActiveP95 float64
	HasSM       bool

	// MemMax is bytes, the largest device's. It is DEVICE memory in use, not
	// the container's: on the lab node an unheld GPU reads 8MiB of it, the
	// host's Xorg, and the pod that takes the device inherits that figure.
	MemMax float64
	HasMem bool

	// Devices are the device UUIDs attributed to the container in the window,
	// and IdleDevices those whose busy-time never left zero for as long as
	// the container held them.
	Devices     []string
	IdleDevices []string

	// FirstSeen and LastSeen bound the time the container was attributed any
	// device, in unix seconds.
	FirstSeen float64
	LastSeen  float64

	// SharedWith names other pods attributed the same device over an
	// overlapping span. A device's metrics cannot be split between the pods
	// using it, so a shared container's values above describe the whole
	// device and must not be read as its own.
	SharedWith []string
}

// GPUObserver is an optional Source capability, for the same reason
// NodeObserver is: the simulator has no GPU, and a stub returning a plausible
// zero would read as an idle device.
type GPUObserver interface {
	// ObserveGPUs attributes device metrics to containers. holders is the set
	// of live containers that request a GPU; the caller has it from the API
	// server and the metrics backend does not. Namespaces filter the result,
	// empty meaning all.
	ObserveGPUs(holders map[Key]bool, namespaces []string, window time.Duration) (map[Key]GPUObservation, error)
}

// gpuShareOverlap is how long two pods must both hold one device before they
// count as sharing it. A rollout hands a device from one pod to the next and
// the two series can overlap by a scrape or two; that is a hand-over, not a
// share. Chosen, not derived: long enough to clear scrape jitter at the
// exporter's 30s interval, short against any window worth reporting.
const gpuShareOverlap = 5 * 60

// gpuGroup is every label a DCGM series can name its owner or device by.
// Exporter-specific labels (instance, the exporter's own pod under
// honorLabels: false) are dropped so that an exporter restart does not split
// a device's history.
const gpuGroup = "UUID,exported_namespace,exported_pod,exported_container,namespace,pod,container"

// ObserveGPUs reads dcgm-exporter's series and attributes them to containers.
//
// Attribution is the hard part, and it is done here rather than in PromQL.
// Under a default ServiceMonitor (honorLabels: false) every series carries the
// EXPORTER's namespace and pod, and the pod holding the device appears only as
// exported_pod; with honorLabels: true it is the other way round, and nothing
// in a series says which. A plain namespace/pod/container join, the one every
// cAdvisor query uses, credits the GPU to the exporter. So: exported_* when
// present, otherwise namespace/pod/container only for a container known to
// hold a GPU. The exporter holds none, so it can never be credited with one.
// The cost is that under honorLabels: true a pod that is gone cannot be
// attributed; its history is lost rather than guessed.
func (p *Prom) ObserveGPUs(holders map[Key]bool, namespaces []string, window time.Duration) (map[Key]GPUObservation, error) {
	out := map[Key]GPUObservation{}
	if len(holders) == 0 {
		return out, nil
	}
	w := durationString(window)
	busy := "DCGM_FI_DEV_GPU_UTIL"
	sm := "DCGM_FI_PROF_SM_ACTIVE"

	type devKey struct {
		owner Key
		uuid  string
	}
	type devObs struct {
		busyMax, busyAvg, smAvg, smP95, mem float64
		hasBusy, hasSM, hasMem              bool
		first, last                         float64
	}
	devs := map[devKey]*devObs{}
	// at returns where a series' values go, nil when it is out of scope, and
	// whether it named an owner at all. The two differ under a namespace
	// filter, and the attribution control below must count owners, not
	// owners in scope, or `-n web` on a cluster whose GPUs live elsewhere
	// would condemn a working exporter.
	at := func(s sample) (*devObs, bool) {
		owner, ok := gpuOwner(s.labels, holders)
		if !ok {
			return nil, false
		}
		if !inNamespaces(owner.Namespace, namespaces) {
			return nil, true
		}
		k := devKey{owner, s.labels["UUID"]}
		d := devs[k]
		if d == nil {
			d = &devObs{}
			devs[k] = d
		}
		return d, true
	}

	queries := []struct {
		signal Signal
		expr   string
		reduce string
		assign func(*devObs, float64)
	}{
		// GPU_UTIL is 0-100; everything here is a fraction.
		{SigGPUBusy, fmt.Sprintf(`max_over_time(%s[%s])`, busy, w), "max",
			func(d *devObs, v float64) { d.busyMax = max(d.busyMax, v/100); d.hasBusy = true }},
		{"", fmt.Sprintf(`avg_over_time(%s[%s])`, busy, w), "max",
			func(d *devObs, v float64) { d.busyAvg = max(d.busyAvg, v/100) }},
		// Span from timestamps rather than sample counts: dcgm-exporter's
		// chart scrapes every 30s, not the 15s every count elsewhere assumes,
		// so a count would make every device look half covered.
		{"", fmt.Sprintf(`min_over_time(timestamp(%s)[%s:1m])`, busy, w), "min",
			func(d *devObs, v float64) {
				if d.first == 0 || v < d.first {
					d.first = v
				}
			}},
		{"", fmt.Sprintf(`max_over_time(timestamp(%s)[%s:1m])`, busy, w), "max",
			func(d *devObs, v float64) { d.last = max(d.last, v) }},
		{SigGPUSMActive, fmt.Sprintf(`avg_over_time(%s[%s])`, sm, w), "max",
			func(d *devObs, v float64) { d.smAvg = max(d.smAvg, v); d.hasSM = true }},
		{"", fmt.Sprintf(`quantile_over_time(0.95, %s[%s])`, sm, w), "max",
			func(d *devObs, v float64) { d.smP95 = max(d.smP95, v) }},
		// FB_USED is MiB.
		{SigGPUMem, fmt.Sprintf(`max_over_time(DCGM_FI_DEV_FB_USED[%s])`, w), "max",
			func(d *devObs, v float64) { d.mem = max(d.mem, v*(1<<20)); d.hasMem = true }},
	}

	var busySeries, busyAttributed int
	for _, q := range queries {
		expr := fmt.Sprintf(`%s by (%s) (%s)`, q.reduce, gpuGroup, q.expr)
		samples, err := p.query(expr)
		if q.signal != "" {
			p.health = append(p.health, SignalHealth{Signal: q.signal, Query: expr, Err: err, Series: len(samples)})
		}
		if err != nil {
			continue
		}
		for _, s := range samples {
			d, named := at(s)
			if q.signal == SigGPUBusy {
				busySeries++
				if named {
					busyAttributed++
				}
			}
			if d != nil {
				q.assign(d, s.value)
			}
		}
	}

	// Some series came back and not one names a container holding a GPU,
	// while such containers exist. That is an exporter that cannot see pods
	// (Kubernetes mode off, or no access to the kubelet's pod-resources
	// socket), not a fleet of idle GPUs, and it must not be read as one.
	if busySeries > 0 && busyAttributed == 0 {
		reason := fmt.Sprintf("dcgm-exporter returned %d device series and none names a pod that holds a GPU, "+
			"while %d containers request one. The exporter is not attributing devices to pods (Kubernetes mode off, "+
			"or no access to the kubelet pod-resources socket), so no container's GPU use is known", busySeries, len(holders))
		for i := range p.health {
			switch p.health[i].Signal {
			case SigGPUBusy, SigGPUSMActive, SigGPUMem:
				p.health[i].Suspect = reason
			}
		}
		return map[Key]GPUObservation{}, nil
	}

	// Pods per device, for sharing.
	holdersOf := map[string][]devKey{}
	for k := range devs {
		if k.uuid != "" {
			holdersOf[k.uuid] = append(holdersOf[k.uuid], k)
		}
	}

	for k, d := range devs {
		o := out[k.owner]
		o.BusyMax, o.BusyAvg = max(o.BusyMax, d.busyMax), max(o.BusyAvg, d.busyAvg)
		o.HasBusy = o.HasBusy || d.hasBusy
		o.SMActiveAvg, o.SMActiveP95 = max(o.SMActiveAvg, d.smAvg), max(o.SMActiveP95, d.smP95)
		o.HasSM = o.HasSM || d.hasSM
		o.MemMax, o.HasMem = max(o.MemMax, d.mem), o.HasMem || d.hasMem
		if d.first > 0 && (o.FirstSeen == 0 || d.first < o.FirstSeen) {
			o.FirstSeen = d.first
		}
		o.LastSeen = max(o.LastSeen, d.last)
		if k.uuid != "" {
			o.Devices = append(o.Devices, k.uuid)
			if d.hasBusy && d.busyMax == 0 {
				o.IdleDevices = append(o.IdleDevices, k.uuid)
			}
			for _, other := range holdersOf[k.uuid] {
				if other.owner.Pod == k.owner.Pod && other.owner.Namespace == k.owner.Namespace {
					continue
				}
				od := devs[other]
				if min(d.last, od.last)-max(d.first, od.first) > gpuShareOverlap {
					o.SharedWith = append(o.SharedWith, other.owner.Namespace+"/"+other.owner.Pod)
				}
			}
		}
		out[k.owner] = o
	}
	for k, o := range out {
		slices.Sort(o.Devices)
		slices.Sort(o.IdleDevices)
		slices.Sort(o.SharedWith)
		o.SharedWith = slices.Compact(o.SharedWith)
		out[k] = o
	}
	return out, nil
}

// gpuOwner reads the container a DCGM series is attributed to. See
// ObserveGPUs for why the plain labels are trusted only for known holders.
func gpuOwner(l map[string]string, holders map[Key]bool) (Key, bool) {
	if pod := l["exported_pod"]; pod != "" {
		k := Key{Namespace: l["exported_namespace"], Pod: pod, Container: l["exported_container"]}
		return k, k.Container != ""
	}
	k := keyOf(l)
	return k, holders[k]
}

func inNamespaces(ns string, namespaces []string) bool {
	return len(namespaces) == 0 || slices.Contains(namespaces, ns)
}
