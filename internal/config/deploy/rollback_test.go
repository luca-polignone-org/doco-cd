package deploy

import (
	"encoding/json"
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

// Senza la chiave, l'hash della config non cambia rispetto a upstream:
// un upgrade al fork non deve rideployare nessuno.
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

// Una config YAML senza la chiave lascia Rollback a nil dopo i default.
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
