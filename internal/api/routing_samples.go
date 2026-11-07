package api

import (
	"context"
	"github.com/newnexus/newnexus/internal/routing"
	"strings"
	"time"
)

type routingVotesContextKey struct{}

// A scheduling pass reads the latest vote per target/node in one statement;
// its 1000 rules then share that consistent observation snapshot.
func (s *Server) cacheRoutingVotes(ctx context.Context) (context.Context, error) {
	query := `SELECT candidate_id,node_id,success,tcp_ms,tls_ms,ttfb_ms,observed_at FROM (SELECT candidate_id,node_id,success,tcp_ms,tls_ms,ttfb_ms,observed_at,ROW_NUMBER() OVER(PARTITION BY candidate_id,node_id ORDER BY observed_at DESC) AS position FROM metrics WHERE candidate_id LIKE 'target:%' AND observed_at>=?) recent WHERE position=1`
	rows, e := s.Store.DB.QueryContext(ctx, s.Store.Q(query), s.Store.Time(time.Now().Add(-90*time.Second)))
	if e != nil {
		return ctx, e
	}
	defer rows.Close()
	all := map[string][]routing.Vote{}
	for rows.Next() {
		var candidate, node, at string
		var success int
		var tcp, tls, ttfb float64
		if e = rows.Scan(&candidate, &node, &success, &tcp, &tls, &ttfb, &at); e != nil {
			return ctx, e
		}
		id := strings.TrimPrefix(candidate, "target:")
		when, _ := time.Parse(time.RFC3339Nano, at)
		all[id] = append(all[id], routing.Vote{NodeID: node, TargetID: id, Healthy: success == 1, Score: 100 / (1 + (tcp+tls+ttfb)/100), At: when})
	}
	if e = rows.Err(); e != nil {
		return ctx, e
	}
	return context.WithValue(ctx, routingVotesContextKey{}, all), nil
}
