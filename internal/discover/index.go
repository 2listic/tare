package discover

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// workloadIndex answers two questions about every pod - which controller owns
// it, and what that controller's template said - with one List per kind rather
// than one Get per pod.
//
// The Get-per-pod shape it replaces was quietly fatal on any real cluster.
// client-go rate-limits itself to 5 requests a second by default, so a
// thousand pods owned by ReplicaSets meant a thousand serialised lookups,
// several minutes of silence, and a run that hit its own deadline before it
// reached Prometheus. It never showed up here because the lab cluster has
// thirty pods. Listing is one request per kind per namespace scope, and the
// count no longer grows with the cluster.
type workloadIndex struct {
	c     *Client
	scope []string // namespaces to list; a single "" means cluster-wide

	tmpl    map[string]map[string]Authored // ns/Kind/name -> container -> authored
	rsOwner map[string]string              // ns/name -> "Kind/Name" of the ReplicaSet's own controller
	loaded  map[string]bool                // kind -> already listed
}

func newWorkloadIndex(c *Client, namespaces []string) *workloadIndex {
	scope := scopeOf(namespaces)
	return &workloadIndex{
		c: c, scope: scope,
		tmpl:    map[string]map[string]Authored{},
		rsOwner: map[string]string{},
		loaded:  map[string]bool{},
	}
}

// ownerOf walks ownerReferences up to the controller a human manages. A pod
// owned by a ReplicaSet is really owned by its Deployment, and that is the unit
// a recommendation applies to.
func (x *workloadIndex) ownerOf(ctx context.Context, pod *corev1.Pod) string {
	ref := metav1.GetControllerOf(pod)
	if ref == nil {
		return "Pod/" + pod.Name
	}
	if ref.Kind == "ReplicaSet" {
		x.load(ctx, "ReplicaSet")
		if owner, ok := x.rsOwner[pod.Namespace+"/"+ref.Name]; ok {
			return owner
		}
	}
	return ref.Kind + "/" + ref.Name
}

// authoredFor returns what the controller's pod template set for one container.
// A zero Authored means the template could not be read, which is not an error:
// the report is still worth printing without this finding.
func (x *workloadIndex) authoredFor(ctx context.Context, ns, kind, name, container string) Authored {
	x.load(ctx, kind)
	byContainer, ok := x.tmpl[ns+"/"+kind+"/"+name]
	if !ok {
		return Authored{}
	}
	return byContainer[container]
}

// load lists one controller kind across the namespace scope, once.
//
// A failure is recorded and not retried. The usual cause is RBAC - a token
// allowed to read pods and nothing else - and retrying it per pod would turn
// one denial into thousands.
func (x *workloadIndex) load(ctx context.Context, kind string) {
	if x.loaded[kind] {
		return
	}
	x.loaded[kind] = true
	for _, ns := range x.scope {
		if err := x.list(ctx, kind, ns); err != nil {
			x.c.warn("%ss in %s could not be read (%v): for those workloads tare cannot tell a request its "+
				"author wrote from one Kubernetes copied out of the limit, and pods are attributed to "+
				"the object that owns them rather than to the controller above it",
				kind, describeScope(ns), err)
			return
		}
	}
}

func (x *workloadIndex) list(ctx context.Context, kind, ns string) error {
	opts := metav1.ListOptions{}
	switch kind {
	case "Deployment":
		l, err := x.c.cs.AppsV1().Deployments(ns).List(ctx, opts)
		if err != nil {
			return err
		}
		for i := range l.Items {
			x.put(kind, l.Items[i].Namespace, l.Items[i].Name, &l.Items[i].Spec.Template.Spec)
		}
	case "StatefulSet":
		l, err := x.c.cs.AppsV1().StatefulSets(ns).List(ctx, opts)
		if err != nil {
			return err
		}
		for i := range l.Items {
			x.put(kind, l.Items[i].Namespace, l.Items[i].Name, &l.Items[i].Spec.Template.Spec)
		}
	case "DaemonSet":
		l, err := x.c.cs.AppsV1().DaemonSets(ns).List(ctx, opts)
		if err != nil {
			return err
		}
		for i := range l.Items {
			x.put(kind, l.Items[i].Namespace, l.Items[i].Name, &l.Items[i].Spec.Template.Spec)
		}
	case "ReplicaSet":
		l, err := x.c.cs.AppsV1().ReplicaSets(ns).List(ctx, opts)
		if err != nil {
			return err
		}
		for i := range l.Items {
			rs := &l.Items[i]
			if ref := metav1.GetControllerOf(rs); ref != nil {
				// The Deployment above it owns the template that matters, and
				// a cluster keeps ten dead ReplicaSets per Deployment. Holding
				// their pod specs too would multiply this index by an order of
				// magnitude for nothing.
				x.rsOwner[rs.Namespace+"/"+rs.Name] = ref.Kind + "/" + ref.Name
				continue
			}
			x.put(kind, rs.Namespace, rs.Name, &rs.Spec.Template.Spec)
		}
	case "Job":
		l, err := x.c.cs.BatchV1().Jobs(ns).List(ctx, opts)
		if err != nil {
			return err
		}
		for i := range l.Items {
			x.put(kind, l.Items[i].Namespace, l.Items[i].Name, &l.Items[i].Spec.Template.Spec)
		}
	case "CronJob":
		l, err := x.c.cs.BatchV1().CronJobs(ns).List(ctx, opts)
		if err != nil {
			return err
		}
		for i := range l.Items {
			x.put(kind, l.Items[i].Namespace, l.Items[i].Name, &l.Items[i].Spec.JobTemplate.Spec.Template.Spec)
		}
	}
	// Any other kind is a bare pod or a CRD-owned workload such as an Argo
	// Rollout. There is no template to compare against, so the finding is
	// simply unavailable and that is not a failure.
	return nil
}

func (x *workloadIndex) put(kind, ns, name string, spec *corev1.PodSpec) {
	x.tmpl[ns+"/"+kind+"/"+name] = authoredFromSpec(spec)
}
