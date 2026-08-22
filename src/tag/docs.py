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
import sys
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

#: Exact pin for the in-process wheel (PRD-133 Tier 2). A 0.x Rust-built wheel
#: on a fast-moving line is exactly the supply-chain shape the no-ranges policy
#: exists for, so it is pinned here, in the `pdf` extra of pyproject.toml, and in
#: the runtime LAZY_DEPS allowlist — bump all three in lockstep.
PDF_INSPECTOR_PIN = "pdf-inspector==0.2.6"
PDF_INSPECTOR_MODULE = "pdf_inspector"

#: Force a backend for tests / parity checks: "subprocess" | "inprocess".
DOC_BACKEND_ENV = "TAG_DOC_BACKEND"

INSTALL_HINT = (
    "install the in-process engine with `pip install 'tag-agent[pdf]'`, "
    "or the CLI with `npm install -g @firecrawl/pdf-inspector` "
    "(ships a prebuilt binary; no toolchain required), or set "
    f"{ENV_OVERRIDE} to an existing one"
)


class DocumentUnavailable(RuntimeError):
    """The engine is not installed. Distinct so callers can offer the hint."""


class DocumentError(RuntimeError):
    """The engine ran and failed, or returned something unusable."""


class DocumentBadInput(DocumentError):
    """The operator named the wrong path — a missing file or a directory.

    Distinct so the CLI can classify it as a usage error (exit 2), matching the
    Go harness's docs.ErrBadInput and `doc read --help`; a genuine engine
    failure stays a plain DocumentError (exit 1).
    """


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

    Selects the in-process ``pdf_inspector`` wheel when it is importable, else
    the ``pdf-inspector`` subprocess CLI — both produce the identical Document
    (see :func:`_finalize`). Raises :class:`DocumentUnavailable` when neither is
    present, :class:`DocumentBadInput` for a missing file or a directory, and
    :class:`DocumentError` when the engine runs and fails.
    """
    # A non-positive cap means "use the default", matching the Go harness
    # (opts.MaxBytes <= 0 -> MaxMarkdownBytes). Without this, a negative value
    # became a negative slice markdown[:max_bytes] that silently dropped the
    # TAIL of the document while reporting it as "truncated".
    if max_bytes <= 0:
        max_bytes = MAX_MARKDOWN_BYTES
    backend = _select_backend()
    if backend == "":
        raise DocumentUnavailable(f"no document engine is available — {INSTALL_HINT}")
    p = Path(path)
    if not p.exists():
        raise DocumentBadInput(f"{path}: no such file")
    if p.is_dir():
        raise DocumentBadInput(f"{path} is a directory")

    if backend == "inprocess":
        summary, page_md = _inprocess_backend(str(p), timeout)
    else:
        summary, page_md = _subprocess_backend(str(p), timeout)
    return _finalize(str(p), summary, page_md, max_bytes=max_bytes, skip_verify=skip_verify)


def _select_backend() -> str:
    """Return "inprocess", "subprocess", or "" (nothing available).

    ``TAG_DOC_BACKEND`` forces one, for tests and parity checks. Otherwise the
    in-process wheel wins when it is already importable (no Node required), then
    the CLI on PATH.
    """
    forced = (os.environ.get(DOC_BACKEND_ENV) or "").strip().lower()
    if forced in ("subprocess", "inprocess"):
        return forced
    import importlib.util

    if importlib.util.find_spec(PDF_INSPECTOR_MODULE) is not None:
        return "inprocess"
    if engine_path() is not None:
        return "subprocess"
    return ""


def _subprocess_backend(path: str, timeout: float):
    """The CLI backend: a summary dict + a per-page markdown reader."""
    binary = engine_path()
    if binary is None:
        raise DocumentUnavailable(f"pdf-inspector is not installed — {INSTALL_HINT}")
    summary = _run_engine(binary, path, timeout)

    def page_md(page: int) -> str:  # page is 1-based
        return (_run_engine(binary, path, timeout, pages=page).get("markdown") or "")

    return summary, page_md


def _inprocess_backend(path: str, timeout: float):
    """The pyo3 wheel backend, mapped to the SAME summary shape as the CLI.

    Two things, verified against pdf-inspector==0.2.6, shape this:

    * ``extract_pages_markdown`` takes and reports pages **0-based** (its
      per-page ``needs_ocr`` flag and 0-based ``page`` index are the reliable
      signal), whereas ``process_pdf().pages_needing_ocr`` is **unreliable** — it
      flagged every page of a plainly text-based document. So the OCR list is
      derived from the trustworthy per-page flags, converted to the one 1-based
      convention here, NOT from ``process_pdf``.
    * The per-page markdown is read once and cached, so per-page verification
      costs no extra parse (the subprocess backend re-runs the engine per page).
    """
    mod = _ensure_inprocess()
    r = mod.process_pdf(path)  # doc-level metadata + concatenated markdown
    pages = list(getattr(mod.extract_pages_markdown(path), "pages", []) or [])

    md_by_page: dict[int, str] = {}  # 0-based -> markdown
    ocr_1based: list[int] = []
    for pg in pages:
        idx = int(getattr(pg, "page", 0))  # 0-based
        md_by_page[idx] = getattr(pg, "markdown", "") or ""
        if getattr(pg, "needs_ocr", False):
            ocr_1based.append(idx + 1)

    summary = {
        "pdfType": getattr(r, "pdf_type", "") or "",
        "pageCount": int(getattr(r, "page_count", 0) or 0),
        "title": getattr(r, "title", "") or "",
        "markdown": getattr(r, "markdown", "") or "",
        "pagesWithTables": list(getattr(r, "pages_with_tables", []) or []),
        "pagesWithColumns": list(getattr(r, "pages_with_columns", []) or []),
        "processingTimeMs": int(getattr(r, "processing_time_ms", 0) or 0),
        "pagesNeedingOcr": ocr_1based,
        "hasEncodingIssues": bool(getattr(r, "has_encoding_issues", False)),
        "isComplexLayout": bool(getattr(r, "is_complex_layout", False)),
    }

    def page_md(page: int) -> str:  # page is 1-based; cache is 0-based
        return md_by_page.get(page - 1, "")

    return summary, page_md


def _lazy_installs_allowed() -> bool:
    """Auto-install is OPT-IN. A silent network install of a pinned wheel in a
    supply-chain-cautious codebase must be asked for, not assumed."""
    if _truthy(os.environ.get("HERMES_DISABLE_LAZY_INSTALLS")):
        return False
    return _truthy(os.environ.get("TAG_ALLOW_LAZY_INSTALLS"))


def _truthy(v: str | None) -> bool:
    return (v or "").strip().lower() in ("1", "true", "yes", "on")


def _ensure_inprocess():
    """Import the wheel, lazy-installing the exact pin only when explicitly
    allowed. Absent and not allowed → an honest DocumentUnavailable naming the
    manual install, never a silent pip call."""
    import importlib

    try:
        return importlib.import_module(PDF_INSPECTOR_MODULE)
    except ImportError:
        pass
    if not _lazy_installs_allowed():
        raise DocumentUnavailable(
            f"{PDF_INSPECTOR_MODULE} is not installed — run "
            f"`pip install 'tag-agent[pdf]'` (or set TAG_ALLOW_LAZY_INSTALLS=1 "
            f"to auto-install {PDF_INSPECTOR_PIN})"
        )
    # Exact-pin discipline: only ever install the one pinned spec.
    subprocess.run(
        [sys.executable, "-m", "pip", "install", PDF_INSPECTOR_PIN],
        check=True,
        capture_output=True,
        text=True,
        timeout=300,
    )
    return importlib.import_module(PDF_INSPECTOR_MODULE)


def _finalize(
    path: str,
    summary: dict,
    page_md,
    *,
    max_bytes: int,
    skip_verify: bool,
) -> Document:
    """Turn a backend summary into a Document, applying the honesty logic that
    is identical for both backends: per-page verification, OCR/truncation/empty
    notes, and the 1-place page normalisation."""
    doc = Document(
        path=path,
        type=summary.get("pdfType", ""),
        page_count=int(summary.get("pageCount") or 0),
        title=(summary.get("title") or "").strip(),
        markdown=summary.get("markdown") or "",
        pages_with_tables=summary.get("pagesWithTables") or [],
        pages_with_columns=summary.get("pagesWithColumns") or [],
        engine_ms=int(summary.get("processingTimeMs") or 0),
    )
    doc.pages_needing_ocr = normalize_pages(summary.get("pagesNeedingOcr"), doc.page_count)

    if summary.get("hasEncodingIssues"):
        doc.note("the engine reported encoding issues, so some characters may be wrong")
    if summary.get("isComplexLayout"):
        doc.note("complex layout: reading order may not match the visual order")

    if not skip_verify:
        _verify_pages(doc, page_md)

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


def _verify_pages(doc: Document, page_md) -> None:
    """Confirm each unflagged page actually yields text.

    The engine's document-level OCR list is a threshold judgement, not a fact. A
    page that produced no text was not read, whatever the summary said — and
    handing back a document with silently blank pages, labelled complete, is the
    pattern this whole feature exists to avoid. ``page_md`` is the backend's
    1-based per-page markdown reader.
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
            md = page_md(page)
        except DocumentError as exc:
            doc.note(
                f"per-page verification could not run ({exc}), so the OCR list is the "
                "engine's summary alone and has not been confirmed"
            )
            return
        if len((md or "").strip()) < MIN_CHARS_PER_PAGE:
            empty.append(page)
    if empty:
        doc.pages_needing_ocr = normalize_pages(doc.pages_needing_ocr + empty, doc.page_count)
        doc.note(
            f"verification found {len(empty)} page(s) the engine did not flag but which "
            f"produced no text: {format_pages(empty)}"
        )
