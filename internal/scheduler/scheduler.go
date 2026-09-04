package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/SnowBall-Bqiu/NewNexus/internal/agenthub"
	"github.com/SnowBall-Bqiu/NewNexus/internal/domain"
	"github.com/SnowBall-Bqiu/NewNexus/internal/jobs"
	"github.com/SnowBall-Bqiu/NewNexus/internal/optimizer"
	"github.com/SnowBall-Bqiu/NewNexus/internal/routing"
	"github.com/SnowBall-Bqiu/NewNexus/internal/store"
)

type Scheduler struct {
	store             *store.Store
	hub               *agenthub.Hub
	optimizer         *optimizer.Service
	routes            *routing.Service
	interval          time.Duration
	inventoryInterval time.Duration
	rankingInterval   time.Duration
	activeInterval    time.Duration
	maintenanceEvery  time.Duration
	lastMaintenance   time.Time
	jobManager        *jobs.Manager
	cursor            int
}

func New(store *store.Store, hub *agenthub.Hub, optimizer *optimizer.Service, routes *routing.Service, interval time.Duration) *Scheduler {
	return &Scheduler{store: store, hub: hub, optimizer: optimizer, routes: routes, interval: interval, inventoryInterval: 6 * time.Hour, rankingInterval: 5 * time.Minute, activeInterval: 30 * time.Second, maintenanceEvery: 5 * time.Minute}
}

func NewWithJobs(database *store.Store, hub *agenthub.Hub, optimizer *optimizer.Service, routes *routing.Service, manager *jobs.Manager, interval, inventoryInterval, rankingInterval, activeInterval time.Duration) *Scheduler {
	return &Scheduler{store: database, hub: hub, optimizer: optimizer, routes: routes, jobManager: manager, interval: interval, inventoryInterval: inventoryInterval, rankingInterval: rankingInterval, activeInterval: activeInterval, maintenanceEvery: 5 * time.Minute}
}

func (s *Scheduler) Run(ctx context.Context) {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	s.tick(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.tick(ctx)
		}
	}
}
func (s *Scheduler) tick(ctx context.Context) {
	var nodes []string
	if s.hub != nil {
		nodes = s.hub.ConnectedNodeIDs()
	}
	if err := s.dispatchTargets(nodes); err != nil {
		slog.Warn("route probe scheduling failed", "error", err)
	}
	if s.routes != nil {
		if err := s.evaluateRoutes(ctx); err != nil {
			slog.Warn("automatic route evaluation failed", "error", err)
		}
	}
	s.raiseOfflineAlerts()
	s.dispatchNotifications()
	if err := s.dispatchSources(ctx); err != nil {
		slog.Warn("candidate source scheduling failed", "error", err)
	}
	maintenanceNow := time.Now().UTC()
	if s.optimizer != nil && s.maintenanceDue(maintenanceNow) {
		if err := s.optimizer.AggregateAndRetain(ctx); err != nil {
			slog.Warn("probe aggregation and retention failed", "error", err)
		} else {
			s.lastMaintenance = maintenanceNow
		}
	}
	if s.hub == nil {
		return
	}
	if len(nodes) == 0 {
		return
	}
	if err := s.dispatchCandidates(nodes); err != nil {
		slog.Warn("candidate probe scheduling failed", "error", err)
	}
}

func (s *Scheduler) maintenanceDue(now time.Time) bool {
	interval := s.maintenanceEvery
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	return s.lastMaintenance.IsZero() || !now.Before(s.lastMaintenance.Add(interval))
}

func (s *Scheduler) evaluateRoutes(ctx context.Context) error {
	if s.routes == nil {
		return nil
	}
	if s.jobManager == nil {
		return s.routes.EvaluateAutomaticRoutes(ctx)
	}
	return s.routes.EvaluateAutomaticRoutesWith(ctx, func(routeID string) error {
		_, _, err := s.jobManager.Enqueue(jobs.EnqueueOptions{Type: "route.apply", ResourceType: "route", ResourceID: routeID, Payload: map[string]any{"actor": "scheduler", "ipAddress": "internal", "trigger": "health"}, IdempotencyKey: fmt.Sprintf("auto:%s:%d", routeID, time.Now().UTC().Unix()/60), MaxAttempts: 5})
		return err
	})
}

func (s *Scheduler) dispatchNotifications() {
	if s.jobManager == nil {
		return
	}
	var configured int64
	_ = s.store.DB.Model(&domain.Setting{}).Where("key IN ? AND value <> ?", []string{"alerts.webhook_url", "alerts.smtp_host"}, "").Count(&configured).Error
	if configured == 0 {
		return
	}
	var alerts []domain.Alert
	if err := s.store.DB.Where("status = ?", "open").Order("created_at asc").Limit(100).Find(&alerts).Error; err != nil {
		return
	}
	for _, alert := range alerts {
		_, _, _ = s.jobManager.Enqueue(jobs.EnqueueOptions{Type: "notification.deliver", ResourceType: "alert", ResourceID: alert.ID, Payload: map[string]any{"occurrences": alert.Occurrences}, IdempotencyKey: fmt.Sprintf("%s:%d", alert.ID, alert.Occurrences), MaxAttempts: 5})
	}
}

func (s *Scheduler) dispatchTargets(nodes []string) error {
	connected := make(map[string]bool, len(nodes))
	for _, nodeID := range nodes {
		connected[nodeID] = true
	}
	var routes []domain.Route
	if err := s.store.DB.Where("status <> ?", "disabled").Find(&routes).Error; err != nil {
		return err
	}
	for _, route := range routes {
		profile := s.profile(route.ProbeProfileID, route.ProjectID)
		site := s.routeSite(route)
		var lines []domain.RouteLine
		if err := s.store.DB.Where("route_id = ?", route.ID).Find(&lines).Error; err != nil {
			return err
		}
		for _, line := range lines {
			lineNodes := nodes
			if line.HealthPerspective == "matching_group" && line.NodeGroup != "" {
				var grouped []domain.ProbeNode
				if err := s.store.DB.Where("group_name = ?", line.NodeGroup).Find(&grouped).Error; err != nil {
					return err
				}
				lineNodes = make([]string, 0, len(grouped))
				for _, node := range grouped {
					if connected[node.ID] {
						lineNodes = append(lineNodes, node.ID)
					}
				}
			}
			if len(lineNodes) == 0 {
				s.updateSampleAlert(line, 0, maxInt(2, line.MinNodes))
				continue
			}
			minimum := maxInt(2, line.MinNodes)
			if len(lineNodes) < minimum {
				s.updateSampleAlert(line, len(lineNodes), minimum)
				continue
			}
			s.resolveSampleAlert(line.ID)
			if len(lineNodes) > 8 {
				lineNodes = lineNodes[:8]
			}
			var targets []domain.RouteTarget
			if err := s.store.DB.Where("route_line_id = ? AND enabled = ?", line.ID, true).Find(&targets).Error; err != nil {
				return err
			}
			for _, target := range targets {
				task := s.routeTask(route, target, profile, site)
				for _, node := range lineNodes {
					if s.taskPending(node, "", target.ID) {
						continue
					}
					_ = s.hub.Dispatch(node, task)
				}
			}
		}
	}
	return nil
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func (s *Scheduler) dispatchCandidates(nodes []string) error {
	var candidates []domain.Candidate
	now := time.Now().UTC()
	if err := s.store.DB.Where("tier IN ? AND (next_probe_at IS NULL OR next_probe_at <= ?)", []string{"inventory", "active", "ranking"}, now).Order("score desc").Limit(1000).Find(&candidates).Error; err != nil {
		return err
	}
	profiles := make(map[string]domain.ProbeProfile)
	projects := make(map[string]bool)
	for _, candidate := range candidates {
		profile := s.profileForProject(candidate.ProjectID, profiles)
		task := s.candidateTask(candidate, profile)
		node := nodes[s.cursor%len(nodes)]
		s.cursor++
		if !s.taskPending(node, candidate.ID, "") {
			_ = s.hub.Dispatch(node, task)
		}
		projects[candidate.ProjectID] = true
	}
	for projectID := range projects {
		go func(id string) {
			if err := s.optimizer.RecomputeScores(id); err != nil {
				slog.Warn("score recomputation failed", "error", err, "project", id)
			}
		}(projectID)
	}
	return nil
}

func (s *Scheduler) dispatchSources(ctx context.Context) error {
	var sources []domain.CandidateSource
	if err := s.store.DB.Where("enabled = ?", true).Find(&sources).Error; err != nil {
		return err
	}
	now := time.Now().UTC()
	for _, source := range sources {
		interval := s.durationSetting("probe.discovery_interval", s.inventoryInterval)
		if source.LastSyncedAt != nil && now.Sub(*source.LastSyncedAt) < interval {
			continue
		}
		key := fmt.Sprintf("%s:%d", source.ID, now.Unix()/int64(interval.Seconds()))
		if s.jobManager != nil {
			_, _, err := s.jobManager.Enqueue(jobs.EnqueueOptions{Type: "optimizer.source.sync", ResourceType: "candidate_source", ResourceID: source.ID, Payload: map[string]any{"sourceId": source.ID, "samplePerPrefix": 32}, IdempotencyKey: key, MaxAttempts: 3})
			if err != nil {
				return err
			}
			continue
		}
		if _, err := s.optimizer.SyncSource(ctx, source.ID, 32); err != nil {
			slog.Warn("candidate source sync failed", "source", source.ID, "error", err)
		}
	}
	return nil
}

func (s *Scheduler) durationSetting(key string, fallback time.Duration) time.Duration {
	if fallback <= 0 {
		fallback = 6 * time.Hour
	}
	var setting domain.Setting
	if s.store.DB.First(&setting, "key = ?", key).Error != nil {
		return fallback
	}
	value, err := time.ParseDuration(setting.Value)
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}

func (s *Scheduler) raiseOfflineAlerts() {
	now := time.Now().UTC()
	var nodes []domain.ProbeNode
	if err := s.store.DB.Where("status <> ?", "revoked").Find(&nodes).Error; err != nil {
		return
	}
	for _, node := range nodes {
		key := "agent.offline:" + node.ID
		offline := node.Status == "offline" || node.LastSeenAt == nil || now.Sub(*node.LastSeenAt) > 90*time.Second
		if !offline {
			s.resolveAlertByKey(key)
			continue
		}
		if node.Status != "offline" {
			_ = s.store.DB.Model(&domain.ProbeNode{}).Where("id = ? AND status <> ?", node.ID, "revoked").Update("status", "offline").Error
		}
		var existing domain.Alert
		if s.store.DB.Where("dedupe_key = ? AND status IN ?", key, []string{"open", "acknowledged", "silenced"}).First(&existing).Error == nil {
			s.reopenExpiredSilence(existing, now)
			continue
		}
		_ = s.store.DB.Create(&domain.Alert{Model: domain.Model{ID: uuidForAlert()}, Severity: "warning", Category: "agent", Title: "探测节点离线", Message: node.Name + " 超过 90 秒未上报心跳", ResourceType: "probe_node", ResourceID: node.ID, DedupeKey: key, Status: "open", Occurrences: 1, LastOccurredAt: &now}).Error
	}
}

func (s *Scheduler) updateSampleAlert(line domain.RouteLine, available, required int) {
	key := "route.samples:" + line.ID
	reason := fmt.Sprintf("当前只有 %d 个在线探测节点，需要至少 %d 个", available, required)
	_ = s.store.DB.Model(&domain.RouteLine{}).Where("id = ?", line.ID).Updates(map[string]any{"auto_switch_paused": true, "pause_reason": reason}).Error
	var existing domain.Alert
	if s.store.DB.Where("dedupe_key = ? AND status IN ?", key, []string{"open", "acknowledged", "silenced"}).First(&existing).Error == nil {
		s.reopenExpiredSilence(existing, time.Now().UTC())
		return
	}
	now := time.Now().UTC()
	message := fmt.Sprintf("%s%s；自动切换已暂停", line.Alias, reason)
	_ = s.store.DB.Create(&domain.Alert{Model: domain.Model{ID: uuidForAlert()}, Severity: "warning", Category: "route", Title: "线路探测样本不足", Message: message, ResourceType: "route_line", ResourceID: line.ID, DedupeKey: key, Status: "open", Occurrences: 1, LastOccurredAt: &now}).Error
}

func (s *Scheduler) resolveSampleAlert(lineID string) {
	_ = s.store.DB.Model(&domain.RouteLine{}).Where("id = ? AND auto_switch_paused = ?", lineID, true).Updates(map[string]any{"auto_switch_paused": false, "pause_reason": ""}).Error
	s.resolveAlertByKey("route.samples:" + lineID)
}

func (s *Scheduler) resolveAlertByKey(key string) {
	now := time.Now().UTC()
	_ = s.store.DB.Model(&domain.Alert{}).Where("dedupe_key = ? AND status IN ?", key, []string{"open", "acknowledged", "silenced"}).Updates(map[string]any{"status": "resolved", "resolved_at": now, "silenced_until": nil}).Error
}

func (s *Scheduler) reopenExpiredSilence(alert domain.Alert, now time.Time) {
	if alert.Status != "silenced" || (alert.SilencedUntil != nil && alert.SilencedUntil.After(now)) {
		return
	}
	occurrences := alert.Occurrences + 1
	if occurrences < 1 {
		occurrences = 1
	}
	_ = s.store.DB.Model(&alert).Updates(map[string]any{"status": "open", "silenced_until": nil, "occurrences": occurrences, "last_occurred_at": now}).Error
}

func (s *Scheduler) profile(id, projectID string) domain.ProbeProfile {
	var profile domain.ProbeProfile
	if id != "" && s.store.DB.First(&profile, "id = ?", id).Error == nil {
		return profile
	}
	return s.profileForProject(projectID, map[string]domain.ProbeProfile{})
}

func (s *Scheduler) profileForProject(projectID string, cache map[string]domain.ProbeProfile) domain.ProbeProfile {
	if profile, ok := cache[projectID]; ok {
		return profile
	}
	var profile domain.ProbeProfile
	if s.store.DB.Where("project_id = ? AND scope = ?", projectID, "optimizer").First(&profile).Error != nil {
		_ = s.store.DB.Where("scope = ?", "optimizer").First(&profile).Error
	}
	cache[projectID] = profile
	return profile
}

func (s *Scheduler) routeSite(route domain.Route) domain.ProjectSite {
	if route.SiteID == "" {
		return domain.ProjectSite{}
	}
	var site domain.ProjectSite
	_ = s.store.DB.First(&site, "id = ? AND project_id = ? AND status <> ?", route.SiteID, route.ProjectID, "disabled").Error
	return site
}

type probeOverrides struct {
	Mode             string            `json:"mode"`
	Port             int               `json:"port"`
	Method           string            `json:"method"`
	Headers          map[string]string `json:"headers"`
	ExpectedStatuses []int             `json:"expectedStatuses"`
	ExpectedContains string            `json:"expectedContains"`
	TimeoutMS        int               `json:"timeoutMs"`
	MaxBytes         int64             `json:"maxBytes"`
}

func (s *Scheduler) routeTask(route domain.Route, target domain.RouteTarget, profile domain.ProbeProfile, site domain.ProjectSite) agenthub.Task {
	rawURL := profile.URL
	if site.URL != "" {
		rawURL = site.URL
	}
	if rawURL == "" {
		rawURL = "https://" + route.Hostname + "/"
	}
	parsed, _ := url.Parse(rawURL)
	businessHost := route.Hostname
	if parsed != nil && parsed.Hostname() != "" {
		businessHost = parsed.Hostname()
	}
	task := agenthub.Task{RouteTargetID: target.ID, Mode: defaultMode(profile.Mode, rawURL), Address: target.CNAME, URL: rawURL, Host: businessHost, SNI: businessHost, Method: defaultMethod(profile.Method), Headers: parseHeaders(profile.HeadersJSON), ExpectedStatuses: parseStatuses(profile.ExpectedStatuses), ExpectedContains: profile.ExpectedContains, TimeoutMS: defaultInt(profile.TimeoutMS, 5000), MaxBytes: defaultInt64(profile.MaxBytes, 65536), AllowedPorts: []int{80, 443, 8080, 8443}, LeaseSeconds: 60}
	applyProbeOverrides(&task, site.ProbeOverridesJSON)
	applyProbeOverrides(&task, target.ProbeOverrideJSON)
	return task
}

func applyProbeOverrides(task *agenthub.Task, raw string) {
	if strings.TrimSpace(raw) == "" {
		return
	}
	var overrides probeOverrides
	if json.Unmarshal([]byte(raw), &overrides) != nil {
		return
	}
	if overrides.Mode == "tcp" || overrides.Mode == "http" {
		task.Mode = overrides.Mode
	}
	if overrides.Port > 0 {
		task.Port = overrides.Port
	}
	if overrides.Method != "" {
		task.Method = defaultMethod(overrides.Method)
	}
	if len(overrides.Headers) > 0 {
		if task.Headers == nil {
			task.Headers = make(map[string]string)
		}
		for key, value := range overrides.Headers {
			task.Headers[key] = value
		}
	}
	if len(overrides.ExpectedStatuses) > 0 {
		task.ExpectedStatuses = overrides.ExpectedStatuses
	}
	if overrides.ExpectedContains != "" {
		task.ExpectedContains = overrides.ExpectedContains
	}
	if overrides.TimeoutMS > 0 {
		task.TimeoutMS = overrides.TimeoutMS
	}
	if overrides.MaxBytes > 0 {
		task.MaxBytes = overrides.MaxBytes
	}
}

func (s *Scheduler) candidateTask(candidate domain.Candidate, profile domain.ProbeProfile) agenthub.Task {
	parsed, _ := url.Parse(profile.URL)
	host, sni := profile.Host, profile.SNI
	if host == "" && parsed != nil {
		host = parsed.Hostname()
	}
	if sni == "" {
		sni = host
	}
	return agenthub.Task{CandidateID: candidate.ID, Mode: defaultMode(profile.Mode, profile.URL), Address: candidate.Address, URL: profile.URL, Host: host, SNI: sni, Method: defaultMethod(profile.Method), Headers: parseHeaders(profile.HeadersJSON), ExpectedStatuses: parseStatuses(profile.ExpectedStatuses), ExpectedContains: profile.ExpectedContains, TimeoutMS: defaultInt(profile.TimeoutMS, 5000), MaxBytes: defaultInt64(profile.MaxBytes, 65536), AllowedPorts: []int{80, 443, 8080, 8443}, LeaseSeconds: 60}
}

func (s *Scheduler) taskPending(nodeID, candidateID, targetID string) bool {
	query := s.store.DB.Where("node_id = ? AND status IN ?", nodeID, []string{"queued", "sent", "acknowledged", "running"})
	if candidateID != "" {
		query = query.Where("candidate_id = ?", candidateID)
	} else {
		query = query.Where("route_target_id = ?", targetID)
	}
	var task domain.AgentTask
	return query.Order("created_at DESC").First(&task).Error == nil
}

func defaultMode(mode, rawURL string) string {
	if mode != "" {
		return mode
	}
	if rawURL != "" {
		return "http"
	}
	return "tcp"
}

func defaultMethod(method string) string {
	if strings.TrimSpace(method) == "" {
		return "GET"
	}
	return strings.ToUpper(method)
}

func parseHeaders(raw string) map[string]string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var headers map[string]string
	if json.Unmarshal([]byte(raw), &headers) != nil {
		return nil
	}
	return headers
}

func defaultInt(value, fallback int) int {
	if value <= 0 {
		return fallback
	}
	return value
}

func defaultInt64(value, fallback int64) int64 {
	if value <= 0 {
		return fallback
	}
	return value
}

func uuidForAlert() string {
	return fmt.Sprintf("alert-%d", time.Now().UTC().UnixNano())
}
func parseStatuses(raw string) []int {
	var values []int
	if strings.TrimSpace(raw) == "" {
		return []int{200, 204}
	}
	_ = json.Unmarshal([]byte(raw), &values)
	if len(values) > 0 {
		return values
	}
	for _, part := range strings.Split(raw, ",") {
		switch strings.TrimSpace(part) {
		case "200":
			values = append(values, 200)
		case "204":
			values = append(values, 204)
		case "301":
			values = append(values, 301)
		case "302":
			values = append(values, 302)
		}
	}
	return values
}
