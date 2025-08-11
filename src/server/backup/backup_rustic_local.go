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

type RusticLocalBackup struct {
	Backup
}

var _ BackupInterface = (*RusticLocalBackup)(nil)

// RusticSnapshot represents a rustic snapshot in JSON format
type RusticSnapshot struct {
	ID       string    `json:"id"`
	Time     time.Time `json:"time"`
	Hostname string    `json:"hostname"`
	Username string    `json:"username"`
	Tags     []string  `json:"tags"`
	Paths    []string  `json:"paths"`
	Size     int64     `json:"size"`
	Summary  struct {
		FilesNew            int     `json:"files_new"`
		FilesChanged        int     `json:"files_changed"`
		FilesUnmodified     int     `json:"files_unmodified"`
		DirsNew             int     `json:"dirs_new"`
		DirsChanged         int     `json:"dirs_changed"`
		DirsUnmodified      int     `json:"dirs_unmodified"`
		DataBlobs           int     `json:"data_blobs"`
		TreeBlobs           int     `json:"tree_blobs"`
		DataAdded           int64   `json:"data_added"`
		TotalFilesProcessed int     `json:"total_files_processed"`
		TotalBytesProcessed int64   `json:"total_bytes_processed"`
		TotalDuration       float64 `json:"total_duration"`
	} `json:"summary"`
}

func NewRusticLocal(client remote.Client, uuid, ignore string) *RusticLocalBackup {
	return &RusticLocalBackup{
		Backup{
			client:  client,
			Uuid:    uuid,
			Ignore:  ignore,
			adapter: RusticLocalBackupAdapter,
		},
	}
}

// LocateRusticLocal finds a rustic backup for a server and returns the backup instance
func LocateRusticLocal(client remote.Client, uuid string) (*RusticLocalBackup, error) {
	b := NewRusticLocal(client, uuid, "")

	// Check if the backup exists in the rustic repository
	snapshots, err := b.listSnapshots(context.Background())
	if err != nil {
		return nil, errors.Wrap(err, "failed to locate rustic backup")
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
		return nil, errors.New("backup not found in rustic repository")
	}

	return b, nil
}

// Remove removes a backup from the rustic repository
func (b *RusticLocalBackup) Remove() error {
	if err := b.validateConfig(); err != nil {
		return errors.Wrap(err, "invalid rustic local configuration")
	}

	cfg := config.Get()

	args := []string{
		"forget",
		"--filter-tags", fmt.Sprintf("backup_uuid=%s", b.Uuid),
	}

	if cfg.System.Backups.Rustic.Local.RetentionPolicy.PruneAfterForget {
		args = append(args, "--prune")
	}

	cmd := exec.Command("rustic", args...)
	cmd.Env = b.buildEnv()

	if err := cmd.Run(); err != nil {
		return errors.Wrap(err, "failed to remove rustic backup")
	}

	return nil
}

// WithLogContext attaches additional context to the log output for this backup
func (b *RusticLocalBackup) WithLogContext(c map[string]interface{}) {
	b.logContext = c
}

// Generate creates a backup using rustic
func (b *RusticLocalBackup) Generate(ctx context.Context, fsys *filesystem.Filesystem, ignore string) (*ArchiveDetails, error) {
	cfg := config.Get()
	if !cfg.System.Backups.Rustic.Enabled {
		return nil, errors.New("rustic backups are not enabled")
	}

	// Ensure repository exists
	if err := b.initRepository(); err != nil {
		return nil, errors.Wrap(err, "failed to initialize rustic repository")
	}

	// Create ignore file if needed
	ignoreFile := ""
	if ignore != "" {
		ignoreFile = filepath.Join(os.TempDir(), fmt.Sprintf("rustic-ignore-%s", b.Uuid))
		if err := os.WriteFile(ignoreFile, []byte(ignore), 0o600); err != nil {
			return nil, errors.Wrap(err, "failed to create ignore file")
		}
		defer os.Remove(ignoreFile)
	}

	b.log().WithField("repository", cfg.System.Backups.Rustic.Local.Path).Info("creating rustic backup for server")

	// Build rustic backup command
	args := []string{
		"backup",
		"--tag", fmt.Sprintf("backup_uuid=%s", b.Uuid),
		"--tag", "server_uuid=unknown",
		"--tag", "source=elytra",
		"--label", fmt.Sprintf("elytra_backup_%s", b.Uuid),
		"--json",
	}

	if ignoreFile != "" {
		args = append(args, "--glob-file", ignoreFile)
	}

	args = append(args, fsys.Path())

	cmd := exec.CommandContext(ctx, "rustic", args...)
	cmd.Env = b.buildEnv()

	// Set a reasonable timeout for backup operations
	if ctx.Err() == nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 24*time.Hour) // 24 hour max backup time
		defer cancel()
		cmd = exec.CommandContext(ctx, "rustic", args...)
		cmd.Env = b.buildEnv()
	}

	output, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			b.log().Error("rustic backup timed out after 24 hours")
			return nil, errors.New("rustic backup timed out")
		}
		b.log().WithError(err).WithField("output", string(output)).Error("rustic backup failed")
		return nil, errors.Wrap(err, "rustic backup command failed")
	}

	b.log().Info("created rustic backup successfully")

	// Get backup details
	ad, err := b.Details(ctx, nil)
	if err != nil {
		return nil, errors.WrapIf(err, "backup: failed to get archive details for rustic backup")
	}

	return ad, nil
}

// Restore restores files from a rustic backup
func (b *RusticLocalBackup) Restore(ctx context.Context, _ io.Reader, callback RestoreCallback) error {
	cfg := config.Get()
	if !cfg.System.Backups.Rustic.Enabled {
		return errors.New("rustic backups are not enabled")
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
	tempDir := filepath.Join(os.TempDir(), fmt.Sprintf("rustic-restore-%s", b.Uuid))
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
			return errors.New("rustic restore timed out after 12 hours")
		}
		return errors.Wrap(err, "failed to restore rustic backup")
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

// initRepository initializes the rustic repository if it doesn't exist
func (b *RusticLocalBackup) initRepository() error {
	cfg := config.Get()

	// Check if repository exists
	cmd := exec.Command("rustic", "snapshots", "--json")
	cmd.Env = b.buildEnv()

	if err := cmd.Run(); err != nil {
		// Repository doesn't exist, initialize it
		b.log().Info("initializing new rustic repository")

		// Ensure directory exists
		if err := os.MkdirAll(cfg.System.Backups.Rustic.Local.Path, 0o755); err != nil {
			return errors.Wrap(err, "failed to create repository directory")
		}

		initArgs := []string{"init"}
		if cfg.System.Backups.Rustic.CompressionLevel > 0 {
			initArgs = append(initArgs, "--set-compression", strconv.Itoa(cfg.System.Backups.Rustic.CompressionLevel))
		}

		cmd = exec.Command("rustic", initArgs...)
		cmd.Env = b.buildEnv()

		if err := cmd.Run(); err != nil {
			return errors.Wrap(err, "failed to initialize rustic repository")
		}

		b.log().Info("rustic repository initialized successfully")
	}

	return nil
}

// buildEnv builds the environment variables for rustic commands
func (b *RusticLocalBackup) buildEnv() []string {
	cfg := config.Get()
	env := os.Environ()

	// Set rustic repository to local path
	env = append(env, fmt.Sprintf("RUSTIC_REPOSITORY=%s", cfg.System.Backups.Rustic.Local.Path))

	// Set password
	if cfg.System.Backups.Rustic.Password != "" {
		env = append(env, fmt.Sprintf("RUSTIC_PASSWORD=%s", cfg.System.Backups.Rustic.Password))
	} else if cfg.System.Backups.Rustic.PasswordFile != "" {
		env = append(env, fmt.Sprintf("RUSTIC_PASSWORD_FILE=%s", cfg.System.Backups.Rustic.PasswordFile))
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
func (b *RusticLocalBackup) listSnapshots(ctx context.Context) ([]RusticSnapshot, error) {
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
func (b *RusticLocalBackup) Size() (int64, error) {
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
func (b *RusticLocalBackup) Checksum() ([]byte, error) {
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

// validateConfig validates the local rustic configuration
func (b *RusticLocalBackup) validateConfig() error {
	cfg := config.Get()

	if !cfg.System.Backups.Rustic.Enabled {
		return errors.New("rustic backups are not enabled")
	}

	// Check if rustic binary is available
	if _, err := exec.LookPath("rustic"); err != nil {
		return errors.Wrap(err, "rustic binary not found in PATH")
	}

	if cfg.System.Backups.Rustic.Local.Path == "" {
		return errors.New("local repository path not configured for rustic backups")
	}

	if cfg.System.Backups.Rustic.Password == "" && cfg.System.Backups.Rustic.PasswordFile == "" {
		return errors.New("rustic repository password not configured")
	}

	// Validate compression level
	if cfg.System.Backups.Rustic.CompressionLevel < 0 || cfg.System.Backups.Rustic.CompressionLevel > 22 {
		return errors.New("rustic compression level must be between 0 and 22")
	}

	return nil
}

// Details returns details about the rustic backup archive
func (b *RusticLocalBackup) Details(ctx context.Context, parts []remote.BackupPart) (*ArchiveDetails, error) {
	ad := ArchiveDetails{ChecksumType: "sha1", Parts: parts}

	// Get the size and checksum from the rustic snapshot
	size, err := b.Size()
	if err != nil {
		return nil, errors.Wrap(err, "failed to get rustic backup size")
	}
	ad.Size = size

	checksum, err := b.Checksum()
	if err != nil {
		return nil, errors.Wrap(err, "failed to get rustic backup checksum")
	}
	ad.Checksum = string(checksum)

	return &ad, nil
}

// Path returns a virtual path for this backup
func (b *RusticLocalBackup) Path() string {
	cfg := config.Get()
	return fmt.Sprintf("rustic://%s/%s", cfg.System.Backups.Rustic.Local.Path, b.Uuid)
}
