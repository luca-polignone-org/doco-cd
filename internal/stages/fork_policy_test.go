package stages

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/moby/moby/api/types/container"

	"github.com/kimdre/doco-cd/internal/composepolicy"
	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/docker"
)

// The tests in this file register a process-wide compose policy, so none of them calls
// t.Parallel: sequential top-level tests never overlap with the parallel ones.

const forkPolicyRepo = "https://github.com/o/r"

// setForkComposePolicy writes policy to a temp file, registers it and resets it at cleanup.
func setForkComposePolicy(t *testing.T, policy string) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "compose-policy.yaml")
	if err := os.WriteFile(path, []byte(policy), 0o600); err != nil {
		t.Fatalf("write compose policy: %v", err)
	}

	holder, err := composepolicy.NewHolder(path, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewHolder() error = %v", err)
	}

	SetComposePolicy(holder)
	t.Cleanup(func() { SetComposePolicy(nil) })
}

func forkPolicyFor(object string) string {
	return "version: 1\ntargets:\n  \"" + object + "\":\n    repository: " + forkPolicyRepo + "\n"
}

func newForkPolicyStageManager(t *testing.T, name, sourceURL string) *StageManager {
	t.Helper()

	checkout := t.TempDir()

	return &StageManager{
		AppConfig:    &app.Config{},
		DeployConfig: deploy.New(name, "main"),
		Docker:       &Docker{},
		Repository: &RepositoryData{
			SourceUrl:    sourceURL,
			PathInternal: checkout,
			PathExternal: checkout,
		},
	}
}

func privilegedProject(name string) *types.Project {
	return &types.Project{
		Name:     name,
		Services: types.Services{"app": {Name: "app", Image: "busybox:latest", Privileged: true}},
	}
}

func requirePolicyDenied(t *testing.T, err error, want []composepolicy.Violation) {
	t.Helper()

	if !errors.Is(err, ErrPolicyDenied) {
		t.Fatalf("error = %v, want ErrPolicyDenied", err)
	}

	var denied *PolicyDeniedError
	if !errors.As(err, &denied) {
		t.Fatalf("error = %T, want *PolicyDeniedError", err)
	}

	if !reflect.DeepEqual(denied.Violations, want) {
		t.Fatalf("violations = %+v, want %+v", denied.Violations, want)
	}
}

func TestCheckForkPolicies(t *testing.T) {
	t.Run("no policy registered", func(t *testing.T) {
		SetComposePolicy(nil)

		s := newForkPolicyStageManager(t, "web", "https://github.com/o/evil")
		s.Docker.Project = privilegedProject("web")

		if err := s.checkForkPolicies(context.Background()); err != nil {
			t.Fatalf("checkForkPolicies() error = %v, want nil", err)
		}
	})

	t.Run("privileged service of a bound target", func(t *testing.T) {
		setForkComposePolicy(t, forkPolicyFor("default/web"))

		s := newForkPolicyStageManager(t, "web", forkPolicyRepo)
		s.Docker.Project = privilegedProject("web")

		requirePolicyDenied(t, s.checkForkPolicies(context.Background()),
			[]composepolicy.Violation{{Service: "app", Field: "privileged", Value: "true"}})
	})

	t.Run("target bound to another repository", func(t *testing.T) {
		setForkComposePolicy(t, forkPolicyFor("default/web"))

		s := newForkPolicyStageManager(t, "web", "https://github.com/o/evil")
		s.Docker.Project = privilegedProject("web")

		requirePolicyDenied(t, s.checkForkPolicies(context.Background()),
			[]composepolicy.Violation{{Field: "target", Value: "default/web"}})
	})

	t.Run("rollback key on swarm with the policy disabled", func(t *testing.T) {
		SetComposePolicy(nil)

		s := newForkPolicyStageManager(t, "web", forkPolicyRepo)
		s.Docker.SwarmMode = true
		s.DeployConfig.Rollback = &deploy.RollbackConfig{Enabled: true}

		requirePolicyDenied(t, s.checkForkPolicies(context.Background()),
			[]composepolicy.Violation{{Field: "swarm", Value: "true"}})
	})
}

func TestPolicyDeniedErrorMessage(t *testing.T) {
	err := &PolicyDeniedError{Object: "default/web", Violations: []composepolicy.Violation{
		{Service: "app", Field: "privileged", Value: "true"},
		{Field: "target", Value: "default/web"},
	}}

	want := "deployment denied by policy for default/web: app: privileged=true; target=default/web"
	if got := err.Error(); got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}

	if err.Is(errors.New("deployment denied by policy")) {
		t.Fatal("Is() matched an error other than ErrPolicyDenied")
	}
}

// TestCheckForkPoliciesSeesEnvFilesConsumedByInit runs the real init stage, which consumes
// deploy.Config.EnvFiles, and checks that the policy still sees the declared list.
func TestCheckForkPoliciesSeesEnvFilesConsumedByInit(t *testing.T) {
	dataMount := t.TempDir()
	originPath := filepath.Join(t.TempDir(), "origin")
	cloneURL := initEnvFileTestRepo(t, originPath, map[string]string{
		"stack/.env": "IN_CHECKOUT=1\n",
	})

	outside := filepath.Join(t.TempDir(), "x")
	if err := os.WriteFile(outside, []byte("OUTSIDE=1\n"), 0o600); err != nil {
		t.Fatalf("write env file outside the checkout: %v", err)
	}

	repoName := "forkpolicy/repo"
	artifactPath, revision, mirrorDir := resolveViaGitStore(t, cloneURL, "main", filepath.Join(dataMount, repoName))

	repository := &RepositoryData{
		Source:            config.SourceTypeGit,
		SourceUrl:         forkPolicyRepo,
		Name:              repoName,
		PathInternal:      artifactPath,
		PathExternal:      artifactPath,
		MirrorDir:         mirrorDir,
		Revision:          revision,
		ResolvedReference: "refs/heads/main",
	}

	deployConfig := deploy.New("app", "main")
	deployConfig.WorkingDirectory = "stack"
	deployConfig.EnvFiles = []string{".env", outside}

	sm := newFastPathStageManager(t, repository, deployConfig, dataMount)

	if err := sm.RunInitStage(context.Background(), sm.Log); err != nil {
		t.Fatalf("RunInitStage() error = %v", err)
	}

	if len(sm.DeployConfig.EnvFiles) != 0 {
		t.Fatalf("EnvFiles = %v, want them consumed by the init stage", sm.DeployConfig.EnvFiles)
	}

	setForkComposePolicy(t, forkPolicyFor("default/app"))

	requirePolicyDenied(t, sm.checkForkPolicies(context.Background()),
		[]composepolicy.Violation{{Field: "deploy.env_files", Value: outside}})
}

func TestForkExternalPaths(t *testing.T) {
	s := &StageManager{
		DeployConfig: deploy.New("app", "main"),
		Repository:   &RepositoryData{PathExternal: "/data/deploy/artifacts/r2", ConfigPath: "/data/config/artifacts/r1"},
	}
	s.DeployConfig.WorkingDirectory = "stack"

	t.Run("without repository_url", func(t *testing.T) {
		got := s.externalPaths([]string{".env", "/abs/x"}, true)
		want := []string{"/data/deploy/artifacts/r2/stack/.env", "/abs/x"}

		if !reflect.DeepEqual(got, want) {
			t.Fatalf("externalPaths() = %v, want %v", got, want)
		}
	})

	t.Run("with repository_url", func(t *testing.T) {
		s.DeployConfig.RepositoryUrl = "https://github.com/o/deploy.git"

		t.Cleanup(func() { s.DeployConfig.RepositoryUrl = "" })

		got := s.externalPaths([]string{".env", "remote:.env", "/abs/x", "remote:/abs/y"}, true)
		want := []string{
			"/data/config/artifacts/r1/.env",
			"/data/deploy/artifacts/r2/stack/.env",
			"/abs/x",
			"/abs/y",
		}

		if !reflect.DeepEqual(got, want) {
			t.Fatalf("externalPaths() = %v, want %v", got, want)
		}
	})

	t.Run("external secrets files join absolute entries", func(t *testing.T) {
		got := s.externalPaths([]string{"/abs/secrets.yaml"}, false)
		want := []string{"/data/deploy/artifacts/r2/stack/abs/secrets.yaml"}

		if !reflect.DeepEqual(got, want) {
			t.Fatalf("externalPaths() = %v, want %v", got, want)
		}
	})
}

func TestForkPolicyPaths(t *testing.T) {
	s := &StageManager{
		Docker: &Docker{DataMountPoint: container.MountPoint{Source: "/srv/doco/data", Destination: "/data"}},
		Repository: &RepositoryData{
			PathExternal: "/srv/doco/data/deploy/artifacts/r2",
			PathInternal: "/data/deploy/artifacts/r2",
			ConfigPath:   "/srv/doco/data/config/artifacts/r1",
		},
	}

	got := s.policyPaths()
	want := composepolicy.Paths{
		CheckoutExternal: "/srv/doco/data/deploy/artifacts/r2",
		CheckoutInternal: "/data/deploy/artifacts/r2",
		ConfigExternal:   "/srv/doco/data/config/artifacts/r1",
		ConfigInternal:   "/data/config/artifacts/r1",
	}

	if got != want {
		t.Fatalf("policyPaths() = %+v, want %+v", got, want)
	}

	s.Repository.ConfigPath = "/elsewhere/config"
	if got = s.policyPaths(); got.ConfigExternal != "" || got.ConfigInternal != "" {
		t.Fatalf("policyPaths() = %+v, want no config source outside the data mount", got)
	}
}

func TestForkPolicyGate(t *testing.T) {
	newFirstDeploy := func(t *testing.T) *StageManager {
		t.Helper()

		s := newForkPolicyStageManager(t, "web", forkPolicyRepo)
		s.DeployConfig.WorkingDirectory = "."
		s.DeployConfig.ComposeFiles = []string{"compose.yaml"}
		s.DeployConfig.EnvFiles = nil

		compose := "services:\n  app:\n    image: busybox:latest\n    privileged: true\n"
		if err := os.WriteFile(filepath.Join(s.Repository.PathExternal, "compose.yaml"), []byte(compose), 0o600); err != nil {
			t.Fatalf("write compose file: %v", err)
		}

		return s
	}

	t.Run("first deploy with the policy enabled", func(t *testing.T) {
		setForkComposePolicy(t, forkPolicyFor("default/web"))

		s := newFirstDeploy(t)

		requirePolicyDenied(t, s.forkPolicyGate(context.Background()),
			[]composepolicy.Violation{{Service: "app", Field: "privileged", Value: "true"}})

		if s.Docker.Project == nil {
			t.Fatal("forkPolicyGate() did not load the Compose project")
		}
	})

	t.Run("project already loaded by pre-deploy", func(t *testing.T) {
		setForkComposePolicy(t, forkPolicyFor("default/web"))

		s := newFirstDeploy(t)
		s.Docker.Project = &types.Project{Name: "web"}

		if err := s.forkPolicyGate(context.Background()); err != nil {
			t.Fatalf("forkPolicyGate() error = %v, want nil (pre-deploy already checked the project)", err)
		}
	})

	t.Run("no fork keys", func(t *testing.T) {
		SetComposePolicy(nil)

		s := newFirstDeploy(t)

		if err := s.forkPolicyGate(context.Background()); err != nil {
			t.Fatalf("forkPolicyGate() error = %v, want nil", err)
		}

		if s.Docker.Project != nil {
			t.Fatal("forkPolicyGate() loaded the Compose project without fork keys")
		}
	})
}

func TestForkJobGate(t *testing.T) {
	t.Cleanup(func() { verifiedObjects.Clear() })

	t.Run("policy disabled", func(t *testing.T) {
		SetComposePolicy(nil)

		if docker.ForkJobGate != nil {
			t.Fatal("SetComposePolicy(nil) left docker.ForkJobGate set")
		}

		if err := forkJobGate("", "jobs-unknown"); err != nil {
			t.Fatalf("forkJobGate() error = %v, want nil without a policy", err)
		}
	})

	t.Run("unknown, passed, then denied", func(t *testing.T) {
		setForkComposePolicy(t, forkPolicyFor("default/jobs"))

		if docker.ForkJobGate == nil {
			t.Fatal("SetComposePolicy() did not install docker.ForkJobGate")
		}

		if err := forkJobGate("default", "jobs"); !errors.Is(err, ErrJobNotVerified) {
			t.Fatalf("forkJobGate() error = %v, want ErrJobNotVerified for an unverified target", err)
		}

		s := newForkPolicyStageManager(t, "jobs", forkPolicyRepo)
		s.Docker.Project = &types.Project{Name: "jobs", Services: types.Services{"app": {Name: "app", Image: "busybox:latest"}}}

		if err := s.checkForkPolicies(context.Background()); err != nil {
			t.Fatalf("checkForkPolicies() error = %v, want nil", err)
		}

		if err := forkJobGate("", "jobs"); err != nil {
			t.Fatalf("forkJobGate() error = %v, want nil after a passed check", err)
		}

		s.Docker.Project = privilegedProject("jobs")
		if err := s.checkForkPolicies(context.Background()); !errors.Is(err, ErrPolicyDenied) {
			t.Fatalf("checkForkPolicies() error = %v, want ErrPolicyDenied", err)
		}

		if err := forkJobGate("default", "jobs"); !errors.Is(err, ErrPolicyDenied) {
			t.Fatalf("forkJobGate() error = %v, want ErrPolicyDenied after a denied check", err)
		}
	})
}
