package metrics

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

func TestSummariseCPUWait(t *testing.T) {
	quiet := WaitStretch{Wait: 0.02, Run: 0.9, Usage: 1} // slowdown 0.02
	cases := []struct {
		name        string
		st          []WaitStretch
		stalled     float64
		hasNormal   bool
		busy        bool
		normal, cov float64
	}{
		{
			// Twice the normal and above the idle floor: stalled.
			name:    "relative",
			st:      append(repeat(quiet, 9), WaitStretch{Wait: 0.5, Run: 1, Usage: 1}),
			stalled: 0.1, hasNormal: true, busy: true, normal: 0.02, cov: 0.9,
		},
		{
			// Twice a tiny normal, but the wait itself is negligible.
			name:    "idle floor",
			st:      append(repeat(WaitStretch{Wait: 0.001, Run: 1, Usage: 1}, 9), WaitStretch{Wait: 0.005, Run: 1, Usage: 1}),
			stalled: 0, hasNormal: true, busy: true, normal: 0.001, cov: 1,
		},
		{
			// Always contended: the normal is high, so only the ceiling
			// can call it stalled, and it does.
			name:    "ceiling",
			st:      repeat(WaitStretch{Wait: 1.2, Run: 1, Usage: 1}, 4),
			stalled: 1, hasNormal: true, busy: true, normal: 1.2, cov: 1,
		},
		{
			// Never above MinUsage: no slowdown, no normal, no coverage.
			name: "idle",
			st:   repeat(WaitStretch{Wait: 0.004, Run: 0.001, Usage: 0.001}, 5),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := SummariseCPUWait(c.st)
			if got.Stretches != len(c.st) {
				t.Errorf("stretches = %d, want %d", got.Stretches, len(c.st))
			}
			if math.Abs(got.Stalled-c.stalled) > 1e-9 {
				t.Errorf("stalled = %v, want %v", got.Stalled, c.stalled)
			}
			if got.HasNormal != c.hasNormal || math.Abs(got.Normal-c.normal) > 1e-9 {
				t.Errorf("normal = %v (%v), want %v (%v)", got.Normal, got.HasNormal, c.normal, c.hasNormal)
			}
			if got.Busy != c.busy || math.Abs(got.Coverage-c.cov) > 1e-9 {
				t.Errorf("coverage = %v (%v), want %v (%v)", got.Coverage, got.Busy, c.cov, c.busy)
			}
		})
	}
}

func repeat(s WaitStretch, n int) []WaitStretch {
	out := make([]WaitStretch, n)
	for i := range out {
		out[i] = s
	}
	return out
}

// The quantile is PromQL's, interpolated, so the Go reference and the query
// give the same normal.
func TestInterpolatedQuantileMatchesPromQL(t *testing.T) {
	if got := interpolatedQuantile([]float64{4, 1, 3, 2}, 0.1); math.Abs(got-1.3) > 1e-9 {
		t.Errorf("p10 of 1..4 = %v, want 1.3", got)
	}
	if got := interpolatedQuantile([]float64{7}, 0.5); got != 7 {
		t.Errorf("median of one value = %v", got)
	}
}

// The agent's series carry the agent pod's own labels and no container names.
// A namespace filter on them matches nothing, and an unsummed join collides
// with the labels it adds.
func TestCPUWaitJoinShape(t *testing.T) {
	wait, _, _ := cpuWaitExprs(selector([]string{"shop"}), `instance!~"10.0.0.1:9105"`)
	if !strings.Contains(wait, `tare_cpu_wait_seconds_total{instance!~"10.0.0.1:9105"}`) {
		t.Errorf("agent side must carry only the instance exclusion: %s", wait)
	}
	if !strings.Contains(wait, `sum by (id) (rate(tare_cpu_wait_seconds_total`) {
		t.Errorf("agent series must be summed by id before the join: %s", wait)
	}
	if !strings.Contains(wait, `container_cpu_usage_seconds_total{namespace=~"shop",container!="",container!="POD"}`) {
		t.Errorf("kubelet side of the join must carry the report's selector: %s", wait)
	}
	if !strings.Contains(wait, "on (id) group_left (namespace,pod,container)") {
		t.Errorf("join must be on id: %s", wait)
	}
	for _, e := range append([]string{stalledCandidatesExpr("", "")}, exprs(cpuWaitQueries("", "", 24*time.Hour))...) {
		if strings.Count(e, "(") != strings.Count(e, ")") || strings.Count(e, "{") != strings.Count(e, "}") {
			t.Errorf("unbalanced: %s", truncate(e))
		}
	}
}

func exprs(qs []cpuWaitQuery) []string {
	out := make([]string, len(qs))
	for i, q := range qs {
		out[i] = q.expr
	}
	return out
}

// agentCluster fakes a Prometheus with tare-agent. psi says whether the
// kubelet exports PSI; candidates are the stalled-candidate slowdowns of
// container "app"; unknownLayout names an agent that fails a self-check.
type agentCluster struct {
	agent, psi    bool
	unknownLayout string
	normal        float64
	stretches     int
	candidates    []float64
}

func (c agentCluster) respond(expr string) (string, int) {
	switch {
	case expr == agentPresentExpr:
		if !c.agent {
			return emptyVector, 200
		}
		return vector("", 3), 200
	case strings.Contains(expr, "tare_agent_exits_layout_unknown_total"):
		if c.unknownLayout == "" {
			return emptyVector, 200
		}
		return labelled(map[string]string{"instance": c.unknownLayout}, 1), 200
	case strings.Contains(expr, "tare_agent_"):
		return emptyVector, 200
	case strings.Contains(expr, " or ("): // the stalled candidates, a range query
		return matrix("app", c.candidates), 200
	case strings.Contains(expr, "count_over_time((sum by (namespace,pod,container) (sum by (id)"):
		return vector("app", float64(c.stretches)), 200
	case strings.Contains(expr, "quantile_over_time(0.1,"):
		return vector("app", c.normal), 200
	case strings.Contains(expr, "quantile_over_time(0.5, (sum by (namespace,pod,container) (sum by (id) (rate(tare_cpu_run"):
		return vector("app", 0.8), 200
	case strings.Contains(expr, "container_pressure_cpu_waiting") && !c.psi:
		return emptyVector, 200
	}
	return vector("app", 0.5), 200
}

func labelled(labels map[string]string, v float64) string {
	b, _ := json.Marshal(map[string]any{
		"status": "success",
		"data": map[string]any{"resultType": "vector", "result": []any{
			map[string]any{"metric": labels, "value": []any{1700000000, formatFloat(v)}},
		}},
	})
	return string(b)
}

func matrix(container string, values []float64) string {
	pts := make([]any, len(values))
	for i, v := range values {
		pts[i] = []any{1700000000 + 300*i, formatFloat(v)}
	}
	b, _ := json.Marshal(map[string]any{
		"status": "success",
		"data": map[string]any{"resultType": "matrix", "result": []any{
			map[string]any{
				"metric": map[string]string{"namespace": "ns", "pod": "p-0", "container": container},
				"values": pts,
			},
		}},
	})
	return string(b)
}

func healthOf(p *Prom, s Signal) []SignalHealth {
	var out []SignalHealth
	for _, h := range p.Health() {
		if h.Signal == s {
			out = append(out, h)
		}
	}
	return out
}

// The stalled share is the candidates at or above the container's own
// threshold, over every stretch: here normal 0.1, so 0.2 and up counts.
func TestObserveCPUWait(t *testing.T) {
	c := agentCluster{agent: true, normal: 0.1, stretches: 100, candidates: []float64{0.15, 0.2, 0.5, 1.3}}
	f := newFakeProm(c.respond)
	defer f.Close()

	p := NewProm(f.URL)
	obs, err := p.Observe([]string{"ns"}, 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	o := obs[Key{Namespace: "ns", Pod: "p-0", Container: "app"}]
	if !o.Has(SigCPUWait) {
		t.Fatal("CPU wait not marked")
	}
	w := o.CPUWait
	if w.Stretches != 100 || !w.HasNormal || w.Normal != 0.1 || !w.Busy || w.Coverage != 0.8 {
		t.Errorf("summary = %+v", w)
	}
	if math.Abs(w.Stalled-0.03) > 1e-9 {
		t.Errorf("stalled = %v, want 0.03 (0.2, 0.5 and 1.3 of 100 stretches)", w.Stalled)
	}
	for _, h := range healthOf(p, SigCPUWait) {
		if !h.OK() {
			t.Errorf("unhealthy CPU wait query on a healthy agent: %+v", h)
		}
	}
}

// Without a normal only the ceiling applies.
func TestObserveCPUWaitWithoutNormal(t *testing.T) {
	c := agentCluster{agent: true, stretches: 10, candidates: []float64{0.5, 1.0}}
	f := newFakeProm(func(expr string) (string, int) {
		if strings.Contains(expr, "quantile_over_time(0.1,") {
			return emptyVector, 200
		}
		return c.respond(expr)
	})
	defer f.Close()

	obs, err := NewProm(f.URL).Observe([]string{"ns"}, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	w := obs[Key{Namespace: "ns", Pod: "p-0", Container: "app"}].CPUWait
	if w.HasNormal || math.Abs(w.Stalled-0.1) > 1e-9 {
		t.Errorf("want only the ceiling stretch stalled, got %+v", w)
	}
}

// An agent failing its self-checks is left out of every CPU wait query, and
// the report header says which node and why.
func TestFailingAgentIsExcluded(t *testing.T) {
	c := agentCluster{agent: true, unknownLayout: "10.0.0.7:9105", normal: 0.1, stretches: 50}
	f := newFakeProm(c.respond)
	defer f.Close()

	p := NewProm(f.URL)
	if _, err := p.Observe(nil, 24*time.Hour); err != nil {
		t.Fatal(err)
	}
	var queried int
	for _, e := range f.seen {
		if strings.Contains(e, "tare_cpu_wait_seconds_total{") {
			queried++
			if !strings.Contains(e, `tare_cpu_wait_seconds_total{instance!~"10\\.0\\.0\\.7:9105"}`) {
				t.Errorf("failing agent not excluded: %s", truncate(e))
			}
		}
	}
	if queried == 0 {
		t.Fatal("no CPU wait query was sent")
	}
	var suspect string
	for _, h := range healthOf(p, SigCPUWait) {
		if h.Suspect != "" {
			suspect = h.Suspect
		}
	}
	if !strings.Contains(suspect, "10.0.0.7:9105") || !strings.Contains(suspect, "layout") {
		t.Errorf("health must name the node and the problem, got %q", suspect)
	}
}

// Where PSI works nobody installs the agent, and saying it is missing would
// be noise. Where PSI is missing, its absence is the reason findings are
// UNVERIFIED, and must be said.
func TestAgentAbsenceReportedOnlyWithoutPSI(t *testing.T) {
	for _, psi := range []bool{true, false} {
		f := newFakeProm(agentCluster{psi: psi}.respond)
		p := NewProm(f.URL)
		if _, err := p.Observe(nil, 24*time.Hour); err != nil {
			t.Fatal(err)
		}
		f.Close()
		h := healthOf(p, SigCPUWait)
		if psi && len(h) != 0 {
			t.Errorf("PSI present: agent absence reported anyway: %+v", h)
		}
		if !psi && (len(h) != 1 || h[0].OK()) {
			t.Errorf("PSI absent: agent absence must be reported once, got %+v", h)
		}
		for _, e := range f.seen {
			if strings.Contains(e, "tare_cpu_wait_seconds_total{") {
				t.Errorf("CPU wait queried with no agent present: %s", truncate(e))
			}
		}
	}
}
