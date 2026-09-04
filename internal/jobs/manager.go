package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/SnowBall-Bqiu/NewNexus/internal/domain"
	"github.com/SnowBall-Bqiu/NewNexus/internal/events"
	"github.com/SnowBall-Bqiu/NewNexus/internal/store"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	StatusQueued    = "queued"
	StatusRunning   = "running"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
	StatusCancelled = "cancelled"
)

type Handler func(context.Context, domain.Job, Reporter) (any, error)

type Reporter interface {
	Progress(int, any) error
}

type Error struct {
	Code      string
	Message   string
	Retryable bool
}

func (e *Error) Error() string { return e.Message }

type EnqueueOptions struct {
	Type           string
	ResourceType   string
	ResourceID     string
	Payload        any
	IdempotencyKey string
	ExecutionKey   string
	MaxAttempts    int
	RunAt          time.Time
}

type Manager struct {
	store    *store.Store
	broker   *events.Broker
	workerID string
	handlers map[string]Handler
	mu       sync.RWMutex
}

func New(database *store.Store, broker *events.Broker) *Manager {
	return &Manager{store: database, broker: broker, workerID: uuid.NewString(), handlers: make(map[string]Handler)}
}

func (m *Manager) Register(jobType string, handler Handler) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.handlers[jobType] = handler
}

func (m *Manager) Enqueue(options EnqueueOptions) (domain.Job, bool, error) {
	if options.Type == "" {
		return domain.Job{}, false, errors.New("job type is required")
	}
	if options.MaxAttempts <= 0 {
		options.MaxAttempts = 5
	}
	if options.RunAt.IsZero() {
		options.RunAt = time.Now().UTC()
	}
	key := options.IdempotencyKey
	if key == "" {
		key = "generated:" + uuid.NewString()
	} else {
		key = options.Type + ":" + options.ResourceID + ":" + key
	}
	if len(key) > 160 {
		return domain.Job{}, false, errors.New("idempotency key is too long")
	}
	executionKey := strings.TrimSpace(options.ExecutionKey)
	if len(executionKey) > 128 {
		return domain.Job{}, false, errors.New("execution key is too long")
	}
	payload, err := json.Marshal(options.Payload)
	if err != nil {
		return domain.Job{}, false, fmt.Errorf("encode job payload: %w", err)
	}
	job := domain.Job{
		Type: options.Type, Status: StatusQueued, ResourceType: options.ResourceType, ResourceID: options.ResourceID,
		PayloadJSON: string(payload), MaxAttempts: options.MaxAttempts, IdempotencyKey: key, ExecutionKey: executionKey, RunAt: options.RunAt,
	}
	result := m.store.DB.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "idempotency_key"}},
		DoNothing: true,
	}).Create(&job)
	if result.Error != nil {
		return domain.Job{}, false, result.Error
	}
	if result.RowsAffected == 0 {
		var existing domain.Job
		if err := m.store.DB.Where("idempotency_key = ?", key).First(&existing).Error; err != nil {
			return domain.Job{}, false, err
		}
		return existing, true, nil
	}
	m.publish(job, "queued")
	return job, false, nil
}

func (m *Manager) Run(ctx context.Context, workers int) {
	if workers < 1 {
		workers = 1
	}
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			m.worker(ctx, fmt.Sprintf("%s-%d", m.workerID, index+1))
		}(i)
	}
	<-ctx.Done()
	wg.Wait()
}

func (m *Manager) worker(ctx context.Context, worker string) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			job, ok, err := m.claim(worker)
			if err != nil {
				slog.Error("claim job failed", "error", err)
				continue
			}
			if ok {
				m.execute(ctx, worker, job)
			}
		}
	}
}

func (m *Manager) claim(worker string) (domain.Job, bool, error) {
	var job domain.Job
	now := time.Now().UTC()
	err := m.store.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("expires_at < ?", now).Delete(&domain.JobExecutionLease{}).Error; err != nil {
			return err
		}
		if err := tx.Model(&domain.Job{}).Where("status = ? AND lease_until IS NOT NULL AND lease_until < ?", StatusRunning, now).Updates(map[string]any{"status": StatusQueued, "run_at": now, "lease_owner": "", "lease_until": nil, "error_code": "lease_expired", "error_message": "worker lease expired; job was requeued"}).Error; err != nil {
			return err
		}
		query := tx.Where("status = ? AND run_at <= ? AND (lease_until IS NULL OR lease_until < ?)", StatusQueued, now, now).
			Where("execution_key = '' OR NOT EXISTS (SELECT 1 FROM job_execution_leases WHERE job_execution_leases.execution_key = jobs.execution_key AND job_execution_leases.expires_at > ?)", now).
			Order("run_at ASC, created_at ASC")
		if m.store.Driver == "postgres" || m.store.Driver == "postgresql" {
			query = query.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"})
		}
		if err := query.First(&job).Error; err != nil {
			return err
		}
		started := now
		leaseUntil := now.Add(2 * time.Minute)
		result := tx.Model(&domain.Job{}).Where("id = ? AND status = ?", job.ID, StatusQueued).Updates(map[string]any{
			"status": StatusRunning, "attempts": gorm.Expr("attempts + 1"), "lease_owner": worker, "lease_until": leaseUntil, "started_at": started,
		})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return gorm.ErrRecordNotFound
		}
		if job.ExecutionKey != "" {
			lease := domain.JobExecutionLease{ExecutionKey: job.ExecutionKey, JobID: job.ID, LeaseOwner: worker, ExpiresAt: leaseUntil, UpdatedAt: now}
			created := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&lease)
			if created.Error != nil {
				return created.Error
			}
			if created.RowsAffected != 1 {
				return gorm.ErrRecordNotFound
			}
		}
		job.Status = StatusRunning
		job.Attempts++
		job.LeaseOwner = worker
		job.LeaseUntil = &leaseUntil
		job.StartedAt = &started
		return nil
	})
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return domain.Job{}, false, nil
	}
	return job, err == nil, err
}

func (m *Manager) execute(parent context.Context, worker string, job domain.Job) {
	m.mu.RLock()
	handler := m.handlers[job.Type]
	m.mu.RUnlock()
	attempt := domain.JobAttempt{JobID: job.ID, Attempt: job.Attempts, Worker: worker, Status: StatusRunning, StartedAt: time.Now().UTC()}
	_ = m.store.DB.Create(&attempt).Error
	m.publish(job, "running")
	if handler == nil {
		m.finish(job, attempt, nil, &Error{Code: "handler_missing", Message: "job handler is not registered"})
		return
	}
	ctx, cancel := context.WithTimeout(parent, 10*time.Minute)
	defer cancel()
	renewCtx, stopRenew := context.WithCancel(ctx)
	renewed := make(chan struct{})
	go func() {
		defer close(renewed)
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-renewCtx.Done():
				return
			case <-ticker.C:
				expires := time.Now().UTC().Add(2 * time.Minute)
				_ = m.store.DB.Model(&domain.Job{}).Where("id = ? AND status = ? AND lease_owner = ?", job.ID, StatusRunning, worker).Update("lease_until", expires).Error
				if job.ExecutionKey != "" {
					_ = m.store.DB.Model(&domain.JobExecutionLease{}).Where("execution_key = ? AND job_id = ? AND lease_owner = ?", job.ExecutionKey, job.ID, worker).Update("expires_at", expires).Error
				}
			}
		}
	}()
	result, err := handler(ctx, job, jobReporter{manager: m, jobID: job.ID})
	stopRenew()
	<-renewed
	m.finish(job, attempt, result, err)
}

func (m *Manager) finish(job domain.Job, attempt domain.JobAttempt, result any, handlerErr error) {
	now := time.Now().UTC()
	attempt.FinishedAt = &now
	updates := map[string]any{"lease_owner": "", "lease_until": nil, "finished_at": now}
	if handlerErr == nil {
		raw, _ := json.Marshal(result)
		updates["status"] = StatusSucceeded
		updates["progress"] = 100
		updates["result_json"] = string(raw)
		updated := m.store.DB.Model(&domain.Job{}).Where("id = ? AND status = ?", job.ID, StatusRunning).Updates(updates)
		if updated.Error != nil || updated.RowsAffected == 0 {
			attempt.Status = StatusCancelled
			attempt.ErrorCode = "cancelled"
			attempt.ErrorMessage = "job was cancelled while the handler was running"
			_ = m.store.DB.Model(&attempt).Updates(attempt).Error
			m.releaseExecutionLease(job)
			return
		}
		m.releaseExecutionLease(job)
		attempt.Status = StatusSucceeded
		_ = m.store.DB.Model(&attempt).Updates(attempt).Error
		job.Status, job.Progress, job.ResultJSON = StatusSucceeded, 100, string(raw)
		m.publish(job, "succeeded")
		return
	}
	jobError := &Error{Code: "job_failed", Message: handlerErr.Error()}
	var typed *Error
	if errors.As(handlerErr, &typed) {
		jobError = typed
	}
	attempt.Status, attempt.ErrorCode, attempt.ErrorMessage = StatusFailed, jobError.Code, jobError.Message
	updates["error_code"], updates["error_message"] = jobError.Code, jobError.Message
	if jobError.Retryable && job.Attempts < job.MaxAttempts {
		delay := time.Duration(math.Min(math.Pow(2, float64(job.Attempts-1))*5, 300)) * time.Second
		updates["status"], updates["run_at"], updates["finished_at"] = StatusQueued, now.Add(delay), nil
		job.Status = StatusQueued
	} else {
		updates["status"] = StatusFailed
		job.Status = StatusFailed
	}
	job.ErrorCode, job.ErrorMessage = jobError.Code, jobError.Message
	updated := m.store.DB.Model(&domain.Job{}).Where("id = ? AND status = ?", job.ID, StatusRunning).Updates(updates)
	if updated.Error != nil || updated.RowsAffected == 0 {
		attempt.Status = StatusCancelled
		attempt.ErrorCode = "cancelled"
		attempt.ErrorMessage = "job was cancelled while the handler was running"
		_ = m.store.DB.Model(&attempt).Updates(attempt).Error
		m.releaseExecutionLease(job)
		return
	}
	m.releaseExecutionLease(job)
	_ = m.store.DB.Model(&attempt).Updates(attempt).Error
	m.publish(job, job.Status)
}

type jobReporter struct {
	manager *Manager
	jobID   string
}

func (r jobReporter) Progress(progress int, details any) error {
	if progress < 0 {
		progress = 0
	}
	if progress > 99 {
		progress = 99
	}
	updates := map[string]any{"progress": progress, "lease_until": time.Now().UTC().Add(2 * time.Minute)}
	if details != nil {
		raw, _ := json.Marshal(details)
		updates["result_json"] = string(raw)
	}
	if err := r.manager.store.DB.Model(&domain.Job{}).Where("id = ? AND status = ?", r.jobID, StatusRunning).Updates(updates).Error; err != nil {
		return err
	}
	var job domain.Job
	if err := r.manager.store.DB.Select("execution_key").First(&job, "id = ?", r.jobID).Error; err != nil || job.ExecutionKey == "" {
		return err
	}
	return r.manager.store.DB.Model(&domain.JobExecutionLease{}).Where("execution_key = ? AND job_id = ?", job.ExecutionKey, r.jobID).Update("expires_at", time.Now().UTC().Add(2*time.Minute)).Error
}

func (m *Manager) releaseExecutionLease(job domain.Job) {
	if job.ExecutionKey == "" {
		return
	}
	_ = m.store.DB.Where("execution_key = ? AND job_id = ?", job.ExecutionKey, job.ID).Delete(&domain.JobExecutionLease{}).Error
}

func (m *Manager) Cancel(id string) (domain.Job, error) {
	var before domain.Job
	if err := m.store.DB.First(&before, "id = ?", id).Error; err != nil {
		return domain.Job{}, err
	}
	wasRunning := before.Status == StatusRunning
	now := time.Now().UTC()
	result := m.store.DB.Model(&domain.Job{}).Where("id = ? AND status IN ?", id, []string{StatusQueued, StatusRunning}).Updates(map[string]any{"status": StatusCancelled, "cancelled_at": now, "finished_at": now, "lease_owner": "", "lease_until": nil})
	if result.Error != nil {
		return domain.Job{}, result.Error
	}
	var job domain.Job
	if err := m.store.DB.First(&job, "id = ?", id).Error; err != nil {
		return domain.Job{}, err
	}
	if result.RowsAffected == 0 && job.Status != StatusCancelled {
		return job, errors.New("only queued or running jobs can be cancelled")
	}
	if !wasRunning {
		m.releaseExecutionLease(job)
	}
	m.publish(job, "cancelled")
	return job, nil
}

func (m *Manager) Retry(id string) (domain.Job, error) {
	now := time.Now().UTC()
	result := m.store.DB.Model(&domain.Job{}).Where("id = ? AND status IN ?", id, []string{StatusFailed, StatusCancelled}).Updates(map[string]any{
		"status": StatusQueued, "attempts": 0, "progress": 0, "error_code": "", "error_message": "", "run_at": now,
		"finished_at": nil, "cancelled_at": nil, "lease_owner": "", "lease_until": nil,
	})
	if result.Error != nil {
		return domain.Job{}, result.Error
	}
	if result.RowsAffected != 1 {
		return domain.Job{}, errors.New("only failed or cancelled jobs can be retried")
	}
	var job domain.Job
	if err := m.store.DB.First(&job, "id = ?", id).Error; err != nil {
		return domain.Job{}, err
	}
	m.publish(job, "queued")
	return job, nil
}

func (m *Manager) publish(job domain.Job, phase string) {
	if m.broker == nil {
		return
	}
	raw, _ := json.Marshal(map[string]any{"type": "job", "phase": phase, "resourceId": job.ID, "job": job})
	m.broker.Publish(raw)
}
