package main

import (
	"testing"
	"time"

	"github.com/2listic/tare/internal/render"
)

func TestParseDuration(t *testing.T) {
	day := 24 * time.Hour
	for _, tc := range []struct {
		in   string
		want time.Duration
		bad  bool
	}{
		{in: "14d", want: 14 * day},
		{in: "1d", want: day},
		{in: "2w", want: 14 * day},
		{in: "12h", want: 12 * time.Hour},
		{in: "30m", want: 30 * time.Minute},
		{in: "1.5d", want: 36 * time.Hour},
		{in: "0d", bad: true},
		{in: "-3d", bad: true},
		{in: "0s", bad: true},
		{in: "yesterday", bad: true},
		{in: "", bad: true},
	} {
		got, err := parseDuration(tc.in)
		if tc.bad {
			if err == nil {
				t.Errorf("%q: expected an error, got %s", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%q = %s, want %s", tc.in, got, tc.want)
		}
	}
}

func TestHumanDuration(t *testing.T) {
	for in, want := range map[time.Duration]string{
		14 * 24 * time.Hour: "2w",
		3 * 24 * time.Hour:  "3d",
		6 * time.Hour:       "6h",
		90 * time.Minute:    "1h30m0s",
	} {
		if got := render.HumanDuration(in); got != want {
			t.Errorf("%s = %q, want %q", in, got, want)
		}
	}
}
