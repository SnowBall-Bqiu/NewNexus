package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"strings"
	"testing"

	"github.com/newnexus/newnexus/internal/agenttls"
	"github.com/newnexus/newnexus/internal/domain"
	"github.com/newnexus/newnexus/internal/security"
)

func TestAgentSelfSignedTLSSettingsAndPublicCA(t *testing.T) {
	s := testServer(t)
	s.AgentPublicAddr = "127.0.0.1:9091"
	s.AgentInsecureLoopback = true
	cookie, csrf := setup(t, s)
	readTLS := func() domain.Doc {
		t.Helper()
		w := request(t, s, "GET", "nodes/installation", nil, cookie, "")
		if w.Code != http.StatusOK {
			t.Fatal(w.Code, w.Body.String())
		}
		result := domain.Doc{}
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	assertDisabled := func() {
		t.Helper()
		metadata := readTLS()
		tlsInfo := domain.M(metadata, "tls")
		if len(tlsInfo) != 1 || domain.B(tlsInfo, "self_signed_enabled") || !domain.B(metadata, "insecure_loopback") {
			t.Fatalf("disabled TLS leaked CA metadata or changed loopback fallback: %+v", metadata)
		}
		for _, method := range []string{"GET", "HEAD"} {
			w := request(t, s, method, "agent-ca.pem", nil, nil, "")
			if w.Code != http.StatusNotFound || w.Header().Get("Cache-Control") != "no-store" || strings.Contains(w.Body.String(), "BEGIN CERTIFICATE") {
				t.Fatal("disabled CA remained public or cacheable", w.Code, w.Body.String())
			}
		}
	}
	assertDisabled()
	if s.Store.Count(context.Background(), "agent-tls") != 0 {
		t.Fatal("reading disabled installation metadata generated private material")
	}
	update := domain.Doc{agenttls.SettingKey: true}
	if w := request(t, s, "PUT", "settings", update, nil, ""); w.Code != http.StatusUnauthorized {
		t.Fatal("anonymous client changed Agent TLS", w.Code)
	}
	if w := request(t, s, "PUT", "settings", update, cookie, ""); w.Code != http.StatusForbidden {
		t.Fatal("Agent TLS update bypassed CSRF", w.Code)
	}
	for _, value := range []any{"true", "false", 1, nil, domain.Doc{"enabled": true}} {
		if w := request(t, s, "PUT", "settings", domain.Doc{agenttls.SettingKey: value}, cookie, csrf); w.Code != http.StatusBadRequest {
			t.Fatal("Agent TLS accepted a non-boolean setting", w.Code, w.Body.String())
		}
	}
	if w := request(t, s, "PUT", "settings", domain.Doc{agenttls.SettingKey: true, "unknown.setting": true}, cookie, csrf); w.Code != http.StatusBadRequest {
		t.Fatal(w.Code, w.Body.String())
	}
	if enabled, err := s.AgentTLS.Enabled(context.Background()); err != nil || enabled || s.Store.Count(context.Background(), "agent-tls") != 0 {
		t.Fatal("invalid settings batch enabled TLS or generated certificates", enabled, err)
	}
	if w := request(t, s, "PUT", "settings", update, cookie, csrf); w.Code != http.StatusOK {
		t.Fatal(w.Code, w.Body.String())
	}
	metadata := readTLS()
	tlsInfo := domain.M(metadata, "tls")
	if !domain.B(tlsInfo, "self_signed_enabled") || domain.B(metadata, "insecure_loopback") || domain.S(tlsInfo, "ca_url") != "/api/v1/agent-ca.pem" || domain.S(tlsInfo, "server_name") == "" || domain.S(tlsInfo, "not_after") == "" {
		t.Fatalf("enabled TLS metadata is incomplete or still advertises plaintext: %+v", metadata)
	}
	ca := request(t, s, "GET", "agent-ca.pem", nil, nil, "")
	if ca.Code != http.StatusOK || ca.Header().Get("Content-Type") != "application/x-pem-file" || ca.Header().Get("Cache-Control") != "no-store" || !strings.Contains(ca.Header().Get("Content-Disposition"), "newnexus-agent-ca.pem") {
		t.Fatal("invalid public CA response", ca.Code, ca.Header())
	}
	block, rest := pem.Decode(ca.Body.Bytes())
	if block == nil || block.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) != 0 {
		t.Fatal("CA endpoint must contain exactly one certificate and no private material")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil || !certificate.IsCA || certificate.KeyUsage&x509.KeyUsageCertSign == 0 {
		t.Fatal("CA endpoint returned a leaf or invalid certificate", err)
	}
	pemHash, derHash := sha256.Sum256(ca.Body.Bytes()), sha256.Sum256(block.Bytes)
	if domain.S(tlsInfo, "ca_sha256") != hex.EncodeToString(pemHash[:]) || domain.S(tlsInfo, "ca_fingerprint_sha256") != hex.EncodeToString(derHash[:]) {
		t.Fatal("CA metadata hashes do not match the public CA certificate", tlsInfo)
	}
	head := request(t, s, "HEAD", "agent-ca.pem", nil, nil, "")
	if head.Code != http.StatusOK || head.Body.Len() != 0 || head.Header().Get("Content-Length") == "" {
		t.Fatal("CA HEAD response invalid", head.Code, head.Header())
	}
	if w := request(t, s, "POST", "agent-ca.pem", nil, nil, ""); w.Code != http.StatusMethodNotAllowed {
		t.Fatal("CA endpoint permits mutation", w.Code)
	}
	settings := request(t, s, "GET", "settings", nil, cookie, "")
	publicPayload := settings.Body.String()
	if settings.Code != http.StatusOK || strings.Contains(publicPayload, "PRIVATE KEY") || strings.Contains(publicPayload, "BEGIN CERTIFICATE") || strings.Contains(publicPayload, "ca_private") || strings.Contains(publicPayload, "leaf_private") {
		t.Fatal("settings exposed TLS material", settings.Code)
	}
	if w := request(t, s, "GET", "agent-tls/authority", nil, cookie, ""); w.Code != http.StatusNotFound {
		t.Fatal("private Agent TLS resource became REST-addressable", w.Code)
	}
	// A new API instance reads persisted settings/material rather than generating
	// another CA. Existing nodes can keep trusting the certificate across restarts.
	restarted := New(s.Store, s.Vault, s.Hub, rejectingOperations{})
	restarted.Auth.SecureCookie = false
	if w := request(t, restarted, "GET", "agent-ca.pem", nil, nil, ""); w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), ca.Body.Bytes()) {
		t.Fatal("restarted API changed the persisted CA", w.Code)
	}
	if w := request(t, s, "PUT", "settings", domain.Doc{agenttls.SettingKey: false}, cookie, csrf); w.Code != http.StatusOK {
		t.Fatal(w.Code, w.Body.String())
	}
	assertDisabled()
	if s.Store.Count(context.Background(), "agent-tls") == 0 {
		t.Fatal("disabling TLS destroyed existing trust material")
	}
	if w := request(t, s, "PUT", "settings", update, cookie, csrf); w.Code != http.StatusOK {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := request(t, s, "GET", "agent-ca.pem", nil, nil, ""); w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), ca.Body.Bytes()) {
		t.Fatal("reenabling TLS replaced the trusted CA", w.Code)
	}
}

func TestAgentTLSPreparationFailureDoesNotEnableSetting(t *testing.T) {
	s := testServer(t)
	cookie, csrf := setup(t, s)
	if _, err := s.AgentTLS.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	wrongVault, err := security.NewVault(bytes.Repeat([]byte{19}, 32))
	if err != nil {
		t.Fatal(err)
	}
	s.AgentTLS = agenttls.New(s.Store, wrongVault)
	w := request(t, s, "PUT", "settings", domain.Doc{agenttls.SettingKey: true}, cookie, csrf)
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "agent_tls_unavailable") || strings.Contains(w.Body.String(), "PRIVATE KEY") {
		t.Fatal("failed certificate preparation did not return a safe error", w.Code, w.Body.String())
	}
	row, err := s.Store.Get(context.Background(), "settings", agenttls.SettingKey)
	if err != nil || domain.B(row, "value") {
		t.Fatal("certificate preparation failure persisted an enabled setting", row, err)
	}
	if w := request(t, s, "GET", "agent-ca.pem", nil, nil, ""); w.Code != http.StatusNotFound {
		t.Fatal("failed enable published an unavailable CA", w.Code)
	}
}
