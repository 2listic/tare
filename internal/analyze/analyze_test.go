package analyze_test

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mfranzon/tare/internal/analyze"
	"github.com/mfranzon/tare/internal/ceilings"
	"github.com/mfranzon/tare/internal/discover"
	"github.com/mfranzon/tare/internal/metrics"
	"github.com/mfranzon/tare/internal/simulate"
)

var origin = time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC) // a Monday

const (
	mib    = 1 << 20
	gib    = 1 << 30
	window = 14 * 24 * time.Hour
)

// observe runs one archetype under a generous allocation, which is how a
// container arrives at the tool: over-provisioned, so its true demand is
// visible.
func observe(t *testing.T, d simulate.Demand, seed int64) analyze.Finding {
	t.Helper()
	c := simulate.NewCluster("sim", origin, seed, d)
	c.SetAllocation(d.Name, simulate.Allocation{
		CPURequest: 2, CPULimit: 4,
		MemRequest: 4 * gib, MemLimit: 8 * gib,
	})
	c.Run(window)

	obs, err := c.Observe(nil, window)
	if err != nil {
		t.Fatal(err)
	}
	// A demand may render more than one container: a sidecar shares the pod.
	// Pick the finding for the demand's own container rather than assuming the
	// workload has exactly one.
	fs := analyze.Run(c.Containers(), obs, window, analyze.DefaultThresholds())
	for _, f := range fs {
		if f.Container == d.Name {
			return f
		}
	}
	t.Fatalf("no finding for %s among %d", d.Name, len(fs))
	return analyze.Finding{}
}

// TestRecommendationsAreSurvivable is the safety claim in executable form:
// apply the tool's own recommendation as a hard limit, replay the same two
// weeks, and require that the workload is not OOMKilled and is not severely
// starved.
func TestRecommendationsAreSurvivable(t *testing.T) {
	for _, d := range simulate.Catalog() {
		t.Run(d.Name, func(t *testing.T) {
			f := observe(t, d, 21)
			// Withholding a recommendation is a valid, and sometimes the only
			// correct, outcome.
			if f.Verdict == analyze.Blocked || f.Verdict == analyze.Leaking {
				t.Skipf("recommendation correctly withheld (%s): %v", f.Verdict, f.Reasons)
			}
			if f.RecMem == 0 {
				t.Fatalf("no memory recommendation offered for verdict %s", f.Verdict)
			}

			// Replay under the recommendation, treated as a hard limit.
			replay := simulate.New(d, simulate.Allocation{
				CPURequest: float64(f.RecCPU) / 1000,
				CPULimit:   float64(f.RecCPULimit) / 1000,
				MemRequest: f.RecMem,
				MemLimit:   f.RecMem,
			}, origin, 21)
			replay.Run(window)
			after, err := (&simulate.Cluster{
				Namespace: "sim",
				Envs:      map[string]*simulate.Env{d.Name: replay},
			}).Observe(nil, window)
			if err != nil {
				t.Fatal(err)
			}
			var oom, psi float64
			for _, o := range after {
				oom, psi = o.OOMEvents, o.PSIWaitingMax
			}

			if oom > 0 {
				t.Errorf("recommendation of %s memory caused %v OOMKills", human(f.RecMem), oom)
			}
			t.Logf("verdict=%s cpu req=%dm limit=%dm mem=%s -> replay psi=%.3f oom=%v",
				f.Verdict, f.RecCPU, f.RecCPULimit, human(f.RecMem), psi, oom)
			if psi > 0.20 {
				t.Errorf("recommendation of %dm CPU left the workload severely starved (psi %.3f)", f.RecCPU, psi)
			}
		})
	}
}

// TestCeilingRuleRaisesTheRecommendation is the differentiator, asserted
// against ground truth rather than a hard-coded number: a JVM whose heap is
// capped uses far less than it needs allocated, and usage alone would size it
// into an OOMKill.
func TestCeilingRuleRaisesTheRecommendation(t *testing.T) {
	f := observe(t, simulate.CeilingJVM, 33)

	usageOnly := int64(f.MemUsageMax * 1.15)
	if f.RecMem <= usageOnly {
		t.Fatalf("recommendation %s did not exceed the usage-only figure %s; the ceiling rule did not fire",
			human(f.RecMem), human(usageOnly))
	}
	if f.RecMem <= simulate.CeilingJVM.MemCeiling {
		t.Errorf("recommendation %s is not above the %s heap ceiling",
			human(f.RecMem), human(simulate.CeilingJVM.MemCeiling))
	}
	t.Logf("usage-only would say %s; tare says %s (heap ceiling %s)",
		human(usageOnly), human(f.RecMem), human(simulate.CeilingJVM.MemCeiling))
}

// TestStarvedWorkloadIsNeverShrunk guards the asymmetry: a container already
// under pressure must only ever be told to grow.
func TestStarvedWorkloadIsNeverShrunk(t *testing.T) {
	d := simulate.SteadyWeb
	peak := simulate.PeakCPU(d, origin, window, 44)

	c := simulate.NewCluster("sim", origin, 44, d)
	starved := simulate.Allocation{
		CPURequest: peak * 0.4, CPULimit: peak * 0.4,
		MemRequest: 1 * gib, MemLimit: 1 * gib,
	}
	c.SetAllocation(d.Name, starved)
	c.Run(window)

	obs, _ := c.Observe(nil, window)
	f := analyze.Run(c.Containers(), obs, window, analyze.DefaultThresholds())[0]

	if f.Verdict != analyze.Saturated {
		t.Fatalf("verdict = %s, want SATURATED (psi %.3f)", f.Verdict, f.PSIWaiting)
	}
	if f.RecCPU < int64(starved.CPURequest*1000) {
		t.Errorf("a starved container was told to shrink: %dm -> %dm",
			int64(starved.CPURequest*1000), f.RecCPU)
	}
}

// TestConfidenceGrowsWithEvidence: most clusters retain about 15 days, and
// many containers are far younger, so the rating has to move.
func TestConfidenceGrowsWithEvidence(t *testing.T) {
	c := simulate.NewCluster("sim", origin, 55, simulate.DailyWeb)
	c.SetAllocation(simulate.DailyWeb.Name, simulate.Allocation{CPULimit: 4, MemLimit: 8 * gib})
	c.Run(20 * 24 * time.Hour)

	want := map[time.Duration]analyze.Confidence{
		30 * time.Minute:    analyze.Low,
		12 * time.Hour:      analyze.Medium,
		14 * 24 * time.Hour: analyze.High,
	}
	for w, expect := range want {
		obs, _ := c.Observe(nil, w)
		f := analyze.Run(c.Containers(), obs, w, analyze.DefaultThresholds())[0]
		if f.Confidence != expect {
			t.Errorf("window %s: confidence = %s (%d samples), want %s",
				w, f.Confidence, f.Samples, expect)
		}
	}
}

func human(b int64) string {
	switch {
	case b >= gib:
		return strconv.FormatFloat(float64(b)/gib, 'f', 1, 64) + "Gi"
	case b >= mib:
		return strconv.FormatFloat(float64(b)/mib, 'f', 0, 64) + "Mi"
	}
	return strconv.FormatInt(b, 10) + "B"
}

// TestLeakIsReportedNotSized: a leaking workload must be named as a leak, and
// must not be handed a memory number that implies sizing can fix it.
func TestLeakIsReportedNotSized(t *testing.T) {
	f := observe(t, simulate.LeakyService, 77)
	if f.Verdict != analyze.Leaking {
		t.Fatalf("verdict = %s, want LEAKING (early %s late %s)",
			f.Verdict, human(int64(f.MemEarly)), human(int64(f.MemLate)))
	}
	if f.RecMem != 0 {
		t.Errorf("a leaking container was given a memory recommendation of %s", human(f.RecMem))
	}
}

// TestBurstyWorkloadGetsHeadroom: the request may sit at a percentile, but the
// limit must cover the bursts the percentile ignores.
func TestBurstyWorkloadGetsHeadroom(t *testing.T) {
	f := observe(t, simulate.SpikyAPI, 88)
	if f.RecCPULimit <= f.RecCPU {
		t.Fatalf("limit %dm must exceed request %dm for a bursty workload", f.RecCPULimit, f.RecCPU)
	}
	ratio := float64(f.RecCPULimit) / float64(f.RecCPU)
	t.Logf("request %dm, limit %dm, headroom %.1fx (peak %.3f cores, p95 %.3f)",
		f.RecCPU, f.RecCPULimit, ratio, f.CPUUsageMax, f.CPUUsageP95)
	if ratio < 2 {
		t.Errorf("headroom %.1fx is too tight for a workload that spikes 8x", ratio)
	}
}

// TestStartupDrivesTheLimit is the fix for the defect the live cluster showed:
// a Tomcat drawing 19.5x its steady-state CPU while booting was being handed a
// limit derived from idle-hour data.
//
// The test asserts the recommendation covers boot, and then demonstrates the
// counterfactual: a steady-state-only limit throttles the container every time
// it restarts.
func TestStartupDrivesTheLimit(t *testing.T) {
	d := simulate.CeilingJVM // boots at 1.5 cores for 40s, settles at 0.2
	f := observe(t, d, 99)

	if f.CPUStartupMax == 0 {
		t.Fatal("startup usage was not observed at all")
	}
	t.Logf("boot peak %.2f cores, steady peak %.2f cores (%.1fx), limit %dm",
		f.CPUStartupMax, f.CPUSteadyMax, f.CPUStartupMax/f.CPUSteadyMax, f.RecCPULimit)
	if f.CPUSteadyMax >= f.CPUStartupMax {
		t.Errorf("steady peak %.2f should be well below boot peak %.2f; the two "+
			"regimes are not being separated", f.CPUSteadyMax, f.CPUStartupMax)
	}

	if float64(f.RecCPULimit)/1000 < d.StartupCPU {
		t.Errorf("limit %dm does not cover the %.0fm boot draw", f.RecCPULimit, d.StartupCPU*1000)
	}

	// Counterfactual: size the limit to steady state alone and replay. The
	// container restarts daily, so every restart hits the wall.
	steadyOnly := simulate.Demand(d)
	steadyLimit := 0.20 * 1.25 // steady peak plus the burst margin
	throttled := simulate.New(steadyOnly, simulate.Allocation{
		CPURequest: float64(f.RecCPU) / 1000,
		CPULimit:   steadyLimit,
		MemRequest: f.RecMem, MemLimit: f.RecMem,
	}, origin, 99)
	throttled.Run(window)
	obsBad, _ := (&simulate.Cluster{Namespace: "sim",
		Envs: map[string]*simulate.Env{d.Name: throttled}}).Observe(nil, window)

	// And with the recommendation the tool actually makes.
	ok := simulate.New(steadyOnly, simulate.Allocation{
		CPURequest: float64(f.RecCPU) / 1000,
		CPULimit:   float64(f.RecCPULimit) / 1000,
		MemRequest: f.RecMem, MemLimit: f.RecMem,
	}, origin, 99)
	ok.Run(window)
	obsGood, _ := (&simulate.Cluster{Namespace: "sim",
		Envs: map[string]*simulate.Env{d.Name: ok}}).Observe(nil, window)

	var badPSI, goodPSI float64
	for _, o := range obsBad {
		badPSI = o.PSIWaitingMax
	}
	for _, o := range obsGood {
		goodPSI = o.PSIWaitingMax
	}
	t.Logf("steady-state-only limit (%.0fm): psi %.3f | tare's limit (%dm): psi %.3f",
		steadyLimit*1000, badPSI, f.RecCPULimit, goodPSI)

	if badPSI < 0.5 {
		t.Errorf("the counterfactual should have been badly throttled, got psi %.3f; "+
			"if this stops failing the simulator is no longer modelling the defect", badPSI)
	}
	if goodPSI > 0.05 {
		t.Errorf("tare's own limit still throttles boot: psi %.3f", goodPSI)
	}
}

// TestCeilingInferredFromBehaviour covers the case the corpus showed is common
// and that no amount of parsing or model intelligence can solve: the heap cap
// is not in the pod spec, because it lives in the image entrypoint.
//
// Usage alone would size this container into an OOMKill the first time load
// pushed its heap toward the hidden cap.
func TestCeilingInferredFromBehaviour(t *testing.T) {
	d := simulate.HiddenCeilingJVM
	f := observe(t, d, 123)

	var inferred *ceilings.Ceiling
	for i := range f.Ceilings {
		if f.Ceilings[i].Kind == analyze.ObservedCeiling {
			inferred = &f.Ceilings[i]
		}
	}
	if inferred == nil {
		t.Fatalf("no ceiling inferred; parsed ceilings were %v, plateau p95=%s max=%s",
			kindsOf(f.Ceilings), human(int64(f.MemP95)), human(int64(f.MemUsageMax)))
	}
	t.Logf("inferred floor %s from a plateau at %s: %s",
		human(inferred.MinContainerBytes), human(inferred.Bytes), inferred.Note)

	if f.RecMem < inferred.MinContainerBytes {
		t.Errorf("recommendation %s ignored the inferred floor %s",
			human(f.RecMem), human(inferred.MinContainerBytes))
	}
	// The plateau plus overhead must clear the real hidden cap.
	if f.RecMem <= d.MemCeiling {
		t.Errorf("recommendation %s does not clear the hidden %s heap cap",
			human(f.RecMem), human(d.MemCeiling))
	}
}

// TestNoInferenceForOrdinaryWorkloads guards the other side: inferring a
// ceiling for a process that simply uses little memory would pin it to today's
// usage forever.
func TestNoInferenceForOrdinaryWorkloads(t *testing.T) {
	// VariableMemJVM is the one that matters: a JVM image, so the runtime
	// check passes, but memory that genuinely fluctuates rather than sitting
	// at a cap. The others only confirm non-runtime images are left alone.
	for _, d := range []simulate.Demand{
		simulate.VariableMemJVM, simulate.SteadyWeb, simulate.IdleHoarder, simulate.SpikyAPI,
	} {
		t.Run(d.Name, func(t *testing.T) {
			f := observe(t, d, 124)
			for _, c := range f.Ceilings {
				if c.Kind == analyze.ObservedCeiling {
					t.Errorf("inferred a ceiling for a plain workload: %s", c.Note)
				}
			}
		})
	}
}

// TestScalingCeilingIsNotAFloor: a heap defined as a percentage of the limit
// moves when the limit moves, so it must never prevent a reduction.
func TestScalingCeilingIsNotAFloor(t *testing.T) {
	cs := ceilings.Detect(ceilings.Container{
		Image:            "eclipse-temurin:21-jre",
		Args:             []string{"java", "-XX:MaxRAMPercentage=25.0", "-jar", "a.jar"},
		MemoryLimitBytes: 8 * gib,
	})
	floor, blocked := ceilings.MemoryFloor(cs)
	if blocked {
		t.Fatal("a percentage ceiling should not block")
	}
	if floor != 0 {
		t.Errorf("floor = %s, want 0: a heap that follows the limit constrains nothing",
			human(floor))
	}
}

func kindsOf(cs []ceilings.Ceiling) []ceilings.Kind {
	var out []ceilings.Kind
	for _, c := range cs {
		out = append(out, c.Kind)
	}
	return out
}

// TestNoSaturationSignalWithholdsConfidence is the behaviour the signal-health
// work exists for. On a cluster exporting neither PSI nor CFS counters, an idle
// container and a throttled one look identical. Reporting the first as WASTE at
// high confidence would be a guess dressed as a measurement.
func TestNoSaturationSignalWithholdsConfidence(t *testing.T) {
	d := simulate.IdleHoarder
	c := simulate.NewCluster("sim", origin, 202, d)
	c.SetAllocation(d.Name, simulate.Allocation{
		CPURequest: 2, CPULimit: 4, MemRequest: 4 * gib, MemLimit: 8 * gib,
	})
	c.Run(window)

	// With the signals: an ordinary waste finding.
	obs, _ := c.Observe(nil, window)
	full := analyze.Run(c.Containers(), obs, window, analyze.DefaultThresholds())[0]
	if full.Verdict != analyze.Waste {
		t.Fatalf("baseline verdict = %s, want WASTE", full.Verdict)
	}

	// Same data, same workload, but the cluster exports no saturation signal.
	obs2, _ := c.Observe(nil, window)
	blind := analyze.Run(c.Containers(),
		simulate.Without(obs2, metrics.SigPSI, metrics.SigPSIAvg, metrics.SigThrottle),
		window, analyze.DefaultThresholds())[0]

	if blind.Verdict != analyze.Unverified {
		t.Errorf("verdict = %s, want UNVERIFIED when nothing can confirm headroom", blind.Verdict)
	}
	if blind.Confidence != analyze.Low {
		t.Errorf("confidence = %s, want low: sample count cannot substitute for a missing signal",
			blind.Confidence)
	}
	var explained bool
	for _, r := range analyze.ReasonTexts(blind.Reasons) {
		if strings.Contains(r, "no saturation signal") {
			explained = true
		}
	}
	if !explained {
		t.Errorf("the reader must be told why, reasons were: %v", blind.Reasons)
	}
	if len(blind.Missing) == 0 {
		t.Error("missing signals should be recorded on the finding")
	}
}

// TestOOMBlindnessIsStated: reducing memory without OOMKill history is riskier
// than reducing it with a clean history, and the report must say so.
func TestOOMBlindnessIsStated(t *testing.T) {
	d := simulate.SteadyWeb
	c := simulate.NewCluster("sim", origin, 203, d)
	c.SetAllocation(d.Name, simulate.Allocation{
		CPURequest: 2, CPULimit: 4, MemRequest: 4 * gib, MemLimit: 8 * gib,
	})
	c.Run(window)
	obs, _ := c.Observe(nil, window)

	f := analyze.Run(c.Containers(), simulate.Without(obs, metrics.SigOOM),
		window, analyze.DefaultThresholds())[0]

	var warned bool
	for _, r := range analyze.ReasonTexts(f.Reasons) {
		if strings.Contains(r, "OOMKill history is unavailable") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("a memory reduction without OOM history must be flagged; reasons: %v", f.Reasons)
	}
}

// TestGuaranteedPodKeepsItsQoS: a pod with requests equal to limits is
// Guaranteed. Recommending different values silently demotes it to Burstable,
// costing eviction priority, and clusters that support in-place resize reject
// the change outright because QoS may not change.
func TestGuaranteedPodKeepsItsQoS(t *testing.T) {
	d := simulate.SteadyWeb
	c := simulate.NewCluster("sim", origin, 301, d)
	c.SetAllocation(d.Name, simulate.Allocation{
		CPURequest: 2, CPULimit: 2, MemRequest: 4 * gib, MemLimit: 4 * gib,
	})
	c.Run(window)
	obs, _ := c.Observe(nil, window)

	containers := c.Containers()
	for i := range containers {
		containers[i].QoS = "Guaranteed"
	}
	f := analyze.RunWithCluster(containers, obs, analyze.Cluster{Constraints: nil}, window, analyze.DefaultThresholds())[0]

	if f.RecCPU != f.RecCPULimit {
		t.Errorf("CPU request %dm != limit %dm; QoS would change to Burstable", f.RecCPU, f.RecCPULimit)
	}
	if f.RecMem != f.RecMemLimit {
		t.Errorf("memory request %s != limit %s; QoS would change to Burstable",
			human(f.RecMem), human(f.RecMemLimit))
	}
	var explained bool
	for _, r := range analyze.ReasonTexts(f.Reasons) {
		if strings.Contains(r, "Guaranteed") {
			explained = true
		}
	}
	if !explained {
		t.Errorf("the reader must be told why requests and limits are equal: %v", f.Reasons)
	}
}

// TestHPAOnCPUIsWarned: an HPA scaling on CPU utilisation measures against the
// request, so changing the request moves the setpoint. Recommending a cut
// without saying so trades a CPU saving for extra replicas.
func TestHPAOnCPUIsWarned(t *testing.T) {
	d := simulate.SteadyWeb
	c := simulate.NewCluster("sim", origin, 302, d)
	c.SetAllocation(d.Name, simulate.Allocation{
		CPURequest: 2, CPULimit: 4, MemRequest: 4 * gib, MemLimit: 8 * gib,
	})
	c.Run(window)
	obs, _ := c.Observe(nil, window)

	containers := c.Containers()
	cons := map[string]discover.Constraints{
		containers[0].WorkloadKey(): {HPAOnCPU: true, HPAName: "web", HPAMin: 2, HPAMax: 10},
	}
	f := analyze.RunWithCluster(containers, obs, analyze.Cluster{Constraints: cons}, window, analyze.DefaultThresholds())[0]

	var warned bool
	for _, r := range analyze.ReasonTexts(f.Reasons) {
		if strings.Contains(r, "HorizontalPodAutoscaler") && strings.Contains(r, "utilisation") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("an HPA on CPU must be called out; reasons: %v", f.Reasons)
	}
}

// TestVPAConflictWithholdsRecommendation: two controllers writing the same
// field is a fight, and the loser is whichever wrote last.
func TestVPAConflictWithholdsRecommendation(t *testing.T) {
	d := simulate.SteadyWeb
	c := simulate.NewCluster("sim", origin, 303, d)
	c.SetAllocation(d.Name, simulate.Allocation{
		CPURequest: 2, CPULimit: 4, MemRequest: 4 * gib, MemLimit: 8 * gib,
	})
	c.Run(window)
	obs, _ := c.Observe(nil, window)

	containers := c.Containers()
	cons := map[string]discover.Constraints{
		containers[0].WorkloadKey(): {VPAName: "web-vpa", VPAMode: "Auto"},
	}
	f := analyze.RunWithCluster(containers, obs, analyze.Cluster{Constraints: cons}, window, analyze.DefaultThresholds())[0]

	if f.Verdict != analyze.Conflicted {
		t.Errorf("verdict = %s, want CONFLICT", f.Verdict)
	}
	if f.RecCPU != 0 || f.RecMem != 0 {
		t.Errorf("a recommendation was offered despite a VPA in Auto mode: cpu=%dm mem=%s",
			f.RecCPU, human(f.RecMem))
	}
}

// TestVPAOffIsNotAConflict: a VPA in recommendation-only mode writes nothing.
func TestVPAOffIsNotAConflict(t *testing.T) {
	d := simulate.SteadyWeb
	c := simulate.NewCluster("sim", origin, 304, d)
	c.SetAllocation(d.Name, simulate.Allocation{
		CPURequest: 2, CPULimit: 4, MemRequest: 4 * gib, MemLimit: 8 * gib,
	})
	c.Run(window)
	obs, _ := c.Observe(nil, window)
	containers := c.Containers()
	cons := map[string]discover.Constraints{
		containers[0].WorkloadKey(): {VPAName: "web-vpa", VPAMode: "Off"},
	}
	f := analyze.RunWithCluster(containers, obs, analyze.Cluster{Constraints: cons}, window, analyze.DefaultThresholds())[0]
	if f.Verdict == analyze.Conflicted {
		t.Error("a VPA in Off mode sets nothing and must not block a recommendation")
	}
}

// TestCrashLoopIsNotWaste: the lab's slow-leaker filled a pod-scoped tmpfs,
// and every restart after the first OOMKill was then killed inside runc init,
// before the container existed to have an OOM event recorded against it. The
// observation that reached the analyzer had OOMEvents zero, 34 restarts, and
// usage samples from the hours before it died. tare reported WASTE and
// recommended 1.17GiB for a container that had not run in two hours.
//
// An idle-looking container that cannot stay up must never be told to shrink:
// its allocation is a candidate for what is killing it.
func TestCrashLoopIsNotWaste(t *testing.T) {
	d := simulate.SteadyWeb
	c := simulate.NewCluster("sim", origin, 907, d)
	c.SetAllocation(d.Name, simulate.Allocation{
		CPURequest: 2, CPULimit: 4, MemRequest: 4 * gib, MemLimit: 8 * gib,
	})
	c.Run(window)
	obs, _ := c.Observe(nil, window)

	th := analyze.DefaultThresholds()
	base := analyze.Run(c.Containers(), obs, window, th)[0]
	if base.Verdict != analyze.Waste {
		t.Fatalf("precondition: an over-allocated steady web should be WASTE, got %s", base.Verdict)
	}

	// Same container, same usage, but it kept dying. No OOM event, because the
	// kill happened before the container started.
	for _, o := range obs {
		o.Restarts = th.RestartChurn
		o.OOMEvents = 0
	}
	f := analyze.Run(c.Containers(), obs, window, th)[0]

	if f.Verdict == analyze.Waste {
		t.Error("a container restarting throughout the window was reported WASTE")
	}
	if f.Verdict != analyze.Unverified {
		t.Errorf("verdict = %s, want UNVERIFIED", f.Verdict)
	}
	if f.RecMem < f.MemRequest {
		t.Errorf("recommended memory %d is below the current request %d: a crash loop must never produce a reduction",
			f.RecMem, f.MemRequest)
	}
	if f.RecCPU < f.CPURequest {
		t.Errorf("recommended CPU %dm is below the current request %dm", f.RecCPU, f.CPURequest)
	}
	var explained bool
	for _, r := range analyze.ReasonTexts(f.Reasons) {
		if strings.Contains(r, "restarted") && strings.Contains(r, "OOM event") {
			explained = true
		}
	}
	if !explained {
		t.Errorf("the reader must be told why, and that a missing OOM event proves nothing here; reasons: %v", f.Reasons)
	}
}

// TestOneRestartIsNotACrashLoop: the lab's slow-leaker is meant to OOMKill
// about every 40 hours. A single restart inside a multi-day window is that
// design working, not a container that cannot stay up, and suppressing the
// verdict there would hide every genuine oversize behind one unlucky kill.
func TestOneRestartIsNotACrashLoop(t *testing.T) {
	d := simulate.SteadyWeb
	c := simulate.NewCluster("sim", origin, 908, d)
	c.SetAllocation(d.Name, simulate.Allocation{
		CPURequest: 2, CPULimit: 4, MemRequest: 4 * gib, MemLimit: 8 * gib,
	})
	c.Run(window)
	obs, _ := c.Observe(nil, window)
	for _, o := range obs {
		o.Restarts = 1
	}

	f := analyze.Run(c.Containers(), obs, window, analyze.DefaultThresholds())[0]
	if f.Verdict != analyze.Waste {
		t.Errorf("verdict = %s, want WASTE: one restart in a 14d window is not a crash loop", f.Verdict)
	}
}
