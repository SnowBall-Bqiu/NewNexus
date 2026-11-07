package control

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/newnexus/newnexus/internal/domain"
	"github.com/newnexus/newnexus/internal/provider"
	"github.com/newnexus/newnexus/internal/security"
)

func (s *Service) saasZone(ctx context.Context, action string, z domain.Doc) (domain.Doc, error) {
	_, a, e := s.adapter(ctx, domain.S(z, "connection_id"))
	if e != nil {
		return nil, e
	}
	if a.Provider != "cloudflare" {
		return nil, errors.New("Cloudflare for SaaS requires a Cloudflare connection")
	}
	zone := domain.S(z, "zone_id")
	if zone == "" {
		return nil, errors.New("Cloudflare zone ID is required")
	}
	quota, e := a.Quota(ctx, zone)
	if e != nil {
		z["status"] = "unavailable"
		z["error_message"] = "SaaS quota API permission or plan capability is unavailable"
		_ = s.Store.Put(ctx, "saas/zones", z)
		return nil, e
	}
	z["quota_allocated"] = quota["allocated"]
	z["quota_used"] = quota["used"]
	z["quota_hard_cap"] = quota["hard_cap"]
	z["quota_exceeded"] = quota["exceeded"]
	z["quota_scope"] = quota["scope"]
	z["quota_status"] = "within_allocation"
	z["quota_guidance"] = "Cloudflare is the authoritative quota and permission enforcement point"
	if domain.B(quota, "exceeded") {
		z["quota_status"] = "over_allocation"
		z["quota_guidance"] = "The soft allocation is exceeded; additional hostnames can be billable"
	}
	if domain.N(quota, "hard_cap") > 0 && domain.N(quota, "used") >= domain.N(quota, "hard_cap") {
		z["quota_status"] = "enforcement_threshold_reached"
		z["quota_guidance"] = "Non-Enterprise creation is rejected at this threshold; Enterprise exceptions are decided by Cloudflare"
	}
	var response domain.Doc
	if action == "create" {
		fallback := canonical(domain.S(z, "fallback_origin"))
		if !validHostname(fallback) {
			return nil, errors.New("fallback origin must be a public hostname")
		}
		if _, e = s.Resolve(ctx, fallback); e != nil {
			return nil, fmt.Errorf("fallback origin does not resolve publicly: %w", e)
		}
		response, e = a.SaaS(ctx, http.MethodPut, zone, "custom_hostnames/fallback_origin", domain.Doc{"origin": fallback})
	} else {
		response, e = a.SaaS(ctx, http.MethodGet, zone, "custom_hostnames/fallback_origin", nil)
	}
	if e != nil {
		return nil, e
	}
	remote := domain.M(response, "result")
	z["fallback_status"] = domain.S(remote, "status")
	z["observed_fallback_origin"] = domain.S(remote, "origin")
	z["last_synced_at"] = domain.Now()
	z["status"] = domain.S(remote, "status")
	if z["status"] == "" {
		z["status"] = "pending"
	}
	if action != "create" && canonical(domain.S(remote, "origin")) != canonical(domain.S(z, "fallback_origin")) {
		z["status"] = "drifted"
		s.alert(ctx, "external_drift", "saas/zones", domain.S(z, "id"), "Fallback origin differs from desired configuration; it was not overwritten")
	}
	z["observed"] = remote
	return z, s.Store.Put(ctx, "saas/zones", z)
}

func (s *Service) saasHostname(ctx context.Context, action string, h domain.Doc) (domain.Doc, error) {
	if action == "resume-automation" {
		h["dns_automation_paused"] = false
		h["dns_automation_pause_reason"] = ""
		if e := s.Store.Put(ctx, "saas/hostnames", h); e != nil {
			return h, e
		}
		action = "sync"
	}
	if action != "create" && action != "sync" && action != "activate" && action != "delete" && action != "preview" {
		return nil, errors.New("unsupported custom hostname action")
	}
	z, e := s.Store.Get(ctx, "saas/zones", domain.S(h, "saas_zone_id"))
	if e != nil {
		return nil, e
	}
	_, a, e := s.adapter(ctx, domain.S(z, "connection_id"))
	if e != nil {
		return nil, e
	}
	zone := domain.S(z, "zone_id")
	lockCtx, unlock, e := s.locked(ctx, "saas:"+zone)
	if e != nil {
		return nil, e
	}
	defer unlock()
	ctx = lockCtx
	id := domain.S(h, "provider_id")
	if action == "delete" {
		if e = s.cleanupManaged(ctx, "saas/hostnames", domain.S(h, "id")); e != nil {
			return h, s.pauseHostname(ctx, h, e)
		}
		if id != "" {
			_, e = a.SaaS(ctx, http.MethodDelete, zone, "custom_hostnames/"+url.PathEscape(id), nil)
			if e != nil {
				return h, e
			}
		}
		h["status"] = "deleted"
		if e = s.Store.Delete(ctx, "saas/hostnames", domain.S(h, "id")); e != nil {
			return h, e
		}
		return h, nil
	}
	host := canonical(domain.S(h, "hostname"))
	if !validHostname(host) || host == canonical(domain.S(z, "zone_name")) {
		return nil, errors.New("custom hostname must be valid and different from the SaaS zone name")
	}
	if action == "create" && id == "" {
		if _, e = a.Quota(ctx, zone); e != nil {
			return nil, e
		}
		existing, err := a.CustomHostnames(ctx, zone)
		if err != nil {
			return nil, err
		}
		for _, remote := range existing {
			if canonical(domain.S(remote, "hostname")) == host {
				return nil, errors.New("custom hostname already exists at Cloudflare; refusing to adopt an unowned resource")
			}
		}
		method := domain.S(h, "validation_method")
		if method == "" {
			method = "txt"
		}
		if method != "txt" && method != "http" {
			return nil, errors.New("validation method must be txt or http")
		}
		payload := domain.Doc{"hostname": host, "ssl": domain.Doc{"method": method, "type": "dv", "settings": domain.Doc{"min_tls_version": "1.2"}}}
		if origin := canonical(domain.S(h, "custom_origin")); origin != "" {
			if !validHostname(origin) {
				return nil, errors.New("custom origin must be a hostname, not a URL or IP address")
			}
			if _, e = s.Resolve(ctx, origin); e != nil {
				return nil, e
			}
			if e = a.ValidateCustomOrigin(ctx, zone, origin); e != nil {
				return nil, e
			}
			payload["custom_origin_server"] = origin
		}
		response, err := a.SaaS(ctx, http.MethodPost, zone, "custom_hostnames", payload)
		if err != nil {
			h["error_message"] = security.Redact(err.Error())
			var providerErr *provider.Error
			h["creation_uncertain"] = !errors.As(err, &providerErr) || providerErr.Ambiguous
			h["status"] = "provisioning_failed"
			if providerErr != nil {
				h["provider_error_code"] = providerErr.Code
			}
			_ = s.Store.Put(ctx, "saas/hostnames", h)
			return h, err
		}
		remote := domain.M(response, "result")
		id = domain.S(remote, "id")
		if id == "" {
			return nil, errors.New("Cloudflare did not return the custom hostname ID")
		}
		h["provider_id"] = id
		h["creation_uncertain"] = false
		h["billing_messages"] = response["messages"]
		s.observeHostname(h, remote)
		if e = s.Store.Put(ctx, "saas/hostnames", h); e != nil {
			return h, e
		}
	}
	if id == "" {
		return nil, errors.New("hostname has not been provisioned; create it before syncing")
	}
	response, e := a.SaaS(ctx, http.MethodGet, zone, "custom_hostnames/"+url.PathEscape(id), nil)
	if e != nil {
		return h, e
	}
	remote := domain.M(response, "result")
	if canonical(domain.S(remote, "hostname")) != host {
		return h, errors.New("provider hostname ID does not match the desired hostname")
	}
	s.observeHostname(h, remote)
	checklist := validationChecklist(remote)
	h["validation_records"] = checklist
	// The platform reads ownership and certificate validation separately. It never
	// claims either status active based merely on a locally visible TXT record.
	h["validation_observed"] = s.checkValidation(ctx, checklist)
	if e = s.Store.Put(ctx, "saas/hostnames", h); e != nil {
		return h, e
	}
	if action == "preview" {
		return s.previewMigration(ctx, h, z)
	}
	auto := domain.S(h, "automation_mode") == "auto"
	if auto && !domain.B(h, "dns_automation_paused") {
		records := []provider.Record{}
		for _, item := range checklist {
			typ := domain.S(item, "type")
			if typ == "TXT" || typ == "CNAME" {
				records = append(records, provider.Record{Name: domain.S(item, "name"), Type: typ, Value: domain.S(item, "value"), TTL: 300, Line: "default", Weight: 1})
			}
		}
		if len(records) > 0 {
			if e = s.hostnameDNS(ctx, h, z, records); e != nil {
				return h, s.pauseHostname(ctx, h, e)
			}
		}
	}
	if action == "activate" || auto && !domain.B(h, "dns_automation_paused") && domain.S(h, "status") == "active" && domain.S(h, "ssl_status") == "active" {
		if domain.S(h, "status") != "active" || domain.S(h, "ssl_status") != "active" {
			return h, errors.New("hostname and certificate must both be active before traffic cutover")
		}
		target := canonical(domain.S(z, "cname_target"))
		if !validHostname(target) || target == host {
			return h, errors.New("SaaS CNAME target is missing or creates a loop")
		}
		if _, e = s.Resolve(ctx, target); e != nil {
			return h, e
		}
		if e = s.hostnameDNS(ctx, h, z, []provider.Record{{Name: host, Type: "CNAME", Value: target, TTL: 300, Line: "default", Weight: 1}}); e != nil {
			return h, s.pauseHostname(ctx, h, e)
		}
		h["cutover_at"] = domain.Now()
		h["dns_automation_paused"] = false
		h["dns_automation_pause_reason"] = ""
	}
	return h, s.Store.Put(ctx, "saas/hostnames", h)
}

func (s *Service) pauseHostname(ctx context.Context, h domain.Doc, e error) error {
	h["dns_automation_paused"] = true
	h["dns_automation_pause_reason"] = security.Redact(e.Error())
	_ = s.Store.Put(ctx, "saas/hostnames", h)
	s.alert(ctx, "dns_automation_paused", "saas/hostnames", domain.S(h, "id"), e.Error())
	return e
}
func (s *Service) observeHostname(h, remote domain.Doc) {
	h["status"] = domain.S(remote, "status")
	h["ssl_status"] = domain.S(domain.M(remote, "ssl"), "status")
	h["observed"] = remote
	h["last_checked_at"] = domain.Now()
	ownership := domain.M(remote, "ownership_verification")
	h["validation_name"] = domain.S(ownership, "name")
	h["validation_value"] = domain.S(ownership, "value")
	h["validation_http"] = remote["ownership_verification_http"]
	h["error_message"] = strings.Join(stringArray(remote["verification_errors"]), "; ")
}
func stringArray(v any) []string {
	out := []string{}
	switch values := v.(type) {
	case []any:
		for _, v := range values {
			out = append(out, fmt.Sprint(v))
		}
	case []string:
		return values
	}
	return out
}
func validationChecklist(remote domain.Doc) []domain.Doc {
	out := []domain.Doc{}
	seen := map[string]bool{}
	add := func(d domain.Doc) {
		key := hash(d)
		if !seen[key] {
			seen[key] = true
			out = append(out, d)
		}
	}
	ownership := domain.M(remote, "ownership_verification")
	if domain.S(ownership, "name") != "" {
		add(domain.Doc{"type": "TXT", "name": ownership["name"], "value": ownership["value"], "purpose": "hostname_ownership"})
	}
	ownershipHTTP := domain.M(remote, "ownership_verification_http")
	if domain.S(ownershipHTTP, "http_url") != "" {
		add(domain.Doc{"type": "HTTP", "url": ownershipHTTP["http_url"], "body": ownershipHTTP["http_body"], "purpose": "hostname_ownership"})
	}
	for _, v := range domain.A(domain.M(remote, "ssl"), "validation_records") {
		if domain.S(v, "txt_name") != "" {
			add(domain.Doc{"type": "TXT", "name": v["txt_name"], "value": v["txt_value"], "purpose": "certificate"})
		}
		if domain.S(v, "http_url") != "" {
			add(domain.Doc{"type": "HTTP", "url": v["http_url"], "body": v["http_body"], "purpose": "certificate"})
		}
		if domain.S(v, "cname") != "" && domain.S(v, "cname_target") != "" {
			add(domain.Doc{"type": "CNAME", "name": v["cname"], "value": v["cname_target"], "purpose": "certificate"})
		}
	}
	return out
}
func (s *Service) checkValidation(ctx context.Context, records []domain.Doc) []domain.Doc {
	out := []domain.Doc{}
	for _, r := range records {
		observed := domain.Clone(r)
		ok := false
		switch domain.S(r, "type") {
		case "TXT":
			values, e := net.DefaultResolver.LookupTXT(ctx, domain.S(r, "name"))
			if e == nil {
				for _, value := range values {
					if value == domain.S(r, "value") {
						ok = true
					}
				}
			}
		case "CNAME":
			value, e := net.DefaultResolver.LookupCNAME(ctx, domain.S(r, "name"))
			ok = e == nil && canonical(value) == canonical(domain.S(r, "value"))
		case "HTTP":
			raw := domain.S(r, "url")
			if _, e := security.ValidateURL(raw); e == nil {
				req, e := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
				if e == nil {
					resp, e := s.HTTP.Do(req)
					if e == nil {
						body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
						resp.Body.Close()
						ok = resp.StatusCode == http.StatusOK && strings.TrimSpace(string(body)) == strings.TrimSpace(domain.S(r, "body"))
					}
				}
			}
		}
		observed["verified"] = ok
		out = append(out, observed)
	}
	return out
}

func (s *Service) hostnameDNS(ctx context.Context, h, z domain.Doc, desired []provider.Record) error {
	connection := domain.S(h, "dns_connection_id")
	zone := domain.S(h, "dns_zone_id")
	zoneName := canonical(domain.S(h, "dns_zone_name"))
	if connection == "" || zone == "" || zoneName == "" {
		return errors.New("DNS automation requires a verified DNS connection, zone ID and zone name")
	}
	_, a, e := s.adapter(ctx, connection)
	if e != nil {
		return e
	}
	cap, e := a.ZoneCapabilities(ctx, zone)
	if e != nil {
		return e
	}
	for i := range desired {
		if desired[i].Name != zoneName && !strings.HasSuffix(desired[i].Name, "."+zoneName) {
			return errors.New("validation record is outside the selected DNS zone")
		}
		if desired[i].TTL < cap.MinTTL {
			desired[i].TTL = cap.MinTTL
		}
		if desired[i].Type == "CNAME" && canonical(desired[i].Name) == zoneName && !cap.Apex {
			return errors.New("DNS provider does not support apex CNAME flattening")
		}
	}
	return s.manageIncremental(ctx, "saas/hostnames", domain.S(h, "id"), connection, zone, zoneName, desired)
}
