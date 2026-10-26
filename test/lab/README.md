# tare test lab

This is the cluster tare was developed and measured against, and the manifests
that plant workloads on it. Every "measured on tare's test cluster" figure in
the main README comes from here. It is a record of the authors' setup, not a
supported install: nothing below is required to use tare.

The authors' lab is k3d `tare-test` on a single Linux host (`$LAB` below is that
host's address), k3s v1.35.8+k3s1, 1 server + 2 agents, cgroup v2, containerd
2.2.7. To build an equivalent one:

```sh
k3d cluster create tare-test --agents 2 \
  --api-port 0.0.0.0:6443 --k3s-arg "--tls-san=$LAB@server:*" \
  -p 30090:30090@loadbalancer -p 30030:30030@loadbalancer
helm install kps prometheus-community/kube-prometheus-stack -n monitoring --create-namespace \
  --set alertmanager.enabled=false \
  --set prometheus.service.type=NodePort --set prometheus.service.nodePort=30090 \
  --set grafana.service.type=NodePort --set grafana.service.nodePort=30030
kubectl apply -f test/lab/workloads.yaml -f test/lab/shapes.yaml
```

Omit the TLS SAN and the API is unreachable from any other machine with a
certificate error. tare needs history before it says much: expect `low`
confidence for the first day.

| endpoint | url |
| --- | --- |
| Kubernetes API | https://$LAB:6443 (context `k3d-tare-test`) |
| Prometheus | http://$LAB:30090 |
| Grafana | http://$LAB:30030 |

Monitoring is `kube-prometheus-stack` (alertmanager disabled, 30d retention,
15s scrape) in namespace `monitoring`.

## Verified capabilities

- **In-place resize works**: CPU request 100m to 150m and memory limit 128Mi to
  96Mi (a shrink) both applied via the `resize` subresource with **0 restarts**.
- **QoS immutability is enforced**: a resize that would move a pod from
  BestEffort to Burstable is rejected by the API server.
- **CFS throttling metrics cover only containers with a CPU limit.** cAdvisor
  emits `container_cpu_cfs_*` only where a CPU quota is set: measured here, PSI
  covers 23 of 23 containers and CFS covers 3 of 23. (An earlier note in this
  file claimed the metrics were absent entirely; that measurement was taken on a
  fresh cluster where nothing had a CPU limit.) Do not depend on
  `container_cpu_cfs_throttled_periods_total` for saturation detection.
- **PSI metrics ARE exported**: `container_pressure_cpu_waiting_seconds_total`
  and `container_pressure_cpu_stalled_seconds_total`. These are the primary
  saturation gradient.

## Planted workloads (`tare-lab` namespace)

| workload | what it tests | expected finding |
| --- | --- | --- |
| `cpu-hog` | saturation detection | pegged at its 100m limit, PSI waiting ~0.88 s/s. Must be ranked first. |
| `tomcat-lowxmx` | runtime ceiling refusal | `JAVA_OPTS=-Xmx256m` under a 1Gi memory limit. Must report the ceiling, not a reduction. |
| `overprovisioned` | classic waste | requests 1000m CPU / 1Gi, uses ~0 CPU and ~12 MiB. |
| `guaranteed-app` | QoS preservation | requests equal limits. The recommendation must keep them equal. |

## Declaration shapes (`shapes.yaml`)

The nine original workloads all declare both requests and limits, because that
is what an allocation model naturally expresses. Real clusters are not like
that, and two shipped defects came from the gap: `IDLE` told the operator to
delete four sidecars that reserve nothing, and tare proposed *adding* limits to
a workload that deliberately had none without saying so. These four carry **no
new behaviour** - only new manifests.

| workload | the shape | what it catches |
| --- | --- | --- |
| `node-agent` | DaemonSet, no `resources` block at all: BestEffort | a container with no reservation has nothing to reclaim, and is first to be evicted |
| `metrics-app` + `config-reloader` | two containers in one pod, one with requests and one without | the sidecar shape that produced the `IDLE` defect; also the only case that reaches the per-pod fold |
| `no-limits-api` | requests only | no CFS throttle counters exist for it at all, so it leans entirely on PSI; proposing a limit here is a behavioural change |
| `guaranteed-cache` | request == limit on both resources | moving one field without the other silently demotes it to Burstable |

Apply with `kubectl apply -f test/lab/shapes.yaml`. `test/longrun/restart.sh`
applies it alongside the long-run manifest and refuses to start the clock if it
is not staged at `/tmp/shapes.yaml`.

**`nodeSelector` is deliberately not here.** It is the third shape the live
cluster showed, but it changes *packing* rather than verdicts, and the cluster
already pins six pods that way (k3s infrastructure). It is covered in
`internal/packing` against a node inventory instead, where the two defects it
exposed are written down as 21 and 22 in `KNOWN-FAILURES.md`.

## Load generation: nginx must outweigh its client

Serving a static file costs nginx about 0.05ms of CPU. Any load generator that
forks a process per request costs more than that, so a naive client becomes the
biggest consumer in the namespace and the workload under test looks idle. The
first long run failed exactly this way (`test/longrun/README.md`).

`diurnal-web` therefore gzips a 159KB payload per request (`gzip_comp_level 6`),
which costs ~2.5ms of nginx CPU, and the client is `curl` with keep-alive and
`--rate`. Calibrated on one replica:

| target rate | nginx | client | ratio |
| --- | --- | --- | --- |
| 50/s | 103m | 46m | 2.2x |
| 100/s | 184m | 83m | 2.2x |
| 200/s | 285m | 123m | 2.3x |
| 400/s | 342m | 144m | *not delivered - sequential curl saturates ~250/s* |

Use `curl`, not busybox `wget`: `wget` opens a fresh connection per request and
exhausts ephemeral ports under sustained load (`Cannot assign requested
address`), which `-q` hides completely.

**Check the ratio before trusting any measurement taken under load.** If the
client outweighs the server, nothing downstream means anything.

## Recreate

    kubectl apply -f test/lab/workloads.yaml

## Verified first end-to-end run (2026-09-14)

    kubectl tare report --prometheus http://$LAB:30090 \
      --namespace tare-lab --window 1h -v

All three planted findings were produced correctly:

| workload | verdict | evidence |
| --- | --- | --- |
| `cpu-hog` | SATURATED | CPU pressure 0.872 s/s, pinned at its 100m limit. Ranked first, recommended an increase. |
| `tomcat-lowxmx` | WASTE, ceiling-corrected | memory 512Mi to **384Mi**, raised from the 72Mi that observed usage alone would have suggested |
| `overprovisioned` | WASTE | 1000m to 10m CPU, 1.0Gi to 16Mi memory |

The tomcat case is the one that matters. Three recommenders, three answers:

| method | memory recommendation | outcome |
| --- | --- | --- |
| observed usage plus 15% | 72Mi | OOMKill: the JVM heap alone is 256Mi |
| the same, with a 100Mi floor | 100Mi | OOMKill: still below the heap |
| the same, with a 250MB floor | 238Mi | OOMKill: that is only the floor, not a measurement |
| tare | **384Mi** | correct, and it explains the 256Mi heap plus 128Mi non-heap |

**Regression to guard:** an earlier build recommended exactly 256Mi, the heap
size, which would also have OOMKilled. Container memory must always exceed the
heap ceiling. `TestFloorAlwaysExceedsHeap` covers this.
