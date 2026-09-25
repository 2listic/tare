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
