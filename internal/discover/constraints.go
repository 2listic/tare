package discover

import (
	"context"
	"strings"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Constraints are facts about a workload that limit what may be recommended
// for it. Ignoring them produces numbers that look fine and break something
// when applied: a Guaranteed pod silently demoted to Burstable, or a CPU
// request changed underneath an HPA that scales on CPU utilisation.
type Constraints struct {
	// Guaranteed means every container has requests equal to limits for both
	// CPU and memory. Changing that ratio changes the pod's QoS class, which
	// costs it eviction priority and, on clusters that support it, makes the
	// change ineligible for in-place resize.
	Guaranteed bool

	// HPAOnCPU and HPAOnMemory mean a HorizontalPodAutoscaler targets this
	// workload on that resource's utilisation. Utilisation is measured against
	// the request, so changing the request moves the scaling setpoint: halving
	// a request doubles measured utilisation and the workload scales out.
	HPAOnCPU    bool
	HPAOnMemory bool
	HPAName     string
	HPAMin      int32
	HPAMax      int32

	// VPAName and VPAMode record a VerticalPodAutoscaler already managing this
	// workload. Two controllers setting the same field is not a merge, it is a
	// fight.
	VPAName string
	VPAMode string
}

// Managed reports whether another controller is already setting resources.
func (c Constraints) Managed() bool {
	return c.VPAName != "" && !strings.EqualFold(c.VPAMode, "Off")
}

var vpaGVR = schema.GroupVersionResource{
	Group: "autoscaling.k8s.io", Version: "v1", Resource: "verticalpodautoscalers",
}

// LoadConstraints indexes HPAs and VPAs by the workload they target. A read
// that fails degrades to a warning rather than an error: a cluster without the
// VPA CRD installed is the common case.
func (c *Client) LoadConstraints(ctx context.Context, namespaces []string) map[string]Constraints {
	out := map[string]Constraints{}

	targets := scopeOf(namespaces)

	for _, ns := range targets {
		hpas, err := c.cs.AutoscalingV2().HorizontalPodAutoscalers(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			// Degrade rather than abort, and say so. An HPA scaling on CPU
			// utilisation measures against the request, so a resize moves its
			// setpoint; not being able to read them does not make that safe,
			// it makes it unverified, and the report has to carry that.
			c.warn("horizontal pod autoscalers in %s could not be read (%v): "+
				"if any of these workloads is scaled on CPU or memory utilisation, resizing it moves "+
				"the scaling setpoint and tare cannot warn you which ones", describeScope(ns), err)
			continue
		}
		for i := range hpas.Items {
			h := &hpas.Items[i]
			key := ConstraintKey(h.Namespace, h.Spec.ScaleTargetRef.Kind+"/"+h.Spec.ScaleTargetRef.Name)
			cs := out[key]
			cs.HPAName, cs.HPAMax = h.Name, h.Spec.MaxReplicas
			if h.Spec.MinReplicas != nil {
				cs.HPAMin = *h.Spec.MinReplicas
			}
			for _, m := range h.Spec.Metrics {
				if m.Type != autoscalingv2.ResourceMetricSourceType || m.Resource == nil {
					continue
				}
				// Only utilisation is request-relative; AverageValue is not.
				if m.Resource.Target.Type != autoscalingv2.UtilizationMetricType {
					continue
				}
				switch m.Resource.Name {
				case corev1.ResourceCPU:
					cs.HPAOnCPU = true
				case corev1.ResourceMemory:
					cs.HPAOnMemory = true
				}
			}
			out[key] = cs
		}
	}

	if c.dyn != nil {
		for _, ns := range targets {
			list, err := c.dyn.Resource(vpaGVR).Namespace(ns).List(ctx, metav1.ListOptions{})
			if err != nil {
				// A cluster without the VPA CRD is the common case and not
				// worth a word, and every namespace will say the same.
				if errors.IsNotFound(err) {
					break
				}
				// Being refused the read is a different thing: a VPA in Auto
				// mode writes the very fields tare recommends, and two
				// controllers writing one field is a fight. RBAC is per
				// namespace, so one refusal says nothing about the next one;
				// stopping here left every later namespace unscanned and
				// unreported.
				c.warn("vertical pod autoscalers in %s could not be read (%v): "+
					"tare cannot tell whether a VPA is already setting these resources",
					describeScope(ns), err)
				continue
			}
			for _, item := range list.Items {
				kind := unstructuredString(item.Object, "spec", "targetRef", "kind")
				name := unstructuredString(item.Object, "spec", "targetRef", "name")
				mode := unstructuredString(item.Object, "spec", "updatePolicy", "updateMode")
				if name == "" {
					continue
				}
				key := ConstraintKey(item.GetNamespace(), kind+"/"+name)
				cs := out[key]
				cs.VPAName, cs.VPAMode = item.GetName(), mode
				out[key] = cs
			}
		}
	}
	return out
}

// ConstraintKey is the index key LoadConstraints stores a workload's
// constraints under, from its namespace and "Kind/name" workload string.
//
// It is built from Workload rather than WorkloadKind, so that it is correct for
// a Container built by anything that sets the one field a workload is
// identified by. The simulator sets Workload and not WorkloadKind, and keying
// off the empty field silently missed every constraint.
func ConstraintKey(namespace, workload string) string {
	return namespace + "/" + workload
}

// unstructuredString reads a string field, or "" if any step of the path is
// missing or of the wrong type.
func unstructuredString(obj map[string]any, path ...string) string {
	cur := any(obj)
	for _, p := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur = m[p]
	}
	s, _ := cur.(string)
	return s
}
