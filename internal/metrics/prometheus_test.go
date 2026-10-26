package metrics

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeProm serves canned responses. It exists because the entire query set was
// previously validated only against one live cluster, where a broken
// expression would have returned nothing and been read as "no pressure".

type fakeProm struct {
	*httptest.Server
	// respond decides what each query gets. It receives the raw expression.
	respond func(expr string) (body string, status int)
	seen    []string
}

func newFakeProm(respond func(expr string) (string, int)) *fakeProm {
	f := &fakeProm{respond: respond}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		expr := r.URL.Query().Get("query")
		f.seen = append(f.seen, expr)
		body, status := f.respond(expr)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	return f
}

func vector(container string, value float64) string {
	b, _ := json.Marshal(map[string]any{
		"status": "success",
		"data": map[string]any{
			"resultType": "vector",
			"result": []any{map[string]any{
				"metric": map[string]string{"namespace": "ns", "pod": "p-0", "container": container},
				"value":  []any{1700000000, formatFloat(value)},
			}},
		},
	})
	return string(b)
}

func formatFloat(v float64) string {
	b, _ := json.Marshal(v)
	return strings.Trim(string(b), `"`)
}

const emptyVector = `{"status":"success","data":{"resultType":"vector","result":[]}}`

func TestAllSignalsPresent(t *testing.T) {
	f := newFakeProm(func(expr string) (string, int) { return vector("app", 1.5), 200 })
	defer f.Close()

	obs, err := NewProm(f.URL).Observe([]string{"ns"}, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	o := obs[Key{Namespace: "ns", Pod: "p-0", Container: "app"}]
	if o == nil {
		t.Fatal("no observation produced")
	}
	for _, s := range []Signal{SigCPUUsage, SigCPUPeak, SigMemory, SigMemP95,
		SigPSI, SigThrottle, SigOOM, SigRestarts, SigStartup, SigMemTrend, SigLimitTrend} {
		if !o.Has(s) {
			t.Errorf("signal %s not marked despite data being returned", s)
		}
	}
}

// TestMissingSignalIsRecordedNotZeroed is the regression this whole change
// exists for: a metric this cluster does not export must leave a visible gap.
func TestMissingSignalIsRecordedNotZeroed(t *testing.T) {
	f := newFakeProm(func(expr string) (string, int) {
		if strings.Contains(expr, "container_pressure_cpu_waiting") {
			return emptyVector, 200 // this cluster does not export PSI
		}
		return vector("app", 1), 200
	})
	defer f.Close()

	p := NewProm(f.URL)
	obs, err := p.Observe([]string{"ns"}, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	o := obs[Key{Namespace: "ns", Pod: "p-0", Container: "app"}]
	if o.Has(SigPSI) {
		t.Error("PSI marked present when the cluster returned nothing")
	}
	if o.PSIWaitingMax != 0 {
		t.Error("PSI value should be zero, but the point is that Has() is false")
	}

	var found bool
	for _, h := range p.Health() {
		if h.Signal == SigPSI {
			found = true
			if h.OK() {
				t.Error("health should report the PSI query as producing nothing")
			}
			if h.Err != nil {
				t.Errorf("an empty result is not an error, got %v", h.Err)
			}
		}
	}
	if !found {
		t.Error("PSI missing from the health report entirely")
	}
}

// TestBrokenQueryIsAnErrorNotSilence distinguishes a broken expression from an
// unexported metric. Conflating them hides bugs in this repository.
func TestBrokenQueryIsAnErrorNotSilence(t *testing.T) {
	f := newFakeProm(func(expr string) (string, int) {
		if strings.Contains(expr, "container_oom_events_total") {
			return `{"status":"error","errorType":"bad_data","error":"parse error"}`, 400
		}
		return vector("app", 1), 200
	})
	defer f.Close()

	p := NewProm(f.URL)
	if _, err := p.Observe([]string{"ns"}, 24*time.Hour); err != nil {
		t.Fatalf("an optional signal failing must not fail the whole report: %v", err)
	}
	for _, h := range p.Health() {
		if h.Signal == SigOOM {
			if h.Err == nil {
				t.Error("a query that returned an API error must record it")
			}
			return
		}
	}
	t.Error("OOM signal missing from the health report")
}

// TestRequiredSignalFailureFailsLoudly: without CPU or memory there is no
// report worth printing, and pretending otherwise wastes the reader's trust.
func TestRequiredSignalFailureFailsLoudly(t *testing.T) {
	f := newFakeProm(func(expr string) (string, int) {
		if strings.Contains(expr, "container_cpu_usage_seconds_total") {
			return `{"status":"error","error":"boom"}`, 500
		}
		return vector("app", 1), 200
	})
	defer f.Close()

	if _, err := NewProm(f.URL).Observe([]string{"ns"}, 24*time.Hour); err == nil {
		t.Fatal("a required signal failing must return an error")
	}
}

// TestNamespaceSelectorIsApplied guards against a silently unfiltered query
// pulling the whole cluster into a namespaced report.
func TestNamespaceSelectorIsApplied(t *testing.T) {
	f := newFakeProm(func(expr string) (string, int) { return emptyVector, 200 })
	defer f.Close()
	NewProm(f.URL).Observe([]string{"alpha", "beta"}, 24*time.Hour)

	for _, expr := range f.seen {
		if agentWide(expr) {
			continue
		}
		if !strings.Contains(expr, `namespace=~"alpha|beta"`) {
			t.Errorf("query is not namespace-scoped: %s", truncate(expr))
		}
		if !strings.Contains(expr, `container!=""`) {
			t.Errorf("query does not exclude the pod sandbox: %s", truncate(expr))
		}
	}
}

// TestQueriesAreWellFormed catches the class of bug that produced a 10m CPU
// limit for a JVM: a plausible-looking expression with the wrong window.
func TestQueriesAreWellFormed(t *testing.T) {
	f := newFakeProm(func(expr string) (string, int) { return emptyVector, 200 })
	defer f.Close()
	NewProm(f.URL).Observe(nil, 24*time.Hour)

	var peak, p95 string
	for _, raw := range containerQueries(f.seen) {
		e := unwrapReduce(t, raw)
		if strings.HasPrefix(e, "max_over_time(rate(container_cpu_usage_seconds_total") {
			peak = e
		}
		if strings.HasPrefix(e, "quantile_over_time(0.95, rate(container_cpu_usage_seconds_total") {
			p95 = e
		}
	}
	if peak == "" || p95 == "" {
		t.Fatal("expected both a peak and a percentile CPU query")
	}
	// The peak must be measured at a resolution that can see a short burst.
	if !strings.Contains(peak, "[1m]") {
		t.Errorf("peak query must use a 1m rate window or it averages bursts away: %s", truncate(peak))
	}
	if !strings.Contains(p95, "[5m]") {
		t.Errorf("percentile query should smooth with a 5m rate window: %s", truncate(p95))
	}
	// Pressure is queried twice, for two different jobs, and each has its own
	// requirement. The peak has the same one as peak CPU: a 1m rate window, or
	// a short starvation episode is averaged away. The sustained measure is a
	// mean and must span the whole window, because deciding saturation on a
	// peak marks a container starved for one bad minute.
	var psiPeak, psiAvg int
	for _, raw := range containerQueries(f.seen) {
		e := unwrapReduce(t, raw)
		if !strings.Contains(e, "container_pressure_cpu_waiting") {
			continue
		}
		switch {
		case strings.HasPrefix(e, "max_over_time"):
			psiPeak++
			if !strings.Contains(e, "[1m]") {
				t.Errorf("PSI peak must use a 1m rate window, or a short starvation episode "+
					"is averaged away exactly as the CPU peak was: %s", truncate(e))
			}
		case strings.HasPrefix(e, "avg_over_time"):
			psiAvg++
			if !strings.Contains(e, ":") {
				t.Errorf("sustained PSI must average over the whole window, not a single "+
					"instant: %s", truncate(e))
			}
		default:
			t.Errorf("unclassified pressure query: %s", truncate(e))
		}
	}
	if psiPeak != 1 || psiAvg != 1 {
		t.Errorf("want one peak and one sustained pressure query, got %d and %d. "+
			"The verdict rests on the sustained one; losing it silently returns "+
			"saturation to being decided on a single worst bucket", psiPeak, psiAvg)
	}
	for _, e := range f.seen {
		if strings.Count(e, "{") != strings.Count(e, "}") {
			t.Errorf("unbalanced braces in: %s", truncate(e))
		}
		if strings.Count(e, "(") != strings.Count(e, ")") {
			t.Errorf("unbalanced parens in: %s", truncate(e))
		}
	}
}

// unwrapReduce strips the by-container aggregation every query is wrapped in,
// so the assertions above match the expression that does the measuring rather
// than the wrapper - and fails the query that is missing it, because an
// unaggregated query is what let a dead container's series win the join.
func unwrapReduce(t *testing.T, e string) string {
	t.Helper()
	for _, op := range []string{"max", "sum"} {
		prefix := op + " by (namespace,pod,container) ("
		if strings.HasPrefix(e, prefix) && strings.HasSuffix(e, ")") {
			return e[len(prefix) : len(e)-1]
		}
	}
	t.Errorf("query is not aggregated by container, so a restarted container's dead "+
		"series can win the join: %s", truncate(e))
	return e
}

// TestIncarnationsAreAggregated is the regression test for a leaker that
// OOMKilled, restarted, and then reported the dead container's sample count,
// pressure and peak unchanged for twelve hours while the live one ran
// underneath. cAdvisor gives the restarted container its own series under the
// same namespace, pod and container labels, so an unaggregated query returns a
// sample per incarnation and the join keeps whichever arrived last.
func TestIncarnationsAreAggregated(t *testing.T) {
	f := newFakeProm(func(expr string) (string, int) { return emptyVector, 200 })
	defer f.Close()
	NewProm(f.URL).Observe(nil, 24*time.Hour)

	if len(f.seen) == 0 {
		t.Fatal("no queries were sent")
	}
	var summed int
	for _, e := range containerQueries(f.seen) {
		unwrapReduce(t, e)
		if strings.HasPrefix(e, "sum by (namespace,pod,container) (count_over_time(container_memory_working_set_bytes") {
			summed++
		}
	}
	if summed != 1 {
		t.Errorf("want exactly one summed sample-count query, got %d. Incarnations "+
			"partition the window, so maxing the count freezes confidence at the "+
			"longest-lived one instead of totalling the container's coverage", summed)
	}
}

// agentWide reports a query about tare-agent itself rather than about
// containers: whether it is scraped at all, and its per-node self-checks. Those
// are labelled by node, not by namespace or container, and are exempt from the
// container-query rules above for that reason alone.
func agentWide(expr string) bool {
	return expr == agentPresentExpr || strings.Contains(expr, "tare_agent_")
}

func containerQueries(seen []string) []string {
	var out []string
	for _, e := range seen {
		if !agentWide(e) {
			out = append(out, e)
		}
	}
	return out
}

func truncate(s string) string {
	if len(s) > 110 {
		return s[:110] + "..."
	}
	return s
}

// A 14-day request probed 14d twice before stepping down: the default window
// is also the first step of the ladder.
func TestAvailableWindowProbesEachWindowOnce(t *testing.T) {
	f := newFakeProm(func(expr string) (string, int) {
		if strings.Contains(expr, "offset 3d") {
			return vector("app", 1), 200
		}
		return emptyVector, 200
	})
	defer f.Close()

	got, err := NewProm(f.URL).AvailableWindow([]string{"ns"}, 14*24*time.Hour)
	if err != nil || got != 3*24*time.Hour {
		t.Fatalf("AvailableWindow = %v, %v; want 72h", got, err)
	}
	probed := map[string]int{}
	for _, q := range f.seen {
		probed[q]++
		if probed[q] > 1 {
			t.Errorf("probed twice: %s", q)
		}
	}
}
