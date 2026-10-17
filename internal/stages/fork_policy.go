package stages

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/kimdre/doco-cd/internal/composepolicy"
	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/docker"
	"github.com/kimdre/doco-cd/internal/rbac"
)

// Jobs that rebuild containers from a deployed revision (scheduled and one-off jobs, cert
// rotation, managed recreate, self-apply) go through docker.loadComposeScheduledProjectAll,
// which re-reads the compose project and the declared env/secrets files without pre-deploy.
// The policy may have been switched on or tightened after that revision was deployed, so
// SetComposePolicy installs forkJobGate as docker.ForkJobGate: a job runs only on an object whose
// last checkForkPolicies in this process passed. After a restart every job is refused until the
// next poll or deploy of its target has checked it again.

var (
	composePolicy atomic.Pointer[composepolicy.Holder]
	// verifiedObjects maps an object to the outcome of its last checkForkPolicies (true = passed).
	verifiedObjects sync.Map
	rbacEnforcer    atomic.Pointer[rbac.Enforcer]
)

// SetComposePolicy registers the compose policy of the process and the job gate; nil disables
// both. It is called at startup, before any scheduler reads docker.ForkJobGate.
func SetComposePolicy(h *composepolicy.Holder) {
	composePolicy.Store(h)

	if h != nil {
		docker.ForkJobGate = forkJobGate
	} else {
		docker.ForkJobGate = nil
	}
}

var (
	ErrPolicyDenied   = errors.New("deployment denied by policy")
	ErrJobNotVerified = errors.New("job refused: target not yet verified by compose policy")
)

// forkJobGate allows a job on <contextName>/<project> only if checkForkPolicies passed for that
// object in this process and did not deny it since.
func forkJobGate(contextName, project string) error {
	if composePolicy.Load() == nil {
		return nil
	}

	object := deploy.ObjectName(contextName, project)

	passed, known := verifiedObjects.Load(object)
	if !known {
		return fmt.Errorf("%w: %s", ErrJobNotVerified, object)
	}

	if ok, _ := passed.(bool); !ok {
		return fmt.Errorf("job refused: %w for %s", ErrPolicyDenied, object)
	}

	return nil
}

// PolicyDeniedError is returned when a deployment violates the fork policies.
type PolicyDeniedError struct {
	Object     string
	Violations []composepolicy.Violation
}

func (e *PolicyDeniedError) Error() string {
	parts := make([]string, 0, len(e.Violations))
	for _, v := range e.Violations {
		if v.Service != "" {
			parts = append(parts, fmt.Sprintf("%s: %s=%s", v.Service, v.Field, v.Value))
		} else {
			parts = append(parts, fmt.Sprintf("%s=%s", v.Field, v.Value))
		}
	}

	return fmt.Sprintf("%s for %s: %s", ErrPolicyDenied, e.Object, strings.Join(parts, "; "))
}

func (e *PolicyDeniedError) Is(target error) bool { return target == ErrPolicyDenied }

// declaredFiles are the file lists of the deploy config as declared, before the init stage
// consumes them.
type declaredFiles struct{ EnvFiles, ExternalSecretsFiles []string }

func declaredFilesOf(c *deploy.Config) declaredFiles {
	return declaredFiles{EnvFiles: slices.Clone(c.EnvFiles), ExternalSecretsFiles: slices.Clone(c.ExternalSecretsFiles)}
}

// checkForkPolicies applies the fork policies to the project just loaded, before pre-deploy
// touches anything: a violation is a terminal error of the stage. The outcome is recorded for
// forkJobGate.
//
// targets/sync is not checked here: runForkPreDeployStage checks it before pre-deploy starts,
// and pre-deploy is the only caller of loadComposeProjectHash, so a second check would only
// write a second audit line for every allowed run.
func (s *StageManager) checkForkPolicies(_ context.Context) error {
	object := s.DeployConfig.Object()

	err := s.evaluateForkPolicies(object)
	verifiedObjects.Store(object, err == nil)

	return err
}

func (s *StageManager) evaluateForkPolicies(object string) error {
	if s.Docker.SwarmMode && s.DeployConfig.Rollback != nil {
		return &PolicyDeniedError{Object: object, Violations: []composepolicy.Violation{{Field: "swarm", Value: "true"}}}
	}

	f := composePolicy.Load().Current()
	if f == nil {
		return nil
	}

	violations := f.Check(composepolicy.Input{
		Project: s.Docker.Project,
		Identity: composepolicy.Identity{
			Object: object,
			// The repository that declares name and context is the one holding the deploy config.
			Repository:   cmp.Or(s.Repository.ConfigSourceUrl, s.Repository.SourceUrl),
			Reference:    s.Repository.ResolvedReference,
			ConfigTarget: s.DeployConfig.Internal.ConfigTarget,
			Swarm:        s.Docker.SwarmMode,
		},
		Paths:          s.policyPaths(),
		DeployEnvFiles: s.externalPaths(s.forkDeclared.EnvFiles, true),
		SecretsFiles:   s.externalPaths(s.forkDeclared.ExternalSecretsFiles, false),
	})
	if len(violations) > 0 {
		return &PolicyDeniedError{Object: object, Violations: violations}
	}

	return nil
}

// externalPaths maps the files declared by the deploy config to host paths, with the same rule
// as upstream: with repository_url the "remote:" entries live in the deploy checkout (relative
// to the working directory) and the others at the root of the config source; without
// repository_url all of them live in the deploy checkout, relative to the working directory.
// keepAbs mirrors the loaders: LoadLocalDotEnv reads an absolute entry as is, while
// LoadExternalSecretsFiles joins every entry to its base path.
//
// An absolute env file entry (e.g. "remote:/abs" or "/abs") is a path inside the Doco-CD
// container, but it is checked as a host path: one that points into the internal path of the
// checkout is reported as outside it. This false positive errs toward denial.
//
// Reconciliation replays deploy configs whose lists the init stage already consumed, so on those
// runs forkDeclared holds only the remainder. That is safe: those configs passed the same rule
// earlier in this process, and their files are not read again (Internal.Environment is reused).
func (s *StageManager) externalPaths(files []string, keepAbs bool) []string {
	cfgBase := s.Repository.PathExternal
	if s.DeployConfig.RepositoryUrl != "" && s.Repository.ConfigPath != "" {
		cfgBase = s.Repository.ConfigPath
	}

	out := make([]string, 0, len(files))
	for _, f := range files {
		base, wd := cfgBase, ""
		if rest, remote := strings.CutPrefix(f, "remote:"); remote {
			f, base, wd = rest, s.Repository.PathExternal, s.DeployConfig.WorkingDirectory
		} else if cfgBase == s.Repository.PathExternal {
			wd = s.DeployConfig.WorkingDirectory // without repository_url: stage_1_init.go, second pass
		}

		// With repository_url upstream reads the local entries from the root of the config
		// source, without the working directory (loadConfigSourceFiles).
		if !keepAbs || !filepath.IsAbs(f) {
			f = filepath.Join(base, wd, f)
		}

		out = append(out, filepath.Clean(f))
	}

	return out
}

// policyPaths returns the checkouts Doco-CD reads files from: the deploy one and, if
// different, the config source one (Repository.ConfigPath is its host path).
func (s *StageManager) policyPaths() composepolicy.Paths {
	p := composepolicy.Paths{CheckoutExternal: s.Repository.PathExternal, CheckoutInternal: s.Repository.PathInternal}

	if cfg := s.Repository.ConfigPath; cfg != "" && cfg != s.Repository.PathExternal && s.Docker.DataMountPoint.Source != "" {
		if rel, err := filepath.Rel(s.Docker.DataMountPoint.Source, cfg); err == nil && rel != ".." && !strings.HasPrefix(rel, "../") {
			p.ConfigExternal = cfg
			p.ConfigInternal = filepath.Join(s.Docker.DataMountPoint.Destination, rel)
		}
	}

	return p
}

// SetRBAC registers the enforcer of the process for the targets/sync check; nil disables it.
func SetRBAC(e *rbac.Enforcer) { rbacEnforcer.Store(e) }

// checkTargetSync checks targets/sync on the project when the run carries a principal
// (poll/run and trigger_poll). Webhooks and scheduled polls carry none: their secret or the
// operator's configuration authorizes them.
func (s *StageManager) checkTargetSync(ctx context.Context, object string) error {
	p, ok := rbac.PrincipalFrom(ctx)
	if !ok {
		return nil
	}

	if rbacEnforcer.Load().Enforce(p, "targets", "sync", object) {
		return nil
	}

	return &PolicyDeniedError{Object: object, Violations: []composepolicy.Violation{{Field: "rbac", Value: "targets/sync"}}}
}

// runForkPreDeployStage is the pre-deploy stage of the deploy stage order: targets/sync, then
// upstream RunPreDeployStage, then forkPolicyGate.
//
// targets/sync comes first, before external secrets are resolved (pki-role issues real
// certificates) and before the cached-preflight "no changes" skip, so a denied caller gets a
// terminal PolicyDeniedError on every target. The denial is about the caller, not the project:
// it does not touch the forkJobGate record, so it neither marks the object as verified nor
// lets a caller without permission disable the jobs of a target that passed.
func (s *StageManager) runForkPreDeployStage(ctx context.Context, stageLog *slog.Logger) error {
	if err := s.checkTargetSync(ctx, s.DeployConfig.Object()); err != nil {
		return err
	}

	if err := s.RunPreDeployStage(ctx, stageLog); err != nil {
		return err
	}

	return s.forkPolicyGate(ctx)
}

// runForkDestroyStage is the destroy stage of the destroy stage order: targets/sync, then
// upstream RunDestroyStage. Destroy has no pre-deploy, so a run carrying a principal is checked
// here, before anything is removed.
func (s *StageManager) runForkDestroyStage(ctx context.Context, stageLog *slog.Logger) error {
	if err := s.checkTargetSync(ctx, s.DeployConfig.Object()); err != nil {
		return err
	}

	return s.RunDestroyStage(ctx, stageLog)
}

// forkPolicyGate makes sure the fork policies saw the project even when pre-deploy did not
// load it (first deploy). Without fork keys it does nothing, so the upstream path is unchanged.
func (s *StageManager) forkPolicyGate(ctx context.Context) error {
	if s.Docker.Project != nil || !s.forkPoliciesActive(ctx) {
		return nil
	}

	_, err := s.loadComposeProjectHash(ctx) // runs checkForkPolicies and leaves Project loaded

	return err
}

// forkPoliciesActive also counts a run that carries a principal while RBAC is on, so that the
// first deploy of such a run goes through checkTargetSync. Without an enforcer a principal
// changes nothing, which keeps the upstream path for any caller that sets one.
func (s *StageManager) forkPoliciesActive(ctx context.Context) bool {
	_, hasPrincipal := rbac.PrincipalFrom(ctx)

	return composePolicy.Load() != nil || (hasPrincipal && rbacEnforcer.Load() != nil) || (s.Docker.SwarmMode && s.DeployConfig.Rollback != nil)
}
