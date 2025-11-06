package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"emperror.dev/errors"
	"github.com/apex/log"
)

// LocalRepository implements Repository for local filesystem storage
type LocalRepository struct {
	config   Config
	repoPath string
	logger   *log.Entry
}

// NewLocalRepository creates a new local repository instance
func NewLocalRepository(cfg Config) (*LocalRepository, error) {
	if cfg.LocalPath == "" {
		return nil, errors.New("local path is required")
	}

	// Clean and validate repository path
	repoPath := filepath.Clean(cfg.LocalPath)
	if !filepath.IsAbs(repoPath) {
		return nil, errors.New("repository path must be absolute")
	}

	return &LocalRepository{
		config:   cfg,
		repoPath: repoPath,
		logger:   log.WithField("component", "rustic_local"),
	}, nil
}

// Initialize creates the local repository if it doesn't exist
func (r *LocalRepository) Initialize(ctx context.Context) error {
	// Check if repository already exists
	exists, err := r.Exists(ctx)
	if err != nil {
		return errors.Wrap(err, "failed to check repository existence")
	}

	if exists {
		r.logger.WithField("path", r.repoPath).Debug("repository already exists")
		return nil
	}

	// Create repository directory
	if err := os.MkdirAll(r.repoPath, 0750); err != nil {
		return errors.Wrap(err, "failed to create repository directory")
	}

	// Initialize repository
	cmd, err := r.buildCommand(ctx, "init")
	if err != nil {
		return errors.Wrap(err, "failed to build init command")
	}

	_, err = cmd.CombinedOutput()
	if err != nil {
		return errors.New("repository initialization failed")
	}

	r.logger.WithField("path", r.repoPath).Info("local repository initialized")
	return nil
}

// Exists checks if the repository exists and is accessible
func (r *LocalRepository) Exists(ctx context.Context) (bool, error) {
	// Quick filesystem check first
	if _, err := os.Stat(filepath.Join(r.repoPath, "config")); os.IsNotExist(err) {
		return false, nil
	}

	// Verify with rustic
	cmd, err := r.buildCommand(ctx, "snapshots", "--json")
	if err != nil {
		return false, err
	}

	_, err = cmd.Output()
	return err == nil, nil
}

// Info returns repository information
func (r *LocalRepository) Info(ctx context.Context) (*RepositoryInfo, error) {
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
func (r *LocalRepository) CreateSnapshot(ctx context.Context, path string, tags map[string]string, ignoreFile string) (*Snapshot, error) {
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
		return nil, errors.New("backup failed")
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
func (r *LocalRepository) GetSnapshot(ctx context.Context, id string) (*Snapshot, error) {
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
func (r *LocalRepository) ListSnapshots(ctx context.Context, filter map[string]string) ([]*Snapshot, error) {
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

// DeleteSnapshot deletes a snapshot by ID
func (r *LocalRepository) DeleteSnapshot(ctx context.Context, id string) error {
	if !isValidSnapshotID(id) {
		return errors.New("invalid snapshot ID format")
	}

	// Create a timeout context for the deletion operation
	deleteCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	cmd, err := r.buildCommand(deleteCtx, "forget", "--prune", id)
	if err != nil {
		return errors.Wrap(err, "failed to build delete command")
	}

	output, err := cmd.CombinedOutput()
	if err != nil {
		// Check if snapshot was already deleted
		if strings.Contains(string(output), "not found") || strings.Contains(string(output), "NotFound") {
			r.logger.WithField("snapshot_id", id).Info("snapshot already deleted, treating as success")
			return nil
		}
		// Check for context timeout
		if deleteCtx.Err() == context.DeadlineExceeded {
			r.logger.Error("snapshot deletion timed out")
			return errors.New("snapshot deletion timed out after 60 seconds")
		}
		// Never expose raw rustic errors - they may contain credentials
		r.logger.WithField("snapshot_id", id).WithError(err).Error("snapshot deletion failed")
		return errors.New("failed to delete snapshot")
	}

	r.logger.WithField("snapshot_id", id).Info("snapshot deleted")
	return nil
}

// GetRepositorySize returns the total size of the repository in bytes
func (r *LocalRepository) GetRepositorySize(ctx context.Context) (int64, error) {
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

	r.logger.WithField("total_size_bytes", totalSize).Info("local repository size retrieved")
	return totalSize, nil
}

// GetSnapshotSizes returns the actual size of each snapshot in the repository
func (r *LocalRepository) GetSnapshotSizes(ctx context.Context) (map[string]int64, error) {
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

	// Parse JSON output to get snapshot sizes
	var snapshotList []struct {
		ID   string `json:"id"`
		Size int64  `json:"size"`
	}

	if err := json.Unmarshal(output, &snapshotList); err != nil {
		return nil, errors.New("failed to parse snapshot list")
	}

	// Convert to map of snapshot ID -> size
	snapshotSizes := make(map[string]int64)
	for _, snapshot := range snapshotList {
		snapshotSizes[snapshot.ID] = snapshot.Size
	}

	r.logger.WithField("snapshot_count", len(snapshotSizes)).Info("local repository sizes retrieved")
	return snapshotSizes, nil
}

// RestoreSnapshot restores a snapshot to the target path
func (r *LocalRepository) RestoreSnapshot(ctx context.Context, snapshotID string, targetPath string, sourcePath string) error {
	if !isValidSnapshotID(snapshotID) {
		return errors.New("invalid snapshot ID format")
	}

	// Build restore target (snapshot:path format)
	restoreTarget := fmt.Sprintf("%s:%s", snapshotID, sourcePath)

	cmd, err := r.buildCommand(ctx, "restore", restoreTarget, targetPath)
	if err != nil {
		return errors.Wrap(err, "failed to build restore command")
	}

	if err := cmd.Run(); err != nil {
		return errors.Wrap(err, "restore failed")
	}

	return nil
}

// Destroy completely removes the repository and all its data
func (r *LocalRepository) Destroy(ctx context.Context) error {
	r.logger.WithField("path", r.repoPath).Info("destroying local repository")

	// Check if repository exists
	if _, err := os.Stat(r.repoPath); os.IsNotExist(err) {
		r.logger.Info("repository does not exist, nothing to destroy")
		return nil
	}

	// Remove the entire repository directory
	if err := os.RemoveAll(r.repoPath); err != nil {
		return errors.Wrap(err, "failed to remove repository directory")
	}

	r.logger.Info("local repository destroyed successfully")
	return nil
}

// Close cleans up resources (no-op for local repository)
func (r *LocalRepository) Close() error {
	return nil
}

// buildCommand builds a rustic command for this repository
func (r *LocalRepository) buildCommand(ctx context.Context, args ...string) (*exec.Cmd, error) {
	binaryPath, err := getRusticBinary()
	if err != nil {
		return nil, err
	}

	cmd := exec.CommandContext(ctx, binaryPath, args...)

	// Set environment
	cmd.Env = os.Environ()
	cmd.Env = append(cmd.Env, fmt.Sprintf("RUSTIC_REPOSITORY=%s", r.repoPath))
	cmd.Env = append(cmd.Env, fmt.Sprintf("RUSTIC_PASSWORD=%s", r.config.Password))

	// Set cache directory
	if cacheDir := getCacheDir(); cacheDir != "" {
		cmd.Env = append(cmd.Env, fmt.Sprintf("RUSTIC_CACHE_DIR=%s", cacheDir))
	}

	return cmd, nil
}
