package provider

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/newnexus/newnexus/internal/domain"
)

func TestCustomOriginCapabilitiesRequireActualSaaSAccess(t *testing.T) {
	for _, allowed := range []bool{true, false} {
		a := testAdapter(t, "cloudflare", func(w http.ResponseWriter, r *http.Request) {
			if !allowed {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"success":false,"errors":[{"code":10000}]}`))
				return
			}
			cfJSON(w, domain.Doc{"allocated": 100, "used": 0})
		})
		capability, err := a.ZoneCapabilities(context.Background(), "saas")
		if err != nil || capability.SaaS != allowed || capability.CustomOrigin != allowed {
			t.Fatalf("allowed=%v capability=%+v err=%v", allowed, capability, err)
		}
	}
}

func TestCustomOriginRequiresSameAccountAndExplicitProxiedRecord(t *testing.T) {
	for _, scenario := range []struct {
		name, account              string
		proxied, wildcard, allowed bool
	}{
		{"same-account-proxied", "a", true, false, true},
		{"different-account", "b", true, false, false},
		{"dns-only", "a", false, false, false},
		{"wildcard", "a", true, true, false},
		{"unknown-account", "", true, false, false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			a := testAdapter(t, "cloudflare", func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/zones" {
					cfJSON(w, []domain.Doc{{"id": "saas", "name": "saas.example", "account": domain.Doc{"id": "a"}}, {"id": "origin", "name": "origin.example", "account": domain.Doc{"id": scenario.account}}})
					return
				}
				if strings.Contains(r.URL.Path, "/dns_records") {
					name := "app.origin.example"
					if scenario.wildcard {
						name = "*.origin.example"
					}
					cfJSON(w, []domain.Doc{{"id": "record", "type": "A", "name": name, "content": "1.1.1.1", "proxied": scenario.proxied}})
					return
				}
				t.Fatalf("unexpected provider request %s", r.URL.Path)
			})
			err := a.ValidateCustomOrigin(context.Background(), "saas", "app.origin.example")
			if (err == nil) != scenario.allowed {
				t.Fatalf("allowed=%v err=%v", scenario.allowed, err)
			}
		})
	}
}
