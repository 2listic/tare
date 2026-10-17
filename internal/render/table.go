package render

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/mfranzon/tare/internal/analyze"
	"github.com/mfranzon/tare/internal/report"
)

// The default report is what fits on a screen: one row per finding somebody
// might act on. That table is the only thing on stdout.
//
// The summary and the warnings go to stderr, after it. They are not dropped:
// a warning is how a run that could not see PSI, or a third of the cluster,
// avoids looking identical to one that saw everything. stderr is where the
// contract already puts warnings, so a terminal shows them and a pipe gets
// pure findings.
//
// The first table rewrite put every fact the run established on stdout. It
// parsed, and it was 241 lines for 28 containers, two thirds of them evidence
// rows restating the findings table: SIGNAL_NOT_MEASURED once per container
// for a signal the cluster does not export at all, CPU_UNDERUSED under a row
// that already says WASTE 2000m→10m. `-o wide` is that output, unchanged, for
// the script and the ticket. This one is for the person.
//
// stdout follows the same contract as -o wide: a section marker, a header,
// two-space columns, "-" for empty.

// impliedCodes say nothing the row they sit on does not already show. They
// stay in -o wide and the JSON; in the compact view they would bury the codes
// that are news.
var impliedCodes = map[analyze.Code]bool{
	analyze.CPUUnderused: true, // WASTE with a smaller number
	analyze.MemUnderused: true,
	analyze.CPUFloor:     true, // the recommendation is the minimum, visibly
	analyze.MemFloor:     true,
	analyze.BestEffort:   true, // the current value reads "unset"
}

const maxWhyCodes = 2

func (p *printer) printCompact(r *report.Report) {
	shown, hidden := r.Findings, []analyze.Finding(nil)
	if !r.VerdictFiltered {
		shown, hidden = compactRows(r.Findings)
	}
	p.printCompactFindings(shown, r.BySavings)

	fmt.Fprintln(p.diag)
	p.printSummary(r, len(shown))
	p.printWarnings(r)
	if len(hidden) > 0 {
		fmt.Fprintf(p.diag, "not listed: %s (--verdict %s)\n",
			VerdictCounts(hidden), strings.Join(verdictNames(hidden), ","))
	}
}

// printSummary is one line, so the answer is the first thing after the table.
// Nodes lead because they are the only figure that maps to a bill; request
// changes follow, signed, and never called "reclaimed": a lowered request
// frees nothing anyone pays for until a node goes away.
func (p *printer) printSummary(r *report.Report, listed int) {
	s := report.Totals(r.Findings)
	head := fmt.Sprintf("%d containers", len(r.Findings))
	if listed != len(r.Findings) {
		head = fmt.Sprintf("%d containers (%d listed)", len(r.Findings), listed)
	}
	line := fmt.Sprintf("%s over %s:", head, r.WindowUsed)
	if nodes, freed := nodeCells(r); nodes != "" {
		line += fmt.Sprintf(" nodes freed %s of %s;", freed, nodes)
	}
	line += fmt.Sprintf(" requests lowered on %d (%s CPU, %s mem)",
		s.Workloads, CPUDelta(-s.CPUMillicores), MemDelta(-s.MemBytes))
	if s.AddWorkloads > 0 {
		line += fmt.Sprintf(", raised on %d (%s CPU, %s mem)",
			s.AddWorkloads, CPUDelta(s.AddCPUMillicores), MemDelta(s.AddMemBytes))
	}
	if s.MemUnknown > 0 {
		line += fmt.Sprintf("; memory unknown on %d (OOMKilled)", s.MemUnknown)
	}
	if s.IdleWorkloads > 0 {
		line += fmt.Sprintf("; %d IDLE not counted", s.IdleWorkloads)
	}
	fmt.Fprintln(p.diag, line)
}

// nodeCells gives the node count and the nodes the recommendations free, or
// nothing when the count was not computed; a warning says why. Consolidation
// slack is not tare's saving and is a warning too, not folded in here.
func nodeCells(r *report.Report) (running, freed string) {
	if report.NodesNotComputed(r) != "" {
		return "", ""
	}
	return strconv.Itoa(r.Nodes.NodesRunning), freedLabel(r.Nodes)
}

// printWarnings collects everything that makes the rest of the report less
// complete than it looks. It is the part the compact view must not lose: a
// signal that is absent is not a signal that is zero.
func (p *printer) printWarnings(r *report.Report) {
	type warning struct{ code, detail string }
	var ws []warning
	add := func(code, detail string) { ws = append(ws, warning{code, detail}) }

	if r.Demo {
		add("DEMO", "simulated workloads, no cluster was read")
	}
	if r.WindowUsed != r.Window {
		add("WINDOW_SHORT", r.Window)
	}
	if r.CoverageTotal > 0 {
		noData := report.Tally(r.Findings)[analyze.NoData]
		detail := fmt.Sprintf("%d of %d pod containers have no series (%d NO-DATA workloads; the rest have other pods covered)",
			r.CoverageMissing, r.CoverageTotal, noData)
		if r.CoverageMissing == noData {
			detail = fmt.Sprintf("%d of %d pod containers have no series (%d NO-DATA)",
				r.CoverageMissing, r.CoverageTotal, noData)
		}
		add("METRICS_COVERAGE", detail)
	}
	for _, g := range r.Missing {
		add("SIGNAL_"+g.Status, string(g.Signal)+": "+g.Detail)
	}

	// Per-container gaps are one fact per signal, not one row per container.
	missing := map[string]int{}
	var order []string
	for _, f := range r.Findings {
		for _, m := range f.Missing {
			if missing[string(m)] == 0 {
				order = append(order, string(m))
			}
			missing[string(m)]++
		}
	}
	for _, m := range order {
		add("SIGNAL_NOT_MEASURED", fmt.Sprintf("%s: %d of %d containers", m, missing[m], len(r.Findings)))
	}

	if note := report.NodesNotComputed(r); note != "" {
		add("NODES_NOT_COMPUTED", note)
	} else {
		if n := r.Nodes.FromConsolidation(); n > 0 {
			add("NODES_ALREADY_FREE", fmt.Sprintf("%d nodes free by consolidation alone, not counted as freed", n))
		}
		if r.Nodes.Pinned > 0 {
			add("PODS_PINNED", fmt.Sprintf("%d pods held their node, so freed is a lower bound", r.Nodes.Pinned))
		}
		for _, u := range r.Nodes.Unusable {
			add("NODE_UNUSABLE", u)
		}
	}
	if n := len(r.DrainBlockers); n > 0 {
		add("DRAIN_BLOCKED", fmt.Sprintf("%d disruption budget(s) allow no eviction", n))
	}

	over := map[string]int{}
	for _, c := range r.Commitment {
		for _, code := range commitmentCodes(c) {
			over[code]++
		}
	}
	for _, code := range []string{"MEM_OVERCOMMIT", "CPU_OVERCOMMIT_HEAVY", "CPU_OVERCOMMIT"} {
		if over[code] > 0 {
			add(code, fmt.Sprintf("%d of %d nodes", over[code], len(r.Commitment)))
		}
	}

	if len(ws) == 0 {
		return
	}
	w := tabwriter.NewWriter(p.diag, 0, 0, 2, ' ', 0)
	width := termWidth() - len("warning  SIGNAL_NOT_MEASURED") - 2
	for _, x := range ws {
		fmt.Fprintf(w, "warning\t%s\t%s\n", x.code, Clip(x.detail, width))
	}
	w.Flush()
}

// compactRows drops the verdicts that ask nothing of the reader. They are
// counted in a comment rather than vanishing, and an explicit --verdict lists
// every row it selected.
func compactRows(fs []analyze.Finding) (shown, hidden []analyze.Finding) {
	for _, f := range fs {
		if f.Verdict == analyze.Healthy || f.Verdict == analyze.NoData {
			hidden = append(hidden, f)
		} else {
			shown = append(shown, f)
		}
	}
	return shown, hidden
}

// VerdictCounts is "3 SATURATED, 12 WASTE" in report order.
func VerdictCounts(fs []analyze.Finding) string {
	var parts []string
	for _, c := range report.CountVerdicts(fs) {
		parts = append(parts, fmt.Sprintf("%d %s", c.N, c.Verdict))
	}
	return strings.Join(parts, ", ")
}

func verdictNames(fs []analyze.Finding) []string {
	var out []string
	for _, c := range report.CountVerdicts(fs) {
		out = append(out, string(c.Verdict))
	}
	return out
}

// compactOrder groups by verdict in report order and, inside a band, puts the
// biggest reclaim first: the order a reader works through the list in.
func compactOrder(fs []analyze.Finding) []analyze.Finding {
	rank := map[analyze.Verdict]int{}
	for i, v := range analyze.ReportOrder() {
		rank[v] = i
	}
	out := append([]analyze.Finding(nil), fs...)
	sort.SliceStable(out, func(i, j int) bool {
		if ri, rj := rank[out[i].Verdict], rank[out[j].Verdict]; ri != rj {
			return ri < rj
		}
		return report.SavingsWeight(out[i]) > report.SavingsWeight(out[j])
	})
	return out
}

func (p *printer) printCompactFindings(fs []analyze.Finding, bySavings bool) {
	if !bySavings {
		fs = compactOrder(fs)
	}

	// A fixed budget, not whatever the terminal has left: WHY is the widest
	// column and the one that varies most, and sizing names against it cut
	// every workload to a stub on the one row that happened to carry four codes.
	const nameWidth = 32
	w := p.section("findings", "VERDICT", "CONF", "NAMESPACE", "WORKLOAD", "CONTAINER", "PODS", "CPU", "MEM", "WHY")
	for _, f := range fs {
		row(w, string(f.Verdict), string(f.Confidence), f.Namespace,
			WorkloadCell(f.Workload, nameWidth), f.Container, strconv.Itoa(f.Pods),
			changeCell(f.CPURequest, f.RecCPU, Milli), memChangeCell(f),
			whyCell(f))
	}
	// No placeholder row when nothing matched: a "NONE" row is a data row to
	// every script that counts lines after the header. The stderr summary
	// already says 0.
	w.Flush()
}

// memChangeCell is "16Mi→?" for a memory tare does not know. Printed as an
// unchanged "16Mi" beside OOMKILLED, it read as an answer.
func memChangeCell(f analyze.Finding) string {
	if f.MemCensored {
		return Human(f.MemRequest) + "→?"
	}
	return changeCell(f.MemRequest, f.RecMem, Human)
}

// changeCell shows the request and where it should go. No recommendation, or
// one equal to the current value, is the current value alone: in both cases
// the reader should change nothing.
func changeCell(cur, rec int64, f func(int64) string) string {
	if rec == 0 || rec == cur {
		return f(cur)
	}
	return f(cur) + "→" + f(rec)
}

// WorkloadCell drops the kind when it is a Deployment, which almost
// everything is, and abbreviates it otherwise. A name that still does not fit
// loses its middle: the tail is what tells two generated names apart.
func WorkloadCell(workload string, width int) string {
	kind, name, ok := strings.Cut(workload, "/")
	if ok {
		switch kind {
		case "Deployment":
			workload = name
		case "StatefulSet":
			workload = "sts/" + name
		case "DaemonSet":
			workload = "ds/" + name
		case "ReplicaSet":
			workload = "rs/" + name
		default:
			workload = strings.ToLower(kind) + "/" + name
		}
	}
	if len(workload) <= width {
		return workload
	}
	head := (width - 1) / 3
	return workload[:head] + "…" + workload[len(workload)-(width-1-head):]
}

// whyCell is the codes that are news for this row, most important first as the
// analyzer ordered them, with a count of the rest so a truncation is visible.
func whyCell(f analyze.Finding) string {
	var codes []string
	seen := map[analyze.Code]bool{}
	for _, r := range f.Reasons {
		if impliedCodes[r.Code] || seen[r.Code] {
			continue
		}
		seen[r.Code] = true
		codes = append(codes, string(r.Code))
	}
	for _, c := range f.Ceilings {
		if c.Unresolved {
			codes = append(codes, string(analyze.CeilingBlocked))
		}
	}
	if len(codes) > maxWhyCodes {
		codes = append(codes[:maxWhyCodes], fmt.Sprintf("+%d", len(codes)-maxWhyCodes))
	}
	return strings.Join(codes, ",")
}

func (p *printer) compactHint() {
	fmt.Fprintln(p.diag, "-o wide for limits, pressure, per-code evidence and node detail; --explain for the reasoning")
}
