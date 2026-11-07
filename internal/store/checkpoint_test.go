package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/newnexus/newnexus/internal/domain"
)

func exerciseLeaseCheckpoint(t *testing.T, s *Store) {
	t.Helper()
	ctx := t.Context()
	leaseID := "checkpoint-" + domain.ID()
	if held, err := s.Lease(ctx, leaseID, "leader", time.Minute); err != nil || !held {
		t.Fatal(held, err)
	}
	doc := domain.Doc{"id": domain.ID(), "finished_at": domain.Now(), "attempt": "first"}
	if err := s.PutUnderLease(ctx, leaseID, "leader", "system-state", doc); err != nil {
		t.Fatal(err)
	}
	firstVersion := doc["_version"]
	stale := domain.Doc{"id": doc["id"], "attempt": "stale"}
	if err := s.PutUnderLease(ctx, leaseID, "leader", "system-state", stale); !errors.Is(err, ErrConflict) || stale["_version"] != nil {
		t.Fatal("fenced checkpoint bypassed CAS or advanced failed version", stale, err)
	}
	if _, err := s.DB.ExecContext(ctx, s.Q("UPDATE job_leases SET expires_at=? WHERE id=?"), s.Time(time.Now().Add(-time.Hour)), leaseID); err != nil {
		t.Fatal(err)
	}
	doc["attempt"] = "expired"
	if err := s.PutUnderLease(ctx, leaseID, "leader", "system-state", doc); !errors.Is(err, ErrLeaseLost) || doc["_version"] != firstVersion {
		t.Fatal("expired leader checkpoint accepted", err)
	}
	if held, err := s.Lease(ctx, leaseID, "replacement", time.Minute); err != nil || !held {
		t.Fatal(held, err)
	}
	if err := s.PutUnderLease(ctx, leaseID, "leader", "system-state", doc); !errors.Is(err, ErrLeaseLost) {
		t.Fatal("superseded leader checkpoint accepted", err)
	}
	current, err := s.Get(ctx, "system-state", domain.S(doc, "id"))
	if err != nil || domain.S(current, "attempt") != "first" || current["_version"] != firstVersion {
		t.Fatal("rejected checkpoint changed completion", current, err)
	}
	current["attempt"] = "replacement"
	if err := s.PutUnderLease(ctx, leaseID, "replacement", "system-state", current); err != nil {
		t.Fatal(err)
	}
	missing := domain.Doc{"id": domain.ID()}
	if err := s.PutUnderLease(ctx, "missing", "replacement", "system-state", missing); !errors.Is(err, ErrLeaseLost) || missing["_version"] != nil {
		t.Fatal("missing lease accepted", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := s.PutUnderLease(canceled, leaseID, "replacement", "system-state", current); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled checkpoint accepted", err)
	}
}

func TestSQLiteLeaseCheckpointFencesOwnershipExpiryCASAndCancellation(t *testing.T) {
	exerciseLeaseCheckpoint(t, testStore(t))
}

func TestMySQLLeaseCheckpointFencesOwnershipExpiryCASAndCancellation(t *testing.T) {
	exerciseLeaseCheckpoint(t, mysqlTestStore(t))
}

func TestMySQLLeaseCheckpointLocksLeaseUntilCheckpointCommit(t *testing.T) {
	s := mysqlTestStore(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	leaseID := domain.ID()
	if held, err := s.Lease(ctx, leaseID, "leader", time.Minute); err != nil || !held {
		t.Fatal(held, err)
	}
	doc := domain.Doc{"id": domain.ID(), "attempt": "before"}
	if err := s.Put(ctx, "system-state", doc); err != nil {
		t.Fatal(err)
	}
	gate, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Rollback()
	var version int64
	if err = gate.QueryRowContext(ctx, "SELECT version FROM resources WHERE kind='system-state' AND id=? FOR UPDATE", doc["id"]).Scan(&version); err != nil {
		t.Fatal(err)
	}
	doc["attempt"] = "completed"
	done := make(chan error, 1)
	go func() { done <- s.PutUnderLease(ctx, leaseID, "leader", "system-state", doc) }()
	// Once the checkpoint blocks on its resource row, the same transaction
	// must retain the lease lock. A NOWAIT current read proves another leader
	// cannot slip between ownership validation and the checkpoint commit.
	for {
		probe, err := s.DB.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		var owner string
		err = probe.QueryRowContext(ctx, "SELECT owner FROM job_leases WHERE id=? FOR UPDATE NOWAIT", leaseID).Scan(&owner)
		probe.Rollback()
		var failure *mysql.MySQLError
		if errors.As(err, &failure) && failure.Number == 3572 {
			break
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			t.Fatal(err)
		}
		select {
		case err := <-done:
			t.Fatal("checkpoint escaped resource gate", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err = gate.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	current, err := s.Get(ctx, "system-state", domain.S(doc, "id"))
	if err != nil || domain.S(current, "attempt") != "completed" {
		t.Fatal(current, err)
	}
	if _, err = s.DB.ExecContext(ctx, "UPDATE job_leases SET owner='replacement' WHERE id=?", leaseID); err != nil {
		t.Fatal(err)
	}
	current["attempt"] = "old-resumed"
	if err = s.PutUnderLease(ctx, leaseID, "leader", "system-state", current); !errors.Is(err, ErrLeaseLost) {
		t.Fatal("old leader wrote after takeover", err)
	}
}

func TestMySQLSharedLeaseCheckpointAllows16IndependentWriters(t *testing.T) {
	s := mysqlTestStore(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	leaseID := domain.ID()
	if held, err := s.Lease(ctx, leaseID, "routing", time.Minute); err != nil || !held {
		t.Fatal(held, err)
	}
	docs := make([]domain.Doc, 16)
	for i := range docs {
		docs[i] = domain.Doc{"id": domain.ID(), "status": "before"}
		if err := s.Put(ctx, "rules", docs[i]); err != nil {
			t.Fatal(err)
		}
		docs[i]["status"] = "after"
	}
	gate, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Rollback()
	var version int64
	if err = gate.QueryRowContext(ctx, "SELECT version FROM resources WHERE kind='rules' AND id=? FOR UPDATE", docs[0]["id"]).Scan(&version); err != nil {
		t.Fatal(err)
	}
	blocked := make(chan error, 1)
	go func() { blocked <- s.PutUnderSharedLease(ctx, leaseID, "routing", "rules", docs[0]) }()
	for {
		probe, err := s.DB.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		var owner string
		err = probe.QueryRowContext(ctx, "SELECT owner FROM job_leases WHERE id=? FOR UPDATE NOWAIT", leaseID).Scan(&owner)
		probe.Rollback()
		var failure *mysql.MySQLError
		if errors.As(err, &failure) && failure.Number == 3572 {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-blocked:
			t.Fatal("checkpoint bypassed gate", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	// The first writer retains the shared lease lock while waiting on its
	// rule. All other 15 independent rule writes must commit before it can.
	done := make(chan error, 15)
	start := make(chan struct{})
	for i := 1; i < len(docs); i++ {
		go func(i int) { <-start; done <- s.PutUnderSharedLease(ctx, leaseID, "routing", "rules", docs[i]) }(i)
	}
	close(start)
	for i := 1; i < len(docs); i++ {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal("shared lease serialized unrelated rule writes", ctx.Err())
		}
	}
	select {
	case err := <-blocked:
		t.Fatal("held rule lock was bypassed", err)
	default:
	}
	if err = gate.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = <-blocked; err != nil {
		t.Fatal(err)
	}
	for _, doc := range docs {
		got, err := s.Get(ctx, "rules", domain.S(doc, "id"))
		if err != nil || domain.S(got, "status") != "after" {
			t.Fatal("independent checkpoint lost", got, err)
		}
	}
}
