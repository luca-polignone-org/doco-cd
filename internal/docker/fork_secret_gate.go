package docker

import (
	"context"
	"fmt"

	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/secretprovider"
)

// ForkSecretGate is the fork secret allowlist gate of jobs that resolve external secrets again
// (prepareComposeScheduledDeployConfig). nil allows everything, so upstream behaviour is
// unchanged; internal/stages sets it with the compose policy, at startup.
var ForkSecretGate func(object, provider string, refs map[string]string) error

type forkJobObjectKey struct{}

// withForkJobObject records the object the job gate checked (checkForkJobGate), so the secret
// gate applies the allowlist of that object and not of whatever the reloaded config declares.
func withForkJobObject(ctx context.Context, object string) context.Context {
	return context.WithValue(ctx, forkJobObjectKey{}, object)
}

// checkForkSecretGate applies ForkSecretGate to the encoded references of a scheduled job. With the
// gate set, the object the job gate checked must be in the context (loadComposeScheduledProjectAll
// puts it there): a context without it, or a reloaded deploy config naming another object, is
// refused before the gate, fail closed.
func checkForkSecretGate(ctx context.Context, cfg *deploy.Config, p secretprovider.SecretProvider, refs map[string]string) error {
	gate := ForkSecretGate
	if gate == nil {
		return nil
	}

	object := cfg.Object()

	checked, ok := ctx.Value(forkJobObjectKey{}).(string)
	if !ok {
		return fmt.Errorf("compose project %s: secrets refused, no job gate object in the context", cfg.Name)
	}

	if checked != object {
		return fmt.Errorf("compose project %s: deploy config names %s, job gate checked %s", cfg.Name, object, checked)
	}

	if err := gate(object, p.Name(), refs); err != nil {
		return fmt.Errorf("compose project %s: %w", cfg.Name, err)
	}

	return nil
}
