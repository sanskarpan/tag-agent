package tool

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// The false-positive side matters at least as much as the true-positive side:
// refusing a legitimate text file breaks the common case. These are the shapes
// that plausibly look binary and are not.
func TestNotTextReasonAcceptsRealTextFiles(t *testing.T) {
	cases := map[string]string{
		"go source":        "package main\n\nfunc main() { println(\"hi\") }\n",
		"minified js":      strings.Repeat(`!function(e,t){"use strict";var n=e.x||{};n.y=t}(window,document);`, 40),
		"base64 blob":      strings.Repeat("SGVsbG8gd29ybGQgdGhpcyBpcyBiYXNlNjQgcGFkZGluZw==", 60),
		"json escapes":     `{"a":"A\n\t\"quoted\"","b":[1,2,3],"c":{"d":null}}`,
		"cjk prose":        "日本語のテキストです。これは普通のテキストファイルです。\n中文内容也应该被接受。\n",
		"emoji":            "commit 🎉 done ✅ shipping 🚀\n",
		"crlf":             "line one\r\nline two\r\n",
		"tabs and form":    "a\tb\tc\n\x0c\n",
		"empty":            "",
		"single newline":   "\n",
		"long single line": strings.Repeat("x", 100_000),
		"ansi colours":     "\x1b[31mred\x1b[0m and \x1b[32mgreen\x1b[0m\n",
	}
	for name, body := range cases {
		if r := notTextReason([]byte(body)); r != "" {
			t.Errorf("%s: wrongly refused as %q", name, r)
		}
	}
}

func TestNotTextReasonRefusesBinary(t *testing.T) {
	cases := map[string][]byte{
		"pdf":    []byte("%PDF-1.7\n1 0 obj\n<< /Type /Catalog >>\nstream\n\x00\x01\x02\xff"),
		"zip":    []byte("PK\x03\x04\x14\x00\x00\x00\x08\x00"),
		"png":    []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"),
		"jpeg":   []byte("\xff\xd8\xff\xe0\x00\x10JFIF"),
		"gzip":   []byte("\x1f\x8b\x08\x00\x00\x00\x00\x00"),
		"elf":    []byte("\x7fELF\x02\x01\x01\x00"),
		"sqlite": []byte("SQLite format 3\x00\x10\x00\x01\x01"),
		"wasm":   []byte("\x00asm\x01\x00\x00\x00"),
		"nul in the middle": append([]byte(strings.Repeat("text ", 100)),
			append([]byte{0x00}, []byte(strings.Repeat("more ", 100))...)...),
		"deflate-ish noise": func() []byte {
			b := make([]byte, 4096)
			for i := range b {
				b[i] = byte((i*7919 + 13) % 256)
			}
			return b
		}(),
	}
	for name, body := range cases {
		if r := notTextReason(body); r == "" {
			t.Errorf("%s: not refused", name)
		}
	}
}

// UTF-16 is text, but not text this tool can hand back as a Go string; the
// message should say which encoding rather than calling it binary.
func TestNotTextReasonNamesUTF16(t *testing.T) {
	for name, body := range map[string][]byte{
		"utf16le": append([]byte{0xff, 0xfe}, []byte("h\x00e\x00l\x00l\x00o\x00")...),
		"utf16be": append([]byte{0xfe, 0xff}, []byte("\x00h\x00e\x00l\x00l\x00o")...),
	} {
		r := notTextReason(body)
		if !strings.Contains(r, "UTF-16") {
			t.Errorf("%s: reason %q should name the encoding", name, r)
		}
	}
}

// A probe boundary that lands mid-rune is not corruption.
func TestNotTextReasonToleratesMidRuneTruncation(t *testing.T) {
	body := []byte(strings.Repeat("日", binaryProbeBytes)) // 3 bytes per rune
	if r := notTextReason(body); r != "" {
		t.Errorf("multibyte text truncated at the probe boundary was refused: %q", r)
	}
}

func readFileExec(t *testing.T, root string) func(string) (string, error) {
	t.Helper()
	opts := DefaultOptions()
	opts.Root = root
	tool := readFileTool(opts)
	return func(p string) (string, error) {
		return tool.Exec(context.Background(), map[string]any{"path": p})
	}
}

// TestReadFileRefusesBinary is the #666 regression: read_file returned raw bytes
// and reported success.
func TestReadFileRefusesBinary(t *testing.T) {
	root := t.TempDir()
	pdf := append([]byte("%PDF-1.4\n"), make([]byte, 5000)...)
	for i := range pdf[9:] {
		pdf[9+i] = byte(i % 251)
	}
	if err := os.WriteFile(filepath.Join(root, "doc.pdf"), pdf, 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := readFileExec(t, root)("doc.pdf")
	if err == nil {
		t.Fatalf("reading a PDF must fail, got %d bytes of output", len(out))
	}
	if out != "" {
		t.Errorf("no bytes may reach the caller on refusal, got %d", len(out))
	}
	for _, want := range []string{"doc.pdf", "PDF"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal should mention %q: %v", want, err)
		}
	}
	// The advice must be actionable, and which advice is right depends on
	// whether the document engine is installed — so assert that ONE of the two
	// real answers is offered rather than pinning a fixed string. This test
	// previously required "Convert it", which stopped being the best advice for
	// a PDF once read_document existed.
	msg := err.Error()
	if !strings.Contains(msg, "read_document") && !strings.Contains(msg, "npm install") {
		t.Errorf("the refusal must say what would work: %v", err)
	}
}

func TestReadFileStillReadsText(t *testing.T) {
	root := t.TempDir()
	body := "package main\n\n// 日本語 comment 🎉\nfunc main() {}\n"
	os.WriteFile(filepath.Join(root, "a.go"), []byte(body), 0o644)
	got, err := readFileExec(t, root)("a.go")
	if err != nil {
		t.Fatalf("a legitimate text file must still be readable: %v", err)
	}
	if got != body {
		t.Errorf("content changed:\n got %q\nwant %q", got, body)
	}
}

// Truncation must be stated even for text — returning a third of a file and
// reporting success is the same broken guarantee in a quieter form.
func TestReadFileStatesTruncation(t *testing.T) {
	root := t.TempDir()
	opts := DefaultOptions()
	big := strings.Repeat("abcdefghij\n", int(opts.MaxReadBytes/10))
	os.WriteFile(filepath.Join(root, "big.txt"), []byte(big), 0o644)

	got, err := readFileExec(t, root)("big.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "TRUNCATED") {
		t.Errorf("a truncated read must say so; got %d bytes ending %q",
			len(got), got[max(0, len(got)-80):])
	}
	if int64(len(got)) < opts.MaxReadBytes {
		t.Errorf("the content itself must still be returned, got %d bytes", len(got))
	}
}

// A file exactly at the cap is complete, not truncated.
func TestReadFileDoesNotClaimTruncationAtExactSize(t *testing.T) {
	root := t.TempDir()
	opts := DefaultOptions()
	os.WriteFile(filepath.Join(root, "exact.txt"),
		[]byte(strings.Repeat("z", int(opts.MaxReadBytes))), 0o644)
	got, err := readFileExec(t, root)("exact.txt")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "TRUNCATED") {
		t.Error("a file exactly at the cap is complete and must not be labelled truncated")
	}
}

func TestReadFileEmptyFileIsFine(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "empty.txt"), nil, 0o644)
	got, err := readFileExec(t, root)("empty.txt")
	if err != nil || got != "" {
		t.Errorf("an empty file is a legitimate read: %q %v", got, err)
	}
}

// --- findings from the adversarial review of this change --------------------

// Nine of the magic numbers are plain printable ASCII, matched at offset 0 with
// no corroboration — so any text file starting with those characters was
// refused. `MZ,Region,Total` was reported as a Windows executable.
func TestPrintableMagicsNeedCorroboration(t *testing.T) {
	cases := map[string]string{
		"MZ csv":     "MZ,Region,Total\nEU,3,4\n",
		"MZ prose":   "MZ Corporation quarterly report for the period ending 31 March.\n",
		"MZ base64":  "MZqQAAMAAAAEAAAA//8AALgAAAAAAAAAQAAAAAAAAAAAAAAAAAAAAAAAAAA=\n",
		"ID3 csv":    "ID3,ID4,Name\n1,2,alice\n",
		"ID3 sql":    "CREATE TABLE t (\n  ID3 INT NOT NULL,\n  ID4 INT\n);\n",
		"BZh prose":  "BZh is the bzip2 magic number, three ASCII characters.\n",
		"RIFF doc":   "RIFF containers hold WAV and AVI data. See the spec for details.\n",
		"OggS notes": "OggS pages carry the codec payload.\n",
		"fLaC notes": "fLaC is the FLAC stream marker.\n",
		"OTTO notes": "OTTO indicates CFF outlines in an OpenType font.\n",
		"wOFF notes": "wOFF and wOF2 are the two web font wrappers.\n",
	}
	for name, body := range cases {
		if r := notTextReason([]byte(body)); r != "" {
			t.Errorf("%s: text wrongly refused as %q", name, r)
		}
	}
}

// The corroborated forms must still be caught.
func TestCorroboratedBinariesAreStillRefused(t *testing.T) {
	pe := make([]byte, 0x80)
	copy(pe, "MZ")
	pe[0x3c] = 0x40
	copy(pe[0x40:], "PE\x00\x00")
	riff := append([]byte("RIFF"), append([]byte{0, 0, 0, 0}, []byte("WAVE")...)...)
	for name, body := range map[string][]byte{
		"real PE":    pe,
		"real RIFF":  riff,
		"real bzip2": []byte("BZh9\x31\x41\x59\x26\x53\x59"),
		"real ID3":   append([]byte("ID3"), []byte{0x03, 0x00, 0x00}...),
	} {
		if notTextReason(body) == "" {
			t.Errorf("%s: not refused", name)
		}
	}
}

// A ratio over a handful of bytes is not evidence. Progress bars and bells are
// ordinary in the build logs an agent is pointed at.
func TestControlRatioNeedsASample(t *testing.T) {
	for name, body := range map[string]string{
		"tiny with DEL":  "a\x7f",
		"short with SOH": "hi\x01there",
		"backspace bar":  "progress \x08\x08\x08 50%\ndone\n",
		"bell log":       "beep\x07\x07\x07 done\n",
	} {
		if r := notTextReason([]byte(body)); r != "" {
			t.Errorf("%s: wrongly refused as %q", name, r)
		}
	}
	// A genuinely control-heavy large file is still caught.
	noisy := make([]byte, 4096)
	for i := range noisy {
		noisy[i] = byte(i % 0x1f)
	}
	if notTextReason(noisy) == "" {
		t.Error("a large control-heavy file must still be refused")
	}
}

// The signature loop was anchored at offset 0, so one leading newline defeated
// it and the PDF came back as text.
func TestLeadingWhitespaceDoesNotHideASignature(t *testing.T) {
	body := append([]byte("\n  \t"), []byte("%PDF-1.4\nstream\n")...)
	if r := notTextReason(body); r == "" {
		t.Error("a PDF behind leading whitespace was accepted as text")
	}
}

// The head window is 8 KiB of a 256 KiB read, so binary further in was missed.
func TestBinaryPastTheHeadWindowIsCaught(t *testing.T) {
	body := append([]byte(strings.Repeat("plain ascii text\n", 600)), make([]byte, 20000)...)
	for i := 10200; i < len(body); i++ {
		body[i] = byte((i * 7) % 256)
	}
	if r := notTextReason(body); r == "" {
		t.Error("binary past the head window was accepted as text")
	}
}

// Truncation must not hand back invalid UTF-8 from a tool whose contract is
// UTF-8 text, and the notice must lead so it cannot be written back silently.
func TestTruncatedReadIsValidUTF8AndLeadsWithTheNotice(t *testing.T) {
	root := t.TempDir()
	opts := DefaultOptions()
	// Multibyte content guarantees the cap lands mid-rune.
	body := strings.Repeat("日", int(opts.MaxReadBytes/3)+2000)
	os.WriteFile(filepath.Join(root, "big.txt"), []byte(body), 0o644)

	got, err := readFileExec(t, root)("big.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !utf8.ValidString(got) {
		t.Error("a truncated read returned invalid UTF-8")
	}
	if !strings.HasPrefix(got, TruncationMarker) {
		t.Errorf("the notice must lead, got %.80q", got)
	}
	if !strings.Contains(got, "do not write it back") {
		t.Error("the notice must tell the model not to write it back")
	}
}

// ...and write_file must enforce that, since read-whole/write-whole is the only
// edit path available.
func TestWriteFileRefusesTruncatedContent(t *testing.T) {
	root := t.TempDir()
	opts := DefaultOptions()
	opts.Root = root
	w := writeFileTool(opts)
	_, err := w.Exec(context.Background(), map[string]any{
		"path":    "out.txt",
		"content": TruncationMarker + " ...]\n\nsome partial content",
	})
	if err == nil {
		t.Fatal("writing back a truncated read must be refused")
	}
	if _, serr := os.Stat(filepath.Join(root, "out.txt")); serr == nil {
		t.Error("nothing may be written on refusal")
	}
}
