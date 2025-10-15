package router

import (
	"net/http"

	"github.com/apex/log"
	"github.com/gin-gonic/gin"

	"github.com/pyrohost/elytra/src/router/middleware"
	"github.com/pyrohost/elytra/src/server/backup"
)

// postServerBackup initiates a backup operation asynchronously and returns a job ID
func postServerBackup(c *gin.Context) {
	s := middleware.ExtractServer(c)
	manager := middleware.ExtractJobManager(c)

	var data struct {
		Adapter backup.AdapterType `json:"adapter"`
		Uuid    string             `json:"uuid"`
		Ignore  string             `json:"ignore"`
		Name    string             `json:"name"`
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

	// Create job data for our new system
	jobData := map[string]interface{}{
		"server_id":    s.ID(),
		"backup_uuid":  data.Uuid,
		"adapter_type": string(data.Adapter),
		"ignore":       data.Ignore,
		"name":         data.Name,
	}

	// Submit job to manager
	jobID, err := manager.CreateJob("backup_create", jobData)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": "Failed to submit backup job: " + err.Error(),
		})
		return
	}

	c.JSON(http.StatusAccepted, gin.H{
		"job_id": jobID,
		"status": "accepted",
		"message": "Backup job has been queued for processing",
	})
}

// postServerRestoreBackup initiates a backup restoration operation asynchronously and returns a job ID
func postServerRestoreBackup(c *gin.Context) {
	s := middleware.ExtractServer(c)
	manager := middleware.ExtractJobManager(c)
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

	// Create job data for our new system
	jobData := map[string]interface{}{
		"server_id":          s.ID(),
		"backup_uuid":        backupUuid,
		"adapter_type":       string(data.Adapter),
		"truncate_directory": data.TruncateDirectory,
		"download_url":       data.DownloadUrl,
	}

	// Submit job to manager
	jobID, err := manager.CreateJob("backup_restore", jobData)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": "Failed to submit restore job: " + err.Error(),
		})
		return
	}

	c.JSON(http.StatusAccepted, gin.H{
		"job_id": jobID,
		"status": "accepted",
		"message": "Backup restoration job has been queued for processing",
	})
}

// deleteServerBackup initiates a backup deletion operation asynchronously and returns a job ID
func deleteServerBackup(c *gin.Context) {
	s := middleware.ExtractServer(c)
	manager := middleware.ExtractJobManager(c)
	backupUuid := c.Param("backup")

	var data struct {
		AdapterType string `json:"adapter_type" binding:"required"`
		SnapshotId  string `json:"snapshot_id,omitempty"`
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

	// Create job data for our new system
	jobData := map[string]interface{}{
		"server_id":    s.ID(),
		"backup_uuid":  backupUuid,
		"adapter_type": data.AdapterType,
	}

	// Add snapshot_id for rustic backups if provided
	if data.SnapshotId != "" {
		jobData["snapshot_id"] = data.SnapshotId
	}

	// DEBUG: Log what data we received
	log.WithFields(log.Fields{
		"backup_uuid":  backupUuid,
		"adapter_type": data.AdapterType,
		"snapshot_id":  data.SnapshotId,
		"job_data":     jobData,
	}).Info("DEBUG: Router received deletion request")

	// Submit job to manager
	jobID, err := manager.CreateJob("backup_delete", jobData)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": "Failed to submit delete job: " + err.Error(),
		})
		return
	}

	c.JSON(http.StatusAccepted, gin.H{
		"job_id": jobID,
		"status": "accepted",
		"message": "Backup deletion job has been queued for processing",
	})
}
