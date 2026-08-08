package routing

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/SnowBall-Bqiu/NewNexus/internal/domain"
	"github.com/SnowBall-Bqiu/NewNexus/internal/events"
	"github.com/SnowBall-Bqiu/NewNexus/internal/provider"
	"github.com/SnowBall-Bqiu/NewNexus/internal/security"
	"github.com/SnowBall-Bqiu/NewNexus/internal/store"
)

type Service struct {
	store     *store.Store
	vault     *security.Vault
	providers *provider.Registry
	broker    *events.Broker
}

func New(store *store.Store, vault *security.Vault, providers *provider.Registry, broker *events.Broker) *Service {
	return &Service{store: store, vault: vault, providers: providers, broker: broker}
}

type RouteDetail struct {
	domain.Route
	Lines []LineDetail `json:"lines"`
}
type LineDetail struct {
	domain.RouteLine
	Targets []domain.RouteTarget `json:"targets"`
}
type Preview struct {
	RouteID  string                  `json:"routeId"`
	Hostname string                  `json:"hostname"`
	Provider string                  `json:"provider"`
	Changes  []provider.RecordChange `json:"changes"`
	Warnings []string                `json:"warnings"`
	CanApply bool                    `json:"canApply"`
}

// EvaluateAutomaticRoutes advances only priority-group changes. Weight values
// remain operator-owned, and a line with no healthy target keeps its DNS state.
func (s *Service) EvaluateAutomaticRoutes(ctx context.Context) error {
	var routes []domain.Route
	if err := s.store.DB.Where("publish_mode = ? AND status <> ?", "automatic", "disabled").Find(&routes).Error; err != nil {
		return err
	}
	now := time.Now().UTC()
	for _, route := range routes {
		if route.CooldownUntil != nil && route.CooldownUntil.After(now) {
			continue
		}
		detail, err := s.Detail(route.ID)
		if err != nil {
			continue
		}
		changed := false
		allFailed := true
		originalPriorities := make(map[string]int)
		switchedLineIDs := make([]string, 0)
		for _, lineDetail := range detail.Lines {
			line := lineDetail.RouteLine
			newestSample := newestTargetCheck(lineDetail.Targets)
			current := retainedTargetsAtPriority(lineDetail.Targets, line.ActivePriority)
			fallback := activeTargets(lineDetail.Targets)
			if len(current) == 0 && len(fallback) == 0 {
				if line.Status != "failed" {
					s.raiseAlert("critical", "route", line.Alias+"线路全部目标不可用", "已停止自动变更并保留当前 DNS 记录。", "route_line", line.ID)
				}
				_ = s.store.DB.Model(&line).Update("status", "failed").Error
				continue
			}
			allFailed = false
			if newestSample == nil || (line.LastEvaluatedAt != nil && !newestSample.After(*line.LastEvaluatedAt)) {
				continue
			}
			line.LastEvaluatedAt = newestSample
			nextPriority := line.ActivePriority
			if nextPriority == 0 || len(current) == 0 {
				nextPriority = fallback[0].Priority
			} else if recovered := lowestHealthyPriorityBelow(lineDetail.Targets, line.ActivePriority); recovered > 0 {
				nextPriority = recovered
			} else if candidate := preferredPriority(lineDetail.Targets, line.ActivePriority, route.SwitchMode); candidate > 0 {
				if line.PendingPriority == candidate {
					line.ConsecutiveBetter++
				} else {
					line.PendingPriority = candidate
					line.ConsecutiveBetter = 1
				}
				if line.ConsecutiveBetter >= 5 {
					nextPriority = candidate
					line.PendingPriority = 0
					line.ConsecutiveBetter = 0
				}
			} else {
				line.PendingPriority = 0
				line.ConsecutiveBetter = 0
			}
			if nextPriority != line.ActivePriority {
				originalPriorities[line.ID] = line.ActivePriority
				line.ActivePriority = nextPriority
				line.Status = "switching"
				switchedLineIDs = append(switchedLineIDs, line.ID)
				changed = true
			} else if line.Status != "healthy" {
				line.Status = "healthy"
			}
			_ = s.store.DB.Save(&line).Error
		}
		if allFailed {
			_ = s.store.DB.Model(&route).Update("status", "failed").Error
			continue
		}
		if changed {
			if _, err := s.Apply(ctx, route.ID, "health", "scheduler", "internal"); err != nil {
				for lineID, priority := range originalPriorities {
					_ = s.store.DB.Model(&domain.RouteLine{}).Where("id = ?", lineID).Updates(map[string]any{"active_priority": priority, "status": "degraded"}).Error
				}
				continue
			}
			_ = s.store.DB.Model(&domain.RouteLine{}).Where("id IN ?", switchedLineIDs).Update("status", "healthy").Error
		}
	}
	return nil
}

func (s *Service) Detail(routeID string) (RouteDetail, error) {
	var route domain.Route
	if err := s.store.DB.First(&route, "id = ?", routeID).Error; err != nil {
		return RouteDetail{}, err
	}
	var lines []domain.RouteLine
	if err := s.store.DB.Where("route_id = ?", routeID).Order("created_at asc").Find(&lines).Error; err != nil {
		return RouteDetail{}, err
	}
	detail := RouteDetail{Route: route, Lines: make([]LineDetail, 0, len(lines))}
	for _, line := range lines {
		var targets []domain.RouteTarget
		if err := s.store.DB.Where("route_line_id = ?", line.ID).Order("priority asc, weight desc").Find(&targets).Error; err != nil {
			return RouteDetail{}, err
		}
		detail.Lines = append(detail.Lines, LineDetail{RouteLine: line, Targets: targets})
	}
	return detail, nil
}

func (s *Service) Preview(ctx context.Context, routeID string) (Preview, error) {
	detail, err := s.Detail(routeID)
	if err != nil {
		return Preview{}, err
	}
	var connection domain.Connection
	if err := s.store.DB.First(&connection, "id = ?", detail.DNSConnectionID).Error; err != nil {
		return Preview{}, err
	}
	preview := Preview{RouteID: routeID, Hostname: detail.Hostname, Provider: connection.Provider, CanApply: connection.CredentialEnc != ""}
	if err := validateHostname(detail.Hostname, detail.ZoneName); err != nil {
		return Preview{}, err
	}
	for _, line := range detail.Lines {
		active := retainedTargetsAtPriority(line.Targets, line.ActivePriority)
		if len(active) == 0 {
			active = activeTargets(line.Targets)
		}
		if len(active) == 0 {
			preview.Warnings = append(preview.Warnings, line.Alias+"没有健康目标，将保持当前记录")
			continue
		}
		for _, target := range active {
			if err := validateCNAME(detail.Hostname, target.CNAME); err != nil {
				return Preview{}, fmt.Errorf("%s: %w", line.Alias, err)
			}
			record := provider.Record{Name: detail.Hostname, Type: "CNAME", Value: strings.TrimSuffix(target.CNAME, "."), LineCode: line.ProviderLineCode, TTL: detail.TTL, Weight: target.Weight, Enabled: true}
			preview.Changes = append(preview.Changes, provider.RecordChange{Operation: "create", After: &record})
		}
	}
	var customHostname domain.CustomHostname
	if err := s.store.DB.Where("hostname = ?", detail.Hostname).First(&customHostname).Error; err == nil && (customHostname.Status != "active" || customHostname.SSLStatus != "active") {
		preview.Warnings = append(preview.Warnings, "Cloudflare SaaS 主机名与证书必须同时为 active 才能切流")
		preview.CanApply = false
	}
	if !preview.CanApply {
		preview.Warnings = append(preview.Warnings, "连接未保存真实凭据，只能预览变更")
	}
	if connection.Provider == "cloudflare" {
		for _, line := range detail.Lines {
			if line.ProviderLineCode != "default" && line.ProviderLineCode != "0" {
				preview.Warnings = append(preview.Warnings, "Cloudflare DNS 不支持运营商线路，非默认线路不会发布")
				preview.CanApply = false
				break
			}
		}
	}
	return preview, nil
}

func (s *Service) Apply(ctx context.Context, routeID, trigger, actor, ipAddress string) (domain.Release, error) {
	preview, err := s.Preview(ctx, routeID)
	if err != nil {
		return domain.Release{}, err
	}
	var version int64
	_ = s.store.DB.Model(&domain.Release{}).Where("route_id = ?", routeID).Count(&version).Error
	diff, _ := json.Marshal(preview.Changes)
	release := domain.Release{RouteID: routeID, Version: int(version) + 1, Trigger: trigger, Status: "pending", DiffJSON: string(diff)}
	if !preview.CanApply {
		release.Status = "preview"
		release.ErrorMessage = strings.Join(preview.Warnings, "；")
		_ = s.store.DB.Create(&release).Error
		return release, fmt.Errorf("route cannot be applied: %s", release.ErrorMessage)
	}
	var route domain.Route
	_ = s.store.DB.First(&route, "id = ?", routeID).Error
	var connection domain.Connection
	if err := s.store.DB.First(&connection, "id = ?", route.DNSConnectionID).Error; err != nil {
		return release, err
	}
	adapter, ok := s.providers.Get(connection.Provider)
	if !ok {
		return release, fmt.Errorf("unknown provider %q", connection.Provider)
	}
	secretJSON, err := s.vault.Decrypt(connection.CredentialEnc)
	if err != nil {
		return release, err
	}
	credentials, err := provider.ParseCredentials(secretJSON)
	if err != nil {
		return release, err
	}
	zone := provider.Zone{ID: route.ZoneID, Name: route.ZoneName, Status: "active"}
	existing, err := adapter.ListRecords(ctx, credentials, zone, route.Hostname)
	if err != nil {
		return release, err
	}
	release.SnapshotJSON = mustJSON(existing)
	changes := reconcile(existing, preview.Changes)
	release.DiffJSON = mustJSON(changes)
	if err := s.store.DB.Create(&release).Error; err != nil {
		return release, err
	}
	if _, err := adapter.ApplyChanges(ctx, credentials, zone, changes); err != nil {
		release.Status = "failed"
		release.ErrorMessage = err.Error()
		_ = s.store.DB.Save(&release).Error
		s.raiseAlert("critical", "release", "线路发布失败", err.Error(), "route", route.ID)
		return release, err
	}
	now := time.Now().UTC()
	release.Status = "applied"
	release.AppliedAt = &now
	route.LastReleaseID = release.ID
	route.Status = "healthy"
	cooldown := now.Add(10 * time.Minute)
	route.CooldownUntil = &cooldown
	_ = s.store.DB.Save(&release).Error
	_ = s.store.DB.Save(&route).Error
	audit := domain.AuditEvent{Action: "route.publish", ResourceType: "route", ResourceID: route.ID, Summary: "已发布 " + route.Hostname + " 的线路变更", DetailsJSON: release.DiffJSON, Actor: actor, IPAddress: ipAddress}
	_ = s.store.DB.Create(&audit).Error
	s.publish("route.applied", route.ID, "线路变更已发布")
	return release, nil
}

func (s *Service) Rollback(ctx context.Context, releaseID, actor, ipAddress string) (domain.Release, error) {
	var previous domain.Release
	if err := s.store.DB.First(&previous, "id = ?", releaseID).Error; err != nil {
		return domain.Release{}, err
	}
	var snapshot []provider.Record
	if err := json.Unmarshal([]byte(previous.SnapshotJSON), &snapshot); err != nil || len(snapshot) == 0 {
		return domain.Release{}, fmt.Errorf("release has no verified snapshot")
	}
	var route domain.Route
	if err := s.store.DB.First(&route, "id = ?", previous.RouteID).Error; err != nil {
		return domain.Release{}, err
	}
	var connection domain.Connection
	if err := s.store.DB.First(&connection, "id = ?", route.DNSConnectionID).Error; err != nil {
		return domain.Release{}, err
	}
	adapter, ok := s.providers.Get(connection.Provider)
	if !ok {
		return domain.Release{}, fmt.Errorf("unknown provider")
	}
	secretJSON, err := s.vault.Decrypt(connection.CredentialEnc)
	if err != nil {
		return domain.Release{}, err
	}
	credentials, _ := provider.ParseCredentials(secretJSON)
	current, err := adapter.ListRecords(ctx, credentials, provider.Zone{ID: route.ZoneID, Name: route.ZoneName}, route.Hostname)
	if err != nil {
		return domain.Release{}, err
	}
	desired := make([]provider.RecordChange, 0, len(snapshot))
	for i := range snapshot {
		record := snapshot[i]
		desired = append(desired, provider.RecordChange{Operation: "create", After: &record})
	}
	changes := reconcile(current, desired)
	if _, err := adapter.ApplyChanges(ctx, credentials, provider.Zone{ID: route.ZoneID, Name: route.ZoneName}, changes); err != nil {
		return domain.Release{}, err
	}
	now := time.Now().UTC()
	previous.RolledBackAt = &now
	_ = s.store.DB.Save(&previous).Error
	rollback := domain.Release{RouteID: route.ID, Version: previous.Version + 1, Trigger: "rollback", Status: "applied", DiffJSON: mustJSON(changes), SnapshotJSON: mustJSON(current), AppliedAt: &now}
	_ = s.store.DB.Create(&rollback).Error
	audit := domain.AuditEvent{Action: "route.rollback", ResourceType: "route", ResourceID: route.ID, Summary: fmt.Sprintf("回滚至发布 #%d 前的记录", previous.Version), DetailsJSON: rollback.DiffJSON, Actor: actor, IPAddress: ipAddress}
	_ = s.store.DB.Create(&audit).Error
	s.publish("route.rolled_back", route.ID, "线路记录已回滚")
	return rollback, nil
}

func activeTargets(targets []domain.RouteTarget) []domain.RouteTarget {
	minimum := 0
	for _, target := range targets {
		if target.Enabled && target.Health == "healthy" && (minimum == 0 || target.Priority < minimum) {
			minimum = target.Priority
		}
	}
	if minimum == 0 {
		return nil
	}
	active := make([]domain.RouteTarget, 0)
	for _, target := range targets {
		if target.Enabled && target.Health == "healthy" && target.Priority == minimum {
			active = append(active, target)
		}
	}
	sort.Slice(active, func(i, j int) bool { return active[i].Weight > active[j].Weight })
	return active
}

func retainedTargetsAtPriority(targets []domain.RouteTarget, priority int) []domain.RouteTarget {
	if priority <= 0 {
		return nil
	}
	result := make([]domain.RouteTarget, 0)
	for _, target := range targets {
		if target.Enabled && target.Priority == priority && target.Health != "unhealthy" {
			result = append(result, target)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Weight > result[j].Weight })
	return result
}

func newestTargetCheck(targets []domain.RouteTarget) *time.Time {
	var newest *time.Time
	for _, target := range targets {
		if target.LastCheckedAt != nil && (newest == nil || target.LastCheckedAt.After(*newest)) {
			value := *target.LastCheckedAt
			newest = &value
		}
	}
	return newest
}

func lowestHealthyPriorityBelow(targets []domain.RouteTarget, current int) int {
	result := 0
	for _, target := range targets {
		if target.Enabled && target.Health == "healthy" && target.Priority < current && (result == 0 || target.Priority < result) {
			result = target.Priority
		}
	}
	return result
}

func preferredPriority(targets []domain.RouteTarget, current int, mode string) int {
	if mode == "availability" || current <= 0 {
		return 0
	}
	currentScore, ok := priorityScore(targets, current)
	if !ok {
		return 0
	}
	bestPriority, bestScore := 0, currentScore
	seen := make(map[int]bool)
	for _, target := range targets {
		if target.Priority == current || seen[target.Priority] {
			continue
		}
		seen[target.Priority] = true
		score, available := priorityScore(targets, target.Priority)
		if !available || score <= bestScore {
			continue
		}
		bestPriority, bestScore = target.Priority, score
	}
	if bestPriority == 0 {
		return 0
	}
	if mode == "availability_performance" && bestScore < currentScore*1.20 {
		return 0
	}
	return bestPriority
}

func priorityScore(targets []domain.RouteTarget, priority int) (float64, bool) {
	total, count := 0.0, 0
	for _, target := range targets {
		if target.Enabled && target.Health == "healthy" && target.Priority == priority {
			total += target.Score
			count++
		}
	}
	if count == 0 {
		return 0, false
	}
	return total / float64(count), true
}

func reconcile(existing []provider.Record, desired []provider.RecordChange) []provider.RecordChange {
	used := make(map[string]bool)
	result := make([]provider.RecordChange, 0, len(desired))
	for _, requested := range desired {
		if requested.After == nil {
			continue
		}
		matched := -1
		for index, record := range existing {
			if used[record.ID] {
				continue
			}
			if record.Type == requested.After.Type && record.Name == requested.After.Name && record.LineCode == requested.After.LineCode {
				matched = index
				break
			}
		}
		if matched < 0 {
			result = append(result, requested)
			continue
		}
		before := existing[matched]
		used[before.ID] = true
		after := *requested.After
		after.ID = before.ID
		if before.Value != after.Value || before.TTL != after.TTL || before.Weight != after.Weight {
			result = append(result, provider.RecordChange{Operation: "update", Before: &before, After: &after})
		}
	}
	return result
}

func validateHostname(hostname, zone string) error {
	if hostname == "" || zone == "" {
		return fmt.Errorf("hostname and zone are required")
	}
	if hostname == zone {
		return fmt.Errorf("apex CNAME requires an adapter capability that is not enabled")
	}
	if !strings.HasSuffix(hostname, "."+zone) {
		return fmt.Errorf("hostname is outside the selected DNS zone")
	}
	return nil
}
func validateCNAME(hostname, target string) error {
	target = strings.TrimSuffix(strings.ToLower(target), ".")
	if target == "" || net.ParseIP(target) != nil {
		return fmt.Errorf("CNAME target must be a hostname")
	}
	source := strings.TrimSuffix(strings.ToLower(hostname), ".")
	if source == target || strings.HasSuffix(target, "."+source) {
		return fmt.Errorf("CNAME loop detected")
	}
	return nil
}

func ValidateTarget(hostname, target string) error {
	return validateCNAME(hostname, target)
}
func mustJSON(value any) string { raw, _ := json.Marshal(value); return string(raw) }
func (s *Service) publish(kind, resource, message string) {
	raw, _ := json.Marshal(map[string]any{"type": kind, "resourceId": resource, "message": message, "at": time.Now().UTC()})
	s.broker.Publish(raw)
}
func (s *Service) raiseAlert(severity, category, title, message, resourceType, resourceID string) {
	alert := domain.Alert{Severity: severity, Category: category, Title: title, Message: message, ResourceType: resourceType, ResourceID: resourceID, Status: "open"}
	_ = s.store.DB.Create(&alert).Error
	s.publish("alert.created", alert.ID, title)
}
