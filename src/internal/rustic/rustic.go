package rustic

import (
	"os/exec"

	"emperror.dev/errors"
	"github.com/apex/log"
)

func GetBinaryPath() (string, error) {
	path, err := exec.LookPath("rustic")
	if err != nil {
		return "", errors.Wrap(err, "rustic binary not found in PATH")
	}

	log.WithField("path", path).Debug("found rustic binary in PATH")
	return path, nil
}

func IsAvailable() bool {
	_, err := exec.LookPath("rustic")
	return err == nil
}
