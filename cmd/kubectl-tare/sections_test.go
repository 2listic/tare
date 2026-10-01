package main

import (
	"bufio"
	"bytes"
	"io"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/mfranzon/tare/internal/analyze"
	"github.com/mfranzon/tare/internal/ceilings"
	"github.com/mfranzon/tare/internal/metrics"
	"github.com/mfranzon/tare/internal/packing"
)

// capture runs f with stdout redirected, and returns what it printed.
func capture(t *testing.T, f func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stdout
	os.Stdout = w
	sectionOpen = false
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	f()
	w.Close()
	os.Stdout = saved
	return <-done
}

func sampleReport() *report {
	return &report{
		Source: "simulator", Window: "2w", Demo: true,
		Findings: []analyze.Finding{{
			Namespace: "prod", Workload: "Deployment/checkout", Container: "api",
			Verdict: analyze.Saturated, Confidence: analyze.Medium, Pods: 6,
			CPURequest: 50, CPULimit: 100, MemRequest: 1 << 24, MemLimit: 1 << 24,
			RecCPU: 115, RecCPULimit: 125, RecMem: 1 << 24, RecMemLimit: 1 << 24,
			Reasons: []analyze.Reason{
				{Code: analyze.OOMKilled, Text: "container has been OOMKilled; memory must not be reduced"},
				{Code: analyze.CPUPressure, Observed: 0.9, Against: 0.1, Unit: analyze.SecPerSec,
					Text: "CPU pressure 0.900 s/s exceeds 0.100 while using 98% of its allocation"},
			},
			Missing:  []metrics.Signal{metrics.SigMemTrend},
			Ceilings: []ceilings.Ceiling{{Kind: ceilings.JVMHeapMax, Note: "capped at 256Mi"}},
		}},
		Missing: []analyze.SignalGap{
			{Signal: metrics.SigPSI, Status: analyze.GapAbsent, Detail: "not exported by this cluster"},
		},
		Commitment: []packing.Commitment{{Node: "ip-10-0-3-41", Pods: 14, Schedulable: true}},
		NodeNote:   "node packing is not modelled in demo mode",
	}
}

// packedReport exercises the sections a demo run never reaches: a node delta
// that was actually computed, the pin breakdown under it, and the budgets that
// can make all of it unactionable.
func packedReport() *report {
	r := sampleReport()
	r.NodeNote = ""
	r.Nodes = packing.Result{
		Trustworthy: true, NodesRunning: 12, NodesPackedNow: 11, NodesPackedRecommended: 9,
		EmptyToday: []string{"ip-10-0-7-2"},
		Removable:  []string{"ip-10-0-3-41", "ip-10-0-5-12", "ip-10-0-7-2"},
		Pinned:     38, Movable: 176,
		PinReasons: map[string]int{
			"has a PersistentVolumeClaim, which may bind it to a zone or a node": 21,
			"has affinity or anti-affinity rules":                                11,
			"requests an extended resource such as a GPU":                        6,
		},
		ExcludedNode: 1,
		Unusable:     []string{"ip-10-0-9-7: taints no pod in this report tolerates"},
	}
	r.DrainBlockers = []string{"prod/checkout allows no disruption (minAvailable equals replicas)"}
	return r
}

func renderTable(t *testing.T, r *report, explain bool) string {
	t.Helper()
	return renderFormat(t, r, "wide", explain)
}

func renderFormat(t *testing.T, r *report, format string, explain bool) string {
	t.Helper()
	return capture(t, func() {
		if err := emit(r, format, explain); err != nil {
			t.Fatal(err)
		}
	})
}

// The whole point of the rewrite: stdout is a sequence of tables a script can
// read without knowing anything about tare. A section whose rows do not line up
// with its header is worse than prose, because it parses and is wrong.
func TestEverySectionIsAWellFormedTable(t *testing.T) {
	for _, tc := range []struct {
		name string
		r    *report
		want []string
	}{
		{"demo", sampleReport(), []string{"meta", "signals", "findings", "evidence", "totals", "nodes", "commitment"}},
		{"packed", packedReport(), []string{"verdicts", "totals", "nodes", "node_caveats", "node_pins", "drain_blockers", "commitment"}},
	} {
		t.Run(tc.name, func(t *testing.T) { checkTableShape(t, renderFormat(t, tc.r, "wide", false), tc.want) })
	}
}

func checkTableShape(t *testing.T, out string, wantSections []string) {
	t.Helper()

	gap := regexp.MustCompile(` {2,}`)
	var section string
	var want int
	seen := map[string]bool{}

	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			section, want = "", 0
			continue
		case strings.HasPrefix(line, "# section="):
			section = strings.TrimPrefix(line, "# section=")
			seen[section] = true
			want = 0
			continue
		case strings.HasPrefix(line, "#"):
			continue // a comment is allowed anywhere and is never data
		}
		if section == "" {
			t.Errorf("data line outside any section: %q", line)
			continue
		}
		fields := gap.Split(strings.TrimRight(line, " "), -1)
		if want == 0 {
			want = len(fields) // the header sets the shape
			continue
		}
		if len(fields) != want {
			t.Errorf("section %s: %d fields, header has %d: %q", section, len(fields), want, line)
		}
		for i, f := range fields {
			if strings.TrimSpace(f) == "" {
				t.Errorf("section %s: field %d is blank, should be \"-\": %q", section, i, line)
			}
		}
	}
	for _, want := range wantSections {
		if !seen[want] {
			t.Errorf("section %s is missing from the report", want)
		}
	}
}

// Prose in the default output is the thing that was removed. A reason's
// sentence reappearing in a table cell would put it back one release later
// without anyone noticing, because nothing else would break.
func TestDefaultOutputCarriesNoSentences(t *testing.T) {
	out := renderTable(t, sampleReport(), false)
	for _, prose := range []string{
		"memory must not be reduced",
		"while using 98% of its allocation",
		"capped at 256Mi",
	} {
		if strings.Contains(out, prose) {
			t.Errorf("the default table output still prints the sentence %q; it belongs behind --explain", prose)
		}
	}
	// The codes, however, must be there: they are what replaced it.
	for _, code := range []string{"OOMKILLED", "CPU_PRESSURE", "SIGNAL_NOT_MEASURED", "CEILING"} {
		if !strings.Contains(out, code) {
			t.Errorf("evidence code %s is missing from the report", code)
		}
	}
}

// --explain is the whole reason dropping the prose was safe.
func TestExplainRestoresTheSentences(t *testing.T) {
	out := renderTable(t, sampleReport(), true)
	for _, prose := range []string{
		"container has been OOMKilled; memory must not be reduced",
		"while using 98% of its allocation",
		"capped at 256Mi",
	} {
		if !strings.Contains(out, prose) {
			t.Errorf("--explain did not print %q", prose)
		}
	}
}

// A measured figure and an absent one must not render alike. "0" for a reason
// that carries no comparand reads as a measurement of zero, which for a
// threshold is the most reassuring number there is.
func TestAbsentMeasurementsRenderAsDash(t *testing.T) {
	if got := evidenceValue(0, analyze.Millicores); got != "-" {
		t.Errorf("an absent measurement rendered as %q, want \"-\"", got)
	}
	if got := evidenceValue(0.9, analyze.SecPerSec); got != "0.9" {
		t.Errorf("a pressure reading rendered as %q", got)
	}
	if got := evidenceValue(1<<30, analyze.Bytes); got != "1.0Gi" {
		t.Errorf("a byte figure rendered as %q", got)
	}
}

// The evidence table is only worth anything if the codes are stable. This
// pins the ones a script would plausibly filter on.
func TestCodesAreStable(t *testing.T) {
	for code, want := range map[analyze.Code]string{
		analyze.OOMKilled:        "OOMKILLED",
		analyze.MemLeak:          "MEM_LEAK",
		analyze.IdleWindow:       "IDLE_NO_WORK",
		analyze.CPUUnderused:     "CPU_UNDERUSED",
		analyze.MemUnderused:     "MEM_UNDERUSED",
		analyze.RestartChurn:     "RESTART_CHURN",
		analyze.CPUThrottled:     "CPU_THROTTLED",
		analyze.VPAManaged:       "VPA_MANAGES_WORKLOAD",
		analyze.UnschedulableCPU: "UNSCHEDULABLE_CPU",
	} {
		if string(code) != want {
			t.Errorf("code renamed to %q, was %q: a filter on the old name now silently matches nothing", code, want)
		}
	}
}

// renderCompact returns the default view's stdout and stderr separately,
// because which one a line lands on is the point of it.
func renderCompact(t *testing.T, r *report) (stdout, stderr string) {
	t.Helper()
	var buf bytes.Buffer
	saved := diag
	diag = &buf
	defer func() { diag = saved }()
	stdout = renderFormat(t, r, "table", false)
	return stdout, buf.String()
}

// stdout is the findings table and nothing else, in the same contract as
// -o wide. It is the one people will pipe first.
func TestCompactStdoutIsOnlyFindings(t *testing.T) {
	for _, r := range []*report{sampleReport(), packedReport()} {
		out, _ := renderCompact(t, r)
		checkTableShape(t, out, []string{"findings"})
		if n := strings.Count(out, "# section="); n != 1 {
			t.Errorf("stdout has %d sections, want only findings:\n%s", n, out)
		}
	}
}

// Shortening the report must not shorten what it admits it could not see.
// Every gap the wide view carries has to survive into the compact one, or the
// compact view is a partial run presented as a complete one.
func TestCompactKeepsEveryGap(t *testing.T) {
	r := packedReport()
	r.Commitment = []packing.Commitment{{Node: "n1", Pods: 3, Schedulable: true, LimMem: 2, AllocMem: 1}}
	_, out := renderCompact(t, r)
	for _, want := range []string{
		"SIGNAL_ABSENT", "psi",
		"SIGNAL_NOT_MEASURED", "memory-trend: 1 of 1",
		"PODS_PINNED", "NODE_UNUSABLE", "DRAIN_BLOCKED", "MEM_OVERCOMMIT",
		"freed >=2", // pinned pods make the freed count a lower bound
	} {
		if !strings.Contains(out, want) {
			t.Errorf("compact output lost %q:\n%s", want, out)
		}
	}
}

// One row per signal, however many containers lack it. The first table view
// printed SIGNAL_NOT_MEASURED once per container, 45 of 140 evidence rows on
// the lab cluster, all saying the same thing.
func TestCompactAggregatesPerContainerGaps(t *testing.T) {
	r := sampleReport()
	f := r.Findings[0]
	for i := 0; i < 5; i++ {
		r.Findings = append(r.Findings, f)
	}
	_, out := renderCompact(t, r)
	if n := strings.Count(out, "SIGNAL_NOT_MEASURED"); n != 1 {
		t.Errorf("SIGNAL_NOT_MEASURED printed %d times, want once:\n%s", n, out)
	}
	if !strings.Contains(out, "memory-trend: 6 of 6 containers") {
		t.Errorf("the aggregate row does not say how many containers it covers:\n%s", out)
	}
}

func TestCompactHidesOnlyWhatAsksNothing(t *testing.T) {
	r := sampleReport()
	ok := r.Findings[0]
	ok.Verdict, ok.Container = analyze.Healthy, "sidecar"
	idle := r.Findings[0]
	idle.Verdict, idle.Container = analyze.Idle, "dormant"
	idle.Reasons = []analyze.Reason{{Code: analyze.IdleWindow}, {Code: analyze.IdleCaveat}, {Code: analyze.CPUFloor}}
	r.Findings = append(r.Findings, ok, idle)

	out, stderr := renderCompact(t, r)
	if strings.Contains(out, "sidecar") {
		t.Errorf("an OK row is listed by default:\n%s", out)
	}
	if !strings.Contains(stderr, "not listed: 1 OK") {
		t.Errorf("the hidden OK row is not counted:\n%s", stderr)
	}
	// The caveat is the reason an idle container is not simply deleted.
	if !strings.Contains(out, "IDLE_NO_WORK,IDLE_NOT_RULED_OUT") {
		t.Errorf("the idle caveat is missing from WHY:\n%s", out)
	}
	if strings.Contains(out, "CPU_FLOOR") {
		t.Errorf("an implied code made it into WHY:\n%s", out)
	}

	r.VerdictFiltered = true
	if out, _ := renderCompact(t, r); !strings.Contains(out, "sidecar") {
		t.Errorf("--verdict did not bring the OK row back:\n%s", out)
	}
}

func TestCompactCarriesNoSentences(t *testing.T) {
	out, _ := renderCompact(t, sampleReport())
	for _, prose := range []string{"memory must not be reduced", "while using 98% of its allocation", "capped at 256Mi"} {
		if strings.Contains(out, prose) {
			t.Errorf("the compact output prints the sentence %q", prose)
		}
	}
	for _, code := range []string{"OOMKILLED", "CPU_PRESSURE"} {
		if !strings.Contains(out, code) {
			t.Errorf("code %s is missing from WHY:\n%s", code, out)
		}
	}
}

// An empty result is a header and no rows. A placeholder row counts as a
// finding to anything that does `tail -n +3 | wc -l`.
func TestCompactEmptyHasNoRows(t *testing.T) {
	r := sampleReport()
	r.Findings = nil
	out, _ := renderCompact(t, r)
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 2 {
		t.Errorf("empty findings printed %d lines, want section marker and header only:\n%s", len(lines), out)
	}
}

// The first node section printed every node empty after the recommendations
// beside the count tare freed, so a node that repacking alone empties was
// named as tare's saving. Each stage lists what is empty at that stage, and
// the node the cluster's own slack frees is on the cluster_slack row.
func TestNodesSectionCreditsEachStage(t *testing.T) {
	out := renderTable(t, packedReport(), false)
	rows := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Fields(line); len(f) > 0 {
			rows[f[0]] = line
		}
	}
	today, after := rows["packed_today"], rows["packed_after"]
	if !strings.Contains(today, "cluster_slack") || !strings.Contains(today, "ip-10-0-7-2") {
		t.Errorf("the consolidation node is not on the cluster_slack row: %q", today)
	}
	if strings.Contains(today, "ip-10-0-3-41") {
		t.Errorf("a node only the recommendations empty is on the cluster_slack row: %q", today)
	}
	if !strings.Contains(after, " >=2 ") || !strings.Contains(after, "tare") {
		t.Errorf("tare's row should carry FREED >=2 (pinned pods make it a lower bound): %q", after)
	}
	for _, gone := range []string{"nodes.from_resizing", "reclaimable.", "addback.", "HUMAN"} {
		if strings.Contains(out, gone) {
			t.Errorf("the old key %q is still printed", gone)
		}
	}
}

// Held values beside OOMKILLED read as an answer. Every view that prints a
// memory recommendation has to print "?" for one tare does not know.
func TestCensoredMemoryRendersAsUnknown(t *testing.T) {
	r := sampleReport()
	r.Findings[0].MemCensored = true
	r.Findings[0].Reasons = append(r.Findings[0].Reasons, analyze.Reason{Code: analyze.MemDemandCensored})
	compact, stderr := renderCompact(t, r)
	if !strings.Contains(compact, "16Mi→?") {
		t.Errorf("the compact row does not show the memory as unknown:\n%s", compact)
	}
	if !strings.Contains(stderr, "memory unknown on 1") {
		t.Errorf("the summary does not count the unknown memory:\n%s", stderr)
	}
	wide := renderTable(t, r, false)
	if !strings.Contains(wide, "16Mi>?") || !strings.Contains(wide, "MEM_DEMAND_CENSORED") {
		t.Errorf("the wide findings row does not show the memory as unknown, or the code is missing:\n%s", wide)
	}
	if !regexp.MustCompile(`(?m)^unknown +1 +- +\? +MEM_DEMAND_CENSORED`).MatchString(wide) {
		t.Errorf("totals has no unknown row:\n%s", wide)
	}
	v := buildHTMLView(r)
	if !strings.HasSuffix(v.Rows[0].Mem, "?") || !strings.HasSuffix(v.Rows[0].MemLimit, "?") {
		t.Errorf("HTML shows %q / %q, want both unknown", v.Rows[0].Mem, v.Rows[0].MemLimit)
	}
}
