package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/newnexus/newnexus/internal/agentclient"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		slog.Error("agent stopped", "error", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	flags := flag.NewFlagSet("newnexus-agent", flag.ContinueOnError)
	var cfg agentclient.Config
	flags.StringVar(&cfg.Server, "server", env("NEWNEXUS_AGENT_SERVER", ""), "Control plane gRPC host:port (TLS required)")
	flags.StringVar(&cfg.EnrollmentToken, "enrollment-token", env("NEWNEXUS_AGENT_ENROLLMENT_TOKEN", ""), "One-time enrollment token; prefer environment over command line")
	flags.StringVar(&cfg.Name, "name", env("NEWNEXUS_AGENT_NAME", hostname()), "Node display name")
	flags.StringVar(&cfg.Region, "region", env("NEWNEXUS_AGENT_REGION", ""), "Node region")
	flags.StringVar(&cfg.Carrier, "carrier", env("NEWNEXUS_AGENT_CARRIER", ""), "Node carrier")
	flags.StringVar(&cfg.Mode, "mode", env("NEWNEXUS_AGENT_MODE", "all"), "Probe node mode: all, tcp, or http")
	flags.StringVar(&cfg.DataDir, "data-dir", env("NEWNEXUS_AGENT_DATA_DIR", "./agent-data"), "Persistent private queue and credentials directory")
	flags.StringVar(&cfg.CAFile, "ca-file", env("NEWNEXUS_AGENT_CA_FILE", ""), "Additional PEM trust roots for the control plane")
	flags.StringVar(&cfg.ClientCertFile, "client-cert", env("NEWNEXUS_AGENT_CLIENT_CERT", ""), "Optional TLS client certificate")
	flags.StringVar(&cfg.ClientKeyFile, "client-key", env("NEWNEXUS_AGENT_CLIENT_KEY", ""), "Optional TLS client private key")
	flags.StringVar(&cfg.ServerName, "tls-server-name", env("NEWNEXUS_AGENT_TLS_SERVER_NAME", ""), "Optional TLS certificate/SNI hostname override")
	insecure, err := envBool("NEWNEXUS_AGENT_INSECURE_LOOPBACK", false)
	if err != nil {
		return err
	}
	flags.BoolVar(&cfg.InsecureLoopback, "insecure-loopback", insecure, "Development only: plaintext allowed for literal loopback server addresses")
	concurrency, err := envInt("NEWNEXUS_AGENT_CONCURRENCY", 4)
	if err != nil {
		return err
	}
	flags.IntVar(&cfg.Concurrency, "concurrency", concurrency, "Maximum concurrent probe tasks (1..128)")
	maxPending, err := envInt("NEWNEXUS_AGENT_MAX_PENDING", 10000)
	if err != nil {
		return err
	}
	flags.IntVar(&cfg.MaxPending, "max-pending", maxPending, "Maximum durable pending tasks plus unacknowledged results")
	maxOutbox, err := envInt64("NEWNEXUS_AGENT_MAX_OUTBOX_BYTES", 128<<20)
	if err != nil {
		return err
	}
	flags.Int64Var(&cfg.MaxOutboxBytes, "max-outbox-bytes", maxOutbox, "Durable queue admission byte limit; completed results are always persisted")
	rate, err := envFloat("NEWNEXUS_AGENT_RATE", 10)
	if err != nil {
		return err
	}
	flags.Float64Var(&cfg.Probe.RatePerSecond, "rate", rate, "Maximum probe samples per second across all workers")
	ports := env("NEWNEXUS_AGENT_ALLOWED_PORTS", "80,443")
	flags.StringVar(&ports, "allowed-ports", ports, "Comma-separated destination port allowlist")
	timeout, err := envDuration("NEWNEXUS_AGENT_MAX_TIMEOUT", 30*time.Second)
	if err != nil {
		return err
	}
	flags.DurationVar(&cfg.Probe.MaxTimeout, "max-timeout", timeout, "Maximum time per probe sample (1s..60s)")
	maxBytes, err := envInt64("NEWNEXUS_AGENT_MAX_RESPONSE_BYTES", 16<<20)
	if err != nil {
		return err
	}
	flags.Int64Var(&cfg.Probe.MaxBytes, "max-response-bytes", maxBytes, "Maximum download sample bytes permitted per task")
	maxSamples, err := envInt("NEWNEXUS_AGENT_MAX_SAMPLES", 10)
	if err != nil {
		return err
	}
	flags.IntVar(&cfg.Probe.MaxSamples, "max-samples", maxSamples, "Maximum probe samples permitted per task")
	flags.StringVar(&cfg.Probe.UserAgent, "user-agent", env("NEWNEXUS_AGENT_USER_AGENT", ""), "Default HTTP User-Agent override")
	var version, healthcheck bool
	flags.BoolVar(&version, "version", false, "Print version and exit")
	flags.BoolVar(&healthcheck, "healthcheck", false, "Check persisted stream heartbeat and exit without opening a listener")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments")
	}
	if version {
		fmt.Printf("NewNexus Agent %s (protocol %d)\n", agentclient.Version, agentclient.ProtocolVersion)
		return nil
	}
	if healthcheck {
		return agentclient.CheckHealth(cfg.DataDir)
	}
	if cfg.Probe.MaxTimeout > 60*time.Second {
		return fmt.Errorf("max-timeout cannot exceed 60s")
	}
	for _, value := range strings.Split(ports, ",") {
		port, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || port < 1 || port > 65535 {
			return fmt.Errorf("invalid allowed-ports")
		}
		cfg.Probe.AllowedPorts = append(cfg.Probe.AllowedPorts, port)
	}
	cfg.Logger = slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	return agentclient.Run(ctx, cfg)
}

func env(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}
func envInt(key string, fallback int) (int, error) {
	value, ok := os.LookupEnv(key)
	if !ok {
		return fallback, nil
	}
	n, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer", key)
	}
	return n, nil
}
func envInt64(key string, fallback int64) (int64, error) {
	value, ok := os.LookupEnv(key)
	if !ok {
		return fallback, nil
	}
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer", key)
	}
	return n, nil
}
func envFloat(key string, fallback float64) (float64, error) {
	value, ok := os.LookupEnv(key)
	if !ok {
		return fallback, nil
	}
	n, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be a number", key)
	}
	return n, nil
}
func envBool(key string, fallback bool) (bool, error) {
	value, ok := os.LookupEnv(key)
	if !ok {
		return fallback, nil
	}
	n, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean", key)
	}
	return n, nil
}
func envDuration(key string, fallback time.Duration) (time.Duration, error) {
	value, ok := os.LookupEnv(key)
	if !ok {
		return fallback, nil
	}
	n, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be a duration", key)
	}
	return n, nil
}
func hostname() string {
	value, err := os.Hostname()
	if err != nil {
		return "probe-node"
	}
	return value
}
