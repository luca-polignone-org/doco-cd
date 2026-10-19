//go:build e2e

package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/hooks"
)

const forkRevertStack = "e2e-fork-rollback-revert"

// containerStartedAt returns the State.StartedAt of a container.
func (h *Harness) containerStartedAt(containerID string) string {
	h.t.Helper()

	inspect, err := h.docker.ContainerInspect(h.ctx, containerID, client.ContainerInspectOptions{})
	if err != nil {
		h.t.Fatalf("inspect container %s: %v", shortContainerID(containerID), err)
	}

	return inspect.Container.State.StartedAt
}

// forkRevertFailure returns the state, commit and rolled_back_to of the target's failure record,
// or "" without one.
func (r *preRollbackReceiver) forkRevertFailure() (state, commit, rolledBackTo string) {
	r.h.t.Helper()

	raw := r.storeFile("/data/fork/failures/default/" + forkRevertStack + ".json")
	if raw == "" {
		return "", "", ""
	}

	var f struct {
		State        string `json:"state"`
		CommitSHA    string `json:"commit_sha"`
		RolledBackTo string `json:"rolled_back_to"`
	}

	if err := json.Unmarshal([]byte(raw), &f); err != nil {
		r.h.t.Fatalf("decode failure record %q: %v", raw, err)
	}

	return f.State, f.CommitSHA, f.RolledBackTo
}

// TestForkRollbackRevertKeepsContainers covers TCK-644 (criterion 2b of poli-gate), on one
// harness:
//
//  1. c1 deploys and becomes the last good;
//  2. c2 changes the image and fails after its containers started; pre_rollback proceeds and the
//     rollback brings c1 back, with the image reference of c1's compose file (not the image ID);
//  3. the daemon restarts, so the failure is read from disk;
//  4. c3 has the content of c1 (a revert): unblocked, then deployed, and the container is still
//     the one the rollback created, same ID and StartedAt.
func TestForkRollbackRevertKeepsContainers(t *testing.T) {
	t.Parallel()
	requireStandaloneSuite(t)

	h := NewHarness(t, "fork-rollback-revert")
	if h.isSwarmMode() {
		t.Skip("rollback is Compose only: a Swarm target with rollback is denied by the policy")
	}

	rec := h.newPreRollbackReceiver("proceed")

	h.SetEnv("HOOK_PRE_ROLLBACK_URL", forkRollbackHookURL)
	h.SetEnv("HOOK_OUTCOME_URL", forkOutcomeHookURL)
	h.SetEnv("HOOK_SECRET", forkRollbackSecret)
	h.Start()
	rec.start()

	object := "default/" + forkRevertStack

	// The content of c1, to push it again as the revert.
	c1Files := map[string]string{}

	for _, rel := range []string{".doco-cd.yml", "deploy/compose.yaml"} {
		data, err := os.ReadFile(filepath.Join(h.worktree, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}

		c1Files[rel] = string(data)
	}

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

	c1ImageID, _ := h.containerImageID(h.ContainerID(forkRevertStack, "app"))

	// 2. c2 is broken; the rollback brings c1 back.
	mark := h.LogMark()
	h.ReplaceInWorktree(".doco-cd.yml", `HOOK_EXIT: "0"`, `HOOK_EXIT: "1"`)
	h.ReplaceInWorktree("deploy/compose.yaml", "alpine:3.22", "nginx:1.29-alpine")
	c2 := h.RepoPush("broken c2").String()

	h.WaitForLogLineAfter(mark, 180*time.Second, forkRollbackFinished, `"state":"rolled_back"`)

	_, iRolledBack := rec.waitOutcome(object, iDeployed+1, 60*time.Second, "rolled_back c2", func(ev hooks.Event) bool {
		return ev.Outcome == hooks.OutcomeRolledBack && ev.Commit == c2
	})

	if state, commit, to := rec.forkRevertFailure(); state != "rolled_back" || commit != c2 || to != c1 {
		t.Fatalf("failure = %s/%s rolled back to %q, want rolled_back/%s to %s", state, commit, to, c2, c1)
	}

	rolled := h.ContainerID(forkRevertStack, "app")
	rolledStartedAt := h.containerStartedAt(rolled)

	// Errorf: the recreate check below is a separate defect, and must run too.
	if imageID, configured := h.containerImageID(rolled); imageID != c1ImageID || configured != "alpine:3.22" {
		t.Errorf("app after rollback runs %s (configured %q), want %s with the reference of c1, alpine:3.22", imageID, configured, c1ImageID)
	}

	// 3. A restart empties the in-memory failure: the retry below reads it from disk.
	h.KillAndRestartDaemon()
	rec.start()

	mark = h.LogMark()
	h.WaitForLogAfter(forkRollbackPoisoned, mark, 90*time.Second)

	// 4. c3 reverts to the content of c1.
	mark = h.LogMark()
	h.writeWorktree(c1Files)
	c3 := h.RepoPush("revert to c1").String()

	_, iUnblocked := rec.waitOutcome(object, iRolledBack+1, 180*time.Second, "unblocked by c3", func(ev hooks.Event) bool {
		return ev.Outcome == hooks.OutcomeUnblocked && ev.Commit == c3
	})
	rec.waitOutcome(object, iUnblocked+1, 180*time.Second, "deployed c3 after its unblocked", func(ev hooks.Event) bool {
		return ev.Outcome == hooks.OutcomeDeployed && ev.Commit == c3
	})

	h.WaitForLogAfter(deployCompletedLog, mark, 60*time.Second)

	got := h.ContainerID(forkRevertStack, "app")
	if got != rolled || h.containerStartedAt(got) != rolledStartedAt {
		t.Fatalf("app container after the revert = %s started %s, want the one of the rollback %s started %s (no recreate)",
			shortContainerID(got), h.containerStartedAt(got), shortContainerID(rolled), rolledStartedAt)
	}

	h.WaitFor(30*time.Second, "failure removed", func() bool {
		state, _, _ := rec.forkRevertFailure()
		return state == ""
	})
}
