"""PRD-133 Tier 2 — the in-process pdf-inspector backend.

The real wheel is an optional extra and is not installed in CI, so these tests
inject a FAKE `pdf_inspector` module (mirroring the real 0.2.6 API shape) and
force the in-process backend. They pin the contract this code controls: the
0-based per-page flags become the 1-based OCR list, verification cross-checks
actual per-page text, and lazy-install is opt-in.
"""

from __future__ import annotations

import sys
import tomllib
from pathlib import Path
from types import SimpleNamespace

import pytest

from tag import docs


def _fake_module(*, meta: dict, pages: list[dict]):
    """Build a fake pdf_inspector module.

    meta -> attrs of process_pdf()'s result. pages -> a list of dicts with
    page (0-based), markdown, needs_ocr for extract_pages_markdown().pages.
    """
    mod = SimpleNamespace()

    def process_pdf(_path):
        return SimpleNamespace(**meta)

    def extract_pages_markdown(_path, pages_arg=None, **_kw):
        objs = [SimpleNamespace(page=p["page"], markdown=p["markdown"], needs_ocr=p.get("needs_ocr", False), ocr_reason=None) for p in pages]
        return SimpleNamespace(pages=objs)

    mod.process_pdf = process_pdf
    mod.extract_pages_markdown = extract_pages_markdown
    return mod


@pytest.fixture
def inprocess(monkeypatch, tmp_path):
    """Force the in-process backend with a fake module, over a real file."""
    def install(meta, pages):
        monkeypatch.setitem(sys.modules, "pdf_inspector", _fake_module(meta=meta, pages=pages))
        monkeypatch.setenv(docs.DOC_BACKEND_ENV, "inprocess")
        f = tmp_path / "doc.pdf"
        f.write_bytes(b"%PDF-1.4 fake")
        return f

    return install


def _meta(**kw):
    base = dict(pdf_type="text_based", page_count=0, title="", markdown="",
                pages_with_tables=[], pages_with_columns=[], processing_time_ms=1,
                pages_needing_ocr=[], has_encoding_issues=False, is_complex_layout=False)
    base.update(kw)
    return base


def test_inprocess_reads_a_clean_document(inprocess):
    f = inprocess(_meta(page_count=2, markdown="full document markdown here"),
                  [{"page": 0, "markdown": "real text one here"},
                   {"page": 1, "markdown": "real text two here"}])
    d = docs.extract(f)
    assert d.complete is True
    assert d.pages_needing_ocr == []
    # The full markdown comes from process_pdf; per-page text is for verification.
    assert d.markdown == "full document markdown here"


def test_inprocess_respects_per_page_needs_ocr_flag(inprocess):
    # Page 1 (0-based 0) is flagged needs_ocr -> 1-based [1]; page 2 has text.
    f = inprocess(_meta(page_count=2, markdown="only page two"),
                  [{"page": 0, "markdown": "", "needs_ocr": True},
                   {"page": 1, "markdown": "genuine text on page two"}])
    d = docs.extract(f)
    assert d.pages_needing_ocr == [1]
    assert d.complete is False


def test_inprocess_verification_catches_an_unflagged_empty_page(inprocess):
    # No page is FLAGGED, but page 2 (0-based 1) produced no text: verification
    # must catch it, or a silently blank page ships labelled complete.
    f = inprocess(_meta(page_count=3, markdown="p1 ... p3"),
                  [{"page": 0, "markdown": "real text page one"},
                   {"page": 1, "markdown": ""},
                   {"page": 2, "markdown": "real text page three"}])
    d = docs.extract(f)
    assert d.pages_needing_ocr == [2]
    assert d.complete is False
    assert any("did not flag" in n for n in d.notes)


def test_inprocess_missing_file_is_bad_input(monkeypatch, tmp_path):
    monkeypatch.setitem(sys.modules, "pdf_inspector", _fake_module(meta=_meta(), pages=[]))
    monkeypatch.setenv(docs.DOC_BACKEND_ENV, "inprocess")
    with pytest.raises(docs.DocumentBadInput):
        docs.extract(tmp_path / "nope.pdf")


def test_lazy_install_is_opt_in(monkeypatch, tmp_path):
    # Module absent + not allowed -> DocumentUnavailable naming the pip extra,
    # and NO pip subprocess is ever spawned.
    monkeypatch.delitem(sys.modules, "pdf_inspector", raising=False)
    monkeypatch.setenv(docs.DOC_BACKEND_ENV, "inprocess")
    monkeypatch.delenv("TAG_ALLOW_LAZY_INSTALLS", raising=False)

    import importlib
    real_import = importlib.import_module

    def no_pdf(name, *a, **k):
        if name == "pdf_inspector":
            raise ImportError("no pdf_inspector")
        return real_import(name, *a, **k)

    monkeypatch.setattr(importlib, "import_module", no_pdf)

    def boom(*a, **k):  # pragma: no cover - must never be called
        raise AssertionError("lazy install must not run when not allowed")

    monkeypatch.setattr(docs.subprocess, "run", boom)

    f = tmp_path / "doc.pdf"
    f.write_bytes(b"%PDF-1.4 fake")
    with pytest.raises(docs.DocumentUnavailable) as exc:
        docs.extract(f)
    assert "tag-agent[pdf]" in str(exc.value)


def test_pin_parity_across_constant_and_pyproject():
    # The exact pin must agree between the code constant and the pyproject extra.
    root = Path(__file__).resolve().parent.parent
    data = tomllib.loads((root / "pyproject.toml").read_text())
    extras = data["project"]["optional-dependencies"]
    assert extras["pdf"] == [docs.PDF_INSPECTOR_PIN]
    # And it must NOT be in the [all] convenience extra (supply-chain policy).
    assert not any("pdf-inspector" in dep for dep in extras.get("all", []))


def _doc_args(**kw):
    import argparse
    ns = argparse.Namespace(doc_subcommand="read", file=None, max_bytes=0,
                            skip_verify=False, json=False)
    for k, v in kw.items():
        setattr(ns, k, v)
    return ns


def test_cmd_doc_json_error_path_and_exit_codes(monkeypatch, tmp_path, capsys):
    """Python parity for ISSUE-001/005: --json error path emits JSON on stdout;
    missing file / directory exit 2; a genuine engine failure exits 1."""
    import json as _json

    from tag.cmd.prd_clusters import cmd_doc

    # Stub the engine so extract() reaches its path checks.
    monkeypatch.setenv("TAG_PDF_INSPECTOR", "/bin/echo")
    monkeypatch.delenv("TAG_DOC_BACKEND", raising=False)
    monkeypatch.delitem(sys.modules, "pdf_inspector", raising=False)

    # ISSUE-001: --json on a missing file -> parseable {"error": ...} on stdout.
    code = cmd_doc(_doc_args(file=str(tmp_path / "nope.pdf"), json=True))
    out = capsys.readouterr().out
    assert code == 2
    assert _json.loads(out)["error"]

    # ISSUE-005: a directory is a usage error (exit 2).
    assert cmd_doc(_doc_args(file=str(tmp_path))) == 2

    # A genuine engine failure (echo returns non-JSON) stays exit 1.
    real = tmp_path / "real.pdf"
    real.write_bytes(b"%PDF-1.4 fake")
    assert cmd_doc(_doc_args(file=str(real))) == 1
