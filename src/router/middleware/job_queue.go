package middleware

import (
	"github.com/gin-gonic/gin"
	"github.com/pyrohost/elytra/src/jobs"
)

const JobQueueKey = "job_queue"

// AttachJobQueue attaches a job queue instance to the gin context
func AttachJobQueue(queue *jobs.JobQueue) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Set(JobQueueKey, queue)
		c.Next()
	}
}

// ExtractJobQueue extracts the job queue from the gin context
func ExtractJobQueue(c *gin.Context) *jobs.JobQueue {
	if v, ok := c.Get(JobQueueKey); ok {
		return v.(*jobs.JobQueue)
	}
	panic("request does not have a job queue set in the context")
}