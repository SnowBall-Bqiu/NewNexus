package main

import (
	"strings"
	"testing"
)

func TestCLIVersionAndHealthcheck(t *testing.T) {
	if err := run([]string{"--version"}); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{"--healthcheck", "--data-dir", t.TempDir()}); err == nil {
		t.Fatal("missing health state accepted")
	}
	if err := run([]string{"unexpected"}); err == nil {
		t.Fatal("positional argument accepted")
	}
}

func TestCLIEnvironmentRejectsInvalidLimits(t *testing.T) {
	for _, env := range []string{"NEWNEXUS_AGENT_CONCURRENCY", "NEWNEXUS_AGENT_MAX_PENDING", "NEWNEXUS_AGENT_RATE", "NEWNEXUS_AGENT_MAX_TIMEOUT", "NEWNEXUS_AGENT_INSECURE_LOOPBACK"} {
		t.Run(env, func(t *testing.T) {
			t.Setenv(env, "invalid")
			if err := run([]string{"--version"}); err == nil || !strings.Contains(err.Error(), env) {
				t.Fatalf("invalid environment not identified: %v", err)
			}
		})
	}
	if err := run([]string{"--max-timeout", "61s"}); err == nil {
		t.Fatal("unbounded timeout accepted")
	}
	if err := run([]string{"--allowed-ports", "443,0"}); err == nil {
		t.Fatal("invalid port accepted")
	}
	if err := run([]string{"--server", "example.com:9090", "--rate", "NaN"}); err == nil {
		t.Fatal("NaN rate accepted")
	}
}
