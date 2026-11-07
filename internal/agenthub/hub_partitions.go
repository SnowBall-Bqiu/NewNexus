package agenthub

import (
	"context"
	"time"
)

// MySQL partition DDL implicitly commits. The first observation of a UTC day
// prepares it outside the result transaction. Cache only yesterday, today and
// tomorrow: raw retention is at least 24 hours, so no legitimate retention
// worker can drop those partitions while cached. Historical partitions are
// always checked again, including after retention is shortened and increased.
func protectedMetricDay(at, now time.Time) bool {
	u := now.UTC()
	today := time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
	return !at.Before(today.AddDate(0, 0, -1)) && at.Before(today.AddDate(0, 0, 2))
}

func (h *Hub) knownMySQLMetricDay(at, now time.Time) bool {
	if !protectedMetricDay(at, now) {
		return false
	}
	h.metricMu.RLock()
	defer h.metricMu.RUnlock()
	return h.metricDays[at.UTC().Format("2006-01-02")]
}

func (h *Hub) prepareMetricDay(ctx context.Context, at time.Time) error {
	if h.Store.Driver != "mysql" || !protectedMetricDay(at, time.Now()) {
		return h.Store.EnsureMetricDay(ctx, at)
	}
	h.metricPrepareMu.Lock()
	defer h.metricPrepareMu.Unlock()
	key := at.UTC().Format("2006-01-02")
	if h.knownMySQLMetricDay(at, time.Now()) {
		return nil
	}
	if err := h.Store.EnsureMetricDay(ctx, at); err != nil {
		return err
	}
	h.metricMu.Lock()
	defer h.metricMu.Unlock()
	for old := range h.metricDays {
		day, err := time.Parse("2006-01-02", old)
		if err != nil || !protectedMetricDay(day, time.Now()) {
			delete(h.metricDays, old)
		}
	}
	if h.metricDays == nil {
		h.metricDays = map[string]bool{}
	}
	h.metricDays[key] = true
	return nil
}
