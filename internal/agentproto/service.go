// Package agentproto defines the agent.v1 wire contract without a local protoc dependency.
package agentproto

import (
	"context"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"
)

type AgentServiceServer interface {
	Enroll(context.Context, *structpb.Struct) (*structpb.Struct, error)
	Connect(AgentService_ConnectServer) error
}
type AgentService_ConnectServer interface {
	Send(*structpb.Struct) error
	Recv() (*structpb.Struct, error)
	grpc.ServerStream
}
type connectServer struct{ grpc.ServerStream }

func (s *connectServer) Send(m *structpb.Struct) error { return s.ServerStream.SendMsg(m) }
func (s *connectServer) Recv() (*structpb.Struct, error) {
	m := new(structpb.Struct)
	if err := s.ServerStream.RecvMsg(m); err != nil {
		return nil, err
	}
	return m, nil
}
func RegisterAgentServiceServer(s grpc.ServiceRegistrar, impl AgentServiceServer) {
	s.RegisterService(&AgentService_ServiceDesc, impl)
}

var AgentService_ServiceDesc = grpc.ServiceDesc{ServiceName: "agent.v1.AgentService", HandlerType: (*AgentServiceServer)(nil), Methods: []grpc.MethodDesc{{MethodName: "Enroll", Handler: func(s interface{}, ctx context.Context, dec func(interface{}) error, i grpc.UnaryServerInterceptor) (interface{}, error) {
	m := new(structpb.Struct)
	if err := dec(m); err != nil {
		return nil, err
	}
	if i == nil {
		return s.(AgentServiceServer).Enroll(ctx, m)
	}
	return i(ctx, m, &grpc.UnaryServerInfo{Server: s, FullMethod: "/agent.v1.AgentService/Enroll"}, func(ctx context.Context, r interface{}) (interface{}, error) {
		return s.(AgentServiceServer).Enroll(ctx, r.(*structpb.Struct))
	})
}}}, Streams: []grpc.StreamDesc{{StreamName: "Connect", Handler: func(s interface{}, stream grpc.ServerStream) error {
	return s.(AgentServiceServer).Connect(&connectServer{stream})
}, ServerStreams: true, ClientStreams: true}}, Metadata: "proto/agent/v1/agent.proto"}

type AgentServiceClient interface {
	Enroll(context.Context, *structpb.Struct, ...grpc.CallOption) (*structpb.Struct, error)
	Connect(context.Context, ...grpc.CallOption) (AgentService_ConnectClient, error)
}
type agentClient struct{ c grpc.ClientConnInterface }

func NewAgentServiceClient(c grpc.ClientConnInterface) AgentServiceClient { return &agentClient{c} }
func (c *agentClient) Enroll(ctx context.Context, m *structpb.Struct, o ...grpc.CallOption) (*structpb.Struct, error) {
	out := new(structpb.Struct)
	err := c.c.Invoke(ctx, "/agent.v1.AgentService/Enroll", m, out, o...)
	return out, err
}
func (c *agentClient) Connect(ctx context.Context, o ...grpc.CallOption) (AgentService_ConnectClient, error) {
	s, e := c.c.NewStream(ctx, &AgentService_ServiceDesc.Streams[0], "/agent.v1.AgentService/Connect", o...)
	if e != nil {
		return nil, e
	}
	return &connectClient{s}, nil
}

type AgentService_ConnectClient interface {
	Send(*structpb.Struct) error
	Recv() (*structpb.Struct, error)
	grpc.ClientStream
}
type connectClient struct{ grpc.ClientStream }

func (c *connectClient) Send(m *structpb.Struct) error { return c.ClientStream.SendMsg(m) }
func (c *connectClient) Recv() (*structpb.Struct, error) {
	m := new(structpb.Struct)
	if err := c.ClientStream.RecvMsg(m); err != nil {
		return nil, err
	}
	return m, nil
}
