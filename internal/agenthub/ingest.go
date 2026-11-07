package agenthub

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/newnexus/newnexus/internal/agentproto"
	"github.com/newnexus/newnexus/internal/domain"
	"github.com/newnexus/newnexus/internal/security"
)

type batchObservation struct {
	id, taskID, candidate          string
	at                             time.Time
	expired, success               bool
	samples                        int
	rate, tcp, tls, ttfb, download float64
	wire                           domain.Doc
}
type metricBucket struct {
	candidate, bucket                 string
	count                             int
	success, tcp, tls, ttfb, download float64
}

// ingestBatch atomically commits the entire results batch, including durable
// idempotency IDs, raw samples, both retention aggregates, task completion and
// node counters. ACK never depends on N+1 historical scoring queries.
func (h *Hub) ingestBatch(ctx context.Context, nodeID string, input []domain.Doc) error {
	var err error
	for attempt := 0; attempt < 4; attempt++ {
		err = h.ingestBatchAttempt(ctx, nodeID, input, false)
		if err == nil || !retryableBatchError(err) || attempt == 3 {
			return err
		}
		// The whole batch transaction is safe to replay. Bound contention retries
		// without reporting a committed result twice or closing healthy streams.
		timer := time.NewTimer(time.Duration(10*(1<<attempt)) * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return err
}

func retryableBatchError(err error) bool {
	var pgError *pgconn.PgError
	var mysqlError *mysql.MySQLError
	return (errors.As(err, &pgError) && (pgError.Code == "40P01" || pgError.Code == "40001")) ||
		(errors.As(err, &mysqlError) && (mysqlError.Number == 1205 || mysqlError.Number == 1213))
}

func (h *Hub) ingestBatchAttempt(ctx context.Context, nodeID string, input []domain.Doc, partitionsReady bool) error {
	if len(input) == 0 {
		return nil
	}
	if len(input) > 256 {
		return errors.New("batch exceeds 256 results")
	}
	byID := map[string]domain.Doc{}
	ids := []string{}
	for _, wire := range input {
		id := domain.S(wire, "id")
		taskID := domain.S(wire, "task_id")
		if id == "" || taskID == "" || len(id) > 512 || len(taskID) > 512 {
			return errors.New("result id and task_id are required and limited to 512 bytes")
		}
		if id != agentproto.ResultID(nodeID, taskID) {
			return errors.New("result id must be SHA-256 of node_id + ':' + task_id")
		}
		if old, ok := byID[id]; ok {
			if domain.S(old, "task_id") != taskID {
				return errors.New("result id is reused for a different task")
			}
			continue
		}
		byID[id] = wire
		ids = append(ids, id)
	}
	sort.Strings(ids)
	tx, err := h.Store.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if h.Store.Driver == "sqlite" {
		// Reserve SQLite's writer before reading the node. Another gateway's
		// lifecycle transaction cannot invalidate a read snapshot mid-batch.
		if _, err = tx.ExecContext(ctx, `UPDATE resources SET version=version WHERE kind='nodes' AND id=?`, nodeID); err != nil {
			return err
		}
	}
	// Serialize receipt lookup and commit with credential revocation. In MySQL's
	// REPEATABLE READ this locking read must precede the first consistent read,
	// so a waiter sees the preceding gateway's committed receipts afterwards.
	var nodeRaw string
	nodeQuery := `SELECT doc FROM resources WHERE kind='nodes' AND id=?`
	if h.Store.Driver == "pgx" || h.Store.Driver == "mysql" {
		nodeQuery += ` FOR UPDATE`
	}
	if err := tx.QueryRowContext(ctx, h.Store.Q(nodeQuery), nodeID).Scan(&nodeRaw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errors.New("result node does not exist")
		}
		return err
	}
	var node domain.Doc
	if err := json.Unmarshal([]byte(nodeRaw), &node); err != nil {
		return err
	}
	if domain.S(node, "status") == "revoked" {
		return errors.New("node credential revoked")
	}
	if err := h.validateFrameTx(ctx, tx, nodeID, node); err != nil {
		return err
	}
	existing, err := existingResults(ctx, tx, h.Store.Q, ids)
	if err != nil {
		return err
	}
	freshIDs := []string{}
	taskSet := map[string]bool{}
	for _, id := range ids {
		if owner, ok := existing[id]; ok {
			if owner != nodeID {
				return errors.New("result id belongs to a different node")
			}
			continue
		}
		freshIDs = append(freshIDs, id)
		taskSet[domain.S(byID[id], "task_id")] = true
	}
	if len(freshIDs) == 0 {
		return tx.Commit()
	}
	taskIDs := sortedKeys(taskSet)
	query, args := inQuery(`SELECT id,doc FROM resources WHERE kind='tasks' AND id IN (`, taskIDs)
	if h.Store.Driver == "mysql" {
		join, pointArgs := mysqlResourcePoints("tasks", taskIDs)
		query, args = `SELECT target.id,target.doc FROM `+join, pointArgs
	}
	rows, err := tx.QueryContext(ctx, h.Store.Q(query), args...)
	if err != nil {
		return err
	}
	tasks := map[string]domain.Doc{}
	for rows.Next() {
		var id, raw string
		if err := rows.Scan(&id, &raw); err != nil {
			rows.Close()
			return err
		}
		var task domain.Doc
		if err := json.Unmarshal([]byte(raw), &task); err != nil {
			rows.Close()
			return err
		}
		tasks[id] = task
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	now := time.Now().UTC()
	rawDays := 7
	var retentionJSON string
	err = tx.QueryRowContext(ctx, `SELECT doc FROM resources WHERE kind='settings' AND id='retention.raw_days'`).Scan(&retentionJSON)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil {
		var retention domain.Doc
		if err := json.Unmarshal([]byte(retentionJSON), &retention); err != nil {
			return err
		}
		rawDays = max(1, min(3650, int(domain.N(retention, "value"))))
	}
	cutoff := now.Add(-time.Duration(rawDays) * 24 * time.Hour)
	oldestPreparedDay := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -8)
	lateDays := map[string]time.Time{}
	observations := map[string]batchObservation{}
	values := make([]string, 0, len(freshIDs))
	params := make([]any, 0, 3*len(freshIDs))
	for _, id := range freshIDs {
		wire := byID[id]
		taskID := domain.S(wire, "task_id")
		task, ok := tasks[taskID]
		if !ok || domain.S(task, "node_id") != nodeID {
			return errors.New("result does not belong to an assigned task")
		}
		at, err := time.Parse(time.RFC3339Nano, domain.S(wire, "observed_at"))
		if err != nil || at.After(now.Add(2*time.Minute)) {
			return errors.New("result timestamp is invalid or outside retention window")
		}
		for _, key := range []string{"tcp_ms", "tls_ms", "ttfb_ms", "download_bps", "p50_ms", "p95_ms", "sample_count", "success_rate", "bytes_read"} {
			n := domain.N(wire, key)
			if math.IsNaN(n) || math.IsInf(n, 0) || n < 0 {
				return fmt.Errorf("invalid non-finite or negative result field %s", key)
			}
		}
		rate := domain.N(wire, "success_rate")
		if wire["success_rate"] == nil && domain.B(wire, "success") {
			rate = 1
		}
		if rate > 1 {
			return errors.New("result success_rate must be between zero and one")
		}
		n := int(domain.N(wire, "sample_count"))
		if n < 1 {
			n = 1
		}
		if n > 40 {
			n = 40
		}
		expired := at.Before(cutoff)
		observations[id] = batchObservation{id: id, taskID: taskID, candidate: domain.S(task, "candidate_id"), at: at.UTC(), expired: expired, success: domain.B(wire, "success"), samples: n, rate: rate, tcp: domain.N(wire, "tcp_ms"), tls: domain.N(wire, "tls_ms"), ttfb: domain.N(wire, "ttfb_ms"), download: domain.N(wire, "download_bps"), wire: wire}
		if !expired && ((h.Store.Driver == "mysql" && !h.knownMySQLMetricDay(at, now)) || (h.Store.Driver == "pgx" && at.Before(oldestPreparedDay))) {
			lateDays[at.UTC().Format("2006-01-02")] = at.UTC()
		}
		values = append(values, "(?,?,?)")
		params = append(params, id, nodeID, h.Store.Time(at))
	}
	if !partitionsReady && len(lateDays) > 0 {
		// Validate ownership before creating any historical partition. Release
		// our transaction first: a pool with one connection must also work, and
		// PostgreSQL DDL must not wait on this transaction's own table locks;
		// MySQL partition DDL implicitly commits, so it must never run inside it.
		if err := tx.Rollback(); err != nil {
			return err
		}
		for _, day := range sortedKeys(lateDays) {
			if err := h.prepareMetricDay(ctx, lateDays[day]); err != nil {
				return fmt.Errorf("prepare delayed result partition: %w", err)
			}
		}
		return h.ingestBatchAttempt(ctx, nodeID, input, true)
	}
	inserted := map[string]bool{}
	if h.Store.Driver == "mysql" {
		// Canonical IDs bind every receipt to this locked node. The lookup and
		// plain INSERT are serialized with other gateways; do not INSERT IGNORE,
		// which could silently discard truncation or other invalid data errors.
		if _, err := tx.ExecContext(ctx, `INSERT INTO result_ids(id,node_id,observed_at) VALUES `+strings.Join(values, ","), params...); err != nil {
			return fmt.Errorf("batch result IDs: %w", err)
		}
		for _, id := range freshIDs {
			inserted[id] = true
		}
	} else {
		rows, err = tx.QueryContext(ctx, h.Store.Q(`INSERT INTO result_ids(id,node_id,observed_at) VALUES `+strings.Join(values, ",")+` ON CONFLICT(id) DO NOTHING RETURNING id`), params...)
		if err != nil {
			return fmt.Errorf("batch result IDs: %w", err)
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			inserted[id] = true
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
	}
	// A concurrent gateway may have won after our initial lookup. Ownership
	// must still be checked before accepting its idempotency result.
	lostRace := []string{}
	for _, id := range freshIDs {
		if !inserted[id] {
			lostRace = append(lostRace, id)
		}
	}
	if len(lostRace) > 0 {
		owners, err := existingResults(ctx, tx, h.Store.Q, lostRace)
		if err != nil {
			return err
		}
		for _, id := range lostRace {
			if owners[id] != nodeID {
				return errors.New("result id belongs to a different node")
			}
		}
	}
	if len(inserted) == 0 {
		return tx.Commit()
	}
	var metricValues []string
	var metricParams []any
	taskPatches := map[string]domain.Doc{}
	latest := map[string]batchObservation{}
	freshCount := 0
	sampleTotal, sampleFailed := 0., 0.
	for _, id := range freshIDs {
		if !inserted[id] {
			continue
		}
		m := observations[id]
		summary := domain.Doc{"tcp_ms": m.tcp, "tls_ms": m.tls, "ttfb_ms": m.ttfb, "download_bps": m.download, "p50_ms": m.wire["p50_ms"], "p95_ms": m.wire["p95_ms"], "sample_count": m.samples, "success_rate": m.rate, "status_code": m.wire["status_code"]}
		patch := domain.Doc{"status": "done", "finished_at": h.Store.Time(now), "observed_at": h.Store.Time(m.at), "success": m.success, "result_error": security.Redact(domain.S(m.wire, "error")), "result_summary": summary}
		if m.expired {
			patch["result_expired"] = true
		}
		taskPatches[m.taskID] = patch
		if m.expired {
			continue
		}
		freshCount++
		sampleTotal += float64(m.samples)
		sampleFailed += float64(m.samples) * (1 - m.rate)
		success := 0
		if m.success {
			success = 1
		}
		metricValues = append(metricValues, "(?,?,?,?,?,?,?,?,?,?,?)")
		metricParams = append(metricParams, m.id, nodeID, m.candidate, h.Store.Time(m.at), success, m.tcp, m.tls, m.ttfb, m.download, m.samples, m.rate)
		if old, ok := latest[m.candidate]; !ok || m.at.After(old.at) {
			latest[m.candidate] = m
		}
	}
	if len(metricValues) > 0 {
		if _, err := tx.ExecContext(ctx, h.Store.Q(`INSERT INTO metrics(id,node_id,candidate_id,observed_at,success,tcp_ms,tls_ms,ttfb_ms,download_bps,sample_count,success_rate) VALUES `+strings.Join(metricValues, ",")), metricParams...); err != nil {
			return fmt.Errorf("batch raw metrics: %w", err)
		}
		for _, aggregation := range []struct {
			table  string
			period time.Duration
		}{{"metrics_5m", 5 * time.Minute}, {"metrics_1h", time.Hour}} {
			groups := map[string]*metricBucket{}
			for _, id := range freshIDs {
				if !inserted[id] {
					continue
				}
				m := observations[id]
				if m.expired {
					continue
				}
				bucket := h.Store.Time(m.at.Truncate(aggregation.period))
				key := m.candidate + "\x00" + bucket
				g := groups[key]
				if g == nil {
					g = &metricBucket{candidate: m.candidate, bucket: bucket}
					groups[key] = g
				}
				n := float64(m.samples)
				g.count += m.samples
				g.success += m.rate * n
				g.tcp += m.tcp * n
				g.tls += m.tls * n
				g.ttfb += m.ttfb * n
				g.download += m.download * n
			}
			var values []string
			var params []any
			for _, key := range sortedKeys(groups) {
				g := groups[key]
				values = append(values, "(?,?,?,?,?,?,?,?,?)")
				params = append(params, nodeID, g.candidate, g.bucket, g.count, g.success, g.tcp, g.tls, g.ttfb, g.download)
			}
			t := aggregation.table
			query := `INSERT INTO ` + t + `(node_id,candidate_id,bucket,sample_count,success_count,tcp_sum,tls_sum,ttfb_sum,download_sum) VALUES ` + strings.Join(values, ",") + ` ON CONFLICT(node_id,candidate_id,bucket) DO UPDATE SET sample_count=` + t + `.sample_count+excluded.sample_count,success_count=` + t + `.success_count+excluded.success_count,tcp_sum=` + t + `.tcp_sum+excluded.tcp_sum,tls_sum=` + t + `.tls_sum+excluded.tls_sum,ttfb_sum=` + t + `.ttfb_sum+excluded.ttfb_sum,download_sum=` + t + `.download_sum+excluded.download_sum`
			if h.Store.Driver == "mysql" {
				query = `INSERT INTO ` + t + `(node_id,candidate_id,bucket,sample_count,success_count,tcp_sum,tls_sum,ttfb_sum,download_sum) VALUES ` + strings.Join(values, ",") + ` AS incoming ON DUPLICATE KEY UPDATE sample_count=` + t + `.sample_count+incoming.sample_count,success_count=` + t + `.success_count+incoming.success_count,tcp_sum=` + t + `.tcp_sum+incoming.tcp_sum,tls_sum=` + t + `.tls_sum+incoming.tls_sum,ttfb_sum=` + t + `.ttfb_sum+incoming.ttfb_sum,download_sum=` + t + `.download_sum+incoming.download_sum`
			}
			if _, err := tx.ExecContext(ctx, h.Store.Q(query), params...); err != nil {
				return fmt.Errorf("batch %s: %w", t, err)
			}
		}
	}
	if err := h.mergeBatch(ctx, tx, "tasks", taskPatches); err != nil {
		return err
	}
	if freshCount > 0 {
		priorTotal := `COALESCE(json_extract(doc,'$.probe_samples_total'),0)`
		priorFailed := `COALESCE(json_extract(doc,'$.probe_samples_failed'),0)`
		update := `json_set(doc,'$.tasks_completed',COALESCE(json_extract(doc,'$.tasks_completed'),0)+?,'$.probe_samples_total',` + priorTotal + `+?,'$.probe_samples_failed',` + priorFailed + `+?,'$.failure_rate',(` + priorFailed + `+?)/(` + priorTotal + `+?),'$.updated_at',?)`
		if h.Store.Driver == "pgx" {
			priorTotal = `COALESCE((doc::jsonb->>'probe_samples_total')::double precision,0)`
			priorFailed = `COALESCE((doc::jsonb->>'probe_samples_failed')::double precision,0)`
			update = `(doc::jsonb || jsonb_build_object('tasks_completed',COALESCE((doc::jsonb->>'tasks_completed')::bigint,0)+?::bigint,'probe_samples_total',` + priorTotal + `+?::double precision,'probe_samples_failed',` + priorFailed + `+?::double precision,'failure_rate',(` + priorFailed + `+?::double precision)/(` + priorTotal + `+?::double precision),'updated_at',?::text))::text`
		} else if h.Store.Driver == "mysql" {
			priorTotal = `COALESCE(CAST(` + mysqlScalar("doc", "probe_samples_total") + ` AS DOUBLE),0)`
			priorFailed = `COALESCE(CAST(` + mysqlScalar("doc", "probe_samples_failed") + ` AS DOUBLE),0)`
			priorTasks := `COALESCE(CAST(` + mysqlScalar("doc", "tasks_completed") + ` AS UNSIGNED),0)`
			update = `JSON_SET(doc,'$.tasks_completed',` + priorTasks + `+?,'$.probe_samples_total',` + priorTotal + `+?,'$.probe_samples_failed',` + priorFailed + `+?,'$.failure_rate',(` + priorFailed + `+?)/(` + priorTotal + `+?),'$.updated_at',?)`
		}
		if _, err := tx.ExecContext(ctx, h.Store.Q(`UPDATE resources SET doc=`+update+`,version=version+1,updated_at=? WHERE kind='nodes' AND id=?`), freshCount, sampleTotal, sampleFailed, sampleFailed, sampleTotal, h.Store.Time(now), h.Store.Time(now), nodeID); err != nil {
			return err
		}
	}
	for _, kind := range []string{"candidates", "sites"} {
		wanted := map[string]batchObservation{}
		for candidate, m := range latest {
			if strings.HasPrefix(candidate, "target:") {
				continue
			}
			if kind == "sites" {
				if !strings.HasPrefix(candidate, "site:") {
					continue
				}
				wanted[strings.TrimPrefix(candidate, "site:")] = m
			} else {
				if strings.HasPrefix(candidate, "site:") {
					continue
				}
				wanted[candidate] = m
			}
		}
		if len(wanted) == 0 {
			continue
		}
		ids := sortedKeys(wanted)
		query, args := inQuery(`SELECT id,doc FROM resources WHERE kind=? AND id IN (`, ids)
		args = append([]any{kind}, args...)
		query += ` ORDER BY id`
		if h.Store.Driver == "mysql" {
			join, pointArgs := mysqlResourcePoints(kind, ids)
			query, args = `SELECT target.id,target.doc FROM `+join+` ORDER BY requested.ordinal FOR UPDATE OF target`, pointArgs
		} else if h.Store.Driver == "pgx" {
			query += ` FOR UPDATE`
		}
		rows, err := tx.QueryContext(ctx, h.Store.Q(query), args...)
		if err != nil {
			return err
		}
		patches := map[string]domain.Doc{}
		for rows.Next() {
			var id, raw string
			if err := rows.Scan(&id, &raw); err != nil {
				rows.Close()
				return err
			}
			var current domain.Doc
			if err := json.Unmarshal([]byte(raw), &current); err != nil {
				rows.Close()
				return err
			}
			m := wanted[id]
			patch := domain.Doc{"aggregate_pending": true, "aggregate_generation": domain.ID()}
			previous, _ := time.Parse(time.RFC3339Nano, domain.S(current, "last_probed_at"))
			if !m.at.Before(previous) {
				patch["last_probed_at"] = h.Store.Time(m.at)
				patch["tcp_ms"] = m.tcp
				patch["tls_ms"] = m.tls
				patch["ttfb_ms"] = m.ttfb
				patch["download_bps"] = m.download
				patch["last_probe_error"] = security.Redact(domain.S(m.wire, "error"))
			}
			patches[id] = patch
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		if err := h.mergeBatch(ctx, tx, kind, patches); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func existingResults(ctx context.Context, tx *sql.Tx, q func(string) string, ids []string) (map[string]string, error) {
	owners := map[string]string{}
	if len(ids) == 0 {
		return owners, nil
	}
	query, args := inQuery(`SELECT id,node_id FROM result_ids WHERE id IN (`, ids)
	rows, err := tx.QueryContext(ctx, q(query), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, node string
		if err := rows.Scan(&id, &node); err != nil {
			return nil, err
		}
		owners[id] = node
	}
	return owners, rows.Err()
}
func inQuery(prefix string, ids []string) (string, []any) {
	marks := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		marks[i] = "?"
		args[i] = id
	}
	return prefix + strings.Join(marks, ",") + ")", args
}
func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
func (h *Hub) mergeBatch(ctx context.Context, tx *sql.Tx, kind string, patches map[string]domain.Doc) error {
	ids := sortedKeys(patches)
	if len(ids) == 0 {
		return nil
	}
	cases := []string{}
	args := []any{}
	now := h.Store.Time(time.Now())
	for _, id := range ids {
		patch := domain.Clone(patches[id])
		patch["updated_at"] = now
		raw, err := json.Marshal(patch)
		if err != nil {
			return err
		}
		merge := `json_patch(doc,?)`
		if h.Store.Driver == "pgx" {
			merge = `(doc::jsonb || ?::jsonb)::text`
		} else if h.Store.Driver == "mysql" {
			merge = `JSON_MERGE_PATCH(target.doc,CAST(? AS JSON))`
		}
		cases = append(cases, "WHEN ? THEN "+merge)
		args = append(args, id, string(raw))
	}
	args = append(args, now, kind)
	marks := []string{}
	for _, id := range ids {
		marks = append(marks, "?")
		args = append(args, id)
	}
	query := `UPDATE resources SET doc=CASE id ` + strings.Join(cases, " ") + ` ELSE doc END,version=version+1,updated_at=? WHERE kind=? AND id IN (` + strings.Join(marks, ",") + ")"
	if h.Store.Driver == "mysql" {
		join, pointArgs := mysqlResourcePoints(kind, ids)
		// Both locking reads and writes use unique primary-key points. Under
		// REPEATABLE READ a large IN update may otherwise lock unrelated rows.
		query = `UPDATE ` + join + ` SET target.doc=CASE target.id ` + strings.Join(cases, " ") + ` ELSE target.doc END,target.version=target.version+1,target.updated_at=?`
		args = append(pointArgs, args[:len(ids)*2]...)
		args = append(args, now)
	}
	if _, err := tx.ExecContext(ctx, h.Store.Q(query), args...); err != nil {
		return fmt.Errorf("batch patch %s: %w", kind, err)
	}
	return nil
}

func mysqlResourcePoints(kind string, ids []string) (string, []any) {
	requested, args := mysqlRequestedResources(ids)
	return requested + ` STRAIGHT_JOIN resources AS target FORCE INDEX(PRIMARY) ON target.kind=? AND target.id=CAST(requested.id AS CHAR CHARACTER SET ascii) COLLATE ascii_bin`, append(args, kind)
}

func mysqlRequestedResources(ids []string) (string, []any) {
	raw, _ := json.Marshal(ids)
	return `JSON_TABLE(?, '$[*]' COLUMNS(ordinal FOR ORDINALITY,id VARCHAR(512) PATH '$')) AS requested`, []any{string(raw)}
}
