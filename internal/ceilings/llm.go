package ceilings

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Inference servers are the clearest case this package exists for.
//
// A vLLM process started with --gpu-memory-utilization 0.9 claims 90% of the
// device at boot and holds it for the KV cache whether or not a single request
// arrives. Its observed GPU memory is therefore a constant that describes the
// flag and not the demand, which makes usage-based sizing not merely
// imprecise but meaningless. The honest output is the configuration, the
// arithmetic it implies, and a refusal where the two do not fit.
//
// Nothing here is a recommendation. Kubernetes allocates GPUs as whole integer
// devices, so there is no smaller number to ask for; every finding is either a
// fit check or a configuration defect.

// bytesPerParam is how much VRAM one parameter occupies at a given precision.
// Expressed in thousandths of a byte so the table stays integer.
var bytesPerParamMilli = map[string]int64{
	"float32": 4000, "fp32": 4000, "f32": 4000,
	"float16": 2000, "fp16": 2000, "f16": 2000, "half": 2000,
	"bfloat16": 2000, "bf16": 2000,
	"float8": 1000, "fp8": 1000, "int8": 1000, "8bit": 1000,
	"awq": 500, "gptq": 500, "int4": 500, "4bit": 500, "nf4": 500, "q4": 500,
}

// paramCount reads a parameter count out of a model identifier.
//
// Model names on the Hugging Face hub encode their size almost without
// exception: Llama-3.1-8B-Instruct, Qwen2.5-72B, Mistral-7B-v0.3. The digit
// must be followed by a B to match, so the 3.1 in a version string is not
// mistaken for 3.1 billion parameters. When no size can be read the caller
// reports the ceiling as unresolved rather than assuming one, because a wrong
// weight figure produces a fit check that passes on a model that cannot load.
var paramRe = regexp.MustCompile(`(?i)(\d+(?:\.\d+)?)\s*b(?:[-_./]|$)`)

func paramCount(model string) (billions float64, ok bool) {
	// Strip any registry path so an organisation called "1b-labs" cannot be
	// read as a parameter count.
	name := lastSegment(model)
	m := paramRe.FindStringSubmatch(name + "/")
	if m == nil {
		return 0, false
	}
	v, err := strconv.ParseFloat(m[1], 64)
	if err != nil || v <= 0 || v > 2000 {
		return 0, false
	}
	return v, true
}

// precisionOf resolves bytes-per-parameter from an explicit dtype or
// quantization flag, falling back to what the model name advertises and then
// to fp16, which is what almost every published checkpoint ships as.
func precisionOf(dtype, quant, model string) (milli int64, source string) {
	for _, cand := range []struct{ v, from string }{
		{quant, "quantization"}, {dtype, "dtype"},
	} {
		if cand.v == "" {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(cand.v))
		if key == "auto" || key == "none" {
			continue
		}
		// A flag can name two precisions ("gptq-int8" holds both gptq and int8),
		// and map order used to pick between them at random. Take the widest:
		// overstating the weights refuses a fit, understating it passes one that
		// cannot load.
		var widest int64
		for name, b := range bytesPerParamMilli {
			if strings.Contains(key, name) {
				widest = max(widest, b)
			}
		}
		if widest > 0 {
			return widest, cand.from + "=" + cand.v
		}
	}
	lower := strings.ToLower(model)
	for _, name := range []string{"awq", "gptq", "int4", "4bit", "fp8", "int8", "8bit"} {
		if strings.Contains(lower, name) {
			return bytesPerParamMilli[name], "model name advertises " + name
		}
	}
	return 2000, "fp16 assumed: no dtype or quantization flag, and published checkpoints are almost always 16-bit"
}

func detectInference(c Container) []Ceiling {
	var out []Ceiling
	out = append(out, detectVLLM(c)...)
	out = append(out, detectOllama(c)...)
	out = append(out, detectTGI(c)...)
	out = append(out, detectLlamaCpp(c)...)
	return out
}

func looksVLLM(c Container) bool {
	argv := strings.Join(c.argv(), " ")
	return strings.Contains(strings.ToLower(c.Image), "vllm") ||
		strings.Contains(argv, "vllm")
}

func detectVLLM(c Container) []Ceiling {
	if !looksVLLM(c) {
		return nil
	}
	argv := expandArgv(c.argv())
	var out []Ceiling

	model, _, _ := flagValue(argv, "--model", "--served-model-name")
	if model == "" {
		model = c.Env["MODEL"]
	}
	dtype, _, _ := flagValue(argv, "--dtype")
	quant, _, _ := flagValue(argv, "--quantization", "-q")
	out = append(out, weightsCeiling(model, dtype, quant, tensorParallel(argv)))

	// --gpu-memory-utilization is the whole reservation. It is a FRACTION of
	// whatever device the pod lands on, so the same manifest claims 21.6GiB on
	// a 24GB card and 5.4GiB on a 6GB one, which is why this finding has to be
	// resolved against the node rather than reported on its own.
	util := 0.9
	raw, rawFlag, found := flagValue(argv, "--gpu-memory-utilization")
	source := "default"
	if found {
		if v, err := strconv.ParseFloat(raw, 64); err == nil && v > 0 && v <= 1 {
			util, source = v, rawFlag
		} else {
			out = append(out, Ceiling{
				Kind: VLLMMemUtil, Resource: VRAM, Source: "args", Raw: rawFlag, Unresolved: true,
				Note: fmt.Sprintf("--gpu-memory-utilization=%q is not a fraction between 0 and 1; vLLM will refuse to start", raw),
			})
			return out
		}
	}
	out = append(out, Ceiling{
		Kind: VLLMMemUtil, Resource: VRAM, Source: source, Raw: rawFlag, Fraction: util,
		Note: fmt.Sprintf(
			"vLLM claims %.0f%% of the GPU at startup and holds it for the KV cache for the life of the process (--gpu-memory-utilization, default 0.9). "+
				"Its observed GPU memory is therefore this flag and not its demand: the number will not move however little traffic it serves. "+
				"Two replicas sharing one device at this setting cannot both start.", util*100),
	})

	// --max-model-len x --max-num-seqs is the KV cache budget. The exact bytes
	// per token need the model's layer count and head dimensions, which live
	// in its config.json and not in the pod spec, so the token count is
	// reported and the bytes deliberately are not.
	modelLen, mlRaw, mlFound := flagValue(argv, "--max-model-len")
	numSeqs, _, _ := flagValue(argv, "--max-num-seqs")
	if mlFound {
		ml, err1 := strconv.ParseInt(modelLen, 10, 64)
		ns := int64(256) // vLLM's default
		if numSeqs != "" {
			if v, err := strconv.ParseInt(numSeqs, 10, 64); err == nil && v > 0 {
				ns = v
			}
		}
		if err1 == nil && ml > 0 {
			out = append(out, Ceiling{
				Kind: VLLMModelLen, Resource: VRAM, Source: "args", Raw: mlRaw,
				Note: fmt.Sprintf(
					"KV cache must hold %d concurrent sequences of %d tokens (%s tokens total) in whatever the weights leave of the reserved pool. "+
						"The bytes per token depend on the model's layer count and head dimensions, which are in its config.json and not in this pod spec, so tare does not compute them. "+
						"If vLLM logs a smaller effective max_num_seqs than %d, the cache did not fit and concurrency was silently reduced.",
					ns, ml, humanCount(ml*ns), ns),
			})
		}
	}

	// --swap-space is CPU memory, not VRAM, and it is charged to the container
	// limit. Getting this wrong OOMKills the pod for a reason that looks like
	// it should have been a GPU problem.
	if sw, swRaw, ok := flagValue(argv, "--swap-space"); ok {
		if v, err := strconv.ParseFloat(sw, 64); err == nil && v > 0 {
			b := int64(v * float64(1<<30))
			out = append(out, Ceiling{
				Kind: VLLMSwapSpace, Resource: Memory, Source: "args", Raw: swRaw,
				Bytes: b, MinContainerBytes: b + (2 << 30),
				Note: fmt.Sprintf(
					"--swap-space %s is HOST memory per GPU, not VRAM, and it counts against the container's memory limit. "+
						"Allow for it plus the weights' staging buffers or the pod is OOMKilled for what looks like a GPU problem.", humanBytes(b)),
			})
		}
	}
	return out
}

func tensorParallel(argv []string) int64 {
	v, _, ok := flagValue(argv, "--tensor-parallel-size", "-tp")
	if !ok {
		return 1
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return 1
	}
	return n
}

// weightsCeiling is the one piece of VRAM arithmetic that can be done honestly
// from a pod spec: parameters times bytes per parameter, divided across tensor
// parallel shards.
func weightsCeiling(model, dtype, quant string, shards int64) Ceiling {
	ce := Ceiling{Kind: ModelWeights, Resource: VRAM, Source: "args", Raw: model}
	if model == "" {
		ce.Unresolved = true
		ce.Note = "no --model could be read from the pod spec, so the VRAM the weights need cannot be checked against the device"
		return ce
	}
	params, ok := paramCount(model)
	if !ok {
		ce.Unresolved = true
		ce.Note = fmt.Sprintf(
			"no parameter count could be read from the model id %q, so the weights' VRAM cannot be computed. "+
				"tare will not guess: a wrong figure produces a fit check that passes on a model that cannot load.", model)
		return ce
	}
	milli, precSource := precisionOf(dtype, quant, model)
	bytes := int64(params * float64(milli) / 1000 * 1e9)
	per := bytes / shards
	ce.VRAMBytes = per
	shardNote := ""
	if shards > 1 {
		shardNote = fmt.Sprintf(" split across %d tensor-parallel shards, so %s per GPU,", shards, humanVRAM(per))
	}
	ce.Note = fmt.Sprintf(
		"model weights alone need about %s (%.0fB parameters at %s)%s before any KV cache, activations or CUDA context. "+
			"This is a floor on the device, not a request Kubernetes can size: a GPU is allocated whole.",
		humanVRAM(bytes), params, precSource, shardNote)
	return ce
}

func detectOllama(c Container) []Ceiling {
	img := strings.ToLower(c.Image)
	if !strings.Contains(img, "ollama") {
		return nil
	}
	var out []Ceiling

	// OLLAMA_KEEP_ALIVE is the single most expensive setting in a self-hosted
	// deployment. A negative value pins every loaded model in VRAM forever, so
	// an idle GPU stays fully occupied and unavailable to anything else.
	if v, ok := c.Env["OLLAMA_KEEP_ALIVE"]; ok {
		ce := Ceiling{Kind: OllamaKeepWarm, Resource: VRAM, Source: "env:OLLAMA_KEEP_ALIVE", Raw: v}
		switch t := strings.TrimSpace(v); {
		case t == "-1" || strings.EqualFold(t, "forever"):
			ce.Note = "OLLAMA_KEEP_ALIVE=" + t + " pins every loaded model in VRAM indefinitely. The GPU stays fully occupied while idle and no other workload can use it. This is the correct setting only if latency on a cold request is unacceptable; otherwise it is the most expensive line in the manifest."
		case t == "0":
			ce.Note = "OLLAMA_KEEP_ALIVE=0 unloads the model immediately after each request, so every request pays the full model load from disk. VRAM is freed between requests at a large latency cost."
		default:
			ce.Note = fmt.Sprintf("models stay resident in VRAM for %s after their last request (OLLAMA_KEEP_ALIVE), during which the GPU is unavailable to anything else", t)
		}
		out = append(out, ce)
	}
	if v, ok := c.Env["OLLAMA_MAX_LOADED_MODELS"]; ok {
		if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil && n > 1 {
			out = append(out, Ceiling{
				Kind: OllamaMaxModels, Resource: VRAM, Source: "env:OLLAMA_MAX_LOADED_MODELS", Raw: v,
				Note: fmt.Sprintf("up to %d models may be resident in VRAM at once, so peak GPU memory is the sum of the %d largest that get requested, not the size of any one of them", n, n),
			})
		}
	}
	if v, ok := c.Env["OLLAMA_NUM_PARALLEL"]; ok {
		if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil && n > 0 {
			out = append(out, Ceiling{
				Kind: OllamaParallel, Resource: VRAM, Source: "env:OLLAMA_NUM_PARALLEL", Raw: v,
				Note: fmt.Sprintf("KV cache is sized for %d concurrent requests per model; the context window is multiplied by this before it is allocated", n),
			})
		}
	}
	return out
}

func detectTGI(c Container) []Ceiling {
	img := strings.ToLower(c.Image)
	if !strings.Contains(img, "text-generation-inference") && !strings.Contains(img, "/tgi") {
		return nil
	}
	argv := expandArgv(c.argv())
	var out []Ceiling

	model, _, _ := flagValue(argv, "--model-id")
	if model == "" {
		model = c.Env["MODEL_ID"]
	}
	quant, _, _ := flagValue(argv, "--quantize")
	dtype, _, _ := flagValue(argv, "--dtype")
	shards := int64(1)
	if v, _, ok := flagValue(argv, "--num-shard"); ok {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			shards = n
		}
	}
	out = append(out, weightsCeiling(model, dtype, quant, shards))

	if v, raw, ok := flagValue(argv, "--cuda-memory-fraction"); ok {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 && f <= 1 {
			out = append(out, Ceiling{
				Kind: TGIMemFraction, Resource: VRAM, Source: "args", Raw: raw, Fraction: f,
				Note: fmt.Sprintf("TGI reserves %.0f%% of the device up front (--cuda-memory-fraction), so its observed GPU memory reflects this flag rather than its load", f*100),
			})
		}
	}
	if v, raw, ok := flagValue(argv, "--max-total-tokens"); ok {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			out = append(out, Ceiling{
				Kind: TGITokens, Resource: VRAM, Source: "args", Raw: raw,
				Note: fmt.Sprintf("--max-total-tokens %d bounds prompt plus generation per request and sizes the KV cache with it; TGI refuses to start if the cache does not fit the reserved memory", n),
			})
		}
	}
	return out
}

// detectLlamaCpp catches the failure that wastes an entire GPU silently: a
// llama.cpp server with no --n-gpu-layers runs every layer on the CPU. The pod
// holds an allocated device, reports almost no GPU memory, and is slow for a
// reason nothing in the Kubernetes view explains.
func detectLlamaCpp(c Container) []Ceiling {
	img := strings.ToLower(c.Image)
	argv := expandArgv(c.argv())
	joined := strings.ToLower(strings.Join(argv, " "))
	if !strings.Contains(img, "llama.cpp") && !strings.Contains(img, "llamacpp") &&
		!strings.Contains(joined, "llama-server") && !strings.Contains(joined, "llama-cli") {
		return nil
	}
	var out []Ceiling

	v, raw, ok := flagValue(argv, "--n-gpu-layers", "-ngl", "--gpu-layers")
	switch {
	case !ok:
		out = append(out, Ceiling{
			Kind: LlamaGPULayers, Resource: VRAM, Source: "args",
			Note: "no --n-gpu-layers (-ngl) flag: llama.cpp defaults to running every layer on the CPU. If this pod holds a GPU it is paying for a device it never touches, which shows up as an idle GPU and unexplained latency rather than as any Kubernetes-visible error.",
		})
	default:
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil {
			out = append(out, Ceiling{
				Kind: LlamaGPULayers, Resource: VRAM, Source: "args", Raw: raw, Unresolved: true,
				Note: fmt.Sprintf("--n-gpu-layers=%q could not be parsed", v),
			})
		} else if n == 0 {
			out = append(out, Ceiling{
				Kind: LlamaGPULayers, Resource: VRAM, Source: "args", Raw: raw,
				Note: "--n-gpu-layers 0 puts every layer on the CPU. Any GPU allocated to this pod is idle by configuration.",
			})
		} else {
			out = append(out, Ceiling{
				Kind: LlamaGPULayers, Resource: VRAM, Source: "args", Raw: raw,
				Note: fmt.Sprintf("%d layers are offloaded to the GPU; any remaining layers run on the CPU and their share of the work is bounded by the container's CPU limit rather than the device", n),
			})
		}
	}
	return out
}

// humanCount renders a token count readably; the figures here reach millions.
func humanCount(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1_000:
		return fmt.Sprintf("%.0fk", float64(n)/1e3)
	}
	return strconv.FormatInt(n, 10)
}
