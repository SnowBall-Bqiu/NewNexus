package api

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/newnexus/newnexus/internal/domain"
	"github.com/newnexus/newnexus/internal/store"
)

var addressSettingKeys = []string{"addresses.api_origin", "addresses.agent_endpoint", "addresses.console_origin"}

// NormalizeHTTPOrigin validates an optional public origin without guessing from
// request headers or admitting credentials, paths, queries or fragments.
func NormalizeHTTPOrigin(configured string) (string, error) {
	value := strings.TrimSpace(configured)
	if value == "" {
		return "", nil
	}
	invalid := errors.New("origin must be an http or https origin without credentials, path, query or fragment")
	parsed, err := url.Parse(value)
	if len(value) > 4096 || err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil || (parsed.Path != "" && parsed.Path != "/") || parsed.RawPath != "" || parsed.RawQuery != "" || parsed.ForceQuery || strings.Contains(value, "#") || parsed.Opaque != "" {
		return "", invalid
	}
	host, port := parsed.Hostname(), parsed.Port()
	if host == "" || strings.ContainsAny(host, "\\/?#@ \t\r\n%") || strings.HasSuffix(parsed.Host, ":") || (strings.Contains(host, ":") || strings.HasPrefix(parsed.Host, "[")) && net.ParseIP(host) == nil {
		return "", invalid
	}
	if port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return "", invalid
		}
		port = strconv.Itoa(number)
		if parsed.Scheme == "http" && port == "80" || parsed.Scheme == "https" && port == "443" {
			port = ""
		}
	}
	host = strings.ToLower(host)
	if port != "" {
		host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return parsed.Scheme + "://" + host, nil
}

// NormalizeAgentEndpoint accepts an optional reachable gRPC host:port. An empty
// value means inheritance; wildcard bind addresses are never public endpoints.
func NormalizeAgentEndpoint(configured string) (string, error) {
	value := strings.TrimSpace(configured)
	if value == "" {
		return "", nil
	}
	invalid := errors.New("agent endpoint must be a host:port without a URL scheme, credentials or wildcard host")
	host, port, err := net.SplitHostPort(value)
	number, portErr := strconv.Atoi(port)
	if len(value) > 4096 || err != nil || portErr != nil || number < 1 || number > 65535 || host == "" {
		return "", invalid
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsUnspecified() {
			return "", invalid
		}
		host = ip.String()
	} else {
		host = strings.ToLower(host)
		if strings.HasPrefix(value, "[") || !validEndpointHostname(host) {
			return "", invalid
		}
	}
	return net.JoinHostPort(host, strconv.Itoa(number)), nil
}

func validEndpointHostname(host string) bool {
	host = strings.TrimSuffix(host, ".")
	if len(host) == 0 || len(host) > 253 {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, char := range label {
			if !(char >= 'a' && char <= 'z' || char >= '0' && char <= '9' || char == '-') {
				return false
			}
		}
	}
	return true
}

func isAddressSetting(key string) bool {
	return key == "addresses.api_origin" || key == "addresses.agent_endpoint" || key == "addresses.console_origin"
}

func normalizeAddressSetting(key string, value any) (string, error) {
	raw, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("%s must be a string", key)
	}
	if key == "addresses.agent_endpoint" {
		return NormalizeAgentEndpoint(raw)
	}
	return NormalizeHTTPOrigin(raw)
}

func (s *Server) addressFallback(key string) string {
	switch key {
	case "addresses.api_origin":
		return s.AgentDownloadOrigin
	case "addresses.agent_endpoint":
		return s.AgentPublicAddr
	case "addresses.console_origin":
		return s.ConsoleOrigin
	}
	return ""
}

func (s *Server) addressSettingValue(key, configured string) (string, string) {
	if configured != "" {
		return configured, "database"
	}
	if inherited := s.addressFallback(key); inherited != "" {
		return inherited, "runtime"
	}
	return "", "unset"
}

func (s *Server) effectiveAddresses(ctx context.Context) (domain.Doc, error) {
	values := domain.Doc{}
	for _, key := range addressSettingKeys {
		row, err := s.Store.Get(ctx, "settings", key)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return nil, err
		}
		values[key], _ = s.addressSettingValue(key, domain.S(row, "value"))
	}
	return values, nil
}
