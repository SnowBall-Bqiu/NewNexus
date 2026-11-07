package api

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/newnexus/newnexus/internal/domain"
)

func metricResource(t *testing.T, s *Server, kind string, d domain.Doc) domain.Doc {
	t.Helper()
	if e := s.Store.Put(context.Background(), kind, d); e != nil {
		t.Fatal(e)
	}
	return d
}
func rawMetric(t *testing.T, s *Server, node, candidate string, count int, rate, tcp, download float64) {
	t.Helper()
	_, e := s.Store.DB.ExecContext(context.Background(), s.Store.Q(`INSERT INTO metrics(id,node_id,candidate_id,observed_at,success,tcp_ms,tls_ms,ttfb_ms,download_bps,sample_count,success_rate) VALUES(?,?,?,?,?,?,?,?,?,?,?)`), domain.ID(), node, candidate, s.Store.Time(time.Now()), 1, tcp, 0, 0, download, count, rate)
	if e != nil {
		t.Fatal(e)
	}
}
func TestRankingScopeUsesActualSamplesAndSettings(t *testing.T) {
	s := testServer(t)
	s.DevAuth = true
	ctx := context.Background()
	east := metricResource(t, s, "nodes", domain.Doc{"name": "east", "group_name": "east"})
	west := metricResource(t, s, "nodes", domain.Doc{"name": "west", "group_name": "west"})
	group := metricResource(t, s, "node-groups", domain.Doc{"name": "east", "node_ids": []any{east["id"]}})
	empty := metricResource(t, s, "node-groups", domain.Doc{"name": "empty", "node_ids": []any{}})
	a := metricResource(t, s, "candidates", domain.Doc{"address": "1.1.1.1", "family": 4, "healthy": true, "score": 9999})
	b := metricResource(t, s, "candidates", domain.Doc{"address": "8.8.8.8", "family": 4, "healthy": true, "score": 1})
	rawMetric(t, s, domain.S(east, "id"), domain.S(a, "id"), 3, 1, 10, 0)
	rawMetric(t, s, domain.S(west, "id"), domain.S(a, "id"), 3, 1, 1000, 0)
	rawMetric(t, s, domain.S(east, "id"), domain.S(b, "id"), 3, 1, 100, 1000000)
	rawMetric(t, s, domain.S(west, "id"), domain.S(b, "id"), 3, 1, 1, 1000000)
	global, e := s.rankings(ctx, "", "", "", 0)
	if e != nil || domain.S(global[0], "id") != domain.S(b, "id") {
		t.Fatal("global used cached score rather than real scoped samples", global, e)
	}
	scoped, e := s.rankings(ctx, domain.S(group, "id"), "", "", 0)
	if e != nil || domain.S(scoped[0], "id") != domain.S(a, "id") || domain.N(scoped[0], "sample_count") != 3 {
		t.Fatal("group filter did not use node membership", scoped, e)
	}
	lineID := domain.ID()
	metricResource(t, s, "rules", domain.Doc{"lines": []domain.Doc{{"id": lineID, "node_group": group["id"], "health_perspective": "node_group", "targets": []domain.Doc{{"id": "target-one"}}}}})
	lineScoped, e := s.rankings(ctx, "", lineID, "", 0)
	if e != nil || domain.S(lineScoped[0], "id") != domain.S(a, "id") {
		t.Fatal("line perspective was ignored", lineScoped, e)
	}
	none, e := s.rankings(ctx, domain.S(empty, "id"), "", "", 0)
	if e != nil {
		t.Fatal(e)
	}
	for _, c := range none {
		if domain.B(c, "healthy") || domain.N(c, "sample_count") != 0 || domain.N(c, "score") != 0 {
			t.Fatal("empty group fell back to global", none)
		}
	}
	setting, _ := s.Store.Get(ctx, "settings", "score.weights")
	setting["value"] = domain.Doc{"tcp": 0, "tls": 0, "ttfb": 0, "p50": 0, "p95": 0, "download": 100, "failure": 0}
	metricResource(t, s, "settings", setting)
	scoped, e = s.rankings(ctx, domain.S(group, "id"), "", "", 0)
	if e != nil || domain.S(scoped[0], "id") != domain.S(b, "id") {
		t.Fatal("score.weights was ignored", scoped, e)
	}
	w := request(t, s, "GET", "candidates/rankings?group_id="+domain.S(group, "id")+"&limit=1", nil, nil, "")
	result := domain.Doc{}
	_ = json.Unmarshal(w.Body.Bytes(), &result)
	if w.Code != 200 || len(domain.A(result, "items")) != 1 || domain.S(result, "next_cursor") == "" {
		t.Fatal("ranking route or pagination failed", w.Code, result)
	}
}
func TestHistoryReadsRollupsAndIntersectsResourceLineAndGroup(t *testing.T) {
	s := testServer(t)
	s.DevAuth = true
	ctx := context.Background()
	east := metricResource(t, s, "nodes", domain.Doc{"group_name": "east"})
	west := metricResource(t, s, "nodes", domain.Doc{"group_name": "west"})
	group := metricResource(t, s, "node-groups", domain.Doc{"name": "east", "node_ids": []any{east["id"]}})
	lineID, targetID := domain.ID(), domain.ID()
	rule := metricResource(t, s, "rules", domain.Doc{"lines": []domain.Doc{{"id": lineID, "node_group": group["id"], "targets": []domain.Doc{{"id": targetID}}}}})
	candidate := metricResource(t, s, "candidates", domain.Doc{"address": "1.1.1.1"})
	bucket := s.Store.Time(time.Now().Truncate(time.Hour))
	for _, item := range []struct {
		node, candidate     string
		count, success, tcp float64
	}{{domain.S(east, "id"), "target:" + targetID, 3, 3, 150}, {domain.S(west, "id"), "target:" + targetID, 9, 0, 900}, {domain.S(east, "id"), domain.S(candidate, "id"), 2, 2, 20}} {
		for _, table := range []string{"metrics_1h", "metrics_5m"} {
			_, e := s.Store.DB.ExecContext(ctx, s.Store.Q("INSERT INTO "+table+"(node_id,candidate_id,bucket,sample_count,success_count,tcp_sum,tls_sum,ttfb_sum,download_sum) VALUES(?,?,?,?,?,?,?,?,?)"), item.node, item.candidate, bucket, item.count, item.success, item.tcp, 0, 0, 0)
			if e != nil {
				t.Fatal(e)
			}
		}
	}
	history, e := s.readHistory(ctx, "rules", domain.S(rule, "id"), "", "", "hour", 24)
	if e != nil || len(history) != 1 || domain.N(history[0], "samples") != 12 || domain.N(history[0], "success") != 25 || domain.N(history[0], "latency") != 87.5 {
		t.Fatal("history did not sample-weight actual hourly rollups", history, e)
	}
	history, e = s.readHistory(ctx, "rules", domain.S(rule, "id"), "", lineID, "five_minute", 24)
	if e != nil || len(history) != 1 || domain.N(history[0], "samples") != 3 || domain.N(history[0], "latency") != 50 || domain.N(history[0], "success") != 100 {
		t.Fatal("line history leaked global observations", history, e)
	}
	history, e = s.readHistory(ctx, "candidates", domain.S(candidate, "id"), domain.S(group, "id"), lineID, "hour", 24)
	if e != nil || len(history) != 1 || domain.N(history[0], "samples") != 2 {
		t.Fatal("candidate history should use line node perspective without selecting line targets", history, e)
	}
	w := request(t, s, "GET", "metrics/history?resource_type=rules&id="+domain.S(rule, "id")+"&line_id="+lineID, nil, nil, "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, path := range []string{"metrics/history?hours=0", "metrics/history?resolution=raw", "metrics/history?group_id=missing", "metrics/history?resource_type=rules"} {
		w = request(t, s, "GET", path, nil, nil, "")
		if w.Code != 400 {
			t.Fatal("invalid scope accepted", path, w.Code)
		}
	}
}
