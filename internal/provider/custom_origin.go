package provider

import (
	"context"
	"errors"
	"strings"
)

// ValidateCustomOrigin verifies Cloudflare's same-account, proxied DNS
// prerequisite without creating or modifying a provider resource.
func (a *Adapter) ValidateCustomOrigin(ctx context.Context, saasZone, hostname string) error {
	if a.Provider != "cloudflare" {
		return errors.New("custom origins require a Cloudflare connection")
	}
	hostname = strings.ToLower(strings.TrimSuffix(hostname, "."))
	zones, err := a.Zones(ctx)
	if err != nil {
		return err
	}
	account := ""
	for _, zone := range zones {
		if zone.ID == saasZone {
			account = zone.AccountID
			break
		}
	}
	if account == "" {
		return errors.New("cannot verify SaaS account for custom origin; grant zone read permission and synchronize the connection")
	}
	var originZone *Zone
	for index := range zones {
		zone := &zones[index]
		name := strings.ToLower(strings.TrimSuffix(zone.Name, "."))
		if zone.AccountID == account && (hostname == name || strings.HasSuffix(hostname, "."+name)) && (originZone == nil || len(name) > len(originZone.Name)) {
			originZone = zone
		}
	}
	if originZone == nil {
		return errors.New("custom origin must have DNS in the same Cloudflare account; grant read access to its zone")
	}
	records, err := a.Records(ctx, originZone.ID)
	if err != nil {
		return err
	}
	for _, record := range records {
		if strings.EqualFold(strings.TrimSuffix(record.Name, "."), hostname) && record.Proxied && (record.Type == "A" || record.Type == "AAAA" || record.Type == "CNAME") {
			return nil
		}
	}
	return errors.New("custom origin requires an explicit proxied A, AAAA or CNAME record in the same Cloudflare account; wildcard or DNS-only records are not supported")
}
