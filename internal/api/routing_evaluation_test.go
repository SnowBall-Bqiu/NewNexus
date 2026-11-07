package api

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/newnexus/newnexus/internal/agentproto"
	"github.com/newnexus/newnexus/internal/domain"
	"github.com/newnexus/newnexus/internal/store"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/structpb"
)

func TestScheduleEvaluationRetainsCommittedProbeTimestamp(t *testing.T) {
	s := testServer(t)
	ctx := context.Background()
	node := domain.Doc{"id": "evaluation-node", "status": "online", "last_seen_at": domain.Now()}
	if err := s.Store.Put(ctx, "nodes", node); err != nil {
		t.Fatal(err)
	}
	rule := domain.Doc{"id": "evaluation-rule", "hostname": "evaluation.invalid", "status": "healthy", "lines": []any{domain.Doc{"id": "line", "targets": []any{domain.Doc{"id": "target", "cname": "origin.invalid", "enabled": true, "priority": 1}}}}}
	if err := s.Store.Put(ctx, "rules", rule); err != nil {
		t.Fatal(err)
	}
	s.ScheduleTick(ctx)
	got, err := s.Store.Get(ctx, "rules", "evaluation-rule")
	if err != nil || domain.S(got, "last_probe_scheduled_at") == "" || domain.S(got, "status") != "paused" || s.Store.Count(ctx, "tasks") == 0 {
		t.Fatalf("scheduling metadata and health evaluation must both commit: %+v, %v", got, err)
	}
}

func TestMySQLBlockedBackgroundMonitoringAndEvaluationDoNotDelayProbeDelivery(t *testing.T) {
	s := testServer(t)
	if s.Store.Driver != "mysql" {
		t.Skip("requires actual MySQL row locks")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
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
	token, err := s.Hub.Enrollment(ctx, time.Minute, "evaluation-check")
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
	if err = s.Store.Put(ctx, "candidates", domain.Doc{"id": "unblocked-candidate", "address": "1.1.1.1", "tier": "active"}); err != nil {
		t.Fatal(err)
	}
	// Its probes are not due. Only the health-decision write will wait on
	// the held row, so the test distinguishes evaluation from scheduling.
	at := s.Store.Time(time.Now())
	rule := domain.Doc{"id": "blocked-evaluation", "hostname": "evaluation.invalid", "status": "healthy", "last_probe_scheduled_at": at, "lines": []any{domain.Doc{"id": "line", "targets": []any{domain.Doc{"id": "target", "cname": "origin.invalid", "enabled": true, "priority": 1}}}}}
	if err = s.Store.Put(ctx, "rules", rule); err != nil {
		t.Fatal(err)
	}
	if err = s.Store.Put(ctx, "system-state", domain.Doc{"id": "storage"}); err != nil {
		t.Fatal(err)
	}
	if err = s.Store.Put(ctx, "alerts", domain.Doc{"id": "monitor-alert", "status": "open", "created_at": domain.Now()}); err != nil {
		t.Fatal(err)
	}
	gate, err := s.Store.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Rollback()
	var id string
	if err = gate.QueryRowContext(ctx, "SELECT id FROM resources WHERE kind='rules' AND id=? FOR UPDATE", "blocked-evaluation").Scan(&id); err != nil {
		t.Fatal(err)
	}
	if err = gate.QueryRowContext(ctx, "SELECT id FROM resources WHERE kind='system-state' AND id='storage' FOR UPDATE").Scan(&id); err != nil {
		t.Fatal(err)
	}
	work, stop := context.WithCancel(ctx)
	routingDone, schedulerDone, monitorDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() { defer close(routingDone); s.routingEvaluator(work) }()
	go func() { defer close(monitorDone); s.externalMonitor(work) }()
	go func() { defer close(schedulerDone); s.scheduler(work) }()
	defer func() {
		stop()
		for _, done := range []chan struct{}{routingDone, schedulerDone, monitorDone} {
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("canceled routing/scheduler worker did not stop")
			}
		}
	}()
	for {
		frame, err := stream.Recv()
		if err != nil {
			t.Fatal("probe delivery waited for the blocked rule evaluation:", err)
		}
		message := domain.Doc(frame.AsMap())
		if domain.S(message, "type") != "task" {
			continue
		}
		task := domain.M(message, "task")
		stored, err := s.Store.Get(ctx, "tasks", domain.S(task, "id"))
		if err != nil || domain.S(stored, "candidate_id") != "unblocked-candidate" {
			t.Fatal("delivered probe was not durable", stored, err)
		}
		break
	}
	// A newly due candidate must also be delivered by a later scheduling pass
	// while the same rule evaluation remains blocked, not just by the first pass.
	if err = s.Store.Put(ctx, "candidates", domain.Doc{"id": "later-candidate", "address": "1.0.0.1", "tier": "active"}); err != nil {
		t.Fatal(err)
	}
	for {
		frame, err := stream.Recv()
		if err != nil {
			t.Fatal("a later scheduling pass waited for rule evaluation:", err)
		}
		message := domain.Doc(frame.AsMap())
		if domain.S(message, "type") == "task" && domain.S(domain.M(message, "task"), "candidate_id") == "later-candidate" {
			break
		}
	}
	if err = gate.Rollback(); err != nil {
		t.Fatal(err)
	}
	for {
		got, err := s.Store.Get(ctx, "rules", "blocked-evaluation")
		if err != nil {
			t.Fatal(err)
		}
		if domain.S(got, "status") == "paused" {
			if domain.S(got, "last_probe_scheduled_at") != at {
				t.Fatal("health evaluation overwrote scheduling metadata", got)
			}
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("rule evaluation did not resume after unlock:", ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	for {
		alert, err := s.Store.Get(ctx, "alerts", "monitor-alert")
		if err != nil {
			t.Fatal(err)
		}
		if jobID := domain.S(alert, "notification_job_id"); jobID != "" {
			job, err := s.Store.Get(ctx, "jobs", jobID)
			if err != nil || domain.S(job, "type") != "notify" {
				t.Fatal("background monitor did not queue durable notification", job, err)
			}
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("monitor did not resume after storage lock release:", ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

type rejectedRoutingOperations struct{ calls atomic.Int64 }

func (o *rejectedRoutingOperations) Run(context.Context, string, string, domain.Doc) (domain.Doc, error) {
	o.calls.Add(1)
	return nil, errors.New("superseded routing worker must not invoke provider operations")
}

func TestRoutingSupersededLiveContextCannotCommitAlertOrAutoPublish(t *testing.T) {
	s := testServer(t)
	ctx := t.Context()
	ops := &rejectedRoutingOperations{}
	s.Operations = ops
	nodes := []domain.Doc{}
	for i := 0; i < 3; i++ {
		node := domain.Doc{"id": domain.ID(), "status": "online", "last_seen_at": domain.Now()}
		if err := s.Store.Put(ctx, "nodes", node); err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, node)
		rawMetric(t, s, domain.S(node, "id"), "target:eligible", 3, 1, 10, 1000)
	}
	if held, err := s.Store.Lease(ctx, routingEvaluationLease, "replacement", time.Minute); err != nil || !held {
		t.Fatal(held, err)
	}
	rules := []domain.Doc{
		{"id": domain.ID(), "status": "healthy", "publish_mode": "auto", "lines": []any{domain.Doc{"id": "no-quorum", "targets": []any{domain.Doc{"id": "missing", "enabled": true, "priority": 0, "weight": 1}}}}},
		{"id": domain.ID(), "status": "healthy", "publish_mode": "auto", "lines": []any{domain.Doc{"id": "switch", "active_priority": 9, "targets": []any{domain.Doc{"id": "eligible", "enabled": true, "priority": 0, "weight": 1}}}}},
	}
	for _, rule := range rules {
		if err := s.Store.Put(ctx, "rules", rule); err != nil {
			t.Fatal(err)
		}
	}
	oldCtx := context.WithValue(s.cachedSettings(ctx), routingOwnerKey{}, "old-worker")
	oldCtx, err := s.cacheRoutingVotes(oldCtx)
	if err != nil {
		t.Fatal(err)
	}
	for _, rule := range rules {
		originalVersion := rule["_version"]
		if _, err := s.evaluateRule(oldCtx, domain.Clone(rule), nodes, nil); !errors.Is(err, store.ErrLeaseLost) {
			t.Fatal("superseded decision was not rejected by lease fence", err)
		}
		stored, err := s.Store.Get(ctx, "rules", domain.S(rule, "id"))
		if err != nil || stored["_version"] != originalVersion || domain.S(stored, "status") != "healthy" {
			t.Fatal("superseded decision changed rule", stored, err)
		}
	}
	if s.Store.Count(ctx, "alerts") != 0 || s.Store.Count(ctx, "jobs") != 0 || s.Store.Count(ctx, "releases") != 0 || ops.calls.Load() != 0 {
		t.Fatal("rejected decision produced alert or automatic publication side effects")
	}
	// Manual evaluations retain their original behavior without a production
	// worker owner, and still emit a real quorum alert after committing state.
	manual, err := s.EvaluateRule(ctx, domain.Clone(rules[0]))
	if err != nil || domain.S(manual, "status") != "paused" || s.Store.Count(ctx, "alerts") != 1 || s.Store.Count(ctx, "jobs") != 1 {
		t.Fatal("manual evaluation behavior changed", manual, err)
	}
}
