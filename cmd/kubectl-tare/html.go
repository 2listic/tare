package main

import (
	"fmt"
	"html/template"
	"io"
	"strings"

	"github.com/mfranzon/tare/internal/analyze"
)

// The HTML report exists for the case the terminal cannot serve: the person who
// decides on a resize is usually not the person who ran the tool, and often has
// no access to the cluster's Prometheus. It is one self-contained file with no
// script sources, no fonts and no network calls, so it survives being attached
// to a ticket or opened from a downloads folder.
//
// It is deliberately not a dashboard. Every defect in KNOWN-FAILURES.md was a
// clean plausible number with its uncertainty stripped off, and that is the
// default thing a UI does to a report. So the rules here are:
//
//   - a withheld recommendation renders as WITHHELD with its reason, never as a
//     blank cell or a zero;
//   - confidence and missing signals are chrome-free text in the row, not a
//     tooltip;
//   - the evidence is one click away for every row, not only for the
//     interesting ones;
//   - there is no headline currency figure, because tare does not know what a
//     node costs.

type htmlRow struct {
	Verdict    string
	VerdictKey string
	Confidence string
	Namespace  string
	Workload   string
	Container  string
	CPU        string
	CPULimit   string
	Mem        string
	MemLimit   string
	Pressure   string
	Samples    int
	Withheld   bool
	Reasons    []string
	Missing    []string
	Ceilings   []string
	// Sort is the ranking key used by the "biggest first" control, in the same
	// core-versus-gibibyte units the table's --by-savings ordering uses.
	Sort float64
}

type htmlView struct {
	Rows     []htmlRow
	Source   string
	Window   string
	Coverage string
	Missing  []string
	Demo     bool
	NodeNote string
	Counts   []htmlCount
	Savings  htmlSavings
	Nodes    []string
	Blockers []string
}

// htmlSavings exports what the template needs. The totals are computed by the
// same totalSavings the table uses, so the two formats cannot disagree about
// the headline, and the add-back figure is carried beside the reclaim figure
// rather than netted into it: summing only the positive side is how the
// headline once came out optimistic by exactly the amount the reader most
// needed to see.
type htmlSavings struct {
	Workloads        int
	CPUMillicores    int64
	MemBytes         int64
	AddWorkloads     int
	AddCPUMillicores int64
	AddMemBytes      int64
}

type htmlCount struct {
	Verdict string
	N       int
}

func buildHTMLView(r *report) htmlView {
	v := htmlView{
		Source: r.Source, Window: r.Window, Missing: analyze.GapTexts(r.Missing), Coverage: r.Coverage,
		Demo: r.Demo, NodeNote: r.NodeNote, Blockers: r.DrainBlockers,
	}
	t := totalSavings(r.Findings)
	v.Savings = htmlSavings{
		Workloads: t.workloads, CPUMillicores: t.cpuMillicores, MemBytes: t.memBytes,
		AddWorkloads: t.addWorkloads, AddCPUMillicores: t.addCPUMillicores, AddMemBytes: t.addMemBytes,
	}

	counts := map[analyze.Verdict]int{}
	for _, f := range r.Findings {
		counts[f.Verdict]++
	}
	for _, verdict := range analyze.ReportOrder() {
		if counts[verdict] > 0 {
			v.Counts = append(v.Counts, htmlCount{string(verdict), counts[verdict]})
		}
	}

	for _, f := range r.Findings {
		c, m := reclaimable(f)
		var missing []string
		for _, s := range f.Missing {
			missing = append(missing, string(s))
		}
		var ceil []string
		for _, ce := range f.Ceilings {
			mark := "ceiling"
			if ce.Unresolved {
				mark = "ceiling?"
			}
			ceil = append(ceil, fmt.Sprintf("[%s %s] %s", mark, ce.Kind, ce.Note))
		}
		v.Rows = append(v.Rows, htmlRow{
			Verdict:    string(f.Verdict),
			VerdictKey: strings.ToLower(string(f.Verdict)),
			Confidence: string(f.Confidence),
			Namespace:  f.Namespace,
			Workload:   f.Workload,
			Container:  f.Container,
			CPU:        arrow(milli(f.CPURequest), milliRec(f.RecCPU)),
			CPULimit:   arrow(milli(f.CPULimit), milliRec(f.RecCPULimit)),
			Mem:        arrow(human(f.MemRequest), humanRec(f.RecMem)),
			MemLimit:   arrow(human(f.MemLimit), humanRec(f.RecMemLimit)),
			Pressure:   pressureCell(f),
			Samples:    f.Samples,
			// A row with no recommendation at all is the tool declining to
			// answer, and the page has to look like a refusal rather than like
			// a gap in the data.
			Withheld: f.RecCPU == 0 && f.RecMem == 0,
			Reasons:  analyze.ReasonTexts(f.Reasons),
			Missing:  missing,
			Ceilings: ceil,
			Sort:     float64(c)/1000 + float64(m)/float64(1<<30),
		})
	}

	if r.NodeNote == "" {
		v.Nodes = nodeLines(r.Nodes)
	}
	return v
}

func arrow(from, to string) string { return from + " → " + to }

func milliRec(m int64) string {
	if m == 0 {
		return "withheld"
	}
	return fmt.Sprintf("%dm", m)
}

func humanRec(b int64) string {
	if b == 0 {
		return "withheld"
	}
	return human(b)
}

func writeHTML(w io.Writer, r *report) error {
	return htmlTemplate.Execute(w, buildHTMLView(r))
}

var htmlTemplate = template.Must(template.New("report").Funcs(template.FuncMap{
	"cores": cores,
	"human": human,
}).Parse(htmlSource))

const htmlSource = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>tare report</title>
<style>
:root {
  --bg: #fbfbf9; --fg: #1c1c1a; --dim: #6b6b64; --line: #e2e1db;
  --card: #fff; --accent: #8a5a2b;
  --sat: #a8321e; --waste: #8a6d1f; --idle: #5a5a8a; --leak: #7a2166; --ok: #2f6b45;
}
@media (prefers-color-scheme: dark) {
  :root:not([data-theme="light"]) {
    --bg: #17171a; --fg: #e8e8e4; --dim: #9a9a92; --line: #2e2e33;
    --card: #1e1e22; --accent: #d9a05b;
    --sat: #e8705c; --waste: #d6b44a; --idle: #9a9ad0; --leak: #d07ec0; --ok: #6fc28d;
  }
}
:root[data-theme="dark"] {
  --bg: #17171a; --fg: #e8e8e4; --dim: #9a9a92; --line: #2e2e33;
  --card: #1e1e22; --accent: #d9a05b;
  --sat: #e8705c; --waste: #d6b44a; --idle: #9a9ad0; --leak: #d07ec0; --ok: #6fc28d;
}
* { box-sizing: border-box; }
body {
  margin: 0; background: var(--bg); color: var(--fg);
  font: 15px/1.55 ui-sans-serif, -apple-system, "Segoe UI", Roboto, sans-serif;
}
.wrap { max-width: 1180px; margin: 0 auto; padding: 32px 16px 80px; }
h1 { font-size: 20px; margin: 0 0 4px; letter-spacing: -0.01em; }
.meta { color: var(--dim); font-size: 13px; margin-bottom: 24px; }
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }

.banner {
  border: 1px solid var(--accent); border-left-width: 4px; border-radius: 6px;
  padding: 12px 14px; margin-bottom: 20px; background: var(--card); font-size: 14px;
}
.banner strong { color: var(--accent); }

.gaps {
  border: 1px solid var(--line); border-left: 4px solid var(--sat); border-radius: 6px;
  padding: 12px 14px; margin-bottom: 20px; background: var(--card); font-size: 14px;
}
.gaps h2 { font-size: 14px; margin: 0 0 6px; }
.gaps ul { margin: 6px 0 6px 18px; padding: 0; }
.gaps li { margin: 3px 0; }
.gaps .note { color: var(--dim); font-size: 13px; }

.counts { display: flex; flex-wrap: wrap; gap: 8px; margin-bottom: 16px; }
.pill {
  border: 1px solid var(--line); border-radius: 999px; padding: 4px 12px;
  background: var(--card); font-size: 13px; cursor: pointer; color: inherit;
}
.pill[aria-pressed="true"] { border-color: var(--accent); color: var(--accent); }
.pill .n { font-weight: 600; }

.totals { font-size: 14px; margin-bottom: 20px; }
.totals div { margin: 2px 0; }
.totals .warn { color: var(--sat); }

table { width: 100%; border-collapse: collapse; font-size: 13px; }
th, td { text-align: left; padding: 7px 8px; border-bottom: 1px solid var(--line); vertical-align: top; }
th { font-weight: 600; color: var(--dim); font-size: 12px; text-transform: uppercase; letter-spacing: 0.04em; }
tr.row { cursor: pointer; }
tr.row:hover { background: var(--card); }
td.num { white-space: nowrap; font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
.v { font-weight: 600; white-space: nowrap; }
.v-saturated { color: var(--sat); } .v-waste { color: var(--waste); }
.v-idle { color: var(--idle); } .v-leaking { color: var(--leak); }
.v-ok, .v-healthy { color: var(--ok); }
.conf { color: var(--dim); }
.withheld { color: var(--dim); font-style: italic; }
.wl { word-break: break-word; }
.wl small { color: var(--dim); display: block; font-size: 12px; }

tr.ev > td { padding: 0 8px 14px; background: var(--card); }
tr.ev ul { margin: 6px 0 0 18px; padding: 0; }
tr.ev li { margin: 4px 0; }
tr.ev .lab { color: var(--dim); font-size: 12px; text-transform: uppercase; letter-spacing: 0.04em; margin-top: 10px; }
.hint { color: var(--dim); font-size: 13px; margin: 10px 0 18px; }
.nodes { margin-top: 28px; font-size: 14px; }
.nodes h2 { font-size: 15px; margin: 0 0 8px; }
.nodes div { margin: 2px 0; }
footer { margin-top: 36px; color: var(--dim); font-size: 12px; border-top: 1px solid var(--line); padding-top: 12px; }
</style>
</head>
<body>
<div class="wrap">

<h1>tare report</h1>
<div class="meta mono">source {{.Source}} &middot; window {{.Window}} &middot; {{len .Rows}} containers</div>

{{if .Demo}}
<div class="banner">
  <strong>Demo data.</strong> These workloads are simulated. No cluster was read, and
  nothing here describes anything you are running. The verdicts are produced by the
  real analyzer over simulated series, so the shape of the output is honest even
  though the numbers describe nobody's infrastructure.
</div>
{{end}}

{{if .Coverage}}
<div class="gaps">
  <h2>What this report could see</h2>
  <ul><li>{{.Coverage}}</li></ul>
</div>
{{end}}

{{if .Missing}}
<div class="gaps">
  <h2>Signals unavailable on this cluster</h2>
  <ul>{{range .Missing}}<li>{{.}}</li>{{end}}</ul>
  <div class="note">Findings that depended on these are downgraded, not silently assumed.</div>
</div>
{{end}}

<div class="counts">
  <button class="pill" data-v="" aria-pressed="true">all <span class="n">{{len .Rows}}</span></button>
  {{range .Counts}}<button class="pill" data-v="{{.Verdict}}" aria-pressed="false">{{.Verdict}} <span class="n">{{.N}}</span></button>{{end}}
</div>

<div class="totals">
  {{if .Savings.Workloads}}<div>reclaimable across {{.Savings.Workloads}} containers:
    <span class="mono">{{cores .Savings.CPUMillicores}} CPU, {{human .Savings.MemBytes}} memory</span></div>{{end}}
  {{if .Savings.AddWorkloads}}<div class="warn">to be added back across {{.Savings.AddWorkloads}} containers:
    <span class="mono">{{cores .Savings.AddCPUMillicores}} CPU, {{human .Savings.AddMemBytes}} memory</span></div>{{end}}
</div>

<div class="hint">Click any row for the evidence behind it. Column headers sort.</div>

<table id="t">
<thead><tr>
  <th data-sort="verdict">Verdict</th>
  <th data-sort="conf">Conf</th>
  <th data-sort="name">Namespace / workload</th>
  <th data-sort="num">CPU req</th>
  <th data-sort="num">CPU limit</th>
  <th data-sort="num">Memory req</th>
  <th data-sort="num">Memory limit</th>
  <th data-sort="name">Pressure</th>
  <th data-sort="savings">Saving</th>
</tr></thead>
<tbody>
{{range $i, $r := .Rows}}
<tr class="row" data-v="{{$r.Verdict}}" data-i="{{$i}}" data-savings="{{$r.Sort}}">
  <td class="v v-{{$r.VerdictKey}}">{{$r.Verdict}}</td>
  <td class="conf">{{$r.Confidence}}</td>
  <td class="wl">{{$r.Workload}}<small>{{$r.Namespace}} &middot; {{$r.Container}} &middot; {{$r.Samples}} samples</small></td>
  <td class="num{{if $r.Withheld}} withheld{{end}}">{{$r.CPU}}</td>
  <td class="num{{if $r.Withheld}} withheld{{end}}">{{$r.CPULimit}}</td>
  <td class="num{{if $r.Withheld}} withheld{{end}}">{{$r.Mem}}</td>
  <td class="num{{if $r.Withheld}} withheld{{end}}">{{$r.MemLimit}}</td>
  <td class="num">{{$r.Pressure}}</td>
  <td class="num">{{if $r.Withheld}}<span class="withheld">none offered</span>{{else}}{{printf "%.2f" $r.Sort}}{{end}}</td>
</tr>
<tr class="ev" data-for="{{$i}}" hidden><td colspan="9">
  {{if $r.Reasons}}<div class="lab">Why</div><ul>{{range $r.Reasons}}<li>{{.}}</li>{{end}}</ul>{{end}}
  {{if $r.Ceilings}}<div class="lab">Ceilings detected</div><ul>{{range $r.Ceilings}}<li>{{.}}</li>{{end}}</ul>{{end}}
  {{if $r.Missing}}<div class="lab">Not measured for this container</div><ul>{{range $r.Missing}}<li class="mono">{{.}}</li>{{end}}</ul>{{end}}
  {{if not $r.Reasons}}<div class="lab">Why</div><ul><li>No caveats were recorded for this container.</li></ul>{{end}}
</td></tr>
{{end}}
</tbody>
</table>

<div class="nodes">
<h2>Nodes</h2>
{{if .NodeNote}}<div class="withheld">not computed &mdash; {{.NodeNote}}</div>
{{else}}{{range .Nodes}}<div>{{.}}</div>{{end}}{{end}}
{{if .Blockers}}<div class="lab">Disruption budgets that would block a drain</div>
<ul>{{range .Blockers}}<li>{{.}}</li>{{end}}</ul>{{end}}
</div>

<footer>
Reclaimed millicores are not a bill: a request is a scheduling reservation, and lowering
one changes nothing you pay until a node goes away. tare reports nodes, never currency,
because only you know what your nodes cost.
</footer>

</div>
<script>
(function () {
  var tbody = document.querySelector('#t tbody');
  var rows = Array.prototype.slice.call(tbody.querySelectorAll('tr.row'));

  rows.forEach(function (tr) {
    tr.addEventListener('click', function () {
      var ev = tbody.querySelector('tr.ev[data-for="' + tr.dataset.i + '"]');
      ev.hidden = !ev.hidden;
    });
  });

  document.querySelectorAll('.pill').forEach(function (b) {
    b.addEventListener('click', function () {
      var want = b.dataset.v;
      document.querySelectorAll('.pill').forEach(function (o) {
        o.setAttribute('aria-pressed', String(o === b));
      });
      rows.forEach(function (tr) {
        var show = !want || tr.dataset.v === want;
        tr.hidden = !show;
        var ev = tbody.querySelector('tr.ev[data-for="' + tr.dataset.i + '"]');
        if (!show) { ev.hidden = true; }
      });
    });
  });

  var asc = {};
  document.querySelectorAll('#t th').forEach(function (th, col) {
    th.style.cursor = 'pointer';
    th.addEventListener('click', function () {
      var kind = th.dataset.sort;
      asc[col] = !asc[col];
      var dir = asc[col] ? 1 : -1;
      var pairs = rows.map(function (tr) {
        return [tr, tbody.querySelector('tr.ev[data-for="' + tr.dataset.i + '"]')];
      });
      pairs.sort(function (a, b) {
        var x, y;
        if (kind === 'savings') {
          x = parseFloat(a[0].dataset.savings) || 0;
          y = parseFloat(b[0].dataset.savings) || 0;
          return (x - y) * dir;
        }
        x = a[0].cells[col].innerText.trim();
        y = b[0].cells[col].innerText.trim();
        return x.localeCompare(y, undefined, {numeric: true}) * dir;
      });
      pairs.forEach(function (p) { tbody.appendChild(p[0]); tbody.appendChild(p[1]); });
    });
  });
})();
</script>
</body>
</html>
`
