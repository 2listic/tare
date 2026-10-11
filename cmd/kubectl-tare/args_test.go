package main

import (
	"flag"
	"testing"
	"time"
)

// testFlags builds the subset of run()'s flags these tests need.
func testFlags() (*flag.FlagSet, *string, *time.Duration) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	ns := fs.String("namespace", "", "")
	fs.StringVar(ns, "n", "", "")
	fs.String("o", "table", "")
	fs.Bool("demo", false, "")
	window := new(time.Duration)
	fs.Var(durationValue{window}, "window", "")
	return fs, ns, window
}

// Go's flag package stops at the first non-flag argument. Without parseArgs,
// "report -n prod" parsed zero flags and silently reported on the whole
// cluster, and "-n prod check" parsed -n and then ran a report instead of the
// check, exiting 0 where a CI gate expected a verdict.
func TestFlagsOnEitherSideOfTheSubcommand(t *testing.T) {
	for _, tc := range []struct {
		args []string
		sub  string
	}{
		{[]string{"report", "-n", "prod", "--window", "3d"}, "report"},
		{[]string{"-n", "prod", "--window", "3d"}, "report"},
		{[]string{"-n", "prod", "check", "--window", "3d"}, "check"},
		{[]string{"--window", "3d", "diff", "-n", "prod"}, "diff"},
	} {
		fs, ns, window := testFlags()
		sub, _, err := parseArgs(fs, tc.args, nil)
		if err != nil {
			t.Fatalf("%v: %v", tc.args, err)
		}
		if sub != tc.sub || *ns != "prod" || *window != 3*24*time.Hour {
			t.Errorf("%v: sub=%q ns=%q window=%s", tc.args, sub, *ns, *window)
		}
	}
}

// A leftover argument used to be ignored, which is how "report foo" and
// "-n a b" (a space where a comma belongs) ran on the wrong scope.
func TestLeftoverArgumentsAreErrors(t *testing.T) {
	for _, args := range [][]string{
		{"report", "foo"},
		{"-n", "a", "b"},
		{"check", "diff"},
		{"frobnicate"},
	} {
		fs, _, _ := testFlags()
		if sub, _, err := parseArgs(fs, args, nil); err == nil {
			t.Errorf("%v parsed as %q, want an error", args, sub)
		}
	}
	fs, _, _ := testFlags()
	if sub, topic, err := parseArgs(fs, []string{"help", "diff"}, nil); err != nil || sub != "help" || topic != "diff" {
		t.Errorf("help diff: sub=%q topic=%q err=%v", sub, topic, err)
	}
}

// Each of these used to be accepted and then ignored: diff -o json skipped
// the diff and exited 0, check --demo printed the demo report, --write without
// diff wrote nothing, and a misspelled -o failed only after the whole run.
func TestIgnoredCombinationsAreRejected(t *testing.T) {
	for _, tc := range []struct {
		sub, output  string
		demo, redact bool
		set          []string
		ok           bool
	}{
		{"report", "table", false, false, nil, true},
		{"report", "json", true, true, []string{"o", "demo"}, true},
		{"diff", "table", false, false, []string{"repo", "write"}, true},
		{"report", "jsn", false, false, []string{"o"}, false},
		{"diff", "json", false, false, []string{"o", "repo", "write"}, false},
		{"diff", "table", false, true, []string{"repo"}, false},
		{"check", "table", true, false, []string{"demo"}, false},
		{"check", "json", false, false, []string{"o"}, false},
		{"report", "table", false, false, []string{"write"}, false},
	} {
		set := map[string]bool{}
		for _, f := range tc.set {
			set[f] = true
		}
		if err := checkCombination(tc.sub, tc.output, tc.demo, tc.redact, set); (err == nil) != tc.ok {
			t.Errorf("%+v: err = %v", tc, err)
		}
	}
}
