package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"strings"
	"time"

	"github.com/newnexus/newnexus/internal/domain"
	"github.com/newnexus/newnexus/internal/provider"
	"github.com/newnexus/newnexus/internal/routing"
	"github.com/newnexus/newnexus/internal/security"
)

func records(d domain.Doc, key string) []provider.Record {
	b, _ := json.Marshal(d[key])
	out := []provider.Record{}
	_ = json.Unmarshal(b, &out)
	return out
}
func changes(d domain.Doc, key string) []provider.Change {
	b, _ := json.Marshal(d[key])
	out := []provider.Change{}
	_ = json.Unmarshal(b, &out)
	return out
}
func lineCode(v string) string {
	if v == "" || v == "0" {
		return "default"
	}
	return v
}
func recordKey(r provider.Record) string {
	return canonical(r.Name) + "|" + r.Type + "|" + lineCode(r.Line) + "|" + canonical(r.Value)
}
func normalizedRecord(r provider.Record) provider.Record {
	r.Name = canonical(r.Name)
	if r.Type == "CNAME" {
		r.Value = canonical(r.Value)
	}
	r.Line = lineCode(r.Line)
	if r.Weight <= 0 {
		r.Weight = 1
	}
	return r
}
func sameRecord(a, b provider.Record, withID bool) bool {
	a = normalizedRecord(a)
	b = normalizedRecord(b)
	if !withID {
		a.ID = ""
		b.ID = ""
	}
	return a == b
}
func sameRecords(a, b []provider.Record, withID bool) bool {
	if len(a) != len(b) {
		return false
	}
	used := make([]bool, len(b))
	for _, r := range a {
		found := false
		for i, v := range b {
			if !used[i] && sameRecord(r, v, withID) {
				used[i] = true
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func (s *Service) preview(ctx context.Context, kind string, d domain.Doc) (domain.Doc, error) {
	connection := domain.S(d, "dns_connection_id")
	zone := domain.S(d, "zone_id")
	zoneName := canonical(domain.S(d, "zone_name"))
	if connection == "" || zone == "" || zoneName == "" {
		return nil, errors.New("DNS connection, zone ID and zone name are required")
	}
	_, a, e := s.adapter(ctx, connection)
	if e != nil {
		return nil, e
	}
	desired, e := s.desired(ctx, kind, d, a)
	if e != nil {
		return nil, e
	}
	return s.makeRelease(ctx, kind, domain.S(d, "id"), connection, zone, zoneName, desired, false)
}

func (s *Service) desired(ctx context.Context, kind string, d domain.Doc, a *provider.Adapter) ([]provider.Record, error) {
	zone := domain.S(d, "zone_id")
	zoneName := canonical(domain.S(d, "zone_name"))
	host := canonical(domain.S(d, "hostname"))
	if !validHostname(host) || host != zoneName && !strings.HasSuffix(host, "."+zoneName) {
		return nil, errors.New("hostname must belong to the configured DNS zone")
	}
	zones, e := a.Zones(ctx)
	if e != nil {
		return nil, e
	}
	found := false
	for _, z := range zones {
		if z.ID == zone && canonical(z.Name) == zoneName {
			found = true
		}
	}
	if !found {
		return nil, errors.New("DNS zone ID/name is not accessible through this connection")
	}
	cap, e := a.ZoneCapabilities(ctx, zone)
	if e != nil {
		return nil, e
	}
	ttl := int(domain.N(d, "ttl"))
	if ttl <= 0 {
		ttl = 300
	}
	if ttl < cap.MinTTL && !(a.Provider == "cloudflare" && ttl == 1) {
		return nil, fmt.Errorf("TTL must be at least %d seconds for this DNS zone", cap.MinTTL)
	}
	if domain.B(d, "proxied") {
		return nil, errors.New("managed CNAME and optimized A/AAAA records must use DNS-only")
	}
	if kind == "optimized-hostnames" {
		return s.optimized(ctx, d, host, ttl)
	}
	if e = routing.Validate(d, cap); e != nil {
		return nil, e
	}
	supported, e := a.Lines(ctx, zone)
	if e != nil {
		return nil, e
	}
	supportedCodes := map[string]bool{}
	for _, l := range supported {
		supportedCodes[lineCode(l.Code)] = true
	}
	desired := []provider.Record{}
	edges := map[string][]string{}
	rules, e := s.Store.List(ctx, "rules")
	if e != nil {
		return nil, e
	}
	for _, r := range rules {
		if domain.S(r, "id") == domain.S(d, "id") {
			continue
		}
		for _, l := range domain.A(r, "lines") {
			for _, target := range domain.A(l, "targets") {
				if domain.B(target, "enabled") {
					edges[canonical(domain.S(r, "hostname"))] = append(edges[canonical(domain.S(r, "hostname"))], canonical(domain.S(target, "cname")))
				}
			}
		}
	}
	lines := domain.A(d, "lines")
	if len(lines) == 0 {
		return nil, errors.New("at least one DNS line with a healthy target is required")
	}
	saas, e := s.Store.List(ctx, "saas/hostnames")
	if e != nil {
		return nil, e
	}
	for _, l := range lines {
		code := lineCode(domain.S(l, "provider_line_code"))
		if !supportedCodes[code] {
			return nil, fmt.Errorf("DNS line %q is not supported by the current provider plan", code)
		}
		active := int(domain.N(l, "active_priority"))
		priorityExists := false
		best := int(^uint(0) >> 1)
		for _, target := range domain.A(l, "targets") {
			if domain.B(target, "enabled") && domain.S(target, "health") == "healthy" {
				p := int(domain.N(target, "priority"))
				if p == active {
					priorityExists = true
				}
				if p < best {
					best = p
				}
			}
		}
		if !priorityExists {
			active = best
		}
		if best == int(^uint(0)>>1) {
			return nil, errors.New("all targets are unhealthy or unprobed; existing DNS will be preserved")
		}
		selected := 0
		for _, target := range domain.A(l, "targets") {
			if !domain.B(target, "enabled") || domain.S(target, "health") != "healthy" || int(domain.N(target, "priority")) != active {
				continue
			}
			cname := canonical(domain.S(target, "cname"))
			if !validHostname(cname) {
				return nil, errors.New("CNAME targets must be valid public hostnames")
			}
			if _, e = s.Resolve(ctx, cname); e != nil {
				return nil, fmt.Errorf("CNAME target %s does not resolve publicly: %w", cname, e)
			}
			for _, h := range saas {
				if canonical(domain.S(h, "hostname")) == cname && (domain.S(h, "status") != "active" || domain.S(h, "ssl_status") != "active") {
					return nil, errors.New("referenced SaaS hostname and certificate must both be active")
				}
			}
			edges[host] = append(edges[host], cname)
			weight := int(domain.N(target, "weight"))
			if weight < 1 {
				weight = 1
			}
			desired = append(desired, provider.Record{Name: host, Type: "CNAME", Value: cname, TTL: ttl, Line: code, Weight: weight, Proxied: false})
			selected++
		}
		if selected > 1 && !cap.Weights {
			return nil, errors.New("the provider has not confirmed weighted CNAME capability; duplicate records cannot simulate weights")
		}
	}
	if e = routing.DetectLoop(host, edges); e != nil {
		return nil, e
	}
	return desired, nil
}

func (s *Service) optimized(ctx context.Context, d domain.Doc, host string, ttl int) ([]provider.Record, error) {
	candidates, e := s.Store.List(ctx, "candidates")
	if e != nil {
		return nil, e
	}
	family := int(domain.N(d, "family"))
	if family != 4 && family != 6 {
		return nil, errors.New("optimized hostname IP family must be 4 or 6")
	}
	top := int(domain.N(d, "top_n"))
	if top < 1 || top > 100 {
		return nil, errors.New("top N must be between 1 and 100")
	}
	minimum := 3.
	rate := .8
	freshness := 15 * time.Minute
	if setting, e := s.Store.Get(ctx, "settings", "probe.min_samples"); e == nil && domain.N(setting, "value") > 0 {
		minimum = domain.N(setting, "value")
	}
	if setting, e := s.Store.Get(ctx, "settings", "probe.minimum_success_rate"); e == nil {
		rate = domain.N(setting, "value")
	}
	if setting, e := s.Store.Get(ctx, "settings", "probe.freshness_seconds"); e == nil && domain.N(setting, "value") > 0 {
		freshness = time.Duration(domain.N(setting, "value")) * time.Second
	}
	selected := []domain.Doc{}
	pending := 0
	for _, candidate := range candidates {
		if domain.S(candidate, "project_id") == domain.S(d, "project_id") && int(domain.N(candidate, "family")) == family && domain.B(candidate, "aggregate_pending") {
			pending++
			continue
		}
		at, _ := time.Parse(time.RFC3339Nano, domain.S(candidate, "last_probed_at"))
		if domain.S(candidate, "project_id") == domain.S(d, "project_id") && int(domain.N(candidate, "family")) == family && domain.B(candidate, "healthy") && domain.N(candidate, "sample_count") >= minimum && domain.N(candidate, "success_rate") >= rate && !at.IsZero() && time.Since(at) <= freshness && time.Until(at) < 2*time.Minute {
			selected = append(selected, candidate)
		}
	}
	sort.Slice(selected, func(i, j int) bool {
		si, sj := domain.N(selected[i], "score"), domain.N(selected[j], "score")
		if si == sj {
			return domain.S(selected[i], "address") < domain.S(selected[j], "address")
		}
		return si > sj
	})
	if len(selected) < top {
		if pending > 0 {
			return nil, fmt.Errorf("candidate scores are being recomputed (%d pending); only %d qualify for top N %d, retry after the aggregation job completes", pending, len(selected), top)
		}
		return nil, fmt.Errorf("only %d fresh, healthy candidates qualify; top N is %d", len(selected), top)
	}
	out := []provider.Record{}
	seen := map[string]bool{}
	for _, c := range selected {
		address := domain.S(c, "address")
		ip, e := netip.ParseAddr(address)
		if e != nil || !security.PublicAddress(ip) || ip.Unmap().Is4() != (family == 4) {
			continue
		}
		if seen[address] {
			continue
		}
		seen[address] = true
		typ := "A"
		if family == 6 {
			typ = "AAAA"
		}
		out = append(out, provider.Record{Name: host, Type: typ, Value: address, TTL: ttl, Line: "default", Weight: 1, Proxied: false})
		if len(out) == top {
			break
		}
	}
	if len(out) != top {
		return nil, errors.New("not enough distinct public candidate IPs qualify")
	}
	return out, nil
}

func scopeRows(all []provider.Record, desired []provider.Record) []provider.Record {
	names := map[string]bool{}
	types := map[string]bool{}
	for _, r := range desired {
		names[canonical(r.Name)] = true
		types[r.Type] = true
	}
	out := []provider.Record{}
	for _, r := range all {
		if names[canonical(r.Name)] && types[r.Type] {
			out = append(out, normalizedRecord(r))
		}
	}
	return out
}

func (s *Service) makeRelease(ctx context.Context, kind, id, connection, zone, zoneName string, desired []provider.Record, allowExisting bool) (domain.Doc, error) {
	_, a, e := s.adapter(ctx, connection)
	if e != nil {
		return nil, e
	}
	all, e := a.Records(ctx, zone)
	if e != nil {
		return nil, e
	}
	scopeSeed := append([]provider.Record{}, desired...)
	if kind == "optimized-hostnames" {
		scopeSeed = append(scopeSeed, provider.Record{Name: desired[0].Name, Type: "A"}, provider.Record{Name: desired[0].Name, Type: "AAAA"})
	}
	before := scopeRows(all, scopeSeed)
	owned, e := s.owned(ctx, kind, id, connection, zone)
	if e != nil {
		return nil, e
	}
	ownedByID := map[string]provider.Record{}
	for _, v := range owned {
		for _, r := range records(v, "records") {
			ownedByID[r.ID] = r
		}
	}
	filtered := []provider.Record{}
	for _, want := range desired {
		externalMatch := false
		for _, have := range all {
			if canonical(have.Name) != canonical(want.Name) {
				continue
			}
			_, managed := ownedByID[have.ID]
			if !managed {
				if allowExisting && sameRecord(have, want, false) {
					externalMatch = true
					continue
				}
				if have.Type == want.Type || have.Type == "CNAME" || want.Type == "CNAME" && have.Type != "NS" && have.Type != "SOA" {
					return nil, fmt.Errorf("DNS ownership conflict at %s: existing record is not managed by this resource", want.Name)
				}
			}
		}
		if !externalMatch {
			filtered = append(filtered, want)
		}
	}
	if len(filtered) == 0 {
		return domain.Doc{"status": "unchanged", "diff": []provider.Change{}}, nil
	}
	desired = filtered
	before = scopeRows(before, scopeSeed)
	for _, have := range before {
		prior, managed := ownedByID[have.ID]
		if !managed || !sameRecord(prior, have, true) {
			s.alert(ctx, "external_drift", kind, id, "Managed DNS was changed externally; publishing is paused")
			return nil, errors.New("managed DNS drift detected; refusing to overwrite external edits")
		}
	}
	for _, prior := range ownedByID {
		if len(scopeRows([]provider.Record{prior}, scopeSeed)) == 0 {
			continue
		}
		found := false
		for _, have := range before {
			if sameRecord(prior, have, true) {
				found = true
			}
		}
		if !found {
			return nil, errors.New("a managed DNS record was removed externally; reconcile before publishing")
		}
	}
	diff := recordDiff(before, desired)
	release := domain.Doc{"id": domain.ID(), "resource_kind": kind, "resource_id": id, "connection_id": connection, "zone_id": zone, "zone_name": zoneName, "hostname": desired[0].Name, "status": "pending", "trigger": "manual", "before": before, "desired": desired, "diff": diff, "desired_fingerprint": hash(desired), "applied_changes": []provider.Change{}}
	encoded, _ := json.Marshal(diff)
	release["diff_json"] = string(encoded)
	if kind == "rules" {
		release["rule_id"] = id
		release["route_id"] = id
	}
	versions, e := s.Store.List(ctx, "releases")
	if e != nil {
		return nil, e
	}
	version := 1
	for _, v := range versions {
		if domain.S(v, "resource_kind") == kind && domain.S(v, "resource_id") == id && int(domain.N(v, "version")) >= version {
			version = int(domain.N(v, "version")) + 1
		}
	}
	release["version"] = version
	return release, s.Store.Put(ctx, "releases", release)
}

// Prefer an in-place update for one-to-one target changes. Remaining removals
// precede creates so providers enforcing CNAME exclusivity can accept the batch.
func recordDiff(before, desired []provider.Record) []provider.Change {
	left := append([]provider.Record(nil), before...)
	after := []provider.Record{}
	out := []provider.Change{}
	for _, want := range desired {
		match := -1
		for i, have := range left {
			if recordKey(have) == recordKey(want) {
				match = i
				break
			}
		}
		if match >= 0 {
			have := left[match]
			left = append(left[:match], left[match+1:]...)
			if !sameRecord(have, want, false) {
				want.ID = have.ID
				b, w := have, want
				out = append(out, provider.Change{Action: "update", Name: w.Name, Type: w.Type, Line: w.Line, Before: &b, After: &w})
			}
		} else {
			after = append(after, want)
		}
	}
	remaining := []provider.Record{}
	for _, want := range after {
		match := -1
		for i, have := range left {
			if canonical(have.Name) == canonical(want.Name) && have.Type == want.Type && lineCode(have.Line) == lineCode(want.Line) {
				match = i
				break
			}
		}
		if match >= 0 {
			have := left[match]
			left = append(left[:match], left[match+1:]...)
			want.ID = have.ID
			b, w := have, want
			out = append(out, provider.Change{Action: "update", Name: w.Name, Type: w.Type, Line: w.Line, Before: &b, After: &w})
		} else {
			remaining = append(remaining, want)
		}
	}
	for _, r := range left {
		b := r
		out = append(out, provider.Change{Action: "delete", Name: r.Name, Type: r.Type, Line: r.Line, Before: &b})
	}
	for _, r := range remaining {
		w := r
		out = append(out, provider.Change{Action: "create", Name: r.Name, Type: r.Type, Line: r.Line, After: &w})
	}
	return out
}
