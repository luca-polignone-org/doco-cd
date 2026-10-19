package rollback

import (
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
)

func pinProject(services ...types.ServiceConfig) *types.Project {
	p := &types.Project{Name: "web", Services: types.Services{}}
	for _, svc := range services {
		p.Services[svc.Name] = svc
	}

	return p
}

// localImages is a local image store holding the image IDs ids, and no reference.
func localImages(ids ...string) LocalImage {
	return func(ref string) (string, bool) {
		for _, known := range ids {
			if ref == known {
				return known, true
			}
		}

		return "", false
	}
}

func TestPinImagesLocalImageID(t *testing.T) {
	p := pinProject(types.ServiceConfig{
		Name: "app", Image: "app:latest", PullPolicy: types.PullPolicyAlways,
		Build: &types.BuildConfig{Context: "."},
	})

	err := PinImages(p, []ServiceImage{{Service: "app", Image: "app:latest", ImageID: "sha256:1", RepoDigests: []string{"app@sha256:d"}}},
		localImages("sha256:1"))
	if err != nil {
		t.Fatal(err)
	}

	got := p.Services["app"]
	if got.Image != "sha256:1" || got.PullPolicy != types.PullPolicyNever || got.Build != nil {
		t.Fatalf("service %+v", got)
	}
}

func TestPinImagesRegistryDigest(t *testing.T) {
	p := pinProject(
		types.ServiceConfig{Name: "app", Image: "app:1", PullPolicy: types.PullPolicyAlways},
		types.ServiceConfig{Name: "built", Image: "built:1", PullPolicy: types.PullPolicyBuild, Build: &types.BuildConfig{Context: "."}},
	)

	err := PinImages(p, []ServiceImage{
		{Service: "app", Image: "app:1", ImageID: "sha256:gone", RepoDigests: []string{"reg/app@sha256:a", "mirror/app@sha256:a"}},
		{Service: "built", Image: "built:1", RepoDigests: []string{"reg/built@sha256:b"}},
	}, localImages())
	if err != nil {
		t.Fatal(err)
	}

	if got := p.Services["app"]; got.Image != "reg/app@sha256:a" || got.PullPolicy != types.PullPolicyAlways {
		t.Fatalf("app %+v", got)
	}

	if got := p.Services["built"]; got.Image != "reg/built@sha256:b" || got.PullPolicy != types.PullPolicyMissing || got.Build != nil {
		t.Fatalf("built %+v", got)
	}
}

func TestPinImagesGoneWithoutDigest(t *testing.T) {
	p := pinProject(types.ServiceConfig{Name: "app", Image: "app:1"})

	err := PinImages(p, []ServiceImage{{Service: "app", Image: "app:1", ImageID: "sha256:gone"}}, localImages())
	if err == nil || !strings.Contains(err.Error(), "service app") || !strings.Contains(err.Error(), "app:1") {
		t.Fatalf("err = %v", err)
	}
}

func TestPinImagesServiceWithoutRecord(t *testing.T) {
	p := pinProject(types.ServiceConfig{Name: "new", Image: "new:1"})

	err := PinImages(p, []ServiceImage{{Service: "app", ImageID: "sha256:1"}}, localImages("sha256:1"))
	if err == nil || !strings.Contains(err.Error(), "service new has no recorded image") {
		t.Fatalf("err = %v", err)
	}
}

// A service scaled to zero has no container, hence no recorded image: it is left as rendered.
func TestPinImagesSkipsScaledToZero(t *testing.T) {
	zero := 0
	p := pinProject(types.ServiceConfig{Name: "idle", Image: "idle:1", Scale: &zero})

	if err := PinImages(p, nil, localImages()); err != nil {
		t.Fatal(err)
	}

	if got := p.Services["idle"]; got.Image != "idle:1" {
		t.Fatalf("idle %+v", got)
	}
}
