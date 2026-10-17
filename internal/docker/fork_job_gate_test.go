package docker

import (
	"errors"
	"testing"

	"github.com/docker/cli/cli/command"
)

// contextCli is a command.Cli that only knows its current Docker context.
type contextCli struct {
	command.Cli

	name string
}

func (c contextCli) CurrentContext() string { return c.name }

// TestCheckForkJobGate sets the process-wide ForkJobGate, so it does not call t.Parallel:
// sequential top-level tests never overlap with the parallel ones.
func TestCheckForkJobGate(t *testing.T) {
	t.Cleanup(func() { ForkJobGate = nil })

	ForkJobGate = nil

	if err := checkForkJobGate(nil, "web"); err != nil {
		t.Fatalf("checkForkJobGate() error = %v, want nil without a gate", err)
	}

	errDenied := errors.New("denied")

	var gotContext, gotProject string

	ForkJobGate = func(contextName, project string) error {
		gotContext, gotProject = contextName, project

		return errDenied
	}

	if err := checkForkJobGate(nil, "web"); !errors.Is(err, errDenied) {
		t.Fatalf("checkForkJobGate() error = %v, want the gate error", err)
	}

	if gotContext != "" || gotProject != "web" {
		t.Fatalf("gate called with %q/%q, want \"\"/web", gotContext, gotProject)
	}
}

func TestForkJobObject(t *testing.T) {
	t.Parallel()

	if got := forkJobObject(nil, "web"); got != "default/web" {
		t.Fatalf("forkJobObject(nil) = %q, want default/web", got)
	}

	if got := forkJobObject(contextCli{name: "remote"}, "web"); got != "remote/web" {
		t.Fatalf("forkJobObject(remote) = %q, want remote/web", got)
	}
}

// TestCheckForkJobGateContext: the job gate receives the current context of the Docker client.
func TestCheckForkJobGateContext(t *testing.T) {
	t.Cleanup(func() { ForkJobGate = nil })

	var gotContext string

	ForkJobGate = func(contextName, _ string) error {
		gotContext = contextName
		return nil
	}

	if err := checkForkJobGate(contextCli{name: "remote"}, "web"); err != nil {
		t.Fatalf("checkForkJobGate() error = %v", err)
	}

	if gotContext != "remote" {
		t.Fatalf("gate called with context %q, want remote", gotContext)
	}
}
