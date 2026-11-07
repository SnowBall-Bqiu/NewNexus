package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/newnexus/newnexus/internal/domain"
)

var ErrLeaseLost = errors.New("lease expired or owned by another worker")

// PutUnderLease fences a CAS checkpoint against lease takeover. The lease row
// stays locked until the checkpoint commits; checking ownership before an
// independent Put would leave a race with a new leader acquiring the lease.
func (s *Store) PutUnderLease(ctx context.Context, leaseID, owner, kind string, d domain.Doc) error {
	return s.putUnderLease(ctx, leaseID, owner, kind, d, false)
}

// PutUnderSharedLease permits concurrent checkpoints for distinct resources
// while retaining a shared lock against renewal or leadership takeover.
func (s *Store) PutUnderSharedLease(ctx context.Context, leaseID, owner, kind string, d domain.Doc) error {
	return s.putUnderLease(ctx, leaseID, owner, kind, d, true)
}

func (s *Store) putUnderLease(ctx context.Context, leaseID, owner, kind string, d domain.Doc, shared bool) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	query := "SELECT owner,expires_at FROM job_leases WHERE id=?"
	clock := "SELECT strftime('%Y-%m-%dT%H:%M:%fZ','now')"
	if s.Driver == "sqlite" {
		// SQLite has no row-level FOR UPDATE. Obtain its writer reservation
		// before reading the lease, preventing another writer from taking it.
		if _, err = tx.ExecContext(ctx, "UPDATE job_leases SET owner=owner WHERE id=?", leaseID); err != nil {
			return err
		}
	} else {
		if shared {
			query += " FOR SHARE"
		} else {
			query += " FOR UPDATE"
		}
		if s.Driver == "mysql" {
			clock = "SELECT DATE_FORMAT(UTC_TIMESTAMP(6),'%Y-%m-%dT%H:%i:%s.%fZ')"
		} else {
			clock = `SELECT to_char(clock_timestamp() AT TIME ZONE 'UTC','YYYY-MM-DD"T"HH24:MI:SS.US"Z"')`
		}
	}
	var held, expiry string
	if err = tx.QueryRowContext(ctx, s.Q(query), leaseID).Scan(&held, &expiry); errors.Is(err, sql.ErrNoRows) {
		return ErrLeaseLost
	} else if err != nil {
		return err
	}
	if held != owner || owner == "" {
		return ErrLeaseLost
	}
	// Query the database clock after the lock has been acquired. MySQL's
	// UTC_TIMESTAMP is fixed at statement start, which can predate a lock wait.
	var timestamp string
	if err = tx.QueryRowContext(ctx, clock).Scan(&timestamp); err != nil {
		return err
	}
	now, err := time.Parse(time.RFC3339Nano, timestamp)
	if err != nil {
		return err
	}
	until, err := time.Parse(time.RFC3339Nano, expiry)
	if err != nil || !until.After(now) {
		return ErrLeaseLost
	}
	checkpoint := domain.Doc{}
	for key, value := range d {
		checkpoint[key] = value
	}
	if err = s.put(ctx, tx, kind, checkpoint); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	// Do not advance the caller's CAS version for a transaction that failed.
	for key, value := range checkpoint {
		d[key] = value
	}
	return nil
}
