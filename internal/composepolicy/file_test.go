package composepolicy

import (
	"os"
	"reflect"
	"testing"
)

func TestParse(t *testing.T) {
	ok := `
version: 1
default: {bind_allowlist: ["/srv/shared"]}
targets:
  "noi/poli-gate":
    repository: https://github.com/luca-polignone-org/poli-os-apps
    reference: main
    bind_allowlist: ["/srv/poli-gate/data/"]
    cap_add_allowlist: [NET_ADMIN]
`

	f, err := Parse([]byte(ok))
	if err != nil {
		t.Fatal(err)
	}

	tp := f.Targets["noi/poli-gate"]
	if tp.Reference != "main" || tp.BindAllowlist[0] != "/srv/poli-gate/data" {
		t.Fatalf("not normalized: %+v", tp)
	}

	bad := map[string]string{
		"version":         "version: 2",
		"glob key":        "version: 1\ntargets: {\"noi/*\": {repository: https://x/y}}",
		"no repo":         "version: 1\ntargets: {\"noi/a\": {}}",
		"bad object":      "version: 1\ntargets: {\"noi\": {repository: https://x/y}}",
		"empty context":   "version: 1\ntargets: {\"/a\": {repository: https://x/y}}",
		"empty project":   "version: 1\ntargets: {\"noi/\": {repository: https://x/y}}",
		"deep object":     "version: 1\ntargets: {\"noi/a/b\": {repository: https://x/y}}",
		"relative path":   "version: 1\ndefault: {bind_allowlist: [srv]}",
		"target path":     "version: 1\ntargets: {\"noi/a\": {repository: https://x/y, bind_allowlist: [srv]}}",
		"cap ALL":         "version: 1\ndefault: {cap_add_allowlist: [ALL]}",
		"cap CAP_ALL":     "version: 1\ndefault: {cap_add_allowlist: [cap_all]}",
		"cap empty":       "version: 1\ndefault: {cap_add_allowlist: [\"CAP_\"]}",
		"unknown key":     "version: 1\nfoo: 1",
		"unknown nested":  "version: 1\ndefault: {bind: [/a]}",
		"yaml":            "version: [",
		"default repo":    "version: 1\ndefault: {repository: https://x/y}",
		"default ref":     "version: 1\ndefault: {reference: main}",
		"default target":  "version: 1\ndefault: {config_target: t}",
		"two documents":   "version: 1\n---\nversion: 1",
		"broken trailing": "version: 1\n---\n[",
		"empty":           "",
	}
	for name, doc := range bad {
		if _, err := Parse([]byte(doc)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestParseNormalization(t *testing.T) {
	doc := `
version: 1
default: {cap_add_allowlist: [" cap_net_admin "]}
targets:
  "noi/a": {repository: " https://github.com/o/r ", config_target: t}
  "noi/b": {repository: "/srv/repos/x", reference: " dev "}
`

	f, err := Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}

	want := TargetPolicy{
		CapAddAllowlist: []string{"NET_ADMIN"},
	}
	if !reflect.DeepEqual(f.Default, want) {
		t.Errorf("default = %+v, want %+v", f.Default, want)
	}

	if got := f.Targets["noi/a"]; got.Repository != "https://github.com/o/r" || got.ConfigTarget != "t" {
		t.Errorf("target = %+v", got)
	}

	if got := f.Targets["noi/b"]; got.Repository != "file:///srv/repos/x" || got.Reference != "dev" {
		t.Errorf("target = %+v", got)
	}
}

func TestParseFixture(t *testing.T) {
	data, err := os.ReadFile("testdata/policy.yaml")
	if err != nil {
		t.Fatal(err)
	}

	f, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}

	if got := f.Targets["noi/poli-gate"].CapAddAllowlist; !reflect.DeepEqual(got, []string{"NET_ADMIN"}) {
		t.Errorf("cap_add_allowlist = %v", got)
	}
}
