package discover

import (
	"context"
	"sort"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ListNamespaces returns the names of all namespaces in the cluster, sorted.
func (c *Client) ListNamespaces(ctx context.Context) ([]string, error) {
	list, err := c.cs.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(list.Items))
	for _, item := range list.Items {
		out = append(out, item.Name)
	}
	sort.Strings(out)
	return out, nil
}

// MissingNamespaces reports which of the requested namespaces do not exist.
//
// It exists because listing pods in a namespace that was never created returns
// an empty list rather than an error, so a mistyped -n produces no containers,
// no series, and a report that tells the operator their cluster exports none
// of the signals tare needs. That is a clean, plausible, wrong answer about
// the cluster, produced by a typo in the command. The names are checked before
// any measurement is taken so the tool can say which one it was.
//
// A namespace that cannot be read for any other reason is not reported
// missing. The common case is a namespace-scoped user who may list pods in
// their own namespace and nothing at cluster scope: absence of proof is not
// proof of absence, and refusing them a report is worse than the typo this
// guards against.
func (c *Client) MissingNamespaces(ctx context.Context, namespaces []string) []string {
	var missing []string
	for _, ns := range namespaces {
		if _, err := c.cs.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{}); apierrors.IsNotFound(err) {
			missing = append(missing, ns)
		}
	}
	return missing
}
