package metrics

import (
	"fmt"
	"time"
)

// SeriesCheck is one metric family's presence on this cluster, and what the
// report loses without it.
type SeriesCheck struct {
	Metric   string
	Buys     string // what this metric family pays for in the report
	Required bool
	Series   int
	Err      error

	// Note carries a qualification about series that exist: most importantly
	// the dead-pressure case, where the metric is present, the values are all
	// zero, and zero is the reassuring answer.
	Note string
}

// OK reports whether the family is usable.
func (c SeriesCheck) OK() bool { return c.Err == nil && c.Series > 0 && c.Note == "" }

// Preflight asks the backend which of tare's metric families it actually
// carries, before a report is run rather than after.
//
// The report already says what it could not see, but only once it has done all
// the work, and a first run against an unfamiliar cluster is exactly when
// somebody needs to know whether the answer will be worth anything. Each check
// is one instant query against a count, which is cheap enough to run on a
// cluster nobody has pointed this tool at before.
func (p *Prom) Preflight(namespaces []string, window time.Duration) []SeriesCheck {
	sel := selector(namespaces)
	w := durationString(window)

	checks := []SeriesCheck{
		{Metric: "container_cpu_usage_seconds_total", Required: true,
			Buys: "every CPU number in the report"},
		{Metric: "container_memory_working_set_bytes", Required: true,
			Buys: "every memory number, the leak check, and the confidence rating"},
		{Metric: "container_start_time_seconds",
			Buys: "telling a startup burst from steady state, so CPU limits are sized for boot"},
		{Metric: "container_pressure_cpu_waiting_seconds_total",
			Buys: "CPU saturation evidence on cgroup v2: without it or the throttle counters below, " +
				"no shrink can be called safe and findings drop to UNVERIFIED"},
		{Metric: "container_cpu_cfs_throttled_periods_total",
			Buys: "throttling evidence, which only exists for containers that already have a CPU limit"},
		{Metric: "container_oom_events_total",
			Buys: "OOMKill evidence, the hardest proof that a memory limit is too small"},
		{Metric: "kube_pod_container_status_restarts_total",
			Buys: "restart counts (kube-state-metrics), which corroborate an OOMKill"},
		{Metric: "kube_pod_container_resource_limits",
			Buys: "watching a limit move, which is how a runtime ceiling is inferred when the flag is not in the pod spec"},
		{Metric: "node_cpu_seconds_total",
			Buys: "node utilisation (node-exporter), without which tare cannot say whether a throttled container " +
				"was competing for a busy machine or stalling next to idle cores"},
	}

	for i := range checks {
		expr := fmt.Sprintf("count(%s{%s})", checks[i].Metric, sel)
		if checks[i].Metric == "node_cpu_seconds_total" {
			expr = fmt.Sprintf(`count(%s{mode="idle"})`, checks[i].Metric)
		}
		samples, err := p.query(expr)
		checks[i].Err = err
		for _, s := range samples {
			checks[i].Series += int(s.value)
		}
	}

	p.notePressure(&checks, sel, w)
	return checks
}

// CoveredKeys is the set of containers this backend holds any memory series
// for.
//
// Presence of a metric family says the backend scrapes cAdvisor somewhere; it
// says nothing about whether it scrapes the containers in front of you. A
// Prometheus configured with a namespace selector, or a node whose kubelet
// stopped reporting, produces a backend that passes every other check and
// covers a fraction of the cluster. The containers it misses do not vanish from
// the report - they arrive as NO-DATA rows - but a user deserves to know before
// running it that half the answer will be blank, and why.
// The query is windowed, not instant, because that is what a report reads: a
// container whose node stopped reporting this morning still has a fortnight of
// history behind it, and calling it uncovered here would contradict the report
// that is about to size it.
func (p *Prom) CoveredKeys(namespaces []string, window time.Duration) (map[Key]bool, error) {
	samples, err := p.query(fmt.Sprintf(
		`count by (namespace,pod,container) (count_over_time(container_memory_working_set_bytes{%s}[%s]))`,
		selector(namespaces), durationString(window)))
	if err != nil {
		return nil, err
	}
	out := make(map[Key]bool, len(samples))
	for _, s := range samples {
		out[Key{
			Namespace: s.labels["namespace"],
			Pod:       s.labels["pod"],
			Container: s.labels["container"],
		}] = true
	}
	return out, nil
}

// notePressure runs the dead-instrument control against the pressure family.
//
// Kubernetes issue #136333: a kubelet with the PSI feature gate enabled on a
// kernel without PSI support exports container_pressure_* for every container
// with every value zero. The metric is present, the query succeeds, and the
// answer means "no data" while reading as "no pressure". The full control in
// Observe needs a population of busy containers; here it is enough to say that
// nothing in the cluster has ever registered any pressure at all, which is
// worth a warning on a report nobody has run yet.
func (p *Prom) notePressure(checks *[]SeriesCheck, sel, w string) {
	for i := range *checks {
		c := &(*checks)[i]
		if c.Metric != "container_pressure_cpu_waiting_seconds_total" || !c.OK() {
			continue
		}
		samples, err := p.query(fmt.Sprintf(
			`count(max_over_time(rate(container_pressure_cpu_waiting_seconds_total{%s}[5m])[%s:5m]) > 0)`, sel, w))
		if err != nil {
			return
		}
		if len(samples) == 0 || samples[0].value == 0 {
			c.Note = fmt.Sprintf("%d series exist and every one of them is exactly zero across %s. "+
				"That is what a kernel without PSI support looks like behind a kubelet that exports "+
				"the series anyway (kubernetes#136333), and tare will withhold pressure rather than "+
				"read it as calm", c.Series, w)
		}
	}
}
