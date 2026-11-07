package agenthub

import (
	"context"
	"github.com/newnexus/newnexus/internal/agentproto"
	"github.com/newnexus/newnexus/internal/domain"
	"github.com/newnexus/newnexus/internal/optimizer"
	"github.com/newnexus/newnexus/internal/testutil"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
	"path/filepath"
	"testing"
	"time"
)

func fixture(t *testing.T) (*Hub, domain.Doc) {
	t.Helper()
	s := testutil.Open(t, filepath.Join(t.TempDir(), "hub.db"))
	h := New(s)
	token, e := h.Enrollment(context.Background(), time.Hour, "node")
	if e != nil {
		t.Fatal(e)
	}
	req, _ := structpb.NewStruct(map[string]any{"enrollment_token": token["token"], "name": "node", "protocol_version": 1})
	resp, e := h.Enroll(context.Background(), req)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = h.Enroll(context.Background(), req); status.Code(e) != codes.Unauthenticated {
		t.Fatal("token reused", e)
	}
	return h, domain.Doc(resp.AsMap())
}
func TestEnrollmentResultIdempotencyAndWeightedSamples(t *testing.T) {
	h, enrolled := fixture(t)
	ctx := context.Background()
	candidate := domain.Doc{"id": domain.ID(), "address": "1.1.1.1", "tier": "inventory"}
	if e := h.Store.Put(ctx, "candidates", candidate); e != nil {
		t.Fatal(e)
	}
	task := domain.Doc{"id": domain.ID(), "candidate_id": candidate["id"], "address": "1.1.1.1"}
	if e := h.Dispatch(ctx, domain.S(enrolled, "node_id"), task); e != nil {
		t.Fatal(e)
	}
	result := domain.Doc{"id": agentproto.ResultID(domain.S(enrolled, "node_id"), domain.S(task, "id")), "task_id": task["id"], "candidate_id": candidate["id"], "success": true, "success_rate": 1., "sample_count": 3, "tcp_ms": 10., "tls_ms": 20., "ttfb_ms": 30., "download_bps": 1000000., "observed_at": domain.Now()}
	for i := 0; i < 2; i++ {
		if e := h.Ingest(ctx, domain.S(enrolled, "node_id"), []domain.Doc{result}); e != nil {
			t.Fatal(e)
		}
	}
	var count int
	if e := h.Store.DB.QueryRow(`SELECT COUNT(*) FROM metrics`).Scan(&count); e != nil || count != 1 {
		t.Fatal("duplicate metric", count, e)
	}
	if _, e := optimizer.RefreshPending(ctx, h.Store, 100); e != nil {
		t.Fatal(e)
	}
	d, _ := h.Store.Get(ctx, "candidates", domain.S(candidate, "id"))
	if !domain.B(d, "healthy") || domain.N(d, "sample_count") != 3 {
		t.Fatal(d)
	}
	if e := h.Ingest(ctx, "other-node", []domain.Doc{result}); e == nil {
		t.Fatal("accepted result for another node")
	}
	if e := h.Revoke(ctx, domain.S(enrolled, "node_id")); e != nil {
		t.Fatal(e)
	}
	if _, e := h.authenticate(ctx, domain.S(enrolled, "node_id"), domain.S(enrolled, "credential")); status.Code(e) != codes.Unauthenticated {
		t.Fatal(e)
	}
}
func TestExpiredOutboxCanDrain(t *testing.T) {
	h, enrolled := fixture(t)
	ctx := context.Background()
	task := domain.Doc{"id": domain.ID(), "candidate_id": "old", "address": "1.1.1.1"}
	_ = h.Dispatch(ctx, domain.S(enrolled, "node_id"), task)
	result := domain.Doc{"id": agentproto.ResultID(domain.S(enrolled, "node_id"), domain.S(task, "id")), "task_id": task["id"], "observed_at": time.Now().UTC().Add(-8 * 24 * time.Hour).Format(time.RFC3339Nano)}
	if e := h.Ingest(ctx, domain.S(enrolled, "node_id"), []domain.Doc{result}); e != nil {
		t.Fatal(e)
	}
	stored, _ := h.Store.Get(ctx, "tasks", domain.S(task, "id"))
	if domain.S(stored, "status") != "done" || !domain.B(stored, "result_expired") {
		t.Fatal(stored)
	}
}
