package main

import (
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/mfranzon/tare/internal/analyze"
	"github.com/mfranzon/tare/internal/metrics"
)

// The interactive report. It exists because -v is all-or-nothing: on a cluster
// with three hundred containers the evidence for every row at once is not a
// report, it is a log. Here the evidence is one keystroke away for the row you
// are looking at and invisible for the rest.
//
// It is hand-rolled on golang.org/x/term, which is already in the module graph
// through client-go, so the interactive mode costs no new dependency. The
// alternative was a rendering framework and about a dozen transitive modules
// for a list with an expander in it.
//
// The rules the HTML report follows apply here too, for the same reason: a
// withheld recommendation renders as "withheld" and never as a blank or a zero,
// and the signals the run could not see are in the header rather than hidden
// behind a key.

// tuiModel is the whole state, kept pure so the layout can be tested without a
// terminal. Nothing in here reads or writes a file descriptor.
type tuiModel struct {
	rows    []tuiRow
	source  string
	window  string
	missing []string
	demo    bool
	summary string

	filter   string // verdict filter; empty means all
	search   string // incremental substring match on the workload
	scanning bool   // true while / is being typed
	cursor   int    // index into visible()
	offset   int    // first visible row, for scrolling
	expanded map[int]bool
	explain  bool // expanded rows also show the sentences behind the codes

	usage     func(analyze.Finding) (cpu, mem metrics.Series, err error)
	usageNote string // why there is no plot, when usage is nil
	pending   int    // row whose usage is fetched after the next frame, -1 for none
}

type tuiRow struct {
	verdict    string
	confidence string
	name       string // namespace/workload, plus [container] when it differs
	search     string // the full namespace/Kind/name [container], so search still finds a kind
	cpu, mem   string
	pressure   string
	withheld   bool
	detail     []string // the resource table, always shown when expanded
	evidence   []string // the sentences, shown when explain is on

	finding analyze.Finding
	plot    *tuiPlot // nil until fetched
}

type tuiPlot struct {
	cpu, mem metrics.Series
	err      error
}

func newTUIModel(r *report) *tuiModel {
	m := &tuiModel{
		source: r.Source, window: r.Window, missing: analyze.GapTexts(r.Missing), demo: r.Demo,
		expanded: map[int]bool{},
		usage:    r.Usage, usageNote: r.UsageNote, pending: -1,
	}
	for _, f := range r.Findings {
		var ev []string
		ev = append(ev, analyze.ReasonTexts(f.Reasons)...)
		for _, c := range f.Ceilings {
			mark := "ceiling"
			if c.Unresolved {
				mark = "ceiling?"
			}
			ev = append(ev, fmt.Sprintf("[%s %s] %s", mark, c.Kind, c.Note))
		}
		if len(f.Missing) > 0 {
			var names []string
			for _, s := range f.Missing {
				names = append(names, string(s))
			}
			ev = append(ev, "not measured for this container: "+strings.Join(names, ", "))
		}
		if len(ev) == 0 {
			ev = []string{"No caveats were recorded for this container."}
		}
		m.rows = append(m.rows, tuiRow{
			verdict:    string(f.Verdict),
			confidence: string(f.Confidence),
			name:       tuiName(f),
			search:     strings.ToLower(fmt.Sprintf("%s/%s [%s]", f.Namespace, f.Workload, f.Container)),
			cpu:        arrow(milli(f.CPURequest), milliRec(f.RecCPU)),
			mem:        arrow(human(f.MemRequest), memRec(f)),
			pressure:   pressureCell(f),
			withheld:   f.RecCPU == 0 && f.RecMem == 0,
			detail:     tuiDetail(f),
			evidence:   ev,
			finding:    f,
		})
	}
	m.summary = tuiSummary(r.Findings)
	return m
}

// tuiName is the row label in the same short form as the table view: no kind
// for a Deployment, and no container when it is named after the workload,
// which is most of them.
func tuiName(f analyze.Finding) string {
	name := f.Namespace + "/" + workloadCell(f.Workload, 1<<10)
	_, base, _ := strings.Cut(f.Workload, "/")
	if f.Container != base {
		name += " [" + f.Container + "]"
	}
	return name
}

func tuiSummary(fs []analyze.Finding) string {
	counts := map[analyze.Verdict]int{}
	for _, f := range fs {
		counts[f.Verdict]++
	}
	var parts []string
	for _, v := range analyze.ReportOrder() {
		if counts[v] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[v], v))
		}
	}
	s := totalSavings(fs)
	out := strings.Join(parts, ", ")
	if s.workloads > 0 {
		out += fmt.Sprintf("  ·  requests lowered %s CPU, %s mem", cpuDelta(-s.cpuMillicores), memDelta(-s.memBytes))
	}
	// The add-back figure is never folded into the reclaim figure. Summing only
	// the positive side is how the headline once came out optimistic by exactly
	// the amount the reader most needed to see.
	if s.addWorkloads > 0 {
		out += fmt.Sprintf("  ·  raised %s CPU, %s mem", cpuDelta(s.addCPUMillicores), memDelta(s.addMemBytes))
	}
	return out
}

// visible is the row indices passing the current filter and search, in order.
func (m *tuiModel) visible() []int {
	var out []int
	for i, r := range m.rows {
		if m.filter != "" && r.verdict != m.filter {
			continue
		}
		if m.search != "" && !strings.Contains(r.search, strings.ToLower(m.search)) {
			continue
		}
		out = append(out, i)
	}
	return out
}

const (
	ansiReset = "\x1b[0m"
	ansiDim   = "\x1b[2m"
	ansiBold  = "\x1b[1m"
	ansiRev   = "\x1b[7m"
)

// verdictColour is deliberately coarse. Severity is the verdict's job; a
// gradient here would invent precision the analysis does not have.
func verdictColour(v string) string {
	switch v {
	case "SATURATED":
		return "\x1b[31m" // red: needs more, now
	case "LEAKING":
		return "\x1b[35m" // magenta: sizing cannot fix it
	case "WASTE":
		return "\x1b[33m"
	case "IDLE":
		return "\x1b[34m"
	case "OK", "HEALTHY":
		return "\x1b[32m"
	default:
		return "\x1b[36m" // UNVERIFIED, BLOCKED, CONFLICT: tare declining to answer
	}
}

// lines renders the whole screen. It returns exactly height lines, already
// padded, so the caller can write them without clearing and without flicker.
func (m *tuiModel) lines(width, height int) []string {
	if width < 40 {
		width = 40
	}
	if height < 8 {
		height = 8
	}
	vis := m.visible()

	head := m.header(width)
	foot := m.footer(width, len(vis))
	body := height - len(head) - len(foot)
	if body < 1 {
		body = 1
	}

	// Build the body as a flat list of (rowIndex, text) so an expanded row's
	// evidence scrolls with it rather than being a separate pane that can
	// disagree with the selection.
	type line struct {
		text string
		sel  int // index into vis, -1 for evidence continuation lines
	}
	var flat []line
	for vi, ri := range vis {
		flat = append(flat, line{m.rowLine(m.rows[ri], width, vi == m.cursor, m.expanded[ri]), vi})
		if !m.expanded[ri] {
			continue
		}
		for _, d := range append(m.rows[ri].detail, m.plotLines(ri, width-13)...) {
			flat = append(flat, line{"      " + clip(d, width-7), -1})
		}
		if !m.explain {
			continue
		}
		for _, e := range m.rows[ri].evidence {
			for _, w := range wrap(e, width-8) {
				flat = append(flat, line{ansiDim + "      " + w + ansiReset, -1})
			}
		}
	}

	// Keep the cursor on screen. The offset is stored so paging does not jump
	// the view every time a row is expanded.
	cursorAt := 0
	for i, l := range flat {
		if l.sel == m.cursor {
			cursorAt = i
			break
		}
	}
	if cursorAt < m.offset {
		m.offset = cursorAt
	}
	if cursorAt >= m.offset+body {
		m.offset = cursorAt - body + 1
	}
	if m.offset > len(flat)-body {
		m.offset = len(flat) - body
	}
	if m.offset < 0 {
		m.offset = 0
	}

	out := head
	for i := m.offset; i < m.offset+body; i++ {
		if i < len(flat) {
			out = append(out, flat[i].text)
		} else {
			out = append(out, "")
		}
	}
	return append(out, foot...)
}

func (m *tuiModel) header(width int) []string {
	title := fmt.Sprintf(" tare  %s  ·  window %s  ·  %d containers", m.source, m.window, len(m.rows))
	out := []string{ansiBold + pad(clip(title, width), width) + ansiReset}

	if m.demo {
		out = append(out, "\x1b[33m"+pad(clip(" DEMO DATA — simulated workloads, no cluster was read", width), width)+ansiReset)
	}
	// A signal that is absent is not a signal that is zero, and an interactive
	// report that hides that behind a keystroke is worse than the table, which
	// at least prints it whether you wanted it or not.
	for _, g := range m.missing {
		out = append(out, "\x1b[31m"+pad(clip(" ! "+g, width), width)+ansiReset)
	}

	tabs := " [a]ll"
	for _, v := range []struct{ label, verdict string }{
		{"[s]aturated", "SATURATED"}, {"[w]aste", "WASTE"}, {"[i]dle", "IDLE"},
		{"[l]eaking", "LEAKING"}, {"[u]nverified", "UNVERIFIED"},
	} {
		label := " " + v.label
		if m.filter == v.verdict {
			label = ansiRev + label + ansiReset
		}
		tabs += label
	}
	if m.scanning {
		tabs += "   /" + m.search + "█"
	} else if m.search != "" {
		tabs += "   /" + m.search
	}
	out = append(out, pad(clip(tabs, width), width))
	return append(out, ansiDim+pad(clip("  VERDICT     CONF    WORKLOAD", width), width)+ansiReset)
}

func (m *tuiModel) footer(width, n int) []string {
	keys := " ↑↓ move   ⏎ details   e explain   / search   a s w i l u filter   q quit"
	if m.scanning {
		keys = " typing a filter — ⏎ accept, esc cancel"
	}
	return []string{
		ansiDim + pad(clip(" "+m.summary, width), width) + ansiReset,
		ansiRev + pad(clip(keys+fmt.Sprintf("   (%d shown)", n), width), width) + ansiReset,
	}
}

func (m *tuiModel) rowLine(r tuiRow, width int, selected, expanded bool) string {
	marker := "▸"
	if expanded {
		marker = "▾"
	}
	// Numbers are right-hand columns of fixed width so the eye can run down
	// them; the workload name takes whatever is left.
	//
	// rowFixed is every column except the name, counted exactly: a leading
	// space, the expander, a space, the verdict (10), a space, the confidence
	// (6), a space, a space before the numbers, and the two 16-wide number
	// columns with two spaces between them. Getting this wrong by one makes every
	// row wrap, which shifts the whole frame and leaves the previous screen
	// showing underneath.
	const rowFixed = 1 + 1 + 1 + 10 + 1 + 6 + 1 + 1 + 16 + 2 + 16
	nameW := width - rowFixed
	if nameW < 10 {
		nameW = 10
	}
	// padTo, not %-16s: Go's width verb counts bytes, and the arrow between the
	// current value and the recommendation is three bytes for one column. Every
	// number cell was under-padded by two per arrow, which walked the columns
	// out of alignment by a different amount on every row.
	// Two spaces between them: "2000m → withheld" is exactly 16 and ran into
	// the memory cell with one.
	nums := padTo(r.cpu, 16) + "  " + padTo(r.mem, 16)

	// The left of the row is measured and clipped BEFORE any styling is put on
	// it. Clipping a string that already contains escape sequences counts those
	// bytes as columns: the dim wrapper around a withheld cell is eight runes
	// that occupy no width, so the row lost eight real characters off its right
	// edge and the recommendation read "1.0Gi -> wit...".
	head := marker + " " + padTo(r.verdict, 10) + " " + padTo(r.confidence, 6) + " "
	budget := width - 1 - len([]rune(nums)) - 1
	if n := budget - len([]rune(head)); n > 0 {
		head += padTo(clip(r.name, n), n)
	}
	head = clip(head, max(budget, 0))

	body := head + " " + nums
	if r.withheld {
		// Dim the numbers only, and only after the widths are settled.
		body = head + " " + ansiDim + nums + ansiReset
	}

	line := " " + verdictColour(r.verdict) + body + ansiReset
	if selected {
		line = ansiRev + " " + pad(stripANSI(body), width-1) + ansiReset
	}
	return line
}

// runTUI takes over the terminal. It restores it on every exit path, including
// a panic: leaving a user's shell in raw mode with no cursor is the kind of
// thing that gets a tool uninstalled.
func runTUI(r *report) error {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return fmt.Errorf("--tui needs an interactive terminal; use -o table or -o html when redirecting")
	}
	state, err := term.MakeRaw(fd)
	if err != nil {
		return fmt.Errorf("cannot put the terminal in raw mode: %w", err)
	}
	restore := func() {
		fmt.Print("\x1b[?25h\x1b[?1049l") // cursor back, leave the alternate screen
		term.Restore(fd, state)
	}
	defer restore()
	defer func() {
		if p := recover(); p != nil {
			restore()
			panic(p)
		}
	}()

	fmt.Print("\x1b[?1049h\x1b[?25l") // alternate screen, hide cursor

	m := newTUIModel(r)
	buf := make([]byte, 16)
	for {
		w, h, err := term.GetSize(int(os.Stdout.Fd()))
		if err != nil || w <= 0 {
			w, h = 100, 30
		}
		fmt.Print("\x1b[H\x1b[2J")
		fmt.Print(strings.Join(m.lines(w, h), "\r\n"))

		n, err := os.Stdin.Read(buf)
		if err != nil || n == 0 {
			return nil
		}
		if quit := m.key(buf[:n]); quit {
			return nil
		}
		// The fetch runs after a frame that says it is running: a range query
		// through the API server proxy takes long enough to look like a hang.
		if m.pending >= 0 {
			fmt.Print("\x1b[H\x1b[2J")
			fmt.Print(strings.Join(m.lines(w, h), "\r\n"))
			m.fetchPending()
		}
	}
}

// fetchPending runs the query key() queued. Separate so a test can drive it.
func (m *tuiModel) fetchPending() {
	if m.pending < 0 {
		return
	}
	row := &m.rows[m.pending]
	cpu, mem, err := m.usage(row.finding)
	row.plot = &tuiPlot{cpu: cpu, mem: mem, err: err}
	m.pending = -1
}

// plotLines is the usage over the window for an expanded row: CPU and memory
// as two charts, never one with two scales.
func (m *tuiModel) plotLines(ri, width int) []string {
	r := m.rows[ri]
	switch {
	case m.usage == nil:
		if m.usageNote == "" {
			return nil
		}
		return []string{fmt.Sprintf("%-15s%s", "plot", m.usageNote)}
	case r.plot == nil:
		return []string{fmt.Sprintf("%-15s%s", "plot", "fetching usage over the window…")}
	case r.plot.err != nil:
		return []string{fmt.Sprintf("%-15scannot fetch: %v", "plot", r.plot.err)}
	}
	f := r.finding
	cores := func(m int64) float64 { return float64(m) / 1000 }
	recOnly := func(rec, cur int64) int64 {
		if rec == cur {
			return 0
		}
		return rec
	}
	return stackPlots(width,
		newPlot("cpu", r.plot.cpu, func(v float64) string { return fmt.Sprintf("%.0fm", v*1000) }, []plotMark{
			{"request", cores(f.CPURequest)}, {"limit", cores(f.CPULimit)}, {"rec", cores(recOnly(f.RecCPU, f.CPURequest))},
		}),
		newPlot("memory", r.plot.mem, usageBytes, []plotMark{
			{"request", float64(f.MemRequest)}, {"limit", float64(f.MemLimit)}, {"rec", float64(recOnly(f.RecMem, f.MemRequest))},
		}))
}

// key applies one input event and reports whether to quit. It is separate from
// the loop so the whole interaction can be driven from a test.
func (m *tuiModel) key(b []byte) bool {
	vis := m.visible()

	// An escape sequence: arrow keys arrive as ESC [ A..D.
	if len(b) >= 3 && b[0] == 0x1b && b[1] == '[' {
		switch b[2] {
		case 'A':
			m.move(-1, vis)
		case 'B':
			m.move(1, vis)
		case '5':
			m.move(-10, vis)
		case '6':
			m.move(10, vis)
		}
		return false
	}

	if m.scanning {
		switch b[0] {
		case 0x1b: // esc: abandon the search, not just the typing
			m.scanning, m.search = false, ""
		case '\r', '\n':
			m.scanning = false
		case 0x7f, 0x08:
			if m.search != "" {
				m.search = m.search[:len(m.search)-1]
			}
		default:
			if b[0] >= 0x20 && b[0] < 0x7f {
				m.search += string(b[:1])
			}
		}
		m.cursor, m.offset = 0, 0
		return false
	}

	switch b[0] {
	case 'q', 0x03: // q, Ctrl-C
		return true
	case 'j':
		m.move(1, vis)
	case 'k':
		m.move(-1, vis)
	case 'g':
		m.cursor, m.offset = 0, 0
	case 'G':
		m.cursor = len(vis) - 1
	case '\r', '\n', ' ':
		if m.cursor < len(vis) {
			ri := vis[m.cursor]
			m.expanded[ri] = !m.expanded[ri]
			if m.expanded[ri] && m.rows[ri].plot == nil && m.usage != nil {
				m.pending = ri
			}
		}
	case 'e':
		m.explain = !m.explain
	case '/':
		m.scanning, m.search = true, ""
	case 'a':
		m.setFilter("")
	case 's':
		m.setFilter("SATURATED")
	case 'w':
		m.setFilter("WASTE")
	case 'i':
		m.setFilter("IDLE")
	case 'l':
		m.setFilter("LEAKING")
	case 'u':
		m.setFilter("UNVERIFIED")
	}
	return false
}

// setFilter resets the cursor: keeping an index across a filter change selects
// a different workload than the one that was highlighted, which is how an
// operator ends up reading the evidence for the wrong container.
func (m *tuiModel) setFilter(v string) {
	if m.filter == v {
		v = "" // pressing the active filter again clears it
	}
	m.filter, m.cursor, m.offset = v, 0, 0
}

func (m *tuiModel) move(d int, vis []int) {
	m.cursor += d
	if m.cursor < 0 {
		m.cursor = 0
	}
	if m.cursor >= len(vis) {
		m.cursor = len(vis) - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n < 2 {
		return string(r[:n])
	}
	return string(r[:n-1]) + "…"
}

// padTo pads to n display columns, counting runes rather than bytes.
func padTo(s string, n int) string {
	if d := n - len([]rune(s)); d > 0 {
		return s + strings.Repeat(" ", d)
	}
	return s
}

func pad(s string, n int) string {
	if d := n - len([]rune(stripANSI(s))); d > 0 {
		return s + strings.Repeat(" ", d)
	}
	return s
}

func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			for i < len(s) && s[i] != 'm' {
				i++
			}
			i++
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// wrap breaks text on spaces so a long reason does not depend on the
// terminal's own wrapping, which would break the line accounting above.
func wrap(s string, width int) []string {
	if width < 20 {
		width = 20
	}
	var out []string
	line := ""
	for _, word := range strings.Fields(s) {
		switch {
		case line == "":
			line = word
		case len(line)+1+len(word) <= width:
			line += " " + word
		default:
			out = append(out, line)
			line = word
		}
	}
	if line != "" {
		out = append(out, line)
	}
	return out
}
