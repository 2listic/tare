# Ceiling detection corpus

135 unique container specs extracted from 39 public Helm charts (Bitnami OCI,
Elastic, prometheus-community, Jenkins, SonarQube, Apache Airflow, codecentric,
Nextcloud), covering 51 distinct images.

This corpus is the prerequisite for every path to better ceiling detection:

- it is the **test suite** for the hand-written parsers in `internal/ceilings`
- it is the **eval set** for any prompt-based or finetuned extraction
- it is the **training data** for a finetune, should one ever be justified

Without it, "a model would do this better" is an assertion rather than a
measurement.

## Rebuilding

    helm template ... > charts.yaml      # see the header of mkcorpus
    go run ./tools/mkcorpus helm-public < charts.yaml > test/corpus/specs.jsonl

Records are deduplicated by (image, command, args, env): charts share sidecars,
and counting the same node-exporter twenty times would flatter any number.

## Ground truth is a proxy, and that matters

There are no hand-written labels. `TestCorpusBaseline` sweeps for tokens that
indicate a runtime ceiling, including runtimes with no parser at all, and
compares that against what the parsers detect. The sweep over-counts, so its
"misses" are candidates to verify rather than proven defects.

Two rules keep it honest, both learned by getting them wrong first:

1. A family is credited only for a ceiling **of its own kind**. Counting any
   detection let a JVM finding paper over the absent Spark and Flink parsers.
2. Image hints match the **image reference only**, never env values. Searching
   env values for the bare word "elasticsearch" matched every client of
   Elasticsearch, and flagged a Node.js Kibana and a Python sidecar as JVMs.

## Baseline, 2026-09-14

| family | parser | indicated | detected | missed |
| --- | --- | --- | --- | --- |
| jvm | yes | 32 | 25 | 7 |
| node | yes | 1 | 1 | 0 |
| python | yes | 3 | 1 | 2 |
| spark / flink | none | 4 | 0 | 4 |

**Overall 27/36 (75%)**, up from 56% before the corpus existed.

Every miss was inspected by hand. The 7 JVM and 2 Python misses are **correct
rejections**: bash and sh init containers reusing an application base image, and
one container whose command lives in the image entrypoint. Spark and Flink are a
genuine gap with no parser.

## What building the corpus found

Three real parser defects, none of which the live cluster or the simulator
surfaced:

1. **A third of containers (46 of 135) have a shell entrypoint**, so their real
   command sits inside a string. Anything scanning only argv elements misses
   them. `expandArgv` now splits the script of a `sh -c` invocation.
2. **A JVM base image does not mean a JVM runs.** `sysctl -w` on an
   elasticsearch image was being told about heap sizing. Detection now requires
   evidence: a `java` token in the expanded command, or a JVM environment
   variable. An unreadable entrypoint yields an explicit unresolved finding.
3. **Application-specific heap variables were unknown.** `ES_JAVA_OPTS`,
   `LS_JAVA_OPTS`, `KAFKA_HEAP_OPTS`, `SPARK_DAEMON_JAVA_OPTS`, and a second
   shape entirely: `ELASTICSEARCH_HEAP_SIZE=1024m`, `MAX_HEAP_SIZE=1G`, bare
   sizes rather than option strings. Also missing from the image hints:
   jenkins, keycloak, solr, opensearch, zookeeper.

## The finding that matters most for the roadmap

**A large share of real ceilings are not in the pod spec at all.** They live in
image entrypoints, ConfigMaps and `envFrom`. No parser reaches them, and neither
would a perfect language model reading the same input, because the information
is absent rather than obscure.

That is an argument for an **observational fallback**: infer a ceiling from
behaviour. A JVM whose working set plateaus flat at 256Mi under a 1Gi limit is
telling you its heap ceiling directly, without any spec to read. That mechanism
is also the correct cross-check on any inferred ceiling, model-produced or not.
