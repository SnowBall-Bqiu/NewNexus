package api

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/textproto"
	"strings"
	"testing"
	"time"

	"github.com/newnexus/newnexus/internal/domain"
)

func smtpCertificate(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "smtp.newnexus.test"}, DNSNames: []string{"smtp.newnexus.test"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	private, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private}))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(parsed)
	return pair, roots
}

type smtpObservation struct {
	body, from, to     string
	authenticated, tls bool
	err                error
}

func smtpFixture(t *testing.T, cert tls.Certificate, implicit, advertiseTLS, rejectAuth bool) (net.Conn, <-chan smtpObservation) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	observed := make(chan smtpObservation, 1)
	go func() {
		result := smtpObservation{}
		defer func() { observed <- result }()
		conn, err := listener.Accept()
		if err != nil {
			result.err = err
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		upgrade := func() bool {
			secure := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
			if err := secure.Handshake(); err != nil {
				result.err = err
				return false
			}
			conn = secure
			result.tls = true
			return true
		}
		if implicit && !upgrade() {
			return
		}
		reader := textproto.NewReader(bufio.NewReader(conn))
		writer := bufio.NewWriter(conn)
		send := func(text string) bool {
			_, err := fmt.Fprint(writer, text)
			if err == nil {
				err = writer.Flush()
			}
			if err != nil {
				result.err = err
				return false
			}
			return true
		}
		if !send("220 smtp.newnexus.test ESMTP\r\n") {
			return
		}
		for {
			line, err := reader.ReadLine()
			if err != nil {
				if err != io.EOF {
					result.err = err
				}
				return
			}
			switch {
			case strings.HasPrefix(line, "EHLO "):
				if !result.tls && advertiseTLS {
					if !send("250-smtp.newnexus.test\r\n250 STARTTLS\r\n") {
						return
					}
				} else {
					if !send("250-smtp.newnexus.test\r\n250 AUTH PLAIN\r\n") {
						return
					}
				}
			case line == "STARTTLS":
				if !advertiseTLS || !send("220 Start TLS\r\n") {
					return
				}
				if !upgrade() {
					return
				}
				reader = textproto.NewReader(bufio.NewReader(conn))
				writer = bufio.NewWriter(conn)
			case strings.HasPrefix(line, "AUTH PLAIN "):
				if !result.tls {
					result.err = fmt.Errorf("credentials sent before TLS")
					return
				}
				raw, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(line, "AUTH PLAIN "))
				if string(raw) != "\x00qa-user\x00qa-password" {
					result.err = fmt.Errorf("unexpected authentication payload")
					return
				}
				if rejectAuth {
					if !send("535 Authentication rejected\r\n") {
						return
					}
				} else {
					result.authenticated = true
					if !send("235 Authenticated\r\n") {
						return
					}
				}
			case strings.HasPrefix(line, "MAIL FROM:"):
				result.from = strings.TrimPrefix(line, "MAIL FROM:")
				if !send("250 Sender accepted\r\n") {
					return
				}
			case strings.HasPrefix(line, "RCPT TO:"):
				result.to = strings.TrimPrefix(line, "RCPT TO:")
				if !send("250 Recipient accepted\r\n") {
					return
				}
			case line == "DATA":
				if !send("354 End with dot\r\n") {
					return
				}
				data, err := reader.ReadDotBytes()
				if err != nil {
					result.err = err
					return
				}
				result.body = string(data)
				if !send("250 Queued\r\n") {
					return
				}
			case line == "QUIT":
				send("221 Bye\r\n")
				return
			default:
				result.err = fmt.Errorf("unexpected SMTP command")
				return
			}
		}
	}()
	conn, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return conn, observed
}

func TestSMTPVerifiedTLSAuthenticationAndMessage(t *testing.T) {
	for _, implicit := range []bool{false, true} {
		t.Run(fmt.Sprint("implicit=", implicit), func(t *testing.T) {
			s := testServer(t)
			s.DevAuth = true
			if response := request(t, s, "PUT", "settings", domain.Doc{"alerts.smtp_username": "qa-user", "alerts.smtp_password": "qa-password"}, nil, ""); response.Code != 200 {
				t.Fatal(response.Code)
			}
			certificate, roots := smtpCertificate(t)
			conn, done := smtpFixture(t, certificate, implicit, true, false)
			port := "587"
			if implicit {
				port = "465"
			}
			err := s.smtpSession(context.Background(), conn, &tls.Config{ServerName: "smtp.newnexus.test", RootCAs: roots, MinVersion: tls.VersionTLS12}, "smtp.newnexus.test", port, "NewNexus <sender@example.com>", "Operator <recipient@example.com>", domain.Doc{"id": "event-qa", "title": "线路故障\r\n仅测试", "message": "本地 SMTP 验收\n.second line"})
			if err != nil {
				t.Fatal(err)
			}
			result := <-done
			if result.err != nil {
				t.Fatal(result.err)
			}
			if !result.tls || !result.authenticated || result.from != "<sender@example.com>" || result.to != "<recipient@example.com>" {
				t.Fatalf("invalid TLS/auth/envelope: %+v", result)
			}
			message, err := mail.ReadMessage(strings.NewReader(result.body))
			if err != nil {
				t.Fatal(err)
			}
			subject, err := new(mime.WordDecoder).DecodeHeader(message.Header.Get("Subject"))
			if err != nil || subject != "[NewNexus] 线路故障  仅测试" {
				t.Fatalf("subject=%q err=%v", subject, err)
			}
			body, _ := io.ReadAll(quotedprintable.NewReader(message.Body))
			if !strings.Contains(string(body), "本地 SMTP 验收") || !strings.Contains(string(body), "Event ID: event-qa") {
				t.Fatalf("missing message body/event ID: %s", body)
			}
		})
	}
}

func TestSMTPRejectsMissingTLSWrongCertificateAndBadCredentials(t *testing.T) {
	for _, scenario := range []struct {
		name                            string
		startTLS, wrongName, rejectAuth bool
	}{{"no-starttls", false, false, false}, {"wrong-certificate", true, true, false}, {"auth-rejected", true, false, true}} {
		t.Run(scenario.name, func(t *testing.T) {
			s := testServer(t)
			s.DevAuth = true
			_ = request(t, s, "PUT", "settings", domain.Doc{"alerts.smtp_username": "qa-user", "alerts.smtp_password": "qa-password"}, nil, "")
			certificate, roots := smtpCertificate(t)
			conn, done := smtpFixture(t, certificate, false, scenario.startTLS, scenario.rejectAuth)
			serverName := "smtp.newnexus.test"
			if scenario.wrongName {
				serverName = "other.newnexus.test"
			}
			err := s.smtpSession(context.Background(), conn, &tls.Config{ServerName: serverName, RootCAs: roots, MinVersion: tls.VersionTLS12}, "smtp.newnexus.test", "587", "sender@example.com", "recipient@example.com", domain.Doc{"title": "QA", "message": "must not deliver"})
			if err == nil {
				t.Fatal("invalid SMTP security state accepted")
			}
			result := <-done
			if result.body != "" || result.from != "" {
				t.Fatal("delivered after failed TLS/authentication")
			}
		})
	}
}
