package api

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/kimdre/doco-cd/internal/common/id"
	"github.com/kimdre/doco-cd/internal/config/poll"
	"github.com/kimdre/doco-cd/internal/controlplane"

	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/rbac"
	"github.com/kimdre/doco-cd/internal/restapi"
	"github.com/kimdre/doco-cd/internal/stages"
)

// TriggerPollHandler validates and runs poll configurations from the request body.
func (h *Handler) TriggerPollHandler(w http.ResponseWriter, r *http.Request) {
	// Add a job id to the context to track deployments in the logs
	jobID := id.New()
	jobLog := h.log.With(slog.String("job_id", jobID), slog.String("ip", h.requestIP(r)))

	jobLog.Debug("received api request")

	if !requireMethod(w, jobLog, r, http.MethodPost) {
		return
	}

	principal, ok := h.authenticate(w, r, jobLog, jobID)
	if !ok {
		return
	}
	defer func() {
		_ = r.Body.Close()
	}()

	wait, ok := getBoolQueryParam(r, w, jobLog, jobID, "wait", true)
	if !ok {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, h.appConfig.MaxPayloadSize)

	decoder := json.NewDecoder(r.Body)

	var pollConfigs []poll.Config
	if err := decoder.Decode(&pollConfigs); err != nil {
		h.pollDecodeError(w, jobID, err)

		return
	}

	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("request body must contain a single JSON value")
		}

		h.pollDecodeError(w, jobID, err)

		return
	}

	// Fork: with RBAC a caller picks which operator poll target to re-run, not how; the
	// principal travels with the run so that pre-deploy can check targets/sync.
	ctx := r.Context()

	if h.rbac.Enabled() {
		allowed, err := controlplane.RestrictPollConfigs(h.appConfig.PollConfig, pollConfigs)
		if err != nil {
			jobLog.Warn("poll configuration not allowed", logger.ErrAttr(err))
			restapi.JSONError(w, "poll configuration not allowed", err.Error(), jobID, http.StatusBadRequest)

			return
		}

		pollConfigs = allowed
		ctx = rbac.WithPrincipal(ctx, principal)
	}

	jobID, err := h.controlPlaneRuns.TriggerPoll(ctx, pollConfigs, wait, jobLog)
	if err != nil {
		if blocked, ok := errors.AsType[*stages.SyncWindowBlockedError](err); ok {
			jobLog.Info("poll jobs deferred by sync window", slog.String("reason", blocked.Error()))
			restapi.JSONResponse(w, blocked.Error(), jobID, http.StatusAccepted)

			return
		}

		if controlplane.IsLifecycleCancellation(err) {
			restapi.JSONError(w, err.Error(), "", jobID, http.StatusServiceUnavailable)

			return
		}

		if _, ok := errors.AsType[*controlplane.PollRunsFailedError](err); ok && wait {
			restapi.JSONError(w, err.Error(), "", jobID, http.StatusInternalServerError)

			return
		}

		errMsg := err.Error()
		detail := ""

		if validationErr, ok := errors.AsType[*controlplane.PollConfigValidationError](err); ok {
			errMsg = "invalid poll configuration at index " + strconv.Itoa(validationErr.Index)
			detail = validationErr.Err.Error()
		}

		jobLog.Error(errMsg, logger.ErrAttr(err))
		restapi.JSONError(w, errMsg, detail, jobID, http.StatusBadRequest)

		return
	}

	if wait {
		restapi.JSONResponse(w, "poll jobs complete", jobID, http.StatusOK)

		return
	}

	restapi.JSONResponse(w, "poll jobs started", jobID, http.StatusAccepted)
}

func (h *Handler) pollDecodeError(w http.ResponseWriter, jobID string, err error) {
	errMsg := "failed to decode json in body"
	h.log.Error(errMsg, logger.ErrAttr(err))

	status := http.StatusBadRequest
	if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
		status = http.StatusRequestEntityTooLarge
	}

	restapi.JSONError(w, errMsg, err.Error(), jobID, status)
}
