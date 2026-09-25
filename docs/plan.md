# tare - Kubernetes right-sizer, v0.1 plan

*`tare`: the weighing term for subtracting the container's own weight. Apache-2.0.*


## Current state (2026-09-14)

**Working and verified end to end.** `kubectl tare report` runs against a real
cluster with no flags, finds Prometheus itself, and produces correct findings for
every planted case. 96 test assertions pass across six packages. Nothing is
committed to version control; that is deliberate.

| area | state |
| --- | --- |
| `internal/ceilings` | 4 runtime families plus behavioural inference. 25/32 JVM specs in a 135-spec corpus; the other 7 are verified correct rejections. |
| `internal/metrics` | Prometheus over HTTP or the API server proxy. 11 tracked signals, health surfaced per query. Tested against a fake server. |
| `internal/discover` | Workload inventory, Prometheus auto-discovery, HPA/VPA/QoS constraints. |
| `internal/analyze` | Verdicts, recommendations, signal-gap degradation, constraint handling. Tested by replay against the simulator. |
| `internal/simulate` | 12 archetypes, 30 simulated days in 27ms, implements `metrics.Source` so the real analyzer runs against it unchanged. |
| `cmd/kubectl-tare` | report only, read-only, with table/json/patch output. |

*Updated 2026-09-19. The morning's pass rewrote this plan only - "Savings
reality", "Node and hardware awareness" and "ML position, fourth pass" set the
current direction and "Where to pick up" is reordered to match. The afternoon's
pass implemented the first three items of that list; see "Implementation pass".
160 tests now pass across 8 packages.*

| area | state *(added 2026-09-19)* |
| --- | --- |
| `internal/packing` | Bin-packing replay producing a node-count delta, separating the saving the recommendations earn from the cluster's existing slack. Pure; 10 tests. |
| `internal/discover` | Gained node and pod inventories: allocatable, node classes, effective pod requests, and the pin reasons the replay refuses to model. |
| `internal/analyze` | Gained the `IDLE` verdict and four node-aware findings (per-node workload, mixed hardware, thread-pool mismatch, unschedulable recommendation). |

### Where to pick up

**Reordered 2026-09-19.** The previous list was three correctness gaps in the
sizing path. It was not wrong, but it assumed the tool's output was already the
thing a user wants, and it is not: see "Savings reality" below. The gaps are
now ordered by distance to a saving a user can actually bank.

1. ~~**The savings number is in the wrong unit.**~~ **Done** *(2026-09-19)*:
   `internal/packing` and the node-count delta. The replay's design changed
   during implementation; see "Implementation pass" below.
2. ~~**DaemonSets are not ranked first.**~~ **Done, and the premise was partly
   wrong** *(2026-09-19)*. See "Implementation pass" below: the multiplier was
   already correct, because a DaemonSet has one pod per node and the existing
   replica multiplier therefore counts nodes. What was missing was that the
   reader could not tell, and that a DaemonSet's request is per-node overhead
   in the packing model.
3. ~~**Idle and zombie workloads are not detected.**~~ **Done** *(2026-09-19)*:
   the `IDLE` verdict, with `DormantSidecar` and `RareBatch` added to the
   catalog as the positive case and the negative control.
4. **The VPA baseline comparison was specified and never built.** This document
   calls it "the single most persuasive artifact in the tool" and "cheap to
   build" (see Notes on the pieces). `internal/analyze/constraints.go:26`
   detects VPA *conflicts*; nothing computes what the VPA recommender would have
   said. Every parameter needed is already written down in the Landscape check.
5. **Nobody has run tare against a cluster we did not build.** Verification step
   4, never performed. Every measurement in this document comes from k3d with
   planted workloads, and the one time real load was introduced the generator
   turned out to be the workload.

Then the three sizing-path gaps, which were the previous top of this list:

6. **Init containers and native sidecars are skipped entirely.** `discover`
   reads only `pod.Spec.Containers`. The corpus is full of init containers and
   34% of specs have a shell entrypoint, so this is a real blind spot.
7. **Pod-level resources** (`spec.resources`, Kubernetes 1.34+, Beta as of 1.37)
   are unmodelled.
8. **Spark and Flink memory configuration** is the one genuine parser gap the
   corpus found (0 of 4).

Lower down: `PlateauHeadroom` is still unmeasured because no archetype exercises
it, and the observational ceiling inference cannot distinguish a capped heap
from genuinely steady demand (the consequence of being wrong is bounded, and
documented where it is implemented).

**A long-running stability experiment is collecting on msi.** See
`test/longrun/README.md`. It has no end date and nothing will notify you. The
clock was **restarted 2026-09-14 21:55 UTC** after the load generator was found
to be generating no measurable load; day 7 and day 14 count from then.

## Context

Kubernetes workloads are chronically over-provisioned, and every platform team
already knows it. The blocker is not the recommendation (Goldilocks and KRR give
that away for free) but **trust in actuation**: nobody gets promoted for saving
$12k/month, but they do get paged for the OOMKill at 3am.

This project targets that gap. The original framing was "an AI tool that learns
resource sizing from telemetry history." Grilling that idea produced something
better and more honest: **a safety-first, evidence-driven right-sizer** that uses
directly measurable saturation signals (CPU throttle ratio, PSI, OOMKills, GC and
page-fault pressure) as a gradient rather than inferring a counterfactual
performance curve it has no data to identify. Machine learning enters later, with
one specific and benchmarkable job: predicting where the descent will end, so
convergence takes days instead of weeks.

Intended outcome: an open-source Go tool that a platform engineer can run in 30
seconds against an existing cluster, that tells them something KRR does not, and
that (after opting in, workload by workload) can safely act without ever
restarting a pod.

### Decisions settled during design (do not relitigate)

| Decision | Choice |
| --- | --- |
| Product shape | Advisor first; actuation as a per-workload opt-in graduation, safety contract designed up front |
| Wedge | SLO-aware sizing plus explainability (every recommendation carries evidence, confidence, blast radius) |
| Core mechanism | Saturation evidence as a measured gradient, not an inferred performance curve. **Primary signal is PSI** (`container_pressure_cpu_*`); CFS throttle ratio is enrichment where exported, never a dependency (see Environment findings) |
| Beachhead | Stateless HTTP services |
| Knobs owned | requests + limits + HPA min/max; runtime flags detected and reported, never written |
| SLI source | eBPF auto-instrumentation (Beyla/Cilium) default, BYO PromQL escape hatch, mesh/OTel adapters. **v0.3, not v0.1** |
| Loop shape | Asymmetric: down by at most 20% per step with long observation, **up instantly and uncapped** |
| Trip wires | Deploy detected then freeze and re-baseline; OOMKill or restart increase; SLO burn or p99 regression; throttle threshold |
| Memory | Reduce only on positive headroom evidence; **refuse to recommend below a detected runtime ceiling** (`-Xmx`, `GOMEMLIMIT`, `--max-old-space-size`) and surface the flag as the finding |
| Scope defaults | Observe and recommend cluster-wide; actuate only on labelled workloads; dry-run on install |
| Stack | Pure Go, one binary, no ML runtime |
| Interfaces | kubectl plugin (read), CRD (policy), PR bot (write) |
| ML | None in v1. Later: descent-endpoint prediction and periodic demand forecasting, both additive to a loop that works without them. **Revised 2026-09-19:** still none in the sizing path, for the identifiability reason restated below; admitted for **ceiling extraction from unstructured config**, which has ground truth in the corpus and a refusal rule downstream of it |
| Testing | Workload simulator built **before** the loop |
| Positioning | Lead with evidence and auditability: "every change carries its evidence, its confidence, and the trigger that will reverse it." Never-restarting is a supporting property, not the headline, since VPA can now claim it too |
| Version support | Read path supports k8s 1.29+ (needs only the API and Prometheus). In-place actuation gated on 1.35+ and cgroup v2; webhook piggybacking is the fallback below that |
| Name / license | `tare`, Apache-2.0, pure OSS |
| Priority order | **User experience first, then integration ease, then capability.** This is an open source project whose distribution is a GitHub page: a feature nobody can run without configuration does not count as shipped. Zero required flags, one binary, no cluster changes. |
| No-Prometheus clusters | v0.1 requires a Prometheus-compatible endpoint with auto-discovery; degraded snapshot mode lands in v0.2 |
| Unit of savings *(2026-09-19)* | **Node-count delta from a bin-packing replay**, not reclaimed millicores. Millicores are the input to a saving and are reported as such. Never print a currency figure the replay cannot defend |
| Node inventory *(2026-09-19)* | A first-class input to `internal/discover`. Used for **scheduling** first (feasibility, bin packing, per-node overhead) and hardware characteristics second |
| Hardware awareness *(2026-09-19)* | Admitted only where it changes a number or produces a refusal. Config findings and refusals, never CPU-model performance normalisation, which cannot be identified without benchmarking |

## Landscape check (research, September 2026)

Verified against primary sources. What it changes:

**In-place resize is further along than the plan assumed.** Container-level
KEP-1287 went **GA in Kubernetes 1.35** (Dec 2025); the pod-level aggregate variant
is still Beta as of 1.37 (Aug 2026). Requires cgroup v2 for non-disruptive resize.
QoS class is immutable across a resize. Memory *shrink* is best-effort only: the
kubelet checks current usage first, but it is a TOCTOU race and cgroup v2 can
OOM-kill. **This independently validates the Q17 memory rule** and means in-place
actuation can be a v0.4 concern rather than v0.5.

**VPA already ships the "no restart" behavior.** VPA v1.7.1 has `InPlaceOrRecreate`
GA since v1.6.0 and an `InPlace` (never-evict) alpha mode in v1.7.0. The Q14
positioning line therefore needs care: VPA gets there too, but it still updates all
containers in a pod together, refuses QoS-changing resizes, falls back to recreation
for memory downscale, and its own AEP-4016 states it "cannot guarantee NO
disruptions." More usefully, **kubernetes/autoscaler issue #9362 (filed 2026-03-16,
still open)** documents VPA 1.6.0 evicting pods roughly once per minute on
production EKS clusters against stable recommendations. That bug is the trust
argument, in one link.

**The VPA recommender baseline to beat** (current defaults, worth encoding as
comparison output): 24h decay half-life for both resources; target percentile 0.90;
lower bound p50, upper bound p95; safety margin +15%; confidence interval widening
over a 24h window; memory as an 8-day rolling window of daily peaks; OOM bump-up
1.2x with a +100Mi floor; resource floors 25m CPU and 250MB memory.

**Nobody in the field is actually doing ML.** KRR is p95 CPU and max-over-week +15%.
Cast.ai is p95/p99 despite the name. Goldilocks is a dashboard over the stock VPA
recommender. StormForge, Densify, ScaleOps and Sedai claim ML with no disclosed
algorithm. The Q19 decision to ship an honest controller costs nothing competitively.

**Two corrections to earlier claims made during design:**

1. *"No incumbent surfaces the runtime ceiling"* was too strong. **PerfectScale**
   already reads JVM heap, non-heap and GC behavior via an eBPF node agent. What
   remains genuinely unoccupied is the **refusal rule**: reporting the `-Xmx` value
   as the finding and declining to recommend below it. Keep the rule, drop the
   "nobody does this" framing, and note it is unoccupied in OSS specifically.
2. *The SLO wedge is not empty.* **Akamas** does real experiment-driven optimization
   on application telemetry (Prometheus + Istio latency, throughput, error rates).
   It is enterprise, credit-priced, and requires active experimentation, so the
   passive-observation OSS position still stands, but it is prior art and the README
   should not claim novelty it does not have. Academic work (SLO-Scaler, arXiv
   2608.18390) targets replica scaling, not vertical sizing. Google Autopilot
   (EuroSys 2020) remains the strongest production precedent and is usage-based, not
   telemetry-driven.

**Note on v0.1's shape:** KRR is an agentless CLI that queries Prometheus and the
K8s API directly. That is exactly the form of the day-1 report. The differentiation
has to live in the *findings* (throttle-first ranking, ceiling refusal, confidence
scoring), not in the shape of the tool.

**Storage reality:** Prometheus still defaults to ~15 days retention, so most first
runs will have less history than the recommender wants. That makes the confidence
field load-bearing rather than decorative. Long-term backends to support behind
`metrics.Source`: Thanos, Grafana Mimir, VictoriaMetrics (the most resource-efficient
and the sensible default recommendation). The OpenTelemetry Collector is a pipeline,
not storage.

**Karpenter:** no primary-source-documented conflict between VPA and Karpenter
consolidation was found. Consolidation keys off requests, not usage, so lower
requests generally improve bin-packing; oscillating recommendations causing node
churn remains a theoretical risk. `karpenter.sh/do-not-disrupt` is the escape hatch.

## Environment findings (measured on the test cluster, 2026-09-14)

Verified on k3d `tare-test`: k3s v1.35.8+k3s1, cgroup v2, containerd 2.2.7,
kube-prometheus-stack. See `test/lab/README.md`.

**CFS throttling metrics cover only a minority of containers.**

*(Corrected 2026-09-14. The original claim here was that this cluster exports no
`container_cpu_cfs_*` series at all. That was wrong: it exports them for
containers that have a CPU limit. The first measurement was taken on a fresh
cluster where no workload had one, so the metric family was absent entirely.)*

cAdvisor emits `container_cpu_cfs_*` only where a CPU quota is set. Measured on
this cluster: **PSI covers 23 of 23 containers, CFS covers 3 of 23** - exactly
the three with a CPU limit.

This strengthens rather than weakens the case for PSI. A container with no CPU
limit cannot be throttled, so throttle-based detection is structurally blind to
it, while PSI still registers contention for CPU it did not get from the node.
Any tool whose saturation signal is throttling can only see the containers
someone already capped.

**Consequence: PSI is the primary gradient, not throttle ratio.** Measured on a
planted busy-loop capped at 100m CPU, PSI waiting read **0.8754 s/s** while a
healthy Tomcat read 0.0003 and an idle nginx read 0.0000. The signal separates
starved from healthy containers cleanly with no CFS metric involved. The analyzer
must treat throttle ratio as optional enrichment and degrade gracefully when it is
missing, or it will report nothing on clusters like this one.

**In-place resize confirmed working.** CPU request 100m to 150m and memory limit
128Mi to 96Mi (a live shrink) both applied through the `resize` subresource with
**zero restarts**. The QoS-immutability guard is enforced: a BestEffort-to-Burstable
resize is rejected by the API server. Both match the research, on real hardware.

## Simulator findings (2026-09-14)

`internal/simulate` models seven workload archetypes whose correct size is known
in advance, and implements `metrics.Source` plus `discover.Container` so the
real analyzer runs against it with no test-only code path. 30 simulated days
runs in **27ms**, which is what makes threshold sweeps practical.

The closed-loop test is the safety claim in executable form: take the analyzer's
own recommendation, apply it as a hard limit, replay the same window, and
require no OOMKill and no severe starvation.

**It found two real defects on its first run, neither visible in the live lab:**

1. **A single CPU number cannot serve as both request and limit.** The bursty
   archetype was left at PSI 0.864 by a p95-sized limit. Fixed by splitting the
   recommendation: request from p95 plus margin, limit from observed peak plus a
   25% burst margin. The bursty workload now gets 128m request and 1179m limit,
   a 9.1x headroom matching its 8x spikes, and replays at PSI 0.000.
2. **A memory leak was being sized rather than reported.** The leaking archetype
   was handed 9.2Gi and OOMKilled anyway. Added a `LEAKING` verdict from the
   early-window versus late-window memory ratio, ranked above every other
   verdict, which withholds the recommendation and says why. No allocation
   survives unbounded growth, so offering a number is worse than offering none.

A third defect came from the fix itself: the first leak check required
`Restarts == 0`, which excluded exactly the leaks bad enough to OOMKill. The
restart guard is unnecessary, because a container whose restarts reset memory
fails the ratio test on its own.

### Known gaps the simulator cannot yet see

- ~~No startup burst modelling.~~ **Fixed 2026-09-14.** See "Startup regime"
  below. Tomcat's CPU limit recommendation went from 10m to 62m.
- **Cold-start warming can look like a leak.** For a container younger than the
  window, the early hour is its warm-up, so the ratio test may false-positive.
  Frequent-restart workloads are safe (their memory sawtooths), but a
  cache-warming service on its first day is not.
- **Thresholds are still unswept.** The simulator makes sweeping cheap, but
  `DefaultThresholds` are hand-picked and no sweep has been run.
- `internal/discover` and `internal/metrics` have no tests; only the analyzer
  and the parsers are covered.

## Startup regime (fixed 2026-09-14)

The 10m CPU limit recommended for Tomcat was diagnosed as a **windowing defect,
not missing domain knowledge**. The evidence was already in Prometheus:

| container | boot peak | steady peak | ratio |
| --- | --- | --- | --- |
| tomcat | 0.0449 cores | 0.0023 | **19.5x** |
| cpu-hog | 0.1000 | 0.1002 | 1.0x |

Two causes, both fixed:

1. **The peak query used a five minute inner rate window**, which averages a
   twenty second boot burst down to nothing. Peaks that drive limits now use a
   one minute window; percentiles that drive requests keep five minutes.
2. **Boot and steady state were not separated.** Container age at each
   evaluation step comes from `timestamp(container_start_time_seconds) -
   container_start_time_seconds`, which lets a range query split the two
   regimes retrospectively. The request tracks steady state; the limit must
   cover boot, with a 40% margin rather than the 25% burst margin, because a
   throttled boot can fail a readiness probe and turn a rollout into an outage.

A regime only counts as distinct above a 1.5x ratio. Below that, boot and steady
state are the same thing and saying otherwise is noise: a busy loop looked
identical in both and was emitting a pointless "1.0x" finding.

**Verified both ways.** In simulation, `CeilingJVM` boots at 1.5 cores for 40s
and settles at 0.28. A steady-state-only limit of 250m leaves it at PSI 0.833 at
every restart; tare's 2100m limit gives PSI 0.000. On the live cluster, Tomcat's
limit recommendation moved from 10m to 62m with the reason attached.

**The general lesson, worth applying to the next gap:** before reaching for a
model, ask whether the signal is genuinely absent from telemetry or merely being
measured badly. Here it was measured badly.

## Observational ceiling inference (2026-09-14)

The corpus showed that a large share of real ceilings are **not in the pod spec
at all**: they live in image entrypoints, ConfigMaps and `envFrom`. No parser
reaches them, and neither would a perfect language model reading the same input,
because the information is absent rather than obscure.

Behaviour is not absent. A runtime holding a fixed heap plateaus at that heap
and stays there. `internal/analyze/observed.go` infers a ceiling from that shape
when the spec is silent, and fires only when all of these hold:

- no memory ceiling was **read** from a flag (an assumed one does not count)
- the image is a runtime that can cap its own heap
- p95 memory is within 90% of the maximum, so it is a plateau and not a peak
- the plateau sits below 70% of the limit, so there is headroom to explain
- at least twelve hours of samples

An inferred ceiling can only ever **raise** a recommendation, and it is reported
as inferred so a human can confirm the flag.

**Verified by discrimination, not by assertion.** Two archetypes share the image
`eclipse-temurin:21-jre`. `HiddenCeilingJVM` has a 256Mi cap declared nowhere in
its spec and shows p95/max = 1.000; inference fires and floors the
recommendation at 368Mi, clearing the hidden cap. `VariableMemJVM` has no cap
and shows p95/max = 0.619; inference correctly stays silent. Adding memory noise
to the demand model was necessary to make this testable: without it every
simulated workload plateaued perfectly and the negative control passed for the
wrong reason.

### A latent bug this uncovered

`jvm-default-heap` and `MaxRAMPercentage` were both contributing to the memory
**floor**. Both scale with the container limit, so shrinking the limit shrinks
the heap. Treating them as a floor meant tare could never recommend a reduction
for any unflagged JVM: it was pinned at a quarter of whatever limit it happened
to have. Ceilings now carry `ScalesWithLimit`, and only fixed ceilings act as
floors.

### Honest limitation

For a managed-runtime image, a flat plateau is treated as a ceiling, and a
capped heap cannot be fully distinguished from an application that simply has
steady demand. The consequence of being wrong is bounded: the recommendation is
raised to plateau plus overhead, leaving some memory unclaimed rather than
causing an OOMKill. The strong form of the signal, a limit that moved while the
plateau did not, is implemented and carries a much more confident message, but
it only applies when the limit actually changed during the window.

## Threshold sweep (2026-09-14)

`internal/analyze/sweep_test.go` replaces judgement with measurement for the
nine thresholds in `DefaultThresholds`. Scoring is by replay: take the
recommendation a configuration produces, apply it as a hard limit, run the same
week again, and check the workload survives. 12 archetypes x 3 starting
scenarios (generous, mildly starved, severely starved) = 36 outcomes per value.

**Two shipped values were wrong and are changed:**

| threshold | was | now | evidence |
| --- | --- | --- | --- |
| `LeakRatio` | 2.0 | **1.5** | 1.5 scores 36/36; 2.0 and 1.2 both score 33/36. At 2.0 a workload that merely doubles its memory over a week is not recognised as leaking. |
| `PSIWaiting` | 0.05 | **0.03** | Every value to 0.05 scores 36/36; 0.10 and above drops to 26/36 by missing mild starvation. 0.05 was the last good value, sitting on the cliff edge. |

`BurstMargin` is verdict-neutral across its whole range but costs reclaimed CPU
monotonically: 0.00 reclaims 25%, the shipped 0.25 reclaims 15%, and 1.00
reaches -21%, recommending more than the container started with. The catalog
cannot resolve the right value because replay only ever sees load the observation
already saw; it cannot price the spike that has not happened yet. Left at 0.25.

### Two rounds were needed, and the first measured nothing

The initial sweep reported five of nine thresholds as having no effect anywhere.
That was a fault in the harness, not a fact about the thresholds:

1. **Every scenario started from a generous allocation**, so nothing was ever
   starved and the entire saturation path was unreachable. Severe starvation was
   then added, which produced PSI around 0.65, above every plausible threshold
   and therefore equally uninformative. Only mild starvation at 0.92 of peak,
   landing at PSI 0.08, straddles the decision boundary.
2. **Only replay safety was scored, not verdict correctness.** `WasteRatio`
   changes a label rather than a number, so it appeared inert.
3. **No archetype sat near a boundary.** Three were added for that purpose alone:
   `MarginalPlateau` at p95/max 0.83, `MildStartup` at a 1.4x boot ratio, and
   `SlowGrowth` doubling memory over a week.
4. **Averages hid single-archetype effects.** A threshold that moves one
   workload's limit by 11% shifts a twelve-way mean by 1%, which reads as noise.
   The sweep now names the archetypes that respond to each threshold.

`PlateauHeadroom` still has no responsive archetype and remains unmeasured.

## User-facing work (2026-09-14)

Directive: open source, user oriented, easy to integrate. What that changed:

**Zero-configuration first run.** The tool required `--prometheus <url>`, which a
laptop usually cannot reach at all: a ClusterIP service has no route from outside
the cluster, so the real requirement was "set up a port-forward first". It now
finds a Prometheus-compatible service itself and queries it **through the
Kubernetes API server proxy**, the mechanism `kubectl proxy` uses. That works
wherever kubectl works: no port forward, no ingress, no VPN.

Discovery prefers long-retention backends (Thanos, Mimir, VictoriaMetrics) over
Prometheus, because this tool wants weeks of history and a default Prometheus
keeps about fifteen days. It matches on labels before names so a renamed Helm
release is still found, skips gRPC ports that do not serve the query API, and
prints which service it chose and why so a wrong guess is visible and
correctable rather than mysterious.

`metrics.Transport` was introduced for this: the HTTP and API-proxy paths sit
behind one interface, so the analyzer is unaware of which is in use.

**A repository a visitor can understand.** README, Apache-2.0 LICENSE,
`.gitignore`. `plan.md` moved to `docs/`: it is a design record full of dated
corrections, valuable to us and confusing as the most prominent file in the repo.

**`--window 14d` did not work.** Go's `time.ParseDuration` stops at hours, so the
obvious input for a tool whose whole subject is weeks of history was rejected
with "parse error". Days and weeks are now accepted, the default is 2w, and the
header renders `2w` rather than `336h0m0s`. This was documented in the README
before it worked, which is the right way round to catch it.

**`internal/discover` gained its first tests**, covering the classifier: label
matching, gRPC port avoidance, and the long-retention preference.

## Usefulness pass (2026-09-14)

Directive: usability, functionality and integration; skip release mechanics.
Three correctness holes were found before any usability work, because a tool
whose identity is "safe and honest" cannot ship recommendations that break
things when applied.

**There was no memory limit recommendation at all.** `RecMem` was the request,
and nothing said what the limit should be. Added `RecMemLimit`, set at the
request plus 25%: memory has no graceful degradation, so a wide gap between
request and limit only buys time before an OOMKill.

**Guaranteed pods were being silently demoted.** A pod whose requests equal its
limits is Guaranteed. Recommending different values costs it eviction priority
and, on clusters with in-place resize, is rejected outright because QoS may not
change. Requests are now held equal to limits for such pods, with the reason
stated.

**HPA interaction was invisible.** An HPA scaling on CPU or memory *utilisation*
measures against the request, so the request is the scaling setpoint. Halving it
doubles observed utilisation and the workload scales out: a CPU saving paid for
in replicas. tare now reads HPAs, matches them by `scaleTargetRef`, and states
the multiplier. Only `Utilization` targets are affected; `AverageValue` is not
request-relative.

**VPA conflicts were invisible.** A workload managed by a VerticalPodAutoscaler
in Auto or Recreate mode now gets a `CONFLICT` verdict and no numbers at all.
Two controllers writing the same field is a fight, and the loser is whichever
wrote last. `updateMode: Off` is correctly not treated as a conflict.

### Usability and integration

- **A summary line.** Verdict counts plus total reclaimable CPU and memory,
  multiplied by replica count: a 100m saving on twenty replicas is two cores,
  and reporting it as 100m understates the thing that matters.
  *(Corrected 2026-09-19. The multiplication is right and the unit is wrong:
  reclaimed millicores are not a saving, because a request is a scheduling
  reservation and freeing one changes no bill until a node goes away. See
  "Savings reality" below. The claim is left standing here because the arithmetic
  it describes is still correct and still needed as the input to the replay.)*
- **`--top N`, `--verdict`, `--by-savings`.** A cluster-wide run on hundreds of
  containers is unreadable sorted by severity alone.
- **`-o patch`.** The first version emitted a partial manifest for
  `kubectl apply` and **did not work**: apply performs a three-way merge and
  treats the fragment as the complete desired state, nulling out the selector
  and image. Server dry-run rejected all three test workloads. It now emits
  runnable `kubectl patch --type=strategic` commands, which touch only the named
  fields, with the reasoning as comments. Verified against a live API server.
- **`--window 14d`** and the API-server-proxy discovery from the previous pass.

`internal/discover` and `cmd/kubectl-tare` now have tests.

## Load-generation findings (2026-09-14, long run restarted)

The long run's first six hours were discarded and the clock restarted at
21:55 UTC. The full account is in `test/longrun/README.md`; what matters for the
design record:

**The load generator was the workload.** `load-generator` (busybox `wget` in a
fork loop) drew 35m of CPU while `diurnal-web`, the thing it was supposed to be
loading, drew 3.2m per replica against a 300m request. The scaffolding outweighed
the subject 5.5x, because serving a static file costs nginx ~0.05ms and forking a
process per request costs the client 1-2ms.

**A floored recommendation is a false pass.** At 3m of CPU, `diurnal-web`'s
recommendation clamped to `MinCPUMillicores` and became a constant. The long
run's whole question is "does the same workload get the same recommendation on
day 3, 7 and 14?", and a constant answers it affirmatively without the analyzer
ever having read the data. This is the same shape as the five defects already in
this record: a clean, plausible number in place of an error.

Three changes came out of it:

1. `Finding.CPUFloored` / `MemFloored`, set where the recommendation is the floor
   rather than anything derived from usage, with a reason string that says so.
   Previously `RecCPU: 10` was indistinguishable from a measured 10m.
2. `stability.py` prints `FLOOR` instead of `stable` for such a series, and the
   failure criteria now say a floored series is not evidence.
3. `PSISeen` and `ThrottleSeen` were declared on `Finding` and never assigned or
   read - they serialised `false` into every snapshot while `PSIWaiting` carried
   a real value. Deleted.

**The methodological point, which generalises past this bug:** the simulator
cannot find this class of defect, because in simulation the demand curve is an
input. On real hardware demand must be *produced*, and the producer is a program
that can be wrong in ways that look like a quiet, healthy workload. `wget` also
turned out to exhaust ephemeral ports under sustained load while `-q` hid every
error. Calibrate the generator against the target and check the ratio before
starting any clock that depends on it.

**Both cronjobs were also dead.** `bitnami/kubectl:1.33` 404s (Bitnami retired
their free Docker Hub catalog), so `redeploy-churn-app` and
`toggle-resize-limit` had been in `ImagePullBackOff` for four hours.
`churn-app` was still at revision 1 and had never been redeployed, and the
resize toggle - the only producer of the limit-trend signal this document calls
the strongest evidence of a fixed runtime ceiling - would never have fired.
Now `alpine/k8s:1.33.1`, verified by triggering both jobs by hand.

So of the five behaviours only elapsed time can test, **three were dead on
arrival** and none of them failed loudly. The cost of finding this on day 14
instead of day 0 would have been the entire experiment.

### Two open questions the live cluster raised

- **`resize-target` is verdicted SATURATED at PSI 0.047 while using 1.8m of CPU
  against a 100m request.** The swept `PSIWaiting` threshold of 0.03 fires; the
  previous 0.05 would not have. This is the first evidence that the sweep, scored
  entirely in simulation, picked a value that false-positives on real hardware -
  a JVM's GC and JIT threads appear to register genuine waiting without the
  container being starved in any useful sense. Deliberately **not** changed on
  six hours of data; the long run should now show whether it persists.
- **`MemEarly` is 0 on every container**, so the leak ratio test cannot fire and
  `slow-leaker` is currently being *sized* (166Mi) rather than reported - the
  exact defect the simulator caught and fixed. Here it is inert rather than
  wrong: the window is 7d and nothing is older than a few hours. It should
  self-correct as the window fills, and the long run will confirm that it does.
  If it does not, the early-window query is broken against real data.

## Savings reality (2026-09-19)

**The tool reports a number that is not a saving.** The summary line totals
reclaimed CPU and memory across replicas. A request is a scheduling reservation,
not consumption: lowering it frees allocatable capacity and improves bin
packing, and it changes the invoice only when the node count falls. On a fixed
node pool the bill after applying every recommendation in the report is
byte-identical to the bill before. This is the same failure shape that has now bitten
this project eighteen times - **a clean, plausible number in place of the honest
answer** - and it sits in the one field a user reads first.

Where the number does become real: Karpenter and cluster-autoscaler consolidation
key off *requests*, not usage, so lower requests genuinely do remove nodes there.
That is an argument for reporting the node delta, not for reporting millicores
and hoping.

**The honest unit is a node-count delta**, produced by replaying the
recommendations through bin packing against real node allocatable and real pod
placement. This is also the answer to the question a platform engineer actually
has, which is not "how much is over-provisioned" (they know) but "if I apply
this, does anything happen".

### What the replay must respect, or it lies

An approximation that ignores scheduling constraints will overstate the saving,
and an overstated saving is worse than no number at all for a tool whose whole
claim is that its outputs can be trusted. The replay must honour node affinity
and anti-affinity, taints and tolerations, topology spread constraints, PDBs,
extended resources (`nvidia.com/gpu` and friends), and the per-node DaemonSet
overhead that does not disappear when a node does. Where it cannot evaluate a
constraint, it must exclude the node and say so, in the same way the report
already degrades on a missing signal.

Kilter built an entire component for this, which is the correct size estimate
for the work. It is the largest single item now outstanding.

### Two findings that are bigger than mis-sizing and far cheaper

1. **DaemonSets carry a per-node multiplier.** 100m over-set on a 100-node
   cluster is ten cores. `internal/discover` resolves owners generically
   (`discover.go:159` walks ReplicaSet to Deployment) and nothing distinguishes
   a per-node workload from a replicated one. Same arithmetic as the existing
   replica multiplier with a larger factor. *(Whether KRR or Goldilocks already
   rank this way is unverified; check before making a claim about it in the
   README, per the two retractions in the Landscape check.)*
2. **Idle and zombie workloads.** Near-zero CPU with no traffic across the
   window is cheap to detect from signals `internal/metrics` already collects,
   and the recommendation is deletion rather than resizing. In most clusters
   this is a larger slice of waste than sizing error. It needs a distinct
   verdict, because "size this to nothing" and "this should not exist" are
   different sentences.

### Positioning consequence

tare does not print a currency figure. It prints a node delta with the
constraint set it honoured and the nodes it had to exclude, and leaves the
multiplication by instance price to the user who knows their own contract.
Every competitor that prints dollars also owns node consolidation; tare does
not, and claiming their unit without their mechanism is exactly the overclaim
this document keeps having to retract.

## Node and hardware awareness (2026-09-19)

Admitted to the design, with a filter: **a hardware fact earns its place only if
it changes a recommendation or produces a refusal.** CPU model, NUMA layout, LLC
size and memory bandwidth are all measurable and none of them survive that test
today. Ranked by value per line:

1. **`GOMAXPROCS` / `-XX:ActiveProcessorCount` against node cores and the CFS
   quota.** A Go runtime with `GOMAXPROCS` unset on a 96-core node under a
   2-core quota spawns 96 Ps, burns the quota in the first few milliseconds of
   each 100ms period and stalls for the remainder. The throttling is a
   configuration defect, and raising the limit is the wrong fix.
   `internal/ceilings` already parses both flags and has no idea how many cores
   the node has; `node.status.capacity.cpu` closes it.
2. **Refuse to break exclusive CPU pinning.** On a node with
   `cpuManagerPolicy: static`, a Guaranteed pod with integer CPU owns physical
   cores. Recommending 1500m silently returns it to the shared pool, which is a
   latency regression no saving justifies. Read the policy from
   `/api/v1/nodes/<name>/proxy/configz` - read-only, no agent, works wherever
   kubectl works. This is the Q17 memory-ceiling refusal applied to CPU.
3. **Node PSI as a confounder control.** `analyze.go:425` already distinguishes
   "contention on the node" from "shortage of its own", but infers it from the
   container's own utilisation alone. `node_pressure_cpu_waiting_seconds_total`
   turns that inference into a measurement, and is the cheapest available shot
   at the open `resize-target` question (SATURATED at PSI 0.047 on 1.8m of CPU).
4. **Heterogeneity as a confidence penalty, not a normalisation.** A millicore
   on Graviton is not a millicore on a 2.0GHz Xeon, and there is no way to
   recover the ratio without running a benchmark, which tare will not do. The
   honest handling: if a workload's pods span mixed `kubernetes.io/arch` or
   `node.kubernetes.io/instance-type`, say so, size against the slowest class
   observed, and downgrade confidence. Do not invent a scalar.
5. **Feasibility guard.** A raised request above the largest node's allocatable
   makes the pod Pending. Trivial, obviously correct, and currently absent.

**Structural prerequisite for all five.** tare aggregates by workload owner, and
the pods of one Deployment land on different nodes. Pod-to-node mapping must be
carried through aggregation (`kube_pod_info{node=...}`) and a node inventory
added to `internal/discover`. That is the real work; items 1, 2 and 5 are cheap
once it exists, and the bin-packing replay above needs the same inventory.

### Occupancy, stated honestly

This document has twice had to retract a "nobody does this" claim, so the
assessment is recorded up front rather than discovered later:

| item | prior art |
| --- | --- |
| 1 | Solved per-process by `uber-go/automaxprocs` and JDK `UseContainerSupport`; PerfectScale's eBPF agent reads JVM internals. Cluster-wide detection is what is missing, which is narrower than novel. |
| 2 | No prior art found, OSS or commercial. The strongest of the five. |
| 3 | Downstream of tare's existing PSI-first bet rather than a second differentiator. |
| 4 | **Occupied commercially.** Hardware-normalised sizing is Densify's historical pitch and Cast.ai does instance-type selection. Unoccupied in OSS only. |
| 5 | Unremarkable; correctness, not differentiation. |

**The thing that compounds is the shape, not the hardware.** Items 1 and 2 are
refusals: they convert a sizing question into a configuration finding and
decline to give a number. That is the same move as the `-Xmx` rule, the
`LEAKING` verdict and the VPA `CONFLICT` verdict. A fifth instance of one
coherent rule - *tare knows when not to answer* - is worth more than a fifth
unrelated feature, and that is how these should be framed in the README.

## ML position, fourth pass (2026-09-19)

The question came back as "the ai learner can be a good tool, we should add it".
The answer splits three ways, and one third of it is now admitted to the plan.

**Not in the sizing decision, and the reason is identifiability rather than
caution.** Resource-to-latency curves are not recoverable from observational
data in which allocation never varied; a model fitted to it learns the
scheduler's past decisions, not the workload's requirement. No quantity of
training data repairs a non-identifiable parameter. The Landscape check
independently makes the commercial case: Cast.ai, StormForge, Densify and Sedai
all claim ML, none discloses an algorithm, and KRR at plain p95 is competitive
with all of them. A learner in the sizing path costs the one sentence that is
the product and buys nothing against the field.

**Admitted: ceiling extraction from unstructured configuration.** The corpus
finding is that a large share of real ceilings are not in the pod spec at all -
they live in image entrypoints, ConfigMaps and `envFrom`, where no parser
reaches. That is a verifiable-reward extraction task and not a decision task:
ground truth exists, `test/corpus` is already a 135-specification evaluation
set, `internal/ceilings` scores 25/32 on JVM today, and the output is a parsed
flag a human can check against the source. A wrong extraction is caught
downstream by the refusal rule, so the failure is bounded. Explainability
survives because the model's output is evidence, not a recommendation.

**Deferred: descent-endpoint prediction.** Predicting where the 20%-per-step
descent terminates converts weeks of convergence into days, is benchmarkable
against the naive loop, and is bounded by the trip wires. It requires the
actuation loop, which is v0.4 and does not exist. Unchanged from the original
roadmap.

**Honest scope of the win.** Extraction plausibly moves corpus coverage from
25/32 to around 30/32. That is worth having and it is not why anyone will adopt
the tool.

## Implementation pass (2026-09-19)

Shipped: `internal/packing`, `internal/discover/nodes.go` and
`internal/discover/pods.go`, the `IDLE` verdict, and the four node-aware
findings. 160 tests pass across 8 packages. Two claims written into this
document earlier the same day turned out to be wrong, and one design in it was
wrong. All three are recorded here rather than quietly fixed.

### Correction: the DaemonSet multiplier was already right

The claim was that `discover.go:159` treats a DaemonSet as just another owner
and that its per-node multiplier was missing. **The arithmetic was already
correct.** A DaemonSet runs one pod per node, so `Finding.Pods` *is* the node
count, and `reclaimable()` was already multiplying by it.

What was actually missing is smaller and still worth having: the reader cannot
distinguish "40 pods because there are 40 nodes" from "40 pods because someone
set replicas: 40", and the two call for opposite reactions. `perNodeWorkload`
in `internal/analyze/nodes.go` says which it is, and says the saving scales
with the cluster rather than with traffic. The genuinely new part is in the
packing model, where a DaemonSet is charged to every node as overhead instead
of being packed as cargo.

### Correction: `WorkloadKey` was not dead code

It was deleted as unreachable on the strength of `golang.org/x/tools/cmd/deadcode`,
which reports reachability **from main** and therefore calls every test-only
function dead. Tests failed immediately and it was restored. The lesson for the
next deletion pass: `deadcode` output is a list of candidates, not a verdict,
and the whole `internal/simulate` package appears in it.

Restoring it surfaced a real bug that had been introduced alongside: the
restored version keyed off the new `WorkloadKind` field, which the simulator
does not populate, so every constraint lookup silently missed and two
constraint tests failed. It now parses `Workload`, which every producer sets.
That is the project's recurring failure shape again - a lookup that returns a
clean empty answer instead of an error - caught this time only because the
tests were already there.

### Design change: the self-check was wrong as specified

This document specified: "replay a cluster's current placement with current
requests and require it to reproduce the current node count before any
counterfactual is believed." **That check fails on almost every real cluster**,
because a cluster that is merely not tightly packed replays onto fewer nodes
than it is running on, and that gap is the cluster's existing slack rather than
a model error.

The implemented check is the claim the model can actually be held to: **every
pod must fit on the node it is demonstrably running on**. A violation means the
capacity accounting is wrong - effective requests, pod overhead, or allocatable
read incorrectly - and the delta is withheld with the disagreement named.

This also forced a better output. There are three node counts, not two:

| figure | meaning |
| --- | --- |
| `NodesRunning` | nodes currently hosting at least one pod |
| `NodesPackedNow` | what today's requests fit onto |
| `NodesPackedRecommended` | what the recommendations fit onto |

`FromConsolidation` is the first minus the second: real, available today, and
**not tare's saving** - no request changes to get it. `FromResizing` is the
second minus the third, and is the only figure the recommendations earned.
Reporting one number would have credited tare with the cluster's pre-existing
slack, which is the same overclaim as reporting millicores, one level up.

### What the replay refuses to model

Pinning is deliberately broad, because an overstated saving is worse than no
number: nodeSelector, any affinity, topology spread, a non-default toleration,
extended resources, PVCs, hostPath, ephemeral volumes, a priority class, and
any pod not owned by a controller. Each pinned pod holds its current node, so
the reported figure is a **lower bound**, and the pin reasons are printed with
counts so a reader can see when the replay has measured almost nothing.

First-fit-decreasing, not an optimal packing. An optimal answer would promise a
consolidation no real cluster performs: Karpenter and the cluster autoscaler
are themselves greedy, and pods move one disruption budget at a time.

The bins are walked with the **already-conceded nodes first** - the ones a
pinned pod holds, which no packing can free - and only then largest-free-first.
Their leftover space is the only space in the cluster that costs nothing to
use. *(2026-09-21; see defect 22.)*

A DaemonSet is charged as per-node overhead rather than packed, **once per set**
and only on the nodes it runs on: the union of what its `nodeSelector` matches
and where its pods are observed. The union is deliberate - a node whose labels
were not read matches no selector - and the observed half is what keeps the
model from handing out capacity an agent is currently occupying.
*(2026-09-21; see defect 21.)*

### The IDLE verdict, and the archetype that nearly broke it

`IDLE` fires on three independent guards: **peak** CPU below 5m (not a
percentile - a nightly batch job is quiet at p95 and deleting it breaks
something), flat memory, and at least half the window actually observed. It
keeps the sizing numbers, because the smaller allocation is still right if the
workload turns out to be needed, and it prints what it cannot rule out: a hot
standby, a DR replica, a rare-event controller, a job whose period exceeds the
window.

Two archetypes were added to make this testable by discrimination rather than
assertion, in the pattern `HiddenCeilingJVM`/`VariableMemJVM` established:
`DormantSidecar` (0.4m, must fire) and `RareBatch` (0.6m baseline with 900x
spikes, must not). `IdleHoarder` at 5m stays on the WASTE side and is the
boundary case between the two.

`IdleCPUMillicores` is **unswept**. No sweep archetype exercises it yet, which
is exactly how `PlateauHeadroom` ended up unmeasured; it is a starting point,
not a defended value.

### A latent bug the IDLE work exposed

`printSummary` enumerated the verdicts to count by writing them out at the call
site, so `IDLE` would have been computed, attached to findings, and silently
missing from the summary a reader actually reads. The list now lives in
`analyze.ReportOrder()` beside the constants. Any verdict added later is
counted by construction.

### Deviation from the file layout above

`internal/nodes/` was specified; node and pod inventory went into
`internal/discover/nodes.go` and `internal/discover/pods.go` instead, because
reading the cluster API is exactly what `discover` is and they need its client.
`internal/packing` is its own package as specified, and is pure: it takes an
inventory and a pod list and touches no cluster.

## Live validation on the lab cluster (2026-09-19)

The long run was stopped and attempt 5's data archived to
`~/tare-longrun/attempt5-data/` on msi (88 snapshots, 793 rows,
2026-09-15 23:07 to 2026-09-19 13:17). The new code was then run against the
live k3d cluster with `--window 4d`.

**The node-count delta was verified by hand and is correct.** tare reported
3 nodes running, 2 needed for today's requests, 2 after the recommendations.
Checked independently: all three nodes have 16 cores and 15.4Gi allocatable,
total requests across the cluster are 3070m and 4.8Gi, so raw capacity would
fit everything on one node. The binding constraint is pinning, not capacity -
the five unmodellable pods sit on exactly two nodes (`coredns`,
`local-path-provisioner` and `metrics-server` by nodeSelector on
`server-0`, `traefik` by toleration on `agent-0`, and Prometheus by affinity
plus a PVC on `server-0`) - so two is the correct floor and `agent-1`'s seven
pods fold onto them.

**"0 nodes come from these recommendations" is the right answer here, and it is
the point.** 2.1 cores and 2.7Gi of reclaimable requests produce no node saving
on this cluster, which is precisely what the millicore headline would have
obscured.

### The IDLE verdict was wrong on first contact, in the usual way

It fired on **8 containers**, five of which had **no resource requests at all**:
`node-exporter`, `kube-state-metrics`, both Grafana sidecars and the Prometheus
`config-reloader`. Every one is load-bearing, and the report told the operator
the larger saving was to delete their metrics pipeline.

The guard added: **a container reserving nothing frees nothing by being
deleted.** IDLE now requires a CPU or memory request above the recommendation
floor. That drops it to exactly the three workloads the lab designed to be
idle - `overprovisioned`, `guaranteed-app` and `tomcat-lowxmx` - and the five
infrastructure sidecars fall to OK.

Worth noting how this was found: the simulator could not have found it, because
every simulated container has requests by construction. It took a real cluster
with real unconfigured sidecars, which is the same lesson as the load generator.

### The node-aware findings fired correctly, and one found something real

- **Thread-pool mismatch fired on `tomcat-lowxmx`**: a 1.00-core limit on a
  16-core node, a JVM image, and no `ActiveProcessorCount` anywhere in the spec.
  Genuine, and present in the lab since it was built.
- **Per-node framing fired on `node-exporter`**: "the 3 replicas are 3 nodes".
- **Mixed hardware stayed silent**, correctly: all three nodes are amd64 with no
  instance-type label, so there is one class and nothing to doubt.
- **The feasibility guard stayed silent**, correctly: nothing was recommended
  above 16 cores.

### Two pre-existing problems this run surfaced, neither from this pass

1. **`slow-leaker` still does not reach LEAKING**, and the cause was already
   known rather than newly discovered. It is SATURATED, and the evidence line
   says the memory-trend signal is unavailable, so the ratio test cannot fire.
   `MemEarly` for this container has been zero since the pod was replaced on
   2026-09-16: the early-window query offsets by `window-1h`, which always lands
   before the new pod existed, and it cannot recover until the window caps at 7d
   and `now-7d` clears the pod's start - about **2026-09-23**. The 4d window
   used for today's run is inside that dead zone by construction.

   What is new is only the confirmation that it persisted across all 88
   snapshots: `WASTEx59, SATURATEDx29`, memory recommendation swinging 16Mi to
   1174Mi at cv 0.43. The deeper issue behind it is recorded and still open: the
   MemEarly/MemLate pair can straddle two incarnations of a pod that OOMKills
   every ~41h, because observations are keyed by pod rather than container id.
   **Decide that before any further clock is started**, and fix it against
   `attempt5-data/history.csv` rather than by collecting more.
2. ~~**`diurnal-web` recommendation has cv 0.18**, above the 0.15 the long run
   defines as failure, across 88 snapshots. The load generator is working this
   time (94-335m tracks the sine), so this is the recommender chasing the daily
   curve rather than a dead instrument.~~

   **Retracted the same day. This was wrong, and it was the instrument again.**
   `stability.py` computed the coefficient of variation across snapshots whose
   windows ranged from 6h to 86h, and a recommendation taken through a 6h
   window is not the same quantity as one taken through an 86h window. The
   window grows with elapsed run time, so the aggregate measured the window
   widening rather than the recommender moving. Segmented by window width:

   | window | n | recCPU | cv |
   | --- | --- | --- | --- |
   | 0-24h | 25 | 94-335m | 0.319 |
   | 24-48h | 24 | 318-330m | 0.010 |
   | 48-72h | 24 | 323-334m | 0.013 |
   | 72h+ | 15 | 328-333m | **0.004** |

   Once the window covers a whole diurnal cycle the recommendation is stable to
   within 5m, which is the recommender working exactly as designed.
   `load-generator` moves the same way: cv 0.18 across all windows, **cv 0.00**
   (260-265m) across the settled ones. `stability.py` now excludes snapshots
   below a 48h window and says how many it dropped.

Attempt 5 therefore failed **one** of its own stated criteria, not two: the
leak detector, for a cause already diagnosed and unrelated to the recommender.

**On its central question the answer is affirmative for the first time in five
attempts.** Does the same workload get the same recommendation on day 3, day 7
and day 14? Across the settled window, `diurnal-web` holds 323-334m at cv 0.01,
`load-generator` 260-265m at cv 0.00, `cpu-hog` a flat 115m, `resize-target` a
flat 328Mi, and confidence is uniformly `high`. That is the property the whole
experiment exists to measure, and it holds.

## Pivot: self-hosted inference serving (2026-09-19)

Direction set by Marco: point tare at LLM inference on Kubernetes - vLLM,
Ollama, TGI, llama.cpp - rather than at general web workloads. First pass is
shipped and verified on the lab cluster. 206 tests across 8 packages.

### Why it fits, stated honestly

**What transfers, and is stronger here than in the original domain.** This
document's central rule is "report the flag, refuse to size below it". For an
inference server that rule is not one finding among several, it is the entire
subject. vLLM started with `--gpu-memory-utilization 0.9` claims 90% of the
device at boot and holds it for the KV cache whether or not a request ever
arrives, so its observed GPU memory is a **constant that describes the flag**.
Usage-based sizing is not imprecise here, it is meaningless. `-Xmx` was a
special case; this is the general one.

The `IDLE` verdict also changes in value rather than in code: an idle 100m CPU
request is noise, an idle accelerator is a dollar an hour or more. And the node
awareness built earlier today stops being optional, because the same manifest
means different things on different hardware - `--gpu-memory-utilization 0.9`
is 21.6GiB on a 24GB card and 5.4GiB on a 6GB one.

**What does not transfer.** The saturation gradient, which this document calls
the core mechanism, is largely dead: PSI and CFS throttling measure CPU
contention and GPU work does not appear in them. The replacement signals are
DCGM utilisation and vLLM's own queue depth (`vllm:num_requests_waiting`
against `num_requests_running`), neither of which is wired up yet. The
asymmetric descent loop has **no analogue at all**: `nvidia.com/gpu` is an
integer extended resource, so there is no smaller number to recommend. MIG and
time-slicing are node configuration, not pod requests.

**Consequence for the product's shape:** for this class of workload tare stops
being a right-sizer and becomes a **fit checker and configuration linter**.
Every finding is a refusal or a defect; none of them changes `RecCPU` or
`RecMem`. That is a smaller claim than "right-sizes your cluster" and a more
defensible one.

### What shipped

`internal/ceilings/inference.go` parses vLLM, Ollama, TGI and llama.cpp:

| flag | why it is a finding |
| --- | --- |
| `--gpu-memory-utilization` / `--cuda-memory-fraction` | the reservation, reported even when it comes from the default, because a manifest with no flag still claims 90% |
| `--model` / `--model-id` | parameter count read from the identifier, times bytes-per-parameter from `--dtype`/`--quantization`, divided by tensor-parallel shards |
| `--max-model-len` x `--max-num-seqs` | the KV cache budget, **in tokens only** |
| `--swap-space` | host memory, not VRAM, charged to the container limit |
| `OLLAMA_KEEP_ALIVE=-1` | pins every model in VRAM forever; the most expensive line in a self-hosted manifest |
| `OLLAMA_MAX_LOADED_MODELS` | peak VRAM is the sum of the N largest, not the largest |
| missing `-ngl` | llama.cpp runs every layer on the CPU while holding an allocated device |

`internal/analyze/inference.go` turns those into fit checks against the device,
using GPU fields added to the node inventory (`nvidia.com/gpu` capacity, plus
`nvidia.com/gpu.product` and `nvidia.com/gpu.memory` from the GPU Operator).

### Two places it deliberately refuses to compute

1. **KV cache bytes.** Exact bytes per token need the model's layer count, KV
   head count and head dimension, which live in its `config.json` and not in
   the pod spec. The token budget is reported and the bytes are not. Inventing
   a per-token figure would produce a fit check that passes on a configuration
   that cannot start, which is worse than no check.
2. **Unreadable model identifiers.** `paramCount` reads a size out of a Hugging
   Face identifier and refuses on anything it cannot parse, including version
   numbers (`Llama-3.1` is not 3.1 billion parameters) and organisation names.
   `internal/ceilings/inference_test.go` tests fourteen identifiers, five of
   which must return nothing.

### Verified on the lab cluster, against pods that never started

`test/lab/inference.yaml` plants three workloads on msi. Every finding below was
produced while the vLLM pod sat in `ImagePullBackOff`, which is the claim in
executable form - the tool reads the configuration, not the behaviour:

- 14.9GiB of weights (8B at fp16, inferred precision stated as an assumption)
- the 90% device reservation, with "the number will not move however little
  traffic it serves"
- a KV budget of 256 sequences x 32768 tokens = 8.4M tokens
- `--swap-space 8Gi` correctly routed to the **container memory floor**, not VRAM
- `OLLAMA_KEEP_ALIVE=-1` and `MAX_LOADED_MODELS=3` on the Ollama pod
- a missing `-ngl` on the llama.cpp pod
- and on all three, "configured as an inference server but requests no GPU"

The lab cluster has no GPU exposed to k3d, so the fit check itself is covered
only by unit tests. msi does have an RTX 3060 (6GB); exposing it needs the
NVIDIA container runtime and device plugin, and that is the next honest step,
because a 6GB card refusing an 8B fp16 model is exactly the refusal this pass
was written to produce.

### One defect found by running it

The weight figure rendered as `15625000Ki`. `humanBytes` is exact so that a
Kubernetes quantity round-trips, and 16e9 bytes is not a whole number of
mebibytes. VRAM is never a Kubernetes quantity - a GPU is allocated whole - so
those figures now use `humanVRAM` and read `14.9GiB`. Unit tests passed
throughout; only running it against a cluster showed the output.

## Refocus: optimising a fixed node pool (2026-09-19, third pass)

Direction from Marco after the inference pass: rethink around the hardware
actually available, and make tare useful to someone who wants to optimise the
cluster they have. That is the right correction, for a reason this document
had already established and then walked past.

**The lab is a fixed, homogeneous, three-node pool with no autoscaler, and that
is the shape where the node delta is honestly zero.** Every measurement today
confirmed it: 4.5 cores and 5.6Gi reclaimable, 0 nodes freed. Building further
on a headline figure that reports zero on the only cluster we can test was the
wrong direction, whatever its intellectual merit.

**So what is available on a fixed pool?** Not money. Headroom and the absence
of a 3am stall. Two findings deliver that, neither of them visible in a
per-container sizing table, and both fully testable on the hardware we have.

### 1. The manifest does not say what its author thinks

Kubernetes **copies an omitted request from the limit**. A manifest reading

    resources:
      limits: {cpu: "2", memory: 1Gi}

does not cap a burst at two cores. It reserves two cores and 1Gi on a node for
every replica, permanently, and makes the pod Guaranteed. Confirmed against
the live API server, twice: a server-side dry-run returns
`requests: {cpu: "2", memory: 1Gi}` and `qosClass: Guaranteed`.

A running pod cannot be asked about this, because the copied value is
indistinguishable from a chosen one. **The controller's pod template still
holds the omission** - also verified live: the Deployment shows
`{"limits":{"cpu":"2","memory":"1Gi"}}` while its pod shows both. Comparing the
two is the only way to detect it, and `internal/discover/authored.go` now does,
for Deployment, StatefulSet, DaemonSet, ReplicaSet, Job and CronJob, cached per
workload so one template is not fetched once per pod.

Three findings come out of it: the CPU copy, the memory copy, and **Guaranteed
by accident**. The last one matters beyond the waste, because tare's existing
QoS rule then holds requests equal to limits to preserve a class nobody chose,
so the available reduction is larger than the report shows and taking it is a
decision rather than an edit.

Honest limits: a LimitRange that defaults a pod looks identical to an author
who wrote the values, a workload owned by a CRD (an Argo Rollout, say) has no
template tare can read, and `Authored.Known` is false in both cases so nothing
is claimed.

### 2. A node can be under-requested and over-limited at once

The scheduler reads requests. It does not read limits. So a node can look 85%
free and be promised away twice over, and a right-sizing report that only
shrinks requests **makes this worse** by inviting more pods onto it.

`internal/packing/commitment.go` prints both numbers per node. Measured on the
lab cluster after planting one deliberately overcommitted Deployment:

| node | pods | CPU req | CPU lim | MEM req | MEM lim |
| --- | --- | --- | --- | --- | --- |
| server-0 | 13 | 17% | **197%** | 19% | **118%** |
| agent-1 | 10 | 19% | **144%** | 32% | **100%** |
| agent-0 | 10 | 8% | **120%** | 32% | **104%** |

Memory overcommit is called out separately from CPU because the consequences
differ in kind: CPU overcommit throttles, memory overcommit means the kernel
picks a process and kills it. Containers with no limit at all are counted per
node and the totals declared a floor, because an unbounded container cannot be
summed and presenting the ratio as complete would be the usual confident wrong
answer.

### Where this leaves the earlier passes

- **The node delta stays.** It is correct, it is verified by hand, and on a
  cluster with Karpenter or the cluster autoscaler it is non-zero. On a fixed
  pool it prints zero, which is the honest answer and now sits beside two
  findings that are not zero.
- **The inference work stays as a runtime family, not as the product.** It is
  built, tested and costs nothing: vLLM, Ollama, TGI and llama.cpp are simply
  more entries in `internal/ceilings`, the same way the JVM is. What is dropped
  is the idea of *devoting* the tool to them, because the only accelerator
  available is a 6GB laptop GPU that is not exposed to the cluster.
- **Nothing was deleted.** The pivot cost was one package of parsers that earn
  their place on any cluster that happens to run an inference server.

### The through-line, finally stated

Every finding tare produces is now one sentence: **the cluster is not what its
manifests say it is.** A JVM whose heap is fixed below its limit. A recommendation
in millicores that frees no node. A DaemonSet whose replica count is a node
count. A runtime sizing its thread pool from the wrong core count. A request
nobody wrote. A node the scheduler believes is empty. That is a tool with a
subject, rather than a right-sizer with features bolted on.

## Drain blockers, and a retraction (2026-09-19, fourth pass)

### The recommender is stable. The instrument said otherwise.

`stability.py` was computing the coefficient of variation across snapshots
whose windows ranged from 6h to 86h. The window grows with elapsed run time, so
the aggregate measured the window widening rather than the recommendation
moving, and attempt 5 was written up as failing a criterion it passes
comfortably. Segmented by window width, `diurnal-web` runs cv 0.319 below 24h
and **cv 0.004** above 72h; `load-generator` is **cv 0.00**. The script now
drops snapshots under a 48h window and prints how many it excluded.

**The correction matters more than the fix.** Across five attempts this is the
first affirmative answer to the experiment's central question - the same
workload does get the same recommendation on day 3 and day 4 - and it was
nearly recorded as a failure. It is also the nineteenth instance of the same
bug shape, this time in the analysis script rather than the tool: a clean,
plausible number computed over data that could not support it.

The general rule this yields, worth applying beyond `stability.py`: **a series
whose measurement window changes is not a series.** Before computing any
statistic over time, check that every point was measured the same way.

### A node-count saving the cluster will refuse is not a saving

The delta computed in the second pass assumed any unpinned pod could move. It
cannot if a PodDisruptionBudget forbids evicting it, and `minAvailable` equal to
the replica count - which reads as prudence and is far more common in the wild
than `maxUnavailable: 0` - forbids it permanently.

`internal/discover/pdb.go` reads the budgets and uses
`status.disruptionsAllowed`, the controller's own live computation, rather than
inferring from `minAvailable` against a replica count. It separates three
cases, because they need different sentences: `maxUnavailable: 0` (permanent,
fix the config), every pod required healthy (permanent, raise replicas or lower
minAvailable), and a budget that is merely tight today because a pod is
unhealthy (transient, resolves itself). Telling an operator to change a config
that is about to fix itself is its own kind of wrong answer.

Measured on the lab cluster by planting one `minAvailable: 4` budget over a
4-replica Deployment:

| | before the PDB | after |
| --- | --- | --- |
| nodes needed today | 2 | **3** |
| freed by consolidation | 1 | **0** |
| pods pinned | 5 of 22 | **9 of 27** |

The previous answer was optimistic, and the cluster would have refused every
drain that made it true. This is the same correction as reporting nodes instead
of millicores, applied one level further down: a number is only a saving if the
thing it describes can actually be done.

## Throttled while the node was idle (2026-09-19, fifth pass)

Chosen after a research pass over the state of the field in September 2026. The
numbers that set the direction:

- 70% of requested CPU and memory is never used; the median cluster runs at 10%
  CPU and 23% memory utilisation.
- **Fewer than 1% of organisations run VPA in full automation.** The named
  barriers, in order: no transparency in recommendations, eviction and
  disruption, **HPA conflict**, and staleness.
- 68% of enterprises report Kubernetes costs partially or entirely invisible in
  their FinOps tooling; 13% do showback.
- CPU throttling on an idle node is **still actively contested in 2026**, argued
  on both sides from anecdote.

The first three barriers are ones tare already addresses, which is a useful
independent check on the Q14 positioning rather than a new direction. The fourth
item is the gap: a question the field argues about and nobody measures per
workload.

### What it does

A container with a CPU limit is throttled by CFS at its quota regardless of what
the node is doing, so it can stall repeatedly a few microseconds from idle
cores. tare now states, per workload: how much it was throttled, how busy its
node actually was over the same window, and therefore that the constraint was
the limit rather than the machine.

It says **nothing about latency**. tare has no SLI signal until v0.3, and a
figure like "this cost you 180ms of p99" would be invented. A test asserts the
finding text contains no "p99", "latency" or "ms".

### The join, which is the whole technical problem

Node metrics are keyed by node; observations are keyed by
(namespace, pod, container). Worse, **node-exporter carries no node label at
all**: verified on the lab cluster, its series are labelled
`instance="172.18.0.3:9100"` and nothing more.

Two things follow, both verified against the live cluster before any code was
written:

1. **The join is done in Go, not PromQL.** `discover.Node` gained `InternalIP`
   from `node.status.addresses`, the metrics layer returns utilisation keyed by
   bare IP, and `cmd` resolves IP to node name once. The alternative - a PromQL
   `label_replace` matching node-exporter's `instance` against
   `kube_node_info`'s `internal_ip` - hardcodes a `:9100` port and adds a second
   thing that can silently mis-join.
2. **Node utilisation is an optional capability, not a `Source` method.**
   `metrics.NodeObserver` is type-asserted, exactly like the existing
   `WindowProber`. Widening `Source` would force a stub into `internal/simulate`,
   which has no node model, and a stub returning a plausible zero is the worst
   possible failure here: an unmeasured node and an idle node look identical in
   a float, so the stub would rubber-stamp "the node had room" on every
   simulated workload.

### Two gates, and the statistic took two attempts to get right

The shipped rule needs both: **mean below `NodeIdleCeiling` (0.50)** as the
primary gate, and **p95 below `NodeBusyP95` (0.85)** as a guard. The mean
catches a node busy throughout; the p95 catches one that averaged low and was
saturated for the hours that mattered, which tare cannot correlate in time.
Both are **unswept** and say so at their definition: the simulator models no
node at all, so no archetype reaches this path. `PlateauHeadroom` is the
precedent for what happens when that is not written down.

**The first version got both of these wrong, and only running it showed either.**

First it gated on the peak *alone*, below 50%. Wrong over a real window: one
busy minute in a fortnight disqualifies a node idle the rest of the time.

Then it gated on mean plus **maximum**, which is the same error wearing a
different hat, and the live negative control proved it by accident. Creating a
busy node to test the negative path meant running a seven-minute load test, and
afterwards the positive case stopped firing and stayed dead. Measured over the
four days containing that test:

| statistic | value |
| --- | --- |
| max | **99.9%** |
| p95 | **7.7%** |
| mean | 5.9% |

A maximum lets any single spike describe a node as saturated for the whole
window, so the finding silently vanished on a cluster that was idle 99.9% of the
time. The gate is a 95th percentile now, and `CPUUtilPeak` was renamed
`CPUUtilP95` so the mistake cannot quietly return. The lesson generalises past
this feature: **over a long window, a maximum is a statement about one moment,
not about the window.**

### Verified both ways on the lab cluster

The negative control had to be created, because the lab had no busy node. With
three burner replicas driving all nodes to **93.6% mean**, `cpu-hog` kept its
`SATURATED` verdict and its 100% throttle ratio and **lost the idle-node
finding**, which is exactly right. With the burner removed and a four-day window
that still contains the burn, the finding returns, quoting mean 5.9% and p95
7.7%.

### A defect the tests caught immediately

`NodeIdleCeiling` was declared and its default never set, so it was `0`, and
`peak >= 0` withheld the finding on every workload. The report would have been
silent, which is indistinguishable from "no idle nodes were found" - the
project's signature failure, attempt twenty-one. Two tests failed on the first
run. `TestNodeIdleCeilingHasANonZeroDefault` now guards both thresholds, because
a zero here disables a feature without erroring.

### Findings on the lab cluster

`cpu-hog`, throttled 100.0% of scheduling periods against a 100m limit while its
node averaged 5.9% CPU. `slow-leaker` at 8.9%. Both produce the full finding
with the commitment-table caveat attached, because "remove your CPU limits" is
only sound advice next to the overcommit picture, and tare is the only tool
holding both numbers.

**A lab limitation worth recording:** the three k3d nodes are containers on one
16-core host, so all three node-exporters report the same host CPU - earlier
readings were 0.9 cores and 7.8% on all three, identical to the decimal. The lab
can exercise the busy-versus-idle gate but **cannot** validate per-node
discrimination. The mixed idle/busy case is covered by unit tests only, and a
real multi-machine cluster is needed to confirm it.

## The reviewable diff (2026-09-19, sixth pass)

The persona this tool is for is a platform engineer who owns the cluster and
**not** the workloads on it. Every finding is about somebody else's manifest, so
the person who runs tare cannot fix what it finds. A table cannot be approved by
the team that can, and `kubectl patch` commands leave nothing behind in the
repository that is the source of truth. `kubectl tare diff --repo <checkout>`
writes the change where it can be reviewed, commented on and merged.

### Sidestepping the problem that stops everyone else

The known barrier, named in the September 2026 research, is that
recommendations describe *rendered* objects while the edit has to land in a Helm
or Kustomize *source*, and mapping one to the other is hard. That problem is
Helm-specific:

- **Plain manifests** are edited where they sit, matched by kind, name and
  namespace. Deterministic.
- **Kustomize needs no mapping at all.** A strategic-merge patch addresses the
  object by kind and name, which is exactly what a finding already carries.
- **Helm is refused, not guessed at.** Any file containing `{{` is skipped and
  reported as unmatched, because a template is not YAML until it is rendered and
  editing one produces a corrupt file.

### Two defects, both found by running it against a real repository

**1. Re-encoding destroyed the diff.** The first editor decoded to `yaml.Node`,
edited, and re-encoded. `yaml.Node` preserves comments and key order, so this
looked right and passed its unit tests. On a real manifest it reindented every
sequence: `diurnal-web.yaml` came back with **117 changed lines for a
four-value edit**. A reviewer rejects that without reading it, and being read is
the entire point of the output.

Replaced with textual splicing: the tree is parsed only to *locate* values, and
the bytes are edited in place. The same file now produces **8 changed lines**,
which is the four values before and after. `TestOnlyChangedLinesDiffer` asserts
the line count cannot move when no key is added.

**2. Flow-style mappings were corrupted.** The value token was taken as running
to the end of the line or to a trailing comment. On
`limits: {cpu: "2", memory: 1Gi}` that swallows the closing brace and writes
YAML that no longer parses - the worst thing a tool that edits your repository
can do. Real manifests use both styles and the lab's did; no unit test had.

Two fixes, because one was not enough. `valueToken` now terminates on `,`, `}`,
`]` and quotes as well as comments. And every edited file is **re-parsed before
it is written**: a future tokenising mistake now produces a skipped file with a
reason instead of a corrupted one.

### What the reviewer actually gets

The diff is the small part. `--pr-body` writes the evidence - the throttle
ratios, the pressure figures, the ceiling refusals, the node utilisation - which
is the only part a reviewer cannot reconstruct from the diff itself and the
reason they say yes rather than "who are you and why are you changing my
Deployment". Findings tare will not defend are listed at the end with the
reason, so a diff that covers half the report says so.

`analyze.Patchable` was extracted so the patch script and the diff cannot drift
apart on the question of which numbers tare stands behind. It also refuses to
write a change that would silently demote a Guaranteed pod to Burstable.

### Verified end to end

A git repository was built on msi from the live lab manifests, `tare diff
--write` was run against the cluster, and the result inspected with `git diff`:
5 files changed, 28 insertions, 18 deletions, every file still parsing, comments
and inline notes intact, and the BestEffort pod correctly gaining a resources
block it never had.

## nodeSelector, and what the replay was getting wrong (2026-09-21, seventh pass)

The live cluster's shapes came in three parts: containers with no requests,
workloads with no limits, and **pods with a `nodeSelector`**. The first two are
declaration shapes and change verdicts, so they went into the simulator's
catalogue and into `test/lab/shapes.yaml`. The third changes *packing* and
nothing else - a selector never alters what a container should be sized to - so
it belongs in `internal/packing`, against an inventory rather than a workload
population. Writing that test found two defects in the number that corresponds
to an invoice.

**A DaemonSet was charged to every node, once per pod of the set** (defect 21).
Three nodes meant three node-exporters billed to each of the three; thirty
nodes, thirty. The same loop charged a `nodeSelector` DaemonSet to nodes it can
never run on. Both errors are pessimistic, which is why nothing caught them: the
saving only ever came out too small, and too small reads as caution.

**The replay would not pack onto nodes it had already conceded** (defect 22).
Largest-free-first tried the node held by a pinned pod last, so the model kept a
second node alive rather than use the room it had already paid for. With a pin
list this broad - the lab pins six pods by `nodeSelector` alone, seventeen of
fifty-three in total - that is not an edge case.

`discover.Pod` now carries `NodeSelector`, `discover.Node` carries its labels as
a map with a `Matches` method that is `nodeSelector` semantics and nothing more,
and the pod translation moved into `podFrom` so that it can be tested without a
cluster. Every new test was run against a reverted fix and confirmed red, one
fix at a time.

### Applying the shapes found two more, and they were not about shapes

The manifest went onto the cluster at 09:43 and the new DaemonSet came up with
`DESIRED 1` on a three-node cluster, every new pod on the server node. Both
agents had been unreachable since 08:03 and the API was still listing seventeen
pods as `Running` on them - **including in the tare report produced at 09:25**,
which counted three live nodes and printed commitment percentages for two dead
ones. `Schedulable` was `!spec.unschedulable` and nothing more, so an
unreachable node was a destination with full allocatable capacity (defect 23).
Looking for what else the node inventory knew and never used turned up
`Taints`, populated from the first version and read by no code path, so a node
tainted `dedicated=gpu:NoSchedule` was offered as free space - and, being large,
offered first (defect 24).

The replay now asks three questions of a node before using it as a destination:
is it cordoned, is it Ready, and does it carry a taint no movable pod tolerates.
Pods on a node that has stopped reporting are held with a reason that says the
pod may not be running at all. The same run then reads:

    35 of 56 pods could not be modelled as movable and held their node
        18  its node has stopped reporting, so the pod may not be running at all
    2 node(s) could not be used as destinations at all:
      - k3d-tare-test-agent-0: the kubelet has stopped reporting

Readiness has the property that makes this list what it is: a kubelet that stops
reporting leaves the condition at **Unknown**, not False, so every natural
spelling of the check reads a dead node as healthy.

**On the lab cluster the answer does not move:** 3 nodes running, 3 needed now,
3 after the recommendations, both before and after the change. Seventeen of
fifty-three pods are pinned across all three nodes, so the count is decided by
pinning and neither defect can reach it. That is worth stating rather than
hiding - the evidence for these two fixes is the unit tests, not the cluster.
(The same run reported 3/2/2 on 2026-09-19. The difference is
`overcommitter-pdb`, which currently allows no disruption and pins eight more
pods, and now the two unreachable nodes; both are cluster state, not a
regression. The 3/3/3 is also not a *correct* answer to a question worth
asking - two of those nodes are dead - which is why the lines beneath it name
them.)

## v0.1 scope: the day-1 report, and nothing else

One binary, one command, no install into the cluster, no CRD, no mutation:

```
kubectl tare report [-n ns] [--prometheus URL] [-o table|json|yaml]
```

It reads the cluster API and an existing Prometheus-compatible endpoint and prints,
per container:

1. **Request vs usage**: p50/p95/p99/max over the queried window, with the window
   and sample coverage stated.
2. **Throttling hotspots**: `throttled_periods / periods`, ranked. This is the
   headline finding and the thing competitors bury.
3. **OOMKill history**: from `last_terminated_reason`, with restart counts.
4. **Detected runtime ceilings**: `-Xmx`, `-XX:MaxRAMPercentage`, `GOMEMLIMIT`,
   `GOMAXPROCS`, `--max-old-space-size`, gunicorn/uvicorn worker counts, parsed
   from container command, args and env.
5. **A confidence-tagged recommendation**: low/medium/high based on observation
   span, sample coverage, and how much natural variation in allocation was seen.
   Any recommendation blocked by a runtime ceiling reports the ceiling instead.
6. **A savings summary in nodes** *(added 2026-09-19)*: the node-count delta a
   bin-packing replay produces from the recommendations, with the constraints it
   honoured and the nodes it had to exclude. Reclaimed millicores and bytes are
   reported as the input to that figure, never as the figure.
7. **Per-node workloads ranked by node count** *(added 2026-09-19)*: DaemonSets
   multiplied by nodes rather than replicas.
8. **An IDLE verdict** *(added 2026-09-19)*: near-zero CPU and no traffic across
   the window recommends deletion, not a smaller number.

Explicitly **not** in v0.1: eBPF agent, CRD, PR bot, webhook, any mutation, any SLO
logic beyond reporting p99 from whatever latency metric already exists.

## Implementation

New Go module (`go 1.23+`), single binary named for kubectl plugin discovery.

```
cmd/kubectl-tare/main.go          cobra CLI; report subcommand
internal/discover/                workload + container inventory via client-go
internal/metrics/                 Source interface + Prometheus impl
internal/ceilings/                runtime flag parsers (one per runtime)
internal/analyze/                 decaying histograms, throttle ratio, evidence,
                                  confidence scoring, recommendation synthesis
internal/report/                  table / json / yaml renderers
internal/simulate/                synthetic workload generator (see Verification)
internal/discover/nodes.go        node inventory, allocatable, node classes
internal/discover/pods.go         effective pod requests, pin reasons
internal/packing/                 bin-packing replay: node-count delta under
                                  scheduling constraints     (added 2026-09-19)
```

Key dependencies: `k8s.io/client-go`, `sigs.k8s.io/controller-runtime` (client only,
for v0.2 continuity), `github.com/prometheus/client_golang/api/v1`,
`github.com/spf13/cobra`.

### Notes on the pieces

- **`internal/metrics`**: define the `Source` interface first and keep the
  Prometheus implementation behind it. v0.2 adds a Thanos/Mimir-aware
  implementation and a no-Prometheus degraded mode; the analyzer must never know
  which it is talking to.
- **Queries**: `container_pressure_cpu_waiting_seconds_total` and
  `container_pressure_cpu_stalled_seconds_total` (the primary saturation signal),
  `container_cpu_usage_seconds_total`,
  `container_cpu_cfs_throttled_periods_total` over `container_cpu_cfs_periods_total`
  (optional enrichment, absent on some clusters),
  `container_memory_working_set_bytes`, `container_memory_failures_total`
  (major faults), `kube_pod_container_status_last_terminated_reason`,
  `kube_pod_container_resource_requests|limits`. Kubelet PSI metrics where the
  cluster exposes them, treated as optional enrichment and never a hard dependency.
  Aggregate by workload owner (ReplicaSet to Deployment), not by pod.
- **`internal/ceilings`**: small, well-tested parsers. This is the highest
  value-per-line code in v0.1 and the clearest differentiator; make each runtime a
  separate file with table-driven tests over real-world flag spellings.
- **`internal/analyze`**: exponentially-decaying histograms. Every parameter must
  be visible in the output, not hidden. Encode the VPA recommender defaults (24h
  half-life, p90 target, p50/p95 bounds, +15% margin, 25m/250MB floors) as a named
  baseline profile so the report can show "VPA would say X, we say Y, here is why."
  That comparison is cheap to build and is the single most persuasive artifact in
  the tool. Confidence is a first-class field on every recommendation, never an
  afterthought, and matters more than expected: most clusters retain only ~15 days
  of Prometheus data.

## Verification

1. **Simulator-driven tests (primary).** For v0.1, `internal/simulate` only needs
   to emit synthetic metric series with known statistics: daily and weekly
   periodicity, throttle episodes, deploy events, traffic spikes. Roughly a day of
   work, and it is what makes the analyzer's percentiles and confidence scoring
   testable rather than eyeballed. Analyzer tests assert that recommendations
   converge to the known-correct size and that confidence rises with observation
   span. The full closed-loop version, where the simulated workload *responds* to
   resource changes, arrives with the descent loop in v0.4. Put it in this package
   from the start so it grows in place instead of being rewritten.
2. **Ceiling parser unit tests.** Table-driven over real container specs from
   public Helm charts (JVM, Go, Node, Python).
3. **End-to-end on kind.** `kind` cluster plus `kube-prometheus-stack` plus two
   planted workloads: one CPU-throttled at a low limit, one with `-Xmx` well below
   its memory limit. Assert the report ranks the throttled one first and reports
   the ceiling rather than a reduction for the second.
4. **Manual smoke against a real cluster.** Run against a live cluster read-only
   and check the findings against what the platform team already believes about
   those workloads: the fastest way to find embarrassing bugs. **Still not done
   as of 2026-09-19**, and it is now item 5 of "Where to pick up": every
   measurement in this document was taken on a cluster we built ourselves.
5. **Bin-packing replay validated against the scheduler, not against itself**
   *(added 2026-09-19, revised the same day)*. The original wording required the
   replay to reproduce the current node count, which fails on every cluster that
   is merely not tightly packed. The implemented check is that **every pod fits
   the node it is running on**; see "Design change" under the implementation
   pass. Still outstanding: run it against a cluster with real affinity and
   topology rules and check how much of it ends up pinned. A replay that pins
   90% of the cluster is honest and useless, and only a real cluster will say
   which it is.

## Usable on a cluster nobody here owns (2026-09-21, eighth pass)

Everything to this point was measured against the k3d lab cluster, which has
thirty pods, an admin kubeconfig and a Prometheus we installed ourselves. Asked
what a stranger's first run would look like, the answer was: a broken install
command, a timeout, a Kubernetes RBAC error, or a report that looked complete and
was not. Six changes, in the order they matter.

**1. API calls no longer scale with pod count.** `ownerOf` fetched a ReplicaSet
per pod, uncached, and the packing replay did it again. client-go's default rate
limiter is 5 QPS and the run was capped at 3 minutes, so ~900 pods exhausted the
budget inside the client before Prometheus was ever queried. Replaced by
`workloadIndex`: one List per controller kind per namespace scope, shared
between the sizing pass and the replay, QPS/Burst raised to 50/100. A test
asserts that ten pods and a thousand pods cost the same number of API calls.
Recorded as defect 25. ReplicaSets owned by a Deployment contribute only their
owner reference and not their pod template, because a cluster keeps ten dead
ReplicaSets per Deployment and their templates are not the ones that matter.

**2. Every denied read degrades and says so.** A denied HPA list aborted the run;
a denied PDB list was skipped in silence, which overstates the node-count saving;
a denied VPA list was indistinguishable from the CRD not being installed, which
is the difference between "no VPA" and "a VPA is already writing these fields".
All three now produce a header line naming what was lost, in every output
format, and only the pod list is fatal. Defect 26. The header is the right place
rather than stderr, because the person reading an HTML report is not the person
who ran the command.

**3. `kubectl tare check`.** A preflight that answers, before a report exists,
what this cluster can tell the tool: server version, pod visibility, thirteen
permissions via SelfSubjectAccessReview, endpoint discovery and reachability,
history available, the nine metric families, and coverage. Every line states
what its absence costs. The report has always said what it could not see, but
only after doing all the work and only about signals it got as far as querying;
the failures that stop a first run are further down than that.

**4. Coverage is now measured and printed.** The observation map is filled by
fifteen queries, two of which come from kube-state-metrics and exist for every
container in the cluster, so a container whose kubelet never reported still
arrived with an entry and nothing in it. Per container the verdict was already
safe (`NO-DATA` on zero samples), but nothing counted them, and a Prometheus
scraping two namespaces out of forty produced a report that looked complete.
Counted now on samples rather than on map entries, in `check` and in every
report format. On the lab cluster it immediately read "metrics cover 50 of 66
containers", which was true and had never been said. Defect 27.

**5. First-contact errors carry the instruction.** A missing kubeconfig produced
client-go's suggestion to set `KUBERNETES_MASTER`, an environment variable
nothing has used since 2015, with no mention of `--kubeconfig`, `--context` or
`--demo`. A cluster-wide pod list refused by RBAC now says to name namespaces
with `-n`; a refused service list says to pass `--prometheus`; a timeout names
both knobs. `--timeout` is a flag with a 10-minute default rather than a
hardcoded 3 minutes, and it now reaches the Prometheus queries, which previously
ran on `context.Background()` and could hang forever against a backend that
accepts a connection and never answers.

**6. Identity and distribution.** `kubectl tare version`, stamped by
`tools/build.sh` with a date and a hash of the sources, because binaries get
passed around by hand and "which build produced this number" had no answer. The
README's `go install ...@latest` was verified against proxy.golang.org: 404, the
module has never been published, so the first command in the documentation could
not work. Replaced with the build script. `deploy/rbac.yaml` is the exact read
set, verified end to end by applying it, minting a token and running both `check`
and a full report as that service account.

**Also added:** `--redact`, which hashes every namespace, workload, container and
node name in the columns *and* in the evidence text, for sending a report outside
the organisation. Token-based replacement rather than substring, so a namespace
called "sim" does not turn "simulator" into a hash followed by "ulator". The hash
is stable, not secret, and the flag help says so.

**What this pass did not fix.** A container whose metrics stopped mid-window is
still sized from stale data with nothing saying the data is old; confidence falls
only because the sample count falls. Listed under "Open" in KNOWN-FAILURES.

## The report as data (2026-09-22, ninth pass)

The report was a table wrapped in prose. Everything the run established that was
not a resource number lived in a sentence: the summary, the node block, the
commitment notes, and under `-v` a bulleted essay per container. All of it was
true and none of it could be piped anywhere.

The defect this hid is the familiar one. A consumer that wanted "was this
container OOMKilled" had to substring-match prose that no test pinned. Rewording
a sentence broke that filter silently, and a filter that silently matches nothing
is indistinguishable from a healthy cluster — the same shape as every serious bug
in this tree.

**A reason is now a record, not a string.** `analyze.Reason` carries a stable
`Code`, the `Observed` and `Against` figures as raw floats with a `Unit`, an
optional `Subject`, and the sentence. Forty-seven emission sites across
`analyze.go`, `signals.go`, `constraints.go`, `authored.go`, `inference.go` and
`nodes.go` were converted. `SummariseHealth` got the same treatment: a signal gap
is now `{Signal, Status, Detail}` with statuses ABSENT, PARTIAL, QUERY_FAILED,
SUSPECT and DENIED, rather than `"psi: not exported by this cluster"`.

The sentences were kept, not summarised. A code says `IDLE_NO_WORK`; it cannot
say "what this does NOT rule out: a hot standby, a disaster-recovery replica, an
operator that only acts on a rare event". That paragraph is the difference
between a report and a workload somebody deleted on a Friday. So the tables
became the default and the prose moved behind `--explain` (`-v` still works),
rather than the other way round.

**stdout is now a sequence of tables.** Sections `meta`, `signals`, `findings`,
`evidence`, `totals`, `nodes`, `node_pins`, `drain_blockers` and `commitment`,
each opened by a `# section=` marker, each with a header row, columns separated
by two or more spaces, an empty field written `-`, and every hint or warning on
stderr. `kubectl tare report | awk '$4 == "OOMKILLED"'` now works without a JSON
parser. The commitment NOTE column became codes (`MEM_OVERCOMMIT`,
`UNLIMITED_CPU_PODS=3`) rather than a paragraph in a table cell.

`-o json` benefits most: the reasons carry raw numbers, so a consumer doing
arithmetic no longer parses `"4.0Gi"` back into bytes with a rounding error the
tool introduced.

**Verification.** A test walks the rendered report and asserts every data line
has the field count its header declares and no blank field, across a demo report
and one with a computed node delta. Two more assert that no reason sentence
reaches the default output and that `--explain` restores all of them. A fourth
pins the codes a script would plausibly filter on, so a rename has to be
deliberate.

**What this pass did not change.** The HTML report and the TUI still render the
sentences as before — the codes are available to them and unused. `kubectl tare
check` is still prose end to end.

## Roadmap after v0.1

- **v0.2**: CRD for policy and scoping; degraded no-Prometheus mode; PR bot
  against manifest/Helm repos; per-workload state store and recommendation audit
  log (this table is both the trust product and the future training set).
  *(2026-09-19)* The PR bot moves up in importance rather than scope: the unit a
  platform engineer can act on is a reviewable diff against Helm values, not a
  table, because the thing blocking adoption is the reviewer who says no.
- **v0.3**: eBPF SLI collection (Beyla/Cilium), inferred latency baseline
  ("we will not make your p99 worse than the last 14 days"), SLO overrides.
- **v0.4**: Actuation. Mutating webhook piggybacking on user deploys, plus
  container-level in-place resize (GA since k8s 1.35, so no longer an exotic
  dependency) for clusters on cgroup v2. Asymmetric descent loop with the full
  trip-wire set. Global kill switch. Never a self-triggered rollout. Memory
  decreases go through the evidence-plus-ceiling rule and never rely on the
  kubelet's best-effort shrink check.
- **v0.5**: HPA bound coordination; pod-level resources once that variant reaches
  GA (Beta as of 1.37, targeting ~1.38/1.39).
- **Later**: Descent-endpoint prediction and periodic demand forecasting,
  benchmarked against the naive loop. Opt-in federated priors as a separate RFC.
  *(2026-09-19)* Plus model-assisted ceiling extraction from image entrypoints,
  ConfigMaps and `envFrom`, scored against `test/corpus` as its evaluation set.
  Independent of the loop and of everything above it; see "ML position, fourth
  pass".
