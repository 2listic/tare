# Known failures

Every serious defect found in tare so far, in the order they were found.

This file exists because tare's claim is not that it is clever. It is that it
will not confidently tell you something false. That claim is worth exactly as
much as the evidence behind it, and the evidence is this list: fifty-three times
this tool produced an answer it had no business producing, almost always a clean
and plausible one, and fifty-three times it was caught and written down.

Three of them surfaced as an error: a run that timed out on a cluster larger than
the lab (25), a denied HPA read that aborted (26), and a panic on an empty
`resources: {}` (36). The other fifty did not surface at all, which is the
whole point.

## The shape

They are all the same defect wearing different clothes: **a measurement that
returns a reassuring, meaningless number rather than failing.** A missing metric
read as a comfortable zero. A query that matched nothing. A ratchet applied to
the wrong resource. A flag declared and never assigned. In each case the report
looked normal, the numbers were plausible, and a user acting on them would have
been acting on nothing.

If you are evaluating this tool, the useful question is not whether it has bugs.
It is whether its bugs are the kind that announce themselves. These were not, so
assume the next one is not either.

---

## 1. CFS throttle metrics were declared absent

**Looked like:** a fresh test cluster exported no
`container_cpu_cfs_throttled_periods_total` at all. The conclusion written down
was "k3s does not export throttle metrics."

**Was actually:** cAdvisor exports them only for containers that have a CPU
quota. No workload on that cluster had a CPU limit. On a cluster where some do,
they exist — for 3 of 23 containers, against PSI's 23 of 23.

**Caught by:** re-measuring with a workload that had a limit.

**Consequence if shipped:** throttling would have been dropped from the design
as unavailable, on a sample of zero.

## 2. A 5-minute rate window averaged away a JVM's startup burst

**Looked like:** a 10m CPU limit recommendation for Tomcat.

**Was actually:** the inner `rate()` window was 5 minutes, and Tomcat's startup
burst lasts about 20 seconds. Averaged over five minutes it disappears. A Tomcat
draws **19.7x** more CPU booting than running; the recommendation would have
throttled every restart into a crash loop.

**Caught by:** disbelieving the number.

**Found a second time** in the PSI peak query, which had the identical defect.

## 3. A threshold sweep that measured nothing

**Looked like:** five of nine thresholds reported as having no effect on any
outcome, which read as "these knobs do not matter."

**Was actually:** four independent harness faults — no scenario was ever
starved, no archetype sat near a decision boundary, only replay safety was
scored, and averaging across archetypes hid single-archetype effects.

**Caught by:** asking what the sweep would look like if it were broken, and
finding that it would look exactly like that.

## 4. `-o patch` output was rejected by every workload

**Looked like:** correct YAML.

**Was actually:** a partial manifest for `kubectl apply`, which three-way-merges
a fragment as the *complete* desired state and nulls out the selector and image.
Server-side dry run rejected all three test workloads.

**Fixed by:** emitting `kubectl patch --type=strategic` commands instead, which
touch only the named fields. Verified against a live API server.

## 5. The documented invocation silently ignored its own flags

**Looked like:** `kubectl tare report -n prod` ran and printed a report.

**Was actually:** Go's `flag.Parse` stops at the first non-flag argument, so
every flag after `report` was discarded and the tool reported on the whole
cluster with defaults. This was the invocation in the README.

## 6. The long-run load generator sent almost no load

**Looked like:** a healthy 14-day stability experiment collecting rows on
schedule.

**Was actually:** a busybox `wget` loop that burned 5.5x more CPU than the nginx
it was targeting. The target sat at 1% of its request and its recommendation
clamped to the CPU floor. A floored recommendation is constant by construction,
so the experiment's question — "is the recommendation stable over 14 days?" —
would have answered *yes* without the analyzer ever reading real data.

**Rule learned:** measure the load generator against its target before trusting
any load-based measurement.

## 7. Both lab cronjobs sat in ImagePullBackOff for four hours

**Looked like:** `history.csv` filling normally.

**Was actually:** `bitnami/kubectl:1.33` had started returning 404 after Bitnami
retired their Docker Hub catalog. The workload that was supposed to redeploy
never redeployed, and the limit toggle — the only producer of the limit-trend
signal — had never fired once.

**Rule learned:** for anything on a clock, exercise every time-dependent path by
hand and confirm it changed something observable *before* starting the clock.

## 8. Two signal flags were declared, never assigned, and never read

**Looked like:** `PSISeen` and `ThrottleSeen` on every record, serialised as
`false`, beside a `PSIWaiting` field carrying a real value.

**Was actually:** dead fields. Anyone filtering the long-run record on them
would have got zero rows out of a full dataset.

## 9. The saturation ratchet clamped the wrong resource

**Looked like:** a JVM using 65MiB of its 512Mi request, recommended at exactly
512Mi. A measurement.

**Was actually:** the input echoed back. When a verdict was `SATURATED`, all
four recommendations were clamped to at least their current values regardless of
*which* resource was starved. The container was CPU-starved on PSI, so its
memory recommendation froze at its own request. Symmetric in the other
direction: an OOMKill froze the CPU recommendation.

**Caught by:** noticing `recMem == memReq` exactly, for ten consecutive hourly
snapshots.

**Fixed by:** splitting the ratchet per resource, plus `reconcileFloors`, which
clears a `CPUFloored`/`MemFloored` flag whose value was later raised — those
flags were stale too, and the analysis script was labelling a live series as
pinned to the floor.

## 10. A 7-day window on a cluster holding 18 hours of data

**Looked like:** a normal report.

**Was actually:** tare's early-window queries carry `offset window-1h`. On a
young cluster that offset matches no series at all, so `MemEarly` and
`MemLimitEarly` were zero for every container in every snapshot. Leak detection
and limit-trend inference are both guarded on those being non-zero, so **both
were inert for an entire experiment** while every report looked ordinary. At a
12h window the leaking workload is detected immediately.

## 11. The lab's limit toggle replaced the pod it was measuring

**Looked like:** a workload whose memory limit changed on schedule, producing
the limit-trend signal.

**Was actually:** changing a limit on a Deployment rolls the pod, and tare keys
observations by pod name, so the replacement had no early observation and
`MemLimitEarly` was zero by construction. The workload existed solely to produce
that signal and could never have produced it.

**Fixed by:** resizing the pod in place through the `pods/resize` subresource.

**Worth knowing:** on any cluster without in-place resize, a memory limit change
always replaces the pod, so limit-trend ceiling inference may be unreachable in
production too.

## 12. Trend values took the last pod instead of the worst

**Looked like:** correct per-workload aggregation.

**Was actually:** `MemEarly` and `MemLate` were assigned rather than maxed inside
the loop over a workload's pods, unlike every other field three lines above. A
two-replica workload reported whichever replica came last, so a leaking replica
beside a freshly-rolled one reported `OK`.

**Caught by:** reverting the fix and watching the new regression test go red. It
would otherwise have passed for free.

## 13. Saturation was decided on peak pressure, against a threshold documented as sustained

**Looked like:** a container correctly identified as starved.

**Was actually:** `max_over_time` over the window, so a single stalled minute
marked a container `SATURATED` for as long as the window was wide — and a peak
over a widening window never decreases, so verdicts drift one way over a
fortnight. A JVM stalling 1.7% of minutes on GC while using 1.5% of its request
was called starved, and defect 9's ratchet then pinned its recommendation to its
own request.

**The obvious fix was wrong.** Switching the verdict to the window mean dropped
the simulator from 36/36 to 25/36, because a genuinely starved spiky workload
has a mean below the threshold too. No pressure statistic separates the two
cases. The discriminator is **utilisation**: a container cannot be starved of
CPU it never asked for. A new `MinSaturationUtil` restores 36/36.

**The part worth more than the fix:** the simulator scores 36/36 with that
threshold *disabled*, because every simulated stall comes from the container's
own demand meeting its own allocation — pressure and utilisation always move
together there. On real hardware they come apart. No amount of simulator work
would have found this.

## 14. CFS throttling had the identical peak defect

Reported at 5.9% throttled; sustained figure was 0.17%.

## 15. The report's pressure column showed only the peak

`psi 0.031` and `throttle 5.9%` read as current state. Now `psi 0.0005/0.031`,
mean and peak.

## 16. The new sustained signals were not gated on their own availability

**Was actually:** defect 8, re-introduced inside the fix for defect 13. A
cluster where the sustained queries failed would have printed `psi 0.0000/0.031`
— the most reassuring number on the row, for a value nobody measured.

**Caught by:** grepping for declared-but-never-read signals, because defect 8
had happened before.

## 17. Restart counts were the pod's lifetime total, not restarts in the window

`restartCount` is cumulative and resets only when the pod is replaced, so a
month-old pod reported every restart it had ever had against a 7-day window.

## 18. A trend signal was marked available when only half of it was measured

**Looked like:** a leaking container reported `WASTE`, confidence `medium`, with
a 674MiB recommendation, no caveats, and nothing listed as missing.

**Was actually:** each trend needs two queries, an early window and a late one.
Signal availability is marked per returned series, so the late query alone was
enough to mark `memory-trend` present while the early query matched nothing and
`MemEarly` was zero. Leak detection is guarded on `MemEarly > 0`, so it silently
never ran, and the "unbounded memory growth cannot be detected" caveat never
printed.

**Trigger:** the container's pod had been replaced mid-experiment, and the early
query's offset always points back before the replacement.

**Caught by:** a routine status check on a running experiment, 28 snapshots after
it started failing.

## 19. An OOMKill destroyed the evidence that it was a leak

**Looked like:** the lab's slow-leaker, which OOMKills about every 41 hours,
reported `SATURATED` with "memory must not be reduced" and a memory
recommendation 2.3x its request. Nothing was said about growth. Across 88
snapshots it was never once reported `LEAKING`.

**Was actually:** `MemEarly` and `MemLate` are keyed by (namespace, pod,
container), not by container id, so the pair spans every incarnation between
them. The OOMKill resets the working set, so late/early measures where the
window edges fell in the restart cycle rather than anything about the workload:
the same container, the same window, the same four kills reads as **4.1x growth
or 0.38x shrinkage** depending on the hour it is sampled. Below the 1.5x ratio
the LEAKING branch does not fire, control falls through to the OOMKilled branch,
and the operator is told to raise memory - the one action that never helps a
leak and buys it a few more hours.

**Caught by:** writing the sawtooth out as two observations that differ only in
sampling phase, and asserting the verdict was the same. It was not.

**Fixed by:** withholding the trend **one-sidedly**. A ratio above the threshold
still fires LEAKING, because a restart cannot manufacture that much growth and
the cost of believing it is a withheld recommendation. A ratio below it is
discarded when the container was OOMKilled, because that is the direction that
reads as a clean bill of health on the one verdict an operator acts on by adding
memory. The report then says the restarts reset the working set and that growth
cannot be ruled out.

**The first attempt at the fix was worse than the bug.** Suppressing the trend
on *any* restart is symmetric, tidy, and silenced the simulator's leaky-service
- the one workload the leak test exists for, which OOMKills once and then reads
25x. `p95/max` looked like a way to tell a sawtooth from a container pinned at
its limit, and is not: leaky-service sits at 0.918 and the honest
variable-mem-jvm at 0.605. The asymmetry is the whole fix.

**Consequence if shipped:** every cyclically-leaking container in the fleet
reported as merely undersized, with a confident larger number attached.

## 20. A right-sizer that silently added limits nobody asked for

**Looked like:** `no-limits-api`, a workload declaring requests and no limits —
the shape most charts ship and most style guides recommend — reported `WASTE`
with `CPU lim unset>3156m` and `MEM lim unset>451Mi`. Two evidence lines, both
about usage. Nothing about the limits.

**Was actually:** proposing a limit where the manifest sets none is a
behavioural change, not a resize. A CPU limit throttles at quota even when the
node is idle, which is precisely why the omission is usually deliberate. A
memory limit is worse in kind: the container becomes OOMKillable at that cap
rather than evicted under node pressure, so the failure mode changes from
"rescheduled elsewhere" to "killed in place". The reader is looking at a report
about reclaiming waste and is not watching that direction.

**Caught by:** giving the simulator a *declaration* shape axis and looking at
what each shape was told. The `Guaranteed` row two lines below in the same
report already refuses to split requests from limits and says why, which is what
made the silence on this row visible.

**Fixed by:** stating it. The numbers are kept — a reader who wants limits
should have them — and each carries what applying it changes.

**Why neither instrument could see it:** every simulated container was rendered
Burstable with both requests and limits set, because that is what the allocation
model expresses, and the simulator never populated `Authored` at all — so every
finding derived from the *manifest* rather than the running pod was unreachable
from the catalogue, including the one that says a pod is BestEffort. The lab's
own workloads all declare both. This is the same root cause as the `IDLE`
defect the live cluster found: **the population of behaviours is not the
population of declarations**, and only the second finds verdict bugs.

---

## 21. A DaemonSet's reservation was multiplied by the size of the cluster

**Looked like:** nothing at all. The node-count delta on the lab cluster was
checked by hand against the real allocatable capacity and agreed, because on
that cluster the answer is decided by pinning rather than by capacity.

**Was actually:** the replay charged every DaemonSet **pod** to every node.
One DaemonSet on a three-node cluster was charged three times to each of the
three nodes; on a thirty-node cluster, thirty times. A node-exporter asking for
100m reserved 3 cores of imaginary overhead per node at that size, and the
replay concluded the cluster had no room to consolidate. The same loop charged
a DaemonSet with a `nodeSelector` to nodes it can never be scheduled onto, so a
GPU device plugin asking for 5 cores made every CPU node in the fleet look
nearly full.

**Caught by:** the third of the live observation that the declaration shapes
could not carry. `nodeSelector` changes packing rather than verdicts, so it
needed a packing test rather than an analyzer one, and writing the first such
test meant writing down what a DaemonSet costs a node - which is one copy, on
the nodes it selects.

**Fixed by:** charging per DaemonSet rather than per DaemonSet pod, taking the
largest pod of the set, and charging only the nodes the set runs on: the union
of what its `nodeSelector` matches and where its pods are observed. The union
matters - a node whose labels were not read matches no selector at all, and
trusting the selector alone would hand out capacity an agent is demonstrably
occupying, which is the expensive direction.

**Why it stayed invisible:** the error is *pessimistic*. It understates free
capacity, so it can only ever shrink the reported saving, and a saving that
comes out too small reads as the caution this tool advertises. The existing
DaemonSet test passed throughout - its numbers happened to give the same node
count under both models, which is defect 3 wearing different clothes: a check
that comes back green while measuring nothing.

---

## 22. The replay would not pack onto the nodes it had already conceded

**Looked like:** a plausible lower bound. Pinned pods hold their node, and the
report says so with counts.

**Was actually:** first-fit-decreasing walked the nodes largest-free-first, so a
node held by a pinned pod - a node that can never be freed, and whose remaining
space is therefore the only space in the cluster that costs nothing to use - was
tried last. The replay would keep a second node alive rather than put a movable
pod in the room it had already paid for. The pin list is deliberately broad, so
on a real cluster this is not an edge case: the k3s lab pins six pods by
`nodeSelector` alone.

**Caught by:** writing the lab's own shape as a test - two infrastructure pods
pinned to the server node, ordinary workloads on the agents - and getting two
nodes back where one was demonstrably enough.

**Fixed by:** ordering the bins with the already-conceded nodes first. Both
orderings produce a packing that fits, so this is a stronger claim rather than a
looser one: the number is still one the model can demonstrate, and it is now the
smaller of the two it could demonstrate.

---

## 23. A dead node was still a live destination, and its phantom pods were cargo

**Looked like:** `nodes: 3 running, 3 needed for today's requests, 3 needed
after these recommendations`, with a per-node commitment table giving CPU and
memory percentages for all three, and 53 pods in the replay.

**Was actually:** two of the three nodes had been unreachable since 08:03 UTC
and the report was produced at 09:25. The API server keeps every pod object a
vanished kubelet last owned, so `kubectl get pods` - and tare - went on listing
seventeen pods as `Running` on machines that had stopped answering an hour and a
half earlier. tare read `Schedulable` as `!spec.unschedulable` and nothing else,
so a dead node was a valid destination with full allocatable capacity, and the
phantom pods were movable cargo to be folded onto it or off it.

**Caught by:** applying `shapes.yaml` to the lab cluster and noticing that the
new DaemonSet reported `DESIRED 1` on a three-node cluster, and that all four
new pods had landed on the same node.

**Fixed by:** reading the Ready condition. A node that is not Ready is not a
destination and is named in the report with the reason; every pod on one is held
where it is, with a pin reason that says the pod may not be running at all. The
same run now says `18 ... its node has stopped reporting` and names both nodes.

**The detail that makes this the usual shape:** when a kubelet stops reporting,
the Ready condition goes to **Unknown**, not False. Every natural way to write
the check - `!= False`, or a bool that defaults to false only on an explicit
False - reads Unknown as healthy. `isReady` therefore requires an explicit True,
and the test asserts all three values plus a node reporting no conditions at all.

---

## 24. Node taints were collected from the first version and read by nothing

**Looked like:** a replay that moved pods onto the largest node available.

**Was actually:** `discover.Node.Taints` was populated on every node and used by
no code path in the repository. The pod side of the same rule was implemented -
a pod that tolerates a taint is pinned, because it may sit where others cannot
follow - which is what made the node side look handled. It was not: a node
carrying `dedicated=gpu:NoSchedule` was offered as free space to pods that
tolerate nothing, and since bins are tried largest-first, a big tainted node was
tried **first**. The promised consolidation is one the scheduler refuses
outright.

**Caught by:** looking for what else the node inventory knows and does not use,
immediately after defect 23 showed that one such field had been costing
correctness.

**Fixed by:** `Node.BlockingTaint`, next to the toleration logic it mirrors. A
NoSchedule or NoExecute taint that is not one of the kubelet's own
not-ready/unreachable taints makes the node a non-destination, named in the
report. PreferNoSchedule is a preference and does not.

---

## 25. Every pod cost an API call, and the lab cluster was too small to notice

**Looked like:** nothing. Every run on the k3d lab cluster finished in seconds.

**Was actually:** `ownerOf` fetched a ReplicaSet per pod to find the Deployment
above it, uncached, and `Pods` did it a second time for the packing replay.
client-go's default client-side rate limiter is 5 requests per second, and the
run was capped at three minutes. A cluster of 900 pods therefore spent its
entire budget inside the client's own rate limiter and died with `context
deadline exceeded` before it ever queried Prometheus - and 900 pods is a small
cluster. The lab has thirty, which is why thirty runs over nine days all passed.

**Caught by:** asking what the tool would do on a cluster nobody here owns,
rather than on the one it was written against.

**Fixed by:** `workloadIndex`, which lists each controller kind once per
namespace scope and is shared between the sizing pass and the replay, plus
QPS/Burst raised to 50/100 on the rest config. API calls no longer scale with
pod count at all, and a test asserts it: ten pods and a thousand pods must cost
the same number of calls.

---

## 26. Three denied reads, handled three different ways, none of them right

**Looked like:** a report, or no report, depending on which read was denied.

**Was actually:** a denied HPA list aborted the whole run with a Kubernetes
error. A denied PDB list was skipped silently, which overstates the node-count
saving, because a budget allowing no disruption is exactly what makes a node
undrainable. A denied VPA list was indistinguishable from the common case of the
CRD not being installed, so a cluster where a VPA was already writing these
fields read as a cluster with no VPA - two controllers writing one field, and
nothing said so. The first failure is loud and harmless; the other two are the
project's usual shape.

**Caught by:** running the tool against its own cluster with a token that could
list pods and nothing else, which is what a first evaluation inside a company
actually looks like.

**Fixed by:** all three degrade and all three say what was lost, in the report
header, in every output format. A VPA read refused with Forbidden is reported; a
missing CRD stays silent, because that one really is nothing.

---

## 27. A container with no measurements was not a container with no data

**Looked like:** a report covering a cluster where half the containers had no
metrics at all, with nothing in the header to say so.

**Was actually:** the observation map is keyed by container and filled by
fifteen queries, two of which come from kube-state-metrics and exist for every
container in the cluster whether or not anything ever scraped its cgroup. A
container whose kubelet had stopped reporting therefore arrived with an
observation attached and nothing in it. The per-container verdict was safe - a
zero sample count forces `NO-DATA` - but nothing counted how many of those there
were, so a Prometheus scraping two namespaces out of forty produced a report
that looked complete and described a twentieth of the cluster.

**Caught by:** `kubectl tare check` disagreeing with `kubectl tare report` about
how much of the same cluster was covered, on the first cluster both were run
against.

**Fixed by:** a coverage line in the header of every format, counting containers
with zero memory samples rather than containers with no map entry, and the same
measurement in `check`. On the lab cluster it immediately read "metrics cover 50
of 66 containers", which was true and had never been said.

---

## 28. A mistyped namespace was reported as a cluster with no metrics

**Looked like:** `kubectl tare report -n prod-1` on a cluster whose namespace is
`prod1`, printing `0 containers`, `nodes: 0 running`, and nine lines of
`signals unavailable on this cluster: cpu-usage: not exported by this cluster`.
Exit code 0.

**Was actually:** listing pods in a namespace that was never created returns an
empty list rather than an error, so `discover.Containers` came back with nothing
and every downstream signal probe matched nothing. The report then attributed
the emptiness to the cluster. Sixty seconds earlier the same binary against the
same cluster had reported all nine signals present and three nodes, so the
output was not merely useless: it told the operator to go and fix a metrics
pipeline that was working.

**Caught by:** a UX review running the documented surfaces against the lab
cluster, typing a namespace that did not exist.

**Fixed by:** `discover.MissingNamespaces`, called before any measurement is
taken, which gets each requested namespace and errors naming the ones that do
not exist. A Get that fails for any other reason is **not** treated as missing,
because the common case is a namespace-scoped user who may not read cluster
resources, and refusing them a report is worse than the typo this guards
against. `-n "a, b"` is trimmed, so the space a person types does not become a
namespace that does not exist.

---

## 29. A misspelled --verdict filtered everything away and exited 0

**Looked like:** `kubectl tare report --verdict SATURATD` printing a column
header, no rows, and exit 0 - byte-identical to a cluster with nothing
saturated on it.

**Was actually:** `filterVerdicts` built its keep-set from whatever strings the
user typed and compared them against the findings. A name matching no verdict
matched no finding. The one output an operator reads as "you are fine" was also
the output for "you spelled it wrong", and nothing distinguished them.

**Caught by:** the same review, in the same session as 28.

**Fixed by:** `checkVerdicts`, run before any cluster call so a typo costs
nothing and `--demo` is held to the same spelling. The accepted set is
`analyze.ReportOrder()` rather than a list written at the call site, so a
verdict added later cannot become unselectable - the same reason that function
exists at all. The `--verdict` help text was also advertising eight verdicts
when there are nine; `NO-DATA` was missing.

---

## 30. `-o tui` against a real cluster printed the table

**Looked like:** a working interactive browser, because every time anyone
opened it, it was with `--demo`.

**Was actually:** the live-cluster path sent `patch`, `json` and `html` to
`emit` by name and let everything else fall through to `emit(rep, "table")`.
`-o tui` on a real cluster printed the table and exited 0. The demo path called
`emit` with the requested format, so the only place anyone looked, it worked.

**Caught by:** adding `-o wide` on 2026-09-23. The new format would have fallen
through the same way.

**Fixed by:** every run goes to `emit` with the requested format, except a
`diff` with the default format. Same lesson as 5: a code path exercised only in
demo mode is not exercised.

## 31. The nodes section named the cluster's own slack as tare's saving

**Looked like:** `nodes.from_resizing  2  ip-10-0-3-41, ip-10-0-5-12`, a count
with the nodes that make it up.

**Was actually:** the list was every node empty after packing the
recommendations, including the ones repacking today's requests already empties.
On the consolidation test fixture it printed `nodes.from_resizing  0  n2`: zero
nodes credited to tare, one node named as tare's. `--explain` and the HTML
report said the same through `nodeLines`. The counts were tested and correct;
nothing tested the names beside them, and the one test that did
(`TestResizingFreesANode`) had zero consolidation, so the two lists coincided.

**Caught by:** a review from someone who had never seen the tool, asking what
the section meant (2026-09-30). The confusion was the signal.

**Fixed by:** `packing.Result.EmptyToday`, and a nodes section with one row
per replay stage whose names are what is empty at that stage, not what that
stage freed. The two passes are packed independently, so the sets do not
subtract and the report no longer pretends they do.

## 32. IDLE was summed into the headline its own evidence said not to act on

**Looked like:** `reclaim 20.2 cores` on the demo.

**Was actually:** 1990m of it was `dormant-sidecar`, whose row says
`IDLE_NOT_RULED_OUT`. `--explain` stated that IDLE "is not folded into
reclaimable.*"; `totalSavings` folded it in, and the node replay applied the
same IDLE recommendations. The sentence and the sum disagreed and nothing
compared them.

**Caught by:** the same review, while reconciling the headline with the rows.

**Fixed by:** IDLE counted apart (`idle` row in `totals`, `N IDLE not counted`
in the summary) and left at its current requests in the replay.

**Also found:** the README's example output could not have come from the tool.
Its totals said 4 containers beside a summary of 35, and `1.2 cores` for rows
that sum to 12.6. Hand-written example output is a claim no test checks; the
current example is reconciled against its own findings table.

## 33. An OOMKilled container's memory printed as if it were the answer

**Looked like:** `SATURATED  checkout  16Mi  OOMKILLED`, memory unchanged.

**Was actually:** a ratchet, not a measurement. Usage above the limit that
killed a container is never observed, and a 15-30s scrape usually misses the
climb to it, so the peak × 1.15 almost never exceeds the request and
`max(peak, request)` returns the request. The row read as "16Mi is right" for
the one container known to be too small. Under it, the limit was set to
`max(request × 1.25, limit)`: a 25% bump nothing measured, shipped by
`-o patch`. For a Guaranteed pod the QoS hold then pulled the request up to the
bumped limit, so the "held" memory was in fact raised 25%. Same shape as 9: the
input echoed back as a recommendation.

**Caught by:** the 2026-09-30 review, asking why the crashed container was the
only one with no memory recommendation.

**Fixed by:** `Finding.MemCensored` and `MEM_DEMAND_CENSORED`. Request and
limit hold their current values so every numeric consumer leaves memory alone;
every renderer prints `?`; `-o patch` and `diff` omit the memory fields; the
totals count it as `unknown`. A peak observed above the request is still a
measured raise and is not censored.

**Still open:** the threshold sweep is byte-identical with and without the fix.
No simulator archetype reaches this path, because the simulator observes the
working set at the limit when it kills, which a real scrape does not. Like
`PlateauHeadroom`, the case is pinned by unit tests only.

## 34. A ratcheted CPU request above the limit it shipped with

**Looked like:** `SATURATED  1000m`, a CPU increase for a starved container.

**Was actually:** a patch the API server rejects. The ratchet raised the
request to its current value, `max(rec, request)`, and held the limit at
`max(rec, limit)`. With no current limit that is `max(rec, 0)`, so the
usage-sized limit (875m) stayed below the ratcheted 1000m request. The
restart-churn hold had the same line. Defect 9 split the ratchet by resource;
nobody checked the two halves of one resource against each other.

**Caught by:** the 2026-10-08 pre-release review, constructing a starved
container with a request and no limit.

**Fixed by:** both holds take `max(..., RecCPU)` for the limit.
`TestRatchetedCPURequestNeverExceedsItsLimit` covers both paths.

## 35. Two copies of "what tare will write" disagreed, and one wrote zeros

**Looked like:** `-o patch` and `diff --write`, two renderings of the same
findings.

**Was actually:** two rules. `analyze.Patchable`, whose own comment warned that
two copies would drift, accepted IDLE. The patch script kept its own copy, which
did not. So `diff --write` committed `4Gi -> 26Mi` for containers the report
said not to act on: defect 32's IDLE, now landing in git instead of in a total.
The script, separately, wrote `"cpu":"0"` for every CPU field it had no number
for. In a strategic merge patch that strips the request; the table showed the
same field as withheld.

**Caught by:** the same review, comparing the two paths field by field.

**Fixed by:** `-o patch` renders `gitops.Plan`'s changes, so there is one rule.
IDLE is out of `Patchable`, and a field without a number is omitted, not zeroed.

## 36. The manifest editor reported edits it had not made

**Looked like:** `edited deploy/api.yaml (2 container(s))`.

**Was actually:** each of these produced a wrong edit or a false "applied":

- `resources: {}`, which most chart scaffolds render, panicked on
  `res.Content[0]` partway through the tree. Earlier files were already
  rewritten, with non-atomic writes. A bare `resources:` and an alias did the
  same.
- A section holding `memory` but not `cpu` dropped the CPU change and still
  counted the container as applied.
- A manifest with no namespace (a Kustomize base) matched every namespace. The
  first namespace's numbers won, the second's splice found the old text gone,
  and both were reported as applied.
- An edit through a YAML anchor changed every container that aliased it. A
  container built from `<<: *base` had a block added that overrode what it
  inherited.
- CRLF files lost the `\r` on each edited line.
- yaml.v3 columns count characters and the splice sliced bytes, so an `é`
  earlier on the line wrote `cpu:500m`. That still parses, as a bare key, so the
  parse check passed it.
- Kustomize patch files were named by namespace and name only, so
  Deployment/web and StatefulSet/web overwrote each other. A container named
  `123` was emitted as the integer 123.
- Writes followed symlinks out of the repository.

**Caught by:** the same review, which fed the editor each shape a real repository
contains.

**Fixed by:** `spliceContainer` either edits a shape correctly or refuses it with
a reason. The reason reaches the skipped list, and no longer reads "may be
Helm". It covers empty values, missing keys inside an existing section, a
namespace-less file with conflicting namespaces, anchors, merge keys and flow
style. Line endings are preserved and columns are converted to bytes. A failed
splice un-applies its change. Inserts after one line are joined in order,
because applied one at a time a requests key landed under limits. Writes go
through a temp file and a rename, and symlinks are refused. Each case has a
test in `hazards_test.go`, checked against the pre-fix code: all ten failed
there. The first run of that control passed everything, because it had copied
the fixed working tree instead of HEAD.

## 37. `-n prod check` ran a report and exited 0

**Looked like:** a CI gate that passed.

**Was actually:** defect 5 again, one position later. The fix for 5 split off a
subcommand only when it came first, so a flag before the subcommand stopped Go's
parser at `check`, and the full report ran instead. Leftover arguments were
never inspected, so `report foo` and `-n a b` ran on the wrong scope. Several
flag combinations were also accepted and then ignored:

- `diff -o json --write` skipped the diff and exited 0.
- `check --demo` printed the demo.
- `--write` without `diff` wrote nothing.
- A misspelled `-o` failed only after the whole cluster run.

The test for 5 passed throughout, because it checked a copy of `run()`'s logic,
not `run()`.

**Caught by:** the same review.

**Fixed by:** `parseArgs` parses flags on either side of the subcommand and
rejects anything left over. `checkCombination` rejects ignored combinations
before any cluster call. The tests call the real functions.

**Also found:** `G` then Enter on an empty TUI view indexed `vis[-1]` and
panicked.

## 38. A limit that was throttling the container was cut by 70%

**Looked like:** `WASTE  100m→30m`, a Guaranteed cache using a quarter of its
CPU.

**Was actually:** the 100m limit throttled it in 82% of scheduling periods.
Its own `LIMIT_NOT_NODE` line, in the same finding, said the limit was the
constraint. The limit was sized from peak usage, which is a rate averaged over
seconds, while CFS spends the quota every 100ms. A container that bursts past
its quota inside each period is throttled while its averaged peak sits far below
the limit. metrics-app was the same, 600m cut to 165m at 63% throttled. The
saturation path never saw either one, because their pressure was low against
their allocation and read as node contention. This is defect 2 at a shorter
timescale.

**Caught by:** running the post-review build on the lab cluster on 2026-10-11 and
reading the patch script's comments against its numbers.

**Fixed by:** a CPU limit that throttled the container above `ThrottleRatio`
is never lowered. The finding says so under `CPU_LIMIT_HELD_THROTTLED`, and the
request is still sized from usage. On the live cluster the rule also held
tomcat-lowxmx (1000m, which had been cut to 54m) and resize-target (500m to 40m).
Both were throttled at boot, so the startup limit sized from averaged boot
usage had the same flaw. `TestThrottledLimitIsNeverLowered` failed on the
pre-fix code (100m to 31m). `TestUnthrottledLimitIsStillSized` is the control.

**Cost:** fewer limit reductions. Nothing measured says how far a throttled
limit could safely go down, so tare no longer offers one.

## 39. A model's weight figure was decided by map order

**Looked like:** a VRAM fit check for `--quantization gptq-int8`.

**Was actually:** a coin toss. `precisionOf` looked the flag up by ranging over
a map of precision names and returned the first one the flag contained. That
flag contains both `gptq` (0.5 bytes per parameter) and `int8` (1.0), so the
weights figure, and with it `VRAM_FITS` or `VRAM_EXCEEDS_DEVICE`, could change
by a factor of two between two runs on the same cluster.

**Caught by:** the 2026-10-11 consolidation review, which was looking for map
iteration that reaches output. It found two more that only reordered findings
from run to run: the analyzer's sort had no tie-break past the CPU request, and
the simulator listed its inventory from a map.

**Fixed by:** the widest matching precision. Overstating the weights refuses a
fit; understating them passes one that cannot load. Findings are now sorted to
a total order, and demo output is byte-identical between runs.

## 40. `diff --write` wrote half of a change it reported as refused

**Looked like:** `SKIP api [app]: the manifest text did not match its parsed
structure, so the edit was not made`.

**Was actually:** part of the edit was made. A change is several splices, one
for each value it touches. Each splice was checked against the text only as it
was applied. One that failed marked the change refused, while the splices
already applied stayed in the file. The PR body said the container was left
alone. The diff showed its CPU request changed and its memory not.

**Caught by:** the 2026-10-11 consolidation review.

**Fixed by:** every splice is checked before any is applied, and a change goes
in whole or not at all (`applyWhole`). The first version of that fix had its
own defect, which the gitops harness caught before it shipped. When two changes
targeted one container, both were now checked against the original text and
both passed, so the second overwrote part of the first. The result was
`memory: 16MiMi`, which still parses. The old order had refused the second change
only by accident, and still wrote two `resources:` blocks into a sidecar.
Two changes for one container are now both refused with a reason, and two
splices over the same bytes refuse both their changes.
`TestFailedSpliceUnappliesItsWholeChange`,
`TestTwoChangesForOneContainerAreBothRefused` and
`TestOverlappingSplicesRefuseBothOwners` each fail when the fix is reverted.

## 41. A CPU-starved container that kept restarting was told to cut memory

**Looked like:** `SATURATED`, with a CPU increase and a memory decrease.

**Was actually:** a container with 6 restarts in the window. The restart-churn
hold refuses to reduce a container that keeps dying, because its samples
describe a container that never ran at that size. But the hold only ran when
nothing was starved. Starvation holds only the starved resource, so a
CPU-throttled container skipped the churn hold entirely, and its memory was
sized from a dying container's samples. In the fuzz population, 4GiB was cut to
384MiB.

**Caught by:** the 2026-10-11 consolidation review.

**Fixed by:** churn now holds whichever resources starvation did not, and the
verdict stays `SATURATED`. 567 of the 4,501 findings in the regression snapshot
gained `RESTART_CHURN`, and 70 of those no longer get a memory cut. An
OOMKilled container that keeps restarting now also keeps its CPU. `TestRestartChurnHoldsWhatStarvationDoesNot`.

**Cost:** an OOM-looping container no longer gets a CPU reduction. Its CPU
samples come from the same dying container, so that reduction was never
measured either.

## 42. A Guaranteed pod's held request was never checked against the node

**Looked like:** a memory increase with no `UNSCHEDULABLE_MEM`.

**Was actually:** a request no node could hold. The Guaranteed QoS hold raises
the request to the limit, and it ran last, after the node-size check had
already passed the lower value. The HPA notes and the authored-limit notes ran
before it too, so they quoted numbers that were no longer the recommendation.

**Caught by:** the 2026-10-11 consolidation review.

**Fixed by:** `holdGuaranteed` runs straight after `decide`, before anything
reads the numbers. `TestGuaranteedHoldIsCheckedAgainstTheLargestNode`.

## 43. The floor flags described numbers that were gone

**Looked like:** `"CPUFloored": true` in JSON output, which says the number is
a constant rather than a measurement.

**Was actually:** describing a value that had since changed. The flags were
reconciled inside `decide`, so they missed everything that moved the numbers
afterwards. The Guaranteed hold raised a floored 10m request to 14m, and a VPA
conflict zeroed it, and in both cases the flag stayed set.

**Caught by:** the 2026-10-11 consolidation review.

**Fixed by:** the flags are reconciled once, after the last step that moves a
number, and stay set only while the number is the floor.
`TestFloorFlagsFollowTheFinalNumbers`.

## 44. "Raise the limit" on a container starved only of memory

**Looked like:** `CPU_PINNED_AT_LIMIT: usage is pinned at the 500m CPU limit;
raise the limit, not just the request`.

**Was actually:** printed beside a recommendation that raised nothing on CPU.
The container was OOMKilled, and its CPU showed no stall. The note was gated
only on CPU p95 being near the limit, not on CPU starvation.

**Caught by:** the 2026-10-11 consolidation review.

**Fixed by:** the note now requires CPU starvation.
`TestPinnedAtLimitNeedsCPUStarvation`.

## 45. One refused namespace stopped the VPA scan

**Looked like:** recommendations for workloads in namespaces `b` and `c`, with
one warning about VPAs in namespace `a`.

**Was actually:** namespaces `b` and `c` were never scanned. The scan used
`break` on any error, which is right when the VPA CRD is missing because every
namespace fails the same way. RBAC is per namespace, though, so a refusal in one
namespace says nothing about the next. A VPA in Auto mode in a later namespace
went unseen, and tare recommended values for a field that controller was
already writing. Errors other than Forbidden were also dropped silently.

**Caught by:** the 2026-10-11 consolidation review.

**Fixed by:** a missing CRD still stops the scan, silently. Any other error is
reported and the scan continues. `TestVPAScanContinuesPastARefusedNamespace`,
`TestMissingVPACRDIsSilent`.

## 46. The commitment table counted pods, not containers, as unlimited

**Looked like:** `UNLIMITED_CPU_PODS=0` on a node full of pods with sidecars.

**Was actually:** a count of pods whose summed limit was zero, so one limited
container hid every unlimited container beside it. The type's own comment said
it counted containers.

**Caught by:** the 2026-10-11 consolidation review.

**Fixed by:** `podFrom` counts the unlimited containers in each pod, and the
labels are now `UNLIMITED_CPU_CONTAINERS` / `UNLIMITED_MEM_CONTAINERS`.
`TestUnlimitedContainersAreCountedPerContainer`,
`TestUnlimitedSidecarBesideALimitedContainerIsCounted`.

## 47. Four smaller ones from the same review

- **`-o patch` and `diff --write` spelled one value two ways**, `1024Mi` against
  `1Gi` and `1000m` against `1`, which reads as two different recommendations.
  `--write` also rendered zero bytes as `1Mi`. Both now use
  `gitops.CPUQuantity` / `gitops.MemQuantity`.
- **`OLLAMA_MAX_LOADED_MODELS` was filed under the keep-alive kind**, so a
  lookup by kind could return the wrong setting. The test asserted the wrong
  kind, which is how it survived. It now has its own kind,
  `ollama-max-loaded-models`.
- **`AvailableWindow` probed the default 14-day window twice** before stepping
  down. The probe ladder now skips a step equal to the requested window.
- **`diff --repo /typo` was rejected only after the whole run**, minutes of
  Prometheus queries later. The target is now checked before any cluster call.

## 48. `--redact` sent PDB names, namespaces and the HPA name in the clear

**Looked like:** a redacted report, every finding hashed, `names redacted` in
the header.

**Was actually:** leaking. `node_pins` (and `PinReasons` in JSON) printed
`PodDisruptionBudget tare-lab/overcommitter-pdb` unhashed, the drain blockers
hashed the namespace and kept the PDB name, and JSON carried `"HPAName":
"frontend"`. The header also hashed the Prometheus namespace twice, so
`source` named `4b0c96d8` while the `using` line on stderr named `14a2326b`
for the same endpoint. The test enumerated the fields it checked, so every
field nobody listed passed.

**Caught by:** the 2026-10-24 pre-release run on the lab cluster, grepping every redacted
output for every pod, node, container and image name in the cluster.

**Fixed by:** PDB names read out of the sentences that carry them, HPA and VPA
names and pin reasons redacted, the source hashed once.
`TestRedactionLeavesNoNameAnywhereInTheReport` checks every field of the
report rather than a list, and fails four ways with the fix reverted.

## 49. Node contention and "not the node" on the same row

**Looked like:** `NODE_CONTENTION: ... this is contention on the node rather
than a shortage of its own` on guaranteed-cache, metrics-app and resize-target.

**Was actually:** contradicted two lines down by `LIMIT_NOT_NODE: the
constraint is this container's own limit, not the machine`, on a node 8% busy.
Contention was decided from 5-minute averaged usage against the limit; the
container was throttled in 89% of periods by bursts the average hides. The
stall was its own quota. `throttled_limit_test.go` had documented this very
container "reading as node contention" without anyone testing the reason.

**Caught by:** the 2026-10-24 pre-release run on the lab cluster.

**Fixed by:** pressure on a throttle-stalled container is not attributed to the
node. `TestThrottledPressureIsNotBlamedOnTheNode`, both directions.

## 50. A container minutes old was sized, patched and put in a pull request

**Looked like:** `WASTE low boutique/frontend 100m→10m`, and the same number in
`-o patch` and `diff --write`.

**Was actually:** 14 samples, three and a half minutes of a pod still warming
up: p95 0m while `kubectl top` showed 29-51m. `low` confidence was the only
caveat, and nothing that acts on a row reads confidence. `diff --write` wrote a
10m CPU *limit* into eight Go and Node services of Google's Online Boutique.

**Caught by:** deploying a third-party application on the lab cluster and running tare at
once, which is what a first-time user does.

**Fixed by:** under `MinSamplesForSizing` (240, an hour at a 15s scrape) a
reduction becomes `UNVERIFIED` with `TOO_FEW_SAMPLES`; starvation still
stands. `TestTooFewSamplesProposesNoReduction`.

## 51. UNVERIFIED reductions were summed into the headline and the node replay

**Looked like:** `requests lowered on 12 (-1540m CPU, -980Mi mem)`.

**Was actually:** twelve rows `-o patch` refused to write. Defect 31 for IDLE,
again: every row said the reduction could not be shown safe, the summary and
`nodes freed` counted it anyway. On a cluster with no PSI every row is
UNVERIFIED, so the headline would be made entirely of disowned reductions.

**Caught by:** the same run, comparing the summary with `-o patch`.

**Fixed by:** UNVERIFIED is counted apart, as IDLE is: an `unverified` row in
`totals`, `N UNVERIFIED not counted` in the summary, kept at current requests
in the replay. `TestUnverifiedIsNotSummedIntoTheReduction`,
`TestSizerKeepsUnverifiedWorkloadsAsTheyAre`.

## 52. A timeout was reported as an RBAC denial

**Looked like:** `warning SIGNAL_DENIED rbac: horizontal pod autoscalers ...
could not be read (context deadline exceeded)`.

**Was actually:** an expired `--timeout`, with full read access. Every failed
non-fatal read was labelled a denial, which sends the reader to fix
permissions they already have.

**Caught by:** `--timeout 1s` in the same run.

**Fixed by:** only an API-server Forbidden is `SIGNAL_DENIED rbac`; anything
else is `SIGNAL_QUERY_FAILED kube-api`. `TestOnlyAForbiddenReadIsCalledDenied`.

## 53. tare's own patch took down Online Boutique

**Looked like:** `WASTE medium` on eight Boutique services, an hour of steady
traffic behind each, every patch passing `--dry-run=server`. cartservice's CPU
limit went 300m→29m, recommendationservice 200m→42m, checkout and shipping
200m→10m.

**Was actually:** an outage. Applied at the same 100-user load it was sized
from, the shop failed **66% of requests** (baseline 0.03%), cartservice and
recommendationservice crash-looped on 1s liveness probes while throttled in
95-100% of periods, currencyservice restarted 6 times. The startup guard that
the README promises ("a limit sized to steady state throttles every
restart") never ran: the boots happened 66 minutes before the report, the
window was rounded down to 1h, and no boot fell inside it. `CPUStartupMax` was
zero, the steady half of the startup query had data so the signal counted as
measured, and the limit came from steady state alone. Every pod older than its
window is in this position, which in production means nearly all of them.

**Caught by:** the first closed-loop test against a live third-party
application, 2026-10-24 - apply `-o patch`, measure the application. Nothing
before it measured what happens *after* a recommendation is applied. tare's
next report did call the damage (`SATURATED`, `RESTART_CHURN`), after the fact.

**Fixed by (first):** a CPU limit is never lowered, or added, unless a boot of
that container was observed in the window (then `CPU_LIMIT_HELD_NO_BOOT`). The request is
still sized from usage. `TestUnobservedBootHoldsTheCPULimit`, three cases, fails
four ways with the fix reverted. For a Guaranteed pod holding the limit holds
the request too.

**Not fixed:** the CPU limit can still be sized down to the 10m floor when a
boot *was* observed. At 10m, checkout and shipping were throttled in 24-30% of
periods and failed readiness probes without restarting. A 1-minute rate cannot
see the sub-100ms bursts a CFS quota is enforced against, so no CPU limit tare
proposes below the current one is backed by a measurement. See Open.

**Second loop, with the fix (same day, 3h window):** the hold fired on three
services. Nothing restarted and the error rate stayed at baseline (0.02-0.03%),
so a check that looks only at errors and restarts passes this patch. Latency
did not. A separate probe pod measured 300 GETs to the product page:

| phase | p95 | p99 |
|---|---|---|
| original sizing, 100 users | 26ms | 54ms |
| tare's patch, 100 users | 113ms | 181ms |
| original sizing, 200 users | 49ms | 78ms |
| tare's patch, 200 users | 896ms | 1115ms |

Under tare's patch, checkout (10m limit) was throttled in 60-73% of periods,
recommendationservice (63m) in 66%, and shipping (10m) in 21-30%. The frontend
HPA went to 4 replicas because the request was now lower. tare's next report
marked six of the services `SATURATED`.

The patch also cut the load generator to 25m. That throttled it in 98% of
periods, so locust's own latency figures from that phase say nothing; the probe
figures above come from a pod outside the patch. A cost-only view would rate
this patch a success: a 4x slowdown at the load it was sized from, 18x at 2x
the load. That is the Open item below, measured.


**Fixed by (final):** a CPU limit is never lowered and never added, whether or
not a boot was seen: `CPU_LIMIT_HELD`, which replaces the boot-only hold. A
limit goes up only when a measured boot or the request needs more. Requests are
still sized from usage. `TestCPULimitIsNeverLowered` (four cases) and the
updated `TestLoweredLimitsCarryReasons` fail eight ways with the rule
disabled. A Guaranteed pod's request is held equal to its limit, so a
Guaranteed pod now gets no CPU reduction at all. That is the price of keeping
its QoS class without a measured limit.

**Third loop, with the final rule (2026-10-24, 6h window, load generator left
out of the patch):** requests were cut by 1.6 CPU and 1 GiB, and every CPU
limit was unchanged.

| phase | p95 | p99 |
|---|---|---|
| original sizing, 100 users | 29ms | 52ms |
| original sizing, 200 users | 22ms | 52ms |
| tare's patch, 100 users | 10ms | 29ms |
| tare's patch, 200 users | 14ms | 46ms |

Over the two 20-minute phases there were no restarts and no OOMs, and no
service was throttled above 1.4%. The frontend HPA went to 4 replicas at 200
users, where the control ran 3. At 58m a replica, that is still less CPU
reserved than 3 at 100m.

The 100-user phase's locust aggregate shows 0.27% failures and one 60s
request. That aggregate covers the rollout of eleven single-replica services,
and the 200-user phase ran at baseline (0.04%). The failures are probably
rollout disruption rather than sizing, but the locust log was gone before that
could be checked.
---

## 54. A fortnight's report built from the last hour

**Looked like:** `40 containers (21 listed) over 2w`, every row `low`, and
cpu-hog SATURATED from 318 samples.

**Was actually:** observations were joined to live pods only. A node of the
lab cluster went NotReady, its pods were rescheduled under new names, and
every one of those workloads was sized from the hour since. Prometheus held
about 10,000 samples of cpu-hog across four pods of the same ReplicaSet; tare
read the newest 318. Any rollout does the same, so a team that deploys daily
never gets more than a day of history, while the header names the window.

**Caught by:** the 2026-10-25 stress pass, comparing a row's sample count with
`count_over_time` by pod on the lab cluster's Prometheus.

**Fixed by:** `kube_pod_owner` and `kube_replicaset_owner` over the window
credit every pod, dead or alive, to its controller, and `aggregate` folds a
dead pod's worst case into its workload's row. Its memory-trend half is not
used: a dead pod has no late hour. Coverage is samples summed and divided by
how many pods ran at once (total span over union of spans), because both
obvious versions were wrong on the same cluster: a plain sum rated three
DaemonSet pods as three times the evidence, and a union of first-to-last
spans rated a pod that lived through a week of nightly power-offs as a full
week. Without kube-state-metrics the row is measured from live pods as before
and `pod-owner` is reported missing. Tests: `internal/analyze/history_test.go`,
`TestOwnersResolveThroughReplicaSets`.

On the lab cluster this moved cartservice, currencyservice and recommendationservice from
OK to UNVERIFIED: their replaced pods carried sustained pressure up to 0.12
and 19 restarts from when the cluster was crowded onto two nodes. That
evidence existed all along and was being dropped.

## 55. An hour-old pod's CPU wait decided a workload with a week behind it

**Looked like:** nothing on a cluster yet. Found reading the merge of 54 into
the CPU wait work, and confirmed by tests written to the behaviour wanted.

**Was actually:** a workload's CPU wait was merged field by field, each from
the worst pod, and the checks ran on the merged row. The stall came from the
most stalled pod and the history from the longest one, so a pod that lived an
hour, stalled at twice a normal made of that hour, made a workload SATURATED
on the strength of another pod's week. Alone it would have been withdrawn as
`CPU_WAIT_SHORT_HISTORY`. 54 made this common, since gone pods are often short
ones, but a replica added by a scale-up did the same while live. And a gone
pod the agent barely saw withdrew the whole row as `CPU_WAIT_LOW_COVERAGE`,
though the live pod beside it was seen well. Separately, the node-context
fields (`LongestConfirmedRun`, `NodeKnown`, `NodeContended`) were never
merged: they were the first pod's, so a confirmed starvation on any other
replica was lost.

**Fixed by:** a pod's stall counts only if it has the history for its own
normal; short pods decide only while no pod has one, and the row is then
withdrawn as short history as before. A gone pod's CPU wait is used only if it
would pass both checks alone. A live replica's low coverage still doubts the
workload, because that replica runs now. The node fields travel with the stall,
and the confirmed run takes the worst pod's. Tests:
`internal/analyze/cpuwait_history_test.go`. The catalog sweep
(`TestCPUWaitOnCatalog`) is unchanged at 28/54.

**And then, as first written, hid a starved replacement.** On a one-hour run
on a test cluster (2026-10-29, gates scaled to the hour, PSI hidden), a
Deployment was rolled out while a CPU hog filled its node: its new pod waited
1.18 s per second run for 22 minutes, node-confirmed, beside its predecessor's
quiet history. The fix above dropped the short pod's whole CPU wait, confirmed run
included, and called the workload OK where PSI and the code before it said
SATURATED. A confirmed run needs no normal (the node established its cause), so
the confirmed run is now taken from every pod, short or not. The
`CPU_WAIT_NODE_CONFIRMED` text gives the run's own share of the window when the
stalled share beside it is lower. `TestShortPodsConfirmedRunCounts`.

## 56. A limit it never reached was blamed, beside node contention

**Looked like:** `LIMIT_NOT_NODE: ... the constraint is this container's own
limit, not the machine` on a Guaranteed pod with a 300m limit, using 14% of it.

**Was actually:** throttled in 0% of periods, and `NODE_CONTENTION` on the same
row: 49 the other way round. `LIMIT_NOT_NODE` took CPU pressure as evidence
against the limit wherever throttling was below the threshold. Pressure is
meant to stand in for CFS counters that are missing (`TestPSIAloneIsEnough`,
as it was), but measured-and-zero was read the same as missing. Pressure says
the tasks waited, not why: a CPU hog beside it filled the node for a few minutes
of a window the node averaged 43% over, and the idle gates, on whole-window figures, could not see
the two were at the same time. Separately, the CPU wait branch of
`NODE_CONTENTION` had no throttle guard, so 49 could still happen on a
container without PSI.

**Caught by:** a 15-minute report during a run on a test cluster,
2026-10-29.

**Fixed by:** pressure stands in for throttling only where throttling is not
measured; the CPU wait `NODE_CONTENTION` is withheld from a throttle-stalled
container, as the PSI one is. `TestUnthrottledLimitIsNotBlamed`.

**And where throttling is not measured,** pressure on a node idle on average
cannot tell the quota from the neighbours either, and the row got both reasons.
It now gets `THROTTLE_UNKNOWN` instead of either, naming the missing CFS
metric, and the `NODE_CONTENTION` already on the row is withdrawn
(`TestPSIAloneCannotTellLimitFromNode`, formerly `TestPSIAloneIsEnough`;
`TestUnmeasuredThrottlingBlamesNeither`). Without node figures the row keeps
`NODE_CONTENTION` (`TestSuppressedPressureIsExplained`), beside
`NODE_UTIL_UNKNOWN` where the nodes were known. Telling the two apart needs the time: whether
the node was full in the stretches the container waited, which the CPU wait
rule already reads per stretch.

## Open, and not yet solved

**A workload redeployed more than once a day never has usable CPU wait.**
CPU wait compares each pod with its own normal (55), which needs a day of
history, and every rollout starts new pods. Such a workload is UNVERIFIED with
`CPU_WAIT_SHORT_HISTORY` however long the window: safe, and the agent is of no
use to it. A normal per workload, pooled across its pods, is not the free fix
it looks like. PromQL cannot take one quantile over time and series at once,
so the stretches would be fetched and pooled in Go, and that is nearly every
busy stretch rather than the candidates `stalledCandidatesExpr` keeps small. A
rollout runs new code, whose normal may differ: pooled with the old version's,
a new version that simply waits more per second run reads as stalled, or hides
a real stall. It changes the rule for replicas side by side, not only for
replacements. The rule's 98.5% and 99.6% were measured per container on
one-hour runs, which cannot show rollouts spread over days, and
`agent/compare-pressure` applies it per container. The first thing to try is a
normal per ReplicaSet (one pod template, so one version), which covers
evictions and reschedules but not frequent deploys, measured on
`test/longrun` before anything wider.

**The kubelet can lie about PSI. Mitigated 2026-09-20, not eliminated.**
Kubernetes issue #136333: on hosts without kernel PSI support, the kubelet
emitted **all-zero** `container_pressure_*` series for every container, purely
because the feature gate was enabled. No error, no absent metric — a flat zero
that reads as "no pressure" rather than "no data". tare marks a signal available
when the query returns a series, and a zero-valued series is still a series, so
on such a cluster it would print `psi 0.0000/0.0000`, report nothing missing,
and call every container `WASTE` with high confidence.

The cluster-level negative control described here is now implemented
(`flagDeadPressure`, `internal/metrics/prometheus.go`): if every container
drawing at least 50m reports **exactly** zero pressure across the whole window,
and there are at least three such containers, the pressure values are withheld
and the report header says why. It tests the digit rather than a threshold, so a
cluster genuinely at 1e-9 still passes.

Two things it does not fix. **The population thresholds are not derived** — 50m
and three containers are chosen so the control errs toward silence, and what
limits the damage is the direction of the error: firing wrongly removes a
saturation signal, which pushes findings to `Unverified` and confidence to
`Low`, so a mistake makes the tool more cautious and never less. Failing to fire
is the expensive direction and the thresholds do not protect it. **A partially
dead instrument still passes**: one node without kernel support in an otherwise
healthy cluster leaves the fleet-wide test false, and the containers on that
node are read as calm. The control is per-cluster because the observation is
per-cluster; making it per-node needs the node label carried through the
pressure queries, which has not been verified against a real Prometheus.

**The memory trend is still keyed by pod, not by container id.** Defect 19 is
contained, not cured: a container that OOMKills inside the window has no usable
trend at all, so tare can say "growth cannot be ruled out" and nothing more. The
measurement that would settle it is a within-incarnation trend, which needs the
query keyed by container id and has not been verified against a real
Prometheus. Until then a cyclic leaker and a container that is simply too small
are indistinguishable here, and the report says so rather than picking one.

**A container whose metrics stopped mid-window is sized from stale data.** The
window is wide on purpose, so a kubelet that stopped reporting five days ago
still leaves a fortnight of history behind it, and nothing in the report says
the data is old. Confidence falls, because the sample count falls, but that is
an accident of the same arithmetic rather than a statement about staleness. The
measurement that would settle it is the timestamp of the last sample per
container, which is one more query and has not been added.

**A CPU limit below the current one is never measured.** It has been decided:
tare holds CPU limits and only ever raises them (53). Lowering one safely would
need a signal finer than a 1-minute rate, such as per-period usage or
throttling observed at the candidate limit. tare collects neither, so this
stays out of scope.

**The memory floor is undefended.** `MinMemBytes` is 16Mi. Comparable floors
elsewhere are 100MiB and 250MB, an order of magnitude higher. On a typical
report the floor decides more rows than any evidence logic does, and 16Mi has no
published derivation. Nor do the others, but tare is the tool claiming it does
not produce numbers it cannot justify.

---

## What this list changed about how tare is built

- **When a check comes back clean, ask what it would look like if the instrument
  were broken.** Several tests in this repository passed for the wrong reason
  until they were deliberately made to discriminate.
- **Prefer a negative control to a green result.** A fake server, a
  counterfactual assertion, or a deliberately reverted fix that must turn the
  test red.
- **A missing measurement is not a zero.** Every signal is tracked separately
  from its value, and the report states what it could not see rather than
  printing the comfortable number.
- **For anything that runs on a clock, exercise every time-dependent path by
  hand first** and confirm each one changed something observable. Defects 6 and
  7 were both found that way, and each would otherwise have cost fourteen days.
