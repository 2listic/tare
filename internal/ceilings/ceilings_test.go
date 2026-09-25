package ceilings

import (
	"strings"
	"testing"
)

const (
	mi = 1 << 20
	gi = 1 << 30
)

// find returns the first ceiling of the given kind, or nil.
func find(cs []Ceiling, k Kind) *Ceiling {
	for i := range cs {
		if cs[i].Kind == k {
			return &cs[i]
		}
	}
	return nil
}

func TestDetectMemoryCeilings(t *testing.T) {
	tests := []struct {
		name       string
		container  Container
		wantKind   Kind
		wantBytes  int64
		unresolved bool
	}{
		{
			name: "tomcat lab workload, JAVA_OPTS Xmx under a 1Gi limit",
			container: Container{
				Image:            "tomcat:10-jre21",
				Env:              map[string]string{"JAVA_OPTS": "-Xmx256m -XX:MaxRAMPercentage=25.0"},
				MemoryLimitBytes: 1 * gi,
			},
			wantKind:  JVMHeapMax,
			wantBytes: 256 * mi,
		},
		{
			name:      "Xmx on the command line, gigabyte suffix",
			container: Container{Image: "eclipse-temurin:21-jre", Args: []string{"java", "-Xmx4g", "-jar", "app.jar"}},
			wantKind:  JVMHeapMax,
			wantBytes: 4 * gi,
		},
		{
			name:      "Xmx uppercase G",
			container: Container{Image: "openjdk:17", Command: []string{"java", "-Xmx2G"}},
			wantKind:  JVMHeapMax,
			wantBytes: 2 * gi,
		},
		{
			name:      "Xmx in kilobytes",
			container: Container{Image: "openjdk:17", Command: []string{"java", "-Xmx1048576k"}},
			wantKind:  JVMHeapMax,
			wantBytes: 1 * gi,
		},
		{
			name:      "Xmx as a bare byte count",
			container: Container{Image: "openjdk:17", Command: []string{"java", "-Xmx536870912"}},
			wantKind:  JVMHeapMax,
			wantBytes: 512 * mi,
		},
		{
			name: "JAVA_TOOL_OPTIONS rather than JAVA_OPTS",
			container: Container{
				Image: "gcr.io/distroless/java17",
				Env:   map[string]string{"JAVA_TOOL_OPTIONS": "-XX:+UseG1GC -Xmx768m"},
			},
			wantKind:  JVMHeapMax,
			wantBytes: 768 * mi,
		},
		{
			name: "MaxRAMPercentage resolves against the memory limit",
			container: Container{
				Image:            "eclipse-temurin:21-jre",
				Args:             []string{"java", "-XX:MaxRAMPercentage=75.0", "-jar", "app.jar"},
				MemoryLimitBytes: 2 * gi,
			},
			wantKind:  JVMMaxRAMPercent,
			wantBytes: 1536 * mi,
		},
		{
			name: "MaxRAMPercentage without a memory limit is unresolved",
			container: Container{
				Image: "eclipse-temurin:21-jre",
				Args:  []string{"java", "-XX:MaxRAMPercentage=75.0"},
			},
			wantKind:   JVMMaxRAMPercent,
			unresolved: true,
		},
		{
			name: "deprecated MaxRAMFraction",
			container: Container{
				Image:            "openjdk:11",
				Args:             []string{"java", "-XX:MaxRAMFraction=4"},
				MemoryLimitBytes: 2 * gi,
			},
			wantKind:  JVMMaxRAMPercent,
			wantBytes: 512 * mi,
		},
		{
			name: "JVM with no flags falls back to the 25 percent container default",
			container: Container{
				Image:            "eclipse-temurin:21-jre",
				MemoryLimitBytes: 4 * gi,
			},
			wantKind:  JVMDefaultHeap,
			wantBytes: 1 * gi,
		},
		{
			name:       "JVM options hidden in a ConfigMap are unresolved, not absent",
			container:  Container{Image: "tomcat:10-jre21", OpaqueEnv: []string{"JAVA_OPTS"}, MemoryLimitBytes: 1 * gi},
			wantKind:   JVMHeapMax,
			unresolved: true,
		},
		{
			name:      "GOMEMLIMIT with binary units",
			container: Container{Image: "myapp:1.0", Env: map[string]string{"GOMEMLIMIT": "512MiB"}},
			wantKind:  GoMemLimit,
			wantBytes: 512 * mi,
		},
		{
			name:      "GOMEMLIMIT as a bare byte count",
			container: Container{Image: "myapp:1.0", Env: map[string]string{"GOMEMLIMIT": "1073741824"}},
			wantKind:  GoMemLimit,
			wantBytes: 1 * gi,
		},
		{
			name:       "GOMEMLIMIT in decimal units is invalid and must not be guessed",
			container:  Container{Image: "myapp:1.0", Env: map[string]string{"GOMEMLIMIT": "512MB"}},
			wantKind:   GoMemLimit,
			unresolved: true,
		},
		{
			name:      "node max-old-space-size as a joined flag",
			container: Container{Image: "node:22-alpine", Args: []string{"node", "--max-old-space-size=512", "server.js"}},
			wantKind:  NodeOldSpace,
			wantBytes: 512 * mi,
		},
		{
			name:      "node max-old-space-size via NODE_OPTIONS",
			container: Container{Image: "node:22-alpine", Env: map[string]string{"NODE_OPTIONS": "--max-old-space-size=1024 --enable-source-maps"}},
			wantKind:  NodeOldSpace,
			wantBytes: 1 * gi,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := find(Detect(tc.container), tc.wantKind)
			if got == nil {
				t.Fatalf("no %s ceiling detected; got %+v", tc.wantKind, Detect(tc.container))
			}
			if got.Unresolved != tc.unresolved {
				t.Fatalf("Unresolved = %v, want %v (note: %s)", got.Unresolved, tc.unresolved, got.Note)
			}
			if !tc.unresolved && got.Bytes != tc.wantBytes {
				t.Errorf("Bytes = %d (%s), want %d (%s)", got.Bytes, humanBytes(got.Bytes), tc.wantBytes, humanBytes(tc.wantBytes))
			}
			if got.Note == "" {
				t.Error("Note is empty; every finding must be explainable to an operator")
			}
		})
	}
}

func TestDetectCPUCeilings(t *testing.T) {
	tests := []struct {
		name           string
		container      Container
		wantKind       Kind
		wantMillicores int64
		unresolved     bool
	}{
		{
			name:           "GOMAXPROCS caps usable CPU",
			container:      Container{Image: "myapp:1.0", Env: map[string]string{"GOMAXPROCS": "2"}},
			wantKind:       GoMaxProcs,
			wantMillicores: 2000,
		},
		{
			name:           "gunicorn worker count",
			container:      Container{Image: "python:3.12", Command: []string{"gunicorn", "--workers", "4", "app:app"}},
			wantKind:       PythonWorkers,
			wantMillicores: 4000,
		},
		{
			name:           "gunicorn short worker flag",
			container:      Container{Image: "python:3.12", Command: []string{"gunicorn", "-w", "8", "app:app"}},
			wantKind:       PythonWorkers,
			wantMillicores: 8000,
		},
		{
			name:           "uvicorn workers via WEB_CONCURRENCY",
			container:      Container{Image: "python:3.12", Command: []string{"uvicorn", "app:app"}, Env: map[string]string{"WEB_CONCURRENCY": "3"}},
			wantKind:       PythonWorkers,
			wantMillicores: 3000,
		},
		{
			name:       "gunicorn with no worker count is unresolved",
			container:  Container{Image: "python:3.12", Command: []string{"gunicorn", "app:app"}},
			wantKind:   PythonWorkers,
			unresolved: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := find(Detect(tc.container), tc.wantKind)
			if got == nil {
				t.Fatalf("no %s ceiling detected", tc.wantKind)
			}
			if got.Unresolved != tc.unresolved {
				t.Fatalf("Unresolved = %v, want %v", got.Unresolved, tc.unresolved)
			}
			if !tc.unresolved && got.Millicores != tc.wantMillicores {
				t.Errorf("Millicores = %d, want %d", got.Millicores, tc.wantMillicores)
			}
		})
	}
}

func TestNoFalsePositives(t *testing.T) {
	for _, c := range []Container{
		{Name: "nginx", Image: "nginx:1.27-alpine", MemoryLimitBytes: 2 * gi},
		{Name: "hog", Image: "busybox:1.36", Command: []string{"sh", "-c", "while :; do :; done"}},
		{Name: "redis", Image: "redis:7-alpine", MemoryLimitBytes: 1 * gi},
	} {
		if got := Detect(c); len(got) != 0 {
			t.Errorf("%s: expected no ceilings, got %+v", c.Image, got)
		}
	}
}

func TestMemoryFloorBlocksOnUnresolved(t *testing.T) {
	// A resolved ceiling sets the floor.
	floor, blocked := MemoryFloor(Detect(Container{
		Image: "tomcat:10-jre21",
		Env:   map[string]string{"JAVA_OPTS": "-Xmx256m"},
	}))
	if blocked {
		t.Error("resolved ceiling should not block")
	}
	// 256Mi heap plus the 128Mi minimum non-heap allowance.
	if floor != 384*mi {
		t.Errorf("floor = %s, want 384Mi (heap plus non-heap overhead)", humanBytes(floor))
	}

	// An unresolved ceiling must block the recommendation entirely.
	_, blocked = MemoryFloor(Detect(Container{
		Image:     "tomcat:10-jre21",
		OpaqueEnv: []string{"JAVA_OPTS"},
	}))
	if !blocked {
		t.Error("an unreadable JAVA_OPTS must block recommendation, not be treated as absent")
	}
}

func TestCPUCeilingTakesTheSmallest(t *testing.T) {
	cs := Detect(Container{
		Image:   "python:3.12",
		Command: []string{"gunicorn", "-w", "8", "app:app"},
		Env:     map[string]string{"GOMAXPROCS": "2"},
	})
	got, ok := CPUCeiling(cs)
	if !ok {
		t.Fatal("expected a CPU ceiling")
	}
	if got != 2000 {
		t.Errorf("CPUCeiling = %d, want 2000 (the tighter of the two)", got)
	}
}

func TestExplicitXmxOverridesPercentage(t *testing.T) {
	// The lab's tomcat workload sets both. An explicit -Xmx wins, so the
	// percentage flag must not claim the heap follows the memory limit.
	cs := Detect(Container{
		Image:            "tomcat:10-jre21",
		Env:              map[string]string{"JAVA_OPTS": "-Xmx256m -XX:MaxRAMPercentage=25.0"},
		MemoryLimitBytes: 1 * gi,
	})

	pct := find(cs, JVMMaxRAMPercent)
	if pct == nil {
		t.Fatal("expected the percentage flag to still be reported")
	}
	if pct.Bytes != 0 {
		t.Errorf("overridden percentage ceiling should carry no resolved size, got %s", humanBytes(pct.Bytes))
	}
	if !strings.Contains(pct.Note, "overridden") {
		t.Errorf("note must say the flag is overridden, got: %s", pct.Note)
	}

	// The floor still comes from -Xmx.
	floor, blocked := MemoryFloor(cs)
	if blocked || floor != 384*mi {
		t.Errorf("floor = %s blocked = %v, want 384Mi false", humanBytes(floor), blocked)
	}
}

// TestFloorAlwaysExceedsHeap guards the invariant that broke the first
// end-to-end run: a container sized to exactly its heap will be OOMKilled.
func TestFloorAlwaysExceedsHeap(t *testing.T) {
	for _, xmx := range []string{"-Xmx64m", "-Xmx256m", "-Xmx1g", "-Xmx8g"} {
		cs := Detect(Container{Image: "eclipse-temurin:21-jre", Args: []string{"java", xmx}})
		heap := find(cs, JVMHeapMax)
		if heap == nil {
			t.Fatalf("%s: no ceiling detected", xmx)
		}
		if heap.MinContainerBytes <= heap.Bytes {
			t.Errorf("%s: MinContainerBytes %s must exceed heap %s",
				xmx, humanBytes(heap.MinContainerBytes), humanBytes(heap.Bytes))
		}
		floor, _ := MemoryFloor(cs)
		if floor != heap.MinContainerBytes {
			t.Errorf("%s: floor %s should be the container minimum %s, not the heap",
				xmx, humanBytes(floor), humanBytes(heap.MinContainerBytes))
		}
	}
}
