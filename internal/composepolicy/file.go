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

// Violation è una regola della compose policy non rispettata. Service è vuoto per le
// regole di progetto (identità, swarm, secrets, configs, file del deploy).
type Violation struct{ Service, Field, Value string }

// TargetPolicy è la policy di un oggetto <context>/<project>.
type TargetPolicy struct {
	Repository      string   `yaml:"repository"`
	Reference       string   `yaml:"reference"`
	ConfigTarget    string   `yaml:"config_target"`
	BindAllowlist   []string `yaml:"bind_allowlist"`
	CapAddAllowlist []string `yaml:"cap_add_allowlist"`
}

// File è la compose policy compilata da Portico (3b). Le chiavi di targets sono
// oggetti esatti <context>/<project>: un glob legherebbe a un repository più oggetti
// di quelli dichiarati.
type File struct {
	Version int                     `yaml:"version"`
	Default TargetPolicy            `yaml:"default"`
	Targets map[string]TargetPolicy `yaml:"targets"`
}

// Identity è l'identità con cui il deploy si presenta: oggetto, sorgente e target di config.
type Identity struct {
	Object, Repository, Reference, ConfigTarget string
	Swarm                                       bool
}

// Paths sono le checkout da cui Doco-CD legge file: quella del deploy e, se la deploy
// config viene da un altro repository (repository_url), quella del config source.
type Paths struct{ CheckoutExternal, CheckoutInternal, ConfigExternal, ConfigInternal string }

// Input è quanto Check valuta.
type Input struct {
	Project        *types.Project
	Identity       Identity
	Paths          Paths
	DeployEnvFiles []string // deploy.Config.EnvFiles risolti come path dell'host (external)
	SecretsFiles   []string // deploy.Config.ExternalSecretsFiles risolti (external)
}

const fileVersion = 1

// Parse legge e normalizza una compose policy; qualunque difetto è un errore.
func Parse(data []byte) (*File, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	var f File
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("decode compose policy: %w", err)
	}

	// Un secondo documento sarebbe ignorato in silenzio: meglio rifiutare il file.
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("compose policy: multiple YAML documents or trailing data")
	}

	if f.Version != fileVersion {
		return nil, fmt.Errorf("compose policy: unsupported version %d", f.Version)
	}

	// default porta solo le liste: un repository o una reference di default
	// legherebbero ogni voce che non li dichiara.
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
