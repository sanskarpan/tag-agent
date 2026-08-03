package docs

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeEngine writes a stub `pdf-inspector` that replies from a scripted table,
// so the suite exercises this package's contract without depending on the real
// engine being installed.
//
// script maps a --pages value ("" for the whole-document call) to the JSON the
// engine should emit.
func fakeEngine(t *testing.T, script map[string]any) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell stub is POSIX-only")
	}
	dir := t.TempDir()
	table := map[string]string{}
	for k, v := range script {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		table[k] = string(b)
	}
	var cases strings.Builder
	for k, v := range table {
		fmt.Fprintf(&cases, "  %q) printf '%%s' %s ;;\n", k, shellQuote(v))
	}
	body := fmt.Sprintf(`#!/bin/sh
pages=""
prev=""
for a in "$@"; do
  if [ "$prev" = "--pages" ]; then pages="$a"; fi
  prev="$a"
done
case "$pages" in
%s  *) echo "no scripted reply for pages=$pages" >&2; exit 1 ;;
esac
`, cases.String())
	p := filepath.Join(dir, BinaryName)
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvOverride, p)
	return p
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func tempPDF(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "doc.pdf")
	if err := os.WriteFile(p, []byte("%PDF-1.4\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAvailableReportsAbsenceNotFailure(t *testing.T) {
	t.Setenv(EnvOverride, filepath.Join(t.TempDir(), "does-not-exist"))
	if _, ok := Available(); ok {
		t.Error("an override that does not resolve must not report available")
	}
	_, err := Extract(context.Background(), "x.pdf", Options{})
	if err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Errorf("expected an unavailable error naming the fix, got %v", err)
	}
}

// A clean text-based document is complete and carries no caveats — a warning on
// every read teaches the reader to ignore warnings.
func TestCleanDocumentIsCompleteAndQuiet(t *testing.T) {
	fakeEngine(t, map[string]any{
		"": map[string]any{
			"pdfType": "TextBased", "pageCount": 2, "title": "Spec",
			"markdown": "# Spec\n\nbody text that is clearly long enough\n\nmore body",
		},
		"1": map[string]any{"markdown": "page one has real text"},
		"2": map[string]any{"markdown": "page two has real text"},
	})
	d, err := Extract(context.Background(), tempPDF(t), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !d.Complete {
		t.Errorf("a fully-read document must be complete; notes=%v", d.Notes)
	}
	if len(d.Notes) != 0 {
		t.Errorf("a clean read must be quiet, got %v", d.Notes)
	}
	if d.Title != "Spec" || d.PageCount != 2 {
		t.Errorf("metadata lost: %+v", d)
	}
}

// The headline behaviour: the engine says every page is fine, but a page
// produced no text. That page was NOT read, and the result must say so.
func TestVerificationCatchesPagesTheEngineMissed(t *testing.T) {
	fakeEngine(t, map[string]any{
		"": map[string]any{
			"pdfType": "TextBased", "pageCount": 3,
			"pagesNeedingOcr": []int{}, // the engine claims everything is readable
			"markdown":        "# Doc\n\nplenty of text from pages one and three",
		},
		"1": map[string]any{"markdown": "real text on page one"},
		"2": map[string]any{"markdown": "   "}, // blank: a scan the engine did not flag
		"3": map[string]any{"markdown": "real text on page three"},
	})
	d, err := Extract(context.Background(), tempPDF(t), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.PagesNeedingOCR) != 1 || d.PagesNeedingOCR[0] != 2 {
		t.Errorf("page 2 produced no text and must be reported: got %v", d.PagesNeedingOCR)
	}
	if d.Complete {
		t.Error("a document with an unreadable page is not complete")
	}
	if !strings.Contains(strings.Join(d.Notes, " "), "did not flag") {
		t.Errorf("the notes must say verification found it, got %v", d.Notes)
	}
}

// Indexing is normalised in one place because the engine's own APIs disagree.
func TestPageIndexingIsNormalised(t *testing.T) {
	got := normalizePages([]int{3, 1, 3, 0, 99, -2, 2}, 5)
	want := []int{1, 2, 3}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("got %v, want %v (0 and out-of-range dropped, sorted, deduped)", got, want)
	}
	if normalizePages(nil, 5) != nil {
		t.Error("no pages means nil, not a zero-length surprise")
	}
}

func TestFormatPagesIsCompact(t *testing.T) {
	for in, want := range map[string]string{
		"1,2,3,7":       "1-3, 7",
		"4":             "4",
		"1,2":           "1, 2",
		"9,1,2,3,4,5,6": "1-6, 9",
	} {
		var pages []int
		for _, f := range strings.Split(in, ",") {
			var n int
			fmt.Sscanf(f, "%d", &n)
			pages = append(pages, n)
		}
		if got := formatPages(pages); got != want {
			t.Errorf("formatPages(%s) = %q, want %q", in, got, want)
		}
	}
	if formatPages(nil) != "none" {
		t.Error("an empty list should read as none")
	}
}

// Extraction size is chosen by the input, so it must be bounded by us — and
// exceeding the bound must be stated, never silently clipped.
func TestOversizedExtractionIsTruncatedAndSaysSo(t *testing.T) {
	big := strings.Repeat("word ", 5000)
	fakeEngine(t, map[string]any{
		"":  map[string]any{"pdfType": "TextBased", "pageCount": 1, "markdown": big},
		"1": map[string]any{"markdown": big},
	})
	d, err := Extract(context.Background(), tempPDF(t), Options{MaxBytes: 500})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Markdown) > 500 {
		t.Errorf("markdown is %d bytes, over the 500 cap", len(d.Markdown))
	}
	if d.Complete {
		t.Error("a truncated document is not complete")
	}
	if !strings.Contains(strings.Join(d.Notes, " "), "truncated") {
		t.Errorf("truncation must be stated, got %v", d.Notes)
	}
}

// Nothing extracted and nothing flagged is "could not read it", never "it is
// empty".
func TestEmptyExtractionIsReportedAsUnread(t *testing.T) {
	fakeEngine(t, map[string]any{
		"":  map[string]any{"pdfType": "TextBased", "pageCount": 1, "markdown": ""},
		"1": map[string]any{"markdown": ""},
	})
	d, err := Extract(context.Background(), tempPDF(t), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if d.Complete {
		t.Error("a document that produced no text is not complete")
	}
	joined := strings.Join(d.Notes, " ")
	if !strings.Contains(joined, "OCR") && !strings.Contains(joined, "not evidence that it is empty") {
		t.Errorf("must not read as an empty document, got %v", d.Notes)
	}
}

// A scanned document is reported as scanned, not partially invented.
func TestScannedDocumentIsReportedNotGuessed(t *testing.T) {
	fakeEngine(t, map[string]any{
		"":  map[string]any{"pdfType": "Scanned", "pageCount": 2, "pagesNeedingOcr": []int{1, 2}, "markdown": ""},
		"1": map[string]any{"markdown": ""},
		"2": map[string]any{"markdown": ""},
	})
	d, err := Extract(context.Background(), tempPDF(t), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if d.Type != "Scanned" || d.Complete {
		t.Errorf("expected an incomplete scanned document, got %+v", d)
	}
	if len(d.PagesNeedingOCR) != 2 {
		t.Errorf("both pages need OCR, got %v", d.PagesNeedingOCR)
	}
}

// An engine failure is an error, not an empty document.
func TestEngineFailureIsAnError(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, BinaryName)
	os.WriteFile(p, []byte("#!/bin/sh\necho 'boom: bad xref' >&2\nexit 1\n"), 0o755)
	t.Setenv(EnvOverride, p)
	if _, err := Extract(context.Background(), tempPDF(t), Options{}); err == nil {
		t.Fatal("an engine failure must surface as an error")
	} else if !strings.Contains(err.Error(), "boom") {
		t.Errorf("the engine's own message should reach the caller: %v", err)
	}
}

func TestNonJSONOutputIsAnError(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, BinaryName)
	os.WriteFile(p, []byte("#!/bin/sh\necho not json\n"), 0o755)
	t.Setenv(EnvOverride, p)
	if _, err := Extract(context.Background(), tempPDF(t), Options{}); err == nil ||
		!strings.Contains(err.Error(), "not JSON") {
		t.Errorf("expected a parse error, got %v", err)
	}
}

func TestMissingFileIsAnError(t *testing.T) {
	fakeEngine(t, map[string]any{"": map[string]any{}})
	if _, err := Extract(context.Background(), filepath.Join(t.TempDir(), "nope.pdf"), Options{}); err == nil {
		t.Error("a missing file must error before the engine runs")
	}
}

func TestSupportedIsExtensionBased(t *testing.T) {
	if !Supported("a/b/C.PDF") || !Supported("x.pdf") {
		t.Error("PDFs are supported regardless of case")
	}
	if Supported("notes.txt") || Supported("x.docx") {
		t.Error("only PDF is claimed today; claiming more would be a promise we do not keep")
	}
}
