package metrics

import (
	"fmt"
	"strings"
	"time"
)

// ObserveNodes reads how busy each node actually was over the window.
//
// The expression divides non-idle CPU seconds by the core count, both taken
// from node-exporter's own series, which keeps the result correct on nodes of
// different sizes without needing the Kubernetes inventory here.
//
// It deliberately does not reuse the `by (namespace,pod,container)` aggregation
// that wraps every query in Observe: these series have no container labels, and
// aggregating by them would collapse every node into one meaningless row.
func (p *Prom) ObserveNodes(ips []string, window time.Duration) (map[string]NodeObservation, error) {
	if len(ips) == 0 {
		return map[string]NodeObservation{}, nil
	}
	w := durationString(window)
	const step = "5m"

	// util is the instantaneous utilisation fraction per node-exporter target.
	util := fmt.Sprintf(
		`(sum by (instance) (rate(node_cpu_seconds_total{mode!="idle"}[%s])) / count by (instance) (node_cpu_seconds_total{mode="idle"}))`,
		step)

	queries := []struct {
		expr   string
		assign func(*NodeObservation, float64)
	}{
		{fmt.Sprintf(`quantile_over_time(0.95, %s[%s:%s])`, util, w, step),
			func(n *NodeObservation, v float64) { n.CPUUtilP95 = v }},
		{fmt.Sprintf(`avg_over_time(%s[%s:%s])`, util, w, step),
			func(n *NodeObservation, v float64) { n.CPUUtilMean = v }},
		{fmt.Sprintf(`count_over_time(node_cpu_seconds_total{mode="idle",cpu="0"}[%s])`, w),
			func(n *NodeObservation, v float64) { n.Samples = int(v) }},
	}

	out := map[string]NodeObservation{}
	want := map[string]bool{}
	for _, ip := range ips {
		want[ip] = true
	}

	for i, q := range queries {
		samples, err := p.query(q.expr)
		// Health is appended rather than reset: Observe owns the slice and has
		// already run by the time this is called. Only the first query is
		// recorded, so one absent node-exporter reads as one missing signal
		// rather than three.
		if i == 0 {
			p.health = append(p.health, SignalHealth{
				Signal: SigNodeUtil, Query: q.expr, Err: err, Series: len(samples),
			})
		}
		if err != nil {
			// Node utilisation is never required. A cluster without
			// node-exporter still gets every other finding.
			return out, nil
		}
		for _, s := range samples {
			ip, ok := ipOf(s.labels["instance"])
			if !ok || !want[ip] {
				continue
			}
			n := out[ip]
			q.assign(&n, s.value)
			out[ip] = n
		}
	}
	return out, nil
}

// ipOf strips the port from node-exporter's `instance` label.
//
// The label is "172.18.0.3:9100" and carries no node name, verified against a
// live cluster. The port is not assumed to be 9100: any suffix after the last
// colon is dropped, and an IPv6 target in brackets is handled by taking what is
// inside them.
func ipOf(instance string) (string, bool) {
	if instance == "" {
		return "", false
	}
	if strings.HasPrefix(instance, "[") {
		if end := strings.Index(instance, "]"); end > 1 {
			return instance[1:end], true
		}
		return "", false
	}
	host, _, found := strings.Cut(instance, ":")
	if !found {
		return instance, true
	}
	return host, host != ""
}
