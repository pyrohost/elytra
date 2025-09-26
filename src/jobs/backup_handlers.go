package jobs

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"emperror.dev/errors"
	"github.com/apex/log"

	"github.com/pyrohost/elytra/src/remote"
	"github.com/pyrohost/elytra/src/server"
	"github.com/pyrohost/elytra/src/server/backup"
)

// BackupJobHandlers contains all backup-related job handlers
type BackupJobHandlers struct {
	serverManager *server.Manager
	client        remote.Client
	jobQueue      *JobQueue
}

// NewBackupJobHandlers creates a new instance of backup job handlers
func NewBackupJobHandlers(manager *server.Manager, client remote.Client, queue *JobQueue) *BackupJobHandlers {
	return &BackupJobHandlers{
		serverManager: manager,
		client:        client,
		jobQueue:      queue,
	}
}

// RegisterAll registers all backup job handlers with the job queue
func (h *BackupJobHandlers) RegisterAll() {
	h.jobQueue.RegisterHandler(JobTypeBackupCreate, h.HandleBackupCreate)
	h.jobQueue.RegisterHandler(JobTypeBackupDelete, h.HandleBackupDelete)
	h.jobQueue.RegisterHandler(JobTypeBackupRestore, h.HandleBackupRestore)
}

// HandleBackupCreate processes backup creation jobs
func (h *BackupJobHandlers) HandleBackupCreate(ctx context.Context, job *Job) error {
	// Parse job data
	var data BackupCreateData
	if err := h.parseJobData(job.Data, &data); err != nil {
		return errors.Wrap(err, "failed to parse backup create job data")
	}

	logger := log.WithFields(log.Fields{
		"job_id":      job.ID,
		"server_id":   data.ServerID,
		"backup_uuid": data.BackupUUID,
		"adapter":     data.AdapterType,
	})

	h.jobQueue.UpdateJob(job.ID, JobStatusRunning, 10, "Locating server...")

	// Get server instance
	s, exists := h.serverManager.Get(data.ServerID)
	if !exists {
		return errors.New("server not found: " + data.ServerID)
	}

	h.jobQueue.UpdateJob(job.ID, JobStatusRunning, 20, "Initializing backup adapter...")

	// Create backup adapter based on type
	var adapter backup.BackupInterface
	switch backup.AdapterType(data.AdapterType) {
	case backup.LocalBackupAdapter:
		adapter = backup.NewLocal(h.client, data.BackupUUID, data.Ignore)
	case backup.S3BackupAdapter:
		adapter = backup.NewS3(h.client, data.BackupUUID, data.Ignore)
	case backup.RusticLocalAdapter:
		rusticConfig, err := h.client.GetServerRusticConfig(ctx, s.ID(), "local")
		if err != nil {
			return errors.Wrap(err, "failed to get server rustic config")
		}
		adapter = backup.NewRusticWithServerPath(h.client, s.ID(), data.BackupUUID, data.Ignore, "local", nil, rusticConfig.RepositoryPassword, rusticConfig.RepositoryPath)
	case backup.RusticS3Adapter:
		rusticConfig, err := h.client.GetServerRusticConfig(ctx, s.ID(), "s3")
		if err != nil {
			return errors.Wrap(err, "failed to get server rustic config")
		}
		adapter = backup.NewRusticWithServerPath(h.client, s.ID(), data.BackupUUID, data.Ignore, "s3", rusticConfig.S3Credentials, rusticConfig.RepositoryPassword, rusticConfig.RepositoryPath)
	default:
		return errors.New("unsupported backup adapter: " + data.AdapterType)
	}

	// Attach context from job data
	if data.Context != nil {
		adapter.WithLogContext(data.Context)
	}

	h.jobQueue.UpdateJob(job.ID, JobStatusRunning, 30, "Starting backup generation...")

	logger.Info("starting backup generation")

	// Create a custom backup function that reports progress
	backupErr := h.performBackupWithProgress(ctx, job.ID, s, adapter)
	if backupErr != nil {
		logger.WithError(backupErr).Error("backup generation failed")
		return errors.Wrap(backupErr, "backup generation failed")
	}

	h.jobQueue.UpdateJob(job.ID, JobStatusRunning, 90, "Finalizing backup...")

	logger.Info("backup generation completed successfully")
	return nil
}

// HandleBackupDelete processes backup deletion jobs
func (h *BackupJobHandlers) HandleBackupDelete(ctx context.Context, job *Job) error {
	// Parse job data
	var data BackupDeleteData
	if err := h.parseJobData(job.Data, &data); err != nil {
		return errors.Wrap(err, "failed to parse backup delete job data")
	}

	logger := log.WithFields(log.Fields{
		"job_id":      job.ID,
		"server_id":   data.ServerID,
		"backup_uuid": data.BackupUUID,
		"adapter":     data.AdapterType,
	})

	h.jobQueue.UpdateJob(job.ID, JobStatusRunning, 10, "Locating server...")

	// Get server instance
	s, exists := h.serverManager.Get(data.ServerID)
	if !exists {
		return errors.New("server not found: " + data.ServerID)
	}

	// Delete backup based on the specific adapter type - no more guessing!
	switch backup.AdapterType(data.AdapterType) {
	case backup.LocalBackupAdapter:
		return h.deleteLocalBackup(ctx, job.ID, data.BackupUUID, logger)

	case backup.S3BackupAdapter:
		return h.deleteS3Backup(ctx, job.ID, data.BackupUUID, logger)

	case backup.RusticLocalAdapter:
		return h.deleteRusticLocalBackup(ctx, job.ID, s, data.BackupUUID, logger)

	case backup.RusticS3Adapter:
		return h.deleteRusticS3Backup(ctx, job.ID, s, data.BackupUUID, logger)

	default:
		return errors.New("unsupported backup adapter type: " + data.AdapterType)
	}
}

// HandleBackupRestore processes backup restoration jobs
func (h *BackupJobHandlers) HandleBackupRestore(ctx context.Context, job *Job) error {
	// Parse job data
	var data BackupRestoreData
	if err := h.parseJobData(job.Data, &data); err != nil {
		return errors.Wrap(err, "failed to parse backup restore job data")
	}

	logger := log.WithFields(log.Fields{
		"job_id":      job.ID,
		"server_id":   data.ServerID,
		"backup_uuid": data.BackupUUID,
		"adapter":     data.AdapterType,
		"truncate":    data.TruncateDirectory,
	})
 
	h.jobQueue.UpdateJob(job.ID, JobStatusRunning, 10, "Locating server...")

	// Get server instance
	s, exists := h.serverManager.Get(data.ServerID)
	if !exists {
		return errors.New("server not found: " + data.ServerID)
	}

	s.SetRestoring(true)
	defer s.SetRestoring(false)

	logger.Info("starting backup restoration")

	h.jobQueue.UpdateJob(job.ID, JobStatusRunning, 20, "Preparing server for restore...")

	if data.TruncateDirectory {
		h.jobQueue.UpdateJob(job.ID, JobStatusRunning, 30, "Truncating server directory...")
		logger.Info("truncating server directory")
		if err := s.Filesystem().TruncateRootDirectory(); err != nil {
			return errors.Wrap(err, "failed to truncate server directory")
		}
	}

	h.jobQueue.UpdateJob(job.ID, JobStatusRunning, 40, "Locating backup...")

	// Handle different backup types
	switch backup.AdapterType(data.AdapterType) {
	case backup.LocalBackupAdapter:
		return h.restoreLocalBackup(ctx, job.ID, s, data.BackupUUID, logger)
	case backup.RusticLocalAdapter:
		return h.restoreRusticBackup(ctx, job.ID, s, data.BackupUUID, "local", logger)
	case backup.RusticS3Adapter:
		return h.restoreRusticBackup(ctx, job.ID, s, data.BackupUUID, "s3", logger)
	case backup.S3BackupAdapter:
		return h.restoreS3Backup(ctx, job.ID, s, data.BackupUUID, data.DownloadURL, logger)
	default:
		return errors.New("unsupported backup adapter for restore: " + data.AdapterType)
	}
}

// performBackupWithProgress performs a backup while reporting progress
func (h *BackupJobHandlers) performBackupWithProgress(ctx context.Context, jobID string, s *server.Server, adapter backup.BackupInterface) error {
	// Get server-wide ignored files
	ignored := adapter.Ignored()
	if ignored == "" {
		h.jobQueue.UpdateJob(jobID, JobStatusRunning, 35, "Reading server ignore patterns...")
		if i, err := s.GetServerwideIgnoredFiles(); err != nil {
			log.WithField("server", s.ID()).WithField("error", err).Warn("failed to get server-wide ignored files")
		} else {
			ignored = i
		}
	}

	h.jobQueue.UpdateJob(jobID, JobStatusRunning, 40, "Generating backup archive...")

	// Generate the backup
	ad, err := adapter.Generate(ctx, s.Filesystem(), ignored)
	if err != nil {
		// Notify panel of failed backup
		if notifyErr := s.NotifyPanelOfBackup(adapter.Identifier(), &backup.ArchiveDetails{}, false); notifyErr != nil {
			log.WithFields(log.Fields{
				"backup": adapter.Identifier(),
				"error":  notifyErr,
			}).Warn("failed to notify panel of failed backup state")
		}

		// Publish failed event
		s.Events().Publish(server.BackupCompletedEvent+":"+adapter.Identifier(), map[string]interface{}{
			"uuid":          adapter.Identifier(),
			"is_successful": false,
			"checksum":      "",
			"checksum_type": "sha1",
			"file_size":     0,
		})

		return err
	}

	h.jobQueue.UpdateJob(jobID, JobStatusRunning, 80, "Notifying panel of backup completion...")

	// Notify panel of successful backup
	if notifyError := s.NotifyPanelOfBackup(adapter.Identifier(), ad, true); notifyError != nil {
		_ = adapter.Remove()
		return errors.Wrap(notifyError, "failed to notify panel of successful backup")
	}

	// Complete job with backup result data
	h.jobQueue.CompleteJob(jobID, map[string]interface{}{
		"checksum":    ad.Checksum,
		"size":        ad.Size,
		"snapshot_id": "", // Will be populated by rustic backups
	})

	// Publish success event
	s.Events().Publish(server.BackupCompletedEvent+":"+adapter.Identifier(), map[string]interface{}{
		"uuid":          adapter.Identifier(),
		"is_successful": true,
		"checksum":      ad.Checksum,
		"checksum_type": "sha1",
		"file_size":     ad.Size,
	})

	return nil
}

// restoreLocalBackup restores a local backup
func (h *BackupJobHandlers) restoreLocalBackup(ctx context.Context, jobID string, s *server.Server, backupUUID string, logger *log.Entry) error {
	h.jobQueue.UpdateJob(jobID, JobStatusRunning, 50, "Locating local backup...")

	b, _, err := backup.LocateLocal(h.client, backupUUID)
	if err != nil {
		return errors.Wrap(err, "failed to locate local backup")
	}

	h.jobQueue.UpdateJob(jobID, JobStatusRunning, 60, "Starting restoration process...")

	logger.Info("starting restoration process for local backup")
	if err := s.RestoreBackup(b, nil); err != nil {
		logger.WithError(err).Error("failed to restore local backup")
		return errors.Wrap(err, "failed to restore local backup")
	}

	h.jobQueue.UpdateJob(jobID, JobStatusRunning, 95, "Publishing completion events...")

	// Complete the restore job
	h.jobQueue.CompleteJob(jobID, map[string]interface{}{
		"restored": true,
		"type":     "local",
	})

	s.Events().Publish(server.DaemonMessageEvent, "Completed server restoration from local backup.")
	s.Events().Publish(server.BackupRestoreCompletedEvent, "")
	logger.Info("completed server restoration from local backup")

	return nil
}

// restoreRusticBackup restores a rustic backup (local or S3)
func (h *BackupJobHandlers) restoreRusticBackup(ctx context.Context, jobID string, s *server.Server, backupUUID string, backupType string, logger *log.Entry) error {
	h.jobQueue.UpdateJob(jobID, JobStatusRunning, 50, "Getting rustic configuration...")

	rusticConfig, err := h.client.GetServerRusticConfig(ctx, s.ID(), backupType)
	if err != nil {
		return errors.Wrap(err, "failed to get server rustic config for restore")
	}

	h.jobQueue.UpdateJob(jobID, JobStatusRunning, 60, "Locating rustic backup...")

	var rusticBackup backup.BackupInterface
	if backupType == "local" {
		rusticBackup, err = backup.LocateRusticWithPath(h.client, s.ID(), backupUUID, "local", nil, rusticConfig.RepositoryPassword, rusticConfig.RepositoryPath)
	} else {
		rusticBackup, err = backup.LocateRusticWithPath(h.client, s.ID(), backupUUID, "s3", rusticConfig.S3Credentials, rusticConfig.RepositoryPassword, rusticConfig.RepositoryPath)
	}
	if err != nil {
		return errors.Wrap(err, "failed to locate rustic backup for restore")
	}

	h.jobQueue.UpdateJob(jobID, JobStatusRunning, 70, "Starting restoration process...")

	logger.Info("starting restoration process for rustic backup")
	if err := s.RestoreBackup(rusticBackup, nil); err != nil {
		logger.WithError(err).Error("failed to restore rustic backup")
		return errors.Wrap(err, "failed to restore rustic backup")
	}

	h.jobQueue.UpdateJob(jobID, JobStatusRunning, 95, "Publishing completion events...")

	s.Events().Publish(server.DaemonMessageEvent, "Completed server restoration from rustic backup.")
	s.Events().Publish(server.BackupRestoreCompletedEvent, "")
	logger.Info("completed server restoration from rustic backup")

	return nil
}

// restoreS3Backup restores an S3 backup
func (h *BackupJobHandlers) restoreS3Backup(ctx context.Context, jobID string, s *server.Server, backupUUID string, downloadURL string, logger *log.Entry) error {
	h.jobQueue.UpdateJob(jobID, JobStatusRunning, 50, "Downloading S3 backup...")

	if downloadURL == "" {
		return errors.New("download URL is required for S3 backup restoration")
	}

	// Create HTTP client with context
	httpClient := &http.Client{
		Timeout: 30 * time.Minute, // Large timeout for backup downloads
	}

	logger.Info("downloading backup from S3 location")
	h.jobQueue.UpdateJob(jobID, JobStatusRunning, 60, "Establishing connection to S3...")

	// Create request with context for cancellation support
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return errors.Wrap(err, "failed to create download request")
	}

	h.jobQueue.UpdateJob(jobID, JobStatusRunning, 70, "Downloading backup archive...")

	res, err := httpClient.Do(req)
	if err != nil {
		return errors.Wrap(err, "failed to download backup")
	}
	defer res.Body.Close()

	// Validate content type
	contentType := res.Header.Get("Content-Type")
	if contentType != "" && !strings.Contains("application/x-gzip application/gzip", contentType) {
		return errors.Errorf("unsupported content type: %s (expected gzip)", contentType)
	}

	h.jobQueue.UpdateJob(jobID, JobStatusRunning, 80, "Starting file restoration...")

	logger.Info("starting restoration process for S3 backup")
	s3Backup := backup.NewS3(h.client, backupUUID, "")
	if err := s.RestoreBackup(s3Backup, res.Body); err != nil {
		logger.WithError(err).Error("failed to restore S3 backup")
		return errors.Wrap(err, "failed to restore S3 backup")
	}

	h.jobQueue.UpdateJob(jobID, JobStatusRunning, 95, "Publishing completion events...")

	s.Events().Publish(server.DaemonMessageEvent, "Completed server restoration from S3 backup.")
	s.Events().Publish(server.BackupRestoreCompletedEvent, "")
	logger.Info("completed server restoration from S3 backup")

	return nil
}

// deleteLocalBackup deletes a local backup
func (h *BackupJobHandlers) deleteLocalBackup(ctx context.Context, jobID string, backupUUID string, logger *log.Entry) error {
	h.jobQueue.UpdateJob(jobID, JobStatusRunning, 30, "Locating local backup...")

	b, _, err := backup.LocateLocal(h.client, backupUUID)
	if err != nil {
		return errors.Wrap(err, "failed to locate local backup")
	}

	h.jobQueue.UpdateJob(jobID, JobStatusRunning, 70, "Deleting local backup...")

	if removeErr := b.Remove(); removeErr != nil {
		return errors.Wrap(removeErr, "failed to delete local backup")
	}

	// Complete the delete job
	h.jobQueue.CompleteJob(jobID, map[string]interface{}{
		"deleted": true,
		"type":    "local",
	})

	logger.Info("successfully deleted local backup")
	return nil
}

// deleteS3Backup deletes an S3 backup
func (h *BackupJobHandlers) deleteS3Backup(ctx context.Context, jobID string, backupUUID string, logger *log.Entry) error {
	h.jobQueue.UpdateJob(jobID, JobStatusRunning, 30, "Deleting S3 backup...")

	s3Backup := backup.NewS3(h.client, backupUUID, "")
	if removeErr := s3Backup.Remove(); removeErr != nil {
		return errors.Wrap(removeErr, "failed to delete S3 backup")
	}

	// Complete the delete job
	h.jobQueue.CompleteJob(jobID, map[string]interface{}{
		"deleted": true,
		"type":    "s3",
	})

	logger.Info("successfully deleted S3 backup")
	return nil
}

// deleteRusticLocalBackup deletes a rustic local backup
func (h *BackupJobHandlers) deleteRusticLocalBackup(ctx context.Context, jobID string, s *server.Server, backupUUID string, logger *log.Entry) error {
	h.jobQueue.UpdateJob(jobID, JobStatusRunning, 30, "Getting rustic local configuration...")

	rusticConfig, err := h.client.GetServerRusticConfig(ctx, s.ID(), "local")
	if err != nil {
		return errors.Wrap(err, "failed to get server rustic config")
	}

	h.jobQueue.UpdateJob(jobID, JobStatusRunning, 50, "Locating rustic local backup...")

	rusticBackup, err := backup.LocateRusticWithPath(h.client, s.ID(), backupUUID, "local", nil, rusticConfig.RepositoryPassword, rusticConfig.RepositoryPath)
	if err != nil {
		return errors.Wrap(err, "failed to locate rustic local backup")
	}

	h.jobQueue.UpdateJob(jobID, JobStatusRunning, 70, "Deleting rustic local backup...")

	if removeErr := rusticBackup.Remove(); removeErr != nil {
		return errors.Wrap(removeErr, "failed to delete rustic local backup")
	}

	logger.Info("successfully deleted rustic local backup")
	return nil
}

// deleteRusticS3Backup deletes a rustic S3 backup
func (h *BackupJobHandlers) deleteRusticS3Backup(ctx context.Context, jobID string, s *server.Server, backupUUID string, logger *log.Entry) error {
	h.jobQueue.UpdateJob(jobID, JobStatusRunning, 30, "Getting rustic S3 configuration...")

	rusticS3Config, err := h.client.GetServerRusticConfig(ctx, s.ID(), "s3")
	if err != nil {
		return errors.Wrap(err, "failed to get server rustic S3 config")
	}

	if rusticS3Config.S3Credentials == nil {
		return errors.New("rustic S3 credentials not configured")
	}

	h.jobQueue.UpdateJob(jobID, JobStatusRunning, 50, "Locating rustic S3 backup...")

	rusticBackup, err := backup.LocateRusticWithPath(h.client, s.ID(), backupUUID, "s3", rusticS3Config.S3Credentials, rusticS3Config.RepositoryPassword, rusticS3Config.RepositoryPath)
	if err != nil {
		return errors.Wrap(err, "failed to locate rustic S3 backup")
	}

	h.jobQueue.UpdateJob(jobID, JobStatusRunning, 70, "Deleting rustic S3 backup...")

	if removeErr := rusticBackup.Remove(); removeErr != nil {
		return errors.Wrap(removeErr, "failed to delete rustic S3 backup")
	}

	logger.Info("successfully deleted rustic S3 backup")
	return nil
}

// parseJobData parses job data from interface{} to the specific type
func (h *BackupJobHandlers) parseJobData(data JobData, target interface{}) error {
	jsonData, err := json.Marshal(data)
	if err != nil {
		return errors.Wrap(err, "failed to marshal job data")
	}

	err = json.Unmarshal(jsonData, target)
	if err != nil {
		return errors.Wrap(err, "failed to unmarshal job data")
	}

	return nil
}