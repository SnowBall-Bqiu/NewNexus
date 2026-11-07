package agenthub

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/newnexus/newnexus/internal/auth"
	"github.com/newnexus/newnexus/internal/domain"
	"github.com/newnexus/newnexus/internal/store"
	"github.com/newnexus/newnexus/internal/testutil"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
)

func enrollToken(t *testing.T, h *Hub, token string) (domain.Doc, error) {
	t.Helper()
	req, _ := structpb.NewStruct(map[string]any{"enrollment_token": token, "protocol_version": 1, "name": "untrusted replacement name", "region": "changed-region"})
	out, err := h.Enroll(context.Background(), req)
	if err != nil {
		return nil, err
	}
	return domain.Doc(out.AsMap()), nil
}

func TestNodeLifecycle(t *testing.T) { h, enrolled := fixture(t); checkNodeLifecycle(t, h, enrolled) }
func TestMySQLNodeLifecycle(t *testing.T) {
	h := New(testutil.MySQL(t))
	tok, err := h.Enrollment(context.Background(), time.Hour, "node")
	if err != nil {
		t.Fatal(err)
	}
	enrolled, err := enrollToken(t, h, domain.S(tok, "token"))
	if err != nil {
		t.Fatal(err)
	}
	checkNodeLifecycle(t, h, enrolled)
}

func checkNodeLifecycle(t *testing.T, h *Hub, enrolled domain.Doc) {
	t.Helper()
	ctx := context.Background()
	id := domain.S(enrolled, "node_id")
	node, err := h.Store.Get(ctx, "nodes", id)
	if err != nil {
		t.Fatal(err)
	}
	node["name"] = "administrator name"
	node["group_name"] = "edge"
	node["region"] = "cn"
	node["carrier"] = "telecom"
	node["mode"] = "http"
	node["tasks_completed"] = 72
	node["status"] = "online"
	if err = h.Store.Put(ctx, "nodes", node); err != nil {
		t.Fatal(err)
	}
	if ok, e := h.Store.Lease(ctx, "node-connect:"+id, "old-gateway", time.Minute); e != nil || !ok {
		t.Fatal(ok, e)
	}
	oldctx := fencedContext(ctx, int64(domain.N(node, "session_generation")), "old-gateway")
	_, results := batchFixture(t, h, id, 2)
	taskID := domain.S(results[0], "task_id")
	if err = h.Ingest(oldctx, id, results[:1]); err != nil {
		t.Fatal(err)
	}
	first, err := h.Redeploy(ctx, id, 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.authenticate(ctx, id, domain.S(enrolled, "credential")); status.Code(err) != codes.Unauthenticated {
		t.Fatal("old credential survived", err)
	}
	second, err := h.Redeploy(ctx, id, 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = enrollToken(t, h, domain.S(first, "token")); status.Code(err) != codes.Unauthenticated {
		t.Fatal("superseded token accepted", err)
	}
	newIdentity, err := enrollToken(t, h, domain.S(second, "token"))
	if err != nil {
		t.Fatal(err)
	}
	if domain.S(newIdentity, "node_id") != id {
		t.Fatal("redeployment changed identity")
	}
	if _, err = enrollToken(t, h, domain.S(second, "token")); status.Code(err) != codes.Unauthenticated {
		t.Fatal("redeployment token reused", err)
	}
	node, err = h.Store.Get(ctx, "nodes", id)
	if err != nil {
		t.Fatal(err)
	}
	if domain.S(node, "name") != "administrator name" || domain.S(node, "region") != "cn" || domain.S(node, "group_name") != "edge" || domain.S(node, "mode") != "http" || domain.S(node, "carrier") != "telecom" || domain.N(node, "tasks_completed") != 73 || domain.S(node, "status") != "offline" {
		t.Fatalf("redeployment lost administration/history: %+v", node)
	}
	if _, err = h.authenticate(ctx, id, domain.S(newIdentity, "credential")); err != nil {
		t.Fatal(err)
	}
	if ok, e := h.Store.Lease(ctx, "node-connect:"+id, "new-gateway", time.Minute); e != nil || !ok {
		t.Fatal(ok, e)
	}
	newctx := fencedContext(ctx, int64(domain.N(node, "session_generation")), "new-gateway")
	if err = h.heartbeatFrame(newctx, id, domain.Doc{"version": "new-session"}); err != nil {
		t.Fatal(err)
	}
	// Even deliberately copying a current pending ID into an old frame must
	// not rotate the new node's credentials.
	node, _ = h.Store.Get(ctx, "nodes", id)
	node["pending_credential_hash"] = auth.Hash("new pending")
	node["pending_credential_encrypted"] = "ciphertext"
	if err = h.Store.Put(ctx, "nodes", node); err != nil {
		t.Fatal(err)
	}
	if err = h.heartbeatFrame(oldctx, id, domain.Doc{"version": "old-session"}); !errors.Is(err, errFrameNodeRevoked) {
		t.Fatal("old heartbeat unfenced", err)
	}
	if err = h.backpressureFrame(oldctx, id, domain.Doc{}); !errors.Is(err, errFrameNodeRevoked) {
		t.Fatal("old backpressure unfenced", err)
	}
	if err = h.credentialAckFrame(oldctx, id, auth.Hash("new pending")); err != nil {
		t.Fatal(err)
	}
	if err = h.acknowledgeTasks(oldctx, id, []string{domain.S(results[1], "task_id")}); err != nil {
		t.Fatal(err)
	}
	if err = h.Ingest(oldctx, id, results[1:]); !errors.Is(err, errFrameNodeRevoked) {
		t.Fatal("old result unfenced", err)
	}
	if err = h.patchFrameNodeForLease(oldctx, id, "old-gateway", domain.Doc{"status": "offline"}); err != nil {
		t.Fatal(err)
	}
	node, _ = h.Store.Get(ctx, "nodes", id)
	if domain.S(node, "version") != "new-session" || domain.S(node, "status") != "online" || domain.S(node, "pending_credential_hash") != auth.Hash("new pending") || domain.B(node, "backpressure") || domain.N(node, "tasks_completed") != 73 {
		t.Fatalf("old stream changed replacement: %+v", node)
	}
	task, _ := h.Store.Get(ctx, "tasks", domain.S(results[1], "task_id"))
	if domain.S(task, "status") != "queued" {
		t.Fatal("old ACK accepted task")
	}
	// Same-generation lease takeover is fenced too, including results.
	staleOwnerCtx := fencedContext(ctx, int64(domain.N(node, "session_generation")), "old-gateway")
	if err = h.Ingest(staleOwnerCtx, id, results[1:]); !errors.Is(err, errFrameNodeRevoked) {
		t.Fatal("old lease ingested result", err)
	}
	group := domain.Doc{"id": domain.ID(), "name": "group", "node_ids": []any{id, "other"}}
	if err = h.Store.Put(ctx, "node-groups", group); err != nil {
		t.Fatal(err)
	}
	staleGroup, _ := h.Store.Get(ctx, "node-groups", domain.S(group, "id"))
	pending, err := h.Redeploy(ctx, id, 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err = h.DeleteNode(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err = h.Store.Get(ctx, "nodes", id); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("node not deleted", err)
	}
	if _, err = enrollToken(t, h, domain.S(pending, "token")); status.Code(err) != codes.Unauthenticated {
		t.Fatal("deleted node restored by token", err)
	}
	if _, err = h.authenticate(ctx, id, domain.S(newIdentity, "credential")); status.Code(err) != codes.Unauthenticated {
		t.Fatal("deleted credential accepted", err)
	}
	if err = h.heartbeatFrame(newctx, id, domain.Doc{}); !errors.Is(err, errFrameNodeRevoked) {
		t.Fatal("deleted node restored by heartbeat", err)
	}
	if err = h.Dispatch(ctx, id, domain.Doc{"id": domain.ID()}); err == nil {
		t.Fatal("scheduler recreated deleted node work")
	}
	if _, err = h.Store.Get(ctx, "tasks", domain.S(results[1], "task_id")); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("unfinished task retained", err)
	}
	if task, err = h.Store.Get(ctx, "tasks", taskID); err != nil || domain.S(task, "status") != "done" {
		t.Fatal("historical task removed", err)
	}
	group, _ = h.Store.Get(ctx, "node-groups", domain.S(group, "id"))
	members, _ := group["node_ids"].([]any)
	if len(members) != 1 || members[0] != "other" {
		t.Fatal("stale membership retained", group)
	}
	if err = h.Store.Put(ctx, "node-groups", staleGroup); !errors.Is(err, store.ErrConflict) {
		t.Fatal("stale editor restored membership", err)
	}
	var metrics int
	if err = h.Store.DB.QueryRow(`SELECT COUNT(*) FROM metrics WHERE node_id=`+h.Store.Q(`?`), id).Scan(&metrics); err != nil || metrics != 1 {
		t.Fatal("historical metric removed", metrics, err)
	}
}

func TestEnrollmentConsumptionIsAtomic(t *testing.T) {
	h, _ := fixture(t)
	checkConcurrentEnrollment(t, h)
}
func TestMySQLEnrollmentConsumptionIsAtomic(t *testing.T) {
	checkConcurrentEnrollment(t, New(testutil.MySQL(t)))
}
func checkConcurrentEnrollment(t *testing.T, h *Hub) {
	t.Helper()
	ctx := context.Background()
	token, err := h.Enrollment(ctx, time.Hour, "concurrent")
	if err != nil {
		t.Fatal(err)
	}
	before := h.Store.Count(ctx, "nodes")
	var success atomic.Int32
	var wg sync.WaitGroup
	gate := make(chan struct{})
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-gate
			if _, err := enrollToken(t, h, domain.S(token, "token")); err == nil {
				success.Add(1)
			}
		}()
	}
	close(gate)
	wg.Wait()
	if success.Load() != 1 || h.Store.Count(ctx, "nodes") != before+1 {
		t.Fatal("token consumption was not atomic", success.Load())
	}
}

func TestRedeploymentExpiryAndRevoke(t *testing.T) {
	h, enrolled := fixture(t)
	ctx := context.Background()
	id := domain.S(enrolled, "node_id")
	if _, err := h.Redeploy(ctx, id, time.Hour); err == nil {
		t.Fatal("unbounded redeployment lifetime")
	}
	tok, err := h.Redeploy(ctx, id, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	row, _ := h.Store.Get(ctx, "enrollment-tokens", auth.Hash(domain.S(tok, "token")))
	row["expires_at"] = time.Now().Add(-time.Second).Format(time.RFC3339Nano)
	if err = h.Store.Put(ctx, "enrollment-tokens", row); err != nil {
		t.Fatal(err)
	}
	if _, err = enrollToken(t, h, domain.S(tok, "token")); status.Code(err) != codes.Unauthenticated {
		t.Fatal("expired token accepted", err)
	}
	tok, err = h.Redeploy(ctx, id, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err = h.Revoke(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err = enrollToken(t, h, domain.S(tok, "token")); status.Code(err) != codes.Unauthenticated {
		t.Fatal("revoke retained redeployment token", err)
	}
}

func TestDeleteSerializesWithEnrollmentAndRedeployment(t *testing.T) {
	h, _ := fixture(t)
	checkConcurrentDelete(t, h)
}
func TestMySQLDeleteSerializesWithEnrollmentAndRedeployment(t *testing.T) {
	checkConcurrentDelete(t, New(testutil.MySQL(t)))
}
func checkConcurrentDelete(t *testing.T, h *Hub) {
	t.Helper()
	ctx := context.Background()
	for range 8 {
		created, err := h.Enrollment(ctx, time.Hour, "race")
		if err != nil {
			t.Fatal(err)
		}
		identity, err := enrollToken(t, h, domain.S(created, "token"))
		if err != nil {
			t.Fatal(err)
		}
		id := domain.S(identity, "node_id")
		pending, err := h.Redeploy(ctx, id, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		gate := make(chan struct{})
		var wg sync.WaitGroup
		var deletion error
		var replacement domain.Doc
		for operation := 0; operation < 3; operation++ {
			wg.Add(1)
			go func(operation int) {
				defer wg.Done()
				<-gate
				switch operation {
				case 0:
					deletion = h.DeleteNode(ctx, id)
				case 1:
					_, _ = enrollToken(t, h, domain.S(pending, "token"))
				case 2:
					replacement, _ = h.Redeploy(ctx, id, time.Minute)
				}
			}(operation)
		}
		close(gate)
		wg.Wait()
		if deletion != nil {
			t.Fatal("delete failed under lifecycle contention", deletion)
		}
		if _, err = h.Store.Get(ctx, "nodes", id); !errors.Is(err, store.ErrNotFound) {
			t.Fatal("lifecycle race restored deleted node", err)
		}
		for _, token := range []string{domain.S(pending, "token"), domain.S(replacement, "token")} {
			if token != "" {
				if _, err = enrollToken(t, h, token); err == nil {
					t.Fatal("racing token restored deleted node")
				}
			}
		}
	}
}

func TestFailedEnrollmentDoesNotConsumeToken(t *testing.T) {
	h, _ := fixture(t)
	if h.Store.Driver != "sqlite" {
		t.Skip("SQLite failure injection")
	}
	ctx := context.Background()
	token, err := h.Enrollment(ctx, time.Minute, "atomic")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.Store.DB.Exec(`CREATE TRIGGER fail_node_enrollment BEFORE INSERT ON resources WHEN NEW.kind='nodes' BEGIN SELECT RAISE(ABORT,'injected enrollment failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err = enrollToken(t, h, domain.S(token, "token")); err == nil {
		t.Fatal("failure injection did not execute")
	}
	row, err := h.Store.Get(ctx, "enrollment-tokens", auth.Hash(domain.S(token, "token")))
	if err != nil || domain.B(row, "used") {
		t.Fatal("failed enrollment consumed token", err)
	}
	if _, err = h.Store.DB.Exec(`DROP TRIGGER fail_node_enrollment`); err != nil {
		t.Fatal(err)
	}
	if _, err = enrollToken(t, h, domain.S(token, "token")); err != nil {
		t.Fatal("rolled back token could not be consumed", err)
	}
}
