package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/newnexus/newnexus/internal/domain"
)

func testAdapter(t *testing.T, kind string, handler http.HandlerFunc) *Adapter {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	a := New(kind, domain.Doc{"api_token": "do-not-leak", "secret_id": "test-id", "secret_key": "do-not-leak", "access_key_id": "test-id", "access_key_secret": "do-not-leak"})
	a.BaseURL = server.URL
	a.Client = server.Client()
	return a
}
func cfJSON(w http.ResponseWriter, result any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(domain.Doc{"success": true, "result": result})
}

func TestCloudflarePaginationAndRateLimit(t *testing.T) {
	var calls atomic.Int32
	a := testAdapter(t, "cloudflare", func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer do-not-leak" {
			t.Error("missing bearer header")
		}
		if n == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(429)
			_, _ = w.Write([]byte(`{"success":false,"errors":[{"code":1015}]}`))
			return
		}
		items := []domain.Doc{}
		if r.URL.Query().Get("page") == "1" {
			for i := 0; i < 50; i++ {
				items = append(items, domain.Doc{"id": fmt.Sprint(i), "name": fmt.Sprintf("%d.example.com", i)})
			}
		} else {
			items = append(items, domain.Doc{"id": "last", "name": "last.example.com"})
		}
		cfJSON(w, items)
	})
	zones, e := a.Zones(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	if len(zones) != 51 || calls.Load() != 3 {
		t.Fatalf("pagination/rate-limit contract: zones=%d calls=%d", len(zones), calls.Load())
	}
}

func TestMutationServerFailureIsNotRetriedAndRedactsBody(t *testing.T) {
	var calls atomic.Int32
	a := testAdapter(t, "cloudflare", func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"success":false,"errors":[{"code":1000,"message":"api_token do-not-leak"}]}`))
	})
	_, e := a.Apply(context.Background(), "zone", []Change{{Action: "create", After: &Record{Name: "www.example.com", Type: "A", Value: "1.1.1.1", TTL: 300, Weight: 1, Line: "default"}}})
	var external *Error
	if !errors.As(e, &external) || !external.Ambiguous {
		t.Fatalf("expected ambiguous write error, got %v", e)
	}
	if calls.Load() != 1 {
		t.Fatal("a potentially successful mutation was retried")
	}
	if strings.Contains(e.Error(), "do-not-leak") {
		t.Fatal("provider error leaked credentials")
	}
}

func TestApplyReturnsRealizedSubsetAndRollbackUsesProviderIDs(t *testing.T) {
	deleted := ""
	calls := 0
	a := testAdapter(t, "cloudflare", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			calls++
			if calls == 2 {
				w.WriteHeader(403)
				_, _ = w.Write([]byte(`{"success":false,"errors":[{"code":10000}]}`))
				return
			}
			cfJSON(w, domain.Doc{"id": "actual-record-id"})
		case http.MethodDelete:
			deleted = r.URL.Path
			cfJSON(w, domain.Doc{"id": "actual-record-id"})
		}
	})
	first := Record{Name: "a.example.com", Type: "A", Value: "1.1.1.1", TTL: 300, Line: "default", Weight: 1}
	second := first
	second.Name = "b.example.com"
	done, e := a.ApplyChanges(context.Background(), "zone", []Change{{Action: "create", After: &first}, {Action: "create", After: &second}})
	if e == nil || len(done) != 1 || done[0].After.ID != "actual-record-id" {
		t.Fatalf("lost partial-success IDs: %+v %v", done, e)
	}
	if e = a.Rollback(context.Background(), "zone", done); e != nil {
		t.Fatal(e)
	}
	if deleted != "/zones/zone/dns_records/actual-record-id" {
		t.Fatalf("rollback deleted wrong record: %s", deleted)
	}
}

func TestDNSPodSignatureHeadersEmptyListAndExactNumericIDs(t *testing.T) {
	a := testAdapter(t, "dnspod", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "TC3-HMAC-SHA256 Credential=test-id/") || r.Header.Get("X-TC-Version") != "2021-03-23" || r.Host != "dnspod.tencentcloudapi.com" {
			t.Error("invalid TC3 request envelope")
		}
		body := domain.Doc{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["ErrorOnEmpty"] != "no" {
			t.Error("empty DNS zones must return an empty page")
		}
		_, _ = w.Write([]byte(`{"Response":{"RecordList":[{"RecordId":9007199254740993,"Name":"www","Type":"CNAME","Value":"edge.example.net","TTL":600,"LineId":"0","Weight":null}]}}`))
	})
	rows, e := a.Records(context.Background(), "example.com")
	if e != nil {
		t.Fatal(e)
	}
	if len(rows) != 1 || rows[0].ID != "9007199254740993" || rows[0].Line != "default" || rows[0].Weight != 1 {
		t.Fatalf("record normalization lost information: %+v", rows)
	}
}

func TestAliDNSRejectsUnsupportedWeightWithoutWriting(t *testing.T) {
	writes := 0
	a := testAdapter(t, "alidns", func(w http.ResponseWriter, r *http.Request) {
		if e := r.ParseForm(); e != nil {
			t.Fatal(e)
		}
		if r.Form.Get("Signature") == "" || r.Form.Get("SignatureNonce") == "" {
			t.Error("unsigned AliDNS request")
		}
		if r.Form.Get("Action") != "DescribeDNSSLBSubDomains" {
			writes++
		}
		_, _ = w.Write([]byte(`{"RequestId":"req","SlbSubDomains":{"SlbSubDomain":[]}}`))
	})
	_, e := a.Apply(context.Background(), "example.com", []Change{{Action: "create", After: &Record{Name: "www.example.com", Type: "CNAME", Value: "edge.example.net", TTL: 600, Line: "default", Weight: 30}}})
	if e == nil || writes != 0 {
		t.Fatalf("unsupported weight must fail before writes, err=%v writes=%d", e, writes)
	}
}
