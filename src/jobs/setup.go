package jobs

import (
	"github.com/pyrohost/elytra/src/remote"
	"github.com/pyrohost/elytra/src/server"
)

// SetupJobs registers all available job types with the manager
func SetupJobs(manager *Manager, serverManager *server.Manager, client remote.Client) {
	manager.RegisterJobType("backup_create", func(data map[string]interface{}) (Job, error) {
		return NewBackupCreateJob(data, serverManager, client)
	})

	manager.RegisterJobType("backup_delete", func(data map[string]interface{}) (Job, error) {
		return NewBackupDeleteJob(data, serverManager, client)
	})

	manager.RegisterJobType("backup_restore", func(data map[string]interface{}) (Job, error) {
		return NewBackupRestoreJob(data, serverManager, client)
	})

}