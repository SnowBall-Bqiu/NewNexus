package config

import (
	"strings"
	"testing"
)

func setProductionConfig(t *testing.T) {
	t.Helper()
	t.Setenv("NEWNEXUS_DEV_MODE", "false")
	t.Setenv("NEWNEXUS_DEV_AUTH", "false")
	t.Setenv("NEWNEXUS_DEMO", "false")
	t.Setenv("NEWNEXUS_MASTER_KEY", "production-test-master-key")
	t.Setenv("NEWNEXUS_DATABASE_DRIVER", "sqlite")
	t.Setenv("NEWNEXUS_DATABASE_URL", t.TempDir()+"/newnexus.db")
	t.Setenv("NEWNEXUS_GRPC_TLS_CERT_FILE", "")
	t.Setenv("NEWNEXUS_GRPC_TLS_KEY_FILE", "")
}

func TestProductionGRPCRequiresTLSOrExplicitH2C(t *testing.T) {
	setProductionConfig(t)
	t.Setenv("NEWNEXUS_GRPC_ALLOW_H2C", "false")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "requires TLS") {
		t.Fatalf("expected production TLS requirement, got %v", err)
	}
}

func TestProductionH2CAllowsPrivateAddress(t *testing.T) {
	setProductionConfig(t)
	t.Setenv("NEWNEXUS_GRPC_ALLOW_H2C", "true")
	t.Setenv("NEWNEXUS_GRPC_ADDR", "127.0.0.1:9090")
	config, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !config.GRPCAllowH2C {
		t.Fatal("expected h2c to be enabled")
	}
}

func TestProductionH2CRejectsWildcardAndPublicAddress(t *testing.T) {
	for _, address := range []string{":9090", "0.0.0.0:9090", "203.0.113.10:9090"} {
		t.Run(address, func(t *testing.T) {
			setProductionConfig(t)
			t.Setenv("NEWNEXUS_GRPC_ALLOW_H2C", "true")
			t.Setenv("NEWNEXUS_GRPC_ADDR", address)
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), "loopback or literal private IP") {
				t.Fatalf("expected private address rejection for %q, got %v", address, err)
			}
		})
	}
}

func TestDevelopmentDefaultsToH2C(t *testing.T) {
	t.Setenv("NEWNEXUS_DEV_MODE", "true")
	t.Setenv("NEWNEXUS_DEV_AUTH", "false")
	t.Setenv("NEWNEXUS_DEMO", "false")
	t.Setenv("NEWNEXUS_MASTER_KEY", "")
	t.Setenv("NEWNEXUS_DATABASE_DRIVER", "sqlite")
	t.Setenv("NEWNEXUS_DATABASE_URL", t.TempDir()+"/newnexus.db")
	t.Setenv("NEWNEXUS_GRPC_ALLOW_H2C", "")
	t.Setenv("NEWNEXUS_GRPC_ADDR", ":9090")
	config, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !config.GRPCAllowH2C {
		t.Fatal("development mode should default to h2c")
	}
}
