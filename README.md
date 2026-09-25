# tare

Right-sizes Kubernetes workloads by asking the kernel whether they are waiting
for CPU, not only how much they used, and explains every number it produces.

## Usage tells you what a container got, not what it needed

Every right-sizer works from CPU usage. Usage has a blind spot: it can only show
what the container was given. A container capped at 100m that wants 400m shows
100m of usage, which is exactly what a correctly sized container shows too. From
the usage graph alone the two look the same.

Linux has measured the difference since kernel 4.20. **PSI (Pressure Stall
Information)** records, for every cgroup, how much of the time its tasks were
ready to run but left waiting for a CPU. Kubernetes exposes it through cAdvisor as
`container_pressure_cpu_waiting_seconds_total`. The rate of that counter is
seconds spent waiting per second of wall time:

| PSI | meaning |
|---|---|
| `0.00` | never waited: every time it wanted CPU, it got it |
| `0.05` | something in the container was waiting 5% of the time |
| `0.87` | waiting 87% of the time: starved |

Measured on tare's test cluster, over the same window:

```
container               PSI waiting   what it is
busy loop, limit 100m   0.8754        starved: usage sat at its limit the whole time
tomcat                  0.0003        healthy
nginx                   0.0000        idle
```

The busy loop's usage graph is a flat line at its limit, the same line a
correctly sized container would draw. PSI separates all three without needing
any other signal.

**Why not CPU throttling?** Throttling counters only exist for containers that
have a CPU limit, and most workloads are deployed without one. On the test
cluster, **PSI covered 23 of 23 containers; throttling covered 3 of 23.** A
container without a limit can't be throttled, but it can still wait for a CPU
that another pod on the node is using, and PSI records that wait.

tare uses PSI in both directions. Pressure above zero means don't shrink, and
possibly grow. Pressure at zero across the whole window is what makes a shrink
safe to recommend.

**What PSI does not tell you.** It says the container waited, not why. The
cause can be the container's own request, or a noisy neighbour on a contended
node, and tare does not yet separate the two. It measures CPU only; tare does not
use memory or IO pressure. If your metrics backend does not have the series,
`kubectl tare check` says so, and shrink recommendations drop to `UNVERIFIED`
instead of treating the missing signal as zero.

## It also reads the runtime's own ceiling

A Tomcat started with `JAVA_OPTS=256m` will never use more than 256Mi of heap,
however much memory you give it. Every right-sizer sizes it from observed usage
anyway:

```
                                           memory recommendation
observed usage + 15%                                       72Mi   OOMKill
the same, with a 100Mi floor                              100Mi   OOMKill
the same, with a 250MB floor                              238Mi   OOMKill
                                          the heap alone is 256Mi
tare                                                      384Mi   256Mi heap + 128Mi non-heap
```

tare reads the flag, reports it as the finding, and refuses to recommend below
it. Sizing from observed usage alone cannot get there, however the floor is set.

*tare: the weighing term for subtracting the container's own weight.*

## What it prints

```
$ kubectl tare report
# section=findings
VERDICT    CONF    NAMESPACE  WORKLOAD     CONTAINER  PODS  CPU        MEM          WHY
SATURATED  medium  prod       checkout     api        6     50m→115m   16Mi         OOMKILLED,CPU_PRESSURE
IDLE       high    prod       legacy-sync  worker     1     500m→10m   256Mi→16Mi   IDLE_NO_WORK,IDLE_NOT_RULED_OUT
WASTE      medium  prod       static       nginx      12    1000m→10m  1.0Gi→16Mi   -
WASTE      medium  prod       catalog      tomcat     4     200m→10m   512Mi→384Mi  CPU_SIZED_FOR_STARTUP,MEM_RAISED_BY_CEILING

35 containers over 2w: reclaim 1.2 cores, 876Mi; add back 65m, 0; nodes 12, freed >=2
warning  SIGNAL_NOT_MEASURED  cfs-throttle: 3 of 35 containers
warning  PODS_PINNED          38 pods held their node, so freed is a lower bound
not listed: 31 OK (--verdict OK)
```

stdout is the findings table and nothing else: one row per container somebody
might act on. `CPU` and `MEM` are the request and where it should go; `WHY` is
the evidence codes that are news for that row. Under it, on stderr, come one
summary line and a warning for everything the run could not see, so a terminal
shows them and `> findings.txt` or a pipe gets only the table. Two more views
are one flag away:

```sh
kubectl tare report -o wide     # every section: limits, pressure, one row per evidence code, node detail
kubectl tare report --explain   # the sentence behind every code
```

No agent, no CRD, no changes to your cluster. It reads the Kubernetes API and a
Prometheus you already have.

## Install

tare is one static binary with no runtime dependencies. It is not published to a
module proxy yet, so build it from this tree:

```sh
./tools/build.sh                                 # this machine, into bin/
sudo install bin/kubectl-tare /usr/local/bin/
```

Anything named `kubectl-*` on your `PATH` becomes a kubectl subcommand, so it is
available immediately as `kubectl tare`. The build is stamped with a version;
`kubectl tare version` prints it, and it is the first thing to quote when a
number looks wrong.

Building for someone else's laptop:

```sh
./tools/build.sh linux/amd64 darwin/arm64 darwin/amd64
```

## First run
```sh
kubectl tare check
```

`check` answers, before any report exists, whether this cluster can produce one:
which reads your token is allowed, whether a Prometheus-compatible endpoint is
reachable through the API server, which of the nine metric families tare uses
this cluster actually exports, how much history it holds, and how many of your
containers the backend has series for. Every line says what it costs, because
"missing" is not actionable:

```
series
  ✓ container_cpu_usage_seconds_total              36 series
  ✗ container_pressure_cpu_waiting_seconds_total   absent: CPU saturation evidence on cgroup v2:
                                                   without it or the throttle counters below, no
                                                   shrink can be called safe and findings drop to
                                                   UNVERIFIED
  ✗ coverage                                       only 50 of 66 containers in scope have metrics.
                                                   Missing in monitoring (1), tare-lab (15)
```

The last line is the one most worth reading on an unfamiliar cluster: a
Prometheus scraping a subset of namespaces passes every other check and produces
a report describing a fraction of your workloads.

## Use

```sh
kubectl tare report                            # whole cluster
kubectl tare report -n prod                    # one namespace
kubectl tare report --window 14d --explain     # longer history, with the reasoning spelled out
kubectl tare report --top 20 --by-savings      # the twenty biggest wins
kubectl tare report --verdict SATURATED        # only what is hurting right now
kubectl tare report -o wide                    # every section and evidence row, for a script or a ticket
kubectl tare report -o json | jq               # for a pipeline
kubectl tare report -o patch > fix.sh          # runnable kubectl patch commands
kubectl tare report -o html > report.html      # one self-contained file you can send to someone
kubectl tare report -o tui                     # browse the findings interactively
kubectl tare report --redact                   # hash every namespace, workload and node name
kubectl tare report --timeout 30m              # a large cluster is mostly waiting for the API server
kubectl tare version                           # which build this is
```

`-o tui` exists because `-v` is all-or-nothing: on a cluster with three hundred
containers the evidence for every row at once is a log, not a report. Arrow keys
or `j`/`k` move, `enter` opens the evidence for the row under the cursor,
`a s w i l u` filter by verdict, `/` searches by name, `q` quits. An open
row also plots CPU and memory over the window, one range query to Prometheus
when you open it: each column is the worst pod's peak in that slice of time,
with the request, limit and recommendation drawn as lines, or named above the
chart when they are off its scale. A blank column is no sample, never zero.
Only the pods the report analysed are plotted, so a rollout inside the window
shows as a series that starts late. `--redact` turns the plot off. It is
hand-rolled on `golang.org/x/term`, which client-go already pulls in, so the
interactive mode costs no new dependency.

You can see all of this without a cluster:

```sh
kubectl tare report --demo                     # simulated workloads, real analyzer
kubectl tare report --demo -o html > demo.html
```

The demo workloads are the same catalog the test suite sizes against, so the
verdicts are produced by the real analyzer over real (simulated) series rather
than canned. Every page built from them says so at the top, because a report
that looks like a cluster and is not one would be the most misleading thing
this tool could produce.

The HTML report exists for the case the terminal cannot serve: the person who
decides on a resize is usually not the person who ran tare, and often has no
access to the cluster's Prometheus. It is one file with no scripts, fonts or
network calls of its own, it sorts and filters, and every row opens onto the
evidence behind it. It is deliberately not a dashboard — there is no headline
currency figure, a withheld recommendation renders as **withheld** rather than
as a blank or a zero, and the signals the run could not see sit at the top of
the page rather than in a tooltip.

Every run's summary line ends with the number that corresponds to a bill: nodes freed. Reclaimed millicores
are not that number: a request is a scheduling reservation, and lowering one
changes nothing you pay until a node goes away. `-o wide` shows how it was
reached:

```
# section=totals
KEY                         VALUE      HUMAN
containers.total            4          -
verdict.SATURATED           1          -
verdict.IDLE                1          -
verdict.WASTE               2          -
reclaimable.containers      3          -
reclaimable.cpu_millicores  1180       1.2 cores
reclaimable.mem_bytes       918552576  876Mi
addback.containers          1          -
addback.cpu_millicores      65         65m
addback.mem_bytes           0          -

# section=nodes
KEY                       VALUE  DETAIL
nodes.computed            true   -
nodes.running             12     -
nodes.needed_now          11     -
nodes.needed_after        9      -
nodes.from_consolidation  1      already free without resizing anything
nodes.from_resizing       2      ip-10-0-3-41, ip-10-0-5-12
nodes.pinned_pods         38     held their node, so nodes.from_resizing is a LOWER bound
nodes.movable_pods        176    -
nodes.unusable            0      -

# section=node_pins
PODS  REASON
21    has a PersistentVolumeClaim, which may bind it to a zone or a node
11    has affinity or anti-affinity rules
6     requests an extended resource such as a GPU
```

tare reports nodes, not currency: only you know what your nodes cost.

The three node counts are kept apart on purpose. A cluster that is merely not
tightly packed can free nodes today without changing a single request, and that
saving is real but it is not tare's. Only the difference between packing
today's requests and packing the recommendations was earned by the tool.

Before any of it is reported, the packer has to fit every pod onto the node it
is demonstrably already running on. If tare's capacity model cannot reproduce a
placement the scheduler actually made, it says so and withholds the number
rather than predicting a future it got the present wrong about.

There is no required configuration. tare finds a Prometheus-compatible service
in your cluster and queries it through the Kubernetes API server proxy, so it
works wherever `kubectl` works: no port-forward, no ingress, no VPN. Point it
somewhere else with `--prometheus http://localhost:9090` if you prefer.

Thanos, Mimir and VictoriaMetrics are preferred over Prometheus when present,
because this tool wants weeks of history and a default Prometheus keeps about
fifteen days.

## Output contract

Both table views are meant to be read by a person and parsed by a script
without either getting in the other's way.

- A line beginning with `#` is a section marker or a comment. Never data.
- `# section=<name>` opens a section; the next line is its column header.
- Columns are separated by two or more spaces. Values never contain two
  consecutive spaces; free text lives in the last column only.
- A field with no value is `-`. Never blank.
- Hints, progress and warnings go to stderr. stdout is data.

The default view puts one section on stdout, `findings`, and the summary and
warnings on stderr. It leaves out what asks nothing of the reader: `OK` and `NO-DATA` rows are counted
in a comment instead of listed (any `--verdict` lists everything it selects),
and `WHY` omits the codes the row already shows (`CPU_UNDERUSED`,
`MEM_UNDERUSED`, `CPU_FLOOR`, `MEM_FLOOR`, `BEST_EFFORT`) and stops at two with
a `+N`. What it never leaves out is a gap: every signal the run could not see,
every node it could not use, every budget that blocks a drain is a `warning`
line on stderr, once, however many containers it touches.

`-o wide` is the whole record: `meta`, `signals`, `findings` (with limits and
pressure), `evidence`, `totals`, `nodes`, `node_pins`, `drain_blockers`,
`commitment`. The `evidence` section is one row per piece of evidence behind a
finding, keyed by a stable `CODE`:

```sh
# every container with a past OOMKill
kubectl tare report -o wide | awk '$4 == "OOMKILLED" {print $1, $2, $3}'

# how many findings rest on each kind of evidence
kubectl tare report -o wide | sed -n '/section=evidence/,/^$/p' | awk 'NR>2 {print $4}' | sort | uniq -c | sort -rn
```

Codes are API. New ones get added; an existing one does not change meaning,
because a filter on `OOMKILLED` that silently matches nothing after a rename is
the exact shape of every serious bug this tool has had.

`OBSERVED` and `AGAINST` are rendered for a terminal. `-o json` carries the same
reasons with the raw numbers, which is what to use when the arithmetic matters:

```json
{"code": "MEM_LEAK", "observed": 5905580032, "against": 230686720, "unit": "bytes",
 "text": "memory grew from 220Mi to 5.5Gi across the window, a 25.6x increase: ..."}
```

`--explain` appends the sentence behind every code, in either view. The tables
say what was measured; the sentences say what it means and what it does not
rule out, which is the part no column can carry — `IDLE_NO_WORK` does not tell
you that a hot standby, a DR replica and a controller waiting on a rare event
all look exactly like this from CPU and memory.

## Permissions

tare only reads, and it reads as you. On a kubeconfig that already has cluster
read access there is nothing to set up.

For a service account, a CI job, or an admin who would rather grant exactly
what is needed, `deploy/rbac.yaml` is that grant and nothing else: `get` and
`list` on pods, services, nodes, the six controller kinds, PDBs, HPAs and VPAs,
plus `get` on `services/proxy`, which is how the query reaches Prometheus
without a port-forward. There is no write verb in the file.

```sh
kubectl apply -f deploy/rbac.yaml
kubectl -n kube-system create token tare --duration 1h
```

**A narrower token still works.** Every read but the pod list degrades rather
than failing, and the report says what it lost in the header, in every output
format:

```
signals unavailable on this cluster:
  - horizontal pod autoscalers in namespace prod could not be read (forbidden): if any of these
    workloads is scaled on CPU or memory utilisation, resizing it moves the scaling setpoint and
    tare cannot warn you which ones
```

With a token scoped to one namespace, name it (`-n prod`) and pass
`--prometheus`, since finding the endpoint needs a cluster-wide service list.
`kubectl tare check` prints exactly which of these apply to you.

## Large clusters

Every API read is a list, and the number of them is fixed: they do not grow with
the number of pods. A cluster-wide run is therefore mostly waiting on the API
server and on Prometheus, and the default budget is ten minutes
(`--timeout 30m` to raise it, `-n <namespace>` to narrow the work, a shorter
`--window` to make the Prometheus queries cheaper).

The report goes through the API server's service proxy, which is the right
choice for one report and the wrong one for a query every minute.

## Sending a report to someone else

`--redact` replaces every namespace, workload, container and node name with a
stable hash, in the columns and in the evidence text, and says so in the header.
It is for sending a report outside the organisation - to a vendor, or attached
to a bug report against this tool. The hash is stable, not secret: anyone who
guesses "payments" can confirm it.

## What makes it different

**It refuses to recommend a size it cannot justify.** A JVM started with
`-Xmx256m` will never use more than 256MB of heap however much memory you give
it. Sizing that container to its observed usage produces a number that looks
reasonable and OOMKills on the next restart:


tare reads `-Xmx`, `MaxRAMPercentage`, `GOMEMLIMIT`, `GOMAXPROCS`,
`--max-old-space-size`, gunicorn worker counts and about twenty
application-specific heap variables, and when the flag is not in the pod spec at
all, infers the ceiling from a memory plateau instead.

**Requests and limits get different answers.** A request sized to a percentile
must not double as a limit that throttles every burst. And a limit sized to
steady state throttles every restart: measured on real workloads, a Tomcat draws
**19.7x** more CPU booting than running.

**It says when it cannot see.** Metrics differ between clusters. CFS throttle
counters only exist for containers that already have a CPU limit; PSI exists for
all of them. When a signal is missing, findings that depended on it are
downgraded and the report says so, rather than reading a missing metric as a
comfortable zero.

**It knows what else is managing the workload.** A pod with requests equal to
limits is Guaranteed, and splitting them demotes it to Burstable; tare keeps them
equal and says so. An HPA scaling on CPU utilisation measures against the
request, so changing the request moves the scaling setpoint; tare warns instead
of trading a CPU saving for extra replicas. A workload already managed by a VPA
in Auto mode gets no recommendation at all, because two controllers writing the
same field is a fight rather than a merge.

**It finds the requests nobody wrote.** Kubernetes copies an omitted request
from the limit, so a manifest reading `limits: {cpu: "2", memory: 1Gi}` does not
cap a burst at two cores: it reserves two cores and a gigabyte on a node for
every replica, forever, and quietly makes the pod Guaranteed. A running pod
cannot be asked about this, because the copied value looks exactly like a chosen
one. tare compares the controller's pod template against the pod it created,
which is the only place the omission survives.

**It proves when a CPU limit is hurting you for nothing.** CFS throttles a
container at its quota whatever the node is doing, so a container can stall over
and over a few microseconds from idle cores. Whether to set CPU limits at all
has been argued for years on both sides, almost entirely from anecdote. tare
settles it per workload, with the throttle ratio next to the node's own measured
utilisation:

```
SATURATED  tare-lab/deploy/cpu-hog  hog
  - throttled 100.0% of scheduling periods against a 100m CPU limit, while the
    busiest node it ran on averaged 5.9% CPU across the window and stayed below 7.7%
    for 95% of it.
    The constraint is this container's own limit, not the machine. Raising or
    removing the limit buys back that time without buying hardware. It is not free
    either: check the node commitment table below first, because limits are what
    stop a busy node from being oversubscribed.
```

It deliberately says nothing about latency. tare has no latency signal, so a
figure like "this cost you 180ms of p99" would be invented rather than measured,
and a test asserts the finding never contains one. Where node metrics are
missing, it names the missing metric instead of assuming the node was idle.

**It shows what your nodes have promised, not just what they have reserved.**
The scheduler reads requests and ignores limits, so a node can look 85% free and
be committed to twice its own capacity. That node keeps accepting pods and
stalls the moment enough of them get busy together, and a right-sizing report
that only shrinks requests makes it worse:

```
node commitment (requests are what the scheduler reserved, limits are what the pods may take):
  NODE                    PODS  CPU req  CPU lim  MEM req  MEM lim  NOTE
  k3d-tare-test-server-0  13    17%      197%     19%      118%     memory OVERCOMMITTED: if these pods all
                                                                    claim their limit the kernel starts OOMKilling
  k3d-tare-test-agent-1   10    19%      144%     32%      100%     memory OVERCOMMITTED
  k3d-tare-test-agent-0   10    8%       120%     32%      104%     memory OVERCOMMITTED
```

Memory is called out separately from CPU because the consequences differ in
kind: CPU overcommit throttles, memory overcommit means the kernel picks a
process and kills it.

**It counts nodes, not millicores.** Every other right-sizer reports the
resource it reclaimed. Nobody is billed for a reclaimed millicore. tare replays
your recommendations through bin packing against real node allocatable and real
pod placement and tells you how many nodes stop being needed, which is often
zero, and says so when it is.

**It reads inference servers' configuration, not their usage.** A vLLM process
started with `--gpu-memory-utilization 0.9` claims 90% of the device at boot and
holds it whether or not a request ever arrives, so its observed GPU memory
describes the flag and not the demand. tare parses vLLM, Ollama, TGI and
llama.cpp the same way it parses the JVM: the model's weights against the
device's VRAM, `OLLAMA_KEEP_ALIVE=-1` pinning a model in memory forever, a
missing `-ngl` running every layer on the CPU while holding an allocated GPU.

**It knows what it is running on.** A DaemonSet's replica count is your node
count, so its saving grows with the cluster and its request is subtracted from
every node before anything else is scheduled. A container under a 2-core quota
on a 96-core node whose runtime sized its thread pool from the node will
exhaust that quota early in every scheduling period, and that is a
configuration defect no resizing fixes. A recommendation larger than your
biggest node makes the pod Pending rather than faster. And when a workload's
pods ran on different hardware, tare lowers its confidence instead of inventing
a per-core conversion factor it has no way to measure.

**It tells you when the answer is deletion.** A workload with no measurable CPU
all window and flat memory gets an `IDLE` verdict rather than a smaller number,
because resizing something that should not exist is the smaller half of the
saving. It still prints the size, and it prints what it cannot rule out: a hot
standby, a rare-event controller, a job whose period is longer than the window.

**It produces a change someone else can approve.** The person who runs tare owns
the cluster; the manifests belong to other teams. `kubectl tare diff --repo
./manifests` edits the resources where they sit, so the change arrives as a
normal pull request instead of a screenshot of a table:

```
$ kubectl tare diff --repo ./manifests --write --pr-body pr.md
wrote ./manifests/diurnal-web.yaml (1 container(s))
...
5 file(s) changed. Review with: git -C ./manifests diff

 diurnal-web.yaml | 8 ++++----
-            cpu: "1"
-            memory: 512Mi
+            cpu: 718m
+            memory: 20Mi
```

Only the values move. Comments, key order and formatting are preserved, because
the file is spliced rather than re-serialised, and every edited file is
re-parsed before it is written. Kustomize repositories get strategic-merge
patches instead (`--kustomize`), which need no mapping from a rendered object
back to its source. Helm templates are skipped and reported rather than mangled.

`--pr-body` writes the evidence behind every number, which is the part a
reviewer cannot get from the diff and the reason they merge it.

**Nothing is applied.** `report` is read-only. `-o patch` emits `kubectl patch`
commands for you to review and run, deliberately not a manifest for
`kubectl apply`: apply does a three-way merge and would treat a resources-only
fragment as the whole desired state. Automatic actuation is on the roadmap as an
explicit, per-workload opt-in.

## How it decides

Saturation evidence, not a guessed performance curve. CPU pressure (PSI),
throttling where available, OOMKills and restarts say directly whether a
container is being starved; the kernel already measured it. Requests come from a
steady-state percentile, limits from the peak, including the startup burst.
Memory is never reduced below a detected runtime ceiling.

Every recommendation carries a confidence rating driven by how much evidence
actually backs it, because most clusters retain about fifteen days of metrics
and many containers are younger than that.

## What it does not do yet

- Apply anything by itself. `-o patch` gives you the commands; you run them.
- Read application latency or SLOs. The saturation signals are
  infrastructure-level; SLO-aware sizing is the next milestone.
- Understand Spark or Flink memory configuration.
- Report on init containers or native sidecars.
- Account for pod-level resources (Kubernetes 1.34+).

## Development

```sh
go test ./...              # full suite
go test -short ./...       # skips the threshold sweep
```

The analyzer is tested against a workload simulator in `internal/simulate`,
which models twelve archetypes whose correct size is known in advance and runs
thirty simulated days in about 27ms. Recommendations are scored by replay: apply
the tool's own output as a hard limit, run the window again, and require the
workload survives.

`internal/ceilings` is measured against a corpus of 135 real container specs
extracted from 39 public Helm charts. See `test/corpus/README.md`.

Design decisions and their reasoning are in `docs/plan.md`.

## License

Apache-2.0.
