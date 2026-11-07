package main

import (
	"bytes"
	"encoding/base64"
	"testing"
)

func TestProductionMasterKeyCaseInsensitive(t *testing.T) {
	t.Setenv("NEWNEXUS_ENV", " Production ")
	t.Setenv("NEWNEXUS_MASTER_KEY", "")
	if _, e := loadKey(t.TempDir()); e == nil {
		t.Fatal("mixed-case production generated a local key")
	}
	t.Setenv("NEWNEXUS_MASTER_KEY", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)))
	if key, e := loadKey(t.TempDir()); e != nil || len(key) != 32 {
		t.Fatal(e)
	}
}

func TestProductionRequiresTLSAndSecureAuthentication(t *testing.T) {
	tests := []struct {
		name, environment, http, grpc, cert, key string
		dev, secure, insecure, ok                bool
	}{
		{"production cannot use loopback exception", "production", "127.0.0.1:8080", "127.0.0.1:9090", "", "", false, true, true, false},
		{"production cannot bypass auth", "production", "127.0.0.1:8080", "127.0.0.1:9090", "cert", "key", true, true, false, false},
		{"production cannot use insecure cookies", "production", "127.0.0.1:8080", "127.0.0.1:9090", "cert", "key", false, false, false, false},
		{"production TLS", "production", ":8080", ":9090", "cert", "key", false, true, false, true},
		{"certificate pair required", "development", "127.0.0.1:8080", "127.0.0.1:9090", "cert", "", false, true, true, false},
		{"explicit local development", "development", "127.0.0.1:8080", "127.0.0.1:9090", "", "", true, false, true, true},
		{"insecure agent cannot be public", "development", "127.0.0.1:8080", ":9090", "", "", false, true, true, false},
		{"no silent plaintext", "development", "127.0.0.1:8080", "127.0.0.1:9090", "", "", false, true, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := validateRuntime(tt.environment, tt.http, tt.grpc, tt.cert, tt.key, tt.dev, tt.secure, tt.insecure)
			if (e == nil) != tt.ok {
				t.Fatalf("runtime check error=%v want success %v", e, tt.ok)
			}
		})
	}
}

func TestManagedAgentTLSDoesNotWeakenRESTProductionRequirements(t *testing.T) {
	for _, tt := range []struct {
		name, environment, http, grpc, cert, key string
		dev, secure, insecure, managed, ok       bool
	}{
		{"managed independent public gRPC", "development", "127.0.0.1:8080", ":9091", "", "", false, false, false, true, true},
		{"disabled independent public gRPC", "development", "127.0.0.1:8080", ":9091", "", "", false, true, false, false, false},
		{"managed TLS still requires production HTTP certificate", "production", ":8080", ":9091", "", "", false, true, false, true, false},
		{"managed production with HTTP certificate", "production", ":8080", ":9091", "cert", "key", false, true, false, true, true},
		{"managed TLS does not allow insecure public cookies", "development", ":8080", ":9091", "", "", false, false, false, true, false},
		{"managed TLS does not allow public authentication bypass", "development", ":8080", ":9091", "", "", true, true, false, true, false},
		{"managed TLS still requires complete global key pair", "development", "127.0.0.1:8080", ":9091", "cert", "", false, true, false, true, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := validateRuntimeWithAgentTLS(tt.environment, tt.http, tt.grpc, tt.cert, tt.key, tt.dev, tt.secure, tt.insecure, tt.managed)
			if (err == nil) != tt.ok {
				t.Fatalf("validation error=%v want success=%v", err, tt.ok)
			}
		})
	}
}

func TestPlaintextFallbackNeedsExplicitDevelopmentLoopbackCapability(t *testing.T) {
	for _, tt := range []struct {
		name, environment, cert, listening string
		allowed, want                      bool
	}{
		{"explicit local development", "development", "", "127.0.0.1:9091", true, true},
		{"explicit IPv6 development", " Development ", "", "[::1]:9091", true, true},
		{"no opt-in", "development", "", "127.0.0.1:9091", false, false},
		{"production remains TLS", "production", "", "127.0.0.1:9091", true, false},
		{"other environment is not implicit development", "staging", "", "127.0.0.1:9091", true, false},
		{"public listener is not local", "development", "", "0.0.0.0:9091", true, false},
		{"public IP is not local", "development", "", "8.8.8.8:9091", true, false},
		{"existing TLS remains TLS", "development", "certificate", "127.0.0.1:9091", true, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := agentAllowsLoopbackPlaintext(tt.environment, tt.cert, tt.listening, tt.allowed); got != tt.want {
				t.Fatalf("plaintext capability=%v want=%v", got, tt.want)
			}
		})
	}
}
