package stages

import (
	"testing"

	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/docker"
)

// The fork's object must use the same context name that upstream shows.
func TestObjectNameMatchesDockerDisplayName(t *testing.T) {
	if got, want := deploy.ObjectName("", "x"), docker.DisplayContextName("")+"/x"; got != want {
		t.Fatalf("%q != %q", got, want)
	}
}
