package rollback

import (
	"errors"
	"testing"
	"time"

	"github.com/kimdre/doco-cd/internal/hooks"
)

// Only a rollback that deployed the last good records it in rolled_back_to; every other outcome
// leaves the field empty, also when the failure passed in carries one (TCK-644).
func TestRunRecordsRolledBackTo(t *testing.T) {
	cases := map[string]struct {
		decision hooks.Decision
		exec     *fakeExecutor
		state    State
		want     string
	}{
		"rolled_back":     {hooks.DecisionProceed, &fakeExecutor{}, StateRolledBack, "c1"},
		"blocked":         {hooks.DecisionBlock, &fakeExecutor{}, StateBlocked, ""},
		"rollback_failed": {hooks.DecisionProceed, &fakeExecutor{err: errors.New("unhealthy")}, StateRollbackFailed, ""},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s := storeWithLastGood(t)
			f := runFailure()
			f.RolledBackTo = "stale"

			out := Run(t.Context(), s, &fakeDecider{decision: tc.decision}, tc.exec, f, time.Second, time.Second)
			if out.State != tc.state {
				t.Fatalf("state %s, want %s", out.State, tc.state)
			}

			if got := mustFailure(t, s).RolledBackTo; got != tc.want {
				t.Fatalf("rolled_back_to = %q, want %q", got, tc.want)
			}
		})
	}
}

// rolled_back_to survives the store round trip and is omitted when empty, so a record without a
// rollback reads as before.
func TestFailureRolledBackToRoundTrip(t *testing.T) {
	s := StoreFor(t.TempDir())

	f := runFailure()
	f.State, f.RolledBackTo = StateRolledBack, "c1"

	if err := s.PutFailure(f); err != nil {
		t.Fatal(err)
	}

	if got := mustFailure(t, s); got.RolledBackTo != "c1" || got.RetryRecreates() {
		t.Fatalf("failure %+v", got)
	}
}
