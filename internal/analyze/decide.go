package analyze

import (
	"fmt"
	"time"

	"github.com/2listic/tare/internal/ceilings"
	"github.com/2listic/tare/internal/metrics"
)

// pinnedAtLimit is how close CPU p95 must sit to the limit before usage counts
// as pinned there rather than merely high.
const pinnedAtLimit = 0.95

// decide classifies one container and sizes its recommendation.
//
// The steps run in a fixed order and the order is the policy: a leak withholds
// everything, a ceiling that cannot be resolved blocks sizing, a starved
// resource is never reduced, a container that keeps restarting is never
// reduced, and only then is anything called idle or wasteful. Every step
// appends its reasons in the order it decides, so the evidence reads in the
// order the verdict was reached.
func decide(f *Finding, window time.Duration, th Thresholds) {
	f.Confidence = confidence(f.Samples, window)

	if f.Samples == 0 {
		f.Verdict = NoData
		f.Reasons = append(f.Reasons, note(NoSamples, "no metrics found for this container in the window"))
		return
	}
	if detectLeak(f, th) {
		return
	}
	maskTrendAfterOOM(f)
	maskUnusableCPUWait(f, th)
	st := assessSaturation(f, th)

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
		if !st.any() {
			f.Verdict = Blocked
		}
		for _, c := range f.Ceilings {
			if c.Unresolved {
				f.Reasons = append(f.Reasons, about(CeilingBlocked, string(c.Kind), c.Note))
			}
		}
		return
	}

	f.RecCPU = sizeCPURequest(f, th, cpuCap, hasCPUCap)
	f.RecCPULimit = sizeCPULimit(f, th, st.cpu, f.RecCPU, cpuCap, hasCPUCap)
	f.RecMem = sizeMemRequest(f, th, floor)
	f.RecMemLimit = memLimitFor(f.RecMem, th)

	switch {
	case st.any():
		holdStarved(f, th, st)
		// Starvation holds only the starved resource. A container that also
		// keeps restarting must not have the other one reduced either: a
		// CPU-throttled container with 34 restarts was being told to cut
		// memory sized from the samples of a container that kept dying.
		if f.Restarts >= th.RestartChurn {
			holdRestartChurn(f, th, st)
		}
	case f.Restarts >= th.RestartChurn:
		holdRestartChurn(f, th, starved{})
	default:
		if idleReason := isIdle(f, window, th); idleReason != nil {
			markIdle(f, *idleReason)
			noteLoweredLimits(f)
			return
		}
		classifyWaste(f, th)
	}
}

// detectLeak reports a leak, which outranks everything: no allocation survives
// unbounded growth, so reporting it as a sizing problem sends the operator down
// the wrong path. Restarts are deliberately NOT a disqualifier here: a leak bad
// enough to OOMKill has restarts by definition, and that is the case that
// matters most. A container whose restarts reset memory shows late close to
// early and fails the ratio test on its own.
func detectLeak(f *Finding, th Thresholds) bool {
	if !f.hasSignal(metrics.SigMemTrend) || f.MemEarly <= 0 || f.MemLate <= f.MemEarly*th.LeakRatio {
		return false
	}
	f.Verdict = Leaking
	f.Reasons = append(f.Reasons, measured(MemLeak, f.MemLate, f.MemEarly, Bytes, fmt.Sprintf(
		"memory grew from %s to %s across the window, a %.1fx increase: this is unbounded growth and no limit will hold it. Sizing cannot fix a leak; the recommendation is withheld deliberately.",
		HumanBytes(int64(f.MemEarly)), HumanBytes(int64(f.MemLate)), f.MemLate/f.MemEarly)))
	return true
}

// maskTrendAfterOOM discards a negative leak test on an OOMKilled container,
// because that answer is not trustworthy. The kill reset the working set, and
// the trend pair is keyed by (namespace, pod, container) rather than by
// container id, so late/early spans both incarnations: the lab's slow-leaker
// reads 4.1x growth or 0.38x shrinkage depending only on where in its ~41h
// sawtooth the window edges happened to fall.
//
// The suppression is deliberately one-sided. A ratio ABOVE the threshold still
// fires LEAKING, because a restart cannot manufacture that much growth and the
// cost of believing it is a withheld recommendation. A ratio below it is the
// dangerous direction: it reads as a clean bill of health on the one verdict -
// SATURATED, memory must not be reduced - that an operator acts on by raising
// memory, which is the one thing that never helps a leak. So the negative is
// withheld, not the positive.
func maskTrendAfterOOM(f *Finding) {
	if f.OOMKilled && f.Restarts > 0 && f.hasSignal(metrics.SigMemTrend) {
		delete(f.Have, metrics.SigMemTrend)
		f.trendKilledByRestart = true
	}
}

// starved records which resource a container is starved of. They are tracked
// separately because an OOMKill is evidence about memory and a CPU stall is
// evidence about CPU, and neither says anything about the other.
type starved struct{ cpu, mem bool }

func (s starved) any() bool { return s.cpu || s.mem }

// assessSaturation runs first among the sizing decisions: a starved container
// must never be told to shrink. A container can be starved of both resources,
// so these are independent tests rather than one switch.
func assessSaturation(f *Finding, th Thresholds) starved {
	var st starved
	if f.OOMKilled {
		f.Verdict, st.mem = Saturated, true
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
	if f.psiStalled(th) && wantsCPU {
		f.Verdict, st.cpu = Saturated, true
		f.Reasons = append(f.Reasons, measured(CPUPressure, f.PSIWaitingPeak, th.PSIWaiting, SecPerSec, fmt.Sprintf(
			"CPU pressure %.3f s/s exceeds %.3f while using %.0f%% of its allocation: the container is waiting on CPU it was denied (window mean %.4f)",
			f.PSIWaitingPeak, th.PSIWaiting, util*100, f.PSIWaiting)))
	} else if f.cpuWaitStalled(th) && wantsCPU {
		f.Verdict, st.cpu = Saturated, true
		if f.CPUWait.Stalled >= th.CPUWaitStalledShare {
			f.Reasons = append(f.Reasons, measured(CPUWaitRelative, f.CPUWait.Stalled, th.CPUWaitStalledShare, Ratio, fmt.Sprintf(
				"CPU wait (tare-agent; this container has no PSI) shows it stalled in %.0f%% of 5-minute stretches against a %.0f%% threshold, "+
					"while using %.0f%% of its allocation: its tasks waited for CPU %s",
				f.CPUWait.Stalled*100, th.CPUWaitStalledShare*100, util*100, cpuWaitStall(f.CPUWait))))
		} else {
			f.Reasons = append(f.Reasons, measured(CPUWaitNodeConfirmed, float64(f.CPUWait.LongestConfirmedRun), float64(th.CPUWaitStalledRun), Count, fmt.Sprintf(
				"CPU wait (tare-agent; this container has no PSI) shows its tasks waiting for CPU for %s in a row while its node was short of CPU, "+
					"using %.0f%% of its allocation. Only %.1f%% of the window, but the node confirms the cause: a starvation at one time of day, a batch run or an incident",
				stretchesDuration(f.CPUWait.LongestConfirmedRun), util*100, f.CPUWait.Stalled*100)))
		}
	} else if f.throttleStalled(th) && wantsCPU {
		f.Verdict, st.cpu = Saturated, true
		f.Reasons = append(f.Reasons, measured(CPUThrottled, f.ThrottlePeak, th.ThrottleRatio, Ratio, fmt.Sprintf(
			"CFS throttling peaks at %.1f%% of scheduling periods against a %.1f%% threshold (window mean %.1f%%), while using %.0f%% of its allocation",
			f.ThrottlePeak*100, th.ThrottleRatio*100, f.Throttle*100, util*100)))
	}

	// A throttled container's stalls are its own quota, however low its
	// averaged usage reads: CFS enforces the quota per 100ms period and a
	// 5-minute rate hides the bursts that hit it. Blaming the node there
	// contradicts LIMIT_NOT_NODE on the same row.
	if !wantsCPU && f.psiStalled(th) && !f.throttleStalled(th) {
		f.Reasons = append(f.Reasons, measured(NodeContention, f.PSIWaitingPeak, th.PSIWaiting, SecPerSec, fmt.Sprintf(
			"CPU pressure peaked at %.3f s/s but the container used only %.0f%% of its allocation (window mean pressure %.4f), so this is contention on the node rather than a shortage of its own: a larger request would not remove it",
			f.PSIWaitingPeak, util*100, f.PSIWaiting)))
	} else if !wantsCPU && f.cpuWaitStalled(th) {
		f.Reasons = append(f.Reasons, measured(NodeContention, f.CPUWait.Stalled, th.CPUWaitStalledShare, Ratio, fmt.Sprintf(
			"CPU wait (tare-agent) shows it stalled in %.0f%% of 5-minute stretches but the container used only %.0f%% of its allocation, so this is contention on the node rather than a shortage of its own: a larger request would not remove it",
			f.CPUWait.Stalled*100, util*100)))
	}
	return st
}

// cpuWaitStalled reports the relative slowdown rule firing on enough of the
// window. PSI comes first wherever it exists: the two disagree mostly about
// mild waiting, and picking a winner per disagreement would make the verdict
// depend on which instrument happened to be louder. CPU wait fills the gap
// where PSI is missing; it is not a second opinion beside it.
//
// A run of stalled stretches its node confirmed counts too, however short
// against the window: its cause is known, as a PSI peak's is.
func (f *Finding) cpuWaitStalled(th Thresholds) bool {
	return !f.hasSignal(metrics.SigPSI) && f.hasSignal(metrics.SigCPUWait) &&
		(f.CPUWait.Stalled >= th.CPUWaitStalledShare || f.CPUWait.LongestConfirmedRun >= th.CPUWaitStalledRun)
}

// cpuWaitStall says how much worse than usual the stalled stretches were.
func cpuWaitStall(c metrics.CPUWait) string {
	if !c.HasNormal {
		return "at least as long as they ran"
	}
	if c.NodeKnown > 0 {
		return fmt.Sprintf("while its node was short of CPU, or at least twice as long per second of running as is normal for it (normal %.3f s waited per s run)", c.Normal)
	}
	return fmt.Sprintf("at least twice as long per second of running as is normal for it (normal %.3f s waited per s run), or as long as they ran", c.Normal)
}

// maskUnusableCPUWait withdraws CPU wait the agent measured but that cannot be
// believed for this container, and says why. Withdrawn, the container falls
// to throttling or to UNVERIFIED, as if the agent were absent, which is the
// truth: a container whose tasks the agent cannot see shows no wait, and no
// wait reads as "safe to shrink". The same holds for a container contended
// all the time, whose normal is the contention.
//
// A long run of stalled stretches under the stalled share is the other way
// the rule's "not starved" is wrong: the share is diluted by the window. That
// keeps CPU wait but marks the finding, so a reduction becomes UNVERIFIED.
// Neither case adds SATURATED: both are where the rule cannot tell, not where
// it found starvation.
//
// The node, where it is known, tells in both cases. A run its node's
// contention confirmed is starvation (cpuWaitStalled). A container with no
// quiet normal on a node known not to be contended is not contended all the
// time: its high normal is how it runs (many short wake-ups), and stands.
//
// Only for containers without PSI. Where PSI decides, CPU wait is not read,
// and a caveat about it would be noise.
func maskUnusableCPUWait(f *Finding, th Thresholds) {
	if !f.hasSignal(metrics.SigCPUWait) || f.hasSignal(metrics.SigPSI) {
		return
	}
	c := f.CPUWait
	switch {
	case c.Stretches < th.CPUWaitMinStretches:
		f.Reasons = append(f.Reasons, measured(CPUWaitShortHistory, float64(c.Stretches), float64(th.CPUWaitMinStretches), Count, fmt.Sprintf(
			"tare-agent has %s of CPU wait for this container, and the rule compares it with its own normal, which needs at least %s, quiet periods included. CPU wait is not used yet",
			stretchesDuration(c.Stretches), stretchesDuration(th.CPUWaitMinStretches))))
	case c.Busy && c.Coverage < th.CPUWaitMinCoverage:
		f.Reasons = append(f.Reasons, measured(CPUWaitLowCoverage, c.Coverage, th.CPUWaitMinCoverage, Ratio, fmt.Sprintf(
			"tare-agent accounted only %.0f%% of this container's CPU time (needs %.0f%%), so it is not seeing the container's tasks and their lack of wait is not evidence. "+
				"CPU wait is not used; see tare_agent_exits_unattributed_total on its node",
			c.Coverage*100, th.CPUWaitMinCoverage*100)))
	case c.Stalled >= th.CPUWaitStalledShare, c.LongestConfirmedRun >= th.CPUWaitStalledRun:
		// Starved by the rule: whatever else is true, that answer stands.
		return
	case c.HasNormal && c.Normal >= th.CPUWaitMaxNormal && c.NodeKnown < th.CPUWaitMinNodeKnown:
		// With its node known, a high normal needs no mask: had the node been
		// contended, its stretches would be confirmed and stalled above; it was
		// not, so the high normal is how this container runs, and is its normal.
		f.Reasons = append(f.Reasons, measured(CPUWaitNeverQuiet, c.Normal, th.CPUWaitMaxNormal, SecPerSec, fmt.Sprintf(
			"its tasks usually wait %.2f s for every second they run (limit %.2f): it is contended nearly all the time, so its own normal is the contention "+
				"and CPU wait cannot show starvation against it. CPU wait is not used",
			c.Normal, th.CPUWaitMaxNormal)))
	case c.LongestRun >= th.CPUWaitStalledRun:
		// The measurement is sound and stays; it just cannot vouch for a
		// reduction. applySignalGaps turns a reduction into UNVERIFIED.
		f.cpuWaitStalledRun = true
		f.Reasons = append(f.Reasons, measured(CPUWaitStalledRun, float64(c.LongestRun), float64(th.CPUWaitStalledRun), Count, fmt.Sprintf(
			"CPU wait shows it stalled for %s in a row, too short a part of the window (%.1f%% of stretches, threshold %.0f%%) to call it starved, "+
				"too long to call it noise: a starvation at one time of day, a batch run or an incident looks like this. A reduction cannot be shown safe",
			stretchesDuration(c.LongestRun), c.Stalled*100, th.CPUWaitStalledShare*100)))
		return
	default:
		return
	}
	delete(f.Have, metrics.SigCPUWait)
}

// stretchesDuration renders a count of 5-minute stretches as time.
func stretchesDuration(n int) string {
	d := time.Duration(n) * 5 * time.Minute
	if d >= time.Hour {
		return fmt.Sprintf("%.0fh", d.Hours())
	}
	return fmt.Sprintf("%.0fm", d.Minutes())
}

// psiStalled reports CPU pressure above the threshold at its worst bucket.
func (f *Finding) psiStalled(th Thresholds) bool {
	return f.hasSignal(metrics.SigPSI) && f.PSIWaitingPeak > th.PSIWaiting
}

// throttleStalled reports CFS throttling above the threshold at its worst
// bucket.
func (f *Finding) throttleStalled(th Thresholds) bool {
	return f.hasSignal(metrics.SigThrottle) && f.ThrottlePeak > th.ThrottleRatio
}

// sizeCPURequest sizes the request from p95 usage plus margin, floored and
// capped by any runtime ceiling.
func sizeCPURequest(f *Finding, th Thresholds, cpuCap int64, hasCPUCap bool) int64 {
	rec := int64(f.CPUUsageP95 * 1000 * (1 + th.Margin))
	if rec < th.MinCPUMillicores {
		f.Reasons = append(f.Reasons, measured(CPUFloor, f.CPUUsageP95*1000, float64(th.MinCPUMillicores), Millicores, fmt.Sprintf(
			"CPU p95 of %.0fm sizes below the %dm floor, so the recommendation is the floor rather than a measurement: this container is too idle for its own usage to size it",
			f.CPUUsageP95*1000, th.MinCPUMillicores)))
		f.CPUFloored = true
		rec = th.MinCPUMillicores
	}
	if hasCPUCap && rec > cpuCap {
		rec = cpuCap
		f.Reasons = append(f.Reasons, measured(CPUCapped, float64(cpuCap), 0, Millicores, fmt.Sprintf("CPU recommendation capped at %dm by a runtime ceiling", cpuCap)))
	}
	return rec
}

// sizeCPULimit sizes the limit from peak usage, which is deliberately looser
// than the request: a request sized to a percentile must not become a limit
// that throttles every burst.
func sizeCPULimit(f *Finding, th Thresholds, cpuStarved bool, recCPU, cpuCap int64, hasCPUCap bool) int64 {
	steadyPeak := f.CPUUsageMax
	if f.hasSignal(metrics.SigStartup) && f.CPUSteadyMax > 0 {
		steadyPeak = f.CPUSteadyMax
	}
	limit := int64(steadyPeak * 1000 * (1 + th.BurstMargin))

	// Boot is a separate regime. Sizing the limit to steady state throttles
	// every restart, and a throttled boot can fail a readiness probe, which
	// turns an ordinary rollout into an outage.
	if f.hasSignal(metrics.SigStartup) && f.CPUStartupMax > 0 && steadyPeak > 0 {
		ratio := f.CPUStartupMax / steadyPeak
		if ratio >= th.StartupRatio {
			// A genuinely distinct boot regime: size for it and say so.
			if startupLimit := int64(f.CPUStartupMax * 1000 * (1 + th.StartupMargin)); startupLimit > limit {
				f.Reasons = append(f.Reasons, measured(CPUStartup, f.CPUStartupMax*1000, steadyPeak*1000, Millicores, fmt.Sprintf(
					"CPU limit is set by startup, not steady state: this container draws %.0fm while booting against %.0fm afterwards (%.1fx), so a steady-state limit would throttle every restart",
					f.CPUStartupMax*1000, steadyPeak*1000, ratio)))
				limit = startupLimit
			}
		} else if boot := int64(f.CPUStartupMax * 1000 * (1 + th.BurstMargin)); boot > limit {
			// Boot and steady state look the same. Cover the peak, quietly.
			limit = boot
		}
	}

	limit = max(limit, recCPU)
	// Usage is a rate averaged over seconds; CFS spends the quota every 100ms.
	// A container that bursts past its quota inside each period is throttled
	// while its averaged peak sits far below the limit, so sizing the limit
	// from that peak tightens the very thing already holding it back. On the
	// lab cluster, guaranteed-cache was throttled in 82% of periods against a
	// 100m limit and was told to cut it to 30m. A starved container's limit is
	// ratcheted in holdStarved; everything else holds it here.
	if !cpuStarved && f.CPULimit > limit && f.throttleStalled(th) {
		f.Reasons = append(f.Reasons, measured(CPULimitThrottled, f.ThrottlePeak, th.ThrottleRatio, Ratio, fmt.Sprintf(
			"the %dm CPU limit throttled this container in %.1f%% of scheduling periods at its worst (window mean %.1f%%). "+
				"Usage is averaged over seconds and CFS enforces the quota every 100ms, so a limit sized from averaged usage (%dm) "+
				"would throttle it harder: the limit is held, and only the request is sized from usage.",
			f.CPULimit, f.ThrottlePeak*100, f.Throttle*100, limit)))
		limit = f.CPULimit
	}
	// Nothing above measures whether a lower limit is enough. CFS spends the
	// quota every 100ms and the finest CPU signal here is a rate averaged over
	// a minute, so a container that needs 50m for a few milliseconds per
	// request reads as using 5m, and a limit sized from that stretches every
	// such burst across several periods. On the lab cluster Online Boutique ran at
	// baseline error rate under tare's limits, restarted nothing, and served
	// its product page at 4x the p95 latency, 18x at twice the load. Lowering
	// a limit saves nothing the scheduler sees - it reserves the request - so
	// a CPU limit is only ever held or raised, and none is added.
	if !cpuStarved && limit > 0 && (f.CPULimit == 0 || limit < f.CPULimit) {
		if f.CPULimit > 0 {
			f.Reasons = append(f.Reasons, measured(CPULimitHeld, float64(f.CPULimit), float64(limit), Millicores, fmt.Sprintf(
				"the %dm CPU limit is held. Usage sizes it at %dm, but CFS enforces a limit every 100ms and usage is a rate averaged over a minute, "+
					"so nothing measured shows the lower limit covers this container's bursts; one that does not slows every request without an error or a restart. "+
					"The scheduler reserves the request, not the limit, so only the request is sized from usage.",
				f.CPULimit, limit)))
		}
		limit = f.CPULimit
	}
	if hasCPUCap && limit > cpuCap {
		limit = cpuCap
	}
	return limit
}

// sizeMemRequest sizes memory from the peak working set plus margin, floored,
// and raised to any runtime ceiling's floor.
func sizeMemRequest(f *Finding, th Thresholds, floor int64) int64 {
	rec := int64(f.MemUsageMax * (1 + th.Margin))
	if rec < th.MinMemBytes {
		f.Reasons = append(f.Reasons, measured(MemFloor, float64(rec), float64(th.MinMemBytes), Bytes, fmt.Sprintf(
			"memory peak of %s sizes below the %s floor, so the recommendation is the floor rather than a measurement",
			HumanBytes(rec), HumanBytes(th.MinMemBytes))))
		f.MemFloored = true
		rec = th.MinMemBytes
	}
	if floor > 0 && rec < floor {
		f.Reasons = append(f.Reasons, measured(MemRaised, float64(rec), float64(floor), Bytes, fmt.Sprintf(
			"memory recommendation raised from %s to %s by a runtime ceiling; observed usage alone would have been unsafe",
			HumanBytes(rec), HumanBytes(floor))))
		rec = floor
	}
	return rec
}

// memLimitFor is the memory limit for a request. Memory has no graceful
// degradation, so the limit sits at the request plus a headroom fraction rather
// than at a peak: the gap only buys time before an OOMKill.
func memLimitFor(request int64, th Thresholds) int64 {
	return int64(float64(request) * (1 + th.MemLimitHeadroom))
}

// ratchetCPU never lets the CPU recommendation fall below what is allocated
// now. With no current limit the ratchet has nothing to hold the limit at, and
// a usage-sized limit can land below the ratcheted request, which the API
// server rejects; the limit is therefore held at least at the request.
func ratchetCPU(f *Finding) {
	f.RecCPU = max(f.RecCPU, f.CPURequest)
	f.RecCPULimit = max(f.RecCPULimit, f.CPULimit, f.RecCPU)
}

// ratchetMem never lets the memory recommendation fall below what is
// allocated now.
func ratchetMem(f *Finding, th Thresholds) {
	f.RecMem = max(f.RecMem, f.MemRequest)
	f.RecMemLimit = max(memLimitFor(f.RecMem, th), f.MemLimit)
}

// holdStarved gives a starved container only ever an increase, and only in the
// resource it is actually starved of. Ratcheting both would answer a question
// the evidence never asked: a CPU-throttled container whose memory is idle
// would be told to keep a request it has never come close to using, which
// reads as a measurement and is really the input echoed back.
func holdStarved(f *Finding, th Thresholds, st starved) {
	measuredMem := f.RecMem
	if st.cpu {
		ratchetCPU(f)
	}
	if st.mem {
		ratchetMem(f, th)
		// Nothing measured asked for more, so the ratchet decided this, and
		// the ratchet's 25% limit headroom is not evidence of anything: it
		// was shipping as a limit bump, and for a Guaranteed pod the QoS
		// hold then pulled the request up to it. Hold both and say why.
		// st.mem is only ever set by an OOMKill.
		if measuredMem <= f.MemRequest {
			censorMemory(f)
		}
	}
	// Only for a CPU-starved container: the advice is to raise the limit, and
	// for one starved of memory alone the recommendation raises nothing on
	// CPU, so the note contradicted the numbers printed beside it.
	if st.cpu && f.CPULimit > 0 && f.CPUUsageP95*1000 >= float64(f.CPULimit)*pinnedAtLimit {
		f.Reasons = append(f.Reasons, measured(CPUAtLimit, f.CPUUsageP95*1000, float64(f.CPULimit), Millicores, fmt.Sprintf("usage is pinned at the %dm CPU limit; raise the limit, not just the request", f.CPULimit)))
	}
}

// censorMemory withholds a memory number from an OOMKilled container whose
// observed peak never asked for more than it already has.
func censorMemory(f *Finding) {
	f.MemCensored = true
	f.RecMem, f.RecMemLimit = f.MemRequest, f.MemLimit
	killedAt := "the point it was killed at"
	if f.MemLimit > 0 {
		killedAt = "the " + HumanBytes(f.MemLimit) + " limit that killed it"
	}
	f.Reasons = append(f.Reasons, measured(MemDemandCensored, f.MemUsageMax, float64(f.MemLimit), Bytes, fmt.Sprintf(
		"OOMKilled, and usage above %s is never observed: the %s peak is a lower bound on "+
			"what it needs, not a measurement of it. tare does not know how much memory this "+
			"container needs, so it proposes no memory change; raise it by a step you choose and "+
			"re-run once the new size has a window of its own.",
		killedAt, HumanBytes(int64(f.MemUsageMax)))))
}

// holdRestartChurn refuses to reduce a container that keeps restarting. It has
// not run long enough at any one size for its own usage to size it, and the
// samples that do exist describe a container that kept dying. Reducing it is
// the dangerous direction, because the allocation is a candidate for what
// killed it.
//
// The absence of an OOM event is not evidence here. tare-lab's slow-leaker
// filled a pod-scoped tmpfs, so every restart was OOMKilled inside runc init,
// before the container existed to have an OOM event recorded against it: 34
// restarts, OOMKilled false, and a confident WASTE verdict with a 1.17GiB
// recommendation for a container that had not run in two hours.
//
// held names the resources a starvation hold has already decided; those keep
// their numbers and their verdict, and churn holds the rest.
func holdRestartChurn(f *Finding, th Thresholds, held starved) {
	if !held.any() {
		f.Verdict = Unverified
	}
	f.Reasons = append(f.Reasons, measured(RestartChurn, f.Restarts, float64(th.RestartChurn), Count, fmt.Sprintf(
		"container restarted %.0f times in the window, so its usage describes a container that kept dying rather than one running at this size; no reduction is proposed. A kill during container init is never recorded as an OOM event, so the absence of one is not evidence that this allocation is adequate.",
		f.Restarts)))
	if !held.cpu {
		ratchetCPU(f)
	}
	if !held.mem {
		ratchetMem(f, th)
	}
}

// markIdle records no measurable work for the whole window. This is the only
// verdict whose action is deletion rather than a number, so it is guarded
// harder than any other (see isIdle) and says plainly what it cannot rule out.
//
// The sizing numbers stay: if the workload turns out to be needed, the smaller
// allocation is still the right one. Deletion is the larger saving, not the
// only one.
func markIdle(f *Finding, why Reason) {
	f.Verdict = Idle
	f.Reasons = append(f.Reasons, why)
	f.Reasons = append(f.Reasons, note(IdleCaveat, "what this does NOT rule out: a hot standby, a disaster-recovery replica, "+
		"an operator or controller that only acts on a rare event, a job whose period is "+
		"longer than the window, or a service kept warm on purpose. tare sees CPU and memory, "+
		"not intent. Confirm with the owner before deleting anything."))
}

// classifyWaste calls a container wasteful when either request sits far above
// what it uses, UNSET when it sets neither requests nor limits, and healthy
// otherwise.
func classifyWaste(f *Finding, th Thresholds) {
	if f.CPURequest == 0 && f.MemRequest == 0 && f.CPULimit == 0 && f.MemLimit == 0 {
		f.Verdict = Unset
		return
	}
	wastedCPU := f.CPURequest > 0 && f.CPUUsageP95*1000 < float64(f.CPURequest)*th.WasteRatio
	wastedMem := f.MemRequest > 0 && f.MemUsageMax < float64(f.MemRequest)*th.WasteRatio
	if !wastedCPU && !wastedMem {
		f.Verdict = Healthy
		noteLoweredLimits(f)
		return
	}
	f.Verdict = Waste
	if wastedCPU {
		f.Reasons = append(f.Reasons, measured(CPUUnderused, f.CPUUsageP95*1000, float64(f.CPURequest), Millicores, fmt.Sprintf("CPU p95 is %.0fm against a %dm request", f.CPUUsageP95*1000, f.CPURequest)))
	}
	if wastedMem {
		f.Reasons = append(f.Reasons, measured(MemUnderused, f.MemUsageMax, float64(f.MemRequest), Bytes, fmt.Sprintf("memory peak is %s against a %s request", HumanBytes(int64(f.MemUsageMax)), HumanBytes(f.MemRequest))))
	}
	noteLoweredLimits(f)
}

// noteLoweredLimits explains why an existing limit is recommended lower.
// Requests get CPU_UNDERUSED and MEM_UNDERUSED; without a corresponding reason
// for limits, a row whose memory limit drops by 90% or whose IDLE container
// has its limits lowered carries no reason for the limit change at all.
func noteLoweredLimits(f *Finding) {
	if f.CPULimit > 0 && f.RecCPULimit > 0 && f.RecCPULimit < f.CPULimit {
		peak := max(f.CPUUsageMax, f.CPUSteadyMax, f.CPUStartupMax) * 1000
		f.Reasons = append(f.Reasons, measured(CPULimitUnderused, peak, float64(f.CPULimit), Millicores,
			fmt.Sprintf("CPU peak is %.0fm against a %dm limit", peak, f.CPULimit)))
	}
	if f.MemLimit > 0 && f.RecMemLimit > 0 && f.RecMemLimit < f.MemLimit {
		f.Reasons = append(f.Reasons, measured(MemLimitUnderused, f.MemUsageMax, float64(f.MemLimit), Bytes,
			fmt.Sprintf("memory peak is %s against a %s limit", HumanBytes(int64(f.MemUsageMax)), HumanBytes(f.MemLimit))))
	}
}

// reconcileFloors clears a floor flag whose recommendation no longer sits at
// the floor. The flags exist so a reader can tell a constant from a
// measurement; a stale one mislabels a real number as constant. It runs once,
// after every step that moves the numbers: a ratchet, an inferred ceiling or
// the Guaranteed hold can raise a floored value, and a workload another
// controller owns has its numbers zeroed. Run inside decide, it missed the
// last two.
func (f *Finding) reconcileFloors(th Thresholds) {
	if f.CPUFloored && f.RecCPU != th.MinCPUMillicores {
		f.CPUFloored = false
	}
	if f.MemFloored && f.RecMem != th.MinMemBytes {
		f.MemFloored = false
	}
}

// isIdle reports why a container counts as doing no work, or nil if it does
// not. It is deliberately a single function returning the reason it justifies:
// the guards and the explanation must not be able to drift apart, because the
// explanation is what a human uses to decide whether to delete a workload.
//
// The guards, and what each one is for: the PEAK must be below the threshold,
// so a job that runs once a day is not called idle; memory must be flat, so a
// process quietly accumulating something is not called idle; and at least half
// the window must actually have been observed, so a container deployed this
// morning is not called idle for the fortnight before it existed.
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
	if !f.hasSignal(metrics.SigMemTrend) || f.MemEarly <= 0 {
		return nil
	}
	if f.MemLate > f.MemEarly*th.IdleMemGrowth {
		return nil
	}
	if expected := expectedSamples(window); expected > 0 &&
		float64(f.Samples) < float64(expected)*th.IdleMinCoverage {
		return nil
	}
	r := measured(IdleWindow, peak, th.IdleCPUMillicores, Millicores, fmt.Sprintf(
		"no measurable work in the window: CPU peaked at %.1fm (below the %.0fm idle threshold) and memory stayed flat at %s. "+
			"Resizing this saves %dm of CPU; deleting it saves that on every replica and the operational surface as well.",
		peak, th.IdleCPUMillicores, HumanBytes(int64(f.MemUsageMax)), f.CPURequest))
	return &r
}

// scrapeSeconds is the scrape interval every sample count here assumes.
const scrapeSeconds = 15

// expectedSamples is what a 15 second scrape interval produces over window.
func expectedSamples(window time.Duration) int {
	return int(window.Seconds() / scrapeSeconds)
}

// confidence rates the evidence by sample count against expectedSamples.
func confidence(samples int, window time.Duration) Confidence {
	if samples == 0 {
		return Low
	}
	expected := expectedSamples(window)
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
