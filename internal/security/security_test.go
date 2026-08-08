package security

import (
	"strings"
	"testing"
)

func TestPasswordHashAndVerification(t *testing.T) {
	encoded, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(encoded, "$argon2id$") {
		t.Fatalf("unexpected password encoding: %s", encoded)
	}
	if !VerifyPassword(encoded, "correct horse battery staple") {
		t.Fatal("correct password was rejected")
	}
	if VerifyPassword(encoded, "incorrect") {
		t.Fatal("incorrect password was accepted")
	}
}

func TestVaultRoundTripAndTamperDetection(t *testing.T) {
	vault, err := NewVault([]byte("01234567890123456789012345678901"))
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := vault.Encrypt(`{"token":"sensitive"}`)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := vault.Decrypt(sealed)
	if err != nil {
		t.Fatal(err)
	}
	if plain != `{"token":"sensitive"}` {
		t.Fatalf("unexpected plaintext: %s", plain)
	}
	position := len(sealed) / 2
	last := sealed[position]
	replacement := byte('A')
	if last == replacement {
		replacement = 'B'
	}
	tampered := sealed[:position] + string(replacement) + sealed[position+1:]
	if _, err := vault.Decrypt(tampered); err == nil {
		t.Fatal("tampered ciphertext was accepted")
	}
}
