package agentproto

import (
	"crypto/sha256"
	"encoding/hex"
)

// ResultID is the protocol-v1 identity of exactly one node/task observation.
// Both gateways and nodes use this binding, so a result cannot be reassigned to
// another task or counted twice by changing only its caller-supplied ID.
func ResultID(nodeID, taskID string) string {
	sum := sha256.Sum256([]byte(nodeID + ":" + taskID))
	return hex.EncodeToString(sum[:])
}
