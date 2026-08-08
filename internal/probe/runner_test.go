package probe

import "testing"

func TestValidateDestinationBlocksInternalNetworks(t *testing.T) {
	blocked := []string{"127.0.0.1", "10.0.0.1", "169.254.169.254", "::1", "fc00::1", "fe80::1"}
	for _, address := range blocked {
		if err := validateDestination(address); err == nil {
			t.Errorf("blocked destination %s was accepted", address)
		}
	}
}

func TestValidateDestinationAllowsPublicAddress(t *testing.T) {
	if err := validateDestination("1.1.1.1"); err != nil {
		t.Fatalf("public destination was rejected: %v", err)
	}
}

func TestClassifyError(t *testing.T) {
	tests := map[string]string{"blocked destination": "blocked_destination", "context deadline exceeded": "timeout", "unexpected HTTP status 500": "http_status", "response assertion failed": "assertion_failed"}
	for message, expected := range tests {
		if actual := classifyError(assertionError(message)); actual != expected {
			t.Errorf("%q: expected %s, got %s", message, expected, actual)
		}
	}
}

type assertionError string

func (e assertionError) Error() string { return string(e) }
