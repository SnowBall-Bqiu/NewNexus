package api

import (
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/SnowBall-Bqiu/NewNexus/internal/config"
	"github.com/SnowBall-Bqiu/NewNexus/internal/domain"
	"github.com/SnowBall-Bqiu/NewNexus/internal/store"
)

type cursorListResponse[T any] struct {
	Data []T `json:"data"`
	Meta struct {
		Total      int    `json:"total"`
		NextCursor string `json:"nextCursor"`
	} `json:"meta"`
}

func TestProjectsCursorPaginationIsStable(t *testing.T) {
	database, err := store.Open(config.Config{DatabaseDriver: "sqlite", DatabaseURL: filepath.Join(t.TempDir(), "project-pagination.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	created := time.Now().UTC().Add(-time.Hour)
	for _, id := range []string{"project-a", "project-b", "project-c"} {
		project := domain.Project{Model: domain.Model{ID: id, CreatedAt: created, UpdatedAt: created}, Name: id, Status: "healthy"}
		if err := database.DB.Create(&project).Error; err != nil {
			t.Fatal(err)
		}
	}
	server := &Server{store: database}
	first := requestProjectsPage(t, server, "")
	if first.Meta.Total != 3 || len(first.Data) != 2 || first.Meta.NextCursor == "" {
		t.Fatalf("unexpected first page: %#v", first)
	}
	second := requestProjectsPage(t, server, first.Meta.NextCursor)
	if len(second.Data) != 1 || second.Data[0].ID != "project-c" || second.Meta.NextCursor != "" {
		t.Fatalf("unexpected second page: %#v", second)
	}
}

func TestCandidateCompositeCursorOrdersEqualScores(t *testing.T) {
	items := []domain.Candidate{
		{Model: domain.Model{ID: "candidate-a"}, Address: "1.1.1.1", Score: 99},
		{Model: domain.Model{ID: "candidate-b"}, Address: "1.1.1.1", Score: 99},
		{Model: domain.Model{ID: "candidate-c"}, Address: "1.0.0.1", Score: 98},
	}
	page, cursor := pageCandidates(items, 2)
	if len(page) != 2 || cursor == "" {
		t.Fatalf("unexpected candidate page: %#v %q", page, cursor)
	}
	score, address, id, ok := decodeCandidateCursor(cursor)
	if !ok || score != 99 || address != "1.1.1.1" || id != "candidate-b" {
		t.Fatalf("unexpected decoded cursor: %v %q %q %v", score, address, id, ok)
	}
	if !candidateFollowsCursor(items[2], score, address, id) || candidateFollowsCursor(items[0], score, address, id) {
		t.Fatal("candidate cursor boundary is unstable")
	}
}

func requestProjectsPage(t *testing.T, server *Server, cursor string) cursorListResponse[domain.Project] {
	t.Helper()
	target := "/api/v1/projects?limit=2"
	if cursor != "" {
		target += "&cursor=" + url.QueryEscape(cursor)
	}
	recorder := httptest.NewRecorder()
	server.listProjects(recorder, httptest.NewRequest("GET", target, nil))
	if recorder.Code != 200 {
		t.Fatalf("list projects returned %d: %s", recorder.Code, recorder.Body.String())
	}
	var response cursorListResponse[domain.Project]
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	return response
}
