package tui

import (
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/2listic/tare/internal/metrics"
)

func series(vs ...float64) metrics.Series {
	return metrics.Series{Start: time.Unix(0, 0), Step: time.Hour, Values: vs}
}

func plotText(s metrics.Series, marks ...plotMark) string {
	p := newPlot("cpu", s, func(v float64) string { return strconv.FormatFloat(v, 'g', 3, 64) }, marks)
	return strings.Join(stackPlots(80, p), "\n")
}

// A measured zero and a missing sample are different facts. The whole tool
// rests on never drawing "not measured" as the reassuring value.
func TestPlotGapIsNotZero(t *testing.T) {
	vs := make([]float64, 20)
	for i := range vs {
		vs[i] = 5
	}
	vs[3], vs[10] = 0, math.NaN()
	rows := strings.Split(plotText(series(vs...)), "\n")
	bottom := []rune(rows[len(rows)-2])
	start := len([]rune("      0 │"))
	if got := bottom[start+3]; got != '▁' {
		t.Errorf("measured zero drew %q, want the minimum block", got)
	}
	if got := bottom[start+10]; got != ' ' {
		t.Errorf("missing sample drew %q, want blank", got)
	}
}

// Downsampling keeps the peak. A mean would erase the one spike that sets a
// limit, and the chart would contradict the PEAK column above it.
func TestDownsampleKeepsPeak(t *testing.T) {
	got := downsample([]float64{1, 9, 1, 1, math.NaN(), math.NaN()}, 3)
	if got[0] != 9 || got[1] != 1 || !math.IsNaN(got[2]) {
		t.Errorf("downsample = %v, want [9 1 NaN]", got)
	}
}

// A request twenty times the usage must not flatten the chart. It is named,
// not drawn, and the usage keeps the full height.
func TestPlotMarkAboveScaleIsNamed(t *testing.T) {
	out := plotText(series(1, 2, 3, 4), plotMark{"request", 80}, plotMark{"rec", 2})
	if !strings.Contains(out, "above: request 80") {
		t.Errorf("request above the scale is not named:\n%s", out)
	}
	if !strings.Contains(out, "█") {
		t.Errorf("usage did not fill the chart height:\n%s", out)
	}
	drawn := false
	for _, l := range strings.Split(out, "\n") {
		drawn = drawn || strings.Contains(l, "╌") && strings.HasSuffix(l, " rec 2")
	}
	if !drawn {
		t.Errorf("recommendation inside the scale is not drawn:\n%s", out)
	}
}
