package config

import (
	"path/filepath"
	"testing"
)

func TestLoadSaveUpdateRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "tag.yaml")
	if err := Save(p, map[string]any{"defaults": map[string]any{"master_profile": "coder"}}); err != nil {
		t.Fatalf("save: %v", err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.MasterProfile() != "coder" {
		t.Errorf("master_profile = %q", c.MasterProfile())
	}
	// atomic RMW
	_, err = Update(p, func(m map[string]any) {
		m["defaults"].(map[string]any)["master_profile"] = "reviewer"
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	c2, _ := Load(p)
	if c2.MasterProfile() != "reviewer" {
		t.Errorf("after update master_profile = %q", c2.MasterProfile())
	}
}

func TestMalformedYAMLRejected(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bad.yaml")
	Save(p, map[string]any{"ok": 1})
	// overwrite with malformed
	writeRaw(t, p, "not: [valid: : :")
	if _, err := Load(p); err == nil {
		t.Error("malformed YAML should error")
	}
}
