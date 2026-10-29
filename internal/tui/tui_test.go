package tui

import (
	"strings"
	"testing"

	"github.com/2listic/tare/internal/analyze"
	"github.com/2listic/tare/internal/render"
	"github.com/2listic/tare/internal/report"
)

// The tests use the same two-week window the CLI defaults to. A shorter one is
// faster and changes the verdicts - at 48h the leakers have not yet grown
// enough to be reported LEAKING - so a test written against it would be
// asserting on a different report than the one anybody runs.
const testWindow = 14 * 24 * 3600e9

func demoModel(t *testing.T) *tuiModel {
	t.Helper()
	return newTUIModel(report.Demo(testWindow, render.HumanDuration(testWindow)))
}

func frame(m *tuiModel, w, h int) string {
	return render.StripANSI(strings.Join(m.lines(w, h), "\n"))
}

// TestTUIFrameFits is the property the whole layout rests on: the caller writes
// the frame without clearing between rows, so a frame that is the wrong height
// leaves the previous screen's rows visible under the new one, and one that is
// too wide wraps and shifts everything below it.
func TestTUIFrameFits(t *testing.T) {
	m := demoModel(t)
	for _, size := range [][2]int{{80, 24}, {120, 40}, {200, 60}, {40, 10}} {
		w, h := size[0], size[1]
		ls := m.lines(w, h)
		if len(ls) != h {
			t.Errorf("%dx%d: rendered %d lines", w, h, len(ls))
		}
		for i, l := range ls {
			if n := len([]rune(render.StripANSI(l))); n > w {
				t.Errorf("%dx%d: line %d is %d columns wide: %q", w, h, i, n, render.StripANSI(l))
			}
		}
	}
}

// TestTUIEvidenceIsHiddenUntilAsked is the reason this mode exists. `-v` prints
// the evidence for every row at once, which on a real cluster is a log rather
// than a report. Expanding a row shows data - the resources, what was measured,
// the codes - and the sentences only on 'e'.
func TestTUIEvidenceIsHiddenUntilAsked(t *testing.T) {
	m := demoModel(t)
	// Short enough to survive the evidence wrapper, which breaks on spaces.
	const sentence = "unbounded growth"

	if strings.Contains(frame(m, 120, 40), "RECOMMENDED") {
		t.Fatal("detail is on screen before anything was expanded")
	}
	// Move to the first LEAKING row and open it.
	m.setFilter("LEAKING")
	m.key([]byte{'\r'})
	f := frame(m, 120, 40)
	for _, want := range []string{"RECOMMENDED", "mem request", "MEM_LEAK"} {
		if !strings.Contains(f, want) {
			t.Errorf("expanding a row did not show %q", want)
		}
	}
	if strings.Contains(f, sentence) {
		t.Error("expanding a row printed the prose; it belongs behind 'e'")
	}
	m.key([]byte{'e'})
	if !strings.Contains(frame(m, 120, 40), sentence) {
		t.Error("'e' did not bring the sentences back")
	}
	m.key([]byte{'\r'})
	if strings.Contains(frame(m, 120, 40), "RECOMMENDED") {
		t.Error("collapsing a row did not hide its detail again")
	}
}

// A signal that was not measured renders as "?", never as a zero.
func TestTUIDetailMarksUnmeasured(t *testing.T) {
	d := strings.Join(tuiDetail(analyze.Finding{CPURequest: 100, MemRequest: 1 << 28}), "\n")
	for _, want := range []string{"psi ?", "throttle ?", "oom ?", "restarts ?"} {
		if !strings.Contains(d, want) {
			t.Errorf("unmeasured signal not marked: want %q in\n%s", want, d)
		}
	}
}

// The GPU line is there for a GPU holder only, and an unmeasured GPU is "?"
// like any other signal.
func TestTUIDetailGPU(t *testing.T) {
	d := strings.Join(tuiDetail(analyze.Finding{GPUs: 2, GPUResource: "nvidia.com/gpu"}), "\n")
	if !strings.Contains(d, "2 nvidia.com/gpu   idle ?   busy ?   sm ?   vram ?") {
		t.Errorf("unmeasured GPU not marked:\n%s", d)
	}
	if d := strings.Join(tuiDetail(analyze.Finding{CPURequest: 100}), "\n"); strings.Contains(d, "gpu") {
		t.Errorf("a container with no GPU has a gpu line:\n%s", d)
	}
}

// TestTUIWithheldLooksWithheld. Same rule as the HTML report: the refusal to
// give a number is the product, and rendering it as a blank or a zero turns the
// most honest thing in the report into a gap in the data.
func TestTUIWithheldLooksWithheld(t *testing.T) {
	m := newTUIModel(&report.Report{Findings: []analyze.Finding{{
		Namespace: "prod", Workload: "Deployment/api", Container: "api",
		Verdict: analyze.Leaking, Confidence: analyze.High,
		CPURequest: 500, MemRequest: 1 << 30,
	}}})
	out := frame(m, 120, 20)
	if strings.Count(out, "withheld") != 2 {
		t.Errorf("expected both resources to read withheld:\n%s", out)
	}
}

// TestTUIShowsMissingSignals: an interactive report that hides a dead signal
// behind a keystroke is worse than the table, which prints it whether you
// wanted it or not.
func TestTUIShowsMissingSignals(t *testing.T) {
	m := newTUIModel(&report.Report{
		Findings: []analyze.Finding{{Namespace: "p", Workload: "Deployment/a", Container: "a", Verdict: analyze.Waste, RecCPU: 10, RecMem: 1 << 20}},
		Missing:  []analyze.SignalGap{{Signal: "psi", Status: analyze.GapAbsent, Detail: "not exported by this cluster"}},
	})
	if !strings.Contains(frame(m, 120, 20), "psi: not exported") {
		t.Error("a missing signal is not on screen without pressing anything")
	}
}

// TestTUIDemoSaysSo. The banner has to be unmissable in the mode most likely to
// be screen-shared.
func TestTUIDemoSaysSo(t *testing.T) {
	if !strings.Contains(frame(demoModel(t), 120, 30), "DEMO DATA") {
		t.Error("the demo banner is missing")
	}
	m := newTUIModel(&report.Report{Findings: []analyze.Finding{{Namespace: "p", Workload: "Deployment/a", Container: "a", Verdict: analyze.Waste}}})
	if strings.Contains(frame(m, 120, 20), "DEMO DATA") {
		t.Error("a real report claims to be demo data")
	}
}

// TestTUIFilterResetsTheCursor. Keeping the index across a filter change points
// the cursor at a different workload than the one that was highlighted, which
// is how someone reads the evidence for the wrong container and acts on it.
func TestTUIFilterResetsTheCursor(t *testing.T) {
	m := demoModel(t)
	for range 6 {
		m.key([]byte{'j'})
	}
	if m.cursor == 0 {
		t.Fatal("precondition: the cursor did not move")
	}
	m.key([]byte{'w'})
	if m.cursor != 0 {
		t.Errorf("cursor survived a filter change at index %d", m.cursor)
	}
	vis := m.visible()
	if len(vis) == 0 {
		t.Fatal("the WASTE filter matched nothing")
	}
	for _, i := range vis {
		if m.rows[i].verdict != "WASTE" {
			t.Errorf("the WASTE filter kept a %s row", m.rows[i].verdict)
		}
	}
	// Pressing the active filter again clears it.
	m.key([]byte{'w'})
	if len(m.visible()) != len(m.rows) {
		t.Error("pressing the active filter twice did not clear it")
	}
}

// TestTUICursorStaysOnScreen. The selection is rendered in reverse video and
// nothing else is, so a cursor scrolled out of the viewport is a screen with no
// selection on it at all.
func TestTUICursorStaysOnScreen(t *testing.T) {
	m := demoModel(t)
	for i := range m.rows {
		m.expanded[i] = true // the worst case: every row is tall
	}
	for _, press := range []byte{'G', 'g', 'j', 'j', 'j', 'G'} {
		m.key([]byte{press})
		vis := m.visible()
		want := m.rows[vis[m.cursor]].name
		if !strings.Contains(frame(m, 120, 24), render.Clip(want, 120-2-11-7-34)) {
			t.Errorf("after %q the selected row %q is off screen", press, want)
		}
	}
}

// TestTUISearchFiltersByName covers the / key, including that abandoning the
// search restores every row rather than leaving a hidden filter applied.
func TestTUISearchFiltersByName(t *testing.T) {
	m := demoModel(t)
	m.key([]byte{'/'})
	for _, c := range []byte("jvm") {
		m.key([]byte{c})
	}
	m.key([]byte{'\r'})
	vis := m.visible()
	if len(vis) == 0 || len(vis) == len(m.rows) {
		t.Fatalf("search matched %d of %d rows", len(vis), len(m.rows))
	}
	for _, i := range vis {
		if !strings.Contains(m.rows[i].name, "jvm") {
			t.Errorf("search kept %q", m.rows[i].name)
		}
	}
	m.key([]byte{'/'})
	m.key([]byte{0x1b})
	if len(m.visible()) != len(m.rows) {
		t.Error("escaping the search left a filter applied")
	}
}

// TestTUIQuitKeys. A tool that takes over the terminal and cannot be left with
// q or Ctrl-C is a tool people kill from another window.
func TestTUIQuitKeys(t *testing.T) {
	for _, b := range []byte{'q', 0x03} {
		m := demoModel(t)
		if !m.key([]byte{b}) {
			t.Errorf("%q did not quit", b)
		}
	}
	m := demoModel(t)
	if m.key([]byte{'/'}); !m.scanning {
		t.Fatal("precondition: / did not start a search")
	}
	if m.key([]byte{'q'}) {
		t.Error("q quit the program while typing a search instead of typing a q")
	}
}

// TestTUINotATerminal: redirected output must fail with advice rather than
// emitting escape sequences into a file.
func TestTUINotATerminal(t *testing.T) {
	err := Run(&report.Report{})
	if err == nil {
		t.Skip("stdin is a terminal in this environment")
	}
	if !strings.Contains(err.Error(), "-o table") {
		t.Errorf("the error does not say what to use instead: %v", err)
	}
}

func TestTUINameIsShort(t *testing.T) {
	for _, tc := range []struct {
		workload, container, want string
	}{
		{"Deployment/web", "web", "p/web"},
		{"Deployment/web", "sidecar", "p/web [sidecar]"},
		{"StatefulSet/db", "db", "p/sts/db"},
	} {
		if got := tuiName(analyze.Finding{Namespace: "p", Workload: tc.workload, Container: tc.container}); got != tc.want {
			t.Errorf("%s [%s]: got %q, want %q", tc.workload, tc.container, got, tc.want)
		}
	}
	// Search still sees the kind the label dropped.
	m := newTUIModel(&report.Report{Findings: []analyze.Finding{{Namespace: "p", Workload: "Deployment/web", Container: "web"}}})
	m.search = "deployment"
	if len(m.visible()) != 1 {
		t.Error("searching for the kind no longer finds a Deployment")
	}
}

// Expanding a row queues the usage query instead of running it inside key(),
// so the frame before the fetch can say a fetch is running.
func TestTUIPlotFetchesOnExpand(t *testing.T) {
	m := demoModel(t)
	m.setFilter("LEAKING")
	m.key([]byte{'\r'})
	if f := frame(m, 120, 60); !strings.Contains(f, "fetching usage") {
		t.Fatalf("no fetching line before the query ran:\n%s", f)
	}
	m.fetchPending()
	f := frame(m, 120, 60)
	for _, want := range []string{"cpu            peak", "memory         peak", "-2w", "now"} {
		if !strings.Contains(f, want) {
			t.Errorf("expanded row is missing %q:\n%s", want, f)
		}
	}
	if m.pending != -1 {
		t.Errorf("pending = %d after the fetch", m.pending)
	}
}

// The frame contract holds with a plot open, at every size including the one
// too narrow to draw it.
func TestTUIFrameFitsWithPlot(t *testing.T) {
	m := demoModel(t)
	m.key([]byte{'\r'})
	m.fetchPending()
	for _, size := range [][2]int{{80, 24}, {120, 40}, {200, 60}, {40, 10}} {
		w, h := size[0], size[1]
		ls := m.lines(w, h)
		if len(ls) != h {
			t.Errorf("%dx%d: rendered %d lines", w, h, len(ls))
		}
		for i, l := range ls {
			if n := len([]rune(render.StripANSI(l))); n > w {
				t.Errorf("%dx%d: line %d is %d columns wide: %q", w, h, i, n, render.StripANSI(l))
			}
		}
	}
}

// Redaction hashes the names the query is keyed by. The row says so rather
// than showing an error, or a plot of nothing.
func TestTUIPlotUnderRedact(t *testing.T) {
	r := report.Demo(testWindow, render.HumanDuration(testWindow))
	report.Redact(r)
	m := newTUIModel(r)
	m.key([]byte{'\r'})
	if m.pending != -1 {
		t.Fatal("a redacted report queued a usage query")
	}
	if f := frame(m, 120, 60); !strings.Contains(f, "not drawn under --redact") {
		t.Errorf("redacted row does not say why there is no plot:\n%s", f)
	}
}

// G on an empty view set the cursor to -1, and Enter then indexed vis[-1].
func TestTUIEmptyViewDoesNotPanic(t *testing.T) {
	m := demoModel(t)
	m.key([]byte{'/'})
	for _, c := range "no-such-workload" {
		m.key([]byte{byte(c)})
	}
	m.key([]byte{'\r'})
	for _, k := range []byte{'G', '\r', 'k', '\r', 'j', ' ', 'g', '\n'} {
		m.key([]byte{k})
	}
	if m.cursor < 0 {
		t.Errorf("cursor = %d on an empty view", m.cursor)
	}
	_ = frame(m, 80, 24)
}
