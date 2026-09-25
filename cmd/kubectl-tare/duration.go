package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// durationValue accepts day and week suffixes on top of Go's own units.
//
// This tool's whole subject is weeks of history, so "14d" is the natural thing
// to type. time.ParseDuration stops at hours, and rejecting the obvious input
// with "parse error" is a poor first impression.
type durationValue struct{ d *time.Duration }

func (v durationValue) String() string {
	if v.d == nil {
		return ""
	}
	return humanDuration(*v.d)
}

func (v durationValue) Set(s string) error {
	d, err := parseDuration(s)
	if err != nil {
		return err
	}
	*v.d = d
	return nil
}

func parseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	for suffix, unit := range map[string]time.Duration{
		"w": 7 * 24 * time.Hour,
		"d": 24 * time.Hour,
	} {
		if !strings.HasSuffix(s, suffix) {
			continue
		}
		n, err := strconv.ParseFloat(strings.TrimSuffix(s, suffix), 64)
		if err != nil || n <= 0 {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		return time.Duration(n * float64(unit)), nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q: use forms like 30m, 12h, 7d or 2w", s)
	}
	if d <= 0 {
		return 0, fmt.Errorf("duration must be positive, got %q", s)
	}
	return d, nil
}

func humanDuration(d time.Duration) string {
	switch {
	case d%(7*24*time.Hour) == 0:
		return fmt.Sprintf("%dw", int(d/(7*24*time.Hour)))
	case d%(24*time.Hour) == 0:
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	return d.String()
}
