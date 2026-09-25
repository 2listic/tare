package metrics

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// multiVector serves one series per container, all with the same value.
func multiVector(value float64, containers ...string) string {
	var result []any
	for i, c := range containers {
		result = append(result, map[string]any{
			"metric": map[string]string{
				"namespace": "ns",
				"pod":       c + "-0",
				"container": c,
			},
			"value": []any{1700000000, formatFloat(value)},
		})
		_ = i
	}
	b, _ := json.Marshal(map[string]any{
		"status": "success",
		"data":   map[string]any{"resultType": "vector", "result": result},
	})
	return string(b)
}

// psiCluster serves a cluster of busy containers whose CPU usage is cpu cores
// and whose pressure is psi, for both the peak and the sustained query.
func psiCluster(t *testing.T, cpu, psi float64, n int) (map[Key]*Observation, []SignalHealth) {
	t.Helper()
	containers := make([]string, n)
	for i := range containers {
		containers[i] = string(rune('a' + i))
	}
	f := newFakeProm(func(expr string) (string, int) {
		switch {
		case strings.Contains(expr, "container_pressure_cpu_waiting_seconds_total"):
			return multiVector(psi, containers...), 200
		case strings.Contains(expr, "container_cpu_usage_seconds_total"):
			return multiVector(cpu, containers...), 200
		default:
			return multiVector(1, containers...), 200
		}
	})
	defer f.Close()

	p := NewProm(f.URL)
	obs, err := p.Observe([]string{"ns"}, 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return obs, p.Health()
}

func psiHealth(h []SignalHealth) (suspect string) {
	for _, s := range h {
		if s.Signal == SigPSI && s.Suspect != "" {
			return s.Suspect
		}
	}
	return ""
}

// TestDeadPSIIsNotReadAsCalm is kubernetes#136333 in executable form. A kubelet
// on a kernel without PSI support exports the series for every container with
// every value zero. Nothing fails, nothing is absent, and every container looks
// comfortable.
func TestDeadPSIIsNotReadAsCalm(t *testing.T) {
	obs, health := psiCluster(t, 0.5, 0, 5)
	if len(obs) != 5 {
		t.Fatalf("expected 5 observations, got %d", len(obs))
	}
	for _, o := range obs {
		if o.Has(SigPSI) || o.Has(SigPSIAvg) {
			t.Errorf("%s still reports PSI as available at a flat cluster-wide zero", o.Container)
		}
	}
	if psiHealth(health) == "" {
		t.Error("the report header says nothing about the pressure signal being withheld")
	}
}

// TestCalmClusterKeepsPSI is the first negative control: the check must not
// fire on a cluster that is merely quiet. One container registering any
// pressure at all proves the instrument is connected, and the other four being
// at zero is then a fact about the cluster.
func TestCalmClusterKeepsPSI(t *testing.T) {
	containers := []string{"a", "b", "c", "d", "e"}
	f := newFakeProm(func(expr string) (string, int) {
		switch {
		case strings.Contains(expr, "container_pressure_cpu_waiting_seconds_total"):
			// Four at zero, one that once waited.
			var result []any
			for i, c := range containers {
				v := 0.0
				if i == 0 {
					v = 0.0004
				}
				result = append(result, map[string]any{
					"metric": map[string]string{"namespace": "ns", "pod": c + "-0", "container": c},
					"value":  []any{1700000000, formatFloat(v)},
				})
			}
			b, _ := json.Marshal(map[string]any{
				"status": "success",
				"data":   map[string]any{"resultType": "vector", "result": result},
			})
			return string(b), 200
		case strings.Contains(expr, "container_cpu_usage_seconds_total"):
			return multiVector(0.5, containers...), 200
		default:
			return multiVector(1, containers...), 200
		}
	})
	defer f.Close()

	p := NewProm(f.URL)
	obs, err := p.Observe([]string{"ns"}, 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range obs {
		if !o.Has(SigPSI) {
			t.Errorf("%s lost PSI on a cluster where a container did register pressure", o.Container)
		}
	}
	if s := psiHealth(p.Health()); s != "" {
		t.Errorf("pressure marked suspect on a calm cluster: %s", s)
	}
}

// TestIdleClusterKeepsPSI is the second negative control. Containers drawing
// almost no CPU have nothing to wait for, so their zeros are not evidence about
// the instrument and must not be counted as such - otherwise every idle cluster
// loses its saturation signal.
func TestIdleClusterKeepsPSI(t *testing.T) {
	obs, health := psiCluster(t, 0.001, 0, 5)
	for _, o := range obs {
		if !o.Has(SigPSI) {
			t.Errorf("%s lost PSI on an idle cluster, where zero pressure is the honest answer", o.Container)
		}
	}
	if s := psiHealth(health); s != "" {
		t.Errorf("pressure marked suspect on an idle cluster: %s", s)
	}
}

// TestTinyClusterKeepsPSI is the third negative control: a population of two
// busy containers can honestly have no contention, and the control must not
// claim otherwise on a sample that small.
func TestTinyClusterKeepsPSI(t *testing.T) {
	obs, health := psiCluster(t, 0.5, 0, 2)
	for _, o := range obs {
		if !o.Has(SigPSI) {
			t.Errorf("%s lost PSI on a two-container cluster", o.Container)
		}
	}
	if s := psiHealth(health); s != "" {
		t.Errorf("pressure marked suspect on a two-container cluster: %s", s)
	}
}

// TestNearZeroPSISurvives pins that the control tests the digit and not a
// threshold. A cluster genuinely running at 1e-9 pressure is measuring
// something, and inventing a floor here would start discarding real readings.
func TestNearZeroPSISurvives(t *testing.T) {
	obs, health := psiCluster(t, 0.5, 1e-9, 5)
	for _, o := range obs {
		if !o.Has(SigPSI) {
			t.Errorf("%s lost PSI at a real but tiny pressure reading", o.Container)
		}
	}
	if s := psiHealth(health); s != "" {
		t.Errorf("pressure marked suspect at 1e-9: %s", s)
	}
}
