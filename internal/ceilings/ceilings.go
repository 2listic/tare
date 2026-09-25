// Package ceilings detects resource ceilings that an application imposes on
// itself, independently of its Kubernetes requests and limits.
//
// A JVM started with -Xmx256m will never use more than 256MB of heap however
// much memory the container is given. For such a container the Kubernetes
// memory limit is not the lever: raising it buys nothing and lowering it below
// the heap size plus non-heap overhead invites an OOMKill. The honest output is
// the flag itself, not a recommendation.
//
// The rule this package exists to enforce: never recommend a memory size below
// a detected ceiling, and when a runtime is recognised but its ceiling cannot
// be read, say so rather than guess.
package ceilings

// Resource names the Kubernetes resource a ceiling constrains.
type Resource string

const (
	CPU    Resource = "cpu"
	Memory Resource = "memory"
	// VRAM is GPU memory. It is a separate resource because Kubernetes cannot
	// schedule it: `nvidia.com/gpu: 1` reserves a whole device whatever the
	// model needs, so VRAM is never a request to right-size and always a
	// constraint to check the configuration against.
	VRAM Resource = "vram"
)

// Kind identifies which runtime knob produced a ceiling.
type Kind string

const (
	JVMHeapMax       Kind = "jvm-heap-max"        // -Xmx
	JVMMaxRAMPercent Kind = "jvm-max-ram-percent" // -XX:MaxRAMPercentage
	JVMDefaultHeap   Kind = "jvm-default-heap"    // no flag; JVM container default
	GoMemLimit       Kind = "go-mem-limit"        // GOMEMLIMIT
	GoMaxProcs       Kind = "go-max-procs"        // GOMAXPROCS
	NodeOldSpace     Kind = "node-old-space"      // --max-old-space-size
	PythonWorkers    Kind = "python-workers"      // gunicorn/uvicorn --workers

	// Inference servers. For these the pod spec is almost the whole story:
	// what the process reserves on the GPU is set by flags at startup and
	// observation cannot see past them.
	VLLMMemUtil    Kind = "vllm-gpu-memory-utilization"
	VLLMModelLen   Kind = "vllm-max-model-len"
	VLLMSwapSpace  Kind = "vllm-swap-space"
	ModelWeights   Kind = "model-weights" // VRAM the weights alone occupy
	OllamaKeepWarm Kind = "ollama-keep-alive"
	OllamaParallel Kind = "ollama-num-parallel"
	TGITokens      Kind = "tgi-max-total-tokens"
	TGIMemFraction Kind = "tgi-cuda-memory-fraction"
	LlamaGPULayers Kind = "llama-n-gpu-layers"
)

// Container is the minimal view of a pod container that detection needs.
// Keeping k8s API types out of this package makes every parser a pure function
// over data, which is what allows the table-driven tests to cover the real
// world spellings without a cluster.
type Container struct {
	Name    string
	Image   string
	Command []string
	Args    []string
	Env     map[string]string

	// OpaqueEnv holds names of environment variables whose values come from a
	// ConfigMap, Secret or the downward API. Their values are not in the pod
	// spec, so a ceiling hiding in one of them is undetectable and must be
	// reported as unresolved rather than assumed absent.
	OpaqueEnv []string

	// MemoryLimitBytes is the container's memory limit, needed to resolve
	// percentage-based ceilings. Zero when unset.
	MemoryLimitBytes int64
}

// Ceiling is a self-imposed limit found in a container's configuration.
type Ceiling struct {
	Kind     Kind
	Resource Resource

	// Source says where the value was found: "args", "env:JAVA_OPTS", or
	// "image" for a runtime default inferred from the image.
	Source string

	// Raw is the flag or value exactly as it appeared, empty for defaults.
	Raw string

	// Bytes is the resolved memory ceiling. Zero when Resource is CPU or when
	// the value could not be resolved.
	Bytes int64

	// Millicores is the resolved ceiling on useful CPU. Zero when Resource is
	// Memory or when the value could not be resolved.
	Millicores int64

	// VRAMBytes is GPU memory this ceiling accounts for, resolved. Zero when
	// the resource is not VRAM or when the value could not be determined.
	VRAMBytes int64

	// Fraction is the share of a device a flag claims, for the flags that are
	// expressed that way (vLLM's --gpu-memory-utilization, TGI's
	// --cuda-memory-fraction). Zero when the ceiling is not a fraction.
	Fraction float64

	// MinContainerBytes is the smallest container memory size that can host
	// this ceiling without dying. It is always larger than Bytes: a JVM needs
	// its heap PLUS metaspace, code cache, thread stacks and direct buffers,
	// and sizing a container to the heap alone is a reliable way to OOMKill it.
	// This, not Bytes, is what a recommendation must respect.
	MinContainerBytes int64

	// ScalesWithLimit marks a ceiling defined as a proportion of the container
	// memory limit rather than as a fixed size. Such a ceiling moves when the
	// limit moves, so it must NOT act as a floor: treating it as one would
	// make every unflagged JVM permanently unshrinkable, pinned at a quarter
	// of whatever limit it happens to have today.
	ScalesWithLimit bool

	// Unresolved marks a runtime that was recognised but whose ceiling could
	// not be determined. Guessing here produces recommendations that OOMKill,
	// so this is a first-class outcome and not an error.
	Unresolved bool

	// Note explains the finding in terms an operator can act on.
	Note string
}

// Detect returns every ceiling found in c.
func Detect(c Container) []Ceiling {
	var out []Ceiling
	out = append(out, detectJVM(c)...)
	out = append(out, detectGo(c)...)
	out = append(out, detectNode(c)...)
	out = append(out, detectPython(c)...)
	out = append(out, detectInference(c)...)
	return out
}

// VRAMFloor reports the GPU memory every resolved ceiling requires, and
// whether any VRAM ceiling was unresolved.
//
// Unlike MemoryFloor this is never a recommendation input: a GPU is an integer
// extended resource and there is no smaller size to ask for. It exists to be
// compared against the device the pod actually landed on, which is the only
// question worth asking - does this configuration fit?
func VRAMFloor(cs []Ceiling) (floor int64, blocked bool) {
	for _, c := range cs {
		if c.Resource != VRAM {
			continue
		}
		if c.Unresolved {
			blocked = true
			continue
		}
		floor += c.VRAMBytes
	}
	return floor, blocked
}

// MemUtilFraction returns the largest share of a GPU device any detected flag
// claims, and whether one was found.
func MemUtilFraction(cs []Ceiling) (float64, bool) {
	var out float64
	var found bool
	for _, c := range cs {
		if c.Resource == VRAM && c.Fraction > 0 {
			out, found = max(out, c.Fraction), true
		}
	}
	return out, found
}

// MemoryFloor reports the smallest container memory size that satisfies every
// detected ceiling, and whether any ceiling was unresolved. A caller seeing
// blocked=true must decline to recommend rather than fall back to the usage
// percentiles.
//
// The floor is MinContainerBytes, not the raw app-level cap: the container has
// to hold the runtime's overhead too.
func MemoryFloor(cs []Ceiling) (floor int64, blocked bool) {
	for _, c := range cs {
		if c.Resource != Memory {
			continue
		}
		if c.Unresolved {
			blocked = true
			continue
		}
		if c.ScalesWithLimit {
			continue // moves with the limit, so it constrains nothing
		}
		if c.MinContainerBytes > floor {
			floor = c.MinContainerBytes
		}
	}
	return floor, blocked
}

// jvmOverhead estimates non-heap JVM memory: metaspace, code cache, thread
// stacks, GC structures and direct buffers. A quarter of heap with a 128Mi
// minimum is the widely used rule of thumb and errs on the safe side, which is
// the correct direction to err when the failure mode is an OOMKill.
func jvmOverhead(heap int64) int64 {
	return max(heap/4, 128<<20)
}

// CPUCeiling reports the smallest resolved cap on useful CPU. Recommending
// more CPU than this is waste: the application cannot use it.
func CPUCeiling(cs []Ceiling) (millicores int64, ok bool) {
	for _, c := range cs {
		if c.Resource != CPU || c.Unresolved || c.Millicores <= 0 {
			continue
		}
		if !ok || c.Millicores < millicores {
			millicores, ok = c.Millicores, true
		}
	}
	return millicores, ok
}

func (c Container) opaque(name string) bool {
	for _, o := range c.OpaqueEnv {
		if o == name {
			return true
		}
	}
	return false
}

func (c Container) argv() []string {
	return append(append([]string{}, c.Command...), c.Args...)
}
