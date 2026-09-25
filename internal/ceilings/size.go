package ceilings

import (
	"fmt"
	"strconv"
	"strings"
)

// parseJVMSize parses a JVM size argument: 512m, 4G, 1048576k, or a plain byte
// count. JVM suffixes are binary and case-insensitive, and unlike Go's
// GOMEMLIMIT the JVM does not accept "iB" forms.
func parseJVMSize(s string) (int64, error) {
	if s == "" {
		return 0, fmt.Errorf("empty size")
	}
	mult := int64(1)
	switch s[len(s)-1] {
	case 'k', 'K':
		mult, s = 1<<10, s[:len(s)-1]
	case 'm', 'M':
		mult, s = 1<<20, s[:len(s)-1]
	case 'g', 'G':
		mult, s = 1<<30, s[:len(s)-1]
	case 't', 'T':
		mult, s = 1<<40, s[:len(s)-1]
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid JVM size %q", s)
	}
	return n * mult, nil
}

// goMemUnits are GOMEMLIMIT's accepted suffixes, longest first so that "KiB"
// is matched before "B". Go deliberately rejects decimal units such as "MB".
var goMemUnits = []struct {
	suffix string
	mult   int64
}{
	{"TiB", 1 << 40},
	{"GiB", 1 << 30},
	{"MiB", 1 << 20},
	{"KiB", 1 << 10},
	{"B", 1},
}

// parseGoMemLimit parses a GOMEMLIMIT value. The literal "off" means no limit.
func parseGoMemLimit(s string) (bytes int64, off bool, err error) {
	s = strings.TrimSpace(s)
	if s == "off" {
		return 0, true, nil
	}
	for _, u := range goMemUnits {
		if !strings.HasSuffix(s, u.suffix) {
			continue
		}
		n, perr := strconv.ParseInt(strings.TrimSuffix(s, u.suffix), 10, 64)
		if perr != nil || n <= 0 {
			return 0, false, fmt.Errorf("invalid GOMEMLIMIT %q", s)
		}
		return n * u.mult, false, nil
	}
	n, perr := strconv.ParseInt(s, 10, 64)
	if perr != nil || n <= 0 {
		return 0, false, fmt.Errorf("invalid GOMEMLIMIT %q", s)
	}
	return n, false, nil
}

// humanBytes renders a byte count the way a Kubernetes manifest would.
func humanBytes(b int64) string {
	switch {
	case b >= 1<<30 && b%(1<<30) == 0:
		return fmt.Sprintf("%dGi", b/(1<<30))
	case b >= 1<<20 && b%(1<<20) == 0:
		return fmt.Sprintf("%dMi", b/(1<<20))
	case b >= 1<<10 && b%(1<<10) == 0:
		return fmt.Sprintf("%dKi", b/(1<<10))
	default:
		return fmt.Sprintf("%d", b)
	}
}

// humanVRAM renders a GPU memory figure for a human to compare against a
// device size.
//
// It is separate from humanBytes on purpose. humanBytes must round-trip as a
// Kubernetes quantity, so it falls back to exact units and renders 16e9 as
// "15625000Ki". Model weights are never a Kubernetes quantity - a GPU is
// allocated whole - so here readability wins and an approximation is correct.
func humanVRAM(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1fGiB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.0fMiB", float64(b)/(1<<20))
	}
	return fmt.Sprintf("%dB", b)
}

// flagValue returns the value of --name=value or of --name value, scanning argv
// so that both spellings are covered.
func flagValue(argv []string, names ...string) (value string, raw string, found bool) {
	for i, a := range argv {
		for _, n := range names {
			if strings.HasPrefix(a, n+"=") {
				return strings.TrimPrefix(a, n+"="), a, true
			}
			if a == n && i+1 < len(argv) {
				return argv[i+1], n + " " + argv[i+1], true
			}
		}
	}
	return "", "", false
}
