package api

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/newnexus/newnexus/internal/agenttls"
	"github.com/newnexus/newnexus/internal/auth"
	"github.com/newnexus/newnexus/internal/domain"
	"github.com/newnexus/newnexus/internal/optimizer"
	"github.com/newnexus/newnexus/internal/routing"
	"github.com/newnexus/newnexus/internal/security"
	"github.com/newnexus/newnexus/internal/store"
	"log/slog"
	"net/url"
	"strings"
	"time"
)

func (s *Server) EnsureDefaults(ctx context.Context) error {
	defaults := domain.Doc{"probe.discovery_interval_seconds": 21600, "probe.ranked_interval_seconds": 300, "probe.active_interval_seconds": 30, "probe.concurrency": 32, "probe.timeout_ms": 5000, "probe.retries": 2, "probe.min_samples": 3, "probe.freshness_seconds": 900, "probe.minimum_success_rate": .8, "probe.default_url": "https://speed.cloudflare.com/__down?bytes=1048576", "probe.default_user_agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:132.0) Gecko/20100101 Firefox/132.0", "probe.max_bytes": 1048576, "score.weights": domain.Doc{"tcp": .2, "tls": .15, "ttfb": .25, "p50": .15, "p95": .25, "download": .2, "failure": 1}, "routing.min_nodes": 2, "routing.quorum_percent": 67, "routing.failure_rounds": 3, "routing.recovery_rounds": 5, "routing.performance_rounds": 5, "routing.improvement_percent": 20, "routing.cooldown_seconds": 600, "publish.large_change_threshold": 20, "retention.raw_days": 7, "retention.five_minute_days": 30, "retention.hourly_days": 180, "alerts.webhook_url": "", "alerts.smtp_host": "", "alerts.smtp_port": 587, "alerts.smtp_username": "", "alerts.email_from": "", "alerts.email_to": "", "auth.require_totp": false}
	defaults["system.disk_warning_percent"] = 15
	defaults["system.disk_critical_percent"] = 5
	defaults[agenttls.SettingKey] = false
	for _, key := range addressSettingKeys {
		defaults[key] = ""
	}
	for key, value := range (domain.Doc{"probe.host": "", "probe.sni": "", "probe.path": "", "probe.method": "GET", "probe.headers": domain.Doc{}, "probe.expected_statuses": []any{200}, "probe.expected_contains": "", "probe.samples": 3, "probe.max_redirects": 3}) {
		defaults[key] = value
	}
	for key, value := range defaults {
		if _, e := s.Store.Get(ctx, "settings", key); errors.Is(e, store.ErrNotFound) {
			if e = s.Store.Put(ctx, "settings", domain.Doc{"id": key, "key": key, "value": value}); e != nil {
				return e
			}
		}
	}
	return nil
}
func (s *Server) setting(ctx context.Context, key string, fallback float64) float64 {
	if values, ok := ctx.Value(settingsContextKey{}).(domain.Doc); ok {
		if value, exists := values[key]; exists {
			v := domain.N(domain.Doc{"value": value}, "value")
			if v >= 0 {
				return v
			}
		}
		return fallback
	}
	d, e := s.Store.Get(ctx, "settings", key)
	if e != nil {
		return fallback
	}
	v := domain.N(d, "value")
	if v < 0 || v == 0 && settingRanges[key][0] != 0 {
		return fallback
	}
	return v
}
func (s *Server) Queue(ctx context.Context, action, kind string, d domain.Doc, key string) (domain.Doc, error) {
	id := domain.ID()
	if key != "" {
		id = idempotentJobID(action, kind, domain.S(d, "id"), key)
		if existing, e := s.Store.Get(ctx, "jobs", id); e == nil {
			return existing, nil
		}
	}
	job := domain.Doc{"id": id, "type": action, "resource_type": kind, "resource_id": d["id"], "status": "queued", "progress": 0, "attempts": 0, "max_attempts": 3, "next_attempt_at": domain.Now(), "request_id": domain.ID()}
	if kind == "candidates" && action == "probe" {
		job["ids"] = d["ids"]
	}
	if kind == "alerts" && action == "notify" {
		job["incident_id"] = d["incident_id"]
		job["alert_snapshot"] = public(d)
	}
	if e := s.Store.Put(ctx, "jobs", job); e != nil {
		if errors.Is(e, store.ErrConflict) {
			return s.Store.Get(ctx, "jobs", id)
		}
		return nil, e
	}
	s.emit("jobs", id)
	return job, nil
}

func idempotentJobID(action, kind, resourceID, key string) string {
	digest := auth.Hash(action + ":" + kind + ":" + resourceID + ":" + key)
	return digest[:8] + "-" + digest[8:12] + "-4" + digest[13:16] + "-a" + digest[17:20] + "-" + digest[20:32]
}
func (s *Server) StartWorkers(ctx context.Context) {
	go s.worker(ctx)
	go s.scheduler(ctx)
	go s.aggregator(ctx)
	go s.maintenance(ctx)
	go s.routingEvaluator(ctx)
	go s.externalMonitor(ctx)
}
func (s *Server) aggregator(ctx context.Context) {
	owner := domain.ID()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			acquired, e := s.Store.Lease(ctx, "optimizer-aggregator", owner, 15*time.Second)
			if e != nil || !acquired {
				continue
			}
			workCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			_, e = optimizer.RefreshPending(workCtx, s.Store, 2500)
			cancel()
			if e != nil && ctx.Err() == nil {
				slog.Error("candidate aggregation failed", "error", e)
			}
		}
	}
}

// ScheduleTick executes the same scheduling pass used by the live leader. It is
// exposed for isolated scale acceptance against the real repository and hub.
func (s *Server) ScheduleTick(ctx context.Context) { s.scheduleTick(ctx) }
func (s *Server) worker(ctx context.Context) {
	owner := domain.ID()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			jobs, e := s.Store.List(ctx, "jobs")
			if e != nil {
				slog.Error("job scan failed", "error", e)
				continue
			}
			for i := len(jobs) - 1; i >= 0; i-- {
				job := jobs[i]
				state := domain.S(job, "status")
				if state != "queued" && state != "retrying" && state != "running" {
					continue
				}
				next, _ := time.Parse(time.RFC3339Nano, domain.S(job, "next_attempt_at"))
				if next.After(time.Now()) {
					continue
				}
				acquired, e := s.Store.Lease(ctx, "job:"+domain.S(job, "id"), owner, 2*time.Minute)
				if e != nil || !acquired {
					continue
				}
				// The scan can predate another replica finishing this job.
				// Refresh under the lease before attempting any external work.
				job, e = s.Store.Get(ctx, "jobs", domain.S(job, "id"))
				if e != nil {
					s.Store.ReleaseLease(ctx, "job:"+domain.S(jobs[i], "id"), owner)
					continue
				}
				state = domain.S(job, "status")
				next, _ = time.Parse(time.RFC3339Nano, domain.S(job, "next_attempt_at"))
				if (state != "queued" && state != "retrying" && state != "running") || next.After(time.Now()) {
					s.Store.ReleaseLease(ctx, "job:"+domain.S(job, "id"), owner)
					continue
				}
				if e = s.runJob(ctx, job); e != nil {
					slog.Error("job persistence failed", "job_id", job["id"], "error", e)
				}
				s.Store.ReleaseLease(ctx, "job:"+domain.S(job, "id"), owner)
				if ctx.Err() != nil {
					return
				}
			}
		}
	}
}
func (s *Server) runJob(ctx context.Context, job domain.Doc) error {
	job["status"] = "running"
	job["attempts"] = domain.N(job, "attempts") + 1
	job["progress"] = 10
	if e := s.Store.Put(ctx, "jobs", job); e != nil {
		return e
	}
	workCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	kind := domain.S(job, "resource_type")
	action := domain.S(job, "type")
	var result domain.Doc
	var e error
	if kind == "alerts" && action == "notify" {
		alert := domain.M(job, "alert_snapshot")
		legacy := len(alert) == 0
		if len(alert) == 0 {
			alert, e = s.Store.Get(workCtx, "alerts", domain.S(job, "resource_id"))
		}
		if e == nil {
			if legacy && domain.S(job, "id") != idempotentJobID("notify", "alerts", domain.S(alert, "id"), alertNotificationKey(alert)) {
				result = domain.Doc{"skipped": true, "reason": "notification incident superseded"}
			} else {
				e = s.deliverAlert(workCtx, alert)
			}
		}
	} else if kind == "candidates" && action == "aggregate" {
		total := 0
		for e == nil {
			var n int
			n, e = optimizer.RefreshPending(workCtx, s.Store, 5000)
			total += n
			if n < 5000 {
				break
			}
		}
		result = domain.Doc{"aggregated": total}
	} else if kind == "candidates" && action == "probe" {
		result, e = s.dispatchCandidates(workCtx, job)
	} else {
		var resource domain.Doc
		resource, e = s.Store.Get(workCtx, kind, domain.S(job, "resource_id"))
		if e == nil {
			if kind == "sources" && action == "sync" {
				var count int
				count, e = optimizer.Import(workCtx, s.Store, resource)
				result = domain.Doc{"imported": count}
			} else {
				result, e = s.Operations.Run(workCtx, action, kind, resource)
			}
		}
	}
	if e != nil {
		job["error_code"] = "operation_failed"
		job["error_message"] = e.Error()
		if domain.N(job, "attempts") < domain.N(job, "max_attempts") {
			job["status"] = "retrying"
			job["next_attempt_at"] = time.Now().UTC().Add(time.Duration(1<<int(domain.N(job, "attempts"))) * time.Second).Format(time.RFC3339Nano)
		} else {
			job["status"] = "failed"
			job["finished_at"] = domain.Now()
			if action != "notify" {
				s.Alert(ctx, "critical", "job_failed", "操作失败", fmt.Sprintf("%s %s failed: %s", kind, action, e), kind, domain.S(job, "resource_id"))
			}
		}
	} else {
		job["status"] = "succeeded"
		job["progress"] = 100
		job["finished_at"] = domain.Now()
		job["error_code"] = ""
		job["error_message"] = ""
		if result != nil {
			job["result"] = public(result)
		}
	}
	if e = s.Store.Put(ctx, "jobs", job); e != nil {
		return e
	}
	s.emit("jobs", domain.S(job, "id"))
	s.emit(kind, domain.S(job, "resource_id"))
	return nil
}
func (s *Server) dispatchCandidates(ctx context.Context, request domain.Doc) (domain.Doc, error) {
	ctx = s.cachedSettings(ctx)
	candidates, e := s.Store.List(ctx, "candidates")
	if e != nil {
		return nil, e
	}
	ids := map[string]bool{}
	if rows, ok := request["ids"].([]any); ok {
		for _, id := range rows {
			ids[fmt.Sprint(id)] = true
		}
	}
	nodes, e := s.Store.List(ctx, "nodes")
	if e != nil {
		return nil, e
	}
	online := []domain.Doc{}
	for _, node := range nodes {
		if domain.S(node, "status") == "online" {
			online = append(online, node)
		}
	}
	if len(online) == 0 {
		return nil, errors.New("no connected probe nodes are available")
	}
	profiles, _ := s.Store.List(ctx, "probe-profiles")
	count := 0
	for _, candidate := range candidates {
		if len(ids) > 0 && !ids[domain.S(candidate, "id")] {
			continue
		}
		profile := domain.Doc{}
		for _, p := range profiles {
			if domain.S(p, "project_id") == domain.S(candidate, "project_id") {
				profile = p
				break
			}
		}
		for _, node := range online {
			task := s.task(ctx, candidate, profile)
			if !nodeTask(node, task, false) {
				continue
			}
			if e = s.Hub.Dispatch(ctx, domain.S(node, "id"), task); e != nil {
				return nil, e
			}
			count++
		}
	}
	return domain.Doc{"tasks_dispatched": count}, nil
}
func (s *Server) task(ctx context.Context, candidate, profile domain.Doc) domain.Doc {
	task := mergeProfile(s.probeDefaults(ctx), profile)
	delete(task, "_version")
	delete(task, "created_at")
	delete(task, "updated_at")
	task["id"] = domain.ID()
	task["candidate_id"] = candidate["id"]
	task["address"] = candidate["address"]
	if task["mode"] == nil {
		task["mode"] = "all"
	}
	if task["url"] == nil {
		d, _ := s.Store.Get(ctx, "settings", "probe.default_url")
		task["url"] = d["value"]
	}
	if task["timeout_ms"] == nil {
		task["timeout_ms"] = s.setting(ctx, "probe.timeout_ms", 5000)
	}
	if task["max_bytes"] == nil {
		task["max_bytes"] = s.setting(ctx, "probe.max_bytes", 1048576)
	}
	if task["retries"] == nil {
		task["retries"] = s.setting(ctx, "probe.retries", 2)
	}
	if task["user_agent"] == nil {
		d, _ := s.Store.Get(ctx, "settings", "probe.default_user_agent")
		task["user_agent"] = d["value"]
	}
	if task["samples"] == nil {
		task["samples"] = 3
	}
	if task["max_redirects"] == nil {
		task["max_redirects"] = 3
	}
	if path := domain.S(task, "path"); path != "" {
		if u, e := url.Parse(domain.S(task, "url")); e == nil {
			u.Path = path
			u.RawPath = ""
			task["url"] = u.String()
		}
	}
	return task
}
func (s *Server) scheduler(ctx context.Context) {
	owner := domain.ID()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	first := true
	for {
		if !first {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
		first = false
		if ctx.Err() != nil {
			return
		}
		acquired, e := s.Store.Lease(ctx, "scheduler", owner, 15*time.Second)
		if e != nil || !acquired {
			continue
		}
		passCtx, passCancel := context.WithCancel(ctx)
		renewed := make(chan struct{})
		go func() {
			defer close(renewed)
			renew := time.NewTicker(5 * time.Second)
			defer renew.Stop()
			for {
				select {
				case <-passCtx.Done():
					return
				case <-renew.C:
					ok, e := s.Store.Lease(passCtx, "scheduler", owner, 15*time.Second)
					if e != nil || !ok {
						passCancel()
						return
					}
				}
			}
		}()
		s.dispatchTick(passCtx)
		passCancel()
		<-renewed
	}
}
func (s *Server) scheduleTick(ctx context.Context) {
	s.monitorExternal(ctx, time.Now())
	s.dispatchTick(ctx)
	s.evaluateCurrentRoutingPass(ctx)
}

func (s *Server) dispatchTick(ctx context.Context) {
	started := time.Now()
	stage := started
	phases := domain.Doc{}
	mark := func(name string) { phases[name] = time.Since(stage).Milliseconds(); stage = time.Now() }
	defer func() {
		if time.Since(started) > time.Second {
			slog.Info("scheduler pass", "duration_ms", time.Since(started).Milliseconds(), "phases_ms", phases)
		}
	}()
	ctx = s.cachedSettings(ctx)
	now := time.Now()
	groups, _ := s.Store.List(ctx, "node-groups")
	nodes, _ := s.Store.List(ctx, "nodes")
	online := []domain.Doc{}
	for _, node := range nodes {
		if domain.S(node, "status") == "revoked" {
			continue
		}
		last, _ := time.Parse(time.RFC3339Nano, domain.S(node, "last_seen_at"))
		if domain.S(node, "status") == "online" && now.Sub(last) > 45*time.Second {
			node["status"] = "offline"
			_ = s.Store.Put(ctx, "nodes", node)
			s.Alert(ctx, "warning", "node_offline", "探测节点离线", domain.S(node, "name")+" has stopped sending heartbeats", "nodes", domain.S(node, "id"))
			s.emit("nodes", domain.S(node, "id"))
		}
		if domain.S(node, "status") == "online" {
			online = append(online, node)
		}
	}
	sources, _ := s.Store.List(ctx, "sources")
	for _, source := range sources {
		if !domain.B(source, "enabled") {
			continue
		}
		last, _ := time.Parse(time.RFC3339Nano, domain.S(source, "last_synced_at"))
		if now.Sub(last) >= time.Duration(s.setting(ctx, "probe.discovery_interval_seconds", 21600))*time.Second {
			if !s.hasPending(ctx, "sources", domain.S(source, "id"), "sync") {
				_, _ = s.Queue(ctx, "sync", "sources", source, "scheduled:"+fmt.Sprint(now.Unix()/21600))
			}
		}
	}
	mark("state")
	if len(online) == 0 {
		return
	}
	profiles, _ := s.Store.List(ctx, "probe-profiles")
	defaults := s.probeDefaults(ctx)
	byProject := map[string]domain.Doc{}
	byProfile := map[string]domain.Doc{}
	for _, p := range profiles {
		merged := mergeProfile(defaults, p)
		byProject[domain.S(p, "project_id")] = merged
		byProfile[domain.S(p, "id")] = merged
	}
	budget := int(s.setting(ctx, "probe.concurrency", 32)) * len(online) * 4
	candidates, _ := s.Store.DueCandidates(ctx, now, time.Duration(s.setting(ctx, "probe.discovery_interval_seconds", 21600))*time.Second, time.Duration(s.setting(ctx, "probe.ranked_interval_seconds", 300))*time.Second, time.Duration(s.setting(ctx, "probe.active_interval_seconds", 30))*time.Second, max(1, budget/3))
	dispatch := []domain.Doc{}
	candidateScheduled := []string{}
	ruleScheduled := []string{}
	siteScheduled := []string{}
	pending, pendingErr := s.Store.PendingTaskKeys(ctx)
	if pendingErr != nil {
		slog.Error("pending task scan failed", "error", pendingErr)
		return
	}
	for _, candidate := range candidates {
		if budget <= 0 {
			break
		}
		profile := byProject[domain.S(candidate, "project_id")]
		if profile == nil {
			profile = defaults
		}
		for _, node := range nodeSubset(online, domain.S(candidate, "id"), 3) {
			key := domain.S(node, "id") + ":" + domain.S(candidate, "id")
			if pending[key] {
				continue
			}
			task := s.task(ctx, candidate, profile)
			if !nodeTask(node, task, false) {
				continue
			}
			task["node_id"] = node["id"]
			dispatch = append(dispatch, task)
			budget--
			pending[key] = true
		}
		candidateScheduled = append(candidateScheduled, domain.S(candidate, "id"))
	}
	mark("candidate_selection")
	rules, _ := s.Store.List(ctx, "rules")
	for _, rule := range rules {
		profile := byProfile[domain.S(rule, "probe_profile_id")]
		if profile == nil {
			profile = defaults
		}
		profile = domain.Clone(profile)
		if domain.S(rule, "probe_profile_id") == "" {
			profile["url"] = "https://" + domain.S(rule, "hostname") + "/"
		}
		if domain.S(rule, "site_id") != "" {
			if site, e := s.Store.Get(ctx, "sites", domain.S(rule, "site_id")); e == nil {
				if domain.S(rule, "probe_profile_id") == "" {
					profile = byProfile[domain.S(site, "probe_profile_id")]
					if profile == nil {
						profile = defaults
					}
					profile = domain.Clone(profile)
				}
				if profile == nil {
					profile = domain.Doc{}
				}
				profile["url"] = site["url"]
			}
		}
		last, _ := time.Parse(time.RFC3339Nano, domain.S(rule, "last_probe_scheduled_at"))
		if now.Sub(last) >= profilePeriod(profile, time.Duration(s.setting(ctx, "probe.active_interval_seconds", 30))*time.Second) {
			for _, line := range domain.A(rule, "lines") {
				lineNodes := []domain.Doc{}
				for _, node := range online {
					if nodeInLine(node, line, groups) {
						lineNodes = append(lineNodes, node)
					}
				}
				for _, target := range domain.A(line, "targets") {
					if !domain.B(target, "enabled") {
						continue
					}
					minimum := domain.N(line, "min_nodes")
					if line["min_nodes"] == nil {
						minimum = s.setting(ctx, "routing.min_nodes", 2)
					}
					for _, node := range nodeSubset(lineNodes, domain.S(target, "id"), max(3, int(minimum))) {
						id := "target:" + domain.S(target, "id")
						if pending[domain.S(node, "id")+":"+id] {
							continue
						}
						task := s.task(ctx, domain.Doc{"id": id, "address": target["cname"]}, profile)
						if domain.S(profile, "url") == "" {
							task["url"] = "https://" + domain.S(rule, "hostname") + "/"
						}
						if protocol := domain.S(target, "protocol"); protocol != "" {
							if u, e := url.Parse(domain.S(task, "url")); e == nil {
								u.Scheme = protocol
								task["url"] = u.String()
							}
						}
						if target["port"] != nil {
							task["port"] = target["port"]
						}
						if target["headers"] != nil {
							task["headers"] = target["headers"]
						}
						if !nodeTask(node, task, domain.S(profile, "mode") != "tcp") {
							continue
						}
						task["node_id"] = node["id"]
						dispatch = append(dispatch, task)
					}
				}
			}
			ruleScheduled = append(ruleScheduled, domain.S(rule, "id"))
		}
	}
	mark("routing")
	sites, _ := s.Store.List(ctx, "sites")
	for _, site := range sites {
		last, _ := time.Parse(time.RFC3339Nano, domain.S(site, "last_probe_scheduled_at"))
		u, e := url.Parse(domain.S(site, "url"))
		if e != nil || u.Hostname() == "" {
			continue
		}
		profile := byProfile[domain.S(site, "probe_profile_id")]
		if profile == nil {
			profile = defaults
		}
		if now.Sub(last) < profilePeriod(profile, time.Duration(s.setting(ctx, "probe.active_interval_seconds", 30))*time.Second) {
			continue
		}
		siteNodes := []domain.Doc{}
		for _, node := range online {
			if nodeInLine(node, site, groups) {
				siteNodes = append(siteNodes, node)
			}
		}
		for _, node := range nodeSubset(siteNodes, domain.S(site, "id"), 3) {
			candidate := domain.Doc{"id": "site:" + domain.S(site, "id"), "address": u.Hostname()}
			if pending[domain.S(node, "id")+":"+domain.S(candidate, "id")] {
				continue
			}
			task := s.task(ctx, candidate, profile)
			task["url"] = site["url"]
			if !nodeTask(node, task, true) {
				continue
			}
			task["node_id"] = node["id"]
			dispatch = append(dispatch, task)
		}
		siteScheduled = append(siteScheduled, domain.S(site, "id"))
	}
	mark("sites")
	if e := s.Hub.PersistTasks(ctx, dispatch); e != nil {
		slog.Error("batch dispatch failed", "error", e)
		return
	}
	mark("persist")
	at := domain.Now()
	for _, spec := range []struct {
		kind, field string
		ids         []string
	}{{"candidates", "last_scheduled_at", candidateScheduled}, {"rules", "last_probe_scheduled_at", ruleScheduled}, {"sites", "last_probe_scheduled_at", siteScheduled}} {
		if e := s.Store.SetSchedule(ctx, spec.kind, spec.field, spec.ids, at); e != nil {
			slog.Error("schedule timestamp update failed", "kind", spec.kind, "error", e)
			return
		}
	}
	mark("schedule_timestamps")
	s.Hub.PushTasks(dispatch)
	mark("push")
	phases["tasks"] = len(dispatch)
	phases["candidates"] = len(candidateScheduled)
	phases["rules"] = len(ruleScheduled)
}
func (s *Server) hasPending(ctx context.Context, kind, id, action string) bool {
	jobs, _ := s.Store.List(ctx, "jobs")
	for _, j := range jobs {
		state := domain.S(j, "status")
		if domain.S(j, "resource_type") == kind && domain.S(j, "resource_id") == id && domain.S(j, "type") == action && (state == "queued" || state == "running" || state == "retrying") {
			return true
		}
	}
	return false
}
func (s *Server) EvaluateRule(ctx context.Context, rule domain.Doc) (domain.Doc, error) {
	ctx = s.cachedSettings(ctx)
	nodes, _ := s.Store.List(ctx, "nodes")
	groups, _ := s.Store.List(ctx, "node-groups")
	return s.evaluateRule(ctx, rule, nodes, groups)
}
func (s *Server) evaluateRule(ctx context.Context, rule domain.Doc, nodes, groups []domain.Doc) (domain.Doc, error) {
	type pendingAlert struct{ severity, category, title, message string }
	pendingAlerts := []pendingAlert{}
	before, _ := json.Marshal(rule)
	previousStatus := domain.S(rule, "status")
	changed := false
	paused := false
	allFailed := false
	byNode := map[string]domain.Doc{}
	for _, node := range nodes {
		byNode[domain.S(node, "id")] = node
	}
	lines := domain.A(rule, "lines")
	for _, line := range lines {
		minOverride, hasMin := line["min_nodes"]
		quorumOverride, hasQuorum := line["quorum_percent"]
		if line["min_nodes"] == nil {
			line["min_nodes"] = s.setting(ctx, "routing.min_nodes", 2)
		}
		if line["quorum_percent"] == nil {
			line["quorum_percent"] = s.setting(ctx, "routing.quorum_percent", 67)
		}
		line["failure_required_rounds"] = s.setting(ctx, "routing.failure_rounds", 3)
		line["recovery_required_rounds"] = s.setting(ctx, "routing.recovery_rounds", 5)
		line["performance_required_rounds"] = s.setting(ctx, "routing.performance_rounds", 5)
		line["improvement_percent"] = s.setting(ctx, "routing.improvement_percent", 20)
		line["cooldown_seconds"] = s.setting(ctx, "routing.cooldown_seconds", 600)
		votes := []routing.Vote{}
		for _, target := range domain.A(line, "targets") {
			if cached, ok := ctx.Value(routingVotesContextKey{}).(map[string][]routing.Vote); ok {
				for _, v := range cached[domain.S(target, "id")] {
					if domain.S(byNode[v.NodeID], "status") == "online" && nodeInLine(byNode[v.NodeID], line, groups) {
						votes = append(votes, v)
					}
				}
				continue
			}
			rows, e := s.Store.DB.QueryContext(ctx, s.Store.Q(`SELECT node_id,success,tcp_ms,tls_ms,ttfb_ms,observed_at FROM metrics WHERE candidate_id=? AND observed_at>=? ORDER BY observed_at DESC LIMIT 1000`), "target:"+domain.S(target, "id"), s.Store.Time(time.Now().Add(-90*time.Second)))
			if e != nil {
				return nil, e
			}
			for rows.Next() {
				var node, at string
				var success int
				var tcp, tls, ttfb float64
				if rows.Scan(&node, &success, &tcp, &tls, &ttfb, &at) != nil {
					continue
				}
				if domain.S(byNode[node], "status") != "online" || !nodeInLine(byNode[node], line, groups) {
					continue
				}
				when, _ := time.Parse(time.RFC3339Nano, at)
				votes = append(votes, routing.Vote{NodeID: node, TargetID: domain.S(target, "id"), Healthy: success == 1, Score: 100 / (1 + (tcp+tls+ttfb)/100), At: when})
			}
			rows.Close()
		}
		decision := routing.Evaluate(line, votes, domain.S(rule, "switch_mode"), time.Now())
		if !hasMin || minOverride == nil {
			delete(line, "min_nodes")
		}
		if !hasQuorum || quorumOverride == nil {
			delete(line, "quorum_percent")
		}
		changed = changed || decision.Changed
		paused = paused || decision.Paused
		allFailed = allFailed || decision.AllFailed
		if decision.AllFailed && previousStatus != "fail_stale" {
			pendingAlerts = append(pendingAlerts, pendingAlert{"critical", "all_targets_failed", "线路全部目标故障", decision.Reason})
		}
		if decision.Paused && strings.Contains(decision.Reason, "samples") && previousStatus != "paused" {
			pendingAlerts = append(pendingAlerts, pendingAlert{"warning", "quorum_insufficient", "线路仲裁样本不足", decision.Reason})
		}
	}
	rule["lines"] = lines
	rule["status"] = "healthy"
	if paused {
		rule["status"] = "paused"
	}
	if allFailed {
		rule["status"] = "fail_stale"
	}
	after, _ := json.Marshal(rule)
	if string(before) != string(after) || len(pendingAlerts) > 0 || changed {
		var e error
		if owner, leased := ctx.Value(routingOwnerKey{}).(string); leased {
			e = s.Store.PutUnderSharedLease(ctx, routingEvaluationLease, owner, "rules", rule)
		} else {
			e = s.Store.Put(ctx, "rules", rule)
		}
		if e != nil {
			return nil, e
		}
	}
	for _, alert := range pendingAlerts {
		s.Alert(ctx, alert.severity, alert.category, alert.title, alert.message, "rules", domain.S(rule, "id"))
	}
	if changed && !allFailed && !paused {
		s.Alert(ctx, "info", "route_switched", "线路策略选择已更新", "DNS publication requires the configured publish policy", "rules", domain.S(rule, "id"))
		if domain.S(rule, "publish_mode") == "auto" {
			release, e := s.Operations.Run(ctx, "preview", "rules", rule)
			if e != nil {
				return nil, e
			}
			if len(domain.A(release, "diff")) <= int(s.setting(ctx, "publish.large_change_threshold", 20)) {
				_, e = s.Queue(ctx, "apply", "releases", release, "auto:"+domain.S(release, "id"))
				if e != nil {
					return nil, e
				}
			} else {
				s.Alert(ctx, "warning", "confirmation_required", "DNS 变更需要确认", "automatic change exceeds the configured safety threshold", "releases", domain.S(release, "id"))
			}
		}
	}
	return rule, nil
}
func (s *Server) cleanup(ctx context.Context) error {
	started := time.Now()
	stage := started
	phases := domain.Doc{}
	mark := func(name string) { phases[name] = time.Since(stage).Milliseconds(); stage = time.Now() }
	defer func() {
		if time.Since(started) > time.Second {
			slog.Info("retention cleanup", "duration_ms", time.Since(started).Milliseconds(), "phases_ms", phases)
		}
	}()
	failures := []string{}
	record := func(e error) {
		if e != nil {
			failures = append(failures, security.Redact(e.Error()))
		}
	}
	record(s.Store.EnsureMetricPartitions(ctx, time.Now()))
	mark("ensure_partitions")
	cutoff := s.Store.Time(time.Now().Add(-time.Duration(s.setting(ctx, "retention.raw_days", 7)) * 24 * time.Hour))
	_, e := s.Store.DB.ExecContext(ctx, s.Store.Q(`DELETE FROM metrics WHERE observed_at<?`), cutoff)
	record(e)
	mark("raw_samples")
	cutoffTime, _ := time.Parse(time.RFC3339Nano, cutoff)
	record(s.Store.DropExpiredMetricPartitions(ctx, cutoffTime))
	mark("drop_partitions")
	_, e = s.Store.DB.ExecContext(ctx, s.Store.Q(`DELETE FROM metrics_5m WHERE bucket<?`), s.Store.Time(time.Now().Add(-time.Duration(s.setting(ctx, "retention.five_minute_days", 30))*24*time.Hour)))
	record(e)
	mark("five_minute")
	_, e = s.Store.DB.ExecContext(ctx, s.Store.Q(`DELETE FROM metrics_1h WHERE bucket<?`), s.Store.Time(time.Now().Add(-time.Duration(s.setting(ctx, "retention.hourly_days", 180))*24*time.Hour)))
	record(e)
	mark("hourly")
	_, e = s.Store.DB.ExecContext(ctx, s.Store.Q(`DELETE FROM result_ids WHERE observed_at<?`), s.Store.Time(time.Now().Add(-time.Duration(max(30, s.setting(ctx, "retention.raw_days", 7)))*24*time.Hour)))
	record(e)
	mark("receipts")
	// Filter in SQL instead of loading every active task into the scheduler.
	// Atomic predicates also prevent a concurrent update from making a loaded
	// snapshot stale before its unconditional deletion.
	field := func(name string) string {
		if s.Store.Driver == "pgx" {
			return "(doc::jsonb->>'" + name + "')"
		}
		if s.Store.Driver == "mysql" {
			switch name {
			case "status":
				return "status_idx"
			case "finished_at":
				return "finished_at_idx"
			case "expires_at":
				return "expires_at_idx"
			}
			return "NULLIF(JSON_UNQUOTE(JSON_EXTRACT(doc,'$." + name + "')),'null')"
		}
		return "json_extract(doc,'$." + name + "')"
	}
	_, e = s.Store.DB.ExecContext(ctx, s.Store.Q("DELETE FROM resources WHERE kind='tasks' AND "+field("status")+"='done' AND "+field("finished_at")+"<?"), cutoff)
	record(e)
	mark("tasks")
	_, e = s.Store.DB.ExecContext(ctx, s.Store.Q("DELETE FROM resources WHERE kind='sessions' AND "+field("expires_at")+"<?"), s.Store.Time(time.Now()))
	record(e)
	mark("sessions")
	// A canceled or superseded maintenance leader must leave the last
	// completed run intact so the next leader retries the unfinished work.
	if ctx.Err() != nil {
		return ctx.Err()
	}
	state, _ := s.Store.Get(ctx, "system-state", "cleanup")
	if state == nil {
		state = domain.Doc{"id": "cleanup"}
	}
	state["finished_at"] = domain.Now()
	state["duration_ms"] = time.Since(started).Milliseconds()
	state["error"] = strings.Join(failures, "; ")
	if owner, leased := ctx.Value(maintenanceOwnerKey{}).(string); leased {
		e = s.Store.PutUnderLease(ctx, maintenanceLease, owner, "system-state", state)
	} else {
		e = s.Store.Put(ctx, "system-state", state)
	}
	if e != nil {
		return e
	}
	if len(failures) > 0 {
		s.Alert(ctx, "warning", "cleanup_failed", "历史数据清理失败", domain.S(state, "error"), "system", "cleanup")
		return errors.New(strings.Join(failures, "; "))
	}
	return nil
}
func (s *Server) Alert(ctx context.Context, severity, category, title, message, kind, id string) {
	key := auth.Hash(category + ":" + kind + ":" + id)
	alert, e := s.Store.Get(ctx, "alerts", key)
	if e == nil && domain.S(alert, "status") != "resolved" {
		alert["occurrences"] = domain.N(alert, "occurrences") + 1
		alert["last_occurred_at"] = domain.Now()
		_ = s.Store.Put(ctx, "alerts", alert)
		return
	}
	previousVersion := alert["_version"]
	created := alert["created_at"]
	alert = domain.Doc{"id": key, "_version": previousVersion, "created_at": created, "incident_id": domain.ID(), "severity": severity, "category": category, "title": title, "message": message, "resource_type": kind, "resource_id": id, "status": "open", "occurrences": 1, "last_occurred_at": domain.Now()}
	if created == nil {
		delete(alert, "created_at")
	}
	if e = s.Store.Put(ctx, "alerts", alert); e == nil {
		s.emit("alerts", key)
		job, _ := s.queueAlertNotification(ctx, alert)
		if job != nil {
			alert["notification_job_id"] = job["id"]
			_ = s.Store.Put(ctx, "alerts", alert)
		}
	}
}

func nodeTask(node, task domain.Doc, requireHTTP bool) bool {
	mode := domain.S(node, "mode")
	if mode == "" {
		mode = "all"
	}
	capabilities := map[string]bool{"tcp": true, "http": true}
	if values, ok := node["capabilities"].([]any); ok {
		capabilities = map[string]bool{}
		for _, value := range values {
			capabilities[fmt.Sprint(value)] = true
		}
	}
	want := domain.S(task, "mode")
	if requireHTTP {
		if mode == "tcp" || !capabilities["http"] {
			return false
		}
		task["mode"] = "http"
		return true
	}
	if want == "tcp" {
		return mode != "http" && capabilities["tcp"]
	}
	if want == "http" {
		return mode != "tcp" && capabilities["http"]
	}
	if mode == "tcp" || !capabilities["http"] {
		if !capabilities["tcp"] {
			return false
		}
		task["mode"] = "tcp"
	} else if mode == "http" || !capabilities["tcp"] {
		task["mode"] = "http"
	}
	return true
}

func nodeInLine(node, line domain.Doc, groups []domain.Doc) bool {
	if domain.S(line, "health_perspective") == "global" {
		return true
	}
	group := domain.S(line, "node_group")
	if group == "" {
		return true
	}
	if domain.S(node, "group_name") == group {
		return true
	}
	for _, g := range groups {
		if domain.S(g, "id") != group && domain.S(g, "name") != group {
			continue
		}
		if domain.S(node, "group_name") == domain.S(g, "name") {
			return true
		}
		if members, ok := g["node_ids"].([]any); ok {
			for _, id := range members {
				if fmt.Sprint(id) == domain.S(node, "id") {
					return true
				}
			}
		}
	}
	return false
}
func profilePeriod(profile domain.Doc, fallback time.Duration) time.Duration {
	if n := domain.N(profile, "interval_seconds"); n >= 5 {
		return time.Duration(n) * time.Second
	}
	return fallback
}

func nodeSubset(nodes []domain.Doc, key string, count int) []domain.Doc {
	if len(nodes) <= count {
		return nodes
	}
	hash := sha256.Sum256([]byte(key))
	start := int(binary.BigEndian.Uint32(hash[:4])) % len(nodes)
	out := make([]domain.Doc, 0, count)
	for i := 0; i < count; i++ {
		out = append(out, nodes[(start+i)%len(nodes)])
	}
	return out
}
func mergeProfile(base, profile domain.Doc) domain.Doc {
	out := domain.Clone(base)
	for k, v := range profile {
		if v == nil {
			continue
		}
		if k == "url" && (v == nil || fmt.Sprint(v) == "") {
			continue
		}
		out[k] = v
	}
	return out
}

type settingsContextKey struct{}

func (s *Server) cachedSettings(ctx context.Context) context.Context {
	rows, e := s.Store.List(ctx, "settings")
	if e != nil {
		return ctx
	}
	values := domain.Doc{}
	for _, row := range rows {
		values[domain.S(row, "key")] = row["value"]
	}
	return context.WithValue(ctx, settingsContextKey{}, values)
}
func (s *Server) probeDefaults(ctx context.Context) domain.Doc {
	values, ok := ctx.Value(settingsContextKey{}).(domain.Doc)
	if !ok {
		rows, _ := s.Store.List(ctx, "settings")
		values = domain.Doc{}
		for _, d := range rows {
			values[domain.S(d, "key")] = d["value"]
		}
	}
	result := domain.Doc{"mode": "all", "url": values["probe.default_url"], "timeout_ms": values["probe.timeout_ms"], "max_bytes": values["probe.max_bytes"], "user_agent": values["probe.default_user_agent"], "retries": values["probe.retries"]}
	for _, field := range []string{"host", "sni", "path", "method", "headers", "expected_statuses", "expected_contains", "samples", "max_redirects"} {
		if value, exists := values["probe."+field]; exists {
			result[field] = value
		}
	}
	if result["method"] == nil {
		result["method"] = "GET"
	}
	if result["samples"] == nil {
		result["samples"] = 3
	}
	if result["max_redirects"] == nil {
		result["max_redirects"] = 3
	}
	return result
}

func (s *Server) monitorExternal(ctx context.Context, now time.Time) {
	if ctx.Err() != nil {
		return
	}
	s.observeStorage(ctx)
	for _, spec := range []struct {
		kind, key string
		period    time.Duration
	}{{"connections", "last_validated_at", 15 * time.Minute}, {"saas/zones", "last_synced_at", 5 * time.Minute}, {"saas/hostnames", "last_checked_at", time.Minute}} {
		rows, _ := s.Store.List(ctx, spec.kind)
		for _, d := range rows {
			if ctx.Err() != nil {
				return
			}
			last, _ := time.Parse(time.RFC3339Nano, domain.S(d, spec.key))
			if now.Sub(last) >= spec.period && !s.hasPending(ctx, spec.kind, domain.S(d, "id"), "sync") {
				_, _ = s.Queue(ctx, "sync", spec.kind, d, fmt.Sprintf("monitor:%d", now.Unix()/int64(spec.period.Seconds())))
			}
		}
	}
	alerts, _ := s.Store.List(ctx, "alerts")
	for _, alert := range alerts {
		if ctx.Err() != nil {
			return
		}
		if domain.S(alert, "status") != "open" || domain.S(alert, "notification_job_id") != "" {
			continue
		}
		job, e := s.queueAlertNotification(ctx, alert)
		if e == nil {
			alert["notification_job_id"] = job["id"]
			_ = s.Store.Put(ctx, "alerts", alert)
		}
	}
}
