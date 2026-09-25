// Package analyze turns container specs and observations into findings.
//
// The ordering principle: saturation evidence first, then blocked
// recommendations, then waste. A platform engineer reading the report should
// meet the containers that are hurting before the ones that are merely
// expensive.
package analyze

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/mfranzon/tare/internal/ceilings"
	"github.com/mfranzon/tare/internal/discover"
	"github.com/mfranzon/tare/internal/metrics"
)

// Verdict is the headline classification of a container.
type Verdict string

const (
	Leaking    Verdict = "LEAKING"    // memory grows without bound; sizing cannot fix it
	Saturated  Verdict = "SATURATED"  // starved of CPU, or being OOMKilled
	Blocked    Verdict = "BLOCKED"    // a runtime ceiling makes sizing meaningless
	Idle       Verdict = "IDLE"       // no measurable work all window; the saving is deletion, not resizing
	Waste      Verdict = "WASTE"      // requesting far more than it uses
	Unverified Verdict = "UNVERIFIED" // a reduction cannot be shown to be safe: no saturation signal, or the container kept restarting
	Healthy    Verdict = "OK"
	NoData     Verdict = "NO-DATA"
)

// ReportOrder is every verdict, in the order a summary should list them. It
// lives beside the constants because the alternative - a list written out at
// the call site - silently drops any verdict added later, and a verdict
// missing from the summary is a finding the reader never learns exists.
func ReportOrder() []Verdict {
	return []Verdict{Conflicted, Saturated, Leaking, Blocked, Idle, Unverified, Waste, Healthy, NoData}
}

// Confidence reflects how much evidence stands behind a recommendation. It is
// a first-class output: most clusters retain about 15 days of Prometheus data,
// and many containers are far younger than that.
type Confidence string

const (
	Low    Confidence = "low"
	Medium Confidence = "medium"
	High   Confidence = "high"
)

// Thresholds are the decision boundaries. They are struct fields rather than
// constants so the simulator can sweep them.
type Thresholds struct {
	// PSIWaiting above this many seconds of stall per second counts as
	// saturation. 0.05 means the container spends 5% of wall time waiting for
	// CPU it was denied. It is compared against the window MEAN, not the peak:
	// the peak is monotonically non-decreasing as the window widens, so
	// deciding on it means a single stalled minute marks a container saturated
	// for the rest of the window, and a fortnight-long window eventually
	// swallows everything that ever paused for GC.
	PSIWaiting float64
	// ThrottleRatio is the fallback when CFS counters exist.
	ThrottleRatio float64
	// WasteRatio: p95 usage below this fraction of the request is waste.
	WasteRatio float64
	// Margin is added on top of observed usage when recommending.
	Margin float64
	// MinCPUMillicores floors CPU recommendations.
	MinCPUMillicores int64
	// MinMemBytes floors memory recommendations.
	MinMemBytes int64
	// MemLimitHeadroom is how far the memory limit sits above the request.
	MemLimitHeadroom float64
	// LeakRatio: late-window memory this many times the early-window average,
	// with no restart in between, is treated as an unbounded leak.
	LeakRatio float64
	// BurstMargin is added on top of peak usage when recommending a CPU limit.
	// Limits must cover bursts that a request percentile deliberately ignores.
	BurstMargin float64
	// StartupMargin is added on top of peak boot usage. It is larger than
	// BurstMargin because throttling a boot does not merely slow a request, it
	// can fail a readiness probe and turn a rollout into an outage.
	StartupMargin float64
	// PlateauRatio: p95 memory this close to the maximum counts as a plateau
	// rather than an occasional peak.
	PlateauRatio float64
	// PlateauHeadroom: the plateau must sit below this fraction of the limit,
	// otherwise the container is constrained by the limit and there is no
	// internal ceiling to infer.
	PlateauHeadroom float64
	// LimitMoveRatio: how much the memory limit must have changed during the
	// window before its stability counts as evidence.
	LimitMoveRatio float64
	// MinSamplesForInference guards against inferring a ceiling from an hour
	// of data.
	MinSamplesForInference int
	// MinSaturationUtil guards every pressure-based saturation verdict: the
	// container's own usage must reach this fraction of its allocation before
	// stalls are read as starvation. A container using 1.5% of its request is
	// not being denied CPU it wanted; it is watching other tenants fight over
	// the node, and raising its request would change nothing. Without this,
	// pressure alone marks an idle JVM that pauses for GC as starved, and the
	// recommendation ratchets to its current request and stays there.
	MinSaturationUtil float64
	// StartupRatio is how much higher boot usage must be than steady state
	// before the two count as distinct regimes. Below it, a busy loop looks
	// identical booting and running, and saying so would be noise.
	StartupRatio float64
	// NodeIdleCeiling: a node whose MEAN utilisation stayed below this fraction
	// was broadly idle, so capacity the container was denied was going spare.
	//
	// The mean is the primary gate rather than the peak, because the peak is
	// taken over the whole window: gating on it would mean one busy minute in
	// a fortnight permanently disqualifies a node that is idle the rest of the
	// time. That is the wrong direction for a claim about what was available
	// in general.
	//
	// UNSWEPT, and it cannot be swept by the existing harness: the simulator
	// models no node at all, so no archetype in the catalog exercises this
	// path. 0.50 is a starting point chosen with margin, not a measured value.
	// PlateauHeadroom is the cautionary precedent - it has sat unmeasured for
	// the same reason since the first threshold sweep.
	NodeIdleCeiling float64
	// NodeBusyP95 guards the mean. A node can average 20% and still have been
	// saturated for the hours that mattered, and tare does not correlate the
	// throttling with the node's busy periods in time, so a second gate on how
	// busy it got is what lets the finding say "there was room" rather than
	// "there was room on average".
	//
	// It is a 95th percentile and NOT a maximum, which the first version got
	// wrong. Over a two-week window a max is pinned by any single spike: a
	// seven-minute load test on the lab cluster put the four-day max at 99.9%
	// while the p95 read 7.7%, and the finding silently stopped reporting on a
	// cluster that was idle 99.9% of the time. Also unswept.
	NodeBusyP95 float64
	// RestartChurn: this many container restarts inside the window means the
	// container has not run long enough at any one size to be sized from its
	// own usage.
	RestartChurn float64
	// IdleCPUMillicores: a container whose PEAK CPU across the whole window
	// stays below this is doing no measurable work. The peak is the right
	// statistic here and a percentile is not: a job that runs for ten minutes
	// a day is not idle, and p95 would call it idle every time.
	IdleCPUMillicores float64
	// IdleMemGrowth: memory must also be flat. A process that is accumulating
	// anything is doing something, and "delete this" is the most destructive
	// sentence the report can print, so it takes two independent signals.
	IdleMemGrowth float64
	// IdleMinCoverage is the fraction of the window that must actually be
	// observed before silence counts as evidence of idleness. Without it, a
	// container deployed an hour ago reads as idle for the other thirteen
	// days, which is the failure mode this project keeps finding: a clean,
	// confident answer computed from data that was never there.
	IdleMinCoverage float64
}

func DefaultThresholds() Thresholds {
	return Thresholds{
		// 0.03, chosen with margin rather than at the edge. Sweep result: every
		// value from 0.005 to 0.05 classifies 36/36 scenarios correctly, and
		// 0.10 upward drops to 26/36 by missing mild starvation. The shipped
		// 0.05 sat on the last good value. The low end is unconstrained by the
		// catalog but constrained by reality: the simulator models no
		// background node contention, so a very low threshold that looks free
		// here would flag every container on a busy node.
		PSIWaiting: 0.03,
		// 0.5. Sweep result: every value from 0 to 1.0 scores 36/36, and 1.2
		// collapses to 16/36 by demanding a container exceed its allocation
		// before its stalls count. The plateau is wide and 0.5 sits in the
		// middle of it.
		//
		// Note what that sweep does NOT say: at 0 the guard is disabled and the
		// score is still 36/36, because the simulator never produces the case
		// this exists for. Every simulated stall comes from the container's own
		// demand meeting its own allocation, so pressure and utilisation always
		// move together. On a real node they come apart: tare-lab's JVM stalled
		// 1.7% of minutes from a neighbour burning a core while using 1.5% of
		// its own request, and was reported SATURATED. This threshold is
		// calibrated against the cluster, not the catalog.
		MinSaturationUtil: 0.5,
		ThrottleRatio:     0.05,
		WasteRatio:        0.40,
		Margin:            0.15, // matches VPA's recommendation-margin-fraction
		MinCPUMillicores:  10,
		MinMemBytes:       16 << 20,
		MemLimitHeadroom:  0.25,
		// 1.5 rather than 2.0: measured by sweep on the archetype catalog,
		// where 1.5 classifies 24/24 correctly and 2.0 misses a workload that
		// merely doubles its memory over a week. 1.2 false-positives.
		LeakRatio:     1.5,
		BurstMargin:   0.25,
		StartupMargin: 0.40,
		StartupRatio:  1.5,
		// 3, not 1: the lab's slow-leaker is designed to OOMKill about every
		// 40 hours, and a single restart inside a multi-day window is that
		// working as intended rather than a container that cannot stay up.
		RestartChurn: 3,

		PlateauRatio: 0.90,
		// 5m of CPU. A container genuinely serving traffic does not peak below
		// this in a fortnight; the sidecars and operators that do peak below it
		// are exactly the population worth questioning. Unswept: no archetype
		// in the catalog is idle, and one must be added before this number is
		// defended as anything but a starting point.
		IdleCPUMillicores:      5,
		IdleMemGrowth:          1.1,
		IdleMinCoverage:        0.5,
		NodeIdleCeiling:        0.50,
		NodeBusyP95:            0.85,
		PlateauHeadroom:        0.70,
		LimitMoveRatio:         0.20,
		MinSamplesForInference: 2880, // twelve hours at a 15s scrape
	}
}

// Finding is one container's verdict, evidence and recommendation.
type Finding struct {
	Namespace string
	Workload  string
	Container string
	Image     string
	QoS       string
	Pods      int

	// GPUs is the accelerator count this container requests, per pod.
	GPUs int64

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

	// Constraints are the workload facts that shaped the recommendation.
	Constraints discover.Constraints

	// Authored records which resource fields the workload's manifest set, as
	// opposed to which ones the API server filled in for it.
	Authored discover.Authored

	// Reasons explains the verdict and any refusal: a stable code and the
	// numbers behind it for a script, the sentence for a person.
	Reasons []Reason
}

// Cluster is what the analyzer knows about the cluster beyond the containers
// themselves: the workload facts that limit what may be recommended (QoS, an
// HPA scaling on a resource whose request is about to change, a VPA already
// managing the same fields) and the node inventory. Both are optional, and a
// zero Cluster must produce exactly the findings the container data alone
// supports.
type Cluster struct {
	Constraints map[string]discover.Constraints
	Nodes       discover.Inventory

	// NodeUtil is how busy each node actually was, keyed by node NAME. The
	// caller resolves metric addresses to names, so the analyzer never handles
	// an IP. A nil map means node utilisation could not be read, which is a
	// reportable outcome rather than an idle cluster.
	NodeUtil map[string]metrics.NodeObservation
}

// Run joins containers with observations and produces findings, most severe
// first. It is the form the simulator and the tests use, where there is no
// cluster to describe.
func Run(containers []discover.Container, obs map[metrics.Key]*metrics.Observation, window time.Duration, th Thresholds) []Finding {
	return RunWithCluster(containers, obs, Cluster{}, window, th)
}

// RunWithCluster is Run with the cluster context the CLI has and the simulator
// does not.
func RunWithCluster(containers []discover.Container, obs map[metrics.Key]*metrics.Observation,
	cl Cluster, window time.Duration, th Thresholds) []Finding {
	constraints := cl.Constraints
	// Group by workload plus container name: a recommendation applies to the
	// workload, not to an individual pod.
	type groupKey struct{ ns, workload, container string }
	groups := map[groupKey]*Finding{}
	// worstGrowth tracks, per group, the largest late/early memory ratio seen
	// on any single pod, so the trend pair kept below comes from one pod.
	worstGrowth := map[groupKey]float64{}

	for _, c := range containers {
		gk := groupKey{c.Namespace, c.Workload, c.Name}
		f, ok := groups[gk]
		if !ok {
			f = &Finding{
				Namespace: c.Namespace, Workload: c.Workload, Container: c.Name,
				Image:      c.Image,
				QoS:        c.QoS,
				CPURequest: c.CPURequest, MemRequest: c.MemRequest,
				CPULimit: c.CPULimit, MemLimit: c.MemLimit,
				WorkloadKind: c.WorkloadKind,
				GPUs:         c.GPUs,
				Authored:     c.Authored,
				Ceilings:     ceilings.Detect(c.Spec),
			}
			groups[gk] = f
		}
		f.Pods++
		if c.Node != "" && !slices.Contains(f.Nodes, c.Node) {
			f.Nodes = append(f.Nodes, c.Node)
		}
		if c.LastTermError == "OOMKilled" {
			f.OOMKilled = true
		}

		o := obs[metrics.Key{Namespace: c.Namespace, Pod: c.Pod, Container: c.Name}]
		if o == nil {
			continue
		}
		// Across pods of one workload take the worst case: the recommendation
		// must hold for every replica, not for the average one.
		f.CPUUsageP95 = max(f.CPUUsageP95, o.CPUUsageP95)
		f.CPUUsageMax = max(f.CPUUsageMax, o.CPUUsageMax)
		f.CPUStartupMax = max(f.CPUStartupMax, o.CPUStartupMax)
		f.CPUSteadyMax = max(f.CPUSteadyMax, o.CPUSteadyMax)
		f.MemUsageMax = max(f.MemUsageMax, o.MemWorkingSet)
		f.MemP95 = max(f.MemP95, o.MemP95)
		// A limit is a property of the workload, so the informative value wins:
		// a pod created after a limit change has no early observation, and
		// taking its zero would erase the very movement being looked for.
		f.MemLimitEarly = max(f.MemLimitEarly, o.MemLimitEarly)
		f.MemLimitLate = max(f.MemLimitLate, o.MemLimitLate)
		f.Restarts = max(f.Restarts, o.Restarts)
		f.PSIWaiting = max(f.PSIWaiting, o.PSIWaitingAvg)
		f.PSIWaitingPeak = max(f.PSIWaitingPeak, o.PSIWaitingMax)
		f.Throttle = max(f.Throttle, o.ThrottleAvg)
		f.ThrottlePeak = max(f.ThrottlePeak, o.ThrottleRatio)
		for sig, ok := range o.Have {
			if ok {
				if f.Have == nil {
					f.Have = map[metrics.Signal]bool{}
				}
				f.Have[sig] = true
			}
		}
		if o.OOMEvents > 0 {
			f.OOMKilled = true
		}
		f.Samples = max(f.Samples, o.Samples)

		// The memory trend is a per-pod pair, and the two halves must come from
		// the same pod: comparing one replica's early average against another's
		// late average manufactures growth out of a rollout, or hides a leak
		// behind a replica that restarted. Keep the pair from whichever pod
		// grew most, which is the worst case the other fields here already take.
		if o.MemEarly > 0 {
			if g := o.MemLate / o.MemEarly; g > worstGrowth[gk] {
				worstGrowth[gk] = g
				f.MemEarly, f.MemLate = o.MemEarly, o.MemLate
			}
		} else if f.MemEarly == 0 {
			// No pod has an early observation yet. Record the current level so
			// the reader sees a measurement, but leave MemEarly at zero: the
			// leak test is guarded on it and must stay unevaluated, not guessed.
			f.MemLate = max(f.MemLate, o.MemLate)
		}
	}

	out := make([]Finding, 0, len(groups))
	for gk, f := range groups {
		sort.Strings(f.Nodes)
		f.NodeClasses = cl.Nodes.Classes(f.Nodes)
		decide(f, window, th)
		applyNodes(f, cl.Nodes, cl.NodeUtil, th)
		applyInference(f, cl.Nodes)
		applyAuthored(f, f.Authored)
		authoredQoSNote(f, f.Authored)
		applyConstraints(f, constraints[keyOf(gk.ns, gk.workload)], th)
		applySignalGaps(f)
		out = append(out, *f)
	}
	sort.Slice(out, func(i, j int) bool {
		if p := severity(out[i].Verdict) - severity(out[j].Verdict); p != 0 {
			return p > 0
		}
		// within a verdict, worst pressure first, then biggest waste
		if out[i].PSIWaiting != out[j].PSIWaiting {
			return out[i].PSIWaiting > out[j].PSIWaiting
		}
		return out[i].CPURequest > out[j].CPURequest
	})
	return out
}

func severity(v Verdict) int {
	switch v {
	case Conflicted:
		return 7
	case Leaking:
		return 6
	case Saturated:
		return 5
	case Blocked:
		return 4
	// Idle outranks Waste because the two recommend different actions and the
	// more valuable one is easy to miss: Waste says make this smaller, Idle
	// says this may not need to exist. It sits below the refusals because a
	// container doing no work is expensive rather than hurting, and this
	// report shows what hurts first.
	case Idle:
		return 3
	case Unverified:
		return 2
	case Waste:
		return 2
	case Healthy:
		return 1
	}
	return 0
}

func decide(f *Finding, window time.Duration, th Thresholds) {
	f.Confidence = confidence(f.Samples, window)

	if f.Samples == 0 {
		f.Verdict = NoData
		f.Reasons = append(f.Reasons, note(NoSamples, "no metrics found for this container in the window"))
		return
	}

	// A leak outranks everything: no allocation survives unbounded growth, so
	// reporting it as a sizing problem sends the operator down the wrong path.
	// Restarts are deliberately NOT a disqualifier here: a leak bad enough to
	// OOMKill has restarts by definition, and that is the case that matters
	// most. A container whose restarts reset memory shows late close to early
	// and fails the ratio test on its own.
	if f.has(metrics.SigMemTrend) && f.MemEarly > 0 && f.MemLate > f.MemEarly*th.LeakRatio {
		f.Verdict = Leaking
		f.Reasons = append(f.Reasons, measured(MemLeak, f.MemLate, f.MemEarly, Bytes, fmt.Sprintf(
			"memory grew from %s to %s across the window, a %.1fx increase: this is unbounded growth and no limit will hold it. Sizing cannot fix a leak; the recommendation is withheld deliberately.",
			humanBytes(int64(f.MemEarly)), humanBytes(int64(f.MemLate)), f.MemLate/f.MemEarly)))
		return
	}

	// The leak test just came back negative, and for an OOMKilled container
	// that answer is not trustworthy. The kill reset the working set, and the
	// trend pair is keyed by (namespace, pod, container) rather than by
	// container id, so late/early spans both incarnations: the lab's
	// slow-leaker reads 4.1x growth or 0.38x shrinkage depending only on where
	// in its ~41h sawtooth the window edges happened to fall.
	//
	// The suppression is deliberately one-sided. A ratio ABOVE the threshold
	// still fires LEAKING above, because a restart cannot manufacture that much
	// growth and the cost of believing it is a withheld recommendation. A ratio
	// below it is the dangerous direction: it reads as a clean bill of health
	// on the one verdict - SATURATED, memory must not be reduced - that an
	// operator acts on by raising memory, which is the one thing that never
	// helps a leak. So the negative is withheld, not the positive.
	if f.OOMKilled && f.Restarts > 0 && f.has(metrics.SigMemTrend) {
		delete(f.Have, metrics.SigMemTrend)
		f.trendKilledByRestart = true
	}

	// Saturation comes first: a starved container must never be told to shrink.
	// Which resource is starved is tracked separately, because an OOMKill is
	// evidence about memory and a CPU stall is evidence about CPU, and neither
	// says anything about the other. A container can be both, so these are
	// independent tests rather than one switch.
	var cpuStarved, memStarved bool
	if f.OOMKilled {
		f.Verdict, memStarved = Saturated, true
		f.Reasons = append(f.Reasons, note(OOMKilled, "container has been OOMKilled; memory must not be reduced"))
		// Raising memory fixes a container that is simply too small and buys a
		// leak a few hours. The two look identical here, because the kill
		// itself destroyed the evidence that would separate them: the trend
		// pair spans the restart it caused. Say so, rather than let SATURATED
		// be read as "give it more".
		if f.trendKilledByRestart {
			f.Reasons = append(f.Reasons, measured(LeakMasked, f.Restarts, 0, Count, fmt.Sprintf(
				"the %.0f restarts in this window reset its working set, so the memory trend spans "+
					"more than one incarnation and unbounded growth cannot be ruled out. If this "+
					"container is leaking, a larger allocation only delays the next kill; compare "+
					"working set within a single incarnation before applying any increase.",
				f.Restarts)))
		}
	}
	// PSI and CFS throttling measure the same stall; prefer PSI where both exist.
	// Pressure only counts as starvation if the container was using what it
	// was given. See MinSaturationUtil: stalls on a container sitting at a
	// fraction of its request come from the node, not from its own allocation.
	alloc := float64(f.CPURequest)
	if f.CPULimit > 0 {
		alloc = float64(f.CPULimit)
	}
	// Peak usage, not the percentile: a spiky workload is quiet at p95 and
	// starved during its spikes, which is exactly when the pressure appears.
	// Asking "did it ever reach its allocation" is the question that matters.
	util := 1.0
	if alloc > 0 {
		util = max(f.CPUUsageMax, f.CPUSteadyMax) * 1000 / alloc
	}
	wantsCPU := util >= th.MinSaturationUtil
	if f.has(metrics.SigPSI) && f.PSIWaitingPeak > th.PSIWaiting && wantsCPU {
		f.Verdict, cpuStarved = Saturated, true
		f.Reasons = append(f.Reasons, measured(CPUPressure, f.PSIWaitingPeak, th.PSIWaiting, SecPerSec, fmt.Sprintf(
			"CPU pressure %.3f s/s exceeds %.3f while using %.0f%% of its allocation: the container is waiting on CPU it was denied (window mean %.4f)",
			f.PSIWaitingPeak, th.PSIWaiting, util*100, f.PSIWaiting)))
	} else if f.has(metrics.SigThrottle) && f.ThrottlePeak > th.ThrottleRatio && wantsCPU {
		f.Verdict, cpuStarved = Saturated, true
		f.Reasons = append(f.Reasons, measured(CPUThrottled, f.ThrottlePeak, th.ThrottleRatio, Ratio, fmt.Sprintf(
			"CFS throttling peaks at %.1f%% of scheduling periods against a %.1f%% threshold (window mean %.1f%%), while using %.0f%% of its allocation",
			f.ThrottlePeak*100, th.ThrottleRatio*100, f.Throttle*100, util*100)))
	}

	if !wantsCPU && f.has(metrics.SigPSI) && f.PSIWaitingPeak > th.PSIWaiting {
		f.Reasons = append(f.Reasons, measured(NodeContention, f.PSIWaitingPeak, th.PSIWaiting, SecPerSec, fmt.Sprintf(
			"CPU pressure peaked at %.3f s/s but the container used only %.0f%% of its allocation (window mean pressure %.4f), so this is contention on the node rather than a shortage of its own: a larger request would not remove it",
			f.PSIWaitingPeak, util*100, f.PSIWaiting)))
	}

	// The spec is often silent about a ceiling that behaviour makes obvious.
	// Inference runs before the floor is computed so an inferred ceiling
	// participates in it, but only ever to raise the recommendation.
	if inferred := inferCeiling(f, th); inferred != nil {
		f.Ceilings = append(f.Ceilings, *inferred)
	}

	// Recommendations, subject to the ceiling rule.
	floor, blocked := ceilings.MemoryFloor(f.Ceilings)
	cpuCap, hasCPUCap := ceilings.CPUCeiling(f.Ceilings)

	if blocked {
		if f.Verdict != Saturated {
			f.Verdict = Blocked
		}
		for _, c := range f.Ceilings {
			if c.Unresolved {
				f.Reasons = append(f.Reasons, about(CeilingBlocked, string(c.Kind), c.Note))
			}
		}
		return
	}

	recCPU := int64(f.CPUUsageP95 * 1000 * (1 + th.Margin))
	if recCPU < th.MinCPUMillicores {
		f.Reasons = append(f.Reasons, measured(CPUFloor, f.CPUUsageP95*1000, float64(th.MinCPUMillicores), Millicores, fmt.Sprintf(
			"CPU p95 of %.0fm sizes below the %dm floor, so the recommendation is the floor rather than a measurement: this container is too idle for its own usage to size it",
			f.CPUUsageP95*1000, th.MinCPUMillicores)))
		f.CPUFloored = true
		recCPU = th.MinCPUMillicores
	}
	if hasCPUCap && recCPU > cpuCap {
		recCPU = cpuCap
		f.Reasons = append(f.Reasons, measured(CPUCapped, float64(cpuCap), 0, Millicores, fmt.Sprintf("CPU recommendation capped at %dm by a runtime ceiling", cpuCap)))
	}

	steadyPeak := f.CPUUsageMax
	if f.has(metrics.SigStartup) && f.CPUSteadyMax > 0 {
		steadyPeak = f.CPUSteadyMax
	}
	recCPULimit := int64(steadyPeak * 1000 * (1 + th.BurstMargin))

	// Boot is a separate regime. Sizing the limit to steady state throttles
	// every restart, and a throttled boot can fail a readiness probe, which
	// turns an ordinary rollout into an outage.
	if f.has(metrics.SigStartup) && f.CPUStartupMax > 0 && steadyPeak > 0 {
		ratio := f.CPUStartupMax / steadyPeak
		if ratio >= th.StartupRatio {
			// A genuinely distinct boot regime: size for it and say so.
			if startupLimit := int64(f.CPUStartupMax * 1000 * (1 + th.StartupMargin)); startupLimit > recCPULimit {
				f.Reasons = append(f.Reasons, measured(CPUStartup, f.CPUStartupMax*1000, steadyPeak*1000, Millicores, fmt.Sprintf(
					"CPU limit is set by startup, not steady state: this container draws %.0fm while booting against %.0fm afterwards (%.1fx), so a steady-state limit would throttle every restart",
					f.CPUStartupMax*1000, steadyPeak*1000, ratio)))
				recCPULimit = startupLimit
			}
		} else if boot := int64(f.CPUStartupMax * 1000 * (1 + th.BurstMargin)); boot > recCPULimit {
			// Boot and steady state look the same. Cover the peak, quietly.
			recCPULimit = boot
		}
	}

	if recCPULimit < recCPU {
		recCPULimit = recCPU
	}
	if hasCPUCap && recCPULimit > cpuCap {
		recCPULimit = cpuCap
	}

	recMem := int64(f.MemUsageMax * (1 + th.Margin))
	if recMem < th.MinMemBytes {
		f.Reasons = append(f.Reasons, measured(MemFloor, float64(recMem), float64(th.MinMemBytes), Bytes, fmt.Sprintf(
			"memory peak of %s sizes below the %s floor, so the recommendation is the floor rather than a measurement",
			humanBytes(recMem), humanBytes(th.MinMemBytes))))
		f.MemFloored = true
		recMem = th.MinMemBytes
	}
	if floor > 0 && recMem < floor {
		f.Reasons = append(f.Reasons, measured(MemRaised, float64(recMem), float64(floor), Bytes, fmt.Sprintf(
			"memory recommendation raised from %s to %s by a runtime ceiling; observed usage alone would have been unsafe",
			humanBytes(recMem), humanBytes(floor))))
		recMem = floor
	}

	f.RecCPU, f.RecCPULimit, f.RecMem = recCPU, recCPULimit, recMem
	f.RecMemLimit = int64(float64(recMem) * (1 + th.MemLimitHeadroom))
	// An inferred ceiling can raise the recommendation above the floor it was
	// just clamped to, which leaves the flag describing a value that is gone.
	defer f.reconcileFloors(th)

	// A starved container only ever gets an increase, but only in the resource
	// it is actually starved of. Ratcheting both would answer a question the
	// evidence never asked: a CPU-throttled container whose memory is idle
	// would be told to keep a request it has never come close to using, which
	// reads as a measurement and is really the input echoed back.
	if f.Verdict == Saturated {
		if cpuStarved {
			f.RecCPU = max(recCPU, f.CPURequest)
			f.RecCPULimit = max(recCPULimit, f.CPULimit)
		}
		if memStarved {
			f.RecMem = max(recMem, f.MemRequest)
			f.RecMemLimit = max(int64(float64(f.RecMem)*(1+th.MemLimitHeadroom)), f.MemLimit)
		}
		// The floor flags were set from usage alone; a ratcheted value is no
		// longer the floor, and leaving them set makes a real recommendation
		// read as a constant.
		if f.CPULimit > 0 && f.CPUUsageP95*1000 >= float64(f.CPULimit)*0.95 {
			f.Reasons = append(f.Reasons, measured(CPUAtLimit, f.CPUUsageP95*1000, float64(f.CPULimit), Millicores, fmt.Sprintf("usage is pinned at the %dm CPU limit; raise the limit, not just the request", f.CPULimit)))
		}
		return
	}

	// A container that keeps restarting has not run long enough at any one size
	// for its own usage to size it, and the samples that do exist describe a
	// container that kept dying. Reducing it is the dangerous direction,
	// because the allocation is a candidate for what killed it.
	//
	// The absence of an OOM event is not evidence here. tare-lab's slow-leaker
	// filled a pod-scoped tmpfs, so every restart was OOMKilled inside runc
	// init, before the container existed to have an OOM event recorded against
	// it: 34 restarts, OOMKilled false, and a confident WASTE verdict with a
	// 1.17GiB recommendation for a container that had not run in two hours.
	if f.Restarts >= th.RestartChurn {
		f.Verdict = Unverified
		f.Reasons = append(f.Reasons, measured(RestartChurn, f.Restarts, float64(th.RestartChurn), Count, fmt.Sprintf(
			"container restarted %.0f times in the window, so its usage describes a container that kept dying rather than one running at this size; no reduction is proposed. A kill during container init is never recorded as an OOM event, so the absence of one is not evidence that this allocation is adequate.",
			f.Restarts)))
		f.RecCPU = max(f.RecCPU, f.CPURequest)
		f.RecCPULimit = max(f.RecCPULimit, f.CPULimit)
		f.RecMem = max(f.RecMem, f.MemRequest)
		f.RecMemLimit = max(int64(float64(f.RecMem)*(1+th.MemLimitHeadroom)), f.MemLimit)
		return
	}

	// Idle: no measurable work for the whole window. This is the only verdict
	// whose action is deletion rather than a number, so it is guarded harder
	// than any other and says plainly what it cannot rule out.
	//
	// The guards, and what each one is for: the PEAK must be below the
	// threshold, so a job that runs once a day is not called idle; memory must
	// be flat, so a process quietly accumulating something is not called idle;
	// and at least half the window must actually have been observed, so a
	// container deployed this morning is not called idle for the fortnight
	// before it existed.
	if idleReason := isIdle(f, window, th); idleReason != nil {
		f.Verdict = Idle
		f.Reasons = append(f.Reasons, *idleReason)
		f.Reasons = append(f.Reasons, note(IdleCaveat, "what this does NOT rule out: a hot standby, a disaster-recovery replica, "+
			"an operator or controller that only acts on a rare event, a job whose period is "+
			"longer than the window, or a service kept warm on purpose. tare sees CPU and memory, "+
			"not intent. Confirm with the owner before deleting anything."))
		// The sizing numbers stay: if the workload turns out to be needed, the
		// smaller allocation is still the right one. Deletion is the larger
		// saving, not the only one.
		return
	}

	wastedCPU := f.CPURequest > 0 && f.CPUUsageP95*1000 < float64(f.CPURequest)*th.WasteRatio
	wastedMem := f.MemRequest > 0 && f.MemUsageMax < float64(f.MemRequest)*th.WasteRatio
	if wastedCPU || wastedMem {
		f.Verdict = Waste
		if wastedCPU {
			f.Reasons = append(f.Reasons, measured(CPUUnderused, f.CPUUsageP95*1000, float64(f.CPURequest), Millicores, fmt.Sprintf("CPU p95 is %.0fm against a %dm request", f.CPUUsageP95*1000, f.CPURequest)))
		}
		if wastedMem {
			f.Reasons = append(f.Reasons, measured(MemUnderused, f.MemUsageMax, float64(f.MemRequest), Bytes, fmt.Sprintf("memory peak is %s against a %s request", humanBytes(int64(f.MemUsageMax)), humanBytes(f.MemRequest))))
		}
		return
	}
	f.Verdict = Healthy
}

// reconcileFloors clears a floor flag whose recommendation was subsequently
// raised above the floor. The flags exist so a reader can tell a constant from
// a measurement; a stale one mislabels a real number as constant.
func (f *Finding) reconcileFloors(th Thresholds) {
	if f.CPUFloored && f.RecCPU > th.MinCPUMillicores {
		f.CPUFloored = false
	}
	if f.MemFloored && f.RecMem > th.MinMemBytes {
		f.MemFloored = false
	}
}

// isIdle reports why a container counts as doing no work, or nil if it does
// not. It is deliberately a single function returning the reason it justifies:
// the guards and the explanation must not be able to drift apart, because the
// explanation is what a human uses to decide whether to delete a workload.
func isIdle(f *Finding, window time.Duration, th Thresholds) *Reason {
	// Nothing reserved, nothing to reclaim. An idle container with no requests
	// costs the scheduler nothing and frees nothing when deleted, so the
	// verdict would be an observation about a quiet process rather than a
	// saving. Measured on the lab cluster this guard is the difference between
	// 8 idle findings and 3: node-exporter, kube-state-metrics, both Grafana
	// sidecars and the config-reloader all sit below the CPU threshold, all
	// have unset requests, and all are load-bearing. Telling an operator the
	// larger saving is to delete their metrics pipeline is exactly the clean,
	// plausible, wrong answer this tool exists not to produce.
	if f.CPURequest <= th.MinCPUMillicores && f.MemRequest <= th.MinMemBytes {
		return nil
	}
	peak := max(f.CPUUsageMax, f.CPUSteadyMax, f.CPUStartupMax) * 1000
	if peak >= th.IdleCPUMillicores {
		return nil
	}
	// Flat memory. MemEarly is zero when no pod had an early observation, in
	// which case the trend was never evaluated and must not be read as flat.
	if !f.has(metrics.SigMemTrend) || f.MemEarly <= 0 {
		return nil
	}
	if f.MemLate > f.MemEarly*th.IdleMemGrowth {
		return nil
	}
	if expected := int(window.Seconds() / 15); expected > 0 &&
		float64(f.Samples) < float64(expected)*th.IdleMinCoverage {
		return nil
	}
	r := measured(IdleWindow, peak, th.IdleCPUMillicores, Millicores, fmt.Sprintf(
		"no measurable work in the window: CPU peaked at %.1fm (below the %.0fm idle threshold) and memory stayed flat at %s. "+
			"Resizing this saves %dm of CPU; deleting it saves that on every replica and the operational surface as well.",
		peak, th.IdleCPUMillicores, humanBytes(int64(f.MemUsageMax)), f.CPURequest))
	return &r
}

// confidence rates the evidence. Sample counts are compared against what a 15
// second scrape interval would produce over the window.
func confidence(samples int, window time.Duration) Confidence {
	if samples == 0 {
		return Low
	}
	expected := int(window.Seconds() / 15)
	switch {
	case samples < 240: // under an hour of data
		return Low
	case expected > 0 && samples*3 < expected: // less than a third of the window
		return Low
	case samples < 5760: // under a day
		return Medium
	}
	return High
}

func humanBytes(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1fGi", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.0fMi", float64(b)/(1<<20))
	default:
		return fmt.Sprintf("%dB", b)
	}
}

// keyOf builds the constraints index key from a namespace and a "Kind/name"
// workload string.
func keyOf(ns, workload string) string {
	parts := strings.SplitN(workload, "/", 2)
	if len(parts) != 2 {
		return ns + "/" + workload
	}
	return ns + "/" + parts[0] + "/" + parts[1]
}
