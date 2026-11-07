package store

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
)

// MySQLConfig parses the application URL or explicitly prefixed native DSN.
// Config is returned rather than a DSN so private CA settings are not lost in
// FormatDSN. Callers must not log its password or TLS material.
func MySQLConfig(dsn string) (*mysql.Config, error) {
	if !strings.HasPrefix(dsn, "mysql:") {
		return nil, errors.New("MySQL connection must begin mysql:// or mysql:")
	}
	var cfg *mysql.Config
	var e error
	ca, serverName, tlsMode := "", "", "true"
	if strings.HasPrefix(dsn, "mysql://") {
		u, err := url.Parse(dsn)
		if err != nil || u.Scheme != "mysql" || u.Hostname() == "" || u.User == nil || u.Fragment != "" || u.Opaque != "" {
			return nil, errors.New("invalid MySQL URL")
		}
		cfg = mysql.NewConfig()
		cfg.User = u.User.Username()
		cfg.Passwd, _ = u.User.Password()
		cfg.Net = "tcp"
		port := u.Port()
		if port == "" {
			port = "3306"
		}
		if number, err := strconv.Atoi(port); err != nil || number < 1 || number > 65535 {
			return nil, errors.New("invalid MySQL port")
		}
		cfg.Addr = net.JoinHostPort(u.Hostname(), port)
		cfg.DBName = strings.TrimPrefix(u.Path, "/")
		if cfg.DBName == "" || strings.ContainsAny(cfg.DBName, "/\\\x00") {
			return nil, errors.New("MySQL URL must select one database")
		}
		values, err := url.ParseQuery(u.RawQuery)
		if err != nil {
			return nil, errors.New("invalid MySQL URL parameters")
		}
		for key, values := range values {
			if len(values) != 1 {
				return nil, errors.New("duplicate MySQL connection parameter")
			}
			value := values[0]
			switch key {
			case "tls":
				tlsMode = value
			case "tls_ca":
				ca = value
			case "tls_server_name":
				serverName = value
			case "timeout", "readTimeout", "writeTimeout":
				duration, err := time.ParseDuration(value)
				if err != nil || duration <= 0 || duration > 5*time.Minute {
					return nil, fmt.Errorf("invalid MySQL %s", key)
				}
				if key == "timeout" {
					cfg.Timeout = duration
				} else if key == "readTimeout" {
					cfg.ReadTimeout = duration
				} else {
					cfg.WriteTimeout = duration
				}
			default:
				return nil, fmt.Errorf("unsupported MySQL connection parameter %q", key)
			}
		}
	} else {
		cfg, e = mysql.ParseDSN(strings.TrimPrefix(dsn, "mysql:"))
		if e != nil {
			return nil, errors.New("invalid MySQL native DSN")
		}
		if cfg.DBName == "" {
			return nil, errors.New("MySQL DSN must select a database")
		}
		if cfg.TLSConfig != "" {
			tlsMode = cfg.TLSConfig
		}
		if cfg.MultiStatements || cfg.InterpolateParams || cfg.ClientFoundRows || cfg.ParseTime || cfg.AllowAllFiles || cfg.AllowCleartextPasswords || cfg.AllowOldPasswords || cfg.AllowFallbackToPlaintext {
			return nil, errors.New("unsafe MySQL driver options are not allowed")
		}
		for key := range cfg.Params {
			return nil, fmt.Errorf("unsupported MySQL session parameter %q", key)
		}
		if cfg.Net != "tcp" && cfg.Net != "tcp4" && cfg.Net != "tcp6" && cfg.Net != "unix" {
			return nil, errors.New("unsupported MySQL network")
		}
	}
	if cfg.User == "" {
		return nil, errors.New("MySQL username is required")
	}
	if tlsMode != "true" && tlsMode != "false" {
		return nil, errors.New("MySQL tls must be true or false; verification bypass and plaintext fallback are forbidden")
	}
	if tlsMode == "false" && (ca != "" || serverName != "") {
		return nil, errors.New("MySQL CA and server name require TLS")
	}
	cfg.TLSConfig = ""
	cfg.TLS = nil
	if tlsMode == "true" {
		if serverName == "" && cfg.Net != "unix" {
			serverName, _, e = net.SplitHostPort(cfg.Addr)
			if e != nil {
				return nil, errors.New("invalid MySQL TCP address")
			}
		}
		if serverName == "" {
			return nil, errors.New("MySQL TLS requires a server name")
		}
		roots, err := x509.SystemCertPool()
		if err != nil {
			roots = x509.NewCertPool()
		}
		if ca != "" {
			pem, err := os.ReadFile(ca)
			if err != nil {
				return nil, fmt.Errorf("read MySQL CA: %w", err)
			}
			if !roots.AppendCertsFromPEM(pem) {
				return nil, errors.New("MySQL CA file contains no certificates")
			}
		}
		cfg.TLS = &tls.Config{RootCAs: roots, ServerName: serverName, MinVersion: tls.VersionTLS12}
	}
	cfg.Loc = time.UTC
	cfg.ParseTime = false
	cfg.RejectReadOnly = true
	if cfg.Timeout == 0 {
		cfg.Timeout = 10 * time.Second
	}
	if cfg.ReadTimeout == 0 {
		cfg.ReadTimeout = 90 * time.Second
	}
	if cfg.WriteTimeout == 0 {
		cfg.WriteTimeout = 90 * time.Second
	}
	cfg.Params = map[string]string{"time_zone": "'+00:00'", "sql_mode": "'STRICT_ALL_TABLES,ERROR_FOR_DIVISION_BY_ZERO,NO_ENGINE_SUBSTITUTION'"}
	if e = cfg.Apply(mysql.Charset("utf8mb4", "utf8mb4_bin")); e != nil {
		return nil, e
	}
	return cfg, nil
}

func openMySQL(dsn string) (*sql.DB, error) {
	cfg, e := MySQLConfig(dsn)
	if e != nil {
		return nil, e
	}
	connector, e := mysql.NewConnector(cfg)
	if e != nil {
		return nil, e
	}
	return sql.OpenDB(connector), nil
}
func duplicateMySQL(err error) bool {
	var failure *mysql.MySQLError
	return errors.As(err, &failure) && failure.Number == 1062
}

// A pinned connection is essential: GET_LOCK survives implicit DDL commits but
// belongs to the connection, not to a database/sql transaction or pool.
func (s *Store) mysqlDDL(ctx context.Context, fn func(*sql.Conn) error) error {
	conn, e := s.DB.Conn(ctx)
	if e != nil {
		return e
	}
	defer conn.Close()
	var database string
	if e = conn.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&database); e != nil {
		return e
	}
	hash := sha256.Sum256([]byte(database))
	name := "newnexus-ddl-" + hex.EncodeToString(hash[:24])
	var locked sql.NullInt64
	if e = conn.QueryRowContext(ctx, "SELECT GET_LOCK(?,30)", name).Scan(&locked); e != nil {
		_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		return e
	}
	if !locked.Valid || locked.Int64 != 1 {
		return errors.New("timed out acquiring MySQL migration lock")
	}
	defer func() {
		release, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var released sql.NullInt64
		if err := conn.QueryRowContext(release, "SELECT RELEASE_LOCK(?)", name).Scan(&released); err != nil || !released.Valid || released.Int64 != 1 {
			// Close the physical session rather than returning a potentially
			// reentrant GET_LOCK owner to the connection pool.
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
	}()
	return fn(conn)
}

func mysqlPartitionDefinition(now time.Time) string {
	day := time.Date(now.UTC().Year(), now.UTC().Month(), now.UTC().Day(), 0, 0, 0, 0, time.UTC)
	parts := []string{}
	for offset := 0; offset < 3; offset++ {
		at := day.AddDate(0, 0, offset)
		parts = append(parts, "PARTITION metrics_"+at.Format("20060102")+" VALUES LESS THAN ('"+mysqlTime(at.AddDate(0, 0, 1))+"')")
	}
	parts = append(parts, "PARTITION metrics_future VALUES LESS THAN (MAXVALUE)")
	return " PARTITION BY RANGE COLUMNS(observed_at) (" + strings.Join(parts, ",") + ")"
}

func (s *Store) migrateMySQL(ctx context.Context) error {
	e := s.mysqlDDL(ctx, func(conn *sql.Conn) error {
		var version string
		if e := conn.QueryRowContext(ctx, "SELECT VERSION()").Scan(&version); e != nil {
			return e
		}
		if strings.Contains(strings.ToLower(version), "mariadb") {
			return errors.New("MySQL 8.4 or later is required; MariaDB has different JSON and partition semantics")
		}
		var major, minor int
		if _, e := fmt.Sscanf(version, "%d.%d", &major, &minor); e != nil || major < 8 || major == 8 && minor < 4 {
			return errors.New("MySQL 8.4 or later is required")
		}
		scalar := func(name string) string { return "NULLIF(JSON_UNQUOTE(JSON_EXTRACT(doc,'$." + name + "')),'null')" }
		stamp := func(name string) string { return "NULLIF(" + scalar(name) + ",'')" }
		queries := []string{
			`CREATE TABLE IF NOT EXISTS schema_migrations(version INTEGER PRIMARY KEY,applied_at VARCHAR(35) CHARACTER SET ascii COLLATE ascii_bin NOT NULL) ENGINE=InnoDB`,
			`CREATE TABLE IF NOT EXISTS resources(kind VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,id VARCHAR(512) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,doc JSON NOT NULL,version BIGINT NOT NULL DEFAULT 1,created_at VARCHAR(35) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,updated_at VARCHAR(35) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,` +
				`node_id_idx VARCHAR(512) CHARACTER SET ascii COLLATE ascii_bin GENERATED ALWAYS AS (` + scalar("node_id") + `) STORED,` +
				`candidate_id_idx VARCHAR(512) CHARACTER SET ascii COLLATE ascii_bin GENERATED ALWAYS AS (` + scalar("candidate_id") + `) STORED,` +
				`status_idx VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin GENERATED ALWAYS AS (` + scalar("status") + `) STORED,` +
				`tier_idx VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin GENERATED ALWAYS AS (` + scalar("tier") + `) STORED,` +
				`last_scheduled_idx VARCHAR(35) CHARACTER SET ascii COLLATE ascii_bin GENERATED ALWAYS AS (` + stamp("last_scheduled_at") + `) STORED,` +
				`finished_at_idx VARCHAR(35) CHARACTER SET ascii COLLATE ascii_bin GENERATED ALWAYS AS (` + stamp("finished_at") + `) STORED,` +
				`expires_at_idx VARCHAR(35) CHARACTER SET ascii COLLATE ascii_bin GENERATED ALWAYS AS (` + stamp("expires_at") + `) STORED,` +
				`aggregate_pending_idx TINYINT GENERATED ALWAYS AS (CASE WHEN JSON_TYPE(JSON_EXTRACT(doc,'$.aggregate_pending'))='BOOLEAN' THEN JSON_UNQUOTE(JSON_EXTRACT(doc,'$.aggregate_pending'))='true' ELSE NULL END) STORED,` +
				`healthy_idx TINYINT GENERATED ALWAYS AS (CASE WHEN JSON_TYPE(JSON_EXTRACT(doc,'$.healthy'))='BOOLEAN' THEN JSON_UNQUOTE(JSON_EXTRACT(doc,'$.healthy'))='true' ELSE NULL END) STORED,` +
				`last_probed_idx VARCHAR(35) CHARACTER SET ascii COLLATE ascii_bin GENERATED ALWAYS AS (` + scalar("last_probed_at") + `) STORED,` +
				`PRIMARY KEY(kind,id),INDEX resources_kind_created(kind,created_at,id),INDEX resources_updated(updated_at,kind),INDEX resources_tasks_node_status(kind,node_id_idx,status_idx,created_at),INDEX resources_tasks_status(kind,status_idx,created_at),INDEX resources_tasks_pending_keys(kind,status_idx,node_id_idx,candidate_id_idx),INDEX resources_tasks_retention(kind,status_idx,finished_at_idx),INDEX resources_sessions_retention(kind,expires_at_idx),INDEX resources_candidates_schedule(kind,tier_idx,last_scheduled_idx),INDEX resources_aggregation(kind,aggregate_pending_idx,updated_at),INDEX resources_candidates_stale(kind,healthy_idx,last_probed_idx,updated_at,aggregate_pending_idx)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_bin`,
			`CREATE TABLE IF NOT EXISTS result_ids(id VARCHAR(512) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,node_id VARCHAR(512) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,observed_at VARCHAR(35) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,INDEX result_ids_time(observed_at)) ENGINE=InnoDB`,
			`CREATE TABLE IF NOT EXISTS metrics(id VARCHAR(512) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,node_id VARCHAR(512) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,candidate_id VARCHAR(512) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,observed_at VARCHAR(35) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,success INTEGER NOT NULL,tcp_ms DOUBLE NOT NULL,tls_ms DOUBLE NOT NULL,ttfb_ms DOUBLE NOT NULL,download_bps DOUBLE NOT NULL,sample_count INTEGER NOT NULL DEFAULT 1,success_rate DOUBLE NOT NULL DEFAULT 0,PRIMARY KEY(id,observed_at),INDEX metrics_candidate_time(candidate_id,observed_at),INDEX metrics_time(observed_at)) ENGINE=InnoDB` + mysqlPartitionDefinition(time.Now()),
			`CREATE TABLE IF NOT EXISTS job_leases(id VARCHAR(512) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,owner VARCHAR(512) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,expires_at VARCHAR(35) CHARACTER SET ascii COLLATE ascii_bin NOT NULL) ENGINE=InnoDB`,
			"CREATE TABLE IF NOT EXISTS idempotency(scope VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,`key` VARCHAR(200) CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin NOT NULL,request_hash VARCHAR(128) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,response LONGTEXT NOT NULL,created_at VARCHAR(35) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,PRIMARY KEY(scope,`key`)) ENGINE=InnoDB",
			`CREATE TABLE IF NOT EXISTS singleton_locks(name VARCHAR(512) CHARACTER SET ascii COLLATE ascii_bin PRIMARY KEY,owner VARCHAR(512) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,expires_at VARCHAR(35) CHARACTER SET ascii COLLATE ascii_bin NOT NULL) ENGINE=InnoDB`,
		}
		for _, table := range []string{"metrics_5m", "metrics_1h"} {
			queries = append(queries, `CREATE TABLE IF NOT EXISTS `+table+`(node_id VARCHAR(512) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,candidate_id VARCHAR(512) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,bucket VARCHAR(35) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,sample_count BIGINT NOT NULL,success_count DOUBLE NOT NULL,tcp_sum DOUBLE NOT NULL,tls_sum DOUBLE NOT NULL,ttfb_sum DOUBLE NOT NULL,download_sum DOUBLE NOT NULL,PRIMARY KEY(node_id,candidate_id,bucket),INDEX `+table+`_time(bucket)) ENGINE=InnoDB`)
		}
		for _, q := range queries {
			if _, e := conn.ExecContext(ctx, q); e != nil {
				return fmt.Errorf("MySQL migration: %w", e)
			}
		}
		// CREATE TABLE IF NOT EXISTS does not add indexes to an existing
		// installation. The connection-scoped migration lock makes the check
		// and ALTER safe when multiple replicas upgrade together.
		alter := []string{}
		for _, field := range []struct{ column, key string }{{"finished_at_idx", "finished_at"}, {"expires_at_idx", "expires_at"}} {
			var count int
			if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='resources' AND COLUMN_NAME=?`, field.column).Scan(&count); err != nil {
				return err
			}
			if count == 0 {
				alter = append(alter, "ADD COLUMN "+field.column+" VARCHAR(35) CHARACTER SET ascii COLLATE ascii_bin GENERATED ALWAYS AS ("+stamp(field.key)+") STORED")
			}
		}
		var scheduleExpression string
		if err := conn.QueryRowContext(ctx, `SELECT GENERATION_EXPRESSION FROM information_schema.COLUMNS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='resources' AND COLUMN_NAME='last_scheduled_idx'`).Scan(&scheduleExpression); err != nil {
			return err
		}
		if strings.Count(strings.ToLower(scheduleExpression), "nullif") < 2 {
			alter = append(alter, "MODIFY COLUMN last_scheduled_idx VARCHAR(35) CHARACTER SET ascii COLLATE ascii_bin GENERATED ALWAYS AS ("+stamp("last_scheduled_at")+") STORED")
		}
		for _, index := range []struct{ name, columns string }{{"resources_tasks_status", "kind,status_idx,created_at"}, {"resources_tasks_retention", "kind,status_idx,finished_at_idx"}, {"resources_sessions_retention", "kind,expires_at_idx"}} {
			var count int
			if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.STATISTICS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='resources' AND INDEX_NAME=?`, index.name).Scan(&count); err != nil {
				return err
			}
			if count == 0 {
				alter = append(alter, "ADD INDEX "+index.name+"("+index.columns+")")
			}
		}
		if len(alter) > 0 {
			if _, err := conn.ExecContext(ctx, "ALTER TABLE resources "+strings.Join(alter, ",")); err != nil {
				return err
			}
		}
		if _, err := conn.ExecContext(ctx, "INSERT INTO schema_migrations(version,applied_at) VALUES(2,?) ON DUPLICATE KEY UPDATE version=version", s.Time(time.Now())); err != nil {
			return err
		}
		for _, index := range []struct{ table, column string }{{"result_ids", "observed_at"}, {"metrics_5m", "bucket"}, {"metrics_1h", "bucket"}} {
			var count int
			if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.STATISTICS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME=? AND INDEX_NAME=?`, index.table, index.table+"_time").Scan(&count); err != nil {
				return err
			}
			if count == 0 {
				if _, err := conn.ExecContext(ctx, "ALTER TABLE "+index.table+" ADD INDEX "+index.table+"_time("+index.column+")"); err != nil {
					return err
				}
			}
		}
		if _, err := conn.ExecContext(ctx, "INSERT INTO schema_migrations(version,applied_at) VALUES(3,?) ON DUPLICATE KEY UPDATE version=version", s.Time(time.Now())); err != nil {
			return err
		}
		// Pending results and stale health checks are separate indexed paths.
		// The JSON predicates otherwise force a full inventory scan on every
		// aggregation pass, including inventories with no new observations.
		alter = nil
		for _, column := range []struct{ name, definition string }{
			{"healthy_idx", "TINYINT GENERATED ALWAYS AS (CASE WHEN JSON_TYPE(JSON_EXTRACT(doc,'$.healthy'))='BOOLEAN' THEN JSON_UNQUOTE(JSON_EXTRACT(doc,'$.healthy'))='true' ELSE NULL END) STORED"},
			{"last_probed_idx", "VARCHAR(35) CHARACTER SET ascii COLLATE ascii_bin GENERATED ALWAYS AS (" + scalar("last_probed_at") + ") STORED"},
		} {
			var count int
			if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='resources' AND COLUMN_NAME=?`, column.name).Scan(&count); err != nil {
				return err
			}
			if count == 0 {
				alter = append(alter, "ADD COLUMN "+column.name+" "+column.definition)
			}
		}
		var staleIndex int
		if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.STATISTICS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='resources' AND INDEX_NAME='resources_candidates_stale'`).Scan(&staleIndex); err != nil {
			return err
		}
		if staleIndex == 0 {
			alter = append(alter, "ADD INDEX resources_candidates_stale(kind,healthy_idx,last_probed_idx,updated_at,aggregate_pending_idx)")
		}
		if len(alter) > 0 {
			if _, err := conn.ExecContext(ctx, "ALTER TABLE resources "+strings.Join(alter, ",")); err != nil {
				return err
			}
		}
		if _, err := conn.ExecContext(ctx, "INSERT INTO schema_migrations(version,applied_at) VALUES(4,?) ON DUPLICATE KEY UPDATE version=version", s.Time(time.Now())); err != nil {
			return err
		}
		// Pending task deduplication reads only these generated identifiers;
		// the covering index avoids fetching and decoding every task document.
		alter = nil
		var candidateColumn, pendingKeysIndex int
		if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='resources' AND COLUMN_NAME='candidate_id_idx'`).Scan(&candidateColumn); err != nil {
			return err
		}
		if candidateColumn == 0 {
			alter = append(alter, "ADD COLUMN candidate_id_idx VARCHAR(512) CHARACTER SET ascii COLLATE ascii_bin GENERATED ALWAYS AS ("+scalar("candidate_id")+") STORED")
		}
		if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.STATISTICS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='resources' AND INDEX_NAME='resources_tasks_pending_keys'`).Scan(&pendingKeysIndex); err != nil {
			return err
		}
		if pendingKeysIndex == 0 {
			alter = append(alter, "ADD INDEX resources_tasks_pending_keys(kind,status_idx,node_id_idx,candidate_id_idx)")
		}
		if len(alter) > 0 {
			if _, err := conn.ExecContext(ctx, "ALTER TABLE resources "+strings.Join(alter, ",")); err != nil {
				return err
			}
		}
		if _, err := conn.ExecContext(ctx, "INSERT INTO schema_migrations(version,applied_at) VALUES(5,?) ON DUPLICATE KEY UPDATE version=version", s.Time(time.Now())); err != nil {
			return err
		}
		_, e := conn.ExecContext(ctx, "INSERT INTO schema_migrations(version,applied_at) VALUES(1,?) ON DUPLICATE KEY UPDATE version=version", time.Now().UTC().Format(time.RFC3339Nano))
		return e
	})
	if e != nil {
		return e
	}
	return s.EnsureMetricPartitions(ctx, time.Now())
}

type mysqlPartition struct{ name, upper string }

func mysqlPartitions(ctx context.Context, conn *sql.Conn) ([]mysqlPartition, error) {
	rows, e := conn.QueryContext(ctx, `SELECT PARTITION_NAME,PARTITION_DESCRIPTION FROM information_schema.PARTITIONS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='metrics' AND PARTITION_NAME IS NOT NULL ORDER BY PARTITION_ORDINAL_POSITION`)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []mysqlPartition{}
	for rows.Next() {
		var p mysqlPartition
		if e = rows.Scan(&p.name, &p.upper); e != nil {
			return nil, e
		}
		p.upper = strings.Trim(p.upper, "'")
		out = append(out, p)
	}
	return out, rows.Err()
}
func (s *Store) ensureMySQLMetricDay(ctx context.Context, at time.Time) error {
	day := time.Date(at.UTC().Year(), at.UTC().Month(), at.UTC().Day(), 0, 0, 0, 0, time.UTC)
	name := "metrics_" + day.Format("20060102")
	var count int
	if e := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.PARTITIONS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='metrics' AND PARTITION_NAME=?`, name).Scan(&count); e != nil {
		return e
	}
	if count == 1 {
		return nil
	}
	return s.mysqlDDL(ctx, func(conn *sql.Conn) error {
		parts, e := mysqlPartitions(ctx, conn)
		if e != nil {
			return e
		}
		for _, p := range parts {
			if p.name == name {
				return nil
			}
		}
		lower, start, end := "", mysqlTime(day), mysqlTime(day.AddDate(0, 0, 1))
		for _, p := range parts {
			if !safeMySQLPartitionName(p.name) {
				return errors.New("unexpected MySQL metric partition name")
			}
			if p.upper != "MAXVALUE" && p.upper <= start {
				lower = p.upper
				continue
			}
			definitions := []string{}
			if lower < start {
				definitions = append(definitions, "PARTITION metrics_gap_"+day.Format("20060102")+" VALUES LESS THAN ('"+start+"')")
			}
			definitions = append(definitions, "PARTITION "+name+" VALUES LESS THAN ('"+end+"')")
			if p.upper == "MAXVALUE" {
				definitions = append(definitions, "PARTITION "+p.name+" VALUES LESS THAN (MAXVALUE)")
			} else if p.upper > end {
				definitions = append(definitions, "PARTITION "+p.name+" VALUES LESS THAN ('"+p.upper+"')")
			} else if p.upper < end {
				return errors.New("MySQL metric partition boundaries are not aligned to UTC days")
			}
			_, e = conn.ExecContext(ctx, "ALTER TABLE metrics REORGANIZE PARTITION `"+p.name+"` INTO ("+strings.Join(definitions, ",")+")")
			return e
		}
		return errors.New("MySQL metric table has no catch-all partition")
	})
}
func (s *Store) dropMySQLMetricPartitions(ctx context.Context, cutoff time.Time) error {
	return s.mysqlDDL(ctx, func(conn *sql.Conn) error {
		parts, e := mysqlPartitions(ctx, conn)
		if e != nil {
			return e
		}
		names := []string{}
		for _, p := range parts {
			if p.upper == "MAXVALUE" {
				continue
			}
			boundary, err := time.Parse(time.RFC3339, p.upper)
			if err != nil {
				return errors.New("unexpected MySQL partition bound")
			}
			if !boundary.After(cutoff) {
				if !safeMySQLPartitionName(p.name) {
					return errors.New("unexpected MySQL partition name")
				}
				names = append(names, "`"+p.name+"`")
			}
		}
		if len(names) == 0 {
			return nil
		}
		_, e = conn.ExecContext(ctx, "ALTER TABLE metrics DROP PARTITION "+strings.Join(names, ","))
		return e
	})
}
func safeMySQLPartitionName(name string) bool {
	if !strings.HasPrefix(name, "metrics_") {
		return false
	}
	for _, r := range name {
		if r != '_' && (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

func (s *Store) leaseMySQL(ctx context.Context, id, owner string, duration time.Duration) (bool, error) {
	if duration <= 0 {
		return false, errors.New("lease duration must be positive")
	}
	tx, e := s.DB.BeginTx(ctx, nil)
	if e != nil {
		return false, e
	}
	defer tx.Rollback()
	if _, e = tx.ExecContext(ctx, `INSERT INTO job_leases(id,owner,expires_at) VALUES(?,?,?) ON DUPLICATE KEY UPDATE id=id`, id, owner, "1970-01-01T00:00:00Z"); e != nil {
		return false, e
	}
	var held, expires, clock string
	if e = tx.QueryRowContext(ctx, `SELECT owner,expires_at,DATE_FORMAT(UTC_TIMESTAMP(6),'%Y-%m-%dT%H:%i:%s.%fZ') FROM job_leases WHERE id=? FOR UPDATE`, id).Scan(&held, &expires, &clock); e != nil {
		return false, e
	}
	now, e := time.Parse(time.RFC3339Nano, clock)
	if e != nil {
		return false, e
	}
	until, e := time.Parse(time.RFC3339Nano, expires)
	if e != nil {
		return false, e
	}
	if held != owner && !until.Before(now) {
		return false, tx.Commit()
	}
	if _, e = tx.ExecContext(ctx, `UPDATE job_leases SET owner=?,expires_at=? WHERE id=?`, owner, now.Add(duration).Format("2006-01-02T15:04:05.000000000Z"), id); e != nil {
		return false, e
	}
	return true, tx.Commit()
}

// Sort IDs before acquiring InnoDB row locks; every writer uses this order.
func mysqlLockResources(ctx context.Context, tx *sql.Tx, kind string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	ids = append([]string(nil), ids...)
	sort.Strings(ids)
	join, args := mysqlResourcePoints(kind, ids)
	rows, e := tx.QueryContext(ctx, "SELECT target.id FROM "+join+" ORDER BY requested.ordinal FOR UPDATE OF target", args...)
	if e != nil {
		return e
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if e = rows.Scan(&id); e != nil {
			return e
		}
	}
	return rows.Err()
}

// A large IN list may be planned as a scan of every resource of this kind,
// acquiring unrelated next-key locks under REPEATABLE READ. Drive a join from
// the sorted requested IDs instead, with one unique primary-key lookup each.
// Casting the requested value (not the indexed column) preserves ascii_bin
// equality and makes the primary key usable independently of session charset.
func mysqlResourcePoints(kind string, ids []string) (string, []any) {
	raw, _ := json.Marshal(ids)
	return `JSON_TABLE(?, '$[*]' COLUMNS(ordinal FOR ORDINALITY,id VARCHAR(512) PATH '$')) AS requested STRAIGHT_JOIN resources AS target FORCE INDEX(PRIMARY) ON target.kind=? AND target.id=CAST(requested.id AS CHAR CHARACTER SET ascii) COLLATE ascii_bin`, []any{string(raw), kind}
}
