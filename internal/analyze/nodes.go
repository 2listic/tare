package analyze

import (
	"fmt"
	"strings"

	"github.com/mfranzon/tare/internal/ceilings"
	"github.com/mfranzon/tare/internal/discover"
	"github.com/mfranzon/tare/internal/metrics"
)

// threadPoolRatio is how many times more cores the node must have than the
// container's CPU quota before an unconfigured runtime thread pool is worth
// reporting. At 4x a runtime sized to the node burns its whole quota in the
// first quarter of every scheduling period and stalls for the remainder; below
// that the effect exists but is not worth a line in the report.
const threadPoolRatio = 4

// applyNodes adds the findings that need to know what the container is running
// on. Every one of them is a refusal or a configuration finding: none changes a
// number on the strength of hardware alone, because the one thing hardware
// data would be needed for - how fast a core on THIS machine is relative to
// that one - cannot be recovered without running a benchmark.
func applyNodes(f *Finding, inv discover.Inventory, util map[string]metrics.NodeObservation, th Thresholds) {
	if inv.Partial || len(inv.Nodes) == 0 {
		return
	}
	perNodeWorkload(f)
	mixedHardware(f)
	threadPoolMismatch(f, inv)
	unschedulable(f, inv)
	limitBoundOnIdleNode(f, util, th)
}

// limitBoundOnIdleNode reports a container held back by its own CPU quota while
// the machine underneath it had capacity to spare.
//
// This is the one case where throttling is unambiguously self-inflicted. CFS
// enforces a quota per period regardless of what the node is doing, so a
// container can stall repeatedly a few microseconds away from idle cores. The
// question of whether to set CPU limits at all has been argued for years on
// both sides without measurement; the ingredients to settle it per workload -
// throttle ratio, pressure, and the node's own utilisation - are all here.
//
// It says nothing about latency. tare has no SLI signal, and a figure like
// "this cost you 180ms of p99" would be invented rather than measured.
func limitBoundOnIdleNode(f *Finding, util map[string]metrics.NodeObservation, th Thresholds) {
	if f.CPULimit <= 0 {
		return // no quota, so nothing can be bound by one
	}
	throttled := f.ThrottlePeak > th.ThrottleRatio
	pressured := f.psiStalled(th)
	if !throttled && !pressured {
		return
	}

	p95, mean, known := worstNodeUtil(f.Nodes, util)
	f.NodeUtilP95, f.NodeUtilKnown = p95, known

	evidence := fmt.Sprintf("throttled %.1f%% of scheduling periods", f.ThrottlePeak*100)
	if !throttled {
		evidence = fmt.Sprintf("CPU pressure peaked at %.3f s/s", f.PSIWaitingPeak)
	}

	if !known {
		// Silence here would read as "the node was fine". Name the missing
		// input instead: without it the two explanations for the same stall -
		// your quota, or a full machine - cannot be told apart.
		f.Reasons = append(f.Reasons, note(NodeUtilUnknown, fmt.Sprintf(
			"%s against a %dm CPU limit. Whether the node had spare capacity at the time is unknown: no node utilisation metrics (node_cpu_seconds_total, normally from node-exporter) were found for the nodes this workload ran on, so tare cannot say whether the limit or the machine was the constraint.",
			evidence, f.CPULimit)))
		return
	}
	if mean >= th.NodeIdleCeiling || p95 >= th.NodeBusyP95 {
		// The machine was genuinely busy, either throughout or at its worst.
		// The stall may still be the quota's doing, but "there was room going
		// spare" is not a claim this evidence supports, so it is not made.
		// Both gates are needed: the mean catches a node that was busy all
		// window, the peak catches one that averaged low and saturated during
		// the hours that mattered, which tare cannot correlate in time.
		return
	}

	f.Reasons = append(f.Reasons, measured(LimitNotNode, p95*100, th.NodeBusyP95*100, Percent, fmt.Sprintf(
		"%s against a %dm CPU limit, while the busiest node it ran on averaged %.1f%% CPU across the window and stayed below %.1f%% for 95%% of it. "+
			"The constraint is this container's own limit, not the machine: the capacity it was denied was idle a few microseconds away, and CFS enforces a quota per period whatever the node is doing. "+
			"Raising or removing the limit buys back that time without buying hardware. It is not free either: check the node commitment table below first, because limits are what stop a busy node from being oversubscribed.",
		evidence, f.CPULimit, mean*100, p95*100)))
}

// worstNodeUtil returns the highest peak utilisation across the nodes a
// workload ran on, and the mean belonging to that same node.
//
// The busiest node governs on purpose. A workload spread over an idle node and
// a saturated one has no safe claim that "the node had room", so taking the
// maximum withholds the finding in exactly the case where it would be wrong.
func worstNodeUtil(names []string, util map[string]metrics.NodeObservation) (p95, mean float64, known bool) {
	for _, n := range names {
		o, ok := util[n]
		if !ok || o.Samples == 0 {
			continue
		}
		if !known || o.CPUUtilP95 > p95 {
			p95, mean = o.CPUUtilP95, o.CPUUtilMean
		}
		known = true
	}
	return p95, mean, known
}

// perNodeWorkload states the multiplier a DaemonSet's numbers carry.
//
// The arithmetic in the summary is already right, because a DaemonSet has one
// pod per node and the replica multiplier therefore counts nodes. What is
// missing is that the reader cannot tell: 40 pods reads as an over-replicated
// Deployment, and the two call for opposite reactions. A DaemonSet's request
// is also subtracted from every node's allocatable before anything else is
// scheduled, so it sets the per-node overhead that decides how many nodes the
// cluster needs at all.
func perNodeWorkload(f *Finding) {
	if f.WorkloadKind != "DaemonSet" || f.Pods == 0 {
		return
	}
	f.Reasons = append(f.Reasons, measured(PerNodeWorkload, float64(f.Pods), 0, Count, fmt.Sprintf(
		"per-node workload: the %d replicas are %d nodes, so this saving scales with the cluster rather than with traffic and applies to every node added later. "+
			"Its request is also subtracted from every node's allocatable before anything else is scheduled, which makes it one of the few workloads whose size changes how many nodes the cluster needs.",
		f.Pods, f.Pods)))
}

// mixedHardware lowers confidence when the usage being summarised came from
// more than one class of machine.
//
// It deliberately does not correct the numbers. A millicore on one
// architecture is not a millicore on another, the ratio is not recoverable
// from observational data, and inventing a scaling factor would be exactly the
// failure this project keeps finding: a clean, plausible number with nothing
// behind it. Saying "this is less certain, and here is why" is the honest
// available answer.
func mixedHardware(f *Finding) {
	if len(f.NodeClasses) < 2 {
		return
	}
	f.Confidence = Low
	f.Reasons = append(f.Reasons, measuredAbout(MixedHardware, strings.Join(f.NodeClasses, ", "), float64(len(f.NodeClasses)), 0, Count, fmt.Sprintf(
		"this workload's pods ran on %d different node classes (%s), so the usage summarised here was produced by hardware of different speeds and the worst case is sized for. "+
			"tare does not normalise across hardware: the per-core performance ratio cannot be measured from observation alone, and guessing it would make the recommendation less trustworthy, not more. Confidence is lowered instead.",
		len(f.NodeClasses), strings.Join(f.NodeClasses, ", "))))
}

// threadPoolMismatch reports a runtime that sized its thread pool from the
// node instead of from its cgroup quota.
//
// This is the case where throttling is a configuration defect and raising the
// limit is the wrong fix. A Go process with GOMAXPROCS unset on a 96-core node
// under a 2-core quota runs 96 Ps, exhausts the quota in the first few
// milliseconds of each 100ms period and stalls for the rest of it, and the
// same shape appears in a JVM whose ActiveProcessorCount is unset.
func threadPoolMismatch(f *Finding, inv discover.Inventory) {
	if f.CPULimit <= 0 {
		return // no quota, no period to exhaust
	}
	cores := maxCoresAmong(f.Nodes, inv)
	if cores <= 0 {
		return
	}
	quotaCores := float64(f.CPULimit) / 1000
	if quotaCores <= 0 || float64(cores)/quotaCores < threadPoolRatio {
		return
	}

	// An explicit setting that disagrees with the quota is a measurement. An
	// absent one on a runtime we can recognise is an inference, and says so.
	for _, c := range f.Ceilings {
		if c.Kind != ceilings.GoMaxProcs || c.Millicores <= 0 {
			continue
		}
		if set := c.Millicores / 1000; float64(set) > quotaCores*2 {
			f.Reasons = append(f.Reasons, measured(GoMaxProcs, float64(set), quotaCores, CoreCount, fmt.Sprintf(
				"GOMAXPROCS is set to %d but the CPU limit is %.2f cores: the runtime schedules %d threads against a quota that can satisfy %.2f, so it exhausts its quota early in each 100ms period and stalls for the remainder. This is a configuration defect, not a sizing one, and raising the limit is the more expensive fix. Set GOMAXPROCS to the quota, or use uber-go/automaxprocs.",
				set, quotaCores, set, quotaCores)))
		}
		return // an explicit value was found; nothing to infer
	}

	if !looksManagedRuntime(f.Image) {
		// A Go binary in a scratch or distroless image is invisible here, so
		// silence is not evidence of correctness. Say nothing rather than
		// guess: an unrecognised image is the common case, and a warning
		// printed on every one of them would be noise.
		return
	}
	f.Reasons = append(f.Reasons, measured(ThreadPoolUnset, float64(cores), quotaCores, CoreCount, fmt.Sprintf(
		"this container runs on a %d-core node under a %.2f-core limit (%.0fx) and no thread-pool setting was found in its spec. "+
			"A managed runtime that sizes its pools from the visible core count will start ~%d threads against a quota that can satisfy %.2f, exhausting each 100ms period early and stalling for the remainder. "+
			"Check GOMAXPROCS, -XX:ActiveProcessorCount or the equivalent before treating any throttling here as a sizing problem. The setting may be inside the image, where the pod spec cannot see it.",
		cores, quotaCores, float64(cores)/quotaCores, cores, quotaCores)))
}

// unschedulable refuses a recommendation no node in the cluster can hold.
//
// A raised request larger than the biggest allocatable node does not produce a
// bigger pod, it produces a Pending one, which is a worse outcome than the
// throttling it was meant to fix.
func unschedulable(f *Finding, inv discover.Inventory) {
	cpu, mem := inv.Largest()
	if cpu == 0 && mem == 0 {
		return // every node cordoned, or no allocatable reported
	}
	if cpu > 0 && f.RecCPU > cpu {
		f.Reasons = append(f.Reasons, measured(UnschedulableCPU, float64(f.RecCPU), float64(cpu), Millicores, fmt.Sprintf(
			"the recommended %dm CPU request exceeds the largest schedulable node's %dm allocatable: applying it makes the pod Pending rather than faster. The workload needs a bigger node class or a smaller unit of work, and tare cannot size its way out of this.",
			f.RecCPU, cpu)))
	}
	if mem > 0 && f.RecMem > mem {
		f.Reasons = append(f.Reasons, measured(UnschedulableMem, float64(f.RecMem), float64(mem), Bytes, fmt.Sprintf(
			"the recommended %s memory request exceeds the largest schedulable node's %s allocatable: applying it makes the pod Pending rather than safer.",
			HumanBytes(f.RecMem), HumanBytes(mem))))
	}
}

func maxCoresAmong(names []string, inv discover.Inventory) int64 {
	var out int64
	for _, name := range names {
		if n, ok := inv.Get(name); ok {
			out = max(out, n.Cores)
		}
	}
	return out
}
