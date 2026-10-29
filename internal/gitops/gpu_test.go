package gitops

import (
	"os"
	"strings"
	"testing"

	"github.com/2listic/tare/internal/analyze"
)

// A GPU count is reported, never written. A workload holding two devices and
// using one gets GPU_DEVICE_UNUSED, and the manifest tare edits for its memory
// must still ask for two: nvidia.com/gpu is an integer with no smaller size,
// and freeing a device is a change to the program, not to a number.
func TestDiffNeverTouchesTheGPUCount(t *testing.T) {
	const manifest = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: api
  namespace: prod
spec:
  template:
    spec:
      containers:
        - name: app
          image: trainer:1
          resources:
            requests:
              cpu: 2
              memory: 4Gi
            limits:
              memory: 8Gi
              nvidia.com/gpu: 2
`
	dir := t.TempDir()
	p := write(t, dir, "api.yaml", manifest)
	changes, _ := Plan([]analyze.Finding{{
		Namespace: "prod", Workload: "Deployment/api", Container: "app", Verdict: analyze.Waste,
		CPURequest: 2000, MemRequest: 4 << 30, MemLimit: 8 << 30,
		RecCPU: 2000, RecMem: 1 << 30, RecMemLimit: 2 << 30,
		GPUs: 2, GPUResource: "nvidia.com/gpu", GPUIdleDevices: 1,
		Reasons: []analyze.Reason{{Code: analyze.GPUDeviceUnused}},
	}})
	if len(changes) != 1 {
		t.Fatalf("want one change for the memory, got %d", len(changes))
	}
	if _, _, err := ApplyTree(dir, changes, false); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	if !strings.Contains(string(got), "memory: 1Gi") {
		t.Fatalf("the memory change was not written, so this test proves nothing:\n%s", got)
	}
	if !strings.Contains(string(got), "nvidia.com/gpu: 2\n") {
		t.Errorf("the GPU count was edited:\n%s", got)
	}
}
