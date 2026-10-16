package deploy

import (
	"bytes"
	"encoding/json"
	"errors"

	"go.yaml.in/yaml/v4"

	"github.com/kimdre/doco-cd/internal/common/defaults"
	"github.com/kimdre/doco-cd/internal/common/types/duration"
)

// RollbackConfig accende il rollback locale all'ultimo deploy buono (fork Portico).
// Si scrive come bool o come oggetto, come destroy.
type RollbackConfig struct {
	Enabled            bool              `yaml:"enabled" json:"enabled" default:"false"`                         // Enabled abilita rollback e failure store su disco per questo target
	Timeout            duration.Duration `yaml:"timeout" json:"timeout" default:"5m"`                            // Timeout del rollback: Create/Start più wait healthy
	PreRollbackTimeout duration.Duration `yaml:"pre_rollback_timeout" json:"pre_rollback_timeout" default:"30s"` // PreRollbackTimeout è l'attesa massima della risposta di pre_rollback
}

var errInvalidRollback = errors.New("invalid rollback value: expected bool or object")

func (c *RollbackConfig) UnmarshalYAML(node *yaml.Node) error {
	type plain RollbackConfig

	decoded := plain{}
	if err := defaults.Set(&decoded); err != nil {
		return err
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

	decoded := plain{}
	if err := defaults.Set(&decoded); err != nil {
		return err
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
