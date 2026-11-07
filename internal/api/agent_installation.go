package api

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"github.com/newnexus/newnexus/internal/agenttls"
	"io"
	"net/http"
	"os"
	"path/filepath"
)

var agentArchitectures = []string{"amd64", "arm64"}

type agentArtifact struct {
	Arch   string `json:"arch"`
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

// openAgentArtifact never accepts a path supplied by a request. Missing,
// nonregular and symlinked artifacts are intentionally indistinguishable.
func (s *Server) openAgentArtifact(arch string) (*os.File, os.FileInfo, error) {
	if s.AgentDownloadDir == "" || (arch != "amd64" && arch != "arm64") {
		return nil, nil, os.ErrNotExist
	}
	filename := filepath.Join(s.AgentDownloadDir, "newnexus-agent-linux-"+arch)
	info, err := os.Lstat(filename)
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		return nil, nil, os.ErrNotExist
	}
	file, err := os.Open(filename)
	if err != nil {
		return nil, nil, os.ErrNotExist
	}
	info, err = file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		file.Close()
		return nil, nil, os.ErrNotExist
	}
	return file, info, nil
}

func (s *Server) nodeInstallation(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		fail(w, http.StatusMethodNotAllowed, "method_not_allowed", errors.New("use GET"))
		return
	}
	addresses, err := s.effectiveAddresses(r.Context())
	if err != nil {
		fail(w, http.StatusInternalServerError, "settings_failed", errors.New("installation address settings could not be loaded"))
		return
	}
	server := addresses["addresses.agent_endpoint"].(string)
	tlsInfo, err := s.AgentTLS.Public(r.Context())
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "agent_tls_unavailable", errors.New("Agent TLS information could not be loaded"))
		return
	}
	artifacts := make([]agentArtifact, 0, len(agentArchitectures))
	for _, arch := range agentArchitectures {
		file, _, err := s.openAgentArtifact(arch)
		if err != nil {
			continue
		}
		hash := sha256.New()
		_, err = io.Copy(hash, file)
		file.Close()
		if err != nil {
			continue
		}
		artifacts = append(artifacts, agentArtifact{Arch: arch, URL: "/api/v1/agent-downloads/linux-" + arch, SHA256: hex.EncodeToString(hash.Sum(nil))})
	}
	write(w, http.StatusOK, struct {
		Server           string               `json:"server"`
		DownloadOrigin   string               `json:"download_origin"`
		ConsoleOrigin    string               `json:"console_origin"`
		InsecureLoopback bool                 `json:"insecure_loopback"`
		TLS              *agenttls.PublicInfo `json:"tls"`
		Artifacts        []agentArtifact      `json:"artifacts"`
	}{server, addresses["addresses.api_origin"].(string), addresses["addresses.console_origin"].(string), s.AgentInsecureLoopback && server == s.AgentPublicAddr && !tlsInfo.Enabled, tlsInfo, artifacts})
}

func (s *Server) agentDownload(w http.ResponseWriter, r *http.Request) {
	arch := ""
	for _, candidate := range agentArchitectures {
		if r.URL.Path == "/api/v1/agent-downloads/linux-"+candidate {
			arch = candidate
			break
		}
	}
	if arch == "" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		fail(w, http.StatusMethodNotAllowed, "method_not_allowed", errors.New("use GET or HEAD"))
		return
	}
	file, info, err := s.openAgentArtifact(arch)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer file.Close()
	filename := "newnexus-agent-linux-" + arch
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	http.ServeContent(w, r, filename, info.ModTime(), file)
}
