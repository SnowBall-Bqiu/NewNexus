package agentclient

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
)

// Includes IANA special-use, documentation, transition and multicast ranges.
// IPv4-mapped IPv6 addresses are unwrapped before checking.
var blockedNetworks = prefixes(
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16",
	"172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24", "192.168.0.0/16",
	"198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
	"::/128", "::1/128", "::ffff:0:0:0/96", "64:ff9b::/96", "64:ff9b:1::/48", "100::/64",
	"2001::/23", "2001:db8::/32", "2002::/16", "3fff::/20", "fc00::/7", "fe80::/10", "ff00::/8",
)

func prefixes(values ...string) []netip.Prefix {
	p := make([]netip.Prefix, len(values))
	for i, value := range values {
		p[i] = netip.MustParsePrefix(value)
	}
	return p
}

func PublicIP(ip net.IP) bool {
	a, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	a = a.Unmap()
	if !a.IsGlobalUnicast() {
		return false
	}
	for _, prefix := range blockedNetworks {
		if prefix.Contains(a) {
			return false
		}
	}
	// Currently allocated global unicast IPv6 space only; fail closed on future ranges.
	if a.Is6() && !netip.MustParsePrefix("2000::/3").Contains(a) {
		return false
	}
	return true
}

func resolvePublic(ctx context.Context, resolver Resolver, host string) ([]net.IPAddr, error) {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if host == "" || strings.ContainsAny(host, "%/\\@\r\n\t ") {
		return nil, fmt.Errorf("invalid target hostname")
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || host == "metadata.google.internal" || strings.HasSuffix(host, ".internal") || strings.HasSuffix(host, ".local") {
		return nil, fmt.Errorf("target hostname is not public")
	}
	if ip := net.ParseIP(host); ip != nil {
		if !PublicIP(ip) {
			return nil, fmt.Errorf("target IP is not public")
		}
		return []net.IPAddr{{IP: ip}}, nil
	}
	if !strings.Contains(host, ".") {
		return nil, fmt.Errorf("target hostname must be fully qualified")
	}
	addresses, err := resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("resolve target: %w", err)
	}
	if len(addresses) == 0 {
		return nil, fmt.Errorf("target resolves to no addresses")
	}
	for _, address := range addresses {
		if address.Zone != "" || !PublicIP(address.IP) {
			return nil, fmt.Errorf("target resolves to a non-public address")
		}
	}
	return addresses, nil
}

func targetHostPort(address string, port int) (string, int, error) {
	address = strings.TrimSpace(address)
	if net.ParseIP(strings.Trim(address, "[]")) != nil {
		return strings.Trim(address, "[]"), port, nil
	}
	if strings.Contains(address, ":") {
		h, p, err := net.SplitHostPort(address)
		if err != nil {
			return "", 0, fmt.Errorf("invalid target address")
		}
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return "", 0, fmt.Errorf("invalid target port")
		}
		if port != 0 && port != n {
			return "", 0, fmt.Errorf("conflicting target ports")
		}
		return h, n, nil
	}
	return address, port, nil
}

func allowedPort(ports []int, port int) bool {
	for _, p := range ports {
		if p == port {
			return true
		}
	}
	return false
}
