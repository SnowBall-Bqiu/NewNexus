package api

import "testing"

func TestValidateSetting(t *testing.T) {
	valid := map[string]string{
		"retention.raw_days":       "7",
		"probe.discovery_interval": "6h",
		"probe.ranking_interval":   "5m",
		"probe.active_interval":    "30s",
		"alerts.webhook_url":       "https://hooks.example.com/newnexus",
		"alerts.smtp_host":         "smtp.example.com:587",
		"alerts.smtp_from":         "alerts@example.com",
		"alerts.smtp_to":           "operator@example.com",
		"alerts.smtp_password":     "<masked>",
	}
	for key, value := range valid {
		if err := validateSetting(key, value); err != nil {
			t.Fatalf("expected %s=%q to be valid: %v", key, value, err)
		}
	}
	invalid := map[string]string{
		"unknown.setting":          "value",
		"retention.raw_days":       "0",
		"probe.discovery_interval": "1s",
		"probe.active_interval":    "0s",
		"alerts.webhook_url":       "http://127.0.0.1/hook",
		"alerts.smtp_host":         "smtp.example.com",
		"alerts.smtp_from":         "NewNexus <alerts@example.com>",
		"alerts.smtp_to":           "operator@example.com\r\nBcc: attacker@example.com",
	}
	for key, value := range invalid {
		if err := validateSetting(key, value); err == nil {
			t.Fatalf("expected %s=%q to be rejected", key, value)
		}
	}
}
