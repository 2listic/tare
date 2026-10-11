package discover

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func res(cpu, mem string) corev1.ResourceList {
	return corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse(cpu),
		corev1.ResourceMemory: resource.MustParse(mem),
	}
}

func ctr(name, cpu, mem string) corev1.Container {
	return corev1.Container{Name: name, Resources: corev1.ResourceRequirements{Requests: res(cpu, mem)}}
}

// The scheduler reserves the sum of the regular containers, floored by the
// largest single init container, because init containers run one at a time and
// finish before the others start. Getting this wrong makes the replay disagree
// with the placement the scheduler actually made, which the self-check catches
// as a model failure rather than as the arithmetic bug it is.
func TestEffectiveRequestsSumContainersAndMaxInit(t *testing.T) {
	p := &corev1.Pod{Spec: corev1.PodSpec{
		Containers:     []corev1.Container{ctr("a", "200m", "256Mi"), ctr("b", "300m", "256Mi")},
		InitContainers: []corev1.Container{ctr("small", "100m", "64Mi"), ctr("big", "400m", "128Mi")},
	}}
	cpu, mem := effectiveRequests(p)
	if cpu != 500 {
		t.Errorf("cpu = %dm, want 500: the containers sum to 500m, above the 400m largest init", cpu)
	}
	if mem != 512<<20 {
		t.Errorf("mem = %d, want 512Mi", mem)
	}
}

func TestLargeInitContainerFloorsTheRequest(t *testing.T) {
	p := &corev1.Pod{Spec: corev1.PodSpec{
		Containers:     []corev1.Container{ctr("a", "100m", "64Mi")},
		InitContainers: []corev1.Container{ctr("migrate", "2", "1Gi")},
	}}
	cpu, mem := effectiveRequests(p)
	if cpu != 2000 || mem != 1<<30 {
		t.Errorf("got %dm / %d, want the init container's 2000m / 1Gi to floor the pod", cpu, mem)
	}
}

// A native sidecar is an init container with restartPolicy: Always. It runs
// alongside the regular containers rather than before them, so it adds instead
// of flooring.
func TestNativeSidecarAddsRatherThanFloors(t *testing.T) {
	always := corev1.ContainerRestartPolicyAlways
	side := ctr("proxy", "100m", "64Mi")
	side.RestartPolicy = &always
	p := &corev1.Pod{Spec: corev1.PodSpec{
		Containers:     []corev1.Container{ctr("app", "200m", "128Mi")},
		InitContainers: []corev1.Container{side},
	}}
	cpu, mem := effectiveRequests(p)
	if cpu != 300 || mem != 192<<20 {
		t.Errorf("got %dm / %d, want 300m / 192Mi", cpu, mem)
	}
}

func TestPodOverheadIsCharged(t *testing.T) {
	p := &corev1.Pod{Spec: corev1.PodSpec{
		Containers: []corev1.Container{ctr("app", "100m", "128Mi")},
		Overhead:   res("50m", "64Mi"),
	}}
	cpu, mem := effectiveRequests(p)
	if cpu != 150 || mem != 192<<20 {
		t.Errorf("got %dm / %d, want the runtime overhead included", cpu, mem)
	}
}

func TestPinReasons(t *testing.T) {
	cases := []struct {
		name string
		kind string
		spec corev1.PodSpec
		want bool
	}{
		{"plain deployment pod", "Deployment", corev1.PodSpec{}, false},
		{"daemonset", "DaemonSet", corev1.PodSpec{NodeSelector: map[string]string{"a": "b"}}, false},
		{"bare pod", "", corev1.PodSpec{}, true},
		{"node selector", "Deployment", corev1.PodSpec{NodeSelector: map[string]string{"disk": "ssd"}}, true},
		{"affinity", "Deployment", corev1.PodSpec{Affinity: &corev1.Affinity{}}, true},
		{"topology spread", "Deployment", corev1.PodSpec{
			TopologySpreadConstraints: []corev1.TopologySpreadConstraint{{}}}, true},
		{"pvc", "Deployment", corev1.PodSpec{Volumes: []corev1.Volume{{
			VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{}}}}}, true},
		{"hostpath", "Deployment", corev1.PodSpec{Volumes: []corev1.Volume{{
			VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{}}}}}, true},
		{"priority class", "Deployment", corev1.PodSpec{PriorityClassName: "high"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := pinReason(&corev1.Pod{Spec: tc.spec}, tc.kind) != ""
			if got != tc.want {
				t.Errorf("pinned = %v, want %v (reason %q)", got, tc.want,
					pinReason(&corev1.Pod{Spec: tc.spec}, tc.kind))
			}
		})
	}
}

// Every pod carries the kubelet's two default tolerations. Treating those as a
// reason to pin would pin the entire cluster and report a zero saving on every
// run, which would look like a working tool finding nothing.
func TestDefaultTolerationsDoNotPin(t *testing.T) {
	spec := corev1.PodSpec{Tolerations: []corev1.Toleration{
		{Key: corev1.TaintNodeNotReady}, {Key: corev1.TaintNodeUnreachable},
	}}
	if r := pinReason(&corev1.Pod{Spec: spec}, "Deployment"); r != "" {
		t.Errorf("the kubelet's own tolerations must not pin a pod: %q", r)
	}
	spec.Tolerations = append(spec.Tolerations, corev1.Toleration{Key: "gpu"})
	if pinReason(&corev1.Pod{Spec: spec}, "Deployment") == "" {
		t.Error("a real toleration must pin")
	}
}

func TestExtendedResourcesPin(t *testing.T) {
	c := ctr("trainer", "1", "2Gi")
	c.Resources.Limits = corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("1")}
	p := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{c}}}
	if pinReason(p, "Deployment") == "" {
		t.Error("a GPU pod must not be modelled as freely movable")
	}
}

// A pod's nodeSelector is two different facts depending on what owns it, and
// the replay needs both. For a workload pod it is a pin reason. For a
// DaemonSet - which is never packed, only charged as per-node overhead - it is
// the set of nodes that overhead belongs to, so it has to survive the
// translation even though the pod is not pinned.
func TestNodeSelectorIsCarriedForBothPinningAndOverhead(t *testing.T) {
	sel := map[string]string{"node-role.kubernetes.io/control-plane": "true"}
	spec := corev1.PodSpec{
		NodeName:     "server-0",
		NodeSelector: sel,
		Containers:   []corev1.Container{ctr("app", "100m", "128Mi")},
	}

	app := podFrom(&corev1.Pod{Spec: spec}, "Deployment/metrics-server", nil)
	if app.NodeSelector["node-role.kubernetes.io/control-plane"] != "true" {
		t.Errorf("NodeSelector = %v, want the selector the pod declares", app.NodeSelector)
	}
	if !app.Pinned || app.PinReason != "has a nodeSelector" {
		t.Errorf("pinned=%v reason=%q, want a workload pod pinned by its selector", app.Pinned, app.PinReason)
	}

	ds := podFrom(&corev1.Pod{Spec: spec}, "DaemonSet/node-exporter", nil)
	if ds.Pinned {
		t.Errorf("a DaemonSet pod is overhead, not cargo, and must not be pinned: %q", ds.PinReason)
	}
	if ds.NodeSelector["node-role.kubernetes.io/control-plane"] != "true" {
		t.Error("the DaemonSet's selector was dropped: its per-node charge would then be " +
			"applied to every node in the cluster, including those it never runs on")
	}
}

// Matches is nodeSelector semantics and nothing more: every key present with
// the same value. The empty selector is every node, and an unlabelled node is
// no node - which is why the packing model also charges a DaemonSet where its
// pods are observed rather than trusting this alone.
func TestNodeMatchesSelector(t *testing.T) {
	n := Node{Name: "gpu-0", Labels: map[string]string{"accelerator": "nvidia", "zone": "a"}}
	cases := []struct {
		sel  map[string]string
		want bool
	}{
		{nil, true},
		{map[string]string{"accelerator": "nvidia"}, true},
		{map[string]string{"accelerator": "nvidia", "zone": "a"}, true},
		{map[string]string{"accelerator": "amd"}, false},
		{map[string]string{"accelerator": "nvidia", "zone": "b"}, false},
		{map[string]string{"missing": "x"}, false},
	}
	for _, tc := range cases {
		if got := n.Matches(tc.sel); got != tc.want {
			t.Errorf("Matches(%v) = %v, want %v", tc.sel, got, tc.want)
		}
	}
	if (Node{Name: "unlabelled"}).Matches(map[string]string{"role": "infra"}) {
		t.Error("a node with no labels read must not match a selector")
	}
}

// A kubelet that stops reporting leaves the Ready condition at Unknown, not
// False. Anything testing for "not False" reads that as a healthy node, which
// is how a dead machine keeps offering its capacity.
func TestReadyIsTrueOnlyWhenExplicitlyTrue(t *testing.T) {
	withReady := func(status corev1.ConditionStatus) *corev1.Node {
		return &corev1.Node{Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{
			{Type: corev1.NodeMemoryPressure, Status: corev1.ConditionFalse},
			{Type: corev1.NodeReady, Status: status},
		}}}
	}
	for _, tc := range []struct {
		status corev1.ConditionStatus
		want   bool
	}{
		{corev1.ConditionTrue, true},
		{corev1.ConditionFalse, false},
		{corev1.ConditionUnknown, false},
	} {
		if got := isReady(withReady(tc.status)); got != tc.want {
			t.Errorf("Ready=%s read as %v, want %v", tc.status, got, tc.want)
		}
	}
	if isReady(&corev1.Node{}) {
		t.Error("a node reporting no conditions at all is not a node known to be live")
	}
}

// BlockingTaint is about what a MOVABLE pod can be sent to, and the replay will
// only move pods that tolerate nothing beyond the kubelet's defaults. So the
// kubelet's own taints must not exclude a node - they arrive on every node that
// so much as flaps - while an operator's taint must.
func TestBlockingTaint(t *testing.T) {
	cases := []struct {
		name  string
		taint corev1.Taint
		want  bool
	}{
		{"operator taint", corev1.Taint{Key: "dedicated", Value: "gpu", Effect: corev1.TaintEffectNoSchedule}, true},
		{"no-execute", corev1.Taint{Key: "dedicated", Effect: corev1.TaintEffectNoExecute}, true},
		{"preference only", corev1.Taint{Key: "spot", Effect: corev1.TaintEffectPreferNoSchedule}, false},
		{"unreachable", corev1.Taint{Key: corev1.TaintNodeUnreachable, Effect: corev1.TaintEffectNoSchedule}, false},
		{"not ready", corev1.Taint{Key: corev1.TaintNodeNotReady, Effect: corev1.TaintEffectNoExecute}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Node{Taints: []corev1.Taint{tc.taint}}.BlockingTaint()
			if (got != "") != tc.want {
				t.Errorf("BlockingTaint() = %q, want blocking=%v", got, tc.want)
			}
		})
	}
	if (Node{}).BlockingTaint() != "" {
		t.Error("an untainted node blocks nothing")
	}
}

// The pod's summed limit cannot say how many of its containers are unbounded:
// a limited app container hides the unlimited sidecar beside it.
func TestUnlimitedContainersAreCountedPerContainer(t *testing.T) {
	app := ctr("app", "100m", "128Mi")
	app.Resources.Limits = res("500m", "256Mi")
	sidecar := ctr("proxy", "10m", "32Mi")
	sidecar.Resources.Limits = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("64Mi")}

	p := podFrom(&corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{app, sidecar}}}, "Deployment/web", nil)
	if p.UnlimitedCPU != 1 || p.UnlimitedMem != 0 {
		t.Errorf("UnlimitedCPU=%d UnlimitedMem=%d, want 1 and 0", p.UnlimitedCPU, p.UnlimitedMem)
	}
}
