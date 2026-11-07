package api

import (
	"context"
	"time"

	"github.com/newnexus/newnexus/internal/domain"
)

const externalMonitorLease = "external-monitor"

// Monitoring starts immediately and retains its five-second cadence. Enqueuing
// notifications and external synchronizations must not hold up the next probe
// pass. Slow passes coalesce ticks, and shared leases select one replica.
func (s *Server) externalMonitor(ctx context.Context) {
	owner := domain.ID()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	defer func() {
		release, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s.Store.ReleaseLease(release, externalMonitorLease, owner)
	}()
	first := true
	for {
		if !first {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
		first = false
		if ctx.Err() != nil {
			return
		}
		acquire, done := context.WithTimeout(ctx, 5*time.Second)
		held, err := s.Store.Lease(acquire, externalMonitorLease, owner, 30*time.Second)
		done()
		if err != nil || !held {
			continue
		}
		work, cancel := context.WithCancel(ctx)
		renewed := make(chan struct{})
		go func() {
			defer close(renewed)
			ticker := time.NewTicker(5 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-work.Done():
					return
				case <-ticker.C:
					renew, done := context.WithTimeout(work, 5*time.Second)
					held, err := s.Store.Lease(renew, externalMonitorLease, owner, 30*time.Second)
					done()
					if err != nil || !held {
						cancel()
						return
					}
				}
			}
		}()
		// Queue uses deterministic job IDs; a crash or leader change retries
		// unfinished monitoring without creating a second notification job.
		s.monitorExternal(s.cachedSettings(work), time.Now())
		cancel()
		<-renewed
	}
}
