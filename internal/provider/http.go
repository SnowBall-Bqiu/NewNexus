package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type HTTPClient struct {
	Client      *http.Client
	MaxRetries  int
	MinInterval time.Duration
	limiter     *providerLimiter
}

type providerLimiter struct {
	mu          sync.Mutex
	nextRequest time.Time
}

func NewHTTPClient() HTTPClient {
	maxRetries := 3
	if value, err := strconv.Atoi(strings.TrimSpace(os.Getenv("NEWNEXUS_PROVIDER_MAX_RETRIES"))); err == nil && value >= 0 && value <= 8 {
		maxRetries = value
	}
	var minInterval time.Duration
	if value := strings.TrimSpace(os.Getenv("NEWNEXUS_PROVIDER_MIN_INTERVAL")); value != "" {
		if parsed, err := time.ParseDuration(value); err == nil && parsed >= 0 {
			minInterval = parsed
		}
	}
	return HTTPClient{Client: &http.Client{Timeout: 20 * time.Second}, MaxRetries: maxRetries, MinInterval: minInterval, limiter: &providerLimiter{}}
}

func (h HTTPClient) JSON(ctx context.Context, method, endpoint string, headers map[string]string, body any, result any) error {
	return h.json(ctx, method, endpoint, headers, body, result, retryableMethod(method) || hasIdempotencyKey(headers))
}

func (h HTTPClient) JSONWithRetryPolicy(ctx context.Context, method, endpoint string, headers map[string]string, body any, result any, safeToReplay bool) error {
	return h.json(ctx, method, endpoint, headers, body, result, safeToReplay || hasIdempotencyKey(headers))
}

func (h HTTPClient) json(ctx context.Context, method, endpoint string, headers map[string]string, body any, result any, canRetry bool) error {
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	client := h.Client
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	maxRetries := h.MaxRetries
	if maxRetries < 0 {
		maxRetries = 0
	}
	if !canRetry {
		maxRetries = 0
	}
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if err := h.waitForRateLimit(ctx); err != nil {
			return err
		}
		var reader io.Reader
		if payload != nil {
			reader = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
		if err != nil {
			return err
		}
		req.Header.Set("Accept", "application/json")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		for key, value := range headers {
			req.Header.Set(key, value)
		}
		response, err := client.Do(req)
		if err != nil {
			lastErr = err
			if attempt == maxRetries || !temporaryNetworkError(err) {
				return fmt.Errorf("provider request failed: %w", err)
			}
			if err := sleepContext(ctx, retryDelay(attempt, "")); err != nil {
				return err
			}
			continue
		}
		raw, readErr := io.ReadAll(io.LimitReader(response.Body, 4<<20))
		_ = response.Body.Close()
		if readErr != nil {
			return readErr
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			lastErr = fmt.Errorf("provider returned HTTP %d: %s", response.StatusCode, redactProviderError(raw, headers))
			if attempt < maxRetries && retryableStatus(response.StatusCode) {
				if err := sleepContext(ctx, retryDelay(attempt, response.Header.Get("Retry-After"))); err != nil {
					return err
				}
				continue
			}
			return lastErr
		}
		if result == nil || len(raw) == 0 {
			return nil
		}
		if err := json.Unmarshal(raw, result); err != nil {
			return fmt.Errorf("decode provider response: %w", err)
		}
		return nil
	}
	return lastErr
}

func (h HTTPClient) waitForRateLimit(ctx context.Context) error {
	if h.MinInterval <= 0 {
		return nil
	}
	limiter := h.limiter
	if limiter == nil {
		limiter = &providerLimiter{}
	}
	limiter.mu.Lock()
	now := time.Now()
	wait := limiter.nextRequest.Sub(now)
	if wait < 0 {
		wait = 0
	}
	limiter.nextRequest = now.Add(wait).Add(h.MinInterval)
	limiter.mu.Unlock()
	return sleepContext(ctx, wait)
}

func retryableMethod(method string) bool {
	switch strings.ToUpper(method) {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodPut, http.MethodDelete:
		return true
	default:
		return false
	}
}

func hasIdempotencyKey(headers map[string]string) bool {
	for key, value := range headers {
		if (strings.EqualFold(key, "Idempotency-Key") || strings.EqualFold(key, "X-NewNexus-Idempotency-Key")) && strings.TrimSpace(value) != "" {
			return true
		}
	}
	return false
}

func retryableStatus(status int) bool {
	switch status {
	case http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

func temporaryNetworkError(err error) bool {
	var networkError net.Error
	return err != nil && (strings.Contains(strings.ToLower(err.Error()), "connection reset") || strings.Contains(strings.ToLower(err.Error()), "connection refused") || (errors.As(err, &networkError) && (networkError.Timeout() || networkError.Temporary())))
}

func retryDelay(attempt int, retryAfter string) time.Duration {
	if seconds, err := strconv.Atoi(strings.TrimSpace(retryAfter)); err == nil && seconds >= 0 {
		return min(time.Duration(seconds)*time.Second, 30*time.Second)
	}
	if when, err := http.ParseTime(retryAfter); err == nil {
		return min(max(time.Until(when), 0), 30*time.Second)
	}
	return time.Duration(math.Min(math.Pow(2, float64(attempt))*250, 5000)) * time.Millisecond
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func redactProviderError(raw []byte, headers map[string]string) string {
	text := strings.TrimSpace(string(raw))
	for key, value := range headers {
		lower := strings.ToLower(key)
		if value != "" && (strings.Contains(lower, "authorization") || strings.Contains(lower, "token") || strings.Contains(lower, "secret") || strings.Contains(lower, "key")) {
			text = strings.ReplaceAll(text, value, "[REDACTED]")
		}
	}
	var decoded any
	if json.Unmarshal([]byte(text), &decoded) == nil {
		redactProviderValue(decoded)
		if encoded, err := json.Marshal(decoded); err == nil {
			text = string(encoded)
		}
	}
	if len(text) > 1024 {
		text = text[:1024] + "..."
	}
	if text == "" {
		return "request rejected"
	}
	return text
}

func redactProviderValue(value any) {
	switch typed := value.(type) {
	case map[string]any:
		for key, item := range typed {
			lower := strings.ToLower(key)
			if strings.Contains(lower, "token") || strings.Contains(lower, "secret") || strings.Contains(lower, "password") || strings.Contains(lower, "authorization") || strings.Contains(lower, "api_key") || strings.Contains(lower, "apikey") {
				typed[key] = "[REDACTED]"
				continue
			}
			redactProviderValue(item)
		}
	case []any:
		for _, item := range typed {
			redactProviderValue(item)
		}
	}
}
