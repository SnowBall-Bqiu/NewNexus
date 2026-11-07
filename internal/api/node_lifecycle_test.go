package api

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/newnexus/newnexus/internal/domain"
	"google.golang.org/protobuf/types/known/structpb"
)

func TestNodeRedeploymentRequiresConfirmationAndKeepsIdentity(t *testing.T) {
	s := testServer(t)
	cookie, csrf := setup(t, s)
	ctx := context.Background()
	node := domain.Doc{"id": domain.ID(), "name": "original", "status": "revoked", "credential_hash": "old", "group_name": "edge", "mode": "tcp", "tasks_completed": 9, "session_generation": 1}
	if err := s.Store.Put(ctx, "nodes", node); err != nil {
		t.Fatal(err)
	}
	path := "nodes/" + domain.S(node, "id") + "/redeploy"
	if w := request(t, s, "POST", path, domain.Doc{"confirmed": true}, nil, ""); w.Code != 401 {
		t.Fatal("authentication", w.Code)
	}
	if w := request(t, s, "POST", path, domain.Doc{"confirmed": true}, cookie, ""); w.Code != 403 {
		t.Fatal("CSRF", w.Code)
	}
	for _, payload := range []domain.Doc{{}, {"confirmed": false}, {"confirmed": "true"}, {"confirmed": true, "expires_in_seconds": "900"}, {"confirmed": true, "expires_in_seconds": true}, {"confirmed": true, "expires_in_seconds": nil}, {"confirmed": true, "expires_in_seconds": 0}, {"confirmed": true, "expires_in_seconds": 901}, {"confirmed": true, "expires_in_seconds": 60.5}} {
		if w := request(t, s, "POST", path, payload, cookie, csrf); w.Code != 400 {
			t.Fatalf("invalid payload accepted: %#v: %d", payload, w.Code)
		}
	}
	w := request(t, s, "POST", path, domain.Doc{"confirmed": true, "expires_in_seconds": 900}, cookie, csrf)
	if w.Code != 201 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal(w.Code, w.Body.String())
	}
	var out domain.Doc
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if domain.S(out, "token") == "" || domain.S(out, "expires_at") == "" || out["node_id"] != node["id"] || out["name"] != "original" {
		t.Fatal("unexpected redeployment response")
	}
	visible := request(t, s, "GET", "nodes/"+domain.S(node, "id"), nil, cookie, "")
	for _, key := range []string{"credential_hash", "session_generation", "redeploy_token_hash"} {
		if strings.Contains(visible.Body.String(), key) {
			t.Fatal("internal state exposed", key)
		}
	}
	req, _ := structpb.NewStruct(map[string]any{"enrollment_token": out["token"], "protocol_version": 1, "name": "replacement"})
	registered, err := s.Hub.Enroll(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if registered.AsMap()["node_id"] != node["id"] {
		t.Fatal("redeployment changed node identity")
	}
	current, err := s.Store.Get(ctx, "nodes", domain.S(node, "id"))
	if err != nil || current["name"] != "original" || current["mode"] != "tcp" || current["group_name"] != "edge" || domain.N(current, "tasks_completed") != 9 {
		t.Fatal("configuration lost", err)
	}
	audit, err := s.Store.List(ctx, "audit")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range audit {
		if event["action"] == "redeploy" && event["resource_id"] == node["id"] {
			found = true
		}
		if strings.Contains(fmt.Sprint(event), domain.S(out, "token")) {
			t.Fatal("token leaked to audit")
		}
	}
	if !found {
		t.Fatal("redeployment missing audit")
	}
}

func TestNodeDeleteClearsMembershipAndInvalidatesRedeployment(t *testing.T) {
	s := testServer(t)
	cookie, csrf := setup(t, s)
	ctx := context.Background()
	id := domain.ID()
	if err := s.Store.Put(ctx, "nodes", domain.Doc{"id": id, "name": "delete", "status": "offline"}); err != nil {
		t.Fatal(err)
	}
	group := domain.Doc{"id": domain.ID(), "name": "group", "node_ids": []any{id}}
	if err := s.Store.Put(ctx, "node-groups", group); err != nil {
		t.Fatal(err)
	}
	token, err := s.Hub.Redeploy(ctx, id, 0)
	if err != nil {
		t.Fatal(err)
	}
	path := "nodes/" + id
	if w := request(t, s, "DELETE", path, nil, nil, ""); w.Code != 401 {
		t.Fatal("delete auth", w.Code)
	}
	if w := request(t, s, "DELETE", path, nil, cookie, ""); w.Code != 403 {
		t.Fatal("delete CSRF", w.Code)
	}
	if w := request(t, s, "DELETE", path, nil, cookie, csrf); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := request(t, s, "GET", path, nil, cookie, ""); w.Code != 404 {
		t.Fatal("deleted node still visible", w.Code)
	}
	group, _ = s.Store.Get(ctx, "node-groups", domain.S(group, "id"))
	if members, _ := group["node_ids"].([]any); len(members) != 0 {
		t.Fatal("deleted node still in group")
	}
	req, _ := structpb.NewStruct(map[string]any{"enrollment_token": token["token"], "protocol_version": 1})
	if _, err = s.Hub.Enroll(ctx, req); err == nil {
		t.Fatal("deleted node token accepted")
	}
	audit, _ := s.Store.List(ctx, "audit")
	found := false
	for _, event := range audit {
		if event["action"] == "delete" && event["resource_id"] == id {
			found = true
		}
	}
	if !found {
		t.Fatal("deletion audit missing")
	}
}
