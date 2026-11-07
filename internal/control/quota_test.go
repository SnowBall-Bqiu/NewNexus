package control

import (
	"context"
	"testing"

	"github.com/newnexus/newnexus/internal/domain"
)

func TestCloudflareEnforcesQuotaAndWritePermissionWithoutFakeSuccess(t *testing.T) {
	s, state, c := fixture(t)
	ctx := context.Background()
	z := zone(t, s, c)
	state.hostReject = 403
	h := put(t, s, "saas/hostnames", domain.Doc{"saas_zone_id": z["id"], "hostname": "customer.example.com", "validation_method": "txt"})
	if _, e := s.Run(ctx, "create", "saas/hostnames", h); e == nil {
		t.Fatal("provider quota/permission rejection reported success")
	}
	saved, e := s.Store.Get(ctx, "saas/hostnames", domain.S(h, "id"))
	if e != nil || domain.S(saved, "provider_id") != "" || domain.B(saved, "creation_uncertain") || domain.S(saved, "status") != "provisioning_failed" {
		t.Fatal("definitive permission rejection fabricated or hid provisioning state", saved, e)
	}
	z, _ = s.Store.Get(ctx, "saas/zones", domain.S(z, "id"))
	result, e := s.Run(ctx, "sync", "saas/zones", z)
	if e != nil || domain.S(result, "quota_status") != "over_allocation" || domain.N(result, "quota_hard_cap") != 2 {
		t.Fatal("soft quota should be reported without guessing Enterprise enforcement", result, e)
	}
}
