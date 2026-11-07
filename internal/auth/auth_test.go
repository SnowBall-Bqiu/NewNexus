package auth

import (
	"bytes"
	"context"
	"github.com/newnexus/newnexus/internal/security"
	"github.com/newnexus/newnexus/internal/testutil"
	"path/filepath"
	"testing"
	"time"
)

func service(t *testing.T) *Service {
	t.Helper()
	s := testutil.Open(t, filepath.Join(t.TempDir(), "auth.db"))
	v, _ := security.NewVault(bytes.Repeat([]byte{4}, 32))
	return &Service{Store: s, Vault: v}
}
func TestBootstrapRetryAndPasswordHash(t *testing.T) {
	s := service(t)
	ctx := context.Background()
	if _, e := s.Bootstrap(ctx, "admin", "short"); e == nil {
		t.Fatal("weak password allowed")
	}
	d, e := s.Bootstrap(ctx, "admin", "long-password-123")
	if e != nil {
		t.Fatal("failed bootstrap left lock", e)
	}
	if !PasswordOK(d["password_hash"].(string), "long-password-123") || PasswordOK(d["password_hash"].(string), "wrong") {
		t.Fatal("bad password verification")
	}
	if _, e = s.Bootstrap(ctx, "other", "long-password-123"); e == nil {
		t.Fatal("second administrator accepted")
	}
	if _, e = s.Login(ctx, "admin", "long-password-123", ""); e != nil {
		t.Fatal(e)
	}
}
func TestTOTPVector(t *testing.T) {
	secret := "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ"
	if step, ok := VerifyTOTP(secret, "287082", time.Unix(59, 0)); !ok || step != 1 {
		t.Fatal(step, ok)
	}
	if _, ok := VerifyTOTP(secret, "000000", time.Unix(59, 0)); ok {
		t.Fatal("invalid TOTP accepted")
	}
}
