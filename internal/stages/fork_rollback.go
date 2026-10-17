package stages

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/docker/compose/v5/pkg/api"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	"go.yaml.in/yaml/v4"

	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/docker"
	gitInternal "github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/rollback"
)

// forkStore returns the failure store only if the target has rollback enabled:
// otherwise the fork writes nothing under <data>/fork/ (spec, "Configuration").
func (s *StageManager) forkStore() *rollback.Store {
	if !s.DeployConfig.RollbackEnabled() || s.AppConfig == nil {
		return nil
	}

	return rollback.StoreFor(s.AppConfig.DataMountPath)
}

func (s *StageManager) persistForkFailure(f docker.DeploymentFailure, state rollback.State, decision string) {
	store := s.forkStore()
	if store == nil {
		return
	}

	err := store.PutFailure(rollback.Failure{
		Object: s.DeployConfig.Object(), Repository: f.Repository, Stack: f.Stack,
		CommitSHA: f.CommitSHA, Stage: f.Stage, Error: truncateRunes(f.Error, 2000),
		FailedAt: f.FailedAt, State: state, Decision: decision,
	})
	if err != nil {
		s.Log.Error("failed to persist deployment failure", slog.String("object", s.DeployConfig.Object()), slog.Any("error", err))
	}
}

// loadForkFailure reads the failure from disk. An unreadable file counts as a
// blocked failure: never a blind deploy on top of a state that cannot be read.
func (s *StageManager) loadForkFailure() (rollback.Failure, bool) {
	store := s.forkStore()
	if store == nil {
		return rollback.Failure{}, false
	}

	f, ok, err := store.GetFailure(s.DeployConfig.Object())
	if err != nil {
		s.Log.Error("failed to read deployment failure, treating target as blocked",
			slog.String("object", s.DeployConfig.Object()), slog.Any("error", err))

		return rollback.Failure{Object: s.DeployConfig.Object(), State: rollback.StateBlocked, CommitSHA: s.Repository.Revision}, true
	}

	return f, ok
}

func (s *StageManager) deleteForkFailure() {
	if store := s.forkStore(); store != nil {
		if err := store.DeleteFailure(s.DeployConfig.Object()); err != nil {
			s.Log.Error("failed to delete deployment failure", slog.Any("error", err))
		}
	}
}

func truncateRunes(v string, n int) string {
	if r := []rune(v); len(r) > n {
		return string(r[:n])
	}

	return v
}

// recordLastGood makes a successful deploy the target's last good. A destroy never does, and
// without rollback enabled nothing is written. A failure is logged and never fails the deploy.
func (s *StageManager) recordLastGood(ctx context.Context) {
	if s.DeployConfig.Destroy.Enabled {
		return
	}

	store := s.forkStore()
	if store == nil {
		return
	}

	object := s.DeployConfig.Object()
	log := s.Log.With(slog.String("object", object))

	commit, err := s.deployedCommit()
	if err != nil {
		log.Error("failed to resolve deployed commit for last good", slog.Any("error", err))
		return
	}

	images, err := s.serviceImages(ctx)
	if err != nil {
		log.Error("failed to collect images for last good", slog.Any("error", err))
		return
	}

	cfg, err := deployConfigForLastGood(s.DeployConfig, s.forkDeclared)
	if err != nil {
		log.Error("failed to encode deploy config for last good", slog.Any("error", err))
		return
	}

	// The ref goes before the state: a last good pointing at an unprotected commit could
	// lose it on the next force push.
	if s.Repository.Source != config.SourceTypeOCI && s.Repository.MirrorDir != "" {
		if err = gitInternal.PinLastGood(s.Repository.MirrorDir, object, commit); err != nil {
			log.Error("failed to pin last good commit", slog.Any("error", err))
			return
		}
	}

	err = store.PutLastGood(rollback.LastGood{
		Object: object, CommitSHA: commit, SourceURL: withoutUserinfo(s.Repository.SourceUrl),
		ProjectHash: s.Docker.ProjectHash, DeployConfig: cfg, Images: images,
		RecordedAt: time.Now().UTC(),
	})
	if err != nil {
		log.Error("failed to record last good deployment", slog.Any("error", err))
	}
}

// deployConfigForLastGood serializes the deploy config with the internal struct zeroed. It is
// mandatory, not a precaution: Internal.Environment holds the resolved secrets and has no
// yaml:"-". Credentials embedded in repository_url are dropped as well.
func deployConfigForLastGood(c *deploy.Config, declared declaredFiles) ([]byte, error) {
	copyCfg := *c
	copyCfg.Internal = deploy.Config{}.Internal
	copyCfg.RepositoryUrl = config.GitUrl(withoutUserinfo(string(c.RepositoryUrl)))
	// The init stage consumed the file lists (only the remote: entries are left): the rollback
	// must read them all again from the artifact, the default .env included.
	copyCfg.EnvFiles = slices.Clone(declared.EnvFiles)
	copyCfg.ExternalSecretsFiles = slices.Clone(declared.ExternalSecretsFiles)

	data, err := yaml.Marshal(&copyCfg)
	if err != nil {
		return nil, fmt.Errorf("encode deploy config: %w", err)
	}

	return data, nil
}

// withoutUserinfo drops the user and password of a URL, where an access token can be embedded
// (https://user:token@host/...). A value that does not parse as a URL is kept only if it has no
// scheme, like the scp-like git@host:org/repo form, whose user is never a secret.
func withoutUserinfo(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		if strings.Contains(raw, "://") {
			return ""
		}

		return raw
	}

	if u.User == nil {
		return raw
	}

	u.User = nil

	return u.String()
}

// serviceImages reads the effective image of every service: ImageID from the container,
// RepoDigests from the image. One container per service, the first by name; one-off and
// ephemeral job containers are skipped, as Compose does not reconcile them.
func (s *StageManager) serviceImages(ctx context.Context) ([]rollback.ServiceImage, error) {
	apiClient := s.Docker.Cmd.Client()

	list, err := apiClient.ContainerList(ctx, client.ContainerListOptions{
		All: true,
		Filters: make(client.Filters).
			Add("label", api.ProjectLabel+"="+s.DeployConfig.Name).
			Add("label", api.OneoffLabel+"=False"),
	})
	if err != nil {
		return nil, fmt.Errorf("list project containers: %w", err)
	}

	containers := slices.Clone(list.Items)
	slices.SortStableFunc(containers, func(a, b container.Summary) int {
		return cmp.Compare(firstName(a.Names), firstName(b.Names))
	})

	seen := map[string]bool{}
	out := []rollback.ServiceImage{}

	for _, c := range containers {
		svc := c.Labels[api.ServiceLabel]
		if svc == "" || seen[svc] || strings.EqualFold(strings.TrimSpace(c.Labels[docker.DocoCDJobLabels.JobEphemeral]), "true") {
			continue
		}

		seen[svc] = true

		inspect, err := apiClient.ImageInspect(ctx, c.ImageID)
		if err != nil {
			return nil, fmt.Errorf("inspect image of %s: %w", svc, err)
		}

		out = append(out, rollback.ServiceImage{Service: svc, Image: c.Image, ImageID: c.ImageID, RepoDigests: inspect.RepoDigests})
	}

	slices.SortFunc(out, func(a, b rollback.ServiceImage) int { return cmp.Compare(a.Service, b.Service) })

	return out, nil
}

func firstName(names []string) string {
	if len(names) == 0 {
		return ""
	}

	return names[0]
}
