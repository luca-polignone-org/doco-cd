package docker

import (
	"errors"
	"testing"
)

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
