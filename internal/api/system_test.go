package api

import (
	"context"
	"github.com/newnexus/newnexus/internal/domain"
	"testing"
	"time"
)

func TestSystemStatusObservesStorageAndCleanup(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	status := s.systemStatus(ctx)
	if status["raw_samples"] == nil || status["db_bytes"] == nil {
		t.Fatal(status)
	}
	remote := s.Store.Driver == "mysql"
	if remote {
		if domain.S(status, "database") != "mysql" || !domain.B(status, "db_bytes_estimated") || len(domain.A(status, "partitions")) < 3 || status["disk_total_bytes"] != nil {
			t.Fatal(status)
		}
	} else if domain.S(status, "database") != "sqlite" || domain.N(status, "db_bytes") <= 0 || domain.N(status, "disk_total_bytes") <= 0 {
		t.Fatal(status)
	}
	s.cleanup(ctx)
	status = s.systemStatus(ctx)
	if domain.S(status, "last_cleanup_at") == "" || domain.S(status, "last_cleanup_error") != "" {
		t.Fatal(status)
	}
	if remote {
		s.observeStorage(ctx)
		if domain.S(status, "disk_observation") == "" {
			t.Fatal("remote storage limitations must be explicit")
		}
		return
	}
	warning, _ := s.Store.Get(ctx, "settings", "system.disk_warning_percent")
	warning["value"] = 100.
	if e := s.Store.Put(ctx, "settings", warning); e != nil {
		t.Fatal(e)
	}
	s.observeStorage(ctx)
	alerts, _ := s.Store.List(ctx, "alerts")
	found := false
	for _, a := range alerts {
		if domain.S(a, "category") == "disk_warning" {
			found = true
		}
	}
	if !found {
		t.Fatal("configured low disk watermark did not produce an alert", alerts)
	}
}

func TestCleanupPreservesActiveTasksAndSessions(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	old := s.Store.Time(time.Now().Add(-10 * 24 * time.Hour))
	for _, d := range []domain.Doc{
		{"id": "expired-done", "status": "done", "finished_at": old},
		{"id": "old-queued", "status": "queued", "created_at": old},
		{"id": "recent-done", "status": "done", "finished_at": s.Store.Time(time.Now())},
	} {
		if err := s.Store.Put(ctx, "tasks", d); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range []domain.Doc{
		{"id": "expired-session", "expires_at": old},
		{"id": "active-session", "expires_at": s.Store.Time(time.Now().Add(time.Hour))},
	} {
		if err := s.Store.Put(ctx, "sessions", d); err != nil {
			t.Fatal(err)
		}
	}
	s.cleanup(ctx)
	for _, item := range []struct {
		kind, id string
		exists   bool
	}{
		{"tasks", "expired-done", false}, {"tasks", "old-queued", true}, {"tasks", "recent-done", true},
		{"sessions", "expired-session", false}, {"sessions", "active-session", true},
	} {
		_, err := s.Store.Get(ctx, item.kind, item.id)
		if (err == nil) != item.exists {
			t.Fatalf("cleanup %s/%s exists=%v: %v", item.kind, item.id, item.exists, err)
		}
	}
	state, err := s.Store.Get(ctx, "system-state", "cleanup")
	if err != nil || domain.S(state, "error") != "" {
		t.Fatal(state, err)
	}
}
