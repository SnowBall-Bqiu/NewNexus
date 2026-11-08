package agenthub

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/newnexus/newnexus/internal/agentproto"
	"github.com/newnexus/newnexus/internal/domain"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/structpb"
)

type helloReceive struct {
	started  chan struct{}
	finished chan struct{}
}

type trackedHelloStream struct {
	agentproto.AgentService_ConnectServer
	receive *helloReceive
	first   bool
}

func (s *trackedHelloStream) Recv() (*structpb.Struct, error) {
	if !s.first {
		return s.AgentService_ConnectServer.Recv()
	}
	s.first = false
	close(s.receive.started)
	defer close(s.receive.finished)
	return s.AgentService_ConnectServer.Recv()
}

type helloTestService struct {
	*Hub
	receives chan *helloReceive
}

func (s *helloTestService) Connect(stream agentproto.AgentService_ConnectServer) error {
	receive := &helloReceive{started: make(chan struct{}), finished: make(chan struct{})}
	s.receives <- receive
	return s.Hub.Connect(&trackedHelloStream{AgentService_ConnectServer: stream, receive: receive, first: true})
}

func helloTestServer(t *testing.T, hub *Hub) (*helloTestService, func() agentproto.AgentServiceClient, *grpc.Server) {
	t.Helper()
	listener := bufconn.Listen(65536)
	service := &helloTestService{Hub: hub, receives: make(chan *helloReceive, 16)}
	server := grpc.NewServer()
	agentproto.RegisterAgentServiceServer(server, service)
	go server.Serve(listener)
	t.Cleanup(func() { server.Stop(); listener.Close() })
	dial := func() agentproto.AgentServiceClient {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		conn, err := grpc.DialContext(ctx, "passthrough:///hello-test", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }), grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close() })
		return agentproto.NewAgentServiceClient(conn)
	}
	return service, dial, server
}

func awaitHelloSignal(t *testing.T, done <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal(description)
	}
}

func awaitHelloSlots(t *testing.T, hub *Hub, count int) {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for len(hub.helloSlots) != count {
		select {
		case <-deadline.C:
			t.Fatalf("pending hello slots = %d, want %d", len(hub.helloSlots), count)
		case <-ticker.C:
		}
	}
}

func nextHelloReceive(t *testing.T, service *helloTestService) *helloReceive {
	t.Helper()
	select {
	case receive := <-service.receives:
		return receive
	case <-time.After(3 * time.Second):
		t.Fatal("Connect handler did not start")
		return nil
	}
}

func connectWithHello(t *testing.T, ctx context.Context, client agentproto.AgentServiceClient, enrolled domain.Doc) agentproto.AgentService_ConnectClient {
	t.Helper()
	stream, err := client.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	hello, err := structpb.NewStruct(map[string]any{"type": "hello", "node_id": enrolled["node_id"], "credential": enrolled["credential"], "protocol_version": 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(hello); err != nil {
		t.Fatal(err)
	}
	config, err := stream.Recv()
	if err != nil || domain.S(domain.Doc(config.AsMap()), "type") != "config" {
		t.Fatalf("node configuration: %v, %v", config, err)
	}
	return stream
}

func TestConnectHelloDeadlineReleasesSlot(t *testing.T) {
	hub, enrolled := fixture(t)
	hub.helloTimeout = 250 * time.Millisecond
	hub.helloSlots = make(chan struct{}, 1)
	service, dial, _ := helloTestServer(t, hub)
	client := dial()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := client.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	receive := nextHelloReceive(t, service)
	awaitHelloSignal(t, receive.started, "hello receiver did not start")
	if _, err := stream.Recv(); status.Code(err) != codes.DeadlineExceeded || status.Convert(err).Message() != "node hello deadline exceeded" {
		t.Fatalf("silent hello returned %v, want the server hello deadline", err)
	}
	awaitHelloSignal(t, receive.finished, "timed-out gRPC hello receiver did not exit")
	awaitHelloSlots(t, hub, 0)
	// The timed-out stream must neither retain its receive goroutine nor block
	// the next legitimate node from using the only authentication slot.
	connectWithHello(t, ctx, client, enrolled)
}

func TestConnectHelloLimitAcrossConnectionsAndCancellation(t *testing.T) {
	hub, enrolled := fixture(t)
	hub.helloTimeout = time.Minute
	hub.helloSlots = make(chan struct{}, 1)
	service, dial, _ := helloTestServer(t, hub)
	firstClient, secondClient := dial(), dial()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	idleCtx, cancelIdle := context.WithCancel(ctx)
	defer cancelIdle()
	first, err := firstClient.Connect(idleCtx)
	if err != nil {
		t.Fatal(err)
	}
	receive := nextHelloReceive(t, service)
	awaitHelloSignal(t, receive.started, "first hello receiver did not start")
	second, err := secondClient.Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := second.Recv(); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("second connection returned %v, want ResourceExhausted", err)
	}
	rejected := nextHelloReceive(t, service)
	select {
	case <-rejected.started:
		t.Fatal("rejected hello started a receive goroutine")
	default:
	}
	awaitHelloSlots(t, hub, 1)
	cancelIdle()
	if _, err := first.Recv(); status.Code(err) != codes.Canceled {
		t.Fatalf("canceled hello returned %v", err)
	}
	awaitHelloSignal(t, receive.finished, "canceled gRPC hello receiver did not exit")
	awaitHelloSlots(t, hub, 0)
	connectWithHello(t, ctx, secondClient, enrolled)
}

func TestConnectHelloDeadlineDoesNotEndAuthenticatedSession(t *testing.T) {
	hub, enrolled := fixture(t)
	hub.helloTimeout = 250 * time.Millisecond
	hub.helloSlots = make(chan struct{}, 1)
	_, dial, _ := helloTestServer(t, hub)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream := connectWithHello(t, ctx, dial(), enrolled)
	awaitHelloSlots(t, hub, 0)
	// Exercise the authenticated stream after its original hello deadline.
	timer := time.NewTimer(3 * hub.helloTimeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	case <-timer.C:
	}
	results, err := structpb.NewStruct(map[string]any{"type": "results", "batch_id": "after-hello-deadline", "results": []any{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(results); err != nil {
		t.Fatal(err)
	}
	ack, err := stream.Recv()
	if err != nil || ack.AsMap()["batch_id"] != "after-hello-deadline" {
		t.Fatalf("authenticated session ended at hello deadline: %v, %v", ack, err)
	}
}

func TestConnectHelloServerStopReleasesSlot(t *testing.T) {
	hub, _ := fixture(t)
	hub.helloTimeout = time.Minute
	hub.helloSlots = make(chan struct{}, 1)
	service, dial, server := helloTestServer(t, hub)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream, err := dial().Connect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	receive := nextHelloReceive(t, service)
	awaitHelloSignal(t, receive.started, "hello receiver did not start")
	server.Stop()
	if _, err := stream.Recv(); err == nil {
		t.Fatal("stopped server left hello stream open")
	}
	awaitHelloSignal(t, receive.finished, "server stop did not stop gRPC hello receiver")
	awaitHelloSlots(t, hub, 0)
}

type blockedHelloStream struct {
	agentproto.AgentService_ConnectServer
	ctx     context.Context
	unblock chan struct{}
}

func (s *blockedHelloStream) Context() context.Context { return s.ctx }
func (s *blockedHelloStream) Recv() (*structpb.Struct, error) {
	<-s.unblock
	return nil, status.Error(codes.Canceled, "receiver stopped")
}

func TestConnectHelloRetainsSlotUntilReceiveExits(t *testing.T) {
	hub := New(nil)
	hub.helloTimeout = 10 * time.Millisecond
	hub.helloSlots = make(chan struct{}, 1)
	stream := &blockedHelloStream{ctx: context.Background(), unblock: make(chan struct{})}
	t.Cleanup(func() {
		select {
		case <-stream.unblock:
		default:
			close(stream.unblock)
		}
	})
	// A deliberately delayed transport models the gap between a handler's
	// return and gRPC canceling its blocked Recv. Never release capacity early.
	if err := hub.Connect(stream); status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("blocked hello returned %v", err)
	}
	if len(hub.helloSlots) != 1 {
		t.Fatal("hello deadline released a slot while its receiver was still blocked")
	}
	for i := 0; i < 10; i++ {
		if err := hub.Connect(stream); status.Code(err) != codes.ResourceExhausted {
			t.Fatalf("pending receiver admitted another stream: %v", err)
		}
	}
	close(stream.unblock)
	awaitHelloSlots(t, hub, 0)
}
