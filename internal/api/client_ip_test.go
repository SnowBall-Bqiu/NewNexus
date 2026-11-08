package api

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClientIPTrustsOnlyConfiguredProxySuffix(t *testing.T) {
	proxies, err := ParseTrustedProxies("127.0.0.1/32, ::1, 10.0.0.0/24")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{TrustedProxies: proxies}
	for _, tt := range []struct {
		name, remote, forwarded, want string
	}{
		{"direct ignores spoofed header", "198.51.100.1:1234", "203.0.113.1", "198.51.100.1"},
		{"trusted single proxy", "127.0.0.1:1234", "203.0.113.1", "203.0.113.1"},
		{"trusted proxy chain", "127.0.0.1:1234", "203.0.113.1, 10.0.0.2", "203.0.113.1"},
		{"client cannot spoof leftmost IP", "127.0.0.1:1234", "192.0.2.9, 203.0.113.1, 10.0.0.2", "203.0.113.1"},
		{"ignore malformed attacker prefix", "127.0.0.1:1234", "not-an-ip, 203.0.113.1", "203.0.113.1"},
		{"IPv6 client", "[::1]:1234", "2001:db8::7", "2001:db8::7"},
		{"IPv4-mapped peer and client", "[::ffff:127.0.0.1]:1234", "::ffff:203.0.113.1", "203.0.113.1"},
		{"all hops trusted", "127.0.0.1:1234", "10.0.0.3, 10.0.0.2", "10.0.0.3"},
		{"missing header", "127.0.0.1:1234", "", "127.0.0.1"},
		{"malformed nearest hop", "127.0.0.1:1234", "203.0.113.1, invalid", "127.0.0.1"},
		{"scoped IP header", "127.0.0.1:1234", "fe80::1%eth0", "127.0.0.1"},
		{"oversized header", "127.0.0.1:1234", strings.Repeat("1", 4097), "127.0.0.1"},
		{"too many hops", "127.0.0.1:1234", strings.Repeat("10.0.0.2,", 32) + "10.0.0.2", "127.0.0.1"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/api/v1/auth/login", nil)
			r.RemoteAddr = tt.remote
			r.Header.Set("X-Forwarded-For", tt.forwarded)
			if got := s.clientIP(r); got != tt.want {
				t.Fatalf("client IP = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseTrustedProxiesRejectsUnsafeConfiguration(t *testing.T) {
	for _, value := range []string{"*", "0.0.0.0/0", "::/0", "localhost", "127.0.0.1:443", "10.0.0.1/33", "127.0.0.1,", "fe80::1%eth0", "::ffff:127.0.0.1/80", "none,127.0.0.1"} {
		if _, err := ParseTrustedProxies(value); err == nil {
			t.Errorf("accepted unsafe/invalid trusted proxy %q", value)
		}
	}
	proxies, err := ParseTrustedProxies("::ffff:127.0.0.1/128, 192.0.2.17/24")
	if err != nil || len(proxies) != 2 || proxies[0].String() != "127.0.0.1/32" || proxies[1].String() != "192.0.2.0/24" {
		t.Fatalf("normalized proxy configuration = %v, error = %v", proxies, err)
	}
	for _, value := range []string{"", " \t "} {
		proxies, err := ParseTrustedProxies(value)
		if err != nil || len(proxies) != 8 || proxies[0].String() != "127.0.0.1/32" || proxies[1].String() != "::1/128" {
			t.Fatalf("default configuration = %v, error = %v", proxies, err)
		}
	}
}

func TestClientIPDefaultTrustAndExplicitOverrides(t *testing.T) {
	for _, tt := range []struct {
		name, configuration, remote, want string
	}{
		{"default IPv4 loopback", "", "127.0.0.1:1234", "203.0.113.1"},
		{"default IPv6 loopback", "", "[::1]:1234", "203.0.113.1"},
		{"default mapped loopback", "", "[::ffff:127.0.0.1]:1234", "203.0.113.1"},
		{"default excludes other loopback addresses", "", "127.0.0.2:1234", "127.0.0.2"},
		{"default excludes private peers", "", "10.0.0.1:1234", "10.0.0.1"},
		{"default Docker bridge gateway", "", "172.17.0.1:1234", "203.0.113.1"},
		{"default Docker Compose gateway", "", "172.18.0.1:1234", "203.0.113.1"},
		{"default Docker container proxy", "", "172.19.0.2:1234", "203.0.113.1"},
		{"default Docker pool 172.20", "", "172.20.0.2:1234", "203.0.113.1"},
		{"default Docker pool 172.24", "", "172.24.0.2:1234", "203.0.113.1"},
		{"default Docker pool 172.28", "", "172.28.0.2:1234", "203.0.113.1"},
		{"default Docker range upper bound", "", "172.31.255.254:1234", "203.0.113.1"},
		{"default mapped Docker peer", "", "[::ffff:172.18.0.1]:1234", "203.0.113.1"},
		{"blank configuration uses Docker defaults", " \t ", "172.18.0.1:1234", "203.0.113.1"},
		{"default excludes preceding range", "", "172.16.255.254:1234", "172.16.255.254"},
		{"default excludes following range", "", "172.32.0.1:1234", "172.32.0.1"},
		{"default excludes LAN peers", "", "192.168.1.1:1234", "192.168.1.1"},
		{"default excludes IPv6 private peers", "", "[fd00::1]:1234", "fd00::1"},
		{"default excludes public peers", "", "198.51.100.1:1234", "198.51.100.1"},
		{"custom list trusts configured peers", "10.0.0.0/24", "10.0.0.1:1234", "203.0.113.1"},
		{"custom list replaces IPv4 default", "10.0.0.0/24", "127.0.0.1:1234", "127.0.0.1"},
		{"custom list replaces IPv6 default", "10.0.0.0/24", "[::1]:1234", "::1"},
		{"custom list replaces Docker default", "10.0.0.0/24", "172.18.0.1:1234", "172.18.0.1"},
		{"none disables IPv4 trust", "none", "127.0.0.1:1234", "127.0.0.1"},
		{"none disables IPv6 trust", " NoNe \t", "[::1]:1234", "::1"},
		{"none disables Docker trust", "none", "172.18.0.1:1234", "172.18.0.1"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			proxies, err := ParseTrustedProxies(tt.configuration)
			if err != nil {
				t.Fatal(err)
			}
			s := &Server{TrustedProxies: proxies}
			r := httptest.NewRequest("POST", "/api/v1/auth/login", nil)
			r.RemoteAddr = tt.remote
			r.Header.Set("X-Forwarded-For", "203.0.113.1")
			if got := s.clientIP(r); got != tt.want {
				t.Fatalf("client IP = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestClientIPDefaultDockerProxyChain(t *testing.T) {
	proxies, err := ParseTrustedProxies("")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{TrustedProxies: proxies}
	for _, tt := range []struct {
		name, forwarded, want string
	}{
		{"trusted bridge chain", "203.0.113.1, 172.17.0.2", "203.0.113.1"},
		{"client cannot spoof leftmost IP", "192.0.2.9, 203.0.113.1, 172.17.0.2", "203.0.113.1"},
		{"missing header", "", "172.18.0.1"},
		{"malformed nearest hop", "203.0.113.1, invalid", "172.18.0.1"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/api/v1/auth/login", nil)
			r.RemoteAddr = "172.18.0.1:1234"
			r.Header.Set("X-Forwarded-For", tt.forwarded)
			if got := s.clientIP(r); got != tt.want {
				t.Fatalf("client IP = %q, want %q", got, tt.want)
			}
		})
	}
}
