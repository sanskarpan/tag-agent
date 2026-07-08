package paths

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExpandTilde(t *testing.T) {
	home, _ := os.UserHomeDir()
	if got := Expand("~"); got != home {
		t.Errorf("Expand(~) = %q, want %q", got, home)
	}
	if got := Expand("~/foo/bar"); got != filepath.Join(home, "foo/bar") {
		t.Errorf("Expand(~/foo/bar) = %q", got)
	}
	if got := Expand("/abs/path"); got != "/abs/path" {
		t.Errorf("absolute path should be unchanged, got %q", got)
	}
	if got := Expand("rel/path"); got != "rel/path" {
		t.Errorf("relative path should be unchanged, got %q", got)
	}
}
