package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/newnexus/newnexus/internal/agenthub"
	"github.com/newnexus/newnexus/internal/domain"
	"github.com/newnexus/newnexus/internal/security"
	"github.com/newnexus/newnexus/internal/testutil"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

type rejectingOperations struct{}

func (rejectingOperations) Run(context.Context, string, string, domain.Doc) (domain.Doc, error) {
	return nil, errors.New("provider rejected test operation")
}
func testServer(t *testing.T) *Server {
	t.Helper()
	s := testutil.Open(t, filepath.Join(t.TempDir(), "api.db"))
	v, _ := security.NewVault(bytes.Repeat([]byte{1}, 32))
	api := New(s, v, agenthub.New(s), rejectingOperations{})
	api.Auth.SecureCookie = false
	if e := api.EnsureDefaults(context.Background()); e != nil {
		t.Fatal(e)
	}
	return api
}
func request(t *testing.T, s *Server, method, path string, payload any, cookie *http.Cookie, csrf string) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(payload)
	r := httptest.NewRequest(method, "http://localhost/api/v1/"+path, bytes.NewReader(raw))
	if payload == nil {
		r.Body = http.NoBody
	}
	if cookie != nil {
		r.AddCookie(cookie)
	}
	if csrf != "" {
		r.Header.Set("X-CSRF-Token", csrf)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}
func setup(t *testing.T, s *Server) (*http.Cookie, string) {
	t.Helper()
	w := request(t, s, "POST", "bootstrap", domain.Doc{"username": "admin", "password": "long-password-123"}, nil, "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	d := domain.Doc{}
	_ = json.Unmarshal(w.Body.Bytes(), &d)
	return w.Result().Cookies()[0], domain.S(d, "csrf_token")
}
func TestAuthenticationCSRFAndCredentialMasking(t *testing.T) {
	s := testServer(t)
	cookie, csrf := setup(t, s)
	if w := request(t, s, "GET", "projects", nil, nil, ""); w.Code != 401 {
		t.Fatal(w.Code)
	}
	if w := request(t, s, "POST", "projects", domain.Doc{"name": "denied"}, cookie, ""); w.Code != 403 {
		t.Fatal(w.Code, w.Body.String())
	}
	w := request(t, s, "POST", "connections", domain.Doc{"name": "CF", "provider": "cloudflare", "credentials": domain.Doc{"api_token": "super-sensitive-token"}}, cookie, csrf)
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "super-sensitive") || strings.Contains(w.Body.String(), "credentials_encrypted") {
		t.Fatal("secret escaped API", w.Body.String())
	}
	all := request(t, s, "GET", "connections", nil, cookie, "")
	if strings.Contains(all.Body.String(), "super-sensitive") {
		t.Fatal("secret escaped list")
	}
	stored, _ := s.Store.List(context.Background(), "connections")
	if domain.S(stored[0], "credentials_encrypted") == "" {
		t.Fatal("secret was not encrypted")
	}
	w = request(t, s, "PUT", "settings", domain.Doc{"alerts.smtp_password": "smtp-secret-value", "alerts.webhook_secret": "signature-secret-value"}, cookie, csrf)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	w = request(t, s, "GET", "settings", nil, cookie, "")
	if strings.Contains(w.Body.String(), "smtp-secret-value") || strings.Contains(w.Body.String(), "signature-secret-value") {
		t.Fatal("settings leaked plaintext")
	}
	if w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("missing security headers")
	}
}
func TestCursorPaginationAndEmptyOverview(t *testing.T) {
	s := testServer(t)
	s.DevAuth = true
	w := request(t, s, "GET", "overview", nil, nil, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"latency_series":[]`) {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, name := range []string{"one", "two", "three"} {
		w = request(t, s, "POST", "projects", domain.Doc{"name": name}, nil, "")
		if w.Code != 201 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	w = request(t, s, "GET", "projects?limit=2", nil, nil, "")
	d := domain.Doc{}
	_ = json.Unmarshal(w.Body.Bytes(), &d)
	if len(domain.A(d, "items")) != 2 || domain.N(d, "total") != 3 || domain.S(d, "next_cursor") == "" {
		t.Fatal(d)
	}
	w = request(t, s, "GET", "projects?limit=2&cursor="+domain.S(d, "next_cursor"), nil, nil, "")
	_ = json.Unmarshal(w.Body.Bytes(), &d)
	if len(domain.A(d, "items")) != 1 {
		t.Fatal(d)
	}
}
func TestFailedProviderJobNeverReportsSuccess(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	d := domain.Doc{"id": domain.ID(), "name": "CF", "provider": "cloudflare"}
	if e := s.Store.Put(ctx, "connections", d); e != nil {
		t.Fatal(e)
	}
	job, e := s.Queue(ctx, "validate", "connections", d, "same-key")
	if e != nil {
		t.Fatal(e)
	}
	duplicate, e := s.Queue(ctx, "validate", "connections", d, "same-key")
	if e != nil || domain.S(job, "id") != domain.S(duplicate, "id") {
		t.Fatal(duplicate, e)
	}
	job["max_attempts"] = 1
	if e = s.runJob(ctx, job); e != nil {
		t.Fatal(e)
	}
	stored, _ := s.Store.Get(ctx, "jobs", domain.S(job, "id"))
	if domain.S(stored, "status") != "failed" {
		t.Fatal(stored)
	}
	if s.Store.Count(ctx, "alerts") != 1 {
		t.Fatal("no failure alert")
	}
}

func TestProtectedFieldsAndSettingBounds(t *testing.T) {
	s := testServer(t)
	s.DevAuth = true
	ctx := context.Background()
	node := domain.Doc{"id": domain.ID(), "name": "trusted", "credential_hash": "original", "status": "online", "version": "trusted-version"}
	if e := s.Store.Put(ctx, "nodes", node); e != nil {
		t.Fatal(e)
	}
	w := request(t, s, "PUT", "nodes/"+domain.S(node, "id"), domain.Doc{"name": "renamed", "mode": "tcp", "credential_hash": "attacker", "status": "revoked", "version": "forged"}, nil, "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	stored, _ := s.Store.Get(ctx, "nodes", domain.S(node, "id"))
	if domain.S(stored, "credential_hash") != "original" || domain.S(stored, "status") != "online" || domain.S(stored, "version") != "trusted-version" || domain.S(stored, "name") != "renamed" {
		t.Fatal(stored)
	}
	for _, settings := range []domain.Doc{{"probe.retries": 10}, {"probe.timeout_ms": 120000}, {"unknown.setting": true}, {"score.weights": domain.Doc{"ttfb": -1}}, {"auth.require_totp": "false"}} {
		w = request(t, s, "PUT", "settings", settings, nil, "")
		if w.Code != 400 {
			t.Fatal("unsafe setting accepted", settings, w.Code, w.Body.String())
		}
	}
	w = request(t, s, "PUT", "settings", domain.Doc{"probe.retries": 0, "routing.cooldown_seconds": 0}, nil, "")
	if w.Code != 200 || s.setting(ctx, "probe.retries", 2) != 0 || s.setting(ctx, "routing.cooldown_seconds", 600) != 0 {
		t.Fatal("zero override ignored", w.Code, w.Body.String())
	}
	for _, kind := range []string{"jobs", "releases", "audit", "alerts"} {
		w = request(t, s, "POST", kind, domain.Doc{"status": "succeeded"}, nil, "")
		if w.Code != 405 {
			t.Fatal("server managed collection writable", kind, w.Code)
		}
	}
}

func TestNodeGroupPerspectiveAndModes(t *testing.T) {
	node := domain.Doc{"id": "a", "group_name": "east", "mode": "tcp", "capabilities": []any{"tcp"}}
	groups := []domain.Doc{{"id": "g", "name": "west", "node_ids": []any{"a"}}}
	if !nodeInLine(node, domain.Doc{"node_group": "west"}, groups) {
		t.Fatal("explicit membership ignored")
	}
	if !nodeInLine(node, domain.Doc{"node_group": "missing", "health_perspective": "global"}, groups) {
		t.Fatal("global perspective filtered")
	}
	task := domain.Doc{"mode": "all"}
	if !nodeTask(node, task, false) || domain.S(task, "mode") != "tcp" {
		t.Fatal(task)
	}
	if nodeTask(node, domain.Doc{"mode": "all"}, true) {
		t.Fatal("TCP-only node accepted for HTTP quorum")
	}
}

func TestSchedulerUsesSiteBusinessURLAndBatchesOnlyDueCandidates(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	node := domain.Doc{"id": domain.ID(), "name": "HTTP node", "status": "online", "last_seen_at": domain.Now(), "mode": "http", "capabilities": []any{"http"}}
	_ = s.Store.Put(ctx, "nodes", node)
	site := domain.Doc{"id": domain.ID(), "project_id": "p", "name": "Site", "url": "https://business.example.com/health"}
	_ = s.Store.Put(ctx, "sites", site)
	rule := domain.Doc{"id": domain.ID(), "project_id": "p", "site_id": site["id"], "hostname": "service.example.com", "switch_mode": "availability", "publish_mode": "manual", "lines": []domain.Doc{{"id": domain.ID(), "targets": []domain.Doc{{"id": "target-one", "cname": "cdn.example.net", "priority": 0, "weight": 1, "enabled": true, "health": "unknown", "protocol": "https"}}}}}
	_ = s.Store.Put(ctx, "rules", rule)
	candidate := domain.Doc{"id": domain.ID(), "address": "1.1.1.1", "tier": "active"}
	_ = s.Store.Put(ctx, "candidates", candidate)
	s.ScheduleTick(ctx)
	tasks, e := s.Store.List(ctx, "tasks")
	if e != nil {
		t.Fatal(e)
	}
	found := false
	for _, task := range tasks {
		if domain.S(task, "candidate_id") == "target:target-one" {
			found = true
			if domain.S(task, "url") != "https://business.example.com/health" || domain.S(task, "address") != "cdn.example.net" || domain.S(task, "mode") != "http" {
				t.Fatal("business Host/SNI target contract lost", task)
			}
		}
	}
	if !found {
		t.Fatal("no route probe dispatched", tasks)
	}
	count := len(tasks)
	s.ScheduleTick(ctx)
	tasks, _ = s.Store.List(ctx, "tasks")
	if len(tasks) != count {
		t.Fatalf("scheduled duplicate not-due tasks: before=%d after=%d", count, len(tasks))
	}
}
