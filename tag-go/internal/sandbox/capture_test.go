package sandbox

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Regression for the unbounded output capture in both sandbox backends
// (CWE-400). The confinement is not the issue — the SUPERVISOR is: a sandboxed
// `cat /dev/zero` was copied into an unbounded bytes.Buffer in the TAG process,
// which is outside the sandbox and outside docker's --memory limit.

func TestCapBufferBoundsAndDiscloses(t *testing.T) {
	b := &capBuffer{Max: 16}
	if _, err := b.Write([]byte(strings.Repeat("a", 100))); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Write([]byte(strings.Repeat("b", 100))); err != nil {
		t.Fatal(err)
	}
	s := b.String()
	if !strings.HasPrefix(s, strings.Repeat("a", 16)) {
		t.Errorf("the first Max bytes must be kept verbatim: %q", s)
	}
	if kept, _, _ := strings.Cut(s, "\n[sandbox:"); kept != strings.Repeat("a", 16) {
		t.Errorf("bytes past the cap must be discarded, kept %q", kept)
	}
	if !strings.Contains(s, "truncated") || !strings.Contains(s, "184") {
		t.Errorf("truncation must be disclosed with the dropped count: %q", s)
	}
}

func TestCapBufferNeverShortWrites(t *testing.T) {
	b := &capBuffer{Max: 4}
	n, err := b.Write([]byte("0123456789"))
	if n != 10 || err != nil {
		t.Fatalf("a capped writer must still report a full write: n=%d err=%v", n, err)
	}
}

func TestCapBufferUnderCapIsVerbatim(t *testing.T) {
	b := newCapBuffer()
	b.Write([]byte("hello"))
	if b.String() != "hello" {
		t.Errorf("output under the cap must pass through unchanged: %q", b.String())
	}
}

// TestRestrictedBackendBoundsItsCapture drives the real backend. It is skipped
// where the platform cannot confine (the sandbox fails closed there and never
// runs the command).
func TestRestrictedBackendBoundsItsCapture(t *testing.T) {
	res, err := Exec(context.Background(), Options{
		// 64 MiB against a 4 MiB cap.
		Command: "head -c 67108864 /dev/zero | tr '\\0' 'a'",
		Dir:     t.TempDir(),
		Timeout: 60 * time.Second,
	})
	if err != nil {
		t.Skipf("restricted backend unavailable here: %v", err)
	}
	if res.Exit == 127 && strings.Contains(res.Isolation, "failed closed") {
		t.Skip("platform cannot confine; the command never ran")
	}
	if len(res.Stdout) > MaxCaptureBytes+4096 {
		t.Fatalf("UNBOUNDED: captured %d bytes against a %d-byte cap", len(res.Stdout), MaxCaptureBytes)
	}
	if !strings.Contains(res.Stdout, "truncated") {
		t.Errorf("truncation must be disclosed, got %d bytes", len(res.Stdout))
	}
}
