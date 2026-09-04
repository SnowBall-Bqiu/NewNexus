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

var ErrInvalidAPIKeyInput = errors.New("invalid API key input")

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
	admin, _, err := m.SetupWithRecovery(username, password)
	return admin, err
}

func (m *Manager) SetupWithRecovery(username, password string) (*domain.Admin, []string, error) {
	required, err := m.SetupRequired()
	if err != nil {
		return nil, nil, err
	}
	if !required {
		return nil, nil, errors.New("administrator is already configured")
	}
	username = strings.TrimSpace(username)
	if len(username) < 3 {
		return nil, nil, errors.New("username must contain at least 3 characters")
	}
	if len(password) < 12 {
		return nil, nil, errors.New("password must contain at least 12 characters")
	}
	hash, err := security.HashPassword(password)
	if err != nil {
		return nil, nil, err
	}
	codes, hashes, err := newRecoveryCodes()
	if err != nil {
		return nil, nil, err
	}
	admin := &domain.Admin{Username: username, PasswordHash: hash, RecoveryHashes: strings.Join(hashes, "\n")}
	if err := m.store.DB.Create(admin).Error; err != nil {
		return nil, nil, err
	}
	return admin, codes, nil
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
			if err := m.UseRecoveryCode(&admin, code); err != nil {
				return nil, errors.New("invalid verification code")
			}
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
	if err := m.store.DB.Model(&domain.Admin{}).Where("id = ?", admin.ID).Update("pending_totp_secret_enc", encrypted).Error; err != nil {
		return "", "", err
	}
	return key.Secret(), key.URL(), nil
}

func (m *Manager) ConfirmTOTP(admin domain.Admin, code string) error {
	secretEnc := admin.PendingTOTPSecretEnc
	if secretEnc == "" && !admin.TOTPEnabled {
		// Compatibility for a binding started before pending secrets existed.
		secretEnc = admin.TOTPSecretEnc
	}
	if secretEnc == "" {
		return errors.New("no pending TOTP binding")
	}
	secret, err := m.vault.Decrypt(secretEnc)
	if err != nil {
		return err
	}
	if !totp.Validate(strings.TrimSpace(code), secret) {
		return errors.New("invalid verification code")
	}
	return m.store.DB.Model(&domain.Admin{}).Where("id = ?", admin.ID).Updates(map[string]any{"totp_secret_enc": secretEnc, "pending_totp_secret_enc": "", "totp_enabled": true}).Error
}

func (m *Manager) DisableTOTP(admin domain.Admin, code string) error {
	secret, err := m.vault.Decrypt(admin.TOTPSecretEnc)
	if err != nil || !totp.Validate(strings.TrimSpace(code), secret) {
		return errors.New("invalid verification code")
	}
	admin.TOTPEnabled = false
	return m.store.DB.Model(&domain.Admin{}).Where("id = ?", admin.ID).Updates(map[string]any{"totp_enabled": false, "totp_secret_enc": "", "pending_totp_secret_enc": ""}).Error
}

func (m *Manager) CreateSession(w http.ResponseWriter, adminID string) (string, error) {
	return m.CreateSessionForRequest(w, nil, adminID)
}

func (m *Manager) CreateSessionForRequest(w http.ResponseWriter, r *http.Request, adminID string) (string, error) {
	token, err := security.RandomToken(32)
	if err != nil {
		return "", err
	}
	csrf, err := security.RandomToken(24)
	if err != nil {
		return "", err
	}
	session := domain.Session{AdminID: adminID, TokenHash: security.HashToken(token), CSRFHash: security.HashToken(csrf), ExpiresAt: time.Now().UTC().Add(30 * 24 * time.Hour), LastSeen: time.Now().UTC()}
	if r != nil {
		session.IPAddress = r.RemoteAddr
		session.UserAgent = r.UserAgent()
	}
	if err := m.store.DB.Create(&session).Error; err != nil {
		return "", err
	}
	http.SetCookie(w, &http.Cookie{Name: "newnexus_session", Value: token, Path: "/", HttpOnly: true, Secure: m.cfg.SecureCookies, SameSite: http.SameSiteLaxMode, Expires: session.ExpiresAt})
	http.SetCookie(w, &http.Cookie{Name: "newnexus_csrf", Value: csrf, Path: "/", HttpOnly: false, Secure: m.cfg.SecureCookies, SameSite: http.SameSiteLaxMode, Expires: session.ExpiresAt})
	return csrf, nil
}

func (m *Manager) Logout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie("newnexus_session"); err == nil {
		_ = m.store.DB.Model(&domain.Session{}).Where("token_hash = ?", security.HashToken(cookie.Value)).Updates(map[string]any{"revoked_at": time.Now().UTC()}).Error
	}
	http.SetCookie(w, &http.Cookie{Name: "newnexus_session", Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: m.cfg.SecureCookies, SameSite: http.SameSiteLaxMode})
	http.SetCookie(w, &http.Cookie{Name: "newnexus_csrf", Value: "", Path: "/", MaxAge: -1, Secure: m.cfg.SecureCookies, SameSite: http.SameSiteLaxMode})
}

func (m *Manager) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if m.cfg.DevAuth {
			ctx := context.WithValue(r.Context(), adminContextKey, domain.Admin{Username: "dev-admin", TOTPEnabled: false})
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}
		admin, session, ok := m.authenticate(r)
		if !ok {
			writeAuthError(w, http.StatusUnauthorized, "authentication_required", "请先登录")
			return
		}
		if session == nil && !m.apiKeyAllows(r) {
			writeAuthError(w, http.StatusForbidden, "api_key_scope_denied", "API Key 作用域不允许该操作")
			return
		}
		if session != nil && r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
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

func (m *Manager) apiKeyAllows(r *http.Request) bool {
	value := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !strings.HasPrefix(value, "nn_") {
		return false
	}
	prefix := value
	if len(prefix) > 12 {
		prefix = prefix[:12]
	}
	now := time.Now().UTC()
	var key domain.APIKey
	if m.store.DB.Where("prefix = ? AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > ?)", prefix, now).First(&key).Error != nil || key.SecretHash != security.HashToken(value) {
		return false
	}
	action := "write"
	if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
		action = "read"
	}
	resource := strings.TrimPrefix(r.URL.Path, "/api/v1/")
	if index := strings.IndexByte(resource, '/'); index >= 0 {
		resource = resource[:index]
	}
	for _, raw := range strings.Split(key.Scopes, ",") {
		scope := strings.TrimSpace(strings.ToLower(raw))
		if scope == "*" || scope == "admin" || scope == action || scope == resource+":"+action {
			return true
		}
	}
	return false
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
		now := time.Now().UTC()
		if m.store.DB.Where("prefix = ? AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > ?)", prefix, now).First(&key).Error == nil && key.SecretHash == security.HashToken(value) {
			_ = m.store.DB.Model(&key).Update("last_used_at", now).Error
			var admin domain.Admin
			if m.store.DB.First(&admin, "id = ?", key.AdminID).Error != nil {
				return nil, nil, false
			}
			return &admin, nil, true
		}
	}
	if token == "" {
		return nil, nil, false
	}
	var session domain.Session
	if err := m.store.DB.Where("token_hash = ? AND revoked_at IS NULL AND expires_at > ?", security.HashToken(token), time.Now().UTC()).First(&session).Error; err != nil {
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
		return map[string]any{"setupRequired": false, "authenticated": true, "username": "dev-admin", "totpEnabled": false, "securityWarning": "开发模式已启用，生产环境请配置主密钥并启用 TOTP", "devMode": true}
	}
	admin, _, ok := m.authenticate(r)
	result := map[string]any{"setupRequired": required, "authenticated": ok, "devMode": false}
	if ok {
		result["username"] = admin.Username
		result["totpEnabled"] = admin.TOTPEnabled
		if !admin.TOTPEnabled {
			result["securityWarning"] = "尚未启用 TOTP，多因素认证可以降低管理员账户风险"
		}
	}
	return result
}

func (m *Manager) String() string { return fmt.Sprintf("auth(dev=%t)", m.cfg.DevAuth) }

func newRecoveryCodes() ([]string, []string, error) {
	codes := make([]string, 0, 10)
	hashes := make([]string, 0, 10)
	for i := 0; i < 10; i++ {
		code, err := security.RandomToken(9)
		if err != nil {
			return nil, nil, err
		}
		code = strings.ToUpper(strings.TrimRight(code, "="))
		codes = append(codes, code)
		hashes = append(hashes, security.HashToken(code))
	}
	return codes, hashes, nil
}

func (m *Manager) RegenerateRecoveryCodes(adminID string) ([]string, error) {
	codes, hashes, err := newRecoveryCodes()
	if err != nil {
		return nil, err
	}
	if err := m.store.DB.Model(&domain.Admin{}).Where("id = ?", adminID).Update("recovery_hashes", strings.Join(hashes, "\n")).Error; err != nil {
		return nil, err
	}
	return codes, nil
}

func (m *Manager) UseRecoveryCode(admin *domain.Admin, raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" || admin == nil {
		return errors.New("recovery code is required")
	}
	hash := security.HashToken(raw)
	parts := strings.Split(strings.TrimSpace(admin.RecoveryHashes), "\n")
	remaining := make([]string, 0, len(parts))
	found := false
	for _, part := range parts {
		if part == "" {
			continue
		}
		if part == hash && !found {
			found = true
			continue
		}
		remaining = append(remaining, part)
	}
	if !found {
		return errors.New("invalid recovery code")
	}
	admin.RecoveryHashes = strings.Join(remaining, "\n")
	return m.store.DB.Model(&domain.Admin{}).Where("id = ?", admin.ID).Update("recovery_hashes", admin.RecoveryHashes).Error
}

func (m *Manager) RevokeAllSessions(adminID string) error {
	return m.store.DB.Model(&domain.Session{}).Where("admin_id = ? AND revoked_at IS NULL", adminID).Update("revoked_at", time.Now().UTC()).Error
}

func (m *Manager) CreateAPIKey(adminID, name, scopes string, expiresAt *time.Time) (domain.APIKey, string, error) {
	name = strings.TrimSpace(name)
	if len(name) > 120 {
		return domain.APIKey{}, "", fmt.Errorf("%w: name exceeds 120 characters", ErrInvalidAPIKeyInput)
	}
	if expiresAt != nil {
		utcExpiry := expiresAt.UTC()
		if !utcExpiry.After(time.Now().UTC()) {
			return domain.APIKey{}, "", fmt.Errorf("%w: expiry must be in the future", ErrInvalidAPIKeyInput)
		}
		expiresAt = &utcExpiry
	}
	scopes, err := normalizeScopes(scopes)
	if err != nil {
		return domain.APIKey{}, "", err
	}
	secret, err := security.RandomToken(32)
	if err != nil {
		return domain.APIKey{}, "", err
	}
	value := "nn_" + secret
	prefix := value
	if len(prefix) > 12 {
		prefix = prefix[:12]
	}
	key := domain.APIKey{AdminID: adminID, Name: name, Prefix: prefix, SecretHash: security.HashToken(value), Scopes: strings.TrimSpace(scopes), ExpiresAt: expiresAt}
	if key.Name == "" {
		key.Name = "API Key"
	}
	if err := m.store.DB.Create(&key).Error; err != nil {
		return domain.APIKey{}, "", err
	}
	return key, value, nil
}

func normalizeScopes(raw string) (string, error) {
	if len(raw) > 512 {
		return "", fmt.Errorf("%w: scopes exceed 512 characters", ErrInvalidAPIKeyInput)
	}
	if strings.TrimSpace(raw) == "" {
		return "read", nil
	}
	seen := make(map[string]bool)
	result := make([]string, 0)
	for _, item := range strings.Split(raw, ",") {
		if len(result) >= 32 {
			return "", fmt.Errorf("%w: too many scopes", ErrInvalidAPIKeyInput)
		}
		scope := strings.TrimSpace(strings.ToLower(item))
		valid := scope == "read" || scope == "write" || scope == "admin" || scope == "*" || strings.HasSuffix(scope, ":read") || strings.HasSuffix(scope, ":write")
		if !valid || strings.ContainsAny(scope, " /\\") {
			return "", fmt.Errorf("%w: invalid scope %q", ErrInvalidAPIKeyInput, scope)
		}
		if !seen[scope] {
			seen[scope] = true
			result = append(result, scope)
		}
	}
	return strings.Join(result, ","), nil
}
