# PRD-133: Document Ingestion — Stop Feeding the Model Binary, Then Parse PDFs Properly

> **Stack: both.** The Tier 0 fix is Go (`internal/tool`). The extraction capability lands Python-first because that is the only distribution where a good engine is actually reachable — §5 explains why, and it is not a preference.

> **Scope boundary.** This is about getting *document text* into a session. It is not about retrieval (PRD-024 repo-map, PRD-025 semantic memory), not about the context window budget (PRD-039), and not about OCR — this PRD deliberately stops at *routing to* OCR rather than performing it.

**Status:** Proposed
**Priority:** P1 High (Tier 0 sub-item is P0 — it is a live no-fake-success violation)
**Estimated Effort:** S for Tier 0, M for Tier 1, M for Tier 2
**Category:** Core / DX
**Affects:** `tag-go/internal/tool` (`read_file` binary refusal), `src/tag` (new document-loading path), `pyproject.toml` (new optional extra), `tag-go/internal/ciauto`+`internal/security` (both already skip `.pdf` — they become consistent with a real answer rather than a silent skip)
**Depends on:** — (Tier 0 depends on nothing)
**Inspired by:** [firecrawl/pdf-inspector](https://github.com/firecrawl/pdf-inspector) (MIT, Rust, 6.4k stars, created 2026-02)

---

## 1. Overview

TAG has **no document ingestion path at all**. There is no PDF handling anywhere in either distribution. The two places `.pdf` appears in the Go harness are both skip-lists: `internal/diffcontext` treats it as binary and `internal/security/scan.go` excludes it from scanning.

That would be a defensible gap on its own. What makes it a bug is what happens when a user asks the obvious thing — *"read spec.pdf and implement it"* — because `read_file` does not know it is a gap:

```go
b, err := io.ReadAll(io.LimitReader(f, opts.MaxReadBytes))
if err != nil {
    return "", err
}
return string(b), nil
```

It returns the raw bytes as a Go string. No detection, no error, no warning. Measured on a real 896 KB, 5-page PDF from this machine:

| | what `read_file` hands the model today |
|---|---|
| bytes returned | 262,144 (capped at 256 KiB) |
| share of the document | **29.3%** — silently truncated mid-stream |
| printable ASCII | **38.2%** |
| bytes that are not valid UTF-8 | **102,799** |
| rough token cost | **~83,000–125,000 tokens** |
| usable content | none |

So the model is handed ~100k tokens of deflate output, a third of a document, with nothing anywhere saying so — and it will then answer questions about "the spec". This is the fabricated-success pattern the August 2026 audit spent nineteen PRs removing from `swarm`, `loop` and `agentic-ci`, still live in the most-used tool in the harness.

Fixing *that* costs about twenty lines and depends on nothing. Everything else in this PRD is the follow-on capability question.

---

## 2. Problem Statement

### 2.1 `read_file` reports success on content it cannot read

The tool's contract is "read a text file". Handed a PDF, an image or a `.zip`, it returns bytes and exit-success. A tool that cannot do the thing must say so; this one says nothing and returns garbage that *looks* like content. It is also the expensive kind of wrong: the failure consumes six figures of tokens before the model discovers it has nothing.

### 2.2 The truncation is invisible

256 KiB of a 896 KB file is 29% of it. Even if the format were text, silently returning a third of a document and reporting success is the "report the achieved guarantee, not the attempted one" rule broken.

### 2.3 There is no path for the ordinary request

"Summarise this PDF", "implement what the RFC says", "extract the table from page 4" are table-stakes for a coding agent. Peers ship this. TAG has no answer, and — worse than having no answer — it has a wrong one.

### 2.4 A scanned PDF and a text PDF are indistinguishable to TAG

Roughly half of real-world PDFs need OCR. TAG cannot tell the two apart, so it cannot even route: it cannot say *"this is a scan, I can't read it, here is what you could do"*. Honest refusal requires classification, and classification is not free (§5.3).

---

## 3. What pdf-inspector Is (verified, not read off the README)

MIT, Rust, ~75k LOC, `lopdf` for the object layer. Created 2026-02-06; 6,407 stars; 30 commits in the last 30 days; PyPI at **0.2.6** while the README still documents `0.1` — young and moving fast.

Verified locally against the same 896 KB / 5-page PDF used above, via `pip install pdf-inspector`:

| | `read_file` today | pdf-inspector |
|---|---|---|
| output | 262 KB of binary | **4,879 chars of clean markdown** |
| coverage | 29% of the file | whole document |
| tokens | ~83k–125k of noise | **~1,200** |
| time | instant, useless | **6 ms** (engine self-reports 10 ms) |
| structure | none | title, page count, per-page tables/columns, `pages_needing_ocr`, `has_encoding_issues`, confidence |

That is a **~70–100× token reduction** and the difference between garbage and a usable document. The first 600 characters were correct markdown with the real title and heading structure.

The published benchmark (0.875 overall, fastest of five engines on a 200-PDF corpus) is **vendor-reported** and is not load-bearing for this PRD. The single-file result above is ours and is.

The genuinely differentiating design choice is `pages_needing_ocr`: per-page OCR routing rather than all-or-nothing. That is the field TAG needs even if it never adopts the extractor, because it is what turns "I can't read this" into "pages 3 and 7 are scans".

---

## 4. Goals and Non-Goals

### Goals

1. `read_file` never hands the model bytes it cannot read, and says why. **No dependency on anything else in this PRD.**
2. A user can get the *text* of a text-based PDF into a session, in at least one distribution.
3. TAG can distinguish text-based from scanned and say which pages need OCR.
4. Every degraded path stays honest: partial extraction is labelled partial, a scan is labelled a scan.

### Non-Goals

- **Performing OCR.** Route to it; do not embed it.
- **Parity with pdf-inspector's extractor in Go.** §5 explains why this is not on the table.
- **Office formats** (`.docx`, `.xlsx`, `.pptx`). Same shape of problem, different engines, separate PRD.
- **Retrieval or chunking.** A 300-page PDF that extracts to 400k tokens is PRD-039's problem, not this one's.

---

## 5. The Integration Question, Answered Honestly

The interesting finding is not "pdf-inspector is good" — it is. It is that **the Go harness has no viable way to use it**, and the reasons are structural rather than a matter of effort.

### 5.1 Python: use it directly

`pyo3` with `abi3-py38`, so one wheel per platform covers every Python version. Wheels published for macOS x86_64 + arm64, manylinux x86_64 + aarch64, win_amd64. **Missing: win_arm64 and musl/Alpine** — those fall back to an sdist build, which needs a Rust toolchain.

It must go in `[project.optional-dependencies]` with lazy install via `tools/lazy_deps.py`, not in core `dependencies`. The scope rule in `pyproject.toml` is explicit — *"only packages used by EVERY hermes session belong here"* — and a PDF parser is not that. The exact-pin discipline (added after the Mini Shai-Hulud worm hit `mistralai` on PyPI) applies: pin `pdf-inspector==0.2.6`, regenerate `uv.lock`. A 6-month-old package on a 0.x line is precisely the kind of dependency that pin discipline exists for.

### 5.2 Go: every in-process path is closed

| path | verdict | why |
|---|---|---|
| CGO → Rust `cdylib` | **closed** | `crate-type = ["lib", "cdylib"]` looks promising, but there is **no `extern "C"` and no `#[no_mangle]` anywhere in `src/`** — the cdylib *is* the pyo3 extension module. There is no C ABI to link against. And CGO would break the CGO-free static binary the project has committed to. |
| WASM via wazero | **closed** | The wasm crate is `wasm-bindgen` + `js-sys` + `serde-wasm-bindgen`, targeting browsers. There is no `wasm32-wasip1` target. wazero runs WASI, not wasm-bindgen's JS glue. This was the one option that would have preserved the static binary; it does not exist. |
| subprocess `pdf2md` | **impractical** | The CLI exists (`pdf2md`, `detect-pdf`, `dump_ops`) and the Go harness already shells out to `gh`, `git`, `pip` and `npm` — so the *pattern* is fine. But the repo has **zero GitHub releases**, so there are no prebuilt binaries. The user would need a Rust toolchain and `cargo install`. That is a hard dependency dressed as an optional one. |
| port the extractor | **rejected** | `extractor/` 10.8k LOC + `markdown/` 8.0k + `tables/` 16.3k + ~24.8k of glyph/CMap tables. Not a port; a rewrite of somebody else's actively-developed product. |

### 5.3 The classifier is smaller — but not as small as it looks, and a naive version is wrong

`detector.rs` is 3,645 LOC. The tempting reading is "the classifier is the cheap 5% — port that". Before recommending it I tested the assumption that a byte-level scan for `Tj`/`TJ` operators can classify a PDF without a real object parser. On the same real file:

```
raw bytes containing 'Tj' or 'TJ':                    93
streams that yield Tj/TJ ONLY after inflate:           5
```

The 93 raw hits are overwhelmingly **coincidental byte pairs inside deflate output**. A naive scan would confidently classify a purely *scanned* PDF as text-based — a false negative on the exact question being asked, in the direction that produces a wrong answer rather than a refusal. Getting this right requires walking the object graph and inflating `FlateDecode` streams, which is precisely the work `detector.rs` spends its 3.6k lines on.

The conclusion is not "port the detector". It is: **the classify step needs a real PDF object layer, so use one that already exists in Go** — `pdfcpu`, `ledongthuc/pdf`, or `gopdf` (pure Go, no CGo, MIT) all do object parsing and stream inflation. Classification on top of any of them is small. Extraction *quality* on top of them is not, and should not be promised.

---

## 6. Recommendation

### Tier 0 — `read_file` refuses binary (P0, ~20 lines, no dependencies)

Detect non-text content and return an error naming the format and the reason, instead of bytes. Detect truncation and say so. Ship this **regardless of every other decision in this PRD**; it is a correctness fix, not a feature, and it is the only item here that is unambiguously right.

The message should be actionable, in the style the rest of the codebase already uses:

> `read_file: doc.pdf is a PDF, not text — 38% of its bytes are unprintable. Reading it would spend ~100k tokens on deflate output. Use the document loader (tag doc read) or convert it first.`

### Tier 1 — Python document loader on pdf-inspector (P1, M)

Optional extra + lazy install. Exposed as a tool the agent can call, returning markdown plus the structural metadata (`page_count`, `pages_with_tables`, `pages_needing_ocr`, `has_encoding_issues`). Scanned pages are **reported, never guessed at**.

### Tier 2 — Go classification via an existing pure-Go PDF library (P2, M)

Enough to answer "text-based or scanned, how many pages, which pages need OCR" and to extract text from straightforwardly text-based documents. Explicitly *not* pdf-inspector-quality layout, reading order or table reconstruction — and the docs must say that rather than implying parity.

### Tier 3 — optional `pdf2md` discovery (P3, S, only if demand appears)

If a user has `pdf2md` on `PATH`, use it and say so. Same shape as the existing `gh` dependency: discovered, optional, never assumed. Not worth building until Tier 1 shows the capability is used.

### Explicitly rejected

- Vendoring or porting the extractor.
- CGO. There is no C ABI, and the static binary is a project commitment.
- WASM. wasm-bindgen is not WASI.
- Making `pdf-inspector` a core Python dependency.

---

## 7. Risks

| risk | mitigation |
|---|---|
| 0.x dependency, 6 months old, API churn (README documents 0.1, PyPI ships 0.2.6) | Exact pin; optional extra; the loader wraps it behind our own interface so a swap is local |
| Supply chain — a new Rust-built wheel in a project that pins exactly *because* of a PyPI worm | Optional extra keeps it out of the default install; pin + `uv.lock` |
| No win_arm64 / musl wheels | Lazy install fails loudly with a clear message on those platforms rather than half-working |
| Capability gap between distributions | Say so in the docs. The Go harness classifying but not extracting is a real limitation and must not be papered over |
| A large PDF extracts to more tokens than the context holds | Out of scope here; must not be silently truncated — Tier 1 reports the size and refuses rather than clipping |

---

## 8. Success Criteria

1. `read_file` on a PDF returns an explaining error, and **zero** bytes of binary reach the model.
2. Truncation at `MaxReadBytes` is always stated.
3. Python: a text-based PDF yields markdown, with page count and OCR routing reported. Verified on real documents, not fixtures.
4. Scanned input is reported as scanned. It is never partially extracted and presented as complete.
5. The Go harness's narrower capability is documented as narrower.
