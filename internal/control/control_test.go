package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/newnexus/newnexus/internal/domain"
	"github.com/newnexus/newnexus/internal/provider"
	"github.com/newnexus/newnexus/internal/security"
	"github.com/newnexus/newnexus/internal/store"
	"github.com/newnexus/newnexus/internal/testutil"
)

type cloudFixture struct {
	mu                                       sync.Mutex
	rows                                     []provider.Record
	writes, failAt, hostDeletes, hostCreates int
	remote                                   domain.Doc
	fallback                                 string
	createdPayload                           domain.Doc
	hostReject                               int
}

func (f *cloudFixture) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	respond := func(result any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(domain.Doc{"success": true, "result": result})
	}
	path := r.URL.Path
	switch {
	case path == "/zones":
		respond([]domain.Doc{{"id": "z1", "name": "example.com", "account": domain.Doc{"id": "test-account"}}})
	case strings.HasSuffix(path, "/custom_hostnames/quota"):
		respond(domain.Doc{"allocated": 1, "used": 1, "exceeded": true, "hard_cap": 2})
	case strings.HasSuffix(path, "/custom_hostnames/fallback_origin"):
		if r.Method == http.MethodPut {
			d := domain.Doc{}
			_ = json.NewDecoder(r.Body).Decode(&d)
			f.fallback = domain.S(d, "origin")
		}
		respond(domain.Doc{"origin": f.fallback, "status": "active"})
	case path == "/zones/z1/custom_hostnames":
		if r.Method == http.MethodGet {
			respond([]domain.Doc{})
			return
		}
		f.hostCreates++
		if f.hostReject != 0 {
			w.WriteHeader(f.hostReject)
			_, _ = w.Write([]byte(`{"success":false,"errors":[{"code":1003,"message":"quota enforced; secret_token must not escape"}]}`))
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&f.createdPayload)
		f.remote = domain.Doc{"id": "host-provider-id", "hostname": f.createdPayload["hostname"], "status": "pending", "ssl": domain.Doc{"status": "pending_validation"}}
		respond(f.remote)
	case path == "/zones/z1/custom_hostnames/host-provider-id":
		if r.Method == http.MethodDelete {
			f.hostDeletes++
			respond(domain.Doc{"id": "host-provider-id"})
			return
		}
		respond(f.remote)
	case path == "/zones/z1/dns_records" && r.Method == http.MethodGet:
		out := []domain.Doc{}
		for _, v := range f.rows {
			out = append(out, domain.Doc{"id": v.ID, "name": v.Name, "type": v.Type, "content": v.Value, "ttl": v.TTL, "proxied": v.Proxied})
		}
		respond(out)
	case strings.Contains(path, "/dns_records"):
		f.writes++
		if f.failAt > 0 && f.writes == f.failAt {
			w.WriteHeader(403)
			_, _ = w.Write([]byte(`{"success":false,"errors":[{"code":10000}]}`))
			return
		}
		if r.Method == http.MethodDelete {
			id := path[strings.LastIndex(path, "/")+1:]
			out := []provider.Record{}
			for _, v := range f.rows {
				if v.ID != id {
					out = append(out, v)
				}
			}
			f.rows = out
			respond(domain.Doc{"id": id})
			return
		}
		d := domain.Doc{}
		_ = json.NewDecoder(r.Body).Decode(&d)
		id := fmt.Sprintf("record-%d", f.writes)
		if r.Method == http.MethodPut {
			id = path[strings.LastIndex(path, "/")+1:]
			out := []provider.Record{}
			for _, v := range f.rows {
				if v.ID != id {
					out = append(out, v)
				}
			}
			f.rows = out
		}
		f.rows = append(f.rows, provider.Record{ID: id, Name: domain.S(d, "name"), Type: domain.S(d, "type"), Value: domain.S(d, "content"), TTL: int(domain.N(d, "ttl")), Line: "default", Weight: 1, Proxied: domain.B(d, "proxied")})
		respond(domain.Doc{"id": id})
	default:
		tell := domain.Doc{"success": false, "errors": []domain.Doc{{"code": 1000}}}
		w.WriteHeader(404)
		_ = json.NewEncoder(w).Encode(tell)
	}
}
func fixture(t *testing.T, testDSN ...string) (*Service, *cloudFixture, domain.Doc) {
	t.Helper()
	var db *store.Store
	if len(testDSN) > 0 {
		var err error
		db, err = store.Open(testDSN[0])
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.DB.Close() })
	} else {
		db = testutil.Open(t, filepath.Join(t.TempDir(), "control.db"))
	}
	vault, e := security.NewVault(make([]byte, 32))
	if e != nil {
		t.Fatal(e)
	}
	connection := domain.Doc{"id": domain.ID(), "name": "test connection", "provider": "cloudflare"}
	raw, _ := json.Marshal(domain.Doc{"api_token": "secret-not-in-results"})
	connection["credentials_encrypted"], e = vault.Seal(raw, "connection:"+domain.S(connection, "id"))
	if e != nil {
		t.Fatal(e)
	}
	if e = db.Put(context.Background(), "connections", connection); e != nil {
		t.Fatal(e)
	}
	state := &cloudFixture{remote: domain.Doc{"id": "host-provider-id", "hostname": "customer.example.com", "status": "active", "ssl": domain.Doc{"status": "pending_validation"}}, fallback: "fallback.example.com"}
	server := httptest.NewServer(http.HandlerFunc(state.serve))
	t.Cleanup(server.Close)
	s := New(db, vault)
	s.Adapter = func(kind string, credentials domain.Doc) *provider.Adapter {
		a := provider.New(kind, credentials)
		a.BaseURL = server.URL
		a.Client = server.Client()
		return a
	}
	s.Resolve = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("1.1.1.1")}, nil
	}
	s.Verify = func(context.Context, string, []provider.Record) error { return nil }
	return s, state, connection
}
func put(t *testing.T, s *Service, kind string, d domain.Doc) domain.Doc {
	t.Helper()
	if e := s.Store.Put(context.Background(), kind, d); e != nil {
		t.Fatal(e)
	}
	return d
}
func zone(t *testing.T, s *Service, c domain.Doc) domain.Doc {
	return put(t, s, "saas/zones", domain.Doc{"connection_id": c["id"], "zone_id": "z1", "zone_name": "example.com", "fallback_origin": "fallback.example.com", "cname_target": "edge.example.net"})
}

func TestPublicationPartialFailureCompensatesOnlyRealizedRecords(t *testing.T) {
	s, state, c := fixture(t)
	state.failAt = 2
	desired := []provider.Record{{Name: "a.example.com", Type: "A", Value: "1.1.1.1", TTL: 300, Line: "default", Weight: 1}, {Name: "b.example.com", Type: "A", Value: "8.8.8.8", TTL: 300, Line: "default", Weight: 1}}
	release, e := s.makeRelease(context.Background(), "saas/hostnames", "test", domain.S(c, "id"), "z1", "example.com", desired, false)
	if e != nil {
		t.Fatal(e)
	}
	result, e := s.Run(context.Background(), "apply", "releases", release)
	if e == nil {
		t.Fatal("expected second provider mutation to fail")
	}
	if domain.S(result, "status") != "failed" || !domain.B(result, "rolled_back_after_failure") {
		t.Fatalf("compensation state: %+v %v", result, e)
	}
	if len(state.rows) != 0 || state.writes != 3 {
		t.Fatalf("partial publication was not rolled back: %+v writes=%d", state.rows, state.writes)
	}
}

func TestAuthoritativeFailureRollsBackAndDriftBeforeApplyDoesNotWrite(t *testing.T) {
	s, state, c := fixture(t)
	desired := []provider.Record{{Name: "a.example.com", Type: "A", Value: "1.1.1.1", TTL: 300, Line: "default", Weight: 1}}
	release, e := s.makeRelease(context.Background(), "saas/hostnames", "test", domain.S(c, "id"), "z1", "example.com", desired, false)
	if e != nil {
		t.Fatal(e)
	}
	checks := 0
	s.Verify = func(_ context.Context, _ string, records []provider.Record) error {
		checks++
		if len(records) > 0 && records[0].Value != "" {
			return errors.New("authoritative result mismatch")
		}
		return nil
	}
	_, e = s.Run(context.Background(), "apply", "releases", release)
	if e == nil || len(state.rows) != 0 || checks != 2 {
		t.Fatalf("API success alone must not publish: err=%v rows=%+v checks=%d", e, state.rows, checks)
	}
	s.Verify = func(context.Context, string, []provider.Record) error { return nil }
	release, e = s.makeRelease(context.Background(), "saas/hostnames", "test", domain.S(c, "id"), "z1", "example.com", desired, false)
	if e != nil {
		t.Fatal(e)
	}
	state.rows = []provider.Record{{ID: "external", Name: "a.example.com", Type: "A", Value: "8.8.8.8", TTL: 300, Line: "default", Weight: 1}}
	writes := state.writes
	_, e = s.Run(context.Background(), "apply", "releases", release)
	if e == nil || state.writes != writes {
		t.Fatal("preview drift must prevent any provider mutation")
	}
}

func TestSaaSActivationReReadsBothStatesAndCleanupProtectsExternalEdits(t *testing.T) {
	s, state, c := fixture(t)
	z := zone(t, s, c)
	h := put(t, s, "saas/hostnames", domain.Doc{"saas_zone_id": z["id"], "hostname": "customer.example.com", "provider_id": "host-provider-id", "status": "active", "ssl_status": "active", "automation_mode": "manual", "dns_connection_id": c["id"], "dns_zone_id": "z1", "dns_zone_name": "example.com"})
	if _, e := s.Run(context.Background(), "activate", "saas/hostnames", h); e == nil || state.writes != 0 {
		t.Fatal("stale local active status must never permit cutover")
	}
	state.remote["ssl"] = domain.Doc{"status": "active"}
	h, _ = s.Store.Get(context.Background(), "saas/hostnames", domain.S(h, "id"))
	result, e := s.Run(context.Background(), "activate", "saas/hostnames", h)
	if e != nil {
		t.Fatal(e)
	}
	if domain.S(result, "ssl_status") != "active" || len(state.rows) != 1 || state.rows[0].Value != "edge.example.net" || state.rows[0].Proxied {
		t.Fatalf("invalid cutover: %+v rows=%+v", result, state.rows)
	}
	original := state.rows[0].Value
	state.rows[0].Value = "external.example.net"
	h, _ = s.Store.Get(context.Background(), "saas/hostnames", domain.S(h, "id"))
	if _, e = s.Run(context.Background(), "delete", "saas/hostnames", h); e == nil || state.hostDeletes != 0 {
		t.Fatal("cleanup must stop before deleting externally edited DNS or the remote hostname")
	}
	state.rows[0].Value = original
	h, _ = s.Store.Get(context.Background(), "saas/hostnames", domain.S(h, "id"))
	if _, e = s.Run(context.Background(), "delete", "saas/hostnames", h); e != nil {
		t.Fatal(e)
	}
	if len(state.rows) != 0 || state.hostDeletes != 1 {
		t.Fatal("managed DNS and remote custom hostname were not removed")
	}
}

func TestSaaSSoftQuotaIsInformationalAndSecretsAreNotReturned(t *testing.T) {
	s, state, c := fixture(t)
	z := zone(t, s, c)
	h := put(t, s, "saas/hostnames", domain.Doc{"saas_zone_id": z["id"], "hostname": "new.example.com", "validation_method": "txt", "automation_mode": "manual"})
	result, e := s.Run(context.Background(), "create", "saas/hostnames", h)
	if e != nil {
		t.Fatal(e)
	}
	if state.hostCreates != 1 || domain.S(result, "provider_id") == "" || domain.S(domain.M(state.createdPayload, "ssl"), "method") != "txt" {
		t.Fatalf("valid create at soft quota was blocked or malformed: %+v", result)
	}
	connection, e := s.Run(context.Background(), "validate", "connections", c)
	if e != nil {
		t.Fatal(e)
	}
	encoded, _ := json.Marshal(connection)
	if strings.Contains(string(encoded), "secret-not-in-results") || strings.Contains(string(encoded), "credentials_encrypted") {
		t.Fatal("connection response leaked stored credentials")
	}
}

func TestValidationChecklistIncludesOwnershipAndCertificateTXTAndHTTP(t *testing.T) {
	remote := domain.Doc{"ownership_verification": domain.Doc{"name": "_cf-custom-hostname.customer.example.com", "value": "ownership"}, "ownership_verification_http": domain.Doc{"http_url": "http://customer.example.com/.well-known/cf-custom-hostname-challenge/token", "http_body": "ownership"}, "ssl": domain.Doc{"validation_records": []domain.Doc{{"txt_name": "_acme-challenge.customer.example.com", "txt_value": "certificate", "http_url": "http://customer.example.com/.well-known/acme-challenge/token", "http_body": "certificate"}}}}
	records := validationChecklist(remote)
	if len(records) != 4 {
		t.Fatalf("lost a prevalidation step: %+v", records)
	}
}

func TestRulePreviewIsImmutableAndAppliedReleaseRollsBack(t *testing.T) {
	s, state, c := fixture(t)
	target := domain.Doc{"id": "target", "cname": "edge.example.net", "enabled": true, "priority": 0, "weight": 1, "health": "healthy"}
	rule := put(t, s, "rules", domain.Doc{"name": "production", "hostname": "www.example.com", "zone_id": "z1", "zone_name": "example.com", "dns_connection_id": c["id"], "ttl": 300, "lines": []domain.Doc{{"provider_line_code": "default", "targets": []domain.Doc{target}}}})
	first, e := s.Run(context.Background(), "preview", "rules", rule)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Run(context.Background(), "apply", "releases", first); e != nil {
		t.Fatal(e)
	}
	rule, _ = s.Store.Get(context.Background(), "rules", domain.S(rule, "id"))
	domain.A(domain.A(rule, "lines")[0], "targets")[0]["cname"] = "new.example.net"
	put(t, s, "rules", rule)
	second, e := s.Run(context.Background(), "preview", "rules", rule)
	if e != nil {
		t.Fatal(e)
	}
	if len(changes(second, "diff")) != 1 || changes(second, "diff")[0].Action != "update" {
		t.Fatalf("one-to-one switch should update in place: %+v", second)
	}
	if _, e = s.Run(context.Background(), "apply", "releases", second); e != nil {
		t.Fatal(e)
	}
	if state.rows[0].Value != "new.example.net" {
		t.Fatal("new target was not published")
	}
	if _, e = s.Run(context.Background(), "rollback", "releases", second); e != nil {
		t.Fatal(e)
	}
	if state.rows[0].Value != "edge.example.net" {
		t.Fatal("rollback did not restore reviewed old target")
	}
	third, e := s.Run(context.Background(), "preview", "rules", rule)
	if e != nil {
		t.Fatal(e)
	}
	domain.A(domain.A(rule, "lines")[0], "targets")[0]["cname"] = "changed-after-review.example.net"
	rule, _ = s.Store.Get(context.Background(), "rules", domain.S(rule, "id"))
	domain.A(domain.A(rule, "lines")[0], "targets")[0]["cname"] = "changed-after-review.example.net"
	put(t, s, "rules", rule)
	writes := state.writes
	if _, e = s.Run(context.Background(), "apply", "releases", third); e == nil || writes != state.writes {
		t.Fatal("changed configuration must invalidate the reviewed diff")
	}
}

func TestOptimizedHostnamePublishesOnlyFreshQualifyingTopNAndRemovesOldFamily(t *testing.T) {
	s, state, c := fixture(t)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, candidate := range []domain.Doc{{"address": "8.8.8.8", "family": 4, "score": 80, "last_probed_at": now}, {"address": "1.1.1.1", "family": 4, "score": 100, "last_probed_at": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)}, {"address": "203.0.113.5", "family": 4, "score": 100, "last_probed_at": now}, {"address": "2606:4700:4700::1111", "family": 6, "score": 90, "last_probed_at": now}} {
		candidate["project_id"] = "project"
		candidate["healthy"] = true
		candidate["sample_count"] = 5
		candidate["success_rate"] = 1
		put(t, s, "candidates", candidate)
	}
	hostname := put(t, s, "optimized-hostnames", domain.Doc{"project_id": "project", "hostname": "optimized.example.com", "zone_id": "z1", "zone_name": "example.com", "dns_connection_id": c["id"], "ttl": 300, "family": 4, "top_n": 1})
	release, e := s.Run(context.Background(), "preview", "optimized-hostnames", hostname)
	if e != nil {
		t.Fatal(e)
	}
	desired := records(release, "desired")
	if len(desired) != 1 || desired[0].Value != "8.8.8.8" || desired[0].Proxied {
		t.Fatalf("topN qualification/DNS-only failed: %+v", desired)
	}
	if _, e = s.Run(context.Background(), "apply", "releases", release); e != nil {
		t.Fatal(e)
	}
	hostname, _ = s.Store.Get(context.Background(), "optimized-hostnames", domain.S(hostname, "id"))
	hostname["family"] = 6
	put(t, s, "optimized-hostnames", hostname)
	release, e = s.Run(context.Background(), "preview", "optimized-hostnames", hostname)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Run(context.Background(), "apply", "releases", release); e != nil {
		t.Fatal(e)
	}
	if len(state.rows) != 1 || state.rows[0].Type != "AAAA" {
		t.Fatalf("changing family left obsolete managed A records: %+v", state.rows)
	}
}

func TestPostgresPublicationCompensationAndZoneLease(t *testing.T) {
	dsn := os.Getenv("NEWNEXUS_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("set NEWNEXUS_TEST_POSTGRES for an isolated PostgreSQL integration database")
	}
	s, state, c := fixture(t, dsn)
	ctx := context.Background()
	connection := domain.S(c, "id")
	owner := domain.ID()
	state.failAt = 2
	t.Cleanup(func() {
		for _, kind := range []string{"managed-dns", "releases", "connections"} {
			all, _ := s.Store.List(context.Background(), kind)
			for _, d := range all {
				if domain.S(d, "connection_id") == connection || kind == "connections" && domain.S(d, "id") == connection {
					_ = s.Store.Delete(context.Background(), kind, domain.S(d, "id"))
				}
			}
		}
	})
	desired := []provider.Record{{Name: "pg-a.example.com", Type: "A", Value: "1.1.1.1", TTL: 300, Line: "default", Weight: 1}, {Name: "pg-b.example.com", Type: "A", Value: "8.8.8.8", TTL: 300, Line: "default", Weight: 1}}
	release, e := s.makeRelease(ctx, "saas/hostnames", owner, connection, "z1", "example.com", desired, false)
	if e != nil {
		t.Fatal(e)
	}
	leaseCtx, unlock, e := s.locked(ctx, connection+":z1")
	if e != nil {
		t.Fatal(e)
	}
	if leaseCtx.Err() != nil {
		t.Fatal(leaseCtx.Err())
	}
	if _, _, e = s.locked(ctx, connection+":z1"); e == nil {
		unlock()
		t.Fatal("a competing publisher acquired the same zone lease")
	}
	unlock()
	result, e := s.Run(ctx, "apply", "releases", release)
	if e == nil || !domain.B(result, "rolled_back_after_failure") || len(state.rows) != 0 {
		t.Fatalf("Postgres compensated publication failed: %+v %v", result, e)
	}
}
