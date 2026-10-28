package analyze_test

import (
	"testing"
	"time"

	"github.com/2listic/tare/internal/analyze"
	"github.com/2listic/tare/internal/discover"
	"github.com/2listic/tare/internal/metrics"
)

// rescheduled is cpu-hog as the lab cluster held it on 2026-10-25: one live
// pod an hour old, and before it a pod of the same Deployment that ran for
// thirteen days and was evicted when its node went NotReady. Before pods were
// attributed through kube-state-metrics, the report was built from the hour
// and called it two weeks.
func rescheduled() ([]discover.Container, map[metrics.Key]*metrics.Observation) {
	const now = 1_800_000_000.0
	day := (24 * time.Hour).Seconds()
	c := discover.Container{
		Namespace: "tare-lab", Pod: "cpu-hog-5bc668f478-stkx8", Name: "hog",
		Workload: "Deployment/cpu-hog", WorkloadKind: "Deployment",
		CPURequest: 50, CPULimit: 100, MemRequest: 16 * mib, MemLimit: 32 * mib,
		QoS: "Burstable",
	}
	obs := func(pod string, from, to float64, cpuMax float64) *metrics.Observation {
		o := &metrics.Observation{
			Key:         metrics.Key{Namespace: "tare-lab", Pod: pod, Container: "hog"},
			CPUUsageP95: 0.02, CPUUsageMax: cpuMax, CPUSteadyMax: cpuMax,
			MemWorkingSet: 3 * mib, MemP95: 3 * mib,
			Samples:   int((to - from) / 15),
			FirstSeen: from, LastSeen: to,
			Owner: "Deployment/cpu-hog",
		}
		for _, s := range []metrics.Signal{metrics.SigCPUUsage, metrics.SigCPUPeak,
			metrics.SigMemory, metrics.SigMemP95, metrics.SigPSI} {
			o.Mark(s)
		}
		return o
	}
	live := obs(c.Pod, now-3600, now, 0.03)
	dead := obs("cpu-hog-5bc668f478-pdmnb", now-14*day, now-day, 0.09)
	// The dead pod was alive at the window's opening hour, so it carries an
	// early half and Observe left its trend marked.
	dead.MemEarly = 3 * mib
	dead.Mark(metrics.SigMemTrend)
	gone := obs("old-thing-7f9c-abcde", now-14*day, now-day, 4)
	gone.Owner = "Deployment/old-thing"
	return []discover.Container{c}, map[metrics.Key]*metrics.Observation{
		live.Key: live, dead.Key: dead, gone.Key: gone,
	}
}

// TestDeadPodsCountTowardTheirWorkload: the evicted pod's thirteen days are
// evidence about the same workload, and its higher peak must reach the
// recommendation, because a reschedule changes the pod's name and nothing else.
func TestDeadPodsCountTowardTheirWorkload(t *testing.T) {
	cs, obs := rescheduled()
	fs := analyze.Run(cs, obs, 14*24*time.Hour, analyze.DefaultThresholds())
	if len(fs) != 1 {
		t.Fatalf("want one finding, the deleted workload must not get a row; got %d", len(fs))
	}
	f := fs[0]
	if f.CPUUsageMax != 0.09 {
		t.Errorf("CPU peak %v: the dead pod's 0.09 must win, the live pod alone says 0.03", f.CPUUsageMax)
	}
	if want := int(14 * 24 * 3600 / 15 * 13 / 14); f.Samples < want-300 {
		t.Errorf("samples %d, want about %d: thirteen days of the same workload were dropped", f.Samples, want)
	}
	if f.Pods != 1 {
		t.Errorf("pods %d: a dead pod is history, not a replica", f.Pods)
	}
	if f.Confidence == analyze.Low {
		t.Errorf("confidence low on thirteen of fourteen days covered")
	}
	// A dead pod has no late hour. Its trend half must not mark the signal
	// present on a row whose pair came from nowhere.
	if f.Have[metrics.SigMemTrend] {
		t.Errorf("memory-trend marked measured from a dead pod: the live pod has no early half")
	}
}

// TestReplicasAreNotSummed: three pods side by side for a day are one day of
// evidence. Summing them would rate the row as three days' worth.
func TestReplicasAreNotSummed(t *testing.T) {
	const now = 1_800_000_000.0
	day := (24 * time.Hour).Seconds()
	var cs []discover.Container
	obs := map[metrics.Key]*metrics.Observation{}
	for _, pod := range []string{"web-6d9f-aaaaa", "web-6d9f-bbbbb", "web-6d9f-ccccc"} {
		cs = append(cs, discover.Container{
			Namespace: "ns", Pod: pod, Name: "app", Workload: "Deployment/web",
			CPURequest: 100, MemRequest: 64 * mib, QoS: "Burstable",
		})
		k := metrics.Key{Namespace: "ns", Pod: pod, Container: "app"}
		o := &metrics.Observation{Key: k, CPUUsageP95: 0.01, CPUUsageMax: 0.02,
			MemWorkingSet: 10 * mib, Samples: int(day / 15), FirstSeen: now - day, LastSeen: now,
			Owner: "Deployment/web"}
		o.Mark(metrics.SigMemory)
		obs[k] = o
	}
	f := analyze.Run(cs, obs, 7*24*time.Hour, analyze.DefaultThresholds())[0]
	if want := int(day / 15); f.Samples != want {
		t.Errorf("samples %d, want %d: concurrent replicas cover the same day once", f.Samples, want)
	}
}

// TestOutagesInsideASpanAreNotCoverage is node-agent on the lab cluster: a
// DaemonSet whose three pods each lived through a fortnight in which the host
// was switched off most nights. Each span is the whole fortnight; the samples
// are about four days. The union of the spans claimed fourteen days, summing
// the samples claimed three pods' worth, and the row came out high confidence.
func TestOutagesInsideASpanAreNotCoverage(t *testing.T) {
	const now = 1_800_000_000.0
	day := (24 * time.Hour).Seconds()
	var cs []discover.Container
	obs := map[metrics.Key]*metrics.Observation{}
	for _, pod := range []string{"agent-aaaaa", "agent-bbbbb", "agent-ccccc"} {
		cs = append(cs, discover.Container{
			Namespace: "ns", Pod: pod, Name: "agent", Workload: "DaemonSet/agent",
			CPURequest: 10, MemRequest: 16 * mib, QoS: "Burstable",
		})
		k := metrics.Key{Namespace: "ns", Pod: pod, Container: "agent"}
		o := &metrics.Observation{Key: k, MemWorkingSet: 4 * mib, Samples: int(4 * day / 15),
			FirstSeen: now - 14*day, LastSeen: now, Owner: "DaemonSet/agent"}
		o.Mark(metrics.SigMemory)
		obs[k] = o
	}
	f := analyze.Run(cs, obs, 14*24*time.Hour, analyze.DefaultThresholds())[0]
	if want := int(4 * day / 15); f.Samples != want {
		t.Errorf("samples %d, want %d: four days of samples are four days, whatever the spans say", f.Samples, want)
	}
}

// TestWithoutSpansTheBestPodStands: the simulator, or a failed timestamp
// query, gives no spans. Summing samples blind would overcount replicas, so
// coverage falls back to what tare reported before: the best single pod.
func TestWithoutSpansTheBestPodStands(t *testing.T) {
	cs, obs := rescheduled()
	for _, o := range obs {
		o.FirstSeen, o.LastSeen = 0, 0
	}
	f := analyze.Run(cs, obs, 14*24*time.Hour, analyze.DefaultThresholds())[0]
	if want := obs[metrics.Key{Namespace: "tare-lab", Pod: "cpu-hog-5bc668f478-pdmnb", Container: "hog"}].Samples; f.Samples != want {
		t.Errorf("samples %d, want the best single pod's %d", f.Samples, want)
	}
}
