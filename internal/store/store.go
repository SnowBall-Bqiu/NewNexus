package store

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/SnowBall-Bqiu/NewNexus/internal/config"
	"github.com/SnowBall-Bqiu/NewNexus/internal/domain"
	"github.com/glebarez/sqlite"
	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
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
	db, err := gorm.Open(dialector, &gorm.Config{Logger: logger.Default.LogMode(logger.Warn)})
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
		value, zero := field.ValueOf(tx.Statement.Context, tx.Statement.ReflectValue)
		if zero || value == "" {
			_ = field.Set(tx.Statement.Context, tx.Statement.ReflectValue, uuid.NewString())
		}
	})
}

func (s *Store) Migrate() error {
	models := []any{
		&domain.Admin{}, &domain.Session{}, &domain.APIKey{},
		&domain.Connection{}, &domain.Project{}, &domain.SaaSZone{}, &domain.CustomHostname{},
		&domain.CandidateSource{}, &domain.Candidate{}, &domain.OptimizedHostname{}, &domain.ProbeProfile{}, &domain.ProbeNode{}, &domain.ProbeSample{},
		&domain.Route{}, &domain.RouteLine{}, &domain.RouteTarget{}, &domain.Release{},
		&domain.Alert{}, &domain.AuditEvent{}, &domain.Setting{},
	}
	if err := s.DB.AutoMigrate(models...); err != nil {
		return fmt.Errorf("migrate database: %w", err)
	}
	return nil
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
	if err := s.DB.Model(&domain.Project{}).Count(&count).Error; err != nil || count > 0 {
		return err
	}
	now := time.Now().UTC()
	project := domain.Project{Name: "星桥网络", Description: "Cloudflare 与多 CDN 生产接入", Status: "healthy", Color: "#1d7a4c"}
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
		{ProjectID: project.ID, SourceID: source.ID, Address: "104.16.132.229", Family: 4, Tier: "active", Tags: "香港,移动", Healthy: true, Score: 94.8, SuccessRate: 99.98, TCPMS: 18.4, TLSMS: 32.1, TTFBMS: 48.7, DownloadBPS: 14800000, LastProbedAt: &now},
		{ProjectID: project.ID, SourceID: source.ID, Address: "172.67.74.94", Family: 4, Tier: "active", Tags: "东京,联通", Healthy: true, Score: 92.6, SuccessRate: 99.92, TCPMS: 21.7, TLSMS: 35.2, TTFBMS: 53.3, DownloadBPS: 13200000, LastProbedAt: &now},
		{ProjectID: project.ID, SourceID: source.ID, Address: "104.18.36.118", Family: 4, Tier: "ranking", Tags: "新加坡,电信", Healthy: true, Score: 89.4, SuccessRate: 99.87, TCPMS: 27.9, TLSMS: 42.7, TTFBMS: 61.1, DownloadBPS: 11900000, LastProbedAt: &now},
		{ProjectID: project.ID, SourceID: source.ID, Address: "2606:4700::6810:84e5", Family: 6, Tier: "ranking", Tags: "香港,IPv6", Healthy: true, Score: 87.9, SuccessRate: 99.80, TCPMS: 24.6, TLSMS: 39.8, TTFBMS: 58.9, DownloadBPS: 10200000, LastProbedAt: &now},
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

func ptrTime(value time.Time) *time.Time { return &value }
