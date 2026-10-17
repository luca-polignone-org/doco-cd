package stages

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/hooks"
	"github.com/kimdre/doco-cd/internal/rbac"
	"github.com/kimdre/doco-cd/internal/rollback"
)

// outcomeOutbox is the outbox of the process, registered at startup when HOOK_OUTCOME_URL is set.
// Unset, every emission is a no-op: no event, no file under <data>/fork/outbox.
var outcomeOutbox atomic.Pointer[hooks.Outbox]

// SetOutbox registers the outbox of the outcome events; nil disables them.
func SetOutbox(o *hooks.Outbox) { outcomeOutbox.Store(o) }

// outcomeErrorRunes is the length the error of an event is truncated to (spec, "hooks.outcome").
const outcomeErrorRunes = 2000

// triggerAPI is the trigger of a run that carries a principal: poll/run and trigger_poll.
const triggerAPI = "api"

// EmitOutcome enqueues ev in the registered outbox, with the version of this process and the error
// truncated. It is fire-and-forget: an enqueue error is logged and never reaches the deploy.
func EmitOutcome(log *slog.Logger, ev hooks.Event) {
	o := outcomeOutbox.Load()
	if o == nil {
		return
	}

	ev.Error = truncateRunes(ev.Error, outcomeErrorRunes)
	ev.DocoCDVersion = app.Version

	if err := o.Enqueue(ev); err != nil {
		log.Error("failed to enqueue outcome event", slog.String("object", ev.Object),
			slog.String("outcome", string(ev.Outcome)), slog.Any("error", err))
	}
}

// EmitUnblocked emits unblocked for object, released by the unblock action.
func EmitUnblocked(log *slog.Logger, object string) {
	ev := hooks.NewEvent(object)
	ev.Outcome = hooks.OutcomeUnblocked

	EmitOutcome(log, ev)
}

type outcomeOpt func(*hooks.Event)

func withFailure(stage StageName, errText string) outcomeOpt {
	return func(e *hooks.Event) { e.Stage, e.Error = string(stage), errText }
}

func withImages(images []rollback.ServiceImage) outcomeOpt {
	return func(e *hooks.Event) {
		for _, img := range images {
			e.Images = append(e.Images, hooks.ImageOut{
				Service: img.Service, Image: img.Image, ImageID: img.ImageID, RepoDigests: img.RepoDigests,
			})
		}
	}
}

// withRollback reports the last good and the pre_rollback decision of a rollback flow. A target
// refused before pre_rollback was asked (Swarm, OCI, repository_url) has no decision, and the
// rollback object is omitted.
func withRollback(out rollback.Outcome) outcomeOpt {
	return func(e *hooks.Event) {
		if out.LastGood != nil {
			e.LastGoodCommit = out.LastGood.CommitSHA
		}

		if out.Decision != "" {
			e.Rollback = &hooks.RollbackOut{Decision: string(out.Decision), ToCommit: out.ToCommit}
		}
	}
}

// emitOutcome emits an event of the target with the fields every event of a run carries; opts add
// the ones of the outcome. The error goes through redactSecrets.
func (s *StageManager) emitOutcome(ctx context.Context, kind hooks.Outcome, opts ...outcomeOpt) {
	if outcomeOutbox.Load() == nil {
		return
	}

	ev := hooks.NewEvent(s.DeployConfig.Object())
	ev.Outcome = kind
	ev.Repository = s.Repository.Name
	ev.Ref = s.Repository.ResolvedReference
	ev.Commit = cmp.Or(s.forkDeployedCommit, strings.TrimSpace(s.Repository.Revision))
	ev.Trigger = string(s.JobTrigger)

	if _, ok := rbac.PrincipalFrom(ctx); ok {
		ev.Trigger = triggerAPI
	}

	if s.Stages != nil && s.Stages.Init != nil && !s.Stages.Init.StartedAt.IsZero() {
		ev.DurationMS = time.Since(s.Stages.Init.StartedAt).Milliseconds()
	}

	for _, opt := range opts {
		opt(&ev)
	}

	ev.Error = s.redactSecrets(ev.Error)

	EmitOutcome(s.Log, ev)
}

// emitSuccessOutcome emits destroyed or deployed at the end of a successful run. The images are
// those recordLastGood collected; without them (rollback disabled, or the collection failed) they
// are read here. Swarm services have no compose containers to read them from.
func (s *StageManager) emitSuccessOutcome(ctx context.Context, images []rollback.ServiceImage) {
	if outcomeOutbox.Load() == nil {
		return
	}

	if s.DeployConfig.Destroy.Enabled {
		s.emitOutcome(ctx, hooks.OutcomeDestroyed)
		return
	}

	if images == nil && !s.Docker.SwarmMode {
		var err error
		if images, err = s.serviceImages(ctx); err != nil {
			s.Log.Warn("failed to collect images for the outcome event", slog.Any("error", err))
		}
	}

	s.emitOutcome(ctx, hooks.OutcomeDeployed, withImages(images))
}

// emittedPolicyDenials holds the (object, commit, violations) already reported as policy_denied in
// this process, so the next poll of the same commit does not emit it again. It is in memory only:
// one more event after a restart is accepted.
var emittedPolicyDenials sync.Map

// emitFailureOutcome emits the outcome of a failure the rollback flow did not handle: policy_denied
// once per (object, commit, violations), failed otherwise.
func (s *StageManager) emitFailureOutcome(ctx context.Context, stageName StageName, err error) {
	if outcomeOutbox.Load() == nil {
		return
	}

	var denied *PolicyDeniedError
	if !errors.As(err, &denied) {
		s.emitOutcome(ctx, hooks.OutcomeFailed, withFailure(stageName, err.Error()))
		return
	}

	violations := make([]hooks.ViolationOut, 0, len(denied.Violations))
	for _, v := range denied.Violations {
		violations = append(violations, hooks.ViolationOut{Service: v.Service, Field: v.Field, Value: v.Value})
	}

	encoded, _ := json.Marshal(violations) //nolint:errchkjson // a slice of string fields always encodes
	sum := sha256.Sum256(encoded)
	key := s.DeployConfig.Object() + "\x00" + strings.TrimSpace(s.Repository.Revision) + "\x00" + hex.EncodeToString(sum[:])

	if _, seen := emittedPolicyDenials.LoadOrStore(key, struct{}{}); seen {
		return
	}

	s.emitOutcome(ctx, hooks.OutcomePolicyDenied, withFailure(stageName, err.Error()),
		func(e *hooks.Event) { e.Violations = violations })
}

// emitRollbackOutcome emits the outcome of a rollback flow. A flow cut by shutdown left the
// failure failed with the context error, and emits nothing, as a cancelled deploy does.
func (s *StageManager) emitRollbackOutcome(ctx context.Context, f rollback.Failure, out rollback.Outcome) {
	opts := []outcomeOpt{withFailure(StageName(f.Stage), f.Error), withRollback(out)}

	switch out.State {
	case rollback.StateRolledBack:
		s.emitOutcome(ctx, hooks.OutcomeRolledBack, append(opts, withImages(out.LastGood.Images))...)
	case rollback.StateBlocked:
		s.emitOutcome(ctx, hooks.OutcomeRollbackBlocked, opts...)
	case rollback.StateRollbackFailed:
		s.emitOutcome(ctx, hooks.OutcomeRollbackFailed, opts...)
	default:
		if out.Err == nil {
			s.emitOutcome(ctx, hooks.OutcomeFailed, opts...)
		}
	}
}

// redactedSecret replaces a secret value in an error, as the notifications do.
const redactedSecret = "[REDACTED]"

// minRedactedEnvRunes is the shortest value of Internal.Environment, other than a resolved external
// secret, that redactSecrets replaces: shorter values (ports, flags, booleans) are not credentials,
// and replacing them would make the error unreadable.
const minRedactedEnvRunes = 8

// redactSecrets replaces in v the values of the resolved external secrets and the other values of
// Internal.Environment (dotenv files, possibly SOPS-decrypted) of at least minRedactedEnvRunes.
// Upstream redacts neither in the error of a failed deploy; the fork does it before the error
// reaches the outcome event, the failure store and pre_rollback. Longer values go first, so a value
// that contains another is replaced whole.
func (s *StageManager) redactSecrets(v string) string {
	if v == "" {
		return v
	}

	var values []string

	for k, val := range s.DeployConfig.Internal.Environment {
		_, secret := s.DeployConfig.ExternalSecrets[k]
		if val != "" && (secret || len([]rune(val)) >= minRedactedEnvRunes) {
			values = append(values, val)
		}
	}

	slices.SortFunc(values, func(a, b string) int { return cmp.Compare(len(b), len(a)) })

	for _, val := range values {
		v = strings.ReplaceAll(v, val, redactedSecret)
	}

	return v
}
