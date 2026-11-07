package agentclient

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/newnexus/newnexus/internal/agentproto"
)

var ErrBackpressure = errors.New("durable queue capacity reached")
var ErrRevoked = errors.New("node credentials revoked; enroll a replacement node with a new data directory")

type diskTask struct {
	Task     Task      `json:"task"`
	QueuedAt time.Time `json:"queued_at"`
}
type diskResult struct {
	Result   Result    `json:"result"`
	QueuedAt time.Time `json:"queued_at"`
}

// Outbox persists work before ACK and results before releasing work. Renames and
// directory fsyncs establish crash-safe ordering. A process lock prevents two
// Agents sharing one queue volume.
type Outbox struct {
	mu         sync.Mutex
	dir        string
	maxPending int
	maxBytes   int64
	bytes      int64
	tasks      map[string]diskTask
	results    map[string]diskResult
	completed  map[string]time.Time
	busy       map[string]bool
	unlock     func() error
}

func OpenOutbox(dir string, maxPending int, maxBytes int64) (*Outbox, error) {
	if maxPending < 1 || maxBytes < 1 {
		return nil, fmt.Errorf("invalid outbox limits")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return nil, err
	}
	unlock, err := lockDirectory(dir)
	if err != nil {
		return nil, fmt.Errorf("lock agent data directory: %w", err)
	}
	o := &Outbox{dir: dir, maxPending: maxPending, maxBytes: maxBytes, tasks: map[string]diskTask{}, results: map[string]diskResult{}, completed: map[string]time.Time{}, busy: map[string]bool{}, unlock: unlock}
	failed := true
	defer func() {
		if failed {
			_ = unlock()
		}
	}()
	if _, err := os.Stat(filepath.Join(dir, "revoked.json")); err == nil {
		return nil, ErrRevoked
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	for _, name := range []string{"tasks", "results"} {
		folder := filepath.Join(dir, name)
		if err := os.MkdirAll(folder, 0700); err != nil {
			return nil, err
		}
		entries, err := os.ReadDir(folder)
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
				continue
			}
			path := filepath.Join(folder, entry.Name())
			data, err := os.ReadFile(path)
			if err != nil {
				return nil, err
			}
			o.bytes += int64(len(data))
			if name == "tasks" {
				var row diskTask
				if err := json.Unmarshal(data, &row); err != nil || row.Task.ID == "" || fileKey(row.Task.ID)+".json" != entry.Name() {
					return nil, fmt.Errorf("corrupt durable task %s", entry.Name())
				}
				o.tasks[row.Task.ID] = row
			} else {
				var row diskResult
				if err := json.Unmarshal(data, &row); err != nil || row.Result.ID == "" || fileKey(row.Result.ID)+".json" != entry.Name() {
					return nil, fmt.Errorf("corrupt durable result %s", entry.Name())
				}
				o.results[row.Result.ID] = row
			}
		}
	}
	if data, err := os.ReadFile(filepath.Join(dir, "receipts.json")); err == nil {
		if err := json.Unmarshal(data, &o.completed); err != nil {
			return nil, fmt.Errorf("corrupt durable task receipts: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	// A crash after writing a result but before deleting its task is recoverable.
	for _, row := range o.results {
		if _, ok := o.tasks[row.Result.TaskID]; ok {
			if err := o.removeTaskLocked(row.Result.TaskID); err != nil {
				return nil, err
			}
		}
	}
	for taskID := range o.completed {
		if _, ok := o.tasks[taskID]; ok {
			if err := o.removeTaskLocked(taskID); err != nil {
				return nil, err
			}
		}
	}
	failed = false
	return o, nil
}

func (o *Outbox) Close() error {
	if o.unlock != nil {
		return o.unlock()
	}
	return nil
}

func fileKey(id string) string              { sum := sha256.Sum256([]byte(id)); return hex.EncodeToString(sum[:]) }
func ResultID(nodeID, taskID string) string { return agentproto.ResultID(nodeID, taskID) }

func (o *Outbox) LoadCredentials() (Credentials, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	var c Credentials
	data, err := os.ReadFile(filepath.Join(o.dir, "credentials.json"))
	if os.IsNotExist(err) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return c, fmt.Errorf("corrupt credentials: %w", err)
	}
	if c.NodeID == "" || c.Credential == "" || c.ProtocolVersion != ProtocolVersion {
		return c, fmt.Errorf("invalid persisted credentials")
	}
	return c, nil
}

func (o *Outbox) SaveCredentials(c Credentials) error {
	if c.NodeID == "" || c.Credential == "" || c.ProtocolVersion != ProtocolVersion {
		return fmt.Errorf("invalid credentials")
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	data, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return atomicFile(filepath.Join(o.dir, "credentials.json"), data)
}

func (o *Outbox) Revoke(reason string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	data, _ := json.Marshal(map[string]string{"reason": reason, "revoked_at": time.Now().UTC().Format(time.RFC3339)})
	if err := atomicFile(filepath.Join(o.dir, "revoked.json"), data); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(o.dir, "credentials.json")); err != nil && !os.IsNotExist(err) {
		return err
	}
	return syncDirectory(o.dir)
}

func (o *Outbox) AddTask(task Task) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if task.ID == "" || len(task.ID) > 256 {
		return fmt.Errorf("invalid task id")
	}
	if _, ok := o.tasks[task.ID]; ok {
		return nil
	}
	if _, ok := o.completed[task.ID]; ok {
		return nil
	}
	for _, result := range o.results {
		if result.Result.TaskID == task.ID {
			return nil
		}
	}
	if len(o.tasks)+len(o.results) >= o.maxPending {
		return ErrBackpressure
	}
	row := diskTask{Task: task, QueuedAt: time.Now().UTC()}
	data, err := json.Marshal(row)
	if err != nil {
		return err
	}
	if len(data) > 1<<20 {
		return fmt.Errorf("task frame exceeds 1 MiB")
	}
	if o.bytes+int64(len(data)) > o.maxBytes {
		return ErrBackpressure
	}
	if err := atomicFile(filepath.Join(o.dir, "tasks", fileKey(task.ID)+".json"), data); err != nil {
		return err
	}
	o.tasks[task.ID] = row
	o.bytes += int64(len(data))
	return nil
}

func (o *Outbox) NextTask() (Task, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	var oldest diskTask
	for id, row := range o.tasks {
		if !o.busy[id] && (oldest.Task.ID == "" || row.QueuedAt.Before(oldest.QueuedAt)) {
			oldest = row
		}
	}
	if oldest.Task.ID == "" {
		return Task{}, false
	}
	o.busy[oldest.Task.ID] = true
	return oldest.Task, true
}

func (o *Outbox) ReleaseTask(taskID string) { o.mu.Lock(); defer o.mu.Unlock(); delete(o.busy, taskID) }

func (o *Outbox) SaveResult(result Result) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if result.ID == "" || result.TaskID == "" {
		return fmt.Errorf("invalid result id")
	}
	if _, ok := o.results[result.ID]; ok {
		return o.removeTaskLocked(result.TaskID)
	}
	row := diskResult{Result: result, QueuedAt: time.Now().UTC()}
	data, err := json.Marshal(row)
	if err != nil {
		return err
	}
	// Reserve room for a result even if the queue reaches capacity during a probe.
	// Result durability takes precedence over accepting further tasks.
	if err := atomicFile(filepath.Join(o.dir, "results", fileKey(result.ID)+".json"), data); err != nil {
		return err
	}
	o.results[result.ID] = row
	o.bytes += int64(len(data))
	return o.removeTaskLocked(result.TaskID)
}

func (o *Outbox) removeTaskLocked(taskID string) error {
	path := filepath.Join(o.dir, "tasks", fileKey(taskID)+".json")
	if info, err := os.Stat(path); err == nil {
		if err := os.Remove(path); err != nil {
			return err
		}
		o.bytes -= info.Size()
		if err := syncDirectory(filepath.Dir(path)); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	delete(o.tasks, taskID)
	delete(o.busy, taskID)
	return nil
}

func (o *Outbox) Results(limit int) []Result {
	o.mu.Lock()
	defer o.mu.Unlock()
	if limit < 1 || limit > 256 {
		limit = 256
	}
	rows := make([]diskResult, 0, len(o.results))
	for _, row := range o.results {
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].QueuedAt.Before(rows[j].QueuedAt) })
	if len(rows) > limit {
		rows = rows[:limit]
	}
	results := make([]Result, len(rows))
	for i, row := range rows {
		results[i] = row.Result
	}
	return results
}

func (o *Outbox) AckResults(ids []string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	now := time.Now().UTC()
	for _, id := range ids {
		if row, ok := o.results[id]; ok {
			o.completed[row.Result.TaskID] = now
		}
	}
	for id, when := range o.completed {
		if now.Sub(when) > 7*24*time.Hour {
			delete(o.completed, id)
		}
	}
	if len(o.completed) > 50000 {
		type receipt struct {
			id string
			at time.Time
		}
		rows := make([]receipt, 0, len(o.completed))
		for id, at := range o.completed {
			rows = append(rows, receipt{id, at})
		}
		sort.Slice(rows, func(i, j int) bool { return rows[i].at.Before(rows[j].at) })
		for _, row := range rows[:len(rows)-50000] {
			delete(o.completed, row.id)
		}
	}
	data, err := json.Marshal(o.completed)
	if err != nil {
		return err
	}
	if err := atomicFile(filepath.Join(o.dir, "receipts.json"), data); err != nil {
		return err
	}
	for _, id := range ids {
		if _, ok := o.results[id]; !ok {
			continue
		}
		path := filepath.Join(o.dir, "results", fileKey(id)+".json")
		info, err := os.Stat(path)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if err == nil {
			if err := os.Remove(path); err != nil {
				return err
			}
			o.bytes -= info.Size()
		}
		delete(o.results, id)
	}
	return syncDirectory(filepath.Join(o.dir, "results"))
}

func (o *Outbox) Stats() (pending, results int, bytes int64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.tasks), len(o.results), o.bytes
}

func atomicFile(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".pending-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err := f.Chmod(0600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

// ReadJSONLimited is used at trust boundaries to reject oversized local data.
func ReadJSONLimited(r io.Reader, target any, limit int64) error {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return err
	}
	if int64(len(data)) > limit {
		return fmt.Errorf("JSON exceeds size limit")
	}
	return json.Unmarshal(data, target)
}
