package api

import (
	"errors"
	"fmt"
	"github.com/newnexus/newnexus/internal/agenttls"
	"github.com/newnexus/newnexus/internal/domain"
	"github.com/newnexus/newnexus/internal/security"
	"math"
	"net/mail"
	"strings"
)

var writable = map[string]string{
	"projects":            "name description status color",
	"sites":               "project_id name url probe_profile_id node_group description",
	"connections":         "name provider purposes credentials",
	"sources":             "project_id name type location enabled",
	"candidates":          "project_id source_id address tier tags",
	"nodes":               "name group_name region carrier mode",
	"node-groups":         "name description node_ids",
	"probe-profiles":      "project_id name scope mode url host sni path method headers expected_statuses expected_contains timeout_ms max_bytes interval_seconds user_agent retries samples max_redirects",
	"rules":               "project_id site_id name hostname dns_connection_id zone_id zone_name ttl publish_mode switch_mode probe_profile_id lines",
	"saas/zones":          "project_id connection_id zone_id zone_name fallback_origin cname_target",
	"saas/hostnames":      "saas_zone_id hostname validation_method automation_mode custom_origin dns_connection_id dns_zone_id dns_zone_name cname_target",
	"optimized-hostnames": "project_id hostname dns_connection_id zone_id zone_name family top_n ttl",
}

func fields(input domain.Doc, allowed string) domain.Doc {
	out := domain.Doc{}
	for _, key := range strings.Fields(allowed) {
		if value, ok := input[key]; ok {
			out[key] = value
		}
	}
	return out
}
func cleanInput(kind string, input, previous domain.Doc) domain.Doc {
	out := fields(input, writable[kind])
	if kind != "rules" || out["lines"] == nil {
		return out
	}
	oldLines := map[string]domain.Doc{}
	for _, line := range domain.A(previous, "lines") {
		oldLines[domain.S(line, "id")] = line
	}
	lines := []domain.Doc{}
	for _, line := range domain.A(out, "lines") {
		old := oldLines[domain.S(line, "id")]
		clean := domain.Clone(old)
		if clean == nil {
			clean = domain.Doc{}
		}
		if old == nil {
			clean["id"] = domain.ID()
		}
		for k, v := range fields(line, "provider_line_code alias node_group health_perspective min_nodes quorum_percent") {
			clean[k] = v
		}
		oldTargets := map[string]domain.Doc{}
		for _, t := range domain.A(old, "targets") {
			oldTargets[domain.S(t, "id")] = t
		}
		targets := []domain.Doc{}
		for _, t := range domain.A(line, "targets") {
			prior := oldTargets[domain.S(t, "id")]
			target := domain.Clone(prior)
			if target == nil {
				target = domain.Doc{}
			}
			if prior == nil {
				target["id"] = domain.ID()
			}
			for k, v := range fields(t, "name cname priority weight enabled protocol port headers") {
				target[k] = v
			}
			targets = append(targets, target)
		}
		clean["targets"] = targets
		lines = append(lines, clean)
	}
	out["lines"] = lines
	return out
}

var settingRanges = map[string][2]float64{
	"probe.samples": {1, 10}, "probe.max_redirects": {0, 5},
	"system.disk_warning_percent": {1, 100}, "system.disk_critical_percent": {1, 100},
	"probe.discovery_interval_seconds": {60, 604800}, "probe.ranked_interval_seconds": {10, 86400}, "probe.active_interval_seconds": {5, 3600}, "probe.concurrency": {1, 1000}, "probe.timeout_ms": {100, 60000}, "probe.retries": {0, 3}, "probe.min_samples": {1, 1000}, "probe.freshness_seconds": {30, 604800}, "probe.minimum_success_rate": {0, 1}, "probe.max_bytes": {1, 16 << 20},
	"routing.min_nodes": {2, 1000}, "routing.quorum_percent": {66.66666666, 100}, "routing.failure_rounds": {1, 1000}, "routing.recovery_rounds": {1, 1000}, "routing.performance_rounds": {1, 1000}, "routing.improvement_percent": {1, 1000}, "routing.cooldown_seconds": {0, 604800}, "publish.large_change_threshold": {1, 10000}, "retention.raw_days": {1, 3650}, "retention.five_minute_days": {1, 3650}, "retention.hourly_days": {1, 3650}, "alerts.smtp_port": {1, 65535}}

func validateSetting(key string, value any) error {
	d := domain.Doc{"value": value}
	if bounds, ok := settingRanges[key]; ok {
		switch value.(type) {
		case float64, int, int64:
		default:
			return fmt.Errorf("%s must be numeric", key)
		}
		n := domain.N(d, "value")
		if math.IsNaN(n) || math.IsInf(n, 0) || n < bounds[0] || n > bounds[1] {
			return fmt.Errorf("%s must be between %g and %g", key, bounds[0], bounds[1])
		}
		if key != "probe.minimum_success_rate" && key != "routing.quorum_percent" && n != math.Trunc(n) {
			return fmt.Errorf("%s must be an integer", key)
		}
		return nil
	}
	switch key {
	case "addresses.api_origin", "addresses.agent_endpoint", "addresses.console_origin":
		_, err := normalizeAddressSetting(key, value)
		return err
	case "probe.host", "probe.sni", "probe.expected_contains":
		value, ok := value.(string)
		if !ok || len(value) > 4096 || strings.ContainsAny(value, "\r\n") {
			return errors.New("probe string must be at most 4096 bytes and contain no line breaks")
		}
	case "probe.path":
		path, ok := value.(string)
		if !ok || len(path) > 4096 || path != "" && !strings.HasPrefix(path, "/") || strings.ContainsAny(path, "\r\n?#") {
			return errors.New("probe path must be empty or an absolute path without query/fragment")
		}
	case "probe.method":
		method, ok := value.(string)
		if !ok || !map[string]bool{"GET": true, "HEAD": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true, "OPTIONS": true}[method] {
			return errors.New("unsupported probe HTTP method")
		}
	case "probe.headers":
		headers, ok := value.(map[string]any)
		if !ok {
			if d, valid := value.(domain.Doc); valid {
				headers = d
				ok = true
			}
		}
		if !ok || len(headers) > 100 {
			return errors.New("probe headers must be an object of at most 100 string values")
		}
		for name, value := range headers {
			raw, isString := value.(string)
			if !isString || len(raw) > 8192 || strings.ContainsAny(raw, "\r\n") || name == "" {
				return errors.New("invalid probe HTTP header")
			}
			for _, r := range name {
				if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", r)) {
					return errors.New("invalid probe HTTP header name")
				}
			}
		}
	case "probe.expected_statuses":
		statuses, ok := value.([]any)
		if !ok || len(statuses) < 1 || len(statuses) > 100 {
			return errors.New("expected statuses must be an array of 1 to 100 HTTP status codes")
		}
		for _, value := range statuses {
			n := domain.N(domain.Doc{"n": value}, "n")
			switch value.(type) {
			case float64, int, int64:
			default:
				return errors.New("expected status must be an integer")
			}
			if n < 200 || n > 599 || n != math.Trunc(n) {
				return errors.New("expected status must be an integer between 200 and 599")
			}
		}
	case "auth.require_totp", agenttls.SettingKey:
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("%s must be boolean", key)
		}
	case "score.weights":
		weights := domain.M(d, "value")
		if len(weights) == 0 {
			return errors.New("score.weights must be a nonempty object")
		}
		sum := 0.
		for k, v := range weights {
			switch v.(type) {
			case float64, int, int64:
			default:
				return errors.New("score weights must be numeric")
			}
			if !map[string]bool{"tcp": true, "tls": true, "ttfb": true, "p50": true, "p95": true, "download": true, "failure": true}[k] {
				return fmt.Errorf("unknown score weight %s", k)
			}
			n := domain.N(domain.Doc{"n": v}, "n")
			if n < 0 || n > 100 || math.IsNaN(n) || math.IsInf(n, 0) {
				return errors.New("score weights must be finite numbers between zero and 100")
			}
			sum += n
		}
		if sum == 0 {
			return errors.New("at least one score weight must be positive")
		}
	case "probe.default_url", "alerts.webhook_url":
		raw, ok := value.(string)
		if !ok {
			return errors.New("URL must be a string")
		}
		if raw != "" {
			u, e := security.ValidateURL(raw)
			if e != nil {
				return e
			}
			if key == "alerts.webhook_url" && u.Scheme != "https" {
				return errors.New("webhook URL must use HTTPS")
			}
		}
	case "probe.default_user_agent", "alerts.smtp_username", "alerts.smtp_password", "alerts.webhook_secret", "alerts.smtp_host", "alerts.email_from", "alerts.email_to":
		raw, ok := value.(string)
		if !ok || len(raw) > 4096 || strings.ContainsAny(raw, "\r\n") {
			return fmt.Errorf("invalid value for %s", key)
		}
		if key == "alerts.webhook_secret" && raw != "" && len(raw) < 16 {
			return errors.New("webhook HMAC secret must be at least 16 characters")
		}
		if (key == "alerts.email_from" || key == "alerts.email_to") && raw != "" {
			if _, e := mail.ParseAddress(raw); e != nil {
				return errors.New("invalid email address")
			}
		}
	default:
		return fmt.Errorf("unknown setting %s", key)
	}
	return nil
}
