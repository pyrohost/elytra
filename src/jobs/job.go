package jobs

import (
	"context"
	"fmt"
	"time"

	"github.com/pyrohost/elytra/src/remote"
	"github.com/pyrohost/elytra/src/server"
)

// Status represents the current status of a job
type Status string

const (
	StatusPending   Status = "pending"
	StatusRunning   Status = "running"
	StatusCompleted Status = "completed"
	StatusFailed    Status = "failed"
)

// ProgressReporter allows jobs to report progress updates automatically
type ProgressReporter interface {
	ReportProgress(progress int, message string)
	ReportStatus(status Status, message string)
}

// Job represents a generic job that can be executed
type Job interface {
	// GetID returns the unique identifier for this job
	GetID() string

	// GetType returns the job type (e.g., "backup_create", "backup_delete")
	GetType() string

	// Execute runs the job and returns any result or error
	// The ProgressReporter is provided to allow real-time status updates
	Execute(ctx context.Context, reporter ProgressReporter) (interface{}, error)

	// Validate ensures the job data is valid before execution
	Validate() error

	// GetProgress returns current progress (0-100)
	GetProgress() int

	// GetMessage returns current status message
	GetMessage() string
}

// WebSocketJob is an optional interface for jobs that need WebSocket events
type WebSocketJob interface {
	// GetServerID returns the server ID for WebSocket event targeting
	GetServerID() string

	// GetWebSocketEventType returns the event type for this job (e.g., "backup.status")
	GetWebSocketEventType() string

	// GetWebSocketEventData returns job-specific data to include in WebSocket events
	GetWebSocketEventData() map[string]interface{}
}

// Manager handles job execution and lifecycle
type Manager struct {
	jobs          map[string]*Instance
	jobTypes      map[string]func(data map[string]interface{}) (Job, error)
	workers       int
	queue         chan *Instance
	client        remote.Client      // For notifying Panel automatically
	serverManager *server.Manager    // For WebSocket event publishing
}

// Instance wraps a Job implementation with execution metadata
type Instance struct {
	Job       Job         `json:"job"`
	Status    Status      `json:"status"`
	Progress  int         `json:"progress"`
	Message   string      `json:"message"`
	Error     string      `json:"error"`
	Result    interface{} `json:"result"`
	CreatedAt time.Time   `json:"created_at"`
	UpdatedAt time.Time   `json:"updated_at"`
}

// NewManager creates a new job manager
func NewManager(workers int) *Manager {
	return &Manager{
		jobs:          make(map[string]*Instance),
		jobTypes:      make(map[string]func(data map[string]interface{}) (Job, error)),
		workers:       workers,
		queue:         make(chan *Instance, 100),
		serverManager: nil, // Set later via SetServerManager
	}
}

// RegisterJobType registers a job constructor for a specific type
func (m *Manager) RegisterJobType(jobType string, constructor func(data map[string]interface{}) (Job, error)) {
	m.jobTypes[jobType] = constructor
}

// SetClient sets the remote client for Panel notifications
func (m *Manager) SetClient(client remote.Client) {
	m.client = client
}

// SetServerManager sets the server manager for WebSocket event publishing
func (m *Manager) SetServerManager(serverManager *server.Manager) {
	m.serverManager = serverManager
}

// CreateJob creates and queues a new job for execution
func (m *Manager) CreateJob(jobType string, data map[string]interface{}) (string, error) {
	constructor, exists := m.jobTypes[jobType]
	if !exists {
		return "", fmt.Errorf("unknown job type: %s", jobType)
	}

	job, err := constructor(data)
	if err != nil {
		return "", fmt.Errorf("failed to create job: %w", err)
	}

	if err := job.Validate(); err != nil {
		return "", fmt.Errorf("job validation failed: %w", err)
	}

	instance := &Instance{
		Job:       job,
		Status:    StatusPending,
		Progress:  0,
		Message:   "Job queued",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}

	m.jobs[job.GetID()] = instance

	// Automatically report job creation to Panel
	m.reportProgress(instance)

	// Queue for execution
	select {
	case m.queue <- instance:
		return job.GetID(), nil
	default:
		return "", fmt.Errorf("job queue is full")
	}
}

// GetJob returns specific job information
func (m *Manager) GetJob(jobID string) (*Instance, bool) {
	instance, exists := m.jobs[jobID]
	return instance, exists
}

// UpdateJob updates job status, progress, and result
func (m *Manager) UpdateJob(jobID string, status Status, progress int, message string, result interface{}) error {
	instance, exists := m.jobs[jobID]
	if !exists {
		return fmt.Errorf("job not found: %s", jobID)
	}
	instance.Status = status
	instance.Progress = progress
	instance.Message = message
	if result != nil {
		instance.Result = result
	}
	instance.UpdatedAt = time.Now()
	m.reportProgress(instance)
	return nil
}

// CancelJob cancels a running job
func (m *Manager) CancelJob(jobID string) error {
	instance, exists := m.jobs[jobID]
	if !exists {
		return fmt.Errorf("job not found: %s", jobID)
	}
	if instance.Status == StatusCompleted || instance.Status == StatusFailed {
		return fmt.Errorf("job already finished")
	}
	instance.Status = StatusFailed
	instance.Message = "Job cancelled"
	instance.Error = "Cancelled by user"
	instance.UpdatedAt = time.Now()
	m.reportProgress(instance)
	return nil
}

// Start begins processing jobs with workers
func (m *Manager) Start() {
	for i := 0; i < m.workers; i++ {
		go m.worker()
	}
}

// worker processes jobs from the queue
func (m *Manager) worker() {
	for instance := range m.queue {
		m.executeJob(instance)
	}
}

// progressReporter implements ProgressReporter for real-time updates
type progressReporter struct {
	instance *Instance
	manager  *Manager
}

func (r *progressReporter) ReportProgress(progress int, message string) {
	r.instance.Progress = progress
	r.instance.Message = message
	r.instance.UpdatedAt = time.Now()
	r.manager.reportProgress(r.instance)
}

func (r *progressReporter) ReportStatus(status Status, message string) {
	r.instance.Status = status
	r.instance.Message = message
	r.instance.UpdatedAt = time.Now()
	r.manager.reportProgress(r.instance)
}

// executeJob runs a single job instance
func (m *Manager) executeJob(instance *Instance) {
	defer func() {
		if r := recover(); r != nil {
			instance.Status = StatusFailed
			instance.Error = fmt.Sprintf("job panicked: %v", r)
			instance.UpdatedAt = time.Now()
			// Automatically report panic to Panel
			m.reportProgress(instance)
		}
	}()

	// Update status to running and automatically notify Panel
	instance.Status = StatusRunning
	instance.Message = "Job executing"
	instance.UpdatedAt = time.Now()
	m.reportProgress(instance)

	// Create progress reporter for real-time updates
	reporter := &progressReporter{
		instance: instance,
		manager:  m,
	}

	// Execute the job
	ctx := context.Background() // Add timeout/cancellation if needed
	result, err := instance.Job.Execute(ctx, reporter)

	instance.UpdatedAt = time.Now()

	if err != nil {
		instance.Status = StatusFailed
		instance.Error = err.Error()
		instance.Message = "Job failed"
		// Always report failures
		m.reportProgress(instance)
	} else {
		// Only report completion if the job hasn't already reported 100% progress
		if instance.Progress < 100 || instance.Status != StatusCompleted {
			instance.Status = StatusCompleted
			instance.Result = result
			instance.Progress = 100
			instance.Message = "Job completed successfully"
			// Report completion
			m.reportProgress(instance)
		} else {
			// Job already reported completion, just update the result
			instance.Result = result
		}
	}
}

// reportProgress automatically sends ALL status updates to the Panel and WebSocket clients
func (m *Manager) reportProgress(instance *Instance) {
	// Send Panel notification
	if m.client != nil {
		// Prepare status notification with all current information
		notification := map[string]interface{}{
			"status":        string(instance.Status),
			"progress":      instance.Progress,
			"message":       instance.Message,
			"job_type":      instance.Job.GetType(),
			"error_message": instance.Error,
			"updated_at":    instance.UpdatedAt.Unix(),
		}

		// Set successful field based on current status
		if instance.Status == StatusCompleted {
			notification["successful"] = true
			if instance.Result != nil {
				if resultMap, ok := instance.Result.(map[string]interface{}); ok {
					for key, value := range resultMap {
						notification[key] = value
					}
				}
			}
		} else if instance.Status == StatusFailed {
			notification["successful"] = false
		} else {
			// For pending/running statuses, indicate not yet complete
			notification["successful"] = false
		}

		// Send status notification to Panel asynchronously for ALL status changes
		go func() {
			if err := m.client.ReportJobCompletion(context.Background(), instance.Job.GetID(), notification); err != nil {
				// Log error but don't fail the job
				fmt.Printf("Failed to notify Panel of job status: %v\n", err)
			}
		}()
	}

	// Send WebSocket event for real-time updates
	m.publishWebSocketEvent(instance)
}

// publishWebSocketEvent sends real-time status updates via WebSocket
func (m *Manager) publishWebSocketEvent(instance *Instance) {
	// Only publish if we have a server manager and the job supports WebSocket events
	if m.serverManager == nil {
		return
	}

	// Check if the job implements WebSocketJob interface
	webSocketJob, ok := instance.Job.(WebSocketJob)
	if !ok {
		return
	}

	// Get the server for this job
	serverID := webSocketJob.GetServerID()
	server, exists := m.serverManager.Get(serverID)
	if !exists {
		return
	}

	// Prepare base event data with generic job information
	eventData := map[string]interface{}{
		"job_id":    instance.Job.GetID(),
		"job_type":  instance.Job.GetType(),
		"status":    string(instance.Status),
		"progress":  instance.Progress,
		"message":   instance.Message,
		"timestamp": instance.UpdatedAt.Unix(),
	}

	// Add job-specific WebSocket data
	for key, value := range webSocketJob.GetWebSocketEventData() {
		eventData[key] = value
	}

	// Include error information if present
	if instance.Error != "" {
		eventData["error"] = instance.Error
	}

	// Include result data if completed
	if instance.Status == StatusCompleted && instance.Result != nil {
		if resultMap, ok := instance.Result.(map[string]interface{}); ok {
			for key, value := range resultMap {
				eventData[key] = value
			}
		}
	}

	// Get the event type from the job (allows job-specific event types)
	eventType := webSocketJob.GetWebSocketEventType()

	// Publish the WebSocket event
	server.Events().Publish(eventType, eventData)
}