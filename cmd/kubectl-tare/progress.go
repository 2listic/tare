package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"golang.org/x/term"
)

// progressf says what the run is waiting for.
//
// A cluster-wide report is mostly a pod list and a dozen range queries, and on
// a large cluster that is minutes of complete silence followed, at worst, by a
// timeout. Silence is indistinguishable from a hang, and the first thing a new
// user does about a hang is kill it.
//
// It writes to stderr, and only to a terminal: a report piped into jq or a file
// belongs to a script, and a script does not need to be told what is happening.
func progressf(format string, args ...any) {
	if !term.IsTerminal(int(os.Stderr.Fd())) {
		return
	}
	fmt.Fprintf(os.Stderr, "... "+format+"\n", args...)
}

func scopeName(namespaces []string) string {
	if len(namespaces) == 0 {
		return "every namespace"
	}
	return "namespace " + strings.Join(namespaces, ", ")
}

// deadlineHint turns the one error a large cluster is most likely to produce
// into an instruction. A bare "context deadline exceeded" tells the user
// nothing about which of the two knobs to reach for.
func deadlineHint(ctx context.Context, err error, timeout time.Duration) error {
	if err == nil {
		return nil
	}
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("%w\n  the run did not finish within --timeout %s.\n"+
		"  Narrow it:  kubectl tare report -n <namespace>\n"+
		"  Or wait longer:  kubectl tare report --timeout 30m\n"+
		"  A shorter --window also makes the Prometheus queries cheaper", err, timeout)
}
