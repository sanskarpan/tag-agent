package cli_test

import "testing"

// TestPluginRejectsUnknownProfile: plugin enable/install/disable must refuse an
// unknown profile (exit 2), not silently create a phantom profiles/<name>/.env
// and report success — matching set-model and import-* (#756). RED against
// pre-fix code, which exited 0 and created the phantom dir.
func TestPluginRejectsUnknownProfile(t *testing.T) {
	h := newHome(t)
	for _, sub := range []string{"enable", "disable"} {
		if _, code := run(t, h, "plugin", sub, "hermes-web-search", "--profile", "NOPE"); code != 2 {
			t.Errorf("plugin %s --profile NOPE should exit 2, got %d", sub, code)
		}
	}
	// A real profile still works.
	if _, code := run(t, h, "plugin", "enable", "hermes-web-search", "--profile", "coder"); code != 0 {
		t.Errorf("plugin enable on a real profile should exit 0, got %d", code)
	}
}
