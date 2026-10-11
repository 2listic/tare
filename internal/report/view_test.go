package report

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
	err := CheckVerdicts("SATURATD")
	if err == nil {
		t.Fatal("CheckVerdicts(SATURATD) = nil, want an error naming the unknown verdict")
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
		if err := CheckVerdicts(string(v)); err != nil {
			t.Errorf("CheckVerdicts(%s) = %v, want nil", v, err)
		}
	}
	if err := CheckVerdicts(" saturated , waste "); err != nil {
		t.Errorf("CheckVerdicts with padding and lower case = %v, want nil", err)
	}
	if err := CheckVerdicts(""); err != nil {
		t.Errorf("CheckVerdicts(\"\") = %v, want nil: no filter is not a bad filter", err)
	}
}
