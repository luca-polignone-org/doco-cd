package stages

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/kimdre/doco-cd/internal/composepolicy"
	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/secretprovider"
	secrettypes "github.com/kimdre/doco-cd/internal/secretprovider/types"
)

// The tests in this file register a process-wide compose policy, so none of them calls t.Parallel.

// allowlistSecretProvider counts ResolveSecretReferences calls: a denied reference must never reach
// the provider.
type allowlistSecretProvider struct {
	secretprovider.SecretProvider

	calls int
}

func (p *allowlistSecretProvider) Name() string { return "openbao" }

func (p *allowlistSecretProvider) ResolveSecretReferences(context.Context, map[string]string) (secrettypes.ResolvedSecrets, error) {
	p.calls++
	return secrettypes.ResolvedSecrets{"DB": "value"}, nil
}

func secretAllowlistPolicy(object string) string {
	return "version: 1\ndefault:\n  secret_allowlist: []\ntargets:\n  \"" + object + "\":\n    repository: " + forkPolicyRepo +
		"\n    secret_allowlist:\n      - kv:secret:prod/web/\n"
}

func newSecretStageManager(t *testing.T, refs map[string]string) (*StageManager, *allowlistSecretProvider) {
	t.Helper()

	s := newForkPolicyStageManager(t, "web", forkPolicyRepo)
	p := &allowlistSecretProvider{}
	s.SecretProvider = p
	s.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	s.Stages = &Stages{PreDeploy: &PreDeployStageData{MetaData: NewMetaData(StagePreDeploy)}}
	s.DeployConfig.ExternalSecrets = map[string]secrettypes.ExternalSecretRef{}

	for k, v := range refs {
		s.DeployConfig.ExternalSecrets[k] = secrettypes.ExternalSecretRef{LegacyRef: v}
	}

	return s, p
}

func TestPreDeploySecretAllowlist(t *testing.T) {
	setForkComposePolicy(t, secretAllowlistPolicy("default/web"))
	verifiedObjects.Clear()
	t.Cleanup(func() { verifiedObjects.Clear() })

	t.Run("denied reference never reaches the provider", func(t *testing.T) {
		s, p := newSecretStageManager(t, map[string]string{"DB": "kv:secret:prod/other/db:PASS"})

		verifiedObjects.Store("default/web", true)

		err := s.RunPreDeployStage(context.Background(), s.Log)
		requirePolicyDenied(t, err, []composepolicy.Violation{{Field: "external_secrets.DB", Value: "kv:secret:prod/other/db:PASS"}})

		if p.calls != 0 {
			t.Fatalf("provider called %d times, want 0", p.calls)
		}

		if v, _ := verifiedObjects.Load("default/web"); v != false {
			t.Fatalf("job gate verdict = %v, want false after a denial", v)
		}
	})

	t.Run("interpolated reference is checked after interpolation", func(t *testing.T) {
		t.Setenv("APP", "other")

		s, p := newSecretStageManager(t, map[string]string{"DB": "kv:secret:prod/${APP}/db:PASS"})
		s.AppConfig.InterpolateExternalSecrets = true

		err := s.RunPreDeployStage(context.Background(), s.Log)
		requirePolicyDenied(t, err, []composepolicy.Violation{{Field: "external_secrets.DB", Value: "kv:secret:prod/other/db:PASS"}})

		if p.calls != 0 {
			t.Fatalf("provider called %d times, want 0", p.calls)
		}
	})
}

func TestCheckForkSecretsNoPolicy(t *testing.T) {
	prev := composePolicy.Load()

	SetComposePolicy(nil)
	t.Cleanup(func() { SetComposePolicy(prev) })

	s, _ := newSecretStageManager(t, nil)
	if err := s.checkForkSecrets(map[string]string{"DB": "kv:secret:x/y:z"}, true); err != nil {
		t.Fatalf("checkForkSecrets() error = %v, want nil without a policy", err)
	}
}

func TestCheckForkSecretsAllowed(t *testing.T) {
	setForkComposePolicy(t, secretAllowlistPolicy("default/web"))

	s, _ := newSecretStageManager(t, nil)
	if err := s.checkForkSecrets(map[string]string{"DB": "kv:secret:prod/web/db:PASS"}, true); err != nil {
		t.Fatalf("checkForkSecrets() error = %v, want nil", err)
	}
}

func TestLastGoodSecretAllowlist(t *testing.T) {
	setForkComposePolicy(t, secretAllowlistPolicy("default/web"))

	r := newRollbackRepo(t)
	sm := newRollbackManager(t, r, &lastGoodClient{})
	sm.Repository.ConfigSourceUrl = forkPolicyRepo
	p := &allowlistSecretProvider{}
	sm.SecretProvider = p

	cfg, err := decodeLastGoodConfig([]byte(rollbackConfig + "external_secrets:\n  DB: kv:secret:prod/other/db:PASS\n"))
	if err != nil {
		t.Fatal(err)
	}

	verifiedObjects.Store("default/web", true)
	t.Cleanup(func() { verifiedObjects.Clear() })

	err = sm.loadLastGoodFiles(context.Background(), cfg, t.TempDir())
	requirePolicyDenied(t, err, []composepolicy.Violation{{Field: "external_secrets.DB", Value: "kv:secret:prod/other/db:PASS"}})

	if p.calls != 0 {
		t.Fatalf("provider called %d times, want 0", p.calls)
	}

	// The rollback checks with record=false: a denied last good does not change the job gate verdict.
	if v, _ := verifiedObjects.Load("default/web"); v != true {
		t.Fatalf("job gate verdict = %v, want true: the rollback must not record a denial", v)
	}
}

func TestForkSecretGate(t *testing.T) {
	setForkComposePolicy(t, secretAllowlistPolicy("default/web"))
	verifiedObjects.Clear()
	t.Cleanup(func() { verifiedObjects.Clear() })

	if docker.ForkSecretGate == nil {
		t.Fatal("SetComposePolicy must install docker.ForkSecretGate")
	}

	if err := forkSecretGate("default/web", "openbao", map[string]string{"DB": "kv:secret:prod/web/db:PASS"}); err != nil {
		t.Fatalf("allowed reference: error = %v", err)
	}

	err := forkSecretGate("default/web", "openbao", map[string]string{"DB": "kv:secret:prod/other/db:PASS"})
	if !errors.Is(err, ErrPolicyDenied) {
		t.Fatalf("denied reference: error = %v, want ErrPolicyDenied", err)
	}

	if v, ok := verifiedObjects.Load("default/web"); ok {
		t.Fatalf("job gate verdict = %v, want none: the secret gate does not record", v)
	}

	// An object missing from targets gets the target violation (errata 7).
	err = forkSecretGate("default/api", "openbao", map[string]string{"DB": "kv:secret:prod/web/db:PASS"})
	if !errors.Is(err, ErrPolicyDenied) {
		t.Fatalf("object not in targets: error = %v, want ErrPolicyDenied", err)
	}

	SetComposePolicy(nil)

	if docker.ForkSecretGate != nil {
		t.Fatal("SetComposePolicy(nil) must remove docker.ForkSecretGate")
	}

	if err := forkSecretGate("default/web", "openbao", map[string]string{"DB": "kv:secret:prod/other/db:PASS"}); err != nil {
		t.Fatalf("no policy: error = %v, want nil", err)
	}
}

// TestForkSecretGateDeclaredReference: the identity built from the target itself must pass the
// identity check when the target declares a full reference.
func TestForkSecretGateDeclaredReference(t *testing.T) {
	policy := "version: 1\ntargets:\n  \"default/web\":\n    repository: " + forkPolicyRepo +
		"\n    reference: refs/heads/main\n    secret_allowlist:\n      - kv:secret:prod/web/\n"
	setForkComposePolicy(t, policy)

	if err := forkSecretGate("default/web", "openbao", map[string]string{"DB": "kv:secret:prod/web/db:PASS"}); err != nil {
		t.Fatalf("allowed reference with declared reference: error = %v", err)
	}

	err := forkSecretGate("default/web", "openbao", map[string]string{"DB": "kv:secret:prod/other/db:PASS"})
	if !errors.Is(err, ErrPolicyDenied) {
		t.Fatalf("denied reference: error = %v, want ErrPolicyDenied", err)
	}
}

func TestLastGoodIdentityMatchesCheckLastGoodPolicy(t *testing.T) {
	s := newForkPolicyStageManager(t, "web", forkPolicyRepo)
	s.Repository.ResolvedReference = "refs/heads/main"
	s.DeployConfig.Internal.ConfigTarget = "web"

	got := s.lastGoodIdentity(deploy.New("web", "main"))
	want := composepolicy.Identity{Object: "default/web", Repository: forkPolicyRepo, Reference: "refs/heads/main", ConfigTarget: "web"}

	if got != want {
		t.Fatalf("lastGoodIdentity() = %+v, want %+v", got, want)
	}
}

// TestEveryResolveIsGated walks the production code for every secret read and wants exactly the
// three known call sites, each preceded by its secret allowlist check: an upstream merge that adds
// a read elsewhere breaks this test instead of silently opening a hole.
func TestEveryResolveIsGated(t *testing.T) {
	gates := map[string]string{
		"internal/stages/stage_2_pre-deploy.go":    "s.checkForkSecrets(encodedSecrets, true)",
		"internal/stages/fork_rollback.go":         "s.checkForkSecretsAs(s.lastGoodIdentity(cfg), encoded, false)",
		"internal/docker/compose_scheduled_run.go": "checkForkSecretGate(ctx, deployConfig, secretProvider, encodedSecrets)",
	}

	call := regexp.MustCompile(`ResolveSecretReferences\(|\.GetSecrets?\(`)
	lineComment := regexp.MustCompile(`(?m)^\s*//.*$`)
	root := filepath.Join("..", "..")
	found := map[string]int{}

	// fs.WalkDir on os.DirFS keeps every read inside the repository root (no symlink escape).
	repo := os.DirFS(root)

	for _, dir := range []string{"internal", "cmd"} {
		err := fs.WalkDir(repo, dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}

			if d.IsDir() {
				if path == "internal/secretprovider" {
					return fs.SkipDir
				}

				return nil
			}

			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}

			data, err := fs.ReadFile(repo, path)
			if err != nil {
				return err
			}

			if n := len(call.FindAllIndex(data, -1)); n > 0 {
				found[path] = n
			}

			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	if len(found) != len(gates) {
		t.Fatalf("secret reads in %v, want exactly %v: gate the new one with the secret allowlist", found, gates)
	}

	for file, gate := range gates {
		if found[file] != 1 {
			t.Fatalf("%s: %d secret reads, want 1: gate the new one", file, found[file])
		}

		data, err := fs.ReadFile(repo, file)
		if err != nil {
			t.Fatal(err)
		}

		// Whole-line comments are blanked out so a gate quoted in one does not count; trailing and block
		// comments and string literals are not handled, and the gates above are not inside any.
		src := lineComment.ReplaceAllString(string(data), "")
		if gi, ri := strings.Index(src, gate), call.FindStringIndex(src); gi < 0 || gi > ri[0] {
			t.Fatalf("%s: %q must come before the secret read", file, gate)
		}
	}
}
