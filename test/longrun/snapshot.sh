#!/bin/sh
# Hourly snapshot of tare findings for the long run.
#
# The point is not any single report: it is whether the same workload gets the
# same numbers on day 3, 7 and 14. A recommender that oscillates is unusable
# however correct any individual answer is, and that can only be seen over time.
set -u
export PATH="$HOME/.local/bin:$PATH"
DIR="$HOME/tare-longrun"
TS=$(date -u +%Y%m%dT%H%M%SZ)

# The observation window tracks how much data actually exists, capped at 7d.
#
# A fixed 7d window on a young cluster is not merely imprecise, it is silently
# inert: tare's early-window queries carry `offset window-1h`, so on a cluster
# holding 18 hours they return no series at all. MemEarly and MemLimitEarly
# stay zero, and both leak detection and limit-trend ceiling inference are
# guarded on those being non-zero. Attempt 2 had leak detection dead for its
# whole life for exactly this reason. From day 7 the window is constant at 7d, so the day-7 and
# day-14 comparison the experiment exists for is unaffected.
START_FILE="$DIR/STARTED_AT"
[ -f "$START_FILE" ] || date -u +%s > "$START_FILE"
#
# The floor is 6h, not something smaller, for a second reason: tare only adds
# the early-window queries at all when the window is >= 4h, so any window below
# that has no trend signal by construction, not merely a short one. 6h leaves
# margin above that edge.
ELAPSED_H=$(( ( $(date -u +%s) - $(cat "$START_FILE") ) / 3600 ))
[ "$ELAPSED_H" -lt 6 ] && ELAPSED_H=6
[ "$ELAPSED_H" -gt 168 ] && ELAPSED_H=168
WINDOW="${TARE_WINDOW:-${ELAPSED_H}h}"

OUT="$DIR/snapshots/$TS.json"
if ! kubectl-tare report -n tare-lab --window "$WINDOW" -o json > "$OUT" 2>"$DIR/snapshots/$TS.err"; then
  echo "$TS FAILED $(head -c 200 "$DIR/snapshots/$TS.err")" >> "$DIR/errors.log"
  exit 0
fi
rm -f "$DIR/snapshots/$TS.err"

# One row per container per snapshot: the shape stability analysis needs.
#
# The trend inputs (MemEarly/MemLate, MemLimitEarly/MemLimitLate) are recorded
# alongside the outputs deliberately. Three of this experiment's restarts were
# caused by an input that was silently absent while the output still looked
# plausible; a zero in these columns makes that visible in the record itself
# rather than requiring someone to go and ask Prometheus about it.
python3 - "$OUT" "$TS" "$WINDOW" >> "$DIR/history.csv" <<'PY'
import json, sys
path, ts, window = sys.argv[1], sys.argv[2], sys.argv[3]
rows = json.load(open(path))
for r in rows or []:
    # Jobs are five-second pods that exist only to poke the lab. They can never
    # hold a stable recommendation and they pollute every stability table.
    if r.get("Workload", "").startswith(("Job/", "CronJob/")):
        continue
    print(",".join(str(x) for x in [
        ts, window, r.get("Namespace",""), r.get("Workload",""), r.get("Container",""),
        r.get("Verdict",""), r.get("Confidence",""), r.get("Samples",0),
        r.get("CPURequest",0), r.get("RecCPU",0), r.get("RecCPULimit",0),
        r.get("MemRequest",0), r.get("RecMem",0), r.get("RecMemLimit",0),
        # Both halves of each pressure signal. The verdict rests on the peak
        # and the report shows the mean beside it; recording only one leaves a
        # reader unable to tell a container that stalls constantly from one
        # that was unlucky for a minute.
        round(r.get("PSIWaiting",0), 5), round(r.get("PSIWaitingPeak",0), 4),
        round(r.get("Throttle",0), 5), round(r.get("ThrottlePeak",0), 4),
        r.get("OOMKilled", False), r.get("Restarts",0),
        r.get("CPUFloored", False), r.get("MemFloored", False),
        int(r.get("MemEarly",0)), int(r.get("MemLate",0)),
        int(r.get("MemLimitEarly",0)), int(r.get("MemLimitLate",0)),
    ]))
PY
