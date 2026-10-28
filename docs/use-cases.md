# Three scenarios tare is for

Written against what the tool does today, with the gaps named where a scenario
needs something not yet built. Output below is abridged to the relevant lines;
`kubectl tare report --demo -v` shows the full format without a cluster.

tare integrates with telemetry, not with other tooling: the Kubernetes API for
what is declared, and a Prometheus-compatible endpoint — Prometheus, Thanos,
Mimir or VictoriaMetrics — for what actually happened. It is found automatically
and queried through the API server proxy, so there is no agent, no CRD, no port
forward and nothing installed in the cluster.

---

## 1. The pre-apply safety gate

**Who:** whoever is about to change requests and limits on a few dozen
containers, whether the numbers came from a dashboard, a capacity spreadsheet,
last quarter's incident, or an autoscaler's suggestion.

**The problem:** a memory number derived from observed usage is unsafe whenever
the container's runtime holds a fixed cap — a JVM with `-Xmx`, a Node process
with `--max-old-space-size`, a Go service with `GOMEMLIMIT`, a Python service
with a worker count. Such a container uses far less than it needs, right up
until the moment it needs it. The number looks conservative and OOMKills on the
next restart.

**What they run:**

```sh
kubectl tare report -n prod -v
kubectl tare report --verdict SATURATED
```

**What they get:** the few rows where the safe number and the observed number
disagree, each with the flag quoted as the reason:

```
MEM_RAISED_BY_CEILING   memory recommendation raised from 253Mi to 384Mi by a runtime ceiling;
                        observed usage alone would have been unsafe
CEILING jvm-heap-max    JVM heap is capped at 256Mi by JAVA_OPTS=256m, so the container needs
                        about 384Mi once metaspace, code cache and thread stacks are counted.
                        Raising the limit above that buys nothing; lowering it below invites
                        an OOMKill. Change the flag, not the limit.
```

Where the flag is not in the pod spec at all, the ceiling is inferred from a
memory plateau instead, and reported as an inference rather than a reading.

**Why this scenario is the strongest:** the ask is one read-only command before
merging, nothing in the cluster changes, and the failure it prevents is the one
that pages someone at three in the morning.

**Gap:** a non-zero exit code when a proposed change is unsafe, so this can gate
a pull request rather than being run by hand. Reading the proposed values out of
the manifest diff, instead of comparing two reports by eye, is the step after
that.

---

## 2. The fixed-hardware capacity squeeze

**Who:** whoever runs an on-prem or bare-metal cluster where the answer to "we
need more capacity" is a purchase order and a quarter of lead time, not an
autoscaling group.

**The problem:** cost tooling is priced and pitched against a cloud bill, and
has nothing to say to a cluster whose hardware was paid for two years ago. The
currency here is not dollars per month, it is how many more workloads fit before
the next purchase.

**What they run:**

```sh
kubectl tare report --by-savings --top 20
```

**What they get:** the twenty containers holding the most unused capacity, and a
total that keeps both directions apart:

```
40 containers (20 listed) over 2w: nodes freed >=1 of 6; requests lowered on 19 (-2535m CPU, -6.2Gi mem), raised on 11 (+476m CPU, +132Mi mem)
```

Both directions are reported, because a capacity plan built on the lowered
figure alone is short by whatever the starved containers are owed.

**Why bare metal suits tare specifically:** PSI, the saturation signal the
analysis rests on, needs cgroup v2, a kernel with PSI enabled, containerd ≥2.0
and the `KubeletPSI` feature gate. Managed platforms are uneven — it is not
exposed on AKS at all, and the Amazon Linux 2023 and RHEL kernels ship it
disabled by default. On hardware you own, all of that is yours to set. tare also
calls no cloud APIs and works air-gapped.

---

## 3. The starvation diagnosis

**Who:** someone holding an incident where a service is slow or restarting, and
the dashboards show CPU usage well under the request.

**The problem:** usage under request looks like headroom, so the obvious reading
is "not a resource problem". It can be exactly a resource problem: a container
pinned at its CPU limit shows modest average usage and stalls constantly, and a
container whose heap is capped restarts under load while its working set looks
comfortable. Usage percentiles cannot tell these apart — that is what the
kernel's pressure accounting is for.

**What they run:**

```sh
kubectl tare report -n prod --verdict SATURATED -v
```

**What they get:** the containers being starved, ranked first, with the evidence
rather than a score:

```
SATURATED  prod/Deployment/checkout [api]  confidence high (17098 samples)
    CPU_PRESSURE          CPU pressure 0.900 s/s exceeds 0.030 while using 100% of its allocation:
                          the container is waiting on CPU it was denied (window mean 0.8437)
    CPU_PINNED_AT_LIMIT   usage is pinned at the 100m CPU limit; raise the limit, not just the request
```

Sustained pressure is shown beside peak pressure, because one unlucky minute
over a two-week window is not a starved container, and a tool that reports only
the peak says it is.

**The refusal is the point here too:** a container tare cannot see is reported
as `UNVERIFIED` rather than shrunk, and a cluster with no saturation signal at
all is told so in the report header before any recommendation. The output
degrades to "I cannot verify this" rather than to a confident guess.

**Gap:** this is the scenario most exposed to PSI availability. On a cluster
without it, and without CPU limits to produce throttle counters, tare has no
saturation signal and says so — which is honest, and is also the whole report.
