package rollback

import (
	"fmt"

	"github.com/compose-spec/compose-go/v2/types"
)

// LocalImage resolves an image reference or ID to the ID of the local image it names; ok is false
// when there is no such local image.
type LocalImage func(ref string) (id string, ok bool)

// PinImages pins every service to the image that ran in the last good. In order:
//
//   - the reference of the last good's own compose file, when it still names the local image
//     that ran: the service is left as the repository wrote it, so the containers of the
//     rollback have the same config hash and image as a regular deploy of that content, and a
//     later commit with the same content does not recreate them;
//   - otherwise the local ImageID (the tag was moved, e.g. by the failed deploy's pull);
//   - otherwise the registry digest.
//
// A local image is never pulled (pull_policy: never). A pinned service never builds: its build
// section is dropped, because a rebuild would not give back the image that ran. A service of the
// project without a recorded image (other than one scaled to zero), or whose image is gone and
// has no registry digest, is an error: the rollback cannot reproduce it.
func PinImages(project *types.Project, images []ServiceImage, local LocalImage) error {
	byService := make(map[string]ServiceImage, len(images))
	for _, im := range images {
		byService[im.Service] = im
	}

	for name, svc := range project.Services {
		im, ok := byService[name]
		if !ok {
			// A service scaled to zero never had a container to record.
			if svc.GetScale() == 0 {
				continue
			}

			return fmt.Errorf("service %s has no recorded image in the last good deployment", name)
		}

		switch {
		case im.ImageID != "" && svc.Image != "" && resolvesTo(local, svc.Image, im.ImageID):
			// Present locally, and a pull could move a tag to another image.
			svc.PullPolicy = types.PullPolicyNever
		case im.ImageID != "" && resolvesTo(local, im.ImageID, im.ImageID):
			svc.Image = im.ImageID
			// A sha256: image ID cannot be pulled.
			svc.PullPolicy = types.PullPolicyNever
		case len(im.RepoDigests) > 0:
			svc.Image = im.RepoDigests[0]
			// Without a build section "build" has nothing to build from.
			if svc.PullPolicy == types.PullPolicyBuild {
				svc.PullPolicy = types.PullPolicyMissing
			}
		default:
			return fmt.Errorf("service %s: image %s is gone and has no registry digest", name, im.Image)
		}

		svc.Build = nil
		project.Services[name] = svc
	}

	return nil
}

func resolvesTo(local LocalImage, ref, id string) bool {
	got, ok := local(ref)
	return ok && got == id
}
