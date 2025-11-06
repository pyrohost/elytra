package router

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/pyrohost/elytra/src/jobs"
	"github.com/pyrohost/elytra/src/router/middleware"
)

func getJobStatus(c *gin.Context) {
	manager := middleware.ExtractJobManager(c)
	jobID := c.Param("job_id")

	instance, exists := manager.GetJob(jobID)
	if !exists {
		c.JSON(http.StatusNotFound, gin.H{
			"error": "Job not found",
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"job_id":     instance.Job.GetID(),
		"type":       instance.Job.GetType(),
		"status":     instance.Status,
		"progress":   instance.Progress,
		"message":    instance.Message,
		"error":      instance.Error,
		"result":     instance.Result,
		"created_at": instance.CreatedAt,
		"updated_at": instance.UpdatedAt,
	})
}

func postCreateJob(c *gin.Context) {
	manager := middleware.ExtractJobManager(c)

	var request struct {
		JobType string                 `json:"job_type" binding:"required"`
		JobData map[string]interface{} `json:"job_data" binding:"required"`
	}

	if err := c.BindJSON(&request); err != nil {
		return
	}

	jobID, err := manager.CreateJob(request.JobType, request.JobData)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusAccepted, gin.H{
		"job_id": jobID,
		"status": "pending",
	})
}

func putUpdateJob(c *gin.Context) {
	manager := middleware.ExtractJobManager(c)
	jobID := c.Param("job_id")

	var request struct {
		Status   string      `json:"status"`
		Progress int         `json:"progress"`
		Message  string      `json:"message"`
		Result   interface{} `json:"result"`
	}

	if err := c.BindJSON(&request); err != nil {
		return
	}

	if err := manager.UpdateJob(jobID, jobs.Status(request.Status), request.Progress, request.Message, request.Result); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true})
}

func deleteJob(c *gin.Context) {
	manager := middleware.ExtractJobManager(c)
	jobID := c.Param("job_id")

	if err := manager.CancelJob(jobID); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true})
}
