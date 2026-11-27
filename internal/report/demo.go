package report

import (
	"fmt"
	"strings"
	"time"

	"github.com/2listic/tare/internal/analyze"
	"github.com/2listic/tare/internal/metrics"
	"github.com/2listic/tare/internal/simulate"
)

// Demo builds a report from the simulator instead of a cluster.
//
// Until this existed, seeing tare's output at all required Kubernetes,
// Prometheus, and ideally two weeks of retention. That is a long way to walk
// before finding out whether the tool is worth installing, and it made the
// README's pasted output the only thing a new reader could actually look at.
//
// The workloads are the same catalog the test suite sizes against, so the
// verdicts here are produced by the real analyzer over real (simulated)
// series - not canned. A demo that hardcoded its own findings would be a
// screenshot, and the first thing to rot.
func Demo(window time.Duration, label string) *Report {
	const seed = 21
	origin := time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC) // a Monday

	c := simulate.NewCluster("demo", origin, seed, append(simulate.Catalog(), simulate.VMCatalog()...)...)
	// Deliberately generous, which is how a container arrives at this tool:
	// over-provisioned, so its true demand is visible.
	for _, d := range simulate.Catalog() {
		c.SetAllocation(d.Name, simulate.Allocation{
			CPURequest: 2, CPULimit: 4,
			MemRequest: 4 << 30, MemLimit: 8 << 30,
		})
	}
	// A VM's pod is sized by KubeVirt from its spec, not by its author, so
	// the VMs get what KubeVirt would derive rather than the generous
	// allocation above.
	for _, d := range simulate.VMCatalog() {
		c.SetAllocation(d.Name, simulate.VMAllocation(d))
	}
	c.Run(window)

	obs, err := c.Observe(nil, window)
	if err != nil {
		// The simulator has no I/O and no cluster to fail against; an error
		// here is a programming fault, not an environment one.
		panic(fmt.Sprintf("demo: simulator failed to observe: %v", err))
	}
	vms, _ := c.ObserveVMs(nil, window)

	return &Report{
		Usage: func(f analyze.Finding) (metrics.Series, metrics.Series, error) {
			name := f.Container
			if f.IsVM() {
				// Every VM's container is called compute; its environment is
				// named after the VM.
				_, name, _ = strings.Cut(f.Workload, "/")
			}
			cpu, mem, ok := c.Usage(name, window, PlotBuckets)
			if !ok {
				return cpu, mem, fmt.Errorf("the simulator has no container %q", f.Container)
			}
			return cpu, mem, nil
		},
		Findings: analyze.RunWithCluster(c.Containers(), obs, analyze.Cluster{VMs: vms}, window, analyze.DefaultThresholds()),
		Source:   "simulator (demo data, no cluster was read)",
		Window:   label,
		// Always the window asked for: the simulator has all of its history.
		WindowUsed: label,
		Demo:       true,
		// The node-count delta is the figure that corresponds to a bill, and
		// the simulator has no node model at all. Reporting a zero here would
		// be indistinguishable from a cluster that cannot free a node, which is
		// exactly the confusion the three separate node counts exist to avoid.
		NodeNote: "node packing is not modelled in demo mode: there are no real nodes to pack, " +
			"and a savings figure invented here would be the one number in this report that maps to money",
	}
}
