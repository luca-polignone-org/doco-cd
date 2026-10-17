// Package rbac implements an Argo CD-style RBAC policy: a CSV of p (permission)
// and g (role membership) lines, a reloading file-backed enforcer with an audit
// log, and helpers to carry the authenticated principal in a context.
package rbac

import (
	"bufio"
	"fmt"
	"io"
	"path"
	"strings"
)

// Policy is the Argo CD CSV format: p lines (permissions) and g lines (role
// membership). Default is deny, and an explicit deny wins over any allow. Globs
// follow path.Match, so '*' does not cross '/'.

// Principal is the authenticated caller: a subject plus the groups it belongs to.
type Principal struct {
	Subject string
	Groups  []string
}

// APIKeySubject is the subject used for requests authenticated with an API key.
const APIKeySubject = "apikey"

type rule struct {
	subject, resource, action, object string
	allow                             bool
}

// Policy is a parsed RBAC policy.
type Policy struct {
	rules  []rule
	groups map[string][]string // member -> roles
}

// Parse reads a policy in the Argo CD CSV format.
func Parse(r io.Reader) (*Policy, error) {
	p := &Policy{groups: map[string][]string{}}
	sc := bufio.NewScanner(r)
	line := 0

	for sc.Scan() {
		line++

		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}

		fields := strings.Split(text, ",")
		for i := range fields {
			fields[i] = strings.TrimSpace(fields[i])
			if fields[i] == "" {
				return nil, fmt.Errorf("rbac policy line %d: empty field", line)
			}
		}

		switch fields[0] {
		case "p":
			ru, err := parseRule(fields)
			if err != nil {
				return nil, fmt.Errorf("rbac policy line %d: %w", line, err)
			}

			p.rules = append(p.rules, ru)
		case "g":
			if len(fields) != 3 {
				return nil, fmt.Errorf("rbac policy line %d: g needs 3 fields", line)
			}

			p.groups[fields[1]] = append(p.groups[fields[1]], fields[2])
		default:
			return nil, fmt.Errorf("rbac policy line %d: unknown line type %q", line, fields[0])
		}
	}

	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read rbac policy: %w", err)
	}

	return p, nil
}

func parseRule(f []string) (rule, error) {
	// The effect is mandatory: with a default allow, a deny line truncated before
	// the effect would silently become a permission.
	if len(f) != 6 {
		return rule{}, fmt.Errorf("p needs 6 fields, got %d", len(f))
	}

	ru := rule{subject: f[1], resource: f[2], action: f[3], object: f[4]}

	switch f[5] {
	case "allow":
		ru.allow = true
	case "deny":
	default:
		return rule{}, fmt.Errorf("invalid effect %q", f[5])
	}

	for _, g := range []string{ru.resource, ru.action, ru.object} {
		if _, err := path.Match(g, ""); err != nil {
			return rule{}, fmt.Errorf("invalid glob %q: %w", g, err)
		}
	}

	return ru, nil
}

// subjects returns the principal's subject set: the subject, its groups and every
// role reachable through g lines, guarded against cycles.
func (p *Policy) subjects(pr Principal) map[string]bool {
	seen := map[string]bool{}
	queue := make([]string, 0, 1+len(pr.Groups))

	if pr.Subject != "" {
		queue = append(queue, pr.Subject)
	}

	queue = append(queue, pr.Groups...)

	for len(queue) > 0 {
		s := queue[0]
		queue = queue[1:]

		if seen[s] {
			continue
		}

		seen[s] = true
		queue = append(queue, p.groups[s]...)
	}

	return seen
}

func match(pattern, value string) bool {
	ok, _ := path.Match(pattern, value) // patterns were validated in Parse
	return ok
}

// Allowed reports whether the principal may perform action on object of resource.
func (p *Policy) Allowed(pr Principal, resource, action, object string) bool {
	subjects := p.subjects(pr)
	allowed := false

	for _, ru := range p.rules {
		if !subjects[ru.subject] || !match(ru.resource, resource) || !match(ru.action, action) || !match(ru.object, object) {
			continue
		}

		if !ru.allow {
			return false
		}

		allowed = true
	}

	return allowed
}
