package api

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/newnexus/newnexus/internal/domain"
)

const routingEvaluationLease = "routing-evaluator"

type routingOwnerKey struct{}

// Health decisions have their own leader and never hold the probe scheduler's
// next pass. Every acquired pass evaluates the complete rule set; a slow pass
// coalesces subsequent ticks rather than starting overlapping decisions.
func (s *Server) routingEvaluator(ctx context.Context) {
	owner := domain.ID()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	defer func() {
		release, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s.Store.ReleaseLease(release, routingEvaluationLease, owner)
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
		held, err := s.Store.Lease(acquire, routingEvaluationLease, owner, 30*time.Second)
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
					held, err := s.Store.Lease(renew, routingEvaluationLease, owner, 30*time.Second)
					done()
					if err != nil || !held {
						cancel()
						return
					}
				}
			}
		}()
		s.evaluateCurrentRoutingPass(context.WithValue(work, routingOwnerKey{}, owner))
		cancel()
		<-renewed
	}
}

func (s *Server) evaluateCurrentRoutingPass(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	ctx = s.cachedSettings(ctx)
	nodes, err := s.Store.List(ctx, "nodes")
	if err != nil {
		return
	}
	online := false
	for _, node := range nodes {
		last, _ := time.Parse(time.RFC3339Nano, domain.S(node, "last_seen_at"))
		if domain.S(node, "status") == "online" && time.Since(last) > 45*time.Second {
			node["status"] = "offline"
		}
		online = online || domain.S(node, "status") == "online"
	}
	if !online {
		return
	}
	groups, err := s.Store.List(ctx, "node-groups")
	if err != nil {
		return
	}
	started := time.Now()
	s.evaluateRoutingPass(ctx, nodes, groups)
	if time.Since(started) > time.Second {
		slog.Info("rule evaluation pass", "duration_ms", time.Since(started).Milliseconds())
	}
}

// Scheduling updates rule versions independently. Use fresh documents and an
// observation snapshot; the normal CAS check rejects concurrent rule edits or
// scheduling changes instead of overwriting their fields.
func (s *Server) evaluateRoutingPass(ctx context.Context, nodes, groups []domain.Doc) {
	if ctx.Err() != nil {
		return
	}
	rules, err := s.Store.List(ctx, "rules")
	if err != nil {
		slog.Error("rule scan failed", "error", err)
		return
	}
	ctx, err = s.cacheRoutingVotes(ctx)
	if err != nil {
		slog.Error("routing observations failed", "error", err)
		return
	}
	work := make(chan domain.Doc, len(rules))
	for _, rule := range rules {
		work <- rule
	}
	close(work)
	var evaluation sync.WaitGroup
	for i := 0; i < min(16, len(rules)); i++ {
		evaluation.Add(1)
		go func() {
			defer evaluation.Done()
			for rule := range work {
				if ctx.Err() != nil {
					return
				}
				if _, err := s.evaluateRule(ctx, rule, nodes, groups); err != nil {
					slog.Error("rule evaluation failed", "rule_id", rule["id"], "error", err)
				}
			}
		}()
	}
	evaluation.Wait()
}
