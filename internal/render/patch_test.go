package render

import (
	"strings"
	"testing"

	"github.com/2listic/tare/internal/analyze"
	"github.com/2listic/tare/internal/report"
)

// A Job's spec.template is immutable, so a patch against it is rejected by the
// API server with "field is immutable". A finding the report disclaimed must
// not come back as an apply-ready command one screen later. IDLE is in the
// second group: its own row says IDLE_NOT_RULED_OUT, and its action is
// deletion, not a number. -o patch and diff --write used to keep separate
// copies of this rule and disagreed about IDLE, so both now go through
// analyze.Patchable and are pinned here together.
func TestPatchAndDiffAgreeOnWhatIsPatchable(t *testing.T) {
	for _, tc := range []struct {
		kind    string
		verdict analyze.Verdict
		want    bool
	}{
		{"Deployment", analyze.Waste, true},
		{"StatefulSet", analyze.Saturated, true},
		{"DaemonSet", analyze.Healthy, true},
		{"Job", analyze.Waste, false},
		{"CronJob", analyze.Waste, false},
		{"Pod", analyze.Waste, false},
		{"Deployment", analyze.Idle, false},
		{"Deployment", analyze.Unverified, false},
		{"Deployment", analyze.NoData, false},
		{"Deployment", analyze.Conflicted, false},
		{"Deployment", analyze.Blocked, false},
		{"Deployment", analyze.Leaking, false},
	} {
		f := analyze.Finding{
			Namespace: "prod", Workload: tc.kind + "/web", Container: "app", Verdict: tc.verdict,
			RecCPU: 100, RecCPULimit: 200, RecMem: 64 << 20, RecMemLimit: 80 << 20,
		}
		if ok, _ := analyze.Patchable(f); ok != tc.want {
			t.Errorf("Patchable(%s %s) = %v, want %v", tc.kind, tc.verdict, ok, tc.want)
		}
		out := capture(t, func(p *printer) { p.printPatches([]analyze.Finding{f}, "", "") })
		if got := strings.Contains(out, "kubectl -n 'prod' patch"); got != tc.want {
			t.Errorf("-o patch for %s %s emitted a command = %v, want %v:\n%s", tc.kind, tc.verdict, got, tc.want, out)
		}
	}
}

// A field tare has no number for must be left out of the patch, not written as
// zero. "cpu":"0" in a strategic merge patch strips the container's CPU
// request; the table rendered the same finding as withheld.
func TestPatchOmitsFieldsWithNoRecommendation(t *testing.T) {
	for _, tc := range []struct {
		name string
		f    analyze.Finding
	}{
		{"cpu withheld", analyze.Finding{RecMem: 300 << 20, RecMemLimit: 400 << 20}},
		{"no cpu limit", analyze.Finding{RecCPU: 100, RecMem: 300 << 20, RecMemLimit: 400 << 20}},
		{"no memory limit", analyze.Finding{RecCPU: 100, RecCPULimit: 200, RecMem: 300 << 20}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := tc.f
			f.Namespace, f.Workload, f.Container, f.Verdict = "prod", "Deployment/web", "app", analyze.Waste
			out := capture(t, func(p *printer) { p.printPatches([]analyze.Finding{f}, "", "") })
			if !strings.Contains(out, "kubectl -n 'prod' patch") {
				t.Fatalf("no patch emitted:\n%s", out)
			}
			if strings.Contains(out, `":"0"`) {
				t.Errorf("patch writes a zero quantity for a field with no recommendation:\n%s", out)
			}
		})
	}
}

// A long workload name must not wrap the row. The tail is what distinguishes
// prometheus-kps-...-prometheus from prometheus-kps-...-operator, so it must
// survive truncation.
func TestShortWorkloadKeepsTheTail(t *testing.T) {
	got := shortWorkload("monitoring", "StatefulSet/prometheus-kps-kube-prometheus-stack-prometheus", 24)
	if len([]rune(got)) > 24 {
		t.Errorf("shortWorkload = %q, %d runes, want <= 24", got, len([]rune(got)))
	}
	if !strings.HasSuffix(got, "prometheus") {
		t.Errorf("shortWorkload = %q, want the distinguishing tail kept", got)
	}
	if full := shortWorkload("ns", "Deployment/api", 40); full != "ns/deploy/api" {
		t.Errorf("shortWorkload = %q, want ns/deploy/api when it fits", full)
	}
}

// A memory tare does not know must not come back as a value to apply. The
// patch used to write the ratchet's numbers, which for an OOMKilled container
// was a 25% limit bump nothing measured.
func TestPatchLeavesCensoredMemoryAlone(t *testing.T) {
	fs := []analyze.Finding{{
		Namespace: "prod", Workload: "Deployment/checkout", Container: "api", Verdict: analyze.Saturated,
		Pods: 1, CPURequest: 50, CPULimit: 100, MemRequest: 16 << 20, MemLimit: 16 << 20,
		RecCPU: 115, RecCPULimit: 125, RecMem: 16 << 20, RecMemLimit: 16 << 20, MemCensored: true,
	}}
	out := capture(t, func(p *printer) { p.printPatches(fs, "", "") })
	if strings.Contains(out, "memory") {
		t.Errorf("the patch names memory for a container whose memory tare does not know:\n%s", out)
	}
	if !strings.Contains(out, `"cpu":"115m"`) {
		t.Errorf("the CPU change was lost with the memory:\n%s", out)
	}
	if s := report.Totals(fs); s.MemUnknown != 1 {
		t.Errorf("memUnknown = %d, want 1: the total must say a container's memory is unknown", s.MemUnknown)
	}
}

// TestPatchIsPinnedToItsCluster: a script generated against prod and run after
// kubectl was switched to staging patched staging, silently, because the
// workload names matched. Every command must name the context it was measured
// on, quoted, since a context name is free text.
func TestPatchIsPinnedToItsCluster(t *testing.T) {
	f := analyze.Finding{
		Namespace: "prod", Workload: "Deployment/web", Container: "app", Verdict: analyze.Waste,
		RecCPU: 100, RecCPULimit: 200, RecMem: 64 << 20, RecMemLimit: 80 << 20,
	}
	out := capture(t, func(p *printer) { p.printPatches([]analyze.Finding{f}, "arn:aws:eks:prod's", "/k/config") })
	want := `kubectl --context 'arn:aws:eks:prod'\''s' --kubeconfig '/k/config' -n 'prod' patch deployment 'web'`
	if !strings.Contains(out, want) {
		t.Errorf("command not pinned to its context; want %q in:\n%s", want, out)
	}
	unpinned := capture(t, func(p *printer) { p.printPatches([]analyze.Finding{f}, "", "") })
	if !strings.Contains(unpinned, "whatever kubectl points at") {
		t.Errorf("an unpinned script must say so:\n%s", unpinned)
	}
}
