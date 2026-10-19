package stages

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/docker/compose/v5/pkg/api"
	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/image"
	"github.com/moby/moby/client"
	"go.yaml.in/yaml/v4"

	"github.com/kimdre/doco-cd/internal/common/types/duration"
	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/rollback"
)

// lastGoodClient serves the containers and images of a project. Any other call panics via
// the nil embedded interface, so no test here can reach a real Docker daemon.
type lastGoodClient struct {
	client.APIClient

	containers []container.Summary
	digests    map[string][]string
	refs       map[string]string // reference -> ID of the local image it names; any other ref is its own ID
	listErr    error
	inspectErr error
	options    client.ContainerListOptions
}

func (c *lastGoodClient) ContainerList(_ context.Context, options client.ContainerListOptions) (client.ContainerListResult, error) {
	c.options = options
	return client.ContainerListResult{Items: c.containers}, c.listErr
}

func (c *lastGoodClient) ImageInspect(_ context.Context, id string, _ ...client.ImageInspectOption) (client.ImageInspectResult, error) {
	if c.inspectErr != nil {
		return client.ImageInspectResult{}, c.inspectErr
	}

	if ref, ok := c.refs[id]; ok {
		id = ref
	}

	return client.ImageInspectResult{InspectResponse: image.InspectResponse{ID: id, RepoDigests: c.digests[id]}}, nil
}

func serviceContainer(name, service, img, id string, extra map[string]string) container.Summary {
	labels := map[string]string{api.ProjectLabel: "web", api.ServiceLabel: service}
	for k, v := range extra {
		labels[k] = v
	}

	return container.Summary{Names: []string{name}, Image: img, ImageID: id, Labels: labels}
}

func newLastGoodManager(t *testing.T, rb *deploy.RollbackConfig, apiClient *lastGoodClient) (*StageManager, string) {
	t.Helper()

	sm := newTestStageManager(t)
	data := t.TempDir()
	sm.AppConfig.DataMountPath = data
	sm.DeployConfig.Name = "web"
	sm.DeployConfig.Rollback = rb
	sm.DeployConfig.EnvFiles = nil
	sm.DeployConfig.Environment = map[string]string{"PUBLIC": "visible-value"}
	sm.DeployConfig.Internal.Environment = map[string]string{"SECRET": "s3cr3t", "PUBLIC": "visible-value"}
	sm.forkDeclared = declaredFiles{EnvFiles: []string{".env", "x.env"}}
	sm.Repository.Source = config.SourceTypeOCI
	sm.Repository.Revision = " sha256:abc "
	sm.Repository.SourceUrl = "https://user:tok3n@example.com/org/repo.git"
	sm.Docker.ProjectHash = "project-hash"
	sm.Docker.Cmd = destroyOwnershipTestCli{apiClient: apiClient}

	return sm, data
}

func assertNoForkDir(t *testing.T, data string) {
	t.Helper()

	if _, err := os.Stat(filepath.Join(data, "fork")); !os.IsNotExist(err) {
		t.Fatalf("expected no fork dir, got err=%v", err)
	}
}

func assertNoSecretUnder(t *testing.T, root string, secrets ...string) {
	t.Helper()

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}

		data, err := os.ReadFile(path) //nolint:gosec // test files under t.TempDir()
		if err != nil {
			return err
		}

		for _, secret := range secrets {
			if bytes.Contains(data, []byte(secret)) {
				t.Errorf("%s contains %q", path, secret)
			}
		}

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// initLastGoodRepo creates a git repository with two commits on its branch and returns the
// first one and the tip; go-git opens the repository as a mirror.
func initLastGoodRepo(t *testing.T) (dir, first, tip string) {
	t.Helper()

	dir = t.TempDir()

	repo, err := gogit.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}

	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}

	commits := make([]string, 0, 2)

	for _, content := range []string{"services: {}\n", "services: {app: {image: nginx}}\n"} {
		if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}

		if _, err := wt.Add("compose.yaml"); err != nil {
			t.Fatal(err)
		}

		hash, err := wt.Commit("commit", &gogit.CommitOptions{Author: &object.Signature{Name: "t", Email: "t@example.com", When: time.Now()}})
		if err != nil {
			t.Fatal(err)
		}

		commits = append(commits, hash.String())
	}

	return dir, commits[0], commits[1]
}

func lastGoodRefOf(t *testing.T, mirror string) string {
	t.Helper()

	repo, err := gogit.PlainOpen(mirror)
	if err != nil {
		t.Fatal(err)
	}

	ref, err := repo.Reference("refs/doco-cd/lastgood/default/web", false)
	if err != nil {
		return ""
	}

	return ref.Hash().String()
}

func TestRecordLastGoodWritesNothingWithoutRollback(t *testing.T) {
	t.Parallel()

	sm, data := newLastGoodManager(t, nil, &lastGoodClient{})
	sm.recordLastGood(t.Context())
	assertNoForkDir(t, data)

	sm, data = newLastGoodManager(t, &deploy.RollbackConfig{Enabled: false}, &lastGoodClient{})
	sm.recordLastGood(t.Context())
	assertNoForkDir(t, data)
}

func TestRecordLastGoodNeverOnDestroyOrSwarm(t *testing.T) {
	t.Parallel()

	sm, data := newLastGoodManager(t, &deploy.RollbackConfig{Enabled: true}, &lastGoodClient{})
	sm.DeployConfig.Destroy.Enabled = true

	sm.recordLastGood(t.Context())
	assertNoForkDir(t, data)

	sm, data = newLastGoodManager(t, &deploy.RollbackConfig{Enabled: true}, &lastGoodClient{})
	sm.Docker.SwarmMode = true

	sm.recordLastGood(t.Context())
	assertNoForkDir(t, data)
}

func TestRecordLastGoodOCI(t *testing.T) {
	t.Parallel()

	apiClient := &lastGoodClient{
		containers: []container.Summary{serviceContainer("/web-app-1", "app", "nginx:1", "sha256:1", nil)},
		digests:    map[string][]string{"sha256:1": {"nginx@sha256:d1"}},
	}
	sm, data := newLastGoodManager(t, &deploy.RollbackConfig{Enabled: true}, apiClient)

	sm.recordLastGood(t.Context())

	lg, ok, err := rollback.StoreFor(data).GetLastGood("default/web")
	if err != nil || !ok {
		t.Fatalf("last good not recorded: ok=%v err=%v", ok, err)
	}

	if lg.CommitSHA != "sha256:abc" || lg.ProjectHash != "project-hash" || lg.Object != "default/web" || lg.RecordedAt.IsZero() {
		t.Fatalf("unexpected last good: %+v", lg)
	}

	if lg.SourceURL != "https://example.com/org/repo.git" {
		t.Fatalf("SourceURL = %q, want the credentials stripped", lg.SourceURL)
	}

	if len(lg.Images) != 1 || lg.Images[0].ImageID != "sha256:1" || lg.Images[0].RepoDigests[0] != "nginx@sha256:d1" {
		t.Fatalf("unexpected images: %+v", lg.Images)
	}

	if !bytes.Contains(lg.DeployConfig, []byte("x.env")) || !bytes.Contains(lg.DeployConfig, []byte("visible-value")) {
		t.Fatalf("deploy config misses the declared env files or the public environment:\n%s", lg.DeployConfig)
	}

	assertNoSecretUnder(t, filepath.Join(data, "fork"), "s3cr3t", "tok3n")
}

// The recorded commit is the deployed one, never the mirror tip, which moved after the deploy.
func TestRecordLastGoodPinsDeployedCommitNotMirrorTip(t *testing.T) {
	t.Parallel()

	for name, stamped := range map[string]bool{"published revision": false, "commit stamped by the deploy stage": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			mirror, deployed, tip := initLastGoodRepo(t)

			sm, data := newLastGoodManager(t, &deploy.RollbackConfig{Enabled: true}, &lastGoodClient{})
			sm.Repository.Source = config.SourceTypeGit
			sm.Repository.MirrorDir = mirror
			sm.DeployState.latestCommit = ""

			sm.Repository.Revision = " " + deployed + " "
			if stamped {
				sm.Repository.Revision = tip
				sm.forkDeployedCommit = deployed
			}

			sm.recordLastGood(t.Context())

			lg, ok, err := rollback.StoreFor(data).GetLastGood("default/web")
			if err != nil || !ok || lg.CommitSHA != deployed {
				t.Fatalf("last good = %+v ok=%v err=%v, want commit %s (tip %s)", lg, ok, err, deployed, tip)
			}

			if lg.Images == nil || len(lg.Images) != 0 {
				t.Fatalf("images = %#v, want an empty list", lg.Images)
			}

			if got := lastGoodRefOf(t, mirror); got != deployed {
				t.Fatalf("last good ref = %q, want %s", got, deployed)
			}
		})
	}
}

// A failed pin is logged; the store already holds the last good, whose commit is still in the
// mirror.
func TestRecordLastGoodPinFailureKeepsRecord(t *testing.T) {
	t.Parallel()

	sm, data := newLastGoodManager(t, &deploy.RollbackConfig{Enabled: true}, &lastGoodClient{})
	sm.Repository.Source = config.SourceTypeGit
	sm.Repository.MirrorDir = filepath.Join(t.TempDir(), "missing")
	sm.forkDeployedCommit = strings.Repeat("ab", 20)

	sm.recordLastGood(t.Context())

	if lg, ok, err := rollback.StoreFor(data).GetLastGood("default/web"); err != nil || !ok || lg.CommitSHA != sm.forkDeployedCommit {
		t.Fatalf("last good = %+v ok=%v err=%v", lg, ok, err)
	}
}

// A failed store write leaves the mirror ref where it was: the JSON and the ref never disagree.
func TestRecordLastGoodStoreFailureDoesNotMoveRef(t *testing.T) {
	t.Parallel()

	mirror, deployed, _ := initLastGoodRepo(t)

	sm, data := newLastGoodManager(t, &deploy.RollbackConfig{Enabled: true}, &lastGoodClient{})
	sm.Repository.Source = config.SourceTypeGit
	sm.Repository.MirrorDir = mirror
	sm.forkDeployedCommit = deployed

	if err := os.WriteFile(filepath.Join(data, "fork"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	sm.recordLastGood(t.Context())

	if got := lastGoodRefOf(t, mirror); got != "" {
		t.Fatalf("last good ref = %q, want none", got)
	}
}

// Every failure is logged and leaves no last good behind; none of them panics or fails the deploy.
func TestRecordLastGoodFailuresAreNotFatal(t *testing.T) {
	t.Parallel()

	cases := map[string]func(t *testing.T, sm *StageManager, data string){
		"deployed commit": func(_ *testing.T, sm *StageManager, _ string) {
			sm.Repository.Revision = " "
			sm.forkDeployedCommit = ""
		},
		"images": func(_ *testing.T, sm *StageManager, _ string) {
			sm.Docker.Cmd = destroyOwnershipTestCli{apiClient: &lastGoodClient{listErr: errors.New("daemon down")}}
		},
		"deploy config": func(_ *testing.T, sm *StageManager, _ string) {
			sm.DeployConfig.Timeout = duration.Duration(1500 * time.Millisecond)
		},
		"store": func(t *testing.T, _ *StageManager, data string) {
			if err := os.WriteFile(filepath.Join(data, "fork"), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
	}

	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			sm, data := newLastGoodManager(t, &deploy.RollbackConfig{Enabled: true}, &lastGoodClient{})
			setup(t, sm, data)

			sm.recordLastGood(t.Context())

			if lg, ok, _ := rollback.StoreFor(data).GetLastGood("default/web"); ok {
				t.Fatalf("expected no last good, got %+v", lg)
			}
		})
	}
}

func TestDeployConfigForLastGood(t *testing.T) {
	t.Parallel()

	c := deploy.New("web", "main")
	c.RepositoryUrl = "https://x-access-token:tok3n@github.com/org/repo.git"
	c.EnvFiles = nil
	c.ExternalSecretsFiles = []string{"remote:only.yaml"}
	c.Environment = map[string]string{"PUBLIC": "visible-value"}
	c.Internal.Environment = map[string]string{"SECRET": "s3cr3t"}
	c.Internal.File = "/path/.doco-cd.yaml"

	data, err := deployConfigForLastGood(c, declaredFiles{EnvFiles: []string{".env", "x.env"}, ExternalSecretsFiles: []string{"secrets.yaml"}})
	if err != nil {
		t.Fatal(err)
	}

	for _, secret := range []string{"s3cr3t", "tok3n", "x-access-token", "internal:"} {
		if bytes.Contains(data, []byte(secret)) {
			t.Errorf("deploy config contains %q:\n%s", secret, data)
		}
	}

	for _, want := range []string{"visible-value", ".env", "x.env", "secrets.yaml", "https://github.com/org/repo.git"} {
		if !bytes.Contains(data, []byte(want)) {
			t.Errorf("deploy config misses %q:\n%s", want, data)
		}
	}

	// The live config is untouched.
	if c.Internal.Environment["SECRET"] != "s3cr3t" || c.EnvFiles != nil || !strings.Contains(string(c.RepositoryUrl), "tok3n") {
		t.Fatalf("deployConfigForLastGood mutated its input: %+v", c)
	}

	c.Timeout = duration.Duration(1500 * time.Millisecond)
	if _, err := deployConfigForLastGood(c, declaredFiles{}); err == nil {
		t.Fatal("expected an encode error for a non-whole-second timeout")
	}
}

const lastGoodRoundTripConfig = `
name: web
reference: main
repository_url: https://github.com/org/repo.git
environment:
  PUBLIC: visible-value
env_files: [x.env]
external_secrets:
  LEGACY: 138e3a97-ed58-431c-b366-b35500663411
  HOOK:
    store_ref: bitwarden-login
    remote_ref:
      key: abc
      property: password
destroy:
  enabled: false
  remove_volumes: false
rollback:
  enabled: true
  timeout: 2m
reconciliation:
  enabled: false
  events: [die]
  restart_timeout: 30s
auto_discovery:
  enabled: true
  depth: 2
  remove_images: false
timeout: 90s
`

// The stored YAML reads back into the same deploy config, external secret references included.
func TestDeployConfigForLastGoodRoundTrip(t *testing.T) {
	t.Parallel()

	var orig deploy.Config
	if err := yaml.Unmarshal([]byte(lastGoodRoundTripConfig), &orig); err != nil {
		t.Fatal(err)
	}

	orig.Internal.Environment = map[string]string{"SECRET": "s3cr3t"}

	data, err := deployConfigForLastGood(&orig, declaredFiles{EnvFiles: []string{".env", "x.env"}, ExternalSecretsFiles: []string{"secrets.yaml"}})
	if err != nil {
		t.Fatal(err)
	}

	var got deploy.Config
	if err := yaml.Unmarshal(data, &got); err != nil {
		t.Fatalf("stored deploy config does not read back: %v\n%s", err, data)
	}

	want := orig
	want.Internal = deploy.Config{}.Internal
	want.EnvFiles = []string{".env", "x.env"}
	want.ExternalSecretsFiles = []string{"secrets.yaml"}

	if len(want.ExternalSecrets) != 2 || !reflect.DeepEqual(got.ExternalSecrets, want.ExternalSecrets) {
		t.Fatalf("external secrets = %+v, want %+v", got.ExternalSecrets, want.ExternalSecrets)
	}

	for name, pair := range map[string][2]any{
		"destroy":        {got.Destroy, want.Destroy},
		"rollback":       {got.Rollback, want.Rollback},
		"reconciliation": {got.Reconciliation, want.Reconciliation},
		"auto_discovery": {got.AutoDiscovery, want.AutoDiscovery},
		"timeout":        {got.Timeout, want.Timeout},
		"environment":    {got.Environment, want.Environment},
	} {
		if !reflect.DeepEqual(pair[0], pair[1]) {
			t.Errorf("%s = %+v, want %+v", name, pair[0], pair[1])
		}
	}

	// The hash covers every field; it only tells nil and empty collections apart, which YAML
	// does not.
	gotHash, err := got.Hash()
	if err != nil {
		t.Fatal(err)
	}

	wantHash, err := want.Hash()
	if err != nil {
		t.Fatal(err)
	}

	if gotHash != wantHash {
		t.Fatalf("round trip changed the deploy config:\n%s", data)
	}
}

func TestWithoutUserinfo(t *testing.T) {
	t.Parallel()

	for in, want := range map[string]string{ //nolint:gosec // fake credentials in test URLs
		"https://user:token@example.com/org/repo.git": "https://example.com/org/repo.git",
		"https://token@example.com/org/repo.git":      "https://example.com/org/repo.git",
		"ssh://git@example.com/org/repo.git":          "ssh://example.com/org/repo.git",
		"https://example.com/org/repo.git":            "https://example.com/org/repo.git",
		"ghcr.io/org/app:1.0":                         "ghcr.io/org/app:1.0",
		"ghcr.io/org/app@sha256:abc":                  "ghcr.io/org/app@sha256:abc",
		"git@github.com:org/repo.git":                 "github.com:org/repo.git",
		"user:token@host:org/repo":                    "host:org/repo",
		"https://user:token@example.com/%zz":          redactedURL,
		"":                                            "",
	} {
		if got := withoutUserinfo(in); got != want {
			t.Errorf("withoutUserinfo(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestServiceImages(t *testing.T) {
	t.Parallel()

	apiClient := &lastGoodClient{
		containers: []container.Summary{
			serviceContainer("/web-db-1", "db", "postgres:16", "sha256:db", nil),
			serviceContainer("/web-app-2", "app", "nginx:1", "sha256:app-2", nil),
			serviceContainer("/web-app-1", "app", "nginx:1", "sha256:app-1", nil),
			serviceContainer("/web-backup-run", "backup", "restic", "sha256:job", map[string]string{docker.DocoCDJobLabels.JobEphemeral: "true"}),
			{Names: nil, Image: "x", ImageID: "sha256:x", Labels: map[string]string{api.ProjectLabel: "web"}},
		},
		digests: map[string][]string{"sha256:app-1": {"nginx@sha256:d1"}},
	}
	sm, _ := newLastGoodManager(t, &deploy.RollbackConfig{Enabled: true}, apiClient)

	images, err := sm.serviceImages(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	want := []rollback.ServiceImage{
		{Service: "app", Image: "nginx:1", ImageID: "sha256:app-1", RepoDigests: []string{"nginx@sha256:d1"}},
		{Service: "db", Image: "postgres:16", ImageID: "sha256:db"},
	}
	if len(images) != len(want) {
		t.Fatalf("images = %+v, want %+v", images, want)
	}

	for i := range want {
		if images[i].Service != want[i].Service || images[i].ImageID != want[i].ImageID ||
			images[i].Image != want[i].Image || len(images[i].RepoDigests) != len(want[i].RepoDigests) {
			t.Fatalf("images[%d] = %+v, want %+v", i, images[i], want[i])
		}
	}

	if !apiClient.options.All || !apiClient.options.Filters["label"][api.ProjectLabel+"=web"] ||
		!apiClient.options.Filters["label"][api.OneoffLabel+"=False"] {
		t.Fatalf("unexpected list options: %+v", apiClient.options)
	}

	apiClient.inspectErr = errors.New("no such image")

	if _, err := sm.serviceImages(t.Context()); err == nil {
		t.Fatal("expected an inspect error")
	}
}

func TestDeployedCommit(t *testing.T) {
	t.Parallel()

	sm, _ := newLastGoodManager(t, nil, &lastGoodClient{})

	if got, err := sm.deployedCommit(); err != nil || got != "sha256:abc" {
		t.Fatalf("OCI deployedCommit() = %q, %v", got, err)
	}

	sm.Repository.Source = config.SourceTypeGit
	sm.DeployState.latestCommit = "c1"

	if got, err := sm.deployedCommit(); err != nil || got != "c1" {
		t.Fatalf("git deployedCommit() = %q, %v", got, err)
	}

	sm.DeployState.latestCommit = ""
	sm.Repository.MirrorDir = ""

	if _, err := sm.deployedCommit(); err == nil {
		t.Fatal("expected an error without a mirror to resolve the commit from")
	}
}
