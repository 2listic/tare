package analyze

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
	// CPUWaitStalledShare: a container is starved by tare-agent's CPU wait when
	// at least this share of its 5-minute stretches is stalled under the
	// relative slowdown rule (metrics/cpuwait.go). It plays the role
	// PSIWaiting plays for PSI, on a different scale.
	CPUWaitStalledShare float64
	// CPUWaitMinCoverage: below this share of the container's CPU time
	// accounted, the agent is not seeing the container's tasks, and no wait
	// is not evidence of no starvation.
	CPUWaitMinCoverage float64
	// CPUWaitMinStretches is the history the rule needs before it is believed:
	// the normal a stretch is compared with must include quiet periods.
	CPUWaitMinStretches int
	// CPUWaitStalledRun: this many stalled stretches in a row, with the share
	// under CPUWaitStalledShare, is a starvation the share diluted. A run its
	// node confirmed has a known cause and makes SATURATED. Any other run
	// cannot be told apart from a load peak the container's own normal does
	// not cover, so it only holds a reduction (UNVERIFIED).
	CPUWaitStalledRun int
	// CPUWaitMaxNormal: a container whose normal slowdown is at least this is
	// contended all the time, so it has no quiet normal to be compared with
	// and only the ceiling, just above it, could flag it. Its CPU wait is not
	// used unless it already shows starvation.
	CPUWaitMaxNormal float64
	// CPUWaitMinNodeKnown: the share of the window the container's node state
	// must be known for before the node, not CPUWaitMaxNormal, decides a
	// container with no quiet normal. Its node contended means its stretches
	// are confirmed; its node not contended means the high normal is the
	// container's own way of running, and is its normal.
	CPUWaitMinNodeKnown float64
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
		// 0.05, as agent/compare-pressure's --share, which is what the rule was
		// evaluated with on the harness clusters. Unswept in this catalog.
		CPUWaitStalledShare: 0.05,
		// 0.5. Measured on the harness: the agent's failures to see a container
		// sat at 0.01-0.09, healthy fork-heavy containers at 0.66-0.88.
		CPUWaitMinCoverage: 0.5,
		// A day of 5-minute stretches, so the normal has seen a night.
		CPUWaitMinStretches: 288,
		// 4 stretches, 20 minutes: the shortest starvation a reduction should
		// not walk past. On the harness the 35-minute batch episodes ran to 7
		// in a row. Unvalidated over week-long windows: the harness runs an
		// hour, and high-load phases there also made short runs, so expect
		// some UNVERIFIED on containers that were only busy.
		CPUWaitStalledRun: 4,
		// 0.5: half a second waited per second run as the usual state. The
		// harness's always-contended k6 sat at 0.95, its quiet containers
		// below 0.15.
		CPUWaitMaxNormal: 0.5,
		// Half the window: less, and the stretches without node state are
		// judged by the relative rule alone, which is the case
		// CPUWaitMaxNormal guards.
		CPUWaitMinNodeKnown: 0.5,
	}
}
