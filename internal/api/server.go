package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/SnowBall-Bqiu/NewNexus/internal/agenthub"
	"github.com/SnowBall-Bqiu/NewNexus/internal/auth"
	"github.com/SnowBall-Bqiu/NewNexus/internal/config"
	"github.com/SnowBall-Bqiu/NewNexus/internal/domain"
	"github.com/SnowBall-Bqiu/NewNexus/internal/events"
	"github.com/SnowBall-Bqiu/NewNexus/internal/optimizer"
	"github.com/SnowBall-Bqiu/NewNexus/internal/provider"
	"github.com/SnowBall-Bqiu/NewNexus/internal/routing"
	"github.com/SnowBall-Bqiu/NewNexus/internal/security"
	"github.com/SnowBall-Bqiu/NewNexus/internal/store"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"gorm.io/gorm"
)

type Server struct {
	cfg       config.Config
	store     *store.Store
	vault     *security.Vault
	auth      *auth.Manager
	providers *provider.Registry
	routes    *routing.Service
	optimizer *optimizer.Service
	hub       *agenthub.Hub
	broker    *events.Broker
}

func New(cfg config.Config, store *store.Store, vault *security.Vault, authManager *auth.Manager, providers *provider.Registry, routes *routing.Service, optimizer *optimizer.Service, hub *agenthub.Hub, broker *events.Broker) *Server {
	return &Server{cfg: cfg, store: store, vault: vault, auth: authManager, providers: providers, routes: routes, optimizer: optimizer, hub: hub, broker: broker}
}

func (s *Server) Router() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID, middleware.RealIP, middleware.Recoverer, s.securityHeaders, s.logRequests)
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeData(w, http.StatusOK, map[string]any{"status": "ok", "version": s.cfg.Version, "database": s.store.Driver})
	})
	r.Get("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		sqlDB, err := s.store.DB.DB()
		if err != nil || sqlDB.Ping() != nil {
			writeError(w, http.StatusServiceUnavailable, "database_unavailable", "数据库不可用")
			return
		}
		writeData(w, http.StatusOK, map[string]string{"status": "ready"})
	})
	r.Get("/install-agent.sh", s.agentInstallScript)
	r.Get("/downloads/newnexus-agent-linux-amd64", s.agentDownload)
	r.Route("/api/v1", func(api chi.Router) {
		api.Get("/auth/status", s.authStatus)
		api.Post("/auth/setup", s.authSetup)
		api.Post("/auth/login", s.authLogin)
		api.Post("/auth/logout", s.authLogout)
		api.Get("/openapi.yaml", s.openapi)
		api.Group(func(protected chi.Router) {
			protected.Use(s.auth.Middleware)
			protected.Get("/summary", s.summary)
			protected.Get("/events/stream", s.eventStream)
			protected.Get("/projects", s.listProjects)
			protected.Post("/projects", s.createProject)
			protected.Get("/connections", s.listConnections)
			protected.Post("/connections", s.createConnection)
			protected.Post("/connections/{id}/validate", s.validateConnection)
			protected.Get("/connections/{id}/zones", s.connectionZones)
			protected.Get("/connections/{id}/zones/{zone}/lines", s.connectionLines)
			protected.Get("/saas/zones", s.listSaaSZones)
			protected.Post("/saas/zones", s.createSaaSZone)
			protected.Get("/saas/hostnames", s.listCustomHostnames)
			protected.Post("/saas/hostnames", s.createCustomHostname)
			protected.Post("/saas/hostnames/{id}/sync", s.syncCustomHostname)
			protected.Get("/optimizer/sources", s.listSources)
			protected.Post("/optimizer/sources", s.createSource)
			protected.Post("/optimizer/sources/{id}/sync", s.syncSource)
			protected.Get("/optimizer/candidates", s.listCandidates)
			protected.Post("/optimizer/recompute", s.recomputeScores)
			protected.Get("/optimizer/hostnames", s.listOptimizedHostnames)
			protected.Post("/optimizer/hostnames", s.createOptimizedHostname)
			protected.Post("/optimizer/hostnames/{id}/preview", s.previewOptimizedHostname)
			protected.Post("/optimizer/hostnames/{id}/publish", s.publishOptimizedHostname)
			protected.Get("/probe/profiles", s.listProbeProfiles)
			protected.Put("/probe/profiles/{id}", s.updateProbeProfile)
			protected.Get("/nodes", s.listNodes)
			protected.Post("/nodes/enroll", s.createNodeEnrollment)
			protected.Post("/nodes/{id}/probe", s.probeNow)
			protected.Get("/routes", s.listRoutes)
			protected.Post("/routes", s.createRoute)
			protected.Get("/routes/{id}", s.routeDetail)
			protected.Post("/routes/{id}/lines", s.createRouteLine)
			protected.Post("/routes/{id}/lines/{lineId}/targets", s.createRouteTarget)
			protected.Put("/routes/{id}/lines/{lineId}/targets/{targetId}", s.updateRouteTarget)
			protected.Post("/routes/{id}/preview", s.previewRoute)
			protected.Post("/routes/{id}/apply", s.applyRoute)
			protected.Get("/releases", s.listReleases)
			protected.Post("/releases/{id}/rollback", s.rollbackRelease)
			protected.Get("/alerts", s.listAlerts)
			protected.Post("/alerts/{id}/acknowledge", s.acknowledgeAlert)
			protected.Get("/audit", s.listAudit)
			protected.Get("/settings", s.listSettings)
			protected.Put("/settings", s.updateSettings)
			protected.Post("/auth/totp/start", s.startTOTP)
			protected.Post("/auth/totp/confirm", s.confirmTOTP)
		})
	})
	r.Handle("/*", spaHandler("web/dist"))
	return r
}

func (s *Server) authStatus(w http.ResponseWriter, r *http.Request) {
	writeData(w, http.StatusOK, s.auth.Status(r))
}
func (s *Server) authSetup(w http.ResponseWriter, r *http.Request) {
	var input struct{ Username, Password string }
	if !decode(w, r, &input) {
		return
	}
	admin, err := s.auth.Setup(input.Username, input.Password)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "setup_failed", err.Error())
		return
	}
	csrf, err := s.auth.CreateSession(w, admin.ID)
	if err != nil {
		writeError(w, 500, "session_failed", err.Error())
		return
	}
	writeData(w, http.StatusCreated, map[string]any{"username": admin.Username, "csrfToken": csrf})
}
func (s *Server) authLogin(w http.ResponseWriter, r *http.Request) {
	var input struct{ Username, Password, Code string }
	if !decode(w, r, &input) {
		return
	}
	admin, err := s.auth.Login(input.Username, input.Password, input.Code)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "login_failed", err.Error())
		return
	}
	csrf, err := s.auth.CreateSession(w, admin.ID)
	if err != nil {
		writeError(w, 500, "session_failed", err.Error())
		return
	}
	writeData(w, http.StatusOK, map[string]any{"username": admin.Username, "totpEnabled": admin.TOTPEnabled, "csrfToken": csrf})
}
func (s *Server) authLogout(w http.ResponseWriter, r *http.Request) {
	s.auth.Logout(w, r)
	writeData(w, http.StatusOK, map[string]bool{"loggedOut": true})
}
func (s *Server) startTOTP(w http.ResponseWriter, r *http.Request) {
	admin, ok := auth.AdminFromContext(r.Context())
	if !ok || admin.ID == "" {
		writeError(w, 400, "totp_unavailable", "开发模式不能配置 TOTP")
		return
	}
	secret, urlValue, err := s.auth.StartTOTP(admin)
	if err != nil {
		writeError(w, 500, "totp_failed", err.Error())
		return
	}
	writeData(w, http.StatusOK, map[string]string{"secret": secret, "url": urlValue})
}
func (s *Server) confirmTOTP(w http.ResponseWriter, r *http.Request) {
	admin, _ := auth.AdminFromContext(r.Context())
	var input struct{ Code string }
	if !decode(w, r, &input) {
		return
	}
	if err := s.auth.ConfirmTOTP(admin, input.Code); err != nil {
		writeError(w, 422, "totp_invalid", err.Error())
		return
	}
	writeData(w, http.StatusOK, map[string]bool{"enabled": true})
}

func (s *Server) summary(w http.ResponseWriter, _ *http.Request) {
	counts := map[string]int64{}
	for name, model := range map[string]any{"projects": &domain.Project{}, "routes": &domain.Route{}, "candidates": &domain.Candidate{}, "nodes": &domain.ProbeNode{}, "alerts": &domain.Alert{}, "hostnames": &domain.CustomHostname{}} {
		var count int64
		query := s.store.DB.Model(model)
		if name == "alerts" {
			query = query.Where("status = ?", "open")
		}
		_ = query.Count(&count).Error
		counts[name] = count
	}
	var healthy, online int64
	_ = s.store.DB.Model(&domain.Route{}).Where("status = ?", "healthy").Count(&healthy).Error
	_ = s.store.DB.Model(&domain.ProbeNode{}).Where("status = ?", "online").Count(&online).Error
	series := make([]map[string]any, 24)
	now := time.Now().UTC().Truncate(time.Hour)
	for i := 0; i < 24; i++ {
		point := now.Add(time.Duration(i-23) * time.Hour)
		series[i] = map[string]any{"time": point.Format("15:04"), "latency": math.Round((42+8*math.Sin(float64(i)/3)+float64(i%4))*10) / 10, "success": math.Round((99.82+0.14*math.Sin(float64(i)/5))*100) / 100}
	}
	writeData(w, http.StatusOK, map[string]any{"counts": counts, "healthyRoutes": healthy, "onlineNodes": online, "latencySeries": series, "updatedAt": time.Now().UTC()})
}

func (s *Server) listProjects(w http.ResponseWriter, _ *http.Request) {
	var items []domain.Project
	s.store.DB.Order("created_at asc").Find(&items)
	writeList(w, items, len(items))
}
func (s *Server) createProject(w http.ResponseWriter, r *http.Request) {
	var item domain.Project
	if !decode(w, r, &item) {
		return
	}
	if strings.TrimSpace(item.Name) == "" {
		writeError(w, 422, "invalid_project", "项目名称不能为空")
		return
	}
	if item.Status == "" {
		item.Status = "healthy"
	}
	if item.Color == "" {
		item.Color = "#1d7a4c"
	}
	if err := s.store.DB.Create(&item).Error; err != nil {
		writeError(w, 500, "create_failed", err.Error())
		return
	}
	s.audit(r, "project.create", "project", item.ID, "创建项目 "+item.Name, nil)
	writeData(w, 201, item)
}

func (s *Server) listConnections(w http.ResponseWriter, _ *http.Request) {
	var items []domain.Connection
	s.store.DB.Order("created_at asc").Find(&items)
	writeList(w, items, len(items))
}
func (s *Server) createConnection(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name, Provider, Purposes string
		Credentials              provider.Credentials
	}
	if !decode(w, r, &input) {
		return
	}
	adapter, ok := s.providers.Get(input.Provider)
	if !ok {
		writeError(w, 422, "unknown_provider", "不支持该厂商")
		return
	}
	raw, _ := json.Marshal(input.Credentials)
	encrypted, err := s.vault.Encrypt(string(raw))
	if err != nil {
		writeError(w, 500, "encryption_failed", err.Error())
		return
	}
	capability, validationErr := adapter.Validate(r.Context(), input.Credentials)
	status := "connected"
	message := ""
	if validationErr != nil {
		status = "error"
		message = validationErr.Error()
	}
	capabilityJSON, _ := json.Marshal(capability)
	mask := "••••••••"
	for _, value := range input.Credentials {
		if value != "" {
			mask = security.MaskSecret(value)
			break
		}
	}
	now := time.Now().UTC()
	item := domain.Connection{Name: input.Name, Provider: adapter.Name(), Purposes: input.Purposes, CredentialEnc: encrypted, CredentialMask: mask, Capabilities: string(capabilityJSON), Status: status, StatusMessage: message, LastValidatedAt: &now}
	if err := s.store.DB.Create(&item).Error; err != nil {
		writeError(w, 500, "create_failed", err.Error())
		return
	}
	s.audit(r, "connection.create", "connection", item.ID, "添加连接 "+item.Name, map[string]any{"provider": item.Provider})
	writeData(w, 201, item)
}
func (s *Server) validateConnection(w http.ResponseWriter, r *http.Request) {
	connection, credentials, adapter, ok := s.connectionContext(w, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	capability, err := adapter.Validate(r.Context(), credentials)
	now := time.Now().UTC()
	connection.LastValidatedAt = &now
	if err != nil {
		connection.Status = "error"
		connection.StatusMessage = err.Error()
	} else {
		connection.Status = "connected"
		connection.StatusMessage = ""
		connection.Capabilities = mustJSON(capability)
	}
	_ = s.store.DB.Save(&connection).Error
	if err != nil {
		writeError(w, 422, "validation_failed", err.Error())
		return
	}
	writeData(w, 200, connection)
}
func (s *Server) connectionZones(w http.ResponseWriter, r *http.Request) {
	_, credentials, adapter, ok := s.connectionContext(w, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	zones, err := adapter.ListZones(r.Context(), credentials)
	if err != nil {
		writeError(w, 502, "provider_failed", err.Error())
		return
	}
	writeList(w, zones, len(zones))
}
func (s *Server) connectionLines(w http.ResponseWriter, r *http.Request) {
	_, credentials, adapter, ok := s.connectionContext(w, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	zone := provider.Zone{ID: chi.URLParam(r, "zone"), Name: r.URL.Query().Get("name")}
	lines, err := adapter.ListLines(r.Context(), credentials, zone)
	if err != nil {
		writeError(w, 502, "provider_failed", err.Error())
		return
	}
	writeList(w, lines, len(lines))
}

func (s *Server) listSaaSZones(w http.ResponseWriter, _ *http.Request) {
	var items []domain.SaaSZone
	s.store.DB.Order("created_at asc").Find(&items)
	writeList(w, items, len(items))
}
func (s *Server) createSaaSZone(w http.ResponseWriter, r *http.Request) {
	var item domain.SaaSZone
	if !decode(w, r, &item) {
		return
	}
	if item.Status == "" {
		item.Status = "checking"
	}
	if err := s.store.DB.Create(&item).Error; err != nil {
		writeError(w, 500, "create_failed", err.Error())
		return
	}
	s.audit(r, "saas_zone.create", "saas_zone", item.ID, "添加 SaaS 区域 "+item.ZoneName, nil)
	writeData(w, 201, item)
}
func (s *Server) listCustomHostnames(w http.ResponseWriter, r *http.Request) {
	var items []domain.CustomHostname
	query := s.store.DB.Order("created_at desc")
	if zone := r.URL.Query().Get("saasZoneId"); zone != "" {
		query = query.Where("saas_zone_id = ?", zone)
	}
	query.Find(&items)
	writeList(w, items, len(items))
}
func (s *Server) createCustomHostname(w http.ResponseWriter, r *http.Request) {
	var item domain.CustomHostname
	if !decode(w, r, &item) {
		return
	}
	item.Hostname = strings.ToLower(strings.TrimSpace(item.Hostname))
	item.DNSZoneName = strings.ToLower(strings.TrimSpace(item.DNSZoneName))
	if item.Hostname == "" {
		writeError(w, 422, "hostname_required", "业务主机名不能为空")
		return
	}
	if item.ValidationMethod == "" {
		item.ValidationMethod = "txt"
	}
	if item.AutomationMode == "" {
		item.AutomationMode = "manual"
	}
	if item.AutomationMode != "manual" && item.AutomationMode != "automatic" {
		writeError(w, 422, "automation_mode_invalid", "DNS 自动化方式必须是 manual 或 automatic")
		return
	}
	if item.AutomationMode == "automatic" {
		if item.DNSConnectionID == "" || item.DNSZoneID == "" || item.DNSZoneName == "" {
			writeError(w, 422, "dns_target_required", "自动 DNS 需要选择连接并填写区域 ID 与区域名称")
			return
		}
		var dnsConnection domain.Connection
		if err := s.store.DB.First(&dnsConnection, "id = ?", item.DNSConnectionID).Error; err != nil {
			writeError(w, 404, "dns_connection_not_found", "DNS 连接不存在")
			return
		}
		if _, ok := s.providers.Get(dnsConnection.Provider); !ok {
			writeError(w, 422, "provider_unknown", "DNS 连接没有可用适配器")
			return
		}
	}
	item.Status = "pending"
	item.SSLStatus = "pending_validation"
	var zone domain.SaaSZone
	if err := s.store.DB.First(&zone, "id = ?", item.SaaSZoneID).Error; err != nil {
		writeError(w, 404, "zone_not_found", "SaaS 区域不存在")
		return
	}
	var connection domain.Connection
	_ = s.store.DB.First(&connection, "id = ?", zone.ConnectionID).Error
	if connection.CredentialEnc != "" && connection.Provider == "cloudflare" {
		_, credentials, adapter, ok := s.connectionContext(w, connection.ID)
		if !ok {
			return
		}
		cloudflareAdapter, ok := adapter.(*provider.Cloudflare)
		if !ok {
			writeError(w, 422, "provider_mismatch", "连接不是 Cloudflare")
			return
		}
		result, err := cloudflareAdapter.CreateCustomHostname(r.Context(), credentials, zone.ZoneID, provider.CFCustomHostnameInput{Hostname: item.Hostname, SSL: map[string]any{"method": item.ValidationMethod, "type": "dv"}, CustomOriginServer: item.CustomOrigin})
		if err != nil {
			writeError(w, 502, "cloudflare_failed", err.Error())
			return
		}
		applyCloudflareCustomHostnameResult(&item, result)
	} else if !s.cfg.Demo {
		writeError(w, 422, "credentials_required", "Cloudflare 连接没有可用凭据")
		return
	} else {
		item.ValidationName = "_cf-custom-hostname." + item.Hostname
		item.ValidationValue = "newnexus-demo-validation"
	}
	now := time.Now().UTC()
	item.LastCheckedAt = &now
	if err := s.store.DB.Create(&item).Error; err != nil {
		writeError(w, 500, "create_failed", err.Error())
		return
	}
	if item.AutomationMode == "automatic" {
		if err := s.syncCustomHostnameDNS(r.Context(), &item, zone); err != nil {
			item.ErrorMessage = err.Error()
		} else {
			item.ErrorMessage = ""
		}
		_ = s.store.DB.Save(&item).Error
	}
	s.audit(r, "custom_hostname.create", "custom_hostname", item.ID, "添加自定义主机名 "+item.Hostname, map[string]any{"automationMode": item.AutomationMode, "dnsError": item.ErrorMessage})
	writeData(w, 201, item)
}

func (s *Server) syncCustomHostname(w http.ResponseWriter, r *http.Request) {
	var item domain.CustomHostname
	if err := s.store.DB.First(&item, "id = ?", chi.URLParam(r, "id")).Error; err != nil {
		writeError(w, 404, "hostname_not_found", "自定义主机名不存在")
		return
	}
	var zone domain.SaaSZone
	if err := s.store.DB.First(&zone, "id = ?", item.SaaSZoneID).Error; err != nil {
		writeError(w, 404, "zone_not_found", "SaaS 区域不存在")
		return
	}
	var cloudflareConnection domain.Connection
	if err := s.store.DB.First(&cloudflareConnection, "id = ?", zone.ConnectionID).Error; err != nil {
		writeError(w, 404, "connection_not_found", "Cloudflare 连接不存在")
		return
	}
	item.ErrorMessage = ""
	if cloudflareConnection.CredentialEnc != "" && item.ProviderID != "" {
		_, credentials, adapter, ok := s.connectionContext(w, cloudflareConnection.ID)
		if !ok {
			return
		}
		cloudflareAdapter, ok := adapter.(*provider.Cloudflare)
		if !ok {
			writeError(w, 422, "provider_mismatch", "SaaS 区域连接不是 Cloudflare")
			return
		}
		result, err := cloudflareAdapter.GetCustomHostname(r.Context(), credentials, zone.ZoneID, item.ProviderID)
		if err != nil {
			item.ErrorMessage = "Cloudflare 状态同步失败: " + err.Error()
		} else {
			applyCloudflareCustomHostnameResult(&item, result)
		}
	} else if !s.cfg.Demo {
		item.ErrorMessage = "Cloudflare 连接没有可用凭据或厂商资源 ID"
	}
	if item.AutomationMode == "automatic" && item.ErrorMessage == "" {
		if err := s.syncCustomHostnameDNS(r.Context(), &item, zone); err != nil {
			item.ErrorMessage = err.Error()
		}
	}
	now := time.Now().UTC()
	item.LastCheckedAt = &now
	if err := s.store.DB.Save(&item).Error; err != nil {
		writeError(w, 500, "sync_failed", err.Error())
		return
	}
	s.audit(r, "custom_hostname.sync", "custom_hostname", item.ID, "同步自定义主机名 "+item.Hostname, map[string]any{"status": item.Status, "sslStatus": item.SSLStatus, "dnsError": item.ErrorMessage})
	writeData(w, 200, item)
}

func applyCloudflareCustomHostnameResult(item *domain.CustomHostname, result map[string]any) {
	if value := stringAny(result["id"]); value != "" {
		item.ProviderID = value
	}
	if value := stringAny(result["status"]); value != "" {
		item.Status = value
	}
	if ownership, ok := result["ownership_verification"].(map[string]any); ok {
		if value := stringAny(ownership["name"]); value != "" {
			item.ValidationName = value
		}
		if value := stringAny(ownership["value"]); value != "" {
			item.ValidationValue = value
		}
	}
	if sslValue, ok := result["ssl"].(map[string]any); ok {
		if value := stringAny(sslValue["status"]); value != "" {
			item.SSLStatus = value
		}
		if records, ok := sslValue["validation_records"].([]any); ok {
			for _, rawRecord := range records {
				record, ok := rawRecord.(map[string]any)
				if !ok {
					continue
				}
				name := stringAny(record["txt_name"])
				value := stringAny(record["txt_value"])
				if name != "" && value != "" {
					item.ValidationName = name
					item.ValidationValue = value
					break
				}
			}
		}
	}
}

func (s *Server) syncCustomHostnameDNS(ctx context.Context, item *domain.CustomHostname, zone domain.SaaSZone) error {
	if item.DNSConnectionID == "" || item.DNSZoneID == "" || item.DNSZoneName == "" {
		return fmt.Errorf("自动 DNS 配置不完整")
	}
	var connection domain.Connection
	if err := s.store.DB.First(&connection, "id = ?", item.DNSConnectionID).Error; err != nil {
		return fmt.Errorf("DNS 连接不存在")
	}
	if connection.CredentialEnc == "" {
		return fmt.Errorf("DNS 连接未保存真实凭据，验证记录尚未自动写入")
	}
	raw, err := s.vault.Decrypt(connection.CredentialEnc)
	if err != nil {
		return fmt.Errorf("无法解密 DNS 连接凭据")
	}
	credentials, err := provider.ParseCredentials(raw)
	if err != nil {
		return fmt.Errorf("DNS 连接凭据无效: %w", err)
	}
	adapter, ok := s.providers.Get(connection.Provider)
	if !ok {
		return fmt.Errorf("DNS 厂商适配器不存在")
	}
	dnsZone := provider.Zone{ID: item.DNSZoneID, Name: item.DNSZoneName, Status: "active"}
	lineCode := "default"
	if connection.Provider == "dnspod" {
		lineCode = "0"
	}
	if item.ValidationMethod == "txt" {
		if item.ValidationName == "" || item.ValidationValue == "" {
			return fmt.Errorf("Cloudflare 尚未返回 TXT 验证记录")
		}
		validation := provider.Record{Name: item.ValidationName, Type: "TXT", Value: item.ValidationValue, LineCode: lineCode, TTL: 60, Weight: 100, Enabled: true}
		if err := ensureSaaSDNSRecord(ctx, adapter, credentials, dnsZone, validation, false); err != nil {
			return fmt.Errorf("写入 TXT 验证记录失败: %w", err)
		}
	}
	if item.Status != "active" || item.SSLStatus != "active" {
		return nil
	}
	if strings.TrimSpace(zone.CNAMETarget) == "" {
		return fmt.Errorf("SaaS 区域没有配置 CNAME 目标")
	}
	businessRecord := provider.Record{Name: item.Hostname, Type: "CNAME", Value: zone.CNAMETarget, LineCode: lineCode, TTL: 60, Weight: 100, Enabled: true}
	if err := ensureSaaSDNSRecord(ctx, adapter, credentials, dnsZone, businessRecord, true); err != nil {
		return fmt.Errorf("发布业务 CNAME 失败: %w", err)
	}
	return nil
}

func ensureSaaSDNSRecord(ctx context.Context, adapter provider.Adapter, credentials provider.Credentials, zone provider.Zone, desired provider.Record, exclusive bool) error {
	existing, err := adapter.ListRecords(ctx, credentials, zone, desired.Name)
	if err != nil {
		return err
	}
	var updateBefore *provider.Record
	for index := range existing {
		record := existing[index]
		if normalizeDNSName(record.Name) != normalizeDNSName(desired.Name) {
			continue
		}
		if strings.EqualFold(record.Type, desired.Type) && normalizeDNSValue(record.Value, desired.Type) == normalizeDNSValue(desired.Value, desired.Type) {
			return nil
		}
		if exclusive && !strings.EqualFold(record.Type, "CNAME") {
			return fmt.Errorf("%s 已存在 %s 记录，不能与 CNAME 共存", desired.Name, record.Type)
		}
		if exclusive && strings.EqualFold(record.Type, desired.Type) && updateBefore == nil {
			copy := record
			updateBefore = &copy
		}
	}
	change := provider.RecordChange{Operation: "create", After: &desired}
	if updateBefore != nil {
		change.Operation = "update"
		change.Before = updateBefore
	}
	if _, err := adapter.ApplyChanges(ctx, credentials, zone, []provider.RecordChange{change}); err != nil {
		return err
	}
	observed, err := adapter.ListRecords(ctx, credentials, zone, desired.Name)
	if err != nil {
		return err
	}
	for _, record := range observed {
		if normalizeDNSName(record.Name) == normalizeDNSName(desired.Name) && strings.EqualFold(record.Type, desired.Type) && normalizeDNSValue(record.Value, desired.Type) == normalizeDNSValue(desired.Value, desired.Type) {
			return nil
		}
	}
	return fmt.Errorf("厂商 API 回读未确认记录")
}

func normalizeDNSName(value string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(value)), ".")
}

func normalizeDNSValue(value, recordType string) string {
	value = strings.TrimSpace(value)
	if strings.EqualFold(recordType, "TXT") {
		return strings.Trim(value, `"`)
	}
	return normalizeDNSName(value)
}

func (s *Server) listSources(w http.ResponseWriter, _ *http.Request) {
	var items []domain.CandidateSource
	s.store.DB.Order("created_at asc").Find(&items)
	writeList(w, items, len(items))
}
func (s *Server) createSource(w http.ResponseWriter, r *http.Request) {
	var item domain.CandidateSource
	if !decode(w, r, &item) {
		return
	}
	item.Enabled = true
	item.Status = "idle"
	if err := s.store.DB.Create(&item).Error; err != nil {
		writeError(w, 500, "create_failed", err.Error())
		return
	}
	writeData(w, 201, item)
}
func (s *Server) syncSource(w http.ResponseWriter, r *http.Request) {
	count, err := s.optimizer.SyncCloudflare(r.Context(), chi.URLParam(r, "id"), parseIntDefault(r.URL.Query().Get("samplePerPrefix"), 32))
	if err != nil {
		writeError(w, 502, "sync_failed", err.Error())
		return
	}
	writeData(w, 200, map[string]int{"candidateCount": count})
}
func (s *Server) listCandidates(w http.ResponseWriter, r *http.Request) {
	var items []domain.Candidate
	limit := parseIntDefault(r.URL.Query().Get("limit"), 100)
	if limit > 500 {
		limit = 500
	}
	query := s.store.DB.Order("score desc, address asc").Limit(limit)
	if project := r.URL.Query().Get("projectId"); project != "" {
		query = query.Where("project_id = ?", project)
	}
	if tier := r.URL.Query().Get("tier"); tier != "" {
		query = query.Where("tier = ?", tier)
	}
	query.Find(&items)
	var total int64
	s.store.DB.Model(&domain.Candidate{}).Count(&total)
	writeList(w, items, int(total))
}
func (s *Server) recomputeScores(w http.ResponseWriter, r *http.Request) {
	var input struct {
		ProjectID string `json:"projectId"`
	}
	if !decode(w, r, &input) {
		return
	}
	if err := s.optimizer.RecomputeScores(input.ProjectID); err != nil {
		writeError(w, 500, "score_failed", err.Error())
		return
	}
	writeData(w, 200, map[string]bool{"recomputed": true})
}

type optimizedHostnamePreview struct {
	Hostname   string                  `json:"hostname"`
	RecordType string                  `json:"recordType"`
	Addresses  []string                `json:"addresses"`
	Changes    []provider.RecordChange `json:"changes"`
	Warnings   []string                `json:"warnings"`
	CanApply   bool                    `json:"canApply"`
}

func (s *Server) listOptimizedHostnames(w http.ResponseWriter, r *http.Request) {
	var items []domain.OptimizedHostname
	query := s.store.DB.Order("created_at asc")
	if projectID := r.URL.Query().Get("projectId"); projectID != "" {
		query = query.Where("project_id = ?", projectID)
	}
	query.Find(&items)
	writeList(w, items, len(items))
}

func (s *Server) createOptimizedHostname(w http.ResponseWriter, r *http.Request) {
	var item domain.OptimizedHostname
	if !decode(w, r, &item) {
		return
	}
	item.Hostname = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(item.Hostname)), ".")
	item.ZoneName = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(item.ZoneName)), ".")
	if item.ProjectID == "" || item.DNSConnectionID == "" || item.Hostname == "" || item.ZoneName == "" {
		writeError(w, 422, "invalid_optimized_hostname", "项目、DNS 连接、区域和主机名均为必填项")
		return
	}
	if item.Hostname == item.ZoneName || !strings.HasSuffix(item.Hostname, "."+item.ZoneName) {
		writeError(w, 422, "invalid_optimized_hostname", "优选域名必须是所选 DNS 区域下的子域名")
		return
	}
	if item.Family != 6 {
		item.Family = 4
	}
	if item.TopN <= 0 {
		item.TopN = 2
	}
	if item.TopN > 20 {
		writeError(w, 422, "invalid_top_n", "首版单个优选域名最多发布 20 个地址")
		return
	}
	if item.TTL <= 0 {
		item.TTL = 60
	}
	item.Status = "draft"
	if err := s.store.DB.Create(&item).Error; err != nil {
		writeError(w, 409, "create_failed", err.Error())
		return
	}
	s.audit(r, "optimizer_hostname.create", "optimized_hostname", item.ID, "创建优选域名 "+item.Hostname, nil)
	writeData(w, 201, item)
}

func (s *Server) previewOptimizedHostname(w http.ResponseWriter, r *http.Request) {
	_, preview, _, _, _, err := s.buildOptimizedHostnamePreview(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, 422, "preview_failed", err.Error())
		return
	}
	writeData(w, 200, preview)
}

func (s *Server) publishOptimizedHostname(w http.ResponseWriter, r *http.Request) {
	item, preview, adapter, credentials, zone, err := s.buildOptimizedHostnamePreview(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, 422, "publish_failed", err.Error())
		return
	}
	if !preview.CanApply {
		writeErrorWithData(w, 422, "publish_blocked", strings.Join(preview.Warnings, "；"), preview)
		return
	}
	if _, err := adapter.ApplyChanges(r.Context(), credentials, zone, preview.Changes); err != nil {
		item.Status = "failed"
		item.LastError = err.Error()
		_ = s.store.DB.Save(&item).Error
		writeError(w, 502, "provider_failed", err.Error())
		return
	}
	observed, err := adapter.ListRecords(r.Context(), credentials, zone, item.Hostname)
	if err != nil || !containsAllAddresses(observed, preview.RecordType, preview.Addresses) {
		item.Status = "failed"
		item.LastError = "厂商 API 回读未确认全部优选地址"
		_ = s.store.DB.Save(&item).Error
		writeError(w, 502, "verification_failed", item.LastError)
		return
	}
	now := time.Now().UTC()
	item.Status = "active"
	item.LastError = ""
	item.LastPublishedAt = &now
	item.ObservedJSON = mustJSON(observed)
	_ = s.store.DB.Save(&item).Error
	s.audit(r, "optimizer_hostname.publish", "optimized_hostname", item.ID, "发布优选域名 "+item.Hostname, map[string]any{"addresses": preview.Addresses})
	writeData(w, 200, map[string]any{"hostname": item, "preview": preview})
}

func (s *Server) buildOptimizedHostnamePreview(ctx context.Context, id string) (domain.OptimizedHostname, optimizedHostnamePreview, provider.Adapter, provider.Credentials, provider.Zone, error) {
	var item domain.OptimizedHostname
	if err := s.store.DB.First(&item, "id = ?", id).Error; err != nil {
		return item, optimizedHostnamePreview{}, nil, nil, provider.Zone{}, fmt.Errorf("优选域名不存在")
	}
	recordType := "A"
	if item.Family == 6 {
		recordType = "AAAA"
	}
	var candidates []domain.Candidate
	if err := s.store.DB.Where("project_id = ? AND family = ? AND healthy = ? AND tier IN ?", item.ProjectID, item.Family, true, []string{"active", "ranking"}).Order("score desc, address asc").Limit(item.TopN).Find(&candidates).Error; err != nil {
		return item, optimizedHostnamePreview{}, nil, nil, provider.Zone{}, err
	}
	preview := optimizedHostnamePreview{Hostname: item.Hostname, RecordType: recordType, Addresses: make([]string, 0, len(candidates))}
	for _, candidate := range candidates {
		preview.Addresses = append(preview.Addresses, candidate.Address)
	}
	if len(preview.Addresses) == 0 {
		preview.Warnings = append(preview.Warnings, "没有满足健康与榜单门槛的候选，保留当前 DNS 记录")
		return item, preview, nil, nil, provider.Zone{}, nil
	}
	var connection domain.Connection
	if err := s.store.DB.First(&connection, "id = ?", item.DNSConnectionID).Error; err != nil {
		return item, preview, nil, nil, provider.Zone{}, fmt.Errorf("DNS 连接不存在")
	}
	adapter, ok := s.providers.Get(connection.Provider)
	if !ok {
		return item, preview, nil, nil, provider.Zone{}, fmt.Errorf("DNS 厂商适配器不存在")
	}
	zone := provider.Zone{ID: item.ZoneID, Name: item.ZoneName, Status: "active"}
	if connection.CredentialEnc == "" {
		for _, address := range preview.Addresses {
			record := provider.Record{Name: item.Hostname, Type: recordType, Value: address, LineCode: "default", TTL: item.TTL, Weight: 100, Enabled: true}
			preview.Changes = append(preview.Changes, provider.RecordChange{Operation: "create", After: &record})
		}
		preview.Warnings = append(preview.Warnings, "连接未保存真实凭据，只能预览；Cloudflare 发布时固定为 DNS-only")
		return item, preview, adapter, nil, zone, nil
	}
	secretJSON, err := s.vault.Decrypt(connection.CredentialEnc)
	if err != nil {
		return item, preview, nil, nil, zone, err
	}
	credentials, err := provider.ParseCredentials(secretJSON)
	if err != nil {
		return item, preview, nil, nil, zone, err
	}
	existing, err := adapter.ListRecords(ctx, credentials, zone, item.Hostname)
	if err != nil {
		return item, preview, nil, nil, zone, err
	}
	preview.Changes = reconcileAddressRecords(existing, item.Hostname, recordType, item.TTL, preview.Addresses)
	preview.CanApply = true
	if connection.Provider == "cloudflare" {
		preview.Warnings = append(preview.Warnings, "Cloudflare A/AAAA 将以 DNS-only 发布，禁止橙云代理")
	}
	return item, preview, adapter, credentials, zone, nil
}

func reconcileAddressRecords(existing []provider.Record, hostname, recordType string, ttl int, addresses []string) []provider.RecordChange {
	desired := make(map[string]bool, len(addresses))
	changes := make([]provider.RecordChange, 0)
	for _, address := range addresses {
		desired[address] = true
		matched := false
		for index := range existing {
			if existing[index].Type == recordType && existing[index].Value == address {
				matched = true
				if existing[index].TTL != ttl {
					after := existing[index]
					after.TTL = ttl
					changes = append(changes, provider.RecordChange{Operation: "update", Before: &existing[index], After: &after})
				}
				break
			}
		}
		if !matched {
			record := provider.Record{Name: hostname, Type: recordType, Value: address, LineCode: "default", TTL: ttl, Weight: 100, Enabled: true}
			changes = append(changes, provider.RecordChange{Operation: "create", After: &record})
		}
	}
	for index := range existing {
		if existing[index].Type == recordType && !desired[existing[index].Value] {
			changes = append(changes, provider.RecordChange{Operation: "delete", Before: &existing[index]})
		}
	}
	return changes
}

func containsAllAddresses(records []provider.Record, recordType string, addresses []string) bool {
	found := make(map[string]bool)
	for _, record := range records {
		if record.Type == recordType {
			found[record.Value] = true
		}
	}
	for _, address := range addresses {
		if !found[address] {
			return false
		}
	}
	return true
}
func (s *Server) listProbeProfiles(w http.ResponseWriter, _ *http.Request) {
	var items []domain.ProbeProfile
	s.store.DB.Order("created_at asc").Find(&items)
	writeList(w, items, len(items))
}
func (s *Server) updateProbeProfile(w http.ResponseWriter, r *http.Request) {
	var item domain.ProbeProfile
	if err := s.store.DB.First(&item, "id = ?", chi.URLParam(r, "id")).Error; err != nil {
		writeError(w, 404, "not_found", "探测配置不存在")
		return
	}
	if !decode(w, r, &item) {
		return
	}
	item.ID = chi.URLParam(r, "id")
	if err := s.store.DB.Save(&item).Error; err != nil {
		writeError(w, 500, "update_failed", err.Error())
		return
	}
	writeData(w, 200, item)
}

func (s *Server) listNodes(w http.ResponseWriter, _ *http.Request) {
	var items []domain.ProbeNode
	s.store.DB.Order("status asc, name asc").Find(&items)
	writeList(w, items, len(items))
}
func (s *Server) createNodeEnrollment(w http.ResponseWriter, r *http.Request) {
	var input domain.ProbeNode
	if !decode(w, r, &input) {
		return
	}
	code, err := security.RandomToken(18)
	if err != nil {
		writeError(w, 500, "token_failed", err.Error())
		return
	}
	input.EnrollCodeHash = security.HashToken(code)
	input.Status = "pending"
	if input.Protocols == "" {
		input.Protocols = "tcp,http"
	}
	if err := s.store.DB.Create(&input).Error; err != nil {
		writeError(w, 500, "create_failed", err.Error())
		return
	}
	baseURL := strings.TrimRight(s.cfg.PublicURL, "/")
	manualCommand := fmt.Sprintf("newnexus-agent --server %s --enroll-code %s --name %s --protocols %s", shellQuote(s.cfg.AgentPublicServer), shellQuote(code), shellQuote(input.Name), shellQuote(input.Protocols))
	installArgs := fmt.Sprintf("--server %s --code %s --name %s --protocols %s --download-url %s", shellQuote(s.cfg.AgentPublicServer), shellQuote(code), shellQuote(input.Name), shellQuote(input.Protocols), shellQuote(baseURL+"/downloads/newnexus-agent-linux-amd64"))
	checksum := ""
	if file, err := os.Open(s.cfg.AgentBinaryPath); err == nil {
		hash := sha256.New()
		if _, err := io.Copy(hash, file); err == nil {
			checksum = fmt.Sprintf("%x", hash.Sum(nil))
			installArgs += " --sha256 " + shellQuote(checksum)
		}
		_ = file.Close()
	}
	if s.cfg.AgentInstallInsecure {
		installArgs += " --insecure"
		manualCommand += " --insecure"
	}
	installCommand := fmt.Sprintf("curl -fsSL %s | sudo sh -s -- %s", shellQuote(baseURL+"/install-agent.sh"), installArgs)
	writeData(w, 201, map[string]any{"node": input, "enrollmentCode": code, "command": manualCommand, "installCommand": installCommand, "binarySha256": checksum, "architecture": "linux-amd64"})
}

func (s *Server) agentInstallScript(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	w.Header().Set("Content-Disposition", `inline; filename="install-agent.sh"`)
	w.Header().Set("Cache-Control", "public, max-age=300")
	http.ServeFile(w, r, "deploy/install-agent.sh")
}

func (s *Server) agentDownload(w http.ResponseWriter, r *http.Request) {
	info, err := os.Stat(s.cfg.AgentBinaryPath)
	if err != nil || info.IsDir() {
		writeError(w, http.StatusNotFound, "agent_binary_unavailable", "主控未安装 Linux amd64 Agent 二进制")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="newnexus-agent-linux-amd64"`)
	w.Header().Set("Cache-Control", "public, max-age=3600")
	http.ServeFile(w, r, s.cfg.AgentBinaryPath)
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}
func (s *Server) probeNow(w http.ResponseWriter, r *http.Request) {
	nodeID := chi.URLParam(r, "id")
	var input agenthub.Task
	if !decode(w, r, &input) {
		return
	}
	if err := s.hub.Dispatch(nodeID, input); err != nil {
		writeError(w, 409, "dispatch_failed", err.Error())
		return
	}
	writeData(w, 202, map[string]string{"jobId": input.JobID, "status": "queued"})
}

func (s *Server) listRoutes(w http.ResponseWriter, _ *http.Request) {
	var items []domain.Route
	s.store.DB.Order("created_at asc").Find(&items)
	writeList(w, items, len(items))
}

type createRouteInput struct {
	domain.Route
	Lines []struct {
		domain.RouteLine
		Targets []domain.RouteTarget `json:"targets"`
	} `json:"lines"`
}

func (s *Server) createRoute(w http.ResponseWriter, r *http.Request) {
	var input createRouteInput
	if !decode(w, r, &input) {
		return
	}
	input.Hostname = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(input.Hostname), "."))
	if input.TTL == 0 {
		input.TTL = 60
	}
	if input.PublishMode == "" {
		input.PublishMode = "manual"
	}
	if input.SwitchMode == "" {
		input.SwitchMode = "availability_performance"
	}
	input.Status = "draft"
	if len(input.Lines) == 0 {
		writeError(w, 422, "invalid_route", "至少需要一条默认线路")
		return
	}
	for lineIndex := range input.Lines {
		if len(input.Lines[lineIndex].Targets) == 0 {
			writeError(w, 422, "invalid_route", "每条线路至少需要一个 CNAME 目标")
			return
		}
		for targetIndex := range input.Lines[lineIndex].Targets {
			target := &input.Lines[lineIndex].Targets[targetIndex]
			target.Enabled = true
			normalizeRouteTarget(target)
			if err := validateRouteTarget(input.Hostname, *target); err != nil {
				writeError(w, 422, "invalid_route_target", err.Error())
				return
			}
		}
	}
	err := s.store.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&input.Route).Error; err != nil {
			return err
		}
		for _, lineInput := range input.Lines {
			line := lineInput.RouteLine
			line.RouteID = input.ID
			if line.ActivePriority == 0 {
				for _, target := range lineInput.Targets {
					if target.Priority > 0 && (line.ActivePriority == 0 || target.Priority < line.ActivePriority) {
						line.ActivePriority = target.Priority
					}
				}
				if line.ActivePriority == 0 {
					line.ActivePriority = 10
				}
			}
			if line.MinNodes == 0 {
				line.MinNodes = 2
			}
			if line.QuorumPercent == 0 {
				line.QuorumPercent = 67
			}
			if line.HealthPerspective == "" {
				line.HealthPerspective = "matching_group"
			}
			line.Status = "unknown"
			if err := tx.Create(&line).Error; err != nil {
				return err
			}
			for _, targetInput := range lineInput.Targets {
				target := targetInput
				target.RouteLineID = line.ID
				if target.Weight == 0 {
					target.Weight = 100
				}
				if target.Priority == 0 {
					target.Priority = 10
				}
				target.Enabled = true
				target.Health = "unknown"
				if err := tx.Create(&target).Error; err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		writeError(w, 500, "create_failed", err.Error())
		return
	}
	s.audit(r, "route.create", "route", input.ID, "创建线路规则 "+input.Hostname, nil)
	detail, _ := s.routes.Detail(input.ID)
	writeData(w, 201, detail)
}
func (s *Server) routeDetail(w http.ResponseWriter, r *http.Request) {
	detail, err := s.routes.Detail(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, 404, "not_found", "线路规则不存在")
		return
	}
	writeData(w, 200, detail)
}

type createRouteLineInput struct {
	domain.RouteLine
	Targets []domain.RouteTarget `json:"targets"`
}

func (s *Server) createRouteLine(w http.ResponseWriter, r *http.Request) {
	var route domain.Route
	if err := s.store.DB.First(&route, "id = ?", chi.URLParam(r, "id")).Error; err != nil {
		writeError(w, 404, "route_not_found", "线路规则不存在")
		return
	}
	var input createRouteLineInput
	if !decode(w, r, &input) {
		return
	}
	input.Alias = strings.TrimSpace(input.Alias)
	input.ProviderLineCode = strings.TrimSpace(input.ProviderLineCode)
	if input.Alias == "" || input.ProviderLineCode == "" || len(input.Targets) == 0 {
		writeError(w, 422, "invalid_route_line", "线路别名、厂商线路代码和至少一个 CNAME 目标均为必填项")
		return
	}
	var connection domain.Connection
	if err := s.store.DB.First(&connection, "id = ?", route.DNSConnectionID).Error; err != nil {
		writeError(w, 422, "connection_not_found", "线路规则没有可用的 DNS 连接")
		return
	}
	if connection.Provider == "cloudflare" && input.ProviderLineCode != "0" && input.ProviderLineCode != "default" {
		writeError(w, 422, "provider_line_unsupported", "Cloudflare DNS 只支持默认线路")
		return
	}
	var duplicate int64
	_ = s.store.DB.Model(&domain.RouteLine{}).Where("route_id = ? AND provider_line_code = ?", route.ID, input.ProviderLineCode).Count(&duplicate).Error
	if duplicate > 0 {
		writeError(w, 409, "route_line_exists", "该厂商线路代码已存在")
		return
	}
	for index := range input.Targets {
		input.Targets[index].Enabled = true
		normalizeRouteTarget(&input.Targets[index])
		if err := validateRouteTarget(route.Hostname, input.Targets[index]); err != nil {
			writeError(w, 422, "invalid_route_target", err.Error())
			return
		}
		if input.ActivePriority == 0 || input.Targets[index].Priority < input.ActivePriority {
			input.ActivePriority = input.Targets[index].Priority
		}
	}
	input.RouteID = route.ID
	if input.HealthPerspective == "" {
		input.HealthPerspective = "matching_group"
	}
	if input.NodeGroup == "" {
		input.NodeGroup = "全局节点"
	}
	if input.MinNodes < 2 {
		input.MinNodes = 2
	}
	if input.QuorumPercent == 0 {
		input.QuorumPercent = 67
	}
	input.Status = "unknown"
	err := s.store.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&input.RouteLine).Error; err != nil {
			return err
		}
		for index := range input.Targets {
			input.Targets[index].RouteLineID = input.ID
			if err := tx.Create(&input.Targets[index]).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		writeError(w, 500, "create_failed", err.Error())
		return
	}
	s.audit(r, "route_line.create", "route_line", input.ID, "添加线路 "+input.Alias, map[string]any{"routeId": route.ID, "lineCode": input.ProviderLineCode})
	detail, _ := s.routes.Detail(route.ID)
	writeData(w, 201, detail)
}

func (s *Server) createRouteTarget(w http.ResponseWriter, r *http.Request) {
	route, line, ok := s.routeLineContext(w, chi.URLParam(r, "id"), chi.URLParam(r, "lineId"))
	if !ok {
		return
	}
	var target domain.RouteTarget
	if !decode(w, r, &target) {
		return
	}
	target.Enabled = true
	normalizeRouteTarget(&target)
	if err := validateRouteTarget(route.Hostname, target); err != nil {
		writeError(w, 422, "invalid_route_target", err.Error())
		return
	}
	target.RouteLineID = line.ID
	if err := s.store.DB.Create(&target).Error; err != nil {
		writeError(w, 500, "create_failed", err.Error())
		return
	}
	s.audit(r, "route_target.create", "route_target", target.ID, "添加 CNAME 目标 "+target.Name, map[string]any{"lineId": line.ID})
	writeData(w, 201, target)
}

func (s *Server) updateRouteTarget(w http.ResponseWriter, r *http.Request) {
	route, line, ok := s.routeLineContext(w, chi.URLParam(r, "id"), chi.URLParam(r, "lineId"))
	if !ok {
		return
	}
	var target domain.RouteTarget
	if err := s.store.DB.First(&target, "id = ? AND route_line_id = ?", chi.URLParam(r, "targetId"), line.ID).Error; err != nil {
		writeError(w, 404, "target_not_found", "CNAME 目标不存在")
		return
	}
	var input struct {
		Name     string `json:"name"`
		CNAME    string `json:"cname"`
		Priority int    `json:"priority"`
		Weight   int    `json:"weight"`
		Enabled  *bool  `json:"enabled"`
	}
	if !decode(w, r, &input) {
		return
	}
	target.Name, target.CNAME, target.Priority, target.Weight = input.Name, input.CNAME, input.Priority, input.Weight
	if input.Enabled != nil {
		target.Enabled = *input.Enabled
	}
	normalizeRouteTarget(&target)
	if err := validateRouteTarget(route.Hostname, target); err != nil {
		writeError(w, 422, "invalid_route_target", err.Error())
		return
	}
	if err := s.store.DB.Save(&target).Error; err != nil {
		writeError(w, 500, "update_failed", err.Error())
		return
	}
	s.audit(r, "route_target.update", "route_target", target.ID, "更新 CNAME 目标 "+target.Name, nil)
	writeData(w, 200, target)
}

func (s *Server) routeLineContext(w http.ResponseWriter, routeID, lineID string) (domain.Route, domain.RouteLine, bool) {
	var route domain.Route
	if err := s.store.DB.First(&route, "id = ?", routeID).Error; err != nil {
		writeError(w, 404, "route_not_found", "线路规则不存在")
		return route, domain.RouteLine{}, false
	}
	var line domain.RouteLine
	if err := s.store.DB.First(&line, "id = ? AND route_id = ?", lineID, route.ID).Error; err != nil {
		writeError(w, 404, "route_line_not_found", "线路不存在")
		return route, line, false
	}
	return route, line, true
}

func normalizeRouteTarget(target *domain.RouteTarget) {
	target.Name = strings.TrimSpace(target.Name)
	target.CNAME = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(target.CNAME)), ".")
	if target.Priority <= 0 {
		target.Priority = 10
	}
	if target.Weight <= 0 {
		target.Weight = 100
	}
	if target.Health == "" {
		target.Health = "unknown"
	}
}

func validateRouteTarget(hostname string, target domain.RouteTarget) error {
	if target.Name == "" {
		return fmt.Errorf("目标名称不能为空")
	}
	if target.Priority > 1000 {
		return fmt.Errorf("优先级必须在 1 到 1000 之间")
	}
	if target.Weight < 1 || target.Weight > 100 {
		return fmt.Errorf("权重必须在 1 到 100 之间")
	}
	return routing.ValidateTarget(hostname, target.CNAME)
}
func (s *Server) previewRoute(w http.ResponseWriter, r *http.Request) {
	preview, err := s.routes.Preview(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, 422, "preview_failed", err.Error())
		return
	}
	writeData(w, 200, preview)
}
func (s *Server) applyRoute(w http.ResponseWriter, r *http.Request) {
	admin, _ := auth.AdminFromContext(r.Context())
	release, err := s.routes.Apply(r.Context(), chi.URLParam(r, "id"), "manual", admin.Username, r.RemoteAddr)
	if err != nil {
		writeErrorWithData(w, 422, "apply_failed", err.Error(), release)
		return
	}
	writeData(w, 200, release)
}
func (s *Server) listReleases(w http.ResponseWriter, _ *http.Request) {
	var items []domain.Release
	s.store.DB.Order("created_at desc").Limit(200).Find(&items)
	writeList(w, items, len(items))
}
func (s *Server) rollbackRelease(w http.ResponseWriter, r *http.Request) {
	admin, _ := auth.AdminFromContext(r.Context())
	release, err := s.routes.Rollback(r.Context(), chi.URLParam(r, "id"), admin.Username, r.RemoteAddr)
	if err != nil {
		writeError(w, 422, "rollback_failed", err.Error())
		return
	}
	writeData(w, 200, release)
}
func (s *Server) listAlerts(w http.ResponseWriter, r *http.Request) {
	var items []domain.Alert
	query := s.store.DB.Order("created_at desc")
	if status := r.URL.Query().Get("status"); status != "" {
		query = query.Where("status = ?", status)
	}
	query.Limit(200).Find(&items)
	writeList(w, items, len(items))
}
func (s *Server) acknowledgeAlert(w http.ResponseWriter, r *http.Request) {
	now := time.Now().UTC()
	if err := s.store.DB.Model(&domain.Alert{}).Where("id = ?", chi.URLParam(r, "id")).Updates(map[string]any{"status": "acknowledged", "acknowledged_at": &now}).Error; err != nil {
		writeError(w, 500, "update_failed", err.Error())
		return
	}
	writeData(w, 200, map[string]bool{"acknowledged": true})
}
func (s *Server) listAudit(w http.ResponseWriter, _ *http.Request) {
	var items []domain.AuditEvent
	s.store.DB.Order("created_at desc").Limit(200).Find(&items)
	writeList(w, items, len(items))
}
func (s *Server) listSettings(w http.ResponseWriter, _ *http.Request) {
	var items []domain.Setting
	s.store.DB.Order("key asc").Find(&items)
	defaults := map[string]string{"retention.raw_days": "7", "retention.five_minute_days": "30", "retention.hourly_days": "180", "probe.discovery_interval": "6h", "probe.ranking_interval": "5m", "probe.active_interval": "30s", "alerts.webhook_url": "", "alerts.smtp_host": ""}
	for key, value := range defaults {
		found := false
		for _, item := range items {
			if item.Key == key {
				found = true
				break
			}
		}
		if !found {
			items = append(items, domain.Setting{Key: key, Value: value})
		}
	}
	writeList(w, items, len(items))
}
func (s *Server) updateSettings(w http.ResponseWriter, r *http.Request) {
	var values map[string]string
	if !decode(w, r, &values) {
		return
	}
	for key, value := range values {
		item := domain.Setting{Key: key, Value: value, UpdatedAt: time.Now().UTC()}
		_ = s.store.DB.Save(&item).Error
	}
	s.audit(r, "settings.update", "settings", "global", "更新系统设置", map[string]any{"keys": keys(values)})
	writeData(w, 200, values)
}

func (s *Server) eventStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, 500, "stream_unavailable", "服务器不支持事件流")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	id, events := s.broker.Subscribe()
	defer s.broker.Unsubscribe(id)
	fmt.Fprintf(w, "event: ready\ndata: {\"connected\":true}\n\n")
	flusher.Flush()
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case payload := <-events:
			fmt.Fprintf(w, "event: update\ndata: %s\n\n", payload)
			flusher.Flush()
		case <-ticker.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

func (s *Server) connectionContext(w http.ResponseWriter, id string) (domain.Connection, provider.Credentials, provider.Adapter, bool) {
	var connection domain.Connection
	if err := s.store.DB.First(&connection, "id = ?", id).Error; err != nil {
		writeError(w, 404, "connection_not_found", "连接不存在")
		return connection, nil, nil, false
	}
	if connection.CredentialEnc == "" {
		writeError(w, 422, "credentials_required", "连接没有保存真实凭据")
		return connection, nil, nil, false
	}
	raw, err := s.vault.Decrypt(connection.CredentialEnc)
	if err != nil {
		writeError(w, 500, "decrypt_failed", "无法解密连接凭据")
		return connection, nil, nil, false
	}
	credentials, err := provider.ParseCredentials(raw)
	if err != nil {
		writeError(w, 500, "credentials_invalid", err.Error())
		return connection, nil, nil, false
	}
	adapter, ok := s.providers.Get(connection.Provider)
	if !ok {
		writeError(w, 422, "provider_unknown", "未注册厂商适配器")
		return connection, nil, nil, false
	}
	return connection, credentials, adapter, true
}
func (s *Server) audit(r *http.Request, action, resourceType, resourceID, summary string, details any) {
	admin, _ := auth.AdminFromContext(r.Context())
	event := domain.AuditEvent{Action: action, ResourceType: resourceType, ResourceID: resourceID, Summary: summary, DetailsJSON: mustJSON(details), Actor: admin.Username, IPAddress: r.RemoteAddr}
	_ = s.store.DB.Create(&event).Error
}
func (s *Server) openapi(w http.ResponseWriter, _ *http.Request) {
	raw, err := os.ReadFile("api/openapi.yaml")
	if err != nil {
		writeError(w, http.StatusNotFound, "openapi_unavailable", "OpenAPI 文档不可用")
		return
	}
	w.Header().Set("Content-Type", "application/yaml")
	_, _ = w.Write(raw)
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		next.ServeHTTP(w, r)
	})
}
func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		next.ServeHTTP(w, r)
		if !strings.HasPrefix(r.URL.Path, "/health") {
			slog.Debug("http request", "method", r.Method, "path", r.URL.Path, "duration", time.Since(started))
		}
	})
}
func decode(w http.ResponseWriter, r *http.Request, value any) bool {
	decoder := json.NewDecoder(io.LimitReader(r.Body, 2<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		writeError(w, 400, "invalid_json", err.Error())
		return false
	}
	return true
}
func writeData(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}
func writeList(w http.ResponseWriter, data any, total int) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "meta": map[string]int{"total": total}})
}
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeErrorWithData(w, status, code, message, nil)
}
func writeErrorWithData(w http.ResponseWriter, status int, code, message string, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": code, "message": message, "data": data}})
}
func mustJSON(value any) string {
	if value == nil {
		return "{}"
	}
	raw, _ := json.Marshal(value)
	return string(raw)
}
func stringAny(value any) string {
	if value == nil {
		return ""
	}
	if result, ok := value.(string); ok {
		return result
	}
	return fmt.Sprint(value)
}
func parseIntDefault(raw string, fallback int) int {
	value, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	return value
}
func keys(values map[string]string) []string {
	result := make([]string, 0, len(values))
	for key := range values {
		result = append(result, key)
	}
	return result
}
func spaHandler(root string) http.Handler {
	absolute, _ := filepath.Abs(root)
	files := http.FileServer(http.Dir(absolute))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := filepath.Join(absolute, filepath.Clean(r.URL.Path))
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			if contentType := mime.TypeByExtension(filepath.Ext(path)); contentType != "" {
				w.Header().Set("Content-Type", contentType)
			}
			files.ServeHTTP(w, r)
			return
		}
		index := filepath.Join(absolute, "index.html")
		if _, err := os.Stat(index); err != nil {
			writeError(w, 404, "frontend_not_built", "前端尚未构建，请运行 npm run build")
			return
		}
		http.ServeFile(w, r, index)
	})
}
func (s *Server) Context() context.Context { return context.Background() }
