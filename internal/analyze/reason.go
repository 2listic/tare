package analyze

// A reason used to be a sentence. That made the report readable and made it
// impossible to act on with anything but a human: every consumer that wanted
// "was this container OOMKilled" had to match a substring of prose that no
// test pinned, and a reworded sentence broke it silently.
//
// A Reason now carries the sentence AND the two or three facts it was built
// from, under a stable code. The renderer decides which of the two to show;
// the code and the numbers are what a script reads. The prose is kept, not
// summarised: it is the part that stops somebody applying a number they should
// not, and no code communicates "what this does NOT rule out".

// Code identifies a reason. Codes are API: they may be added, but an existing
// one must not change meaning, because a script filtering on OOMKILLED after a
// rename would silently match nothing, which in this tool is the shape every
// serious bug has taken.
type Code string

const (
	// Evidence about the container's own behaviour.
	NoSamples    Code = "NO_SAMPLES"
	MemLeak      Code = "MEM_LEAK"
	OOMKilled    Code = "OOMKILLED"
	LeakMasked   Code = "LEAK_MASKED_BY_RESTART"
	CPUPressure  Code = "CPU_PRESSURE"
	CPUThrottled Code = "CPU_THROTTLED"
	// CPUWaitRelative: starved by tare-agent's CPU wait, where PSI is missing.
	// The name is agent/compare-pressure's, so the two outputs match.
	CPUWaitRelative   Code = "CPU_WAIT_RELATIVE"
	NodeContention    Code = "NODE_CONTENTION"
	RestartChurn      Code = "RESTART_CHURN"
	IdleWindow        Code = "IDLE_NO_WORK"
	IdleCaveat        Code = "IDLE_NOT_RULED_OUT"
	CPUUnderused      Code = "CPU_UNDERUSED"
	MemUnderused      Code = "MEM_UNDERUSED"
	CPULimitUnderused Code = "CPU_LIMIT_UNDERUSED"
	MemLimitUnderused Code = "MEM_LIMIT_UNDERUSED"
	CPUAtLimit        Code = "CPU_PINNED_AT_LIMIT"

	// How the recommendation was arrived at.
	CPUFloor   Code = "CPU_FLOOR"
	MemFloor   Code = "MEM_FLOOR"
	CPUCapped  Code = "CPU_CAPPED_BY_CEILING"
	MemRaised  Code = "MEM_RAISED_BY_CEILING"
	CPUStartup Code = "CPU_SIZED_FOR_STARTUP"
	// CPULimitThrottled: the current CPU limit is already throttling the
	// container, so it is not lowered, whatever averaged usage says.
	CPULimitThrottled Code = "CPU_LIMIT_HELD_THROTTLED"
	CeilingBlocked    Code = "CEILING_UNRESOLVED"
	UnschedulableCPU  Code = "UNSCHEDULABLE_CPU"
	UnschedulableMem  Code = "UNSCHEDULABLE_MEM"
	// MemDemandCensored: OOMKilled, and nothing measured says how much more it
	// needs, so memory is left as it is rather than given a number.
	MemDemandCensored Code = "MEM_DEMAND_CENSORED"

	// What the run could not see.
	NoSaturationSignal Code = "NO_SATURATION_SIGNAL"
	NoOOMHistory       Code = "NO_OOM_HISTORY"
	NoMemTrendOnRaise  Code = "NO_MEM_TREND_ON_RAISE"
	NoMemP95           Code = "NO_MEM_P95"
	NodeUtilUnknown    Code = "NODE_UTIL_UNKNOWN"
	// CPU wait was measured and not used: the agent saw too little of the
	// container's CPU time, or the container has too little history for its
	// own normal.
	CPUWaitLowCoverage  Code = "CPU_WAIT_LOW_COVERAGE"
	CPUWaitShortHistory Code = "CPU_WAIT_SHORT_HISTORY"

	// Facts about the workload that bound what may be recommended.
	VPAManaged      Code = "VPA_MANAGES_WORKLOAD"
	HPAOnCPUCode    Code = "HPA_SCALES_ON_CPU"
	HPAOnMemCode    Code = "HPA_SCALES_ON_MEMORY"
	QoSHeld         Code = "QOS_GUARANTEED_HELD"
	QoSAccidental   Code = "QOS_GUARANTEED_BY_ACCIDENT"
	PerNodeWorkload Code = "PER_NODE_WORKLOAD"
	MixedHardware   Code = "MIXED_NODE_CLASSES"
	LimitNotNode    Code = "LIMIT_NOT_NODE"
	GoMaxProcs      Code = "GOMAXPROCS_EXCEEDS_QUOTA"
	ThreadPoolUnset Code = "THREAD_POOL_UNSET"

	// The gap between the manifest and the admitted pod.
	CPULimitCopied Code = "CPU_LIMIT_COPIED_TO_REQUEST"
	MemLimitCopied Code = "MEM_LIMIT_COPIED_TO_REQUEST"
	CPULimitAdded  Code = "CPU_LIMIT_WOULD_BE_ADDED"
	MemLimitAdded  Code = "MEM_LIMIT_WOULD_BE_ADDED"
	BestEffort     Code = "BEST_EFFORT"

	// Accelerators.
	GPUNotRequested   Code = "GPU_NOT_REQUESTED"
	GPUMemUnknown     Code = "GPU_DEVICE_MEM_UNKNOWN"
	VRAMUnresolved    Code = "VRAM_UNRESOLVED"
	VRAMOverDevice    Code = "VRAM_EXCEEDS_DEVICE"
	VRAMOverPool      Code = "VRAM_EXCEEDS_RESERVED_POOL"
	VRAMFits          Code = "VRAM_FITS"
	GPUOversubscribed Code = "GPU_OVERSUBSCRIBED"
)

// Unit says how to read Observed and Against. Without it a consumer comparing
// 0.9 against 0.9 cannot tell a pressure figure from a fraction of a device,
// and a renderer cannot format either.
type Unit string

const (
	Millicores Unit = "millicores"
	Bytes      Unit = "bytes"
	CoreCount  Unit = "cores"
	Count      Unit = "count"
	Ratio      Unit = "ratio"
	Percent    Unit = "percent"
	SecPerSec  Unit = "s/s"
)

// Reason is one piece of evidence behind a verdict.
//
// Observed and Against are raw numbers in Unit, never pre-formatted: a
// consumer that has to parse "4.0Gi" back into bytes is doing the tool's job,
// and doing it with a rounding error the tool introduced.
type Reason struct {
	Code Code `json:"code"`
	// Observed is what was measured; Against is what it was measured against
	// - a threshold, a request, a device size. Either may be zero when the
	// reason is a statement rather than a comparison.
	Observed float64 `json:"observed,omitempty"`
	Against  float64 `json:"against,omitempty"`
	Unit     Unit    `json:"unit,omitempty"`
	// Subject names the thing the reason is about when that is not the
	// container itself: a ceiling kind, an HPA, a GPU product, a node class.
	Subject string `json:"subject,omitempty"`
	// Text is the operator-facing sentence. It is not derivable from the
	// fields above and is not redundant with them.
	Text string `json:"text"`
}

// note is a reason with no measurement behind it.
func note(code Code, text string) Reason { return Reason{Code: code, Text: text} }

// about is a reason about a named thing rather than a measured one.
func about(code Code, subject, text string) Reason {
	return Reason{Code: code, Subject: subject, Text: text}
}

// measured is the common case: a number, what it was compared with, and the
// sentence that interprets them.
func measured(code Code, observed, against float64, u Unit, text string) Reason {
	return Reason{Code: code, Observed: observed, Against: against, Unit: u, Text: text}
}

// measuredAbout is a measurement whose subject is not the container: the HPA
// that will react to the change, the GPU the weights have to fit on.
func measuredAbout(code Code, subject string, observed, against float64, u Unit, text string) Reason {
	return Reason{Code: code, Observed: observed, Against: against, Unit: u, Subject: subject, Text: text}
}

// ReasonTexts is the prose alone, for the renderers that show sentences.
func ReasonTexts(rs []Reason) []string {
	if len(rs) == 0 {
		return nil
	}
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Text
	}
	return out
}

// Has reports whether any reason carries this code. This is the lookup that
// used to be a substring match on prose.
func (f Finding) Has(code Code) bool {
	for _, r := range f.Reasons {
		if r.Code == code {
			return true
		}
	}
	return false
}
