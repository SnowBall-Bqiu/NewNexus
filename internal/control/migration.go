package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/newnexus/newnexus/internal/domain"
	"github.com/newnexus/newnexus/internal/provider"
	"github.com/newnexus/newnexus/internal/routing"
)

func migrationConfig(h, z domain.Doc) domain.Doc {
	out := domain.Doc{}
	for _, k := range []string{"id", "hostname", "provider_id", "saas_zone_id", "dns_connection_id", "dns_zone_id", "dns_zone_name", "custom_origin"} {
		out[k] = h[k]
	}
	out["saas_connection_id"] = z["connection_id"]
	out["saas_zone_provider_id"] = z["zone_id"]
	out["cname_target"] = z["cname_target"]
	return out
}

// migrationDesired checks actual provider state for both preview and apply. A
// saved 'active' UI status cannot authorize a cutover after certificate expiry.
func (s *Service) migrationDesired(ctx context.Context, h, z domain.Doc) (*provider.Adapter, provider.Record, error) {
	host, target := canonical(domain.S(h, "hostname")), canonical(domain.S(z, "cname_target"))
	empty := provider.Record{}
	if !validHostname(host) || !validHostname(target) || host == target {
		return nil, empty, errors.New("SaaS hostname and CNAME target must be valid and cannot form a loop")
	}
	_, cloud, e := s.adapter(ctx, domain.S(z, "connection_id"))
	if e != nil {
		return nil, empty, e
	}
	remoteResponse, e := cloud.SaaS(ctx, http.MethodGet, domain.S(z, "zone_id"), "custom_hostnames/"+url.PathEscape(domain.S(h, "provider_id")), nil)
	if e != nil {
		return nil, empty, e
	}
	remote := domain.M(remoteResponse, "result")
	if canonical(domain.S(remote, "hostname")) != host || domain.S(remote, "status") != "active" || domain.S(domain.M(remote, "ssl"), "status") != "active" {
		return nil, empty, errors.New("hostname ownership and certificate must both be active at Cloudflare before zero-downtime migration")
	}
	if canonical(domain.S(remote, "custom_origin_server")) != canonical(domain.S(h, "custom_origin")) {
		return nil, empty, errors.New("custom origin differs from Cloudflare; reconcile the origin before migration")
	}
	if _, e = s.Resolve(ctx, target); e != nil {
		return nil, empty, fmt.Errorf("SaaS target must resolve publicly: %w", e)
	}
	connection, zone, zoneName := domain.S(h, "dns_connection_id"), domain.S(h, "dns_zone_id"), canonical(domain.S(h, "dns_zone_name"))
	if connection == "" || zone == "" || zoneName == "" || host != zoneName && !strings.HasSuffix(host, "."+zoneName) {
		return nil, empty, errors.New("select the accessible DNS connection and zone containing this hostname")
	}
	_, a, e := s.adapter(ctx, connection)
	if e != nil {
		return nil, empty, e
	}
	zones, e := a.Zones(ctx)
	if e != nil {
		return nil, empty, e
	}
	accessible := false
	for _, v := range zones {
		if v.ID == zone && canonical(v.Name) == zoneName {
			accessible = true
		}
	}
	if !accessible {
		return nil, empty, errors.New("DNS zone ID/name is not accessible through the selected connection")
	}
	cap, e := a.ZoneCapabilities(ctx, zone)
	if e != nil {
		return nil, empty, e
	}
	if host == zoneName && !cap.Apex {
		return nil, empty, errors.New("the DNS provider cannot flatten an apex CNAME")
	}
	edges := map[string][]string{host: {target}}
	rules, e := s.Store.List(ctx, "rules")
	if e != nil {
		return nil, empty, e
	}
	for _, rule := range rules {
		for _, line := range domain.A(rule, "lines") {
			for _, t := range domain.A(line, "targets") {
				if domain.B(t, "enabled") {
					name := canonical(domain.S(rule, "hostname"))
					edges[name] = append(edges[name], canonical(domain.S(t, "cname")))
				}
			}
		}
	}
	if e = routing.DetectLoop(host, edges); e != nil {
		return nil, empty, e
	}
	return a, provider.Record{Name: host, Type: "CNAME", Value: target, TTL: max(300, cap.MinTTL), Line: "default", Weight: 1, Proxied: false}, nil
}

// Only an explicitly reviewed migration may replace previously unowned traffic
// records. Routine sync/activation continues to refuse ownership conflicts.
func (s *Service) previewMigration(ctx context.Context, h, z domain.Doc) (domain.Doc, error) {
	a, want, e := s.migrationDesired(ctx, h, z)
	if e != nil {
		return nil, e
	}
	connection, zone := domain.S(h, "dns_connection_id"), domain.S(h, "dns_zone_id")
	all, e := a.Records(ctx, zone)
	if e != nil {
		return nil, e
	}
	before := []provider.Record{}
	for _, r := range all {
		if canonical(r.Name) != want.Name {
			continue
		}
		if r.Type != "A" && r.Type != "AAAA" && r.Type != "CNAME" {
			if r.Type == "NS" || r.Type == "SOA" {
				continue
			}
			return nil, fmt.Errorf("migration would conflict with an existing %s record at %s", r.Type, want.Name)
		}
		if lineCode(r.Line) != "default" {
			return nil, errors.New("migration requires default-line traffic records; preserve geographic lines in a routing rule")
		}
		before = append(before, r)
	}
	owned, e := s.Store.List(ctx, "managed-dns")
	if e != nil {
		return nil, e
	}
	previousOwned := []provider.Record{}
	for _, entry := range owned {
		if domain.S(entry, "connection_id") != connection || domain.S(entry, "zone_id") != zone {
			continue
		}
		for _, r := range records(entry, "records") {
			if canonical(r.Name) != want.Name {
				continue
			}
			if domain.S(entry, "owner_kind") != "saas/hostnames" || domain.S(entry, "owner_id") != domain.S(h, "id") {
				return nil, errors.New("hostname traffic DNS is managed by another resource")
			}
			found := false
			for _, live := range before {
				if sameRecord(r, live, true) {
					found = true
					previousOwned = append(previousOwned, live)
				}
			}
			if !found {
				return nil, errors.New("managed DNS drift detected; reconcile before migration")
			}
		}
	}
	desired := []provider.Record{want}
	diff := recordDiff(before, desired)
	encoded, _ := json.Marshal(diff)
	release := domain.Doc{"id": domain.ID(), "resource_kind": "saas/hostnames", "resource_id": h["id"], "connection_id": connection, "zone_id": zone, "zone_name": canonical(domain.S(h, "dns_zone_name")), "hostname": want.Name, "status": "pending", "trigger": "manual", "migration": true, "migration_config_fingerprint": hash(migrationConfig(h, z)), "before": before, "desired": desired, "diff": diff, "diff_json": string(encoded), "previous_owned_records": previousOwned, "desired_fingerprint": hash(desired), "applied_changes": []provider.Change{}}
	versions, e := s.Store.List(ctx, "releases")
	if e != nil {
		return nil, e
	}
	version := 1
	for _, v := range versions {
		if domain.S(v, "resource_kind") == "saas/hostnames" && domain.S(v, "resource_id") == domain.S(h, "id") && int(domain.N(v, "version")) >= version {
			version = int(domain.N(v, "version")) + 1
		}
	}
	release["version"] = version
	return release, s.Store.Put(ctx, "releases", release)
}

func (s *Service) validateMigration(ctx context.Context, release domain.Doc) error {
	h, e := s.Store.Get(ctx, "saas/hostnames", domain.S(release, "resource_id"))
	if e != nil {
		return e
	}
	z, e := s.Store.Get(ctx, "saas/zones", domain.S(h, "saas_zone_id"))
	if e != nil {
		return e
	}
	if hash(migrationConfig(h, z)) != domain.S(release, "migration_config_fingerprint") {
		return errors.New("SaaS or DNS configuration changed after preview; review a fresh migration diff")
	}
	_, want, e := s.migrationDesired(ctx, h, z)
	if e != nil {
		return e
	}
	if !sameRecords([]provider.Record{want}, records(release, "desired"), false) {
		return errors.New("SaaS target capability changed; review a fresh migration diff")
	}
	return nil
}
