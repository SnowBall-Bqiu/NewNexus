package agenthub

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/newnexus/newnexus/internal/agentproto"
	"github.com/newnexus/newnexus/internal/auth"
	"github.com/newnexus/newnexus/internal/domain"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/structpb"
)

func TestFrameUpdatesPreserveOwnershipCredentialsAndCounters(t *testing.T) {
	h, _ := fixture(t)
	checkFrameTransitions(t, h)
}

func checkFrameTransitions(t *testing.T, h *Hub) {
	t.Helper()
	ctx := context.Background()
	nodeID := domain.ID()
	oldHash, newHash := auth.Hash("old-test-credential"), auth.Hash("new-test-credential")
	node := domain.Doc{"id": nodeID, "status": "online", "mode": "http", "tasks_completed": 72, "credential_hash": oldHash, "pending_credential_hash": newHash, "pending_credential_encrypted": "ciphertext"}
	if err := h.Store.Put(ctx, "nodes", node); err != nil {
		t.Fatal(err)
	}
	_, results := batchFixture(t, h, nodeID, 2)
	first, second := domain.S(results[0], "task_id"), domain.S(results[1], "task_id")
	if err := h.acknowledgeTasks(ctx, "foreign-node", []string{first}); err != nil {
		t.Fatal(err)
	}
	task, _ := h.Store.Get(ctx, "tasks", first)
	if domain.S(task, "status") != "queued" {
		t.Fatal("foreign node accepted task")
	}
	if err := h.acknowledgeTasks(ctx, nodeID, []string{first, first}); err != nil {
		t.Fatal(err)
	}
	task, _ = h.Store.Get(ctx, "tasks", first)
	accepted, version := domain.S(task, "accepted_at"), domain.N(task, "_version")
	if accepted == "" || domain.S(task, "status") != "accepted" {
		t.Fatal("ACK did not persist acceptance")
	}
	if err := h.acknowledgeTasks(ctx, nodeID, []string{first}); err != nil {
		t.Fatal(err)
	}
	task, _ = h.Store.Get(ctx, "tasks", first)
	if domain.S(task, "accepted_at") != accepted || domain.N(task, "_version") != version {
		t.Fatal("duplicate ACK extended accepted retry window")
	}
	task["status"] = "done"
	if err := h.Store.Put(ctx, "tasks", task); err != nil {
		t.Fatal(err)
	}
	if err := h.acknowledgeTasks(ctx, nodeID, []string{first}); err != nil {
		t.Fatal(err)
	}
	task, _ = h.Store.Get(ctx, "tasks", first)
	if domain.S(task, "status") != "done" {
		t.Fatal("delayed ACK resurrected completed task")
	}
	if err := h.backpressureFrame(ctx, nodeID, domain.Doc{"pending_tasks": 10000, "outbox_size": 20, "retry_after_seconds": 5}); err != nil {
		t.Fatal(err)
	}
	if err := h.heartbeatFrame(ctx, nodeID, domain.Doc{"version": "test", "mode": "all", "pending_tasks": 0, "outbox_size": 0, "capabilities": []any{"tcp", "http", "http", "untrusted"}}); err != nil {
		t.Fatal(err)
	}
	node, _ = h.Store.Get(ctx, "nodes", nodeID)
	if domain.S(node, "mode") != "http" || domain.S(node, "agent_mode") != "all" || domain.N(node, "tasks_completed") != 72 || domain.S(node, "credential_hash") != oldHash || domain.B(node, "backpressure") {
		t.Fatalf("heartbeat overwrote administration or counters: %+v", node)
	}
	if err := h.credentialAckFrame(ctx, nodeID, "unrelated"); err != nil {
		t.Fatal(err)
	}
	node, _ = h.Store.Get(ctx, "nodes", nodeID)
	if domain.S(node, "credential_hash") != oldHash {
		t.Fatal("unrelated ACK changed credential")
	}
	if err := h.credentialAckFrame(ctx, nodeID, newHash); err != nil {
		t.Fatal(err)
	}
	node, _ = h.Store.Get(ctx, "nodes", nodeID)
	if domain.S(node, "credential_hash") != newHash || node["pending_credential_hash"] != nil || node["pending_credential_encrypted"] != nil || domain.N(node, "tasks_completed") != 72 {
		t.Fatalf("credential ACK corrupted node: %+v", node)
	}
	if acquired, err := h.Store.Lease(ctx, "node-connect:"+nodeID, "old-gateway", time.Minute); err != nil || !acquired {
		t.Fatal("lease fixture:", acquired, err)
	}
	if _, err := h.Store.DB.ExecContext(ctx, h.Store.Q(`UPDATE job_leases SET owner=? WHERE id=?`), "new-gateway", "node-connect:"+nodeID); err != nil {
		t.Fatal(err)
	}
	if err := h.patchFrameNodeForLease(ctx, nodeID, "old-gateway", domain.Doc{"status": "offline"}); err != nil {
		t.Fatal(err)
	}
	node, _ = h.Store.Get(ctx, "nodes", nodeID)
	if domain.S(node, "status") != "online" {
		t.Fatal("delayed old-gateway disconnect marked reconnected node offline")
	}
	node["status"] = "revoked"
	if err := h.Store.Put(ctx, "nodes", node); err != nil {
		t.Fatal(err)
	}
	if err := h.heartbeatFrame(ctx, nodeID, domain.Doc{"version": "late"}); !errors.Is(err, errFrameNodeRevoked) {
		t.Fatal("heartbeat revived revoked node", err)
	}
	if err := h.acknowledgeTasks(ctx, nodeID, []string{second}); err != nil {
		t.Fatal(err)
	}
	task, _ = h.Store.Get(ctx, "tasks", second)
	if domain.S(task, "status") != "queued" {
		t.Fatal("revoked node accepted queued task")
	}
	if err := h.patchFrameNode(ctx, nodeID, domain.Doc{"status": "offline"}); !errors.Is(err, errFrameNodeRevoked) {
		t.Fatal("disconnect revived revoked node", err)
	}
}

func TestBidirectionalFlowControlDoesNotBlockResultIngestion(t *testing.T) {
	h, enrolled := fixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	listener := bufconn.Listen(65536)
	t.Cleanup(func() { listener.Close() })
	server := grpc.NewServer(grpc.StaticStreamWindowSize(65535), grpc.StaticConnWindowSize(65535))
	agentproto.RegisterAgentServiceServer(server, h)
	go server.Serve(listener)
	t.Cleanup(server.Stop)
	conn, err := grpc.DialContext(ctx, "passthrough:///flow-test", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }), grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithStaticStreamWindowSize(65535), grpc.WithStaticConnWindowSize(65535), grpc.WithBlock())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	stream, err := agentproto.NewAgentServiceClient(conn).Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	send := func(frame map[string]any) {
		t.Helper()
		wire, err := structpb.NewStruct(frame)
		if err != nil {
			t.Fatal(err)
		}
		if err := stream.Send(wire); err != nil {
			t.Fatal("bidirectional flow stalled:", err)
		}
	}
	node := domain.S(enrolled, "node_id")
	send(map[string]any{"type": "hello", "node_id": node, "credential": enrolled["credential"], "protocol_version": 1})
	if _, err := stream.Recv(); err != nil {
		t.Fatal(err)
	}
	candidate := domain.ID()
	if err := h.Store.Put(ctx, "candidates", domain.Doc{"id": candidate, "address": "1.1.1.1"}); err != nil {
		t.Fatal(err)
	}
	var tasks []domain.Doc
	for i := 0; i < 64; i++ {
		tasks = append(tasks, domain.Doc{"id": domain.ID(), "node_id": node, "candidate_id": candidate, "body": strings.Repeat("p", 65536)})
	}
	if err := h.DispatchBatch(ctx, tasks); err != nil {
		t.Fatal(err)
	}
	var received []string
	for i := 0; i < 40; i++ {
		frame, err := stream.Recv()
		if err != nil {
			t.Fatal(err)
		}
		task, ok := frame.AsMap()["task"].(map[string]any)
		if !ok {
			t.Fatal("expected replayed task")
		}
		received = append(received, task["id"].(string))
	}
	// Stop reading downstream tasks. More than the server's inbound frame
	// queue fills with ACKs while the outbound HTTP/2 window is also full.
	for _, task := range received {
		send(map[string]any{"type": "ack", "task_id": task})
	}
	send(map[string]any{"type": "results", "batch_id": "large-flow-batch", "results": []any{map[string]any{"id": agentproto.ResultID(node, received[0]), "task_id": received[0], "observed_at": domain.Now(), "success": true, "sample_count": 3, "padding": strings.Repeat("x", 1<<20)}}})
	for {
		frame, err := stream.Recv()
		if err != nil {
			t.Fatal(err)
		}
		if frame.AsMap()["batch_id"] == "large-flow-batch" {
			break
		}
	}
	var count int
	if err := h.Store.DB.QueryRow(`SELECT COUNT(*) FROM metrics`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("large result not ingested: count=%d error=%v", count, err)
	}
}
