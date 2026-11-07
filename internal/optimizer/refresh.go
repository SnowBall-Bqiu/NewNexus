package optimizer

import (
	"context"
	"github.com/newnexus/newnexus/internal/domain"
	"github.com/newnexus/newnexus/internal/store"
	"strings"
	"time"
)

// RefreshPending computes complete recent-history scores outside the ingestion
// transaction. A generation guard retains reports received during aggregation.
func RefreshPending(ctx context.Context, s *store.Store, limit int) (int, error) {
	if limit < 1 {
		limit = 5000
	}
	config, e := loadConfig(ctx, s)
	if e != nil {
		return 0, e
	}
	now := time.Now().UTC()
	total := 0
	for _, kind := range []string{"candidates", "sites"} {
		docs, e := s.AggregationResources(ctx, kind, limit, now.Add(-config.freshness))
		if e != nil {
			return total, e
		}
		for start := 0; start < len(docs); start += 500 {
			end := min(start+500, len(docs))
			args := []any{s.Time(now.Add(-config.freshness))}
			marks := []string{}
			samples := map[string][]Sample{}
			for _, d := range docs[start:end] {
				id := domain.S(d, "id")
				if kind == "sites" {
					id = "site:" + id
				}
				marks = append(marks, "?")
				args = append(args, id)
			}
			query := `SELECT candidate_id,node_id,success,tcp_ms,tls_ms,ttfb_ms,download_bps,observed_at,sample_count,success_rate FROM metrics WHERE observed_at>=? AND candidate_id IN (` + strings.Join(marks, ",") + `) ORDER BY candidate_id,observed_at DESC`
			rows, e := s.DB.QueryContext(ctx, s.Q(query), args...)
			if e != nil {
				return total, e
			}
			for rows.Next() {
				var id, at string
				var success int
				v := Sample{}
				if e = rows.Scan(&id, &v.NodeID, &success, &v.TCP, &v.TLS, &v.TTFB, &v.Download, &at, &v.Count, &v.Rate); e != nil {
					rows.Close()
					return total, e
				}
				if len(samples[id]) >= 1000 {
					continue
				}
				v.Success = success == 1
				v.At, _ = time.Parse(time.RFC3339Nano, at)
				samples[id] = append(samples[id], v)
			}
			e = rows.Err()
			rows.Close()
			if e != nil {
				return total, e
			}
			patches := map[string]domain.Doc{}
			for _, d := range docs[start:end] {
				id := domain.S(d, "id")
				metricID := id
				if kind == "sites" {
					metricID = "site:" + id
				}
				sum := config.score(samples[metricID], now)
				patch := summaryPatch(d, sum)
				patch["aggregate_pending"] = false
				patch["_aggregate_generation"] = domain.S(d, "aggregate_generation")
				if kind == "sites" {
					patch["status"] = "unhealthy"
					if sum.Healthy {
						patch["status"] = "healthy"
					}
				}
				patches[id] = patch
			}
			if e = s.PatchMany(ctx, kind, patches); e != nil {
				return total, e
			}
			total += end - start
		}
	}
	return total, nil
}
func summaryPatch(candidate domain.Doc, sum Summary) domain.Doc {
	patch := domain.Doc{"healthy": sum.Healthy, "score": sum.Score, "success_rate": sum.SuccessRate, "tcp_ms": sum.TCP, "tls_ms": sum.TLS, "ttfb_ms": sum.TTFB, "p50_ms": sum.P50, "p95_ms": sum.P95, "download_bps": sum.Download, "sample_count": sum.Count}
	if !sum.Last.IsZero() {
		patch["last_probed_at"] = sum.Last.UTC().Format(time.RFC3339Nano)
	}
	if sum.Healthy && domain.S(candidate, "tier") == "inventory" {
		patch["tier"] = "ranked"
	}
	if !sum.Healthy && domain.S(candidate, "tier") == "ranked" {
		patch["tier"] = "inventory"
	}
	return patch
}
