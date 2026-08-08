package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/SnowBall-Bqiu/NewNexus/internal/config"
	"github.com/SnowBall-Bqiu/NewNexus/internal/domain"
	"github.com/SnowBall-Bqiu/NewNexus/internal/security"
	"github.com/SnowBall-Bqiu/NewNexus/internal/store"
	"github.com/pquerna/otp/totp"
)

type contextKey string

const adminContextKey contextKey = "admin"

type Manager struct {
	store *store.Store
	vault *security.Vault
	cfg   config.Config
}

func NewManager(store *store.Store, vault *security.Vault, cfg config.Config) *Manager {
	return &Manager{store: store, vault: vault, cfg: cfg}
}

func (m *Manager) SetupRequired() (bool, error) {
	var count int64
	if err := m.store.DB.Model(&domain.Admin{}).Count(&count).Error; err != nil {
		return false, err
	}
	return count == 0, nil
}

func (m *Manager) Setup(username, password string) (*domain.Admin, error) {
	required, err := m.SetupRequired()
	if err != nil {
		return nil, err
	}
	if !required {
		return nil, errors.New("administrator is already configured")
	}
	username = strings.TrimSpace(username)
	if len(username) < 3 {
		return nil, errors.New("username must contain at least 3 characters")
	}
	if len(password) < 12 {
		return nil, errors.New("password must contain at least 12 characters")
	}
	hash, err := security.HashPassword(password)
	if err != nil {
		return nil, err
	}
	admin := &domain.Admin{Username: username, PasswordHash: hash}
	if err := m.store.DB.Create(admin).Error; err != nil {
		return nil, err
	}
	return admin, nil
}

func (m *Manager) Login(username, password, code string) (*domain.Admin, error) {
	var admin domain.Admin
	if err := m.store.DB.Where("username = ?", strings.TrimSpace(username)).First(&admin).Error; err != nil {
		return nil, errors.New("invalid username or password")
	}
	if !security.VerifyPassword(admin.PasswordHash, password) {
		return nil, errors.New("invalid username or password")
	}
	if admin.TOTPEnabled {
		secret, err := m.vault.Decrypt(admin.TOTPSecretEnc)
		if err != nil {
			return nil, errors.New("unable to verify TOTP")
		}
		if !totp.Validate(strings.TrimSpace(code), secret) {
			return nil, errors.New("invalid verification code")
		}
	}
	return &admin, nil
}

func (m *Manager) StartTOTP(admin domain.Admin) (string, string, error) {
	key, err := totp.Generate(totp.GenerateOpts{Issuer: "NewNexus", AccountName: admin.Username})
	if err != nil {
		return "", "", err
	}
	encrypted, err := m.vault.Encrypt(key.Secret())
	if err != nil {
		return "", "", err
	}
	admin.TOTPSecretEnc = encrypted
	admin.TOTPEnabled = false
	if err := m.store.DB.Save(&admin).Error; err != nil {
		return "", "", err
	}
	return key.Secret(), key.URL(), nil
}

func (m *Manager) ConfirmTOTP(admin domain.Admin, code string) error {
	secret, err := m.vault.Decrypt(admin.TOTPSecretEnc)
	if err != nil {
		return err
	}
	if !totp.Validate(strings.TrimSpace(code), secret) {
		return errors.New("invalid verification code")
	}
	admin.TOTPEnabled = true
	return m.store.DB.Save(&admin).Error
}

func (m *Manager) CreateSession(w http.ResponseWriter, adminID string) (string, error) {
	token, err := security.RandomToken(32)
	if err != nil {
		return "", err
	}
	csrf, err := security.RandomToken(24)
	if err != nil {
		return "", err
	}
	session := domain.Session{AdminID: adminID, TokenHash: security.HashToken(token), CSRFHash: security.HashToken(csrf), ExpiresAt: time.Now().UTC().Add(30 * 24 * time.Hour), LastSeen: time.Now().UTC()}
	if err := m.store.DB.Create(&session).Error; err != nil {
		return "", err
	}
	http.SetCookie(w, &http.Cookie{Name: "newnexus_session", Value: token, Path: "/", HttpOnly: true, Secure: m.cfg.SecureCookies, SameSite: http.SameSiteLaxMode, Expires: session.ExpiresAt})
	http.SetCookie(w, &http.Cookie{Name: "newnexus_csrf", Value: csrf, Path: "/", HttpOnly: false, Secure: m.cfg.SecureCookies, SameSite: http.SameSiteLaxMode, Expires: session.ExpiresAt})
	return csrf, nil
}

func (m *Manager) Logout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie("newnexus_session"); err == nil {
		_ = m.store.DB.Where("token_hash = ?", security.HashToken(cookie.Value)).Delete(&domain.Session{}).Error
	}
	http.SetCookie(w, &http.Cookie{Name: "newnexus_session", Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: m.cfg.SecureCookies, SameSite: http.SameSiteLaxMode})
	http.SetCookie(w, &http.Cookie{Name: "newnexus_csrf", Value: "", Path: "/", MaxAge: -1, Secure: m.cfg.SecureCookies, SameSite: http.SameSiteLaxMode})
}

func (m *Manager) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if m.cfg.DevAuth {
			ctx := context.WithValue(r.Context(), adminContextKey, domain.Admin{Username: "dev-admin", TOTPEnabled: true})
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}
		admin, session, ok := m.authenticate(r)
		if !ok {
			writeAuthError(w, http.StatusUnauthorized, "authentication_required", "请先登录")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
			csrfCookie, err := r.Cookie("newnexus_csrf")
			csrfHeader := r.Header.Get("X-CSRF-Token")
			if err != nil || csrfHeader == "" || csrfCookie.Value != csrfHeader || security.HashToken(csrfHeader) != session.CSRFHash {
				writeAuthError(w, http.StatusForbidden, "csrf_failed", "安全令牌无效，请刷新页面")
				return
			}
		}
		session.LastSeen = time.Now().UTC()
		_ = m.store.DB.Model(&session).Update("last_seen", session.LastSeen).Error
		ctx := context.WithValue(r.Context(), adminContextKey, *admin)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (m *Manager) authenticate(r *http.Request) (*domain.Admin, *domain.Session, bool) {
	var token string
	if cookie, err := r.Cookie("newnexus_session"); err == nil {
		token = cookie.Value
	}
	if token == "" && strings.HasPrefix(r.Header.Get("Authorization"), "Bearer nn_") {
		value := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		prefix := value
		if len(prefix) > 12 {
			prefix = prefix[:12]
		}
		var key domain.APIKey
		if m.store.DB.Where("prefix = ?", prefix).First(&key).Error == nil && key.SecretHash == security.HashToken(value) {
			now := time.Now().UTC()
			_ = m.store.DB.Model(&key).Update("last_used_at", now).Error
			return &domain.Admin{Username: "api-key", TOTPEnabled: true}, &domain.Session{CSRFHash: security.HashToken("")}, true
		}
	}
	if token == "" {
		return nil, nil, false
	}
	var session domain.Session
	if err := m.store.DB.Where("token_hash = ? AND expires_at > ?", security.HashToken(token), time.Now().UTC()).First(&session).Error; err != nil {
		return nil, nil, false
	}
	var admin domain.Admin
	if err := m.store.DB.First(&admin, "id = ?", session.AdminID).Error; err != nil {
		return nil, nil, false
	}
	return &admin, &session, true
}

func AdminFromContext(ctx context.Context) (domain.Admin, bool) {
	admin, ok := ctx.Value(adminContextKey).(domain.Admin)
	return admin, ok
}

func writeAuthError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": code, "message": message}})
}

func (m *Manager) Status(r *http.Request) map[string]any {
	required, _ := m.SetupRequired()
	if m.cfg.DevAuth {
		return map[string]any{"setupRequired": false, "authenticated": true, "username": "dev-admin", "totpEnabled": true, "devMode": true}
	}
	admin, _, ok := m.authenticate(r)
	result := map[string]any{"setupRequired": required, "authenticated": ok, "devMode": false}
	if ok {
		result["username"] = admin.Username
		result["totpEnabled"] = admin.TOTPEnabled
	}
	return result
}

func (m *Manager) String() string { return fmt.Sprintf("auth(dev=%t)", m.cfg.DevAuth) }
