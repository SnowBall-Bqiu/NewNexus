package api

import (
	"context"
	"github.com/newnexus/newnexus/internal/domain"
	"sync"
	"testing"
	"time"
)

func TestInheritedQuorumAndNamedNodeGroupApplyWithoutPersistingEffectiveValues(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	groups := []domain.Doc{{"id": "group-id", "name": "east"}}
	nodes := []domain.Doc{{"id": "one", "group_name": "east", "status": "online"}, {"id": "two", "group_name": "east", "status": "online"}}
	if !nodeInLine(nodes[0], domain.Doc{"node_group": "group-id"}, groups) {
		t.Fatal("node group name not mapped to selected group ID")
	}
	for _, d := range []domain.Doc{{"id": "routing.min_nodes", "key": "routing.min_nodes", "value": 3}, {"id": "routing.quorum_percent", "key": "routing.quorum_percent", "value": 100}} {
		old, _ := s.Store.Get(ctx, "settings", domain.S(d, "id"))
		d["_version"] = old["_version"]
		if e := s.Store.Put(ctx, "settings", d); e != nil {
			t.Fatal(e)
		}
	}
	rule := domain.Doc{"id": domain.ID(), "switch_mode": "availability", "publish_mode": "manual", "lines": []domain.Doc{{"id": "line", "node_group": "group-id", "targets": []domain.Doc{{"id": "target", "priority": 0, "enabled": true, "weight": 1}}}}}
	if e := s.Store.Put(ctx, "rules", rule); e != nil {
		t.Fatal(e)
	}
	for _, node := range nodes {
		_, e := s.Store.DB.Exec(`INSERT INTO metrics(id,node_id,candidate_id,observed_at,success,tcp_ms,tls_ms,ttfb_ms,download_bps,sample_count,success_rate)VALUES(?,?,?,?,?,?,?,?,?,?,?)`, domain.ID(), node["id"], "target:target", domain.Now(), 1, 10, 10, 10, 1000, 3, 1)
		if e != nil {
			t.Fatal(e)
		}
	}
	result, e := s.evaluateRule(s.cachedSettings(ctx), rule, nodes, groups)
	if e != nil {
		t.Fatal(e)
	}
	if domain.S(result, "status") != "paused" {
		t.Fatal("global minimum was ignored", result)
	}
	line := domain.A(result, "lines")[0]
	if line["min_nodes"] != nil || line["quorum_percent"] != nil {
		t.Fatal("effective inherited settings were persisted as overrides", line)
	}
	setting, _ := s.Store.Get(ctx, "settings", "routing.min_nodes")
	setting["value"] = 2
	if e = s.Store.Put(ctx, "settings", setting); e != nil {
		t.Fatal(e)
	}
	result, e = s.evaluateRule(s.cachedSettings(ctx), result, nodes, groups)
	if e != nil {
		t.Fatal(e)
	}
	if domain.S(result, "status") != "healthy" {
		t.Fatal("changed global minimum did not take effect", result)
	}
}
func TestProfileIntervalSchedulesAssociatedSitesAndRules(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	profile := domain.Doc{"id": domain.ID(), "name": "fast", "interval_seconds": 5, "mode": "all"}
	if e := s.Store.Put(ctx, "probe-profiles", profile); e != nil {
		t.Fatal(e)
	}
	node := domain.Doc{"id": domain.ID(), "status": "online", "last_seen_at": domain.Now(), "capabilities": []any{"tcp", "http"}}
	_ = s.Store.Put(ctx, "nodes", node)
	site := domain.Doc{"id": domain.ID(), "name": "site", "url": "https://business.example.com/health", "probe_profile_id": profile["id"]}
	_ = s.Store.Put(ctx, "sites", site)
	rule := domain.Doc{"id": domain.ID(), "hostname": "business.example.com", "site_id": site["id"], "publish_mode": "manual", "lines": []domain.Doc{{"id": domain.ID(), "targets": []domain.Doc{{"id": domain.ID(), "cname": "cdn.example.net", "enabled": true, "priority": 0, "weight": 1}}}}}
	_ = s.Store.Put(ctx, "rules", rule)
	s.ScheduleTick(ctx)
	tasks, _ := s.Store.List(ctx, "tasks")
	if len(tasks) != 2 {
		t.Fatal("initial associated tasks missing", tasks)
	}
	for _, task := range tasks {
		task["status"] = "done"
		if e := s.Store.Put(ctx, "tasks", task); e != nil {
			t.Fatal(e)
		}
	}
	at := time.Now().UTC().Add(-10 * time.Second).Format(time.RFC3339Nano)
	_ = s.Store.SetSchedule(ctx, "sites", "last_probe_scheduled_at", []string{domain.S(site, "id")}, at)
	_ = s.Store.SetSchedule(ctx, "rules", "last_probe_scheduled_at", []string{domain.S(rule, "id")}, at)
	s.ScheduleTick(ctx)
	tasks, _ = s.Store.List(ctx, "tasks")
	if len(tasks) != 4 {
		t.Fatal("5s profile interval fell back to global 30s", len(tasks))
	}
	s.DevAuth = true
	w := request(t, s, "POST", "probe-profiles", domain.Doc{"name": "invalid", "interval_seconds": 4}, nil, "")
	if w.Code != 400 {
		t.Fatal("out of range interval accepted", w.Code, w.Body.String())
	}
}
func TestGlobalHTTPTemplateInheritanceAndValidation(t *testing.T) {
	s := testServer(t)
	s.DevAuth = true
	ctx := context.Background()
	values := domain.Doc{"probe.host": "virtual.example.com", "probe.sni": "tls.example.com", "probe.path": "/ready", "probe.method": "POST", "probe.headers": domain.Doc{"X-Check": "global"}, "probe.expected_statuses": []any{201, 204}, "probe.expected_contains": "ready", "probe.samples": 7, "probe.max_redirects": 0, "probe.timeout_ms": 8000}
	if w := request(t, s, "PUT", "settings", values, nil, ""); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	profile := domain.Doc{"name": "inherited", "url": "https://business.example.com/original"}
	if e := s.prepare(ctx, "probe-profiles", profile); e != nil {
		t.Fatal(e)
	}
	task := s.task(s.cachedSettings(ctx), domain.Doc{"id": "candidate", "address": "1.1.1.1"}, profile)
	if domain.S(task, "url") != "https://business.example.com/ready" || domain.S(task, "host") != "virtual.example.com" || domain.S(task, "sni") != "tls.example.com" || domain.S(task, "method") != "POST" || domain.N(task, "samples") != 7 || domain.N(task, "max_redirects") != 0 || domain.N(task, "timeout_ms") != 8000 || domain.S(task, "expected_contains") != "ready" || domain.S(domain.M(task, "headers"), "X-Check") != "global" {
		t.Fatal("global template was not inherited", task)
	}
	profile["samples"] = 1
	profile["method"] = "HEAD"
	profile["path"] = "/override"
	overridden := s.task(s.cachedSettings(ctx), domain.Doc{"id": "candidate", "address": "1.1.1.1"}, profile)
	if domain.N(overridden, "samples") != 1 || domain.S(overridden, "method") != "HEAD" || domain.S(overridden, "url") != "https://business.example.com/override" || domain.S(overridden, "host") != "virtual.example.com" {
		t.Fatal("profile partial override erased defaults", overridden)
	}
	for _, bad := range []domain.Doc{{"probe.samples": 11}, {"probe.max_redirects": 6}, {"probe.method": "GET HEAD"}, {"probe.headers": domain.Doc{"X-Test": "a\r\nb"}}, {"probe.expected_statuses": []any{99}}, {"probe.path": "relative"}, {"score.weights": domain.Doc{"tcp tls": 1}}, {"score.weights": domain.Doc{"tcp": "garbage", "ttfb": 1}}} {
		if w := request(t, s, "PUT", "settings", bad, nil, ""); w.Code != 400 {
			t.Fatal("invalid global template accepted", bad, w.Code, w.Body.String())
		}
	}
}
func TestMaskedOrBlankSecretsPreserveEncryptedCredentials(t *testing.T) {
	s := testServer(t)
	s.DevAuth = true
	ctx := context.Background()
	initial := domain.Doc{"alerts.smtp_password": "actual-smtp-password", "alerts.webhook_secret": "actual-webhook-secret-at-least-16"}
	if w := request(t, s, "PUT", "settings", initial, nil, ""); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, replacement := range []string{"••••••••", ""} {
		if w := request(t, s, "PUT", "settings", domain.Doc{"alerts.smtp_password": replacement, "alerts.webhook_secret": replacement, "probe.timeout_ms": 7000}, nil, ""); w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		for key, value := range initial {
			if got := s.settingString(ctx, key); got != value {
				t.Fatalf("%s overwritten by masked/blank edit: %q", key, got)
			}
		}
	}
}
func TestSchedulerImmediatelyClaimsOneLeaderOnStartup(t *testing.T) {
	s := testServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	second := New(s.Store, s.Vault, s.Hub, rejectingOperations{})
	node := domain.Doc{"id": domain.ID(), "status": "online", "last_seen_at": domain.Now(), "capabilities": []any{"tcp", "http"}}
	candidate := domain.Doc{"id": domain.ID(), "address": "1.1.1.1", "tier": "active"}
	if e := s.Store.Put(ctx, "nodes", node); e != nil {
		t.Fatal(e)
	}
	if e := s.Store.Put(ctx, "candidates", candidate); e != nil {
		t.Fatal(e)
	}
	var workers sync.WaitGroup
	for _, replica := range []*Server{s, second} {
		workers.Add(1)
		go func(api *Server) { defer workers.Done(); api.scheduler(ctx) }(replica)
	}
	defer func() { cancel(); workers.Wait() }()
	deadline := time.Now().Add(2 * time.Second)
	for s.Store.Count(ctx, "tasks") == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if s.Store.Count(ctx, "tasks") != 1 {
		t.Fatal("startup waited for the 5s ticker or allowed duplicate leaders")
	}
	time.Sleep(50 * time.Millisecond)
	if s.Store.Count(ctx, "tasks") != 1 {
		t.Fatal("second replica dispatched the same startup probe")
	}
}
