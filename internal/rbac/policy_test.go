package rbac

import (
	"strings"
	"testing"
)

const argoLike = `
# comment
p, role:deployer, projects, get, noi/*, allow
p, role:deployer, targets, sync, noi/poli-gate, allow
p, role:deployer, projects, action/*, noi/*, allow
p, role:deployer, projects, delete, noi/*, deny
p, role:admin, *, *, */*, allow
p, alice, runs, get, *, allow
g, team-poli-gate, role:deployer
g, role:deployer, role:reader
p, role:reader, syncwindows, get, *, allow
g, bob, role:admin
`

type allowedCase struct {
	pr                       Principal
	resource, action, object string
	want                     bool
}

func runAllowed(t *testing.T, p *Policy, cases []allowedCase) {
	t.Helper()

	for _, c := range cases {
		if got := p.Allowed(c.pr, c.resource, c.action, c.object); got != c.want {
			t.Errorf("%+v %s %s %s = %v, want %v", c.pr, c.resource, c.action, c.object, got, c.want)
		}
	}
}

func TestAllowed(t *testing.T) {
	p, err := Parse(strings.NewReader(argoLike))
	if err != nil {
		t.Fatal(err)
	}

	team := Principal{Subject: "sandbox:x", Groups: []string{"team-poli-gate"}}
	bob := Principal{Subject: "bob"}

	runAllowed(t, p, []allowedCase{
		{team, "projects", "get", "noi/web", true},
		{team, "projects", "get", "other/web", false},
		{team, "projects", "get", "noi/a/b", false},           // * does not cross /
		{team, "projects", "action/restart", "noi/web", true}, // glob on the action
		{team, "projects", "delete", "noi/web", false},        // deny wins
		{team, "targets", "sync", "noi/poli-gate", true},
		{team, "targets", "sync", "noi/worklog", false},
		{team, "syncwindows", "get", "*", true}, // nested g
		{Principal{Subject: "alice"}, "runs", "get", "*", true},
		{bob, "projects", "delete", "x/y", true},
		{Principal{Subject: "nobody"}, "projects", "get", "noi/web", false}, // default deny
		{Principal{}, "projects", "get", "noi/web", false},
		// '*' does not cross '/': the admin line misses action/restart and global objects.
		{bob, "projects", "action/restart", "x/y", false},
		{bob, "runs", "get", "*", false},
	})

	extended := argoLike + "p, role:admin, *, action/*, */*, allow\np, role:admin, *, *, *, allow\n"

	p, err = Parse(strings.NewReader(extended))
	if err != nil {
		t.Fatal(err)
	}

	runAllowed(t, p, []allowedCase{
		{bob, "projects", "action/restart", "x/y", true},
		{bob, "runs", "get", "*", true},
	})
}

func TestGroupCycle(t *testing.T) {
	p, err := Parse(strings.NewReader("g, a, role:b\ng, role:b, role:c\ng, role:c, role:b\np, role:c, x, get, *, allow\n"))
	if err != nil {
		t.Fatal(err)
	}

	if !p.Allowed(Principal{Subject: "a"}, "x", "get", "o") {
		t.Fatal("cycle must still resolve")
	}
}

func TestParseErrors(t *testing.T) {
	for _, doc := range []string{
		"p, a, b, c",           // too few fields
		"p, a, b, c, d",        // missing effect: a truncated deny must not become an allow
		"p, a, b, c, d, maybe", // invalid effect
		"p, a, b, c, d, allow, extra",
		"g, a",                 // incomplete g
		"g, a, b",              // g target is not a role
		"x, a, b",              // unknown line type
		"p, a, b, c, [, allow", // invalid glob
		"p, , b, c, d, allow",  // empty field
	} {
		if _, err := Parse(strings.NewReader(doc)); err == nil {
			t.Errorf("%q: expected error", doc)
		}
	}
}

func TestParseReadError(t *testing.T) {
	// A single line longer than bufio.MaxScanTokenSize makes the scanner fail.
	doc := "p, a, b, c, " + strings.Repeat("d", 70*1024) + ", allow"
	if _, err := Parse(strings.NewReader(doc)); err == nil {
		t.Fatal("expected read error")
	}
}

func TestAllowedEmptyValues(t *testing.T) {
	p, err := Parse(strings.NewReader("p, alice, projects, get, noi/*, allow\np, alice, *, *, *, allow\n"))
	if err != nil {
		t.Fatal(err)
	}

	alice := Principal{Subject: "alice"}

	runAllowed(t, p, []allowedCase{
		{alice, "projects", "get", "noi/web", true},
		{alice, "projects", "get", "noi/", false},
		{alice, "projects", "get", "/web", false},
		{alice, "projects", "get", "", false},
		{alice, "projects", "", "noi/web", false},
		{alice, "", "get", "noi/web", false},
	})
}

func TestReservedSubjects(t *testing.T) {
	p, err := Parse(strings.NewReader(
		"p, role:x, r, get, *, allow\np, apikey, r, get, *, allow\ng, alice, role:x\n"))
	if err != nil {
		t.Fatal(err)
	}

	runAllowed(t, p, []allowedCase{
		{Principal{Subject: "alice"}, "r", "get", "o", true},
		{Principal{Subject: APIKeySubject}, "r", "get", "o", true},
		{Principal{Subject: "role:x"}, "r", "get", "o", false},
		{Principal{Subject: "bob", Groups: []string{"role:x"}}, "r", "get", "o", false},
		{Principal{Subject: "bob", Groups: []string{APIKeySubject}}, "r", "get", "o", false},
		{Principal{Subject: "bob", Groups: []string{"alice"}}, "r", "get", "o", true},
	})
}
