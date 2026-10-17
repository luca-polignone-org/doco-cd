package api

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/hooks"
	"github.com/kimdre/doco-cd/internal/logger"
	"github.com/kimdre/doco-cd/internal/rollback"
	"github.com/kimdre/doco-cd/internal/stages"
)

func TestProjectActions(t *testing.T) {
	t.Parallel()

	upstream := []any{"start", "stop", "restart", "recreate"}

	for name, cfg := range map[string]*app.Config{"nil": nil, "no hook": {}} {
		if got := projectActions(cfg); !reflect.DeepEqual(got, upstream) {
			t.Fatalf("%s: projectActions() = %v, want %v", name, got, upstream)
		}
	}

	if got := projectActions(&app.Config{HookPreRollbackURL: "https://gate.example/pre"}); !reflect.DeepEqual(got, append(upstream, "unblock")) {
		t.Fatalf("with hook: projectActions() = %v", got)
	}
}

// The action enum of the OpenAPI document gains unblock only with HOOK_PRE_ROLLBACK_URL.
func TestOpenAPIUnblockEnum(t *testing.T) {
	t.Parallel()

	enumOf := func(t *testing.T, cfg *app.Config) []any {
		t.Helper()

		routes, err := createRouteCatalog(&Handler{appConfig: cfg, log: logger.New(logger.LevelCritical)}, Mounts{}, newSchemaBuilder())
		if err != nil {
			t.Fatal(err)
		}

		for _, route := range routes {
			if route.Pattern != APIPath+"/project/{projectName}/{action}" {
				continue
			}

			for _, p := range route.Operations[0].Operation.Parameters {
				if p.Value.Name == "action" {
					return p.Value.Schema.Value.Enum
				}
			}
		}

		t.Fatal("project action parameter not found")

		return nil
	}

	if got := enumOf(t, &app.Config{}); len(got) != 4 {
		t.Fatalf("enum without hook = %v", got)
	}

	if got := enumOf(t, &app.Config{HookPreRollbackURL: "https://gate.example/pre"}); len(got) != 5 || got[4] != "unblock" {
		t.Fatalf("enum with hook = %v", got)
	}
}

func newUnblockMux(t *testing.T, hookURL, dataPath string, deny bool) *http.ServeMux {
	t.Helper()

	var h *Handler
	if deny {
		h = newForkAuthHandler(t, &forkFakeRuns{}, newForkEnforcer(t, forkDenyPolicy), true)
	} else {
		h = newForkAuthHandler(t, &forkFakeRuns{}, nil, false)
	}

	h.appConfig.HookPreRollbackURL = hookURL
	h.appConfig.DataMountPath = dataPath

	return newForkMux(t, h)
}

func TestUnblockProjectAction(t *testing.T) {
	startForkFakeDocker(t)

	const hook = "https://gate.example/pre"

	target := APIPath + "/project/allowed/unblock"

	t.Run("blocked target goes back to failed", func(t *testing.T) {
		data := t.TempDir()
		store := rollback.StoreFor(data)

		if err := store.PutFailure(rollback.Failure{Object: "default/allowed", CommitSHA: "c2", Stage: "deploy", State: rollback.StateBlocked, Decision: "block"}); err != nil {
			t.Fatal(err)
		}

		mux := newUnblockMux(t, hook, data, false)

		rr, body := serveFork(t, mux, http.MethodPost, target, "", true)
		if rr.Code != http.StatusOK || body["content"] != "unblocked" {
			t.Fatalf("status = %d, body = %v", rr.Code, body)
		}

		f, _, _ := store.GetFailure("default/allowed")
		if f.State != rollback.StateFailed || f.CommitSHA != "c2" {
			t.Fatalf("failure %+v", f)
		}

		if rr, body = serveFork(t, mux, http.MethodPost, target, "", true); rr.Code != http.StatusConflict {
			t.Fatalf("second unblock: status = %d, body = %v, want 409", rr.Code, body)
		}
	})

	t.Run("unblock emits unblocked once", func(t *testing.T) {
		outbox := filepath.Join(t.TempDir(), "outbox")

		o, err := hooks.NewOutbox(hooks.OutboxConfig{Dir: outbox, URL: "http://127.0.0.1:1/outcome"})
		if err != nil {
			t.Fatal(err)
		}

		stages.SetOutbox(o)
		t.Cleanup(func() { stages.SetOutbox(nil) })

		data := t.TempDir()
		if err := rollback.StoreFor(data).PutFailure(rollback.Failure{Object: "default/allowed", CommitSHA: "c2", Stage: "deploy", State: rollback.StateBlocked}); err != nil {
			t.Fatal(err)
		}

		mux := newUnblockMux(t, hook, data, false)

		for range 2 { // the second one is a 409 and emits nothing
			serveFork(t, mux, http.MethodPost, target, "", true)
		}

		files, err := filepath.Glob(filepath.Join(outbox, "*.json"))
		if err != nil || len(files) != 1 {
			t.Fatalf("outbox files %v, err %v, want one event", files, err)
		}

		raw, err := os.ReadFile(files[0])
		if err != nil {
			t.Fatal(err)
		}

		var e struct {
			Event hooks.Event `json:"event"`
		}

		if err := json.Unmarshal(raw, &e); err != nil || e.Event.Outcome != hooks.OutcomeUnblocked || e.Event.Object != "default/allowed" {
			t.Fatalf("event %s, err %v", raw, err)
		}
	})

	t.Run("POST only", func(t *testing.T) {
		rr, body := serveFork(t, newUnblockMux(t, hook, t.TempDir(), false), http.MethodGet, target, "", true)
		if rr.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, body = %v, want 405", rr.Code, body)
		}
	})

	t.Run("store error", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "data")
		if err := os.WriteFile(file, nil, 0o600); err != nil {
			t.Fatal(err)
		}

		rr, body := serveFork(t, newUnblockMux(t, hook, file, false), http.MethodPost, target, "", true)
		if rr.Code != http.StatusInternalServerError || body["error"] != "failed to unblock project" {
			t.Fatalf("status = %d, body = %v, want 500", rr.Code, body)
		}
	})

	t.Run("RBAC denies action/unblock", func(t *testing.T) {
		rr, body := serveFork(t, newUnblockMux(t, hook, t.TempDir(), true), http.MethodPost, target, "", true)
		requireForbidden(t, rr, body, "unblock without permission")
	})

	// Without the hook unblock is not an action of the fork: the request takes the upstream path,
	// which looks the project up in Docker (the fake daemon answers 500).
	t.Run("without HOOK_PRE_ROLLBACK_URL", func(t *testing.T) {
		data := t.TempDir()
		if err := rollback.StoreFor(data).PutFailure(rollback.Failure{Object: "default/allowed", State: rollback.StateBlocked}); err != nil {
			t.Fatal(err)
		}

		rr, body := serveFork(t, newUnblockMux(t, "", data, false), http.MethodPost, target, "", true)
		if rr.Code == http.StatusOK || rr.Code == http.StatusConflict {
			t.Fatalf("status = %d, body = %v: unblock answered without the hook", rr.Code, body)
		}

		if f, _, _ := rollback.StoreFor(data).GetFailure("default/allowed"); f.State != rollback.StateBlocked {
			t.Fatalf("failure %+v changed without the hook", f)
		}
	})
}
