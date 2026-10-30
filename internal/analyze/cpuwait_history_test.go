package analyze_test

import (
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/2listic/tare/internal/analyze"
	"github.com/2listic/tare/internal/discover"
	"github.com/2listic/tare/internal/metrics"
)

// replaced is one Deployment seen through two pods over a week: aaaaa, which
// is gone, and bbbbb, which runs now. Each pod's span and CPU wait are the
// caller's, so the pod that holds the history and the pod that holds the stall
// can be put on either side.
func replaced(gone, live metrics.CPUWait, goneFrom, goneTo, liveFrom float64) ([]discover.Container, map[metrics.Key]*metrics.Observation) {
	const now = 1_800_000_000.0
	c := discover.Container{
		Namespace: "shop", Pod: "api-6d9f-bbbbb", Name: "api",
		Workload: "Deployment/api", WorkloadKind: "Deployment",
		CPURequest: 1000, MemRequest: 1024 * mib, QoS: "Burstable",
	}
	obs := func(pod string, w metrics.CPUWait, from, to float64) *metrics.Observation {
		o := &metrics.Observation{
			Key:         metrics.Key{Namespace: "shop", Pod: pod, Container: "api"},
			CPUUsageP95: 0.2, CPUUsageMax: 0.9, CPUSteadyMax: 0.9,
			MemWorkingSet: 100 * mib, MemP95: 100 * mib,
			CPUWait: w,
			Samples: int((to - from) / 15), FirstSeen: from, LastSeen: to,
			Owner: "Deployment/api",
		}
		for _, s := range []metrics.Signal{metrics.SigCPUUsage, metrics.SigCPUPeak, metrics.SigMemory,
			metrics.SigMemP95, metrics.SigOOM, metrics.SigRestarts, metrics.SigCPUWait} {
			o.Mark(s)
		}
		return o
	}
	g := obs("api-6d9f-aaaaa", gone, now+goneFrom, now+goneTo)
	l := obs(c.Pod, live, now+liveFrom, now)
	return []discover.Container{c}, map[metrics.Key]*metrics.Observation{g.Key: g, l.Key: l}
}

var (
	week = -(7 * 24 * time.Hour).Seconds()
	hour = -time.Hour.Seconds()
	// quietWait is a week of a container the rule finds not starved.
	quietWait = metrics.CPUWait{Stretches: 2016, Stalled: 0.01, Normal: 0.05, HasNormal: true, Mean: 0.06, Coverage: 0.85, Busy: true}
)

// A pod replaced an hour ago has an hour of CPU wait of its own, but the week
// its predecessor ran is the same workload's: the history check passes on it.
func TestReplacedPodKeepsItsPredecessorsCPUWait(t *testing.T) {
	young := quietWait
	young.Stretches = 12
	cs, obs := replaced(quietWait, young, week, hour, hour)
	f := judge(t, cs, obs)
	if hasCode(f, analyze.CPUWaitShortHistory) || !f.HasCPUWait() {
		t.Fatalf("a week of the workload's CPU wait dropped for the new pod's hour: %v", analyze.ReasonTexts(f.Reasons))
	}
}

// A gone pod that lived an hour has a normal made of that hour, and a stall
// measured against it means nothing: alive, it would be withdrawn as short
// history. Its stall must not decide a workload whose live pod has a week of
// quiet history behind it.
func TestShortLivedGonePodsStallIsNotTheWorkloads(t *testing.T) {
	brief := metrics.CPUWait{Stretches: 12, Stalled: 0.5, LongestRun: 6, Normal: 0.01, HasNormal: true, Mean: 0.2, Coverage: 0.85, Busy: true}
	cs, obs := replaced(brief, quietWait, week, week-hour, week)
	f := judge(t, cs, obs)
	if f.Verdict == analyze.Saturated || hasCode(f, analyze.CPUWaitRelative) {
		t.Fatalf("an hour-old normal decided the workload: verdict %s, %v", f.Verdict, analyze.ReasonTexts(f.Reasons))
	}
}

// A gone pod the agent barely saw says nothing about the workload either way:
// its lack of wait is not evidence, and neither is it doubt about the live pod
// the agent sees well.
func TestGonePodsLowCoverageDoesNotWithdrawCPUWait(t *testing.T) {
	blind := quietWait
	blind.Stretches, blind.Coverage = 288, 0.1
	day := -(24 * time.Hour).Seconds()
	cs, obs := replaced(blind, quietWait, week, week-day, week)
	f := judge(t, cs, obs)
	if hasCode(f, analyze.CPUWaitLowCoverage) || !f.HasCPUWait() {
		t.Fatalf("a gone pod the agent could not see withdrew the live pod's CPU wait: %v", analyze.ReasonTexts(f.Reasons))
	}
}

// twoReplicas is waitContainer with a second live replica beside it, in the
// order given: pods are folded in the order discover lists them.
func twoReplicas(first, second metrics.CPUWait) ([]discover.Container, map[metrics.Key]*metrics.Observation) {
	cs, obs := waitContainer(first)
	k := metrics.Key{Namespace: "shop", Pod: "api-0", Container: "api"}
	c2, o2 := cs[0], *obs[k]
	c2.Pod, o2.Key.Pod = "api-1", "api-1"
	o2.CPUWait = second
	obs[o2.Key] = &o2
	return append(cs, c2), obs
}

// A replica added an hour ago is the same as a gone pod that lived an hour:
// its normal is that hour, and alone it would be withdrawn as short history.
// Live, its stall must not decide either, whichever replica is folded first.
func TestNewReplicasStallIsNotTheWorkloads(t *testing.T) {
	young := metrics.CPUWait{Stretches: 12, Stalled: 0.5, LongestRun: 6, Normal: 0.01, HasNormal: true, Mean: 0.2, Coverage: 0.85, Busy: true}
	for name, pods := range map[string][2]metrics.CPUWait{
		"old replica first":   {quietWait, young},
		"young replica first": {young, quietWait},
	} {
		t.Run(name, func(t *testing.T) {
			cs, obs := twoReplicas(pods[0], pods[1])
			f := judge(t, cs, obs)
			if f.Verdict == analyze.Saturated || hasCode(f, analyze.CPUWaitRelative) || hasCode(f, analyze.CPUWaitShortHistory) {
				t.Fatalf("an hour-old replica decided the workload: verdict %s, %v", f.Verdict, analyze.ReasonTexts(f.Reasons))
			}
			if f.CPUWait.Stretches != quietWait.Stretches || f.CPUWait.Normal != quietWait.Normal {
				t.Errorf("row carries %d stretches and normal %v, want the old replica's %d and %v",
					f.CPUWait.Stretches, f.CPUWait.Normal, quietWait.Stretches, quietWait.Normal)
			}
		})
	}
}

// A starvation its node confirmed on the second replica is starvation of the
// workload, as on the first.
func TestConfirmedRunOnAnyReplicaCounts(t *testing.T) {
	confirmed := quietWait
	confirmed.Stalled, confirmed.LongestRun, confirmed.LongestConfirmedRun, confirmed.NodeKnown = 0.002, 4, 4, 1
	cs, obs := twoReplicas(quietWait, confirmed)
	f := judge(t, cs, obs)
	if f.Verdict != analyze.Saturated || !hasCode(f, analyze.CPUWaitNodeConfirmed) {
		t.Fatalf("confirmed run on the second replica lost: verdict %s, %v", f.Verdict, analyze.ReasonTexts(f.Reasons))
	}
}

// A Deployment on a test cluster (2026-10-29), replaced under contention: its
// new pod starved for 22 minutes its node confirmed, beside the quiet history
// of the pod it replaced. The run needs no normal, so the new pod's short history does not
// discard it, live or beside a live replica, whichever is folded first.
func TestShortPodsConfirmedRunCounts(t *testing.T) {
	young := metrics.CPUWait{Stretches: 12, Stalled: 0.33, LongestRun: 4, LongestConfirmedRun: 4, NodeKnown: 1, NodeContended: 0.33,
		Normal: 0.01, HasNormal: true, Mean: 0.6, Coverage: 0.85, Busy: true}
	check := func(t *testing.T, f analyze.Finding) {
		t.Helper()
		if f.Verdict != analyze.Saturated || !hasCode(f, analyze.CPUWaitNodeConfirmed) {
			t.Fatalf("a confirmed starvation of the short pod lost: verdict %s, %v", f.Verdict, analyze.ReasonTexts(f.Reasons))
		}
		if hasCode(f, analyze.CPUWaitRelative) {
			t.Errorf("the short pod's relative stall decided: %v", analyze.ReasonTexts(f.Reasons))
		}
	}
	t.Run("replaced", func(t *testing.T) {
		cs, obs := replaced(quietWait, young, week, hour, hour)
		check(t, judge(t, cs, obs))
	})
	for name, pods := range map[string][2]metrics.CPUWait{
		"old replica first":   {quietWait, young},
		"young replica first": {young, quietWait},
	} {
		t.Run(name, func(t *testing.T) {
			cs, obs := twoReplicas(pods[0], pods[1])
			check(t, judge(t, cs, obs))
		})
	}
}

// A live replica with no CPU wait at all (no agent on its node, or one that
// failed its self-checks) may be the starved one. The quiet replica beside it
// cannot vouch for it, whichever is folded first.
func TestUnseenReplicaWithdrawsCPUWait(t *testing.T) {
	for _, unseen := range []string{"api-0", "api-1"} {
		t.Run(unseen, func(t *testing.T) {
			cs, obs := twoReplicas(quietWait, quietWait)
			o := obs[metrics.Key{Namespace: "shop", Pod: unseen, Container: "api"}]
			o.Have = maps.Clone(o.Have)
			delete(o.Have, metrics.SigCPUWait)
			o.CPUWait = metrics.CPUWait{}
			f := judge(t, cs, obs)
			if !hasCode(f, analyze.CPUWaitReplicaUnseen) || f.HasCPUWait() || f.Verdict == analyze.Waste {
				t.Fatalf("a replica the agent cannot see was vouched for: verdict %s, %v", f.Verdict, analyze.ReasonTexts(f.Reasons))
			}
		})
	}
	// A replica the agent sees starved is starvation whatever the unseen one did.
	cs, obs := twoReplicas(starvedWait, quietWait)
	o := obs[metrics.Key{Namespace: "shop", Pod: "api-1", Container: "api"}]
	o.Have = maps.Clone(o.Have)
	delete(o.Have, metrics.SigCPUWait)
	if f := judge(t, cs, obs); f.Verdict != analyze.Saturated || hasCode(f, analyze.CPUWaitReplicaUnseen) {
		t.Fatalf("a seen starved replica beside an unseen one: verdict %s, %v", f.Verdict, analyze.ReasonTexts(f.Reasons))
	}
}

// A workload rolled out under a day ago is short of history, but a run its
// node confirmed does not need the container's normal: it stays SATURATED.
func TestShortHistoryKeepsNodeConfirmedRun(t *testing.T) {
	w := metrics.CPUWait{Stretches: 36, Stalled: 0.03, LongestRun: 6, LongestConfirmedRun: 6,
		Normal: 0.05, HasNormal: true, Mean: 0.2, Coverage: 0.85, Busy: true, NodeKnown: 1}
	f := judgeWait(t, w)
	if f.Verdict != analyze.Saturated || hasCode(f, analyze.CPUWaitShortHistory) || !hasCode(f, analyze.CPUWaitNodeConfirmed) {
		t.Fatalf("node-confirmed starvation withdrawn as short history: verdict %s, %v", f.Verdict, analyze.ReasonTexts(f.Reasons))
	}
}

// Node contention found by the confirmed run alone quotes the run, not a
// share under its own threshold.
func TestNodeContentionByConfirmedRunQuotesTheRun(t *testing.T) {
	w := quietWait
	w.Stalled, w.LongestRun, w.LongestConfirmedRun, w.NodeKnown = 0.02, 6, 6, 1
	cs, obs := waitContainer(w)
	for _, o := range obs {
		o.CPUUsageMax, o.CPUSteadyMax = 0.1, 0.1
	}
	f := judge(t, cs, obs)
	for _, r := range f.Reasons {
		if r.Code == analyze.NodeContention {
			if r.Unit != analyze.Count || r.Observed < r.Against || strings.Contains(r.Text, "% of 5-minute stretches") {
				t.Fatalf("reason does not meet its own threshold: %+v", r)
			}
			return
		}
	}
	t.Fatalf("no NODE_CONTENTION: %v", analyze.ReasonTexts(f.Reasons))
}
