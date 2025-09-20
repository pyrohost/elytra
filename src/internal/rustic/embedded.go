package rustic

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"emperror.dev/errors"
	"github.com/apex/log"
)

//go:generate ../../../scripts/download-rustic.sh

//go:embed binaries/rustic-linux-amd64
var rusticLinuxAMD64 []byte

//go:embed binaries/rustic-linux-arm64
var rusticLinuxARM64 []byte

var (
	extractedBinaryPath string
	extractMutex        sync.Mutex
	extractOnce         sync.Once
)

func GetBinaryPath() (string, error) {
	var extractErr error

	extractOnce.Do(func() {
		extractErr = extractBinary()
	})

	if extractErr != nil {
		return "", extractErr
	}

	return extractedBinaryPath, nil
}

func extractBinary() error {
	extractMutex.Lock()
	defer extractMutex.Unlock()

	binaryData, err := getPlatformBinary()
	if err != nil {
		return errors.Wrap(err, "failed to get platform binary")
	}

	tempDir, err := os.MkdirTemp("", "elytra-rustic-")
	if err != nil {
		return errors.Wrap(err, "failed to create temp directory")
	}

	if err := os.Chmod(tempDir, 0700); err != nil {
		os.RemoveAll(tempDir)
		return errors.Wrap(err, "failed to set temp directory permissions")
	}

	binaryName := "rustic"
	if runtime.GOOS == "windows" {
		binaryName = "rustic.exe"
	}

	binaryPath := filepath.Join(tempDir, binaryName)

	if err := os.WriteFile(binaryPath, binaryData, 0755); err != nil {
		os.RemoveAll(tempDir)
		return errors.Wrap(err, "failed to write binary file")
	}

	if runtime.GOOS != "windows" {
		if err := os.Chmod(binaryPath, 0755); err != nil {
			os.RemoveAll(tempDir)
			return errors.Wrap(err, "failed to set binary permissions")
		}
	}

	extractedBinaryPath = binaryPath

	log.WithFields(log.Fields{
		"platform": fmt.Sprintf("%s/%s", runtime.GOOS, runtime.GOARCH),
		"path":     binaryPath,
		"size":     len(binaryData),
	}).Info("extracted embedded rustic binary")

	return nil
}

func getPlatformBinary() ([]byte, error) {
	platform := fmt.Sprintf("%s/%s", runtime.GOOS, runtime.GOARCH)

	switch platform {
	case "linux/amd64":
		return rusticLinuxAMD64, nil
	case "linux/arm64":
		return rusticLinuxARM64, nil
	default:
		return nil, errors.Errorf("unsupported platform: %s", platform)
	}
}

func IsAvailable() bool {
	_, err := getPlatformBinary()
	return err == nil
}

func CleanupExtractedBinary() {
	if extractedBinaryPath != "" {
		tempDir := filepath.Dir(extractedBinaryPath)
		if err := os.RemoveAll(tempDir); err != nil {
			log.WithField("error", err).Warn("failed to cleanup extracted rustic binary")
		} else {
			log.WithField("path", tempDir).Debug("cleaned up extracted rustic binary")
		}
		extractedBinaryPath = ""
	}
}

func GetSupportedPlatforms() []string {
	return []string{
		"linux/amd64",
		"linux/arm64",
	}
}
