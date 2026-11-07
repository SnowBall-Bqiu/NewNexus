// Package control coordinates external desired/observed state and compensated DNS publications.
package control

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/newnexus/newnexus/internal/domain"
	"github.com/newnexus/newnexus/internal/provider"
	"github.com/newnexus/newnexus/internal/security"
	"github.com/newnexus/newnexus/internal/store"
)

type Service struct {
	Store *store.Store
	Vault *security.Vault
	// These seams permit contract tests with real HTTP/DNS test servers, never
	// production bypasses supplied through stored connection credentials.
	Adapter func(string, domain.Doc) *provider.Adapter
	Resolve func(context.Context, string) ([]netip.Addr, error)
	Verify  func(context.Context, string, []provider.Record) error
	HTTP    *http.Client
}

func New(s *store.Store, v *security.Vault) *Service {
	return &Service{Store: s, Vault: v, Adapter: provider.New, Resolve: security.ResolvePublic, Verify: VerifyAuthoritative, HTTP: security.SafeClient(15 * time.Second)}
}

func (s *Service) Run(ctx context.Context, action, kind string, d domain.Doc) (domain.Doc, error) {
	if kind == "routes" {
		kind = "rules"
	}
	var result domain.Doc
	var err error
	switch kind {
	case "connections":
		if action == "validate" || action == "sync" {
			result, err = s.connection(ctx, d)
		} else {
			err = errors.New("unsupported connection action")
		}
	case "saas/zones":
		if action == "sync" || action == "create" {
			result, err = s.saasZone(ctx, action, d)
		} else {
			err = errors.New("unsupported SaaS zone action")
		}
	case "saas/hostnames":
		result, err = s.saasHostname(ctx, action, d)
	case "rules", "optimized-hostnames":
		if action == "preview" {
			result, err = s.preview(ctx, kind, d)
		} else {
			err = errors.New("a DNS preview is required before publication")
		}
	case "releases":
		if action == "apply" || action == "rollback" {
			result, err = s.publish(ctx, action, d)
		} else {
			err = errors.New("unsupported release action")
		}
	default:
		err = fmt.Errorf("unsupported control resource %q", kind)
	}
	if err != nil {
		return result, fmt.Errorf("%s %s: %w", kind, action, err)
	}
	return public(result), nil
}

func public(d domain.Doc) domain.Doc {
	if d == nil {
		return nil
	}
	out := domain.Clone(d)
	for _, key := range []string{"credentials", "credentials_encrypted", "_version"} {
		delete(out, key)
	}
	return out
}
func doc(value any) domain.Doc {
	b, _ := json.Marshal(value)
	out := domain.Doc{}
	_ = json.Unmarshal(b, &out)
	return out
}
func hash(value any) string {
	b, _ := json.Marshal(value)
	v := sha256.Sum256(b)
	return hex.EncodeToString(v[:])
}
func canonical(host string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
}
func validHostname(host string) bool {
	if len(host) > 253 || host == "" || strings.ContainsAny(host, "/:@* \\_\t\r\n") {
		return false
	}
	if _, e := netip.ParseAddr(host); e == nil {
		return false
	}
	parts := strings.Split(host, ".")
	if len(parts) < 2 {
		return false
	}
	for _, label := range parts {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if r != '-' && (r < 'a' || r > 'z') && (r < '0' || r > '9') {
				return false
			}
		}
	}
	return true
}
func (s *Service) adapter(ctx context.Context, id string) (domain.Doc, *provider.Adapter, error) {
	c, e := s.Store.Get(ctx, "connections", id)
	if e != nil {
		return nil, nil, e
	}
	if s.Vault == nil {
		return nil, nil, errors.New("credential vault is unavailable")
	}
	raw, e := s.Vault.Open(domain.S(c, "credentials_encrypted"), "connection:"+id)
	if e != nil {
		return nil, nil, errors.New("cannot decrypt connection credentials")
	}
	credentials := domain.Doc{}
	if e = json.Unmarshal(raw, &credentials); e != nil {
		return nil, nil, errors.New("stored credential format is invalid")
	}
	return c, s.Adapter(domain.S(c, "provider"), credentials), nil
}

func (s *Service) connection(ctx context.Context, c domain.Doc) (domain.Doc, error) {
	_, a, e := s.adapter(ctx, domain.S(c, "id"))
	if e != nil {
		return nil, e
	}
	zones, e := a.Zones(ctx)
	if e != nil {
		c["status"] = "error"
		c["status_message"] = security.Redact(e.Error())
		_ = s.Store.Put(ctx, "connections", c)
		return nil, e
	}
	aggregate := provider.Capabilities{Apex: a.Provider == "cloudflare"}
	caps := domain.Doc{}
	lineMap := domain.Doc{}
	capErrors := domain.Doc{}
	for _, z := range zones {
		cap, err := a.ZoneCapabilities(ctx, z.ID)
		if err != nil {
			capErrors[z.ID] = security.Redact(err.Error())
			continue
		}
		caps[z.ID] = doc(cap)
		aggregate.Lines = aggregate.Lines || cap.Lines
		aggregate.Weights = aggregate.Weights || cap.Weights
		aggregate.SaaS = aggregate.SaaS || cap.SaaS
		aggregate.CustomOrigin = aggregate.CustomOrigin || cap.CustomOrigin
		lines, err := a.Lines(ctx, z.ID)
		if err != nil {
			capErrors[z.ID] = security.Redact(err.Error())
		} else {
			lineMap[z.ID] = lines
		}
	}
	c["zones"] = zones
	c["zone_capabilities"] = caps
	c["lines"] = lineMap
	c["capability_errors"] = capErrors
	c["capabilities"] = doc(aggregate)
	c["status"] = "valid"
	c["last_validated_at"] = domain.Now()
	c["status_message"] = ""
	if len(capErrors) > 0 {
		c["status"] = "limited"
		c["status_message"] = "Some zone capabilities could not be verified; unsupported operations remain disabled"
	}
	return c, s.Store.Put(ctx, "connections", c)
}

func (s *Service) alert(ctx context.Context, category, kind, id, message string) {
	alerts, e := s.Store.List(ctx, "alerts")
	if e != nil {
		return
	}
	for _, a := range alerts {
		if domain.S(a, "category") == category && domain.S(a, "resource_type") == kind && domain.S(a, "resource_id") == id && domain.S(a, "status") == "open" {
			a["occurrences"] = domain.N(a, "occurrences") + 1
			a["last_occurred_at"] = domain.Now()
			a["message"] = security.Redact(message)
			_ = s.Store.Put(ctx, "alerts", a)
			return
		}
	}
	_ = s.Store.Put(ctx, "alerts", domain.Doc{"category": category, "resource_type": kind, "resource_id": id, "title": "DNS automation requires attention", "message": security.Redact(message), "severity": "critical", "status": "open", "occurrences": 1, "last_occurred_at": domain.Now()})
}

func (s *Service) locked(ctx context.Context, key string) (context.Context, func(), error) {
	owner := domain.ID()
	ok, e := s.Store.Lease(ctx, "control:"+key, owner, 5*time.Minute)
	if e != nil {
		return nil, nil, e
	}
	if !ok {
		return nil, nil, errors.New("another operation holds the DNS zone lock")
	}
	active, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(30 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-active.Done():
				return
			case <-tick.C:
				ok, e := s.Store.Lease(active, "control:"+key, owner, 5*time.Minute)
				if e != nil || !ok {
					cancel()
					return
				}
			}
		}
	}()
	return active, func() { cancel(); <-done; s.Store.ReleaseLease(context.Background(), "control:"+key, owner) }, nil
}
