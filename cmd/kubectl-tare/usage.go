package main

import (
	"flag"
	"fmt"
	"io"
	"slices"
)

// Every subcommand parses the same flag set, so each one's help lists only
// the flags it reads. Showing report's list under "diff --help" hid --repo
// among twenty flags diff ignores.
var usageText = map[string]string{
	"report": `tare: right-sizes Kubernetes workloads from saturation evidence.

Usage:
  kubectl tare report [flags]

Examples:
  kubectl tare report                          whole cluster
  kubectl tare report -n prod                  one namespace
  kubectl tare report --top 20 --by-savings    the biggest opportunities
  kubectl tare report -o wide                  every section and evidence row, for a script or a ticket
  kubectl tare report --explain                the reasoning behind every evidence code
  kubectl tare report --verdict SATURATED      only what is hurting now
  kubectl tare report -o patch > fix.sh        runnable kubectl patch commands
  kubectl tare report -o tui                   browse the findings interactively
  kubectl tare report -o html > report.html    a self-contained file to send to whoever owns the workload
  kubectl tare report --demo                   simulated workloads, so you can see the output with no cluster
  kubectl tare check                           what this cluster can and cannot answer, before running a report
  kubectl tare diff --repo ./manifests         show the change as a reviewable diff
  kubectl tare diff --repo ./manifests --write apply it, then run git diff and open a PR
  kubectl tare version                         which build this is

kubectl tare help diff and kubectl tare help check list those commands' flags.

Flags:
`,
	"diff": `tare diff: writes tare's recommendations into your manifests, so the change is a reviewable diff.

Usage:
  kubectl tare diff --repo <checkout> [flags]
  kubectl tare diff --kustomize <dir> [flags]

Examples:
  kubectl tare diff --repo ./manifests                   show what would change
  kubectl tare diff --repo ./manifests --write           apply it, then run git diff and open a PR
  kubectl tare diff --repo ./manifests -n prod --verdict SATURATED
                                                         only the starved containers in one namespace

Flags:
`,
	"check": `tare check: what this cluster can and cannot answer, before running a report.

Usage:
  kubectl tare check [flags]

Flags:
`,
}

var scopeFlags = []string{"namespace", "n", "all-namespaces", "window", "prometheus", "kubeconfig", "context", "timeout"}

var usageFlags = map[string][]string{
	"diff":  append([]string{"repo", "write", "kustomize", "pr-body", "verdict", "top"}, scopeFlags...),
	"check": scopeFlags,
}

// diffOnly are the flags only diff reads. They are hidden from report's help,
// and checkCombination rejects them anywhere else.
var diffOnly = []string{"repo", "write", "kustomize", "pr-body"}

func usage(w io.Writer, cmd string) {
	fmt.Fprint(w, usageText[cmd])
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(w)
	keep := map[string]bool{}
	for _, name := range usageFlags[cmd] {
		keep[name] = true
	}
	flag.VisitAll(func(f *flag.Flag) {
		if (cmd == "report" && !slices.Contains(diffOnly, f.Name)) || keep[f.Name] {
			fs.Var(f.Value, f.Name, f.Usage)
			// Var records the current value as the default; restore the real
			// one so "diff -n prod --help" does not claim prod is the default.
			fs.Lookup(f.Name).DefValue = f.DefValue
		}
	})
	fs.PrintDefaults()
}
