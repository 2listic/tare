# tare

<img width="1400" height="760" alt="tui" src="https://github.com/user-attachments/assets/33b8e558-ea3a-4468-886a-2690c464f3b5" />

Right-sizes Kubernetes workloads by asking the kernel whether they are waiting
for CPU, not only how much they used, and explains every number it produces.

Read-only. No agent, no CRD, no configuration: it reads the Kubernetes API and
a Prometheus you already have, through the API server proxy.

## Main ideas

- **Pressure, not just usage.** A container capped at 100m that wants 400m shows
  the same usage graph as a correctly sized one. PSI
  (`container_pressure_cpu_waiting_seconds_total`) records the time it spent
  waiting for a CPU, and exists for every container, with or without a limit
  (23 of 23 on the test cluster; throttle counters: 3 of 23). Pressure above
  zero blocks a shrink; zero across the window is what makes one safe.
- **Runtime ceilings.** It reads `-Xmx`, `GOMEMLIMIT`, `--max-old-space-size`,
  gunicorn workers, vLLM/Ollama flags and more, and never recommends memory
  below them. A JVM with a 256Mi heap sized from usage OOMKills; tare gives it
  384Mi.
- **CPU limits are never lowered or added** (`CPU_LIMIT_HELD`). On Online
  Boutique, limits sized from usage made p95 18x worse at 2x load; sizing only
  the requests cut 1.6 CPU and 1GiB with latency at baseline.
  [KNOWN-FAILURES.md](KNOWN-FAILURES.md) has the full record.
- **Nodes, not millicores.** Recommendations are replayed through bin packing
  against real nodes and placements. The headline is nodes freed, split between
  what tight packing frees today and what tare's changes add.
- **Says when it cannot see.** A missing signal downgrades findings to
  `UNVERIFIED` instead of reading as zero. OOMKilled memory prints `?`, not a
  guess.
- **GPUs measured, never patched.** With NVIDIA's dcgm-exporter, a GPU whose
  busy-time never left zero is `GPU_IDLE`. Busy-time only bounds use from
  above (one thread reads 100%), so a busy GPU stays `GPU_BUSY_UNVERIFIED`
  unless the card exports SM activity, which GeForce cards never do. The
  summary counts `GPUs idle N of M`; a GPU count is never written to a patch,
  and a GPU container's CPU is not cut unless its GPU is proven idle.
- **Knows what else manages the workload.** It keeps Guaranteed pods
  Guaranteed, warns on HPAs that scale on utilisation, and skips VPA Auto.

## Install

```sh
os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')
curl -fsSL "https://github.com/2listic/tare/releases/latest/download/kubectl-tare_${os}_${arch}.tar.gz" | tar xz kubectl-tare
sudo install kubectl-tare /usr/local/bin/
sudo ln -sf kubectl-tare /usr/local/bin/kubectl_complete-tare   # tab completion
```

Or `go install github.com/2listic/tare/cmd/kubectl-tare@latest` (Go 1.27.3+),
or `./tools/build.sh` from this tree. Windows zips are on the
[releases page](https://github.com/2listic/tare/releases).

## Use

```sh
kubectl tare check                             # can this cluster produce a report? what is missing?
kubectl tare report                            # findings table
kubectl tare report -n prod --window 14d       # one namespace, longer history
kubectl tare report --verdict SATURATED        # only what is hurting now
kubectl tare report --explain                  # the sentence behind every code
kubectl tare report -o wide                    # every section: evidence, gpus, totals, nodes, commitment
kubectl tare report -o patch > fix.sh          # kubectl patch commands to review and run
kubectl tare report -o html > report.html      # one self-contained file for whoever approves
kubectl tare report -o tui                     # browse findings, with per-row CPU/memory plots
kubectl tare report -o json                    # raw numbers, for a pipeline
kubectl tare report --redact                   # hash every name before sending it out
kubectl tare report --demo                     # simulated workloads, real analyzer, no cluster
kubectl tare diff --repo ./manifests --write   # edit manifests in place, for a PR
```

```
$ kubectl tare report
VERDICT    CONF    NAMESPACE  WORKLOAD     CONTAINER  PODS  CPU        MEM          WHY
SATURATED  medium  prod       checkout     api        6     50m→115m   16Mi→?       OOMKILLED,CPU_PRESSURE,+1
IDLE       high    prod       legacy-sync  worker     1     500m→10m   256Mi→16Mi   IDLE_NO_WORK,IDLE_NOT_RULED_OUT
WASTE      medium  prod       static       nginx      12    1000m→10m  1.0Gi→16Mi   -

35 containers over 2w: nodes freed >=2 of 12; requests lowered on 2 (-12640m CPU, -12.3Gi mem), ...
warning  SIGNAL_NOT_MEASURED  cfs-throttle: 3 of 35 containers
```

stdout is data, stderr is the summary and warnings. Sections start with
`# section=<name>`, columns are split by two or more spaces, empty is `-`, and
evidence codes are a stable API.

`--prometheus <url>` overrides discovery; `--timeout 30m` for large clusters;
`--node-watts` and `--grid-intensity` add an energy/CO2 estimate to `-o wide`.

## Permissions

tare reads as you. For a service account, `deploy/rbac.yaml` grants only `get`
and `list` plus `services/proxy`. A narrower token degrades the report and says
what it lost; `kubectl tare check` tells you which.

## Known gaps

- One node with dead (all-zero) PSI in a healthy cluster reads as calm.
- Metrics that stopped mid-window are not flagged as stale.
- The 16Mi memory floor has no derivation: read it as "lower than now".
- A cyclic leak and an undersized container look the same.
- GPUs: no usage verdict for MIG slices or shared (time-sliced, MPS) devices,
  and no GPU view in `-o html`.
- No latency/SLO input, no Spark/Flink, init containers or pod-level resources.
  Nothing is applied automatically.

## Development

```sh
go test ./...                          # full suite (-short skips the threshold sweep)
./tools/release.sh v0.1.0 [--publish]  # build every platform, optionally tag and release
```

Design record: `docs/plan.md`. Failure history: `KNOWN-FAILURES.md`.

Apache-2.0.
