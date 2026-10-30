// Package analyze turns container specs and observations into findings.
//
// The ordering principle: saturation evidence first, then blocked
// recommendations, then waste. A platform engineer reading the report should
// meet the containers that are hurting before the ones that are merely
// expensive.
package analyze

import (
	"cmp"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/2listic/tare/internal/ceilings"
	"github.com/2listic/tare/internal/discover"
	"github.com/2listic/tare/internal/metrics"
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
	Unset      Verdict = "UNSET"      // no requests and no limits; recommendations are a starting point, not a reduction
	Healthy    Verdict = "OK"
	NoData     Verdict = "NO-DATA"
	Conflicted Verdict = "CONFLICT" // another controller already manages these fields
)

// ReportOrder is every verdict, in the order a summary should list them. It
// lives beside the constants because the alternative - a list written out at
// the call site - silently drops any verdict added later, and a verdict
// missing from the summary is a finding the reader never learns exists.
func ReportOrder() []Verdict {
	return []Verdict{Conflicted, Saturated, Leaking, Blocked, Idle, Unverified, Waste, Unset, Healthy, NoData}
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
	groups := aggregate(containers, obs, th)
	out := make([]Finding, 0, len(groups))
	for _, f := range groups {
		sort.Strings(f.Nodes)
		f.NodeClasses = cl.Nodes.Classes(f.Nodes)
		// The order is load-bearing. decide sets the verdict and the numbers;
		// holdGuaranteed is the last step that moves them before anything
		// reads them; the annotators after it add what the cluster knows;
		// applyConstraints may zero the numbers for a workload another
		// controller owns; reconcileFloors then clears floor flags the steps
		// above made stale; applySignalGaps reads the final numbers to say
		// which of them stand on a missing signal.
		c := cl.Constraints[discover.ConstraintKey(f.Namespace, f.Workload)]
		decide(f, window, th)
		holdGuaranteed(f, c)
		applyNodes(f, cl.Nodes, cl.NodeUtil, th)
		applyInference(f, cl.Nodes)
		applyAuthored(f, f.Authored)
		authoredQoSNote(f, f.Authored)
		applyConstraints(f, c)
		f.reconcileFloors(th)
		applySignalGaps(f)
		holdTooYoung(f, th)
		out = append(out, *f)
	}
	sortFindings(out)
	return out
}

// aggregate groups containers by workload plus container name, because a
// recommendation applies to the workload, not to an individual pod, and joins
// each group with the worst case of its pods' observations. That includes pods
// that no longer exist: a rollout or a reschedule replaces every pod, and a
// workload measured from its live pods alone is measured from whenever it was
// last deployed, however wide the window.
func aggregate(containers []discover.Container, obs map[metrics.Key]*metrics.Observation, th Thresholds) map[groupKey]*Finding {
	groups := map[groupKey]*Finding{}
	// worstGrowth tracks, per group, the largest late/early memory ratio seen
	// on any single pod, so the trend pair kept below comes from one pod.
	worstGrowth := map[groupKey]float64{}
	spans := map[groupKey][]*metrics.Observation{}
	live := map[metrics.Key]bool{}

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

		k := metrics.Key{Namespace: c.Namespace, Pod: c.Pod, Container: c.Name}
		live[k] = true
		o := obs[k]
		if o == nil {
			continue
		}
		fold(f, o, true, th)
		spans[gk] = append(spans[gk], o)

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

	// Pods that are gone, credited to the workload that owned them. A workload
	// that is itself gone has no row to join and is skipped. Their trend pair
	// is not used: a dead pod has no late hour, and pairing its early hour
	// with a live pod's late one is the cross-pod comparison ruled out above.
	for k, o := range obs {
		if live[k] || o.Owner == "" {
			continue
		}
		gk := groupKey{k.Namespace, o.Owner, k.Container}
		f, ok := groups[gk]
		if !ok {
			continue
		}
		fold(f, o, false, th)
		spans[gk] = append(spans[gk], o)
	}

	for gk, f := range groups {
		f.Samples = coverage(spans[gk])
	}
	return groups
}

// fold merges one pod's observation into its workload's finding, taking the
// worst case: the recommendation must hold for every replica, not for the
// average one. A dead pod contributes everything but the memory trend, whose
// pair aggregate takes from live pods only, and CPU wait it could not be
// judged on alone.
func fold(f *Finding, o *metrics.Observation, live bool, th Thresholds) {
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
	wait := o.Has(metrics.SigCPUWait) && (live || usableAlone(o.CPUWait, th))
	if wait {
		mergeCPUWait(&f.CPUWait, o.CPUWait, !f.hasSignal(metrics.SigCPUWait), th)
	}
	for sig, ok := range o.Have {
		if !ok || (!live && sig == metrics.SigMemTrend) || (sig == metrics.SigCPUWait && !wait) {
			continue
		}
		if f.Have == nil {
			f.Have = map[metrics.Signal]bool{}
		}
		f.Have[sig] = true
	}
	if o.OOMEvents > 0 {
		f.OOMKilled = true
	}
}

// coverage is how many scrapes' worth of the window a workload's pods cover
// between them. Summing their samples would rate three replicas over one day
// as three days of evidence, so the sum is divided by how many pods ran at
// once on average: the total of their spans over the union of them. Spans
// alone are not enough, because a span is first-to-last and a pod that lived
// through a week of outages spans the week; the samples are what was actually
// taken. Never below the best single pod, which is what tare reported before
// spans existed. Without spans for every pod - the simulator, or a failed
// query - only the best single pod can be trusted.
func coverage(obs []*metrics.Observation) int {
	var best, sum int
	type span struct{ from, to float64 }
	var ss []span
	known := true
	for _, o := range obs {
		best = max(best, o.Samples)
		sum += o.Samples
		if o.LastSeen == 0 {
			known = false
		}
		ss = append(ss, span{o.FirstSeen, o.LastSeen})
	}
	if !known {
		return best
	}
	slices.SortFunc(ss, func(a, b span) int { return cmp.Compare(a.from, b.from) })
	var union, total, end float64
	for _, s := range ss {
		total += s.to - s.from
		from := max(s.from, end)
		if s.to > from {
			union += s.to - from
		}
		end = max(end, s.to)
	}
	if total <= 0 {
		return best
	}
	return max(best, int(float64(sum)*union/total))
}

// usableAlone reports whether a gone pod's CPU wait says anything about its
// workload: enough history for its own normal, and an agent that saw it. A
// live replica's blind spot is doubt about the workload as it runs now; a gone
// pod's is only missing evidence about the past.
func usableAlone(c metrics.CPUWait, th Thresholds) bool {
	return c.Stretches >= th.CPUWaitMinStretches && (!c.Busy || c.Coverage >= th.CPUWaitMinCoverage)
}

// mergeCPUWait folds one pod's CPU wait into its workload's, worst case per
// field. The normal is each pod's own, so a stall only counts from a pod with
// the history to have one: a replica an hour old next to one a week old has a
// normal made of that hour, and twice it is no evidence. Short pods decide
// only while no pod has a history, and the row is then withdrawn as short
// history anyway. Coverage is taken from every pod: one replica the agent
// cannot see is enough to doubt the workload. So is a run of stalled stretches
// its node confirmed: the node, not the pod's normal, established its cause,
// so a replacement pod starved since its first hour is starved whatever its
// predecessor's history says.
//
// The stall fields travel together, from the most stalled pod, since that is
// the pod the verdict is about; among equally stalled pods, the highest
// normal, so a never-quiet replica is not hidden by a quiet one.
func mergeCPUWait(dst *metrics.CPUWait, c metrics.CPUWait, first bool, th Thresholds) {
	if first {
		*dst = c
		return
	}
	long, settled := c.Stretches >= th.CPUWaitMinStretches, dst.Stretches >= th.CPUWaitMinStretches
	cov, busy, confirmed := dst.Coverage, dst.Busy, dst.LongestConfirmedRun
	switch {
	case long && !settled:
		// The first pod with a history of its own replaces the short ones.
		*dst = c
	case !long && settled:
		// Only its coverage and confirmed run below.
	default:
		if c.Stalled > dst.Stalled || (c.Stalled == dst.Stalled && c.HasNormal && c.Normal > dst.Normal) {
			dst.Stalled, dst.Normal, dst.HasNormal = c.Stalled, c.Normal, c.HasNormal
			dst.NodeKnown, dst.NodeContended = c.NodeKnown, c.NodeContended
		}
		dst.LongestRun = max(dst.LongestRun, c.LongestRun)
		dst.Mean = max(dst.Mean, c.Mean)
		dst.Stretches = max(dst.Stretches, c.Stretches)
	}
	dst.LongestConfirmedRun = max(confirmed, dst.LongestConfirmedRun, c.LongestConfirmedRun)
	switch {
	case busy && c.Busy:
		dst.Coverage, dst.Busy = min(cov, c.Coverage), true
	case busy:
		dst.Coverage, dst.Busy = cov, true
	case c.Busy:
		dst.Coverage, dst.Busy = c.Coverage, true
	}
}

type groupKey struct{ ns, workload, container string }

// sortFindings orders findings most severe first.
func sortFindings(out []Finding) {
	sort.Slice(out, func(i, j int) bool {
		if p := severity(out[i].Verdict) - severity(out[j].Verdict); p != 0 {
			return p > 0
		}
		// within a verdict, worst pressure first, then biggest waste
		if out[i].PSIWaiting != out[j].PSIWaiting {
			return out[i].PSIWaiting > out[j].PSIWaiting
		}
		if out[i].CPURequest != out[j].CPURequest {
			return out[i].CPURequest > out[j].CPURequest
		}
		// groups is a map, so without an identity tie-break equal findings
		// came out in a different order on every run.
		return identity(out[i]) < identity(out[j])
	})
}

func identity(f Finding) string {
	return f.Namespace + "\x00" + f.Workload + "\x00" + f.Container
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
	case Unset:
		return 2
	case Healthy:
		return 1
	}
	return 0
}

// HumanBytes renders a byte count the way the report spells it: Gi to one
// decimal, Mi whole, bytes below that.
func HumanBytes(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1fGi", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.0fMi", float64(b)/(1<<20))
	default:
		return fmt.Sprintf("%dB", b)
	}
}
