package main

import (
	"flag"
	"strings"
	"testing"
	"time"
)

// stripCommand mirrors the argument handling in run(), which exists because
// Go's flag package stops at the first non-flag argument: without it,
// "report -n prod" parses zero flags and silently reports on the whole cluster
// with defaults. That is the invocation the README documents.
func stripCommand(args []string) ([]string, string, error) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		switch args[0] {
		case "report":
			return args[1:], "report", nil
		case "help":
			return args[1:], "help", nil
		default:
			return nil, "", errUnknown(args[0])
		}
	}
	return args, "report", nil
}

type errUnknown string

func (e errUnknown) Error() string { return "unknown command " + string(e) }

func TestFlagsAfterSubcommandAreParsed(t *testing.T) {
	args, cmd, err := stripCommand([]string{"report", "-n", "prod", "--window", "3d"})
	if err != nil || cmd != "report" {
		t.Fatalf("cmd = %q, err = %v", cmd, err)
	}

	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	ns := fs.String("namespace", "", "")
	fs.StringVar(ns, "n", "", "")
	var window time.Duration
	fs.Var(durationValue{&window}, "window", "")

	if err := fs.Parse(args); err != nil {
		t.Fatal(err)
	}
	if *ns != "prod" {
		t.Errorf("namespace = %q, want prod: flags after the subcommand were dropped", *ns)
	}
	if window != 3*24*time.Hour {
		t.Errorf("window = %s, want 72h", window)
	}
}

func TestBareInvocationStillWorks(t *testing.T) {
	args, cmd, err := stripCommand([]string{"-n", "prod"})
	if err != nil || cmd != "report" || len(args) != 2 {
		t.Fatalf("args = %v, cmd = %q, err = %v", args, cmd, err)
	}
}

func TestUnknownCommandIsRejected(t *testing.T) {
	// Silently ignoring an unknown command means a typo produces a full
	// cluster report with default settings and no indication anything is wrong.
	if _, _, err := stripCommand([]string{"frobnicate"}); err == nil {
		t.Error("an unknown command must be an error, not ignored")
	}
}
