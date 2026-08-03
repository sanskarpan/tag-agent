package tool

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// The tool's own description says "Read a UTF-8 text file". It did not check.
// `return string(b), nil` handed a PDF's raw bytes to the model and reported
// success — measured on a real 896 KB document: 262 KB of binary, 38% printable,
// 102,799 invalid UTF-8 bytes, 29% of the file, and roughly 100k tokens spent
// before the model could discover it had nothing.
//
// That is the fabricated-success pattern this project treats as a hard bar: the
// tool could not do the thing, did not say so, and returned output that LOOKS
// like content. It is also the expensive kind of wrong.
//
// The design constraint that matters here is the false-positive rate. Refusing a
// legitimate text file is a regression in the common case, so the checks below
// are deliberately conservative: they fire on evidence a file is *not* text, not
// on the absence of evidence that it is.

// binaryProbeBytes is how much of a file is examined. Signatures live at the
// head; a byte-class sample from the same window is representative enough
// without reading the whole file twice.
const binaryProbeBytes = 8192

// signature couples a magic-number prefix with the human name of the format, so
// the refusal can say "PDF" rather than "binary data".
type signature struct {
	magic []byte
	name  string
}

// signatures covers the formats a coding agent is actually pointed at by
// accident. It is not exhaustive and does not need to be — the byte-class
// heuristic below is the general case, and this list only improves the message.
var signatures = []signature{
	{[]byte("%PDF-"), "PDF"},
	{[]byte("PK\x03\x04"), "ZIP archive (or a zip-based format such as .docx/.xlsx/.pptx/.jar)"},
	{[]byte("\x89PNG\r\n\x1a\n"), "PNG image"},
	{[]byte("\xff\xd8\xff"), "JPEG image"},
	{[]byte("GIF87a"), "GIF image"},
	{[]byte("GIF89a"), "GIF image"},
	{[]byte("RIFF"), "RIFF container (WAV/AVI/WebP)"},
	{[]byte("\x1f\x8b"), "gzip archive"},
	{[]byte("BZh"), "bzip2 archive"},
	{[]byte("\xfd7zXZ\x00"), "xz archive"},
	{[]byte("7z\xbc\xaf\x27\x1c"), "7-Zip archive"},
	{[]byte("\x7fELF"), "ELF binary"},
	{[]byte("MZ"), "Windows executable"},
	{[]byte("\xca\xfe\xba\xbe"), "Mach-O universal binary or Java class file"},
	{[]byte("\xcf\xfa\xed\xfe"), "Mach-O binary"},
	{[]byte("SQLite format 3\x00"), "SQLite database"},
	{[]byte("OggS"), "Ogg media"},
	{[]byte("fLaC"), "FLAC audio"},
	{[]byte("ID3"), "MP3 audio"},
	{[]byte("\x00\x61\x73\x6d"), "WebAssembly module"},
	{[]byte("wOFF"), "WOFF font"},
	{[]byte("wOF2"), "WOFF2 font"},
	{[]byte("\x00\x01\x00\x00\x00"), "TrueType font"},
	{[]byte("OTTO"), "OpenType font"},
}

// notTextReason returns a human explanation when b is not readable text, or ""
// when it is. Empty input is text (an empty file is a legitimate read).
func notTextReason(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	probe := b
	if len(probe) > binaryProbeBytes {
		probe = probe[:binaryProbeBytes]
	}

	for _, s := range signatures {
		if len(probe) >= len(s.magic) && string(probe[:len(s.magic)]) == string(s.magic) {
			return "it is a " + s.name
		}
	}

	// UTF-16/UTF-32 are text, but not text this tool can return: the caller gets
	// a Go string and every other byte would be NUL. Naming the encoding is more
	// useful than calling it binary.
	switch {
	case len(probe) >= 2 && probe[0] == 0xff && probe[1] == 0xfe:
		return "it is UTF-16/UTF-32 little-endian text, which this tool cannot return as UTF-8"
	case len(probe) >= 2 && probe[0] == 0xfe && probe[1] == 0xff:
		return "it is UTF-16 big-endian text, which this tool cannot return as UTF-8"
	}

	// A NUL byte is the classic binary tell and effectively never appears in
	// source, config or prose. Checking it separately from the ratio below means
	// a mostly-printable binary with embedded NULs is still caught.
	if idxByte(probe, 0x00) >= 0 {
		return "it contains NUL bytes, so it is not text"
	}

	if !utf8.Valid(probe) {
		// Truncating mid-rune at the probe boundary is not corruption, so only
		// treat invalid UTF-8 as decisive when it is not the boundary.
		trimmed := trimPartialRune(probe)
		if !utf8.Valid(trimmed) {
			return "it is not valid UTF-8"
		}
	}

	// Byte-class ratio, as the general case for formats with no signature.
	// Threshold is deliberately loose: minified JS, base64 blobs, JSON with
	// escapes and source in any language are all far above it, while a
	// compressed or encoded payload is far below.
	ctrl := 0
	for _, c := range probe {
		// ESC is deliberately not counted: ANSI colour sequences are ordinary in
		// build and test logs, which are exactly the files an agent is pointed at.
		// A short coloured log is >10% ESC and is unambiguously text.
		if c == 0x1b {
			continue
		}
		if c < 0x09 || (c > 0x0d && c < 0x20) || c == 0x7f {
			ctrl++
		}
	}
	if ratio := float64(ctrl) / float64(len(probe)); ratio > 0.10 {
		return fmt.Sprintf("%.0f%% of its bytes are control characters, so it is not text", ratio*100)
	}
	return ""
}

func idxByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}

// trimPartialRune drops a trailing incomplete UTF-8 sequence, so a probe that
// stops mid-character is not mistaken for corruption.
func trimPartialRune(b []byte) []byte {
	for i := len(b) - 1; i >= 0 && i > len(b)-5; i-- {
		if b[i]&0xc0 != 0x80 { // start of a rune
			if r, size := utf8.DecodeRune(b[i:]); r == utf8.RuneError && size <= 1 {
				return b[:i]
			}
			return b
		}
	}
	return b
}

// binaryRefusal builds the error a caller sees. It names the file, the reason,
// and what to do instead — a refusal that does not say what would work is just
// a dead end.
func binaryRefusal(displayPath, reason string, size int64) error {
	var b strings.Builder
	fmt.Fprintf(&b, "read_file: %s is not a text file — %s", displayPath, reason)
	if size > 0 {
		fmt.Fprintf(&b, " (%s)", humanBytes(size))
	}
	b.WriteString(". Returning its bytes would spend a large amount of context on content " +
		"the model cannot read. Convert it to text first, or use a tool built for this format.")
	return fmt.Errorf("%s", b.String())
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d bytes", n)
	}
}
