package api

import (
	"bytes"
	"errors"
	"net/http"
	"time"
)

// agentCACertificate publishes only the CA certificate. AgentTLS owns the
// separately encrypted private material, which never becomes an API resource.
func (s *Server) agentCACertificate(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		fail(w, http.StatusMethodNotAllowed, "method_not_allowed", errors.New("use GET or HEAD"))
		return
	}
	enabled, err := s.AgentTLS.Enabled(r.Context())
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "agent_tls_unavailable", errors.New("Agent TLS configuration could not be loaded"))
		return
	}
	if !enabled {
		http.NotFound(w, r)
		return
	}
	material, err := s.AgentTLS.Ensure(r.Context())
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "agent_tls_unavailable", errors.New("Agent CA certificate could not be loaded"))
		return
	}
	w.Header().Set("Content-Type", "application/x-pem-file")
	w.Header().Set("Content-Disposition", `attachment; filename="newnexus-agent-ca.pem"`)
	http.ServeContent(w, r, "newnexus-agent-ca.pem", time.Time{}, bytes.NewReader(material.CAPEM))
}
