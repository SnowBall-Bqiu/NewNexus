package provider

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/newnexus/newnexus/internal/domain"
)

// ZoneCapabilities is checked again before every publish. It never guesses from a plan name.
func (a *Adapter) ZoneCapabilities(ctx context.Context, zone string) (Capabilities, error) {
	lines, err := a.Lines(ctx, zone)
	if err != nil {
		return Capabilities{}, err
	}
	cap := Capabilities{Lines: len(lines) > 1, Apex: a.Provider == "cloudflare", MinTTL: 600}
	switch a.Provider {
	case "cloudflare":
		cap.MinTTL = 60
		_, err = a.Quota(ctx, zone)
		cap.SaaS = err == nil
		// Custom origins are included on every SaaS plan. SNI rewrites are a
		// separate entitlement and are deliberately not inferred here.
		// https://developers.cloudflare.com/cloudflare-for-platforms/cloudflare-for-saas/plans/
		cap.CustomOrigin = cap.SaaS
	case "dnspod":
		d, e := a.tencent(ctx, "DescribeDomainPurview", domain.Doc{"Domain": zone})
		if e != nil {
			return cap, e
		}
		for _, v := range domain.A(d, "PurviewList") {
			name := strings.ToLower(domain.S(v, "Name"))
			if strings.Contains(name, "负载均衡") || strings.Contains(name, "load balanc") {
				cap.Weights = domain.N(v, "Value") > 1
			}
			if strings.Contains(name, "ttl") {
				n := int(domain.N(v, "Value"))
				if n > 0 {
					cap.MinTTL = n
				}
			}
		}
	case "alidns":
		// The API advertises CNAME weights separately from A/AAAA. Only an enabled
		// CNAME policy is evidence that this account/zone can actually use them.
		for page := 1; page <= 10000; page++ {
			d, e := a.aliyun(ctx, "DescribeDNSSLBSubDomains", url.Values{"DomainName": {zone}, "PageNumber": {strconv.Itoa(page)}, "PageSize": {"100"}})
			if e != nil {
				return cap, e
			}
			rows := domain.A(domain.M(d, "SlbSubDomains"), "SlbSubDomain")
			for _, v := range rows {
				if domain.S(v, "Type") == "CNAME" && domain.B(v, "Open") {
					cap.Weights = true
				}
			}
			if len(rows) < 100 {
				break
			}
		}
	default:
		return cap, errors.New("unsupported DNS provider")
	}
	return cap, nil
}

func (a *Adapter) Quota(ctx context.Context, zone string) (domain.Doc, error) {
	d, e := a.SaaS(ctx, "GET", zone, "custom_hostnames/quota", nil)
	if e != nil {
		return nil, e
	}
	return domain.M(d, "result"), nil
}
func (a *Adapter) CustomHostnames(ctx context.Context, zone string) ([]domain.Doc, error) {
	out := []domain.Doc{}
	for page := 1; page <= 10000; page++ {
		d, e := a.SaaS(ctx, "GET", zone, fmt.Sprintf("custom_hostnames?per_page=50&page=%d", page), nil)
		if e != nil {
			return nil, e
		}
		rows := domain.A(d, "result")
		out = append(out, rows...)
		info := domain.M(d, "result_info")
		if len(rows) < 50 || domain.N(info, "total_pages") > 0 && float64(page) >= domain.N(info, "total_pages") {
			return out, nil
		}
	}
	return nil, errors.New("provider custom hostname pagination exceeds safety bound")
}
