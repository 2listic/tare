package metrics

import (
	"context"
	"fmt"
	"net/url"

	"k8s.io/client-go/kubernetes"
)

// ProxyTransport reaches an in-cluster Prometheus through the Kubernetes API
// server's service proxy, the same mechanism `kubectl proxy` uses.
//
// This is what makes the tool work with no setup: any cluster the user can
// already reach with kubectl, they can query Prometheus in, without a port
// forward, an ingress, or a VPN. The cost is that traffic goes through the API
// server, so it is not the right choice for very large queries.
type ProxyTransport struct {
	Clientset kubernetes.Interface
	Namespace string
	Service   string
	Port      string
	Scheme    string
}

func (t *ProxyTransport) Describe() string {
	return fmt.Sprintf("apiserver-proxy://%s/%s:%s", t.Namespace, t.Service, t.Port)
}

func (t *ProxyTransport) Get(ctx context.Context, path string, params url.Values) ([]byte, error) {
	scheme := t.Scheme
	if scheme == "" {
		scheme = "http"
	}
	req := t.Clientset.CoreV1().Services(t.Namespace).
		ProxyGet(scheme, t.Service, t.Port, path, paramMap(params))
	b, err := req.DoRaw(ctx)
	if err != nil {
		return nil, fmt.Errorf("querying %s: %w", t.Describe(), err)
	}
	return b, nil
}

func paramMap(v url.Values) map[string]string {
	m := make(map[string]string, len(v))
	for k, vs := range v {
		if len(vs) > 0 {
			m[k] = vs[0]
		}
	}
	return m
}
