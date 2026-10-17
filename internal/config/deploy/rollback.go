package deploy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"go.yaml.in/yaml/v4"

	"github.com/kimdre/doco-cd/internal/common/types/duration"
)

const (
	// DefaultRollbackTimeout is the rollback timeout when the key is missing or zero.
	DefaultRollbackTimeout = 5 * time.Minute
	// DefaultPreRollbackTimeout is the pre_rollback wait when the key is missing or zero.
	DefaultPreRollbackTimeout = 30 * time.Second
	// MaxRollbackTimeout caps timeout: the rollback holds the target lock and a mutation slot.
	MaxRollbackTimeout = 30 * time.Minute
	// MaxPreRollbackTimeout caps pre_rollback_timeout, for the same reason.
	MaxPreRollbackTimeout = 5 * time.Minute
)

// RollbackConfig enables local rollback to the last good deploy (Portico fork).
// It is written as a bool or as an object, like destroy.
type RollbackConfig struct {
	Enabled            bool              `yaml:"enabled" json:"enabled" default:"false"`           // Enabled turns on rollback and the on-disk failure store for this target
	Timeout            duration.Duration `yaml:"timeout" json:"timeout"`                           // Timeout of the rollback: Create/Start plus wait healthy (default 5m, at most 30m)
	PreRollbackTimeout duration.Duration `yaml:"pre_rollback_timeout" json:"pre_rollback_timeout"` // PreRollbackTimeout is the maximum wait for the pre_rollback response (default 30s, at most 5m)
	UnblockOn          string            `yaml:"unblock_on,omitempty" json:"unblock_on,omitempty"` // UnblockOn is commit (default) or content: with content a new commit releases a poisoned target only if its content changed
}

const (
	// UnblockOnCommit releases a poisoned target on any new commit (default).
	UnblockOnCommit = "commit"
	// UnblockOnContent releases it only when the working directory tree or the deploy config changed.
	UnblockOnContent = "content"
)

var (
	errInvalidRollback        = errors.New("invalid rollback value: expected bool or object")
	errInvalidRollbackTimeout = errors.New("invalid rollback timeout")
	errInvalidUnblockOn       = errors.New("invalid rollback unblock_on: expected commit or content")
)

// checkUnblockOn accepts an empty unblock_on (commit) and the two known values.
func checkUnblockOn(v string) error {
	switch v {
	case "", UnblockOnCommit, UnblockOnContent:
		return nil
	}

	return errInvalidUnblockOn
}

// UnblocksOnContent reports whether a new commit must also change the content to release the target.
func (c *RollbackConfig) UnblocksOnContent() bool {
	return c != nil && c.UnblockOn == UnblockOnContent
}

// checkRollbackTimeouts bounds the timeouts read from the repository: they come from the app's
// deploy config, and the rollback waits for them while it holds the fork target lock and a global
// mutation slot.
func checkRollbackTimeouts(timeout, preRollback duration.Duration) error {
	if timeout < 0 || timeout.Duration() > MaxRollbackTimeout {
		return fmt.Errorf("%w: timeout must be between 0 and %s", errInvalidRollbackTimeout, MaxRollbackTimeout)
	}

	if preRollback < 0 || preRollback.Duration() > MaxPreRollbackTimeout {
		return fmt.Errorf("%w: pre_rollback_timeout must be between 0 and %s", errInvalidRollbackTimeout, MaxPreRollbackTimeout)
	}

	return nil
}

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

	if err := checkRollbackTimeouts(decoded.Timeout, decoded.PreRollbackTimeout); err != nil {
		return err
	}

	if err := checkUnblockOn(decoded.UnblockOn); err != nil {
		return err
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

	if err := checkRollbackTimeouts(decoded.Timeout, decoded.PreRollbackTimeout); err != nil {
		return err
	}

	if err := checkUnblockOn(decoded.UnblockOn); err != nil {
		return err
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
