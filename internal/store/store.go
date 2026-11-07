package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/newnexus/newnexus/internal/domain"
	_ "modernc.org/sqlite"
	"strings"
	"time"
)

var ErrNotFound = errors.New("resource not found")
var ErrConflict = errors.New("resource changed concurrently")

type Store struct {
	DB     *sql.DB
	Driver string
	DSN    string
}

func Open(dsn string) (*Store, error) {
	driver := "sqlite"
	var db *sql.DB
	var e error
	if strings.HasPrefix(dsn, "mysql:") {
		driver = "mysql"
		db, e = openMySQL(dsn)
		if e != nil {
			return nil, e
		}
	}
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		driver = "pgx"
	}
	if db == nil {
		db, e = sql.Open(driver, dsn)
		if e != nil {
			return nil, e
		}
	}
	s := &Store{DB: db, Driver: driver, DSN: dsn}
	if driver == "pgx" || driver == "mysql" {
		db.SetMaxOpenConns(32)
		db.SetMaxIdleConns(8)
		db.SetConnMaxLifetime(30 * time.Minute)
	}
	if driver == "sqlite" {
		db.SetMaxOpenConns(1)
		for _, q := range []string{"PRAGMA journal_mode=WAL", "PRAGMA busy_timeout=10000", "PRAGMA foreign_keys=ON"} {
			if _, e = db.Exec(q); e != nil {
				return nil, e
			}
		}
	}
	if e = s.Migrate(context.Background()); e != nil {
		db.Close()
		return nil, e
	}
	return s, nil
}
func (s *Store) Q(q string) string {
	if s.Driver != "pgx" {
		return q
	}
	n := 0
	return replaceQuestions(q, &n)
}
func replaceQuestions(q string, n *int) string {
	var b strings.Builder
	for _, c := range q {
		if c == '?' {
			*n++
			fmt.Fprintf(&b, "$%d", *n)
		} else {
			b.WriteRune(c)
		}
	}
	return b.String()
}
func (s *Store) Migrate(ctx context.Context) error {
	if s.Driver == "mysql" {
		return s.migrateMySQL(ctx)
	}
	queries := []string{
		`CREATE TABLE IF NOT EXISTS schema_migrations(version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS resources(kind TEXT NOT NULL,id TEXT NOT NULL,doc TEXT NOT NULL,version BIGINT NOT NULL DEFAULT 1,created_at TEXT NOT NULL,updated_at TEXT NOT NULL,PRIMARY KEY(kind,id))`,
		`CREATE INDEX IF NOT EXISTS resources_kind_created ON resources(kind,created_at,id)`,
		`CREATE INDEX IF NOT EXISTS resources_updated ON resources(updated_at,kind)`,
		`CREATE TABLE IF NOT EXISTS result_ids(id TEXT PRIMARY KEY,node_id TEXT NOT NULL,observed_at TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS metrics(id TEXT PRIMARY KEY,node_id TEXT NOT NULL,candidate_id TEXT NOT NULL,observed_at TEXT NOT NULL,success INTEGER NOT NULL,tcp_ms DOUBLE PRECISION NOT NULL,tls_ms DOUBLE PRECISION NOT NULL,ttfb_ms DOUBLE PRECISION NOT NULL,download_bps DOUBLE PRECISION NOT NULL,sample_count INTEGER NOT NULL DEFAULT 1,success_rate DOUBLE PRECISION NOT NULL DEFAULT 0)`,
		`CREATE INDEX IF NOT EXISTS metrics_candidate_time ON metrics(candidate_id,observed_at)`,
		`CREATE INDEX IF NOT EXISTS metrics_time ON metrics(observed_at)`,
		`CREATE TABLE IF NOT EXISTS job_leases(id TEXT PRIMARY KEY,owner TEXT NOT NULL,expires_at TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS idempotency(scope TEXT NOT NULL,key TEXT NOT NULL,request_hash TEXT NOT NULL,response TEXT NOT NULL,created_at TEXT NOT NULL,PRIMARY KEY(scope,key))`,
		`CREATE TABLE IF NOT EXISTS singleton_locks(name TEXT PRIMARY KEY,owner TEXT NOT NULL,expires_at TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS metrics_5m(node_id TEXT NOT NULL,candidate_id TEXT NOT NULL,bucket TEXT NOT NULL,sample_count BIGINT NOT NULL,success_count DOUBLE PRECISION NOT NULL,tcp_sum DOUBLE PRECISION NOT NULL,tls_sum DOUBLE PRECISION NOT NULL,ttfb_sum DOUBLE PRECISION NOT NULL,download_sum DOUBLE PRECISION NOT NULL,PRIMARY KEY(node_id,candidate_id,bucket))`,
		`CREATE TABLE IF NOT EXISTS metrics_1h(node_id TEXT NOT NULL,candidate_id TEXT NOT NULL,bucket TEXT NOT NULL,sample_count BIGINT NOT NULL,success_count DOUBLE PRECISION NOT NULL,tcp_sum DOUBLE PRECISION NOT NULL,tls_sum DOUBLE PRECISION NOT NULL,ttfb_sum DOUBLE PRECISION NOT NULL,download_sum DOUBLE PRECISION NOT NULL,PRIMARY KEY(node_id,candidate_id,bucket))`,
	}
	if s.Driver == "pgx" {
		for i, q := range queries {
			if strings.HasPrefix(q, "CREATE TABLE IF NOT EXISTS metrics(") {
				queries[i] = `CREATE TABLE IF NOT EXISTS metrics(id TEXT NOT NULL,node_id TEXT NOT NULL,candidate_id TEXT NOT NULL,observed_at TIMESTAMPTZ NOT NULL,success INTEGER NOT NULL,tcp_ms DOUBLE PRECISION NOT NULL,tls_ms DOUBLE PRECISION NOT NULL,ttfb_ms DOUBLE PRECISION NOT NULL,download_bps DOUBLE PRECISION NOT NULL,sample_count INTEGER NOT NULL DEFAULT 1,success_rate DOUBLE PRECISION NOT NULL DEFAULT 0,PRIMARY KEY(id,observed_at)) PARTITION BY RANGE(observed_at)`
			}
		}
	}
	tx, e := s.DB.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if s.Driver == "pgx" {
		if _, e = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(63822291)`); e != nil {
			return e
		}
	}
	for _, q := range queries {
		if _, e = tx.ExecContext(ctx, q); e != nil {
			return e
		}
	}
	for _, definition := range []struct{ name, kind, fields string }{{"resources_tasks_node_status", "tasks", s.field("node_id") + "," + s.field("status") + ",created_at"}, {"resources_candidates_schedule", "candidates", s.field("tier") + "," + s.field("last_scheduled_at")}, {"resources_candidates_aggregation", "candidates", s.field("aggregate_pending") + ",updated_at"}, {"resources_sites_aggregation", "sites", s.field("aggregate_pending") + ",updated_at"}} {
		q := "CREATE INDEX IF NOT EXISTS " + definition.name + " ON resources(" + definition.fields + ") WHERE kind='" + definition.kind + "'"
		if _, e = tx.ExecContext(ctx, q); e != nil {
			return e
		}
	}
	if _, e = tx.ExecContext(ctx, s.Q(`INSERT INTO schema_migrations(version,applied_at) VALUES(1,?) ON CONFLICT(version) DO NOTHING`), domain.Now()); e != nil {
		return e
	}
	if e = tx.Commit(); e != nil {
		return e
	}
	return s.EnsureMetricPartitions(ctx, time.Now())
}

// EnsureMetricDay allows delayed reports within an extended retention window
// without precreating years of empty daily partitions at startup.
func (s *Store) EnsureMetricDay(ctx context.Context, at time.Time) error {
	if s.Driver == "mysql" {
		return s.ensureMySQLMetricDay(ctx, at)
	}
	if s.Driver != "pgx" {
		return nil
	}
	day := time.Date(at.UTC().Year(), at.UTC().Month(), at.UTC().Day(), 0, 0, 0, 0, time.UTC)
	name := "metrics_" + day.Format("20060102")
	var exists bool
	if e := s.DB.QueryRowContext(ctx, `SELECT to_regclass($1) IS NOT NULL`, name).Scan(&exists); e != nil {
		return e
	}
	if exists {
		return nil
	}
	tx, e := s.DB.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if _, e = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(63822291)`); e != nil {
		return e
	}
	query := fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s PARTITION OF metrics FOR VALUES FROM ('%s') TO ('%s')", name, day.Format(time.RFC3339), day.AddDate(0, 0, 1).Format(time.RFC3339))
	if _, e = tx.ExecContext(ctx, query); e != nil {
		return e
	}
	return tx.Commit()
}
func (s *Store) DropExpiredMetricPartitions(ctx context.Context, cutoff time.Time) error {
	if s.Driver == "mysql" {
		return s.dropMySQLMetricPartitions(ctx, cutoff)
	}
	if s.Driver != "pgx" {
		return nil
	}
	tx, e := s.DB.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if _, e = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(63822291)`); e != nil {
		return e
	}
	rows, e := tx.QueryContext(ctx, `SELECT n.nspname,c.relname FROM pg_inherits i JOIN pg_class c ON c.oid=i.inhrelid JOIN pg_namespace n ON n.oid=c.relnamespace WHERE i.inhparent='metrics'::regclass`)
	if e != nil {
		return e
	}
	names := []string{}
	for rows.Next() {
		var schema, name string
		if e = rows.Scan(&schema, &name); e != nil {
			rows.Close()
			return e
		}
		if !strings.HasPrefix(name, "metrics_") {
			continue
		}
		day, parseErr := time.Parse("20060102", strings.TrimPrefix(name, "metrics_"))
		if parseErr == nil && !day.AddDate(0, 0, 1).After(cutoff) {
			names = append(names, `"`+strings.ReplaceAll(schema, `"`, `""`)+`"."`+name+`"`)
		}
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	for _, name := range names {
		if _, e = tx.ExecContext(ctx, "DROP TABLE "+name); e != nil {
			return e
		}
	}
	return tx.Commit()
}

func (s *Store) EnsureMetricPartitions(ctx context.Context, now time.Time) error {
	if s.Driver == "mysql" {
		// Historical partitions are prepared before accepting an eligible late
		// report. Recreating already-expired days here makes maintenance create
		// and immediately drop them every pass, requiring unnecessary global MDL.
		for offset := 0; offset <= 2; offset++ {
			if e := s.EnsureMetricDay(ctx, now.AddDate(0, 0, offset)); e != nil {
				return e
			}
		}
		return nil
	}
	if s.Driver != "pgx" {
		return nil
	}
	tx, e := s.DB.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if _, e = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(63822291)`); e != nil {
		return e
	}
	for day := -8; day <= 2; day++ {
		start := time.Date(now.UTC().Year(), now.UTC().Month(), now.UTC().Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, day)
		name := "metrics_" + start.Format("20060102")
		query := fmt.Sprintf("CREATE TABLE IF NOT EXISTS %s PARTITION OF metrics FOR VALUES FROM ('%s') TO ('%s')", name, start.Format(time.RFC3339), start.AddDate(0, 0, 1).Format(time.RFC3339))
		if _, e := tx.ExecContext(ctx, query); e != nil {
			return e
		}
	}
	return tx.Commit()
}
func (s *Store) Get(ctx context.Context, kind, id string) (domain.Doc, error) {
	var raw string
	var ver int64
	e := s.DB.QueryRowContext(ctx, s.Q(`SELECT doc,version FROM resources WHERE kind=? AND id=?`), kind, id).Scan(&raw, &ver)
	if errors.Is(e, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if e != nil {
		return nil, e
	}
	d := domain.Doc{}
	if e = json.Unmarshal([]byte(raw), &d); e != nil {
		return nil, e
	}
	d["_version"] = ver
	return d, nil
}
func (s *Store) Put(ctx context.Context, kind string, d domain.Doc) error {
	return s.put(ctx, s.DB, kind, d)
}

type resourceExecutor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func (s *Store) put(ctx context.Context, executor resourceExecutor, kind string, d domain.Doc) error {
	if domain.S(d, "id") == "" {
		d["id"] = domain.ID()
	}
	now := s.Time(time.Now())
	if domain.S(d, "created_at") == "" {
		d["created_at"] = now
	}
	d["updated_at"] = now
	s.normalizeTimes(d)
	version := int64(domain.N(d, "_version"))
	serial := domain.Doc{}
	for key, value := range d {
		if key != "_version" {
			serial[key] = value
		}
	}
	raw, e := json.Marshal(serial)
	if e != nil {
		return e
	}
	if version == 0 {
		var result sql.Result
		query := `INSERT INTO resources(kind,id,doc,created_at,updated_at) VALUES(?,?,?,?,?)`
		if s.Driver != "mysql" {
			query += ` ON CONFLICT(kind,id) DO NOTHING`
		}
		result, e = executor.ExecContext(ctx, s.Q(query), kind, domain.S(d, "id"), string(raw), domain.S(d, "created_at"), now)
		if s.Driver == "mysql" && duplicateMySQL(e) {
			return ErrConflict
		}
		if e == nil {
			if n, _ := result.RowsAffected(); n == 0 {
				return ErrConflict
			}
		}
		if e == nil {
			d["_version"] = int64(1)
		}
		return e
	}
	res, e := executor.ExecContext(ctx, s.Q(`UPDATE resources SET doc=?,version=version+1,updated_at=? WHERE kind=? AND id=? AND version=?`), string(raw), now, kind, domain.S(d, "id"), version)
	if e != nil {
		return e
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return ErrConflict
	}
	d["_version"] = version + 1
	return nil
}
func (s *Store) Delete(ctx context.Context, kind, id string) error {
	res, e := s.DB.ExecContext(ctx, s.Q(`DELETE FROM resources WHERE kind=? AND id=?`), kind, id)
	if e != nil {
		return e
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
func (s *Store) List(ctx context.Context, kind string) ([]domain.Doc, error) {
	rows, e := s.DB.QueryContext(ctx, s.Q(`SELECT doc,version FROM resources WHERE kind=? ORDER BY created_at DESC,id DESC`), kind)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []domain.Doc{}
	for rows.Next() {
		var raw string
		var v int64
		if e = rows.Scan(&raw, &v); e != nil {
			return nil, e
		}
		d := domain.Doc{}
		if e = json.Unmarshal([]byte(raw), &d); e != nil {
			return nil, e
		}
		d["_version"] = v
		out = append(out, d)
	}
	return out, rows.Err()
}
func (s *Store) Lease(ctx context.Context, id, owner string, duration time.Duration) (bool, error) {
	if s.Driver == "mysql" {
		return s.leaseMySQL(ctx, id, owner, duration)
	}
	now := domain.Now()
	until := time.Now().UTC().Add(duration).Format(time.RFC3339Nano)
	r, e := s.DB.ExecContext(ctx, s.Q(`INSERT INTO job_leases(id,owner,expires_at) VALUES(?,?,?) ON CONFLICT(id) DO UPDATE SET owner=excluded.owner,expires_at=excluded.expires_at WHERE job_leases.expires_at<? OR job_leases.owner=?`), id, owner, until, now, owner)
	if e != nil {
		return false, e
	}
	n, _ := r.RowsAffected()
	return n == 1, nil
}
func (s *Store) ReleaseLease(ctx context.Context, id, owner string) {
	_, _ = s.DB.ExecContext(ctx, s.Q(`DELETE FROM job_leases WHERE id=? AND owner=?`), id, owner)
}
func (s *Store) Count(ctx context.Context, kind string) int {
	var n int
	_ = s.DB.QueryRowContext(ctx, s.Q(`SELECT COUNT(*) FROM resources WHERE kind=?`), kind).Scan(&n)
	return n
}
