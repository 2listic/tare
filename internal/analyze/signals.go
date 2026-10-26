package analyze

import (
	"fmt"

	"github.com/mfranzon/tare/internal/metrics"
)

// has reports whether a signal backed this finding.
func (f *Finding) hasSignal(s metrics.Signal) bool { return f.Have[s] }

// HasPSI and HasThrottle let a renderer distinguish a measured zero from an
// absent measurement.
func (f Finding) HasPSI() bool      { return f.Have[metrics.SigPSI] }
func (f Finding) HasThrottle() bool { return f.Have[metrics.SigThrottle] }

// HasCPUWait reports tare-agent's CPU wait as the pressure source: measured,
// believed, and not overruled by PSI, which decides wherever it exists.
func (f Finding) HasCPUWait() bool { return f.Have[metrics.SigCPUWait] && !f.Have[metrics.SigPSI] }

// The sustained halves are separate signals because they are separate queries
// and either can fail on its own. A renderer that printed a missing mean as
// 0.0000 beside a real peak would be showing the most reassuring number on the
// row for a value nobody measured.
func (f Finding) HasPSIAvg() bool      { return f.Have[metrics.SigPSIAvg] }
func (f Finding) HasThrottleAvg() bool { return f.Have[metrics.SigThrottleAvg] }

// wanted lists the signals the analyzer uses, with what is lost without each.
// Everything here can legitimately be absent on some cluster, which is exactly
// why absence has to be stated rather than read as a zero.
var wanted = []struct {
	sig  metrics.Signal
	lost string
}{
	{metrics.SigPSI, "CPU starvation cannot be detected from pressure"},
	{metrics.SigThrottle, "CPU starvation cannot be detected from throttling"},
	{metrics.SigOOM, "past OOMKills are invisible, so memory may be reduced below a size already known to fail"},
	{metrics.SigRestarts, "restart-driven memory resets cannot be distinguished from a leak"},
	{metrics.SigMemP95, "a runtime heap ceiling cannot be inferred from a memory plateau"},
	{metrics.SigStartup, "the CPU limit cannot cover a startup burst, so restarts may be throttled"},
	{metrics.SigMemTrend, "unbounded memory growth cannot be detected"},
}

// applySignalGaps records what was missing and degrades the conclusion
// accordingly. The rule that matters: a container with no saturation signal at
// all must never be presented as comfortably shrinkable, because nothing in the
// data distinguishes it from one that is being throttled right now.
func applySignalGaps(f *Finding) {
	for _, w := range wanted {
		if !f.hasSignal(w.sig) {
			f.Missing = append(f.Missing, w.sig)
		}
	}

	blind := !f.hasAny(metrics.SaturationSignals...)
	if blind {
		f.Reasons = append(f.Reasons, note(NoSaturationSignal, "no saturation signal is usable for this container "+
			"(no PSI, no CPU wait from tare-agent, no CFS throttle metrics), so it cannot be shown to have headroom. "+
			"Treat any reduction as unverified. Where PSI is missing, tare-agent provides CPU wait: see kubectl tare check."))
		// Confidence is about how much the recommendation can be trusted, and
		// without a saturation signal it cannot be trusted much whatever the
		// sample count says.
		f.Confidence = Low
		if f.Verdict == Waste || f.Verdict == Healthy {
			f.Verdict = Unverified
		}
	}

	if !f.hasSignal(metrics.SigOOM) && f.RecMem > 0 && f.MemRequest > 0 && f.RecMem < f.MemRequest {
		f.Reasons = append(f.Reasons, note(NoOOMHistory, "OOMKill history is unavailable, so a past failure at this "+
			"size would not be visible; reduce memory in stages rather than in one step"))
	}
	// Raising memory is the safe direction for an OOMKill and the unsafe one
	// for a leak: a container whose working set climbs forever will accept any
	// number offered and come back for more. Without the trend signal there is
	// nothing in the data that separates real growth from a leak, and the pod
	// being younger than the window is enough to lose it.
	if !f.hasSignal(metrics.SigMemTrend) && f.MemRequest > 0 && f.RecMem > f.MemRequest {
		f.Reasons = append(f.Reasons, measured(NoMemTrendOnRaise, float64(f.RecMem), float64(f.MemRequest), Bytes, "this recommendation RAISES memory, and the memory-trend signal "+
			"is unavailable for this container, so steady growth cannot be distinguished from a leak. "+
			"Check the working set over time before applying it."))
		if f.Confidence == High {
			f.Confidence = Medium
		}
	}
	if !f.hasSignal(metrics.SigMemP95) {
		f.Reasons = append(f.Reasons, note(NoMemP95, "memory percentile data is unavailable, so a hidden runtime "+
			"heap ceiling could not be inferred from behaviour"))
	}
}

func (f *Finding) hasAny(ss ...metrics.Signal) bool {
	for _, s := range ss {
		if f.hasSignal(s) {
			return true
		}
	}
	return false
}

// SignalGap is one signal that did not fully work, and why.
//
// The status is separate from the prose for the same reason a Reason's code
// is: a renderer laying the gaps out as a table, and an operator grepping for
// a signal that is absent rather than merely partial, both need the fact
// without the sentence around it.
type SignalGap struct {
	Signal metrics.Signal `json:"signal"`
	// Status is one of ABSENT, PARTIAL, QUERY_FAILED, SUSPECT.
	Status string `json:"status"`
	Detail string `json:"detail"`
}

// Gap statuses. SUSPECT is not a coverage gap: the signal returned data and
// the data is not believable, which is worse than silence.
const (
	GapAbsent      = "ABSENT"
	GapPartial     = "PARTIAL"
	GapQueryFailed = "QUERY_FAILED"
	GapSuspect     = "SUSPECT"
	// GapDenied is RBAC, not the metrics backend: the query was never asked.
	GapDenied = "DENIED"
)

func (g SignalGap) String() string { return string(g.Signal) + ": " + g.Detail }

// GapTexts is the prose form, for renderers that show sentences.
func GapTexts(gs []SignalGap) []string {
	if len(gs) == 0 {
		return nil
	}
	out := make([]string, len(gs))
	for i, g := range gs {
		out[i] = g.String()
	}
	return out
}

// SummariseHealth renders the signal health of a source for the report header.
//
// A signal counts as degraded if ANY of its queries failed or returned nothing,
// not only if all of them did. The memory trend needs both an early and a late
// window: with only the late one it silently stops detecting leaks, which is
// precisely the kind of half-working signal this reporting exists to expose.
func SummariseHealth(h []metrics.SignalHealth) (missing []SignalGap) {
	type agg struct {
		total, ok int
		firstErr  error
		suspect   string
	}
	byName := map[metrics.Signal]*agg{}
	var order []metrics.Signal
	for _, s := range h {
		a, seen := byName[s.Signal]
		if !seen {
			a = &agg{}
			byName[s.Signal] = a
			order = append(order, s.Signal)
		}
		a.total++
		if s.OK() {
			a.ok++
		} else if s.Err != nil && a.firstErr == nil {
			a.firstErr = s.Err
		}
		// A suspect signal returned data and errored on nothing, so it would
		// otherwise fall to the "only partly available" branch below and be
		// described as a coverage gap rather than as an instrument that is
		// lying.
		if s.Suspect != "" && a.suspect == "" {
			a.suspect = s.Suspect
		}
	}
	for _, name := range order {
		a := byName[name]
		switch {
		case a.ok == a.total:
			continue
		case a.suspect != "":
			missing = append(missing, SignalGap{name, GapSuspect, a.suspect})
		case a.firstErr != nil:
			missing = append(missing, SignalGap{name, GapQueryFailed, fmt.Sprintf("query failed (%v)", a.firstErr)})
		case a.ok == 0:
			missing = append(missing, SignalGap{name, GapAbsent, "not exported by this cluster"})
		default:
			missing = append(missing, SignalGap{name, GapPartial, fmt.Sprintf(
				"only %d of %d queries returned data, so anything depending on the missing part is not evaluated",
				a.ok, a.total)})
		}
	}
	return missing
}
