package api

import (
	"context"
	"errors"
	"github.com/newnexus/newnexus/internal/domain"
	"github.com/newnexus/newnexus/internal/store"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func (s *Server) systemStatus(ctx context.Context) domain.Doc {
	driver := "sqlite"
	if s.Store.Driver == "pgx" {
		driver = "postgres"
	} else if s.Store.Driver == "mysql" {
		driver = "mysql"
	}
	out := domain.Doc{"database": driver, "db_bytes": nil, "disk_free_bytes": nil, "disk_total_bytes": nil, "disk_free_percent": nil, "last_cleanup_at": nil, "last_cleanup_error": "", "partitions": []domain.Doc{}, "raw_samples": nil, "aggregates": domain.Doc{}, "observed_at": domain.Now()}
	var bytes int64
	if driver == "postgres" {
		if e := s.Store.DB.QueryRowContext(ctx, `SELECT pg_database_size(current_database())`).Scan(&bytes); e == nil {
			out["db_bytes"] = bytes
		}
		rows, e := s.Store.DB.QueryContext(ctx, `SELECT c.relname,pg_total_relation_size(c.oid) FROM pg_inherits i JOIN pg_class c ON c.oid=i.inhrelid WHERE i.inhparent='metrics'::regclass ORDER BY c.relname`)
		if e == nil {
			partitions := []domain.Doc{}
			for rows.Next() {
				var name string
				var size int64
				if rows.Scan(&name, &size) == nil {
					partitions = append(partitions, domain.Doc{"name": name, "bytes": size})
				}
			}
			rows.Close()
			out["partitions"] = partitions
		}
	} else if driver == "mysql" {
		// InnoDB reports allocated table/index bytes, not filesystem capacity.
		// A remote server's disk availability cannot be inferred from this host.
		if e := s.Store.DB.QueryRowContext(ctx, `SELECT COALESCE(SUM(DATA_LENGTH+INDEX_LENGTH),0) FROM information_schema.TABLES WHERE TABLE_SCHEMA=DATABASE()`).Scan(&bytes); e == nil {
			out["db_bytes"] = bytes
			out["db_bytes_estimated"] = true
		}
		rows, e := s.Store.DB.QueryContext(ctx, `SELECT PARTITION_NAME,COALESCE(DATA_LENGTH+INDEX_LENGTH,0) FROM information_schema.PARTITIONS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME='metrics' AND PARTITION_NAME IS NOT NULL ORDER BY PARTITION_ORDINAL_POSITION`)
		if e == nil {
			partitions := []domain.Doc{}
			for rows.Next() {
				var name string
				var size int64
				if rows.Scan(&name, &size) == nil {
					partitions = append(partitions, domain.Doc{"name": name, "bytes": size})
				}
			}
			rows.Close()
			out["partitions"] = partitions
		}
	} else {
		path := s.Store.DSN
		if strings.HasPrefix(path, "file:") {
			path = strings.TrimPrefix(strings.SplitN(path, "?", 2)[0], "file:")
		}
		if path != "" && path != ":memory:" {
			for _, suffix := range []string{"", "-wal"} {
				if stat, e := os.Stat(path + suffix); e == nil {
					bytes += stat.Size()
				}
			}
			out["db_bytes"] = bytes
			directory := filepath.Dir(path)
			if free, total, e := store.DiskUsage(directory); e == nil {
				out["disk_free_bytes"] = free
				out["disk_total_bytes"] = total
				if total > 0 {
					out["disk_free_percent"] = float64(free) * 100 / float64(total)
				}
			}
		}
	}
	if driver == "postgres" {
		out["disk_observation"] = "PostgreSQL storage may reside on a remote volume; local disk values are unavailable"
	} else if driver == "mysql" {
		out["disk_observation"] = "MySQL storage may reside on a remote volume; local disk values are unavailable"
	}
	var count int64
	if e := s.Store.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM metrics`).Scan(&count); e == nil {
		out["raw_samples"] = count
	}
	aggregates := domain.Doc{}
	for _, table := range []string{"metrics_5m", "metrics_1h"} {
		if e := s.Store.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&count); e == nil {
			aggregates[table] = count
		}
	}
	out["aggregates"] = aggregates
	if state, e := s.Store.Get(ctx, "system-state", "cleanup"); e == nil {
		out["last_cleanup_at"] = state["finished_at"]
		out["last_cleanup_error"] = state["error"]
		out["cleanup_duration_ms"] = state["duration_ms"]
	}
	if state, e := s.Store.Get(ctx, "system-state", "storage"); e == nil {
		out["db_growth_bytes"] = state["growth_bytes"]
		out["db_growth_window_seconds"] = state["window_seconds"]
	}
	return out
}
func (s *Server) systemRoutes(w http.ResponseWriter, r *http.Request, path string) bool {
	if path != "system/status" {
		return false
	}
	if r.Method != "GET" {
		fail(w, 405, "method_not_allowed", errors.New("use GET"))
		return true
	}
	write(w, 200, s.systemStatus(r.Context()))
	return true
}
func (s *Server) observeStorage(ctx context.Context) {
	prior, e := s.Store.Get(ctx, "system-state", "storage")
	if e != nil {
		prior = domain.Doc{"id": "storage"}
	}
	at, _ := time.Parse(time.RFC3339Nano, domain.S(prior, "observed_at"))
	if time.Since(at) < time.Minute {
		return
	}
	status := s.systemStatus(ctx)
	state := domain.Doc{"id": "storage", "_version": prior["_version"], "observed_at": domain.Now(), "db_bytes": status["db_bytes"], "growth_bytes": nil, "window_seconds": nil}
	if prior["db_bytes"] != nil && status["db_bytes"] != nil {
		state["growth_bytes"] = domain.N(status, "db_bytes") - domain.N(prior, "db_bytes")
		state["window_seconds"] = time.Since(at).Seconds()
	}
	_ = s.Store.Put(ctx, "system-state", state)
	if status["disk_free_percent"] != nil {
		free := domain.N(status, "disk_free_percent")
		if free < s.setting(ctx, "system.disk_critical_percent", 5) {
			s.Alert(ctx, "critical", "disk_critical", "磁盘空间严重不足", "Storage free percentage is below the configured critical threshold", "system", "storage")
		} else if free < s.setting(ctx, "system.disk_warning_percent", 15) {
			s.Alert(ctx, "warning", "disk_warning", "磁盘空间不足", "Storage free percentage is below the configured warning threshold", "system", "storage")
		}
	}
}
