package control

import (
	"context"
	"testing"

	"github.com/newnexus/newnexus/internal/domain"
	"github.com/newnexus/newnexus/internal/provider"
)

func TestCustomOriginChecksProxiedRecordBeforeCreatingHostname(t *testing.T) {
	for _, proxied := range []bool{false, true} {
		s, remote, connection := fixture(t)
		z := zone(t, s, connection)
		remote.rows = []provider.Record{{ID: "origin-record", Type: "A", Name: "origin.example.com", Value: "1.1.1.1", Proxied: proxied}}
		h := put(t, s, "saas/hostnames", domain.Doc{"saas_zone_id": z["id"], "hostname": "customer.example.com", "custom_origin": "origin.example.com", "validation_method": "txt", "automation_mode": "manual"})
		_, err := s.Run(context.Background(), "create", "saas/hostnames", h)
		if !proxied {
			if err == nil || remote.hostCreates != 0 {
				t.Fatalf("DNS-only origin reached mutation: creates=%d err=%v", remote.hostCreates, err)
			}
		} else if err != nil || remote.hostCreates != 1 || domain.S(remote.createdPayload, "custom_origin_server") != "origin.example.com" {
			t.Fatalf("valid custom origin not forwarded: creates=%d err=%v", remote.hostCreates, err)
		}
	}
}
