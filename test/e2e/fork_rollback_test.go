//go:build e2e

package e2e

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/kimdre/doco-cd/internal/hooks"
)

const (
	forkRollbackStack  = "e2e-fork-rollback"
	forkRollbackAPIKey = "e2e-fork-rollback-api-key" // #nosec G101 -- test fixture, not a real credential.
	forkRollbackSecret = "e2e-fork-rollback-hook"    // #nosec G101 -- test fixture, not a real credential.
	// forkRollbackHookURL is on the daemon's own loopback: the receiver shares its network
	// namespace, like the JWKS sidecar of TestForkRBAC.
	forkRollbackHookURL = "http://127.0.0.1:8090/pre-rollback"

	forkRollbackFinished = "deployment rollback finished"
	forkRollbackPoisoned = "deployment of this commit was rolled back or blocked, skipping"
)

// forkRollbackReceiverScript answers pre_rollback with the decision in /rec/decision and appends
// every request (X-Doco-Signature and the raw body) to /rec/requests.jsonl. It does not verify
// the signature itself: the test does, with hooks.Verify, on the recorded raw body.
const forkRollbackReceiverScript = `import base64, http.server, json


class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200)
        self.end_headers()

    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("Content-Length", "0")))
        with open("/rec/requests.jsonl", "a") as f:
            f.write(json.dumps({"signature": self.headers.get("X-Doco-Signature", ""),
                                "body": base64.b64encode(body).decode()}) + "\n")
        with open("/rec/decision") as f:
            decision = f.read().strip()
        out = json.dumps({"decision": decision}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(out)))
        self.end_headers()
        self.wfile.write(out)

    def log_message(self, *args):
        pass


http.server.HTTPServer(("127.0.0.1", 8090), Handler).serve_forever()
`

// preRollbackReceiver runs forkRollbackReceiverScript in the daemon's network namespace, with
// the daemon's data volume mounted read-only so the test can read the fork store through it
// (the daemon image is distroless and has no shell).
type preRollbackReceiver struct {
	h        *Harness
	dir      string
	sidecar  testcontainers.Container
	restarts int
}

// newPreRollbackReceiver prepares the receiver's directory with the first decision. Call before
// Start; start the sidecar with start once the daemon runs.
func (h *Harness) newPreRollbackReceiver(decision string) *preRollbackReceiver {
	h.t.Helper()

	r := &preRollbackReceiver{h: h, dir: filepath.Join(h.workDir, "prerollback")}

	if err := os.MkdirAll(r.dir, 0o750); err != nil {
		h.t.Fatalf("create receiver dir: %v", err)
	}

	// The sidecar runs as root and appends to requests.jsonl in this host directory.
	if err := os.Chmod(r.dir, 0o777); err != nil { //nolint:gosec // test-only directory shared with a container
		h.t.Fatalf("chmod receiver dir: %v", err)
	}

	if err := os.WriteFile(filepath.Join(r.dir, "receiver.py"), []byte(forkRollbackReceiverScript), 0o644); err != nil { //nolint:gosec // read by the sidecar
		h.t.Fatalf("write receiver script: %v", err)
	}

	r.setDecision(decision)

	return r
}

func (r *preRollbackReceiver) setDecision(decision string) {
	r.h.t.Helper()

	if err := os.WriteFile(filepath.Join(r.dir, "decision"), []byte(decision+"\n"), 0o644); err != nil { //nolint:gosec // read by the sidecar
		r.h.t.Fatalf("write decision: %v", err)
	}
}

// start runs the sidecar in the current daemon's network namespace. A restarted daemon has a new
// namespace, so start is called again after KillAndRestartDaemon.
func (r *preRollbackReceiver) start() {
	h := r.h
	h.t.Helper()

	if r.sidecar != nil {
		h.terminateContainer(r.sidecar)
	}

	r.restarts++
	daemonID := h.daemon.GetContainerID()

	sidecar, err := testcontainers.GenericContainer(h.ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image: "python:3.13-alpine",
			Name:  h.containerName(fmt.Sprintf("prerollback-%d", r.restarts)),
			Cmd:   []string{"python3", "/rec/receiver.py"},
			HostConfigModifier: func(hc *container.HostConfig) {
				hc.NetworkMode = container.NetworkMode("container:" + daemonID)
				hc.Binds = append(hc.Binds, r.dir+":/rec", h.dataVolume+":/data:ro")
			},
			WaitingFor: wait.ForExec([]string{"wget", "-q", "-O", "/dev/null", "http://127.0.0.1:8090/"}).
				WithStartupTimeout(60 * time.Second).
				WithPollInterval(500 * time.Millisecond),
		},
		Started: true,
	})
	if err != nil {
		h.t.Fatalf("start pre_rollback receiver: %v", err)
	}

	r.sidecar = sidecar

	h.t.Cleanup(func() { h.terminateContainer(sidecar) })
}

// requests returns the pre_rollback requests received so far, each checked against the HMAC.
func (r *preRollbackReceiver) requests() []hooks.PreRollbackRequest {
	h := r.h
	h.t.Helper()

	data, err := os.ReadFile(filepath.Join(r.dir, "requests.jsonl"))
	if os.IsNotExist(err) {
		return nil
	}

	if err != nil {
		h.t.Fatalf("read recorded requests: %v", err)
	}

	var out []hooks.PreRollbackRequest

	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		var rec struct {
			Signature string `json:"signature"`
			Body      string `json:"body"`
		}

		if err := json.Unmarshal(scanner.Bytes(), &rec); err != nil {
			h.t.Fatalf("decode recorded request %q: %v", scanner.Text(), err)
		}

		body, err := base64.StdEncoding.DecodeString(rec.Body)
		if err != nil {
			h.t.Fatalf("decode recorded body: %v", err)
		}

		if err := hooks.Verify([]byte(forkRollbackSecret), rec.Signature, body, time.Now(), time.Hour); err != nil {
			h.t.Fatalf("pre_rollback request %s is not signed with HOOK_SECRET: %v", body, err)
		}

		var req hooks.PreRollbackRequest
		if err := json.Unmarshal(body, &req); err != nil {
			h.t.Fatalf("decode pre_rollback body %s: %v", body, err)
		}

		out = append(out, req)
	}

	return out
}

// storeFile returns a file of the daemon's data volume, or "" when it does not exist.
func (r *preRollbackReceiver) storeFile(path string) string {
	r.h.t.Helper()

	return r.h.ExecOutput(r.sidecar.GetContainerID(), "sh", "-c", "cat "+path+" 2>/dev/null || true")
}

// failure returns the state and commit of the target's failure record, or "" without one.
func (r *preRollbackReceiver) failure() (state, commit string) {
	r.h.t.Helper()

	raw := r.storeFile("/data/fork/failures/default/" + forkRollbackStack + ".json")
	if raw == "" {
		return "", ""
	}

	var f struct {
		State     string `json:"state"`
		CommitSHA string `json:"commit_sha"`
	}

	if err := json.Unmarshal([]byte(raw), &f); err != nil {
		r.h.t.Fatalf("decode failure record %q: %v", raw, err)
	}

	return f.State, f.CommitSHA
}

// lastGoodRef returns the commit the mirror's lastgood ref of the target points to, loose or packed.
func (r *preRollbackReceiver) lastGoodRef() string {
	r.h.t.Helper()

	ref := "refs/doco-cd/lastgood/default/" + forkRollbackStack
	script := fmt.Sprintf("find /data -path '*/mirror/%s' -exec cat {} \\; ; "+
		"find /data -path '*/mirror/packed-refs' -exec grep -h ' %s$' {} \\; ; true", ref, ref)

	fields := strings.Fields(r.h.ExecOutput(r.sidecar.GetContainerID(), "sh", "-c", script))
	if len(fields) == 0 {
		return ""
	}

	return fields[0]
}

// containerImageID returns the ID of the image a container runs, and the image it was created with.
func (h *Harness) containerImageID(containerID string) (id, configured string) {
	h.t.Helper()

	inspect, err := h.docker.ContainerInspect(h.ctx, containerID, client.ContainerInspectOptions{})
	if err != nil {
		h.t.Fatalf("inspect container %s: %v", shortContainerID(containerID), err)
	}

	return inspect.Container.Image, inspect.Container.Config.Image
}

func (h *Harness) writeWorktree(files map[string]string) {
	h.t.Helper()

	for rel, content := range files {
		if err := os.WriteFile(filepath.Join(h.worktree, rel), []byte(content), 0o600); err != nil {
			h.t.Fatalf("write %s: %v", rel, err)
		}
	}
}

// forkRollbackBroken is a commit whose deploy fails after the containers were created (post_start
// exits 1), with an image and a service env_file that differ from the last good.
func forkRollbackBroken(greeting string) map[string]string {
	return map[string]string{
		".doco-cd.yml": "name: e2e-fork-rollback\nworking_dir: deploy\nrollback:\n  enabled: true\n  timeout: 2m\n" +
			"  pre_rollback_timeout: 10s\nenvironment:\n  HOOK_EXIT: \"1\"\n",
		"deploy/compose.yaml": "include:\n  - extra.yaml\nservices:\n  app:\n    image: nginx:1.29-alpine\n" +
			"    command: [\"sleep\", \"600\"]\n    env_file: app.env\n    post_start:\n" +
			"      - command: [\"sh\", \"-c\", \"exit ${HOOK_EXIT:-0}\"]\n",
		"deploy/app.env": "GREETING=" + greeting + "\n",
	}
}

// TestForkRollback covers the rollback flow against a real daemon, on one harness:
//
//  1. the first deploy (c1) records the last good: the JSON in the store and the mirror ref;
//  2. a broken commit (c2) asks pre_rollback with a signed request; proceed rolls back to the
//     image ID of c1 (image: sha256:<id>, pull_policy: never), with the include and the service
//     env_file of c1, and the next poll skips c2;
//  3. a broken commit (c2b) with block stays as it is (blocked); after a restart of the daemon
//     the next polls do not recreate the containers;
//  4. unblock puts the failure back to failed, and the next poll retries c2b with a full
//     recreate, which fails and blocks again;
//  5. a healthy commit (c3) deploys and removes the failure.
func TestForkRollback(t *testing.T) {
	t.Parallel()
	requireStandaloneSuite(t)

	h := NewHarness(t, "fork-rollback")
	if h.isSwarmMode() {
		t.Skip("rollback is Compose only: a Swarm target with rollback is denied by the policy")
	}

	rec := h.newPreRollbackReceiver("proceed")

	h.EnableAPI(forkRollbackAPIKey)
	h.SetEnv("HOOK_PRE_ROLLBACK_URL", forkRollbackHookURL)
	h.SetEnv("HOOK_SECRET", forkRollbackSecret)
	h.Start()
	rec.start()

	// 1. c1 deploys and becomes the last good.
	h.WaitForLog(deployCompletedLog, 120*time.Second)

	head, err := h.repo.Head()
	if err != nil {
		t.Fatalf("read HEAD: %v", err)
	}

	c1 := head.Hash().String()

	// The last good is recorded after the cleanup stage, store first, then the mirror ref.
	h.WaitFor(30*time.Second, "last good recorded in the store and the mirror", func() bool {
		return strings.Contains(rec.storeFile("/data/fork/lastgood/default/"+forkRollbackStack+".json"), c1) &&
			rec.lastGoodRef() == c1
	})

	appC1 := h.ContainerID(forkRollbackStack, "app")
	c1ImageID, _ := h.containerImageID(appC1)

	// 2. c2 fails after its containers started; pre_rollback proceeds; c1 comes back.
	mark := h.LogMark()
	h.writeWorktree(forkRollbackBroken("from-c2"))
	c2 := h.RepoPush("broken c2").String()

	h.WaitForLogLineAfter(mark, 180*time.Second, forkRollbackFinished, `"state":"rolled_back"`)

	reqs := rec.requests()
	if len(reqs) != 1 {
		t.Fatalf("pre_rollback requests = %d, want 1", len(reqs))
	}

	want := hooks.PreRollbackRequest{
		Schema: hooks.PreRollbackSchema, Object: "default/" + forkRollbackStack,
		FailedCommit: c2, LastGoodCommit: c1, Stage: "deploy", Error: reqs[0].Error,
	}
	if reqs[0] != want || reqs[0].Error == "" {
		t.Fatalf("pre_rollback request = %+v, want %+v with an error", reqs[0], want)
	}

	app := h.ContainerID(forkRollbackStack, "app")
	if app == "" {
		t.Fatal("app must run after the rollback")
	}

	imageID, configured := h.containerImageID(app)
	if imageID != c1ImageID || !strings.HasPrefix(configured, "sha256:") {
		t.Fatalf("app image after rollback = %s (configured %q), want the image ID of c1 %s", imageID, configured, c1ImageID)
	}

	if got := h.ExecOutput(app, "printenv", "GREETING"); got != "from-c1" {
		t.Fatalf("GREETING after rollback = %q, want the env_file of c1", got)
	}

	if h.ContainerID(forkRollbackStack, "extra") == "" {
		t.Fatal("the included service must run after the rollback")
	}

	if state, commit := rec.failure(); state != "rolled_back" || commit != c2 {
		t.Fatalf("failure = %s/%s, want rolled_back/%s", state, commit, c2)
	}

	h.WaitForLogAfter(forkRollbackPoisoned, mark, 60*time.Second)

	if got := h.ContainerID(forkRollbackStack, "app"); got != app {
		t.Fatal("the poll after the rollback recreated the containers")
	}

	// 3. c2b fails too; pre_rollback blocks; the containers of c2b stay, also across a restart.
	rec.setDecision("block")

	mark = h.LogMark()
	h.writeWorktree(forkRollbackBroken("from-c2b"))
	c2b := h.RepoPush("broken c2b").String()

	h.WaitForLogLineAfter(mark, 180*time.Second, forkRollbackFinished, `"state":"blocked"`, `"decision":"block"`)

	if state, commit := rec.failure(); state != "blocked" || commit != c2b {
		t.Fatalf("failure = %s/%s, want blocked/%s", state, commit, c2b)
	}

	blocked := h.ContainerID(forkRollbackStack, "app")
	if id, _ := h.containerImageID(blocked); id == c1ImageID {
		t.Fatal("a blocked failure must not roll back")
	}

	h.KillAndRestartDaemon()
	rec.start()

	mark = h.LogMark()
	h.WaitForLogOccurrencesAfter(forkRollbackPoisoned, mark, 2, 90*time.Second)

	if got := h.ContainerID(forkRollbackStack, "app"); got != blocked {
		t.Fatalf("app container after restart = %s, want the blocked one %s untouched", got, blocked)
	}

	// 4. unblock: the failure is failed again with c2b, and the next poll retries it.
	mark = h.LogMark()
	h.APIRequest(http.MethodPost, "/v1/api/project/"+forkRollbackStack+"/unblock", http.StatusOK)

	// A poll may already be retrying, in which case the record can be blocked again by now.
	state, commit := rec.failure()
	if commit != c2b || (state != "failed" && !strings.Contains(h.logsSince(mark), "last deployment attempt failed, retrying")) {
		t.Fatalf("failure after unblock = %s/%s, want failed/%s", state, commit, c2b)
	}

	h.WaitForLogAfter("last deployment attempt failed, retrying", mark, 60*time.Second)
	h.WaitForContainerRecreate(forkRollbackStack, "app", blocked, 120*time.Second)
	h.WaitForLogLineAfter(mark, 180*time.Second, forkRollbackFinished, `"state":"blocked"`)

	// 5. c3 is healthy: it deploys and closes the failure.
	mark = h.LogMark()
	h.ReplaceInWorktree(".doco-cd.yml", `HOOK_EXIT: "1"`, `HOOK_EXIT: "0"`)
	h.ReplaceInWorktree("deploy/compose.yaml", "nginx:1.29-alpine", "alpine:3.22")
	h.writeWorktree(map[string]string{"deploy/app.env": "GREETING=from-c3\n"})
	c3 := h.RepoPush("healthy c3")

	h.WaitForLogAfter(deployCompletedLog, mark, 180*time.Second)
	h.WaitFor(30*time.Second, "failure removed", func() bool {
		state, _ := rec.failure()
		return state == ""
	})

	if got := h.ExecOutput(h.ContainerID(forkRollbackStack, "app"), "printenv", "GREETING"); got != "from-c3" {
		t.Fatalf("GREETING after c3 = %q", got)
	}

	h.WaitFor(30*time.Second, "lastgood ref moved to c3", func() bool { return rec.lastGoodRef() == c3.String() })
}
