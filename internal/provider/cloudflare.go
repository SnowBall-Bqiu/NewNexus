package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

type Cloudflare struct{ HTTPClient }

func NewCloudflare() *Cloudflare   { return &Cloudflare{HTTPClient: NewHTTPClient()} }
func (c *Cloudflare) Name() string { return "cloudflare" }

type cfEnvelope[T any] struct {
	Success bool `json:"success"`
	Result  T    `json:"result"`
	Errors  []struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"errors"`
	ResultInfo struct {
		Page       int `json:"page"`
		PerPage    int `json:"per_page"`
		TotalPages int `json:"total_pages"`
		Count      int `json:"count"`
		TotalCount int `json:"total_count"`
	} `json:"result_info"`
}

func checkCF[T any](response cfEnvelope[T]) error {
	if response.Success {
		return nil
	}
	if len(response.Errors) == 0 {
		return fmt.Errorf("Cloudflare API returned success=false")
	}
	parts := make([]string, 0, len(response.Errors))
	for _, item := range response.Errors {
		parts = append(parts, fmt.Sprintf("%d: %s", item.Code, item.Message))
	}
	return fmt.Errorf("Cloudflare API: %s", strings.Join(parts, "; "))
}

func (c *Cloudflare) headers(credentials Credentials) (map[string]string, error) {
	if err := Required(credentials, "apiToken"); err != nil {
		return nil, err
	}
	return map[string]string{"Authorization": "Bearer " + credentials["apiToken"]}, nil
}

func (c *Cloudflare) Validate(ctx context.Context, credentials Credentials) (Capability, error) {
	headers, err := c.headers(credentials)
	if err != nil {
		return Capability{}, err
	}
	var response cfEnvelope[struct {
		Status string `json:"status"`
	}]
	if err := c.JSON(ctx, http.MethodGet, "https://api.cloudflare.com/client/v4/user/tokens/verify", headers, nil, &response); err != nil {
		return Capability{}, err
	}
	if err := checkCF(response); err != nil {
		return Capability{}, err
	}
	if response.Result.Status != "active" {
		return Capability{}, fmt.Errorf("Cloudflare token is not active")
	}
	zones, err := c.ListZones(ctx, credentials)
	if err != nil {
		return Capability{}, fmt.Errorf("Cloudflare DNS permission check failed: %w", err)
	}
	capability := Capability{DNS: true, ApexCNAME: true, Details: map[string]string{"dns": "verified", "lines": "default only"}}
	if len(zones) > 0 {
		if _, err := c.CustomHostnameQuota(ctx, credentials, zones[0].ID); err == nil {
			capability.SaaS = true
			capability.Details["saas"] = "verified"
		} else {
			capability.Details["saas"] = "unavailable: " + err.Error()
		}
	}
	return capability, nil
}

func (c *Cloudflare) ListZones(ctx context.Context, credentials Credentials) ([]Zone, error) {
	headers, err := c.headers(credentials)
	if err != nil {
		return nil, err
	}
	zones := make([]Zone, 0)
	for page := 1; ; page++ {
		var response cfEnvelope[[]struct{ ID, Name, Status string }]
		endpoint := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones?per_page=50&page=%d", page)
		if err := c.JSON(ctx, http.MethodGet, endpoint, headers, nil, &response); err != nil {
			return nil, err
		}
		if err := checkCF(response); err != nil {
			return nil, err
		}
		for _, zone := range response.Result {
			zones = append(zones, Zone{ID: zone.ID, Name: zone.Name, Status: zone.Status})
		}
		if len(response.Result) == 0 || (response.ResultInfo.TotalPages > 0 && page >= response.ResultInfo.TotalPages) || len(response.Result) < 50 {
			break
		}
	}
	return zones, nil
}

func (c *Cloudflare) ListLines(context.Context, Credentials, Zone) ([]Line, error) {
	return []Line{{Code: "default", Name: "默认线路"}}, nil
}

func (c *Cloudflare) ListRecords(ctx context.Context, credentials Credentials, zone Zone, name string) ([]Record, error) {
	headers, err := c.headers(credentials)
	if err != nil {
		return nil, err
	}
	records := make([]Record, 0)
	for page := 1; ; page++ {
		endpoint := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records?per_page=100&page=%d", url.PathEscape(zone.ID), page)
		if name != "" {
			endpoint += "&name=" + url.QueryEscape(name)
		}
		var response cfEnvelope[[]struct {
			ID, Name, Type, Content, Comment string
			TTL                              int
			Proxied                          bool
		}]
		if err := c.JSON(ctx, http.MethodGet, endpoint, headers, nil, &response); err != nil {
			return nil, err
		}
		if err := checkCF(response); err != nil {
			return nil, err
		}
		for _, item := range response.Result {
			records = append(records, Record{ID: item.ID, Name: item.Name, Type: item.Type, Value: item.Content, LineCode: "default", TTL: item.TTL, Weight: 100, Enabled: true, Proxied: item.Proxied, Comment: item.Comment, Managed: strings.Contains(item.Comment, "Managed by NewNexus")})
		}
		if len(response.Result) == 0 || (response.ResultInfo.TotalPages > 0 && page >= response.ResultInfo.TotalPages) || len(response.Result) < 100 {
			break
		}
	}
	return records, nil
}

func (c *Cloudflare) ApplyChanges(ctx context.Context, credentials Credentials, zone Zone, changes []RecordChange) ([]Record, error) {
	headers, err := c.headers(credentials)
	if err != nil {
		return nil, err
	}
	result := make([]Record, 0, len(changes))
	for _, change := range changes {
		base := "https://api.cloudflare.com/client/v4/zones/" + url.PathEscape(zone.ID) + "/dns_records"
		switch change.Operation {
		case "create":
			if change.After == nil {
				return nil, fmt.Errorf("create change is missing after record")
			}
			body := map[string]any{"type": change.After.Type, "name": change.After.Name, "content": strings.TrimSuffix(change.After.Value, "."), "ttl": change.After.TTL, "proxied": false, "comment": "Managed by NewNexus"}
			var response cfEnvelope[struct {
				ID string `json:"id"`
			}]
			if err := c.JSON(ctx, http.MethodPost, base, headers, body, &response); err != nil {
				return nil, err
			}
			if err := checkCF(response); err != nil {
				return nil, err
			}
			record := *change.After
			record.ID = response.Result.ID
			record.Managed = true
			record.Comment = "Managed by NewNexus"
			result = append(result, record)
		case "update":
			if change.Before == nil || change.After == nil {
				return nil, fmt.Errorf("update change is incomplete")
			}
			body := map[string]any{"type": change.After.Type, "name": change.After.Name, "content": strings.TrimSuffix(change.After.Value, "."), "ttl": change.After.TTL, "proxied": false, "comment": "Managed by NewNexus"}
			var response cfEnvelope[struct {
				ID string `json:"id"`
			}]
			if err := c.JSON(ctx, http.MethodPut, base+"/"+url.PathEscape(change.Before.ID), headers, body, &response); err != nil {
				return nil, err
			}
			if err := checkCF(response); err != nil {
				return nil, err
			}
			record := *change.After
			record.ID = response.Result.ID
			result = append(result, record)
		case "delete":
			if change.Before == nil {
				return nil, fmt.Errorf("delete change is missing before record")
			}
			var response cfEnvelope[map[string]any]
			if err := c.JSON(ctx, http.MethodDelete, base+"/"+url.PathEscape(change.Before.ID), headers, nil, &response); err != nil {
				return nil, err
			}
			if err := checkCF(response); err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("unknown record operation %q", change.Operation)
		}
	}
	return result, nil
}

type CFCustomHostnameInput struct {
	Hostname           string         `json:"hostname"`
	SSL                map[string]any `json:"ssl"`
	CustomOriginServer string         `json:"custom_origin_server,omitempty"`
}

func (c *Cloudflare) CreateCustomHostname(ctx context.Context, credentials Credentials, zoneID string, input CFCustomHostnameInput) (map[string]any, error) {
	headers, err := c.headers(credentials)
	if err != nil {
		return nil, err
	}
	var response cfEnvelope[map[string]any]
	endpoint := "https://api.cloudflare.com/client/v4/zones/" + url.PathEscape(zoneID) + "/custom_hostnames"
	if err := c.JSON(ctx, http.MethodPost, endpoint, headers, input, &response); err != nil {
		return nil, err
	}
	if err := checkCF(response); err != nil {
		return nil, err
	}
	return response.Result, nil
}

func (c *Cloudflare) GetCustomHostname(ctx context.Context, credentials Credentials, zoneID, hostnameID string) (map[string]any, error) {
	headers, err := c.headers(credentials)
	if err != nil {
		return nil, err
	}
	var response cfEnvelope[map[string]any]
	endpoint := "https://api.cloudflare.com/client/v4/zones/" + url.PathEscape(zoneID) + "/custom_hostnames/" + url.PathEscape(hostnameID)
	if err := c.JSON(ctx, http.MethodGet, endpoint, headers, nil, &response); err != nil {
		return nil, err
	}
	if err := checkCF(response); err != nil {
		return nil, err
	}
	return response.Result, nil
}

func (c *Cloudflare) CustomHostnameQuota(ctx context.Context, credentials Credentials, zoneID string) (map[string]any, error) {
	headers, err := c.headers(credentials)
	if err != nil {
		return nil, err
	}
	var response cfEnvelope[map[string]any]
	endpoint := "https://api.cloudflare.com/client/v4/zones/" + url.PathEscape(zoneID) + "/custom_hostnames/quota"
	if err := c.JSON(ctx, http.MethodGet, endpoint, headers, nil, &response); err != nil {
		return nil, err
	}
	if err := checkCF(response); err != nil {
		return nil, err
	}
	return response.Result, nil
}

func (c *Cloudflare) DeleteCustomHostname(ctx context.Context, credentials Credentials, zoneID, hostnameID string) error {
	headers, err := c.headers(credentials)
	if err != nil {
		return err
	}
	var response cfEnvelope[map[string]any]
	endpoint := "https://api.cloudflare.com/client/v4/zones/" + url.PathEscape(zoneID) + "/custom_hostnames/" + url.PathEscape(hostnameID)
	if err := c.JSON(ctx, http.MethodDelete, endpoint, headers, nil, &response); err != nil {
		return err
	}
	return checkCF(response)
}

func (c *Cloudflare) GetFallbackOrigin(ctx context.Context, credentials Credentials, zoneID string) (map[string]any, error) {
	headers, err := c.headers(credentials)
	if err != nil {
		return nil, err
	}
	var response cfEnvelope[map[string]any]
	endpoint := "https://api.cloudflare.com/client/v4/zones/" + url.PathEscape(zoneID) + "/custom_hostnames/fallback_origin"
	if err := c.JSON(ctx, http.MethodGet, endpoint, headers, nil, &response); err != nil {
		return nil, err
	}
	if err := checkCF(response); err != nil {
		return nil, err
	}
	return response.Result, nil
}

func (c *Cloudflare) UpdateFallbackOrigin(ctx context.Context, credentials Credentials, zoneID, origin string) (map[string]any, error) {
	headers, err := c.headers(credentials)
	if err != nil {
		return nil, err
	}
	var response cfEnvelope[map[string]any]
	endpoint := "https://api.cloudflare.com/client/v4/zones/" + url.PathEscape(zoneID) + "/custom_hostnames/fallback_origin"
	if err := c.JSON(ctx, http.MethodPut, endpoint, headers, map[string]string{"origin": strings.TrimSuffix(origin, ".")}, &response); err != nil {
		return nil, err
	}
	if err := checkCF(response); err != nil {
		return nil, err
	}
	return response.Result, nil
}
