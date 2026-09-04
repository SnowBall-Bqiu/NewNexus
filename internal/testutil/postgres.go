package testutil

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// IsolatedPostgresURL creates a test-only schema and returns a connection URL
// whose search_path is restricted to that schema.
func IsolatedPostgresURL(t *testing.T) string {
	t.Helper()
	baseURL := os.Getenv("NEWNEXUS_TEST_POSTGRES_URL")
	if baseURL == "" {
		t.Skip("NEWNEXUS_TEST_POSTGRES_URL is not configured")
	}

	admin, err := gorm.Open(postgres.Open(baseURL), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open PostgreSQL test database: %v", err)
	}
	sqlDB, err := admin.DB()
	if err != nil {
		t.Fatalf("get PostgreSQL test connection: %v", err)
	}

	schema := "newnexus_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if err := admin.Exec(fmt.Sprintf(`CREATE SCHEMA %q`, schema)).Error; err != nil {
		_ = sqlDB.Close()
		t.Fatalf("create PostgreSQL test schema: %v", err)
	}
	t.Cleanup(func() {
		_ = admin.Exec(fmt.Sprintf(`DROP SCHEMA IF EXISTS %q CASCADE`, schema)).Error
		_ = sqlDB.Close()
	})

	parsed, err := url.Parse(baseURL)
	if err != nil {
		t.Fatalf("parse PostgreSQL test URL: %v", err)
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	return parsed.String()
}
