package routing

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/SnowBall-Bqiu/NewNexus/internal/config"
	"github.com/SnowBall-Bqiu/NewNexus/internal/domain"
	"github.com/SnowBall-Bqiu/NewNexus/internal/events"
	"github.com/SnowBall-Bqiu/NewNexus/internal/provider"
	"github.com/SnowBall-Bqiu/NewNexus/internal/store"
)

func TestActiveTargetsUsesFirstHealthyPriorityGroup(t *testing.T) {
	targets := []domain.RouteTarget{
		{Name: "primary-a", Priority: 10, Weight: 70, Enabled: true, Health: "unhealthy"},
		{Name: "primary-b", Priority: 10, Weight: 30, Enabled: true, Health: "unhealthy"},
		{Name: "backup", Priority: 20, Weight: 100, Enabled: true, Health: "healthy"},
	}
	active := activeTargets(targets)
	if len(active) != 1 || active[0].Name != "backup" {
		t.Fatalf("expected backup priority group, got %#v", active)
	}
}

func TestAutomaticEvaluationSkipsSamplePausedLine(t *testing.T) {
	database, err := store.Open(config.Config{DatabaseDriver: "sqlite", DatabaseURL: filepath.Join(t.TempDir(), "route.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	route := domain.Route{Name: "route", Hostname: "www.example.com", PublishMode: "automatic", SwitchMode: "availability", Status: "healthy"}
	if err := database.DB.Create(&route).Error; err != nil {
		t.Fatal(err)
	}
	line := domain.RouteLine{RouteID: route.ID, Alias: "电信", ActivePriority: 10, AutoSwitchPaused: true, PauseReason: "样本不足", Status: "healthy"}
	if err := database.DB.Create(&line).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	targets := []domain.RouteTarget{
		{RouteLineID: line.ID, Name: "primary", CNAME: "primary.example.net", Priority: 10, Weight: 100, Enabled: true, Health: "unhealthy", LastCheckedAt: &now},
		{RouteLineID: line.ID, Name: "backup", CNAME: "backup.example.net", Priority: 20, Weight: 100, Enabled: true, Health: "healthy", LastCheckedAt: &now},
	}
	if err := database.DB.Create(&targets).Error; err != nil {
		t.Fatal(err)
	}
	service := New(database, nil, nil, events.NewBroker())
	published := 0
	if err := service.EvaluateAutomaticRoutesWith(context.Background(), func(string) error { published++; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := database.DB.First(&line, "id = ?", line.ID).Error; err != nil {
		t.Fatal(err)
	}
	if published != 0 || line.ActivePriority != 10 {
		t.Fatalf("sample-paused line was switched: published=%d line=%#v", published, line)
	}
}

func TestRaiseAlertHonorsActiveSilenceAndReopensExpiredSilence(t *testing.T) {
	database, err := store.Open(config.Config{DatabaseDriver: "sqlite", DatabaseURL: filepath.Join(t.TempDir(), "alerts.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	service := New(database, nil, nil, events.NewBroker())
	service.raiseAlert("critical", "route", "目标不可用", "first", "route_line", "line-1")
	var alert domain.Alert
	if err := database.DB.First(&alert, "resource_id = ?", "line-1").Error; err != nil {
		t.Fatal(err)
	}
	future := time.Now().UTC().Add(time.Hour)
	if err := database.DB.Model(&alert).Updates(map[string]any{"status": "silenced", "silenced_until": future}).Error; err != nil {
		t.Fatal(err)
	}
	service.raiseAlert("critical", "route", "目标不可用", "still failing", "route_line", "line-1")
	if err := database.DB.First(&alert, "id = ?", alert.ID).Error; err != nil {
		t.Fatal(err)
	}
	if alert.Status != "silenced" || alert.SilencedUntil == nil {
		t.Fatalf("active silence was cleared by a repeated condition: %#v", alert)
	}
	past := time.Now().UTC().Add(-time.Minute)
	if err := database.DB.Model(&alert).Update("silenced_until", past).Error; err != nil {
		t.Fatal(err)
	}
	alertID := alert.ID
	service.raiseAlert("critical", "route", "目标不可用", "still failing", "route_line", "line-1")
	alert = domain.Alert{}
	if err := database.DB.First(&alert, "id = ?", alertID).Error; err != nil {
		t.Fatal(err)
	}
	if alert.Status != "open" || alert.SilencedUntil != nil {
		t.Fatalf("expired silence was not reopened: %#v", alert)
	}
}

func TestActiveTargetsFailStaleWhenEverythingIsUnhealthy(t *testing.T) {
	targets := []domain.RouteTarget{{Priority: 10, Enabled: true, Health: "unhealthy"}, {Priority: 20, Enabled: true, Health: "unhealthy"}}
	if active := activeTargets(targets); len(active) != 0 {
		t.Fatalf("all-failure policy must emit no desired records, got %#v", active)
	}
}

func TestActiveTargetsKeepsFixedWeights(t *testing.T) {
	targets := []domain.RouteTarget{{Name: "a", Priority: 10, Weight: 30, Enabled: true, Health: "healthy"}, {Name: "b", Priority: 10, Weight: 70, Enabled: true, Health: "healthy"}}
	active := activeTargets(targets)
	if len(active) != 2 || active[0].Weight != 70 || active[1].Weight != 30 {
		t.Fatalf("fixed provider weights changed: %#v", active)
	}
}

func TestRetainedPriorityDoesNotJumpOnUnknownSamples(t *testing.T) {
	targets := []domain.RouteTarget{{Priority: 10, Enabled: true, Health: "unknown"}, {Priority: 20, Enabled: true, Health: "healthy"}}
	retained := retainedTargetsAtPriority(targets, 10)
	if len(retained) != 1 || retained[0].Priority != 10 {
		t.Fatalf("sample shortage must retain the current group: %#v", retained)
	}
}

func TestPreferredPriorityRequiresTwentyPercentImprovement(t *testing.T) {
	targets := []domain.RouteTarget{{Priority: 10, Enabled: true, Health: "healthy", Score: 80}, {Priority: 20, Enabled: true, Health: "healthy", Score: 95}}
	if priority := preferredPriority(targets, 10, "availability_performance"); priority != 0 {
		t.Fatalf("18.75%% improvement should not switch, got P%d", priority)
	}
	targets[1].Score = 96
	if priority := preferredPriority(targets, 10, "availability_performance"); priority != 20 {
		t.Fatalf("20%% improvement should select P20, got P%d", priority)
	}
	if priority := preferredPriority(targets, 10, "availability"); priority != 0 {
		t.Fatalf("availability-only mode selected performance candidate P%d", priority)
	}
}

func TestCNAMEAndApexValidation(t *testing.T) {
	if err := validateCNAME("www.example.com", "cdn.example.net"); err != nil {
		t.Fatal(err)
	}
	if err := validateCNAME("www.example.com", "edge.www.example.com"); err == nil {
		t.Fatal("descendant CNAME loop was accepted")
	}
	if err := validateHostname("example.com", "example.com"); err == nil {
		t.Fatal("apex CNAME was accepted without provider capability")
	}
}

func TestReconcileIsIdempotent(t *testing.T) {
	existing := []provider.Record{{ID: "record-1", Name: "www.example.com", Type: "CNAME", Value: "cdn.example.net", LineCode: "0", TTL: 60, Weight: 100}}
	desired := []provider.RecordChange{{Operation: "create", After: &provider.Record{Name: "www.example.com", Type: "CNAME", Value: "cdn.example.net", LineCode: "0", TTL: 60, Weight: 100}}}
	if changes := reconcile(existing, desired); len(changes) != 0 {
		t.Fatalf("identical desired state produced changes: %#v", changes)
	}
}
