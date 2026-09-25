package ceilings

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
)

// The corpus is a set of real container specs rendered from public Helm
// charts. It measures what the hand-written parsers actually cover, which is
// the number any alternative approach has to beat.
//
// Ground truth here is a proxy: a deliberately over-inclusive sweep for tokens
// that indicate a runtime ceiling, including runtimes with no parser at all.
// It can over-count (a token may appear without configuring a real ceiling),
// so the misses it reports are candidates to verify, not proven defects. It is
// still far better than guessing, and it is the same corpus any model-based
// approach would be evaluated on.

type corpusRecord struct {
	Source    string            `json:"source"`
	Workload  string            `json:"workload"`
	Container string            `json:"container"`
	Image     string            `json:"image"`
	Command   []string          `json:"command"`
	Args      []string          `json:"args"`
	Env       map[string]string `json:"env"`
	OpaqueEnv []string          `json:"opaqueEnv"`
	MemLimit  int64             `json:"memLimitBytes"`
}

func (r corpusRecord) container() Container {
	env := r.Env
	if env == nil {
		env = map[string]string{}
	}
	return Container{
		Name: r.Container, Image: r.Image,
		Command: r.Command, Args: r.Args, Env: env,
		OpaqueEnv: r.OpaqueEnv, MemoryLimitBytes: r.MemLimit,
	}
}

// text flattens everything a ceiling could hide in.
func (r corpusRecord) text() string {
	var b strings.Builder
	b.WriteString(r.Image)
	for _, s := range append(append([]string{}, r.Command...), r.Args...) {
		b.WriteString(" " + s)
	}
	for k, v := range r.Env {
		b.WriteString(" " + k + "=" + v)
	}
	return b.String()
}

// family groups indicators by the runtime that owns them. Families with no
// parser are listed deliberately: the point is to measure the tail.
// indicators are matched against the full spec text; imageIndicators only
// against the image reference. Searching env VALUES for a bare word like
// "elasticsearch" matches every client of Elasticsearch, which flagged a
// Node.js Kibana and a Python sidecar as JVMs and made the baseline
// meaningless.
var families = []struct {
	name            string
	supported       bool
	kinds           []Kind
	indicators      []string
	imageIndicators []string
}{
	{"jvm", true, []Kind{JVMHeapMax, JVMMaxRAMPercent, JVMDefaultHeap}, []string{
		"-Xmx", "-Xms", "MaxRAMPercentage", "MaxRAMFraction", "JAVA_OPTS",
		"JAVA_TOOL_OPTIONS", "CATALINA_OPTS", "ES_JAVA_OPTS", "LS_JAVA_OPTS",
		"OPENSEARCH_JAVA_OPTS", "KAFKA_HEAP_OPTS", "SOLR_HEAP", "JVM_OPTS",
		"_JAVA_OPTIONS", "JDK_JAVA_OPTIONS", "ELASTICSEARCH_HEAP_SIZE",
		"OPENSEARCH_HEAP_SIZE", "MAX_HEAP_SIZE", "SPARK_DAEMON_JAVA_OPTS",
	}, []string{
		// Running a JVM at all is an indicator: a JVM with no heap flag still
		// has an implicit 25% ceiling worth reporting.
		"elasticsearch", "opensearch", "logstash", "cassandra", "kafka",
		"jenkins", "keycloak", "solr", "tomcat", "jdk", "jre", "spark", "flink",
	}},
	{"go", true, []Kind{GoMemLimit, GoMaxProcs}, []string{"GOMEMLIMIT", "GOMAXPROCS"}, nil},
	{"node", true, []Kind{NodeOldSpace}, []string{"--max-old-space-size", "NODE_OPTIONS"}, nil},
	{"python", true, []Kind{PythonWorkers}, []string{"gunicorn", "uvicorn", "hypercorn", "WEB_CONCURRENCY", "celery"}, nil},

	{"dotnet", false, nil, []string{"DOTNET_GCHeapHardLimit", "DOTNET_gcServer", "ASPNETCORE_"}, nil},
	{"ruby", false, nil, []string{"RUBY_GC_", "UNICORN_WORKERS", "PUMA_", "RAILS_MAX_THREADS"}, nil},
	{"php", false, nil, []string{"pm.max_children", "PHP_MEMORY_LIMIT", "memory_limit"}, nil},
	{"erlang", false, nil, []string{"RABBITMQ_VM_MEMORY", "ERL_MAX", "+MHas", "ERL_MAX_PORTS"}, nil},
	{"redis", false, nil, []string{"maxmemory"}, nil},
	{"postgres", false, nil, []string{"shared_buffers", "effective_cache_size"}, nil},
	{"mysql", false, nil, []string{"innodb_buffer_pool_size", "key_buffer_size"}, nil},
	{"envoy", false, nil, []string{"--concurrency"}, nil},
	{"spark-flink", false, nil, []string{"SPARK_DAEMON_MEMORY", "SPARK_WORKER_MEMORY", "jobmanager.memory", "taskmanager.memory", "FLINK_"}, nil},
}

func loadCorpus(t *testing.T) []corpusRecord {
	t.Helper()
	f, err := os.Open("../../test/corpus/specs.jsonl")
	if err != nil {
		t.Skipf("corpus not present: %v", err)
	}
	defer f.Close()

	var out []corpusRecord
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var r corpusRecord
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatalf("bad corpus line: %v", err)
		}
		out = append(out, r)
	}
	return out
}

// TestCorpusBaseline is the measurement, not a pass/fail gate on quality. It
// prints coverage per runtime family and fails only if the supported families
// regress, so the number to beat is recorded in the repository rather than in
// somebody's memory.
func TestCorpusBaseline(t *testing.T) {
	corpus := loadCorpus(t)

	type stat struct{ withIndicator, detected int }
	stats := map[string]*stat{}
	for _, f := range families {
		stats[f.name] = &stat{}
	}

	var anyIndicator, anyDetected, cleanFalsePositive int

	for _, r := range corpus {
		txt := r.text()
		found := Detect(r.container())
		hasIndicator := false

		for _, f := range families {
			hit := false
			for _, ind := range f.indicators {
				if strings.Contains(txt, ind) {
					hit = true
					break
				}
			}
			for _, ind := range f.imageIndicators {
				if strings.Contains(strings.ToLower(r.Image), ind) {
					hit = true
					break
				}
			}
			if !hit {
				continue
			}
			hasIndicator = true
			stats[f.name].withIndicator++
			// Credit the family only for a ceiling of its own kind. Counting
			// any detection would let a JVM finding paper over a missing
			// Spark or Flink parser.
			for _, k := range f.kinds {
				if hasKind(found, k) {
					stats[f.name].detected++
					break
				}
			}
		}
		if hasIndicator {
			anyIndicator++
			if len(found) > 0 {
				anyDetected++
			}
		} else if len(found) > 0 {
			// A detection with no indicator anywhere: either the sweep is
			// incomplete or the parser fired on nothing.
			cleanFalsePositive++
			t.Logf("detection with no indicator: %s (%s) -> %v", r.Container, r.Image, kinds(found))
		}
	}

	names := make([]string, 0, len(stats))
	for n := range stats {
		names = append(names, n)
	}
	sort.Strings(names)

	shell := 0
	for _, r := range corpus {
		argv := append(append([]string{}, r.Command...), r.Args...)
		if len(argv) > 0 {
			base := argv[0]
			if i := strings.LastIndex(base, "/"); i >= 0 {
				base = base[i+1:]
			}
			if base == "sh" || base == "bash" {
				shell++
			}
		}
	}
	t.Logf("corpus: %d unique container specs from public Helm charts", len(corpus))
	t.Logf("%d of them (%s) have a shell entrypoint, so their real command is inside a string",
		shell, fmtPct(shell, len(corpus)))
	t.Logf("%-14s %-10s %9s %9s %9s", "FAMILY", "PARSER", "INDICATED", "DETECTED", "MISSED")
	for _, f := range families {
		s := stats[f.name]
		if s.withIndicator == 0 {
			continue
		}
		parser := "none"
		if f.supported {
			parser = "yes"
		}
		t.Logf("%-14s %-10s %9d %9d %9d", f.name, parser, s.withIndicator, s.detected, s.withIndicator-s.detected)
	}
	t.Logf("overall: %d/%d specs with any indicator detected (%.0f%%)",
		anyDetected, anyIndicator, 100*float64(anyDetected)/float64(max(anyIndicator, 1)))

	// Recorded floors, not perfection. The gap between these and the indicated
	// counts was inspected by hand on 2026-09-14 and is entirely correct
	// rejections: bash and sh init containers that reuse a JVM base image to
	// run a shell one-liner, plus one container whose command lives in the
	// image entrypoint and is therefore invisible to any spec parser.
	//
	// Raising a floor when detection improves is the point. Lowering one
	// requires re-inspecting the misses and saying why.
	floors := map[string]int{
		"jvm":    25, // of 32 indicated; the other 7 are verified non-JVMs
		"node":   1,
		"python": 1, // of 3; 1 non-Python init container, 1 image entrypoint
	}
	for name, floor := range floors {
		if got := stats[name].detected; got < floor {
			t.Errorf("%s: detected %d, floor is %d. Detection regressed; "+
				"if this is intentional, re-inspect the misses and move the floor.",
				name, got, floor)
		}
	}
}

func hasKind(cs []Ceiling, k Kind) bool {
	for _, c := range cs {
		if c.Kind == k {
			return true
		}
	}
	return false
}

func kinds(cs []Ceiling) []Kind {
	var out []Kind
	for _, c := range cs {
		out = append(out, c.Kind)
	}
	return out
}

func fmtPct(a, b int) string { return fmt.Sprintf("%.0f%%", 100*float64(a)/float64(max(b, 1))) }
