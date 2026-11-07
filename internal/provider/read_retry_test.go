package provider

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type retryTransport func(*http.Request) (*http.Response, error)

func (f retryTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestOnlyReadOperationsRetryTransientTransportFailures(t *testing.T) {
	calls := 0
	a := New("cloudflare", nil)
	a.Client = &http.Client{Transport: retryTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return nil, io.ErrUnexpectedEOF
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"success":true,"result":[]}`)), Request: r}, nil
	})}
	req, _ := http.NewRequestWithContext(context.Background(), "GET", "https://api.example.com/zones", nil)
	if _, e := a.doJSON(req, "cloudflare", true); e != nil || calls != 2 {
		t.Fatal("safe read did not retry its transient transport error", calls, e)
	}
	calls = 0
	req, _ = http.NewRequestWithContext(context.Background(), "POST", "https://api.example.com/records", strings.NewReader(`{}`))
	_, e := a.doJSON(req, "cloudflare", false)
	var failure *Error
	if !errors.As(e, &failure) || !failure.Ambiguous || calls != 1 {
		t.Fatal("ambiguous mutation was retried", calls, e)
	}
}
