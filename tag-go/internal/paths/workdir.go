package paths

import (
	"os"
	"path/filepath"
	"strings"
)

// DefaultWorkRootName is the directory under TAG_HOME that holds per-unit-of-work
// working directories when a caller does not configure its own work root.
const DefaultWorkRootName = "work"

// defaultSegment is the last-resort name for a path segment whose identifier
// sanitises away to nothing. Callers that want a domain-specific fallback
// ("job", "case", …) pre-sanitise with SafeSegment before calling WorkDir.
const defaultSegment = "work"

// SafeSegment reduces an identifier to a single path element so a hostile id
// (a traversal, an absolute path, an embedded separator) can never steer a
// working directory outside its work root. It is idempotent: a segment that is
// already safe is returned unchanged.
//
// Both the OS separator and "/" are folded to "_" so the confinement property
// holds on Windows too, where "/" is not os.PathSeparator but is still accepted
// by the path APIs.
func SafeSegment(id, fallback string) string {
	s := strings.ReplaceAll(id, string(os.PathSeparator), "_")
	s = strings.ReplaceAll(s, "/", "_")
	seg := filepath.Base(filepath.Clean("/" + s))
	if seg == "." || seg == ".." || seg == string(os.PathSeparator) || seg == "" {
		if fallback == "" {
			return defaultSegment
		}
		return fallback
	}
	return seg
}

// WorkDir creates the private working directory for one unit of work and returns
// it together with a cleanup func.
//
// This is the single implementation of the #591 contract, which was previously
// duplicated across the worker, swarm, eval, ciauto and loop packages:
//
//   - the unit of work starts in an EMPTY scratch dir, not a copy or checkout of
//     the repo. Copying a tree would be surprisingly expensive and would silently
//     diverge from disk; a checkout is git-worktree isolation, specified
//     separately as PRD-130 and deliberately out of scope here.
//   - the dir is <workRoot>/<segments...>, i.e. stable and derivable from the
//     identifiers, so an operator can find artifacts after the fact.
//   - every segment is sanitised to a single path element, so no identifier can
//     escape workRoot. Sanitisation happens here and cannot be bypassed by a
//     caller, even one that passes raw ids straight through.
//   - cleanup removes the dir ONLY if the unit of work left it empty. Work that
//     produced files keeps them; work that produced nothing leaves no litter.
//
// An empty workRoot defaults to <TAG_HOME>/work. Callers with their own default
// root name (e.g. eval's "eval-work") resolve it before calling.
func WorkDir(workRoot string, segments ...string) (string, func(), error) {
	if workRoot == "" {
		workRoot = filepath.Join(Home(), DefaultWorkRootName)
	}
	parts := make([]string, 0, len(segments)+1)
	parts = append(parts, workRoot)
	for _, s := range segments {
		parts = append(parts, SafeSegment(s, defaultSegment))
	}
	if len(segments) == 0 {
		// Never hand back the work root itself: cleanup would then be able to
		// remove the shared root rather than one unit of work's scratch dir.
		parts = append(parts, defaultSegment)
	}
	dir := filepath.Join(parts...)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", func() {}, err
	}
	return dir, func() { RemoveIfEmpty(dir) }, nil
}

// RemoveIfEmpty removes dir if and only if it holds no entries. It is the
// building block of WorkDir's cleanup and is exported for callers that also
// prune an intermediate parent (eval prunes its per-run dir).
func RemoveIfEmpty(dir string) {
	if entries, err := os.ReadDir(dir); err == nil && len(entries) == 0 {
		_ = os.Remove(dir)
	}
}
