package stages

import (
	"testing"

	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/docker"
)

// L'oggetto del fork deve usare lo stesso nome di context che upstream mostra.
func TestObjectNameMatchesDockerDisplayName(t *testing.T) {
	if got, want := deploy.ObjectName("", "x"), docker.DisplayContextName("")+"/x"; got != want {
		t.Fatalf("%q != %q", got, want)
	}
}
