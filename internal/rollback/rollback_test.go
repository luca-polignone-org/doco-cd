package rollback

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kimdre/doco-cd/internal/hooks"
)

type fakeDecider struct {
	decision hooks.Decision
	calls    int
	got      hooks.PreRollbackRequest
	timeout  time.Duration
}

func (d *fakeDecider) Decide(_ context.Context, req hooks.PreRollbackRequest, timeout time.Duration) hooks.Decision {
	d.calls++
	d.got, d.timeout = req, timeout

	return d.decision
}

type fakeExecutor struct {
	err   error
	block bool
	calls int
	got   LastGood
}

func (e *fakeExecutor) Deploy(ctx context.Context, lg LastGood) error {
	e.calls++
	e.got = lg

	if e.block {
		<-ctx.Done()

		return ctx.Err()
	}

	return e.err
}

const runObject = "default/web"

func runFailure() Failure {
	return Failure{
		Object: runObject, Repository: "repo", Stack: "web", CommitSHA: "c2", Stage: "deploy",
		Error: "boom", FailedAt: time.Unix(30, 0).UTC(), State: StateFailed,
	}
}

func storeWithLastGood(t *testing.T) *Store {
	t.Helper()

	s := StoreFor(t.TempDir())
	if err := s.PutLastGood(LastGood{Object: runObject, CommitSHA: "c1", DeployConfig: []byte("name: web\n")}); err != nil {
		t.Fatal(err)
	}

	return s
}

func mustFailure(t *testing.T, s *Store) Failure {
	t.Helper()

	f, ok, err := s.GetFailure(runObject)
	if err != nil || !ok {
		t.Fatalf("failure on disk: ok=%v err=%v", ok, err)
	}

	return f
}

func TestRunWithoutLastGood(t *testing.T) {
	s := StoreFor(t.TempDir())
	d, e := &fakeDecider{decision: hooks.DecisionProceed}, &fakeExecutor{}

	out := Run(t.Context(), s, d, e, runFailure(), time.Second, time.Second)
	if out.State != StateFailed || out.Decision != "" || out.ToCommit != "" || out.Err != nil || out.PersistErr != nil {
		t.Fatalf("outcome %+v", out)
	}

	if d.calls != 0 || e.calls != 0 {
		t.Fatalf("decider %d / executor %d calls, want none", d.calls, e.calls)
	}

	if f := mustFailure(t, s); f.State != StateFailed || f.Decision != "" || f.CommitSHA != "c2" {
		t.Fatalf("failure %+v", f)
	}
}

func TestRunProceedRollsBack(t *testing.T) {
	s := storeWithLastGood(t)
	d, e := &fakeDecider{decision: hooks.DecisionProceed}, &fakeExecutor{}

	out := Run(t.Context(), s, d, e, runFailure(), 3*time.Second, time.Minute)
	if out.State != StateRolledBack || out.Decision != hooks.DecisionProceed || out.ToCommit != "c1" || out.Err != nil || out.PersistErr != nil {
		t.Fatalf("outcome %+v", out)
	}

	want := hooks.PreRollbackRequest{Object: runObject, FailedCommit: "c2", LastGoodCommit: "c1", Stage: "deploy", Error: "boom"}
	if d.got != want || d.timeout != 3*time.Second {
		t.Fatalf("pre_rollback request %+v timeout %s", d.got, d.timeout)
	}

	if e.calls != 1 || e.got.CommitSHA != "c1" {
		t.Fatalf("executor calls %d with %+v", e.calls, e.got)
	}

	f := mustFailure(t, s)
	if f.State != StateRolledBack || f.Decision != "proceed" || f.CommitSHA != "c2" || f.Error != "boom" {
		t.Fatalf("failure %+v", f)
	}
}

// Every pre_rollback outcome other than proceed blocks, without a rollback.
func TestRunNonProceedBlocks(t *testing.T) {
	for _, decision := range []hooks.Decision{
		hooks.DecisionBlock, hooks.DecisionTimeout, hooks.DecisionHTTPError, hooks.DecisionInvalid, hooks.DecisionUnconfigured,
	} {
		t.Run(string(decision), func(t *testing.T) {
			s := storeWithLastGood(t)
			d, e := &fakeDecider{decision: decision}, &fakeExecutor{}

			out := Run(t.Context(), s, d, e, runFailure(), time.Second, time.Second)
			if out.State != StateBlocked || out.Decision != decision || out.ToCommit != "" || out.PersistErr != nil {
				t.Fatalf("outcome %+v", out)
			}

			if d.calls != 1 || e.calls != 0 {
				t.Fatalf("decider %d / executor %d calls", d.calls, e.calls)
			}

			if f := mustFailure(t, s); f.State != StateBlocked || f.Decision != string(decision) {
				t.Fatalf("failure %+v", f)
			}
		})
	}
}

// A nil *hooks.PreRollbackClient is a valid Decider: HOOK_PRE_ROLLBACK_URL unset gives unconfigured.
func TestRunNilClientIsUnconfigured(t *testing.T) {
	s := storeWithLastGood(t)
	e := &fakeExecutor{}

	var client *hooks.PreRollbackClient

	out := Run(t.Context(), s, client, e, runFailure(), time.Second, time.Second)
	if out.State != StateBlocked || out.Decision != hooks.DecisionUnconfigured || e.calls != 0 {
		t.Fatalf("outcome %+v, executor calls %d", out, e.calls)
	}
}

func TestRunExecutorErrorIsRollbackFailed(t *testing.T) {
	s := storeWithLastGood(t)
	boom := errors.New("publish failed")
	d, e := &fakeDecider{decision: hooks.DecisionProceed}, &fakeExecutor{err: boom}

	out := Run(t.Context(), s, d, e, runFailure(), time.Second, time.Second)
	if out.State != StateRollbackFailed || out.Decision != hooks.DecisionProceed || out.ToCommit != "c1" || !errors.Is(out.Err, boom) {
		t.Fatalf("outcome %+v", out)
	}

	if f := mustFailure(t, s); f.State != StateRollbackFailed || f.Decision != "proceed" {
		t.Fatalf("failure %+v", f)
	}
}

func TestRunExecutorTimeout(t *testing.T) {
	s := storeWithLastGood(t)
	d, e := &fakeDecider{decision: hooks.DecisionProceed}, &fakeExecutor{block: true}

	out := Run(t.Context(), s, d, e, runFailure(), time.Second, 20*time.Millisecond)
	if out.State != StateRollbackFailed || !errors.Is(out.Err, context.DeadlineExceeded) {
		t.Fatalf("outcome %+v", out)
	}

	if f := mustFailure(t, s); f.State != StateRollbackFailed {
		t.Fatalf("failure %+v", f)
	}
}

// An unreadable last good blocks without asking pre_rollback (spec, "Store").
func TestRunCorruptLastGoodBlocks(t *testing.T) {
	data := t.TempDir()
	s := StoreFor(data)

	path := filepath.Join(data, "fork", "lastgood", "default", "web.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte(`{"version":1,"commit_sha":`), 0o600); err != nil {
		t.Fatal(err)
	}

	d, e := &fakeDecider{decision: hooks.DecisionProceed}, &fakeExecutor{}

	out := Run(t.Context(), s, d, e, runFailure(), time.Second, time.Second)
	if out.State != StateBlocked || out.Decision != hooks.DecisionInvalid || out.Err == nil || out.PersistErr != nil {
		t.Fatalf("outcome %+v", out)
	}

	if d.calls != 0 || e.calls != 0 {
		t.Fatalf("decider %d / executor %d calls, want none", d.calls, e.calls)
	}

	if f := mustFailure(t, s); f.State != StateBlocked || f.Decision != "invalid" {
		t.Fatalf("failure %+v", f)
	}
}

func TestRunPersistError(t *testing.T) {
	data := t.TempDir()
	s := StoreFor(data)

	if err := s.PutLastGood(LastGood{Object: runObject, CommitSHA: "c1"}); err != nil {
		t.Fatal(err)
	}

	// A file where the failures directory should be: the final write cannot succeed.
	if err := os.WriteFile(filepath.Join(data, "fork", "failures"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	d, e := &fakeDecider{decision: hooks.DecisionProceed}, &fakeExecutor{}

	out := Run(t.Context(), s, d, e, runFailure(), time.Second, time.Second)
	if out.State != StateRolledBack || out.PersistErr == nil {
		t.Fatalf("outcome %+v", out)
	}
}
