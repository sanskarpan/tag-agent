// Package security implements secret scanning (Go port of security.py):
// named patterns + Shannon-entropy detection, binary-file skip, size cap.
package security

import (
	"bufio"
	"bytes"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const maxFileBytes = 10_000_000
const entropyWindow = 32
const entropyThreshold = 4.5

var skipExts = map[string]bool{".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".pdf": true, ".zip": true, ".tar": true, ".gz": true, ".pyc": true, ".so": true, ".dylib": true, ".dll": true, ".exe": true, ".woff": true, ".ttf": true, ".mp4": true, ".sqlite3": true, ".db": true}
var skipDirs = map[string]bool{".git": true, "node_modules": true, "vendor": true, "__pycache__": true, ".venv": true}

var patterns = []struct {
	name string
	re   *regexp.Regexp
}{
	// --- existing patterns (kept) ---
	{"aws_access_key", regexp.MustCompile(`AKIA[0-9A-Z]{16}`)},
	{"github_token", regexp.MustCompile(`ghp_[0-9A-Za-z]{36}`)},
	{"openai_key", regexp.MustCompile(`sk-(?:proj-)?[A-Za-z0-9_\-]{20,}`)},
	{"anthropic_key", regexp.MustCompile(`sk-ant-[A-Za-z0-9\-_]{20,}`)},
	{"private_key", regexp.MustCompile(`-----BEGIN (?:RSA |EC |DSA |OPENSSH )?PRIVATE KEY`)},
	{"generic_secret", regexp.MustCompile(`(?i)(secret|token|password|api_key)\s*[=:]\s*["']?[A-Za-z0-9/+_\-]{16,}`)},
	// --- ported from security.py ---
	{"openai_org", regexp.MustCompile(`org-[A-Za-z0-9]{20,}`)},
	{"aws_secret_key", regexp.MustCompile(`(?i)aws.{0,20}(?:secret|key).{0,20}["']?[A-Za-z0-9/+]{20,}`)},
	{"github_oauth", regexp.MustCompile(`gh[opusr]_[0-9A-Za-z]{36}`)},
	{"github_pat_fine", regexp.MustCompile(`github_pat_[0-9A-Za-z_]{59,}`)},
	{"npm_access_token", regexp.MustCompile(`npm_[0-9A-Za-z]{36}`)},
	{"stripe_secret", regexp.MustCompile(`sk_live_[0-9a-zA-Z]{24,}`)},
	{"stripe_restricted", regexp.MustCompile(`rk_live_[0-9a-zA-Z]{24,}`)},
	{"twilio_account_sid", regexp.MustCompile(`AC[0-9a-f]{32}`)},
	{"twilio_auth_token", regexp.MustCompile(`SK[0-9a-f]{32}`)},
	{"google_api_key", regexp.MustCompile(`AIza[0-9A-Za-z_\-]{35}`)},
	{"slack_token", regexp.MustCompile(`xox[baprs]-[0-9A-Za-z\-]{10,}`)},
	{"heroku_api_key", regexp.MustCompile(`(?i)heroku.{0,20}[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)},
	{"jwt_token", regexp.MustCompile(`eyJ[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}`)},
}

// Finding is one secret hit.
type Finding struct {
	File    string `json:"file"`
	LineNo  int    `json:"line_no"`
	Pattern string `json:"pattern"`
	Entropy bool   `json:"entropy"`
}

func shannon(s string) float64 {
	if s == "" {
		return 0
	}
	freq := map[rune]int{}
	for _, r := range s {
		freq[r]++
	}
	var h float64
	n := float64(len(s))
	for _, c := range freq {
		p := float64(c) / n
		h -= p * math.Log2(p)
	}
	return h
}

func highEntropy(line string) bool {
	line = strings.TrimSpace(line)
	if len(line) < entropyWindow {
		return false
	}
	for i := 0; i+entropyWindow <= len(line); i++ {
		w := line[i : i+entropyWindow]
		if shannon(w) >= entropyThreshold {
			return true
		}
	}
	return false
}

// ScanFile scans one file, skipping binary/oversized/unsupported files.
//
// The path is an explicit user target, so a symlink is resolved and its real
// target scanned. Symlinks encountered during directory walks go through
// ScanDir, which only follows them when the real target resolves inside the
// scanned root.
func ScanFile(path string) []Finding {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	return scanFile(path, "")
}

// scanFile is the root-aware implementation. When root is non-empty, a symlink
// is scanned only if it resolves inside root; when root is empty, symlinks are
// skipped entirely (prevents following links out of the scanned tree).
func scanFile(path, root string) []Finding {
	if skipExts[strings.ToLower(filepath.Ext(path))] {
		return nil
	}
	if li, err := os.Lstat(path); err == nil && li.Mode()&os.ModeSymlink != 0 {
		if !symlinkInsideRoot(path, root) {
			return nil
		}
	}
	st, err := os.Stat(path)
	if err != nil || st.Size() > maxFileBytes {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	if bytes.IndexByte(b[:min(len(b), 8192)], 0) >= 0 {
		return nil // binary
	}
	var out []Finding
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64*1024), maxFileBytes+1)
	line := 0
	for sc.Scan() {
		line++
		text := sc.Text()
		matched := false
		for _, p := range patterns {
			if p.re.MatchString(text) {
				out = append(out, Finding{File: path, LineNo: line, Pattern: p.name})
				matched = true
				break
			}
		}
		if !matched && highEntropy(text) {
			out = append(out, Finding{File: path, LineNo: line, Pattern: "high_entropy", Entropy: true})
		}
	}
	return out
}

// symlinkInsideRoot reports whether the symlink at path resolves to a real
// target inside root. An empty root, or a target that cannot be resolved or
// escapes root, returns false so the entry is skipped.
func symlinkInsideRoot(path, root string) bool {
	if root == "" {
		return false
	}
	realTarget, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(realRoot, realTarget)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

// ScanDir walks root, scanning files (skip dirs), capped at maxFiles.
// Unreadable entries (e.g. permission-denied subtrees) are surfaced as
// "walk_error" findings rather than silently skipped.
func ScanDir(root string, maxFiles int) []Finding {
	var out []Finding
	count := 0
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			out = append(out, Finding{File: p, Pattern: "walk_error"})
			return nil
		}
		if count >= maxFiles {
			return filepath.SkipAll
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		out = append(out, scanFile(p, root)...)
		count++
		return nil
	})
	return out
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
