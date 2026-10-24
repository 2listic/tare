package render

import (
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/2listic/tare/internal/analyze"
	"github.com/2listic/tare/internal/packing"
	"github.com/2listic/tare/internal/report"
)

// --explain is where the sentences went.
//
// Dropping them was never an option. A code says CPU_UNDERUSED; it does not
// say "what this does NOT rule out: a hot standby, a disaster-recovery
// replica, an operator that only acts on a rare event". That paragraph is the
// difference between a report and a workload somebody deleted on a Friday, and
// no column can carry it. So the tables are the default and the prose is one
// flag away, rather than the other way round.
func (p *printer) printExplanations(r *report.Report, wide bool) {
	fmt.Fprintln(p.out)
	fmt.Fprintln(p.out, "# explanations")

	for _, f := range r.Findings {
		if len(f.Reasons) == 0 && len(f.Ceilings) == 0 {
			continue
		}
		fmt.Fprintf(p.out, "\n%s  %s/%s [%s]  confidence %s (%d samples)\n",
			f.Verdict, f.Namespace, f.Workload, f.Container, f.Confidence, f.Samples)
		for _, reason := range f.Reasons {
			fmt.Fprintf(p.out, "    %-28s %s\n", reason.Code, reason.Text)
		}
		for _, c := range f.Ceilings {
			fmt.Fprintf(p.out, "    %-28s %s\n", CeilingCode(c)+" "+string(c.Kind), c.Note)
		}
	}

	p.explainTotals(r.Findings)
	p.explainNodes(r)
	p.explainCommitment(r.Commitment, wide)
}

// explainTotals carries the two readings of the summary that a count does not:
// that a starved container is the thing to fix first, and that an idle one is
// a deletion rather than a resize.
func (p *printer) explainTotals(fs []analyze.Finding) {
	counts := report.Tally(fs)
	var out []string
	if n := counts[analyze.Saturated]; n > 0 {
		out = append(out, fmt.Sprintf(
			"%d container(s) are starved and need MORE, not less: fix those first.", n))
	}
	if n := counts[analyze.Idle]; n > 0 {
		out = append(out, fmt.Sprintf(
			"%d container(s) did no measurable work all window. The larger saving there is deletion, "+
				"not resizing, which is why it is counted apart from the lower row in totals.", n))
	}
	if len(out) == 0 {
		return
	}
	fmt.Fprintln(p.out, "\ntotals:")
	for _, s := range out {
		fmt.Fprintf(p.out, "  - %s\n", s)
	}
}

func (p *printer) explainNodes(r *report.Report) {
	fmt.Fprintln(p.out, "\nnodes:")
	if r.NodeNote != "" {
		fmt.Fprintf(p.out, "  - not computed: %s\n", r.NodeNote)
		return
	}
	for _, l := range nodeLines(r.Nodes) {
		fmt.Fprintf(p.out, "  %s\n", l)
	}
	for _, l := range footprintLines(r) {
		fmt.Fprintf(p.out, "  %s\n", l)
	}
}

func (p *printer) explainCommitment(cs []packing.Commitment, wide bool) {
	if len(cs) == 0 {
		return
	}
	fmt.Fprintln(p.out, "\ncommitment:")
	if !wide {
		w := tabwriter.NewWriter(p.out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "  NODE	PODS	CPU_REQ_PCT	CPU_LIM_PCT	MEM_REQ_PCT	MEM_LIM_PCT	NOTE")
		for _, c := range cs {
			note := strings.Join(commitmentCodes(c), ",")
			if note == "" {
				note = "-"
			}
			fmt.Fprintf(w, "  %s	%d	%.0f	%.0f	%.0f	%.0f	%s\n",
				c.Node, c.Pods, c.ReqCPUFrac()*100, c.CPURatio()*100,
				c.ReqMemFrac()*100, c.MemRatio()*100, note)
		}
		w.Flush()
	}
	fmt.Fprintln(p.out, "  Requests are what the scheduler reserved; limits are what the pods may take.")
	seen := map[string]bool{}
	for _, c := range cs {
		for _, code := range commitmentCodes(c) {
			name, _, _ := strings.Cut(code, "=")
			seen[name] = true
		}
	}
	if seen["CORDONED"] {
		fmt.Fprintln(p.out, "  CORDONED: unschedulable node, excluded as a packing destination.")
	}
	if seen["MEM_OVERCOMMIT"] {
		fmt.Fprintln(p.out, "  MEM_OVERCOMMIT: if these pods all claim their limit the kernel starts OOMKilling.")
	}
	if seen["CPU_OVERCOMMIT_HEAVY"] {
		fmt.Fprintln(p.out, "  CPU_OVERCOMMIT_HEAVY: looks empty to the scheduler, throttles under simultaneous load.")
	}
	if seen["CPU_OVERCOMMIT"] {
		fmt.Fprintln(p.out, "  CPU_OVERCOMMIT: CPU limits exceed twice the node's allocatable capacity.")
	}
	if seen["UNLIMITED_CPU_CONTAINERS"] || seen["UNLIMITED_MEM_CONTAINERS"] {
		fmt.Fprintln(p.out, "  UNLIMITED_CPU_CONTAINERS / UNLIMITED_MEM_CONTAINERS: containers with no limit, so these totals are a floor.")
	}
	fmt.Fprintln(p.out, "  A node can be under-requested and over-limited at once. The scheduler only reads the first,")
	fmt.Fprintln(p.out, "  which is why shrinking requests without looking at limits packs more onto a node that is")
	fmt.Fprintln(p.out, "  already promised away.")
}
