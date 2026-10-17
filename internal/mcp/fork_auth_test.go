package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/docker/cli/cli/command"
	composeapi "github.com/docker/compose/v5/pkg/api"
	"github.com/moby/moby/api/types/container"
	dockerswarmtypes "github.com/moby/moby/api/types/swarm"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kimdre/doco-cd/internal/config/poll"
	"github.com/kimdre/doco-cd/internal/controlplane"
	"github.com/kimdre/doco-cd/internal/docker"
	dockerswarm "github.com/kimdre/doco-cd/internal/docker/swarm"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/rbac"
	"github.com/kimdre/doco-cd/internal/restapi"
	"github.com/kimdre/doco-cd/internal/scheduler"
)

// forkMCPPolicy gives team-a the read and run permissions the tests exercise and nothing else:
// in particular projects/get without projects/delete, the case that motivates per-tool checks.
// team-z appears nowhere, so it has no permission at all.
const forkMCPPolicy = `p, team-a, runs, get, *, allow
p, team-a, syncwindows, get, *, allow
p, team-a, storage, update, *, allow
p, team-a, projects, get, default/allowed, allow
p, team-a, stacks, get, default/allowed, allow
p, team-a, jobs, get, default/allowed, allow
p, team-a, jobs, run, default/allowed, allow
`

// forkTokenVerifier accepts any bearer token and uses it as the subject.
type forkTokenVerifier struct{}

func (forkTokenVerifier) Verify(_ context.Context, raw string) (rbac.Principal, error) {
	return rbac.Principal{Subject: raw}, nil
}

// forkMCPRuns implements RunOperations without a control plane and records what reached it.
type forkMCPRuns struct {
	jobs        []controlplane.ScheduledJobInfo
	listJobsErr error
	triggered   []string
	compactions int
	pollCalls   int
	pollCtx     context.Context
	pollConfigs []poll.Config
}

func (f *forkMCPRuns) List(int, string, string) []controlplane.Run { return []controlplane.Run{} }

func (f *forkMCPRuns) Get(jobID string) (controlplane.Run, bool) {
	return controlplane.Run{JobID: jobID}, true
}

func (f *forkMCPRuns) ListScheduledJobs(context.Context, string, string) ([]controlplane.ScheduledJobInfo, error) {
	return f.jobs, f.listJobsErr
}

func (f *forkMCPRuns) TriggerScheduledJob(_ context.Context, _, _, jobName, _ string, _ bool) (string, error) {
	f.triggered = append(f.triggered, jobName)

	return "job-run", nil
}

func (f *forkMCPRuns) TriggerPoll(ctx context.Context, configs []poll.Config, _ bool, _ *slog.Logger) (string, error) {
	f.pollCalls++
	f.pollCtx = ctx
	f.pollConfigs = configs

	return "poll-run", nil
}

func (f *forkMCPRuns) TriggerMirrorCompaction(context.Context, string, controlplane.MirrorCompactionRequest, bool) (string, error) {
	f.compactions++

	return "compact-run", nil
}

func newForkMCPRuns() *forkMCPRuns {
	return &forkMCPRuns{jobs: []controlplane.ScheduledJobInfo{
		{JobInfo: scheduler.JobInfo{Name: "job-allowed", Context: "default", Stack: "allowed"}},
		{JobInfo: scheduler.JobInfo{Name: "job-denied", Context: "default", Stack: "denied"}},
	}}
}

// startForkMCPFakeDocker serves a Swarm manager with the projects and stacks "allowed" and
// "denied": the info, ping and list endpoints answer, every other request (per-project and
// per-stack lookups, mutations) gets 500, so a call that passes authorization by mistake fails
// loudly instead of touching anything.
func startForkMCPFakeDocker(t *testing.T) {
	t.Helper()

	containers := make([]container.Summary, 0, 2)
	services := make([]dockerswarmtypes.Service, 0, 2)

	for _, name := range []string{"allowed", "denied"} {
		containers = append(containers, container.Summary{
			ID:    "id-" + name,
			Names: []string{"/" + name + "-app-1"},
			State: container.StateRunning,
			Labels: map[string]string{
				composeapi.ProjectLabel:     name,
				composeapi.ServiceLabel:     "app",
				composeapi.ConfigHashLabel:  "hash",
				composeapi.ConfigFilesLabel: "/srv/" + name + "/compose.yaml",
			},
		})
		services = append(services, dockerswarmtypes.Service{
			ID: "svc-" + name,
			Spec: dockerswarmtypes.ServiceSpec{
				Annotations: dockerswarmtypes.Annotations{
					Name:   name + "_app",
					Labels: map[string]string{dockerswarm.StackNamespaceLabel: name},
				},
			},
		})
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		filters := r.URL.Query().Get("filters")

		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/_ping"):
			_, _ = w.Write([]byte("OK"))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/info"):
			_, _ = w.Write([]byte(`{"Swarm":{"LocalNodeState":"active","ControlAvailable":true}}`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/containers/json") &&
			!strings.Contains(filters, composeapi.ProjectLabel+"="):
			_ = json.NewEncoder(w).Encode(containers)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/services") && filters == "":
			_ = json.NewEncoder(w).Encode(services)
		default:
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"message":"fake docker: unsupported request"}`))
		}
	}))
	t.Cleanup(server.Close)
	t.Setenv("DOCKER_HOST", "tcp://"+strings.TrimPrefix(server.URL, "http://"))
	t.Setenv("DOCKER_API_VERSION", "1.52")
}

func newForkMCPEnforcer(t *testing.T) *rbac.Enforcer {
	t.Helper()

	path := filepath.Join(t.TempDir(), "rbac.csv")
	if err := os.WriteFile(path, []byte(forkMCPPolicy), 0o600); err != nil {
		t.Fatalf("write rbac policy: %v", err)
	}

	e, err := rbac.NewFileEnforcer(path, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewFileEnforcer() error = %v", err)
	}

	return e
}

// newForkMCPServer serves the MCP handler on the fake Docker daemon, with an Authenticator that
// takes API keys and any bearer token. enforcer nil is the upstream handler without RBAC.
func newForkMCPServer(t *testing.T, runs *forkMCPRuns, enforcer *rbac.Enforcer) *httptest.Server {
	t.Helper()

	startForkMCPFakeDocker(t)

	dockerCli, err := docker.CreateDockerCli(true)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = dockerCli.Client().Close() })

	mcpHandler := newHandler(Dependencies{
		Version:        "test",
		MaxPayloadSize: 1 << 20,
		Logger:         logger.New(logger.LevelCritical),
		DockerCLI:      dockerCli,
		Contexts:       docker.NewContextRegistry(dockerCli, docker.ContextRegistryOptions{SwarmFeatures: true}),
		Runs:           runs,
		Auth:           &restapi.Authenticator{APISecret: testMCPAPIKey, APIKeyEnabled: true, JWT: forkTokenVerifier{}},
		RBAC:           enforcer,
		PollConfig: []poll.Config{
			{SourceUrl: validPollSourceURL, Reference: "refs/heads/main", Interval: 5 * time.Minute},
		},
	})

	mux := http.NewServeMux()
	mux.Handle("POST "+testMCPPath, mcpHandler)

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	return server
}

// headerRoundTripper sets one request header on every MCP request.
type headerRoundTripper struct {
	name, value string
	base        http.RoundTripper
}

func (rt headerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	cloned := req.Clone(req.Context())
	cloned.Header.Set(rt.name, rt.value)

	return rt.base.RoundTrip(cloned)
}

// connectForkMCPClient connects to endpoint sending the header name: value on every request.
func connectForkMCPClient(t *testing.T, server *httptest.Server, endpoint, name, value string) *sdkmcp.ClientSession {
	t.Helper()

	httpClient := server.Client()
	httpClient.Transport = headerRoundTripper{name: name, value: value, base: httpClient.Transport}
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "doco-cd-fork-test", Version: "test"}, nil)

	session, err := client.Connect(t.Context(), &sdkmcp.StreamableClientTransport{
		Endpoint:             endpoint,
		HTTPClient:           httpClient,
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		if err := session.Close(); err != nil {
			t.Errorf("close MCP session: %v", err)
		}
	})

	return session
}

func connectForkBearer(t *testing.T, server *httptest.Server, token string) *sdkmcp.ClientSession {
	t.Helper()

	return connectForkMCPClient(t, server, server.URL+testMCPPath, "Authorization", "Bearer "+token)
}

// TestForkMCPPrincipalReachesToolCall checks the assumption the per-tool checks rest on: with the
// stateless transport the context of a tool call derives from the HTTP request, so the principal
// ServeHTTP stores there is visible inside the tool.
func TestForkMCPPrincipalReachesToolCall(t *testing.T) {
	probe := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "probe", Version: "test"}, nil)
	seen := make(chan rbac.Principal, 2)

	sdkmcp.AddTool(probe, &sdkmcp.Tool{Name: "probe"}, func(ctx context.Context, _ *sdkmcp.CallToolRequest, _ struct{}) (*sdkmcp.CallToolResult, any, error) {
		p, ok := rbac.PrincipalFrom(ctx)
		if !ok {
			return nil, nil, errors.New("no principal in the tool call context")
		}

		seen <- p

		return &sdkmcp.CallToolResult{Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: "ok"}}}, nil, nil
	})

	h := &Handler{
		auth: &restapi.Authenticator{APISecret: testMCPAPIKey, APIKeyEnabled: true, JWT: forkTokenVerifier{}},
		log:  logger.New(logger.LevelCritical),
		// Same transport options as newHandler.
		transport: sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return probe }, &sdkmcp.StreamableHTTPOptions{
			Stateless:                    true,
			PropagateRequestCancellation: true,
			DisableLocalhostProtection:   true,
		}),
	}

	server := httptest.NewServer(h)
	t.Cleanup(server.Close)

	for _, testCase := range []struct {
		name, header, value, want string
	}{
		{name: "bearer token", header: "Authorization", value: "Bearer team-a", want: "team-a"},
		{name: "API key", header: restapi.KeyHeader, value: testMCPAPIKey, want: rbac.APIKeySubject},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			session := connectForkMCPClient(t, server, server.URL, testCase.header, testCase.value)
			callMCPTool(t, session, "probe", map[string]any{})

			select {
			case p := <-seen:
				if p.Subject != testCase.want {
					t.Fatalf("principal subject = %q, want %q", p.Subject, testCase.want)
				}
			case <-time.After(time.Second):
				t.Fatal("probe tool was not called")
			}
		})
	}
}

// TestForkMCPToolPermissionsCoverRegisteredTools fails when registerTools gains a tool without a
// toolPermissions entry, or the table keeps a tool that no longer exists.
func TestForkMCPToolPermissionsCoverRegisteredTools(t *testing.T) {
	server, _ := newMCPTestServer(t, true, testMCPAPIKey, 1024)
	session := connectMCPTestClient(t, server)

	result, err := session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}

	registered := make([]string, 0, len(result.Tools))
	for _, tool := range result.Tools {
		registered = append(registered, tool.Name)
	}

	slices.Sort(registered)

	classified := slices.Sorted(maps.Keys(toolPermissions))
	if !slices.Equal(registered, classified) {
		t.Fatalf("registered tools %v, toolPermissions %v", registered, classified)
	}
}

// TestForkMCPToolsDenyWithoutPermission calls every tool that acts on one object (or on "*")
// as a principal without any permission: each one must answer forbidden and reach nothing.
func TestForkMCPToolsDenyWithoutPermission(t *testing.T) {
	runs := newForkMCPRuns()
	server := newForkMCPServer(t, runs, newForkMCPEnforcer(t))
	session := connectForkBearer(t, server, "team-z")

	calls := map[string]map[string]any{
		"list_deployment_runs":  {},
		"get_deployment_run":    {"job_id": "run-1"},
		"trigger_scheduled_job": {"job_name": "job-allowed"},
		"list_sync_windows":     {},
		"get_project":           {"project_name": "allowed"},
		"control_project":       {"project_name": "allowed", "action": "restart"},
		"destroy_project":       {"project_name": "allowed"},
		"get_stack":             {"stack_name": "allowed"},
		"control_stack":         {"stack_name": "allowed", "action": "restart"},
		"remove_stack":          {"stack_name": "allowed"},
		"compact_mirrors":       {},
	}

	// The list tools filter instead of failing, and trigger_poll is restricted to the operator's
	// targets with targets/sync checked in pre-deploy; both have their own tests.
	listOrPoll := map[string]bool{"list_scheduled_jobs": true, "list_projects": true, "list_stacks": true, "trigger_poll": true}

	for tool, permission := range toolPermissions {
		if permission == "" || listOrPoll[tool] {
			continue
		}

		arguments, ok := calls[tool]
		if !ok {
			t.Fatalf("no deny case for tool %s (%s)", tool, permission)
		}

		t.Run(tool, func(t *testing.T) {
			assertMCPToolError(t, session, tool, arguments, errToolForbidden.Error())
		})
	}

	if len(runs.triggered) != 0 || runs.compactions != 0 {
		t.Fatalf("denied calls reached the control plane: triggered %v, compactions %d", runs.triggered, runs.compactions)
	}
}

// TestForkMCPDestroyProjectNeedsDelete: projects/get on the object does not grant destroy_project
// nor control_project.
func TestForkMCPDestroyProjectNeedsDelete(t *testing.T) {
	server := newForkMCPServer(t, newForkMCPRuns(), newForkMCPEnforcer(t))
	session := connectForkBearer(t, server, "team-a")

	assertMCPToolError(t, session, "destroy_project", map[string]any{"project_name": "allowed"}, errToolForbidden.Error())
	assertMCPToolError(t, session, "control_project", map[string]any{"project_name": "allowed", "action": "stop"}, errToolForbidden.Error())
	assertMCPToolError(t, session, "remove_stack", map[string]any{"stack_name": "allowed"}, errToolForbidden.Error())
}

func TestForkMCPListsAreFiltered(t *testing.T) {
	server := newForkMCPServer(t, newForkMCPRuns(), newForkMCPEnforcer(t))
	session := connectForkBearer(t, server, "team-a")

	var projects listProjectsOutput
	decodeMCPStructuredContent(t, callMCPTool(t, session, "list_projects", map[string]any{"all": true}), &projects)

	if len(projects.Projects) != 1 || projects.Projects[0].Name != "allowed" {
		t.Fatalf("list_projects = %#v, want only allowed", projects.Projects)
	}

	var stacks listStacksOutput
	decodeMCPStructuredContent(t, callMCPTool(t, session, "list_stacks", map[string]any{}), &stacks)

	if _, ok := stacks.Stacks["allowed"]; !ok || len(stacks.Stacks) != 1 {
		t.Fatalf("list_stacks = %#v, want only allowed", stacks.Stacks)
	}

	var jobs listScheduledJobsOutput
	decodeMCPStructuredContent(t, callMCPTool(t, session, "list_scheduled_jobs", map[string]any{}), &jobs)

	if len(jobs.Jobs) != 1 || jobs.Jobs[0].Name != "job-allowed" {
		t.Fatalf("list_scheduled_jobs = %#v, want only job-allowed", jobs.Jobs)
	}

	// A principal without permissions sees nothing.
	denied := connectForkBearer(t, server, "team-z")
	assertMCPToolError(t, denied, "list_projects", map[string]any{"all": true}, "no projects found")
	assertMCPToolError(t, denied, "list_stacks", map[string]any{}, "no stacks found")
	decodeMCPStructuredContent(t, callMCPTool(t, denied, "list_scheduled_jobs", map[string]any{}), &jobs)

	if len(jobs.Jobs) != 0 {
		t.Fatalf("list_scheduled_jobs for team-z = %#v, want none", jobs.Jobs)
	}
}

func TestForkMCPAllowedCallsReachOperations(t *testing.T) {
	runs := newForkMCPRuns()
	server := newForkMCPServer(t, runs, newForkMCPEnforcer(t))
	session := connectForkBearer(t, server, "team-a")

	callMCPTool(t, session, "list_deployment_runs", map[string]any{})
	callMCPTool(t, session, "get_deployment_run", map[string]any{"job_id": "run-1"})
	callMCPTool(t, session, "list_sync_windows", map[string]any{})
	callMCPTool(t, session, "compact_mirrors", map[string]any{})
	callMCPTool(t, session, "trigger_scheduled_job", map[string]any{"job_name": "job-allowed"})

	assertMCPToolError(t, session, "trigger_scheduled_job", map[string]any{"job_name": "job-denied"}, errToolForbidden.Error())
	assertMCPToolError(t, session, "trigger_scheduled_job", map[string]any{"job_name": "job-missing"}, scheduler.ErrScheduledJobNotFound.Error())

	if !slices.Equal(runs.triggered, []string{"job-allowed"}) || runs.compactions != 1 {
		t.Fatalf("triggered %v, compactions %d; want [job-allowed] and 1", runs.triggered, runs.compactions)
	}

	runs.listJobsErr = errors.New("scheduler down")

	assertMCPToolError(t, session, "trigger_scheduled_job", map[string]any{"job_name": "job-allowed"}, "failed to list scheduled jobs")
}

func TestForkMCPTriggerPollRestricted(t *testing.T) {
	runs := newForkMCPRuns()
	server := newForkMCPServer(t, runs, newForkMCPEnforcer(t))
	session := connectForkBearer(t, server, "team-a")

	assertMCPToolError(t, session, "trigger_poll", map[string]any{
		"configs": []any{map[string]any{"url": "https://example.com/other.git"}},
	}, "poll configuration not allowed")

	if runs.pollCalls != 0 {
		t.Fatalf("rejected poll configurations reached TriggerPoll %d times", runs.pollCalls)
	}

	callMCPTool(t, session, "trigger_poll", map[string]any{
		"configs": []any{map[string]any{"url": validPollSourceURL, "reference": "main"}},
	})

	if runs.pollCalls != 1 || len(runs.pollConfigs) != 1 || runs.pollConfigs[0].Interval != 5*time.Minute {
		t.Fatalf("TriggerPoll got %#v, want the operator's entry", runs.pollConfigs)
	}

	if p, ok := rbac.PrincipalFrom(runs.pollCtx); !ok || p.Subject != "team-a" {
		t.Fatalf("TriggerPoll principal = %#v, %v; want team-a", p, ok)
	}
}

// TestForkMCPWithoutRBACIsUpstream: with no enforcer nothing is checked, filtered or restricted.
func TestForkMCPWithoutRBACIsUpstream(t *testing.T) {
	runs := newForkMCPRuns()
	server := newForkMCPServer(t, runs, nil)
	session := connectForkMCPClient(t, server, server.URL+testMCPPath, restapi.KeyHeader, testMCPAPIKey)

	var jobs listScheduledJobsOutput
	decodeMCPStructuredContent(t, callMCPTool(t, session, "list_scheduled_jobs", map[string]any{}), &jobs)

	if len(jobs.Jobs) != 2 {
		t.Fatalf("list_scheduled_jobs = %#v, want both jobs", jobs.Jobs)
	}

	var stacks listStacksOutput
	decodeMCPStructuredContent(t, callMCPTool(t, session, "list_stacks", map[string]any{}), &stacks)

	if len(stacks.Stacks) != 2 {
		t.Fatalf("list_stacks = %#v, want both stacks", stacks.Stacks)
	}

	// No lookup before the trigger: the scheduler decides, as upstream.
	callMCPTool(t, session, "trigger_scheduled_job", map[string]any{"job_name": "job-missing"})
	callMCPTool(t, session, "trigger_poll", map[string]any{
		"configs": []any{map[string]any{"url": "https://example.com/other.git"}},
	})

	if !slices.Equal(runs.triggered, []string{"job-missing"}) || runs.pollCalls != 1 || runs.pollConfigs[0].SourceUrl != "https://example.com/other.git" {
		t.Fatalf("triggered %v, poll configs %#v; want the requests unchanged", runs.triggered, runs.pollConfigs)
	}

	h := &Handler{}
	if err := h.authorizeTool(t.Context(), "projects", "delete", "default/x"); err != nil {
		t.Fatalf("authorizeTool() without RBAC and without principal = %v, want nil", err)
	}
}

func TestForkMCPAuthorizeToolWithoutPrincipal(t *testing.T) {
	h := &Handler{rbac: newForkMCPEnforcer(t)}

	if err := h.authorizeTool(t.Context(), "runs", "get", "*"); !errors.Is(err, errToolForbidden) {
		t.Fatalf("authorizeTool() without principal = %v, want forbidden", err)
	}

	ctx := rbac.WithPrincipal(t.Context(), rbac.Principal{Subject: "team-a"})
	if err := h.authorizeTool(ctx, "runs", "get", "*"); err != nil {
		t.Fatalf("authorizeTool() for team-a = %v, want nil", err)
	}
}

func TestForkMCPDenyUnclassifiedTools(t *testing.T) {
	calls := 0
	next := func(context.Context, string, sdkmcp.Request) (sdkmcp.Result, error) {
		calls++

		return &sdkmcp.CallToolResult{}, nil
	}
	handler := denyUnclassifiedTools(next)

	result, err := handler(t.Context(), "tools/call", &sdkmcp.CallToolRequest{Params: &sdkmcp.CallToolParamsRaw{Name: "unclassified_tool"}})
	if err != nil {
		t.Fatal(err)
	}

	toolResult, ok := result.(*sdkmcp.CallToolResult)
	if !ok || !toolResult.IsError || calls != 0 {
		t.Fatalf("unclassified tool result = %#v, next calls %d; want a forbidden tool error", result, calls)
	}

	if text, ok := toolResult.Content[0].(*sdkmcp.TextContent); !ok || text.Text != errToolForbidden.Error() {
		t.Fatalf("unclassified tool content = %#v, want forbidden", toolResult.Content)
	}

	if _, err := handler(t.Context(), "tools/call", &sdkmcp.CallToolRequest{Params: &sdkmcp.CallToolParamsRaw{Name: "get_health"}}); err != nil || calls != 1 {
		t.Fatalf("classified tool: err %v, next calls %d; want it forwarded", err, calls)
	}

	if _, err := handler(t.Context(), "tools/list", &sdkmcp.ListToolsRequest{}); err != nil || calls != 2 {
		t.Fatalf("tools/list: err %v, next calls %d; want it forwarded", err, calls)
	}
}

// TestForkMCPNewHandlerWithoutAPISecret: a JWT-only setup (Auth set, no API secret) starts.
func TestForkMCPNewHandlerWithoutAPISecret(t *testing.T) {
	dockerCli, err := command.NewDockerCli()
	if err != nil {
		t.Fatal(err)
	}

	_, err = NewHandler(Dependencies{
		Version:        "test",
		MaxPayloadSize: 1024,
		Logger:         logger.New(logger.LevelCritical),
		DockerCLI:      dockerCli,
		Contexts:       &docker.ContextRegistry{},
		Runs:           newForkMCPRuns(),
		Auth:           &restapi.Authenticator{JWT: forkTokenVerifier{}},
	})
	if err != nil {
		t.Fatalf("NewHandler() with Auth and no APISecret = %v, want nil", err)
	}
}

func TestForkMCPRejectsInvalidBearer(t *testing.T) {
	server := newForkMCPServer(t, newForkMCPRuns(), newForkMCPEnforcer(t))

	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+testMCPPath, strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}

	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Basic team-a")

	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close() //nolint:errcheck

	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusUnauthorized)
	}
}
