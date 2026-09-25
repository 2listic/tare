package main

import (
	"strings"
	"testing"

	"github.com/mfranzon/tare/internal/analyze"
)

// TestCheckVerdictsRejectsAMisspelling: a --verdict value naming no verdict
// used to filter every finding away, so the report printed a bare header and
// exited 0. On a cluster with plenty to fix that reads as "nothing to fix",
// which is the failure shape this tool exists to avoid.
func TestCheckVerdictsRejectsAMisspelling(t *testing.T) {
	err := checkVerdicts("SATURATD")
	if err == nil {
		t.Fatal("checkVerdicts(SATURATD) = nil, want an error naming the unknown verdict")
	}
	if !strings.Contains(err.Error(), "SATURATD") {
		t.Errorf("error %q does not quote what the user typed", err)
	}
	if !strings.Contains(err.Error(), string(analyze.Saturated)) {
		t.Errorf("error %q does not list the verdicts that would have worked", err)
	}
}

// TestCheckVerdictsAcceptsEveryRealVerdict pins the accepted set to
// analyze.ReportOrder, so a verdict added later cannot become unselectable.
func TestCheckVerdictsAcceptsEveryRealVerdict(t *testing.T) {
	for _, v := range analyze.ReportOrder() {
		if err := checkVerdicts(string(v)); err != nil {
			t.Errorf("checkVerdicts(%s) = %v, want nil", v, err)
		}
	}
	if err := checkVerdicts(" saturated , waste "); err != nil {
		t.Errorf("checkVerdicts with padding and lower case = %v, want nil", err)
	}
	if err := checkVerdicts(""); err != nil {
		t.Errorf("checkVerdicts(\"\") = %v, want nil: no filter is not a bad filter", err)
	}
}
