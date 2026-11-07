package control

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/newnexus/newnexus/internal/domain"
	"github.com/newnexus/newnexus/internal/provider"
	"github.com/newnexus/newnexus/internal/security"
)

func (s *Service) owned(ctx context.Context, kind, id, connection, zone string) ([]domain.Doc, error) {
	all, e := s.Store.List(ctx, "managed-dns")
	if e != nil {
		return nil, e
	}
	out := []domain.Doc{}
	for _, d := range all {
		if domain.S(d, "owner_kind") == kind && domain.S(d, "owner_id") == id && (connection == "" || domain.S(d, "connection_id") == connection) && (zone == "" || domain.S(d, "zone_id") == zone) {
			out = append(out, d)
		}
	}
	return out, nil
}
func (s *Service) saveOwnership(ctx context.Context, release domain.Doc, observed []provider.Record) error {
	kind, id := domain.S(release, "resource_kind"), domain.S(release, "resource_id")
	connection, zone := domain.S(release, "connection_id"), domain.S(release, "zone_id")
	owned, e := s.owned(ctx, kind, id, connection, zone)
	if e != nil {
		return e
	}
	seed := append(records(release, "desired"), records(release, "before")...)
	for _, entry := range owned {
		kept := []provider.Record{}
		for _, r := range records(entry, "records") {
			if len(scopeRows([]provider.Record{r}, seed)) == 0 {
				kept = append(kept, r)
			}
		}
		if len(kept) == 0 {
			if e = s.Store.Delete(ctx, "managed-dns", domain.S(entry, "id")); e != nil {
				return e
			}
		} else {
			entry["records"] = kept
			if e = s.Store.Put(ctx, "managed-dns", entry); e != nil {
				return e
			}
		}
	}
	if len(observed) == 0 {
		return nil
	}
	return s.Store.Put(ctx, "managed-dns", domain.Doc{"owner_kind": kind, "owner_id": id, "connection_id": connection, "zone_id": zone, "zone_name": release["zone_name"], "records": observed, "observed_at": domain.Now(), "fingerprint": hash(observed)})
}

func (s *Service) manageIncremental(ctx context.Context, kind, id, connection, zone, zoneName string, desired []provider.Record) error {
	release, e := s.makeRelease(ctx, kind, id, connection, zone, zoneName, desired, true)
	if e != nil {
		return e
	}
	if domain.S(release, "status") == "unchanged" {
		return nil
	}
	_, e = s.publish(ctx, "apply", release)
	return e
}

func (s *Service) cleanupManaged(ctx context.Context, kind, id string) error {
	owned, e := s.owned(ctx, kind, id, "", "")
	if e != nil {
		return e
	}
	for _, entry := range owned {
		connection, zone := domain.S(entry, "connection_id"), domain.S(entry, "zone_id")
		_, a, e := s.adapter(ctx, connection)
		if e != nil {
			return e
		}
		live, e := a.Records(ctx, zone)
		if e != nil {
			return e
		}
		before := []provider.Record{}
		diff := []provider.Change{}
		for _, r := range records(entry, "records") {
			for _, have := range live {
				if have.ID != r.ID {
					continue
				}
				if !sameRecord(have, r, true) {
					return errors.New("DNS cleanup stopped: managed record was edited externally")
				}
				before = append(before, have)
				v := have
				diff = append(diff, provider.Change{Action: "delete", Name: r.Name, Type: r.Type, Line: r.Line, Before: &v})
			}
		}
		if len(diff) > 0 {
			release := domain.Doc{"resource_kind": kind, "resource_id": id, "connection_id": connection, "zone_id": zone, "zone_name": entry["zone_name"], "status": "pending", "trigger": "cleanup", "before": before, "desired": []provider.Record{}, "diff": diff}
			if e = s.Store.Put(ctx, "releases", release); e != nil {
				return e
			}
			if _, e = s.publish(ctx, "apply", release); e != nil {
				return e
			}
		}
		if _, e = s.Store.Get(ctx, "managed-dns", domain.S(entry, "id")); e == nil {
			if e = s.Store.Delete(ctx, "managed-dns", domain.S(entry, "id")); e != nil {
				return e
			}
		}
	}
	return nil
}

func (s *Service) publish(ctx context.Context, action string, input domain.Doc) (domain.Doc, error) {
	release, e := s.Store.Get(ctx, "releases", domain.S(input, "id"))
	if e != nil {
		return nil, e
	}
	if action == "apply" && domain.S(release, "status") == "applied" {
		return release, nil
	}
	if action == "rollback" && domain.S(release, "status") == "rolled_back" {
		return release, nil
	}
	if action == "apply" && domain.S(release, "status") != "pending" && domain.S(release, "status") != "applying" {
		return release, errors.New("this release cannot be reapplied; create a fresh preview")
	}
	if action == "rollback" && domain.S(release, "status") != "applied" {
		return release, errors.New("only an applied release can be rolled back")
	}
	connection, zone := domain.S(release, "connection_id"), domain.S(release, "zone_id")
	locked, unlock, e := s.locked(ctx, connection+":"+zone)
	if e != nil {
		return release, e
	}
	defer unlock()
	ctx = locked
	_, a, e := s.adapter(ctx, connection)
	if e != nil {
		return release, e
	}
	before, desired := records(release, "before"), records(release, "desired")
	seed := append(append([]provider.Record{}, before...), desired...)
	live, e := a.Records(ctx, zone)
	if e != nil {
		return release, e
	}
	current := scopeRows(live, seed)
	if action == "rollback" {
		applied := records(release, "applied_records")
		if !sameRecords(current, applied, true) {
			return release, errors.New("rollback stopped: DNS has drifted since this release")
		}
		e = s.restore(ctx, a, release, before, changes(release, "applied_changes"))
		if e != nil {
			s.alert(ctx, "rollback_failed", "releases", domain.S(release, "id"), e.Error())
			return release, e
		}
		release["status"] = "rolled_back"
		release["rolled_back_at"] = domain.Now()
		return release, s.Store.Put(ctx, "releases", release)
	}
	kind, id := domain.S(release, "resource_kind"), domain.S(release, "resource_id")
	if domain.B(release, "migration") {
		if e = s.validateMigration(ctx, release); e != nil {
			return release, e
		}
	}
	if kind == "rules" || kind == "optimized-hostnames" {
		resource, e := s.Store.Get(ctx, kind, id)
		if e != nil {
			return release, e
		}
		expected, e := s.desired(ctx, kind, resource, a)
		if e != nil {
			return release, e
		}
		if !sameRecords(expected, desired, false) {
			return release, errors.New("configuration or eligible targets changed after preview; review a fresh DNS diff")
		}
	}
	// Recover after a worker restart only when the complete desired state is
	// observable. A partial write is compensated rather than repeated blindly.
	if domain.S(release, "status") == "applying" {
		if sameRecords(current, desired, false) {
			return s.finish(ctx, a, release, current)
		}
		if len(changes(release, "applied_changes")) > 0 {
			e = s.restore(ctx, a, release, before, changes(release, "applied_changes"))
			release["status"] = "failed"
			release["error_message"] = "interrupted release was compensated; create a fresh preview"
			if e != nil {
				release["status"] = "rollback_failed"
				release["error_message"] = security.Redact(e.Error())
			}
			_ = s.Store.Put(ctx, "releases", release)
			return release, errors.New(domain.S(release, "error_message"))
		}
	}
	if !sameRecords(current, before, true) {
		s.alert(ctx, "external_drift", "releases", domain.S(release, "id"), "DNS differs from the reviewed preview snapshot")
		return release, errors.New("DNS drift detected since preview; no records were changed")
	}
	release["status"] = "applying"
	release["started_at"] = domain.Now()
	if e = s.Store.Put(ctx, "releases", release); e != nil {
		return release, e
	}
	completed := []provider.Change{}
	for _, change := range changes(release, "diff") {
		done, err := a.ApplyChanges(ctx, zone, []provider.Change{change})
		completed = append(completed, done...)
		release["applied_changes"] = completed
		if saveErr := s.Store.Put(ctx, "releases", release); saveErr != nil && err == nil {
			err = saveErr
		}
		if err != nil {
			return s.failPublication(ctx, a, release, before, completed, err)
		}
	}
	live, e = a.Records(ctx, zone)
	if e != nil {
		return s.failPublication(ctx, a, release, before, completed, e)
	}
	current = scopeRows(live, seed)
	if !sameRecords(current, desired, false) {
		return s.failPublication(ctx, a, release, before, completed, errors.New("DNS provider read-back did not match the reviewed desired state"))
	}
	return s.finish(ctx, a, release, current)
}

func (s *Service) finish(ctx context.Context, a *provider.Adapter, release domain.Doc, current []provider.Record) (domain.Doc, error) {
	if e := s.Verify(ctx, domain.S(release, "zone_name"), verificationSet(current, records(release, "before"))); e != nil {
		return s.failPublication(ctx, a, release, records(release, "before"), changes(release, "applied_changes"), e)
	}
	if e := s.saveOwnership(ctx, release, current); e != nil {
		return s.failPublication(ctx, a, release, records(release, "before"), changes(release, "applied_changes"), e)
	}
	release["status"] = "applied"
	release["applied_at"] = domain.Now()
	release["applied_records"] = current
	release["api_verified"] = true
	release["authoritative_verified"] = true
	release["error_message"] = ""
	if e := s.Store.Put(ctx, "releases", release); e != nil {
		return release, e
	}
	if kind := domain.S(release, "resource_kind"); kind == "rules" || kind == "optimized-hostnames" {
		if resource, e := s.Store.Get(ctx, kind, domain.S(release, "resource_id")); e == nil {
			resource["last_release_id"] = release["id"]
			resource["last_published_at"] = release["applied_at"]
			resource["last_error"] = ""
			if domain.S(release, "resource_kind") == "optimized-hostnames" {
				resource["status"] = "published"
			}
			if e = s.Store.Put(ctx, domain.S(release, "resource_kind"), resource); e != nil {
				return release, e
			}
		}
	}
	if domain.B(release, "migration") {
		if h, e := s.Store.Get(ctx, "saas/hostnames", domain.S(release, "resource_id")); e == nil {
			h["cutover_at"] = release["applied_at"]
			h["last_release_id"] = release["id"]
			h["dns_automation_paused"] = false
			h["dns_automation_pause_reason"] = ""
			if e = s.Store.Put(ctx, "saas/hostnames", h); e != nil {
				return release, e
			}
		}
	}
	return release, nil
}

func (s *Service) failPublication(ctx context.Context, a *provider.Adapter, release domain.Doc, before []provider.Record, completed []provider.Change, cause error) (domain.Doc, error) {
	// Compensation remains bounded and runs after a request/worker cancellation.
	rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()
	rollbackErr := s.restore(rollbackCtx, a, release, before, completed)
	release["status"] = "failed"
	release["error_message"] = security.Redact(cause.Error())
	release["rolled_back_after_failure"] = rollbackErr == nil
	if rollbackErr != nil {
		release["status"] = "rollback_failed"
		release["rollback_error"] = security.Redact(rollbackErr.Error())
		s.alert(rollbackCtx, "rollback_failed", "releases", domain.S(release, "id"), rollbackErr.Error())
	}
	_ = s.Store.Put(rollbackCtx, "releases", release)
	if rollbackErr != nil {
		return release, fmt.Errorf("publication failed: %w; compensation also failed: %v", cause, rollbackErr)
	}
	return release, fmt.Errorf("publication failed and was rolled back: %w", cause)
}

func (s *Service) restore(ctx context.Context, a *provider.Adapter, release domain.Doc, before []provider.Record, completed []provider.Change) error {
	zone := domain.S(release, "zone_id")
	live, e := a.Records(ctx, zone)
	if e != nil {
		return e
	}
	seed := append(append([]provider.Record{}, before...), records(release, "desired")...)
	current := scopeRows(live, seed)
	known := map[string]provider.Record{}
	for _, r := range before {
		known[r.ID] = r
	}
	for _, change := range completed {
		if change.After != nil {
			known[change.After.ID] = *change.After
		}
	}
	for _, r := range current {
		expected, ok := known[r.ID]
		if !ok {
			return errors.New("rollback stopped: unknown external record appeared inside the publication scope")
		}
		if !sameRecord(expected, r, true) {
			return errors.New("rollback stopped: a DNS record was edited externally during publication")
		}
	}
	if _, e = a.ApplyChanges(ctx, zone, recordDiff(current, before)); e != nil {
		return e
	}
	live, e = a.Records(ctx, zone)
	if e != nil {
		return e
	}
	restored := scopeRows(live, seed)
	if !sameRecords(restored, before, false) {
		return errors.New("rollback API verification did not match the pre-publication snapshot")
	}
	if e = s.Verify(ctx, domain.S(release, "zone_name"), verificationSet(restored, records(release, "desired"))); e != nil {
		return fmt.Errorf("rollback authoritative verification failed: %w", e)
	}
	if domain.B(release, "migration") {
		// Before this explicitly confirmed migration some records belonged to
		// the customer. Rollback restores them without claiming their cleanup.
		previous := records(release, "previous_owned_records")
		ownedRestored := []provider.Record{}
		for _, r := range restored {
			for _, old := range previous {
				if sameRecord(r, old, false) {
					ownedRestored = append(ownedRestored, r)
					break
				}
			}
		}
		return s.saveOwnership(ctx, release, ownedRestored)
	}
	return s.saveOwnership(ctx, release, restored)
}
