package analyze

import (
	"github.com/2listic/tare/internal/ceilings"
	"github.com/2listic/tare/internal/discover"
	"github.com/2listic/tare/internal/metrics"
)

// Finding is one container's verdict, evidence and recommendation.
type Finding struct {
	Namespace string
	Workload  string
	Container string
	Image     string
	QoS       string
	Pods      int

	// GPUs is the accelerator count this container requests, per pod, and
	// GPUResource what they are requested as. A MIG slice is not a device.
	GPUs        int64
	GPUResource string

	// GPU use from dcgm-exporter, worst case across pods. Check Have for
	// SigGPUBusy, SigGPUSMActive and SigGPUMem before reading any of them, and
	// see metrics.GPUObservation for what each can and cannot prove: busy-time
	// bounds use from above only, so a high GPUBusyMax is not evidence of use.
	GPUBusyMax     float64
	GPUBusyAvg     float64
	GPUSMActiveAvg float64
	GPUSMActiveP95 float64
	GPUMemMax      float64 // bytes, device-wide
	// GPUIdleDevices is the fewest devices any one pod held without running a
	// kernel on: the count every replica could give up.
	GPUIdleDevices int
	GPUFirstSeen   float64
	GPULastSeen    float64
	// GPUShared records that a device this workload used was also used by
	// another pod, either seen in the metrics or declared by the node
	// (time-slicing, MPS). Per-device numbers then describe the device, not
	// this container.
	GPUShared     bool
	GPUSharedWith []string
	gpuPods       int
	// cpuHeldForGPU records that the CPU request was held because lowering it
	// could starve a GPU, so it must not also be reported as wasted.
	cpuHeldForGPU bool

	// WorkloadKind is the controller kind. DaemonSet is the one that changes
	// the arithmetic: Pods is then the node count, so the total scales with
	// the cluster rather than with traffic, and every request is subtracted
	// from every node's allocatable.
	WorkloadKind string

	// Nodes are the nodes this workload's pods were found on, and NodeClasses
	// the distinct arch/instance-type pairs among them. More than one class
	// means the usage being summarised was produced by hardware of different
	// speeds, which is a reason to lower confidence, never a reason to apply
	// a correction factor this tool has no way to measure.
	Nodes       []string
	NodeClasses []string

	// NodeUtilP95 is the 95th percentile utilisation of the busiest node this
	// workload ran on. NodeUtilKnown separates "the nodes were idle" from
	// "nobody measured", which look identical in a float.
	NodeUtilP95   float64
	NodeUtilKnown bool

	Verdict    Verdict
	Confidence Confidence

	CPURequest int64 // millicores
	MemRequest int64 // bytes
	CPULimit   int64
	MemLimit   int64

	CPUUsageP95   float64 // cores
	CPUUsageMax   float64 // cores, drives the limit recommendation
	CPUStartupMax float64 // cores, peak during boot
	CPUSteadyMax  float64 // cores, peak excluding boot
	MemUsageMax   float64 // bytes
	MemP95        float64 // bytes, 95th percentile, used to detect a plateau
	// PSIWaiting is sustained pressure, the mean over the window, and is what
	// the verdict is decided on. PSIWaitingPeak is the worst single bucket,
	// reported because boot starvation lives there and a mean erases it.
	PSIWaiting     float64
	PSIWaitingPeak float64
	// Throttle is sustained, ThrottlePeak the worst single bucket, matching
	// the pressure pair above.
	Throttle     float64
	ThrottlePeak float64
	OOMKilled    bool
	Restarts     float64
	Samples      int

	Ceilings []ceilings.Ceiling

	MemEarly      float64
	MemLate       float64
	MemLimitEarly float64
	MemLimitLate  float64

	// Have records which signals backed this finding, and Missing lists the
	// ones the analyzer wanted but did not get. A conclusion drawn without its
	// supporting signal must say so.
	Have    map[metrics.Signal]bool
	Missing []metrics.Signal

	// trendKilledByRestart records that the memory trend was measured and then
	// discarded because an OOMKill inside the window reset the working set it
	// was measured across. It is not the same as the signal never having been
	// exported, and the caveat it drives says so.
	trendKilledByRestart bool

	// RecCPU and RecMem are the recommended requests, zero when none is
	// offered. RecCPULimit is the recommended CPU limit, which is deliberately
	// higher: a request sized to a percentile must not become a limit that
	// throttles every burst.
	RecCPU      int64
	RecCPULimit int64
	RecMem      int64
	// CPUFloored and MemFloored record that the recommendation is
	// MinCPUMillicores / MinMemBytes rather than anything derived from usage.
	// A floored recommendation is constant by construction, so it must not be
	// read as a stable measurement.
	CPUFloored bool
	MemFloored bool
	// RecMemLimit is the recommended memory limit. Memory has no graceful
	// degradation, so the limit sits at the request plus a headroom fraction
	// rather than at a peak: the gap only buys time before an OOMKill.
	RecMemLimit int64
	// MemCensored records that the container was OOMKilled and no measurement
	// raised its memory, so tare does not know what it needs: usage above the
	// limit that killed it is never observed, and the recorded peak is a lower
	// bound on demand. RecMem and RecMemLimit then hold the current values so
	// every numeric consumer leaves memory alone, and renderers must print the
	// recommendation as unknown, never as those values: an unchanged "16Mi"
	// beside OOMKILLED reads as "16Mi is right", which is the input echoed back.
	MemCensored bool

	// Constraints are the workload facts that shaped the recommendation.
	Constraints discover.Constraints

	// Authored records which resource fields the workload's manifest set, as
	// opposed to which ones the API server filled in for it.
	Authored discover.Authored

	// Reasons explains the verdict and any refusal: a stable code and the
	// numbers behind it for a script, the sentence for a person.
	Reasons []Reason
}
