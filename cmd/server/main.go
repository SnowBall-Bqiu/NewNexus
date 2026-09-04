package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
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
	"github.com/SnowBall-Bqiu/NewNexus/internal/jobs"
	"github.com/SnowBall-Bqiu/NewNexus/internal/optimizer"
	"github.com/SnowBall-Bqiu/NewNexus/internal/provider"
	"github.com/SnowBall-Bqiu/NewNexus/internal/routing"
	"github.com/SnowBall-Bqiu/NewNexus/internal/scheduler"
	"github.com/SnowBall-Bqiu/NewNexus/internal/security"
	"github.com/SnowBall-Bqiu/NewNexus/internal/store"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
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
	var grpcCertificate *tls.Certificate
	if cfg.GRPCTLSCertFile != "" {
		certificate, loadErr := tls.LoadX509KeyPair(cfg.GRPCTLSCertFile, cfg.GRPCTLSKeyFile)
		if loadErr != nil {
			logger.Error("gRPC TLS configuration failed", "error", loadErr)
			os.Exit(1)
		}
		grpcCertificate = &certificate
		if cfg.AgentTLSFingerprint == "" && len(certificate.Certificate) > 0 {
			digest := sha256.Sum256(certificate.Certificate[0])
			cfg.AgentTLSFingerprint = hex.EncodeToString(digest[:])
		}
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
	jobManager := jobs.New(database, broker)
	registry := provider.NewRegistry(provider.NewCloudflare(), provider.NewDNSPod(), provider.NewAliDNS())
	hub := agenthub.New(database, broker)
	optimizerService := optimizer.New(database, broker)
	routingService := routing.New(database, vault, registry, broker)
	authManager := auth.NewManager(database, vault, cfg)
	httpAPI := api.New(cfg, database, vault, authManager, registry, routingService, optimizerService, hub, broker, jobManager)
	httpServer := &http.Server{Addr: cfg.HTTPAddress, Handler: httpAPI.Router(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 0, IdleTimeout: 120 * time.Second}
	grpcListener, err := net.Listen("tcp", cfg.GRPCAddress)
	if err != nil {
		logger.Error("gRPC listen failed", "error", err)
		os.Exit(1)
	}
	grpcOptions := []grpc.ServerOption{
		grpc.KeepaliveParams(keepalive.ServerParameters{Time: 30 * time.Second, Timeout: 10 * time.Second}),
		grpc.MaxRecvMsgSize(4 << 20),
		grpc.MaxSendMsgSize(4 << 20),
	}
	if grpcCertificate != nil {
		grpcOptions = append(grpcOptions, grpc.Creds(credentials.NewTLS(&tls.Config{
			Certificates: []tls.Certificate{*grpcCertificate},
			MinVersion:   tls.VersionTLS12,
		})))
	} else {
		logger.Warn("gRPC h2c is enabled; bind this listener only behind a private reverse proxy", "address", cfg.GRPCAddress)
	}
	grpcServer := grpc.NewServer(grpcOptions...)
	agentproto.RegisterServer(grpcServer, hub)
	agentproto.RegisterAgentServiceV1Server(grpcServer, agenthub.NewV1Server(hub))
	reflection.Register(grpcServer)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if database.Driver == "sqlite" {
		leaseTTL := 30 * time.Second
		if cfg.DevMode {
			leaseTTL = 3 * time.Second
		}
		if err := database.AcquireLease("control-plane", cfg.InstanceID, leaseTTL); err != nil {
			logger.Error("another SQLite control-plane instance is active", "error", err)
			os.Exit(1)
		}
		defer database.ReleaseLease("control-plane", cfg.InstanceID)
		go renewInstanceLease(ctx, database, cfg.InstanceID, leaseTTL, stop)
	}
	go jobManager.Run(ctx, cfg.JobWorkers)
	if cfg.SchedulerEnabled {
		go scheduler.NewWithJobs(database, hub, optimizerService, routingService, jobManager, cfg.ProbeInterval, cfg.InventoryInterval, cfg.RankingInterval, cfg.ActiveInterval).Run(ctx)
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

func renewInstanceLease(ctx context.Context, database *store.Store, owner string, ttl time.Duration, stop context.CancelFunc) {
	ticker := time.NewTicker(ttl / 3)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := database.RenewLease("control-plane", owner, ttl); err != nil {
				slog.Error("SQLite instance lease lost", "error", err)
				stop()
				return
			}
		}
	}
}
