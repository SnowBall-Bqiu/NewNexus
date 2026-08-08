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
}

type Hub struct {
	store  *store.Store
	broker *events.Broker
	mu     sync.RWMutex
	agents map[string]chan Task
}

func New(store *store.Store, broker *events.Broker) *Hub {
	return &Hub{store: store, broker: broker, agents: make(map[string]chan Task)}
}

func (h *Hub) Enroll(ctx context.Context, request *structpb.Struct) (*structpb.Struct, error) {
	code := fieldString(request, "code")
	if code == "" {
		return nil, status.Error(codes.InvalidArgument, "enrollment code is required")
	}
	var node domain.ProbeNode
	if err := h.store.DB.Where("enroll_code_hash = ?", security.HashToken(code)).First(&node).Error; err != nil {
		return nil, status.Error(codes.PermissionDenied, "invalid or expired enrollment code")
	}
	token, err := security.RandomToken(32)
	if err != nil {
		return nil, status.Error(codes.Internal, "could not create agent token")
	}
	now := time.Now().UTC()
	updates := map[string]any{"agent_token_hash": security.HashToken(token), "enroll_code_hash": "", "status": "offline", "version": fieldString(request, "version"), "last_seen_at": &now}
	if protocols := fieldString(request, "protocols"); protocols != "" {
		updates["protocols"] = protocols
	}
	if name := fieldString(request, "name"); name != "" {
		updates["name"] = name
	}
	if err := h.store.DB.Model(&node).Updates(updates).Error; err != nil {
		return nil, status.Error(codes.Internal, "could not enroll node")
	}
	response, _ := structpb.NewStruct(map[string]any{"nodeId": node.ID, "token": token, "protocolVersion": "agent.v1", "heartbeatSeconds": 15})
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
	if err := h.store.DB.Where("agent_token_hash = ?", security.HashToken(token)).First(&node).Error; err != nil {
		return status.Error(codes.Unauthenticated, "invalid agent token")
	}
	tasks := make(chan Task, 64)
	h.mu.Lock()
	if previous, ok := h.agents[node.ID]; ok {
		close(previous)
	}
	h.agents[node.ID] = tasks
	h.mu.Unlock()
	now := time.Now().UTC()
	_ = h.store.DB.Model(&node).Updates(map[string]any{"status": "online", "last_seen_at": &now}).Error
	h.publish("node.online", node.ID, node.Name+" 已连接")
	defer func() {
		h.mu.Lock()
		if current, ok := h.agents[node.ID]; ok && current == tasks {
			delete(h.agents, node.ID)
			close(tasks)
		}
		h.mu.Unlock()
		now := time.Now().UTC()
		_ = h.store.DB.Model(&node).Updates(map[string]any{"status": "offline", "last_seen_at": &now}).Error
		h.publish("node.offline", node.ID, node.Name+" 已断开")
	}()
	received := make(chan error, 1)
	go func() {
		for {
			message, err := stream.Recv()
			if err != nil {
				received <- err
				return
			}
			if err := h.handleMessage(node.ID, message); err != nil {
				slog.Warn("agent message rejected", "node", node.ID, "error", err)
			}
		}
	}()
	ack, _ := structpb.NewStruct(map[string]any{"type": "connected", "nodeId": node.ID, "serverTime": time.Now().UTC().Format(time.RFC3339Nano)})
	if err := stream.Send(ack); err != nil {
		return err
	}
	for {
		select {
		case <-stream.Context().Done():
			return stream.Context().Err()
		case err := <-received:
			if err == io.EOF {
				return nil
			}
			return err
		case task, ok := <-tasks:
			if !ok {
				return nil
			}
			payload := map[string]any{"type": "task"}
			raw, _ := json.Marshal(task)
			_ = json.Unmarshal(raw, &payload)
			message, _ := structpb.NewStruct(payload)
			if err := stream.Send(message); err != nil {
				return err
			}
		}
	}
}

func (h *Hub) Dispatch(nodeID string, task Task) error {
	if task.JobID == "" {
		task.JobID = uuid.NewString()
	}
	h.mu.RLock()
	channel, ok := h.agents[nodeID]
	h.mu.RUnlock()
	if !ok {
		return fmt.Errorf("node is not connected")
	}
	select {
	case channel <- task:
		return nil
	default:
		return fmt.Errorf("node task queue is full")
	}
}

func (h *Hub) Broadcast(task Task) int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	sent := 0
	for _, channel := range h.agents {
		copyTask := task
		copyTask.JobID = uuid.NewString()
		select {
		case channel <- copyTask:
			sent++
		default:
		}
	}
	return sent
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

func (h *Hub) handleMessage(nodeID string, message *structpb.Struct) error {
	typeName := fieldString(message, "type")
	now := time.Now().UTC()
	switch typeName {
	case "heartbeat":
		return h.store.DB.Model(&domain.ProbeNode{}).Where("id = ?", nodeID).Updates(map[string]any{"status": "online", "last_seen_at": &now}).Error
	case "result":
		sample := domain.ProbeSample{JobID: fieldString(message, "jobId"), NodeID: nodeID, CandidateID: fieldString(message, "candidateId"), RouteTargetID: fieldString(message, "routeTargetId"), Mode: fieldString(message, "mode"), Success: fieldBool(message, "success"), ErrorCode: fieldString(message, "errorCode"), TCPMS: fieldNumber(message, "tcpMs"), TLSMS: fieldNumber(message, "tlsMs"), TTFBMS: fieldNumber(message, "ttfbMs"), TotalMS: fieldNumber(message, "totalMs"), BytesRead: int64(fieldNumber(message, "bytesRead")), StatusCode: int(fieldNumber(message, "statusCode")), ObservedAt: now}
		if observed := fieldString(message, "observedAt"); observed != "" {
			if parsed, err := time.Parse(time.RFC3339Nano, observed); err == nil && time.Since(parsed) < 24*time.Hour {
				sample.ObservedAt = parsed
			}
		}
		if sample.JobID == "" {
			return fmt.Errorf("result is missing jobId")
		}
		if err := h.store.DB.Where("job_id = ?", sample.JobID).FirstOrCreate(&sample).Error; err != nil {
			return err
		}
		updates := map[string]any{"last_seen_at": &now, "status": "online", "tasks_completed": h.store.DB.Raw("tasks_completed + 1")}
		_ = updates
		_ = h.store.DB.Model(&domain.ProbeNode{}).Where("id = ?", nodeID).Updates(map[string]any{"last_seen_at": &now, "status": "online"}).UpdateColumn("tasks_completed", h.store.DB.Raw("tasks_completed + 1")).Error
		if sample.CandidateID != "" {
			_ = h.updateCandidate(sample)
		}
		if sample.RouteTargetID != "" {
			_ = h.updateRouteTarget(sample)
		}
		h.publish("probe.result", sample.JobID, map[bool]string{true: "探测成功", false: "探测失败"}[sample.Success])
		return nil
	default:
		return fmt.Errorf("unknown agent message type %q", typeName)
	}
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
	failureQuorum := failures*100 >= quorum*len(latest)
	healthyQuorum := (len(latest)-failures)*100 >= quorum*len(latest)
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
