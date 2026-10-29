// Package render writes a report in its non-interactive formats: the compact
// table, the wide sectioned tables, the --explain prose, JSON, the
// self-contained HTML page and the kubectl patch script. Data goes to one
// writer and the summary, warnings and hints to another, so a redirected
// report stays pure data.
package render

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/2listic/tare/internal/analyze"
	"github.com/2listic/tare/internal/packing"
	"github.com/2listic/tare/internal/report"
)

// Render writes r in every non-interactive format: the data to out, and the
// summary, warnings and hints to diag, so a redirected report stays pure data.
func Render(out, diag io.Writer, r *report.Report, format string, verbose bool) error {
	p := &printer{out: out, diag: diag}
	switch format {
	case "patch":
		p.printPatches(r.Findings, r.KubeContext, r.Kubeconfig)
		return nil
	case "json":
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		// An object rather than a bare array of findings: the node-count delta
		// is the figure that corresponds to a bill, and a consumer that only
		// sums reclaimed millicores is computing the thing this report exists
		// to stop people reporting.
		return enc.Encode(struct {
			Findings      []analyze.Finding    `json:"findings"`
			Nodes         packing.Result       `json:"nodes"`
			Commitment    []packing.Commitment `json:"commitment"`
			DrainBlockers []string             `json:"drainBlockers"`
			Missing       []analyze.SignalGap  `json:"missingSignals"`
			Coverage      string               `json:"coverage,omitempty"`
			Demo          bool                 `json:"demo,omitempty"`
			Footprint     *jsonFootprint       `json:"footprint,omitempty"`
		}{r.Findings, r.Nodes, r.Commitment, r.DrainBlockers, r.Missing, r.Coverage, r.Demo, footprintJSON(r)})
	case "html":
		return writeHTML(out, r)
	case "table":
		p.printCompact(r)
		if verbose {
			p.printExplanations(r, false)
		} else {
			p.compactHint()
		}
		return nil
	case "wide":
		p.printMeta(r)
		p.printSignalGaps(r.Missing)
		p.printFindings(r.Findings)
		p.printEvidence(r.Findings)
		p.printGPUs(r.Findings)
		p.printVerdicts(r.Findings)
		p.printTotals(r.Findings)
		p.printNodes(r)
		p.printFootprint(r)
		p.printDrainBlockers(r.DrainBlockers)
		p.printCommitment(r.Commitment)
		if verbose {
			p.printExplanations(r, true)
		} else {
			// stderr, so a redirected report stays pure data.
			fmt.Fprintln(p.diag, "run with --explain for the reasoning behind every code above")
		}
		return nil
	}
	return UnknownFormat(format)
}

// jsonFootprint carries the factors beside the figures, so a consumer can
// never read a kgCO2e value without the coefficient it was multiplied by.
type jsonFootprint struct {
	NodeWatts      float64               `json:"nodeWatts"`
	GridGCO2PerKWh float64               `json:"gridGCO2ePerKWh,omitempty"`
	GridSource     string                `json:"gridSource,omitempty"`
	Rows           []report.FootprintRow `json:"rows"`
}

func footprintJSON(r *report.Report) *jsonFootprint {
	rows := report.Footprint(r)
	if rows == nil {
		return nil
	}
	return &jsonFootprint{r.Factors.NodeWatts, r.Factors.GridGCO2PerKWh, r.Factors.GridSource, rows}
}

// Formats are every output format, the interactive one included, in the order
// help lists them.
var Formats = []string{"table", "wide", "tui", "json", "html", "patch"}

// UnknownFormat is the error for a format not in Formats.
func UnknownFormat(format string) error {
	return fmt.Errorf("unknown output %q; the formats are %s and %s",
		format, strings.Join(Formats[:len(Formats)-1], ", "), Formats[len(Formats)-1])
}
