package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"k8s.io/client-go/tools/clientcmd"

	"github.com/mfranzon/tare/internal/analyze"
	"github.com/mfranzon/tare/internal/discover"
	"github.com/mfranzon/tare/internal/render"
)

// Shell completion directives for kubectl's Cobra __complete protocol.
// When kubectl completes a plugin ("kubectl tare <TAB>"), it invokes
// kubectl_complete-tare on PATH with the arguments after "tare" and reads
// candidate lines followed by ":<directive>". Installing kubectl_complete-tare
// as a link to kubectl-tare makes completion work out of the box once
// kubectl's own completion is enabled.
const (
	compDirectiveDefault    = 0
	compDirectiveNoSpace    = 2
	compDirectiveNoFileComp = 4
	compDirectiveFilterDirs = 16
)

func isCompleteInvocation(argv0 string, args []string) (rest []string, ok bool) {
	base := filepath.Base(argv0)
	if base == "kubectl_complete-tare" || strings.HasPrefix(base, "kubectl_complete-tare") {
		return args, true
	}
	if len(args) > 0 && args[0] == "__complete" {
		return args[1:], true
	}
	return nil, false
}

var subcommandDescriptions = []struct {
	name, desc string
}{
	{"report", "report right-sizing findings (default)"},
	{"check", "check cluster permissions, Prometheus reachability and metric coverage"},
	{"diff", "write recommendations into manifests for review"},
	{"version", "print the build version"},
	{"help", "show help for a command"},
}

var formatDescriptions = map[string]string{
	"table": "compact table on stdout, summary and warnings on stderr",
	"wide":  "every section, limit, pressure reading and evidence row",
	"tui":   "interactive terminal browser with usage plots",
	"json":  "machine-readable JSON with raw numbers",
	"html":  "self-contained HTML report",
	"patch": "runnable kubectl strategic-merge patch commands",
}

var verdictDescriptions = map[analyze.Verdict]string{
	analyze.Conflicted: "another controller already manages these fields",
	analyze.Saturated:  "starved of CPU, or being OOMKilled",
	analyze.Leaking:    "memory grows without bound; sizing cannot fix it",
	analyze.Blocked:    "a runtime ceiling makes sizing meaningless",
	analyze.Idle:       "no measurable work all window; candidate for deletion",
	analyze.Unverified: "reduction cannot be backed by a saturation signal",
	analyze.Waste:      "requesting far more than it uses",
	analyze.Unset:      "no requests or limits set; starting-point size",
	analyze.Healthy:    "sized appropriately",
	analyze.NoData:     "no metric series in the window",
}

var windowSuggestions = []struct {
	val, desc string
}{
	{"30m", "30 minutes"},
	{"12h", "12 hours"},
	{"7d", "7 days"},
	{"14d", "14 days (default)"},
	{"2w", "2 weeks"},
}

var timeoutSuggestions = []struct {
	val, desc string
}{
	{"5m", "5 minutes"},
	{"10m", "10 minutes (default)"},
	{"30m", "30 minutes"},
}

type boolFlagValue interface {
	flag.Value
	IsBoolFlag() bool
}

func isBoolFlag(f *flag.Flag) bool {
	if bf, ok := f.Value.(boolFlagValue); ok {
		return bf.IsBoolFlag()
	}
	return false
}

// runComplete writes Cobra-compatible completion lines to out.
func runComplete(out io.Writer, args []string) error {
	fs := flag.NewFlagSet("complete", flag.ContinueOnError)
	defineFlags(fs)

	if len(args) == 0 {
		args = []string{""}
	}
	cur := args[len(args)-1]
	prevArgs := args[:len(args)-1]

	// Track subcommand and context/kubeconfig seen before cur.
	var sub, kubeconfig, kubectx string
	var waitingFlag string
	for i := 0; i < len(prevArgs); i++ {
		tok := prevArgs[i]
		if waitingFlag != "" {
			switch waitingFlag {
			case "kubeconfig":
				kubeconfig = tok
			case "context":
				kubectx = tok
			}
			waitingFlag = ""
			continue
		}
		if strings.HasPrefix(tok, "-") {
			trimmed := strings.TrimLeft(tok, "-")
			if name, val, hasEq := strings.Cut(trimmed, "="); hasEq {
				switch name {
				case "kubeconfig":
					kubeconfig = val
				case "context":
					kubectx = val
				}
				continue
			}
			if f := fs.Lookup(trimmed); f != nil && !isBoolFlag(f) {
				waitingFlag = trimmed
			}
			continue
		}
		if sub == "" && commands[tok] {
			sub = tok
		}
	}

	// If the previous token is a value-taking flag, complete its value.
	if waitingFlag != "" {
		cands, dir := completeFlagValue(waitingFlag, cur, kubeconfig, kubectx)
		writeCompletions(out, cands, dir)
		return nil
	}

	// Handle --flag=value or -o=value in the current token.
	if strings.HasPrefix(cur, "-") {
		trimmed := strings.TrimLeft(cur, "-")
		if name, val, hasEq := strings.Cut(trimmed, "="); hasEq {
			if f := fs.Lookup(name); f != nil && !isBoolFlag(f) {
				cands, dir := completeFlagValue(name, val, kubeconfig, kubectx)
				writeCompletions(out, cands, dir)
				return nil
			}
		}
		cands := completeFlags(fs, sub, cur)
		writeCompletions(out, cands, compDirectiveNoFileComp)
		return nil
	}

	// Completing the topic after "help".
	if sub == "help" {
		var cands []string
		for _, c := range subcommandDescriptions {
			if c.name != "help" && strings.HasPrefix(c.name, cur) {
				cands = append(cands, c.name+"\t"+c.desc)
			}
		}
		writeCompletions(out, cands, compDirectiveNoFileComp)
		return nil
	}

	// If no subcommand has been typed yet, complete subcommands.
	if sub == "" {
		var cands []string
		for _, c := range subcommandDescriptions {
			if strings.HasPrefix(c.name, cur) {
				cands = append(cands, c.name+"\t"+c.desc)
			}
		}
		writeCompletions(out, cands, compDirectiveNoFileComp)
		return nil
	}

	writeCompletions(out, nil, compDirectiveNoFileComp)
	return nil
}

func completeFlags(fs *flag.FlagSet, sub, cur string) []string {
	cmd := sub
	if cmd == "" {
		cmd = "report"
	}
	allowed := map[string]bool{}
	for _, name := range usageFlags[cmd] {
		allowed[name] = true
	}
	allows := func(name string) bool {
		if sub == "version" || sub == "help" {
			return false
		}
		if sub == "" {
			return true
		}
		if cmd == "report" {
			return !slices.Contains(diffOnly, name)
		}
		return allowed[name]
	}

	shortOnly := cur == "-"
	var cands []string
	fs.VisitAll(func(f *flag.Flag) {
		if !allows(f.Name) {
			return
		}
		var form string
		if len(f.Name) == 1 {
			if !shortOnly && strings.HasPrefix(cur, "--") {
				return
			}
			form = "-" + f.Name
		} else {
			form = "--" + f.Name
		}
		if strings.HasPrefix(form, cur) || (shortOnly && strings.HasPrefix(form, "--")) {
			desc := firstSentence(f.Usage)
			cands = append(cands, form+"\t"+desc)
		}
	})
	sort.Strings(cands)
	return cands
}

func firstSentence(s string) string {
	if i := strings.IndexAny(s, ".;\n"); i >= 0 {
		return s[:i]
	}
	return s
}

func completeFlagValue(name, val, kubeconfig, kubectx string) ([]string, int) {
	switch name {
	case "o":
		var out []string
		for _, f := range render.Formats {
			if strings.HasPrefix(f, val) {
				out = append(out, f+"\t"+formatDescriptions[f])
			}
		}
		return out, compDirectiveNoFileComp

	case "verdict":
		return completeCommaList(val, func() ([]string, map[string]string) {
			var items []string
			descs := map[string]string{}
			for _, v := range analyze.ReportOrder() {
				s := string(v)
				items = append(items, s)
				descs[s] = verdictDescriptions[v]
			}
			return items, descs
		}), compDirectiveNoFileComp

	case "n", "namespace":
		return completeCommaList(val, func() ([]string, map[string]string) {
			return clusterNamespaces(kubeconfig, kubectx), nil
		}), compDirectiveNoFileComp

	case "context":
		var out []string
		for _, ctxName := range kubeContexts(kubeconfig) {
			if strings.HasPrefix(ctxName, val) {
				out = append(out, ctxName)
			}
		}
		return out, compDirectiveNoFileComp

	case "window":
		var out []string
		for _, w := range windowSuggestions {
			if strings.HasPrefix(w.val, val) {
				out = append(out, w.val+"\t"+w.desc)
			}
		}
		return out, compDirectiveNoFileComp

	case "timeout":
		var out []string
		for _, t := range timeoutSuggestions {
			if strings.HasPrefix(t.val, val) {
				out = append(out, t.val+"\t"+t.desc)
			}
		}
		return out, compDirectiveNoFileComp

	case "repo", "kustomize":
		return nil, compDirectiveFilterDirs

	case "kubeconfig", "pr-body":
		return nil, compDirectiveDefault
	}
	return nil, compDirectiveNoFileComp
}

func completeCommaList(val string, source func() ([]string, map[string]string)) []string {
	items, descs := source()
	var prefix string
	partial := val
	used := map[string]bool{}
	if idx := strings.LastIndex(val, ","); idx >= 0 {
		prefix = val[:idx+1]
		partial = val[idx+1:]
		for _, p := range strings.Split(val[:idx], ",") {
			used[strings.ToUpper(strings.TrimSpace(p))] = true
			used[strings.TrimSpace(p)] = true
		}
	}
	var out []string
	for _, item := range items {
		if used[item] || used[strings.ToUpper(item)] {
			continue
		}
		if strings.HasPrefix(strings.ToLower(item), strings.ToLower(partial)) {
			cand := prefix + item
			if d := descs[item]; d != "" {
				cand += "\t" + d
			}
			out = append(out, cand)
		}
	}
	return out
}

func kubeContexts(kubeconfig string) []string {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		rules.ExplicitPath = kubeconfig
	}
	raw, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}).RawConfig()
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(raw.Contexts))
	for name := range raw.Contexts {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func clusterNamespaces(kubeconfig, kubectx string) []string {
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	kc, err := discover.NewClient(kubeconfig, kubectx)
	if err != nil {
		return nil
	}
	ns, _ := kc.ListNamespaces(ctx)
	return ns
}

func writeCompletions(out io.Writer, cands []string, directive int) {
	for _, c := range cands {
		fmt.Fprintln(out, c)
	}
	fmt.Fprintf(out, ":%d\n", directive)
}
