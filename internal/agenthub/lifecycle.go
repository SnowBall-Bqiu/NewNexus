package agenthub

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/newnexus/newnexus/internal/auth"
	"github.com/newnexus/newnexus/internal/domain"
	"github.com/newnexus/newnexus/internal/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Node and token lifecycle changes share the same lock order: node, token,
// then related resources. A generation survives reconnects but changes when
// credentials are administratively revoked. It fences old streams even if a
// new enrollment finishes before the old gateway notices its revocation.
type sessionFence struct {
	generation int64
	owner      string
}
type sessionFenceKey struct{}

func fencedContext(ctx context.Context, generation int64, owner string) context.Context {
	return context.WithValue(ctx, sessionFenceKey{}, sessionFence{generation, owner})
}

func (h *Hub) lockDoc(ctx context.Context, tx *sql.Tx, kind, id string) (domain.Doc, error) {
	if h.Store.Driver == "sqlite" {
		if _, err := tx.ExecContext(ctx, `UPDATE resources SET version=version WHERE kind=? AND id=?`, kind, id); err != nil {
			return nil, err
		}
	}
	query := `SELECT doc,version FROM resources WHERE kind=? AND id=?`
	if h.Store.Driver != "sqlite" {
		query += ` FOR UPDATE`
	}
	var raw string
	var version int64
	if err := tx.QueryRowContext(ctx, h.Store.Q(query), kind, id).Scan(&raw, &version); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}
	d := domain.Doc{}
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		return nil, err
	}
	d["_version"] = version
	return d, nil
}

func (h *Hub) saveDoc(ctx context.Context, tx *sql.Tx, kind string, d domain.Doc) error {
	at := h.Store.Time(time.Now())
	if domain.S(d, "id") == "" {
		d["id"] = domain.ID()
	}
	if domain.S(d, "created_at") == "" {
		d["created_at"] = at
	}
	d["updated_at"] = at
	serial := domain.Clone(d)
	delete(serial, "_version")
	raw, err := json.Marshal(serial)
	if err != nil {
		return err
	}
	version := int64(domain.N(d, "_version"))
	if version == 0 {
		_, err = tx.ExecContext(ctx, h.Store.Q(`INSERT INTO resources(kind,id,doc,created_at,updated_at) VALUES(?,?,?,?,?)`), kind, d["id"], string(raw), d["created_at"], at)
		return err
	}
	r, err := tx.ExecContext(ctx, h.Store.Q(`UPDATE resources SET doc=?,version=version+1,updated_at=? WHERE kind=? AND id=? AND version=?`), string(raw), at, kind, d["id"], version)
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return store.ErrConflict
	}
	return nil
}

func clearNodeCredentials(node domain.Doc) {
	node["status"] = "revoked"
	node["credential_hash"] = ""
	node["session_generation"] = int64(domain.N(node, "session_generation")) + 1
	for _, key := range []string{"pending_credential_hash", "pending_credential_encrypted", "redeploy_token_hash"} {
		delete(node, key)
	}
}

// Redeploy preserves node identity, placement and historical counters. Only
// its credentials are replaced. The plaintext token is returned once.
func (h *Hub) Redeploy(ctx context.Context, id string, expires time.Duration) (domain.Doc, error) {
	if expires == 0 {
		expires = 15 * time.Minute
	}
	if expires < time.Minute || expires > 15*time.Minute {
		return nil, errors.New("redeployment token lifetime must be 60 to 900 seconds")
	}
	tx, err := h.Store.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	node, err := h.lockDoc(ctx, tx, "nodes", id)
	if err != nil {
		return nil, err
	}
	if old := domain.S(node, "redeploy_token_hash"); old != "" {
		if _, err = tx.ExecContext(ctx, h.Store.Q(`DELETE FROM resources WHERE kind='enrollment-tokens' AND id=?`), old); err != nil {
			return nil, err
		}
	}
	clearNodeCredentials(node)
	node["revoked_at"] = h.Store.Time(time.Now())
	token := auth.Token()
	hash := auth.Hash(token)
	node["redeploy_token_hash"] = hash
	expiry := time.Now().UTC().Add(expires).Format(time.RFC3339Nano)
	d := domain.Doc{"id": hash, "node_id": id, "name": node["name"], "expires_at": expiry, "used": false, "session_generation": node["session_generation"]}
	if err = h.saveDoc(ctx, tx, "nodes", node); err != nil {
		return nil, err
	}
	if err = h.saveDoc(ctx, tx, "enrollment-tokens", d); err != nil {
		return nil, err
	}
	if _, err = tx.ExecContext(ctx, h.Store.Q(`DELETE FROM job_leases WHERE id=?`), "node-connect:"+id); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	h.disconnect(id)
	return domain.Doc{"token": token, "expires_at": expiry, "node_id": id, "name": node["name"]}, nil
}

func (h *Hub) consumeEnrollment(ctx context.Context, in domain.Doc) (domain.Doc, error) {
	hash := auth.Hash(domain.S(in, "enrollment_token"))
	preview, err := h.Store.Get(ctx, "enrollment-tokens", hash)
	if err != nil {
		return nil, status.Error(codes.Unauthenticated, "invalid enrollment token")
	}
	tx, err := h.Store.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, status.Error(codes.Unavailable, "enrollment unavailable")
	}
	defer tx.Rollback()
	id := domain.S(preview, "node_id")
	var node domain.Doc
	if id != "" {
		node, err = h.lockDoc(ctx, tx, "nodes", id)
		if err != nil {
			return nil, status.Error(codes.Unauthenticated, "redeployment node unavailable")
		}
	}
	token, err := h.lockDoc(ctx, tx, "enrollment-tokens", hash)
	if err != nil || domain.B(token, "used") {
		return nil, status.Error(codes.Unauthenticated, "invalid enrollment token")
	}
	until, err := time.Parse(time.RFC3339Nano, domain.S(token, "expires_at"))
	if err != nil || !time.Now().Before(until) {
		return nil, status.Error(codes.Unauthenticated, "expired enrollment token")
	}
	if domain.S(token, "node_id") != id {
		return nil, status.Error(codes.Unauthenticated, "invalid enrollment token")
	}
	credential := auth.Token()
	if id == "" {
		name := domain.S(in, "name")
		if name == "" {
			name = domain.S(token, "name")
		}
		node = domain.Doc{"id": domain.ID(), "name": name, "region": in["region"], "carrier": in["carrier"], "version": in["version"], "protocols": "tcp,http", "protocol_version": 1, "tasks_completed": 0, "failure_rate": 0, "session_generation": 1}
	} else if domain.S(node, "redeploy_token_hash") != hash || domain.N(node, "session_generation") != domain.N(token, "session_generation") || domain.S(node, "status") != "revoked" {
		return nil, status.Error(codes.Unauthenticated, "superseded redeployment token")
	}
	node["credential_hash"] = auth.Hash(credential)
	node["status"] = "offline"
	delete(node, "redeploy_token_hash")
	delete(node, "revoked_at")
	token["used"] = true
	if err = h.saveDoc(ctx, tx, "nodes", node); err != nil {
		return nil, status.Error(codes.Unavailable, "node enrollment failed")
	}
	if err = h.saveDoc(ctx, tx, "enrollment-tokens", token); err != nil {
		return nil, status.Error(codes.Unavailable, "token consumption failed")
	}
	if err = tx.Commit(); err != nil {
		return nil, status.Error(codes.Unavailable, "node enrollment commit failed")
	}
	return domain.Doc{"node_id": node["id"], "credential": credential, "protocol_version": 1, "heartbeat_seconds": 15}, nil
}

// DeleteNode removes the live identity and outstanding work while retaining
// metrics, completed tasks and audit records for historical investigation.
func (h *Hub) DeleteNode(ctx context.Context, id string) error {
	tx, err := h.Store.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = h.lockDoc(ctx, tx, "nodes", id); err != nil {
		return err
	}
	// Read IDs first, then lock individual groups consistently. CAS writers
	// cannot restore a stale membership list after this transaction commits.
	rows, err := tx.QueryContext(ctx, `SELECT id FROM resources WHERE kind='node-groups' ORDER BY id`)
	if err != nil {
		return err
	}
	ids := []string{}
	for rows.Next() {
		var group string
		if err = rows.Scan(&group); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, group)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, group := range ids {
		d, e := h.lockDoc(ctx, tx, "node-groups", group)
		if errors.Is(e, store.ErrNotFound) {
			continue
		}
		if e != nil {
			return e
		}
		members, _ := d["node_ids"].([]any)
		next := []any{}
		changed := false
		for _, member := range members {
			if member == id {
				changed = true
			} else {
				next = append(next, member)
			}
		}
		if changed {
			d["node_ids"] = next
			if err = h.saveDoc(ctx, tx, "node-groups", d); err != nil {
				return err
			}
		}
	}
	field := `json_extract(doc,'$.node_id')`
	state := `json_extract(doc,'$.status')`
	if h.Store.Driver == "pgx" {
		field = `doc::jsonb->>'node_id'`
		state = `doc::jsonb->>'status'`
	} else if h.Store.Driver == "mysql" {
		field = mysqlScalar("doc", "node_id")
		state = mysqlScalar("doc", "status")
	}
	if _, err = tx.ExecContext(ctx, h.Store.Q(`DELETE FROM resources WHERE kind='tasks' AND `+field+`=? AND COALESCE(`+state+`,'')<>'done'`), id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, h.Store.Q(`DELETE FROM resources WHERE kind='enrollment-tokens' AND `+field+`=?`), id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, h.Store.Q(`DELETE FROM job_leases WHERE id=?`), "node-connect:"+id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, h.Store.Q(`DELETE FROM resources WHERE kind='nodes' AND id=?`), id); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	h.disconnect(id)
	return nil
}

func (h *Hub) disconnect(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if ch := h.streams[id]; ch != nil {
		select {
		case ch <- domain.Doc{"type": "revoked", "reason": "revoked by administrator"}:
		default:
		}
	}
}

// frameGuard is appended to each frame's atomic write. Without a context fence
// these helpers retain their administrative/test semantics; actual streams
// always carry both a generation and a live lease owner.
func (h *Hub) frameGuard(ctx context.Context, column, nodeID string) (string, []any) {
	fence, ok := ctx.Value(sessionFenceKey{}).(sessionFence)
	if !ok {
		return "", nil
	}
	generation := `json_extract(` + column + `,'$.session_generation')`
	if h.Store.Driver == "pgx" {
		generation = `(` + column + `::jsonb->>'session_generation')::bigint`
	} else if h.Store.Driver == "mysql" {
		generation = mysqlScalar(column, "session_generation")
	}
	return ` AND COALESCE(` + generation + `,0)=? AND EXISTS(SELECT 1 FROM job_leases l WHERE l.id=? AND l.owner=? AND l.expires_at>?)`, []any{fence.generation, "node-connect:" + nodeID, fence.owner, domain.Now()}
}

func (h *Hub) validateFrameTx(ctx context.Context, tx *sql.Tx, nodeID string, node domain.Doc) error {
	fence, ok := ctx.Value(sessionFenceKey{}).(sessionFence)
	if !ok {
		return nil
	}
	if int64(domain.N(node, "session_generation")) != fence.generation {
		return errFrameNodeRevoked
	}
	q := `SELECT owner,expires_at FROM job_leases WHERE id=?`
	if h.Store.Driver != "sqlite" {
		q += ` FOR UPDATE`
	}
	var owner, expiry string
	if err := tx.QueryRowContext(ctx, h.Store.Q(q), "node-connect:"+nodeID).Scan(&owner, &expiry); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errFrameNodeRevoked
		}
		return err
	}
	until, err := time.Parse(time.RFC3339Nano, expiry)
	if owner != fence.owner || err != nil || !until.After(time.Now()) {
		return errFrameNodeRevoked
	}
	return nil
}
