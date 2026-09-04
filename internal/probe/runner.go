package probe

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/SnowBall-Bqiu/NewNexus/internal/agenthub"
	"github.com/SnowBall-Bqiu/NewNexus/internal/netguard"
)

type Result struct {
	JobID         string    `json:"jobId"`
	CandidateID   string    `json:"candidateId,omitempty"`
	RouteTargetID string    `json:"routeTargetId,omitempty"`
	Mode          string    `json:"mode"`
	Success       bool      `json:"success"`
	ErrorCode     string    `json:"errorCode,omitempty"`
	TCPMS         float64   `json:"tcpMs"`
	TLSMS         float64   `json:"tlsMs"`
	TTFBMS        float64   `json:"ttfbMs"`
	TotalMS       float64   `json:"totalMs"`
	BytesRead     int64     `json:"bytesRead"`
	DownloadBPS   float64   `json:"downloadBps"`
	StatusCode    int       `json:"statusCode"`
	ObservedAt    time.Time `json:"observedAt"`
}

func Run(ctx context.Context, task agenthub.Task) Result {
	result := Result{JobID: task.JobID, CandidateID: task.CandidateID, RouteTargetID: task.RouteTargetID, Mode: task.Mode, ObservedAt: time.Now().UTC()}
	timeout := time.Duration(task.TimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	started := time.Now()
	if task.Mode == "tcp" {
		err := runTCP(ctx, task, &result)
		result.TotalMS = milliseconds(time.Since(started))
		result.Success = err == nil
		if err != nil {
			result.ErrorCode = classifyError(err)
		}
		return result
	}
	err := runHTTP(ctx, task, &result)
	result.TotalMS = milliseconds(time.Since(started))
	result.Success = err == nil
	if err != nil {
		result.ErrorCode = classifyError(err)
	}
	return result
}

func runTCP(ctx context.Context, task agenthub.Task, result *Result) error {
	port := defaultPort(task.Port, 443)
	if !portAllowed(task, port) {
		return fmt.Errorf("blocked probe port %d", port)
	}
	addresses, err := netguard.ResolvePublic(ctx, net.DefaultResolver, task.Address)
	if err != nil {
		return fmt.Errorf("blocked destination: %w", err)
	}
	address := net.JoinHostPort(addresses[0].String(), fmt.Sprintf("%d", port))
	started := time.Now()
	connection, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", address)
	result.TCPMS = milliseconds(time.Since(started))
	if err != nil {
		return err
	}
	return connection.Close()
}

func runHTTP(ctx context.Context, task agenthub.Task, result *Result) error {
	parsed, err := url.Parse(task.URL)
	if err != nil || parsed.Hostname() == "" {
		return fmt.Errorf("invalid probe URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("invalid probe URL scheme")
	}
	port := task.Port
	if port == 0 {
		if parsed.Scheme == "http" {
			port = 80
		} else {
			port = 443
		}
	}
	if !portAllowed(task, port) {
		return fmt.Errorf("blocked probe port %d", port)
	}
	target := task.Address
	if target == "" {
		target = parsed.Hostname()
	}
	addresses, err := netguard.ResolvePublic(ctx, net.DefaultResolver, target)
	if err != nil {
		return err
	}
	targetIP := addresses[0].String()
	transport := &http.Transport{DisableKeepAlives: true, ForceAttemptHTTP2: true, TLSClientConfig: &tls.Config{ServerName: firstNonEmpty(task.SNI, parsed.Hostname()), MinVersion: tls.VersionTLS12}}
	transport.DialContext = func(dialCtx context.Context, network, _ string) (net.Conn, error) {
		started := time.Now()
		connection, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(dialCtx, network, net.JoinHostPort(targetIP, fmt.Sprintf("%d", port)))
		result.TCPMS = milliseconds(time.Since(started))
		return connection, err
	}
	transport.DialTLSContext = func(dialCtx context.Context, network, _ string) (net.Conn, error) {
		started := time.Now()
		raw, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(dialCtx, network, net.JoinHostPort(targetIP, fmt.Sprintf("%d", port)))
		result.TCPMS = milliseconds(time.Since(started))
		if err != nil {
			return nil, err
		}
		tlsStarted := time.Now()
		secure := tls.Client(raw, transport.TLSClientConfig.Clone())
		if err := secure.HandshakeContext(dialCtx); err != nil {
			_ = raw.Close()
			return nil, err
		}
		result.TLSMS = milliseconds(time.Since(tlsStarted))
		return secure, nil
	}
	client := &http.Client{Transport: transport, CheckRedirect: func(request *http.Request, via []*http.Request) error {
		if len(via) >= 3 {
			return fmt.Errorf("too many redirects")
		}
		if normalizeHost(request.URL.Hostname()) != normalizeHost(parsed.Hostname()) {
			return fmt.Errorf("redirect target hostname changed")
		}
		return nil
	}}
	method := task.Method
	if method == "" {
		method = http.MethodGet
	}
	request, err := http.NewRequestWithContext(ctx, method, task.URL, nil)
	if err != nil {
		return err
	}
	request.Host = firstNonEmpty(task.Host, parsed.Hostname())
	for key, value := range task.Headers {
		request.Header.Set(key, value)
	}
	if request.Header.Get("User-Agent") == "" {
		request.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64; rv:128.0) Gecko/20100101 Firefox/128.0")
	}
	started := time.Now()
	trace := &httptrace.ClientTrace{GotFirstResponseByte: func() {
		if result.TTFBMS == 0 {
			result.TTFBMS = milliseconds(time.Since(started))
		}
	}}
	request = request.WithContext(httptrace.WithClientTrace(request.Context(), trace))
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	result.StatusCode = response.StatusCode
	allowed := task.ExpectedStatuses
	if len(allowed) == 0 {
		allowed = []int{200, 204}
	}
	if !containsInt(allowed, response.StatusCode) {
		return fmt.Errorf("unexpected HTTP status %d", response.StatusCode)
	}
	limit := task.MaxBytes
	if limit <= 0 || limit > 8<<20 {
		limit = 1 << 20
	}
	reader := io.LimitReader(response.Body, limit)
	downloadStarted := time.Now()
	if task.ExpectedContains == "" {
		bytesRead, err := io.Copy(io.Discard, reader)
		result.BytesRead = bytesRead
		result.DownloadBPS = bytesPerSecond(bytesRead, time.Since(downloadStarted))
		return err
	}
	buffered := bufio.NewReader(reader)
	raw, err := io.ReadAll(buffered)
	result.BytesRead = int64(len(raw))
	result.DownloadBPS = bytesPerSecond(result.BytesRead, time.Since(downloadStarted))
	if err != nil {
		return err
	}
	if !strings.Contains(string(raw), task.ExpectedContains) {
		return fmt.Errorf("response assertion failed")
	}
	return nil
}

func validateDestination(host string) error {
	_, err := netguard.ResolvePublic(context.Background(), net.DefaultResolver, host)
	return err
}

func SaveState(path string, value any) error {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0600)
}
func LoadState(path string, value any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, value)
}
func defaultPort(value, fallback int) int {
	if value == 0 {
		return fallback
	}
	return value
}
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
func milliseconds(value time.Duration) float64 { return float64(value.Microseconds()) / 1000 }
func bytesPerSecond(bytes int64, duration time.Duration) float64 {
	if bytes <= 0 || duration <= 0 {
		return 0
	}
	return float64(bytes) / duration.Seconds()
}
func portAllowed(task agenthub.Task, port int) bool {
	allowed := task.AllowedPorts
	if len(allowed) == 0 {
		allowed = []int{80, 443, 8080, 8443}
	}
	for _, candidate := range allowed {
		if candidate == port {
			return true
		}
	}
	return false
}
func normalizeHost(value string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(value)), ".")
}
func containsInt(values []int, expected int) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}
func classifyError(err error) string {
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "blocked"):
		return "blocked_destination"
	case strings.Contains(message, "timeout"), strings.Contains(message, "deadline"):
		return "timeout"
	case strings.Contains(message, "tls"), strings.Contains(message, "certificate"):
		return "tls_error"
	case strings.Contains(message, "status"):
		return "http_status"
	case strings.Contains(message, "assertion"):
		return "assertion_failed"
	default:
		return "connection_error"
	}
}
