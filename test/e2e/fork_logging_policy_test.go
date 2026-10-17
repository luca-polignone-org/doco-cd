//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/kimdre/doco-cd/internal/hooks"
)

const forkLoggingStack = "e2e-fork-logging"

// forkLoggingClean is the compose file of the last commit: neither logging nor labels of its own.
const forkLoggingClean = "services:\n  app:\n    image: alpine:3.22\n    command: [\"sleep\", \"600\"]\n"

// TestForkLoggingPolicy covers the logging rule of the compose policy (default.logging), on one
// harness:
//
//  1. a service with a logging of its own, other than default.logging, is denied with
//     app: logging=json-file;
//  2. a service with a com.docker.compose.* label of its own is denied with
//     app: labels=com.docker.compose.project;
//  3. a clean service deploys: the labels Compose and Doco-CD add themselves do not trip the rule.
//
// default.logging names json-file: journald is not on the runner, and the rule does not care
// which driver it compares. Each denial is one signed policy_denied outcome in pre-deploy, and no
// container is created before the clean commit.
func TestForkLoggingPolicy(t *testing.T) {
	t.Parallel()
	requireStandaloneSuite(t)

	h := NewHarness(t, "fork-logging-policy")
	if h.isSwarmMode() {
		t.Skip("a Swarm target is always denied by the compose policy")
	}

	policy := "version: 1\ndefault:\n  logging:\n    driver: json-file\n    options: {}\n" +
		"targets:\n  \"default/" + forkLoggingStack + "\":\n" +
		"    repository: http://gitserver/fork-logging-policy.git\n    reference: main\n"

	policyPath := filepath.Join(h.workDir, "compose-policy.yaml")
	if err := os.WriteFile(policyPath, []byte(policy), 0o600); err != nil {
		t.Fatalf("write compose policy: %v", err)
	}

	rec := h.newPreRollbackReceiver("proceed")

	h.BindDaemonFile(policyPath, "/config/compose-policy.yaml")
	h.SetEnv("COMPOSE_POLICY_FILE", "/config/compose-policy.yaml")
	h.SetEnv("HOOK_OUTCOME_URL", forkOutcomeHookURL)
	h.SetEnv("HOOK_SECRET", forkRollbackSecret)
	h.Start()
	rec.start()

	object := "default/" + forkLoggingStack

	head, err := h.repo.Head()
	if err != nil {
		t.Fatalf("read HEAD: %v", err)
	}

	// denial waits for the policy_denied event of commit and checks its violations.
	denial := func(commit string, after int, want hooks.ViolationOut) int {
		t.Helper()

		ev, i := rec.waitOutcome(object, after, 120*time.Second, "policy_denied "+want.Field, func(ev hooks.Event) bool {
			return ev.Outcome == hooks.OutcomePolicyDenied && ev.Commit == commit
		})
		if !reflect.DeepEqual(ev.Violations, []hooks.ViolationOut{want}) || ev.Stage != "pre-deploy" {
			t.Fatalf("policy_denied = %+v, want the single violation %+v in pre-deploy", ev, want)
		}

		if n := h.projectContainerCount(forkLoggingStack); n != 0 {
			t.Fatalf("project %s has %d containers after a denied deploy, want 0", forkLoggingStack, n)
		}

		return i
	}

	// 1. A logging of its own, other than default.logging.
	h.WaitForLog("app: logging=json-file", 120*time.Second)

	iLogging := denial(head.Hash().String(), 0, hooks.ViolationOut{Service: "app", Field: "logging", Value: "json-file"})

	// 2. A compose label of its own.
	mark := h.LogMark()
	h.writeWorktree(map[string]string{
		"deploy/compose.yaml": forkLoggingClean + "    labels:\n      com.docker.compose.project: other\n",
	})
	c2 := h.RepoPush("forged compose label").String()

	h.WaitForLogAfter("app: labels=com.docker.compose.project", mark, 120*time.Second)

	iLabels := denial(c2, iLogging+1, hooks.ViolationOut{Service: "app", Field: "labels", Value: "com.docker.compose.project"})

	// 3. Clean: deployed.
	mark = h.LogMark()
	h.writeWorktree(map[string]string{"deploy/compose.yaml": forkLoggingClean})
	c3 := h.RepoPush("clean service").String()

	h.WaitForLogAfter(deployCompletedLog, mark, 120*time.Second)

	rec.waitOutcome(object, iLabels+1, 90*time.Second, "deployed after the clean commit", func(ev hooks.Event) bool {
		return ev.Outcome == hooks.OutcomeDeployed && ev.Commit == c3
	})

	if h.ContainerID(forkLoggingStack, "app") == "" {
		t.Fatal("app container must run after the clean deploy")
	}
}
