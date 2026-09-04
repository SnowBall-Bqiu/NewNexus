package scheduler

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/SnowBall-Bqiu/NewNexus/internal/agenthub"
	"github.com/SnowBall-Bqiu/NewNexus/internal/config"
	"github.com/SnowBall-Bqiu/NewNexus/internal/domain"
	"github.com/SnowBall-Bqiu/NewNexus/internal/events"
	"github.com/SnowBall-Bqiu/NewNexus/internal/jobs"
	"github.com/SnowBall-Bqiu/NewNexus/internal/optimizer"
	"github.com/SnowBall-Bqiu/NewNexus/internal/store"
)

func TestParseStatuses(t *testing.T) {
	for _, test := range []struct {
		raw  string
		want int
	}{{"[200,204,301]", 3}, {"200,204", 2}, {"", 2}} {
		if values := parseStatuses(test.raw); len(values) != test.want {
			t.Fatalf("parseStatuses(%q): expected %d values, got %#v", test.raw, test.want, values)
		}
	}
}

func TestMaintenanceUsesIndependentFiveMinuteCadence(t *testing.T) {
	now := time.Now().UTC()
	scheduler := &Scheduler{maintenanceEvery: 5 * time.Minute, lastMaintenance: now}
	if scheduler.maintenanceDue(now.Add(4 * time.Minute)) {
		t.Fatal("maintenance became due before its independent interval")
	}
	if !scheduler.maintenanceDue(now.Add(5 * time.Minute)) {
		t.Fatal("maintenance did not become due at its interval")
	}
}

func TestRouteTaskUsesSiteHostAndMergesOverrides(t *testing.T) {
	scheduler := &Scheduler{}
	task := scheduler.routeTask(
		domain.Route{Hostname: "www.business.example"},
		domain.RouteTarget{Model: domain.Model{ID: "target"}, CNAME: "customer.cdn.example", ProbeOverrideJSON: `{"port":8443,"headers":{"X-Target":"yes"}}`},
		domain.ProbeProfile{URL: "https://global.example/health", Method: "GET", HeadersJSON: `{"X-Base":"yes"}`, TimeoutMS: 5000},
		domain.ProjectSite{URL: "https://www.business.example/status", ProbeOverridesJSON: `{"method":"HEAD","timeoutMs":2500,"headers":{"X-Site":"yes"}}`},
	)
	if task.Address != "customer.cdn.example" || task.URL != "https://www.business.example/status" {
		t.Fatalf("task does not use the site URL through the target: %#v", task)
	}
	if task.Host != "www.business.example" || task.SNI != "www.business.example" {
		t.Fatalf("Host/SNI must remain the business hostname: %#v", task)
	}
	if task.Method != "HEAD" || task.TimeoutMS != 2500 || task.Port != 8443 {
		t.Fatalf("probe overrides were not applied: %#v", task)
	}
	for _, header := range []string{"X-Base", "X-Site", "X-Target"} {
		if task.Headers[header] != "yes" {
			t.Fatalf("missing merged header %s: %#v", header, task.Headers)
		}
	}
}

func TestTickSchedulesSourceWithoutConnectedAgents(t *testing.T) {
	database, err := store.Open(config.Config{DatabaseDriver: "sqlite", DatabaseURL: filepath.Join(t.TempDir(), "scheduler.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	project := domain.Project{Name: "project", Status: "active"}
	if err := database.DB.Create(&project).Error; err != nil {
		t.Fatal(err)
	}
	source := domain.CandidateSource{ProjectID: project.ID, Name: "source", Type: "manual", Enabled: true, ConfigJSON: `{"content":"1.1.1.1"}`}
	if err := database.DB.Create(&source).Error; err != nil {
		t.Fatal(err)
	}
	broker := events.NewBroker()
	manager := jobs.New(database, broker)
	scheduler := NewWithJobs(database, agenthub.New(database, broker), optimizer.New(database, broker), nil, manager, time.Second, 6*time.Hour, 5*time.Minute, 30*time.Second)
	scheduler.tick(context.Background())
	var count int64
	if err := database.DB.Model(&domain.Job{}).Where("type = ? AND resource_id = ?", "optimizer.source.sync", source.ID).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("expected source sync job without connected agents, got %d", count)
	}
}

func TestDispatchTargetsPersistsAndClearsSamplePause(t *testing.T) {
	database, err := store.Open(config.Config{DatabaseDriver: "sqlite", DatabaseURL: filepath.Join(t.TempDir(), "samples.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	route := domain.Route{Name: "route", Hostname: "www.example.com", Status: "healthy"}
	if err := database.DB.Create(&route).Error; err != nil {
		t.Fatal(err)
	}
	line := domain.RouteLine{RouteID: route.ID, Alias: "电信", MinNodes: 2, Status: "healthy"}
	if err := database.DB.Create(&line).Error; err != nil {
		t.Fatal(err)
	}
	target := domain.RouteTarget{RouteLineID: line.ID, Name: "primary", CNAME: "cdn.example.net", Priority: 10, Weight: 100, Enabled: true, Health: "unknown"}
	if err := database.DB.Create(&target).Error; err != nil {
		t.Fatal(err)
	}
	scheduler := New(database, agenthub.New(database, events.NewBroker()), nil, nil, time.Second)
	if err := scheduler.dispatchTargets(nil); err != nil {
		t.Fatal(err)
	}
	if err := database.DB.First(&line, "id = ?", line.ID).Error; err != nil {
		t.Fatal(err)
	}
	if !line.AutoSwitchPaused || line.PauseReason == "" {
		t.Fatalf("sample shortage did not persist the automatic-switch pause: %#v", line)
	}
	var alert domain.Alert
	if err := database.DB.First(&alert, "dedupe_key = ?", "route.samples:"+line.ID).Error; err != nil || alert.Status != "open" {
		t.Fatalf("sample shortage alert missing: %#v, %v", alert, err)
	}
	expired := time.Now().UTC().Add(-time.Minute)
	if err := database.DB.Model(&alert).Updates(map[string]any{"status": "silenced", "silenced_until": expired}).Error; err != nil {
		t.Fatal(err)
	}
	alertID := alert.ID
	if err := scheduler.dispatchTargets(nil); err != nil {
		t.Fatal(err)
	}
	alert = domain.Alert{}
	if err := database.DB.First(&alert, "id = ?", alertID).Error; err != nil || alert.Status != "open" || alert.SilencedUntil != nil {
		t.Fatalf("expired sample silence did not reopen: %#v, %v", alert, err)
	}
	if err := scheduler.dispatchTargets([]string{"node-a", "node-b"}); err != nil {
		t.Fatal(err)
	}
	if err := database.DB.First(&line, "id = ?", line.ID).Error; err != nil {
		t.Fatal(err)
	}
	if line.AutoSwitchPaused || line.PauseReason != "" {
		t.Fatalf("sample pause was not cleared after quorum recovered: %#v", line)
	}
	if err := database.DB.First(&alert, "id = ?", alert.ID).Error; err != nil || alert.Status != "resolved" {
		t.Fatalf("sample alert was not resolved: %#v, %v", alert, err)
	}
}

func TestOfflineAlertKeepsNodeStatusAndAlertInSync(t *testing.T) {
	database, err := store.Open(config.Config{DatabaseDriver: "sqlite", DatabaseURL: filepath.Join(t.TempDir(), "offline.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	stale := time.Now().UTC().Add(-2 * time.Minute)
	node := domain.ProbeNode{Name: "edge-one", Status: "online", LastSeenAt: &stale}
	if err := database.DB.Create(&node).Error; err != nil {
		t.Fatal(err)
	}
	scheduler := New(database, nil, nil, nil, time.Second)
	scheduler.raiseOfflineAlerts()
	if err := database.DB.First(&node, "id = ?", node.ID).Error; err != nil {
		t.Fatal(err)
	}
	if node.Status != "offline" {
		t.Fatalf("stale node remained %q", node.Status)
	}
	var alert domain.Alert
	if err := database.DB.First(&alert, "dedupe_key = ?", "agent.offline:"+node.ID).Error; err != nil || alert.Status != "open" {
		t.Fatalf("offline alert missing: %#v, %v", alert, err)
	}
	now := time.Now().UTC()
	if err := database.DB.Model(&node).Updates(map[string]any{"status": "online", "last_seen_at": now}).Error; err != nil {
		t.Fatal(err)
	}
	scheduler.raiseOfflineAlerts()
	if err := database.DB.First(&alert, "id = ?", alert.ID).Error; err != nil || alert.Status != "resolved" {
		t.Fatalf("recovered node alert was not resolved: %#v, %v", alert, err)
	}
}
