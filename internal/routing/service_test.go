package routing

import (
	"testing"

	"github.com/SnowBall-Bqiu/NewNexus/internal/domain"
	"github.com/SnowBall-Bqiu/NewNexus/internal/provider"
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
