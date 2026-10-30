package analyze

import (
	"strings"
	"testing"

	"github.com/2listic/tare/internal/ceilings"
	"github.com/2listic/tare/internal/discover"
	"github.com/2listic/tare/internal/metrics"
)

func inv(nodes ...discover.Node) discover.Inventory {
	return discover.Inventory{Nodes: nodes}
}

func big(name string, cores int64) discover.Node {
	return discover.Node{
		Name: name, Cores: cores,
		AllocCPU: cores * 1000, AllocMem: cores * 4 << 30,
		Arch: "amd64", InstanceType: "m6i." + name, Schedulable: true,
	}
}

func reasons(f *Finding) string { return strings.Join(ReasonTexts(f.Reasons), " | ") }

// applyNodesNoUtil runs the node findings with no node-utilisation data, which
// is what every test written before that signal existed assumed.
func applyNodesNoUtil(f *Finding, i discover.Inventory) {
	applyNodes(f, i, nil, DefaultThresholds())
}

func TestPartialInventoryAddsNothing(t *testing.T) {
	// The failure this exists for: an inventory that could not be read is an
	// empty one, and every rule here would then compute a confident answer
	// from no data. Zero nodes must mean silence, never "no node is big
	// enough" or "0 nodes can be removed".
	f := &Finding{WorkloadKind: "DaemonSet", Pods: 40, RecCPU: 999999, CPULimit: 100}
	applyNodesNoUtil(f, discover.Inventory{Partial: true, Reason: "forbidden"})
	if len(f.Reasons) != 0 {
		t.Fatalf("a partial inventory must produce no findings, got: %s", reasons(f))
	}
	applyNodesNoUtil(f, discover.Inventory{})
	if len(f.Reasons) != 0 {
		t.Fatalf("an empty inventory must produce no findings, got: %s", reasons(f))
	}
}

func TestDaemonSetMultiplierIsNamed(t *testing.T) {
	f := &Finding{WorkloadKind: "DaemonSet", Pods: 40}
	applyNodesNoUtil(f, inv(big("a", 8)))
	if !strings.Contains(reasons(f), "40 replicas are 40 nodes") {
		t.Errorf("a DaemonSet must say its replica count is a node count: %s", reasons(f))
	}

	// A Deployment with the same replica count must not get the claim, which
	// would be false and would misdirect the reader about what scales.
	d := &Finding{WorkloadKind: "Deployment", Pods: 40}
	applyNodesNoUtil(d, inv(big("a", 8)))
	if strings.Contains(reasons(d), "per-node workload") {
		t.Errorf("a Deployment is not a per-node workload: %s", reasons(d))
	}
}

func TestMixedHardwareLowersConfidenceWithoutChangingNumbers(t *testing.T) {
	f := &Finding{
		Confidence: High, RecCPU: 500, RecMem: 1 << 30,
		NodeClasses: []string{"amd64/m6i.large", "arm64/m7g.large"},
	}
	applyNodesNoUtil(f, inv(big("a", 8)))
	if f.Confidence != Low {
		t.Errorf("confidence = %s, want low across mixed hardware", f.Confidence)
	}
	// The numbers must be untouched. A per-core performance ratio cannot be
	// measured from observation, so applying one would be invention.
	if f.RecCPU != 500 || f.RecMem != 1<<30 {
		t.Errorf("mixed hardware must not rescale the recommendation: cpu=%d mem=%d", f.RecCPU, f.RecMem)
	}
	if !strings.Contains(reasons(f), "does not normalise") {
		t.Errorf("the refusal to normalise must be stated: %s", reasons(f))
	}
}

func TestSingleNodeClassIsSilent(t *testing.T) {
	f := &Finding{Confidence: High, NodeClasses: []string{"amd64/m6i.large"}}
	applyNodesNoUtil(f, inv(big("a", 8)))
	if f.Confidence != High {
		t.Errorf("one node class is not a reason to doubt anything")
	}
}

func TestThreadPoolMismatchOnExplicitSetting(t *testing.T) {
	f := &Finding{
		CPULimit: 2000, Nodes: []string{"a"}, Image: "golang:1.23",
		Ceilings: []ceilings.Ceiling{{Kind: ceilings.GoMaxProcs, Resource: ceilings.CPU, Millicores: 96000}},
	}
	applyNodesNoUtil(f, inv(big("a", 96)))
	if !strings.Contains(reasons(f), "GOMAXPROCS is set to 96") {
		t.Errorf("an explicit setting that disagrees with the quota must be reported: %s", reasons(f))
	}
	if !strings.Contains(reasons(f), "configuration defect") {
		t.Errorf("the finding must say resizing is the wrong fix: %s", reasons(f))
	}
}

func TestThreadPoolInferredOnlyForRecognisedRuntimes(t *testing.T) {
	// A JVM image on a large node with a small quota: inferable.
	jvm := &Finding{CPULimit: 2000, Nodes: []string{"a"}, Image: "eclipse-temurin:21-jre"}
	applyNodesNoUtil(jvm, inv(big("a", 96)))
	if !strings.Contains(reasons(jvm), "no thread-pool setting was found") {
		t.Errorf("a managed runtime under a small quota on a big node must be flagged: %s", reasons(jvm))
	}

	// A scratch image could hold a Go binary or a shell script, and there is
	// no way to tell from here. Warning on every unrecognised image would put
	// a line on most of the cluster, so silence is correct - and is the reason
	// this check cannot be claimed to cover Go.
	unknown := &Finding{CPULimit: 2000, Nodes: []string{"a"}, Image: "gcr.io/distroless/static"}
	applyNodesNoUtil(unknown, inv(big("a", 96)))
	if strings.Contains(reasons(unknown), "thread-pool") {
		t.Errorf("an unrecognisable image must not be guessed at: %s", reasons(unknown))
	}
}

func TestThreadPoolQuietWhenQuotaMatchesNode(t *testing.T) {
	f := &Finding{CPULimit: 8000, Nodes: []string{"a"}, Image: "eclipse-temurin:21-jre"}
	applyNodesNoUtil(f, inv(big("a", 8)))
	if strings.Contains(reasons(f), "thread-pool") {
		t.Errorf("a quota matching the node is not a mismatch: %s", reasons(f))
	}
}

func TestThreadPoolNeedsAQuota(t *testing.T) {
	// No CPU limit means no quota and no period to exhaust: the runtime gets
	// the cores it asked for whenever the node has them spare.
	f := &Finding{CPULimit: 0, Nodes: []string{"a"}, Image: "eclipse-temurin:21-jre"}
	applyNodesNoUtil(f, inv(big("a", 96)))
	if strings.Contains(reasons(f), "thread-pool") {
		t.Errorf("an unlimited container cannot be throttled by its quota: %s", reasons(f))
	}
}

func TestRecommendationLargerThanAnyNodeIsRefused(t *testing.T) {
	f := &Finding{RecCPU: 32000, RecMem: 200 << 30}
	applyNodesNoUtil(f, inv(big("a", 8), big("b", 16)))
	r := reasons(f)
	if !strings.Contains(r, "exceeds the largest schedulable node") {
		t.Errorf("an unschedulable recommendation must be refused: %s", r)
	}
	if !strings.Contains(r, "Pending") {
		t.Errorf("the consequence must be named: %s", r)
	}
}

func TestCordonedNodesAreNotDestinations(t *testing.T) {
	// The only node big enough is cordoned. Counting it would promise a
	// placement the scheduler will never make.
	huge := big("huge", 64)
	huge.Schedulable = false
	f := &Finding{RecCPU: 32000}
	applyNodesNoUtil(f, inv(big("a", 8), huge))
	if !strings.Contains(reasons(f), "exceeds the largest schedulable node") {
		t.Errorf("a cordoned node must not count as capacity: %s", reasons(f))
	}
}

func TestFittingRecommendationIsSilent(t *testing.T) {
	f := &Finding{RecCPU: 2000, RecMem: 4 << 30}
	applyNodesNoUtil(f, inv(big("a", 16)))
	if len(f.Reasons) != 0 {
		t.Errorf("a recommendation that fits needs no comment: %s", reasons(f))
	}
}

// --- throttled while the node was idle ------------------------------------
//
// The claim is that a container's own CPU quota, not the machine, was the
// constraint. It is only safe when the machine demonstrably had room, so the
// negative controls matter more than the positive case here.

func util(p95, mean float64) metrics.NodeObservation {
	return metrics.NodeObservation{CPUUtilP95: p95, CPUUtilMean: mean, Samples: 500}
}

func throttledFinding() *Finding {
	return &Finding{
		Workload: "Deployment/hog", Container: "hog", Pods: 1,
		CPULimit: 100, ThrottlePeak: 0.998, Nodes: []string{"n1"},
	}
}

func TestThrottledOnIdleNodeIsReported(t *testing.T) {
	f := throttledFinding()
	applyNodes(f, inv(big("n1", 16)), map[string]metrics.NodeObservation{"n1": util(0.078, 0.056)}, DefaultThresholds())
	r := reasons(f)
	if !strings.Contains(r, "99.8% of scheduling periods") {
		t.Errorf("the throttle ratio must be quoted: %s", r)
	}
	if !strings.Contains(r, "7.8%") || !strings.Contains(r, "5.6%") {
		t.Errorf("both node figures must be quoted: %s", r)
	}
	if !strings.Contains(r, "own limit, not the machine") {
		t.Errorf("the conclusion must be stated: %s", r)
	}
	// Acting on this is not free, and the report holds the number that says so.
	if !strings.Contains(r, "commitment table") {
		t.Errorf("the cost of removing a limit must be named: %s", r)
	}
	if !f.NodeUtilKnown || f.NodeUtilP95 != 0.078 {
		t.Errorf("the node figure must be recorded on the finding: known=%v p95=%v", f.NodeUtilKnown, f.NodeUtilP95)
	}
}

// The primary negative control. A container throttled on a genuinely busy node
// may still be suffering from its quota, but "there was capacity going spare"
// is not a claim this evidence supports.
func TestThrottledOnBusyNodeIsNotReported(t *testing.T) {
	f := throttledFinding()
	applyNodes(f, inv(big("n1", 16)), map[string]metrics.NodeObservation{"n1": util(0.91, 0.80)}, DefaultThresholds())
	if strings.Contains(reasons(f), "not the machine") {
		t.Errorf("a busy node must not be described as having room: %s", reasons(f))
	}
}

// A workload spread across an idle node and a saturated one has no safe claim.
// The busiest node governs.
func TestMixedIdleAndBusyNodesWithholdsTheClaim(t *testing.T) {
	f := throttledFinding()
	f.Nodes = []string{"n1", "n2"}
	applyNodes(f, inv(big("n1", 16), big("n2", 16)), map[string]metrics.NodeObservation{
		"n1": util(0.05, 0.03),
		"n2": util(0.88, 0.71),
	}, DefaultThresholds())
	if strings.Contains(reasons(f), "not the machine") {
		t.Errorf("one busy node is enough to withhold the claim: %s", reasons(f))
	}
}

func TestUnthrottledContainerIsNotReported(t *testing.T) {
	f := throttledFinding()
	f.ThrottlePeak = 0
	applyNodes(f, inv(big("n1", 16)), map[string]metrics.NodeObservation{"n1": util(0.05, 0.03)}, DefaultThresholds())
	if strings.Contains(reasons(f), "scheduling periods") {
		t.Errorf("an unthrottled container has nothing to report: %s", reasons(f))
	}
}

func TestNoCPULimitMeansNoQuotaToBeBoundBy(t *testing.T) {
	f := throttledFinding()
	f.CPULimit = 0
	applyNodes(f, inv(big("n1", 16)), map[string]metrics.NodeObservation{"n1": util(0.05, 0.03)}, DefaultThresholds())
	if len(f.Reasons) != 0 {
		t.Errorf("without a limit there is no quota to be bound by: %s", reasons(f))
	}
}

// Silence would read as "the node was fine". An unmeasured node and an idle one
// look identical in a float, so the missing input has to be named.
func TestUnknownNodeUtilisationIsNamedNotAssumed(t *testing.T) {
	f := throttledFinding()
	applyNodes(f, inv(big("n1", 16)), nil, DefaultThresholds())
	r := reasons(f)
	if !strings.Contains(r, "is unknown") || !strings.Contains(r, "node_cpu_seconds_total") {
		t.Fatalf("the missing signal must be named: %s", r)
	}
	if strings.Contains(r, "not the machine") {
		t.Errorf("no verdict may be reached without node data: %s", r)
	}
	if f.NodeUtilKnown {
		t.Error("NodeUtilKnown must be false when nothing was measured")
	}
}

// A node present in the map but with no samples is not a measurement.
func TestZeroSamplesIsNotAMeasurement(t *testing.T) {
	f := throttledFinding()
	applyNodes(f, inv(big("n1", 16)), map[string]metrics.NodeObservation{
		"n1": {CPUUtilP95: 0, CPUUtilMean: 0, Samples: 0},
	}, DefaultThresholds())
	if !strings.Contains(reasons(f), "is unknown") {
		t.Errorf("zero samples must read as unmeasured, not as an idle node: %s", reasons(f))
	}
}

// Without CFS counters, pressure under a limit on a node idle on average is
// reported, but blames neither the limit nor the node: pressure says the tasks
// waited, not why, and a node idle on average can be full for the minutes that
// made it (KNOWN-FAILURES 56).
func TestPSIAloneCannotTellLimitFromNode(t *testing.T) {
	f := throttledFinding()
	f.ThrottlePeak = 0
	f.PSIWaitingPeak = 0.9
	f.Have = map[metrics.Signal]bool{metrics.SigPSI: true}
	applyNodes(f, inv(big("n1", 16)), map[string]metrics.NodeObservation{"n1": util(0.05, 0.03)}, DefaultThresholds())
	r := reasons(f)
	if !strings.Contains(r, "CPU pressure peaked") || !hasReason(f, ThrottleUnknown) {
		t.Fatalf("pressure alone must still be reported, as %s: %s", ThrottleUnknown, r)
	}
	if hasReason(f, LimitNotNode) {
		t.Errorf("unmeasured throttling must not be read as the limit: %s", r)
	}
	if strings.Contains(r, "scheduling periods") {
		t.Errorf("without CFS data the message must not quote a throttle ratio: %s", r)
	}
	if !strings.Contains(r, "container_cpu_cfs_throttled_periods_total") {
		t.Errorf("the missing metric must be named: %s", r)
	}
}

func hasReason(f *Finding, c Code) bool {
	for _, r := range f.Reasons {
		if r.Code == c {
			return true
		}
	}
	return false
}

// The one thing this feature must never do. tare has no latency signal until
// v0.3, and a p99 figure here would be invented rather than measured.
func TestFindingMakesNoLatencyClaim(t *testing.T) {
	f := throttledFinding()
	applyNodes(f, inv(big("n1", 16)), map[string]metrics.NodeObservation{"n1": util(0.078, 0.056)}, DefaultThresholds())
	r := strings.ToLower(reasons(f))
	for _, banned := range []string{"p99", "latency", "ms of", "millisecond"} {
		if strings.Contains(r, banned) {
			t.Errorf("no latency claim is measurable here, found %q in: %s", banned, r)
		}
	}
}

// A zero NodeIdleCeiling makes `peak >= ceiling` true for every node, which
// withholds the finding on an idle cluster and looks exactly like "no idle
// nodes were found". That is how this threshold shipped broken the first time,
// and it cost two failing tests to notice rather than a production report.
func TestNodeIdleCeilingHasANonZeroDefault(t *testing.T) {
	if c := DefaultThresholds().NodeIdleCeiling; c <= 0 || c >= 1 {
		t.Fatalf("NodeIdleCeiling = %v, want a fraction between 0 and 1: a zero silently disables the finding", c)
	}
}

// A node can average 20% and still have been saturated for the hours that
// mattered. tare does not correlate throttling with the node's busy periods in
// time, so a high p95 withholds the claim even when the mean looks idle.
func TestLowMeanWithSustainedBusyPeriodsWithholdsTheClaim(t *testing.T) {
	f := throttledFinding()
	applyNodes(f, inv(big("n1", 16)), map[string]metrics.NodeObservation{"n1": util(0.97, 0.20)}, DefaultThresholds())
	if strings.Contains(reasons(f), "not the machine") {
		t.Errorf("a node that saturated during the window has no proven spare capacity: %s", reasons(f))
	}
}

// The defect that only a live run found. The second gate was a MAXIMUM, and a
// maximum over a two-week window is pinned by any single spike. Measured on the
// lab cluster: a seven-minute load test put the four-day max at 99.9% while the
// p95 stayed at 7.7% and the mean at 5.9%. The finding silently stopped
// reporting on a cluster that was idle 99.9% of the time, which is why the gate
// is a percentile.
func TestBriefSpikeDoesNotDisqualifyAnIdleNode(t *testing.T) {
	f := throttledFinding()
	// p95 7.7%, mean 5.9%: the real four-day figures from the lab cluster
	// during the window that contained the load test.
	applyNodes(f, inv(big("n1", 16)), map[string]metrics.NodeObservation{"n1": util(0.077, 0.059)}, DefaultThresholds())
	if !strings.Contains(reasons(f), "not the machine") {
		t.Errorf("a node idle for 99.9%% of the window had room: %s", reasons(f))
	}
}

func TestNodeBusyP95HasANonZeroDefault(t *testing.T) {
	if c := DefaultThresholds().NodeBusyP95; c <= 0 || c > 1 {
		t.Fatalf("NodeBusyP95 = %v, want a fraction: a zero silently disables the finding", c)
	}
}
