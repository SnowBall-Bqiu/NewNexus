package agentproto

import (
	"context"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/structpb"
)

const ServiceName = "agent.v1.AgentService"

type Server interface {
	Enroll(context.Context, *structpb.Struct) (*structpb.Struct, error)
	Connect(ConnectServer) error
}

type ConnectServer interface {
	Send(*structpb.Struct) error
	Recv() (*structpb.Struct, error)
	grpc.ServerStream
}

type connectServer struct{ grpc.ServerStream }

func (s *connectServer) Send(message *structpb.Struct) error { return s.ServerStream.SendMsg(message) }
func (s *connectServer) Recv() (*structpb.Struct, error) {
	message := new(structpb.Struct)
	if err := s.ServerStream.RecvMsg(message); err != nil {
		return nil, err
	}
	return message, nil
}

func RegisterServer(server grpc.ServiceRegistrar, implementation Server) {
	server.RegisterService(&grpc.ServiceDesc{
		ServiceName: ServiceName,
		HandlerType: (*Server)(nil),
		Methods:     []grpc.MethodDesc{{MethodName: "Enroll", Handler: enrollHandler}},
		Streams:     []grpc.StreamDesc{{StreamName: "Connect", Handler: connectHandler, ServerStreams: true, ClientStreams: true}},
		Metadata:    "agent/v1/agent.proto",
	}, implementation)
}

func enrollHandler(srv any, ctx context.Context, decoder func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	request := new(structpb.Struct)
	if err := decoder(request); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(Server).Enroll(ctx, request)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/" + ServiceName + "/Enroll"}
	handler := func(ctx context.Context, req any) (any, error) {
		return srv.(Server).Enroll(ctx, req.(*structpb.Struct))
	}
	return interceptor(ctx, request, info, handler)
}

func connectHandler(srv any, stream grpc.ServerStream) error {
	return srv.(Server).Connect(&connectServer{ServerStream: stream})
}

type Client struct{ connection grpc.ClientConnInterface }

func NewClient(connection grpc.ClientConnInterface) *Client { return &Client{connection: connection} }
func (c *Client) Enroll(ctx context.Context, request *structpb.Struct, options ...grpc.CallOption) (*structpb.Struct, error) {
	response := new(structpb.Struct)
	if err := c.connection.Invoke(ctx, "/"+ServiceName+"/Enroll", request, response, options...); err != nil {
		return nil, err
	}
	return response, nil
}
func (c *Client) Connect(ctx context.Context, options ...grpc.CallOption) (ConnectClient, error) {
	descriptor := &grpc.StreamDesc{StreamName: "Connect", ServerStreams: true, ClientStreams: true}
	stream, err := c.connection.NewStream(ctx, descriptor, "/"+ServiceName+"/Connect", options...)
	if err != nil {
		return nil, err
	}
	return &connectClient{ClientStream: stream}, nil
}

type ConnectClient interface {
	Send(*structpb.Struct) error
	Recv() (*structpb.Struct, error)
	CloseSend() error
	grpc.ClientStream
}
type connectClient struct{ grpc.ClientStream }

func (c *connectClient) Send(message *structpb.Struct) error { return c.ClientStream.SendMsg(message) }
func (c *connectClient) Recv() (*structpb.Struct, error) {
	message := new(structpb.Struct)
	if err := c.ClientStream.RecvMsg(message); err != nil {
		return nil, err
	}
	return message, nil
}
