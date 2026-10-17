package stages

import (
	"cmp"

	"github.com/kimdre/doco-cd/internal/composepolicy"
	"github.com/kimdre/doco-cd/internal/config/deploy"
)

// The secret allowlist (spec 4, innesto 1) is checked right before each ResolveSecretReferences of
// a target: in pre-deploy (here), in scheduled jobs (docker.ForkSecretGate, see forkSecretGate)
// and when the rollback redeploys the last good (loadLastGoodFiles). The references checked are
// the encoded ones, the exact map the provider receives.

// forkIdentity is the identity pre-deploy presents to the compose policy: the repository that
// declares name and context is the one holding the deploy config.
func (s *StageManager) forkIdentity(object string) composepolicy.Identity {
	return composepolicy.Identity{
		Object:       object,
		Repository:   cmp.Or(s.Repository.ConfigSourceUrl, s.Repository.SourceUrl),
		Reference:    s.Repository.ResolvedReference,
		ConfigTarget: s.DeployConfig.Internal.ConfigTarget,
		Swarm:        s.Docker.SwarmMode,
	}
}

// lastGoodIdentity is the identity of the last good of the target: the saved config names the
// object, the current run the repository, the reference and the config target.
func (s *StageManager) lastGoodIdentity(cfg *deploy.Config) composepolicy.Identity {
	return composepolicy.Identity{
		Object:       cfg.Object(),
		Repository:   cmp.Or(s.Repository.ConfigSourceUrl, s.Repository.SourceUrl),
		Reference:    s.Repository.ResolvedReference,
		ConfigTarget: s.DeployConfig.Internal.ConfigTarget,
	}
}

// checkForkSecrets applies the secret allowlist of the policy in force to the encoded references
// of the target of this run. record also marks the object as denied for the job gate: pre-deploy
// does, the rollback does not (see checkLastGoodPolicy).
func (s *StageManager) checkForkSecrets(encoded map[string]string, record bool) error {
	return s.checkForkSecretsAs(s.forkIdentity(s.DeployConfig.Object()), encoded, record)
}

func (s *StageManager) checkForkSecretsAs(id composepolicy.Identity, encoded map[string]string, record bool) error {
	f := composePolicy.Load().Current()
	if f == nil || s.SecretProvider == nil {
		return nil
	}

	violations := f.CheckSecrets(id, s.SecretProvider.Name(), encoded)
	if len(violations) == 0 {
		return nil
	}

	if record {
		verifiedObjects.Store(id.Object, false)
	}

	return &PolicyDeniedError{Object: id.Object, Violations: violations}
}
