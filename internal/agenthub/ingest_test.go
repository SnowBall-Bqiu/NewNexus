package agenthub

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/newnexus/newnexus/internal/agentproto"
	"github.com/newnexus/newnexus/internal/domain"
	"github.com/newnexus/newnexus/internal/store"
)

func batchFixture(t *testing.T, h *Hub, nodeID string, count int) (string, []domain.Doc) {
	t.Helper()
	ctx := context.Background()
	candidate := domain.ID()
	if err := h.Store.Put(ctx, "candidates", domain.Doc{"id": candidate, "address": "1.1.1.1", "tier": "active"}); err != nil {
		t.Fatal(err)
	}
	var tasks []domain.Doc
	var results []domain.Doc
	for i := 0; i < count; i++ {
		taskID := domain.ID()
		tasks = append(tasks, domain.Doc{"id": taskID, "node_id": nodeID, "candidate_id": candidate, "address": "1.1.1.1", "status": "queued"})
		results = append(results, domain.Doc{"id": agentproto.ResultID(nodeID, taskID), "task_id": taskID, "success": true, "sample_count": 3, "success_rate": 1, "tcp_ms": 10, "tls_ms": 20, "ttfb_ms": 30, "download_bps": 1024, "observed_at": domain.Now()})
	}
	if err := h.Store.PutMany(ctx, "tasks", tasks); err != nil {
		t.Fatal(err)
	}
	return candidate, results
}

func TestBatchValidationAndDatabaseErrorsRollBackEverything(t *testing.T) {
	h, enrolled := fixture(t)
	node := domain.S(enrolled, "node_id")
	_, results := batchFixture(t, h, node, 2)
	bad := domain.Clone(results[1])
	bad["task_id"] = "unassigned"
	if err := h.ingestBatch(context.Background(), node, []domain.Doc{results[0], bad}); err == nil {
		t.Fatal("accepted unassigned task")
	}
	assertBatchCounts(t, h, 0, 0)
	// Force a failure after result IDs/raw/aggregates/task/node SQL has run.
	trigger := `CREATE TRIGGER fail_candidate_batch BEFORE UPDATE ON resources WHEN OLD.kind='candidates' BEGIN SELECT RAISE(ABORT,'forced candidate patch failure'); END;`
	if h.Store.Driver == "mysql" {
		trigger = `CREATE TRIGGER fail_candidate_batch BEFORE UPDATE ON resources FOR EACH ROW BEGIN IF OLD.kind='candidates' THEN SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='forced candidate patch failure'; END IF; END`
	}
	if _, err := h.Store.DB.Exec(trigger); err != nil {
		t.Fatal(err)
	}
	if err := h.ingestBatch(context.Background(), node, results); err == nil {
		t.Fatal("database/document failure silently ACKed")
	}
	assertBatchCounts(t, h, 0, 0)
	n, err := h.Store.Get(context.Background(), "nodes", node)
	if err != nil || domain.N(n, "tasks_completed") != 0 {
		t.Fatalf("node counter escaped rollback: %+v %v", n, err)
	}
}

func TestBatchAggregatesDedupAndOutOfOrderPatch(t *testing.T) {
	h, enrolled := fixture(t)
	node := domain.S(enrolled, "node_id")
	candidate, results := batchFixture(t, h, node, 3)
	base := time.Now().UTC().Truncate(time.Minute)
	for i := range results {
		results[i]["observed_at"] = base.Add(time.Duration(i) * time.Second).Format(time.RFC3339Nano)
		results[i]["tcp_ms"] = (i + 1) * 10
	}
	results[1]["success_rate"] = .5
	results[1]["success"] = false
	results[2]["success_rate"] = 0.
	results[2]["success"] = false
	input := append(append([]domain.Doc(nil), results...), results[0])
	if err := h.ingestBatch(context.Background(), node, input); err != nil {
		t.Fatal(err)
	}
	assertBatchCounts(t, h, 3, 3)
	n, err := h.Store.Get(context.Background(), "nodes", node)
	if err != nil || domain.N(n, "tasks_completed") != 3 || domain.N(n, "probe_samples_total") != 9 || domain.N(n, "probe_samples_failed") != 4.5 || domain.N(n, "failure_rate") != .5 {
		t.Fatalf("node weighted failure telemetry incorrect: %+v %v", n, err)
	}
	for _, table := range []string{"metrics_5m", "metrics_1h"} {
		var samples int
		var successes, tcp float64
		if err := h.Store.DB.QueryRow(`SELECT SUM(sample_count),SUM(success_count),SUM(tcp_sum) FROM `+table).Scan(&samples, &successes, &tcp); err != nil || samples != 9 || successes != 4.5 || tcp != 180 {
			t.Fatalf("wrong %s aggregate: samples=%d success=%f tcp=%f error=%v", table, samples, successes, tcp, err)
		}
	}
	d, err := h.Store.Get(context.Background(), "candidates", candidate)
	if err != nil || !domain.B(d, "aggregate_pending") || domain.S(d, "aggregate_generation") == "" || domain.N(d, "tcp_ms") != 30 {
		t.Fatalf("missing pending/latest patch: %+v %v", d, err)
	}
	generation := domain.S(d, "aggregate_generation")
	if err := h.ingestBatch(context.Background(), node, results); err != nil {
		t.Fatal(err)
	}
	assertBatchCounts(t, h, 3, 3)
	_, older := batchFixture(t, h, node, 1)
	task, err := h.Store.Get(context.Background(), "tasks", domain.S(older[0], "task_id"))
	if err != nil {
		t.Fatal(err)
	}
	task["candidate_id"] = candidate
	if err := h.Store.Put(context.Background(), "tasks", task); err != nil {
		t.Fatal(err)
	}
	older[0]["observed_at"] = base.Add(-time.Minute).Format(time.RFC3339Nano)
	older[0]["tcp_ms"] = 999
	if err := h.ingestBatch(context.Background(), node, older); err != nil {
		t.Fatal(err)
	}
	d, err = h.Store.Get(context.Background(), "candidates", candidate)
	if err != nil || domain.N(d, "tcp_ms") != 30 || domain.S(d, "aggregate_generation") == generation {
		t.Fatalf("out-of-order result overwrote latest or lost aggregation generation: %+v %v", d, err)
	}
}

func TestBatchUsesConfiguredRetentionAndAcknowledgesExpired(t *testing.T) {
	h, enrolled := fixture(t)
	node := domain.S(enrolled, "node_id")
	checkDelayedRetention(t, h, node)
}

func TestResultIdentityCannotBeChangedOrRebound(t *testing.T) {
	h, enrolled := fixture(t)
	node := domain.S(enrolled, "node_id")
	_, results := batchFixture(t, h, node, 2)
	if err := h.ingestBatch(context.Background(), node, results[:1]); err != nil {
		t.Fatal(err)
	}
	changedID := domain.Clone(results[0])
	changedID["id"] = domain.ID()
	if err := h.ingestBatch(context.Background(), node, []domain.Doc{changedID}); err == nil {
		t.Fatal("same task accepted under a second result ID")
	}
	rebound := domain.Clone(results[1])
	rebound["id"] = results[0]["id"]
	if err := h.ingestBatch(context.Background(), node, []domain.Doc{rebound}); err == nil {
		t.Fatal("committed result ID rebound to another task")
	}
	assertBatchCounts(t, h, 1, 1)
	if err := h.ingestBatch(context.Background(), node, results); err != nil {
		t.Fatal(err)
	}
	assertBatchCounts(t, h, 2, 2)
}

func TestPostgresRevocationSerializesWithResultCommit(t *testing.T) {
	db, _ := isolatedBatchPostgres(t)
	h := New(db)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	node := domain.ID()
	if err := db.Put(ctx, "nodes", domain.Doc{"id": node, "status": "online", "credential_hash": "old", "pending_credential_hash": "new", "pending_credential_encrypted": "ciphertext"}); err != nil {
		t.Fatal(err)
	}
	_, results := batchFixture(t, h, node, 2)
	gate, err := db.DB.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Close()
	lockID := time.Now().UnixNano()
	if _, err := gate.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, lockID); err != nil {
		t.Fatal(err)
	}
	defer gate.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, lockID)
	function := fmt.Sprintf(`CREATE FUNCTION block_metric_commit() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_advisory_xact_lock(%d); RETURN NEW; END $$`, lockID)
	if _, err := db.DB.ExecContext(ctx, function); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.ExecContext(ctx, `CREATE TRIGGER block_metric_commit BEFORE INSERT ON metrics FOR EACH ROW EXECUTE FUNCTION block_metric_commit()`); err != nil {
		t.Fatal(err)
	}
	ingested := make(chan error, 1)
	go func() { ingested <- h.ingestBatch(ctx, node, results[:1]) }()
	// Wait until ingestion has locked the node and reached the raw INSERT.
	for {
		var waiting int
		if err := db.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM pg_locks WHERE locktype='advisory' AND NOT granted AND classid::bigint=$1 AND objid::bigint=$2`, lockID>>32, lockID&0xffffffff).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			break
		}
		select {
		case err := <-ingested:
			t.Fatal("ingest did not reach controlled commit gate:", err)
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
	if _, err := gate.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, lockID); err != nil {
		t.Fatal(err)
	}
	if err := <-ingested; err != nil {
		t.Fatal(err)
	}
	if err := <-revoked; err != nil {
		t.Fatal(err)
	}
	if err := h.ingestBatch(ctx, node, results[1:]); err == nil {
		t.Fatal("new result accepted after revocation completed")
	}
	assertBatchCounts(t, h, 1, 1)
	stored, err := db.Get(ctx, "nodes", node)
	if err != nil || domain.S(stored, "status") != "revoked" || domain.S(stored, "credential_hash") != "" || stored["pending_credential_hash"] != nil || stored["pending_credential_encrypted"] != nil {
		t.Fatalf("revocation did not clear credentials: %+v %v", stored, err)
	}
}

func checkDelayedRetention(t *testing.T, h *Hub, node string) {
	t.Helper()
	ctx := context.Background()
	var beforeMetrics, beforeIDs int
	if err := h.Store.DB.QueryRow(`SELECT COUNT(*) FROM metrics`).Scan(&beforeMetrics); err != nil {
		t.Fatal(err)
	}
	if err := h.Store.DB.QueryRow(`SELECT COUNT(*) FROM result_ids`).Scan(&beforeIDs); err != nil {
		t.Fatal(err)
	}
	if err := h.Store.Put(ctx, "settings", domain.Doc{"id": "retention.raw_days", "value": 30}); err != nil {
		t.Fatal(err)
	}
	_, delayed := batchFixture(t, h, node, 1)
	delayed[0]["observed_at"] = time.Now().UTC().Add(-10 * 24 * time.Hour).Format(time.RFC3339Nano)
	if err := h.ingestBatch(ctx, node, delayed); err != nil {
		t.Fatal("delayed result within configured window:", err)
	}
	setting, err := h.Store.Get(ctx, "settings", "retention.raw_days")
	if err != nil {
		t.Fatal(err)
	}
	setting["value"] = 1
	if err := h.Store.Put(ctx, "settings", setting); err != nil {
		t.Fatal(err)
	}
	_, expired := batchFixture(t, h, node, 1)
	expired[0]["observed_at"] = time.Now().UTC().Add(-2 * 24 * time.Hour).Format(time.RFC3339Nano)
	if err := h.ingestBatch(ctx, node, expired); err != nil {
		t.Fatal("expired outbox result must be safely acknowledged:", err)
	}
	var metrics, ids int
	if err := h.Store.DB.QueryRow(`SELECT COUNT(*) FROM metrics`).Scan(&metrics); err != nil || metrics != beforeMetrics+1 {
		t.Fatalf("retention metrics=%d expected=%d error=%v", metrics, beforeMetrics+1, err)
	}
	if err := h.Store.DB.QueryRow(`SELECT COUNT(*) FROM result_ids`).Scan(&ids); err != nil || ids != beforeIDs+2 {
		t.Fatalf("expired result receipt lost: ids=%d error=%v", ids, err)
	}
	task, err := h.Store.Get(ctx, "tasks", domain.S(expired[0], "task_id"))
	if err != nil || domain.S(task, "status") != "done" || !domain.B(task, "result_expired") {
		t.Fatalf("expired task not completed: %+v error=%v", task, err)
	}
}

func assertBatchCounts(t *testing.T, h *Hub, metrics, completed int) {
	t.Helper()
	for _, table := range []string{"metrics", "result_ids"} {
		var count int
		if err := h.Store.DB.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil || count != metrics {
			t.Fatalf("unexpected %s count=%d expected=%d error=%v", table, count, metrics, err)
		}
	}
	var done int
	query := `SELECT COUNT(*) FROM resources WHERE kind='tasks' AND json_extract(doc,'$.status')='done'`
	if h.Store.Driver == "pgx" {
		query = `SELECT COUNT(*) FROM resources WHERE kind='tasks' AND doc::jsonb->>'status'='done'`
	}
	if err := h.Store.DB.QueryRow(query).Scan(&done); err != nil || done != completed {
		t.Fatalf("task completion count=%d expected=%d error=%v", done, completed, err)
	}
}

func TestPostgresBatchConcurrentFirstInsert(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, dsn := isolatedBatchPostgres(t)
	h := New(db)
	node := domain.ID()
	if err := db.Put(ctx, "nodes", domain.Doc{"id": node, "status": "online", "tasks_completed": 0}); err != nil {
		t.Fatal(err)
	}
	_, results := batchFixture(t, h, node, 64)
	db2, err := store.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db2.DB.Close() })
	other := New(db2)
	gate := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, hub := range []*Hub{h, other} {
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
	if err != nil || domain.N(n, "tasks_completed") != 64 {
		t.Fatal(fmt.Sprintf("atomic node increment duplicated: %+v error=%v", n, err))
	}
	// A missing historical partition must be created without holding the only
	// pool connection or deadlocking the ingest transaction.
	db.DB.SetMaxOpenConns(1)
	checkDelayedRetention(t, h, node)
	checkFrameTransitions(t, h)
}

func isolatedBatchPostgres(t testing.TB) (*store.Store, string) {
	t.Helper()
	dsn := os.Getenv("NEWNEXUS_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("set NEWNEXUS_TEST_POSTGRES for isolated real PostgreSQL batch concurrency test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close() })
	schema := "newnexus_batch_" + strings.ReplaceAll(domain.ID(), "-", "")
	if _, err := admin.ExecContext(ctx, `CREATE SCHEMA "`+schema+`"`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(`DROP SCHEMA "` + schema + `" CASCADE`); err != nil {
			t.Error(err)
		}
	})
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	db, err := store.Open(u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.DB.Close() })
	return db, u.String()
}

// BenchmarkPostgresBatchIngest256 isolates actual repository ingestion from
// scheduler/gRPC latency. It is not the production-size acceptance workload.
// Run -benchtime=100x for 100 simultaneous node owners and 25,600 observations.
func BenchmarkPostgresBatchIngest256(b *testing.B) {
	b.StopTimer()
	db, dsn := isolatedBatchPostgres(b)
	db2, err := store.Open(dsn)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { db2.DB.Close() })
	hubs := []*Hub{New(db), New(db2)}
	ctx := context.Background()
	type batch struct {
		node    string
		results []domain.Doc
	}
	batches := make([]batch, b.N)
	owners := make([]string, 100)
	for i := range owners {
		owners[i] = domain.ID()
		if err := db.Put(ctx, "nodes", domain.Doc{"id": owners[i], "status": "online", "tasks_completed": 0}); err != nil {
			b.Fatal(err)
		}
	}
	for i := range batches {
		owner := owners[i%len(owners)]
		batches[i].node = owner
		candidates := make([]domain.Doc, 256)
		tasks := make([]domain.Doc, 256)
		for j := range tasks {
			candidate, task := domain.ID(), domain.ID()
			candidates[j] = domain.Doc{"id": candidate, "address": "1.1.1.1", "tier": "active"}
			tasks[j] = domain.Doc{"id": task, "node_id": owner, "candidate_id": candidate, "status": "queued"}
			batches[i].results = append(batches[i].results, domain.Doc{"id": agentproto.ResultID(owner, task), "task_id": task, "observed_at": domain.Now(), "success": true, "sample_count": 3, "success_rate": 1, "tcp_ms": 20, "tls_ms": 15, "ttfb_ms": 50, "download_bps": 1048576})
		}
		if err := db.PutMany(ctx, "candidates", candidates); err != nil {
			b.Fatal(err)
		}
		if err := db.PutMany(ctx, "tasks", tasks); err != nil {
			b.Fatal(err)
		}
	}
	work := make(chan int, b.N)
	for i := range batches {
		work <- i
	}
	close(work)
	errs := make(chan error, b.N)
	var wg sync.WaitGroup
	b.StartTimer()
	started := time.Now()
	for worker := 0; worker < min(100, b.N); worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range work {
				item := batches[index]
				if err := hubs[index%len(hubs)].ingestBatch(ctx, item.node, item.results); err != nil {
					errs <- err
				}
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(started)
	b.StopTimer()
	close(errs)
	for err := range errs {
		if err != nil {
			b.Fatal(err)
		}
	}
	var count int
	if err := db.DB.QueryRow(`SELECT COUNT(*) FROM metrics`).Scan(&count); err != nil || count != b.N*256 {
		b.Fatalf("metric count=%d expected=%d error=%v", count, b.N*256, err)
	}
	b.ReportMetric(256, "results/op")
	b.ReportMetric(float64(count)/elapsed.Seconds(), "results/s")
}
