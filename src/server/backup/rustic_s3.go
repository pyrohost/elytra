// note: a lot of this logic is duplicated and flakey, but it works! I need to
// completely rewrite this from scratch now that I know all the intricies of rustic
// but oh my god it was a fucking journey to get here - ellie

package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"emperror.dev/errors"
	"github.com/apex/log"
)

// S3Repository implements Repository for S3 storage
type S3Repository struct {
	config     Config
	s3Config   *S3Config
	tempDir    string
	configFile string
	logger     *log.Entry
	mu         sync.RWMutex
}

// NewS3Repository creates a new S3 repository instance
func NewS3Repository(cfg Config) (*S3Repository, error) {
	if cfg.S3Config == nil {
		return nil, errors.New("S3 configuration is required")
	}

	if cfg.ServerUUID == "" {
		return nil, errors.New("server UUID is required for S3 repositories")
	}

	repo := &S3Repository{
		config:   cfg,
		s3Config: cfg.S3Config,
		logger:   log.WithField("component", "rustic_s3"),
	}

	// Create config file immediately
	if err := repo.createConfigFile(); err != nil {
		return nil, errors.Wrap(err, "failed to create S3 config file")
	}

	return repo, nil
}

// Initialize creates the S3 repository if it doesn't exist.
// MAC/decryption errors are surfaced rather than treated as corruption,
// because the dominant cause is a wrong password and the previous
// recovery path silently destroyed legitimate backup history.
func (r *S3Repository) Initialize(ctx context.Context) error {
	exists, err := r.Exists(ctx)
	if err != nil {
		if isDecryptionError(err.Error()) {
			return wrapDecryptionError(err, "repository check", r.config.ServerUUID)
		}
		return errors.Wrap(err, "failed to check repository existence")
	}

	if exists {
		r.logger.WithField("bucket", r.s3Config.Bucket).Debug("repository already exists")
		return nil
	}

	return r.initializeFreshRepository(ctx)
}

// initializeFreshRepository creates a new repository (called after backup is complete)
func (r *S3Repository) initializeFreshRepository(ctx context.Context) error {
	r.logger.WithField("server_uuid", r.config.ServerUUID).Info("initializing fresh repository after backup")

	// Initialize fresh repository
	cmd, err := r.buildCommand(ctx, "init")
	if err != nil {
		return errors.Wrap(err, "failed to build init command")
	}

	output, err := cmd.CombinedOutput()
	if err != nil {
		// If init still fails due to existing config, the repository deletion didn't work completely
		if strings.Contains(string(output), "Config file already exists") {
			r.logger.WithFields(log.Fields{
				"server_uuid": r.config.ServerUUID,
				"error":       string(output),
			}).Error("repository config still exists after S3 deletion - S3 cleanup may have failed")
			return errors.New("repository in corrupted state, S3 deletion incomplete")
		}
		return errors.New("S3 repository initialization failed")
	}

	r.logger.WithFields(log.Fields{
		"bucket": r.s3Config.Bucket,
		"root":   fmt.Sprintf("rustic-repos/%s", r.config.ServerUUID),
	}).Info("S3 repository initialized successfully")

	return nil
}

// Exists checks if the S3 repository exists and is accessible
func (r *S3Repository) Exists(ctx context.Context) (bool, error) {
	cmd, err := r.buildCommand(ctx, "snapshots", "--json")
	if err != nil {
		return false, err
	}

	output, err := cmd.CombinedOutput()
	if err != nil {
		errorOutput := string(output)

		// Repository definitely doesn't exist
		if strings.Contains(errorOutput, "No repository config file found") {
			return false, nil
		}

		// For MAC check failures, the repository exists but we can't access it
		// This indicates a compatibility issue with existing repositories
		if strings.Contains(errorOutput, "MAC check failed") ||
			strings.Contains(errorOutput, "Data decryption failed") {
			return false, errors.New("repository exists but is incompatible with current rustic version - MAC check failed")
		}

		// Wrong password means repository exists
		if strings.Contains(errorOutput, "password") ||
			strings.Contains(errorOutput, "No suitable key found") {
			return true, errors.Wrapf(err, "repository exists but password incorrect: %s", errorOutput)
		}

		// Other errors - assume repository exists but has access issues
		return true, errors.Wrapf(err, "repository exists but access failed: %s", errorOutput)
	}

	// Command succeeded - repository exists and is accessible
	return true, nil
}

// Info returns S3 repository information
func (r *S3Repository) Info(ctx context.Context) (*RepositoryInfo, error) {
	cmd, err := r.buildCommand(ctx, "repoinfo", "--json")
	if err != nil {
		return nil, err
	}

	output, err := cmd.Output()
	if err != nil {
		return nil, errors.Wrap(err, "failed to get repository info")
	}

	var info struct {
		Index struct {
			Blobs []struct {
				BlobType string `json:"blob_type"`
				Count    int    `json:"count"`
				Size     int64  `json:"size"`
				DataSize int64  `json:"data_size"`
			} `json:"blobs"`
		} `json:"index"`
	}

	if err := json.Unmarshal(output, &info); err != nil {
		return nil, errors.Wrap(err, "failed to parse repository info")
	}

	var totalSize int64
	for _, blob := range info.Index.Blobs {
		totalSize += blob.DataSize
	}

	return &RepositoryInfo{
		TotalSize:     totalSize,
		SnapshotCount: len(info.Index.Blobs),
		LastUpdate:    time.Now(),
	}, nil
}

// CreateSnapshot creates a new backup snapshot
func (r *S3Repository) CreateSnapshot(ctx context.Context, path string, tags map[string]string, ignoreFile string) (*Snapshot, error) {
	// Build backup command
	args := []string{"backup"}

	// Add tags
	for _, tag := range formatTags(tags) {
		args = append(args, "--tag", tag)
	}

	// Add path
	args = append(args, path)

	cmd, err := r.buildCommand(ctx, args...)
	if err != nil {
		return nil, errors.Wrap(err, "failed to build backup command")
	}

	// Set ignore file if provided
	if ignoreFile != "" {
		cmd.Env = append(cmd.Env, fmt.Sprintf("RUSTIC_IGNORE_FILE=%s", ignoreFile))
	}

	output, err := cmd.CombinedOutput()
	if err != nil {
		if isDecryptionError(string(output)) {
			return nil, wrapDecryptionError(err, "backup", r.config.ServerUUID)
		}
		return nil, errors.New("S3 backup failed")
	}

	// Extract snapshot ID from output
	snapshotID := extractSnapshotID(string(output))
	if snapshotID == "" {
		return nil, errors.New("failed to extract snapshot ID from output")
	}

	// Get detailed snapshot info
	return r.GetSnapshot(ctx, snapshotID)
}

// GetSnapshot retrieves a snapshot by ID
func (r *S3Repository) GetSnapshot(ctx context.Context, id string) (*Snapshot, error) {
	if !isValidSnapshotID(id) {
		return nil, errors.New("invalid snapshot ID format")
	}

	cmd, err := r.buildCommand(ctx, "snapshots", "--json", id)
	if err != nil {
		return nil, err
	}

	output, err := cmd.Output()
	if err != nil {
		return nil, errors.Wrap(err, "failed to get snapshot")
	}

	var snapshots []SnapshotInfo
	if err := json.Unmarshal(output, &snapshots); err != nil {
		return nil, errors.Wrap(err, "failed to parse snapshot info")
	}

	if len(snapshots) == 0 {
		return nil, errors.New("snapshot not found")
	}

	info := snapshots[0]
	size := int64(1024) // Default minimum size
	if info.Summary != nil && info.Summary.DataAdded > 0 {
		size = info.Summary.DataAdded
	}

	// Parse backup UUID from tags (for admin purposes)
	tags := parseTags(info.Tags)
	backupUUID := tags["backup_uuid"]

	return &Snapshot{
		ID:         info.ID,
		BackupUUID: backupUUID,
		Size:       size,
		CreatedAt:  info.Time,
		Tags:       tags,
		Paths:      info.Paths,
	}, nil
}

// ListSnapshots lists all snapshots matching the filter
func (r *S3Repository) ListSnapshots(ctx context.Context, filter map[string]string) ([]*Snapshot, error) {
	args := []string{"snapshots", "--json"}

	// Add tag filters
	for k, v := range filter {
		if v == "" {
			args = append(args, "--filter-tags", k)
		} else {
			args = append(args, "--filter-tags", fmt.Sprintf("%s:%s", k, v))
		}
	}

	cmd, err := r.buildCommand(ctx, args...)
	if err != nil {
		return nil, err
	}

	output, err := cmd.Output()
	if err != nil {
		return nil, errors.Wrap(err, "failed to list snapshots")
	}

	// Handle grouped results
	var results []struct {
		GroupKey  json.RawMessage `json:"group_key"`
		Snapshots []SnapshotInfo  `json:"snapshots"`
	}

	if err := json.Unmarshal(output, &results); err != nil {
		return nil, errors.Wrap(err, "failed to parse snapshot list")
	}

	var snapshots []*Snapshot
	for _, result := range results {
		for _, info := range result.Snapshots {
			size := int64(1024)
			if info.Summary != nil && info.Summary.DataAdded > 0 {
				size = info.Summary.DataAdded
			}

			tags := parseTags(info.Tags)
			backupUUID := tags["backup_uuid"]

			snapshots = append(snapshots, &Snapshot{
				ID:         info.ID,
				BackupUUID: backupUUID,
				Size:       size,
				CreatedAt:  info.Time,
				Tags:       tags,
				Paths:      info.Paths,
			})
		}
	}

	return snapshots, nil
}

// DeleteSnapshot deletes a snapshot by ID with retry mechanism for pruning
func (r *S3Repository) DeleteSnapshot(ctx context.Context, id string) error {
	if !isValidSnapshotID(id) {
		return errors.New("invalid snapshot ID format")
	}

	// Try deletion with pruning up to 3 times with 3-minute timeouts
	// Pruning requires scanning repository and deleting unreferenced S3 blobs which can take time
	maxRetries := 3
	timeout := 3 * time.Minute

	for attempt := 1; attempt <= maxRetries; attempt++ {
		r.logger.WithFields(log.Fields{
			"snapshot_id": id,
			"attempt":     attempt,
			"max_retries": maxRetries,
			"timeout":     timeout,
		}).Info("attempting S3 snapshot deletion with pruning")

		err := r.deleteSnapshotWithTimeout(ctx, id, timeout)
		if err == nil {
			r.logger.WithField("snapshot_id", id).Info("S3 snapshot deleted successfully")
			return nil
		}

		// Check if it's a timeout error
		if strings.Contains(err.Error(), "timed out") && attempt < maxRetries {
			r.logger.WithFields(log.Fields{
				"snapshot_id": id,
				"attempt":     attempt,
				"error":       err.Error(),
			}).Warn("deletion attempt timed out, retrying")
			continue
		}

		// For non-timeout errors or final attempt, return the error
		r.logger.WithFields(log.Fields{
			"snapshot_id": id,
			"attempt":     attempt,
			"error":       err.Error(),
		}).Error("deletion attempt failed")

		if attempt == maxRetries {
			return errors.Wrapf(err, "failed to delete snapshot after %d attempts", maxRetries)
		}
	}

	return errors.New("deletion failed after all retry attempts")
}

// deleteSnapshotWithTimeout performs a single deletion attempt with the specified timeout
func (r *S3Repository) deleteSnapshotWithTimeout(ctx context.Context, id string, timeout time.Duration) error {
	// Create a timeout context for this specific attempt
	deleteCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Use forget with --prune and --keep-delete to safely reclaim storage space
	// The 1-day delay ensures any parallel backups have time to complete and reference shared blobs
	cmd, err := r.buildCommand(deleteCtx, "forget", "--prune", "--keep-delete", "1d", id)
	if err != nil {
		return errors.Wrap(err, "failed to build delete command")
	}

	// Capture output properly by setting up pipes
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	// Start the command and monitor for timeout
	if err := cmd.Start(); err != nil {
		return errors.Wrap(err, "failed to start delete command")
	}

	// Wait for completion or timeout in a goroutine
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	// Wait for either completion or timeout
	select {
	case err := <-done:
		output := stdout.String() + stderr.String()
		if err != nil {
			// Check if snapshot was already deleted (404 with delete marker)
			if strings.Contains(output, "NotFound") && strings.Contains(output, "x-amz-delete-marker") {
				r.logger.WithField("snapshot_id", id).Info("snapshot already deleted, treating as success")
				return nil
			}
			// Never expose raw rustic errors - they may contain credentials
			r.logger.WithField("snapshot_id", id).WithError(err).Error("snapshot deletion failed")
			return errors.New("failed to delete snapshot")
		}
		return nil
	case <-deleteCtx.Done():
		// Kill the process if it's still running
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
		// Never expose raw rustic errors - they may contain credentials
		r.logger.Error("snapshot deletion timed out")
		return errors.Errorf("snapshot deletion timed out after %v", timeout)
	}
}

// GetRepositorySize returns the total size of the repository in bytes
func (r *S3Repository) GetRepositorySize(ctx context.Context) (int64, error) {
	// Create a timeout context for the size query
	sizeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cmd, err := r.buildCommand(sizeCtx, "repoinfo", "--json")
	if err != nil {
		return 0, errors.Wrap(err, "failed to build size query command")
	}

	// Use Output() instead of CombinedOutput() to get only stdout (pure JSON)
	// This avoids log messages from stderr mixing into the JSON output
	output, err := cmd.Output()
	if err != nil {
		// Check for context timeout
		if sizeCtx.Err() == context.DeadlineExceeded {
			return 0, errors.New("repository size query timed out after 30 seconds")
		}
		return 0, errors.New("failed to get repository size")
	}

	// Parse JSON output to calculate repository size from all repository files
	var repoInfo struct {
		Files struct {
			Repo []struct {
				Type  string `json:"tpe"`
				Count int    `json:"count"`
				Size  int64  `json:"size"`
			} `json:"repo"`
		} `json:"files"`
	}

	if err := json.Unmarshal(output, &repoInfo); err != nil {
		return 0, errors.New("failed to parse repository info")
	}

	// Calculate total size from all repository files (actual disk usage)
	var totalSize int64
	for _, file := range repoInfo.Files.Repo {
		totalSize += file.Size
	}

	r.logger.WithField("total_size_bytes", totalSize).Info("S3 repository size retrieved")
	return totalSize, nil
}

// GetSnapshotSizes returns the actual size of each snapshot in the repository
func (r *S3Repository) GetSnapshotSizes(ctx context.Context) (map[string]int64, error) {
	// Create a timeout context for the snapshot size query
	sizeCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	cmd, err := r.buildCommand(sizeCtx, "snapshots", "--json")
	if err != nil {
		return nil, errors.Wrap(err, "failed to build snapshot list command")
	}

	output, err := cmd.CombinedOutput()
	if err != nil {
		// Check for context timeout
		if sizeCtx.Err() == context.DeadlineExceeded {
			return nil, errors.New("snapshot list query timed out after 60 seconds")
		}
		return nil, errors.New("failed to get snapshot list")
	}

	// Clean the output - remove any INFO messages before JSON
	outputStr := string(output)
	// Remove lines that start with [INFO], [WARN], [ERROR], etc.
	lines := strings.Split(outputStr, "\n")
	var jsonLines []string
	foundJSON := false

	for _, line := range lines {
		line = strings.TrimSpace(line)
		// Skip empty lines and log messages
		if line == "" || strings.HasPrefix(line, "[INFO]") || strings.HasPrefix(line, "[WARN]") || strings.HasPrefix(line, "[ERROR]") {
			continue
		}
		// Look for JSON content (starts with [ or {)
		if !foundJSON && (strings.HasPrefix(line, "[") || strings.HasPrefix(line, "{")) {
			foundJSON = true
		}
		if foundJSON {
			jsonLines = append(jsonLines, line)
		}
	}

	cleanedOutput := []byte(strings.Join(jsonLines, "\n"))

	// Handle grouped results (same format as ListSnapshots)
	var results []struct {
		GroupKey  json.RawMessage `json:"group_key"`
		Snapshots []SnapshotInfo  `json:"snapshots"`
	}

	if err := json.Unmarshal(cleanedOutput, &results); err != nil {
		return nil, errors.Wrapf(err, "failed to parse snapshot list: %s", string(cleanedOutput))
	}

	// Convert to map of snapshot ID -> size
	snapshotSizes := make(map[string]int64)
	for _, result := range results {
		for _, info := range result.Snapshots {
			// Use same size calculation logic as ListSnapshots
			size := int64(1024) // Default minimum size
			if info.Summary != nil && info.Summary.DataAdded > 0 {
				size = info.Summary.DataAdded
			}
			snapshotSizes[info.ID] = size
		}
	}

	r.logger.WithField("snapshot_count", len(snapshotSizes)).Info("S3 snapshot sizes retrieved")
	return snapshotSizes, nil
}

// RestoreSnapshot restores a snapshot to the target path
func (r *S3Repository) RestoreSnapshot(ctx context.Context, snapshotID string, targetPath string, sourcePath string) error {
	if !isValidSnapshotID(snapshotID) {
		return errors.New("invalid snapshot ID format")
	}

	// Build restore target (snapshot:path format)
	restoreTarget := fmt.Sprintf("%s:%s", snapshotID, sourcePath)

	cmd, err := r.buildCommand(ctx, "restore", restoreTarget, targetPath)
	if err != nil {
		return errors.Wrap(err, "failed to build restore command")
	}

	output, err := cmd.CombinedOutput()
	if err != nil {
		outputStr := string(output)
		r.logger.WithFields(log.Fields{
			"snapshot_id": snapshotID,
			"output":      outputStr,
		}).Error("rustic restore command failed")
		return errors.Wrapf(err, "S3 restore failed: %s", outputStr)
	}

	return nil
}

// Destroy completely removes all repository data from S3
func (r *S3Repository) Destroy(ctx context.Context) error {
	r.logger.Info("destroying S3 repository")

	// Create a timeout context for the destroy operation
	destroyCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	// Use rustic to remove all snapshots first, then delete the repository config
	// This ensures proper cleanup of all S3 objects
	// Use --keep-none to delete all snapshots, --instant-delete to immediately reclaim storage
	cmd, err := r.buildCommand(destroyCtx, "forget", "--keep-none", "--prune", "--instant-delete")
	if err != nil {
		return errors.Wrap(err, "failed to build destroy command")
	}

	output, err := cmd.CombinedOutput()
	outputStr := string(output)

	if err != nil {
		// If repository doesn't exist, that's fine
		if strings.Contains(outputStr, "no repository") ||
			strings.Contains(outputStr, "not found") ||
			strings.Contains(outputStr, "No repository config file found") {
			r.logger.Info("S3 repository does not exist, nothing to destroy")
			return nil
		}

		// Check if it timed out
		if destroyCtx.Err() == context.DeadlineExceeded {
			r.logger.WithField("timeout", "10 minutes").Error("repository destruction timed out")
			return errors.New("repository destruction timed out after 10 minutes - repository may be very large or S3 is slow")
		}

		// Log the actual error for debugging (but don't expose credentials in return value)
		r.logger.WithFields(log.Fields{
			"error":  err.Error(),
			"output": outputStr,
		}).Error("rustic destroy command failed")

		return errors.Errorf("failed to destroy S3 repository: %s", err.Error())
	}

	r.logger.WithField("output", outputStr).Info("S3 repository destroyed successfully")
	return nil
}

// Close cleans up temporary files and directories
func (r *S3Repository) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.tempDir != "" {
		if err := os.RemoveAll(r.tempDir); err != nil {
			r.logger.WithError(err).Error("failed to remove S3 temp directory")
			return err
		}
		r.tempDir = ""
		r.configFile = ""
	}

	return nil
}

// createConfigFile creates the rustic configuration file for S3
func (r *S3Repository) createConfigFile() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Only create once
	if r.tempDir != "" {
		return nil
	}

	// Create secure temp directory
	tempDir, err := os.MkdirTemp("", TempPrefix+"s3-")
	if err != nil {
		return errors.Wrap(err, "failed to create temp directory")
	}

	if err := os.Chmod(tempDir, SecureDirMode); err != nil {
		os.RemoveAll(tempDir)
		return errors.Wrap(err, "failed to set temp directory permissions")
	}

	// Create config file using shared helper
	configFile := filepath.Join(tempDir, "elytra.toml")
	repoRoot := fmt.Sprintf("rustic-repos/%s", r.config.ServerUUID)
	if err := r.createConfigFileAtPath(configFile, repoRoot); err != nil {
		os.RemoveAll(tempDir)
		return errors.Wrap(err, "failed to create config file")
	}

	r.tempDir = tempDir
	r.configFile = configFile
	return nil
}

// createConfigFileAtPath creates a rustic config file at the specified path with given repository root
func (r *S3Repository) createConfigFileAtPath(configPath, repoRoot string) error {
	// Validate inputs
	if configPath == "" {
		return errors.New("config path cannot be empty")
	}
	if repoRoot == "" {
		return errors.New("repository root cannot be empty")
	}
	if r.s3Config == nil {
		return errors.New("S3 configuration is missing")
	}

	file, err := os.OpenFile(configPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, SecureFileMode)
	if err != nil {
		return errors.Wrap(err, "failed to create config file")
	}
	defer file.Close()

	// Generate config content with proper escaping
	configContent := fmt.Sprintf(`[repository]
repository = "opendal:s3"
password = "%s"

[repository.options]
bucket = "%s"
root = "%s"
access_key_id = "%s"
secret_access_key = "%s"
region = "%s"
`,
		escapeTomlString(r.config.Password),
		escapeTomlString(r.s3Config.Bucket),
		escapeTomlString(repoRoot),
		escapeTomlString(r.s3Config.AccessKeyID),
		escapeTomlString(r.s3Config.SecretAccessKey),
		escapeTomlString(r.s3Config.Region))

	// Add optional fields with validation
	if r.s3Config.SessionToken != "" {
		configContent += fmt.Sprintf(`session_token = "%s"
`, escapeTomlString(r.s3Config.SessionToken))
	}

	if r.s3Config.Endpoint != "" {
		configContent += fmt.Sprintf(`endpoint = "%s"
`, escapeTomlString(r.s3Config.Endpoint))
	}

	if r.s3Config.ForcePathStyle {
		configContent += `enable_virtual_host_style = "false"
`
	}

	// Write config content
	if _, err := file.WriteString(configContent); err != nil {
		return errors.Wrap(err, "failed to write config file")
	}

	return nil
}

// escapeTomlString escapes special characters in TOML strings
func escapeTomlString(s string) string {
	// Replace backslashes and quotes to prevent TOML injection
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "\"", "\\\"")
	s = strings.ReplaceAll(s, "\n", "\\n")
	s = strings.ReplaceAll(s, "\r", "\\r")
	s = strings.ReplaceAll(s, "\t", "\\t")
	return s
}

// buildCommand builds a rustic command for S3 repository
func (r *S3Repository) buildCommand(ctx context.Context, args ...string) (*exec.Cmd, error) {
	binaryPath, err := getRusticBinary()
	if err != nil {
		return nil, err
	}

	// Get the temp directory containing the config file
	r.mu.RLock()
	tempDir := r.tempDir
	configFile := r.configFile
	r.mu.RUnlock()

	if tempDir == "" || configFile == "" {
		return nil, errors.New("config file not initialized")
	}

	// Create command with profile (looks for elytra.toml in current directory)
	profileArgs := append([]string{"--use-profile", "elytra"}, args...)
	cmd := exec.CommandContext(ctx, binaryPath, profileArgs...)

	// Set environment
	cmd.Env = os.Environ()
	// Note: Password is set in config file, not environment variable
	// to avoid conflicts with config file password

	// Set cache directory
	if cacheDir := getCacheDir(); cacheDir != "" {
		cmd.Env = append(cmd.Env, fmt.Sprintf("RUSTIC_CACHE_DIR=%s", cacheDir))
	}

	// IMPORTANT: Set working directory to temp directory containing config file
	// This allows rustic to find ./elytra.toml in the current directory
	cmd.Dir = tempDir

	return cmd, nil
}

// isDecryptionError reports whether the given rustic CLI output indicates a
// MAC or decryption failure. The typical causes are a wrong repository
// password or a rustic binary version mismatch with the repository on disk.
func isDecryptionError(output string) bool {
	return strings.Contains(output, "MAC check failed") ||
		strings.Contains(output, "Data decryption failed") ||
		strings.Contains(output, "incompatible with current rustic version")
}

// wrapDecryptionError annotates a rustic decryption error with operator-
// actionable context. It states explicitly that the S3 repository has not
// been modified, so an operator triaging the failure knows nothing was
// destroyed in response to the error.
func wrapDecryptionError(err error, op, serverUUID string) error {
	return errors.Wrapf(err,
		"rustic %s failed for rustic-repos/%s: if this is a 'MAC check failed' or "+
			"'Data decryption failed' error, the repository password is likely incorrect "+
			"or the rustic binary version has changed since the repo was created. "+
			"The repository has NOT been modified. Verify the panel's stored repository "+
			"password and rustic binary version, then retry. To intentionally start fresh, "+
			"manually delete the rustic-repos/%s prefix from the bucket and retry",
		op, serverUUID, serverUUID)
}
