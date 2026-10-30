package render

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/2listic/tare/internal/analyze"
	"github.com/2listic/tare/internal/packing"
	"github.com/2listic/tare/internal/report"
)

// The terminal report is a sequence of tables and nothing else.
//
// It used to be a table wrapped in prose: a header sentence, a paragraph of
// summary, and under -v a bulleted essay per container. All of it was true and
// none of it could be piped anywhere. Everything a run establishes is now a
// row, under a "# section=" marker, so a reader sees columns and awk sees
// fields. The sentences are not gone - they are the part that stops somebody
// applying a number they should not - they moved behind --explain.
//
// Rules a consumer can rely on:
//   - a line starting with '#' is a section marker or a comment, never data
//   - the first line of a section is its column header
//   - columns are separated by two or more spaces
//   - a field with no value is "-", never blank
//   - every hint, warning and progress note goes to stderr, so stdout is data

// printer writes the tabular formats: data to out, and the summary,
// warnings and hints to diag, so a redirected report stays pure data.
type printer struct {
	out, diag io.Writer
	// open records that a section has been written, so the next one is
	// separated from it by a blank line.
	open bool
}

func (p *printer) section(name string, cols ...string) *tabwriter.Writer {
	if p.open {
		fmt.Fprintln(p.out)
	}
	p.open = true
	fmt.Fprintf(p.out, "# section=%s\n", name)
	w := tabwriter.NewWriter(p.out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, strings.Join(cols, "\t"))
	return w
}

func row(w *tabwriter.Writer, cells ...string) {
	for i, c := range cells {
		if c == "" {
			cells[i] = "-"
		}
	}
	fmt.Fprintln(w, strings.Join(cells, "\t"))
}

// printMeta says what this run was, which is the first thing a reader of a
// pasted report needs and the first thing a script filing one needs.
func (p *printer) printMeta(r *report.Report) {
	w := p.section("meta", "KEY", "VALUE")
	row(w, "source", r.Source)
	row(w, "window", r.Window)
	row(w, "containers", strconv.Itoa(len(r.Findings)))
	row(w, "demo", strconv.FormatBool(r.Demo))
	row(w, "redacted", strconv.FormatBool(r.Redacted))
	if r.Coverage != "" {
		row(w, "coverage", r.Coverage)
	}
	w.Flush()
}

// printSignalGaps is the degraded-report block. A signal that is absent is not
// a signal that is zero, and a format that drops this turns a partial run into
// a confident one.
func (p *printer) printSignalGaps(gaps []analyze.SignalGap) {
	if len(gaps) == 0 {
		return
	}
	w := p.section("signals", "SIGNAL", "STATUS", "DETAIL")
	for _, g := range gaps {
		row(w, string(g.Signal), g.Status, g.Detail)
	}
	w.Flush()
	fmt.Fprintln(p.out, "# findings that depended on these are downgraded, not silently assumed")
}

func (p *printer) printFindings(fs []analyze.Finding) {
	// Every column but the workload name has a bounded width, so the workload
	// column gets whatever the terminal has left. Unbounded, one
	// StatefulSet/prometheus-kps-kube-prometheus-stack-prometheus wraps every
	// row in the report into two unreadable ones.
	fixed := len("VERDICT") + len("CONF") + len("CONTAINER") + len("PRESSURE") + len("PODS")
	for _, f := range fs {
		n := len(f.Verdict) + len(f.Confidence) + len(f.Container) + len(PressureCell(f)) +
			max(len("PODS"), len(strconv.Itoa(f.Pods)))
		if n > fixed {
			fixed = n
		}
	}
	cells := len("CPU_REQ>REC") + len("CPU_LIM>REC") + len("MEM_REQ>REC") + len("MEM_LIM>REC")
	for _, f := range fs {
		if n := len(cpuCell(f)) + len(limitCell(f)) + len(memCell(f)) + len(memLimitCell(f)); n > cells {
			cells = n
		}
	}
	nameWidth := termWidth() - fixed - cells - 2*9
	if nameWidth < 24 {
		nameWidth = 24
	}

	w := p.section("findings",
		"VERDICT", "CONF", "NAMESPACE/WORKLOAD", "CONTAINER",
		"CPU_REQ>REC", "CPU_LIM>REC", "MEM_REQ>REC", "MEM_LIM>REC", "PODS", "PRESSURE")
	for _, f := range fs {
		row(w, string(f.Verdict), string(f.Confidence),
			shortWorkload(f.Namespace, f.Workload, nameWidth), f.Container,
			cpuCell(f), limitCell(f), memCell(f), memLimitCell(f),
			strconv.Itoa(f.Pods), PressureCell(f))
	}
	w.Flush()
}

// printEvidence is one row per piece of evidence: the code is the stable part,
// the numbers are what it was decided on. `grep OOMKILLED` now answers a
// question that used to need a human to read a paragraph.
func (p *printer) printEvidence(fs []analyze.Finding) {
	w := p.section("evidence",
		"NAMESPACE", "WORKLOAD", "CONTAINER", "CODE", "OBSERVED", "AGAINST", "UNIT", "SUBJECT")
	rows := 0
	for _, f := range fs {
		for _, r := range f.Reasons {
			row(w, f.Namespace, f.Workload, f.Container, string(r.Code),
				evidenceValue(r.Observed, r.Unit), evidenceValue(r.Against, r.Unit),
				string(r.Unit), r.Subject)
			rows++
		}
		// A signal that was not measured for THIS container is evidence about
		// the finding in exactly the way a measurement is, so it is a row and
		// not a footnote.
		for _, m := range f.Missing {
			row(w, f.Namespace, f.Workload, f.Container, "SIGNAL_NOT_MEASURED", "", "", "", string(m))
			rows++
		}
		for _, c := range f.Ceilings {
			row(w, f.Namespace, f.Workload, f.Container, CeilingCode(c), "", "", "", string(c.Kind))
			rows++
		}
	}
	if rows == 0 {
		row(w, "", "", "", "NO_EVIDENCE", "", "", "", "")
	}
	w.Flush()
}

// printGPUs is one row per container that holds a GPU: what the device did,
// beside the codes decided from it. Busy and SM columns are avg/max and
// avg/p95 percentages; busy-time bounds use from above only, so a high BUSY
// next to an SM of "?" is not evidence of work. VRAM_MAX is the device's,
// display server included, and only ever a floor. Nothing at all when no
// container holds a GPU.
func (p *printer) printGPUs(fs []analyze.Finding) {
	var w *tabwriter.Writer
	for _, f := range fs {
		if f.GPUs == 0 {
			continue
		}
		if w == nil {
			w = p.section("gpus", "NAMESPACE", "WORKLOAD", "CONTAINER", "RESOURCE", "DEVICES", "IDLE_DEVICES",
				"BUSY_AVG/MAX", "SM_ACTIVE_AVG/P95", "VRAM_MAX", "CODES")
		}
		u := GPUCells(f)
		row(w, f.Namespace, f.Workload, f.Container, f.GPUResource, strconv.FormatInt(f.GPUs, 10),
			u.IdleDevices, u.Busy, u.SMActive, u.VRAM, strings.Join(GPUCodes(f), ","))
	}
	if w != nil {
		w.Flush()
	}
}

// evidenceValue renders a measurement for a terminal. The raw float is in the
// JSON output; here the unit column says how to read a figure a person can
// take in at a glance. A zero means the reason had no comparand, not that
// something measured zero - a measured zero belongs in a reason that says so.
func evidenceValue(v float64, u analyze.Unit) string {
	if v == 0 {
		return "-"
	}
	switch u {
	case analyze.Bytes:
		return Human(int64(v))
	case analyze.Millicores:
		return fmt.Sprintf("%.0fm", v)
	case analyze.CoreCount:
		return fmt.Sprintf("%.2f", v)
	case analyze.Percent:
		return fmt.Sprintf("%.1f%%", v)
	case analyze.Count:
		return fmt.Sprintf("%.0f", v)
	case analyze.Ratio, analyze.SecPerSec:
		return fmt.Sprintf("%.4g", v)
	}
	return fmt.Sprintf("%g", v)
}

// printVerdicts counts containers per verdict. It used to share the totals
// section as verdict.* keys, beside rows in a different unit.
func (p *printer) printVerdicts(fs []analyze.Finding) {
	w := p.section("verdicts", "VERDICT", "CONTAINERS")
	for _, c := range report.CountVerdicts(fs) {
		row(w, string(c.Verdict), strconv.Itoa(c.N))
	}
	row(w, "ALL", strconv.Itoa(len(fs)))
	w.Flush()
}

// printTotals is the change to requests, one row per direction. A request is
// a scheduling reservation: none of these figures is usage and none is money.
// The bill moves when a node goes away, which is the nodes section.
//
// It replaces reclaimable.* / addback.* key-value rows, where a container
// count sat in the same list as two amounts, the amounts came twice (raw and
// "human"), and neither word said what was being counted.
func (p *printer) printTotals(fs []analyze.Finding) {
	s := report.Totals(fs)
	w := p.section("totals", "CHANGE", "CONTAINERS", "CPU_REQUESTS", "MEM_REQUESTS", "NOTE")
	row(w, "lower", strconv.Itoa(s.Workloads), CPUDelta(-s.CPUMillicores), MemDelta(-s.MemBytes), "")
	// A container that needs MORE than it has is not a rounding error in the
	// total, it is the finding that matters most, so it is never folded in.
	row(w, "raise", strconv.Itoa(s.AddWorkloads), CPUDelta(s.AddCPUMillicores), MemDelta(s.AddMemBytes), "")
	if s.IdleWorkloads > 0 {
		row(w, "idle", strconv.Itoa(s.IdleWorkloads), CPUDelta(-s.IdleCPUMillicores), MemDelta(-s.IdleMemBytes),
			"not_in_lower:"+string(analyze.IdleCaveat))
	}
	if s.UnverifiedWorkloads > 0 {
		row(w, "unverified", strconv.Itoa(s.UnverifiedWorkloads), CPUDelta(-s.UnverifiedCPUMillicores), MemDelta(-s.UnverifiedMemBytes),
			"not_in_lower:"+string(analyze.Unverified))
	}
	if s.MemUnknown > 0 {
		row(w, "unknown", strconv.Itoa(s.MemUnknown), "-", "?", string(analyze.MemDemandCensored))
	}
	w.Flush()
}

// printGPUTotals is the devices the GPU containers hold, apart from totals
// because a GPU is given back, not resized. IDLE is proven by busy-time;
// UNMEASURED is held with nothing known, never folded into "not idle". Nothing
// at all when no container holds a GPU.
func (p *printer) printGPUTotals(fs []analyze.Finding) {
	s := report.Totals(fs)
	if s.GPUsHeld == 0 && s.MIGSlices == 0 {
		return
	}
	w := p.section("gpu_totals", "HELD", "IDLE", "UNMEASURED", "MIG_SLICES")
	row(w, strconv.FormatInt(s.GPUsHeld, 10), strconv.FormatInt(s.GPUsIdle, 10),
		strconv.FormatInt(s.GPUsUnmeasured, 10), strconv.FormatInt(s.MIGSlices, 10))
	w.Flush()
}

// CPUDelta and memDelta sign a change so its direction is in the cell, in the
// units the findings table already uses. A zero change is "0": it is a sum, not
// an unset field.
func CPUDelta(m int64) string {
	if m == 0 {
		return "0"
	}
	return fmt.Sprintf("%+dm", m)
}

func MemDelta(b int64) string {
	switch {
	case b == 0:
		return "0"
	case b < 0:
		return "-" + Human(-b)
	}
	return "+" + Human(b)
}

// printNodes reports the only savings figure that corresponds to an invoice.
// Reclaimed millicores are the input to it, never a substitute: on a fixed node
// pool, freeing half the cluster's requests changes no bill at all.
//
// One row per stage of the replay, so the arithmetic is on the page: FREED is
// the difference from the row above, and CREDIT says whose saving it is. The
// two savings are kept apart because only one of them is tare's; folding the
// cluster's existing slack into it would be taking credit for nodes a
// consolidation pass frees without changing a request.
//
// EMPTY_NODES is what is empty at that stage, not what that stage freed. The
// two passes are packed independently, so which nodes end up empty is not a
// set one can subtract; the first version printed every empty node beside the
// resizing count and so named consolidation nodes as tare's.
func (p *printer) printNodes(r *report.Report) {
	w := p.section("nodes", "STAGE", "NODES", "FREED", "CREDIT", "EMPTY_NODES")
	if note := report.NodesNotComputed(r); note != "" {
		row(w, "not_computed", "-", "-", "-", "-")
		w.Flush()
		fmt.Fprintf(p.out, "# not computed: %s\n", note)
		return
	}
	n := r.Nodes
	freed := freedLabel(n)
	row(w, "running", strconv.Itoa(n.NodesRunning), "-", "-", "-")
	row(w, "packed_today", strconv.Itoa(n.NodesPackedNow), strconv.Itoa(n.FromConsolidation()), "cluster_slack", strings.Join(n.EmptyToday, ", "))
	row(w, "packed_after", strconv.Itoa(n.NodesPackedRecommended), freed, "tare", strings.Join(n.Removable, ", "))
	w.Flush()

	if n.Pinned > 0 || n.ExcludedNode > 0 || len(n.Unusable) > 0 {
		w := p.section("node_caveats", "CAVEAT", "COUNT", "DETAIL")
		if n.Pinned > 0 {
			row(w, "pinned_pods", strconv.Itoa(n.Pinned),
				fmt.Sprintf("of %d pods held their node, so tare's FREED is a lower bound; reasons in node_pins", n.Pinned+n.Movable))
		}
		if n.ExcludedNode > 0 {
			row(w, "cordoned_nodes", strconv.Itoa(n.ExcludedNode), "excluded as destinations")
		}
		if len(n.Unusable) > 0 {
			row(w, "unusable_nodes", strconv.Itoa(len(n.Unusable)), strings.Join(n.Unusable, "; "))
		}
		w.Flush()
	}

	if n.Pinned > 0 {
		w := p.section("node_pins", "PODS", "REASON")
		for _, reason := range n.SortedPinReasons() {
			row(w, strconv.Itoa(n.PinReasons[reason]), reason)
		}
		w.Flush()
	}
}

// printFootprint converts each replay stage's FREED into energy and emissions
// with the reader's own factors, and prints the factors under it so the
// figure never travels without its coefficient. Nothing at all when no
// factors were given: an unasked-for section of dashes is noise.
func (p *printer) printFootprint(r *report.Report) {
	if !r.Factors.Set() {
		return
	}
	w := p.section("footprint", "STAGE", "NODES_FREED", "CREDIT", "KWH_YEAR", "KGCO2E_YEAR")
	rows := report.Footprint(r)
	if rows == nil {
		row(w, "not_computed", "-", "-", "-", "-")
		w.Flush()
		fmt.Fprintln(p.out, "# not computed: no node count to convert, see section=nodes")
		return
	}
	for _, f := range rows {
		row(w, f.Stage, footprintNodes(f), f.Credit, footprintKWh(f), footprintKg(f))
	}
	w.Flush()
	fmt.Fprintf(p.out, "# factors: %s\n", factorsNote(r.Factors))
}

// printDrainBlockers reports the budgets that make a drain impossible. A
// cluster can have three nodes' worth of slack and free none of them.
func (p *printer) printDrainBlockers(why []string) {
	if len(why) == 0 {
		return
	}
	w := p.section("drain_blockers", "BLOCKER")
	for _, b := range why {
		row(w, b)
	}
	w.Flush()
}

// printCommitment shows what each node has promised against what it has.
//
// The NOTE column is codes rather than sentences: a node is either
// MEM_OVERCOMMIT or it is not, and a paragraph in a table cell is the thing
// this report was rewritten to stop doing. --explain spells them out.
func (p *printer) printCommitment(cs []packing.Commitment) {
	if len(cs) == 0 {
		return
	}
	w := p.section("commitment", "NODE", "PODS", "CPU_REQ_PCT", "CPU_LIM_PCT", "MEM_REQ_PCT", "MEM_LIM_PCT", "NOTE")
	for _, c := range cs {
		row(w, c.Node, strconv.Itoa(c.Pods),
			fmt.Sprintf("%.0f", c.ReqCPUFrac()*100), fmt.Sprintf("%.0f", c.CPURatio()*100),
			fmt.Sprintf("%.0f", c.ReqMemFrac()*100), fmt.Sprintf("%.0f", c.MemRatio()*100),
			strings.Join(commitmentCodes(c), ","))
	}
	w.Flush()
}

func commitmentCodes(c packing.Commitment) []string {
	var out []string
	if !c.Schedulable {
		out = append(out, "CORDONED")
	}
	switch {
	case c.MemRatio() > 1:
		// Memory overcommit has no graceful degradation: the kernel picks a
		// process and kills it.
		out = append(out, "MEM_OVERCOMMIT")
	case c.CPURatio() > 4:
		out = append(out, "CPU_OVERCOMMIT_HEAVY")
	case c.CPURatio() > 2:
		out = append(out, "CPU_OVERCOMMIT")
	}
	if c.UnlimitedCPU > 0 {
		out = append(out, fmt.Sprintf("UNLIMITED_CPU_CONTAINERS=%d", c.UnlimitedCPU))
	}
	if c.UnlimitedMem > 0 {
		out = append(out, fmt.Sprintf("UNLIMITED_MEM_CONTAINERS=%d", c.UnlimitedMem))
	}
	return out
}
