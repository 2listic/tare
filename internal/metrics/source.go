// Package metrics reads container behaviour from a Prometheus-compatible
// endpoint.
//
// The Source interface exists so the analyzer never learns which backend it is
// talking to. Thanos, Mimir, VictoriaMetrics and a future no-Prometheus
// degraded mode all sit behind it.
//
// Signals are tracked explicitly. Several of the metrics this tool wants do not
// exist on every cluster: CFS throttle counters are absent on cgroup v2 setups
// that export PSI instead, kube-state-metrics may not be installed, and
// container_start_time_seconds is not universal. A missing metric must produce
// a visible gap rather than a zero, because a zero here reads as good news: no
// pressure, no OOMKills, no restarts. Silently reporting a starved container as
// healthy is the worst thing this tool could do.
package metrics

import "time"

// Signal names a metric family the analyzer depends on.
type Signal string

const (
	SigCPUUsage    Signal = "cpu-usage"
	SigCPUPeak     Signal = "cpu-peak"
	SigMemory      Signal = "memory"
	SigMemP95      Signal = "memory-p95"
	SigPSI         Signal = "psi"
	SigPSIAvg      Signal = "psi-sustained"
	SigThrottle    Signal = "cfs-throttle"
	SigThrottleAvg Signal = "cfs-throttle-sustained"
	SigOOM         Signal = "oom-events"
	SigRestarts    Signal = "restarts"
	SigStartup     Signal = "startup-window"
	SigMemTrend    Signal = "memory-trend"
	SigLimitTrend  Signal = "limit-trend"
	SigNodeUtil    Signal = "node-utilisation"
	SigOwner       Signal = "pod-owner"
	// SigCPUWait is tare-agent's CPU wait, the saturation signal for nodes
	// without PSI. See cpuwait.go.
	SigCPUWait Signal = "cpu-wait"
)

// SaturationSignals are the ones that reveal a container is being starved.
// With none of them, a shrink recommendation cannot be made safely: there is
// no way to tell a comfortable container from a throttled one.
var SaturationSignals = []Signal{SigPSI, SigCPUWait, SigThrottle}

// Key identifies one container of one pod.
type Key struct {
	Namespace string
	Pod       string
	Container string
}

// Observation is what the analyzer needs to know about a container over a
// window. Check Has before trusting any field that could legitimately be zero.
type Observation struct {
	Key

	CPUUsageP95   float64 // cores, steady-state basis for the request
	CPUUsageMax   float64 // cores, peak at fine resolution, basis for the limit
	MemWorkingSet float64 // bytes, max over window
	MemP95        float64 // bytes, close to the max means a plateau

	// CPUStartupMax is peak usage during the first startup window after a
	// container start; CPUSteadyMax is the peak outside it.
	CPUStartupMax float64
	CPUSteadyMax  float64

	// ThrottleRatio is the worst single bucket; ThrottleAvg is the mean across
	// the window. Same asymmetry as the two pressure fields below: a peak over
	// a widening window never decreases, so a report that shows only the peak
	// tells a reader a container is throttled 6% of the time when it is
	// throttled 0.2% of the time and was once unlucky.
	ThrottleRatio float64
	ThrottleAvg   float64

	// PSIWaitingAvg is mean stall per second across the whole window: the
	// fraction of wall time the container spent waiting for CPU it was denied.
	// This is what the saturation threshold is defined against.
	//
	// PSIWaitingMax is the highest stall seen in any single fine-grained
	// bucket. It is kept for boot starvation, which a window average erases,
	// but it must not decide the verdict on its own: a peak over a window is
	// monotonically non-decreasing, so one stalled minute marks a container
	// saturated for as long as the window is wide, and the longer the window
	// grows the more containers it swallows.
	PSIWaitingAvg float64
	PSIWaitingMax float64

	// MemEarly and MemLate are average working set in the first and last hour
	// of the window; a large gap is a leak.
	MemEarly float64
	MemLate  float64

	// MemLimitEarly and MemLimitLate detect whether the limit itself moved. If
	// it moved and the plateau did not follow, the runtime holds a fixed cap.
	MemLimitEarly float64
	MemLimitLate  float64

	OOMEvents float64
	Restarts  float64

	// CPUWait is tare-agent's view of the container, valid when Has(SigCPUWait).
	CPUWait CPUWait

	// Samples is populated buckets seen for this container, which drives the
	// confidence rating.
	Samples int

	// FirstSeen and LastSeen bound the part of the window in which this
	// container reported memory, in unix seconds, zero when unknown. A
	// workload's coverage is the union of its pods' spans, not their sum:
	// three replicas running side by side for a day are a day of evidence.
	FirstSeen float64
	LastSeen  float64

	// Owner is the controller the pod belonged to, "Deployment/web", read from
	// kube-state-metrics rather than from the API server, because the pods
	// that matter here are the ones the API server has already forgotten. A
	// rollout or a reschedule replaces every pod, and history keyed only by
	// live pod names starts again from nothing each time. Empty when
	// kube-state-metrics is not scraped.
	Owner string

	// GPU is what the devices attributed to this container did, nil when
	// none were or nobody asked (see GPUObserver).
	GPU *GPUObservation

	// Have records which signals actually produced data for this container.
	Have map[Signal]bool
}

// Has reports whether a signal was obtained. A false here means "not measured",
// which is different from a measured zero and must be reported differently.
func (o *Observation) Has(s Signal) bool {
	if o == nil || o.Have == nil {
		return false
	}
	return o.Have[s]
}

// Mark records that s backed this observation.
func (o *Observation) Mark(s Signal) {
	if o.Have == nil {
		o.Have = map[Signal]bool{}
	}
	o.Have[s] = true
}

// HasAny reports whether any of the given signals was obtained.
func (o *Observation) HasAny(ss ...Signal) bool {
	for _, s := range ss {
		if o.Has(s) {
			return true
		}
	}
	return false
}

// SignalHealth is the outcome of one query, for the report header. A query that
// errored and a query that returned nothing are different problems: the first
// is a broken endpoint or a bad expression, the second is a metric this cluster
// does not export.
type SignalHealth struct {
	Signal   Signal
	Query    string
	Err      error
	Series   int
	Required bool

	// Suspect is set when the query succeeded and returned series whose values
	// cannot be believed. A failed query and an absent metric both announce
	// themselves; an instrument that reports a confident zero does not, and it
	// is the more dangerous of the three because zero is the reassuring answer
	// for every signal here.
	Suspect string
}

// OK reports whether the signal produced usable data. Returning series is not
// enough: see Suspect.
func (h SignalHealth) OK() bool { return h.Err == nil && h.Series > 0 && h.Suspect == "" }

// NodeObservation is what a node actually did, as opposed to what was reserved
// on it.
//
// Requests and limits are promises; this is the machine's own account of how
// busy it was. The two come apart constantly, and the gap is the only way to
// tell a container throttled by its own quota from one competing for a node
// that was genuinely full.
type NodeObservation struct {
	CPUUtilMean float64 // 0..1 of all cores, averaged over the window
	// CPUUtilP95 is the 95th percentile of 5m buckets, deliberately NOT the
	// maximum. Measured on the lab cluster over four days: a seven-minute load
	// test put the max at 99.9% while the p95 stayed at 7.7% and the mean at
	// 5.9%. A max would let one brief event describe a node as saturated for
	// the entire window, which is how this feature briefly stopped reporting
	// anything at all.
	CPUUtilP95 float64
	Samples    int
}

// NodeObserver is an optional Source capability: reading node-level metrics,
// which most clusters get from node-exporter and some do not export at all.
//
// It is deliberately not part of Source. Widening that interface would force a
// stub into internal/simulate, which has no node model, and a stub returning a
// plausible zero is how this project's defects usually begin: an idle node is
// exactly what "no data" would look like. Callers type-assert, and findings
// that need node utilisation are withheld with a reason when it is absent.
type NodeObserver interface {
	// ObserveNodes is keyed by node IP, because that is what the metrics
	// carry. node-exporter labels its series with an `instance` of host:port
	// and no node name at all; resolving that to a Kubernetes node is the
	// caller's job, since only the caller holds the node inventory.
	ObserveNodes(ips []string, window time.Duration) (map[string]NodeObservation, error)
}

// Placer is a Source that can use each container's node. Like NodeObserver it
// is not part of Source: the simulator has no nodes.
type Placer interface {
	// SetPlacement gives each container's node before Observe, for the node
	// half of the CPU wait rule. Optional: without it that rule decides from
	// the containers alone.
	SetPlacement(map[Key]Placement)
}

// WindowProber is an optional Source capability: reporting how much history the
// backend actually holds, so the caller can ask for a window that exists rather
// than one that produces a report full of "low" confidence.
type WindowProber interface {
	AvailableWindow(namespaces []string, want time.Duration) (time.Duration, error)
}

// Source reads observations for a window ending now.
type Source interface {
	Observe(namespaces []string, window time.Duration) (map[Key]*Observation, error)
	// Health describes what each signal produced during the last Observe.
	Health() []SignalHealth
	// Describe names the backend for the report header.
	Describe() string
}
