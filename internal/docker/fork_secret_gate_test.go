package docker

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"testing"

	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/filesystem"
	"github.com/kimdre/doco-cd/internal/secretprovider"
	secrettypes "github.com/kimdre/doco-cd/internal/secretprovider/types"
)

type namedProvider struct{ secretprovider.SecretProvider }

func (namedProvider) Name() string { return "openbao" }

// TestCheckForkSecretGate sets the process-wide ForkSecretGate, so it does not call t.Parallel.
func TestCheckForkSecretGate(t *testing.T) {
	t.Cleanup(func() { ForkSecretGate = nil })

	cfg := deploy.New("web", "main")
	refs := map[string]string{"DB": "kv:secret:prod/web/db:PASS"}

	ForkSecretGate = nil

	if err := checkForkSecretGate(context.Background(), cfg, namedProvider{}, refs); err != nil {
		t.Fatalf("error = %v, want nil without a gate", err)
	}

	errDenied := errors.New("denied")

	var gotObject, gotProvider string

	ForkSecretGate = func(object, provider string, _ map[string]string) error {
		gotObject, gotProvider = object, provider
		return errDenied
	}

	// With the gate set, a context without the job gate's object is refused before the gate: the
	// only callers go through loadComposeScheduledProjectAll, which always sets it.
	if err := checkForkSecretGate(context.Background(), cfg, namedProvider{}, refs); err == nil || errors.Is(err, errDenied) {
		t.Fatalf("error = %v, want a refusal for a missing job object", err)
	}

	if gotObject != "" {
		t.Fatalf("gate called with %q, want no call without the job object", gotObject)
	}

	ctx := withForkJobObject(context.Background(), "remote/web")
	if err := checkForkSecretGate(ctx, cfg, namedProvider{}, refs); err == nil || errors.Is(err, errDenied) {
		t.Fatalf("error = %v, want a mismatch refusal before the gate", err)
	}

	ctx = withForkJobObject(context.Background(), "default/web")
	if err := checkForkSecretGate(ctx, cfg, namedProvider{}, refs); !errors.Is(err, errDenied) {
		t.Fatalf("error = %v, want the gate error with a matching object", err)
	}

	if gotObject != "default/web" || gotProvider != "openbao" {
		t.Fatalf("gate called with %q, %q", gotObject, gotProvider)
	}

	ForkSecretGate = func(string, string, map[string]string) error { return nil }

	if err := checkForkSecretGate(ctx, cfg, namedProvider{}, refs); err != nil {
		t.Fatalf("error = %v, want nil when the gate allows", err)
	}
}

// TestWithForkJobGateObject sets the process-wide ForkSecretGate, so it does not call t.Parallel.
func TestWithForkJobGateObject(t *testing.T) {
	t.Cleanup(func() { ForkSecretGate = nil })

	// Without the gate the client is not touched: a nil embedded command.Cli would panic.
	ForkSecretGate = nil

	if ctx := withForkJobGateObject(context.Background(), contextCli{}, "web"); ctx.Value(forkJobObjectKey{}) != nil {
		t.Fatal("object recorded without a gate")
	}

	ForkSecretGate = func(string, string, map[string]string) error { return nil }

	ctx := withForkJobGateObject(context.Background(), contextCli{name: "remote"}, "web")
	if got, _ := ctx.Value(forkJobObjectKey{}).(string); got != "remote/web" {
		t.Fatalf("object = %q, want remote/web", got)
	}
}

// countingProvider counts ResolveSecretReferences calls: a refused job must never reach it.
type countingProvider struct {
	namedProvider

	calls int
}

func (p *countingProvider) ResolveSecretReferences(context.Context, map[string]string) (secrettypes.ResolvedSecrets, error) {
	p.calls++
	return secrettypes.ResolvedSecrets{"CERT": "value"}, nil
}

// TestScheduledReloadSecretGate drives the scheduled reload end to end: the object computed next to
// the job gate reaches ForkSecretGate with the encoded references, and a refusal stops the reload
// before the provider. It sets the process-wide ForkSecretGate, so it does not call t.Parallel.
func TestScheduledReloadSecretGate(t *testing.T) {
	t.Cleanup(func() { ForkSecretGate = nil })

	dataMountPath := t.TempDir()
	opts := ScheduledComposeOptions{ComposeLoad: ComposeLoadOptions{DataMountPath: dataMountPath}, DeployConfigBaseDir: "/"}
	repoRoot := filepath.Join(dataMountPath, "example.com", "owner", "repo")
	workingDir := filepath.Join(repoRoot, "stack")

	if err := os.MkdirAll(filepath.Join(repoRoot, ".git"), filesystem.PermDir); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(workingDir, filesystem.PermDir); err != nil {
		t.Fatal(err)
	}

	createComposeFile(t, filepath.Join(workingDir, "compose.yml"), "services:\n  app:\n    image: busybox:latest\n    environment:\n      CERT: ${CERT}\n")
	createComposeFile(t, filepath.Join(repoRoot, ".doco-cd.yml"),
		"name: web\nreference: refs/heads/main\nworking_dir: stack\ncompose_files:\n  - compose.yml\nexternal_secrets:\n  CERT: \"kv:secret:prod/other/db:PASS\"\n")

	ref, err := composeScheduledServiceRefFromSwarmLabels(map[string]string{
		DocoCDLabels.Deployment.Name:       "web",
		DocoCDLabels.Deployment.WorkingDir: workingDir,
		DocoCDLabels.Source.URL:            "https://example.com/owner/repo",
		DocoCDLabels.Deployment.TargetRef:  "refs/heads/main",
	})
	if err != nil {
		t.Fatal(err)
	}

	errDenied := errors.New("denied")

	var gotObject string

	var gotRefs map[string]string

	ForkSecretGate = func(object, _ string, refs map[string]string) error {
		gotObject, gotRefs = object, refs
		return errDenied
	}

	p := &countingProvider{}

	_, _, err = loadComposeScheduledProjectAll(context.Background(), nil, ref, p, opts)
	if !errors.Is(err, errDenied) {
		t.Fatalf("error = %v, want the gate error", err)
	}

	if p.calls != 0 {
		t.Fatalf("provider called %d times, want 0", p.calls)
	}

	wantRefs, err := secrettypes.EncodeExternalSecretRefs(map[string]secrettypes.ExternalSecretRef{
		"CERT": {LegacyRef: "kv:secret:prod/other/db:PASS"},
	})
	if err != nil {
		t.Fatal(err)
	}

	if gotObject != "default/web" || !maps.Equal(gotRefs, wantRefs) {
		t.Fatalf("gate called with %q, %v, want default/web, %v", gotObject, gotRefs, wantRefs)
	}
}
