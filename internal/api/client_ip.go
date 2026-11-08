package api

import (
	"fmt"
	"net/http"
	"net/netip"
	"strings"
)

func defaultTrustedProxies() []netip.Prefix {
	return []netip.Prefix{
		netip.MustParsePrefix("127.0.0.1/32"),
		netip.MustParsePrefix("::1/128"),
		// Common Docker IPv4 address pools, including Compose bridge networks.
		netip.MustParsePrefix("172.17.0.0/16"),
		netip.MustParsePrefix("172.18.0.0/16"),
		netip.MustParsePrefix("172.19.0.0/16"),
		netip.MustParsePrefix("172.20.0.0/14"),
		netip.MustParsePrefix("172.24.0.0/14"),
		netip.MustParsePrefix("172.28.0.0/14"),
	}
}

// ParseTrustedProxies defaults to loopback and common Docker bridge ranges when
// empty. "none" disables trust; an explicit IP/CIDR list replaces the defaults.
func ParseTrustedProxies(value string) ([]netip.Prefix, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return defaultTrustedProxies(), nil
	}
	if strings.EqualFold(value, "none") {
		return nil, nil
	}
	var proxies []netip.Prefix
	for _, entry := range strings.Split(value, ",") {
		entry = strings.TrimSpace(entry)
		prefix, err := netip.ParsePrefix(entry)
		if err != nil {
			address, parseErr := netip.ParseAddr(entry)
			if parseErr != nil || address.Zone() != "" {
				return nil, fmt.Errorf("invalid trusted proxy IP or CIDR %q", entry)
			}
			prefix = netip.PrefixFrom(address, address.BitLen())
		}
		if prefix.Addr().Is4In6() {
			if prefix.Bits() < 96 {
				return nil, fmt.Errorf("invalid IPv4-mapped trusted proxy CIDR %q", entry)
			}
			prefix = netip.PrefixFrom(prefix.Addr().Unmap(), prefix.Bits()-96)
		}
		if prefix.Bits() == 0 {
			return nil, fmt.Errorf("trusted proxies must not include all addresses: %q", entry)
		}
		proxies = append(proxies, prefix.Masked())
	}
	return proxies, nil
}

func (s *Server) trustedProxy(address netip.Addr) bool {
	for _, prefix := range s.TrustedProxies {
		if prefix.Contains(address) {
			return true
		}
	}
	return false
}

func (s *Server) clientIP(r *http.Request) string {
	remote, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return "unknown"
	}
	peer := remote.Addr().WithZone("").Unmap()
	if !s.trustedProxy(peer) {
		return peer.String()
	}
	values := r.Header.Values("X-Forwarded-For")
	size := 0
	for _, value := range values {
		size += len(value) + 1
		if size > 4096 {
			return peer.String()
		}
	}
	hops := strings.Split(strings.Join(values, ","), ",")
	if len(hops) > 32 {
		return peer.String()
	}
	// Walk from our trusted peer toward the client. A client can supply the
	// left-hand part of X-Forwarded-For; stop at the first untrusted hop rather
	// than accepting that spoofable prefix.
	client := peer
	for i := len(hops) - 1; i >= 0; i-- {
		address, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil || address.Zone() != "" {
			return peer.String()
		}
		client = address.Unmap()
		if !s.trustedProxy(client) {
			return client.String()
		}
	}
	return client.String()
}
