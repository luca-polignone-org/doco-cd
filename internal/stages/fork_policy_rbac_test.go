package stages

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/kimdre/doco-cd/internal/composepolicy"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/rbac"
	"github.com/kimdre/doco-cd/internal/secretprovider"
	secrettypes "github.com/kimdre/doco-cd/internal/secretprovider/types"
)

// setForkRBAC writes policy to a temp file, registers its enforcer and resets it at cleanup.
func setForkRBAC(t *testing.T, policy string) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "rbac.csv")
	if err := os.WriteFile(path, []byte(policy), 0o600); err != nil {
		t.Fatalf("write rbac policy: %v", err)
	}

	e, err := rbac.NewFileEnforcer(path, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("NewFileEnforcer() error = %v", err)
	}

	SetRBAC(e)
	t.Cleanup(func() { SetRBAC(nil) })
}

func TestCheckTargetSync(t *testing.T) {
	alice := rbac.WithPrincipal(context.Background(), rbac.Principal{Subject: "alice"})
	s := newForkPolicyStageManager(t, "web", forkPolicyRepo)

	t.Run("principal without permission", func(t *testing.T) {
		setForkRBAC(t, "p, alice, targets, sync, default/other, allow\n")

		requirePolicyDenied(t, s.checkTargetSync(alice, "default/web"),
			[]composepolicy.Violation{{Service: "", Field: "rbac", Value: "targets/sync"}})
	})

	t.Run("principal with permission", func(t *testing.T) {
		setForkRBAC(t, "p, alice, targets, sync, default/*, allow\n")

		if err := s.checkTargetSync(alice, "default/web"); err != nil {
			t.Fatalf("checkTargetSync() error = %v, want nil", err)
		}
	})

	t.Run("context without principal", func(t *testing.T) {
		setForkRBAC(t, "p, alice, targets, sync, default/other, allow\n")

		if err := s.checkTargetSync(context.Background(), "default/web"); err != nil {
			t.Fatalf("checkTargetSync() error = %v, want nil", err)
		}
	})

	t.Run("RBAC disabled", func(t *testing.T) {
		SetRBAC(nil)

		if err := s.checkTargetSync(alice, "default/web"); err != nil {
			t.Fatalf("checkTargetSync() error = %v, want nil", err)
		}
	})
}

// countingSecretProvider counts secret resolutions and fails them, so pre-deploy stops right
// after resolving and the test can tell whether resolution happened.
type countingSecretProvider struct {
	secretprovider.SecretProvider

	resolved int
}

var errForkTestSecrets = errors.New("fork test: secret resolution reached")

func (p *countingSecretProvider) ResolveSecretReferences(context.Context, map[string]string) (secrettypes.ResolvedSecrets, error) {
	p.resolved++

	return nil, errForkTestSecrets
}

func TestRunForkPreDeployStageTargetSync(t *testing.T) {
	t.Cleanup(func() { verifiedObjects.Clear() })

	anonymous := context.Background()
	alice := rbac.WithPrincipal(anonymous, rbac.Principal{Subject: "alice"})
	bob := rbac.WithPrincipal(anonymous, rbac.Principal{Subject: "bob"})
	stageLog := slog.New(slog.NewTextHandler(io.Discard, nil))

	setForkComposePolicy(t, forkPolicyFor("default/sync"))
	setForkRBAC(t, "p, alice, targets, sync, default/other, allow\np, bob, targets, sync, default/sync, allow\n")

	// newRun returns a pre-deploy of default/sync with an external secret. It has no Docker
	// client and no deployed state, so a check placed after secret resolution, after the
	// cached-preflight skip or after any Docker lookup could not return PolicyDeniedError.
	newRun := func(t *testing.T) (*StageManager, *countingSecretProvider) {
		t.Helper()

		provider := &countingSecretProvider{}
		s := newForkPolicyStageManager(t, "sync", forkPolicyRepo)
		s.Stages = &Stages{PreDeploy: &PreDeployStageData{}}
		s.SecretProvider = provider
		s.DeployConfig.ExternalSecrets = map[string]secrettypes.ExternalSecretRef{"TOKEN": {LegacyRef: "secret-id"}}

		return s, provider
	}

	t.Run("denied principal is refused before secret resolution and the cache skip", func(t *testing.T) {
		s, provider := newRun(t)

		err := s.runForkPreDeployStage(alice, stageLog)
		requirePolicyDenied(t, err, []composepolicy.Violation{{Field: "rbac", Value: "targets/sync"}})

		if errors.Is(err, ErrSkipDeployment) {
			t.Fatal("a denied principal got a skip instead of a terminal denial")
		}

		if provider.resolved != 0 {
			t.Fatalf("external secrets resolved %d times before the denial, want 0", provider.resolved)
		}

		if _, known := verifiedObjects.Load("default/sync"); known {
			t.Fatal("a targets/sync denial recorded the object for the job gate")
		}
	})

	t.Run("denial keeps the outcome of the last compose check", func(t *testing.T) {
		s, _ := newRun(t)
		s.Docker.Project = &types.Project{Name: "sync", Services: types.Services{"app": {Name: "app", Image: "busybox:latest"}}}

		if err := s.checkForkPolicies(anonymous); err != nil {
			t.Fatalf("checkForkPolicies() error = %v, want nil", err)
		}

		requirePolicyDenied(t, s.runForkPreDeployStage(alice, stageLog),
			[]composepolicy.Violation{{Field: "rbac", Value: "targets/sync"}})

		if err := forkJobGate("default", "sync"); err != nil {
			t.Fatalf("forkJobGate() error = %v, want nil: a caller without permission must not disable the jobs", err)
		}
	})

	for name, ctx := range map[string]context.Context{"allowed principal": bob, "no principal (upstream)": anonymous} {
		t.Run(name, func(t *testing.T) {
			s, provider := newRun(t)

			if err := s.runForkPreDeployStage(ctx, stageLog); !errors.Is(err, errForkTestSecrets) {
				t.Fatalf("runForkPreDeployStage() error = %v, want upstream pre-deploy to run and reach secret resolution", err)
			}

			if provider.resolved != 1 {
				t.Fatalf("external secrets resolved %d times, want 1", provider.resolved)
			}
		})
	}
}

func TestRunForkDestroyStageTargetSync(t *testing.T) {
	var dockerRequests atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		dockerRequests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[]")) // no containers: nothing to destroy
	}))
	t.Cleanup(server.Close)
	t.Setenv("DOCKER_HOST", "tcp://"+strings.TrimPrefix(server.URL, "http://"))
	t.Setenv("DOCKER_API_VERSION", "1.52")

	dockerCli, err := docker.CreateDockerCli(true)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = dockerCli.Client().Close() })

	setForkRBAC(t, "p, alice, targets, sync, default/other, allow\n")

	alice := rbac.WithPrincipal(context.Background(), rbac.Principal{Subject: "alice"})
	stageLog := slog.New(slog.NewTextHandler(io.Discard, nil))

	newDestroy := func(t *testing.T) *StageManager {
		t.Helper()

		s := newForkPolicyStageManager(t, "gone", forkPolicyRepo)
		s.Stages = &Stages{Destroy: &DestroyStageData{}}
		s.Docker.Cmd = dockerCli
		s.DeployConfig.Destroy.Enabled = true

		return s
	}

	if got := newDestroy(t).GetDestroyStageOrder().Funcs[StageDestroy]; got == nil {
		t.Fatal("destroy stage order has no destroy stage")
	}

	t.Run("denied principal is refused before Docker is touched", func(t *testing.T) {
		dockerRequests.Store(0)

		requirePolicyDenied(t, newDestroy(t).GetDestroyStageOrder().Funcs[StageDestroy](alice, stageLog),
			[]composepolicy.Violation{{Field: "rbac", Value: "targets/sync"}})

		if n := dockerRequests.Load(); n != 0 {
			t.Fatalf("docker requests = %d before the denial, want 0", n)
		}
	})

	t.Run("no principal runs upstream destroy", func(t *testing.T) {
		dockerRequests.Store(0)

		if err := newDestroy(t).GetDestroyStageOrder().Funcs[StageDestroy](t.Context(), stageLog); !errors.Is(err, ErrSkipDeployment) {
			t.Fatalf("destroy stage error = %v, want the upstream nothing-to-destroy skip", err)
		}

		if dockerRequests.Load() == 0 {
			t.Fatal("upstream destroy did not query Docker")
		}
	})
}

func TestForkPoliciesActiveWithPrincipal(t *testing.T) {
	previous := composePolicy.Load()

	SetComposePolicy(nil)
	t.Cleanup(func() { SetComposePolicy(previous) })

	alice := rbac.WithPrincipal(context.Background(), rbac.Principal{Subject: "alice"})
	s := newForkPolicyStageManager(t, "web", forkPolicyRepo)

	if s.forkPoliciesActive(alice) {
		t.Fatal("forkPoliciesActive() = true with a principal but RBAC disabled, want the upstream path")
	}

	setForkRBAC(t, "p, alice, targets, sync, default/*, allow\n")

	if !s.forkPoliciesActive(alice) {
		t.Fatal("forkPoliciesActive() = false with a principal and RBAC enabled")
	}

	if s.forkPoliciesActive(context.Background()) {
		t.Fatal("forkPoliciesActive() = true without a principal and without a compose policy")
	}
}
