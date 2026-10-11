package analyze

import (
	"fmt"

	"github.com/mfranzon/tare/internal/ceilings"
	"github.com/mfranzon/tare/internal/discover"
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
