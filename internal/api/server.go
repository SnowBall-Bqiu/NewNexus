package api

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/mail"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/SnowBall-Bqiu/NewNexus/internal/agenthub"
	"github.com/SnowBall-Bqiu/NewNexus/internal/auth"
	"github.com/SnowBall-Bqiu/NewNexus/internal/config"
	"github.com/SnowBall-Bqiu/NewNexus/internal/dnsverify"
	"github.com/SnowBall-Bqiu/NewNexus/internal/domain"
	"github.com/SnowBall-Bqiu/NewNexus/internal/events"
	"github.com/SnowBall-Bqiu/NewNexus/internal/jobs"
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
	jobs      *jobs.Manager
}

type requestSecurityContextKey string

const forwardedHTTPSContextKey requestSecurityContextKey = "forwarded_https"

func New(cfg config.Config, store *store.Store, vault *security.Vault, authManager *auth.Manager, providers *provider.Registry, routes *routing.Service, optimizer *optimizer.Service, hub *agenthub.Hub, broker *events.Broker, jobManager *jobs.Manager) *Server {
	server := &Server{cfg: cfg, store: store, vault: vault, auth: authManager, providers: providers, routes: routes, optimizer: optimizer, hub: hub, broker: broker, jobs: jobManager}
	server.registerJobHandlers()
	return server
}

func (s *Server) Router() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.RequestID, s.realIP, middleware.Recoverer, s.securityHeaders, s.logRequests)
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
	r.Get("/downloads/newnexus-agent-linux-{architecture}", s.agentDownload)
	r.Route("/api/v1", func(api chi.Router) {
		api.Get("/auth/status", s.authStatus)
		api.Post("/auth/setup", s.authSetup)
		api.Post("/auth/login", s.authLogin)
		api.Post("/auth/logout", s.authLogout)
		api.Get("/openapi.yaml", s.openapi)
		api.Group(func(protected chi.Router) {
			protected.Use(s.auth.Middleware)
			protected.Get("/summary", s.summary)
			protected.Get("/observability", s.observability)
			protected.Get("/events/stream", s.eventStream)
			protected.Get("/jobs", s.listJobs)
			protected.Get("/jobs/{id}", s.getJob)
			protected.Post("/jobs/{id}/cancel", s.cancelJob)
			protected.Post("/jobs/{id}/retry", s.retryJob)
			protected.Post("/jobs/{id}/resume", s.resumeJob)
			protected.Get("/projects", s.listProjects)
			protected.Post("/projects", s.createProject)
			protected.Put("/projects/{id}", s.updateProject)
			protected.Delete("/projects/{id}", s.deleteProject)
			protected.Get("/projects/{id}/sites", s.listProjectSites)
			protected.Post("/projects/{id}/sites", s.createProjectSite)
			protected.Put("/projects/{id}/sites/{siteId}", s.updateProjectSite)
			protected.Delete("/projects/{id}/sites/{siteId}", s.deleteProjectSite)
			protected.Get("/connections", s.listConnections)
			protected.Post("/connections", s.createConnection)
			protected.Put("/connections/{id}", s.updateConnection)
			protected.Delete("/connections/{id}", s.deleteConnection)
			protected.Post("/connections/{id}/validate", s.validateConnection)
			protected.Get("/connections/{id}/zones", s.connectionZones)
			protected.Get("/connections/{id}/zones/{zone}/lines", s.connectionLines)
			protected.Get("/saas/zones", s.listSaaSZones)
			protected.Post("/saas/zones", s.createSaaSZone)
			protected.Post("/saas/zones/{id}/sync", s.syncSaaSZone)
			protected.Get("/saas/hostnames", s.listCustomHostnames)
			protected.Post("/saas/hostnames", s.createCustomHostname)
			protected.Post("/saas/hostnames/bulk", s.bulkCreateCustomHostnames)
			protected.Post("/saas/hostnames/bulk-delete", s.bulkDeleteCustomHostnames)
			protected.Post("/saas/hostnames/{id}/sync", s.syncCustomHostname)
			protected.Post("/saas/hostnames/{id}/resume-automation", s.resumeCustomHostnameAutomation)
			protected.Post("/saas/hostnames/{id}/cutover", s.approveCustomHostnameCutover)
			protected.Delete("/saas/hostnames/{id}", s.deleteCustomHostname)
			protected.Get("/optimizer/sources", s.listSources)
			protected.Post("/optimizer/sources", s.createSource)
			protected.Post("/optimizer/sources/{id}/sync", s.syncSource)
			protected.Get("/optimizer/candidates", s.listCandidates)
			protected.Get("/optimizer/ranking-scopes", s.listRankingScopes)
			protected.Post("/optimizer/recompute", s.recomputeScores)
			protected.Get("/optimizer/hostnames", s.listOptimizedHostnames)
			protected.Post("/optimizer/hostnames", s.createOptimizedHostname)
			protected.Post("/optimizer/hostnames/{id}/preview", s.previewOptimizedHostname)
			protected.Post("/optimizer/hostnames/{id}/publish", s.publishOptimizedHostname)
			protected.Get("/probe/profiles", s.listProbeProfiles)
			protected.Post("/probe/profiles", s.createProbeProfile)
			protected.Put("/probe/profiles/{id}", s.updateProbeProfile)
			protected.Get("/nodes", s.listNodes)
			protected.Post("/nodes/enroll", s.createNodeEnrollment)
			protected.Post("/nodes/{id}/recovery-code", s.createNodeRecoveryCode)
			protected.Delete("/nodes/{id}/recovery-code", s.revokeNodeRecoveryCode)
			protected.Post("/nodes/{id}/re-enroll", s.createNodeRecoveryCode)
			protected.Post("/nodes/{id}/rotate", s.rotateNodeCredentials)
			protected.Post("/nodes/{id}/revoke", s.revokeNode)
			protected.Delete("/nodes/{id}", s.deleteNode)
			protected.Post("/nodes/{id}/probe", s.probeNow)
			protected.Get("/routes", s.listRoutes)
			protected.Post("/routes", s.createRoute)
			protected.Get("/routes/{id}", s.routeDetail)
			protected.Put("/routes/{id}", s.updateRoute)
			protected.Delete("/routes/{id}", s.deleteRoute)
			protected.Post("/routes/{id}/lines", s.createRouteLine)
			protected.Put("/routes/{id}/lines/{lineId}", s.updateRouteLine)
			protected.Delete("/routes/{id}/lines/{lineId}", s.deleteRouteLine)
			protected.Post("/routes/{id}/lines/{lineId}/targets", s.createRouteTarget)
			protected.Put("/routes/{id}/lines/{lineId}/targets/{targetId}", s.updateRouteTarget)
			protected.Delete("/routes/{id}/lines/{lineId}/targets/{targetId}", s.deleteRouteTarget)
			protected.Post("/routes/{id}/preview", s.previewRoute)
			protected.Post("/routes/{id}/apply", s.applyRoute)
			protected.Get("/releases", s.listReleases)
			protected.Post("/releases/{id}/rollback", s.rollbackRelease)
			protected.Get("/alerts", s.listAlerts)
			protected.Post("/alerts/{id}/acknowledge", s.acknowledgeAlert)
			protected.Post("/alerts/{id}/resolve", s.resolveAlert)
			protected.Post("/alerts/{id}/silence", s.silenceAlert)
			protected.Post("/notifications/test", s.testNotifications)
			protected.Get("/audit", s.listAudit)
			protected.Get("/settings", s.listSettings)
			protected.Put("/settings", s.updateSettings)
			protected.Post("/auth/totp/start", s.startTOTP)
			protected.Post("/auth/totp/confirm", s.confirmTOTP)
			protected.Post("/auth/totp/disable", s.disableTOTP)
			protected.Post("/auth/recovery-codes/regenerate", s.regenerateRecoveryCodes)
			protected.Get("/auth/sessions", s.listSessions)
			protected.Post("/auth/sessions/revoke-all", s.revokeAllSessions)
			protected.Get("/api-keys", s.listAPIKeys)
			protected.Post("/api-keys", s.createAPIKey)
			protected.Post("/api-keys/{id}/revoke", s.revokeAPIKey)
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
	admin, recoveryCodes, err := s.auth.SetupWithRecovery(input.Username, input.Password)
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, "setup_failed", err.Error())
		return
	}
	csrf, err := s.auth.CreateSessionForRequest(w, r, admin.ID)
	if err != nil {
		writeError(w, 500, "session_failed", err.Error())
		return
	}
	writeData(w, http.StatusCreated, map[string]any{"username": admin.Username, "csrfToken": csrf, "recoveryCodes": recoveryCodes, "totpEnabled": false})
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
	csrf, err := s.auth.CreateSessionForRequest(w, r, admin.ID)
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
	s.audit(r, "auth.totp.start", "admin", admin.ID, "开始绑定 TOTP", map[string]any{"rebind": admin.TOTPEnabled})
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
	s.audit(r, "auth.totp.enable", "admin", admin.ID, "启用 TOTP", nil)
	writeData(w, http.StatusOK, map[string]bool{"enabled": true})
}

func (s *Server) disableTOTP(w http.ResponseWriter, r *http.Request) {
	admin, ok := auth.AdminFromContext(r.Context())
	if !ok || admin.ID == "" {
		writeError(w, http.StatusBadRequest, "totp_unavailable", "当前会话不能修改 TOTP")
		return
	}
	var input struct{ Code string }
	if !decode(w, r, &input) {
		return
	}
	if input.Code == "" {
		writeError(w, http.StatusUnprocessableEntity, "totp_code_required", "请输入当前验证码")
		return
	}
	if err := s.auth.DisableTOTP(admin, input.Code); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "totp_invalid", err.Error())
		return
	}
	s.audit(r, "auth.totp.disable", "admin", admin.ID, "停用 TOTP", nil)
	writeData(w, http.StatusOK, map[string]bool{"enabled": false})
}

func (s *Server) regenerateRecoveryCodes(w http.ResponseWriter, r *http.Request) {
	admin, ok := auth.AdminFromContext(r.Context())
	if !ok || admin.ID == "" {
		writeError(w, http.StatusBadRequest, "recovery_unavailable", "当前会话不能生成恢复码")
		return
	}
	codes, err := s.auth.RegenerateRecoveryCodes(admin.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "recovery_failed", "恢复码生成失败")
		return
	}
	s.audit(r, "auth.recovery.regenerate", "admin", admin.ID, "重新生成恢复码", nil)
	writeData(w, http.StatusOK, map[string]any{"recoveryCodes": codes})
}

func (s *Server) listSessions(w http.ResponseWriter, r *http.Request) {
	admin, ok := auth.AdminFromContext(r.Context())
	if !ok || admin.ID == "" {
		writeError(w, http.StatusBadRequest, "sessions_unavailable", "当前会话不能读取会话")
		return
	}
	var sessions []domain.Session
	if err := s.store.DB.Select("id, admin_id, expires_at, last_seen, ip_address, user_agent, revoked_at, created_at, updated_at").Where("admin_id = ?", admin.ID).Order("last_seen desc").Limit(100).Find(&sessions).Error; err != nil {
		writeError(w, http.StatusInternalServerError, "sessions_unavailable", "会话读取失败")
		return
	}
	writeList(w, sessions, len(sessions))
}

func (s *Server) revokeAllSessions(w http.ResponseWriter, r *http.Request) {
	admin, ok := auth.AdminFromContext(r.Context())
	if !ok || admin.ID == "" {
		writeError(w, http.StatusBadRequest, "sessions_unavailable", "当前会话不能撤销会话")
		return
	}
	if err := s.auth.RevokeAllSessions(admin.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "sessions_revoke_failed", "会话撤销失败")
		return
	}
	s.audit(r, "auth.sessions.revoke_all", "admin", admin.ID, "撤销全部会话", nil)
	writeData(w, http.StatusOK, map[string]bool{"revoked": true})
}

func (s *Server) listAPIKeys(w http.ResponseWriter, r *http.Request) {
	admin, ok := auth.AdminFromContext(r.Context())
	if !ok || admin.ID == "" {
		writeError(w, http.StatusBadRequest, "api_keys_unavailable", "当前会话不能读取 API Key")
		return
	}
	var keys []domain.APIKey
	if err := s.store.DB.Select("id, admin_id, name, prefix, scopes, last_used_at, expires_at, revoked_at, created_at, updated_at").Where("admin_id = ?", admin.ID).Order("created_at desc").Find(&keys).Error; err != nil {
		writeError(w, http.StatusInternalServerError, "api_keys_unavailable", "API Key 读取失败")
		return
	}
	writeList(w, keys, len(keys))
}

func (s *Server) createAPIKey(w http.ResponseWriter, r *http.Request) {
	admin, ok := auth.AdminFromContext(r.Context())
	if !ok || admin.ID == "" {
		writeError(w, http.StatusBadRequest, "api_key_unavailable", "当前会话不能创建 API Key")
		return
	}
	var input struct {
		Name      string     `json:"name"`
		Scopes    string     `json:"scopes"`
		ExpiresAt *time.Time `json:"expiresAt"`
	}
	if !decode(w, r, &input) {
		return
	}
	key, secret, err := s.auth.CreateAPIKey(admin.ID, input.Name, input.Scopes, input.ExpiresAt)
	if err != nil {
		if errors.Is(err, auth.ErrInvalidAPIKeyInput) {
			writeError(w, http.StatusUnprocessableEntity, "api_key_invalid", "API Key 名称、作用域或有效期无效")
			return
		}
		writeError(w, http.StatusInternalServerError, "api_key_create_failed", "API Key 创建失败")
		return
	}
	s.audit(r, "auth.api_key.create", "api_key", key.ID, "创建 API Key", map[string]any{"name": key.Name, "scopes": key.Scopes})
	writeData(w, http.StatusCreated, map[string]any{"key": key, "secret": secret})
}

func (s *Server) revokeAPIKey(w http.ResponseWriter, r *http.Request) {
	admin, ok := auth.AdminFromContext(r.Context())
	if !ok || admin.ID == "" {
		writeError(w, http.StatusBadRequest, "api_key_unavailable", "当前会话不能撤销 API Key")
		return
	}
	now := time.Now().UTC()
	result := s.store.DB.Model(&domain.APIKey{}).Where("id = ? AND admin_id = ? AND revoked_at IS NULL", chi.URLParam(r, "id"), admin.ID).Update("revoked_at", now)
	if result.Error != nil {
		writeError(w, http.StatusInternalServerError, "api_key_revoke_failed", "API Key 撤销失败")
		return
	}
	if result.RowsAffected == 0 {
		writeError(w, http.StatusNotFound, "api_key_not_found", "API Key 不存在或已撤销")
		return
	}
	s.audit(r, "auth.api_key.revoke", "api_key", chi.URLParam(r, "id"), "撤销 API Key", nil)
	writeData(w, http.StatusOK, map[string]bool{"revoked": true})
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
	series := s.summarySeries()
	writeData(w, http.StatusOK, map[string]any{"counts": counts, "healthyRoutes": healthy, "onlineNodes": online, "latencySeries": series, "database": s.store.Driver, "version": s.cfg.Version, "updatedAt": time.Now().UTC()})
}

func (s *Server) observability(w http.ResponseWriter, _ *http.Request) {
	queue := map[string]int64{}
	for _, status := range []string{jobs.StatusQueued, jobs.StatusRunning, jobs.StatusFailed} {
		var count int64
		_ = s.store.DB.Model(&domain.Job{}).Where("status = ?", status).Count(&count).Error
		queue[status] = count
	}
	oldestLatency := 0.0
	var oldest domain.Job
	if s.store.DB.Where("status = ?", jobs.StatusQueued).Order("run_at asc").First(&oldest).Error == nil {
		oldestLatency = time.Since(oldest.RunAt).Seconds()
		if oldestLatency < 0 {
			oldestLatency = 0
		}
	}
	databaseBytes := int64(0)
	if s.store.Driver == "postgres" || s.store.Driver == "postgresql" {
		_ = s.store.DB.Raw("SELECT pg_database_size(current_database())").Scan(&databaseBytes).Error
	} else if info, err := os.Stat(s.cfg.DatabaseURL); err == nil {
		databaseBytes = info.Size()
	}
	var samples, aggregates int64
	_ = s.store.DB.Model(&domain.ProbeSample{}).Count(&samples).Error
	_ = s.store.DB.Model(&domain.ProbeAggregate{}).Count(&aggregates).Error
	migration, _ := s.store.MigrationVersion()
	writeData(w, http.StatusOK, map[string]any{"database": map[string]any{"driver": s.store.Driver, "bytes": databaseBytes, "migrationVersion": migration}, "jobs": map[string]any{"queueDepth": queue, "oldestQueuedSeconds": oldestLatency}, "metrics": map[string]any{"rawSamples": samples, "aggregates": aggregates}, "updatedAt": time.Now().UTC()})
}

func (s *Server) summarySeries() []map[string]any {
	now := time.Now().UTC().Truncate(time.Hour)
	series := make([]map[string]any, 24)
	type bucket struct{ total, success, latency float64 }
	buckets := make(map[time.Time]bucket)
	var samples []domain.ProbeSample
	_ = s.store.DB.Where("observed_at >= ?", now.Add(-23*time.Hour)).Find(&samples).Error
	for _, sample := range samples {
		start := sample.ObservedAt.UTC().Truncate(time.Hour)
		item := buckets[start]
		item.total++
		if sample.Success {
			item.success++
		}
		if sample.Success {
			item.latency += sample.TotalMS
		}
		buckets[start] = item
	}
	for i := 0; i < 24; i++ {
		point := now.Add(time.Duration(i-23) * time.Hour)
		item := buckets[point]
		latency, success := 0.0, 0.0
		if item.success > 0 {
			latency = item.latency / item.success
		}
		if item.total > 0 {
			success = item.success / item.total * 100
		}
		series[i] = map[string]any{"time": point.Format("15:04"), "latency": latency, "success": success, "samples": item.total}
	}
	return series
}

func (s *Server) listProjects(w http.ResponseWriter, r *http.Request) {
	var items []domain.Project
	query := s.store.DB.Model(&domain.Project{})
	total, limit := countAndLimit(query, r, 100, 500)
	if cursorTime, cursorID, ok := decodeCursor(r.URL.Query().Get("cursor")); ok {
		query = query.Where("created_at > ? OR (created_at = ? AND id > ?)", cursorTime, cursorTime, cursorID)
	}
	if err := query.Order("created_at ASC, id ASC").Limit(limit + 1).Find(&items).Error; err != nil {
		writeError(w, 500, "projects_unavailable", "无法读取项目列表")
		return
	}
	items, next := pageProjects(items, limit)
	writeCursorList(w, items, next, total, nil)
}

func normalizeProjectAvatar(name, avatar string) string {
	value := []rune(strings.TrimSpace(avatar))
	if len(value) == 0 {
		value = []rune(strings.TrimSpace(name))
		if len(value) > 1 {
			value = value[:1]
		}
		return string(value)
	}
	if len(value) > 2 {
		value = value[:2]
	}
	return string(value)
}

func validProjectColor(value string) bool {
	if len(value) != 7 || value[0] != '#' {
		return false
	}
	for _, character := range value[1:] {
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f') || (character >= 'A' && character <= 'F')) {
			return false
		}
	}
	return true
}

func (s *Server) createProject(w http.ResponseWriter, r *http.Request) {
	var item domain.Project
	if !decode(w, r, &item) {
		return
	}
	item.Name = strings.TrimSpace(item.Name)
	item.Description = strings.TrimSpace(item.Description)
	if item.Name == "" {
		writeError(w, 422, "invalid_project", "项目名称不能为空")
		return
	}
	if len([]rune(item.Name)) > 80 {
		writeError(w, 422, "invalid_project", "项目名称不能超过 80 个字符")
		return
	}
	item.Avatar = normalizeProjectAvatar(item.Name, item.Avatar)
	if item.Status == "" {
		item.Status = "healthy"
	}
	if item.Color == "" {
		item.Color = "#1d7a4c"
	}
	if !validProjectColor(item.Color) {
		writeError(w, 422, "invalid_project_color", "项目颜色必须使用 #RRGGBB 格式")
		return
	}
	if err := s.store.DB.Create(&item).Error; err != nil {
		writeError(w, 500, "create_failed", err.Error())
		return
	}
	s.audit(r, "project.create", "project", item.ID, "创建项目 "+item.Name, nil)
	writeData(w, 201, item)
}

func (s *Server) updateProject(w http.ResponseWriter, r *http.Request) {
	var item domain.Project
	if err := s.store.DB.First(&item, "id = ?", chi.URLParam(r, "id")).Error; err != nil {
		writeError(w, http.StatusNotFound, "project_not_found", "项目不存在")
		return
	}
	var input struct {
		Name        string `json:"name"`
		Avatar      string `json:"avatar"`
		Description string `json:"description"`
		Status      string `json:"status"`
		Color       string `json:"color"`
	}
	if !decode(w, r, &input) {
		return
	}
	if strings.TrimSpace(input.Name) == "" {
		writeError(w, http.StatusUnprocessableEntity, "invalid_project", "项目名称不能为空")
		return
	}
	if len([]rune(strings.TrimSpace(input.Name))) > 80 {
		writeError(w, http.StatusUnprocessableEntity, "invalid_project", "项目名称不能超过 80 个字符")
		return
	}
	item.Name, item.Description = strings.TrimSpace(input.Name), strings.TrimSpace(input.Description)
	item.Avatar = normalizeProjectAvatar(item.Name, input.Avatar)
	if input.Status != "" {
		item.Status = input.Status
	}
	if input.Color != "" && !validProjectColor(input.Color) {
		writeError(w, http.StatusUnprocessableEntity, "invalid_project_color", "项目颜色必须使用 #RRGGBB 格式")
		return
	}
	if input.Color != "" {
		item.Color = input.Color
	}
	if err := s.store.DB.Save(&item).Error; err != nil {
		writeError(w, http.StatusInternalServerError, "update_failed", "项目更新失败")
		return
	}
	s.audit(r, "project.update", "project", item.ID, "更新项目 "+item.Name, nil)
	writeData(w, http.StatusOK, item)
}

func (s *Server) deleteProject(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if !s.ensureNoActiveResourceJobs(w, "project", id) {
		return
	}
	for name, model := range map[string]any{"线路规则": &domain.Route{}, "SaaS 区域": &domain.SaaSZone{}, "候选源": &domain.CandidateSource{}} {
		var count int64
		_ = s.store.DB.Model(model).Where("project_id = ?", id).Count(&count).Error
		if count > 0 {
			writeError(w, http.StatusConflict, "project_in_use", "项目仍包含"+name+"，请先清理关联资源")
			return
		}
	}
	result := s.store.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("project_id = ?", id).Delete(&domain.ProjectSite{}).Error; err != nil {
			return err
		}
		return tx.Delete(&domain.Project{}, "id = ?", id).Error
	})
	if result != nil {
		writeError(w, http.StatusInternalServerError, "delete_failed", "项目删除失败")
		return
	}
	s.audit(r, "project.delete", "project", id, "删除项目", nil)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listProjectSites(w http.ResponseWriter, r *http.Request) {
	var items []domain.ProjectSite
	if err := s.store.DB.Where("project_id = ?", chi.URLParam(r, "id")).Order("created_at asc").Find(&items).Error; err != nil {
		writeError(w, http.StatusInternalServerError, "sites_unavailable", "站点读取失败")
		return
	}
	writeList(w, items, len(items))
}

func (s *Server) createProjectSite(w http.ResponseWriter, r *http.Request) {
	var project domain.Project
	if err := s.store.DB.First(&project, "id = ?", chi.URLParam(r, "id")).Error; err != nil {
		writeError(w, http.StatusNotFound, "project_not_found", "项目不存在")
		return
	}
	var item domain.ProjectSite
	if !decode(w, r, &item) {
		return
	}
	if err := validateProjectSite(&item); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid_site", err.Error())
		return
	}
	item.ProjectID, item.Status = project.ID, "active"
	if err := s.validateProjectSiteReferences(item); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid_site", err.Error())
		return
	}
	if err := s.store.DB.Create(&item).Error; err != nil {
		writeError(w, http.StatusInternalServerError, "create_failed", "站点创建失败")
		return
	}
	s.audit(r, "site.create", "project_site", item.ID, "创建站点 "+item.Name, map[string]any{"projectId": project.ID})
	writeData(w, http.StatusCreated, item)
}

func (s *Server) updateProjectSite(w http.ResponseWriter, r *http.Request) {
	var item domain.ProjectSite
	if err := s.store.DB.First(&item, "id = ? AND project_id = ?", chi.URLParam(r, "siteId"), chi.URLParam(r, "id")).Error; err != nil {
		writeError(w, http.StatusNotFound, "site_not_found", "站点不存在")
		return
	}
	var input domain.ProjectSite
	if !decode(w, r, &input) {
		return
	}
	input.ProjectID, input.ID = item.ProjectID, item.ID
	if err := validateProjectSite(&input); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid_site", err.Error())
		return
	}
	if err := s.validateProjectSiteReferences(input); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid_site", err.Error())
		return
	}
	item.Name, item.URL, item.ProbeProfileID, item.ProbeOverridesJSON = input.Name, input.URL, input.ProbeProfileID, input.ProbeOverridesJSON
	if input.Status != "" {
		item.Status = input.Status
	}
	if err := s.store.DB.Save(&item).Error; err != nil {
		writeError(w, http.StatusInternalServerError, "update_failed", "站点更新失败")
		return
	}
	s.audit(r, "site.update", "project_site", item.ID, "更新站点 "+item.Name, nil)
	writeData(w, http.StatusOK, item)
}

func (s *Server) deleteProjectSite(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "siteId")
	var count int64
	_ = s.store.DB.Model(&domain.Route{}).Where("site_id = ?", id).Count(&count).Error
	if count > 0 {
		writeError(w, http.StatusConflict, "site_in_use", "站点仍被线路规则引用")
		return
	}
	result := s.store.DB.Delete(&domain.ProjectSite{}, "id = ? AND project_id = ?", id, chi.URLParam(r, "id"))
	if result.Error != nil {
		writeError(w, http.StatusInternalServerError, "delete_failed", "站点删除失败")
		return
	}
	if result.RowsAffected == 0 {
		writeError(w, http.StatusNotFound, "site_not_found", "站点不存在")
		return
	}
	s.audit(r, "site.delete", "project_site", id, "删除站点", nil)
	w.WriteHeader(http.StatusNoContent)
}

func validateProjectSite(item *domain.ProjectSite) error {
	item.Name, item.URL = strings.TrimSpace(item.Name), strings.TrimSpace(item.URL)
	if item.Name == "" {
		return fmt.Errorf("站点名称不能为空")
	}
	parsed, err := url.Parse(item.URL)
	if err != nil || parsed.Hostname() == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return fmt.Errorf("站点 URL 必须是完整的 HTTP 或 HTTPS 地址")
	}
	return validateProbeOverrides(item.ProbeOverridesJSON)
}

func (s *Server) validateProjectSiteReferences(item domain.ProjectSite) error {
	if item.ProbeProfileID == "" {
		return nil
	}
	var count int64
	if err := s.store.DB.Model(&domain.ProbeProfile{}).Where("id = ? AND (project_id = ? OR project_id = '')", item.ProbeProfileID, item.ProjectID).Count(&count).Error; err != nil || count != 1 {
		return fmt.Errorf("所选探测模板不存在或不属于当前项目")
	}
	return nil
}

func (s *Server) listConnections(w http.ResponseWriter, r *http.Request) {
	var items []domain.Connection
	query := s.store.DB.Model(&domain.Connection{})
	total, limit := countAndLimit(query, r, 100, 500)
	if cursorTime, cursorID, ok := decodeCursor(r.URL.Query().Get("cursor")); ok {
		query = query.Where("created_at > ? OR (created_at = ? AND id > ?)", cursorTime, cursorTime, cursorID)
	}
	if err := query.Order("created_at ASC, id ASC").Limit(limit + 1).Find(&items).Error; err != nil {
		writeError(w, 500, "connections_unavailable", "无法读取连接列表")
		return
	}
	items, next := pageConnections(items, limit)
	writeCursorList(w, items, next, total, nil)
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
	input.Name = strings.TrimSpace(input.Name)
	if input.Name == "" {
		writeError(w, 422, "name_required", "连接名称不能为空")
		return
	}
	capability, validationErr := adapter.Validate(r.Context(), input.Credentials)
	if validationErr != nil {
		writeError(w, 422, "validation_failed", validationErr.Error())
		return
	}
	raw, _ := json.Marshal(input.Credentials)
	encrypted, err := s.vault.Encrypt(string(raw))
	if err != nil {
		writeError(w, 500, "encryption_failed", err.Error())
		return
	}
	capabilityJSON, _ := json.Marshal(capability)
	now := time.Now().UTC()
	desired := mustJSON(map[string]any{"provider": adapter.Name(), "purposes": input.Purposes})
	item := domain.Connection{Name: input.Name, Provider: adapter.Name(), Purposes: input.Purposes, CredentialEnc: encrypted, CredentialMask: connectionCredentialMask(input.Credentials), Capabilities: string(capabilityJSON), Status: "connected", LastValidatedAt: &now, DesiredJSON: desired, ObservedJSON: string(capabilityJSON), ConfigFingerprint: hashJSON(desired), LastSyncedAt: &now, DriftStatus: "in_sync"}
	if err := s.store.DB.Create(&item).Error; err != nil {
		writeError(w, 500, "create_failed", err.Error())
		return
	}
	s.audit(r, "connection.create", "connection", item.ID, "添加连接 "+item.Name, map[string]any{"provider": item.Provider})
	writeData(w, 201, item)
}
func (s *Server) updateConnection(w http.ResponseWriter, r *http.Request) {
	var item domain.Connection
	if err := s.store.DB.First(&item, "id = ?", chi.URLParam(r, "id")).Error; err != nil {
		writeError(w, http.StatusNotFound, "connection_not_found", "厂商连接不存在")
		return
	}
	var input struct {
		Name        string               `json:"name"`
		Purposes    string               `json:"purposes"`
		Credentials provider.Credentials `json:"credentials"`
	}
	if !decode(w, r, &input) {
		return
	}
	input.Name = strings.TrimSpace(input.Name)
	if input.Name == "" {
		writeError(w, http.StatusUnprocessableEntity, "name_required", "连接名称不能为空")
		return
	}
	adapter, ok := s.providers.Get(item.Provider)
	if !ok {
		writeError(w, http.StatusUnprocessableEntity, "unknown_provider", "不支持该厂商")
		return
	}
	credentialsRotated := hasConnectionCredentials(input.Credentials)
	if credentialsRotated {
		capability, err := adapter.Validate(r.Context(), input.Credentials)
		if err != nil {
			writeError(w, http.StatusUnprocessableEntity, "validation_failed", err.Error())
			return
		}
		raw, _ := json.Marshal(input.Credentials)
		encrypted, err := s.vault.Encrypt(string(raw))
		if err != nil {
			writeError(w, http.StatusInternalServerError, "encryption_failed", err.Error())
			return
		}
		now := time.Now().UTC()
		capabilityJSON := mustJSON(capability)
		item.CredentialEnc = encrypted
		item.CredentialMask = connectionCredentialMask(input.Credentials)
		item.Capabilities = capabilityJSON
		item.Status = "connected"
		item.StatusMessage = ""
		item.LastValidatedAt = &now
		item.ObservedJSON = capabilityJSON
		item.LastSyncedAt = &now
		item.DriftStatus = "in_sync"
	}
	item.Name = input.Name
	if strings.TrimSpace(input.Purposes) != "" {
		item.Purposes = strings.TrimSpace(input.Purposes)
	}
	item.DesiredJSON = mustJSON(map[string]any{"provider": item.Provider, "purposes": item.Purposes})
	item.ConfigFingerprint = hashJSON(item.DesiredJSON)
	if err := s.store.DB.Save(&item).Error; err != nil {
		writeError(w, http.StatusInternalServerError, "update_failed", "厂商连接更新失败")
		return
	}
	s.audit(r, "connection.update", "connection", item.ID, "更新连接 "+item.Name, map[string]any{"provider": item.Provider, "credentialsRotated": credentialsRotated})
	writeData(w, http.StatusOK, item)
}
func (s *Server) deleteConnection(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if !s.ensureNoActiveResourceJobs(w, "connection", id) {
		return
	}
	var item domain.Connection
	if err := s.store.DB.First(&item, "id = ?", id).Error; err != nil {
		writeError(w, http.StatusNotFound, "connection_not_found", "厂商连接不存在")
		return
	}
	dependencies := []struct {
		key    string
		label  string
		model  any
		column string
	}{
		{key: "saasZones", label: "SaaS 区域", model: &domain.SaaSZone{}, column: "connection_id"},
		{key: "customHostnames", label: "SaaS 主机名 DNS 自动化", model: &domain.CustomHostname{}, column: "dns_connection_id"},
		{key: "optimizedHostnames", label: "优选域名", model: &domain.OptimizedHostname{}, column: "dns_connection_id"},
		{key: "routes", label: "CNAME 线路规则", model: &domain.Route{}, column: "dns_connection_id"},
		{key: "managedDNSRecords", label: "受管 DNS 记录", model: &domain.ManagedDNSRecord{}, column: "connection_id"},
	}
	counts := make(map[string]int64, len(dependencies))
	labels := make([]string, 0, len(dependencies))
	for _, dependency := range dependencies {
		var count int64
		if err := s.store.DB.Model(dependency.model).Where(dependency.column+" = ?", id).Count(&count).Error; err != nil {
			writeError(w, http.StatusInternalServerError, "connection_dependency_check_failed", "无法检查连接的关联资源")
			return
		}
		if count > 0 {
			counts[dependency.key] = count
			labels = append(labels, fmt.Sprintf("%s %d 项", dependency.label, count))
		}
	}
	if len(labels) > 0 {
		writeErrorWithData(w, http.StatusConflict, "connection_in_use", "连接仍被以下资源使用："+strings.Join(labels, "、")+"。请先迁移或清理关联资源", counts)
		return
	}
	result := s.store.DB.Delete(&domain.Connection{}, "id = ?", id)
	if result.Error != nil {
		writeError(w, http.StatusInternalServerError, "delete_failed", "厂商连接删除失败")
		return
	}
	if result.RowsAffected == 0 {
		writeError(w, http.StatusNotFound, "connection_not_found", "厂商连接不存在")
		return
	}
	s.audit(r, "connection.delete", "connection", id, "删除连接 "+item.Name, map[string]any{"provider": item.Provider})
	w.WriteHeader(http.StatusNoContent)
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

func hasConnectionCredentials(credentials provider.Credentials) bool {
	for _, value := range credentials {
		if strings.TrimSpace(value) != "" {
			return true
		}
	}
	return false
}

func connectionCredentialMask(credentials provider.Credentials) string {
	for _, value := range credentials {
		if strings.TrimSpace(value) != "" {
			return security.MaskSecret(value)
		}
	}
	return "••••••••"
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

func (s *Server) listSaaSZones(w http.ResponseWriter, r *http.Request) {
	var items []domain.SaaSZone
	query := s.store.DB.Model(&domain.SaaSZone{})
	total, limit := countAndLimit(query, r, 100, 500)
	if cursorTime, cursorID, ok := decodeCursor(r.URL.Query().Get("cursor")); ok {
		query = query.Where("created_at > ? OR (created_at = ? AND id > ?)", cursorTime, cursorTime, cursorID)
	}
	if err := query.Order("created_at ASC, id ASC").Limit(limit + 1).Find(&items).Error; err != nil {
		writeError(w, 500, "saas_zones_unavailable", "无法读取 SaaS 区域")
		return
	}
	items, next := pageSaaSZones(items, limit)
	writeCursorList(w, items, next, total, nil)
}
func (s *Server) createSaaSZone(w http.ResponseWriter, r *http.Request) {
	var item domain.SaaSZone
	if !decode(w, r, &item) {
		return
	}
	item.ZoneName = normalizeDNSName(item.ZoneName)
	item.FallbackOrigin = normalizeDNSName(item.FallbackOrigin)
	item.CNAMETarget = normalizeDNSName(item.CNAMETarget)
	if item.ProjectID == "" || item.ConnectionID == "" || item.ZoneID == "" || item.ZoneName == "" || item.CNAMETarget == "" {
		writeError(w, 422, "saas_zone_invalid", "项目、Cloudflare 连接、区域 ID、区域名称和 CNAME 目标均为必填项")
		return
	}
	var connection domain.Connection
	if err := s.store.DB.First(&connection, "id = ?", item.ConnectionID).Error; err != nil || connection.Provider != "cloudflare" {
		writeError(w, 422, "cloudflare_connection_required", "请选择有效的 Cloudflare 连接")
		return
	}
	if item.FallbackOrigin != "" && (net.ParseIP(item.FallbackOrigin) != nil || strings.Contains(item.FallbackOrigin, "*")) {
		writeError(w, 422, "fallback_origin_invalid", "回退源必须是具体主机名，不能使用 IP 或通配符")
		return
	}
	item.Status, item.FallbackStatus, item.DriftStatus = "checking", "checking", "unknown"
	item.DesiredJSON = mustJSON(map[string]any{"fallbackOrigin": item.FallbackOrigin, "cnameTarget": item.CNAMETarget})
	item.ConfigFingerprint = hashJSON(item.DesiredJSON)
	if err := s.store.DB.Create(&item).Error; err != nil {
		writeError(w, 500, "create_failed", err.Error())
		return
	}
	s.audit(r, "saas_zone.create", "saas_zone", item.ID, "添加 SaaS 区域 "+item.ZoneName, nil)
	admin, _ := auth.AdminFromContext(r.Context())
	s.enqueueHTTPJob(w, r, "saas.zone.sync", "saas_zone", item.ID, jobPayload{Actor: admin.Username, IPAddress: r.RemoteAddr})
}
func (s *Server) syncSaaSZone(w http.ResponseWriter, r *http.Request) {
	var item domain.SaaSZone
	if err := s.store.DB.First(&item, "id = ?", chi.URLParam(r, "id")).Error; err != nil {
		writeError(w, 404, "zone_not_found", "SaaS 区域不存在")
		return
	}
	admin, _ := auth.AdminFromContext(r.Context())
	s.enqueueHTTPJob(w, r, "saas.zone.sync", "saas_zone", item.ID, jobPayload{Actor: admin.Username, IPAddress: r.RemoteAddr})
}
func (s *Server) listCustomHostnames(w http.ResponseWriter, r *http.Request) {
	var items []domain.CustomHostname
	query := s.store.DB.Model(&domain.CustomHostname{})
	if zone := r.URL.Query().Get("saasZoneId"); zone != "" {
		query = query.Where("saas_zone_id = ?", zone)
	}
	total, limit := countAndLimit(query, r, 100, 500)
	if cursorTime, cursorID, ok := decodeCursor(r.URL.Query().Get("cursor")); ok {
		query = query.Where("created_at < ? OR (created_at = ? AND id < ?)", cursorTime, cursorTime, cursorID)
	}
	if err := query.Order("created_at DESC, id DESC").Limit(limit + 1).Find(&items).Error; err != nil {
		writeError(w, 500, "hostnames_unavailable", "无法读取 SaaS 主机名")
		return
	}
	items, next := pageCustomHostnames(items, limit)
	writeCursorList(w, items, next, total, nil)
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
	if item.ValidationMethod != "txt" && item.ValidationMethod != "http" {
		writeError(w, 422, "validation_method_invalid", "验证方式必须是 txt 或 http")
		return
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
	item.Status, item.OwnershipStatus, item.SSLStatus, item.CutoverStatus, item.DriftStatus = "pending", "pending", "pending_validation", "waiting_validation", "unknown"
	if item.CutoverApprovalRequired {
		item.CutoverStatus = "prevalidating"
	}
	var zone domain.SaaSZone
	if err := s.store.DB.First(&zone, "id = ?", item.SaaSZoneID).Error; err != nil {
		writeError(w, 404, "zone_not_found", "SaaS 区域不存在")
		return
	}
	var existing int64
	_ = s.store.DB.Model(&domain.CustomHostname{}).Where("saas_zone_id = ? AND hostname = ?", item.SaaSZoneID, item.Hostname).Count(&existing).Error
	if existing > 0 {
		writeError(w, 409, "hostname_exists", "该自定义主机名已存在")
		return
	}
	item.DesiredJSON = mustJSON(map[string]any{"hostname": item.Hostname, "validationMethod": item.ValidationMethod, "customOrigin": item.CustomOrigin, "automationMode": item.AutomationMode, "cutoverApprovalRequired": item.CutoverApprovalRequired})
	item.ConfigFingerprint = hashJSON(item.DesiredJSON)
	if err := s.store.DB.Create(&item).Error; err != nil {
		writeError(w, 500, "create_failed", err.Error())
		return
	}
	s.audit(r, "custom_hostname.create", "custom_hostname", item.ID, "添加自定义主机名 "+item.Hostname, map[string]any{"automationMode": item.AutomationMode})
	admin, _ := auth.AdminFromContext(r.Context())
	s.enqueueHTTPJob(w, r, "saas.hostname.create", "custom_hostname", item.ID, jobPayload{Actor: admin.Username, IPAddress: r.RemoteAddr})
}

func (s *Server) bulkCreateCustomHostnames(w http.ResponseWriter, r *http.Request) {
	var input struct {
		SaaSZoneID, DNSConnectionID, DNSZoneID, DNSZoneName string
		ValidationMethod, AutomationMode, CustomOrigin      string
		Hostnames                                           []string
	}
	if !decode(w, r, &input) {
		return
	}
	if len(input.Hostnames) == 0 || len(input.Hostnames) > 500 {
		writeError(w, 422, "bulk_size_invalid", "每批必须包含 1 到 500 个主机名")
		return
	}
	if input.ValidationMethod == "" {
		input.ValidationMethod = "txt"
	}
	if input.AutomationMode == "" {
		input.AutomationMode = "manual"
	}
	if input.ValidationMethod != "txt" && input.ValidationMethod != "http" {
		writeError(w, 422, "validation_method_invalid", "验证方式必须是 txt 或 http")
		return
	}
	if input.AutomationMode != "manual" && input.AutomationMode != "automatic" {
		writeError(w, 422, "automation_mode_invalid", "DNS 自动化方式必须是 manual 或 automatic")
		return
	}
	var zone domain.SaaSZone
	if err := s.store.DB.First(&zone, "id = ?", input.SaaSZoneID).Error; err != nil {
		writeError(w, 404, "zone_not_found", "SaaS 区域不存在")
		return
	}
	if input.AutomationMode == "automatic" {
		if input.DNSConnectionID == "" || input.DNSZoneID == "" || strings.TrimSpace(input.DNSZoneName) == "" {
			writeError(w, http.StatusUnprocessableEntity, "dns_target_required", "自动 DNS 需要选择连接并填写区域 ID 与区域名称")
			return
		}
		var connection domain.Connection
		if err := s.store.DB.First(&connection, "id = ?", input.DNSConnectionID).Error; err != nil {
			writeError(w, http.StatusNotFound, "dns_connection_not_found", "DNS 连接不存在")
			return
		}
		if _, ok := s.providers.Get(connection.Provider); !ok {
			writeError(w, http.StatusUnprocessableEntity, "provider_unknown", "DNS 连接没有可用适配器")
			return
		}
	}
	normalized := make([]string, 0, len(input.Hostnames))
	invalid := make([]string, 0)
	seen := make(map[string]bool)
	for _, rawHostname := range input.Hostnames {
		hostname := normalizeDNSName(rawHostname)
		if hostname == "" || net.ParseIP(hostname) != nil || strings.Contains(hostname, "*") {
			invalid = append(invalid, rawHostname)
			continue
		}
		if seen[hostname] {
			invalid = append(invalid, rawHostname)
			continue
		}
		seen[hostname] = true
		normalized = append(normalized, hostname)
	}
	if len(invalid) > 0 {
		writeErrorWithData(w, http.StatusUnprocessableEntity, "hostname_validation_failed", "批量主机名包含无效或重复项；未创建任何资源", map[string]any{"invalid": invalid})
		return
	}
	var existing []string
	if err := s.store.DB.Model(&domain.CustomHostname{}).Where("saas_zone_id = ? AND hostname IN ?", input.SaaSZoneID, normalized).Pluck("hostname", &existing).Error; err != nil {
		writeError(w, http.StatusInternalServerError, "hostname_lookup_failed", "无法检查已有主机名")
		return
	}
	if len(existing) > 0 {
		writeErrorWithData(w, http.StatusConflict, "hostname_exists", "批量主机名包含已存在对象；未创建任何资源", map[string]any{"existing": existing})
		return
	}
	admin, _ := auth.AdminFromContext(r.Context())
	created := make([]domain.CustomHostname, 0, len(normalized))
	queued := make([]domain.Job, 0, len(normalized))
	baseKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	for _, hostname := range normalized {
		item := domain.CustomHostname{SaaSZoneID: input.SaaSZoneID, DNSConnectionID: input.DNSConnectionID, DNSZoneID: input.DNSZoneID, DNSZoneName: normalizeDNSName(input.DNSZoneName), Hostname: hostname, ValidationMethod: input.ValidationMethod, AutomationMode: input.AutomationMode, CustomOrigin: normalizeDNSName(input.CustomOrigin), Status: "pending", OwnershipStatus: "pending", SSLStatus: "pending_validation", CutoverStatus: "waiting_validation", DriftStatus: "unknown"}
		item.DesiredJSON = mustJSON(map[string]any{"hostname": hostname, "validationMethod": item.ValidationMethod, "customOrigin": item.CustomOrigin, "automationMode": item.AutomationMode})
		item.ConfigFingerprint = hashJSON(item.DesiredJSON)
		if err := s.store.DB.Create(&item).Error; err != nil {
			continue
		}
		idempotency := ""
		if baseKey != "" {
			idempotency = baseKey + ":" + hostname
		}
		job, _, err := s.jobs.Enqueue(jobs.EnqueueOptions{Type: "saas.hostname.create", ResourceType: "custom_hostname", ResourceID: item.ID, Payload: jobPayload{Actor: admin.Username, IPAddress: r.RemoteAddr}, IdempotencyKey: idempotency, MaxAttempts: 5})
		if err != nil {
			item.ErrorMessage = "作业创建失败: " + err.Error()
			_ = s.store.DB.Save(&item).Error
			continue
		}
		created, queued = append(created, item), append(queued, job)
	}
	s.audit(r, "custom_hostname.bulk_create", "saas_zone", zone.ID, "批量添加自定义主机名", map[string]any{"requested": len(input.Hostnames), "created": len(created)})
	writeData(w, http.StatusAccepted, map[string]any{"created": created, "jobs": queued})
}

func (s *Server) syncCustomHostname(w http.ResponseWriter, r *http.Request) {
	var item domain.CustomHostname
	if err := s.store.DB.First(&item, "id = ?", chi.URLParam(r, "id")).Error; err != nil {
		writeError(w, 404, "hostname_not_found", "自定义主机名不存在")
		return
	}
	admin, _ := auth.AdminFromContext(r.Context())
	s.enqueueHTTPJob(w, r, "saas.hostname.sync", "custom_hostname", item.ID, jobPayload{Actor: admin.Username, IPAddress: r.RemoteAddr})
}

func (s *Server) resumeCustomHostnameAutomation(w http.ResponseWriter, r *http.Request) {
	var item domain.CustomHostname
	if err := s.store.DB.First(&item, "id = ?", chi.URLParam(r, "id")).Error; err != nil {
		writeError(w, http.StatusNotFound, "hostname_not_found", "自定义主机名不存在")
		return
	}
	if item.AutomationMode != "automatic" {
		writeError(w, http.StatusConflict, "automation_not_enabled", "该主机名未启用自动 DNS")
		return
	}
	item.DNSAutomationPaused, item.DNSAutomationPauseReason, item.ErrorMessage = false, "", ""
	if err := s.store.DB.Save(&item).Error; err != nil {
		writeError(w, http.StatusInternalServerError, "automation_resume_failed", "无法恢复 DNS 自动化")
		return
	}
	s.audit(r, "custom_hostname.automation.resume", "custom_hostname", item.ID, "恢复 SaaS 主机名 DNS 自动化 "+item.Hostname, nil)
	admin, _ := auth.AdminFromContext(r.Context())
	s.enqueueHTTPJob(w, r, "saas.hostname.sync", "custom_hostname", item.ID, jobPayload{Actor: admin.Username, IPAddress: r.RemoteAddr})
}

func (s *Server) approveCustomHostnameCutover(w http.ResponseWriter, r *http.Request) {
	var item domain.CustomHostname
	if err := s.store.DB.First(&item, "id = ?", chi.URLParam(r, "id")).Error; err != nil {
		writeError(w, http.StatusNotFound, "hostname_not_found", "自定义主机名不存在")
		return
	}
	if item.Status != "active" || item.SSLStatus != "active" {
		writeError(w, http.StatusConflict, "hostname_not_ready", "主机名状态和证书状态必须同时为 active 才能切流")
		return
	}
	if !item.CutoverApprovalRequired {
		writeError(w, http.StatusConflict, "cutover_not_required", "该主机名未启用人工批准切流")
		return
	}
	item.CutoverStatus = "approved"
	if err := s.store.DB.Save(&item).Error; err != nil {
		writeError(w, http.StatusInternalServerError, "cutover_approval_failed", "切流批准状态保存失败")
		return
	}
	s.audit(r, "custom_hostname.cutover.approve", "custom_hostname", item.ID, "批准 SaaS 主机名切流 "+item.Hostname, nil)
	admin, _ := auth.AdminFromContext(r.Context())
	s.enqueueHTTPJob(w, r, "saas.hostname.sync", "custom_hostname", item.ID, jobPayload{Actor: admin.Username, IPAddress: r.RemoteAddr})
}

func (s *Server) bulkDeleteCustomHostnames(w http.ResponseWriter, r *http.Request) {
	var input struct {
		IDs []string `json:"ids"`
	}
	if !decode(w, r, &input) {
		return
	}
	if len(input.IDs) == 0 || len(input.IDs) > 500 {
		writeError(w, http.StatusUnprocessableEntity, "bulk_size_invalid", "每批必须选择 1 到 500 个主机名")
		return
	}
	admin, _ := auth.AdminFromContext(r.Context())
	baseKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	queued := make([]domain.Job, 0, len(input.IDs))
	skipped := make([]string, 0)
	seen := make(map[string]bool)
	for _, id := range input.IDs {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		var item domain.CustomHostname
		if err := s.store.DB.First(&item, "id = ?", id).Error; err != nil {
			skipped = append(skipped, id)
			continue
		}
		idempotency := ""
		if baseKey != "" {
			idempotency = baseKey + ":" + id
		}
		job, _, err := s.jobs.Enqueue(jobs.EnqueueOptions{Type: "saas.hostname.delete", ResourceType: "custom_hostname", ResourceID: id, Payload: jobPayload{Actor: admin.Username, IPAddress: r.RemoteAddr}, IdempotencyKey: idempotency, MaxAttempts: 5})
		if err != nil {
			skipped = append(skipped, id)
			continue
		}
		queued = append(queued, job)
	}
	s.audit(r, "custom_hostname.bulk_delete", "custom_hostname", "bulk", "批量清理自定义主机名", map[string]any{"requested": len(input.IDs), "queued": len(queued), "skipped": len(skipped)})
	writeData(w, http.StatusAccepted, map[string]any{"jobs": queued, "skipped": skipped})
}

func (s *Server) deleteCustomHostname(w http.ResponseWriter, r *http.Request) {
	var item domain.CustomHostname
	if err := s.store.DB.First(&item, "id = ?", chi.URLParam(r, "id")).Error; err != nil {
		writeError(w, 404, "hostname_not_found", "自定义主机名不存在")
		return
	}
	admin, _ := auth.AdminFromContext(r.Context())
	s.enqueueHTTPJob(w, r, "saas.hostname.delete", "custom_hostname", item.ID, jobPayload{Actor: admin.Username, IPAddress: r.RemoteAddr})
}

func applyCloudflareCustomHostnameResult(item *domain.CustomHostname, result map[string]any) {
	if value := stringAny(result["id"]); value != "" {
		item.ProviderID = value
	}
	if value := stringAny(result["status"]); value != "" {
		item.Status = value
		item.OwnershipStatus = value
	}
	if ownership, ok := result["ownership_verification"].(map[string]any); ok {
		item.OwnershipValidationJSON = mustJSON(ownership)
		if value := stringAny(ownership["name"]); value != "" {
			item.ValidationName = value
		}
		if value := stringAny(ownership["value"]); value != "" {
			item.ValidationValue = value
		}
	}
	if ownershipHTTP, ok := result["ownership_verification_http"].(map[string]any); ok {
		item.OwnershipValidationJSON = mustJSON(map[string]any{"dns": map[string]any{"status": item.OwnershipStatus}, "http": ownershipHTTP})
	}
	if sslValue, ok := result["ssl"].(map[string]any); ok {
		item.SSLValidationJSON = mustJSON(sslValue["validation_records"])
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
	desiredRecords := make([]provider.Record, 0)
	if item.ValidationMethod == "txt" {
		validationRecords := extractValidationRecords(item.OwnershipValidationJSON)
		if len(validationRecords) == 0 && item.ValidationName != "" && item.ValidationValue != "" {
			validationRecords = append(validationRecords, validationRecord{Name: item.ValidationName, Value: item.ValidationValue})
		}
		if len(validationRecords) == 0 {
			return fmt.Errorf("Cloudflare 尚未返回所有权 TXT 验证记录")
		}
		for _, value := range validationRecords {
			record := provider.Record{Name: value.Name, Type: "TXT", Value: value.Value, LineCode: lineCode, TTL: 60, Weight: 100, Enabled: true}
			observed, changed, err := ensureSaaSDNSRecord(ctx, adapter, credentials, dnsZone, record, false)
			if err != nil {
				return fmt.Errorf("写入所有权 TXT 验证记录失败: %w", err)
			}
			if changed {
				if err := s.trackSaaSManagedDNSRecord(item, dnsZone, observed); err != nil {
					return err
				}
			}
			desiredRecords = append(desiredRecords, record)
		}
	}
	for _, value := range extractValidationRecords(item.SSLValidationJSON) {
		record := provider.Record{Name: value.Name, Type: "TXT", Value: value.Value, LineCode: lineCode, TTL: 60, Weight: 100, Enabled: true}
		observed, changed, err := ensureSaaSDNSRecord(ctx, adapter, credentials, dnsZone, record, false)
		if err != nil {
			return fmt.Errorf("写入证书 TXT 验证记录失败: %w", err)
		}
		if changed {
			if err := s.trackSaaSManagedDNSRecord(item, dnsZone, observed); err != nil {
				return err
			}
		}
		desiredRecords = append(desiredRecords, record)
	}
	if item.Status != "active" || item.SSLStatus != "active" {
		if len(desiredRecords) == 0 {
			return nil
		}
		return s.verifyAndStoreSaaSDNS(ctx, adapter, credentials, dnsZone, item, desiredRecords)
	}
	if item.CutoverApprovalRequired && item.CutoverStatus != "approved" && item.CutoverStatus != "active" {
		item.CutoverStatus = "approval_required"
		if len(desiredRecords) == 0 {
			return nil
		}
		return s.verifyAndStoreSaaSDNS(ctx, adapter, credentials, dnsZone, item, desiredRecords)
	}
	if strings.TrimSpace(zone.CNAMETarget) == "" {
		return fmt.Errorf("SaaS 区域没有配置 CNAME 目标")
	}
	businessRecord := provider.Record{Name: item.Hostname, Type: "CNAME", Value: zone.CNAMETarget, LineCode: lineCode, TTL: 60, Weight: 100, Enabled: true}
	observed, changed, err := ensureSaaSDNSRecord(ctx, adapter, credentials, dnsZone, businessRecord, true)
	if err != nil {
		return fmt.Errorf("发布业务 CNAME 失败: %w", err)
	}
	if changed {
		if err := s.trackSaaSManagedDNSRecord(item, dnsZone, observed); err != nil {
			return err
		}
	}
	desiredRecords = append(desiredRecords, businessRecord)
	if err := s.verifyAndStoreSaaSDNS(ctx, adapter, credentials, dnsZone, item, desiredRecords); err != nil {
		return err
	}
	item.CutoverStatus = "active"
	return nil
}

func (s *Server) verifyAndStoreSaaSDNS(ctx context.Context, adapter provider.Adapter, credentials provider.Credentials, dnsZone provider.Zone, item *domain.CustomHostname, desiredRecords []provider.Record) error {
	observed := make([]provider.Record, 0)
	for _, desired := range desiredRecords {
		items, err := adapter.ListRecords(ctx, credentials, dnsZone, desired.Name)
		if err != nil {
			return err
		}
		observed = append(observed, items...)
	}
	if err := dnsverify.New().Verify(ctx, dnsZone.Name, desiredRecords); err != nil {
		return fmt.Errorf("权威 DNS 未确认验证记录或业务 CNAME: %w", err)
	}
	now := time.Now().UTC()
	for _, record := range observed {
		if findEquivalentRecord(desiredRecords, record) == nil {
			continue
		}
		updates := map[string]any{"observed_json": mustJSON(record), "last_verified_at": &now, "drift_status": "in_sync", "fingerprint": hashJSON(record)}
		if err := s.store.DB.Model(&domain.ManagedDNSRecord{}).Where("resource_type = ? AND resource_id = ? AND provider_record_id = ?", "custom_hostname", item.ID, record.ID).Updates(updates).Error; err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) trackSaaSManagedDNSRecord(item *domain.CustomHostname, zone provider.Zone, record provider.Record) error {
	now := time.Now().UTC()
	managed := domain.ManagedDNSRecord{ConnectionID: item.DNSConnectionID, ZoneID: zone.ID, ResourceType: "custom_hostname", ResourceID: item.ID, ProviderRecordID: record.ID}
	values := domain.ManagedDNSRecord{ConnectionID: item.DNSConnectionID, ZoneID: zone.ID, ResourceType: "custom_hostname", ResourceID: item.ID, ProviderRecordID: record.ID, Name: record.Name, Type: record.Type, Value: record.Value, LineCode: record.LineCode, TTL: record.TTL, Weight: record.Weight, Fingerprint: hashJSON(record), ObservedJSON: mustJSON(record), LastVerifiedAt: &now, DriftStatus: "in_sync"}
	return s.store.DB.Where("resource_type = ? AND resource_id = ? AND provider_record_id = ?", "custom_hostname", item.ID, record.ID).Assign(values).FirstOrCreate(&managed).Error
}

type validationRecord struct {
	Name  string
	Value string
}

func extractValidationRecords(raw string) []validationRecord {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var values []map[string]any
	if json.Unmarshal([]byte(raw), &values) != nil {
		var single map[string]any
		if json.Unmarshal([]byte(raw), &single) != nil {
			return nil
		}
		values = []map[string]any{single}
	}
	result := make([]validationRecord, 0, len(values))
	for _, item := range values {
		name := firstString(item, "name", "txt_name", "record_name")
		value := firstString(item, "value", "txt_value", "record_value")
		if name != "" && value != "" {
			result = append(result, validationRecord{Name: name, Value: value})
		}
	}
	return result
}

func firstString(values map[string]any, keys ...string) string {
	for _, key := range keys {
		if value := stringAny(values[key]); value != "" {
			return value
		}
	}
	return ""
}

func ensureSaaSDNSRecord(ctx context.Context, adapter provider.Adapter, credentials provider.Credentials, zone provider.Zone, desired provider.Record, exclusive bool) (provider.Record, bool, error) {
	existing, err := adapter.ListRecords(ctx, credentials, zone, desired.Name)
	if err != nil {
		return provider.Record{}, false, err
	}
	var updateBefore *provider.Record
	for index := range existing {
		record := existing[index]
		if normalizeDNSName(record.Name) != normalizeDNSName(desired.Name) {
			continue
		}
		if strings.EqualFold(record.Type, desired.Type) && normalizeDNSValue(record.Value, desired.Type) == normalizeDNSValue(desired.Value, desired.Type) {
			return record, false, nil
		}
		if exclusive && !strings.EqualFold(record.Type, "CNAME") {
			return provider.Record{}, false, fmt.Errorf("%s 已存在 %s 记录，不能与 CNAME 共存", desired.Name, record.Type)
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
		return provider.Record{}, false, err
	}
	observed, err := adapter.ListRecords(ctx, credentials, zone, desired.Name)
	if err != nil {
		return provider.Record{}, false, err
	}
	for _, record := range observed {
		if normalizeDNSName(record.Name) == normalizeDNSName(desired.Name) && strings.EqualFold(record.Type, desired.Type) && normalizeDNSValue(record.Value, desired.Type) == normalizeDNSValue(desired.Value, desired.Type) {
			return record, true, nil
		}
	}
	return provider.Record{}, false, fmt.Errorf("厂商 API 回读未确认记录")
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

func (s *Server) listSources(w http.ResponseWriter, r *http.Request) {
	var items []domain.CandidateSource
	query := s.store.DB.Model(&domain.CandidateSource{})
	total, limit := countAndLimit(query, r, 100, 500)
	if cursorTime, cursorID, ok := decodeCursor(r.URL.Query().Get("cursor")); ok {
		query = query.Where("created_at > ? OR (created_at = ? AND id > ?)", cursorTime, cursorTime, cursorID)
	}
	if err := query.Order("created_at ASC, id ASC").Limit(limit + 1).Find(&items).Error; err != nil {
		writeError(w, 500, "sources_unavailable", "无法读取候选源")
		return
	}
	items, next := pageCandidateSources(items, limit)
	writeCursorList(w, items, next, total, nil)
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
	var source domain.CandidateSource
	if err := s.store.DB.First(&source, "id = ?", chi.URLParam(r, "id")).Error; err != nil {
		writeError(w, 404, "source_not_found", "候选源不存在")
		return
	}
	admin, _ := auth.AdminFromContext(r.Context())
	s.enqueueHTTPJob(w, r, "optimizer.source.sync", "candidate_source", source.ID, jobPayload{Actor: admin.Username, IPAddress: r.RemoteAddr, Sample: parseIntDefault(r.URL.Query().Get("samplePerPrefix"), 32)})
}
func (s *Server) listCandidates(w http.ResponseWriter, r *http.Request) {
	limit := parseIntDefault(r.URL.Query().Get("limit"), 100)
	if limit < 1 {
		limit = 1
	}
	if limit > 500 {
		limit = 500
	}
	project := strings.TrimSpace(r.URL.Query().Get("projectId"))
	scope := strings.TrimSpace(r.URL.Query().Get("scope"))
	cursorScore, cursorAddress, cursorID, hasCursor := decodeCandidateCursor(r.URL.Query().Get("cursor"))
	if scope == "node_group" || scope == "line" {
		all, err := s.optimizer.ScopedRankings(project, scope, strings.TrimSpace(r.URL.Query().Get("scopeValue")), 0)
		if err != nil {
			writeError(w, http.StatusUnprocessableEntity, "ranking_scope_invalid", err.Error())
			return
		}
		items := make([]domain.Candidate, 0, limit+1)
		for _, candidate := range all {
			if hasCursor && !candidateFollowsCursor(candidate, cursorScore, cursorAddress, cursorID) {
				continue
			}
			items = append(items, candidate)
			if len(items) > limit {
				break
			}
		}
		items, next := pageCandidates(items, limit)
		writeCursorList(w, items, next, len(all), nil)
		return
	}
	var items []domain.Candidate
	query := s.store.DB.Model(&domain.Candidate{})
	if project != "" {
		query = query.Where("project_id = ?", project)
	}
	if tier := r.URL.Query().Get("tier"); tier != "" {
		query = query.Where("tier = ?", tier)
	}
	var total int64
	_ = query.Count(&total).Error
	if hasCursor {
		query = query.Where("score < ? OR (score = ? AND (address > ? OR (address = ? AND id > ?)))", cursorScore, cursorScore, cursorAddress, cursorAddress, cursorID)
	}
	if err := query.Order("score DESC, address ASC, id ASC").Limit(limit + 1).Find(&items).Error; err != nil {
		writeError(w, 500, "candidates_unavailable", "无法读取候选榜单")
		return
	}
	items, next := pageCandidates(items, limit)
	writeCursorList(w, items, next, int(total), nil)
}

func (s *Server) listRankingScopes(w http.ResponseWriter, r *http.Request) {
	type rankingScope struct {
		Kind  string `json:"kind"`
		Value string `json:"value"`
		Label string `json:"label"`
	}
	items := []rankingScope{{Kind: "global", Value: "", Label: "全局榜单"}}
	var groups []string
	_ = s.store.DB.Model(&domain.ProbeNode{}).Distinct("group_name").Where("group_name <> ?", "").Order("group_name").Pluck("group_name", &groups).Error
	for _, group := range groups {
		items = append(items, rankingScope{Kind: "node_group", Value: group, Label: "节点组 · " + group})
	}
	var lines []domain.RouteLine
	lineQuery := s.store.DB.Order("alias asc")
	if projectID := strings.TrimSpace(r.URL.Query().Get("projectId")); projectID != "" {
		lineQuery = lineQuery.Where("route_id IN (?)", s.store.DB.Model(&domain.Route{}).Select("id").Where("project_id = ?", projectID))
	}
	_ = lineQuery.Find(&lines).Error
	for _, line := range lines {
		items = append(items, rankingScope{Kind: "line", Value: line.ID, Label: "线路 · " + line.Alias})
	}
	writeList(w, items, len(items))
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
	Hostname           string                  `json:"hostname"`
	RecordType         string                  `json:"recordType"`
	Addresses          []string                `json:"addresses"`
	Changes            []provider.RecordChange `json:"changes"`
	Warnings           []string                `json:"warnings"`
	UnmanagedConflicts []provider.Record       `json:"unmanagedConflicts"`
	CanApply           bool                    `json:"canApply"`
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
	var item domain.OptimizedHostname
	if err := s.store.DB.First(&item, "id = ?", chi.URLParam(r, "id")).Error; err != nil {
		writeError(w, 404, "hostname_not_found", "优选域名不存在")
		return
	}
	admin, _ := auth.AdminFromContext(r.Context())
	s.enqueueHTTPJob(w, r, "optimizer.hostname.publish", "optimized_hostname", item.ID, jobPayload{Actor: admin.Username, IPAddress: r.RemoteAddr})
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
	owned := make(map[string]bool)
	var managed []domain.ManagedDNSRecord
	_ = s.store.DB.Where("resource_type = ? AND resource_id = ?", "optimized_hostname", item.ID).Find(&managed).Error
	for _, record := range managed {
		owned[record.ProviderRecordID] = true
	}
	var previous []provider.Record
	if json.Unmarshal([]byte(item.ObservedJSON), &previous) == nil {
		for _, record := range existing {
			if record.Managed || findEquivalentRecord(previous, record) != nil {
				owned[record.ID] = true
			}
		}
	}
	preview.Changes, preview.UnmanagedConflicts = reconcileManagedAddressRecords(existing, item.Hostname, recordType, item.TTL, preview.Addresses, owned)
	preview.CanApply = len(preview.UnmanagedConflicts) == 0
	if len(preview.UnmanagedConflicts) > 0 {
		preview.Warnings = append(preview.Warnings, "发现同名但不归 NewNexus 管理的地址记录，发布已阻止")
	}
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

func reconcileManagedAddressRecords(existing []provider.Record, hostname, recordType string, ttl int, addresses []string, owned map[string]bool) ([]provider.RecordChange, []provider.Record) {
	desired := make(map[string]bool, len(addresses))
	used := make(map[int]bool)
	changes := make([]provider.RecordChange, 0)
	conflicts := make([]provider.Record, 0)
	for _, address := range addresses {
		desired[address] = true
		matched := -1
		for index := range existing {
			if used[index] || !strings.EqualFold(existing[index].Type, recordType) || normalizeDNSName(existing[index].Name) != normalizeDNSName(hostname) || existing[index].Value != address {
				continue
			}
			matched = index
			break
		}
		if matched < 0 {
			record := provider.Record{Name: hostname, Type: recordType, Value: address, LineCode: "default", TTL: ttl, Weight: 100, Enabled: true}
			changes = append(changes, provider.RecordChange{Operation: "create", After: &record})
			continue
		}
		used[matched] = true
		current := existing[matched]
		if current.TTL != ttl || current.Proxied {
			if owned[current.ID] || current.Managed {
				after := current
				after.TTL, after.Proxied, after.Enabled = ttl, false, true
				changes = append(changes, provider.RecordChange{Operation: "update", Before: &current, After: &after})
			} else {
				conflicts = append(conflicts, current)
			}
		}
	}
	for index := range existing {
		current := existing[index]
		if used[index] || !strings.EqualFold(current.Type, recordType) || normalizeDNSName(current.Name) != normalizeDNSName(hostname) || desired[current.Value] {
			continue
		}
		if owned[current.ID] || current.Managed {
			changes = append(changes, provider.RecordChange{Operation: "delete", Before: &current})
		} else {
			conflicts = append(conflicts, current)
		}
	}
	return changes, conflicts
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
func (s *Server) createProbeProfile(w http.ResponseWriter, r *http.Request) {
	var item domain.ProbeProfile
	if !decode(w, r, &item) {
		return
	}
	applyProbeProfileDefaults(&item)
	if err := validateProbeProfile(item); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid_probe_profile", err.Error())
		return
	}
	if err := s.store.DB.Create(&item).Error; err != nil {
		writeError(w, http.StatusInternalServerError, "create_failed", "探测模板创建失败")
		return
	}
	s.audit(r, "probe_profile.create", "probe_profile", item.ID, "创建探测模板 "+item.Name, nil)
	writeData(w, http.StatusCreated, item)
}
func (s *Server) updateProbeProfile(w http.ResponseWriter, r *http.Request) {
	var item domain.ProbeProfile
	if err := s.store.DB.First(&item, "id = ?", chi.URLParam(r, "id")).Error; err != nil {
		writeError(w, 404, "not_found", "探测配置不存在")
		return
	}
	var input domain.ProbeProfile
	if !decode(w, r, &input) {
		return
	}
	input.Model = item.Model
	applyProbeProfileDefaults(&input)
	if err := validateProbeProfile(input); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid_probe_profile", err.Error())
		return
	}
	item = input
	if err := s.store.DB.Save(&item).Error; err != nil {
		writeError(w, 500, "update_failed", err.Error())
		return
	}
	s.audit(r, "probe_profile.update", "probe_profile", item.ID, "更新探测模板 "+item.Name, nil)
	writeData(w, 200, item)
}

func applyProbeProfileDefaults(item *domain.ProbeProfile) {
	if item.Name == "" {
		item.Name = "全局 HTTPS 探测"
	}
	if item.Scope == "" {
		item.Scope = "optimizer"
	}
	if item.Mode == "" {
		item.Mode = "http"
	}
	if item.Method == "" {
		item.Method = http.MethodGet
	}
	if item.ExpectedStatuses == "" {
		item.ExpectedStatuses = "[200,204,301,302,307,308,401,403]"
	}
	if item.TimeoutMS <= 0 {
		item.TimeoutMS = 5000
	}
	if item.MaxBytes <= 0 {
		item.MaxBytes = 65536
	}
	if item.UserAgent == "" {
		item.UserAgent = "Mozilla/5.0 (X11; Linux x86_64; rv:128.0) Gecko/20100101 Firefox/128.0"
	}
}

func validateProbeProfile(item domain.ProbeProfile) error {
	if item.Mode != "tcp" && item.Mode != "http" && item.Mode != "all" {
		return fmt.Errorf("探测模式必须是 tcp、http 或 all")
	}
	if item.Mode != "tcp" {
		parsed, err := url.Parse(item.URL)
		if err != nil || parsed.Hostname() == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") {
			return fmt.Errorf("HTTP 探测需要完整的 HTTP 或 HTTPS URL")
		}
	}
	if item.HeadersJSON != "" && !json.Valid([]byte(item.HeadersJSON)) {
		return fmt.Errorf("请求头必须是有效 JSON")
	}
	return nil
}

func (s *Server) listNodes(w http.ResponseWriter, _ *http.Request) {
	var items []domain.ProbeNode
	s.store.DB.Order("status asc, name asc").Find(&items)
	writeList(w, items, len(items))
}
func (s *Server) createNodeEnrollment(w http.ResponseWriter, r *http.Request) {
	var request struct {
		domain.ProbeNode
		Architecture string `json:"architecture"`
	}
	if !decode(w, r, &request) {
		return
	}
	architecture, ok := normalizeAgentArchitecture(request.Architecture)
	if !ok {
		writeError(w, http.StatusUnprocessableEntity, "agent_architecture_invalid", "Agent 架构仅支持 amd64 或 arm64")
		return
	}
	input := request.ProbeNode
	code, err := security.RandomToken(18)
	if err != nil {
		writeError(w, 500, "token_failed", err.Error())
		return
	}
	input.EnrollCodeHash = security.HashToken(code)
	now := time.Now().UTC()
	enrollExpires := now.Add(15 * time.Minute)
	input.EnrollExpiresAt = &enrollExpires
	input.Status = "pending"
	input.ProtocolVersion = "legacy"
	input.CompatibilityStatus = "pending"
	if input.Protocols == "" {
		input.Protocols = "tcp,http"
	}
	if err := s.store.DB.Create(&input).Error; err != nil {
		writeError(w, 500, "create_failed", err.Error())
		return
	}
	baseURL := strings.TrimRight(s.cfg.PublicURL, "/")
	manualCommand := fmt.Sprintf("newnexus-agent --server %s --enroll-code %s --name %s --protocols %s", shellQuote(s.cfg.AgentPublicServer), shellQuote(code), shellQuote(input.Name), shellQuote(input.Protocols))
	downloadURL := fmt.Sprintf("%s/downloads/newnexus-agent-linux-%s", baseURL, architecture)
	installArgs := fmt.Sprintf("--server %s --code %s --name %s --protocols %s --architecture %s --download-url %s", shellQuote(s.cfg.AgentPublicServer), shellQuote(code), shellQuote(input.Name), shellQuote(input.Protocols), shellQuote("linux-"+architecture), shellQuote(downloadURL))
	checksum := ""
	if file, err := os.Open(s.agentBinaryPath(architecture)); err == nil {
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
	} else if s.cfg.AgentTLSFingerprint != "" {
		installArgs += " --tls-sha256 " + shellQuote(s.cfg.AgentTLSFingerprint)
		manualCommand += " --tls-sha256 " + shellQuote(s.cfg.AgentTLSFingerprint)
	}
	installCommand := fmt.Sprintf("curl -fsSL %s | sudo sh -s -- %s", shellQuote(baseURL+"/install-agent.sh"), installArgs)
	writeData(w, 201, map[string]any{"node": input, "enrollmentCode": code, "command": manualCommand, "installCommand": installCommand, "binarySha256": checksum, "architecture": "linux-" + architecture})
}

func (s *Server) createNodeRecoveryCode(w http.ResponseWriter, r *http.Request) {
	nodeID := chi.URLParam(r, "id")
	code, expiresAt, err := s.hub.CreateRecoveryCode(nodeID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			writeError(w, http.StatusNotFound, "node_not_found", "节点不存在或已撤销")
			return
		}
		writeError(w, http.StatusUnprocessableEntity, "recovery_code_failed", err.Error())
		return
	}
	command := fmt.Sprintf("sudo sh -c %s", shellQuote("sed -i '/^NEWNEXUS_AGENT_RECOVERY_CODE=/d' /etc/newnexus/agent.env && printf '%s\\n' 'NEWNEXUS_AGENT_RECOVERY_CODE=\""+code+"\"' >> /etc/newnexus/agent.env && systemctl restart newnexus-agent.service"))
	s.audit(r, "node.recovery_code.create", "probe_node", nodeID, "生成 Agent 一次性恢复码", map[string]any{"expiresAt": expiresAt})
	writeData(w, http.StatusCreated, map[string]any{"recoveryCode": code, "expiresAt": expiresAt, "command": command})
}

func (s *Server) revokeNodeRecoveryCode(w http.ResponseWriter, r *http.Request) {
	nodeID := chi.URLParam(r, "id")
	if err := s.hub.RevokeRecoveryCode(nodeID); err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			writeError(w, http.StatusNotFound, "node_not_found", "节点不存在")
			return
		}
		writeError(w, http.StatusUnprocessableEntity, "recovery_code_revoke_failed", err.Error())
		return
	}
	s.audit(r, "node.recovery_code.revoke", "probe_node", nodeID, "撤销 Agent 一次性恢复码", nil)
	writeData(w, http.StatusOK, map[string]bool{"revoked": true})
}

func (s *Server) rotateNodeCredentials(w http.ResponseWriter, r *http.Request) {
	nodeID := chi.URLParam(r, "id")
	var node domain.ProbeNode
	if err := s.store.DB.First(&node, "id = ?", nodeID).Error; err != nil {
		writeError(w, http.StatusNotFound, "node_not_found", "节点不存在")
		return
	}
	if node.RevokedAt != nil || node.Status == "revoked" {
		writeError(w, http.StatusConflict, "node_revoked", "已撤销节点不能轮换凭据")
		return
	}
	admin, _ := auth.AdminFromContext(r.Context())
	job, enqueued := s.enqueueHTTPJob(w, r, "node.rotate", "probe_node", nodeID, jobPayload{Actor: admin.Username, IPAddress: r.RemoteAddr})
	if !enqueued {
		return
	}
	s.audit(r, "node.rotate.queued", "probe_node", nodeID, "Agent 凭据轮换已进入作业队列", map[string]any{"jobId": job.ID})
}

func (s *Server) revokeNode(w http.ResponseWriter, r *http.Request) {
	if err := s.hub.RevokeNode(chi.URLParam(r, "id")); err != nil {
		writeError(w, 422, "revoke_failed", err.Error())
		return
	}
	s.audit(r, "node.revoke", "probe_node", chi.URLParam(r, "id"), "撤销 Agent 凭据", nil)
	writeData(w, 200, map[string]bool{"revoked": true})
}

func (s *Server) deleteNode(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var node domain.ProbeNode
	if err := s.store.DB.First(&node, "id = ?", id).Error; err != nil {
		writeError(w, http.StatusNotFound, "node_not_found", "节点不存在")
		return
	}
	if node.Status == "online" && node.RevokedAt == nil {
		writeError(w, http.StatusConflict, "node_online", "在线节点必须先撤销凭据并断开连接")
		return
	}
	var activeTasks int64
	if err := s.store.DB.Model(&domain.AgentTask{}).
		Where("node_id = ? AND status IN ?", id, []string{"queued", "sent", "leased", "acknowledged", "running"}).
		Count(&activeTasks).Error; err != nil {
		writeError(w, http.StatusInternalServerError, "node_dependency_check_failed", "无法检查节点任务")
		return
	}
	if activeTasks > 0 {
		writeErrorWithData(w, http.StatusConflict, "node_has_active_tasks", "节点仍有未完成任务，请等待任务结束或取消后再删除", map[string]any{"activeTasks": activeTasks})
		return
	}
	if node.RevokedAt == nil && node.GroupName != "" {
		var remaining int64
		if err := s.store.DB.Model(&domain.ProbeNode{}).
			Where("id <> ? AND group_name = ? AND revoked_at IS NULL", id, node.GroupName).
			Count(&remaining).Error; err != nil {
			writeError(w, http.StatusInternalServerError, "node_dependency_check_failed", "无法检查节点组容量")
			return
		}
		var lines []domain.RouteLine
		if err := s.store.DB.Where("node_group = ?", node.GroupName).Find(&lines).Error; err != nil {
			writeError(w, http.StatusInternalServerError, "node_dependency_check_failed", "无法检查线路仲裁依赖")
			return
		}
		for _, line := range lines {
			minimum := line.MinNodes
			if minimum < 2 {
				minimum = 2
			}
			if remaining < int64(minimum) {
				writeErrorWithData(w, http.StatusConflict, "node_group_capacity_required", "删除后节点组不足以满足线路仲裁，请先调整线路节点组或补充节点", map[string]any{"nodeGroup": node.GroupName, "remaining": remaining, "required": minimum, "routeLineId": line.ID})
				return
			}
		}
	}
	if node.RevokedAt == nil {
		if err := s.hub.RevokeNode(id); err != nil {
			writeError(w, http.StatusUnprocessableEntity, "revoke_failed", "删除前无法撤销节点凭据")
			return
		}
	}
	if err := s.store.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("node_id = ?", id).Delete(&domain.AgentTask{}).Error; err != nil {
			return err
		}
		return tx.Delete(&domain.ProbeNode{}, "id = ?", id).Error
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "node_delete_failed", "节点删除失败")
		return
	}
	s.audit(r, "node.delete", "probe_node", id, "删除 Agent 节点 "+node.Name, map[string]any{"groupName": node.GroupName})
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) agentInstallScript(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	w.Header().Set("Content-Disposition", `inline; filename="install-agent.sh"`)
	w.Header().Set("Cache-Control", "public, max-age=300")
	http.ServeFile(w, r, "deploy/install-agent.sh")
}

func (s *Server) agentDownload(w http.ResponseWriter, r *http.Request) {
	architecture, ok := normalizeAgentArchitecture(chi.URLParam(r, "architecture"))
	if !ok {
		writeError(w, http.StatusNotFound, "agent_architecture_unavailable", "不支持该 Agent 架构")
		return
	}
	binaryPath := s.agentBinaryPath(architecture)
	info, err := os.Stat(binaryPath)
	if err != nil || info.IsDir() {
		writeError(w, http.StatusNotFound, "agent_binary_unavailable", "主控未安装 Linux "+architecture+" Agent 二进制")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="newnexus-agent-linux-%s"`, architecture))
	w.Header().Set("Cache-Control", "public, max-age=3600")
	http.ServeFile(w, r, binaryPath)
}

func (s *Server) agentBinaryPath(architecture string) string {
	if strings.Contains(s.cfg.AgentBinaryPath, "{arch}") {
		return strings.ReplaceAll(s.cfg.AgentBinaryPath, "{arch}", architecture)
	}
	if architecture == "amd64" {
		return s.cfg.AgentBinaryPath
	}
	return ""
}

func normalizeAgentArchitecture(value string) (string, bool) {
	value = strings.ToLower(strings.TrimSpace(value))
	switch value {
	case "", "amd64", "x86_64", "linux-amd64":
		return "amd64", true
	case "arm64", "aarch64", "linux-arm64":
		return "arm64", true
	default:
		return "", false
	}
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

func (s *Server) listRoutes(w http.ResponseWriter, r *http.Request) {
	var items []domain.Route
	query := s.store.DB.Model(&domain.Route{})
	total, limit := countAndLimit(query, r, 100, 500)
	if cursorTime, cursorID, ok := decodeCursor(r.URL.Query().Get("cursor")); ok {
		query = query.Where("created_at > ? OR (created_at = ? AND id > ?)", cursorTime, cursorTime, cursorID)
	}
	if err := query.Order("created_at ASC, id ASC").Limit(limit + 1).Find(&items).Error; err != nil {
		writeError(w, 500, "routes_unavailable", "无法读取线路规则")
		return
	}
	items, next := pageRoutes(items, limit)
	writeCursorList(w, items, next, total, nil)
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
	input.ZoneName = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(input.ZoneName), "."))
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
	if err := s.validateRouteReferences(input.Route); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid_route", err.Error())
		return
	}
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

func (s *Server) updateRoute(w http.ResponseWriter, r *http.Request) {
	var route domain.Route
	if err := s.store.DB.First(&route, "id = ?", chi.URLParam(r, "id")).Error; err != nil {
		writeError(w, http.StatusNotFound, "route_not_found", "线路规则不存在")
		return
	}
	var input struct {
		Name            string `json:"name"`
		SiteID          string `json:"siteId"`
		TTL             int    `json:"ttl"`
		PublishMode     string `json:"publishMode"`
		SwitchMode      string `json:"switchMode"`
		Status          string `json:"status"`
		ProbeProfileID  string `json:"probeProfileId"`
		DNSConnectionID string `json:"dnsConnectionId"`
		ZoneID          string `json:"zoneId"`
		ZoneName        string `json:"zoneName"`
	}
	if !decode(w, r, &input) {
		return
	}
	if strings.TrimSpace(input.Name) == "" || input.TTL < 1 {
		writeError(w, http.StatusUnprocessableEntity, "invalid_route", "规则名称不能为空，TTL 必须大于 0")
		return
	}
	if input.DNSConnectionID == "" || input.ZoneID == "" || input.ZoneName == "" {
		writeError(w, http.StatusUnprocessableEntity, "invalid_route", "必须显式选择 DNS 连接和区域")
		return
	}
	proposed := route
	proposed.SiteID, proposed.ProbeProfileID = input.SiteID, input.ProbeProfileID
	proposed.DNSConnectionID, proposed.ZoneID, proposed.ZoneName = input.DNSConnectionID, input.ZoneID, strings.ToLower(strings.TrimSuffix(input.ZoneName, "."))
	if err := s.validateRouteReferences(proposed); err != nil {
		writeError(w, http.StatusUnprocessableEntity, "invalid_route", err.Error())
		return
	}
	route.Name, route.SiteID, route.TTL = strings.TrimSpace(input.Name), input.SiteID, input.TTL
	route.PublishMode, route.SwitchMode, route.ProbeProfileID = input.PublishMode, input.SwitchMode, input.ProbeProfileID
	route.DNSConnectionID, route.ZoneID, route.ZoneName = input.DNSConnectionID, input.ZoneID, strings.ToLower(strings.TrimSuffix(input.ZoneName, "."))
	if input.Status != "" {
		route.Status = input.Status
	}
	if err := s.store.DB.Save(&route).Error; err != nil {
		writeError(w, http.StatusInternalServerError, "update_failed", "线路规则更新失败")
		return
	}
	s.audit(r, "route.update", "route", route.ID, "更新线路规则 "+route.Name, nil)
	detail, _ := s.routes.Detail(route.ID)
	writeData(w, http.StatusOK, detail)
}

func (s *Server) deleteRoute(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if !s.ensureNoActiveResourceJobs(w, "route", id) {
		return
	}
	var managed int64
	_ = s.store.DB.Model(&domain.ManagedDNSRecord{}).Where("resource_type = ? AND resource_id = ?", "route", id).Count(&managed).Error
	if managed > 0 {
		writeError(w, http.StatusConflict, "route_has_managed_dns", "该规则仍拥有已发布 DNS 记录，请先回滚或清理发布记录")
		return
	}
	err := s.store.DB.Transaction(func(tx *gorm.DB) error {
		var lines []domain.RouteLine
		if err := tx.Where("route_id = ?", id).Find(&lines).Error; err != nil {
			return err
		}
		for _, line := range lines {
			if err := tx.Where("route_line_id = ?", line.ID).Delete(&domain.RouteTarget{}).Error; err != nil {
				return err
			}
		}
		if err := tx.Where("route_id = ?", id).Delete(&domain.RouteLine{}).Error; err != nil {
			return err
		}
		if err := tx.Where("route_id = ?", id).Delete(&domain.Release{}).Error; err != nil {
			return err
		}
		return tx.Delete(&domain.Route{}, "id = ?", id).Error
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "delete_failed", "线路规则删除失败")
		return
	}
	s.audit(r, "route.delete", "route", id, "删除线路规则", nil)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) ensureNoActiveResourceJobs(w http.ResponseWriter, resourceType, resourceID string) bool {
	var active []domain.Job
	if err := s.store.DB.Select("id, type, status, created_at").
		Where("resource_type = ? AND resource_id = ? AND status IN ?", resourceType, resourceID, []string{jobs.StatusQueued, jobs.StatusRunning}).
		Order("created_at ASC").Limit(10).Find(&active).Error; err != nil {
		writeError(w, http.StatusInternalServerError, "job_dependency_check_failed", "无法检查资源关联作业")
		return false
	}
	if len(active) > 0 {
		writeErrorWithData(w, http.StatusConflict, "resource_job_in_progress", "资源仍有未完成的外部写操作，请等待作业结束或先取消作业", map[string]any{"jobs": active})
		return false
	}
	return true
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

func (s *Server) updateRouteLine(w http.ResponseWriter, r *http.Request) {
	_, line, ok := s.routeLineContext(w, chi.URLParam(r, "id"), chi.URLParam(r, "lineId"))
	if !ok {
		return
	}
	var input struct {
		Alias             string `json:"alias"`
		ProviderLineCode  string `json:"providerLineCode"`
		NodeGroup         string `json:"nodeGroup"`
		HealthPerspective string `json:"healthPerspective"`
		MinNodes          int    `json:"minNodes"`
		QuorumPercent     int    `json:"quorumPercent"`
		Status            string `json:"status"`
	}
	if !decode(w, r, &input) {
		return
	}
	if strings.TrimSpace(input.Alias) == "" || strings.TrimSpace(input.ProviderLineCode) == "" || input.MinNodes < 2 || input.QuorumPercent < 51 || input.QuorumPercent > 100 {
		writeError(w, http.StatusUnprocessableEntity, "invalid_route_line", "线路别名、代码、至少 2 个节点和 51-100% 仲裁比例均为必填项")
		return
	}
	line.Alias, line.ProviderLineCode, line.NodeGroup = strings.TrimSpace(input.Alias), strings.TrimSpace(input.ProviderLineCode), strings.TrimSpace(input.NodeGroup)
	line.HealthPerspective, line.MinNodes, line.QuorumPercent = input.HealthPerspective, input.MinNodes, input.QuorumPercent
	if input.Status != "" {
		line.Status = input.Status
	}
	if err := s.store.DB.Save(&line).Error; err != nil {
		writeError(w, http.StatusInternalServerError, "update_failed", "线路更新失败")
		return
	}
	s.audit(r, "route_line.update", "route_line", line.ID, "更新线路 "+line.Alias, nil)
	writeData(w, http.StatusOK, line)
}

func (s *Server) deleteRouteLine(w http.ResponseWriter, r *http.Request) {
	route, line, ok := s.routeLineContext(w, chi.URLParam(r, "id"), chi.URLParam(r, "lineId"))
	if !ok {
		return
	}
	var count int64
	_ = s.store.DB.Model(&domain.RouteLine{}).Where("route_id = ?", route.ID).Count(&count).Error
	if count <= 1 {
		writeError(w, http.StatusConflict, "last_route_line", "规则必须保留至少一条线路")
		return
	}
	err := s.store.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("route_line_id = ?", line.ID).Delete(&domain.RouteTarget{}).Error; err != nil {
			return err
		}
		return tx.Delete(&line).Error
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "delete_failed", "线路删除失败")
		return
	}
	s.audit(r, "route_line.delete", "route_line", line.ID, "删除线路 "+line.Alias, nil)
	w.WriteHeader(http.StatusNoContent)
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
		Name              string  `json:"name"`
		CNAME             string  `json:"cname"`
		Priority          int     `json:"priority"`
		Weight            int     `json:"weight"`
		Enabled           *bool   `json:"enabled"`
		ProbeOverrideJSON *string `json:"probeOverrideJson"`
	}
	if !decode(w, r, &input) {
		return
	}
	target.Name, target.CNAME, target.Priority, target.Weight = input.Name, input.CNAME, input.Priority, input.Weight
	if input.ProbeOverrideJSON != nil {
		target.ProbeOverrideJSON = *input.ProbeOverrideJSON
	}
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

func (s *Server) deleteRouteTarget(w http.ResponseWriter, r *http.Request) {
	_, line, ok := s.routeLineContext(w, chi.URLParam(r, "id"), chi.URLParam(r, "lineId"))
	if !ok {
		return
	}
	var count int64
	_ = s.store.DB.Model(&domain.RouteTarget{}).Where("route_line_id = ?", line.ID).Count(&count).Error
	if count <= 1 {
		writeError(w, http.StatusConflict, "last_route_target", "每条线路必须保留至少一个 CNAME 目标")
		return
	}
	id := chi.URLParam(r, "targetId")
	result := s.store.DB.Delete(&domain.RouteTarget{}, "id = ? AND route_line_id = ?", id, line.ID)
	if result.Error != nil {
		writeError(w, http.StatusInternalServerError, "delete_failed", "CNAME 目标删除失败")
		return
	}
	if result.RowsAffected == 0 {
		writeError(w, http.StatusNotFound, "target_not_found", "CNAME 目标不存在")
		return
	}
	s.audit(r, "route_target.delete", "route_target", id, "删除 CNAME 目标", nil)
	w.WriteHeader(http.StatusNoContent)
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
	if err := validateProbeOverrides(target.ProbeOverrideJSON); err != nil {
		return err
	}
	return routing.ValidateTarget(hostname, target.CNAME)
}

func (s *Server) validateRouteReferences(route domain.Route) error {
	if route.ProjectID == "" || route.DNSConnectionID == "" || route.ZoneID == "" || route.ZoneName == "" || route.Hostname == "" {
		return fmt.Errorf("项目、DNS 连接、区域和业务主机名均为必填项")
	}
	var count int64
	if err := s.store.DB.Model(&domain.Project{}).Where("id = ?", route.ProjectID).Count(&count).Error; err != nil || count != 1 {
		return fmt.Errorf("所选项目不存在")
	}
	if err := s.store.DB.Model(&domain.Connection{}).Where("id = ?", route.DNSConnectionID).Count(&count).Error; err != nil || count != 1 {
		return fmt.Errorf("所选 DNS 连接不存在")
	}
	if route.SiteID != "" {
		if err := s.store.DB.Model(&domain.ProjectSite{}).Where("id = ? AND project_id = ? AND status <> ?", route.SiteID, route.ProjectID, "disabled").Count(&count).Error; err != nil || count != 1 {
			return fmt.Errorf("所选站点不存在、已停用或不属于当前项目")
		}
	}
	if route.ProbeProfileID != "" {
		if err := s.store.DB.Model(&domain.ProbeProfile{}).Where("id = ? AND (project_id = ? OR project_id = '')", route.ProbeProfileID, route.ProjectID).Count(&count).Error; err != nil || count != 1 {
			return fmt.Errorf("所选探测模板不存在或不属于当前项目")
		}
	}
	return nil
}

func validateProbeOverrides(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var input struct {
		Mode             string            `json:"mode"`
		Port             int               `json:"port"`
		Method           string            `json:"method"`
		Headers          map[string]string `json:"headers"`
		ExpectedStatuses []int             `json:"expectedStatuses"`
		ExpectedContains string            `json:"expectedContains"`
		TimeoutMS        int               `json:"timeoutMs"`
		MaxBytes         int64             `json:"maxBytes"`
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return fmt.Errorf("探测覆盖配置不是有效 JSON: %w", err)
	}
	if input.Mode != "" && input.Mode != "tcp" && input.Mode != "http" {
		return fmt.Errorf("探测模式只能是 tcp 或 http")
	}
	if input.Port < 0 || input.Port > 65535 {
		return fmt.Errorf("探测端口必须在 1 到 65535 之间")
	}
	if input.TimeoutMS < 0 || input.TimeoutMS > 120000 {
		return fmt.Errorf("探测超时不能超过 120 秒")
	}
	if input.MaxBytes < 0 || input.MaxBytes > 8<<20 {
		return fmt.Errorf("探测下载上限不能超过 8 MiB")
	}
	for _, status := range input.ExpectedStatuses {
		if status < 100 || status > 599 {
			return fmt.Errorf("期望 HTTP 状态码无效")
		}
	}
	return nil
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
	var route domain.Route
	if err := s.store.DB.First(&route, "id = ?", chi.URLParam(r, "id")).Error; err != nil {
		writeError(w, 404, "route_not_found", "线路规则不存在")
		return
	}
	s.enqueueHTTPJob(w, r, "route.apply", "route", route.ID, jobPayload{Actor: admin.Username, IPAddress: r.RemoteAddr, Trigger: "manual"})
}
func (s *Server) listReleases(w http.ResponseWriter, r *http.Request) {
	var items []domain.Release
	query := s.store.DB.Model(&domain.Release{})
	total, limit := countAndLimit(query, r, 50, 200)
	if cursorTime, cursorID, ok := decodeCursor(r.URL.Query().Get("cursor")); ok {
		query = query.Where("created_at < ? OR (created_at = ? AND id < ?)", cursorTime, cursorTime, cursorID)
	}
	if err := query.Order("created_at DESC, id DESC").Limit(limit + 1).Find(&items).Error; err != nil {
		writeError(w, 500, "releases_unavailable", "无法读取发布记录")
		return
	}
	items, next := pageReleases(items, limit)
	writeCursorList(w, items, next, total, nil)
}
func (s *Server) rollbackRelease(w http.ResponseWriter, r *http.Request) {
	admin, _ := auth.AdminFromContext(r.Context())
	var release domain.Release
	if err := s.store.DB.First(&release, "id = ?", chi.URLParam(r, "id")).Error; err != nil {
		writeError(w, 404, "release_not_found", "发布记录不存在")
		return
	}
	s.enqueueHTTPJob(w, r, "route.rollback", "route", release.RouteID, jobPayload{Actor: admin.Username, IPAddress: r.RemoteAddr, ReleaseID: release.ID})
}
func (s *Server) listAlerts(w http.ResponseWriter, r *http.Request) {
	var items []domain.Alert
	query := s.store.DB.Model(&domain.Alert{})
	if status := r.URL.Query().Get("status"); status != "" {
		query = query.Where("status = ?", status)
	}
	total, limit := countAndLimit(query, r, 50, 200)
	if cursorTime, cursorID, ok := decodeCursor(r.URL.Query().Get("cursor")); ok {
		query = query.Where("created_at < ? OR (created_at = ? AND id < ?)", cursorTime, cursorTime, cursorID)
	}
	if err := query.Order("created_at DESC, id DESC").Limit(limit + 1).Find(&items).Error; err != nil {
		writeError(w, 500, "alerts_unavailable", "无法读取告警列表")
		return
	}
	items, next := pageAlerts(items, limit)
	statusCounts, severityCounts := map[string]int64{}, map[string]int64{}
	var statusRows []struct {
		Status string
		Count  int64
	}
	var severityRows []struct {
		Severity string
		Count    int64
	}
	if s.store.DB.Model(&domain.Alert{}).Select("status, COUNT(*) AS count").Group("status").Scan(&statusRows).Error == nil {
		for _, row := range statusRows {
			statusCounts[row.Status] = row.Count
		}
	}
	if s.store.DB.Model(&domain.Alert{}).Select("severity, COUNT(*) AS count").Group("severity").Scan(&severityRows).Error == nil {
		for _, row := range severityRows {
			severityCounts[row.Severity] = row.Count
		}
	}
	writeCursorList(w, items, next, total, map[string]any{"statusCounts": statusCounts, "severityCounts": severityCounts})
}
func (s *Server) acknowledgeAlert(w http.ResponseWriter, r *http.Request) {
	now := time.Now().UTC()
	if err := s.store.DB.Model(&domain.Alert{}).Where("id = ?", chi.URLParam(r, "id")).Updates(map[string]any{"status": "acknowledged", "acknowledged_at": &now}).Error; err != nil {
		writeError(w, 500, "update_failed", err.Error())
		return
	}
	s.audit(r, "alert.acknowledge", "alert", chi.URLParam(r, "id"), "确认告警", nil)
	writeData(w, 200, map[string]bool{"acknowledged": true})
}

func (s *Server) resolveAlert(w http.ResponseWriter, r *http.Request) {
	now := time.Now().UTC()
	result := s.store.DB.Model(&domain.Alert{}).Where("id = ?", chi.URLParam(r, "id")).Updates(map[string]any{"status": "resolved", "resolved_at": &now, "silenced_until": nil})
	if result.Error != nil {
		writeError(w, http.StatusInternalServerError, "update_failed", "告警状态更新失败")
		return
	}
	if result.RowsAffected == 0 {
		writeError(w, http.StatusNotFound, "alert_not_found", "告警不存在")
		return
	}
	s.audit(r, "alert.resolve", "alert", chi.URLParam(r, "id"), "恢复告警", nil)
	writeData(w, http.StatusOK, map[string]bool{"resolved": true})
}

func (s *Server) silenceAlert(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Until time.Time `json:"until"`
	}
	if !decode(w, r, &input) {
		return
	}
	if input.Until.Before(time.Now().UTC()) {
		writeError(w, http.StatusUnprocessableEntity, "invalid_silence", "静默截止时间必须晚于当前时间")
		return
	}
	result := s.store.DB.Model(&domain.Alert{}).Where("id = ?", chi.URLParam(r, "id")).Updates(map[string]any{"status": "silenced", "silenced_until": input.Until.UTC()})
	if result.Error != nil {
		writeError(w, http.StatusInternalServerError, "update_failed", "告警状态更新失败")
		return
	}
	if result.RowsAffected == 0 {
		writeError(w, http.StatusNotFound, "alert_not_found", "告警不存在")
		return
	}
	s.audit(r, "alert.silence", "alert", chi.URLParam(r, "id"), "静默告警", map[string]any{"until": input.Until.UTC()})
	writeData(w, http.StatusOK, map[string]any{"silenced": true, "until": input.Until.UTC()})
}

func (s *Server) testNotifications(w http.ResponseWriter, r *http.Request) {
	webhook := strings.TrimSpace(s.settingValue("alerts.webhook_url"))
	smtpHost := strings.TrimSpace(s.settingValue("alerts.smtp_host"))
	if webhook == "" && smtpHost == "" {
		writeError(w, http.StatusUnprocessableEntity, "notification_channel_missing", "请先配置 Webhook 或 SMTP 通知渠道")
		return
	}
	admin, _ := auth.AdminFromContext(r.Context())
	actor := admin.Username
	if actor == "" {
		actor = "api-key"
	}
	s.enqueueHTTPJob(w, r, "notification.test", "notification", "configured-channels", jobPayload{Actor: actor, IPAddress: r.RemoteAddr, Trigger: "manual_test"})
}
func (s *Server) listAudit(w http.ResponseWriter, r *http.Request) {
	var items []domain.AuditEvent
	query := s.store.DB.Model(&domain.AuditEvent{})
	total, limit := countAndLimit(query, r, 50, 200)
	if cursorTime, cursorID, ok := decodeCursor(r.URL.Query().Get("cursor")); ok {
		query = query.Where("created_at < ? OR (created_at = ? AND id < ?)", cursorTime, cursorTime, cursorID)
	}
	if err := query.Order("created_at DESC, id DESC").Limit(limit + 1).Find(&items).Error; err != nil {
		writeError(w, 500, "audit_unavailable", "无法读取审计日志")
		return
	}
	items, next := pageAuditEvents(items, limit)
	writeCursorList(w, items, next, total, nil)
}
func (s *Server) listJobs(w http.ResponseWriter, r *http.Request) {
	limit := parseIntDefault(r.URL.Query().Get("limit"), 50)
	if limit < 1 {
		limit = 1
	}
	if limit > 200 {
		limit = 200
	}
	query := s.store.DB.Model(&domain.Job{})
	if status := strings.TrimSpace(r.URL.Query().Get("status")); status != "" {
		query = query.Where("status = ?", status)
	}
	if jobType := strings.TrimSpace(r.URL.Query().Get("type")); jobType != "" {
		query = query.Where("type = ?", jobType)
	}
	var total int64
	if err := query.Count(&total).Error; err != nil {
		writeError(w, 500, "jobs_unavailable", "无法统计作业列表")
		return
	}
	if cursorTime, cursorID, ok := decodeCursor(r.URL.Query().Get("cursor")); ok {
		query = query.Where("created_at < ? OR (created_at = ? AND id < ?)", cursorTime, cursorTime, cursorID)
	}
	var items []domain.Job
	if err := query.Order("created_at DESC, id DESC").Limit(limit + 1).Find(&items).Error; err != nil {
		writeError(w, 500, "jobs_unavailable", "无法读取作业列表")
		return
	}
	next := ""
	if len(items) > limit {
		items = items[:limit]
		last := items[len(items)-1]
		next = encodeCursor(last.CreatedAt, last.ID)
	}
	s.decorateJobLocks(items)
	statusCounts := make(map[string]int64)
	var grouped []struct {
		Status string
		Count  int64
	}
	if err := s.store.DB.Model(&domain.Job{}).Select("status, COUNT(*) AS count").Group("status").Scan(&grouped).Error; err == nil {
		for _, row := range grouped {
			statusCounts[row.Status] = row.Count
		}
	}
	writeCursorList(w, items, next, int(total), map[string]any{"statusCounts": statusCounts})
}

func (s *Server) decorateJobLocks(items []domain.Job) {
	keys := make([]string, 0)
	seen := make(map[string]bool)
	for _, item := range items {
		if item.ExecutionKey != "" && !seen[item.ExecutionKey] {
			seen[item.ExecutionKey] = true
			keys = append(keys, item.ExecutionKey)
		}
	}
	if len(keys) == 0 {
		return
	}
	var leases []domain.JobExecutionLease
	_ = s.store.DB.Where("execution_key IN ? AND expires_at > ?", keys, time.Now().UTC()).Find(&leases).Error
	owners := make(map[string]string, len(leases))
	for _, lease := range leases {
		owners[lease.ExecutionKey] = lease.JobID
	}
	var queued []domain.Job
	_ = s.store.DB.Select("id, execution_key, run_at, created_at").Where("execution_key IN ? AND status = ?", keys, jobs.StatusQueued).Order("run_at ASC, created_at ASC").Find(&queued).Error
	positions := make(map[string]int)
	firstQueued := make(map[string]string)
	for _, queuedJob := range queued {
		positions[queuedJob.ExecutionKey]++
		if firstQueued[queuedJob.ExecutionKey] == "" {
			firstQueued[queuedJob.ExecutionKey] = queuedJob.ID
		}
		for index := range items {
			if items[index].ID != queuedJob.ID {
				continue
			}
			items[index].ExecutionOrder = positions[queuedJob.ExecutionKey]
			owner := owners[queuedJob.ExecutionKey]
			if owner == "" && items[index].ExecutionOrder > 1 {
				owner = firstQueued[queuedJob.ExecutionKey]
			}
			items[index].LockOwnerJobID = owner
			items[index].WaitingForLock = owner != "" && owner != items[index].ID
		}
	}
}
func (s *Server) getJob(w http.ResponseWriter, r *http.Request) {
	var job domain.Job
	if err := s.store.DB.First(&job, "id = ?", chi.URLParam(r, "id")).Error; err != nil {
		writeError(w, 404, "job_not_found", "作业不存在")
		return
	}
	var attempts []domain.JobAttempt
	_ = s.store.DB.Where("job_id = ?", job.ID).Order("attempt ASC").Find(&attempts).Error
	writeData(w, 200, map[string]any{"job": job, "attempts": attempts})
}
func (s *Server) cancelJob(w http.ResponseWriter, r *http.Request) {
	job, err := s.jobs.Cancel(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, 422, "job_not_cancellable", err.Error())
		return
	}
	s.audit(r, "job.cancel", "job", job.ID, "取消作业", map[string]any{"type": job.Type})
	writeData(w, 200, job)
}
func (s *Server) retryJob(w http.ResponseWriter, r *http.Request) {
	job, err := s.jobs.Retry(chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, 422, "job_not_retryable", err.Error())
		return
	}
	s.audit(r, "job.retry", "job", job.ID, "重试作业", map[string]any{"type": job.Type})
	writeData(w, 202, map[string]any{"jobId": job.ID, "job": job})
}

func (s *Server) resumeJob(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var failed domain.Job
	if err := s.store.DB.First(&failed, "id = ?", id).Error; err != nil {
		writeError(w, http.StatusNotFound, "job_not_found", "作业不存在")
		return
	}
	if failed.ErrorCode == "dns_automation_paused" && failed.ResourceType == "custom_hostname" {
		result := s.store.DB.Model(&domain.CustomHostname{}).Where("id = ? AND automation_mode = ?", failed.ResourceID, "automatic").Updates(map[string]any{"dns_automation_paused": false, "dns_automation_pause_reason": "", "error_message": ""})
		if result.Error != nil || result.RowsAffected != 1 {
			writeError(w, http.StatusUnprocessableEntity, "automation_resume_failed", "无法恢复 SaaS DNS 自动化")
			return
		}
	}
	job, err := s.jobs.Retry(id)
	if err != nil {
		writeError(w, 422, "job_resume_failed", err.Error())
		return
	}
	s.audit(r, "job.resume", "job", job.ID, "恢复作业", map[string]any{"type": job.Type, "resourceId": job.ResourceID})
	writeData(w, http.StatusAccepted, map[string]any{"jobId": job.ID, "job": job})
}
func (s *Server) listSettings(w http.ResponseWriter, _ *http.Request) {
	var items []domain.Setting
	s.store.DB.Order("key asc").Find(&items)
	for index := range items {
		if sensitiveSetting(items[index].Key) && items[index].Value != "" {
			items[index].Value = "<masked>"
		}
	}
	defaults := map[string]string{"retention.raw_days": "7", "retention.five_minute_days": "30", "retention.hourly_days": "180", "probe.discovery_interval": "6h", "probe.ranking_interval": "5m", "probe.active_interval": "30s", "alerts.webhook_url": "", "alerts.webhook_secret": "", "alerts.smtp_host": "", "alerts.smtp_username": "", "alerts.smtp_password": "", "alerts.smtp_from": "", "alerts.smtp_to": ""}
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
		if err := validateSetting(key, value); err != nil {
			writeError(w, http.StatusUnprocessableEntity, "invalid_setting", err.Error())
			return
		}
	}
	response := make(map[string]string, len(values))
	for key, value := range values {
		if sensitiveSetting(key) {
			if value == "<masked>" {
				response[key] = value
				continue
			}
			if value != "" {
				encrypted, err := s.vault.Encrypt(value)
				if err != nil {
					writeError(w, http.StatusInternalServerError, "setting_encrypt_failed", "敏感设置加密失败")
					return
				}
				value = "enc:" + encrypted
			}
			response[key] = map[bool]string{true: "<masked>", false: ""}[value != ""]
		} else {
			response[key] = value
		}
		item := domain.Setting{Key: key, Value: value, UpdatedAt: time.Now().UTC()}
		if err := s.store.DB.Save(&item).Error; err != nil {
			writeError(w, http.StatusInternalServerError, "setting_save_failed", "设置保存失败")
			return
		}
	}
	s.audit(r, "settings.update", "settings", "global", "更新系统设置", map[string]any{"keys": keys(values)})
	writeData(w, http.StatusOK, response)
}

func validateSetting(key, value string) error {
	allowed := map[string]bool{
		"retention.raw_days": true, "retention.five_minute_days": true, "retention.hourly_days": true,
		"probe.discovery_interval": true, "probe.ranking_interval": true, "probe.active_interval": true,
		"alerts.webhook_url": true, "alerts.webhook_secret": true, "alerts.smtp_host": true,
		"alerts.smtp_username": true, "alerts.smtp_password": true, "alerts.smtp_from": true, "alerts.smtp_to": true,
	}
	if !allowed[key] {
		return fmt.Errorf("不支持的设置项 %s", key)
	}
	if value == "<masked>" && sensitiveSetting(key) {
		return nil
	}
	switch key {
	case "retention.raw_days", "retention.five_minute_days", "retention.hourly_days":
		days, err := strconv.Atoi(value)
		if err != nil || days < 1 || days > 3650 {
			return fmt.Errorf("%s 必须是 1 到 3650 之间的天数", key)
		}
	case "probe.discovery_interval", "probe.ranking_interval", "probe.active_interval":
		interval, err := time.ParseDuration(value)
		minimum := map[string]time.Duration{"probe.discovery_interval": time.Minute, "probe.ranking_interval": 10 * time.Second, "probe.active_interval": 5 * time.Second}[key]
		if err != nil || interval < minimum || interval > 30*24*time.Hour {
			return fmt.Errorf("%s 不是有效频率或超出允许范围", key)
		}
	case "alerts.webhook_url":
		if value == "" {
			return nil
		}
		parsed, err := url.Parse(value)
		if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil {
			return fmt.Errorf("Webhook URL 必须是不含凭据的 HTTPS 地址")
		}
	case "alerts.smtp_host":
		if value == "" {
			return nil
		}
		_, portValue, err := net.SplitHostPort(value)
		port, parseErr := strconv.Atoi(portValue)
		if err != nil || parseErr != nil || port < 1 || port > 65535 {
			return fmt.Errorf("SMTP 主机必须包含有效端口")
		}
	case "alerts.smtp_from", "alerts.smtp_to":
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			return nil
		}
		address, err := mail.ParseAddress(trimmed)
		if err != nil || address.Address != trimmed || strings.ContainsAny(trimmed, "\r\n") {
			return fmt.Errorf("%s 必须是单个有效邮件地址", key)
		}
	case "alerts.webhook_secret", "alerts.smtp_username", "alerts.smtp_password":
		if len(value) > 4096 {
			return fmt.Errorf("%s 超过长度限制", key)
		}
	}
	return nil
}

func sensitiveSetting(key string) bool {
	return key == "alerts.webhook_secret" || key == "alerts.smtp_password"
}

func (s *Server) settingValue(key string) string {
	var setting domain.Setting
	if s.store.DB.First(&setting, "key = ?", key).Error != nil {
		return ""
	}
	if strings.HasPrefix(setting.Value, "enc:") {
		value, err := s.vault.Decrypt(strings.TrimPrefix(setting.Value, "enc:"))
		if err != nil {
			return ""
		}
		return value
	}
	return setting.Value
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
		w.Header().Set("Content-Security-Policy", "default-src 'self'; base-uri 'self'; frame-ancestors 'none'; object-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; font-src 'self' data:")
		forwardedHTTPS, _ := r.Context().Value(forwardedHTTPSContextKey).(bool)
		if s.cfg.EnableHSTS && (r.TLS != nil || forwardedHTTPS) {
			w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}
func (s *Server) realIP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		remoteHost, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			remoteHost = r.RemoteAddr
		}
		remote := net.ParseIP(remoteHost)
		trusted := false
		for _, raw := range strings.Split(s.cfg.TrustedProxyCIDRs, ",") {
			if _, network, err := net.ParseCIDR(strings.TrimSpace(raw)); err == nil && remote != nil && network.Contains(remote) {
				trusted = true
				break
			}
		}
		if trusted {
			forwardedProto := strings.ToLower(strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0]))
			if forwardedProto == "https" {
				r = r.WithContext(context.WithValue(r.Context(), forwardedHTTPSContextKey, true))
			}
			candidate := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-For"), ",")[0])
			if candidate == "" {
				candidate = strings.TrimSpace(r.Header.Get("X-Real-IP"))
			}
			if net.ParseIP(candidate) != nil {
				r.RemoteAddr = candidate
			}
		}
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

func countAndLimit(query *gorm.DB, r *http.Request, fallback, maximum int) (int, int) {
	var total int64
	_ = query.Count(&total).Error
	limit := parseIntDefault(r.URL.Query().Get("limit"), fallback)
	if limit < 1 {
		limit = 1
	}
	if limit > maximum {
		limit = maximum
	}
	return int(total), limit
}

func trimCreatedPage[T any](items []T, limit int, model func(T) domain.Model) ([]T, string) {
	if len(items) <= limit {
		return items, ""
	}
	items = items[:limit]
	last := model(items[len(items)-1])
	return items, encodeCursor(last.CreatedAt, last.ID)
}

func pageProjects(items []domain.Project, limit int) ([]domain.Project, string) {
	return trimCreatedPage(items, limit, func(item domain.Project) domain.Model { return item.Model })
}
func pageConnections(items []domain.Connection, limit int) ([]domain.Connection, string) {
	return trimCreatedPage(items, limit, func(item domain.Connection) domain.Model { return item.Model })
}
func pageSaaSZones(items []domain.SaaSZone, limit int) ([]domain.SaaSZone, string) {
	return trimCreatedPage(items, limit, func(item domain.SaaSZone) domain.Model { return item.Model })
}
func pageCustomHostnames(items []domain.CustomHostname, limit int) ([]domain.CustomHostname, string) {
	return trimCreatedPage(items, limit, func(item domain.CustomHostname) domain.Model { return item.Model })
}
func pageCandidateSources(items []domain.CandidateSource, limit int) ([]domain.CandidateSource, string) {
	return trimCreatedPage(items, limit, func(item domain.CandidateSource) domain.Model { return item.Model })
}
func pageRoutes(items []domain.Route, limit int) ([]domain.Route, string) {
	return trimCreatedPage(items, limit, func(item domain.Route) domain.Model { return item.Model })
}
func pageReleases(items []domain.Release, limit int) ([]domain.Release, string) {
	return trimCreatedPage(items, limit, func(item domain.Release) domain.Model { return item.Model })
}
func pageAlerts(items []domain.Alert, limit int) ([]domain.Alert, string) {
	return trimCreatedPage(items, limit, func(item domain.Alert) domain.Model { return item.Model })
}
func pageAuditEvents(items []domain.AuditEvent, limit int) ([]domain.AuditEvent, string) {
	return trimCreatedPage(items, limit, func(item domain.AuditEvent) domain.Model { return item.Model })
}

type candidateCursor struct {
	Score   float64 `json:"score"`
	Address string  `json:"address"`
	ID      string  `json:"id"`
}

func encodeCandidateCursor(item domain.Candidate) string {
	raw, _ := json.Marshal(candidateCursor{Score: item.Score, Address: item.Address, ID: item.ID})
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeCandidateCursor(raw string) (float64, string, string, bool) {
	decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(raw))
	if err != nil {
		return 0, "", "", false
	}
	var cursor candidateCursor
	if json.Unmarshal(decoded, &cursor) != nil || cursor.Address == "" || cursor.ID == "" {
		return 0, "", "", false
	}
	return cursor.Score, cursor.Address, cursor.ID, true
}

func candidateFollowsCursor(item domain.Candidate, score float64, address, id string) bool {
	return item.Score < score || (item.Score == score && (item.Address > address || (item.Address == address && item.ID > id)))
}

func pageCandidates(items []domain.Candidate, limit int) ([]domain.Candidate, string) {
	if len(items) <= limit {
		return items, ""
	}
	items = items[:limit]
	return items, encodeCandidateCursor(items[len(items)-1])
}

func writeCursorList(w http.ResponseWriter, data any, nextCursor string, total int, extra map[string]any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	meta := map[string]any{"nextCursor": nextCursor, "total": total}
	for key, value := range extra {
		meta[key] = value
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "meta": meta})
}
func encodeCursor(createdAt time.Time, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(createdAt.UTC().Format(time.RFC3339Nano) + "|" + id))
}
func decodeCursor(raw string) (time.Time, string, bool) {
	decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(raw))
	if err != nil {
		return time.Time{}, "", false
	}
	parts := strings.SplitN(string(decoded), "|", 2)
	if len(parts) != 2 {
		return time.Time{}, "", false
	}
	parsed, err := time.Parse(time.RFC3339Nano, parts[0])
	return parsed, parts[1], err == nil && parts[1] != ""
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
