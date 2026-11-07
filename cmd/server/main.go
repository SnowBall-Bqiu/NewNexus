package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"github.com/newnexus/newnexus/internal/agenthub"
	"github.com/newnexus/newnexus/internal/agentproto"
	"github.com/newnexus/newnexus/internal/api"
	"github.com/newnexus/newnexus/internal/auth"
	"github.com/newnexus/newnexus/internal/control"
	"github.com/newnexus/newnexus/internal/domain"
	"github.com/newnexus/newnexus/internal/security"
	"github.com/newnexus/newnexus/internal/store"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
func production() bool {
	return strings.EqualFold(strings.TrimSpace(env("NEWNEXUS_ENV", "development")), "production")
}
func loopback(addr string) bool {
	h, _, e := net.SplitHostPort(addr)
	if e != nil {
		return false
	}
	ip := net.ParseIP(h)
	return h == "localhost" || ip != nil && ip.IsLoopback()
}

func agentPublicAddress(configured, listening string) (string, error) {
	if strings.TrimSpace(configured) != "" {
		return api.NormalizeAgentEndpoint(configured)
	}
	host, _, err := net.SplitHostPort(listening)
	if err == nil {
		if ip := net.ParseIP(host); host == "" || ip != nil && ip.IsUnspecified() {
			return "", nil
		}
	}
	return api.NormalizeAgentEndpoint(listening)
}

func normalizeAgentDownloadOrigin(configured string) (string, error) {
	return api.NormalizeHTTPOrigin(configured)
}

// Plaintext is safe to advertise only for the exact bound loopback endpoint.
// Hostnames (including localhost) can resolve to a different IP or TLS proxy.
func agentUsesLoopbackPlaintext(environment, cert, advertised, listening string, allowed bool) bool {
	if strings.EqualFold(strings.TrimSpace(environment), "production") || cert != "" || !allowed {
		return false
	}
	public, publicErr := netip.ParseAddrPort(advertised)
	bound, boundErr := netip.ParseAddrPort(listening)
	return publicErr == nil && boundErr == nil && public.Addr().IsLoopback() && bound.Addr().IsLoopback() && public.Port() != 0 && public.Port() == bound.Port() && public.Addr().Unmap() == bound.Addr().Unmap()
}

func agentAllowsLoopbackPlaintext(environment, cert, listening string, allowed bool) bool {
	if !strings.EqualFold(strings.TrimSpace(environment), "development") || cert != "" || !allowed {
		return false
	}
	bound, err := netip.ParseAddrPort(listening)
	return err == nil && bound.Addr().IsLoopback()
}

func validateRuntime(environment, httpAddr, grpcAddr, cert, key string, devAuth, secureCookie, insecureAgent bool) error {
	return validateRuntimeWithAgentTLS(environment, httpAddr, grpcAddr, cert, key, devAuth, secureCookie, insecureAgent, false)
}

func validateRuntimeWithAgentTLS(environment, httpAddr, grpcAddr, cert, key string, devAuth, secureCookie, insecureAgent, managedAgentTLS bool) error {
	production := strings.EqualFold(strings.TrimSpace(environment), "production")
	if (cert == "") != (key == "") {
		return errors.New("configure both NEWNEXUS_TLS_CERT and NEWNEXUS_TLS_KEY")
	}
	if production && cert == "" {
		return errors.New("production REST and gRPC require NEWNEXUS_TLS_CERT and NEWNEXUS_TLS_KEY")
	}
	if devAuth && (production || !loopback(httpAddr)) {
		return errors.New("development authentication bypass requires a loopback listener and non-production environment")
	}
	if !secureCookie && (production || !loopback(httpAddr)) {
		return errors.New("insecure cookies are only allowed on development loopback listeners")
	}
	if cert == "" && !managedAgentTLS && (production || !loopback(grpcAddr) || !insecureAgent) {
		return errors.New("Agent gRPC requires TLS; only explicit loopback development permits NEWNEXUS_ALLOW_INSECURE_AGENT=1")
	}
	return nil
}
func loadKey(dataDir string) ([]byte, error) {
	value := os.Getenv("NEWNEXUS_MASTER_KEY")
	if value != "" {
		if b, e := base64.StdEncoding.DecodeString(value); e == nil && len(b) == 32 {
			return b, nil
		}
		if b, e := hex.DecodeString(value); e == nil && len(b) == 32 {
			return b, nil
		}
		return nil, errors.New("NEWNEXUS_MASTER_KEY must be base64 or hex encoding of exactly 32 bytes")
	}
	if production() {
		return nil, errors.New("NEWNEXUS_MASTER_KEY is required in production")
	}
	path := filepath.Join(dataDir, "master.key")
	if b, e := os.ReadFile(path); e == nil {
		if len(b) != 32 {
			return nil, errors.New("invalid local master key")
		}
		return b, nil
	}
	b := make([]byte, 32)
	if _, e := rand.Read(b); e != nil {
		return nil, e
	}
	file, e := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return nil, e
	}
	if _, e = file.Write(b); e != nil {
		file.Close()
		return nil, e
	}
	return b, file.Close()
}
func run() error {
	addr := flag.String("listen", env("NEWNEXUS_HTTP_ADDR", "127.0.0.1:8080"), "REST HTTP listen address")
	grpcAddr := flag.String("grpc-listen", env("NEWNEXUS_GRPC_ADDR", "127.0.0.1:9090"), "Agent gRPC listen address")
	dataDir := flag.String("data-dir", env("NEWNEXUS_DATA_DIR", "data"), "data directory")
	webDir := flag.String("web-dir", env("NEWNEXUS_WEB_DIR", "web/dist"), "built frontend directory")
	agentDownloadDir := flag.String("agent-download-dir", os.Getenv("NEWNEXUS_AGENT_DOWNLOAD_DIR"), "directory containing distributable Linux agent binaries (default web-dir/agent)")
	agentDownloadOrigin := flag.String("agent-download-origin", os.Getenv("NEWNEXUS_AGENT_DOWNLOAD_ORIGIN"), "HTTP(S) origin reachable by nodes for downloading agent binaries")
	consoleOriginFlag := flag.String("console-origin", os.Getenv("NEWNEXUS_CONSOLE_ORIGIN"), "public HTTP(S) console origin advertised by installation metadata")
	agentPublicAddr := flag.String("agent-public-addr", os.Getenv("NEWNEXUS_AGENT_PUBLIC_ADDR"), "public Agent gRPC host:port advertised to the console")
	migrateOnly := flag.Bool("migrate", false, "apply database migrations then exit")
	healthOnly := flag.Bool("healthcheck", false, "check the configured local HTTP service and exit")
	flag.Parse()
	if *healthOnly {
		return healthcheck(*addr)
	}
	downloadOrigin, e := normalizeAgentDownloadOrigin(*agentDownloadOrigin)
	if e != nil {
		return e
	}
	consoleOrigin, e := api.NormalizeHTTPOrigin(*consoleOriginFlag)
	if e != nil {
		return e
	}
	if e := os.MkdirAll(*dataDir, 0700); e != nil {
		return e
	}
	key, e := loadKey(*dataDir)
	if e != nil {
		return e
	}
	vault, e := security.NewVault(key)
	if e != nil {
		return e
	}
	dsn := env("NEWNEXUS_DATABASE_URL", filepath.Join(*dataDir, "newnexus.db"))
	db, e := store.Open(dsn)
	if e != nil {
		return fmt.Errorf("open database: %w", e)
	}
	defer db.DB.Close()
	if *migrateOnly {
		return nil
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if db.Driver == "sqlite" {
		owner := "instance-" + domain.ID()
		ok, e := db.Lease(ctx, "sqlite-instance", owner, 20*time.Second)
		if e != nil {
			return e
		}
		if !ok {
			return errors.New("SQLite permits exactly one running control-plane instance")
		}
		defer db.ReleaseLease(context.Background(), "sqlite-instance", owner)
		go func() {
			ticker := time.NewTicker(5 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					ok, e := db.Lease(ctx, "sqlite-instance", owner, 20*time.Second)
					if e != nil || !ok {
						slog.Error("SQLite instance lock lost")
						stop()
						return
					}
				}
			}
		}()
	}
	hub := agenthub.New(db)
	server := api.New(db, vault, hub, control.New(db, vault))
	server.WebDir = *webDir
	server.AgentDownloadOrigin = downloadOrigin
	server.ConsoleOrigin = consoleOrigin
	server.AgentDownloadDir = *agentDownloadDir
	if server.AgentDownloadDir == "" {
		server.AgentDownloadDir = filepath.Join(*webDir, "agent")
	}
	cert := os.Getenv("NEWNEXUS_TLS_CERT")
	keyFile := os.Getenv("NEWNEXUS_TLS_KEY")
	devAuth := os.Getenv("NEWNEXUS_DEV_AUTH") == "1"
	server.DevAuth = devAuth
	server.Auth.SecureCookie = env("NEWNEXUS_COOKIE_SECURE", "1") == "1"
	if e = server.EnsureDefaults(ctx); e != nil {
		return e
	}
	managedAgentTLS, e := server.AgentTLS.Enabled(ctx)
	if e != nil {
		return e
	}
	if e = validateRuntimeWithAgentTLS(env("NEWNEXUS_ENV", "development"), *addr, *grpcAddr, cert, keyFile, devAuth, server.Auth.SecureCookie, os.Getenv("NEWNEXUS_ALLOW_INSECURE_AGENT") == "1", managedAgentTLS); e != nil {
		return e
	}
	if managedAgentTLS {
		if _, e = server.AgentTLS.Ensure(ctx); e != nil {
			return e
		}
	}
	if production() && !server.Auth.Initialized(ctx) {
		setupToken, err := loadSetupToken(*dataDir)
		if err != nil {
			return err
		}
		server.SetupTokenHash = auth.Hash(setupToken)
	}
	grpcOptions := []grpc.ServerOption{grpc.MaxRecvMsgSize(4 << 20), grpc.MaxSendMsgSize(4 << 20), grpc.MaxConcurrentStreams(256), grpc.KeepaliveParams(keepalive.ServerParameters{Time: 30 * time.Second, Timeout: 10 * time.Second}), grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: 10 * time.Second, PermitWithoutStream: true})}
	var fallbackTLS *tls.Config
	if cert != "" {
		tlsCert, e := tls.LoadX509KeyPair(cert, keyFile)
		if e != nil {
			return e
		}
		fallbackTLS = &tls.Config{Certificates: []tls.Certificate{tlsCert}, MinVersion: tls.VersionTLS12, NextProtos: []string{"h2"}}
	}
	listener, e := net.Listen("tcp", *grpcAddr)
	if e != nil {
		return e
	}
	server.AgentPublicAddr, e = agentPublicAddress(*agentPublicAddr, listener.Addr().String())
	if e != nil {
		listener.Close()
		return e
	}
	allowPlaintext := agentAllowsLoopbackPlaintext(env("NEWNEXUS_ENV", "development"), cert, listener.Addr().String(), os.Getenv("NEWNEXUS_ALLOW_INSECURE_AGENT") == "1")
	server.AgentInsecureLoopback = allowPlaintext && agentUsesLoopbackPlaintext(env("NEWNEXUS_ENV", "development"), cert, server.AgentPublicAddr, listener.Addr().String(), os.Getenv("NEWNEXUS_ALLOW_INSECURE_AGENT") == "1")
	grpcOptions = append(grpcOptions, grpc.Creds(server.AgentTLS.Credentials(fallbackTLS, allowPlaintext)))
	grpcServer := grpc.NewServer(grpcOptions...)
	agentproto.RegisterAgentServiceServer(grpcServer, hub)
	httpServer := &http.Server{Addr: *addr, Handler: server, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 1 << 20}
	errs := make(chan error, 2)
	go func() {
		slog.Info("agent gateway listening", "address", *grpcAddr, "tls", cert != "" || managedAgentTLS, "managed_tls", managedAgentTLS)
		errs <- grpcServer.Serve(listener)
	}()
	go func() {
		slog.Info("control plane listening", "address", *addr, "database", db.Driver, "dev_auth", devAuth)
		if cert != "" {
			errs <- httpServer.ListenAndServeTLS(cert, keyFile)
		} else {
			errs <- httpServer.ListenAndServe()
		}
	}()
	server.StartWorkers(ctx)
	select {
	case <-ctx.Done():
	case e = <-errs:
		if !errors.Is(e, http.ErrServerClosed) {
			stop()
		}
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutdown)
	done := make(chan struct{})
	go func() { grpcServer.GracefulStop(); close(done) }()
	select {
	case <-done:
	case <-shutdown.Done():
		grpcServer.Stop()
	}
	return e
}
func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if e := run(); e != nil && !errors.Is(e, http.ErrServerClosed) && !strings.Contains(e.Error(), "context canceled") {
		slog.Error("server stopped", "error", e)
		os.Exit(1)
	}
}
