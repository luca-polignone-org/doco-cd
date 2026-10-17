package api

import (
	"log/slog"
	"net/http"

	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/restapi"
	"github.com/kimdre/doco-cd/internal/stages"
)

// unblockAction releases a target blocked by the fork's rollback flow.
const unblockAction = "unblock"

// projectActions lists the values of the project action enum. unblock is there only with
// HOOK_PRE_ROLLBACK_URL set, so without the key the OpenAPI document is the upstream one.
func projectActions(cfg *app.Config) []any {
	actions := []any{"start", "stop", "restart", "recreate"}
	if cfg != nil && cfg.HookPreRollbackURL != "" {
		actions = append(actions, unblockAction)
	}

	return actions
}

// unblockProject puts a blocked target back to failed with the same commit (stages.UnblockTarget),
// so the next poll retries it. 409 when the target is not blocked.
func (h *Handler) unblockProject(w http.ResponseWriter, r *http.Request, jobLog *slog.Logger, jobID, contextName, projectName string) {
	if !requireMethod(w, jobLog, r, http.MethodPost) {
		return
	}

	object := deploy.ObjectName(contextName, projectName)

	changed, err := stages.UnblockTarget(r.Context(), h.appConfig.DataMountPath, object)
	if err != nil {
		jobLog.With(logger.ErrAttr(err)).Error("failed to unblock project", slog.String("object", object))
		restapi.JSONError(w, "failed to unblock project", err.Error(), jobID, http.StatusInternalServerError)

		return
	}

	if !changed {
		restapi.JSONError(w, "project is not blocked: "+projectName, "", jobID, http.StatusConflict)

		return
	}

	jobLog.Info("project unblocked", slog.String("object", object))
	restapi.JSONResponse(w, "unblocked", jobID, http.StatusOK)
}
