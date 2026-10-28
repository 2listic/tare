package analyze_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/2listic/tare/internal/analyze"
	"github.com/2listic/tare/internal/simulate"
)

// Every threshold in DefaultThresholds was chosen by judgement. The simulator
// runs 30 days in 27ms, so judgement can be replaced by measurement.
//
// Scoring is by replay, not by opinion: take the recommendation a configuration
// produces, apply it as a hard limit, run the same window again, and see
// whether the workload survives. A threshold is good if it reclaims resource
// without killing anything, and the interesting question for each is whether
// the shipped value sits on a plateau or next to a cliff.

const sweepWindow = 7 * 24 * time.Hour

// scenario is a starting condition. A sweep that only ever starts from a
// generous allocation cannot exercise any threshold on the saturation path,
// because nothing is ever starved.
type scenario struct {
	name  string
	alloc func(peakCPU float64) simulate.Allocation
	// want is the verdict a correct analyzer produces, or "" where the
	// archetype decides (a leak is a leak whatever it was given).
	want func(d simulate.Demand) analyze.Verdict
}

var scenarios = []scenario{
	{
		name: "generous",
		alloc: func(float64) simulate.Allocation {
			return simulate.Allocation{CPURequest: 2, CPULimit: 4, MemRequest: 4 * gib, MemLimit: 8 * gib}
		},
		want: func(d simulate.Demand) analyze.Verdict {
			if d.MemGrowthPerHour > 0 && d.DeployEvery == 0 {
				return analyze.Leaking
			}
			if d.Shape == simulate.NoRequests {
				return analyze.Unset
			}
			return analyze.Waste
		},
	},
	{
		// Severe starvation produces PSI around 0.65, above every plausible
		// threshold, so it tests nothing about where the threshold sits. Mild
		// starvation at 0.92 of peak lands at PSI 0.08, straddling the shipped
		// 0.05: a tool that only notices catastrophic starvation is not much
		// of a warning system.
		name: "mildly-starved",
		alloc: func(peak float64) simulate.Allocation {
			return simulate.Allocation{
				CPURequest: peak * 0.92, CPULimit: peak * 0.92,
				MemRequest: 4 * gib, MemLimit: 8 * gib,
			}
		},
		want: func(d simulate.Demand) analyze.Verdict {
			if d.MemGrowthPerHour > 0 && d.DeployEvery == 0 {
				return analyze.Leaking
			}
			return analyze.Saturated
		},
	},
	{
		name: "cpu-starved",
		alloc: func(peak float64) simulate.Allocation {
			return simulate.Allocation{
				CPURequest: peak * 0.35, CPULimit: peak * 0.35,
				MemRequest: 4 * gib, MemLimit: 8 * gib,
			}
		},
		want: func(d simulate.Demand) analyze.Verdict {
			if d.MemGrowthPerHour > 0 && d.DeployEvery == 0 {
				return analyze.Leaking
			}
			return analyze.Saturated
		},
	},
}

type outcome struct {
	verdict  analyze.Verdict
	correct  bool
	withheld bool
	oom      float64
	psi      float64
	cpuSaved float64 // fraction of the starting CPU limit reclaimed
	memSaved float64
}

func (o outcome) safe() bool { return o.oom == 0 && o.psi <= 0.10 }

// evaluate observes one archetype, takes the recommendation, and replays under
// it.
func evaluate(d simulate.Demand, sc scenario, th analyze.Thresholds, seed int64) outcome {
	peak := simulate.PeakCPU(d, origin, sweepWindow, seed)
	start := sc.alloc(peak)

	c := simulate.NewCluster("sim", origin, seed, d)
	c.SetAllocation(d.Name, start)
	c.Run(sweepWindow)
	obs, _ := c.Observe(nil, sweepWindow)
	f := analyze.Run(c.Containers(), obs, sweepWindow, th)[0]

	out := outcome{verdict: f.Verdict, correct: f.Verdict == sc.want(d)}
	if f.RecMem == 0 || f.RecCPULimit == 0 {
		out.withheld = true
		return out
	}
	out.cpuSaved = 1 - float64(f.RecCPULimit)/1000/start.CPULimit
	out.memSaved = 1 - float64(f.RecMem)/float64(start.MemLimit)

	replay := simulate.New(d, simulate.Allocation{
		CPURequest: float64(f.RecCPU) / 1000,
		CPULimit:   float64(f.RecCPULimit) / 1000,
		MemRequest: f.RecMem, MemLimit: f.RecMem,
	}, origin, seed)
	replay.Run(sweepWindow)
	after, _ := (&simulate.Cluster{Namespace: "sim",
		Envs: map[string]*simulate.Env{d.Name: replay}}).Observe(nil, sweepWindow)
	for _, o := range after {
		out.oom, out.psi = o.OOMEvents, o.PSIWaitingMax
	}
	return out
}

type row struct {
	value                   float64
	safe, withheld, correct int
	total                   int
	cpuSaved                float64
	memSaved                float64
	problems                []string
}

func runCatalog(th analyze.Thresholds, value float64) row {
	r := row{value: value}
	for i, d := range simulate.Catalog() {
		for _, sc := range scenarios {
			o := evaluate(d, sc, th, int64(500+i))
			r.total++
			if o.correct {
				r.correct++
			} else {
				r.problems = append(r.problems, fmt.Sprintf("%s/%s=%s", d.Name, sc.name, o.verdict))
			}
			switch {
			case o.withheld:
				r.withheld++
			case o.safe():
				r.safe++
				r.cpuSaved += o.cpuSaved
				r.memSaved += o.memSaved
			default:
				r.problems = append(r.problems, fmt.Sprintf("UNSAFE %s/%s(psi=%.2f,oom=%.0f)", d.Name, sc.name, o.psi, o.oom))
			}
		}
	}
	if r.safe > 0 {
		r.cpuSaved /= float64(r.safe)
		r.memSaved /= float64(r.safe)
	}
	return r
}

func TestThresholdSweep(t *testing.T) {
	if testing.Short() {
		t.Skip("sweep is slow; run without -short")
	}
	base := analyze.DefaultThresholds()

	sweeps := []struct {
		name    string
		shipped float64
		values  []float64
		set     func(*analyze.Thresholds, float64)
	}{
		{"PSIWaiting", base.PSIWaiting, []float64{0.005, 0.01, 0.02, 0.05, 0.10, 0.25, 0.50},
			func(th *analyze.Thresholds, v float64) { th.PSIWaiting = v }},
		{"MinSaturationUtil", base.MinSaturationUtil, []float64{0, 0.25, 0.5, 0.75, 0.9, 1.0, 1.2},
			func(th *analyze.Thresholds, v float64) { th.MinSaturationUtil = v }},
		{"WasteRatio", base.WasteRatio, []float64{0.10, 0.25, 0.40, 0.60, 0.80},
			func(th *analyze.Thresholds, v float64) { th.WasteRatio = v }},
		{"Margin", base.Margin, []float64{0.00, 0.05, 0.15, 0.30, 0.50},
			func(th *analyze.Thresholds, v float64) { th.Margin = v }},
		{"BurstMargin", base.BurstMargin, []float64{0.00, 0.10, 0.25, 0.50, 1.00},
			func(th *analyze.Thresholds, v float64) { th.BurstMargin = v }},
		{"StartupMargin", base.StartupMargin, []float64{0.00, 0.20, 0.40, 0.75},
			func(th *analyze.Thresholds, v float64) { th.StartupMargin = v }},
		{"StartupRatio", base.StartupRatio, []float64{1.0, 1.2, 1.5, 2.0, 4.0},
			func(th *analyze.Thresholds, v float64) { th.StartupRatio = v }},
		{"LeakRatio", base.LeakRatio, []float64{1.2, 1.5, 2.0, 4.0, 10.0},
			func(th *analyze.Thresholds, v float64) { th.LeakRatio = v }},
		{"PlateauRatio", base.PlateauRatio, []float64{0.70, 0.80, 0.90, 0.95, 0.99},
			func(th *analyze.Thresholds, v float64) { th.PlateauRatio = v }},
		{"PlateauHeadroom", base.PlateauHeadroom, []float64{0.30, 0.50, 0.70, 0.90},
			func(th *analyze.Thresholds, v float64) { th.PlateauHeadroom = v }},
	}

	for _, sw := range sweeps {
		t.Run(sw.name, func(t *testing.T) {
			t.Logf("%-8s %8s %8s %6s %9s %9s", "value", "correct", "safe", "wthld", "cpu saved", "mem saved")
			var rows []row
			for _, v := range sw.values {
				th := analyze.DefaultThresholds()
				sw.set(&th, v)
				r := runCatalog(th, v)
				rows = append(rows, r)
				marker := " "
				if v == sw.shipped {
					marker = "*" // the value currently shipped
				}
				t.Logf("%s%-7.3f %4d/%-3d %4d/%-3d %6d %8.0f%% %8.0f%%",
					marker, v, r.correct, r.total, r.safe, r.total, r.withheld,
					100*r.cpuSaved, 100*r.memSaved)
			}

			// Averages hide single-archetype effects: a threshold that changes
			// one workload's limit by 11% moves a twelve-way mean by 1%, which
			// reads as noise. Name the archetypes that actually responded.
			lo, hi := analyze.DefaultThresholds(), analyze.DefaultThresholds()
			sw.set(&lo, sw.values[0])
			sw.set(&hi, sw.values[len(sw.values)-1])
			var moved []string
			for i, d := range simulate.Catalog() {
				for _, sc := range scenarios {
					a := evaluate(d, sc, lo, int64(500+i))
					b := evaluate(d, sc, hi, int64(500+i))
					if a.verdict != b.verdict || a.withheld != b.withheld ||
						absf(a.cpuSaved-b.cpuSaved) > 0.005 || absf(a.memSaved-b.memSaved) > 0.005 {
						moved = append(moved, d.Name+"/"+sc.name)
					}
				}
			}
			if len(moved) == 0 {
				t.Logf("SENSITIVITY: no archetype responds to %s anywhere in this range; "+
					"the catalog does not exercise it", sw.name)
			} else {
				t.Logf("SENSITIVITY: %s changes the outcome for %v", sw.name, moved)
			}

			// A threshold that changes nothing across its whole range is either
			// inert or unexercised by this catalog. Both are worth knowing.
			same := true
			for _, r := range rows[1:] {
				if r.safe != rows[0].safe || r.withheld != rows[0].withheld || r.correct != rows[0].correct ||
					absf(r.memSaved-rows[0].memSaved) > 0.005 || absf(r.cpuSaved-rows[0].cpuSaved) > 0.005 {
					same = false
				}
			}
			if same {
				t.Logf("NOTE: %s had no effect anywhere in this range on this catalog", sw.name)
			}
		})
	}
}

func absf(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}
