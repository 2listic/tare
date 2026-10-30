// Package discover builds the container inventory from the Kubernetes API.
//
// It exists to answer two questions the metrics backend cannot: which workload
// owns a pod, and what the container spec says. The second is what feeds
// ceiling detection, which is the whole reason the report can be honest about
// runtime limits.
package discover

import (
	"context"
	"fmt"
	"maps"
	"os"
	"slices"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/2listic/tare/internal/ceilings"
)

// Container is one container of one pod, joined to the workload that owns it.
type Container struct {
	Namespace string
	Pod       string
	Name      string
	Image     string

	// Workload is the controller that owns the pod, e.g. "Deployment/nginx".
	// Recommendations are made per workload, never per pod.
	Workload string
	// WorkloadKind is the controller kind alone. It is kept separate from
	// Workload because one kind behaves differently from all the others: a
	// DaemonSet's replica count is the node count, so its request is
	// subtracted from every node's allocatable and grows with the cluster
	// rather than with traffic.
	WorkloadKind string

	// Node is the node this pod is running on, empty for an unscheduled pod.
	// It is carried through to the workload aggregate because the pods of one
	// Deployment land on different nodes, and both the bin-packing replay and
	// the heterogeneity check need to know which.
	Node string

	// GPUs is the accelerator allocations this container requests, and
	// GPUResource the extended resource they are requested under. A GPU is an
	// integer extended resource, so this is a count and never a size. Under
	// MIG's mixed strategy the resource is a slice (nvidia.com/mig-1g.10gb), so
	// one allocation is NOT one device: check GPUResource before comparing
	// anything against a whole device's memory.
	GPUs        int64
	GPUResource string

	CPURequest int64 // millicores, 0 when unset
	MemRequest int64 // bytes, 0 when unset
	CPULimit   int64 // millicores, 0 when unset
	MemLimit   int64 // bytes, 0 when unset
	QoS        string

	// Authored says which of the four resource fields the workload's own
	// manifest set. Kubernetes copies an omitted request from the limit, so a
	// running pod cannot be asked this question; only the controller's
	// template can.
	Authored Authored

	Spec          ceilings.Container
	LastTermError string // e.g. "OOMKilled"
}

// Client wraps the Kubernetes clientset.
type Client struct {
	cs  kubernetes.Interface
	dyn dynamic.Interface

	// Context is the kubeconfig context the client resolved to, named so that
	// anything tare hands back to run later, such as -o patch, can be pinned
	// to the cluster it was measured on. Empty when the config has none.
	Context string

	// idxs caches one workload index per namespace scope. Both the sizing pass
	// and the packing replay need to know which controller owns a pod, and
	// they run far apart; without this the second one repeats every List the
	// first one already did.
	idxs map[string]*workloadIndex

	// warnings are reads that failed without stopping the run. Each one
	// silently removes a finding, so none of them is allowed to stay inside
	// this package.
	warnings []string
	// denied marks the warnings the API server refused, as opposed to reads
	// that timed out or failed in transit. A --timeout that expired was
	// reported as an RBAC denial, which sends the reader to fix permissions
	// they already have.
	denied map[string]bool
}

// warn records a read that failed without being fatal.
func (c *Client) warn(format string, args ...any) {
	w := fmt.Sprintf(format, args...)
	c.warnings = append(c.warnings, w)
	for _, a := range args {
		if err, ok := a.(error); ok && errors.IsForbidden(err) {
			if c.denied == nil {
				c.denied = map[string]bool{}
			}
			c.denied[w] = true
		}
	}
}

// Denied reports whether a warning from Warnings was a refusal by the API
// server rather than a read that failed.
func (c *Client) Denied(warning string) bool { return c.denied[warning] }

// Warnings lists the non-fatal read failures of this run, usually RBAC. Each
// one silently removes a finding from the report, so none of them is allowed
// to stay inside this package.
func (c *Client) Warnings() []string {
	out := []string{}
	seen := map[string]bool{}
	for _, w := range c.warnings {
		if !seen[w] {
			seen[w] = true
			out = append(out, w)
		}
	}
	sort.Strings(out)
	if len(out) == 0 {
		return nil
	}
	return out
}

// index returns the workload index for a namespace scope, building it once.
func (c *Client) index(namespaces []string) *workloadIndex {
	key := strings.Join(namespaces, ",")
	if x, ok := c.idxs[key]; ok {
		return x
	}
	x := newWorkloadIndex(c, namespaces)
	if c.idxs == nil {
		c.idxs = map[string]*workloadIndex{}
	}
	c.idxs[key] = x
	return x
}

// NewClient builds a client from the usual kubeconfig resolution rules.
func NewClient(kubeconfig, context string) (*Client, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		rules.ExplicitPath = kubeconfig
	}
	loader := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		rules,
		&clientcmd.ConfigOverrides{CurrentContext: context},
	)
	cfg, err := loader.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("no usable kubeconfig: %w\n%s", err, kubeconfigHelp())
	}

	// client-go defaults to 5 requests per second, which is sized for a
	// controller that runs forever, not for a one-shot report. tare issues a
	// bounded number of List calls and then stops, and at the default rate a
	// large cluster spends minutes inside the client's own rate limiter.
	cfg.QPS, cfg.Burst = 50, 100
	cfg.UserAgent = UserAgent
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("building client: %w", err)
	}
	// The dynamic client reads CRDs such as VPA. Its absence is not fatal:
	// most clusters have no VPA installed.
	dyn, _ := dynamic.NewForConfig(cfg)
	if context == "" {
		if raw, err := loader.RawConfig(); err == nil {
			context = raw.CurrentContext
		}
	}
	return &Client{cs: cs, dyn: dyn, Context: context}, nil
}

// UserAgent identifies tare in API server audit logs. A cluster admin watching
// an unfamiliar client list every pod in the cluster deserves to be able to
// tell what it is, and which build of it.
var UserAgent = "kubectl-tare"

// kubeconfigHelp is what to do about the most common first failure there is.
// client-go answers a missing kubeconfig by suggesting KUBERNETES_MASTER, an
// environment variable nothing has set since 2015, and says nothing about the
// flags this tool actually has.
func kubeconfigHelp() string {
	h := "  tare reads the same kubeconfig kubectl does. Point it at a cluster:\n" +
		"    export KUBECONFIG=/path/to/kubeconfig     or  --kubeconfig /path/to/kubeconfig\n" +
		"    kubectl tare report --context my-cluster  to pick a context\n" +
		"  To see what the output looks like with no cluster at all:\n" +
		"    kubectl tare report --demo"
	if os.Getenv("KUBECONFIG") != "" {
		h = "  KUBECONFIG is set to " + os.Getenv("KUBECONFIG") + " and could not be used.\n" + h
	}
	return h
}

// NewWithClientset builds a Client over an existing clientset. It exists so
// the discovery and preflight paths can be exercised against a fake API server
// rather than only against whatever cluster the author happened to have.
func NewWithClientset(cs kubernetes.Interface) *Client { return &Client{cs: cs} }

// Containers lists every container in the given namespaces. An empty list
// means all namespaces, which is the default: observation is cluster-wide even
// though actuation is opt-in.
func (c *Client) Containers(ctx context.Context, namespaces []string) ([]Container, error) {
	var out []Container

	targets := scopeOf(namespaces)
	idx := c.index(namespaces)
	for _, ns := range targets {
		pods, err := c.cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("listing pods in %s: %w%s", describeScope(ns), err, podListHelp(ns, err))
		}
		for i := range pods.Items {
			out = append(out, c.fromPod(ctx, idx, &pods.Items[i])...)
		}
	}
	return out, nil
}

// CountPods reports how many pods are visible, without fetching them. The API
// server fills in RemainingItemCount for a limited list, so this costs one
// small request rather than the several megabytes a real inventory does.
func (c *Client) CountPods(ctx context.Context, namespaces []string) (int, error) {
	targets := scopeOf(namespaces)
	var total int
	for _, ns := range targets {
		list, err := c.cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{Limit: 1})
		if err != nil {
			return 0, err
		}
		total += len(list.Items)
		if n := list.GetRemainingItemCount(); n != nil {
			total += int(*n)
		}
	}
	return total, nil
}

// describeScope names a namespace for an error message. The empty string means
// every namespace, and printing it as `""` has sent more than one reader
// looking for a namespace by that name.
func describeScope(ns string) string {
	if ns == metav1.NamespaceAll {
		return "every namespace"
	}
	return "namespace " + ns
}

// podListHelp turns the one error that stops the tool dead into an instruction.
// A cluster-wide pod list is the first thing tare does and the first thing a
// namespace-scoped token is refused, and the raw Kubernetes message does not
// mention that the same run would have worked with -n.
func podListHelp(ns string, err error) string {
	if ns != metav1.NamespaceAll || !errors.IsForbidden(err) {
		return ""
	}
	return "\n  this token cannot list pods across the whole cluster." +
		"\n  Name the namespaces you can read:  kubectl tare report -n prod,staging" +
		"\n  Or check what the run needs:       kubectl tare check"
}

func (c *Client) fromPod(ctx context.Context, idx *workloadIndex, pod *corev1.Pod) []Container {
	workload := idx.ownerOf(ctx, pod)

	var out []Container
	for i := range pod.Spec.Containers {
		spec := &pod.Spec.Containers[i]
		cc := Container{
			Namespace:    pod.Namespace,
			Pod:          pod.Name,
			Name:         spec.Name,
			Image:        spec.Image,
			Workload:     workload,
			WorkloadKind: kindOf(workload),
			Node:         pod.Spec.NodeName,
			QoS:          string(pod.Status.QOSClass),
			CPURequest:   milli(spec.Resources.Requests, corev1.ResourceCPU),
			CPULimit:     milli(spec.Resources.Limits, corev1.ResourceCPU),
			MemRequest:   bytes(spec.Resources.Requests, corev1.ResourceMemory),
			MemLimit:     bytes(spec.Resources.Limits, corev1.ResourceMemory),
		}
		cc.GPUs, cc.GPUResource = gpuCount(spec.Resources.Limits, spec.Resources.Requests)
		if kind, name, ok := SplitWorkload(workload); ok {
			cc.Authored = idx.authoredFor(ctx, pod.Namespace, kind, name, spec.Name)
		}
		cc.Spec = specFor(spec, cc.MemLimit)
		cc.LastTermError = lastTermination(pod, spec.Name)
		out = append(out, cc)
	}
	return out
}

// specFor converts a container spec into the dependency-free view that ceiling
// detection consumes, marking env vars whose values are not in the pod spec.
func specFor(spec *corev1.Container, memLimit int64) ceilings.Container {
	c := ceilings.Container{
		Name:             spec.Name,
		Image:            spec.Image,
		Command:          spec.Command,
		Args:             spec.Args,
		Env:              map[string]string{},
		MemoryLimitBytes: memLimit,
	}
	for _, e := range spec.Env {
		if e.ValueFrom != nil {
			c.OpaqueEnv = append(c.OpaqueEnv, e.Name)
			continue
		}
		c.Env[e.Name] = e.Value
	}
	// envFrom pulls an unknown set of names from a ConfigMap or Secret, so any
	// runtime flag could be hiding there. Record the sources by name so the
	// report can say which object to look in.
	for _, ef := range spec.EnvFrom {
		switch {
		case ef.ConfigMapRef != nil:
			c.OpaqueEnv = append(c.OpaqueEnv, "configMap:"+ef.ConfigMapRef.Name)
		case ef.SecretRef != nil:
			c.OpaqueEnv = append(c.OpaqueEnv, "secret:"+ef.SecretRef.Name)
		}
	}
	return c
}

// scopeOf is the namespaces to query: the ones asked for, or every namespace
// when none were.
func scopeOf(namespaces []string) []string {
	if len(namespaces) == 0 {
		return []string{metav1.NamespaceAll}
	}
	return namespaces
}

// SplitWorkload splits a "Kind/name" workload string. ok is false unless both
// halves are present.
func SplitWorkload(workload string) (kind, name string, ok bool) {
	kind, name, ok = strings.Cut(workload, "/")
	return kind, name, ok && kind != "" && name != ""
}

// kindOf pulls the controller kind out of a "Kind/name" workload string.
func kindOf(workload string) string {
	kind, _, ok := strings.Cut(workload, "/")
	if !ok {
		return ""
	}
	return kind
}

// gpuCount reads the accelerator count and the resource it is requested
// under. Limits are checked first because an extended resource must have
// limit == request, and a manifest that sets only the limit is the common
// spelling.
func gpuCount(lists ...corev1.ResourceList) (int64, string) {
	for _, rl := range lists {
		// Sorted, because a map's order would make a container listing two
		// GPU resources report a different one on every run.
		for _, name := range slices.Sorted(maps.Keys(rl)) {
			if q := rl[name]; IsGPUResource(name) && q.Value() > 0 {
				return q.Value(), string(name)
			}
		}
	}
	return 0, ""
}

func lastTermination(pod *corev1.Pod, container string) string {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name != container {
			continue
		}
		if cs.LastTerminationState.Terminated != nil {
			return cs.LastTerminationState.Terminated.Reason
		}
	}
	return ""
}

func milli(rl corev1.ResourceList, name corev1.ResourceName) int64 {
	if q, ok := rl[name]; ok {
		return q.MilliValue()
	}
	return 0
}

func bytes(rl corev1.ResourceList, name corev1.ResourceName) int64 {
	if q, ok := rl[name]; ok {
		return q.Value()
	}
	return 0
}

// SkipNamespace reports whether a namespace is cluster infrastructure that a
// first report should not clutter itself with.
func SkipNamespace(ns string) bool {
	return strings.HasPrefix(ns, "kube-") || ns == "local-path-storage"
}
