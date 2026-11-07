package agentclient

import (
	"context"
	"crypto/tls"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/newnexus/newnexus/internal/agentproto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/structpb"
)

type testAgentServer struct {
	enroll  func(context.Context, *structpb.Struct) (*structpb.Struct, error)
	connect func(agentproto.AgentService_ConnectServer) error
}

func (s *testAgentServer) Enroll(ctx context.Context, m *structpb.Struct) (*structpb.Struct, error) {
	if s.enroll != nil {
		return s.enroll(ctx, m)
	}
	return nil, status.Error(codes.Unimplemented, "test enroll")
}
func (s *testAgentServer) Connect(stream agentproto.AgentService_ConnectServer) error {
	return s.connect(stream)
}

func bufClient(t *testing.T, server agentproto.AgentServiceServer) agentproto.AgentServiceClient {
	t.Helper()
	listener := bufconn.Listen(4 << 20)
	grpcServer := grpc.NewServer()
	agentproto.RegisterAgentServiceServer(grpcServer, server)
	go grpcServer.Serve(listener)
	t.Cleanup(grpcServer.Stop)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := grpc.DialContext(ctx, "bufnet", grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }), grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithBlock())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return agentproto.NewAgentServiceClient(conn)
}

func testConfig(t *testing.T) Config {
	t.Helper()
	cfg := Config{Server: "127.0.0.1:9090", InsecureLoopback: true, DataDir: t.TempDir(), Concurrency: 1, Probe: ProbeOptions{RatePerSecond: 10000, Dialer: dialerFunc(func(context.Context, string, string) (net.Conn, error) { a, b := net.Pipe(); b.Close(); return a, nil })}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := cfg.defaults(); err != nil {
		t.Fatal(err)
	}
	return cfg
}
func sendTestFrame(stream agentproto.AgentService_ConnectServer, m map[string]any) error {
	frame, err := structpb.NewStruct(m)
	if err != nil {
		return err
	}
	return stream.Send(frame)
}
func recvFrameType(stream agentproto.AgentService_ConnectServer, wanted string) (map[string]any, error) {
	for {
		frame, err := stream.Recv()
		if err != nil {
			return nil, err
		}
		m := frame.AsMap()
		if m["type"] == wanted {
			return m, nil
		}
	}
}

func TestEnrollWireAndVersionNegotiation(t *testing.T) {
	client := bufClient(t, &testAgentServer{enroll: func(_ context.Context, in *structpb.Struct) (*structpb.Struct, error) {
		m := in.AsMap()
		if m["enrollment_token"] != "one-time" || number(m["protocol_version"]) != 1 {
			t.Errorf("wrong enrollment wire fields: %+v", m)
		}
		return structpb.NewStruct(map[string]any{"node_id": "node-1", "credential": "node-secret", "protocol_version": 1})
	}})
	cfg := testConfig(t)
	cfg.EnrollmentToken = "one-time"
	credential, err := enroll(context.Background(), cfg, client)
	if err != nil || credential.NodeID != "node-1" || credential.Credential != "node-secret" {
		t.Fatalf("enrollment failed: %+v %v", credential, err)
	}
	client = bufClient(t, &testAgentServer{enroll: func(context.Context, *structpb.Struct) (*structpb.Struct, error) {
		return structpb.NewStruct(map[string]any{"node_id": "node-1", "credential": "s", "protocol_version": 2})
	}})
	if _, err := enroll(context.Background(), cfg, client); !errors.Is(err, ErrProtocol) {
		t.Fatalf("incompatible protocol accepted: %v", err)
	}
}

func TestStreamTaskDurabilityRotationAndRevocation(t *testing.T) {
	cfg := testConfig(t)
	outbox, err := OpenOutbox(cfg.DataDir, cfg.MaxPending, cfg.MaxOutboxBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer outbox.Close()
	credential := Credentials{NodeID: "node-1", Credential: "old-secret", ProtocolVersion: 1}
	if err := outbox.SaveCredentials(credential); err != nil {
		t.Fatal(err)
	}
	serverDone := make(chan error, 1)
	client := bufClient(t, &testAgentServer{connect: func(stream agentproto.AgentService_ConnectServer) (finalErr error) {
		defer func() { serverDone <- finalErr }()
		m, err := recvFrameType(stream, "hello")
		if err != nil {
			return err
		}
		if m["credential"] != "old-secret" {
			t.Error("wrong hello credential")
		}
		if err := sendTestFrame(stream, map[string]any{"type": "config", "protocol_version": 1, "max_batch_size": 2, "heartbeat_seconds": 5}); err != nil {
			return err
		}
		if err := sendTestFrame(stream, map[string]any{"type": "task", "task": map[string]any{"id": "task-1", "address": "8.8.8.8", "mode": "tcp", "samples": 1}}); err != nil {
			return err
		}
		if _, err := recvFrameType(stream, "ack"); err != nil {
			return err
		}
		pending, _, _ := outbox.Stats()
		if pending == 0 && len(outbox.Results(1)) == 0 {
			t.Error("task ACK preceded durable queue")
		}
		if err := sendTestFrame(stream, map[string]any{"type": "credential", "credential": "new-secret", "credential_id": "rotate-1"}); err != nil {
			return err
		}
		if _, err := recvFrameType(stream, "credential_ack"); err != nil {
			return err
		}
		persisted, err := outbox.LoadCredentials()
		if err != nil || persisted.Credential != "new-secret" {
			t.Error("rotation ACK preceded credential persistence")
		}
		results, err := recvFrameType(stream, "results")
		if err != nil {
			return err
		}
		rows, _ := results["results"].([]any)
		if len(rows) != 1 {
			t.Errorf("unexpected results batch: %+v", results)
		} else {
			result := rows[0].(map[string]any)
			if result["id"] != ResultID("node-1", "task-1") || result["success"] != true {
				t.Errorf("invalid probe result: %+v", result)
			}
		}
		if err := sendTestFrame(stream, map[string]any{"type": "ack", "batch_id": results["batch_id"]}); err != nil {
			return err
		}
		if err := sendTestFrame(stream, map[string]any{"type": "task", "task": map[string]any{"id": "task-1", "address": "8.8.8.8", "mode": "tcp"}}); err != nil {
			return err
		}
		if _, err := recvFrameType(stream, "ack"); err != nil {
			return err
		}
		if _, ok := outbox.NextTask(); ok {
			t.Error("duplicate completed task executed again")
		}
		return sendTestFrame(stream, map[string]any{"type": "revoked", "reason": "test revocation"})
	}})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var workers sync.WaitGroup
	workerErrors := make(chan error, 1)
	startWorkers(ctx, cfg, outbox, credential.NodeID, &workers, workerErrors)
	err = runSession(ctx, cfg, client, outbox, &credential, workerErrors)
	cancel()
	workers.Wait()
	if !errors.Is(err, ErrRevoked) {
		t.Fatalf("revocation did not stop stream: %v", err)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cfg.DataDir, "credentials.json")); !os.IsNotExist(err) {
		t.Fatal("revocation retained local credentials")
	}
	if len(outbox.Results(1)) != 0 {
		t.Fatal("acknowledged results retained")
	}
}

func TestStreamReplayAfterDisconnectAndWrongACK(t *testing.T) {
	cfg := testConfig(t)
	outbox, err := OpenOutbox(cfg.DataDir, cfg.MaxPending, cfg.MaxOutboxBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer outbox.Close()
	credential := Credentials{NodeID: "n", Credential: "secret", ProtocolVersion: 1}
	result := Result{ID: ResultID("n", "t"), TaskID: "t", Success: true, ObservedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	if err := outbox.SaveResult(result); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	connections := 0
	client := bufClient(t, &testAgentServer{connect: func(stream agentproto.AgentService_ConnectServer) error {
		if _, err := recvFrameType(stream, "hello"); err != nil {
			return err
		}
		if err := sendTestFrame(stream, map[string]any{"type": "config", "protocol_version": 1, "max_batch_size": 256}); err != nil {
			return err
		}
		m, err := recvFrameType(stream, "results")
		if err != nil {
			return err
		}
		rows := m["results"].([]any)
		if rows[0].(map[string]any)["id"] != result.ID {
			t.Error("replay result identity changed")
		}
		mu.Lock()
		connections++
		attempt := connections
		mu.Unlock()
		if attempt == 1 {
			if err := sendTestFrame(stream, map[string]any{"type": "ack", "batch_id": "wrong"}); err != nil {
				return err
			}
			return status.Error(codes.Unavailable, "simulated outage before ACK")
		}
		if err := sendTestFrame(stream, map[string]any{"type": "ack", "batch_id": m["batch_id"]}); err != nil {
			return err
		}
		return sendTestFrame(stream, map[string]any{"type": "config", "protocol_version": 2})
	}})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = runSession(ctx, cfg, client, outbox, &credential, make(chan error))
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("unexpected first disconnect: %v", err)
	}
	if len(outbox.Results(1)) != 1 {
		t.Fatal("result deleted on wrong ACK or disconnect")
	}
	err = runSession(ctx, cfg, client, outbox, &credential, make(chan error))
	if !errors.Is(err, ErrProtocol) {
		t.Fatalf("incompatible server config accepted: %v", err)
	}
	if len(outbox.Results(1)) != 0 {
		t.Fatal("replayed batch not acknowledged")
	}
}

func TestTLSAndPlaintextConfigGuard(t *testing.T) {
	for _, server := range []string{"example.com:9090", "localhost:9090", "10.0.0.1:9090", "0.0.0.0:9090", "[::]:9090"} {
		if _, err := DialOptions(Config{Server: server, InsecureLoopback: true}); err == nil {
			t.Errorf("plaintext allowed for %s", server)
		}
	}
	for _, server := range []string{"127.0.0.1:9090", "[::1]:9090"} {
		if _, err := DialOptions(Config{Server: server, InsecureLoopback: true}); err != nil {
			t.Errorf("explicit loopback rejected: %v", err)
		}
	}
	for _, server := range []string{"https://example.com:9090", "dns:///example.com:9090", "example.com", "example.com:0", "example.com:65536"} {
		if _, err := DialOptions(Config{Server: server}); err == nil {
			t.Errorf("invalid server accepted: %s", server)
		}
	}
	if _, err := DialOptions(Config{Server: "example.com:9090"}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ca.pem"), []byte("not a certificate"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := DialOptions(Config{Server: "example.com:9090", CAFile: filepath.Join(dir, "ca.pem")}); err == nil {
		t.Fatal("invalid CA file accepted")
	}
}

func TestNegotiatedBatchLimitAndBackpressure(t *testing.T) {
	cfg := testConfig(t)
	outbox, err := OpenOutbox(cfg.DataDir, 5, cfg.MaxOutboxBytes)
	if err != nil {
		t.Fatal(err)
	}
	defer outbox.Close()
	for i := 0; i < 5; i++ {
		id := fmt.Sprint(i)
		if err := outbox.SaveResult(Result{ID: ResultID("n", id), TaskID: id, ObservedAt: time.Now().UTC().Format(time.RFC3339Nano)}); err != nil {
			t.Fatal(err)
		}
	}
	client := bufClient(t, &testAgentServer{connect: func(stream agentproto.AgentService_ConnectServer) error {
		if _, err := recvFrameType(stream, "hello"); err != nil {
			return err
		}
		if err := sendTestFrame(stream, map[string]any{"type": "config", "protocol_version": 1, "max_batch_size": 2}); err != nil {
			return err
		}
		// Config must arrive before the first batch so the server's smaller
		// receive window applies even to a reconnect replay backlog.
		first, err := recvFrameType(stream, "results")
		if err != nil {
			return err
		}
		if got := len(first["results"].([]any)); got != 2 {
			t.Errorf("negotiation ignored for initial batch: %d", got)
		}
		if err := sendTestFrame(stream, map[string]any{"type": "task", "task": map[string]any{"id": "over-capacity", "address": "8.8.8.8", "mode": "tcp"}}); err != nil {
			return err
		}
		backpressure, err := recvFrameType(stream, "backpressure")
		if err != nil {
			return err
		}
		if number(backpressure["outbox_size"]) != 5 {
			t.Errorf("invalid backpressure counts: %+v", backpressure)
		}
		if err := sendTestFrame(stream, map[string]any{"type": "ack", "batch_id": first["batch_id"]}); err != nil {
			return err
		}
		total := 2
		for total < 5 {
			batch, err := recvFrameType(stream, "results")
			if err != nil {
				return err
			}
			size := len(batch["results"].([]any))
			if size < 1 || size > 2 {
				t.Errorf("batch exceeds negotiated window: %d", size)
			}
			total += size
			if err := sendTestFrame(stream, map[string]any{"type": "ack", "batch_id": batch["batch_id"]}); err != nil {
				return err
			}
		}
		return sendTestFrame(stream, map[string]any{"type": "revoked", "reason": "finished"})
	}})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	credential := Credentials{NodeID: "n", Credential: "secret", ProtocolVersion: 1}
	if err := runSession(ctx, cfg, client, outbox, &credential, make(chan error)); !errors.Is(err, ErrRevoked) {
		t.Fatal(err)
	}
	if pending, results, _ := outbox.Stats(); pending != 0 || results != 0 {
		t.Fatalf("backpressure ACKed rejected work or lost result ACK: %d %d", pending, results)
	}
}

func TestHealthFile(t *testing.T) {
	dir := t.TempDir()
	if err := CheckHealth(dir); err == nil {
		t.Fatal("missing health file accepted")
	}
	if err := writeHealth(dir, "node", true, 15*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := CheckHealth(dir); err != nil {
		t.Fatal(err)
	}
	if err := writeHealth(dir, "node", false, 15*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := CheckHealth(dir); err == nil {
		t.Fatal("disconnected health accepted")
	}
}

func TestRuntimeTLSHTTP2EnrollmentAndShutdown(t *testing.T) {
	certificate, _ := testCertificate(t, "control.example")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{certificate}})))
	var verifiedTLS atomic.Bool
	agentproto.RegisterAgentServiceServer(server, &testAgentServer{
		enroll: func(ctx context.Context, in *structpb.Struct) (*structpb.Struct, error) {
			remote, ok := peer.FromContext(ctx)
			if !ok {
				return nil, status.Error(codes.Internal, "missing gRPC transport peer")
			}
			info, ok := remote.AuthInfo.(credentials.TLSInfo)
			if !ok || info.State.NegotiatedProtocol != "h2" {
				t.Error("agent registration did not use TLS HTTP/2")
			}
			verifiedTLS.Store(ok)
			if in.AsMap()["enrollment_token"] != "once" {
				t.Error("enrollment token lost")
			}
			return structpb.NewStruct(map[string]any{"node_id": "tls-node", "credential": "tls-secret", "protocol_version": 1})
		},
		connect: func(stream agentproto.AgentService_ConnectServer) error {
			hello, err := recvFrameType(stream, "hello")
			if err != nil {
				return err
			}
			if hello["credential"] != "tls-secret" {
				t.Error("enrolled credential not used")
			}
			if err := sendTestFrame(stream, map[string]any{"type": "config", "protocol_version": 1, "max_batch_size": 256}); err != nil {
				return err
			}
			if _, err := recvFrameType(stream, "heartbeat"); err != nil {
				return err
			}
			return sendTestFrame(stream, map[string]any{"type": "revoked", "reason": "TLS roundtrip completed"})
		},
	})
	go server.Serve(listener)
	defer server.Stop()
	cfg := testConfig(t)
	cfg.Server = listener.Addr().String()
	cfg.InsecureLoopback = false
	cfg.ServerName = "control.example"
	cfg.EnrollmentToken = "once"
	cfg.CAFile = filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(cfg.CAFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Certificate[0]}), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := Run(ctx, cfg); !errors.Is(err, ErrRevoked) {
		t.Fatalf("TLS runtime failed: %v", err)
	}
	if !verifiedTLS.Load() {
		t.Fatal("TLS enrollment was not performed")
	}
	if _, err := OpenOutbox(cfg.DataDir, 10, 1<<20); !errors.Is(err, ErrRevoked) {
		t.Fatalf("runtime revocation not durable: %v", err)
	}
}
