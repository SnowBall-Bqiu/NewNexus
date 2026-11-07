package provider

import (
	"context"
	"errors"
	"github.com/newnexus/newnexus/internal/domain"
	"net/url"
	"strings"
)

func normalizedLine(s string) string {
	if s == "" || s == "0" {
		return "default"
	}
	return s
}
func positiveWeight(n int) int {
	if n <= 0 {
		return 1
	}
	return n
}

// ApplyChanges returns realized provider IDs even when a later step fails. The
// caller can persist the completed subset and compensate without deleting a
// record that was never created by this operation.
func (a *Adapter) ApplyChanges(ctx context.Context, zone string, changes []Change) ([]Change, error) {
	done := []Change{}
	for _, c := range changes {
		if c.Action != "create" && c.Action != "update" && c.Action != "delete" {
			return nil, errors.New("invalid DNS change action")
		}
		if c.Action != "delete" && c.After == nil || c.Action != "create" && c.Before == nil {
			return nil, errors.New("incomplete DNS change snapshot")
		}
		if c.Action != "create" && c.Before.ID == "" {
			return nil, errors.New("provider record ID is required for mutation")
		}
	}
	for _, c := range changes {
		r, e := a.write(ctx, zone, c)
		if r != nil {
			if c.Action == "delete" {
				c.Before = r
			} else {
				c.After = r
			}
			done = append(done, c)
		}
		if e != nil {
			return done, e
		}
	}
	return done, nil
}

func (a *Adapter) checkAliWeight(ctx context.Context, zone string, r Record) error {
	d, e := a.aliyun(ctx, "DescribeDNSSLBSubDomains", url.Values{"DomainName": {zone}, "Rr": {strings.TrimSuffix(r.Name, "."+zone)}, "PageSize": {"100"}})
	if e != nil {
		return e
	}
	for _, policy := range domain.A(domain.M(d, "SlbSubDomains"), "SlbSubDomain") {
		if !strings.EqualFold(strings.TrimSpace(domain.S(policy, "SubDomain")), r.Name) || domain.S(policy, "Type") != r.Type || !domain.B(policy, "Open") {
			continue
		}
		lines := domain.A(domain.M(policy, "LineAlgorithms"), "LineAlgorithm")
		for _, line := range lines {
			if normalizedLine(domain.S(line, "Line")) == normalizedLine(r.Line) && domain.B(line, "Open") {
				return nil
			}
		}
	}
	return errors.New("AliDNS weight policy is not enabled for this hostname, type and line; enable it with the DNS provider and synchronize capabilities")
}
