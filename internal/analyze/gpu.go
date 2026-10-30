package analyze

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/2listic/tare/internal/ceilings"
	"github.com/2listic/tare/internal/discover"
	"github.com/2listic/tare/internal/metrics"
)

// Inference servers break the assumption the rest of this package rests on.
//
// Everywhere else, observed usage is evidence about demand and the
// recommendation is a number. Here the observation is an artifact of a startup
// flag, and there is no number to recommend: Kubernetes allocates accelerators
// as whole integer devices, so a pod either gets a GPU or it does not. Every
// finding below is therefore a fit check or a configuration defect, and none
// of them changes RecCPU or RecMem.

// applyInference adds the GPU findings. It runs after the ordinary
// recommendation so that a workload with no inference configuration is
// completely unaffected.
func applyInference(f *Finding, inv discover.Inventory) {
	vram, blocked := ceilings.VRAMFloor(f.Ceilings)
	util, hasUtil := ceilings.MemUtilFraction(f.Ceilings)
	isInference := vram > 0 || blocked || hasUtil || hasVRAMCeiling(f.Ceilings)
	if !isInference {
		return
	}

	device, deviceKnown := gpuMemAmong(f.Nodes, inv)
	product := gpuProductAmong(f.Nodes, inv)

	// An inference server holding no accelerator is either running on the CPU
	// at a fraction of the speed, or crash-looping. Both are invisible in a
	// sizing report that only looks at CPU and memory.
	if f.GPUs == 0 {
		f.Reasons = append(f.Reasons, note(GPUNotRequested, "this container is configured as an inference server but requests no GPU. "+
			"Ollama and llama.cpp will fall back to CPU and run one to two orders of magnitude slower; vLLM and TGI will fail to start. "+
			"Neither outcome is visible in CPU and memory metrics, which is why no amount of right-sizing would surface it."))
		return
	}

	// The node's memory label is the whole device's. A slice has a fraction
	// of it, so a fit against the label passes models the slice cannot load.
	if discover.IsMIGResource(f.GPUResource) {
		f.Reasons = append(f.Reasons, about(VRAMMIGUnchecked, f.GPUResource, fmt.Sprintf(
			"this container holds a MIG slice (%s), and the node reports only the whole device's memory, so the fit is not checked. "+
				"Checking it against the device would pass configurations the slice cannot load.", f.GPUResource)))
		return
	}

	if !deviceKnown {
		f.Reasons = append(f.Reasons, measured(GPUMemUnknown, float64(f.GPUs), 0, Count, fmt.Sprintf(
			"this container holds %d GPU(s), but no node it ran on reports a device memory size, so tare cannot check whether its configuration fits. "+
				"The NVIDIA GPU Operator and node-feature-discovery set nvidia.com/gpu.memory; a hand-installed device plugin does not. Install one, or check the fit by hand.",
			f.GPUs)))
		return
	}

	if blocked {
		f.Reasons = append(f.Reasons, about(VRAMUnresolved, product, fmt.Sprintf(
			"a VRAM requirement in this container's configuration could not be resolved, so the fit against its %s (%s) is unknown and is not guessed",
			product, HumanBytes(device))))
		return
	}

	// The pool a server reserves is a fraction of the device, so the same
	// manifest means different things on different hardware. That is exactly
	// the case a spec-only tool gets wrong and a cluster-aware one does not.
	pool := device
	if hasUtil {
		pool = int64(float64(device) * util)
	}

	switch {
	case vram > device:
		f.Reasons = append(f.Reasons, measuredAbout(VRAMOverDevice, product, float64(vram), float64(device), Bytes, fmt.Sprintf(
			"REFUSED: the model weights need about %s and the %s has %s. This cannot load, whatever the requests say. "+
				"Use a smaller model, quantize it, or shard it across devices with tensor parallelism.",
			HumanBytes(vram), product, HumanBytes(device))))
	case hasUtil && vram > pool:
		f.Reasons = append(f.Reasons, measuredAbout(VRAMOverPool, product, float64(vram), float64(pool), Bytes, fmt.Sprintf(
			"REFUSED: the weights need about %s but the server reserves only %.0f%% of the %s, which is %s. "+
				"The process will fail at startup while the device itself has room: raise the utilisation fraction rather than the memory limit.",
			HumanBytes(vram), util*100, product, HumanBytes(pool))))
	default:
		left := pool - vram
		f.Reasons = append(f.Reasons, measuredAbout(VRAMFits, product, float64(vram), float64(pool), Bytes, fmt.Sprintf(
			"fits: weights need about %s of the %s reserved on a %s (%s device), leaving roughly %s for KV cache, activations and the CUDA context. "+
				"Concurrency is bounded by that remainder, not by the CPU and memory requests in this report.",
			HumanBytes(vram), HumanBytes(pool), product, HumanBytes(device), HumanBytes(left))))
	}

	// Replicas sharing one device is the failure that looks like a crash loop.
	if hasUtil && f.Pods > 1 {
		total := util * float64(f.Pods)
		if total > 1 {
			f.Reasons = append(f.Reasons, measured(GPUOversubscribed, total*100, 100, Percent, fmt.Sprintf(
				"%d replicas each reserving %.0f%% of a device need %.0f%% in total. If any two land on the same GPU the second will not start; whether they do is up to the scheduler, so this may be working today by luck.",
				f.Pods, util*100, total*100)))
		}
	}
}

func hasVRAMCeiling(cs []ceilings.Ceiling) bool {
	for _, c := range cs {
		if c.Resource == ceilings.VRAM {
			return true
		}
	}
	return false
}

// gpuMemAmong returns the SMALLEST device memory across the nodes a workload
// ran on. The smallest is the one the fit check has to pass: a replica that
// lands on the small card is the one that fails.
func gpuMemAmong(names []string, inv discover.Inventory) (int64, bool) {
	var out int64
	for _, name := range names {
		n, ok := inv.Get(name)
		if !ok || n.GPUMemBytes <= 0 {
			continue
		}
		if out == 0 || n.GPUMemBytes < out {
			out = n.GPUMemBytes
		}
	}
	return out, out > 0
}

func gpuProductAmong(names []string, inv discover.Inventory) string {
	for _, name := range names {
		if n, ok := inv.Get(name); ok && n.GPUProduct != "" {
			return n.GPUProduct
		}
	}
	return "GPU"
}

// Measured device use.
//
// Every metric here is trusted only in the direction it bounds. Busy-time is
// the share of time any kernel ran: zero proves a device idle and a low mean
// proves it underused, but a high one proves nothing, because a single thread
// in one block reads 100% (measured on the lab's RTX 3060). Only SM activity
// says how much of a device is used, and consumer cards do not export it.
// None of these findings changes a recommendation except holdCPUForGPU, and
// none of them is ever written to a patch: a GPU count is a scheduling and
// parallelism decision, not a size.

// markGPUSharing records whether this workload's devices were shared, from
// the metrics or from the node's own sharing configuration. It runs before
// decide, because a shared device's numbers are the device's, and isIdle must
// not read them as this container's.
func markGPUSharing(f *Finding, inv discover.Inventory) {
	f.GPUShared = len(f.GPUSharedWith) > 0
	for _, name := range f.Nodes {
		if n, ok := inv.Get(name); ok && n.GPUShared && f.GPUs > 0 {
			f.GPUShared = true
		}
	}
}

// gpuCovered reports whether device use was seen for enough of the window for
// silence to count, the same guard isIdle applies to CPU.
func gpuCovered(f *Finding, window time.Duration, th Thresholds) bool {
	return f.GPULastSeen > f.GPUFirstSeen && f.GPULastSeen-f.GPUFirstSeen >= window.Seconds()*th.IdleMinCoverage
}

// gpuProvedIdle is the one GPU conclusion strong enough to let a GPU workload
// be called IDLE: a whole, unshared device whose busy-time never left zero
// for most of the window.
func gpuProvedIdle(f *Finding, window time.Duration, th Thresholds) bool {
	return f.GPUs > 0 && !discover.IsMIGResource(f.GPUResource) && !f.GPUShared &&
		f.hasSignal(metrics.SigGPUBusy) && f.GPUBusyMax == 0 && gpuCovered(f, window, th)
}

// holdCPUForGPU keeps a GPU workload's CPU request where it is. A GPU fed too
// little CPU (a data loader, a tokenizer, the host side of every launch) does
// not show CPU pressure, it shows an idle GPU, so the cut would degrade the
// expensive device and then make its own evidence look like waste. A device
// proven idle has nothing to starve.
func holdCPUForGPU(f *Finding, window time.Duration, th Thresholds) {
	if f.GPUs == 0 || f.RecCPU == 0 || f.RecCPU >= f.CPURequest || gpuProvedIdle(f, window, th) {
		return
	}
	f.cpuHeldForGPU = true
	f.Reasons = append(f.Reasons, measured(GPUCPUHeld, float64(f.RecCPU), float64(f.CPURequest), Millicores, fmt.Sprintf(
		"usage sizes the CPU request at %dm, below its %dm, and it is held: this container drives a GPU, and a GPU short of CPU "+
			"shows up as an idle device rather than as CPU pressure, so the cut would slow the expensive part and hide why.",
		f.RecCPU, f.CPURequest)))
	f.RecCPU = f.CPURequest
	f.RecCPULimit = max(f.RecCPULimit, f.CPULimit, f.RecCPU)
}

// applyGPUUsage adds what the devices measurably did.
func applyGPUUsage(f *Finding, window time.Duration, th Thresholds) {
	if f.GPUs == 0 {
		return
	}
	switch {
	case discover.IsMIGResource(f.GPUResource):
		f.Reasons = append(f.Reasons, about(GPUMIGUnverified, f.GPUResource,
			"this container holds a MIG slice. How dcgm-exporter attributes a slice's metrics has not been verified against MIG hardware, "+
				"so no usage verdict is drawn from them: a device-wide figure read as the slice's would be the clean wrong answer."))
		return
	case f.GPUShared:
		with := "the node shares each device between pods (time-slicing or MPS)"
		if len(f.GPUSharedWith) > 0 {
			slices.Sort(f.GPUSharedWith)
			with = "it was used at the same time by " + strings.Join(f.GPUSharedWith, ", ")
		}
		f.Reasons = append(f.Reasons, note(GPUSharedDevice, "this container's GPU is shared: "+with+". "+
			"Device metrics describe the whole device and nothing measured splits them between the pods using it, so no GPU usage is attributed to this container."))
		return
	case !f.hasSignal(metrics.SigGPUBusy):
		f.Reasons = append(f.Reasons, measured(GPUNotMeasured, float64(f.GPUs), 0, Count, fmt.Sprintf(
			"this container holds %d GPU allocation(s) and no device metric names it, so whether it uses them is unknown. "+
				"dcgm-exporter in Kubernetes mode attributes devices to pods; without it, an idle GPU and a busy one look the same here.", f.GPUs)))
		return
	}

	span := f.GPULastSeen - f.GPUFirstSeen
	covered := gpuCovered(f, window, th)
	if f.GPUBusyMax == 0 {
		if covered {
			f.Reasons = append(f.Reasons, measured(GPUIdle, 0, span, Count, fmt.Sprintf(
				"no kernel ran on this container's %d GPU allocation(s) in %.1fh of observation: busy-time never left zero. "+
					"Busy-time can only overstate use, so zero is proof. The device is reserved and unused, and nothing else can schedule on it.",
				f.GPUs, span/3600)))
		} else {
			f.Reasons = append(f.Reasons, measured(GPUNotMeasured, span, window.Seconds()*th.IdleMinCoverage, Count, fmt.Sprintf(
				"no kernel ran on this container's GPU, but device use was seen for only %.1fh of the %.0fh window, too little for silence to count as idleness",
				span/3600, window.Hours())))
		}
		return
	}

	if covered && f.GPUIdleDevices > 0 {
		f.Reasons = append(f.Reasons, measured(GPUDeviceUnused, float64(f.GPUIdleDevices), float64(f.GPUs), Count, fmt.Sprintf(
			"each pod holds %d GPUs and %d of them never ran a kernel in %.1fh: the workload ran on %d. "+
				"Requesting fewer usually also means changing its parallelism (tensor-parallel size, world size), so this is reported, not patched.",
			f.GPUs, f.GPUIdleDevices, span/3600, f.GPUs-int64(f.GPUIdleDevices))))
	}

	underused := false
	switch {
	case f.hasSignal(metrics.SigGPUSMActive):
		if f.GPUSMActiveP95 < th.GPUUnderused {
			underused = true
			f.Reasons = append(f.Reasons, measured(GPUUnderused, f.GPUSMActiveP95, th.GPUUnderused, Ratio, fmt.Sprintf(
				"SM activity p95 is %.0f%% (mean %.0f%%) against a %.0f%% threshold: most of the device sat unused even at its busiest. "+
					"A candidate for a smaller GPU, a MIG slice, or sharing.",
				f.GPUSMActiveP95*100, f.GPUSMActiveAvg*100, th.GPUUnderused*100)))
		}
	case f.GPUBusyAvg < th.GPUUnderused:
		underused = true
		f.Reasons = append(f.Reasons, measured(GPUUnderused, f.GPUBusyAvg, th.GPUUnderused, Ratio, fmt.Sprintf(
			"a kernel was running only %.0f%% of the time on average (peak %.0f%%), against a %.0f%% threshold. "+
				"Busy-time overstates use, so the device did at most this much work. A candidate for sharing.",
			f.GPUBusyAvg*100, f.GPUBusyMax*100, th.GPUUnderused*100)))
	default:
		f.Reasons = append(f.Reasons, measured(GPUBusyUnverified, f.GPUBusyAvg, 0, Ratio, fmt.Sprintf(
			"a kernel was running %.0f%% of the time on average, which proves only that something was launched: one thread in one block reads 100%%. "+
				"This GPU does not export SM activity (consumer cards never do), so how much of it is used cannot be measured and underuse cannot be ruled out.",
			f.GPUBusyAvg*100)))
	}

	if underused && slices.ContainsFunc(f.Reasons, func(r Reason) bool { return r.Code == CPUPressure || r.Code == CPUThrottled }) {
		f.Reasons = append(f.Reasons, note(GPUCPUBound,
			"the GPU is underused while the container is starved of CPU: the device is probably waiting on the host. Raise CPU before concluding the GPU is oversized."))
	}
}
