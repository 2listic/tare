package simulate

import (
	"math"
	"math/rand"
	"sort"
	"time"

	"github.com/mfranzon/tare/internal/metrics"
)

// Tick is the simulation resolution. It matches a typical Prometheus scrape
// interval so that sample counts, and therefore confidence ratings, mean the
// same thing in simulation as in production.
const Tick = 15 * time.Second

// Allocation is what Kubernetes gives the container. Zero means unset.
type Allocation struct {
	CPURequest float64 // cores
	CPULimit   float64 // cores
	MemRequest int64   // bytes
	MemLimit   int64   // bytes
}

// Sample is one tick of simulated container behaviour.
type Sample struct {
	T             time.Time
	CPUDemand     float64 // what it wanted
	CPUUsage      float64 // what it got
	PSIWaiting    float64 // seconds of stall per second
	MemDemand     int64
	MemWorkingSet int64
	OOMKilled     bool
	Restarts      float64
	// Startup marks a tick inside the container's boot phase.
	Startup bool
}

// Env is a steppable simulation of one container. The Reset/Step shape is
// deliberate: it is the interface a control loop drives, and the same shape a
// reinforcement learning environment would expose if one is ever wanted.
type Env struct {
	Demand Demand
	Alloc  Allocation

	now       time.Time
	lastStart time.Time // container start, for memory growth and deploys
	restarts  float64
	spikeLeft int
	rng       *rand.Rand
	seed      int64
	samples   []Sample
}

// New creates an environment starting at from. The seed makes every run
// reproducible, which matters because a flaky simulator is worse than none.
func New(d Demand, alloc Allocation, from time.Time, seed int64) *Env {
	e := &Env{Demand: d, Alloc: alloc, seed: seed}
	e.Reset(from)
	return e
}

// Reset returns the environment to its initial state at time from, keeping the
// current allocation.
func (e *Env) Reset(from time.Time) {
	e.now = from
	e.lastStart = from
	e.restarts = 0
	e.spikeLeft = 0
	e.rng = rand.New(rand.NewSource(e.seed))
	e.samples = e.samples[:0]
}

// SetAllocation changes the container's resources. A control loop calls this
// between steps; it does not reset the clock or the sample history, because a
// resize in the real world does not erase what came before.
func (e *Env) SetAllocation(a Allocation) { e.Alloc = a }

// Step advances one tick and returns the resulting sample.
func (e *Env) Step() Sample {
	d := e.Demand

	// Deploys restart the container, which resets memory growth. A right-sizer
	// must notice this and re-baseline rather than keep descending.
	if d.DeployEvery > 0 && e.now.Sub(e.lastStart) >= d.DeployEvery {
		e.lastStart = e.now
		e.restarts++
	}

	if e.spikeLeft > 0 {
		e.spikeLeft--
	} else if d.SpikeChance > 0 && e.rng.Float64() < d.SpikeChance {
		e.spikeLeft = d.SpikeTicks
	}

	booting := d.StartupFor > 0 && e.now.Sub(e.lastStart) < d.StartupFor
	cpuWant := d.cpuDemand(e.now, e.spikeLeft > 0, e.rng)
	if booting {
		cpuWant = d.StartupCPU
	}
	cpuGot, psi := serve(cpuWant, e.Alloc.CPULimit)

	memWant := d.memDemand(e.now.Sub(e.lastStart), e.rng)
	memGot := memWant
	oom := false
	if e.Alloc.MemLimit > 0 && memWant > e.Alloc.MemLimit {
		// The kernel kills the container: memory has no graceful degradation.
		oom = true
		e.restarts++
		e.lastStart = e.now
		memGot = d.memDemand(0, nil)
	}

	s := Sample{
		T: e.now, CPUDemand: cpuWant, CPUUsage: cpuGot, PSIWaiting: psi,
		MemDemand: memWant, MemWorkingSet: memGot, OOMKilled: oom,
		Restarts: e.restarts, Startup: booting,
	}
	e.samples = append(e.samples, s)
	e.now = e.now.Add(Tick)
	return s
}

// Run advances for a duration and returns the samples produced.
func (e *Env) Run(d time.Duration) []Sample {
	n := int(d / Tick)
	first := len(e.samples)
	for i := 0; i < n; i++ {
		e.Step()
	}
	return e.samples[first:]
}

// serve models CPU contention. Below the quota a container still queues as it
// approaches saturation; above it, the stalled fraction is exactly the share
// of wanted work that cannot run, which is what PSI measures.
func serve(want, limit float64) (got, psi float64) {
	if limit <= 0 {
		return want, 0
	}
	if want <= limit {
		u := want / limit
		// Queueing becomes measurable only in the last 20% of headroom.
		return want, 0.02 * math.Max(0, (u-0.8)/0.2)
	}
	return limit, (want - limit) / want
}

// Cluster is a set of simulated containers with the Observe and Usage methods
// of a metrics source, so the real analyzer can be run against simulation with
// no code path of its own.
type Cluster struct {
	Namespace string
	Envs      map[string]*Env // container name to environment
}

// NewCluster builds a cluster from demands, all sharing a start time and a
// seed offset so runs stay reproducible.
func NewCluster(namespace string, from time.Time, seed int64, demands ...Demand) *Cluster {
	c := &Cluster{Namespace: namespace, Envs: map[string]*Env{}}
	for i, d := range demands {
		c.Envs[d.Name] = New(d, Allocation{}, from, seed+int64(i))
		// A sidecar is a container that runs, not a label on its neighbour.
		// Registered here it gets its own environment, its own series and its
		// own finding; without this it rendered into the inventory with no
		// metrics behind it and came back NO-DATA.
		if sc := d.Sidecar; sc != nil {
			c.Envs[sc.Name] = New(*sc, Allocation{}, from, seed+int64(i)+1000)
		}
	}
	return c
}

// SetAllocation assigns resources to one container.
func (c *Cluster) SetAllocation(name string, a Allocation) {
	if e, ok := c.Envs[name]; ok {
		e.SetAllocation(a)
	}
}

// Run advances every container by the same duration.
func (c *Cluster) Run(d time.Duration) {
	for _, e := range c.Envs {
		e.Run(d)
	}
}

// Observe aggregates the trailing window into the same Observation values the
// Prometheus source produces.
func (c *Cluster) Observe(namespaces []string, window time.Duration) (map[metrics.Key]*metrics.Observation, error) {
	out := map[metrics.Key]*metrics.Observation{}
	for name, e := range c.Envs {
		// A sidecar shares its app's pod, and an observation is keyed by pod:
		// keying it by its own name would put it in a pod the inventory does
		// not contain, and the join in Run would silently drop it.
		pod := name + "-0"
		if e.Demand.PodOf != "" {
			pod = e.Demand.PodOf + "-0"
		}
		k := metrics.Key{Namespace: c.Namespace, Pod: pod, Container: name}
		out[k] = e.observe(k, window)
	}
	return out, nil
}

func (e *Env) observe(k metrics.Key, window time.Duration) *metrics.Observation {
	cutoff := e.now.Add(-window)
	earlyEnd := cutoff.Add(time.Hour)
	lateStart := e.now.Add(-time.Hour)
	o := &metrics.Observation{Key: k}
	// The simulator can produce every signal, which is exactly why tests that
	// exercise missing ones have to remove them deliberately.
	for _, sig := range []metrics.Signal{
		metrics.SigCPUUsage, metrics.SigCPUPeak, metrics.SigMemory,
		metrics.SigMemP95, metrics.SigPSI, metrics.SigPSIAvg, metrics.SigOOM,
		metrics.SigRestarts, metrics.SigStartup, metrics.SigCPUWait,
	} {
		o.Mark(sig)
	}
	if window >= 4*time.Hour {
		o.Mark(metrics.SigMemTrend)
	}
	if e.Alloc.MemLimit > 0 {
		o.Mark(metrics.SigLimitTrend)
	}

	var early, late []float64
	var cpu, mem []float64
	// Sustained pressure, the mean over the window, alongside the peak. The
	// two diverge exactly where it matters: a workload that spikes briefly has
	// a high peak and a negligible mean.
	var psiSum float64
	var psiN int
	// CPU wait, as tare-agent would see it, in 5-minute stretches. The model
	// has one task, so wait is the stalled share of the work it wanted: the
	// demand the quota refused above it, the queueing below it. There is no
	// background wait from neighbours, so a container that never approaches
	// its quota has no normal; see TestCPUWaitMatchesPSIOnCatalog.
	var stretches []metrics.WaitStretch
	var cur metrics.WaitStretch
	var curN int
	const perStretch = int(5 * time.Minute / Tick)
	for _, s := range e.samples {
		if s.T.Before(cutoff) {
			continue
		}
		cur.Wait += s.PSIWaiting * s.CPUDemand
		cur.Usage += s.CPUUsage
		if curN++; curN == perStretch {
			cur.Wait, cur.Usage = cur.Wait/float64(curN), cur.Usage/float64(curN)
			cur.Run = cur.Usage
			stretches = append(stretches, cur)
			cur, curN = metrics.WaitStretch{}, 0
		}
		cpu = append(cpu, s.CPUUsage)
		o.CPUUsageMax = math.Max(o.CPUUsageMax, s.CPUUsage)
		if s.Startup {
			o.CPUStartupMax = math.Max(o.CPUStartupMax, s.CPUUsage)
		} else {
			o.CPUSteadyMax = math.Max(o.CPUSteadyMax, s.CPUUsage)
		}
		o.PSIWaitingMax = math.Max(o.PSIWaitingMax, s.PSIWaiting)
		psiSum += s.PSIWaiting
		psiN++
		mem = append(mem, float64(s.MemWorkingSet))
		if float64(s.MemWorkingSet) > o.MemWorkingSet {
			o.MemWorkingSet = float64(s.MemWorkingSet)
		}
		if s.OOMKilled {
			o.OOMEvents++
		}
		o.Restarts = s.Restarts
		o.Samples++

		switch {
		case s.T.Before(earlyEnd):
			early = append(early, float64(s.MemWorkingSet))
		case !s.T.Before(lateStart):
			late = append(late, float64(s.MemWorkingSet))
		}
	}
	o.CPUUsageP95 = quantile(cpu, 0.95)
	o.MemP95 = quantile(mem, 0.95)
	if psiN > 0 {
		o.PSIWaitingAvg = psiSum / float64(psiN)
	}
	o.CPUWait = metrics.SummariseCPUWait(stretches)
	o.MemEarly, o.MemLate = mean(early), mean(late)
	o.MemLimitEarly, o.MemLimitLate = float64(e.Alloc.MemLimit), float64(e.Alloc.MemLimit)
	return o
}

func quantile(xs []float64, q float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	i := int(q * float64(len(s)-1))
	return s[i]
}

func mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	var sum float64
	for _, x := range xs {
		sum += x
	}
	return sum / float64(len(xs))
}

// Usage is the trailing window of one container as the same bucketed peaks
// metrics.Prom.Usage returns, so a demo can draw what a cluster would.
func (c *Cluster) Usage(container string, window time.Duration, buckets int) (cpu, mem metrics.Series, ok bool) {
	e, ok := c.Envs[container]
	if !ok {
		return cpu, mem, false
	}
	step := window / time.Duration(buckets)
	start := e.now.Add(-window)
	cpu = metrics.Series{Start: start, Step: step, Values: make([]float64, buckets)}
	mem = metrics.Series{Start: start, Step: step, Values: make([]float64, buckets)}
	for i := range cpu.Values {
		cpu.Values[i], mem.Values[i] = math.NaN(), math.NaN()
	}
	for _, s := range e.samples {
		i := int(s.T.Sub(start) / step)
		if i < 0 || i >= buckets {
			continue
		}
		if !(cpu.Values[i] >= s.CPUUsage) {
			cpu.Values[i] = s.CPUUsage
		}
		if v := float64(s.MemWorkingSet); !(mem.Values[i] >= v) {
			mem.Values[i] = v
		}
	}
	return cpu, mem, true
}
