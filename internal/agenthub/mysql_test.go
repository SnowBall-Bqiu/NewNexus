package agenthub

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/newnexus/newnexus/internal/agentproto"
	"github.com/newnexus/newnexus/internal/auth"
	"github.com/newnexus/newnexus/internal/domain"
	"github.com/newnexus/newnexus/internal/security"
	"github.com/newnexus/newnexus/internal/store"
	"github.com/newnexus/newnexus/internal/testutil"
)

func openOtherMySQL(t *testing.T, database string) *store.Store {
	t.Helper()
	u, err := url.Parse(os.Getenv("NEWNEXUS_TEST_MYSQL"))
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + database
	db, err := store.Open(u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.DB.Close() })
	return db
}

func TestMySQLBatchConcurrentFirstInsert(t *testing.T) {
	db := testutil.MySQL(t)
	h := New(db)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	node := domain.ID()
	if err := db.Put(ctx, "nodes", domain.Doc{"id": node, "status": "online", "tasks_completed": 0, "name": "administrator name", "mode": "http", "probe_samples_total": nil}); err != nil {
		t.Fatal(err)
	}
	_, results := batchFixture(t, h, node, 64)
	// Independent connection pools model two gateways, including MySQL's
	// default REPEATABLE READ isolation on both first-insert transactions.
	var database string
	if err := db.DB.QueryRow(`SELECT DATABASE()`).Scan(&database); err != nil {
		t.Fatal(err)
	}
	otherDB := openOtherMySQL(t, database)
	gate := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, hub := range []*Hub{h, New(otherDB)} {
		wg.Add(1)
		go func(hub *Hub) { defer wg.Done(); <-gate; errs <- hub.ingestBatch(ctx, node, results) }(hub)
	}
	close(gate)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	assertBatchCounts(t, h, 64, 64)
	n, err := db.Get(ctx, "nodes", node)
	if err != nil || domain.N(n, "tasks_completed") != 64 || domain.N(n, "probe_samples_total") != 192 || domain.S(n, "name") != "administrator name" || domain.S(n, "mode") != "http" {
		t.Fatalf("atomic counters/admin settings: %+v error=%v", n, err)
	}
	for _, field := range []string{"tasks_completed", "probe_samples_total", "probe_samples_failed", "failure_rate"} {
		var kind string
		if err := db.DB.QueryRow(`SELECT JSON_TYPE(JSON_EXTRACT(doc,?)) FROM resources WHERE kind='nodes' AND id=?`, "$."+field, node).Scan(&kind); err != nil || (kind != "INTEGER" && kind != "DOUBLE" && kind != "DECIMAL") {
			t.Fatalf("%s became non-numeric JSON %q: %v", field, kind, err)
		}
	}
	// Partition creation may use a separate connection only after rollback;
	// delayed outbox replay must also succeed with a one-connection pool.
	db.DB.SetMaxOpenConns(1)
	checkDelayedRetention(t, h, node)
	checkFrameTransitions(t, h)
}

func TestMySQLRevocationSerializesWithResultCommit(t *testing.T) {
	db := testutil.MySQL(t)
	h := New(db)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	node := domain.ID()
	if err := db.Put(ctx, "nodes", domain.Doc{"id": node, "status": "online", "credential_hash": "old", "pending_credential_hash": "new", "pending_credential_encrypted": "ciphertext"}); err != nil {
		t.Fatal(err)
	}
	candidate, results := batchFixture(t, h, node, 2)
	// Hold a real InnoDB candidate row. Ingestion reaches it after writing
	// metrics and locking the node, so no artificial PG trigger is required.
	gate, err := db.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Rollback()
	var raw string
	if err := gate.QueryRowContext(ctx, `SELECT doc FROM resources WHERE kind='candidates' AND id=? FOR UPDATE`, candidate).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	ingested := make(chan error, 1)
	go func() { ingested <- h.ingestBatch(ctx, node, results[:1]) }()
	for {
		var waiting int
		if err := db.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM performance_schema.data_locks WHERE OBJECT_SCHEMA=DATABASE() AND OBJECT_NAME='resources' AND LOCK_STATUS='WAITING' AND LOCK_DATA LIKE ?`, "%"+candidate+"%").Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			break
		}
		select {
		case err := <-ingested:
			t.Fatal("ingestion did not reach controlled InnoDB row gate:", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(5 * time.Millisecond):
		}
	}
	revoked := make(chan error, 1)
	go func() { revoked <- h.Revoke(ctx, node) }()
	select {
	case err := <-revoked:
		t.Fatal("revocation passed a still-uncommitted node transaction:", err)
	case <-time.After(50 * time.Millisecond):
	}
	if err := gate.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := <-ingested; err != nil {
		t.Fatal(err)
	}
	if err := <-revoked; err != nil {
		t.Fatal(err)
	}
	if err := h.ingestBatch(ctx, node, results[1:]); err == nil {
		t.Fatal("new metric accepted after revocation completed")
	}
	assertBatchCounts(t, h, 1, 1)
	n, err := db.Get(ctx, "nodes", node)
	if err != nil || domain.S(n, "status") != "revoked" || domain.S(n, "credential_hash") != "" || n["pending_credential_hash"] != nil || n["pending_credential_encrypted"] != nil {
		t.Fatalf("revocation credentials: %+v error=%v", n, err)
	}
}

func TestBatchRetryClassification(t *testing.T) {
	for _, code := range []uint16{1205, 1213} {
		if !retryableBatchError(&mysql.MySQLError{Number: code}) {
			t.Fatalf("transient MySQL error %d not retried", code)
		}
	}
	if retryableBatchError(&mysql.MySQLError{Number: 1062}) || retryableBatchError(errors.New("invalid result")) {
		t.Fatal("nontransient data error retried")
	}
}

func TestMySQLResultTimestampPartitionAndOrdering(t *testing.T) {
	db := testutil.MySQL(t)
	h := New(db)
	ctx := context.Background()
	node := domain.ID()
	if err := db.Put(ctx, "nodes", domain.Doc{"id": node, "status": "online"}); err != nil {
		t.Fatal(err)
	}
	candidate, results := batchFixture(t, h, node, 4)
	now := time.Now().UTC()
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -1)
	ats := []time.Time{day.Add(-time.Nanosecond), day, day.Add(100 * time.Millisecond), day.Add(110 * time.Millisecond)}
	for i := range results {
		results[i]["observed_at"] = ats[i].Format(time.RFC3339Nano)
		results[i]["tcp_ms"] = i + 1
	}
	if err := h.Ingest(ctx, node, results); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []struct {
		day   time.Time
		count int
	}{{day.AddDate(0, 0, -1), 1}, {day, 3}} {
		var count int
		partition := "metrics_" + expected.day.Format("20060102")
		if err := db.DB.QueryRow(`SELECT COUNT(*) FROM metrics PARTITION (` + partition + `)`).Scan(&count); err != nil || count != expected.count {
			t.Fatalf("UTC partition %s count=%d want=%d error=%v", partition, count, expected.count, err)
		}
	}
	rows, err := db.DB.Query(`SELECT observed_at FROM metrics ORDER BY observed_at`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	i := 0
	for rows.Next() {
		var at string
		if err := rows.Scan(&at); err != nil || i >= len(ats) || at != db.Time(ats[i]) {
			t.Fatalf("lexical timestamp order at %d: %q error=%v", i, at, err)
		}
		i++
	}
	if err := rows.Err(); err != nil || i != 4 {
		t.Fatalf("timestamps count=%d error=%v", i, err)
	}
	doc, err := db.Get(ctx, "candidates", candidate)
	if err != nil || domain.N(doc, "tcp_ms") != 4 || domain.S(doc, "last_probed_at") != db.Time(ats[3]) {
		t.Fatalf("latest timestamp patch: %+v error=%v", doc, err)
	}
}

func TestMySQLFirstMetricDayWithOnePoolConnection(t *testing.T) {
	db := testutil.MySQL(t)
	h := New(db)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var owners []string
	var batches [][]domain.Doc
	for i := 0; i < 8; i++ {
		node := domain.ID()
		if err := db.Put(ctx, "nodes", domain.Doc{"id": node, "status": "online"}); err != nil {
			t.Fatal(err)
		}
		_, batch := batchFixture(t, h, node, 1)
		owners = append(owners, node)
		batches = append(batches, batch)
	}
	db.DB.SetMaxOpenConns(1)
	start := make(chan struct{})
	errs := make(chan error, len(owners))
	for i := range owners {
		go func(i int) { <-start; errs <- h.ingestBatch(ctx, owners[i], batches[i]) }(i)
	}
	close(start)
	for range owners {
		if err := <-errs; err != nil {
			t.Fatal("first-day partition preparation must not hold cache locks while waiting for the pool:", err)
		}
	}
	assertBatchCounts(t, h, 8, 8)
	now := time.Now().UTC()
	if !h.knownMySQLMetricDay(now, now) || h.knownMySQLMetricDay(now, now.AddDate(0, 0, 2)) {
		t.Fatal("day cache must expire when the partition is no longer protected by minimum retention")
	}
}

func TestMySQLConcurrentIngestRotateAndRevoke(t *testing.T) {
	db := testutil.MySQL(t)
	h := New(db)
	h.Vault, _ = security.NewVault(make([]byte, 32))
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	node := domain.ID()
	if err := db.Put(ctx, "nodes", domain.Doc{"id": node, "status": "online", "name": "admin name", "mode": "http", "credential_hash": auth.Hash("original")}); err != nil {
		t.Fatal(err)
	}
	candidate, results := batchFixture(t, h, node, 2)
	gate, err := db.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Rollback()
	var raw string
	if err := gate.QueryRowContext(ctx, `SELECT doc FROM resources WHERE kind='candidates' AND id=? FOR UPDATE`, candidate).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	ingested := make(chan error, 1)
	go func() { ingested <- h.Ingest(ctx, node, results[:1]) }()
	waitMySQLLock(t, ctx, db, candidate)
	rotated := make(chan error, 1)
	go func() { rotated <- h.Rotate(ctx, node) }()
	waitMySQLLock(t, ctx, db, node)
	if !h.mu.TryLock() {
		t.Fatal("blocked node rotation held the entire hub's stream-map mutex")
	}
	h.mu.Unlock()
	if err := gate.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := <-ingested; err != nil {
		t.Fatal(err)
	}
	if err := <-rotated; err != nil {
		t.Fatal("rotation conflicted with an in-flight probe counter update:", err)
	}
	stored, err := db.Get(ctx, "nodes", node)
	if err != nil || domain.N(stored, "tasks_completed") != 1 || domain.S(stored, "name") != "admin name" || domain.S(stored, "mode") != "http" || domain.S(stored, "pending_credential_hash") == "" {
		t.Fatalf("rotation replayed a stale node document: %+v error=%v", stored, err)
	}
	if err := h.credentialAckFrame(ctx, node, domain.S(stored, "pending_credential_hash")); err != nil {
		t.Fatal(err)
	}
	// All three real mutations race for the node row. Either probe/rotation
	// may precede revoke; none may add data or credentials after it commits.
	start := make(chan struct{})
	probeDone, rotationDone, revokeDone := make(chan error, 1), make(chan error, 1), make(chan error, 1)
	go func() { <-start; probeDone <- h.Ingest(ctx, node, results[1:]) }()
	go func() { <-start; rotationDone <- h.Rotate(ctx, node) }()
	go func() { <-start; revokeDone <- h.Revoke(ctx, node) }()
	close(start)
	probeErr, rotationErr, revokeErr := <-probeDone, <-rotationDone, <-revokeDone
	if revokeErr != nil || (rotationErr != nil && !errors.Is(rotationErr, errFrameNodeRevoked)) || (probeErr != nil && !strings.Contains(probeErr.Error(), "revoked")) {
		t.Fatalf("concurrent mutation errors: ingest=%v rotate=%v revoke=%v", probeErr, rotationErr, revokeErr)
	}
	want := 2
	if probeErr != nil {
		want = 1
	}
	assertBatchCounts(t, h, want, want)
	h.streams[node] = make(chan domain.Doc, 1) // Disconnect cleanup may lag revoke.
	if err := h.Rotate(ctx, node); !errors.Is(err, errFrameNodeRevoked) {
		t.Fatal("late local stream reintroduced revoked pending credentials:", err)
	}
	stored, err = db.Get(ctx, "nodes", node)
	if err != nil || domain.S(stored, "status") != "revoked" || domain.S(stored, "credential_hash") != "" || stored["pending_credential_hash"] != nil || stored["pending_credential_encrypted"] != nil || domain.N(stored, "tasks_completed") != float64(want) {
		t.Fatalf("revoked state/counters: %+v error=%v", stored, err)
	}
}

func waitMySQLLock(t *testing.T, ctx context.Context, db *store.Store, identifier string) {
	t.Helper()
	for {
		var waiting int
		if err := db.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM performance_schema.data_locks WHERE OBJECT_SCHEMA=DATABASE() AND OBJECT_NAME='resources' AND LOCK_STATUS='WAITING' AND LOCK_DATA LIKE ?`, "%"+identifier+"%").Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("waiting for controlled InnoDB row lock:", ctx.Err())
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestMySQL256ResultBatchDoesNotLockUnrequestedResources(t *testing.T) {
	db := testutil.MySQL(t)
	h := New(db)
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	node := domain.ID()
	if err := db.Put(ctx, "nodes", domain.Doc{"id": node, "status": "online"}); err != nil {
		t.Fatal(err)
	}
	var candidates, tasks, results []domain.Doc
	var taskIDs []string
	for i := 0; i < 256; i++ {
		candidate, task := fmt.Sprintf("point-candidate-%06d", i), fmt.Sprintf("point-task-%06d", i)
		candidates = append(candidates, domain.Doc{"id": candidate, "address": "1.1.1.1", "tier": "active"})
		tasks = append(tasks, domain.Doc{"id": task, "node_id": node, "candidate_id": candidate, "status": "queued"})
		results = append(results, domain.Doc{"id": agentproto.ResultID(node, task), "task_id": task, "observed_at": domain.Now(), "success": true, "sample_count": 3, "success_rate": 1, "tcp_ms": 10})
		taskIDs = append(taskIDs, task)
	}
	// These keys lie inside the requested keys' lexical range, belong to the
	// same kinds/node, and are deliberately absent from the request set.
	outCandidate, outTask := "point-candidate-000128-outside", "point-task-000128-outside"
	candidates = append(candidates, domain.Doc{"id": outCandidate, "address": "1.0.0.1", "tier": "active"})
	tasks = append(tasks, domain.Doc{"id": outTask, "node_id": node, "candidate_id": outCandidate, "status": "queued"})
	if err := db.PutMany(ctx, "candidates", candidates); err != nil {
		t.Fatal(err)
	}
	if err := db.PutMany(ctx, "tasks", tasks); err != nil {
		t.Fatal(err)
	}
	gate, err := db.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Rollback()
	for _, resource := range []struct{ kind, id string }{{"candidates", outCandidate}, {"tasks", outTask}} {
		var raw string
		if err := gate.QueryRowContext(ctx, `SELECT doc FROM resources WHERE kind=? AND id=? FOR UPDATE`, resource.kind, resource.id).Scan(&raw); err != nil {
			t.Fatal(err)
		}
	}
	// Both operations must finish while the unrelated locks remain held.
	// JSON_TABLE + forced unique point lookups prevent RR next-key/kind scans.
	if err := h.acknowledgeTasks(ctx, node, taskIDs); err != nil {
		t.Fatal("256-task ACK waited for an unrequested task:", err)
	}
	if err := h.Ingest(ctx, node, results); err != nil {
		t.Fatal("atomic 256-result batch waited for an unrequested resource:", err)
	}
	assertBatchCounts(t, h, 256, 256)
	untouched, err := db.Get(ctx, "tasks", outTask)
	if err != nil || domain.S(untouched, "status") != "queued" {
		t.Fatalf("unrequested task was changed: %+v error=%v", untouched, err)
	}
	if err := gate.Rollback(); err != nil {
		t.Fatal(err)
	}
}
