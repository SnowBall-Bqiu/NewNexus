package security

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type Vault struct{ aead cipher.AEAD }

func NewVault(key []byte) (*Vault, error) {
	if len(key) != 32 {
		return nil, errors.New("master key must contain exactly 32 bytes")
	}
	b, e := aes.NewCipher(key)
	if e != nil {
		return nil, e
	}
	a, e := cipher.NewGCM(b)
	return &Vault{a}, e
}
func (v *Vault) Seal(plain []byte, scope string) (string, error) {
	nonce := make([]byte, v.aead.NonceSize())
	if _, e := rand.Read(nonce); e != nil {
		return "", e
	}
	return base64.RawURLEncoding.EncodeToString(v.aead.Seal(nonce, nonce, plain, []byte(scope))), nil
}
func (v *Vault) Open(encoded, scope string) ([]byte, error) {
	b, e := base64.RawURLEncoding.DecodeString(encoded)
	if e != nil || len(b) < v.aead.NonceSize() {
		return nil, errors.New("invalid encrypted credential")
	}
	return v.aead.Open(nil, b[:v.aead.NonceSize()], b[v.aead.NonceSize():], []byte(scope))
}

var reserved = []netip.Prefix{netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"), netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"), netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("240.0.0.0/4"), netip.MustParsePrefix("2001:db8::/32"), netip.MustParsePrefix("64:ff9b::/96"), netip.MustParsePrefix("64:ff9b:1::/48"), netip.MustParsePrefix("2001::/23")}

func PublicAddress(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsMulticast() {
		return false
	}
	for _, p := range reserved {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}
func ValidateURL(raw string) (*url.URL, error) {
	u, e := url.Parse(raw)
	if e != nil || u.Hostname() == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil {
		return nil, errors.New("only absolute HTTP(S) URLs without credentials are allowed")
	}
	p := u.Port()
	if p != "" {
		n, e := strconv.Atoi(p)
		if e != nil || n < 1 || n > 65535 {
			return nil, errors.New("URL port must be between 1 and 65535")
		}
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "localhost" || host == "localhost.localdomain" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") || strings.HasSuffix(host, ".home.arpa") {
		return nil, errors.New("local hostnames are prohibited")
	}
	if ip, e := netip.ParseAddr(host); e == nil && !PublicAddress(ip) {
		return nil, errors.New("private, reserved, metadata and local addresses are prohibited")
	}
	return u, nil
}
func ResolvePublic(ctx context.Context, host string) ([]netip.Addr, error) {
	ips, e := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if e != nil {
		return nil, e
	}
	if len(ips) == 0 {
		return nil, errors.New("hostname has no addresses")
	}
	for _, ip := range ips {
		if !PublicAddress(ip) {
			return nil, errors.New("private, reserved, metadata and local addresses are prohibited")
		}
	}
	return ips, nil
}
func SafeClient(timeout time.Duration) *http.Client {
	d := &net.Dialer{Timeout: timeout}
	transport := &http.Transport{Proxy: nil, ResponseHeaderTimeout: timeout, DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		h, p, e := net.SplitHostPort(addr)
		if e != nil {
			return nil, e
		}
		if p != "80" && p != "443" && p != "8080" && p != "8443" {
			return nil, errors.New("port is not allowed")
		}
		ips, e := ResolvePublic(ctx, h)
		if e != nil {
			return nil, e
		}
		return d.DialContext(ctx, network, net.JoinHostPort(ips[0].String(), p))
	}}
	return &http.Client{Transport: transport, Timeout: timeout, CheckRedirect: func(r *http.Request, via []*http.Request) error {
		if len(via) >= 3 {
			return errors.New("too many redirects")
		}
		_, e := ValidateURL(r.URL.String())
		return e
	}}
}
func Redact(s string) string {
	for _, word := range []string{"Bearer ", "api_token", "secret_key", "access_key_secret", "credential"} {
		if strings.Contains(strings.ToLower(s), strings.ToLower(word)) {
			return "external operation failed; inspect provider permissions and connectivity"
		}
	}
	if len(s) > 1000 {
		return fmt.Sprintf("%s…", s[:1000])
	}
	return s
}
