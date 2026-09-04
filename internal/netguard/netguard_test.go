package netguard

import (
	"net/netip"
	"testing"
)

func TestValidateAddressRejectsInternalAndMetadataRanges(t *testing.T) {
	for _, raw := range []string{"127.0.0.1", "10.0.0.1", "169.254.169.254", "::1", "fe80::1", "2001:db8::1"} {
		if err := ValidateAddress(netip.MustParseAddr(raw)); err == nil {
			t.Fatalf("unsafe address %s was accepted", raw)
		}
	}
	if err := ValidateAddress(netip.MustParseAddr("1.1.1.1")); err != nil {
		t.Fatalf("public address rejected: %v", err)
	}
}
