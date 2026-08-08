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
	if !response.Success || response.Result.Status != "active" {
		return Capability{}, fmt.Errorf("Cloudflare token is not active")
	}
	return Capability{DNS: true, SaaS: true, BatchChanges: true}, nil
}

func (c *Cloudflare) ListZones(ctx context.Context, credentials Credentials) ([]Zone, error) {
	headers, err := c.headers(credentials)
	if err != nil {
		return nil, err
	}
	var response cfEnvelope[[]struct{ ID, Name, Status string }]
	if err := c.JSON(ctx, http.MethodGet, "https://api.cloudflare.com/client/v4/zones?per_page=50", headers, nil, &response); err != nil {
		return nil, err
	}
	zones := make([]Zone, 0, len(response.Result))
	for _, zone := range response.Result {
		zones = append(zones, Zone{ID: zone.ID, Name: zone.Name, Status: zone.Status})
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
	endpoint := "https://api.cloudflare.com/client/v4/zones/" + url.PathEscape(zone.ID) + "/dns_records?per_page=100"
	if name != "" {
		endpoint += "&name=" + url.QueryEscape(name)
	}
	var response cfEnvelope[[]struct {
		ID, Name, Type, Content string
		TTL                     int
		Proxied                 bool
	}]
	if err := c.JSON(ctx, http.MethodGet, endpoint, headers, nil, &response); err != nil {
		return nil, err
	}
	records := make([]Record, 0, len(response.Result))
	for _, item := range response.Result {
		records = append(records, Record{ID: item.ID, Name: item.Name, Type: item.Type, Value: item.Content, LineCode: "default", TTL: item.TTL, Weight: 100, Enabled: true})
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
			record := *change.After
			record.ID = response.Result.ID
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
			record := *change.After
			record.ID = response.Result.ID
			result = append(result, record)
		case "delete":
			if change.Before == nil {
				return nil, fmt.Errorf("delete change is missing before record")
			}
			if err := c.JSON(ctx, http.MethodDelete, base+"/"+url.PathEscape(change.Before.ID), headers, nil, nil); err != nil {
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
	return response.Result, nil
}
