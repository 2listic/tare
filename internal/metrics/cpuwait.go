package metrics

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// CPU wait is tare-agent's measure of CPU starvation, for nodes where PSI is
// missing: cgroup v1, RHEL-family kernels that ship PSI switched off, kubelets
// older than 1.33. It is the seconds a container's tasks spent ready to run but
// waiting for a CPU, summed over every task, including the ones that have
// already exited.
//
// It is not on PSI's scale. PSI is wall time during which some task waited;
// CPU wait is a sum over tasks, so a container with eight busy threads can wait
// eight seconds per second, and PSI's 0.03 means nothing here. What carries
// across containers is the slowdown, wait divided by usage: seconds waited per
// second run. Even that differs a lot between containers that are perfectly
// fine, so the rule compares each container with its own normal rather than
// with one cut for everybody.
//
// The relative slowdown rule, per 5-minute stretch:
//
//	stalled = (slowdown >= K x normal and wait >= IdleFloor) or slowdown >= Ceiling
//
// with normal the container's 10th percentile slowdown over the window. Against
// the contention actually created on the harness clusters it was right in 98.5%
// of stretches, against 91.6% for PSI at tare's threshold, cgroup v1 included.
// The constants are those of that evaluation and of agent/compare-pressure,
// which applies the same rule to any cluster; change them together.
//
// What the rule cannot do is size an increase. Usage plus wait is not demand,
// so the size of an increase stays with the usage-based logic, as for PSI.
//
// Node context. A task waits for a CPU only while every CPU it may use is
// busy, which is a fact about the node, and node-exporter reports it on every
// kernel without PSI: the run-queue wait of all the node's tasks
// (/proc/schedstat) and its idle time. Each stretch is judged with its node's
// state:
//
//	node contended (queue >= nodeContendedQueue and its busiest minute at
//	    least nodeFullBusy): stalled if the container waited at all (wait >=
//	    IdleFloor). The cause is established, so no normal is needed:
//	    always-contended containers and short incidents.
//	node idle in every minute (busy < nodeIdleBusy): not stalled. A request
//	    is a CPU weight and only matters while CPUs are contested; the waits of
//	    a container with idle CPUs next to it are its own wake-ups, which a
//	    larger request does not remove. Not for a container that may be pinned
//	    to some of the node's CPUs, whose CPUs can be full while the rest idle.
//	in between: the relative test, without the ceiling, which only stood in
//	    for the missing quiet normal that node confirmation now supplies.
//	node unknown: the relative slowdown rule as above.
//
// On the harness's seven one-hour runs (five distributions, cgroup v1
// included) it was right in 99.6% of stretches with no stalled stretch missed,
// against 98.5% for the relative rule alone and 91.6% for PSI.
const (
	cpuWaitStep = 5 * time.Minute

	// slowdownK: a stretch is stalled at twice the container's normal slowdown.
	slowdownK = 2.0
	// slowdownIdleFloor: below this much wait (s/s) the relative test never
	// fires. A nearly idle container doubling a tiny wait is still nearly idle.
	slowdownIdleFloor = 0.01
	// slowdownCeiling: a container that waits a second for every second it runs
	// is stalled whatever its normal. It is the backstop for containers that
	// are always contended and so have no quiet normal to compare with.
	slowdownCeiling = 1.0
	// slowdownMinUsage: below this many cores the slowdown is not defined.
	// Dividing by an idle container's usage turns noise into starvation.
	slowdownMinUsage = 0.005
	// slowdownNormalMinWait: stretches with less wait than this do not shape
	// the normal, or a container idle at night gets a normal of zero and every
	// busy stretch is "twice normal".
	slowdownNormalMinWait = 0.001
	// slowdownNormalQuantile picks the normal from the container's slowdowns.
	// Over a seven-day window, a container contended less than 90% of the time
	// still has a quiet normal.
	slowdownNormalQuantile = 0.10

	// nodeContendedQueue: a node whose tasks waited this many seconds per
	// second per CPU over the stretch was short of CPU. On the harness, quiet
	// worker nodes stayed at or below 0.28 (0.16 typically) and contended ones
	// read 0.33 to 2.7; two busy neighbours on 4 CPUs read 0.5 to 1.2.
	nodeContendedQueue = 0.3
	// nodeFullBusy: the queue alone is not contention. A task held back by its
	// own CPU limit (CFS throttling) counts as waiting in schedstat while the
	// CPUs idle: one busy loop throttled to 100m added 0.23 per CPU to a
	// 4-CPU node half idle. Contention needs the CPUs full, and a throttled
	// task does not fill them. On the harness the busiest minute of a quiet
	// stretch never passed 0.65 and that of a contended one never fell below
	// 0.97, on every kernel tried.
	nodeFullBusy = 0.9
	// nodeIdleBusy: a node below this busy share in every minute of the
	// stretch had CPUs going spare all the time. Every minute, not the
	// average: a 30-second burst raises the average little and is real
	// contention while it lasts. The harness's quiet nodes ran 25-50% busy at
	// their workloads' own load, contended ones 54-100%.
	nodeIdleBusy = 0.35

	// agentUnattributedMax is the share of a node's exit records the agent may
	// fail to place before its numbers for that node are not believed.
	agentUnattributedMax = 0.05
)

// CPUWait is what tare-agent says about one container over the window.
type CPUWait struct {
	// Stretches is how many 5-minute stretches had both CPU wait and usage.
	Stretches int
	// Stalled is the share of Stretches the relative slowdown rule calls
	// stalled.
	Stalled float64
	// LongestRun is the most stalled stretches in a row. A share dilutes a
	// starvation that is serious but short against the window: a batch job
	// starved for its whole hour every night is 4% of a week, under the share.
	// A run of them does not dilute.
	LongestRun int
	// LongestConfirmedRun is the most stalled stretches in a row that its
	// node's contention confirmed. Unlike a run the relative test alone found,
	// its cause is known, so it is starvation however short against the window.
	LongestConfirmedRun int
	// NodeKnown is the share of Stretches whose node state was known, and
	// NodeContended the share whose node was contended. A container with no
	// quiet normal is contended all the time only if its node was.
	NodeKnown, NodeContended float64
	// Normal is the container's normal slowdown. HasNormal is false when it
	// never waited enough to have one; only the ceiling applies then.
	Normal    float64
	HasNormal bool
	// Mean is the mean CPU wait over the window, s/s.
	Mean float64
	// Coverage is the median share of the container's CPU time the agent
	// accounted for. Well below one the agent is not seeing the container's
	// tasks, and its silence is not evidence of anything. Busy is false when
	// the container never ran enough for coverage to be measured.
	Coverage float64
	Busy     bool
}

// stalled is the relative slowdown rule for one stretch.
func stalled(wait, slowdown, normal float64, hasNormal bool) bool {
	if slowdown >= slowdownCeiling {
		return true
	}
	return hasNormal && slowdown >= slowdownK*normal && wait >= slowdownIdleFloor
}

// NodeState is what the container's node did over one stretch. Known is false
// when node-exporter had nothing for it, or the container's node is not known.
type NodeState struct {
	Known bool
	// Queue is the run-queue wait of all the node's tasks per CPU, s/s.
	Queue float64
	// BusyMax is the busiest minute's share of the node's CPU time not idle.
	BusyMax float64
	// EveryMinute: node-exporter answered in every minute of the stretch.
	// A node too busy to be scraped leaves gaps exactly in its busiest
	// minutes, and the minutes left look idle.
	EveryMinute bool
	// Pinned: the container may run on only some of the node's CPUs (an
	// integer CPU allocation the kubelet's CPU manager may pin), so the node's
	// idle CPUs prove nothing about it.
	Pinned bool
}

func (n NodeState) contended() bool {
	return n.Known && n.Queue >= nodeContendedQueue && n.BusyMax >= nodeFullBusy
}
func (n NodeState) idle() bool {
	return n.Known && n.EveryMinute && !n.Pinned && n.BusyMax < nodeIdleBusy
}

// judge decides one stretch. confirmed is set when the node's contention made
// it stalled.
func judge(wait, slowdown, normal float64, hasNormal bool, node NodeState) (isStalled, confirmed bool) {
	switch {
	case !node.Known:
		return stalled(wait, slowdown, normal, hasNormal), false
	case node.contended():
		return wait >= slowdownIdleFloor, wait >= slowdownIdleFloor
	case node.idle():
		return false, false
	}
	return hasNormal && slowdown >= slowdownK*normal && wait >= slowdownIdleFloor, false
}

// WaitStretch is one 5-minute stretch: CPU wait (s/s), the CPU time the agent
// accounted (cores), the kubelet's CPU usage (cores), and the node's state.
type WaitStretch struct {
	Wait, Run, Usage float64
	Node             NodeState
}

// SummariseCPUWait applies the rule to stretches already in hand. It is the
// reference the queries in observeCPUWait are tested against, and what the
// simulator uses, so a threshold sweep exercises the rule a report applies.
func SummariseCPUWait(st []WaitStretch) CPUWait {
	var normal, cover []float64
	for _, s := range st {
		if s.Usage <= slowdownMinUsage {
			continue
		}
		if s.Wait >= slowdownNormalMinWait {
			normal = append(normal, s.Wait/s.Usage)
		}
		cover = append(cover, s.Run/s.Usage)
	}
	c := CPUWait{Stretches: len(st)}
	if len(normal) > 0 {
		c.Normal, c.HasNormal = interpolatedQuantile(normal, slowdownNormalQuantile), true
	}
	if len(cover) > 0 {
		c.Coverage, c.Busy = interpolatedQuantile(cover, 0.5), true
	}
	var n, known, contended int
	var runs runCounter
	for _, s := range st {
		c.Mean += s.Wait
		if s.Node.Known {
			known++
		}
		if s.Node.contended() {
			contended++
		}
		var isStalled, confirmed bool
		if s.Usage > slowdownMinUsage {
			isStalled, confirmed = judge(s.Wait, s.Wait/s.Usage, c.Normal, c.HasNormal, s.Node)
		}
		if isStalled {
			n++
		}
		runs.add(isStalled, confirmed, true)
	}
	c.LongestRun, c.LongestConfirmedRun = runs.longest, runs.longestConfirmed
	if len(st) > 0 {
		c.Mean /= float64(len(st))
		c.Stalled = float64(n) / float64(len(st))
		c.NodeKnown = float64(known) / float64(len(st))
		c.NodeContended = float64(contended) / float64(len(st))
	}
	return c
}

// runCounter tracks the longest runs of stalled stretches, all of them and
// those the node confirmed.
type runCounter struct {
	run, confirmed, longest, longestConfirmed int
}

// add records the next stretch. adjacent is false when a stretch is missing
// before this one, which breaks both runs.
func (r *runCounter) add(isStalled, confirmed, adjacent bool) {
	if !adjacent {
		r.run, r.confirmed = 0, 0
	}
	if !isStalled {
		r.run, r.confirmed = 0, 0
		return
	}
	r.run++
	r.longest = max(r.longest, r.run)
	if confirmed {
		r.confirmed++
		r.longestConfirmed = max(r.longestConfirmed, r.confirmed)
	} else {
		r.confirmed = 0
	}
}

// interpolatedQuantile is PromQL's quantile_over_time: linear interpolation
// between the two nearest ranks.
func interpolatedQuantile(xs []float64, q float64) float64 {
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	pos := q * float64(len(s)-1)
	lo := int(pos)
	if lo+1 >= len(s) {
		return s[lo]
	}
	return s[lo] + (pos-float64(lo))*(s[lo+1]-s[lo])
}

// cpuWaitExprs builds the per-container 5-minute series the rule is made of.
//
// The agent has no Kubernetes API access and labels its series with the
// container's cgroup path only (`id`). The kubelet's cAdvisor series carry the
// same `id` next to namespace, pod and container, and that is the join. The
// agent's series are first summed by `id` alone: scraped by a PodMonitor they
// also carry the agent pod's own namespace, pod and container labels, which
// would collide with the ones the join adds. For the same reason the report's
// namespaces filter only the kubelet side.
//
// Incarnations: a restarted container has a new `id`, and in any one stretch
// only one of them runs, so summing by container gives the running one.
func cpuWaitExprs(sel, agentSel string) (wait, run, usage string) {
	names := fmt.Sprintf(`max by (id,namespace,pod,container) (container_cpu_usage_seconds_total{%s} * 0 + 1)`, sel)
	agent := func(metric string) string {
		return fmt.Sprintf(`sum by (namespace,pod,container) (sum by (id) (rate(%s{%s}[5m])) * on (id) group_left (namespace,pod,container) %s)`,
			metric, agentSel, names)
	}
	usage = fmt.Sprintf(`sum by (namespace,pod,container) (max by (id,namespace,pod,container) (rate(container_cpu_usage_seconds_total{%s}[5m])))`, sel)
	return agent("tare_cpu_wait_seconds_total"), agent("tare_cpu_run_seconds_total"), usage
}

// cpuWaitQuery is one instant query of the CPU wait set.
type cpuWaitQuery struct {
	expr   string
	assign func(c *CPUWait, v float64)
}

// cpuWaitQueries are the instant half of the rule: everything that is one
// number per container over the window. Each is wrapped in the by-container
// aggregate the Observe set uses, so every container query reads alike.
func cpuWaitQueries(sel, agentSel string, window time.Duration) []cpuWaitQuery {
	w, step := durationString(window), durationString(cpuWaitStep)
	wait, run, usage := cpuWaitExprs(sel, agentSel)
	busy := fmt.Sprintf(`(%s > %g)`, usage, slowdownMinUsage)
	slowdown := fmt.Sprintf(`(%s / %s)`, wait, busy)
	qs := []cpuWaitQuery{
		{
			expr:   fmt.Sprintf(`count_over_time((%s and %s)[%s:%s])`, wait, usage, w, step),
			assign: func(c *CPUWait, v float64) { c.Stretches = int(v) },
		},
		{
			expr:   fmt.Sprintf(`avg_over_time(%s[%s:%s])`, wait, w, step),
			assign: func(c *CPUWait, v float64) { c.Mean = v },
		},
		{
			expr: fmt.Sprintf(`quantile_over_time(%g, (%s and (%s >= %g))[%s:%s])`,
				slowdownNormalQuantile, slowdown, wait, slowdownNormalMinWait, w, step),
			assign: func(c *CPUWait, v float64) { c.Normal, c.HasNormal = v, true },
		},
		{
			expr:   fmt.Sprintf(`quantile_over_time(0.5, (%s / %s)[%s:%s])`, run, busy, w, step),
			assign: func(c *CPUWait, v float64) { c.Coverage, c.Busy = v, true },
		},
	}
	for i := range qs {
		qs[i].expr = fmt.Sprintf(`max by (namespace,pod,container) (%s)`, qs[i].expr)
	}
	return qs
}

// stalledCandidatesExpr is the range half of the rule. Whether a stretch is
// stalled depends on the container's own normal, a different number for every
// container, and PromQL cannot compare a subquery with that without the @
// modifier, which not every backend has. So the stretches that could be
// stalled are fetched and compared with the normal here.
//
// Only candidates come back: stretches at or above the idle floor, or at the
// ceiling. Every other stretch is not stalled whatever the normal, and on a
// healthy container that is nearly all of them, which keeps a seven-day range
// query small. The value is the slowdown.
func stalledCandidatesExpr(sel, agentSel string) string {
	wait, _, usage := cpuWaitExprs(sel, agentSel)
	slowdown := fmt.Sprintf(`(%s / (%s > %g))`, wait, usage, slowdownMinUsage)
	return fmt.Sprintf(`max by (namespace,pod,container) ((%s and (%s >= %g)) or (%s >= %g))`,
		slowdown, wait, slowdownIdleFloor, slowdown, slowdownCeiling)
}

// AgentNode is one tare-agent target and the self-checks it failed over the
// window. Instance is Prometheus' `instance`: the agent runs on the host
// network, so it is the node's address.
type AgentNode struct {
	Instance string
	Problems []string
}

// agentSelfChecks finds the agents whose numbers are not believed over the
// window. Every portability problem found while building the agent showed up
// in these counters and nowhere else: a kernel whose record layout the agent
// does not know raises no error, it produces less wait, and less wait reads as
// "not starved".
func (p *Prom) agentSelfChecks(window time.Duration) ([]AgentNode, error) {
	w := durationString(window)
	checks := []struct{ expr, problem string }{
		{fmt.Sprintf(`sum by (instance) (increase(tare_agent_exits_layout_unknown_total[%s])) > 0`, w),
			"exit records in a kernel layout it does not know"},
		{fmt.Sprintf(`sum by (instance) (increase(tare_agent_netlink_drops_total[%s])) > 0`, w),
			"exit records dropped by the kernel"},
		{fmt.Sprintf(`sum by (instance) (increase(tare_agent_exits_unattributed_total[%s])) > %g * sum by (instance) (increase(tare_agent_exits_total[%s]))`,
			w, agentUnattributedMax, w),
			fmt.Sprintf("more than %.0f%% of exit records placed in no container", agentUnattributedMax*100)},
	}
	byInstance := map[string]*AgentNode{}
	for _, c := range checks {
		samples, err := p.query(c.expr)
		if err != nil {
			return nil, err
		}
		for _, s := range samples {
			in := s.labels["instance"]
			if in == "" {
				continue
			}
			if byInstance[in] == nil {
				byInstance[in] = &AgentNode{Instance: in}
			}
			byInstance[in].Problems = append(byInstance[in].Problems, c.problem)
		}
	}
	out := make([]AgentNode, 0, len(byInstance))
	for _, n := range byInstance {
		out = append(out, *n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Instance < out[j].Instance })
	return out, nil
}

// excludeInstances is the agent selector that leaves the given targets out.
func excludeInstances(nodes []AgentNode) string {
	if len(nodes) == 0 {
		return ""
	}
	quoted := make([]string, len(nodes))
	for i, n := range nodes {
		quoted[i] = regexp.QuoteMeta(n.Instance)
	}
	return fmt.Sprintf(`instance!~%q`, strings.Join(quoted, "|"))
}

// agentPresentExpr asks whether any agent is scraped at all.
const agentPresentExpr = `count(tare_cpu_wait_seconds_total)`

// observeCPUWait adds tare-agent's CPU wait to the observations. It runs after
// the PSI queries because whether the agent's absence is worth reporting
// depends on them: where PSI works nobody installs the agent, and a report
// header saying it is missing would be noise.
func (p *Prom) observeCPUWait(out map[Key]*Observation, sel string, window time.Duration) {
	present, err := p.query(agentPresentExpr)
	if err != nil || len(present) == 0 || present[0].value == 0 {
		if err != nil || !p.pressureOK() {
			p.health = append(p.health, SignalHealth{Signal: SigCPUWait, Query: agentPresentExpr, Err: err})
		}
		return
	}

	bad, err := p.agentSelfChecks(window)
	if err != nil {
		p.health = append(p.health, SignalHealth{Signal: SigCPUWait, Query: "tare-agent self-checks", Err: err})
		return
	}
	agentSel := excludeInstances(bad)
	if len(bad) > 0 {
		parts := make([]string, len(bad))
		for i, n := range bad {
			parts[i] = n.Instance + " (" + strings.Join(n.Problems, ", ") + ")"
		}
		p.health = append(p.health, SignalHealth{
			Signal: SigCPUWait, Query: "tare-agent self-checks", Series: len(bad),
			Suspect: "tare-agent failed its self-checks on " + strings.Join(parts, "; ") +
				". Its CPU wait from those nodes is not used, so containers there are judged as if it were absent",
		})
	}

	got := map[Key]*CPUWait{}
	for _, q := range cpuWaitQueries(sel, agentSel, window) {
		samples, err := p.query(q.expr)
		p.health = append(p.health, SignalHealth{Signal: SigCPUWait, Query: q.expr, Err: err, Series: len(samples)})
		if err != nil {
			return
		}
		for _, s := range samples {
			k := keyOf(s.labels)
			if out[k] == nil {
				// No kubelet series of its own in this window: nothing else in
				// the report knows the container, so neither does this.
				continue
			}
			if got[k] == nil {
				got[k] = &CPUWait{}
			}
			q.assign(got[k], s.value)
		}
	}

	end := time.Now().Truncate(cpuWaitStep)
	start := end.Add(-window)
	expr := stalledCandidatesExpr(sel, agentSel)
	candidates, err := p.queryRangeByKey(expr, start, end, cpuWaitStep)
	// A healthy cluster has no candidates at all, which is an answer, not an
	// absent signal: the series that counts here is the stretch count above.
	p.health = append(p.health, SignalHealth{Signal: SigCPUWait, Query: expr, Err: err, Series: len(got)})
	if err != nil {
		return
	}
	nodes := p.nodeStates(sel, agentSel, start, end)
	for k, c := range got {
		if c.Stretches == 0 {
			continue
		}
		at := nodes.of(k, p.placement[k])
		var n int
		var runs runCounter
		var last int64
		for _, s := range candidates[k] {
			// Only candidates come back, so a missing step between two of
			// them is a stretch that was not stalled.
			adjacent := last != 0 && s.t-last == int64(cpuWaitStep/time.Second)
			last = s.t
			// Every candidate below the ceiling waited at least the idle floor.
			// The ones at the ceiling are checked where the node is known: the
			// node test is about waiting at all.
			wait := slowdownIdleFloor
			if nodes.waited != nil && !nodes.waited[k][s.t] {
				wait = 0
			}
			isStalled, confirmed := judge(wait, s.v, c.Normal, c.HasNormal, at(s.t))
			if isStalled {
				n++
			}
			runs.add(isStalled, confirmed, adjacent)
		}
		c.LongestRun, c.LongestConfirmedRun = runs.longest, runs.longestConfirmed
		c.Stalled = min(float64(n)/float64(c.Stretches), 1)
		c.NodeKnown, c.NodeContended = nodes.shares(p.placement[k], c.Stretches)
		out[k].CPUWait = *c
		out[k].Mark(SigCPUWait)
	}
}

// Placement is where a container runs, for the node half of the CPU wait
// rule: its node's address, as node-exporter's `instance` carries it, and
// whether it may be pinned to some of that node's CPUs.
type Placement struct {
	NodeIP string
	Pinned bool
}

// SetPlacement gives the CPU wait rule each container's node. A container it
// does not list, or a Prom never given one, has no node context, and the
// relative rule decides alone.
func (p *Prom) SetPlacement(m map[Key]Placement) { p.placement = m }

// The node's state per stretch, from node-exporter: run-queue wait per CPU over
// the stretch, and its busiest minute. Rates over 2 minutes, so a 60-second
// scrape interval still gives a value every minute.
const (
	nodeQueueExpr = `sum by (instance) (rate(node_schedstat_waiting_seconds_total[5m])) / count by (instance) (node_cpu_seconds_total{mode="idle"})`
	nodeBusyExpr  = `max_over_time(` + nodeBusyMinute + `[5m:1m])`
	// nodeMinutesExpr counts the minutes of the stretch that have a busy share.
	nodeMinutesExpr = `count_over_time(` + nodeBusyMinute + `[5m:1m])`
	nodeBusyMinute  = `(1 - sum by (instance) (rate(node_cpu_seconds_total{mode="idle"}[2m])) / count by (instance) (node_cpu_seconds_total{mode="idle"}))`
	// stretchMinutes is how many minutes a stretch has.
	stretchMinutes = 5
)

// nodeContext is the node half of the rule over the window.
type nodeContext struct {
	queue, busy, minutes map[string]map[int64]float64 // node IP -> stretch end -> value
	waited               map[Key]map[int64]bool       // stretches a container waited at least the idle floor
}

// nodeStates reads the node half of the rule. Without placements there is
// nothing to join it to, and it is not asked for. A failed or empty query is
// no node context, never an idle node.
func (p *Prom) nodeStates(sel, agentSel string, start, end time.Time) nodeContext {
	nc := nodeContext{}
	if len(p.placement) == 0 {
		return nc
	}
	byIP := func(expr string) map[string]map[int64]float64 {
		series, err := p.queryRangeSeries(expr, start, end, cpuWaitStep)
		if err != nil {
			return nil
		}
		out := map[string]map[int64]float64{}
		for _, s := range series {
			ip, ok := ipOf(s.labels["instance"])
			if !ok {
				continue
			}
			if out[ip] == nil {
				out[ip] = map[int64]float64{}
			}
			for _, pt := range s.points {
				out[ip][pt.t] = pt.v
			}
		}
		return out
	}
	nc.queue, nc.busy, nc.minutes = byIP(nodeQueueExpr), byIP(nodeBusyExpr), byIP(nodeMinutesExpr)
	wait, _, _ := cpuWaitExprs(sel, agentSel)
	waited, err := p.queryRangeByKey(fmt.Sprintf(`max by (namespace,pod,container) (%s >= %g)`, wait, slowdownIdleFloor),
		start, end, cpuWaitStep)
	if err == nil {
		nc.waited = map[Key]map[int64]bool{}
		for k, pts := range waited {
			nc.waited[k] = map[int64]bool{}
			for _, pt := range pts {
				nc.waited[k][pt.t] = true
			}
		}
	}
	return nc
}

// of returns the state of a container's node at each stretch end.
func (nc nodeContext) of(k Key, pl Placement) func(t int64) NodeState {
	return func(t int64) NodeState {
		q, ok1 := nc.queue[pl.NodeIP][t]
		b, ok2 := nc.busy[pl.NodeIP][t]
		// A placement without wait data cannot tell a stretch that waited
		// from one that did not, which the node test depends on.
		known := pl.NodeIP != "" && ok1 && ok2 && nc.waited != nil
		every := nc.minutes[pl.NodeIP][t] >= stretchMinutes
		return NodeState{Known: known, Queue: q, BusyMax: b, EveryMinute: every, Pinned: pl.Pinned}
	}
}

// shares is how much of the window the container's node was known, and
// contended, as shares of the container's stretches.
func (nc nodeContext) shares(pl Placement, stretches int) (known, contended float64) {
	if pl.NodeIP == "" || stretches == 0 || nc.waited == nil {
		return 0, 0
	}
	var k, c int
	for t, q := range nc.queue[pl.NodeIP] {
		if _, ok := nc.busy[pl.NodeIP][t]; !ok {
			continue
		}
		k++
		if q >= nodeContendedQueue {
			c++
		}
	}
	n := float64(stretches)
	return min(float64(k)/n, 1), min(float64(c)/n, 1)
}

// pressureOK reports whether PSI produced believable data in this Observe.
func (p *Prom) pressureOK() bool {
	var seen bool
	for _, h := range p.health {
		if h.Signal == SigPSI || h.Signal == SigPSIAvg {
			if !h.OK() {
				return false
			}
			seen = true
		}
	}
	return seen
}

// point is one step of a range query: Unix seconds and value.
type point struct {
	t int64
	v float64
}

// queryRangeByKey runs a range query and returns each container's points in
// time order. Steps with no value are left out, as Prometheus leaves them out.
func (p *Prom) queryRangeByKey(expr string, start, end time.Time, step time.Duration) (map[Key][]point, error) {
	series, err := p.queryRangeSeries(expr, start, end, step)
	if err != nil {
		return nil, err
	}
	out := map[Key][]point{}
	for _, s := range series {
		k := keyOf(s.labels)
		out[k] = append(out[k], s.points...)
	}
	return out, nil
}

// rangeSeries is one series of a range query.
type rangeSeries struct {
	labels map[string]string
	points []point
}

// queryRangeSeries runs a range query and returns every series with its points in
// time order.
func (p *Prom) queryRangeSeries(expr string, start, end time.Time, step time.Duration) ([]rangeSeries, error) {
	body, err := p.transport.Get(p.ctx(), "/api/v1/query_range", url.Values{
		"query": {expr},
		"start": {strconv.FormatInt(start.Unix(), 10)},
		"end":   {strconv.FormatInt(end.Unix(), 10)},
		"step":  {strconv.FormatInt(int64(step/time.Second), 10)},
	})
	if err != nil {
		return nil, err
	}
	var pr struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			Result []struct {
				Metric map[string]string `json:"metric"`
				Values [][2]any          `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &pr); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}
	if pr.Status != "success" {
		return nil, fmt.Errorf("query failed: %s", pr.Error)
	}
	out := make([]rangeSeries, 0, len(pr.Data.Result))
	for _, r := range pr.Data.Result {
		s := rangeSeries{labels: r.Metric}
		for _, pt := range r.Values {
			ts, ok1 := pt[0].(float64)
			vs, ok2 := pt[1].(string)
			if !ok1 || !ok2 {
				continue
			}
			v, err := strconv.ParseFloat(vs, 64)
			if err != nil || v != v {
				continue
			}
			s.points = append(s.points, point{int64(ts), v})
		}
		out = append(out, s)
	}
	return out, nil
}

// AgentStatus is tare-agent as `tare check` describes it.
type AgentStatus struct {
	// Nodes is how many agents report, by their build info.
	Nodes int
	// Matched is how many containers in scope the agent's series join to
	// through the kubelet's `id`. Agents present and nothing matched means the
	// join is broken: the kubelet's series lost `id` to a relabelling, or the
	// cgroup drivers of kubelet and runtime disagree.
	Matched int
	// Outside is containers the agents found outside the kubelet's `kubepods`
	// hierarchy, a cgroup driver mismatch, where the join may fail.
	Outside int
	// Failing are the agents whose self-checks failed over the window.
	Failing []AgentNode
	Err     error
}

// Agent describes tare-agent on this cluster for `tare check`.
func (p *Prom) Agent(namespaces []string, window time.Duration) AgentStatus {
	var st AgentStatus
	count := func(expr string) int {
		samples, err := p.query(expr)
		if err != nil && st.Err == nil {
			st.Err = err
		}
		var n float64
		for _, s := range samples {
			n += s.value
		}
		return int(n)
	}
	if st.Nodes = count(`count(tare_agent_build_info)`); st.Nodes == 0 || st.Err != nil {
		return st
	}
	wait, _, _ := cpuWaitExprs(selector(namespaces), "")
	st.Matched = count(fmt.Sprintf(`count(%s)`, wait))
	st.Outside = count(`sum(tare_agent_containers{location="outside"})`)
	failing, err := p.agentSelfChecks(window)
	if err != nil && st.Err == nil {
		st.Err = err
	}
	st.Failing = failing
	return st
}
