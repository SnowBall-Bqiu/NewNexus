package main

import (
	"crypto/sha256"
	"fmt"
	"net/http/httptest"
	"testing"
)

func TestAgentTLSFingerprintVerification(t *testing.T) {
	server := httptest.NewTLSServer(nil)
	defer server.Close()
	certificate := server.Certificate()
	fingerprint := sha256.Sum256(certificate.Raw)
	config, err := agentTLSConfig("", fmt.Sprintf("%x", fingerprint[:]))
	if err != nil {
		t.Fatal(err)
	}
	if err := config.VerifyPeerCertificate([][]byte{certificate.Raw}, nil); err != nil {
		t.Fatalf("matching fingerprint was rejected: %v", err)
	}
	wrong := sha256.Sum256([]byte("wrong-certificate"))
	config, err = agentTLSConfig("", fmt.Sprintf("%x", wrong[:]))
	if err != nil {
		t.Fatal(err)
	}
	if err := config.VerifyPeerCertificate([][]byte{certificate.Raw}, nil); err == nil {
		t.Fatal("mismatched fingerprint was accepted")
	}
}

func TestAgentTLSFingerprintRejectsInvalidLength(t *testing.T) {
	if _, err := agentTLSConfig("", "abcd"); err == nil {
		t.Fatal("short fingerprint was accepted")
	}
}
