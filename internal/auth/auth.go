package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/newnexus/newnexus/internal/domain"
	"github.com/newnexus/newnexus/internal/security"
	"github.com/newnexus/newnexus/internal/store"
	"golang.org/x/crypto/argon2"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var ErrUnauthorized = errors.New("invalid username, password or authentication code")

type Service struct {
	Store        *store.Store
	Vault        *security.Vault
	SecureCookie bool
}

func Token() string {
	b := make([]byte, 32)
	if _, e := rand.Read(b); e != nil {
		panic(e)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
func Hash(v string) string { b := sha256.Sum256([]byte(v)); return hex.EncodeToString(b[:]) }
func PasswordHash(password string) (string, error) {
	if len(password) < 12 {
		return "", errors.New("password must be at least 12 characters")
	}
	salt := make([]byte, 16)
	if _, e := rand.Read(salt); e != nil {
		return "", e
	}
	hash := argon2.IDKey([]byte(password), salt, 3, 64*1024, 2, 32)
	return "$argon2id$v=19$m=65536,t=3,p=2$" + base64.RawStdEncoding.EncodeToString(salt) + "$" + base64.RawStdEncoding.EncodeToString(hash), nil
}
func PasswordOK(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 {
		return false
	}
	salt, e := base64.RawStdEncoding.DecodeString(parts[4])
	if e != nil {
		return false
	}
	expected, e := base64.RawStdEncoding.DecodeString(parts[5])
	if e != nil || len(expected) != 32 {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, 3, 64*1024, 2, 32)
	return subtle.ConstantTimeCompare(expected, got) == 1
}
func (s *Service) Initialized(ctx context.Context) bool { return s.Store.Count(ctx, "admin") > 0 }
func (s *Service) Bootstrap(ctx context.Context, name, password string) (domain.Doc, error) {
	owner := "bootstrap-" + domain.ID()
	ok, e := s.Store.Lease(ctx, "bootstrap", owner, time.Minute)
	if e != nil || !ok {
		return nil, errors.New("bootstrap is already in progress")
	}
	defer s.Store.ReleaseLease(ctx, "bootstrap", owner)
	if s.Initialized(ctx) {
		return nil, errors.New("administrator already exists")
	}
	if strings.TrimSpace(name) == "" {
		return nil, errors.New("username is required")
	}
	hash, e := PasswordHash(password)
	if e != nil {
		return nil, e
	}
	d := domain.Doc{"id": "singleton", "username": name, "password_hash": hash, "totp_enabled": false}
	e = s.Store.Put(ctx, "admin", d)
	return d, e
}
func (s *Service) Login(ctx context.Context, name, password, code string) (domain.Doc, error) {
	d, e := s.Store.Get(ctx, "admin", "singleton")
	if e != nil || domain.S(d, "username") != name || !PasswordOK(domain.S(d, "password_hash"), password) {
		return nil, ErrUnauthorized
	}
	if domain.B(d, "totp_enabled") {
		secret, e := s.Vault.Open(domain.S(d, "totp_secret"), "admin-totp")
		if e != nil {
			return nil, ErrUnauthorized
		}
		step, ok := VerifyTOTP(string(secret), code, time.Now())
		if !ok || float64(step) <= domain.N(d, "last_totp_step") {
			recovery := domain.M(d, "recovery_hashes")
			if !domain.B(recovery, Hash(code)) {
				return nil, ErrUnauthorized
			}
			delete(recovery, Hash(code))
			d["recovery_hashes"] = recovery
		} else {
			d["last_totp_step"] = step
		}
		if e = s.Store.Put(ctx, "admin", d); e != nil {
			return nil, e
		}
	}
	return d, nil
}
func (s *Service) NewSession(ctx context.Context, user domain.Doc) (string, domain.Doc, error) {
	token := Token()
	d := domain.Doc{"id": Hash(token), "username": user["username"], "csrf_token": Token(), "expires_at": time.Now().UTC().Add(12 * time.Hour).Format(time.RFC3339Nano)}
	e := s.Store.Put(ctx, "sessions", d)
	return token, d, e
}
func (s *Service) Authenticate(r *http.Request) (domain.Doc, bool, error) {
	ctx := r.Context()
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		d, e := s.Store.Get(ctx, "api-keys", Hash(strings.TrimPrefix(h, "Bearer ")))
		if e != nil || domain.B(d, "revoked") {
			return nil, true, ErrUnauthorized
		}
		return d, true, nil
	}
	cookie, e := r.Cookie("newnexus_session")
	if e != nil {
		return nil, false, ErrUnauthorized
	}
	d, e := s.Store.Get(ctx, "sessions", Hash(cookie.Value))
	if e != nil {
		return nil, false, ErrUnauthorized
	}
	expiry, e := time.Parse(time.RFC3339Nano, domain.S(d, "expires_at"))
	if e != nil || time.Now().After(expiry) {
		return nil, false, ErrUnauthorized
	}
	return d, false, nil
}
func (s *Service) SetCookie(w http.ResponseWriter, token string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: "newnexus_session", Value: token, Path: "/", HttpOnly: true, Secure: s.SecureCookie, SameSite: http.SameSiteStrictMode, MaxAge: maxAge})
}
func (s *Service) TOTPSetup(ctx context.Context) (domain.Doc, error) {
	d, e := s.Store.Get(ctx, "admin", "singleton")
	if e != nil {
		return nil, e
	}
	if domain.B(d, "totp_enabled") {
		return nil, errors.New("TOTP is already enabled")
	}
	b := make([]byte, 20)
	_, _ = rand.Read(b)
	secret := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)
	sealed, e := s.Vault.Seal([]byte(secret), "admin-totp")
	if e != nil {
		return nil, e
	}
	d["totp_pending"] = sealed
	if e = s.Store.Put(ctx, "admin", d); e != nil {
		return nil, e
	}
	return domain.Doc{"secret": secret, "otpauth_url": "otpauth://totp/NewNexus:" + url.PathEscape(domain.S(d, "username")) + "?secret=" + secret + "&issuer=NewNexus&algorithm=SHA1&digits=6&period=30"}, nil
}
func (s *Service) TOTPEnable(ctx context.Context, code string) ([]string, error) {
	d, e := s.Store.Get(ctx, "admin", "singleton")
	if e != nil {
		return nil, e
	}
	secret, e := s.Vault.Open(domain.S(d, "totp_pending"), "admin-totp")
	if e != nil {
		return nil, errors.New("start TOTP setup first")
	}
	step, ok := VerifyTOTP(string(secret), code, time.Now())
	if !ok {
		return nil, errors.New("TOTP code is invalid")
	}
	codes := []string{}
	hashes := domain.Doc{}
	for i := 0; i < 10; i++ {
		c := Token()[:16]
		codes = append(codes, c)
		hashes[Hash(c)] = true
	}
	d["totp_secret"] = d["totp_pending"]
	delete(d, "totp_pending")
	d["totp_enabled"] = true
	d["last_totp_step"] = step
	d["recovery_hashes"] = hashes
	e = s.Store.Put(ctx, "admin", d)
	return codes, e
}
func VerifyTOTP(secret, code string, now time.Time) (int64, bool) {
	key, e := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(secret))
	if e != nil || len(code) != 6 {
		return 0, false
	}
	for delta := int64(-1); delta <= 1; delta++ {
		step := now.Unix()/30 + delta
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(step))
		mac := hmac.New(sha1.New, key)
		mac.Write(b[:])
		sum := mac.Sum(nil)
		o := sum[len(sum)-1] & 15
		n := (uint32(sum[o])&127)<<24 | uint32(sum[o+1])<<16 | uint32(sum[o+2])<<8 | uint32(sum[o+3])
		want := fmt.Sprintf("%06d", n%1000000)
		if subtle.ConstantTimeCompare([]byte(want), []byte(code)) == 1 {
			return step, true
		}
	}
	return 0, false
}
