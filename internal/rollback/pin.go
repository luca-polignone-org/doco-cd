package rollback

import (
	"fmt"

	"github.com/compose-spec/compose-go/v2/types"
)

// PinImages pins every service to the image that ran in the last good: the local ImageID if it
// still exists (no pull), otherwise the registry digest. A pinned service never builds: its
// build section is dropped, because a rebuild would not give back the image that ran. A service
// of the project without a recorded image (other than one scaled to zero), or whose image is
// gone and has no registry digest, is an error: the rollback cannot reproduce it.
func PinImages(project *types.Project, images []ServiceImage, localImageExists func(id string) bool) error {
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
		case im.ImageID != "" && localImageExists(im.ImageID):
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
