package agenthub

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/SnowBall-Bqiu/NewNexus/internal/config"
	"github.com/SnowBall-Bqiu/NewNexus/internal/domain"
	"github.com/SnowBall-Bqiu/NewNexus/internal/events"
	"github.com/SnowBall-Bqiu/NewNexus/internal/store"
	"github.com/SnowBall-Bqiu/NewNexus/internal/testutil"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
)

func TestDispatchDeduplicatesConcurrentActiveTasks(t *testing.T) {
	database, err := store.Open(config.Config{DatabaseDriver: "sqlite", DatabaseURL: filepath.Join(t.TempDir(), "agent-dispatch.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	hub := New(database, events.NewBroker())
	start := make(chan struct{})
	errors := make(chan error, 2)
	var workers sync.WaitGroup
	for index := 0; index < 2; index++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			errors <- hub.Dispatch("node-one", Task{CandidateID: "candidate-one", Mode: "tcp", Address: "1.1.1.1"})
		}()
	}
	close(start)
	workers.Wait()
	close(errors)
	succeeded := 0
	for err := range errors {
		if err == nil {
			succeeded++
		}
	}
	if succeeded != 1 {
		t.Fatalf("expected exactly one active task creation, got %d", succeeded)
	}
	var count int64
	if err := database.DB.Model(&domain.AgentTask{}).Where("node_id = ? AND candidate_id = ? AND status = ?", "node-one", "candidate-one", "queued").Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("expected one active task, got %d", count)
	}
}

func TestRecoveryCodeIsOneTimeAndRevokedNodesCannotRecover(t *testing.T) {
	database, err := store.Open(config.Config{DatabaseDriver: "sqlite", DatabaseURL: filepath.Join(t.TempDir(), "agent-recovery.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	hub := New(database, events.NewBroker())
	node := domain.ProbeNode{Name: "recoverable", Status: "re_enroll_required"}
	if err := database.DB.Create(&node).Error; err != nil {
		t.Fatal(err)
	}
	code, _, err := hub.CreateRecoveryCode(node.ID)
	if err != nil {
		t.Fatal(err)
	}
	request, _ := structpb.NewStruct(map[string]any{"code": code, "nodeId": node.ID, "recoveryCode": code, "protocolVersion": "agent.v1"})
	if _, err := hub.Enroll(context.Background(), request); err != nil {
		t.Fatalf("first recovery failed: %v", err)
	}
	if _, err := hub.Enroll(context.Background(), request); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("recovery code reuse should be denied, got %v", err)
	}
	if err := hub.RevokeNode(node.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := hub.CreateRecoveryCode(node.ID); err == nil {
		t.Fatal("revoked node unexpectedly received a recovery code")
	}
}

func TestCredentialRotationCommitsOnlyAfterAck(t *testing.T) {
	database, err := store.Open(config.Config{DatabaseDriver: "sqlite", DatabaseURL: filepath.Join(t.TempDir(), "agent-rotation.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	hub := New(database, events.NewBroker())
	node := domain.ProbeNode{Name: "rotation", Status: "online", AgentTokenHash: "old-token-hash"}
	if err := database.DB.Create(&node).Error; err != nil {
		t.Fatal(err)
	}
	responses := make(chan *structpb.Struct, 1)
	hub.responses[node.ID] = responses
	type result struct {
		token string
		err   error
	}
	done := make(chan result, 1)
	go func() {
		token, _, err := hub.RotateCredentials(node.ID)
		done <- result{token: token, err: err}
	}()
	message := <-responses
	rotationID := fieldString(message, "rotationId")
	var pending domain.ProbeNode
	if err := database.DB.First(&pending, "id = ?", node.ID).Error; err != nil {
		t.Fatal(err)
	}
	if pending.AgentTokenHash != "old-token-hash" || pending.PendingTokenHash == "" || rotationID == "" {
		t.Fatalf("rotation committed before ACK: %#v", pending)
	}
	ack, _ := structpb.NewStruct(map[string]any{"type": "credential_rotation_ack", "rotationId": rotationID})
	if _, err := hub.handleMessage(node.ID, ack); err != nil {
		t.Fatal(err)
	}
	rotation := <-done
	if rotation.err != nil || rotation.token == "" {
		t.Fatalf("rotation failed after ACK: %+v", rotation)
	}
	var committed domain.ProbeNode
	if err := database.DB.First(&committed, "id = ?", node.ID).Error; err != nil {
		t.Fatal(err)
	}
	if committed.AgentTokenHash == "old-token-hash" || committed.PendingTokenHash != "" || committed.RotationID != "" {
		t.Fatalf("rotation was not committed: %#v", committed)
	}
}

func TestCredentialRotationTimeoutRestoresOldToken(t *testing.T) {
	database, err := store.Open(config.Config{DatabaseDriver: "sqlite", DatabaseURL: filepath.Join(t.TempDir(), "agent-rotation-timeout.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	hub := New(database, events.NewBroker())
	hub.rotationWait = 10 * time.Millisecond
	node := domain.ProbeNode{Name: "rotation-timeout", Status: "online", AgentTokenHash: "old-token-hash"}
	if err := database.DB.Create(&node).Error; err != nil {
		t.Fatal(err)
	}
	hub.responses[node.ID] = make(chan *structpb.Struct, 2)
	if _, _, err := hub.RotateCredentials(node.ID); err == nil {
		t.Fatal("rotation without ACK unexpectedly succeeded")
	}
	var restored domain.ProbeNode
	if err := database.DB.First(&restored, "id = ?", node.ID).Error; err != nil {
		t.Fatal(err)
	}
	if restored.AgentTokenHash != "old-token-hash" || restored.PendingTokenHash != "" || restored.RotationID != "" {
		t.Fatalf("old token was not restored: %#v", restored)
	}
}

func TestDefaultQuorumUsesExactTwoThirds(t *testing.T) {
	for _, test := range []struct {
		votes, total int
		want         bool
	}{{1, 2, false}, {2, 2, true}, {2, 3, true}, {1, 3, false}, {3, 4, true}, {2, 4, false}} {
		if got := meetsQuorum(test.votes, test.total, 67); got != test.want {
			t.Fatalf("meetsQuorum(%d, %d, 67) = %v, want %v", test.votes, test.total, got, test.want)
		}
	}
}

func TestResultBatchIsInsertedOnce(t *testing.T) {
	database, err := store.Open(config.Config{DatabaseDriver: "sqlite", DatabaseURL: filepath.Join(t.TempDir(), "agent-batch.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	node := domain.ProbeNode{Name: "batch-node", Status: "online"}
	if err := database.DB.Create(&node).Error; err != nil {
		t.Fatal(err)
	}
	hub := New(database, events.NewBroker())
	batch := []any{
		map[string]any{"jobId": "job-one", "mode": "tcp", "success": true, "tcpMs": 12.0},
		map[string]any{"jobId": "job-two", "mode": "http", "success": false, "errorCode": "timeout"},
	}
	acked, err := hub.handleResultBatch(node.ID, batch)
	if err != nil {
		t.Fatal(err)
	}
	if len(acked) != 2 {
		t.Fatalf("unexpected acknowledgements: %#v", acked)
	}
	assertBatchCounts(t, database, node.ID, 2, 2)

	if _, err := hub.handleResultBatch(node.ID, batch); err != nil {
		t.Fatal(err)
	}
	assertBatchCounts(t, database, node.ID, 2, 2)
}

func TestPostgresPartitionedResultBatchIsInsertedOnce(t *testing.T) {
	databaseURL := testutil.IsolatedPostgresURL(t)
	database, err := store.Open(config.Config{DatabaseDriver: "postgres", DatabaseURL: databaseURL})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	node := domain.ProbeNode{Name: "postgres-batch-node", Status: "online"}
	if err := database.DB.Create(&node).Error; err != nil {
		t.Fatal(err)
	}
	hub := New(database, events.NewBroker())
	batch := []any{map[string]any{"jobId": "postgres-job-one", "mode": "tcp", "success": true, "tcpMs": 12.0}}
	if _, err := hub.handleResultBatch(node.ID, batch); err != nil {
		t.Fatal(err)
	}
	if _, err := hub.handleResultBatch(node.ID, batch); err != nil {
		t.Fatal(err)
	}
	assertBatchCounts(t, database, node.ID, 1, 1)
}

func assertBatchCounts(t *testing.T, database *store.Store, nodeID string, samples, completed int64) {
	t.Helper()
	var count int64
	if err := database.DB.Model(&domain.ProbeSample{}).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != samples {
		t.Fatalf("sample count = %d, want %d", count, samples)
	}
	var node domain.ProbeNode
	if err := database.DB.First(&node, "id = ?", nodeID).Error; err != nil {
		t.Fatal(err)
	}
	if node.TasksCompleted != completed {
		t.Fatalf("tasks completed = %d, want %d", node.TasksCompleted, completed)
	}
}
