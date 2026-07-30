package ciauto

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// ErrEmptyLog marks an input log that carries no usable text. Callers map it to
// a usage error (exit 2).
var ErrEmptyLog = errors.New("empty CI log")

// Failure is the structured signal extracted from a CI log. Field names mirror
// src/tag/ci.py:parse_ci_failure.
type Failure struct {
	ErrorType    string `json:"error_type,omitempty"`
	ErrorMessage string `json:"error_message,omitempty"`
	FilePath     string `json:"file_path,omitempty"`
	LineNumber   int    `json:"line_number,omitempty"`
	TestName     string `json:"test_name,omitempty"`
}

// Empty reports whether nothing at all was recognised in the log. A diagnose
// run over such a log is reported as DEGRADED rather than as a confident
// diagnosis of nothing.
func (f Failure) Empty() bool {
	return f.ErrorType == "" && f.ErrorMessage == "" && f.FilePath == "" && f.LineNumber == 0 && f.TestName == ""
}

// ciFailurePattern is one extraction rule. Ordered exactly as Python's
// _CI_FAILURE_PATTERNS, and like Python the LAST match in the log wins (it is
// closest to the failure) and an already-populated field is never overwritten.
type ciFailurePattern struct {
	re     *regexp.Regexp
	fields []string // named groups to lift, in order
}

var ciFailurePatterns = []ciFailurePattern{
	// Python traceback.
	{regexp.MustCompile(`(?m)File "(?P<file_path>[^"]+)", line (?P<line_number>\d+)`), []string{"file_path", "line_number"}},
	// pytest FAILED line.
	{regexp.MustCompile(`(?m)^.*FAILED (?P<test_name>\S+) - (?P<error_message>.+)$`), []string{"test_name", "error_message"}},
	// pytest ERROR line.
	{regexp.MustCompile(`(?m)ERROR (?P<test_name>\S+)`), []string{"test_name"}},
	// Standard exception line.
	{regexp.MustCompile(`(?m)^.*?(?P<error_type>\w*(?:Error|Exception)): (?P<error_message>.+)$`), []string{"error_type", "error_message"}},
	// Jest failure bullet.
	{regexp.MustCompile(`(?m)^\s*●\s*(?P<test_name>.+)$`), []string{"test_name"}},
	// Go test failure.
	{regexp.MustCompile(`(?m)--- FAIL: (?P<test_name>\S+)`), []string{"test_name"}},
	// Rust test failure.
	{regexp.MustCompile(`(?m)test (?P<test_name>\S+) \.\.\. FAILED`), []string{"test_name"}},
}

// ParseCIFailure extracts structured failure data from a raw CI log.
func ParseCIFailure(log string) Failure {
	var f Failure
	set := func(field, val string) {
		val = strings.TrimSpace(val)
		if val == "" {
			return
		}
		switch field {
		case "file_path":
			if f.FilePath == "" {
				f.FilePath = val
			}
		case "line_number":
			if f.LineNumber == 0 {
				n, err := strconv.Atoi(val)
				if err == nil {
					f.LineNumber = n
				}
			}
		case "test_name":
			if f.TestName == "" {
				f.TestName = val
			}
		case "error_type":
			if f.ErrorType == "" {
				f.ErrorType = val
			}
		case "error_message":
			if f.ErrorMessage == "" {
				f.ErrorMessage = val
			}
		}
	}
	for _, p := range ciFailurePatterns {
		all := p.re.FindAllStringSubmatch(log, -1)
		if len(all) == 0 {
			continue
		}
		m := all[len(all)-1] // last occurrence, closest to the failure
		names := p.re.SubexpNames()
		for _, want := range p.fields {
			for gi, gn := range names {
				if gn == want && gi < len(m) {
					set(want, m[gi])
				}
			}
		}
	}
	if f.ErrorType == "" && f.ErrorMessage != "" {
		f.ErrorType = "UnknownError"
	}
	return f
}

// ReadCILog reads a CI log file, rejecting an empty/whitespace-only file.
func ReadCILog(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("CI log not found: %s: %w", path, os.ErrNotExist)
		}
		return "", err
	}
	if strings.TrimSpace(string(b)) == "" {
		return "", fmt.Errorf("%w: %s contains no text", ErrEmptyLog, path)
	}
	return string(b), nil
}

// DiagnoseSystemPrompt is the system message for ci-diagnose.
const DiagnoseSystemPrompt = "You are a CI failure analyst. Perform root-cause analysis on the failure " +
	"below and answer with: (1) the root cause, (2) the specific file and change needed, " +
	"(3) how to verify the fix. Be concrete; do not invent files you were not shown."

// BuildDiagnosePrompt renders the diagnosis user message: the structured
// extraction plus a bounded tail of the raw log (the failure is at the end).
func BuildDiagnosePrompt(f Failure, log string) string {
	var b strings.Builder
	b.WriteString("# Extracted failure signal\n")
	fmt.Fprintf(&b, "- error type: %s\n", orUnknown(f.ErrorType))
	fmt.Fprintf(&b, "- error message: %s\n", orUnknown(f.ErrorMessage))
	fmt.Fprintf(&b, "- file: %s\n", orUnknown(f.FilePath))
	line := "unknown"
	if f.LineNumber > 0 {
		line = strconv.Itoa(f.LineNumber)
	}
	fmt.Fprintf(&b, "- line: %s\n", line)
	fmt.Fprintf(&b, "- test: %s\n", orUnknown(f.TestName))
	b.WriteString("\n# CI log (tail)\n```\n")
	b.WriteString(tailChars(log, 8000))
	b.WriteString("\n```\n")
	return b.String()
}

func orUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return "unknown"
	}
	return s
}

// tailChars returns the last n characters of s, prefixed with a truncation
// marker when it had to cut. CI logs put the failure at the END, so a head
// truncation would throw away exactly the part that matters.
func tailChars(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return fmt.Sprintf("[... %d earlier characters truncated ...]\n%s", len(r)-n, string(r[len(r)-n:]))
}
