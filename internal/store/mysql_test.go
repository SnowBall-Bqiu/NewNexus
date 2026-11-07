package store

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/newnexus/newnexus/internal/domain"
)

func mysqlTestDatabase(t *testing.T) (string, *sql.DB) {
	t.Helper()
	dsn := os.Getenv("NEWNEXUS_TEST_MYSQL")
	if dsn == "" {
		t.Skip("set NEWNEXUS_TEST_MYSQL to an isolated MySQL 8.4 integration instance")
	}
	cfg, e := MySQLConfig(dsn)
	if e != nil {
		t.Fatal(e)
	}
	connector, e := mysql.NewConnector(cfg)
	if e != nil {
		t.Fatal(e)
	}
	admin := sql.OpenDB(connector)
	name := "newnexus_store_" + strings.ReplaceAll(domain.ID(), "-", "")
	if _, e = admin.Exec("CREATE DATABASE `" + name + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin"); e != nil {
		admin.Close()
		t.Fatal(e)
	}
	t.Cleanup(func() {
		_, e := admin.Exec("DROP DATABASE `" + name + "`")
		if e != nil {
			t.Error(e)
		}
		admin.Close()
	})
	u, e := url.Parse(dsn)
	if e != nil || u.Scheme != "mysql" {
		t.Fatal("NEWNEXUS_TEST_MYSQL must use the documented mysql:// URL")
	}
	u.Path = "/" + name
	return u.String(), admin
}
func mysqlTestStore(t *testing.T) *Store {
	t.Helper()
	dsn, _ := mysqlTestDatabase(t)
	s, e := Open(dsn)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.DB.Close() })
	return s
}

func TestMySQLConfigSecurityAndEscapedCredentials(t *testing.T) {
	u := &url.URL{Scheme: "mysql", Host: "db.example:3306", Path: "/newnexus", User: url.UserPassword("operator", "p:@/?#&%")}
	cfg, e := MySQLConfig(u.String())
	if e != nil {
		t.Fatal(e)
	}
	if cfg.Passwd != "p:@/?#&%" || cfg.TLS == nil || cfg.TLS.InsecureSkipVerify || cfg.TLS.ServerName != "db.example" || cfg.ParseTime || cfg.MultiStatements || cfg.ClientFoundRows {
		t.Fatal("unsafe or corrupted URL configuration")
	}
	for _, suffix := range []string{"?tls=skip-verify", "?tls=preferred", "?parseTime=true", "?multiStatements=true", "?clientFoundRows=true", "?tls=false&tls_ca=/secret", "?tls=true&tls=false", "?timeout=-1s", "?sql_mode=''"} {
		if _, e := MySQLConfig(u.String() + suffix); e == nil {
			t.Fatalf("unsafe URL options accepted %s", suffix)
		}
	}
	for _, dsn := range []string{"mysql:operator:pw@tcp(localhost:3306)/db?tls=skip-verify", "mysql:operator:pw@tcp(localhost:3306)/db?parseTime=true", "mysql:operator:pw@tcp(localhost:3306)/db?multiStatements=true", "mysql:operator:pw@tcp(localhost:3306)/db?sql_mode=''"} {
		if _, e := MySQLConfig(dsn); e == nil {
			t.Fatal("unsafe native DSN accepted")
		}
	}
	if cfg, e = MySQLConfig("mysql:operator:pw@tcp(localhost:3306)/db?tls=false"); e != nil || cfg.TLS != nil {
		t.Fatal("explicit native DSN failed", e)
	}
}

func TestMySQLActualTLSVerifiesCAHostnameAndForbidsDowngrade(t *testing.T) {
	dsn := os.Getenv("NEWNEXUS_TEST_MYSQL_TLS")
	if dsn == "" {
		t.Skip("set NEWNEXUS_TEST_MYSQL_TLS to a disposable TLS-required MySQL 8.4 server with a private test CA")
	}
	s, e := Open(dsn)
	if e != nil {
		t.Fatal("verified MySQL TLS connection failed", e)
	}
	defer s.DB.Close()
	var name, cipher string
	if e = s.DB.QueryRow("SHOW SESSION STATUS LIKE 'Ssl_cipher'").Scan(&name, &cipher); e != nil || cipher == "" {
		t.Fatal("MySQL connected without TLS encryption", e)
	}
	u, e := url.Parse(dsn)
	if e != nil {
		t.Fatal(e)
	}
	for _, check := range []string{"wrong-hostname", "untrusted-ca", "plaintext"} {
		t.Run(check, func(t *testing.T) {
			copy := *u
			q := copy.Query()
			switch check {
			case "wrong-hostname":
				q.Set("tls_server_name", "wrong.newnexus.invalid")
			case "untrusted-ca":
				q.Del("tls_ca")
			case "plaintext":
				q.Set("tls", "false")
				q.Del("tls_ca")
				q.Del("tls_server_name")
			}
			copy.RawQuery = q.Encode()
			connection, err := Open(copy.String())
			if err == nil {
				connection.DB.Close()
				t.Fatal("MySQL accepted an unverified or plaintext connection")
			}
		})
	}
}

func TestMySQLMigrationCASLeaseAndReplicaConcurrency(t *testing.T) {
	dsn, _ := mysqlTestDatabase(t)
	ctx := t.Context()
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	stores := make(chan *Store, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, e := Open(dsn)
			if e != nil {
				errs <- e
			} else {
				stores <- s
			}
		}()
	}
	wg.Wait()
	close(errs)
	close(stores)
	for e := range errs {
		t.Error(e)
	}
	all := []*Store{}
	for s := range stores {
		all = append(all, s)
		defer s.DB.Close()
	}
	if len(all) != 8 {
		t.Fatal("concurrent migrations failed")
	}
	exerciseStore(t, all[0])
	d := domain.Doc{"name": "CAS"}
	if e := all[0].Put(ctx, "projects", d); e != nil {
		t.Fatal(e)
	}
	start := make(chan struct{})
	ready := make(chan struct{}, 16)
	wins := make(chan string, 16)
	errs = make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			db := all[i%len(all)]
			copy, e := db.Get(ctx, "projects", domain.S(d, "id"))
			ready <- struct{}{}
			if e != nil {
				errs <- e
				return
			}
			<-start
			copy["name"] = domain.ID()
			e = db.Put(ctx, "projects", copy)
			if e == nil {
				wins <- "won"
			} else if !errors.Is(e, ErrConflict) {
				errs <- e
			}
		}(i)
	}
	// Ensure every contender read the same version before any write.
	for i := 0; i < 16; i++ {
		<-ready
	}
	close(start)
	wg.Wait()
	close(wins)
	close(errs)
	for e := range errs {
		t.Error(e)
	}
	if len(wins) != 1 {
		t.Fatalf("CAS winners %d", len(wins))
	}
	lease := "parallel-" + domain.ID()
	wins = make(chan string, 16)
	errs = make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			owner := domain.ID()
			ok, e := all[i%len(all)].Lease(ctx, lease, owner, time.Minute)
			if e != nil {
				errs <- e
			} else if ok {
				wins <- owner
			}
		}(i)
	}
	wg.Wait()
	close(wins)
	close(errs)
	for e := range errs {
		t.Error(e)
	}
	if len(wins) != 1 {
		t.Fatalf("lease winners %d", len(wins))
	}
	owner := <-wins
	if ok, e := all[1].Lease(ctx, lease, owner, time.Minute); e != nil || !ok {
		t.Fatal("owner renewal failed", e)
	}
	all[0].ReleaseLease(ctx, lease, "wrong")
	if ok, e := all[1].Lease(ctx, lease, "other", time.Minute); e != nil || ok {
		t.Fatal("wrong owner released a lease", e)
	}
	if ok, e := all[0].Lease(ctx, "expired", "first", time.Millisecond); e != nil || !ok {
		t.Fatal(e)
	}
	time.Sleep(5 * time.Millisecond)
	if ok, e := all[1].Lease(ctx, "expired", "second", time.Minute); e != nil || !ok {
		t.Fatal("expired lease was not transferred", e)
	}
}

func TestMySQLBatchAtomicityJSONNullAndGenerationGuards(t *testing.T) {
	s := mysqlTestStore(t)
	ctx := t.Context()
	invalid := domain.Doc{"id": domain.ID(), "payload": make(chan bool)}
	if e := s.Put(ctx, "retryable", invalid); e == nil || domain.N(invalid, "_version") != 0 || s.Count(ctx, "retryable") != 0 {
		t.Fatal("failed insert changed the caller version or stored a partial resource", e)
	}
	delete(invalid, "payload")
	if e := s.Put(ctx, "retryable", invalid); e != nil {
		t.Fatal("fixed resource could not retry its failed insert", e)
	}
	docs := make([]domain.Doc, 501)
	for i := range docs {
		docs[i] = domain.Doc{"id": domain.ID(), "name": "valid"}
	}
	docs[500]["id"] = strings.Repeat("x", 513)
	if e := s.PutMany(ctx, "atomic", docs); e == nil || s.Count(ctx, "atomic") != 0 {
		t.Fatal("invalid last batch did not roll back the entire transaction", e)
	}
	d := domain.Doc{"id": domain.ID(), "name": "old", "nullable": "value", "aggregate_pending": true, "aggregate_generation": "old", "healthy": true, "last_probed_at": s.Time(time.Now())}
	if e := s.Put(ctx, "candidates", d); e != nil {
		t.Fatal(e)
	}
	if e := s.PutMany(ctx, "candidates", []domain.Doc{{"id": d["id"], "name": "must-not-overwrite"}, {"id": domain.ID(), "name": "new"}}); e != nil {
		t.Fatal(e)
	}
	patch := map[string]domain.Doc{domain.S(d, "id"): {"nullable": nil, "aggregate_pending": false, "_aggregate_generation": "stale", "name": "stale"}}
	if e := s.PatchMany(ctx, "candidates", patch); e != nil {
		t.Fatal(e)
	}
	got, e := s.Get(ctx, "candidates", domain.S(d, "id"))
	if e != nil || domain.S(got, "name") != "old" || !domain.B(got, "aggregate_pending") {
		t.Fatal("stale aggregation overwrote current fields", e)
	}
	patch[domain.S(d, "id")]["_aggregate_generation"] = "old"
	patch[domain.S(d, "id")]["name"] = "fresh"
	if e = s.PatchMany(ctx, "candidates", patch); e != nil {
		t.Fatal(e)
	}
	got, _ = s.Get(ctx, "candidates", domain.S(d, "id"))
	if _, ok := got["nullable"]; ok || domain.B(got, "aggregate_pending") || domain.S(got, "name") != "fresh" {
		t.Fatal("JSON merge/null semantics incorrect", got)
	}
	if e = s.FlagAggregations(ctx); e != nil {
		t.Fatal(e)
	}
	pending, e := s.PendingAggregations(ctx, 10)
	if e != nil || len(pending) != 2 {
		t.Fatal("generated boolean index query failed", pending, e)
	}
	at := time.Date(2026, 11, 7, 0, 0, 0, 100000000, time.UTC)
	if e = s.SetSchedule(ctx, "candidates", "last_scheduled_at", []string{domain.S(d, "id")}, at.Format(time.RFC3339Nano)); e != nil {
		t.Fatal(e)
	}
	got, _ = s.Get(ctx, "candidates", domain.S(d, "id"))
	if domain.S(got, "last_scheduled_at") != s.Time(at) {
		t.Fatal("schedule timestamp was not normalized", got)
	}
	// Native JSON must reject malformed input rather than INSERT IGNORE it.
	if _, e = s.DB.ExecContext(ctx, `INSERT INTO resources(kind,id,doc,created_at,updated_at) VALUES('invalid','broken','not json',?,?)`, s.Time(time.Now()), s.Time(time.Now())); e == nil {
		t.Fatal("invalid JSON was silently accepted")
	}
}

func TestMySQLDailyPartitionsLateReportsRetentionAndMidnightFractions(t *testing.T) {
	s := mysqlTestStore(t)
	ctx := t.Context()
	day := time.Now().UTC().Truncate(24 * time.Hour)
	late := day.AddDate(0, 0, -30)
	future := day.AddDate(0, 0, 7)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- s.EnsureMetricDay(ctx, late) }()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	if e := s.EnsureMetricDay(ctx, future); e != nil {
		t.Fatal(e)
	}
	insert := func(at time.Time) string {
		id := domain.ID()
		_, e := s.DB.ExecContext(ctx, `INSERT INTO metrics(id,node_id,candidate_id,observed_at,success,tcp_ms,tls_ms,ttfb_ms,download_bps,sample_count,success_rate) VALUES(?,?,?,?,1,1,1,1,1,1,1)`, id, "node", "candidate", s.Time(at))
		if e != nil {
			t.Fatal(e)
		}
		return id
	}
	oldID := insert(late.Add(100 * time.Millisecond))
	first := insert(day.Add(100 * time.Millisecond))
	second := insert(day.Add(110 * time.Millisecond))
	insert(future.Add(time.Second))
	var count int
	partition := "metrics_" + day.Format("20060102")
	if e := s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM metrics PARTITION (`"+partition+"`)").Scan(&count); e != nil || count != 2 {
		t.Fatal("midnight fractions entered the previous UTC partition", count, e)
	}
	rows, e := s.DB.QueryContext(ctx, `SELECT id FROM metrics WHERE observed_at>=? AND observed_at<? ORDER BY observed_at`, s.Time(day), s.Time(day.Add(time.Second)))
	if e != nil {
		t.Fatal(e)
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if e = rows.Scan(&id); e != nil {
			t.Fatal(e)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if len(ids) != 2 || ids[0] != first || ids[1] != second {
		t.Fatal("fractional timestamp lexical ordering failed", ids)
	}
	if e = s.DropExpiredMetricPartitions(ctx, day.AddDate(0, 0, -7)); e != nil {
		t.Fatal(e)
	}
	if e = s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM metrics WHERE id=?`, oldID).Scan(&count); e != nil || count != 0 {
		t.Fatal("expired partition still retained samples", count, e)
	}
	if e = s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM metrics PARTITION (`+partition+`)`).Scan(&count); e != nil || count != 2 {
		t.Fatal("cleanup deleted the current day's fractional samples", count, e)
	}
	// Extending retention later must recreate a dropped day without moving or
	// deleting existing current/future reports.
	if e = s.EnsureMetricDay(ctx, late); e != nil {
		t.Fatal(e)
	}
	insert(late.Add(200 * time.Millisecond))
	if e = s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM metrics`).Scan(&count); e != nil || count != 4 {
		t.Fatal("late-day reorganization lost reports", count, e)
	}
}

func TestMySQLDueTierPriorityAndPendingTaskScope(t *testing.T) {
	s := mysqlTestStore(t)
	ctx := t.Context()
	now := time.Now().UTC()
	docs := []domain.Doc{
		{"id": "active-never", "tier": "active"},
		{"id": "active-empty", "tier": "active", "last_scheduled_at": ""},
		{"id": "active-due", "tier": "active", "last_scheduled_at": s.Time(now.Add(-31 * time.Second))},
		{"id": "active-fresh", "tier": "active", "last_scheduled_at": s.Time(now.Add(-29 * time.Second))},
		{"id": "ranked-due", "tier": "ranked", "last_scheduled_at": s.Time(now.Add(-301 * time.Second))},
		{"id": "ranked-fresh", "tier": "ranked", "last_scheduled_at": s.Time(now.Add(-299 * time.Second))},
		{"id": "inventory-due", "tier": "inventory", "last_scheduled_at": s.Time(now.Add(-7 * time.Hour))},
	}
	if e := s.PutMany(ctx, "candidates", docs); e != nil {
		t.Fatal(e)
	}
	rows, e := s.DueCandidates(ctx, now, 6*time.Hour, 5*time.Minute, 30*time.Second, 4)
	if e != nil {
		t.Fatal(e)
	}
	if len(rows) != 4 || domain.S(rows[0], "id") != "active-empty" || domain.S(rows[1], "id") != "active-never" || domain.S(rows[2], "id") != "active-due" || domain.S(rows[3], "id") != "ranked-due" {
		t.Fatal("indexed queries changed tier priority, freshness or limit", rows)
	}
	rows, e = s.DueCandidates(ctx, now, 6*time.Hour, 5*time.Minute, 30*time.Second, 10)
	if e != nil || len(rows) != 5 || domain.S(rows[4], "id") != "inventory-due" {
		t.Fatal("due inventory was omitted", rows, e)
	}
	tasks := []domain.Doc{{"id": "queued", "node_id": "node", "candidate_id": "one", "status": "queued"}, {"id": "accepted", "node_id": "node", "candidate_id": "two", "status": "accepted"}, {"id": "done", "node_id": "node", "candidate_id": "three", "status": "done"}}
	if e = s.PutMany(ctx, "tasks", tasks); e != nil {
		t.Fatal(e)
	}
	keys, e := s.PendingTaskKeys(ctx)
	if e != nil || len(keys) != 2 || !keys["node:one"] || !keys["node:two"] || keys["node:three"] {
		t.Fatal("pending status index included completed tasks", keys, e)
	}
}

func TestMySQLV1UpgradeAndIndexedRetentionPredicates(t *testing.T) {
	s := mysqlTestStore(t)
	ctx := t.Context()
	now := time.Now().UTC()
	old := s.Time(now.Add(-8 * 24 * time.Hour))
	fresh := s.Time(now)
	tasks := []domain.Doc{{"id": "expired", "status": "done", "finished_at": old}, {"id": "fresh", "status": "done", "finished_at": fresh}, {"id": "active", "status": "accepted", "finished_at": old}, {"id": "unknown", "status": "done", "finished_at": nil}}
	if e := s.PutMany(ctx, "tasks", tasks); e != nil {
		t.Fatal(e)
	}
	if e := s.PutMany(ctx, "sessions", []domain.Doc{{"id": "expired", "expires_at": old}, {"id": "fresh", "expires_at": s.Time(now.Add(time.Hour))}}); e != nil {
		t.Fatal(e)
	}
	query := `ALTER TABLE resources DROP INDEX resources_tasks_status,DROP INDEX resources_tasks_retention,DROP INDEX resources_sessions_retention,DROP INDEX resources_candidates_stale,DROP INDEX resources_tasks_pending_keys,DROP COLUMN candidate_id_idx,DROP COLUMN healthy_idx,DROP COLUMN last_probed_idx,DROP COLUMN finished_at_idx,DROP COLUMN expires_at_idx,MODIFY COLUMN last_scheduled_idx VARCHAR(35) CHARACTER SET ascii COLLATE ascii_bin GENERATED ALWAYS AS (NULLIF(JSON_UNQUOTE(JSON_EXTRACT(doc,'$.last_scheduled_at')),'null')) STORED`
	if _, e := s.DB.ExecContext(ctx, query); e != nil {
		t.Fatal(e)
	}
	if _, e := s.DB.ExecContext(ctx, `DELETE FROM schema_migrations WHERE version=2`); e != nil {
		t.Fatal(e)
	}
	for _, table := range []string{"result_ids", "metrics_5m", "metrics_1h"} {
		if _, e := s.DB.ExecContext(ctx, "ALTER TABLE "+table+" DROP INDEX "+table+"_time"); e != nil {
			t.Fatal(e)
		}
	}
	if _, e := s.DB.ExecContext(ctx, `DELETE FROM schema_migrations WHERE version=3`); e != nil {
		t.Fatal(e)
	}
	if _, e := s.DB.ExecContext(ctx, `DELETE FROM schema_migrations WHERE version=4`); e != nil {
		t.Fatal(e)
	}
	if _, e := s.DB.ExecContext(ctx, `DELETE FROM schema_migrations WHERE version=5`); e != nil {
		t.Fatal(e)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- s.Migrate(ctx) }()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal("concurrent existing-v1 upgrade failed", e)
		}
	}
	var count int
	if e := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version=2`).Scan(&count); e != nil || count != 1 {
		t.Fatal("upgrade was not durably recorded", count, e)
	}
	if e := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version=3`).Scan(&count); e != nil || count != 1 {
		t.Fatal("time-index upgrade was not recorded", count, e)
	}
	if e := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version=4`).Scan(&count); e != nil || count != 1 {
		t.Fatal("aggregation-index upgrade was not recorded", count, e)
	}
	if e := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version=5`).Scan(&count); e != nil || count != 1 {
		t.Fatal("pending-key covering-index upgrade was not recorded", count, e)
	}
	cutoff := s.Time(now.Add(-7 * 24 * time.Hour))
	if _, e := s.DB.ExecContext(ctx, `DELETE FROM resources WHERE kind='tasks' AND status_idx='done' AND finished_at_idx<?`, cutoff); e != nil {
		t.Fatal(e)
	}
	if _, e := s.DB.ExecContext(ctx, `DELETE FROM resources WHERE kind='sessions' AND expires_at_idx<?`, fresh); e != nil {
		t.Fatal(e)
	}
	if s.Count(ctx, "tasks") != 3 || s.Count(ctx, "sessions") != 1 {
		t.Fatal("retention deleted active, fresh, missing-date resources or retained expired rows")
	}
	for _, id := range []string{"active", "fresh", "unknown"} {
		if _, e := s.Get(ctx, "tasks", id); e != nil {
			t.Fatal("retention removed protected task", id, e)
		}
	}
}

func TestMySQLCleanupUsesTimeRangesAndDoesNotRecreateExpiredPartitions(t *testing.T) {
	s := mysqlTestStore(t)
	ctx := t.Context()
	now := time.Now().UTC()
	old := now.Add(-40 * 24 * time.Hour)
	cutoff := s.Time(now.Add(-7 * 24 * time.Hour))
	for _, table := range []string{"result_ids", "metrics_5m", "metrics_1h"} {
		marks := []string{}
		args := []any{}
		for i := 0; i < 101; i++ {
			at := now
			if i == 100 {
				at = old
			}
			if table == "result_ids" {
				marks = append(marks, "(?,?,?)")
				args = append(args, domain.ID(), "node", s.Time(at))
			} else {
				marks = append(marks, "(?,?,?,1,1,1,1,1,1)")
				args = append(args, "node", fmt.Sprint("candidate-", i), s.Time(at))
			}
		}
		columns := "(id,node_id,observed_at)"
		column := "observed_at"
		if table != "result_ids" {
			columns = "(node_id,candidate_id,bucket,sample_count,success_count,tcp_sum,tls_sum,ttfb_sum,download_sum)"
			column = "bucket"
		}
		if _, e := s.DB.ExecContext(ctx, "INSERT INTO "+table+columns+" VALUES "+strings.Join(marks, ","), args...); e != nil {
			t.Fatal(e)
		}
		rows, e := s.DB.QueryContext(ctx, "EXPLAIN DELETE FROM "+table+" WHERE "+column+"<?", cutoff)
		if e != nil {
			t.Fatal(e)
		}
		fields, e := rows.Columns()
		if e != nil {
			rows.Close()
			t.Fatal(e)
		}
		if !rows.Next() {
			rows.Close()
			t.Fatal("EXPLAIN returned no plan")
		}
		values := make([]sql.NullString, len(fields))
		scan := make([]any, len(fields))
		for i := range scan {
			scan[i] = &values[i]
		}
		if e = rows.Scan(scan...); e != nil {
			rows.Close()
			t.Fatal(e)
		}
		rows.Close()
		access, key := "", ""
		for i, field := range fields {
			if field == "type" {
				access = values[i].String
			}
			if field == "key" {
				key = values[i].String
			}
		}
		if access != "range" || key != table+"_time" {
			t.Fatalf("%s cleanup does not use its time range index: type=%s key=%s", table, access, key)
		}
		t.Logf("%s cleanup: type=%s key=%s", table, access, key)
	}
	if e := s.EnsureMetricDay(ctx, old); e != nil {
		t.Fatal(e)
	}
	if e := s.DropExpiredMetricPartitions(ctx, now.Add(-7*24*time.Hour)); e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 3; i++ {
		if e := s.EnsureMetricPartitions(ctx, now); e != nil {
			t.Fatal(e)
		}
		if e := s.DropExpiredMetricPartitions(ctx, now.Add(-7*24*time.Hour)); e != nil {
			t.Fatal(e)
		}
	}
	var count int
	if e := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.PARTITIONS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='metrics'`).Scan(&count); e != nil || count != 4 {
		t.Fatal("maintenance recreated already expired historical partitions", count, e)
	}
}
