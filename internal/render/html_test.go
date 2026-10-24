package render

import (
	"bytes"
	"io"
	"regexp"
	"strings"
	"testing"

	"github.com/2listic/tare/internal/analyze"
	"github.com/2listic/tare/internal/metrics"
	"github.com/2listic/tare/internal/packing"
	"github.com/2listic/tare/internal/report"
)

func renderHTML(t *testing.T, r *report.Report) string {
	t.Helper()
	var b bytes.Buffer
	if err := writeHTML(&b, r); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// TestHTMLIsSelfContained is the property that makes this format worth having.
// The report is for someone who does not have access to the cluster it
// describes: it gets attached to a ticket, opened from a downloads folder, read
// on a plane. Any external reference makes it a page that sometimes renders.
func TestHTMLIsSelfContained(t *testing.T) {
	out := renderHTML(t, report.Demo(48*3600e9, HumanDuration(48*3600e9)))

	for _, pat := range []string{`<script[^>]+src=`, `<link[^>]+href=`, `@import`, `https?://`} {
		if re := regexp.MustCompile(pat); re.MatchString(out) {
			t.Errorf("report references something outside itself: %s", re.FindString(out))
		}
	}
}

// TestWithheldRecommendationLooksWithheld is the rule that separates this from
// a dashboard. tare withholds a number whenever it cannot stand behind one, and
// that refusal is the product. Rendering it as an empty cell or a zero turns
// the one honest thing in the report into a gap in the data.
func TestWithheldRecommendationLooksWithheld(t *testing.T) {
	r := &report.Report{Findings: []analyze.Finding{{
		Namespace: "prod", Workload: "Deployment/api", Container: "api",
		Verdict: analyze.Leaking, Confidence: analyze.High,
		CPURequest: 500, MemRequest: 1 << 30,
		Reasons: []analyze.Reason{{Code: analyze.MemLeak, Text: "memory grew from 200MiB to 3.1GiB across the window"}},
		// No RecCPU, no RecMem: the recommendation was deliberately withheld.
	}}}
	out := renderHTML(t, r)

	if !strings.Contains(out, "withheld") {
		t.Error("a withheld recommendation is not described as withheld anywhere in the page")
	}
	// The arrow's target is the recommendation. A zero there reads as a
	// measurement of nothing rather than as a refusal to answer; the source
	// side may legitimately be "unset" or "0m", which is a fact about the
	// manifest.
	targets := regexp.MustCompile(`\x{2192} ([^<]+)<`).FindAllStringSubmatch(out, -1)
	if len(targets) != 4 {
		t.Fatalf("expected 4 recommendation cells, got %d", len(targets))
	}
	for _, m := range targets {
		if strings.TrimSpace(m[1]) != "withheld" {
			t.Errorf("a withheld recommendation rendered as %q", m[1])
		}
	}
	if !strings.Contains(out, "memory grew from 200MiB to 3.1GiB") {
		t.Error("the reason the recommendation was withheld is not in the page")
	}
}

// TestMissingSignalsSurvive: a format that drops what the run could not see
// turns a degraded report into a confident one. That is the defect this project
// keeps finding in itself, and a renderer is an easy place to reintroduce it.
func TestMissingSignalsSurvive(t *testing.T) {
	r := &report.Report{
		Findings: []analyze.Finding{{
			Namespace: "prod", Workload: "Deployment/api", Container: "api",
			Verdict: analyze.Unverified, Confidence: analyze.Low,
			RecCPU: 100, RecMem: 1 << 20,
			Missing: []metrics.Signal{metrics.SigPSI, metrics.SigThrottle},
		}},
		Missing: []analyze.SignalGap{{Signal: metrics.SigPSI, Status: analyze.GapSuspect,
			Detail: "every one of the 9 containers drawing at least 50m reported exactly zero"}},
	}
	out := renderHTML(t, r)

	if !strings.Contains(out, "every one of the 9 containers") {
		t.Error("the cluster-level signal gap is missing from the page")
	}
	if !strings.Contains(out, "psi") || !strings.Contains(out, "cfs-throttle") {
		t.Error("the per-container missing signals are not shown")
	}
	if !strings.Contains(out, "downgraded, not silently assumed") {
		t.Error("the page does not say what a missing signal did to the findings")
	}
}

// TestDemoSaysSoLoudly. A page of findings that looks like a cluster and is not
// one would be the most misleading thing this tool has ever produced, and it is
// the exact page a new user is most likely to forward to someone else.
func TestDemoSaysSoLoudly(t *testing.T) {
	out := renderHTML(t, report.Demo(48*3600e9, HumanDuration(48*3600e9)))
	if !strings.Contains(out, "Demo data") {
		t.Error("a simulated report does not announce itself")
	}
	if !strings.Contains(out, "No cluster was read") {
		t.Error("the demo banner does not say that no cluster was read")
	}

	real := renderHTML(t, &report.Report{Findings: []analyze.Finding{{
		Namespace: "prod", Workload: "Deployment/api", Container: "api",
		Verdict: analyze.Waste, RecCPU: 100, RecMem: 1 << 20,
	}}})
	if strings.Contains(real, "Demo data") {
		t.Error("a real report claims to be demo data")
	}
}

// TestEveryFindingGetsARow. A renderer that silently drops rows is worse than
// one that fails: the total at the top still adds up and nothing looks wrong.
func TestEveryFindingGetsARow(t *testing.T) {
	r := report.Demo(48*3600e9, HumanDuration(48*3600e9))
	out := renderHTML(t, r)
	if n := strings.Count(out, `<tr class="row"`); n != len(r.Findings) {
		t.Errorf("%d findings produced %d rows", len(r.Findings), n)
	}
	for _, f := range r.Findings {
		if !strings.Contains(out, f.Container) {
			t.Errorf("%s is missing from the page", f.Container)
		}
	}
}

// TestHTMLEscapesWorkloadNames. Kubernetes names are constrained, but the
// reasons and the pin explanations are free text assembled from cluster data,
// and this file gets opened in a browser by someone who did not generate it.
func TestHTMLEscapesWorkloadNames(t *testing.T) {
	r := &report.Report{Findings: []analyze.Finding{{
		Namespace: "prod", Workload: "Deployment/<script>alert(1)</script>", Container: "api",
		Verdict: analyze.Waste, RecCPU: 100, RecMem: 1 << 20,
		Reasons: []analyze.Reason{{Code: analyze.MemLeak, Text: `<img src=x onerror="alert(2)">`}},
	}}}
	out := renderHTML(t, r)
	if strings.Contains(out, "<script>alert(1)</script>") || strings.Contains(out, `onerror="alert(2)"`) {
		t.Error("cluster-derived text reached the page unescaped")
	}
}

// TestNodeBlockKeepsItsCounts. The three node figures are the only ones in this
// tool that map to money, and they are reported apart on purpose: a cluster
// that is merely not tightly packed can free nodes without tare's help, and
// folding that in would be taking credit for it.
func TestNodeBlockKeepsItsCounts(t *testing.T) {
	r := &report.Report{
		Findings: []analyze.Finding{{Namespace: "prod", Workload: "Deployment/api", Container: "api", Verdict: analyze.Waste, RecCPU: 100, RecMem: 1 << 20}},
		Nodes: packing.Result{
			Trustworthy: true, NodesRunning: 12, NodesPackedNow: 11, NodesPackedRecommended: 9,
			Removable: []string{"ip-10-0-3-41", "ip-10-0-5-12"},
		},
	}
	out := renderHTML(t, r)
	for _, want := range []string{"12 running", "11 needed", "9 needed", "ip-10-0-3-41"} {
		if !strings.Contains(out, want) {
			t.Errorf("node block lost %q", want)
		}
	}
	// html/template escapes the apostrophe, so match either side of it.
	if !strings.Contains(out, "That saving is not tare") {
		t.Error("the page claims the cluster's existing slack as a saving tare produced")
	}
}

// TestUnknownFormatIsRejected: a typo must not silently fall through to the
// table and produce a report the caller did not ask for.
func TestUnknownFormatIsRejected(t *testing.T) {
	err := Render(io.Discard, io.Discard, &report.Report{}, "hmtl", false)
	if err == nil {
		t.Fatal("a misspelled output format was accepted")
	}
	if !strings.Contains(err.Error(), "html") {
		t.Errorf("the error does not list the valid formats: %v", err)
	}
}
