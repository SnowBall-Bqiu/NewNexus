package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/newnexus/newnexus/internal/domain"
)

// Error contains codes only: provider responses may contain submitted secrets.
type Error struct {
	Status    int
	Code      string
	Retryable bool
	Ambiguous bool
}

func (e *Error) Error() string {
	return fmt.Sprintf("provider operation failed (HTTP %d, code %s)", e.Status, e.Code)
}
func retryWait(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
func (a *Adapter) doJSON(request *http.Request, envelope string, readOnly bool) (domain.Doc, error) {
	client := a.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	for attempt := 0; attempt < 4; attempt++ {
		req := request.Clone(request.Context())
		if request.GetBody != nil {
			body, err := request.GetBody()
			if err != nil {
				return nil, err
			}
			req.Body = body
		}
		resp, err := client.Do(req)
		if err != nil {
			if req.Context().Err() != nil {
				return nil, req.Context().Err()
			}
			// Read-only calls can safely retry a transient disconnect/dial
			// failure. A mutation can have reached the provider and remains
			// ambiguous, so it must be reconciled before any retry.
			if readOnly && attempt < 3 {
				if e := retryWait(req.Context(), time.Duration(1<<attempt)*time.Second); e != nil {
					return nil, e
				}
				continue
			}
			return nil, &Error{Code: "transport_error", Retryable: readOnly, Ambiguous: !readOnly}
		}
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, (8<<20)+1))
		resp.Body.Close()
		if readErr != nil || len(raw) > 8<<20 {
			return nil, &Error{Status: resp.StatusCode, Code: "invalid_response", Ambiguous: !readOnly}
		}
		result := domain.Doc{}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		decodeErr := decoder.Decode(&result)
		if envelope != "" && envelope != "cloudflare" {
			result = domain.M(result, envelope)
		}
		code := ""
		if envelope == "cloudflare" {
			if !domain.B(result, "success") {
				items := domain.A(result, "errors")
				if len(items) > 0 {
					code = domain.S(items[0], "code")
				}
				if code == "" {
					code = "cloudflare_rejected"
				}
			}
		} else {
			code = domain.S(domain.M(result, "Error"), "Code")
			if code == "" {
				code = domain.S(result, "Code")
			}
		}
		if len(code) > 128 {
			code = "provider_rejected"
		}
		for _, r := range code {
			if r != '.' && r != '_' && r != '-' && (r < '0' || r > '9') && (r < 'A' || r > 'Z') && (r < 'a' || r > 'z') {
				code = "provider_rejected"
				break
			}
		}
		limited := resp.StatusCode == 429 || strings.HasPrefix(code, "RequestLimitExceeded") || strings.HasPrefix(code, "Throttling")
		retryable := limited || readOnly && resp.StatusCode >= 500
		if retryable && attempt < 3 {
			delay := time.Duration(1<<attempt) * time.Second
			if seconds, e := strconv.Atoi(resp.Header.Get("Retry-After")); e == nil && seconds >= 0 && seconds <= 60 {
				delay = time.Duration(seconds) * time.Second
			} else if until, e := http.ParseTime(resp.Header.Get("Retry-After")); e == nil {
				delay = time.Until(until)
				if delay < 0 {
					delay = 0
				}
				if delay > time.Minute {
					delay = time.Minute
				}
			}
			if err = retryWait(req.Context(), delay); err != nil {
				return nil, err
			}
			continue
		}
		if decodeErr != nil {
			return nil, &Error{Status: resp.StatusCode, Code: "malformed_json", Ambiguous: !readOnly}
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 || code != "" {
			if code == "" {
				code = "http_error"
			}
			return nil, &Error{Status: resp.StatusCode, Code: code, Retryable: retryable, Ambiguous: !readOnly && resp.StatusCode >= 500}
		}
		if result == nil {
			return nil, errors.New("provider returned an empty response")
		}
		return result, nil
	}
	return nil, errors.New("provider retry budget exhausted")
}
