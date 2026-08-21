// Package docs turns documents the model cannot read into text it can.
//
// It shells out to firecrawl/pdf-inspector, discovered on PATH exactly the way
// `gh` and `git` already are in this harness: used when present, never assumed,
// and its absence is reported as an absence rather than a failure.
//
// Why a subprocess and not a library. pdf-inspector is Rust. Its cdylib is the
// pyo3 extension module -- there is no C ABI to link against -- and its wasm
// build is wasm-bindgen, which wazero cannot run. Linking would also mean CGO,
// and a CGO-free static binary is a commitment this project has made. The npm
// package ships a prebuilt native binary and a CLI, so the process boundary is
// the one door that is actually open. See docs/prd/PRD-133.
//
// The contract this package adds on top of the engine is honesty about what was
// read. The engine's document-level "which pages need OCR" is a threshold
// judgement, not a fact, so Extract cross-checks it: a page the engine implies
// was read must have produced text, and a page that produced none is reported as
// unreadable no matter what the summary said. Handing back a document with
// silently blank pages, labelled complete, would reintroduce the exact pattern
// the read_file refusal was written to remove.
package docs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// BinaryName is the executable published by @firecrawl/pdf-inspector.
const BinaryName = "pdf-inspector"

// EnvOverride names an explicit path to the binary, for installs that keep it
// outside PATH (a repo-local node_modules/.bin, for instance).
const EnvOverride = "TAG_PDF_INSPECTOR"

// DefaultTimeout bounds one extraction. Documents are attacker-influenced input
// in the general case -- a webhook can hand TAG a path -- so the subprocess gets
// a deadline rather than the caller's patience.
const DefaultTimeout = 60 * time.Second

// MaxMarkdownBytes caps what an extraction may return.
//
// A large PDF can extract to megabytes of markdown, and every byte of it lands
// in a context window. This is the same class of bound as the SSE accumulators
// and the bash output cap: the size of the result is chosen by the input, so it
// has to be chosen by us instead. Exceeding it truncates and SAYS SO -- silently
// clipping a document and presenting it as whole is the failure mode this
// package exists to avoid.
const MaxMarkdownBytes = 1 << 20 // 1 MiB

// ErrUnavailable is returned when the engine is not installed. It is a distinct
// error so a caller can offer the install hint instead of reporting a failure.
var ErrUnavailable = errors.New("pdf-inspector is not installed")

// ErrBadInput marks operator-supplied path mistakes — a missing file or a
// directory. The CLI classifies these as usage errors (exit 2), matching
// `doc read --help` and the `fix-vuln` sibling; a genuine engine failure is a
// run failure (exit 1) and does not carry this sentinel.
var ErrBadInput = errors.New("bad document input")

// Document is the result of reading one file.
type Document struct {
	Path      string `json:"path"`
	Type      string `json:"type"`       // TextBased | Scanned | ImageBased | Mixed
	PageCount int    `json:"page_count"` // 0 when the engine did not report one
	Title     string `json:"title,omitempty"`
	Markdown  string `json:"markdown"`

	// PagesNeedingOCR is 1-BASED and reconciled: it is the union of what the
	// engine reported and what the verification pass found empty. Indexing is
	// normalised here, in one place, because the engine's own APIs disagree
	// about it and an off-by-one in "which page is unreadable" is the kind of
	// wrong that reads as right.
	PagesNeedingOCR []int `json:"pages_needing_ocr"`

	PagesWithTables  []int `json:"pages_with_tables,omitempty"`
	PagesWithColumns []int `json:"pages_with_columns,omitempty"`

	// Complete is false when any page could not be read, or when the markdown
	// was truncated. A caller must not present an incomplete document as whole.
	Complete bool `json:"complete"`

	// Notes explain every way the result falls short of "the whole document as
	// text". Empty means it does not.
	Notes []string `json:"notes,omitempty"`

	EngineMillis int `json:"engine_ms,omitempty"`
}

// Available reports the engine's path, or false when it is not installed.
func Available() (string, bool) {
	if p := strings.TrimSpace(os.Getenv(EnvOverride)); p != "" {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p, true
		}
		// An override that does not resolve is an operator mistake worth failing
		// on rather than silently falling back to PATH and using a different
		// binary than the one they named.
		return "", false
	}
	p, err := exec.LookPath(BinaryName)
	if err != nil {
		return "", false
	}
	return p, true
}

// InstallHint is the message shown when the engine is absent. It names the one
// command that fixes it.
const InstallHint = "install it with `npm install -g @firecrawl/pdf-inspector` " +
	"(ships a prebuilt binary; no toolchain required), or set " + EnvOverride +
	" to an existing one"

// Supported reports whether this package can be expected to handle the file,
// based on extension alone. It is a routing hint, not a guarantee.
func Supported(path string) bool {
	return strings.EqualFold(filepath.Ext(path), ".pdf")
}

// engineResult mirrors the CLI's --json document payload.
type engineResult struct {
	PDFType          string `json:"pdfType"`
	PageCount        int    `json:"pageCount"`
	ProcessingTimeMs int    `json:"processingTimeMs"`
	PagesNeedingOCR  []int  `json:"pagesNeedingOcr"`
	Confidence       any    `json:"confidence"`
	IsComplexLayout  bool   `json:"isComplexLayout"`
	PagesWithTables  []int  `json:"pagesWithTables"`
	PagesWithColumns []int  `json:"pagesWithColumns"`
	HasEncodingIsses bool   `json:"hasEncodingIssues"`
	Markdown         string `json:"markdown"`
	Title            string `json:"title"`
}

// Options configures Extract.
type Options struct {
	// Timeout bounds the subprocess (0 = DefaultTimeout).
	Timeout time.Duration
	// MaxBytes caps the returned markdown (0 = MaxMarkdownBytes).
	MaxBytes int
	// SkipVerify disables the per-page cross-check. Off by default and only
	// worth setting when the caller has already established the document is
	// text-based; the check is the difference between "the engine said it read
	// this" and "we confirmed it did".
	SkipVerify bool
}

// Extract reads path and returns its text.
func Extract(ctx context.Context, path string, opts Options) (*Document, error) {
	bin, ok := Available()
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnavailable, InstallHint)
	}
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultTimeout
	}
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = MaxMarkdownBytes
	}
	if fi, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("reading %s: %w (%w)", path, err, ErrBadInput)
	} else if fi.IsDir() {
		return nil, fmt.Errorf("%s is a directory (%w)", path, ErrBadInput)
	}

	res, err := runEngine(ctx, bin, opts.Timeout, path, "--json")
	if err != nil {
		return nil, err
	}

	doc := &Document{
		Path:             path,
		Type:             res.PDFType,
		PageCount:        res.PageCount,
		Title:            strings.TrimSpace(res.Title),
		Markdown:         res.Markdown,
		PagesWithTables:  res.PagesWithTables,
		PagesWithColumns: res.PagesWithColumns,
		EngineMillis:     res.ProcessingTimeMs,
		Complete:         true,
	}
	doc.PagesNeedingOCR = normalizePages(res.PagesNeedingOCR, res.PageCount)
	if doc.PagesNeedingOCR == nil {
		// [] rather than null: this repo's --json contract is an empty list, and
		// a consumer checking `pages_needing_ocr.length` must not hit null.
		doc.PagesNeedingOCR = []int{}
	}

	if res.HasEncodingIsses {
		doc.Note("the engine reported encoding issues, so some characters may be wrong")
	}
	if res.IsComplexLayout {
		doc.Note("complex layout: reading order may not match the visual order")
	}

	// The cross-check. See the package comment: the engine's summary is a
	// judgement, and a page that produced no text was not read whatever the
	// summary says.
	if !opts.SkipVerify {
		if err := doc.verifyPages(ctx, bin, opts.Timeout); err != nil {
			doc.Note("per-page verification could not run (" + err.Error() +
				"), so the OCR list below is the engine's summary alone and has not been confirmed")
		}
	}

	if len(doc.PagesNeedingOCR) > 0 {
		doc.Complete = false
		doc.Note(fmt.Sprintf("%d of %d page(s) produced no text and need OCR: %s",
			len(doc.PagesNeedingOCR), doc.PageCount, formatPages(doc.PagesNeedingOCR)))
	}
	if len(doc.Markdown) > opts.MaxBytes {
		doc.Markdown = truncateAtRune(doc.Markdown, opts.MaxBytes)
		doc.Complete = false
		doc.Note(fmt.Sprintf("the extracted text exceeded %d bytes and was truncated; "+
			"this is not the whole document", opts.MaxBytes))
	}
	if strings.TrimSpace(doc.Markdown) == "" && doc.PageCount > 0 && len(doc.PagesNeedingOCR) == 0 {
		// Nothing extracted, nothing flagged: say so rather than returning an
		// empty string that reads like an empty document.
		doc.Complete = false
		doc.Note("no text was extracted and no page was flagged as needing OCR — " +
			"the document could not be read, and this is not evidence that it is empty")
	}
	return doc, nil
}

// verifyPages confirms that each page the engine did NOT flag actually yields
// text, and folds any that do not into PagesNeedingOCR.
//
// Only unflagged pages are checked, and only when the document is small enough
// that one subprocess per page is affordable. Beyond that a density heuristic
// stands in, and the note says which was used -- an unverified result must not
// look like a verified one.
func (d *Document) verifyPages(ctx context.Context, bin string, timeout time.Duration) error {
	if d.PageCount <= 0 {
		return nil
	}
	flagged := map[int]bool{}
	for _, p := range d.PagesNeedingOCR {
		flagged[p] = true
	}
	unflagged := make([]int, 0, d.PageCount)
	for p := 1; p <= d.PageCount; p++ {
		if !flagged[p] {
			unflagged = append(unflagged, p)
		}
	}
	if len(unflagged) == 0 {
		return nil
	}

	if len(unflagged) > maxVerifyPages {
		// Density check instead: if the whole document produced less text than
		// one plausible page, the summary is not believable regardless.
		if len(strings.TrimSpace(d.Markdown)) < minCharsPerPage {
			d.PagesNeedingOCR = normalizePages(unflagged, d.PageCount)
		}
		d.Note(fmt.Sprintf("per-page verification skipped: %d pages is over the %d-page budget; "+
			"the OCR list is the engine's summary plus a whole-document density check",
			d.PageCount, maxVerifyPages))
		return nil
	}

	var empty []int
	for _, p := range unflagged {
		res, err := runEngine(ctx, bin, timeout, d.Path, "--json", "--pages", strconv.Itoa(p))
		if err != nil {
			return err
		}
		if len(strings.TrimSpace(res.Markdown)) < minCharsPerPage {
			empty = append(empty, p)
		}
	}
	if len(empty) > 0 {
		d.PagesNeedingOCR = normalizePages(append(d.PagesNeedingOCR, empty...), d.PageCount)
		d.Note(fmt.Sprintf("verification found %d page(s) the engine did not flag but which "+
			"produced no text: %s", len(empty), formatPages(empty)))
	}
	return nil
}

const (
	// maxVerifyPages bounds the per-page pass. One subprocess per page is
	// ~15ms; past this the wall-clock cost stops being worth the certainty.
	maxVerifyPages = 64
	// minCharsPerPage is the floor below which a page is treated as producing
	// no usable text. Deliberately small: a near-empty page is legitimate, and
	// the cost of a false "needs OCR" is a wasted OCR call, while the cost of a
	// false "read fine" is a silently blank page presented as content.
	minCharsPerPage = 8
)

// Note appends an explanation, skipping duplicates.
func (d *Document) Note(s string) {
	for _, n := range d.Notes {
		if n == s {
			return
		}
	}
	d.Notes = append(d.Notes, s)
}

func runEngine(ctx context.Context, bin string, timeout time.Duration, args ...string) (*engineResult, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if cctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("pdf-inspector timed out after %s", timeout)
		}
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("pdf-inspector failed: %s", firstLine(msg))
	}
	var res engineResult
	if err := json.Unmarshal(stdout.Bytes(), &res); err != nil {
		return nil, fmt.Errorf("pdf-inspector returned output that is not JSON: %w", err)
	}
	return &res, nil
}

// normalizePages returns a sorted, de-duplicated, 1-based page list with
// out-of-range entries dropped.
//
// The engine's own APIs disagree about indexing -- the document summary is
// 1-based while the per-page API is 0-based -- so every list is funnelled
// through here. An off-by-one in "which page is unreadable" is the kind of wrong
// that looks right.
func normalizePages(in []int, pageCount int) []int {
	if len(in) == 0 {
		return nil
	}
	seen := map[int]bool{}
	out := make([]int, 0, len(in))
	for _, p := range in {
		if p < 1 {
			continue // a 0 can only be a 0-based leak; dropping beats guessing
		}
		if pageCount > 0 && p > pageCount {
			continue
		}
		if seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	sort.Ints(out)
	if len(out) == 0 {
		return nil
	}
	return out
}

// formatPages renders a page list compactly (1-3, 7).
func formatPages(pages []int) string {
	if len(pages) == 0 {
		return "none"
	}
	sorted := append([]int(nil), pages...)
	sort.Ints(sorted)
	var parts []string
	start, prev := sorted[0], sorted[0]
	flush := func() {
		switch {
		case start == prev:
			parts = append(parts, strconv.Itoa(start))
		case prev == start+1:
			parts = append(parts, strconv.Itoa(start), strconv.Itoa(prev))
		default:
			parts = append(parts, fmt.Sprintf("%d-%d", start, prev))
		}
	}
	for _, p := range sorted[1:] {
		if p == prev+1 {
			prev = p
			continue
		}
		flush()
		start, prev = p, p
	}
	flush()
	return strings.Join(parts, ", ")
}

// truncateAtRune cuts s to at most n bytes without splitting a character.
func truncateAtRune(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xc0 == 0x80 {
		n--
	}
	return s[:n]
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
