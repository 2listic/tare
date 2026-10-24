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

// scrubbed passes every error through scrub, so a source built under
// --redact cannot print the endpoint it was asked to hide, whichever caller
// ends up printing the error. The original stays reachable for errors.Is.
type scrubbed struct {
	Transport
	scrub func(string) string
}

func (t scrubbed) Get(ctx context.Context, path string, params url.Values) ([]byte, error) {
	b, err := t.Transport.Get(ctx, path, params)
	if err != nil {
		return nil, scrubbedError{t.scrub(err.Error()), err}
	}
	return b, nil
}

type scrubbedError struct {
	msg string
	err error
}

func (e scrubbedError) Error() string { return e.msg }
func (e scrubbedError) Unwrap() error { return e.err }
