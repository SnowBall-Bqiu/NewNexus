package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/newnexus/newnexus/internal/domain"
)

func batchRequest(t *testing.T, s *Server, path, key string, payload domain.Doc) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(payload)
	r := httptest.NewRequest("POST", "http://localhost/api/v1/"+path, bytes.NewReader(raw))
	r.Header.Set("Idempotency-Key", key)
	w := httptest.NewRecorder()
	if !s.bulk(w, r, path, domain.Doc{"username": "test"}) {
		t.Fatal("route unhandled")
	}
	return w
}
func TestBulkHostnamePartialSuccessAndReplay(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	z := domain.Doc{"id": domain.ID()}
	if e := s.Store.Put(ctx, "saas/zones", z); e != nil {
		t.Fatal(e)
	}
	payload := domain.Doc{"saas_zone_id": z["id"], "hostnames": []string{"Customer.Example.com.", "http://invalid.example.com", "customer.example.com", "second.example.com"}, "status": "active", "provider_id": "forged"}
	w := batchRequest(t, s, "saas/hostnames/bulk", "batch-one", payload)
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	result := domain.Doc{}
	_ = json.Unmarshal(w.Body.Bytes(), &result)
	if domain.N(result, "accepted") != 2 || len(domain.A(result, "errors")) != 2 || s.Store.Count(ctx, "jobs") != 2 || s.Store.Count(ctx, "saas/hostnames") != 2 {
		t.Fatal(result)
	}
	for _, item := range domain.A(result, "items") {
		if domain.S(item, "status") != "pending" || domain.S(item, "provider_id") != "" || domain.S(item, "job_id") == "" {
			t.Fatal("client fields bypassed server defaults", item)
		}
	}
	replay := batchRequest(t, s, "saas/hostnames/bulk", "batch-one", payload)
	if replay.Code != 202 || replay.Body.String() != w.Body.String() || s.Store.Count(ctx, "jobs") != 2 {
		t.Fatal("replay enqueued duplicates", replay.Code, replay.Body.String())
	}
	payload["hostnames"] = []string{"different.example.com"}
	w = batchRequest(t, s, "saas/hostnames/bulk", "batch-one", payload)
	if w.Code != 409 {
		t.Fatal("changed replay accepted", w.Code, w.Body.String())
	}
}
func TestBulkBoundsAndCleanupConfirmation(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	z := domain.Doc{"id": domain.ID()}
	_ = s.Store.Put(ctx, "saas/zones", z)
	h := domain.Doc{"id": domain.ID(), "hostname": "customer.example.com", "saas_zone_id": z["id"]}
	_ = s.Store.Put(ctx, "saas/hostnames", h)
	w := batchRequest(t, s, "saas/hostnames/bulk-delete", "delete-one", domain.Doc{"ids": []string{domain.S(h, "id")}})
	if w.Code != 400 || s.Store.Count(ctx, "jobs") != 0 {
		t.Fatal("cleanup was not confirmed", w.Code)
	}
	w = batchRequest(t, s, "saas/hostnames/bulk-delete", "delete-one", domain.Doc{"ids": []string{domain.S(h, "id"), "missing"}, "confirmed": true})
	result := domain.Doc{}
	_ = json.Unmarshal(w.Body.Bytes(), &result)
	if w.Code != 202 || domain.N(result, "accepted") != 1 || len(domain.A(result, "errors")) != 1 || s.Store.Count(ctx, "saas/hostnames") != 1 {
		t.Fatal("delete must queue independently without removing local state first", result)
	}
	values := make([]string, 501)
	w = batchRequest(t, s, "saas/hostnames/bulk", "oversize", domain.Doc{"saas_zone_id": z["id"], "hostnames": values})
	if w.Code != 400 || s.Store.Count(ctx, "jobs") != 1 {
		t.Fatal("oversized batch accepted", w.Code)
	}
	w = batchRequest(t, s, "saas/hostnames/bulk", "", domain.Doc{"saas_zone_id": z["id"], "hostnames": []string{"a.example.com"}})
	if w.Code != 400 {
		t.Fatal("missing idempotency accepted", w.Code)
	}
}
