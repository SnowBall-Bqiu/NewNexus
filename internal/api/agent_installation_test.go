package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNodeInstallationMetadataAuthenticationAndChecksums(t *testing.T) {
	s := testServer(t)
	s.AgentDownloadDir = t.TempDir()
	s.AgentPublicAddr = "grpc.example.com:9443"
	s.AgentDownloadOrigin = "https://downloads.example.com"
	for _, arch := range agentArchitectures {
		if err := os.WriteFile(filepath.Join(s.AgentDownloadDir, "newnexus-agent-linux-"+arch), []byte("agent-"+arch), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if w := request(t, s, "GET", "nodes/installation", nil, nil, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("installation metadata bypassed authentication: %d", w.Code)
	}
	cookie, _ := setup(t, s)
	readMetadata := func() []agentArtifact {
		t.Helper()
		w := request(t, s, "GET", "nodes/installation", nil, cookie, "")
		if w.Code != http.StatusOK {
			t.Fatal(w.Code, w.Body.String())
		}
		var response struct {
			Server           string          `json:"server"`
			DownloadOrigin   string          `json:"download_origin"`
			InsecureLoopback bool            `json:"insecure_loopback"`
			Artifacts        []agentArtifact `json:"artifacts"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if response.Server != s.AgentPublicAddr || response.DownloadOrigin != s.AgentDownloadOrigin || response.InsecureLoopback != s.AgentInsecureLoopback {
			t.Fatalf("incorrect advertised gateway: %+v", response)
		}
		if strings.Contains(w.Body.String(), s.AgentDownloadDir) {
			t.Fatal("metadata exposed local filesystem path")
		}
		return response.Artifacts
	}
	artifacts := readMetadata()
	if len(artifacts) != 2 {
		t.Fatalf("expected both Linux architectures: %+v", artifacts)
	}
	for _, artifact := range artifacts {
		hash := sha256.Sum256([]byte("agent-" + artifact.Arch))
		if artifact.URL != "/api/v1/agent-downloads/linux-"+artifact.Arch || artifact.SHA256 != hex.EncodeToString(hash[:]) {
			t.Fatalf("incorrect artifact: %+v", artifact)
		}
	}
	// Replacing an artifact must update its checksum even at the same file size.
	if err := os.WriteFile(filepath.Join(s.AgentDownloadDir, "newnexus-agent-linux-amd64"), []byte("AGENT-amd64"), 0600); err != nil {
		t.Fatal(err)
	}
	s.AgentInsecureLoopback = true
	updated := readMetadata()
	if updated[0].SHA256 == artifacts[0].SHA256 {
		t.Fatal("artifact replacement retained stale checksum")
	}
	if err := os.Remove(filepath.Join(s.AgentDownloadDir, "newnexus-agent-linux-arm64")); err != nil {
		t.Fatal(err)
	}
	if artifacts := readMetadata(); len(artifacts) != 1 || artifacts[0].Arch != "amd64" {
		t.Fatalf("missing artifact was advertised: %+v", artifacts)
	}
	if err := os.Remove(filepath.Join(s.AgentDownloadDir, "newnexus-agent-linux-amd64")); err != nil {
		t.Fatal(err)
	}
	if artifacts := readMetadata(); len(artifacts) != 0 || artifacts == nil {
		t.Fatalf("empty installation must advertise an empty array: %+v", artifacts)
	}
}

func TestNodeInstallationDownloadOriginNeverUsesRequestHeaders(t *testing.T) {
	s := testServer(t)
	s.AgentPublicAddr = "agents.example.com:9443"
	cookie, _ := setup(t, s)
	r := httptest.NewRequest("GET", "http://local-console/api/v1/nodes/installation", nil)
	r.AddCookie(cookie)
	r.Header.Set("Origin", "https://attacker.example")
	r.Header.Set("Forwarded", "host=attacker.example;proto=https")
	r.Header.Set("X-Forwarded-Host", "attacker.example")
	r.Header.Set("X-Forwarded-Proto", "https")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatal(w.Code, w.Body.String())
	}
	var metadata struct {
		Server         string `json:"server"`
		DownloadOrigin string `json:"download_origin"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &metadata); err != nil {
		t.Fatal(err)
	}
	if metadata.Server != s.AgentPublicAddr || metadata.DownloadOrigin != "" || strings.Contains(w.Body.String(), "attacker.example") || strings.Contains(w.Body.String(), "local-console") {
		t.Fatalf("metadata inferred download origin from untrusted request headers: %s", w.Body.String())
	}
}

func TestPublicAgentDownloadRangesAndWhitelist(t *testing.T) {
	s := testServer(t)
	s.AgentDownloadDir = t.TempDir()
	if err := os.WriteFile(filepath.Join(s.AgentDownloadDir, "newnexus-agent-linux-amd64"), []byte("agent-binary"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.AgentDownloadDir, "master.key"), []byte("private-key"), 0600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, method, path, byteRange string
		status                        int
		body                          string
	}{
		{"public binary", "GET", "linux-amd64", "", 200, "agent-binary"},
		{"public head", "HEAD", "linux-amd64", "", 200, ""},
		{"resume binary", "GET", "linux-amd64", "bytes=2-5", 206, "ent-"},
		{"no write", "POST", "linux-amd64", "", 405, ""},
		{"missing binary", "GET", "linux-arm64", "", 404, ""},
		{"unsupported architecture", "GET", "linux-386", "", 404, ""},
		{"private file", "GET", "master.key", "", 404, ""},
		{"path traversal", "GET", "../master.key", "", 404, ""},
		{"no directory listing", "GET", "", "", 404, ""},
		{"no suffix", "GET", "linux-amd64/extra", "", 404, ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			r := httptest.NewRequest(test.method, "http://console/api/v1/agent-downloads/"+test.path, nil)
			r.Header.Set("Range", test.byteRange)
			w := httptest.NewRecorder()
			s.ServeHTTP(w, r)
			if w.Code != test.status {
				t.Fatalf("status %d, want %d: %s", w.Code, test.status, w.Body.String())
			}
			if test.status < 300 {
				if w.Body.String() != test.body || w.Header().Get("Content-Type") != "application/octet-stream" || !strings.Contains(w.Header().Get("Content-Disposition"), "newnexus-agent-linux-amd64") {
					t.Fatalf("invalid binary response: %v %q", w.Header(), w.Body.String())
				}
			}
			if strings.Contains(w.Body.String(), "private-key") || strings.Contains(w.Body.String(), s.AgentDownloadDir) {
				t.Fatal("download error exposed private filesystem data")
			}
		})
	}
}
