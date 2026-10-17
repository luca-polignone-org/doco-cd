//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/moby/moby/client"

	"github.com/kimdre/doco-cd/internal/hooks"
)

// forkPolicyStack is the stack of the fork-compose-policy scenario.
const forkPolicyStack = "e2e-fork-policy"

// BindDaemonFile bind mounts a host file into the daemon container, read-only. Call before Start.
func (h *Harness) BindDaemonFile(hostPath, containerPath string) {
	h.t.Helper()
	h.extraBinds = append(h.extraBinds, hostPath+":"+containerPath+":ro")
}

// projectContainerCount counts every container of a Compose project, stopped ones included.
func (h *Harness) projectContainerCount(project string) int {
	h.t.Helper()

	containers, err := h.docker.ContainerList(h.ctx, client.ContainerListOptions{
		All:     true,
		Filters: client.Filters{}.Add("label", "com.docker.compose.project="+project),
	})
	if err != nil {
		h.t.Fatalf("list containers of project %s: %v", project, err)
	}

	return len(containers.Items)
}

// TestForkComposePolicy covers the fork compose policy (COMPOSE_POLICY_FILE) in two phases on
// one harness: a first deploy with a privileged service is denied in pre-deploy without creating
// any container, then the same target with a clean compose file deploys. The denial is one signed
// policy_denied outcome event with the privileged violation, although two polls denied the commit.
func TestForkComposePolicy(t *testing.T) {
	t.Parallel()
	requireStandaloneSuite(t)

	h := NewHarness(t, "fork-compose-policy")
	if h.isSwarmMode() {
		t.Skip("a Swarm target is always denied by the compose policy")
	}

	policy := "version: 1\ntargets:\n  \"default/" + forkPolicyStack + "\":\n" +
		"    repository: http://gitserver/fork-compose-policy.git\n    reference: main\n"

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

	// Phase 1: privileged service, denied before any mutation.
	h.WaitForLog("deployment denied by policy", 120*time.Second)

	if !h.daemonHasLog("app: privileged=true") {
		t.Fatal("daemon log must name the privileged violation")
	}

	// One more poll of the same commit must still create nothing.
	mark := h.LogMark()
	h.WaitForLogAfter("deployment denied by policy", mark, 60*time.Second)

	if n := h.projectContainerCount(forkPolicyStack); n != 0 {
		t.Fatalf("project %s has %d containers after a denied deploy, want 0", forkPolicyStack, n)
	}

	// Phase 2: clean compose file, same policy, deployed.
	mark = h.LogMark()
	h.ReplaceInWorktree("deploy/compose.yaml", "    privileged: true\n", "")
	h.RepoPush("drop privileged")

	h.WaitForLogAfter(deployCompletedLog, mark, 120*time.Second)

	if h.ContainerID(forkPolicyStack, "app") == "" {
		t.Fatal("app container must run after the clean deploy")
	}

	// Delivery is FIFO per object: once deployed arrived, every earlier event of the target did.
	object := "default/" + forkPolicyStack
	_, iDeployed := rec.waitOutcome(object, 0, 90*time.Second, "deployed after the clean commit", func(ev hooks.Event) bool {
		return ev.Outcome == hooks.OutcomeDeployed
	})

	var denials []hooks.Event

	for _, ev := range rec.outcomes(object)[:iDeployed] {
		if ev.Outcome == hooks.OutcomePolicyDenied {
			denials = append(denials, ev)
		}
	}

	want := []hooks.ViolationOut{{Service: "app", Field: "privileged", Value: "true"}}
	if len(denials) != 1 || !reflect.DeepEqual(denials[0].Violations, want) || denials[0].Stage != "pre-deploy" {
		t.Fatalf("policy_denied events = %+v, want one with %+v", denials, want)
	}
}
