package discover

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// A MIG pod under the mixed strategy requests nvidia.com/mig-<profile> and no
// nvidia.com/gpu. Before this was recognised it counted as GPUs=0 and got the
// CPU-only treatment, which is the gap GPU measurement exists to close.
func TestMIGSliceIsAGPU(t *testing.T) {
	n, name := gpuCount(corev1.ResourceList{"nvidia.com/mig-1g.10gb": resource.MustParse("1")})
	if n != 1 || name != "nvidia.com/mig-1g.10gb" {
		t.Fatalf("got %d %q, want 1 nvidia.com/mig-1g.10gb", n, name)
	}
	if !IsMIGResource(name) {
		t.Error("a mig-* resource must be recognised as a slice, not a whole device")
	}
	if IsMIGResource("nvidia.com/gpu") {
		t.Error("nvidia.com/gpu is a whole device")
	}
}

func TestNotAGPU(t *testing.T) {
	n, name := gpuCount(res("100m", "64Mi"), corev1.ResourceList{"example.com/fpga": resource.MustParse("1")})
	if n != 0 || name != "" {
		t.Errorf("got %d %q from a container with no accelerator", n, name)
	}
}

// Two GPU resources on one container is unusual, and the answer must not
// depend on map order: run it enough times that a map would have reordered.
func TestGPUResourceChoiceIsStable(t *testing.T) {
	rl := corev1.ResourceList{
		"nvidia.com/mig-3g.40gb": resource.MustParse("1"),
		"nvidia.com/mig-1g.10gb": resource.MustParse("2"),
		"nvidia.com/gpu":         resource.MustParse("1"),
	}
	_, first := gpuCount(rl)
	for range 50 {
		if _, got := gpuCount(rl); got != first {
			t.Fatalf("gpuCount chose %q then %q from the same list", first, got)
		}
	}
}

func gfdNode(labels map[string]string) Node {
	var out Node
	readGPU(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Labels: labels}}, &out)
	return out
}

// The unshared labels are the ones measured on the lab's RTX 3060 under GFD;
// the shared ones are what time-slicing with replicas: 4 sets.
func TestGPUSharingFromGFDLabels(t *testing.T) {
	cases := []struct {
		labels map[string]string
		shared bool
	}{
		{map[string]string{"nvidia.com/gpu.replicas": "1", "nvidia.com/gpu.sharing-strategy": "none"}, false},
		{map[string]string{}, false},
		{map[string]string{"nvidia.com/gpu.replicas": "4", "nvidia.com/gpu.sharing-strategy": "time-slicing"}, true},
		{map[string]string{"nvidia.com/gpu.sharing-strategy": "mps"}, true},
		{map[string]string{"nvidia.com/gpu.replicas": "4"}, true},
	}
	for _, c := range cases {
		if got := gfdNode(c.labels).GPUShared; got != c.shared {
			t.Errorf("%v: shared=%v, want %v", c.labels, got, c.shared)
		}
	}
}
