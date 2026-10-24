package report

import (
	"testing"

	"github.com/mfranzon/tare/internal/packing"
)

func footprintReport(f Factors) *Report {
	return &Report{Factors: f, Nodes: packing.Result{
		Trustworthy: true, NodesRunning: 12, NodesPackedNow: 11, NodesPackedRecommended: 9, Pinned: 3,
	}}
}

// 2 nodes x 100 W x 8760 h = 1752 kWh; at 400 g/kWh, 700.8 kg. Each stage is
// credited separately, and tare's row is a lower bound because pods were
// pinned.
func TestFootprintArithmetic(t *testing.T) {
	rows := Footprint(footprintReport(Factors{NodeWatts: 100, GridGCO2PerKWh: 400}))
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	slack, tare := rows[0], rows[1]
	if slack.Credit != "cluster_slack" || slack.Nodes != 1 || slack.KWhYear != 876 || slack.LowerBound {
		t.Errorf("cluster_slack row wrong: %+v", slack)
	}
	if tare.Credit != "tare" || tare.Nodes != 2 || tare.KWhYear != 1752 || !tare.LowerBound {
		t.Errorf("tare row wrong: %+v", tare)
	}
	if tare.KgCO2eYear == nil || *tare.KgCO2eYear != 700.8 {
		t.Errorf("tare kgCO2e = %v, want 700.8", tare.KgCO2eYear)
	}
}

// Without a grid intensity, emissions are unknown, not zero.
func TestFootprintWithoutGridIsUnknown(t *testing.T) {
	for _, row := range Footprint(footprintReport(Factors{NodeWatts: 100})) {
		if row.KgCO2eYear != nil {
			t.Errorf("%s: kgCO2e = %v with no grid intensity, want nil", row.Stage, *row.KgCO2eYear)
		}
	}
}

// Recommendations that need more nodes cost energy; the sign has to survive.
func TestFootprintCanBeNegative(t *testing.T) {
	r := footprintReport(Factors{NodeWatts: 100})
	r.Nodes.NodesPackedRecommended = 13
	if got := Footprint(r)[1]; got.Nodes != -2 || got.KWhYear != -1752 {
		t.Errorf("tare row = %+v, want -2 nodes and -1752 kWh", got)
	}
}

// No factors, or no trustworthy count, is no footprint: nothing to multiply.
func TestFootprintNeedsFactorsAndACount(t *testing.T) {
	if Footprint(footprintReport(Factors{})) != nil {
		t.Error("footprint printed with no factors given")
	}
	r := footprintReport(Factors{NodeWatts: 100})
	r.Nodes.Trustworthy = false
	if Footprint(r) != nil {
		t.Error("footprint computed from a node count the self-check withheld")
	}
}
