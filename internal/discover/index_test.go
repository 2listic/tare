package discover

import (
	"context"
	"fmt"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// deploymentWithPods builds one Deployment, the ReplicaSet it owns, and n pods
// under that ReplicaSet.
func deploymentWithPods(n int) []runtime.Object {
	ctrl := true
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "prod"},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name: "app",
				// Only a limit is authored: the request below is the copy
				// Kubernetes makes, which is the distinction this index exists
				// to preserve.
				Resources: corev1.ResourceRequirements{
					Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")},
				},
			}}},
		}},
	}
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
		Name: "web-7d9", Namespace: "prod",
		OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "web", Controller: &ctrl}},
	}}
	objs := []runtime.Object{dep, rs}
	for i := range n {
		objs = append(objs, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: fmt.Sprintf("web-7d9-%d", i), Namespace: "prod",
				OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "web-7d9", Controller: &ctrl}},
			},
			Spec: corev1.PodSpec{NodeName: "n1", Containers: []corev1.Container{{
				Name: "app",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")},
					Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")},
				},
			}}},
		})
	}
	return objs
}

func TestPodsResolveToTheirDeployment(t *testing.T) {
	c := &Client{cs: fake.NewSimpleClientset(deploymentWithPods(3)...)}
	cs, err := c.Containers(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 3 {
		t.Fatalf("got %d containers, want 3", len(cs))
	}
	for _, got := range cs {
		if got.Workload != "Deployment/web" {
			t.Errorf("workload = %q, want Deployment/web", got.Workload)
		}
		if !got.Authored.Known {
			t.Fatal("authored template was not read")
		}
		if got.Authored.CPURequest {
			t.Error("the CPU request was copied from the limit, not authored")
		}
		if !got.Authored.CPULimit {
			t.Error("the CPU limit was authored and should be reported as such")
		}
	}
}

// TestAPICallsDoNotScaleWithPodCount is the regression guard for the defect
// that made tare unusable on any cluster bigger than the lab: one ReplicaSet
// GET per pod, serialised behind client-go's 5-per-second rate limiter, so a
// thousand pods spent minutes in the client and the run hit its own deadline
// before it ever queried Prometheus. Ten pods and a thousand must cost the
// same number of API calls.
func TestAPICallsDoNotScaleWithPodCount(t *testing.T) {
	count := func(pods int) int {
		cs := fake.NewSimpleClientset(deploymentWithPods(pods)...)
		var n int
		cs.PrependReactor("*", "*", func(k8stesting.Action) (bool, runtime.Object, error) {
			n++
			return false, nil, nil
		})
		c := &Client{cs: cs}
		if _, err := c.Containers(context.Background(), nil); err != nil {
			t.Fatal(err)
		}
		return n
	}
	small, large := count(10), count(1000)
	if small != large {
		t.Fatalf("API calls scale with pod count: %d calls for 10 pods, %d for 1000", small, large)
	}
	if large > 4 {
		t.Fatalf("%d API calls for one Deployment; the index should need a pod list, "+
			"a ReplicaSet list and a Deployment list", large)
	}
}

// TestIndexIsSharedBetweenTheSizingPassAndTheReplay: Containers and Pods both
// need owner resolution and run far apart in one run. The second must not
// repeat the lists the first already paid for.
func TestIndexIsSharedBetweenTheSizingPassAndTheReplay(t *testing.T) {
	cs := fake.NewSimpleClientset(deploymentWithPods(5)...)
	c := &Client{cs: cs}
	ctx := context.Background()
	if _, err := c.Containers(ctx, nil); err != nil {
		t.Fatal(err)
	}
	before := len(cs.Actions())
	pods, err := c.Pods(ctx, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(pods) != 5 {
		t.Fatalf("got %d pods, want 5", len(pods))
	}
	if got := len(cs.Actions()) - before; got != 1 {
		t.Fatalf("the replay made %d API calls; only the pod list should be repeated", got)
	}
	for _, p := range pods {
		if p.Workload != "Deployment/web" {
			t.Fatalf("replay workload = %q, want Deployment/web", p.Workload)
		}
	}
}

// TestUnreadableTemplatesWarnRatherThanFail: a token that can list pods and
// nothing else must still produce a report, and must say what it lost.
func TestUnreadableTemplatesWarnRatherThanFail(t *testing.T) {
	cs := fake.NewSimpleClientset(deploymentWithPods(2)...)
	cs.PrependReactor("list", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("deployments.apps is forbidden")
	})
	c := &Client{cs: cs}
	got, err := c.Containers(context.Background(), nil)
	if err != nil {
		t.Fatalf("a forbidden template read must not fail the report: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d containers, want 2", len(got))
	}
	if got[0].Authored.Known {
		t.Error("authored must not be claimed known when the template could not be read")
	}
	if len(c.Warnings()) != 1 {
		t.Fatalf("warnings = %v, want one line naming what was lost", c.Warnings())
	}
}
