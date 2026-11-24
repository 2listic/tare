package metrics

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func vmVector(labels map[string]string, value float64) string {
	b, _ := json.Marshal(map[string]any{
		"status": "success",
		"data": map[string]any{
			"resultType": "vector",
			"result": []any{map[string]any{
				"metric": labels,
				"value":  []any{1700000000, formatFloat(value)},
			}},
		},
	})
	return string(b)
}

var dbVMI = map[string]string{"namespace": "vms", "name": "db"}

// Every series here is keyed by the VMI, not by a container, and the join
// must not carry the container matchers that would exclude all of them.
func TestVMQueriesAreScopedByNamespaceOnly(t *testing.T) {
	for _, q := range vmQueries([]string{"vms"}, 7*24*time.Hour) {
		if strings.Contains(q.expr, "container") {
			t.Errorf("%s carries a container matcher: %s", q.signal, q.expr)
		}
		if !strings.Contains(q.expr, `namespace=~"vms"`) {
			t.Errorf("%s is not scoped to the namespace: %s", q.signal, q.expr)
		}
		if strings.Contains(q.expr, "kubevirt_vmi_vcpu_seconds_total") && !strings.Contains(q.expr, vmRunning) {
			t.Errorf("%s reads vCPU time in every state, not only running: %s", q.signal, q.expr)
		}
		if strings.Contains(q.expr, "vcpu_wait_seconds") {
			t.Errorf("%s reads vCPU I/O wait, which is not CPU saturation: %s", q.signal, q.expr)
		}
	}
	// Cluster-wide, the selector must not leave a dangling comma.
	for _, q := range vmQueries(nil, 24*time.Hour) {
		if strings.Contains(q.expr, "{,") || strings.Contains(q.expr, ",}") {
			t.Errorf("%s has a malformed selector: %s", q.signal, q.expr)
		}
	}
}

func TestObserveVMsJoinsByVMI(t *testing.T) {
	f := newFakeProm(func(expr string) (string, int) {
		switch {
		case strings.Contains(expr, "migration"):
			// Migration series name the VMI as vmi on some releases.
			return vmVector(map[string]string{"namespace": "vms", "vmi": "db"}, 1700000000), 200
		case strings.Contains(expr, "vcpu_delay"):
			return vmVector(dbVMI, 0.07), 200
		case strings.Contains(expr, "usable_bytes{") && strings.Contains(expr, "count_over_time"):
			return vmVector(dbVMI, 0.5), 200
		}
		return vmVector(dbVMI, 2), 200
	})
	defer f.Close()
	obs, err := NewProm(f.URL).ObserveVMs([]string{"vms"}, 7*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	o := obs[VMKey{Namespace: "vms", Name: "db"}]
	if o == nil || len(obs) != 1 {
		t.Fatalf("want one VMI, got %v", obs)
	}
	for _, s := range VMSignals {
		if !o.Has(s) {
			t.Errorf("%s not marked", s)
		}
	}
	if o.VCPUDelayAvg != 0.07 || o.LastMigrationStart != 1700000000 || o.GuestMemCoverage != 0.5 || !o.NonEvictable {
		t.Errorf("values misassigned: %+v", o)
	}
}

// A guest without the balloon driver or the guest agent reports nothing from
// inside. That must be a gap, never a guest using zero bytes.
func TestAbsentGuestMetricsAreGapsNotZeros(t *testing.T) {
	f := newFakeProm(func(expr string) (string, int) {
		for _, guest := range []string{"usable_bytes", "available_bytes", "actual_balloon", "swap_", "pgmajfault", "guest_load"} {
			if strings.Contains(expr, guest) {
				return emptyVector, 200
			}
		}
		return vmVector(dbVMI, 1), 200
	})
	defer f.Close()
	p := NewProm(f.URL)
	obs, _ := p.ObserveVMs(nil, 24*time.Hour)
	o := obs[VMKey{Namespace: "vms", Name: "db"}]
	if o == nil {
		t.Fatal("host-side series alone must still produce an observation")
	}
	for _, s := range VMSignals {
		if GuestReported(s) == o.Has(s) {
			t.Errorf("%s: guest-reported=%v, marked=%v", s, GuestReported(s), o.Has(s))
		}
	}
	var gaps int
	for _, h := range p.Health() {
		if GuestReported(h.Signal) && !h.OK() {
			gaps++
		}
	}
	if gaps == 0 {
		t.Error("absent guest metrics left no trace in the signal health")
	}
}

// A failed query is recorded and the others still run: none of these is
// required, and one broken expression must not cost the rest.
func TestVMQueryFailureIsNotFatal(t *testing.T) {
	f := newFakeProm(func(expr string) (string, int) {
		if strings.Contains(expr, "vcpu_delay") {
			return `{"status":"error","error":"boom"}`, 400
		}
		return vmVector(dbVMI, 1), 200
	})
	defer f.Close()
	p := NewProm(f.URL)
	obs, err := p.ObserveVMs(nil, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	o := obs[VMKey{Namespace: "vms", Name: "db"}]
	if o.Has(SigVMVCPUDelay) || !o.Has(SigVMVCPU) {
		t.Errorf("delay marked=%v, vcpu marked=%v", o.Has(SigVMVCPUDelay), o.Has(SigVMVCPU))
	}
}

func TestPreflightVMsCountsSeries(t *testing.T) {
	f := newFakeProm(func(expr string) (string, int) {
		if strings.Contains(expr, "guest_load") {
			return emptyVector, 200
		}
		return vmVector(map[string]string{}, 3), 200
	})
	defer f.Close()
	for _, c := range NewProm(f.URL).PreflightVMs([]string{"vms"}) {
		if strings.Contains(c.Metric, "guest_load") != (c.Series == 0) {
			t.Errorf("%s: %d series", c.Metric, c.Series)
		}
	}
}
