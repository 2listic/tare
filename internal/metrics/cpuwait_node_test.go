package metrics

import (
	"math"
	"strings"
	"testing"
	"time"
)

// Node states measured on the harness's 4-vCPU nodes (test/psi-vs-schedstat,
// k8s-experiments tare-waitcount): queue is run-queue wait per CPU, busy the
// busiest minute.
var (
	quietNode = NodeState{Known: true, EveryMinute: true, Queue: 0.16, BusyMax: 0.46} // workloads alone, nothing next to them
	idleNode  = NodeState{Known: true, EveryMinute: true, Queue: 0.05, BusyMax: 0.25} // the realistic apps at load x1
	halfNode  = NodeState{Known: true, EveryMinute: true, Queue: 0.14, BusyMax: 0.49} // the realistic apps' own load x2.5
	infraNode = NodeState{Known: true, EveryMinute: true, Queue: 0.18, BusyMax: 0.23} // the load generators' node
	mildNode  = NodeState{Known: true, EveryMinute: true, Queue: 1.15, BusyMax: 0.98} // two busy neighbours on 4 CPUs
	heavyNode = NodeState{Known: true, EveryMinute: true, Queue: 2.0, BusyMax: 0.999} // six busy neighbours
	// one neighbour throttled by its own limit on a half-idle node: its
	// throttled time is queue, its CPUs idle
	throttleNode = NodeState{Known: true, EveryMinute: true, Queue: 0.45, BusyMax: 0.54}
	burstNode    = NodeState{Known: true, EveryMinute: true, Queue: 0.2, BusyMax: 0.95} // a 30-second burst inside the stretch
	unknown      = NodeState{}
)

// ws is a stretch of a container using `usage` cores at `slowdown` s waited per
// s run, on node n.
func ws(usage, slowdown float64, n NodeState) WaitStretch {
	return WaitStretch{Wait: usage * slowdown, Run: usage, Usage: usage, Node: n}
}

// join concatenates runs of stretches.
func join(parts ...[]WaitStretch) []WaitStretch {
	var out []WaitStretch
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// withNode is the same stretches on another node state.
func withNode(st []WaitStretch, n NodeState) []WaitStretch {
	out := append([]WaitStretch(nil), st...)
	for i := range out {
		out[i].Node = n
	}
	return out
}

const day = 288 // 5-minute stretches

// Every situation measured so far, judged with its node's state. stalled is how
// many stretches the rule calls stalled, confirmed the longest run the node
// confirmed. without, where set, is the relative rule alone (node unknown) on
// the same stretches, for the cases the node changes.
func TestCPUWaitNodeContext(t *testing.T) {
	pinned := func(n NodeState) NodeState { n.Pinned = true; return n }
	// One CPU of four full of a neighbour and the container pinned to it: the
	// node is a quarter busy and its queue is that CPU's spread over four.
	pinnedFull := NodeState{Known: true, EveryMinute: true, Queue: 0.25, BusyMax: 0.3}
	pinnedQuiet := NodeState{Known: true, EveryMinute: true, Queue: 0.02, BusyMax: 0.1}

	for _, c := range []struct {
		name               string
		st                 []WaitStretch
		stalled, confirmed int
		without            int // -1: not checked
	}{
		// 1-3: a neighbour takes the node. Duty process, short-lived
		// processes, and a chatty thread next to starved workers: the node
		// confirms every contended stretch, whatever the container's shape.
		{"1 neighbour contention, duty process", join(repeat(ws(0.25, 0.06, quietNode), day-12), repeat(ws(0.25, 0.64, heavyNode), 12)), 12, 12, -1},
		{"2 neighbour contention, short-lived processes", join(repeat(ws(0.15, 0.26, quietNode), day-12), repeat(ws(0.12, 2.85, heavyNode), 12)), 12, 12, -1},
		{"3 neighbour contention, chatty thread beside starved workers", join(repeat(ws(0.37, 0.31, quietNode), day-12), repeat(ws(0.38, 1.03, heavyNode), 12)), 12, 12, -1},
		{"1 realistic apps during the batch job (nginx)", join(repeat(ws(0.3, 0.08, idleNode), day-12), repeat(ws(0.25, 4.2, heavyNode), 12)), 12, 12, -1},
		// 4: a node just full. The node confirms it; the container's
		// slowdown only doubled.
		{"4 mild neighbour contention", join(repeat(ws(0.25, 0.06, quietNode), day-12), repeat(ws(0.25, 0.12, mildNode), 12)), 12, 12, -1},
		// 5: the container's own peak. On a node with idle CPUs every
		// minute, a larger request would not remove the wait; on a full node
		// it is contention.
		{"5 own peak, node with idle CPUs", join(repeat(ws(0.3, 0.17, idleNode), day-12), repeat(ws(0.6, 0.6, NodeState{Known: true, EveryMinute: true, Queue: 0.1, BusyMax: 0.3}), 12)), 0, 0, 12},
		{"5 own peak, node full", join(repeat(ws(0.3, 0.17, idleNode), day-12), repeat(ws(0.6, 0.6, mildNode), 12)), 12, 12, -1},
		// 6-7: contended all the time, so the container's normal is the
		// contention. The node says so.
		{"6 always contended, heavy", repeat(ws(0.1, 0.95, heavyNode), day), day, day, 0},
		{"7 always contended, mild", repeat(ws(0.25, 0.3, mildNode), day), day, day, 0},
		// 8: 20 minutes in a day. A confirmed run, however short.
		{"8 short incident", join(repeat(ws(0.25, 0.06, quietNode), 140), repeat(ws(0.25, 0.64, heavyNode), 4), repeat(ws(0.25, 0.06, quietNode), 144)), 4, 4, -1},
		// A burst inside the stretch: the node's average queue is low, but
		// its busiest minute was full, so it is not vetoed and the relative
		// test decides.
		{"9 a 30-second burst inside the stretch", join(repeat(ws(0.25, 0.06, quietNode), day-12), repeat(ws(0.25, 0.3, burstNode), 12)), 12, 0, -1},
		// 10: PostgreSQL at its own load x2.5, the node half idle. PSI called
		// it starved; it got all the CPU its load asked for.
		{"10 own heavy load, half-idle node", join(repeat(ws(0.3, 0.17, idleNode), day-88), repeat(ws(0.6, 0.3, halfNode), 88)), 0, 0, -1},
		// 11: many short wake-ups on a quiet node: slowdown above the old
		// ceiling, and no shortage of CPU.
		{"11 chatty thread, quiet node", repeat(ws(0.05, 2.46, quietNode), day), 0, 0, day},
		{"11 k6 on its idle node", repeat(ws(0.105, 0.95, infraNode), day), 0, 0, -1},
		// 12: short-lived processes on a quiet node.
		{"12 short-lived processes, quiet node", repeat(ws(0.15, 0.26, quietNode), day), 0, 0, -1},
		// 13: eight threads read like two.
		{"13 eight threads under contention", join(repeat(ws(0.33, 0.23, quietNode), day-12), repeat(ws(0.34, 1.38, heavyNode), 12)), 12, 12, -1},
		{"13 two threads under contention", join(repeat(ws(0.31, 0.07, quietNode), day-12), repeat(ws(0.32, 0.73, heavyNode), 12)), 12, 12, -1},
		// Pinned to a CPU a neighbour fills, the rest of the node idle: not
		// vetoed, the relative test sees it.
		{"pinned to full CPUs, the rest idle", join(repeat(ws(0.3, 0.06, pinned(pinnedQuiet)), day-12), repeat(ws(0.3, 0.6, pinned(pinnedFull)), 12)), 12, 0, -1},
		// A container whose request protects it waits less than the floor on
		// a contended node.
		{"protected by its request on a contended node", join(repeat(ws(0.2, 0.01, quietNode), day-12), repeat(ws(0.2, 0.02, heavyNode), 12)), 0, 0, -1},
		// A neighbour throttled by its own limit fills the queue, not the
		// CPUs: no contention, and the relative test decides.
		{"throttled neighbour, half-idle node", join(repeat(ws(0.25, 0.06, throttleNode), day-12), repeat(ws(0.25, 0.09, throttleNode), 12)), 0, 0, -1},
		{"throttled neighbour, chatty", repeat(ws(0.05, 2.97, throttleNode), day), 0, 0, day},
		// The batch job on AlmaLinux 10: node-exporter went unscraped for the
		// 3 busiest minutes, and the 2 left read an idle node. Not every minute
		// was measured, so no veto, and the relative test sees the stall.
		{"batch job, node too busy to be scraped", join(repeat(ws(0.47, 0.08, idleNode), day-12),
			repeat(ws(0.47, 4.2, NodeState{Known: true, Queue: 0.07, BusyMax: 0.25}), 12)), 12, 0, -1},
		// Node unknown: the relative rule alone, ceiling included.
		{"node unknown, chatty", repeat(ws(0.05, 2.46, unknown), day), day, 0, -1},
	} {
		t.Run(c.name, func(t *testing.T) {
			w := SummariseCPUWait(c.st)
			if got := int(math.Round(w.Stalled * float64(len(c.st)))); got != c.stalled {
				t.Errorf("stalled %d of %d, want %d (%+v)", got, len(c.st), c.stalled, w)
			}
			if w.LongestConfirmedRun != c.confirmed {
				t.Errorf("confirmed run %d, want %d", w.LongestConfirmedRun, c.confirmed)
			}
			if c.without >= 0 {
				r := SummariseCPUWait(withNode(c.st, unknown))
				if got := int(math.Round(r.Stalled * float64(len(c.st)))); got != c.without {
					t.Errorf("relative rule alone: stalled %d, want %d", got, c.without)
				}
			}
		})
	}
}

// The veto needs a pin to be lifted: the same pinned container, not marked
// pinned, is judged by its node's average and missed. That is why the CPU
// manager's candidates are marked.
func TestCPUWaitPinnedNeedsTheMark(t *testing.T) {
	full := NodeState{Known: true, EveryMinute: true, Queue: 0.25, BusyMax: 0.3}
	st := join(repeat(ws(0.3, 0.06, NodeState{Known: true, EveryMinute: true, Queue: 0.02, BusyMax: 0.1}), day-12), repeat(ws(0.3, 0.6, full), 12))
	if w := SummariseCPUWait(st); w.Stalled != 0 {
		t.Fatalf("unmarked pinned container: stalled %v, want the veto to hide it (0)", w.Stalled)
	}
}

// The node shares: how much of the window was known, and contended.
func TestCPUWaitNodeShares(t *testing.T) {
	st := join(repeat(ws(0.25, 0.06, unknown), 100), repeat(ws(0.25, 0.06, quietNode), 138), repeat(ws(0.25, 0.64, heavyNode), 50))
	w := SummariseCPUWait(st)
	if math.Abs(w.NodeKnown-188.0/288) > 1e-9 || math.Abs(w.NodeContended-50.0/288) > 1e-9 {
		t.Errorf("known %v, contended %v", w.NodeKnown, w.NodeContended)
	}
}

// A run breaks where the node stops confirming, though it stays stalled.
func TestCPUWaitConfirmedRunBreaks(t *testing.T) {
	st := join(repeat(ws(0.25, 0.06, quietNode), 10), repeat(ws(0.25, 0.64, heavyNode), 3),
		repeat(ws(0.25, 0.64, burstNode), 2), repeat(ws(0.25, 0.64, heavyNode), 3))
	w := SummariseCPUWait(st)
	if w.LongestRun != 8 || w.LongestConfirmedRun != 3 {
		t.Errorf("runs %d / confirmed %d, want 8 / 3", w.LongestRun, w.LongestConfirmedRun)
	}
}

// nodeCluster fakes a Prometheus with tare-agent and node-exporter: container
// "app" on node 10.0.0.5, its stalled candidates, the stretches it waited at
// least the idle floor, and the node's queue and busiest minute per stretch.
type nodeCluster struct {
	agentCluster
	waited       []int
	queue, busy  []float64
	minutes      []float64 // nil: every minute of every stretch
	nodeInstance string
}

func (c nodeCluster) respond(expr string) (string, int) {
	switch {
	case expr == nodeQueueExpr:
		return nodeMatrix(c.nodeInstance, c.queue), 200
	case expr == nodeBusyExpr:
		return nodeMatrix(c.nodeInstance, c.busy), 200
	case expr == nodeMinutesExpr:
		m := c.minutes
		if m == nil {
			m = make([]float64, len(c.busy))
			for i := range m {
				m[i] = stretchMinutes
			}
		}
		return nodeMatrix(c.nodeInstance, m), 200
	case strings.HasPrefix(expr, "max by (namespace,pod,container) (sum by (namespace,pod,container)") && strings.Contains(expr, ">= 0.01)"):
		vals := make([]float64, len(c.waited))
		for i := range vals {
			vals[i] = 0.05
		}
		return matrix("app", vals, c.waited), 200
	}
	return c.agentCluster.respond(expr)
}

func nodeMatrix(instance string, values []float64) string {
	return strings.Replace(matrix("x", values, nil),
		`{"container":"x","namespace":"ns","pod":"p-0"}`, `{"instance":"`+instance+`"}`, 1)
}

// Through Prometheus: the chatty-like candidates at the ceiling on a quiet
// node are vetoed; the same container's stretches on a contended node are
// confirmed, and run.
func TestObserveCPUWaitNodeContext(t *testing.T) {
	steps := []int{0, 1, 2, 3, 4, 5, 6, 7}
	c := nodeCluster{
		agentCluster: agentCluster{agent: true, normal: 0.9, stretches: 8,
			candidates: []float64{1.2, 1.2, 1.2, 1.2, 1.5, 1.5, 1.5, 1.5}, steps: steps},
		waited:       steps,
		queue:        []float64{0.1, 0.1, 0.1, 0.1, 1.2, 1.2, 1.2, 1.2},
		busy:         []float64{0.3, 0.3, 0.3, 0.3, 0.99, 0.99, 0.99, 0.99},
		nodeInstance: "10.0.0.5:9100",
	}
	f := newFakeProm(c.respond)
	defer f.Close()
	key := Key{Namespace: "ns", Pod: "p-0", Container: "app"}

	p := NewProm(f.URL)
	p.SetPlacement(map[Key]Placement{key: {NodeIP: "10.0.0.5"}})
	obs, err := p.Observe([]string{"ns"}, 40*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	w := obs[key].CPUWait
	if math.Abs(w.Stalled-0.5) > 1e-9 || w.LongestConfirmedRun != 4 || w.NodeKnown != 1 || w.NodeContended != 0.5 {
		t.Errorf("want the 4 quiet stretches vetoed and the 4 contended confirmed, got %+v", w)
	}

	// Without a placement the same container is judged as before: every
	// candidate at the ceiling.
	obs, err = NewProm(f.URL).Observe([]string{"ns"}, 40*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if w := obs[key].CPUWait; w.Stalled != 1 || w.LongestConfirmedRun != 0 || w.NodeKnown != 0 {
		t.Errorf("no placement: want the relative rule alone, got %+v", w)
	}
}

// node-exporter answered in only some minutes of the quiet-looking
// stretches: they are not vetoed, and the relative test calls them stalled.
func TestObserveCPUWaitNodeScrapeGaps(t *testing.T) {
	steps := []int{0, 1, 2, 3}
	c := nodeCluster{
		agentCluster: agentCluster{agent: true, normal: 0.9, stretches: 4,
			candidates: []float64{2, 2, 2, 2}, steps: steps},
		waited:       steps,
		queue:        []float64{0.07, 0.07, 0.07, 0.07},
		busy:         []float64{0.25, 0.25, 0.25, 0.25},
		minutes:      []float64{5, 2, 2, 5},
		nodeInstance: "10.0.0.5:9100",
	}
	f := newFakeProm(c.respond)
	defer f.Close()
	key := Key{Namespace: "ns", Pod: "p-0", Container: "app"}
	p := NewProm(f.URL)
	p.SetPlacement(map[Key]Placement{key: {NodeIP: "10.0.0.5"}})
	obs, err := p.Observe([]string{"ns"}, 20*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if w := obs[key].CPUWait; math.Abs(w.Stalled-0.5) > 1e-9 {
		t.Errorf("want the 2 stretches with gaps judged by the relative rule (stalled 0.5), got %+v", w)
	}
}

// node-exporter missing: no node context, never an idle node.
func TestObserveCPUWaitNodeExporterMissing(t *testing.T) {
	c := nodeCluster{
		agentCluster: agentCluster{agent: true, normal: 0.9, stretches: 4,
			candidates: []float64{1.2, 1.2, 1.2, 1.2}},
		waited: []int{0, 1, 2, 3},
	}
	f := newFakeProm(func(expr string) (string, int) {
		if expr == nodeQueueExpr || expr == nodeBusyExpr {
			return `{"status":"success","data":{"resultType":"matrix","result":[]}}`, 200
		}
		return c.respond(expr)
	})
	defer f.Close()
	key := Key{Namespace: "ns", Pod: "p-0", Container: "app"}
	p := NewProm(f.URL)
	p.SetPlacement(map[Key]Placement{key: {NodeIP: "10.0.0.5"}})
	obs, err := p.Observe([]string{"ns"}, 20*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if w := obs[key].CPUWait; w.Stalled != 1 || w.NodeKnown != 0 {
		t.Errorf("want the relative rule alone, got %+v", w)
	}
}
