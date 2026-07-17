// Package diffcontext builds a git-diff context block for agent injection
// (port of src/tag/diff_context.py). Secret/binary files are filtered out and
// the total is token-estimated. Shells out to the local `git` binary.
package diffcontext

import (
	"os/exec"
	"path/filepath"
	"strings"
)

// DefaultBlockedPatterns are always excluded (secrets/credentials).
var DefaultBlockedPatterns = []string{
	".env", "*.env", ".env.*",
	"*.key", "*.pem", "*.p12", "*.pfx",
	"*secret*", "*credential*", "*password*",
	"*.token",
}

var binaryExts = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".ico": true, ".svg": true, ".webp": true,
	".zip": true, ".tar": true, ".gz": true, ".bz2": true, ".xz": true, ".7z": true, ".rar": true,
	".exe": true, ".dll": true, ".so": true, ".dylib": true, ".pyc": true,
	".pdf": true, ".docx": true, ".xlsx": true, ".pptx": true,
	".mp4": true, ".mov": true, ".mp3": true, ".wav": true,
	".ttf": true, ".woff": true, ".woff2": true, ".eot": true,
}

// WarnTokenThreshold triggers the "large diff" warning.
const WarnTokenThreshold = 10000

// Result is the assembled diff context.
type Result struct {
	Content         string   `json:"content"`
	FilesIncluded   []string `json:"files_included"`
	FilesSkipped    []string `json:"files_skipped"`
	EstimatedTokens int      `json:"estimated_tokens"`
	Warn            bool     `json:"warn"`
}

func isBlocked(filename string, patterns []string) bool {
	name := filepath.Base(filename)
	for _, pat := range patterns {
		if ok, _ := filepath.Match(pat, name); ok {
			return true
		}
		if ok, _ := filepath.Match(pat, filename); ok {
			return true
		}
	}
	return false
}

func isBinary(filename string) bool {
	return binaryExts[strings.ToLower(filepath.Ext(filename))]
}

func estimateTokens(text string) int {
	t := len(text) / 4
	if t < 1 {
		return 1
	}
	return t
}

// GitError is returned for git failures with a concise message.
type GitError struct{ Msg string }

func (e *GitError) Error() string { return e.Msg }

func validateRef(ref string) error {
	if strings.HasPrefix(ref, "-") {
		return &GitError{Msg: "invalid git ref: " + ref}
	}
	return nil
}

func changedFiles(ref string, staged bool, workdir string) ([]string, error) {
	args := []string{"diff", "--name-only"}
	if staged {
		args = append(args, "--cached")
	} else {
		if err := validateRef(ref); err != nil {
			return nil, err
		}
		args = append(args, ref)
	}
	cmd := exec.Command("git", args...)
	cmd.Dir = workdir
	out, err := cmd.CombinedOutput()
	if err != nil {
		if _, ok := err.(*exec.Error); ok {
			return nil, &GitError{Msg: "git not found in PATH"}
		}
		low := strings.ToLower(string(out))
		if strings.Contains(low, "not a git repository") || strings.Contains(low, "usage:") || strings.TrimSpace(string(out)) == "" {
			return nil, &GitError{Msg: "not a git repository (or no commits to diff against)"}
		}
		first := strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0]
		return nil, &GitError{Msg: "git diff failed: " + first}
	}
	var files []string
	for _, f := range strings.Split(string(out), "\n") {
		if s := strings.TrimSpace(f); s != "" {
			files = append(files, s)
		}
	}
	return files, nil
}

func fileDiff(filename, ref string, contextLines int, staged bool, workdir string) string {
	args := []string{"diff", "-U" + itoa(contextLines)}
	if staged {
		args = append(args, "--cached")
	} else {
		if validateRef(ref) != nil {
			return ""
		}
		args = append(args, ref)
	}
	args = append(args, "--", filename)
	cmd := exec.Command("git", args...)
	cmd.Dir = workdir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return string(out)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

// Build assembles the diff context per the ref/staged mode, filtering blocked
// and binary files and capping at maxFiles.
func Build(ref string, staged bool, contextLines, maxFiles int, extraBlocked []string, workdir string) (*Result, error) {
	patterns := append(append([]string{}, extraBlocked...), DefaultBlockedPatterns...)
	all, err := changedFiles(ref, staged, workdir)
	if err != nil {
		return nil, err
	}
	var included, skipped []string
	for _, f := range all {
		if isBlocked(f, patterns) || isBinary(f) || len(included) >= maxFiles {
			skipped = append(skipped, f)
			continue
		}
		included = append(included, f)
	}
	var parts []string
	for _, f := range included {
		d := fileDiff(f, ref, contextLines, staged, workdir)
		if strings.TrimSpace(d) != "" {
			parts = append(parts, "### "+f+"\n```diff\n"+strings.TrimRight(d, "\n")+"\n```")
		}
	}
	content := strings.Join(parts, "\n\n")
	tokens := estimateTokens(content)
	return &Result{
		Content: content, FilesIncluded: included, FilesSkipped: skipped,
		EstimatedTokens: tokens, Warn: tokens > WarnTokenThreshold,
	}, nil
}
