package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/newnexus/newnexus/internal/auth"
	"github.com/newnexus/newnexus/internal/domain"
)

func TestBootstrapRequiresOperatorProofAndRemainsOneTime(t *testing.T) {
	s := testServer(t)
	proof := strings.Repeat("random-setup-proof-", 3)
	s.SetupTokenHash = auth.Hash(proof)
	status := request(t, s, "GET", "bootstrap/status", nil, nil, "")
	d := domain.Doc{}
	_ = json.Unmarshal(status.Body.Bytes(), &d)
	if status.Code != 200 || !domain.B(d, "setup_token_required") || domain.B(d, "initialized") || strings.Contains(status.Body.String(), proof) {
		t.Fatal(status.Code, status.Body.String())
	}
	for _, input := range []string{"", "wrong"} {
		w := request(t, s, "POST", "bootstrap", domain.Doc{"username": "admin", "password": "long-password-123", "setup_token": input}, nil, "")
		if w.Code != 403 || !strings.Contains(w.Body.String(), "setup_token_invalid") || s.Auth.Initialized(t.Context()) {
			t.Fatal("bootstrap accepted missing or incorrect operator proof", w.Code, w.Body.String())
		}
	}
	w := request(t, s, "POST", "bootstrap", domain.Doc{"username": "admin", "password": "short", "setup_token": proof}, nil, "")
	if w.Code != 400 {
		t.Fatal("password validation was bypassed", w.Code, w.Body.String())
	}
	w = request(t, s, "POST", "bootstrap", domain.Doc{"username": "admin", "password": "long-password-123", "setup_token": proof}, nil, "")
	if w.Code != 200 || strings.Contains(w.Body.String(), proof) {
		t.Fatal("correct setup proof could not initialize after a failed attempt", w.Code, w.Body.String())
	}
	status = request(t, s, "GET", "bootstrap/status", nil, nil, "")
	_ = json.Unmarshal(status.Body.Bytes(), &d)
	if !domain.B(d, "initialized") || domain.B(d, "setup_token_required") {
		t.Fatal("setup proof remains required after initialization", status.Body.String())
	}
	w = request(t, s, "POST", "bootstrap", domain.Doc{"username": "attacker", "password": "long-password-123", "setup_token": proof}, nil, "")
	if w.Code != 400 {
		t.Fatal("setup proof initialized another administrator", w.Code, w.Body.String())
	}
	w = request(t, s, "POST", "auth/login", domain.Doc{"username": "admin", "password": "long-password-123"}, nil, "")
	if w.Code != 200 {
		t.Fatal("normal login incorrectly requires setup proof", w.Code, w.Body.String())
	}
}
