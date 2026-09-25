package ceilings

import (
	"strings"
	"testing"
)

func TestParamCountFromModelName(t *testing.T) {
	cases := []struct {
		model string
		want  float64
		ok    bool
	}{
		{"meta-llama/Llama-3.1-8B-Instruct", 8, true},
		{"Qwen/Qwen2.5-72B-Instruct", 72, true},
		{"mistralai/Mistral-7B-v0.3", 7, true},
		{"meta-llama/Llama-2-13b-chat-hf", 13, true},
		{"google/gemma-2b-it", 2, true},
		{"meta-llama/Llama-3.2-1B", 1, true},
		{"deepseek-ai/DeepSeek-R1-Distill-Qwen-32B", 32, true},
		{"TheBloke/Llama-2-7B-AWQ", 7, true},
		// A version number is not a parameter count. Reading 3.1 here would
		// under-size an 8B model by 60% and pass a fit check that must fail.
		{"meta-llama/Llama-3.1", 0, false},
		{"stabilityai/stable-diffusion-xl-base-1.0", 0, false},
		{"openai/whisper-large-v3", 0, false},
		{"BAAI/bge-m3", 0, false},
		// The organisation is stripped, so an org name cannot be read as a size.
		{"1b-labs/some-model", 0, false},
		{"", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			got, ok := paramCount(tc.model)
			if ok != tc.ok || got != tc.want {
				t.Errorf("paramCount(%q) = %v,%v want %v,%v", tc.model, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func vllm(args ...string) Container {
	return Container{Name: "server", Image: "vllm/vllm-openai:v0.6.3", Args: args, Env: map[string]string{}}
}

func TestVLLMDefaultMemoryUtilisationIsReported(t *testing.T) {
	// The default is the whole point: a manifest with no flag still claims 90%
	// of the device, and the operator reading the manifest cannot see it.
	cs := Detect(vllm("--model", "meta-llama/Llama-3.1-8B-Instruct"))
	c := find(cs, VLLMMemUtil)
	ok := c != nil
	if !ok {
		t.Fatal("the reservation must be reported even when it comes from the default")
	}
	if c.Fraction != 0.9 {
		t.Errorf("Fraction = %v, want 0.9", c.Fraction)
	}
	if !strings.Contains(c.Note, "not its demand") {
		t.Errorf("the note must say observation cannot see past the flag: %s", c.Note)
	}
}

func TestVLLMWeightsArithmetic(t *testing.T) {
	cs := Detect(vllm("--model", "meta-llama/Llama-3.1-8B-Instruct"))
	c := find(cs, ModelWeights)
	ok := c != nil
	if !ok || c.Unresolved {
		t.Fatalf("weights must resolve for a named 8B model: %+v", c)
	}
	// 8e9 params x 2 bytes = 16GB.
	if want := int64(16e9); c.VRAMBytes != want {
		t.Errorf("VRAMBytes = %d, want %d", c.VRAMBytes, want)
	}
}

func TestVLLMQuantizationFromFlagAndFromName(t *testing.T) {
	byFlag := Detect(vllm("--model", "meta-llama/Llama-2-7B", "--quantization", "awq"))
	c := find(byFlag, ModelWeights)
	if want := int64(7e9 / 2); c.VRAMBytes != want {
		t.Errorf("awq flag: VRAMBytes = %d, want %d", c.VRAMBytes, want)
	}
	byName := Detect(vllm("--model", "TheBloke/Llama-2-7B-AWQ"))
	c2 := find(byName, ModelWeights)
	if want := int64(7e9 / 2); c2.VRAMBytes != want {
		t.Errorf("awq in name: VRAMBytes = %d, want %d", c2.VRAMBytes, want)
	}
}

func TestVLLMTensorParallelDividesWeights(t *testing.T) {
	cs := Detect(vllm("--model", "Qwen/Qwen2.5-72B-Instruct", "--tensor-parallel-size", "4"))
	c := find(cs, ModelWeights)
	if want := int64(72e9*2) / 4; c.VRAMBytes != want {
		t.Errorf("VRAMBytes = %d, want %d per shard", c.VRAMBytes, want)
	}
	if !strings.Contains(c.Note, "4 tensor-parallel shards") {
		t.Errorf("the split must be explained: %s", c.Note)
	}
}

// An unreadable model id must refuse rather than assume. A wrong weight figure
// produces a fit check that passes on a model that cannot load, which is worse
// than no check at all.
func TestVLLMUnknownModelIsUnresolved(t *testing.T) {
	cs := Detect(vllm("--model", "internal/our-finetune-latest"))
	c := find(cs, ModelWeights)
	ok := c != nil
	if !ok || !c.Unresolved {
		t.Fatalf("an unreadable model id must be unresolved, got %+v", c)
	}
	if _, blocked := VRAMFloor(cs); !blocked {
		t.Error("VRAMFloor must report blocked when a VRAM ceiling is unresolved")
	}
}

func TestVLLMSwapSpaceIsHostMemoryNotVRAM(t *testing.T) {
	// The trap: --swap-space sounds like a GPU setting and is charged to the
	// container memory limit. It must land in the memory floor, not the VRAM one.
	cs := Detect(vllm("--model", "meta-llama/Llama-3.1-8B-Instruct", "--swap-space", "8"))
	c := find(cs, VLLMSwapSpace)
	ok := c != nil
	if !ok {
		t.Fatal("--swap-space must be detected")
	}
	if c.Resource != Memory {
		t.Errorf("Resource = %s, want memory: swap space is host RAM", c.Resource)
	}
	floor, _ := MemoryFloor(cs)
	if floor < 8<<30 {
		t.Errorf("memory floor = %s, must cover the 8Gi swap space", humanBytes(floor))
	}
}

func TestVLLMKVBudgetIsStatedNotGuessed(t *testing.T) {
	cs := Detect(vllm("--model", "meta-llama/Llama-3.1-8B-Instruct",
		"--max-model-len", "32768", "--max-num-seqs", "256"))
	c := find(cs, VLLMModelLen)
	ok := c != nil
	if !ok {
		t.Fatal("the KV cache budget must be reported")
	}
	if !strings.Contains(c.Note, "8.4M tokens") {
		t.Errorf("the token budget must be computed: %s", c.Note)
	}
	// Bytes per token need config.json, which is not in the pod spec. Claiming
	// a byte figure here would be the invention this tool refuses to make.
	if c.VRAMBytes != 0 {
		t.Errorf("VRAMBytes = %d: KV cache bytes cannot be known from a pod spec", c.VRAMBytes)
	}
	if !strings.Contains(c.Note, "config.json") {
		t.Errorf("the note must say why the bytes are not computed: %s", c.Note)
	}
}

func TestVLLMBadUtilisationIsUnresolved(t *testing.T) {
	cs := Detect(vllm("--model", "meta-llama/Llama-3.1-8B-Instruct", "--gpu-memory-utilization", "90"))
	c := find(cs, VLLMMemUtil)
	ok := c != nil
	if !ok || !c.Unresolved {
		t.Fatalf("90 is not a fraction and vLLM will not start; got %+v", c)
	}
}

func TestOllamaKeepAliveForever(t *testing.T) {
	c := Container{Name: "ollama", Image: "ollama/ollama:0.5.4",
		Env: map[string]string{"OLLAMA_KEEP_ALIVE": "-1"}}
	ce := find(Detect(c), OllamaKeepWarm)
	if ce == nil {
		t.Fatal("OLLAMA_KEEP_ALIVE must be detected")
	}
	if !strings.Contains(ce.Note, "indefinitely") {
		t.Errorf("a pinned model is the most expensive setting here and must say so: %s", ce.Note)
	}
}

func TestOllamaMaxLoadedModelsSumsVRAM(t *testing.T) {
	c := Container{Name: "ollama", Image: "ollama/ollama",
		Env: map[string]string{"OLLAMA_MAX_LOADED_MODELS": "3"}}
	ce := find(Detect(c), OllamaKeepWarm)
	if ce == nil || !strings.Contains(ce.Note, "sum of the 3 largest") {
		t.Errorf("peak VRAM is the sum, not the largest single model: %+v", ce)
	}
}

// The silent whole-GPU waste: llama.cpp with no -ngl runs entirely on the CPU
// while holding an allocated device.
func TestLlamaCppWithoutGPULayers(t *testing.T) {
	c := Container{Name: "srv", Image: "ghcr.io/ggerganov/llama.cpp:server",
		Command: []string{"/llama-server"}, Args: []string{"-m", "/models/q4.gguf"}}
	ce := find(Detect(c), LlamaGPULayers)
	if ce == nil {
		t.Fatal("a missing -ngl must be reported")
	}
	if !strings.Contains(ce.Note, "every layer on the CPU") {
		t.Errorf("the consequence must be named: %s", ce.Note)
	}
}

func TestLlamaCppExplicitZeroLayers(t *testing.T) {
	c := Container{Name: "srv", Image: "ghcr.io/ggerganov/llama.cpp:server",
		Command: []string{"/llama-server"}, Args: []string{"-ngl", "0"}}
	ce := find(Detect(c), LlamaGPULayers)
	if !strings.Contains(ce.Note, "idle by configuration") {
		t.Errorf("-ngl 0 is an idle GPU: %s", ce.Note)
	}
}

func TestTGIWeightsAndFraction(t *testing.T) {
	c := Container{Name: "tgi", Image: "ghcr.io/huggingface/text-generation-inference:2.4.0",
		Args: []string{"--model-id", "meta-llama/Llama-3.1-8B-Instruct",
			"--cuda-memory-fraction", "0.8", "--max-total-tokens", "4096"}}
	cs := Detect(c)
	if w := find(cs, ModelWeights); w == nil || w.VRAMBytes != int64(16e9) {
		t.Errorf("TGI weights not resolved: %+v", w)
	}
	if f := find(cs, TGIMemFraction); f == nil || f.Fraction != 0.8 {
		t.Errorf("--cuda-memory-fraction not read: %+v", f)
	}
	if find(cs, TGITokens) == nil {
		t.Error("--max-total-tokens must be reported")
	}
}

// The negative control. An ordinary web server must produce no inference
// findings at all: a parser that fires on everything is worse than none.
func TestOrdinaryContainersProduceNoInferenceCeilings(t *testing.T) {
	for _, c := range []Container{
		{Name: "nginx", Image: "nginx:1.27"},
		{Name: "app", Image: "eclipse-temurin:21-jre", Env: map[string]string{"JAVA_OPTS": "-Xmx256m"}},
		{Name: "api", Image: "python:3.12", Args: []string{"-m", "gunicorn", "--workers", "4"}},
		{Name: "go", Image: "alpine", Env: map[string]string{"GOMEMLIMIT": "512MiB"}},
	} {
		t.Run(c.Image, func(t *testing.T) {
			for _, ce := range Detect(c) {
				if ce.Resource == VRAM {
					t.Errorf("a %s container produced a VRAM ceiling: %+v", c.Image, ce)
				}
			}
		})
	}
}

func TestVRAMFloorSumsResolvedCeilings(t *testing.T) {
	// VRAM ceilings add rather than max: the weights and anything else
	// resident occupy the device at the same time.
	cs := []Ceiling{
		{Resource: VRAM, VRAMBytes: 16e9},
		{Resource: VRAM, VRAMBytes: 2e9},
		{Resource: Memory, Bytes: 1 << 30},
	}
	floor, blocked := VRAMFloor(cs)
	if blocked {
		t.Error("nothing here is unresolved")
	}
	if want := int64(18e9); floor != want {
		t.Errorf("floor = %d, want %d", floor, want)
	}
}

// humanBytes is exact so that a Kubernetes quantity round-trips, which renders
// 16e9 as "15625000Ki". A VRAM figure is never a Kubernetes quantity and has to
// be readable against a device size instead.
func TestVRAMFiguresAreReadable(t *testing.T) {
	cs := Detect(vllm("--model", "meta-llama/Llama-3.1-8B-Instruct"))
	c := find(cs, ModelWeights)
	if strings.Contains(c.Note, "Ki)") || strings.Contains(c.Note, "Ki ") {
		t.Errorf("a VRAM figure rendered in exact kibibytes is unreadable: %s", c.Note)
	}
	if !strings.Contains(c.Note, "14.9GiB") {
		t.Errorf("8B at fp16 is 16e9 bytes, which is 14.9GiB: %s", c.Note)
	}
}
