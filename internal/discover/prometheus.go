package discover

import (
	"context"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// PromCandidate is a metrics service found in the cluster.
type PromCandidate struct {
	Namespace string
	Service   string
	Port      string
	Scheme    string
	// Why records how it was identified, so a user can see why the tool picked
	// this one and correct it with a flag if it guessed wrong.
	Why string
	// Rank orders candidates; lower is preferred.
	Rank int
}

func (c PromCandidate) String() string {
	return fmt.Sprintf("%s/%s:%s (%s)", c.Namespace, c.Service, c.Port, c.Why)
}

// knownServices are the service names the common installations create. Name
// matching is a fallback: label matching is tried first because it survives
// people renaming their release.
var knownServices = map[string]struct {
	rank int
	why  string
}{
	"prometheus-operated":              {10, "prometheus-operator"},
	"prometheus-k8s":                   {10, "kube-prometheus"},
	"kube-prometheus-stack-prometheus": {10, "kube-prometheus-stack"},
	"prometheus-server":                {20, "prometheus helm chart"},
	"prometheus":                       {30, "name match"},
	"thanos-query":                     {5, "thanos, longer retention than prometheus"},
	"thanos-query-frontend":            {5, "thanos query frontend"},
	"thanos-querier":                   {5, "thanos querier"},
	"mimir-query-frontend":             {5, "grafana mimir"},
	"vmselect":                         {5, "victoriametrics select"},
	"victoria-metrics-single-server":   {15, "victoriametrics single"},
	"vmsingle":                         {15, "victoriametrics single"},
}

// FindPrometheus looks for a Prometheus-compatible service in the cluster.
//
// It exists so the first run needs no arguments. Requiring a URL means
// requiring a port-forward or an ingress before the tool does anything, which
// is a poor trade when the cluster already knows where its own Prometheus is.
//
// Long-retention backends are preferred over Prometheus itself: this tool wants
// weeks of history, and a default Prometheus keeps about fifteen days.
func (c *Client) FindPrometheus(ctx context.Context) ([]PromCandidate, error) {
	svcs, err := c.cs.CoreV1().Services(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing services: %w\n"+
			"  tare looks for the cluster's own Prometheus by listing services, which this token cannot do.\n"+
			"  Name the endpoint instead:  kubectl tare report --prometheus http://localhost:9090\n"+
			"  (with kubectl port-forward svc/prometheus 9090 running, if it is not otherwise reachable)", err)
	}

	var out []PromCandidate
	for i := range svcs.Items {
		s := &svcs.Items[i]
		if cand, ok := classify(s); ok {
			out = append(out, cand)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Rank != out[j].Rank {
			return out[i].Rank < out[j].Rank
		}
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Service < out[j].Service
	})
	return out, nil
}

func classify(s *corev1.Service) (PromCandidate, bool) {
	port, scheme, ok := metricsPort(s)
	if !ok {
		return PromCandidate{}, false
	}
	cand := PromCandidate{
		Namespace: s.Namespace, Service: s.Name, Port: port, Scheme: scheme,
	}

	// Labels first: they survive a renamed Helm release, a service name does not.
	switch s.Labels["app.kubernetes.io/name"] {
	case "prometheus":
		cand.Rank, cand.Why = 10, "label app.kubernetes.io/name=prometheus"
		return cand, true
	case "thanos-query", "thanos-query-frontend":
		cand.Rank, cand.Why = 5, "label app.kubernetes.io/name=thanos-query"
		return cand, true
	case "mimir":
		cand.Rank, cand.Why = 5, "label app.kubernetes.io/name=mimir"
		return cand, true
	}
	if s.Labels["app"] == "prometheus" || s.Labels["operated-prometheus"] == "true" {
		cand.Rank, cand.Why = 12, "label app=prometheus"
		return cand, true
	}

	if k, ok := knownServices[s.Name]; ok {
		cand.Rank, cand.Why = k.rank, k.why
		return cand, true
	}
	return PromCandidate{}, false
}

// metricsPort picks the port to query. Named ports are preferred because a
// service may expose several and only one speaks the query API.
func metricsPort(s *corev1.Service) (port, scheme string, ok bool) {
	for _, p := range s.Spec.Ports {
		switch strings.ToLower(p.Name) {
		case "http-web", "web", "http", "http-query", "query": // not grpc-query: gRPC does not serve the HTTP query API
			return p.Name, "http", true
		}
	}
	for _, p := range s.Spec.Ports {
		if p.Port == 9090 || p.Port == 8080 || p.Port == 10902 || p.Port == 8481 || p.Port == 8428 {
			name := p.Name
			if name == "" {
				name = fmt.Sprintf("%d", p.Port)
			}
			return name, "http", true
		}
	}
	return "", "", false
}

// Clientset exposes the underlying client so a caller can build a proxy
// transport against a discovered service.
func (c *Client) Clientset() kubernetes.Interface { return c.cs }
