package report

// Factors are the reader's own figures for turning a node count into energy
// and emissions. tare measures neither: a container's power is not in any
// signal it reads, and grid intensity depends on where and when the cluster
// runs. Both are supplied, never defaulted, for the same reason tare prints no
// currency: a coefficient the tool made up would be the most precise-looking
// number on the page and the least defensible one.
type Factors struct {
	// NodeWatts is the power that goes away with one node, which is its
	// baseline draw and not its loaded draw: the workload moves to another
	// node and takes its dynamic power with it. Facility overhead (PUE) is
	// the reader's to fold in.
	NodeWatts float64
	// GridGCO2PerKWh is grid carbon intensity. Zero means not supplied, and
	// emissions are then unknown rather than zero.
	GridGCO2PerKWh float64
	// GridSource is "user" for a number the reader typed, or the Ember
	// dataset, year and country it was looked up from.
	GridSource string
}

// Set is whether any factor was supplied, which is whether a footprint was
// asked for at all.
func (f Factors) Set() bool { return f.NodeWatts > 0 }

const hoursPerYear = 8760

// FootprintRow is one replay stage expressed in energy and emissions. It is a
// node count multiplied by the reader's factors and nothing more, so it
// inherits every caveat of the count: a lower bound when pods were pinned,
// and zero on a fixed node pool that never removes the node.
type FootprintRow struct {
	Stage  string `json:"stage"`
	Credit string `json:"credit"`
	Nodes  int    `json:"nodesFreed"`
	// LowerBound mirrors the ">=" on FREED: pinned pods held their node, so
	// a real consolidation could free more.
	LowerBound bool    `json:"lowerBound"`
	KWhYear    float64 `json:"kWhPerYear"`
	// KgCO2eYear is nil when no grid intensity was given: unknown, not zero.
	KgCO2eYear *float64 `json:"kgCO2ePerYear"`
}

// Footprint converts the two savings stages of the replay. Nil when no
// factors were given or there is no trustworthy node count to convert; the
// caller says which. A negative row means the recommendations need more
// nodes, and costs energy rather than saving it.
func Footprint(r *Report) []FootprintRow {
	if !r.Factors.Set() || NodesNotComputed(r) != "" {
		return nil
	}
	n := r.Nodes
	return []FootprintRow{
		footprintRow(r.Factors, "packed_today", "cluster_slack", n.FromConsolidation(), false),
		footprintRow(r.Factors, "packed_after", "tare", n.FromResizing(), n.Pinned > 0),
	}
}

func footprintRow(f Factors, stage, credit string, nodes int, lowerBound bool) FootprintRow {
	row := FootprintRow{Stage: stage, Credit: credit, Nodes: nodes, LowerBound: lowerBound,
		KWhYear: float64(nodes) * f.NodeWatts * hoursPerYear / 1000}
	if f.GridGCO2PerKWh > 0 {
		kg := row.KWhYear * f.GridGCO2PerKWh / 1000
		row.KgCO2eYear = &kg
	}
	return row
}

// TareFootprint is the row tare can take credit for, for the summary line.
func TareFootprint(r *Report) (FootprintRow, bool) {
	rows := Footprint(r)
	if len(rows) == 0 {
		return FootprintRow{}, false
	}
	return rows[1], true
}
