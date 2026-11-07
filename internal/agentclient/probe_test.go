package agentclient

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type resolverFunc func(context.Context, string) ([]net.IPAddr, error)

func (f resolverFunc) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	return f(ctx, host)
}

type dialerFunc func(context.Context, string, string) (net.Conn, error)

func (f dialerFunc) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return f(ctx, network, address)
}

func publicResolver() Resolver {
	return resolverFunc(func(_ context.Context, host string) ([]net.IPAddr, error) {
		if host == "private.example" {
			return []net.IPAddr{{IP: net.ParseIP("10.0.0.1")}}, nil
		}
		return []net.IPAddr{{IP: net.ParseIP("1.1.1.1")}}, nil
	})
}

func TestPublicIP(t *testing.T) {
	for _, value := range []string{"0.0.0.0", "10.1.2.3", "127.0.0.1", "169.254.169.254", "100.100.100.200", "172.31.255.255", "192.168.0.2", "192.0.2.1", "198.18.0.1", "198.51.100.1", "203.0.113.1", "224.0.0.1", "255.255.255.255", "::", "::1", "::ffff:127.0.0.1", "::ffff:10.0.0.1", "64:ff9b::a00:1", "2002:7f00:1::", "2001:db8::1", "fc00::1", "fe80::1", "ff02::1", "3fff::1"} {
		if PublicIP(net.ParseIP(value)) {
			t.Errorf("allowed reserved address %s", value)
		}
	}
	for _, value := range []string{"1.1.1.1", "8.8.8.8", "104.16.1.1", "2606:4700:4700::1111", "2001:4860:4860::8888", "::ffff:8.8.8.8"} {
		if !PublicIP(net.ParseIP(value)) {
			t.Errorf("blocked public address %s", value)
		}
	}
}

func TestResolveRejectsMixedAndSpecialDNS(t *testing.T) {
	mixed := resolverFunc(func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("8.8.8.8")}, {IP: net.ParseIP("127.0.0.1")}}, nil
	})
	for _, host := range []string{"mixed.example", "localhost", "foo.localhost", "metadata.google.internal", "service.local", "internal", "fe80::1%eth0", "127.0.0.1"} {
		if _, err := resolvePublic(context.Background(), mixed, host); err == nil {
			t.Errorf("accepted %s", host)
		}
	}
}

func TestHTTPConnectsToCandidateWithBusinessHostAndSNI(t *testing.T) {
	certificate, roots := testCertificate(t, "business.example")
	var mu sync.Mutex
	var gotHost, gotSNI, gotAddress string
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotHost = r.Host
		mu.Unlock()
		if r.URL.Path != "/health" {
			t.Errorf("wrong path %s", r.URL.Path)
		}
		if r.Header.Get("X-Probe") != "true" {
			t.Errorf("missing template header")
		}
		time.Sleep(5 * time.Millisecond)
		w.WriteHeader(201)
		w.(http.Flusher).Flush()
		time.Sleep(5 * time.Millisecond)
		fmt.Fprint(w, "healthy "+strings.Repeat("z", 2048))
	}))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{certificate}, GetConfigForClient: func(info *tls.ClientHelloInfo) (*tls.Config, error) {
		mu.Lock()
		gotSNI = info.ServerName
		mu.Unlock()
		return nil, nil
	}}
	server.StartTLS()
	defer server.Close()
	opts := ProbeOptions{Resolver: publicResolver(), TLSConfig: &tls.Config{RootCAs: roots}, RatePerSecond: 10000,
		Dialer: dialerFunc(func(ctx context.Context, network, address string) (net.Conn, error) {
			mu.Lock()
			gotAddress = address
			mu.Unlock()
			return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
		})}
	result := NewProber(opts).Probe(context.Background(), Task{ID: "task-1", Address: "8.8.8.8", Mode: "all", URL: "https://business.example/health", Host: "business.example", Headers: map[string]string{"X-Probe": "true"}, ExpectedStatuses: []int{201}, ExpectedContains: "healthy", MaxBytes: 64, Samples: 3})
	if !result.Success || result.SampleCount != 3 || result.SuccessRate != 1 {
		t.Fatalf("probe failed: %+v", result)
	}
	if result.BytesRead != 192 || result.StatusCode != 201 || result.TLSMS <= 0 || result.TCPMS <= 0 || result.TTFBMS <= 0 || result.DownloadBPS <= 0 || result.P50MS <= 0 || result.P95MS < result.P50MS {
		t.Fatalf("invalid measurements: %+v", result)
	}
	mu.Lock()
	defer mu.Unlock()
	if gotHost != "business.example" || gotSNI != "business.example" || gotAddress != "8.8.8.8:443" {
		t.Fatalf("routing identities lost: host=%s sni=%s dial=%s", gotHost, gotSNI, gotAddress)
	}
}

func TestHTTPSSRFAndPolicyNeverDials(t *testing.T) {
	called := 0
	prober := NewProber(ProbeOptions{Resolver: publicResolver(), RatePerSecond: 10000, Dialer: dialerFunc(func(context.Context, string, string) (net.Conn, error) {
		called++
		return nil, fmt.Errorf("unexpected dial")
	})})
	for _, task := range []Task{
		{ID: "a", Mode: "tcp", Address: "127.0.0.1"},
		{ID: "a", Mode: "tcp", Address: "::ffff:169.254.169.254"},
		{ID: "a", Mode: "tcp", Address: "private.example"},
		{ID: "a", Mode: "tcp", Address: "8.8.8.8", Port: 22},
		{ID: "a", Mode: "http", Address: "8.8.8.8", URL: "http://169.254.169.254/latest/meta-data"},
		{ID: "a", Mode: "http", Address: "8.8.8.8", URL: "http://private.example/"},
		{ID: "a", Mode: "http", Address: "8.8.8.8", URL: "file:///etc/passwd"},
		{ID: "a", Mode: "http", Address: "8.8.8.8", URL: "http://user:secret@business.example/"},
		{ID: "a", Mode: "http", Address: "8.8.8.8", URL: "http://business.example/", Headers: map[string]string{"X-Test": "good\r\nEvil: true"}},
		{ID: "a", Mode: "tcp", Address: "8.8.8.8", TimeoutMS: 600000},
		{ID: "a", Mode: "tcp", Address: "8.8.8.8", Samples: 1000},
	} {
		result := prober.Probe(context.Background(), task)
		if result.Success || result.Error == "" {
			t.Errorf("unsafe task passed: %+v", task)
		}
	}
	if called != 0 {
		t.Fatalf("unsafe tasks dialed %d times", called)
	}
}

func TestHTTPRedirectGuardsAndExpectedContent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/private":
			http.Redirect(w, r, "http://127.0.0.1/", 302)
		case "/loop":
			http.Redirect(w, r, "/loop", 302)
		case "/bad-status":
			w.WriteHeader(503)
		default:
			fmt.Fprint(w, "healthy")
		}
	}))
	defer server.Close()
	p := NewProber(ProbeOptions{Resolver: publicResolver(), RatePerSecond: 10000, Dialer: dialerFunc(func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	})})
	for _, path := range []string{"/private", "/loop", "/bad-status", "/missing-content"} {
		result := p.Probe(context.Background(), Task{ID: path, Mode: "http", Address: "8.8.8.8", URL: "http://business.example" + path, ExpectedContains: "not found", Samples: 1})
		if result.Success || result.Error == "" {
			t.Fatalf("unsafe or invalid response accepted: %+v", result)
		}
	}
}

func TestHTTPToHTTPSRedirectKeepsCandidateRoute(t *testing.T) {
	certificate, roots := testCertificate(t, "business.example")
	secure := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "business.example" {
			t.Errorf("business Host changed on redirect: %s", r.Host)
		}
		fmt.Fprint(w, "healthy")
	}))
	secure.TLS = &tls.Config{Certificates: []tls.Certificate{certificate}}
	secure.StartTLS()
	defer secure.Close()
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://business.example/health", 302)
	}))
	defer plain.Close()
	var mu sync.Mutex
	seen := map[string]bool{}
	p := NewProber(ProbeOptions{Resolver: publicResolver(), TLSConfig: &tls.Config{RootCAs: roots}, RatePerSecond: 10000, Dialer: dialerFunc(func(ctx context.Context, network, address string) (net.Conn, error) {
		mu.Lock()
		seen[address] = true
		mu.Unlock()
		destination := plain.Listener.Addr().String()
		if strings.HasSuffix(address, ":443") {
			destination = secure.Listener.Addr().String()
		}
		return (&net.Dialer{}).DialContext(ctx, network, destination)
	})})
	result := p.Probe(context.Background(), Task{ID: "redirect", Address: "8.8.8.8", Mode: "http", URL: "http://business.example/", Samples: 1, ExpectedContains: "healthy"})
	if !result.Success {
		t.Fatalf("HTTP -> HTTPS redirect failed: %+v", result)
	}
	mu.Lock()
	defer mu.Unlock()
	if !seen["8.8.8.8:80"] || !seen["8.8.8.8:443"] {
		t.Fatalf("redirect escaped pinned candidate or failed to change port: %+v", seen)
	}
}

func TestProbeTimeoutAndRateLimit(t *testing.T) {
	p := NewProber(ProbeOptions{RatePerSecond: 100, Dialer: dialerFunc(func(ctx context.Context, _, _ string) (net.Conn, error) { a, b := net.Pipe(); b.Close(); return a, nil })})
	start := time.Now()
	result := p.Probe(context.Background(), Task{ID: "rate", Address: "8.8.8.8", Mode: "tcp", Samples: 3})
	if !result.Success {
		t.Fatalf("tcp failed: %+v", result)
	}
	if time.Since(start) < 18*time.Millisecond {
		t.Fatalf("global rate limit not enforced")
	}
	p = NewProber(ProbeOptions{Resolver: publicResolver(), RatePerSecond: 10000, Dialer: dialerFunc(func(ctx context.Context, _, _ string) (net.Conn, error) { <-ctx.Done(); return nil, ctx.Err() })})
	start = time.Now()
	result = p.Probe(context.Background(), Task{ID: "timeout", Address: "8.8.8.8", Mode: "tcp", Samples: 1, TimeoutMS: 100})
	if result.Success || result.Error != "probe timeout" || time.Since(start) > time.Second {
		t.Fatalf("timeout was not bounded: %+v", result)
	}
}

func TestRetriesPreserveAttemptSuccessRate(t *testing.T) {
	var mu sync.Mutex
	attempts := 0
	p := NewProber(ProbeOptions{RatePerSecond: 10000, Dialer: dialerFunc(func(context.Context, string, string) (net.Conn, error) {
		mu.Lock()
		attempts++
		attempt := attempts
		mu.Unlock()
		if attempt == 1 {
			return nil, fmt.Errorf("transient failure")
		}
		a, b := net.Pipe()
		b.Close()
		return a, nil
	})})
	result := p.Probe(context.Background(), Task{ID: "retry", Mode: "tcp", Address: "8.8.8.8", Samples: 3, Retries: 2})
	if !result.Success || result.SampleCount != 4 || result.AttemptedSamples != 4 || result.RequestedSamples != 3 || result.SuccessRate != .75 || result.Error != "" {
		t.Fatalf("retry hid failures or invalidated recovered availability: %+v", result)
	}
	p = NewProber(ProbeOptions{RatePerSecond: 10000, Dialer: dialerFunc(func(context.Context, string, string) (net.Conn, error) { return nil, fmt.Errorf("always failed") })})
	result = p.Probe(context.Background(), Task{ID: "failed", Mode: "tcp", Address: "8.8.8.8", Samples: 2, Retries: 2})
	if result.Success || result.SampleCount != 6 || result.SuccessRate != 0 || result.Error == "" {
		t.Fatalf("retry exhaustion not recorded: %+v", result)
	}
	result = p.Probe(context.Background(), Task{ID: "invalid", Mode: "tcp", Address: "8.8.8.8", Retries: 4})
	if result.Success || result.Error == "" || result.SampleCount != 0 {
		t.Fatalf("retry policy not enforced: %+v", result)
	}
}

func TestCNAMEPublicAddressFallbackSharesTimeoutBudget(t *testing.T) {
	addresses := []net.IPAddr{{IP: net.ParseIP("2606:4700:4700::1111")}, {IP: net.ParseIP("8.8.8.8")}}
	p := NewProber(ProbeOptions{RatePerSecond: 10000, Resolver: resolverFunc(func(context.Context, string) ([]net.IPAddr, error) { return addresses, nil }), Dialer: dialerFunc(func(ctx context.Context, _, address string) (net.Conn, error) {
		if strings.HasPrefix(address, "[") {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		a, b := net.Pipe()
		b.Close()
		return a, nil
	})})
	start := time.Now()
	result := p.Probe(context.Background(), Task{ID: "fallback", Address: "candidate.example", Mode: "tcp", Samples: 1, TimeoutMS: 400})
	if !result.Success || time.Since(start) > time.Second {
		t.Fatalf("unreachable public IPv6 starved available IPv4: %+v", result)
	}
}

func testCertificate(t *testing.T, host string) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: host}, DNSNames: []string{host}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := tls.X509KeyPair(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(parsed)
	return certificate, roots
}
