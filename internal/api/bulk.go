package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/newnexus/newnexus/internal/auth"
	"github.com/newnexus/newnexus/internal/domain"
	"github.com/newnexus/newnexus/internal/security"
	"github.com/newnexus/newnexus/internal/store"
)

// bulk runs after authentication/CSRF checks. Each accepted item is durable and
// owns an independent job; one provider failure does not cancel its neighbours.
func (s *Server) bulk(w http.ResponseWriter, r *http.Request, path string, session domain.Doc) bool {
	if path != "saas/hostnames/bulk" && path != "saas/hostnames/bulk-delete" {
		return false
	}
	if r.Method != http.MethodPost {
		fail(w, 405, "method_not_allowed", errors.New("bulk actions require POST"))
		return true
	}
	payload, e := body(r)
	if e != nil {
		fail(w, 400, "invalid_json", e)
		return true
	}
	deleting := strings.HasSuffix(path, "bulk-delete")
	keyName := "hostnames"
	if deleting {
		keyName = "ids"
		if !domain.B(payload, "confirmed") {
			fail(w, 400, "confirmation_required", errors.New("explicitly confirm the reviewed bulk cleanup"))
			return true
		}
	}
	values, ok := payload[keyName].([]any)
	if !ok || len(values) < 1 || len(values) > 500 {
		fail(w, 400, "invalid_batch_size", errors.New("a bulk action requires between 1 and 500 items"))
		return true
	}
	key := r.Header.Get("Idempotency-Key")
	if key == "" || len(key) > 200 {
		fail(w, 400, "idempotency_required", errors.New("Idempotency-Key (1–200 characters) is required for bulk actions"))
		return true
	}
	if !deleting {
		if _, e = s.Store.Get(r.Context(), "saas/zones", domain.S(payload, "saas_zone_id")); e != nil {
			fail(w, 400, "invalid_saas_zone", errors.New("select an existing SaaS zone"))
			return true
		}
		if method := domain.S(payload, "validation_method"); method != "" && method != "txt" && method != "http" {
			fail(w, 400, "invalid_validation_method", errors.New("validation_method must be txt or http"))
			return true
		}
		if mode := domain.S(payload, "automation_mode"); mode != "" && mode != "manual" && mode != "auto" {
			fail(w, 400, "invalid_automation_mode", errors.New("automation_mode must be manual or auto"))
			return true
		}
	}
	owner := domain.ID()
	lockID := "bulk:" + auth.Hash(path+":"+key)
	if !deleting {
		lockID = "bulk:saas-hostnames:create"
	}
	locked, e := s.Store.Lease(r.Context(), lockID, owner, 5*time.Minute)
	if e != nil || !locked {
		fail(w, 409, "batch_in_progress", errors.New("this bulk request is already being processed; retry with the same key"))
		return true
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
		defer cancel()
		s.Store.ReleaseLease(cleanup, lockID, owner)
	}()
	raw, _ := json.Marshal(payload)
	requestHash := auth.Hash(string(raw))
	var priorHash, response string
	keyColumn := "key"
	if s.Store.Driver == "mysql" {
		keyColumn = "`key`"
	}
	e = s.Store.DB.QueryRowContext(r.Context(), s.Store.Q(`SELECT request_hash,response FROM idempotency WHERE scope=? AND `+keyColumn+`=?`), path, key).Scan(&priorHash, &response)
	if e == nil {
		if priorHash != requestHash {
			fail(w, 409, "idempotency_conflict", errors.New("the same Idempotency-Key was used for a different bulk request"))
			return true
		}
		if response != "" {
			cached := domain.Doc{}
			if json.Unmarshal([]byte(response), &cached) == nil {
				write(w, 202, cached)
				return true
			}
		}
	} else if !errors.Is(e, sql.ErrNoRows) {
		fail(w, 500, "idempotency_failed", e)
		return true
	} else {
		if _, e = s.Store.DB.ExecContext(r.Context(), s.Store.Q(`INSERT INTO idempotency(scope,`+keyColumn+`,request_hash,response,created_at) VALUES(?,?,?,?,?)`), path, key, requestHash, "", domain.Now()); e != nil {
			fail(w, 500, "idempotency_failed", e)
			return true
		}
	}
	existing, e := s.Store.List(r.Context(), "saas/hostnames")
	if e != nil {
		fail(w, 500, "list_failed", e)
		return true
	}
	byHostname := map[string]string{}
	for _, h := range existing {
		byHostname[strings.TrimSuffix(strings.ToLower(domain.S(h, "hostname")), ".")] = domain.S(h, "id")
	}
	items := []domain.Doc{}
	jobIDs := []string{}
	failures := []domain.Doc{}
	seen := map[string]bool{}
	retryable := false
	addFailure := func(index int, item, code string, e error) {
		d := domain.Doc{"index": index, "code": code, "message": security.Redact(e.Error())}
		if deleting {
			d["id"] = item
		} else {
			d["hostname"] = item
		}
		failures = append(failures, d)
	}
	for index, value := range values {
		item, valid := value.(string)
		item = strings.TrimSpace(item)
		if !deleting {
			item = strings.TrimSuffix(strings.ToLower(item), ".")
		}
		if !valid || item == "" {
			addFailure(index, item, "invalid_item", errors.New("each item must be a non-empty string"))
			continue
		}
		if seen[item] {
			addFailure(index, item, "duplicate_item", errors.New("duplicate item in this batch"))
			continue
		}
		seen[item] = true
		var resource domain.Doc
		if deleting {
			resource, e = s.Store.Get(r.Context(), "saas/hostnames", item)
			if e != nil {
				addFailure(index, item, "not_found", errors.New("custom hostname does not exist"))
				continue
			}
		} else {
			if !bulkHostname(item) {
				addFailure(index, item, "invalid_hostname", errors.New("use a valid public hostname without a URL, wildcard or IP address"))
				continue
			}
			digest := auth.Hash(path + ":" + key + ":" + domain.S(payload, "saas_zone_id") + ":" + item)
			id := digest[:8] + "-" + digest[8:12] + "-4" + digest[13:16] + "-a" + digest[17:20] + "-" + digest[20:32]
			if existingID := byHostname[item]; existingID != "" && existingID != id {
				addFailure(index, item, "hostname_conflict", errors.New("hostname is already managed; edit its existing resource"))
				continue
			}
			resource, e = s.Store.Get(r.Context(), "saas/hostnames", id)
			if errors.Is(e, store.ErrNotFound) {
				resource = domain.Doc{"id": id, "hostname": item}
				for _, field := range []string{"saas_zone_id", "validation_method", "automation_mode", "dns_connection_id", "dns_zone_id", "dns_zone_name", "custom_origin", "project_id", "site_id"} {
					if v, exists := payload[field]; exists {
						resource[field] = v
					}
				}
				if e = s.prepare(r.Context(), "saas/hostnames", resource); e == nil {
					e = s.Store.Put(r.Context(), "saas/hostnames", resource)
				}
				if e == nil {
					s.audit(r, "create", "saas/hostnames", id, "Custom hostname created by bulk import", domain.S(session, "username"))
					s.emit("saas/hostnames", id)
				}
			}
			if e != nil {
				addFailure(index, item, "validation_failed", e)
				continue
			}
			byHostname[item] = id
		}
		action := "create"
		if deleting {
			action = "delete"
		}
		job, e := s.Queue(r.Context(), action, "saas/hostnames", resource, key+":"+action+":"+item)
		if e != nil {
			retryable = true
			addFailure(index, item, "queue_failed", e)
			continue
		}
		entry := public(resource)
		entry["job_id"] = job["id"]
		items = append(items, entry)
		jobIDs = append(jobIDs, domain.S(job, "id"))
	}
	result := domain.Doc{"items": items, "job_ids": jobIDs, "errors": failures, "accepted": len(items), "total": len(values)}
	if !retryable {
		encoded, _ := json.Marshal(result)
		if _, e = s.Store.DB.ExecContext(r.Context(), s.Store.Q(`UPDATE idempotency SET response=? WHERE scope=? AND `+keyColumn+`=? AND request_hash=?`), string(encoded), path, key, requestHash); e != nil {
			fail(w, 500, "idempotency_failed", e)
			return true
		}
	}
	s.audit(r, "bulk", "saas/hostnames", "", fmt.Sprintf("%d of %d bulk items queued", len(items), len(values)), domain.S(session, "username"))
	write(w, 202, result)
	return true
}

func bulkHostname(host string) bool {
	if len(host) > 253 || strings.ContainsAny(host, "/:@* \\_\t\r\n") {
		return false
	}
	if _, e := netip.ParseAddr(host); e == nil {
		return false
	}
	labels := strings.Split(host, ".")
	if len(labels) < 2 {
		return false
	}
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if r != '-' && (r < 'a' || r > 'z') && (r < '0' || r > '9') {
				return false
			}
		}
	}
	return true
}
