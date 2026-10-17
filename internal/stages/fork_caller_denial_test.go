package stages

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kimdre/doco-cd/internal/composepolicy"
	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/config/app"
	gitInternal "github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/hooks"
	"github.com/kimdre/doco-cd/internal/notification"
)

// The tests in this file register the process-wide outbox and the git auth resolver, so none of
// them calls t.Parallel.

// A targets/sync denial is about the caller: it emits policy_denied but neither notifies nor marks
// the target owner's commit as failed. Any other denial still does both.
func TestTargetSyncDenialDoesNotPaintTheTarget(t *testing.T) {
	gitInternal.ConfigureAuthResolver(nil, "", "", "pat-token", "", gitInternal.GitHubAppConfig{})
	t.Cleanup(func() {
		gitInternal.ConfigureAuthResolver(nil, "", "", "", "", gitInternal.GitHubAppConfig{})
	})

	var statuses atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		statuses.Add(1)
		w.WriteHeader(http.StatusCreated)
	}))
	t.Cleanup(server.Close)

	newManager := func(t *testing.T) (*StageManager, *levelGatedSender) {
		t.Helper()

		sm, _ := newForkFailureManager(t, nil)
		sender := &levelGatedSender{minLevel: notification.Info}
		sm.Notifier = sender
		sm.AppConfig = &app.Config{
			GitCommitStatus: true, GitScmProvider: "gitea",
			GitScmApiUrl: config.HttpUrl(server.URL), GitAccessToken: "pat-token",
		}
		sm.Repository.SourceUrl = server.URL + "/org/repo.git"

		return sm, sender
	}

	t.Run("targets/sync only", func(t *testing.T) {
		dir := startTestOutbox(t)
		sm, sender := newManager(t)

		statuses.Store(0)

		denied := &PolicyDeniedError{Object: "default/web", Violations: []composepolicy.Violation{targetSyncViolation}}

		err := sm.handleStageFailure(t.Context(), StagePreDeploy, sm.Log, denied)
		if !isTargetSyncDenial(err) || !notification.WasNotified(err) {
			t.Fatalf("handleStageFailure() = %v, want the denial marked as notified", err)
		}

		requireOneOutcome(t, dir, hooks.OutcomePolicyDenied)

		// NotifyFailure sends from a goroutine: give a wrong send the time to show up.
		time.Sleep(100 * time.Millisecond)

		if n := len(sender.sentNotifications()); n != 0 || statuses.Load() != 0 {
			t.Fatalf("notifications %d, commit statuses %d, want none", n, statuses.Load())
		}
	})

	t.Run("targets/sync with another violation", func(t *testing.T) {
		dir := startTestOutbox(t)
		sm, sender := newManager(t)

		statuses.Store(0)

		denied := &PolicyDeniedError{Object: "default/web", Violations: []composepolicy.Violation{
			targetSyncViolation, {Service: "app", Field: "privileged", Value: "true"},
		}}

		if err := sm.handleStageFailure(t.Context(), StagePreDeploy, sm.Log, denied); !notification.WasNotified(err) {
			t.Fatalf("handleStageFailure() = %v", err)
		}

		requireOneOutcome(t, dir, hooks.OutcomePolicyDenied)

		deadline := time.Now().Add(5 * time.Second)
		for len(sender.sentNotifications()) == 0 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}

		if n := len(sender.sentNotifications()); n != 1 || statuses.Load() != 1 {
			t.Fatalf("notifications %d, commit statuses %d, want one each", n, statuses.Load())
		}
	})
}
