package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/SnowBall-Bqiu/NewNexus/internal/config"
)

func TestHSTSUsesForwardedProtoOnlyFromTrustedProxy(t *testing.T) {
	server := &Server{cfg: config.Config{EnableHSTS: true, TrustedProxyCIDRs: "10.0.0.0/8"}}
	handler := server.realIP(server.securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})))

	trusted := httptest.NewRequest(http.MethodGet, "http://newnexus.example/healthz", nil)
	trusted.RemoteAddr = "10.1.2.3:12345"
	trusted.Header.Set("X-Forwarded-Proto", "https")
	trustedRecorder := httptest.NewRecorder()
	handler.ServeHTTP(trustedRecorder, trusted)
	if trustedRecorder.Header().Get("Strict-Transport-Security") == "" {
		t.Fatal("trusted HTTPS proxy did not enable HSTS")
	}

	untrusted := httptest.NewRequest(http.MethodGet, "http://newnexus.example/healthz", nil)
	untrusted.RemoteAddr = "192.0.2.10:12345"
	untrusted.Header.Set("X-Forwarded-Proto", "https")
	untrustedRecorder := httptest.NewRecorder()
	handler.ServeHTTP(untrustedRecorder, untrusted)
	if untrustedRecorder.Header().Get("Strict-Transport-Security") != "" {
		t.Fatal("untrusted client spoofed X-Forwarded-Proto to enable HSTS")
	}
}
