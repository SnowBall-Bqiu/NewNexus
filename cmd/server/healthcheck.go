package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"
)

func healthcheck(addr string) error {
	host, port, e := net.SplitHostPort(addr)
	if e != nil {
		return e
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	target := net.JoinHostPort(host, port)
	scheme := "http"
	transport := &http.Transport{Proxy: nil}
	if certPath := os.Getenv("NEWNEXUS_TLS_CERT"); certPath != "" {
		scheme = "https"
		raw, e := os.ReadFile(certPath)
		if e != nil {
			return e
		}
		roots, e := x509.SystemCertPool()
		if e != nil {
			roots = x509.NewCertPool()
		}
		if !roots.AppendCertsFromPEM(raw) {
			return errors.New("TLS health check certificate is not valid PEM")
		}
		block, _ := pem.Decode(raw)
		if block == nil {
			return errors.New("TLS health check certificate is missing")
		}
		cert, e := x509.ParseCertificate(block.Bytes)
		if e != nil {
			return e
		}
		serverName := host
		if len(cert.DNSNames) > 0 {
			serverName = cert.DNSNames[0]
		}
		transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, ServerName: serverName}
		dialer := net.Dialer{Timeout: 3 * time.Second}
		transport.DialContext = func(ctx context.Context, network, ignored string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, target)
		}
	}
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	resp, e := client.Get(scheme + "://" + target + "/healthz")
	if e != nil {
		return e
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("health check returned HTTP %d", resp.StatusCode)
	}
	return nil
}
