package analyze_test

import (
	"slices"
	"testing"
	"time"

	"github.com/2listic/tare/internal/analyze"
	"github.com/2listic/tare/internal/discover"
	"github.com/2listic/tare/internal/metrics"
)

const gpuWindow = 7 * 24 * time.Hour

// gpuCase is one container on the lab's single-GPU node, observed for the
// whole window with flat memory. cpuCores is its steady CPU; gpu is what
// dcgm-exporter attributed to it, nil for nothing.
type gpuCase struct {
	gpus     int64
	resource string
	cpuCores float64
	psi      float64
	gpu      *metrics.GPUObservation
	shared   bool // the node's GFD labels declare time-slicing
}

func runGPU(t *testing.T, c gpuCase) analyze.Finding {
	t.Helper()
	if c.resource == "" && c.gpus > 0 {
		c.resource = "nvidia.com/gpu"
	}
	now := float64(time.Now().Unix())
	from := now - gpuWindow.Seconds()
	k := metrics.Key{Namespace: "gpu-lab", Pod: "worker-0", Container: "worker"}
	con := discover.Container{
		Namespace: k.Namespace, Pod: k.Pod, Name: k.Container,
		Workload: "Deployment/worker", WorkloadKind: "Deployment", Node: "gpu-0",
		CPURequest: 1000, MemRequest: 256 << 20, MemLimit: 256 << 20,
		GPUs: c.gpus, GPUResource: c.resource,
	}
	o := &metrics.Observation{
		Key: k, CPUUsageP95: c.cpuCores, CPUUsageMax: c.cpuCores, CPUSteadyMax: c.cpuCores,
		MemWorkingSet: 64 << 20, MemP95: 64 << 20, MemEarly: 64 << 20, MemLate: 64 << 20,
		PSIWaitingAvg: c.psi, PSIWaitingMax: c.psi,
		Samples: int(gpuWindow.Seconds() / 15), FirstSeen: from, LastSeen: now,
		GPU: c.gpu,
	}
	for _, s := range []metrics.Signal{metrics.SigCPUUsage, metrics.SigCPUPeak, metrics.SigMemory, metrics.SigMemP95,
		metrics.SigPSI, metrics.SigPSIAvg, metrics.SigMemTrend, metrics.SigOOM, metrics.SigRestarts} {
		o.Mark(s)
	}
	node := discover.Node{Name: "gpu-0", Cores: 16, AllocCPU: 16000, AllocMem: 15 << 30, Schedulable: true,
		Arch: "amd64", GPUs: 1, GPUResource: "nvidia.com/gpu", GPUShared: c.shared}
	fs := analyze.RunWithCluster([]discover.Container{con}, map[metrics.Key]*metrics.Observation{k: o},
		analyze.Cluster{Nodes: discover.Inventory{Nodes: []discover.Node{node}}}, gpuWindow, analyze.DefaultThresholds())
	if len(fs) != 1 {
		t.Fatalf("got %d findings", len(fs))
	}
	return fs[0]
}

// fullSpan is device use seen for the whole window.
func fullSpan(g metrics.GPUObservation) *metrics.GPUObservation {
	now := float64(time.Now().Unix())
	g.FirstSeen, g.LastSeen = now-gpuWindow.Seconds(), now
	g.HasBusy = true
	if len(g.Devices) == 0 {
		g.Devices = []string{"GPU-0"}
	}
	return &g
}

func codes(f analyze.Finding) []analyze.Code {
	var out []analyze.Code
	for _, r := range f.Reasons {
		out = append(out, r.Code)
	}
	return out
}

func has(f analyze.Finding, c analyze.Code) bool { return slices.Contains(codes(f), c) }

// gpu-sleeper in the lab: holds a device, never launches a kernel.
func TestSleeperIsGPUIdle(t *testing.T) {
	f := runGPU(t, gpuCase{gpus: 1, gpu: fullSpan(metrics.GPUObservation{IdleDevices: []string{"GPU-0"}})})
	if f.Verdict != analyze.Idle || !has(f, analyze.GPUIdle) {
		t.Fatalf("verdict %s codes %v, want IDLE with GPU_IDLE", f.Verdict, codes(f))
	}
}

// The regression this pass exists for. Before it, isIdle read CPU alone, and a
// process waiting on its kernels at 1m of CPU was IDLE: "delete the GPU job".
// The counterfactual - the same container with no GPU - must still be IDLE,
// or this test passes because something else stopped it.
func TestGPUBoundWithQuietCPUIsNotIdle(t *testing.T) {
	busy := gpuCase{gpus: 1, cpuCores: 0.001, gpu: fullSpan(metrics.GPUObservation{BusyMax: 1, BusyAvg: 0.9})}
	f := runGPU(t, busy)
	if f.Verdict == analyze.Idle {
		t.Fatalf("a GPU busy 90%% of the time was called IDLE from its CPU: %v", codes(f))
	}
	without := runGPU(t, gpuCase{cpuCores: 0.001})
	if without.Verdict != analyze.Idle {
		t.Fatalf("counterfactual: the same container without a GPU is %s, not IDLE, so the test above discriminates nothing", without.Verdict)
	}
}

// A GPU starved of host CPU looks idle, not CPU-pressured, so cutting a GPU
// workload's CPU degrades the device and hides why.
func TestGPUWorkloadKeepsItsCPU(t *testing.T) {
	f := runGPU(t, gpuCase{gpus: 1, cpuCores: 0.05, gpu: fullSpan(metrics.GPUObservation{BusyMax: 1, BusyAvg: 0.9})})
	if f.RecCPU != 1000 || !has(f, analyze.GPUCPUHeld) {
		t.Errorf("rec CPU %dm codes %v, want the 1000m request held with GPU_CPU_HELD", f.RecCPU, codes(f))
	}
	if has(f, analyze.CPUUnderused) {
		t.Error("a held CPU request was also reported as wasted")
	}
	without := runGPU(t, gpuCase{cpuCores: 0.05})
	if without.RecCPU >= 1000 {
		t.Fatalf("counterfactual: without a GPU the request is not lowered (%dm), so the hold is untested", without.RecCPU)
	}
}

func TestUnmeasuredGPUIsSaidAndNotIdle(t *testing.T) {
	f := runGPU(t, gpuCase{gpus: 1})
	if f.Verdict == analyze.Idle {
		t.Error("a GPU nobody measured was called IDLE")
	}
	if !has(f, analyze.GPUNotMeasured) {
		t.Errorf("codes %v, want GPU_NOT_MEASURED", codes(f))
	}
}

// gpu-spinner on the lab card: 100% busy, no SM activity exported. The one
// verdict it must never get is a clean bill of health.
func TestSpinnerWithoutSMIsUnverified(t *testing.T) {
	f := runGPU(t, gpuCase{gpus: 1, cpuCores: 0.9, gpu: fullSpan(metrics.GPUObservation{BusyMax: 1, BusyAvg: 0.985})})
	if !has(f, analyze.GPUBusyUnverified) || has(f, analyze.GPUUnderused) {
		t.Errorf("codes %v, want GPU_BUSY_UNVERIFIED and no underuse claim", codes(f))
	}
}

// The same spinner on a datacenter card, which exports SM activity: now the
// underuse is measured.
func TestSpinnerWithSMIsUnderused(t *testing.T) {
	g := fullSpan(metrics.GPUObservation{BusyMax: 1, BusyAvg: 0.985, SMActiveAvg: 0.03, SMActiveP95: 0.034, HasSM: true})
	f := runGPU(t, gpuCase{gpus: 1, cpuCores: 0.9, gpu: g})
	if !has(f, analyze.GPUUnderused) || has(f, analyze.GPUBusyUnverified) {
		t.Errorf("codes %v, want GPU_UNDERUSED from SM activity", codes(f))
	}
}

// Busy-time bounds use from above, so a low one proves underuse even on a card
// with no SM activity.
func TestLowBusyTimeProvesUnderuse(t *testing.T) {
	f := runGPU(t, gpuCase{gpus: 1, cpuCores: 0.9, gpu: fullSpan(metrics.GPUObservation{BusyMax: 0.6, BusyAvg: 0.05})})
	if !has(f, analyze.GPUUnderused) {
		t.Errorf("codes %v, want GPU_UNDERUSED from a 5%% busy mean", codes(f))
	}
}

func TestShortGPUObservationIsNotIdle(t *testing.T) {
	now := float64(time.Now().Unix())
	g := &metrics.GPUObservation{HasBusy: true, Devices: []string{"GPU-0"}, IdleDevices: []string{"GPU-0"},
		FirstSeen: now - 3600, LastSeen: now}
	f := runGPU(t, gpuCase{gpus: 1, gpu: g})
	if f.Verdict == analyze.Idle || has(f, analyze.GPUIdle) {
		t.Errorf("an hour of silence in a week was called idle: %s %v", f.Verdict, codes(f))
	}
}

// A time-sliced device's numbers are the device's. Zero busy-time on it would
// still mean idle, but which pod's metrics carry it is unverified.
func TestSharedDeviceIsNotAttributed(t *testing.T) {
	g := fullSpan(metrics.GPUObservation{IdleDevices: []string{"GPU-0"}})
	f := runGPU(t, gpuCase{gpus: 1, shared: true, gpu: g})
	if f.Verdict == analyze.Idle || !has(f, analyze.GPUSharedDevice) {
		t.Errorf("verdict %s codes %v, want GPU_SHARED_UNATTRIBUTED and not IDLE", f.Verdict, codes(f))
	}
	g2 := fullSpan(metrics.GPUObservation{IdleDevices: []string{"GPU-0"}, SharedWith: []string{"gpu-lab/other-0"}})
	if f := runGPU(t, gpuCase{gpus: 1, gpu: g2}); !has(f, analyze.GPUSharedDevice) {
		t.Errorf("sharing seen in the metrics was not reported: %v", codes(f))
	}
}

func TestMIGSliceGetsNoUsageVerdict(t *testing.T) {
	g := fullSpan(metrics.GPUObservation{IdleDevices: []string{"MIG-0"}})
	f := runGPU(t, gpuCase{gpus: 1, resource: "nvidia.com/mig-1g.10gb", gpu: g})
	if f.Verdict == analyze.Idle || has(f, analyze.GPUIdle) || !has(f, analyze.GPUMIGUnverified) {
		t.Errorf("verdict %s codes %v, want GPU_MIG_UNVERIFIED and no idle claim", f.Verdict, codes(f))
	}
}

func TestUnusedDeviceIsCounted(t *testing.T) {
	g := fullSpan(metrics.GPUObservation{BusyMax: 1, BusyAvg: 0.7, Devices: []string{"GPU-0", "GPU-1"}, IdleDevices: []string{"GPU-1"}})
	f := runGPU(t, gpuCase{gpus: 2, cpuCores: 0.9, gpu: g})
	for _, r := range f.Reasons {
		if r.Code == analyze.GPUDeviceUnused {
			if r.Observed != 1 || r.Against != 2 {
				t.Errorf("GPU_DEVICE_UNUSED observed %v against %v, want 1 of 2", r.Observed, r.Against)
			}
			return
		}
	}
	t.Errorf("codes %v, want GPU_DEVICE_UNUSED", codes(f))
}

func TestStarvedHostMakesAnUnderusedGPU(t *testing.T) {
	g := fullSpan(metrics.GPUObservation{BusyMax: 0.5, BusyAvg: 0.1})
	f := runGPU(t, gpuCase{gpus: 1, cpuCores: 0.95, psi: 0.2, gpu: g})
	if !has(f, analyze.CPUPressure) {
		t.Fatalf("fixture is not CPU-starved (%v), so this test checks nothing", codes(f))
	}
	if !has(f, analyze.GPUCPUBound) {
		t.Errorf("codes %v, want GPU_CPU_BOUND", codes(f))
	}
}
