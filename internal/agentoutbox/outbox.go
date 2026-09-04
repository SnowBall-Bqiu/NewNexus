package agentoutbox

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	bolt "go.etcd.io/bbolt"
)

var resultsBucket = []byte("results")

type Entry struct {
	JobID   string
	Payload []byte
}

type Outbox struct {
	db       *bolt.DB
	path     string
	maxBytes int64
}

func Open(path string, maxBytes int64) (*Outbox, error) {
	if maxBytes <= 0 {
		maxBytes = 100 << 20
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil && filepath.Dir(path) != "." {
		return nil, err
	}
	db, err := bolt.Open(path, 0600, &bolt.Options{Timeout: 2 * time.Second, NoGrowSync: false})
	if err != nil {
		return nil, err
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(resultsBucket)
		return err
	}); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Outbox{db: db, path: path, maxBytes: maxBytes}, nil
}

func (o *Outbox) Put(jobID string, payload []byte) error {
	if jobID == "" {
		return fmt.Errorf("job ID is required")
	}
	if stats, err := os.Stat(o.path); err == nil && stats.Size()+int64(len(payload)) > o.maxBytes {
		return fmt.Errorf("agent outbox has reached its %d-byte limit", o.maxBytes)
	}
	copyPayload := append([]byte(nil), payload...)
	return o.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(resultsBucket).Put([]byte(jobID), copyPayload)
	})
}

func (o *Outbox) Batch(limit int) ([]Entry, error) {
	if limit <= 0 {
		limit = 50
	}
	entries := make([]Entry, 0, limit)
	err := o.db.View(func(tx *bolt.Tx) error {
		cursor := tx.Bucket(resultsBucket).Cursor()
		for key, value := cursor.First(); key != nil && len(entries) < limit; key, value = cursor.Next() {
			entries = append(entries, Entry{JobID: string(key), Payload: append([]byte(nil), value...)})
		}
		return nil
	})
	return entries, err
}

func (o *Outbox) Ack(jobIDs []string) error {
	return o.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(resultsBucket)
		for _, jobID := range jobIDs {
			if err := bucket.Delete([]byte(jobID)); err != nil {
				return err
			}
		}
		return nil
	})
}

func (o *Outbox) Stats() (count int, bytes int64, err error) {
	err = o.db.View(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(resultsBucket)
		return bucket.ForEach(func(key, value []byte) error {
			count++
			bytes += int64(len(key) + len(value))
			return nil
		})
	})
	return
}

func (o *Outbox) Close() error { return o.db.Close() }
