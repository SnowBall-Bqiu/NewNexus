package security

import (
	"bytes"
	"io"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestVaultBindsCredentialsToConnection(t *testing.T) {
	v, e := NewVault(bytes.Repeat([]byte{7}, 32))
	if e != nil {
		t.Fatal(e)
	}
	sealed, e := v.Seal([]byte("secret"), "connection:a")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = v.Open(sealed, "connection:b"); e == nil {
		t.Fatal("ciphertext moved between resources")
	}
	plain, e := v.Open(sealed, "connection:a")
	if e != nil || string(plain) != "secret" {
		t.Fatal(string(plain), e)
	}
}
func TestReservedAndMetadataAddressesBlocked(t *testing.T) {
	for _, address := range []string{"127.0.0.1", "10.0.0.1", "172.16.0.1", "192.168.0.1", "169.254.169.254", "100.100.100.200", "0.0.0.0", "192.0.2.1", "198.18.0.1", "224.0.0.1", "::1", "::ffff:127.0.0.1", "fc00::1", "fe80::1", "2001:db8::1", "64:ff9b::7f00:1"} {
		if PublicAddress(netip.MustParseAddr(address)) {
			t.Errorf("allowed %s", address)
		}
	}
	for _, address := range []string{"1.1.1.1", "8.8.8.8", "2606:4700:4700::1111"} {
		if !PublicAddress(netip.MustParseAddr(address)) {
			t.Errorf("blocked public %s", address)
		}
	}
}
func TestProbeURLPolicy(t *testing.T) {
	for _, value := range []string{"file:///etc/passwd", "http://user:pass@example.com", "https://example.com:65536", "https://localhost/", "http://169.254.169.254/", "//example.com"} {
		if _, e := ValidateURL(value); e == nil {
			t.Errorf("allowed %s", value)
		}
	}
}

type redirectTransport func(*http.Request) (*http.Response, error)

func (f redirectTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func redirectResponse(r *http.Request, status int, location string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Location": {location}}, Body: io.NopCloser(strings.NewReader("")), Request: r}
}

func TestSafeClientRejectsHTTPSDowngradeRedirects(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		for _, status := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
			t.Run(method+"/"+strconv.Itoa(status), func(t *testing.T) {
				const payload = `{"message":"private alert"}`
				calls := 0
				client := SafeClient(time.Second)
				client.Transport = redirectTransport(func(r *http.Request) (*http.Response, error) {
					calls++
					if calls > 1 {
						t.Fatal("sent a request to the plaintext redirect destination")
					}
					if r.Method != method || r.URL.Scheme != "https" {
						t.Fatal("unexpected initial request", r.Method, r.URL)
					}
					if method == http.MethodPost {
						body, err := io.ReadAll(r.Body)
						if err != nil || string(body) != payload || r.Header.Get("X-NewNexus-Signature") != "sha256=test-signature" {
							t.Fatal("initial webhook request lost its payload or signature", string(body), err)
						}
					}
					return redirectResponse(r, status, "http://public.example.com/final"), nil
				})
				var body io.Reader
				if method == http.MethodPost {
					body = bytes.NewReader([]byte(payload))
				}
				req, err := http.NewRequest(method, "https://public.example.com/start", body)
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("X-NewNexus-Signature", "sha256=test-signature")
				resp, err := client.Do(req)
				if resp != nil {
					resp.Body.Close()
				}
				if err == nil || !strings.Contains(err.Error(), "must not downgrade") || calls != 1 {
					t.Fatal("HTTPS downgrade was not rejected before sending", calls, err)
				}
			})
		}
	}
}

func TestSafeClientAllowsHTTPSWebhookRedirects(t *testing.T) {
	for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			const payload = `{"message":"private alert"}`
			calls := 0
			client := SafeClient(time.Second)
			client.Transport = redirectTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				body, err := io.ReadAll(r.Body)
				if err != nil || r.Method != http.MethodPost || string(body) != payload || r.Header.Get("X-NewNexus-Signature") != "sha256=test-signature" {
					t.Fatal("HTTPS redirect lost the webhook method, body or signature", r.Method, string(body), err)
				}
				if calls == 1 {
					return redirectResponse(r, status, "https://public.example.com/final"), nil
				}
				if calls != 2 || r.URL.String() != "https://public.example.com/final" {
					t.Fatal("unexpected redirected request", r.URL)
				}
				return redirectResponse(r, http.StatusOK, ""), nil
			})
			req, err := http.NewRequest(http.MethodPost, "https://public.example.com/start", bytes.NewReader([]byte(payload)))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("X-NewNexus-Signature", "sha256=test-signature")
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if calls != 2 || resp.StatusCode != http.StatusOK {
				t.Fatal("HTTPS redirect did not complete", calls, resp.StatusCode)
			}
		})
	}
}

func TestSafeClientAllowsHTTPValidationAndRejectsLaterDowngrade(t *testing.T) {
	for _, scenario := range []struct {
		name      string
		locations []string
		rejected  bool
	}{
		{"http validation", nil, false},
		{"http validation redirect", []string{"http://public.example.com/final"}, false},
		{"http validation upgrade", []string{"https://public.example.com/final"}, false},
		{"downgrade after upgrade", []string{"https://public.example.com/secure", "http://public.example.com/final"}, true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			calls := 0
			client := SafeClient(time.Second)
			client.Transport = redirectTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if scenario.rejected && calls > len(scenario.locations) {
					t.Fatal("sent a plaintext request after upgrading to HTTPS")
				}
				if calls <= len(scenario.locations) {
					return redirectResponse(r, http.StatusFound, scenario.locations[calls-1]), nil
				}
				return redirectResponse(r, http.StatusOK, ""), nil
			})
			resp, err := client.Get("http://public.example.com/validation")
			if resp != nil {
				resp.Body.Close()
			}
			if scenario.rejected {
				if err == nil || !strings.Contains(err.Error(), "must not downgrade") || calls != len(scenario.locations) {
					t.Fatal("later downgrade was not rejected", calls, err)
				}
			} else if err != nil || calls != len(scenario.locations)+1 || resp.StatusCode != http.StatusOK {
				t.Fatal("HTTP validation or secure upgrade failed", calls, err)
			}
		})
	}
}
