package deploy

import (
	"bytes"
	"encoding/json"
	"errors"
	"time"

	"go.yaml.in/yaml/v4"

	"github.com/kimdre/doco-cd/internal/common/types/duration"
)

const (
	// DefaultRollbackTimeout è il timeout del rollback quando la chiave manca o è zero.
	DefaultRollbackTimeout = 5 * time.Minute
	// DefaultPreRollbackTimeout è l'attesa di pre_rollback quando la chiave manca o è zero.
	DefaultPreRollbackTimeout = 30 * time.Second
)

// RollbackConfig accende il rollback locale all'ultimo deploy buono (fork Portico).
// Si scrive come bool o come oggetto, come destroy.
type RollbackConfig struct {
	Enabled            bool              `yaml:"enabled" json:"enabled" default:"false"`           // Enabled abilita rollback e failure store su disco per questo target
	Timeout            duration.Duration `yaml:"timeout" json:"timeout"`                           // Timeout del rollback: Create/Start più wait healthy (default 5m)
	PreRollbackTimeout duration.Duration `yaml:"pre_rollback_timeout" json:"pre_rollback_timeout"` // PreRollbackTimeout è l'attesa massima della risposta di pre_rollback (default 30s)
}

var errInvalidRollback = errors.New("invalid rollback value: expected bool or object")

func (c *RollbackConfig) UnmarshalYAML(node *yaml.Node) error {
	type plain RollbackConfig

	decoded := plain{
		Timeout:            duration.Duration(DefaultRollbackTimeout),
		PreRollbackTimeout: duration.Duration(DefaultPreRollbackTimeout),
	}

	switch node.Kind {
	case yaml.ScalarNode:
		if err := node.Decode(&decoded.Enabled); err != nil {
			return errInvalidRollback
		}
	case yaml.MappingNode:
		if err := node.Decode(&decoded); err != nil {
			return err
		}
	default:
		return errInvalidRollback
	}

	*c = RollbackConfig(decoded)

	return nil
}

func (c *RollbackConfig) UnmarshalJSON(data []byte) error {
	type plain RollbackConfig

	decoded := plain{
		Timeout:            duration.Duration(DefaultRollbackTimeout),
		PreRollbackTimeout: duration.Duration(DefaultPreRollbackTimeout),
	}

	trimmed := bytes.TrimSpace(data)
	if bytes.Equal(trimmed, []byte("true")) || bytes.Equal(trimmed, []byte("false")) {
		decoded.Enabled = bytes.Equal(trimmed, []byte("true"))
	} else if err := json.Unmarshal(data, &decoded); err != nil {
		return errInvalidRollback
	}

	*c = RollbackConfig(decoded)

	return nil
}

// RollbackEnabled dice se il target ha il rollback abilitato.
func (c *Config) RollbackEnabled() bool {
	return c != nil && c.Rollback != nil && c.Rollback.Enabled
}

// EffectiveTimeout restituisce il timeout del rollback, con il default se zero.
func (c *RollbackConfig) EffectiveTimeout() time.Duration {
	if c == nil || c.Timeout == 0 {
		return DefaultRollbackTimeout
	}

	return c.Timeout.Duration()
}

// EffectivePreRollbackTimeout restituisce l'attesa di pre_rollback, con il default se zero.
func (c *RollbackConfig) EffectivePreRollbackTimeout() time.Duration {
	if c == nil || c.PreRollbackTimeout == 0 {
		return DefaultPreRollbackTimeout
	}

	return c.PreRollbackTimeout.Duration()
}
