package deploy

import (
	"encoding/json"
	"strings"
	"testing"

	"go.yaml.in/yaml/v4"
)

func TestUnblockOn(t *testing.T) {
	for in, want := range map[string]bool{
		"enabled: true\n":                      false,
		"enabled: true\nunblock_on: commit\n":  false,
		"enabled: true\nunblock_on: content\n": true,
	} {
		var rc RollbackConfig
		if err := yaml.Unmarshal([]byte(in), &rc); err != nil {
			t.Fatalf("%q: %v", in, err)
		}

		if rc.UnblocksOnContent() != want {
			t.Fatalf("%q: UnblocksOnContent() = %v, want %v", in, rc.UnblocksOnContent(), want)
		}
	}

	var rc RollbackConfig
	if err := yaml.Unmarshal([]byte("enabled: true\nunblock_on: hash\n"), &rc); err == nil {
		t.Fatal("an unknown unblock_on must be refused")
	}

	if err := json.Unmarshal([]byte(`{"enabled":true,"unblock_on":"hash"}`), &rc); err == nil {
		t.Fatal("an unknown unblock_on must be refused in JSON too")
	}

	var nilRC *RollbackConfig
	if nilRC.UnblocksOnContent() {
		t.Fatal("a nil config unblocks on commit")
	}
}

// TestUnblockOnKeepsHash: a config without the key hashes as before the key existed.
func TestUnblockOnKeepsHash(t *testing.T) {
	c := New("web", "main")
	c.Rollback = &RollbackConfig{Enabled: true}

	data, err := yaml.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(string(data), "unblock_on") {
		t.Fatalf("marshaled config carries unblock_on without the key:\n%s", data)
	}
}
