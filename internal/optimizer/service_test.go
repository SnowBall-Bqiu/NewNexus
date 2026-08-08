package optimizer

import (
	"net/netip"
	"testing"
)

func TestSamplePrefixBoundsIPv6Enumeration(t *testing.T) {
	prefix := netip.MustParsePrefix("2606:4700::/32")
	addresses := samplePrefix(prefix, 32)
	if len(addresses) != 32 {
		t.Fatalf("expected 32 samples, got %d", len(addresses))
	}
	for _, address := range addresses {
		if !prefix.Contains(address) || !address.Is6() {
			t.Fatalf("invalid IPv6 sample %s", address)
		}
	}
}

func TestSamplePrefixRejectsPrivateAddresses(t *testing.T) {
	if addresses := samplePrefix(netip.MustParsePrefix("10.0.0.0/8"), 16); len(addresses) != 0 {
		t.Fatalf("private prefix produced %d candidates", len(addresses))
	}
}

func TestClamp(t *testing.T) {
	if clamp(-1, 0, 100) != 0 || clamp(101, 0, 100) != 100 || clamp(42, 0, 100) != 42 {
		t.Fatal("score clamp violated its bounds")
	}
}
