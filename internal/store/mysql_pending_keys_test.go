package store

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/newnexus/newnexus/internal/domain"
)

func TestMySQLPendingTaskKeysCoveringIndexPreservesScopeEmptyAndNullSemantics(t *testing.T) {
	s := mysqlTestStore(t)
	ctx := t.Context()
	docs := make([]domain.Doc, 1000)
	expected := map[string]bool{}
	for i := range docs {
		status := "done"
		if i%10 == 0 {
			status = "queued"
		}
		if i%10 == 1 {
			status = "accepted"
		}
		node, candidate := fmt.Sprintf("node-%03d", i%100), fmt.Sprintf("candidate-%04d", i)
		docs[i] = domain.Doc{"id": fmt.Sprintf("task-%04d", i), "status": status, "node_id": node, "candidate_id": candidate, "payload": strings.Repeat("x", 1024)}
		if status != "done" {
			expected[node+":"+candidate] = true
		}
	}
	for i, d := range []domain.Doc{
		{"node_id": "", "candidate_id": "candidate-empty-node"},
		{"node_id": "node-empty-candidate", "candidate_id": ""},
		{"node_id": "", "candidate_id": ""},
	} {
		d["id"] = fmt.Sprintf("empty-%d", i)
		d["status"] = "queued"
		docs = append(docs, d)
		expected[domain.S(d, "node_id")+":"+domain.S(d, "candidate_id")] = true
	}
	if err := s.PutMany(ctx, "tasks", docs); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(ctx, "jobs", domain.Doc{"id": "foreign-kind", "status": "queued", "node_id": "foreign", "candidate_id": "foreign"}); err != nil {
		t.Fatal(err)
	}
	keys, err := s.PendingTaskKeys(ctx)
	if err != nil || len(keys) != len(expected) {
		t.Fatal("pending key set or scan behavior changed", len(keys), len(expected), err)
	}
	for key := range expected {
		if !keys[key] {
			t.Fatal("pending task key omitted", key)
		}
	}

	query := s.pendingTaskKeysSQL()
	var raw string
	if err = s.DB.QueryRowContext(ctx, "EXPLAIN FORMAT=JSON "+query).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var plan any
	if err = json.Unmarshal([]byte(raw), &plan); err != nil {
		t.Fatal(err)
	}
	covered := false
	var visit func(any)
	visit = func(value any) {
		switch value := value.(type) {
		case map[string]any:
			if value["table_name"] == "resources" && value["key"] == "resources_tasks_pending_keys" && value["using_index"] == true {
				covered = true
				t.Logf("PendingTaskKeys type=%v key=%v using_index=%v rows_examined_per_scan=%v", value["access_type"], value["key"], value["using_index"], value["rows_examined_per_scan"])
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
	if !covered {
		t.Fatal("production pending-key query does not use the covering index", raw)
	}

	// Existing scanning into strings rejects SQL NULL instead of silently
	// turning malformed task identifiers into empty valid deduplication keys.
	for _, invalid := range []domain.Doc{
		{"id": "nil-candidate", "node_id": "node", "candidate_id": nil},
		{"id": "missing-candidate", "node_id": "node"},
		{"id": "literal-null-candidate", "node_id": "node", "candidate_id": "null"},
		{"id": "nil-node", "node_id": nil, "candidate_id": "candidate"},
	} {
		invalid["status"] = "queued"
		if err = s.Put(ctx, "tasks", invalid); err != nil {
			t.Fatal(err)
		}
		if _, err = s.PendingTaskKeys(ctx); err == nil || !strings.Contains(err.Error(), "converting NULL to string") {
			t.Fatal("NULL task identifier scan semantics changed", invalid, err)
		}
		if err = s.Delete(ctx, "tasks", domain.S(invalid, "id")); err != nil {
			t.Fatal(err)
		}
	}
}
