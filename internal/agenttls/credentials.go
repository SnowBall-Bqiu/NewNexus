package agenttls

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"time"

	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

type dynamicCredentials struct {
	manager                *Manager
	fallback               *tls.Config
	allowLoopbackPlaintext bool
}

// Credentials reads the durable switch for each new connection. Existing gRPC
// streams retain their negotiated transport until their normal reconnection.
func (m *Manager) Credentials(fallback *tls.Config, allowLoopbackPlaintext bool) credentials.TransportCredentials {
	if fallback != nil {
		fallback = fallback.Clone()
		fallback.MinVersion = max(fallback.MinVersion, tls.VersionTLS12)
		fallback.NextProtos = []string{"h2"}
	}
	return &dynamicCredentials{manager: m, fallback: fallback, allowLoopbackPlaintext: allowLoopbackPlaintext}
}

func (c *dynamicCredentials) ServerHandshake(raw net.Conn) (net.Conn, credentials.AuthInfo, error) {
	if err := raw.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return nil, nil, err
	}
	defer raw.SetDeadline(time.Time{})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	enabled, err := c.manager.Enabled(ctx)
	if err != nil {
		return nil, nil, err
	}
	if enabled {
		material, err := c.manager.Ensure(ctx)
		if err != nil {
			return nil, nil, err
		}
		return credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{material.Certificate}, MinVersion: tls.VersionTLS12, NextProtos: []string{"h2"}}).ServerHandshake(raw)
	}
	if c.fallback != nil {
		return credentials.NewTLS(c.fallback.Clone()).ServerHandshake(raw)
	}
	if !c.allowLoopbackPlaintext {
		return nil, nil, errors.New("Agent TLS is disabled and no explicitly permitted fallback is available")
	}
	// A switch must never turn an internet-facing connection into plaintext.
	// The legacy development exception serves local h2c proxies and tests only.
	host, _, err := net.SplitHostPort(raw.RemoteAddr().String())
	if err != nil || !net.ParseIP(host).IsLoopback() {
		return nil, nil, errors.New("Agent TLS is disabled and plaintext is restricted to loopback peers")
	}
	return insecure.NewCredentials().ServerHandshake(raw)
}

func (*dynamicCredentials) ClientHandshake(context.Context, string, net.Conn) (net.Conn, credentials.AuthInfo, error) {
	return nil, nil, errors.New("managed Agent TLS credentials are server-side only")
}

func (*dynamicCredentials) Info() credentials.ProtocolInfo {
	return credentials.ProtocolInfo{SecurityProtocol: "tls", SecurityVersion: "1.2"}
}

func (c *dynamicCredentials) Clone() credentials.TransportCredentials {
	return c.manager.Credentials(c.fallback, c.allowLoopbackPlaintext)
}

func (*dynamicCredentials) OverrideServerName(string) error {
	return errors.New("managed Agent TLS server name is bound to its authority")
}
