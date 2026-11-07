package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/newnexus/newnexus/internal/domain"
)

func (s *Store) mysqlDueCandidates(ctx context.Context, now time.Time, discovery, ranked, active time.Duration, limit int) ([]domain.Doc, error) {
	// Query each tier in priority order. Wrapping the generated scheduling
	// column in COALESCE and sorting a CASE across tiers prevents MySQL from
	// using the composite range index on a large candidate inventory.
	out := []domain.Doc{}
	for _, tier := range []struct {
		name   string
		period time.Duration
	}{{"active", active}, {"ranked", ranked}, {"inventory", discovery}} {
		if len(out) >= limit {
			return out, nil
		}
		query := `SELECT doc,version FROM resources WHERE kind='candidates' AND tier_idx=? AND (last_scheduled_idx IS NULL OR last_scheduled_idx<?) ORDER BY last_scheduled_idx,id LIMIT ?`
		rows, e := s.documents(ctx, query, tier.name, s.Time(now.Add(-tier.period)), limit-len(out))
		if e != nil {
			return nil, e
		}
		out = append(out, rows...)
	}
	return out, nil
}

func (s *Store) mysqlTransaction(ctx context.Context, fn func(*sql.Tx) error) error {
	for attempt := 0; attempt < 4; attempt++ {
		tx, e := s.DB.BeginTx(ctx, nil)
		if e != nil {
			return e
		}
		e = fn(tx)
		if e == nil {
			e = tx.Commit()
		} else {
			_ = tx.Rollback()
		}
		if e == nil {
			return nil
		}
		_ = tx.Rollback()
		var failure *mysql.MySQLError
		if !errors.As(e, &failure) || (failure.Number != 1205 && failure.Number != 1213) || attempt == 3 {
			return e
		}
		timer := time.NewTimer(time.Duration(10*(1<<attempt)) * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return errors.New("MySQL transaction retry exhausted")
}

func (s *Store) mysqlSetSchedule(ctx context.Context, kind, field string, ids []string, at string) error {
	parsed, e := time.Parse(time.RFC3339Nano, at)
	if e != nil {
		return errors.New("invalid scheduling timestamp")
	}
	at = s.Time(parsed)
	ids = append([]string(nil), ids...)
	sort.Strings(ids)
	return s.mysqlTransaction(ctx, func(tx *sql.Tx) error {
		for start := 0; start < len(ids); start += 500 {
			batch := ids[start:min(start+500, len(ids))]
			if e := mysqlLockResources(ctx, tx, kind, batch); e != nil {
				return e
			}
			join, args := mysqlResourcePoints(kind, batch)
			args = append(args, at, at)
			query := "UPDATE " + join + " SET target.doc=JSON_SET(target.doc,'$." + field + "',?),target.version=target.version+1,target.updated_at=?"
			if _, e := tx.ExecContext(ctx, query, args...); e != nil {
				return e
			}
		}
		return nil
	})
}

func (s *Store) mysqlPatchMany(ctx context.Context, kind string, patches map[string]domain.Doc) error {
	ids := []string{}
	for id := range patches {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	// Each independent aggregation row has a generation guard. Commit the
	// same bounded chunks used by the other dialects so unrelated result
	// ingestion is not blocked behind locks in a later chunk. PutMany retains
	// its separate, whole-operation atomic transaction.
	for start := 0; start < len(ids); start += 250 {
		batch := ids[start:min(start+250, len(ids))]
		if e := s.mysqlTransaction(ctx, func(tx *sql.Tx) error {
			if e := mysqlLockResources(ctx, tx, kind, batch); e != nil {
				return e
			}
			join, args := mysqlResourcePoints(kind, batch)
			cases := []string{}
			for _, id := range batch {
				patch := domain.Doc{}
				for key, value := range patches[id] {
					patch[key] = value
				}
				s.normalizeTimes(patch)
				generation, guarded := patch["_aggregate_generation"]
				delete(patch, "_aggregate_generation")
				raw, e := json.Marshal(patch)
				if e != nil {
					return e
				}
				fragment := "WHEN ? THEN "
				args = append(args, id)
				if guarded {
					fragment += "CASE WHEN COALESCE(" + s.field("aggregate_generation") + ",'')=? THEN JSON_MERGE_PATCH(doc,CAST(? AS JSON)) ELSE doc END"
					args = append(args, fmt.Sprint(generation), string(raw))
				} else {
					fragment += "JSON_MERGE_PATCH(doc,CAST(? AS JSON))"
					args = append(args, string(raw))
				}
				cases = append(cases, fragment)
			}
			args = append(args, s.Time(time.Now()))
			query := "UPDATE " + join + " SET target.doc=CASE target.id " + strings.Join(cases, " ") + " ELSE target.doc END,target.version=target.version+1,target.updated_at=?"
			if _, e := tx.ExecContext(ctx, query, args...); e != nil {
				return e
			}
			return nil
		}); e != nil {
			return e
		}
	}
	return nil
}

func (s *Store) mysqlAggregationResources(ctx context.Context, kind string, limit int, staleBefore time.Time) ([]domain.Doc, error) {
	if limit <= 0 {
		return []domain.Doc{}, nil
	}
	if staleBefore.IsZero() {
		return s.documents(ctx, `SELECT doc,version FROM resources WHERE kind=? AND aggregate_pending_idx=1 ORDER BY updated_at,id LIMIT ?`, kind, limit)
	}
	// Each branch has its own bounded, covering index path. Excluding pending
	// rows from the stale branch preserves OR semantics without duplicates.
	// Materialize only IDs, then fetch JSON for the final global limit.
	query := `SELECT r.doc,r.version FROM (
		SELECT id,updated_at FROM (
			(SELECT id,updated_at FROM resources WHERE kind=? AND aggregate_pending_idx=1 ORDER BY updated_at,id LIMIT ?)
			UNION ALL
			(SELECT id,updated_at FROM resources WHERE kind=? AND healthy_idx=1 AND last_probed_idx<? AND (aggregate_pending_idx IS NULL OR aggregate_pending_idx<>1) ORDER BY updated_at,id LIMIT ?)
		) scoped ORDER BY updated_at,id LIMIT ?
	) due JOIN resources r ON r.kind=? AND r.id=due.id ORDER BY due.updated_at,due.id`
	return s.documents(ctx, query, kind, limit, kind, s.Time(staleBefore), limit, limit, kind)
}
