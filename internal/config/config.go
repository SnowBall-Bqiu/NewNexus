package config

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	HTTPAddress          string
	GRPCAddress          string
	DatabaseDriver       string
	DatabaseURL          string
	MasterKey            []byte
	PublicURL            string
	DevAuth              bool
	Demo                 bool
	SecureCookies        bool
	SchedulerEnabled     bool
	Version              string
	ProbeInterval        time.Duration
	AgentPublicServer    string
	AgentBinaryPath      string
	AgentInstallInsecure bool
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
	key, err := parseMasterKey(os.Getenv("NEWNEXUS_MASTER_KEY"))
	if err != nil {
		return Config{}, err
	}
	interval, err := time.ParseDuration(env("NEWNEXUS_PROBE_INTERVAL", "30s"))
	if err != nil {
		return Config{}, fmt.Errorf("invalid NEWNEXUS_PROBE_INTERVAL: %w", err)
	}
	return Config{
		HTTPAddress:          env("NEWNEXUS_HTTP_ADDR", ":8080"),
		GRPCAddress:          env("NEWNEXUS_GRPC_ADDR", ":9090"),
		DatabaseDriver:       driver,
		DatabaseURL:          databaseURL,
		MasterKey:            key,
		PublicURL:            env("NEWNEXUS_PUBLIC_URL", "http://localhost:8080"),
		DevAuth:              envBool("NEWNEXUS_DEV_AUTH", false),
		Demo:                 envBool("NEWNEXUS_DEMO", false),
		SecureCookies:        envBool("NEWNEXUS_SECURE_COOKIES", false),
		SchedulerEnabled:     envBool("NEWNEXUS_SCHEDULER_ENABLED", true),
		Version:              env("NEWNEXUS_VERSION", "dev"),
		ProbeInterval:        interval,
		AgentPublicServer:    env("NEWNEXUS_AGENT_PUBLIC_SERVER", "localhost:9090"),
		AgentBinaryPath:      env("NEWNEXUS_AGENT_BINARY_PATH", "newnexus-agent-linux-{arch}"),
		AgentInstallInsecure: envBool("NEWNEXUS_AGENT_INSTALL_INSECURE", false),
	}, nil
}

func parseMasterKey(raw string) ([]byte, error) {
	if raw == "" {
		// Development can boot without external secrets. Credential writes remain
		// encrypted, but production deployments should always provide this value.
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
