package main

import (
	"testing"

	"github.com/mfranzon/tare/internal/discover"
	"github.com/mfranzon/tare/internal/metrics"
	"github.com/mfranzon/tare/internal/report"
)

// Three uncovered replicas of one workload are one NO-DATA row, not three, and
// "the rest have other pods covered" must not be said of them.
func TestCoverageSplitsPodsFromRows(t *testing.T) {
	pod := func(workload, name string) discover.Container {
		return discover.Container{Namespace: "ns", Pod: name, Name: "app", Workload: workload}
	}
	containers := []discover.Container{
		pod("Deployment/dark", "dark-1"), pod("Deployment/dark", "dark-2"), pod("Deployment/dark", "dark-3"),
		pod("Deployment/half", "half-1"), pod("Deployment/half", "half-2"),
		pod("Deployment/lit", "lit-1"),
	}
	seen := &metrics.Observation{Samples: 100}
	obs := map[metrics.Key]*metrics.Observation{
		{Namespace: "ns", Pod: "half-1", Container: "app"}: seen,
		{Namespace: "ns", Pod: "lit-1", Container: "app"}:  seen,
		// An entry with nothing in it is not coverage.
		{Namespace: "ns", Pod: "dark-1", Container: "app"}: {},
	}
	note, got := coverageNote(containers, obs)
	want := report.Coverage{Missing: 4, Total: 6, Dark: 3, DarkRows: 1}
	if got != want {
		t.Fatalf("coverage = %+v, want %+v", got, want)
	}
	if split := "3 in 1 NO-DATA row; 1 in rows sized from their other pods"; got.Split() != split {
		t.Errorf("split = %q, want %q", got.Split(), split)
	}
	if note == "" {
		t.Error("partial coverage produced no note")
	}

	if note, got := coverageNote(containers[5:], obs); note != "" || got != (report.Coverage{}) {
		t.Errorf("full coverage reported a gap: %q %+v", note, got)
	}
}
