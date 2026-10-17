package composepolicy

import (
	"reflect"
	"strings"
	"testing"
)

const secretRepo = "https://github.com/o/apps"

func secretPolicy(t *testing.T, body string) *File {
	t.Helper()

	f, err := Parse([]byte(body))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	return f
}

func worklogID() Identity {
	return Identity{Object: "default/worklog", Repository: secretRepo, Reference: "refs/heads/main", ConfigTarget: "worklog"}
}

const twoTargets = `version: 1
default:
  secret_allowlist: []
targets:
  default/worklog:
    repository: https://github.com/o/apps
    reference: refs/heads/main
    config_target: worklog
    secret_allowlist:
      - kv:secret:prod/worklog/
  default/gymlog:
    repository: https://github.com/o/apps
    reference: refs/heads/main
    config_target: gymlog
`

func TestParseSecretAllowlistPresence(t *testing.T) {
	absent := secretPolicy(t, "version: 1\ntargets:\n  default/w:\n    repository: "+secretRepo+"\n")
	if absent.Default.SecretAllowlist != nil || absent.Targets["default/w"].SecretAllowlist != nil {
		t.Fatal("an absent key must stay nil")
	}

	empty := secretPolicy(t, "version: 1\ndefault:\n  secret_allowlist: []\n")
	if empty.Default.SecretAllowlist == nil || len(*empty.Default.SecretAllowlist) != 0 {
		t.Fatalf("an empty list must be present and empty, got %v", empty.Default.SecretAllowlist)
	}

	for name, body := range map[string]string{
		"null":           "version: 1\ndefault:\n  secret_allowlist: null\n",
		"tilde":          "version: 1\ndefault:\n  secret_allowlist: ~\n",
		"empty entry":    "version: 1\ndefault:\n  secret_allowlist: [\"\"]\n",
		"bad charset":    "version: 1\ndefault:\n  secret_allowlist: [\"kv:secret:prod/w%rk/\"]\n",
		"dot segment":    "version: 1\ndefault:\n  secret_allowlist: [\"kv:secret:prod/../x/\"]\n",
		"too few fields": "version: 1\ndefault:\n  secret_allowlist: [\"kv:secret\"]\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(body)); err == nil || !strings.Contains(err.Error(), "secret_allowlist") {
				t.Fatalf("Parse() error = %v, want a secret_allowlist error", err)
			}
		})
	}
}

func TestCheckSecretsWithoutKey(t *testing.T) {
	f := secretPolicy(t, "version: 1\ntargets:\n  default/worklog:\n    repository: "+secretRepo+"\n")
	refs := map[string]string{"A": "kv:secret:prod/gymlog/db:PASS"}

	// The key is absent: a bound target resolves what it names, as upstream.
	if got := f.CheckSecrets(Identity{Object: "default/worklog", Repository: secretRepo}, "openbao", refs); got != nil {
		t.Fatalf("CheckSecrets() = %v, want nil without the key", got)
	}

	// With a compose policy, an object missing from targets never resolves secrets, key or not:
	// checkForkPolicies would deny it later, after the secrets were already read.
	want := []Violation{{Field: "target", Value: "default/other"}}
	if got := f.CheckSecrets(Identity{Object: "default/other"}, "openbao", refs); !reflect.DeepEqual(got, want) {
		t.Fatalf("CheckSecrets() = %v, want %v", got, want)
	}

	// Same for a deploy config bound to another repository.
	want = []Violation{{Field: "target", Value: "default/worklog"}}
	if got := f.CheckSecrets(Identity{Object: "default/worklog", Repository: "https://github.com/o/evil"}, "openbao", refs); !reflect.DeepEqual(got, want) {
		t.Fatalf("CheckSecrets() = %v, want %v", got, want)
	}
}

func TestCheckSecrets(t *testing.T) {
	f := secretPolicy(t, twoTargets)

	cases := []struct {
		name string
		id   Identity
		refs map[string]string
		want []Violation
	}{
		{"allowed", worklogID(), map[string]string{"DB": "kv:secret:prod/worklog/db:PASS"}, nil},
		{"explicit root namespace", worklogID(), map[string]string{"DB": "kv:root:secret:prod/worklog/db:PASS"}, nil},
		{"other app", worklogID(), map[string]string{"DB": "kv:secret:prod/gymlog/db:PASS"},
			[]Violation{{Field: "external_secrets.DB", Value: "kv:secret:prod/gymlog/db:PASS"}}},
		{"prefix by segment", worklogID(), map[string]string{"DB": "kv:secret:prod/worklog-old/db:PASS"},
			[]Violation{{Field: "external_secrets.DB", Value: "kv:secret:prod/worklog-old/db:PASS"}}},
		{"other namespace", worklogID(), map[string]string{"DB": "kv:team:secret:prod/worklog/db:PASS"},
			[]Violation{{Field: "external_secrets.DB", Value: "kv:team:secret:prod/worklog/db:PASS"}}},
		{"dot dot", worklogID(), map[string]string{"DB": "kv:secret:prod/worklog/../gymlog:PASS"},
			[]Violation{{Field: "external_secrets.DB", Value: "kv:secret:prod/worklog/../gymlog:PASS"}}},
		{"double slash", worklogID(), map[string]string{"DB": "kv:secret:prod/worklog//db:PASS"},
			[]Violation{{Field: "external_secrets.DB", Value: "kv:secret:prod/worklog//db:PASS"}}},
		{"leading slash", worklogID(), map[string]string{"DB": "kv:secret:/prod/worklog/db:PASS"},
			[]Violation{{Field: "external_secrets.DB", Value: "kv:secret:/prod/worklog/db:PASS"}}},
		{"bad charset in key", worklogID(), map[string]string{"DB": "kv:secret:prod/worklog/db:PA$S"},
			[]Violation{{Field: "external_secrets.DB", Value: "kv:secret:prod/worklog/db:PA$S"}}},
		{"pki by prefix never", worklogID(), map[string]string{"C": "pki:pki:prod/worklog/x"},
			[]Violation{{Field: "external_secrets.C", Value: "pki:pki:prod/worklog/x"}}},
		{"sorted", worklogID(), map[string]string{"B": "kv:secret:x/y:k", "A": "kv:secret:x/z:k"},
			[]Violation{{Field: "external_secrets.A", Value: "kv:secret:x/z:k"}, {Field: "external_secrets.B", Value: "kv:secret:x/y:k"}}},
		{"default deny-all", Identity{Object: "default/gymlog", Repository: secretRepo, Reference: "refs/heads/main", ConfigTarget: "gymlog"},
			map[string]string{"DB": "kv:secret:prod/gymlog/db:PASS"},
			[]Violation{{Field: "external_secrets.DB", Value: "kv:secret:prod/gymlog/db:PASS"}}},
		{"name of another target", Identity{Object: "default/worklog", Repository: secretRepo, Reference: "refs/heads/main", ConfigTarget: "gymlog"},
			map[string]string{"DB": "kv:secret:prod/worklog/db:PASS"},
			[]Violation{{Field: "target", Value: "default/worklog"}}},
		{"target absent", Identity{Object: "default/unknown", Repository: secretRepo},
			map[string]string{"DB": "kv:secret:prod/worklog/db:PASS"},
			[]Violation{{Field: "target", Value: "default/unknown"}}},
		{"no refs", Identity{Object: "default/unknown"}, nil, nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := f.CheckSecrets(tc.id, "openbao", tc.refs); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("CheckSecrets() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestCheckSecretsPKILiteral(t *testing.T) {
	f := secretPolicy(t, `version: 1
targets:
  default/worklog:
    repository: https://github.com/o/apps
    secret_allowlist:
      - pki-role:pki:worklog-role:worklog.example.com
      - kv:secret:prod/worklog/
`)
	id := Identity{Object: "default/worklog", Repository: secretRepo}

	if got := f.CheckSecrets(id, "openbao", map[string]string{"C": "pki-role:pki:worklog-role:worklog.example.com"}); got != nil {
		t.Fatalf("exact pki-role entry must match, got %v", got)
	}

	if got := f.CheckSecrets(id, "openbao", map[string]string{"C": "pki-role:pki:worklog-role:other.example.com"}); len(got) != 1 {
		t.Fatalf("a different common name must not match, got %v", got)
	}
}

func TestCheckSecretsOtherProviderLiteralPrefix(t *testing.T) {
	f := secretPolicy(t, `version: 1
targets:
  default/worklog:
    repository: https://github.com/o/apps
    secret_allowlist:
      - "proj:prod:/worklog/"
`)
	id := Identity{Object: "default/worklog", Repository: secretRepo}

	if got := f.CheckSecrets(id, "infisical", map[string]string{"A": "proj:prod:/worklog/db"}); got != nil {
		t.Fatalf("literal prefix must match, got %v", got)
	}

	if got := f.CheckSecrets(id, "infisical", map[string]string{"A": "proj:prod:/gymlog/db"}); len(got) != 1 {
		t.Fatalf("another prefix must not match, got %v", got)
	}
}

func TestParseSecretAllowlistEdgeCases(t *testing.T) {
	// A decode error is reported by Parse itself, not by the null check.
	if _, err := Parse([]byte("version: 1\ndefault: [\n")); err == nil || !strings.Contains(err.Error(), "decode compose policy") {
		t.Fatalf("Parse() error = %v, want the decode error", err)
	}

	for name, body := range map[string]string{
		"null through alias": "version: 1\ndefault:\n  bind_allowlist: &n null\n  secret_allowlist: *n\n",
		"aliased key, null":  "version: 1\ndefault:\n  bind_allowlist: []\n  &k secret_allowlist: [\"kv:secret:x/\"]\ntargets:\n  default/w:\n    repository: " + secretRepo + "\n    *k : null\n",
		"null in a target":   "version: 1\ntargets:\n  default/w:\n    repository: " + secretRepo + "\n    secret_allowlist: null\n",
		"kv too many fields": "version: 1\ndefault:\n  secret_allowlist: [\"kv:a:b:c:d\"]\n",
		"kv bad namespace":   "version: 1\ndefault:\n  secret_allowlist: [\"kv:t$:secret:prod/\"]\n",
		"kv only slash":      "version: 1\ndefault:\n  secret_allowlist: [\"kv:secret:/\"]\n",
		"pki too short":      "version: 1\ndefault:\n  secret_allowlist: [\"pki:pki\"]\n",
		"pki-role too short": "version: 1\ndefault:\n  secret_allowlist: [\"pki-role:pki:role\"]\n",
		"pki too long":       "version: 1\ndefault:\n  secret_allowlist: [\"pki:a:b:c:d\"]\n",
		"pki empty field":    "version: 1\ndefault:\n  secret_allowlist: [\"pki::x\"]\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(body)); err == nil || !strings.Contains(err.Error(), "secret_allowlist") {
				t.Fatalf("Parse() error = %v, want a secret_allowlist error", err)
			}
		})
	}

	// An aliased key with a list is a normal secret_allowlist.
	aliased := secretPolicy(t, "version: 1\ndefault:\n  &k secret_allowlist: [\"kv:secret:x/\"]\ntargets:\n  default/w:\n    repository: "+secretRepo+"\n    *k : [\"kv:secret:y/\"]\n")
	if got := aliased.Targets["default/w"].SecretAllowlist; got == nil || !reflect.DeepEqual(*got, []string{"kv:secret:y/"}) {
		t.Fatalf("aliased key entries = %v, want [kv:secret:y/]", got)
	}

	f := secretPolicy(t, "version: 1\ndefault:\n  secret_allowlist: [\"  kv:secret:prod/  \", \"pki:ns:pki:x.example.com\", \"other:literal\"]\n")
	if got := *f.Default.SecretAllowlist; !reflect.DeepEqual(got, []string{"kv:secret:prod/", "pki:ns:pki:x.example.com", "other:literal"}) {
		t.Fatalf("entries = %q, want trimmed entries", got)
	}
}

func TestCheckSecretsOpenbaoEdgeCases(t *testing.T) {
	f := secretPolicy(t, `version: 1
targets:
  default/worklog:
    repository: https://github.com/o/apps
    secret_allowlist:
      - pki-role:pki:worklog-role:worklog.example.com
      - kv:team:secret:prod/worklog
`)
	id := Identity{Object: "default/worklog", Repository: secretRepo}

	for name, tc := range map[string]struct {
		ref     string
		allowed bool
	}{
		"namespace entry, exact name":   {"kv:team:secret:prod/worklog:K", true},
		"namespace entry, below prefix": {"kv:team:secret:prod/worklog/db:K", true},
		"namespace entry, root ref":     {"kv:secret:prod/worklog/db:K", false},
		"kv too few fields":             {"kv:secret:K", false},
		"unknown kind":                  {"aws:secret:prod/worklog/db:K", false},
		"dot key":                       {"kv:team:secret:prod/worklog:..", false},
	} {
		t.Run(name, func(t *testing.T) {
			got := f.CheckSecrets(id, "openbao", map[string]string{"A": tc.ref})
			if (got == nil) != tc.allowed {
				t.Fatalf("CheckSecrets(%q) = %v, want allowed=%v", tc.ref, got, tc.allowed)
			}
		})
	}
}
