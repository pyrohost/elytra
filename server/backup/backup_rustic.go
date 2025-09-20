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

	"github.com/pterodactyl/wings/config"
	"github.com/pterodactyl/wings/remote"
	"github.com/pterodactyl/wings/server/filesystem"
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
	secureTempFileMode = 0o600
	secureTempDirMode  = 0o700
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
	// Temporary credential file for secure S3 access
	credentialFile string
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
func LocateRustic(client remote.Client, uuid, backupType string, s3Creds *remote.S3Credentials, password string) (*RusticBackup, error) {
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

// LocateRusticLocal finds a rustic local backup by snapshot ID
func LocateRusticLocal(client remote.Client, uuid, password string) (*RusticBackup, error) {
	return LocateRustic(client, uuid, "local", nil, password)
}

// LocateRusticS3 finds a rustic S3 backup by snapshot ID
func LocateRusticS3(client remote.Client, uuid string, s3Creds *remote.S3Credentials, password string) (*RusticBackup, error) {
	return LocateRustic(client, uuid, "s3", s3Creds, password)
}

// NewRustic creates a new rustic backup instance
func NewRustic(client remote.Client, uuid, ignore, backupType string, s3Creds *remote.S3Credentials, password string) *RusticBackup {
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

	cmd := r.buildRusticCommandWithContext(ctx, "forget", r.snapshotID)
	if err := cmd.Run(); err != nil {
		return errors.Wrap(err, "rustic: failed to remove snapshot")
	}

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

	cmd := r.buildRusticCommandWithContext(backupCtx, "backup", fsys.Path())
	cmd.Env = append(cmd.Env, fmt.Sprintf("RUSTIC_IGNORE_FILE=%s", ignoreFile))

	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, errors.Wrapf(err, "rustic: backup failed: %s", string(output))
	}

	// Extract and validate snapshot ID from output
	r.snapshotID = r.extractSnapshotID(string(output))
	if r.snapshotID == "" || !r.isValidSnapshotID(r.snapshotID) {
		return nil, errors.New("rustic: failed to extract valid snapshot ID from backup output")
	}

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

	// Restore snapshot to temporary directory with timeout
	restoreCtx, cancel := context.WithTimeout(ctx, rusticCommandTimeout)
	defer cancel()

	cmd := r.buildRusticCommandWithContext(restoreCtx, "restore", fmt.Sprintf("%s:", r.snapshotID), tempDir)
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

	// Parse rustic's nested JSON structure: [ [GroupMetadata, [Snapshot, ...]], ... ]
	var groups []json.RawMessage
	if err := json.Unmarshal(output, &groups); err != nil {
		return 0, errors.Wrap(err, "rustic: failed to parse top-level JSON array")
	}

	var size int64
	for _, groupRaw := range groups {
		var groupArray [2]json.RawMessage
		if err := json.Unmarshal(groupRaw, &groupArray); err != nil {
			continue // Skip malformed groups
		}

		// Parse the snapshots array (second element)
		var snapshots []RusticSnapshotInfo
		if err := json.Unmarshal(groupArray[1], &snapshots); err != nil {
			continue // Skip malformed snapshot arrays
		}

		// Find our specific snapshot (match by short or full ID)
		for _, snapshot := range snapshots {
			// Check if snapshot ID matches (full match or starts with our short ID)
			if snapshot.ID == r.snapshotID ||
				snapshot.Original == r.snapshotID ||
				(len(r.snapshotID) == 8 && strings.HasPrefix(snapshot.ID, r.snapshotID)) ||
				(len(r.snapshotID) == 8 && strings.HasPrefix(snapshot.Original, r.snapshotID)) {
				if snapshot.Summary != nil {
					// Use data_added as the backup size (compressed size)
					size = snapshot.Summary.DataAdded
				}
				break
			}
		}

		if size > 0 {
			break
		}
	}

	if size == 0 {
		return 0, errors.New("rustic: snapshot not found or size unavailable")
	}

	return size, nil
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
		if err := os.MkdirAll(filepath.Dir(r.repositoryPath), secureTempDirMode); err != nil {
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

	if err := cmd.Run(); err != nil {
		return errors.Wrap(err, "rustic: failed to initialize repository")
	}

	r.log().WithField("repository", r.repositoryPath).Info("rustic repository initialized")
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
	cfg := config.Get().System.Backups.Rustic

	switch r.backupType {
	case "local":
		return filepath.Join(cfg.Local.RepositoryPath, r.Uuid)
	case "s3":
		if r.s3Credentials != nil {
			if r.s3Credentials.Endpoint != "" {
				// Custom S3 endpoint
				return fmt.Sprintf("s3:%s/%s/%s%s", r.s3Credentials.Endpoint, r.s3Credentials.Bucket, r.s3Credentials.KeyPrefix, r.Uuid)
			}
			// AWS S3
			return fmt.Sprintf("s3:s3.%s.amazonaws.com/%s/%s%s", r.s3Credentials.Region, r.s3Credentials.Bucket, r.s3Credentials.KeyPrefix, r.Uuid)
		}
		if cfg.S3.Endpoint != "" {
			// Custom S3 endpoint from config
			return fmt.Sprintf("s3:%s/%s/%s%s", cfg.S3.Endpoint, cfg.S3.Bucket, cfg.S3.KeyPrefix, r.Uuid)
		}
		// AWS S3 from config
		return fmt.Sprintf("s3:s3.%s.amazonaws.com/%s/%s%s", cfg.S3.Region, cfg.S3.Bucket, cfg.S3.KeyPrefix, r.Uuid)
	default:
		return filepath.Join(cfg.Local.RepositoryPath, r.Uuid)
	}
}

// buildRusticCommandWithContext builds a rustic command with context and timeout
func (r *RusticBackup) buildRusticCommandWithContext(ctx context.Context, args ...string) *exec.Cmd {
	cfg := config.Get().System.Backups.Rustic
	cmd := exec.CommandContext(ctx, cfg.BinaryPath, args...)

	// Set repository
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("RUSTIC_REPOSITORY=%s", r.repositoryPath),
	)

	// Set repository password (provided by panel)
	if r.repositoryPassword != "" {
		cmd.Env = append(cmd.Env, fmt.Sprintf("RUSTIC_PASSWORD=%s", r.repositoryPassword))
	}

	// Disable caching for security
	cmd.Env = append(cmd.Env, "RUSTIC_NO_CACHE=true")

	// Set S3 credentials if using S3
	if r.backupType == "s3" && r.s3Credentials != nil {
		if err := r.setS3EnvironmentSecure(cmd, r.s3Credentials, &cfg.S3); err != nil {
			r.log().WithError(err).Error("rustic: failed to set S3 credentials")
		}
	} else if r.backupType == "s3" {
		// Fall back to config values
		r.setS3EnvironmentFromConfig(cmd, &cfg.S3)
	}

	return cmd
}

// setS3EnvironmentSecure sets S3 environment variables securely using a credential file
func (r *RusticBackup) setS3EnvironmentSecure(cmd *exec.Cmd, creds *remote.S3Credentials, _ *config.S3RepositoryConfig) error {
	// Create secure credential file instead of using environment variables
	credFile, err := r.createCredentialFile(creds)
	if err != nil {
		return errors.Wrap(err, "failed to create credential file")
	}
	r.credentialFile = credFile

	// Set AWS shared credentials file
	cmd.Env = append(cmd.Env,
		fmt.Sprintf("AWS_SHARED_CREDENTIALS_FILE=%s", credFile),
		fmt.Sprintf("AWS_DEFAULT_REGION=%s", creds.Region),
	)

	// Set custom endpoint if provided
	if creds.Endpoint != "" {
		cmd.Env = append(cmd.Env, fmt.Sprintf("AWS_ENDPOINT_URL=%s", creds.Endpoint))
	}

	// S3 configuration options
	if creds.ForcePathStyle {
		cmd.Env = append(cmd.Env, "AWS_S3_FORCE_PATH_STYLE=true")
	}
	if creds.DisableSSL {
		cmd.Env = append(cmd.Env, "AWS_S3_DISABLE_SSL=true")
	}
	if creds.CACertPath != "" {
		cmd.Env = append(cmd.Env, fmt.Sprintf("AWS_CA_BUNDLE=%s", creds.CACertPath))
	}

	return nil
}

// setS3EnvironmentFromConfig sets S3 environment variables from config
func (r *RusticBackup) setS3EnvironmentFromConfig(cmd *exec.Cmd, s3Cfg *config.S3RepositoryConfig) {
	cmd.Env = append(cmd.Env,
		fmt.Sprintf("AWS_DEFAULT_REGION=%s", s3Cfg.Region),
	)

	if s3Cfg.Endpoint != "" {
		cmd.Env = append(cmd.Env, fmt.Sprintf("AWS_ENDPOINT_URL=%s", s3Cfg.Endpoint))
	}

	// S3 configuration options from config
	if s3Cfg.ForcePathStyle {
		cmd.Env = append(cmd.Env, "AWS_S3_FORCE_PATH_STYLE=true")
	}
	if s3Cfg.DisableSSL {
		cmd.Env = append(cmd.Env, "AWS_S3_DISABLE_SSL=true")
	}
	if s3Cfg.CACertPath != "" {
		cmd.Env = append(cmd.Env, fmt.Sprintf("AWS_CA_BUNDLE=%s", s3Cfg.CACertPath))
	}
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
		// We only have the short ID (8 chars), we'll need to find the full ID via snapshots
		// For now, store the short ID and resolve to full ID when needed
		return shortID
	}
	return ""
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
		// Check for directory traversal
		if strings.Contains(repoPath, "..") {
			return errors.New("repository path contains directory traversal")
		}

		// Ensure it's within allowed directory structure
		cfg := config.Get().System.Backups.Rustic.Local
		if !strings.HasPrefix(repoPath, cfg.RepositoryPath) {
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

// createCredentialFile creates a secure temporary credential file for S3
func (r *RusticBackup) createCredentialFile(creds *remote.S3Credentials) (string, error) {
	// Create secure temporary directory
	tempDir, err := r.createSecureTempDir()
	if err != nil {
		return "", err
	}

	// Create credentials file
	credFile := filepath.Join(tempDir, "credentials")
	file, err := os.OpenFile(credFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, secureTempFileMode)
	if err != nil {
		os.RemoveAll(tempDir)
		return "", err
	}
	defer file.Close()

	// Write AWS credentials format
	credContent := fmt.Sprintf("[default]\naws_access_key_id = %s\naws_secret_access_key = %s\n",
		creds.AccessKeyID, creds.SecretAccessKey)

	if creds.SessionToken != "" {
		credContent += fmt.Sprintf("aws_session_token = %s\n", creds.SessionToken)
	}

	if _, err := file.WriteString(credContent); err != nil {
		os.RemoveAll(tempDir)
		return "", err
	}

	return credFile, nil
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

	// Use the snapshot ID with ":/" to dump the entire snapshot root as tar.gz
	cmd := r.buildRusticCommandWithContext(dumpCtx, "dump", "--archive", "targz", fmt.Sprintf("%s:/", r.snapshotID))

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

// snapshotExists checks if a snapshot exists in the repository
func (r *RusticBackup) snapshotExists(ctx context.Context, snapshotID string) (bool, error) {
	cmd := r.buildRusticCommandWithContext(ctx, "snapshots", "--json", snapshotID)
	output, err := cmd.Output()
	if err != nil {
		// If command fails, snapshot likely doesn't exist
		return false, nil
	}

	// Parse the output to check if snapshot was found
	var groups []json.RawMessage
	if err := json.Unmarshal(output, &groups); err != nil {
		return false, errors.Wrap(err, "rustic: failed to parse snapshots JSON")
	}

	// Check if any snapshots were returned
	for _, groupRaw := range groups {
		var groupArray [2]json.RawMessage
		if err := json.Unmarshal(groupRaw, &groupArray); err != nil {
			continue
		}

		var snapshots []RusticSnapshotInfo
		if err := json.Unmarshal(groupArray[1], &snapshots); err != nil {
			continue
		}

		// Look for our snapshot
		for _, snapshot := range snapshots {
			if snapshot.ID == snapshotID ||
				snapshot.Original == snapshotID ||
				(len(snapshotID) == 8 && strings.HasPrefix(snapshot.ID, snapshotID)) ||
				(len(snapshotID) == 8 && strings.HasPrefix(snapshot.Original, snapshotID)) {
				return true, nil
			}
		}
	}

	return false, nil
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
