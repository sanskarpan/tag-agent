package paths

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// within reports whether dir is strictly inside root.
func within(root, dir string) bool {
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != "."
}

// TestWorkDirConfinesHostileSegments is the security property #591's fix rests
// on: no identifier, however hostile, may place the working directory outside
// the work root.
func TestWorkDirConfinesHostileSegments(t *testing.T) {
	root := t.TempDir()
	cases := []struct {
		name     string
		segments []string
	}{
		{"parent traversal", []string{".."}},
		{"deep traversal", []string{"../../etc"}},
		{"traversal with tail", []string{"../../etc/pwn"}},
		{"traversal in every segment", []string{"../../etc", "../../pwn", ".."}},
		{"absolute path", []string{"/etc/passwd"}},
		{"absolute root", []string{"/"}},
		{"dot", []string{"."}},
		{"dotdot embedded", []string{"a/../../b"}},
		{"empty id", []string{""}},
		{"only separators", []string{"///"}},
		{"whitespace id", []string{"   "}},
		{"newline id", []string{"a\nb"}},
		{"no segments at all", nil},
		{"nested empties", []string{"", "", ""}},
		{"mixed", []string{"run-1", "../../escape"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir, cleanup, err := WorkDir(root, tc.segments...)
			if err != nil {
				t.Fatalf("WorkDir(%q): %v", tc.segments, err)
			}
			defer cleanup()
			if !within(root, dir) {
				t.Fatalf("segments %q escaped the work root: dir=%q root=%q", tc.segments, dir, root)
			}
			if st, err := os.Stat(dir); err != nil || !st.IsDir() {
				t.Fatalf("work dir %q was not created: %v", dir, err)
			}
			if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
				t.Fatalf("work dir %q must start empty, got %d entries (err %v)", dir, len(entries), err)
			}
		})
	}
}

// A hostile id must never yield the work root itself, because cleanup would
// then be able to remove the shared root.
func TestWorkDirNeverReturnsTheWorkRootItself(t *testing.T) {
	root := t.TempDir()
	for _, segs := range [][]string{nil, {}, {""}, {"."}, {".."}, {"/"}} {
		dir, cleanup, err := WorkDir(root, segs...)
		if err != nil {
			t.Fatalf("WorkDir(%q): %v", segs, err)
		}
		if filepath.Clean(dir) == filepath.Clean(root) {
			t.Fatalf("segments %q returned the work root itself", segs)
		}
		cleanup()
		if _, err := os.Stat(root); err != nil {
			t.Fatalf("cleanup removed the work root for segments %q: %v", segs, err)
		}
	}
}

// Distinct identifiers must get distinct directories — the actual #591 bug was
// concurrent units of work sharing one cwd.
func TestWorkDirIsPerIdentifier(t *testing.T) {
	root := t.TempDir()
	a, _, err := WorkDir(root, "a")
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := WorkDir(root, "b")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatalf("two identifiers shared a directory: %q", a)
	}
	// Nesting is honoured, and repeated calls with the same segments are stable.
	n1, _, err := WorkDir(root, "swarm", "s1", "t1")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(root, "swarm", "s1", "t1"); n1 != want {
		t.Fatalf("nested layout = %q, want %q", n1, want)
	}
	n2, _, err := WorkDir(root, "swarm", "s1", "t1")
	if err != nil {
		t.Fatal(err)
	}
	if n1 != n2 {
		t.Fatalf("same identifiers gave unstable dirs %q vs %q", n1, n2)
	}
}

// Cleanup must remove an untouched dir and must NEVER destroy artifacts.
func TestWorkDirCleanupRemovesOnlyEmptyDirs(t *testing.T) {
	root := t.TempDir()

	empty, cleanEmpty, err := WorkDir(root, "empty")
	if err != nil {
		t.Fatal(err)
	}
	cleanEmpty()
	if _, err := os.Stat(empty); !os.IsNotExist(err) {
		t.Errorf("empty work dir %q should have been removed", empty)
	}

	withFile, cleanFile, err := WorkDir(root, "with-file")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(withFile, "out.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cleanFile()
	if _, err := os.Stat(filepath.Join(withFile, "out.txt")); err != nil {
		t.Errorf("cleanup destroyed an artifact: %v", err)
	}

	// A subdirectory counts as content too.
	withDir, cleanDir, err := WorkDir(root, "with-dir")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(withDir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	cleanDir()
	if _, err := os.Stat(withDir); err != nil {
		t.Errorf("work dir holding a subdirectory must be kept: %v", err)
	}

	// Cleanup is idempotent and safe to call twice.
	again, cleanAgain, err := WorkDir(root, "twice")
	if err != nil {
		t.Fatal(err)
	}
	cleanAgain()
	cleanAgain()
	if _, err := os.Stat(again); !os.IsNotExist(err) {
		t.Errorf("dir %q should stay removed", again)
	}
}

// Cleanup of a nested dir must not touch the intermediate parent (only eval
// prunes its run dir, explicitly, at its own call site).
func TestWorkDirCleanupLeavesParentAlone(t *testing.T) {
	root := t.TempDir()
	dir, cleanup, err := WorkDir(root, "parent", "child")
	if err != nil {
		t.Fatal(err)
	}
	cleanup()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("empty child dir should be removed")
	}
	if _, err := os.Stat(filepath.Join(root, "parent")); err != nil {
		t.Errorf("intermediate parent must be left in place: %v", err)
	}
}

// An empty work root defaults under TAG_HOME.
func TestWorkDirDefaultsUnderTagHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("TAG_HOME", home)
	dir, cleanup, err := WorkDir("", "job-1")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	want := filepath.Join(home, DefaultWorkRootName, "job-1")
	if dir != want {
		t.Fatalf("default work dir = %q, want %q", dir, want)
	}
}

func TestSafeSegment(t *testing.T) {
	// Separators are folded to "_" BEFORE the Clean/Base pass, which is what the
	// five original copies did: a traversal is flattened into one inert segment
	// rather than resolved (so "../../etc" becomes ".._.._etc", never "etc").
	cases := []struct{ in, fallback, want string }{
		{"job-a", "job", "job-a"},
		{"..", "job", "job"},
		{"../../etc", "job", ".._.._etc"},
		{"/etc/passwd", "job", "_etc_passwd"},
		{"a/b", "job", "a_b"},
		{"", "case", "case"},
		{".", "run", "run"},
		{"/", "task", "_"},
		{"", "", "work"},
		{"ok", "", "ok"},
	}
	for _, tc := range cases {
		if got := SafeSegment(tc.in, tc.fallback); got != tc.want {
			t.Errorf("SafeSegment(%q, %q) = %q, want %q", tc.in, tc.fallback, got, tc.want)
		}
	}
}

// SafeSegment must be idempotent: callers pre-sanitise to choose a fallback
// name, and WorkDir sanitises again so it cannot be bypassed.
func TestSafeSegmentIsIdempotent(t *testing.T) {
	for _, in := range []string{"job-a", "..", "../../etc", "/etc/passwd", "a/b", "", ".", "loop-x"} {
		once := SafeSegment(in, "fb")
		if twice := SafeSegment(once, "fb"); twice != once {
			t.Errorf("SafeSegment not idempotent for %q: %q then %q", in, once, twice)
		}
	}
}

func TestRemoveIfEmpty(t *testing.T) {
	root := t.TempDir()
	empty := filepath.Join(root, "empty")
	full := filepath.Join(root, "full")
	for _, d := range []string{empty, full} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(full, "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	RemoveIfEmpty(empty)
	RemoveIfEmpty(full)
	RemoveIfEmpty(filepath.Join(root, "does-not-exist")) // must not panic
	if _, err := os.Stat(empty); !os.IsNotExist(err) {
		t.Error("empty dir should be removed")
	}
	if _, err := os.Stat(full); err != nil {
		t.Errorf("non-empty dir must be kept: %v", err)
	}
}
