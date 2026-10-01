// Command kubectl-tare reports Kubernetes resource right-sizing findings.
//
// tare: the weighing term for subtracting the container's own weight.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/mfranzon/tare/internal/analyze"
	"github.com/mfranzon/tare/internal/discover"
	"github.com/mfranzon/tare/internal/metrics"
	"github.com/mfranzon/tare/internal/packing"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	cmd := "report"
	flag.CommandLine.Usage = func() { usage(os.Stderr, cmd) }

	var (
		promURL    = flag.String("prometheus", os.Getenv("TARE_PROMETHEUS"), "Prometheus-compatible endpoint. Omit to auto-discover one in the cluster and query it through the API server proxy.")
		nsFlag     = flag.String("namespace", "", "comma-separated namespaces (default: all, excluding cluster infrastructure)")
		window     = 14 * 24 * time.Hour
		kubeconfig = flag.String("kubeconfig", "", "path to kubeconfig")
		kubectx    = flag.String("context", "", "kubeconfig context")
		output     = flag.String("o", "table", "output format: table (a screenful: summary, warnings, one row per finding), wide (every section, evidence row and limit, for scripts and tickets), tui (interactive), json, html (a self-contained file you can send to someone), or patch (apply-ready strategic merge patches)")
		top        = flag.Int("top", 0, "show only the N biggest opportunities")
		verdicts   = flag.String("verdict", "", "show only these verdicts, comma separated: SATURATED,WASTE,IDLE,LEAKING,CONFLICT,BLOCKED,UNVERIFIED,OK,NO-DATA")
		bySavings  = flag.Bool("by-savings", false, "order by reclaimable resource rather than by severity")
		all        = flag.Bool("all-namespaces", false, "include kube-* and other infrastructure namespaces")
		verbose    = flag.Bool("explain", false, "after the tables, print the sentence behind every evidence code. The tables say what was measured; this says what it means and what it does not rule out.")
		demo       = flag.Bool("demo", false, "report on simulated workloads instead of a cluster, so the output can be seen without Kubernetes or Prometheus")
		timeout    = flag.Duration("timeout", 10*time.Minute, "give up after this long. A cluster-wide run on a large cluster is mostly waiting for the API server and Prometheus")
		redact     = flag.Bool("redact", false, "replace namespace, workload, container and node names with stable hashes, so a report can be sent to someone who should not see them. The hash is stable, not secret: a guessed name can be confirmed")
		showVer    = flag.Bool("version", false, "print the version and exit")
	)
	// -n is what people actually type, and kubectl has trained them to.
	flag.StringVar(nsFlag, "n", "", "shorthand for --namespace")
	flag.BoolVar(verbose, "v", false, "shorthand for --explain")
	repo := flag.String("repo", "", "path to a checkout of your manifests; tare edits the resources in place so the change can be reviewed as a diff")
	writeOut := flag.Bool("write", false, "actually modify the files under --repo (default is to show what would change)")
	kustomizeDir := flag.String("kustomize", "", "directory to write strategic-merge patches into, for repositories managed with Kustomize")
	prBody := flag.String("pr-body", "", "write the pull request body, with the evidence, to this file instead of stdout")
	flag.Var(durationValue{&window}, "window",
		"observation window: 30m, 12h, 7d, 2w. Longer is better; most clusters keep about 15 days.")
	// Strip a leading subcommand before parsing. Go's flag package stops at the
	// first non-flag argument, so without this every flag after "report" is
	// silently ignored: the documented invocation would quietly report on the
	// whole cluster with default settings.
	diffMode, checkMode := false, false
	args := os.Args[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		switch args[0] {
		case "report":
			args = args[1:]
		case "diff":
			args = args[1:]
			diffMode, cmd = true, "diff"
		case "check":
			args = args[1:]
			checkMode, cmd = true, "check"
		case "version":
			printVersion()
			return nil
		case "help":
			// "help diff" is how people ask for one command's flags.
			if len(args) > 1 {
				if _, ok := usageText[args[1]]; ok {
					cmd = args[1]
				}
			}
			flag.CommandLine.Usage()
			return nil
		default:
			return fmt.Errorf("unknown command %q; the commands are \"report\" (the default), \"check\", \"diff\" and \"version\"", args[0])
		}
	}
	if err := flag.CommandLine.Parse(args); err != nil {
		return err
	}
	if *showVer {
		printVersion()
		return nil
	}
	discover.UserAgent = "kubectl-tare/" + buildVersion()

	var namespaces []string
	if *nsFlag != "" {
		// Trimmed because "-n a, b" is what a person types and " b" is not a
		// namespace: without this the space survives into every query and the
		// run reports nothing for it.
		for _, ns := range strings.Split(*nsFlag, ",") {
			if ns = strings.TrimSpace(ns); ns != "" {
				namespaces = append(namespaces, ns)
			}
		}
	}

	// Checked before any cluster call, so a typo costs nothing and --demo is
	// held to the same spelling as a real run.
	if err := checkVerdicts(*verdicts); err != nil {
		return err
	}

	// Demo runs before any cluster call: the whole point is that it works on a
	// laptop with no kubeconfig.
	if *demo {
		if diffMode {
			return fmt.Errorf("--demo has no manifests to diff against")
		}
		rep := demoReport(window)
		rep.VerdictFiltered, rep.BySavings = *verdicts != "", *bySavings
		rep.Findings = filterVerdicts(rep.Findings, *verdicts)
		if *bySavings {
			sortBySavings(rep.Findings)
		}
		if *top > 0 && len(rep.Findings) > *top {
			rep.Findings = rep.Findings[:*top]
		}
		if *redact {
			redactReport(rep)
		}
		return emit(rep, *output, *verbose)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	kc, err := discover.NewClient(*kubeconfig, *kubectx)
	if err != nil {
		return err
	}

	if missing := kc.MissingNamespaces(ctx, namespaces); len(missing) > 0 {
		return fmt.Errorf("no such namespace: %s", strings.Join(missing, ", "))
	}

	if checkMode {
		return runCheck(ctx, os.Stdout, kc, namespaces, *promURL, window, *all)
	}

	progressf("reading pods from %s", scopeName(namespaces))
	containers, err := kc.Containers(ctx, namespaces)
	if err != nil {
		return deadlineHint(ctx, err, *timeout)
	}
	if !*all {
		kept := containers[:0]
		for _, c := range containers {
			if !discover.SkipNamespace(c.Namespace) {
				kept = append(kept, c)
			}
		}
		containers = kept
	}

	progressf("%d containers", len(containers))

	src, err := buildSource(ctx, kc, *promURL)
	if err != nil {
		return deadlineHint(ctx, err, *timeout)
	}

	// Ask for a window that exists. Confidence is driven by sample coverage
	// against the window, so requesting two weeks from a backend holding three
	// days rates every finding "low" - the tool reporting that it knows
	// nothing, on the one run that decides whether anyone keeps it.
	windowNote := humanDuration(window)
	if pr, ok := src.(metrics.WindowProber); ok {
		switch avail, err := pr.AvailableWindow(namespaces, window); {
		case err != nil:
			fmt.Fprintf(os.Stderr, "could not measure available history (%v); using %s as asked\n",
				err, humanDuration(window))
		case avail == 0:
			windowNote = fmt.Sprintf("%s requested, under an hour available", humanDuration(window))
		case avail < window:
			windowNote = fmt.Sprintf("%s requested, %s available", humanDuration(window), humanDuration(avail))
			window = avail
		}
	}

	progressf("querying %s over %s", src.Describe(), windowNote)
	obs, err := src.Observe(namespaces, window)
	if err != nil {
		return deadlineHint(ctx, err, *timeout)
	}

	coverage, covMissing := coverageNote(containers, obs)

	constraints, err := kc.LoadConstraints(ctx, namespaces)
	if err != nil {
		return deadlineHint(ctx, err, *timeout)
	}
	// Listing nodes is a cluster-scoped read a namespace-scoped user may not
	// have. Its absence costs the per-node findings and the node-count delta,
	// and is reported rather than silently producing a report that looks
	// complete.
	inv := kc.Nodes(ctx)

	// Node utilisation is optional: node-exporter is ubiquitous but not
	// universal, and a source that cannot answer simply is not asked. The
	// address-to-name join happens here, because this is where the inventory
	// and the metrics backend are both in scope; the analyzer only ever sees
	// node names.
	nodeUtil := map[string]metrics.NodeObservation{}
	if no, ok := src.(metrics.NodeObserver); ok && !inv.Partial {
		if byIP, err := no.ObserveNodes(inv.IPs(), window); err == nil {
			for ip, o := range byIP {
				if n, ok := inv.ByIP(ip); ok {
					nodeUtil[n.Name] = o
				}
			}
		}
	}

	cluster := analyze.Cluster{Constraints: constraints, Nodes: inv, NodeUtil: nodeUtil}
	findings := analyze.RunWithCluster(containers, obs, cluster, window, analyze.DefaultThresholds())

	// The node-count delta is computed over the WHOLE cluster, before the
	// display filters below cut the finding list down. Packing a filtered
	// cluster would remove pods from nodes they are still occupying and
	// manufacture free capacity out of a --top flag.
	nodeDelta := packing.Result{}
	var commitment []packing.Commitment
	var blockingPDBs []string
	if !inv.Partial {
		// Disruption budgets decide whether a node CAN be drained. A node-count
		// saving that the cluster would refuse to perform is not a saving.
		pdbs := kc.PDBs(ctx, namespaces)
		for _, p := range pdbs {
			if p.Blocking {
				blockingPDBs = append(blockingPDBs, p.Why)
			}
		}
		if pods, err := kc.Pods(ctx, namespaces, pdbs); err == nil {
			nodeDelta = packing.Replay(inv, pods, sizerFor(findings))
			commitment = packing.Commitments(inv, pods, packing.UnlimitedCounts(pods))
		} else {
			nodeDelta.SelfCheck = fmt.Sprintf("cannot list pods (%v)", err)
		}
	} else {
		nodeDelta.SelfCheck = inv.Reason
	}
	// A read tare was refused removes a finding from the report. It belongs in
	// the same header block as a missing metric, and in every output format,
	// because the alternative is a report that looks complete and is quietly
	// missing the part RBAC denied.
	missingSignals := analyze.SummariseHealth(src.Health())
	for _, w := range kc.Warnings() {
		missingSignals = append(missingSignals, analyze.SignalGap{Signal: "rbac", Status: analyze.GapDenied, Detail: w})
	}

	findings = filterVerdicts(findings, *verdicts)
	if *bySavings {
		sortBySavings(findings)
	}
	if *top > 0 && len(findings) > *top {
		findings = findings[:*top]
	}

	rep := &report{
		Findings: findings, Nodes: nodeDelta, Commitment: commitment,
		DrainBlockers: blockingPDBs, Missing: missingSignals,
		Source: src.Describe(), Window: windowNote, Coverage: coverage,
		WindowUsed: humanDuration(window), VerdictFiltered: *verdicts != "", BySavings: *bySavings,
	}
	if covMissing > 0 {
		rep.CoverageMissing, rep.CoverageTotal = covMissing, len(containers)
	}
	if prom, ok := src.(*metrics.Prom); ok && *output == "tui" {
		rep.Usage = promUsage(prom, containers, window)
	}
	if *redact {
		redactReport(rep)
	}

	// Only diff with the default format skips emit. This used to send patch,
	// json and html to emit and everything else to the table, so -o tui
	// against a real cluster printed a table and only --demo ever opened it.
	if !diffMode || *output != "table" {
		return emit(rep, *output, *verbose)
	}
	if *repo == "" && *kustomizeDir == "" {
		return fmt.Errorf("diff needs --repo <checkout> or --kustomize <dir>")
	}
	if *repo != "" {
		if err := repoLooksLikeManifests(*repo); err != nil {
			return err
		}
	}
	return runDiff(findings, *repo, *writeOut, *kustomizeDir, *prBody, src.Describe(), windowNote)
}

// coverageNote states how many of the containers the cluster has were actually
// found in the metrics backend.
//
// Observe has already answered this for free: a container with no series has no
// observation. It is worth a line in the header because every other total in
// the report is computed over the containers that had data, and a backend
// scraping two namespaces out of forty passes every signal check there is.
func coverageNote(containers []discover.Container, obs map[metrics.Key]*metrics.Observation) (string, int) {
	var missing int
	for _, c := range containers {
		// An entry is not the same as data. kube-state-metrics publishes a
		// series for every container in the cluster whether or not anything
		// ever scraped its cgroup, so a container whose kubelet never reported
		// still arrives here with an observation attached and nothing in it.
		// Samples counts memory buckets, which is what every recommendation is
		// actually built from.
		o := obs[metrics.Key{Namespace: c.Namespace, Pod: c.Pod, Container: c.Name}]
		if o == nil || o.Samples == 0 {
			missing++
		}
	}
	if missing == 0 || len(containers) == 0 {
		return "", 0
	}
	return fmt.Sprintf("metrics cover %d of %d containers; the other %d have no series in this backend "+
		"and appear as NO-DATA. A workload with other pods covered is still sized, from those pods",
		len(containers)-missing, len(containers), missing), missing
}

// emit renders one report in the requested format. Every format goes through
// here so that adding one cannot quietly skip the parts of the report that say
// what the run could not see.
func emit(r *report, format string, verbose bool) error {
	switch format {
	case "patch":
		printPatches(r.Findings)
		return nil
	case "json":
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		// An object rather than a bare array of findings: the node-count delta
		// is the figure that corresponds to a bill, and a consumer that only
		// sums reclaimed millicores is computing the thing this report exists
		// to stop people reporting.
		return enc.Encode(struct {
			Findings      []analyze.Finding    `json:"findings"`
			Nodes         packing.Result       `json:"nodes"`
			Commitment    []packing.Commitment `json:"commitment"`
			DrainBlockers []string             `json:"drainBlockers"`
			Missing       []analyze.SignalGap  `json:"missingSignals"`
			Coverage      string               `json:"coverage,omitempty"`
			Demo          bool                 `json:"demo,omitempty"`
		}{r.Findings, r.Nodes, r.Commitment, r.DrainBlockers, r.Missing, r.Coverage, r.Demo})
	case "html":
		return writeHTML(os.Stdout, r)
	case "tui":
		return runTUI(r)
	case "table":
		printCompact(r)
		if verbose {
			printExplanations(r)
		} else {
			compactHint()
		}
		return nil
	case "wide":
		printMeta(r)
		printSignalGaps(r.Missing)
		printFindings(r.Findings)
		printEvidence(r.Findings)
		printVerdicts(r.Findings)
		printTotals(r.Findings)
		printNodes(r)
		printDrainBlockers(r.DrainBlockers)
		printCommitment(r.Commitment)
		if verbose {
			printExplanations(r)
		} else {
			// stderr, so a redirected report stays pure data.
			fmt.Fprintln(os.Stderr, "run with --explain for the reasoning behind every code above")
		}
		return nil
	}
	return fmt.Errorf("unknown output format %q; the formats are table, wide, tui, json, html and patch", format)
}

// buildSource resolves where to read history from. An explicit URL always
// wins; otherwise the cluster is asked where its own Prometheus is, and the
// query goes through the API server proxy so no port forward is needed.
func buildSource(ctx context.Context, kc *discover.Client, promURL string) (metrics.Source, error) {
	if promURL != "" {
		return metrics.NewProm(promURL), nil
	}

	candidates, err := kc.FindPrometheus(ctx)
	if err != nil {
		return nil, fmt.Errorf("looking for a metrics service: %w", err)
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf(
			"no Prometheus-compatible service found in this cluster.\n" +
				"  tare reads history from Prometheus, Thanos, Mimir or VictoriaMetrics.\n" +
				"  Point it at one explicitly:\n" +
				"    kubectl tare report --prometheus http://localhost:9090\n" +
				"  or set TARE_PROMETHEUS in the environment.")
	}

	c := candidates[0]
	fmt.Fprintf(os.Stderr, "using %s\n", c)
	if len(candidates) > 1 {
		fmt.Fprintf(os.Stderr, "  (%d other candidates found; pass --prometheus to choose a different one)\n",
			len(candidates)-1)
	}
	return metrics.New(&metrics.ProxyTransport{
		Clientset: kc.Clientset(),
		Namespace: c.Namespace, Service: c.Service, Port: c.Port, Scheme: c.Scheme,
	}), nil
}

// termWidth is the terminal width to lay the table out for. COLUMNS is what a
// shell exports and what a user can override; the fallback is the width a
// terminal is still expected to have when nothing says otherwise.
func termWidth() int {
	if c := os.Getenv("COLUMNS"); c != "" {
		if n, err := strconv.Atoi(c); err == nil && n > 40 {
			return n
		}
	}
	return 120
}

// shortWorkload renders namespace/Kind/name inside max columns. Kinds are
// abbreviated the way kubectl abbreviates them, and a name too long even then
// loses its middle rather than its tail: the tail is what distinguishes
// prometheus-kps-...-prometheus from prometheus-kps-...-operator.
func shortWorkload(ns, workload string, max int) string {
	kind, name, ok := strings.Cut(workload, "/")
	if ok {
		switch kind {
		case "Deployment":
			kind = "deploy"
		case "StatefulSet":
			kind = "sts"
		case "DaemonSet":
			kind = "ds"
		case "ReplicaSet":
			kind = "rs"
		case "ReplicationController":
			kind = "rc"
		case "CronJob":
			kind = "cronjob"
		default:
			kind = strings.ToLower(kind)
		}
		workload = kind + "/" + name
	}
	full := ns + "/" + workload
	if len(full) <= max {
		return full
	}
	// The kind is the least informative part - almost everything is a
	// Deployment - so it goes before any of the name does.
	if short := ns + "/" + name; ok && len(short) <= max {
		return short
	}
	full = ns + "/" + name
	if len(full) <= max {
		return full
	}
	if max < 8 {
		return full[:max]
	}
	head := (max - 1) / 3
	tail := max - 1 - head
	return full[:head] + "…" + full[len(full)-tail:]
}

func cpuCell(f analyze.Finding) string {
	if f.RecCPU == 0 {
		return fmt.Sprintf("%s>-", milli(f.CPURequest))
	}
	return fmt.Sprintf("%s>%dm", milli(f.CPURequest), f.RecCPU)
}

// limitCell shows the CPU limit recommendation, which is deliberately looser
// than the request: a request sized to a percentile must not double as a limit
// that throttles every burst.
func limitCell(f analyze.Finding) string {
	if f.RecCPULimit == 0 {
		return fmt.Sprintf("%s>-", milli(f.CPULimit))
	}
	return fmt.Sprintf("%s>%dm", milli(f.CPULimit), f.RecCPULimit)
}

func memCell(f analyze.Finding) string {
	if f.MemCensored {
		return fmt.Sprintf("%s>?", human(f.MemRequest))
	}
	if f.RecMem == 0 {
		return fmt.Sprintf("%s>-", human(f.MemRequest))
	}
	return fmt.Sprintf("%s>%s", human(f.MemRequest), human(f.RecMem))
}

// pressureCell must never print "none" for a signal that was not measured.
// "not measured" and "measured as zero" look identical in a table unless the
// difference is spelled out, and conflating them is how a throttled container
// gets reported as comfortable.
func memLimitCell(f analyze.Finding) string {
	if f.MemCensored {
		return fmt.Sprintf("%s>?", human(f.MemLimit))
	}
	if f.RecMemLimit == 0 {
		return fmt.Sprintf("%s>-", human(f.MemLimit))
	}
	return fmt.Sprintf("%s>%s", human(f.MemLimit), human(f.RecMemLimit))
}

func pressureCell(f analyze.Finding) string {
	switch {
	case f.OOMKilled:
		return "OOMKilled"
	// Sustained first, peak in brackets. Showing only the peak told the reader
	// a container was throttled 6% of the time when the true figure was 0.2%
	// and one minute had been unlucky.
	case f.HasPSI() && f.PSIWaitingPeak > 0.001:
		if !f.HasPSIAvg() {
			return fmt.Sprintf("psi ?/%.3f", f.PSIWaitingPeak)
		}
		return fmt.Sprintf("psi %.4f/%.3f", f.PSIWaiting, f.PSIWaitingPeak)
	case f.HasThrottle() && f.ThrottlePeak > 0.001:
		if !f.HasThrottleAvg() {
			return fmt.Sprintf("thr ?/%.1f%%", f.ThrottlePeak*100)
		}
		return fmt.Sprintf("thr %.1f/%.1f%%", f.Throttle*100, f.ThrottlePeak*100)
	case f.HasPSI() || f.HasThrottle():
		return "none"
	}
	return "unmeasured"
}

func milli(m int64) string {
	if m == 0 {
		return "unset"
	}
	return fmt.Sprintf("%dm", m)
}

func human(b int64) string {
	switch {
	case b == 0:
		return "unset"
	case b >= 1<<30:
		return fmt.Sprintf("%.1fGi", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.0fMi", float64(b)/(1<<20))
	default:
		return fmt.Sprintf("%dB", b)
	}
}

// promUsage maps a finding back to the pods it was computed from. Built before
// redaction, which drops it: the lookup is by the names redaction hashes.
func promUsage(prom *metrics.Prom, containers []discover.Container, window time.Duration) func(analyze.Finding) (metrics.Series, metrics.Series, error) {
	pods := map[string][]string{}
	for _, c := range containers {
		k := c.Namespace + "/" + c.Workload + "/" + c.Name
		pods[k] = append(pods[k], c.Pod)
	}
	return func(f analyze.Finding) (metrics.Series, metrics.Series, error) {
		// Its own deadline: the report's has been running since startup and a
		// TUI can stay open far longer than --timeout.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return prom.Usage(ctx, f.Namespace, pods[f.Namespace+"/"+f.Workload+"/"+f.Container], f.Container, window, plotBuckets)
	}
}
