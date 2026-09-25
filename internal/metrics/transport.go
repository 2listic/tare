package metrics

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Transport fetches a Prometheus API path. Two implementations exist because
// the common case for a kubectl plugin is a Prometheus that is not reachable
// from the user's laptop at all: a ClusterIP service has no route from outside
// the cluster, so requiring a URL means requiring a port-forward or an ingress
// before the tool does anything useful.
type Transport interface {
	Get(ctx context.Context, path string, params url.Values) ([]byte, error)
	Describe() string
}

// HTTPTransport talks to a Prometheus reachable over the network: a port
// forward, an ingress, or a managed endpoint.
type HTTPTransport struct {
	BaseURL string
	Client  *http.Client
}

func NewHTTPTransport(baseURL string) *HTTPTransport {
	return &HTTPTransport{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Client:  &http.Client{Timeout: 60 * time.Second},
	}
}

func (t *HTTPTransport) Describe() string { return t.BaseURL }

func (t *HTTPTransport) Get(ctx context.Context, path string, params url.Values) ([]byte, error) {
	u := t.BaseURL + path
	if len(params) > 0 {
		u += "?" + params.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := t.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("querying %s: %w", t.BaseURL, err)
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}
