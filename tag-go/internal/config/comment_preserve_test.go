package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestUpdatePreservesComments: a config edit must keep the file's comments and
// only change the touched key (ISSUE-018 / #746 #754). RED against pre-fix code,
// which marshalled the plain map and dropped every comment.
func TestUpdatePreservesComments(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tag.yaml")
	orig := `# top-of-file docs comment
profiles:
  # the coder profile
  coder:
    config:
      model:
        provider: openrouter
        default: qwen/qwen3-coder  # inline comment
defaults:
  master_profile: coder
`
	if err := os.WriteFile(path, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Update(path, func(d map[string]any) {
		d["profiles"].(map[string]any)["coder"].(map[string]any)["config"].(map[string]any)["model"].(map[string]any)["default"] = "openrouter/new-x"
	}); err != nil {
		t.Fatal(err)
	}
	out, _ := os.ReadFile(path)
	s := string(out)
	for _, want := range []string{"# top-of-file docs comment", "# the coder profile", "# inline comment"} {
		if !strings.Contains(s, want) {
			t.Errorf("comment %q was stripped by Update:\n%s", want, s)
		}
	}
	if !strings.Contains(s, "openrouter/new-x") {
		t.Errorf("the edit did not apply:\n%s", s)
	}
}
