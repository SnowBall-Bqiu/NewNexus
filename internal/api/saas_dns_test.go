package api

import (
	"context"
	"testing"

	"github.com/SnowBall-Bqiu/NewNexus/internal/provider"
)

type recordAdapter struct {
	records []provider.Record
	applied []provider.RecordChange
}

func (a *recordAdapter) Name() string { return "test" }
func (a *recordAdapter) Validate(context.Context, provider.Credentials) (provider.Capability, error) {
	return provider.Capability{DNS: true}, nil
}
func (a *recordAdapter) ListZones(context.Context, provider.Credentials) ([]provider.Zone, error) {
	return nil, nil
}
func (a *recordAdapter) ListLines(context.Context, provider.Credentials, provider.Zone) ([]provider.Line, error) {
	return nil, nil
}
func (a *recordAdapter) ListRecords(context.Context, provider.Credentials, provider.Zone, string) ([]provider.Record, error) {
	return append([]provider.Record(nil), a.records...), nil
}
func (a *recordAdapter) ApplyChanges(_ context.Context, _ provider.Credentials, _ provider.Zone, changes []provider.RecordChange) ([]provider.Record, error) {
	a.applied = append(a.applied, changes...)
	for _, change := range changes {
		if change.After == nil {
			continue
		}
		after := *change.After
		if change.Operation == "update" && change.Before != nil {
			for index := range a.records {
				if a.records[index].ID == change.Before.ID {
					after.ID = change.Before.ID
					a.records[index] = after
				}
			}
		} else {
			after.ID = "created"
			a.records = append(a.records, after)
		}
	}
	return a.records, nil
}

func TestEnsureSaaSDNSRecordIsIdempotent(t *testing.T) {
	adapter := &recordAdapter{records: []provider.Record{{ID: "txt", Name: "_cf.example.com.", Type: "TXT", Value: `"token"`}}}
	desired := provider.Record{Name: "_cf.example.com", Type: "TXT", Value: "token", TTL: 60}
	if err := ensureSaaSDNSRecord(context.Background(), adapter, nil, provider.Zone{Name: "example.com"}, desired, false); err != nil {
		t.Fatal(err)
	}
	if len(adapter.applied) != 0 {
		t.Fatalf("idempotent TXT reconciliation applied changes: %#v", adapter.applied)
	}
}

func TestEnsureSaaSDNSRecordUpdatesCNAME(t *testing.T) {
	adapter := &recordAdapter{records: []provider.Record{{ID: "cname", Name: "www.example.com", Type: "CNAME", Value: "old.example.net"}}}
	desired := provider.Record{Name: "www.example.com", Type: "CNAME", Value: "target.example.net", TTL: 60}
	if err := ensureSaaSDNSRecord(context.Background(), adapter, nil, provider.Zone{Name: "example.com"}, desired, true); err != nil {
		t.Fatal(err)
	}
	if len(adapter.applied) != 1 || adapter.applied[0].Operation != "update" {
		t.Fatalf("expected one CNAME update, got %#v", adapter.applied)
	}
}

func TestEnsureSaaSDNSRecordRejectsCNAMEConflict(t *testing.T) {
	adapter := &recordAdapter{records: []provider.Record{{ID: "address", Name: "www.example.com", Type: "A", Value: "192.0.2.1"}}}
	desired := provider.Record{Name: "www.example.com", Type: "CNAME", Value: "target.example.net", TTL: 60}
	if err := ensureSaaSDNSRecord(context.Background(), adapter, nil, provider.Zone{Name: "example.com"}, desired, true); err == nil {
		t.Fatal("expected an A/CNAME conflict")
	}
	if len(adapter.applied) != 0 {
		t.Fatalf("conflicting records must not be changed: %#v", adapter.applied)
	}
}
