package api

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/rbac"
	"github.com/kimdre/doco-cd/internal/restapi"
	"github.com/kimdre/doco-cd/internal/scheduler"
)

const errForbidden = "forbidden"

// authenticator returns the configured Authenticator or, for a Handler built without one, the
// upstream x-api-key check.
func (h *Handler) authenticator() *restapi.Authenticator {
	if h.auth != nil {
		return h.auth
	}

	return &restapi.Authenticator{APISecret: h.appConfig.ApiSecret, APIKeyEnabled: true}
}

// authenticate replaces the ValidateApiKey block of every handler: same 401 and same message
// as upstream, so existing clients see no difference.
func (h *Handler) authenticate(w http.ResponseWriter, r *http.Request, jobLog *slog.Logger, jobID string) (rbac.Principal, bool) {
	p, err := h.authenticator().Authenticate(r)
	if err != nil {
		jobLog.Error(restapi.ErrInvalidApiKey.Error())
		restapi.JSONError(w, restapi.ErrInvalidApiKey.Error(), "", jobID, http.StatusUnauthorized)

		return rbac.Principal{}, false
	}

	return p, true
}

// authorize checks resource/action on object and writes a 403 when the principal is denied.
// Without RBAC (nil enforcer) it always allows. The Enforcer writes the audit line of every
// deny, so nothing is logged here.
func (h *Handler) authorize(w http.ResponseWriter, jobID string, p rbac.Principal, resource, action, object string) bool {
	if h.rbac.Enforce(p, resource, action, object) {
		return true
	}

	restapi.JSONError(w, errForbidden, resource+", "+action+", "+object, jobID, http.StatusForbidden)

	return false
}

// filterAllowed keeps the items whose object the principal may get on resource. Without RBAC
// it returns items unchanged, so list responses stay byte-for-byte upstream.
func filterAllowed[T any](h *Handler, p rbac.Principal, resource string, items []T, object func(T) string) []T {
	if !h.rbac.Enabled() {
		return items
	}

	out := make([]T, 0, len(items))

	for _, item := range items {
		if h.rbac.Enforce(p, resource, "get", object(item)) {
			out = append(out, item)
		}
	}

	return out
}

// authorizeScheduledJob checks jobs/run on every scheduled job the request can select. The
// scheduler resolves the job only inside the run, so the handler looks it up first with the
// same name and stack filter; a name that matches nothing is answered with the scheduler's
// not-found error instead of being triggered unchecked.
func (h *Handler) authorizeScheduledJob(w http.ResponseWriter, r *http.Request, jobLog *slog.Logger, jobID string, p rbac.Principal,
	contextName, jobName, stackName string,
) bool {
	if !h.rbac.Enabled() {
		return true
	}

	jobs, err := h.controlPlaneRuns.ListScheduledJobs(r.Context(), contextName, stackName)
	if err != nil {
		errMsg := "failed to list scheduled jobs"
		jobLog.With(logger.ErrAttr(err)).Error(errMsg)
		restapi.JSONError(w, errMsg, err.Error(), jobID, http.StatusInternalServerError)

		return false
	}

	name := strings.TrimSpace(jobName)
	found := false

	for _, job := range jobs {
		if job.Name != name {
			continue
		}

		found = true

		if !h.authorize(w, jobID, p, "jobs", "run", deploy.ObjectName(job.Context, job.Stack)) {
			return false
		}
	}

	if !found {
		restapi.JSONError(w, scheduler.ErrScheduledJobNotFound.Error(), "", jobID, http.StatusNotFound)

		return false
	}

	return true
}
