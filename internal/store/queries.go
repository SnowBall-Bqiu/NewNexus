package store

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/newnexus/newnexus/internal/domain"
	"sort"
	"strings"
	"time"
)

func (s *Store) field(name string) string {
	if s.Driver == "mysql" {
		if column, ok := map[string]string{"node_id": "node_id_idx", "candidate_id": "candidate_id_idx", "status": "status_idx", "tier": "tier_idx", "last_scheduled_at": "last_scheduled_idx", "aggregate_pending": "aggregate_pending_idx", "healthy": "healthy_idx", "last_probed_at": "last_probed_idx"}[name]; ok {
			return column
		}
		return "NULLIF(JSON_UNQUOTE(JSON_EXTRACT(doc,'$." + name + "')),'null')"
	}
	if s.Driver == "pgx" {
		return "(doc::jsonb->>'" + name + "')"
	}
	return "json_extract(doc,'$." + name + "')"
}
func (s *Store) documents(ctx context.Context, query string, args ...any) ([]domain.Doc, error) {
	rows, e := s.DB.QueryContext(ctx, s.Q(query), args...)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []domain.Doc{}
	for rows.Next() {
		var raw string
		var version int64
		if e = rows.Scan(&raw, &version); e != nil {
			return nil, e
		}
		d := domain.Doc{}
		if e = json.Unmarshal([]byte(raw), &d); e != nil {
			return nil, e
		}
		d["_version"] = version
		out = append(out, d)
	}
	return out, rows.Err()
}
func (s *Store) PendingTasks(ctx context.Context, nodeID string, limit int) ([]domain.Doc, error) {
	query := `SELECT doc,version FROM resources WHERE kind='tasks' AND ` + s.field("status") + ` IN ('queued','accepted')`
	args := []any{}
	if nodeID != "" {
		query += " AND " + s.field("node_id") + "=?"
		args = append(args, nodeID)
	}
	query += " ORDER BY created_at"
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}
	return s.documents(ctx, query, args...)
}
func (s *Store) PendingTaskKeys(ctx context.Context) (map[string]bool, error) {
	rows, e := s.DB.QueryContext(ctx, s.pendingTaskKeysSQL())
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var node, candidate string
		if e = rows.Scan(&node, &candidate); e != nil {
			return nil, e
		}
		out[node+":"+candidate] = true
	}
	return out, rows.Err()
}

func (s *Store) pendingTaskKeysSQL() string {
	table := "resources"
	if s.Driver == "mysql" {
		// This fixed projection is fully covered. Newly seeded or recently
		// migrated tables can otherwise choose the clustered kind scan until
		// persistent statistics catch up, fetching every task JSON document.
		table += " FORCE INDEX(resources_tasks_pending_keys)"
	}
	return "SELECT " + s.field("node_id") + "," + s.field("candidate_id") + " FROM " + table + " WHERE kind='tasks' AND " + s.field("status") + " IN ('queued','accepted')"
}
func (s *Store) DueCandidates(ctx context.Context, now time.Time, discovery, ranked, active time.Duration, limit int) ([]domain.Doc, error) {
	if s.Driver == "mysql" {
		return s.mysqlDueCandidates(ctx, now, discovery, ranked, active, limit)
	}
	tier := s.field("tier")
	scheduled := "COALESCE(" + s.field("last_scheduled_at") + ",'')"
	query := `SELECT doc,version FROM resources WHERE kind='candidates' AND ((` + tier + `='active' AND ` + scheduled + `<?) OR (` + tier + `='ranked' AND ` + scheduled + `<?) OR (` + tier + `='inventory' AND ` + scheduled + `<?)) ORDER BY CASE ` + tier + ` WHEN 'active' THEN 0 WHEN 'ranked' THEN 1 ELSE 2 END,` + scheduled + `,id LIMIT ?`
	return s.documents(ctx, query, s.Time(now.Add(-active)), s.Time(now.Add(-ranked)), s.Time(now.Add(-discovery)), limit)
}
func (s *Store) PutMany(ctx context.Context, kind string, docs []domain.Doc) error {
	return s.putMany(ctx, kind, docs, false)
}

// PutNodeTasks locks live node rows before publishing work. A scheduler using
// an old inventory snapshot cannot recreate queued work after node deletion.
func (s *Store) PutNodeTasks(ctx context.Context, docs []domain.Doc) error {
	return s.putMany(ctx, "tasks", docs, true)
}

func (s *Store) putMany(ctx context.Context, kind string, docs []domain.Doc, guardNodes bool) error {
	if len(docs) == 0 {
		return nil
	}
	tx, e := s.DB.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if guardNodes {
		unique := map[string]bool{}
		for _, d := range docs {
			unique[domain.S(d, "node_id")] = true
		}
		ids := make([]string, 0, len(unique))
		for id := range unique {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			if s.Driver == "sqlite" {
				if _, e = tx.ExecContext(ctx, `UPDATE resources SET version=version WHERE kind='nodes' AND id=?`, id); e != nil {
					return e
				}
			}
			q := `SELECT doc FROM resources WHERE kind='nodes' AND id=?`
			if s.Driver != "sqlite" {
				q += ` FOR UPDATE`
			}
			var raw string
			if e = tx.QueryRowContext(ctx, s.Q(q), id).Scan(&raw); e != nil {
				return fmt.Errorf("task node unavailable: %w", e)
			}
			var node domain.Doc
			if e = json.Unmarshal([]byte(raw), &node); e != nil {
				return e
			}
			if domain.S(node, "status") == "revoked" {
				return fmt.Errorf("cannot dispatch work to a revoked node")
			}
		}
	}
	for start := 0; start < len(docs); start += 500 {
		end := min(start+500, len(docs))
		placeholders := []string{}
		args := []any{}
		for _, d := range docs[start:end] {
			if domain.S(d, "id") == "" {
				d["id"] = domain.ID()
			}
			now := s.Time(time.Now())
			if d["created_at"] == nil {
				d["created_at"] = now
			}
			d["updated_at"] = now
			s.normalizeTimes(d)
			serial := domain.Doc{}
			for k, v := range d {
				if k != "_version" {
					serial[k] = v
				}
			}
			raw, e := json.Marshal(serial)
			if e != nil {
				return e
			}
			placeholders = append(placeholders, "(?,?,?,?,?)")
			args = append(args, kind, d["id"], string(raw), d["created_at"], now)
		}
		query := `INSERT INTO resources(kind,id,doc,created_at,updated_at) VALUES ` + strings.Join(placeholders, ",") + ` ON CONFLICT(kind,id) DO NOTHING`
		if s.Driver == "mysql" {
			query = `INSERT INTO resources(kind,id,doc,created_at,updated_at) VALUES ` + strings.Join(placeholders, ",") + ` ON DUPLICATE KEY UPDATE id=id`
		}
		if _, e := tx.ExecContext(ctx, s.Q(query), args...); e != nil {
			return fmt.Errorf("batch insert %s: %w", kind, e)
		}
	}
	if e = tx.Commit(); e != nil {
		return e
	}
	for _, d := range docs {
		d["_version"] = int64(1)
	}
	return nil
}

func (s *Store) SetSchedule(ctx context.Context, kind, field string, ids []string, at string) error {
	if field != "last_scheduled_at" && field != "last_probe_scheduled_at" {
		return fmt.Errorf("invalid scheduling field")
	}
	if s.Driver == "mysql" {
		return s.mysqlSetSchedule(ctx, kind, field, ids, at)
	}
	ids = append([]string(nil), ids...)
	sort.Strings(ids)
	for start := 0; start < len(ids); start += 500 {
		end := min(start+500, len(ids))
		update := "json_set(doc,'$." + field + "',?)"
		if s.Driver == "pgx" {
			update = "jsonb_set(doc::jsonb,'{" + field + "}',to_jsonb(?::text))::text"
		}
		args := []any{at, at, kind}
		marks := []string{}
		for _, id := range ids[start:end] {
			marks = append(marks, "?")
			args = append(args, id)
		}
		query := "UPDATE resources SET doc=" + update + ",version=version+1,updated_at=? WHERE kind=? AND id IN (" + strings.Join(marks, ",") + ")"
		if s.Driver == "pgx" {
			args = []any{kind}
			for _, id := range ids[start:end] {
				args = append(args, id)
			}
			args = append(args, at, at)
			query = "WITH locked AS MATERIALIZED (SELECT kind,id FROM resources WHERE kind=? AND id IN (" + strings.Join(marks, ",") + ") ORDER BY id FOR UPDATE) UPDATE resources AS target SET doc=" + update + ",version=target.version+1,updated_at=? FROM locked WHERE target.kind=locked.kind AND target.id=locked.id"
		}
		if _, e := s.DB.ExecContext(ctx, s.Q(query), args...); e != nil {
			return e
		}
	}
	return nil
}

// PatchMany merges only supplied fields, preserving concurrently written configuration.
// _aggregate_generation prevents an aggregation from clearing observations that
// arrived after its snapshot, including older out-of-order reports.
func (s *Store) PatchMany(ctx context.Context, kind string, patches map[string]domain.Doc) error {
	if s.Driver == "mysql" {
		return s.mysqlPatchMany(ctx, kind, patches)
	}
	ids := []string{}
	for id := range patches {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for start := 0; start < len(ids); start += 250 {
		end := min(start+250, len(ids))
		args := []any{}
		cases := []string{}
		where := []string{}
		for _, id := range ids[start:end] {
			patch := domain.Clone(patches[id])
			generation, guarded := patch["_aggregate_generation"]
			delete(patch, "_aggregate_generation")
			raw, e := json.Marshal(patch)
			if e != nil {
				return e
			}
			merge := "json_patch(doc,?)"
			if s.Driver == "pgx" {
				merge = "(doc::jsonb || ?::jsonb)::text"
			}
			fragment := "WHEN ? THEN "
			args = append(args, id)
			if guarded {
				fragment += "CASE WHEN COALESCE(" + s.field("aggregate_generation") + ",'')=? THEN " + merge + " ELSE doc END"
				args = append(args, fmt.Sprint(generation), string(raw))
			} else {
				fragment += merge
				args = append(args, string(raw))
			}
			cases = append(cases, fragment)
			where = append(where, "?")
		}
		args = append(args, domain.Now(), kind)
		for _, id := range ids[start:end] {
			args = append(args, id)
		}
		query := "UPDATE resources SET doc=CASE id " + strings.Join(cases, " ") + " ELSE doc END,version=version+1,updated_at=? WHERE kind=? AND id IN (" + strings.Join(where, ",") + ")"
		if s.Driver == "pgx" {
			caseArgs := args[:len(args)-len(ids[start:end])-2]
			args = []any{kind}
			for _, id := range ids[start:end] {
				args = append(args, id)
			}
			args = append(args, caseArgs...)
			args = append(args, domain.Now())
			query = "WITH locked AS MATERIALIZED (SELECT kind,id FROM resources WHERE kind=? AND id IN (" + strings.Join(where, ",") + ") ORDER BY id FOR UPDATE) UPDATE resources AS target SET doc=CASE target.id " + strings.Join(cases, " ") + " ELSE target.doc END,version=target.version+1,updated_at=? FROM locked WHERE target.kind=locked.kind AND target.id=locked.id"
		}
		if _, e := s.DB.ExecContext(ctx, s.Q(query), args...); e != nil {
			return e
		}
	}
	return nil
}
func (s *Store) PendingAggregations(ctx context.Context, limit int) ([]domain.Doc, error) {
	return s.AggregationResources(ctx, "candidates", limit, time.Time{})
}
func (s *Store) AggregationResources(ctx context.Context, kind string, limit int, staleBefore time.Time) ([]domain.Doc, error) {
	if s.Driver == "mysql" {
		return s.mysqlAggregationResources(ctx, kind, limit, staleBefore)
	}
	value := any(1)
	if s.Driver == "pgx" {
		value = "true"
	}
	query := "SELECT doc,version FROM resources WHERE kind=? AND (" + s.field("aggregate_pending") + "=?"
	args := []any{kind, value}
	if !staleBefore.IsZero() {
		query += " OR (" + s.field("healthy") + "=? AND " + s.field("last_probed_at") + "<?)"
		args = append(args, value, s.Time(staleBefore))
	}
	query += ") ORDER BY updated_at LIMIT ?"
	args = append(args, limit)
	return s.documents(ctx, query, args...)
}
func (s *Store) FlagAggregations(ctx context.Context) error {
	patch, _ := json.Marshal(domain.Doc{"aggregate_pending": true, "aggregate_generation": domain.ID()})
	merge := "json_patch(doc,?)"
	if s.Driver == "mysql" {
		merge = "JSON_MERGE_PATCH(doc,CAST(? AS JSON))"
	}
	if s.Driver == "pgx" {
		merge = "(doc::jsonb || ?::jsonb)::text"
	}
	_, e := s.DB.ExecContext(ctx, s.Q("UPDATE resources SET doc="+merge+",version=version+1,updated_at=? WHERE kind IN ('candidates','sites')"), string(patch), s.Time(time.Now()))
	return e
}
