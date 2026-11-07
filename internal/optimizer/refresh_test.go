package optimizer

import (
	"context"
	"github.com/newnexus/newnexus/internal/domain"
	"github.com/newnexus/newnexus/internal/store"
	"path/filepath"
	"testing"
	"time"
)

func TestRefreshPendingUsesHistoryZeroThresholdAndPartialWeights(t *testing.T) {
	ctx := context.Background()
	s, e := store.Open(filepath.Join(t.TempDir(), "score.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer s.DB.Close()
	for _, d := range []domain.Doc{{"id": "probe.minimum_success_rate", "key": "probe.minimum_success_rate", "value": 0}, {"id": "score.weights", "key": "score.weights", "value": domain.Doc{"tcp": 0}}} {
		if e = s.Put(ctx, "settings", d); e != nil {
			t.Fatal(e)
		}
	}
	candidate := domain.Doc{"id": domain.ID(), "tier": "inventory", "aggregate_pending": true, "aggregate_generation": "first"}
	site := domain.Doc{"id": domain.ID(), "url": "https://example.com", "aggregate_pending": true, "aggregate_generation": "site-first"}
	if e = s.Put(ctx, "candidates", candidate); e != nil {
		t.Fatal(e)
	}
	if e = s.Put(ctx, "sites", site); e != nil {
		t.Fatal(e)
	}
	now := time.Now().UTC()
	for i, id := range []string{domain.S(candidate, "id"), "site:" + domain.S(site, "id")} {
		if _, e = s.DB.Exec(`INSERT INTO metrics(id,node_id,candidate_id,observed_at,success,tcp_ms,tls_ms,ttfb_ms,download_bps,sample_count,success_rate)VALUES(?,?,?,?,?,?,?,?,?,?,?)`, domain.ID(), "node", id, now.Format(time.RFC3339Nano), 0, 10, 100, 20, 0, 3, float64(i)); e != nil {
			t.Fatal(e)
		}
	}
	n, e := RefreshPending(ctx, s, 100)
	if e != nil || n != 2 {
		t.Fatal(n, e)
	}
	d, _ := s.Get(ctx, "candidates", domain.S(candidate, "id"))
	if !domain.B(d, "healthy") || domain.B(d, "aggregate_pending") || domain.N(d, "sample_count") != 3 {
		t.Fatal("zero success threshold was ignored", d)
	}
	h, _ := s.Get(ctx, "sites", domain.S(site, "id"))
	if domain.S(h, "status") != "healthy" || domain.N(h, "score") >= 100 || domain.N(h, "score") <= 0 {
		t.Fatal("site aggregation or default TLS weight missing", h)
	}
	if e = s.PatchMany(ctx, "candidates", map[string]domain.Doc{domain.S(candidate, "id"): {"aggregate_generation": "second", "aggregate_pending": true}}); e != nil {
		t.Fatal(e)
	}
	if e = s.PatchMany(ctx, "candidates", map[string]domain.Doc{domain.S(candidate, "id"): {"_aggregate_generation": "first", "aggregate_pending": false, "score": 999}}); e != nil {
		t.Fatal(e)
	}
	d, _ = s.Get(ctx, "candidates", domain.S(candidate, "id"))
	if !domain.B(d, "aggregate_pending") || domain.N(d, "score") == 999 {
		t.Fatal("a stale aggregation cleared a newer report", d)
	}
}
