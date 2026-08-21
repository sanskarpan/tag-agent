package cli_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runDoc runs the binary with a stubbed pdf-inspector engine so docs.Available()
// is true and Extract reaches its path checks. /bin/echo stands in for the
// engine: it exists (so the command is "available") and returns non-JSON (so a
// real file is a genuine engine failure, distinct from a bad-input path error).
func runDoc(t *testing.T, home string, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(tagBin, args...)
	cmd.Env = append(os.Environ(), "TAG_HOME="+home, "TAG_PDF_INSPECTOR=/bin/echo")
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	}
	return out.String(), errb.String(), code
}

// TestDocReadJSONErrorPath: `doc read --json` on an error must emit a parseable
// error object on stdout, like every sibling command (ISSUE-001). RED pre-fix,
// where the local --json flag shadowed the root's so stdout was empty.
func TestDocReadJSONErrorPath(t *testing.T) {
	h := newHome(t)
	stdout, _, code := runDoc(t, h, "--json", "doc", "read", "/does-not-exist.pdf")
	if code == 0 {
		t.Fatalf("a missing file must exit non-zero, got 0")
	}
	if !strings.HasPrefix(strings.TrimSpace(stdout), "{") || !strings.Contains(stdout, `"error"`) {
		t.Errorf("--json error path must print a JSON error object on stdout, got: %q", stdout)
	}
}

// TestDocReadExitCodes: a missing file and a directory are usage errors (exit 2),
// matching `doc read --help`; a genuine engine failure stays exit 1 (ISSUE-005).
func TestDocReadExitCodes(t *testing.T) {
	h := newHome(t)

	if _, _, code := runDoc(t, h, "doc", "read", "/does-not-exist.pdf"); code != 2 {
		t.Errorf("missing file should exit 2, got %d", code)
	}
	if _, _, code := runDoc(t, h, "doc", "read", t.TempDir()); code != 2 {
		t.Errorf("a directory should exit 2, got %d", code)
	}

	// A real file that the (stub) engine cannot parse is a run failure, not a
	// usage error — the classification must not over-reach.
	f := filepath.Join(t.TempDir(), "real.pdf")
	if err := os.WriteFile(f, []byte("%PDF-1.4 fake"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, code := runDoc(t, h, "doc", "read", f); code != 1 {
		t.Errorf("a genuine engine failure should exit 1, got %d", code)
	}
}
