package analyze

import (
	"fmt"
	"strings"

	"github.com/mfranzon/tare/internal/ceilings"
	"github.com/mfranzon/tare/internal/metrics"
)

// A large share of real ceilings are not in the pod spec at all: they live in
// image entrypoints, ConfigMaps and envFrom. The corpus in test/corpus puts
// that at roughly a quarter of JVM workloads. No parser reaches them, and
// neither would a perfect language model reading the same input, because the
// information is absent rather than obscure.
//
// Behaviour, however, is not absent. A runtime holding a fixed heap plateaus at
// that heap and stays there. This file infers a ceiling from that shape when
// the spec is silent.
//
// Inference is always weaker evidence than a parsed flag, and it is treated
// that way: it can raise a recommendation, never lower one, and it is reported
// as inferred so a human can check it.

// ObservedCeiling is the kind emitted for a behaviourally inferred ceiling.
const ObservedCeiling ceilings.Kind = "observed-plateau"

// managedRuntimes are images whose runtime holds a heap that may be capped
// independently of the container limit. Detection is deliberately conservative:
// inferring a ceiling for a process that simply uses little memory would pin
// its recommendation to today's usage forever.
var managedRuntimes = []string{
	"jre", "jdk", "openjdk", "temurin", "corretto", "liberica", "zulu", "graalvm",
	"tomcat", "jetty", "wildfly", "jboss", "quarkus",
	"kafka", "cassandra", "elasticsearch", "opensearch", "logstash",
	"spark", "flink", "jenkins", "keycloak", "solr", "zookeeper",
	"sonarqube", "nexus", "artifactory", "nifi", "druid", "trino",
	"node", "dotnet", "aspnet",
}

func looksManagedRuntime(image string) bool {
	img := strings.ToLower(image)
	for _, h := range managedRuntimes {
		if strings.Contains(img, h) {
			return true
		}
	}
	return false
}

// inferCeiling returns a behaviourally inferred memory ceiling, or nil.
//
// It fires only when every one of these holds:
//   - the spec yielded no resolved memory ceiling (a parsed flag always wins)
//   - the image is a runtime that can hold a capped heap
//   - memory sits at a plateau rather than merely peaking there
//   - the plateau is well below the limit, so there is headroom to explain
//   - there is enough evidence to say anything at all
func inferCeiling(f *Finding, th Thresholds) *ceilings.Ceiling {
	if !looksManagedRuntime(f.Image) {
		return nil
	}
	for _, c := range f.Ceilings {
		if c.Resource != ceilings.Memory || c.Unresolved {
			continue
		}
		// A ceiling actually read from a flag settles the question. A ceiling
		// that merely scales with the limit is an assumption about the
		// runtime's default, and behaviour can contradict it: a fixed -Xmx
		// hidden in an entrypoint looks exactly like a default from the spec,
		// right up until the resize OOMKills the container.
		if !c.ScalesWithLimit {
			return nil
		}
	}
	if f.MemUsageMax <= 0 || f.Samples < th.MinSamplesForInference {
		return nil
	}
	// Without the percentile there is no way to tell a plateau from a peak.
	if !f.hasSignal(metrics.SigMemP95) {
		return nil
	}
	// A plateau: the 95th percentile sits close to the maximum, meaning the
	// container lives at that level rather than touching it occasionally.
	if f.MemP95 < f.MemUsageMax*th.PlateauRatio {
		return nil
	}
	// Without headroom there is nothing to explain: a container at its limit is
	// constrained by the limit, not by an internal ceiling.
	if f.MemLimit > 0 && f.MemUsageMax > float64(f.MemLimit)*th.PlateauHeadroom {
		return nil
	}

	plateau := int64(f.MemUsageMax)
	overhead := ceilings.JVMOverhead(plateau)

	c := &ceilings.Ceiling{
		Kind: ObservedCeiling, Resource: ceilings.Memory, Source: "observed",
		Bytes:             plateau,
		MinContainerBytes: plateau + overhead,
	}

	// The strong case: the limit moved during the window and the plateau did
	// not follow. That is a fixed cap, demonstrated rather than guessed.
	if f.hasSignal(metrics.SigLimitTrend) && f.MemLimitEarly > 0 && f.MemLimitLate > 0 &&
		relDiff(f.MemLimitEarly, f.MemLimitLate) > th.LimitMoveRatio {
		c.Note = fmt.Sprintf(
			"memory limit changed from %s to %s during the window and the working set stayed flat at %s. The runtime holds a fixed cap, so shrinking the limit will not shrink the application; %s is a floor, not a target.",
			HumanBytes(int64(f.MemLimitEarly)), HumanBytes(int64(f.MemLimitLate)),
			HumanBytes(plateau), HumanBytes(c.MinContainerBytes))
		return c
	}

	// The weaker case: a plateau with headroom, and a runtime known to cap its
	// own heap. Enough to refuse to size below it, not enough to state it as
	// fact.
	c.Note = fmt.Sprintf(
		"no heap ceiling is visible in the pod spec, but memory sits flat at %s under a %s limit. For this runtime that shape usually means a heap cap set in the image entrypoint or a ConfigMap. Treating %s as a floor; confirm the flag before reducing further.",
		HumanBytes(plateau), HumanBytes(f.MemLimit), HumanBytes(c.MinContainerBytes))
	return c
}

func relDiff(a, b float64) float64 {
	if a == 0 || b == 0 {
		return 0
	}
	if a > b {
		return (a - b) / a
	}
	return (b - a) / b
}
