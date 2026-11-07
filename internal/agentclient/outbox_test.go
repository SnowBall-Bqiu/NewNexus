package agentclient

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestOutboxDurabilityBackpressureAndDedup(t *testing.T) {
	dir := t.TempDir()
	o, err := OpenOutbox(dir, 2, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := OpenOutbox(dir, 2, 1<<20); err == nil {
		t.Fatal("same data directory allowed simultaneous processes")
	}
	credentials := Credentials{NodeID: "node-1", Credential: "secret-1", ProtocolVersion: ProtocolVersion}
	if err := o.SaveCredentials(credentials); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"task-1", "task-2", "task-1"} {
		if err := o.AddTask(Task{ID: id, Address: "8.8.8.8", Mode: "tcp"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := o.AddTask(Task{ID: "task-3"}); !errors.Is(err, ErrBackpressure) {
		t.Fatalf("queue must apply backpressure: %v", err)
	}
	task, ok := o.NextTask()
	if !ok || task.ID != "task-1" {
		t.Fatalf("unexpected next task: %+v", task)
	}
	result := Result{ID: ResultID("node-1", task.ID), TaskID: task.ID, ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), Success: true}
	if err := o.SaveResult(result); err != nil {
		t.Fatal(err)
	}
	if err := o.Close(); err != nil {
		t.Fatal(err)
	}
	o, err = OpenOutbox(dir, 2, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	loaded, err := o.LoadCredentials()
	if err != nil || loaded != credentials {
		t.Fatalf("credentials not durable: %+v %v", loaded, err)
	}
	results := o.Results(256)
	if len(results) != 1 || results[0].ID != result.ID {
		t.Fatalf("result lost across restart: %+v", results)
	}
	if err := o.AddTask(task); err != nil {
		t.Fatal(err)
	}
	pending, queued, _ := o.Stats()
	if pending != 1 || queued != 1 {
		t.Fatalf("duplicate task requeued: %d %d", pending, queued)
	}
	if err := o.AckResults([]string{result.ID}); err != nil {
		t.Fatal(err)
	}
	if len(o.Results(256)) != 0 {
		t.Fatal("acknowledged result remained")
	}
	if err := o.AddTask(task); err != nil {
		t.Fatal(err)
	}
	pending, _, _ = o.Stats()
	if pending != 1 {
		t.Fatal("acknowledged task requeued")
	}
	if err := o.AddTask(Task{ID: "task-3"}); err != nil {
		t.Fatal("ack did not release capacity", err)
	}
	if err := o.SaveCredentials(Credentials{NodeID: "node-1", Credential: "secret-2", ProtocolVersion: 1}); err != nil {
		t.Fatal(err)
	}
	loaded, err = o.LoadCredentials()
	if err != nil || loaded.Credential != "secret-2" {
		t.Fatal("credential rotation not persisted")
	}
}

func TestOutboxCrashRecoveryAndBatchLimit(t *testing.T) {
	dir := t.TempDir()
	o, err := OpenOutbox(dir, 300, 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err := o.AddTask(Task{ID: "crashed"}); err != nil {
		t.Fatal(err)
	}
	row := diskResult{Result: Result{ID: ResultID("node", "crashed"), TaskID: "crashed"}, QueuedAt: time.Now().UTC()}
	// Simulate termination after fsyncing a result, before removing its task.
	data := []byte(fmt.Sprintf(`{"result":{"id":%q,"task_id":"crashed"},"queued_at":%q}`, row.Result.ID, row.QueuedAt.Format(time.RFC3339Nano)))
	if err := atomicFile(filepath.Join(dir, "results", fileKey(row.Result.ID)+".json"), data); err != nil {
		t.Fatal(err)
	}
	o.Close()
	o, err = OpenOutbox(dir, 300, 4<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	pending, results, _ := o.Stats()
	if pending != 0 || results != 1 {
		t.Fatalf("crash recovery invalid: pending %d results %d", pending, results)
	}
	for i := 0; i < 260; i++ {
		task := fmt.Sprint(i)
		if err := o.SaveResult(Result{ID: ResultID("node", task), TaskID: task}); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(o.Results(1000)); got != 256 {
		t.Fatalf("batch exceeded wire limit: %d", got)
	}
}

func TestOutboxRevocationAndCorruption(t *testing.T) {
	dir := t.TempDir()
	o, err := OpenOutbox(dir, 10, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err := o.SaveCredentials(Credentials{NodeID: "n", Credential: "s", ProtocolVersion: 1}); err != nil {
		t.Fatal(err)
	}
	if err := o.Revoke("admin revoked"); err != nil {
		t.Fatal(err)
	}
	o.Close()
	if _, err := os.Stat(filepath.Join(dir, "credentials.json")); !os.IsNotExist(err) {
		t.Fatal("revocation retained credential")
	}
	if _, err := OpenOutbox(dir, 10, 1<<20); !errors.Is(err, ErrRevoked) {
		t.Fatalf("revoked data directory reused: %v", err)
	}
	dir = t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "results"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "results", "bad.json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenOutbox(dir, 10, 1<<20); err == nil {
		t.Fatal("corrupt queue silently discarded")
	}
}
