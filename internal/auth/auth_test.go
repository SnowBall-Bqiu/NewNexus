package auth

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SnowBall-Bqiu/NewNexus/internal/config"
	"github.com/SnowBall-Bqiu/NewNexus/internal/domain"
	"github.com/SnowBall-Bqiu/NewNexus/internal/security"
	"github.com/SnowBall-Bqiu/NewNexus/internal/store"
	"github.com/pquerna/otp/totp"
)

func TestTOTPRebindKeepsExistingSecretUntilConfirmation(t *testing.T) {
	database, err := store.Open(config.Config{DatabaseDriver: "sqlite", DatabaseURL: filepath.Join(t.TempDir(), "auth.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	vault, err := security.NewVault(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManager(database, vault, config.Config{})
	admin, _, err := manager.SetupWithRecovery("administrator", "a-secure-password")
	if err != nil {
		t.Fatal(err)
	}
	firstSecret, _, err := manager.StartTOTP(*admin)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.DB.First(admin, "id = ?", admin.ID).Error; err != nil {
		t.Fatal(err)
	}
	firstCode, _ := totp.GenerateCode(firstSecret, time.Now())
	if err := manager.ConfirmTOTP(*admin, firstCode); err != nil {
		t.Fatal(err)
	}
	if err := database.DB.First(admin, "id = ?", admin.ID).Error; err != nil {
		t.Fatal(err)
	}
	firstEncrypted := admin.TOTPSecretEnc
	secondSecret, _, err := manager.StartTOTP(*admin)
	if err != nil {
		t.Fatal(err)
	}
	var rebinding domain.Admin
	if err := database.DB.First(&rebinding, "id = ?", admin.ID).Error; err != nil {
		t.Fatal(err)
	}
	if !rebinding.TOTPEnabled || rebinding.TOTPSecretEnc != firstEncrypted || rebinding.PendingTOTPSecretEnc == "" {
		t.Fatalf("rebind disabled or replaced active TOTP before confirmation: %#v", rebinding)
	}
	firstCode, _ = totp.GenerateCode(firstSecret, time.Now())
	if _, err := manager.Login(admin.Username, "a-secure-password", firstCode); err != nil {
		t.Fatalf("existing TOTP stopped working during rebind: %v", err)
	}
	secondCode, _ := totp.GenerateCode(secondSecret, time.Now())
	if err := manager.ConfirmTOTP(rebinding, secondCode); err != nil {
		t.Fatal(err)
	}
	if err := database.DB.First(&rebinding, "id = ?", admin.ID).Error; err != nil {
		t.Fatal(err)
	}
	if rebinding.PendingTOTPSecretEnc != "" || rebinding.TOTPSecretEnc == firstEncrypted || !rebinding.TOTPEnabled {
		t.Fatalf("confirmed rebind did not atomically replace TOTP: %#v", rebinding)
	}
}

func TestCreateAPIKeyRejectsInvalidInput(t *testing.T) {
	database, err := store.Open(config.Config{DatabaseDriver: "sqlite", DatabaseURL: filepath.Join(t.TempDir(), "api-key.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	vault, err := security.NewVault(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManager(database, vault, config.Config{})
	admin, _, err := manager.SetupWithRecovery("administrator", "a-secure-password")
	if err != nil {
		t.Fatal(err)
	}

	past := time.Now().UTC().Add(-time.Minute)
	if _, _, err := manager.CreateAPIKey(admin.ID, "expired", "read", &past); !errors.Is(err, ErrInvalidAPIKeyInput) {
		t.Fatalf("expected invalid expiry error, got %v", err)
	}
	if _, _, err := manager.CreateAPIKey(admin.ID, "bad scope", "routes:delete", nil); !errors.Is(err, ErrInvalidAPIKeyInput) {
		t.Fatalf("expected invalid scope error, got %v", err)
	}
	if _, _, err := manager.CreateAPIKey(admin.ID, strings.Repeat("n", 121), "read", nil); !errors.Is(err, ErrInvalidAPIKeyInput) {
		t.Fatalf("expected invalid name error, got %v", err)
	}
}
