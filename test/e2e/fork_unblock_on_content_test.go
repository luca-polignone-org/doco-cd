//go:build e2e

package e2e

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/kimdre/doco-cd/internal/hooks"
)

const (
	forkUnblockStack = "e2e-fork-unblock"

	forkUnblockSameContent = "new commit with the content of the rolled back or blocked one"

	// forkUnblockBroken fails after its containers were created (post_start exits 1).
	forkUnblockBroken = "services:\n  app:\n    image: alpine:3.22\n    command: [\"sleep\", \"600\"]\n" +
		"    post_start:\n      - command: [\"sh\", \"-c\", \"exit 1\"]\n"
	// forkUnblockHealthy differs from both c1 and the broken commit.
	forkUnblockHealthy = "services:\n  app:\n    image: alpine:3.22\n    command: [\"sleep\", \"600\"]\n" +
		"    labels:\n      e2e.phase: healthy-again\n"
)

// forkUnblockFailure returns the state, commit and content key of the target's failure record.
func (r *preRollbackReceiver) forkUnblockFailure() (state, commit, contentKey string) {
	r.h.t.Helper()

	raw := r.storeFile("/data/fork/failures/default/" + forkUnblockStack + ".json")
	if raw == "" {
		return "", "", ""
	}

	var f struct {
		State      string `json:"state"`
		CommitSHA  string `json:"commit_sha"`
		ContentKey string `json:"content_key"`
	}

	if err := json.Unmarshal([]byte(raw), &f); err != nil {
		r.h.t.Fatalf("decode failure record %q: %v", raw, err)
	}

	return f.State, f.CommitSHA, f.ContentKey
}

// TestForkUnblockOnContent covers rollback.unblock_on: content, on one harness:
//
//  1. c1 is healthy and deploys;
//  2. c2 breaks the deploy after its containers started; pre_rollback blocks (rollback_blocked),
//     and the failure record carries the content key of c2;
//  3. c3 changes only other/, outside working_dir: the target stays poisoned for two polls, with
//     no new outcome and the app container untouched;
//  4. after a restart of the daemon, c4 changes only other/ again: same result, so the key was
//     read from disk;
//  5. c5 changes deploy/compose.yaml and makes it healthy: unblocked, then deployed.
func TestForkUnblockOnContent(t *testing.T) {
	t.Parallel()
	requireStandaloneSuite(t)

	h := NewHarness(t, "fork-unblock-on-content")
	if h.isSwarmMode() {
		t.Skip("rollback is Compose only: a Swarm target with rollback is denied by the policy")
	}

	rec := h.newPreRollbackReceiver("block")

	h.SetEnv("HOOK_PRE_ROLLBACK_URL", forkRollbackHookURL)
	h.SetEnv("HOOK_OUTCOME_URL", forkOutcomeHookURL)
	h.SetEnv("HOOK_SECRET", forkRollbackSecret)
	h.Start()
	rec.start()

	object := "default/" + forkUnblockStack

	// 1. c1 deploys.
	h.WaitForLog(deployCompletedLog, 120*time.Second)

	head, err := h.repo.Head()
	if err != nil {
		t.Fatalf("read HEAD: %v", err)
	}

	c1 := head.Hash().String()

	_, iDeployed := rec.waitOutcome(object, 0, 90*time.Second, "deployed c1", func(ev hooks.Event) bool {
		return ev.Outcome == hooks.OutcomeDeployed && ev.Commit == c1
	})

	// 2. c2 is broken; pre_rollback blocks.
	mark := h.LogMark()
	h.writeWorktree(map[string]string{"deploy/compose.yaml": forkUnblockBroken})
	c2 := h.RepoPush("broken c2").String()

	h.WaitForLogLineAfter(mark, 180*time.Second, forkRollbackFinished, `"state":"blocked"`, `"decision":"block"`)

	_, iBlocked := rec.waitOutcome(object, iDeployed+1, 60*time.Second, "rollback_blocked c2", func(ev hooks.Event) bool {
		return ev.Outcome == hooks.OutcomeRollbackBlocked && ev.Commit == c2
	})

	state, commit, key := rec.forkUnblockFailure()
	if state != "blocked" || commit != c2 || key == "" {
		t.Fatalf("failure = %s/%s with content key %q, want blocked/%s with a content key", state, commit, key, c2)
	}

	blocked := h.ContainerID(forkUnblockStack, "app")
	if blocked == "" {
		t.Fatal("the blocked containers of c2 must stay")
	}

	// sameContent pushes a commit that changes only other/README and checks that two polls skip
	// it without a new outcome and without touching the app container.
	sameContent := func(message string) {
		t.Helper()

		before := len(rec.outcomes(object))

		mark := h.LogMark()
		h.writeWorktree(map[string]string{"other/README": message + "\n"})
		c := h.RepoPush(message).String()

		h.WaitForLogLineAfter(mark, 90*time.Second, forkUnblockSameContent, c)
		h.WaitForLogOccurrencesAfter(forkUnblockSameContent, mark, 2, 90*time.Second)

		if n := len(rec.outcomes(object)); n != before {
			t.Fatalf("%s: %d new outcome events for %s, want none", message, n-before, object)
		}

		if got := h.ContainerID(forkUnblockStack, "app"); got != blocked {
			t.Fatalf("%s: app container = %s, want the blocked one %s untouched", message, got, blocked)
		}

		if state, commit, got := rec.forkUnblockFailure(); state != "blocked" || commit != c2 || got != key {
			t.Fatalf("%s: failure = %s/%s with key %q, want blocked/%s with key %q", message, state, commit, got, c2, key)
		}
	}

	// 3. Same content, other/ only.
	sameContent("c3 changes only other")

	// 4. After a restart, the key comes from disk.
	h.KillAndRestartDaemon()
	rec.start()
	sameContent("c4 changes only other after a restart")

	// 5. c5 changes the compose file and makes it healthy.
	mark = h.LogMark()
	h.writeWorktree(map[string]string{"deploy/compose.yaml": forkUnblockHealthy})
	c5 := h.RepoPush("healthy c5").String()

	h.WaitForLogAfter(deployCompletedLog, mark, 180*time.Second)

	_, iUnblocked := rec.waitOutcome(object, iBlocked+1, 60*time.Second, "unblocked by c5", func(ev hooks.Event) bool {
		return ev.Outcome == hooks.OutcomeUnblocked && ev.Commit == c5
	})
	rec.waitOutcome(object, iUnblocked+1, 60*time.Second, "deployed c5 after its unblocked", func(ev hooks.Event) bool {
		return ev.Outcome == hooks.OutcomeDeployed && ev.Commit == c5
	})

	h.WaitFor(30*time.Second, "failure removed", func() bool {
		state, _, _ := rec.forkUnblockFailure()
		return state == ""
	})
}
