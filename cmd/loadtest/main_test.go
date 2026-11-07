package main

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/newnexus/newnexus/internal/domain"
	"github.com/newnexus/newnexus/internal/testutil"
)

func TestQuantileUsesNearestRank(t *testing.T) {
	if quantile(nil, .95) != 0 {
		t.Fatal("empty quantile")
	}
	if quantile([]float64{20, 10}, .95) != 20 {
		t.Fatal("p95 of two observations must include the slower observation")
	}
	if quantile([]float64{3, 1, 2}, .5) != 2 {
		t.Fatal("median")
	}
}

func TestMySQLFixtureAndReportQueries(t *testing.T) {
	db := testutil.MySQL(t)
	var database string
	if err := db.DB.QueryRow(`SELECT DATABASE()`).Scan(&database); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(os.Getenv("NEWNEXUS_TEST_MYSQL"))
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + database
	c := config{Agents: 4, Candidates: 100, Active: 20, Rules: 4, Replicas: 2, Batch: 64, Pool: 32, Budget: 32, Duration: time.Minute}
	nodes := []node{{id: domain.ID()}, {id: domain.ID()}, {id: domain.ID()}, {id: domain.ID()}}
	started := time.Now()
	if err := seed(context.Background(), db, u.String(), c, nodes); err != nil {
		t.Fatal(err)
	}
	admin, err := openAdministrator(u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	if err := inspectMySQLLocks(context.Background(), admin, database); err != nil {
		t.Fatal(err)
	}
	var candidates int
	if err := db.DB.QueryRow(`SELECT COUNT(*) FROM resources WHERE kind='candidates'`).Scan(&candidates); err != nil || candidates != 100 {
		t.Fatalf("candidates=%d error=%v", candidates, err)
	}
	stats := &statistics{started: started, received: map[string]observation{}, acked: map[string]bool{}, firstActive: map[string]time.Time{}, activeNodes: map[string]map[string]time.Time{}, firstTarget: map[string]time.Time{}}
	r := finishReport(context.Background(), c, db, stats, database, time.Since(started), time.Minute)
	if len(r.Errors) != 0 || r.Database != "mysql" || r.HealthyRules != 4 || r.EvaluatedRules != 0 || r.AllRulesEvaluated || r.AllActiveWithinOneCycle || r.TasksPersisted != 0 {
		t.Fatalf("report must query real fixture without claiming work was evaluated: %+v", r)
	}
}

func TestReportIsStandaloneJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "report.json")
	if err := writeReport(path, report{Schema: "newnexus_load_test", Agents: 100, Exclusions: []string{"synthetic probe observations"}}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var decoded report
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Schema != "newnexus_load_test" || decoded.Agents != 100 {
		t.Fatal("report incomplete")
	}
}
