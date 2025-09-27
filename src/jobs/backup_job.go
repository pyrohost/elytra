package jobs

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/apex/log"
	"github.com/pyrohost/elytra/src/server/backup"
	"github.com/pyrohost/elytra/src/server"
	"github.com/pyrohost/elytra/src/remote"
	"emperror.dev/errors"
)

// BackupCreateJob handles backup creation operations
type BackupCreateJob struct {
	id              string
	serverID        string
	backupUUID      string
	adapterType     string
	ignore          string
	name            string
	context         map[string]interface{}
	progress        int
	message         string
	serverManager   *server.Manager
	client          remote.Client
}

// BackupDeleteJob handles backup deletion operations
type BackupDeleteJob struct {
	id            string
	serverID      string
	backupUUID    string
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

	return &BackupDeleteJob{
		id:            uuid.New().String(),
		serverID:      serverID,
		backupUUID:    backupUUID,
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

	return &BackupRestoreJob{
		id:                uuid.New().String(),
		serverID:          serverID,
		backupUUID:        backupUUID,
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
func (j *BackupCreateJob) GetID() string        { return j.id }
func (j *BackupCreateJob) GetType() string      { return "backup_create" }
func (j *BackupCreateJob) GetProgress() int     { return j.progress }
func (j *BackupCreateJob) GetMessage() string   { return j.message }

// WebSocketJob interface implementation for real-time backup status updates
func (j *BackupCreateJob) GetServerID() string { return j.serverID }
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

	// Get server instance
	s, exists := j.serverManager.Get(j.serverID)
	if !exists {
		return nil, errors.New("server not found: " + j.serverID)
	}

	reporter.ReportProgress(20, "Initializing backup adapter...")

	// Create backup adapter based on type
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

	// Attach context from job data
	if j.context != nil {
		adapter.WithLogContext(j.context)
	}

	reporter.ReportProgress(30, "Starting backup generation...")

	logger.Info("starting backup generation")

	// Perform backup with progress reporting
	result, err := j.performBackupWithProgress(ctx, reporter, s, adapter)
	if err != nil {
		logger.WithError(err).Error("backup generation failed")
		return nil, errors.Wrap(err, "backup generation failed")
	}

	reporter.ReportProgress(100, "Backup created successfully")

	logger.Info("backup generation completed successfully")
	return result, nil
}

// performBackupWithProgress performs a backup while reporting progress
func (j *BackupCreateJob) performBackupWithProgress(ctx context.Context, reporter ProgressReporter, s *server.Server, adapter backup.BackupInterface) (map[string]interface{}, error) {
	// Get server-wide ignored files
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

	// Generate the backup
	ad, err := adapter.Generate(ctx, s.Filesystem(), ignored)
	if err != nil {
		return nil, err
	}

	reporter.ReportProgress(80, "Finalizing backup...")

	return map[string]interface{}{
		"checksum":    ad.Checksum,
		"size":        ad.Size,
		"snapshot_id": ad.SnapshotId,
		"adapter":     j.adapterType,
		"successful":  true,
	}, nil
}

// BackupDeleteJob implementation
func (j *BackupDeleteJob) GetID() string        { return j.id }
func (j *BackupDeleteJob) GetType() string      { return "backup_delete" }
func (j *BackupDeleteJob) GetProgress() int     { return j.progress }
func (j *BackupDeleteJob) GetMessage() string   { return j.message }

// WebSocketJob interface implementation for real-time backup status updates
func (j *BackupDeleteJob) GetServerID() string { return j.serverID }
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

	// Get server instance
	s, exists := j.serverManager.Get(j.serverID)
	if !exists {
		return nil, errors.New("server not found: " + j.serverID)
	}

	logger.Info("executing backup delete job")

	// Delete backup based on the specific adapter type
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

	rusticBackup, err := backup.LocateRusticWithPath(j.client, s.ID(), j.backupUUID, "local", nil, rusticConfig.RepositoryPassword, rusticConfig.RepositoryPath)
	if err != nil {
		return nil, errors.Wrap(err, "failed to locate rustic local backup")
	}

	reporter.ReportProgress(70, "Deleting rustic local backup...")

	if removeErr := rusticBackup.Remove(); removeErr != nil {
		return nil, errors.Wrap(removeErr, "failed to delete rustic local backup")
	}

	reporter.ReportProgress(100, "Backup deleted successfully")

	logger.Info("successfully deleted rustic local backup")
	return map[string]interface{}{
		"deleted":    true,
		"type":       "rustic_local",
		"successful": true,
	}, nil
}

// deleteRusticS3Backup deletes a rustic S3 backup
func (j *BackupDeleteJob) deleteRusticS3Backup(ctx context.Context, reporter ProgressReporter, s *server.Server, logger *log.Entry) (map[string]interface{}, error) {
	reporter.ReportProgress(30, "Getting rustic S3 configuration...")

	rusticS3Config, err := j.client.GetServerRusticConfig(ctx, s.ID(), "s3")
	if err != nil {
		return nil, errors.Wrap(err, "failed to get server rustic S3 config")
	}

	if rusticS3Config.S3Credentials == nil {
		return nil, errors.New("rustic S3 credentials not configured")
	}

	reporter.ReportProgress(50, "Locating rustic S3 backup...")

	rusticBackup, err := backup.LocateRusticWithPath(j.client, s.ID(), j.backupUUID, "s3", rusticS3Config.S3Credentials, rusticS3Config.RepositoryPassword, rusticS3Config.RepositoryPath)
	if err != nil {
		return nil, errors.Wrap(err, "failed to locate rustic S3 backup")
	}

	reporter.ReportProgress(70, "Deleting rustic S3 backup...")

	if removeErr := rusticBackup.Remove(); removeErr != nil {
		return nil, errors.Wrap(removeErr, "failed to delete rustic S3 backup")
	}

	reporter.ReportProgress(100, "Backup deleted successfully")

	logger.Info("successfully deleted rustic S3 backup")
	return map[string]interface{}{
		"deleted":    true,
		"type":       "rustic_s3",
		"successful": true,
	}, nil
}

// BackupRestoreJob implementation
func (j *BackupRestoreJob) GetID() string        { return j.id }
func (j *BackupRestoreJob) GetType() string      { return "backup_restore" }
func (j *BackupRestoreJob) GetProgress() int     { return j.progress }
func (j *BackupRestoreJob) GetMessage() string   { return j.message }

// WebSocketJob interface implementation for real-time backup status updates
func (j *BackupRestoreJob) GetServerID() string { return j.serverID }
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

	// Get server instance
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
		if err := s.Filesystem().TruncateRootDirectory(); err != nil {
			return nil, errors.Wrap(err, "failed to truncate server directory")
		}
	}

	reporter.ReportProgress(40, "Locating backup...")

	// Handle different backup types
	switch backup.AdapterType(j.adapterType) {
	case backup.LocalBackupAdapter:
		return j.restoreLocalBackup(ctx, reporter, s, logger)
	case backup.RusticLocalAdapter:
		return j.restoreRusticBackup(ctx, reporter, s, "local", logger)
	case backup.RusticS3Adapter:
		return j.restoreRusticBackup(ctx, reporter, s, "s3", logger)
	case backup.S3BackupAdapter:
		return j.restoreS3Backup(ctx, reporter, s, logger)
	default:
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
		rusticBackup, err = backup.LocateRusticWithPath(j.client, s.ID(), j.backupUUID, "local", nil, rusticConfig.RepositoryPassword, rusticConfig.RepositoryPath)
	} else {
		rusticBackup, err = backup.LocateRusticWithPath(j.client, s.ID(), j.backupUUID, "s3", rusticConfig.S3Credentials, rusticConfig.RepositoryPassword, rusticConfig.RepositoryPath)
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
	reporter.ReportProgress(50, "Downloading S3 backup...")

	if j.downloadURL == "" {
		return nil, errors.New("download URL is required for S3 backup restoration")
	}

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

	// Validate content type
	contentType := res.Header.Get("Content-Type")
	if contentType != "" && !strings.Contains("application/x-gzip application/gzip", contentType) {
		return nil, errors.Errorf("unsupported content type: %s (expected gzip)", contentType)
	}

	reporter.ReportProgress(80, "Starting file restoration...")

	logger.Info("starting restoration process for S3 backup")
	s3Backup := backup.NewS3(j.client, j.backupUUID, "")
	if err := s.RestoreBackup(s3Backup, res.Body); err != nil {
		logger.WithError(err).Error("failed to restore S3 backup")
		return nil, errors.Wrap(err, "failed to restore S3 backup")
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