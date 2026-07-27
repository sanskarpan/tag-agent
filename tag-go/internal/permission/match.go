package permission

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// MatchPath reports whether an absolute, cleaned filesystem path matches a glob.
//
// Dialect:
//   - "*"  matches any run of characters except "/"
//   - "**" matches any run of characters INCLUDING "/"
//   - "?"  matches one character except "/"
//   - a leading "~" expands to the user's home directory
//   - a pattern with NO "/" is matched against the path's BASENAME, so "*.env"
//     protects .env anywhere in the tree; a pattern containing "/" is matched
//     against the whole path.
//
// The subject is expected to already be absolute and lexically cleaned by the
// caller (tool.PermissionSubject does this), so "../../.ssh/id_rsa" is matched
// as the real target path and cannot slip past a rule via traversal syntax.
func MatchPath(pattern, path string) bool {
	if pattern == "" || path == "" {
		return false
	}
	pattern = expandHome(pattern)
	if !strings.ContainsRune(pattern, '/') {
		// basename-only pattern
		return globRe(pattern).MatchString(filepath.Base(path))
	}
	if !filepath.IsAbs(pattern) {
		// A relative multi-segment pattern (e.g. "config/*.key") matches any
		// suffix of the path at a directory boundary.
		return globRe("**/" + strings.TrimPrefix(pattern, "./")).MatchString(path)
	}
	return globRe(pattern).MatchString(path)
}

// MatchCommand reports whether a shell command line matches a command pattern.
//
// Dialect (deliberately narrow — shell is not a path):
//   - a pattern ending in "*" is a PREFIX match on the command
//     ("git *" and "git*" both match "git status")
//   - a pattern with no wildcard matches either the whole command exactly or
//     its first token exactly ("git" matches "git status" but not "github-cli x")
//
// Matching is done on the command with surrounding whitespace trimmed.
// Note the honest limitation: this is textual, not semantic. A shell can defeat
// a prefix allow-rule ("git status; rm -rf /"), so a `bash` allow-rule with a
// pattern is a convenience, not a sandbox. See the package docs and the report.
func MatchCommand(pattern, command string) bool {
	pattern = strings.TrimSpace(pattern)
	command = strings.TrimSpace(command)
	if pattern == "" || command == "" {
		return false
	}
	if pattern == "*" || pattern == "**" {
		return true
	}
	if strings.HasSuffix(pattern, "*") {
		// NOTE: the prefix keeps its trailing whitespace on purpose, so
		// "git *" matches "git status" but NOT "gitleaks scan". Trimming it here
		// would silently widen every allow-rule to a token-prefix match.
		prefix := strings.TrimSuffix(pattern, "*")
		if prefix == "" {
			return true
		}
		return strings.HasPrefix(command, prefix)
	}
	if pattern == command {
		return true
	}
	return firstToken(command) == pattern
}

func firstToken(cmd string) string {
	for i, r := range cmd {
		if r == ' ' || r == '\t' || r == '\n' {
			return cmd[:i]
		}
	}
	return cmd
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return p
		}
		if p == "~" {
			return home
		}
		return filepath.Join(home, p[2:])
	}
	return p
}

var (
	reCacheMu sync.Mutex
	reCache   = map[string]*regexp.Regexp{}
)

// globRe compiles a glob into an anchored regexp (cached).
func globRe(pattern string) *regexp.Regexp {
	reCacheMu.Lock()
	if re, ok := reCache[pattern]; ok {
		reCacheMu.Unlock()
		return re
	}
	reCacheMu.Unlock()

	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		switch c {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				// "**/" collapses so that "**/x" also matches a bare "x"
				if i+2 < len(pattern) && pattern[i+2] == '/' {
					b.WriteString("(?:.*/)?")
					i += 2
				} else {
					b.WriteString(".*")
					i++
				}
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	if err != nil {
		// Unreachable for the grammar above, but fail CLOSED for path rules: a
		// pattern we cannot compile matches nothing rather than everything.
		re = regexp.MustCompile(`$^`)
	}
	reCacheMu.Lock()
	reCache[pattern] = re
	reCacheMu.Unlock()
	return re
}
