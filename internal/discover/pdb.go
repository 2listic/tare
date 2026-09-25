package discover

import (
	"context"
	"fmt"

	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

// PDB is a PodDisruptionBudget as it bears on consolidation.
//
// It matters here because a node-count saving is only real if the nodes can
// actually be drained, and a PDB that allows no disruption blocks that
// permanently. A report that says "two nodes can be freed" while a PDB makes
// the drain impossible is telling the reader something they cannot act on,
// which is the same class of error as reporting millicores as money.
type PDB struct {
	Namespace string
	Name      string

	// Allowed is status.disruptionsAllowed: how many pods may be evicted right
	// now. Zero means every drain touching this budget blocks until something
	// changes. It is the controller's own live computation, which is stronger
	// evidence than reading minAvailable against a replica count.
	Allowed int32

	// Expected is status.expectedPods, and Healthy status.currentHealthy.
	Expected int32
	Healthy  int32

	// Blocking says the budget permits nothing to be evicted, and Why explains
	// whether that is a permanent configuration or a transient unhealthy state.
	Blocking bool
	Why      string

	selector labels.Selector
}

// Covers reports whether this budget applies to a pod's labels.
func (p PDB) Covers(podLabels map[string]string) bool {
	if p.selector == nil {
		return false
	}
	return p.selector.Matches(labels.Set(podLabels))
}

// PDBs lists the disruption budgets in the given namespaces. As everywhere
// else in this package, a failure degrades rather than aborts: the sizing
// report is still worth printing without it.
func (c *Client) PDBs(ctx context.Context, namespaces []string) []PDB {
	targets := namespaces
	if len(targets) == 0 {
		targets = []string{metav1.NamespaceAll}
	}
	var out []PDB
	for _, ns := range targets {
		list, err := c.cs.PolicyV1().PodDisruptionBudgets(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			// Silence here would overstate the node-count saving: a budget
			// that allows no disruption means the node cannot be drained, and
			// a saving the cluster will refuse to perform is not a saving.
			c.warn("pod disruption budgets in %s could not be read (%v): "+
				"a node counted as removable may be undrainable", describeScope(ns), err)
			continue
		}
		for i := range list.Items {
			p := &list.Items[i]
			sel, err := metav1.LabelSelectorAsSelector(p.Spec.Selector)
			if err != nil {
				continue
			}
			pdb := PDB{
				Namespace: p.Namespace, Name: p.Name,
				Allowed:  p.Status.DisruptionsAllowed,
				Expected: p.Status.ExpectedPods,
				Healthy:  p.Status.CurrentHealthy,
				selector: sel,
			}
			pdb.Blocking, pdb.Why = pdbBlocking(p)
			out = append(out, pdb)
		}
	}
	return out
}

// pdbBlocking distinguishes a budget that is permanently unsatisfiable from one
// that merely happens to be tight today. The two need different sentences: the
// first is a configuration to fix, the second resolves on its own.
func pdbBlocking(p *policyv1.PodDisruptionBudget) (bool, string) {
	if p.Status.DisruptionsAllowed > 0 {
		return false, ""
	}
	switch {
	case p.Spec.MaxUnavailable != nil && p.Spec.MaxUnavailable.IntValue() == 0 &&
		p.Spec.MaxUnavailable.Type == 0:
		return true, fmt.Sprintf(
			"PodDisruptionBudget %s/%s sets maxUnavailable: 0, which permits no eviction ever. "+
				"Every drain of a node running these pods blocks indefinitely, so no consolidation can remove such a node and a cluster autoscaler will keep retrying and give up.",
			p.Namespace, p.Name)
	case p.Status.CurrentHealthy <= p.Status.DesiredHealthy && p.Status.ExpectedPods > 0 &&
		p.Status.DesiredHealthy >= p.Status.ExpectedPods:
		return true, fmt.Sprintf(
			"PodDisruptionBudget %s/%s requires all %d of its pods healthy, so it allows no disruption at any time. "+
				"Raise the replica count or lower minAvailable, or nodes running these pods can never be drained.",
			p.Namespace, p.Name, p.Status.ExpectedPods)
	default:
		return true, fmt.Sprintf(
			"PodDisruptionBudget %s/%s currently allows no disruption (%d of %d pods healthy). "+
				"This may be transient, but while it holds, no node running these pods can be drained.",
			p.Namespace, p.Name, p.Status.CurrentHealthy, p.Status.ExpectedPods)
	}
}
