package report

import (
	"fmt"
	"strconv"
	"strings"
)

// GridEntry is one country's grid carbon intensity from Ember's yearly data:
// generation emissions intensity in gCO2e/kWh for the year named. Annual and
// location-based, so it is neither the hour the nodes ran nor what a
// renewable-power contract would let a company report.
type GridEntry struct {
	Area  string
	ISO3  string
	Year  int
	Value float64
}

// gridAliases are the short names people type that are neither Ember's area
// name nor an ISO 3166 alpha-3 code.
var gridAliases = map[string]string{"us": "USA", "uk": "GBR"}

// ParseGrid reads --grid-intensity: a number in gCO2e/kWh, or a country by
// name or ISO-3 code. The source string travels with the value, so a figure
// tare looked up is never printed as if the reader had supplied it.
func ParseGrid(s string) (value float64, source string, err error) {
	if v, err := strconv.ParseFloat(s, 64); err == nil {
		if v <= 0 {
			return 0, "", fmt.Errorf("--grid-intensity must be positive, got %v", v)
		}
		return v, "user", nil
	}
	if e, ok := lookupGrid(s); ok {
		return e.Value, fmt.Sprintf("ember_%d_%s", e.Year, e.ISO3), nil
	}
	return 0, "", fmt.Errorf("--grid-intensity %q is neither a number in gCO2e/kWh nor a country tare knows; use a name like italy or united-kingdom, or an ISO-3 code like ITA", s)
}

func lookupGrid(s string) (GridEntry, bool) {
	key := gridKey(s)
	if iso, ok := gridAliases[key]; ok {
		key = gridKey(iso)
	}
	for _, e := range emberGrid {
		if gridKey(e.Area) == key || gridKey(e.ISO3) == key {
			return e, true
		}
	}
	return GridEntry{}, false
}

// gridKey folds case and separators, so "United Kingdom", "united-kingdom"
// and "united_kingdom" are one country.
func gridKey(s string) string {
	return strings.NewReplacer(" ", "-", "_", "-").Replace(strings.ToLower(strings.TrimSpace(s)))
}
