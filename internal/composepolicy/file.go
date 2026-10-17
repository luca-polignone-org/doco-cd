package composepolicy

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"
	"go.yaml.in/yaml/v4"

	"github.com/kimdre/doco-cd/internal/config"
)

// Violation is a compose policy rule that was not respected. Service is empty for
// project-level rules (identity, swarm, secrets, configs, deploy files).
type Violation struct{ Service, Field, Value string }

// TargetPolicy is the policy of a <context>/<project> object.
type TargetPolicy struct {
	Repository      string   `yaml:"repository"`
	Reference       string   `yaml:"reference"`
	ConfigTarget    string   `yaml:"config_target"`
	BindAllowlist   []string `yaml:"bind_allowlist"`
	CapAddAllowlist []string `yaml:"cap_add_allowlist"`
}

// File is the compose policy compiled by Portico (3b). The keys of targets are exact
// <context>/<project> objects: a glob would bind more objects to a repository than
// the ones declared.
type File struct {
	Version int                     `yaml:"version"`
	Default TargetPolicy            `yaml:"default"`
	Targets map[string]TargetPolicy `yaml:"targets"`
}

// Identity is the identity the deploy presents: object, source and config target.
type Identity struct {
	Object, Repository, Reference, ConfigTarget string
	Swarm                                       bool
}

// Paths are the checkouts Doco-CD reads files from: the deploy one and, if the deploy
// config comes from another repository (repository_url), the config source one.
type Paths struct{ CheckoutExternal, CheckoutInternal, ConfigExternal, ConfigInternal string }

// Input is what Check evaluates.
type Input struct {
	Project        *types.Project
	Identity       Identity
	Paths          Paths
	DeployEnvFiles []string // deploy.Config.EnvFiles resolved as host paths (external)
	SecretsFiles   []string // deploy.Config.ExternalSecretsFiles resolved (external)
}

const fileVersion = 1

// Parse reads and normalizes a compose policy; any defect is an error.
func Parse(data []byte) (*File, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	var f File
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("decode compose policy: %w", err)
	}

	// A second document would be silently ignored: better to reject the file.
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("compose policy: multiple YAML documents or trailing data")
	}

	if f.Version != fileVersion {
		return nil, fmt.Errorf("compose policy: unsupported version %d", f.Version)
	}

	// default carries only the lists: a default repository or reference would
	// bind every entry that does not declare its own.
	if strings.TrimSpace(f.Default.Repository+f.Default.Reference+f.Default.ConfigTarget) != "" {
		return nil, errors.New("compose policy default: repository, reference and config_target are not allowed")
	}

	if err := normalizeTarget(&f.Default, false); err != nil {
		return nil, fmt.Errorf("compose policy default: %w", err)
	}

	for object, tp := range f.Targets {
		if strings.ContainsAny(object, "*?[") {
			return nil, fmt.Errorf("compose policy target %q: globs are not allowed", object)
		}

		ctx, project, ok := strings.Cut(object, "/")
		if !ok || ctx == "" || project == "" || strings.Contains(project, "/") {
			return nil, fmt.Errorf("compose policy target %q: must be <context>/<project>", object)
		}

		if err := normalizeTarget(&tp, true); err != nil {
			return nil, fmt.Errorf("compose policy target %q: %w", object, err)
		}

		f.Targets[object] = tp
	}

	return &f, nil
}

func normalizeTarget(tp *TargetPolicy, needRepository bool) error {
	tp.Repository = strings.TrimSpace(tp.Repository)
	if needRepository && tp.Repository == "" {
		return errors.New("repository is required")
	}

	if tp.Repository != "" {
		tp.Repository = config.NormalizeGitURL(tp.Repository)
	}

	tp.Reference = strings.TrimSpace(tp.Reference)

	for i, p := range tp.BindAllowlist {
		if !filepath.IsAbs(p) {
			return fmt.Errorf("bind_allowlist entry %q must be absolute", p)
		}

		tp.BindAllowlist[i] = filepath.Clean(p)
	}

	for i, c := range tp.CapAddAllowlist {
		c = strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(c)), "CAP_")
		if c == "ALL" || c == "" {
			return fmt.Errorf("cap_add_allowlist entry %q is not allowed", c)
		}

		tp.CapAddAllowlist[i] = c
	}

	return nil
}
