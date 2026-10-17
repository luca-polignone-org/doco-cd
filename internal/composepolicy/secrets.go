package composepolicy

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v4"
)

// The secret allowlist (Portico fork, spec 4) binds the external_secrets references a target may
// resolve to a list of prefixes. The token of a Doco-CD instance reads the secrets of every app it
// serves, and the deploy config that names the references lives in the app repository: without
// the allowlist one app could resolve another app's secrets.

// openbaoProvider is the name OpenBao's provider reports (openbao.Name); it is repeated here to
// keep the policy package free of provider dependencies.
const openbaoProvider = "openbao"

// secretSegment is the positive charset of every namespace, engine, path segment and key.
var secretSegment = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

var errSecretEntry = errors.New("invalid secret reference")

// rejectNullSecretAllowlist refuses secret_allowlist: null or ~ (key or value also through an alias) anywhere
// in the file: decoded into *[]string it would read as an absent key, turning a deny-all into no
// check at all.
func rejectNullSecretAllowlist(data []byte) error {
	var root yaml.Node
	if err := yaml.NewDecoder(bytes.NewReader(data)).Decode(&root); err != nil {
		return nil // Parse reports the decode error itself
	}

	var walk func(n *yaml.Node) error

	walk = func(n *yaml.Node) error {
		if n.Kind == yaml.MappingNode {
			for i := 0; i+1 < len(n.Content); i += 2 {
				k, v := resolveAlias(n.Content[i]), resolveAlias(n.Content[i+1])

				if k.Value == "secret_allowlist" && v.Kind == yaml.ScalarNode && v.ShortTag() == "!!null" {
					return errors.New("compose policy: secret_allowlist must be a list, null is ambiguous")
				}
			}
		}

		for _, c := range n.Content {
			if err := walk(c); err != nil {
				return err
			}
		}

		return nil
	}

	return walk(&root)
}

// resolveAlias follows a chain of YAML aliases to the node it names: both the key and the value
// of secret_allowlist may be aliases (&k secret_allowlist ... *k : null).
func resolveAlias(n *yaml.Node) *yaml.Node {
	for n.Kind == yaml.AliasNode && n.Alias != nil {
		n = n.Alias
	}

	return n
}

// openbaoRef is an OpenBao reference split as upstream does (openbao.parseReference).
type openbaoRef struct{ kind, namespace, engine, name, key string }

// splitOpenbaoRef splits kv:[ns:]engine:name:key; pki and pki-role are only recognized, never
// split, because they match literal entries only.
func splitOpenbaoRef(ref string) (openbaoRef, bool) {
	parts := strings.Split(ref, ":")

	switch parts[0] {
	case "kv":
		switch len(parts) {
		case 4:
			return openbaoRef{kind: "kv", namespace: "root", engine: parts[1], name: parts[2], key: parts[3]}, true
		case 5:
			return openbaoRef{kind: "kv", namespace: parts[1], engine: parts[2], name: parts[3], key: parts[4]}, true
		}
	case "pki", "pki-role":
		return openbaoRef{kind: parts[0]}, true
	}

	return openbaoRef{}, false
}

// splitKVEntry splits an allowlist entry kv:[ns:]engine:prefix.
func splitKVEntry(entry string) (openbaoRef, bool) {
	parts := strings.Split(entry, ":")
	if parts[0] != "kv" {
		return openbaoRef{}, false
	}

	switch len(parts) {
	case 3:
		return openbaoRef{kind: "kv", namespace: "root", engine: parts[1], name: parts[2]}, true
	case 4:
		return openbaoRef{kind: "kv", namespace: parts[1], engine: parts[2], name: parts[3]}, true
	}

	return openbaoRef{}, false
}

// validSegment reports whether s matches the charset and is not . or ..
func validSegment(s string) bool {
	return s != "." && s != ".." && secretSegment.MatchString(s)
}

// validPath reports whether every "/" segment of p is a valid segment; trailing reports whether a
// single trailing "/" is allowed (prefix entries).
func validPath(p string, trailing bool) bool {
	if trailing {
		p = strings.TrimSuffix(p, "/")
	}

	return !slices.ContainsFunc(strings.Split(p, "/"), func(seg string) bool { return !validSegment(seg) })
}

// validateSecretEntry checks an allowlist entry at parse time. A kv entry is split and its
// segments checked; pki and pki-role entries are literal references with upstream's field count
// and no empty field; other strings are literal prefixes for providers other than OpenBao and
// only need to be non-empty.
func validateSecretEntry(e string) error {
	if e == "" {
		return fmt.Errorf("%w: empty entry", errSecretEntry)
	}

	parts := strings.Split(e, ":")

	switch parts[0] {
	case "kv":
		r, ok := splitKVEntry(e)
		if !ok || !validSegment(r.namespace) || !validSegment(r.engine) || !validPath(r.name, true) {
			return fmt.Errorf("%w: want kv:[<namespace>:]<engine>:<prefix> with segments in [A-Za-z0-9._-]", errSecretEntry)
		}
	case "pki", "pki-role":
		minParts := 3
		if parts[0] == "pki-role" {
			minParts = 4
		}

		if len(parts) < minParts || len(parts) > minParts+1 || slices.Contains(parts, "") {
			return fmt.Errorf("%w: want a full %s reference", errSecretEntry, parts[0])
		}
	}

	return nil
}

// kvRefAllowed reports whether a kv reference is well formed and matches one kv entry: same
// namespace and engine, and a name equal to the prefix or below it by path segment.
func kvRefAllowed(r openbaoRef, entries []string) bool {
	if !validSegment(r.namespace) || !validSegment(r.engine) || !validPath(r.name, false) || !validSegment(r.key) {
		return false
	}

	for _, e := range entries {
		er, ok := splitKVEntry(e)
		if !ok || er.namespace != r.namespace || er.engine != r.engine {
			continue
		}

		prefix := strings.TrimSuffix(er.name, "/")
		if r.name == prefix || strings.HasPrefix(r.name, prefix+"/") {
			return true
		}
	}

	return false
}

func refAllowed(provider, ref string, entries []string) bool {
	if provider != openbaoProvider {
		for _, e := range entries {
			if strings.HasPrefix(ref, e) {
				return true
			}
		}

		return false
	}

	r, ok := splitOpenbaoRef(ref)
	if !ok {
		return false
	}

	if r.kind == "kv" {
		return kvRefAllowed(r, entries)
	}

	return slices.Contains(entries, ref) // pki, pki-role: literal and exact only
}

// CheckSecrets applies the compose policy to the encoded references of a target (the map that
// ResolveSecretReferences receives), before any secret is read. The identity comes first and holds
// whether secret_allowlist is set or not: a target missing from targets, or a deploy config whose
// repository, reference or config target is not the target's, gets a target violation and nothing
// is resolved (checkForkPolicies would deny it too, but only after the secrets were read). Then,
// without secret_allowlist in default and in the target, every reference is allowed, as upstream.
// Violations are sorted by variable name.
func (f *File) CheckSecrets(id Identity, provider string, refs map[string]string) []Violation {
	if len(refs) == 0 {
		return nil
	}

	tp, ok := f.targetFor(id.Object)
	if !ok || !identityMatches(tp, id) {
		return []Violation{{Field: "target", Value: id.Object}}
	}

	if tp.SecretAllowlist == nil {
		return nil
	}

	entries := *tp.SecretAllowlist

	names := make([]string, 0, len(refs))
	for name := range refs {
		names = append(names, name)
	}

	slices.Sort(names)

	var out []Violation

	for _, name := range names {
		if !refAllowed(provider, refs[name], entries) {
			out = append(out, Violation{Field: "external_secrets." + name, Value: refs[name]})
		}
	}

	return out
}
