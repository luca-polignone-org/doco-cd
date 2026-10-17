package mcp

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/config/poll"
	"github.com/kimdre/doco-cd/internal/controlplane"
	"github.com/kimdre/doco-cd/internal/rbac"
	"github.com/kimdre/doco-cd/internal/scheduler"
)

var errToolForbidden = errors.New("forbidden")

// toolPermissions documents the resource/action of every tool. The completeness test fails when
// registerTools adds a tool that is missing here, and with RBAC enabled denyUnclassifiedTools
// refuses such a tool at runtime until someone classifies it. An empty value means no check.
// trigger_poll is not checked in the tool: targets/sync is checked in pre-deploy, once the run
// knows <context>/<project>.
var toolPermissions = map[string]string{
	"get_health":            "",
	"list_deployment_runs":  "runs/get",
	"get_deployment_run":    "runs/get",
	"list_scheduled_jobs":   "jobs/get",
	"trigger_scheduled_job": "jobs/run",
	"list_sync_windows":     "syncwindows/get",
	"list_projects":         "projects/get",
	"get_project":           "projects/get",
	"control_project":       "projects/action",
	"destroy_project":       "projects/delete",
	"list_stacks":           "stacks/get",
	"get_stack":             "stacks/get",
	"control_stack":         "stacks/action",
	"remove_stack":          "stacks/delete",
	"trigger_poll":          "targets/sync",
	"compact_mirrors":       "storage/update",
}

// authorizeTool checks resource/action on object for the principal that ServeHTTP put in ctx.
// Without RBAC (nil enforcer) it always allows. The Enforcer writes the audit line of every deny.
func (h *Handler) authorizeTool(ctx context.Context, resource, action, object string) error {
	p, ok := rbac.PrincipalFrom(ctx)
	if !ok && h.rbac.Enabled() {
		return errToolForbidden
	}

	if !h.rbac.Enforce(p, resource, action, object) {
		return errToolForbidden
	}

	return nil
}

// allowedObject is the filter of the list tools: true when the principal may get the object.
func (h *Handler) allowedObject(ctx context.Context, resource, object string) bool {
	return h.authorizeTool(ctx, resource, "get", object) == nil
}

// filterAllowedObjects keeps the items whose object the principal may get on resource. Without
// RBAC it returns items unchanged, so list outputs stay identical to upstream.
func filterAllowedObjects[T any](ctx context.Context, h *Handler, resource string, items []T, object func(T) string) []T {
	if !h.rbac.Enabled() {
		return items
	}

	out := make([]T, 0, len(items))

	for _, item := range items {
		if h.allowedObject(ctx, resource, object(item)) {
			out = append(out, item)
		}
	}

	return out
}

// filterAllowedStacks drops the stacks the principal may not get. Without RBAC it does nothing.
func filterAllowedStacks[V any](ctx context.Context, h *Handler, contextName string, stacks map[string]V) {
	if !h.rbac.Enabled() {
		return
	}

	maps.DeleteFunc(stacks, func(name string, _ V) bool {
		return !h.allowedObject(ctx, "stacks", deploy.ObjectName(contextName, name))
	})
}

// authorizeScheduledJob checks jobs/run on every scheduled job the call can select, as the REST
// route does: the scheduler resolves the job only inside the run, so the job is looked up first
// with the same name and stack filter, and a name that matches nothing is answered with the
// scheduler's not-found error instead of being triggered unchecked. Without RBAC it allows.
func (h *Handler) authorizeScheduledJob(ctx context.Context, contextName, jobName, stackName string) error {
	if !h.rbac.Enabled() {
		return nil
	}

	jobs, err := h.controlPlaneRuns.ListScheduledJobs(ctx, contextName, stackName)
	if err != nil {
		return fmt.Errorf("failed to list scheduled jobs: %w", err)
	}

	name := strings.TrimSpace(jobName)
	found := false

	for _, job := range jobs {
		if job.Name != name {
			continue
		}

		found = true

		if err := h.authorizeTool(ctx, "jobs", "run", deploy.ObjectName(job.Context, job.Stack)); err != nil {
			return err
		}
	}

	if !found {
		return scheduler.ErrScheduledJobNotFound
	}

	return nil
}

// restrictPollConfigs admits, with RBAC, only entries of the operator's poll configuration: a
// caller picks which target to re-run, not how. Without RBAC it returns configs unchanged.
func (h *Handler) restrictPollConfigs(configs []poll.Config) ([]poll.Config, error) {
	if !h.rbac.Enabled() {
		return configs, nil
	}

	allowed, err := controlplane.RestrictPollConfigs(h.pollConfig, configs)
	if err != nil {
		return nil, fmt.Errorf("poll configuration not allowed: %w", err)
	}

	return allowed, nil
}

// denyUnclassifiedTools refuses a tools/call whose tool has no toolPermissions entry, so that a
// tool arriving with an upstream merge is not callable unchecked once RBAC is enabled.
func denyUnclassifiedTools(next sdkmcp.MethodHandler) sdkmcp.MethodHandler {
	return func(ctx context.Context, method string, req sdkmcp.Request) (sdkmcp.Result, error) {
		if call, ok := req.(*sdkmcp.CallToolRequest); ok {
			if _, known := toolPermissions[call.Params.Name]; !known {
				return &sdkmcp.CallToolResult{
					IsError: true,
					Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: errToolForbidden.Error()}},
				}, nil
			}
		}

		return next(ctx, method, req)
	}
}
