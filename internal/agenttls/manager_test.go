package agenttls

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/newnexus/newnexus/internal/domain"
	"github.com/newnexus/newnexus/internal/security"
	"github.com/newnexus/newnexus/internal/store"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

func testManager(t *testing.T) (*Manager, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agent-tls.db")
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.DB.Close() })
	vault, err := security.NewVault(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return New(db, vault), path
}

func setEnabled(t *testing.T, m *Manager, enabled any) {
	t.Helper()
	ctx := context.Background()
	record, err := m.store.Get(ctx, "settings", SettingKey)
	if err != nil {
		record = domain.Doc{"id": SettingKey, "key": SettingKey}
	}
	record["value"] = enabled
	if err := m.store.Put(ctx, "settings", record); err != nil {
		t.Fatal(err)
	}
}

func TestPublicDisabledDoesNotGenerateAuthorityAndRejectsMalformedSettings(t *testing.T) {
	m, _ := testManager(t)
	info, err := m.Public(context.Background())
	if err != nil || info.Enabled || info.CAURL != "" || info.ServerName != "" || m.store.Count(context.Background(), authorityKind) != 0 {
		t.Fatalf("disabled metadata generated material: %+v %v", info, err)
	}
	for _, value := range []any{"true", "false", nil, 1, map[string]any{}} {
		setEnabled(t, m, value)
		if _, err := m.Enabled(context.Background()); err == nil {
			t.Fatalf("accepted malformed switch %#v", value)
		}
	}
}

func TestAuthorityPersistsEncryptedAcrossRestartAndMatchesPublicPins(t *testing.T) {
	m, path := testManager(t)
	ctx := context.Background()
	material, err := m.Ensure(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(material.ServerName, ".newnexus.invalid") || time.Until(material.NotAfter) < 89*24*time.Hour {
		t.Fatalf("unexpected identity or lifetime: %s %s", material.ServerName, material.NotAfter)
	}
	record, err := m.store.Get(ctx, authorityKind, authorityID)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(record)
	if strings.Contains(string(raw), "PRIVATE KEY") || strings.Contains(string(raw), "CERTIFICATE") || strings.Contains(string(raw), material.ServerName) || domain.S(record, "encrypted_material") == "" {
		t.Fatal("authority persisted plaintext material")
	}
	if err := m.store.DB.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.DB.Close() })
	restarted := New(db, m.vault)
	again, err := restarted.Ensure(ctx)
	if err != nil || !bytes.Equal(again.CAPEM, material.CAPEM) || again.ServerName != material.ServerName || !bytes.Equal(again.Certificate.Certificate[0], material.Certificate.Certificate[0]) {
		t.Fatal("restart changed authority or leaf", err)
	}
	setEnabled(t, restarted, true)
	info, err := restarted.Public(ctx)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(material.CAPEM)
	hash, fingerprint := sha256.Sum256(material.CAPEM), sha256.Sum256(block.Bytes)
	if !info.Enabled || info.CAURL != "/api/v1/agent-ca.pem" || info.CASHA256 != hex.EncodeToString(hash[:]) || info.CAFingerprintSHA256 != hex.EncodeToString(fingerprint[:]) || info.ServerName != material.ServerName {
		t.Fatalf("public pins do not match download: %+v", info)
	}
	setEnabled(t, restarted, false)
	info, err = restarted.Public(ctx)
	if err != nil || info.Enabled || info.CASHA256 != "" || info.ServerName != "" {
		t.Fatal("disabled metadata exposed stale public material", info, err)
	}
	setEnabled(t, restarted, true)
	third, err := restarted.Ensure(ctx)
	if err != nil || !bytes.Equal(third.CAPEM, material.CAPEM) {
		t.Fatal("toggling replaced trusted authority", err)
	}
}

func TestConcurrentReplicasUseOneAuthorityAndRenewLeafWithoutChangingCA(t *testing.T) {
	m, path := testManager(t)
	otherDB, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer otherDB.DB.Close()
	const replicas = 8
	managers := make([]*Manager, replicas)
	for index := range managers {
		db := m.store
		if index%2 == 1 {
			db = otherDB
		}
		managers[index] = New(db, m.vault)
	}
	ensureAll := func() []*Material {
		results := make([]*Material, replicas)
		errors := make([]error, replicas)
		var wg sync.WaitGroup
		for index, manager := range managers {
			wg.Add(1)
			go func() { defer wg.Done(); results[index], errors[index] = manager.Ensure(context.Background()) }()
		}
		wg.Wait()
		for _, err := range errors {
			if err != nil {
				t.Fatal(err)
			}
		}
		for _, result := range results[1:] {
			if !bytes.Equal(result.CAPEM, results[0].CAPEM) || !bytes.Equal(result.Certificate.Certificate[0], results[0].Certificate.Certificate[0]) {
				t.Fatal("replicas published different authority or renewed leaf")
			}
		}
		return results
	}
	first := ensureAll()[0]
	future := time.Now().UTC().Add(61 * 24 * time.Hour)
	for _, manager := range managers {
		manager.now = func() time.Time { return future }
	}
	renewed := ensureAll()[0]
	if !bytes.Equal(first.CAPEM, renewed.CAPEM) || first.ServerName != renewed.ServerName || bytes.Equal(first.Certificate.Certificate[0], renewed.Certificate.Certificate[0]) || !renewed.NotAfter.After(first.NotAfter) {
		t.Fatal("renewal failed or changed the trusted identity")
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(renewed.CAPEM)
	if _, err := renewed.Certificate.Leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: renewed.ServerName, CurrentTime: future}); err != nil {
		t.Fatal("renewed certificate does not validate", err)
	}
	record, _ := m.store.Get(context.Background(), authorityKind, authorityID)
	if domain.N(record, "_version") != 2 {
		t.Fatalf("concurrent CAS created extra renewals: version %v", record["_version"])
	}
}

func TestWrongVaultAndCorruptAuthorityNeverRegenerateTrustedCA(t *testing.T) {
	m, _ := testManager(t)
	ctx := context.Background()
	if _, err := m.Ensure(ctx); err != nil {
		t.Fatal(err)
	}
	wrongVault, _ := security.NewVault(bytes.Repeat([]byte{8}, 32))
	if _, err := New(m.store, wrongVault).Ensure(ctx); err == nil {
		t.Fatal("wrong master key silently replaced authority")
	}
	record, _ := m.store.Get(ctx, authorityKind, authorityID)
	if domain.N(record, "_version") != 1 {
		t.Fatal("failed decrypt changed persisted authority")
	}
	record["encrypted_material"] = "corrupt"
	if err := m.store.Put(ctx, authorityKind, record); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Ensure(ctx); err == nil {
		t.Fatal("corrupt authority silently regenerated")
	}
}

func startGRPC(t *testing.T, transport credentials.TransportCredentials) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(grpc.Creds(transport))
	healthpb.RegisterHealthServer(server, health.NewServer())
	go server.Serve(listener)
	t.Cleanup(func() { server.Stop(); listener.Close() })
	return listener.Addr().String()
}

func dialHealth(t *testing.T, address string, transport credentials.TransportCredentials, wantSuccess bool) *grpc.ClientConn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	connection, err := grpc.DialContext(ctx, address, grpc.WithTransportCredentials(transport), grpc.WithBlock(), grpc.WithReturnConnectionError())
	if wantSuccess {
		if err != nil {
			t.Fatal("gRPC handshake failed", err)
		}
		t.Cleanup(func() { connection.Close() })
		if _, err := healthpb.NewHealthClient(connection).Check(ctx, &healthpb.HealthCheckRequest{}); err != nil {
			t.Fatal("authenticated gRPC failed", err)
		}
	} else if err == nil {
		connection.Close()
		t.Fatal("unexpectedly established rejected gRPC transport")
	}
	return connection
}

func clientTLS(material *Material) credentials.TransportCredentials {
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(material.CAPEM)
	return credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool, ServerName: material.ServerName})
}

func TestDynamicGRPCSwitchEnforcesPinnedTLSOnNewConnectionsAndRestoresLoopbackPlaintext(t *testing.T) {
	m, _ := testManager(t)
	address := startGRPC(t, m.Credentials(nil, true))
	dialHealth(t, address, insecure.NewCredentials(), true)
	material, err := m.Ensure(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	setEnabled(t, m, true)
	secured := dialHealth(t, address, clientTLS(material), true)
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(material.CAPEM)
	if connection, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", address, &tls.Config{
		MinVersion: tls.VersionTLS10, MaxVersion: tls.VersionTLS11, RootCAs: pool, ServerName: material.ServerName, NextProtos: []string{"h2"},
	}); err == nil {
		connection.Close()
		t.Fatal("managed Agent TLS accepted TLS older than 1.2")
	}
	if connection, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", address, &tls.Config{
		MinVersion: tls.VersionTLS12, RootCAs: pool, ServerName: "another.newnexus.invalid", NextProtos: []string{"h2"},
	}); err == nil {
		connection.Close()
		t.Fatal("managed Agent TLS accepted a mismatched certificate identity")
	}
	dialHealth(t, address, insecure.NewCredentials(), false)
	dialHealth(t, address, credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12, ServerName: material.ServerName, RootCAs: x509.NewCertPool()}), false)
	other, _ := testManager(t)
	otherMaterial, _ := other.Ensure(context.Background())
	dialHealth(t, address, clientTLS(otherMaterial), false)
	setEnabled(t, m, false)
	dialHealth(t, address, insecure.NewCredentials(), true)
	// Updating the switch only affects subsequent handshakes, never in-flight RPCs.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := healthpb.NewHealthClient(secured).Check(ctx, &healthpb.HealthCheckRequest{}); err != nil {
		t.Fatal("switch interrupted an existing TLS connection", err)
	}
	if err := m.store.DB.Close(); err != nil {
		t.Fatal(err)
	}
	dialHealth(t, address, insecure.NewCredentials(), false)
}

func TestDynamicCredentialsRestoreConfiguredTLSFallback(t *testing.T) {
	m, _ := testManager(t)
	fallbackManager, _ := testManager(t)
	fallback, err := fallbackManager.Ensure(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	transport := m.Credentials(&tls.Config{Certificates: []tls.Certificate{fallback.Certificate}}, false)
	address := startGRPC(t, transport.Clone())
	dialHealth(t, address, clientTLS(fallback), true)
	material, err := m.Ensure(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	setEnabled(t, m, true)
	dialHealth(t, address, clientTLS(material), true)
	dialHealth(t, address, clientTLS(fallback), false)
	setEnabled(t, m, false)
	dialHealth(t, address, clientTLS(fallback), true)
	dialHealth(t, address, insecure.NewCredentials(), false)
}

type remoteConn struct {
	net.Conn
	remote net.Addr
}

func (c remoteConn) RemoteAddr() net.Addr { return c.remote }

func TestDisabledCredentialsRejectPlaintextPublicPeersAndMalformedSettings(t *testing.T) {
	m, _ := testManager(t)
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	public := remoteConn{Conn: left, remote: &net.TCPAddr{IP: net.ParseIP("8.8.8.8"), Port: 1234}}
	if _, _, err := m.Credentials(nil, true).ServerHandshake(public); err == nil {
		t.Fatal("disabled TLS exposed plaintext to a public peer")
	}
	setEnabled(t, m, "false")
	local := remoteConn{Conn: left, remote: &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 1234}}
	if _, _, err := m.Credentials(nil, true).ServerHandshake(local); err == nil {
		t.Fatal("malformed setting silently downgraded to plaintext")
	}
}

func TestDisablingManagedTLSWithoutExplicitPlaintextCapabilityRejectsEvenLoopback(t *testing.T) {
	m, _ := testManager(t)
	material, err := m.Ensure(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	setEnabled(t, m, true)
	address := startGRPC(t, m.Credentials(nil, false).Clone())
	dialHealth(t, address, clientTLS(material), true)
	setEnabled(t, m, false)
	dialHealth(t, address, insecure.NewCredentials(), false)
	setEnabled(t, m, true)
	dialHealth(t, address, clientTLS(material), true)
}
