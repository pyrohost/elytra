package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"emperror.dev/errors"

	"github.com/pyrohost/elytra/src/config"
	"github.com/pyrohost/elytra/src/internal/rustic"
	"github.com/pyrohost/elytra/src/remote"
	"github.com/pyrohost/elytra/src/server/filesystem"
)

// Constants for rustic operations
const (
	DefaultTimeout       = 30 * time.Minute
	SnapshotIDPattern    = `^[a-f0-9]{8}([a-f0-9]{56})?$`
	SnapshotSavedPattern = `snapshot\s+([a-f0-9]{8,64})\s+successfully\s+saved`
	TempPrefix           = "elytra-rustic-"
	SecureFileMode       = 0600
	SecureDirMode        = 0700
)

var (
	snapshotIDRegex    = regexp.MustCompile(SnapshotIDPattern)
	snapshotSavedRegex = regexp.MustCompile(SnapshotSavedPattern)
)

// Config represents a complete rustic backup configuration
type Config struct {
	BackupUUID     string
	ServerUUID     string
	Password       string
	BackupType     string               // "local" or "s3"
	S3Config       *S3Config            `json:"s3_config,omitempty"`
	LocalPath      string               `json:"local_path,omitempty"`
	IgnorePatterns string               `json:"ignore_patterns,omitempty"`
	Tags           map[string]string    `json:"tags,omitempty"`
}

// S3Config holds S3-specific configuration
type S3Config struct {
	Bucket          string `json:"bucket"`
	Region          string `json:"region"`
	Endpoint        string `json:"endpoint,omitempty"`
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	SessionToken    string `json:"session_token,omitempty"`
	ForcePathStyle  bool   `json:"force_path_style"`
}

// Snapshot represents a backup snapshot
type Snapshot struct {
	ID         string            `json:"id"`
	BackupUUID string            `json:"backup_uuid"`
	Size       int64             `json:"size"`
	CreatedAt  time.Time         `json:"created_at"`
	Tags       map[string]string `json:"tags"`
	Paths      []string          `json:"paths"`
}

// Repository defines the interface for rustic repository operations
type Repository interface {
	// Repository lifecycle
	Initialize(ctx context.Context) error
	Exists(ctx context.Context) (bool, error)
	Info(ctx context.Context) (*RepositoryInfo, error)

	// Snapshot operations
	CreateSnapshot(ctx context.Context, path string, tags map[string]string, ignoreFile string) (*Snapshot, error)
	GetSnapshot(ctx context.Context, id string) (*Snapshot, error)
	ListSnapshots(ctx context.Context, filter map[string]string) ([]*Snapshot, error)
	DeleteSnapshot(ctx context.Context, id string) error

	// Data operations
	RestoreSnapshot(ctx context.Context, snapshotID string, targetPath string, sourcePath string) error
	GetRepositorySize(ctx context.Context) (int64, error)
	GetSnapshotSizes(ctx context.Context) (map[string]int64, error)

	// Maintenance
	Destroy(ctx context.Context) error

	// Cleanup
	Close() error
}

// RepositoryInfo contains repository metadata
type RepositoryInfo struct {
	TotalSize     int64 `json:"total_size"`
	SnapshotCount int   `json:"snapshot_count"`
	LastUpdate    time.Time `json:"last_update"`
}

// RusticBackup implements BackupInterface using rustic
type RusticBackup struct {
	Backup
	config     Config
	repository Repository
	snapshot   *Snapshot
}

// SnapshotInfo represents detailed snapshot metadata from rustic JSON output
type SnapshotInfo struct {
	ID             string    `json:"id"`
	Time           time.Time `json:"time"`
	ProgramVersion string    `json:"program_version"`
	Tree           string    `json:"tree"`
	Paths          []string  `json:"paths"`
	Hostname       string    `json:"hostname"`
	Username       string    `json:"username"`
	UID            int       `json:"uid"`
	GID            int       `json:"gid"`
	Tags           []string  `json:"tags,omitempty"`
	Summary        *struct {
		DataAdded       int64 `json:"data_added"`
		DataAddedPacked int64 `json:"data_added_packed"`
	} `json:"summary,omitempty"`
}

// === FACTORY FUNCTIONS ===

// NewRusticBackup creates a new rustic backup with the given configuration
func NewRusticBackup(client remote.Client, cfg Config) (*RusticBackup, error) {
	if err := validateConfig(cfg); err != nil {
		return nil, errors.Wrap(err, "invalid configuration")
	}

	// Create repository based on backup type
	var repo Repository
	var err error

	switch cfg.BackupType {
	case "local":
		repo, err = NewLocalRepository(cfg)
	case "s3":
		repo, err = NewS3Repository(cfg)
	default:
		return nil, errors.Errorf("unsupported backup type: %s", cfg.BackupType)
	}

	if err != nil {
		return nil, errors.Wrap(err, "failed to create repository")
	}

	backup := &RusticBackup{
		Backup: Backup{
			client:  client,
			Uuid:    cfg.BackupUUID,
			Ignore:  cfg.IgnorePatterns,
			adapter: AdapterType(fmt.Sprintf("rustic_%s", cfg.BackupType)),
		},
		config:     cfg,
		repository: repo,
	}

	// Set finalizer for cleanup
	runtime.SetFinalizer(backup, (*RusticBackup).finalize)

	return backup, nil
}

// GetRepository returns the underlying repository for this backup
func (b *RusticBackup) GetRepository() Repository {
	return b.repository
}

// LocateRusticBackup finds an existing backup by snapshot ID
func LocateRusticBackup(client remote.Client, cfg Config, snapshotID string) (*RusticBackup, error) {
	backup, err := NewRusticBackup(client, cfg)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Get the snapshot
	snapshot, err := backup.repository.GetSnapshot(ctx, snapshotID)
	if err != nil {
		backup.Close()
		return nil, errors.Wrap(err, "snapshot not found")
	}

	backup.snapshot = snapshot
	return backup, nil
}

// === BACKUP INTERFACE IMPLEMENTATION ===

// WithLogContext attaches additional context to the log output for this backup
func (r *RusticBackup) WithLogContext(ctx map[string]interface{}) {
	r.logContext = ctx
}

// Generate creates a new backup
// NOTE: The caller is responsible for calling Close() after using the backup and repository
func (r *RusticBackup) Generate(ctx context.Context, fsys *filesystem.Filesystem, ignore string) (*ArchiveDetails, error) {
	// Validate filesystem path
	if err := validatePath(fsys.Path()); err != nil {
		return nil, errors.Wrap(err, "invalid filesystem path")
	}

	// Initialize repository
	if err := r.repository.Initialize(ctx); err != nil {
		return nil, errors.Wrap(err, "failed to initialize repository")
	}

	// Create ignore file
	ignoreFile, err := createIgnoreFile(ignore)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create ignore file")
	}
	defer os.Remove(ignoreFile)

	// Prepare tags
	tags := map[string]string{
		"backup_uuid": r.config.BackupUUID,
		"server_uuid": r.config.ServerUUID,
		"type":        "server_backup",
	}

	// Add custom tags
	for k, v := range r.config.Tags {
		tags[k] = v
	}

	// Create snapshot
	snapshot, err := r.repository.CreateSnapshot(ctx, fsys.Path(), tags, ignoreFile)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create snapshot")
	}

	r.snapshot = snapshot
	r.log().WithField("snapshot_id", snapshot.ID).Info("backup created successfully")

	// Return details
	return r.Details(ctx, nil)
}

// Remove deletes a backup
func (r *RusticBackup) Remove() error {
	if r.snapshot == nil {
		// Failed backup with no snapshot - nothing to delete in rustic
		r.log().Info("no snapshot to remove (failed backup)")
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), DefaultTimeout)
	defer cancel()

	err := r.repository.DeleteSnapshot(ctx, r.snapshot.ID)
	if err != nil {
		errStr := err.Error()
		// For failed backups, snapshot might not exist - handle only true "not found" cases
		if strings.Contains(errStr, "not found") ||
			strings.Contains(errStr, "does not exist") ||
			strings.Contains(errStr, "no such snapshot") {
			r.log().WithField("snapshot_id", r.snapshot.ID).Info("snapshot not found, assuming failed backup")
			return nil
		}
		// For other errors (like cryptographic issues), check if it's a backup UUID vs snapshot ID issue
		if len(r.snapshot.ID) == 36 && strings.Count(r.snapshot.ID, "-") == 4 {
			// This looks like a backup UUID, not a snapshot ID - try to find the actual snapshot
			r.log().WithField("backup_uuid", r.snapshot.ID).Warn("attempting deletion with backup UUID instead of snapshot ID")
			return errors.Wrap(err, "deletion failed - backup UUID used instead of snapshot ID")
		}
		return errors.Wrap(err, "failed to delete snapshot")
	}

	r.log().WithField("snapshot_id", r.snapshot.ID).Info("snapshot deleted successfully")
	return nil
}

// Size returns the backup size
func (r *RusticBackup) Size() (int64, error) {
	if r.snapshot == nil {
		return 0, errors.New("no snapshot available")
	}

	if r.snapshot.Size > 0 {
		return r.snapshot.Size, nil
	}

	return 1024, nil // Minimum size for empty snapshots
}

// Checksum returns a checksum for the backup
func (r *RusticBackup) Checksum() ([]byte, error) {
	if r.snapshot == nil {
		return nil, errors.New("no snapshot available")
	}

	h := sha1.New()
	h.Write([]byte(r.snapshot.ID))
	return h.Sum(nil), nil
}

// Details returns backup details
func (r *RusticBackup) Details(ctx context.Context, parts []remote.BackupPart) (*ArchiveDetails, error) {
	checksum, err := r.Checksum()
	if err != nil {
		return nil, err
	}

	size, err := r.Size()
	if err != nil {
		return nil, err
	}

	snapshotID := ""
	if r.snapshot != nil {
		snapshotID = r.snapshot.ID
	}

	return &ArchiveDetails{
		Checksum:     hex.EncodeToString(checksum),
		ChecksumType: "sha1",
		Size:         size,
		Parts:        parts,
		SnapshotId:   snapshotID,
	}, nil
}

// Path returns a placeholder path for rustic backups
func (r *RusticBackup) Path() string {
	if r.snapshot == nil {
		return fmt.Sprintf("rustic-backup-%s", r.config.BackupUUID)
	}
	return fmt.Sprintf("rustic-snapshot-%s", r.snapshot.ID)
}

// Restore restores files from backup
func (r *RusticBackup) Restore(ctx context.Context, reader io.Reader, callback RestoreCallback) error {
	defer r.Close()

	if r.snapshot == nil {
		return errors.New("no snapshot to restore")
	}

	// Create temp directory for restoration
	tempDir, err := os.MkdirTemp("", TempPrefix+"restore-")
	if err != nil {
		return errors.Wrap(err, "failed to create temp directory")
	}
	defer os.RemoveAll(tempDir)

	// Get original backup path (first path in snapshot)
	if len(r.snapshot.Paths) == 0 {
		return errors.New("no paths found in snapshot")
	}
	originalPath := r.snapshot.Paths[0]

	// Restore snapshot
	err = r.repository.RestoreSnapshot(ctx, r.snapshot.ID, tempDir, originalPath)
	if err != nil {
		return errors.Wrap(err, "failed to restore snapshot")
	}

	// Walk restored files and call callback
	return filepath.Walk(tempDir, func(path string, info fs.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}

		relPath, err := filepath.Rel(tempDir, path)
		if err != nil {
			return err
		}

		if strings.Contains(relPath, "..") {
			return nil // Skip unsafe paths
		}

		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()

		return callback(relPath, info, file)
	})
}

// CanDownload returns true if this backup can be downloaded as a tar.gz
func (r *RusticBackup) CanDownload() bool {
	return r.snapshot != nil && r.snapshot.ID != ""
}

// DownloadTarGz generates a tar.gz archive from the backup and writes it to the provided writer
func (r *RusticBackup) DownloadTarGz(ctx context.Context, writer io.Writer) error {
	if !r.CanDownload() {
		return errors.New("backup cannot be downloaded")
	}

	// Create temp directory for extraction
	tempDir, err := os.MkdirTemp("", TempPrefix+"download-")
	if err != nil {
		return errors.Wrap(err, "failed to create temp directory for download")
	}
	defer os.RemoveAll(tempDir)

	// Get original backup path (first path in snapshot)
	if len(r.snapshot.Paths) == 0 {
		return errors.New("no paths found in snapshot")
	}
	originalPath := r.snapshot.Paths[0]

	// Restore snapshot to temp directory
	err = r.repository.RestoreSnapshot(ctx, r.snapshot.ID, tempDir, originalPath)
	if err != nil {
		return errors.Wrap(err, "failed to restore snapshot for download")
	}

	// Create tar.gz archive from the restored files
	return createTarGzArchive(tempDir, writer)
}

// Close cleans up resources
func (r *RusticBackup) Close() error {
	var err error
	if r.repository != nil {
		err = r.repository.Close()
		r.repository = nil
	}
	runtime.SetFinalizer(r, nil)
	return err
}

// finalize is called by the runtime finalizer
func (r *RusticBackup) finalize() {
	if r.repository != nil {
		r.log().Warn("finalizer cleaning up rustic backup - Close() was not called")
		r.repository.Close()
	}
}

// === UTILITY FUNCTIONS ===

// validateConfig validates the backup configuration
func validateConfig(cfg Config) error {
	// BackupUUID is only required for backup creation, not for deletion/restore operations
	// For deletion/restore, we use snapshot ID instead

	if cfg.Password == "" {
		return errors.New("repository password is required")
	}

	switch cfg.BackupType {
	case "local":
		if cfg.LocalPath == "" {
			return errors.New("local path is required for local backups")
		}
	case "s3":
		if cfg.S3Config == nil {
			return errors.New("S3 configuration is required for S3 backups")
		}
		if cfg.S3Config.Bucket == "" {
			return errors.New("S3 bucket is required")
		}
		if cfg.S3Config.AccessKeyID == "" || cfg.S3Config.SecretAccessKey == "" {
			return errors.New("S3 credentials are required")
		}
		if cfg.ServerUUID == "" {
			return errors.New("server UUID is required for S3 backups")
		}
	default:
		return errors.Errorf("unsupported backup type: %s", cfg.BackupType)
	}

	return nil
}

// validatePath validates filesystem paths for security
func validatePath(path string) error {
	if path == "" {
		return errors.New("path cannot be empty")
	}

	if strings.Contains(path, "..") {
		return errors.New("path contains directory traversal")
	}

	if !filepath.IsAbs(path) {
		return errors.New("path must be absolute")
	}

	if _, err := os.Stat(path); err != nil {
		return errors.Wrap(err, "path not accessible")
	}

	return nil
}

// createIgnoreFile creates a temporary ignore file
func createIgnoreFile(ignore string) (string, error) {
	if ignore == "" {
		return "", nil // No ignore patterns
	}

	tempFile, err := os.CreateTemp("", TempPrefix+"ignore-")
	if err != nil {
		return "", err
	}
	defer tempFile.Close()

	if err := os.Chmod(tempFile.Name(), SecureFileMode); err != nil {
		os.Remove(tempFile.Name())
		return "", err
	}

	if _, err := tempFile.WriteString(ignore); err != nil {
		os.Remove(tempFile.Name())
		return "", err
	}

	return tempFile.Name(), nil
}

// isValidSnapshotID validates snapshot ID format
func isValidSnapshotID(id string) bool {
	// Allow backup UUIDs (with dashes) for deletion scenarios
	if len(id) == 36 && strings.Count(id, "-") == 4 {
		return true
	}
	// Standard rustic snapshot ID format (8-64 char hex)
	return snapshotIDRegex.MatchString(id)
}

// extractSnapshotID extracts snapshot ID from rustic output
func extractSnapshotID(output string) string {
	matches := snapshotSavedRegex.FindStringSubmatch(output)
	if len(matches) >= 2 {
		return matches[1]
	}
	return ""
}

// parseTags converts rustic tag slice to map
func parseTags(tags []string) map[string]string {
	result := make(map[string]string)
	for _, tag := range tags {
		if parts := strings.SplitN(tag, ":", 2); len(parts) == 2 {
			result[parts[0]] = parts[1]
		} else {
			result[tag] = ""
		}
	}
	return result
}

// formatTags converts tag map to rustic tag slice
func formatTags(tags map[string]string) []string {
	var result []string
	for k, v := range tags {
		if v == "" {
			result = append(result, k)
		} else {
			result = append(result, fmt.Sprintf("%s:%s", k, v))
		}
	}
	return result
}

// getRusticBinary returns the path to the rustic binary
func getRusticBinary() (string, error) {
	// Try system PATH first
	if path, err := rustic.GetBinaryPath(); err == nil {
		return path, nil
	}

	// Fall back to configured path
	cfg := config.Get().System.Backups.Rustic
	if cfg.BinaryPath != "" {
		return cfg.BinaryPath, nil
	}

	return "", errors.New("rustic binary not found")
}

// getCacheDir returns the cache directory for rustic
func getCacheDir() string {
	cfg := config.Get().System.Backups.Rustic
	if cfg.Local.RepositoryPath != "" {
		repoBasePath := filepath.Dir(cfg.Local.RepositoryPath)
		return filepath.Join(repoBasePath, "rustic-cache")
	}
	// Always use a dedicated rustic-cache directory for S3 backups
	return "/var/lib/pterodactyl/rustic-cache"
}

// createTarGzArchive creates a tar.gz archive from a directory and writes it to the provided writer
func createTarGzArchive(sourceDir string, writer io.Writer) error {
	// Create gzip writer
	gzWriter := gzip.NewWriter(writer)
	defer gzWriter.Close()

	// Create tar writer
	tarWriter := tar.NewWriter(gzWriter)
	defer tarWriter.Close()

	// Walk the source directory and add files to the archive
	return filepath.Walk(sourceDir, func(path string, info fs.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Skip directories
		if info.IsDir() {
			return nil
		}

		// Calculate relative path
		relPath, err := filepath.Rel(sourceDir, path)
		if err != nil {
			return err
		}

		// Skip unsafe paths
		if strings.Contains(relPath, "..") {
			return nil
		}

		// Create tar header
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = relPath

		// Write header
		if err := tarWriter.WriteHeader(header); err != nil {
			return err
		}

		// Open and copy file content
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()

		_, err = io.Copy(tarWriter, file)
		return err
	})
}