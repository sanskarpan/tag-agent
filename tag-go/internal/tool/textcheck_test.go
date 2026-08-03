package tool

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	for _, want := range []string{"doc.pdf", "PDF", "Convert it"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal should mention %q: %v", want, err)
		}
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
