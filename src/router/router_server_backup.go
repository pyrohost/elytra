package router

import (
	"net/http"
	"os"
	"strings"

	"emperror.dev/errors"
	"github.com/apex/log"
	"github.com/gin-gonic/gin"

	"github.com/pyrohost/elytra/src/router/middleware"
	"github.com/pyrohost/elytra/src/server"
	"github.com/pyrohost/elytra/src/server/backup"
)

// validateRusticAdapter validates that a rustic adapter is properly configured
func validateRusticAdapter(adapter backup.BackupInterface) error {
	rusticBackup, ok := adapter.(*backup.RusticBackup)
	if !ok {
		return errors.New("adapter is not a rustic backup")
	}

	// Validate that we have required configuration
	if rusticBackup.Identifier() == "" {
		return errors.New("backup UUID is required")
	}

	return nil
}

// postServerBackup performs a backup against a given server instance using the
// provided backup adapter.
func postServerBackup(c *gin.Context) {
	s := middleware.ExtractServer(c)
	client := middleware.ExtractApiClient(c)
	logger := middleware.ExtractLogger(c)
	var data struct {
		Adapter backup.AdapterType `json:"adapter"`
		Uuid    string             `json:"uuid"`
		Ignore  string             `json:"ignore"`
	}
	if err := c.BindJSON(&data); err != nil {
		return
	}

	var adapter backup.BackupInterface
	switch data.Adapter {
	case backup.LocalBackupAdapter:
		adapter = backup.NewLocal(client, data.Uuid, data.Ignore)
	case backup.S3BackupAdapter:
		adapter = backup.NewS3(client, data.Uuid, data.Ignore)
	case backup.RusticLocalAdapter:
		rusticConfig, err := client.GetServerRusticConfig(c.Request.Context(), s.ID(), "local")
		if err != nil {
			middleware.CaptureAndAbort(c, errors.Wrap(err, "router/backups: failed to get server rustic config"))
			return
		}
		adapter = backup.NewRusticWithServerPath(client, s.ID(), data.Uuid, data.Ignore, "local", nil, rusticConfig.RepositoryPassword, rusticConfig.RepositoryPath)

		// Validate rustic configuration before proceeding
		if err := validateRusticAdapter(adapter); err != nil {
			middleware.CaptureAndAbort(c, errors.Wrap(err, "router/backups: rustic local configuration invalid"))
			return
		}
	case backup.RusticS3Adapter:
		rusticConfig, err := client.GetServerRusticConfig(c.Request.Context(), s.ID(), "s3")
		if err != nil {
			middleware.CaptureAndAbort(c, errors.Wrap(err, "router/backups: failed to get server rustic config"))
			return
		}
		adapter = backup.NewRusticWithServerPath(client, s.ID(), data.Uuid, data.Ignore, "s3", rusticConfig.S3Credentials, rusticConfig.RepositoryPassword, rusticConfig.RepositoryPath)

		// Validate rustic configuration before proceeding
		if err := validateRusticAdapter(adapter); err != nil {
			middleware.CaptureAndAbort(c, errors.Wrap(err, "router/backups: rustic S3 configuration invalid"))
			return
		}
	default:
		middleware.CaptureAndAbort(c, errors.New("router/backups: provided adapter is not valid: "+string(data.Adapter)))
		return
	}

	// Attach the server ID and the request ID to the adapter log context for easier
	// parsing in the logs.
	adapter.WithLogContext(map[string]interface{}{
		"server":     s.ID(),
		"request_id": c.GetString("request_id"),
	})

	go func(b backup.BackupInterface, s *server.Server, logger *log.Entry) {
		if err := s.Backup(b); err != nil {
			logger.WithField("error", errors.WithStackIf(err)).Error("router: failed to generate server backup")
		}
	}(adapter, s, logger)

	c.Status(http.StatusAccepted)
}

// postServerRestoreBackup handles restoring a backup for a server by downloading
// or finding the given backup on the system and then unpacking the archive into
// the server's data directory. If the TruncateDirectory field is provided and
// is true all of the files will be deleted for the server.
//
// This endpoint will block until the backup is fully restored allowing for a
// spinner to be displayed in the Panel UI effectively.
//
// TODO: stop the server if it is running
func postServerRestoreBackup(c *gin.Context) {
	s := middleware.ExtractServer(c)
	client := middleware.ExtractApiClient(c)
	logger := middleware.ExtractLogger(c)

	var data struct {
		Adapter           backup.AdapterType `binding:"required,oneof=elytra s3 rustic_local rustic_s3" json:"adapter"`
		TruncateDirectory bool               `json:"truncate_directory"`
		// A UUID is always required for this endpoint, however the download URL
		// is only present when the given adapter type is s3.
		DownloadUrl string `json:"download_url"`
	}
	if err := c.BindJSON(&data); err != nil {
		return
	}
	if data.Adapter == backup.S3BackupAdapter && data.DownloadUrl == "" {
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "The download_url field is required when the backup adapter is set to S3."})
		return
	}

	s.SetRestoring(true)
	hasError := true
	defer func() {
		if !hasError {
			return
		}

		s.SetRestoring(false)
	}()

	logger.Info("processing server backup restore request")
	if data.TruncateDirectory {
		logger.Info("received \"truncate_directory\" flag in request: deleting server files")
		if err := s.Filesystem().TruncateRootDirectory(); err != nil {
			middleware.CaptureAndAbort(c, err)
			return
		}
	}

	// Now that we've cleaned up the data directory if necessary, grab the backup file
	// and attempt to restore it into the server directory.
	if data.Adapter == backup.LocalBackupAdapter {
		b, _, err := backup.LocateLocal(client, c.Param("backup"))
		if err != nil {
			middleware.CaptureAndAbort(c, err)
			return
		}
		go func(s *server.Server, b backup.BackupInterface, logger *log.Entry) {
			logger.Info("starting restoration process for server backup using local driver")
			if err := s.RestoreBackup(b, nil); err != nil {
				logger.WithField("error", err).Error("failed to restore local backup to server")
			}
			s.Events().Publish(server.DaemonMessageEvent, "Completed server restoration from local backup.")
			s.Events().Publish(server.BackupRestoreCompletedEvent, "")
			logger.Info("completed server restoration from local backup")
			s.SetRestoring(false)
		}(s, b, logger)
		hasError = false
		c.Status(http.StatusAccepted)
		return
	}

	if data.Adapter == backup.RusticLocalAdapter || data.Adapter == backup.RusticS3Adapter {
		backupType := "local"
		if data.Adapter == backup.RusticS3Adapter {
			backupType = "s3"
		}
		rusticConfig, err := client.GetServerRusticConfig(c.Request.Context(), s.ID(), backupType)
		if err != nil {
			middleware.CaptureAndAbort(c, errors.Wrap(err, "router/backups: failed to get server rustic config for restore"))
			return
		}

		var rusticBackup backup.BackupInterface
		if data.Adapter == backup.RusticLocalAdapter {
			rusticBackup, err = backup.LocateRusticWithPath(client, s.ID(), c.Param("backup"), "local", nil, rusticConfig.RepositoryPassword, rusticConfig.RepositoryPath)
		} else {
			rusticBackup, err = backup.LocateRusticWithPath(client, s.ID(), c.Param("backup"), "s3", rusticConfig.S3Credentials, rusticConfig.RepositoryPassword, rusticConfig.RepositoryPath)
		}
		if err != nil {
			middleware.CaptureAndAbort(c, errors.Wrap(err, "router/backups: failed to locate rustic backup for restore"))
			return
		}

		// Validate rustic backup before proceeding with restore
		if err := validateRusticAdapter(rusticBackup); err != nil {
			middleware.CaptureAndAbort(c, errors.Wrap(err, "router/backups: rustic backup validation failed"))
			return
		}

		go func(s *server.Server, b backup.BackupInterface, logger *log.Entry) {
			logger.Info("starting restoration process for server backup using rustic driver")
			if err := s.RestoreBackup(b, nil); err != nil {
				logger.WithField("error", err).Error("failed to restore rustic backup to server")
			}
			s.Events().Publish(server.DaemonMessageEvent, "Completed server restoration from rustic backup.")
			s.Events().Publish(server.BackupRestoreCompletedEvent, "")
			logger.Info("completed server restoration from rustic backup")
			s.SetRestoring(false)
		}(s, rusticBackup, logger)
		hasError = false
		c.Status(http.StatusAccepted)
		return
	}

	// Since this is not a local backup we need to stream the archive and then
	// parse over the contents as we go in order to restore it to the server.
	httpClient := http.Client{}
	logger.Info("downloading backup from remote location...")
	// TODO: this will hang if there is an issue. We can't use c.Request.Context() (or really any)
	//  since it will be canceled when the request is closed which happens quickly since we push
	//  this into the background.
	//
	// For now I'm just using the server context so at least the request is canceled if
	// the server gets deleted.
	req, err := http.NewRequestWithContext(s.Context(), http.MethodGet, data.DownloadUrl, nil)
	if err != nil {
		middleware.CaptureAndAbort(c, err)
		return
	}
	res, err := httpClient.Do(req)
	if err != nil {
		middleware.CaptureAndAbort(c, err)
		return
	}
	// Don't allow content types that we know are going to give us problems.
	if res.Header.Get("Content-Type") == "" || !strings.Contains("application/x-gzip application/gzip", res.Header.Get("Content-Type")) {
		_ = res.Body.Close()
		c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
			"error": "The provided backup link is not a supported content type. \"" + res.Header.Get("Content-Type") + "\" is not application/x-gzip.",
		})
		return
	}

	go func(s *server.Server, uuid string, logger *log.Entry) {
		logger.Info("starting restoration process for server backup using S3 driver")
		if err := s.RestoreBackup(backup.NewS3(client, uuid, ""), res.Body); err != nil {
			logger.WithField("error", errors.WithStack(err)).Error("failed to restore remote S3 backup to server")
		}
		s.Events().Publish(server.DaemonMessageEvent, "Completed server restoration from S3 backup.")
		s.Events().Publish(server.BackupRestoreCompletedEvent, "")
		logger.Info("completed server restoration from S3 backup")
		s.SetRestoring(false)
	}(s, c.Param("backup"), logger)

	hasError = false
	c.Status(http.StatusAccepted)
}

// deleteServerBackup deletes a backup of a server. It handles both local tar.gz backups
// and rustic backups. If the backup is not found on the machine just return a 404 error.
func deleteServerBackup(c *gin.Context) {
	client := middleware.ExtractApiClient(c)
	s := middleware.ExtractServer(c)
	backupUuid := c.Param("backup")

	// First try to locate as a local backup
	b, _, err := backup.LocateLocal(client, backupUuid)
	if err == nil {
		// Local backup found, delete it
		if err := b.Remove(); err != nil && !errors.Is(err, os.ErrNotExist) {
			middleware.CaptureAndAbort(c, err)
			return
		}
		c.Status(http.StatusNoContent)
		return
	}

	// If local backup not found, try to locate as rustic backup
	// Try local rustic first
	rusticConfig, err := client.GetServerRusticConfig(c.Request.Context(), s.ID(), "local")
	if err == nil {
		rusticBackup, err := backup.LocateRusticWithPath(client, s.ID(), backupUuid, "local", nil, rusticConfig.RepositoryPassword, rusticConfig.RepositoryPath)
		if err == nil {
			if err := rusticBackup.Remove(); err != nil {
				middleware.CaptureAndAbort(c, err)
				return
			}
			c.Status(http.StatusNoContent)
			return
		}
	}

	// Try S3 rustic
	rusticS3Config, err := client.GetServerRusticConfig(c.Request.Context(), s.ID(), "s3")
	if err == nil && rusticS3Config.S3Credentials != nil {
		rusticBackup, err := backup.LocateRusticWithPath(client, s.ID(), backupUuid, "s3", rusticS3Config.S3Credentials, rusticS3Config.RepositoryPassword, rusticS3Config.RepositoryPath)
		if err == nil {
			if err := rusticBackup.Remove(); err != nil {
				middleware.CaptureAndAbort(c, err)
				return
			}
			c.Status(http.StatusNoContent)
			return
		}
	}

	// No backup found with the given UUID
	c.AbortWithStatusJSON(http.StatusNotFound, gin.H{
		"error": "The requested backup was not found on this server.",
	})
}
