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
	"net/netip"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/SnowBall-Bqiu/NewNexus/internal/agenthub"
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
	address := net.JoinHostPort(task.Address, fmt.Sprintf("%d", defaultPort(task.Port, 443)))
	started := time.Now()
	connection, err := (&net.Dialer{}).DialContext(ctx, "tcp", address)
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
	port := task.Port
	if port == 0 {
		if parsed.Scheme == "http" {
			port = 80
		} else {
			port = 443
		}
	}
	target := task.Address
	if target == "" {
		target = parsed.Hostname()
	}
	if err := validateDestination(target); err != nil {
		return err
	}
	transport := &http.Transport{DisableKeepAlives: true, ForceAttemptHTTP2: true, TLSClientConfig: &tls.Config{ServerName: firstNonEmpty(task.SNI, parsed.Hostname()), MinVersion: tls.VersionTLS12}}
	transport.DialContext = func(dialCtx context.Context, network, _ string) (net.Conn, error) {
		started := time.Now()
		connection, err := (&net.Dialer{}).DialContext(dialCtx, network, net.JoinHostPort(target, fmt.Sprintf("%d", port)))
		result.TCPMS = milliseconds(time.Since(started))
		return connection, err
	}
	transport.DialTLSContext = func(dialCtx context.Context, network, _ string) (net.Conn, error) {
		started := time.Now()
		raw, err := (&net.Dialer{}).DialContext(dialCtx, network, net.JoinHostPort(target, fmt.Sprintf("%d", port)))
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
	client := &http.Client{Transport: transport, CheckRedirect: func(_ *http.Request, via []*http.Request) error {
		if len(via) >= 3 {
			return fmt.Errorf("too many redirects")
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
	response, err := client.Do(request)
	result.TTFBMS = milliseconds(time.Since(started))
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
	if task.ExpectedContains == "" {
		bytesRead, err := io.Copy(io.Discard, reader)
		result.BytesRead = bytesRead
		return err
	}
	buffered := bufio.NewReader(reader)
	raw, err := io.ReadAll(buffered)
	result.BytesRead = int64(len(raw))
	if err != nil {
		return err
	}
	if !strings.Contains(string(raw), task.ExpectedContains) {
		return fmt.Errorf("response assertion failed")
	}
	return nil
}

func validateDestination(host string) error {
	if address, err := netip.ParseAddr(host); err == nil {
		if !address.IsGlobalUnicast() || address.IsPrivate() || address.IsLoopback() || address.IsLinkLocalUnicast() {
			return fmt.Errorf("blocked destination")
		}
		return nil
	}
	addresses, err := net.LookupIP(host)
	if err != nil {
		return err
	}
	for _, address := range addresses {
		parsed, ok := netip.AddrFromSlice(address)
		if !ok || !parsed.IsGlobalUnicast() || parsed.IsPrivate() || parsed.IsLoopback() || parsed.IsLinkLocalUnicast() {
			return fmt.Errorf("hostname resolves to a blocked destination")
		}
	}
	return nil
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
