package report

import (
	"testing"

	"github.com/2listic/tare/internal/analyze"
)

// The savings total must not hide the containers that need more: summing only
// the positive side reported a number optimistic by exactly the amount the
// reader most needed to see.
func TestSavingsReportsBothDirections(t *testing.T) {
	fs := []analyze.Finding{
		{Pods: 1, CPURequest: 200, RecCPU: 100, MemRequest: 1 << 28, RecMem: 1 << 27},
		{Pods: 1, CPURequest: 20, RecCPU: 10, MemRequest: 1 << 28, RecMem: 1 << 29},
	}
	s := Totals(fs)
	if s.MemBytes != 1<<27 {
		t.Errorf("reclaimable memory = %d, want %d", s.MemBytes, 1<<27)
	}
	if s.AddMemBytes != 1<<28 {
		t.Errorf("memory to add back = %d, want %d", s.AddMemBytes, 1<<28)
	}
	if s.AddWorkloads != 1 {
		t.Errorf("containers needing more = %d, want 1", s.AddWorkloads)
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
	s := Totals(fs)
	if s.CPUMillicores != 100 || s.Workloads != 1 {
		t.Errorf("reduction = %dm over %d containers, want 100m over 1: IDLE must not be in it",
			s.CPUMillicores, s.Workloads)
	}
	if s.IdleWorkloads != 1 || s.IdleCPUMillicores != 980 {
		t.Errorf("idle = %dm over %d, want 980m over 1, counted apart so it stays visible",
			s.IdleCPUMillicores, s.IdleWorkloads)
	}
}

// The lab's boutique namespace, minutes old, was all UNVERIFIED and still made
// "requests lowered on 12" in the summary while -o patch wrote none of them.
func TestUnverifiedIsNotSummedIntoTheReduction(t *testing.T) {
	fs := []analyze.Finding{
		{Verdict: analyze.Waste, Pods: 1, CPURequest: 200, RecCPU: 100, MemRequest: 1 << 28, RecMem: 1 << 27},
		{Verdict: analyze.Unverified, Pods: 1, CPURequest: 100, RecCPU: 10, MemRequest: 1 << 26, RecMem: 1 << 24},
	}
	s := Totals(fs)
	if s.CPUMillicores != 100 || s.Workloads != 1 {
		t.Errorf("reduction = %dm over %d containers, want 100m over 1: UNVERIFIED must not be in it",
			s.CPUMillicores, s.Workloads)
	}
	if s.UnverifiedWorkloads != 1 || s.UnverifiedCPUMillicores != 90 {
		t.Errorf("unverified = %dm over %d, want 90m over 1, counted apart so it stays visible",
			s.UnverifiedCPUMillicores, s.UnverifiedWorkloads)
	}
}
