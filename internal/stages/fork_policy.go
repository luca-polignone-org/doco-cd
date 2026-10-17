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
	"sync/atomic"

	"github.com/kimdre/doco-cd/internal/composepolicy"
	"github.com/kimdre/doco-cd/internal/config/deploy"
)

// Scheduled jobs (internal/docker/compose_scheduled_run.go) do not go through this check, and
// do not need to: loadComposeScheduledProjectAll (~line 345) loads the compose project from the
// working directory and config revision recorded in the labels of containers that a deploy
// created, i.e. the per-revision artifact of a revision that already passed pre-deploy. A denied
// revision never creates containers, so no label can point a job at it. RunComposeScheduledContainer
// restarts or starts those existing containers; RunComposeOneOffFromServiceDefinitionWithOptions
// (~line 166) creates a one-off container from that same deployed revision. What stays outside the
// check is a policy tightened after that revision was deployed.

var composePolicy atomic.Pointer[composepolicy.Holder]

// SetComposePolicy registers the compose policy of the process; nil disables it.
func SetComposePolicy(h *composepolicy.Holder) { composePolicy.Store(h) }

var ErrPolicyDenied = errors.New("deployment denied by policy")

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
// touches anything: a violation is a terminal error of the stage.
func (s *StageManager) checkForkPolicies(ctx context.Context) error {
	object := s.DeployConfig.Object()

	if s.Docker.SwarmMode && s.DeployConfig.Rollback != nil {
		return &PolicyDeniedError{Object: object, Violations: []composepolicy.Violation{{Field: "swarm", Value: "true"}}}
	}

	if err := s.checkTargetSync(ctx, object); err != nil {
		return err
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

// checkTargetSync is where the RBAC check of targets/sync will go; for now it allows everything.
func (s *StageManager) checkTargetSync(context.Context, string) error { return nil }

// runForkPreDeployStage is the pre-deploy stage of the deploy stage order: upstream
// RunPreDeployStage, then forkPolicyGate.
func (s *StageManager) runForkPreDeployStage(ctx context.Context, stageLog *slog.Logger) error {
	if err := s.RunPreDeployStage(ctx, stageLog); err != nil {
		return err
	}

	return s.forkPolicyGate(ctx)
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

func (s *StageManager) forkPoliciesActive(_ context.Context) bool {
	return composePolicy.Load() != nil || (s.Docker.SwarmMode && s.DeployConfig.Rollback != nil)
}
