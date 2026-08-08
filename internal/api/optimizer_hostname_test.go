package api

import (
	"testing"

	"github.com/SnowBall-Bqiu/NewNexus/internal/provider"
)

func TestReconcileAddressRecordsCreatesUpdatesAndDeletes(t *testing.T) {
	existing := []provider.Record{
		{ID: "keep", Name: "edge.example.com", Type: "A", Value: "1.1.1.1", TTL: 300},
		{ID: "remove", Name: "edge.example.com", Type: "A", Value: "1.0.0.1", TTL: 60},
		{ID: "unmanaged", Name: "edge.example.com", Type: "AAAA", Value: "2606:4700::1111", TTL: 60},
	}
	changes := reconcileAddressRecords(existing, "edge.example.com", "A", 60, []string{"1.1.1.1", "8.8.8.8"})
	operations := map[string]int{}
	for _, change := range changes {
		operations[change.Operation]++
	}
	if operations["update"] != 1 || operations["create"] != 1 || operations["delete"] != 1 {
		t.Fatalf("unexpected address reconciliation: %#v", changes)
	}
}

func TestContainsAllAddresses(t *testing.T) {
	records := []provider.Record{{Type: "AAAA", Value: "2606:4700::1111"}}
	if !containsAllAddresses(records, "AAAA", []string{"2606:4700::1111"}) {
		t.Fatal("observed address was not confirmed")
	}
	if containsAllAddresses(records, "AAAA", []string{"2606:4700::1001"}) {
		t.Fatal("missing address was confirmed")
	}
}

func TestShellQuote(t *testing.T) {
	if got := shellQuote("hk-edge-01"); got != "'hk-edge-01'" {
		t.Fatalf("unexpected simple shell quote: %s", got)
	}
	if got := shellQuote("node's-edge"); got != `'node'"'"'s-edge'` {
		t.Fatalf("single quote was not escaped safely: %s", got)
	}
}
