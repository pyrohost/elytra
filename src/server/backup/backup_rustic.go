package backup

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
	// Command timeout for rustic operations
	rusticCommandTimeout = 30 * time.Minute
	// Snapshot ID validation pattern (8 or 64 character hex)
	snapshotIDPattern = `^[a-f0-9]{8}([a-f0-9]{56})?$`
	// Output parsing patterns
	snapshotSavedPattern = `snapshot\s+([a-f0-9]{8,64})\s+successfully\s+saved`
	// Secure temp directory prefix
	tempDirPrefix = "rustic-secure-"
	// File permissions for secure temp files
	secureTempFileMode = 0600
	secureTempDirMode  = 0700
)

var (
	// Compiled regex for snapshot ID validation
	snapshotIDRegex = regexp.MustCompile(snapshotIDPattern)
	// Compiled regex for output parsing
	snapshotSavedRegex = regexp.MustCompile(snapshotSavedPattern)
)

type RusticBackup struct {
	Backup
	backupType         string
	s3Credentials      *remote.S3Credentials
	repositoryPath     string
	repositoryPassword string
	snapshotID         string
	// Panel-provided repository path
	panelRepositoryPath string
	// Temporary credential file for secure S3 access
	credentialFile string
	// Server UUID for repository deduplication (from panel)
	serverUuid string
}

// RusticSnapshotInfo represents snapshot information from rustic JSON output
type RusticSnapshotInfo struct {
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
	Original       string    `json:"original,omitempty"`
	Summary        *struct {
		DataAdded       int64 `json:"data_added"`
		DataAddedPacked int64 `json:"data_added_packed"`
	} `json:"summary,omitempty"`
}

// RusticGroupMetadata represents group metadata in rustic JSON output
type RusticGroupMetadata struct {
	Hostname string   `json:"hostname"`
	Label    string   `json:"label"`
	Paths    []string `json:"paths"`
}

// RusticSnapshotGroup represents the nested array structure from rustic
// Format: [ [GroupMetadata, [Snapshot, ...]], ... ]
type RusticSnapshotGroup [2]any // [0] = GroupMetadata, [1] = []Snapshot

var _ BackupInterface = (*RusticBackup)(nil)

// LocateRustic finds a rustic backup by snapshot ID and returns a backup instance
// This function checks if the snapshot exists in the repository before returning
func LocateRustic(client remote.Client, uuid string, backupType string, s3Creds *remote.S3Credentials, password string) (*RusticBackup, error) {
	r := NewRustic(client, uuid, "", backupType, s3Creds, password)

	// Set the snapshot ID to the provided UUID
	r.snapshotID = uuid

	// Validate snapshot ID format
	if !r.isValidSnapshotID(uuid) {
		return nil, errors.New("rustic: invalid snapshot ID format")
	}

	// Initialize repository path
	r.repositoryPath = r.getRepositoryPath()

	// Check if the snapshot exists in the repository
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	defer r.cleanup()

	exists, err := r.snapshotExists(ctx, uuid)
	if err != nil {
		return nil, errors.Wrap(err, "rustic: failed to check snapshot existence")
	}

	if !exists {
		return nil, errors.New("rustic: snapshot not found in repository")
	}

	return r, nil
}

// LocateRusticWithPath finds a rustic backup by backup UUID tag with Panel-provided path
func LocateRusticWithPath(client remote.Client, serverUuid string, backupUuid string, backupType string, s3Creds *remote.S3Credentials, password string, repoPath string) (*RusticBackup, error) {
	r := NewRusticWithServerPath(client, serverUuid, backupUuid, "", backupType, s3Creds, password, repoPath)

	// Initialize repository path
	r.repositoryPath = r.getRepositoryPath()

	// Find snapshot by backup UUID tag
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	defer r.cleanup()

	snapshotID, err := r.findSnapshotByBackupUuid(ctx, backupUuid)
	if err != nil {
		return nil, errors.Wrap(err, "rustic: failed to find snapshot by backup UUID")
	}

	if snapshotID == "" {
		return nil, errors.New("rustic: backup not found in repository")
	}

	// Set the found snapshot ID
	r.snapshotID = snapshotID

	return r, nil
}

// LocateRusticLocal finds a rustic local backup by snapshot ID
func LocateRusticLocal(client remote.Client, uuid string, password string) (*RusticBackup, error) {
	return LocateRustic(client, uuid, "local", nil, password)
}

// LocateRusticS3 finds a rustic S3 backup by snapshot ID
func LocateRusticS3(client remote.Client, uuid string, s3Creds *remote.S3Credentials, password string) (*RusticBackup, error) {
	return LocateRustic(client, uuid, "s3", s3Creds, password)
}

// NewRustic creates a new rustic backup instance
func NewRustic(client remote.Client, uuid string, ignore string, backupType string, s3Creds *remote.S3Credentials, password string) *RusticBackup {
	return &RusticBackup{
		Backup: Backup{
			client:  client,
			Uuid:    uuid,
			Ignore:  ignore,
			adapter: AdapterType(fmt.Sprintf("rustic_%s", backupType)),
		},
		backupType:         backupType,
		s3Credentials:      s3Creds,
		repositoryPassword: password,
	}
}

// NewRusticWithServerPath creates a new rustic backup instance with explicit server and backup UUIDs
func NewRusticWithServerPath(client remote.Client, serverUuid string, backupUuid string, ignore string, backupType string, s3Creds *remote.S3Credentials, password string, repoPath string) *RusticBackup {
	return &RusticBackup{
		Backup: Backup{
			client:  client,
			Uuid:    backupUuid,
			Ignore:  ignore,
			adapter: AdapterType(fmt.Sprintf("rustic_%s", backupType)),
		},
		backupType:          backupType,
		s3Credentials:       s3Creds,
		repositoryPassword:  password,
		panelRepositoryPath: repoPath,
		serverUuid:          serverUuid,
	}
}

// SetClient sets the API request client on the backup interface.
func (r *RusticBackup) SetClient(c remote.Client) {
	r.client = c
}

// WithLogContext attaches additional context to the log output for this backup.
func (r *RusticBackup) WithLogContext(c map[string]any) {
	r.logContext = c
}

// Remove removes a backup from the system.
func (r *RusticBackup) Remove() error {
	if r.snapshotID == "" {
		return nil
	}

	// Validate snapshot ID before attempting removal
	if !r.isValidSnapshotID(r.snapshotID) {
		return errors.New("rustic: invalid snapshot ID format")
	}

	ctx, cancel := context.WithTimeout(context.Background(), rusticCommandTimeout)
	defer cancel()
	defer r.cleanup()

	r.log().WithField("snapshot_id", r.snapshotID).Info("removing rustic snapshot with pruning")

	cmd := r.buildRusticCommandWithContext(ctx, "forget", "--prune", r.snapshotID)
	output, err := cmd.CombinedOutput()
	if err != nil {
		r.log().WithField("output", string(output)).
			WithField("snapshot_id", r.snapshotID).
			Error("rustic snapshot removal failed")
		return errors.Wrapf(err, "rustic: failed to remove snapshot: %s", string(output))
	}

	r.log().WithField("output", string(output)).
		WithField("snapshot_id", r.snapshotID).
		Info("rustic snapshot removed successfully")

	return nil
}

// Generate creates a backup using rustic
func (r *RusticBackup) Generate(ctx context.Context, fsys *filesystem.Filesystem, ignore string) (*ArchiveDetails, error) {
	// Validate filesystem path
	if err := r.validatePath(fsys.Path()); err != nil {
		return nil, errors.Wrap(err, "rustic: invalid filesystem path")
	}

	// Initialize repository if it doesn't exist
	if err := r.initializeRepository(ctx); err != nil {
		return nil, errors.Wrap(err, "rustic: failed to initialize repository")
	}

	// Create ignore file for rustic
	ignoreFile, err := r.createSecureIgnoreFile(ignore)
	if err != nil {
		return nil, errors.Wrap(err, "rustic: failed to create ignore file")
	}
	defer os.Remove(ignoreFile)
	defer r.cleanup()

	// Perform the backup
	r.log().WithField("path", fsys.Path()).Info("creating rustic backup for server")

	backupCtx, cancel := context.WithTimeout(ctx, rusticCommandTimeout)
	defer cancel()

	cmd := r.buildRusticCommandWithContext(backupCtx, "backup", "--tag", fmt.Sprintf("backup_uuid:%s", r.Uuid), fsys.Path())
	cmd.Env = append(cmd.Env, fmt.Sprintf("RUSTIC_IGNORE_FILE=%s", ignoreFile))

	r.log().WithField("backup_type", r.backupType).
		WithField("timeout", rusticCommandTimeout).
		WithField("command", fmt.Sprintf("rustic backup --tag backup_uuid:%s %s", r.Uuid, fsys.Path())).
		Info("starting rustic backup command")

	output, err := cmd.CombinedOutput()
	if err != nil {
		r.log().WithField("output", string(output)).Error("rustic backup command failed")
		return nil, errors.Wrapf(err, "rustic: backup failed: %s", string(output))
	}

	r.log().WithField("output", string(output)).Info("rustic backup command completed successfully")

	// Extract and validate snapshot ID from output
	shortID := r.extractSnapshotID(string(output))
	if shortID == "" || !r.isValidSnapshotID(shortID) {
		return nil, errors.New("rustic: failed to extract valid snapshot ID from backup output")
	}

	// Resolve short ID to full ID immediately after backup
	fullID, err := r.resolveFullSnapshotID(ctx, shortID)
	if err != nil {
		return nil, errors.Wrap(err, "rustic: failed to resolve full snapshot ID")
	}
	r.snapshotID = fullID

	r.log().WithField("snapshot_id", r.snapshotID).Info("rustic backup created successfully")

	// Get backup details
	ad, err := r.Details(ctx, nil)
	if err != nil {
		return nil, errors.WrapIf(err, "rustic: failed to get backup details")
	}

	return ad, nil
}

// Restore restores files from a rustic backup
func (r *RusticBackup) Restore(ctx context.Context, reader io.Reader, callback RestoreCallback) error {
	if r.snapshotID == "" {
		return errors.New("rustic: no snapshot ID available for restore")
	}

	// Validate snapshot ID
	if !r.isValidSnapshotID(r.snapshotID) {
		return errors.New("rustic: invalid snapshot ID format")
	}

	// Create secure temporary directory for restoration
	tempDir, err := r.createSecureTempDir()
	if err != nil {
		return errors.Wrap(err, "rustic: failed to create temp directory for restore")
	}
	defer os.RemoveAll(tempDir)
	defer r.cleanup()

	// Get the original backup path from snapshot metadata to restore just the server contents
	originalPath, err := r.getSnapshotOriginalPath(ctx)
	if err != nil {
		return errors.Wrap(err, "rustic: failed to get original backup path for restore")
	}

	// Restore snapshot to temporary directory with timeout
	restoreCtx, cancel := context.WithTimeout(ctx, rusticCommandTimeout)
	defer cancel()

	cmd := r.buildRusticCommandWithContext(restoreCtx, "restore", fmt.Sprintf("%s:%s", r.snapshotID, originalPath), tempDir)
	if err := cmd.Run(); err != nil {
		return errors.Wrap(err, "rustic: failed to restore snapshot")
	}

	// Walk the restored files and call callback for each
	return filepath.Walk(tempDir, func(path string, info fs.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if info.IsDir() {
			return nil
		}

		// Calculate relative path
		relPath, err := filepath.Rel(tempDir, path)
		if err != nil {
			return err
		}

		// Validate relative path to prevent directory traversal
		if strings.Contains(relPath, "..") {
			r.log().WithField("path", relPath).Warn("rustic: skipping potentially unsafe path")
			return nil
		}

		// Open file and call callback
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()

		return callback(relPath, info, file)
	})
}

// Path returns a placeholder path for rustic backups
func (r *RusticBackup) Path() string {
	return fmt.Sprintf("%s/snapshot-%s", r.repositoryPath, r.snapshotID)
}

// Size returns the size of the backup
func (r *RusticBackup) Size() (int64, error) {
	if r.snapshotID == "" {
		return 0, errors.New("rustic: no snapshot ID available")
	}

	// Validate snapshot ID
	if !r.isValidSnapshotID(r.snapshotID) {
		return 0, errors.New("rustic: invalid snapshot ID format")
	}

	ctx, cancel := context.WithTimeout(context.Background(), rusticCommandTimeout)
	defer cancel()
	defer r.cleanup()

	cmd := r.buildRusticCommandWithContext(ctx, "snapshots", "--json", r.snapshotID)
	output, err := cmd.Output()
	if err != nil {
		return 0, errors.Wrap(err, "rustic: failed to get snapshot info")
	}

	var snapshots []RusticSnapshotInfo
	if err := json.Unmarshal(output, &snapshots); err != nil {
		return 0, errors.Wrap(err, "rustic: failed to parse snapshots JSON")
	}

	for _, snapshot := range snapshots {
		if snapshot.ID == r.snapshotID ||
			snapshot.Original == r.snapshotID ||
			(len(r.snapshotID) == 8 && strings.HasPrefix(snapshot.ID, r.snapshotID)) ||
			(len(r.snapshotID) == 8 && strings.HasPrefix(snapshot.Original, r.snapshotID)) {
			if snapshot.Summary != nil {
				return snapshot.Summary.DataAdded, nil
			}
		}
	}

	return 0, errors.New("rustic: snapshot not found or size unavailable")
}

// Checksum returns a checksum for the backup
func (r *RusticBackup) Checksum() ([]byte, error) {
	if r.snapshotID == "" {
		return nil, errors.New("rustic: no snapshot ID available")
	}

	// Use snapshot ID as the basis for checksum
	h := sha1.New()
	h.Write([]byte(r.snapshotID))
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

	return &ArchiveDetails{
		Checksum:     hex.EncodeToString(checksum),
		ChecksumType: "sha1",
		Size:         size,
		Parts:        parts,
		SnapshotId:   r.snapshotID,
	}, nil
}

// initializeRepository initializes the rustic repository
func (r *RusticBackup) initializeRepository(ctx context.Context) error {
	r.repositoryPath = r.getRepositoryPath()

	// Validate repository path
	if err := r.validateRepositoryPath(r.repositoryPath); err != nil {
		return errors.Wrap(err, "rustic: invalid repository path")
	}

	// Check if repository already exists
	if exists, err := r.repositoryExists(ctx); err != nil {
		return errors.Wrap(err, "rustic: failed to check repository existence")
	} else if exists {
		return nil
	}

	// Create repository directory for local repositories
	if r.backupType == "local" {
		if err := os.MkdirAll(r.repositoryPath, secureTempDirMode); err != nil {
			return errors.Wrap(err, "rustic: failed to create repository directory")
		}
	}

	// Initialize repository with timeout
	initCtx, cancel := context.WithTimeout(ctx, rusticCommandTimeout)
	defer cancel()

	cfg := config.Get().System.Backups.Rustic
	cmd := r.buildRusticCommandWithContext(initCtx, "init")

	if cfg.RepositoryVersion > 0 {
		cmd.Args = append(cmd.Args, "--set-version", fmt.Sprintf("%d", cfg.RepositoryVersion))
	}

	// Set pack sizes for performance tuning (defaults: 4MB for tree, 32MB for data)
	if cfg.TreePackSizeMB > 0 {
		cmd.Args = append(cmd.Args, "--set-treepack-size", fmt.Sprintf("%dMiB", cfg.TreePackSizeMB))
	}
	if cfg.DataPackSizeMB > 0 {
		cmd.Args = append(cmd.Args, "--set-datapack-size", fmt.Sprintf("%dMiB", cfg.DataPackSizeMB))
	}

	r.log().WithField("backup_type", r.backupType).
		WithField("repository", r.repositoryPath).
		Info("starting rustic repository initialization")

	output, err := cmd.CombinedOutput()
	if err != nil {
		r.log().WithField("backup_type", r.backupType).
			WithField("repository", r.repositoryPath).
			WithField("rustic_output", string(output)).
			WithError(err).Error("rustic repository initialization failed")
		return errors.Wrapf(err, "rustic: failed to initialize repository: %s", string(output))
	}

	r.log().WithField("backup_type", r.backupType).
		WithField("repository", r.repositoryPath).
		Info("rustic repository initialized successfully")
	return nil
}

// repositoryExists checks if the repository already exists
func (r *RusticBackup) repositoryExists(ctx context.Context) (bool, error) {
	checkCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	defer r.cleanup()

	cmd := r.buildRusticCommandWithContext(checkCtx, "snapshots", "--json")
	_, err := cmd.Output()
	if err != nil {
		// If command fails, repository likely doesn't exist
		return false, nil
	}
	return true, nil
}

// getRepositoryPath returns the path for the repository
func (r *RusticBackup) getRepositoryPath() string {
	// Panel always provides the repository path - no fallbacks
	return r.panelRepositoryPath
}

// buildRusticCommandWithContext builds a rustic command with context and timeout
func (r *RusticBackup) buildRusticCommandWithContext(ctx context.Context, args ...string) *exec.Cmd {
	cfg := config.Get().System.Backups.Rustic

	// Try to use rustic from PATH first, fall back to configured path
	binaryPath := cfg.BinaryPath
	if systemPath, err := rustic.GetBinaryPath(); err == nil {
		binaryPath = systemPath
	} else {
		r.log().WithField("error", err).Debug("rustic not found in PATH, using configured path")
	}

	cmd := exec.CommandContext(ctx, binaryPath, args...)

	// Set repository (for local backups only - S3 uses config file)
	cmd.Env = os.Environ()
	if r.backupType != "s3" {
		cmd.Env = append(cmd.Env, fmt.Sprintf("RUSTIC_REPOSITORY=%s", r.repositoryPath))
	}

	// Set repository password (provided by panel)
	if r.repositoryPassword != "" {
		cmd.Env = append(cmd.Env, fmt.Sprintf("RUSTIC_PASSWORD=%s", r.repositoryPassword))
	}

	// Disable caching for security
	cmd.Env = append(cmd.Env, "RUSTIC_NO_CACHE=true")

	// Set S3 credentials if using S3
	if r.backupType == "s3" {
		r.log().Info("setting up S3 credentials for rustic command")
		if r.s3Credentials == nil {
			r.log().Error("S3 credentials are required for S3 backups")
			return cmd
		}
		if err := r.setS3EnvironmentSecure(cmd, r.s3Credentials, &cfg.S3); err != nil {
			r.log().WithError(err).Error("failed to set S3 credentials")
		} else {
			r.log().Info("S3 credentials configured successfully")
		}
	}

	return cmd
}

// setS3EnvironmentSecure creates a secure temporary config file for opendal S3 backend
func (r *RusticBackup) setS3EnvironmentSecure(cmd *exec.Cmd, creds *remote.S3Credentials, _ *config.S3RepositoryConfig) error {
	// Create secure temporary config file
	configFile, err := r.createSecureS3ConfigFile(creds)
	if err != nil {
		return errors.Wrap(err, "failed to create secure S3 config file")
	}
	r.credentialFile = configFile

	// Update command to use the specific config file via profile
	r.addConfigProfileToCommand(cmd, configFile)

	return nil
}

// addConfigProfileToCommand adds the config file to the rustic command by setting working directory
func (r *RusticBackup) addConfigProfileToCommand(cmd *exec.Cmd, configFile string) {
	// rustic automatically finds rustic.toml in the current working directory
	// Set the command's working directory to the config file directory
	configDir := filepath.Dir(configFile)
	cmd.Dir = configDir
}

// createSecureS3ConfigFile creates a secure temporary rustic config file for S3
func (r *RusticBackup) createSecureS3ConfigFile(creds *remote.S3Credentials) (string, error) {
	// Create secure temporary directory
	tempDir, err := r.createSecureTempDir()
	if err != nil {
		return "", err
	}

	// Create rustic.toml config file
	configFile := filepath.Join(tempDir, "rustic.toml")
	file, err := os.OpenFile(configFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, secureTempFileMode)
	if err != nil {
		os.RemoveAll(tempDir)
		return "", err
	}
	defer file.Close()

	// Build S3 configuration - use server UUID for clean, deduplicated paths
	repoRoot := r.serverUuid

	// Create TOML configuration content for opendal:s3 backend
	configContent := fmt.Sprintf(`[repository]
repository = "opendal:s3"
password = "%s"

[repository.options]
bucket = "%s"
root = "%s"
access_key_id = "%s"
secret_access_key = "%s"
region = "%s"
`, r.repositoryPassword, creds.Bucket, repoRoot, creds.AccessKeyID, creds.SecretAccessKey, creds.Region)

	// Add optional fields
	if creds.SessionToken != "" {
		configContent += fmt.Sprintf(`session_token = "%s"
`, creds.SessionToken)
	}

	if creds.Endpoint != "" {
		configContent += fmt.Sprintf(`endpoint = "%s"
`, creds.Endpoint)
	}

	if creds.ForcePathStyle {
		configContent += `enable_virtual_host_style = "false"
`
	}

	// Write config content
	if _, err := file.WriteString(configContent); err != nil {
		os.RemoveAll(tempDir)
		return "", err
	}

	return configFile, nil
}

// createSecureIgnoreFile creates a secure temporary ignore file for rustic
func (r *RusticBackup) createSecureIgnoreFile(ignore string) (string, error) {
	// Create secure temporary directory first
	tempDir, err := r.createSecureTempDir()
	if err != nil {
		return "", errors.Wrap(err, "failed to create secure temp directory")
	}

	// Create ignore file with secure permissions
	ignoreFile := filepath.Join(tempDir, "rustic-ignore")
	file, err := os.OpenFile(ignoreFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, secureTempFileMode)
	if err != nil {
		os.RemoveAll(tempDir)
		return "", errors.Wrap(err, "failed to create ignore file")
	}
	defer file.Close()

	if _, err := file.WriteString(ignore); err != nil {
		os.RemoveAll(tempDir)
		return "", errors.Wrap(err, "failed to write ignore file")
	}

	return ignoreFile, nil
}

// extractSnapshotID extracts the snapshot ID from rustic backup output using regex
func (r *RusticBackup) extractSnapshotID(output string) string {
	// Use compiled regex for reliable snapshot ID extraction
	// Note: rustic outputs only first 8 chars in success message, but we need the full ID
	matches := snapshotSavedRegex.FindStringSubmatch(output)
	if len(matches) >= 2 {
		shortID := matches[1]
		// Return the short ID (8 chars), which will be resolved to full ID by caller
		return shortID
	}
	return ""
}

// resolveFullSnapshotID resolves a short snapshot ID (8 chars) to the full snapshot ID (64 chars)
func (r *RusticBackup) resolveFullSnapshotID(ctx context.Context, shortID string) (string, error) {
	if len(shortID) == 64 {
		// Already a full ID
		return shortID, nil
	}

	if len(shortID) != 8 {
		return "", errors.New("rustic: invalid short snapshot ID length")
	}

	// Query snapshots to find the full ID
	cmd := r.buildRusticCommandWithContext(ctx, "snapshots", "--json", "--filter-tags", fmt.Sprintf("backup_uuid:%s", r.Uuid))
	output, err := cmd.Output()
	if err != nil {
		return "", errors.Wrap(err, "rustic: failed to query snapshots for ID resolution")
	}

	// Parse grouped snapshot results
	var results []struct {
		GroupKey  json.RawMessage      `json:"group_key"`
		Snapshots []RusticSnapshotInfo `json:"snapshots"`
	}
	if err := json.Unmarshal(output, &results); err != nil {
		return "", errors.Wrap(err, "rustic: failed to parse snapshots JSON for ID resolution")
	}

	// Find snapshot with matching short ID
	for _, result := range results {
		for _, snapshot := range result.Snapshots {
			if strings.HasPrefix(snapshot.ID, shortID) {
				return snapshot.ID, nil
			}
		}
	}

	return "", errors.Errorf("rustic: could not resolve short ID %s to full snapshot ID", shortID)
}

// Helper methods for validation and security

// isValidSnapshotID validates snapshot ID format
func (r *RusticBackup) isValidSnapshotID(id string) bool {
	return snapshotIDRegex.MatchString(id)
}

// validatePath validates filesystem paths for security
func (r *RusticBackup) validatePath(path string) error {
	if path == "" {
		return errors.New("path cannot be empty")
	}

	// Check for directory traversal attempts
	if strings.Contains(path, "..") {
		return errors.New("path contains directory traversal")
	}

	// Ensure path is absolute
	if !filepath.IsAbs(path) {
		return errors.New("path must be absolute")
	}

	// Check if path exists
	if _, err := os.Stat(path); err != nil {
		return errors.Wrap(err, "path does not exist or is not accessible")
	}

	return nil
}

// validateRepositoryPath validates repository paths
func (r *RusticBackup) validateRepositoryPath(repoPath string) error {
	if repoPath == "" {
		return errors.New("repository path cannot be empty")
	}

	// For local repositories, validate the path
	if r.backupType == "local" {
		cfg := config.Get().System.Backups.Rustic.Local

		// Clean and resolve both paths to handle symlinks and relative paths
		cleanRepo, err := filepath.Abs(filepath.Clean(repoPath))
		if err != nil {
			return errors.New("invalid repository path")
		}

		cleanBase, err := filepath.Abs(filepath.Clean(cfg.RepositoryPath))
		if err != nil {
			return errors.New("invalid base repository path")
		}

		// Check if cleanRepo is within cleanBase using relative path calculation
		relPath, err := filepath.Rel(cleanBase, cleanRepo)
		if err != nil || strings.HasPrefix(relPath, "..") || relPath == ".." {
			return errors.New("repository path outside allowed directory")
		}
	}

	return nil
}

// createSecureTempDir creates a temporary directory with secure permissions
func (r *RusticBackup) createSecureTempDir() (string, error) {
	tempDir, err := os.MkdirTemp("", tempDirPrefix)
	if err != nil {
		return "", err
	}

	// Set secure permissions
	if err := os.Chmod(tempDir, secureTempDirMode); err != nil {
		os.RemoveAll(tempDir)
		return "", err
	}

	return tempDir, nil
}

// DownloadTarGz generates a tar.gz archive from the rustic snapshot and writes it to the provided writer
func (r *RusticBackup) DownloadTarGz(ctx context.Context, writer io.Writer) error {
	if r.snapshotID == "" {
		return errors.New("rustic: no snapshot ID available for download")
	}

	// Validate snapshot ID
	if !r.isValidSnapshotID(r.snapshotID) {
		return errors.New("rustic: invalid snapshot ID format")
	}

	// Initialize repository path if not set
	if r.repositoryPath == "" {
		r.repositoryPath = r.getRepositoryPath()
	}

	defer r.cleanup()

	// Build rustic dump command to export snapshot as tar.gz
	dumpCtx, cancel := context.WithTimeout(ctx, rusticCommandTimeout)
	defer cancel()

	// Get the original backup path from snapshot metadata to dump just the server contents
	originalPath, err := r.getSnapshotOriginalPath(ctx)
	if err != nil {
		return errors.Wrap(err, "rustic: failed to get original backup path")
	}

	// Use the original backup path to dump just the server contents, not the full path structure
	cmd := r.buildRusticCommandWithContext(dumpCtx, "dump", "--archive", "targz", fmt.Sprintf("%s:%s", r.snapshotID, originalPath))

	// Set the command's stdout to write directly to our writer
	cmd.Stdout = writer
	cmd.Stderr = os.Stderr // For debugging errors

	r.log().WithField("snapshot_id", r.snapshotID).Info("generating tar.gz download from rustic snapshot")

	if err := cmd.Run(); err != nil {
		return errors.Wrapf(err, "rustic: failed to export snapshot %s as tar.gz", r.snapshotID)
	}

	r.log().WithField("snapshot_id", r.snapshotID).Info("successfully generated tar.gz download from rustic snapshot")
	return nil
}

// CanDownload returns true if this rustic backup can be downloaded as a tar.gz
func (r *RusticBackup) CanDownload() bool {
	return r.snapshotID != "" && r.isValidSnapshotID(r.snapshotID)
}

// getSnapshotOriginalPath retrieves the original backup path from snapshot metadata
func (r *RusticBackup) getSnapshotOriginalPath(ctx context.Context) (string, error) {
	if r.snapshotID == "" {
		return "", errors.New("rustic: no snapshot ID available")
	}

	// Build rustic snapshots command to get metadata
	snapshotsCtx, cancel := context.WithTimeout(ctx, rusticCommandTimeout)
	defer cancel()

	cmd := r.buildRusticCommandWithContext(snapshotsCtx, "snapshots", "--json", "--filter-tags", fmt.Sprintf("backup_uuid:%s", r.Uuid))

	output, err := cmd.Output()
	if err != nil {
		return "", errors.Wrapf(err, "rustic: failed to get snapshot metadata for backup UUID %s", r.Uuid)
	}

	// Parse snapshot metadata JSON
	var snapshots []struct {
		Snapshots []struct {
			Paths []string `json:"paths"`
			ID    string   `json:"id"`
		} `json:"snapshots"`
	}

	if err := json.Unmarshal(output, &snapshots); err != nil {
		return "", errors.Wrapf(err, "rustic: failed to parse snapshot metadata: %s", string(output))
	}

	// Debug: log if no snapshots found
	if len(snapshots) == 0 {
		return "", errors.Errorf("rustic: no snapshots found for backup UUID %s (snapshotID: %s)", r.Uuid, r.snapshotID)
	}

	// Find the snapshot with matching ID (support both full and shortened IDs)
	for _, group := range snapshots {
		for _, snapshot := range group.Snapshots {
			// Check for exact match first, then prefix match for shortened IDs
			if (snapshot.ID == r.snapshotID || strings.HasPrefix(snapshot.ID, r.snapshotID)) && len(snapshot.Paths) > 0 {
				return snapshot.Paths[0], nil
			}
		}
	}

	return "", errors.Errorf("rustic: snapshot ID %s not found in %d snapshot groups for backup UUID %s", r.snapshotID, len(snapshots), r.Uuid)
}

// snapshotExists checks if a snapshot exists in the repository
func (r *RusticBackup) snapshotExists(ctx context.Context, snapshotID string) (bool, error) {
	cmd := r.buildRusticCommandWithContext(ctx, "snapshots", "--json", snapshotID)
	output, err := cmd.Output()
	if err != nil {
		// If command fails, snapshot likely doesn't exist
		return false, nil
	}

	var snapshots []RusticSnapshotInfo
	if err := json.Unmarshal(output, &snapshots); err != nil {
		return false, nil
	}

	for _, snapshot := range snapshots {
		if snapshot.ID == snapshotID ||
			snapshot.Original == snapshotID ||
			(len(snapshotID) == 8 && strings.HasPrefix(snapshot.ID, snapshotID)) ||
			(len(snapshotID) == 8 && strings.HasPrefix(snapshot.Original, snapshotID)) {
			return true, nil
		}
	}

	return false, nil
}

// findSnapshotByBackupUuid finds a snapshot by backup UUID tag
func (r *RusticBackup) findSnapshotByBackupUuid(ctx context.Context, backupUuid string) (string, error) {
	cmd := r.buildRusticCommandWithContext(ctx, "snapshots", "--json", "--filter-tags", fmt.Sprintf("backup_uuid:%s", backupUuid))
	output, err := cmd.Output()
	if err != nil {
		return "", nil
	}

	// Filter results return grouped format: [{"group_key": {...}, "snapshots": [...]}]
	var results []struct {
		GroupKey  json.RawMessage      `json:"group_key"`
		Snapshots []RusticSnapshotInfo `json:"snapshots"`
	}
	if err := json.Unmarshal(output, &results); err != nil {
		return "", nil
	}

	for _, result := range results {
		for _, snapshot := range result.Snapshots {
			for _, tag := range snapshot.Tags {
				if tag == fmt.Sprintf("backup_uuid:%s", backupUuid) {
					if len(snapshot.ID) >= 8 {
						return snapshot.ID[:8], nil
					}
					return snapshot.ID, nil
				}
			}
		}
	}

	return "", nil
}

// cleanup removes temporary files and credentials
func (r *RusticBackup) cleanup() {
	if r.credentialFile != "" {
		// Remove the entire temporary directory containing the credential file
		if dir := filepath.Dir(r.credentialFile); strings.Contains(dir, tempDirPrefix) {
			os.RemoveAll(dir)
		}
		r.credentialFile = ""
	}
}
