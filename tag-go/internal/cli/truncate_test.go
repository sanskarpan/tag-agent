package cli

import (
	"testing"
	"unicode/utf8"
)

// TestTruncateRuneSafe: truncate must never split a multibyte character and emit
// invalid UTF-8 into table output. RED against pre-fix code, which did a bare
// byte slice s[:n] (#738).
func TestTruncateRuneSafe(t *testing.T) {
	const s = "日本語テスト" // each rune is 3 bytes
	for n := 0; n <= len(s)+2; n++ {
		got := truncate(s, n)
		if !utf8.ValidString(got) {
			t.Errorf("truncate(%q, %d) = %q — not valid UTF-8", s, n, got)
		}
		if len(got) > n && n <= len(s) {
			t.Errorf("truncate(%q, %d) = %q exceeds the byte budget (%d)", s, n, got, len(got))
		}
	}
	// ASCII is unaffected.
	if truncate("hello world", 5) != "hello" {
		t.Errorf("ASCII truncate changed behaviour: %q", truncate("hello world", 5))
	}
}
