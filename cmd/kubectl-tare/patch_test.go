package main

import (
	"strings"
	"testing"

	"github.com/mfranzon/tare/internal/analyze"
)

// A Job's spec.template is immutable, so a patch against it is rejected by the
// API server with "field is immutable". The live cluster catches this Job on
// its UNVERIFIED verdict first, which would leave the kind guard passing for
// free, so the case is pinned here with a verdict tare does stand behind.
func TestPatchSkipsImmutableKinds(t *testing.T) {
	for _, tc := range []struct {
		kind string
		want bool
	}{
		{"Deployment", true},
		{"StatefulSet", true},
		{"DaemonSet", true},
		{"Job", false},
		{"CronJob", false},
		{"Pod", false},
	} {
		if got := patchableKind(tc.kind); got != tc.want {
			t.Errorf("patchableKind(%q) = %v, want %v", tc.kind, got, tc.want)
		}
	}
}

// A finding the report disclaimed must not come back as an apply-ready command
// one screen later.
func TestPatchSkipsUndefendedVerdicts(t *testing.T) {
	for _, tc := range []struct {
		verdict analyze.Verdict
		want    bool
	}{
		{analyze.Waste, true},
		{analyze.Saturated, true},
		{analyze.Healthy, true},
		{analyze.Unverified, false},
		{analyze.NoData, false},
		{analyze.Conflicted, false},
		{analyze.Blocked, false},
		{analyze.Leaking, false},
	} {
		if got := patchableVerdict(tc.verdict); got != tc.want {
			t.Errorf("patchableVerdict(%q) = %v, want %v", tc.verdict, got, tc.want)
		}
	}
}

// The savings total must not hide the containers that need more: summing only
// the positive side reported a number optimistic by exactly the amount the
// reader most needed to see.
func TestSavingsReportsBothDirections(t *testing.T) {
	fs := []analyze.Finding{
		{Pods: 1, CPURequest: 200, RecCPU: 100, MemRequest: 1 << 28, RecMem: 1 << 27},
		{Pods: 1, CPURequest: 20, RecCPU: 10, MemRequest: 1 << 28, RecMem: 1 << 29},
	}
	s := totalSavings(fs)
	if s.memBytes != 1<<27 {
		t.Errorf("reclaimable memory = %d, want %d", s.memBytes, 1<<27)
	}
	if s.addMemBytes != 1<<28 {
		t.Errorf("memory to add back = %d, want %d", s.addMemBytes, 1<<28)
	}
	if s.addWorkloads != 1 {
		t.Errorf("containers needing more = %d, want 1", s.addWorkloads)
	}
}

// --explain always said IDLE was "not folded into" the reduction; the sum
// folded it in anyway. In the demo that was 1990m of a 20.2-core headline
// resting on a container whose own row says IDLE_NOT_RULED_OUT.
func TestIdleIsNotSummedIntoTheReduction(t *testing.T) {
	fs := []analyze.Finding{
		{Verdict: analyze.Waste, Pods: 1, CPURequest: 200, RecCPU: 100, MemRequest: 1 << 28, RecMem: 1 << 27},
		{Verdict: analyze.Idle, Pods: 2, CPURequest: 500, RecCPU: 10, MemRequest: 1 << 28, RecMem: 1 << 24},
	}
	s := totalSavings(fs)
	if s.cpuMillicores != 100 || s.workloads != 1 {
		t.Errorf("reduction = %dm over %d containers, want 100m over 1: IDLE must not be in it",
			s.cpuMillicores, s.workloads)
	}
	if s.idleWorkloads != 1 || s.idleCPUMillicores != 980 {
		t.Errorf("idle = %dm over %d, want 980m over 1, counted apart so it stays visible",
			s.idleCPUMillicores, s.idleWorkloads)
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
	out := capture(t, func() { printPatches(fs) })
	if strings.Contains(out, "memory") {
		t.Errorf("the patch names memory for a container whose memory tare does not know:\n%s", out)
	}
	if !strings.Contains(out, `"cpu":"115m"`) {
		t.Errorf("the CPU change was lost with the memory:\n%s", out)
	}
	if s := totalSavings(fs); s.memUnknown != 1 {
		t.Errorf("memUnknown = %d, want 1: the total must say a container's memory is unknown", s.memUnknown)
	}
}
