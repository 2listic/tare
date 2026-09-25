#!/usr/bin/env python3
"""Report whether tare's recommendations are stable over time.

A recommender that oscillates is unusable however correct any single answer is,
and stability is the one property only a long run can show. Everything else in
this project is tested in seconds.
"""
import csv
import sys
import collections
import statistics
import pathlib

path = pathlib.Path(sys.argv[1] if len(sys.argv) > 1
                    else pathlib.Path.home() / "tare-longrun/history.csv")
rows = list(csv.DictReader(path.open()))
if not rows:
    sys.exit("no snapshots yet")

by = collections.defaultdict(list)
for r in rows:
    by[(r["workload"], r["container"])].append(r)

stamps = sorted({r["ts"] for r in rows})
# Sorted by duration, not alphabetically: "10h" sorts before "6h" as a string,
# which printed the window sequence backwards and made a growing window look
# like a shrinking one.
def _hours(w):
    try:
        return int(w.rstrip("hd")) * (24 if w.endswith("d") else 1)
    except ValueError:
        return -1


windows = sorted({r.get("window", "?") for r in rows}, key=_hours)
print("%d snapshots, %s .. %s, window %s\n"
      % (len(stamps), stamps[0], stamps[-1], "->".join(windows)))

# Instrument health comes before any result. Three of this experiment's
# restarts were caused by an input that was silently absent while the output
# still looked plausible: a load generator sending no load, two cronjobs in
# ImagePullBackOff, and early-window queries whose offset fell off the end of
# Prometheus retention. Each produced a clean history.csv. A dead input must be
# louder than the numbers it feeds, because the numbers will not look wrong.
dead = []
latest = [r for r in rows if r["ts"] == stamps[-1]]
if all(int(r.get("memEarly", 0)) == 0 for r in latest):
    dead.append("memory-trend: MemEarly is zero for every workload, so leak "
                "detection cannot fire at all. slow-leaker will never be "
                "reported LEAKING no matter how it behaves")
if all(int(r.get("memLimitEarly", 0)) == 0 for r in latest):
    dead.append("limit-trend: MemLimitEarly is zero for every workload, so "
                "limit-movement ceiling inference cannot fire. resize-target "
                "is measuring nothing")
# The all() tests above only catch a signal that died on every workload at
# once. churn-app is redeployed every six hours and legitimately has no early
# observation, so it holds both of those tests false forever and masks a
# workload that really did go dark.
#
# The per-workload test below is the weak one that let attempt 5 run 28
# snapshots blind: requiring BOTH halves to be zero only catches a container
# with no memory data at all, and the failure that actually happens is an
# early-only zero, which is what a mid-run pod replacement produces. The early
# query offsets by window-1h and always lands before the new pod exists.
#
# A transition catches it without needing an exemption list. A workload whose
# early signal was live and is now zero has gone dark mid-run; churn-app is
# zero from its first snapshot and never transitions, so it cannot trip this.
# history.csv records no pod name, so this is the only evidence available here
# that a replacement happened at all.
for r in sorted(latest, key=lambda r: r["workload"]):
    for col, what in (("memEarly", "memory-trend"), ("memLimitEarly", "limit-trend")):
        if int(r.get(col, 0)) != 0:
            continue
        series = by[(r["workload"], r["container"])]
        live = [x for x in series if int(x.get(col, 0)) != 0]
        if not live:
            continue  # never had one: churn-app, or a pod younger than the run
        went_dark = next(x["ts"] for x in series
                         if x["ts"] > live[-1]["ts"] and int(x.get(col, 0)) == 0)
        dead.append("%s: %s was live until %s and has been zero since %s (%d of "
                    "%d snapshots). Whatever is printed for it below was decided "
                    "without it" % (r["workload"], what, live[-1]["ts"], went_dark,
                                    sum(1 for x in series
                                        if x["ts"] > live[-1]["ts"]), len(series)))
    if int(r.get("memEarly", 0)) == 0 and int(r.get("memLate", 0)) == 0:
        dead.append("%s reported no memory data at all (memEarly and memLate "
                    "both zero). It is not running, and whatever is printed "
                    "for it below was computed from usage it no longer has"
                    % r["workload"])
    if float(r.get("restarts", 0)) >= 3:
        dead.append("%s restarted %.0f times inside the window: its samples "
                    "describe a container that kept dying rather than one "
                    "running at this size" % (r["workload"], float(r["restarts"])))

moved = {r["workload"] for r in rows
         if int(r.get("memLimitEarly", 0)) and int(r.get("memLimitLate", 0))
         and r["memLimitEarly"] != r["memLimitLate"]}
if not moved and len(stamps) > 48:
    dead.append("limit-trend: no workload has shown a limit that moved across "
                "the window in %d snapshots; the resize-target toggle is "
                "probably not firing" % len(stamps))
# A SATURATED verdict whose sustained pressure is far below the threshold means
# the verdict rests on one unlucky bucket. That was the resize-target defect:
# peak 0.031 against a mean of 0.0007, reported starved while using 1.5% of its
# request. The utilisation guard should prevent it now, so this firing means
# the guard has been weakened or a genuinely new case has appeared.
peaky = sorted({
    "%s (mean %.4f, peak %.3f)" % (r["workload"], float(r["psi"]), float(r["psiPeak"]))
    for r in latest
    if r["verdict"] == "SATURATED" and "psi" in r and r.get("psiPeak")
    and float(r["psi"]) < 0.03 <= float(r["psiPeak"])
})
for p_ in peaky:
    dead.append("saturation resting on a peak, not sustained pressure: %s" % p_)

if dead:
    print("INSTRUMENT DEAD - the results below are not evidence:")
    for d in dead:
        print("  ! %s" % d)
    print()
print("%-34s %3s %-24s %16s %16s %s"
      % ("WORKLOAD/CONTAINER", "N", "VERDICTS", "CPU rec (m)", "MEM rec (MiB)", "CONF"))


def spread(xs, floored=False):
    """Summarise a series of recommendations.

    `floored` marks a series that is pinned to MinCPUMillicores / MinMemBytes.
    Such a series is constant by construction and says nothing about whether the
    recommender is stable, so it must never be reported as "stable" - that is a
    pass earned without the analyzer ever having touched the data.
    """
    if not xs:
        return "-"
    if len(set(xs)) == 1:
        return "%d FLOOR" % xs[0] if floored else "%d stable" % xs[0]
    mean = statistics.mean(xs)
    cv = statistics.pstdev(xs) / mean if mean else 0
    tag = " FLOOR" if floored else ""
    return "%d-%d cv%.2f%s" % (min(xs), max(xs), cv, tag)


# Recommendations taken through different windows are not the same quantity.
#
# The window grows with elapsed run time until it caps, so the early snapshots
# summarise less than one diurnal cycle and the late ones summarise several. A
# coefficient of variation computed across both measures the window widening,
# not the recommender moving.
#
# Attempt 5 was reported as failing on this: diurnal-web showed cv 0.18 across
# all 88 snapshots and was called out as "the recommender chasing the daily
# curve". Segmented, it is cv 0.319 below 24h and **cv 0.004** above 72h - the
# recommender is stable to within 5m once the window covers a cycle, and the
# aggregate was measuring the instrument. Same shape as every other defect in
# this project: a clean, plausible, wrong number.
SETTLED_HOURS = 48


def settled(rs):
    """Snapshots whose window is wide enough to compare with each other."""
    return [r for r in rs if _hours(r.get("window", "?")) >= SETTLED_HOURS]


for (wl, c), rs in sorted(by.items()):
    use = settled(rs)
    narrow = len(rs) - len(use)
    if not use:
        # Nothing has a settled window yet. Report that rather than computing a
        # spread over measurements that are not comparable.
        print("%-34s %3d %-24s %16s %16s %s"
              % (wl + "/" + c, len(rs), "window <%dh" % SETTLED_HOURS,
                 "not comparable", "not comparable", "-"))
        continue
    cpus = [int(r["recCPU"]) for r in use if int(r["recCPU"]) > 0]
    mems = [int(r["recMem"]) // (1 << 20) for r in use if int(r["recMem"]) > 0]
    cpu_floored = any(r.get("cpuFloored") == "True" for r in use)
    mem_floored = any(r.get("memFloored") == "True" for r in use)
    verdicts = collections.Counter(r["verdict"] for r in rs)  # verdicts: all of them
    vs = ",".join("%sx%d" % (v, n) for v, n in verdicts.most_common())
    confs = sorted({r["confidence"] for r in use})
    label = wl + "/" + c
    print("%-34s %3d %-24s %16s %16s %s"
          % (label, len(use), vs, spread(cpus, cpu_floored),
             spread(mems, mem_floored), ",".join(confs)))
    if narrow:
        print("%-34s %3s %s" % ("", "",
              "  (%d earlier snapshot(s) excluded: window under %dh is not comparable)"
              % (narrow, SETTLED_HOURS)))

oom = [r for r in rows if r["oom"] == "True"]
if oom:
    names = sorted({r["workload"] + "/" + r["container"] for r in oom})
    print("\nOOMKilled at some point: %s" % ", ".join(names))

print("""
What to look for:
  - a series marked FLOOR is pinned at MinCPUMillicores/MinMemBytes and is
    constant no matter what the data does. It is not evidence of stability;
    exclude it before drawing any conclusion about the recommender
  - verdicts settle rather than alternating between WASTE and SATURATED
  - cv above ~0.15 on a steady workload means the recommendation is noisy.
    N counts only snapshots whose window is at least 48h: a recommendation
    taken through a 6h window and one taken through an 86h window are
    different measurements, and a cv across both reports the window widening
    rather than the recommender moving
  - confidence climbs low -> medium -> high as the window fills
  - slow-leaker reaches LEAKING and stays; churn-app must never be called a leak
  - resize-target keeps a flat plateau while its limit is toggled, which is what
    should trigger the strong form of ceiling inference
  - psi/psiPeak are sustained and peak pressure. A SATURATED verdict wants the
    sustained figure above the threshold; if only the peak clears it, the
    container stalled once rather than continuously
  - memEarly/memLimitEarly are the trend inputs. A zero there means the signal
    was never measured, which is different from a workload that did not move,
    and it is reported above rather than left for the reader to notice""")
