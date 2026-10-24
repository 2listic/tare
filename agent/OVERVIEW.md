# tare-agent: what it does and why

How to build, deploy and use it is in [`README.md`](README.md). This page is the short
version of what it is for.

## Why

tare will not lower a container's CPU while the container is starved of it. It finds
out from PSI (`container_pressure_cpu_waiting_seconds_total`): the time the container
had a task ready to run but no CPU to run it on.

Many clusters do not have PSI:

- cgroup v1 nodes (no per-cgroup PSI at all);
- RHEL 8, 9 and 10 kernels, where PSI ships switched off;
- kubelets older than 1.33, which do not export it.

There tare cannot tell a starved container from a quiet one, and "no pressure" is
exactly what it reads as "safe to shrink".

The existing alternative, cAdvisor's `container_cpu_schedstat_runqueue_seconds_total`,
does not work: it reads only the main thread of each live process, so it is blind to
thread pools (JVM, Go, .NET, Node) and to short-lived processes (fork-per-connection
servers, scripts). On real workloads it saw 0–9% of the CPU time of most containers.
Signals that need no agent (node PSI, usage against the request) tell that the node
is busy, not which container waits.

## What

The agent is a small DaemonSet (Go, about 0.002–0.05 cores and 20 MiB per node) that
exports one number per container: **CPU wait**, the seconds its tasks spent ready to
run but waiting for a CPU, summed over **all** of its tasks:

- **live threads**: on every scrape it reads `/proc/<pid>/task/<tid>/schedstat` for
  every thread of every process in the container's cgroup;
- **exited tasks**: between scrapes the kernel sends it the final counters of every
  task that exits (taskstats exit notifications), so short-lived processes are
  counted too.

It also exports the CPU time it accounted (`tare_cpu_run_seconds_total`), so coverage
against the kubelet's CPU usage can be checked, and self-checks (unattributed exits,
unknown kernel record layouts, dropped notifications) that catch the silent failures
found during testing.

Series are labelled only with the cgroup path (`id`), the same label as the kubelet's
`container_*` series, so the agent needs no Kubernetes API access.

## How the code works

Four files, one process with two concurrent parts: a **scrape path** that reads the
live tasks when Prometheus asks, and an **exit listener** that receives the final
counters of tasks as they exit. Both feed one shared state.

| file | role |
|---|---|
| `main.go` | flags, startup, the HTTP server (`/metrics`, `/healthz`), the Prometheus collector |
| `cgroups.go` | finds the containers in `/sys/fs/cgroup` and reads their threads from `/proc` |
| `exits.go` | the netlink listener for taskstats exit records, and their decoding |
| `account.go` | merges live and exited tasks into one counter per container |

**Startup (`main.go`).** It opens the host's `/proc`, detects the cgroup version (on
v2 it walks `/sys/fs/cgroup`, on v1 the `cpu,cpuacct` tree), starts the exit listener
in the background and serves `/metrics` on port 9105. If the exit listener fails, the
agent keeps serving the live threads, logs why, exports `tare_agent_exit_listener_up 0`
and fails `/healthz`, so the pod shows as not ready.

**Finding containers (`cgroups.go`).** On every scrape it walks the cgroup tree and
keeps the directories whose path contains a 64-hex container id
(`cri-containerd-<id>.scope`, `crio-<id>.scope`, cgroupfs `<id>`, …). Paths are cut
after that component, so CRI-O's nested `…/container` cgroup maps to the same `id` the
kubelet uses. It also counts containers found outside `kubepods` (a cgroup driver
mismatch).

**Reading live tasks (`cgroups.go`).** For each container cgroup it reads the PIDs in
`cgroup.procs`, then every thread of each process
(`/proc/<pid>/task/<tid>/schedstat`): time spent running and time spent waiting in the
run queue. It also remembers which container each process belongs to, for the exit
listener.

**Receiving exits (`exits.go`).** It opens a generic **netlink** socket, looks up the
kernel's `TASKSTATS` family and registers for exit notifications on every online CPU
(`/sys/devices/system/cpu/online`). It asks for a 64 MB receive buffer
(`SO_RCVBUFFORCE`, needs `CAP_NET_ADMIN`), because busy nodes send hundreds of records
per second. For each exiting thread the kernel sends a `struct taskstats` with its
final wait (`cpu_delay_total`) and run time, its pid, parent pid and process id. The
struct layout differs between kernels even at the same version (upstream vs RHEL 10),
so the agent recognises the layout per record by finding the thread id at the
expected offset; unknown layouts are counted, not guessed. A receive error means
records were lost and is counted as a drop.

**Attributing an exit (`account.go`).** An exiting thread has to be placed in a
container, though it may already be gone from `/proc`. In order: where the thread was
seen in the last reads; its process; its own `/proc/<pid>/cgroup` (accepted only if
it is a container cgroup, since on some kernels an exiting task reports a non-pod
cgroup); finally its parent. Exits of host processes are counted as "outside", the
ones that cannot be placed at all as "unattributed".

**Merging (`account.go`).** On every scrape, each of a container's threads is in
exactly one of three groups, and CPU wait is the sum of the three:

| group | which threads | value counted |
|---|---|---|
| exited | the exit record has arrived | the final value from the record |
| vanished | was in `/proc` at the previous scrape, is not now, and no exit record yet | its last read (a placeholder) |
| live | is in `/proc` now, and no exit record yet | the current read |

The groups never overlap:

- a thread missing from `/proc` cannot be live;
- when its exit record arrives, the placeholder is deleted and the final value takes
  its place, so the counter never goes down in between;
- a thread still visible in `/proc` after its exit record (a zombie, or a read racing
  the exit) is skipped in the live sum;
- a thread that was missing from one read and is back is live again, and its
  placeholder is deleted (only if it is the same thread: same container, counters
  not lower, since a reused thread id starts from zero);
- a placeholder whose record has not arrived within 30 seconds becomes final
  ("never received"). The limit is time, not a number of scrapes, because several
  scrapers make reads milliseconds apart; a record that arrives later still adds only
  what the placeholder did not count.

Scrapes are serialised: each one changes this state, and two whose reads were applied
out of order would make a counter go down, which Prometheus takes for a reset.

Containers whose cgroup is gone are dropped from the output.

**Exporting (`main.go`).** The collector turns the totals into `tare_cpu_wait_seconds_total`
and `tare_cpu_run_seconds_total` per `id`, plus the self-checks (`tare_agent_exits_*`,
`tare_agent_netlink_drops_total`, `tare_agent_containers`, `tare_agent_build_info` with
kernel and cgroup version) and the agent's own process and Go runtime metrics.

Everything is computed at scrape time; the agent keeps no history. Rates, coverage and
the decision rules are computed later in Prometheus or by `compare-pressure`.

## How tare would use it

CPU wait is a sum over tasks, not wall time, so it is not on PSI's scale and PSI's
0.03 threshold does not apply. The rule that worked best is **`relative-slowdown`**:

- slowdown = CPU wait ÷ CPU usage (seconds waited per second run);
- a 5-minute stretch is stalled when the slowdown is at least 2× the container's own
  normal (its 10th percentile) and CPU wait ≥ 0.01 s/s, or when the slowdown is ≥ 1.

Against the contention actually created in the tests it was right in 98.5% of
windows, against 91.6% for PSI with tare's current threshold, on cgroup v1 as well.

`compare-pressure` applies this rule (and `fixed-slowdown`, `absolute-wait` and PSI
itself) to Prometheus data, so it can be checked on a real cluster before tare reads
the agent.

## Limits

- Proof of concept: tare does not read it yet; the image is built locally.
- It must be privileged (`hostPID`, `hostNetwork`, `CAP_NET_ADMIN`).
- It says whether work ran slower, not how much CPU is missing: it cannot size an
  increase.
- A container that is never quiet has no normal to compare with and must be reported
  as UNVERIFIED.
