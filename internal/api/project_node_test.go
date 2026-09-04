package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SnowBall-Bqiu/NewNexus/internal/agenthub"
	"github.com/SnowBall-Bqiu/NewNexus/internal/config"
	"github.com/SnowBall-Bqiu/NewNexus/internal/domain"
	"github.com/SnowBall-Bqiu/NewNexus/internal/events"
	jobqueue "github.com/SnowBall-Bqiu/NewNexus/internal/jobs"
	"github.com/SnowBall-Bqiu/NewNexus/internal/store"
	"github.com/go-chi/chi/v5"
	"gorm.io/gorm"
)

func TestProjectIdentityValidation(t *testing.T) {
	if got := normalizeProjectAvatar("星桥网络", ""); got != "星" {
		t.Fatalf("unexpected default avatar %q", got)
	}
	if got := normalizeProjectAvatar("project", "NNX"); got != "NN" {
		t.Fatalf("avatar was not limited to two characters: %q", got)
	}
	if !validProjectColor("#1d7a4c") || validProjectColor("red") || validProjectColor("#abcd") {
		t.Fatal("project color validation accepted an invalid value")
	}
}

func TestDeleteRevokedNodeRemovesIdentityAndTasks(t *testing.T) {
	server, database := newNodeLifecycleTestServer(t)
	now := time.Now().UTC()
	node := domain.ProbeNode{Name: "retired", GroupName: "edge", Status: "revoked", RevokedAt: &now}
	if err := database.DB.Create(&node).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.DB.Create(&domain.AgentTask{NodeID: node.ID, JobID: "done", Status: "completed"}).Error; err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	server.deleteNode(recorder, requestWithID(http.MethodDelete, "/api/v1/nodes/"+node.ID, node.ID))
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("delete returned %d: %s", recorder.Code, recorder.Body.String())
	}
	if err := database.DB.First(&domain.ProbeNode{}, "id = ?", node.ID).Error; err != gorm.ErrRecordNotFound {
		t.Fatalf("deleted node still exists: %v", err)
	}
	var tasks int64
	if err := database.DB.Model(&domain.AgentTask{}).Where("node_id = ?", node.ID).Count(&tasks).Error; err != nil || tasks != 0 {
		t.Fatalf("node tasks were not removed: count=%d err=%v", tasks, err)
	}
}

func TestDeleteNodeBlocksActiveTasks(t *testing.T) {
	server, database := newNodeLifecycleTestServer(t)
	now := time.Now().UTC()
	node := domain.ProbeNode{Name: "busy", GroupName: "edge", Status: "revoked", RevokedAt: &now}
	if err := database.DB.Create(&node).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.DB.Create(&domain.AgentTask{NodeID: node.ID, JobID: "queued", Status: "queued"}).Error; err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	server.deleteNode(recorder, requestWithID(http.MethodDelete, "/api/v1/nodes/"+node.ID, node.ID))
	if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), "node_has_active_tasks") {
		t.Fatalf("active task dependency was not reported: %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestDeleteNodeBlocksRouteQuorumCapacity(t *testing.T) {
	server, database := newNodeLifecycleTestServer(t)
	nodes := []domain.ProbeNode{
		{Name: "remove", GroupName: "telecom", Status: "offline"},
		{Name: "keep", GroupName: "telecom", Status: "online"},
	}
	if err := database.DB.Create(&nodes).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.DB.Create(&domain.RouteLine{RouteID: "route", Alias: "电信", NodeGroup: "telecom", MinNodes: 2}).Error; err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	server.deleteNode(recorder, requestWithID(http.MethodDelete, "/api/v1/nodes/"+nodes[0].ID, nodes[0].ID))
	if recorder.Code != http.StatusConflict || !strings.Contains(recorder.Body.String(), "node_group_capacity_required") {
		t.Fatalf("route quorum dependency was not reported: %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestRotateNodeQueuesLockedJobWithoutReturningCredential(t *testing.T) {
	server, database := newNodeLifecycleTestServer(t)
	node := domain.ProbeNode{Name: "offline", GroupName: "edge", Status: "offline"}
	if err := database.DB.Create(&node).Error; err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	request := requestWithID(http.MethodPost, "/api/v1/nodes/"+node.ID+"/rotate", node.ID)
	request.Header.Set("Idempotency-Key", "rotate-once")
	server.rotateNodeCredentials(recorder, request)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("rotate returned %d: %s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(strings.ToLower(recorder.Body.String()), "token") {
		t.Fatalf("rotation response exposed credential material: %s", recorder.Body.String())
	}
	var job domain.Job
	if err := database.DB.Where("resource_id = ? AND type = ?", node.ID, "node.rotate").First(&job).Error; err != nil {
		t.Fatal(err)
	}
	if job.ExecutionKey != "node:"+node.ID {
		t.Fatalf("unexpected node rotation execution key %q", job.ExecutionKey)
	}
}

func newNodeLifecycleTestServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	database, err := store.Open(config.Config{DatabaseDriver: "sqlite", DatabaseURL: filepath.Join(t.TempDir(), "node-lifecycle.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	broker := events.NewBroker()
	return &Server{store: database, hub: agenthub.New(database, broker), broker: broker, jobs: jobqueue.New(database, broker)}, database
}

func requestWithID(method, target, id string) *http.Request {
	request := httptest.NewRequest(method, target, nil)
	routeContext := chi.NewRouteContext()
	routeContext.URLParams.Add("id", id)
	return request.WithContext(context.WithValue(request.Context(), chi.RouteCtxKey, routeContext))
}
