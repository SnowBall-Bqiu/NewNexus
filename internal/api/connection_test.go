package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SnowBall-Bqiu/NewNexus/internal/config"
	"github.com/SnowBall-Bqiu/NewNexus/internal/domain"
	"github.com/SnowBall-Bqiu/NewNexus/internal/provider"
	"github.com/SnowBall-Bqiu/NewNexus/internal/security"
	"github.com/SnowBall-Bqiu/NewNexus/internal/store"
	"github.com/go-chi/chi/v5"
	"gorm.io/gorm"
)

type connectionAdapter struct {
	validated provider.Credentials
}

func (a *connectionAdapter) Name() string { return "cloudflare" }
func (a *connectionAdapter) Validate(_ context.Context, credentials provider.Credentials) (provider.Capability, error) {
	a.validated = credentials
	if err := provider.Required(credentials, "apiToken"); err != nil {
		return provider.Capability{}, err
	}
	return provider.Capability{DNS: true, SaaS: true}, nil
}
func (a *connectionAdapter) ListZones(context.Context, provider.Credentials) ([]provider.Zone, error) {
	return nil, nil
}
func (a *connectionAdapter) ListLines(context.Context, provider.Credentials, provider.Zone) ([]provider.Line, error) {
	return nil, nil
}
func (a *connectionAdapter) ListRecords(context.Context, provider.Credentials, provider.Zone, string) ([]provider.Record, error) {
	return nil, nil
}
func (a *connectionAdapter) ApplyChanges(context.Context, provider.Credentials, provider.Zone, []provider.RecordChange) ([]provider.Record, error) {
	return nil, nil
}

func TestUpdateConnectionWithoutCredentialsPreservesSecret(t *testing.T) {
	server, database, vault, _ := newConnectionTestServer(t)
	connection := seedConnection(t, database, vault)

	recorder := httptest.NewRecorder()
	server.updateConnection(recorder, connectionRequest(http.MethodPut, "/api/v1/connections/"+connection.ID, connection.ID, `{"name":"Cloudflare 新名称","purposes":"dns,saas"}`))
	if recorder.Code != http.StatusOK {
		t.Fatalf("update returned %d: %s", recorder.Code, recorder.Body.String())
	}

	var updated domain.Connection
	if err := database.DB.First(&updated, "id = ?", connection.ID).Error; err != nil {
		t.Fatal(err)
	}
	if updated.Name != "Cloudflare 新名称" || updated.CredentialEnc != connection.CredentialEnc || updated.CredentialMask != connection.CredentialMask {
		t.Fatalf("metadata update changed credentials: %#v", updated)
	}
}

func TestUpdateConnectionRotatesValidatedCredentials(t *testing.T) {
	server, database, vault, adapter := newConnectionTestServer(t)
	connection := seedConnection(t, database, vault)

	recorder := httptest.NewRecorder()
	server.updateConnection(recorder, connectionRequest(http.MethodPut, "/api/v1/connections/"+connection.ID, connection.ID, `{"name":"Cloudflare 主账户","purposes":"dns,saas","credentials":{"apiToken":"new-api-token"}}`))
	if recorder.Code != http.StatusOK {
		t.Fatalf("update returned %d: %s", recorder.Code, recorder.Body.String())
	}
	if adapter.validated["apiToken"] != "new-api-token" {
		t.Fatalf("adapter did not validate replacement credentials: %#v", adapter.validated)
	}

	var updated domain.Connection
	if err := database.DB.First(&updated, "id = ?", connection.ID).Error; err != nil {
		t.Fatal(err)
	}
	plaintext, err := vault.Decrypt(updated.CredentialEnc)
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := provider.ParseCredentials(plaintext)
	if err != nil || credentials["apiToken"] != "new-api-token" {
		t.Fatalf("replacement credentials were not stored: credentials=%#v err=%v", credentials, err)
	}
	if updated.CredentialMask == connection.CredentialMask || updated.Status != "connected" {
		t.Fatalf("connection state was not refreshed: %#v", updated)
	}
}

func TestDeleteUnusedConnection(t *testing.T) {
	server, database, vault, _ := newConnectionTestServer(t)
	connection := seedConnection(t, database, vault)

	recorder := httptest.NewRecorder()
	server.deleteConnection(recorder, requestWithID(http.MethodDelete, "/api/v1/connections/"+connection.ID, connection.ID))
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("delete returned %d: %s", recorder.Code, recorder.Body.String())
	}
	if err := database.DB.First(&domain.Connection{}, "id = ?", connection.ID).Error; err != gorm.ErrRecordNotFound {
		t.Fatalf("deleted connection still exists: %v", err)
	}
}

func TestDeleteConnectionReportsDependencies(t *testing.T) {
	server, database, vault, _ := newConnectionTestServer(t)
	connection := seedConnection(t, database, vault)
	if err := database.DB.Create(&domain.SaaSZone{ConnectionID: connection.ID, ZoneName: "example.com"}).Error; err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	server.deleteConnection(recorder, requestWithID(http.MethodDelete, "/api/v1/connections/"+connection.ID, connection.ID))
	if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), "connection_in_use") || !strings.Contains(recorder.Body.String(), "SaaS 区域 1 项") {
		t.Fatalf("connection dependency was not reported: %d %s", recorder.Code, recorder.Body.String())
	}
	if err := database.DB.First(&domain.Connection{}, "id = ?", connection.ID).Error; err != nil {
		t.Fatalf("referenced connection was deleted: %v", err)
	}
}

func newConnectionTestServer(t *testing.T) (*Server, *store.Store, *security.Vault, *connectionAdapter) {
	t.Helper()
	database, err := store.Open(config.Config{DatabaseDriver: "sqlite", DatabaseURL: filepath.Join(t.TempDir(), "connections.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	vault, err := security.NewVault(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	adapter := &connectionAdapter{}
	return &Server{store: database, vault: vault, providers: provider.NewRegistry(adapter)}, database, vault, adapter
}

func seedConnection(t *testing.T, database *store.Store, vault *security.Vault) domain.Connection {
	t.Helper()
	encrypted, err := vault.Encrypt(`{"apiToken":"old-api-token"}`)
	if err != nil {
		t.Fatal(err)
	}
	item := domain.Connection{Name: "Cloudflare 主账户", Provider: "cloudflare", Purposes: "dns,saas", CredentialEnc: encrypted, CredentialMask: "old••••oken", Status: "connected"}
	if err := database.DB.Create(&item).Error; err != nil {
		t.Fatal(err)
	}
	return item
}

func connectionRequest(method, target, id, body string) *http.Request {
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	routeContext := chi.NewRouteContext()
	routeContext.URLParams.Add("id", id)
	return request.WithContext(context.WithValue(request.Context(), chi.RouteCtxKey, routeContext))
}
