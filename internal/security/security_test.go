package security

import (
	"bytes"
	"net/netip"
	"testing"
)

func TestVaultBindsCredentialsToConnection(t *testing.T) {
	v, e := NewVault(bytes.Repeat([]byte{7}, 32))
	if e != nil {
		t.Fatal(e)
	}
	sealed, e := v.Seal([]byte("secret"), "connection:a")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = v.Open(sealed, "connection:b"); e == nil {
		t.Fatal("ciphertext moved between resources")
	}
	plain, e := v.Open(sealed, "connection:a")
	if e != nil || string(plain) != "secret" {
		t.Fatal(string(plain), e)
	}
}
func TestReservedAndMetadataAddressesBlocked(t *testing.T) {
	for _, address := range []string{"127.0.0.1", "10.0.0.1", "172.16.0.1", "192.168.0.1", "169.254.169.254", "100.100.100.200", "0.0.0.0", "192.0.2.1", "198.18.0.1", "224.0.0.1", "::1", "::ffff:127.0.0.1", "fc00::1", "fe80::1", "2001:db8::1", "64:ff9b::7f00:1"} {
		if PublicAddress(netip.MustParseAddr(address)) {
			t.Errorf("allowed %s", address)
		}
	}
	for _, address := range []string{"1.1.1.1", "8.8.8.8", "2606:4700:4700::1111"} {
		if !PublicAddress(netip.MustParseAddr(address)) {
			t.Errorf("blocked public %s", address)
		}
	}
}
func TestProbeURLPolicy(t *testing.T) {
	for _, value := range []string{"file:///etc/passwd", "http://user:pass@example.com", "https://example.com:65536", "https://localhost/", "http://169.254.169.254/", "//example.com"} {
		if _, e := ValidateURL(value); e == nil {
			t.Errorf("allowed %s", value)
		}
	}
}
