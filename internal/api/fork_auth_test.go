package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	composeapi "github.com/docker/compose/v5/pkg/api"
	"github.com/moby/moby/api/types/container"
	dockerswarmtypes "github.com/moby/moby/api/types/swarm"

	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/config/poll"
	"github.com/kimdre/doco-cd/internal/controlplane"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/docker/swarm"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/rbac"
	restAPI "github.com/kimdre/doco-cd/internal/restapi"
	"github.com/kimdre/doco-cd/internal/scheduler"
	"github.com/kimdre/doco-cd/internal/syncwindow"
)

const forkAuthSecret = "fork-auth-secret" // #nosec G101 -- test fixture, not a real credential.

// forkAllowPolicy grants the API key every REST permission, but only on default/allowed for
// the routes that name an object.
const forkAllowPolicy = `p, apikey, runs, get, *, allow
p, apikey, storage, update, *, allow
p, apikey, syncwindows, get, *, allow
p, apikey, projects, *, default/allowed, allow
p, apikey, projects, action/*, default/allowed, allow
p, apikey, stacks, *, default/allowed, allow
p, apikey, stacks, action/*, default/allowed, allow
p, apikey, jobs, *, default/allowed, allow
p, apikey, targets, sync, default/allowed, allow
`

// forkDenyPolicy grants nothing to the API key.
const forkDenyPolicy = "p, someone-else, runs, get, *, allow\n"

type forkRouteKind int

const (
	// forkRouteObject is denied with 403 when the principal lacks the permission.
	forkRouteObject forkRouteKind = iota
	// forkRouteList filters its response instead of answering 403.
	forkRouteList
	// forkRoutePoll is restricted to the operator's poll targets; targets/sync is checked
	// later, in pre-deploy (internal/stages).
	forkRoutePoll
)

type forkRouteCase struct {
	kind forkRouteKind
	// method and path of a request on an object the allow policy grants.
	method, path, body string
	// deniedPath, if set, names an object the allow policy does not grant.
	deniedPath string
}

// forkAuthorizedRoutes lists every REST operation protected by the API key, keyed by
// "<method> <route pattern>". TestForkAuthorizedRoutesCoverRouteCatalog fails when an
// operation of createRouteCatalog is missing, so an endpoint merged from upstream without
// authorization breaks CI.
var forkAuthorizedRoutes = map[string]forkRouteCase{
	"GET " + APIPath + "/runs":        {kind: forkRouteObject, method: http.MethodGet, path: APIPath + "/runs"},
	"GET " + APIPath + "/run/{jobID}": {kind: forkRouteObject, method: http.MethodGet, path: APIPath + "/run/some-job"},
	"GET " + APIPath + "/jobs":        {kind: forkRouteList, method: http.MethodGet, path: APIPath + "/jobs"},
	"POST " + APIPath + "/job/{jobName}/run": {
		kind: forkRouteObject, method: http.MethodPost,
		path: APIPath + "/job/job-allowed/run", deniedPath: APIPath + "/job/job-denied/run",
	},
	"GET " + APIPath + "/projects": {kind: forkRouteList, method: http.MethodGet, path: APIPath + "/projects"},
	"GET " + APIPath + "/project/{projectName}": {
		kind: forkRouteObject, method: http.MethodGet,
		path: APIPath + "/project/allowed", deniedPath: APIPath + "/project/denied",
	},
	// An invalid timeout stops the request right after authorization, before any removal.
	"DELETE " + APIPath + "/project/{projectName}": {
		kind: forkRouteObject, method: http.MethodDelete,
		path: APIPath + "/project/allowed?timeout=x", deniedPath: APIPath + "/project/denied?timeout=x",
	},
	"POST " + APIPath + "/project/{projectName}/{action}": {
		kind: forkRouteObject, method: http.MethodPost,
		path: APIPath + "/project/allowed/restart", deniedPath: APIPath + "/project/denied/restart",
	},
	"GET " + APIPath + "/stacks": {kind: forkRouteList, method: http.MethodGet, path: APIPath + "/stacks"},
	"GET " + APIPath + "/stack/{stackName}": {
		kind: forkRouteObject, method: http.MethodGet,
		path: APIPath + "/stack/allowed", deniedPath: APIPath + "/stack/denied",
	},
	"DELETE " + APIPath + "/stack/{stackName}": {
		kind: forkRouteObject, method: http.MethodDelete,
		path: APIPath + "/stack/allowed", deniedPath: APIPath + "/stack/denied",
	},
	"POST " + APIPath + "/stack/{stackName}/{action}": {
		kind: forkRouteObject, method: http.MethodPost,
		path: APIPath + "/stack/allowed/restart", deniedPath: APIPath + "/stack/denied/restart",
	},
	"POST " + APIPath + "/poll/run": {
		kind: forkRoutePoll, method: http.MethodPost, path: APIPath + "/poll/run",
		body: `[{"url": "` + validPollSourceURL + `", "reference": "main"}]`,
	},
	"POST " + APIPath + "/storage/compact": {kind: forkRouteObject, method: http.MethodPost, path: APIPath + "/storage/compact"},
	"GET " + APIPath + "/sync-windows":     {kind: forkRouteObject, method: http.MethodGet, path: APIPath + "/sync-windows"},
}

func TestForkAuthorizedRoutesCoverRouteCatalog(t *testing.T) {
	t.Parallel()

	routes, err := createRouteCatalog(&Handler{appConfig: &app.Config{}, log: logger.New(logger.LevelCritical)}, Mounts{}, newSchemaBuilder())
	if err != nil {
		t.Fatalf("create route catalog: %v", err)
	}

	seen := map[string]bool{}

	for _, route := range routes {
		for _, op := range route.Operations {
			if !requiresAPIKey(op) {
				continue
			}

			key := op.Method + " " + route.Pattern
			seen[key] = true

			if _, ok := forkAuthorizedRoutes[key]; !ok {
				t.Errorf("route %q requires the API key but has no entry in forkAuthorizedRoutes: add its authorization", key)
			}
		}
	}

	for key := range forkAuthorizedRoutes {
		if !seen[key] {
			t.Errorf("forkAuthorizedRoutes entry %q is not an API-key route of the catalog", key)
		}
	}
}

func requiresAPIKey(op Operation) bool {
	if op.Operation.Security == nil {
		return false
	}

	for _, requirement := range *op.Operation.Security {
		if _, ok := requirement[apiKeySecurityScheme]; ok {
			return true
		}
	}

	return false
}

// forkFakeRuns implements RunOperations without a control plane.
type forkFakeRuns struct {
	jobs        []controlplane.ScheduledJobInfo
	listJobsErr error
	triggered   []string
	pollCtx     context.Context
	pollConfigs []poll.Config
}

func (f *forkFakeRuns) List(int, string, string) []controlplane.Run { return []controlplane.Run{} }

func (f *forkFakeRuns) Get(jobID string) (controlplane.Run, bool) {
	return controlplane.Run{JobID: jobID}, true
}

func (f *forkFakeRuns) ListScheduledJobs(context.Context, string, string) ([]controlplane.ScheduledJobInfo, error) {
	return f.jobs, f.listJobsErr
}

func (f *forkFakeRuns) TriggerScheduledJob(_ context.Context, jobID, _, jobName, _ string, _ bool) (string, error) {
	f.triggered = append(f.triggered, jobName)

	return jobID, nil
}

func (f *forkFakeRuns) TriggerPoll(ctx context.Context, configs []poll.Config, _ bool, _ *slog.Logger) (string, error) {
	f.pollCtx = ctx
	f.pollConfigs = configs

	return "poll-job", nil
}

func (f *forkFakeRuns) TriggerMirrorCompaction(context.Context, string, controlplane.MirrorCompactionRequest, bool) (string, error) {
	return "compact-job", nil
}

func newForkScheduledJobs() []controlplane.ScheduledJobInfo {
	return []controlplane.ScheduledJobInfo{
		{JobInfo: scheduler.JobInfo{Name: "job-allowed", Context: "default", Stack: "allowed"}},
		{JobInfo: scheduler.JobInfo{Name: "job-denied", Context: "default", Stack: "denied"}},
	}
}

// startForkFakeDocker serves the list endpoints of the projects "allowed" and "denied" (Compose
// containers and Swarm services) and answers 500 to every other request, including the
// per-project and per-stack lookups, so no handler can reach a mutation.
func startForkFakeDocker(t *testing.T) {
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
					Labels: map[string]string{swarm.StackNamespaceLabel: name},
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

func newForkEnforcer(t *testing.T, policy string) *rbac.Enforcer {
	t.Helper()

	path := filepath.Join(t.TempDir(), "rbac.csv")
	if err := os.WriteFile(path, []byte(policy), 0o600); err != nil {
		t.Fatalf("write rbac policy: %v", err)
	}

	e, err := rbac.NewFileEnforcer(path, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewFileEnforcer() error = %v", err)
	}

	return e
}

// newForkAuthHandler builds a Handler on the fake Docker daemon. With enforcer nil and
// withAuth false it is the upstream Handler: no Authenticator, no RBAC.
func newForkAuthHandler(t *testing.T, runs *forkFakeRuns, enforcer *rbac.Enforcer, withAuth bool) *Handler {
	t.Helper()

	dockerCli, err := docker.CreateDockerCli(true)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = dockerCli.Client().Close() })

	windows, err := syncwindow.New(nil)
	if err != nil {
		t.Fatal(err)
	}

	h := &Handler{
		appConfig: &app.Config{
			ApiSecret:      forkAuthSecret,
			MaxPayloadSize: 1 << 20,
			SyncWindows:    windows,
			PollConfig: []poll.Config{
				{SourceUrl: validPollSourceURL, Reference: "refs/heads/main", Interval: 5 * time.Minute},
			},
		},
		log:              logger.New(logger.LevelCritical),
		dockerCli:        dockerCli,
		controlPlaneRuns: runs,
		rbac:             enforcer,
	}

	if withAuth {
		h.auth = &restAPI.Authenticator{APISecret: forkAuthSecret, APIKeyEnabled: true}
	}

	return h
}

func newForkMux(t *testing.T, h *Handler) *http.ServeMux {
	t.Helper()

	routes, err := createRouteCatalog(h, Mounts{}, newSchemaBuilder())
	if err != nil {
		t.Fatalf("create route catalog: %v", err)
	}

	mux := http.NewServeMux()

	for _, route := range routes {
		if route.Handler != nil {
			mux.Handle(route.Pattern, route.Handler)
		}
	}

	return mux
}

func serveFork(t *testing.T, mux *http.ServeMux, method, target, body string, withKey bool) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()

	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}

	req := httptest.NewRequestWithContext(t.Context(), method, target, reader)
	if withKey {
		req.Header.Set(restAPI.KeyHeader, forkAuthSecret)
	}

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	var decoded map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("%s %s: decode response %q: %v", method, target, rr.Body.String(), err)
	}

	return rr, decoded
}

func requireForbidden(t *testing.T, rr *httptest.ResponseRecorder, body map[string]any, label string) {
	t.Helper()

	if rr.Code != http.StatusForbidden || body["error"] != errForbidden {
		t.Fatalf("%s: status = %d, body = %v; want 403 %q", label, rr.Code, body, errForbidden)
	}
}

func requireNotRejected(t *testing.T, rr *httptest.ResponseRecorder, body map[string]any, label string) {
	t.Helper()

	if rr.Code == http.StatusForbidden || rr.Code == http.StatusUnauthorized {
		t.Fatalf("%s: status = %d, body = %v; want neither 401 nor 403", label, rr.Code, body)
	}
}

func TestForkAuthRoutes(t *testing.T) {
	startForkFakeDocker(t)

	allow := newForkEnforcer(t, forkAllowPolicy)
	deny := newForkEnforcer(t, forkDenyPolicy)

	for key, tc := range forkAuthorizedRoutes {
		t.Run(key, func(t *testing.T) {
			denyMux := newForkMux(t, newForkAuthHandler(t, &forkFakeRuns{jobs: newForkScheduledJobs()}, deny, true))
			allowMux := newForkMux(t, newForkAuthHandler(t, &forkFakeRuns{jobs: newForkScheduledJobs()}, allow, true))
			upstreamMux := newForkMux(t, newForkAuthHandler(t, &forkFakeRuns{jobs: newForkScheduledJobs()}, nil, false))

			rr, body := serveFork(t, denyMux, tc.method, tc.path, tc.body, true)
			if tc.kind == forkRouteObject {
				requireForbidden(t, rr, body, "principal without permissions")
			} else {
				requireNotRejected(t, rr, body, "principal without permissions on a list or poll route")
			}

			rr, body = serveFork(t, allowMux, tc.method, tc.path, tc.body, true)
			requireNotRejected(t, rr, body, "principal with permission")

			if tc.deniedPath != "" {
				rr, body = serveFork(t, allowMux, tc.method, tc.deniedPath, tc.body, true)
				requireForbidden(t, rr, body, "object outside the permission")
			}

			for name, mux := range map[string]*http.ServeMux{"rbac": allowMux, "upstream": upstreamMux} {
				rr, body = serveFork(t, mux, tc.method, tc.path, tc.body, false)

				jobID, _ := body["job_id"].(string)
				want := map[string]any{"error": restAPI.ErrInvalidApiKey.Error(), "job_id": jobID}

				if rr.Code != http.StatusUnauthorized || jobID == "" || !reflect.DeepEqual(body, want) {
					t.Fatalf("%s without credentials: status = %d, body = %v; want the upstream 401 %v", name, rr.Code, body, want)
				}
			}

			rr, body = serveFork(t, upstreamMux, tc.method, tc.path, tc.body, true)
			requireNotRejected(t, rr, body, "upstream handler with the API key")
		})
	}
}

func TestForkAuthListsAreFiltered(t *testing.T) {
	startForkFakeDocker(t)

	allow := newForkEnforcer(t, forkAllowPolicy)
	deny := newForkEnforcer(t, forkDenyPolicy)

	names := func(t *testing.T, mux *http.ServeMux, path, field string) []string {
		t.Helper()

		rr, body := serveFork(t, mux, http.MethodGet, path, "", true)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET %s: status = %d, body = %v", path, rr.Code, body)
		}

		var out []string

		switch content := body["content"].(type) {
		case []any:
			for _, item := range content {
				out = append(out, item.(map[string]any)[field].(string))
			}
		case map[string]any:
			for name := range content {
				out = append(out, name)
			}
		}

		slices.Sort(out)

		return out
	}

	allowMux := newForkMux(t, newForkAuthHandler(t, &forkFakeRuns{jobs: newForkScheduledJobs()}, allow, true))
	upstreamMux := newForkMux(t, newForkAuthHandler(t, &forkFakeRuns{jobs: newForkScheduledJobs()}, nil, false))
	denyMux := newForkMux(t, newForkAuthHandler(t, &forkFakeRuns{jobs: newForkScheduledJobs()}, deny, true))

	for _, tc := range []struct {
		path, field, want string
	}{
		{path: APIPath + "/projects", field: "Name", want: "allowed"},
		{path: APIPath + "/stacks", want: "allowed"},
		{path: APIPath + "/jobs", field: "name", want: "job-allowed"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			if got := names(t, allowMux, tc.path, tc.field); !slices.Equal(got, []string{tc.want}) {
				t.Fatalf("with RBAC = %v, want only the permitted object", got)
			}

			if got := names(t, upstreamMux, tc.path, tc.field); len(got) != 2 {
				t.Fatalf("without RBAC = %v, want both objects (no filtering)", got)
			}
		})
	}

	t.Run("everything filtered out", func(t *testing.T) {
		for _, path := range []string{APIPath + "/projects", APIPath + "/stacks"} {
			rr, body := serveFork(t, denyMux, http.MethodGet, path, "", true)
			if rr.Code != http.StatusNotFound {
				t.Fatalf("GET %s: status = %d, body = %v; want the upstream 404 for an empty list", path, rr.Code, body)
			}
		}

		rr, body := serveFork(t, denyMux, http.MethodGet, APIPath+"/jobs", "", true)
		if content, ok := body["content"].([]any); rr.Code != http.StatusOK || !ok || len(content) != 0 {
			t.Fatalf("GET jobs: status = %d, body = %v; want 200 with an empty list", rr.Code, body)
		}
	})
}

func TestForkAuthScheduledJobRun(t *testing.T) {
	startForkFakeDocker(t)

	allow := newForkEnforcer(t, forkAllowPolicy)

	t.Run("allowed job is triggered", func(t *testing.T) {
		runs := &forkFakeRuns{jobs: newForkScheduledJobs()}
		mux := newForkMux(t, newForkAuthHandler(t, runs, allow, true))

		rr, body := serveFork(t, mux, http.MethodPost, APIPath+"/job/job-allowed/run", "", true)
		if rr.Code != http.StatusOK || !slices.Equal(runs.triggered, []string{"job-allowed"}) {
			t.Fatalf("status = %d, body = %v, triggered = %v", rr.Code, body, runs.triggered)
		}
	})

	t.Run("denied job is not triggered", func(t *testing.T) {
		runs := &forkFakeRuns{jobs: newForkScheduledJobs()}
		mux := newForkMux(t, newForkAuthHandler(t, runs, allow, true))

		rr, body := serveFork(t, mux, http.MethodPost, APIPath+"/job/job-denied/run", "", true)
		requireForbidden(t, rr, body, "job of another project")

		if len(runs.triggered) != 0 {
			t.Fatalf("triggered = %v, want none", runs.triggered)
		}
	})

	t.Run("a name matching two projects needs both", func(t *testing.T) {
		runs := &forkFakeRuns{jobs: []controlplane.ScheduledJobInfo{
			{JobInfo: scheduler.JobInfo{Name: "backup", Context: "default", Stack: "allowed"}},
			{JobInfo: scheduler.JobInfo{Name: "backup", Context: "default", Stack: "denied"}},
		}}
		mux := newForkMux(t, newForkAuthHandler(t, runs, allow, true))

		rr, body := serveFork(t, mux, http.MethodPost, APIPath+"/job/backup/run", "", true)
		requireForbidden(t, rr, body, "ambiguous job name")
	})

	t.Run("unknown job", func(t *testing.T) {
		runs := &forkFakeRuns{jobs: newForkScheduledJobs()}
		mux := newForkMux(t, newForkAuthHandler(t, runs, allow, true))

		rr, body := serveFork(t, mux, http.MethodPost, APIPath+"/job/missing/run", "", true)
		if rr.Code != http.StatusNotFound || body["error"] != scheduler.ErrScheduledJobNotFound.Error() || len(runs.triggered) != 0 {
			t.Fatalf("status = %d, body = %v, triggered = %v; want 404 without a trigger", rr.Code, body, runs.triggered)
		}
	})

	t.Run("job lookup failure", func(t *testing.T) {
		runs := &forkFakeRuns{listJobsErr: errors.New("boom")}
		mux := newForkMux(t, newForkAuthHandler(t, runs, allow, true))

		rr, body := serveFork(t, mux, http.MethodPost, APIPath+"/job/job-allowed/run", "", true)
		if rr.Code != http.StatusInternalServerError || body["error"] != "failed to list scheduled jobs" || len(runs.triggered) != 0 {
			t.Fatalf("status = %d, body = %v, triggered = %v; want 500 without a trigger", rr.Code, body, runs.triggered)
		}
	})
}

func TestForkAuthPollRun(t *testing.T) {
	startForkFakeDocker(t)

	allow := newForkEnforcer(t, forkAllowPolicy)
	path := APIPath + "/poll/run"

	t.Run("operator target runs with the operator entry and the principal", func(t *testing.T) {
		runs := &forkFakeRuns{}
		mux := newForkMux(t, newForkAuthHandler(t, runs, allow, true))

		body := `[{"url": "` + validPollSourceURL + `", "reference": "main", "interval": "1s"}]`

		rr, decoded := serveFork(t, mux, http.MethodPost, path, body, true)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %v", rr.Code, decoded)
		}

		if len(runs.pollConfigs) != 1 || runs.pollConfigs[0].Reference != "refs/heads/main" || runs.pollConfigs[0].Interval != 5*time.Minute {
			t.Fatalf("poll configs = %+v, want the operator entry", runs.pollConfigs)
		}

		p, ok := rbac.PrincipalFrom(runs.pollCtx)
		if !ok || p.Subject != rbac.APIKeySubject {
			t.Fatalf("principal = %+v, %v; want the API key principal in the run context", p, ok)
		}
	})

	for name, body := range map[string]string{
		"not a configured target": `[{"url": "https://github.com/acme/other.git", "reference": "main"}]`,
		"inline deployments":      `[{"url": "` + validPollSourceURL + `", "reference": "main", "deployments": [{"name": "evil"}]}]`,
	} {
		t.Run(name, func(t *testing.T) {
			runs := &forkFakeRuns{}
			mux := newForkMux(t, newForkAuthHandler(t, runs, allow, true))

			rr, decoded := serveFork(t, mux, http.MethodPost, path, body, true)
			if rr.Code != http.StatusBadRequest || decoded["error"] != "poll configuration not allowed" || runs.pollConfigs != nil {
				t.Fatalf("status = %d, body = %v, triggered = %+v; want 400 without a trigger", rr.Code, decoded, runs.pollConfigs)
			}
		})
	}

	t.Run("without RBAC the body runs unchanged and without a principal", func(t *testing.T) {
		runs := &forkFakeRuns{}
		mux := newForkMux(t, newForkAuthHandler(t, runs, nil, false))

		body := `[{"url": "https://github.com/acme/other.git", "reference": "dev"}]`

		rr, decoded := serveFork(t, mux, http.MethodPost, path, body, true)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %v", rr.Code, decoded)
		}

		if len(runs.pollConfigs) != 1 || runs.pollConfigs[0].SourceUrl != "https://github.com/acme/other.git" || runs.pollConfigs[0].Reference != "dev" {
			t.Fatalf("poll configs = %+v, want the body unchanged", runs.pollConfigs)
		}

		if _, ok := rbac.PrincipalFrom(runs.pollCtx); ok {
			t.Fatal("the run context carries a principal without RBAC")
		}
	})
}
