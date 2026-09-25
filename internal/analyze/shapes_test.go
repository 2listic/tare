package analyze_test

import (
	"strings"
	"testing"
	"time"

	"github.com/mfranzon/tare/internal/analyze"
	"github.com/mfranzon/tare/internal/simulate"
)

// shapeFindings runs the whole catalogue and returns every finding by
// container name, so a rule can be asserted against the declaration shapes
// rather than against one hand-built fixture.
func shapeFindings(t *testing.T) map[string]analyze.Finding {
	t.Helper()
	c := simulate.NewCluster("sim", origin, 21, simulate.Catalog()...)
	for _, d := range simulate.Catalog() {
		c.SetAllocation(d.Name, simulate.Allocation{
			CPURequest: 2, CPULimit: 4, MemRequest: 4 * gib, MemLimit: 8 * gib,
		})
	}
	c.Run(window)
	obs, err := c.Observe(nil, window)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]analyze.Finding{}
	for _, f := range analyze.Run(c.Containers(), obs, window, analyze.DefaultThresholds()) {
		out[f.Container] = f
	}
	return out
}

// TestNoRequestsIsNeverToldToShrink is the live cluster's finding, kept.
//
// IDLE shipped firing on node-exporter, kube-state-metrics and two Grafana
// sidecars - five containers with no resource requests at all - and told the
// operator the saving was to delete their metrics pipeline. A container with
// no reservation has nothing to be idle against and nothing to reclaim, and
// every simulated container had requests by construction, so neither the
// simulator nor 88 snapshots of cluster history could see it.
func TestNoRequestsIsNeverToldToShrink(t *testing.T) {
	fs := shapeFindings(t)
	for _, name := range []string{"node-agent", "config-reloader"} {
		f, ok := fs[name]
		if !ok {
			t.Fatalf("%s produced no finding", name)
		}
		if f.CPURequest != 0 || f.MemRequest != 0 {
			t.Fatalf("precondition: %s was rendered with requests", name)
		}
		t.Logf("%-16s verdict=%-10s recCPU=%dm recMem=%.0fMiB reasons=%d",
			name, f.Verdict, f.RecCPU, float64(f.RecMem)/mib, len(f.Reasons))

		if f.Verdict == analyze.Idle {
			t.Errorf("%s has no reservation at all and was reported IDLE: there is nothing to reclaim, "+
				"and the advice reads as delete it", name)
		}
		// Reclaiming from nothing is the same error stated as a number.
		if c, m := f.CPURequest-f.RecCPU, f.MemRequest-f.RecMem; c > 0 || m > 0 {
			t.Errorf("%s claims a saving of %dm / %d bytes against requests of zero", name, c, m)
		}
	}
}

// TestBestEffortIsToldItIsBestEffort. A pod with no requests is first to be
// evicted and can be OOMKilled while the node still has memory it was never
// promised. That is the finding for these containers, and it is worth more
// than any number.
func TestBestEffortIsToldItIsBestEffort(t *testing.T) {
	f := shapeFindings(t)["node-agent"]
	var said bool
	for _, r := range analyze.ReasonTexts(f.Reasons) {
		if strings.Contains(r, "BestEffort") || strings.Contains(r, "no requests") {
			said = true
		}
	}
	if !said {
		t.Errorf("node-agent is BestEffort and the report does not say so: %v", f.Reasons)
	}
}

// TestGuaranteedIsNotSilentlyDemoted. Request equal to limit buys the strongest
// eviction guarantee in Kubernetes. Moving one field without the other takes it
// away, and a right-sizer that does so has charged the workload a downgrade
// nobody asked for and called it a saving.
func TestGuaranteedIsNotSilentlyDemoted(t *testing.T) {
	f := shapeFindings(t)["guaranteed-cache"]
	if f.QoS != "Guaranteed" {
		t.Fatalf("precondition: rendered as %s", f.QoS)
	}
	t.Logf("guaranteed-cache verdict=%s rec cpu=%dm/%dm mem=%.0f/%.0fMiB",
		f.Verdict, f.RecCPU, f.RecCPULimit, float64(f.RecMem)/mib, float64(f.RecMemLimit)/mib)

	if f.RecCPU == 0 && f.RecMem == 0 {
		return // withheld entirely, which is a safe answer
	}
	if f.RecCPU != f.RecCPULimit || f.RecMem != f.RecMemLimit {
		t.Errorf("Guaranteed pod would be demoted to Burstable: cpu %dm/%dm, mem %d/%d",
			f.RecCPU, f.RecCPULimit, f.RecMem, f.RecMemLimit)
	}
}

// TestNoLimitsHasNoThrottleSignal. cAdvisor exports CFS throttle counters only
// for containers with a CPU quota, so this shape - the one most charts use -
// leans entirely on PSI. The report has to say which signals it had.
func TestNoLimitsHasNoThrottleSignal(t *testing.T) {
	f := shapeFindings(t)["no-limits-api"]
	if f.CPULimit != 0 {
		t.Fatalf("precondition: rendered with a CPU limit of %dm", f.CPULimit)
	}
	t.Logf("no-limits-api verdict=%s conf=%s recCPULimit=%dm", f.Verdict, f.Confidence, f.RecCPULimit)
}

// TestEveryShapeProducesAFinding. The cheapest failure here is a shape that
// falls out of the report entirely: the totals still add up and nothing looks
// wrong.
func TestEveryShapeProducesAFinding(t *testing.T) {
	fs := shapeFindings(t)
	for _, name := range []string{"node-agent", "metrics-app", "config-reloader", "no-limits-api", "guaranteed-cache"} {
		f, ok := fs[name]
		if !ok {
			t.Errorf("%s produced no finding at all", name)
			continue
		}
		if f.Verdict == "" {
			t.Errorf("%s produced a finding with no verdict", name)
		}
	}
}

// TestSidecarAndAppAreSeparateFindings. Two containers in one pod are two
// different workloads with two different answers, and folding them would size
// the app against the sidecar's usage or the reverse.
func TestSidecarAndAppAreSeparateFindings(t *testing.T) {
	fs := shapeFindings(t)
	app, side := fs["metrics-app"], fs["config-reloader"]
	if app.Container == side.Container {
		t.Fatal("the sidecar and its app produced one finding")
	}
	if app.MemUsageMax == side.MemUsageMax {
		t.Errorf("the sidecar and its app share a memory measurement: %v", app.MemUsageMax)
	}
	t.Logf("app mem=%.0fMiB rec=%.0fMiB | sidecar mem=%.0fMiB rec=%.0fMiB",
		app.MemUsageMax/mib, float64(app.RecMem)/mib,
		side.MemUsageMax/mib, float64(side.RecMem)/mib)
}

var _ = time.Second

// TestAddingALimitIsDisclosed is the defect this population was built to find.
//
// no-limits-api declares requests and no limits, which is what most charts do
// and what most style guides recommend. tare proposes `unset -> 3156m` for the
// CPU limit and says nothing: a CPU limit throttles at quota even on an idle
// node, so this introduces stalls into a workload that deliberately had none.
// The memory case is worse in kind - a memory limit makes the container
// OOMKillable at that cap rather than evicted under node pressure.
//
// The number itself is useful and is kept. What was missing is that applying it
// is a behavioural change rather than a resize. The Guaranteed case two rows
// below in the same report already gets this treatment, which is what made the
// omission visible.
func TestAddingALimitIsDisclosed(t *testing.T) {
	f := shapeFindings(t)["no-limits-api"]
	if f.CPULimit != 0 || f.MemLimit != 0 {
		t.Fatalf("precondition: rendered with limits cpu=%dm mem=%d", f.CPULimit, f.MemLimit)
	}
	if f.RecCPULimit == 0 && f.RecMemLimit == 0 {
		t.Skip("no limit proposed, so there is nothing to disclose")
	}
	t.Logf("proposes cpu limit %dm, mem limit %.0fMiB; reasons=%v",
		f.RecCPULimit, float64(f.RecMemLimit)/mib, f.Reasons)

	var saidCPU, saidMem bool
	for _, r := range analyze.ReasonTexts(f.Reasons) {
		if strings.Contains(r, "throttl") {
			saidCPU = true
		}
		if strings.Contains(r, "OOMKill") || strings.Contains(r, "memory limit where") {
			saidMem = true
		}
	}
	if f.RecCPULimit > 0 && !saidCPU {
		t.Error("proposes a CPU limit where the manifest set none, without saying it introduces throttling")
	}
	if f.RecMemLimit > 0 && !saidMem {
		t.Error("proposes a memory limit where the manifest set none, without saying it makes the container OOMKillable")
	}
}

// TestAuthoredLimitIsNotDisclosedAsNew is the negative control. A workload that
// already has limits must not collect the caveat, or it appears on nearly every
// row and stops meaning anything.
func TestAuthoredLimitIsNotDisclosedAsNew(t *testing.T) {
	f := shapeFindings(t)["steady-web"]
	if f.CPULimit == 0 {
		t.Fatal("precondition: steady-web has no CPU limit")
	}
	for _, r := range analyze.ReasonTexts(f.Reasons) {
		if strings.Contains(r, "introduces CPU throttling") {
			t.Errorf("a workload that already had a limit was told its limit is new: %s", r)
		}
	}
}
