// Package testutil provides isolated databases for integration tests. It is
// imported by tests only; production code never creates arbitrary databases.
package testutil

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/newnexus/newnexus/internal/domain"
	"github.com/newnexus/newnexus/internal/store"
)

func Open(t testing.TB, sqlitePath string) *store.Store {
	t.Helper()
	if os.Getenv("NEWNEXUS_TEST_BACKEND") == "mysql" {
		return MySQL(t)
	}
	db, err := store.Open(sqlitePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.DB.Close() })
	return db
}

// MySQL creates and later removes only a randomly named test database. The
// supplied QA account needs CREATE/DROP DATABASE on the disposable test server.
func MySQL(t testing.TB) *store.Store {
	t.Helper()
	dsn := os.Getenv("NEWNEXUS_TEST_MYSQL")
	if dsn == "" {
		t.Skip("set NEWNEXUS_TEST_MYSQL to an isolated MySQL 8.4 QA server")
	}
	u, err := url.Parse(dsn)
	if err != nil || u.Scheme != "mysql" {
		t.Fatal("NEWNEXUS_TEST_MYSQL must be a mysql:// URL")
	}
	cfg, err := store.MySQLConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.DBName = ""
	connector, err := mysql.NewConnector(cfg)
	if err != nil {
		t.Fatal(err)
	}
	admin := sql.OpenDB(connector)
	name := "newnexus_qa_" + strings.ReplaceAll(domain.ID(), "-", "")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err = admin.ExecContext(ctx, "CREATE DATABASE `"+name+"` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin"); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), 20*time.Second)
		defer done()
		if _, err := admin.ExecContext(cleanup, "DROP DATABASE `"+name+"`"); err != nil {
			t.Error(err)
		}
		admin.Close()
	})
	u.Path = "/" + name
	db, err := store.Open(u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.DB.Close() })
	return db
}
