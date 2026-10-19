package rollback

import (
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
)

// localStore is a local image store: refs maps a reference to the ID it resolves to, and every ID
// of refs resolves to itself.
func localStore(refs map[string]string) LocalImage {
	return func(ref string) (string, bool) {
		if id, ok := refs[ref]; ok {
			return id, true
		}

		for _, id := range refs {
			if ref == id {
				return id, true
			}
		}

		return "", false
	}
}

// The reference of the last good compose file still names the image that ran: the service keeps
// it, so the containers of the rollback match a regular deploy of the same content (TCK-644).
func TestPinImagesKeepsReferenceOfLastGood(t *testing.T) {
	const ref = "127.0.0.1:5000/e2e/worklog:v1.0.0@sha256:96b20d5f"

	p := pinProject(
		types.ServiceConfig{Name: "app", Image: ref, PullPolicy: types.PullPolicyAlways},
		types.ServiceConfig{Name: "built", Image: "built:1", Build: &types.BuildConfig{Context: "."}},
	)

	err := PinImages(p, []ServiceImage{
		{Service: "app", Image: ref, ImageID: "sha256:app"},
		{Service: "built", Image: "built:1", ImageID: "sha256:built"},
	}, localStore(map[string]string{ref: "sha256:app", "built:1": "sha256:built"}))
	if err != nil {
		t.Fatal(err)
	}

	if got := p.Services["app"]; got.Image != ref || got.PullPolicy != types.PullPolicyNever {
		t.Fatalf("app %+v, want the reference of the last good with pull_policy never", got)
	}

	if got := p.Services["built"]; got.Image != "built:1" || got.PullPolicy != types.PullPolicyNever || got.Build != nil {
		t.Fatalf("built %+v, want its reference, pull_policy never and no build", got)
	}
}

// The record of a last good written by a rollback of poli.2 holds the image ID as Image: the
// reference of the compose file is still the one that wins.
func TestPinImagesReferenceOverRecordedImageID(t *testing.T) {
	p := pinProject(types.ServiceConfig{Name: "app", Image: "app:1"})

	err := PinImages(p, []ServiceImage{{Service: "app", Image: "sha256:1", ImageID: "sha256:1"}},
		localStore(map[string]string{"app:1": "sha256:1"}))
	if err != nil {
		t.Fatal(err)
	}

	if got := p.Services["app"]; got.Image != "app:1" || got.PullPolicy != types.PullPolicyNever {
		t.Fatalf("app %+v", got)
	}
}

// A tag moved to another image (e.g. pulled by the failed deploy) is not the image that ran: the
// service is pinned to the image ID.
func TestPinImagesMovedTagFallsBackToImageID(t *testing.T) {
	p := pinProject(types.ServiceConfig{Name: "app", Image: "app:latest"})

	err := PinImages(p, []ServiceImage{{Service: "app", Image: "app:latest", ImageID: "sha256:old"}},
		localStore(map[string]string{"app:latest": "sha256:new", "old": "sha256:old"}))
	if err != nil {
		t.Fatal(err)
	}

	if got := p.Services["app"]; got.Image != "sha256:old" || got.PullPolicy != types.PullPolicyNever {
		t.Fatalf("app %+v, want the image ID that ran", got)
	}
}

// RetryRecreates: a failure a rollback repaired is retried without a forced recreate.
func TestFailureRetryRecreates(t *testing.T) {
	if !(Failure{}).RetryRecreates() {
		t.Fatal("a failure without a rollback must force recreate on retry")
	}

	if (Failure{RolledBackTo: "c1"}).RetryRecreates() {
		t.Fatal("a failure rolled back to c1 must not force recreate on retry")
	}
}
