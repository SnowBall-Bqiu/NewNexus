package agentclient

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/newnexus/newnexus/internal/agentproto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
)

var ErrProtocol = errors.New("unsupported server protocol version")
var ErrDurability = errors.New("agent cannot persist durable state")

// Run maintains enrollment, durable workers and the reconnecting bidirectional
// stream until cancellation. The caller must provide a cancellation signal.
func Run(ctx context.Context, cfg Config) error {
	if err := cfg.defaults(); err != nil {
		return err
	}
	options, err := DialOptions(cfg)
	if err != nil {
		return err
	}
	outbox, err := OpenOutbox(cfg.DataDir, cfg.MaxPending, cfg.MaxOutboxBytes)
	if err != nil {
		return err
	}
	defer outbox.Close()
	credential, err := outbox.LoadCredentials()
	if err != nil {
		return err
	}
	if credential.Credential == "" && cfg.EnrollmentToken == "" {
		return fmt.Errorf("enrollment token is required for an unregistered node")
	}
	workerCtx, cancelWorkers := context.WithCancel(ctx)
	workerErrors := make(chan error, 1)
	var workers sync.WaitGroup
	defer func() { cancelWorkers(); workers.Wait() }()
	workerStarted := false
	backoff := time.Second
	for ctx.Err() == nil {
		dialCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		conn, dialErr := grpc.DialContext(dialCtx, cfg.Server, options...)
		cancel()
		if dialErr != nil {
			cfg.Logger.Warn("control plane connection unavailable", "error", safeProbeError(dialErr), "retry_after", backoff)
			if err := retryWait(ctx, workerErrors, backoff); err != nil {
				cancelWorkers()
				return shutdownError(ctx, err)
			}
			backoff = nextBackoff(backoff)
			continue
		}
		client := agentproto.NewAgentServiceClient(conn)
		if credential.Credential == "" {
			credential, err = enroll(ctx, cfg, client)
			if err == nil {
				if err := outbox.SaveCredentials(credential); err != nil {
					conn.Close()
					return fmt.Errorf("%w: %v", ErrDurability, err)
				}
			}
			if err != nil {
				conn.Close()
				if status.Code(err) == codes.Unauthenticated || status.Code(err) == codes.PermissionDenied || status.Code(err) == codes.InvalidArgument || status.Code(err) == codes.FailedPrecondition || errors.Is(err, ErrProtocol) {
					return fmt.Errorf("node enrollment rejected: %w", err)
				}
				cfg.Logger.Warn("node enrollment unavailable", "code", status.Code(err).String())
				if err := retryWait(ctx, workerErrors, backoff); err != nil {
					cancelWorkers()
					return shutdownError(ctx, err)
				}
				backoff = nextBackoff(backoff)
				continue
			}
			// Erase the process copy after its single successful use. Secrets are
			// never emitted to logs, status files, or probe results.
			cfg.EnrollmentToken = ""
			cfg.Logger.Info("probe node enrolled", "node_id", credential.NodeID)
		}
		if !workerStarted {
			startWorkers(workerCtx, cfg, outbox, credential.NodeID, &workers, workerErrors)
			workerStarted = true
		}
		connectedAt := time.Now()
		err = runSession(ctx, cfg, client, outbox, &credential, workerErrors)
		conn.Close()
		_ = writeHealth(cfg.DataDir, credential.NodeID, false, cfg.HeartbeatInterval)
		if errors.Is(err, ErrRevoked) || errors.Is(err, ErrProtocol) || errors.Is(err, ErrDurability) {
			cancelWorkers()
			return err
		}
		if status.Code(err) == codes.FailedPrecondition {
			cancelWorkers()
			return ErrProtocol
		}
		if status.Code(err) == codes.Unauthenticated || status.Code(err) == codes.PermissionDenied {
			if revokeErr := outbox.Revoke("control plane rejected node credentials"); revokeErr != nil {
				cancelWorkers()
				return revokeErr
			}
			cancelWorkers()
			return ErrRevoked
		}
		if ctx.Err() != nil {
			cancelWorkers()
			return nil
		}
		select {
		case workerErr := <-workerErrors:
			cancelWorkers()
			return workerErr
		default:
		}
		cfg.Logger.Warn("control plane stream disconnected", "code", status.Code(err).String(), "retry_after", backoff)
		if time.Since(connectedAt) > time.Minute {
			backoff = time.Second
		}
		if err := retryWait(ctx, workerErrors, backoff); err != nil {
			cancelWorkers()
			return shutdownError(ctx, err)
		}
		backoff = nextBackoff(backoff)
	}
	cancelWorkers()
	return nil
}

// DialOptions enforces TLS 1.2+ and the operating system trust store. Plaintext is
// exclusively an explicit, literal-loopback development mode; DNS names cannot
// bypass this check with a resolution race.
func DialOptions(cfg Config) ([]grpc.DialOption, error) {
	if strings.Contains(cfg.Server, "://") || strings.ContainsAny(cfg.Server, "/\\ \r\n") {
		return nil, fmt.Errorf("server must be a host:port without scheme or path")
	}
	host, port, err := net.SplitHostPort(cfg.Server)
	if err != nil {
		return nil, fmt.Errorf("server must be host:port")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return nil, fmt.Errorf("invalid server port")
	}
	var transport credentials.TransportCredentials
	if cfg.InsecureLoopback {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return nil, fmt.Errorf("plaintext gRPC is permitted only for literal loopback addresses")
		}
		transport = insecure.NewCredentials()
	} else {
		tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: cfg.ServerName}
		if cfg.CAFile != "" {
			pem, err := os.ReadFile(cfg.CAFile)
			if err != nil {
				return nil, fmt.Errorf("read control plane CA: %w", err)
			}
			roots, err := x509.SystemCertPool()
			if err != nil {
				roots = x509.NewCertPool()
			}
			if !roots.AppendCertsFromPEM(pem) {
				return nil, fmt.Errorf("CA file contains no certificates")
			}
			tlsConfig.RootCAs = roots
		}
		if cfg.ClientCertFile != "" || cfg.ClientKeyFile != "" {
			certificate, err := tls.LoadX509KeyPair(cfg.ClientCertFile, cfg.ClientKeyFile)
			if err != nil {
				return nil, fmt.Errorf("load client certificate: %w", err)
			}
			tlsConfig.Certificates = []tls.Certificate{certificate}
		}
		transport = credentials.NewTLS(tlsConfig)
	}
	return []grpc.DialOption{grpc.WithTransportCredentials(transport), grpc.WithBlock(),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{Time: 30 * time.Second, Timeout: 10 * time.Second, PermitWithoutStream: false}),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(4<<20), grpc.MaxCallSendMsgSize(4<<20))}, nil
}

func enroll(ctx context.Context, cfg Config, client agentproto.AgentServiceClient) (Credentials, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	request, _ := structpb.NewStruct(map[string]any{"enrollment_token": cfg.EnrollmentToken, "name": cfg.Name, "region": cfg.Region, "carrier": cfg.Carrier, "mode": cfg.Mode, "version": Version, "protocol_version": ProtocolVersion})
	response, err := client.Enroll(ctx, request)
	if err != nil {
		return Credentials{}, err
	}
	m := response.AsMap()
	version := int(number(m["protocol_version"]))
	if number(m["protocol_version"]) != float64(ProtocolVersion) {
		return Credentials{}, ErrProtocol
	}
	nodeID, _ := m["node_id"].(string)
	secret, _ := m["credential"].(string)
	if nodeID == "" || secret == "" {
		return Credentials{}, fmt.Errorf("enrollment response missing credentials")
	}
	return Credentials{NodeID: nodeID, Credential: secret, ProtocolVersion: version}, nil
}

func startWorkers(ctx context.Context, cfg Config, outbox *Outbox, nodeID string, wg *sync.WaitGroup, errors chan<- error) {
	prober := NewProber(cfg.Probe)
	for n := 0; n < cfg.Concurrency; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			timer := time.NewTicker(100 * time.Millisecond)
			defer timer.Stop()
			for ctx.Err() == nil {
				task, ok := outbox.NextTask()
				if !ok {
					select {
					case <-ctx.Done():
						return
					case <-timer.C:
					}
					continue
				}
				if cfg.Mode == "tcp" && (task.Mode == "all" || task.Mode == "" || task.Mode == "tcp") {
					task.Mode = "tcp"
				}
				if (cfg.Mode == "tcp" && task.Mode != "tcp") || (cfg.Mode == "http" && (task.Mode == "tcp" || task.Mode == "tls")) {
					task.Mode = "unsupported_by_node_mode"
				}
				result := prober.Probe(ctx, task)
				if ctx.Err() != nil {
					outbox.ReleaseTask(task.ID)
					return
				}
				result.ID = ResultID(nodeID, task.ID)
				if err := outbox.SaveResult(result); err != nil {
					select {
					case errors <- fmt.Errorf("persist probe result: %w", err):
					default:
					}
					return
				}
			}
		}()
	}
}

type receivedFrame struct {
	frame *structpb.Struct
	err   error
}
type pendingBatch struct {
	id     string
	ids    []string
	frame  *structpb.Struct
	sentAt time.Time
}

func runSession(ctx context.Context, cfg Config, client agentproto.AgentServiceClient, outbox *Outbox, credential *Credentials, workerErrors <-chan error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := client.Connect(ctx)
	if err != nil {
		return err
	}
	defer stream.CloseSend()
	send := func(values map[string]any) error {
		frame, err := structpb.NewStruct(values)
		if err != nil {
			return err
		}
		return stream.Send(frame)
	}
	if err := send(map[string]any{"type": "hello", "node_id": credential.NodeID, "credential": credential.Credential, "protocol_version": ProtocolVersion, "version": Version}); err != nil {
		return err
	}
	incoming := make(chan receivedFrame, 8)
	go func() {
		for {
			frame, err := stream.Recv()
			select {
			case incoming <- receivedFrame{frame, err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	heartbeat := cfg.HeartbeatInterval
	heartbeatTimer := time.NewTicker(heartbeat)
	defer heartbeatTimer.Stop()
	resultTimer := time.NewTicker(time.Second)
	defer resultTimer.Stop()
	handshakeTimer := time.NewTimer(15 * time.Second)
	defer handshakeTimer.Stop()
	handshakeDeadline := handshakeTimer.C
	configured := false
	maxBatch := 256
	var inflight *pendingBatch
	flush := func() error {
		if inflight != nil {
			if time.Since(inflight.sentAt) < 15*time.Second {
				return nil
			}
			if err := stream.Send(inflight.frame); err != nil {
				return err
			}
			inflight.sentAt = time.Now()
			return nil
		}
		results := outbox.Results(maxBatch)
		if len(results) == 0 {
			return nil
		}
		data, err := json.Marshal(results)
		if err != nil {
			return err
		}
		var wire []any
		if err := json.Unmarshal(data, &wire); err != nil {
			return err
		}
		id, err := newID()
		if err != nil {
			return err
		}
		frame, err := structpb.NewStruct(map[string]any{"type": "results", "batch_id": id, "results": wire})
		if err != nil {
			return err
		}
		ids := make([]string, len(results))
		for i, result := range results {
			ids[i] = result.ID
		}
		if err := stream.Send(frame); err != nil {
			return err
		}
		inflight = &pendingBatch{id: id, ids: ids, frame: frame, sentAt: time.Now()}
		return nil
	}
	sendHeartbeat := func() error {
		pending, results, size := outbox.Stats()
		capabilities := []any{"tcp", "tls", "http", "download"}
		if cfg.Mode == "tcp" {
			capabilities = []any{"tcp"}
		}
		if cfg.Mode == "http" {
			capabilities = []any{"http", "download"}
		}
		if err := send(map[string]any{"type": "heartbeat", "node_id": credential.NodeID, "version": Version, "protocol_version": ProtocolVersion,
			"capabilities": capabilities, "mode": cfg.Mode, "architecture": runtime.GOARCH, "os": runtime.GOOS,
			"pending_tasks": pending, "outbox_size": results, "outbox_bytes": size, "concurrency": cfg.Concurrency}); err != nil {
			return err
		}
		if err := writeHealth(cfg.DataDir, credential.NodeID, true, heartbeat); err != nil {
			return fmt.Errorf("%w: %v", ErrDurability, err)
		}
		return nil
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-handshakeDeadline:
			return status.Error(codes.DeadlineExceeded, "server did not complete agent protocol handshake")
		case err := <-workerErrors:
			return fmt.Errorf("%w: %v", ErrDurability, err)
		case <-heartbeatTimer.C:
			if configured {
				if err := sendHeartbeat(); err != nil {
					return err
				}
			}
		case <-resultTimer.C:
			if configured {
				if err := flush(); err != nil {
					return err
				}
			}
		case received := <-incoming:
			if received.err != nil {
				return received.err
			}
			m := received.frame.AsMap()
			switch m["type"] {
			case "config":
				if number(m["protocol_version"]) != float64(ProtocolVersion) {
					return ErrProtocol
				}
				if batch := int(number(m["max_batch_size"])); batch > 0 && batch <= 256 {
					maxBatch = batch
				}
				if seconds := number(m["heartbeat_seconds"]); seconds >= 5 && seconds <= 300 {
					heartbeat = time.Duration(seconds) * time.Second
					heartbeatTimer.Reset(heartbeat)
				}
				if !configured {
					configured = true
					handshakeTimer.Stop()
					handshakeDeadline = nil
					if err := sendHeartbeat(); err != nil {
						return err
					}
					if err := flush(); err != nil {
						return err
					}
				}
			case "task":
				if !configured {
					return ErrProtocol
				}
				data, err := json.Marshal(m["task"])
				if err != nil {
					return err
				}
				var task Task
				if err := json.Unmarshal(data, &task); err != nil {
					return fmt.Errorf("invalid server task: %w", err)
				}
				if err := outbox.AddTask(task); err != nil {
					if errors.Is(err, ErrBackpressure) {
						pending, results, _ := outbox.Stats()
						if err := send(map[string]any{"type": "backpressure", "retry_after_seconds": 5, "pending_tasks": pending, "outbox_size": results}); err != nil {
							return err
						}
						continue
					}
					return fmt.Errorf("%w: %v", ErrDurability, err)
				}
				if err := send(map[string]any{"type": "ack", "task_id": task.ID}); err != nil {
					return err
				}
			case "ack":
				if inflight != nil && m["batch_id"] == inflight.id {
					if err := outbox.AckResults(inflight.ids); err != nil {
						return fmt.Errorf("%w: %v", ErrDurability, err)
					}
					inflight = nil
					if err := flush(); err != nil {
						return err
					}
				}
			case "credential":
				secret, _ := m["credential"].(string)
				if secret == "" {
					return fmt.Errorf("empty rotated credential")
				}
				next := *credential
				next.Credential = secret
				if err := outbox.SaveCredentials(next); err != nil {
					return fmt.Errorf("%w: %v", ErrDurability, err)
				}
				*credential = next
				ack := map[string]any{"type": "credential_ack"}
				if id, ok := m["credential_id"].(string); ok {
					ack["credential_id"] = id
				}
				if err := send(ack); err != nil {
					return err
				}
			case "revoked":
				reason, _ := m["reason"].(string)
				if err := outbox.Revoke(reason); err != nil {
					return err
				}
				return ErrRevoked
			case "ping":
				if err := send(map[string]any{"type": "pong"}); err != nil {
					return err
				}
			case "error":
				code, _ := m["code"].(string)
				if code == "protocol_version" {
					return ErrProtocol
				}
				if code == "revoked" {
					if err := outbox.Revoke("control plane revoked node"); err != nil {
						return err
					}
					return ErrRevoked
				}
				cfg.Logger.Warn("control plane reported an error", "code", code)
			}
		}
	}
}

func number(value any) float64 { n, _ := value.(float64); return n }
func newID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(id[:]), nil
}
func nextBackoff(previous time.Duration) time.Duration {
	if previous >= 30*time.Second {
		return 30 * time.Second
	}
	return min(previous*2, 30*time.Second)
}
func retryWait(ctx context.Context, workerErrors <-chan error, d time.Duration) error {
	// Jitter prevents a fleet of nodes reconnecting in lockstep after an outage.
	var b [1]byte
	if _, err := rand.Read(b[:]); err == nil {
		d += time.Duration(float64(d) * float64(b[0]) / 1024)
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-workerErrors:
		return err
	case <-timer.C:
		return nil
	}
}
func shutdownError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return nil
	}
	return err
}

type Health struct {
	NodeID    string    `json:"node_id"`
	Connected bool      `json:"connected"`
	UpdatedAt time.Time `json:"updated_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

func writeHealth(dir, node string, connected bool, heartbeat time.Duration) error {
	now := time.Now().UTC()
	data, err := json.Marshal(Health{NodeID: node, Connected: connected, UpdatedAt: now, ExpiresAt: now.Add(max(3*heartbeat, time.Minute))})
	if err != nil {
		return err
	}
	return atomicFile(filepath.Join(dir, "health.json"), data)
}
func CheckHealth(dir string) error {
	f, err := os.Open(filepath.Join(dir, "health.json"))
	if err != nil {
		return err
	}
	defer f.Close()
	var health Health
	if err := ReadJSONLimited(f, &health, 4096); err != nil {
		return err
	}
	if !health.Connected || time.Now().After(health.ExpiresAt) {
		return fmt.Errorf("agent stream is disconnected or heartbeat expired")
	}
	return nil
}
