// Package agentclient implements the outbound-only NewNexus probe node.
package agentclient

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"math"
	"net"
	"time"
)

const ProtocolVersion = 1

// Version is set with -ldflags at release build time.
var Version = "0.1.0"

type Task struct {
	ID               string            `json:"id"`
	CandidateID      string            `json:"candidate_id,omitempty"`
	Address          string            `json:"address"`
	Mode             string            `json:"mode"`
	URL              string            `json:"url,omitempty"`
	Host             string            `json:"host,omitempty"`
	SNI              string            `json:"sni,omitempty"`
	Method           string            `json:"method,omitempty"`
	Headers          map[string]string `json:"headers,omitempty"`
	Body             string            `json:"body,omitempty"`
	ExpectedStatuses []int             `json:"expected_statuses,omitempty"`
	ExpectedContains string            `json:"expected_contains,omitempty"`
	TimeoutMS        int               `json:"timeout_ms,omitempty"`
	MaxBytes         int64             `json:"max_bytes,omitempty"`
	Port             int               `json:"port,omitempty"`
	Samples          int               `json:"samples,omitempty"`
	Retries          int               `json:"retries,omitempty"`
	MaxRedirects     *int              `json:"max_redirects,omitempty"`
	UserAgent        string            `json:"user_agent,omitempty"`
}

type Result struct {
	ID               string  `json:"id"`
	TaskID           string  `json:"task_id"`
	CandidateID      string  `json:"candidate_id,omitempty"`
	Address          string  `json:"address"`
	Success          bool    `json:"success"`
	TCPMS            float64 `json:"tcp_ms"`
	TLSMS            float64 `json:"tls_ms"`
	TTFBMS           float64 `json:"ttfb_ms"`
	P50MS            float64 `json:"p50_ms"`
	P95MS            float64 `json:"p95_ms"`
	DownloadBPS      float64 `json:"download_bps"`
	SuccessRate      float64 `json:"success_rate"`
	SampleCount      int     `json:"sample_count"`
	RequestedSamples int     `json:"requested_samples"`
	AttemptedSamples int     `json:"attempted_samples"`
	StatusCode       int     `json:"status_code,omitempty"`
	BytesRead        int64   `json:"bytes_read"`
	Error            string  `json:"error,omitempty"`
	ObservedAt       string  `json:"observed_at"`
}

type Credentials struct {
	NodeID          string `json:"node_id"`
	Credential      string `json:"credential"`
	ProtocolVersion int    `json:"protocol_version"`
}

type Resolver interface {
	LookupIPAddr(context.Context, string) ([]net.IPAddr, error)
}

type ContextDialer interface {
	DialContext(context.Context, string, string) (net.Conn, error)
}

// ProbeOptions are local security limits. A remote task cannot increase them.
type ProbeOptions struct {
	AllowedPorts  []int
	Resolver      Resolver
	Dialer        ContextDialer
	TLSConfig     *tls.Config
	MaxTimeout    time.Duration
	MaxBytes      int64
	MaxSamples    int
	MaxRedirects  int
	RatePerSecond float64
	UserAgent     string
}

func (o *ProbeOptions) defaults() {
	if len(o.AllowedPorts) == 0 {
		o.AllowedPorts = []int{80, 443}
	}
	if o.Resolver == nil {
		o.Resolver = net.DefaultResolver
	}
	if o.Dialer == nil {
		o.Dialer = &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	}
	if o.MaxTimeout == 0 {
		o.MaxTimeout = 30 * time.Second
	}
	if o.MaxBytes == 0 {
		o.MaxBytes = 16 << 20
	}
	if o.MaxSamples == 0 {
		o.MaxSamples = 10
	}
	if o.MaxRedirects == 0 {
		o.MaxRedirects = 5
	}
	if o.RatePerSecond == 0 {
		o.RatePerSecond = 10
	}
	if o.UserAgent == "" {
		o.UserAgent = "Mozilla/5.0 (X11; Linux x86_64; rv:132.0) Gecko/20100101 Firefox/132.0 NewNexus/" + Version
	}
}

type Config struct {
	Server            string
	EnrollmentToken   string
	Name              string
	Region            string
	Carrier           string
	Mode              string
	DataDir           string
	CAFile            string
	ClientCertFile    string
	ClientKeyFile     string
	ServerName        string
	InsecureLoopback  bool
	Concurrency       int
	MaxPending        int
	MaxOutboxBytes    int64
	HeartbeatInterval time.Duration
	Probe             ProbeOptions
	Logger            *slog.Logger
}

func (c *Config) defaults() error {
	if c.Server == "" {
		return fmt.Errorf("server is required")
	}
	if c.DataDir == "" {
		c.DataDir = "./agent-data"
	}
	if c.Name == "" {
		c.Name = "probe-node"
	}
	if c.Mode == "" {
		c.Mode = "all"
	}
	if c.Mode != "all" && c.Mode != "tcp" && c.Mode != "http" {
		return fmt.Errorf("node mode must be all, tcp, or http")
	}
	if c.Concurrency == 0 {
		c.Concurrency = 4
	}
	if c.MaxPending == 0 {
		c.MaxPending = 10000
	}
	if c.MaxOutboxBytes == 0 {
		c.MaxOutboxBytes = 128 << 20
	}
	if c.HeartbeatInterval == 0 {
		c.HeartbeatInterval = 15 * time.Second
	}
	if c.HeartbeatInterval < 5*time.Second || c.HeartbeatInterval > 300*time.Second {
		return fmt.Errorf("heartbeat interval must be between 5s and 300s")
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	c.Probe.defaults()
	if c.Concurrency < 1 || c.Concurrency > 128 {
		return fmt.Errorf("concurrency must be between 1 and 128")
	}
	if c.MaxPending < c.Concurrency || c.MaxPending > 100000 {
		return fmt.Errorf("max pending must be between concurrency and 100000")
	}
	if c.MaxOutboxBytes < 1<<20 {
		return fmt.Errorf("max outbox bytes must be at least 1 MiB")
	}
	if math.IsNaN(c.Probe.RatePerSecond) || math.IsInf(c.Probe.RatePerSecond, 0) || c.Probe.RatePerSecond < .001 || c.Probe.RatePerSecond > 10000 {
		return fmt.Errorf("rate must be between 0.001 and 10000")
	}
	if c.Probe.MaxTimeout < time.Second || c.Probe.MaxTimeout > time.Minute {
		return fmt.Errorf("max timeout must be between 1s and 60s")
	}
	if c.Probe.MaxBytes < 1024 || c.Probe.MaxBytes > 128<<20 {
		return fmt.Errorf("max bytes must be between 1 KiB and 128 MiB")
	}
	if c.Probe.MaxSamples < 1 || c.Probe.MaxSamples > 10 {
		return fmt.Errorf("max samples must be between 1 and 10")
	}
	for _, p := range c.Probe.AllowedPorts {
		if p < 1 || p > 65535 {
			return fmt.Errorf("invalid allowed port %d", p)
		}
	}
	return nil
}
