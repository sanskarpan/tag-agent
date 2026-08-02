package ciauto

import (
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// FlakyTest is one test that both passed and failed in the same log corpus.
//
// Field names mirror src/tag/ci.py:detect_flaky_tests (test_name/pass_count/
// fail_count/flakiness_score) and add fail_rate + quarantine, which PRD-063
// specifies but the Python implementation never produced.
type FlakyTest struct {
	TestName       string  `json:"test_name"`
	PassCount      int     `json:"pass_count"`
	FailCount      int     `json:"fail_count"`
	FlakinessScore float64 `json:"flakiness_score"`
	FailRate       float64 `json:"fail_rate"`
	Quarantine     bool    `json:"quarantine"`
}

// DefaultQuarantineThreshold is PRD-063's default: a test failing at least half
// the time is emitted with quarantine=true so a CI matrix can exclude it.
const DefaultQuarantineThreshold = 0.5

var (
	// pytest prints the outcome first in its short summary (`FAILED a.py::b`) and
	// LAST under -v (`a.py::b FAILED`). Only the first shape was matched, so a
	// verbose log parsed to zero outcomes and flaky-fix reported a clean suite —
	// exit 0 — on a log full of failures. Both shapes are now recognised.
	//
	// The character classes are explicit Unicode (\p{L}\p{N}) rather than \w:
	// Go's \w is ASCII-only where Python's re.\w is Unicode-aware, so non-ASCII
	// test names silently vanished here while Python found them.
	pytestName = `[\p{L}\p{N}_/.\-:]+::[\p{L}\p{N}_\[\]\-.]+`
	// [ \t]+ rather than \s+: with the -v form now also parsed, a newline-crossing
	// \s+ would pair one line's trailing PASSED with the NEXT line's test name.
	rePytest = regexp.MustCompile(`(PASSED|FAILED)[ \t]+(` + pytestName + `)`)
	// Line-anchored so the trailing form only matches a real `-v` result line.
	rePytestSuffix = regexp.MustCompile(`(?m)^\s*(` + pytestName + `)\s+(PASSED|FAILED)\b`)
	reJest         = regexp.MustCompile(`(?m)^\s*([\x{2713}\x{2717}\x{00D7}])\s+(.+)$`)
	reJestMs       = regexp.MustCompile(`\s+\(\d+\s*ms\)$`)
	reGo           = regexp.MustCompile(`--- (PASS|FAIL): (\S+)`)
	reRust         = regexp.MustCompile(`test (\S+) \.\.\. (ok|FAILED)`)
)

// DetectFlaky parses a concatenated multi-run test log and returns the tests
// that both passed AND failed. Supported formats: pytest, Jest, `go test -v`,
// and `cargo test`.
//
// quarantineThreshold is the fail-rate at or above which a test is flagged for
// quarantine; pass <= 0 for the default.
func DetectFlaky(log string, quarantineThreshold float64) []FlakyTest {
	if quarantineThreshold <= 0 {
		quarantineThreshold = DefaultQuarantineThreshold
	}
	pass, fail := parseOutcomes(log)

	names := make([]string, 0, len(pass)+len(fail))
	seen := map[string]bool{}
	for n := range pass {
		if !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	for n := range fail {
		if !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	sort.Strings(names)

	out := []FlakyTest{}
	for _, n := range names {
		pc, fc := pass[n], fail[n]
		if pc == 0 || fc == 0 {
			continue // deterministically green or deterministically red: not flaky
		}
		total := pc + fc
		// Same formula as Python: 1 - |pass-fail|/total, so a 50/50 split scores 1.0.
		score := round4(1.0 - math.Abs(float64(pc-fc))/float64(total))
		failRate := round4(float64(fc) / float64(total))
		out = append(out, FlakyTest{
			TestName:       n,
			PassCount:      pc,
			FailCount:      fc,
			FlakinessScore: score,
			FailRate:       failRate,
			Quarantine:     failRate >= quarantineThreshold,
		})
	}
	// Most flaky first; ties broken by name so the ordering is deterministic
	// (Python's sort left ties in Python-set order, i.e. run-to-run random).
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].FlakinessScore != out[j].FlakinessScore {
			return out[i].FlakinessScore > out[j].FlakinessScore
		}
		return out[i].TestName < out[j].TestName
	})
	return out
}

func round4(f float64) float64 { return math.Round(f*10000) / 10000 }

// ReadTestLog reads a test log, rejecting an empty file.
func ReadTestLog(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("test log not found: %s: %w", path, os.ErrNotExist)
		}
		return "", err
	}
	if strings.TrimSpace(string(b)) == "" {
		return "", fmt.Errorf("%w: %s contains no text", ErrEmptyLog, path)
	}
	return string(b), nil
}

// maxTestFileScan bounds the repo walk that resolves a bare test name to a file,
// so a flaky-fix over a huge monorepo cannot hang unboundedly.
const maxTestFileScan = 20000

var reGoTestName = regexp.MustCompile(`^[A-Z][A-Za-z0-9_]*$`)

// ResolveTestFile makes a best-effort mapping from a test name to a source file
// under root. Returns "" when it cannot be resolved — callers must report that
// honestly rather than skipping the test silently.
func ResolveTestFile(root, testName string) string {
	if root == "" {
		root = "."
	}
	// pytest / Rust style "path::name".
	if i := strings.Index(testName, "::"); i >= 0 {
		cand := filepath.Join(root, filepath.FromSlash(testName[:i]))
		if st, err := os.Stat(cand); err == nil && !st.IsDir() {
			return cand
		}
	}
	// Go: a bare exported identifier -> the *_test.go declaring it.
	if reGoTestName.MatchString(testName) {
		if p := scanFor(root, []string{"*_test.go"}, "func "+testName+"("); p != "" {
			return p
		}
	}
	// Rust: trailing segment is the fn name.
	if i := strings.LastIndex(testName, "::"); i >= 0 {
		fn := testName[i+2:]
		if p := scanFor(root, []string{"*.rs"}, "fn "+fn+"("); p != "" {
			return p
		}
	}
	// Jest / Mocha: the description string appears verbatim in the spec file.
	if p := scanFor(root, []string{"*.test.js", "*.test.ts", "*.test.jsx", "*.test.tsx",
		"*.spec.js", "*.spec.ts"}, testName); p != "" {
		return p
	}
	return ""
}

// scanFor walks root looking for the first file matching any of globs whose
// contents contain needle. Skips VCS/vendor dirs and bounds the walk.
func scanFor(root string, globs []string, needle string) string {
	var found string
	scanned := 0
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable entry: skip, never abort the whole scan
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor", "target", ".venv", "__pycache__", "dist", "build":
				return filepath.SkipDir
			}
			return nil
		}
		if scanned++; scanned > maxTestFileScan {
			return filepath.SkipAll
		}
		match := false
		for _, g := range globs {
			if ok, _ := filepath.Match(g, d.Name()); ok {
				match = true
				break
			}
		}
		if !match {
			return nil
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return nil
		}
		if strings.Contains(string(b), needle) {
			found = p
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

// FlakySystemPrompt is the system message for flaky-fix.
const FlakySystemPrompt = "You are an expert engineer eliminating a flaky (intermittently failing) test. " +
	"Identify the source of non-determinism and rewrite the test so it passes deterministically."

// BuildFlakyFixPrompt renders the per-test remediation prompt. Mirrors
// fix_flaky_test's prompt (same 7000-char file cap, same EXPLANATION: contract).
func BuildFlakyFixPrompt(f FlakyTest, file, content string) string {
	if len(content) > 7000 {
		content = content[:7000]
	}
	return fmt.Sprintf(`## Flaky Test

- Test name: %s
- File: %s
- Observed: %d pass / %d fail (fail rate %.2f) across the supplied log

## Test File Content

`+"```"+`
%s
`+"```"+`

## Task

1. Identify the root cause of the flakiness (race condition, time dependency,
   random seed, shared mutable state, network call, ordering, ...).
2. Rewrite the test so it passes deterministically every time.
3. Output the COMPLETE corrected test file content followed by a final line
   starting with "EXPLANATION:" and a one-sentence explanation.

Output format (no markdown fences):
<corrected file content>
EXPLANATION: <one-sentence explanation>
`, f.TestName, file, f.PassCount, f.FailCount, f.FailRate, content)
}

// SplitFlakyFix separates the corrected file body from the trailing
// "EXPLANATION:" line. Returns ok=false when the model did not honour the
// contract, so the caller can refuse to write a file it cannot parse rather
// than clobbering the test with prose.
func SplitFlakyFix(raw string) (code, explanation string, ok bool) {
	raw = strings.TrimSpace(raw)
	i := strings.LastIndex(raw, "EXPLANATION:")
	if i < 0 {
		return raw, "", false
	}
	return strings.TrimSpace(raw[:i]), strings.TrimSpace(raw[i+len("EXPLANATION:"):]), true
}

// parseOutcomes extracts per-test pass/fail counts from a concatenated log in
// any supported format.
func parseOutcomes(log string) (pass, fail map[string]int) {
	pass = map[string]int{}
	fail = map[string]int{}
	for _, m := range rePytest.FindAllStringSubmatch(log, -1) {
		if m[1] == "PASSED" {
			pass[m[2]]++
		} else {
			fail[m[2]]++
		}
	}
	for _, m := range rePytestSuffix.FindAllStringSubmatch(log, -1) {
		if m[2] == "PASSED" {
			pass[m[1]]++
		} else {
			fail[m[1]]++
		}
	}
	for _, m := range reGo.FindAllStringSubmatch(log, -1) {
		if m[1] == "PASS" {
			pass[m[2]]++
		} else {
			fail[m[2]]++
		}
	}
	for _, m := range reRust.FindAllStringSubmatch(log, -1) {
		if m[2] == "ok" {
			pass[m[1]]++
		} else {
			fail[m[1]]++
		}
	}
	for _, m := range reJest.FindAllStringSubmatch(log, -1) {
		name := strings.TrimSpace(reJestMs.ReplaceAllString(strings.TrimSpace(m[2]), ""))
		if name == "" {
			continue
		}
		if m[1] == "✓" {
			pass[name]++
		} else {
			fail[name]++
		}
	}

	return pass, fail
}

// OutcomeCount reports how many test outcomes were recognised in log, in any
// supported format. Zero means the log's format was not understood — which is a
// different answer from "the suite is stable", and callers must say so rather
// than reporting a clean bill of health.
func OutcomeCount(log string) int {
	pass, fail := parseOutcomes(log)
	n := 0
	for _, c := range pass {
		n += c
	}
	for _, c := range fail {
		n += c
	}
	return n
}
