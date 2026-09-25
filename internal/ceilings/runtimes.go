package ceilings

import (
	"fmt"
	"strconv"
	"strings"
)

// --- Go -------------------------------------------------------------------

func detectGo(c Container) []Ceiling {
	var out []Ceiling

	if v, ok := c.Env["GOMEMLIMIT"]; ok {
		ce := Ceiling{Kind: GoMemLimit, Resource: Memory, Source: "env:GOMEMLIMIT", Raw: v}
		b, off, err := parseGoMemLimit(v)
		switch {
		case err != nil:
			ce.Unresolved = true
			ce.Note = fmt.Sprintf("GOMEMLIMIT=%q could not be parsed. Note that Go accepts only binary units (KiB, MiB, GiB), not MB or GB.", v)
		case off:
			ce.Note = "GOMEMLIMIT is explicitly off, so the Go runtime applies no soft memory limit"
		default:
			ce.Bytes = b
			// GOMEMLIMIT already accounts for most runtime memory, so the
			// container overhead on top is small compared with a JVM's.
			ce.MinContainerBytes = b + max(b/10, 32<<20)
			ce.Note = fmt.Sprintf("Go soft memory limit is %s. The runtime will GC harder rather than exceed it, so memory above this plus off-heap usage is not used.", humanBytes(b))
		}
		out = append(out, ce)
	} else if c.opaque("GOMEMLIMIT") {
		out = append(out, Ceiling{
			Kind: GoMemLimit, Resource: Memory, Source: "env:GOMEMLIMIT", Unresolved: true,
			Note: "GOMEMLIMIT is set from a ConfigMap, Secret or the downward API, so its value is not visible in the pod spec",
		})
	}

	if v, ok := c.Env["GOMAXPROCS"]; ok {
		ce := Ceiling{Kind: GoMaxProcs, Resource: CPU, Source: "env:GOMAXPROCS", Raw: v}
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil || n <= 0 {
			ce.Unresolved = true
			ce.Note = fmt.Sprintf("GOMAXPROCS=%q could not be parsed", v)
		} else {
			ce.Millicores = n * 1000
			ce.Note = fmt.Sprintf("Go runtime will use at most %d OS threads for goroutines, so CPU above %dm is not usable", n, n*1000)
		}
		out = append(out, ce)
	}
	return out
}

// --- Node -----------------------------------------------------------------

func detectNode(c Container) []Ceiling {
	argv := expandArgv(c.argv())
	nodeOpts := strings.Fields(c.Env["NODE_OPTIONS"])

	value, raw, found := flagValue(argv, "--max-old-space-size")
	source := "args"
	if !found {
		value, raw, found = flagValue(nodeOpts, "--max-old-space-size")
		source = "env:NODE_OPTIONS"
	}
	if !found {
		if c.opaque("NODE_OPTIONS") {
			return []Ceiling{{
				Kind: NodeOldSpace, Resource: Memory, Source: "env:NODE_OPTIONS", Unresolved: true,
				Note: "NODE_OPTIONS is set from a ConfigMap, Secret or the downward API and may contain a heap ceiling",
			}}
		}
		return nil
	}

	ce := Ceiling{Kind: NodeOldSpace, Resource: Memory, Source: source, Raw: raw}
	mb, err := strconv.ParseInt(value, 10, 64)
	if err != nil || mb <= 0 {
		ce.Unresolved = true
		ce.Note = fmt.Sprintf("found %s but could not parse its size", raw)
		return []Ceiling{ce}
	}
	ce.Bytes = mb * (1 << 20)
	// V8 needs new-space, native allocations and buffers beyond old-space.
	ce.MinContainerBytes = ce.Bytes + max(ce.Bytes/4, 64<<20)
	ce.Note = fmt.Sprintf("V8 old-space heap is capped at %s by %s. Memory above this plus buffers and native allocations is not used.", humanBytes(ce.Bytes), raw)
	return []Ceiling{ce}
}

// --- Python ---------------------------------------------------------------

// pythonServers are WSGI/ASGI servers whose worker count caps usable
// parallelism regardless of how much CPU the container is given.
var pythonServers = []string{"gunicorn", "uvicorn", "hypercorn", "celery"}

func detectPython(c Container) []Ceiling {
	argv := expandArgv(c.argv())

	var server string
	for _, a := range argv {
		base := a
		if i := strings.LastIndex(a, "/"); i >= 0 {
			base = a[i+1:]
		}
		for _, s := range pythonServers {
			if base == s {
				server = s
			}
		}
	}
	if server == "" {
		return nil
	}

	value, raw, found := flagValue(argv, "--workers", "-w", "--concurrency")
	if !found {
		if v, ok := c.Env["WEB_CONCURRENCY"]; ok {
			value, raw, found = v, "env:WEB_CONCURRENCY="+v, true
		}
	}
	if !found {
		return []Ceiling{{
			Kind: PythonWorkers, Resource: CPU, Source: "args", Unresolved: true,
			Note: fmt.Sprintf("%s detected with no explicit worker count, so usable parallelism cannot be determined from the pod spec", server),
		}}
	}

	ce := Ceiling{Kind: PythonWorkers, Resource: CPU, Source: "args", Raw: raw}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil || n <= 0 {
		ce.Unresolved = true
		ce.Note = fmt.Sprintf("found %s but could not parse the worker count", raw)
		return []Ceiling{ce}
	}
	ce.Millicores = n * 1000
	ce.Note = fmt.Sprintf("%s runs %d workers, so CPU above roughly %dm cannot be used no matter what the limit says", server, n, n*1000)
	return []Ceiling{ce}
}
