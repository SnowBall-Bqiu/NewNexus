package routing

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/SnowBall-Bqiu/NewNexus/internal/dnsverify"
	"github.com/SnowBall-Bqiu/NewNexus/internal/domain"
	"github.com/SnowBall-Bqiu/NewNexus/internal/events"
	"github.com/SnowBall-Bqiu/NewNexus/internal/provider"
	"github.com/SnowBall-Bqiu/NewNexus/internal/security"
	"github.com/SnowBall-Bqiu/NewNexus/internal/store"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type Service struct {
	store     *store.Store
	vault     *security.Vault
	providers *provider.Registry
	broker    *events.Broker
}

var errRollbackUnverified = errors.New("authoritative DNS rollback verification failed")

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
	RouteID            string                  `json:"routeId"`
	Hostname           string                  `json:"hostname"`
	Provider           string                  `json:"provider"`
	DesiredRecords     []provider.Record       `json:"desiredRecords"`
	Changes            []provider.RecordChange `json:"changes"`
	Warnings           []string                `json:"warnings"`
	UnmanagedConflicts []provider.Record       `json:"unmanagedConflicts"`
	Impact             int                     `json:"impact"`
	Risk               string                  `json:"risk"`
	CanApply           bool                    `json:"canApply"`
}

// EvaluateAutomaticRoutes advances only priority-group changes. Weight values
// remain operator-owned, and a line with no healthy target keeps its DNS state.
func (s *Service) EvaluateAutomaticRoutes(ctx context.Context) error {
	return s.EvaluateAutomaticRoutesWith(ctx, func(routeID string) error {
		_, err := s.Apply(ctx, routeID, "health", "scheduler", "internal")
		return err
	})
}

func (s *Service) EvaluateAutomaticRoutesWith(ctx context.Context, publish func(string) error) error {
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
			if line.AutoSwitchPaused {
				allFailed = false
				continue
			}
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
			if err := publish(route.ID); err != nil {
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
	preview := Preview{RouteID: routeID, Hostname: detail.Hostname, Provider: connection.Provider, CanApply: connection.CredentialEnc != "", Risk: "low"}
	adapter, ok := s.providers.Get(connection.Provider)
	if !ok {
		return Preview{}, fmt.Errorf("unknown provider %q", connection.Provider)
	}
	capability := capabilityFromConnection(connection)
	if normalizeName(detail.Hostname) == normalizeName(detail.ZoneName) && !capability.ApexCNAME && connection.CredentialEnc != "" {
		secretJSON, decryptErr := s.vault.Decrypt(connection.CredentialEnc)
		if decryptErr != nil {
			return Preview{}, decryptErr
		}
		credentials, parseErr := provider.ParseCredentials(secretJSON)
		if parseErr != nil {
			return Preview{}, parseErr
		}
		liveCapability, validationErr := adapter.Validate(ctx, credentials)
		if validationErr != nil {
			return Preview{}, fmt.Errorf("apex CNAME capability check failed: %w", validationErr)
		}
		capability = liveCapability
	}
	if err := validateHostnameCapability(detail.Hostname, detail.ZoneName, capability.ApexCNAME); err != nil {
		return Preview{}, err
	}
	if err := s.validateRouteLoops(detail); err != nil {
		return Preview{}, err
	}
	for _, line := range detail.Lines {
		active := retainedTargetsAtPriority(line.Targets, line.ActivePriority)
		if len(active) == 0 {
			active = activeTargets(line.Targets)
		}
		if len(active) == 0 {
			preview.Warnings = append(preview.Warnings, line.Alias+"没有健康目标，将保持当前记录")
			preview.CanApply = false
			continue
		}
		if !capability.Lines && line.ProviderLineCode != "default" && line.ProviderLineCode != "0" {
			preview.Warnings = append(preview.Warnings, line.Alias+"使用了厂商不支持的线路代码")
			preview.CanApply = false
			continue
		}
		if !capability.Weights && len(active) > 1 {
			preview.Warnings = append(preview.Warnings, line.Alias+"包含多个加权目标，但当前厂商套餐未确认支持权重")
			preview.CanApply = false
			continue
		}
		for _, target := range active {
			if err := validateCNAME(detail.Hostname, target.CNAME); err != nil {
				return Preview{}, fmt.Errorf("%s: %w", line.Alias, err)
			}
			record := provider.Record{Name: detail.Hostname, Type: "CNAME", Value: strings.TrimSuffix(target.CNAME, "."), LineCode: line.ProviderLineCode, TTL: detail.TTL, Weight: target.Weight, Enabled: true}
			preview.DesiredRecords = append(preview.DesiredRecords, record)
		}
	}
	var customHostname domain.CustomHostname
	if err := s.store.DB.Where("hostname = ?", detail.Hostname).First(&customHostname).Error; err == nil && (customHostname.Status != "active" || customHostname.SSLStatus != "active") {
		preview.Warnings = append(preview.Warnings, "Cloudflare SaaS 主机名与证书必须同时为 active 才能切流")
		preview.CanApply = false
	}
	if connection.CredentialEnc == "" {
		preview.Warnings = append(preview.Warnings, "连接未保存真实凭据，只能预览变更")
	}
	if len(preview.DesiredRecords) == 0 {
		preview.Warnings = append(preview.Warnings, "没有可发布的健康目标，必须保留当前 DNS 状态")
		preview.CanApply = false
		return preview, nil
	}
	if connection.CredentialEnc == "" {
		for index := range preview.DesiredRecords {
			record := preview.DesiredRecords[index]
			preview.Changes = append(preview.Changes, provider.RecordChange{Operation: "create", After: &record})
		}
		preview.Impact = len(preview.Changes)
		return preview, nil
	}
	secretJSON, err := s.vault.Decrypt(connection.CredentialEnc)
	if err != nil {
		return Preview{}, err
	}
	credentials, err := provider.ParseCredentials(secretJSON)
	if err != nil {
		return Preview{}, err
	}
	if capability.DNS == false {
		liveCapability, validationErr := adapter.Validate(ctx, credentials)
		if validationErr != nil {
			preview.Warnings = append(preview.Warnings, "厂商能力验证失败: "+validationErr.Error())
			preview.CanApply = false
		} else {
			capability = liveCapability
		}
	}
	existing, err := adapter.ListRecords(ctx, credentials, provider.Zone{ID: detail.ZoneID, Name: detail.ZoneName, Status: "active"}, detail.Hostname)
	if err != nil {
		return Preview{}, err
	}
	owned := s.ownedRecordIDs(detail.Route.ID, existing)
	preview.Changes, preview.UnmanagedConflicts = reconcileManaged(existing, preview.DesiredRecords, owned)
	if len(preview.UnmanagedConflicts) > 0 {
		preview.Warnings = append(preview.Warnings, "发现同名但不归 NewNexus 管理的 CNAME 记录，发布已阻止")
		preview.CanApply = false
	}
	preview.Impact = len(preview.Changes)
	if preview.Impact >= 20 || countDeletes(preview.Changes) >= 5 {
		preview.Risk = "high"
	} else if preview.Impact >= 5 || countDeletes(preview.Changes) > 0 {
		preview.Risk = "medium"
	}
	return preview, nil
}

func (s *Service) Apply(ctx context.Context, routeID, trigger, actor, ipAddress string) (domain.Release, error) {
	preview, err := s.Preview(ctx, routeID)
	if err != nil {
		return domain.Release{}, err
	}
	diff, _ := json.Marshal(preview.Changes)
	release := domain.Release{RouteID: routeID, Trigger: trigger, Status: "pending", DiffJSON: string(diff)}
	if !preview.CanApply {
		release.Status = "preview"
		release.ErrorMessage = strings.Join(preview.Warnings, "；")
		_ = s.createRelease(&release)
		return release, fmt.Errorf("route cannot be applied: %s", release.ErrorMessage)
	}
	var route domain.Route
	if err := s.store.DB.First(&route, "id = ?", routeID).Error; err != nil {
		return release, err
	}
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
	release.DiffJSON = mustJSON(preview.Changes)
	if err := s.createRelease(&release); err != nil {
		return release, err
	}
	if len(preview.Changes) > 0 {
		if _, err := adapter.ApplyChanges(ctx, credentials, zone, preview.Changes); err != nil {
			return s.failAndRestore(ctx, release, route, adapter, credentials, zone, existing, preview.DesiredRecords, fmt.Errorf("provider apply failed: %w", err))
		}
	}
	observed, err := adapter.ListRecords(ctx, credentials, zone, route.Hostname)
	if err != nil {
		return s.failAndRestore(ctx, release, route, adapter, credentials, zone, existing, preview.DesiredRecords, fmt.Errorf("provider readback failed: %w", err))
	}
	if err := verifyDesiredRecords(observed, preview.DesiredRecords); err != nil {
		return s.failAndRestore(ctx, release, route, adapter, credentials, zone, existing, preview.DesiredRecords, fmt.Errorf("provider verification failed: %w", err))
	}
	if err := dnsverify.New().Verify(ctx, route.ZoneName, preview.DesiredRecords); err != nil {
		return s.failAndRestore(ctx, release, route, adapter, credentials, zone, existing, preview.DesiredRecords, fmt.Errorf("authoritative DNS verification failed: %w", err))
	}
	now := time.Now().UTC()
	release.Status = "applied"
	release.AppliedAt = &now
	route.LastReleaseID = release.ID
	route.Status = "healthy"
	route.ObservedJSON = mustJSON(observed)
	route.DesiredJSON = mustJSON(preview.DesiredRecords)
	route.ConfigFingerprint = fingerprint(preview.DesiredRecords)
	route.DriftStatus = "in_sync"
	route.LastSyncedAt = &now
	cooldown := now.Add(10 * time.Minute)
	route.CooldownUntil = &cooldown
	_ = s.store.DB.Save(&release).Error
	_ = s.store.DB.Save(&route).Error
	if err := s.replaceManagedRecords(connection.ID, zone.ID, "route", route.ID, observed, preview.DesiredRecords, now); err != nil {
		s.raiseAlert("warning", "drift", "DNS 所有权状态保存失败", err.Error(), "route", route.ID)
	}
	audit := domain.AuditEvent{Action: "route.publish", ResourceType: "route", ResourceID: route.ID, Summary: "已发布 " + route.Hostname + " 的线路变更", DetailsJSON: release.DiffJSON, Actor: actor, IPAddress: ipAddress}
	_ = s.store.DB.Create(&audit).Error
	s.publish("route.applied", route.ID, "线路变更已发布")
	return release, nil
}

func (s *Service) failAndRestore(ctx context.Context, release domain.Release, route domain.Route, adapter provider.Adapter, credentials provider.Credentials, zone provider.Zone, snapshot, attempted []provider.Record, cause error) (domain.Release, error) {
	restoreErr := s.restoreSnapshot(ctx, route, adapter, credentials, zone, snapshot, attempted)
	release.Status = "failed"
	release.ErrorMessage = cause.Error()
	if restoreErr != nil {
		if errors.Is(restoreErr, errRollbackUnverified) {
			release.Status = "rollback_unverified"
		}
		release.ErrorMessage += "; automatic rollback failed: " + restoreErr.Error()
		s.raiseAlert("critical", "release", "线路发布和自动回滚均失败", release.ErrorMessage, "route", route.ID)
	} else {
		s.raiseAlert("critical", "release", "线路发布失败，已自动回滚", cause.Error(), "route", route.ID)
	}
	_ = s.store.DB.Save(&release).Error
	_ = s.store.DB.Model(&route).Updates(map[string]any{"status": "degraded", "drift_status": "unknown"}).Error
	return release, cause
}

func (s *Service) restoreSnapshot(ctx context.Context, route domain.Route, adapter provider.Adapter, credentials provider.Credentials, zone provider.Zone, snapshot, attempted []provider.Record) error {
	current, err := adapter.ListRecords(ctx, credentials, zone, route.Hostname)
	if err != nil {
		return err
	}
	owned := s.ownedRecordIDs(route.ID, current)
	for _, record := range current {
		if matchesAnyDesired(record, attempted) {
			owned[record.ID] = true
		}
	}
	desired := desiredFromSnapshotOrRoute(snapshot, route.Hostname)
	changes, conflicts := reconcileManaged(current, desired, owned)
	if len(conflicts) > 0 {
		return fmt.Errorf("rollback is blocked by %d unmanaged record conflicts", len(conflicts))
	}
	if len(changes) > 0 {
		if _, err := adapter.ApplyChanges(ctx, credentials, zone, changes); err != nil {
			return err
		}
	}
	observed, err := adapter.ListRecords(ctx, credentials, zone, route.Hostname)
	if err != nil {
		return err
	}
	if err := verifyDesiredRecords(observed, desired); err != nil {
		return err
	}
	if err := dnsverify.New().Verify(ctx, route.ZoneName, desired); err != nil {
		return fmt.Errorf("%w: %v", errRollbackUnverified, err)
	}
	return nil
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
	zone := provider.Zone{ID: route.ZoneID, Name: route.ZoneName}
	if err := s.restoreSnapshot(ctx, route, adapter, credentials, zone, snapshot, nil); err != nil {
		return domain.Release{}, err
	}
	observed, err := adapter.ListRecords(ctx, credentials, zone, route.Hostname)
	if err != nil {
		return domain.Release{}, err
	}
	desired := desiredFromSnapshotOrRoute(snapshot, route.Hostname)
	owned := s.ownedRecordIDs(route.ID, current)
	changes, _ := reconcileManaged(current, desired, owned)
	if err := dnsverify.New().Verify(ctx, route.ZoneName, desired); err != nil {
		return domain.Release{}, fmt.Errorf("authoritative DNS rollback verification failed: %w", err)
	}
	now := time.Now().UTC()
	previous.RolledBackAt = &now
	_ = s.store.DB.Save(&previous).Error
	rollback := domain.Release{RouteID: route.ID, Trigger: "rollback", Status: "applied", DiffJSON: mustJSON(changes), SnapshotJSON: mustJSON(current), AppliedAt: &now}
	if err := s.createRelease(&rollback); err != nil {
		return domain.Release{}, err
	}
	_ = s.replaceManagedRecords(connection.ID, zone.ID, "route", route.ID, observed, s.rollbackManagedDesired(route.ID, previous, snapshot), now)
	audit := domain.AuditEvent{Action: "route.rollback", ResourceType: "route", ResourceID: route.ID, Summary: fmt.Sprintf("回滚至发布 #%d 前的记录", previous.Version), DetailsJSON: rollback.DiffJSON, Actor: actor, IPAddress: ipAddress}
	_ = s.store.DB.Create(&audit).Error
	s.publish("route.rolled_back", route.ID, "线路记录已回滚")
	return rollback, nil
}

func (s *Service) createRelease(release *domain.Release) error {
	return s.store.DB.Transaction(func(tx *gorm.DB) error {
		var route domain.Route
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Select("id").First(&route, "id = ?", release.RouteID).Error; err != nil {
			return err
		}
		var version int
		if err := tx.Model(&domain.Release{}).Where("route_id = ?", release.RouteID).Select("COALESCE(MAX(version), 0)").Scan(&version).Error; err != nil {
			return err
		}
		release.Version = version + 1
		return tx.Create(release).Error
	})
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
	records := make([]provider.Record, 0, len(desired))
	for _, change := range desired {
		if change.After != nil {
			records = append(records, *change.After)
		}
	}
	owned := make(map[string]bool, len(existing))
	for _, record := range existing {
		owned[record.ID] = true
	}
	changes, _ := reconcileManaged(existing, records, owned)
	return changes
}

func validateHostname(hostname, zone string) error {
	return validateHostnameCapability(hostname, zone, false)
}

func validateHostnameCapability(hostname, zone string, apexCNAME bool) error {
	hostname = normalizeName(hostname)
	zone = normalizeName(zone)
	if hostname == "" || zone == "" {
		return fmt.Errorf("hostname and zone are required")
	}
	if hostname == zone && !apexCNAME {
		return fmt.Errorf("apex CNAME requires an adapter capability that is not enabled")
	}
	if hostname != zone && !strings.HasSuffix(hostname, "."+zone) {
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

func capabilityFromConnection(connection domain.Connection) provider.Capability {
	var capability provider.Capability
	if json.Unmarshal([]byte(connection.Capabilities), &capability) == nil && (capability.DNS || capability.SaaS || capability.Lines) {
		return capability
	}
	var names []string
	_ = json.Unmarshal([]byte(connection.Capabilities), &names)
	for _, name := range names {
		switch name {
		case "dns":
			capability.DNS = true
		case "lines":
			capability.Lines = true
		case "weights":
			capability.Weights = true
		case "saas":
			capability.SaaS = true
		}
	}
	return capability
}

func reconcileManaged(existing, desired []provider.Record, owned map[string]bool) ([]provider.RecordChange, []provider.Record) {
	used := make(map[int]bool)
	changes := make([]provider.RecordChange, 0)
	conflicts := make([]provider.Record, 0)
	for _, wanted := range desired {
		exact := -1
		for index, current := range existing {
			if used[index] || !sameRecordIdentity(current, wanted) {
				continue
			}
			if normalizeName(current.Value) == normalizeName(wanted.Value) {
				exact = index
				break
			}
		}
		if exact >= 0 {
			used[exact] = true
			current := existing[exact]
			if current.TTL != wanted.TTL || current.Weight != wanted.Weight || current.Enabled != wanted.Enabled || current.Proxied {
				if owned[current.ID] || current.Managed {
					after := wanted
					after.ID = current.ID
					changes = append(changes, provider.RecordChange{Operation: "update", Before: recordPtr(current), After: recordPtr(after)})
				} else {
					conflicts = append(conflicts, current)
				}
			}
			continue
		}
		managedMatch := -1
		unmanagedMatch := -1
		for index, current := range existing {
			if used[index] || !sameRecordIdentity(current, wanted) {
				continue
			}
			if owned[current.ID] || current.Managed {
				managedMatch = index
				break
			}
			unmanagedMatch = index
		}
		if managedMatch >= 0 {
			used[managedMatch] = true
			before := existing[managedMatch]
			after := wanted
			after.ID = before.ID
			changes = append(changes, provider.RecordChange{Operation: "update", Before: recordPtr(before), After: recordPtr(after)})
			continue
		}
		if unmanagedMatch >= 0 {
			used[unmanagedMatch] = true
			conflicts = append(conflicts, existing[unmanagedMatch])
			continue
		}
		created := wanted
		changes = append(changes, provider.RecordChange{Operation: "create", After: &created})
	}
	for index, current := range existing {
		if used[index] || !strings.EqualFold(current.Type, "CNAME") {
			continue
		}
		if owned[current.ID] || current.Managed {
			before := current
			changes = append(changes, provider.RecordChange{Operation: "delete", Before: &before})
		} else if len(desired) > 0 && normalizeName(current.Name) == normalizeName(desired[0].Name) {
			conflicts = append(conflicts, current)
		}
	}
	return changes, conflicts
}

func sameRecordIdentity(a, b provider.Record) bool {
	return strings.EqualFold(a.Type, b.Type) && normalizeName(a.Name) == normalizeName(b.Name) && normalizedLine(a.LineCode) == normalizedLine(b.LineCode)
}

func normalizedLine(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "" {
		return "default"
	}
	return value
}

func normalizeName(value string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(value)), ".")
}
func recordPtr(value provider.Record) *provider.Record {
	copy := value
	return &copy
}

func countDeletes(changes []provider.RecordChange) int {
	count := 0
	for _, change := range changes {
		if change.Operation == "delete" {
			count++
		}
	}
	return count
}

func verifyDesiredRecords(observed, desired []provider.Record) error {
	for _, wanted := range desired {
		found := false
		for _, current := range observed {
			if sameRecordIdentity(current, wanted) && normalizeName(current.Value) == normalizeName(wanted.Value) && !current.Proxied {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("record %s %s %s on line %s was not confirmed", wanted.Name, wanted.Type, wanted.Value, wanted.LineCode)
		}
	}
	return nil
}

func matchesAnyDesired(record provider.Record, desired []provider.Record) bool {
	for _, wanted := range desired {
		if sameRecordIdentity(record, wanted) && normalizeName(record.Value) == normalizeName(wanted.Value) {
			return true
		}
	}
	return false
}

func desiredFromSnapshotOrRoute(snapshot []provider.Record, hostname string) []provider.Record {
	result := make([]provider.Record, 0)
	for _, record := range snapshot {
		if normalizeName(record.Name) == normalizeName(hostname) && strings.EqualFold(record.Type, "CNAME") {
			result = append(result, record)
		}
	}
	return result
}

func fingerprint(records []provider.Record) string {
	copyRecords := append([]provider.Record(nil), records...)
	sort.Slice(copyRecords, func(i, j int) bool {
		left := copyRecords[i].Name + "|" + copyRecords[i].Type + "|" + copyRecords[i].LineCode + "|" + copyRecords[i].Value
		right := copyRecords[j].Name + "|" + copyRecords[j].Type + "|" + copyRecords[j].LineCode + "|" + copyRecords[j].Value
		return left < right
	})
	raw, _ := json.Marshal(copyRecords)
	sum := sha256.Sum256(raw)
	return fmt.Sprintf("%x", sum[:])
}

func (s *Service) ownedRecordIDs(routeID string, existing []provider.Record) map[string]bool {
	owned := make(map[string]bool)
	var records []domain.ManagedDNSRecord
	_ = s.store.DB.Where("resource_type = ? AND resource_id = ?", "route", routeID).Find(&records).Error
	for _, record := range records {
		owned[record.ProviderRecordID] = true
	}
	for _, record := range existing {
		if record.Managed {
			owned[record.ID] = true
		}
	}
	var release domain.Release
	if err := s.store.DB.Where("route_id = ? AND status = ?", routeID, "applied").Order("version DESC").First(&release).Error; err == nil {
		var changes []provider.RecordChange
		if json.Unmarshal([]byte(release.DiffJSON), &changes) == nil {
			for _, change := range changes {
				for _, proof := range []*provider.Record{change.Before, change.After} {
					if proof == nil {
						continue
					}
					for _, record := range existing {
						if sameRecordIdentity(record, *proof) && normalizeName(record.Value) == normalizeName(proof.Value) {
							owned[record.ID] = true
						}
					}
				}
			}
		}
	}
	return owned
}

func (s *Service) rollbackManagedDesired(routeID string, release domain.Release, snapshot []provider.Record) []provider.Record {
	proofs := make([]provider.Record, 0)
	var changes []provider.RecordChange
	if json.Unmarshal([]byte(release.DiffJSON), &changes) == nil {
		for _, change := range changes {
			if change.Before != nil {
				proofs = append(proofs, *change.Before)
			}
		}
	}
	var managed []domain.ManagedDNSRecord
	_ = s.store.DB.Where("resource_type = ? AND resource_id = ?", "route", routeID).Find(&managed).Error
	for _, item := range managed {
		proofs = append(proofs, provider.Record{Name: item.Name, Type: item.Type, Value: item.Value, LineCode: item.LineCode})
	}
	result := make([]provider.Record, 0)
	for _, record := range snapshot {
		if matchesAnyDesired(record, proofs) {
			result = append(result, record)
		}
	}
	return result
}

func (s *Service) replaceManagedRecords(connectionID, zoneID, resourceType, resourceID string, observed, desired []provider.Record, verifiedAt time.Time) error {
	return s.store.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("resource_type = ? AND resource_id = ?", resourceType, resourceID).Delete(&domain.ManagedDNSRecord{}).Error; err != nil {
			return err
		}
		for _, record := range observed {
			if !matchesAnyDesired(record, desired) {
				continue
			}
			managed := domain.ManagedDNSRecord{
				ConnectionID: connectionID, ZoneID: zoneID, ResourceType: resourceType, ResourceID: resourceID,
				ProviderRecordID: record.ID, Name: record.Name, Type: record.Type, Value: record.Value, LineCode: record.LineCode,
				TTL: record.TTL, Weight: record.Weight, Fingerprint: fingerprint([]provider.Record{record}), ObservedJSON: mustJSON(record),
				LastVerifiedAt: &verifiedAt, DriftStatus: "in_sync",
			}
			if err := tx.Create(&managed).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Service) validateRouteLoops(current RouteDetail) error {
	graph := make(map[string][]string)
	var routes []domain.Route
	if err := s.store.DB.Find(&routes).Error; err != nil {
		return err
	}
	for _, route := range routes {
		var lines []domain.RouteLine
		if err := s.store.DB.Where("route_id = ?", route.ID).Find(&lines).Error; err != nil {
			return err
		}
		for _, line := range lines {
			var targets []domain.RouteTarget
			if err := s.store.DB.Where("route_line_id = ? AND enabled = ?", line.ID, true).Find(&targets).Error; err != nil {
				return err
			}
			for _, target := range targets {
				graph[normalizeName(route.Hostname)] = append(graph[normalizeName(route.Hostname)], normalizeName(target.CNAME))
			}
		}
	}
	graph[normalizeName(current.Hostname)] = nil
	for _, line := range current.Lines {
		for _, target := range line.Targets {
			if target.Enabled {
				graph[normalizeName(current.Hostname)] = append(graph[normalizeName(current.Hostname)], normalizeName(target.CNAME))
			}
		}
	}
	visiting, visited := make(map[string]bool), make(map[string]bool)
	var visit func(string) bool
	visit = func(node string) bool {
		if visiting[node] {
			return true
		}
		if visited[node] {
			return false
		}
		visiting[node] = true
		for _, next := range graph[node] {
			if _, managed := graph[next]; managed && visit(next) {
				return true
			}
		}
		visiting[node] = false
		visited[node] = true
		return false
	}
	if visit(normalizeName(current.Hostname)) {
		return fmt.Errorf("multi-hop CNAME loop detected for %s", current.Hostname)
	}
	return nil
}
func mustJSON(value any) string { raw, _ := json.Marshal(value); return string(raw) }
func (s *Service) publish(kind, resource, message string) {
	raw, _ := json.Marshal(map[string]any{"type": kind, "resourceId": resource, "message": message, "at": time.Now().UTC()})
	s.broker.Publish(raw)
}
func (s *Service) raiseAlert(severity, category, title, message, resourceType, resourceID string) {
	dedupeKey := category + ":" + resourceType + ":" + resourceID + ":" + title
	now := time.Now().UTC()
	var existing domain.Alert
	if err := s.store.DB.Where("dedupe_key = ? AND status IN ?", dedupeKey, []string{"open", "acknowledged", "silenced"}).First(&existing).Error; err == nil {
		occurrences := existing.Occurrences + 1
		if occurrences < 1 {
			occurrences = 1
		}
		updates := map[string]any{"message": message, "occurrences": occurrences, "last_occurred_at": now}
		if existing.Status != "silenced" || existing.SilencedUntil == nil || !existing.SilencedUntil.After(now) {
			updates["status"] = "open"
			updates["silenced_until"] = nil
		}
		_ = s.store.DB.Model(&existing).Updates(updates).Error
		s.publish("alert.updated", existing.ID, title)
		return
	}
	alert := domain.Alert{Severity: severity, Category: category, Title: title, Message: message, ResourceType: resourceType, ResourceID: resourceID, DedupeKey: dedupeKey, Status: "open", Occurrences: 1, LastOccurredAt: &now}
	_ = s.store.DB.Create(&alert).Error
	s.publish("alert.created", alert.ID, title)
}
