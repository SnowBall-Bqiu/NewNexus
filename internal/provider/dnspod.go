package provider

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

type DNSPod struct{ HTTPClient }

func NewDNSPod() *DNSPod       { return &DNSPod{HTTPClient: NewHTTPClient()} }
func (d *DNSPod) Name() string { return "dnspod" }

func (d *DNSPod) call(ctx context.Context, credentials Credentials, action string, input any, output any) error {
	if err := Required(credentials, "secretId", "secretKey"); err != nil {
		return err
	}
	payload, err := json.Marshal(input)
	if err != nil {
		return err
	}
	timestamp := time.Now().UTC().Unix()
	date := time.Unix(timestamp, 0).UTC().Format("2006-01-02")
	hashedPayload := sha256Hex(payload)
	canonicalHeaders := "content-type:application/json; charset=utf-8\nhost:dnspod.tencentcloudapi.com\n"
	canonicalRequest := "POST\n/\n\n" + canonicalHeaders + "\ncontent-type;host\n" + hashedPayload
	scope := date + "/dnspod/tc3_request"
	stringToSign := "TC3-HMAC-SHA256\n" + strconv.FormatInt(timestamp, 10) + "\n" + scope + "\n" + sha256Hex([]byte(canonicalRequest))
	secretDate := hmacSHA256([]byte("TC3"+credentials["secretKey"]), []byte(date))
	secretService := hmacSHA256(secretDate, []byte("dnspod"))
	secretSigning := hmacSHA256(secretService, []byte("tc3_request"))
	signature := hex.EncodeToString(hmacSHA256(secretSigning, []byte(stringToSign)))
	authorization := "TC3-HMAC-SHA256 Credential=" + credentials["secretId"] + "/" + scope + ", SignedHeaders=content-type;host, Signature=" + signature
	headers := map[string]string{"Authorization": authorization, "X-TC-Action": action, "X-TC-Version": "2021-03-23", "X-TC-Timestamp": strconv.FormatInt(timestamp, 10), "X-TC-Region": "", "Content-Type": "application/json; charset=utf-8"}
	return d.JSON(ctx, http.MethodPost, "https://dnspod.tencentcloudapi.com/", headers, json.RawMessage(payload), output)
}

func (d *DNSPod) Validate(ctx context.Context, credentials Credentials) (Capability, error) {
	var response struct {
		Response struct {
			RequestID string                          `json:"RequestId"`
			Error     *struct{ Code, Message string } `json:"Error"`
		} `json:"Response"`
	}
	if err := d.call(ctx, credentials, "DescribeDomainList", map[string]any{"Offset": 0, "Limit": 1}, &response); err != nil {
		return Capability{}, err
	}
	if response.Response.Error != nil {
		return Capability{}, fmt.Errorf("%s: %s", response.Response.Error.Code, response.Response.Error.Message)
	}
	return Capability{DNS: true, Lines: true, Weights: true}, nil
}

func (d *DNSPod) ListZones(ctx context.Context, credentials Credentials) ([]Zone, error) {
	var response struct {
		Response struct {
			DomainList []struct {
				DomainID     uint64 `json:"DomainId"`
				Name, Status string
			} `json:"DomainList"`
			Error *struct{ Code, Message string } `json:"Error"`
		} `json:"Response"`
	}
	if err := d.call(ctx, credentials, "DescribeDomainList", map[string]any{"Offset": 0, "Limit": 100}, &response); err != nil {
		return nil, err
	}
	if response.Response.Error != nil {
		return nil, fmt.Errorf("%s: %s", response.Response.Error.Code, response.Response.Error.Message)
	}
	zones := make([]Zone, 0, len(response.Response.DomainList))
	for _, item := range response.Response.DomainList {
		zones = append(zones, Zone{ID: strconv.FormatUint(item.DomainID, 10), Name: item.Name, Status: item.Status})
	}
	return zones, nil
}

func (d *DNSPod) ListLines(ctx context.Context, credentials Credentials, zone Zone) ([]Line, error) {
	var response struct {
		Response struct {
			LineList []struct{ LineID, Name string } `json:"LineList"`
			Error    *struct{ Code, Message string } `json:"Error"`
		} `json:"Response"`
	}
	if err := d.call(ctx, credentials, "DescribeRecordLineList", map[string]any{"Domain": zone.Name, "DomainGrade": "DP_FREE"}, &response); err != nil {
		return nil, err
	}
	if response.Response.Error != nil {
		return nil, fmt.Errorf("%s: %s", response.Response.Error.Code, response.Response.Error.Message)
	}
	lines := make([]Line, 0, len(response.Response.LineList))
	for _, item := range response.Response.LineList {
		lines = append(lines, Line{Code: item.LineID, Name: item.Name})
	}
	return lines, nil
}

func (d *DNSPod) ListRecords(ctx context.Context, credentials Credentials, zone Zone, name string) ([]Record, error) {
	input := map[string]any{"Domain": zone.Name, "Offset": 0, "Limit": 100}
	if name != "" {
		input["Subdomain"] = relativeName(name, zone.Name)
	}
	var response struct {
		Response struct {
			RecordList []struct {
				RecordID                                uint64 `json:"RecordId"`
				Name, Type, Value, Line, LineID, Status string
				TTL, Weight                             int
			} `json:"RecordList"`
			Error *struct{ Code, Message string } `json:"Error"`
		} `json:"Response"`
	}
	if err := d.call(ctx, credentials, "DescribeRecordList", input, &response); err != nil {
		return nil, err
	}
	if response.Response.Error != nil {
		return nil, fmt.Errorf("%s: %s", response.Response.Error.Code, response.Response.Error.Message)
	}
	records := make([]Record, 0, len(response.Response.RecordList))
	for _, item := range response.Response.RecordList {
		records = append(records, Record{ID: strconv.FormatUint(item.RecordID, 10), Name: item.Name + "." + zone.Name, Type: item.Type, Value: item.Value, LineCode: item.LineID, TTL: item.TTL, Weight: item.Weight, Enabled: item.Status == "ENABLE"})
	}
	return records, nil
}

func (d *DNSPod) ApplyChanges(ctx context.Context, credentials Credentials, zone Zone, changes []RecordChange) ([]Record, error) {
	result := make([]Record, 0, len(changes))
	for _, change := range changes {
		if change.Operation == "delete" {
			return nil, fmt.Errorf("DNSPod delete is intentionally disabled until release rollback has a verified snapshot")
		}
		if change.After == nil {
			return nil, fmt.Errorf("change is missing desired record")
		}
		input := map[string]any{"Domain": zone.Name, "SubDomain": relativeName(change.After.Name, zone.Name), "RecordType": change.After.Type, "RecordLineId": change.After.LineCode, "Value": change.After.Value, "TTL": change.After.TTL, "Weight": change.After.Weight}
		action := "CreateRecord"
		if change.Operation == "update" && change.Before != nil {
			action = "ModifyRecord"
			recordID, _ := strconv.ParseUint(change.Before.ID, 10, 64)
			input["RecordId"] = recordID
		}
		var response struct {
			Response struct {
				RecordID uint64                          `json:"RecordId"`
				Error    *struct{ Code, Message string } `json:"Error"`
			} `json:"Response"`
		}
		if err := d.call(ctx, credentials, action, input, &response); err != nil {
			return nil, err
		}
		if response.Response.Error != nil {
			return nil, fmt.Errorf("%s: %s", response.Response.Error.Code, response.Response.Error.Message)
		}
		record := *change.After
		record.ID = strconv.FormatUint(response.Response.RecordID, 10)
		result = append(result, record)
	}
	return result, nil
}

func sha256Hex(value []byte) string { sum := sha256.Sum256(value); return hex.EncodeToString(sum[:]) }
func hmacSHA256(key, value []byte) []byte {
	h := hmac.New(sha256.New, key)
	_, _ = h.Write(value)
	return h.Sum(nil)
}
func relativeName(name, zone string) string {
	if name == zone {
		return "@"
	}
	if len(name) > len(zone)+1 && name[len(name)-len(zone):] == zone {
		return name[:len(name)-len(zone)-1]
	}
	return name
}
