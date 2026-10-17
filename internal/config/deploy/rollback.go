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
	// DefaultRollbackTimeout is the rollback timeout when the key is missing or zero.
	DefaultRollbackTimeout = 5 * time.Minute
	// DefaultPreRollbackTimeout is the pre_rollback wait when the key is missing or zero.
	DefaultPreRollbackTimeout = 30 * time.Second
)

// RollbackConfig enables local rollback to the last good deploy (Portico fork).
// It is written as a bool or as an object, like destroy.
type RollbackConfig struct {
	Enabled            bool              `yaml:"enabled" json:"enabled" default:"false"`           // Enabled turns on rollback and the on-disk failure store for this target
	Timeout            duration.Duration `yaml:"timeout" json:"timeout"`                           // Timeout of the rollback: Create/Start plus wait healthy (default 5m)
	PreRollbackTimeout duration.Duration `yaml:"pre_rollback_timeout" json:"pre_rollback_timeout"` // PreRollbackTimeout is the maximum wait for the pre_rollback response (default 30s)
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

// RollbackEnabled reports whether the target has rollback enabled.
func (c *Config) RollbackEnabled() bool {
	return c != nil && c.Rollback != nil && c.Rollback.Enabled
}

// EffectiveTimeout returns the rollback timeout, falling back to the default if zero.
func (c *RollbackConfig) EffectiveTimeout() time.Duration {
	if c == nil || c.Timeout == 0 {
		return DefaultRollbackTimeout
	}

	return c.Timeout.Duration()
}

// EffectivePreRollbackTimeout returns the pre_rollback wait, falling back to the default if zero.
func (c *RollbackConfig) EffectivePreRollbackTimeout() time.Duration {
	if c == nil || c.PreRollbackTimeout == 0 {
		return DefaultPreRollbackTimeout
	}

	return c.PreRollbackTimeout.Duration()
}
