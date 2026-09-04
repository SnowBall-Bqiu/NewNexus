package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestHTTPClientRetriesRetryAfter(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"limited"}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	client := NewHTTPClient()
	var result struct {
		OK bool `json:"ok"`
	}
	if err := client.JSON(context.Background(), http.MethodGet, server.URL, nil, nil, &result); err != nil {
		t.Fatal(err)
	}
	if !result.OK || attempts.Load() != 2 {
		t.Fatalf("unexpected result=%+v attempts=%d", result, attempts.Load())
	}
}

func TestHTTPClientDoesNotRetryUnsafePost(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	client := NewHTTPClient()
	err := client.JSON(context.Background(), http.MethodPost, server.URL, nil, map[string]string{"name": "example"}, nil)
	if err == nil || attempts.Load() != 1 {
		t.Fatalf("unsafe POST should not retry: err=%v attempts=%d", err, attempts.Load())
	}
}

func TestHTTPClientRetryPolicyOverridesTransportMethod(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()
	client := NewHTTPClient()
	client.MinInterval = 0
	var output map[string]any
	if err := client.JSONWithRetryPolicy(context.Background(), http.MethodPost, server.URL, nil, map[string]string{"query": "safe"}, &output, true); err != nil {
		t.Fatal(err)
	}
	if attempts.Load() != 2 {
		t.Fatalf("expected safe POST to retry once, got %d attempts", attempts.Load())
	}

	attempts.Store(0)
	if err := client.JSONWithRetryPolicy(context.Background(), http.MethodGet, server.URL, nil, nil, &output, false); err == nil {
		t.Fatal("expected unsafe GET-style write to fail without retry")
	}
	if attempts.Load() != 1 {
		t.Fatalf("expected unsafe GET-style write not to retry, got %d attempts", attempts.Load())
	}
}

func TestHTTPClientRedactsProviderSecrets(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"message":"bad token secret-value","api_token":"secret-value"}`))
	}))
	defer server.Close()

	client := NewHTTPClient()
	err := client.JSON(context.Background(), http.MethodGet, server.URL, map[string]string{"Authorization": "secret-value"}, nil, nil)
	if err == nil || strings.Contains(err.Error(), "secret-value") {
		t.Fatalf("provider error leaked a secret: %v", err)
	}
}
