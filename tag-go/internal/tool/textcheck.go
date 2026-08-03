package tool

import (
	"fmt"

	"github.com/tag-agent/tag/internal/docs"
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

// minRatioSample is the smallest window the control-byte ratio is trusted on.
const minRatioSample = 512

// tailProbeBytes is sampled from the END of the buffer as well as the head.
// The head window covers 8 KiB of a 256 KiB read -- 3% -- so a file with an
// ASCII preamble and binary further in sailed through.
const tailProbeBytes = 4096

// signature couples a magic-number prefix with the human name of the format, so
// the refusal can say "PDF" rather than "binary data".
//
// confirm is required for magics that are PLAIN PRINTABLE ASCII. Those match
// ordinary text far too easily -- `MZ` is two letters, so `MZ,Region,Total` in a
// CSV, base64 starting `MZqQ`, and any prose beginning "MZ " were all refused as
// Windows executables. `ID3,ID4,Name` was refused as MP3 audio. A printable
// magic is a hint; it needs a second structural byte before it is evidence.
type signature struct {
	magic   []byte
	name    string
	confirm func(b []byte) bool
}

// signatures covers the formats a coding agent is actually pointed at by
// accident. It is not exhaustive and does not need to be — the byte-class
// heuristic below is the general case, and this list only improves the message.
var signatures = []signature{
	{[]byte("%PDF-"), "PDF", nil},
	{[]byte("PK\x03\x04"), "ZIP archive (or a zip-based format such as .docx/.xlsx/.pptx/.jar)", nil},
	{[]byte("\x89PNG\r\n\x1a\n"), "PNG image", nil},
	{[]byte("\xff\xd8\xff"), "JPEG image", nil},
	{[]byte("GIF87a"), "GIF image", nil},
	{[]byte("GIF89a"), "GIF image", nil},
	{[]byte("RIFF"), "RIFF container (WAV/AVI/WebP)", riffForm},
	{[]byte("\x1f\x8b"), "gzip archive", nil},
	{[]byte("BZh"), "bzip2 archive", func(b []byte) bool { return len(b) > 3 && b[3] >= '1' && b[3] <= '9' }},
	{[]byte("\xfd7zXZ\x00"), "xz archive", nil},
	{[]byte("7z\xbc\xaf\x27\x1c"), "7-Zip archive", nil},
	{[]byte("\x7fELF"), "ELF binary", nil},
	{[]byte("MZ"), "Windows executable", hasPEHeader},
	{[]byte("\xca\xfe\xba\xbe"), "Mach-O universal binary or Java class file", nil},
	{[]byte("\xcf\xfa\xed\xfe"), "Mach-O binary", nil},
	{[]byte("SQLite format 3\x00"), "SQLite database", nil},
	{[]byte("OggS"), "Ogg media", func(b []byte) bool { return len(b) > 4 && b[4] == 0x00 }},
	{[]byte("fLaC"), "FLAC audio", func(b []byte) bool { return len(b) > 4 && b[4]&0x7f <= 6 }},
	{[]byte("ID3"), "MP3 audio", func(b []byte) bool {
		// ID3v2 major version is 2-4 and the size is 7-bit safe. "ID3,ID4,Name"
		// in a CSV header has ',' here and is text.
		return len(b) > 9 && b[3] >= 2 && b[3] <= 4 && b[4] != 0xff &&
			b[6] < 0x80 && b[7] < 0x80 && b[8] < 0x80 && b[9] < 0x80
	}},
	{[]byte("\x00\x61\x73\x6d"), "WebAssembly module", nil},
	{[]byte("wOFF"), "WOFF font", sfntFlavor},
	{[]byte("wOF2"), "WOFF2 font", sfntFlavor},
	{[]byte("\x00\x01\x00\x00\x00"), "TrueType font", nil},
	{[]byte("OTTO"), "OpenType font", func(b []byte) bool { return len(b) > 5 && b[4] == 0x00 }},
}

// notTextReason returns a human explanation when b is not readable text, or ""
// when it is. Empty input is text (an empty file is a legitimate read).
func notTextReason(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	probe := b
	var tail []byte
	if len(probe) > binaryProbeBytes {
		probe = probe[:binaryProbeBytes]
		// A tail sample as well as the head: the head window is 3% of a 256 KiB
		// read, so a file with an ASCII preamble and binary further in slipped
		// through. Checked SEPARATELY rather than concatenated — splicing two
		// windows together invents an invalid-UTF-8 junction that is not in the
		// file.
		tail = b[len(b)-tailProbeBytes:]
	}

	// Skip leading whitespace before matching. The loop was anchored at offset
	// 0, so a single newline in front of "%PDF-" defeated it entirely and the
	// PDF was returned to the model as text.
	head := probe
	for len(head) > 0 && (head[0] == ' ' || head[0] == '\n' || head[0] == '\r' || head[0] == '\t') {
		head = head[1:]
	}
	for _, sig := range signatures {
		if len(head) < len(sig.magic) || string(head[:len(sig.magic)]) != string(sig.magic) {
			continue
		}
		if sig.confirm != nil && !sig.confirm(head) {
			continue // printable magic with no corroborating structure: treat as text
		}
		return "it is a " + sig.name
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
	// A ratio over a handful of bytes is not evidence of anything: "a\x7f" is
	// 50% control characters and is plainly text. Require a real sample first.
	if len(probe) >= minRatioSample {
		ctrl := 0
		for _, c := range probe {
			// ESC, BEL and BS are not counted. ANSI colour, progress bars that
			// backspace over themselves, and terminal bells are all ordinary in
			// build and test logs -- which are exactly the files an agent is
			// pointed at. A short coloured log is well over 10% ESC.
			if c == 0x1b || c == 0x07 || c == 0x08 {
				continue
			}
			if c < 0x09 || (c > 0x0d && c < 0x20) || c == 0x7f {
				ctrl++
			}
		}
		if ratio := float64(ctrl) / float64(len(probe)); ratio > 0.10 {
			return fmt.Sprintf("%.0f%% of its bytes are control characters, so it is not text", ratio*100)
		}
	}
	if len(tail) > 0 {
		if idxByte(tail, 0x00) >= 0 {
			return "it contains NUL bytes further into the file, so it is not text"
		}
		if !utf8.Valid(trimPartialRune(tail[firstRuneStart(tail):])) {
			return "it is not valid UTF-8 further into the file"
		}
	}
	return ""
}

// riffForm requires a known RIFF form type, so prose about RIFF containers is
// not mistaken for one.
func riffForm(b []byte) bool {
	if len(b) < 12 {
		return false
	}
	switch string(b[8:12]) {
	case "WAVE", "AVI ", "WEBP", "RMID":
		return true
	}
	return false
}

// hasPEHeader checks the PE offset at 0x3c, so "MZ,Region,Total" stays a CSV.
func hasPEHeader(b []byte) bool {
	if len(b) < 0x40 {
		return false
	}
	off := int(b[0x3c]) | int(b[0x3d])<<8 | int(b[0x3e])<<16 | int(b[0x3f])<<24
	return off >= 0 && off+4 <= len(b) && string(b[off:off+4]) == "PE\x00\x00"
}

// sfntFlavor requires a plausible sfnt flavor field after a web-font magic.
func sfntFlavor(b []byte) bool {
	if len(b) < 8 {
		return false
	}
	switch string(b[4:8]) {
	case "\x00\x01\x00\x00", "OTTO", "true", "ttcf":
		return true
	}
	return false
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
		"the model cannot read.")
	b.WriteString(" " + readAdvice(displayPath))
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

// TruncationMarker leads a truncated read_file result. write_file refuses
// content containing it: the model's only edit path is read-whole then
// write-whole, so writing back a truncated read would silently destroy
// everything past the cap.
const TruncationMarker = "[read_file: TRUNCATED"

func truncationNotice(cap, size int64) string {
	return fmt.Sprintf(
		"%s — this is the FIRST %s of a %s file. %s was NOT read. "+
			"The content below is INCOMPLETE: do not write it back to the file, "+
			"and do not treat the end of it as the end of the file.]",
		TruncationMarker, humanBytes(cap), humanBytes(size), humanBytes(size-cap))
}

// firstRuneStart returns the offset of the first rune boundary in b, so a tail
// window that begins mid-character is not read as corruption.
func firstRuneStart(b []byte) int {
	for i := 0; i < len(b) && i < 4; i++ {
		if b[i]&0xc0 != 0x80 {
			return i
		}
	}
	return 0
}

// readAdvice tells the caller what WOULD work. A refusal that does not is a
// dead end, and for PDFs the answer depends on whether the engine is installed
// — so the message says which situation the reader is in rather than offering
// advice that may not apply.
func readAdvice(path string) string {
	if !docs.Supported(path) {
		return "Convert it to text first, or use a tool built for this format."
	}
	if _, ok := docs.Available(); ok {
		return "Use read_document, which reads PDFs as text."
	}
	return "PDF support is available but not installed: " + docs.InstallHint + "."
}
