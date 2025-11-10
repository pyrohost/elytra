package router

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"syscall"

	"github.com/apex/log"
	"github.com/gin-gonic/gin"

	"github.com/pyrohost/elytra/src/config"
	"github.com/pyrohost/elytra/src/environment"
)

type transferValidationRequest struct {
	ServerUUID  string       `json:"server_uuid" binding:"required"`
	DiskMB      int64        `json:"disk_mb" binding:"required"`
	MemoryMB    int64        `json:"memory_mb" binding:"required"`
	Allocations []allocation `json:"allocations" binding:"required"`
}

type allocation struct {
	IP   string `json:"ip" binding:"required"`
	Port int    `json:"port" binding:"required"`
}

type transferValidationResponse struct {
	Success  bool     `json:"success"`
	Errors   []string `json:"errors,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
}

func postTransferValidate(c *gin.Context) {
	var req transferValidationRequest
	if err := c.BindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, transferValidationResponse{
			Success: false,
			Errors:  []string{"Invalid request format"},
		})
		return
	}

	var errors []string
	var warnings []string

	// 1. Check Docker daemon health
	ctx := context.Background()

	dockerClient, err := environment.Docker()
	if err != nil {
		errors = append(errors, "Docker daemon is unhealthy or unreachable")
	} else {
		if _, err := dockerClient.Ping(ctx); err != nil {
			errors = append(errors, "Docker daemon is unhealthy or unreachable")
		}
	}

	diskNeeded := req.DiskMB * 2 * 1024 * 1024 // Convert MB to bytes and double for safety margin
	dataPath := config.Get().System.Data

	var stat syscall.Statfs_t
	if err := syscall.Statfs(dataPath, &stat); err != nil {
		errors = append(errors, fmt.Sprintf("Failed to check disk space: %v", err))
	} else {
		diskAvailable := stat.Bavail * uint64(stat.Bsize)
		diskNeededGB := float64(diskNeeded) / 1024 / 1024 / 1024
		diskAvailableGB := float64(diskAvailable) / 1024 / 1024 / 1024

		if diskAvailable < uint64(diskNeeded) {
			errors = append(errors, fmt.Sprintf(
				"Insufficient disk space: need %.2f GB, have %.2f GB available",
				diskNeededGB, diskAvailableGB,
			))
		} else if diskAvailable < uint64(diskNeeded)*2 {
			warnings = append(warnings, fmt.Sprintf(
				"Low disk space: need %.2f GB, have %.2f GB available",
				diskNeededGB, diskAvailableGB,
			))
		}
	}

	// 3. Check port availability
	for _, alloc := range req.Allocations {
		if !isPortAvailable(alloc.IP, alloc.Port) {
			errors = append(errors, fmt.Sprintf("Port %s:%d is already in use", alloc.IP, alloc.Port))
		}
	}

	// 4. Check if temp directory is writable
	tempDir := config.Get().System.TmpDirectory
	testFile := fmt.Sprintf("%s/.write-test-%s", tempDir, req.ServerUUID)
	if err := os.WriteFile(testFile, []byte("test"), 0644); err != nil {
		errors = append(errors, fmt.Sprintf("Temp directory not writable: %v", err))
	} else {
		os.Remove(testFile)
	}

	// 5. Check for existing server with same UUID
	stagingDir := fmt.Sprintf("%s/transfer-%s", tempDir, req.ServerUUID)
	if _, err := os.Stat(stagingDir); err == nil {
		warnings = append(warnings, "Staging directory already exists, will be cleaned")
	}

	success := len(errors) == 0

	if !success {
		log.WithFields(log.Fields{
			"server_uuid": req.ServerUUID,
			"errors":      errors,
		}).Warn("transfer validation failed")
	}

	c.JSON(http.StatusOK, transferValidationResponse{
		Success:  success,
		Errors:   errors,
		Warnings: warnings,
	})
}

func isPortAvailable(ip string, port int) bool {
	address := fmt.Sprintf("%s:%d", ip, port)
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return false
	}
	listener.Close()
	return true
}
