package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/logger"
)

// The goldens are the raw /openapi/rest.json and /openapi/webhook.json documents of upstream
// v0.129.0 (commit 6216106f), served by RegisterRoutes with app.Config{OpenAPIEnabled: true}, the
// handler built as below and app.Version "dev". They are committed exactly as upstream produced them.
const (
	forkRestGolden    = "openapi-upstream-v0.129.0.json"
	forkWebhookGolden = "openapi-webhook-upstream-v0.129.0.json"

	// forkDeployConfigSchema is the deploy config component; the fork adds its rollback property.
	forkDeployConfigSchema = "github.com_kimdre_doco-cd_internal_config_deploy.Config"
	// forkRollbackSchema is the component of deploy.RollbackConfig, added by the fork.
	forkRollbackSchema = "github.com_kimdre_doco-cd_internal_config_deploy.RollbackConfig"
	// forkProjectActionPath is the operation whose action enum gains unblock.
	forkProjectActionPath = "/v1/api/project/{projectName}/{action}"
)

// servedOpenAPIDocument returns the document RegisterRoutes serves at path for cfg, parsed and
// normalized (normalizeOpenAPI).
func servedOpenAPIDocument(t *testing.T, cfg *app.Config, path string) map[string]any {
	t.Helper()

	handler := &Handler{appConfig: cfg, log: logger.New(logger.LevelCritical)}

	mux := http.NewServeMux()
	if _, err := RegisterRoutes(mux, handler, Mounts{}); err != nil {
		t.Fatalf("register routes: %v", err)
	}

	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))

	if response.Code != http.StatusOK {
		t.Fatalf("GET %s status = %d, want %d", path, response.Code, http.StatusOK)
	}

	return parseOpenAPI(t, response.Body.Bytes())
}

func parseOpenAPI(t *testing.T, data []byte) map[string]any {
	t.Helper()

	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatalf("parse OpenAPI document: %v", err)
	}

	normalizeOpenAPI(document)

	return document
}

// normalizeOpenAPI drops the default of every boolean schema. Upstream openapi3gen is
// nondeterministic here: in about 1 run in 20 on v0.127.0 every boolean default of the deploy
// config components flips from true to false at once, so a raw byte comparison would be flaky.
// The same normalization is applied to both sides of every comparison.
func normalizeOpenAPI(node any) {
	switch value := node.(type) {
	case map[string]any:
		if value["type"] == "boolean" {
			delete(value, "default")
		}

		for _, child := range value {
			normalizeOpenAPI(child)
		}
	case []any:
		for _, child := range value {
			normalizeOpenAPI(child)
		}
	}
}

// openAPIObject returns document[keys[0]][keys[1]]..., failing when a key is missing.
func openAPIObject(t *testing.T, document map[string]any, keys ...string) map[string]any {
	t.Helper()

	current := document

	for _, key := range keys {
		next, ok := current[key].(map[string]any)
		if !ok {
			t.Fatalf("OpenAPI document has no object at %q", keys)
		}

		current = next
	}

	return current
}

func readOpenAPIGolden(t *testing.T, name string) map[string]any {
	t.Helper()

	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read golden %s: %v", name, err)
	}

	return parseOpenAPI(t, data)
}

// TestForkOpenAPIMatchesUpstreamWithoutKeys is the non-regression check of the fork on the API
// documents: with no new app key they equal the upstream v0.129.0 documents except for exactly
// two additive pieces, the rollback property of the deploy config schema and the RollbackConfig
// component. rollback is a per-repository deploy config key that no app key gates (without
// HOOK_PRE_ROLLBACK_URL it still works and ends blocked), so the schema documents it whatever
// the app config; hiding it would document the API wrongly. Any other difference fails.
func TestForkOpenAPIMatchesUpstreamWithoutKeys(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct{ path, golden string }{
		{OpenAPIRestPath, forkRestGolden},
		{OpenAPIWebhookPath, forkWebhookGolden},
	} {
		t.Run(testCase.golden, func(t *testing.T) {
			t.Parallel()

			upstream := readOpenAPIGolden(t, testCase.golden)
			fork := servedOpenAPIDocument(t, &app.Config{OpenAPIEnabled: true}, testCase.path)

			schemas := openAPIObject(t, fork, "components", "schemas")
			if _, ok := schemas[forkRollbackSchema]; !ok {
				t.Fatalf("fork document is missing the %s component", forkRollbackSchema)
			}

			delete(schemas, forkRollbackSchema)

			properties := openAPIObject(t, schemas, forkDeployConfigSchema, "properties")
			if _, ok := properties["rollback"]; !ok {
				t.Fatalf("fork document is missing the rollback property of %s", forkDeployConfigSchema)
			}

			delete(properties, "rollback")

			if !reflect.DeepEqual(fork, upstream) {
				got, _ := json.MarshalIndent(fork, "", " ")      //nolint:errchkjson // decoded JSON always encodes
				want, _ := json.MarshalIndent(upstream, "", " ") //nolint:errchkjson // decoded JSON always encodes

				t.Fatalf("fork OpenAPI document differs from upstream beyond the rollback schema\ngot:\n%s\nwant:\n%s", got, want)
			}
		})
	}
}

// TestForkOpenAPIUnblockOnlyWithPreRollbackKey checks that HOOK_PRE_ROLLBACK_URL changes the
// documents only by adding unblock to the project action enum.
func TestForkOpenAPIUnblockOnlyWithPreRollbackKey(t *testing.T) {
	t.Parallel()

	keyed := &app.Config{OpenAPIEnabled: true, HookPreRollbackURL: "http://127.0.0.1:1/pre-rollback"}

	for _, path := range []string{OpenAPIRestPath, OpenAPIWebhookPath} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()

			without := servedOpenAPIDocument(t, &app.Config{OpenAPIEnabled: true}, path)
			with := servedOpenAPIDocument(t, keyed, path)

			if path == OpenAPIRestPath {
				removeUnblockAction(t, with)
			}

			if !reflect.DeepEqual(with, without) {
				t.Fatalf("HOOK_PRE_ROLLBACK_URL changes the %s document beyond the unblock action", path)
			}
		})
	}
}

// removeUnblockAction checks that the project action enum ends with unblock and removes it.
func removeUnblockAction(t *testing.T, document map[string]any) {
	t.Helper()

	operation := openAPIObject(t, document, "paths", forkProjectActionPath, "post")

	parameters, _ := operation["parameters"].([]any)
	for _, parameter := range parameters {
		p, _ := parameter.(map[string]any)
		if p["name"] != "action" {
			continue
		}

		schema := openAPIObject(t, p, "schema")
		enum, _ := schema["enum"].([]any)

		if len(enum) == 0 || slices.Index(enum, any(unblockAction)) != len(enum)-1 {
			t.Fatalf("project action enum = %v, want the upstream values followed by %q", enum, unblockAction)
		}

		schema["enum"] = enum[:len(enum)-1]

		return
	}

	t.Fatalf("operation %s has no action parameter", forkProjectActionPath)
}
