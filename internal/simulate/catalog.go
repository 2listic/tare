package simulate

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/2listic/tare/internal/ceilings"
	"github.com/2listic/tare/internal/discover"
)

const mib = 1 << 20

// Archetypes are the workload shapes a right-sizer has to get right. Each one
// exists because it breaks a naive recommender in a specific way.
var (
	// SteadyWeb is the easy case: flat demand, no surprises. If the analyzer
	// cannot size this correctly, nothing else matters.
	SteadyWeb = Demand{
		Name: "steady-web", BaseCPU: 0.25, NoiseFraction: 0.10,
		BaseMem: 180 * mib,
	}

	// DailyWeb has a strong diurnal cycle. A percentile taken over a short
	// window lands somewhere arbitrary on the curve, which is why the
	// confidence rating has to depend on covering a whole period.
	DailyWeb = Demand{
		Name: "daily-web", BaseCPU: 0.40, DailyAmplitude: 0.60,
		WeekendFactor: 0.35, NoiseFraction: 0.08, BaseMem: 300 * mib,
	}

	// SpikyAPI is quiet most of the time and briefly very busy. Sizing to a
	// percentile starves it during spikes; sizing to the peak wastes most of
	// the allocation. This is where the cost-versus-risk tradeoff is real.
	SpikyAPI = Demand{
		Name: "spiky-api", BaseCPU: 0.10, NoiseFraction: 0.05,
		SpikeChance: 0.002, SpikeFactor: 8, SpikeTicks: 20,
		BaseMem: 120 * mib,
	}

	// LeakyService grows memory until it is killed. Any recommender that
	// trusts a memory percentile will chase the leak upward forever.
	LeakyService = Demand{
		Name: "leaky-service", BaseCPU: 0.15, NoiseFraction: 0.05,
		BaseMem: 200 * mib, MemGrowthPerHour: 40 * mib,
	}

	// CeilingJVM wants far less memory than its limit because its heap is
	// capped. Usage data alone suggests a reduction that would OOMKill it.
	// CeilingJVM has both a capped heap and an expensive boot: it draws 1.5
	// cores for 40 seconds loading classes and warming the JIT, then settles
	// at 0.2. A limit derived from steady state alone throttles every restart,
	// which is how a rollout turns into an outage.
	CeilingJVM = Demand{
		Name: "ceiling-jvm", BaseCPU: 0.20, NoiseFraction: 0.10,
		BaseMem: 220 * mib, MemCeiling: 256 * mib,
		StartupCPU: 1.5, StartupFor: 40 * time.Second,
		DeployEvery: 24 * time.Hour,
	}

	// IdleHoarder uses almost nothing and requests a great deal.
	IdleHoarder = Demand{
		Name: "idle-hoarder", BaseCPU: 0.005, NoiseFraction: 0.20,
		BaseMem: 12 * mib,
	}

	// HiddenCeilingJVM behaves identically to CeilingJVM but declares nothing
	// in its spec. It is the case no parser and no model can solve by reading
	// configuration, because the configuration is not there.
	HiddenCeilingJVM = Demand{
		Name: "hidden-ceiling-jvm", BaseCPU: 0.20, NoiseFraction: 0.10,
		BaseMem: 240 * mib, MemCeiling: 256 * mib, HideCeiling: true,
	}

	// VariableMemJVM runs on a JVM image but its memory genuinely fluctuates
	// with load rather than sitting at a cap. Inferring a ceiling here would
	// pin an ordinary application to today's usage forever, so it must not
	// fire. It is the negative control for plateau detection.
	VariableMemJVM = Demand{
		Name: "variable-mem-jvm", BaseCPU: 0.30, NoiseFraction: 0.10,
		BaseMem: 400 * mib, MemNoiseFraction: 0.35,
	}

	// The three below sit deliberately near a decision boundary. Without them a
	// threshold sweep reports every value as equally good, because nothing in
	// the catalog is close enough to the line for the line to matter.

	// MarginalPlateau has p95/max near 0.88, straddling PlateauRatio.
	MarginalPlateau = Demand{
		Name: "marginal-plateau-jvm", BaseCPU: 0.25, NoiseFraction: 0.10,
		BaseMem: 300 * mib, MemNoiseFraction: 0.08,
	}

	// MildStartup boots at only 1.4x its steady draw, straddling StartupRatio.
	MildStartup = Demand{
		Name: "mild-startup", BaseCPU: 0.50, NoiseFraction: 0.05,
		StartupCPU: 0.70, StartupFor: 60 * time.Second,
		BaseMem: 200 * mib, DeployEvery: 12 * time.Hour,
	}

	// SlowGrowth roughly doubles memory over a week: a real but gentle leak,
	// straddling LeakRatio.
	SlowGrowth = Demand{
		Name: "slow-growth", BaseCPU: 0.20, NoiseFraction: 0.05,
		BaseMem: 250 * mib, MemGrowthPerHour: 1500 * 1024,
	}

	// DormantSidecar does nothing at all: a log shipper in a namespace with no
	// traffic, a controller watching a CRD nobody creates. IdleHoarder is not
	// this case - at 5m baseline it still does measurable work, and it is the
	// one that must read as WASTE rather than IDLE. Sizing this one saves the
	// floor; deleting it saves the whole thing.
	DormantSidecar = Demand{
		Name: "dormant-sidecar", BaseCPU: 0.0004, NoiseFraction: 0.15,
		BaseMem: 20 * mib, MemNoiseFraction: 0.03,
	}

	// RareBatch is the negative control for idleness, and the reason the idle
	// test uses the peak rather than a percentile. It sits at almost nothing
	// for hours and then does real work: a nightly report, a queue consumer
	// woken by a rare message. Its p95 is indistinguishable from
	// DormantSidecar's and deleting it would break something, so any idle test
	// that reads a percentile classifies it wrong.
	RareBatch = Demand{
		Name: "rare-batch", BaseCPU: 0.0006, NoiseFraction: 0.15,
		SpikeChance: 0.0015, SpikeFactor: 900, SpikeTicks: 30,
		BaseMem: 60 * mib, MemNoiseFraction: 0.05,
	}

	// ChurningDeploy redeploys often, so its memory history is a sawtooth and
	// its baseline keeps resetting.
	ChurningDeploy = Demand{
		Name: "churning-deploy", BaseCPU: 0.30, NoiseFraction: 0.10,
		BaseMem: 150 * mib, MemGrowthPerHour: 10 * mib,
		DeployEvery: 6 * time.Hour,
	}
)

// Catalog is every archetype, for sweeps that must hold across all of them.
func Catalog() []Demand {
	return []Demand{
		SteadyWeb, DailyWeb, SpikyAPI, LeakyService,
		CeilingJVM, HiddenCeilingJVM, VariableMemJVM, IdleHoarder, ChurningDeploy,
		MarginalPlateau, MildStartup, SlowGrowth, DormantSidecar, RareBatch,
		NodeAgent, MetricsSidecar, NoLimitsAPI, GuaranteedCache,
	}
}

// The shapes below carry no new behaviour. They exist because the population a
// verdict has to survive is not the population of behaviours, it is the
// population of DECLARATIONS, and the live cluster proved the difference in one
// run: IDLE fired on five containers with no requests at all and told the
// operator to delete their metrics pipeline.

// NodeAgent is a node-exporter: a DaemonSet with no requests and no limits,
// doing a small amount of steady work forever. It is BestEffort, so there is
// nothing to be idle against and nothing to reclaim.
var NodeAgent = Demand{
	Name: "node-agent", Shape: NoRequests,
	BaseCPU: 0.012, NoiseFraction: 0.25,
	BaseMem: 34 * mib, MemNoiseFraction: 0.05,
}

// MetricsSidecar is the config-reloader shape: a second container in someone
// else's pod, no requests, almost no work. The two containers of this pod are
// declared differently, which no single-container fixture reaches.
var MetricsSidecar = Demand{
	Name: "metrics-app", Shape: Burstable,
	BaseCPU: 0.3, DailyAmplitude: 0.3, NoiseFraction: 0.1,
	BaseMem: 180 * mib, MemNoiseFraction: 0.04,
	Sidecar: &Demand{
		Name: "config-reloader", Shape: NoRequests, PodOf: "metrics-app",
		BaseCPU: 0.002, NoiseFraction: 0.4,
		BaseMem: 9 * mib, MemNoiseFraction: 0.08,
	},
}

// NoLimitsAPI sets requests and omits limits, which most style guides
// recommend and most charts do. cAdvisor exports CFS throttle counters only
// for containers with a CPU quota, so this shape has no throttle signal at all
// and leans entirely on PSI.
var NoLimitsAPI = Demand{
	Name: "no-limits-api", Shape: RequestsOnly,
	BaseCPU: 0.45, DailyAmplitude: 0.4, WeekendFactor: 0.5, NoiseFraction: 0.15,
	SpikeChance: 0.004, SpikeFactor: 3, SpikeTicks: 4,
	BaseMem: 260 * mib, MemNoiseFraction: 0.05,
}

// GuaranteedCache has request equal to limit for both resources. Kubernetes
// gives such pods the strongest eviction guarantee, and changing one field
// without the other silently demotes them to Burstable - a downgrade nobody
// asked for, delivered as a right-sizing win.
var GuaranteedCache = Demand{
	Name: "guaranteed-cache", Shape: Guaranteed,
	BaseCPU: 0.2, NoiseFraction: 0.08,
	BaseMem: 700 * mib, MemNoiseFraction: 0.02,
}

// Containers renders the cluster as the inventory discover would return, so
// analyze.Run can be called exactly as the CLI calls it.
//
// The spec carries a real JVM flag for any workload with a memory ceiling,
// which is what makes the ceiling rule testable end to end rather than only in
// the parser's own unit tests.
func (c *Cluster) Containers() []discover.Container {
	var out []discover.Container
	// Sorted: Envs is a map, and the inventory order reaches the report.
	for _, name := range slices.Sorted(maps.Keys(c.Envs)) {
		e := c.Envs[name]
		// A sidecar has its own environment so that it runs and is observed,
		// but it is rendered into the inventory by the workload whose pod it
		// shares. Rendering it here as well produced a second container in a
		// pod of its own, with no metrics behind it, reported NO-DATA.
		if e.Demand.PodOf != "" {
			continue
		}
		dc := c.render(name+"-0", name, "Deployment/"+name, e.Alloc, "Burstable", e.Demand.Shape)
		if strings.Contains(name, "jvm") {
			dc.Spec.Image = "eclipse-temurin:21-jre"
			dc.Image = dc.Spec.Image
		}
		if e.Demand.HideCeiling {
			// A JVM image with no flags at all: exactly what a chart looks
			// like when the heap is set in the entrypoint.
			dc.Spec.Image = "eclipse-temurin:21-jre"
			dc.Image = dc.Spec.Image
		}
		if hc := e.Demand.MemCeiling; hc > 0 && !e.Demand.HideCeiling {
			dc.Spec.Image = "eclipse-temurin:21-jre"
			dc.Spec.Env["JAVA_OPTS"] = fmt.Sprintf("-Xmx%dm", hc/mib)
		}
		out = append(out, dc)

		// A sidecar shares the pod, so it carries the same pod name. The pod is
		// the unit the scheduler places and the unit an observation is keyed
		// by, and a fixture that gave each container its own pod would not
		// reach any of the code that folds several containers into one.
		if sc := e.Demand.Sidecar; sc != nil {
			out = append(out, c.render(dc.Pod, sc.Name, dc.Workload, e.Alloc, dc.QoS, sc.Shape))
		}
	}
	return out
}

// render is one container as discover would report it, before any
// workload-specific spec is added.
func (c *Cluster) render(pod, name, workload string, a Allocation, qos string, shape Shape) discover.Container {
	dc := discover.Container{
		Namespace:  c.Namespace,
		Pod:        pod,
		Name:       name,
		Workload:   workload,
		Image:      "sim/" + name + ":1.0",
		CPURequest: int64(a.CPURequest * 1000),
		CPULimit:   int64(a.CPULimit * 1000),
		MemRequest: a.MemRequest,
		MemLimit:   a.MemLimit,
		QoS:        qos,
	}
	applyShape(&dc, shape)
	dc.Authored = authoredFor(shape)
	dc.Spec = ceilings.Container{
		Name:  name,
		Image: dc.Image,
		Env:   map[string]string{},
		// dc.MemLimit, not the allocation: a shape that drops the limit must
		// drop it here too, or a container declared without one still has a
		// JVM heap inferred from a limit its manifest does not set.
		MemoryLimitBytes: dc.MemLimit,
	}
	return dc
}

// authoredFor is what the controller's pod template set, which is not
// recoverable from the running pod: Kubernetes copies an omitted request from
// the limit, so by the time a pod exists the two are indistinguishable. The
// simulator left this unset, so every authored finding - including the one that
// says a pod is BestEffort - was unreachable from the catalogue and could only
// be tested against hand-built fixtures.
func authoredFor(s Shape) discover.Authored {
	switch s {
	case NoRequests:
		return discover.Authored{Known: true}
	case RequestsOnly:
		return discover.Authored{Known: true, CPURequest: true, MemRequest: true}
	default:
		return discover.Authored{Known: true, CPURequest: true, MemRequest: true, CPULimit: true, MemLimit: true}
	}
}

// applyShape rewrites the declared resources. Behaviour is untouched: a
// container with no requests still uses exactly what it used before, which is
// the whole point - the difference between the shapes is what the manifest
// says, not what the process does.
func applyShape(dc *discover.Container, s Shape) {
	switch s {
	case NoRequests:
		dc.CPURequest, dc.MemRequest = 0, 0
		dc.CPULimit, dc.MemLimit = 0, 0
		dc.QoS = "BestEffort"
	case RequestsOnly:
		dc.CPULimit, dc.MemLimit = 0, 0
		dc.QoS = "Burstable"
	case Guaranteed:
		dc.CPULimit, dc.MemLimit = dc.CPURequest, dc.MemRequest
		dc.QoS = "Guaranteed"
	}
}
