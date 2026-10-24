package metrics

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
)

type failing struct{ err error }

func (f failing) Get(context.Context, string, url.Values) ([]byte, error) { return nil, f.err }
func (f failing) Describe() string                                        { return "apiserver-proxy://monitoring/prom:web" }

func TestScrubErrorsRewritesTextAndKeepsTheChain(t *testing.T) {
	p := New(failing{context.DeadlineExceeded})
	p.ScrubErrors(func(s string) string { return strings.ReplaceAll(s, "deadline", "XXX") })
	_, err := p.transport.Get(context.Background(), "/api/v1/query", nil)
	if err == nil || strings.Contains(err.Error(), "deadline") {
		t.Fatalf("error was not scrubbed: %v", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Error("scrubbing broke errors.Is: callers that check for a timeout stop recognising one")
	}
}
