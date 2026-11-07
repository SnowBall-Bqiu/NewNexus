package agentclient

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/newnexus/newnexus/internal/agenthub"
	"github.com/newnexus/newnexus/internal/domain"
	"github.com/newnexus/newnexus/internal/security"
	"github.com/newnexus/newnexus/internal/testutil"
)

// Exercise the production hub and real SQLite/MySQL repository over gRPC rather than
// only a protocol fixture. The probe socket remains an injected local pipe.
func TestProductionHubAgentRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	db := testutil.Open(t, filepath.Join(t.TempDir(), "control.db"))
	hub := agenthub.New(db)
	vault, err := security.NewVault(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	hub.Vault = vault
	token, err := hub.Enrollment(ctx, time.Hour, "node")
	if err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(t)
	cfg.EnrollmentToken = domain.S(token, "token")
	client := bufClient(t, hub)
	credential, err := enroll(ctx, cfg, client)
	if err != nil {
		t.Fatal(err)
	}
	outbox, err := OpenOutbox(cfg.DataDir, cfg.MaxPending, cfg.MaxOutboxBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer outbox.Close()
	if err := outbox.SaveCredentials(credential); err != nil {
		t.Fatal(err)
	}
	if err := db.Put(ctx, "candidates", domain.Doc{"id": "candidate-1", "address": "8.8.8.8", "tier": "inventory"}); err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	workerErrors := make(chan error, 1)
	startWorkers(ctx, cfg, outbox, credential.NodeID, &workers, workerErrors)
	done := make(chan error, 1)
	go func() { done <- runSession(ctx, cfg, client, outbox, &credential, workerErrors) }()
	defer func() { cancel(); workers.Wait() }()
	nodeID := credential.NodeID
	waitFor(t, ctx, func() bool {
		node, err := db.Get(ctx, "nodes", nodeID)
		return err == nil && domain.S(node, "status") == "online"
	})
	if err := hub.Dispatch(ctx, nodeID, domain.Doc{"id": "task-1", "candidate_id": "candidate-1", "address": "8.8.8.8", "mode": "tcp", "samples": 3}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, ctx, func() bool {
		task, err := db.Get(ctx, "tasks", "task-1")
		_, results, _ := outbox.Stats()
		return err == nil && domain.S(task, "status") == "done" && results == 0
	})
	var count int
	if err := db.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM metrics`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("result was not persisted once: count=%d error=%v", count, err)
	}
	previous, err := outbox.LoadCredentials()
	if err != nil {
		t.Fatal(err)
	}
	for round := 0; round < 2; round++ {
		if err := hub.Rotate(ctx, nodeID); err != nil {
			t.Fatal(err)
		}
		waitFor(t, ctx, func() bool {
			node, err := db.Get(ctx, "nodes", nodeID)
			return err == nil && domain.S(node, "pending_credential_hash") == ""
		})
		persisted, err := outbox.LoadCredentials()
		if err != nil || persisted.Credential == "" || persisted.Credential == previous.Credential {
			t.Fatal("successive confirmed rotation did not persist a new credential", err)
		}
		previous = persisted
	}
	if err := hub.Revoke(ctx, nodeID); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrRevoked) {
			t.Fatalf("revocation failed: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("agent did not process revocation")
	}
}

func waitFor(t *testing.T, ctx context.Context, condition func() bool) {
	t.Helper()
	timer := time.NewTicker(10 * time.Millisecond)
	defer timer.Stop()
	for !condition() {
		select {
		case <-ctx.Done():
			t.Fatal("timed out waiting for condition")
		case <-timer.C:
		}
	}
}
