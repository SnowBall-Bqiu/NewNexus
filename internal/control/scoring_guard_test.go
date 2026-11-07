package control

import (
	"context"
	"github.com/newnexus/newnexus/internal/domain"
	"strings"
	"testing"
)

func TestOptimizedPreviewExcludesPendingScoresAndHonorsZeroRate(t *testing.T) {
	s, _, _ := fixture(t)
	ctx := context.Background()
	candidate := put(t, s, "candidates", domain.Doc{"id": domain.ID(), "project_id": "project", "family": 4, "address": "1.1.1.1", "healthy": true, "score": 1, "sample_count": 3, "success_rate": 0, "last_probed_at": domain.Now(), "aggregate_pending": true})
	put(t, s, "settings", domain.Doc{"id": "probe.minimum_success_rate", "key": "probe.minimum_success_rate", "value": 0})
	config := domain.Doc{"project_id": "project", "family": 4, "top_n": 1}
	if _, e := s.optimized(ctx, config, "optimized.example.com", 300); e == nil || !strings.Contains(e.Error(), "recomputed") {
		t.Fatal("pending score was eligible for DNS publication", e)
	}
	candidate["aggregate_pending"] = false
	put(t, s, "candidates", candidate)
	rows, e := s.optimized(ctx, config, "optimized.example.com", 300)
	if e != nil || len(rows) != 1 {
		t.Fatal("explicit zero success threshold was ignored", rows, e)
	}
}
