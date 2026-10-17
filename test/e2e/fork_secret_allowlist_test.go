//go:build e2e

package e2e

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	"github.com/testcontainers/testcontainers-go"
	tcexec "github.com/testcontainers/testcontainers-go/exec"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/kimdre/doco-cd/internal/hooks"
)

const (
	forkSecretStack = "e2e-fork-secret" // #nosec G101 -- a stack name, not a credential.

	// forkOpenBaoImage is the latest stable OpenBao 2.x, the release of the api client in go.mod,
	// pinned by the digest of its multi-arch index.
	forkOpenBaoImage = "openbao/openbao:2.7.1@sha256:6d2b93856e3fcf7b18ad855a0b51eaba474dc8b79cf554379ea32034797d2acf"
	// forkOpenBaoAddr is on the daemon's own loopback: the dev server shares its network namespace.
	forkOpenBaoAddr      = "http://127.0.0.1:8200"
	forkOpenBaoRootToken = "e2e-fork-bao-root" // #nosec G101 -- test fixture, not a real credential.

	forkSecretAllowed      = "allowed-value"                          // #nosec G101 -- test fixture, not a real credential.
	forkSecretOther        = "other-value"                            // #nosec G101 -- test fixture, not a real credential.
	forkSecretAllowedRef   = "kv:secret:prod/e2e-fork-secret/db:PASS" // #nosec G101 -- a secret reference, not a credential.
	forkSecretForbiddenRef = "kv:secret:prod/other/db:PASS"           // #nosec G101 -- a secret reference, not a credential.
)

// startOpenBaoSidecar runs an OpenBao dev server in the daemon's network namespace, listening on
// the daemon's 127.0.0.1:8200 with forkOpenBaoRootToken as its root token. Call after Start. The
// entrypoint script is skipped: it only adds a config directory and drops privileges, neither of
// which a dev server needs.
func (h *Harness) startOpenBaoSidecar() testcontainers.Container {
	h.t.Helper()

	daemonID := h.daemon.GetContainerID()

	sidecar, err := testcontainers.GenericContainer(h.ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:      forkOpenBaoImage,
			Name:       h.containerName("openbao"),
			Entrypoint: []string{"bao"},
			Cmd: []string{
				"server", "-dev",
				"-dev-root-token-id=" + forkOpenBaoRootToken,
				"-dev-listen-address=127.0.0.1:8200",
			},
			HostConfigModifier: func(hc *container.HostConfig) {
				hc.NetworkMode = container.NetworkMode("container:" + daemonID)
			},
			WaitingFor: wait.ForExec([]string{"bao", "status", "-address=" + forkOpenBaoAddr}).
				WithStartupTimeout(60 * time.Second).
				WithPollInterval(500 * time.Millisecond),
		},
		Started: true,
	})
	if err != nil {
		h.t.Fatalf("start OpenBao sidecar: %v", err)
	}

	h.t.Cleanup(func() { h.terminateContainer(sidecar) })

	return sidecar
}

// baoExec runs the bao CLI in the OpenBao sidecar with token and returns its trimmed stdout. A
// non-zero exit fails the test with stderr, which never carries a token: the CLI only prints one
// on stdout, when asked with -field=token.
func (h *Harness) baoExec(sidecar testcontainers.Container, token string, args ...string) string {
	h.t.Helper()

	cmd := append([]string{"bao"}, args...)

	code, reader, err := sidecar.Exec(h.ctx, cmd, tcexec.WithEnv([]string{"BAO_ADDR=" + forkOpenBaoAddr, "BAO_TOKEN=" + token}))
	if err != nil {
		h.t.Fatalf("exec bao %s: %v", args[0], err)
	}

	var stdout, stderr bytes.Buffer
	if _, err := stdcopy.StdCopy(&stdout, &stderr, reader); err != nil {
		h.t.Fatalf("read output of bao %s: %v", args[0], err)
	}

	if code != 0 {
		h.t.Fatalf("bao %s exited %d: %s", strings.Join(args[:min(2, len(args))], " "), code, stderr.String())
	}

	return strings.TrimSpace(stdout.String())
}

// containerEnv returns the environment of a container, as KEY=value entries.
func (h *Harness) containerEnv(containerID string) []string {
	h.t.Helper()

	inspect, err := h.docker.ContainerInspect(h.ctx, containerID, client.ContainerInspectOptions{})
	if err != nil {
		h.t.Fatalf("inspect container %s: %v", shortContainerID(containerID), err)
	}

	return inspect.Container.Config.Env
}

// TestForkSecretAllowlist covers the secret allowlist of the compose policy and the reload of the
// OpenBao token file against a real OpenBao, on one harness:
//
//  1. the first commit references a secret outside the allowlist of the target: the deploy is
//     denied before the secret is read (policy_denied with the external_secrets.DB violation), no
//     container is created, and the value is in neither the daemon log nor the outcome;
//  2. a commit with a reference inside the allowlist deploys, with the resolved value in the
//     environment of the container;
//  3. the token file is replaced by rename with a new token and the old one is revoked: a later
//     commit still deploys, so the daemon read the new token from the file.
//
// The token file is in a directory bind mounted into the daemon, never bound as a single file: a
// file bind pins the inode, and the container would never see the renamed file.
func TestForkSecretAllowlist(t *testing.T) {
	t.Parallel()
	requireStandaloneSuite(t)

	h := NewHarness(t, "fork-secret-allowlist")
	if h.isSwarmMode() {
		t.Skip("a Swarm target is always denied by the compose policy")
	}

	policy := "version: 1\ntargets:\n  \"default/" + forkSecretStack + "\":\n" +
		"    repository: http://gitserver/fork-secret-allowlist.git\n    reference: main\n" +
		"    secret_allowlist:\n      - kv:secret:prod/e2e-fork-secret/\n"

	policyPath := filepath.Join(h.workDir, "compose-policy.yaml")
	if err := os.WriteFile(policyPath, []byte(policy), 0o600); err != nil {
		t.Fatalf("write compose policy: %v", err)
	}

	baoDir := filepath.Join(h.workDir, "bao")
	if err := os.MkdirAll(baoDir, 0o750); err != nil {
		t.Fatalf("create token dir: %v", err)
	}

	tokenPath := filepath.Join(baoDir, "bao-token")
	if err := os.WriteFile(tokenPath, []byte(forkOpenBaoRootToken+"\n"), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}

	rec := h.newPreRollbackReceiver("proceed")

	h.BindDaemonFile(policyPath, "/config/compose-policy.yaml")
	h.BindDaemonFile(baoDir, "/config/bao")
	h.SetEnv("COMPOSE_POLICY_FILE", "/config/compose-policy.yaml")
	h.SetEnv("SECRET_PROVIDER", "openbao")
	h.SetEnv("SECRET_PROVIDER_SITE_URL", forkOpenBaoAddr)
	h.SetEnv("SECRET_PROVIDER_ACCESS_TOKEN_FILE", "/config/bao/bao-token")
	h.SetEnv("SECRET_PROVIDER_ACCESS_TOKEN_FILE_RELOAD", "true")
	h.SetEnv("HOOK_OUTCOME_URL", forkOutcomeHookURL)
	h.SetEnv("HOOK_SECRET", forkRollbackSecret)
	h.Start()
	rec.start()

	bao := h.startOpenBaoSidecar()
	h.baoExec(bao, forkOpenBaoRootToken, "kv", "put", "secret/prod/e2e-fork-secret/db", "PASS="+forkSecretAllowed)
	h.baoExec(bao, forkOpenBaoRootToken, "kv", "put", "secret/prod/other/db", "PASS="+forkSecretOther)

	object := "default/" + forkSecretStack

	// 1. A reference outside the allowlist: denied before any secret is read.
	h.WaitForLog("deployment denied by policy", 120*time.Second)

	if !h.daemonHasLog("external_secrets.DB=" + forkSecretForbiddenRef) {
		t.Fatal("daemon log must name the external_secrets.DB violation")
	}

	denied, iDenied := rec.waitOutcome(object, 0, 90*time.Second, "policy_denied on the forbidden reference", func(ev hooks.Event) bool {
		return ev.Outcome == hooks.OutcomePolicyDenied
	})

	want := []hooks.ViolationOut{{Field: "external_secrets.DB", Value: forkSecretForbiddenRef}}
	if !reflect.DeepEqual(denied.Violations, want) || denied.Stage != "pre-deploy" {
		t.Fatalf("policy_denied = %+v, want violations %+v in pre-deploy", denied, want)
	}

	if n := h.projectContainerCount(forkSecretStack); n != 0 {
		t.Fatalf("project %s has %d containers after a denied deploy, want 0", forkSecretStack, n)
	}

	// 2. A reference inside the allowlist: resolved and deployed.
	mark := h.LogMark()
	h.ReplaceInWorktree(".doco-cd.yml", forkSecretForbiddenRef, forkSecretAllowedRef)
	c2 := h.RepoPush("allowed secret reference").String()

	h.WaitForLogAfter(deployCompletedLog, mark, 120*time.Second)

	_, iDeployed := rec.waitOutcome(object, iDenied+1, 90*time.Second, "deployed with the allowed reference", func(ev hooks.Event) bool {
		return ev.Outcome == hooks.OutcomeDeployed && ev.Commit == c2
	})

	app := h.ContainerID(forkSecretStack, "app")
	if app == "" {
		t.Fatal("app container must run after the allowed deploy")
	}

	if env := h.containerEnv(app); !slices.Contains(env, "DB="+forkSecretAllowed) {
		t.Fatal("app container must have DB set to the resolved secret")
	}

	// 3. The token file is rewritten by rename, as a bao agent does, and the old token revoked.
	// The new token is an orphan: a child of the root token would be revoked with it.
	newToken := h.baoExec(bao, forkOpenBaoRootToken, "token", "create", "-orphan", "-policy=root", "-field=token")
	if newToken == "" {
		t.Fatal("bao token create returned no token")
	}

	tmpPath := tokenPath + ".tmp"
	if err := os.WriteFile(tmpPath, []byte(newToken+"\n"), 0o600); err != nil {
		t.Fatalf("write new token file: %v", err)
	}

	if err := os.Rename(tmpPath, tokenPath); err != nil {
		t.Fatalf("rename new token file: %v", err)
	}

	h.baoExec(bao, newToken, "token", "revoke", forkOpenBaoRootToken)

	mark = h.LogMark()
	h.ReplaceInWorktree("deploy/compose.yaml", "    command:", "    labels:\n      e2e.token: rotated\n    command:")
	c3 := h.RepoPush("label after the token rotation").String()

	h.WaitForLogAfter(deployCompletedLog, mark, 120*time.Second)

	rec.waitOutcome(object, iDeployed+1, 90*time.Second, "deployed after the token rotation", func(ev hooks.Event) bool {
		return ev.Outcome == hooks.OutcomeDeployed && ev.Commit == c3
	})

	if env := h.containerEnv(h.ContainerID(forkSecretStack, "app")); !slices.Contains(env, "DB="+forkSecretAllowed) {
		t.Fatal("app container must have DB set to the resolved secret after the token rotation")
	}

	// Neither the value outside the allowlist nor the tokens leak.
	logs := h.daemonLogs()
	for name, value := range map[string]string{"the forbidden secret": forkSecretOther, "the root token": forkOpenBaoRootToken, "the new token": newToken} {
		if strings.Contains(logs, value) {
			t.Errorf("the daemon log carries %s", name)
		}

		for _, req := range rec.recorded() {
			if bytes.Contains(req.Body, []byte(value)) {
				t.Errorf("request to %s carries %s", req.Path, name)
			}
		}
	}
}
