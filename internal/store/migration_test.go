package store

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/SnowBall-Bqiu/NewNexus/internal/config"
	"github.com/SnowBall-Bqiu/NewNexus/internal/domain"
	"github.com/SnowBall-Bqiu/NewNexus/internal/testutil"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestRC2SQLiteMigrationPreservesRowsAndIsRepeatable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rc2.db")
	legacy, err := gorm.Open(sqlite.Open(path), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := legacy.Exec(`CREATE TABLE projects (
		id text PRIMARY KEY,
		created_at datetime,
		updated_at datetime,
		name text,
		description text,
		status text,
		color text
	)`).Error; err != nil {
		t.Fatal(err)
	}
	if err := legacy.Exec(`INSERT INTO projects (id, created_at, updated_at, name, status) VALUES (?, ?, ?, ?, ?)`, "rc2-project-id", time.Now().UTC(), time.Now().UTC(), "rc2-project", "healthy").Error; err != nil {
		t.Fatal(err)
	}
	if err := legacy.Exec(`CREATE TABLE custom_hostnames (
		id text PRIMARY KEY,
		saa_s_zone_id text,
		hostname text
	)`).Error; err != nil {
		t.Fatal(err)
	}
	if err := legacy.Exec(`INSERT INTO custom_hostnames (id, saa_s_zone_id, hostname) VALUES (?, ?, ?)`, "rc2-hostname-id", "rc2-saas-zone-id", "legacy.example.com").Error; err != nil {
		t.Fatal(err)
	}
	legacySQL, err := legacy.DB()
	if err != nil {
		t.Fatal(err)
	}
	if err := legacySQL.Close(); err != nil {
		t.Fatal(err)
	}

	database, err := Open(config.Config{DatabaseDriver: "sqlite", DatabaseURL: path})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.Migrate(); err != nil {
		t.Fatalf("repeat migration failed: %v", err)
	}
	var restored domain.Project
	if err := database.DB.First(&restored, "id = ?", "rc2-project-id").Error; err != nil {
		t.Fatalf("legacy row was lost: %v", err)
	}
	if restored.Name != "rc2-project" {
		t.Fatalf("legacy row changed during migration: %#v", restored)
	}
	if restored.Avatar != "r" {
		t.Fatalf("legacy project avatar was not backfilled: %#v", restored)
	}
	var restoredHostname domain.CustomHostname
	if err := database.DB.First(&restoredHostname, "id = ?", "rc2-hostname-id").Error; err != nil {
		t.Fatalf("legacy custom hostname was lost: %v", err)
	}
	if restoredHostname.SaaSZoneID != "rc2-saas-zone-id" {
		t.Fatalf("legacy SaaS zone relation was not backfilled: %#v", restoredHostname)
	}
	version, err := database.MigrationVersion()
	if err != nil || version != 2026080911 {
		t.Fatalf("unexpected migration version %d: %v", version, err)
	}
	if !database.DB.Migrator().HasTable(&domain.JobExecutionLease{}) {
		t.Fatal("job execution lease table was not created")
	}
	if !database.DB.Migrator().HasIndex(&domain.Release{}, "idx_release_route_version") {
		t.Fatal("route release version unique index was not created")
	}
	if !database.DB.Migrator().HasIndex(&domain.Candidate{}, "idx_candidates_score_address_id") {
		t.Fatal("candidate stable cursor index was not created")
	}
	if !database.DB.Migrator().HasColumn(&domain.ProbeNode{}, "RecoveryCodeHash") || !database.DB.Migrator().HasColumn(&domain.CustomHostname{}, "DNSAutomationPaused") {
		t.Fatal("production reliability columns were not created")
	}
}

func TestPostgresMigrationPreservesRowsAndIsRepeatable(t *testing.T) {
	databaseURL := testutil.IsolatedPostgresURL(t)
	database, err := Open(config.Config{DatabaseDriver: "postgres", DatabaseURL: databaseURL})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	project := domain.Project{Name: "postgres-project", Status: "healthy"}
	if err := database.DB.Create(&project).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.Migrate(); err != nil {
		t.Fatalf("repeat PostgreSQL migration failed: %v", err)
	}
	var restored domain.Project
	if err := database.DB.First(&restored, "id = ?", project.ID).Error; err != nil {
		t.Fatalf("PostgreSQL row was lost: %v", err)
	}
	version, err := database.MigrationVersion()
	if err != nil || version != 2026080911 {
		t.Fatalf("unexpected PostgreSQL migration version %d: %v", version, err)
	}
	var partitioned bool
	if err := database.DB.Raw(`SELECT EXISTS (
		SELECT 1 FROM pg_partitioned_table partitioned_table
		JOIN pg_class table_class ON table_class.oid = partitioned_table.partrelid
		JOIN pg_namespace table_namespace ON table_namespace.oid = table_class.relnamespace
		WHERE table_namespace.nspname = current_schema() AND table_class.relname = 'probe_samples'
	)`).Scan(&partitioned).Error; err != nil || !partitioned {
		t.Fatalf("probe_samples is not a PostgreSQL partitioned table: %v", err)
	}
	sample := domain.ProbeSample{JobID: "partition-test", NodeID: "node", Mode: "tcp", Success: true, ObservedAt: time.Now().UTC()}
	if err := database.DB.Create(&sample).Error; err != nil {
		t.Fatalf("insert current probe sample into daily partition: %v", err)
	}
	var partitionName string
	if err := database.DB.Raw(`SELECT tableoid::regclass::text FROM probe_samples WHERE id = ?`, sample.ID).Scan(&partitionName).Error; err != nil || partitionName == "" || partitionName == "probe_samples" {
		t.Fatalf("probe sample did not land in a child partition (%q): %v", partitionName, err)
	}
}

func TestSQLiteInstanceLeaseRejectsSecondOwner(t *testing.T) {
	database, err := Open(config.Config{DatabaseDriver: "sqlite", DatabaseURL: filepath.Join(t.TempDir(), "lease.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.AcquireLease("control-plane", "one", time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := database.AcquireLease("control-plane", "two", time.Minute); !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("expected ErrLeaseHeld, got %v", err)
	}
}
