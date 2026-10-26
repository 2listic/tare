package metrics

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Prom queries a Prometheus-compatible HTTP API. It deliberately uses only the
// standard library: one dependency fewer in a tool whose install story is the
// product.
type Prom struct {
	transport Transport
	health    []SignalHealth

	// Ctx bounds every query. Source.Observe takes no context - the simulator
	// has nothing to cancel - so the deadline the command line asked for has
	// to reach the HTTP calls some other way. Without it a Prometheus that
	// accepts a connection and never answers hangs the whole run forever,
	// which is the failure mode of a proxied query against an overloaded
	// backend.
	Ctx context.Context
}

// New builds a source over any transport.
func New(t Transport) *Prom { return &Prom{transport: t} }

// Health reports what each query produced during the last Observe.
func (p *Prom) Health() []SignalHealth { return p.health }

// NewProm builds a source that talks to a Prometheus over the network.
func NewProm(baseURL string) *Prom { return New(NewHTTPTransport(baseURL)) }

func (p *Prom) Describe() string { return p.transport.Describe() }

type promResponse struct {
	Status string `json:"status"`
	Error  string `json:"error"`
	Data   struct {
		Result []struct {
			Metric map[string]string `json:"metric"`
			Value  [2]any            `json:"value"`
		} `json:"result"`
	} `json:"data"`
}

type sample struct {
	labels map[string]string
	value  float64
}

func (p *Prom) ctx() context.Context {
	if p.Ctx != nil {
		return p.Ctx
	}
	return context.Background()
}

func (p *Prom) query(expr string) ([]sample, error) {
	body, err := p.transport.Get(p.ctx(), "/api/v1/query", url.Values{"query": {expr}})
	if err != nil {
		return nil, err
	}
	var pr promResponse
	if err := json.Unmarshal(body, &pr); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}
	if pr.Status != "success" {
		return nil, fmt.Errorf("query failed: %s", pr.Error)
	}

	out := make([]sample, 0, len(pr.Data.Result))
	for _, r := range pr.Data.Result {
		s, ok := r.Value[1].(string)
		if !ok {
			continue
		}
		v, err := strconv.ParseFloat(s, 64)
		if err != nil || v != v { // reject NaN
			continue
		}
		out = append(out, sample{labels: r.Metric, value: v})
	}
	return out, nil
}

// Ping checks that the backend answers a query at all, so that "this cluster
// does not export the metric" can be told apart from "nothing is listening".
// The two are the same symptom at every layer above this one, and they need
// opposite responses from the user.
func (p *Prom) Ping() error {
	_, err := p.query("count(up)")
	return err
}

// selector builds the common label matcher. An empty namespace list means all.
func selector(namespaces []string) string {
	base := `container!="",container!="POD"`
	if len(namespaces) == 0 {
		return base
	}
	return fmt.Sprintf(`namespace=~"%s",%s`, strings.Join(namespaces, "|"), base)
}

// Observe runs the query set and joins the results by container key.
func (p *Prom) Observe(namespaces []string, window time.Duration) (map[Key]*Observation, error) {
	out := map[Key]*Observation{}
	p.health = p.health[:0]

	for _, q := range observeQueries(selector(namespaces), window) {
		samples, err := p.query(q.expr)
		p.health = append(p.health, SignalHealth{
			Signal: q.signal, Query: q.expr, Err: err,
			Series: len(samples), Required: q.required,
		})
		if err != nil {
			if q.required {
				return nil, fmt.Errorf("required signal %s: %w", q.signal, err)
			}
			continue
		}
		for _, s := range samples {
			k := keyOf(s.labels)
			if k.Container == "" || k.Pod == "" {
				continue
			}
			o, ok := out[k]
			if !ok {
				o = &Observation{Key: k}
				out[k] = o
			}
			q.assign(o, s.value)
			o.Mark(q.signal)
		}
	}
	p.flagDeadPressure(out)
	p.observeCPUWait(out, selector(namespaces), window)
	dropHalfTrends(out)
	return out, nil
}

// observeQuery is one expression of the Observe set and where its value goes.
//
// required marks a signal without which the report is not worth printing.
// Everything else may legitimately be absent on some clusters, but absence is
// recorded and surfaced rather than silently read as zero.
//
// reduce is the PromQL aggregator that collapses a container's incarnations,
// applied by (namespace, pod, container). A container restart gives cAdvisor a
// brand new series for the same container: identical namespace, pod and
// container labels, a new `id`. An unaggregated query therefore returns one
// sample per incarnation for the same key, and the join in Observe keeps
// whichever Prometheus happened to return last - as often as not the dead one,
// whose values never change again. That is how a leaker that OOMKilled and
// restarted kept reporting the pre-restart sample count, pressure and peak for
// twelve hours while the live container ran underneath it. Every query must
// state how its incarnations combine.
type observeQuery struct {
	signal   Signal
	expr     string
	reduce   string // aggregator over incarnations; "max" when empty
	assign   func(o *Observation, v float64)
	required bool
}

// observeQueries builds the Observe query set for a selector and window, each
// already wrapped in its incarnation aggregate.
func observeQueries(sel string, window time.Duration) []observeQuery {
	w := durationString(window)

	// step is the inner rate window for percentiles: five minutes smooths
	// scrape jitter without hiding sustained saturation.
	const step = "5m"

	// peakStep is deliberately shorter. A five minute rate averages a twenty
	// second startup burst down to nothing, which is exactly how a JVM ends up
	// being recommended a 10m CPU limit. Peaks that drive limits must be
	// measured at a resolution that can see them.
	const peakStep = "1m"

	// startupWindow is how long after a container start counts as boot.
	const startupWindow = 120

	queries := []observeQuery{
		{
			expr:   fmt.Sprintf(`quantile_over_time(0.95, rate(container_cpu_usage_seconds_total{%s}[%s])[%s:%s])`, sel, step, w, step),
			signal: SigCPUUsage, required: true,
			assign: func(o *Observation, v float64) { o.CPUUsageP95 = v },
		},
		{
			expr:   fmt.Sprintf(`max_over_time(rate(container_cpu_usage_seconds_total{%s}[%s])[%s:%s])`, sel, peakStep, w, peakStep),
			signal: SigCPUPeak, required: true,
			assign: func(o *Observation, v float64) { o.CPUUsageMax = v },
		},
		{
			// Container age at each evaluation step comes from comparing the
			// step timestamp with the recorded start time, which is what lets
			// a range query separate boot from steady state retrospectively.
			expr: fmt.Sprintf(`max_over_time((rate(container_cpu_usage_seconds_total{%s}[%s]) and on(namespace,pod,container) ((timestamp(container_start_time_seconds{%s}) - container_start_time_seconds{%s}) < %d))[%s:%s])`,
				sel, peakStep, sel, sel, startupWindow, w, peakStep),
			signal: SigStartup,
			assign: func(o *Observation, v float64) { o.CPUStartupMax = v },
		},
		{
			expr: fmt.Sprintf(`max_over_time((rate(container_cpu_usage_seconds_total{%s}[%s]) and on(namespace,pod,container) ((timestamp(container_start_time_seconds{%s}) - container_start_time_seconds{%s}) >= %d))[%s:%s])`,
				sel, peakStep, sel, sel, startupWindow, w, peakStep),
			signal: SigStartup,
			assign: func(o *Observation, v float64) { o.CPUSteadyMax = v },
		},
		{
			expr:   fmt.Sprintf(`max_over_time(container_memory_working_set_bytes{%s}[%s])`, sel, w),
			signal: SigMemory, required: true,
			assign: func(o *Observation, v float64) { o.MemWorkingSet = v },
		},
		{
			// Peak pressure uses the fine window for the same reason the CPU
			// peak does: a container starved for forty seconds during boot
			// shows barely any pressure once averaged over five minutes.
			expr:   fmt.Sprintf(`max_over_time(rate(container_pressure_cpu_waiting_seconds_total{%s}[%s])[%s:%s])`, sel, peakStep, w, peakStep),
			signal: SigPSI,
			assign: func(o *Observation, v float64) { o.PSIWaitingMax = v },
		},
		{
			// Sustained pressure is what the saturation threshold actually
			// means: seconds of stall per second of wall time, averaged over
			// the window. A JVM that stalls for one minute an hour on GC
			// averages 0.0007 here and peaks above 0.03; only the average
			// distinguishes it from a container that is genuinely starved.
			expr:   fmt.Sprintf(`avg_over_time(rate(container_pressure_cpu_waiting_seconds_total{%s}[%s])[%s:%s])`, sel, step, w, step),
			signal: SigPSIAvg,
			assign: func(o *Observation, v float64) { o.PSIWaitingAvg = v },
		},
		{
			// Sustained throttling: the share of scheduling periods clipped
			// across the whole window, which is what an operator means by "this
			// container is throttled".
			expr: fmt.Sprintf(`rate(container_cpu_cfs_throttled_periods_total{%s}[%s]) / clamp_min(rate(container_cpu_cfs_periods_total{%s}[%s]), 1)`,
				sel, w, sel, w),
			signal: SigThrottleAvg,
			assign: func(o *Observation, v float64) { o.ThrottleAvg = v },
		},
		{
			expr: fmt.Sprintf(`max_over_time((rate(container_cpu_cfs_throttled_periods_total{%s}[%s]) / clamp_min(rate(container_cpu_cfs_periods_total{%s}[%s]), 1))[%s:%s])`,
				sel, step, sel, step, w, step),
			signal: SigThrottle,
			assign: func(o *Observation, v float64) { o.ThrottleRatio = v },
		},
		{
			expr:   fmt.Sprintf(`max_over_time(container_oom_events_total{%s}[%s])`, sel, w),
			signal: SigOOM,
			assign: func(o *Observation, v float64) { o.OOMEvents = v },
		},
		{
			// Restarts DURING the window, not the pod's lifetime total.
			// restartCount is cumulative and only resets when the pod is
			// replaced, so max_over_time on a month-old pod reports every
			// restart it ever had against a seven-day window. Subtracting the
			// window's opening value is exact, and needs no extrapolation.
			expr: fmt.Sprintf(`max_over_time(kube_pod_container_status_restarts_total{%s}[%s]) - min_over_time(kube_pod_container_status_restarts_total{%s}[%s])`,
				sel, w, sel, w),
			signal: SigRestarts,
			assign: func(o *Observation, v float64) { o.Restarts = v },
		},
		{
			// Summed, not maxed: the incarnations partition the window between
			// them, so the container's coverage is their total. Taking the
			// largest would freeze confidence at whatever the longest-lived
			// incarnation reached and let it decay as the window slid past.
			expr:   fmt.Sprintf(`count_over_time(container_memory_working_set_bytes{%s}[%s])`, sel, w),
			signal: SigMemory, required: true, reduce: "sum",
			assign: func(o *Observation, v float64) { o.Samples = int(v) },
		},
		{
			expr:   fmt.Sprintf(`quantile_over_time(0.95, container_memory_working_set_bytes{%s}[%s])`, sel, w),
			signal: SigMemP95,
			assign: func(o *Observation, v float64) { o.MemP95 = v },
		},
		{
			expr:   fmt.Sprintf(`max_over_time(kube_pod_container_resource_limits{%s,resource="memory"}[1h])`, sel),
			signal: SigLimitTrend,
			assign: func(o *Observation, v float64) { o.MemLimitLate = v },
		},
		{
			expr:   fmt.Sprintf(`avg_over_time(container_memory_working_set_bytes{%s}[1h])`, sel),
			signal: SigMemTrend,
			assign: func(o *Observation, v float64) { o.MemLate = v },
		},
	}

	// The early-window average needs an offset, which only makes sense once the
	// window is wide enough to have an "early" at all.
	if window >= 4*time.Hour {
		queries = append(queries, observeQuery{
			expr:   fmt.Sprintf(`avg_over_time(container_memory_working_set_bytes{%s}[1h] offset %s)`, sel, durationString(window-time.Hour)),
			signal: SigMemTrend,
			assign: func(o *Observation, v float64) { o.MemEarly = v },
		})
		queries = append(queries, observeQuery{
			expr:   fmt.Sprintf(`max_over_time(kube_pod_container_resource_limits{%s,resource="memory"}[1h] offset %s)`, sel, durationString(window-time.Hour)),
			signal: SigLimitTrend,
			assign: func(o *Observation, v float64) { o.MemLimitEarly = v },
		})
	}

	// Collapse every query to one series per container before it is sent. See
	// observeQuery.reduce for why an unaggregated query is a silent trap.
	// max is the default because every other field here is already a worst case
	// over the pods of a workload, and an incarnation is the same kind of thing:
	// the recommendation has to hold for the worst one, not the most recent.
	for i := range queries {
		op := queries[i].reduce
		if op == "" {
			op = "max"
		}
		queries[i].expr = fmt.Sprintf(`%s by (namespace,pod,container) (%s)`, op, queries[i].expr)
	}

	return queries
}

// dropHalfTrends unmarks a trend that has only one of its two halves. The late
// query returns a series for any live container, and marking per-series would
// otherwise record the signal as present while the early half matched nothing
// - which is what happens to any pod created after the window opened. Leak
// detection and limit-trend inference are both guarded on the early value
// being non-zero, so they would silently never run while the report claimed
// nothing was missing.
func dropHalfTrends(out map[Key]*Observation) {
	for _, o := range out {
		if o.MemEarly == 0 {
			delete(o.Have, SigMemTrend)
		}
		if o.MemLimitEarly == 0 {
			delete(o.Have, SigLimitTrend)
		}
	}
}

// keyOf reads a container key from a series' labels.
func keyOf(labels map[string]string) Key {
	return Key{Namespace: labels["namespace"], Pod: labels["pod"], Container: labels["container"]}
}

// AvailableWindow reports how much history this backend actually holds for the
// containers in scope, capped at want.
//
// The default window is two weeks and Prometheus retains about fifteen days, so
// a first run on a young cluster asks for more history than exists. Sample
// coverage drives the confidence rating, so every finding came back "low" - the
// tool reporting that it knows nothing, on the one run that decides whether
// anyone keeps it. Probing is coarse on purpose: the answer picks a window, and
// each probe is one instant query against an aggregate.
func (p *Prom) AvailableWindow(namespaces []string, want time.Duration) (time.Duration, error) {
	sel := selector(namespaces)
	for i, d := range []time.Duration{
		want,
		14 * 24 * time.Hour,
		10 * 24 * time.Hour,
		7 * 24 * time.Hour,
		5 * 24 * time.Hour,
		3 * 24 * time.Hour,
		2 * 24 * time.Hour,
		36 * time.Hour,
		24 * time.Hour,
		12 * time.Hour,
		6 * time.Hour,
		3 * time.Hour,
		time.Hour,
	} {
		// want is probed first; a later step equal to it is the same query
		// again, and a step above it is a window nobody asked for.
		if i > 0 && d >= want {
			continue
		}
		samples, err := p.query(fmt.Sprintf(
			`count(container_memory_working_set_bytes{%s} offset %s)`, sel, durationString(d)))
		if err != nil {
			return 0, err
		}
		if len(samples) > 0 {
			return d, nil
		}
	}
	return 0, nil
}

// durationString renders a duration the way PromQL wants it.
func durationString(d time.Duration) string {
	if d%(24*time.Hour) == 0 {
		return fmt.Sprintf("%dd", int(d.Hours())/24)
	}
	if d%time.Hour == 0 {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dm", int(d.Minutes()))
}

// psiControlMinCPU is the CPU a container must be drawing before its pressure
// reading counts as evidence about the instrument. A container that never runs
// genuinely has nothing to wait for, and including it would make the control
// fire on any quiet cluster.
//
// psiControlMinPop is how many such containers must agree before a cluster-wide
// zero is called a dead instrument rather than a calm one. A cluster with two
// busy containers can honestly have no contention at all; the claim being made
// here needs a population.
//
// Neither number is derived, and both are chosen so the control errs toward
// silence. What limits the damage is the direction of the error: firing
// wrongly removes a saturation signal, which pushes findings to Unverified and
// confidence to Low, so a mistake here makes the tool more cautious and never
// less. Failing to fire is the expensive direction, and that is the one the
// thresholds are not protecting.
const (
	psiControlMinCPU = 0.05
	psiControlMinPop = 3
)

// flagDeadPressure is the negative control for PSI.
//
// Kubernetes issue #136333: on hosts without kernel PSI support, a kubelet with
// the feature gate enabled emitted container_pressure_* series for every
// container with every value zero. No error, no absent metric, no gap in the
// series - a flat zero, which reads as "no pressure" and is indistinguishable
// from "no data" to everything downstream. Signal availability here is decided
// by a query returning series, and a zero-valued series is still a series, so
// such a cluster would have every container reported comfortably shrinkable at
// high confidence.
//
// The discriminator is that pressure is a peak over a fine bucket across the
// whole window, and it never decreases. Real containers doing real work on a
// real kernel stall occasionally: waiting on a runqueue is ordinary, and over
// days of wall time an entire population registering EXACTLY zero is not a
// quiet cluster, it is an instrument that is not connected. Note this tests the
// digit, not a threshold - a cluster genuinely at 1e-9 pressure still passes.
//
// The bug is fixed upstream. Unfixed kubelets are in the field, and the general
// problem - a signal that reports a confident zero rather than failing - is not
// fixed by anyone else's release.
func (p *Prom) flagDeadPressure(out map[Key]*Observation) {
	var pop, zero int
	for _, o := range out {
		if !o.Has(SigPSI) || o.CPUUsageP95 < psiControlMinCPU {
			continue
		}
		pop++
		if o.PSIWaitingMax == 0 && o.PSIWaitingAvg == 0 {
			zero++
		}
	}
	if pop < psiControlMinPop || zero != pop {
		return
	}

	reason := fmt.Sprintf(
		"every one of the %d containers drawing at least %.0fm reported exactly zero CPU pressure "+
			"across the whole window. A busy container that never once waited for the runqueue is not "+
			"a calm cluster, it is a kernel without PSI support behind a kubelet that exports the "+
			"series anyway (kubernetes#136333). The values are withheld rather than read as no pressure",
		pop, psiControlMinCPU*1000)

	for _, o := range out {
		delete(o.Have, SigPSI)
		delete(o.Have, SigPSIAvg)
	}
	for i := range p.health {
		if p.health[i].Signal == SigPSI || p.health[i].Signal == SigPSIAvg {
			p.health[i].Suspect = reason
		}
	}
}
