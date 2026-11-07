package store

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/newnexus/newnexus/internal/domain"
)

func TestMySQLAggregationIndexedUnionPreservesOrderingScopeAndNulls(t *testing.T) {
	s := mysqlTestStore(t)
	ctx := t.Context()
	now := time.Now().UTC()
	old := s.Time(now.Add(-time.Hour))
	fresh := s.Time(now)
	docs := []domain.Doc{
		{"id": "pending-stale", "aggregate_pending": true, "healthy": true, "last_probed_at": old},
		{"id": "stale", "aggregate_pending": false, "healthy": true, "last_probed_at": old},
		{"id": "pending", "aggregate_pending": true, "healthy": false, "last_probed_at": fresh},
		{"id": "fresh", "healthy": true, "last_probed_at": fresh},
		{"id": "failed", "healthy": false, "last_probed_at": old},
		{"id": "null-time", "healthy": true, "last_probed_at": nil},
		{"id": "empty-time", "healthy": true, "last_probed_at": ""},
		{"id": "string-bools", "aggregate_pending": "true", "healthy": "true", "last_probed_at": old},
	}
	if err := s.PutMany(ctx, "candidates", docs); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(ctx, "sites", domain.Doc{"id": "foreign", "healthy": true, "last_probed_at": old}); err != nil {
		t.Fatal(err)
	}
	for i, id := range []string{"pending-stale", "stale", "pending", "empty-time"} {
		if _, err := s.DB.ExecContext(ctx, `UPDATE resources SET updated_at=? WHERE kind='candidates' AND id=?`, s.Time(now.Add(time.Duration(i)*time.Second)), id); err != nil {
			t.Fatal(err)
		}
	}
	cutoff := now.Add(-time.Minute)
	for _, tc := range []struct {
		limit int
		stale time.Time
		ids   []string
	}{{10, cutoff, []string{"pending-stale", "stale", "pending", "empty-time"}}, {2, cutoff, []string{"pending-stale", "stale"}}, {10, time.Time{}, []string{"pending-stale", "pending"}}, {0, cutoff, nil}} {
		rows, err := s.AggregationResources(ctx, "candidates", tc.limit, tc.stale)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != len(tc.ids) {
			t.Fatalf("limit=%d stale=%v: got %v, want %v", tc.limit, tc.stale, rows, tc.ids)
		}
		for i := range rows {
			if domain.S(rows[i], "id") != tc.ids[i] {
				t.Fatalf("incorrect global ordering: got %v, want %v", rows, tc.ids)
			}
		}
	}
	// Validate both selective index access paths rather than accepting a full
	// inventory scan merely because this small fixture returns correct rows.
	for _, q := range []string{
		`EXPLAIN SELECT id,updated_at FROM resources FORCE INDEX(resources_aggregation) WHERE kind='candidates' AND aggregate_pending_idx=1 ORDER BY updated_at,id LIMIT 2500`,
		`EXPLAIN SELECT id,updated_at FROM resources FORCE INDEX(resources_candidates_stale) WHERE kind='candidates' AND healthy_idx=1 AND last_probed_idx<'` + fresh + `' AND (aggregate_pending_idx IS NULL OR aggregate_pending_idx<>1) ORDER BY updated_at,id LIMIT 2500`,
	} {
		rows, err := s.DB.QueryContext(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		if err != nil {
			rows.Close()
			t.Fatal(err)
		}
		if !rows.Next() {
			rows.Close()
			t.Fatal("missing aggregation query plan")
		}
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		for i, column := range columns {
			if column == "type" && string(values[i].([]byte)) != "range" && string(values[i].([]byte)) != "ref" {
				rows.Close()
				t.Fatalf("aggregation does not use a selective index: %v", values)
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		rows.Close()
	}
}

func TestMySQLPatchManyCommitsBeforeBlockedNextChunkAndPreservesGeneration(t *testing.T) {
	s := mysqlTestStore(t)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	docs := make([]domain.Doc, 501)
	patches := map[string]domain.Doc{}
	for i := range docs {
		id := fmt.Sprintf("candidate-%04d", i)
		docs[i] = domain.Doc{"id": id, "aggregate_pending": true, "aggregate_generation": "before", "name": "configured"}
		patches[id] = domain.Doc{"aggregate_pending": false, "score": 42, "_aggregate_generation": "before"}
	}
	if err := s.PutMany(ctx, "candidates", docs); err != nil {
		t.Fatal(err)
	}
	// Both the lock and subsequent UPDATE must use unique point lookups;
	// fixing only the SELECT would leave the UPDATE's range locks intact.
	ids := make([]string, 250)
	for i := range ids {
		ids[i] = fmt.Sprintf("candidate-%04d", i)
	}
	join, args := mysqlResourcePoints("candidates", ids)
	for _, query := range []string{
		"SELECT target.id FROM " + join + " ORDER BY requested.ordinal FOR UPDATE OF target",
		"UPDATE " + join + " SET target.updated_at=target.updated_at",
	} {
		var raw string
		if err := s.DB.QueryRowContext(ctx, "EXPLAIN FORMAT=JSON "+query, args...).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var plan any
		if err := json.Unmarshal([]byte(raw), &plan); err != nil {
			t.Fatal(err)
		}
		point := false
		var visit func(any)
		visit = func(value any) {
			switch value := value.(type) {
			case map[string]any:
				if value["table_name"] == "target" && value["access_type"] == "eq_ref" && value["key"] == "PRIMARY" {
					point = true
				}
				for _, nested := range value {
					visit(nested)
				}
			case []any:
				for _, nested := range value {
					visit(nested)
				}
			}
		}
		visit(plan)
		if !point {
			t.Fatal("write path did not use unique primary-key lookups", raw)
		}
	}
	lock, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback()
	if err = mysqlLockResources(ctx, lock, "candidates", []string{"candidate-0250"}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.PatchMany(ctx, "candidates", patches) }()
	// A committed first batch must be visible while the next batch remains
	// blocked. A single transaction for all 501 rows cannot satisfy this.
	for {
		row, err := s.Get(ctx, "candidates", "candidate-0000")
		if err != nil {
			t.Fatal(err)
		}
		if !domain.B(row, "aggregate_pending") {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("patch completed before blocked row was released: %v", err)
		case <-ctx.Done():
			t.Fatal("first chunk was not independently committed", ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err = s.SetSchedule(ctx, "candidates", "last_scheduled_at", []string{"candidate-0000"}, s.Time(time.Now())); err != nil {
		t.Fatal("committed chunk retained row locks", err)
	}
	// The second chunk has not acquired its first sorted row, so an arriving
	// observation can change a later row before that chunk obtains its locks.
	if _, err = s.DB.ExecContext(ctx, `UPDATE resources SET doc=JSON_SET(doc,'$.aggregate_generation','after','$.aggregate_pending',CAST('true' AS JSON)),version=version+1 WHERE kind='candidates' AND id='candidate-0251'`); err != nil {
		t.Fatal(err)
	}
	if err = lock.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	changed, err := s.Get(ctx, "candidates", "candidate-0251")
	if err != nil {
		t.Fatal(err)
	}
	if !domain.B(changed, "aggregate_pending") || domain.S(changed, "aggregate_generation") != "after" || changed["score"] != nil {
		t.Fatal("stale aggregation cleared a new observation", changed)
	}
	first, err := s.Get(ctx, "candidates", "candidate-0000")
	if err != nil {
		t.Fatal(err)
	}
	if domain.S(first, "name") != "configured" || domain.S(first, "last_scheduled_at") == "" || domain.B(first, "aggregate_pending") {
		t.Fatal("independent update/configuration was overwritten", first)
	}
}
