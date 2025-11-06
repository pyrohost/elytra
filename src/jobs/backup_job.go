package jobs

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"emperror.dev/errors"
	"github.com/apex/log"
	"github.com/google/uuid"
	"github.com/pyrohost/elytra/src/remote"
	"github.com/pyrohost/elytra/src/server"
	"github.com/pyrohost/elytra/src/server/backup"
)

// BackupCreateJob handles backup creation operations
type BackupCreateJob struct {
	id            string
	serverID      string
	backupUUID    string
	adapterType   string
	ignore        string
	name          string
	context       map[string]interface{}
	progress      int
	message       string
	serverManager *server.Manager
	client        remote.Client
}

// BackupDeleteJob handles backup deletion operations
type BackupDeleteJob struct {
	id            string
	serverID      string
	backupUUID    string
	snapshotID    string // For rustic backups, use this instead of UUID lookup
	adapterType   string
	progress      int
	message       string
	serverManager *server.Manager
	client        remote.Client
}

// BackupRestoreJob handles backup restoration operations
type BackupRestoreJob struct {
	id                string
	serverID          string
	backupUUID        string
	snapshotID        string // For rustic backups, use this instead of UUID lookup
	adapterType       string
	truncateDirectory bool
	downloadURL       string
	progress          int
	message           string
	serverManager     *server.Manager
	client            remote.Client
}

// NewBackupCreateJob creates a new backup creation job
func NewBackupCreateJob(data map[string]interface{}, serverManager *server.Manager, client remote.Client) (Job, error) {
	serverID, ok := data["server_id"].(string)
	if !ok || serverID == "" {
		return nil, fmt.Errorf("server_id is required")
	}

	backupUUID, ok := data["backup_uuid"].(string)
	if !ok || backupUUID == "" {
		return nil, fmt.Errorf("backup_uuid is required")
	}

	adapterType, _ := data["adapter_type"].(string)
	if adapterType == "" {
		adapterType = "elytra" // default
	}

	ignore, _ := data["ignore"].(string)
	name, _ := data["name"].(string)
	context, _ := data["context"].(map[string]interface{})

	return &BackupCreateJob{
		id:            uuid.New().String(),
		serverID:      serverID,
		backupUUID:    backupUUID,
		adapterType:   adapterType,
		ignore:        ignore,
		name:          name,
		context:       context,
		progress:      0,
		message:       "Backup creation queued",
		serverManager: serverManager,
		client:        client,
	}, nil
}

// NewBackupDeleteJob creates a new backup deletion job
func NewBackupDeleteJob(data map[string]interface{}, serverManager *server.Manager, client remote.Client) (Job, error) {
	serverID, ok := data["server_id"].(string)
	if !ok || serverID == "" {
		return nil, fmt.Errorf("server_id is required")
	}

	backupUUID, ok := data["backup_uuid"].(string)
	if !ok || backupUUID == "" {
		return nil, fmt.Errorf("backup_uuid is required")
	}

	adapterType, _ := data["adapter_type"].(string)
	if adapterType == "" {
		return nil, fmt.Errorf("adapter_type is required for deletion")
	}

	// Extract snapshot_id from data for rustic backups
	snapshotID, _ := data["snapshot_id"].(string)

	return &BackupDeleteJob{
		id:            uuid.New().String(),
		serverID:      serverID,
		backupUUID:    backupUUID,
		snapshotID:    snapshotID,
		adapterType:   adapterType,
		progress:      0,
		message:       "Backup deletion queued",
		serverManager: serverManager,
		client:        client,
	}, nil
}

// NewBackupRestoreJob creates a new backup restoration job
func NewBackupRestoreJob(data map[string]interface{}, serverManager *server.Manager, client remote.Client) (Job, error) {
	serverID, ok := data["server_id"].(string)
	if !ok || serverID == "" {
		return nil, fmt.Errorf("server_id is required")
	}

	backupUUID, ok := data["backup_uuid"].(string)
	if !ok || backupUUID == "" {
		return nil, fmt.Errorf("backup_uuid is required")
	}

	adapterType, _ := data["adapter_type"].(string)
	if adapterType == "" {
		return nil, fmt.Errorf("adapter_type is required for restoration")
	}

	truncateDirectory, _ := data["truncate_directory"].(bool)
	downloadURL, _ := data["download_url"].(string)

	// Extract snapshot_id from data for rustic backups
	snapshotID, _ := data["snapshot_id"].(string)

	return &BackupRestoreJob{
		id:                uuid.New().String(),
		serverID:          serverID,
		backupUUID:        backupUUID,
		snapshotID:        snapshotID,
		adapterType:       adapterType,
		truncateDirectory: truncateDirectory,
		downloadURL:       downloadURL,
		progress:          0,
		message:           "Backup restoration queued",
		serverManager:     serverManager,
		client:            client,
	}, nil
}

// BackupCreateJob implementation
func (j *BackupCreateJob) GetID() string      { return j.id }
func (j *BackupCreateJob) GetType() string    { return "backup_create" }
func (j *BackupCreateJob) GetProgress() int   { return j.progress }
func (j *BackupCreateJob) GetMessage() string { return j.message }

// WebSocketJob interface implementation for real-time backup status updates
func (j *BackupCreateJob) GetServerID() string           { return j.serverID }
func (j *BackupCreateJob) GetWebSocketEventType() string { return "backup.status" }
func (j *BackupCreateJob) GetWebSocketEventData() map[string]interface{} {
	return map[string]interface{}{
		"backup_uuid": j.backupUUID,
		"operation":   "create",
		"adapter":     j.adapterType,
		"name":        j.name,
	}
}

func (j *BackupCreateJob) Validate() error {
	if j.serverID == "" {
		return fmt.Errorf("server_id is required")
	}
	if j.backupUUID == "" {
		return fmt.Errorf("backup_uuid is required")
	}
	return nil
}

func (j *BackupCreateJob) Execute(ctx context.Context, reporter ProgressReporter) (interface{}, error) {
	logger := log.WithFields(log.Fields{
		"job_id":      j.id,
		"server_id":   j.serverID,
		"backup_uuid": j.backupUUID,
		"adapter":     j.adapterType,
	})

	reporter.ReportProgress(10, "Locating server...")

	s, exists := j.serverManager.Get(j.serverID)
	if !exists {
		return nil, errors.New("server not found: " + j.serverID)
	}

	reporter.ReportProgress(20, "Initializing backup adapter...")

	var adapter backup.BackupInterface
	switch backup.AdapterType(j.adapterType) {
	case backup.LocalBackupAdapter:
		adapter = backup.NewLocal(j.client, j.backupUUID, j.ignore)
	case backup.S3BackupAdapter:
		adapter = backup.NewS3(j.client, j.backupUUID, j.ignore)
	case backup.RusticLocalAdapter:
		rusticConfig, err := j.client.GetServerRusticConfig(ctx, s.ID(), "local")
		if err != nil {
			return nil, errors.Wrap(err, "failed to get server rustic config")
		}
		adapter = backup.NewRusticWithServerPath(j.client, s.ID(), j.backupUUID, j.ignore, "local", nil, rusticConfig.RepositoryPassword, rusticConfig.RepositoryPath)
	case backup.RusticS3Adapter:
		rusticConfig, err := j.client.GetServerRusticConfig(ctx, s.ID(), "s3")
		if err != nil {
			return nil, errors.Wrap(err, "failed to get server rustic config")
		}
		adapter = backup.NewRusticWithServerPath(j.client, s.ID(), j.backupUUID, j.ignore, "s3", rusticConfig.S3Credentials, rusticConfig.RepositoryPassword, rusticConfig.RepositoryPath)
	default:
		return nil, errors.New("unsupported backup adapter: " + j.adapterType)
	}

	if j.context != nil {
		adapter.WithLogContext(j.context)
	}

	reporter.ReportProgress(30, "Starting backup generation...")

	logger.Info("starting backup generation")

	result, err := j.performBackupWithProgress(ctx, reporter, s, adapter)
	if err != nil {
		logger.WithError(err).Error("backup generation failed")
		return nil, errors.Wrap(err, "backup generation failed")
	}

	// Don't report progress 100 here - let the job manager do it with the result
	// This prevents a race condition where progress 100 is sent before the result is ready

	logger.Info("backup generation completed successfully")
	return result, nil
}

// performBackupWithProgress performs a backup while reporting progress
func (j *BackupCreateJob) performBackupWithProgress(ctx context.Context, reporter ProgressReporter, s *server.Server, adapter backup.BackupInterface) (map[string]interface{}, error) {
	ignored := adapter.Ignored()
	if ignored == "" {
		reporter.ReportProgress(35, "Reading server ignore patterns...")
		if i, err := s.GetServerwideIgnoredFiles(); err != nil {
			log.WithField("server", s.ID()).WithField("error", err).Warn("failed to get server-wide ignored files")
		} else {
			ignored = i
		}
	}

	reporter.ReportProgress(40, "Generating backup archive...")

	ad, err := adapter.Generate(ctx, s.Filesystem(), ignored)
	if err != nil {
		return nil, err
	}

	reporter.ReportProgress(80, "Finalizing backup...")

	result := map[string]interface{}{
		"checksum":    ad.Checksum,
		"snapshot_id": ad.SnapshotId,
		"adapter":     j.adapterType,
		"successful":  true,
		"size":        ad.Size, // Always include individual backup size (DataAdded for rustic, full size for legacy)
	}

	// For rustic backups, also return total repository size
	adapterType := backup.AdapterType(j.adapterType)

	if adapterType == backup.RusticLocalAdapter || adapterType == backup.RusticS3Adapter {
		reporter.ReportProgress(90, "Calculating repository size...")

		rusticBackup, ok := adapter.(*backup.RusticBackup)

		if ok {
			// Close the rustic backup after we're done (Generate() no longer closes it)
			defer rusticBackup.Close()

			repository := rusticBackup.GetRepository()

			if repository != nil {
				repoSize, sizeErr := repository.GetRepositorySize(ctx)
				if sizeErr != nil {
					log.WithError(sizeErr).Warn("Failed to get repository size after backup creation")
					result["repository_size_error"] = sizeErr.Error()
				} else {
					result["repository_size"] = repoSize
					log.WithField("repository_size_bytes", repoSize).Info("Repository size retrieved after backup creation")
				}
			} else {
				log.Warn("Repository was nil after backup generation")
			}
		} else {
			log.Warn("Failed to cast adapter to RusticBackup")
		}
	}

	return result, nil
}

// BackupDeleteJob implementation
func (j *BackupDeleteJob) GetID() string      { return j.id }
func (j *BackupDeleteJob) GetType() string    { return "backup_delete" }
func (j *BackupDeleteJob) GetProgress() int   { return j.progress }
func (j *BackupDeleteJob) GetMessage() string { return j.message }

// WebSocketJob interface implementation for real-time backup status updates
func (j *BackupDeleteJob) GetServerID() string           { return j.serverID }
func (j *BackupDeleteJob) GetWebSocketEventType() string { return "backup.status" }
func (j *BackupDeleteJob) GetWebSocketEventData() map[string]interface{} {
	return map[string]interface{}{
		"backup_uuid": j.backupUUID,
		"operation":   "delete",
		"adapter":     j.adapterType,
	}
}

func (j *BackupDeleteJob) Validate() error {
	if j.serverID == "" {
		return fmt.Errorf("server_id is required")
	}
	if j.backupUUID == "" {
		return fmt.Errorf("backup_uuid is required")
	}
	if j.adapterType == "" {
		return fmt.Errorf("adapter_type is required")
	}
	return nil
}

func (j *BackupDeleteJob) Execute(ctx context.Context, reporter ProgressReporter) (interface{}, error) {
	logger := log.WithFields(log.Fields{
		"job_id":      j.id,
		"server_id":   j.serverID,
		"backup_uuid": j.backupUUID,
		"adapter":     j.adapterType,
	})

	reporter.ReportProgress(10, "Locating server...")

	s, exists := j.serverManager.Get(j.serverID)
	if !exists {
		return nil, errors.New("server not found: " + j.serverID)
	}

	logger.Info("executing backup delete job")

	switch backup.AdapterType(j.adapterType) {
	case backup.LocalBackupAdapter:
		return j.deleteLocalBackup(ctx, reporter, logger)

	case backup.S3BackupAdapter:
		return j.deleteS3Backup(ctx, reporter, logger)

	case backup.RusticLocalAdapter:
		return j.deleteRusticLocalBackup(ctx, reporter, s, logger)

	case backup.RusticS3Adapter:
		return j.deleteRusticS3Backup(ctx, reporter, s, logger)

	default:
		return nil, errors.New("unsupported backup adapter type: " + j.adapterType)
	}
}

// deleteLocalBackup deletes a local backup
func (j *BackupDeleteJob) deleteLocalBackup(ctx context.Context, reporter ProgressReporter, logger *log.Entry) (map[string]interface{}, error) {
	reporter.ReportProgress(30, "Locating local backup...")

	b, _, err := backup.LocateLocal(j.client, j.backupUUID)
	if err != nil {
		return nil, errors.Wrap(err, "failed to locate local backup")
	}

	reporter.ReportProgress(70, "Deleting local backup...")

	if removeErr := b.Remove(); removeErr != nil {
		return nil, errors.Wrap(removeErr, "failed to delete local backup")
	}

	reporter.ReportProgress(100, "Backup deleted successfully")

	logger.Info("successfully deleted local backup")
	return map[string]interface{}{
		"deleted":    true,
		"type":       "local",
		"successful": true,
	}, nil
}

// deleteS3Backup deletes an S3 backup
func (j *BackupDeleteJob) deleteS3Backup(ctx context.Context, reporter ProgressReporter, logger *log.Entry) (map[string]interface{}, error) {
	reporter.ReportProgress(30, "Deleting S3 backup...")

	s3Backup := backup.NewS3(j.client, j.backupUUID, "")
	if removeErr := s3Backup.Remove(); removeErr != nil {
		return nil, errors.Wrap(removeErr, "failed to delete S3 backup")
	}

	reporter.ReportProgress(100, "Backup deleted successfully")

	logger.Info("successfully deleted S3 backup")
	return map[string]interface{}{
		"deleted":    true,
		"type":       "s3",
		"successful": true,
	}, nil
}

// deleteRusticLocalBackup deletes a rustic local backup
func (j *BackupDeleteJob) deleteRusticLocalBackup(ctx context.Context, reporter ProgressReporter, s *server.Server, logger *log.Entry) (map[string]interface{}, error) {
	reporter.ReportProgress(30, "Getting rustic local configuration...")

	rusticConfig, err := j.client.GetServerRusticConfig(ctx, s.ID(), "local")
	if err != nil {
		return nil, errors.Wrap(err, "failed to get server rustic config")
	}

	reporter.ReportProgress(50, "Locating rustic local backup...")

	rusticBackup, err := backup.LocateRusticBySnapshotID(j.client, s.ID(), j.snapshotID, "local", nil, rusticConfig.RepositoryPassword, rusticConfig.RepositoryPath)
	if err != nil {
		logger.WithError(err).Warn("LocateRusticBySnapshotID failed - snapshot may already be deleted")

		// Check if this is a "snapshot not found" error (idempotent deletion)
		if strings.Contains(err.Error(), "failed to get snapshot") || strings.Contains(err.Error(), "exit status 1") {
			logger.Info("Snapshot already deleted - treating as successful deletion for idempotent cleanup")

			// Still need to calculate repository size even though snapshot was already deleted
			reporter.ReportProgress(90, "Calculating repository size...")

			result := map[string]interface{}{
				"deleted":         true,
				"type":            "rustic_local",
				"successful":      true,
				"already_deleted": true,
			}

			// Create repository directly to calculate size
			config := backup.Config{
				ServerUUID: s.ID(),
				Password:   rusticConfig.RepositoryPassword,
				BackupType: "local",
				LocalPath:  rusticConfig.RepositoryPath,
			}

			repository, repoErr := backup.NewLocalRepository(config)
			if repoErr == nil && repository != nil {
				defer repository.Close()

				sizeCalculationCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
				defer cancel()

				repoSize, sizeErr := repository.GetRepositorySize(sizeCalculationCtx)
				if sizeErr != nil {
					logger.WithError(sizeErr).Warn("Failed to get repository size after already-deleted snapshot")
					result["repository_size_error"] = sizeErr.Error()
				} else {
					result["repository_size"] = repoSize
					logger.WithField("repository_size_bytes", repoSize).Info("Repository size retrieved after already-deleted snapshot")
				}
			} else if repoErr != nil {
				logger.WithError(repoErr).Warn("Failed to create repository for size calculation")
			}

			reporter.ReportProgress(100, "Backup already deleted - cleanup successful")
			return result, nil
		}

		return nil, errors.Wrap(err, "failed to locate rustic local backup")
	}

	// Ensure cleanup happens on completion or failure
	defer rusticBackup.Close()

	// Get the repository reference - no longer need to capture before Remove() since we don't close early
	repository := rusticBackup.GetRepository()

	reporter.ReportProgress(70, "Deleting rustic local backup...")

	if removeErr := rusticBackup.Remove(); removeErr != nil {
		return nil, errors.Wrap(removeErr, "failed to delete rustic local backup")
	}

	reporter.ReportProgress(90, "Calculating repository size...")

	// Get repository size after deletion
	logger.Info("retrieving repository size after local deletion")

	result := map[string]interface{}{
		"deleted":    true,
		"type":       "rustic_local",
		"successful": true,
	}

	if repository != nil {
		sizeCalculationCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()

		repoSize, sizeErr := repository.GetRepositorySize(sizeCalculationCtx)
		if sizeErr != nil {
			logger.WithError(sizeErr).Warn("Failed to get repository size after local deletion")
			result["repository_size_error"] = sizeErr.Error()
		} else {
			result["repository_size"] = repoSize
			logger.WithField("repository_size_bytes", repoSize).Info("Repository size retrieved after local deletion")
		}
	} else {
		logger.Warn("repository was nil after Remove() call - skipping size calculation")
	}

	reporter.ReportProgress(100, "Backup deleted successfully")

	logger.Info("successfully deleted rustic local backup")
	return result, nil
}

// deleteRusticS3Backup deletes a rustic S3 backup
func (j *BackupDeleteJob) deleteRusticS3Backup(ctx context.Context, reporter ProgressReporter, s *server.Server, logger *log.Entry) (map[string]interface{}, error) {
	reporter.ReportProgress(30, "Getting rustic S3 configuration...")

	rusticS3Config, err := j.client.GetServerRusticConfig(ctx, s.ID(), "s3")
	if err != nil {
		logger.WithError(err).Error("Failed to get server rustic config")
		return nil, errors.Wrap(err, "failed to get server rustic S3 config")
	}

	if rusticS3Config.S3Credentials == nil {
		logger.Error("rustic S3 credentials not configured")
		return nil, errors.New("rustic S3 credentials not configured")
	}

	reporter.ReportProgress(50, "Locating rustic S3 backup...")

	rusticBackup, err := backup.LocateRusticBySnapshotID(j.client, s.ID(), j.snapshotID, "s3", rusticS3Config.S3Credentials, rusticS3Config.RepositoryPassword, rusticS3Config.RepositoryPath)
	if err != nil {
		logger.WithError(err).Warn("Failed to locate rustic backup - snapshot may already be deleted")

		// Check if this is a "snapshot not found" error (idempotent deletion)
		if strings.Contains(err.Error(), "failed to get snapshot") || strings.Contains(err.Error(), "exit status 1") {
			logger.Info("Snapshot already deleted - treating as successful deletion for idempotent cleanup")

			// Still need to calculate repository size even though snapshot was already deleted
			reporter.ReportProgress(90, "Calculating repository size...")

			result := map[string]interface{}{
				"deleted":         true,
				"type":            "rustic_s3",
				"successful":      true,
				"already_deleted": true,
			}

			// Create repository directly to calculate size
			config := backup.Config{
				ServerUUID: s.ID(),
				Password:   rusticS3Config.RepositoryPassword,
				BackupType: "s3",
				S3Config: &backup.S3Config{
					Bucket:          rusticS3Config.S3Credentials.Bucket,
					Region:          rusticS3Config.S3Credentials.Region,
					Endpoint:        rusticS3Config.S3Credentials.Endpoint,
					AccessKeyID:     rusticS3Config.S3Credentials.AccessKeyID,
					SecretAccessKey: rusticS3Config.S3Credentials.SecretAccessKey,
					SessionToken:    rusticS3Config.S3Credentials.SessionToken,
					ForcePathStyle:  rusticS3Config.S3Credentials.ForcePathStyle,
				},
			}

			repository, repoErr := backup.NewS3Repository(config)
			if repoErr == nil && repository != nil {
				defer repository.Close()

				sizeCalculationCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
				defer cancel()

				repoSize, sizeErr := repository.GetRepositorySize(sizeCalculationCtx)
				if sizeErr != nil {
					logger.WithError(sizeErr).Warn("Failed to get repository size after already-deleted snapshot")
					result["repository_size_error"] = sizeErr.Error()
				} else {
					result["repository_size"] = repoSize
					logger.WithField("repository_size_bytes", repoSize).Info("Repository size retrieved after already-deleted snapshot")
				}
			} else if repoErr != nil {
				logger.WithError(repoErr).Warn("Failed to create repository for size calculation")
			}

			reporter.ReportProgress(100, "Backup already deleted - cleanup successful")
			return result, nil
		}

		return nil, errors.Wrap(err, "failed to locate rustic S3 backup")
	}

	// Ensure cleanup happens on completion or failure
	defer rusticBackup.Close()

	// Get the repository reference - no longer need to capture before Remove() since we don't close early
	repository := rusticBackup.GetRepository()

	reporter.ReportProgress(70, "Deleting rustic S3 backup...")

	if removeErr := rusticBackup.Remove(); removeErr != nil {
		logger.WithError(removeErr).Error("Failed to remove rustic S3 backup")
		return nil, errors.Wrap(removeErr, "failed to delete rustic S3 backup")
	}

	reporter.ReportProgress(90, "Calculating repository size...")

	result := map[string]interface{}{
		"deleted":    true,
		"type":       "rustic_s3",
		"successful": true,
	}

	if repository != nil {
		sizeCalculationCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()

		repoSize, sizeErr := repository.GetRepositorySize(sizeCalculationCtx)
		if sizeErr != nil {
			logger.WithError(sizeErr).Warn("Failed to get repository size after S3 deletion")
			result["repository_size_error"] = sizeErr.Error()
		} else {
			result["repository_size"] = repoSize
			logger.WithField("repository_size_bytes", repoSize).Info("Repository size retrieved after S3 deletion")
		}
	} else {
		logger.Warn("repository was nil after Remove() call - skipping size calculation")
	}

	logger.Info("successfully deleted rustic S3 backup")
	return result, nil
}

// BackupRestoreJob implementation
func (j *BackupRestoreJob) GetID() string      { return j.id }
func (j *BackupRestoreJob) GetType() string    { return "backup_restore" }
func (j *BackupRestoreJob) GetProgress() int   { return j.progress }
func (j *BackupRestoreJob) GetMessage() string { return j.message }

// WebSocketJob interface implementation for real-time backup status updates
func (j *BackupRestoreJob) GetServerID() string           { return j.serverID }
func (j *BackupRestoreJob) GetWebSocketEventType() string { return "backup.status" }
func (j *BackupRestoreJob) GetWebSocketEventData() map[string]interface{} {
	return map[string]interface{}{
		"backup_uuid":        j.backupUUID,
		"operation":          "restore",
		"adapter":            j.adapterType,
		"truncate_directory": j.truncateDirectory,
	}
}

func (j *BackupRestoreJob) Validate() error {
	if j.serverID == "" {
		return fmt.Errorf("server_id is required")
	}
	if j.backupUUID == "" {
		return fmt.Errorf("backup_uuid is required")
	}
	if j.adapterType == "" {
		return fmt.Errorf("adapter_type is required")
	}
	return nil
}

func (j *BackupRestoreJob) Execute(ctx context.Context, reporter ProgressReporter) (interface{}, error) {
	logger := log.WithFields(log.Fields{
		"job_id":      j.id,
		"server_id":   j.serverID,
		"backup_uuid": j.backupUUID,
		"adapter":     j.adapterType,
		"truncate":    j.truncateDirectory,
	})

	reporter.ReportProgress(10, "Preparing for backup restoration...")

	s, exists := j.serverManager.Get(j.serverID)
	if !exists {
		return nil, errors.New("server not found: " + j.serverID)
	}

	s.SetRestoring(true)
	defer s.SetRestoring(false)

	logger.Info("starting backup restoration")

	reporter.ReportProgress(20, "Preparing server for restore...")

	if j.truncateDirectory {
		reporter.ReportProgress(30, "Truncating server directory...")
		logger.Info("truncating server directory")

		// Create a timeout context for directory truncation (can take a while with many files)
		truncateCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		defer cancel()

		// Use a channel to handle truncation with timeout
		type truncateResult struct {
			err error
		}

		resultChan := make(chan truncateResult, 1)

		go func() {
			err := s.Filesystem().TruncateRootDirectory()
			resultChan <- truncateResult{err: err}
		}()

		// Wait for either completion or timeout
		select {
		case result := <-resultChan:
			if result.err != nil {
				return nil, errors.Wrap(result.err, "failed to truncate server directory")
			}
		case <-truncateCtx.Done():
			logger.Error("Directory truncation timed out after 10 minutes")
			return nil, errors.New("directory truncation timed out after 10 minutes")
		}

		logger.Info("server directory truncation completed")
	}

	reporter.ReportProgress(40, "Locating backup...")
	logger.WithField("adapter_type", j.adapterType).Info("determining backup adapter type for restoration")

	switch backup.AdapterType(j.adapterType) {
	case backup.LocalBackupAdapter:
		logger.Info("calling restoreLocalBackup")
		return j.restoreLocalBackup(ctx, reporter, s, logger)
	case backup.RusticLocalAdapter:
		logger.Info("calling restoreRusticBackup with local adapter")
		return j.restoreRusticBackup(ctx, reporter, s, "local", logger)
	case backup.RusticS3Adapter:
		logger.Info("calling restoreRusticBackup with s3 adapter")
		return j.restoreRusticBackup(ctx, reporter, s, "s3", logger)
	case backup.S3BackupAdapter:
		logger.Info("calling restoreS3Backup")
		return j.restoreS3Backup(ctx, reporter, s, logger)
	default:
		logger.WithField("adapter_type", j.adapterType).Error("unsupported backup adapter type")
		return nil, errors.New("unsupported backup adapter for restore: " + j.adapterType)
	}
}

// restoreLocalBackup restores a local backup
func (j *BackupRestoreJob) restoreLocalBackup(ctx context.Context, reporter ProgressReporter, s *server.Server, logger *log.Entry) (map[string]interface{}, error) {
	reporter.ReportProgress(50, "Locating local backup...")

	b, _, err := backup.LocateLocal(j.client, j.backupUUID)
	if err != nil {
		return nil, errors.Wrap(err, "failed to locate local backup")
	}

	reporter.ReportProgress(60, "Starting restoration process...")

	logger.Info("starting restoration process for local backup")
	if err := s.RestoreBackup(b, nil); err != nil {
		logger.WithError(err).Error("failed to restore local backup")
		return nil, errors.Wrap(err, "failed to restore local backup")
	}

	reporter.ReportProgress(95, "Publishing completion events...")

	s.Events().Publish(server.DaemonMessageEvent, "Completed server restoration from local backup.")
	s.Events().Publish(server.BackupRestoreCompletedEvent, "")

	reporter.ReportProgress(100, "Backup restored successfully")

	logger.Info("completed server restoration from local backup")

	return map[string]interface{}{
		"restored":   true,
		"type":       "local",
		"successful": true,
	}, nil
}

// restoreRusticBackup restores a rustic backup (local or S3)
func (j *BackupRestoreJob) restoreRusticBackup(ctx context.Context, reporter ProgressReporter, s *server.Server, backupType string, logger *log.Entry) (map[string]interface{}, error) {
	reporter.ReportProgress(50, "Getting rustic configuration...")

	rusticConfig, err := j.client.GetServerRusticConfig(ctx, s.ID(), backupType)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get server rustic config for restore")
	}

	reporter.ReportProgress(60, "Locating rustic backup...")

	var rusticBackup backup.BackupInterface
	if backupType == "local" {
		rusticBackup, err = backup.LocateRusticBySnapshotID(j.client, s.ID(), j.snapshotID, "local", nil, rusticConfig.RepositoryPassword, rusticConfig.RepositoryPath)
	} else {
		rusticBackup, err = backup.LocateRusticBySnapshotID(j.client, s.ID(), j.snapshotID, "s3", rusticConfig.S3Credentials, rusticConfig.RepositoryPassword, rusticConfig.RepositoryPath)
	}
	if err != nil {
		return nil, errors.Wrap(err, "failed to locate rustic backup for restore")
	}

	reporter.ReportProgress(70, "Starting restoration process...")

	logger.Info("starting restoration process for rustic backup")
	if err := s.RestoreBackup(rusticBackup, nil); err != nil {
		logger.WithError(err).Error("failed to restore rustic backup")
		return nil, errors.Wrap(err, "failed to restore rustic backup")
	}

	reporter.ReportProgress(95, "Publishing completion events...")

	s.Events().Publish(server.DaemonMessageEvent, "Completed server restoration from rustic backup.")
	s.Events().Publish(server.BackupRestoreCompletedEvent, "")

	reporter.ReportProgress(100, "Backup restored successfully")

	logger.Info("completed server restoration from rustic backup")

	return map[string]interface{}{
		"restored":   true,
		"type":       "rustic_" + backupType,
		"successful": true,
	}, nil
}

// restoreS3Backup restores an S3 backup
func (j *BackupRestoreJob) restoreS3Backup(ctx context.Context, reporter ProgressReporter, s *server.Server, logger *log.Entry) (map[string]interface{}, error) {
	logger.WithField("download_url", j.downloadURL).Info("starting S3 backup restoration")
	reporter.ReportProgress(50, "Downloading S3 backup...")

	if j.downloadURL == "" {
		logger.Error("download URL is empty for S3 backup restoration")
		return nil, errors.New("download URL is required for S3 backup restoration")
	}

	logger.Info("download URL validation passed, proceeding with S3 restoration")

	// Create HTTP client with context
	httpClient := &http.Client{
		Timeout: 30 * time.Minute, // Large timeout for backup downloads
	}

	logger.Info("downloading backup from S3 location")
	reporter.ReportProgress(60, "Establishing connection to S3...")

	// Create request with context for cancellation support
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, j.downloadURL, nil)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create download request")
	}

	reporter.ReportProgress(70, "Downloading backup archive...")

	res, err := httpClient.Do(req)
	if err != nil {
		return nil, errors.Wrap(err, "failed to download backup")
	}
	defer res.Body.Close()

	// Validate HTTP response status
	if res.StatusCode != http.StatusOK {
		return nil, errors.Errorf("failed to download backup: HTTP %d %s", res.StatusCode, res.Status)
	}

	// Validate content type
	contentType := res.Header.Get("Content-Type")
	if contentType != "" && !strings.Contains(contentType, "application/x-gzip") && !strings.Contains(contentType, "application/gzip") && !strings.Contains(contentType, "application/octet-stream") {
		return nil, errors.Errorf("unsupported content type: %s (expected gzip)", contentType)
	}

	reporter.ReportProgress(80, "Starting file restoration...")

	logger.Info("starting restoration process for S3 backup")

	// Log server state before restoration
	serverState := s.Environment.State()
	logger.WithField("server_state", serverState).Info("server state before restoration")

	// Create a timeout context for the restoration operation
	restoreCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()

	// Use a channel to handle the restoration with timeout
	type restoreResult struct {
		err error
	}

	resultChan := make(chan restoreResult, 1)

	go func() {
		s3Backup := backup.NewS3(j.client, j.backupUUID, "")

		// Create a context-aware restoration by monitoring the context
		err := func() error {
			// Check if context is cancelled before starting
			select {
			case <-restoreCtx.Done():
				logger.Info("restoration context cancelled before starting")
				return restoreCtx.Err()
			default:
			}

			logger.Info("calling RestoreBackup with S3 data stream")

			// Perform the restoration
			return s.RestoreBackup(s3Backup, res.Body)
		}()

		logger.WithError(err).Info("RestoreBackup completed")
		resultChan <- restoreResult{err: err}
	}()

	// Wait for either completion or timeout
	select {
	case result := <-resultChan:
		if result.err != nil {
			logger.WithError(result.err).Error("failed to restore S3 backup")
			return nil, errors.Wrap(result.err, "failed to restore S3 backup")
		}
		logger.Info("S3 backup restoration completed successfully")
	case <-restoreCtx.Done():
		logger.Error("S3 backup restoration timed out after 30 minutes")
		return nil, errors.New("S3 backup restoration timed out after 30 minutes")
	}

	reporter.ReportProgress(95, "Publishing completion events...")

	s.Events().Publish(server.DaemonMessageEvent, "Completed server restoration from S3 backup.")
	s.Events().Publish(server.BackupRestoreCompletedEvent, "")

	reporter.ReportProgress(100, "Backup restored successfully")

	logger.Info("completed server restoration from S3 backup")

	return map[string]interface{}{
		"restored":   true,
		"type":       "s3",
		"successful": true,
	}, nil
}

// BackupDeleteAllJob handles deletion of all backups and repository destruction
type BackupDeleteAllJob struct {
	id            string
	serverID      string
	backups       []map[string]interface{}
	progress      int
	message       string
	serverManager *server.Manager
	client        remote.Client
}

// NewBackupDeleteAllJob creates a new delete all backups job
func NewBackupDeleteAllJob(data map[string]interface{}, serverManager *server.Manager, client remote.Client) (Job, error) {
	serverID, ok := data["server_id"].(string)
	if !ok || serverID == "" {
		return nil, fmt.Errorf("server_id is required")
	}

	// Parse backups list (optional - may be empty if no backups exist)
	var backups []map[string]interface{}
	if backupsData, ok := data["backups"].([]interface{}); ok {
		for _, b := range backupsData {
			if backupMap, ok := b.(map[string]interface{}); ok {
				backups = append(backups, backupMap)
			}
		}
	}

	return &BackupDeleteAllJob{
		id:            uuid.New().String(),
		serverID:      serverID,
		backups:       backups,
		progress:      0,
		message:       "Delete all backups queued",
		serverManager: serverManager,
		client:        client,
	}, nil
}

// Job interface implementation
func (j *BackupDeleteAllJob) GetID() string      { return j.id }
func (j *BackupDeleteAllJob) GetType() string    { return "backup_delete_all" }
func (j *BackupDeleteAllJob) GetProgress() int   { return j.progress }
func (j *BackupDeleteAllJob) GetMessage() string { return j.message }

// WebSocketJob interface implementation
func (j *BackupDeleteAllJob) GetServerID() string           { return j.serverID }
func (j *BackupDeleteAllJob) GetWebSocketEventType() string { return "backup.status" }
func (j *BackupDeleteAllJob) GetWebSocketEventData() map[string]interface{} {
	return map[string]interface{}{
		"operation": "delete_all",
	}
}

func (j *BackupDeleteAllJob) Validate() error {
	if j.serverID == "" {
		return fmt.Errorf("server_id is required")
	}
	return nil
}

func (j *BackupDeleteAllJob) Execute(ctx context.Context, reporter ProgressReporter) (interface{}, error) {
	logger := log.WithFields(log.Fields{
		"job_id":       j.id,
		"server_id":    j.serverID,
		"backup_count": len(j.backups),
	})

	reporter.ReportProgress(5, "Locating server...")

	s, exists := j.serverManager.Get(j.serverID)
	if !exists {
		return nil, errors.New("server not found: " + j.serverID)
	}

	deletedCount := 0
	failedCount := 0

	// Delete individual backup files first
	if len(j.backups) > 0 {
		reporter.ReportProgress(10, fmt.Sprintf("Deleting %d backup files...", len(j.backups)))
		logger.WithField("backup_count", len(j.backups)).Info("starting individual backup deletion")

		for i, backupData := range j.backups {
			backupUUID, _ := backupData["uuid"].(string)
			adapter, _ := backupData["adapter"].(string)
			snapshotID, _ := backupData["snapshot_id"].(string)
			checksum, _ := backupData["checksum"].(string)

			progress := 10 + (40 * (i + 1) / len(j.backups))
			reporter.ReportProgress(progress, fmt.Sprintf("Deleting backup %d of %d...", i+1, len(j.backups)))

			backupLogger := logger.WithFields(log.Fields{
				"backup_uuid": backupUUID,
				"adapter":     adapter,
			})

			// Delete based on adapter type
			var deleteErr error
			adapterType := backup.AdapterType(adapter)

			// Handle local backup types (elytra, wings, or local)
			if adapterType == backup.LocalBackupAdapter || adapter == "wings" || adapter == "elytra" {
				b, _, err := backup.LocateLocal(j.client, backupUUID)
				if err != nil {
					deleteErr = errors.Wrap(err, "failed to locate local backup")
				} else {
					deleteErr = b.Remove()
				}
			} else {
				switch adapterType {
				case backup.S3BackupAdapter:
					s3Backup := backup.NewS3(j.client, backupUUID, checksum)
					deleteErr = s3Backup.Remove()

				case backup.RusticLocalAdapter:
					if snapshotID != "" {
						rusticConfig, err := j.client.GetServerRusticConfig(ctx, s.ID(), "local")
						if err != nil {
							deleteErr = errors.Wrap(err, "failed to get rustic local config")
						} else {
							rusticBackup, err := backup.LocateRusticBySnapshotID(j.client, s.ID(), snapshotID, "local", nil, rusticConfig.RepositoryPassword, rusticConfig.RepositoryPath)
							if err != nil {
								deleteErr = errors.Wrap(err, "failed to locate rustic local backup")
							} else {
								deleteErr = rusticBackup.Remove()
								rusticBackup.Close()
							}
						}
					}

				case backup.RusticS3Adapter:
					if snapshotID != "" {
						rusticS3Config, err := j.client.GetServerRusticConfig(ctx, s.ID(), "s3")
						if err != nil {
							deleteErr = errors.Wrap(err, "failed to get rustic S3 config")
						} else if rusticS3Config.S3Credentials != nil {
							rusticBackup, err := backup.LocateRusticBySnapshotID(j.client, s.ID(), snapshotID, "s3", rusticS3Config.S3Credentials, rusticS3Config.RepositoryPassword, "")
							if err != nil {
								deleteErr = errors.Wrap(err, "failed to locate rustic S3 backup")
							} else {
								deleteErr = rusticBackup.Remove()
								rusticBackup.Close()
							}
						}
					}

				default:
					backupLogger.Warn("unsupported adapter type, skipping")
					failedCount++
					continue
				}
			}

			if deleteErr != nil {
				backupLogger.WithError(deleteErr).Warn("failed to delete backup, continuing")
				failedCount++
			} else {
				backupLogger.Info("backup deleted successfully")
				deletedCount++
			}
		}
	}

	reporter.ReportProgress(50, "Checking for rustic repositories...")

	// Try to get rustic config for both local and S3 to destroy any existing repositories
	repositories := []struct {
		backupType string
		repo       backup.Repository
	}{}

	// Try local repository
	if localConfig, err := j.client.GetServerRusticConfig(ctx, s.ID(), "local"); err == nil {
		cfg := backup.Config{
			ServerUUID: s.ID(),
			Password:   localConfig.RepositoryPassword,
			BackupType: "local",
			LocalPath:  localConfig.RepositoryPath,
		}
		if localRepo, err := backup.NewLocalRepository(cfg); err == nil {
			repositories = append(repositories, struct {
				backupType string
				repo       backup.Repository
			}{"local", localRepo})
		}
	}

	// Try S3 repository
	if s3Config, err := j.client.GetServerRusticConfig(ctx, s.ID(), "s3"); err == nil {
		s3ConfigStruct := &backup.S3Config{
			Bucket:          s3Config.S3Credentials.Bucket,
			Region:          s3Config.S3Credentials.Region,
			Endpoint:        s3Config.S3Credentials.Endpoint,
			AccessKeyID:     s3Config.S3Credentials.AccessKeyID,
			SecretAccessKey: s3Config.S3Credentials.SecretAccessKey,
			SessionToken:    s3Config.S3Credentials.SessionToken,
			ForcePathStyle:  s3Config.S3Credentials.ForcePathStyle,
		}
		cfg := backup.Config{
			ServerUUID: s.ID(),
			Password:   s3Config.RepositoryPassword,
			BackupType: "s3",
			S3Config:   s3ConfigStruct,
		}
		if s3Repo, err := backup.NewS3Repository(cfg); err == nil {
			repositories = append(repositories, struct {
				backupType string
				repo       backup.Repository
			}{"s3", s3Repo})
		}
	}

	reporter.ReportProgress(70, "Destroying repositories...")

	// Destroy all found repositories - MUST succeed for job to be successful
	destroyedRepos := 0
	var destroyErrors []error

	for _, repoStruct := range repositories {
		logger.WithField("backup_type", repoStruct.backupType).Info("destroying repository")

		if err := repoStruct.repo.Destroy(ctx); err != nil {
			logger.WithError(err).Error("failed to destroy repository")
			destroyErrors = append(destroyErrors, errors.Wrapf(err, "failed to destroy %s repository", repoStruct.backupType))
		} else {
			logger.WithField("backup_type", repoStruct.backupType).Info("repository destroyed successfully")
			destroyedRepos++
		}

		repoStruct.repo.Close()
	}

	// If any repository destruction failed, fail the entire job
	if len(destroyErrors) > 0 {
		reporter.ReportProgress(100, "Repository destruction failed")

		logger.WithFields(log.Fields{
			"deleted_count":   deletedCount,
			"failed_count":    failedCount,
			"destroyed_repos": destroyedRepos,
			"total_repos":     len(repositories),
			"destroy_errors":  len(destroyErrors),
		}).Error("delete all job failed - repository destruction incomplete")

		// Combine all errors
		errorMsg := "failed to destroy repositories: "
		for i, err := range destroyErrors {
			if i > 0 {
				errorMsg += "; "
			}
			errorMsg += err.Error()
		}

		return nil, errors.New(errorMsg)
	}

	reporter.ReportProgress(100, "All backups and repositories destroyed")

	logger.WithFields(log.Fields{
		"deleted_count":   deletedCount,
		"failed_count":    failedCount,
		"destroyed_repos": destroyedRepos,
	}).Info("all backups and repositories destroyed successfully")

	return map[string]interface{}{
		"deleted":          true,
		"backup_count":     len(j.backups),
		"deleted_count":    deletedCount,
		"failed_count":     failedCount,
		"repository_count": len(repositories),
		"destroyed_repos":  destroyedRepos,
		"successful":       true,
	}, nil
}
