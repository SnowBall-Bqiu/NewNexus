package api

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/newnexus/newnexus/internal/agenthub"
	"github.com/newnexus/newnexus/internal/agenttls"
	"github.com/newnexus/newnexus/internal/auth"
	"github.com/newnexus/newnexus/internal/domain"
	"github.com/newnexus/newnexus/internal/provider"
	"github.com/newnexus/newnexus/internal/routing"
	"github.com/newnexus/newnexus/internal/security"
	"github.com/newnexus/newnexus/internal/store"
	"io"
	"math"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Operations interface {
	Run(context.Context, string, string, domain.Doc) (domain.Doc, error)
}
type Server struct {
	webhookClient *http.Client
	Store         *store.Store
	Auth          *auth.Service
	Vault         *security.Vault
	Hub           *agenthub.Hub
	AgentTLS      *agenttls.Manager
	Operations    Operations
	DevAuth       bool
	WebDir        string
	// AgentDownloadDir contains only distributable Linux agent binaries.
	AgentDownloadDir      string
	AgentDownloadOrigin   string
	AgentPublicAddr       string
	AgentInsecureLoopback bool
	ConsoleOrigin         string
	// SetupTokenHash is injected by the production entrypoint, never through REST.
	SetupTokenHash string
	mu             sync.Mutex
	listeners      map[chan domain.Doc]bool
	attempts       map[string][]time.Time
}

func New(s *store.Store, v *security.Vault, h *agenthub.Hub, ops Operations) *Server {
	h.Vault = v
	return &Server{Store: s, Vault: v, Hub: h, AgentTLS: agenttls.New(s, v), Operations: ops, Auth: &auth.Service{Store: s, Vault: v, SecureCookie: true}, listeners: map[chan domain.Doc]bool{}, attempts: map[string][]time.Time{}}
}
func write(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if data != nil {
		_ = json.NewEncoder(w).Encode(data)
	}
}
func fail(w http.ResponseWriter, status int, code string, e error) {
	write(w, status, domain.Doc{"error": domain.Doc{"code": code, "message": security.Redact(e.Error())}})
}
func body(r *http.Request) (domain.Doc, error) {
	d := domain.Doc{}
	if r.Body == nil {
		return d, nil
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, 2<<20+1))
	e := dec.Decode(&d)
	if errors.Is(e, io.EOF) {
		return d, nil
	}
	if e != nil {
		return nil, errors.New("request body must be a JSON object")
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		return nil, errors.New("request body contains trailing data")
	}
	return d, nil
}
func public(d domain.Doc) domain.Doc {
	out := domain.Clone(d)
	for _, k := range []string{"_version", "credentials", "credentials_encrypted", "credential_hash", "pending_credential_hash", "pending_credential_encrypted", "session_generation", "redeploy_token_hash", "password_hash", "totp_secret", "totp_pending", "recovery_hashes", "csrf_token", "request_hash", "lease_owner"} {
		delete(out, k)
	}
	return out
}
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "same-origin")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; font-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'self'; form-action 'self'; object-src 'none'")
	if r.URL.Path == "/healthz" || r.URL.Path == "/api/v1/healthz" {
		if e := s.Store.DB.PingContext(r.Context()); e != nil {
			fail(w, 503, "database_unavailable", errors.New("database health check failed"))
			return
		}
		write(w, 200, domain.Doc{"status": "ok", "version": "0.1.0"})
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/api/") {
		if s.WebDir != "" {
			clean := path.Clean("/" + r.URL.Path)
			file := filepath.Join(s.WebDir, filepath.FromSlash(strings.TrimPrefix(clean, "/")))
			info, e := os.Stat(file)
			if e != nil && path.Ext(clean) == "" {
				http.ServeFile(w, r, filepath.Join(s.WebDir, "index.html"))
				return
			}
			if e == nil && info.IsDir() && clean != "/" {
				http.NotFound(w, r)
				return
			}
			if strings.HasPrefix(clean, "/assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
			http.FileServer(http.Dir(s.WebDir)).ServeHTTP(w, r)
		} else {
			http.NotFound(w, r)
		}
		return
	}
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/"), "/")
	if !strings.HasPrefix(r.URL.Path, "/api/v1/") {
		fail(w, 404, "not_found", errors.New("API version not found"))
		return
	}
	if path == "bootstrap/status" && r.Method == "GET" {
		initialized := s.Auth.Initialized(r.Context())
		write(w, 200, domain.Doc{"initialized": initialized, "setup_token_required": !initialized && s.SetupTokenHash != ""})
		return
	}
	if path == "bootstrap" || path == "auth/login" {
		s.login(w, r, path)
		return
	}
	if r.URL.Path == "/api/v1/agent-downloads" || strings.HasPrefix(r.URL.Path, "/api/v1/agent-downloads/") {
		s.agentDownload(w, r)
		return
	}
	if r.URL.Path == "/api/v1/agent-ca.pem" {
		s.agentCACertificate(w, r)
		return
	}
	session, isKey, e := s.Auth.Authenticate(r)
	if e != nil && s.DevAuth {
		session = domain.Doc{"username": "development", "csrf_token": "development"}
		e = nil
		isKey = true
	}
	if e != nil {
		fail(w, 401, "unauthorized", auth.ErrUnauthorized)
		return
	}
	if r.Method != "GET" && r.Method != "HEAD" && r.Method != "OPTIONS" && !isKey {
		if origin := r.Header.Get("Origin"); origin != "" && origin != "http://"+r.Host && origin != "https://"+r.Host {
			fail(w, 403, "csrf_origin", errors.New("request origin does not match this console"))
			return
		}
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(domain.S(session, "csrf_token"))) != 1 {
			fail(w, 403, "csrf_invalid", errors.New("CSRF token is missing or invalid"))
			return
		}
	}
	if strings.HasPrefix(path, "auth/") {
		s.authRoutes(w, r, path, session)
		return
	}
	if !isKey {
		if required, e := s.Store.Get(r.Context(), "settings", "auth.require_totp"); e == nil && domain.B(required, "value") {
			admin, _ := s.Store.Get(r.Context(), "admin", "singleton")
			if !domain.B(admin, "totp_enabled") {
				fail(w, 403, "totp_required", errors.New("enable TOTP before accessing administration resources"))
				return
			}
		}
	}
	if path == "events" {
		s.events(w, r)
		return
	}
	if path == "nodes/installation" {
		s.nodeInstallation(w, r)
		return
	}
	if path == "overview" {
		s.overview(w, r)
		return
	}
	if path == "settings" {
		s.settings(w, r)
		return
	}
	if s.metricsRoutes(w, r, path) {
		return
	}
	if s.systemRoutes(w, r, path) {
		return
	}
	if s.bulk(w, r, path, session) {
		return
	}
	s.resources(w, r, path, session)
}
func (s *Server) login(w http.ResponseWriter, r *http.Request, path string) {
	if r.Method != "POST" {
		fail(w, 405, "method_not_allowed", errors.New("use POST"))
		return
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	s.mu.Lock()
	now := time.Now()
	history := s.attempts[host]
	fresh := history[:0]
	for _, at := range history {
		if now.Sub(at) < 5*time.Minute {
			fresh = append(fresh, at)
		}
	}
	limited := len(fresh) >= 10
	s.attempts[host] = append(fresh, now)
	s.mu.Unlock()
	if limited {
		w.Header().Set("Retry-After", "300")
		fail(w, 429, "rate_limited", errors.New("too many authentication attempts; retry in five minutes"))
		return
	}
	d, e := body(r)
	if e != nil {
		fail(w, 400, "invalid_json", e)
		return
	}
	var user domain.Doc
	if path == "bootstrap" {
		if s.SetupTokenHash != "" && subtle.ConstantTimeCompare([]byte(auth.Hash(domain.S(d, "setup_token"))), []byte(s.SetupTokenHash)) != 1 {
			fail(w, 403, "setup_token_invalid", errors.New("a valid setup token is required to initialize this console"))
			return
		}
		user, e = s.Auth.Bootstrap(r.Context(), domain.S(d, "username"), domain.S(d, "password"))
	} else {
		user, e = s.Auth.Login(r.Context(), domain.S(d, "username"), domain.S(d, "password"), domain.S(d, "totp_code"))
	}
	if e != nil {
		status := 401
		if path == "bootstrap" {
			status = 400
		}
		fail(w, status, "authentication_failed", e)
		return
	}
	token, session, e := s.Auth.NewSession(r.Context(), user)
	if e != nil {
		fail(w, 500, "session_failed", e)
		return
	}
	s.Auth.SetCookie(w, token, 43200)
	s.audit(r, "auth.login", "admin", "singleton", "administrator signed in", domain.S(user, "username"))
	write(w, 200, domain.Doc{"user": domain.Doc{"username": user["username"], "totp_enabled": user["totp_enabled"]}, "csrf_token": session["csrf_token"]})
}
func (s *Server) authRoutes(w http.ResponseWriter, r *http.Request, path string, session domain.Doc) {
	ctx := r.Context()
	switch {
	case path == "auth/me" && r.Method == "GET":
		user, _ := s.Store.Get(ctx, "admin", "singleton")
		write(w, 200, domain.Doc{"user": domain.Doc{"username": session["username"], "totp_enabled": domain.B(user, "totp_enabled")}, "csrf_token": session["csrf_token"]})
	case path == "auth/logout" && r.Method == "POST":
		if cookie, e := r.Cookie("newnexus_session"); e == nil {
			_ = s.Store.Delete(ctx, "sessions", auth.Hash(cookie.Value))
		}
		s.Auth.SetCookie(w, "", -1)
		write(w, 200, domain.Doc{"ok": true})
	case path == "auth/sessions" && r.Method == "GET":
		rows, _ := s.Store.List(ctx, "sessions")
		out := []domain.Doc{}
		for _, d := range rows {
			out = append(out, public(d))
		}
		write(w, 200, domain.Doc{"items": out, "total": len(out)})
	case path == "auth/sessions/revoke" && r.Method == "POST":
		d, e := body(r)
		if e == nil {
			e = s.Store.Delete(ctx, "sessions", domain.S(d, "id"))
		}
		if e != nil {
			fail(w, 400, "revoke_failed", e)
			return
		}
		write(w, 200, domain.Doc{"ok": true})
	case path == "auth/totp/setup" && r.Method == "POST":
		d, e := s.Auth.TOTPSetup(ctx)
		if e != nil {
			fail(w, 400, "totp_setup_failed", e)
			return
		}
		write(w, 200, d)
	case path == "auth/totp/enable" && r.Method == "POST":
		d, e := body(r)
		if e != nil {
			fail(w, 400, "invalid_json", e)
			return
		}
		codes, e := s.Auth.TOTPEnable(ctx, domain.S(d, "code"))
		if e != nil {
			fail(w, 400, "totp_invalid", e)
			return
		}
		s.audit(r, "totp.enable", "admin", "singleton", "TOTP enabled", domain.S(session, "username"))
		write(w, 200, domain.Doc{"recovery_codes": codes})
	case path == "auth/api-keys" && r.Method == "POST":
		d, e := body(r)
		if e != nil {
			fail(w, 400, "invalid_json", e)
			return
		}
		key := "nn_" + auth.Token()
		record := domain.Doc{"id": auth.Hash(key), "name": d["name"], "username": session["username"], "revoked": false}
		if e = s.Store.Put(ctx, "api-keys", record); e != nil {
			fail(w, 500, "key_failed", e)
			return
		}
		write(w, 201, domain.Doc{"id": record["id"], "key": key})
	case path == "auth/api-keys" && r.Method == "GET":
		rows, _ := s.Store.List(ctx, "api-keys")
		out := []domain.Doc{}
		for _, d := range rows {
			out = append(out, public(d))
		}
		write(w, 200, domain.Doc{"items": out, "total": len(out)})
	case strings.HasPrefix(path, "auth/api-keys/") && r.Method == "DELETE":
		e := s.Store.Delete(ctx, "api-keys", strings.TrimPrefix(path, "auth/api-keys/"))
		if e != nil {
			fail(w, 404, "not_found", e)
			return
		}
		write(w, 200, domain.Doc{"ok": true})
	default:
		fail(w, 404, "not_found", errors.New("authentication endpoint not found"))
	}
}

var resources = map[string]bool{"sites": true, "projects": true, "connections": true, "sources": true, "candidates": true, "nodes": true, "node-groups": true, "probe-profiles": true, "rules": true, "releases": true, "saas/zones": true, "saas/hostnames": true, "optimized-hostnames": true, "jobs": true, "alerts": true, "audit": true}

func split(path string) (kind, id, action string) {
	parts := strings.Split(path, "/")
	kind = parts[0]
	offset := 1
	if kind == "saas" && len(parts) > 1 {
		kind += "/" + parts[1]
		offset = 2
	}
	if kind == "routes" {
		kind = "rules"
	}
	if len(parts) > offset {
		id = parts[offset]
	}
	if len(parts) > offset+1 {
		action = parts[offset+1]
	}
	return
}
func (s *Server) resources(w http.ResponseWriter, r *http.Request, path string, session domain.Doc) {
	kind, id, action := split(path)
	if !resources[kind] {
		fail(w, 404, "not_found", errors.New("resource collection not found"))
		return
	}
	ctx := r.Context()
	if kind == "nodes" && id == "enrollment-tokens" && r.Method == "POST" {
		d, e := body(r)
		if e != nil {
			fail(w, 400, "invalid_json", e)
			return
		}
		out, e := s.Hub.Enrollment(ctx, time.Duration(domain.N(d, "expires_in_seconds"))*time.Second, domain.S(d, "name"))
		if e != nil {
			fail(w, 500, "enrollment_failed", e)
			return
		}
		write(w, 201, out)
		return
	}
	if kind == "candidates" && id == "probe" && r.Method == "POST" {
		d, e := body(r)
		if e == nil {
			_, e = s.Queue(ctx, "probe", "candidates", d, r.Header.Get("Idempotency-Key"))
		}
		if e != nil {
			fail(w, 400, "probe_failed", e)
			return
		}
		write(w, 202, domain.Doc{"status": "queued"})
		return
	}
	if id != "" {
		d, e := s.Store.Get(ctx, kind, id)
		if e != nil {
			fail(w, 404, "not_found", e)
			return
		}
		if action != "" {
			s.action(w, r, kind, action, d, session)
			return
		}
		switch r.Method {
		case "GET":
			write(w, 200, public(d))
		case "PUT", "PATCH":
			if readOnly(kind) {
				fail(w, 405, "read_only", errors.New("this resource is managed by the control plane"))
				return
			}
			patch, e := body(r)
			if e == nil {
				patch = cleanInput(kind, patch, d)
				for k, v := range patch {
					if k != "id" && k != "created_at" && k != "_version" {
						d[k] = v
					}
				}
				e = s.prepare(ctx, kind, d)
			}
			if e == nil {
				e = s.Store.Put(ctx, kind, d)
			}
			if e != nil {
				fail(w, 400, "validation_failed", e)
				return
			}
			s.audit(r, "update", kind, id, "resource configuration updated", domain.S(session, "username"))
			s.emit(kind, id)
			write(w, 200, public(d))
		case "DELETE":
			if readOnly(kind) {
				fail(w, 405, "read_only", errors.New("this resource cannot be deleted directly"))
				return
			}
			if kind == "saas/hostnames" {
				job, e := s.Queue(ctx, "delete", kind, d, r.Header.Get("Idempotency-Key"))
				if e != nil {
					fail(w, 400, "delete_failed", e)
					return
				}
				write(w, 202, domain.Doc{"job_id": job["id"]})
				return
			}
			if e = s.referenceCheck(ctx, kind, id); e != nil {
				fail(w, 409, "resource_in_use", e)
				return
			}
			if kind == "nodes" {
				e = s.Hub.DeleteNode(ctx, id)
			} else {
				e = s.Store.Delete(ctx, kind, id)
			}
			if e != nil {
				fail(w, 500, "delete_failed", e)
				return
			}
			s.audit(r, "delete", kind, id, "resource deleted", domain.S(session, "username"))
			s.emit(kind, id)
			write(w, 200, domain.Doc{"ok": true})
		default:
			fail(w, 405, "method_not_allowed", errors.New("unsupported method"))
		}
		return
	}
	switch r.Method {
	case "GET":
		rows, e := s.Store.List(ctx, kind)
		if e != nil {
			fail(w, 500, "list_failed", e)
			return
		}
		s.list(w, r, rows)
	case "POST":
		if readOnly(kind) || kind == "nodes" {
			fail(w, 405, "read_only", errors.New("this resource is created by the control plane"))
			return
		}
		d, e := body(r)
		if e == nil {
			d = cleanInput(kind, d, nil)
			d["id"] = domain.ID()
			e = s.prepare(ctx, kind, d)
		}
		if e == nil {
			e = s.Store.Put(ctx, kind, d)
		}
		if e != nil {
			fail(w, 400, "validation_failed", e)
			return
		}
		s.audit(r, "create", kind, domain.S(d, "id"), "resource created", domain.S(session, "username"))
		s.emit(kind, domain.S(d, "id"))
		if kind == "saas/hostnames" || kind == "saas/zones" {
			job, e := s.Queue(ctx, "create", kind, d, r.Header.Get("Idempotency-Key"))
			if e != nil {
				fail(w, 500, "job_failed", e)
				return
			}
			d["job_id"] = job["id"]
		}
		write(w, 201, public(d))
	default:
		fail(w, 405, "method_not_allowed", errors.New("unsupported method"))
	}
}
func readOnly(kind string) bool {
	return kind == "releases" || kind == "jobs" || kind == "audit" || kind == "alerts"
}
func (s *Server) list(w http.ResponseWriter, r *http.Request, rows []domain.Doc) {
	q := r.URL.Query()
	filtered := []domain.Doc{}
	needle := strings.ToLower(q.Get("q"))
	for _, d := range rows {
		if needle != "" {
			raw, _ := json.Marshal(public(d))
			if !strings.Contains(strings.ToLower(string(raw)), needle) {
				continue
			}
		}
		match := true
		for _, key := range []string{"project_id", "status", "source_id", "saas_zone_id", "tier", "connection_id"} {
			if value := q.Get(key); value != "" && domain.S(d, key) != value {
				match = false
			}
		}
		if match {
			filtered = append(filtered, d)
		}
	}
	if q.Get("sort") == "score" {
		sort.SliceStable(filtered, func(i, j int) bool { return domain.N(filtered[i], "score") > domain.N(filtered[j], "score") })
	}
	total := len(filtered)
	cursor := ""
	if raw := q.Get("cursor"); raw != "" {
		b, e := base64.RawURLEncoding.DecodeString(raw)
		if e != nil {
			fail(w, 400, "invalid_cursor", errors.New("invalid cursor"))
			return
		}
		cursor = string(b)
	}
	start := 0
	if cursor != "" {
		found := false
		for i, d := range filtered {
			if domain.S(d, "id") == cursor {
				start = i + 1
				found = true
				break
			}
		}
		if !found {
			fail(w, 400, "invalid_cursor", errors.New("cursor resource no longer exists"))
			return
		}
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	end := min(start+limit, len(filtered))
	items := []domain.Doc{}
	for _, d := range filtered[start:end] {
		items = append(items, public(d))
	}
	next := ""
	if end < len(filtered) && end > 0 {
		next = base64.RawURLEncoding.EncodeToString([]byte(domain.S(filtered[end-1], "id")))
	}
	write(w, 200, domain.Doc{"items": items, "total": total, "next_cursor": next})
}
func (s *Server) prepare(ctx context.Context, kind string, d domain.Doc) error {
	if kind == "connections" {
		switch domain.S(d, "provider") {
		case "cloudflare", "dnspod", "alidns":
		default:
			return errors.New("provider must be cloudflare, dnspod or alidns")
		}
		if credentials := domain.M(d, "credentials"); len(credentials) > 0 {
			raw, _ := json.Marshal(credentials)
			sealed, e := s.Vault.Seal(raw, "connection:"+domain.S(d, "id"))
			if e != nil {
				return e
			}
			d["credentials_encrypted"] = sealed
			d["credential_mask"] = "••••••••"
			delete(d, "credentials")
			d["status"] = "unverified"
		}
		if domain.S(d, "credentials_encrypted") == "" {
			return errors.New("provider credentials are required")
		}
	}
	switch kind {
	case "projects", "sites", "connections", "sources", "node-groups", "probe-profiles", "rules":
		if strings.TrimSpace(domain.S(d, "name")) == "" {
			return errors.New("name is required")
		}
	}
	if kind == "candidates" {
		ip, e := netip.ParseAddr(domain.S(d, "address"))
		if e != nil || !security.PublicAddress(ip) {
			return errors.New("candidate must be a public IP address")
		}
		if ip.Is4() {
			d["family"] = 4
		} else {
			d["family"] = 6
		}
		if domain.S(d, "tier") == "" {
			d["tier"] = "inventory"
		}
	}
	if kind == "sites" {
		if _, e := security.ValidateURL(domain.S(d, "url")); e != nil {
			return e
		}
	}
	if kind == "sources" {
		switch domain.S(d, "type") {
		case "manual", "cloudflare", "url", "dns", "csv", "json":
		default:
			return errors.New("unsupported candidate source type")
		}
		if d["enabled"] == nil {
			d["enabled"] = true
		}
		if domain.S(d, "type") == "url" {
			if u, e := security.ValidateURL(domain.S(d, "location")); e != nil {
				return e
			} else if u.Scheme != "https" {
				return errors.New("candidate subscription URLs must use HTTPS")
			}
		}
	}
	if kind == "probe-profiles" {
		if mode := domain.S(d, "mode"); mode != "" && mode != "all" && mode != "tcp" && mode != "http" {
			return errors.New("probe mode must be tcp, http or all")
		}
		if domain.S(d, "url") != "" {
			if _, e := security.ValidateURL(domain.S(d, "url")); e != nil {
				return e
			}
		}
		defaults := domain.Doc{"mode": "all"}
		for k, v := range defaults {
			if d[k] == nil {
				d[k] = v
			}
		}
		if d["timeout_ms"] != nil && (domain.N(d, "timeout_ms") > 60000 || domain.N(d, "timeout_ms") < 100) {
			return errors.New("timeout must be between 100 and 60000 ms")
		}
		if domain.N(d, "max_bytes") > 16<<20 {
			return errors.New("download limit may not exceed 16 MiB")
		}
		if d["retries"] != nil && (domain.N(d, "retries") < 0 || domain.N(d, "retries") > 3) {
			return errors.New("probe retries must be between 0 and 3")
		}
		if d["samples"] != nil && (domain.N(d, "samples") < 1 || domain.N(d, "samples") > 10) {
			return errors.New("probe samples must be between 1 and 10")
		}
		if d["max_redirects"] != nil && (domain.N(d, "max_redirects") < 0 || domain.N(d, "max_redirects") > 5) {
			return errors.New("probe redirects must be between 0 and 5")
		}
		if n := domain.N(d, "interval_seconds"); d["interval_seconds"] != nil && (n < 5 || n > 86400 || n != math.Trunc(n)) {
			return errors.New("probe interval must be an integer between 5 and 86400 seconds")
		}
		for _, key := range []string{"host", "sni", "path", "method", "headers", "expected_statuses", "expected_contains", "samples", "max_redirects", "retries", "timeout_ms", "max_bytes"} {
			if value, present := d[key]; present {
				if value == nil {
					delete(d, key)
					continue
				}
				if e := validateSetting("probe."+key, value); e != nil {
					return e
				}
			}
		}
	}
	if kind == "rules" {
		if domain.S(d, "site_id") != "" {
			site, e := s.Store.Get(ctx, "sites", domain.S(d, "site_id"))
			if e != nil {
				return errors.New("associated site does not exist")
			}
			if domain.S(site, "project_id") != domain.S(d, "project_id") {
				return errors.New("rule and site must belong to the same project")
			}
		}
		conn, e := s.Store.Get(ctx, "connections", domain.S(d, "dns_connection_id"))
		if e != nil {
			return errors.New("a valid DNS connection is required")
		}
		cap := domain.M(conn, "capabilities")
		if zoneCap := domain.M(domain.M(conn, "zone_capabilities"), domain.S(d, "zone_id")); len(zoneCap) > 0 {
			cap = zoneCap
		}
		if domain.S(conn, "status") != "valid" && domain.S(conn, "status") != "connected" && domain.S(conn, "status") != "limited" {
			return errors.New("validate DNS connection before creating a rule")
		}
		defaults := domain.Doc{"ttl": 120, "publish_mode": "manual", "switch_mode": "performance", "status": "pending"}
		for k, v := range defaults {
			if d[k] == nil {
				d[k] = v
			}
		}
		lines := domain.A(d, "lines")
		for _, line := range lines {
			if domain.S(line, "id") == "" {
				line["id"] = domain.ID()
			}
			if domain.S(line, "provider_line_code") == "" {
				line["provider_line_code"] = "default"
			}
			if line["min_nodes"] == nil {
				delete(line, "min_nodes")
			} else if n := domain.N(line, "min_nodes"); n < 2 || n > 1000 || n != math.Trunc(n) {
				return errors.New("line minimum nodes must be an integer from 2 to 1000, or null to inherit")
			}
			if line["quorum_percent"] == nil {
				delete(line, "quorum_percent")
			} else if n := domain.N(line, "quorum_percent"); n < 200./3 || n > 100 {
				return errors.New("line quorum must be at least two thirds and at most 100 percent, or null to inherit")
			}
			targets := domain.A(line, "targets")
			for _, t := range targets {
				if domain.S(t, "id") == "" {
					t["id"] = domain.ID()
				}
				if t["enabled"] == nil {
					t["enabled"] = true
				}
				if t["weight"] == nil {
					t["weight"] = 1
				}
				if t["priority"] == nil {
					t["priority"] = 0
				}
				if t["health"] == nil {
					t["health"] = "unknown"
				}
				if protocol := domain.S(t, "protocol"); protocol != "" && protocol != "http" && protocol != "https" {
					return errors.New("target protocol must be http or https")
				}
				if t["port"] != nil {
					p := int(domain.N(t, "port"))
					if p < 1 || p > 65535 {
						return errors.New("target port must be between 1 and 65535 and allowed by the selected Agent")
					}
				}
			}
			line["targets"] = targets
		}
		d["lines"] = lines
		if e = routing.Validate(d, provider.Capabilities{Lines: domain.B(cap, "lines"), Weights: domain.B(cap, "weights"), Apex: domain.B(cap, "apex")}); e != nil {
			return e
		}
		rules, _ := s.Store.List(ctx, "rules")
		edges := map[string][]string{}
		for _, rule := range append(rules, d) {
			for _, line := range domain.A(rule, "lines") {
				for _, t := range domain.A(line, "targets") {
					edges[domain.S(rule, "hostname")] = append(edges[domain.S(rule, "hostname")], domain.S(t, "cname"))
				}
			}
		}
		if e = routing.DetectLoop(domain.S(d, "hostname"), edges); e != nil {
			return e
		}
	}
	if kind == "saas/zones" {
		if domain.S(d, "connection_id") == "" || domain.S(d, "zone_id") == "" {
			return errors.New("Cloudflare connection and zone are required")
		}
	}
	if kind == "saas/hostnames" {
		if domain.S(d, "hostname") == "" {
			return errors.New("hostname is required")
		}
		if _, e := s.Store.Get(ctx, "saas/zones", domain.S(d, "saas_zone_id")); e != nil {
			return errors.New("SaaS zone is required")
		}
		if d["validation_method"] == nil {
			d["validation_method"] = "txt"
		}
		if d["automation_mode"] == nil {
			d["automation_mode"] = "manual"
		}
		if d["status"] == nil {
			d["status"] = "pending"
		}
	}
	if kind == "optimized-hostnames" {
		if domain.S(d, "hostname") == "" || domain.S(d, "dns_connection_id") == "" || domain.S(d, "zone_id") == "" {
			return errors.New("hostname, DNS connection and zone are required")
		}
		if domain.N(d, "top_n") <= 0 {
			d["top_n"] = 2
		}
		if domain.N(d, "top_n") > 100 {
			return errors.New("top_n may not exceed 100")
		}
		if d["family"] == nil {
			d["family"] = 4
		}
		if d["ttl"] == nil {
			d["ttl"] = 120
		}
	}
	if kind == "nodes" {
		if mode := domain.S(d, "mode"); mode != "" && mode != "tcp" && mode != "http" && mode != "all" {
			return errors.New("node mode must be tcp, http or all")
		}
	}
	if d["status"] == nil {
		d["status"] = "active"
	}
	return nil
}
func (s *Server) referenceCheck(ctx context.Context, kind, id string) error {
	refs := map[string]string{"projects": "project_id", "connections": "dns_connection_id", "probe-profiles": "probe_profile_id", "saas/zones": "saas_zone_id"}
	key := refs[kind]
	if key == "" {
		return nil
	}
	for _, collection := range []string{"rules", "saas/zones", "saas/hostnames", "sources", "optimized-hostnames"} {
		rows, e := s.Store.List(ctx, collection)
		if e != nil {
			return e
		}
		for _, d := range rows {
			if domain.S(d, key) == id || kind == "connections" && domain.S(d, "connection_id") == id {
				return fmt.Errorf("resource is referenced by %s", collection)
			}
		}
	}
	return nil
}
func (s *Server) action(w http.ResponseWriter, r *http.Request, kind, action string, d, session domain.Doc) {
	ctx := r.Context()
	if r.Method == "GET" && (action == "zones" || action == "lines") && kind == "connections" {
		if d["zones"] == nil {
			if _, e := s.Operations.Run(ctx, "sync", kind, d); e != nil {
				fail(w, 400, "provider_read_failed", e)
				return
			}
			d, _ = s.Store.Get(ctx, kind, domain.S(d, "id"))
		}
		var items any
		items = d["zones"]
		if action == "lines" {
			zone := r.URL.Query().Get("zone_id")
			if zone == "" {
				fail(w, 400, "zone_required", errors.New("zone_id is required to read provider lines"))
				return
			}
			items = domain.M(d, "lines")[zone]
		}
		if items == nil {
			items = []any{}
		}
		write(w, 200, domain.Doc{"items": items})
		return
	}
	if r.Method != "POST" {
		fail(w, 405, "method_not_allowed", errors.New("actions require POST"))
		return
	}
	payload, e := body(r)
	if e != nil {
		fail(w, 400, "invalid_json", e)
		return
	}
	switch {
	case kind == "nodes" && action == "redeploy":
		if confirmed, ok := payload["confirmed"].(bool); !ok || !confirmed {
			fail(w, 400, "confirmation_required", errors.New("explicit confirmation is required to replace node credentials"))
			return
		}
		expires := 900.
		if value, exists := payload["expires_in_seconds"]; exists {
			var valid bool
			expires, valid = value.(float64)
			if !valid {
				fail(w, 400, "invalid_expiry", errors.New("redeployment token lifetime must be an integer from 60 to 900 seconds"))
				return
			}
		}
		if expires != math.Trunc(expires) || expires < 60 || expires > 900 {
			fail(w, 400, "invalid_expiry", errors.New("redeployment token lifetime must be 60 to 900 seconds"))
			return
		}
		out, err := s.Hub.Redeploy(ctx, domain.S(d, "id"), time.Duration(expires)*time.Second)
		if err != nil {
			fail(w, 409, "redeploy_failed", err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		s.audit(r, "redeploy", kind, domain.S(d, "id"), "node redeployment token issued and previous credentials revoked", domain.S(session, "username"))
		s.emit(kind, domain.S(d, "id"))
		write(w, 201, out)
		return
	case kind == "nodes" && action == "revoke":
		e = s.Hub.Revoke(ctx, domain.S(d, "id"))
	case kind == "nodes" && action == "rotate":
		e = s.Hub.Rotate(ctx, domain.S(d, "id"))
	case kind == "alerts" && (action == "acknowledge" || action == "resolve"):
		d["status"] = "acknowledged"
		if action == "resolve" {
			d["status"] = "resolved"
		}
		e = s.Store.Put(ctx, kind, d)
	case kind == "jobs" && action == "retry":
		if domain.S(d, "status") != "failed" {
			e = errors.New("only failed jobs can be retried")
			break
		}
		d["status"] = "queued"
		d["next_attempt_at"] = domain.Now()
		d["attempts"] = 0
		e = s.Store.Put(ctx, kind, d)
	case kind == "rules" && action == "evaluate":
		_, e = s.EvaluateRule(ctx, d)
	case action == "preview" && (kind == "rules" || kind == "optimized-hostnames" || kind == "saas/hostnames"):
		out, err := s.Operations.Run(ctx, action, kind, d)
		if err != nil {
			fail(w, 400, "preview_failed", err)
			return
		}
		s.audit(r, "preview", kind, domain.S(d, "id"), "DNS change preview generated", domain.S(session, "username"))
		write(w, 200, public(out))
		return
	case (action == "sync" || action == "resume-automation" || action == "validate" || action == "activate" || action == "apply" || action == "rollback") && (kind == "connections" || kind == "sources" || kind == "saas/zones" || kind == "saas/hostnames" || kind == "releases"):
		if kind == "releases" && action == "apply" {
			if !domain.B(payload, "confirmed") {
				e = errors.New("review the DNS diff and explicitly confirm the release")
				break
			}
			if r.Header.Get("Idempotency-Key") == "" {
				e = errors.New("Idempotency-Key header is required for DNS publishing")
				break
			}
		}
		job, err := s.Queue(ctx, action, kind, d, r.Header.Get("Idempotency-Key"))
		if err != nil {
			fail(w, 409, "job_conflict", err)
			return
		}
		s.audit(r, action, kind, domain.S(d, "id"), "operation queued", domain.S(session, "username"))
		write(w, 202, domain.Doc{"job_id": job["id"]})
		return
	default:
		e = errors.New("unsupported resource action")
	}
	if e != nil {
		fail(w, 400, "action_failed", e)
		return
	}
	s.emit(kind, domain.S(d, "id"))
	write(w, 200, domain.Doc{"ok": true})
}
func (s *Server) audit(r *http.Request, action, kind, id, summary, actor string) {
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	_ = s.Store.Put(r.Context(), "audit", domain.Doc{"action": action, "resource_type": kind, "resource_id": id, "summary": summary, "actor": actor, "ip_address": ip})
}
func (s *Server) emit(kind, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch := range s.listeners {
		select {
		case ch <- domain.Doc{"kind": kind, "id": id}:
		default:
		}
	}
}
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		fail(w, 500, "stream_unavailable", errors.New("streaming is unavailable"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	ch := make(chan domain.Doc, 32)
	s.mu.Lock()
	s.listeners[ch] = true
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.listeners, ch); s.mu.Unlock() }()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	cursorTime := s.Store.Time(time.Now())
	if last := r.Header.Get("Last-Event-ID"); last != "" {
		if parts := strings.SplitN(last, "|", 2); len(parts) == 2 {
			if at, err := time.Parse(time.RFC3339Nano, parts[0]); err == nil {
				cursorTime = s.Store.Time(at)
			}
		}
	}
	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case d := <-ch:
			b, _ := json.Marshal(d)
			fmt.Fprintf(w, "event: resource.updated\ndata: %s\n\n", b)
			flusher.Flush()
		case <-ticker.C:
			rows, e := s.Store.DB.QueryContext(r.Context(), s.Store.Q(`SELECT kind,MAX(updated_at) FROM resources WHERE kind IN ('projects','sites','connections','sources','candidates','nodes','node-groups','probe-profiles','rules','releases','saas/zones','saas/hostnames','optimized-hostnames','jobs','alerts','audit') AND updated_at>? GROUP BY kind ORDER BY MAX(updated_at)`), cursorTime)
			if e == nil {
				for rows.Next() {
					var kind, at string
					if rows.Scan(&kind, &at) == nil {
						cursorTime = at
						b, _ := json.Marshal(domain.Doc{"kind": kind, "id": ""})
						fmt.Fprintf(w, "id: %s|%s\nevent: resource.updated\ndata: %s\n\n", at, kind, b)
					}
				}
				rows.Close()
			}
			fmt.Fprint(w, ": heartbeat\n\n")
			flusher.Flush()
		}
	}
}
func (s *Server) overview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	counts := domain.Doc{}
	for _, kind := range []string{"projects", "connections", "candidates", "nodes", "rules", "releases", "alerts", "saas/hostnames"} {
		counts[strings.ReplaceAll(kind, "/", "_")] = s.Store.Count(ctx, kind)
	}
	nodes, _ := s.Store.List(ctx, "nodes")
	online := 0
	for _, n := range nodes {
		if domain.S(n, "status") == "online" {
			online++
		}
	}
	rules, _ := s.Store.List(ctx, "rules")
	healthy := 0
	for _, rule := range rules {
		if domain.S(rule, "status") == "healthy" {
			healthy++
		}
	}
	alerts, _ := s.Store.List(ctx, "alerts")
	releases, _ := s.Store.List(ctx, "releases")
	series, _ := s.readHistory(ctx, "", "", "", "", "hour", 24)
	if series == nil {
		series = []domain.Doc{}
	}
	sort.Slice(series, func(i, j int) bool { return domain.S(series[i], "time") < domain.S(series[j], "time") })
	driver := "sqlite"
	if s.Store.Driver == "pgx" {
		driver = "postgres"
	} else if s.Store.Driver == "mysql" {
		driver = "mysql"
	}
	for i, d := range alerts {
		alerts[i] = public(d)
	}
	for i, d := range releases {
		releases[i] = public(d)
	}
	write(w, 200, domain.Doc{"counts": counts, "healthy_rules": healthy, "online_nodes": online, "latency_series": series, "recent_alerts": alerts[:min(5, len(alerts))], "recent_releases": releases[:min(5, len(releases))], "database": driver, "version": "0.1.0", "updated_at": domain.Now()})
}
func (s *Server) settings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if r.Method == "GET" {
		rows, e := s.Store.List(ctx, "settings")
		if e != nil {
			fail(w, 500, "settings_failed", e)
			return
		}
		items := []domain.Doc{}
		for _, d := range rows {
			p := public(d)
			if key := domain.S(d, "key"); isAddressSetting(key) {
				p["effective_value"], p["source"] = s.addressSettingValue(key, domain.S(d, "value"))
			}
			if strings.Contains(domain.S(d, "key"), "password") || strings.Contains(domain.S(d, "key"), "secret") {
				p["value"] = "••••••••"
			}
			items = append(items, p)
		}
		write(w, 200, domain.Doc{"items": items, "total": len(items)})
		return
	}
	if r.Method != "PUT" {
		fail(w, 405, "method_not_allowed", errors.New("use GET or PUT"))
		return
	}
	d, e := body(r)
	if e != nil {
		fail(w, 400, "invalid_json", e)
		return
	}
	for _, key := range []string{"alerts.smtp_password", "alerts.webhook_secret"} {
		if value, ok := d[key].(string); ok && (value == "" || value == "••••••••") {
			delete(d, key)
		}
	}
	for key, value := range d {
		if e = validateSetting(key, value); e != nil {
			fail(w, 400, "invalid_setting", e)
			return
		}
		if isAddressSetting(key) {
			d[key], _ = normalizeAddressSetting(key, value)
		}
	}
	// Prepare usable encrypted key material before making the switch effective.
	// A generation/decryption failure must never persist an enabled TLS state.
	if enabled, _ := d[agenttls.SettingKey].(bool); enabled {
		if _, e = s.AgentTLS.Ensure(ctx); e != nil {
			fail(w, http.StatusServiceUnavailable, "agent_tls_unavailable", errors.New("Agent TLS certificates could not be prepared"))
			return
		}
	}
	for key, value := range d {
		row, e := s.Store.Get(ctx, "settings", key)
		if errors.Is(e, store.ErrNotFound) {
			row = domain.Doc{"id": key, "key": key}
		} else if e != nil {
			fail(w, 500, "settings_failed", e)
			return
		}
		if strings.Contains(key, "password") || strings.Contains(key, "secret") {
			sealed, e := s.Vault.Seal([]byte(fmt.Sprint(value)), "setting:"+key)
			if e != nil {
				fail(w, 500, "encryption_failed", e)
				return
			}
			row["value"] = sealed
			row["encrypted"] = true
		} else {
			row["value"] = value
		}
		if e = s.Store.Put(ctx, "settings", row); e != nil {
			fail(w, 500, "settings_failed", e)
			return
		}
	}
	s.audit(r, "settings.update", "settings", "", "system settings updated", "admin")
	response := domain.Doc{"ok": true}
	for _, key := range []string{"score.weights", "probe.min_samples", "probe.freshness_seconds", "probe.minimum_success_rate"} {
		if _, changed := d[key]; changed {
			if e = s.Store.FlagAggregations(ctx); e != nil {
				fail(w, 500, "aggregation_failed", e)
				return
			}
			job, queueErr := s.Queue(ctx, "aggregate", "candidates", domain.Doc{"id": "all"}, "")
			if queueErr != nil {
				fail(w, 500, "aggregation_failed", queueErr)
				return
			}
			response["job_id"] = job["id"]
			break
		}
	}
	write(w, 200, response)
}
