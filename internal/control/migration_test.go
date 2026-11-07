package control

import (
	"context"
	"testing"

	"github.com/newnexus/newnexus/internal/domain"
	"github.com/newnexus/newnexus/internal/provider"
)

func TestMigrationRequiresLiveDualActiveAndRestoresUnownedTraffic(t *testing.T) {
	s, state, c := fixture(t)
	ctx := context.Background()
	z := zone(t, s, c)
	h := put(t, s, "saas/hostnames", domain.Doc{"hostname": "customer.example.com", "provider_id": "host-provider-id", "saas_zone_id": z["id"], "dns_connection_id": c["id"], "dns_zone_id": "z1", "dns_zone_name": "example.com", "status": "active", "ssl_status": "active", "automation_mode": "auto"})
	old := provider.Record{ID: "customer-old-a", Name: "customer.example.com", Type: "A", Value: "8.8.8.8", TTL: 300, Line: "default", Weight: 1}
	state.rows = []provider.Record{old}
	if _, e := s.Run(ctx, "preview", "saas/hostnames", h); e == nil || state.writes != 0 {
		t.Fatal("local active certificate authorized migration", e, state.writes)
	}
	h, _ = s.Store.Get(ctx, "saas/hostnames", domain.S(h, "id"))
	state.remote["ssl"] = domain.Doc{"status": "active"}
	release, e := s.Run(ctx, "preview", "saas/hostnames", h)
	if e != nil {
		t.Fatal(e)
	}
	if state.writes != 0 || !domain.B(release, "migration") || len(records(release, "before")) != 1 || len(changes(release, "diff")) != 2 {
		t.Fatal("preview wrote traffic or lost old snapshot", release, state.writes)
	}
	state.remote["ssl"] = domain.Doc{"status": "pending_validation"}
	if _, e = s.Run(ctx, "apply", "releases", release); e == nil || state.writes != 0 {
		t.Fatal("stale preview accepted an inactive certificate", e)
	}
	state.remote["ssl"] = domain.Doc{"status": "active"}
	applied, e := s.Run(ctx, "apply", "releases", release)
	if e != nil {
		t.Fatal(e)
	}
	if domain.S(applied, "status") != "applied" || len(state.rows) != 1 || state.rows[0].Type != "CNAME" || state.rows[0].Proxied {
		t.Fatal("migration did not cut over DNS-only", applied, state.rows)
	}
	if _, e = s.Run(ctx, "rollback", "releases", applied); e != nil {
		t.Fatal(e)
	}
	if len(state.rows) != 1 || !sameRecord(state.rows[0], old, false) {
		t.Fatal("rollback did not restore customer's old traffic", state.rows)
	}
	owned, e := s.owned(ctx, "saas/hostnames", domain.S(h, "id"), "", "")
	if e != nil || len(owned) != 0 {
		t.Fatal("rollback claimed ownership of externally managed DNS", owned, e)
	}
	if e = s.cleanupManaged(ctx, "saas/hostnames", domain.S(h, "id")); e != nil || len(state.rows) != 1 {
		t.Fatal("cleanup removed customer's restored record", e, state.rows)
	}
}

func TestMigrationConfigurationAndAnotherOwnerPreventCutover(t *testing.T) {
	s, state, c := fixture(t)
	ctx := context.Background()
	z := zone(t, s, c)
	state.remote["ssl"] = domain.Doc{"status": "active"}
	h := put(t, s, "saas/hostnames", domain.Doc{"hostname": "customer.example.com", "provider_id": "host-provider-id", "saas_zone_id": z["id"], "dns_connection_id": c["id"], "dns_zone_id": "z1", "dns_zone_name": "example.com"})
	release, e := s.Run(ctx, "preview", "saas/hostnames", h)
	if e != nil {
		t.Fatal(e)
	}
	z["cname_target"] = "changed.example.net"
	put(t, s, "saas/zones", z)
	if _, e = s.Run(ctx, "apply", "releases", release); e == nil || state.writes != 0 {
		t.Fatal("configuration changed without a fresh diff", e)
	}
	state.rows = []provider.Record{{ID: "owned-by-rule", Name: "customer.example.com", Type: "CNAME", Value: "old.example.net", TTL: 300, Line: "default", Weight: 1}}
	put(t, s, "managed-dns", domain.Doc{"owner_kind": "rules", "owner_id": "other", "connection_id": c["id"], "zone_id": "z1", "records": state.rows})
	h, _ = s.Store.Get(ctx, "saas/hostnames", domain.S(h, "id"))
	if _, e = s.Run(ctx, "preview", "saas/hostnames", h); e == nil {
		t.Fatal("migration adopted another resource's DNS")
	}
}
