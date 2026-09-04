package agenthub

import (
	"context"
	"fmt"
	"strings"

	"github.com/SnowBall-Bqiu/NewNexus/internal/agentproto"
	"google.golang.org/protobuf/types/known/structpb"
)

// V1Server exposes the typed Agent protocol while the Hub continues to own the
// shared authentication, task lease, result idempotency, and rotation state.
type V1Server struct {
	agentproto.UnimplementedAgentServiceV1Server
	hub *Hub
}

func NewV1Server(hub *Hub) *V1Server { return &V1Server{hub: hub} }

func (s *V1Server) Enroll(ctx context.Context, request *agentproto.EnrollRequest) (*agentproto.EnrollResponse, error) {
	code := request.GetCode()
	if code == "" && request.GetNodeId() != "" {
		code = request.GetRecoveryCode()
	}
	legacy, err := structpb.NewStruct(map[string]any{
		"code":            code,
		"nodeId":          request.GetNodeId(),
		"recoveryCode":    request.GetRecoveryCode(),
		"name":            request.GetName(),
		"protocols":       strings.Join(request.GetProtocols(), ","),
		"version":         request.GetVersion(),
		"platform":        request.GetPlatform(),
		"protocolVersion": request.GetProtocolVersion(),
		"features":        stringSliceToAny(request.GetFeatures()),
	})
	if err != nil {
		return nil, err
	}
	response, err := s.hub.Enroll(ctx, legacy)
	if err != nil {
		return nil, err
	}
	return &agentproto.EnrollResponse{
		NodeId:           fieldString(response, "nodeId"),
		Token:            fieldString(response, "token"),
		TokenExpiresAt:   fieldString(response, "tokenExpiresAt"),
		ProtocolVersion:  fieldString(response, "protocolVersion"),
		HeartbeatSeconds: int32(fieldNumber(response, "heartbeatSeconds")),
		EnabledFeatures:  fieldStrings(response, "features"),
	}, nil
}

func (s *V1Server) Connect(stream agentproto.AgentServiceV1_ConnectServer) error {
	return s.hub.Connect(&typedConnectBridge{AgentServiceV1_ConnectServer: stream})
}

type typedConnectBridge struct {
	agentproto.AgentServiceV1_ConnectServer
}

func (s *typedConnectBridge) Send(message *structpb.Struct) error {
	envelope, err := serverEnvelopeFromStruct(message)
	if err != nil {
		return err
	}
	return s.AgentServiceV1_ConnectServer.Send(envelope)
}

func (s *typedConnectBridge) Recv() (*structpb.Struct, error) {
	envelope, err := s.AgentServiceV1_ConnectServer.Recv()
	if err != nil {
		return nil, err
	}
	return clientEnvelopeToStruct(envelope)
}

func clientEnvelopeToStruct(envelope *agentproto.ClientEnvelope) (*structpb.Struct, error) {
	if envelope == nil {
		return nil, fmt.Errorf("agent envelope is required")
	}
	typeName := envelope.GetType()
	if typeName == "" {
		switch {
		case envelope.GetHeartbeat() != nil:
			typeName = "heartbeat"
		case envelope.GetTaskAck() != nil:
			typeName = "task_ack"
		case envelope.GetResultBatch() != nil:
			typeName = "result_batch"
		case envelope.GetCredentialRotationAck() != nil:
			typeName = "credential_rotation_ack"
		default:
			typeName = "hello"
		}
	}
	values := map[string]any{"type": typeName}
	switch typeName {
	case "hello":
		values["protocolVersion"] = envelope.GetProtocolVersion()
		values["features"] = stringSliceToAny(envelope.GetFeatures())
	case "heartbeat":
		heartbeat := envelope.GetHeartbeat()
		if heartbeat == nil {
			return nil, fmt.Errorf("heartbeat payload is required")
		}
		values["observedAt"] = heartbeat.GetObservedAt()
		values["queueDepth"] = heartbeat.GetQueueDepth()
		values["outboxBytes"] = heartbeat.GetOutboxBytes()
	case "task_ack":
		ack := envelope.GetTaskAck()
		if ack == nil {
			return nil, fmt.Errorf("task acknowledgement payload is required")
		}
		values["taskId"] = ack.GetTaskId()
		values["observedAt"] = ack.GetObservedAt()
	case "result_batch":
		batch := envelope.GetResultBatch()
		if batch == nil {
			return nil, fmt.Errorf("result batch payload is required")
		}
		values["cursor"] = batch.GetCursor()
		results := make([]any, 0, len(batch.GetResults()))
		for _, result := range batch.GetResults() {
			results = append(results, map[string]any{
				"taskId": result.GetTaskId(), "jobId": result.GetJobId(), "candidateId": result.GetCandidateId(), "routeTargetId": result.GetRouteTargetId(),
				"mode": result.GetMode(), "success": result.GetSuccess(), "errorCode": result.GetErrorCode(), "tcpMs": result.GetTcpMs(), "tlsMs": result.GetTlsMs(),
				"ttfbMs": result.GetTtfbMs(), "totalMs": result.GetTotalMs(), "bytesRead": result.GetBytesRead(), "downloadBps": result.GetDownloadBps(),
				"statusCode": result.GetStatusCode(), "observedAt": result.GetObservedAt(),
			})
		}
		values["results"] = results
	case "credential_rotation_ack":
		ack := envelope.GetCredentialRotationAck()
		if ack == nil {
			return nil, fmt.Errorf("credential rotation acknowledgement payload is required")
		}
		values["rotationId"] = ack.GetRotationId()
	default:
		return nil, fmt.Errorf("unknown typed agent message type %q", typeName)
	}
	return structpb.NewStruct(values)
}

func serverEnvelopeFromStruct(message *structpb.Struct) (*agentproto.ServerEnvelope, error) {
	if message == nil {
		return nil, fmt.Errorf("server envelope is required")
	}
	typeName := fieldString(message, "type")
	envelope := &agentproto.ServerEnvelope{
		Type:            typeName,
		ProtocolVersion: fieldString(message, "protocolVersion"),
		EnabledFeatures: fieldStrings(message, "enabledFeatures"),
		NodeId:          fieldString(message, "nodeId"),
		ServerTime:      fieldString(message, "serverTime"),
	}
	switch typeName {
	case "connected", "hello_ack":
		return envelope, nil
	case "task":
		envelope.Task = &agentproto.ProbeTask{
			TaskId: fieldString(message, "taskId"), JobId: fieldString(message, "jobId"), CandidateId: fieldString(message, "candidateId"), RouteTargetId: fieldString(message, "routeTargetId"),
			Mode: fieldString(message, "mode"), Address: fieldString(message, "address"), Port: int32(fieldNumber(message, "port")), Url: fieldString(message, "url"),
			Host: fieldString(message, "host"), Sni: fieldString(message, "sni"), Method: fieldString(message, "method"), Headers: stringMapField(message, "headers"),
			ExpectedStatuses: int32SliceField(message, "expectedStatuses"), ExpectedContains: fieldString(message, "expectedContains"), TimeoutMs: int32(fieldNumber(message, "timeoutMs")),
			MaxBytes: int64(fieldNumber(message, "maxBytes")), Attempt: int32(fieldNumber(message, "attempt")), LeaseSeconds: int32(fieldNumber(message, "leaseSeconds")),
			AllowedPorts: int32SliceField(message, "allowedPorts"),
		}
	case "result_ack":
		envelope.ResultAck = &agentproto.ResultAck{Cursor: int64(fieldNumber(message, "cursor")), JobIds: fieldStrings(message, "jobIds")}
	case "credential_rotation":
		envelope.CredentialRotation = &agentproto.CredentialRotation{RotationId: fieldString(message, "rotationId"), Token: fieldString(message, "token"), ExpiresAt: fieldString(message, "expiresAt")}
	case "credential_rotation_committed":
		envelope.RotationCommittedId = fieldString(message, "rotationId")
	case "credential_rotation_cancelled":
		envelope.RotationCancelledId = fieldString(message, "rotationId")
	default:
		return nil, fmt.Errorf("unknown server message type %q", typeName)
	}
	return envelope, nil
}

func stringSliceToAny(values []string) []any {
	result := make([]any, len(values))
	for index, value := range values {
		result[index] = value
	}
	return result
}

func int32SliceField(message *structpb.Struct, name string) []int32 {
	field := message.GetFields()[name]
	if field == nil || field.GetListValue() == nil {
		return nil
	}
	result := make([]int32, 0, len(field.GetListValue().GetValues()))
	for _, value := range field.GetListValue().GetValues() {
		result = append(result, int32(value.GetNumberValue()))
	}
	return result
}

func stringMapField(message *structpb.Struct, name string) map[string]string {
	field := message.GetFields()[name]
	if field == nil || field.GetStructValue() == nil {
		return nil
	}
	result := make(map[string]string, len(field.GetStructValue().GetFields()))
	for key, value := range field.GetStructValue().GetFields() {
		result[key] = value.GetStringValue()
	}
	return result
}
