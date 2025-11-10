package router

import (
	"context"
	"net/http"
	"strings"
	"time"

	"emperror.dev/errors"
	"github.com/gin-gonic/gin"

	"github.com/pyrohost/elytra/src/environment"
	"github.com/pyrohost/elytra/src/router/middleware"
	"github.com/pyrohost/elytra/src/server"
	"github.com/pyrohost/elytra/src/server/installer"
	"github.com/pyrohost/elytra/src/server/transfer"
)

// Data passed over to initiate a server transfer.
type serverTransferRequest struct {
	URL    string                  `binding:"required" json:"url"`
	Token  string                  `binding:"required" json:"token"`
	Server installer.ServerDetails `json:"server"`
}

// postServerTransfer handles the start of a transfer for a server.
func postServerTransfer(c *gin.Context) {
	var data serverTransferRequest
	if err := c.BindJSON(&data); err != nil {
		return
	}

	s := ExtractServer(c)

	// Check if the server is already being transferred.
	// There will be another endpoint for resetting this value either by deleting the
	// server, or by canceling the transfer.
	if s.IsTransferring() {
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{
			"error": "A transfer is already in progress for this server.",
		})
		return
	}

	manager := middleware.ExtractManager(c)

	notifyPanelOfFailure := func() {
		if err := manager.Client().SetTransferStatus(context.Background(), s.ID(), false); err != nil {
			s.Log().WithField("subsystem", "transfer").
				WithField("status", false).
				WithError(err).
				Error("failed to set transfer status")
		}

		s.Events().Publish(server.TransferStatusEvent, "failure")
		s.SetTransferring(false)
	}

	// Block the server from starting while we are transferring it.
	s.SetTransferring(true)

	if s.Environment.State() != environment.ProcessOfflineState {
		s.Log().Info("stopping server for transfer")

		if err := s.Environment.Stop(s.Context()); err != nil {
			if !strings.Contains(strings.ToLower(err.Error()), "no such container") {
				s.Log().WithError(err).Warn("graceful stop failed, will force")
			}
		}

		stopCtx, stopCancel := context.WithTimeout(s.Context(), time.Second*10)
		err := s.Environment.WaitForStop(stopCtx, time.Second*10, false)
		stopCancel()

		if err != nil && !strings.Contains(strings.ToLower(err.Error()), "no such container") {
			s.Log().WithError(err).Warn("graceful stop timeout, forcing kill")

			if killErr := s.Environment.Terminate(context.Background(), "SIGKILL"); killErr != nil {
				if !strings.Contains(strings.ToLower(killErr.Error()), "no such container") {
					s.SetTransferring(false)
					middleware.CaptureAndAbort(c, errors.Wrap(killErr, "failed to force stop server"))
					return
				}
			}
		}

		finalState := s.Environment.State()
		if finalState != environment.ProcessOfflineState {
			s.SetTransferring(false)
			middleware.CaptureAndAbort(c, errors.New("server still running after force stop"))
			return
		}

		s.Log().Info("server stopped successfully for transfer")

		time.Sleep(2 * time.Second)
	}

	// Create a new transfer instance for this server.
	trnsfr := transfer.New(context.Background(), s)
	transfer.Outgoing().Add(trnsfr)
	trnsfr.StartHeartbeat(manager.Client())

	go func() {
		defer transfer.Outgoing().Remove(trnsfr)

		if _, err := trnsfr.PushArchiveToTarget(data.URL, data.Token); err != nil {
			notifyPanelOfFailure()

			if err == context.Canceled {
				trnsfr.Log().Debug("canceled")
				trnsfr.SendMessage("Canceled.")
				return
			}

			trnsfr.Log().WithError(err).Error("failed to push archive to target")
			return
		}

		// DO NOT NOTIFY THE PANEL OF SUCCESS HERE. The only node that should send
		// a success status is the destination node.  When we send a failure status,
		// the panel will automatically cancel the transfer and attempt to reset
		// the server state on the destination node, we just need to make sure
		// we clean up our statuses for failure.

		trnsfr.Log().Debug("transfer complete")
	}()

	c.Status(http.StatusAccepted)
}

// deleteServerTransfer cancels an outgoing transfer for a server.
func deleteServerTransfer(c *gin.Context) {
	s := ExtractServer(c)

	if !s.IsTransferring() {
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{
			"error": "Server is not currently being transferred.",
		})
		return
	}

	trnsfr := transfer.Outgoing().Get(s.ID())
	if trnsfr == nil {
		c.AbortWithStatusJSON(http.StatusConflict, gin.H{
			"error": "Server is not currently being transferred.",
		})
		return
	}

	trnsfr.Cancel()

	c.Status(http.StatusAccepted)
}
