package api

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/newnexus/newnexus/internal/domain"
	"github.com/newnexus/newnexus/internal/security"
	"io"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/http"
	"net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"time"
)

func (s *Server) settingString(ctx context.Context, key string) string {
	row, e := s.Store.Get(ctx, "settings", key)
	if e != nil {
		return ""
	}
	value := domain.S(row, "value")
	if domain.B(row, "encrypted") {
		plain, e := s.Vault.Open(value, "setting:"+key)
		if e != nil {
			return ""
		}
		return string(plain)
	}
	return value
}

func alertEventID(alert domain.Doc) string {
	if incident := domain.S(alert, "incident_id"); incident != "" {
		return incident
	}
	return domain.S(alert, "id")
}

func alertNotificationKey(alert domain.Doc) string {
	key := "delivery:" + domain.S(alert, "incident_id")
	if domain.S(alert, "incident_id") == "" {
		key = "delivery:" + domain.S(alert, "id") + ":" + domain.S(alert, "created_at")
	}
	return key
}

func (s *Server) queueAlertNotification(ctx context.Context, alert domain.Doc) (domain.Doc, error) {
	return s.Queue(ctx, "notify", "alerts", alert, alertNotificationKey(alert))
}

func (s *Server) deliverAlert(ctx context.Context, alert domain.Doc) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	webhook := s.settingString(ctx, "alerts.webhook_url")
	mailHost := s.settingString(ctx, "alerts.smtp_host")
	if webhook != "" {
		if _, e := security.ValidateURL(webhook); e != nil {
			return e
		}
		secret := s.settingString(ctx, "alerts.webhook_secret")
		if len(secret) < 16 {
			return errors.New("webhook signing secret must contain at least 16 characters")
		}
		payload, e := json.Marshal(domain.Doc{"id": alertEventID(alert), "event": "alert.created", "created_at": alert["created_at"], "data": public(alert)})
		if e != nil {
			return e
		}
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(payload)
		req, e := http.NewRequestWithContext(ctx, "POST", webhook, bytes.NewReader(payload))
		if e != nil {
			return e
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-NewNexus-Event-ID", alertEventID(alert))
		req.Header.Set("X-NewNexus-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
		client := s.webhookClient
		if client == nil {
			client = security.SafeClient(20 * time.Second)
		}
		resp, e := client.Do(req)
		if e != nil {
			return e
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 65536))
		resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return fmt.Errorf("webhook returned HTTP %d", resp.StatusCode)
		}
	}
	if mailHost != "" {
		return s.sendMail(ctx, mailHost, alert)
	}
	return nil
}
func (s *Server) sendMail(ctx context.Context, host string, alert domain.Doc) error {
	if strings.ContainsAny(host, "\r\n") {
		return errors.New("invalid SMTP hostname")
	}
	ips, e := security.ResolvePublic(ctx, host)
	if e != nil {
		return e
	}
	port := s.settingString(ctx, "alerts.smtp_port")
	if port == "" {
		port = "587"
	}
	if _, e = strconv.Atoi(port); e != nil {
		return errors.New("invalid SMTP port")
	}
	from := s.settingString(ctx, "alerts.email_from")
	to := s.settingString(ctx, "alerts.email_to")
	for _, value := range []string{from, to} {
		if value == "" || strings.ContainsAny(value, "\r\n") {
			return errors.New("SMTP from/to address is missing or invalid")
		}
	}
	dialer := net.Dialer{Timeout: 10 * time.Second}
	conn, e := dialer.DialContext(ctx, "tcp", net.JoinHostPort(ips[0].String(), port))
	if e != nil {
		return e
	}
	tlsConfig := &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}
	return s.smtpSession(ctx, conn, tlsConfig, host, port, from, to, alert)
}

// smtpSession owns a connected socket and performs the same verified TLS and
// SMTP dialogue for implicit TLS and STARTTLS. Public-address validation and
// pinned dialing remain in sendMail; callers cannot configure a test transport
// through the REST API.
func (s *Server) smtpSession(ctx context.Context, conn net.Conn, tlsConfig *tls.Config, host, port, from, to string, alert domain.Doc) error {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(20 * time.Second))
	fromAddress, e := mail.ParseAddress(from)
	if e != nil {
		return errors.New("invalid SMTP sender address")
	}
	toAddress, e := mail.ParseAddress(to)
	if e != nil {
		return errors.New("invalid SMTP recipient address")
	}
	if port == "465" {
		tlsConn := tls.Client(conn, tlsConfig)
		if e = tlsConn.HandshakeContext(ctx); e != nil {
			conn.Close()
			return e
		}
		conn = tlsConn
	}
	client, e := smtp.NewClient(conn, host)
	if e != nil {
		conn.Close()
		return e
	}
	defer client.Close()
	if port != "465" {
		if ok, _ := client.Extension("STARTTLS"); !ok {
			return errors.New("SMTP server must support STARTTLS")
		}
		if e = client.StartTLS(tlsConfig); e != nil {
			return e
		}
	}
	username := s.settingString(ctx, "alerts.smtp_username")
	if username != "" {
		if e = client.Auth(smtp.PlainAuth("", username, s.settingString(ctx, "alerts.smtp_password"), host)); e != nil {
			return e
		}
	}
	if e = client.Mail(fromAddress.Address); e != nil {
		return e
	}
	if e = client.Rcpt(toAddress.Address); e != nil {
		return e
	}
	writer, e := client.Data()
	if e != nil {
		return e
	}
	title := strings.NewReplacer("\r", " ", "\n", " ").Replace(domain.S(alert, "title"))
	subject := mime.QEncoding.Encode("UTF-8", "[NewNexus] "+title)
	_, e = fmt.Fprintf(writer, "From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: %s\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n", fromAddress.String(), toAddress.String(), subject, time.Now().UTC().Format(time.RFC1123Z))
	if e != nil {
		return e
	}
	encoded := quotedprintable.NewWriter(writer)
	if _, e = fmt.Fprintf(encoded, "%s\r\nEvent ID: %s\r\n", domain.S(alert, "message"), alertEventID(alert)); e != nil {
		return e
	}
	if e = encoded.Close(); e != nil {
		return e
	}
	if e = writer.Close(); e != nil {
		return e
	}
	return client.Quit()
}
