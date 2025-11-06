package backup

import (
	"context"

	"emperror.dev/errors"

	"github.com/pyrohost/elytra/src/remote"
)

// NewRustic creates a new rustic backup instance (legacy interface)
// somebody please refactor this - ellie
func NewRustic(client remote.Client, uuid string, ignore string, backupType string, s3Creds *remote.S3Credentials, password string) *RusticBackup {
	config := Config{
		BackupUUID:     uuid,
		Password:       password,
		BackupType:     backupType,
		IgnorePatterns: ignore,
	}

	// Convert S3 credentials if provided
	if s3Creds != nil {
		config.S3Config = &S3Config{
			Bucket:          s3Creds.Bucket,
			Region:          s3Creds.Region,
			Endpoint:        s3Creds.Endpoint,
			AccessKeyID:     s3Creds.AccessKeyID,
			SecretAccessKey: s3Creds.SecretAccessKey,
			SessionToken:    s3Creds.SessionToken,
			ForcePathStyle:  s3Creds.ForcePathStyle,
		}
	}

	backup, err := NewRusticBackup(client, config)
	if err != nil {
		// Return a broken backup for backward compatibility
		return &RusticBackup{
			Backup: Backup{
				client:  client,
				Uuid:    uuid,
				Ignore:  ignore,
				adapter: AdapterType("rustic_" + backupType),
			},
			config: config,
		}
	}

	return backup
}

// NewRusticWithServerPath creates a new rustic backup instance with server path (legacy interface)
func NewRusticWithServerPath(client remote.Client, serverUuid string, backupUuid string, ignore string, backupType string, s3Creds *remote.S3Credentials, password string, repoPath string) *RusticBackup {
	config := Config{
		BackupUUID:     backupUuid,
		ServerUUID:     serverUuid,
		Password:       password,
		BackupType:     backupType,
		LocalPath:      repoPath,
		IgnorePatterns: ignore,
	}

	// Convert S3 credentials if provided
	if s3Creds != nil {
		config.S3Config = &S3Config{
			Bucket:          s3Creds.Bucket,
			Region:          s3Creds.Region,
			Endpoint:        s3Creds.Endpoint,
			AccessKeyID:     s3Creds.AccessKeyID,
			SecretAccessKey: s3Creds.SecretAccessKey,
			SessionToken:    s3Creds.SessionToken,
			ForcePathStyle:  s3Creds.ForcePathStyle,
		}
	}

	backup, err := NewRusticBackup(client, config)
	if err != nil {
		// Return a broken backup for backward compatibility
		return &RusticBackup{
			Backup: Backup{
				client:  client,
				Uuid:    backupUuid,
				Ignore:  ignore,
				adapter: AdapterType("rustic_" + backupType),
			},
			config: config,
		}
	}

	return backup
}

// LocateRusticBySnapshotID finds a rustic backup by snapshot ID (preferred method)
func LocateRusticBySnapshotID(client remote.Client, serverUuid string, snapshotID string, backupType string, s3Creds *remote.S3Credentials, password string, repoPath string) (*RusticBackup, error) {
	config := Config{
		ServerUUID: serverUuid,
		Password:   password,
		BackupType: backupType,
		LocalPath:  repoPath,
	}

	// Convert S3 credentials if provided
	if s3Creds != nil {
		config.S3Config = &S3Config{
			Bucket:          s3Creds.Bucket,
			Region:          s3Creds.Region,
			Endpoint:        s3Creds.Endpoint,
			AccessKeyID:     s3Creds.AccessKeyID,
			SecretAccessKey: s3Creds.SecretAccessKey,
			SessionToken:    s3Creds.SessionToken,
			ForcePathStyle:  s3Creds.ForcePathStyle,
		}
	}

	backup, err := NewRusticBackup(client, config)
	if err != nil {
		return nil, err
	}

	// For deletion or restore, set the snapshot ID directly
	if snapshotID == "" {
		// NULL snapshot ID means failed backup - nothing to delete
		backup.snapshot = nil
		return backup, nil
	}

	// Get the snapshot details from the repository
	snapshot, err := backup.repository.GetSnapshot(context.Background(), snapshotID)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get snapshot details")
	}

	backup.snapshot = snapshot
	return backup, nil
}
