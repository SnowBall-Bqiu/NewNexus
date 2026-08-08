package provider

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

type AliDNS struct{ HTTPClient }

func NewAliDNS() *AliDNS       { return &AliDNS{HTTPClient: NewHTTPClient()} }
func (a *AliDNS) Name() string { return "alidns" }

func (a *AliDNS) call(ctx context.Context, credentials Credentials, action string, params map[string]string, output any) error {
	if err := Required(credentials, "accessKeyId", "accessKeySecret"); err != nil {
		return err
	}
	query := map[string]string{"Format": "JSON", "Version": "2015-01-09", "AccessKeyId": credentials["accessKeyId"], "SignatureMethod": "HMAC-SHA1", "SignatureVersion": "1.0", "SignatureNonce": uuid.NewString(), "Timestamp": time.Now().UTC().Format("2006-01-02T15:04:05Z"), "Action": action}
	for key, value := range params {
		query[key] = value
	}
	keys := make([]string, 0, len(query))
	for key := range query {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, aliEscape(key)+"="+aliEscape(query[key]))
	}
	canonical := strings.Join(parts, "&")
	stringToSign := "GET&%2F&" + aliEscape(canonical)
	h := hmac.New(sha1.New, []byte(credentials["accessKeySecret"]+"&"))
	_, _ = h.Write([]byte(stringToSign))
	query["Signature"] = base64.StdEncoding.EncodeToString(h.Sum(nil))
	values := url.Values{}
	for key, value := range query {
		values.Set(key, value)
	}
	return a.JSON(ctx, http.MethodGet, "https://alidns.aliyuncs.com/?"+values.Encode(), nil, nil, output)
}

func (a *AliDNS) Validate(ctx context.Context, credentials Credentials) (Capability, error) {
	var response struct{ RequestID, Code, Message string }
	if err := a.call(ctx, credentials, "DescribeDomains", map[string]string{"PageNumber": "1", "PageSize": "1"}, &response); err != nil {
		return Capability{}, err
	}
	if response.Code != "" {
		return Capability{}, fmt.Errorf("%s: %s", response.Code, response.Message)
	}
	return Capability{DNS: true, Lines: true}, nil
}

func (a *AliDNS) ListZones(ctx context.Context, credentials Credentials) ([]Zone, error) {
	var raw map[string]any
	if err := a.call(ctx, credentials, "DescribeDomains", map[string]string{"PageNumber": "1", "PageSize": "100"}, &raw); err != nil {
		return nil, err
	}
	items := nestedSlice(raw, "Domains", "Domain")
	zones := make([]Zone, 0, len(items))
	for _, item := range items {
		zones = append(zones, Zone{ID: stringValue(item["DomainId"]), Name: stringValue(item["DomainName"]), Status: "active"})
	}
	return zones, nil
}

func (a *AliDNS) ListLines(ctx context.Context, credentials Credentials, zone Zone) ([]Line, error) {
	var raw map[string]any
	if err := a.call(ctx, credentials, "DescribeSupportLines", map[string]string{"DomainName": zone.Name}, &raw); err != nil {
		return nil, err
	}
	items := nestedSlice(raw, "RecordLines", "RecordLine")
	lines := make([]Line, 0, len(items))
	for _, item := range items {
		lines = append(lines, Line{Code: stringValue(item["LineCode"]), Name: stringValue(item["LineName"])})
	}
	if len(lines) == 0 {
		lines = append(lines, Line{Code: "default", Name: "默认线路"})
	}
	return lines, nil
}

func (a *AliDNS) ListRecords(ctx context.Context, credentials Credentials, zone Zone, name string) ([]Record, error) {
	params := map[string]string{"DomainName": zone.Name, "PageNumber": "1", "PageSize": "100"}
	if name != "" {
		params["RRKeyWord"] = relativeName(name, zone.Name)
	}
	var raw map[string]any
	if err := a.call(ctx, credentials, "DescribeDomainRecords", params, &raw); err != nil {
		return nil, err
	}
	items := nestedSlice(raw, "DomainRecords", "Record")
	records := make([]Record, 0, len(items))
	for _, item := range items {
		ttl, _ := strconv.Atoi(stringValue(item["TTL"]))
		records = append(records, Record{ID: stringValue(item["RecordId"]), Name: stringValue(item["RR"]) + "." + zone.Name, Type: stringValue(item["Type"]), Value: stringValue(item["Value"]), LineCode: stringValue(item["Line"]), TTL: ttl, Weight: 100, Enabled: stringValue(item["Status"]) != "DISABLE"})
	}
	return records, nil
}

func (a *AliDNS) ApplyChanges(ctx context.Context, credentials Credentials, zone Zone, changes []RecordChange) ([]Record, error) {
	result := make([]Record, 0, len(changes))
	for _, change := range changes {
		if change.Operation == "delete" {
			return nil, fmt.Errorf("AliDNS delete is intentionally disabled until release rollback has a verified snapshot")
		}
		if change.After == nil {
			return nil, fmt.Errorf("change is missing desired record")
		}
		params := map[string]string{"RR": relativeName(change.After.Name, zone.Name), "Type": change.After.Type, "Value": change.After.Value, "TTL": strconv.Itoa(change.After.TTL), "Line": change.After.LineCode}
		action := "AddDomainRecord"
		if change.Operation == "update" && change.Before != nil {
			action = "UpdateDomainRecord"
			params["RecordId"] = change.Before.ID
		}
		if action == "AddDomainRecord" {
			params["DomainName"] = zone.Name
		}
		var response struct{ RecordID, RequestID, Code, Message string }
		if err := a.call(ctx, credentials, action, params, &response); err != nil {
			return nil, err
		}
		if response.Code != "" {
			return nil, fmt.Errorf("%s: %s", response.Code, response.Message)
		}
		record := *change.After
		record.ID = response.RecordID
		result = append(result, record)
	}
	return result, nil
}

func aliEscape(value string) string { return strings.ReplaceAll(url.QueryEscape(value), "+", "%20") }
func nestedSlice(raw map[string]any, outer, inner string) []map[string]any {
	container, _ := raw[outer].(map[string]any)
	values, _ := container[inner].([]any)
	result := make([]map[string]any, 0, len(values))
	for _, value := range values {
		if item, ok := value.(map[string]any); ok {
			result = append(result, item)
		}
	}
	return result
}
func stringValue(value any) string {
	if value == nil {
		return ""
	}
	if stringValue, ok := value.(string); ok {
		return stringValue
	}
	raw, _ := json.Marshal(value)
	return strings.Trim(string(raw), `"`)
}
