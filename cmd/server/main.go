package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/SnowBall-Bqiu/NewNexus/internal/agenthub"
	"github.com/SnowBall-Bqiu/NewNexus/internal/agentproto"
	"github.com/SnowBall-Bqiu/NewNexus/internal/api"
	"github.com/SnowBall-Bqiu/NewNexus/internal/auth"
	"github.com/SnowBall-Bqiu/NewNexus/internal/config"
	"github.com/SnowBall-Bqiu/NewNexus/internal/events"
	"github.com/SnowBall-Bqiu/NewNexus/internal/optimizer"
	"github.com/SnowBall-Bqiu/NewNexus/internal/provider"
	"github.com/SnowBall-Bqiu/NewNexus/internal/routing"
	"github.com/SnowBall-Bqiu/NewNexus/internal/scheduler"
	"github.com/SnowBall-Bqiu/NewNexus/internal/security"
	"github.com/SnowBall-Bqiu/NewNexus/internal/store"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/reflection"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)
	cfg, err := config.Load()
	if err != nil {
		logger.Error("configuration failed", "error", err)
		os.Exit(1)
	}
	database, err := store.Open(cfg)
	if err != nil {
		logger.Error("database failed", "error", err)
		os.Exit(1)
	}
	defer database.Close()
	vault, err := security.NewVault(cfg.MasterKey)
	if err != nil {
		logger.Error("vault failed", "error", err)
		os.Exit(1)
	}
	broker := events.NewBroker()
	registry := provider.NewRegistry(provider.NewCloudflare(), provider.NewDNSPod(), provider.NewAliDNS())
	hub := agenthub.New(database, broker)
	optimizerService := optimizer.New(database, broker)
	routingService := routing.New(database, vault, registry, broker)
	authManager := auth.NewManager(database, vault, cfg)
	httpAPI := api.New(cfg, database, vault, authManager, registry, routingService, optimizerService, hub, broker)
	httpServer := &http.Server{Addr: cfg.HTTPAddress, Handler: httpAPI.Router(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 0, IdleTimeout: 120 * time.Second}
	grpcListener, err := net.Listen("tcp", cfg.GRPCAddress)
	if err != nil {
		logger.Error("gRPC listen failed", "error", err)
		os.Exit(1)
	}
	grpcServer := grpc.NewServer(grpc.KeepaliveParams(keepalive.ServerParameters{Time: 30 * time.Second, Timeout: 10 * time.Second}), grpc.MaxRecvMsgSize(4<<20), grpc.MaxSendMsgSize(4<<20))
	agentproto.RegisterServer(grpcServer, hub)
	reflection.Register(grpcServer)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if cfg.SchedulerEnabled {
		go scheduler.New(database, hub, optimizerService, routingService, cfg.ProbeInterval).Run(ctx)
	}
	go func() {
		logger.Info("gRPC server listening", "address", cfg.GRPCAddress)
		if err := grpcServer.Serve(grpcListener); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			logger.Error("gRPC server stopped", "error", err)
			stop()
		}
	}()
	go func() {
		logger.Info("HTTP server listening", "address", cfg.HTTPAddress, "database", cfg.DatabaseDriver, "demo", cfg.Demo)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("HTTP server stopped", "error", err)
			stop()
		}
	}()
	<-ctx.Done()
	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutdownCtx)
	grpcServer.GracefulStop()
}
