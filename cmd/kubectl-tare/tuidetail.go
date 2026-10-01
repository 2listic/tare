package main

import (
	"fmt"
	"strings"

	"github.com/mfranzon/tare/internal/analyze"
	"github.com/mfranzon/tare/internal/metrics"
)

// tuiDetail is what an expanded row shows: the container's resources and what
// was measured against them, as aligned columns. The sentences behind the
// codes are one key away ('e'), not deleted, because they carry what a number
// cannot, such as what an idle verdict does not rule out.
func tuiDetail(f analyze.Finding) []string {
	has := func(s metrics.Signal) bool { return f.Have[s] }
	cpuUse := func(cores float64, s metrics.Signal) string {
		if !has(s) {
			return "?"
		}
		return fmt.Sprintf("%.0fm", cores*1000)
	}
	memUse := func(b float64, s metrics.Signal) string {
		if !has(s) {
			return "?"
		}
		return usageBytes(b)
	}

	rows := [][]string{
		{"", "CURRENT", "RECOMMENDED", "P95", "PEAK"},
		{"cpu request", milli(f.CPURequest), milliRec(f.RecCPU),
			cpuUse(f.CPUUsageP95, metrics.SigCPUUsage), cpuUse(f.CPUUsageMax, metrics.SigCPUPeak)},
		{"cpu limit", milli(f.CPULimit), milliRec(f.RecCPULimit), "", ""},
		{"mem request", human(f.MemRequest), memRec(f),
			memUse(f.MemP95, metrics.SigMemP95), memUse(f.MemUsageMax, metrics.SigMemory)},
		{"mem limit", human(f.MemLimit), memLimitRec(f), "", ""},
	}
	out := alignColumns(rows)

	// "?" is a signal that was not measured. It must never print as 0, which
	// is the most reassuring figure there is.
	psi, thr := "?", "?"
	if f.HasPSI() {
		psi = fmt.Sprintf("%.3f", f.PSIWaitingPeak)
		if f.HasPSIAvg() {
			psi = fmt.Sprintf("%.4f/%.3f", f.PSIWaiting, f.PSIWaitingPeak)
		}
	}
	if f.HasThrottle() {
		thr = fmt.Sprintf("%.1f%%", f.ThrottlePeak*100)
		if f.HasThrottleAvg() {
			thr = fmt.Sprintf("%.1f/%.1f%%", f.Throttle*100, f.ThrottlePeak*100)
		}
	}
	oom, restarts := "?", "?"
	if has(metrics.SigOOM) {
		oom = map[bool]string{true: "yes", false: "no"}[f.OOMKilled]
	}
	if has(metrics.SigRestarts) {
		restarts = fmt.Sprintf("%.0f", f.Restarts)
	}
	out = append(out,
		fmt.Sprintf("%-15spsi %s   throttle %s   oom %s   restarts %s", "pressure", psi, thr, oom, restarts),
		fmt.Sprintf("%-15sqos %s   pods %d   samples %d", "pod", dash(f.QoS), f.Pods, f.Samples))
	if len(f.Nodes) > 0 {
		out = append(out, fmt.Sprintf("%-15s%s", "nodes", strings.Join(f.Nodes, " ")))
	}

	var codes []string
	for _, r := range f.Reasons {
		codes = append(codes, string(r.Code))
	}
	for _, c := range f.Ceilings {
		code := "CEILING:"
		if c.Unresolved {
			code = "CEILING_UNRESOLVED:"
		}
		codes = append(codes, code+string(c.Kind))
	}
	if len(codes) > 0 {
		out = append(out, fmt.Sprintf("%-15s%s", "codes", strings.Join(codes, " ")))
	}
	if len(f.Missing) > 0 {
		var names []string
		for _, s := range f.Missing {
			names = append(names, string(s))
		}
		out = append(out, fmt.Sprintf("%-15s%s", "not measured", strings.Join(names, " ")))
	}
	return out
}

// alignColumns pads by runes, not bytes, for the same reason padTo exists.
func alignColumns(rows [][]string) []string {
	widths := make([]int, len(rows[0]))
	for _, r := range rows {
		for i, c := range r {
			widths[i] = max(widths[i], len([]rune(c)))
		}
	}
	widths[0] = max(widths[0], 13)
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		var b strings.Builder
		for i, c := range r {
			if i < len(r)-1 {
				c = padTo(c, widths[i]+2)
			}
			b.WriteString(c)
		}
		out = append(out, strings.TrimRight(b.String(), " "))
	}
	return out
}

// usageBytes renders a measured amount, where a small value is a real value:
// human() says "unset" for zero and bytes below a MiB, both wrong here.
func usageBytes(b float64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1fGi", b/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.0fMi", b/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.0fKi", b/(1<<10))
	}
	return fmt.Sprintf("%.0fB", b)
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
