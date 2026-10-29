// Command kubectl-tare reports Kubernetes resource right-sizing findings.
//
// tare: the weighing term for subtracting the container's own weight.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/2listic/tare/internal/analyze"
	"github.com/2listic/tare/internal/discover"
	"github.com/2listic/tare/internal/metrics"
	"github.com/2listic/tare/internal/packing"
	"github.com/2listic/tare/internal/render"
	"github.com/2listic/tare/internal/report"
	"github.com/2listic/tare/internal/tui"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// options are the parsed command-line flags.
type options struct {
	promURL, namespaces, kubeconfig, kubectx string
	output, verdicts                         string
	window, timeout                          time.Duration
	top                                      int
	bySavings, all, explain, demo, redact    bool
	showVersion                              bool
	nodeWatts                                float64
	gridIntensity                            string
	grid                                     float64
	gridSource                               string

	// diff only
	repo, kustomize, prBody string
	write                   bool
}

// defineFlags registers every flag on fs. Help is printed per command from
// this one set, filtered by usageFlags.
func defineFlags(fs *flag.FlagSet) *options {
	o := &options{window: 14 * 24 * time.Hour}
	fs.StringVar(&o.promURL, "prometheus", os.Getenv("TARE_PROMETHEUS"), "Prometheus-compatible endpoint. Omit to auto-discover one in the cluster and query it through the API server proxy.")
	fs.StringVar(&o.namespaces, "namespace", "", "comma-separated namespaces (default: all, excluding cluster infrastructure)")
	fs.StringVar(&o.kubeconfig, "kubeconfig", "", "path to kubeconfig")
	fs.StringVar(&o.kubectx, "context", "", "kubeconfig context")
	fs.StringVar(&o.output, "o", "table", "output format: table (a screenful: summary, warnings, one row per finding), wide (every section, evidence row and limit, for scripts and tickets), tui (interactive), json, html (a self-contained file you can send to someone), or patch (apply-ready strategic merge patches)")
	fs.IntVar(&o.top, "top", 0, "show only the N biggest opportunities")
	fs.StringVar(&o.verdicts, "verdict", "", "show only these verdicts, comma separated: SATURATED,WASTE,IDLE,LEAKING,CONFLICT,BLOCKED,UNVERIFIED,UNSET,OK,NO-DATA")
	fs.BoolVar(&o.bySavings, "by-savings", false, "order by reclaimable resource rather than by severity")
	fs.BoolVar(&o.all, "all-namespaces", false, "include kube-* and other infrastructure namespaces")
	fs.BoolVar(&o.explain, "explain", false, "after the tables, print the sentence behind every evidence code. The tables say what was measured; this says what it means and what it does not rule out.")
	fs.BoolVar(&o.demo, "demo", false, "report on simulated workloads instead of a cluster, so the output can be seen without Kubernetes or Prometheus")
	fs.DurationVar(&o.timeout, "timeout", 10*time.Minute, "give up after this long. A cluster-wide run on a large cluster is mostly waiting for the API server and Prometheus")
	fs.BoolVar(&o.redact, "redact", false, "replace namespace, workload, container and node names with stable hashes, so a report can be sent to someone who should not see them. The hash is stable, not secret: a guessed name can be confirmed")
	fs.BoolVar(&o.showVersion, "version", false, "print the version and exit")
	// -n is what people actually type, and kubectl has trained them to.
	fs.StringVar(&o.namespaces, "n", "", "shorthand for --namespace")
	fs.BoolVar(&o.explain, "v", false, "shorthand for --explain")
	fs.StringVar(&o.repo, "repo", "", "path to a checkout of your manifests; tare edits the resources in place so the change can be reviewed as a diff")
	fs.BoolVar(&o.write, "write", false, "actually modify the files under --repo (default is to show what would change)")
	fs.StringVar(&o.kustomize, "kustomize", "", "directory to write strategic-merge patches into, for repositories managed with Kustomize")
	fs.StringVar(&o.prBody, "pr-body", "", "write the pull request body, with the evidence, to this file instead of stdout")
	fs.Float64Var(&o.nodeWatts, "node-watts", 0, "power saved per node removed, in watts, to express the node delta as energy (-o wide footprint section). Use the node's idle draw, not its loaded draw: the workload moves to another node and takes its dynamic power with it. Multiply by PUE yourself to include facility overhead")
	fs.StringVar(&o.gridIntensity, "grid-intensity", "", "grid carbon intensity where the nodes run, to express the energy as emissions: a number in gCO2e/kWh, or a country (italy, france, united-kingdom, DEU) for its latest annual average from Ember's yearly data. Needs --node-watts")
	fs.Var(durationValue{&o.window}, "window",
		"observation window: 30m, 12h, 7d, 2w. Longer is better; most clusters keep about 15 days.")
	return o
}

func run() error {
	if compArgs, ok := isCompleteInvocation(os.Args[0], os.Args[1:]); ok {
		return runComplete(os.Stdout, compArgs)
	}
	cmd := "report"
	flag.CommandLine.Usage = func() { usage(os.Stderr, cmd) }
	o := defineFlags(flag.CommandLine)
	// --help after a subcommand fires inside parseArgs, so the help shown has
	// to follow the subcommand as soon as it is seen.
	sub, topic, err := parseArgs(flag.CommandLine, os.Args[1:], func(sub string) {
		if _, ok := usageText[sub]; ok {
			cmd = sub
		}
	})
	if err != nil {
		return err
	}
	switch sub {
	case "version":
		printVersion()
		return nil
	case "help":
		// "help diff" is how people ask for one command's flags.
		if _, ok := usageText[topic]; ok {
			cmd = topic
		}
		flag.CommandLine.Usage()
		return nil
	}
	if o.showVersion {
		printVersion()
		return nil
	}
	discover.UserAgent = "kubectl-tare/" + buildVersion()

	// Checked before any cluster call, so a typo costs nothing and --demo is
	// held to the same spelling as a real run.
	if err := report.CheckVerdicts(o.verdicts); err != nil {
		return err
	}
	set := map[string]bool{}
	flag.CommandLine.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if err := checkCombination(sub, o.output, o.demo, o.redact, set); err != nil {
		return err
	}
	if err := checkFactors(sub, o.nodeWatts, set); err != nil {
		return err
	}
	if o.gridIntensity != "" {
		if o.grid, o.gridSource, err = report.ParseGrid(o.gridIntensity); err != nil {
			return err
		}
	}
	// Also before any cluster call: a mistyped --repo used to be found only
	// after the whole run, minutes of Prometheus queries later.
	if sub == "diff" {
		if err := checkDiffTarget(o); err != nil {
			return err
		}
	}

	// Demo runs before any cluster call: the whole point is that it works on a
	// laptop with no kubeconfig.
	if o.demo {
		rep := report.Demo(o.window, render.HumanDuration(o.window))
		o.applyView(rep)
		return emit(rep, o.output, o.explain)
	}
	return runCluster(sub, o, splitNamespaces(o.namespaces))
}

// splitNamespaces reads -n. Entries are trimmed because "-n a, b" is what a
// person types and " b" is not a namespace: without this the space survives
// into every query and the run reports nothing for it.
func splitNamespaces(flagValue string) []string {
	var out []string
	for _, ns := range strings.Split(flagValue, ",") {
		if ns = strings.TrimSpace(ns); ns != "" {
			out = append(out, ns)
		}
	}
	return out
}

// applyView cuts the report down to what the reader asked to see. It runs
// after every whole-cluster computation, never before: packing a filtered
// cluster would remove pods from nodes they are still occupying and
// manufacture free capacity out of a --top flag.
func (o *options) applyView(rep *report.Report) {
	rep.Factors = report.Factors{NodeWatts: o.nodeWatts, GridGCO2PerKWh: o.grid, GridSource: o.gridSource}
	rep.VerdictFiltered, rep.BySavings = o.verdicts != "", o.bySavings
	rep.Findings = report.FilterVerdicts(rep.Findings, o.verdicts)
	if o.bySavings {
		report.SortBySavings(rep.Findings)
	}
	if o.top > 0 && len(rep.Findings) > o.top {
		rep.Findings = rep.Findings[:o.top]
	}
	if o.redact {
		report.Redact(rep)
	}
}

// runCluster is a report, check or diff against a live cluster.
func runCluster(sub string, o *options, namespaces []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), o.timeout)
	defer cancel()
	fail := func(err error) error { return deadlineHint(ctx, err, o.timeout) }

	kc, err := discover.NewClient(o.kubeconfig, o.kubectx)
	if err != nil {
		return err
	}
	if missing := kc.MissingNamespaces(ctx, namespaces); len(missing) > 0 {
		return fmt.Errorf("no such namespace: %s", strings.Join(missing, ", "))
	}
	if sub == "check" {
		return runCheck(ctx, os.Stdout, kc, namespaces, o.promURL, o.window, o.all)
	}

	progressf("reading pods from %s", scopeLabel(namespaces, o.redact))
	containers, err := kc.Containers(ctx, namespaces)
	if err != nil {
		return fail(err)
	}
	if !o.all {
		containers = slices.DeleteFunc(containers, func(c discover.Container) bool {
			return discover.SkipNamespace(c.Namespace)
		})
	}
	progressf("%d containers", len(containers))

	src, err := buildSource(ctx, kc, o.promURL, o.redact)
	if err != nil {
		return fail(err)
	}
	window, windowNote := resolveWindow(src, namespaces, o.window)

	srcLabel := src.Describe()
	if o.redact {
		srcLabel = report.RedactSource(srcLabel)
	}
	progressf("querying %s over %s", srcLabel, windowNote)
	obs, err := src.Observe(namespaces, window)
	if err != nil {
		return fail(err)
	}
	observeGPUs(src, containers, obs, namespaces, window)
	coverage, covCounts := coverageNote(containers, obs)

	constraints := kc.LoadConstraints(ctx, namespaces)
	// Listing nodes is a cluster-scoped read a namespace-scoped user may not
	// have. Its absence costs the per-node findings and the node-count delta,
	// and is reported rather than silently producing a report that looks
	// complete.
	inv := kc.Nodes(ctx)
	cluster := analyze.Cluster{Constraints: constraints, Nodes: inv, NodeUtil: observeNodes(src, inv, window)}
	findings := analyze.RunWithCluster(containers, obs, cluster, window, analyze.DefaultThresholds())

	rep := &report.Report{
		Findings: findings, Source: src.Describe(), Window: windowNote, Coverage: coverage,
		WindowUsed:  render.HumanDuration(window),
		KubeContext: kc.Context, Kubeconfig: o.kubeconfig,
	}
	// The node-count delta is computed over the WHOLE cluster, before the
	// display filters cut the finding list down.
	rep.Nodes, rep.Commitment, rep.DrainBlockers = replayNodes(ctx, kc, inv, findings, namespaces)
	// After every cluster read, so a refusal anywhere above is in the report.
	rep.Missing = signalGaps(src, kc)
	rep.CoverageCounts = covCounts
	if prom, ok := src.(*metrics.Prom); ok && o.output == "tui" {
		rep.Usage = promUsage(prom, containers, window)
	}
	o.applyView(rep)

	if sub != "diff" {
		return emit(rep, o.output, o.explain)
	}
	return runDiff(rep.Findings, o.repo, o.write, o.kustomize, o.prBody, src.Describe(), windowNote)
}

// resolveWindow asks for a window that exists. Confidence is driven by sample
// coverage against the window, so requesting two weeks from a backend holding
// three days rates every finding "low" - the tool reporting that it knows
// nothing, on the one run that decides whether anyone keeps it.
func resolveWindow(src metrics.Source, namespaces []string, want time.Duration) (window time.Duration, note string) {
	window, note = want, render.HumanDuration(want)
	pr, ok := src.(metrics.WindowProber)
	if !ok {
		return window, note
	}
	switch avail, err := pr.AvailableWindow(namespaces, want); {
	case err != nil:
		fmt.Fprintf(os.Stderr, "could not measure available history (%v); using %s as asked\n",
			err, render.HumanDuration(want))
	case avail == 0:
		note = fmt.Sprintf("%s requested, under an hour available", render.HumanDuration(want))
	case avail < want:
		note = fmt.Sprintf("%s requested, %s available", render.HumanDuration(want), render.HumanDuration(avail))
		window = avail
	}
	return window, note
}

// observeNodes reads node utilisation, keyed by node name. It is optional:
// node-exporter is ubiquitous but not universal, and a source that cannot
// answer simply is not asked. The address-to-name join happens here, because
// this is where the inventory and the metrics backend are both in scope; the
// analyzer only ever sees node names.
func observeNodes(src metrics.Source, inv discover.Inventory, window time.Duration) map[string]metrics.NodeObservation {
	out := map[string]metrics.NodeObservation{}
	no, ok := src.(metrics.NodeObserver)
	if !ok || inv.Partial {
		return out
	}
	byIP, err := no.ObserveNodes(inv.IPs(), window)
	if err != nil {
		return out
	}
	for ip, o := range byIP {
		if n, ok := inv.ByIP(ip); ok {
			out[n.Name] = o
		}
	}
	return out
}

// observeGPUs attaches device use to the observations of containers that hold
// a GPU. Optional, like observeNodes, and asked only when some container
// requests one, so a cluster without GPUs gets no GPU gaps in its report. The
// holder set comes from here because only the API server knows which pods were
// allocated a device; the metrics backend needs it to attribute series whose
// owner labels are ambiguous. A device of a container with no observation at
// all is dropped: that container is NO-DATA whatever its GPU did.
func observeGPUs(src metrics.Source, containers []discover.Container, obs map[metrics.Key]*metrics.Observation,
	namespaces []string, window time.Duration) {
	g, ok := src.(metrics.GPUObserver)
	if !ok {
		return
	}
	holders := map[metrics.Key]bool{}
	for _, c := range containers {
		if c.GPUs > 0 {
			holders[metrics.Key{Namespace: c.Namespace, Pod: c.Pod, Container: c.Name}] = true
		}
	}
	if len(holders) == 0 {
		return
	}
	byKey, err := g.ObserveGPUs(holders, namespaces, window)
	if err != nil {
		return
	}
	for k, o := range byKey {
		if ob := obs[k]; ob != nil {
			ob.GPU = &o
		}
	}
}

// replayNodes computes the node-count delta, what each node has promised, and
// the disruption budgets that would refuse a drain. A node-count saving that
// the cluster would refuse to perform is not a saving.
func replayNodes(ctx context.Context, kc *discover.Client, inv discover.Inventory, findings []analyze.Finding,
	namespaces []string) (delta packing.Result, commitment []packing.Commitment, blockers []string) {
	if inv.Partial {
		delta.SelfCheck = inv.Reason
		return delta, nil, nil
	}
	pdbs := kc.PDBs(ctx, namespaces)
	for _, p := range pdbs {
		if p.Blocking {
			blockers = append(blockers, p.Why)
		}
	}
	pods, err := kc.Pods(ctx, namespaces, pdbs)
	if err != nil {
		delta.SelfCheck = fmt.Sprintf("cannot list pods (%v)", err)
		return delta, nil, blockers
	}
	return packing.Replay(inv, pods, report.Sizer(findings)), packing.Commitments(inv, pods), blockers
}

// signalGaps is every signal the run could not fully read. A read tare was
// refused removes a finding from the report, so it belongs in the same header
// block as a missing metric, and in every output format: the alternative is a
// report that looks complete and is quietly missing the part RBAC denied.
func signalGaps(src metrics.Source, kc *discover.Client) []analyze.SignalGap {
	gaps := analyze.SummariseHealth(src.Health())
	for _, w := range kc.Warnings() {
		g := analyze.SignalGap{Signal: "rbac", Status: analyze.GapDenied, Detail: w}
		if !kc.Denied(w) {
			g.Signal, g.Status = "kube-api", analyze.GapQueryFailed
		}
		gaps = append(gaps, g)
	}
	return gaps
}

// coverageNote states how many of the containers the cluster has were actually
// found in the metrics backend.
//
// Observe has already answered this for free: a container with no series has no
// observation. It is worth a line in the header because every other total in
// the report is computed over the containers that had data, and a backend
// scraping two namespaces out of forty passes every signal check there is.
//
// Pods and rows are different units: one NO-DATA row can stand for several
// uncovered replicas, and an uncovered pod whose siblings were scraped is no
// row at all. So the split is counted here, per pod and grouped the way
// analyze groups rows, before any display filter can cut the rows down.
func coverageNote(containers []discover.Container, obs map[metrics.Key]*metrics.Observation) (string, report.Coverage) {
	type row struct{ ns, workload, container string }
	missingIn, covered := map[row]int{}, map[row]bool{}
	var cov report.Coverage
	for _, c := range containers {
		// An entry is not the same as data. kube-state-metrics publishes a
		// series for every container in the cluster whether or not anything
		// ever scraped its cgroup, so a container whose kubelet never reported
		// still arrives here with an observation attached and nothing in it.
		// Samples counts memory buckets, which is what every recommendation is
		// actually built from.
		k := row{c.Namespace, c.Workload, c.Name}
		o := obs[metrics.Key{Namespace: c.Namespace, Pod: c.Pod, Container: c.Name}]
		if o == nil || o.Samples == 0 {
			cov.Missing++
			missingIn[k]++
		} else {
			covered[k] = true
		}
	}
	if cov.Missing == 0 {
		return "", cov
	}
	cov.Total = len(containers)
	for k, n := range missingIn {
		if !covered[k] {
			cov.Dark += n
			cov.DarkRows++
		}
	}
	return fmt.Sprintf("metrics cover %d of %d pod containers; the other %d have no series in this backend (%s)",
		cov.Total-cov.Missing, cov.Total, cov.Missing, cov.Split()), cov
}

// commands are the subcommands; "report" is the default.
var commands = map[string]bool{"report": true, "diff": true, "check": true, "version": true, "help": true}

// parseArgs parses flags wherever they appear, before or after the
// subcommand, the way kubectl does.
//
// Go's flag package stops at the first non-flag argument. Splitting off only a
// leading subcommand fixed "report -n prod" but not "-n prod check": that
// parsed -n, stopped at "check", and ran a full report that exited 0, so a CI
// gate written that way could never fail. Any argument left over is now an
// error rather than ignored.
func parseArgs(fs *flag.FlagSet, args []string, seen func(sub string)) (sub, topic string, err error) {
	for {
		if err := fs.Parse(args); err != nil {
			return "", "", err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			break
		}
		word := rest[0]
		args = rest[1:]
		switch {
		case sub == "" && commands[word]:
			sub = word
			if seen != nil {
				seen(sub)
			}
		case sub == "":
			return "", "", fmt.Errorf("unknown command %q; the commands are \"report\" (the default), \"check\", \"diff\" and \"version\"", word)
		case sub == "help" && topic == "":
			topic = word
		default:
			return "", "", fmt.Errorf("unexpected argument %q: %s takes no positional arguments (namespaces go in -n a,b)", word, sub)
		}
	}
	if sub == "" {
		sub = "report"
	}
	return sub, topic, nil
}

// checkCombination rejects flags that would otherwise be silently ignored.
// It runs before any cluster call, so a mistake costs nothing.
func checkCombination(sub, output string, demo, redact bool, set map[string]bool) error {
	if !slices.Contains(render.Formats, output) {
		return render.UnknownFormat(output)
	}
	if demo && sub != "report" {
		return fmt.Errorf("--demo only applies to report: there is no cluster to %s", sub)
	}
	if sub == "check" && set["o"] {
		return fmt.Errorf("check prints its own table; -o does not apply")
	}
	if sub != "diff" {
		for _, f := range diffOnly {
			if set[f] {
				return fmt.Errorf("--%s only applies to diff", f)
			}
		}
		return nil
	}
	if set["o"] {
		return fmt.Errorf("diff writes manifests, not a report; drop -o, or run report -o %s", output)
	}
	if redact {
		return fmt.Errorf("--redact cannot be used with diff: the manifests carry the real names")
	}
	return nil
}

// checkFactors rejects footprint factors that would print a figure meaning
// nothing. A grid intensity with no wattage has nothing to multiply, and
// accepting it silently would print no footprint to a reader who asked for
// one.
func checkFactors(sub string, watts float64, set map[string]bool) error {
	if sub != "report" && (set["node-watts"] || set["grid-intensity"]) {
		return fmt.Errorf("--node-watts and --grid-intensity only apply to report")
	}
	if watts < 0 {
		return fmt.Errorf("--node-watts cannot be negative")
	}
	if set["node-watts"] && watts == 0 {
		return fmt.Errorf("--node-watts 0 would report every freed node as saving nothing; omit the flag instead")
	}
	if set["grid-intensity"] && !set["node-watts"] {
		return fmt.Errorf("--grid-intensity needs --node-watts: emissions are energy times intensity, and tare does not measure node power")
	}
	return nil
}

// emit renders one report in the requested format. Every format goes through
// here so that adding one cannot quietly skip the parts of the report that say
// what the run could not see.
func emit(r *report.Report, format string, verbose bool) error {
	if format == "tui" {
		return tui.Run(r)
	}
	return render.Render(os.Stdout, os.Stderr, r, format, verbose)
}

// buildSource resolves where to read history from. An explicit URL always
// wins; otherwise the cluster is asked where its own Prometheus is, and the
// query goes through the API server proxy so no port forward is needed.
func buildSource(ctx context.Context, kc *discover.Client, promURL string, redact bool) (metrics.Source, error) {
	if promURL != "" {
		prom := metrics.NewProm(promURL)
		if redact {
			prom.ScrubErrors(report.Scrubber())
		}
		return prom, nil
	}

	prom, c, others, err := discoverProm(ctx, kc)
	if err != nil {
		return nil, fmt.Errorf("looking for a metrics service: %w", err)
	}
	if prom == nil {
		//lint:ignore ST1005 the whole message is what the user reads, so it ends like one
		return nil, fmt.Errorf(
			"no Prometheus-compatible service found in this cluster.\n" +
				"  tare reads history from Prometheus, Thanos, Mimir or VictoriaMetrics.\n" +
				"  Point it at one explicitly:\n" +
				"    kubectl tare report --prometheus http://localhost:9090\n" +
				"  or set TARE_PROMETHEUS in the environment.")
	}

	if redact {
		prom.ScrubErrors(report.Scrubber(c.Namespace, c.Service))
	}
	fmt.Fprintf(os.Stderr, "using %s\n", formatCandidate(c, redact))
	if others > 0 {
		fmt.Fprintf(os.Stderr, "  (%d other candidates found; pass --prometheus to choose a different one)\n", others)
	}
	return prom, nil
}

func formatCandidate(c discover.PromCandidate, redact bool) string {
	if !redact {
		return c.String()
	}
	c.Namespace = report.HashName(c.Namespace)
	c.Service = report.HashName(c.Service)
	return c.String()
}

func scopeLabel(namespaces []string, redact bool) string {
	if !redact || len(namespaces) == 0 {
		return scopeName(namespaces)
	}
	hashed := make([]string, len(namespaces))
	for i, ns := range namespaces {
		hashed[i] = report.HashName(ns)
	}
	return scopeName(hashed)
}

// discoverProm asks the cluster where its own Prometheus is and returns a
// source that queries it through the API server proxy, so no port forward is
// needed. A nil source with a nil error means none was found; each caller
// words that for its own command.
func discoverProm(ctx context.Context, kc *discover.Client) (prom *metrics.Prom, chosen discover.PromCandidate, others int, err error) {
	candidates, err := kc.FindPrometheus(ctx)
	if err != nil || len(candidates) == 0 {
		return nil, chosen, 0, err
	}
	chosen = candidates[0]
	return metrics.New(&metrics.ProxyTransport{
		Clientset: kc.Clientset(),
		Namespace: chosen.Namespace, Service: chosen.Service, Port: chosen.Port, Scheme: chosen.Scheme,
	}), chosen, len(candidates) - 1, nil
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
		return prom.Usage(ctx, f.Namespace, pods[f.Namespace+"/"+f.Workload+"/"+f.Container], f.Container, window, report.PlotBuckets)
	}
}
