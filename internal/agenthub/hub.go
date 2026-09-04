package agenthub

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/SnowBall-Bqiu/NewNexus/internal/agentproto"
	"github.com/SnowBall-Bqiu/NewNexus/internal/domain"
	"github.com/SnowBall-Bqiu/NewNexus/internal/events"
	"github.com/SnowBall-Bqiu/NewNexus/internal/security"
	"github.com/SnowBall-Bqiu/NewNexus/internal/store"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type Task struct {
	JobID            string            `json:"jobId"`
	CandidateID      string            `json:"candidateId,omitempty"`
	RouteTargetID    string            `json:"routeTargetId,omitempty"`
	Mode             string            `json:"mode"`
	Address          string            `json:"address"`
	Port             int               `json:"port"`
	URL              string            `json:"url,omitempty"`
	Host             string            `json:"host,omitempty"`
	SNI              string            `json:"sni,omitempty"`
	Method           string            `json:"method,omitempty"`
	Headers          map[string]string `json:"headers,omitempty"`
	ExpectedStatuses []int             `json:"expectedStatuses,omitempty"`
	ExpectedContains string            `json:"expectedContains,omitempty"`
	TimeoutMS        int               `json:"timeoutMs"`
	MaxBytes         int64             `json:"maxBytes"`
	AllowedPorts     []int             `json:"allowedPorts,omitempty"`
	TaskID           string            `json:"taskId,omitempty"`
	Attempt          int               `json:"attempt,omitempty"`
	LeaseSeconds     int               `json:"leaseSeconds,omitempty"`
}

type Hub struct {
	store        *store.Store
	broker       *events.Broker
	mu           sync.RWMutex
	agents       map[string]chan Task
	responses    map[string]chan *structpb.Struct
	cancels      map[string]context.CancelFunc
	rotationAcks map[string]chan struct{}
	rotationWait time.Duration
}

func New(store *store.Store, broker *events.Broker) *Hub {
	return &Hub{store: store, broker: broker, agents: make(map[string]chan Task), responses: make(map[string]chan *structpb.Struct), cancels: make(map[string]context.CancelFunc), rotationAcks: make(map[string]chan struct{}), rotationWait: 20 * time.Second}
}

func (h *Hub) Enroll(ctx context.Context, request *structpb.Struct) (*structpb.Struct, error) {
	code := fieldString(request, "code")
	if code == "" {
		return nil, status.Error(codes.InvalidArgument, "enrollment code is required")
	}
	var node domain.ProbeNode
	now := time.Now().UTC()
	nodeID := fieldString(request, "nodeId")
	if nodeID != "" {
		recoveryCode := fieldString(request, "recoveryCode")
		if recoveryCode == "" {
			return nil, status.Error(codes.InvalidArgument, "recovery code is required")
		}
		if err := h.store.DB.Where("id = ? AND recovery_code_hash = ? AND recovery_expires_at > ? AND revoked_at IS NULL", nodeID, security.HashToken(recoveryCode), now).First(&node).Error; err != nil {
			return nil, status.Error(codes.PermissionDenied, "invalid or expired recovery code")
		}
	} else if err := h.store.DB.Where("enroll_code_hash = ? AND enroll_expires_at > ? AND revoked_at IS NULL", security.HashToken(code), now).First(&node).Error; err != nil {
		return nil, status.Error(codes.PermissionDenied, "invalid or expired enrollment code")
	}
	token, err := security.RandomToken(32)
	if err != nil {
		return nil, status.Error(codes.Internal, "could not create agent token")
	}
	expiresAt := now.Add(90 * 24 * time.Hour)
	protocolVersion := fieldString(request, "protocolVersion")
	if protocolVersion == "" {
		protocolVersion = "legacy"
	}
	updates := map[string]any{"agent_token_hash": security.HashToken(token), "previous_token_hash": "", "token_expires_at": &expiresAt, "enroll_code_hash": "", "enroll_expires_at": nil, "recovery_code_hash": "", "recovery_expires_at": nil, "pending_token_hash": "", "pending_token_expires_at": nil, "rotation_id": "", "rotation_requested_at": nil, "status": "offline", "version": fieldString(request, "version"), "protocol_version": protocolVersion, "compatibility_status": map[bool]string{true: "compatible", false: "degraded"}[protocolVersion == "agent.v1"], "last_seen_at": &now}
	if protocols := fieldString(request, "protocols"); protocols != "" {
		updates["protocols"] = protocols
	}
	if name := fieldString(request, "name"); name != "" {
		updates["name"] = name
	}
	if err := h.store.DB.Model(&node).Updates(updates).Error; err != nil {
		return nil, status.Error(codes.Internal, "could not enroll node")
	}
	response, _ := structpb.NewStruct(map[string]any{"nodeId": node.ID, "token": token, "tokenExpiresAt": expiresAt.Format(time.RFC3339Nano), "protocolVersion": "agent.v1", "heartbeatSeconds": 15, "features": []any{"task_ack", "result_batch", "result_ack", "credential_rotation", "credential_rotation_ack", "recovery_enroll"}})
	return response, nil
}

func (h *Hub) Connect(stream agentproto.ConnectServer) error {
	md, _ := metadata.FromIncomingContext(stream.Context())
	authorization := ""
	if values := md.Get("authorization"); len(values) > 0 {
		authorization = values[0]
	}
	token := strings.TrimPrefix(authorization, "Bearer ")
	if token == "" {
		return status.Error(codes.Unauthenticated, "agent token is required")
	}
	var node domain.ProbeNode
	now := time.Now().UTC()
	tokenHash := security.HashToken(token)
	if err := h.store.DB.Where("agent_token_hash = ? OR previous_token_hash = ?", tokenHash, tokenHash).First(&node).Error; err != nil {
		return status.Error(codes.Unauthenticated, "invalid agent token")
	}
	if node.RevokedAt != nil {
		return status.Error(codes.Unauthenticated, "agent token revoked")
	}
	isPrevious := tokenHash == node.PreviousTokenHash
	if isPrevious && (node.TokenRotateUntil == nil || !node.TokenRotateUntil.After(now)) {
		return status.Error(codes.Unauthenticated, "invalid agent token")
	}
	if !isPrevious && node.TokenExpiresAt != nil && !node.TokenExpiresAt.After(now) {
		_ = h.store.DB.Model(&node).Updates(map[string]any{"status": "re_enroll_required", "last_seen_at": &now}).Error
		h.publish("node.reenroll_required", node.ID, node.Name+" 的凭据已过期，需要恢复码重新注册")
		return status.Error(codes.Unauthenticated, "agent token expired")
	}
	if tokenHash == node.AgentTokenHash && node.PreviousTokenHash != "" {
		_ = h.store.DB.Model(&node).Updates(map[string]any{"previous_token_hash": "", "token_rotate_until": nil}).Error
	}
	connectionCtx, cancel := context.WithCancel(stream.Context())
	defer cancel()
	tasks := make(chan Task, 64)
	responses := make(chan *structpb.Struct, 64)
	h.mu.Lock()
	if previous, ok := h.cancels[node.ID]; ok {
		previous()
	}
	h.agents[node.ID] = tasks
	h.responses[node.ID] = responses
	h.cancels[node.ID] = cancel
	h.mu.Unlock()
	now = time.Now().UTC()
	_ = h.store.DB.Model(&node).Updates(map[string]any{"status": "online", "last_seen_at": &now}).Error
	h.publish("node.online", node.ID, node.Name+" 已连接")
	defer func() {
		h.mu.Lock()
		if current, ok := h.agents[node.ID]; ok && current == tasks {
			delete(h.agents, node.ID)
			delete(h.responses, node.ID)
			delete(h.cancels, node.ID)
			now := time.Now().UTC()
			_ = h.store.DB.Model(&node).Updates(map[string]any{"status": "offline", "last_seen_at": &now}).Error
			h.publish("node.offline", node.ID, node.Name+" 已断开")
		}
		h.mu.Unlock()
	}()
	received := make(chan error, 1)
	go func() {
		for {
			message, err := stream.Recv()
			if err != nil {
				received <- err
				return
			}
			reply, err := h.handleMessage(node.ID, message)
			if err != nil {
				if fieldString(message, "type") == "hello" {
					received <- status.Error(codes.FailedPrecondition, err.Error())
					return
				}
				slog.Warn("agent message rejected", "node", node.ID, "error", err)
				continue
			}
			if reply != nil {
				select {
				case responses <- reply:
				case <-connectionCtx.Done():
					return
				}
			}
		}
	}()
	ack, _ := structpb.NewStruct(map[string]any{"type": "connected", "nodeId": node.ID, "protocolVersion": "agent.v1", "enabledFeatures": []any{"task_ack", "result_batch", "result_ack", "credential_rotation", "credential_rotation_ack", "recovery_enroll"}, "serverTime": time.Now().UTC().Format(time.RFC3339Nano)})
	if err := stream.Send(ack); err != nil {
		return err
	}
	pendingTicker := time.NewTicker(2 * time.Second)
	defer pendingTicker.Stop()
	for {
		select {
		case <-connectionCtx.Done():
			return connectionCtx.Err()
		case err := <-received:
			if err == io.EOF {
				return nil
			}
			return err
		case task, ok := <-tasks:
			if !ok {
				return nil
			}
			if _, err := h.sendTask(stream, node.ID, task); err != nil {
				return err
			}
		case message := <-responses:
			if err := stream.Send(message); err != nil {
				return err
			}
		case <-pendingTicker.C:
			if task, ok := h.nextPendingTask(node.ID); ok {
				if _, err := h.sendTask(stream, node.ID, task); err != nil {
					return err
				}
			}
		}
	}
}

func (h *Hub) Dispatch(nodeID string, task Task) error {
	if task.JobID == "" {
		task.JobID = uuid.NewString()
	}
	if task.LeaseSeconds <= 0 {
		task.LeaseSeconds = 90
	}
	raw, err := json.Marshal(task)
	if err != nil {
		return err
	}
	persisted := domain.AgentTask{JobID: task.JobID, NodeID: nodeID, CandidateID: task.CandidateID, RouteTargetID: task.RouteTargetID, Kind: "probe", PayloadJSON: string(raw), Status: "queued"}
	if err := h.store.DB.Create(&persisted).Error; err != nil {
		return err
	}
	task.TaskID = persisted.ID
	h.mu.RLock()
	channel, ok := h.agents[nodeID]
	h.mu.RUnlock()
	if !ok {
		return nil
	}
	select {
	case channel <- task:
		return nil
	default:
		return nil
	}
}

func (h *Hub) Broadcast(task Task) int {
	h.mu.RLock()
	ids := make([]string, 0, len(h.agents))
	for id := range h.agents {
		ids = append(ids, id)
	}
	h.mu.RUnlock()
	sent := 0
	for _, nodeID := range ids {
		copyTask := task
		copyTask.JobID = uuid.NewString()
		if h.Dispatch(nodeID, copyTask) == nil {
			sent++
		}
	}
	return sent
}

func (h *Hub) sendTask(stream agentproto.ConnectServer, nodeID string, task Task) (bool, error) {
	if task.TaskID == "" {
		return false, nil
	}
	now := time.Now().UTC()
	leaseSeconds := task.LeaseSeconds
	if leaseSeconds <= 0 {
		leaseSeconds = 90
	}
	leaseUntil := now.Add(time.Duration(leaseSeconds) * time.Second)
	result := h.store.DB.Model(&domain.AgentTask{}).Where("id = ? AND node_id = ? AND (status = ? OR (status IN ? AND lease_until < ?))", task.TaskID, nodeID, "queued", []string{"leased", "acknowledged"}, now).Updates(map[string]any{"status": "leased", "lease_until": leaseUntil, "attempt": gorm.Expr("attempt + 1")})
	if result.Error != nil || result.RowsAffected != 1 {
		return false, result.Error
	}
	var persisted domain.AgentTask
	_ = h.store.DB.First(&persisted, "id = ?", task.TaskID).Error
	task.Attempt = persisted.Attempt
	task.LeaseSeconds = leaseSeconds
	payload := map[string]any{"type": "task"}
	raw, _ := json.Marshal(task)
	_ = json.Unmarshal(raw, &payload)
	message, _ := structpb.NewStruct(payload)
	if err := stream.Send(message); err != nil {
		return false, err
	}
	return true, nil
}

func (h *Hub) nextPendingTask(nodeID string) (Task, bool) {
	now := time.Now().UTC()
	var persisted domain.AgentTask
	if err := h.store.DB.Where("node_id = ? AND (status = ? OR (status IN ? AND lease_until < ?))", nodeID, "queued", []string{"leased", "acknowledged"}, now).Order("created_at ASC").First(&persisted).Error; err != nil {
		return Task{}, false
	}
	var task Task
	if json.Unmarshal([]byte(persisted.PayloadJSON), &task) != nil {
		_ = h.store.DB.Model(&persisted).Updates(map[string]any{"status": "failed", "completed_at": now}).Error
		return Task{}, false
	}
	task.TaskID, task.JobID = persisted.ID, persisted.JobID
	return task, true
}

func (h *Hub) ConnectedNodeIDs() []string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	ids := make([]string, 0, len(h.agents))
	for id := range h.agents {
		ids = append(ids, id)
	}
	return ids
}

func (h *Hub) RotateCredentials(nodeID string) (string, time.Time, error) {
	var node domain.ProbeNode
	if err := h.store.DB.First(&node, "id = ? AND revoked_at IS NULL", nodeID).Error; err != nil {
		return "", time.Time{}, err
	}
	h.mu.RLock()
	responseChannel := h.responses[nodeID]
	h.mu.RUnlock()
	if responseChannel == nil {
		return "", time.Time{}, fmt.Errorf("agent must be online to rotate credentials")
	}
	if node.RotationID != "" || node.PendingTokenHash != "" {
		return "", time.Time{}, fmt.Errorf("credential rotation is already pending")
	}
	token, err := security.RandomToken(32)
	if err != nil {
		return "", time.Time{}, err
	}
	now := time.Now().UTC()
	expires := now.Add(90 * 24 * time.Hour)
	rotationID := uuid.NewString()
	if err := h.store.DB.Model(&node).Updates(map[string]any{"pending_token_hash": security.HashToken(token), "pending_token_expires_at": &expires, "rotation_id": rotationID, "rotation_requested_at": &now, "status": "rotation_pending"}).Error; err != nil {
		return "", time.Time{}, err
	}
	acknowledged := make(chan struct{}, 1)
	h.mu.Lock()
	h.rotationAcks[nodeID] = acknowledged
	h.mu.Unlock()
	message, _ := structpb.NewStruct(map[string]any{"type": "credential_rotation", "rotationId": rotationID, "token": token, "expiresAt": expires.Format(time.RFC3339Nano)})
	select {
	case responseChannel <- message:
	case <-time.After(2 * time.Second):
		h.cancelRotation(node, rotationID, responseChannel)
		return "", time.Time{}, fmt.Errorf("agent did not accept credential rotation")
	}
	select {
	case <-acknowledged:
		return token, expires, nil
	case <-time.After(h.rotationWait):
		h.cancelRotation(node, rotationID, responseChannel)
		return "", time.Time{}, fmt.Errorf("agent did not acknowledge credential rotation")
	}
}

func (h *Hub) cancelRotation(node domain.ProbeNode, rotationID string, responseChannel chan *structpb.Struct) {
	_ = h.store.DB.Model(&node).Where("rotation_id = ?", rotationID).Updates(map[string]any{"pending_token_hash": "", "pending_token_expires_at": nil, "rotation_id": "", "rotation_requested_at": nil, "status": "online"}).Error
	h.mu.Lock()
	delete(h.rotationAcks, node.ID)
	h.mu.Unlock()
	message, _ := structpb.NewStruct(map[string]any{"type": "credential_rotation_cancelled", "rotationId": rotationID})
	select {
	case responseChannel <- message:
	default:
	}
}

func (h *Hub) CreateRecoveryCode(nodeID string) (string, time.Time, error) {
	var node domain.ProbeNode
	if err := h.store.DB.First(&node, "id = ? AND revoked_at IS NULL", nodeID).Error; err != nil {
		return "", time.Time{}, err
	}
	code, err := security.RandomToken(18)
	if err != nil {
		return "", time.Time{}, err
	}
	expires := time.Now().UTC().Add(15 * time.Minute)
	if err := h.store.DB.Model(&node).Updates(map[string]any{"recovery_code_hash": security.HashToken(code), "recovery_expires_at": &expires, "status": "re_enroll_required"}).Error; err != nil {
		return "", time.Time{}, err
	}
	h.publish("node.recovery_code_created", node.ID, "已创建一次性 Agent 恢复码")
	return code, expires, nil
}

func (h *Hub) RevokeRecoveryCode(nodeID string) error {
	result := h.store.DB.Model(&domain.ProbeNode{}).Where("id = ?", nodeID).Updates(map[string]any{"recovery_code_hash": "", "recovery_expires_at": nil})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	h.publish("node.recovery_code_revoked", nodeID, "Agent 一次性恢复码已撤销")
	return nil
}

func (h *Hub) RevokeNode(nodeID string) error {
	now := time.Now().UTC()
	if err := h.store.DB.Model(&domain.ProbeNode{}).Where("id = ?", nodeID).Updates(map[string]any{"revoked_at": now, "agent_token_hash": "", "previous_token_hash": "", "pending_token_hash": "", "pending_token_expires_at": nil, "rotation_id": "", "recovery_code_hash": "", "recovery_expires_at": nil, "status": "revoked"}).Error; err != nil {
		return err
	}
	h.mu.RLock()
	cancel := h.cancels[nodeID]
	h.mu.RUnlock()
	if cancel != nil {
		cancel()
	}
	h.publish("node.revoked", nodeID, "Agent 凭据已撤销")
	return nil
}

func (h *Hub) confirmRotation(nodeID, rotationID string, now time.Time) (*structpb.Struct, error) {
	if rotationID == "" {
		return nil, fmt.Errorf("credential rotation acknowledgement is missing rotationId")
	}
	var node domain.ProbeNode
	if err := h.store.DB.First(&node, "id = ? AND revoked_at IS NULL", nodeID).Error; err != nil {
		return nil, err
	}
	if node.RotationID != rotationID || node.PendingTokenHash == "" || node.PendingTokenExpiresAt == nil {
		return nil, fmt.Errorf("credential rotation acknowledgement does not match a pending rotation")
	}
	rotateUntil := now.Add(10 * time.Minute)
	updates := map[string]any{
		"previous_token_hash":      node.AgentTokenHash,
		"agent_token_hash":         node.PendingTokenHash,
		"token_expires_at":         node.PendingTokenExpiresAt,
		"token_rotate_until":       &rotateUntil,
		"pending_token_hash":       "",
		"pending_token_expires_at": nil,
		"rotation_id":              "",
		"rotation_requested_at":    nil,
		"status":                   "online",
	}
	if err := h.store.DB.Model(&node).Where("rotation_id = ?", rotationID).Updates(updates).Error; err != nil {
		return nil, err
	}
	h.mu.Lock()
	acknowledged := h.rotationAcks[nodeID]
	delete(h.rotationAcks, nodeID)
	h.mu.Unlock()
	if acknowledged != nil {
		select {
		case acknowledged <- struct{}{}:
		default:
		}
	}
	h.publish("node.credentials_rotated", nodeID, "Agent 凭据轮换已确认")
	committed, _ := structpb.NewStruct(map[string]any{"type": "credential_rotation_committed", "rotationId": rotationID})
	return committed, nil
}

func (h *Hub) handleMessage(nodeID string, message *structpb.Struct) (*structpb.Struct, error) {
	typeName := fieldString(message, "type")
	now := time.Now().UTC()
	switch typeName {
	case "hello":
		protocolVersion := fieldString(message, "protocolVersion")
		if protocolVersion != "agent.v1" && !strings.HasPrefix(protocolVersion, "1.") {
			_ = h.store.DB.Model(&domain.ProbeNode{}).Where("id = ?", nodeID).Updates(map[string]any{"protocol_version": protocolVersion, "compatibility_status": "incompatible"}).Error
			return nil, fmt.Errorf("unsupported agent protocol version %q", protocolVersion)
		}
		features := fieldStrings(message, "features")
		if err := h.store.DB.Model(&domain.ProbeNode{}).Where("id = ?", nodeID).Updates(map[string]any{"protocol_version": protocolVersion, "features_json": mustJSON(features), "compatibility_status": "compatible", "last_seen_at": &now}).Error; err != nil {
			return nil, err
		}
		reply, _ := structpb.NewStruct(map[string]any{"type": "hello_ack", "protocolVersion": "agent.v1", "enabledFeatures": []any{"task_ack", "result_batch", "result_ack", "credential_rotation", "credential_rotation_ack", "recovery_enroll"}})
		return reply, nil
	case "heartbeat":
		return nil, h.store.DB.Model(&domain.ProbeNode{}).Where("id = ?", nodeID).Updates(map[string]any{"status": "online", "last_seen_at": &now}).Error
	case "task_ack":
		taskID := fieldString(message, "taskId")
		if taskID == "" {
			return nil, fmt.Errorf("task acknowledgement is missing taskId")
		}
		return nil, h.store.DB.Model(&domain.AgentTask{}).Where("id = ? AND node_id = ? AND status = ?", taskID, nodeID, "leased").Updates(map[string]any{"status": "acknowledged", "acknowledged_at": now}).Error
	case "credential_rotation_ack":
		return h.confirmRotation(nodeID, fieldString(message, "rotationId"), now)
	case "result":
		jobID, err := h.handleResult(nodeID, message)
		if err != nil {
			return nil, err
		}
		ack, _ := structpb.NewStruct(map[string]any{"type": "result_ack", "jobIds": []any{jobID}})
		return ack, nil
	case "result_batch":
		rawResults, _ := message.AsMap()["results"].([]any)
		if len(rawResults) > 100 {
			return nil, fmt.Errorf("result batch exceeds 100 entries")
		}
		acked, err := h.handleResultBatch(nodeID, rawResults)
		if err != nil {
			return nil, err
		}
		h.publish("probe.batch", nodeID, fmt.Sprintf("已接收 %d 条探测结果", len(acked)))
		ack, _ := structpb.NewStruct(map[string]any{"type": "result_ack", "cursor": fieldNumber(message, "cursor"), "jobIds": acked})
		return ack, nil
	default:
		return nil, fmt.Errorf("unknown agent message type %q", typeName)
	}
}

func (h *Hub) handleResult(nodeID string, message *structpb.Struct) (string, error) {
	acked, err := h.handleResultBatch(nodeID, []any{message.AsMap()})
	if err != nil {
		return "", err
	}
	if len(acked) == 0 {
		return "", fmt.Errorf("result is missing jobId")
	}
	return fmt.Sprint(acked[0]), nil
}

func (h *Hub) handleResultBatch(nodeID string, rawResults []any) ([]any, error) {
	now := time.Now().UTC()
	samples := make([]domain.ProbeSample, 0, len(rawResults))
	acked := make([]any, 0, len(rawResults))
	taskIDs := make([]string, 0, len(rawResults))
	seen := make(map[string]bool)
	for _, raw := range rawResults {
		values, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		message, err := structpb.NewStruct(values)
		if err != nil {
			continue
		}
		sample, err := probeSampleFromMessage(nodeID, message, now)
		if err != nil {
			return nil, err
		}
		if seen[sample.JobID] {
			continue
		}
		seen[sample.JobID] = true
		samples = append(samples, sample)
		acked = append(acked, sample.JobID)
		if taskID := fieldString(message, "taskId"); taskID != "" {
			taskIDs = append(taskIDs, taskID)
		}
	}
	if len(samples) == 0 {
		return acked, nil
	}
	inserted := make([]domain.ProbeSample, 0, len(samples))
	if err := h.store.DB.Transaction(func(tx *gorm.DB) error {
		for index := range samples {
			if samples[index].ID == "" {
				samples[index].ID = uuid.NewString()
			}
			receipt := domain.ProbeSampleReceipt{JobID: samples[index].JobID, SampleID: samples[index].ID, ObservedAt: samples[index].ObservedAt, CreatedAt: now}
			result := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "job_id"}}, DoNothing: true}).Create(&receipt)
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected == 1 {
				inserted = append(inserted, samples[index])
			}
		}
		if len(inserted) == 0 {
			return nil
		}
		return tx.CreateInBatches(&inserted, 100).Error
	}); err != nil {
		return nil, err
	}
	if len(taskIDs) > 0 {
		_ = h.store.DB.Model(&domain.AgentTask{}).Where("id IN ? AND node_id = ?", taskIDs, nodeID).Updates(map[string]any{"status": "completed", "completed_at": now, "lease_until": nil}).Error
	}
	updates := map[string]any{"last_seen_at": &now, "status": "online"}
	if len(inserted) > 0 {
		updates["tasks_completed"] = gorm.Expr("tasks_completed + ?", len(inserted))
	}
	_ = h.store.DB.Model(&domain.ProbeNode{}).Where("id = ?", nodeID).Updates(updates).Error
	for _, sample := range inserted {
		if sample.CandidateID != "" {
			_ = h.updateCandidate(sample)
		}
		if sample.RouteTargetID != "" {
			_ = h.updateRouteTarget(sample)
		}
	}
	return acked, nil
}

func probeSampleFromMessage(nodeID string, message *structpb.Struct, now time.Time) (domain.ProbeSample, error) {
	sample := domain.ProbeSample{JobID: fieldString(message, "jobId"), NodeID: nodeID, CandidateID: fieldString(message, "candidateId"), RouteTargetID: fieldString(message, "routeTargetId"), Mode: fieldString(message, "mode"), Success: fieldBool(message, "success"), ErrorCode: fieldString(message, "errorCode"), TCPMS: fieldNumber(message, "tcpMs"), TLSMS: fieldNumber(message, "tlsMs"), TTFBMS: fieldNumber(message, "ttfbMs"), TotalMS: fieldNumber(message, "totalMs"), BytesRead: int64(fieldNumber(message, "bytesRead")), DownloadBPS: fieldNumber(message, "downloadBps"), StatusCode: int(fieldNumber(message, "statusCode")), ObservedAt: now}
	if observed := fieldString(message, "observedAt"); observed != "" {
		if parsed, err := time.Parse(time.RFC3339Nano, observed); err == nil && parsed.After(now.Add(-24*time.Hour)) && parsed.Before(now.Add(5*time.Minute)) {
			sample.ObservedAt = parsed
		}
	}
	if sample.JobID == "" {
		return sample, fmt.Errorf("result is missing jobId")
	}
	return sample, nil
}

func (h *Hub) updateCandidate(sample domain.ProbeSample) error {
	var candidate domain.Candidate
	if err := h.store.DB.First(&candidate, "id = ?", sample.CandidateID).Error; err != nil {
		return err
	}
	alpha := 0.25
	if candidate.TCPMS == 0 {
		alpha = 1
	}
	candidate.TCPMS = candidate.TCPMS*(1-alpha) + sample.TCPMS*alpha
	candidate.TLSMS = candidate.TLSMS*(1-alpha) + sample.TLSMS*alpha
	candidate.TTFBMS = candidate.TTFBMS*(1-alpha) + sample.TTFBMS*alpha
	candidate.Healthy = sample.Success
	candidate.LastProbedAt = &sample.ObservedAt
	latency := candidate.TCPMS + candidate.TLSMS + candidate.TTFBMS
	candidate.Score = maxFloat(0, minFloat(100, 100-latency/4))
	if sample.Success {
		candidate.SuccessRate = minFloat(100, candidate.SuccessRate*0.98+2)
	} else {
		candidate.SuccessRate *= 0.9
	}
	return h.store.DB.Save(&candidate).Error
}

func (h *Hub) updateRouteTarget(sample domain.ProbeSample) error {
	var target domain.RouteTarget
	if err := h.store.DB.First(&target, "id = ?", sample.RouteTargetID).Error; err != nil {
		return err
	}
	var line domain.RouteLine
	if err := h.store.DB.First(&line, "id = ?", target.RouteLineID).Error; err != nil {
		return err
	}
	var recent []domain.ProbeSample
	if err := h.store.DB.Where("route_target_id = ? AND observed_at >= ?", target.ID, time.Now().UTC().Add(-2*time.Minute)).Order("observed_at desc").Limit(200).Find(&recent).Error; err != nil {
		return err
	}
	latest := make(map[string]domain.ProbeSample)
	for _, candidate := range recent {
		if _, exists := latest[candidate.NodeID]; exists {
			continue
		}
		if line.HealthPerspective == "matching_group" && line.NodeGroup != "" {
			var node domain.ProbeNode
			if err := h.store.DB.Select("group_name").First(&node, "id = ?", candidate.NodeID).Error; err != nil || node.GroupName != line.NodeGroup {
				continue
			}
		}
		latest[candidate.NodeID] = candidate
	}
	minimumNodes := line.MinNodes
	if minimumNodes < 2 {
		minimumNodes = 2
	}
	if len(latest) < minimumNodes {
		target.Health = "unknown"
		target.LastCheckedAt = &sample.ObservedAt
		return h.store.DB.Save(&target).Error
	}
	failures := 0
	for _, latestSample := range latest {
		if !latestSample.Success {
			failures++
		}
	}
	quorum := line.QuorumPercent
	if quorum <= 0 {
		quorum = 67
	}
	failureQuorum := meetsQuorum(failures, len(latest), quorum)
	healthyQuorum := meetsQuorum(len(latest)-failures, len(latest), quorum)
	if failureQuorum {
		target.ConsecutiveFailures++
		target.ConsecutiveHealthy = 0
		if target.ConsecutiveFailures >= 3 {
			target.Health = "unhealthy"
		} else {
			target.Health = "degraded"
		}
	} else if healthyQuorum {
		target.ConsecutiveHealthy++
		target.ConsecutiveFailures = 0
		if target.ConsecutiveHealthy >= 5 {
			target.Health = "healthy"
		} else if target.Health == "unhealthy" {
			target.Health = "recovering"
		}
	} else {
		target.Health = "degraded"
	}
	target.Score = maxFloat(0, minFloat(100, 100-sample.TotalMS/5))
	target.LastCheckedAt = &sample.ObservedAt
	return h.store.DB.Save(&target).Error
}

func meetsQuorum(votes, total, percent int) bool {
	if total <= 0 || votes < 0 {
		return false
	}
	if percent <= 0 {
		percent = 67
	}
	required := (percent*total + 99) / 100
	if percent == 67 {
		// The default represents an exact two-thirds policy; 67 is only its
		// integer display value in persisted configuration.
		required = (2*total + 2) / 3
	}
	return votes >= required
}

func (h *Hub) publish(eventType, resourceID, message string) {
	payload, _ := json.Marshal(map[string]any{"type": eventType, "resourceId": resourceID, "message": message, "at": time.Now().UTC()})
	h.broker.Publish(payload)
}

func fieldString(message *structpb.Struct, name string) string {
	if value, ok := message.Fields[name]; ok {
		return value.GetStringValue()
	}
	return ""
}
func fieldStrings(message *structpb.Struct, name string) []string {
	value, ok := message.Fields[name]
	if !ok || value.GetListValue() == nil {
		return nil
	}
	result := make([]string, 0, len(value.GetListValue().Values))
	for _, item := range value.GetListValue().Values {
		if item.GetStringValue() != "" {
			result = append(result, item.GetStringValue())
		}
	}
	return result
}
func fieldNumber(message *structpb.Struct, name string) float64 {
	if value, ok := message.Fields[name]; ok {
		return value.GetNumberValue()
	}
	return 0
}
func fieldBool(message *structpb.Struct, name string) bool {
	if value, ok := message.Fields[name]; ok {
		return value.GetBoolValue()
	}
	return false
}
func minFloat(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func mustJSON(value any) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}
