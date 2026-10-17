package stages

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/moby/moby/api/types/container"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/docker"
	gitInternal "github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/hooks"
	"github.com/kimdre/doco-cd/internal/rollback"
	"github.com/kimdre/doco-cd/internal/secretprovider"
	secrettypes "github.com/kimdre/doco-cd/internal/secretprovider/types"
	"github.com/kimdre/doco-cd/internal/source/store"
)

// The tests in this file replace process-wide state (deployLastGood, the pre_rollback client,
// the compose policy, the in-memory failure map), so none of them calls t.Parallel.

// rollbackGoodFiles is a last good commit that needs what a copy of the compose file alone would
// not find: an include, a service env_file and a dotenv file of the working directory.
var rollbackGoodFiles = map[string]string{
	"deploy/compose.yaml": "include:\n  - extra.yaml\nservices:\n  app:\n    image: alpine:3.22\n" +
		"    build:\n      context: .\n    env_file: app.env\n    environment:\n      FROM_DOTENV: ${DOTENV_VALUE}\n",
	"deploy/extra.yaml": "services:\n  extra:\n    image: busybox:1\n",
	"deploy/app.env":    "GREETING=from-c1\n",
	"deploy/.env":       "DOTENV_VALUE=dotenv-c1\n",
}

// rollbackRepo is a source repository and the GitStore mirror cloned from it.
type rollbackRepo struct {
	dir, url string
	repo     *gogit.Repository
	commits  map[string]string
}

func (r *rollbackRepo) commit(t *testing.T, name string, files map[string]string) string {
	t.Helper()

	if err := os.RemoveAll(filepath.Join(r.dir, "deploy")); err != nil {
		t.Fatal(err)
	}

	for rel, content := range files {
		path := filepath.Join(r.dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	wt, err := r.repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}

	if err = wt.AddWithOptions(&gogit.AddOptions{All: true}); err != nil {
		t.Fatal(err)
	}

	hash, err := wt.Commit(name, &gogit.CommitOptions{Author: &object.Signature{Name: "t", Email: "t@example.com", When: time.Now()}})
	if err != nil {
		t.Fatal(err)
	}

	r.commits[name] = hash.String()

	return hash.String()
}

// newRollbackRepo commits good, empty (no compose file), broken (invalid compose) and
// privileged, in this order, on the default branch.
func newRollbackRepo(t *testing.T) *rollbackRepo {
	t.Helper()

	dir := t.TempDir()

	repo, err := gogit.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}

	// The first commit lands on main, the reference the mirror is cloned for.
	if err = repo.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, plumbing.NewBranchReferenceName("main"))); err != nil {
		t.Fatal(err)
	}

	r := &rollbackRepo{dir: dir, url: "file://" + dir, repo: repo, commits: map[string]string{}}
	r.commit(t, "good", rollbackGoodFiles)
	r.commit(t, "empty", map[string]string{"deploy/README": "nothing here\n"})
	r.commit(t, "broken", map[string]string{"deploy/compose.yaml": "services: [\n"})
	r.commit(t, "privileged", map[string]string{"deploy/compose.yaml": "services:\n  app:\n    image: alpine:3.22\n    privileged: true\n"})

	return r
}

// newRollbackManager returns a stage manager of default/web, after the init stage, whose store
// mirror holds every commit of r.
func newRollbackManager(t *testing.T, r *rollbackRepo, apiClient *lastGoodClient) *StageManager {
	t.Helper()

	sm := newTestStageManager(t)
	data := t.TempDir()
	sm.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	sm.AppConfig.DataMountPath = data
	sm.DeployConfig = deploy.New("web", "main")
	sm.DeployConfig.Rollback = &deploy.RollbackConfig{Enabled: true}
	sm.Repository = &RepositoryData{
		Source: config.SourceTypeGit, Name: "org/" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "-")),
		SourceUrl: r.url, ConfigSourceUrl: r.url, Revision: "failed-commit",
	}
	sm.Docker = &Docker{
		Cmd:            destroyOwnershipTestCli{apiClient: apiClient},
		DataMountPoint: container.MountPoint{Source: data, Destination: data},
	}

	gitStore, err := sm.newGitStore(sm.Log, filepath.Join(data, "repo"))
	if err != nil {
		t.Fatal(err)
	}

	if _, err = gitStore.Resolve(t.Context(), gitInternal.MainBranch); err != nil {
		t.Fatalf("clone mirror: %v", err)
	}

	sm.Repository.MirrorDir = gitStore.MirrorDir()

	t.Cleanup(func() { docker.ClearDeploymentFailure(sm.Repository.Name, "web") })

	return sm
}

func rollbackLastGood(r *rollbackRepo, commit, cfg string) rollback.LastGood {
	return rollback.LastGood{
		Object: "default/web", CommitSHA: r.commits[commit], SourceURL: r.url, DeployConfig: []byte(cfg),
		Images: []rollback.ServiceImage{
			{Service: "app", Image: "alpine:3.22", ImageID: "sha256:app", RepoDigests: []string{"alpine@sha256:a"}},
			{Service: "extra", Image: "busybox:1", ImageID: "sha256:extra", RepoDigests: []string{"busybox@sha256:b"}},
		},
	}
}

const rollbackConfig = "name: web\nworking_dir: deploy\n"

// stubDeployLastGood replaces docker.DeployStack for the test and records the request.
func stubDeployLastGood(t *testing.T, err error) *[]docker.DeployRequest {
	t.Helper()

	var got []docker.DeployRequest

	deployLastGood = func(_ context.Context, req docker.DeployRequest) error {
		got = append(got, req)
		return err
	}

	t.Cleanup(func() { deployLastGood = docker.DeployStack })

	return &got
}

type staticSecretProvider struct {
	secretprovider.SecretProvider

	values secrettypes.ResolvedSecrets
	err    error
}

func (p *staticSecretProvider) ResolveSecretReferences(context.Context, map[string]string) (secrettypes.ResolvedSecrets, error) {
	return p.values, p.err
}

func TestRollbackExecutorDeploysLastGood(t *testing.T) {
	setForkComposePolicy(t, forkPolicyFor("default/web"))

	r := newRollbackRepo(t)
	sm := newRollbackManager(t, r, &lastGoodClient{})
	sm.Repository.ConfigSourceUrl = forkPolicyRepo
	sm.Repository.ConfigRevision = "failed-commit"
	sm.SecretProvider = &staticSecretProvider{values: secrettypes.ResolvedSecrets{"TOKEN": "s3cr3t"}}

	got := stubDeployLastGood(t, nil)
	lg := rollbackLastGood(r, "good", rollbackConfig+"force_image_pull: true\nexternal_secrets:\n  TOKEN: secret-id\n")

	if err := (stageExecutor{s: sm, log: sm.Log}).Deploy(t.Context(), lg); err != nil {
		t.Fatalf("Deploy() error = %v", err)
	}

	if len(*got) != 1 {
		t.Fatalf("DeployStack called %d times, want 1", len(*got))
	}

	req := (*got)[0]
	wantArtifact := filepath.Join(sm.AppConfig.DataMountPath, "repo", store.ArtifactsSubdir)

	if !strings.HasPrefix(req.InternalRepoPath, wantArtifact) || req.ExternalRepoPath != req.InternalRepoPath {
		t.Fatalf("artifact paths %q / %q, want under %q", req.InternalRepoPath, req.ExternalRepoPath, wantArtifact)
	}

	if req.LatestCommit != r.commits["good"] || len(req.DetectedChanges) != 1 || req.DetectedChanges[0].Type != docker.ChangeTypeFailedDeployRetry {
		t.Fatalf("commit %q changes %+v", req.LatestCommit, req.DetectedChanges)
	}

	cfg := req.DeployConfig
	if cfg.ForceImagePull || cfg.Internal.Hash == "" || cfg.Internal.ConfigSourceRevision != r.commits["good"] ||
		cfg.Internal.ConfigSourceWorkingDir != req.ExternalRepoPath {
		t.Fatalf("deploy config force_image_pull=%v hash=%q config revision=%q dir=%q",
			cfg.ForceImagePull, cfg.Internal.Hash, cfg.Internal.ConfigSourceRevision, cfg.Internal.ConfigSourceWorkingDir)
	}

	if cfg.Internal.Environment["TOKEN"] != "s3cr3t" || cfg.Internal.Environment["DOTENV_VALUE"] != "dotenv-c1" {
		t.Fatalf("environment %v", cfg.Internal.Environment)
	}

	app, extra := req.Project.Services["app"], req.Project.Services["extra"]
	if app.Image != "sha256:app" || app.PullPolicy != "never" || app.Build != nil {
		t.Fatalf("app %+v", app)
	}

	if extra.Image != "sha256:extra" {
		t.Fatalf("extra (from the include) %+v", extra)
	}

	if v := app.Environment["GREETING"]; v == nil || *v != "from-c1" {
		t.Fatalf("GREETING from the service env_file = %v", v)
	}

	if v := app.Environment["FROM_DOTENV"]; v == nil || *v != "dotenv-c1" {
		t.Fatalf("FROM_DOTENV from the dotenv file = %v", v)
	}
}

func TestRollbackExecutorRegistryDigestWhenImageIsGone(t *testing.T) {
	r := newRollbackRepo(t)
	sm := newRollbackManager(t, r, &lastGoodClient{inspectErr: errors.New("no such image")})
	got := stubDeployLastGood(t, nil)

	if err := (stageExecutor{s: sm, log: sm.Log}).Deploy(t.Context(), rollbackLastGood(r, "good", rollbackConfig)); err != nil {
		t.Fatal(err)
	}

	req := (*got)[0]
	if img := req.Project.Services["app"].Image; img != "alpine@sha256:a" {
		t.Fatalf("app image %q, want the registry digest", img)
	}

	if req.DeployConfig.Internal.ConfigSourceRevision != "" {
		t.Fatal("without a config revision the label must stay empty")
	}
}

func TestRollbackExecutorDeployError(t *testing.T) {
	r := newRollbackRepo(t)
	sm := newRollbackManager(t, r, &lastGoodClient{})
	boom := errors.New("compose up failed")
	stubDeployLastGood(t, boom)

	if err := (stageExecutor{s: sm, log: sm.Log}).Deploy(t.Context(), rollbackLastGood(r, "good", rollbackConfig)); !errors.Is(err, boom) {
		t.Fatalf("Deploy() error = %v, want %v", err, boom)
	}
}

// A last good commit missing from the mirror is asked to the remote; one gone from both is an error.
func TestRollbackExecutorCommitNotInMirror(t *testing.T) {
	r := newRollbackRepo(t)
	sm := newRollbackManager(t, r, &lastGoodClient{})
	got := stubDeployLastGood(t, nil)

	late := r.commit(t, "late", rollbackGoodFiles)

	if err := (stageExecutor{s: sm, log: sm.Log}).Deploy(t.Context(), rollbackLastGood(r, "late", rollbackConfig)); err != nil {
		t.Fatalf("commit on the remote only: %v", err)
	}

	if len(*got) != 1 || (*got)[0].LatestCommit != late {
		t.Fatalf("requests %+v", *got)
	}

	lg := rollbackLastGood(r, "good", rollbackConfig)
	lg.CommitSHA = "0123456789abcdef0123456789abcdef01234567"

	err := (stageExecutor{s: sm, log: sm.Log}).Deploy(t.Context(), lg)
	if err == nil || !strings.Contains(err.Error(), "publish last good commit "+lg.CommitSHA) {
		t.Fatalf("missing commit: err = %v", err)
	}
}

func TestRollbackExecutorRefusals(t *testing.T) {
	r := newRollbackRepo(t)

	cases := map[string]struct {
		setup func(sm *StageManager, lg *rollback.LastGood)
		want  string
	}{
		"swarm": {func(sm *StageManager, _ *rollback.LastGood) { sm.Docker.SwarmMode = true }, "swarm mode"},
		"oci":   {func(sm *StageManager, _ *rollback.LastGood) { sm.Repository.Source = config.SourceTypeOCI }, "OCI source"},
		"repository_url": {func(sm *StageManager, _ *rollback.LastGood) {
			sm.DeployConfig.RepositoryUrl = "https://example.com/x.git"
		}, "repository_url"},
		"other repo": {func(_ *StageManager, lg *rollback.LastGood) { lg.SourceURL = "https://example.com/other.git" }, "another repository"},
		"saved repo url": {func(_ *StageManager, lg *rollback.LastGood) {
			lg.DeployConfig = []byte(rollbackConfig + "repository_url: https://example.com/x.git\n")
		}, "repository_url"},
		"bad config":     {func(_ *StageManager, lg *rollback.LastGood) { lg.DeployConfig = []byte("name: [\n") }, "decode last good deploy config"},
		"no mirror":      {func(sm *StageManager, _ *rollback.LastGood) { sm.Repository.MirrorDir = "" }, "unexpected mirror layout"},
		"odd mirror":     {func(sm *StageManager, _ *rollback.LastGood) { sm.Repository.MirrorDir = "/data/repo/elsewhere" }, "unexpected mirror layout"},
		"no store":       {func(sm *StageManager, lg *rollback.LastGood) { sm.Repository.SourceUrl, lg.SourceURL = "", "" }, "initialize git store"},
		"relative mount": {func(sm *StageManager, _ *rollback.LastGood) { sm.Docker.DataMountPoint.Destination = "" }, "compute external path"},
		"dotdot": {func(_ *StageManager, lg *rollback.LastGood) {
			lg.DeployConfig = []byte("name: web\nworking_dir: ../x\n")
		}, "invalid working directory"},
		"no compose file": {func(_ *StageManager, lg *rollback.LastGood) { lg.CommitSHA = r.commits["empty"] }, "no compose files found"},
		"invalid compose": {func(_ *StageManager, lg *rollback.LastGood) { lg.CommitSHA = r.commits["broken"] }, "load last good compose project"},
		"unrecorded":      {func(_ *StageManager, lg *rollback.LastGood) { lg.Images = lg.Images[:1] }, "service extra has no recorded image"},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			sm := newRollbackManager(t, r, &lastGoodClient{})
			got := stubDeployLastGood(t, nil)
			lg := rollbackLastGood(r, "good", rollbackConfig)
			tc.setup(sm, &lg)

			// As rollback.Run does: Supports, then Deploy.
			ex := stageExecutor{s: sm, log: sm.Log}

			err := ex.Supports(lg)
			if err == nil {
				err = ex.Deploy(t.Context(), lg)
			}

			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Supports()/Deploy() error = %v, want it to mention %q", err, tc.want)
			}

			if len(*got) != 0 {
				t.Fatal("a refused rollback reached DeployStack")
			}
		})
	}
}

// Deploy decodes the saved config on its own too: a caller that skipped Supports still gets the error.
func TestRollbackExecutorDeployBadConfig(t *testing.T) {
	r := newRollbackRepo(t)
	sm := newRollbackManager(t, r, &lastGoodClient{})
	stubDeployLastGood(t, nil)

	lg := rollbackLastGood(r, "good", "name: [\n")
	if err := (stageExecutor{s: sm, log: sm.Log}).Deploy(t.Context(), lg); err == nil || !strings.Contains(err.Error(), "decode last good deploy config") {
		t.Fatalf("Deploy() error = %v", err)
	}
}

// The compose policy in force applies to the last good: a violation is rollback_failed.
func TestRollbackExecutorPolicyDenied(t *testing.T) {
	setForkComposePolicy(t, forkPolicyFor("default/web"))

	r := newRollbackRepo(t)
	sm := newRollbackManager(t, r, &lastGoodClient{})
	sm.Repository.ConfigSourceUrl = forkPolicyRepo
	got := stubDeployLastGood(t, nil)

	lg := rollbackLastGood(r, "privileged", rollbackConfig)
	lg.Images = lg.Images[:1]

	err := (stageExecutor{s: sm, log: sm.Log}).Deploy(t.Context(), lg)
	if !errors.Is(err, ErrPolicyDenied) || !strings.Contains(err.Error(), "privileged") || len(*got) != 0 {
		t.Fatalf("Deploy() error = %v, deploys %d", err, len(*got))
	}
}

func TestLoadLastGoodFilesErrors(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "bad.yaml"), []byte("x: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cases := map[string]struct {
		cfg      *deploy.Config
		provider secretprovider.SecretProvider
		interp   bool
		want     string
	}{
		"env file":     {cfg: &deploy.Config{EnvFiles: []string{"missing.env"}}, want: "env files of last good"},
		"secrets file": {cfg: &deploy.Config{ExternalSecretsFiles: []string{"bad.yaml"}}, want: "external secrets files of last good"},
		"encode": {
			cfg:      &deploy.Config{ExternalSecrets: map[string]secrettypes.ExternalSecretRef{"X": {}}},
			provider: &staticSecretProvider{}, want: "encode external secret references",
		},
		"resolve": {
			cfg:      &deploy.Config{ExternalSecrets: map[string]secrettypes.ExternalSecretRef{"X": {LegacyRef: "id"}}},
			provider: &staticSecretProvider{err: errors.New("vault down")}, want: "resolve external secrets",
		},
		"interpolate": {
			cfg:      &deploy.Config{ExternalSecrets: map[string]secrettypes.ExternalSecretRef{"A": {LegacyRef: "a"}, "B": {LegacyRef: "b"}}},
			provider: &staticSecretProvider{values: secrettypes.ResolvedSecrets{"A": "${B}", "B": "${A}"}},
			interp:   true, want: "interpolate resolved secrets",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			sm := newTestStageManager(t)
			sm.SecretProvider = tc.provider
			sm.AppConfig.InterpolateResolvedSecrets = tc.interp

			err := sm.loadLastGoodFiles(t.Context(), tc.cfg, dir)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("loadLastGoodFiles() error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// The saved references are final: they win over the files read again and are not interpolated
// a second time.
func TestLoadLastGoodFilesKeepsSavedReferences(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "refs.yaml"), []byte("TOKEN: ${RAW}\nOTHER: other-id\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	sm := newTestStageManager(t)
	sm.AppConfig.InterpolateExternalSecrets = true

	cfg := &deploy.Config{
		ExternalSecretsFiles: []string{"refs.yaml"},
		ExternalSecrets:      map[string]secrettypes.ExternalSecretRef{"TOKEN": {LegacyRef: "lit$eral"}},
	}

	if err := sm.loadLastGoodFiles(t.Context(), cfg, dir); err != nil {
		t.Fatal(err)
	}

	if got := cfg.ExternalSecrets["TOKEN"].LegacyRef; got != "lit$eral" {
		t.Fatalf("TOKEN = %q, want the saved reference", got)
	}

	if got := cfg.ExternalSecrets["OTHER"].LegacyRef; got != "other-id" {
		t.Fatalf("OTHER = %q", got)
	}
}

func TestSameRepository(t *testing.T) {
	cases := []struct {
		recorded, current string
		want              bool
	}{
		{"https://github.com/o/r.git", "https://user:tok@github.com/o/r.git", true},
		{"https://github.com/o/r", "https://github.com/o/r.git", true},
		{"github.com:o/r.git", "git@github.com:o/r.git", true},
		{"https://github.com/o/r.git", "https://github.com/o/other.git", false},
	}

	for _, tc := range cases {
		if got := sameRepository(tc.recorded, tc.current); got != tc.want {
			t.Errorf("sameRepository(%q, %q) = %v, want %v", tc.recorded, tc.current, got, tc.want)
		}
	}
}

// startPreRollback registers a pre_rollback client whose hook answers decision and returns
// the number of requests it received.
func startPreRollback(t *testing.T, decision string) *int {
	t.Helper()

	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		_, _ = io.WriteString(w, `{"decision":"`+decision+`"}`)
	}))
	t.Cleanup(server.Close)

	SetPreRollback(&hooks.PreRollbackClient{URL: server.URL, Signer: hooks.Signer{Secret: []byte("s")}})
	t.Cleanup(func() { SetPreRollback(nil) })

	return &calls
}

func recordMemoryFailure(sm *StageManager, stage StageName) {
	docker.RecordDeploymentFailure(sm.Repository.Name, sm.DeployConfig.Name, docker.DeploymentFailure{
		Repository: sm.Repository.Name, Stack: sm.DeployConfig.Name, CommitSHA: "failed-commit",
		Stage: string(stage), Error: strings.Repeat("e", 2100), FailedAt: time.Unix(40, 0).UTC(),
	})
}

func diskFailure(t *testing.T, sm *StageManager) rollback.Failure {
	t.Helper()

	f, ok, err := rollback.StoreFor(sm.AppConfig.DataMountPath).GetFailure("default/web")
	if err != nil || !ok {
		t.Fatalf("failure on disk: ok=%v err=%v", ok, err)
	}

	return f
}

func captureLog(sm *StageManager) *bytes.Buffer {
	var buf bytes.Buffer

	sm.Log = slog.New(slog.NewTextHandler(&buf, nil))

	return &buf
}

func TestRunForkRollbackRollsBack(t *testing.T) {
	r := newRollbackRepo(t)
	sm := newRollbackManager(t, r, &lastGoodClient{})
	calls := startPreRollback(t, "proceed")
	got := stubDeployLastGood(t, nil)
	logs := captureLog(sm)

	if err := rollback.StoreFor(sm.AppConfig.DataMountPath).PutLastGood(rollbackLastGood(r, "good", rollbackConfig)); err != nil {
		t.Fatal(err)
	}

	recordMemoryFailure(sm, StageDeploy)
	sm.runForkRollback(t.Context(), StageDeploy)

	if *calls != 1 || len(*got) != 1 {
		t.Fatalf("pre_rollback calls %d, deploys %d", *calls, len(*got))
	}

	f := diskFailure(t, sm)
	if f.State != rollback.StateRolledBack || f.Decision != "proceed" || f.CommitSHA != "failed-commit" || len([]rune(f.Error)) != 2000 {
		t.Fatalf("failure %+v", f)
	}

	if !strings.Contains(logs.String(), "to_commit="+r.commits["good"]) {
		t.Fatalf("log %q must name the commit rolled back to", logs.String())
	}
}

func TestRunForkRollbackOutcomes(t *testing.T) {
	r := newRollbackRepo(t)

	t.Run("executor error is rollback_failed", func(t *testing.T) {
		sm := newRollbackManager(t, r, &lastGoodClient{})
		startPreRollback(t, "proceed")
		stubDeployLastGood(t, errors.New("unhealthy"))

		logs := captureLog(sm)

		if err := rollback.StoreFor(sm.AppConfig.DataMountPath).PutLastGood(rollbackLastGood(r, "good", rollbackConfig)); err != nil {
			t.Fatal(err)
		}

		recordMemoryFailure(sm, StagePostDeploy)
		sm.runForkRollback(t.Context(), StagePostDeploy)

		if f := diskFailure(t, sm); f.State != rollback.StateRollbackFailed {
			t.Fatalf("failure %+v", f)
		}

		if !strings.Contains(logs.String(), "unhealthy") {
			t.Fatalf("log %q must carry the rollback error", logs.String())
		}
	})

	t.Run("without HOOK_PRE_ROLLBACK_URL every failure is blocked", func(t *testing.T) {
		sm := newRollbackManager(t, r, &lastGoodClient{})
		got := stubDeployLastGood(t, nil)

		if err := rollback.StoreFor(sm.AppConfig.DataMountPath).PutLastGood(rollbackLastGood(r, "good", rollbackConfig)); err != nil {
			t.Fatal(err)
		}

		recordMemoryFailure(sm, StageCleanup)
		sm.runForkRollback(t.Context(), StageCleanup)

		if f := diskFailure(t, sm); f.State != rollback.StateBlocked || f.Decision != "unconfigured" || len(*got) != 0 {
			t.Fatalf("failure %+v, deploys %d", f, len(*got))
		}
	})

	t.Run("no last good stays failed", func(t *testing.T) {
		sm := newRollbackManager(t, r, &lastGoodClient{})
		calls := startPreRollback(t, "proceed")

		recordMemoryFailure(sm, StageDeploy)
		sm.runForkRollback(t.Context(), StageDeploy)

		if f := diskFailure(t, sm); f.State != rollback.StateFailed || *calls != 0 {
			t.Fatalf("failure %+v, pre_rollback calls %d", f, *calls)
		}
	})

	t.Run("persist error is logged", func(t *testing.T) {
		sm := newRollbackManager(t, r, &lastGoodClient{})
		logs := captureLog(sm)

		if err := os.MkdirAll(filepath.Join(sm.AppConfig.DataMountPath, "fork"), 0o700); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(filepath.Join(sm.AppConfig.DataMountPath, "fork", "failures"), nil, 0o600); err != nil {
			t.Fatal(err)
		}

		recordMemoryFailure(sm, StageDeploy)
		sm.runForkRollback(t.Context(), StageDeploy)

		if !strings.Contains(logs.String(), "failed to persist rollback state") {
			t.Fatalf("log %q", logs.String())
		}
	})
}

// Destroy, init and pre-deploy failures, a target without rollback and a failure missing from
// memory never start the rollback flow.
func TestRunForkRollbackGuards(t *testing.T) {
	r := newRollbackRepo(t)

	cases := map[string]func(sm *StageManager) StageName{
		"destroy":    func(sm *StageManager) StageName { sm.DeployConfig.Destroy.Enabled = true; return StageDeploy },
		"pre-deploy": func(*StageManager) StageName { return StagePreDeploy },
		"disabled":   func(sm *StageManager) StageName { sm.DeployConfig.Rollback = nil; return StageDeploy },
		"no memory": func(sm *StageManager) StageName {
			docker.ClearDeploymentFailure(sm.Repository.Name, "web")
			return StageDeploy
		},
	}

	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			sm := newRollbackManager(t, r, &lastGoodClient{})
			calls := startPreRollback(t, "proceed")

			if err := rollback.StoreFor(sm.AppConfig.DataMountPath).PutLastGood(rollbackLastGood(r, "good", rollbackConfig)); err != nil {
				t.Fatal(err)
			}

			recordMemoryFailure(sm, StageDeploy)
			sm.runForkRollback(t.Context(), setup(sm))

			if *calls != 0 {
				t.Fatal("the rollback flow ran")
			}

			if _, ok, _ := rollback.StoreFor(sm.AppConfig.DataMountPath).GetFailure("default/web"); ok {
				t.Fatal("a failure was written")
			}
		})
	}
}

// handleStageFailure runs the rollback after recording the failure: an unreadable last good
// leaves the failure blocked/invalid.
func TestHandleStageFailureRunsRollback(t *testing.T) {
	sm, data := newForkFailureManager(t, &deploy.RollbackConfig{Enabled: true})
	path := filepath.Join(data, "fork", "lastgood", "default", "web.json")

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}

	_ = sm.handleStageFailure(t.Context(), StageDeploy, sm.Log, errors.New("boom"))

	if f := diskFailure(t, sm); f.State != rollback.StateBlocked || f.Decision != "invalid" || f.CommitSHA != "c1" {
		t.Fatalf("failure %+v", f)
	}
}

func putDiskFailure(t *testing.T, sm *StageManager, f rollback.Failure) {
	t.Helper()

	if err := rollback.StoreFor(sm.AppConfig.DataMountPath).PutFailure(f); err != nil {
		t.Fatal(err)
	}
}

func TestRollbackPoisoned(t *testing.T) {
	stageLog := slog.New(slog.NewTextHandler(io.Discard, nil))

	t.Run("without rollback", func(t *testing.T) {
		sm, _ := newForkFailureManager(t, nil)
		if sm.rollbackPoisoned(t.Context(), "c1", stageLog) {
			t.Fatal("poisoned without rollback")
		}
	})

	t.Run("failed is retried, not poisoned", func(t *testing.T) {
		sm, _ := newForkFailureManager(t, &deploy.RollbackConfig{Enabled: true})
		putDiskFailure(t, sm, rollback.Failure{Object: "default/web", CommitSHA: "c1", State: rollback.StateFailed})

		if sm.rollbackPoisoned(t.Context(), "c1", stageLog) {
			t.Fatal("a failed commit must be retried")
		}
	})

	for _, state := range []rollback.State{rollback.StateRolledBack, rollback.StateBlocked, rollback.StateRollbackFailed} {
		t.Run(string(state)+" same commit", func(t *testing.T) {
			sm, _ := newForkFailureManager(t, &deploy.RollbackConfig{Enabled: true})
			putDiskFailure(t, sm, rollback.Failure{Object: "default/web", CommitSHA: " c1 ", State: state, Decision: "block"})

			if !sm.rollbackPoisoned(t.Context(), "c1", stageLog) {
				t.Fatal("the commit must not be redeployed")
			}
		})
	}

	t.Run("new commit resets to failed", func(t *testing.T) {
		sm, _ := newForkFailureManager(t, &deploy.RollbackConfig{Enabled: true})
		putDiskFailure(t, sm, rollback.Failure{Object: "default/web", CommitSHA: "c1", Stage: "deploy", State: rollback.StateBlocked, Decision: "block"})

		if sm.rollbackPoisoned(t.Context(), "c2", stageLog) {
			t.Fatal("a new commit must deploy")
		}

		if f := diskFailure(t, sm); f.State != rollback.StateFailed || f.Decision != "" || f.CommitSHA != "c1" {
			t.Fatalf("failure %+v", f)
		}
	})

	t.Run("reset error is logged", func(t *testing.T) {
		sm, data := newForkFailureManager(t, &deploy.RollbackConfig{Enabled: true})

		// A record whose object cannot be written back.
		path := filepath.Join(data, "fork", "failures", "default", "web.json")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(path, []byte(`{"version":1,"object":"bad","commit_sha":"c1","state":"blocked"}`), 0o600); err != nil {
			t.Fatal(err)
		}

		var buf bytes.Buffer
		if sm.rollbackPoisoned(t.Context(), "c2", slog.New(slog.NewTextHandler(&buf, nil))) || !strings.Contains(buf.String(), "failed to reset rollback state") {
			t.Fatalf("log %q", buf.String())
		}
	})
}

// After a restart (memory empty) the poison skip holds for OCI sources too and without any
// container of the project; a new revision is not skipped and resets the record to failed.
func TestRunPreDeployStagePoisonAfterRestart(t *testing.T) {
	stageLog := slog.New(slog.NewTextHandler(io.Discard, nil))

	newRun := func(t *testing.T, revision string) *StageManager {
		t.Helper()

		sm, _ := newForkFailureManager(t, &deploy.RollbackConfig{Enabled: true})
		sm.Stages = &Stages{PreDeploy: &PreDeployStageData{}}
		sm.Repository.Source = config.SourceTypeOCI
		sm.Repository.Revision = revision
		sm.Repository.PathInternal = t.TempDir()
		sm.Repository.PathExternal = sm.Repository.PathInternal
		sm.Docker.Cmd = destroyOwnershipTestCli{apiClient: &lastGoodClient{}}
		// Default compose files in an empty checkout: past the poison check, pre-deploy stops
		// at "no compose files found" before any other Docker call.
		sm.DeployConfig.ComposeFiles = deploy.New("web", "main").ComposeFiles
		putDiskFailure(t, sm, rollback.Failure{Object: "default/web", CommitSHA: "c1", Stage: "deploy", State: rollback.StateRolledBack, Decision: "proceed"})
		docker.ClearDeploymentFailure("org/repo", "web")

		return sm
	}

	t.Run("same commit", func(t *testing.T) {
		sm := newRun(t, "c1")

		err := sm.RunPreDeployStage(t.Context(), stageLog)

		var skip *SkipError
		if !errors.Is(err, ErrSkipDeployment) || !errors.As(err, &skip) || skip.Reason != SkipReasonRolledBack {
			t.Fatalf("RunPreDeployStage() error = %v, want the rolled back skip", err)
		}
	})

	t.Run("new commit", func(t *testing.T) {
		sm := newRun(t, "c2")

		err := sm.RunPreDeployStage(t.Context(), stageLog)

		if err == nil || !strings.Contains(err.Error(), "no compose files found") {
			t.Fatalf("RunPreDeployStage() error = %v, want pre-deploy to go past the poison check", err)
		}

		if f := diskFailure(t, sm); f.State != rollback.StateFailed || f.CommitSHA != "c1" {
			t.Fatalf("failure %+v", f)
		}
	})
}

func TestUnblockTarget(t *testing.T) {
	t.Run("blocked goes back to failed with its commit", func(t *testing.T) {
		data := t.TempDir()
		s := rollback.StoreFor(data)

		if err := s.PutFailure(rollback.Failure{Object: "default/web", CommitSHA: "c2", Stage: "deploy", State: rollback.StateBlocked, Decision: "block"}); err != nil {
			t.Fatal(err)
		}

		changed, err := UnblockTarget(t.Context(), data, "default/web")
		if err != nil || !changed {
			t.Fatalf("UnblockTarget() = %v, %v", changed, err)
		}

		f, _, _ := s.GetFailure("default/web")
		if f.State != rollback.StateFailed || f.Decision != "" || f.CommitSHA != "c2" || f.Stage != "deploy" {
			t.Fatalf("failure %+v", f)
		}
	})

	t.Run("nothing to unblock", func(t *testing.T) {
		data := t.TempDir()

		if changed, err := UnblockTarget(t.Context(), data, "default/web"); changed || err != nil {
			t.Fatalf("missing: %v, %v", changed, err)
		}

		if err := rollback.StoreFor(data).PutFailure(rollback.Failure{Object: "default/web", State: rollback.StateFailed}); err != nil {
			t.Fatal(err)
		}

		if changed, err := UnblockTarget(t.Context(), data, "default/web"); changed || err != nil {
			t.Fatalf("failed: %v, %v", changed, err)
		}
	})

	t.Run("unreadable record is replaced by a failed one", func(t *testing.T) {
		data := t.TempDir()
		path := filepath.Join(data, "fork", "failures", "default", "web.json")

		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}

		if changed, err := UnblockTarget(t.Context(), data, "default/web"); !changed || err != nil {
			t.Fatalf("UnblockTarget() = %v, %v", changed, err)
		}

		f, ok, err := rollback.StoreFor(data).GetFailure("default/web")
		if err != nil || !ok || f.State != rollback.StateFailed || f.Stage != "deploy" {
			t.Fatalf("failure %+v ok=%v err=%v", f, ok, err)
		}
	})

	// Only a corrupt record is replaced: an I/O error (here EISDIR) is returned, the API answers 500.
	t.Run("read error is returned", func(t *testing.T) {
		data := t.TempDir()
		if err := os.MkdirAll(filepath.Join(data, "fork", "failures", "default", "web.json"), 0o700); err != nil {
			t.Fatal(err)
		}

		changed, err := UnblockTarget(t.Context(), data, "default/web")
		if changed || err == nil || errors.Is(err, rollback.ErrCorrupt) {
			t.Fatalf("UnblockTarget() = %v, %v; want the read error", changed, err)
		}
	})

	t.Run("waits for the target lock", func(t *testing.T) {
		unlock, err := lockForkTarget(t.Context(), "default/unblock-locked")
		if err != nil {
			t.Fatal(err)
		}
		defer unlock()

		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		if changed, err := UnblockTarget(ctx, t.TempDir(), "default/unblock-locked"); changed || !errors.Is(err, context.Canceled) {
			t.Fatalf("UnblockTarget() = %v, %v; want context canceled while the target is locked", changed, err)
		}
	})

	t.Run("invalid object", func(t *testing.T) {
		if _, err := UnblockTarget(t.Context(), t.TempDir(), "no-slash"); err == nil {
			t.Fatal("want an error")
		}
	})
}

// An OCI target is rollback_failed without asking pre_rollback: there is nothing to veto.
func TestRunForkRollbackUnsupportedSkipsPreRollback(t *testing.T) {
	r := newRollbackRepo(t)
	sm := newRollbackManager(t, r, &lastGoodClient{})
	sm.Repository.Source = config.SourceTypeOCI
	calls := startPreRollback(t, "proceed")
	got := stubDeployLastGood(t, nil)

	if err := rollback.StoreFor(sm.AppConfig.DataMountPath).PutLastGood(rollbackLastGood(r, "good", rollbackConfig)); err != nil {
		t.Fatal(err)
	}

	recordMemoryFailure(sm, StageDeploy)
	sm.runForkRollback(t.Context(), StageDeploy)

	if f := diskFailure(t, sm); f.State != rollback.StateRollbackFailed || f.Decision != "" || *calls != 0 || len(*got) != 0 {
		t.Fatalf("failure %+v, pre_rollback calls %d, deploys %d", f, *calls, len(*got))
	}
}

func TestLockForkTarget(t *testing.T) {
	unlock, err := lockForkTarget(t.Context(), "default/lock-test")
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err = lockForkTarget(ctx, "default/lock-test"); !errors.Is(err, context.Canceled) {
		t.Fatalf("second lock = %v, want context canceled", err)
	}

	unlock()

	// A cancelled context fails even on a free lock, and leaves it free.
	if _, err = lockForkTarget(ctx, "default/lock-test"); !errors.Is(err, errForkTargetWait) || !errors.Is(err, context.Canceled) {
		t.Fatalf("lock with a cancelled context = %v", err)
	}

	again, err := lockForkTarget(t.Context(), "default/lock-test")
	if err != nil {
		t.Fatalf("lock after unlock: %v", err)
	}

	again()
}

// A job whose wait for the target lock is cancelled must not overwrite the sticky state the
// holder wrote: nothing is recorded for it, on disk or in memory.
func TestCanceledLockWaitKeepsHolderState(t *testing.T) {
	b, data := newForkFailureManager(t, &deploy.RollbackConfig{Enabled: true})
	b.Stages = &Stages{Deploy: &DeployStageData{}}
	b.Repository.Revision = "c2"

	putDiskFailure(t, b, rollback.Failure{Object: "default/web", CommitSHA: "c2", Stage: "deploy", State: rollback.StateBlocked, Decision: "block"})

	unlock, err := lockForkTarget(t.Context(), "default/web")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err = b.handleStageFailure(ctx, StageDeploy, b.Log, b.runForkDeployStage(ctx, b.Log))
	if !errors.Is(err, errForkTargetWait) {
		t.Fatalf("handleStageFailure() error = %v, want the lock wait error", err)
	}

	if f, _, _ := rollback.StoreFor(data).GetFailure("default/web"); f.State != rollback.StateBlocked || f.Decision != "block" {
		t.Fatalf("failure %+v, want the holder's blocked record", f)
	}

	if _, ok := docker.GetDeploymentFailure("org/repo", "web"); ok {
		t.Fatal("the cancelled waiter recorded a failure in memory")
	}
}

// A job that skips in the deploy stage closes its per-stack "In Progress" status as skipped.
func TestRunForkDeployStageSkipClosesInProgressStatus(t *testing.T) {
	var states []string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			var body struct {
				State       string `json:"state"`
				Description string `json:"description"`
			}

			_ = json.NewDecoder(r.Body).Decode(&body)
			states = append(states, body.State+"|"+body.Description)
		}

		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, "{}")
	}))
	t.Cleanup(server.Close)

	sm, _ := newForkFailureManager(t, &deploy.RollbackConfig{Enabled: true})
	sm.AppConfig.GitCommitStatus = true
	sm.AppConfig.GitScmProvider = "github"
	sm.AppConfig.GitScmApiUrl = config.HttpUrl(server.URL)
	sm.AppConfig.GitAccessToken = "test-token"
	// Not github.com: the provider then posts to GitScmApiUrl (GitHub Enterprise), the test server.
	sm.Repository.SourceUrl = "https://git.example.com/owner/repo"
	sm.Repository.Revision = "deadbeef"
	sm.inProgressPosted = true

	putDiskFailure(t, sm, rollback.Failure{Object: "default/web", CommitSHA: "deadbeef", Stage: "deploy", State: rollback.StateRolledBack})

	err := sm.runForkDeployStage(t.Context(), sm.Log)
	sm.releaseForkTarget()

	var skip *SkipError
	if !errors.As(err, &skip) || skip.Reason != SkipReasonRolledBack {
		t.Fatalf("runForkDeployStage() error = %v, want the rolled back skip", err)
	}

	if len(states) != 1 || !strings.HasPrefix(states[0], "success|Skipped: ") {
		t.Fatalf("posted statuses %q, want one terminal skipped status", states)
	}
}

func TestRunForkDeployStage(t *testing.T) {
	stageLog := slog.New(slog.NewTextHandler(io.Discard, nil))

	newDeploy := func(t *testing.T, rb *deploy.RollbackConfig) *StageManager {
		t.Helper()

		sm, _ := newForkFailureManager(t, rb)
		sm.Stages = &Stages{Deploy: &DeployStageData{}}
		sm.Repository.Source = config.SourceTypeOCI
		sm.Docker.Cmd = destroyOwnershipTestCli{apiClient: &lastGoodClient{}}

		return sm
	}

	t.Run("without rollback no lock is taken", func(t *testing.T) {
		sm := newDeploy(t, nil)

		// No artifact path: upstream DeployStack refuses the request, past the fork part.
		if err := sm.runForkDeployStage(t.Context(), stageLog); err == nil || errors.Is(err, ErrSkipDeployment) {
			t.Fatalf("runForkDeployStage() error = %v, want the upstream deploy error", err)
		}

		if sm.forkTargetUnlock != nil {
			t.Fatal("a target lock was taken without rollback")
		}
	})

	t.Run("with rollback the lock is held until released", func(t *testing.T) {
		sm := newDeploy(t, &deploy.RollbackConfig{Enabled: true})

		if err := sm.runForkDeployStage(t.Context(), stageLog); err == nil || errors.Is(err, ErrSkipDeployment) {
			t.Fatalf("runForkDeployStage() error = %v, want the upstream deploy error", err)
		}

		if sm.forkTargetUnlock == nil {
			t.Fatal("the target lock must be held after the stage")
		}

		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		if _, err := lockForkTarget(ctx, "default/web"); err == nil {
			t.Fatal("the target lock is free while the stage manager holds it")
		}

		sm.releaseForkTarget()
		sm.releaseForkTarget()

		unlock, err := lockForkTarget(t.Context(), "default/web")
		if err != nil {
			t.Fatal(err)
		}

		unlock()
	})

	t.Run("lock wait canceled", func(t *testing.T) {
		sm := newDeploy(t, &deploy.RollbackConfig{Enabled: true})

		unlock, err := lockForkTarget(t.Context(), "default/web")
		if err != nil {
			t.Fatal(err)
		}
		defer unlock()

		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		if err := sm.runForkDeployStage(ctx, stageLog); !errors.Is(err, context.Canceled) {
			t.Fatalf("runForkDeployStage() error = %v, want context canceled", err)
		}
	})
}

// Two jobs of the same object: A is inside runForkRollback (holding the target lock, as
// runForkDeployStage leaves it), B passed pre-deploy with the same failed commit and reaches its
// deploy stage. B waits for A, then skips the commit A rolled back instead of deploying it.
func TestConcurrentJobWaitsForRollbackAndSkipsPoisonedCommit(t *testing.T) {
	r := newRollbackRepo(t)
	a := newRollbackManager(t, r, &lastGoodClient{})
	startPreRollback(t, "proceed")

	entered, release := make(chan struct{}), make(chan struct{})
	deploys := 0

	deployLastGood = func(context.Context, docker.DeployRequest) error {
		deploys++

		close(entered)
		<-release

		return nil
	}

	t.Cleanup(func() { deployLastGood = docker.DeployStack })

	if err := rollback.StoreFor(a.AppConfig.DataMountPath).PutLastGood(rollbackLastGood(r, "good", rollbackConfig)); err != nil {
		t.Fatal(err)
	}

	unlock, err := lockForkTarget(t.Context(), "default/web")
	if err != nil {
		t.Fatal(err)
	}

	a.forkTargetUnlock = unlock

	recordMemoryFailure(a, StageDeploy)

	aDone := make(chan struct{})

	go func() {
		defer close(aDone)

		a.runForkRollback(t.Context(), StageDeploy)
		a.releaseForkTarget()
	}()

	<-entered

	b := newTestStageManager(t)
	b.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	b.AppConfig.DataMountPath = a.AppConfig.DataMountPath
	b.DeployConfig = deploy.New("web", "main")
	b.DeployConfig.Rollback = &deploy.RollbackConfig{Enabled: true}
	b.Repository = &RepositoryData{Source: config.SourceTypeGit, Name: a.Repository.Name, Revision: "failed-commit"}
	b.Stages = &Stages{Deploy: &DeployStageData{}}

	waiting := make(chan struct{})

	forkTargetWaiting = func(object string) {
		if object == "default/web" {
			close(waiting)
		}
	}

	t.Cleanup(func() { forkTargetWaiting = func(string) {} })

	bErr := make(chan error, 1)

	go func() { bErr <- b.runForkDeployStage(t.Context(), b.Log) }()

	// B is queued on the target lock while A is still inside its rollback.
	<-waiting
	close(release)
	<-aDone

	err = <-bErr

	b.releaseForkTarget()

	var skip *SkipError
	if !errors.As(err, &skip) || skip.Reason != SkipReasonRolledBack {
		t.Fatalf("job B error = %v, want the rolled back skip", err)
	}

	if deploys != 1 {
		t.Fatalf("deploys = %d, want only the rollback", deploys)
	}

	if f := diskFailure(t, a); f.State != rollback.StateRolledBack {
		t.Fatalf("failure %+v", f)
	}
}
