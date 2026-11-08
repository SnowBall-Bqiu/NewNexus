package agenthub

import (
	"context"
	"errors"
	"github.com/newnexus/newnexus/internal/agentproto"
	"github.com/newnexus/newnexus/internal/auth"
	"github.com/newnexus/newnexus/internal/domain"
	"github.com/newnexus/newnexus/internal/security"
	"github.com/newnexus/newnexus/internal/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultHelloTimeout = 10 * time.Second
	maxPendingHellos    = 64
)

type Hub struct {
	Store           *store.Store
	Vault           *security.Vault
	mu              sync.Mutex
	streams         map[string]chan domain.Doc
	metricMu        sync.RWMutex
	metricPrepareMu sync.Mutex
	metricDays      map[string]bool
	helloTimeout    time.Duration
	helloSlots      chan struct{}
}

func New(s *store.Store) *Hub {
	return &Hub{
		Store:        s,
		streams:      map[string]chan domain.Doc{},
		helloTimeout: defaultHelloTimeout,
		helloSlots:   make(chan struct{}, maxPendingHellos),
	}
}
func (h *Hub) Enrollment(ctx context.Context, expires time.Duration, name string) (domain.Doc, error) {
	if expires < time.Minute || expires > 24*time.Hour {
		expires = time.Hour
	}
	token := auth.Token()
	d := domain.Doc{"id": auth.Hash(token), "name": name, "expires_at": time.Now().UTC().Add(expires).Format(time.RFC3339Nano), "used": false}
	e := h.Store.Put(ctx, "enrollment-tokens", d)
	return domain.Doc{"token": token, "expires_at": d["expires_at"]}, e
}
func (h *Hub) Enroll(ctx context.Context, in *structpb.Struct) (*structpb.Struct, error) {
	d := domain.Doc(in.AsMap())
	if domain.N(d, "protocol_version") != 1 {
		return nil, status.Error(codes.FailedPrecondition, "only protocol version 1 is supported")
	}
	node, e := h.consumeEnrollment(ctx, d)
	if e != nil {
		return nil, e
	}
	return structpb.NewStruct(map[string]any(node))
}
func (h *Hub) authenticate(ctx context.Context, id, credential string) (domain.Doc, error) {
	d, e := h.Store.Get(ctx, "nodes", id)
	if e != nil || domain.S(d, "status") == "revoked" {
		return nil, status.Error(codes.Unauthenticated, "node credential revoked or unknown")
	}
	hash := auth.Hash(credential)
	if hash != domain.S(d, "credential_hash") && hash != domain.S(d, "pending_credential_hash") {
		return nil, status.Error(codes.Unauthenticated, "invalid node credential")
	}
	return d, nil
}

// authenticateHello bounds unauthenticated streams across all connections to
// this Hub. Returning from Connect makes gRPC cancel a blocked Recv; a derived
// context deadline alone would not interrupt the stream's transport read.
func (h *Hub) authenticateHello(ctx context.Context, stream agentproto.AgentService_ConnectServer) (string, domain.Doc, error) {
	select {
	case <-ctx.Done():
		return "", nil, status.FromContextError(ctx.Err()).Err()
	case h.helloSlots <- struct{}{}:
	default:
		return "", nil, status.Error(codes.ResourceExhausted, "too many unauthenticated node streams")
	}
	// Keep the slot until both authentication and the receive goroutine have
	// finished. In particular, do not admit a replacement while a timed-out
	// stream's Recv is still waiting for gRPC to cancel its transport context.
	var unfinished atomic.Int32
	unfinished.Store(2)
	finished := func() {
		if unfinished.Add(-1) == 0 {
			<-h.helloSlots
		}
	}
	defer finished()
	type helloResult struct {
		frame *structpb.Struct
		err   error
	}
	received := make(chan helloResult, 1)
	go func() {
		defer finished()
		frame, err := stream.Recv()
		received <- helloResult{frame, err}
	}()
	timer := time.NewTimer(h.helloTimeout)
	defer timer.Stop()
	var hello *structpb.Struct
	select {
	case <-ctx.Done():
		return "", nil, status.FromContextError(ctx.Err()).Err()
	case <-timer.C:
		return "", nil, status.Error(codes.DeadlineExceeded, "node hello deadline exceeded")
	case result := <-received:
		if result.err != nil {
			return "", nil, result.err
		}
		hello = result.frame
	}
	first := domain.Doc(hello.AsMap())
	if domain.S(first, "type") != "hello" || domain.N(first, "protocol_version") != 1 {
		return "", nil, status.Error(codes.FailedPrecondition, "hello with protocol version 1 is required")
	}
	id := domain.S(first, "node_id")
	node, err := h.authenticate(ctx, id, domain.S(first, "credential"))
	return id, node, err
}

func (h *Hub) Connect(stream agentproto.AgentService_ConnectServer) error {
	ctx, cancel := context.WithCancel(stream.Context())
	defer cancel()
	id, node, e := h.authenticateHello(ctx, stream)
	if e != nil {
		return e
	}
	owner := domain.ID()
	acquired, e := h.Store.Lease(ctx, "node-connect:"+id, owner, 45*time.Second)
	if e != nil {
		return status.Error(codes.Unavailable, "cannot acquire node gateway lease")
	}
	if !acquired {
		return status.Error(codes.AlreadyExists, "node is connected to another gateway")
	}
	ctx = fencedContext(ctx, int64(domain.N(node, "session_generation")), owner)
	// The credential may have been revoked while the lease was acquired.
	if e = h.patchFrameNode(ctx, id, domain.Doc{}); e != nil {
		h.Store.ReleaseLease(context.Background(), "node-connect:"+id, owner)
		return status.Error(codes.Unauthenticated, "node credential revoked")
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		h.Store.ReleaseLease(cleanup, "node-connect:"+id, owner)
	}()
	out := make(chan domain.Doc, 256)
	h.mu.Lock()
	if _, ok := h.streams[id]; ok {
		h.mu.Unlock()
		return status.Error(codes.AlreadyExists, "node already connected")
	}
	h.streams[id] = out
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		if h.streams[id] == out {
			delete(h.streams, id)
		}
		h.mu.Unlock()
		cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = h.patchFrameNodeForLease(fencedContext(cleanup, int64(domain.N(node, "session_generation")), owner), id, owner, domain.Doc{"status": "offline"})
	}()
	received := make(chan domain.Doc, 32)
	errs := make(chan error, 1)
	go func() {
		for {
			m, e := stream.Recv()
			if e != nil {
				errs <- e
				return
			}
			select {
			case received <- domain.Doc(m.AsMap()):
			case <-ctx.Done():
				return
			}
		}
	}()
	writeFrame := func(d domain.Doc) error {
		m, e := structpb.NewStruct(map[string]any(d))
		if e != nil {
			return e
		}
		return stream.Send(m)
	}
	if e = writeFrame(domain.Doc{"type": "config", "protocol_version": 1, "heartbeat_seconds": 15, "max_batch_size": 256}); e != nil {
		return e
	}
	// Sending tasks must never stop the main loop from receiving ACK/results.
	// Otherwise simultaneous HTTP/2 flow-control exhaustion in both directions
	// can deadlock a reconnect with a large durable result batch.
	control := make(chan domain.Doc, 64)
	writeErrors := make(chan error, 1)
	go func() {
		for {
			var frame domain.Doc
			select {
			case frame = <-control:
			default:
				select {
				case <-ctx.Done():
					return
				case frame = <-control:
				case frame = <-out:
				}
			}
			if err := writeFrame(frame); err != nil {
				writeErrors <- err
				return
			}
			if domain.S(frame, "type") == "revoked" {
				writeErrors <- status.Error(codes.PermissionDenied, "node revoked")
				return
			}
		}
	}()
	send := func(frame domain.Doc) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case control <- frame:
			return nil
		default:
			// Results remain in the node's durable outbox until their ACK is
			// received. Reconnect is preferable to unbounded control buffering.
			return status.Error(codes.ResourceExhausted, "node control queue is full")
		}
	}
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	// A task persisted by another gateway must not wait for credential/lease
	// maintenance. Keep durable replay independent of its five-second cadence.
	taskTicker := time.NewTicker(time.Second)
	defer taskTicker.Stop()
	ackTicker := time.NewTicker(100 * time.Millisecond)
	defer ackTicker.Stop()
	defer cancel()
	pendingAcks := map[string]bool{}
	flushAcks := func() error {
		if len(pendingAcks) == 0 {
			return nil
		}
		if err := h.acknowledgeTasks(ctx, id, sortedKeys(pendingAcks)); err != nil {
			return status.Error(codes.Unavailable, "persist task acknowledgments: "+err.Error())
		}
		pendingAcks = map[string]bool{}
		return nil
	}
	frameError := func(err error) error {
		if errors.Is(err, errFrameNodeRevoked) {
			_ = send(domain.Doc{"type": "revoked", "reason": "revoked by administrator"})
			return status.Error(codes.PermissionDenied, "node revoked")
		}
		return status.Error(codes.Unavailable, "persist node frame: "+err.Error())
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case e = <-errs:
			if errors.Is(e, io.EOF) {
				if e = flushAcks(); e != nil {
					return e
				}
				return nil
			}
			return e
		case e = <-writeErrors:
			return e
		case d := <-received:
			switch domain.S(d, "type") {
			case "heartbeat":
				if e = h.heartbeatFrame(ctx, id, d); e != nil {
					return frameError(e)
				}
			case "credential_ack":
				if e = h.credentialAckFrame(ctx, id, domain.S(d, "credential_id")); e != nil {
					return frameError(e)
				}
			case "results":
				results := domain.A(d, "results")
				if len(results) > 256 {
					return status.Error(codes.ResourceExhausted, "batch exceeds 256 results")
				}
				if e = flushAcks(); e != nil {
					return e
				}
				if e = h.Ingest(ctx, id, results); e != nil {
					return status.Error(codes.InvalidArgument, e.Error())
				}
				if e = send(domain.Doc{"type": "ack", "batch_id": d["batch_id"]}); e != nil {
					return e
				}
			case "ack":
				if taskID := domain.S(d, "task_id"); taskID != "" && len(taskID) <= 512 {
					pendingAcks[taskID] = true
				}
				if len(pendingAcks) == 256 {
					if e = flushAcks(); e != nil {
						return e
					}
				}
			case "backpressure":
				if e = h.backpressureFrame(ctx, id, d); e != nil {
					return frameError(e)
				}
			}
		case <-ackTicker.C:
			if e = flushAcks(); e != nil {
				return e
			}
		case <-taskTicker.C:
			tasks, e := h.Store.PendingTasks(ctx, id, 512)
			if e != nil {
				continue
			}
			sent := 0
		replay:
			for _, task := range tasks {
				if domain.S(task, "node_id") != id || domain.S(task, "status") == "done" {
					continue
				}
				if domain.S(task, "status") == "accepted" {
					accepted, _ := time.Parse(time.RFC3339Nano, domain.S(task, "accepted_at"))
					if time.Since(accepted) < 2*time.Minute {
						continue
					}
				}
				select {
				case out <- domain.Doc{"type": "task", "task": sanitizeTask(task)}:
				default:
					break replay
				}
				sent++
				if sent >= 256 {
					break
				}
			}
		case <-ticker.C:
			if ok, e := h.Store.Lease(ctx, "node-connect:"+id, owner, 45*time.Second); e != nil || !ok {
				return status.Error(codes.Unavailable, "node gateway lease lost")
			}
			if current, e := h.Store.Get(ctx, "nodes", id); e == nil {
				if domain.S(current, "status") == "revoked" || domain.N(current, "session_generation") != domain.N(node, "session_generation") {
					_ = send(domain.Doc{"type": "revoked", "reason": "revoked by administrator"})
					return status.Error(codes.PermissionDenied, "node revoked")
				}
				if h.Vault != nil && domain.S(current, "pending_credential_encrypted") != "" {
					if credential, e := h.Vault.Open(domain.S(current, "pending_credential_encrypted"), "node-rotation:"+id); e == nil {
						if e = send(domain.Doc{"type": "credential", "credential": string(credential), "credential_id": current["pending_credential_hash"]}); e != nil {
							return e
						}
					}
				}
			} else {
				return status.Error(codes.PermissionDenied, "node no longer exists")
			}
		}
	}
}
func sanitizeTask(d domain.Doc) map[string]any {
	out := map[string]any(domain.Clone(d))
	delete(out, "_version")
	delete(out, "node_id")
	return out
}
func (h *Hub) Ingest(ctx context.Context, nodeID string, results []domain.Doc) error {
	return h.ingestBatch(ctx, nodeID, results)
}
func (h *Hub) Dispatch(ctx context.Context, nodeID string, task domain.Doc) error {
	task["node_id"] = nodeID
	task["status"] = "queued"
	if e := h.Store.PutNodeTasks(ctx, []domain.Doc{task}); e != nil {
		return e
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if ch := h.streams[nodeID]; ch != nil {
		select {
		case ch <- domain.Doc{"type": "task", "task": sanitizeTask(task)}:
		default:
		}
	}
	return nil
}

func (h *Hub) DispatchBatch(ctx context.Context, tasks []domain.Doc) error {
	if err := h.PersistTasks(ctx, tasks); err != nil {
		return err
	}
	h.PushTasks(tasks)
	return nil
}

// PersistTasks durably queues tasks without starting local delivery. Schedulers
// can update candidate scheduling metadata before probe results contend for the
// same rows. A gateway restart still replays these queued tasks from the database.
func (h *Hub) PersistTasks(ctx context.Context, tasks []domain.Doc) error {
	for _, task := range tasks {
		task["status"] = "queued"
	}
	return h.Store.PutNodeTasks(ctx, tasks)
}

func (h *Hub) PushTasks(tasks []domain.Doc) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, task := range tasks {
		if ch := h.streams[domain.S(task, "node_id")]; ch != nil {
			select {
			case ch <- domain.Doc{"type": "task", "task": sanitizeTask(task)}:
			default:
			}
		}
	}
}
func (h *Hub) Revoke(ctx context.Context, id string) error {
	if err := h.revokeNode(ctx, id); err != nil {
		return err
	}
	h.disconnect(id)
	return nil
}
func (h *Hub) Rotate(ctx context.Context, id string) error {
	if h.Vault == nil {
		return errors.New("credential vault unavailable")
	}
	h.mu.Lock()
	ch := h.streams[id]
	h.mu.Unlock()
	credential := auth.Token()
	hash := auth.Hash(credential)
	sealed, e := h.Vault.Seal([]byte(credential), "node-rotation:"+id)
	if e != nil {
		return e
	}
	if e = h.pendingRotation(ctx, id, hash, sealed, ch != nil); e != nil {
		return e
	}
	// Connect's main loop is the sole producer of credential frames. Its
	// five-second ticker re-reads durable pending state and queues credentials
	// in the same FIFO control path that processes persistence ACKs. Sending a
	// plaintext snapshot here could arrive after a later rotation was ACKed.
	return nil
}
