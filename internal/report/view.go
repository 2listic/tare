package report

import (
	"fmt"
	"strings"

	"github.com/mfranzon/tare/internal/analyze"
)

// CheckVerdicts rejects a --verdict value that names no verdict.
//
// Without it a misspelling matches nothing, so the report prints a bare header
// and exits 0. "SATURATD" and a genuinely healthy cluster produce identical
// output, and the operator believes the second one. The accepted set is
// analyze.ReportOrder rather than a list written here, so a verdict added
// later cannot become unselectable.
func CheckVerdicts(want string) error {
	if want == "" {
		return nil
	}
	known := map[string]bool{}
	for _, v := range analyze.ReportOrder() {
		known[string(v)] = true
	}
	var unknown []string
	for _, v := range strings.Split(want, ",") {
		if name := strings.ToUpper(strings.TrimSpace(v)); name != "" && !known[name] {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	valid := make([]string, 0, len(analyze.ReportOrder()))
	for _, v := range analyze.ReportOrder() {
		valid = append(valid, string(v))
	}
	return fmt.Errorf("no such verdict: %s. Valid verdicts are %s",
		strings.Join(unknown, ", "), strings.Join(valid, ", "))
}

func FilterVerdicts(fs []analyze.Finding, want string) []analyze.Finding {
	if want == "" {
		return fs
	}
	keep := map[string]bool{}
	for _, v := range strings.Split(want, ",") {
		keep[strings.ToUpper(strings.TrimSpace(v))] = true
	}
	out := fs[:0]
	for _, f := range fs {
		if keep[string(f.Verdict)] {
			out = append(out, f)
		}
	}
	return out
}
