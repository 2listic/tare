package analyze

import (
	"strings"
	"testing"

	"github.com/2listic/tare/internal/ceilings"
	"github.com/2listic/tare/internal/discover"
)

func gpuNode(name, product string, memGiB int64) discover.Node {
	return discover.Node{
		Name: name, Cores: 32, AllocCPU: 32000, AllocMem: 128 << 30,
		Schedulable: true, Arch: "amd64",
		GPUs: 1, GPUResource: "nvidia.com/gpu",
		GPUProduct: product, GPUMemBytes: memGiB << 30,
	}
}

// vllmFinding builds a finding as the pipeline would for a vLLM container.
func vllmFinding(args ...string) *Finding {
	c := ceilings.Container{Name: "server", Image: "vllm/vllm-openai:v0.6.3", Args: args, Env: map[string]string{}}
	return &Finding{
		Workload: "Deployment/llm", Container: "server", Pods: 1, GPUs: 1,
		Nodes: []string{"gpu-0"}, Ceilings: ceilings.Detect(c),
	}
}

func TestOrdinaryWorkloadGetsNoInferenceFindings(t *testing.T) {
	// The negative control that matters most: this runs on every container in
	// every cluster, and a false positive here is noise on the whole report.
	f := &Finding{
		Workload: "Deployment/web", Container: "nginx", Pods: 3, Nodes: []string{"gpu-0"},
		Ceilings: ceilings.Detect(ceilings.Container{Name: "nginx", Image: "nginx:1.27"}),
	}
	applyInference(f, inv(gpuNode("gpu-0", "NVIDIA-A100", 80)))
	if len(f.Reasons) != 0 {
		t.Errorf("a web server must produce no GPU findings: %v", f.Reasons)
	}
}

// An inference server with no GPU request is invisible to a CPU-and-memory
// report, and is either crash-looping or running 50x slower than intended.
func TestInferenceServerWithoutAGPU(t *testing.T) {
	f := vllmFinding("--model", "meta-llama/Llama-3.1-8B-Instruct")
	f.GPUs = 0
	applyInference(f, inv(gpuNode("gpu-0", "NVIDIA-A100", 80)))
	if !strings.Contains(strings.Join(ReasonTexts(f.Reasons), " "), "requests no GPU") {
		t.Errorf("a GPU-less inference server must be called out: %v", f.Reasons)
	}
}

func TestModelTooLargeForDeviceIsRefused(t *testing.T) {
	// 72B at fp16 is ~144GB. An 80GB A100 cannot hold it without sharding.
	f := vllmFinding("--model", "Qwen/Qwen2.5-72B-Instruct")
	applyInference(f, inv(gpuNode("gpu-0", "NVIDIA-A100-SXM4-80GB", 80)))
	joined := strings.Join(ReasonTexts(f.Reasons), " ")
	if !strings.Contains(joined, "REFUSED") {
		t.Fatalf("a model that cannot load must be refused: %v", f.Reasons)
	}
	if !strings.Contains(joined, "tensor parallelism") {
		t.Errorf("the refusal must name the way out: %v", f.Reasons)
	}
}

// The subtler refusal: the device has room but the server's own reservation
// fraction does not. Raising the memory limit does nothing; the fix is a flag.
func TestModelLargerThanReservedPoolIsRefused(t *testing.T) {
	// 8B at fp16 is 16GB. A 24GB card at --gpu-memory-utilization 0.5 reserves
	// 12GB, which cannot hold it, though the card itself easily could.
	f := vllmFinding("--model", "meta-llama/Llama-3.1-8B-Instruct", "--gpu-memory-utilization", "0.5")
	applyInference(f, inv(gpuNode("gpu-0", "NVIDIA-L4", 24)))
	joined := strings.Join(ReasonTexts(f.Reasons), " ")
	if !strings.Contains(joined, "REFUSED") {
		t.Fatalf("weights above the reserved pool must be refused: %v", f.Reasons)
	}
	if !strings.Contains(joined, "raise the utilisation fraction rather than the memory limit") {
		t.Errorf("the fix must be the flag, not the k8s limit: %v", f.Reasons)
	}
}

func TestFittingModelReportsTheRemainder(t *testing.T) {
	f := vllmFinding("--model", "meta-llama/Llama-3.1-8B-Instruct")
	applyInference(f, inv(gpuNode("gpu-0", "NVIDIA-A100-SXM4-80GB", 80)))
	joined := strings.Join(ReasonTexts(f.Reasons), " ")
	if !strings.Contains(joined, "fits:") {
		t.Fatalf("a fitting model must say so and quantify the remainder: %v", f.Reasons)
	}
	if !strings.Contains(joined, "KV cache") {
		t.Errorf("the remainder is what bounds concurrency and must be named: %v", f.Reasons)
	}
}

// The same manifest means different things on different hardware. This is the
// case a spec-only linter gets wrong and a cluster-aware tool does not.
func TestSameManifestDiffersByDevice(t *testing.T) {
	args := []string{"--model", "meta-llama/Llama-3.1-8B-Instruct"}
	big := vllmFinding(args...)
	applyInference(big, inv(gpuNode("gpu-0", "NVIDIA-A100-SXM4-80GB", 80)))
	small := vllmFinding(args...)
	applyInference(small, inv(gpuNode("gpu-0", "NVIDIA-GeForce-RTX-3060", 6)))

	if strings.Contains(strings.Join(ReasonTexts(big.Reasons), " "), "REFUSED") {
		t.Errorf("8B fits an 80GB A100: %v", big.Reasons)
	}
	if !strings.Contains(strings.Join(ReasonTexts(small.Reasons), " "), "REFUSED") {
		t.Errorf("8B at fp16 cannot fit a 6GB card: %v", small.Reasons)
	}
}

// The smallest device is the one the check must pass: the replica that lands
// on the small card is the one that fails.
func TestMixedGPUFleetSizesAgainstTheSmallest(t *testing.T) {
	f := vllmFinding("--model", "meta-llama/Llama-3.1-8B-Instruct")
	f.Nodes = []string{"gpu-0", "gpu-1"}
	applyInference(f, inv(
		gpuNode("gpu-0", "NVIDIA-A100-SXM4-80GB", 80),
		gpuNode("gpu-1", "NVIDIA-GeForce-RTX-3060", 6),
	))
	if !strings.Contains(strings.Join(ReasonTexts(f.Reasons), " "), "REFUSED") {
		t.Errorf("a fleet containing a 6GB card must fail the check: %v", f.Reasons)
	}
}

func TestUnknownDeviceMemoryIsStatedNotGuessed(t *testing.T) {
	n := gpuNode("gpu-0", "", 0)
	n.GPUMemBytes, n.GPUProduct = 0, "" // a hand-installed device plugin
	f := vllmFinding("--model", "meta-llama/Llama-3.1-8B-Instruct")
	applyInference(f, inv(n))
	joined := strings.Join(ReasonTexts(f.Reasons), " ")
	if !strings.Contains(joined, "no node it ran on reports a device memory size") {
		t.Fatalf("an unknown device must be reported, not assumed: %v", f.Reasons)
	}
	if strings.Contains(joined, "REFUSED") || strings.Contains(joined, "fits:") {
		t.Errorf("no fit verdict may be reached without a device size: %v", f.Reasons)
	}
}

func TestUnresolvedModelBlocksTheFitCheck(t *testing.T) {
	f := vllmFinding("--model", "internal/our-finetune-latest")
	applyInference(f, inv(gpuNode("gpu-0", "NVIDIA-A100", 80)))
	joined := strings.Join(ReasonTexts(f.Reasons), " ")
	if !strings.Contains(joined, "is unknown and is not guessed") {
		t.Fatalf("an unreadable model must block the fit check: %v", f.Reasons)
	}
	if strings.Contains(joined, "fits:") {
		t.Errorf("a blocked check must not report a fit: %v", f.Reasons)
	}
}

func TestReplicasSharingADeviceAreWarned(t *testing.T) {
	f := vllmFinding("--model", "meta-llama/Llama-3.2-1B", "--gpu-memory-utilization", "0.9")
	f.Pods = 2
	applyInference(f, inv(gpuNode("gpu-0", "NVIDIA-L4", 24)))
	joined := strings.Join(ReasonTexts(f.Reasons), " ")
	if !strings.Contains(joined, "180% in total") {
		t.Fatalf("two replicas at 0.9 cannot share a device: %v", f.Reasons)
	}
	if !strings.Contains(joined, "working today by luck") {
		t.Errorf("the finding must say the current success is not evidence: %v", f.Reasons)
	}
}

func TestOllamaKeepAliveSurfacesWithoutAFitCheck(t *testing.T) {
	c := ceilings.Container{Name: "ollama", Image: "ollama/ollama:0.5.4",
		Env: map[string]string{"OLLAMA_KEEP_ALIVE": "-1"}}
	f := &Finding{Workload: "Deployment/ollama", Pods: 1, GPUs: 1,
		Nodes: []string{"gpu-0"}, Ceilings: ceilings.Detect(c)}
	applyInference(f, inv(gpuNode("gpu-0", "NVIDIA-L4", 24)))
	// No model is declared in an Ollama spec, so there is nothing to fit-check;
	// the keep-alive finding still has to reach the report.
	if len(f.Ceilings) == 0 {
		t.Fatal("the keep-alive ceiling must be detected")
	}
}
