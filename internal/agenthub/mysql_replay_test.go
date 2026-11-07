package agenthub

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/newnexus/newnexus/internal/agentproto"
	"github.com/newnexus/newnexus/internal/auth"
	"github.com/newnexus/newnexus/internal/domain"
	"github.com/newnexus/newnexus/internal/testutil"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/structpb"
)

func TestMySQLRemoteGatewayTaskReplayWithinTwoSeconds(t *testing.T) {
	db := testutil.MySQL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var database string
	if err := db.DB.QueryRowContext(ctx, `SELECT DATABASE()`).Scan(&database); err != nil {
		t.Fatal(err)
	}
	connected, remote := New(db), New(openOtherMySQL(t, database))
	nodeID, credential := domain.ID(), auth.Token()
	if err := db.Put(ctx, "nodes", domain.Doc{"id": nodeID, "status": "online", "credential_hash": auth.Hash(credential)}); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	agentproto.RegisterAgentServiceServer(server, connected)
	go server.Serve(listener)
	t.Cleanup(func() { server.Stop(); listener.Close() })
	connection, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { connection.Close() })
	streamCtx, disconnect := context.WithCancel(ctx)
	defer disconnect()
	stream, err := agentproto.NewAgentServiceClient(connection).Connect(streamCtx)
	if err != nil {
		t.Fatal(err)
	}
	hello, _ := structpb.NewStruct(map[string]any{"type": "hello", "protocol_version": 1, "node_id": nodeID, "credential": credential})
	if err := stream.Send(hello); err != nil {
		t.Fatal(err)
	}
	configuration, err := stream.Recv()
	if err != nil || configuration.AsMap()["type"] != "config" {
		t.Fatalf("initial configuration: %v, %v", configuration, err)
	}
	leaseID := "node-connect:" + nodeID
	var initialExpiry string
	if err := db.DB.QueryRowContext(ctx, `SELECT expires_at FROM job_leases WHERE id=?`, leaseID).Scan(&initialExpiry); err != nil {
		t.Fatal(err)
	}
	// Observe the real five-second maintenance tick rather than relying on a
	// sleep. Persisting immediately after it made the previous implementation
	// wait nearly five seconds for its next coupled task replay.
	probe := time.NewTicker(20 * time.Millisecond)
	defer probe.Stop()
	for {
		var expiry string
		if err := db.DB.QueryRowContext(ctx, `SELECT expires_at FROM job_leases WHERE id=?`, leaseID).Scan(&expiry); err != nil {
			t.Fatal(err)
		}
		if expiry != initialExpiry {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("five-second gateway maintenance did not run:", ctx.Err())
		case <-probe.C:
		}
	}
	// Let the maintenance case finish its node read, so an old implementation
	// cannot pick up this task in the tail of the just-observed fifth-second tick.
	select {
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	case <-time.After(200 * time.Millisecond):
	}
	taskID := domain.ID()
	if err := remote.PersistTasks(ctx, []domain.Doc{{"id": taskID, "node_id": nodeID, "candidate_id": "remote-replay-candidate", "address": "1.1.1.1", "mode": "tcp"}}); err != nil {
		t.Fatal(err)
	}
	if len(remote.streams) != 0 {
		t.Fatal("remote gateway must not have a local stream or use PushTasks")
	}
	committedAt := time.Now()
	type received struct {
		message *structpb.Struct
		err     error
	}
	frames := make(chan received, 1)
	go func() { frame, err := stream.Recv(); frames <- received{frame, err} }()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	select {
	case result := <-frames:
		if result.err != nil {
			t.Fatal("remote durable task delivery:", result.err)
		}
		message := domain.Doc(result.message.AsMap())
		task, ok := message["task"].(map[string]any)
		if domain.S(message, "type") != "task" || !ok || task["id"] != taskID {
			t.Fatalf("unexpected replay frame: %+v", message)
		}
		if elapsed := time.Since(committedAt); elapsed >= 2*time.Second {
			t.Fatalf("remote task delivered after %v", elapsed)
		}
	case <-deadline.C:
		t.Fatal("persisted task on the other gateway was not received within two seconds")
	}
	// Cancellation must unblock both receive and the connected Hub's cleanup.
	go func() { frame, err := stream.Recv(); frames <- received{frame, err} }()
	disconnect()
	select {
	case result := <-frames:
		if result.err == nil {
			t.Fatal("canceled stream kept receiving tasks")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stream receive did not unblock on cancellation")
	}
	cleanupDeadline := time.Now().Add(2 * time.Second)
	for {
		connected.mu.Lock()
		_, present := connected.streams[nodeID]
		connected.mu.Unlock()
		if !present {
			break
		}
		if time.Now().After(cleanupDeadline) {
			t.Fatal("canceled gateway stream was not removed")
		}
		<-probe.C
	}
}
