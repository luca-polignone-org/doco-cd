package deploy

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v4"

	"github.com/kimdre/doco-cd/internal/common/types/duration"
)

func TestRollbackConfigYAML(t *testing.T) {
	var c struct {
		Rollback *RollbackConfig `yaml:"rollback,omitempty"`
	}

	if err := yaml.Unmarshal([]byte("rollback: true"), &c); err != nil {
		t.Fatal(err)
	}

	if !c.Rollback.Enabled || c.Rollback.Timeout.Duration() != 5*time.Minute ||
		c.Rollback.PreRollbackTimeout.Duration() != 30*time.Second {
		t.Fatalf("defaults not applied: %+v", c.Rollback)
	}

	c.Rollback = nil
	if err := yaml.Unmarshal([]byte("rollback: {enabled: true, timeout: 2m}"), &c); err != nil {
		t.Fatal(err)
	}

	if c.Rollback.Timeout.Duration() != 2*time.Minute || c.Rollback.PreRollbackTimeout.Duration() != 30*time.Second {
		t.Fatalf("%+v", c.Rollback)
	}

	if err := yaml.Unmarshal([]byte("rollback: {timeout: [1]}"), &c); err == nil {
		t.Fatal("expected error for invalid mapping field")
	}

	if err := yaml.Unmarshal([]byte("rollback: [1]"), &c); err == nil {
		t.Fatal("expected error for sequence")
	}

	if err := yaml.Unmarshal([]byte("rollback: maybe"), &c); err == nil {
		t.Fatal("expected error for non-bool scalar")
	}
}

func TestRollbackConfigJSON(t *testing.T) {
	var c RollbackConfig
	if err := json.Unmarshal([]byte("true"), &c); err != nil || !c.Enabled {
		t.Fatalf("%v %+v", err, c)
	}

	if err := json.Unmarshal([]byte("false"), &c); err != nil || c.Enabled {
		t.Fatalf("%v %+v", err, c)
	}

	if err := json.Unmarshal([]byte(`{"enabled":true,"pre_rollback_timeout":"10s"}`), &c); err != nil ||
		c.PreRollbackTimeout.Duration() != 10*time.Second || c.Timeout.Duration() != 5*time.Minute {
		t.Fatalf("%v %+v", err, c)
	}

	c = RollbackConfig{Timeout: 1}
	if err := json.Unmarshal([]byte("null"), &c); err != nil {
		t.Fatal(err)
	}

	if err := json.Unmarshal([]byte(`"x"`), &c); err == nil {
		t.Fatal("expected error")
	}
}

func TestRollbackEffectiveTimeouts(t *testing.T) {
	var nilCfg *RollbackConfig
	if nilCfg.EffectiveTimeout() != DefaultRollbackTimeout || nilCfg.EffectivePreRollbackTimeout() != DefaultPreRollbackTimeout {
		t.Fatal("nil config must yield defaults")
	}

	zero := &RollbackConfig{}
	if zero.EffectiveTimeout() != 5*time.Minute || zero.EffectivePreRollbackTimeout() != 30*time.Second {
		t.Fatal("zero fields must yield defaults")
	}

	set := &RollbackConfig{Timeout: duration.Duration(time.Minute), PreRollbackTimeout: duration.Duration(time.Second)}
	if set.EffectiveTimeout() != time.Minute || set.EffectivePreRollbackTimeout() != time.Second {
		t.Fatal("set fields must be returned as is")
	}
}

// Without the key, the config hash is unchanged from upstream:
// upgrading to the fork must not redeploy anyone.
func TestRollbackAbsentKeepsHash(t *testing.T) {
	c := &Config{Name: "x"}

	h1, err := c.Hash()
	if err != nil {
		t.Fatal(err)
	}

	out, _ := yaml.Marshal(c)
	if containsKey(string(out), "rollback") {
		t.Fatalf("rollback key serialized: %s", out)
	}

	c.Rollback = &RollbackConfig{Enabled: true}

	h2, _ := c.Hash()
	if h1 == h2 {
		t.Fatal("hash must change when rollback is set")
	}

	if (&Config{}).RollbackEnabled() || !c.RollbackEnabled() {
		t.Fatal("RollbackEnabled")
	}

	var nilCfg *Config
	if nilCfg.RollbackEnabled() {
		t.Fatal("nil Config must report rollback disabled")
	}
}

// A YAML config without the key leaves Rollback nil after defaults.
func TestRollbackAbsentStaysNilAfterDefaults(t *testing.T) {
	var c Config
	if err := yaml.Unmarshal([]byte("name: x"), &c); err != nil {
		t.Fatal(err)
	}

	if c.Rollback != nil {
		t.Fatalf("Rollback must stay nil, got %+v", c.Rollback)
	}
}

func containsKey(doc, key string) bool {
	var m map[string]any

	_ = yaml.Unmarshal([]byte(doc), &m)
	_, ok := m[key]

	return ok
}

// The timeouts come from the app repository: both decoders cap them, and refuse negative ones.
func TestRollbackTimeoutCeiling(t *testing.T) {
	accepted := []string{
		"{enabled: true, timeout: 30m, pre_rollback_timeout: 5m}",
		"{enabled: true, timeout: 0s, pre_rollback_timeout: 0s}",
	}

	for _, doc := range accepted {
		var c RollbackConfig
		if err := yaml.Unmarshal([]byte(doc), &c); err != nil {
			t.Fatalf("yaml %s: %v", doc, err)
		}
	}

	refused := map[string]string{
		"{timeout: 31m}":              "timeout must be between 0 and 30m0s",
		"{timeout: -1s}":              "timeout must be between 0 and 30m0s",
		"{pre_rollback_timeout: 6m}":  "pre_rollback_timeout must be between 0 and 5m0s",
		"{pre_rollback_timeout: -1s}": "pre_rollback_timeout must be between 0 and 5m0s",
	}

	for doc, want := range refused {
		var c RollbackConfig

		err := yaml.Unmarshal([]byte(doc), &c)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("yaml %s: error = %v, want %q", doc, err, want)
		}
	}

	var c RollbackConfig
	if err := json.Unmarshal([]byte(`{"enabled":true,"timeout":"30m","pre_rollback_timeout":"5m"}`), &c); err != nil {
		t.Fatalf("json at the ceiling: %v", err)
	}

	if err := json.Unmarshal([]byte(`{"timeout":"1h"}`), &c); !errors.Is(err, errInvalidRollbackTimeout) {
		t.Fatalf("json over the ceiling: error = %v", err)
	}
}
