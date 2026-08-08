package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
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
	"github.com/SnowBall-Bqiu/NewNexus/internal/agentproto"
	"github.com/SnowBall-Bqiu/NewNexus/internal/probe"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/structpb"
)

var version = "dev"

type state struct {
	NodeID string `json:"nodeId"`
	Token  string `json:"token"`
}

func main() {
	server := flag.String("server", env("NEWNEXUS_AGENT_SERVER", "localhost:9090"), "gRPC control-plane address")
	enrollCode := flag.String("enroll-code", os.Getenv("NEWNEXUS_AGENT_ENROLL_CODE"), "one-time enrollment code")
	name := flag.String("name", env("NEWNEXUS_AGENT_NAME", hostname()), "node name")
	protocols := flag.String("protocols", env("NEWNEXUS_AGENT_PROTOCOLS", "tcp,http"), "comma-separated probe protocols")
	statePath := flag.String("state", env("NEWNEXUS_AGENT_STATE", "agent-state.json"), "agent state path")
	insecureTransport := flag.Bool("insecure", env("NEWNEXUS_AGENT_INSECURE", "false") == "true", "disable transport TLS")
	serverName := flag.String("tls-server-name", os.Getenv("NEWNEXUS_AGENT_TLS_SERVER_NAME"), "TLS server name override")
	concurrency := flag.Int("concurrency", 16, "maximum concurrent probes")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	backoff := time.Second
	for ctx.Err() == nil {
		if err := run(ctx, *server, *enrollCode, *name, *protocols, *statePath, *serverName, *insecureTransport, *concurrency); err != nil && ctx.Err() == nil {
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

func run(ctx context.Context, server, enrollCode, name, protocols, statePath, serverName string, allowInsecure bool, concurrency int) error {
	var transport credentials.TransportCredentials
	if allowInsecure {
		transport = insecure.NewCredentials()
	} else {
		transport = credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12, ServerName: serverName})
	}
	connection, err := grpc.NewClient(server, grpc.WithTransportCredentials(transport), grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(4<<20), grpc.MaxCallSendMsgSize(4<<20)))
	if err != nil {
		return err
	}
	defer connection.Close()
	client := agentproto.NewClient(connection)
	var current state
	if err := probe.LoadState(statePath, &current); err != nil || current.Token == "" {
		if enrollCode == "" {
			return fmt.Errorf("agent is not enrolled; provide --enroll-code")
		}
		request, _ := structpb.NewStruct(map[string]any{"code": enrollCode, "name": name, "protocols": protocols, "version": version, "platform": runtime.GOOS + "/" + runtime.GOARCH})
		response, err := client.Enroll(ctx, request)
		if err != nil {
			return fmt.Errorf("enroll: %w", err)
		}
		current = state{NodeID: response.Fields["nodeId"].GetStringValue(), Token: response.Fields["token"].GetStringValue()}
		if err := probe.SaveState(statePath, current); err != nil {
			return fmt.Errorf("save enrollment: %w", err)
		}
		log.Printf("enrolled node %s", current.NodeID)
	}
	streamCtx := metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+current.Token, "x-agent-version", version)
	stream, err := client.Connect(streamCtx)
	if err != nil {
		return err
	}
	defer stream.CloseSend()
	var sendMu sync.Mutex
	send := func(message map[string]any) error {
		payload, _ := structpb.NewStruct(message)
		sendMu.Lock()
		defer sendMu.Unlock()
		return stream.Send(payload)
	}
	heartbeatDone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-heartbeatDone:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				_ = send(map[string]any{"type": "heartbeat", "at": time.Now().UTC().Format(time.RFC3339Nano)})
			}
		}
	}()
	defer close(heartbeatDone)
	semaphore := make(chan struct{}, max(1, concurrency))
	var workers sync.WaitGroup
	defer workers.Wait()
	for {
		message, err := stream.Recv()
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if message.Fields["type"].GetStringValue() != "task" {
			continue
		}
		raw, _ := json.Marshal(message.AsMap())
		var task agenthub.Task
		if err := json.Unmarshal(raw, &task); err != nil {
			continue
		}
		semaphore <- struct{}{}
		workers.Add(1)
		go func() {
			defer func() { <-semaphore; workers.Done() }()
			result := probe.Run(ctx, task)
			payload, _ := json.Marshal(result)
			response := map[string]any{}
			_ = json.Unmarshal(payload, &response)
			response["type"] = "result"
			if err := send(response); err != nil {
				log.Printf("send probe result: %v", err)
			}
		}()
	}
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
