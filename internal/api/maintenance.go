package api

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/newnexus/newnexus/internal/domain"
	"github.com/newnexus/newnexus/internal/store"
)

const maintenanceLease = "retention-maintenance"

type maintenanceOwnerKey struct{}

// Retention runs immediately on startup independently of probe scheduling.
// The completion timestamp is shared by all replicas, so a restart or leader
// change does not run another full cleanup within the same hour.
func (s *Server) maintenance(ctx context.Context) {
	owner := domain.ID()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		if _, err := s.maintenancePass(ctx, owner); err != nil && ctx.Err() == nil {
			slog.Error("retention maintenance failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Server) maintenancePass(ctx context.Context, owner string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	// Completed hours need only a read, rather than creating and deleting a
	// lease on every poll. The same check is repeated under the lease below.
	if due, err := s.maintenanceDue(ctx); err != nil || !due {
		return false, err
	}
	acquireCtx, acquireCancel := context.WithTimeout(ctx, 10*time.Second)
	held, err := s.Store.Lease(acquireCtx, maintenanceLease, owner, time.Minute)
	acquireCancel()
	if err != nil || !held {
		return false, err
	}
	workCtx, cancel := context.WithCancel(ctx)
	renewed := make(chan struct{})
	go func() {
		defer close(renewed)
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-workCtx.Done():
				return
			case <-ticker.C:
				// A stalled renewal must fail before the existing lease can
				// expire; the database driver's read timeout may exceed its TTL.
				renewCtx, done := context.WithTimeout(workCtx, 10*time.Second)
				held, err := s.Store.Lease(renewCtx, maintenanceLease, owner, time.Minute)
				done()
				if err != nil || !held {
					cancel()
					return
				}
			}
		}
	}()
	defer func() {
		cancel()
		<-renewed
		// Parent cancellation must not leave a stale lease blocking failover.
		release, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		s.Store.ReleaseLease(release, maintenanceLease, owner)
	}()

	// Re-read after acquiring the lease; another replica may have finished
	// between this process starting and its acquisition of the lease.
	if due, err := s.maintenanceDue(workCtx); err != nil || !due {
		return false, err
	}
	if err := workCtx.Err(); err != nil {
		return false, err
	}
	err = s.cleanup(context.WithValue(workCtx, maintenanceOwnerKey{}, owner))
	if err != nil {
		return true, err
	}
	return true, workCtx.Err()
}

func (s *Server) maintenanceDue(ctx context.Context) (bool, error) {
	state, err := s.Store.Get(ctx, "system-state", "cleanup")
	if errors.Is(err, store.ErrNotFound) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	finished, err := time.Parse(time.RFC3339Nano, domain.S(state, "finished_at"))
	return err != nil || time.Since(finished) >= time.Hour, nil
}
