# tare-agent (proof of concept)

tare decides whether a container is starved of CPU from PSI
(`container_pressure_cpu_waiting_seconds_total`). Many clusters do not have PSI:
cgroup v1 nodes, RHEL 8/9/10 kernels (PSI ships switched off), kubelets older than
1.33. tare-agent measures the same thing another way, so it can be compared with
PSI where PSI exists, and used where it does not.

It measures **CPU wait** per container: the seconds the container's tasks spent
ready to run but waiting for a CPU, summed over all of its tasks, including tasks
that have already exited. On every scrape it reads the scheduler counters of every
live thread (`/proc/<pid>/task/<tid>/schedstat`); in between, the kernel sends it
the final counters of every task that exits (taskstats exit notifications), so
short-lived processes are counted too.

This is a proof of concept. tare does not read the agent's metrics yet. To see how
they compare with PSI today, use `compare-pressure` (below), which reads Prometheus
and judges the agent's data the way tare judges PSI.

## What gets deployed

| what | where | needed for |
|---|---|---|
| the agent, as a DaemonSet in namespace `tare-agent` | `deploy/agent/agent.yaml` | measuring |
| a scrape configuration in your Prometheus | snippets below | storing the metrics |
| nothing else | | |

The agent does not talk to the Kubernetes API (no ServiceAccount token, no RBAC).

Requirements:

- Linux nodes, kernel 4.18 or newer, cgroup v1 or v2, containerd or CRI-O.
- A Prometheus that already scrapes the kubelet's cAdvisor endpoint
  (`/metrics/cadvisor`), with the `id` label kept on those series. The agent labels
  its series only with `id`, the container's cgroup path. The kubelet's series carry
  the same `id`, and that is how namespace, pod and container names are attached.
- kube-state-metrics, optional: with it, CPU requests and limits are taken into account.
- Permission to run a privileged pod with `hostPID` and `hostNetwork` (see below).

## 1. Build the image

From the repository root:

```bash
docker build -t tare-agent:poc agent/
```

The build runs inside Docker (Go is not needed on your machine). The result is a
static binary on a distroless base, about 12 MB.

To build or test the binary itself (Go 1.27):

```bash
cd agent
go test ./...
go build -o tare-agent .
```

## 2. Get the image to the nodes

There is no published image yet. Either push it to a registry the cluster can pull
from, or import it into every node's container runtime.

**Registry:**

```bash
docker tag tare-agent:poc registry.example.com/tare-agent:poc
docker push registry.example.com/tare-agent:poc
```

Then in `deploy/agent/agent.yaml` set `image: registry.example.com/tare-agent:poc`
and `imagePullPolicy: IfNotPresent`.

**Import into each node** (no registry, keep `imagePullPolicy: Never`):

```bash
# containerd
docker save tare-agent:poc | ssh <node> 'sudo ctr -n k8s.io images import -'
# CRI-O
docker save tare-agent:poc | ssh <node> 'cat > /tmp/tare-agent.tar &&
  sudo skopeo copy docker-archive:/tmp/tare-agent.tar containers-storage:docker.io/library/tare-agent:poc'
```

`ctr` may live in `/usr/local/bin`, which `sudo` leaves out of its `PATH` on
RHEL-family nodes: use the full path (`sudo /usr/local/bin/ctr ...`).

## 3. Deploy

```bash
kubectl apply -f deploy/agent/agent.yaml
kubectl -n tare-agent rollout status ds/tare-agent
kubectl -n tare-agent logs ds/tare-agent
```

The log should show the cgroup version and `registered for exits on cpus ...`. If
the exit listener cannot start, the agent keeps serving the live threads and logs why.

Why the pod is privileged:

| setting | reason |
|---|---|
| `hostPID: true` | the PIDs in the cgroups are host PIDs, and `/proc` must be the host's |
| `hostNetwork: true` | the kernel's taskstats interface only answers in the host network namespace |
| `privileged: true` | for the POC. What is actually needed: `CAP_NET_ADMIN` (taskstats exit events, a large receive buffer) and read access to every task's `/proc` entry |
| `/sys/fs/cgroup` mounted read-only | to find the containers' cgroups |

The namespace carries `pod-security.kubernetes.io/enforce: privileged`. On
OpenShift the pods also need a privileged SCC.

Agent flags (rarely needed): `-listen` (default `:9105`), `-cgroup` (default
`/sys/fs/cgroup`), `-proc` (default `/proc`).

## 4. Have Prometheus scrape it

The agent publishes its numbers on a web page on every node
(`http://<node>:9105/metrics`). That alone stores nothing: Prometheus has to be
told to read ("scrape") those pages every 15 seconds and keep the values.
Prometheus does not find new things to scrape on its own, so you add one piece of
configuration. Which one depends on how your Prometheus was installed.

**Check which case you are in:**

```bash
kubectl get prometheus -A
```

If this lists something, your Prometheus is managed by the **Prometheus Operator**
(for example the kube-prometheus-stack Helm chart): case A. If the command says the
resource type does not exist, you configure Prometheus yourself through its
`prometheus.yml`: case B.

### Case A: Prometheus Operator

You do not edit Prometheus' configuration; the operator writes it from Kubernetes
objects. Create one object, a **PodMonitor**, that says "scrape the pods labelled
`app=tare-agent`, on their port named `metrics`".

1. Find the label your Prometheus requires on PodMonitors:

   ```bash
   kubectl get prometheus -A -o jsonpath='{range .items[*]}{.metadata.name}: {.spec.podMonitorSelector}{"\n"}{end}'
   ```

   With kube-prometheus-stack this is usually `{"matchLabels":{"release":"<your Helm release>"}}`.
   An empty selector (`{}`) means every PodMonitor is picked up.

2. Save this as `tare-agent-podmonitor.yaml`, with that label under `metadata.labels`:

   ```yaml
   apiVersion: monitoring.coreos.com/v1
   kind: PodMonitor
   metadata:
     name: tare-agent
     namespace: tare-agent
     labels:
       release: kube-prometheus-stack   # the label found in step 1
   spec:
     selector:
       matchLabels: {app: tare-agent}
     podMetricsEndpoints:
     - port: metrics
       interval: 15s
   ```

3. Apply it:

   ```bash
   kubectl apply -f tare-agent-podmonitor.yaml
   ```

   The Prometheus Operator may also limit which namespaces it reads PodMonitors from
   (`spec.podMonitorNamespaceSelector` of the `prometheus` object); `tare-agent` must
   be one of them.

### Case B: plain Prometheus

Add this job under `scrape_configs:` in `prometheus.yml`, then reload Prometheus
(restart it, or `curl -X POST http://<prometheus>/-/reload` if it runs with
`--web.enable-lifecycle`):

```yaml
scrape_configs:
- job_name: tare-agent
  scrape_interval: 15s
  kubernetes_sd_configs:
  - role: pod
    namespaces: {names: [tare-agent]}
  relabel_configs:
  - {source_labels: [__meta_kubernetes_pod_label_app], regex: tare-agent, action: keep}
  - {source_labels: [__meta_kubernetes_pod_container_port_name], regex: metrics, action: keep}
  - {source_labels: [__meta_kubernetes_pod_node_name], target_label: node}
```

It tells Prometheus to look up the `tare-agent` pods through the Kubernetes API
(Prometheus' service account needs `list` on pods, which it already has if it
discovers the kubelet that way) and to scrape their `metrics` port.

### Check

In Prometheus' web UI, **Status → Targets** must list the agent with one target per
node, all **UP**. Or run this query in Prometheus: it must return one series per node
with value 1.

```promql
up{job=~".*tare-agent.*"}
```

(With a PodMonitor the job is usually called `tare-agent/tare-agent` or `tare-agent`.)

## 5. Check that it works

```bash
kubectl -n tare-agent port-forward ds/tare-agent 9105 &
curl -s localhost:9105/metrics | grep '^tare_'
```

| metric | meaning |
|---|---|
| `tare_cpu_wait_seconds_total{id}` | CPU wait of the container (counter, seconds) |
| `tare_cpu_run_seconds_total{id}` | CPU time the agent accounted for the container: compared with the kubelet's CPU usage it gives coverage |
| `tare_agent_exits_total` | exit records received |
| `tare_agent_exits_unattributed_total` | exit records that could not be assigned to any container. Healthy: a few percent at most |
| `tare_agent_exits_outside_containers_total` | exits of host processes, not in any container (normal) |
| `tare_agent_exits_never_received_total` | threads whose exit record never arrived. Healthy: close to 0 |
| `tare_agent_exits_layout_total{layout}` | records decoded per kernel record layout (`upstream`, `rhel10`) |
| `tare_agent_exits_layout_unknown_total` | records from a kernel whose layout the agent does not know. Must be 0 |
| `tare_agent_netlink_drops_total` | exit records the kernel dropped. Must be 0 |
| `tare_agent_containers{location}` | containers found under the kubelet's `kubepods` hierarchy, or `outside` it (see Troubleshooting) |
| `tare_agent_build_info{version,kernel,cgroup}` | one per node |

In Prometheus, this must return your containers' names; if it returns nothing,
the `id` join is not working (see Troubleshooting):

```promql
sum by (namespace, pod, container) (
  rate(tare_cpu_wait_seconds_total[5m])
  * on (id) group_left (namespace, pod, container)
    max by (id, namespace, pod, container) (container_cpu_usage_seconds_total{container!="",container!="POD"} * 0 + 1))
```

## 6. Compare with PSI: `compare-pressure`

`agent/compare-pressure` is a standalone script (Python 3.9+, standard library
only). It reads Prometheus and, per container, judges the agent's CPU wait the
way tare judges PSI. Let the agent run for at least a day of normal traffic first:
each container's "normal" is learned from its own history in the window.

```bash
agent/compare-pressure --prometheus http://localhost:9090 --window 1d
agent/compare-pressure --prometheus http://localhost:9090 --window 7d --namespace shop,payments
agent/compare-pressure --prometheus http://localhost:9090 --window 1d --all --stretches stretches.csv
```

If Prometheus is not reachable directly:
`kubectl -n <monitoring namespace> port-forward svc/<prometheus service> 9090`.

| option | meaning |
|---|---|
| `--window` | how far back to look (default `1d`; `30m`, `12h`, `7d`, `2w`) |
| `--end` | end of the window (Unix time or `2026-10-21T16:36:00Z`; default now) |
| `--namespace` | comma-separated namespaces (default all) |
| `--share` | share of stalled 5-minute stretches that makes a container SATURATED (default `0.05`) |
| `--kubelet-job` | Prometheus job of the kubelet's cAdvisor endpoint (default `kubelet`; `""` for any job) |
| `--all` | list every container, also those NOT-STARVED under every rule |
| `--stretches FILE` | write every 5-minute stretch of every container to a CSV file, for your own analysis |
| `--redact` | replace namespace, pod, workload and container names and the Prometheus host with stable hashes, in the output and the `--stretches` file, to paste the results outside the organisation. Same hashes as `tare report --redact`; stable, not secret: a guessed name can be confirmed |

### How it decides

Per container, the window is cut into 5-minute stretches. In each stretch:

- **wait**: the agent's CPU wait (s/s);
- **usage**: CPU usage from the kubelet (cores);
- **slowdown** = wait ÷ usage: seconds of waiting per second of running;
- **psi**: PSI waiting from the kubelet (s/s), when the cluster has it.

A stretch is **stalled** under each rule:

| rule | stalled when | idea |
|---|---|---|
| `psi` | PSI > 0.03 | tare's pressure threshold, the reference |
| `relative-slowdown` | slowdown ≥ 2 × the container's own normal and wait ≥ 0.01, or slowdown ≥ 1 | compare each container with itself: containers differ a lot in how much they wait when nothing is wrong. Normal = the container's 10th percentile of slowdown in the window |
| `fixed-slowdown` | slowdown ≥ 0.488 and wait ≥ 0.01 | one slowdown cut for every container |
| `absolute-wait` | wait ≥ 0.0435 s/s | one cut on the raw wait |

Then each rule gives each container the verdict tare's pressure check gives:

| verdict | meaning |
|---|---|
| `SATURATED` | stalled in at least `--share` of its stretches, and its peak CPU reached half of its request or limit. tare would never lower its CPU |
| `NOT-STARVED` | pressure does not stop a reduction: tare would go on to its other checks. Also when the container was stalled as often but used under half of its allocation, with the code `NODE_CONTENTION` (the node was busy, a bigger request would not help; tare treats it the same way) |
| `UNVERIFIED` | fewer than 12 stretches, or (agent rules) the agent accounted under half of the container's CPU time. Idle containers (never above 0.005 cores) have no coverage to check and are not UNVERIFIED for it |
| `-` | no PSI for this container |

tare's other verdicts (LEAKING, IDLE, WASTE, BLOCKED, UNSET, CONFLICT, NO-DATA)
come from memory and usage, not from pressure. The script does not reproduce them.

### Output

```
using http://127.0.0.1:9191: tare-agent on 2 nodes, PSI present, 1h ending 2026-10-21T16:35Z
# section=findings
PSI        RELATIVE-SLOWDOWN  FIXED-SLOWDOWN  ABSOLUTE-WAIT  NAMESPACE  WORKLOAD     CONTAINER    STALLED          NORMAL  COVER  WHY
SATURATED  SATURATED          SATURATED       SATURATED      reallab    postgres-db  postgres-db  69%/38%/38%/85%  0.131   0.86   CPU_PRESSURE,CPU_WAIT_RELATIVE,+2
NOT-STARVED  NOT-STARVED        NOT-STARVED     NOT-STARVED    reallab    load-pg      pgbench      0%/0%/0%/0%      0.216   0.98   -

7 containers (7 listed) over 1h: SATURATED by psi 6, relative-slowdown 6, fixed-slowdown 6, absolute-wait 6
agreement  relative-slowdown: same verdict as psi on 7 of 7 containers; same call on 76.9% of 91 stretches (16 psi only, 5 relative-slowdown only)
```

| column / line | meaning |
|---|---|
| one column per rule | the container's verdict under that rule |
| `WORKLOAD` | the pod name without its ReplicaSet / StatefulSet suffix |
| `STALLED` | share of stretches stalled, per rule in column order |
| `NORMAL` | the container's normal slowdown (used by `relative-slowdown`) |
| `COVER` | share of the container's CPU time the agent accounted (below 0.5 the agent rules say UNVERIFIED) |
| `WHY` | evidence codes: `CPU_PRESSURE` (PSI), `CPU_WAIT_RELATIVE`, `CPU_WAIT_SLOWDOWN`, `CPU_WAIT_ABSOLUTE` (the rule that made it SATURATED), `NODE_CONTENTION`, `LOW_COVERAGE`, `FEW_STRETCHES`, `PSI_ONLY` / `AGENT_ONLY` (psi and relative-slowdown disagree). Like tare, the first two codes and `+N` |
| `agreement` | per agent rule: same container verdict as PSI, and same call per stretch, with the stretches where only PSI or only the rule said stalled |
| `warning` | `SIGNAL_ABSENT` (no PSI, or no CPU requests), `AGENT_LOW_COVERAGE`, `AGENT_LAYOUT_UNKNOWN`, `AGENT_DROPS`, `AGENT_UNATTRIBUTED` |

On a cluster **without PSI** the PSI column and the agreement lines are left out,
and the agent's rules are shown on their own.

`--stretches` writes one row per container and stretch: namespace, pod, container,
time, wait, usage, slowdown, psi, normal, and 0/1 per rule.

## Troubleshooting

| symptom | cause | what to do |
|---|---|---|
| `no tare_cpu_wait_seconds_total series` | the agent is not scraped | check the PodMonitor / scrape config and the Prometheus targets page |
| `tare-agent series exist but none matches a container` | the kubelet's cAdvisor series lack the `id` label, or the kubelet job has another name | keep `id` (some setups drop it with a metric relabelling); pass `--kubelet-job` |
| `tare_agent_containers{location="outside"}` above 0 | the kubelet and the container runtime use different cgroup drivers (e.g. kubelet `systemd`, containerd `SystemdCgroup = false`): containers live under the runtime's service, not under `kubepods` | the agent still finds them by their container id; whether the kubelet's `id` matches depends on the setup. Fixing the driver mismatch is the real cure |
| `AGENT_LOW_COVERAGE` on fork-heavy containers | processes torn down after their exit record is sent; values of 0.7–0.9 are normal for them | below 0.5, look at `tare_agent_exits_unattributed_total` |
| `AGENT_LAYOUT_UNKNOWN` | a kernel whose taskstats record layout the agent does not know | report the kernel version; exits on that node are not counted |
| `AGENT_DROPS` | the kernel dropped exit records (very high process churn) | the agent requests a 64 MB receive buffer; check the agent's CPU |
| `sudo: ctr: command not found` | `/usr/local/bin` is not in `sudo`'s `PATH` (RHEL family) | use `sudo /usr/local/bin/ctr` |

## Remove

```bash
kubectl delete -f deploy/agent/agent.yaml     # and your PodMonitor / scrape config
```

## Limits of the proof of concept

- The image is built locally; there is no published image or Helm chart.
- The pod runs privileged; the capability-only profile is not done yet.
- tare does not read the agent yet: `compare-pressure` is the way to look at it.
- Measured cost: about 0.002–0.05 cores and 15–20 MiB per node, growing with the
  rate of process exits (0.05 cores at about 280 exits per second).
- Exercised on Ubuntu 24.04 (kernel 6.8), Debian 12 (6.1), AlmaLinux 9 (5.14),
  AlmaLinux 10 (6.12), Debian 11 with cgroup v1 (5.10), CRI-O 1.36, Rocky Linux 8.9
  (4.18, without Kubernetes) and arm64 (emulated, without Kubernetes).
