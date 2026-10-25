package tui

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/2listic/tare/internal/metrics"
	"github.com/2listic/tare/internal/render"
)

const (
	plotHeight = 6
	plotAxis   = 9 // "  1840m │"
)

var eighths = []rune(" ▁▂▃▄▅▆▇█")

// plotMark is a horizontal reference line: a request, a limit, or the
// recommendation. Zero means unset and draws nothing.
type plotMark struct {
	label string
	value float64
}

// plot is one resource over the window as a column chart.
//
// A bucket with no sample is blank; a measured bucket is at least one eighth
// tall, so a measured zero and a missing sample never look the same.
//
// The scale runs from zero to the peak, or to the recommendation when that is
// higher, not to the limit: scaled to a limit twenty times the usage, every
// bar sat in the bottom row and a leak's slope was three block heights. A mark
// above the scale is named in the title instead of drawn.
//
// No colour: clip() counts runes, and the marks are told apart by their
// labels, not their hue.
type plot struct {
	name   string
	series metrics.Series
	format func(float64) string

	peak, top float64
	seen      bool
	above     []string   // marks over the scale, for the title
	labels    [][]string // per row, the marks drawn in it
}

func newPlot(name string, s metrics.Series, format func(float64) string, marks []plotMark) *plot {
	p := &plot{name: name, series: s, format: format, labels: make([][]string, plotHeight)}
	for _, v := range s.Values {
		if !math.IsNaN(v) {
			p.peak, p.seen = math.Max(p.peak, v), true
		}
	}
	p.top = p.peak
	for _, m := range marks {
		if m.label == "rec" {
			p.top = math.Max(p.top, m.value)
		}
	}
	if p.top <= 0 {
		p.top = 1
	}
	p.top *= 1.1 // headroom, so a mark at the peak is not hidden by the bars
	for _, m := range marks {
		switch {
		case m.value > p.top:
			p.above = append(p.above, m.label+" "+format(m.value))
		case m.value > 0:
			r := p.rowOf(m.value)
			p.labels[r] = append(p.labels[r], m.label+" "+format(m.value))
		}
	}
	return p
}

// rowOf is the row a value falls in, top row 0.
func (p *plot) rowOf(v float64) int {
	r := plotHeight - 1 - int(v/p.top*plotHeight-1e-9)
	return max(0, min(plotHeight-1, r))
}

// labelWidth is what the right-hand labels need, so plots stacked together can
// share one column count and one time axis.
func (p *plot) labelWidth() int {
	w := 0
	for _, l := range p.labels {
		if len(l) > 0 {
			w = max(w, len([]rune(strings.Join(l, " ")))+1)
		}
	}
	return w
}

func (p *plot) lines(cols int) []string {
	if !p.seen {
		return []string{fmt.Sprintf("%-15sno samples in the window", p.name)}
	}
	window := p.series.Step * time.Duration(len(p.series.Values))
	title := fmt.Sprintf("%-15speak %s   %s per column, worst pod", p.name, p.format(p.peak), shortDuration(window/time.Duration(cols)))
	if len(p.above) > 0 {
		title += "   above: " + strings.Join(p.above, ", ")
	}
	out := []string{title}

	vals := downsample(p.series.Values, cols)
	for r := 0; r < plotHeight; r++ {
		var b strings.Builder
		switch r {
		case 0:
			b.WriteString(fmt.Sprintf("%7s │", p.format(p.top)))
		case plotHeight - 1:
			b.WriteString(fmt.Sprintf("%7s │", "0"))
		default:
			b.WriteString("        │")
		}
		floor := float64(plotHeight-1-r) * 8
		for _, v := range vals {
			cell := ' '
			if !math.IsNaN(v) {
				h := math.Max(1, math.Ceil(v/p.top*plotHeight*8))
				if n := int(math.Min(8, h-floor)); n > 0 {
					cell = eighths[n]
				}
			}
			if cell == ' ' && len(p.labels[r]) > 0 {
				cell = '╌'
			}
			b.WriteRune(cell)
		}
		if len(p.labels[r]) > 0 {
			b.WriteString(" " + strings.Join(p.labels[r], " "))
		}
		out = append(out, b.String())
	}
	left := "-" + render.HumanDuration(window)
	return append(out, "        └"+render.PadTo(left, max(cols-3, len(left)+1))+"now")
}

// stackPlots draws plots one above the other on a shared time axis.
func stackPlots(width int, ps ...*plot) []string {
	labelW, n := 0, 0
	for _, p := range ps {
		labelW = max(labelW, p.labelWidth())
		n = max(n, len(p.series.Values))
	}
	// Width alone decides the refusal: a short series is drawn short, not
	// blamed on the terminal.
	room := width - plotAxis - labelW
	if room < 10 {
		return []string{fmt.Sprintf("%-15sneeds a wider terminal", "plot")}
	}
	cols := min(n, room)
	var out []string
	for _, p := range ps {
		out = append(out, p.lines(cols)...)
	}
	return out
}

// shortDuration is a column's width in time: "4h32m", not Go's "4h32m25.9s".
func shortDuration(d time.Duration) string {
	d = d.Round(time.Minute)
	if d >= time.Hour {
		if m := d % time.Hour / time.Minute; m != 0 {
			return fmt.Sprintf("%dh%02dm", d/time.Hour, m)
		}
		return fmt.Sprintf("%dh", d/time.Hour)
	}
	return fmt.Sprintf("%dm", d/time.Minute)
}

// downsample keeps the peak of each group, never the mean, for the reason
// Series stores peaks.
func downsample(v []float64, cols int) []float64 {
	out := make([]float64, cols)
	for j := range out {
		out[j] = math.NaN()
		for i := j * len(v) / cols; i < (j+1)*len(v)/cols; i++ {
			if !math.IsNaN(v[i]) && !(out[j] >= v[i]) {
				out[j] = v[i]
			}
		}
	}
	return out
}
