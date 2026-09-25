package discover

import (
	"strings"
	"testing"

	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func pdbWith(allowed, healthy, desired, expected int32, maxUnavail *intstr.IntOrString) *policyv1.PodDisruptionBudget {
	return &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Namespace: "prod", Name: "budget"},
		Spec:       policyv1.PodDisruptionBudgetSpec{MaxUnavailable: maxUnavail},
		Status: policyv1.PodDisruptionBudgetStatus{
			DisruptionsAllowed: allowed, CurrentHealthy: healthy,
			DesiredHealthy: desired, ExpectedPods: expected,
		},
	}
}

func TestBudgetAllowingDisruptionIsNotBlocking(t *testing.T) {
	blocking, why := pdbBlocking(pdbWith(1, 3, 2, 3, nil))
	if blocking || why != "" {
		t.Errorf("disruptionsAllowed=1 is not a blocker: %q", why)
	}
}

// maxUnavailable: 0 is permanent and the message must say so, because the fix
// is a config change rather than waiting.
func TestMaxUnavailableZeroIsPermanent(t *testing.T) {
	zero := intstr.FromInt32(0)
	blocking, why := pdbBlocking(pdbWith(0, 3, 3, 3, &zero))
	if !blocking {
		t.Fatal("maxUnavailable: 0 permits no eviction ever")
	}
	if !strings.Contains(why, "no eviction ever") {
		t.Errorf("the permanence must be stated: %s", why)
	}
}

// minAvailable equal to the replica count is the same permanent trap wearing a
// different flag, and is far more common than maxUnavailable: 0.
func TestAllPodsRequiredHealthyIsPermanent(t *testing.T) {
	blocking, why := pdbBlocking(pdbWith(0, 3, 3, 3, nil))
	if !blocking {
		t.Fatal("requiring every pod healthy permits no disruption")
	}
	if !strings.Contains(why, "Raise the replica count or lower minAvailable") {
		t.Errorf("the fix must be named: %s", why)
	}
}

// A budget that is tight only because a pod is currently unhealthy resolves on
// its own. Telling the operator to change their config would be wrong.
func TestTransientlyTightBudgetSaysSo(t *testing.T) {
	blocking, why := pdbBlocking(pdbWith(0, 2, 2, 3, nil))
	if !blocking {
		t.Fatal("no disruption is allowed right now, so a drain does block")
	}
	if !strings.Contains(why, "may be transient") {
		t.Errorf("a transient state must not be reported as a misconfiguration: %s", why)
	}
}

func TestBlockedByPDBMatchesOnLabelsAndNamespace(t *testing.T) {
	sel, _ := metav1.LabelSelectorAsSelector(&metav1.LabelSelector{
		MatchLabels: map[string]string{"app": "api"},
	})
	p := PDB{Namespace: "prod", Name: "budget", Blocking: true, selector: sel}

	if r := blockedByPDB([]PDB{p}, "prod", map[string]string{"app": "api"}); r == "" {
		t.Error("a covered pod in the same namespace must be pinned")
	}
	if r := blockedByPDB([]PDB{p}, "staging", map[string]string{"app": "api"}); r != "" {
		t.Errorf("a budget must not reach across namespaces: %s", r)
	}
	if r := blockedByPDB([]PDB{p}, "prod", map[string]string{"app": "worker"}); r != "" {
		t.Errorf("a budget must not cover a pod it does not select: %s", r)
	}

	p.Blocking = false
	if r := blockedByPDB([]PDB{p}, "prod", map[string]string{"app": "api"}); r != "" {
		t.Errorf("a budget that allows eviction pins nothing: %s", r)
	}
}
