// tare-agent measures per-container CPU wait on nodes without PSI and serves it
// to Prometheus. CPU wait is the time a container's tasks spent ready to run but
// waiting for a CPU, summed over all of its tasks, including those that already
// exited: live threads are read from /proc on every scrape, exited tasks are
// counted from the kernel's taskstats exit notifications. Proof of concept.
package main

import (
	"flag"
	"log"
	"net/http"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/procfs"
	"golang.org/x/sys/unix"
)

var version = "poc" // set at build time: -ldflags "-X main.version=..."

func main() {
	listen := flag.String("listen", ":9105", "address for /metrics")
	cgroupMount := flag.String("cgroup", "/sys/fs/cgroup", "cgroup filesystem mount")
	procMount := flag.String("proc", "/proc", "proc filesystem mount (host PID namespace)")
	flag.Parse()

	proc, err := procfs.NewFS(*procMount)
	if err != nil {
		log.Fatalf("procfs: %v", err)
	}
	r := reader{h: detectHierarchy(*cgroupMount), proc: proc}
	a := newAccount()
	log.Printf("tare-agent %s: cgroup %s at %s", version, r.h.version, r.h.root)

	go func() {
		if err := listenExits(a, r.cgroupOf); err != nil {
			// Without exit records the agent still serves the live threads; the
			// tasks that exit between scrapes are then missing, which shows as
			// low coverage (tare_cpu_run_seconds_total against CPU usage).
			log.Printf("exit listener stopped: %v", err)
		}
	}()

	reg := prometheus.NewRegistry()
	reg.MustRegister(&collector{r: r, a: a}, collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		collectors.NewGoCollector())
	http.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	http.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	log.Fatal(http.ListenAndServe(*listen, nil))
}

// collector reads every container on each scrape, like the Python exporter.
type collector struct {
	r reader
	a *account
}

var (
	descWait = prometheus.NewDesc("tare_cpu_wait_seconds_total",
		"Seconds the container's tasks spent ready to run but waiting for a CPU, live and exited tasks (complete run-queue accounting).",
		[]string{"id"}, nil)
	descRun = prometheus.NewDesc("tare_cpu_run_seconds_total",
		"Seconds of CPU the agent accounted for the container's tasks (for coverage).", []string{"id"}, nil)
	descExits  = prometheus.NewDesc("tare_agent_exits_total", "Exit records received.", nil, nil)
	descUnattr = prometheus.NewDesc("tare_agent_exits_unattributed_total",
		"Exit records that could not be assigned to a container.", nil, nil)
	descOutside = prometheus.NewDesc("tare_agent_exits_outside_containers_total",
		"Exit records of tasks in no container (host processes).", nil, nil)
	descNever = prometheus.NewDesc("tare_agent_exits_never_received_total",
		"Threads that left /proc and whose exit record never arrived.", nil, nil)
	descLayout = prometheus.NewDesc("tare_agent_exits_layout_total",
		"Exit records decoded per struct taskstats layout.", []string{"layout"}, nil)
	descUnknown = prometheus.NewDesc("tare_agent_exits_layout_unknown_total",
		"Exit records whose struct taskstats layout was not recognised.", nil, nil)
	descDrops = prometheus.NewDesc("tare_agent_netlink_drops_total",
		"Netlink receive errors (exit records lost).", nil, nil)
	descReads      = prometheus.NewDesc("tare_agent_reads_total", "Reads of all containers (one per scrape).", nil, nil)
	descContainers = prometheus.NewDesc("tare_agent_containers",
		"Containers found, by location: kubepods, or outside (cgroup driver mismatch).", []string{"location"}, nil)
	descInfo = prometheus.NewDesc("tare_agent_build_info", "Agent version, kernel and cgroup version.",
		[]string{"version", "kernel", "cgroup"}, nil)
)

func (c *collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{descWait, descRun, descExits, descUnattr, descOutside, descNever,
		descLayout, descUnknown, descDrops, descReads, descContainers, descInfo} {
		ch <- d
	}
}

func (c *collector) Collect(ch chan<- prometheus.Metric) {
	totals := c.a.apply(c.r.read(), c.r.exists)
	for id, p := range totals {
		ch <- prometheus.MustNewConstMetric(descWait, prometheus.CounterValue, float64(p.wait)/1e9, id)
		ch <- prometheus.MustNewConstMetric(descRun, prometheus.CounterValue, float64(p.run)/1e9, id)
	}
	s := c.a.snapshot()
	counter := func(d *prometheus.Desc, v uint64, labels ...string) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.CounterValue, float64(v), labels...)
	}
	counter(descExits, s.exits)
	counter(descUnattr, s.unattributed)
	counter(descOutside, s.outside)
	counter(descNever, s.neverReceived)
	counter(descUnknown, s.layoutUnknown)
	counter(descDrops, s.drops)
	counter(descReads, s.reads)
	for _, l := range layouts {
		counter(descLayout, s.layouts[l.name], l.name)
	}
	for _, loc := range []string{"kubepods", "outside"} {
		ch <- prometheus.MustNewConstMetric(descContainers, prometheus.GaugeValue, float64(s.containers[loc]), loc)
	}
	ch <- prometheus.MustNewConstMetric(descInfo, prometheus.GaugeValue, 1, version, kernel(), c.r.h.version)
}

func kernel() string {
	var u unix.Utsname
	if unix.Uname(&u) != nil {
		return "unknown"
	}
	return strings.TrimRight(string(u.Release[:]), "\x00")
}
