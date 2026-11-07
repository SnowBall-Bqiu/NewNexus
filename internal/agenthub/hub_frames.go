package agenthub

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/newnexus/newnexus/internal/domain"
)

var errFrameNodeRevoked = errors.New("node revoked or unknown")

// mysqlScalar returns SQL NULL for absent keys and JSON null. Column and key
// names here are internal constants, never user-controlled SQL identifiers.
func mysqlScalar(column, key string) string {
	return `NULLIF(JSON_UNQUOTE(JSON_EXTRACT(` + column + `,'$.` + key + `')),'null')`
}

// acknowledgeTasks only advances queued tasks owned by this node. Repeated
// ACKs neither resurrect done tasks nor extend the accepted-task retry window.
func (h *Hub) acknowledgeTasks(ctx context.Context, nodeID string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	if len(ids) > 256 {
		return errors.New("task acknowledgment batch exceeds 256")
	}
	unique := map[string]bool{}
	for _, id := range ids {
		if id != "" && len(id) <= 512 {
			unique[id] = true
		}
	}
	ids = sortedKeys(unique)
	if len(ids) == 0 {
		return nil
	}
	at := h.Store.Time(time.Now())
	patch, _ := json.Marshal(domain.Doc{"status": "accepted", "accepted_at": at, "updated_at": at})
	merge := `json_patch(doc,?)`
	nodeField, statusField, revokedField := `json_extract(doc,'$.node_id')`, `json_extract(doc,'$.status')`, `json_extract(n.doc,'$.status')`
	if h.Store.Driver == "pgx" {
		merge = `(doc::jsonb || ?::jsonb)::text`
		nodeField, statusField, revokedField = `doc::jsonb->>'node_id'`, `doc::jsonb->>'status'`, `n.doc::jsonb->>'status'`
	} else if h.Store.Driver == "mysql" {
		// A self-join avoids MySQL's target-table subquery restriction while
		// retaining the live-node guard. Drive requested task IDs as unique
		// primary-key points; an IN range scan can lock unrelated queued tasks.
		requested, args := mysqlRequestedResources(ids)
		query := `UPDATE resources n FORCE INDEX(PRIMARY) STRAIGHT_JOIN ` + requested + ` ON TRUE STRAIGHT_JOIN resources target FORCE INDEX(PRIMARY) ON target.kind='tasks' AND target.id=CAST(requested.id AS CHAR CHARACTER SET ascii) COLLATE ascii_bin SET target.doc=JSON_MERGE_PATCH(target.doc,CAST(? AS JSON)),target.version=target.version+1,target.updated_at=? WHERE n.kind='nodes' AND n.id=? AND ` + mysqlScalar("target.doc", "node_id") + `=? AND ` + mysqlScalar("target.doc", "status") + `='queued' AND COALESCE(` + mysqlScalar("n.doc", "status") + `,'')<>'revoked'`
		args = append(args, string(patch), at, nodeID, nodeID)
		guard, fenceArgs := h.frameGuard(ctx, "n.doc", nodeID)
		query += guard
		args = append(args, fenceArgs...)
		_, err := h.Store.DB.ExecContext(ctx, query, args...)
		return err
	}
	query, idArgs := inQuery(`UPDATE resources SET doc=`+merge+`,version=version+1,updated_at=? WHERE kind='tasks' AND `+nodeField+`=? AND `+statusField+`='queued' AND id IN (`, ids)
	args := []any{string(patch), at, nodeID}
	args = append(args, idArgs...)
	guard, fenceArgs := h.frameGuard(ctx, "n.doc", nodeID)
	query += ` AND EXISTS(SELECT 1 FROM resources n WHERE n.kind='nodes' AND n.id=? AND COALESCE(` + revokedField + `,'')<>'revoked'` + guard + `)`
	args = append(args, nodeID)
	args = append(args, fenceArgs...)
	_, err := h.Store.DB.ExecContext(ctx, h.Store.Q(query), args...)
	return err
}

// patchFrameNode merges only telemetry fields under the current row lock. An
// administrator's settings, credentials and ingestion counters stay intact.
func (h *Hub) patchFrameNode(ctx context.Context, nodeID string, patch domain.Doc) error {
	return h.patchFrameNodeForLease(ctx, nodeID, "", patch)
}

func (h *Hub) patchFrameNodeForLease(ctx context.Context, nodeID, owner string, patch domain.Doc) error {
	at := h.Store.Time(time.Now())
	patch["updated_at"] = at
	raw, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	merge, status := `json_patch(doc,?)`, `json_extract(doc,'$.status')`
	if h.Store.Driver == "pgx" {
		merge, status = `(doc::jsonb || ?::jsonb)::text`, `doc::jsonb->>'status'`
	} else if h.Store.Driver == "mysql" {
		merge, status = `JSON_MERGE_PATCH(doc,CAST(? AS JSON))`, mysqlScalar("doc", "status")
	}
	query := `UPDATE resources SET doc=` + merge + `,version=version+1,updated_at=? WHERE kind='nodes' AND id=? AND COALESCE(` + status + `,'')<>'revoked'`
	args := []any{string(raw), at, nodeID}
	guard, fenceArgs := h.frameGuard(ctx, "doc", nodeID)
	query += guard
	args = append(args, fenceArgs...)
	if owner != "" {
		// A delayed disconnect from an expired gateway must not mark the node
		// offline after another gateway has acquired its lease and reconnected.
		query += ` AND EXISTS(SELECT 1 FROM job_leases WHERE id=? AND owner=?)`
		args = append(args, "node-connect:"+nodeID, owner)
	}
	result, err := h.Store.DB.ExecContext(ctx, h.Store.Q(query), args...)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 && owner == "" {
		return errFrameNodeRevoked
	}
	return nil
}

func (h *Hub) heartbeatFrame(ctx context.Context, nodeID string, frame domain.Doc) error {
	capabilities := []any{}
	seen := map[string]bool{}
	if values, ok := frame["capabilities"].([]any); ok {
		for _, value := range values {
			name, ok := value.(string)
			if ok && !seen[name] && (name == "tcp" || name == "tls" || name == "http" || name == "download") {
				capabilities = append(capabilities, name)
				seen[name] = true
			}
		}
	}
	version := domain.S(frame, "version")
	if len(version) > 128 {
		version = version[:128]
	}
	patch := domain.Doc{"status": "online", "last_seen_at": h.Store.Time(time.Now()), "version": version, "capabilities": capabilities}
	for _, name := range []string{"pending_tasks", "outbox_size", "outbox_bytes"} {
		if _, exists := frame[name]; exists {
			patch[name] = max(0., min(1e12, domain.N(frame, name)))
		}
	}
	if frame["pending_tasks"] != nil && frame["outbox_size"] != nil && domain.N(frame, "pending_tasks") == 0 && domain.N(frame, "outbox_size") == 0 {
		patch["backpressure"] = false
	}
	for _, name := range []string{"os", "architecture"} {
		value := domain.S(frame, name)
		if value != "" && len(value) <= 32 {
			patch[name] = value
		}
	}
	if mode := domain.S(frame, "mode"); mode == "all" || mode == "tcp" || mode == "http" {
		patch["agent_mode"] = mode
	}
	if value := domain.N(frame, "concurrency"); value >= 1 && value <= 128 {
		patch["agent_concurrency"] = value
	}
	return h.patchFrameNode(ctx, nodeID, patch)
}

func (h *Hub) credentialAckFrame(ctx context.Context, nodeID, credentialID string) error {
	if credentialID == "" || len(credentialID) > 512 {
		return nil
	}
	at := h.Store.Time(time.Now())
	merge := `json_set(json_remove(doc,'$.pending_credential_hash','$.pending_credential_encrypted'),'$.credential_hash',json_extract(doc,'$.pending_credential_hash'),'$.updated_at',?)`
	pending, state := `json_extract(doc,'$.pending_credential_hash')`, `json_extract(doc,'$.status')`
	if h.Store.Driver == "pgx" {
		merge = `((doc::jsonb - 'pending_credential_hash' - 'pending_credential_encrypted') || jsonb_build_object('credential_hash',doc::jsonb->>'pending_credential_hash','updated_at',?::text))::text`
		pending, state = `doc::jsonb->>'pending_credential_hash'`, `doc::jsonb->>'status'`
	} else if h.Store.Driver == "mysql" {
		merge = `JSON_SET(JSON_REMOVE(doc,'$.pending_credential_hash','$.pending_credential_encrypted'),'$.credential_hash',JSON_UNQUOTE(JSON_EXTRACT(doc,'$.pending_credential_hash')),'$.updated_at',?)`
		pending, state = mysqlScalar("doc", "pending_credential_hash"), mysqlScalar("doc", "status")
	}
	guard, fenceArgs := h.frameGuard(ctx, "doc", nodeID)
	args := []any{at, at, nodeID, credentialID}
	args = append(args, fenceArgs...)
	_, err := h.Store.DB.ExecContext(ctx, h.Store.Q(`UPDATE resources SET doc=`+merge+`,version=version+1,updated_at=? WHERE kind='nodes' AND id=? AND `+pending+`=? AND COALESCE(`+state+`,'')<>'revoked'`+guard), args...)
	return err
}

func (h *Hub) backpressureFrame(ctx context.Context, nodeID string, frame domain.Doc) error {
	patch := domain.Doc{"backpressure": true, "backpressure_at": h.Store.Time(time.Now()), "backpressure_retry_seconds": max(1., min(300., domain.N(frame, "retry_after_seconds")))}
	for _, name := range []string{"pending_tasks", "outbox_size"} {
		if _, exists := frame[name]; exists {
			patch[name] = max(0., min(1e9, domain.N(frame, name)))
		}
	}
	if reason := strings.TrimSpace(domain.S(frame, "reason")); reason != "" && len(reason) <= 256 {
		patch["backpressure_reason"] = reason
	}
	return h.patchFrameNode(ctx, nodeID, patch)
}

func (h *Hub) revokeNode(ctx context.Context, nodeID string) error {
	tx, err := h.Store.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	node, err := h.lockDoc(ctx, tx, "nodes", nodeID)
	if err != nil {
		return err
	}
	if old := domain.S(node, "redeploy_token_hash"); old != "" {
		if _, err = tx.ExecContext(ctx, h.Store.Q(`DELETE FROM resources WHERE kind='enrollment-tokens' AND id=?`), old); err != nil {
			return err
		}
	}
	clearNodeCredentials(node)
	node["revoked_at"] = h.Store.Time(time.Now())
	if err = h.saveDoc(ctx, tx, "nodes", node); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, h.Store.Q(`DELETE FROM job_leases WHERE id=?`), "node-connect:"+nodeID); err != nil {
		return err
	}
	return tx.Commit()
}

func (h *Hub) pendingRotation(ctx context.Context, nodeID, hash, sealed string, locallyConnected bool) error {
	at := h.Store.Time(time.Now())
	patch, _ := json.Marshal(domain.Doc{"pending_credential_hash": hash, "pending_credential_encrypted": sealed, "updated_at": at})
	merge := `json_patch(doc,?)`
	pending, state := `json_extract(doc,'$.pending_credential_hash')`, `json_extract(doc,'$.status')`
	if h.Store.Driver == "pgx" {
		merge = `(doc::jsonb || ?::jsonb)::text`
		pending, state = `doc::jsonb->>'pending_credential_hash'`, `doc::jsonb->>'status'`
	} else if h.Store.Driver == "mysql" {
		merge = `JSON_MERGE_PATCH(doc,CAST(? AS JSON))`
		pending, state = mysqlScalar("doc", "pending_credential_hash"), mysqlScalar("doc", "status")
	}
	live := 0
	if locallyConnected {
		live = 1
	}
	// One conditional row update serializes with result counters, concurrent
	// rotations, credential ACKs and revocation. It never replays a stale full
	// node document and cannot add pending credentials after revoke completes.
	result, err := h.Store.DB.ExecContext(ctx, h.Store.Q(`UPDATE resources SET doc=`+merge+`,version=version+1,updated_at=? WHERE kind='nodes' AND id=? AND COALESCE(`+state+`,'')<>'revoked' AND COALESCE(`+pending+`,'')='' AND (`+state+`='online' OR ?=1)`), string(patch), at, nodeID, live)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil || count == 1 {
		return err
	}
	node, err := h.Store.Get(ctx, "nodes", nodeID)
	if err != nil {
		return err
	}
	if domain.S(node, "status") == "revoked" {
		return errFrameNodeRevoked
	}
	if domain.S(node, "pending_credential_hash") != "" {
		return errors.New("a credential rotation is already awaiting acknowledgement")
	}
	return errors.New("node must be connected to rotate its credential")
}
