package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/newnexus/newnexus/internal/domain"
	"github.com/newnexus/newnexus/internal/optimizer"
)

type metricFilter struct{ nodes, candidates map[string]bool }

func intersectIDs(a, b map[string]bool) map[string]bool {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	out := map[string]bool{}
	for id := range a {
		if b[id] {
			out[id] = true
		}
	}
	return out
}

// A nil set means all IDs, while an empty set means no observations. Keeping
// those states separate prevents an empty group from falling back to global.
func (s *Server) metricScope(ctx context.Context, resourceType, id, groupID, lineID string, ranking bool) (metricFilter, error) {
	out := metricFilter{}
	nodes, e := s.Store.List(ctx, "nodes")
	if e != nil {
		return out, e
	}
	groups, e := s.Store.List(ctx, "node-groups")
	if e != nil {
		return out, e
	}
	if groupID != "" {
		var selected domain.Doc
		for _, g := range groups {
			if domain.S(g, "id") == groupID {
				selected = g
				break
			}
		}
		if selected == nil {
			return out, errors.New("node group does not exist")
		}
		out.nodes = map[string]bool{}
		for _, n := range nodes {
			if nodeInLine(n, domain.Doc{"node_group": groupID}, groups) || domain.S(n, "group_name") == domain.S(selected, "name") {
				out.nodes[domain.S(n, "id")] = true
			}
		}
	}
	rules, e := s.Store.List(ctx, "rules")
	if e != nil {
		return out, e
	}
	var line, rule domain.Doc
	if lineID != "" {
		for _, r := range rules {
			for _, l := range domain.A(r, "lines") {
				if domain.S(l, "id") == lineID {
					line = l
					rule = r
				}
			}
		}
		if line == nil {
			return out, errors.New("routing line does not exist")
		}
		lineNodes := map[string]bool{}
		for _, n := range nodes {
			if nodeInLine(n, line, groups) {
				lineNodes[domain.S(n, "id")] = true
			}
		}
		out.nodes = intersectIDs(out.nodes, lineNodes)
		if !ranking && (resourceType == "" || resourceType == "rules" || resourceType == "targets") {
			out.candidates = map[string]bool{}
			for _, target := range domain.A(line, "targets") {
				out.candidates["target:"+domain.S(target, "id")] = true
			}
		}
	}
	if resourceType == "" {
		if id != "" {
			return out, errors.New("resource_type is required with id")
		}
		return out, nil
	}
	if id == "" {
		return out, errors.New("id is required with resource_type")
	}
	switch resourceType {
	case "candidates", "nodes", "sites", "rules":
		d, e := s.Store.Get(ctx, resourceType, id)
		if e != nil {
			return out, fmt.Errorf("selected %s resource does not exist", resourceType)
		}
		switch resourceType {
		case "candidates":
			out.candidates = intersectIDs(out.candidates, map[string]bool{id: true})
		case "nodes":
			out.nodes = intersectIDs(out.nodes, map[string]bool{id: true})
		case "sites":
			out.candidates = intersectIDs(out.candidates, map[string]bool{"site:" + id: true})
		case "rules":
			if line != nil && domain.S(rule, "id") != id {
				return out, errors.New("routing line does not belong to the selected rule")
			}
			ids := map[string]bool{}
			for _, l := range domain.A(d, "lines") {
				for _, t := range domain.A(l, "targets") {
					ids["target:"+domain.S(t, "id")] = true
				}
			}
			out.candidates = intersectIDs(out.candidates, ids)
		}
	case "targets":
		found := false
		for _, r := range rules {
			for _, l := range domain.A(r, "lines") {
				for _, t := range domain.A(l, "targets") {
					if domain.S(t, "id") == id {
						found = true
					}
				}
			}
		}
		if !found {
			return out, errors.New("selected routing target does not exist")
		}
		out.candidates = intersectIDs(out.candidates, map[string]bool{"target:" + id: true})
	default:
		return out, errors.New("resource_type must be candidates, nodes, sites, rules or targets")
	}
	return out, nil
}

func appendMetricSet(query *strings.Builder, args *[]any, column string, ids map[string]bool) {
	if ids == nil {
		return
	}
	if len(ids) == 0 {
		query.WriteString(" AND 1=0")
		return
	}
	values := make([]string, 0, len(ids))
	for id := range ids {
		values = append(values, id)
	}
	sort.Strings(values)
	query.WriteString(" AND " + column + " IN (")
	for i, id := range values {
		if i > 0 {
			query.WriteByte(',')
		}
		query.WriteByte('?')
		*args = append(*args, id)
	}
	query.WriteByte(')')
}

func (s *Server) readHistory(ctx context.Context, resourceType, id, groupID, lineID, resolution string, hours int) ([]domain.Doc, error) {
	out := []domain.Doc{}
	if hours < 1 || hours > 4320 {
		return out, errors.New("hours must be between 1 and 4320")
	}
	table := "metrics_1h"
	period := time.Hour
	if resolution == "five_minute" {
		table = "metrics_5m"
		period = 5 * time.Minute
	} else if resolution != "hour" {
		return out, errors.New("resolution must be hour or five_minute")
	}
	filter, e := s.metricScope(ctx, resourceType, id, groupID, lineID, false)
	if e != nil {
		return out, e
	}
	var query strings.Builder
	query.WriteString("SELECT bucket,SUM(sample_count),SUM(success_count),SUM(tcp_sum),SUM(tls_sum),SUM(ttfb_sum),SUM(download_sum) FROM " + table + " WHERE bucket>=? AND bucket<=?")
	args := []any{s.Store.Time(time.Now().Add(-time.Duration(hours) * time.Hour).Truncate(period)), s.Store.Time(time.Now().Truncate(period))}
	appendMetricSet(&query, &args, "node_id", filter.nodes)
	appendMetricSet(&query, &args, "candidate_id", filter.candidates)
	query.WriteString(" GROUP BY bucket ORDER BY bucket")
	rows, e := s.Store.DB.QueryContext(ctx, s.Store.Q(query.String()), args...)
	if e != nil {
		return out, e
	}
	defer rows.Close()
	for rows.Next() {
		var at string
		var count, success, tcp, tls, ttfb, download float64
		if e = rows.Scan(&at, &count, &success, &tcp, &tls, &ttfb, &download); e != nil {
			return out, e
		}
		if count <= 0 {
			continue
		}
		out = append(out, domain.Doc{"time": at, "samples": count, "success": 100 * success / count, "latency": (tcp + tls + ttfb) / count, "tcp_ms": tcp / count, "tls_ms": tls / count, "ttfb_ms": ttfb / count, "download_bps": download / count})
	}
	return out, rows.Err()
}

func (s *Server) rankings(ctx context.Context, groupID, lineID, projectID string, family int) ([]domain.Doc, error) {
	filter, e := s.metricScope(ctx, "", "", groupID, lineID, true)
	if e != nil {
		return nil, e
	}
	candidates, e := s.Store.List(ctx, "candidates")
	if e != nil {
		return nil, e
	}
	eligible := map[string]domain.Doc{}
	for _, c := range candidates {
		if projectID != "" && domain.S(c, "project_id") != projectID || family != 0 && int(domain.N(c, "family")) != family {
			continue
		}
		eligible[domain.S(c, "id")] = c
	}
	now := time.Now()
	freshness := time.Duration(s.setting(ctx, "probe.freshness_seconds", 900)) * time.Second
	minimum := int(s.setting(ctx, "probe.min_samples", 3))
	rate := s.setting(ctx, "probe.minimum_success_rate", .8)
	var query strings.Builder
	query.WriteString("SELECT candidate_id,node_id,success,tcp_ms,tls_ms,ttfb_ms,download_bps,observed_at,sample_count,success_rate FROM metrics WHERE observed_at>=? AND observed_at<=?")
	args := []any{s.Store.Time(now.Add(-freshness)), s.Store.Time(now.Add(2 * time.Minute))}
	appendMetricSet(&query, &args, "node_id", filter.nodes)
	query.WriteString(" ORDER BY observed_at DESC")
	rows, e := s.Store.DB.QueryContext(ctx, s.Store.Q(query.String()), args...)
	if e != nil {
		return nil, e
	}
	samples := map[string][]optimizer.Sample{}
	for rows.Next() {
		var id, at string
		var success int
		v := optimizer.Sample{}
		if e = rows.Scan(&id, &v.NodeID, &success, &v.TCP, &v.TLS, &v.TTFB, &v.Download, &at, &v.Count, &v.Rate); e != nil {
			rows.Close()
			return nil, e
		}
		if eligible[id] == nil {
			continue
		}
		v.At, _ = time.Parse(time.RFC3339Nano, at)
		v.Success = success == 1
		samples[id] = append(samples[id], v)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return nil, e
	}
	weights := domain.Doc{"tcp": .2, "tls": .15, "ttfb": .25, "p50": .15, "p95": .25, "download": .2, "failure": 1}
	if setting, e := s.Store.Get(ctx, "settings", "score.weights"); e == nil {
		for key, value := range domain.M(setting, "value") {
			weights[key] = value
		}
	}
	out := []domain.Doc{}
	for id, c := range eligible {
		sum := optimizer.Score(samples[id], now, minimum, freshness)
		sum.Healthy = sum.Count >= minimum && sum.SuccessRate >= rate && !sum.Last.IsZero() && now.Sub(sum.Last) <= freshness
		sum.Score = 0
		if sum.Healthy {
			latency := sum.TCP*domain.N(weights, "tcp") + sum.TLS*domain.N(weights, "tls") + sum.TTFB*domain.N(weights, "ttfb") + sum.P50*domain.N(weights, "p50") + sum.P95*domain.N(weights, "p95")
			sum.Score = max(0, 100*sum.SuccessRate/(1+latency/100)+min(sum.Download/1000000, 100)*domain.N(weights, "download")-(1-sum.SuccessRate)*100*domain.N(weights, "failure"))
		}
		d := public(c)
		d["healthy"] = sum.Healthy
		d["score"] = sum.Score
		d["sample_count"] = sum.Count
		d["success_rate"] = sum.SuccessRate
		d["tcp_ms"] = sum.TCP
		d["tls_ms"] = sum.TLS
		d["ttfb_ms"] = sum.TTFB
		d["p50_ms"] = sum.P50
		d["p95_ms"] = sum.P95
		d["download_bps"] = sum.Download
		d["last_probed_at"] = nil
		if !sum.Last.IsZero() {
			d["last_probed_at"] = sum.Last.UTC().Format(time.RFC3339Nano)
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := domain.N(out[i], "score"), domain.N(out[j], "score")
		if a == b {
			return domain.S(out[i], "address") < domain.S(out[j], "address")
		}
		return a > b
	})
	return out, nil
}

func (s *Server) metricsRoutes(w http.ResponseWriter, r *http.Request, path string) bool {
	if path != "metrics/history" && path != "candidates/rankings" {
		return false
	}
	if r.Method != http.MethodGet {
		fail(w, 405, "method_not_allowed", errors.New("metrics views require GET"))
		return true
	}
	q := r.URL.Query()
	groupID, lineID := q.Get("group_id"), q.Get("line_id")
	perspective := "global"
	if groupID != "" {
		perspective = "node_group"
	}
	if lineID != "" {
		perspective = "line"
	}
	scope := domain.Doc{"group_id": groupID, "line_id": lineID, "perspective": perspective}
	if path == "metrics/history" {
		hours := 24
		var e error
		if q.Get("hours") != "" {
			hours, e = strconv.Atoi(q.Get("hours"))
			if e != nil {
				fail(w, 400, "invalid_hours", e)
				return true
			}
		}
		resolution := q.Get("resolution")
		if resolution == "" {
			resolution = "hour"
		}
		items, e := s.readHistory(r.Context(), q.Get("resource_type"), q.Get("id"), groupID, lineID, resolution, hours)
		if e != nil {
			fail(w, 400, "invalid_metric_scope", e)
			return true
		}
		scope["resource_type"] = q.Get("resource_type")
		scope["id"] = q.Get("id")
		write(w, 200, domain.Doc{"items": items, "total": len(items), "scope": scope, "resolution": resolution, "hours": hours})
		return true
	}
	family := 0
	if q.Get("family") != "" {
		n, e := strconv.Atoi(q.Get("family"))
		if e != nil || (n != 4 && n != 6) {
			fail(w, 400, "invalid_family", errors.New("family must be 4 or 6"))
			return true
		}
		family = n
	}
	items, e := s.rankings(r.Context(), groupID, lineID, q.Get("project_id"), family)
	if e != nil {
		fail(w, 400, "invalid_metric_scope", e)
		return true
	}
	total := len(items)
	limit := 25
	if q.Get("limit") != "" {
		limit, e = strconv.Atoi(q.Get("limit"))
		if e != nil || limit < 1 || limit > 500 {
			fail(w, 400, "invalid_limit", errors.New("limit must be between 1 and 500"))
			return true
		}
	}
	start := 0
	if cursor := q.Get("cursor"); cursor != "" {
		found := false
		for i, item := range items {
			if domain.S(item, "id") == cursor {
				start = i + 1
				found = true
				break
			}
		}
		if !found {
			fail(w, 400, "invalid_cursor", errors.New("ranking cursor is no longer in this scope; refresh the first page"))
			return true
		}
	}
	end := min(start+limit, total)
	next := ""
	if end < total {
		next = domain.S(items[end-1], "id")
	}
	write(w, 200, domain.Doc{"items": items[start:end], "total": total, "next_cursor": next, "scope": scope})
	return true
}
