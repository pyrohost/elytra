package jobs

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/apex/log"
)

// JobStatus represents the current status of a job
type JobStatus string

const (
	JobStatusPending    JobStatus = "pending"
	JobStatusRunning    JobStatus = "running"
	JobStatusCompleted  JobStatus = "completed"
	JobStatusFailed     JobStatus = "failed"
	JobStatusCancelled  JobStatus = "cancelled"
)

// JobType represents different types of jobs that can be processed
type JobType string

const (
	JobTypeBackupCreate  JobType = "backup_create"
	JobTypeBackupDelete  JobType = "backup_delete"
	JobTypeBackupRestore JobType = "backup_restore"
)

// JobData holds the specific data needed for each job type
type JobData interface{}

// BackupCreateData contains data needed for backup creation jobs
type BackupCreateData struct {
	ServerID    string                 `json:"server_id"`
	BackupUUID  string                 `json:"backup_uuid"`
	AdapterType string                 `json:"adapter_type"`
	Ignore      string                 `json:"ignore"`
	Context     map[string]interface{} `json:"context"`
}

// BackupDeleteData contains data needed for backup deletion jobs
type BackupDeleteData struct {
	ServerID    string `json:"server_id"`
	BackupUUID  string `json:"backup_uuid"`
	AdapterType string `json:"adapter_type"`
}

// BackupRestoreData contains data needed for backup restoration jobs
type BackupRestoreData struct {
	ServerID          string `json:"server_id"`
	BackupUUID        string `json:"backup_uuid"`
	AdapterType       string `json:"adapter_type"`
	TruncateDirectory bool   `json:"truncate_directory"`
	DownloadURL       string `json:"download_url,omitempty"`
}

// Job represents a background job with its metadata and data
type Job struct {
	ID        string      `json:"id"`
	Type      JobType     `json:"type"`
	Status    JobStatus   `json:"status"`
	Data      JobData     `json:"data"`
	Progress  int         `json:"progress"`  // 0-100
	Message   string      `json:"message"`   // Current status message
	Error     string      `json:"error"`     // Error message if failed
	Result    interface{} `json:"result"`    // Job result data for completed jobs
	CreatedAt time.Time   `json:"created_at"`
	UpdatedAt time.Time   `json:"updated_at"`

	// Internal fields
	ctx    context.Context
	cancel context.CancelFunc
}

// JobHandler defines the interface for job processing functions
type JobHandler func(ctx context.Context, job *Job) error

// JobQueue manages background job processing
type JobQueue struct {
	mu       sync.RWMutex
	jobs     map[string]*Job
	handlers map[JobType]JobHandler
	workers  int
	queue    chan *Job
	stopping bool
	wg       sync.WaitGroup
}

// NewJobQueue creates a new job queue with the specified number of workers
func NewJobQueue(workers int) *JobQueue {
	return &JobQueue{
		jobs:     make(map[string]*Job),
		handlers: make(map[JobType]JobHandler),
		workers:  workers,
		queue:    make(chan *Job, workers*2), // Buffer for pending jobs
	}
}

// RegisterHandler registers a handler function for a specific job type
func (jq *JobQueue) RegisterHandler(jobType JobType, handler JobHandler) {
	jq.mu.Lock()
	defer jq.mu.Unlock()
	jq.handlers[jobType] = handler
}

// Start begins processing jobs with the configured number of workers
func (jq *JobQueue) Start(ctx context.Context) {
	jq.mu.Lock()
	defer jq.mu.Unlock()

	if jq.stopping {
		return
	}

	for i := 0; i < jq.workers; i++ {
		jq.wg.Add(1)
		go jq.worker(ctx, i)
	}

	log.WithField("workers", jq.workers).Info("job queue started")
}

// Stop gracefully shuts down the job queue
func (jq *JobQueue) Stop() {
	jq.mu.Lock()
	jq.stopping = true
	close(jq.queue)
	jq.mu.Unlock()

	jq.wg.Wait()
	log.Info("job queue stopped")
}

// Submit adds a new job to the queue and returns the job ID
func (jq *JobQueue) Submit(jobType JobType, data JobData) string {
	ctx, cancel := context.WithCancel(context.Background())

	job := &Job{
		ID:        uuid.New().String(),
		Type:      jobType,
		Status:    JobStatusPending,
		Data:      data,
		Progress:  0,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		ctx:       ctx,
		cancel:    cancel,
	}

	jq.mu.Lock()
	jq.jobs[job.ID] = job
	jq.mu.Unlock()

	// Non-blocking send to queue
	select {
	case jq.queue <- job:
		log.WithFields(log.Fields{
			"job_id": job.ID,
			"type":   job.Type,
		}).Debug("job submitted to queue")
	default:
		// Queue is full, update job status
		job.Status = JobStatusFailed
		job.Error = "job queue is full"
		job.UpdatedAt = time.Now()
		log.WithFields(log.Fields{
			"job_id": job.ID,
			"type":   job.Type,
		}).Warn("job queue is full, rejecting job")
	}

	return job.ID
}

// GetJob retrieves a job by its ID
func (jq *JobQueue) GetJob(id string) (*Job, bool) {
	jq.mu.RLock()
	defer jq.mu.RUnlock()
	job, exists := jq.jobs[id]
	return job, exists
}

// UpdateJob updates a job's status, progress, and message
func (jq *JobQueue) UpdateJob(id string, status JobStatus, progress int, message string) {
	jq.mu.Lock()
	defer jq.mu.Unlock()

	if job, exists := jq.jobs[id]; exists {
		job.Status = status
		job.Progress = progress
		job.Message = message
		job.UpdatedAt = time.Now()
	}
}

// FailJob marks a job as failed with an error message
func (jq *JobQueue) FailJob(id string, err error) {
	jq.mu.Lock()
	defer jq.mu.Unlock()

	if job, exists := jq.jobs[id]; exists {
		job.Status = JobStatusFailed
		job.Error = err.Error()
		job.UpdatedAt = time.Now()

		log.WithFields(log.Fields{
			"job_id": job.ID,
			"type":   job.Type,
			"error":  err.Error(),
		}).Error("job failed")
	}
}

// CompleteJob marks a job as completed with optional result data
func (jq *JobQueue) CompleteJob(id string, result interface{}) {
	jq.mu.Lock()
	defer jq.mu.Unlock()

	if job, exists := jq.jobs[id]; exists {
		job.Status = JobStatusCompleted
		job.Progress = 100
		job.Result = result
		job.UpdatedAt = time.Now()

		log.WithFields(log.Fields{
			"job_id": job.ID,
			"type":   job.Type,
		}).Info("job completed successfully")
	}
}

// CancelJob cancels a running or pending job
func (jq *JobQueue) CancelJob(id string) bool {
	jq.mu.Lock()
	defer jq.mu.Unlock()

	if job, exists := jq.jobs[id]; exists {
		if job.Status == JobStatusPending || job.Status == JobStatusRunning {
			job.cancel()
			job.Status = JobStatusCancelled
			job.UpdatedAt = time.Now()
			return true
		}
	}
	return false
}

// ListJobs returns all jobs, optionally filtered by status or type
func (jq *JobQueue) ListJobs(status JobStatus, jobType JobType) []*Job {
	jq.mu.RLock()
	defer jq.mu.RUnlock()

	var result []*Job
	for _, job := range jq.jobs {
		if status != "" && job.Status != status {
			continue
		}
		if jobType != "" && job.Type != jobType {
			continue
		}
		result = append(result, job)
	}
	return result
}

// CleanupJobs removes old completed, failed, or cancelled jobs
func (jq *JobQueue) CleanupJobs(olderThan time.Duration) int {
	jq.mu.Lock()
	defer jq.mu.Unlock()

	cutoff := time.Now().Add(-olderThan)
	cleaned := 0

	for id, job := range jq.jobs {
		if (job.Status == JobStatusCompleted || job.Status == JobStatusFailed || job.Status == JobStatusCancelled) &&
			job.UpdatedAt.Before(cutoff) {
			delete(jq.jobs, id)
			cleaned++
		}
	}

	if cleaned > 0 {
		log.WithField("cleaned", cleaned).Debug("cleaned up old jobs")
	}

	return cleaned
}

// worker processes jobs from the queue
func (jq *JobQueue) worker(ctx context.Context, workerID int) {
	defer jq.wg.Done()

	logger := log.WithField("worker", workerID)
	logger.Debug("job worker started")

	for job := range jq.queue {
		if job == nil {
			break
		}

		jq.processJob(ctx, job, logger)
	}

	logger.Debug("job worker stopped")
}

// processJob executes a single job
func (jq *JobQueue) processJob(ctx context.Context, job *Job, logger *log.Entry) {
	// Check if job was cancelled before processing
	select {
	case <-job.ctx.Done():
		jq.UpdateJob(job.ID, JobStatusCancelled, 0, "Job was cancelled")
		return
	default:
	}

	// Update job status to running
	jq.UpdateJob(job.ID, JobStatusRunning, 0, "Processing...")

	jobLogger := logger.WithFields(log.Fields{
		"job_id": job.ID,
		"type":   job.Type,
	})

	jobLogger.Debug("processing job")

	// Get handler for job type
	jq.mu.RLock()
	handler, exists := jq.handlers[job.Type]
	jq.mu.RUnlock()

	if !exists {
		jq.FailJob(job.ID, fmt.Errorf("no handler registered for job type: %s", job.Type))
		return
	}

	// Execute the job
	err := handler(job.ctx, job)
	if err != nil {
		jq.FailJob(job.ID, err)
		jobLogger.WithError(err).Error("job processing failed")
		return
	}

	// Mark job as completed
	jq.UpdateJob(job.ID, JobStatusCompleted, 100, "Completed successfully")
	jobLogger.Debug("job completed successfully")
}