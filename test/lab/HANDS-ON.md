# Hands-on session

About 75 minutes, for a group of colleagues on the office network. Part 1 needs
no cluster. Parts 2 and 3 use the shared lab cluster described in
`test/lab/README.md`: k3d `tare-test` on `msi` (192.168.1.122), context
`k3d-tare-test`, Grafana on :30030.

**Ground rules for the lab.** It is shared, and it is running a long stability
experiment. tare only reads, so `report`, `check` and `diff` are always safe.
Do not run the output of `-o patch`. Do not `kubectl apply`, `edit` or `delete`
anything in `tare-lab`. Use `diff --write` only on your own scratch copy of the
manifests (part 3), never on this repository.

## Before the session (each person, 10 minutes)

```sh
git clone <this repo> tare && cd tare
./tools/build.sh                      # needs Go 1.27; produces bin/kubectl-tare
export PATH=$PWD/bin:$PATH            # now `kubectl tare` works
kubectl tare version
kubectl tare report --demo            # must print a table; no cluster involved
```

Get the lab kubeconfig from whoever runs the session, then check you can reach
it:

```sh
kubectl --context k3d-tare-test get pods -n tare-lab
```

`jq` and a browser are useful but not required.

## Part 1: read a report without a cluster (15 minutes)

```sh
kubectl tare report --demo
kubectl tare report --demo --explain | less
```

The demo is the real analyzer run over simulated series, so every verdict is
computed, not canned. Answer from the output alone:

1. `leaky-service` and `slow-growth` have no arrow in `CPU` or `MEM`. Why does
   tare refuse to give them a size?
2. `hidden-ceiling-jvm` uses 240Mi at peak, yet the recommendation is 368Mi.
   Where does the extra come from, given that the pod spec sets no heap flag?
3. Applying the `no-limits-api` recommendation changes more than its size. What
   else changes?
4. `dormant-sidecar` is `IDLE`. What would you check before deleting it?
5. The summary line says nothing about nodes freed. Why not?

<details><summary>Answers</summary>

1. `MEM_LEAK`: memory grew 25.6x (leaky-service) and 3.0x (slow-growth) across
   the window. No size holds a leak, so the recommendation is withheld rather
   than invented.
2. `CEILING observed-plateau`: memory sits flat far below its limit, which for a
   JVM usually means a heap cap set in the image or a ConfigMap. tare infers the
   ceiling and raises the number (`MEM_RAISED_BY_CEILING`) instead of trusting
   usage.
3. `CPU_LIMIT_WOULD_BE_ADDED` and `MEM_LIMIT_WOULD_BE_ADDED`: the manifest sets
   no limits, so applying this introduces throttling at quota and makes the
   container OOMKillable at a cap. That is a behaviour change, not a resize.
4. `IDLE_NOT_RULED_OUT`: hot standby, DR replica, a controller waiting on a rare
   event, a job with a period longer than the window. tare sees CPU and memory,
   not intent. Ask the owner first.
5. `NODES_NOT_COMPUTED` on stderr: the demo has no real nodes, and a node count
   from imaginary nodes would be the misleading number tare exists to avoid.

</details>

Then try the machine-readable side:

```sh
# every finding and the evidence codes behind it
kubectl tare report --demo -o wide | sed -n '/section=evidence/,/^$/p'
# the same reasons with raw numbers
kubectl tare report --demo -o json | jq '.findings[] | {workload: .Workload, verdict: .Verdict}'
```

## Part 2: the lab cluster (20 minutes)

```sh
kubectl config use-context k3d-tare-test
kubectl tare check
kubectl tare report -n tare-lab
kubectl tare report -n tare-lab --explain | less
kubectl tare report -n tare-lab -o tui      # enter on a row plots it over the window
kubectl tare report -n tare-lab -o html > lab.html && open lab.html
```

Several people querying at once all go through the API server proxy to one
Prometheus. If a run times out, add `--timeout 5m`.

1. In `check`, read the `history` and `coverage` lines. How much history does
   the lab have, and what does that do to every finding?
2. `cpu-hog` sits flat at 100m of usage. A usage-based right-sizer would call
   that correctly sized. What does tare see that usage cannot show?
3. Open `cpu-hog` in Grafana (container CPU for `tare-lab/cpu-hog`) next to its
   tare row. Which of the two would you show a team to justify raising the limit?
4. `tomcat-lowxmx` is nearly idle, so why is its memory recommendation 384Mi
   and not something close to its 95Mi of usage?
5. `guaranteed-cache` uses about 27m against a 100m limit, yet tare keeps the
   limit at 100m. Why?

<details><summary>Answers</summary>

1. On 2026-10-11 it was 6h, because the lab machine was off for two weeks.
   Every finding is at most `medium` confidence, and the report says
   `WINDOW_SHORT`. Six hours cannot show a daily cycle. Keep that in mind for
   part 3.
2. `CPU_PRESSURE` of about 0.9 s/s: its tasks were ready to run and waiting 90%
   of the time. `LIMIT_NOT_NODE` adds that the node was about 8% busy, so the
   wait comes from its own limit, not from the machine.
3. Discussion. The Grafana graph shows a flat line; only the pressure figure
   shows that the line is a ceiling.
4. `JAVA_OPTS=-Xmx256m`. A 256Mi heap plus metaspace, code cache and thread
   stacks comes to about 384Mi. Usage is what an idle JVM touched, not what it
   will allocate under load.
5. `CPU_LIMIT_HELD_THROTTLED`: the limit throttled it in about 88% of scheduling
   periods. Usage is averaged over seconds, while CFS enforces the quota every
   100ms, so a limit sized from averaged usage would throttle it harder.

</details>

## Part 3: from finding to pull request (20 minutes)

Work on a throwaway copy of the lab manifests, never on this repository:

```sh
cp -r test/lab /tmp/tare-handson && cd /tmp/tare-handson
git init -q && git add . && git commit -qm "lab manifests as deployed"

kubectl tare diff -n tare-lab --repo .                          # dry run: what would change
kubectl tare diff -n tare-lab --repo . --write --pr-body pr.md  # edit the files
git diff
less pr.md
```

Check that the diff touches only resource values, keeps comments and formatting
intact, and that `pr.md` lists what tare deliberately left alone and why.

Now review it as if a colleague had opened this pull request. **Find the hunks
you would reject.** There are at least three, and tare gives none of them a
low-confidence warning.

<details><summary>Answers</summary>

- **`diurnal-web` in `longrun.yaml`:** memory 256Mi to 16Mi and CPU limit
  1000m to 562m, sized from 6h of a workload whose load follows the day. The
  window has not seen a peak. The report flags `WINDOW_SHORT` once, but the diff
  does not repeat it at the hunk.
- **`overcommitter` in `commitment.yaml`:** limits go from 14 cores and 6Gi to
  10m and 20Mi. The findings table showed `16Mi→16Mi`, because it displays
  requests, so the largest change in the PR was never visible in the report. Read
  `-o wide` before you trust a diff.
- **`cpu-hog`:** 115m is a guess. The container was pinned at its 100m limit the
  whole time, so its real demand was never observed, and any number above 100m
  is a step chosen without evidence. tare does say this for memory after an
  OOMKill (`16Mi→?`), but not yet for CPU.
- Also worth discussing: the `commitment.yaml` container with no `resources` at
  all gains a full block, which moves it out of BestEffort. The PR body says so;
  the diff alone does not.

These are known open issues, not mistakes in your setup.

</details>

## Discussion (10 minutes)

- Which of our clusters could run this today? Run `kubectl tare check` there
  (read-only) and see what is missing.
- Which finding would you act on without asking anyone, and which would you
  never apply without the owner?
- What would make a pull request from tare mergeable in your team?

To report a number that looks wrong, include `kubectl tare version` and the
output of the same run with `--redact`.
