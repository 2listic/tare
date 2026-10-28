package metrics

import (
	"bytes"
	"context"
	"errors"
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

// Describe names the endpoint with any password masked. Basic auth in the URL
// is the only way this transport authenticates, and the description goes into
// the report header, the JSON, the HTML and the PR body: files people are
// told to send to someone else.
func (t *HTTPTransport) Describe() string {
	if u, err := url.Parse(t.BaseURL); err == nil {
		return u.Redacted()
	}
	return t.BaseURL
}

// maxResponse bounds one query's body. The largest real answer is one sample
// per container; a response past this is a misdirected endpoint, not data.
const maxResponse = 256 << 20

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
		// The client's error repeats the full URL, password included.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, fmt.Errorf("querying %s: %w", t.Describe(), err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return nil, fmt.Errorf("reading from %s: %w", t.Describe(), err)
	}
	// Prometheus answers a bad query with a 4xx and a JSON error that the
	// caller reports better than this could. Anything else that is not a 2xx
	// - a 401 from an auth proxy, Grafana's HTML login page - used to surface
	// as "invalid character '<'", which names neither the status nor the fix.
	if resp.StatusCode/100 != 2 && !bytes.HasPrefix(bytes.TrimSpace(body), []byte(`{"status"`)) {
		return nil, fmt.Errorf("querying %s: HTTP %s; is this a Prometheus API endpoint, and does it need credentials?",
			t.Describe(), resp.Status)
	}
	return body, nil
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
