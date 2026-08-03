"""Document ingestion (PRD-133), Python side.

Mirrors tag-go/internal/docs/docs_test.go. A fake engine keeps the suite
independent of whether pdf-inspector is installed.
"""
from __future__ import annotations

import json
import os
import stat
import sys
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "src"))

from tag import docs  # noqa: E402


def fake_engine(tmp_path, script: dict[str, dict], monkeypatch):
    """Write a stub pdf-inspector replying from a scripted table.

    Keys are the --pages value ("" for the whole-document call).
    """
    table = {k: json.dumps(v) for k, v in script.items()}
    cases = "\n".join(
        f"  {k!r}) printf '%s' {shell_quote(v)} ;;".replace("'" + k + "'", '"' + k + '"')
        for k, v in table.items()
    )
    body = f"""#!/bin/sh
pages=""
prev=""
for a in "$@"; do
  if [ "$prev" = "--pages" ]; then pages="$a"; fi
  prev="$a"
done
case "$pages" in
{cases}
  *) echo "no scripted reply for pages=$pages" >&2; exit 1 ;;
esac
"""
    p = tmp_path / docs.BINARY_NAME
    p.write_text(body)
    p.chmod(p.stat().st_mode | stat.S_IEXEC)
    monkeypatch.setenv(docs.ENV_OVERRIDE, str(p))
    return p


def shell_quote(s: str) -> str:
    return "'" + s.replace("'", "'\\''") + "'"


@pytest.fixture
def pdf(tmp_path):
    p = tmp_path / "doc.pdf"
    p.write_bytes(b"%PDF-1.4\n")
    return p


def test_absence_is_reported_as_absence(tmp_path, monkeypatch):
    monkeypatch.setenv(docs.ENV_OVERRIDE, str(tmp_path / "missing"))
    assert not docs.available()
    with pytest.raises(docs.DocumentUnavailable) as exc:
        docs.extract("x.pdf")
    assert "npm install" in str(exc.value), "the error must name the fix"


def test_clean_document_is_complete_and_quiet(tmp_path, monkeypatch, pdf):
    fake_engine(tmp_path, {
        "": {"pdfType": "TextBased", "pageCount": 2, "title": "Spec",
             "markdown": "# Spec\n\nbody text long enough to count"},
        "1": {"markdown": "page one has real text"},
        "2": {"markdown": "page two has real text"},
    }, monkeypatch)
    d = docs.extract(pdf)
    assert d.complete and d.notes == [], f"a clean read must be quiet: {d.notes}"
    assert d.title == "Spec" and d.page_count == 2


def test_verification_catches_pages_the_engine_missed(tmp_path, monkeypatch, pdf):
    """The headline behaviour: the engine says all pages are fine, one is blank."""
    fake_engine(tmp_path, {
        "": {"pdfType": "TextBased", "pageCount": 3, "pagesNeedingOcr": [],
             "markdown": "# Doc\n\ntext from pages one and three"},
        "1": {"markdown": "real text on page one"},
        "2": {"markdown": "   "},
        "3": {"markdown": "real text on page three"},
    }, monkeypatch)
    d = docs.extract(pdf)
    assert d.pages_needing_ocr == [2], f"page 2 produced no text: {d.pages_needing_ocr}"
    assert not d.complete
    assert any("did not flag" in n for n in d.notes)


def test_oversized_extraction_is_truncated_and_says_so(tmp_path, monkeypatch, pdf):
    big = "word " * 5000
    fake_engine(tmp_path, {
        "": {"pdfType": "TextBased", "pageCount": 1, "markdown": big},
        "1": {"markdown": big},
    }, monkeypatch)
    d = docs.extract(pdf, max_bytes=500)
    assert len(d.markdown.encode()) <= 500
    assert not d.complete
    assert any("truncated" in n for n in d.notes)


def test_empty_extraction_is_not_an_empty_document(tmp_path, monkeypatch, pdf):
    fake_engine(tmp_path, {
        "": {"pdfType": "TextBased", "pageCount": 1, "markdown": ""},
        "1": {"markdown": ""},
    }, monkeypatch)
    d = docs.extract(pdf)
    assert not d.complete
    joined = " ".join(d.notes)
    assert "OCR" in joined or "not evidence that it is empty" in joined


def test_scanned_document_is_reported(tmp_path, monkeypatch, pdf):
    fake_engine(tmp_path, {
        "": {"pdfType": "Scanned", "pageCount": 2, "pagesNeedingOcr": [1, 2], "markdown": ""},
        "1": {"markdown": ""}, "2": {"markdown": ""},
    }, monkeypatch)
    d = docs.extract(pdf)
    assert d.type == "Scanned" and not d.complete
    assert d.pages_needing_ocr == [1, 2]


def test_engine_failure_is_an_error(tmp_path, monkeypatch, pdf):
    p = tmp_path / docs.BINARY_NAME
    p.write_text("#!/bin/sh\necho 'boom: bad xref' >&2\nexit 1\n")
    p.chmod(p.stat().st_mode | stat.S_IEXEC)
    monkeypatch.setenv(docs.ENV_OVERRIDE, str(p))
    with pytest.raises(docs.DocumentError) as exc:
        docs.extract(pdf)
    assert "boom" in str(exc.value)


def test_non_json_output_is_an_error(tmp_path, monkeypatch, pdf):
    p = tmp_path / docs.BINARY_NAME
    p.write_text("#!/bin/sh\necho not json\n")
    p.chmod(p.stat().st_mode | stat.S_IEXEC)
    monkeypatch.setenv(docs.ENV_OVERRIDE, str(p))
    with pytest.raises(docs.DocumentError) as exc:
        docs.extract(pdf)
    assert "not JSON" in str(exc.value)


def test_missing_file_errors_before_the_engine_runs(tmp_path, monkeypatch):
    fake_engine(tmp_path, {"": {}}, monkeypatch)
    with pytest.raises(docs.DocumentError):
        docs.extract(tmp_path / "nope.pdf")


# --- parity with the Go implementation ---------------------------------------

def test_page_indexing_is_normalised():
    assert docs.normalize_pages([3, 1, 3, 0, 99, -2, 2], 5) == [1, 2, 3]
    assert docs.normalize_pages(None, 5) == []


@pytest.mark.parametrize("pages,want", [
    ([1, 2, 3, 7], "1-3, 7"),
    ([4], "4"),
    ([1, 2], "1, 2"),
    ([9, 1, 2, 3, 4, 5, 6], "1-6, 9"),
    ([], "none"),
])
def test_format_pages_matches_go(pages, want):
    assert docs.format_pages(pages) == want


def test_supported_is_extension_based():
    assert docs.supported("a/B.PDF") and docs.supported("x.pdf")
    assert not docs.supported("notes.txt") and not docs.supported("x.docx")


def test_constants_match_the_go_harness():
    """Two implementations of the same contract must agree on its numbers."""
    go = (ROOT / "tag-go" / "internal" / "docs" / "docs.go").read_text()
    assert "MaxMarkdownBytes = 1 << 20" in go
    assert docs.MAX_MARKDOWN_BYTES == 1 << 20
    assert "minCharsPerPage = 8" in go
    assert docs.MIN_CHARS_PER_PAGE == 8
    assert "maxVerifyPages = 64" in go
    assert docs.MAX_VERIFY_PAGES == 64
