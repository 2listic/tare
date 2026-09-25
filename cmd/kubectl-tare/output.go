package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/mfranzon/tare/internal/analyze"
)

// savings is what a reader actually wants from a cluster-wide run: not three
// hundred rows, but one number and the handful of workloads that make it up.
type savings struct {
	cpuMillicores int64
	memBytes      int64
	workloads     int

	// A container that needs MORE than it has is not a rounding error in the
	// total, it is the finding that matters most. Summing only the positive
	// side produced a headline number that was optimistic by exactly the amount
	// a reader most needed to see.
	addCPUMillicores int64
	addMemBytes      int64
	addWorkloads     int
}

func totalSavings(fs []analyze.Finding) savings {
	var s savings
	for _, f := range fs {
		c, m := reclaimable(f)
		if c > 0 || m > 0 {
			s.workloads++
		}
		if c < 0 || m < 0 {
			s.addWorkloads++
		}
		switch {
		case c > 0:
			s.cpuMillicores += c
		case c < 0:
			s.addCPUMillicores -= c
		}
		switch {
		case m > 0:
			s.memBytes += m
		case m < 0:
			s.addMemBytes -= m
		}
	}
	return s
}

// reclaimable is per-workload, multiplied by replica count: a 100m saving on a
// twenty-replica deployment is two cores, and reporting it as 100m
// understates it by the factor that matters most.
func reclaimable(f analyze.Finding) (cpu, mem int64) {
	pods := int64(f.Pods)
	if pods < 1 {
		pods = 1
	}
	if f.RecCPU > 0 && f.CPURequest > 0 {
		cpu = (f.CPURequest - f.RecCPU) * pods
	}
	if f.RecMem > 0 && f.MemRequest > 0 {
		mem = (f.MemRequest - f.RecMem) * pods
	}
	return cpu, mem
}

func cores(m int64) string {
	if m >= 1000 {
		return fmt.Sprintf("%.1f cores", float64(m)/1000)
	}
	return fmt.Sprintf("%dm", m)
}

// sortBySavings puts the biggest wins first inside each severity band. On a
// cluster with hundreds of containers, severity ordering alone still leaves an
// unreadable wall of rows.
func sortBySavings(fs []analyze.Finding) {
	sort.SliceStable(fs, func(i, j int) bool {
		ci, mi := reclaimable(fs[i])
		cj, mj := reclaimable(fs[j])
		// Weigh a core against a gigabyte so one ranking covers both.
		return float64(ci)/1000+float64(mi)/float64(1<<30) >
			float64(cj)/1000+float64(mj)/float64(1<<30)
	})
}

// checkVerdicts rejects a --verdict value that names no verdict.
//
// Without it a misspelling matches nothing, so the report prints a bare header
// and exits 0. "SATURATD" and a genuinely healthy cluster produce identical
// output, and the operator believes the second one. The accepted set is
// analyze.ReportOrder rather than a list written here, so a verdict added
// later cannot become unselectable.
func checkVerdicts(want string) error {
	if want == "" {
		return nil
	}
	known := map[string]bool{}
	for _, v := range analyze.ReportOrder() {
		known[string(v)] = true
	}
	var unknown []string
	for _, v := range strings.Split(want, ",") {
		if name := strings.ToUpper(strings.TrimSpace(v)); name != "" && !known[name] {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	valid := make([]string, 0, len(analyze.ReportOrder()))
	for _, v := range analyze.ReportOrder() {
		valid = append(valid, string(v))
	}
	return fmt.Errorf("no such verdict: %s. Valid verdicts are %s",
		strings.Join(unknown, ", "), strings.Join(valid, ", "))
}

func filterVerdicts(fs []analyze.Finding, want string) []analyze.Finding {
	if want == "" {
		return fs
	}
	keep := map[string]bool{}
	for _, v := range strings.Split(want, ",") {
		keep[strings.ToUpper(strings.TrimSpace(v))] = true
	}
	out := fs[:0]
	for _, f := range fs {
		if keep[string(f.Verdict)] {
			out = append(out, f)
		}
	}
	return out
}

// printPatches emits runnable `kubectl patch` commands, one per workload.
//
// It deliberately does not emit a partial manifest for `kubectl apply`: apply
// performs a three-way merge and treats the document as the complete desired
// state, so a fragment containing only resources nulls out the selector and the
// image and is rejected. `kubectl patch --type=strategic` touches only the
// fields named, which is what a right-sizer should ever do.
func printPatches(fs []analyze.Finding) {
	type wl struct{ ns, kind, name string }
	byWorkload := map[wl][]analyze.Finding{}
	var order []wl

	var skipped []string
	for _, f := range fs {
		if f.RecCPU == 0 && f.RecMem == 0 {
			continue // nothing to apply, and a partial patch is worse than none
		}
		parts := strings.SplitN(f.Workload, "/", 2)
		if len(parts) != 2 {
			continue
		}
		if !patchableVerdict(f.Verdict) {
			skipped = append(skipped, fmt.Sprintf("%s/%s [%s]: verdict %s, so tare does not stand behind a number for it",
				f.Namespace, f.Workload, f.Container, f.Verdict))
			continue
		}
		if !patchableKind(parts[0]) {
			skipped = append(skipped, fmt.Sprintf("%s/%s [%s]: a %s pod template is immutable; kubectl rejects the patch with \"field is immutable\"",
				f.Namespace, f.Workload, f.Container, parts[0]))
			continue
		}
		k := wl{f.Namespace, parts[0], parts[1]}
		if _, seen := byWorkload[k]; !seen {
			order = append(order, k)
		}
		byWorkload[k] = append(byWorkload[k], f)
	}

	for _, s := range skipped {
		fmt.Fprintf(os.Stderr, "skipped %s\n", s)
	}

	if len(order) == 0 {
		fmt.Fprintln(os.Stderr, "no applicable recommendations: nothing to patch")
		return
	}

	fmt.Println("#!/bin/sh")
	fmt.Println("# Generated by tare. Review before running; add --dry-run=server to check first.")
	for _, s := range skipped {
		fmt.Printf("# skipped %s\n", s)
	}

	for _, k := range order {
		fmt.Printf("\n# %s/%s/%s\n", k.ns, k.kind, k.name)
		for _, f := range byWorkload[k] {
			for _, r := range f.Reasons {
				fmt.Printf("#   %s %s\n", r.Code, wrapComment(r.Text))
			}
		}

		var containers []map[string]any
		for _, f := range byWorkload[k] {
			res := map[string]any{
				"requests": map[string]string{
					"cpu": quantityCPU(f.RecCPU), "memory": quantityMem(f.RecMem),
				},
			}
			if f.RecCPULimit > 0 || f.RecMemLimit > 0 {
				res["limits"] = map[string]string{
					"cpu": quantityCPU(f.RecCPULimit), "memory": quantityMem(f.RecMemLimit),
				}
			}
			containers = append(containers, map[string]any{
				"name": f.Container, "resources": res,
			})
		}
		patch := map[string]any{
			"spec": map[string]any{
				"template": map[string]any{
					"spec": map[string]any{"containers": containers},
				},
			},
		}
		b, err := json.Marshal(patch)
		if err != nil {
			fmt.Fprintf(os.Stderr, "encoding patch for %s: %v\n", k.name, err)
			continue
		}
		fmt.Printf("kubectl -n %s patch %s %s --type=strategic -p %s\n",
			k.ns, strings.ToLower(k.kind), k.name, shellQuote(string(b)))
	}
}

// shellQuote makes a JSON document safe as a single shell argument.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// patchableKind reports whether a strategic merge patch against the workload's
// pod template is accepted at all. A Job's spec.template is immutable, so the
// generated command fails with "field is immutable" - the same class of defect
// as emitting an apply-shaped fragment, found the same way, with a server-side
// dry run against a kind nobody had tried.
func patchableKind(kind string) bool {
	switch kind {
	case "Deployment", "StatefulSet", "DaemonSet", "ReplicaSet", "ReplicationController":
		return true
	}
	return false
}

// patchableVerdict keeps the script to findings tare is willing to defend.
// Emitting apply-ready commands for a container the report just described as
// unverifiable hands the reader a number the tool disclaimed one screen earlier.
func patchableVerdict(v analyze.Verdict) bool {
	switch v {
	case analyze.Saturated, analyze.Waste, analyze.Healthy:
		return true
	}
	return false
}

// quantityMem emits whole mebibytes: a Kubernetes quantity must round-trip
// exactly, and a decimal gibibyte invites a rounding surprise.
func quantityMem(b int64) string {
	if b <= 0 {
		return "0"
	}
	mi := b >> 20
	if mi < 1 {
		mi = 1
	}
	return fmt.Sprintf("%dMi", mi)
}

func quantityCPU(m int64) string {
	if m <= 0 {
		return "0"
	}
	return fmt.Sprintf("%dm", m)
}

func wrapComment(s string) string {
	return strings.ReplaceAll(s, "\n", " ")
}
