package jobs

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/SnowBall-Bqiu/NewNexus/internal/config"
	"github.com/SnowBall-Bqiu/NewNexus/internal/domain"
	"github.com/SnowBall-Bqiu/NewNexus/internal/events"
	"github.com/SnowBall-Bqiu/NewNexus/internal/store"
	"github.com/SnowBall-Bqiu/NewNexus/internal/testutil"
)

func TestEnqueueReusesIdempotencyKey(t *testing.T) {
	database, err := store.Open(config.Config{DatabaseDriver: "sqlite", DatabaseURL: filepath.Join(t.TempDir(), "jobs.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	manager := New(database, events.NewBroker())
	first, reused, err := manager.Enqueue(EnqueueOptions{Type: "test", ResourceType: "route", ResourceID: "one", IdempotencyKey: "request-one"})
	if err != nil || reused {
		t.Fatalf("first enqueue: reused=%v err=%v", reused, err)
	}
	second, reused, err := manager.Enqueue(EnqueueOptions{Type: "test", ResourceType: "route", ResourceID: "one", IdempotencyKey: "request-one"})
	if err != nil || !reused || second.ID != first.ID {
		t.Fatalf("idempotency failed: %#v %#v %v", first, second, err)
	}
}

func TestCancelledRunningJobCannotFinishSucceeded(t *testing.T) {
	database, err := store.Open(config.Config{DatabaseDriver: "sqlite", DatabaseURL: filepath.Join(t.TempDir(), "cancel.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	manager := New(database, events.NewBroker())
	job, _, err := manager.Enqueue(EnqueueOptions{Type: "test", ResourceID: "one"})
	if err != nil {
		t.Fatal(err)
	}
	claimed, ok, err := manager.claim("worker")
	if err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	if _, err := manager.Cancel(job.ID); err != nil {
		t.Fatal(err)
	}
	attempt := domain.JobAttempt{JobID: job.ID, Attempt: 1, Worker: "worker", Status: StatusRunning, StartedAt: time.Now().UTC()}
	if err := database.DB.Create(&attempt).Error; err != nil {
		t.Fatal(err)
	}
	manager.finish(claimed, attempt, map[string]bool{"ok": true}, nil)
	var stored domain.Job
	if err := database.DB.First(&stored, "id = ?", job.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Status != StatusCancelled {
		t.Fatalf("cancelled job became %s", stored.Status)
	}
}

func TestExecutionKeySerializesJobs(t *testing.T) {
	database, err := store.Open(config.Config{DatabaseDriver: "sqlite", DatabaseURL: filepath.Join(t.TempDir(), "execution-key.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	manager := New(database, events.NewBroker())
	for _, resourceID := range []string{"release-one", "release-two"} {
		if _, _, err := manager.Enqueue(EnqueueOptions{Type: "route.apply", ResourceType: "route", ResourceID: resourceID, ExecutionKey: "route:shared"}); err != nil {
			t.Fatal(err)
		}
	}
	first, ok, err := manager.claim("worker-one")
	if err != nil || !ok {
		t.Fatalf("first claim failed: ok=%v err=%v", ok, err)
	}
	if _, ok, err := manager.claim("worker-two"); err != nil || ok {
		t.Fatalf("second job bypassed execution lock: ok=%v err=%v", ok, err)
	}
	attempt := domain.JobAttempt{JobID: first.ID, Attempt: first.Attempts, Worker: "worker-one", Status: StatusRunning, StartedAt: time.Now().UTC()}
	if err := database.DB.Create(&attempt).Error; err != nil {
		t.Fatal(err)
	}
	manager.finish(first, attempt, map[string]bool{"ok": true}, nil)
	if _, ok, err := manager.claim("worker-two"); err != nil || !ok {
		t.Fatalf("queued job was not claimable after lock release: ok=%v err=%v", ok, err)
	}
}

func TestPostgresWorkersClaimDifferentJobs(t *testing.T) {
	databaseURL := testutil.IsolatedPostgresURL(t)
	database, err := store.Open(config.Config{DatabaseDriver: "postgres", DatabaseURL: databaseURL})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	managerOne := New(database, events.NewBroker())
	managerTwo := New(database, events.NewBroker())
	for _, resourceID := range []string{"one", "two"} {
		if _, _, err := managerOne.Enqueue(EnqueueOptions{Type: "test", ResourceID: resourceID}); err != nil {
			t.Fatal(err)
		}
	}

	start := make(chan struct{})
	type claimResult struct {
		job domain.Job
		ok  bool
		err error
	}
	results := make(chan claimResult, 2)
	var workers sync.WaitGroup
	for worker, manager := range map[string]*Manager{"worker-one": managerOne, "worker-two": managerTwo} {
		workers.Add(1)
		go func(worker string, manager *Manager) {
			defer workers.Done()
			<-start
			job, ok, err := manager.claim(worker)
			results <- claimResult{job: job, ok: ok, err: err}
		}(worker, manager)
	}
	close(start)
	workers.Wait()
	close(results)

	claimed := make(map[string]string)
	for result := range results {
		if result.err != nil || !result.ok {
			t.Fatalf("claim failed: ok=%v err=%v", result.ok, result.err)
		}
		if previousWorker, duplicate := claimed[result.job.ID]; duplicate {
			t.Fatalf("job %s was claimed twice, first by %s", result.job.ID, previousWorker)
		}
		claimed[result.job.ID] = result.job.LeaseOwner
	}
	if len(claimed) != 2 {
		t.Fatalf("expected two distinct claims, got %d", len(claimed))
	}
}
