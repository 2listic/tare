package simulate

import (
	"testing"
	"time"

	"github.com/mfranzon/tare/internal/metrics"
)

const gib = 1 << 30

func key() metrics.Key {
	return metrics.Key{Namespace: "sim", Pod: "p-0", Container: "c"}
}

var origin = time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC) // a Monday

func TestDeterminism(t *testing.T) {
	a := New(DailyWeb, Allocation{}, origin, 42)
	b := New(DailyWeb, Allocation{}, origin, 42)
	sa, sb := a.Run(6*time.Hour), b.Run(6*time.Hour)
	if len(sa) != len(sb) {
		t.Fatalf("sample counts differ: %d vs %d", len(sa), len(sb))
	}
	for i := range sa {
		if sa[i].CPUDemand != sb[i].CPUDemand {
			t.Fatalf("tick %d diverged: %v vs %v", i, sa[i].CPUDemand, sb[i].CPUDemand)
		}
	}
}

func TestSpeed(t *testing.T) {
	start := time.Now()
	e := New(DailyWeb, Allocation{CPULimit: 1, MemLimit: 1 * gib}, origin, 1)
	e.Run(30 * 24 * time.Hour)
	elapsed := time.Since(start)
	if elapsed > 5*time.Second {
		t.Fatalf("30 simulated days took %s; the simulator must stay fast enough to sweep", elapsed)
	}
	t.Logf("30 days (%d ticks) in %s", len(e.samples), elapsed)
}

// TestPSIRespondsToStarvation is the property the whole tool depends on: a
// container given less than it wants must show pressure, and one given enough
// must not.
func TestPSIRespondsToStarvation(t *testing.T) {
	peak := PeakCPU(SteadyWeb, origin, 24*time.Hour, 7)

	generous := New(SteadyWeb, Allocation{CPULimit: peak * 2}, origin, 7)
	generous.Run(24 * time.Hour)
	obsG := generous.observe(key(), 24*time.Hour)

	starved := New(SteadyWeb, Allocation{CPULimit: peak * 0.5}, origin, 7)
	starved.Run(24 * time.Hour)
	obsS := starved.observe(key(), 24*time.Hour)

	if obsG.PSIWaitingMax > 0.01 {
		t.Errorf("generous allocation shows pressure %.4f, want near zero", obsG.PSIWaitingMax)
	}
	if obsS.PSIWaitingMax < 0.2 {
		t.Errorf("halved allocation shows pressure %.4f, want clearly starved", obsS.PSIWaitingMax)
	}
	if obsS.CPUUsageMax > peak*0.51 {
		t.Errorf("starved usage %.3f exceeded its limit %.3f", obsS.CPUUsageMax, peak*0.5)
	}
}

// TestMemoryLimitCausesOOM checks the cliff: memory has no graceful
// degradation, so a limit below demand must kill the container.
func TestMemoryLimitCausesOOM(t *testing.T) {
	peak := PeakMem(LeakyService, origin, 24*time.Hour, 3)

	safe := New(LeakyService, Allocation{MemLimit: peak * 2}, origin, 3)
	safe.Run(24 * time.Hour)
	if o := safe.observe(key(), 24*time.Hour); o.OOMEvents != 0 {
		t.Errorf("generous memory limit still OOMKilled %v times", o.OOMEvents)
	}

	tight := New(LeakyService, Allocation{MemLimit: LeakyService.BaseMem + 10*mib}, origin, 3)
	tight.Run(24 * time.Hour)
	if o := tight.observe(key(), 24*time.Hour); o.OOMEvents == 0 {
		t.Error("a leaking workload under a tight limit should have been OOMKilled")
	}
}

// TestCeilingCapsDemand confirms a runtime ceiling makes extra memory useless,
// which is the situation the ceiling rule exists for.
func TestCeilingCapsDemand(t *testing.T) {
	e := New(CeilingJVM, Allocation{MemLimit: 4 * gib}, origin, 5)
	e.Run(48 * time.Hour)
	o := e.observe(key(), 48*time.Hour)
	if int64(o.MemWorkingSet) > CeilingJVM.MemCeiling {
		t.Errorf("working set %d exceeded the %d ceiling", int64(o.MemWorkingSet), CeilingJVM.MemCeiling)
	}
	if int64(o.MemWorkingSet) < CeilingJVM.MemCeiling/2 {
		t.Errorf("working set %d is implausibly far below the ceiling", int64(o.MemWorkingSet))
	}
}

// TestDailyCycleIsVisible guards the reason confidence depends on window
// length: a six hour window sees a different world from a full week.
func TestDailyCycleIsVisible(t *testing.T) {
	e := New(DailyWeb, Allocation{}, origin, 11)
	e.Run(7 * 24 * time.Hour)

	short := e.observe(key(), 6*time.Hour)
	full := e.observe(key(), 7*24*time.Hour)

	if full.CPUUsageMax <= short.CPUUsageMax {
		t.Errorf("a week (%.3f) should see a higher peak than six hours (%.3f)",
			full.CPUUsageMax, short.CPUUsageMax)
	}
	if full.Samples <= short.Samples*10 {
		t.Errorf("week samples %d vs six hour samples %d: sample counts must "+
			"separate clearly or the confidence rating is meaningless",
			full.Samples, short.Samples)
	}
}

func TestDeploysRestartTheContainer(t *testing.T) {
	e := New(ChurningDeploy, Allocation{MemLimit: 4 * gib}, origin, 13)
	e.Run(24 * time.Hour)
	o := e.observe(key(), 24*time.Hour)
	// Four six-hourly deploys in a day.
	if o.Restarts < 3 {
		t.Errorf("restarts = %v, want about 4 from six-hourly deploys", o.Restarts)
	}
	if o.OOMEvents != 0 {
		t.Errorf("deploy restarts must not be reported as OOMKills, got %v", o.OOMEvents)
	}
}
