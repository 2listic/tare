package metrics

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// instanceVector is a node-exporter style result: labelled by instance, with no
// node name anywhere. Verified against a live cluster - node-exporter carries
// `instance: "172.18.0.3:9100"` and nothing that identifies the Kubernetes
// node, which is why the join has to happen outside this package.
func instanceVector(pairs map[string]float64) string {
	var result []any
	for inst, v := range pairs {
		result = append(result, map[string]any{
			"metric": map[string]string{"instance": inst},
			"value":  []any{1700000000, formatFloat(v)},
		})
	}
	b, _ := json.Marshal(map[string]any{
		"status": "success",
		"data":   map[string]any{"resultType": "vector", "result": result},
	})
	return string(b)
}

func TestObserveNodesStripsThePort(t *testing.T) {
	f := newFakeProm(func(expr string) (string, int) {
		switch {
		case strings.HasPrefix(expr, "quantile_over_time"):
			return instanceVector(map[string]float64{"172.18.0.3:9100": 0.078}), 200
		case strings.HasPrefix(expr, "avg_over_time"):
			return instanceVector(map[string]float64{"172.18.0.3:9100": 0.056}), 200
		}
		return instanceVector(map[string]float64{"172.18.0.3:9100": 500}), 200
	})
	defer f.Close()

	got, err := New(NewHTTPTransport(f.URL)).ObserveNodes([]string{"172.18.0.3"}, 4*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	o, ok := got["172.18.0.3"]
	if !ok {
		t.Fatalf("result must be keyed by bare IP, got keys %v", keysOf(got))
	}
	if o.CPUUtilP95 != 0.078 || o.CPUUtilMean != 0.056 || o.Samples != 500 {
		t.Errorf("got %+v", o)
	}
}

// A node the caller did not ask about must not appear. Prometheus may scrape
// node-exporters that are not Kubernetes nodes at all.
func TestObserveNodesIgnoresUnrequestedTargets(t *testing.T) {
	f := newFakeProm(func(expr string) (string, int) {
		return instanceVector(map[string]float64{
			"172.18.0.3:9100": 0.1,
			"10.9.9.9:9100":   0.9,
		}), 200
	})
	defer f.Close()

	got, _ := New(NewHTTPTransport(f.URL)).ObserveNodes([]string{"172.18.0.3"}, time.Hour)
	if _, ok := got["10.9.9.9"]; ok {
		t.Error("a target that is not a node in this cluster must not be reported as one")
	}
}

// No node-exporter is the common case, not an error. Returning an error here
// would take down a report whose other findings are unaffected.
func TestObserveNodesEmptyIsNotAnError(t *testing.T) {
	f := newFakeProm(func(string) (string, int) { return emptyVector, 200 })
	defer f.Close()

	got, err := New(NewHTTPTransport(f.URL)).ObserveNodes([]string{"172.18.0.3"}, time.Hour)
	if err != nil {
		t.Fatalf("a cluster without node-exporter is not an error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d entries, want none", len(got))
	}
}

// An absent signal has to be visible. Silence here would leave the report
// claiming nothing was missing while a whole class of finding was withheld.
func TestObserveNodesRecordsSignalHealth(t *testing.T) {
	f := newFakeProm(func(string) (string, int) { return emptyVector, 200 })
	defer f.Close()

	p := New(NewHTTPTransport(f.URL))
	p.ObserveNodes([]string{"172.18.0.3"}, time.Hour)
	var found bool
	for _, h := range p.Health() {
		if h.Signal == SigNodeUtil {
			found = true
			if h.OK() {
				t.Error("an empty result must not read as a healthy signal")
			}
		}
	}
	if !found {
		t.Error("node utilisation must appear in signal health")
	}
}

func TestObserveNodesWithNoNodesSkipsTheQuery(t *testing.T) {
	f := newFakeProm(func(string) (string, int) { return emptyVector, 200 })
	defer f.Close()

	p := New(NewHTTPTransport(f.URL))
	if _, err := p.ObserveNodes(nil, time.Hour); err != nil {
		t.Fatal(err)
	}
	if len(f.seen) != 0 {
		t.Errorf("no nodes means nothing to ask about, but %d queries were sent", len(f.seen))
	}
}

func TestIPOf(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"172.18.0.3:9100", "172.18.0.3", true},
		{"172.18.0.3:19100", "172.18.0.3", true}, // the port is not assumed to be 9100
		{"172.18.0.3", "172.18.0.3", true},       // no port at all
		{"[fd00::1]:9100", "fd00::1", true},      // IPv6 targets are bracketed
		{"", "", false},
	}
	for _, tc := range cases {
		got, ok := ipOf(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("ipOf(%q) = %q,%v want %q,%v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func keysOf(m map[string]NodeObservation) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
