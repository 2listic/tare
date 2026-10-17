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
cause can be the container's own allocation or a noisy neighbour on a contended
node. tare separates the two only partly: pressure on a container using a small
fraction of its allocation is reported as `NODE_CONTENTION` rather than
saturation, and a throttled container on a quiet node gets `LIMIT_NOT_NODE`.
Neither is a proof. It measures CPU only; tare does not use memory or IO
pressure. If your metrics backend does not have the series,
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
SATURATED  medium  prod       checkout     api        6     50m→115m   16Mi→?       OOMKILLED,CPU_PRESSURE,+1
IDLE       high    prod       legacy-sync  worker     1     500m→10m   256Mi→16Mi   IDLE_NO_WORK,IDLE_NOT_RULED_OUT
WASTE      medium  prod       static       nginx      12    1000m→10m  1.0Gi→16Mi   -
WASTE      medium  prod       catalog      tomcat     4     200m→10m   512Mi→384Mi  CPU_SIZED_FOR_STARTUP,MEM_RAISED_BY_CEILING

35 containers over 2w: nodes freed >=2 of 12; requests lowered on 2 (-12640m CPU, -12.3Gi mem), raised on 1 (+390m CPU, 0 mem); memory unknown on 1 (OOMKilled); 1 IDLE not counted
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
sudo ln -sf kubectl-tare /usr/local/bin/kubectl_complete-tare
```

Anything named `kubectl-*` on your `PATH` becomes a kubectl subcommand, so it is
available immediately as `kubectl tare`, and `kubectl_complete-tare` gives it
tab completion for subcommands, flags, formats, verdicts and namespaces wherever
kubectl's own completion is enabled. The build is stamped with a version;
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

`-o tui` exists because `--explain` is all-or-nothing: on a cluster with three hundred
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

Every run's summary line starts with the number that corresponds to a bill:
nodes freed. The request changes come after it and are never called savings: a
request is a scheduling reservation, and lowering one changes nothing you pay
until a node goes away. `-o wide` shows how both were reached. The figures below
are the ones the table above adds up to:

```
# section=verdicts
VERDICT    CONTAINERS
SATURATED  1
IDLE       1
WASTE      2
OK         31
ALL        35

# section=totals
CHANGE   CONTAINERS  CPU_REQUESTS  MEM_REQUESTS  NOTE
lower    2           -12640m       -12.3Gi       -
raise    1           +390m         0             -
idle     1           -490m         -240Mi        not_in_lower:IDLE_NOT_RULED_OUT
unknown  1           -             ?             MEM_DEMAND_CENSORED

# section=nodes
STAGE         NODES  FREED  CREDIT         EMPTY_NODES
running       12     -      -              -
packed_today  11     1      cluster_slack  ip-10-0-7-2
packed_after  9      >=2    tare           ip-10-0-3-41, ip-10-0-5-12, ip-10-0-7-2

# section=node_caveats
CAVEAT       COUNT  DETAIL
pinned_pods  38     of 214 pods held their node, so tare's FREED is a lower bound; reasons in node_pins

# section=node_pins
PODS  REASON
21    has a PersistentVolumeClaim, which may bind it to a zone or a node
11    has affinity or anti-affinity rules
6     requests an extended resource such as a GPU
```

`totals` is the change to requests, multiplied by pod count, signed: `lower` is
what the recommendations take away, `raise` is what the starved containers need
added, and the two are never netted. `idle` is kept out of `lower` because its
own evidence says `IDLE_NOT_RULED_OUT`; the node replay leaves it at its current
requests for the same reason. `unknown` counts containers whose memory tare
cannot size: `16Mi→?` above is `checkout`, OOMKilled at its limit. Usage above
the limit that killed a container is never observed, so its peak is a lower
bound on demand and not a measurement of it. tare says so, prints `?`, and
`-o patch` leaves that container's memory alone rather than invent a step.

`nodes` is the replay, one row per stage. `FREED` is the drop from the row
above and `CREDIT` says whose saving it is. `EMPTY_NODES` is what is empty at
that stage, so the `packed_after` list includes the node repacking alone frees:
the two passes are packed independently, and which pass emptied which node is
not something the replay can say.

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
warnings on stderr. It leaves out what asks nothing of the reader: `OK`, `UNSET` and `NO-DATA` rows are counted
in a comment instead of listed (any `--verdict` lists everything it selects),
and `WHY` omits the codes the row already shows (`CPU_UNDERUSED`,
`MEM_UNDERUSED`, `CPU_LIMIT_UNDERUSED`, `MEM_LIMIT_UNDERUSED`, `CPU_FLOOR`, `MEM_FLOOR`, `BEST_EFFORT`) and stops at two with
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
reasonable and OOMKills on the next restart (the table at the top of this file).
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
$ kubectl tare report -n tare-lab --verdict SATURATED --explain
SATURATED  tare-lab/Deployment/cpu-hog [hog]  confidence medium (1436 samples)
    CPU_PRESSURE         CPU pressure 0.900 s/s exceeds 0.030 while using 100% of its allocation: ...
    CPU_PINNED_AT_LIMIT  usage is pinned at the 100m CPU limit; raise the limit, not just the request
    LIMIT_NOT_NODE       throttled 100.0% of scheduling periods against a 100m CPU limit, while the
                         busiest node it ran on averaged 8.1% CPU across the window and stayed below
                         11.0% for 95% of it. The constraint is this container's own limit, not the
                         machine. Raising or removing the limit buys back that time without buying
                         hardware. It is not free either: check the node commitment table first,
                         because limits are what stop a busy node from being oversubscribed.
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
$ kubectl tare report -o wide | sed -n '/section=commitment/,/^$/p'
# section=commitment
NODE                    PODS  CPU_REQ_PCT  CPU_LIM_PCT  MEM_REQ_PCT  MEM_LIM_PCT  NOTE
k3d-tare-test-agent-1   13    24           224          25           122          MEM_OVERCOMMIT,UNLIMITED_CPU_CONTAINERS=2,UNLIMITED_MEM_CONTAINERS=2
k3d-tare-test-server-0  8     10           201          20           113          MEM_OVERCOMMIT,UNLIMITED_CPU_CONTAINERS=3,UNLIMITED_MEM_CONTAINERS=3
k3d-tare-test-agent-0   2     1            3            1            2            UNLIMITED_CPU_CONTAINERS=1,UNLIMITED_MEM_CONTAINERS=1
```

`UNLIMITED_*_CONTAINERS` counts containers with no limit at all. Their
consumption is unbounded, so the limit percentages are a floor wherever it is
non-zero.

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

## Hands-on session

About 75 minutes, for a group of colleagues on the office network. Part 1 needs
no cluster. Parts 2 and 3 use the shared lab cluster described in
`test/lab/README.md`: k3d `tare-test` on `msi` (192.168.1.122), context
`k3d-tare-test`, Grafana on :30030.

**Ground rules for the lab.** It is shared, and it is running a long stability
experiment. tare only reads, so `report`, `check` and `diff` are always safe.
Do not run the output of `-o patch`. Do not `kubectl apply`, `edit` or `delete`
anything in `tare-lab`. Use `diff --write` only on your own scratch copy of the
manifests (part 3), never on this repository.

### Before the session (each person, 10 minutes)

```sh
git clone <this repo> tare && cd tare
./tools/build.sh                      # needs Go 1.27; produces bin/kubectl-tare
export PATH=$PWD/bin:$PATH            # now `kubectl tare` works
kubectl tare version
kubectl tare report --demo            # must print a table; no cluster involved
```

Get the lab kubeconfig from whoever runs the session, then check you can reach
it:

```sh
kubectl --context k3d-tare-test get pods -n tare-lab
```

`jq` and a browser are useful but not required.

### Part 1: read a report without a cluster (15 minutes)

```sh
kubectl tare report --demo
kubectl tare report --demo --explain | less
```

The demo is the real analyzer run over simulated series, so every verdict is
computed, not canned. Answer from the output alone:

1. `leaky-service` and `slow-growth` have no arrow in `CPU` or `MEM`. Why does
   tare refuse to give them a size?
2. `hidden-ceiling-jvm` uses 240Mi at peak, yet the recommendation is 368Mi.
   Where does the extra come from, given that the pod spec sets no heap flag?
3. Applying the `no-limits-api` recommendation changes more than its size. What
   else changes?
4. `dormant-sidecar` is `IDLE`. What would you check before deleting it?
5. The summary line says nothing about nodes freed. Why not?

<details><summary>Answers</summary>

1. `MEM_LEAK`: memory grew 25.6x (leaky-service) and 3.0x (slow-growth) across
   the window. No size holds a leak, so the recommendation is withheld rather
   than invented.
2. `CEILING observed-plateau`: memory sits flat far below its limit, which for a
   JVM usually means a heap cap set in the image or a ConfigMap. tare infers the
   ceiling and raises the number (`MEM_RAISED_BY_CEILING`) instead of trusting
   usage.
3. `CPU_LIMIT_WOULD_BE_ADDED` and `MEM_LIMIT_WOULD_BE_ADDED`: the manifest sets
   no limits, so applying this introduces throttling at quota and makes the
   container OOMKillable at a cap. That is a behaviour change, not a resize.
4. `IDLE_NOT_RULED_OUT`: hot standby, DR replica, a controller waiting on a rare
   event, a job with a period longer than the window. tare sees CPU and memory,
   not intent. Ask the owner first.
5. `NODES_NOT_COMPUTED` on stderr: the demo has no real nodes, and a node count
   from imaginary nodes would be the misleading number tare exists to avoid.

</details>

Then try the machine-readable side:

```sh
# every finding and the evidence codes behind it
kubectl tare report --demo -o wide | sed -n '/section=evidence/,/^$/p'
# the same reasons with raw numbers
kubectl tare report --demo -o json | jq '.findings[] | {workload: .Workload, verdict: .Verdict}'
```

### Part 2: the lab cluster (20 minutes)

```sh
kubectl config use-context k3d-tare-test
kubectl tare check
kubectl tare report -n tare-lab
kubectl tare report -n tare-lab --explain | less
kubectl tare report -n tare-lab -o tui      # enter on a row plots it over the window
kubectl tare report -n tare-lab -o html > lab.html && open lab.html
```

Several people querying at once all go through the API server proxy to one
Prometheus. If a run times out, add `--timeout 5m`.

1. In `check`, read the `history` and `coverage` lines. How much history does
   the lab have, and what does that do to every finding?
2. `cpu-hog` sits flat at 100m of usage. A usage-based right-sizer would call
   that correctly sized. What does tare see that usage cannot show?
3. Open `cpu-hog` in Grafana (container CPU for `tare-lab/cpu-hog`) next to its
   tare row. Which of the two would you show a team to justify raising the limit?
4. `tomcat-lowxmx` is nearly idle, so why is its memory recommendation 384Mi
   and not something close to its 95Mi of usage?
5. `guaranteed-cache` uses about 27m against a 100m limit, yet tare keeps the
   limit at 100m. Why?

<details><summary>Answers</summary>

1. On 2026-10-11 it was 6h, because the lab machine was off for two weeks.
   Every finding is at most `medium` confidence, and the report says
   `WINDOW_SHORT`. Six hours cannot show a daily cycle. Keep that in mind for
   part 3.
2. `CPU_PRESSURE` of about 0.9 s/s: its tasks were ready to run and waiting 90%
   of the time. `LIMIT_NOT_NODE` adds that the node was about 8% busy, so the
   wait comes from its own limit, not from the machine.
3. Discussion. The Grafana graph shows a flat line; only the pressure figure
   shows that the line is a ceiling.
4. `JAVA_OPTS=-Xmx256m`. A 256Mi heap plus metaspace, code cache and thread
   stacks comes to about 384Mi. Usage is what an idle JVM touched, not what it
   will allocate under load.
5. `CPU_LIMIT_HELD_THROTTLED`: the limit throttled it in about 88% of scheduling
   periods. Usage is averaged over seconds, while CFS enforces the quota every
   100ms, so a limit sized from averaged usage would throttle it harder.

</details>

### Part 3: from finding to pull request (20 minutes)

Work on a throwaway copy of the lab manifests, never on this repository:

```sh
cp -r test/lab /tmp/tare-handson && cd /tmp/tare-handson
git init -q && git add . && git commit -qm "lab manifests as deployed"

kubectl tare diff -n tare-lab --repo .                          # dry run: what would change
kubectl tare diff -n tare-lab --repo . --write --pr-body pr.md  # edit the files
git diff
less pr.md
```

Check that the diff touches only resource values, keeps comments and formatting
intact, and that `pr.md` lists what tare deliberately left alone and why.

Now review it as if a colleague had opened this pull request. **Find the hunks
you would reject.** There are at least three, and tare gives none of them a
low-confidence warning.

<details><summary>Answers</summary>

- **`diurnal-web` in `longrun.yaml`:** memory 256Mi to 16Mi and CPU limit
  1000m to 562m, sized from 6h of a workload whose load follows the day. The
  window has not seen a peak. The report flags `WINDOW_SHORT` once, but the diff
  does not repeat it at the hunk.
- **`overcommitter` in `commitment.yaml`:** limits go from 14 cores and 6Gi to
  10m and 20Mi. The findings table showed `16Mi→16Mi`, because it displays
  requests, so the largest change in the PR was never visible in the report. Read
  `-o wide` before you trust a diff.
- **`cpu-hog`:** 115m is a guess. The container was pinned at its 100m limit the
  whole time, so its real demand was never observed, and any number above 100m
  is a step chosen without evidence. tare does say this for memory after an
  OOMKill (`16Mi→?`), but not yet for CPU.
- Also worth discussing: the `commitment.yaml` container with no `resources` at
  all gains a full block, which moves it out of BestEffort. The PR body says so;
  the diff alone does not.

These are known open issues, not mistakes in your setup.

</details>

### Discussion (10 minutes)

- Which of our clusters could run this today? Run `kubectl tare check` there
  (read-only) and see what is missing.
- Which finding would you act on without asking anyone, and which would you
  never apply without the owner?
- What would make a pull request from tare mergeable in your team?

To report a number that looks wrong, include `kubectl tare version` and the
output of the same run with `--redact`.

## Development

```sh
go test ./...              # full suite
go test -short ./...       # skips the threshold sweep
```

The analyzer is tested against a workload simulator in `internal/simulate`,
which models eighteen workload shapes whose correct size is known in advance and runs
thirty simulated days in about 27ms. Recommendations are scored by replay: apply
the tool's own output as a hard limit, run the window again, and require the
workload survives.

`internal/ceilings` is measured against a corpus of 135 real container specs
extracted from 39 public Helm charts. See `test/corpus/README.md`.

Design decisions and their reasoning are in `docs/plan.md`.

## License

Apache-2.0.
