package config

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	HTTPAddress          string
	GRPCAddress          string
	GRPCTLSCertFile      string
	GRPCTLSKeyFile       string
	GRPCAllowH2C         bool
	DatabaseDriver       string
	DatabaseURL          string
	MasterKey            []byte
	PublicURL            string
	DevAuth              bool
	DevMode              bool
	Demo                 bool
	SecureCookies        bool
	SchedulerEnabled     bool
	Version              string
	ProbeInterval        time.Duration
	InventoryInterval    time.Duration
	RankingInterval      time.Duration
	ActiveInterval       time.Duration
	JobWorkers           int
	InstanceID           string
	EnableHSTS           bool
	TrustedProxyCIDRs    string
	AgentPublicServer    string
	AgentBinaryPath      string
	AgentInstallInsecure bool
	AgentTLSFingerprint  string
}

func Load() (Config, error) {
	driver := env("NEWNEXUS_DATABASE_DRIVER", "sqlite")
	databaseURL := os.Getenv("NEWNEXUS_DATABASE_URL")
	if databaseURL == "" {
		if driver == "sqlite" {
			databaseURL = "newnexus.db"
		} else {
			return Config{}, fmt.Errorf("NEWNEXUS_DATABASE_URL is required for %s", driver)
		}
	}
	devAuth := envBool("NEWNEXUS_DEV_AUTH", false)
	demo := envBool("NEWNEXUS_DEMO", false)
	devMode := envBool("NEWNEXUS_DEV_MODE", false) || devAuth || demo
	key, err := parseMasterKey(os.Getenv("NEWNEXUS_MASTER_KEY"), devMode)
	if err != nil {
		return Config{}, err
	}
	interval, err := time.ParseDuration(env("NEWNEXUS_PROBE_INTERVAL", "30s"))
	if err != nil {
		return Config{}, fmt.Errorf("invalid NEWNEXUS_PROBE_INTERVAL: %w", err)
	}
	inventoryInterval, err := time.ParseDuration(env("NEWNEXUS_INVENTORY_INTERVAL", "6h"))
	if err != nil {
		return Config{}, fmt.Errorf("invalid NEWNEXUS_INVENTORY_INTERVAL: %w", err)
	}
	rankingInterval, err := time.ParseDuration(env("NEWNEXUS_RANKING_INTERVAL", "5m"))
	if err != nil {
		return Config{}, fmt.Errorf("invalid NEWNEXUS_RANKING_INTERVAL: %w", err)
	}
	activeInterval, err := time.ParseDuration(env("NEWNEXUS_ACTIVE_INTERVAL", "30s"))
	if err != nil {
		return Config{}, fmt.Errorf("invalid NEWNEXUS_ACTIVE_INTERVAL: %w", err)
	}
	publicURL := env("NEWNEXUS_PUBLIC_URL", "http://localhost:8080")
	grpcCertFile := strings.TrimSpace(os.Getenv("NEWNEXUS_GRPC_TLS_CERT_FILE"))
	grpcKeyFile := strings.TrimSpace(os.Getenv("NEWNEXUS_GRPC_TLS_KEY_FILE"))
	if (grpcCertFile == "") != (grpcKeyFile == "") {
		return Config{}, fmt.Errorf("NEWNEXUS_GRPC_TLS_CERT_FILE and NEWNEXUS_GRPC_TLS_KEY_FILE must be set together")
	}
	grpcAllowH2C := envBool("NEWNEXUS_GRPC_ALLOW_H2C", devMode)
	if !devMode && grpcCertFile == "" && !grpcAllowH2C {
		return Config{}, fmt.Errorf("production gRPC requires TLS certificates or explicit NEWNEXUS_GRPC_ALLOW_H2C=true for a private reverse-proxy listener")
	}
	grpcAddress := env("NEWNEXUS_GRPC_ADDR", ":9090")
	if !devMode && grpcCertFile == "" && grpcAllowH2C {
		if err := validatePrivateH2CAddress(grpcAddress); err != nil {
			return Config{}, err
		}
	}
	hostname, _ := os.Hostname()
	return Config{
		HTTPAddress:          env("NEWNEXUS_HTTP_ADDR", ":8080"),
		GRPCAddress:          grpcAddress,
		GRPCTLSCertFile:      grpcCertFile,
		GRPCTLSKeyFile:       grpcKeyFile,
		GRPCAllowH2C:         grpcAllowH2C,
		DatabaseDriver:       driver,
		DatabaseURL:          databaseURL,
		MasterKey:            key,
		PublicURL:            publicURL,
		DevAuth:              devAuth,
		DevMode:              devMode,
		Demo:                 demo,
		SecureCookies:        envBool("NEWNEXUS_SECURE_COOKIES", strings.HasPrefix(strings.ToLower(publicURL), "https://")),
		SchedulerEnabled:     envBool("NEWNEXUS_SCHEDULER_ENABLED", true),
		Version:              env("NEWNEXUS_VERSION", "dev"),
		ProbeInterval:        interval,
		InventoryInterval:    inventoryInterval,
		RankingInterval:      rankingInterval,
		ActiveInterval:       activeInterval,
		JobWorkers:           envInt("NEWNEXUS_JOB_WORKERS", map[bool]int{true: 1, false: 4}[driver == "sqlite"]),
		InstanceID:           env("NEWNEXUS_INSTANCE_ID", fmt.Sprintf("%s-%d", hostname, os.Getpid())),
		EnableHSTS:           envBool("NEWNEXUS_ENABLE_HSTS", strings.HasPrefix(strings.ToLower(publicURL), "https://")),
		TrustedProxyCIDRs:    strings.TrimSpace(os.Getenv("NEWNEXUS_TRUSTED_PROXY_CIDRS")),
		AgentPublicServer:    env("NEWNEXUS_AGENT_PUBLIC_SERVER", "localhost:9090"),
		AgentBinaryPath:      env("NEWNEXUS_AGENT_BINARY_PATH", "newnexus-agent-linux-{arch}"),
		AgentInstallInsecure: envBool("NEWNEXUS_AGENT_INSTALL_INSECURE", false),
		AgentTLSFingerprint:  strings.TrimSpace(os.Getenv("NEWNEXUS_AGENT_TLS_SHA256")),
	}, nil
}

func validatePrivateH2CAddress(address string) error {
	host, _, err := net.SplitHostPort(strings.TrimSpace(address))
	if err != nil {
		return fmt.Errorf("invalid NEWNEXUS_GRPC_ADDR for h2c: %w", err)
	}
	host = strings.Trim(host, "[]")
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || ip.IsUnspecified() || (!ip.IsLoopback() && !ip.IsPrivate()) {
		return fmt.Errorf("production h2c requires NEWNEXUS_GRPC_ADDR to use a loopback or literal private IP address")
	}
	return nil
}

func parseMasterKey(raw string, devMode bool) ([]byte, error) {
	if raw == "" {
		if !devMode {
			return nil, fmt.Errorf("NEWNEXUS_MASTER_KEY is required outside development mode")
		}
		sum := sha256.Sum256([]byte("newnexus-development-key"))
		return sum[:], nil
	}
	if decoded, err := base64.StdEncoding.DecodeString(raw); err == nil && len(decoded) == 32 {
		return decoded, nil
	}
	if len(raw) < 20 {
		return nil, fmt.Errorf("NEWNEXUS_MASTER_KEY must be a 32-byte base64 key or at least 20 characters")
	}
	sum := sha256.Sum256([]byte(raw))
	return sum[:], nil
}

func envInt(name string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 1 {
		return fallback
	}
	return parsed
}

func env(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func envBool(name string, fallback bool) bool {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}
	return parsed
}
