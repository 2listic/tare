// Package report is what one run of tare established, independent of how it is
// shown: the findings, the node replay, the gaps in what could be measured,
// and the arithmetic every format must agree on - totals, verdict counts and
// the reader's view filters. Renderers read a Report; none of them compute
// their own version of these numbers.
package report

import (
	"github.com/mfranzon/tare/internal/analyze"
	"github.com/mfranzon/tare/internal/metrics"
	"github.com/mfranzon/tare/internal/packing"
)

// Report is everything a renderer needs, so that the table, the JSON and the
// HTML all describe the same run rather than each reaching into the cluster
// for its own version of it.
//
// Missing is carried alongside Findings deliberately. Every renderer has to be
// able to say what the run could not see, and a format that drops it turns a
// degraded Report into a confident one - which is the defect this project
// keeps finding in itself.
type Report struct {
	Findings []analyze.Finding
	Nodes    packing.Result
	// Commitment and DrainBlockers are the two things that decide whether a
	// node-count saving could actually be realised.
	Commitment    []packing.Commitment
	DrainBlockers []string
	Missing       []analyze.SignalGap
	Source        string
	Window        string

	// Coverage is how much of the cluster the metrics backend actually saw.
	// A Prometheus that scrapes a subset of namespaces, or a node whose
	// kubelet has stopped reporting, produces a report that looks complete and
	// silently describes a fraction of the cluster. Those containers do appear,
	// as NO-DATA rows, but nothing else in the report adds them up.
	Coverage       string
	CoverageCounts Coverage

	// Redacted marks a report whose names were replaced with hashes, so every
	// renderer can say so rather than leave a reader hunting for a namespace
	// called 3f2a91c4.
	Redacted bool

	// Demo marks a report built from the simulator. Renderers must say so:
	// a page of findings that looks like a cluster and is not one would be
	// the most misleading thing this tool has ever printed.
	Demo bool
	// NodeNote replaces the node-count block when there is no packing result
	// to show, with the reason there is not.
	NodeNote string

	// WindowUsed is the window actually queried; Window is the note that says
	// how it differs from the one asked for.
	WindowUsed string
	// VerdictFiltered and BySavings tell the compact view that the reader
	// already chose which rows to see and in what order.
	VerdictFiltered, BySavings bool

	// Usage fetches one finding's usage over the window, for the plot an
	// expanded TUI row draws. It runs on demand because a range query per
	// container up front would multiply the report's cost for rows nobody
	// opens. Nil when there is nothing to query; UsageNote says why.
	Usage     func(analyze.Finding) (cpu, mem metrics.Series, err error)
	UsageNote string

	// Factors convert the node delta into energy and emissions; zero when
	// the reader supplied none, and then no footprint is printed.
	Factors Factors
}

// NodesNotComputed is why there is no node count to show, or "" when there is one.
func NodesNotComputed(r *Report) string {
	if r.NodeNote != "" {
		return r.NodeNote
	}
	if !r.Nodes.Trustworthy {
		// A Result nobody filled in is untrustworthy with no reason given,
		// and an empty reason here reads as "computed".
		if r.Nodes.SelfCheck == "" {
			return "the node replay did not run"
		}
		return r.Nodes.SelfCheck
	}
	return ""
}

// PlotBuckets is how many points a usage query returns: wider than any
// terminal, so the drawn columns are always a downsample and never a stretch.
const PlotBuckets = 240
