package tool

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tag-agent/tag/internal/agent"
)

func newReg(t *testing.T) (*agent.Registry, string) {
	root := t.TempDir()
	reg := agent.NewRegistry()
	Register(reg, Options{Root: root})
	return reg, root
}

func TestRegisterDisableBashOmitsBash(t *testing.T) {
	reg := agent.NewRegistry()
	Register(reg, Options{DisableBash: true})
	for _, d := range reg.Defs() {
		if d.Name == "bash" {
			t.Error("bash must be omitted when DisableBash is set")
		}
	}
}

func TestWriteReadRoundTrip(t *testing.T) {
	reg, root := newReg(t)
	out, err := runNamed(t, reg, "write_file", map[string]any{"path": "sub/hello.txt", "content": "hi there"})
	if err != nil || !strings.Contains(out, "wrote 8 bytes") {
		t.Fatalf("write_file: %q err=%v", out, err)
	}
	// file exists on disk under root
	if b, err := os.ReadFile(filepath.Join(root, "sub", "hello.txt")); err != nil || string(b) != "hi there" {
		t.Fatalf("file content wrong: %q err=%v", string(b), err)
	}
	got, err := runNamed(t, reg, "read_file", map[string]any{"path": "sub/hello.txt"})
	if err != nil || got != "hi there" {
		t.Errorf("read_file: %q err=%v", got, err)
	}
}

func TestListDir(t *testing.T) {
	reg, root := newReg(t)
	os.WriteFile(filepath.Join(root, "a.txt"), []byte("x"), 0o644)
	os.Mkdir(filepath.Join(root, "d"), 0o755)
	out, err := runNamed(t, reg, "list_dir", map[string]any{})
	if err != nil || !strings.Contains(out, "a.txt") || !strings.Contains(out, "d/") {
		t.Errorf("list_dir: %q err=%v", out, err)
	}
}

func TestBash(t *testing.T) {
	reg, _ := newReg(t)
	out, err := runNamed(t, reg, "bash", map[string]any{"command": "echo hello-from-bash"})
	if err != nil || !strings.Contains(out, "hello-from-bash") {
		t.Errorf("bash: %q err=%v", out, err)
	}
	// non-zero exit surfaces an error
	if _, err := runNamed(t, reg, "bash", map[string]any{"command": "exit 3"}); err == nil {
		t.Error("bash non-zero exit should error")
	}
}

func TestPathTraversalBlocked(t *testing.T) {
	reg, _ := newReg(t)
	if _, err := runNamed(t, reg, "read_file", map[string]any{"path": "../../../etc/passwd"}); err == nil {
		t.Error("path traversal should be blocked")
	}
	if _, err := runNamed(t, reg, "write_file", map[string]any{"path": "../escape.txt", "content": "x"}); err == nil {
		t.Error("write traversal should be blocked")
	}
}

func TestSymlinkEscapeBlocked(t *testing.T) {
	reg, root := newReg(t)
	// a secret file OUTSIDE the root, and a symlink INSIDE the root pointing to it
	outside := filepath.Join(t.TempDir(), "secret.txt")
	os.WriteFile(outside, []byte("TOPSECRET"), 0o644)
	link := filepath.Join(root, "link.txt")
	if err := os.Symlink(outside, link); err != nil {
		t.Skip("symlinks not supported here")
	}
	if _, err := runNamed(t, reg, "read_file", map[string]any{"path": "link.txt"}); err == nil {
		t.Error("reading through a symlink that escapes the root must be blocked")
	}
}

// Regression for issue #522: write_file must not escape the root when the
// target's intermediate directories don't exist yet and an ancestor is a
// symlink pointing outside the root. Previously the symlink guard was skipped
// when EvalSymlinks failed on the (non-existent) deep path, letting MkdirAll
// follow the link and write outside the sandbox.
func TestSymlinkEscapeViaNonexistentAncestorBlocked(t *testing.T) {
	reg, root := newReg(t)
	outsideDir := t.TempDir() // a directory OUTSIDE the root
	link := filepath.Join(root, "escape")
	if err := os.Symlink(outsideDir, link); err != nil {
		t.Skip("symlinks not supported here")
	}
	// escape -> outsideDir; newsub does not exist yet, file below it does not exist.
	_, err := runNamed(t, reg, "write_file", map[string]any{"path": "escape/newsub/pwned.txt", "content": "x"})
	if err == nil {
		t.Error("write through a symlinked ancestor to a non-existent path must be blocked")
	}
	if _, statErr := os.Stat(filepath.Join(outsideDir, "newsub", "pwned.txt")); statErr == nil {
		t.Fatal("SANDBOX ESCAPE: file was written outside the tool root")
	}
}
