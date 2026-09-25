package discover

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// TestMissingNamespacesNamesTheTypo: listing pods in a namespace that was
// never created returns an empty list rather than an error, so a mistyped -n
// is indistinguishable from a cluster with nothing running. The report then
// blames the cluster for exporting no metrics.
func TestMissingNamespacesNamesTheTypo(t *testing.T) {
	cs := fake.NewSimpleClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "tare-lab"}})
	got := NewWithClientset(cs).MissingNamespaces(context.Background(), []string{"tare-lab", "no-such-ns"})
	if len(got) != 1 || got[0] != "no-such-ns" {
		t.Errorf("MissingNamespaces = %v, want [no-such-ns]", got)
	}
}

func TestMissingNamespacesIsSilentWhenAllExist(t *testing.T) {
	cs := fake.NewSimpleClientset(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "tare-lab"}},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "monitoring"}},
	)
	if got := NewWithClientset(cs).MissingNamespaces(context.Background(), []string{"tare-lab", "monitoring"}); len(got) != 0 {
		t.Errorf("MissingNamespaces = %v, want none", got)
	}
}

// TestMissingNamespacesIgnoresAForbiddenGet: a namespace-scoped user may read
// pods in their own namespace and nothing at cluster scope. Absence of proof
// is not proof of absence, so a Get they are not allowed to make must not
// refuse them a report.
func TestMissingNamespacesIgnoresAForbiddenGet(t *testing.T) {
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("get", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(corev1.Resource("namespaces"), "prod", errors.New("no cluster scope"))
	})
	if got := NewWithClientset(cs).MissingNamespaces(context.Background(), []string{"prod"}); len(got) != 0 {
		t.Errorf("MissingNamespaces = %v, want none: a forbidden read is not a missing namespace", got)
	}
}
