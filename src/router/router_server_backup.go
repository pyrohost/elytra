package router

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/pyrohost/elytra/src/jobs"
	"github.com/pyrohost/elytra/src/router/middleware"
	"github.com/pyrohost/elytra/src/server/backup"
)

// postServerBackup initiates a backup operation asynchronously and returns a job ID
func postServerBackup(c *gin.Context) {
	s := middleware.ExtractServer(c)
	jobQueue := middleware.ExtractJobQueue(c)
	var data struct {
		Adapter backup.AdapterType `json:"adapter"`
		Uuid    string             `json:"uuid"`
		Ignore  string             `json:"ignore"`
	}
	if err := c.BindJSON(&data); err != nil {
		return
	}

	// Validate adapter type
	switch data.Adapter {
	case backup.LocalBackupAdapter, backup.S3BackupAdapter, backup.RusticLocalAdapter, backup.RusticS3Adapter:
		// Valid adapter types
	default:
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "Invalid backup adapter type: " + string(data.Adapter),
		})
		return
	}

	// Create job data
	jobData := jobs.BackupCreateData{
		ServerID:    s.ID(),
		BackupUUID:  data.Uuid,
		AdapterType: string(data.Adapter),
		Ignore:      data.Ignore,
		Context: map[string]interface{}{
			"server":     s.ID(),
			"request_id": c.GetString("request_id"),
		},
	}

	// Submit job to queue
	jobID := jobQueue.Submit(jobs.JobTypeBackupCreate, jobData)

	c.JSON(http.StatusAccepted, gin.H{
		"job_id": jobID,
		"status": "accepted",
		"message": "Backup job has been queued for processing",
	})
}

// postServerRestoreBackup initiates a backup restoration operation asynchronously and returns a job ID
func postServerRestoreBackup(c *gin.Context) {
	s := middleware.ExtractServer(c)
	jobQueue := middleware.ExtractJobQueue(c)
	backupUuid := c.Param("backup")

	var data struct {
		Adapter           backup.AdapterType `binding:"required,oneof=elytra s3 rustic_local rustic_s3" json:"adapter"`
		TruncateDirectory bool               `json:"truncate_directory"`
		DownloadUrl       string             `json:"download_url"`
	}
	if err := c.BindJSON(&data); err != nil {
		return
	}

	// Validate S3 backup requirements
	if data.Adapter == backup.S3BackupAdapter && data.DownloadUrl == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "The download_url field is required when the backup adapter is set to S3.",
		})
		return
	}

	// Create job data
	jobData := jobs.BackupRestoreData{
		ServerID:          s.ID(),
		BackupUUID:        backupUuid,
		AdapterType:       string(data.Adapter),
		TruncateDirectory: data.TruncateDirectory,
		DownloadURL:       data.DownloadUrl,
	}

	// Submit job to queue
	jobID := jobQueue.Submit(jobs.JobTypeBackupRestore, jobData)

	c.JSON(http.StatusAccepted, gin.H{
		"job_id": jobID,
		"status": "accepted",
		"message": "Backup restoration job has been queued for processing",
	})
}

// deleteServerBackup initiates a backup deletion operation asynchronously and returns a job ID
func deleteServerBackup(c *gin.Context) {
	s := middleware.ExtractServer(c)
	jobQueue := middleware.ExtractJobQueue(c)
	backupUuid := c.Param("backup")

	var data struct {
		AdapterType string `json:"adapter_type" binding:"required"`
	}
	if err := c.BindJSON(&data); err != nil {
		return
	}

	// Validate adapter type
	switch backup.AdapterType(data.AdapterType) {
	case backup.LocalBackupAdapter, backup.S3BackupAdapter, backup.RusticLocalAdapter, backup.RusticS3Adapter:
		// Valid adapter types
	default:
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "Invalid backup adapter type: " + data.AdapterType,
		})
		return
	}

	// Create job data
	jobData := jobs.BackupDeleteData{
		ServerID:    s.ID(),
		BackupUUID:  backupUuid,
		AdapterType: data.AdapterType,
	}

	// Submit job to queue
	jobID := jobQueue.Submit(jobs.JobTypeBackupDelete, jobData)

	c.JSON(http.StatusAccepted, gin.H{
		"job_id": jobID,
		"status": "accepted",
		"message": "Backup deletion job has been queued for processing",
	})
}

// getJobStatus returns the status of a specific job
func getJobStatus(c *gin.Context) {
	jobQueue := middleware.ExtractJobQueue(c)
	jobID := c.Param("job_id")

	job, exists := jobQueue.GetJob(jobID)
	if !exists {
		c.JSON(http.StatusNotFound, gin.H{
			"error": "Job not found",
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"job_id":     job.ID,
		"type":       job.Type,
		"status":     job.Status,
		"progress":   job.Progress,
		"message":    job.Message,
		"error":      job.Error,
		"created_at": job.CreatedAt,
		"updated_at": job.UpdatedAt,
	})
}

// listJobs returns a list of jobs, optionally filtered by status or type
func listJobs(c *gin.Context) {
	jobQueue := middleware.ExtractJobQueue(c)

	// Parse query parameters
	status := jobs.JobStatus(c.Query("status"))
	jobType := jobs.JobType(c.Query("type"))

	jobList := jobQueue.ListJobs(status, jobType)

	// Convert jobs to response format
	var response []gin.H
	for _, job := range jobList {
		response = append(response, gin.H{
			"job_id":     job.ID,
			"type":       job.Type,
			"status":     job.Status,
			"progress":   job.Progress,
			"message":    job.Message,
			"error":      job.Error,
			"created_at": job.CreatedAt,
			"updated_at": job.UpdatedAt,
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"jobs": response,
	})
}

// cancelJob cancels a running or pending job
func cancelJob(c *gin.Context) {
	jobQueue := middleware.ExtractJobQueue(c)
	jobID := c.Param("job_id")

	success := jobQueue.CancelJob(jobID)
	if !success {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "Job cannot be cancelled (not found or already completed)",
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"message": "Job cancelled successfully",
	})
}