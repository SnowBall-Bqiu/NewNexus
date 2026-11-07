package agentclient

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Prober struct {
	options ProbeOptions
	mu      sync.Mutex
	next    time.Time
}

func NewProber(options ProbeOptions) *Prober {
	options.defaults()
	return &Prober{options: options}
}

func (p *Prober) wait(ctx context.Context) error {
	p.mu.Lock()
	now := time.Now()
	start := p.next
	if start.Before(now) {
		start = now
	}
	p.next = start.Add(time.Duration(float64(time.Second) / p.options.RatePerSecond))
	p.mu.Unlock()
	if delay := time.Until(start); delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
	return ctx.Err()
}

func (p *Prober) Probe(ctx context.Context, task Task) Result {
	result := Result{TaskID: task.ID, CandidateID: task.CandidateID, Address: task.Address}
	if err := p.validate(&task); err != nil {
		result.Error = err.Error()
		result.ObservedAt = time.Now().UTC().Format(time.RFC3339Nano)
		return result
	}
	var latencies []float64
	var failures []string
	successes := 0
	result.RequestedSamples = task.Samples
	roundsCompleted := 0
probeLoop:
	for n := 0; n < task.Samples; n++ {
		var lastError string
		for attempt := 0; attempt <= task.Retries; attempt++ {
			if err := p.wait(ctx); err != nil {
				failures = append(failures, "probe cancelled")
				break probeLoop
			}
			sampleCtx, cancel := context.WithTimeout(ctx, time.Duration(task.TimeoutMS)*time.Millisecond)
			m, err := p.sample(sampleCtx, task)
			cancel()
			result.SampleCount++
			if err != nil {
				lastError = safeProbeError(err)
				continue
			}
			successes++
			roundsCompleted++
			result.TCPMS += m.tcp
			result.TLSMS += m.tls
			result.TTFBMS += m.ttfb
			result.DownloadBPS += m.bps
			result.BytesRead += m.bytes
			result.StatusCode = m.status
			latency := m.ttfb
			if task.Mode == "tcp" || task.Mode == "tls" {
				latency = m.tcp + m.tls
			}
			latencies = append(latencies, latency)
			lastError = ""
			break
		}
		if lastError != "" {
			failures = append(failures, lastError)
		}
	}
	if successes > 0 {
		n := float64(successes)
		result.TCPMS /= n
		result.TLSMS /= n
		result.TTFBMS /= n
		result.DownloadBPS /= n
		sort.Float64s(latencies)
		result.P50MS = percentile(latencies, .5)
		result.P95MS = percentile(latencies, .95)
	}
	if result.SampleCount > 0 {
		result.SuccessRate = float64(successes) / float64(result.SampleCount)
	}
	result.AttemptedSamples = result.SampleCount
	result.Success = roundsCompleted == task.Samples
	if len(failures) > 0 {
		result.Error = failures[0]
		if len(failures) > 1 {
			result.Error += fmt.Sprintf(" (%d samples failed)", len(failures))
		}
	}
	result.ObservedAt = time.Now().UTC().Format(time.RFC3339Nano)
	return result
}

func percentile(values []float64, quantile float64) float64 {
	if len(values) == 0 {
		return 0
	}
	i := int(math.Ceil(float64(len(values))*quantile)) - 1
	if i < 0 {
		i = 0
	}
	return values[i]
}

func safeProbeError(err error) string {
	var u *url.Error
	if errors.As(err, &u) {
		err = u.Err
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "probe timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "probe cancelled"
	}
	s := strings.ReplaceAll(strings.ReplaceAll(err.Error(), "\n", " "), "\r", " ")
	if len(s) > 512 {
		s = s[:512]
	}
	return s
}

func (p *Prober) validate(task *Task) error {
	if task.ID == "" || len(task.ID) > 256 {
		return fmt.Errorf("invalid task id")
	}
	if task.Mode == "" {
		task.Mode = "all"
	}
	if task.Mode != "tcp" && task.Mode != "tls" && task.Mode != "http" && task.Mode != "all" {
		return fmt.Errorf("unsupported probe mode")
	}
	if task.TimeoutMS == 0 {
		task.TimeoutMS = 5000
	}
	if task.TimeoutMS < 100 || task.TimeoutMS > 60000 || time.Duration(task.TimeoutMS)*time.Millisecond > p.options.MaxTimeout {
		return fmt.Errorf("task timeout exceeds node policy")
	}
	if task.Samples == 0 {
		task.Samples = 3
	}
	if task.Samples < 1 || task.Samples > p.options.MaxSamples {
		return fmt.Errorf("task sample count exceeds node policy")
	}
	if task.Retries < 0 || task.Retries > 3 {
		return fmt.Errorf("task retries exceeds node policy (0..3)")
	}
	if task.MaxBytes == 0 {
		task.MaxBytes = 1 << 20
	}
	if task.MaxBytes < 1 || task.MaxBytes > p.options.MaxBytes {
		return fmt.Errorf("task response size exceeds node policy")
	}
	if task.MaxRedirects != nil && (*task.MaxRedirects < 0 || *task.MaxRedirects > p.options.MaxRedirects) {
		return fmt.Errorf("task redirect count exceeds node policy")
	}
	if len(task.Body) > 65536 || len(task.ExpectedContains) > 65536 || len(task.Headers) > 64 {
		return fmt.Errorf("task HTTP template exceeds node policy")
	}
	for _, identity := range []string{task.Host, task.SNI} {
		if len(identity) > 1024 || strings.ContainsAny(identity, "\r\n\t/\\@ ") {
			return fmt.Errorf("invalid Host or SNI override")
		}
	}
	for name, value := range task.Headers {
		if strings.ContainsAny(name+value, "\r\n") || len(name)+len(value) > 8192 {
			return fmt.Errorf("invalid HTTP header")
		}
		if strings.EqualFold(name, "Host") {
			return fmt.Errorf("use host field instead of Host header")
		}
	}
	for _, status := range task.ExpectedStatuses {
		if status < 100 || status > 599 {
			return fmt.Errorf("invalid expected HTTP status")
		}
	}
	return nil
}

type measurement struct {
	tcp, tls, ttfb, bps float64
	bytes               int64
	status              int
}

func (p *Prober) sample(ctx context.Context, task Task) (measurement, error) {
	u, err := parseProbeURL(task)
	if err != nil {
		return measurement{}, err
	}
	port := task.Port
	host, port, err := targetHostPort(task.Address, port)
	if err != nil {
		return measurement{}, err
	}
	fixedPort := port != 0
	if port == 0 && u != nil && u.Port() != "" {
		port, err = strconv.Atoi(u.Port())
		if err != nil {
			return measurement{}, fmt.Errorf("invalid URL port")
		}
	}
	if port == 0 {
		if u != nil && u.Scheme == "http" {
			port = 80
		} else {
			port = 443
		}
	}
	if !allowedPort(p.options.AllowedPorts, port) {
		return measurement{}, fmt.Errorf("target port is not in node allowlist")
	}
	addresses, err := resolvePublic(ctx, p.options.Resolver, host)
	if err != nil {
		return measurement{}, err
	}
	if len(addresses) > 16 {
		addresses = addresses[:16]
	}
	if u != nil {
		if _, err = resolvePublic(ctx, p.options.Resolver, u.Hostname()); err != nil {
			return measurement{}, fmt.Errorf("unsafe business URL: %w", err)
		}
	}
	// Resolved IPs are pinned, then passed as literals to the dialer. This also
	// prevents DNS rebinding between URL validation and transport connection.
	dial := func(ctx context.Context, _ string, destination string) (net.Conn, error) {
		dialPort := port
		// The candidate route stays fixed across redirects. Unless an explicit
		// candidate port override was provided, HTTP -> HTTPS may change the
		// destination port after passing the same local allowlist.
		if !fixedPort && destination != "" && (task.Mode == "http" || task.Mode == "all") {
			_, requested, err := net.SplitHostPort(destination)
			if err != nil {
				return nil, fmt.Errorf("invalid HTTP transport destination")
			}
			dialPort, err = strconv.Atoi(requested)
			if err != nil || !allowedPort(p.options.AllowedPorts, dialPort) {
				return nil, fmt.Errorf("target port is not in node allowlist")
			}
		}
		var lastErr error
		for i, address := range addresses {
			attemptCtx := ctx
			cancel := func() {}
			// A blackholed IPv6 endpoint must leave a time budget for the
			// remaining public IPv4/IPv6 answers instead of consuming the whole
			// sample timeout before an available address can be attempted.
			if deadline, ok := ctx.Deadline(); ok && len(addresses)-i > 1 {
				attemptCtx, cancel = context.WithTimeout(ctx, time.Until(deadline)/time.Duration(len(addresses)-i))
			}
			conn, err := p.options.Dialer.DialContext(attemptCtx, "tcp", net.JoinHostPort(address.IP.String(), strconv.Itoa(dialPort)))
			cancel()
			if err == nil {
				return conn, nil
			}
			lastErr = err
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
		}
		return nil, lastErr
	}
	if task.Mode == "tcp" || task.Mode == "tls" {
		start := time.Now()
		conn, err := dial(ctx, "tcp", "")
		m := measurement{tcp: milliseconds(time.Since(start))}
		if err != nil {
			return m, err
		}
		defer conn.Close()
		if deadline, ok := ctx.Deadline(); ok {
			_ = conn.SetDeadline(deadline)
		}
		if task.Mode == "tls" {
			cfg := p.tlsConfig(task, u, host)
			secure := tls.Client(conn, cfg)
			start = time.Now()
			if err := secure.HandshakeContext(ctx); err != nil {
				return m, err
			}
			m.tls = milliseconds(time.Since(start))
		}
		return m, nil
	}
	m := measurement{}
	var traceMu sync.Mutex
	var tlsStart, requestStart, firstByte time.Time
	snapshot := func() measurement { traceMu.Lock(); defer traceMu.Unlock(); return m }
	trace := &httptrace.ClientTrace{
		TLSHandshakeStart: func() { traceMu.Lock(); defer traceMu.Unlock(); tlsStart = time.Now() },
		TLSHandshakeDone: func(_ tls.ConnectionState, _ error) {
			traceMu.Lock()
			defer traceMu.Unlock()
			m.tls += milliseconds(time.Since(tlsStart))
		},
		WroteRequest: func(httptrace.WroteRequestInfo) {
			traceMu.Lock()
			defer traceMu.Unlock()
			if firstByte.IsZero() {
				requestStart = time.Now()
			}
		},
		GotFirstResponseByte: func() {
			traceMu.Lock()
			defer traceMu.Unlock()
			firstByte = time.Now()
			m.ttfb = milliseconds(firstByte.Sub(requestStart))
		},
	}
	// A custom dialer does not emit ConnectStart/Done. Measure TCP directly.
	measuredDial := func(ctx context.Context, network, address string) (net.Conn, error) {
		start := time.Now()
		conn, err := dial(ctx, network, address)
		traceMu.Lock()
		m.tcp = milliseconds(time.Since(start))
		traceMu.Unlock()
		return conn, err
	}
	transport := &http.Transport{Proxy: nil, DialContext: measuredDial, TLSClientConfig: p.tlsConfig(task, u, host),
		DisableKeepAlives: true, DisableCompression: true, ForceAttemptHTTP2: true, MaxResponseHeaderBytes: 64 << 10,
		ResponseHeaderTimeout: time.Duration(task.TimeoutMS) * time.Millisecond, TLSHandshakeTimeout: time.Duration(task.TimeoutMS) * time.Millisecond}
	defer transport.CloseIdleConnections()
	redirects := 3
	if task.MaxRedirects != nil {
		redirects = *task.MaxRedirects
	}
	if redirects > p.options.MaxRedirects {
		redirects = p.options.MaxRedirects
	}
	client := &http.Client{Transport: transport, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) > redirects {
			return fmt.Errorf("redirect limit exceeded")
		}
		if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
			return fmt.Errorf("unsafe redirect scheme")
		}
		if req.URL.User != nil {
			return fmt.Errorf("redirect credentials forbidden")
		}
		if _, err := resolvePublic(req.Context(), p.options.Resolver, req.URL.Hostname()); err != nil {
			return fmt.Errorf("unsafe redirect: %w", err)
		}
		redirectPort := 443
		if req.URL.Scheme == "http" {
			redirectPort = 80
		}
		if req.URL.Port() != "" {
			redirectPort, _ = strconv.Atoi(req.URL.Port())
		}
		if !allowedPort(p.options.AllowedPorts, redirectPort) {
			return fmt.Errorf("redirect port is not in node allowlist")
		}
		if via[0].URL.Scheme == "https" && req.URL.Scheme != "https" {
			return fmt.Errorf("HTTPS downgrade redirect forbidden")
		}
		// Keep the business identity and candidate route stable for all redirects.
		req.Host = via[0].Host
		return nil
	}}
	method := task.Method
	if method == "" {
		method = http.MethodGet
	}
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), method, u.String(), strings.NewReader(task.Body))
	if err != nil {
		return snapshot(), fmt.Errorf("invalid HTTP method or template")
	}
	req.Host = task.Host
	if req.Host == "" {
		req.Host = u.Host
	}
	for name, value := range task.Headers {
		req.Header.Set(name, value)
	}
	userAgent := task.UserAgent
	if userAgent == "" {
		userAgent = p.options.UserAgent
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", userAgent)
	}
	requestStart = time.Now()
	response, err := client.Do(req)
	if err != nil {
		return snapshot(), err
	}
	defer response.Body.Close()
	m.status = response.StatusCode
	expected := task.ExpectedStatuses
	if len(expected) == 0 {
		expected = []int{200}
	}
	matched := false
	for _, status := range expected {
		if response.StatusCode == status {
			matched = true
		}
	}
	if !matched {
		return snapshot(), fmt.Errorf("unexpected HTTP status %d", response.StatusCode)
	}
	// Read at most the configured download sample. Large benchmark payloads are
	// sampled rather than treated as failures; content matching uses this prefix.
	body, err := io.ReadAll(io.LimitReader(response.Body, task.MaxBytes))
	if err != nil {
		return snapshot(), err
	}
	m.bytes = int64(len(body))
	if task.ExpectedContains != "" && !strings.Contains(string(body), task.ExpectedContains) {
		return snapshot(), fmt.Errorf("expected content missing from response sample")
	}
	traceMu.Lock()
	downloadStart := firstByte
	traceMu.Unlock()
	if !downloadStart.IsZero() && m.bytes > 0 {
		elapsed := time.Since(downloadStart).Seconds()
		if elapsed > 0 {
			m.bps = float64(m.bytes) / elapsed
		}
	}
	return snapshot(), nil
}

func parseProbeURL(task Task) (*url.URL, error) {
	if task.URL == "" {
		if task.Mode == "http" || task.Mode == "all" {
			return nil, fmt.Errorf("HTTP probe requires a URL")
		}
		return nil, nil
	}
	u, err := url.Parse(task.URL)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
		return nil, fmt.Errorf("URL must be HTTP(S) without userinfo")
	}
	for _, identity := range []string{task.Host, task.SNI} {
		if strings.ContainsAny(identity, "\r\n\t/\\@ ") {
			return nil, fmt.Errorf("invalid Host or SNI override")
		}
	}
	return u, nil
}

func (p *Prober) tlsConfig(task Task, u *url.URL, targetHost string) *tls.Config {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if p.options.TLSConfig != nil {
		cfg = p.options.TLSConfig.Clone()
		cfg.MinVersion = tls.VersionTLS12
	}
	cfg.ServerName = task.SNI
	if cfg.ServerName == "" && u != nil {
		cfg.ServerName = u.Hostname()
	}
	if cfg.ServerName == "" {
		cfg.ServerName = targetHost
	}
	return cfg
}

func milliseconds(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
