package cli_test

import (
	"path/filepath"
	"testing"
)

// TestTemplateImportRegistersProfile: `template import` must register the
// profile in tag.yaml so profile-aware commands can see it — not just write
// runtime files and claim success for a profile nothing can use (#736). RED
// against pre-fix code, where `models <name>` returned "unknown profile".
func TestTemplateImportRegistersProfile(t *testing.T) {
	h := newHome(t)
	tf := filepath.Join(t.TempDir(), "t.yaml")
	if _, code := run(t, h, "template", "export", "--profile", "coder", "--output", tf); code != 0 {
		t.Fatalf("export failed: %d", code)
	}
	if _, code := run(t, h, "template", "import", tf, "--profile", "newcoder"); code != 0 {
		t.Fatalf("import failed: %d", code)
	}
	// The imported profile must now be visible (models resolves it, exit 0).
	if out, code := run(t, h, "models", "newcoder"); code != 0 {
		t.Errorf("imported profile must be visible to models, got exit %d: %s", code, out)
	}
}
