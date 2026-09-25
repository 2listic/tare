// Package simulate models synthetic workloads whose correct size is known in
// advance, so the analyzer and the descent loop can be tested without a
// cluster and without waiting for real observation windows.
//
// The design rule: the simulator produces the same types the real pipeline
// consumes (metrics.Observation and discover.Container), so tests exercise the
// production code path rather than a parallel one built for testing.
package simulate

import (
	"math"
	"math/rand"
	"time"
)

// Demand describes what a workload wants, independent of what it is given.
// Everything the simulator does derives from this: usage, pressure, OOMKills
// and the ground truth a test asserts against.
type Demand struct {
	Name string

	// BaseCPU is baseline demand in cores.
	BaseCPU float64
	// DailyAmplitude scales a sinusoid peaking in the early afternoon, as a
	// fraction of BaseCPU. 0.5 means demand swings plus or minus 50%.
	DailyAmplitude float64
	// WeekendFactor multiplies demand on Saturday and Sunday. 1 means no
	// weekly cycle, 0.3 is a typical business application.
	WeekendFactor float64
	// SpikeChance is the per-tick probability of starting a traffic spike.
	SpikeChance float64
	// SpikeFactor multiplies demand during a spike.
	SpikeFactor float64
	// SpikeTicks is how long a spike lasts.
	SpikeTicks int
	// NoiseFraction is the standard deviation of multiplicative noise.
	NoiseFraction float64

	// BaseMem is baseline memory demand in bytes.
	BaseMem int64
	// MemGrowthPerHour models a leak, in bytes. Reset on restart.
	MemGrowthPerHour int64
	// MemNoiseFraction is multiplicative noise on memory demand. Without it
	// every simulated workload plateaus perfectly, which makes plateau
	// detection untestable: it would appear to work on workloads that merely
	// have no variance rather than on ones with a real ceiling.
	MemNoiseFraction float64

	// MemCeiling caps memory demand however much is allocated, modelling a
	// runtime heap ceiling such as -Xmx. Zero means uncapped.
	MemCeiling int64

	// CPUCeiling caps usable CPU however much is allocated, modelling
	// GOMAXPROCS or a fixed worker count. Zero means uncapped.
	CPUCeiling float64

	// HideCeiling omits the heap flag from the rendered container spec while
	// keeping the behaviour. It models the common real case where the cap
	// lives in the image entrypoint or a ConfigMap, which the corpus puts at
	// roughly a quarter of JVM workloads. Nothing that reads the spec can find
	// it; only behaviour reveals it.
	HideCeiling bool

	// StartupCPU is demand in cores during boot: class loading, JIT warmup,
	// cache population. Zero means no distinct startup phase.
	StartupCPU float64
	// StartupFor is how long the boot phase lasts.
	StartupFor time.Duration

	// DeployEvery triggers a rollout, restarting the container and resetting
	// memory. Zero means never.
	DeployEvery time.Duration

	// Shape is how the container is DECLARED, as opposed to how it behaves.
	//
	// Every simulated container used to be rendered Burstable with both
	// requests and limits set, because that is what the allocation model
	// expresses. Real clusters are not like that, and the gap is not cosmetic:
	// the IDLE verdict shipped telling operators to delete node-exporter,
	// kube-state-metrics and two Grafana sidecars, because a container with no
	// requests at all has nothing to be idle *against* and every simulated
	// container had requests by construction. The live cluster found it in one
	// run; the simulator and 88 snapshots of history could not see it.
	//
	// So the declaration is now its own axis. Behaviour stays in the fields
	// above and can be combined with any shape.
	Shape Shape

	// PodOf names the workload whose pod this container shares, set on a
	// sidecar so its metrics are keyed by the pod that actually exists.
	PodOf string

	// Sidecar names a second container sharing this pod. Infra sidecars are
	// most of the population that has no requests, and a pod with two
	// containers of different shapes is a case no single-container fixture
	// reaches.
	Sidecar *Demand
}

// Shape is how a container's resources are declared in its manifest.
type Shape int

const (
	// Burstable is requests below limits: the ordinary case, and what every
	// simulated container was before this existed.
	Burstable Shape = iota
	// NoRequests declares nothing at all. The pod is BestEffort: the scheduler
	// treats it as free, it is first to be evicted, and it can be OOMKilled
	// while the node still has memory it was never promised. This is the shape
	// most infrastructure DaemonSets ship with.
	NoRequests
	// RequestsOnly sets requests and omits limits, which is the shape most
	// style guides recommend and most charts actually use. It matters here
	// because cAdvisor exports CFS throttle counters only for containers with
	// a CPU quota, so this shape has no throttle signal at all.
	RequestsOnly
	// Guaranteed sets request equal to limit for both resources. Kubernetes
	// gives these pods the strongest eviction guarantee, and changing one
	// field without the other silently demotes them.
	Guaranteed
)

// String names the shape for test failure messages.
func (s Shape) String() string {
	switch s {
	case NoRequests:
		return "NoRequests"
	case RequestsOnly:
		return "RequestsOnly"
	case Guaranteed:
		return "Guaranteed"
	default:
		return "Burstable"
	}
}

// cpuDemand returns wanted cores at time t.
func (d Demand) cpuDemand(t time.Time, spiking bool, rng *rand.Rand) float64 {
	v := d.BaseCPU

	if d.DailyAmplitude != 0 {
		// Peak at 14:00, trough at 02:00.
		hours := float64(t.Hour()) + float64(t.Minute())/60
		v *= 1 + d.DailyAmplitude*math.Sin(2*math.Pi*(hours-8)/24)
	}
	if d.WeekendFactor != 0 {
		if wd := t.Weekday(); wd == time.Saturday || wd == time.Sunday {
			v *= d.WeekendFactor
		}
	}
	if spiking {
		v *= d.SpikeFactor
	}
	if d.NoiseFraction > 0 {
		v *= 1 + rng.NormFloat64()*d.NoiseFraction
	}
	if v < 0 {
		v = 0
	}
	if d.CPUCeiling > 0 && v > d.CPUCeiling {
		v = d.CPUCeiling
	}
	return v
}

// memDemand returns wanted bytes given how long the container has been up.
func (d Demand) memDemand(uptime time.Duration, rng *rand.Rand) int64 {
	v := d.BaseMem + int64(float64(d.MemGrowthPerHour)*uptime.Hours())
	if d.MemNoiseFraction > 0 && rng != nil {
		v = int64(float64(v) * (1 + rng.NormFloat64()*d.MemNoiseFraction))
		if v < 0 {
			v = 0
		}
	}
	if d.MemCeiling > 0 && v > d.MemCeiling {
		v = d.MemCeiling
	}
	return v
}

// PeakCPU is the ground truth: the smallest CPU allocation that never starves
// this workload, computed by replaying demand with no allocation limit.
// Tests assert recommendations against this rather than against a hard-coded
// number, so changing the demand model cannot silently invalidate them.
func PeakCPU(d Demand, from time.Time, dur time.Duration, seed int64) float64 {
	e := New(d, Allocation{}, from, seed)
	e.Run(dur)
	var peak float64
	for _, s := range e.samples {
		peak = math.Max(peak, s.CPUDemand)
	}
	return peak
}

// PeakMem is the ground truth for memory: the largest demand observed with no
// allocation limit, which is the smallest limit that avoids an OOMKill.
func PeakMem(d Demand, from time.Time, dur time.Duration, seed int64) int64 {
	e := New(d, Allocation{}, from, seed)
	e.Run(dur)
	var peak int64
	for _, s := range e.samples {
		if s.MemDemand > peak {
			peak = s.MemDemand
		}
	}
	return peak
}
