package report

import "testing"

// A number is the reader's; a country is Ember's, and the source says which
// year, so a looked-up figure never reads as one the reader supplied.
func TestParseGrid(t *testing.T) {
	for _, tc := range []struct {
		in, source string
		value      float64
	}{
		{"250", "user", 250},
		{"italy", "ember_2025_ITA", 284.63},
		{"Italy", "ember_2025_ITA", 284.63},
		{"ITA", "ember_2025_ITA", 284.63},
		{"united-kingdom", "ember_2025_GBR", 217.41},
		{"United Kingdom", "ember_2025_GBR", 217.41},
		{"uk", "ember_2025_GBR", 217.41},
		{"us", "ember_2025_USA", 384.4},
	} {
		v, src, err := ParseGrid(tc.in)
		if err != nil || v != tc.value || src != tc.source {
			t.Errorf("ParseGrid(%q) = %v, %q, %v; want %v, %q", tc.in, v, src, err, tc.value, tc.source)
		}
	}
}

// A typo must fail, not resolve to zero: zero would print kgCO2e 0 for a
// country that emits.
func TestParseGridRejects(t *testing.T) {
	for _, in := range []string{"itlay", "", "0", "-3", "europe"} {
		if v, _, err := ParseGrid(in); err == nil {
			t.Errorf("ParseGrid(%q) = %v, want an error", in, v)
		}
	}
}

// Every entry must be usable: a zero or a missing year would print a figure
// with no provenance.
func TestEmberTableIsComplete(t *testing.T) {
	if len(emberGrid) < 100 {
		t.Fatalf("only %d countries", len(emberGrid))
	}
	for _, e := range emberGrid {
		if e.Value <= 0 || e.Year < 2022 || len(e.ISO3) != 3 {
			t.Errorf("bad entry %+v", e)
		}
	}
}
