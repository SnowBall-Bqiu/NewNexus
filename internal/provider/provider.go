// Package provider contains the versioned, compiled-in DNS adapter boundary.
package provider

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/newnexus/newnexus/internal/domain"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Capabilities struct {
	Lines        bool `json:"lines"`
	Weights      bool `json:"weights"`
	Apex         bool `json:"apex"`
	SaaS         bool `json:"saas"`
	CustomOrigin bool `json:"custom_origin"`
	MinTTL       int  `json:"min_ttl,omitempty"`
}
type Zone struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Grade     string `json:"grade,omitempty"`
	AccountID string `json:"account_id,omitempty"`
}
type Line struct {
	Code string `json:"code"`
	Name string `json:"name"`
}
type Record struct {
	ID      string `json:"id,omitempty"`
	Name    string `json:"name"`
	Type    string `json:"type"`
	Value   string `json:"value"`
	TTL     int    `json:"ttl"`
	Line    string `json:"line"`
	Weight  int    `json:"weight"`
	Proxied bool   `json:"proxied"`
}
type Change struct {
	Action string  `json:"action"`
	Name   string  `json:"name"`
	Type   string  `json:"type"`
	Line   string  `json:"line"`
	Before *Record `json:"before,omitempty"`
	After  *Record `json:"after,omitempty"`
}

// ProviderAdapterV1 is transport neutral and can be moved behind an external gRPC process.
type ProviderAdapterV1 interface {
	Validate(context.Context) (Capabilities, error)
	Zones(context.Context) ([]Zone, error)
	Lines(context.Context, string) ([]Line, error)
	Records(context.Context, string) ([]Record, error)
	Apply(context.Context, string, []Change) ([]Record, error)
	Rollback(context.Context, string, []Change) error
}

const AdapterProtocolVersion = 1

var _ ProviderAdapterV1 = (*Adapter)(nil)

type Adapter struct {
	Provider    string
	Credentials domain.Doc
	Client      *http.Client
	BaseURL     string
}

func New(name string, credentials domain.Doc) *Adapter {
	return &Adapter{Provider: name, Credentials: credentials, Client: &http.Client{Timeout: 30 * time.Second}}
}
func (a *Adapter) Validate(ctx context.Context) (Capabilities, error) {
	zones, e := a.Zones(ctx)
	if e != nil {
		return Capabilities{}, e
	}
	c := Capabilities{Apex: a.Provider == "cloudflare"}
	for _, z := range zones {
		cap, err := a.ZoneCapabilities(ctx, z.ID)
		if err != nil {
			continue
		}
		c.Lines = c.Lines || cap.Lines
		c.Weights = c.Weights || cap.Weights
		c.SaaS = c.SaaS || cap.SaaS
		c.CustomOrigin = c.CustomOrigin || cap.CustomOrigin
	}
	return c, nil
}
func (a *Adapter) request(ctx context.Context, method, path string, body any) (domain.Doc, error) {
	var payload []byte
	if body != nil {
		var e error
		payload, e = json.Marshal(body)
		if e != nil {
			return nil, e
		}
	}
	base := a.BaseURL
	if base == "" {
		base = "https://api.cloudflare.com/client/v4"
	}
	req, e := http.NewRequestWithContext(ctx, method, base+path, bytes.NewReader(payload))
	if e != nil {
		return nil, e
	}
	req.Header.Set("Authorization", "Bearer "+domain.S(a.Credentials, "api_token"))
	req.Header.Set("Content-Type", "application/json")
	return a.doJSON(req, "cloudflare", method == http.MethodGet)
}

func (a *Adapter) Zones(ctx context.Context) ([]Zone, error) {
	out := []Zone{}
	switch a.Provider {
	case "cloudflare":
		for page := 1; ; page++ {
			if page > 10000 {
				return nil, errors.New("provider pagination exceeds safety bound")
			}
			d, e := a.request(ctx, "GET", fmt.Sprintf("/zones?per_page=50&page=%d", page), nil)
			if e != nil {
				return nil, e
			}
			for _, z := range domain.A(d, "result") {
				out = append(out, Zone{ID: domain.S(z, "id"), Name: domain.S(z, "name"), Grade: domain.S(domain.M(z, "plan"), "name"), AccountID: domain.S(domain.M(z, "account"), "id")})
			}
			if len(domain.A(d, "result")) < 50 {
				break
			}
		}
	case "dnspod":
		for offset := 0; ; offset += 100 {
			if offset > 1000000 {
				return nil, errors.New("provider pagination exceeds safety bound")
			}
			d, e := a.tencent(ctx, "DescribeDomainList", domain.Doc{"Offset": offset, "Limit": 100})
			if e != nil {
				return nil, e
			}
			rows := domain.A(d, "DomainList")
			for _, z := range rows {
				out = append(out, Zone{ID: domain.S(z, "Name"), Name: domain.S(z, "Name"), Grade: domain.S(z, "Grade")})
			}
			if len(rows) < 100 {
				break
			}
		}
	case "alidns":
		for page := 1; ; page++ {
			if page > 10000 {
				return nil, errors.New("provider pagination exceeds safety bound")
			}
			d, e := a.aliyun(ctx, "DescribeDomains", url.Values{"PageNumber": {strconv.Itoa(page)}, "PageSize": {"100"}})
			if e != nil {
				return nil, e
			}
			rows := domain.A(domain.M(d, "Domains"), "Domain")
			for _, z := range rows {
				out = append(out, Zone{ID: domain.S(z, "DomainName"), Name: domain.S(z, "DomainName"), Grade: domain.S(z, "InstanceId")})
			}
			if len(rows) < 100 {
				break
			}
		}
	default:
		return nil, errors.New("unsupported DNS provider")
	}
	return out, nil
}
func (a *Adapter) Lines(ctx context.Context, zone string) ([]Line, error) {
	if a.Provider != "cloudflare" && a.Provider != "dnspod" && a.Provider != "alidns" {
		return nil, errors.New("unsupported DNS provider")
	}
	if a.Provider == "cloudflare" {
		return []Line{{"default", "默认"}}, nil
	}
	if a.Provider == "dnspod" {
		info, e := a.tencent(ctx, "DescribeDomain", domain.Doc{"Domain": zone})
		if e != nil {
			return nil, e
		}
		grade := domain.S(domain.M(info, "DomainInfo"), "Grade")
		d, e := a.tencent(ctx, "DescribeRecordLineList", domain.Doc{"Domain": zone, "DomainGrade": grade})
		if e != nil {
			return nil, e
		}
		out := []Line{}
		for _, l := range domain.A(d, "LineList") {
			out = append(out, Line{normalizedLine(domain.S(l, "LineId")), domain.S(l, "Name")})
		}
		return out, nil
	}
	d, e := a.aliyun(ctx, "DescribeSupportLines", url.Values{"DomainName": {zone}})
	if e != nil {
		return nil, e
	}
	out := []Line{}
	for _, l := range domain.A(domain.M(d, "RecordLines"), "RecordLine") {
		out = append(out, Line{domain.S(l, "LineCode"), domain.S(l, "LineDisplayName")})
	}
	return out, nil
}
func (a *Adapter) Records(ctx context.Context, zone string) ([]Record, error) {
	out := []Record{}
	switch a.Provider {
	case "cloudflare":
		for page := 1; ; page++ {
			if page > 10000 {
				return nil, errors.New("provider pagination exceeds safety bound")
			}
			d, e := a.request(ctx, "GET", fmt.Sprintf("/zones/%s/dns_records?per_page=100&page=%d", url.PathEscape(zone), page), nil)
			if e != nil {
				return nil, e
			}
			rows := domain.A(d, "result")
			for _, r := range rows {
				out = append(out, Record{ID: domain.S(r, "id"), Name: domain.S(r, "name"), Type: domain.S(r, "type"), Value: domain.S(r, "content"), TTL: int(domain.N(r, "ttl")), Line: "default", Weight: 1, Proxied: domain.B(r, "proxied")})
			}
			if len(rows) < 100 {
				break
			}
		}
	case "dnspod":
		for offset := 0; ; offset += 100 {
			if offset > 1000000 {
				return nil, errors.New("provider pagination exceeds safety bound")
			}
			d, e := a.tencent(ctx, "DescribeRecordList", domain.Doc{"Domain": zone, "Offset": offset, "Limit": 100, "ErrorOnEmpty": "no"})
			if e != nil {
				return nil, e
			}
			rows := domain.A(d, "RecordList")
			for _, r := range rows {
				n := domain.S(r, "Name")
				if n == "@" {
					n = zone
				} else {
					n += "." + zone
				}
				out = append(out, Record{ID: domain.S(r, "RecordId"), Name: n, Type: domain.S(r, "Type"), Value: domain.S(r, "Value"), TTL: int(domain.N(r, "TTL")), Line: normalizedLine(domain.S(r, "LineId")), Weight: positiveWeight(int(domain.N(r, "Weight")))})
			}
			if len(rows) < 100 {
				break
			}
		}
	case "alidns":
		for page := 1; ; page++ {
			if page > 10000 {
				return nil, errors.New("provider pagination exceeds safety bound")
			}
			d, e := a.aliyun(ctx, "DescribeDomainRecords", url.Values{"DomainName": {zone}, "PageNumber": {strconv.Itoa(page)}, "PageSize": {"100"}})
			if e != nil {
				return nil, e
			}
			rows := domain.A(domain.M(d, "DomainRecords"), "Record")
			for _, r := range rows {
				n := domain.S(r, "RR")
				if n == "@" {
					n = zone
				} else {
					n += "." + zone
				}
				out = append(out, Record{ID: domain.S(r, "RecordId"), Name: n, Type: domain.S(r, "Type"), Value: domain.S(r, "Value"), TTL: int(domain.N(r, "TTL")), Line: normalizedLine(domain.S(r, "Line")), Weight: positiveWeight(int(domain.N(r, "Weight")))})
			}
			if len(rows) < 100 {
				break
			}
		}
	default:
		return nil, errors.New("unsupported DNS provider")
	}
	return out, nil
}
func (a *Adapter) Apply(ctx context.Context, zone string, changes []Change) ([]Record, error) {
	completed, e := a.ApplyChanges(ctx, zone, changes)
	out := []Record{}
	for _, change := range completed {
		if change.After != nil {
			out = append(out, *change.After)
		} else if change.Before != nil {
			out = append(out, *change.Before)
		}
	}
	return out, e
}
func (a *Adapter) Rollback(ctx context.Context, zone string, changes []Change) error {
	reverse := []Change{}
	for i := len(changes) - 1; i >= 0; i-- {
		c := changes[i]
		switch c.Action {
		case "create":
			reverse = append(reverse, Change{Action: "delete", Before: c.After})
		case "delete":
			reverse = append(reverse, Change{Action: "create", After: c.Before})
		case "update":
			reverse = append(reverse, Change{Action: "update", Before: c.After, After: c.Before})
		}
	}
	_, e := a.Apply(ctx, zone, reverse)
	return e
}
func (a *Adapter) write(ctx context.Context, zone string, c Change) (*Record, error) {
	r := c.After
	if c.Action == "delete" {
		r = c.Before
	}
	if r == nil {
		return nil, errors.New("change has no record")
	}
	if c.Action != "create" && c.Action != "update" && c.Action != "delete" {
		return nil, errors.New("invalid DNS change action")
	}
	if a.Provider != "cloudflare" && a.Provider != "dnspod" && a.Provider != "alidns" {
		return nil, errors.New("unsupported DNS provider")
	}
	if a.Provider != "cloudflare" && r.Proxied {
		return nil, errors.New("proxying is only supported by Cloudflare")
	}
	if a.Provider == "cloudflare" {
		if r.Line != "" && r.Line != "default" {
			return nil, errors.New("Cloudflare DNS only supports default line")
		}
		if r.Weight > 1 {
			return nil, errors.New("Cloudflare DNS does not support weighted records")
		}
		method := "POST"
		path := "/zones/" + url.PathEscape(zone) + "/dns_records"
		if c.Action != "create" {
			method = "PUT"
			id := r.ID
			if c.Before != nil {
				id = c.Before.ID
			}
			path += "/" + url.PathEscape(id)
		}
		if c.Action == "delete" {
			method = "DELETE"
		}
		var payload any = domain.Doc{"type": r.Type, "name": r.Name, "content": r.Value, "ttl": r.TTL, "proxied": r.Proxied}
		if c.Action == "delete" {
			payload = nil
		}
		d, e := a.request(ctx, method, path, payload)
		if e != nil {
			return nil, e
		}
		out := *r
		out.ID = domain.S(domain.M(d, "result"), "id")
		if out.ID == "" {
			return nil, &Error{Code: "missing_record_id", Ambiguous: true}
		}
		return &out, nil
	}
	name := strings.TrimSuffix(r.Name, "."+zone)
	if r.Name == zone {
		name = "@"
	}
	if a.Provider == "dnspod" {
		action := "CreateRecord"
		p := domain.Doc{"Domain": zone, "SubDomain": name, "RecordType": r.Type, "RecordLineId": r.Line, "Value": r.Value, "TTL": r.TTL}
		if r.Line == "" || r.Line == "default" {
			delete(p, "RecordLineId")
			p["RecordLine"] = "默认"
		}
		if r.Weight > 0 {
			p["Weight"] = r.Weight
		}
		if c.Action != "create" {
			action = "ModifyRecord"
			id := r.ID
			if c.Before != nil {
				id = c.Before.ID
			}
			n, e := strconv.ParseUint(id, 10, 64)
			if e != nil {
				return nil, errors.New("invalid provider record ID")
			}
			p["RecordId"] = n
		}
		if c.Action == "delete" {
			action = "DeleteRecord"
			p = domain.Doc{"Domain": zone, "RecordId": p["RecordId"]}
		}
		d, e := a.tencent(ctx, action, p)
		if e != nil {
			return nil, e
		}
		out := *r
		if id := domain.S(d, "RecordId"); id != "" {
			out.ID = id
		}
		if out.ID == "" {
			return nil, &Error{Code: "missing_record_id", Ambiguous: true}
		}
		return &out, nil
	}
	if r.Weight > 1 && c.Action != "delete" {
		if e := a.checkAliWeight(ctx, zone, *r); e != nil {
			return nil, e
		}
	}
	action := "AddDomainRecord"
	p := url.Values{"DomainName": {zone}, "RR": {name}, "Type": {r.Type}, "Value": {r.Value}, "TTL": {strconv.Itoa(r.TTL)}, "Line": {r.Line}}
	if r.Line == "" {
		p.Set("Line", "default")
	}
	if c.Action != "create" {
		action = "UpdateDomainRecord"
		id := r.ID
		if c.Before != nil {
			id = c.Before.ID
		}
		p.Set("RecordId", id)
	}
	if c.Action == "delete" {
		action = "DeleteDomainRecord"
		p = url.Values{"RecordId": {p.Get("RecordId")}}
	}
	d, e := a.aliyun(ctx, action, p)
	if e != nil {
		return nil, e
	}
	out := *r
	if id := domain.S(d, "RecordId"); id != "" {
		out.ID = id
	}
	if out.ID == "" {
		return nil, &Error{Code: "missing_record_id", Ambiguous: true}
	}
	if c.Action != "delete" && r.Weight > 1 {
		_, e = a.aliyun(ctx, "UpdateDNSSLBWeight", url.Values{"RecordId": {out.ID}, "Weight": {strconv.Itoa(r.Weight)}})
		if e != nil {
			return &out, e
		}
	}
	return &out, nil
}
func sha(v string) string { h := sha256.Sum256([]byte(v)); return hex.EncodeToString(h[:]) }
func mac256(key []byte, s string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(s))
	return h.Sum(nil)
}
func (a *Adapter) tencent(ctx context.Context, action string, p domain.Doc) (domain.Doc, error) {
	body, e := json.Marshal(p)
	if e != nil {
		return nil, e
	}
	host := "dnspod.tencentcloudapi.com"
	base := "https://" + host
	if a.BaseURL != "" {
		base = a.BaseURL
	}
	now := time.Now().UTC()
	date := now.Format("2006-01-02")
	ts := strconv.FormatInt(now.Unix(), 10)
	canonical := "POST\n/\n\ncontent-type:application/json; charset=utf-8\nhost:" + host + "\nx-tc-action:" + strings.ToLower(action) + "\n\ncontent-type;host;x-tc-action\n" + sha(string(body))
	scope := date + "/dnspod/tc3_request"
	toSign := "TC3-HMAC-SHA256\n" + ts + "\n" + scope + "\n" + sha(canonical)
	key := mac256([]byte("TC3"+domain.S(a.Credentials, "secret_key")), date)
	key = mac256(key, "dnspod")
	key = mac256(key, "tc3_request")
	signature := hex.EncodeToString(mac256(key, toSign))
	req, e := http.NewRequestWithContext(ctx, "POST", base, bytes.NewReader(body))
	if e != nil {
		return nil, e
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Host = host
	req.Header.Set("X-TC-Action", action)
	req.Header.Set("X-TC-Version", "2021-03-23")
	req.Header.Set("X-TC-Timestamp", ts)
	req.Header.Set("Authorization", "TC3-HMAC-SHA256 Credential="+domain.S(a.Credentials, "secret_id")+"/"+scope+", SignedHeaders=content-type;host;x-tc-action, Signature="+signature)
	return a.signedDo(req, "Response")
}
func percent(s string) string { return strings.ReplaceAll(url.QueryEscape(s), "+", "%20") }
func (a *Adapter) aliyun(ctx context.Context, action string, p url.Values) (domain.Doc, error) {
	copyParams := url.Values{}
	for k, v := range p {
		copyParams[k] = append([]string(nil), v...)
	}
	p = copyParams
	p.Set("Action", action)
	p.Set("Version", "2015-01-09")
	p.Set("Format", "JSON")
	p.Set("AccessKeyId", domain.S(a.Credentials, "access_key_id"))
	p.Set("SignatureMethod", "HMAC-SHA1")
	p.Set("SignatureVersion", "1.0")
	p.Set("SignatureNonce", domain.ID())
	p.Set("Timestamp", time.Now().UTC().Format("2006-01-02T15:04:05Z"))
	keys := []string{}
	for k := range p {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := []string{}
	for _, k := range keys {
		parts = append(parts, percent(k)+"="+percent(p.Get(k)))
	}
	canonical := strings.Join(parts, "&")
	h := hmac.New(sha1.New, []byte(domain.S(a.Credentials, "access_key_secret")+"&"))
	h.Write([]byte("POST&%2F&" + percent(canonical)))
	p.Set("Signature", base64.StdEncoding.EncodeToString(h.Sum(nil)))
	base := a.BaseURL
	if base == "" {
		base = "https://alidns.aliyuncs.com/"
	}
	req, e := http.NewRequestWithContext(ctx, "POST", base, strings.NewReader(p.Encode()))
	if e != nil {
		return nil, e
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return a.signedDo(req, "")
}
func (a *Adapter) signedDo(req *http.Request, envelope string) (domain.Doc, error) {
	action := req.Header.Get("X-TC-Action")
	if action == "" && req.GetBody != nil {
		b, _ := req.GetBody()
		if b != nil {
			raw, _ := io.ReadAll(b)
			b.Close()
			v, _ := url.ParseQuery(string(raw))
			action = v.Get("Action")
		}
	}
	return a.doJSON(req, envelope, strings.HasPrefix(action, "Describe") || strings.HasPrefix(action, "Get"))
}

func (a *Adapter) SaaS(ctx context.Context, method, zone, resource string, payload domain.Doc) (domain.Doc, error) {
	if a.Provider != "cloudflare" {
		return nil, errors.New("Cloudflare for SaaS requires a Cloudflare connection")
	}
	d, e := a.request(ctx, method, "/zones/"+url.PathEscape(zone)+"/"+resource, payload)
	if e != nil {
		return nil, e
	}
	return d, nil
}
