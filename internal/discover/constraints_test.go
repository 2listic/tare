package discover

import (
	"context"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func vpa(ns, name, target, mode string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "autoscaling.k8s.io/v1", "kind": "VerticalPodAutoscaler",
		"metadata": map[string]any{"namespace": ns, "name": name},
		"spec": map[string]any{
			"targetRef":    map[string]any{"kind": "Deployment", "name": target},
			"updatePolicy": map[string]any{"updateMode": mode},
		},
	}}
}

func vpaClient(objs ...runtime.Object) *dynfake.FakeDynamicClient {
	return dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{vpaGVR: "VerticalPodAutoscalerList"}, objs...)
}

// RBAC is per namespace. One refused namespace stopped the scan, so a VPA in
// Auto mode in every later namespace went unseen and tare recommended values
// for a field another controller was already writing.
func TestVPAScanContinuesPastARefusedNamespace(t *testing.T) {
	dyn := vpaClient(vpa("b", "web-vpa", "web", "Auto"))
	dyn.PrependReactor("list", "verticalpodautoscalers", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if a.GetNamespace() == "a" {
			return true, nil, errors.NewForbidden(vpaGVR.GroupResource(), "", nil)
		}
		return false, nil, nil
	})
	c := &Client{cs: fake.NewSimpleClientset(), dyn: dyn}

	out := c.LoadConstraints(context.Background(), []string{"a", "b"})
	if got := out[ConstraintKey("b", "Deployment/web")]; got.VPAName != "web-vpa" {
		t.Errorf("VPA in namespace b not found after namespace a was refused: %+v", out)
	}
	if len(c.warnings) != 1 || !strings.Contains(c.warnings[0], "vertical pod autoscalers") {
		t.Errorf("the refusal must be reported once: %q", c.warnings)
	}
}

// A cluster without the CRD fails every namespace identically; that is not
// worth a warning per namespace, or at all.
func TestMissingVPACRDIsSilent(t *testing.T) {
	dyn := vpaClient()
	dyn.PrependReactor("list", "verticalpodautoscalers", func(a k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.NewNotFound(vpaGVR.GroupResource(), "")
	})
	c := &Client{cs: fake.NewSimpleClientset(), dyn: dyn}
	c.LoadConstraints(context.Background(), []string{"a", "b"})
	if len(c.warnings) != 0 {
		t.Errorf("missing CRD produced warnings: %q", c.warnings)
	}
}
