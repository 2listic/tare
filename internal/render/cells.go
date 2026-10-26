package render

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/mfranzon/tare/internal/analyze"
	"github.com/mfranzon/tare/internal/ceilings"
	"github.com/mfranzon/tare/internal/packing"
)

// termWidth is the terminal width to lay the table out for. COLUMNS is what a
// shell exports and what a user can override; the fallback is the width a
// terminal is still expected to have when nothing says otherwise.
func termWidth() int {
	if c := os.Getenv("COLUMNS"); c != "" {
		if n, err := strconv.Atoi(c); err == nil && n > 40 {
			return n
		}
	}
	return 120
}

// shortWorkload renders namespace/Kind/name inside max columns. Kinds are
// abbreviated the way kubectl abbreviates them, and a name too long even then
// loses its middle rather than its tail: the tail is what distinguishes
// prometheus-kps-...-prometheus from prometheus-kps-...-operator.
func shortWorkload(ns, workload string, max int) string {
	kind, name, ok := strings.Cut(workload, "/")
	if ok {
		switch kind {
		case "Deployment":
			kind = "deploy"
		case "StatefulSet":
			kind = "sts"
		case "DaemonSet":
			kind = "ds"
		case "ReplicaSet":
			kind = "rs"
		case "ReplicationController":
			kind = "rc"
		case "CronJob":
			kind = "cronjob"
		default:
			kind = strings.ToLower(kind)
		}
		workload = kind + "/" + name
	}
	full := ns + "/" + workload
	if len(full) <= max {
		return full
	}
	// The kind is the least informative part - almost everything is a
	// Deployment - so it goes before any of the name does.
	if short := ns + "/" + name; ok && len(short) <= max {
		return short
	}
	full = ns + "/" + name
	if len(full) <= max {
		return full
	}
	if max < 8 {
		return full[:max]
	}
	head := (max - 1) / 3
	tail := max - 1 - head
	return full[:head] + "…" + full[len(full)-tail:]
}

func cpuCell(f analyze.Finding) string {
	if f.RecCPU == 0 {
		return fmt.Sprintf("%s>-", Milli(f.CPURequest))
	}
	return fmt.Sprintf("%s>%dm", Milli(f.CPURequest), f.RecCPU)
}

// limitCell shows the CPU limit recommendation, which is deliberately looser
// than the request: a request sized to a percentile must not double as a limit
// that throttles every burst.
func limitCell(f analyze.Finding) string {
	if f.RecCPULimit == 0 {
		return fmt.Sprintf("%s>-", Milli(f.CPULimit))
	}
	return fmt.Sprintf("%s>%dm", Milli(f.CPULimit), f.RecCPULimit)
}

func memCell(f analyze.Finding) string {
	if f.MemCensored {
		return fmt.Sprintf("%s>?", Human(f.MemRequest))
	}
	if f.RecMem == 0 {
		return fmt.Sprintf("%s>-", Human(f.MemRequest))
	}
	return fmt.Sprintf("%s>%s", Human(f.MemRequest), Human(f.RecMem))
}

func memLimitCell(f analyze.Finding) string {
	if f.MemCensored {
		return fmt.Sprintf("%s>?", Human(f.MemLimit))
	}
	if f.RecMemLimit == 0 {
		return fmt.Sprintf("%s>-", Human(f.MemLimit))
	}
	return fmt.Sprintf("%s>%s", Human(f.MemLimit), Human(f.RecMemLimit))
}

// PressureCell must never print "none" for a signal that was not measured.
// "not measured" and "measured as zero" look identical in a table unless the
// difference is spelled out, and conflating them is how a throttled container
// gets reported as comfortable.
func PressureCell(f analyze.Finding) string {
	switch {
	case f.OOMKilled:
		return "OOMKilled"
	// Sustained first, peak in brackets. Showing only the peak told the reader
	// a container was throttled 6% of the time when the true figure was 0.2%
	// and one minute had been unlucky.
	case f.HasPSI() && f.PSIWaitingPeak > 0.001:
		if !f.HasPSIAvg() {
			return fmt.Sprintf("psi ?/%.3f", f.PSIWaitingPeak)
		}
		return fmt.Sprintf("psi %.4f/%.3f", f.PSIWaiting, f.PSIWaitingPeak)
	// CPU wait: the share of 5-minute stretches stalled, then the normal
	// slowdown it was judged against ("-" when it never waited enough to have
	// one). Not on PSI's scale, hence its own prefix.
	case f.HasCPUWait() && f.CPUWait.Stalled > 0:
		return fmt.Sprintf("wait %.0f%%/%s", f.CPUWait.Stalled*100, cpuWaitNormal(f))
	case f.HasThrottle() && f.ThrottlePeak > 0.001:
		if !f.HasThrottleAvg() {
			return fmt.Sprintf("thr ?/%.1f%%", f.ThrottlePeak*100)
		}
		return fmt.Sprintf("thr %.1f/%.1f%%", f.Throttle*100, f.ThrottlePeak*100)
	case f.HasPSI() || f.HasCPUWait() || f.HasThrottle():
		return "none"
	}
	return "unmeasured"
}

func cpuWaitNormal(f analyze.Finding) string {
	if !f.CPUWait.HasNormal {
		return "-"
	}
	return fmt.Sprintf("%.3f", f.CPUWait.Normal)
}

// CPUWaitDetail is tare-agent's view of a container in one line, for the
// views that have room for it, or "" when CPU wait is not its pressure source.
func CPUWaitDetail(f analyze.Finding) string {
	if !f.HasCPUWait() {
		return ""
	}
	c := f.CPUWait
	cover := "-"
	if c.Busy {
		cover = fmt.Sprintf("%.0f%%", c.Coverage*100)
	}
	return fmt.Sprintf("stalled %.1f%% of %d stretches   normal %s   mean %.3f s/s   coverage %s",
		c.Stalled*100, c.Stretches, cpuWaitNormal(f), c.Mean, cover)
}

func Milli(m int64) string {
	if m == 0 {
		return "unset"
	}
	return fmt.Sprintf("%dm", m)
}

func Human(b int64) string {
	if b == 0 {
		return "unset"
	}
	return analyze.HumanBytes(b)
}

// CeilingCode is the evidence code a ceiling renders under.
func CeilingCode(c ceilings.Ceiling) string {
	if c.Unresolved {
		return "CEILING_UNRESOLVED"
	}
	return "CEILING"
}

// CeilingLine is a ceiling as one line of prose evidence.
func CeilingLine(c ceilings.Ceiling) string {
	mark := "ceiling"
	if c.Unresolved {
		mark = "ceiling?"
	}
	return fmt.Sprintf("[%s %s] %s", mark, c.Kind, c.Note)
}

// MissingNames is the signals not measured for this container.
func MissingNames(f analyze.Finding) []string {
	var names []string
	for _, s := range f.Missing {
		names = append(names, string(s))
	}
	return names
}

// freedLabel is the nodes tare's recommendations free. Pinned pods held their
// nodes, so with any pinned it is a lower bound and says so.
func freedLabel(p packing.Result) string {
	freed := strconv.Itoa(p.FromResizing())
	if p.Pinned > 0 {
		freed = ">=" + freed
	}
	return freed
}
