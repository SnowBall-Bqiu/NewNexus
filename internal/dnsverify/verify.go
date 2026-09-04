package dnsverify

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/SnowBall-Bqiu/NewNexus/internal/provider"
)

type Resolver struct {
	Attempts int
	Delay    time.Duration
}

func New() Resolver { return Resolver{Attempts: 4, Delay: 2 * time.Second} }

func (r Resolver) Verify(ctx context.Context, zone string, records []provider.Record) error {
	if len(records) == 0 {
		return nil
	}
	resolver, err := authoritativeResolver(ctx, zone)
	if err != nil {
		return err
	}
	groups := groupRecords(records)
	var lastErr error
	for attempt := 0; attempt < r.Attempts; attempt++ {
		lastErr = verifyGroups(ctx, resolver, groups)
		if lastErr == nil {
			return nil
		}
		if attempt+1 < r.Attempts {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(r.Delay):
			}
		}
	}
	return lastErr
}

func (r Resolver) VerifyAbsent(ctx context.Context, zone string, records []provider.Record) error {
	if len(records) == 0 {
		return nil
	}
	resolver, err := authoritativeResolver(ctx, zone)
	if err != nil {
		return err
	}
	groups := groupRecords(records)
	var lastErr error
	for attempt := 0; attempt < r.Attempts; attempt++ {
		lastErr = verifyAbsentGroups(ctx, resolver, groups)
		if lastErr == nil {
			return nil
		}
		if attempt+1 < r.Attempts {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(r.Delay):
			}
		}
	}
	return lastErr
}

func authoritativeResolver(ctx context.Context, zone string) (*net.Resolver, error) {
	zone = normalize(zone)
	nameservers, err := net.DefaultResolver.LookupNS(ctx, zone)
	if err != nil || len(nameservers) == 0 {
		return nil, fmt.Errorf("resolve authoritative nameserver for %s: %w", zone, err)
	}
	addresses, err := net.DefaultResolver.LookupIPAddr(ctx, strings.TrimSuffix(nameservers[0].Host, "."))
	if err != nil || len(addresses) == 0 {
		return nil, fmt.Errorf("resolve authoritative nameserver address: %w", err)
	}
	endpoint := net.JoinHostPort(addresses[0].IP.String(), "53")
	return &net.Resolver{PreferGo: true, StrictErrors: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 4 * time.Second}).DialContext(ctx, "udp", endpoint)
	}}, nil
}

func groupRecords(records []provider.Record) map[string][]string {
	groups := make(map[string][]string)
	for _, record := range records {
		key := strings.ToUpper(record.Type) + "|" + normalize(record.Name)
		groups[key] = append(groups[key], normalizeValue(record.Type, record.Value))
	}
	for key := range groups {
		sort.Strings(groups[key])
	}
	return groups
}

func verifyGroups(ctx context.Context, resolver *net.Resolver, groups map[string][]string) error {
	for key, expected := range groups {
		parts := strings.SplitN(key, "|", 2)
		recordType, name := parts[0], parts[1]
		var observed []string
		switch recordType {
		case "CNAME":
			value, err := resolver.LookupCNAME(ctx, name)
			if err != nil {
				return fmt.Errorf("authoritative CNAME lookup for %s: %w", name, err)
			}
			observed = []string{normalize(value)}
		case "A", "AAAA":
			values, err := resolver.LookupIPAddr(ctx, name)
			if err != nil {
				return fmt.Errorf("authoritative address lookup for %s: %w", name, err)
			}
			for _, value := range values {
				if (recordType == "A" && value.IP.To4() != nil) || (recordType == "AAAA" && value.IP.To4() == nil) {
					observed = append(observed, value.IP.String())
				}
			}
		case "TXT":
			values, err := resolver.LookupTXT(ctx, name)
			if err != nil {
				return fmt.Errorf("authoritative TXT lookup for %s: %w", name, err)
			}
			for _, value := range values {
				observed = append(observed, normalizeValue("TXT", value))
			}
		default:
			continue
		}
		if recordType == "CNAME" {
			if !contains(expected, observed[0]) {
				return fmt.Errorf("authoritative DNS returned %s for %s; expected one of %s", observed[0], name, strings.Join(expected, ", "))
			}
			continue
		}
		for _, value := range expected {
			if !contains(observed, value) {
				return fmt.Errorf("authoritative DNS did not return %s %s for %s", recordType, value, name)
			}
		}
	}
	return nil
}

func verifyAbsentGroups(ctx context.Context, resolver *net.Resolver, groups map[string][]string) error {
	for key, removed := range groups {
		parts := strings.SplitN(key, "|", 2)
		recordType, name := parts[0], parts[1]
		observed, err := lookupValues(ctx, resolver, recordType, name)
		if err != nil {
			var dnsErr *net.DNSError
			if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
				continue
			}
			return err
		}
		for _, value := range removed {
			if contains(observed, value) {
				return fmt.Errorf("authoritative DNS still returns %s %s for %s", recordType, value, name)
			}
		}
	}
	return nil
}

func lookupValues(ctx context.Context, resolver *net.Resolver, recordType, name string) ([]string, error) {
	switch recordType {
	case "CNAME":
		value, err := resolver.LookupCNAME(ctx, name)
		if err != nil {
			return nil, err
		}
		return []string{normalize(value)}, nil
	case "A", "AAAA":
		values, err := resolver.LookupIPAddr(ctx, name)
		if err != nil {
			return nil, err
		}
		observed := make([]string, 0, len(values))
		for _, value := range values {
			if (recordType == "A" && value.IP.To4() != nil) || (recordType == "AAAA" && value.IP.To4() == nil) {
				observed = append(observed, value.IP.String())
			}
		}
		return observed, nil
	case "TXT":
		values, err := resolver.LookupTXT(ctx, name)
		if err != nil {
			return nil, err
		}
		for index := range values {
			values[index] = normalizeValue("TXT", values[index])
		}
		return values, nil
	default:
		return nil, nil
	}
}

func normalize(value string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(value)), ".")
}
func normalizeValue(recordType, value string) string {
	if strings.EqualFold(recordType, "TXT") {
		return strings.Trim(strings.TrimSpace(value), `"`)
	}
	return normalize(value)
}
func contains(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}
