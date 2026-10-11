// Command mkcorpus extracts container specs from rendered Kubernetes manifests
// into a JSONL corpus.
//
// The corpus is the prerequisite for every path to better ceiling detection:
// it is the test suite for hand-written parsers, the eval set for prompt-based
// extraction, and the training data for any finetune. Without it, "a model
// would do this better" is an assertion rather than a measurement.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"
)

// Record is one container spec plus where it came from.
type Record struct {
	Source    string            `json:"source"`
	Kind      string            `json:"kind"`
	Workload  string            `json:"workload"`
	Container string            `json:"container"`
	Init      bool              `json:"init,omitempty"`
	Image     string            `json:"image"`
	Command   []string          `json:"command,omitempty"`
	Args      []string          `json:"args,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	OpaqueEnv []string          `json:"opaqueEnv,omitempty"`
	MemLimit  int64             `json:"memLimitBytes,omitempty"`
	CPULimit  int64             `json:"cpuLimitMilli,omitempty"`
}

// key deduplicates: charts share sidecars, and counting the same
// prometheus-node-exporter twenty times would flatter any coverage number.
func (r Record) key() string {
	b, _ := json.Marshal([]any{r.Image, r.Command, r.Args, r.Env, r.OpaqueEnv})
	return string(b)
}

type doc struct {
	Kind     string `json:"kind"`
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		Template       *corev1.PodTemplateSpec `json:"template"`
		Containers     []corev1.Container      `json:"containers"`
		InitContainers []corev1.Container      `json:"initContainers"`
		JobTemplate    *struct {
			Spec struct {
				Template corev1.PodTemplateSpec `json:"template"`
			} `json:"spec"`
		} `json:"jobTemplate"`
	} `json:"spec"`
}

func main() {
	source := "stdin"
	if len(os.Args) > 1 {
		source = os.Args[1]
	}

	seen := map[string]bool{}
	var out []Record

	r := k8syaml.NewYAMLReader(bufio.NewReaderSize(os.Stdin, 1<<20))
	for {
		raw, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "read:", err)
			break
		}
		if len(strings.TrimSpace(string(raw))) == 0 {
			continue
		}
		var d doc
		if err := yaml.Unmarshal(raw, &d); err != nil {
			continue // not every document is a workload
		}

		var pods []corev1.PodSpec
		switch {
		case d.Spec.Template != nil:
			pods = append(pods, d.Spec.Template.Spec)
		case d.Spec.JobTemplate != nil:
			pods = append(pods, d.Spec.JobTemplate.Spec.Template.Spec)
		case len(d.Spec.Containers) > 0:
			pods = append(pods, corev1.PodSpec{
				Containers: d.Spec.Containers, InitContainers: d.Spec.InitContainers,
			})
		}

		for _, ps := range pods {
			for _, c := range ps.InitContainers {
				add(&out, seen, record(source, d, c, true))
			}
			for _, c := range ps.Containers {
				add(&out, seen, record(source, d, c, false))
			}
		}
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Image != out[j].Image {
			return out[i].Image < out[j].Image
		}
		if out[i].Container != out[j].Container {
			return out[i].Container < out[j].Container
		}
		return out[i].key() < out[j].key()
	})

	enc := json.NewEncoder(os.Stdout)
	for _, r := range out {
		if err := enc.Encode(r); err != nil {
			fmt.Fprintln(os.Stderr, "encode:", err)
			os.Exit(1)
		}
	}
	fmt.Fprintf(os.Stderr, "%d unique container specs\n", len(out))
}

func add(out *[]Record, seen map[string]bool, r Record) {
	if r.Image == "" || seen[r.key()] {
		return
	}
	seen[r.key()] = true
	*out = append(*out, r)
}

func record(source string, d doc, c corev1.Container, init bool) Record {
	r := Record{
		Source: source, Kind: d.Kind, Workload: d.Metadata.Name,
		Container: c.Name, Init: init, Image: c.Image,
		Command: c.Command, Args: c.Args, Env: map[string]string{},
	}
	for _, e := range c.Env {
		if e.ValueFrom != nil {
			r.OpaqueEnv = append(r.OpaqueEnv, e.Name)
			continue
		}
		r.Env[e.Name] = e.Value
	}
	for _, ef := range c.EnvFrom {
		switch {
		case ef.ConfigMapRef != nil:
			r.OpaqueEnv = append(r.OpaqueEnv, "configMap:"+ef.ConfigMapRef.Name)
		case ef.SecretRef != nil:
			r.OpaqueEnv = append(r.OpaqueEnv, "secret:"+ef.SecretRef.Name)
		}
	}
	if len(r.Env) == 0 {
		r.Env = nil
	}
	if q, ok := c.Resources.Limits[corev1.ResourceMemory]; ok {
		r.MemLimit = q.Value()
	}
	if q, ok := c.Resources.Limits[corev1.ResourceCPU]; ok {
		r.CPULimit = q.MilliValue()
	}
	return r
}
