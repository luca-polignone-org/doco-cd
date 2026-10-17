package stages

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"

	"github.com/kimdre/doco-cd/internal/composepolicy"
	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/hooks"
	"github.com/kimdre/doco-cd/internal/rollback"
	secrettypes "github.com/kimdre/doco-cd/internal/secretprovider/types"
	"github.com/kimdre/doco-cd/internal/selfupdate"
)

// The tests in this file register the process-wide outbox, so none of them calls t.Parallel: the
// parallel tests of the package only start once every sequential one has finished.

// startTestOutbox registers an outbox on a temporary directory and returns that directory. Its
// worker never runs, so every enqueued event stays there as a file.
func startTestOutbox(t *testing.T) string {
	t.Helper()

	dir := filepath.Join(t.TempDir(), "outbox")

	o, err := hooks.NewOutbox(hooks.OutboxConfig{Dir: dir, URL: "http://127.0.0.1:1/outcome", Signer: hooks.Signer{Secret: []byte("s")}})
	if err != nil {
		t.Fatal(err)
	}

	SetOutbox(o)
	t.Cleanup(func() {
		SetOutbox(nil)
		emittedPolicyDenials.Clear()
	})

	return dir
}

// readOutcomes returns the events in the outbox directory, in emission order (UUIDv7 event IDs).
func readOutcomes(t *testing.T, dir string) []hooks.Event {
	t.Helper()

	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		t.Fatal(err)
	}

	events := make([]hooks.Event, 0, len(paths))

	for _, path := range paths {
		data, err := os.ReadFile(path) //nolint:gosec // test files under t.TempDir()
		if err != nil {
			t.Fatal(err)
		}

		var e struct {
			Event hooks.Event `json:"event"`
		}

		if err := json.Unmarshal(data, &e); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}

		events = append(events, e.Event)
	}

	slices.SortFunc(events, func(a, b hooks.Event) int { return strings.Compare(a.EventID, b.EventID) })

	return events
}

// requireOneOutcome fails unless the outbox holds exactly one event, of kind, and returns it.
func requireOneOutcome(t *testing.T, dir string, kind hooks.Outcome) hooks.Event {
	t.Helper()

	events := readOutcomes(t, dir)
	if len(events) != 1 || events[0].Outcome != kind {
		t.Fatalf("events %+v, want exactly one %s", events, kind)
	}

	if events[0].Schema != hooks.OutcomeSchema || events[0].DocoCDVersion != app.Version {
		t.Fatalf("event %+v lacks schema or version", events[0])
	}

	return events[0]
}

func requireNoOutcome(t *testing.T, dir string) {
	t.Helper()

	if events := readOutcomes(t, dir); len(events) != 0 {
		t.Fatalf("events %+v, want none", events)
	}
}

// Without an outbox nothing is emitted and nothing is written: Docker.Cmd is nil, so reaching
// serviceImages would panic.
func TestOutcomeWithoutOutbox(t *testing.T) {
	SetOutbox(nil)

	sm, data := newForkFailureManager(t, nil)

	sm.emitSuccessOutcome(t.Context(), nil)
	sm.emitOutcome(t.Context(), hooks.OutcomeDeployed)
	sm.emitFailureOutcome(t.Context(), StagePreDeploy, &PolicyDeniedError{Object: "default/web"})

	if EmitOutcome(sm.Log, hooks.NewEvent("default/web")) {
		t.Fatal("EmitOutcome() without an outbox reported an enqueued event")
	}

	_ = sm.handleStageFailure(t.Context(), StageDeploy, sm.Log, errors.New("boom"))

	assertNoForkDir(t, data)
}

func TestOutcomeDeployed(t *testing.T) {
	newManager := func(t *testing.T, apiClient *lastGoodClient) *StageManager {
		t.Helper()

		sm, _ := newLastGoodManager(t, nil, apiClient)
		sm.Repository.Name = "org/repo"
		sm.Repository.ResolvedReference = "refs/heads/main"
		sm.forkDeployedCommit = "c1"
		sm.Stages.Init.StartedAt = time.Now().Add(-time.Second)

		return sm
	}

	t.Run("images read from the containers without rollback", func(t *testing.T) {
		dir := startTestOutbox(t)
		apiClient := &lastGoodClient{
			containers: []container.Summary{serviceContainer("/web-app-1", "app", "alpine:3.22", "sha256:app", nil)},
			digests:    map[string][]string{"sha256:app": {"alpine@sha256:a"}},
		}
		sm := newManager(t, apiClient)

		sm.emitSuccessOutcome(t.Context(), nil)

		ev := requireOneOutcome(t, dir, hooks.OutcomeDeployed)
		want := []hooks.ImageOut{{Service: "app", Image: "alpine:3.22", ImageID: "sha256:app", RepoDigests: []string{"alpine@sha256:a"}}}

		if ev.Object != "default/web" || ev.Context != "default" || ev.Project != "web" || ev.Repository != "org/repo" ||
			ev.Ref != "refs/heads/main" || ev.Commit != "c1" || ev.Trigger != "webhook" || ev.DurationMS < 1000 ||
			!reflect.DeepEqual(ev.Images, want) || ev.Stage != "" || ev.Error != "" || ev.Rollback != nil {
			t.Fatalf("event %+v", ev)
		}
	})

	t.Run("images recorded by the last good are reused", func(t *testing.T) {
		dir := startTestOutbox(t)
		sm := newManager(t, &lastGoodClient{listErr: errors.New("must not be called")})

		sm.emitSuccessOutcome(t.Context(), []rollback.ServiceImage{{Service: "app", Image: "a", ImageID: "sha256:x"}})

		if ev := requireOneOutcome(t, dir, hooks.OutcomeDeployed); len(ev.Images) != 1 || ev.Images[0].ImageID != "sha256:x" {
			t.Fatalf("event %+v", ev)
		}
	})

	t.Run("an image error still emits deployed", func(t *testing.T) {
		dir := startTestOutbox(t)
		sm := newManager(t, &lastGoodClient{listErr: errors.New("daemon down")})
		logs := captureLog(sm)

		sm.emitSuccessOutcome(t.Context(), nil)

		if ev := requireOneOutcome(t, dir, hooks.OutcomeDeployed); ev.Images != nil {
			t.Fatalf("event %+v", ev)
		}

		if !strings.Contains(logs.String(), "failed to collect images for the outcome event") {
			t.Fatalf("log %q", logs.String())
		}
	})

	t.Run("swarm has no images", func(t *testing.T) {
		dir := startTestOutbox(t)
		sm := newManager(t, &lastGoodClient{listErr: errors.New("must not be called")})
		sm.Docker.SwarmMode = true

		sm.emitSuccessOutcome(t.Context(), nil)

		if ev := requireOneOutcome(t, dir, hooks.OutcomeDeployed); ev.Images != nil {
			t.Fatalf("event %+v", ev)
		}
	})

	t.Run("a poll/run or trigger_poll run is api, the revision is the commit before the deploy stage", func(t *testing.T) {
		dir := startTestOutbox(t)
		sm := newManager(t, &lastGoodClient{})
		sm.forkDeployedCommit = ""
		sm.Stages.Init.StartedAt = time.Time{}
		sm.JobTrigger = JobTriggerPoll

		sm.emitSuccessOutcome(hooks.WithAPITrigger(t.Context()), []rollback.ServiceImage{})

		if ev := requireOneOutcome(t, dir, hooks.OutcomeDeployed); ev.Trigger != "api" || ev.Commit != "sha256:abc" || ev.DurationMS != 0 {
			t.Fatalf("event %+v", ev)
		}
	})
}

func TestOutcomeDestroyed(t *testing.T) {
	dir := startTestOutbox(t)
	sm, _ := newForkFailureManager(t, nil)
	sm.DeployConfig.Destroy.Enabled = true

	sm.emitSuccessOutcome(t.Context(), nil)

	if ev := requireOneOutcome(t, dir, hooks.OutcomeDestroyed); ev.Commit != "c1" || ev.Images != nil {
		t.Fatalf("event %+v", ev)
	}
}

// A failure the rollback flow does not take (rollback disabled, a stage that records no failure, a
// destroy) emits failed, with the error truncated to 2000 runes.
func TestOutcomeFailed(t *testing.T) {
	cases := map[string]struct {
		rb      *deploy.RollbackConfig
		stage   StageName
		destroy bool
	}{
		"rollback disabled":            {stage: StageDeploy},
		"pre-deploy with rollback":     {rb: &deploy.RollbackConfig{Enabled: true}, stage: StagePreDeploy},
		"destroy with rollback":        {rb: &deploy.RollbackConfig{Enabled: true}, stage: StageDestroy, destroy: true},
		"post-deploy without rollback": {stage: StagePostDeploy},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dir := startTestOutbox(t)
			sm, _ := newForkFailureManager(t, tc.rb)
			sm.DeployConfig.Destroy.Enabled = tc.destroy

			_ = sm.handleStageFailure(t.Context(), tc.stage, sm.Log, errors.New(strings.Repeat("é", 2100)))

			ev := requireOneOutcome(t, dir, hooks.OutcomeFailed)
			if ev.Stage != string(tc.stage) || len([]rune(ev.Error)) != 2000 || ev.Commit != "c1" || ev.Rollback != nil || ev.LastGoodCommit != "" {
				t.Fatalf("event stage %q, error of %d runes, commit %q, rollback %+v", ev.Stage, len([]rune(ev.Error)), ev.Commit, ev.Rollback)
			}
		})
	}
}

// A cancelled run, a handover and a job that gave up waiting for the target lock emit nothing.
func TestOutcomeNotEmittedOnCancelOrHandover(t *testing.T) {
	for name, err := range map[string]error{
		"shutdown":     fmt.Errorf("deploy: %w", context.Canceled),
		"handover":     fmt.Errorf("deploy: %w", selfupdate.ErrHandover),
		"lock waiting": fmt.Errorf("%w for default/web: %w", errForkTargetWait, context.Canceled),
	} {
		t.Run(name, func(t *testing.T) {
			dir := startTestOutbox(t)
			sm, _ := newForkFailureManager(t, &deploy.RollbackConfig{Enabled: true})

			_ = sm.handleStageFailure(t.Context(), StageDeploy, sm.Log, err)

			requireNoOutcome(t, dir)
		})
	}
}

func TestOutcomePolicyDenied(t *testing.T) {
	dir := startTestOutbox(t)
	sm, _ := newForkFailureManager(t, nil)

	privileged := &PolicyDeniedError{Object: "default/web", Violations: []composepolicy.Violation{{Service: "app", Field: "privileged", Value: "true"}}}

	// Two polls of the same commit with the same violations: one event.
	_ = sm.handleStageFailure(t.Context(), StagePreDeploy, sm.Log, privileged)
	_ = sm.handleStageFailure(t.Context(), StagePreDeploy, sm.Log, fmt.Errorf("pre-deploy: %w", privileged))

	ev := requireOneOutcome(t, dir, hooks.OutcomePolicyDenied)
	if !reflect.DeepEqual(ev.Violations, []hooks.ViolationOut{{Service: "app", Field: "privileged", Value: "true"}}) ||
		ev.Stage != string(StagePreDeploy) || !strings.Contains(ev.Error, "app: privileged=true") || ev.Commit != "c1" {
		t.Fatalf("event %+v", ev)
	}

	// Same commit, other violations: a second event. A new commit with the first ones: a third.
	_ = sm.handleStageFailure(t.Context(), StagePreDeploy, sm.Log,
		&PolicyDeniedError{Object: "default/web", Violations: []composepolicy.Violation{{Field: "rbac", Value: "targets/sync"}}})

	sm.Repository.Revision = "c2"
	_ = sm.handleStageFailure(t.Context(), StagePreDeploy, sm.Log, privileged)

	events := readOutcomes(t, dir)
	if len(events) != 3 || events[1].Violations[0].Field != "rbac" || events[1].Commit != "c1" || events[2].Commit != "c2" {
		t.Fatalf("events %+v", events)
	}

	for _, e := range events {
		if e.Outcome != hooks.OutcomePolicyDenied {
			t.Fatalf("event %+v", e)
		}
	}
}

// The rollback flow emits one event per final state, with the last good, the decision and, for
// rolled_back, the images of the last good.
func TestOutcomeRollbackStates(t *testing.T) {
	r := newRollbackRepo(t)
	good := rollbackLastGood(r, "good", rollbackConfig)

	run := func(t *testing.T, withLastGood bool, setup func(sm *StageManager)) hooks.Event {
		t.Helper()

		dir := startTestOutbox(t)
		sm := newRollbackManager(t, r, &lastGoodClient{})

		if withLastGood {
			if err := rollback.StoreFor(sm.AppConfig.DataMountPath).PutLastGood(good); err != nil {
				t.Fatal(err)
			}
		}

		setup(sm)

		_ = sm.handleStageFailure(t.Context(), StageDeploy, sm.Log, errors.New("boom"))

		events := readOutcomes(t, dir)
		if len(events) != 1 {
			t.Fatalf("events %+v, want one", events)
		}

		ev := events[0]
		if ev.Stage != "deploy" || ev.Error != "boom" || ev.Commit != "failed-commit" || ev.Object != "default/web" {
			t.Fatalf("event %+v", ev)
		}

		return ev
	}

	t.Run("rolled_back", func(t *testing.T) {
		ev := run(t, true, func(*StageManager) {
			startPreRollback(t, "proceed")
			stubDeployLastGood(t, nil)
		})

		wantImages := []hooks.ImageOut{
			{Service: "app", Image: "alpine:3.22", ImageID: "sha256:app", RepoDigests: []string{"alpine@sha256:a"}},
			{Service: "extra", Image: "busybox:1", ImageID: "sha256:extra", RepoDigests: []string{"busybox@sha256:b"}},
		}

		if ev.Outcome != hooks.OutcomeRolledBack || ev.LastGoodCommit != good.CommitSHA ||
			!reflect.DeepEqual(ev.Rollback, &hooks.RollbackOut{Decision: "proceed", ToCommit: good.CommitSHA}) ||
			!reflect.DeepEqual(ev.Images, wantImages) {
			t.Fatalf("event %+v", ev)
		}
	})

	t.Run("rollback_blocked", func(t *testing.T) {
		ev := run(t, true, func(*StageManager) { startPreRollback(t, "block") })

		if ev.Outcome != hooks.OutcomeRollbackBlocked || ev.LastGoodCommit != good.CommitSHA ||
			!reflect.DeepEqual(ev.Rollback, &hooks.RollbackOut{Decision: "block"}) || ev.Images != nil {
			t.Fatalf("event %+v", ev)
		}
	})

	t.Run("rollback_failed after proceed", func(t *testing.T) {
		ev := run(t, true, func(*StageManager) {
			startPreRollback(t, "proceed")
			stubDeployLastGood(t, errors.New("unhealthy"))
		})

		if ev.Outcome != hooks.OutcomeRollbackFailed || ev.LastGoodCommit != good.CommitSHA ||
			!reflect.DeepEqual(ev.Rollback, &hooks.RollbackOut{Decision: "proceed", ToCommit: good.CommitSHA}) || ev.Images != nil {
			t.Fatalf("event %+v", ev)
		}
	})

	// Swarm, OCI and repository_url end rollback_failed before pre_rollback: no decision.
	t.Run("rollback_failed unsupported", func(t *testing.T) {
		ev := run(t, true, func(sm *StageManager) { sm.DeployConfig.RepositoryUrl = "https://example.com/config.git" })

		if ev.Outcome != hooks.OutcomeRollbackFailed || ev.LastGoodCommit != good.CommitSHA || ev.Rollback != nil {
			t.Fatalf("event %+v", ev)
		}
	})

	t.Run("no last good is failed", func(t *testing.T) {
		ev := run(t, false, func(*StageManager) { startPreRollback(t, "proceed") })

		if ev.Outcome != hooks.OutcomeFailed || ev.LastGoodCommit != "" || ev.Rollback != nil {
			t.Fatalf("event %+v", ev)
		}
	})

	t.Run("shutdown during pre_rollback emits nothing", func(t *testing.T) {
		dir := startTestOutbox(t)
		sm := newRollbackManager(t, r, &lastGoodClient{})
		startPreRollback(t, "proceed")

		if err := rollback.StoreFor(sm.AppConfig.DataMountPath).PutLastGood(good); err != nil {
			t.Fatal(err)
		}

		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		_ = sm.handleStageFailure(ctx, StageDeploy, sm.Log, errors.New("boom"))

		requireNoOutcome(t, dir)

		if f := diskFailure(t, sm); f.State != rollback.StateFailed {
			t.Fatalf("failure %+v", f)
		}
	})
}

// A new commit on a poisoned target emits unblocked once, before it is deployed; a failed reset
// emits nothing.
func TestOutcomeUnblockedByNewCommit(t *testing.T) {
	stageLog := slog.New(slog.DiscardHandler)

	t.Run("new commit", func(t *testing.T) {
		dir := startTestOutbox(t)
		sm, _ := newForkFailureManager(t, &deploy.RollbackConfig{Enabled: true})
		sm.Repository.Revision = "c2"
		putDiskFailure(t, sm, rollback.Failure{Object: "default/web", CommitSHA: "c1", Stage: "deploy", State: rollback.StateBlocked, Decision: "block"})

		// pre-deploy and the deploy stage both check it: only the first one resets and emits.
		for range 2 {
			if sm.rollbackPoisoned(t.Context(), "c2", stageLog) {
				t.Fatal("a new commit must deploy")
			}
		}

		if ev := requireOneOutcome(t, dir, hooks.OutcomeUnblocked); ev.Commit != "c2" || ev.Object != "default/web" {
			t.Fatalf("event %+v", ev)
		}
	})

	t.Run("same commit stays poisoned and silent", func(t *testing.T) {
		dir := startTestOutbox(t)
		sm, _ := newForkFailureManager(t, &deploy.RollbackConfig{Enabled: true})
		putDiskFailure(t, sm, rollback.Failure{Object: "default/web", CommitSHA: "c1", State: rollback.StateRolledBack})

		if !sm.rollbackPoisoned(t.Context(), "c1", stageLog) {
			t.Fatal("the commit must not be redeployed")
		}

		requireNoOutcome(t, dir)
	})

	t.Run("reset error", func(t *testing.T) {
		dir := startTestOutbox(t)
		sm, data := newForkFailureManager(t, &deploy.RollbackConfig{Enabled: true})

		path := filepath.Join(data, "fork", "failures", "default", "web.json")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(path, []byte(`{"version":1,"object":"bad","commit_sha":"c1","state":"blocked"}`), 0o600); err != nil {
			t.Fatal(err)
		}

		_ = sm.rollbackPoisoned(t.Context(), "c2", stageLog)

		requireNoOutcome(t, dir)
	})
}

// The unblock action emits unblocked with the repository and commit of the record it rewrote.
func TestUnblockTargetEmitsUnblocked(t *testing.T) {
	dir := startTestOutbox(t)
	data := t.TempDir()

	if err := rollback.StoreFor(data).PutFailure(rollback.Failure{
		Object: "noi/poli-gate", Repository: "org/repo", CommitSHA: " c2 ", Stage: "deploy", State: rollback.StateBlocked, Decision: "block",
	}); err != nil {
		t.Fatal(err)
	}

	for range 2 { // the second call finds nothing blocked and emits nothing
		if _, err := UnblockTarget(t.Context(), data, "noi/poli-gate"); err != nil {
			t.Fatal(err)
		}
	}

	ev := requireOneOutcome(t, dir, hooks.OutcomeUnblocked)
	if ev.Object != "noi/poli-gate" || ev.Context != "noi" || ev.Project != "poli-gate" || ev.Repository != "org/repo" || ev.Commit != "c2" {
		t.Fatalf("event %+v", ev)
	}
}

// breakOutbox replaces the outbox directory with a regular file, so writing an event under it fails
// with ENOTDIR whatever the uid; the returned function restores it.
func breakOutbox(t *testing.T, dir string) func() {
	t.Helper()

	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(dir, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	return func() {
		if err := os.Remove(dir); err != nil {
			t.Fatal(err)
		}
	}
}

// An enqueue error is logged, reported as false and never reaches the deploy.
func TestEmitOutcomeEnqueueError(t *testing.T) {
	dir := startTestOutbox(t)
	breakOutbox(t, dir)

	var buf bytes.Buffer

	ev := hooks.NewEvent("default/web")
	ev.Outcome = hooks.OutcomeUnblocked

	if EmitOutcome(slog.New(slog.NewTextHandler(&buf, nil)), ev) {
		t.Fatal("EmitOutcome() reported an event the outbox could not write")
	}

	if !strings.Contains(buf.String(), "failed to enqueue outcome event") || !strings.Contains(buf.String(), "outcome=unblocked") {
		t.Fatalf("log %q", buf.String())
	}
}

// A policy_denied that could not be enqueued is not marked as emitted: the next poll emits it.
func TestOutcomePolicyDeniedRetriedAfterEnqueueError(t *testing.T) {
	dir := startTestOutbox(t)
	sm, _ := newForkFailureManager(t, nil)
	denied := &PolicyDeniedError{Object: "default/web", Violations: []composepolicy.Violation{{Service: "app", Field: "privileged", Value: "true"}}}

	restore := breakOutbox(t, dir)

	_ = sm.handleStageFailure(t.Context(), StagePreDeploy, sm.Log, denied)

	restore()

	_ = sm.handleStageFailure(t.Context(), StagePreDeploy, sm.Log, denied)

	requireOneOutcome(t, dir, hooks.OutcomePolicyDenied)
}

// Resolved secrets and long environment values never reach the event or the failure store (whose
// error pre_rollback sends), raw or escaped; short values are kept.
func TestOutcomeRedactsSecrets(t *testing.T) {
	const tricky = "pa\"ss\\wo\nrd"

	errText := "render: token=s3cr3t-e2e pin=42 dotenv=dotenv-password-value port=8080 prefix=s3cr3t-e2e-long" +
		" quoted=" + strconv.Quote(tricky) + " json=" + string(mustJSON(t, tricky)) + " raw=" + tricky

	setup := func(sm *StageManager) {
		sm.DeployConfig.ExternalSecrets = map[string]secrettypes.ExternalSecretRef{
			"TOKEN": {LegacyRef: "ref"}, "PIN": {LegacyRef: "ref"}, "TRICKY": {LegacyRef: "ref"},
		}
		sm.DeployConfig.Internal.Environment = map[string]string{
			"TOKEN": "s3cr3t-e2e", "PIN": "42", "DOTENV": "dotenv-password-value", "PORT": "8080",
			"LONGER": "s3cr3t-e2e-long", "EMPTY": "", "TRICKY": tricky,
		}
	}

	// PIN is a secret under the 4-rune floor, PORT an env value under the 8-rune one: both stay.
	const want = "render: token=[REDACTED] pin=42 dotenv=[REDACTED] port=8080 prefix=[REDACTED]" +
		` quoted="[REDACTED]" json="[REDACTED]" raw=[REDACTED]`

	t.Run("failed event", func(t *testing.T) {
		dir := startTestOutbox(t)
		sm, _ := newForkFailureManager(t, nil)
		setup(sm)

		_ = sm.handleStageFailure(t.Context(), StagePreDeploy, sm.Log, errors.New(errText))

		if ev := requireOneOutcome(t, dir, hooks.OutcomeFailed); ev.Error != want {
			t.Fatalf("error %q, want %q", ev.Error, want)
		}
	})

	t.Run("failure store", func(t *testing.T) {
		dir := startTestOutbox(t)
		sm, data := newForkFailureManager(t, &deploy.RollbackConfig{Enabled: true})
		setup(sm)

		_ = sm.handleStageFailure(t.Context(), StageDeploy, sm.Log, errors.New(errText))

		if f := diskFailure(t, sm); f.Error != want {
			t.Fatalf("stored error %q", f.Error)
		}

		if ev := requireOneOutcome(t, dir, hooks.OutcomeFailed); ev.Error != want {
			t.Fatalf("error %q", ev.Error)
		}

		assertNoSecretUnder(t, data, "s3cr3t-e2e", "dotenv-password-value", "ss\\\\wo")
	})

	t.Run("empty error", func(t *testing.T) {
		sm, _ := newForkFailureManager(t, nil)
		setup(sm)

		if got := sm.redactSecrets(""); got != "" {
			t.Fatalf("redactSecrets(\"\") = %q", got)
		}
	})
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()

	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}

	return data
}

// A secret that straddles rune 2000 of the stage error is redacted before the cut: no prefix of it
// reaches pre_rollback, the failure store or the outcome event. Upstream truncates the in-memory
// failure to 2000 runes first, which is why the record is built from the stage error.
func TestOutcomeRedactsSecretAcrossTruncation(t *testing.T) {
	const secret = "s3cr3t-e2e" // runes 1995-2004 of the error

	r := newRollbackRepo(t)
	dir := startTestOutbox(t)
	sm := newRollbackManager(t, r, &lastGoodClient{})
	sm.DeployConfig.ExternalSecrets = map[string]secrettypes.ExternalSecretRef{"TOKEN": {LegacyRef: "ref"}}
	sm.DeployConfig.Internal.Environment = map[string]string{"TOKEN": secret}

	if err := rollback.StoreFor(sm.AppConfig.DataMountPath).PutLastGood(rollbackLastGood(r, "good", rollbackConfig)); err != nil {
		t.Fatal(err)
	}

	var preRollback []hooks.PreRollbackRequest

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body hooks.PreRollbackRequest
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Errorf("decode pre_rollback: %v", err)
		}

		preRollback = append(preRollback, body)
		_, _ = io.WriteString(w, `{"decision":"block"}`)
	}))
	t.Cleanup(server.Close)

	SetPreRollback(&hooks.PreRollbackClient{URL: server.URL, Signer: hooks.Signer{Secret: []byte("s")}})
	t.Cleanup(func() { SetPreRollback(nil) })

	errText := strings.Repeat("x", 1995) + secret + " tail"
	_ = sm.handleStageFailure(t.Context(), StageDeploy, sm.Log, errors.New(errText))

	wantErr := strings.Repeat("x", 1995) + "[REDA"

	if len(preRollback) != 1 || preRollback[0].Error != wantErr {
		t.Fatalf("pre_rollback requests %+v, want one with the redacted error", preRollback)
	}

	if f := diskFailure(t, sm); f.Error != wantErr || f.State != rollback.StateBlocked {
		t.Fatalf("failure %+v", f)
	}

	if ev := requireOneOutcome(t, dir, hooks.OutcomeRollbackBlocked); ev.Error != wantErr {
		t.Fatalf("event error %q", ev.Error)
	}

	raw, err := os.ReadFile(filepath.Join(sm.AppConfig.DataMountPath, "fork", "failures", "default", "web.json"))
	if err != nil || bytes.Contains(raw, []byte("s3c")) {
		t.Fatalf("failure file %s, err %v", raw, err)
	}
}
