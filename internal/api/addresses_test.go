package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/newnexus/newnexus/internal/agenthub"
	"github.com/newnexus/newnexus/internal/domain"
	"github.com/newnexus/newnexus/internal/store"
)

func TestAddressSettingsInheritancePersistenceAndImmediateMetadata(t *testing.T) {
	s := testServer(t)
	s.AgentDownloadOrigin = "https://api.environment.example"
	s.AgentPublicAddr = "127.0.0.1:9091"
	s.AgentInsecureLoopback = true
	s.ConsoleOrigin = "https://console.environment.example"
	cookie, csrf := setup(t, s)
	readSettings := func(server *Server) map[string]domain.Doc {
		t.Helper()
		w := request(t, server, "GET", "settings", nil, cookie, "")
		if w.Code != http.StatusOK {
			t.Fatal(w.Code, w.Body.String())
		}
		payload := domain.Doc{}
		if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		out := map[string]domain.Doc{}
		for _, row := range domain.A(payload, "items") {
			out[domain.S(row, "key")] = row
		}
		return out
	}
	readMetadata := func(server *Server) domain.Doc {
		t.Helper()
		w := request(t, server, "GET", "nodes/installation", nil, cookie, "")
		if w.Code != http.StatusOK {
			t.Fatal(w.Code, w.Body.String())
		}
		out := domain.Doc{}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	initial := readSettings(s)
	for _, key := range addressSettingKeys {
		row := initial[key]
		if domain.S(row, "value") != "" || domain.S(row, "effective_value") != s.addressFallback(key) || domain.S(row, "source") != "runtime" {
			t.Fatalf("incorrect inherited setting %s: %+v", key, row)
		}
		stored, err := s.Store.Get(context.Background(), "settings", key)
		if err != nil || domain.S(stored, "value") != "" {
			t.Fatalf("GET persisted an inherited runtime value: %+v %v", stored, err)
		}
	}
	initialMetadata := readMetadata(s)
	if domain.S(initialMetadata, "server") != s.AgentPublicAddr || domain.S(initialMetadata, "download_origin") != s.AgentDownloadOrigin || domain.S(initialMetadata, "console_origin") != s.ConsoleOrigin || !domain.B(initialMetadata, "insecure_loopback") {
		t.Fatalf("runtime fallback was not advertised: %+v", initialMetadata)
	}
	if w := request(t, s, "PUT", "settings", domain.Doc{"addresses.api_origin": "https://api.database.example"}, cookie, ""); w.Code != http.StatusForbidden {
		t.Fatalf("address setting mutation bypassed CSRF: %d", w.Code)
	}
	input := domain.Doc{"addresses.api_origin": " HTTPS://API.DATABASE.EXAMPLE:443/ ", "addresses.agent_endpoint": " Agents.Database.Example:0443 ", "addresses.console_origin": "https://console.database.example/"}
	if w := request(t, s, "PUT", "settings", input, cookie, csrf); w.Code != http.StatusOK {
		t.Fatal(w.Code, w.Body.String())
	}
	expected := map[string]string{"addresses.api_origin": "https://api.database.example", "addresses.agent_endpoint": "agents.database.example:443", "addresses.console_origin": "https://console.database.example"}
	for key, row := range readSettings(s) {
		if want, ok := expected[key]; ok && (domain.S(row, "value") != want || domain.S(row, "effective_value") != want || domain.S(row, "source") != "database") {
			t.Fatalf("saved setting did not become effective: %+v", row)
		}
	}
	metadata := readMetadata(s)
	if domain.S(metadata, "server") != expected["addresses.agent_endpoint"] || domain.S(metadata, "download_origin") != expected["addresses.api_origin"] || domain.S(metadata, "console_origin") != expected["addresses.console_origin"] || domain.B(metadata, "insecure_loopback") {
		t.Fatalf("saved settings were not applied immediately or weakened TLS: %+v", metadata)
	}
	if s.Store.Count(context.Background(), "audit") == 0 {
		t.Fatal("address setting update was not audited")
	}
	// Reopen the same persisted database and construct a fresh service with
	// different environment defaults, as a control-plane restart would.
	dsn := s.Store.DSN
	if err := s.Store.DB.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.DB.Close() })
	restarted := New(db, s.Vault, agenthub.New(db), rejectingOperations{})
	restarted.Auth.SecureCookie = false
	restarted.AgentDownloadOrigin = "https://api.restart.example"
	restarted.AgentPublicAddr = "127.0.0.1:9092"
	restarted.AgentInsecureLoopback = true
	restarted.ConsoleOrigin = "https://console.restart.example"
	if err := restarted.EnsureDefaults(context.Background()); err != nil {
		t.Fatal(err)
	}
	metadata = readMetadata(restarted)
	if domain.S(metadata, "server") != expected["addresses.agent_endpoint"] || domain.S(metadata, "download_origin") != expected["addresses.api_origin"] || domain.S(metadata, "console_origin") != expected["addresses.console_origin"] || domain.B(metadata, "insecure_loopback") {
		t.Fatalf("restart lost persisted address overrides: %+v", metadata)
	}
	cleared := domain.Doc{"addresses.api_origin": "", "addresses.agent_endpoint": "", "addresses.console_origin": ""}
	if w := request(t, restarted, "PUT", "settings", cleared, cookie, csrf); w.Code != http.StatusOK {
		t.Fatal(w.Code, w.Body.String())
	}
	metadata = readMetadata(restarted)
	if domain.S(metadata, "server") != restarted.AgentPublicAddr || domain.S(metadata, "download_origin") != restarted.AgentDownloadOrigin || domain.S(metadata, "console_origin") != restarted.ConsoleOrigin || !domain.B(metadata, "insecure_loopback") {
		t.Fatalf("clearing overrides did not restore runtime inheritance: %+v", metadata)
	}
	for _, key := range addressSettingKeys {
		row := readSettings(restarted)[key]
		if domain.S(row, "value") != "" || domain.S(row, "source") != "runtime" || domain.S(row, "effective_value") != restarted.addressFallback(key) {
			t.Fatalf("cleared setting pinned environment defaults: %+v", row)
		}
	}
}

func TestAddressSettingsValidationRejectsUnsafeAndMalformedValues(t *testing.T) {
	s := testServer(t)
	s.DevAuth = true
	for _, test := range []struct {
		key   string
		value any
	}{
		{"addresses.api_origin", "javascript:alert(1)"},
		{"addresses.api_origin", "https://user:secret@api.example.com"},
		{"addresses.api_origin", "https://api.example.com/path"},
		{"addresses.api_origin", "https://api.example.com/?token=secret"},
		{"addresses.api_origin", "https://api.example.com/#fragment"},
		{"addresses.api_origin", nil},
		{"addresses.api_origin", true},
		{"addresses.console_origin", "ftp://console.example.com"},
		{"addresses.console_origin", "https://user@console.example.com"},
		{"addresses.console_origin", "https://console.example.com/login"},
		{"addresses.console_origin", "https://console.example.com/?"},
		{"addresses.console_origin", "https://console.example.com/#"},
		{"addresses.console_origin", 443},
		{"addresses.agent_endpoint", "https://agents.example.com:443"},
		{"addresses.agent_endpoint", "user:secret@agents.example.com:443"},
		{"addresses.agent_endpoint", "agents.example.com:443/path"},
		{"addresses.agent_endpoint", "agents.example.com"},
		{"addresses.agent_endpoint", "agents.example.com:0"},
		{"addresses.agent_endpoint", "agents.example.com:65536"},
		{"addresses.agent_endpoint", "agents.example.com:-1"},
		{"addresses.agent_endpoint", "0.0.0.0:9090"},
		{"addresses.agent_endpoint", "[::]:9090"},
		{"addresses.agent_endpoint", ":9090"},
		{"addresses.agent_endpoint", "agents .example.com:443"},
		{"addresses.agent_endpoint", "agents.example.com\r\n:443"},
		{"addresses.agent_endpoint", "[invalid-ip]:443"},
		{"addresses.agent_endpoint", nil},
		{"addresses.agent_endpoint", domain.Doc{"host": "agents.example.com"}},
	} {
		w := request(t, s, "PUT", "settings", domain.Doc{test.key: test.value}, nil, "")
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "invalid_setting") {
			t.Fatalf("invalid value for %s accepted: %d %s", test.key, w.Code, w.Body.String())
		}
		stored, err := s.Store.Get(context.Background(), "settings", test.key)
		if err != nil || domain.S(stored, "value") != "" {
			t.Fatalf("invalid value was persisted for %s: %+v %v", test.key, stored, err)
		}
	}
	// Validate the entire payload before persisting any valid address in it.
	w := request(t, s, "PUT", "settings", domain.Doc{"addresses.api_origin": "https://valid.example.com", "addresses.agent_endpoint": "invalid"}, nil, "")
	if w.Code != http.StatusBadRequest {
		t.Fatal(w.Code, w.Body.String())
	}
	stored, err := s.Store.Get(context.Background(), "settings", "addresses.api_origin")
	if err != nil || domain.S(stored, "value") != "" {
		t.Fatal("invalid batch partially updated address settings", stored, err)
	}
}

func TestAddressAgentEndpointOverrideNeverInheritsPlaintextForDifferentTarget(t *testing.T) {
	s := testServer(t)
	s.DevAuth = true
	s.AgentPublicAddr = "127.0.0.1:9091"
	s.AgentInsecureLoopback = true
	for _, endpoint := range []string{"127.0.0.1:9443", "127.0.0.2:9091", "localhost:9091", "agents.example.com:443"} {
		if w := request(t, s, "PUT", "settings", domain.Doc{"addresses.agent_endpoint": endpoint}, nil, ""); w.Code != http.StatusOK {
			t.Fatal(w.Code, w.Body.String())
		}
		w := request(t, s, "GET", "nodes/installation", nil, nil, "")
		payload := domain.Doc{}
		if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		if w.Code != http.StatusOK || domain.S(payload, "server") != endpoint || domain.B(payload, "insecure_loopback") {
			t.Fatalf("overridden target inherited plaintext: %+v", payload)
		}
	}
	if w := request(t, s, "GET", "settings", nil, nil, ""); !strings.Contains(w.Body.String(), `"source":"unset"`) {
		t.Fatal("empty runtime origins did not report unset inheritance", w.Code, w.Body.String())
	}
}
