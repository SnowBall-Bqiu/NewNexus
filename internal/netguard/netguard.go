package netguard

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

var blockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("169.254.0.0/16"), netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"), netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"), netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("224.0.0.0/4"), netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("::/128"), netip.MustParsePrefix("::1/128"), netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("fe80::/10"), netip.MustParsePrefix("2001:db8::/32"), netip.MustParsePrefix("ff00::/8"),
}

func ValidateAddress(address netip.Addr) error {
	address = address.Unmap()
	if !address.IsValid() || !address.IsGlobalUnicast() {
		return fmt.Errorf("address is not globally routable")
	}
	for _, prefix := range blockedPrefixes {
		if prefix.Contains(address) {
			return fmt.Errorf("address belongs to blocked range %s", prefix)
		}
	}
	return nil
}

func ResolvePublic(ctx context.Context, resolver *net.Resolver, hostname string) ([]netip.Addr, error) {
	hostname = strings.TrimSuffix(strings.TrimSpace(hostname), ".")
	if hostname == "" {
		return nil, fmt.Errorf("hostname is required")
	}
	if parsed, err := netip.ParseAddr(hostname); err == nil {
		if err := ValidateAddress(parsed); err != nil {
			return nil, err
		}
		return []netip.Addr{parsed.Unmap()}, nil
	}
	values, err := resolver.LookupNetIP(ctx, "ip", hostname)
	if err != nil {
		return nil, err
	}
	result := make([]netip.Addr, 0, len(values))
	for _, value := range values {
		value = value.Unmap()
		if err := ValidateAddress(value); err != nil {
			return nil, fmt.Errorf("%s resolved to unsafe address %s: %w", hostname, value, err)
		}
		result = append(result, value)
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("hostname did not resolve to a public address")
	}
	return result, nil
}

func ValidateURL(ctx context.Context, resolver *net.Resolver, rawURL string, allowedPorts map[int]bool) (*url.URL, []netip.Addr, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return nil, nil, err
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, nil, fmt.Errorf("only http and https URLs are allowed")
	}
	if parsed.User != nil || parsed.Hostname() == "" {
		return nil, nil, fmt.Errorf("URL credentials are forbidden and hostname is required")
	}
	port := 80
	if parsed.Scheme == "https" {
		port = 443
	}
	if parsed.Port() != "" {
		port, err = strconv.Atoi(parsed.Port())
		if err != nil {
			return nil, nil, fmt.Errorf("invalid URL port")
		}
	}
	if len(allowedPorts) > 0 && !allowedPorts[port] {
		return nil, nil, fmt.Errorf("port %d is not allowed", port)
	}
	addresses, err := ResolvePublic(ctx, resolver, parsed.Hostname())
	if err != nil {
		return nil, nil, err
	}
	return parsed, addresses, nil
}

func NewHTTPClient(timeout time.Duration, maxRedirects int, maxIdle int) *http.Client {
	resolver := net.DefaultResolver
	transport := &http.Transport{
		Proxy:                 nil,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          maxIdle,
		MaxIdleConnsPerHost:   maxIdle,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   8 * time.Second,
		ResponseHeaderTimeout: timeout,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}
			values, err := ResolvePublic(ctx, resolver, host)
			if err != nil {
				return nil, err
			}
			return (&net.Dialer{Timeout: 8 * time.Second, KeepAlive: 30 * time.Second}).DialContext(ctx, network, net.JoinHostPort(values[0].String(), port))
		},
	}
	client := &http.Client{Timeout: timeout, Transport: transport}
	client.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if len(via) >= maxRedirects {
			return fmt.Errorf("redirect limit exceeded")
		}
		_, _, err := ValidateURL(request.Context(), resolver, request.URL.String(), map[int]bool{80: true, 443: true})
		return err
	}
	return client
}
