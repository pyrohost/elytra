package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"emperror.dev/errors"

	"github.com/pyrohost/elytra/src/config"
	"github.com/pyrohost/elytra/src/remote"
	"github.com/pyrohost/elytra/src/server/filesystem"
)

type RusticS3Backup struct {
	Backup
}

var _ BackupInterface = (*RusticS3Backup)(nil)

func NewRusticS3(client remote.Client, uuid, ignore string) *RusticS3Backup {
	return &RusticS3Backup{
		Backup{
			client:  client,
			Uuid:    uuid,
			Ignore:  ignore,
			adapter: RusticS3BackupAdapter,
		},
	}
}

// LocateRusticS3 finds a rustic S3 backup for a server and returns the backup instance
func LocateRusticS3(client remote.Client, uuid string) (*RusticS3Backup, error) {
	b := NewRusticS3(client, uuid, "")

	// Validate configuration before attempting to locate
	if err := b.validateConfig(); err != nil {
		return nil, errors.Wrap(err, "invalid rustic S3 configuration")
	}

	// Check if the backup exists in the rustic S3 repository
	snapshots, err := b.listSnapshots(context.Background())
	if err != nil {
		return nil, errors.Wrap(err, "failed to locate rustic S3 backup")
	}

	// Look for a snapshot with our backup UUID in tags
	found := false
	for _, snapshot := range snapshots {
		for _, tag := range snapshot.Tags {
			if tag == fmt.Sprintf("backup_uuid=%s", uuid) {
				found = true
				break
			}
		}
		if found {
			break
		}
	}

	if !found {
		return nil, errors.New("backup not found in rustic S3 repository")
	}

	return b, nil
}

// Remove removes a backup from the rustic S3 repository
func (b *RusticS3Backup) Remove() error {
	if err := b.validateConfig(); err != nil {
		return errors.Wrap(err, "invalid rustic S3 configuration")
	}

	cfg := config.Get()
	args := []string{
		"forget",
		"--filter-tags", fmt.Sprintf("backup_uuid=%s", b.Uuid),
	}

	if cfg.System.Backups.Rustic.S3.RetentionPolicy.PruneAfterForget {
		args = append(args, "--prune")
	}

	cmd := exec.Command("rustic", args...)
	cmd.Env = b.buildEnv()

	if err := cmd.Run(); err != nil {
		return errors.Wrap(err, "failed to remove rustic S3 backup")
	}

	return nil
}

// WithLogContext attaches additional context to the log output for this backup
func (b *RusticS3Backup) WithLogContext(c map[string]interface{}) {
	b.logContext = c
}

// Generate creates a backup using rustic with S3 storage
func (b *RusticS3Backup) Generate(ctx context.Context, fsys *filesystem.Filesystem, ignore string) (*ArchiveDetails, error) {
	if err := b.validateConfig(); err != nil {
		return nil, errors.Wrap(err, "invalid rustic S3 configuration")
	}

	// Ensure repository exists
	if err := b.initRepository(); err != nil {
		return nil, errors.Wrap(err, "failed to initialize rustic S3 repository")
	}

	// Create ignore file if needed
	ignoreFile := ""
	if ignore != "" {
		ignoreFile = filepath.Join(os.TempDir(), fmt.Sprintf("rustic-s3-ignore-%s", b.Uuid))
		if err := os.WriteFile(ignoreFile, []byte(ignore), 0o600); err != nil {
			return nil, errors.Wrap(err, "failed to create ignore file")
		}
		defer os.Remove(ignoreFile)
	}

	cfg := config.Get()
	b.log().WithField("s3_bucket", cfg.System.Backups.Rustic.S3.Bucket).Info("creating rustic S3 backup for server")

	// Build rustic backup command
	args := []string{
		"backup",
		"--tag", fmt.Sprintf("backup_uuid=%s", b.Uuid),
		"--tag", "server_uuid=unknown",
		"--tag", "source=elytra",
		"--tag", "storage=s3",
		"--label", fmt.Sprintf("elytra_s3_backup_%s", b.Uuid),
		"--json",
	}

	if ignoreFile != "" {
		args = append(args, "--glob-file", ignoreFile)
	}

	args = append(args, fsys.Path())

	// Set a reasonable timeout for backup operations
	if ctx.Err() == nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 24*time.Hour) // 24 hour max backup time
		defer cancel()
	}

	cmd := exec.CommandContext(ctx, "rustic", args...)
	cmd.Env = b.buildEnv()

	output, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			b.log().Error("rustic S3 backup timed out after 24 hours")
			return nil, errors.New("rustic S3 backup timed out")
		}
		b.log().WithError(err).WithField("output", string(output)).Error("rustic S3 backup failed")
		return nil, errors.Wrap(err, "rustic S3 backup command failed")
	}

	b.log().Info("created rustic S3 backup successfully")

	// Get backup details
	ad, err := b.Details(ctx, nil)
	if err != nil {
		return nil, errors.WrapIf(err, "backup: failed to get archive details for rustic S3 backup")
	}

	return ad, nil
}

// Restore restores files from a rustic S3 backup
func (b *RusticS3Backup) Restore(ctx context.Context, _ io.Reader, callback RestoreCallback) error {
	if err := b.validateConfig(); err != nil {
		return errors.Wrap(err, "invalid rustic S3 configuration")
	}

	// Find the snapshot for this backup
	snapshots, err := b.listSnapshots(ctx)
	if err != nil {
		return errors.Wrap(err, "failed to find snapshot for restore")
	}

	var targetSnapshot *RusticSnapshot
	for _, snapshot := range snapshots {
		for _, tag := range snapshot.Tags {
			if tag == fmt.Sprintf("backup_uuid=%s", b.Uuid) {
				targetSnapshot = &snapshot
				break
			}
		}
		if targetSnapshot != nil {
			break
		}
	}

	if targetSnapshot == nil {
		return errors.New("could not find snapshot for restore")
	}

	// Create temporary directory for restoration with secure permissions
	tempDir := filepath.Join(os.TempDir(), fmt.Sprintf("rustic-s3-restore-%s", b.Uuid))
	if err := os.MkdirAll(tempDir, 0o700); err != nil {
		return errors.Wrap(err, "failed to create temporary restore directory")
	}
	defer func() {
		if err := os.RemoveAll(tempDir); err != nil {
			b.log().WithError(err).Warn("failed to cleanup temporary restore directory")
		}
	}()

	// Set timeout for restore operations
	restoreCtx, cancel := context.WithTimeout(ctx, 12*time.Hour) // 12 hour max restore time
	defer cancel()

	// Restore the snapshot
	cmd := exec.CommandContext(restoreCtx, "rustic", "restore", targetSnapshot.ID, tempDir)
	cmd.Env = b.buildEnv()

	if err := cmd.Run(); err != nil {
		if restoreCtx.Err() == context.DeadlineExceeded {
			return errors.New("rustic S3 restore timed out after 12 hours")
		}
		return errors.Wrap(err, "failed to restore rustic S3 backup")
	}

	// Walk the restored files and call the callback
	return filepath.Walk(tempDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if info.IsDir() {
			return nil
		}

		relPath, err := filepath.Rel(tempDir, path)
		if err != nil {
			return err
		}

		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()

		return callback(relPath, info, file)
	})
}

// validateConfig validates the S3 configuration
func (b *RusticS3Backup) validateConfig() error {
	cfg := config.Get()

	if !cfg.System.Backups.Rustic.Enabled {
		return errors.New("rustic backups are not enabled")
	}

	// Check if rustic binary is available
	if _, err := exec.LookPath("rustic"); err != nil {
		return errors.Wrap(err, "rustic binary not found in PATH")
	}

	if cfg.System.Backups.Rustic.S3.Bucket == "" {
		return errors.New("S3 bucket not configured for rustic backups")
	}

	if cfg.System.Backups.Rustic.S3.AccessKeyId == "" || cfg.System.Backups.Rustic.S3.SecretAccessKey == "" {
		return errors.New("S3 credentials not configured for rustic backups")
	}

	if cfg.System.Backups.Rustic.Password == "" && cfg.System.Backups.Rustic.PasswordFile == "" {
		return errors.New("rustic repository password not configured")
	}

	// Validate compression level
	if cfg.System.Backups.Rustic.CompressionLevel < 0 || cfg.System.Backups.Rustic.CompressionLevel > 22 {
		return errors.New("rustic compression level must be between 0 and 22")
	}

	// Validate S3 region if provided
	if cfg.System.Backups.Rustic.S3.Region == "" {
		b.log().Warn("S3 region not specified, using default us-east-1")
	}

	return nil
}

// initRepository initializes the rustic S3 repository if it doesn't exist
func (b *RusticS3Backup) initRepository() error {
	// Check if repository exists
	cmd := exec.Command("rustic", "snapshots", "--json")
	cmd.Env = b.buildEnv()

	if err := cmd.Run(); err != nil {
		// Repository doesn't exist, initialize it
		b.log().Info("initializing new rustic S3 repository")

		cfg := config.Get()
		initArgs := []string{"init"}
		if cfg.System.Backups.Rustic.CompressionLevel > 0 {
			initArgs = append(initArgs, "--set-compression", strconv.Itoa(cfg.System.Backups.Rustic.CompressionLevel))
		}

		cmd = exec.Command("rustic", initArgs...)
		cmd.Env = b.buildEnv()

		if err := cmd.Run(); err != nil {
			return errors.Wrap(err, "failed to initialize rustic S3 repository")
		}

		b.log().Info("rustic S3 repository initialized successfully")
	}

	return nil
}

// buildEnv builds the environment variables for rustic S3 commands
func (b *RusticS3Backup) buildEnv() []string {
	cfg := config.Get()
	env := os.Environ()

	// Build S3 repository URL
	var repositoryURL string
	if cfg.System.Backups.Rustic.S3.Endpoint != "" {
		// Custom S3-compatible endpoint
		repositoryURL = fmt.Sprintf("s3:%s/%s", cfg.System.Backups.Rustic.S3.Endpoint, cfg.System.Backups.Rustic.S3.Bucket)
	} else {
		// Default AWS S3
		repositoryURL = fmt.Sprintf("s3:s3.amazonaws.com/%s", cfg.System.Backups.Rustic.S3.Bucket)
	}

	// Add prefix if configured
	if cfg.System.Backups.Rustic.S3.Prefix != "" {
		repositoryURL = fmt.Sprintf("%s/%s", repositoryURL, cfg.System.Backups.Rustic.S3.Prefix)
	}

	env = append(env, fmt.Sprintf("RUSTIC_REPOSITORY=%s", repositoryURL))

	// Set password (prefer password file for security)
	if cfg.System.Backups.Rustic.PasswordFile != "" {
		env = append(env, fmt.Sprintf("RUSTIC_PASSWORD_FILE=%s", cfg.System.Backups.Rustic.PasswordFile))
	} else if cfg.System.Backups.Rustic.Password != "" {
		env = append(env, fmt.Sprintf("RUSTIC_PASSWORD=%s", cfg.System.Backups.Rustic.Password))
	}

	// Set AWS credentials - prefer credential files for security
	// Note: In production, consider using IAM roles or credential files instead
	if cfg.System.Backups.Rustic.S3.AccessKeyId != "" {
		env = append(env, fmt.Sprintf("AWS_ACCESS_KEY_ID=%s", cfg.System.Backups.Rustic.S3.AccessKeyId))
	}
	if cfg.System.Backups.Rustic.S3.SecretAccessKey != "" {
		env = append(env, fmt.Sprintf("AWS_SECRET_ACCESS_KEY=%s", cfg.System.Backups.Rustic.S3.SecretAccessKey))
	}

	// Set AWS region
	if cfg.System.Backups.Rustic.S3.Region != "" {
		env = append(env, fmt.Sprintf("AWS_DEFAULT_REGION=%s", cfg.System.Backups.Rustic.S3.Region))
	}

	// Set cache directory
	if cfg.System.Backups.Rustic.CacheDir != "" {
		env = append(env, fmt.Sprintf("RUSTIC_CACHE_DIR=%s", cfg.System.Backups.Rustic.CacheDir))
	}

	// Set log level
	env = append(env, fmt.Sprintf("RUSTIC_LOG_LEVEL=%s", cfg.System.Backups.Rustic.LogLevel))

	// Disable progress bars for automated usage
	env = append(env, "RUSTIC_NO_PROGRESS=true")

	return env
}

// listSnapshots returns all snapshots for this backup
func (b *RusticS3Backup) listSnapshots(ctx context.Context) ([]RusticSnapshot, error) {
	cmd := exec.CommandContext(ctx, "rustic", "snapshots", "--filter-tags", fmt.Sprintf("backup_uuid=%s", b.Uuid), "--json")
	cmd.Env = b.buildEnv()

	output, err := cmd.Output()
	if err != nil {
		return nil, errors.Wrap(err, "failed to list snapshots")
	}

	var snapshots []RusticSnapshot
	if len(strings.TrimSpace(string(output))) > 0 {
		if err := json.Unmarshal(output, &snapshots); err != nil {
			return nil, errors.Wrap(err, "failed to parse snapshots JSON")
		}
	}

	return snapshots, nil
}

// Size returns the size of the backup
func (b *RusticS3Backup) Size() (int64, error) {
	snapshots, err := b.listSnapshots(context.Background())
	if err != nil {
		return 0, errors.Wrap(err, "failed to get backup size")
	}

	if len(snapshots) == 0 {
		return 0, nil
	}

	// Return the size of the most recent snapshot
	return snapshots[len(snapshots)-1].Size, nil
}

// Checksum returns a checksum for the backup (using snapshot ID)
func (b *RusticS3Backup) Checksum() ([]byte, error) {
	snapshots, err := b.listSnapshots(context.Background())
	if err != nil {
		return nil, errors.Wrap(err, "failed to get backup checksum")
	}

	if len(snapshots) == 0 {
		return nil, errors.New("no snapshots found for checksum")
	}

	// Use the snapshot ID as the checksum
	return []byte(snapshots[len(snapshots)-1].ID), nil
}

// Details returns details about the rustic S3 backup archive
func (b *RusticS3Backup) Details(ctx context.Context, parts []remote.BackupPart) (*ArchiveDetails, error) {
	ad := ArchiveDetails{ChecksumType: "sha1", Parts: parts}

	// Get the size and checksum from the rustic snapshot
	size, err := b.Size()
	if err != nil {
		return nil, errors.Wrap(err, "failed to get rustic S3 backup size")
	}
	ad.Size = size

	checksum, err := b.Checksum()
	if err != nil {
		return nil, errors.Wrap(err, "failed to get rustic S3 backup checksum")
	}
	ad.Checksum = string(checksum)

	return &ad, nil
}

// Path returns a virtual path for this backup
func (b *RusticS3Backup) Path() string {
	cfg := config.Get()
	return fmt.Sprintf("rustic-s3://%s/%s/%s", cfg.System.Backups.Rustic.S3.Bucket, cfg.System.Backups.Rustic.S3.Prefix, b.Uuid)
}
