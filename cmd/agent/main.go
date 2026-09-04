package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/SnowBall-Bqiu/NewNexus/internal/agenthub"
	"github.com/SnowBall-Bqiu/NewNexus/internal/agentoutbox"
	"github.com/SnowBall-Bqiu/NewNexus/internal/agentproto"
	"github.com/SnowBall-Bqiu/NewNexus/internal/probe"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

var version = "dev"

type state struct {
	NodeID            string `json:"nodeId"`
	Token             string `json:"token"`
	ExpiresAt         string `json:"expiresAt,omitempty"`
	PendingToken      string `json:"pendingToken,omitempty"`
	PendingExpiresAt  string `json:"pendingExpiresAt,omitempty"`
	PendingRotationID string `json:"pendingRotationId,omitempty"`
}

var errAgentReenrolled = errors.New("agent reenrolled")

func main() {
	server := flag.String("server", env("NEWNEXUS_AGENT_SERVER", "localhost:9090"), "gRPC control-plane address")
	enrollCode := flag.String("enroll-code", os.Getenv("NEWNEXUS_AGENT_ENROLL_CODE"), "one-time enrollment code")
	recoveryCode := flag.String("recovery-code", os.Getenv("NEWNEXUS_AGENT_RECOVERY_CODE"), "one-time node recovery code for expired credentials")
	name := flag.String("name", env("NEWNEXUS_AGENT_NAME", hostname()), "node name")
	protocols := flag.String("protocols", env("NEWNEXUS_AGENT_PROTOCOLS", "tcp,http"), "comma-separated probe protocols")
	statePath := flag.String("state", env("NEWNEXUS_AGENT_STATE", "agent-state.json"), "agent state path")
	insecureTransport := flag.Bool("insecure", env("NEWNEXUS_AGENT_INSECURE", "false") == "true", "disable transport TLS")
	serverName := flag.String("tls-server-name", os.Getenv("NEWNEXUS_AGENT_TLS_SERVER_NAME"), "TLS server name override")
	tlsFingerprint := flag.String("tls-sha256", os.Getenv("NEWNEXUS_AGENT_TLS_SHA256"), "expected control-plane leaf certificate SHA-256 fingerprint")
	concurrency := flag.Int("concurrency", 16, "maximum concurrent probes")
	outboxMaxMB := flag.Int("outbox-max-mb", 100, "maximum persistent result outbox size in MiB")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	backoff := time.Second
	for ctx.Err() == nil {
		if err := run(ctx, *server, *enrollCode, *recoveryCode, *name, *protocols, *statePath, *serverName, *tlsFingerprint, *insecureTransport, *concurrency, *outboxMaxMB); err != nil && ctx.Err() == nil {
			if errors.Is(err, errAgentReenrolled) {
				backoff = time.Second
				continue
			}
			log.Printf("agent disconnected: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func run(ctx context.Context, server, enrollCode, recoveryCode, name, protocols, statePath, serverName, tlsFingerprint string, allowInsecure bool, concurrency, outboxMaxMB int) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var transport credentials.TransportCredentials
	if allowInsecure {
		transport = insecure.NewCredentials()
	} else {
		config, err := agentTLSConfig(serverName, tlsFingerprint)
		if err != nil {
			return err
		}
		transport = credentials.NewTLS(config)
	}
	connection, err := grpc.NewClient(server, grpc.WithTransportCredentials(transport), grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(4<<20), grpc.MaxCallSendMsgSize(4<<20)))
	if err != nil {
		return err
	}
	defer connection.Close()
	client := agentproto.NewAgentServiceV1Client(connection)
	var current state
	if err := probe.LoadState(statePath, &current); err != nil || current.Token == "" {
		if enrollCode == "" {
			return fmt.Errorf("agent is not enrolled; provide --enroll-code")
		}
		response, err := client.Enroll(ctx, enrollmentRequest(enrollCode, "", name, protocols))
		if err != nil {
			return fmt.Errorf("enroll: %w", err)
		}
		current = stateFromEnrollment(response)
		if err := probe.SaveState(statePath, current); err != nil {
			return fmt.Errorf("save enrollment: %w", err)
		}
		log.Printf("enrolled node %s", current.NodeID)
	}
	streamCtx := metadata.AppendToOutgoingContext(runCtx, "authorization", "Bearer "+current.Token, "x-agent-version", version)
	stream, err := client.Connect(streamCtx)
	if err != nil {
		return recoverExpiredEnrollment(ctx, err, client, recoveryCode, name, protocols, statePath, &current)
	}
	defer stream.CloseSend()
	outbox, err := agentoutbox.Open(statePath+".outbox.db", int64(max(outboxMaxMB, 1))<<20)
	if err != nil {
		return fmt.Errorf("open result outbox: %w", err)
	}
	defer outbox.Close()
	var sendMu sync.Mutex
	sendErr := make(chan error, 4)
	send := func(message *agentproto.ClientEnvelope) error {
		sendMu.Lock()
		defer sendMu.Unlock()
		return stream.Send(message)
	}
	if err := send(&agentproto.ClientEnvelope{Type: "hello", ProtocolVersion: "agent.v1", Features: agentFeatures()}); err != nil {
		return err
	}
	if current.PendingRotationID != "" {
		if err := send(&agentproto.ClientEnvelope{Type: "credential_rotation_ack", CredentialRotationAck: &agentproto.CredentialRotationAck{RotationId: current.PendingRotationID}}); err != nil {
			return err
		}
	}
	heartbeatDone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-heartbeatDone:
				return
			case <-runCtx.Done():
				return
			case <-ticker.C:
				count, bytes, _ := outbox.Stats()
				if err := send(&agentproto.ClientEnvelope{Type: "heartbeat", Heartbeat: &agentproto.Heartbeat{ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), QueueDepth: int32(count), OutboxBytes: bytes}}); err != nil {
					sendErr <- err
					return
				}
			}
		}
	}()
	defer close(heartbeatDone)
	flushDone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		defer close(flushDone)
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				entries, err := outbox.Batch(50)
				if err != nil || len(entries) == 0 {
					if err != nil {
						sendErr <- err
					}
					continue
				}
				results := make([]*agentproto.ProbeResult, 0, len(entries))
				for _, entry := range entries {
					if result, ok := probeResultFromJSON(entry.Payload); ok {
						results = append(results, result)
					}
				}
				if err := send(&agentproto.ClientEnvelope{Type: "result_batch", ResultBatch: &agentproto.ResultBatch{Cursor: time.Now().UnixNano(), Results: results}}); err != nil {
					sendErr <- err
					return
				}
			}
		}
	}()
	defer func() { <-flushDone }()
	semaphore := make(chan struct{}, max(1, concurrency))
	var workers sync.WaitGroup
	defer func() {
		cancel()
		workers.Wait()
	}()
	incoming := make(chan *agentproto.ServerEnvelope, 64)
	receiveErr := make(chan error, 1)
	go func() {
		for {
			message, err := stream.Recv()
			if err != nil {
				receiveErr <- err
				return
			}
			select {
			case incoming <- message:
			case <-runCtx.Done():
				return
			}
		}
	}()
	for {
		select {
		case err := <-receiveErr:
			if err == io.EOF {
				return nil
			}
			return recoverExpiredEnrollment(ctx, err, client, recoveryCode, name, protocols, statePath, &current)
		case err := <-sendErr:
			return err
		case message := <-incoming:
			typeName := message.GetType()
			switch typeName {
			case "task":
				task := taskFromProto(message.GetTask())
				if task.TaskID == "" || task.JobID == "" {
					continue
				}
				if err := send(&agentproto.ClientEnvelope{Type: "task_ack", TaskAck: &agentproto.TaskAck{TaskId: task.TaskID, ObservedAt: time.Now().UTC().Format(time.RFC3339Nano)}}); err != nil {
					return err
				}
				semaphore <- struct{}{}
				workers.Add(1)
				go func() {
					defer func() { <-semaphore; workers.Done() }()
					result := probe.Run(runCtx, task)
					payload, _ := json.Marshal(result)
					values := map[string]any{}
					_ = json.Unmarshal(payload, &values)
					values["type"], values["taskId"] = "result", task.TaskID
					if err := outbox.Put(result.JobID, mustJSON(values)); err != nil {
						log.Printf("store probe result in outbox: %v", err)
					}
				}()
			case "result_ack":
				_ = outbox.Ack(message.GetResultAck().GetJobIds())
			case "credential_rotation":
				rotation := message.GetCredentialRotation()
				current.PendingToken = rotation.GetToken()
				current.PendingExpiresAt = rotation.GetExpiresAt()
				current.PendingRotationID = rotation.GetRotationId()
				if current.PendingToken == "" || current.PendingRotationID == "" {
					return fmt.Errorf("credential rotation response did not contain a token")
				}
				if err := probe.SaveState(statePath, current); err != nil {
					return err
				}
				if err := send(&agentproto.ClientEnvelope{Type: "credential_rotation_ack", CredentialRotationAck: &agentproto.CredentialRotationAck{RotationId: current.PendingRotationID}}); err != nil {
					return err
				}
			case "credential_rotation_committed":
				rotationID := message.GetRotationCommittedId()
				if rotationID == "" || rotationID != current.PendingRotationID || current.PendingToken == "" {
					return fmt.Errorf("credential rotation commit did not match local pending state")
				}
				current.Token, current.ExpiresAt = current.PendingToken, current.PendingExpiresAt
				current.PendingToken, current.PendingExpiresAt, current.PendingRotationID = "", "", ""
				if err := probe.SaveState(statePath, current); err != nil {
					return err
				}
				return nil
			case "credential_rotation_cancelled":
				if message.GetRotationCancelledId() == current.PendingRotationID {
					current.PendingToken, current.PendingExpiresAt, current.PendingRotationID = "", "", ""
					if err := probe.SaveState(statePath, current); err != nil {
						return err
					}
				}
			}
		}
	}
}

func enrollmentRequest(code, nodeID, name, protocols string) *agentproto.EnrollRequest {
	request := &agentproto.EnrollRequest{Code: code, Name: name, Protocols: splitProtocols(protocols), Version: version, Platform: runtime.GOOS + "/" + runtime.GOARCH, ProtocolVersion: "agent.v1", Features: agentFeatures()}
	if nodeID != "" {
		request.NodeId = nodeID
		request.RecoveryCode = code
	}
	return request
}

func stateFromEnrollment(response *agentproto.EnrollResponse) state {
	return state{NodeID: response.GetNodeId(), Token: response.GetToken(), ExpiresAt: response.GetTokenExpiresAt()}
}

func recoverExpiredEnrollment(ctx context.Context, cause error, client agentproto.AgentServiceV1Client, recoveryCode, name, protocols, statePath string, current *state) error {
	if !isExpiredTokenError(cause) {
		return cause
	}
	if recoveryCode == "" || current.NodeID == "" {
		return fmt.Errorf("agent token expired; provide a one-time --recovery-code to re-enroll: %w", cause)
	}
	response, err := client.Enroll(ctx, enrollmentRequest(recoveryCode, current.NodeID, name, protocols))
	if err != nil {
		return fmt.Errorf("recover enrollment: %w", err)
	}
	next := stateFromEnrollment(response)
	if next.Token == "" || next.NodeID != current.NodeID {
		return fmt.Errorf("recover enrollment returned invalid node credentials")
	}
	if err := probe.SaveState(statePath, next); err != nil {
		return fmt.Errorf("save recovered enrollment: %w", err)
	}
	*current = next
	log.Printf("re-enrolled node %s after credential expiry", current.NodeID)
	return errAgentReenrolled
}

func isExpiredTokenError(err error) bool {
	statusValue, ok := status.FromError(err)
	return ok && statusValue.Code() == codes.Unauthenticated && strings.Contains(strings.ToLower(statusValue.Message()), "expired")
}

func agentTLSConfig(serverName, rawFingerprint string) (*tls.Config, error) {
	fingerprint := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(rawFingerprint), ":", ""))
	if fingerprint != "" && len(fingerprint) != sha256.Size*2 {
		return nil, fmt.Errorf("TLS SHA-256 fingerprint must contain %d hexadecimal characters", sha256.Size*2)
	}
	config := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: serverName}
	if fingerprint == "" {
		return config, nil
	}
	config.VerifyPeerCertificate = func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return fmt.Errorf("control-plane did not provide a TLS certificate")
		}
		sum := sha256.Sum256(rawCerts[0])
		if fmt.Sprintf("%x", sum[:]) != fingerprint {
			return fmt.Errorf("control-plane TLS certificate fingerprint mismatch")
		}
		return nil
	}
	return config, nil
}

func mustJSON(value any) []byte {
	raw, _ := json.Marshal(value)
	return raw
}

func agentFeatures() []string {
	return []string{"task_ack", "result_batch", "result_ack", "credential_rotation", "credential_rotation_ack", "recovery_enroll"}
}

func splitProtocols(value string) []string {
	parts := strings.Split(value, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			result = append(result, part)
		}
	}
	return result
}

func taskFromProto(task *agentproto.ProbeTask) agenthub.Task {
	if task == nil {
		return agenthub.Task{}
	}
	statuses := make([]int, len(task.GetExpectedStatuses()))
	for index, value := range task.GetExpectedStatuses() {
		statuses[index] = int(value)
	}
	ports := make([]int, len(task.GetAllowedPorts()))
	for index, value := range task.GetAllowedPorts() {
		ports[index] = int(value)
	}
	return agenthub.Task{TaskID: task.GetTaskId(), JobID: task.GetJobId(), CandidateID: task.GetCandidateId(), RouteTargetID: task.GetRouteTargetId(), Mode: task.GetMode(), Address: task.GetAddress(), Port: int(task.GetPort()), URL: task.GetUrl(), Host: task.GetHost(), SNI: task.GetSni(), Method: task.GetMethod(), Headers: task.GetHeaders(), ExpectedStatuses: statuses, ExpectedContains: task.GetExpectedContains(), TimeoutMS: int(task.GetTimeoutMs()), MaxBytes: task.GetMaxBytes(), AllowedPorts: ports, Attempt: int(task.GetAttempt()), LeaseSeconds: int(task.GetLeaseSeconds())}
}

type storedProbeResult struct {
	probe.Result
	TaskID string `json:"taskId"`
}

func probeResultFromJSON(raw []byte) (*agentproto.ProbeResult, bool) {
	var stored storedProbeResult
	if json.Unmarshal(raw, &stored) != nil || stored.JobID == "" {
		return nil, false
	}
	return &agentproto.ProbeResult{TaskId: stored.TaskID, JobId: stored.JobID, CandidateId: stored.CandidateID, RouteTargetId: stored.RouteTargetID, Mode: stored.Mode, Success: stored.Success, ErrorCode: stored.ErrorCode, TcpMs: stored.TCPMS, TlsMs: stored.TLSMS, TtfbMs: stored.TTFBMS, TotalMs: stored.TotalMS, BytesRead: stored.BytesRead, DownloadBps: stored.DownloadBPS, StatusCode: int32(stored.StatusCode), ObservedAt: stored.ObservedAt.Format(time.RFC3339Nano)}, true
}

func env(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}
func hostname() string {
	value, err := os.Hostname()
	if err != nil {
		return "newnexus-agent"
	}
	return value
}
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
