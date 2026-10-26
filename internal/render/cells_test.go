package render

import (
	"testing"

	"github.com/mfranzon/tare/internal/analyze"
	"github.com/mfranzon/tare/internal/metrics"
)

// The pressure column names its source: a reader comparing "psi 0.04" with
// "wait 12%" must not take them for the same scale.
func TestPressureCellNamesTheSource(t *testing.T) {
	wait := metrics.CPUWait{Stretches: 2016, Stalled: 0.12, Normal: 0.031, HasNormal: true}
	have := func(ss ...metrics.Signal) map[metrics.Signal]bool {
		m := map[metrics.Signal]bool{}
		for _, s := range ss {
			m[s] = true
		}
		return m
	}
	cases := []struct {
		name string
		f    analyze.Finding
		want string
	}{
		{"cpu wait", analyze.Finding{CPUWait: wait, Have: have(metrics.SigCPUWait)}, "wait 12%/0.031"},
		{"no normal", analyze.Finding{CPUWait: metrics.CPUWait{Stalled: 0.5}, Have: have(metrics.SigCPUWait)}, "wait 50%/-"},
		{"never stalled", analyze.Finding{Have: have(metrics.SigCPUWait)}, "none"},
		{"psi decides", analyze.Finding{CPUWait: wait, PSIWaitingPeak: 0.002, Have: have(metrics.SigCPUWait, metrics.SigPSI)}, "psi ?/0.002"},
		{"psi calm, wait ignored", analyze.Finding{CPUWait: wait, Have: have(metrics.SigCPUWait, metrics.SigPSI)}, "none"},
		{"nothing", analyze.Finding{}, "unmeasured"},
	}
	for _, c := range cases {
		if got := PressureCell(c.f); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}
