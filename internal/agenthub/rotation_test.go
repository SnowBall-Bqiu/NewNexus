package agenthub

import (
	"context"
	"errors"
	"testing"

	"github.com/newnexus/newnexus/internal/auth"
	"github.com/newnexus/newnexus/internal/domain"
	"github.com/newnexus/newnexus/internal/security"
)

func TestAtomicRotationKeepsBothCredentialsUntilAcknowledged(t *testing.T) {
	h, enrolled := fixture(t)
	ctx := context.Background()
	h.Vault, _ = security.NewVault(make([]byte, 32))
	nodeID, active := domain.S(enrolled, "node_id"), domain.S(enrolled, "credential")
	node, err := h.Store.Get(ctx, "nodes", nodeID)
	if err != nil {
		t.Fatal(err)
	}
	node["status"], node["mode"], node["tasks_completed"] = "online", "http", 72
	if err := h.Store.Put(ctx, "nodes", node); err != nil {
		t.Fatal(err)
	}
	// Rotate must only persist pending state. The Connect loop alone emits
	// credential frames, so an external delayed sender cannot replay an older
	// plaintext credential after a subsequent replacement has been confirmed.
	h.streams[nodeID] = make(chan domain.Doc, 1)
	for round := 0; round < 2; round++ {
		if err := h.Rotate(ctx, nodeID); err != nil {
			t.Fatal(err)
		}
		if len(h.streams[nodeID]) != 0 {
			t.Fatal("Rotate directly queued a stale-capable credential snapshot")
		}
		pending, err := h.Store.Get(ctx, "nodes", nodeID)
		if err != nil || domain.S(pending, "credential_hash") != auth.Hash(active) || domain.N(pending, "tasks_completed") != 72 || domain.S(pending, "mode") != "http" {
			t.Fatalf("rotation overwrote active credential/counters/config: %+v error=%v", pending, err)
		}
		plain, err := h.Vault.Open(domain.S(pending, "pending_credential_encrypted"), "node-rotation:"+nodeID)
		if err != nil || len(plain) == 0 || domain.S(pending, "pending_credential_hash") != auth.Hash(string(plain)) {
			t.Fatalf("pending credential was not durably recoverable: %v", err)
		}
		if _, err := h.authenticate(ctx, nodeID, active); err != nil {
			t.Fatal("old credential stopped working before persistence ACK:", err)
		}
		if _, err := h.authenticate(ctx, nodeID, string(plain)); err != nil {
			t.Fatal("persisted replacement cannot reconnect before ACK:", err)
		}
		if err := h.Rotate(ctx, nodeID); err == nil {
			t.Fatal("unacknowledged rotation was overwritten")
		}
		if err := h.credentialAckFrame(ctx, nodeID, "wrong-ack"); err != nil {
			t.Fatal(err)
		}
		unchanged, _ := h.Store.Get(ctx, "nodes", nodeID)
		if domain.S(unchanged, "credential_hash") != auth.Hash(active) || domain.S(unchanged, "pending_credential_hash") != auth.Hash(string(plain)) {
			t.Fatal("wrong ACK changed the durable credential")
		}
		if err := h.credentialAckFrame(ctx, nodeID, auth.Hash(string(plain))); err != nil {
			t.Fatal(err)
		}
		if _, err := h.authenticate(ctx, nodeID, active); err == nil {
			t.Fatal("old credential still accepted after replacement persistence ACK")
		}
		active = string(plain)
		if _, err := h.authenticate(ctx, nodeID, active); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.Revoke(ctx, nodeID); err != nil {
		t.Fatal(err)
	}
	if err := h.Rotate(ctx, nodeID); !errors.Is(err, errFrameNodeRevoked) {
		t.Fatal("still-registered local stream allowed revoked rotation:", err)
	}
	node, err = h.Store.Get(ctx, "nodes", nodeID)
	if err != nil || domain.S(node, "credential_hash") != "" || node["pending_credential_hash"] != nil || node["pending_credential_encrypted"] != nil || domain.N(node, "tasks_completed") != 72 {
		t.Fatalf("revocation was undone: %+v error=%v", node, err)
	}
}
