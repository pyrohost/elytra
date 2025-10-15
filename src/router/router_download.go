package router

import (
	"bufio"
	"net/http"
	"os"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/pyrohost/elytra/src/router/middleware"
	"github.com/pyrohost/elytra/src/router/tokens"
	"github.com/pyrohost/elytra/src/server/backup"
)

// Handle a download request for a server backup.
func getDownloadBackup(c *gin.Context) {
	client := middleware.ExtractApiClient(c)
	manager := middleware.ExtractManager(c)

	// Get the payload from the token.
	token := tokens.BackupPayload{}
	if err := tokens.ParseToken([]byte(c.Query("token")), &token); err != nil {
		middleware.CaptureAndAbort(c, err)
		return
	}

	// Get the server using the UUID from the token.
	if _, ok := manager.Get(token.ServerUuid); !ok || !token.IsUniqueRequest() {
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{
			"error": "The requested resource was not found on this server.",
		})
		return
	}

	// Validate that the BackupUuid field is actually a UUID and not some random characters or a
	// file path.
	if _, err := uuid.Parse(token.BackupUuid); err != nil {
		middleware.CaptureAndAbort(c, err)
		return
	}

	// Check backup disk type from JWT token to determine how to handle download
	switch token.BackupDisk {
	case "elytra":
		// Legacy Elytra backup - try local file system
		b, st, err := backup.LocateLocal(client, token.BackupUuid)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusNotFound, gin.H{
				"error": "The requested backup was not found on this server.",
			})
			return
		}

		f, err := os.Open(b.Path())
		if err != nil {
			middleware.CaptureAndAbort(c, err)
			return
		}
		defer f.Close()

		c.Header("Content-Length", strconv.Itoa(int(st.Size())))
		c.Header("Content-Disposition", "attachment; filename="+strconv.Quote(st.Name()))
		c.Header("Content-Type", "application/octet-stream")

		_, _ = bufio.NewReader(f).WriteTo(c.Writer)
		return

	case "rustic_local", "rustic_s3":
		// Rustic backup - get configuration and locate backup
		rusticConfig, err := client.GetServerRusticConfig(c.Request.Context(), token.ServerUuid, token.RepositoryType)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusNotFound, gin.H{
				"error": "Failed to get rustic configuration for backup.",
			})
			return
		}

		var rusticBackup *backup.RusticBackup
		switch token.RepositoryType {
		case "local":
			if token.SnapshotId != "" {
				// Use snapshot ID for direct access
				rusticBackup, err = backup.LocateRusticBySnapshotID(client, token.ServerUuid, token.SnapshotId, "local", nil, rusticConfig.RepositoryPassword, rusticConfig.RepositoryPath)
			} else {
				// Fallback to failed backup handling for compatibility
				// todo: completely refactor this dumahh logic - ellie
				rusticBackup, err = backup.LocateRusticBySnapshotID(client, token.ServerUuid, "", "local", nil, rusticConfig.RepositoryPassword, rusticConfig.RepositoryPath)
			}
		case "s3":
			if rusticConfig.S3Credentials == nil {
				c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
					"error": "S3 credentials required for S3 rustic backup.",
				})
				return
			}
			if token.SnapshotId != "" {
				// Use snapshot ID for direct access
				rusticBackup, err = backup.LocateRusticBySnapshotID(client, token.ServerUuid, token.SnapshotId, "s3", rusticConfig.S3Credentials, rusticConfig.RepositoryPassword, rusticConfig.RepositoryPath)
			} else {
				// Fallback to failed backup handling for compatibility
				rusticBackup, err = backup.LocateRusticBySnapshotID(client, token.ServerUuid, "", "s3", rusticConfig.S3Credentials, rusticConfig.RepositoryPassword, rusticConfig.RepositoryPath)
			}
		default:
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
				"error": "Invalid repository type for rustic backup.",
			})
			return
		}

		if err != nil {
			c.AbortWithStatusJSON(http.StatusNotFound, gin.H{
				"error": "The requested backup was not found on this server.",
			})
			return
		}

		// Check if rustic backup can be downloaded
		if !rusticBackup.CanDownload() {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
				"error": "The requested backup cannot be downloaded.",
			})
			return
		}

		// Set response headers for rustic backup download
		filename := token.BackupUuid + ".tar.gz"
		c.Header("Content-Disposition", "attachment; filename="+strconv.Quote(filename))
		c.Header("Content-Type", "application/gzip")

		// Stream the rustic backup directly to the response
		if err := rusticBackup.DownloadTarGz(c.Request.Context(), c.Writer); err != nil {
			middleware.CaptureAndAbort(c, err)
			return
		}
		return

	default:
		// Try legacy approach for backwards compatibility if backup disk is not specified
		b, st, err := backup.LocateLocal(client, token.BackupUuid)
		if err == nil {
			// Legacy local backup found, serve it directly
			f, err := os.Open(b.Path())
			if err != nil {
				middleware.CaptureAndAbort(c, err)
				return
			}
			defer f.Close()

			c.Header("Content-Length", strconv.Itoa(int(st.Size())))
			c.Header("Content-Disposition", "attachment; filename="+strconv.Quote(st.Name()))
			c.Header("Content-Type", "application/octet-stream")

			_, _ = bufio.NewReader(f).WriteTo(c.Writer)
			return
		}

		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{
			"error": "The requested backup was not found on this server.",
		})
		return
	}
}

// Handles downloading a specific file for a server.
func getDownloadFile(c *gin.Context) {
	manager := middleware.ExtractManager(c)
	token := tokens.FilePayload{}
	if err := tokens.ParseToken([]byte(c.Query("token")), &token); err != nil {
		middleware.CaptureAndAbort(c, err)
		return
	}

	s, ok := manager.Get(token.ServerUuid)
	if !ok || !token.IsUniqueRequest() {
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{
			"error": "The requested resource was not found on this server.",
		})
		return
	}

	f, st, err := s.Filesystem().File(token.FilePath)
	if err != nil {
		middleware.CaptureAndAbort(c, err)
		return
	}
	defer f.Close()
	if st.IsDir() {
		c.AbortWithStatusJSON(http.StatusNotFound, gin.H{
			"error": "The requested resource was not found on this server.",
		})
		return
	}

	c.Header("Content-Length", strconv.Itoa(int(st.Size())))
	c.Header("Content-Disposition", "attachment; filename="+strconv.Quote(st.Name()))
	c.Header("Content-Type", "application/octet-stream")

	_, _ = bufio.NewReader(f).WriteTo(c.Writer)
}
