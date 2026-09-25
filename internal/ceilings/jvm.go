package ceilings

import (
	"fmt"
	"strconv"
	"strings"
)

// jvmOptsEnv lists environment variables the JVM and common application servers
// read options from. The JVM applies JAVA_TOOL_OPTIONS first, then the command
// line, then _JAVA_OPTIONS last, so _JAVA_OPTIONS wins where they conflict.
// Detection reports every occurrence and lets the caller see the conflict
// rather than silently picking one.
var jvmOptsEnv = []string{
	"JAVA_TOOL_OPTIONS",
	"JDK_JAVA_OPTIONS",
	"JAVA_OPTS",
	"JAVA_OPTS_APPEND",
	"CATALINA_OPTS",
	"JVM_OPTS",
	"JDK_JAVA_OPTS",
	"_JAVA_OPTIONS",
	// Application-specific options variables. Every one of these was found in
	// a real public Helm chart; a parser that knows only JAVA_OPTS misses most
	// of the JVM workloads people actually run.
	"ES_JAVA_OPTS",
	"LS_JAVA_OPTS",
	"OPENSEARCH_JAVA_OPTS",
	"KAFKA_HEAP_OPTS",
	"KAFKA_JVM_PERFORMANCE_OPTS",
	"SPARK_DAEMON_JAVA_OPTS",
	"SPARK_JAVA_OPTS",
	"KEYCLOAK_JAVA_OPTS",
	"JAVA_OPTIONS",
	"ZOOKEEPER_HEAP_OPTS",
	"SOLR_JAVA_MEM",
}

// jvmHeapSizeEnv hold a bare size rather than an options string: the value is
// "1024m", not "-Xmx1024m". Same ceiling, different spelling.
var jvmHeapSizeEnv = []string{
	"ELASTICSEARCH_HEAP_SIZE",
	"OPENSEARCH_HEAP_SIZE",
	"SOLR_HEAP",
	"MAX_HEAP_SIZE",
	"KAFKA_HEAP_SIZE",
	"ZOOKEEPER_HEAP_SIZE",
	"CASSANDRA_MAX_HEAP_SIZE",
}

// jvmImageHints are substrings that identify a JVM base image.
var jvmImageHints = []string{
	// Base images.
	"jre", "jdk", "openjdk", "temurin", "corretto", "liberica", "zulu", "graalvm",
	// Application servers.
	"tomcat", "jetty", "wildfly", "jboss", "quarkus",
	// JVM applications. Each of these ships a JVM without saying so in the
	// image name, and every one was found in a public Helm chart.
	"kafka", "cassandra", "elasticsearch", "opensearch", "logstash",
	"spark", "flink", "jenkins", "keycloak", "solr", "zookeeper",
	"nifi", "activemq", "artemis", "druid", "trino", "presto",
	"hadoop", "hbase", "storm", "sonarqube", "nexus", "artifactory",
}

type token struct {
	text   string
	source string
}

// shells whose -c argument is a script worth looking inside.
var shells = map[string]bool{"sh": true, "bash": true, "ash": true, "dash": true, "zsh": true}

// expandArgv flattens a command line, splitting the script of a shell -c
// invocation into its own tokens. A third of the containers in the corpus have
// a shell entrypoint, which means their real command, and any flag on it, is
// invisible to anything that only looks at argv elements.
func expandArgv(argv []string) []string {
	out := append([]string{}, argv...)
	if len(argv) == 0 {
		return out
	}
	base := argv[0]
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	if !shells[base] {
		return out
	}
	for i := 1; i < len(argv); i++ {
		// -c, -ec, -euc and friends all introduce the script.
		if strings.HasPrefix(argv[i], "-") && strings.Contains(argv[i], "c") && i+1 < len(argv) {
			out = append(out, strings.Fields(argv[i+1])...)
			break
		}
	}
	return out
}

// runsJava reports whether the container actually executes a JVM, as opposed
// to merely being built on a JVM base image. An init container running
// "sysctl -w" on an elasticsearch image is not a JVM, and telling its owner
// about heap sizing is noise at best.
//
// The third return value is false when the entrypoint is an opaque script that
// cannot be read, where the only honest answer is that we do not know.
func runsJava(c Container) (isJava, known bool) {
	argv := expandArgv(c.argv())
	if len(argv) == 0 {
		// The image entrypoint runs, and the image says JVM.
		return true, true
	}
	for _, a := range argv {
		base := a
		if i := strings.LastIndex(a, "/"); i >= 0 {
			base = a[i+1:]
		}
		if base == "java" {
			return true, true
		}
	}
	first := argv[0]
	if i := strings.LastIndex(first, "/"); i >= 0 {
		first = first[i+1:]
	}
	if shells[first] {
		// We read the script and found no java in it.
		return false, true
	}
	// Some other binary or script: unreadable from the spec.
	return false, false
}

// jvmTokens flattens argv and every JVM options environment variable into
// individually sourced tokens.
func jvmTokens(c Container) []token {
	var ts []token
	for _, a := range expandArgv(c.argv()) {
		ts = append(ts, token{a, "args"})
	}
	for _, k := range jvmOptsEnv {
		v, ok := c.Env[k]
		if !ok {
			continue
		}
		for _, f := range strings.Fields(v) {
			ts = append(ts, token{f, "env:" + k})
		}
	}
	return ts
}

func hasJVMEnv(c Container) bool {
	for _, k := range append(append([]string{}, jvmOptsEnv...), jvmHeapSizeEnv...) {
		if _, ok := c.Env[k]; ok {
			return true
		}
		if c.opaque(k) {
			return true
		}
	}
	return false
}

func imageLooksJVM(c Container) bool {
	img := strings.ToLower(c.Image)
	for _, h := range jvmImageHints {
		if strings.Contains(img, h) {
			return true
		}
	}
	return false
}

func detectJVM(c Container) []Ceiling {
	envEvidence := hasJVMEnv(c)
	if !envEvidence && !imageLooksJVM(c) {
		return nil
	}

	// A JVM base image proves nothing on its own: init containers routinely
	// reuse the application image to run a shell one-liner.
	if !envEvidence {
		isJava, known := runsJava(c)
		switch {
		case known && !isJava:
			return nil // definitely not running a JVM
		case !known:
			return []Ceiling{{
				Kind: JVMDefaultHeap, Resource: Memory, Source: "image",
				Unresolved: true,
				Note:       fmt.Sprintf("%s is a JVM image but the entrypoint is a script this tool cannot read, so whether a JVM runs, and with what heap, is unknown", c.Image),
			}}
		}
	}

	var out []Ceiling

	for _, t := range jvmTokens(c) {
		switch {
		case strings.HasPrefix(t.text, "-Xmx"):
			out = append(out, jvmHeapCeiling(t))
		case strings.HasPrefix(t.text, "-XX:MaxRAMPercentage="):
			out = append(out, jvmPercentCeiling(c, t, "-XX:MaxRAMPercentage="))
		case strings.HasPrefix(t.text, "-XX:MaxRAMFraction="):
			out = append(out, jvmFractionCeiling(c, t))
		}
	}

	for _, k := range jvmHeapSizeEnv {
		v, ok := c.Env[k]
		if !ok {
			continue
		}
		out = append(out, jvmHeapCeiling(token{"-Xmx" + v, "env:" + k}))
	}

	// A ceiling can hide in an env var sourced from a ConfigMap or Secret. The
	// value is not in the pod spec, so absence of a flag proves nothing.
	for _, k := range jvmOptsEnv {
		if !c.opaque(k) {
			continue
		}
		out = append(out, Ceiling{
			Kind: JVMHeapMax, Resource: Memory, Source: "env:" + k,
			Unresolved: true,
			Note:       fmt.Sprintf("%s is set from a ConfigMap, Secret or the downward API, so its value is not visible in the pod spec and may contain a heap ceiling", k),
		})
	}

	if len(out) == 0 {
		out = append(out, jvmDefaultHeap(c))
	}
	return applyJVMPrecedence(out)
}

// applyJVMPrecedence corrects notes when several heap knobs are set at once.
// An explicit -Xmx overrides MaxRAMPercentage and MaxRAMFraction, so a
// container carrying both does NOT respond to a memory resize even though the
// percentage flag alone would suggest it does. Reporting that wrongly is
// exactly the kind of confident-but-false finding this tool must not produce.
func applyJVMPrecedence(cs []Ceiling) []Ceiling {
	var xmx *Ceiling
	for i := range cs {
		if cs[i].Kind == JVMHeapMax && !cs[i].Unresolved {
			xmx = &cs[i]
			break
		}
	}
	if xmx == nil {
		return cs
	}
	for i := range cs {
		if cs[i].Kind != JVMMaxRAMPercent {
			continue
		}
		cs[i].Bytes = 0
		cs[i].MinContainerBytes = 0
		cs[i].Note = fmt.Sprintf("%s is overridden by the explicit %s, which takes precedence. The heap is fixed at %s and does not follow the container memory limit.", cs[i].Raw, xmx.Raw, humanBytes(xmx.Bytes))
	}
	return cs
}

func jvmHeapCeiling(t token) Ceiling {
	raw := t.text
	if strings.HasPrefix(t.source, "env:") && strings.HasPrefix(raw, "-Xmx") {
		// Reconstructed from a bare size variable; report it as the operator
		// wrote it rather than as a flag they never typed.
		raw = strings.TrimPrefix(t.source, "env:") + "=" + strings.TrimPrefix(raw, "-Xmx")
	}
	ce := Ceiling{Kind: JVMHeapMax, Resource: Memory, Source: t.source, Raw: raw}
	b, err := parseJVMSize(strings.TrimPrefix(t.text, "-Xmx"))
	if err != nil {
		ce.Unresolved = true
		ce.Note = fmt.Sprintf("found %s but could not parse its size", raw)
		return ce
	}
	ce.Bytes = b
	ce.MinContainerBytes = b + jvmOverhead(b)
	ce.Note = fmt.Sprintf("JVM heap is capped at %s by %s, so the container needs about %s once metaspace, code cache and thread stacks are counted. Raising the limit above that buys nothing; lowering it below invites an OOMKill. Change the flag, not the limit.", humanBytes(b), raw, humanBytes(ce.MinContainerBytes))
	return ce
}

func jvmPercentCeiling(c Container, t token, prefix string) Ceiling {
	ce := Ceiling{Kind: JVMMaxRAMPercent, Resource: Memory, Source: t.source, Raw: t.text}
	pct, err := strconv.ParseFloat(strings.TrimPrefix(t.text, prefix), 64)
	if err != nil || pct <= 0 || pct > 100 {
		ce.Unresolved = true
		ce.Note = fmt.Sprintf("found %s but could not parse its percentage", t.text)
		return ce
	}
	if c.MemoryLimitBytes == 0 {
		ce.Unresolved = true
		ce.Note = fmt.Sprintf("%s sets the heap to %.1f%% of the container memory limit, but no memory limit is set so the heap size cannot be determined", t.text, pct)
		return ce
	}
	ce.Bytes = int64(float64(c.MemoryLimitBytes) * pct / 100)
	ce.MinContainerBytes = ce.Bytes + jvmOverhead(ce.Bytes)
	ce.ScalesWithLimit = true
	ce.Note = fmt.Sprintf("JVM heap tracks the container memory limit at %.1f%%, currently %s. The heap moves with the limit, so this container does respond to resizing.", pct, humanBytes(ce.Bytes))
	return ce
}

func jvmFractionCeiling(c Container, t token) Ceiling {
	ce := Ceiling{Kind: JVMMaxRAMPercent, Resource: Memory, Source: t.source, Raw: t.text}
	f, err := strconv.ParseInt(strings.TrimPrefix(t.text, "-XX:MaxRAMFraction="), 10, 64)
	if err != nil || f <= 0 {
		ce.Unresolved = true
		ce.Note = fmt.Sprintf("found %s but could not parse its fraction", t.text)
		return ce
	}
	if c.MemoryLimitBytes == 0 {
		ce.Unresolved = true
		ce.Note = fmt.Sprintf("%s sets the heap to 1/%d of the container memory limit, but no memory limit is set", t.text, f)
		return ce
	}
	ce.Bytes = c.MemoryLimitBytes / f
	ce.MinContainerBytes = ce.Bytes + jvmOverhead(ce.Bytes)
	ce.ScalesWithLimit = true
	ce.Note = fmt.Sprintf("JVM heap is 1/%d of the container memory limit, currently %s. MaxRAMFraction is deprecated; MaxRAMPercentage is the supported knob.", f, humanBytes(ce.Bytes))
	return ce
}

// jvmDefaultHeap reports the implicit ceiling of a JVM with no heap flags.
// Container-aware JVMs default MaxRAMPercentage to 25%, so three quarters of
// the container memory limit is reserved for something the heap will never use.
func jvmDefaultHeap(c Container) Ceiling {
	ce := Ceiling{Kind: JVMDefaultHeap, Resource: Memory, Source: "image"}
	if c.MemoryLimitBytes == 0 {
		ce.Unresolved = true
		ce.Note = "JVM detected with no heap flags and no container memory limit, so the heap size cannot be determined"
		return ce
	}
	ce.Bytes = c.MemoryLimitBytes / 4
	ce.MinContainerBytes = ce.Bytes + jvmOverhead(ce.Bytes)
	ce.ScalesWithLimit = true
	ce.Note = fmt.Sprintf("JVM detected with no heap flags. Container-aware JVMs default to 25%% of the memory limit, about %s of heap out of %s. Setting MaxRAMPercentage explicitly usually reclaims most of the difference.", humanBytes(ce.Bytes), humanBytes(c.MemoryLimitBytes))
	return ce
}
