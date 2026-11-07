package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/newnexus/newnexus/internal/agenthub"
	"github.com/newnexus/newnexus/internal/agentproto"
	"github.com/newnexus/newnexus/internal/domain"
	"github.com/newnexus/newnexus/internal/store"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/structpb"
)

func TestMaintenanceReplicasShareCompletionAndRetryUnfinishedRun(t *testing.T) {
	s := testServer(t)
	other := New(s.Store, s.Vault, agenthub.New(s.Store), s.Operations)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	type result struct {
		ran bool
		err error
	}
	results := make(chan result, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, replica := range []*Server{s, other} {
		wg.Add(1)
		go func(s *Server) {
			defer wg.Done()
			<-start
			ran, err := s.maintenancePass(ctx, domain.ID())
			results <- result{ran, err}
		}(replica)
	}
	close(start)
	wg.Wait()
	close(results)
	runs := 0
	for r := range results {
		if r.err != nil {
			t.Fatal(r.err)
		}
		if r.ran {
			runs++
		}
	}
	if runs != 1 {
		t.Fatalf("replicas executed %d initial cleanups, want 1", runs)
	}
	state, err := s.Store.Get(ctx, "system-state", "cleanup")
	if err != nil || domain.S(state, "finished_at") == "" || domain.S(state, "error") != "" {
		t.Fatal(state, err)
	}
	version := state["_version"]
	if ran, err := other.maintenancePass(ctx, domain.ID()); err != nil || ran {
		t.Fatal("new leader repeated recent global cleanup", ran, err)
	}
	state, err = s.Store.Get(ctx, "system-state", "cleanup")
	if err != nil || state["_version"] != version {
		t.Fatal("skipped maintenance changed completion", state, err)
	}
	state["finished_at"] = ""
	if err = s.Store.Put(ctx, "system-state", state); err != nil {
		t.Fatal(err)
	}
	if ran, err := other.maintenancePass(ctx, domain.ID()); err != nil || !ran {
		t.Fatal("new leader did not retry unfinished cleanup", ran, err)
	}
	var leases int
	if err = s.Store.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM job_leases WHERE id=?", maintenanceLease).Scan(&leases); err != nil || leases != 0 {
		t.Fatal("maintenance lease was not released", leases, err)
	}
}

func TestMaintenanceMySQLBlockedDDLDoesNotStopSchedulingAndCancels(t *testing.T) {
	s := testServer(t)
	if s.Store.Driver != "mysql" {
		t.Skip("requires NEWNEXUS_TEST_BACKEND=mysql for real MySQL session-lock gate")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	agentproto.RegisterAgentServiceServer(server, s.Hub)
	go server.Serve(listener)
	defer server.Stop()
	connection, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	client := agentproto.NewAgentServiceClient(connection)
	token, err := s.Hub.Enrollment(ctx, time.Minute, "maintenance-check")
	if err != nil {
		t.Fatal(err)
	}
	enrollment, _ := structpb.NewStruct(map[string]any{"enrollment_token": token["token"], "protocol_version": 1})
	identity, err := client.Enroll(ctx, enrollment)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := client.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	credentials := identity.AsMap()
	hello, _ := structpb.NewStruct(map[string]any{"type": "hello", "protocol_version": 1, "node_id": credentials["node_id"], "credential": credentials["credential"]})
	if err = stream.Send(hello); err != nil {
		t.Fatal(err)
	}
	if _, err = stream.Recv(); err != nil {
		t.Fatal(err)
	}
	heartbeat, _ := structpb.NewStruct(map[string]any{"type": "heartbeat", "capabilities": []any{"tcp", "http"}})
	if err = stream.Send(heartbeat); err != nil {
		t.Fatal(err)
	}
	for {
		node, err := s.Store.Get(ctx, "nodes", credentials["node_id"].(string))
		if err != nil {
			t.Fatal(err)
		}
		if domain.S(node, "status") == "online" {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err = s.Store.Put(ctx, "candidates", domain.Doc{"id": "maintenance-candidate", "address": "1.1.1.1", "tier": "active"}); err != nil {
		t.Fatal(err)
	}
	// GET_LOCK uses a pinned MySQL session and genuinely blocks the first
	// production partition-maintenance operation without blocking scheduler SQL.
	blocker, err := s.Store.DB.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close()
	var database string
	if err = blocker.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&database); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte(database))
	lockName := "newnexus-ddl-" + hex.EncodeToString(hash[:24])
	var held int
	if err = blocker.QueryRowContext(ctx, "SELECT GET_LOCK(?,0)", lockName).Scan(&held); err != nil || held != 1 {
		t.Fatal(held, err)
	}
	defer blocker.ExecContext(context.Background(), "DO RELEASE_LOCK(?)", lockName)
	workCtx, stop := context.WithCancel(ctx)
	defer stop()
	maintenanceDone := make(chan struct{})
	go func() { defer close(maintenanceDone); s.maintenance(workCtx) }()
	for {
		var n int
		if err = s.Store.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM job_leases WHERE id=?", maintenanceLease).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n == 1 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	schedulerDone := make(chan struct{})
	go func() { defer close(schedulerDone); s.scheduler(workCtx) }()
	var task domain.Doc
	for {
		frame, err := stream.Recv()
		if err != nil {
			t.Fatal("scheduler did not push a task while maintenance was blocked", err)
		}
		message := domain.Doc(frame.AsMap())
		if domain.S(message, "type") == "task" {
			task = domain.M(message, "task")
			break
		}
	}
	stored, err := s.Store.Get(ctx, "tasks", domain.S(task, "id"))
	if err != nil || domain.S(stored, "candidate_id") != "maintenance-candidate" {
		t.Fatal("pushed task was not durable", stored, err)
	}
	candidate, err := s.Store.Get(ctx, "candidates", "maintenance-candidate")
	if err != nil || domain.S(candidate, "last_scheduled_at") == "" {
		t.Fatal("scheduler did not persist scheduling time", candidate, err)
	}
	if state, err := s.Store.Get(ctx, "system-state", "cleanup"); err == nil && domain.S(state, "finished_at") != "" {
		t.Fatal("blocked maintenance was reported complete", state)
	}
	stop()
	for _, done := range []chan struct{}{maintenanceDone, schedulerDone} {
		select {
		case <-done:
		case <-ctx.Done():
			t.Fatal("canceled worker did not stop", ctx.Err())
		}
	}
	var leases int
	if err = s.Store.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM job_leases WHERE id=?", maintenanceLease).Scan(&leases); err != nil || leases != 0 {
		t.Fatal("canceled maintenance did not release lease", leases, err)
	}
	if state, err := s.Store.Get(ctx, "system-state", "cleanup"); err == nil && domain.S(state, "finished_at") != "" {
		t.Fatal("canceled cleanup wrote successful completion", state)
	}
}

func TestMaintenanceCompletionRejectsSupersededWorkerWithLiveContext(t *testing.T) {
	s := testServer(t)
	ctx := t.Context()
	state := domain.Doc{"id": "cleanup", "finished_at": domain.Now(), "error": "", "completed_by": "new-leader"}
	if err := s.Store.Put(ctx, "system-state", state); err != nil {
		t.Fatal(err)
	}
	if held, err := s.Store.Lease(ctx, maintenanceLease, "new-leader", time.Minute); err != nil || !held {
		t.Fatal(held, err)
	}
	oldCtx := context.WithValue(ctx, maintenanceOwnerKey{}, "old-leader")
	if err := s.cleanup(oldCtx); !errors.Is(err, store.ErrLeaseLost) {
		t.Fatal("live but superseded worker wrote completion", err)
	}
	current, err := s.Store.Get(ctx, "system-state", "cleanup")
	if err != nil || current["_version"] != state["_version"] || current["finished_at"] != state["finished_at"] {
		t.Fatal("superseded completion changed new leader state", current, err)
	}
}
