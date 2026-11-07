package main

import (
	"context"
	"database/sql"
	"log/slog"
	"time"
)

// observeMySQL is opt-in QA instrumentation. It queries only the isolated test
// database's lock metadata, uses an independent pool, and cannot block a result
// or scheduler transaction. Final performance certification runs leave it off.
func observeMySQL(ctx context.Context, admin *sql.DB, database string) {
	slog.Info("MySQL diagnostic lock observer enabled", "database", database, "interval", time.Second)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			inspection, cancel := context.WithTimeout(ctx, 2*time.Second)
			err := inspectMySQLLocks(inspection, admin, database)
			cancel()
			if err != nil && ctx.Err() == nil {
				slog.Warn("MySQL diagnostic inspection failed", "error", err)
			}
		}
	}
}

func inspectMySQLLocks(ctx context.Context, admin *sql.DB, database string) error {
	rows, err := admin.QueryContext(ctx, `SELECT COALESCE(l.OBJECT_NAME,''),l.LOCK_TYPE,LEFT(COALESCE(t.PROCESSLIST_INFO,''),256) FROM performance_schema.metadata_locks l LEFT JOIN performance_schema.threads t ON t.THREAD_ID=l.OWNER_THREAD_ID WHERE l.OBJECT_SCHEMA=? AND l.LOCK_STATUS='PENDING' LIMIT 12`, database)
	if err != nil {
		return err
	}
	for rows.Next() {
		var table, mode, query string
		if err := rows.Scan(&table, &mode, &query); err != nil {
			rows.Close()
			return err
		}
		slog.Info("MySQL metadata lock wait", "table", table, "mode", mode, "sql", query)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	rows, err = admin.QueryContext(ctx, `SELECT COALESCE(l.OBJECT_NAME,''),COALESCE(l.INDEX_NAME,''),l.LOCK_MODE,LEFT(COALESCE(l.LOCK_DATA,''),100),LEFT(COALESCE(t.PROCESSLIST_INFO,''),256) FROM performance_schema.data_lock_waits w JOIN performance_schema.data_locks l ON l.ENGINE=w.ENGINE AND l.ENGINE_LOCK_ID=w.REQUESTING_ENGINE_LOCK_ID LEFT JOIN performance_schema.threads t ON t.THREAD_ID=l.THREAD_ID WHERE l.OBJECT_SCHEMA=? LIMIT 12`, database)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var table, index, mode, data, query string
		if err := rows.Scan(&table, &index, &mode, &data, &query); err != nil {
			return err
		}
		slog.Info("MySQL row lock wait", "table", table, "index", index, "mode", mode, "data", data, "sql", query)
	}
	return rows.Err()
}
