package discover

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func svc(ns, name string, labels map[string]string, ports ...corev1.ServicePort) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: labels},
		Spec:       corev1.ServiceSpec{Ports: ports},
	}
}

func port(name string, n int32) corev1.ServicePort {
	return corev1.ServicePort{Name: name, Port: n}
}

func TestClassify(t *testing.T) {
	tests := []struct {
		name     string
		svc      *corev1.Service
		wantOK   bool
		wantPort string
	}{
		{
			name:     "prometheus-operator service by label",
			svc:      svc("monitoring", "prometheus-operated", map[string]string{"operated-prometheus": "true"}, port("http-web", 9090)),
			wantOK:   true,
			wantPort: "http-web",
		},
		{
			name:     "renamed helm release still found by label",
			svc:      svc("obs", "acme-metrics", map[string]string{"app.kubernetes.io/name": "prometheus"}, port("http", 9090)),
			wantOK:   true,
			wantPort: "http",
		},
		{
			name:     "thanos query by name",
			svc:      svc("thanos", "thanos-query", nil, port("http", 10902)),
			wantOK:   true,
			wantPort: "http",
		},
		{
			name:     "unnamed port matched by well-known number",
			svc:      svc("monitoring", "prometheus", nil, port("", 9090)),
			wantOK:   true,
			wantPort: "9090",
		},
		{
			name:   "an unrelated service is not a metrics endpoint",
			svc:    svc("prod", "checkout", map[string]string{"app": "checkout"}, port("http", 8080)),
			wantOK: false,
		},
		{
			name:   "prometheus service with no usable port",
			svc:    svc("monitoring", "prometheus", nil, port("grpc", 10901)),
			wantOK: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := classify(tc.svc)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if got.Port != tc.wantPort {
				t.Errorf("port = %q, want %q", got.Port, tc.wantPort)
			}
			if got.Why == "" {
				t.Error("Why must be set: the user has to be able to see why this service was picked")
			}
		})
	}
}

// TestGRPCQueryPortIsSkipped: Thanos exposes both an HTTP query API and a gRPC
// StoreAPI. Picking the gRPC port produces a confusing connection failure
// rather than a useful error.
func TestGRPCQueryPortIsSkipped(t *testing.T) {
	s := svc("thanos", "thanos-query", nil, port("grpc-query", 10901), port("http", 10902))
	got, ok := classify(s)
	if !ok {
		t.Fatal("thanos-query should be recognised")
	}
	if got.Port == "grpc-query" {
		t.Error("picked the gRPC port, which does not serve the HTTP query API")
	}
}

// TestLongRetentionPreferred: this tool wants weeks of history and a default
// Prometheus keeps about fifteen days, so a Thanos or Mimir in the same cluster
// is the better source.
func TestLongRetentionPreferred(t *testing.T) {
	prom, _ := classify(svc("monitoring", "prometheus-operated", map[string]string{"operated-prometheus": "true"}, port("http-web", 9090)))
	thanos, _ := classify(svc("thanos", "thanos-query", nil, port("http", 10902)))
	if thanos.Rank >= prom.Rank {
		t.Errorf("thanos rank %d should sort before prometheus rank %d", thanos.Rank, prom.Rank)
	}
}
