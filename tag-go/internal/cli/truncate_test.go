package cli

import (
	"testing"
	"unicode/utf8"

	"github.com/mattn/go-runewidth"
)

// TestPadDisplayAlignsByCell: padDisplay must produce a field of exactly w
// display cells regardless of CJK/accented content, so table columns line up
// (#763 tui). RED against byte-width "%-Ns" padding.
func TestPadDisplayAlignsByCell(t *testing.T) {
	for _, s := range []string{"", "ab", "日本語", "café", "⏳", "○"} {
		for _, w := range []int{2, 8, 14} {
			got := padDisplay(s, w)
			if gw := runewidth.StringWidth(got); gw != w {
				t.Errorf("padDisplay(%q, %d) width = %d, want %d (%q)", s, w, gw, w, got)
			}
		}
	}
	// ASCII shorter than the field is right-padded with spaces.
	if got := padDisplay("ab", 5); got != "ab   " {
		t.Errorf("padDisplay ASCII pad = %q, want %q", got, "ab   ")
	}
}

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
