package discover

import (
	"context"
	"fmt"

	authv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Permission is one API read tare performs, and what the report loses when the
// answer is no.
//
// The list exists because RBAC is the first wall a new user hits and the one
// the tool is least able to explain after the fact: a denied read surfaces as
// a Kubernetes error deep in a run, or worse, as a finding that quietly
// stopped being produced. Asking the API server up front turns that into a
// sentence.
type Permission struct {
	Verb        string
	Group       string
	Resource    string
	Subresource string
	Buys        string
	Required    bool

	Allowed bool
	Err     error
}

func (p Permission) String() string {
	s := p.Resource
	if p.Subresource != "" {
		s += "/" + p.Subresource
	}
	if p.Group != "" {
		s = s + "." + p.Group
	}
	return p.Verb + " " + s
}

// Needs lists every read a full run makes. Only the first two are fatal; the
// rest each cost a named part of the report, which is the whole point of
// listing them separately.
func Needs() []Permission {
	return []Permission{
		{Verb: "list", Resource: "pods", Required: true,
			Buys: "the container inventory: without it there is no report at all"},
		{Verb: "get", Resource: "services", Subresource: "proxy",
			Buys: "querying the cluster's own Prometheus through the API server, " +
				"which is what makes a first run need no port-forward. Without it, pass --prometheus"},
		{Verb: "list", Resource: "services",
			Buys: "finding that Prometheus automatically. Without it, pass --prometheus"},
		{Verb: "list", Group: "apps", Resource: "deployments",
			Buys: "reading pod templates, which is the only place a request the author never wrote is still visible"},
		{Verb: "list", Group: "apps", Resource: "replicasets",
			Buys: "attributing pods to the Deployment above them rather than to a hash-suffixed ReplicaSet"},
		{Verb: "list", Group: "apps", Resource: "statefulsets", Buys: "the same, for StatefulSets"},
		{Verb: "list", Group: "apps", Resource: "daemonsets",
			Buys: "the same for DaemonSets, whose replica count is your node count"},
		{Verb: "list", Group: "batch", Resource: "jobs", Buys: "the same, for Jobs"},
		{Verb: "list", Group: "batch", Resource: "cronjobs", Buys: "the same, for CronJobs"},
		{Verb: "list", Resource: "nodes",
			Buys: "the node-count delta and the commitment table: the only figures here that correspond to a bill"},
		{Verb: "list", Group: "policy", Resource: "poddisruptionbudgets",
			Buys: "knowing whether a node could actually be drained, rather than promising a saving the cluster will refuse"},
		{Verb: "list", Group: "autoscaling", Resource: "horizontalpodautoscalers",
			Buys: "the warning that resizing a workload moves its HPA's scaling setpoint"},
		{Verb: "list", Group: "autoscaling.k8s.io", Resource: "verticalpodautoscalers",
			Buys: "the check that a VPA is not already writing the fields tare is about to recommend"},
	}
}

// CheckPermissions asks the API server what this token may do, using the same
// SelfSubjectAccessReview endpoint `kubectl auth can-i` uses. Every
// authenticated user may create one for themselves, so the check itself never
// needs a permission of its own.
//
// namespace is the scope to ask about: empty means cluster-wide, which is what
// a default run does.
func (c *Client) CheckPermissions(ctx context.Context, namespace string) []Permission {
	perms := Needs()
	for i := range perms {
		p := &perms[i]
		ssar := &authv1.SelfSubjectAccessReview{
			Spec: authv1.SelfSubjectAccessReviewSpec{
				ResourceAttributes: &authv1.ResourceAttributes{
					Namespace:   namespace,
					Verb:        p.Verb,
					Group:       p.Group,
					Resource:    p.Resource,
					Subresource: p.Subresource,
				},
			},
		}
		res, err := c.cs.AuthorizationV1().SelfSubjectAccessReviews().
			Create(ctx, ssar, metav1.CreateOptions{})
		if err != nil {
			p.Err = err
			continue
		}
		p.Allowed = res.Status.Allowed
	}
	return perms
}

// ServerVersion identifies the cluster, so a bug report can say which one.
func (c *Client) ServerVersion() (string, error) {
	v, err := c.cs.Discovery().ServerVersion()
	if err != nil {
		return "", fmt.Errorf("reaching the API server: %w", err)
	}
	return v.GitVersion, nil
}
