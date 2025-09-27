package middleware

import (
	"github.com/gin-gonic/gin"
	"github.com/pyrohost/elytra/src/jobs"
)

const JobManagerKey = "job_manager"

func AttachJobManager(manager *jobs.Manager) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Set(JobManagerKey, manager)
		c.Next()
	}
}

func ExtractJobManager(c *gin.Context) *jobs.Manager {
	if v, ok := c.Get(JobManagerKey); ok {
		return v.(*jobs.Manager)
	}
	panic("request does not have a job manager set in the context")
}