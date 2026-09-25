# Long run

Everything else in this project is tested in seconds: unit tests, a corpus, and
a simulator that replays thirty days in 27ms. One property cannot be tested that
way.

**Does the same workload get the same numbers on day 3, day 7 and day 14?**

A recommender that oscillates is unusable however correct any individual answer
is, and only elapsed time shows it. The same goes for confidence actually
climbing to `high`, for the memory-trend windows filling, and for limit-trend
detection, which needs a limit that genuinely moves.

## Setup

Cluster: k3d `tare-test` on msi. Prometheus is on a **30Gi local-path PVC** with
30d retention. It was previously on `emptyDir`, which meant any pod restart
would erase the history the experiment exists to collect.

Snapshots run hourly from cron on msi:

    17 * * * * $HOME/tare-longrun/snapshot.sh

Each run writes `snapshots/<timestamp>.json` and appends one row per container
to `history.csv`. Analyse with:

    python3 test/longrun/stability.py ~/tare-longrun/history.csv

## Workloads

`workloads.yaml` holds the static cases. `longrun.yaml` adds the ones that
change over time, because the static lab has a coefficient of variation of about
0.01: two weeks of it is day one repeated fourteen times. `shapes.yaml` adds
four *declaration* shapes with no new behaviour at all (see `test/lab/README.md`);
`restart.sh` applies it too and fails the gate if it is not staged at
`/tmp/shapes.yaml`, because a run that carries only well-declared workloads
cannot see the verdict defects that live in the badly-declared ones.

| workload | what only time can test |
| --- | --- |
| `diurnal-web` + `load-generator` | Request rate follows a daily sine peaking at 14:00 UTC. Confidence should stay low until the window covers whole cycles, and recommendations should not chase the curve. |
| `slow-leaker` | Leaks ~24MiB/hour of anonymous memory in its own process, OOMKilling roughly every 40 hours. Exercises leak detection, OOM history and restart handling together. It must be anonymous memory, not tmpfs: see 2026-09-16 below. |
| `churn-app` | Redeployed every 6 hours. Its memory baseline keeps resetting; this must never be reported as a leak. |
| `resize-target` | A JVM with a fixed `-Xmx200m` whose container memory limit is toggled between 512Mi and 1Gi every two days. A plateau that does not follow a moving limit is the strongest available evidence of a fixed runtime ceiling, and this is the only way to produce it. |
| `cpu-hog` | Permanently starved. Must stay SATURATED for the whole run. |
| `tomcat-lowxmx` | Declared `-Xmx256m`. Its memory recommendation must never drop below 384Mi. |
| `guaranteed-app` | Requests equal limits. Requests must stay equal to limits in every snapshot. |

## Restarted 2026-09-14 21:55 UTC: the load generator generated no load

The first attempt collected for six hours and was discarded. `load-generator`
drove `diurnal-web` with a busybox `wget` loop, and measured on the cluster:

| | CPU p95 |
| --- | --- |
| `load-generator` (the client) | **35m** |
| `diurnal-web` x2 (the target) | **3.2m each** |

The client burned 5.5x more CPU than the server it was loading. Serving a static
file costs nginx about 0.05ms; forking a `wget` per request costs the client
1-2ms, so the scaffolding dominated and the workload under test sat at 1% of its
300m request - indistinguishable from `overprovisioned`.

The consequence was not merely unrealistic, it was a **false pass**. At 3m of
CPU the recommendation clamped to `MinCPUMillicores` (10m), and a constant is
stable no matter what the recommender does. `diurnal-web` is the only workload
here meant to produce a periodic CPU recommendation; it would have answered the
experiment's central question with a number that never touched the data.

Two things were also structurally wrong with `wget` as a load generator:

- **No keep-alive.** Under sustained load it exhausts ephemeral ports
  (`Cannot assign requested address`) and most requests fail instantly. With
  `-q` the errors are invisible, so the generator looks healthy while doing
  nothing.
- **Nothing to compress.** Static file serving is almost free, so no achievable
  request rate would have produced meaningful CPU.

Both are fixed. nginx now gzips a 159KB payload per request (`gzip_comp_level 6`),
and the client is `curl` with keep-alive and `--rate`. Calibrated on one replica
before the restart:

| target rate | nginx | client | ratio |
| --- | --- | --- | --- |
| 50/s | 103m | 46m | 2.2x |
| 100/s | 184m | 83m | 2.2x |
| 200/s | 285m | 123m | 2.3x |

A single sequential `curl` saturates around 250/s, so the generator runs two
processes at `R` each. The sine gives `R` = 10..105 per process, so the target
swings roughly 20m to 145m per replica across the day. Measured immediately
after the restart: 175m across both replicas against 75m for the client, a 2.3x
ratio, with `cpu-hog` PSI unchanged at 1.67 s/s (the new load is 0.4 cores out
of 16, so it does not contaminate anyone else's pressure signal).

`slow-leaker`, `churn-app` and `resize-target` were not modified and did not
restart; only the two broken deployments were replaced.

### And a second dead workload, found the same way

While verifying the restart, both cronjobs turned out to have been in
`ImagePullBackOff` for four hours: `bitnami/kubectl:1.33` 404s, because Bitnami
retired their free Docker Hub catalog. `churn-app` was still at
`deployment.kubernetes.io/revision: 1` - it had **never** been redeployed - and
`toggle-resize-limit` would have failed identically at 03:00.

That is two more of the five time-dependent behaviours dead on arrival:

- the leak-detector false-positive guard (`churn-app`'s baseline never reset), and
- **limit-trend detection**, which `docs/plan.md` calls the strongest available
  evidence of a fixed runtime ceiling and which only this toggle can produce.

Now `alpine/k8s:1.33.1`, and both jobs were triggered manually to prove they
work rather than assumed: `churn-app` went to revision 2 and `resize-target`'s
limit moved 1Gi -> 512Mi.

### The general lesson

The simulator cannot catch this class of defect, because in simulation the
demand curve is an input. On a real cluster the demand has to be *produced*, and
the thing producing it is a program that can be wrong. **Calibrate the load
generator against the target before starting the clock**, and check the ratio:
if the client outweighs the server, nothing downstream means anything.

More generally: everything this experiment depends on failed *quietly*. A
generator that sent no traffic, a cronjob that never ran, a recommendation
pinned to a constant - none of them raised an error, and all three produced a
plausible-looking `history.csv`. Before starting a clock that runs for two
weeks, **exercise every time-dependent path once by hand** and confirm it
changed something observable.

## What would count as a failure

- Any workload alternating between WASTE and SATURATED across snapshots.
- Coefficient of variation above ~0.15 on a steady workload's recommendation.
- `churn-app` ever reported as LEAKING.
- `slow-leaker` not reaching LEAKING, or losing it after an OOMKill restart.
- `tomcat-lowxmx` recommended below 384Mi at any point.
- `guaranteed-app` recommended with request != limit.
- Confidence never reaching `high`.
- `stability.py` printing `INSTRUMENT DEAD` at any point. A zero in `memEarly`
  or `memLimitEarly` means the signal was never measured, which is not the same
  as a workload that did not move, and nothing below that banner is evidence.
- Any recommendation series reported as `FLOOR` by `stability.py` being counted
  as evidence of stability. A floored series is pinned at `MinCPUMillicores` or
  `MinMemBytes` and is constant by construction. `overprovisioned`,
  `guaranteed-app` and `churn-app` are idle by design and will be floored on
  CPU; that is correct for them. `diurnal-web` being floored means the load
  generator has failed again.

## Restarted again 2026-09-15: three dead measurements and a bug in tare

Attempt 2 collected for twelve hours and was discarded. Details and the archived
data are in `~/tare-longrun/attempt2-saturation-ratchet/WHY.md` on msi. In short:

- tare clamped **all four** recommendations to their current values whenever a
  container was `SATURATED`, whichever resource was actually starved. A
  CPU-starved JVM using 65MiB was told to keep its 512Mi request. Fixed, with
  regression tests, plus the mirror case where an OOMKill froze CPU.
- The **7d window** was wider than the cluster's data, so the early-half queries
  matched nothing and both leak detection and limit-trend inference were inert
  for the entire run. The window now tracks elapsed run time, capped at 7d.
- The **limit toggle rolled the Deployment**, replacing the pod. Observations
  are keyed by pod, so the replacement had no early observation and the
  limit-trend signal could never exist. The toggle now resizes the pod in place
  through the `pods/resize` subresource, and fails loudly if the container
  restarts.
- **Trend values took the last pod of a workload rather than the worst**, so a
  leaking replica beside a freshly-rolled one reported `OK`.

The general lesson from attempt 1 held again, so it is worth repeating: every
one of these produced a clean, plausible `history.csv`. Nothing errored.
`restart.sh` now exercises each time-dependent path and refuses to install cron
unless every one of them changed something observable, and `stability.py`
reports a dead input above the results rather than leaving it to be noticed.

## 2026-09-16: the leaker killed itself, and tare did not notice

`slow-leaker` OOMKilled on schedule at 10:17 UTC, about 41 hours in, and then
never came back. 41 restarts, `Reason: StartError`:

    failed to create containerd task: ... container init was OOM-killed
    (memory limit too low?)

**Cause: `/leak` was an `emptyDir{medium: Memory}`, which is pod-scoped.** The
~1GB of `chunk-N` files survived the container restart and stayed charged to
the cgroup, so every restart was killed inside `runc init`, before the shell
could run. It could not have deleted the files: nothing of its own ever
executed again. "OOMKills roughly every 40 hours" assumed a reset that this
volume type cannot perform.

Fixed by leaking **anonymous memory in the container's own process** instead of
files in a tmpfs, which is a better model of a leak anyway and dies with the
process. A new shell variable per iteration rather than appending to one:
appending copies the whole accumulated string, so it would peak at twice the
leak and OOM at half the intended size. Measured in a 64MiB container with swap
off: 14, 25, 37, 48, 60MiB, then OOMKilled, which is the cycle the lab wanted.

### The two defects it exposed, which are worth more than the workload

**tare recommended 1.17GiB for a container that had not run in two hours, and
called it WASTE.** `f.Restarts` was collected at `analyze.go:283` and read
nowhere. Worse, the kill happened during container init, so no OOM event was
ever recorded against the container and `OOMKilled` was false: the one signal
that blocks a shrink was invisible in precisely the case that needed it. A
crash-looping container now goes `UNVERIFIED` and is never recommended below
its current request, with `RestartChurn: 3` chosen so the leaker's own designed
~40h OOM does not trip it. `TestCrashLoopIsNotWaste` and
`TestOneRestartIsNotACrashLoop` cover both sides.

**`stability.py` printed no banner**, correctly by its own logic: the trend
tests use `all()` across workloads, and `churn-app` legitimately has no early
observation after every redeploy, so it holds those tests false forever and
masks any single workload going dark. Per-workload deadness now uses a test
churn cannot trip - `memEarly` *and* `memLate` both zero, meaning no memory
data at all - plus a restart-churn check.

Same lesson as every previous attempt, one layer further in: this time the
input that died quietly was a workload, and the thing that failed to notice was
the analyzer rather than the lab.

## The experiment after this one

The valuable follow-up is not watching the report: it is **applying the
recommendations and leaving them applied**. Baseline for 48 hours, apply via
`-o patch`, then hold for two weeks and watch for OOMKills, throttling and
pressure. That is the closed-loop replay test the simulator runs, but against
reality, and it is the only thing that would turn the safety claim from a
simulation result into evidence.

## Stopped 2026-09-19 16:0x UTC: attempt 5 ended, two real failures

The hourly cron was removed and attempt 5's data archived on msi to
`~/tare-longrun/attempt5-data/` (88 snapshots, 793 rows, 2026-09-15 23:07 to
2026-09-19 13:17, plus `FINAL-stability.txt`). The cluster and its workloads are
untouched and still running; only the snapshot cron is gone. To resume:

    ssh msi '(crontab -l 2>/dev/null; echo "17 * * * * \$HOME/tare-longrun/snapshot.sh >/dev/null 2>&1") | crontab -'

**This is the first attempt that produced a clean answer to its own question.** The load generator worked, both cronjobs ran, the
windows filled, and confidence climbed to `high`. Two stated failure criteria
were still met:

1. **`slow-leaker` never reached LEAKING** - `WASTEx59, SATURATEDx29`, with its
   memory recommendation swinging 16Mi to 1174Mi at cv 0.43. `stability.py`
   printed `INSTRUMENT DEAD` for it: saturation resting on a peak (0.030) rather
   than sustained pressure (mean 0.0013). Running the new binary against the
   cluster shows why - the evidence line reads "the memory-trend signal is
   unavailable for this container", so the leak ratio test cannot fire at all.
   Three attempts in a row now, inert on real data while passing in simulation.
   This is the next thing to fix, and it should be fixed against
   `attempt5-data/history.csv` rather than by starting a fifth clock.

2. ~~**`diurnal-web` recommendation cv 0.18**~~ **Retracted: this was the
   instrument, not the recommender.** `stability.py` was computing cv across
   snapshots whose windows ranged 6h to 86h. Segmented by window width,
   `diurnal-web` is cv 0.319 below 24h and **cv 0.004** above 72h, and
   `load-generator` is cv 0.00. The recommendation is stable to within 5m once
   the window covers a diurnal cycle. `stability.py` now drops snapshots below
   a 48h window and prints how many it excluded.

   **So attempt 5 failed one criterion, not two, and answered its central
   question affirmatively.** The recommender is stable across days. That is the
   first clean result this experiment has produced.

What held: `cpu-hog` SATURATED in all 88 snapshots at a stable 115m,
`tomcat-lowxmx` never recommended below 384Mi, `guaranteed-app` kept request
equal to limit throughout, `churn-app` never called LEAKING, `resize-target`
held a flat 328Mi plateau while its limit toggled.

### What the new code found here that the snapshots could not

Running the node-aware build against the same cluster found a defect no
simulated workload could expose: the `IDLE` verdict fired on five containers
with **no resource requests at all** - `node-exporter`, `kube-state-metrics`,
both Grafana sidecars and the Prometheus `config-reloader` - and told the
operator the larger saving was to delete them. Every simulated container has
requests by construction, so only a real cluster with real unconfigured
sidecars could produce this. Fixed by requiring a reservation above the
recommendation floor before anything is called idle. See `docs/plan.md`,
"Live validation on the lab cluster".
