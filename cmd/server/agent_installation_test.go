package main

import "testing"

func TestAgentDownloadOriginConfiguration(t *testing.T) {
	for _, test := range []struct {
		name, configured, expected string
		invalid                    bool
	}{
		{"unset remains unset", "", "", false},
		{"HTTPS tunnel origin", "https://dev-test-newnexus.bqiu-cn.top/", "https://dev-test-newnexus.bqiu-cn.top", false},
		{"normalizes whitespace host and default port", " HTTPS://DOWNLOADS.EXAMPLE.COM:443/ ", "https://downloads.example.com", false},
		{"development HTTP origin", "http://127.0.0.1:5173/", "http://127.0.0.1:5173", false},
		{"IPv6 origin", "http://[::1]:8080/", "http://[::1]:8080", false},
		{"IPv6 default port", "https://[::1]:443", "https://[::1]", false},
		{"HTTP default port", "http://example.com:80", "http://example.com", false},
		{"unsupported protocol", "ftp://downloads.example.com", "", true},
		{"executable protocol", "javascript:alert(1)", "", true},
		{"data protocol", "data:text/plain,example", "", true},
		{"scheme required", "//downloads.example.com", "", true},
		{"hostname required", "https:///", "", true},
		{"username rejected", "https://admin@downloads.example.com", "", true},
		{"password rejected", "https://admin:secret@downloads.example.com", "", true},
		{"empty credentials rejected", "https://@downloads.example.com", "", true},
		{"path rejected", "https://downloads.example.com/console", "", true},
		{"double slash path rejected", "https://downloads.example.com//", "", true},
		{"encoded path rejected", "https://downloads.example.com/%2F", "", true},
		{"query rejected", "https://downloads.example.com/?token=secret", "", true},
		{"empty query rejected", "https://downloads.example.com/?", "", true},
		{"fragment rejected", "https://downloads.example.com/#install", "", true},
		{"empty fragment rejected", "https://downloads.example.com/#", "", true},
		{"zero port rejected", "https://downloads.example.com:0", "", true},
		{"out of range port rejected", "https://downloads.example.com:65536", "", true},
		{"empty port rejected", "https://downloads.example.com:", "", true},
		{"invalid IPv6 rejected", "https://[not-an-ip]", "", true},
		{"unbracketed IPv6 rejected", "https://::1:443", "", true},
		{"backslash rejected", "https://downloads.example.com\\attacker", "", true},
		{"control character rejected", "https://downloads.example.com\r\nX-Evil: true", "", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := normalizeAgentDownloadOrigin(test.configured)
			if (err != nil) != test.invalid || got != test.expected {
				t.Fatalf("origin %q, error %v; want %q invalid=%v", got, err, test.expected, test.invalid)
			}
		})
	}
}

func TestAdvertisedAgentGateway(t *testing.T) {
	for _, test := range []struct {
		name, configured, listening, expected string
		invalid                               bool
	}{
		{"actual development port", "", "127.0.0.1:9091", "127.0.0.1:9091", false},
		{"configured public host", " grpc.example.com:443 ", "127.0.0.1:9091", "grpc.example.com:443", false},
		{"IPv6 loopback", "", "[::1]:9091", "[::1]:9091", false},
		{"IPv4 wildcard is unknown", "", "0.0.0.0:9090", "", false},
		{"IPv6 wildcard is unknown", "", "[::]:9090", "", false},
		{"empty host is unknown", "", ":9090", "", false},
		{"explicit wildcard rejected", "0.0.0.0:9090", "127.0.0.1:9090", "", true},
		{"URL is not a gRPC address", "https://grpc.example.com:443", "127.0.0.1:9090", "", true},
		{"missing port rejected", "grpc.example.com", "127.0.0.1:9090", "", true},
		{"out of range port rejected", "grpc.example.com:65536", "127.0.0.1:9090", "", true},
		{"zero port rejected", "grpc.example.com:0", "127.0.0.1:9090", "", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := agentPublicAddress(test.configured, test.listening)
			if (err != nil) != test.invalid || got != test.expected {
				t.Fatalf("address %q, error %v; want %q invalid=%v", got, err, test.expected, test.invalid)
			}
		})
	}
}

func TestAdvertisedPlaintextRequiresExactLiteralLoopbackEndpoint(t *testing.T) {
	for _, test := range []struct {
		name, environment, cert, advertised, listening string
		allowed, expected                              bool
	}{
		{"development endpoint", "development", "", "127.0.0.1:9091", "127.0.0.1:9091", true, true},
		{"IPv6 endpoint", "development", "", "[::1]:9091", "[0:0:0:0:0:0:0:1]:9091", true, true},
		{"IPv4 mapped address", "development", "", "[::ffff:127.0.0.1]:9091", "127.0.0.1:9091", true, true},
		{"another local proxy port", "development", "", "127.0.0.1:9443", "127.0.0.1:9091", true, false},
		{"another loopback IP", "development", "", "127.0.0.2:9091", "127.0.0.1:9091", true, false},
		{"localhost resolution is ambiguous", "development", "", "localhost:9091", "127.0.0.1:9091", true, false},
		{"DNS resolution is unknown", "development", "", "grpc.example.com:9091", "127.0.0.1:9091", true, false},
		{"public literal IP", "development", "", "192.0.2.1:9091", "127.0.0.1:9091", true, false},
		{"public listener", "development", "", "127.0.0.1:9091", "0.0.0.0:9091", true, false},
		{"TLS server", "development", "server.crt", "127.0.0.1:9091", "127.0.0.1:9091", true, false},
		{"explicit permission required", "development", "", "127.0.0.1:9091", "127.0.0.1:9091", false, false},
		{"production never plaintext", " Production ", "", "127.0.0.1:9091", "127.0.0.1:9091", true, false},
		{"unknown public address", "development", "", "", "127.0.0.1:9091", true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if actual := agentUsesLoopbackPlaintext(test.environment, test.cert, test.advertised, test.listening, test.allowed); actual != test.expected {
				t.Fatalf("advertised plaintext=%v, want %v", actual, test.expected)
			}
		})
	}
}
