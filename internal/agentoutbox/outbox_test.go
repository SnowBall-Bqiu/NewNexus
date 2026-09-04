package agentoutbox

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestOutboxPersistsAndAcknowledgesResults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "outbox.db")
	box, err := Open(path, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err := box.Put("job-1", json.RawMessage(`{"success":true}`)); err != nil {
		t.Fatal(err)
	}
	if err := box.Close(); err != nil {
		t.Fatal(err)
	}
	box, err = Open(path, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer box.Close()
	items, err := box.Batch(10)
	if err != nil || len(items) != 1 || items[0].JobID != "job-1" {
		t.Fatalf("unexpected batch: %#v %v", items, err)
	}
	if err := box.Ack([]string{"job-1"}); err != nil {
		t.Fatal(err)
	}
	items, err = box.Batch(10)
	if err != nil || len(items) != 0 {
		t.Fatalf("ack did not remove result: %#v %v", items, err)
	}
}
