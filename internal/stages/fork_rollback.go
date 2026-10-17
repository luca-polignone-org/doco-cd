package stages

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/docker/compose/v5/pkg/api"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	"go.yaml.in/yaml/v4"

	"github.com/kimdre/doco-cd/internal/composepolicy"
	"github.com/kimdre/doco-cd/internal/config"
	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/config/deploy"
	"github.com/kimdre/doco-cd/internal/docker"
	gitInternal "github.com/kimdre/doco-cd/internal/git"
	"github.com/kimdre/doco-cd/internal/hooks"
	"github.com/kimdre/doco-cd/internal/rollback"
	secrettypes "github.com/kimdre/doco-cd/internal/secretprovider/types"
	sourcecache "github.com/kimdre/doco-cd/internal/source/cache"
	"github.com/kimdre/doco-cd/internal/source/store"
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
// Its commit is the current revision, so rollbackPoisoned keeps the target blocked
// on every later commit too: only the unblock action (UnblockTarget) releases it.
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

// recordLastGood makes a successful deploy the target's last good. A destroy never does, nor a
// Swarm deploy (rollback is Compose only), and without rollback enabled nothing is written. A
// failure is logged and never fails the deploy.
func (s *StageManager) recordLastGood(ctx context.Context) {
	if s.DeployConfig.Destroy.Enabled || s.Docker.SwarmMode {
		return
	}

	store := s.forkStore()
	if store == nil {
		return
	}

	object := s.DeployConfig.Object()
	log := s.Log.With(slog.String("object", object))

	// The commit the deploy stage stamped, else the immutable published revision; never the
	// mirror's branch tip, which may have moved during the deploy.
	commit := s.forkDeployedCommit
	if commit == "" {
		commit = strings.TrimSpace(s.Repository.Revision)
	}

	if commit == "" {
		log.Error("failed to resolve deployed commit for last good")
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

	// The state goes before the ref. The JSON is what the rollback reads; the ref only keeps
	// its commit reachable across a force push, and mirror compaction never drops objects, so
	// a failed pin leaves a commit that is still in the mirror. The reverse order would leave
	// the ref on a commit the JSON does not name when the store write fails.
	err = store.PutLastGood(rollback.LastGood{
		Object: object, CommitSHA: commit, SourceURL: withoutUserinfo(s.Repository.SourceUrl),
		ProjectHash: s.Docker.ProjectHash, DeployConfig: cfg, Images: images,
		RecordedAt: time.Now().UTC(),
	})
	if err != nil {
		log.Error("failed to record last good deployment", slog.Any("error", err))
		return
	}

	if s.Repository.Source != config.SourceTypeOCI && s.Repository.MirrorDir != "" {
		if err = gitInternal.PinLastGood(s.Repository.MirrorDir, object, commit); err != nil {
			log.Error("failed to pin last good commit", slog.Any("error", err))
		}
	}
}

// lastGoodDocument encodes the deploy config and its external secret references in one pass,
// so that the references can be spliced into the config in their canonical form.
type lastGoodDocument struct {
	Config          *deploy.Config `yaml:"config"`
	ExternalSecrets map[string]any `yaml:"external_secrets"`
}

// structuredSecretRef is the YAML form secrettypes.ExternalSecretRef reads back for a
// structured (webhook) reference.
type structuredSecretRef struct {
	StoreRef  string         `yaml:"store_ref"`
	RemoteRef map[string]any `yaml:"remote_ref,omitempty"`
}

// deployConfigForLastGood serializes the deploy config with the internal struct dropped. It is
// mandatory, not a precaution: Internal.Environment holds the resolved secrets and has no
// yaml:"-". Credentials embedded in repository_url are dropped as well.
//
// ExternalSecretRef has no MarshalYAML, so the default encoding (legacyref, storeref, ...) would
// not read back; changing the type would change deploy.Config.Hash(). The references are
// written here in the form its UnmarshalYAML reads: a scalar for a legacy reference,
// store_ref/remote_ref for a structured one.
func deployConfigForLastGood(c *deploy.Config, declared declaredFiles) ([]byte, error) {
	copyCfg := *c
	copyCfg.Internal = deploy.Config{}.Internal
	copyCfg.RepositoryUrl = config.GitUrl(withoutUserinfo(string(c.RepositoryUrl)))
	// The init stage consumed the file lists (only the remote: entries are left): the rollback
	// must read them all again from the artifact, the default .env included.
	copyCfg.EnvFiles = slices.Clone(declared.EnvFiles)
	copyCfg.ExternalSecretsFiles = slices.Clone(declared.ExternalSecretsFiles)

	refs := make(map[string]any, len(c.ExternalSecrets))
	for name, ref := range c.ExternalSecrets {
		if ref.LegacyRef != "" {
			refs[name] = ref.LegacyRef
			continue
		}

		refs[name] = structuredSecretRef{StoreRef: ref.StoreRef, RemoteRef: ref.RemoteRef}
	}

	var doc yaml.Node
	if err := doc.Encode(lastGoodDocument{Config: &copyCfg, ExternalSecrets: refs}); err != nil {
		return nil, fmt.Errorf("encode deploy config: %w", err)
	}

	cfgNode, refsNode := doc.Content[1], doc.Content[3]
	content := make([]*yaml.Node, 0, len(cfgNode.Content))

	for i := 0; i+1 < len(cfgNode.Content); i += 2 {
		key, value := cfgNode.Content[i], cfgNode.Content[i+1]

		switch key.Value {
		case "internal":
			continue
		case "external_secrets":
			value = refsNode
		}

		content = append(content, key, value)
	}

	cfgNode.Content = content

	return yaml.Marshal(cfgNode)
}

// redactedURL replaces a source URL whose credentials cannot be told apart from the rest.
// It is never empty, so it cannot read as "same repository" as an unset URL.
const redactedURL = "<redacted>"

// withoutUserinfo drops the user and password of a source URL, where an access token can be
// embedded. With a scheme (https://user:token@host/...) the URL is parsed and its userinfo
// dropped; one that does not parse is redacted. Without "://" (scp-like user@host:org/repo,
// or an OCI reference) everything up to the last "@" before the first "/" is dropped, so an
// OCI digest (registry/app@sha256:...) is kept.
func withoutUserinfo(raw string) string {
	if !strings.Contains(raw, "://") {
		head, _, _ := strings.Cut(raw, "/")
		if i := strings.LastIndex(head, "@"); i >= 0 {
			return raw[i+1:]
		}

		return raw
	}

	u, err := url.Parse(raw)
	if err != nil {
		return redactedURL
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

// preRollbackClient is the pre_rollback hook of the process, registered at startup when
// HOOK_PRE_ROLLBACK_URL is set. Unset, Load returns nil, which decides unconfigured.
var preRollbackClient atomic.Pointer[hooks.PreRollbackClient]

// SetPreRollback registers the pre_rollback client of the process; nil leaves it unconfigured.
func SetPreRollback(c *hooks.PreRollbackClient) { preRollbackClient.Store(c) }

// deployLastGood is docker.DeployStack; tests replace it to run the executor without a daemon.
var deployLastGood = docker.DeployStack

// runForkRollback runs the rollback flow after a failed deploy, post-deploy or cleanup stage of
// a target with rollback enabled; it never runs for a destroy. The failure comes from process
// memory, not from disk: if the initial write failed, an empty CommitSHA read back from disk
// would make the failed commit look new to rollbackPoisoned, and it would be deployed again.
//
// It runs inside RunStages, before the mutation admission is released, so it does not race
// another deploy of the same project.
func (s *StageManager) runForkRollback(ctx context.Context, stageName StageName) {
	if s.DeployConfig.Destroy.Enabled || !stageRecordsDeploymentFailure(stageName) {
		return
	}

	st := s.forkStore()
	if st == nil {
		return
	}

	mem, ok := docker.GetDeploymentFailure(s.Repository.Name, s.DeployConfig.Name)
	if !ok {
		return
	}

	f := rollback.Failure{
		Object: s.DeployConfig.Object(), Repository: mem.Repository, Stack: mem.Stack,
		CommitSHA: mem.CommitSHA, Stage: mem.Stage, Error: truncateRunes(mem.Error, 2000),
		FailedAt: mem.FailedAt, State: rollback.StateFailed,
	}

	log := s.Log.With(slog.String("object", f.Object), slog.String("failed_commit", f.CommitSHA))

	out := rollback.Run(ctx, st, preRollbackClient.Load(), stageExecutor{s: s, log: log}, f,
		s.DeployConfig.Rollback.EffectivePreRollbackTimeout(), s.DeployConfig.Rollback.EffectiveTimeout())
	if out.PersistErr != nil {
		log.Error("failed to persist rollback state", slog.Any("error", out.PersistErr))
	}

	attrs := []any{slog.String("state", string(out.State)), slog.String("decision", string(out.Decision))}
	if out.ToCommit != "" {
		attrs = append(attrs, slog.String("to_commit", out.ToCommit))
	}

	if out.Err != nil {
		attrs = append(attrs, slog.Any("error", out.Err))
	}

	log.Warn("deployment rollback finished", attrs...)
}

// rollbackPoisoned reports whether the current revision of the target is a commit that ended
// rolled_back, blocked or rollback_failed: pre-deploy then skips it, on every source and with or
// without containers. On a new commit the failure goes back to failed with its own commit, so
// the upstream retry stays the usual one and the success of the new commit clears it.
func (s *StageManager) rollbackPoisoned(revision string, stageLog *slog.Logger) bool {
	f, ok := s.loadForkFailure()
	if !ok || f.State == rollback.StateFailed {
		return false
	}

	if strings.TrimSpace(f.CommitSHA) != revision {
		f.State, f.Decision = rollback.StateFailed, ""
		if err := s.forkStore().PutFailure(f); err != nil {
			stageLog.Error("failed to reset rollback state", slog.Any("error", err))
		}

		return false
	}

	stageLog.Warn("deployment of this commit was rolled back or blocked, skipping",
		slog.String("commit", revision), slog.String("state", string(f.State)))

	return true
}

// UnblockTarget puts the failure of object back to failed with the same commit, without deleting
// it: a blocked target still runs containers labeled with the failed commit, and without the
// failure the next poll would see "no changes" and never retry (#1702). An unreadable record,
// which blocks the target, is replaced by a failed one of the deploy stage, so the next run
// retries with a full recreate. It reports false when nothing is blocked.
func UnblockTarget(dataMountPath, object string) (bool, error) {
	st := rollback.StoreFor(dataMountPath)

	f, ok, err := st.GetFailure(object)

	switch {
	case err != nil:
		f = rollback.Failure{Object: object, Stage: string(StageDeploy), FailedAt: time.Now().UTC()}
	case !ok || f.State == rollback.StateFailed:
		return false, nil
	}

	f.State, f.Decision = rollback.StateFailed, ""

	return true, st.PutFailure(f)
}

var errRollbackUnsupported = errors.New("rollback not supported for this deployment")

// stageExecutor redeploys the last good with the same DeployStack as a regular deploy.
type stageExecutor struct {
	s   *StageManager
	log *slog.Logger
}

func (e stageExecutor) Deploy(ctx context.Context, lg rollback.LastGood) error {
	req, err := e.lastGoodRequest(ctx, lg)
	if err != nil {
		return err
	}

	return deployLastGood(ctx, req)
}

// lastGoodRequest republishes the artifact of the last good commit, renders it with the saved
// deploy config, pins the images, applies the compose policy in force and returns the deploy
// request: a failed_deploy_retry change force recreates the whole project.
//
// No phase reporter, commit status or signal: a rollback is not the deploy of the requested
// commit.
func (e stageExecutor) lastGoodRequest(ctx context.Context, lg rollback.LastGood) (docker.DeployRequest, error) {
	s := e.s

	// Config and compose in different repositories (repository_url), OCI sources and Swarm do not
	// roll back (spec, "Last good" and "Rischi"); nor does a last good of another repository.
	switch {
	case s.Docker.SwarmMode:
		return docker.DeployRequest{}, fmt.Errorf("%w: swarm mode", errRollbackUnsupported)
	case s.Repository.Source == config.SourceTypeOCI:
		return docker.DeployRequest{}, fmt.Errorf("%w: OCI source", errRollbackUnsupported)
	case s.DeployConfig.RepositoryUrl != "":
		return docker.DeployRequest{}, fmt.Errorf("%w: repository_url", errRollbackUnsupported)
	case !sameRepository(lg.SourceURL, s.Repository.SourceUrl):
		return docker.DeployRequest{}, fmt.Errorf("%w: last good was recorded from another repository", errRollbackUnsupported)
	}

	cfg, err := decodeLastGoodConfig(lg.DeployConfig)
	if err != nil {
		return docker.DeployRequest{}, err
	}

	if cfg.RepositoryUrl != "" {
		return docker.DeployRequest{}, fmt.Errorf("%w: repository_url", errRollbackUnsupported)
	}

	internalPath, externalPath, err := e.publishLastGood(ctx, lg.CommitSHA)
	if err != nil {
		return docker.DeployRequest{}, err
	}

	declared := declaredFilesOf(cfg)

	if err = s.loadLastGoodFiles(ctx, cfg, internalPath); err != nil {
		return docker.DeployRequest{}, err
	}

	// Labels and reconciliation compare this hash, as after pre-deploy. The config was just
	// decoded, which validates every duration, so encoding it again cannot fail.
	cfg.Internal.Hash, _ = cfg.Hash()

	project, err := s.renderLastGood(ctx, cfg, internalPath, externalPath)
	if err != nil {
		return docker.DeployRequest{}, err
	}

	if err = rollback.PinImages(project, lg.Images, e.localImageExists(ctx)); err != nil {
		return docker.DeployRequest{}, err
	}

	if err = s.checkLastGoodPolicy(cfg, project, declared, internalPath, externalPath); err != nil {
		return docker.DeployRequest{}, err
	}

	// The images are pinned: a forced pull would replace an image ID with a pull of "sha256".
	cfg.ForceImagePull = false

	// Scheduled jobs reload the deploy config from the artifact named by this label: after a
	// rollback it is the last good one (same repository, see the checks above).
	if s.Repository.ConfigRevision != "" {
		cfg.Internal.ConfigSourceRevision = lg.CommitSHA
		cfg.Internal.ConfigSourceWorkingDir = externalPath
	}

	return docker.DeployRequest{
		JobLog:           e.log,
		ExternalRepoPath: externalPath,
		InternalRepoPath: internalPath,
		DockerCLI:        s.Docker.Cmd,
		Payload:          s.Payload,
		RepositoryName:   s.Repository.Name,
		SourceURL:        sourceURLForLabels(s.Repository),
		DeployConfig:     cfg,
		DetectedChanges:  []docker.Change{{Type: docker.ChangeTypeFailedDeployRetry}},
		LatestCommit:     lg.CommitSHA,
		AppVersion:       app.Version,
		ComposeLoad:      docker.NewComposeLoadOptions(s.AppConfig),
		SwarmRetention:   docker.NewSwarmRetentionOptions(s.AppConfig),
		// DeployStack hashes the preloaded project itself when ProjectHash is empty.
		HashNormMap: pkiRoleNormMap(cfg.ExternalSecrets, cfg.Internal.Environment),
		Project:     project,
	}, nil
}

// sameRepository compares the source URL recorded with the last good, already stripped of its
// userinfo, with the current one. Both sides go through withoutUserinfo, then through the name
// that keys the store and its mirror (<data>/<name>), so a credential or a ".git" suffix does not
// make the same repository look different.
func sameRepository(recorded, current string) bool {
	return gitInternal.GetRepoName(withoutUserinfo(recorded)) == gitInternal.GetRepoName(withoutUserinfo(current))
}

// publishLastGood republishes the artifact of commit with the stack's own store. The lastgood
// ref keeps the commit in the mirror, so Publish needs no remote; only a commit really missing
// from the mirror (the ref is lost when the mirror is cloned again) is asked to the remote as a
// pinned commit. A commit gone from both is rollback_failed (spec, "Limiti noti").
func (e stageExecutor) publishLastGood(ctx context.Context, commit string) (string, string, error) {
	s := e.s

	// After the init stage PathInternal is the artifact; the store's base is the parent of the
	// mirror ("<BaseDir>/mirror").
	if s.Repository.MirrorDir == "" || filepath.Base(s.Repository.MirrorDir) != store.MirrorSubdir {
		return "", "", fmt.Errorf("unexpected mirror layout %q", s.Repository.MirrorDir)
	}

	gitStore, err := s.newGitStore(e.log, filepath.Dir(s.Repository.MirrorDir))
	if err != nil {
		return "", "", fmt.Errorf("initialize git store: %w", err)
	}

	artifact, err := gitStore.Publish(ctx, store.Revision(commit))
	if errors.Is(err, store.ErrRevisionNotFound) {
		var revision store.Revision

		if revision, err = gitStore.Resolve(ctx, commit); err == nil {
			artifact, err = gitStore.Publish(ctx, revision)
		}
	}

	if err != nil {
		return "", "", fmt.Errorf("publish last good commit %s: %w", commit, err)
	}

	rel, err := filepath.Rel(s.Docker.DataMountPoint.Destination, artifact.Path)
	if err != nil {
		return "", "", fmt.Errorf("compute external path of last good artifact: %w", err)
	}

	return artifact.Path, filepath.Join(s.Docker.DataMountPoint.Source, rel), nil
}

// decodeLastGoodConfig reads the saved deploy config back, with the upstream defaults
// (Config.UnmarshalYAML applies them) for the keys it does not hold.
func decodeLastGoodConfig(data []byte) (*deploy.Config, error) {
	var cfg deploy.Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("decode last good deploy config: %w", err)
	}

	return &cfg, nil
}

// loadLastGoodFiles reads dotenv and external_secrets_files from the artifact as the init stage
// does, then resolves the external secrets with the providers of now: a secret rotated meanwhile
// takes its new value.
//
// The saved external_secrets were recorded after MergeExternalSecretsFromFiles and
// InterpolateExternalSecretRefs, so they are the final references. They win over the entries
// read again from the files (MergeExternalSecretsFromFiles lets the inline ones win), and they
// are not interpolated again: a second pass would expand what the first pass unescaped ($$).
func (s *StageManager) loadLastGoodFiles(ctx context.Context, cfg *deploy.Config, internalPath string) error {
	if strings.Contains(cfg.WorkingDirectory, "..") {
		return errors.New("invalid working directory: must not contain '..' to prevent directory traversal")
	}

	workDir := filepath.Join(internalPath, cfg.WorkingDirectory)

	if err := deploy.LoadLocalDotEnv(cfg, workDir); err != nil {
		return fmt.Errorf("failed to parse env files of last good: %w", err)
	}

	if err := deploy.LoadExternalSecretsFiles(cfg, workDir); err != nil {
		return fmt.Errorf("failed to parse external secrets files of last good: %w", err)
	}

	mergeDeploymentEnvironment(cfg)
	deploy.MergeExternalSecretsFromFiles(cfg)

	if s.SecretProvider == nil || len(cfg.ExternalSecrets) == 0 {
		return nil
	}

	encoded, err := secrettypes.EncodeExternalSecretRefs(cfg.ExternalSecrets)
	if err != nil {
		return fmt.Errorf("failed to encode external secret references: %w", err)
	}

	resolved, err := s.SecretProvider.ResolveSecretReferences(ctx, encoded)
	if err != nil {
		return fmt.Errorf("failed to resolve external secrets: %w", err)
	}

	resolved, err = secrettypes.InterpolateResolvedSecrets(resolved, s.AppConfig.InterpolateResolvedSecrets)
	if err != nil {
		return fmt.Errorf("failed to interpolate resolved secrets: %w", err)
	}

	// LoadLocalDotEnv made Internal.Environment.
	maps.Copy(cfg.Internal.Environment, resolved)

	return nil
}

// renderLastGood loads the compose project of the artifact as loadComposeProjectHash does.
// loadLastGoodFiles already refused a working directory with "..", so it stays in the artifact.
func (s *StageManager) renderLastGood(ctx context.Context, cfg *deploy.Config, internalPath, externalPath string) (*types.Project, error) {
	var err error

	if cfg.ComposeFiles, err = docker.CheckDefaultComposeFiles(cfg.ComposeFiles, filepath.Join(internalPath, cfg.WorkingDirectory)); err != nil {
		return nil, fmt.Errorf("failed to check for default compose files: %w", err)
	}

	opts := docker.NewComposeLoadOptions(s.AppConfig)
	opts.MutationLock = sourcecache.NewPathLocker(internalPath)

	project, err := docker.LoadCompose(ctx, s.Docker.Cmd, externalPath, filepath.Join(externalPath, cfg.WorkingDirectory), cfg.Name,
		cfg.ComposeFiles, cfg.EnvFiles, cfg.Profiles, cfg.Internal.Environment, opts)
	if err != nil {
		return nil, fmt.Errorf("load last good compose project: %w", err)
	}

	return project, nil
}

// checkLastGoodPolicy applies the compose policy in force to the pinned last good project, with
// the files the saved config declares: a violation is rollback_failed.
func (s *StageManager) checkLastGoodPolicy(cfg *deploy.Config, project *types.Project, declared declaredFiles, internalPath, externalPath string) error {
	f := composePolicy.Load().Current()
	if f == nil {
		return nil
	}

	view := &StageManager{
		Repository:   &RepositoryData{PathExternal: externalPath, PathInternal: internalPath},
		DeployConfig: cfg,
		Docker:       s.Docker,
	}

	violations := f.Check(composepolicy.Input{
		Project: project,
		Identity: composepolicy.Identity{
			Object:       cfg.Object(),
			Repository:   cmp.Or(s.Repository.ConfigSourceUrl, s.Repository.SourceUrl),
			Reference:    s.Repository.ResolvedReference,
			ConfigTarget: s.DeployConfig.Internal.ConfigTarget,
		},
		Paths:          view.policyPaths(),
		DeployEnvFiles: view.externalPaths(declared.EnvFiles, true),
		SecretsFiles:   view.externalPaths(declared.ExternalSecretsFiles, false),
	})
	if len(violations) > 0 {
		return &PolicyDeniedError{Object: cfg.Object(), Violations: violations}
	}

	return nil
}

func (e stageExecutor) localImageExists(ctx context.Context) func(string) bool {
	return func(id string) bool {
		_, err := e.s.Docker.Cmd.Client().ImageInspect(ctx, id)
		return err == nil
	}
}
