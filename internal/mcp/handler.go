package mcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"strings"

	"github.com/docker/cli/cli/command"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kimdre/doco-cd/internal/common/defaults"
	"github.com/kimdre/doco-cd/internal/common/validation"
	"github.com/kimdre/doco-cd/internal/config/poll"
	"github.com/kimdre/doco-cd/internal/controlplane"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/rbac"
	"github.com/kimdre/doco-cd/internal/restapi"
	"github.com/kimdre/doco-cd/internal/syncwindow"
)

// RunOperations is the control-plane surface consumed by the MCP server.
type RunOperations interface {
	List(limit int, trigger, status string) []controlplane.Run
	Get(jobID string) (controlplane.Run, bool)
	ListScheduledJobs(ctx context.Context, contextName, stackName string) ([]controlplane.ScheduledJobInfo, error)
	TriggerScheduledJob(ctx context.Context, jobID, contextName, jobName, stackName string, wait bool) (string, error)
	TriggerPoll(ctx context.Context, configs []poll.Config, wait bool, log *slog.Logger) (string, error)
	TriggerMirrorCompaction(ctx context.Context, jobID string, req controlplane.MirrorCompactionRequest, wait bool) (string, error)
}

// Dependencies contains the runtime services and HTTP settings required by Handler.
type Dependencies struct {
	Version              string `validate:"required"`
	APISecret            string `validate:"required_without=Auth"`
	MaxPayloadSize       int64  `validate:"min=1"`
	TrustedProxyHeader   string `default:"X-Forwarded-For"`
	TrustedProxyNetworks []netip.Prefix
	Logger               *logger.Logger          `validate:"required,nostructlevel"`
	DockerCLI            command.Cli             `validate:"required,nostructlevel"`
	Contexts             *docker.ContextRegistry `validate:"required,nostructlevel"`
	Runs                 RunOperations           `validate:"required,nostructlevel"`
	// SyncWindows is the sync-window policy. Nil means no windows.
	SyncWindows *syncwindow.Policy `validate:"-"`
	// Fork: Auth authenticates every request; nil means the upstream x-api-key check on APISecret.
	Auth *restapi.Authenticator `validate:"-"`
	// Fork: RBAC authorizes every tool call; nil disables authorization (upstream behavior).
	RBAC *rbac.Enforcer `validate:"-"`
	// Fork: PollConfig is the operator's poll configuration, the only targets trigger_poll may
	// run when RBAC is enabled.
	PollConfig []poll.Config `validate:"-"`
}

// Handler serves the stateless MCP HTTP transport and owns all MCP tools.
type Handler struct {
	auth                 *restapi.Authenticator
	rbac                 *rbac.Enforcer
	pollConfig           []poll.Config
	trustedProxyHeader   string
	trustedProxyNetworks []netip.Prefix
	log                  *logger.Logger
	dockerCli            command.Cli
	contexts             *docker.ContextRegistry
	controlPlaneRuns     RunOperations
	syncWindows          *syncwindow.Policy
	transport            http.Handler
}

// NewHandler applies defaults, validates dependencies, and constructs the MCP handler.
func NewHandler(dependencies Dependencies) (*Handler, error) {
	dependencies.Version = strings.TrimSpace(dependencies.Version)
	dependencies.TrustedProxyHeader = strings.TrimSpace(dependencies.TrustedProxyHeader)

	// Apply defaults to settings only; defaults.Set recursively traverses structs
	// and must not walk injected service implementations.
	defaultValues := Dependencies{TrustedProxyHeader: dependencies.TrustedProxyHeader}
	if err := defaults.Set(&defaultValues); err != nil {
		return nil, fmt.Errorf("set MCP dependency defaults: %w", err)
	}

	dependencies.TrustedProxyHeader = defaultValues.TrustedProxyHeader

	if err := validation.Validate(dependencies); err != nil {
		return nil, fmt.Errorf("validate MCP dependencies: %w", err)
	}

	if dependencies.Logger.Logger == nil {
		return nil, errors.New("mcp logger is required")
	}

	return newHandler(dependencies), nil
}

// newHandler registers MCP tools and builds the authenticated stateless HTTP transport.
func newHandler(dependencies Dependencies) *Handler {
	auth := dependencies.Auth
	if auth == nil {
		auth = &restapi.Authenticator{APISecret: dependencies.APISecret, APIKeyEnabled: true}
	}

	h := &Handler{
		auth:                 auth,
		rbac:                 dependencies.RBAC,
		pollConfig:           dependencies.PollConfig,
		trustedProxyHeader:   strings.TrimSpace(dependencies.TrustedProxyHeader),
		trustedProxyNetworks: append([]netip.Prefix(nil), dependencies.TrustedProxyNetworks...),
		log: &logger.Logger{
			Logger: dependencies.Logger.With(slog.String("component", "mcp")),
			Level:  dependencies.Logger.Level,
		},
		dockerCli:        dependencies.DockerCLI,
		contexts:         dependencies.Contexts,
		controlPlaneRuns: dependencies.Runs,
		syncWindows:      dependencies.SyncWindows,
	}

	mcpLogger := slog.New(&sdkLogHandler{next: h.log.Handler()})
	server := sdkmcp.NewServer(
		&sdkmcp.Implementation{Name: "doco-cd", Version: dependencies.Version},
		&sdkmcp.ServerOptions{Logger: mcpLogger},
	)

	h.registerTools(server)

	// Fork: with RBAC a registered tool that toolPermissions does not classify is refused.
	if h.rbac.Enabled() {
		server.AddReceivingMiddleware(denyUnclassifiedTools)
	}

	h.transport = sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server {
		return server
	}, &sdkmcp.StreamableHTTPOptions{
		Stateless:                    true,
		PropagateRequestCancellation: true,
		// Same-host reverse proxies are supported, and every request requires API-key authentication.
		DisableLocalhostProtection: true,
		MaxRequestBodyBytes:        dependencies.MaxPayloadSize,
		Logger:                     mcpLogger,
	})

	return h
}

// registerTools installs the complete MCP tool catalog on server.
func (h *Handler) registerTools(server *sdkmcp.Server) {
	h.addHealthTool(server)
	h.addRunTools(server)
	h.addScheduledJobReadTool(server)
	h.addSyncWindowTool(server)
	h.addProjectReadTools(server)
	h.addStackReadTools(server)
	h.addProjectTools(server)
	h.addStackTools(server)
	h.addScheduledJobTriggerTool(server)
	h.addPollTool(server)
	h.addStorageTools(server)
}

// ServeHTTP authenticates a request before forwarding it to the MCP transport.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	principal, err := h.auth.Authenticate(r)
	if err != nil {
		h.log.Error(restapi.ErrInvalidApiKey.Error(), slog.String("ip", h.requestIP(r)))
		http.Error(w, http.StatusText(http.StatusUnauthorized), http.StatusUnauthorized)

		return
	}

	// Fork: the principal travels in the context down to the tool calls, which authorize one
	// by one; the transport is stateless and every POST carries its own credentials.
	h.transport.ServeHTTP(w, r.WithContext(rbac.WithPrincipal(r.Context(), principal)))
}

// requestIP resolves the client address while honoring configured trusted proxies.
func (h *Handler) requestIP(r *http.Request) string {
	return restapi.ResolveRequestIP(
		r.RemoteAddr,
		h.trustedProxyHeader,
		r.Header,
		h.trustedProxyNetworks,
	)
}
