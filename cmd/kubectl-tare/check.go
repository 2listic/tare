package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/2listic/tare/internal/discover"
	"github.com/2listic/tare/internal/metrics"
	"github.com/2listic/tare/internal/render"
)

// runCheck answers, before any report is produced, what this cluster can and
// cannot tell tare.
//
// A report already says what it could not see, but only after doing all the
// work, and only about the signals it got as far as querying. The failures
// that stop a first run are further down: a token that cannot list pods, an
// API server proxy that is closed, a Prometheus that scrapes applications and
// not cAdvisor. Each of those surfaces as a Kubernetes error in the middle of
// something else, and none of them says what to do next.
//
// Every line here names what it costs, because "missing" is not actionable and
// "no OOMKill evidence, so a memory recommendation cannot be corroborated" is.
func runCheck(ctx context.Context, out io.Writer, kc *discover.Client, namespaces []string, promURL string, window time.Duration, all bool) error {
	fmt.Fprintf(out, "kubectl-tare %s\n\n", buildVersion())

	var fatal []string

	fmt.Fprintln(out, "cluster")
	ver, err := kc.ServerVersion()
	if err != nil {
		mark(out, false, "API server", err.Error())
		return fmt.Errorf("cannot reach the cluster, so nothing below could be checked")
	}
	mark(out, true, "API server", ver)

	scope := ""
	if len(namespaces) > 0 {
		scope = namespaces[0]
	}
	pods, err := kc.CountPods(ctx, namespaces)
	switch {
	case err != nil:
		mark(out, false, "pods visible", err.Error())
		fatal = append(fatal, "tare cannot list pods, which is the whole inventory")
	case pods == 0:
		mark(out, false, "pods visible", "none in "+scopeName(namespaces))
		fatal = append(fatal, "no pods are visible in "+scopeName(namespaces))
	default:
		mark(out, true, "pods visible", fmt.Sprintf("%d in %s", pods, scopeName(namespaces)))
	}

	fmt.Fprintf(out, "\npermissions (as you, %s)\n", scopeName(namespaces))
	for _, p := range kc.CheckPermissions(ctx, scope) {
		switch {
		case p.Err != nil:
			mark(out, false, p.String(), "could not be checked: "+p.Err.Error())
		case p.Allowed:
			mark(out, true, p.String(), "")
		default:
			mark(out, false, p.String(), p.Buys)
			if p.Required {
				fatal = append(fatal, "cannot "+p.String())
			}
		}
	}

	fmt.Fprintln(out, "\nmetrics backend")
	src, describe, err := checkSource(ctx, kc, promURL)
	if err != nil {
		mark(out, false, "endpoint", err.Error())
		fatal = append(fatal, "no reachable Prometheus-compatible endpoint")
		return summarise(out, fatal, nil)
	}
	mark(out, true, "endpoint", describe)

	if err := src.Ping(); err != nil {
		mark(out, false, "query API", err.Error())
		fatal = append(fatal, "the endpoint was found but does not answer queries")
		return summarise(out, fatal, nil)
	}
	mark(out, true, "query API", "answers /api/v1/query")

	avail, err := src.AvailableWindow(namespaces, window)
	switch {
	case err != nil:
		mark(out, false, "history", err.Error())
	case avail == 0:
		mark(out, false, "history", fmt.Sprintf(
			"less than an hour. %s was asked for; every finding would be rated low confidence",
			render.HumanDuration(window)))
	case avail < window:
		mark(out, true, "history", fmt.Sprintf("%s (%s asked for, so the window will be shortened)",
			render.HumanDuration(avail), render.HumanDuration(window)))
	default:
		mark(out, true, "history", render.HumanDuration(avail))
	}

	fmt.Fprintln(out, "\nseries")
	var degraded []string
	psiOK := false
	for _, c := range src.Preflight(namespaces, window) {
		if c.Metric == "container_pressure_cpu_waiting_seconds_total" {
			psiOK = c.OK()
		}
		switch {
		case c.Err != nil:
			mark(out, false, c.Metric, c.Err.Error())
		case c.Series == 0:
			mark(out, false, c.Metric, "absent: "+c.Buys)
			if c.Required {
				fatal = append(fatal, c.Metric+" is not exported by this backend")
			} else {
				degraded = append(degraded, c.Metric)
			}
		case c.Note != "":
			mark(out, false, c.Metric, c.Note)
			degraded = append(degraded, c.Metric)
		default:
			mark(out, true, c.Metric, fmt.Sprintf("%d series", c.Series))
		}
	}

	fmt.Fprintln(out, "\ntare-agent")
	if !checkAgent(out, src.Agent(namespaces, window), psiOK) {
		degraded = append(degraded, "tare-agent")
	}

	containers, listErr := kc.Containers(ctx, namespaces)
	if listErr == nil && !all {
		containers = slices.DeleteFunc(containers, func(c discover.Container) bool { return discover.SkipNamespace(c.Namespace) })
	}
	if listErr == nil {
		degraded = append(degraded, checkGPUs(out, src, containers, namespaces, window)...)
	}

	fmt.Fprintln(out)
	if note, ok := checkCoverage(containers, listErr, src, namespaces, window); !ok {
		degraded = append(degraded, "metrics coverage")
		mark(out, false, "coverage", note)
	} else {
		mark(out, true, "coverage", note)
	}

	return summarise(out, fatal, degraded)
}

// checkCoverage compares the containers the cluster has with the containers the
// backend holds series for. It costs one pod list and one instant query, which
// is worth paying to find out that Prometheus is scraping two namespaces out of
// forty before a report says nothing about the other thirty-eight.
func checkCoverage(containers []discover.Container, listErr error, src *metrics.Prom, namespaces []string, window time.Duration) (string, bool) {
	if listErr != nil {
		return "could not list containers: " + listErr.Error(), false
	}
	covered, err := src.CoveredKeys(namespaces, window)
	if err != nil {
		return "could not be measured: " + err.Error(), false
	}

	missingBy := map[string]int{}
	var total, have int
	for _, c := range containers {
		total++
		if covered[metrics.Key{Namespace: c.Namespace, Pod: c.Pod, Container: c.Name}] {
			have++
			continue
		}
		missingBy[c.Namespace]++
	}
	switch {
	case total == 0:
		return "no containers in scope to cover", false
	case have == total:
		return fmt.Sprintf("all %d containers in scope have metrics", total), true
	}

	worst := make([]string, 0, len(missingBy))
	for ns, n := range missingBy {
		worst = append(worst, fmt.Sprintf("%s (%d)", ns, n))
	}
	sort.Slice(worst, func(i, j int) bool { return worst[i] < worst[j] })
	if len(worst) > 4 {
		worst = append(worst[:4], "...")
	}
	return fmt.Sprintf("only %d of %d containers in scope have metrics. Missing in %s. "+
		"Those arrive as NO-DATA rows rather than as recommendations. Usual causes: Prometheus "+
		"scrapes a subset of namespaces, a kubelet has stopped reporting, or the pods are newer "+
		"than the first scrape", have, total, strings.Join(worst, ", ")), false
}

// checkGPUs describes dcgm-exporter, the only source of what a GPU did, and
// returns the gaps it leaves. Nothing at all when no container in scope holds
// a GPU, the same rule the report follows.
//
// It runs the report's own attribution rather than counting series: an
// exporter outside Kubernetes mode exports every device and names no pod,
// which a count calls healthy and the report calls GPU_NOT_MEASURED on every
// GPU container.
func checkGPUs(out io.Writer, src *metrics.Prom, containers []discover.Container, namespaces []string, window time.Duration) []string {
	holders := map[metrics.Key]bool{}
	for _, c := range containers {
		if c.GPUs > 0 {
			holders[metrics.Key{Namespace: c.Namespace, Pod: c.Pod, Container: c.Name}] = true
		}
	}
	if len(holders) == 0 {
		return nil
	}
	fmt.Fprintf(out, "\ngpu (%d containers hold one)\n", len(holders))
	byKey, err := src.ObserveGPUs(holders, namespaces, window)
	health := map[metrics.Signal]metrics.SignalHealth{}
	for _, h := range src.Health() {
		health[h.Signal] = h
	}

	busy := health[metrics.SigGPUBusy]
	switch {
	case err != nil || busy.Err != nil:
		mark(out, false, "DCGM_FI_DEV_GPU_UTIL", "could not be checked: "+errors.Join(err, busy.Err).Error())
		return []string{"dcgm-exporter"}
	case busy.Series == 0:
		mark(out, false, "DCGM_FI_DEV_GPU_UTIL", "absent: no GPU container's use can be measured, so an idle "+
			"GPU and a busy one look the same and every one is reported GPU_NOT_MEASURED. Install NVIDIA's "+
			"dcgm-exporter (part of the GPU Operator) with its ServiceMonitor")
		return []string{"dcgm-exporter"}
	case busy.Suspect != "":
		mark(out, false, "DCGM_FI_DEV_GPU_UTIL", busy.Suspect)
		return []string{"dcgm-exporter"}
	}
	mark(out, true, "DCGM_FI_DEV_GPU_UTIL", fmt.Sprintf("%d device series", busy.Series))

	var gaps []string
	if len(byKey) < len(holders) {
		mark(out, false, "attribution", fmt.Sprintf("%d of %d GPU containers have device metrics. The rest are "+
			"reported GPU_NOT_MEASURED: usually pods newer than the last scrape, or nodes the exporter does not run on",
			len(byKey), len(holders)))
		gaps = append(gaps, "gpu attribution")
	} else {
		mark(out, true, "attribution", fmt.Sprintf("all %d GPU containers have device metrics", len(holders)))
	}

	if health[metrics.SigGPUSMActive].Series == 0 {
		mark(out, false, "DCGM_FI_PROF_SM_ACTIVE", "absent: busy-time can prove a GPU idle or underused but never "+
			"used, so a busy GPU stays GPU_BUSY_UNVERIFIED. It is a DCGM profiling metric; GeForce cards never export it")
		gaps = append(gaps, "gpu SM activity")
	} else {
		mark(out, true, "DCGM_FI_PROF_SM_ACTIVE", fmt.Sprintf("%d device series", health[metrics.SigGPUSMActive].Series))
	}
	return gaps
}

// checkAgent describes tare-agent, the source of CPU wait where PSI is
// missing, and reports whether it is in the state the cluster needs.
//
// tare never installs it: it is a privileged DaemonSet, and the people who run
// tare usually cannot deploy one. So this says what is lost without it and
// who has to act, and stays quiet where PSI makes it unnecessary.
func checkAgent(out io.Writer, st metrics.AgentStatus, psiOK bool) bool {
	switch {
	case st.Err != nil:
		mark(out, false, "agent", "could not be checked: "+st.Err.Error())
		return false
	case st.Nodes == 0 && psiOK:
		mark(out, true, "agent", "not installed, and not needed: PSI is present")
		return true
	case st.Nodes == 0:
		mark(out, false, "agent", "not installed, and PSI is missing or not believable, so CPU starvation is "+
			"visible only through CFS throttling, which needs a CPU limit; shrink verdicts elsewhere are UNVERIFIED. "+
			"A cluster admin can install it: kubectl apply -f deploy/agent/agent.yaml (see agent/README.md)")
		return false
	}
	ok := true
	mark(out, true, "agent", fmt.Sprintf("reporting from %d node(s)", st.Nodes))
	if st.Matched == 0 {
		mark(out, false, "join to containers", "no agent series matches a container in scope. The kubelet's "+
			"cAdvisor series must keep their `id` label (a metric relabelling may drop it), and kubelet and "+
			"container runtime must use the same cgroup driver")
		ok = false
	} else {
		mark(out, true, "join to containers", fmt.Sprintf("%d container(s) in scope", st.Matched))
	}
	if st.Outside > 0 {
		mark(out, false, "cgroup driver", fmt.Sprintf("%d container(s) found outside the kubelet's kubepods "+
			"hierarchy: kubelet and runtime use different cgroup drivers, and their join may fail", st.Outside))
		ok = false
	}
	if len(st.Failing) == 0 {
		mark(out, true, "self-checks", "passing on every node")
		return ok
	}
	for _, n := range st.Failing {
		mark(out, false, "self-checks "+n.Instance, strings.Join(n.Problems, "; ")+
			". CPU wait from this node is not used for the window")
	}
	return false
}

// checkSource resolves the endpoint the same way a report does, so that check
// and report cannot disagree about where the data would come from.
func checkSource(ctx context.Context, kc *discover.Client, promURL string) (*metrics.Prom, string, error) {
	if promURL != "" {
		p := metrics.NewProm(promURL)
		p.Ctx = ctx
		return p, promURL + " (from --prometheus)", nil
	}
	p, c, others, err := discoverProm(ctx, kc)
	if err != nil {
		return nil, "", err
	}
	if p == nil {
		return nil, "", fmt.Errorf("no Prometheus-compatible service found; pass --prometheus http://... " +
			"or set TARE_PROMETHEUS")
	}
	p.Ctx = ctx
	describe := c.String()
	if others > 0 {
		describe += fmt.Sprintf(", %d other candidate(s); --prometheus picks a different one", others)
	}
	return p, describe, nil
}

func mark(w io.Writer, ok bool, label, note string) {
	sym := "✗"
	if ok {
		sym = "✓"
	}
	if note == "" {
		fmt.Fprintf(w, "  %s %s\n", sym, label)
		return
	}
	// The note is the part worth reading, and it is often a sentence. Wrapping
	// it under the label keeps the column of marks scannable.
	lines := render.Wrap(note, 74)
	if len(lines) == 0 {
		lines = []string{""}
	}
	fmt.Fprintf(w, "  %s %-46s %s\n", sym, label, lines[0])
	for _, l := range lines[1:] {
		fmt.Fprintf(w, "  %s %-46s %s\n", " ", "", l)
	}
}

func summarise(w io.Writer, fatal, degraded []string) error {
	fmt.Fprintln(w)
	if len(fatal) > 0 {
		for _, f := range fatal {
			fmt.Fprintln(w, "  -", f)
		}
		return fmt.Errorf("this cluster cannot produce a report yet: %s", strings.Join(fatal, "; "))
	}
	if len(degraded) > 0 {
		fmt.Fprintf(w, "ready, with gaps: %s\n", strings.Join(degraded, ", "))
		fmt.Fprintln(w, "each is explained above. Findings that depended on them are downgraded and")
		fmt.Fprintln(w, "the report says so, rather than reading a missing measurement as a zero.")
		fmt.Fprintln(w, "Run: kubectl tare report")
		return nil
	}
	fmt.Fprintln(w, "ready: every signal tare uses is present, for every container in scope.")
	fmt.Fprintln(w, "Run: kubectl tare report")
	return nil
}
