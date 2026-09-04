package store

import (
	"errors"
	"fmt"
	"log"
	"log/slog"
	"os"
	"reflect"
	"time"

	"github.com/SnowBall-Bqiu/NewNexus/internal/config"
	"github.com/SnowBall-Bqiu/NewNexus/internal/domain"
	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"gorm.io/gorm/schema"
)

type Store struct {
	DB     *gorm.DB
	Driver string
}

func Open(cfg config.Config) (*Store, error) {
	var dialector gorm.Dialector
	switch cfg.DatabaseDriver {
	case "sqlite":
		dialector = sqlite.Open(cfg.DatabaseURL)
	case "postgres", "postgresql":
		dialector = postgres.Open(cfg.DatabaseURL)
	default:
		return nil, fmt.Errorf("unsupported database driver %q", cfg.DatabaseDriver)
	}
	gormLogger := logger.New(log.New(os.Stdout, "", log.LstdFlags), logger.Config{SlowThreshold: time.Second, LogLevel: logger.Warn, IgnoreRecordNotFoundError: true, Colorful: false})
	db, err := gorm.Open(dialector, &gorm.Config{Logger: gormLogger})
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	if err := registerCallbacks(db); err != nil {
		return nil, err
	}
	store := &Store{DB: db, Driver: cfg.DatabaseDriver}
	if err := store.Migrate(); err != nil {
		return nil, err
	}
	if cfg.Demo {
		if err := store.SeedDemo(); err != nil {
			return nil, err
		}
	}
	return store, nil
}

func registerCallbacks(db *gorm.DB) error {
	return db.Callback().Create().Before("gorm:create").Register("newnexus:set_uuid", func(tx *gorm.DB) {
		if tx.Statement.Schema == nil {
			return
		}
		field := tx.Statement.Schema.LookUpField("ID")
		if field == nil {
			return
		}
		value := reflect.Indirect(tx.Statement.ReflectValue)
		if value.Kind() == reflect.Slice || value.Kind() == reflect.Array {
			for index := 0; index < value.Len(); index++ {
				setUUIDField(tx, field, value.Index(index))
			}
			return
		}
		setUUIDField(tx, field, value)
	})
}

func setUUIDField(tx *gorm.DB, field *schema.Field, value reflect.Value) {
	value = reflect.Indirect(value)
	if !value.IsValid() || value.Kind() != reflect.Struct {
		return
	}
	current, zero := field.ValueOf(tx.Statement.Context, value)
	if zero || current == "" {
		_ = field.Set(tx.Statement.Context, value, uuid.NewString())
	}
}

func (s *Store) Migrate() error {
	if err := s.DB.AutoMigrate(&domain.SchemaMigration{}); err != nil {
		return fmt.Errorf("create migration table: %w", err)
	}
	migrations := []struct {
		version int64
		name    string
		apply   func(*gorm.DB) error
	}{
		{2026080901, "rc3_foundation", migrateRC3Foundation},
		{2026080902, "agent_reliability", func(tx *gorm.DB) error {
			return tx.AutoMigrate(&domain.ProbeNode{}, &domain.AgentTask{})
		}},
		{2026080903, "saas_cutover_approval", func(tx *gorm.DB) error {
			return tx.AutoMigrate(&domain.CustomHostname{})
		}},
		{2026080904, "safe_totp_rebinding", func(tx *gorm.DB) error {
			return tx.AutoMigrate(&domain.Admin{})
		}},
		{2026080905, "deduplicate_active_agent_tasks", migrateAgentTaskUniqueness},
		{2026080906, "route_sample_safety_state", func(tx *gorm.DB) error {
			return tx.AutoMigrate(&domain.RouteLine{})
		}},
		{2026080907, "postgres_probe_sample_partitions", migrateProbeSamplePartitions},
		{2026080908, "normalize_saas_zone_column", migrateSaaSZoneColumn},
		{2026080909, "project_avatar", migrateProjectAvatar},
		{2026080910, "production_reliability_hardening", migrateProductionReliabilityHardening},
		{2026080911, "cursor_pagination_indexes", migrateCursorPaginationIndexes},
	}
	for _, migration := range migrations {
		var count int64
		if err := s.DB.Model(&domain.SchemaMigration{}).Where("version = ?", migration.version).Count(&count).Error; err != nil {
			return fmt.Errorf("read migration %d: %w", migration.version, err)
		}
		if count > 0 {
			continue
		}
		if err := s.DB.Transaction(func(tx *gorm.DB) error {
			if err := migration.apply(tx); err != nil {
				return err
			}
			return tx.Create(&domain.SchemaMigration{Version: migration.version, Name: migration.name, AppliedAt: time.Now().UTC()}).Error
		}); err != nil {
			return fmt.Errorf("apply migration %d (%s): %w", migration.version, migration.name, err)
		}
		slog.Info("database migration applied", "version", migration.version, "name", migration.name)
	}
	return nil
}

func migrateProductionReliabilityHardening(tx *gorm.DB) error {
	if err := tx.AutoMigrate(&domain.ProbeNode{}, &domain.CustomHostname{}, &domain.Job{}, &domain.JobExecutionLease{}); err != nil {
		return err
	}
	var releases []domain.Release
	if err := tx.Order("route_id ASC, version ASC, created_at ASC, id ASC").Find(&releases).Error; err != nil {
		return err
	}
	nextVersion := make(map[string]int)
	for index := range releases {
		release := &releases[index]
		if release.Version <= nextVersion[release.RouteID] {
			release.Version = nextVersion[release.RouteID] + 1
			if err := tx.Model(release).Update("version", release.Version).Error; err != nil {
				return err
			}
		}
		nextVersion[release.RouteID] = release.Version
	}
	return tx.AutoMigrate(&domain.Release{})
}

func migrateCursorPaginationIndexes(tx *gorm.DB) error {
	statements := []string{
		`CREATE INDEX IF NOT EXISTS idx_projects_created_id ON projects (created_at, id)`,
		`CREATE INDEX IF NOT EXISTS idx_connections_created_id ON connections (created_at, id)`,
		`CREATE INDEX IF NOT EXISTS idx_saas_zones_created_id ON saa_s_zones (created_at, id)`,
		`CREATE INDEX IF NOT EXISTS idx_custom_hostnames_created_id ON custom_hostnames (created_at, id)`,
		`CREATE INDEX IF NOT EXISTS idx_candidate_sources_created_id ON candidate_sources (created_at, id)`,
		`CREATE INDEX IF NOT EXISTS idx_candidates_created_id ON candidates (created_at, id)`,
		`CREATE INDEX IF NOT EXISTS idx_candidates_score_address_id ON candidates (score DESC, address, id)`,
		`CREATE INDEX IF NOT EXISTS idx_routes_created_id ON routes (created_at, id)`,
		`CREATE INDEX IF NOT EXISTS idx_releases_created_id ON releases (created_at, id)`,
		`CREATE INDEX IF NOT EXISTS idx_alerts_created_id ON alerts (created_at, id)`,
		`CREATE INDEX IF NOT EXISTS idx_audit_events_created_id ON audit_events (created_at, id)`,
		`CREATE INDEX IF NOT EXISTS idx_jobs_created_id ON jobs (created_at, id)`,
	}
	for _, statement := range statements {
		if err := tx.Exec(statement).Error; err != nil {
			return err
		}
	}
	return nil
}

func migrateProjectAvatar(tx *gorm.DB) error {
	if err := tx.AutoMigrate(&domain.Project{}); err != nil {
		return err
	}
	return tx.Model(&domain.Project{}).
		Where("avatar = '' OR avatar IS NULL").
		Update("avatar", gorm.Expr("SUBSTR(name, 1, 1)")).Error
}

func migrateSaaSZoneColumn(tx *gorm.DB) error {
	hasLegacyColumn := tx.Migrator().HasColumn("custom_hostnames", "saa_s_zone_id")
	if err := tx.AutoMigrate(&domain.CustomHostname{}); err != nil {
		return err
	}
	if !hasLegacyColumn {
		return nil
	}
	return tx.Exec(`UPDATE custom_hostnames
		SET saas_zone_id = saa_s_zone_id
		WHERE (saas_zone_id IS NULL OR saas_zone_id = '')
		AND saa_s_zone_id IS NOT NULL`).Error
}

func migrateProbeSamplePartitions(tx *gorm.DB) error {
	if err := tx.AutoMigrate(&domain.ProbeSampleReceipt{}); err != nil {
		return err
	}
	if err := tx.Exec(`INSERT INTO probe_sample_receipts (job_id, sample_id, observed_at, created_at)
		SELECT job_id, id, observed_at, created_at FROM probe_samples WHERE job_id <> ''
		ON CONFLICT (job_id) DO NOTHING`).Error; err != nil {
		return err
	}
	if tx.Dialector.Name() != "postgres" {
		return nil
	}
	var partitioned bool
	if err := tx.Raw(`SELECT EXISTS (
		SELECT 1 FROM pg_partitioned_table partitioned_table
		JOIN pg_class table_class ON table_class.oid = partitioned_table.partrelid
		JOIN pg_namespace table_namespace ON table_namespace.oid = table_class.relnamespace
		WHERE table_namespace.nspname = current_schema() AND table_class.relname = 'probe_samples'
	)`).Scan(&partitioned).Error; err != nil {
		return err
	}
	if partitioned {
		return ensurePostgresProbePartitions(tx, time.Now().UTC(), 7, 32)
	}
	if err := tx.Exec(`ALTER TABLE probe_samples RENAME TO probe_samples_unpartitioned_2026080907`).Error; err != nil {
		return err
	}
	if err := tx.Exec(`CREATE TABLE probe_samples (
		LIKE probe_samples_unpartitioned_2026080907 INCLUDING DEFAULTS INCLUDING STORAGE INCLUDING COMMENTS
	) PARTITION BY RANGE (observed_at)`).Error; err != nil {
		return err
	}
	if err := tx.Exec(`UPDATE probe_samples_unpartitioned_2026080907
		SET observed_at = COALESCE(observed_at, created_at, CURRENT_TIMESTAMP)
		WHERE observed_at IS NULL`).Error; err != nil {
		return err
	}
	if err := tx.Exec(`ALTER TABLE probe_samples ADD CONSTRAINT probe_samples_partitioned_pkey PRIMARY KEY (id, observed_at)`).Error; err != nil {
		return err
	}
	boundary := time.Now().UTC().Truncate(24 * time.Hour).Add(-7 * 24 * time.Hour)
	if err := tx.Exec(fmt.Sprintf(`CREATE TABLE probe_samples_history PARTITION OF probe_samples FOR VALUES FROM (MINVALUE) TO ('%s')`, boundary.Format("2006-01-02T15:04:05Z07:00"))).Error; err != nil {
		return err
	}
	if err := ensurePostgresProbePartitions(tx, time.Now().UTC(), 7, 32); err != nil {
		return err
	}
	if err := tx.Exec(`INSERT INTO probe_samples SELECT * FROM probe_samples_unpartitioned_2026080907`).Error; err != nil {
		return err
	}
	if err := tx.Exec(`DROP TABLE probe_samples_unpartitioned_2026080907`).Error; err != nil {
		return err
	}
	for _, statement := range []string{
		`CREATE INDEX idx_probe_samples_partitioned_job_id ON probe_samples (job_id)`,
		`CREATE INDEX idx_probe_samples_partitioned_node_id ON probe_samples (node_id)`,
		`CREATE INDEX idx_probe_samples_partitioned_candidate_id ON probe_samples (candidate_id)`,
		`CREATE INDEX idx_probe_samples_partitioned_route_target_id ON probe_samples (route_target_id)`,
		`CREATE INDEX idx_probe_samples_partitioned_success ON probe_samples (success)`,
		`CREATE INDEX idx_probe_samples_partitioned_observed_at ON probe_samples (observed_at)`,
	} {
		if err := tx.Exec(statement).Error; err != nil {
			return err
		}
	}
	return nil
}

func ensurePostgresProbePartitions(tx *gorm.DB, now time.Time, pastDays, futureDays int) error {
	day := now.UTC().Truncate(24 * time.Hour)
	for offset := -pastDays; offset <= futureDays; offset++ {
		start := day.Add(time.Duration(offset) * 24 * time.Hour)
		end := start.Add(24 * time.Hour)
		name := "probe_samples_p" + start.Format("20060102")
		statement := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s PARTITION OF probe_samples FOR VALUES FROM ('%s') TO ('%s')`, name, start.Format("2006-01-02T15:04:05Z07:00"), end.Format("2006-01-02T15:04:05Z07:00"))
		if err := tx.Exec(statement).Error; err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) EnsureProbeSamplePartitions(now time.Time) error {
	if s.Driver != "postgres" && s.Driver != "postgresql" {
		return nil
	}
	return ensurePostgresProbePartitions(s.DB, now, 2, 32)
}

func migrateAgentTaskUniqueness(tx *gorm.DB) error {
	if err := tx.Exec(`UPDATE agent_tasks SET status = 'cancelled'
		WHERE id IN (
			SELECT id FROM (
				SELECT id, ROW_NUMBER() OVER (PARTITION BY node_id, candidate_id ORDER BY created_at DESC, id DESC) AS duplicate_rank
				FROM agent_tasks
				WHERE candidate_id <> '' AND status IN ('queued', 'sent', 'leased', 'acknowledged', 'running')
			) ranked WHERE duplicate_rank > 1
		)`).Error; err != nil {
		return err
	}
	if err := tx.Exec(`UPDATE agent_tasks SET status = 'cancelled'
		WHERE id IN (
			SELECT id FROM (
				SELECT id, ROW_NUMBER() OVER (PARTITION BY node_id, route_target_id ORDER BY created_at DESC, id DESC) AS duplicate_rank
				FROM agent_tasks
				WHERE route_target_id <> '' AND status IN ('queued', 'sent', 'leased', 'acknowledged', 'running')
			) ranked WHERE duplicate_rank > 1
		)`).Error; err != nil {
		return err
	}
	if err := tx.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_agent_task_active_candidate
		ON agent_tasks (node_id, candidate_id)
		WHERE candidate_id <> '' AND status IN ('queued', 'sent', 'leased', 'acknowledged', 'running')`).Error; err != nil {
		return err
	}
	return tx.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_agent_task_active_target
		ON agent_tasks (node_id, route_target_id)
		WHERE route_target_id <> '' AND status IN ('queued', 'sent', 'leased', 'acknowledged', 'running')`).Error
}

func migrateRC3Foundation(tx *gorm.DB) error {
	models := []any{
		&domain.Admin{}, &domain.Session{}, &domain.APIKey{},
		&domain.Connection{}, &domain.Project{}, &domain.ProjectSite{}, &domain.SaaSZone{}, &domain.CustomHostname{},
		&domain.CandidateSource{}, &domain.Candidate{}, &domain.OptimizedHostname{}, &domain.ProbeProfile{}, &domain.ProbeNode{}, &domain.ProbeSample{}, &domain.ProbeSampleReceipt{}, &domain.ProbeAggregate{},
		&domain.Route{}, &domain.RouteLine{}, &domain.RouteTarget{}, &domain.Release{}, &domain.ManagedDNSRecord{},
		&domain.Alert{}, &domain.NotificationChannel{}, &domain.NotificationDelivery{}, &domain.AuditEvent{}, &domain.Setting{},
		&domain.Job{}, &domain.JobAttempt{}, &domain.JobExecutionLease{}, &domain.AgentTask{}, &domain.InstanceLease{},
	}
	if err := tx.AutoMigrate(models...); err != nil {
		return err
	}
	now := time.Now().UTC()
	if err := tx.Model(&domain.ProbeNode{}).Where("protocol_version = '' OR protocol_version IS NULL").Updates(map[string]any{"protocol_version": "legacy", "compatibility_status": "degraded"}).Error; err != nil {
		return err
	}
	if err := tx.Model(&domain.CustomHostname{}).Where("ownership_status = '' OR ownership_status IS NULL").Update("ownership_status", gorm.Expr("status")).Error; err != nil {
		return err
	}
	if err := tx.Model(&domain.Alert{}).Where("occurrences = 0").Updates(map[string]any{"occurrences": 1, "last_occurred_at": now}).Error; err != nil {
		return err
	}
	if err := tx.Model(&domain.Connection{}).Where("drift_status = '' OR drift_status IS NULL").Update("drift_status", "unknown").Error; err != nil {
		return err
	}
	return tx.Model(&domain.Route{}).Where("drift_status = '' OR drift_status IS NULL").Update("drift_status", "unknown").Error
}

func (s *Store) MigrationVersion() (int64, error) {
	var migration domain.SchemaMigration
	err := s.DB.Order("version DESC").First(&migration).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return 0, nil
	}
	return migration.Version, err
}

var ErrLeaseHeld = errors.New("instance lease is held by another process")

func (s *Store) AcquireLease(name, owner string, ttl time.Duration) error {
	return s.DB.Transaction(func(tx *gorm.DB) error {
		now := time.Now().UTC()
		var lease domain.InstanceLease
		err := tx.First(&lease, "name = ?", name).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return tx.Create(&domain.InstanceLease{Name: name, Owner: owner, ExpiresAt: now.Add(ttl), UpdatedAt: now}).Error
		}
		if err != nil {
			return err
		}
		if lease.Owner != owner && lease.ExpiresAt.After(now) {
			return fmt.Errorf("%w: %s", ErrLeaseHeld, lease.Owner)
		}
		return tx.Model(&lease).Updates(map[string]any{"owner": owner, "expires_at": now.Add(ttl), "updated_at": now}).Error
	})
}

func (s *Store) RenewLease(name, owner string, ttl time.Duration) error {
	now := time.Now().UTC()
	result := s.DB.Model(&domain.InstanceLease{}).Where("name = ? AND owner = ?", name, owner).Updates(map[string]any{"expires_at": now.Add(ttl), "updated_at": now})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrLeaseHeld
	}
	return nil
}

func (s *Store) ReleaseLease(name, owner string) error {
	return s.DB.Where("name = ? AND owner = ?", name, owner).Delete(&domain.InstanceLease{}).Error
}

func (s *Store) Close() error {
	db, err := s.DB.DB()
	if err != nil {
		return err
	}
	return db.Close()
}

func (s *Store) SeedDemo() error {
	var count int64
	if err := s.DB.Model(&domain.Project{}).Count(&count).Error; err != nil {
		return err
	}
	if count > 0 {
		return s.repairLegacyDemoCandidateMetrics()
	}
	now := time.Now().UTC()
	project := domain.Project{Name: "星桥网络", Avatar: "星", Description: "Cloudflare 与多 CDN 生产接入", Status: "healthy", Color: "#1d7a4c"}
	if err := s.DB.Create(&project).Error; err != nil {
		return err
	}
	connections := []domain.Connection{
		{Name: "Cloudflare 主账户", Provider: "cloudflare", Purposes: "dns,saas", CredentialMask: "••••••••", Capabilities: `["dns","saas","default_line"]`, Status: "connected", LastValidatedAt: &now},
		{Name: "DNSPod 生产解析", Provider: "dnspod", Purposes: "dns,routing", CredentialMask: "AKID••••9T2P", Capabilities: `["dns","lines","weights"]`, Status: "connected", LastValidatedAt: &now},
		{Name: "阿里云容灾解析", Provider: "alidns", Purposes: "dns,routing", CredentialMask: "LTAI••••B7QK", Capabilities: `["dns","lines","weights"]`, Status: "warning", StatusMessage: "令牌将在 12 天后过期", LastValidatedAt: &now},
	}
	for i := range connections {
		if err := s.DB.Create(&connections[i]).Error; err != nil {
			return err
		}
	}
	profile := domain.ProbeProfile{
		ProjectID: project.ID, Name: "Cloudflare 全局 HTTPS", Scope: "optimizer", Mode: "all",
		URL: "https://speed.example.com/__health", Method: "GET", ExpectedStatuses: "200,204", TimeoutMS: 5000,
		MaxBytes: 1048576, IntervalSeconds: 30, UserAgent: "Mozilla/5.0 (X11; Linux x86_64; rv:128.0) Gecko/20100101 Firefox/128.0",
	}
	if err := s.DB.Create(&profile).Error; err != nil {
		return err
	}
	source := domain.CandidateSource{ProjectID: project.ID, Name: "Cloudflare 官方网段", Type: "cloudflare_official", Enabled: true, CandidateCount: 8246, Status: "synced", LastSyncedAt: &now}
	if err := s.DB.Create(&source).Error; err != nil {
		return err
	}
	candidates := []domain.Candidate{
		{ProjectID: project.ID, SourceID: source.ID, Address: "104.16.132.229", Family: 4, Tier: "active", Tags: "香港,移动", Healthy: true, Score: 94.8, SuccessRate: 99.98, FailureRate: 0.02, SampleCount: 60, TCPMS: 18.4, TLSMS: 32.1, TTFBMS: 48.7, P50MS: 72.0, P95MS: 118.4, DownloadBPS: 14800000, LastProbedAt: &now},
		{ProjectID: project.ID, SourceID: source.ID, Address: "172.67.74.94", Family: 4, Tier: "active", Tags: "东京,联通", Healthy: true, Score: 92.6, SuccessRate: 99.92, FailureRate: 0.08, SampleCount: 60, TCPMS: 21.7, TLSMS: 35.2, TTFBMS: 53.3, P50MS: 78.1, P95MS: 125.3, DownloadBPS: 13200000, LastProbedAt: &now},
		{ProjectID: project.ID, SourceID: source.ID, Address: "104.18.36.118", Family: 4, Tier: "ranking", Tags: "新加坡,电信", Healthy: true, Score: 89.4, SuccessRate: 99.87, FailureRate: 0.13, SampleCount: 60, TCPMS: 27.9, TLSMS: 42.7, TTFBMS: 61.1, P50MS: 85.2, P95MS: 141.9, DownloadBPS: 11900000, LastProbedAt: &now},
		{ProjectID: project.ID, SourceID: source.ID, Address: "2606:4700::6810:84e5", Family: 6, Tier: "ranking", Tags: "香港,IPv6", Healthy: true, Score: 87.9, SuccessRate: 99.80, FailureRate: 0.20, SampleCount: 60, TCPMS: 24.6, TLSMS: 39.8, TTFBMS: 58.9, P50MS: 83.8, P95MS: 136.5, DownloadBPS: 10200000, LastProbedAt: &now},
	}
	for i := range candidates {
		if err := s.DB.Create(&candidates[i]).Error; err != nil {
			return err
		}
	}
	optimizedHostname := domain.OptimizedHostname{ProjectID: project.ID, DNSConnectionID: connections[0].ID, ZoneID: "cf-zone-demo", ZoneName: "example.com", Hostname: "edge.example.com", Family: 4, TopN: 2, TTL: 60, Status: "preview"}
	if err := s.DB.Create(&optimizedHostname).Error; err != nil {
		return err
	}
	nodes := []domain.ProbeNode{
		{Name: "hk-edge-01", GroupName: "香港骨干", Region: "香港", Carrier: "移动", Protocols: "tcp,http", Version: "0.1.0", Status: "online", LastSeenAt: &now, TasksCompleted: 148240, FailureRate: 0.0012},
		{Name: "sh-telecom-01", GroupName: "华东电信", Region: "上海", Carrier: "电信", Protocols: "tcp,http", Version: "0.1.0", Status: "online", LastSeenAt: &now, TasksCompleted: 124910, FailureRate: 0.0021},
		{Name: "bj-unicom-01", GroupName: "华北联通", Region: "北京", Carrier: "联通", Protocols: "tcp,http", Version: "0.1.0", Status: "degraded", LastSeenAt: &now, TasksCompleted: 98210, FailureRate: 0.038},
	}
	for i := range nodes {
		if err := s.DB.Create(&nodes[i]).Error; err != nil {
			return err
		}
	}
	route := domain.Route{ProjectID: project.ID, Name: "主站多 CDN 接入", Hostname: "www.example.com", DNSConnectionID: connections[1].ID, ZoneName: "example.com", ZoneID: "demo-zone", TTL: 60, PublishMode: "automatic", SwitchMode: "availability_performance", Status: "healthy", ProbeProfileID: profile.ID}
	if err := s.DB.Create(&route).Error; err != nil {
		return err
	}
	lineSpecs := []struct {
		code, alias, group, status string
		priority                   int
	}{
		{"0", "默认", "香港骨干", "healthy", 10}, {"10=0", "中国电信", "华东电信", "healthy", 10}, {"10=1", "中国联通", "华北联通", "degraded", 10}, {"10=3", "中国移动", "香港骨干", "healthy", 10},
	}
	for index, spec := range lineSpecs {
		line := domain.RouteLine{RouteID: route.ID, ProviderLineCode: spec.code, Alias: spec.alias, NodeGroup: spec.group, HealthPerspective: "matching_group", MinNodes: 2, QuorumPercent: 67, Status: spec.status, ActivePriority: spec.priority}
		if err := s.DB.Create(&line).Error; err != nil {
			return err
		}
		targets := []domain.RouteTarget{
			{RouteLineID: line.ID, Name: "Cloudflare SaaS", CNAME: "customers.saas.example.net", Priority: 10, Weight: 70, Enabled: true, Health: map[bool]string{true: "healthy", false: "degraded"}[index != 2], Score: 93.2 - float64(index)},
			{RouteLineID: line.ID, Name: "网宿 CDN", CNAME: "example.wscdns.com", Priority: 10, Weight: 30, Enabled: true, Health: "healthy", Score: 88.4 + float64(index)},
			{RouteLineID: line.ID, Name: "阿里云 CDN 备用", CNAME: "example.w.kunluncan.com", Priority: 20, Weight: 100, Enabled: true, Health: "healthy", Score: 84.1},
		}
		for i := range targets {
			if err := s.DB.Create(&targets[i]).Error; err != nil {
				return err
			}
		}
	}
	zone := domain.SaaSZone{ProjectID: project.ID, ConnectionID: connections[0].ID, ZoneID: "cf-zone-demo", ZoneName: "saas.example.net", FallbackOrigin: "origin.saas.example.net", CNAMETarget: "customers.saas.example.net", Status: "active", QuotaAllocated: 100, QuotaUsed: 18, QuotaHardCap: 200}
	if err := s.DB.Create(&zone).Error; err != nil {
		return err
	}
	hosts := []domain.CustomHostname{
		{SaaSZoneID: zone.ID, Hostname: "www.example.com", ValidationMethod: "txt", AutomationMode: "automatic", Status: "active", SSLStatus: "active", ProviderID: "cf-host-01", LastCheckedAt: &now},
		{SaaSZoneID: zone.ID, Hostname: "api.example.com", ValidationMethod: "txt", AutomationMode: "manual", Status: "pending", SSLStatus: "pending_validation", ValidationName: "_cf-custom-hostname.api.example.com", ValidationValue: "demo-validation-token", ProviderID: "cf-host-02", LastCheckedAt: &now},
	}
	for i := range hosts {
		if err := s.DB.Create(&hosts[i]).Error; err != nil {
			return err
		}
	}
	releases := []domain.Release{
		{RouteID: route.ID, Version: 12, Trigger: "health", Status: "applied", DiffJSON: `{"line":"中国联通","from":"customers.saas.example.net","to":"example.wscdns.com"}`, AppliedAt: &now},
		{RouteID: route.ID, Version: 11, Trigger: "manual", Status: "applied", DiffJSON: `{"line":"中国移动","weights":"70/30"}`, AppliedAt: ptrTime(now.Add(-6 * time.Hour))},
	}
	for i := range releases {
		if err := s.DB.Create(&releases[i]).Error; err != nil {
			return err
		}
	}
	alerts := []domain.Alert{
		{Severity: "warning", Category: "route", Title: "中国联通线路性能退化", Message: "p95 延迟连续 5 轮高于备用目标 23%，已切换到网宿 CDN。", ResourceType: "route", ResourceID: route.ID, Status: "open"},
		{Severity: "info", Category: "saas", Title: "api.example.com 等待 TXT 验证", Message: "证书验证记录尚未被权威 DNS 解析。", ResourceType: "custom_hostname", ResourceID: hosts[1].ID, Status: "open"},
	}
	for i := range alerts {
		if err := s.DB.Create(&alerts[i]).Error; err != nil {
			return err
		}
	}
	slog.Info("seeded development demo data", "project", project.ID)
	return nil
}

func (s *Store) repairLegacyDemoCandidateMetrics() error {
	var project domain.Project
	if err := s.DB.Where("name = ? AND description = ?", "星桥网络", "Cloudflare 与多 CDN 生产接入").First(&project).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	} else if err != nil {
		return err
	}
	metrics := map[string]map[string]any{
		"104.16.132.229":       {"failure_rate": 0.02, "sample_count": 60, "p50_ms": 72.0, "p95_ms": 118.4},
		"172.67.74.94":         {"failure_rate": 0.08, "sample_count": 60, "p50_ms": 78.1, "p95_ms": 125.3},
		"104.18.36.118":        {"failure_rate": 0.13, "sample_count": 60, "p50_ms": 85.2, "p95_ms": 141.9},
		"2606:4700::6810:84e5": {"failure_rate": 0.20, "sample_count": 60, "p50_ms": 83.8, "p95_ms": 136.5},
	}
	for address, values := range metrics {
		if err := s.DB.Model(&domain.Candidate{}).
			Where("project_id = ? AND address = ? AND sample_count = 0", project.ID, address).
			Updates(values).Error; err != nil {
			return err
		}
	}
	return nil
}

func ptrTime(value time.Time) *time.Time { return &value }
