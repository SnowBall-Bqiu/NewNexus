package store

import (
	"context"
	"errors"
	"github.com/newnexus/newnexus/internal/domain"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, e := Open(filepath.Join(t.TempDir(), "test.db"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.DB.Close() })
	return s
}
func exerciseStore(t *testing.T, s *Store) {
	ctx := context.Background()
	if e := s.Migrate(ctx); e != nil {
		t.Fatal(e)
	}
	d := domain.Doc{"name": "alpha"}
	if e := s.Put(ctx, "projects", d); e != nil {
		t.Fatal(e)
	}
	a, _ := s.Get(ctx, "projects", domain.S(d, "id"))
	b, _ := s.Get(ctx, "projects", domain.S(d, "id"))
	a["name"] = "beta"
	if e := s.Put(ctx, "projects", a); e != nil {
		t.Fatal(e)
	}
	b["name"] = "stale"
	if e := s.Put(ctx, "projects", b); !errors.Is(e, ErrConflict) {
		t.Fatalf("stale update accepted: %v", e)
	}
	ok, e := s.Lease(ctx, "lease", "a", time.Minute)
	if e != nil || !ok {
		t.Fatal(ok, e)
	}
	ok, e = s.Lease(ctx, "lease", "b", time.Minute)
	if e != nil || ok {
		t.Fatal("lease stolen", ok, e)
	}
	s.ReleaseLease(ctx, "lease", "b")
	ok, _ = s.Lease(ctx, "lease", "b", time.Minute)
	if ok {
		t.Fatal("wrong owner released lease")
	}
	s.ReleaseLease(ctx, "lease", "a")
	ok, e = s.Lease(ctx, "lease", "b", time.Minute)
	if !ok || e != nil {
		t.Fatal(ok, e)
	}
}
func TestSQLiteMigrationCASAndLease(t *testing.T) { exerciseStore(t, testStore(t)) }
func TestPostgresMigrationCASAndLease(t *testing.T) {
	dsn := os.Getenv("NEWNEXUS_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("set NEWNEXUS_TEST_POSTGRES for an isolated PostgreSQL integration database")
	}
	s, e := Open(dsn)
	if e != nil {
		t.Fatal(e)
	}
	defer s.DB.Close()
	exerciseStore(t, s)
}

func TestPostgresDailyPartitionsAndReplicaLease(t *testing.T) {
	dsn := os.Getenv("NEWNEXUS_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("set NEWNEXUS_TEST_POSTGRES")
	}
	ctx := context.Background()
	a, e := Open(dsn)
	if e != nil {
		t.Fatal(e)
	}
	defer a.DB.Close()
	b, e := Open(dsn)
	if e != nil {
		t.Fatal(e)
	}
	defer b.DB.Close()
	var partitions int
	if e = a.DB.QueryRow(`SELECT COUNT(*) FROM pg_inherits WHERE inhparent='metrics'::regclass`).Scan(&partitions); e != nil || partitions < 10 {
		t.Fatal("missing daily metric partitions", partitions, e)
	}
	key := "parallel-" + domain.ID()
	var wg sync.WaitGroup
	wins := make(chan string, 20)
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			db := a
			if i%2 == 1 {
				db = b
			}
			owner := domain.ID()
			ok, e := db.Lease(ctx, key, owner, time.Minute)
			if e != nil {
				errs <- e
			} else if ok {
				wins <- owner
			}
		}(i)
	}
	wg.Wait()
	close(wins)
	close(errs)
	for e := range errs {
		t.Error(e)
	}
	if len(wins) != 1 {
		t.Fatalf("expected one replica lease winner, got %d", len(wins))
	}
}
