package api

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/smtp"
	"sort"
	"strings"
	"time"

	"github.com/SnowBall-Bqiu/NewNexus/internal/dnsverify"
	"github.com/SnowBall-Bqiu/NewNexus/internal/domain"
	jobqueue "github.com/SnowBall-Bqiu/NewNexus/internal/jobs"
	"github.com/SnowBall-Bqiu/NewNexus/internal/netguard"
	"github.com/SnowBall-Bqiu/NewNexus/internal/provider"
	"gorm.io/gorm"
)

type jobPayload struct {
	Actor     string `json:"actor"`
	IPAddress string `json:"ipAddress"`
	Trigger   string `json:"trigger"`
	Sample    int    `json:"sample"`
	ReleaseID string `json:"releaseId"`
}

func (s *Server) registerJobHandlers() {
	s.jobs.Register("route.apply", func(ctx context.Context, job domain.Job, reporter jobqueue.Reporter) (any, error) {
		var payload jobPayload
		_ = json.Unmarshal([]byte(job.PayloadJSON), &payload)
		_ = reporter.Progress(10, map[string]string{"phase": "preview"})
		release, err := s.routes.Apply(ctx, job.ResourceID, defaultString(payload.Trigger, "manual"), payload.Actor, payload.IPAddress)
		if err != nil {
			return release, externalJobError("route_apply_failed", err)
		}
		return release, nil
	})
	s.jobs.Register("route.rollback", func(ctx context.Context, job domain.Job, reporter jobqueue.Reporter) (any, error) {
		var payload jobPayload
		_ = json.Unmarshal([]byte(job.PayloadJSON), &payload)
		_ = reporter.Progress(10, map[string]string{"phase": "restore_snapshot"})
		releaseID := defaultString(payload.ReleaseID, job.ResourceID)
		release, err := s.routes.Rollback(ctx, releaseID, payload.Actor, payload.IPAddress)
		if err != nil {
			return release, externalJobError("route_rollback_failed", err)
		}
		return release, nil
	})
	s.jobs.Register("optimizer.source.sync", func(ctx context.Context, job domain.Job, reporter jobqueue.Reporter) (any, error) {
		var payload jobPayload
		_ = json.Unmarshal([]byte(job.PayloadJSON), &payload)
		_ = reporter.Progress(10, map[string]string{"phase": "fetch"})
		count, err := s.optimizer.SyncSource(ctx, job.ResourceID, payload.Sample)
		if err != nil {
			return nil, externalJobError("source_sync_failed", err)
		}
		return map[string]int{"candidateCount": count}, nil
	})
	s.jobs.Register("optimizer.hostname.publish", func(ctx context.Context, job domain.Job, reporter jobqueue.Reporter) (any, error) {
		return s.executeOptimizedHostnamePublish(ctx, job.ResourceID, reporter)
	})
	s.jobs.Register("saas.zone.sync", func(ctx context.Context, job domain.Job, reporter jobqueue.Reporter) (any, error) {
		return s.executeSaaSZoneSync(ctx, job.ResourceID, reporter)
	})
	s.jobs.Register("saas.hostname.create", func(ctx context.Context, job domain.Job, reporter jobqueue.Reporter) (any, error) {
		return s.executeCustomHostnameCreate(ctx, job.ResourceID, reporter)
	})
	s.jobs.Register("saas.hostname.sync", func(ctx context.Context, job domain.Job, reporter jobqueue.Reporter) (any, error) {
		return s.executeCustomHostnameSync(ctx, job.ResourceID, reporter)
	})
	s.jobs.Register("saas.hostname.delete", func(ctx context.Context, job domain.Job, reporter jobqueue.Reporter) (any, error) {
		return s.executeCustomHostnameDelete(ctx, job.ResourceID, reporter)
	})
	s.jobs.Register("node.rotate", func(_ context.Context, job domain.Job, reporter jobqueue.Reporter) (any, error) {
		_ = reporter.Progress(10, map[string]string{"phase": "rotation_requested"})
		_, expiresAt, err := s.hub.RotateCredentials(job.ResourceID)
		if err != nil {
			return nil, &jobqueue.Error{Code: "node_rotation_failed", Message: err.Error(), Retryable: false}
		}
		var node domain.ProbeNode
		if err := s.store.DB.First(&node, "id = ?", job.ResourceID).Error; err != nil {
			return nil, &jobqueue.Error{Code: "node_not_found", Message: err.Error(), Retryable: false}
		}
		return map[string]any{"nodeId": node.ID, "status": node.Status, "rotationId": node.RotationID, "expiresAt": expiresAt}, nil
	})
	s.jobs.Register("notification.deliver", func(ctx context.Context, job domain.Job, reporter jobqueue.Reporter) (any, error) {
		return s.executeNotificationDelivery(ctx, job, reporter)
	})
	s.jobs.Register("notification.test", func(ctx context.Context, job domain.Job, reporter jobqueue.Reporter) (any, error) {
		alert := domain.Alert{Severity: "info", Category: "system", Title: "NewNexus 通知测试", Message: "通知渠道连接正常；这是一条由管理员主动发起的测试消息。", Status: "resolved", Occurrences: 1}
		return s.deliverNotification(ctx, job, reporter, alert)
	})
}

func (s *Server) executeNotificationDelivery(ctx context.Context, job domain.Job, reporter jobqueue.Reporter) (any, error) {
	var alert domain.Alert
	if err := s.store.DB.First(&alert, "id = ?", job.ResourceID).Error; err != nil {
		return nil, &jobqueue.Error{Code: "alert_not_found", Message: "alert no longer exists"}
	}
	return s.deliverNotification(ctx, job, reporter, alert)
}

func (s *Server) deliverNotification(ctx context.Context, job domain.Job, reporter jobqueue.Reporter, alert domain.Alert) (any, error) {
	payload, _ := json.Marshal(map[string]any{"eventId": job.ID, "type": "alert", "alert": alert, "sentAt": time.Now().UTC()})
	delivered := make([]string, 0, 2)
	if rawURL := strings.TrimSpace(s.settingValue("alerts.webhook_url")); rawURL != "" {
		_ = reporter.Progress(25, map[string]string{"phase": "webhook"})
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(payload))
		if err != nil {
			return nil, externalJobError("webhook_invalid", err)
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-NewNexus-Event-ID", job.ID)
		secret := s.settingValue("alerts.webhook_secret")
		if secret == "" {
			return nil, &jobqueue.Error{Code: "webhook_secret_missing", Message: "webhook HMAC secret is required"}
		}
		signature := hmac.New(sha256.New, []byte(secret))
		_, _ = signature.Write(payload)
		request.Header.Set("X-NewNexus-Signature", fmt.Sprintf("sha256=%x", signature.Sum(nil)))
		response, err := netguard.NewHTTPClient(15*time.Second, 0, 1).Do(request)
		if err != nil {
			s.recordDelivery(alert.ID, job.ID+":webhook", "webhook", "failed", err.Error())
			return nil, externalJobError("webhook_failed", err)
		}
		_ = response.Body.Close()
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			err := fmt.Errorf("webhook returned HTTP %d", response.StatusCode)
			s.recordDelivery(alert.ID, job.ID+":webhook", "webhook", "failed", err.Error())
			return nil, externalJobError("webhook_failed", err)
		}
		s.recordDelivery(alert.ID, job.ID+":webhook", "webhook", "delivered", "")
		delivered = append(delivered, "webhook")
	}
	if host := strings.TrimSpace(s.settingValue("alerts.smtp_host")); host != "" {
		_ = reporter.Progress(65, map[string]string{"phase": "smtp"})
		from, to := s.settingValue("alerts.smtp_from"), s.settingValue("alerts.smtp_to")
		if from == "" || to == "" {
			return nil, &jobqueue.Error{Code: "smtp_address_missing", Message: "SMTP sender and recipient are required"}
		}
		username, password := s.settingValue("alerts.smtp_username"), s.settingValue("alerts.smtp_password")
		hostname, _, err := net.SplitHostPort(host)
		if err != nil {
			return nil, &jobqueue.Error{Code: "smtp_host_invalid", Message: "SMTP host must include a port"}
		}
		var auth smtp.Auth
		if username != "" {
			auth = smtp.PlainAuth("", username, password, hostname)
		}
		message := []byte("To: " + to + "\r\nFrom: " + from + "\r\nSubject: [NewNexus] " + alert.Title + "\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\n" + alert.Message + "\r\nEvent-ID: " + job.ID + "\r\n")
		if err := smtp.SendMail(host, auth, from, []string{to}, message); err != nil {
			s.recordDelivery(alert.ID, job.ID+":smtp", "smtp", "failed", err.Error())
			return nil, externalJobError("smtp_failed", err)
		}
		s.recordDelivery(alert.ID, job.ID+":smtp", "smtp", "delivered", "")
		delivered = append(delivered, "smtp")
	}
	return map[string]any{"delivered": delivered, "eventId": job.ID}, nil
}

func (s *Server) recordDelivery(alertID, eventID, channelType, status, lastError string) {
	now := time.Now().UTC()
	channel := domain.NotificationChannel{Name: channelType, Type: channelType, Enabled: true}
	_ = s.store.DB.Where("type = ?", channelType).FirstOrCreate(&channel).Error
	delivery := domain.NotificationDelivery{AlertID: alertID, ChannelID: channel.ID, EventID: eventID, Status: status, Attempts: 1, LastError: lastError}
	if status == "delivered" {
		delivery.DeliveredAt = &now
	}
	_ = s.store.DB.Create(&delivery).Error
}

func (s *Server) enqueueHTTPJob(w http.ResponseWriter, r *http.Request, jobType, resourceType, resourceID string, payload any) (domain.Job, bool) {
	executionKey := ""
	if resourceType == "route" && (jobType == "route.apply" || jobType == "route.rollback") {
		executionKey = "route:" + resourceID
	}
	if resourceType == "probe_node" && jobType == "node.rotate" {
		executionKey = "node:" + resourceID
	}
	job, reused, err := s.jobs.Enqueue(jobqueue.EnqueueOptions{
		Type: jobType, ResourceType: resourceType, ResourceID: resourceID, Payload: payload,
		IdempotencyKey: strings.TrimSpace(r.Header.Get("Idempotency-Key")), ExecutionKey: executionKey, MaxAttempts: 5,
	})
	if err != nil {
		writeError(w, 500, "job_enqueue_failed", err.Error())
		return domain.Job{}, false
	}
	writeData(w, http.StatusAccepted, map[string]any{"jobId": job.ID, "job": job, "reused": reused})
	return job, true
}

func externalJobError(code string, err error) error {
	return &jobqueue.Error{Code: code, Message: err.Error(), Retryable: true}
}

func (s *Server) executeOptimizedHostnamePublish(ctx context.Context, id string, reporter jobqueue.Reporter) (any, error) {
	item, preview, adapter, credentials, zone, err := s.buildOptimizedHostnamePreview(ctx, id)
	if err != nil {
		return nil, &jobqueue.Error{Code: "preview_failed", Message: err.Error()}
	}
	if !preview.CanApply {
		return preview, &jobqueue.Error{Code: "publish_blocked", Message: strings.Join(preview.Warnings, "; ")}
	}
	_ = reporter.Progress(25, map[string]string{"phase": "provider_apply"})
	existing, err := adapter.ListRecords(ctx, credentials, zone, item.Hostname)
	if err != nil {
		return nil, externalJobError("provider_read_failed", err)
	}
	if len(preview.Changes) > 0 {
		if _, err := adapter.ApplyChanges(ctx, credentials, zone, preview.Changes); err != nil {
			_ = restoreAddressChanges(ctx, adapter, credentials, zone, item.Hostname, preview.Changes)
			return nil, externalJobError("provider_apply_failed", err)
		}
	}
	_ = reporter.Progress(60, map[string]string{"phase": "provider_verify"})
	observed, err := adapter.ListRecords(ctx, credentials, zone, item.Hostname)
	if err != nil || !containsAllAddresses(observed, preview.RecordType, preview.Addresses) {
		_ = restoreAddressChanges(ctx, adapter, credentials, zone, item.Hostname, preview.Changes)
		if err == nil {
			err = fmt.Errorf("provider API did not confirm all optimized addresses")
		}
		return nil, externalJobError("verification_failed", err)
	}
	desired := make([]provider.Record, 0, len(preview.Addresses))
	for _, address := range preview.Addresses {
		desired = append(desired, provider.Record{Name: item.Hostname, Type: preview.RecordType, Value: address, LineCode: "default", TTL: item.TTL, Weight: 100, Enabled: true})
	}
	_ = reporter.Progress(75, map[string]string{"phase": "authoritative_dns"})
	if err := dnsverify.New().Verify(ctx, zone.Name, desired); err != nil {
		_ = restoreAddressChanges(ctx, adapter, credentials, zone, item.Hostname, preview.Changes)
		return nil, externalJobError("authoritative_dns_failed", err)
	}
	now := time.Now().UTC()
	item.Status, item.LastError, item.LastPublishedAt, item.ObservedJSON = "active", "", &now, mustJSON(observed)
	if err := s.store.DB.Save(&item).Error; err != nil {
		return nil, err
	}
	if err := s.replaceManagedDNSRecords(item.DNSConnectionID, zone.ID, "optimized_hostname", item.ID, observed, desired, now); err != nil {
		return nil, err
	}
	_ = existing
	return map[string]any{"hostname": item, "preview": preview}, nil
}

func restoreAddressChanges(ctx context.Context, adapter provider.Adapter, credentials provider.Credentials, zone provider.Zone, hostname string, changes []provider.RecordChange) error {
	current, err := adapter.ListRecords(ctx, credentials, zone, hostname)
	if err != nil {
		return err
	}
	inverse := make([]provider.RecordChange, 0, len(changes))
	for index := len(changes) - 1; index >= 0; index-- {
		change := changes[index]
		switch change.Operation {
		case "create":
			if change.After != nil {
				if found := findEquivalentRecord(current, *change.After); found != nil {
					inverse = append(inverse, provider.RecordChange{Operation: "delete", Before: found})
				}
			}
		case "update":
			if change.Before != nil && change.After != nil {
				if found := findEquivalentRecord(current, *change.After); found != nil {
					before := *change.Before
					before.ID = found.ID
					inverse = append(inverse, provider.RecordChange{Operation: "update", Before: found, After: &before})
				}
			}
		case "delete":
			if change.Before != nil && findEquivalentRecord(current, *change.Before) == nil {
				before := *change.Before
				before.ID = ""
				inverse = append(inverse, provider.RecordChange{Operation: "create", After: &before})
			}
		}
	}
	if len(inverse) == 0 {
		return nil
	}
	_, err = adapter.ApplyChanges(ctx, credentials, zone, inverse)
	return err
}

func findEquivalentRecord(records []provider.Record, wanted provider.Record) *provider.Record {
	for _, record := range records {
		if normalizeDNSName(record.Name) == normalizeDNSName(wanted.Name) && strings.EqualFold(record.Type, wanted.Type) && normalizeDNSValue(record.Value, wanted.Type) == normalizeDNSValue(wanted.Value, wanted.Type) && normalizedProviderLine(record.LineCode) == normalizedProviderLine(wanted.LineCode) {
			copy := record
			return &copy
		}
	}
	return nil
}

func normalizedProviderLine(value string) string {
	if strings.TrimSpace(value) == "" {
		return "default"
	}
	return strings.ToLower(strings.TrimSpace(value))
}

func (s *Server) executeSaaSZoneSync(ctx context.Context, id string, reporter jobqueue.Reporter) (any, error) {
	var zone domain.SaaSZone
	if err := s.store.DB.First(&zone, "id = ?", id).Error; err != nil {
		return nil, &jobqueue.Error{Code: "zone_not_found", Message: "SaaS zone does not exist"}
	}
	connection, credentials, adapter, err := s.connectionCredentials(zone.ConnectionID)
	if err != nil {
		return nil, err
	}
	cloudflare, ok := adapter.(*provider.Cloudflare)
	if !ok || connection.Provider != "cloudflare" {
		return nil, &jobqueue.Error{Code: "provider_mismatch", Message: "SaaS zones require a Cloudflare connection"}
	}
	_ = reporter.Progress(15, map[string]string{"phase": "capabilities"})
	capability, err := cloudflare.Validate(ctx, credentials)
	if err != nil || !capability.SaaS {
		if err == nil {
			err = fmt.Errorf("Cloudflare for SaaS permission or quota API is unavailable")
		}
		zone.Status, zone.FallbackStatus = "unavailable", "unknown"
		_ = s.store.DB.Save(&zone).Error
		return zone, externalJobError("saas_unavailable", err)
	}
	zone.CapabilitiesJSON = mustJSON(capability)
	quota, err := cloudflare.CustomHostnameQuota(ctx, credentials, zone.ZoneID)
	if err != nil {
		return nil, externalJobError("quota_failed", err)
	}
	zone.QuotaAllocated = intAny(quota["allocated"])
	zone.QuotaUsed = intAny(quota["used"])
	zone.QuotaHardCap = intAny(quota["hard_cap"])
	zone.QuotaExceeded = boolAny(quota["exceeded"])
	_ = reporter.Progress(45, map[string]string{"phase": "fallback_origin"})
	if zone.FallbackOrigin != "" {
		if err := validateCloudflareOrigin(ctx, cloudflare, credentials, zone, zone.FallbackOrigin); err != nil {
			return nil, &jobqueue.Error{Code: "fallback_invalid", Message: err.Error()}
		}
		if _, err := cloudflare.UpdateFallbackOrigin(ctx, credentials, zone.ZoneID, zone.FallbackOrigin); err != nil {
			return nil, externalJobError("fallback_update_failed", err)
		}
	}
	fallback, err := cloudflare.GetFallbackOrigin(ctx, credentials, zone.ZoneID)
	if err != nil {
		return nil, externalJobError("fallback_read_failed", err)
	}
	zone.FallbackStatus = defaultString(stringAny(fallback["status"]), "unknown")
	if origin := stringAny(fallback["origin"]); origin != "" {
		zone.FallbackOrigin = origin
	}
	now := time.Now().UTC()
	zone.Status = map[bool]string{true: "active", false: "checking"}[zone.FallbackStatus == "active"]
	zone.ObservedJSON, zone.DesiredJSON, zone.LastSyncedAt, zone.DriftStatus = mustJSON(map[string]any{"quota": quota, "fallback": fallback}), mustJSON(map[string]any{"fallbackOrigin": zone.FallbackOrigin, "cnameTarget": zone.CNAMETarget}), &now, "in_sync"
	zone.ConfigFingerprint = hashJSON(zone.DesiredJSON)
	if err := s.store.DB.Save(&zone).Error; err != nil {
		return nil, err
	}
	return zone, nil
}

func (s *Server) executeCustomHostnameCreate(ctx context.Context, id string, reporter jobqueue.Reporter) (any, error) {
	item, zone, cloudflare, credentials, err := s.customHostnameContext(id)
	if err != nil {
		return nil, err
	}
	if item.CustomOrigin != "" {
		if err := validateCloudflareOrigin(ctx, cloudflare, credentials, zone, item.CustomOrigin); err != nil {
			return nil, &jobqueue.Error{Code: "custom_origin_invalid", Message: err.Error()}
		}
	}
	_ = reporter.Progress(20, map[string]string{"phase": "cloudflare_create"})
	result, err := cloudflare.CreateCustomHostname(ctx, credentials, zone.ZoneID, provider.CFCustomHostnameInput{Hostname: item.Hostname, SSL: map[string]any{"method": item.ValidationMethod, "type": "dv"}, CustomOriginServer: item.CustomOrigin})
	if err != nil {
		item.ErrorMessage = err.Error()
		_ = s.store.DB.Save(&item).Error
		return item, externalJobError("cloudflare_create_failed", err)
	}
	applyCloudflareCustomHostnameResult(&item, result)
	if item.AutomationMode == "automatic" {
		_ = reporter.Progress(55, map[string]string{"phase": "dns_validation_records"})
		if err := s.syncCustomHostnameDNS(ctx, &item, zone); err != nil {
			return s.pauseDNSAutomation(&item, result, err)
		}
	}
	now := time.Now().UTC()
	item.LastCheckedAt, item.ObservedJSON, item.DesiredJSON = &now, mustJSON(result), mustJSON(map[string]any{"hostname": item.Hostname, "customOrigin": item.CustomOrigin, "validationMethod": item.ValidationMethod})
	item.ConfigFingerprint, item.DriftStatus = hashJSON(item.DesiredJSON), "in_sync"
	if err := s.store.DB.Save(&item).Error; err != nil {
		return nil, err
	}
	return item, nil
}

func (s *Server) executeCustomHostnameSync(ctx context.Context, id string, reporter jobqueue.Reporter) (any, error) {
	item, zone, cloudflare, credentials, err := s.customHostnameContext(id)
	if err != nil {
		return nil, err
	}
	if item.ProviderID == "" {
		return s.executeCustomHostnameCreate(ctx, id, reporter)
	}
	_ = reporter.Progress(25, map[string]string{"phase": "cloudflare_status"})
	result, err := cloudflare.GetCustomHostname(ctx, credentials, zone.ZoneID, item.ProviderID)
	if err != nil {
		return nil, externalJobError("cloudflare_sync_failed", err)
	}
	applyCloudflareCustomHostnameResult(&item, result)
	item.ErrorMessage = ""
	if item.AutomationMode == "automatic" && !item.DNSAutomationPaused {
		_ = reporter.Progress(55, map[string]string{"phase": "dns_sync"})
		if err := s.syncCustomHostnameDNS(ctx, &item, zone); err != nil {
			return s.pauseDNSAutomation(&item, result, err)
		}
	}
	now := time.Now().UTC()
	item.LastCheckedAt, item.ObservedJSON = &now, mustJSON(result)
	if err := s.store.DB.Save(&item).Error; err != nil {
		return nil, err
	}
	return item, nil
}

func (s *Server) pauseDNSAutomation(item *domain.CustomHostname, cloudflareResult any, cause error) (any, error) {
	now := time.Now().UTC()
	item.DNSAutomationPaused = true
	item.DNSAutomationPauseReason = cause.Error()
	item.ErrorMessage = cause.Error()
	item.LastCheckedAt = &now
	item.ObservedJSON = mustJSON(cloudflareResult)
	item.DriftStatus = "unknown"
	if err := s.store.DB.Save(item).Error; err != nil {
		return nil, err
	}
	s.raiseAlert("critical", "saas", "SaaS 自动 DNS 已暂停", item.Hostname+": "+cause.Error(), "custom_hostname", item.ID)
	return *item, &jobqueue.Error{Code: "dns_automation_paused", Message: "自动 DNS 写入失败，已暂停自动化: " + cause.Error(), Retryable: false}
}

func (s *Server) raiseAlert(severity, category, title, message, resourceType, resourceID string) {
	now := time.Now().UTC()
	dedupe := category + ":" + resourceType + ":" + resourceID + ":" + title
	var existing domain.Alert
	if err := s.store.DB.Where("dedupe_key = ? AND status IN ?", dedupe, []string{"open", "acknowledged"}).First(&existing).Error; err == nil {
		_ = s.store.DB.Model(&existing).Updates(map[string]any{"message": message, "occurrences": existing.Occurrences + 1, "last_occurred_at": &now, "status": "open"}).Error
		return
	}
	alert := domain.Alert{Severity: severity, Category: category, Title: title, Message: message, ResourceType: resourceType, ResourceID: resourceID, DedupeKey: dedupe, Status: "open", Occurrences: 1, LastOccurredAt: &now}
	_ = s.store.DB.Create(&alert).Error
}

func (s *Server) executeCustomHostnameDelete(ctx context.Context, id string, reporter jobqueue.Reporter) (any, error) {
	item, zone, cloudflare, credentials, err := s.customHostnameContext(id)
	if err != nil {
		return nil, err
	}
	_ = reporter.Progress(15, map[string]string{"phase": "managed_dns_cleanup"})
	if err := s.cleanupManagedCustomHostnameDNS(ctx, item); err != nil {
		return nil, externalJobError("managed_dns_cleanup_failed", err)
	}
	_ = reporter.Progress(55, map[string]string{"phase": "cloudflare_delete"})
	if item.ProviderID != "" {
		if err := cloudflare.DeleteCustomHostname(ctx, credentials, zone.ZoneID, item.ProviderID); err != nil {
			return nil, externalJobError("cloudflare_delete_failed", err)
		}
	}
	if err := s.store.DB.Delete(&item).Error; err != nil {
		return nil, err
	}
	return map[string]any{"deleted": true, "hostname": item.Hostname}, nil
}

func (s *Server) cleanupManagedCustomHostnameDNS(ctx context.Context, item domain.CustomHostname) error {
	var managed []domain.ManagedDNSRecord
	if err := s.store.DB.Where("resource_type = ? AND resource_id = ?", "custom_hostname", item.ID).Find(&managed).Error; err != nil || len(managed) == 0 {
		return err
	}
	_, credentials, adapter, err := s.connectionCredentials(item.DNSConnectionID)
	if err != nil {
		return err
	}
	zone := provider.Zone{ID: item.DNSZoneID, Name: item.DNSZoneName, Status: "active"}
	changes := make([]provider.RecordChange, 0, len(managed))
	deletedRecords := make([]provider.Record, 0, len(managed))
	byName := make(map[string][]provider.Record)
	for _, ownership := range managed {
		name := normalizeDNSName(ownership.Name)
		if _, loaded := byName[name]; !loaded {
			records, readErr := adapter.ListRecords(ctx, credentials, zone, ownership.Name)
			if readErr != nil {
				return readErr
			}
			byName[name] = records
		}
		var current *provider.Record
		for index := range byName[name] {
			record := &byName[name][index]
			if ownership.ProviderRecordID != "" && record.ID == ownership.ProviderRecordID {
				current = record
				break
			}
		}
		if current == nil {
			continue
		}
		if normalizeDNSName(current.Name) != name || !strings.EqualFold(current.Type, ownership.Type) || normalizeDNSValue(current.Value, current.Type) != normalizeDNSValue(ownership.Value, ownership.Type) || normalizedProviderLine(current.LineCode) != normalizedProviderLine(ownership.LineCode) {
			return fmt.Errorf("受管 DNS 记录 %s 已发生漂移，拒绝自动删除", ownership.Name)
		}
		copy := *current
		changes = append(changes, provider.RecordChange{Operation: "delete", Before: &copy})
		deletedRecords = append(deletedRecords, copy)
	}
	if len(changes) > 0 {
		if _, err := adapter.ApplyChanges(ctx, credentials, zone, changes); err != nil {
			return err
		}
	}
	for _, ownership := range managed {
		records, err := adapter.ListRecords(ctx, credentials, zone, ownership.Name)
		if err != nil {
			return err
		}
		for _, record := range records {
			if record.ID == ownership.ProviderRecordID {
				return fmt.Errorf("厂商 API 仍返回已删除的受管记录 %s", ownership.Name)
			}
		}
	}
	if len(deletedRecords) > 0 {
		if err := dnsverify.New().VerifyAbsent(ctx, zone.Name, deletedRecords); err != nil {
			return err
		}
	}
	return s.store.DB.Where("resource_type = ? AND resource_id = ?", "custom_hostname", item.ID).Delete(&domain.ManagedDNSRecord{}).Error
}

func (s *Server) customHostnameContext(id string) (domain.CustomHostname, domain.SaaSZone, *provider.Cloudflare, provider.Credentials, error) {
	var item domain.CustomHostname
	if err := s.store.DB.First(&item, "id = ?", id).Error; err != nil {
		return item, domain.SaaSZone{}, nil, nil, &jobqueue.Error{Code: "hostname_not_found", Message: "custom hostname does not exist"}
	}
	var zone domain.SaaSZone
	if err := s.store.DB.First(&zone, "id = ?", item.SaaSZoneID).Error; err != nil {
		return item, zone, nil, nil, &jobqueue.Error{Code: "zone_not_found", Message: "SaaS zone does not exist"}
	}
	connection, credentials, adapter, err := s.connectionCredentials(zone.ConnectionID)
	if err != nil {
		return item, zone, nil, nil, err
	}
	cloudflare, ok := adapter.(*provider.Cloudflare)
	if !ok || connection.Provider != "cloudflare" {
		return item, zone, nil, nil, &jobqueue.Error{Code: "provider_mismatch", Message: "SaaS zone connection is not Cloudflare"}
	}
	return item, zone, cloudflare, credentials, nil
}

func (s *Server) connectionCredentials(id string) (domain.Connection, provider.Credentials, provider.Adapter, error) {
	var connection domain.Connection
	if err := s.store.DB.First(&connection, "id = ?", id).Error; err != nil {
		return connection, nil, nil, &jobqueue.Error{Code: "connection_not_found", Message: "connection does not exist"}
	}
	if connection.CredentialEnc == "" {
		return connection, nil, nil, &jobqueue.Error{Code: "credentials_required", Message: "connection credentials are missing"}
	}
	raw, err := s.vault.Decrypt(connection.CredentialEnc)
	if err != nil {
		return connection, nil, nil, err
	}
	credentials, err := provider.ParseCredentials(raw)
	if err != nil {
		return connection, nil, nil, err
	}
	adapter, ok := s.providers.Get(connection.Provider)
	if !ok {
		return connection, nil, nil, &jobqueue.Error{Code: "provider_unknown", Message: "provider adapter is unavailable"}
	}
	return connection, credentials, adapter, nil
}

func validateCloudflareOrigin(ctx context.Context, cloudflare *provider.Cloudflare, credentials provider.Credentials, _ domain.SaaSZone, origin string) error {
	origin = normalizeDNSName(origin)
	if origin == "" || net.ParseIP(origin) != nil || strings.Contains(origin, "*") {
		return fmt.Errorf("origin must be a concrete hostname, not an IP address or wildcard")
	}
	zones, err := cloudflare.ListZones(ctx, credentials)
	if err != nil {
		return err
	}
	for _, candidateZone := range zones {
		zoneName := normalizeDNSName(candidateZone.Name)
		if origin != zoneName && !strings.HasSuffix(origin, "."+zoneName) {
			continue
		}
		records, err := cloudflare.ListRecords(ctx, credentials, candidateZone, origin)
		if err != nil {
			return err
		}
		for _, record := range records {
			if normalizeDNSName(record.Name) == origin && record.Proxied && (strings.EqualFold(record.Type, "A") || strings.EqualFold(record.Type, "AAAA") || strings.EqualFold(record.Type, "CNAME")) {
				return nil
			}
		}
	}
	return fmt.Errorf("origin must have a proxied A, AAAA, or CNAME record in a zone on the connected Cloudflare account")
}

func (s *Server) replaceManagedDNSRecords(connectionID, zoneID, resourceType, resourceID string, observed, desired []provider.Record, verifiedAt time.Time) error {
	return s.store.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("resource_type = ? AND resource_id = ?", resourceType, resourceID).Delete(&domain.ManagedDNSRecord{}).Error; err != nil {
			return err
		}
		for _, record := range observed {
			if findEquivalentRecord(desired, record) == nil {
				continue
			}
			managed := domain.ManagedDNSRecord{ConnectionID: connectionID, ZoneID: zoneID, ResourceType: resourceType, ResourceID: resourceID, ProviderRecordID: record.ID, Name: record.Name, Type: record.Type, Value: record.Value, LineCode: record.LineCode, TTL: record.TTL, Weight: record.Weight, Fingerprint: hashJSON(record), ObservedJSON: mustJSON(record), LastVerifiedAt: &verifiedAt, DriftStatus: "in_sync"}
			if err := tx.Create(&managed).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

func defaultString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func intAny(value any) int {
	switch typed := value.(type) {
	case float64:
		return int(typed)
	case int:
		return typed
	case json.Number:
		parsed, _ := typed.Int64()
		return int(parsed)
	default:
		return 0
	}
}

func boolAny(value any) bool {
	result, _ := value.(bool)
	return result
}

func hashJSON(value any) string {
	raw, _ := json.Marshal(value)
	return fmt.Sprintf("%x", sha256Sum(raw))
}

func sha256Sum(value []byte) [32]byte {
	return sha256.Sum256(value)
}

func sortedStrings(values []string) []string {
	result := append([]string(nil), values...)
	sort.Strings(result)
	return result
}
