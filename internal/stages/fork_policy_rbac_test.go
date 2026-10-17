package stages

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/kimdre/doco-cd/internal/composepolicy"
	"github.com/kimdre/doco-cd/internal/rbac"
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

func TestCheckForkPoliciesTargetSync(t *testing.T) {
	t.Cleanup(func() { verifiedObjects.Clear() })

	anonymous := context.Background()
	alice := rbac.WithPrincipal(anonymous, rbac.Principal{Subject: "alice"})

	setForkComposePolicy(t, forkPolicyFor("default/sync"))
	setForkRBAC(t, "p, alice, targets, sync, default/other, allow\n")

	s := newForkPolicyStageManager(t, "sync", forkPolicyRepo)
	s.Docker.Project = &types.Project{Name: "sync", Services: types.Services{"app": {Name: "app", Image: "busybox:latest"}}}

	t.Run("denial does not mark an unknown object as verified", func(t *testing.T) {
		requirePolicyDenied(t, s.checkForkPolicies(alice),
			[]composepolicy.Violation{{Field: "rbac", Value: "targets/sync"}})

		if _, known := verifiedObjects.Load("default/sync"); known {
			t.Fatal("a targets/sync denial recorded the object for the job gate")
		}
	})

	t.Run("denial keeps the outcome of the last compose check", func(t *testing.T) {
		if err := s.checkForkPolicies(anonymous); err != nil {
			t.Fatalf("checkForkPolicies() without principal error = %v, want nil", err)
		}

		requirePolicyDenied(t, s.checkForkPolicies(alice),
			[]composepolicy.Violation{{Field: "rbac", Value: "targets/sync"}})

		if err := forkJobGate("default", "sync"); err != nil {
			t.Fatalf("forkJobGate() error = %v, want nil: a caller without permission must not disable the jobs", err)
		}
	})
}

func TestForkPoliciesActiveWithPrincipal(t *testing.T) {
	SetComposePolicy(nil)

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
