"""Document ingestion — turn documents the model cannot read into text it can.

Shells out to firecrawl/pdf-inspector, discovered on PATH exactly the way ``gh``
already is: used when present, never assumed, and its absence reported as an
absence rather than a failure.

Why a subprocess and not the PyPI binding. One integration then serves BOTH
distributions — the Go harness cannot use the Rust library in-process (no C ABI,
and its wasm build is wasm-bindgen rather than WASI) but it can run the CLI the
npm package ships. Two implementations of "which pages need OCR" would be two
answers to the same question, and that is the incoherence this deliberately
avoids. See docs/prd/PRD-133.

Mirrors ``tag-go/internal/docs``; keep the two in step.
"""
from __future__ import annotations

import json
import os
import shutil
import subprocess
from dataclasses import dataclass, field
from pathlib import Path

BINARY_NAME = "pdf-inspector"
ENV_OVERRIDE = "TAG_PDF_INSPECTOR"

#: Bounds one extraction. A path can arrive from a webhook, so the subprocess
#: gets a deadline rather than the caller's patience.
DEFAULT_TIMEOUT = 60.0

#: Caps what an extraction may return. A large PDF extracts to megabytes, and
#: every byte lands in a context window — the size of the result is chosen by
#: the input, so it has to be chosen by us. Exceeding it truncates and SAYS SO.
MAX_MARKDOWN_BYTES = 1 << 20

#: Below this a page is treated as producing no usable text. Deliberately small:
#: a false "needs OCR" costs a wasted OCR call, a false "read fine" costs a
#: silently blank page presented as content.
MIN_CHARS_PER_PAGE = 8

#: Per-page verification budget. One subprocess per page is ~15ms; past this the
#: wall-clock cost stops being worth the certainty.
MAX_VERIFY_PAGES = 64

INSTALL_HINT = (
    "install it with `npm install -g @firecrawl/pdf-inspector` "
    "(ships a prebuilt binary; no toolchain required), or set "
    f"{ENV_OVERRIDE} to an existing one"
)


class DocumentUnavailable(RuntimeError):
    """The engine is not installed. Distinct so callers can offer the hint."""


class DocumentError(RuntimeError):
    """The engine ran and failed, or returned something unusable."""


@dataclass
class Document:
    path: str
    type: str = ""
    page_count: int = 0
    title: str = ""
    markdown: str = ""
    #: 1-BASED and reconciled — the union of what the engine reported and what
    #: verification found empty. Normalised in one place because the engine's
    #: own APIs disagree about indexing, and an off-by-one in "which page is
    #: unreadable" is the kind of wrong that reads as right.
    pages_needing_ocr: list[int] = field(default_factory=list)
    pages_with_tables: list[int] = field(default_factory=list)
    pages_with_columns: list[int] = field(default_factory=list)
    #: False when any page could not be read, or the text was truncated.
    complete: bool = True
    notes: list[str] = field(default_factory=list)
    engine_ms: int = 0

    def note(self, text: str) -> None:
        if text not in self.notes:
            self.notes.append(text)

    def to_dict(self) -> dict:
        return {
            "path": self.path,
            "type": self.type,
            "page_count": self.page_count,
            "title": self.title,
            "markdown": self.markdown,
            "pages_needing_ocr": self.pages_needing_ocr,
            "pages_with_tables": self.pages_with_tables,
            "pages_with_columns": self.pages_with_columns,
            "complete": self.complete,
            "notes": self.notes,
            "engine_ms": self.engine_ms,
        }


def engine_path() -> str | None:
    """Return the engine's path, or None when it is not installed."""
    override = (os.environ.get(ENV_OVERRIDE) or "").strip()
    if override:
        # An override that does not resolve is an operator mistake worth failing
        # on, not a reason to silently use a different binary from PATH.
        return override if Path(override).is_file() else None
    return shutil.which(BINARY_NAME)


def available() -> bool:
    return engine_path() is not None


def supported(path: str | Path) -> bool:
    """Routing hint by extension. Not a guarantee."""
    return str(path).lower().endswith(".pdf")


def normalize_pages(pages, page_count: int) -> list[int]:
    """Sorted, de-duplicated, 1-based, in-range."""
    out: set[int] = set()
    for p in pages or []:
        if not isinstance(p, int) or p < 1:
            # A 0 can only be a 0-based leak; dropping beats guessing.
            continue
        if page_count > 0 and p > page_count:
            continue
        out.add(p)
    return sorted(out)


def format_pages(pages: list[int]) -> str:
    """Render a page list compactly (1-3, 7)."""
    if not pages:
        return "none"
    pages = sorted(pages)
    parts: list[str] = []
    start = prev = pages[0]

    def flush() -> None:
        if start == prev:
            parts.append(str(start))
        elif prev == start + 1:
            parts.extend([str(start), str(prev)])
        else:
            parts.append(f"{start}-{prev}")

    for p in pages[1:]:
        if p == prev + 1:
            prev = p
            continue
        flush()
        start = prev = p
    flush()
    return ", ".join(parts)


def _run_engine(binary: str, path: str, timeout: float, pages: int | None = None) -> dict:
    args = [binary, path, "--json"]
    if pages is not None:
        args += ["--pages", str(pages)]
    try:
        proc = subprocess.run(args, capture_output=True, text=True, timeout=timeout)
    except subprocess.TimeoutExpired:
        raise DocumentError(f"pdf-inspector timed out after {timeout:g}s") from None
    if proc.returncode != 0:
        msg = (proc.stderr or proc.stdout or "").strip().splitlines()
        raise DocumentError(f"pdf-inspector failed: {msg[0] if msg else proc.returncode}")
    try:
        return json.loads(proc.stdout)
    except json.JSONDecodeError as exc:
        raise DocumentError(f"pdf-inspector returned output that is not JSON: {exc}") from exc


def extract(
    path: str | Path,
    *,
    timeout: float = DEFAULT_TIMEOUT,
    max_bytes: int = MAX_MARKDOWN_BYTES,
    skip_verify: bool = False,
) -> Document:
    """Read *path* and return its text.

    Raises :class:`DocumentUnavailable` when the engine is absent and
    :class:`DocumentError` when it runs and fails.
    """
    binary = engine_path()
    if binary is None:
        raise DocumentUnavailable(f"pdf-inspector is not installed — {INSTALL_HINT}")
    p = Path(path)
    if not p.exists():
        raise DocumentError(f"{path}: no such file")
    if p.is_dir():
        raise DocumentError(f"{path} is a directory")

    res = _run_engine(binary, str(p), timeout)
    doc = Document(
        path=str(p),
        type=res.get("pdfType", ""),
        page_count=int(res.get("pageCount") or 0),
        title=(res.get("title") or "").strip(),
        markdown=res.get("markdown") or "",
        pages_with_tables=res.get("pagesWithTables") or [],
        pages_with_columns=res.get("pagesWithColumns") or [],
        engine_ms=int(res.get("processingTimeMs") or 0),
    )
    doc.pages_needing_ocr = normalize_pages(res.get("pagesNeedingOcr"), doc.page_count)

    if res.get("hasEncodingIssues"):
        doc.note("the engine reported encoding issues, so some characters may be wrong")
    if res.get("isComplexLayout"):
        doc.note("complex layout: reading order may not match the visual order")

    if not skip_verify:
        _verify_pages(doc, binary, timeout)

    if doc.pages_needing_ocr:
        doc.complete = False
        doc.note(
            f"{len(doc.pages_needing_ocr)} of {doc.page_count} page(s) produced no text "
            f"and need OCR: {format_pages(doc.pages_needing_ocr)}"
        )
    if len(doc.markdown.encode("utf-8")) > max_bytes:
        doc.markdown = doc.markdown.encode("utf-8")[:max_bytes].decode("utf-8", "ignore")
        doc.complete = False
        doc.note(
            f"the extracted text exceeded {max_bytes} bytes and was truncated; "
            "this is not the whole document"
        )
    if not doc.markdown.strip() and doc.page_count > 0 and not doc.pages_needing_ocr:
        doc.complete = False
        doc.note(
            "no text was extracted and no page was flagged as needing OCR — the document "
            "could not be read, and this is not evidence that it is empty"
        )
    return doc


def _verify_pages(doc: Document, binary: str, timeout: float) -> None:
    """Confirm each unflagged page actually yields text.

    The engine's document-level OCR list is a threshold judgement, not a fact. A
    page that produced no text was not read, whatever the summary said — and
    handing back a document with silently blank pages, labelled complete, is the
    pattern this whole feature exists to avoid.
    """
    if doc.page_count <= 0:
        return
    flagged = set(doc.pages_needing_ocr)
    unflagged = [p for p in range(1, doc.page_count + 1) if p not in flagged]
    if not unflagged:
        return

    if len(unflagged) > MAX_VERIFY_PAGES:
        # Density check instead: if the whole document produced less text than
        # one plausible page, the summary is not believable regardless.
        if len(doc.markdown.strip()) < MIN_CHARS_PER_PAGE:
            doc.pages_needing_ocr = normalize_pages(unflagged, doc.page_count)
        doc.note(
            f"per-page verification skipped: {doc.page_count} pages is over the "
            f"{MAX_VERIFY_PAGES}-page budget; the OCR list is the engine's summary "
            "plus a whole-document density check"
        )
        return

    empty: list[int] = []
    for page in unflagged:
        try:
            res = _run_engine(binary, doc.path, timeout, pages=page)
        except DocumentError as exc:
            doc.note(
                f"per-page verification could not run ({exc}), so the OCR list is the "
                "engine's summary alone and has not been confirmed"
            )
            return
        if len((res.get("markdown") or "").strip()) < MIN_CHARS_PER_PAGE:
            empty.append(page)
    if empty:
        doc.pages_needing_ocr = normalize_pages(doc.pages_needing_ocr + empty, doc.page_count)
        doc.note(
            f"verification found {len(empty)} page(s) the engine did not flag but which "
            f"produced no text: {format_pages(empty)}"
        )
