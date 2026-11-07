// Package agenttls manages the opt-in private authority for Agent gRPC.
package agenttls

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"sync"
	"time"

	"github.com/newnexus/newnexus/internal/domain"
	"github.com/newnexus/newnexus/internal/security"
	"github.com/newnexus/newnexus/internal/store"
)

const SettingKey = "agent_tls.self_signed_enabled"
const authorityKind = "agent-tls"
const authorityID = "authority"
const authorityScope = "agent-tls:authority"
const leafLifetime = 90 * 24 * time.Hour
const renewBefore = 30 * 24 * time.Hour

type Material struct {
	CAPEM       []byte
	Certificate tls.Certificate
	ServerName  string
	NotAfter    time.Time
}

type PublicInfo struct {
	Enabled             bool   `json:"self_signed_enabled"`
	ServerName          string `json:"server_name,omitempty"`
	CAURL               string `json:"ca_url,omitempty"`
	CASHA256            string `json:"ca_sha256,omitempty"`
	CAFingerprintSHA256 string `json:"ca_fingerprint_sha256,omitempty"`
	NotAfter            string `json:"not_after,omitempty"`
}

type privateMaterial struct {
	CAPEM      []byte `json:"ca_pem"`
	CAKeyPEM   []byte `json:"ca_key_pem"`
	LeafPEM    []byte `json:"leaf_pem"`
	LeafKeyPEM []byte `json:"leaf_key_pem"`
	ServerName string `json:"server_name"`
}

type Manager struct {
	store *store.Store
	vault *security.Vault
	mu    sync.Mutex
	now   func() time.Time
}

func New(db *store.Store, vault *security.Vault) *Manager {
	return &Manager{store: db, vault: vault, now: time.Now}
}

func (m *Manager) Enabled(ctx context.Context) (bool, error) {
	setting, err := m.store.Get(ctx, "settings", SettingKey)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read Agent TLS setting: %w", err)
	}
	enabled, ok := setting["value"].(bool)
	if !ok {
		return false, errors.New("Agent TLS setting must be a boolean")
	}
	return enabled, nil
}

// Ensure prepares material independently of the switch, so a failed generation
// never leaves a settings write advertising unavailable TLS credentials.
func (m *Manager) Ensure(ctx context.Context) (*Material, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for attempt := 0; attempt < 8; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		record, err := m.store.Get(ctx, authorityKind, authorityID)
		var secret *privateMaterial
		changed := false
		if errors.Is(err, store.ErrNotFound) {
			secret, err = newAuthority(m.now().UTC())
			record = domain.Doc{"id": authorityID}
			changed = true
		} else if err == nil {
			secret, err = m.decode(record)
		}
		if err != nil {
			return nil, err
		}
		material, err := loadMaterial(secret, m.now().UTC())
		if err != nil {
			return nil, err
		}
		if material.NotAfter.Before(m.now().UTC().Add(renewBefore)) {
			if err := renewLeaf(secret, m.now().UTC()); err != nil {
				return nil, err
			}
			material, err = loadMaterial(secret, m.now().UTC())
			if err != nil {
				return nil, err
			}
			changed = true
		}
		if !changed {
			return material, nil
		}
		plain, err := json.Marshal(secret)
		if err != nil {
			return nil, err
		}
		sealed, err := m.vault.Seal(plain, authorityScope)
		if err != nil {
			return nil, err
		}
		record["encrypted_material"] = sealed
		if err := m.store.Put(ctx, authorityKind, record); errors.Is(err, store.ErrConflict) {
			// A competing replica has already committed its authority or renewal.
			// Reload its winner instead of publishing our uncommitted CA.
			continue
		} else if err != nil {
			return nil, err
		}
		return material, nil
	}
	return nil, errors.New("Agent TLS authority changed repeatedly")
}

func (m *Manager) Public(ctx context.Context) (*PublicInfo, error) {
	enabled, err := m.Enabled(ctx)
	if err != nil {
		return nil, err
	}
	info := &PublicInfo{Enabled: enabled}
	if !enabled {
		return info, nil
	}
	material, err := m.Ensure(ctx)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(material.CAPEM)
	caHash, fingerprint := sha256.Sum256(material.CAPEM), sha256.Sum256(block.Bytes)
	info.ServerName = material.ServerName
	info.CAURL = "/api/v1/agent-ca.pem"
	info.CASHA256 = hex.EncodeToString(caHash[:])
	info.CAFingerprintSHA256 = hex.EncodeToString(fingerprint[:])
	info.NotAfter = material.NotAfter.UTC().Format(time.RFC3339)
	return info, nil
}

func (m *Manager) decode(record domain.Doc) (*privateMaterial, error) {
	plain, err := m.vault.Open(domain.S(record, "encrypted_material"), authorityScope)
	if err != nil {
		return nil, fmt.Errorf("decrypt Agent TLS authority: %w", err)
	}
	secret := &privateMaterial{}
	if err := json.Unmarshal(plain, secret); err != nil {
		return nil, errors.New("invalid Agent TLS authority document")
	}
	return secret, nil
}

func parseAuthority(secret *privateMaterial, now time.Time) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	pair, err := tls.X509KeyPair(secret.CAPEM, secret.CAKeyPEM)
	if err != nil || len(pair.Certificate) != 1 {
		return nil, nil, errors.New("invalid Agent TLS authority key pair")
	}
	ca, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, nil, err
	}
	key, ok := pair.PrivateKey.(*ecdsa.PrivateKey)
	if !ok || !ca.IsCA || !ca.BasicConstraintsValid || ca.CheckSignatureFrom(ca) != nil || now.Before(ca.NotBefore) || !now.Before(ca.NotAfter) {
		return nil, nil, errors.New("invalid or expired Agent TLS authority")
	}
	publicDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return nil, nil, err
	}
	if secret.ServerName != authorityServerName(publicDER) {
		return nil, nil, errors.New("Agent TLS authority server name does not match its key")
	}
	return ca, key, nil
}

func loadMaterial(secret *privateMaterial, now time.Time) (*Material, error) {
	ca, _, err := parseAuthority(secret, now)
	if err != nil {
		return nil, err
	}
	certificate, err := tls.X509KeyPair(secret.LeafPEM, secret.LeafKeyPEM)
	if err != nil || len(certificate.Certificate) != 1 {
		return nil, errors.New("invalid Agent TLS server key pair")
	}
	leaf, err := x509.ParseCertificate(certificate.Certificate[0])
	if err != nil || leaf.CheckSignatureFrom(ca) != nil || leaf.VerifyHostname(secret.ServerName) != nil || now.Before(leaf.NotBefore) {
		return nil, errors.New("invalid Agent TLS server certificate")
	}
	certificate.Leaf = leaf
	return &Material{CAPEM: append([]byte(nil), secret.CAPEM...), Certificate: certificate, ServerName: secret.ServerName, NotAfter: leaf.NotAfter}, nil
}

func serialNumber() (*big.Int, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	return serial.Add(serial, big.NewInt(1)), nil
}

func authorityServerName(publicDER []byte) string {
	hash := sha256.Sum256(publicDER)
	return hex.EncodeToString(hash[:16]) + ".newnexus.invalid"
}

func encodeKey(key *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

func newAuthority(now time.Time) (*privateMaterial, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := serialNumber()
	if err != nil {
		return nil, err
	}
	publicDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return nil, err
	}
	subjectKeyID := sha256.Sum256(publicDER)
	template := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: "NewNexus Agent private authority"},
		NotBefore: now.Add(-5 * time.Minute), NotAfter: now.AddDate(10, 0, 0),
		IsCA: true, BasicConstraintsValid: true, MaxPathLen: 0, MaxPathLenZero: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign, SubjectKeyId: subjectKeyID[:20],
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	keyPEM, err := encodeKey(key)
	if err != nil {
		return nil, err
	}
	secret := &privateMaterial{CAPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), CAKeyPEM: keyPEM, ServerName: authorityServerName(publicDER)}
	if err := renewLeaf(secret, now); err != nil {
		return nil, err
	}
	return secret, nil
}

func renewLeaf(secret *privateMaterial, now time.Time) error {
	ca, caKey, err := parseAuthority(secret, now)
	if err != nil {
		return err
	}
	if !ca.NotAfter.After(now.Add(renewBefore)) {
		return errors.New("Agent TLS authority expires too soon; explicit authority migration is required")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, err := serialNumber()
	if err != nil {
		return err
	}
	notAfter := now.Add(leafLifetime)
	if ca.NotAfter.Before(notAfter) {
		notAfter = ca.NotAfter
	}
	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: secret.ServerName}, DNSNames: []string{secret.ServerName},
		NotBefore: now.Add(-5 * time.Minute), NotAfter: notAfter, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, ca, &key.PublicKey, caKey)
	if err != nil {
		return err
	}
	keyPEM, err := encodeKey(key)
	if err != nil {
		return err
	}
	secret.LeafPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	secret.LeafKeyPEM = keyPEM
	return nil
}
