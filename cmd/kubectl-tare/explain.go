package main

import (
	"fmt"

	"github.com/mfranzon/tare/internal/analyze"
	"github.com/mfranzon/tare/internal/packing"
)

// --explain is where the sentences went.
//
// Dropping them was never an option. A code says CPU_UNDERUSED; it does not
// say "what this does NOT rule out: a hot standby, a disaster-recovery
// replica, an operator that only acts on a rare event". That paragraph is the
// difference between a report and a workload somebody deleted on a Friday, and
// no column can carry it. So the tables are the default and the prose is one
// flag away, rather than the other way round.
func printExplanations(r *report) {
	fmt.Println()
	fmt.Println("# explanations")

	for _, f := range r.Findings {
		if len(f.Reasons) == 0 && len(f.Ceilings) == 0 {
			continue
		}
		fmt.Printf("\n%s  %s/%s [%s]  confidence %s (%d samples)\n",
			f.Verdict, f.Namespace, f.Workload, f.Container, f.Confidence, f.Samples)
		for _, reason := range f.Reasons {
			fmt.Printf("    %-28s %s\n", reason.Code, reason.Text)
		}
		for _, c := range f.Ceilings {
			code := "CEILING"
			if c.Unresolved {
				code = "CEILING_UNRESOLVED"
			}
			fmt.Printf("    %-28s %s\n", code+" "+string(c.Kind), c.Note)
		}
	}

	explainTotals(r.Findings)
	explainNodes(r)
	explainCommitment(r.Commitment)
}

// explainTotals carries the two readings of the summary that a count does not:
// that a starved container is the thing to fix first, and that an idle one is
// a deletion rather than a resize.
func explainTotals(fs []analyze.Finding) {
	counts := map[analyze.Verdict]int{}
	for _, f := range fs {
		counts[f.Verdict]++
	}
	var out []string
	if n := counts[analyze.Saturated]; n > 0 {
		out = append(out, fmt.Sprintf(
			"%d container(s) are starved and need MORE, not less: fix those first.", n))
	}
	if n := counts[analyze.Idle]; n > 0 {
		out = append(out, fmt.Sprintf(
			"%d container(s) did no measurable work all window. The larger saving there is deletion, "+
				"not resizing, which is why it is not folded into reclaimable.*.", n))
	}
	if len(out) == 0 {
		return
	}
	fmt.Println("\ntotals:")
	for _, s := range out {
		fmt.Printf("  - %s\n", s)
	}
}

func explainNodes(r *report) {
	fmt.Println("\nnodes:")
	if r.NodeNote != "" {
		fmt.Printf("  - not computed: %s\n", r.NodeNote)
		return
	}
	for _, l := range nodeLines(r.Nodes) {
		fmt.Printf("  %s\n", l)
	}
}

func explainCommitment(cs []packing.Commitment) {
	if len(cs) == 0 {
		return
	}
	fmt.Println("\ncommitment:")
	fmt.Println("  Requests are what the scheduler reserved; limits are what the pods may take.")
	fmt.Println("  MEM_OVERCOMMIT: if these pods all claim their limit the kernel starts OOMKilling.")
	fmt.Println("  CPU_OVERCOMMIT_HEAVY: looks empty to the scheduler, throttles under simultaneous load.")
	fmt.Println("  UNLIMITED_CPU_PODS / UNLIMITED_MEM_PODS: pods with no limit, so these totals are a floor.")
	fmt.Println("  A node can be under-requested and over-limited at once. The scheduler only reads the first,")
	fmt.Println("  which is why shrinking requests without looking at limits packs more onto a node that is")
	fmt.Println("  already promised away.")
}
