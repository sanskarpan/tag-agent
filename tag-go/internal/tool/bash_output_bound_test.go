package tool

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Regression for the unbounded bash output capture (CWE-400).
//
// read_file was bounded by MaxReadBytes, but bash accumulated combined
// stdout+stderr into a bytes.Buffer with no ceiling. The command string comes
// straight from model output, so `cat /dev/zero`, `yes`, or an accidental
// `find /` filled RAM until the process died — and the resulting string was
// then handed to the model as a tool result.

// TestBashOutputIsBounded: a command that emits far more than the cap must come
// back bounded, promptly, and must SAY it was truncated.
func TestBashOutputIsBounded(t *testing.T) {
	run := bashTool(Options{BashTimeout: 20 * time.Second, MaxReadBytes: 4096}).Exec
	// 8 MiB of output against a 4 KiB cap.
	out, err := run(context.Background(), map[string]any{
		"command": "head -c 8388608 /dev/zero | tr '\\0' 'a'"})
	if err != nil {
		t.Fatalf("the command itself should succeed: %v", err)
	}
	if len(out) > 64*1024 {
		t.Fatalf("UNBOUNDED: bash returned %d bytes against a 4096-byte cap", len(out))
	}
	if !strings.Contains(out, "truncated") {
		t.Errorf("a truncated result must say so, got %d bytes ending %q", len(out), tailOf(out))
	}
}

// TestBashOutputBoundAppliesToStderrToo: the cap covers the combined stream, so
// a command that floods stderr cannot slip past it.
func TestBashOutputBoundAppliesToStderrToo(t *testing.T) {
	run := bashTool(Options{BashTimeout: 20 * time.Second, MaxReadBytes: 4096}).Exec
	out, _ := run(context.Background(), map[string]any{
		"command": "head -c 4194304 /dev/zero | tr '\\0' 'e' >&2"})
	if len(out) > 64*1024 {
		t.Fatalf("UNBOUNDED: stderr flood returned %d bytes against a 4096-byte cap", len(out))
	}
}

// TestBashOutputUnderTheCapIsUnchanged is the anti-overshoot check: normal
// output must arrive byte-for-byte, with no truncation notice bolted on.
func TestBashOutputUnderTheCapIsUnchanged(t *testing.T) {
	run := bashTool(Options{BashTimeout: 10 * time.Second, MaxReadBytes: 4096}).Exec
	out, err := run(context.Background(), map[string]any{"command": "printf 'hello world'"})
	if err != nil || out != "hello world" {
		t.Fatalf("small output must pass through verbatim: %q %v", out, err)
	}
}

// TestBashOutputBoundKeepsTheExitError: truncation must not swallow a non-zero
// exit status.
func TestBashOutputBoundKeepsTheExitError(t *testing.T) {
	run := bashTool(Options{BashTimeout: 20 * time.Second, MaxReadBytes: 1024}).Exec
	out, err := run(context.Background(), map[string]any{
		"command": "head -c 1048576 /dev/zero | tr '\\0' 'x'; exit 3"})
	if err == nil {
		t.Fatal("a non-zero exit must still be reported after truncation")
	}
	if len(out) > 64*1024 {
		t.Fatalf("UNBOUNDED: %d bytes", len(out))
	}
}

func tailOf(s string) string {
	if len(s) < 120 {
		return s
	}
	return s[len(s)-120:]
}
