#!/bin/sh
# Restart the long run cleanly, and refuse to start the clock unless every
# time-dependent path is proven to work.
#
# This experiment has now been restarted twice because something was silently
# dead: a load generator that sent no load, two cronjobs in ImagePullBackOff, a
# saturation ratchet that echoed the request back as a recommendation, and an
# observation window whose early-half queries fell off the end of Prometheus
# retention. Every one of them produced a clean, plausible history.csv.
#
# So this script does not merely set things up. It exercises each path and
# checks that something observable changed, and exits without installing cron
# if any check fails. A run that does not start is far cheaper than fourteen
# days of data that means nothing.
set -u
export PATH="$HOME/.local/bin:$PATH"
DIR="$HOME/tare-longrun"
NS=tare-lab
FAIL=0
TS=$(date -u +%Y%m%dT%H%M%SZ)
# REASON is recorded in the archived run's WHY.md. Set it when the reason is
# a defect rather than an ordinary restart.
REASON="${REASON:-}"

say()  { printf '\n=== %s ===\n' "$1"; }
ok()   { printf '  OK    %s\n' "$1"; }
bad()  { printf '  FAIL  %s\n' "$1"; FAIL=1; }

say "1. stop collection"
crontab -l 2>/dev/null | grep -v tare-longrun | crontab -
crontab -l 2>/dev/null | grep -q tare-longrun && bad "cron entry survived" || ok "cron stopped"

say "2. archive the previous attempt"
if [ -s "$DIR/history.csv" ]; then
  ARC="$DIR/attempt$(( $(ls -d "$DIR"/attempt* 2>/dev/null | wc -l) + 1 ))-restart"
  mkdir -p "$ARC"
  mv "$DIR/history.csv" "$ARC/" 2>/dev/null
  mv "$DIR/snapshots" "$ARC/" 2>/dev/null
  rm -f "$DIR/STARTED_AT"
  ROWS=$(( $(wc -l < "$ARC/history.csv" 2>/dev/null || echo 1) - 1 ))
  cat > "$ARC/WHY.md" <<WHY
# Archived $(date -u +%Y-%m-%dT%H:%M:%SZ)

Stopped after $ROWS rows. Reason: ${REASON:-superseded by a restart}.

Earlier attempts have their own directory here, each with the defect that ended
it. \`test/longrun/README.md\` in the repo carries the full account; this file
only records when this particular run stopped and why.
WHY
  ok "archived to $ARC"
else
  ok "nothing to archive"
fi
mkdir -p "$DIR/snapshots"

say "3. install the fixed binary and scripts"
for f in kubectl-tare-new:"$HOME/.local/bin/kubectl-tare" snapshot.sh:"$DIR/snapshot.sh" stability.py:"$DIR/stability.py"; do
  src="/tmp/${f%%:*}"; dst="${f#*:}"
  [ -f "$src" ] || { bad "missing $src"; continue; }
  install -m 755 "$src" "$dst" && ok "$(basename "$dst")"
done
kubectl-tare report -n "$NS" --window 6h -o json >/dev/null 2>&1 \
  && ok "binary runs" || bad "binary does not run"

say "4. apply the lab manifest"
kubectl apply -f /tmp/longrun.yaml >/dev/null 2>&1 && ok "applied" || bad "apply failed"
# The declaration shapes are a second manifest carrying no new behaviour: a
# container with no requests, a workload with no limits, a Guaranteed pod and
# two containers in one pod. The population of behaviours is not the population
# of declarations, and two shipped verdict defects lived only in the second.
if [ -f /tmp/shapes.yaml ]; then
  kubectl apply -f /tmp/shapes.yaml >/dev/null 2>&1 && ok "shapes applied" || bad "shapes apply failed"
else
  bad "missing /tmp/shapes.yaml: the run would carry no declaration shapes"
fi
for d in diurnal-web load-generator slow-leaker churn-app resize-target cpu-hog \
         tomcat-lowxmx guaranteed-app overprovisioned \
         metrics-app no-limits-api guaranteed-cache; do
  kubectl -n "$NS" rollout status deploy/$d --timeout=120s >/dev/null 2>&1 \
    || bad "rollout $d did not become ready"
done
kubectl -n "$NS" rollout status ds/node-agent --timeout=120s >/dev/null 2>&1 \
  || bad "daemonset node-agent did not become ready"
ok "all deployments ready"

say "5. exercise the churn-app redeploy"
REV0=$(kubectl -n "$NS" get deploy churn-app -o jsonpath='{.metadata.annotations.deployment\.kubernetes\.io/revision}')
kubectl -n "$NS" delete job redeploy-manual >/dev/null 2>&1
kubectl -n "$NS" create job redeploy-manual --from=cronjob/redeploy-churn-app >/dev/null 2>&1
kubectl -n "$NS" wait --for=condition=complete job/redeploy-manual --timeout=120s >/dev/null 2>&1
REV1=$(kubectl -n "$NS" get deploy churn-app -o jsonpath='{.metadata.annotations.deployment\.kubernetes\.io/revision}')
[ "$REV0" != "$REV1" ] && ok "churn-app revision $REV0 -> $REV1" \
                       || bad "churn-app revision did not move (still $REV0)"

say "6. exercise the resize-target limit toggle, in place"
POD0=$(kubectl -n "$NS" get pod -l app=resize-target -o jsonpath='{.items[0].metadata.name}')
LIM0=$(kubectl -n "$NS" get pod "$POD0" -o jsonpath='{.spec.containers[0].resources.limits.memory}')
kubectl -n "$NS" delete job toggle-manual >/dev/null 2>&1
kubectl -n "$NS" create job toggle-manual --from=cronjob/toggle-resize-limit >/dev/null 2>&1
kubectl -n "$NS" wait --for=condition=complete job/toggle-manual --timeout=120s >/dev/null 2>&1
POD1=$(kubectl -n "$NS" get pod -l app=resize-target -o jsonpath='{.items[0].metadata.name}')
LIM1=$(kubectl -n "$NS" get pod "$POD1" -o jsonpath='{.spec.containers[0].resources.limits.memory}')
RC=$(kubectl -n "$NS" get pod "$POD1" -o jsonpath='{.status.containerStatuses[0].restartCount}')
[ "$LIM0" != "$LIM1" ] && ok "limit $LIM0 -> $LIM1" || bad "limit did not move (still $LIM0)"
[ "$POD0" = "$POD1" ]  && ok "same pod survived ($POD1)" \
                       || bad "pod was replaced ($POD0 -> $POD1): limit-trend is dead again"
[ "$RC" = "0" ]        && ok "container did not restart" || bad "container restarted $RC times"

say "7. the load generator is actually loading something"
Q() { curl -s --get "http://192.168.1.122:30090/api/v1/query" --data-urlencode "query=$1" \
      | python3 -c 'import sys,json;d=json.load(sys.stdin)["data"]["result"];print(round(sum(float(r["value"][1]) for r in d)*1000,1) if d else 0)'; }
SRV=$(Q 'sum(rate(container_cpu_usage_seconds_total{namespace="tare-lab",pod=~"diurnal-web.*",container="nginx"}[5m]))')
CLI=$(Q 'sum(rate(container_cpu_usage_seconds_total{namespace="tare-lab",pod=~"load-generator.*",container="gen"}[5m]))')
echo "  target ${SRV}m  client ${CLI}m"
python3 -c "
import sys
s,c=float('$SRV'),float('$CLI')
sys.exit(0 if s>30 and c>0 and s/c>1.5 else 1)" \
  && ok "target outweighs client (ratio $(python3 -c "print(round(float('$SRV')/max(float('$CLI'),0.001),1))"))" \
  || bad "client outweighs target, or target is idle: the generator is broken again"

say "8. the trend signals are live in the record the collector actually writes"
# Check the artifact, not a proxy. An earlier version of this script ran its own
# `--window 6h` query here while snapshot.sh used a different window, so the
# gate passed on a measurement nobody was collecting. Take the real first
# snapshot now and inspect the row it wrote; discard it if it is not sound.
printf 'ts,window,namespace,workload,container,verdict,confidence,samples,cpuReq,recCPU,recCPULimit,memReq,recMem,recMemLimit,psi,psiPeak,throttle,throttlePeak,oom,restarts,cpuFloored,memFloored,memEarly,memLate,memLimitEarly,memLimitLate\n' > "$DIR/history.csv"
date -u +%s > "$DIR/STARTED_AT"
"$DIR/snapshot.sh"
python3 - "$DIR/history.csv" <<'PY' || FAIL=1
import csv, sys
rows = list(csv.DictReader(open(sys.argv[1])))
if not rows:
    print("  FAIL  the first snapshot wrote no rows"); sys.exit(1)
print("  window actually used: %s" % rows[0]["window"])
bad = 0
if any(r["workload"].startswith(("Job/", "CronJob/")) for r in rows):
    print("  FAIL  Job rows are still reaching history.csv"); bad = 1
else:
    print("  OK    Jobs filtered (%d workloads)" % len(rows))
# slow-leaker is the check: a long-lived pod that nothing in this script
# restarted, so it must have an early observation immediately.
sl = [r for r in rows if "slow-leaker" in r["workload"]]
if not sl:
    print("  FAIL  slow-leaker missing from the record"); bad = 1
elif int(sl[0]["memEarly"]) > 0:
    print("  OK    memory-trend live in the record (early=%dMiB late=%dMiB)"
          % (int(sl[0]["memEarly"])//2**20, int(sl[0]["memLate"])//2**20))
else:
    print("  FAIL  memEarly is zero in the collected row: the window the"
          "\n        collector uses has no trend signal"); bad = 1
sys.exit(bad)
PY
echo "  NOTE  MemLimitEarly for resize-target is expected to be zero right now:"
echo "        its pod was just replaced by the resizePolicy change. It becomes"
echo "        live once the pod is older than the window. Re-check before the"
echo "        first toggle with:  python3 $DIR/stability.py"

# Keep what the exercise jobs said before removing them. These logs are the
# only record of which way the limit toggled and whether the pod survived, and
# an earlier version of this script deleted them unread - which is how a
# discrepancy in the toggle direction became unexplainable an hour later.
mkdir -p "$DIR/exercise"
for j in redeploy-manual toggle-manual; do
  kubectl -n "$NS" logs "job/$j" > "$DIR/exercise/$TS-$j.log" 2>&1 || true
done
kubectl -n "$NS" get pod -l app=resize-target -o yaml > "$DIR/exercise/$TS-resize-target-pod.yaml" 2>&1 || true
kubectl -n "$NS" delete job redeploy-manual toggle-manual >/dev/null 2>&1

say "result"
if [ "$FAIL" -ne 0 ]; then
  rm -f "$DIR/history.csv" "$DIR/STARTED_AT"
  echo "NOT STARTING THE CLOCK - fix the failures above and re-run."
  echo "Nothing is collecting, and the trial snapshot was discarded."
  exit 1
fi

( crontab -l 2>/dev/null | grep -v tare-longrun; echo "17 * * * * $DIR/snapshot.sh >/dev/null 2>&1" ) | crontab -

echo "Every time-dependent path changed something observable."
echo "Clock started $(date -u). First snapshot taken; cron installed at :17."
echo "  48h check:  $(date -u -d '+2 days' +%Y-%m-%d)"
echo "  day 7:      $(date -u -d '+7 days' +%Y-%m-%d)"
echo "  day 14:     $(date -u -d '+14 days' +%Y-%m-%d)"
echo "  analyse:    ssh msi 'python3 ~/tare-longrun/stability.py'"
