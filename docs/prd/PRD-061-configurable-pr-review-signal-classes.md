# PRD-061: Configurable PR Review Signal Classes (`tag ci review --signals`)

**Status:** Proposed
**Priority:** P3
**Estimated Effort:** S (3-5 days)
**Category:** CI/CD & Agentic Dev Workflows
**Affects:** `ci.py`
**Depends on:** PRD-020 (CI/CD Integration), PRD-027 (Eval Framework), PRD-028 (Sandbox Code Execution), PRD-013 (Agent Tracing & Observability), PRD-034 (Secret Scanning)
**Inspired by:** CodeRabbit review config, Reviewpad, Danger.js
**GitHub Issue:** #344

---

## 1. Overview

The existing `tag ci review` command (from PRD-020) runs a monolithic LLM review pass over a PR diff, producing a general-purpose review comment that covers whatever the model happens to notice. This approach is sufficient for individual developers running ad-hoc reviews, but it fails in two important ways once teams start using it systematically: first, the review scope is unpredictable and uncontrollable — a security-focused team cannot guarantee that every security pattern gets checked on every PR, and a frontend team cannot suppress irrelevant backend performance warnings; second, there is no repo-level contract defining what review coverage the team expects, which means CI enforcement becomes impossible.

Configurable PR Review Signal Classes addresses both problems by introducing a structured taxonomy of six review signal classes — `security`, `coverage`, `style`, `correctness`, `performance`, and `accessibility` — and letting teams configure, per repository, which classes are active, what their threshold and severity settings are, and which file paths they apply to. The configuration is stored in a `.tag-review.yaml` file checked into the repository root, making it versionable, diff-able, and auditable alongside the code it governs. This mirrors how tools like CodeRabbit use `.coderabbit.yaml`, Reviewpad uses `reviewpad.yml`, and Danger.js uses `Dangerfile` for per-repo behavior customization.

The feature adds a `--signals` flag to `tag ci review --pr` that allows one-time override of the active signal classes for a single run without modifying the persisted config. It also adds a `tag ci review init` subcommand that scaffolds a `.tag-review.yaml` file with sensible defaults based on the detected project type (Python, TypeScript, Go, Rust, etc.). When a `.tag-review.yaml` file is present in the repository root, `tag ci review` automatically loads it; when it is absent, the command falls back to the original monolithic review behavior to preserve backward compatibility.

Signal class activation modifies the LLM system prompt injected into `build_review_prompt()` in `ci.py`, adding class-specific instruction blocks that focus the model's attention on precisely the patterns that matter for each class. Each class maps to a distinct prompt segment, a distinct set of file-path glob filters (e.g., `accessibility` only fires on `.tsx`, `.jsx`, `.html`, `.css` files), and a distinct severity vocabulary that drives the structured output format. Review findings are returned as structured JSON, stored in the `review_findings` SQLite table introduced by this PRD, and then rendered as inline GitHub PR comments via the existing `post_pr_review_comments()` function.

The design is intentionally narrow in scope: this PRD is classified Difficulty 2/5 because it composes entirely on top of existing infrastructure in `ci.py` and `controller.py`. It does not add a new database schema that other subsystems depend on, does not introduce new external API dependencies, and does not modify the agent loop. The primary complexity is in the prompt engineering for each signal class, the YAML config schema design, and the path-filtering logic that routes diff hunks to the correct signal classes before the LLM call.

---

## 2. Problem Statement

### 2.1 Uncontrollable and Unpredictable Review Scope

The current `tag ci review --pr <N>` command sends the entire PR diff plus a generic system prompt to the LLM reviewer. The system prompt (in `ci.py:_REVIEW_SYSTEM`) instructs the model to cover "potential bugs or correctness issues", "style, maintainability, and performance suggestions", and produce an "overall recommendation". In practice, the model's coverage is a function of what happens to be salient in the diff — a 50-line security-critical authentication change in a PR that also changes CSS padding will likely see the model spend tokens on the visual layout rather than the authentication logic.

Teams that have adopted `tag ci review` for CI gating have no mechanism to say "always check for SQL injection patterns on every PR that touches `db.py`" or "never flag style issues on auto-generated protobuf files". The review is a black box with no levers. This means that even when the model does produce good security findings, there is no guarantee the same signal fires on the next PR.

### 2.2 No Repo-Level Review Contract

Tools like CodeRabbit (`.coderabbit.yaml`), Reviewpad (`reviewpad.yml`), and Danger.js (`Dangerfile`) all share a common pattern: a checked-in config file that defines the review contract for a repository. This file can be code-reviewed, discussed in PRs, enforced via branch protection, and evolved as the team's standards change. It serves as documentation of what the automated reviewer is actually checking.

TAG has no equivalent. There is no `.tag-review.yaml`, no per-repo configuration surface, and no way for a team lead to express "we enforce security and correctness checks on all PRs, style checks are advisory only, and we do not run performance checks on the frontend monorepo". Without this, `tag ci review` cannot be reasonably used in a CI gate because the review scope is not defined as an organizational commitment — it is whatever the LLM decided this time.

### 2.3 Wasted LLM Context on Irrelevant Signal Classes

Running all possible review checks on every PR wastes LLM context window and increases review latency and cost. A diff that only touches documentation Markdown files does not need a performance review. A PR that only modifies SQL migration files does not need an accessibility check. Accessibility checks on non-UI diffs consume context that could be used to analyze more of the relevant diff. At enterprise scale, this inefficiency compounds: a team running 50 PRs/day with 10KB average diffs pays for irrelevant signal processing on every single review.

The signal class architecture allows the review context to be focused — only the diff hunks that match the file patterns for the active signal classes are included in the LLM prompt for each class. This reduces prompt size, reduces cost, reduces latency, and improves finding quality by focusing model attention.

---

## 3. Goals

**G1.** Define a stable taxonomy of six review signal classes (`security`, `coverage`, `style`, `correctness`, `performance`, `accessibility`) with documented semantics, file-path defaults, and severity vocabularies.

**G2.** Implement a `.tag-review.yaml` config schema that allows per-class activation, severity thresholds, path glob overrides, and review-level settings (max diff chars, model override, post-comments flag).

**G3.** Add `--signals <class,...>` flag to `tag ci review --pr` that overrides active classes for a single invocation without modifying the on-disk config.

**G4.** Add `--config <path>` flag to `tag ci review --pr` that loads a config from a non-default path, enabling multi-config workflows.

**G5.** Add `tag ci review init` subcommand that scaffolds `.tag-review.yaml` with project-type detection (Python, TypeScript, Go, Rust, generic).

**G6.** Implement per-class prompt segments that inject focused, class-specific review instructions into `build_review_prompt()`, replacing the generic system prompt when signal classes are active.

**G7.** Implement path-glob filtering that routes diff hunks to signal classes based on file extension and path patterns before the LLM call, reducing prompt size.

**G8.** Return structured JSON findings from the LLM (one finding per object) and store them in a new `review_findings` SQLite table for history and deduplication.

**G9.** Preserve full backward compatibility — when no `.tag-review.yaml` is present and `--signals` is not passed, behavior is identical to pre-PRD-061.

**G10.** Expose `tag ci review history --pr <N>` to display findings from previous reviews of a PR, filtered by signal class.

---

## 4. Non-Goals

**NG1.** Running static analysis tools (bandit, eslint, mypy, cargo clippy) as part of the review. This PRD uses LLM-only analysis; SAST tool integration belongs to a separate PRD.

**NG2.** Automatic code fix application. Signal class findings are surfaced as review comments; applying fixes is always a human action.

**NG3.** Custom user-defined signal classes. The taxonomy is fixed at six classes. Extending it requires a new PRD.

**NG4.** Per-file or per-function granularity in config. Path globs operate at the file level only.

**NG5.** Integrating with external review tools (CodeRabbit API, Reviewpad API). This is a standalone TAG feature.

**NG6.** Streaming review findings in real time. Findings are returned after the full LLM call completes.

**NG7.** GitLab or Gitea support in this PRD. Only GitHub (via `gh` CLI) is in scope; the hooks for GitLab already exist in `ci.py` and can be wired up in a follow-on.

**NG8.** Multi-language per-class rule libraries. The system prompts are language-agnostic; per-language prompt specialization is a stretch goal.

---

## 5. Success Metrics

| Metric | Target | Measurement Method |
|--------|--------|--------------------|
| Config load correctness | `.tag-review.yaml` parsed without error for 100% of valid schemas | Unit tests covering all schema variants |
| Signal routing accuracy | Diff hunks routed to correct signal classes for ≥ 95% of file extensions | Unit tests on path-glob matching with fixture diffs |
| Prompt size reduction | Active-signal prompts are ≤ 60% of the size of the monolithic prompt for single-class runs | Token count comparison in benchmark test |
| Finding structure rate | LLM returns valid structured JSON for ≥ 90% of review calls | Integration test against real PR diffs with Claude Sonnet |
| `init` scaffold coverage | `tag ci review init` generates valid YAML for Python, TypeScript, Go, Rust, and generic project types | E2E tests per project type |
| Backward compatibility | Zero behavior change for existing users with no `.tag-review.yaml` | Regression test on existing `build_review_prompt()` output |
| Review history recall | `review_findings` table correctly stores and retrieves findings by PR number and signal class | SQLite integration tests |
| CI gate exit codes | Exit code 1 when `--fail-on severity=error` threshold is breached, 0 otherwise | Integration test with mock findings |

---

## 6. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|-----------|----------|
| U1 | Security engineer | run `tag ci review --pr 123 --signals security,correctness` | I get a focused review that checks for vulnerabilities and logic bugs without noise from style or performance suggestions |
| U2 | Frontend team lead | check `.tag-review.yaml` into my React repo with `signals: [accessibility, style, correctness]` and `accessibility.paths: ["**/*.tsx", "**/*.jsx"]` | every PR automatically gets accessibility and style review without me having to pass flags |
| U3 | Backend developer | run `tag ci review init` in my Python FastAPI repo | I get a `.tag-review.yaml` with security and correctness signals pre-enabled and sensible path defaults for Python |
| U4 | DevOps engineer | run `tag ci review --pr $PR_NUMBER --signals security --fail-on error` in GitHub Actions | the CI job fails automatically when security findings of severity `error` are detected, blocking the merge |
| U5 | Platform architect | run `tag ci review --pr 456 --config .tag-review-strict.yaml` | I can apply a stricter review config to PRs targeting `main` without changing the default config used for feature branches |
| U6 | Developer | run `tag ci review history --pr 123 --signals security` | I can see all security findings from previous reviews of PR #123 and track whether they were addressed |
| U7 | Team lead | configure `style.severity: advisory` in `.tag-review.yaml` | style findings appear in the review but do not fail the CI gate, while `correctness` and `security` findings remain blocking |
| U8 | Developer | run `tag ci review --pr 789 --signals correctness --dry-run` | I can see the signal-filtered diff and the prompt that would be sent to the LLM without spending tokens |
| U9 | Developer | run `tag ci review --pr 100 --signals all` | I get a review covering all six signal classes explicitly, regardless of what `.tag-review.yaml` says |
| U10 | Team member | run `tag ci review --pr 200 --json` | I get machine-readable JSON output of all findings for piping to downstream tooling or custom dashboards |

---

## 7. Proposed CLI Surface

### 7.1 `tag ci review --pr` with `--signals`

```
tag ci review \
  --pr <number> \
  [--repo <owner/name>]          # default: auto-detected from git remote
  [--signals <class,...>]        # comma-separated; overrides .tag-review.yaml
  [--config <path>]              # load config from this path instead of .tag-review.yaml
  [--fail-on <severity>]         # exit 1 if any finding >= this severity (error|warning)
  [--post-comments]              # post findings as inline GitHub PR comments
  [--dry-run]                    # print filtered diff + prompt, do not call LLM
  [--json]                       # output findings as JSON array
  [--model <model-id>]           # override model from config
  [--max-diff-chars <N>]         # truncate diff at N chars (default: 8000)
  [--yes]                        # skip cost confirmation prompt
```

Valid signal class names: `security`, `coverage`, `style`, `correctness`, `performance`, `accessibility`, `all`

**Example: run with explicit signals**

```bash
$ tag ci review --pr 123 --signals security,correctness --repo myorg/myrepo

Loading signal classes: security, correctness
Fetching PR #123 diff (myorg/myrepo)...
  Diff: 487 chars across 3 files
  Filtered for security: 2 files (src/auth.py, src/db.py)
  Filtered for correctness: 3 files (src/auth.py, src/db.py, tests/test_auth.py)
Running security review... done (1.3s)
Running correctness review... done (0.9s)

Findings (5 total):
  [security] [ERROR]   src/auth.py:42   SQL query uses string interpolation — use parameterised queries
  [security] [WARNING] src/auth.py:87   Password compared with == instead of hmac.compare_digest
  [correctness] [ERROR]   src/db.py:15    cursor.execute() result not checked; silent failure on constraint violation
  [correctness] [WARNING] src/auth.py:104  Early return leaves session token in memory after logout
  [correctness] [INFO]    tests/test_auth.py:33  Test does not assert response status code

Exit code: 1  (--fail-on error threshold breached: 2 error-severity findings)
```

**Example: with `.tag-review.yaml` auto-loaded**

```bash
$ tag ci review --pr 456

Found .tag-review.yaml — loading review config
Active signals: security (error), correctness (error), style (advisory)
Fetching PR #456 diff...
...
```

**Example: dry run**

```bash
$ tag ci review --pr 789 --signals performance --dry-run

[DRY RUN] Signal class: performance
[DRY RUN] Files matched by performance path globs:
  src/indexer.py (matched: **/*.py)
  src/cache.py   (matched: **/*.py)

[DRY RUN] Prompt that would be sent (1,243 chars):
---
You are an expert code reviewer focused exclusively on PERFORMANCE.
...
[diff hunk for src/indexer.py]
[diff hunk for src/cache.py]
---
[DRY RUN] No LLM call made.
```

### 7.2 `tag ci review init`

```
tag ci review init \
  [--project-type <python|typescript|go|rust|generic>]  # default: auto-detect
  [--output <path>]                                     # default: ./.tag-review.yaml
  [--force]                                             # overwrite existing file
```

**Output (Python project)**

```bash
$ tag ci review init

Detected project type: python (found pyproject.toml, src/tag/)
Writing .tag-review.yaml...

.tag-review.yaml created. Edit it to customise signal classes for your repo.
Suggested next step: tag ci review --pr <N> --dry-run
```

**Scaffolded `.tag-review.yaml` for a Python project:**

```yaml
# .tag-review.yaml — TAG PR Review Signal Configuration
# Generated by: tag ci review init (python)
# Reference:    https://tag.dev/docs/ci-review-signals

version: "1"

review:
  model: anthropic/claude-sonnet-4-6      # override with --model
  max_diff_chars: 8000
  post_comments: false                    # set true to post inline GitHub comments
  fail_on: error                          # exit 1 when findings at this severity exist

signals:
  security:
    enabled: true
    severity: error                       # error | warning | advisory
    paths:
      - "**/*.py"
      - "**/*.yaml"
      - "**/*.yml"
      - "**/*.toml"
      - "**/*.env*"
      - "**/Dockerfile*"
    focus:
      - "injection vulnerabilities (SQL, shell, SSTI)"
      - "authentication and authorisation flaws"
      - "hardcoded credentials and secrets"
      - "unsafe deserialization"
      - "SSRF and open redirect"

  coverage:
    enabled: false                        # enable if you use a coverage tool
    severity: warning
    paths:
      - "**/*.py"
    focus:
      - "new functions or classes without corresponding tests"
      - "branches or edge cases not covered by existing tests"

  style:
    enabled: true
    severity: advisory                    # advisory = findings shown but never fail CI
    paths:
      - "**/*.py"
    focus:
      - "PEP 8 and ruff compliance"
      - "docstring completeness"
      - "naming conventions"

  correctness:
    enabled: true
    severity: error
    paths:
      - "**/*.py"
      - "**/*.yaml"
    focus:
      - "logic errors and off-by-one mistakes"
      - "incorrect exception handling"
      - "race conditions and thread safety"
      - "resource leaks (unclosed files, connections)"

  performance:
    enabled: false
    severity: warning
    paths:
      - "**/*.py"
    focus:
      - "O(n²) or worse complexity in hot paths"
      - "repeated database queries inside loops (N+1)"
      - "unnecessary copies of large data structures"

  accessibility:
    enabled: false
    severity: warning
    paths:
      - "**/*.html"
      - "**/*.css"
      - "**/*.tsx"
      - "**/*.jsx"
    focus:
      - "missing ARIA labels and roles"
      - "keyboard navigation support"
      - "colour contrast and focus indicators"
      - "alt text on images"
```

### 7.3 `tag ci review history`

```
tag ci review history \
  --pr <number> \
  [--repo <owner/name>]
  [--signals <class,...>]
  [--limit <N>]              # default: 20
  [--json]
```

**Example output:**

```bash
$ tag ci review history --pr 123 --signals security

Review history for PR #123 — signal: security
Run 3  2026-06-17 14:22  1 finding  (1 error)
  [ERROR] src/auth.py:42  SQL string interpolation  (OPEN)

Run 2  2026-06-16 09:11  2 findings (2 errors)
  [ERROR] src/auth.py:42  SQL string interpolation  (OPEN)
  [ERROR] src/auth.py:87  == password comparison    (RESOLVED — line removed in later commit)

Run 1  2026-06-15 18:03  3 findings (2 errors, 1 warning)
  [ERROR] src/auth.py:42  SQL string interpolation  (OPEN)
  [ERROR] src/auth.py:87  == password comparison    (RESOLVED)
  [WARNING] src/auth.py:12 Missing rate limit         (RESOLVED)
```

---

## 8. Functional Requirements

| ID | Requirement | Priority |
|----|-------------|----------|
| FR-01 | `tag ci review --pr <N> --signals <class,...>` MUST accept a comma-separated list of signal class names from the set {`security`, `coverage`, `style`, `correctness`, `performance`, `accessibility`, `all`}. Any unrecognised name MUST produce a clear error and exit 1. | Must Have |
| FR-02 | When `--signals all` is passed, all six signal classes MUST be activated with their default settings regardless of `.tag-review.yaml`. | Must Have |
| FR-03 | When `.tag-review.yaml` exists in the current working directory and `--config` is not passed, it MUST be automatically loaded. When it does not exist, the command MUST fall back to monolithic review behavior identical to pre-PRD-061 output. | Must Have |
| FR-04 | `--config <path>` MUST load the specified YAML file instead of `.tag-review.yaml`. A missing or unreadable file at the specified path MUST produce an error and exit 1. | Must Have |
| FR-05 | `--signals` passed on the CLI MUST override the `enabled` field for the named classes in any loaded config. Classes not named in `--signals` that are enabled in the config MUST remain active. | Must Have |
| FR-06 | Each enabled signal class MUST produce an independent LLM call with a class-specific system prompt segment injected. Results from all class calls MUST be merged into a single findings list before output. | Must Have |
| FR-07 | Each LLM call for a signal class MUST include only the diff hunks matching the `paths` globs for that class. Diff hunks for files matching no active signal class's path globs MUST be silently excluded. | Must Have |
| FR-08 | The LLM MUST be prompted to return findings as a JSON array. Each finding object MUST contain: `signal_class` (str), `severity` (str: `error`|`warning`|`info`), `file` (str), `line` (int|null), `message` (str), `suggestion` (str|null). | Must Have |
| FR-09 | When `--fail-on <severity>` is set (or `review.fail_on` in config), the command MUST exit with code 1 if any finding has severity >= the specified level. Severity ordering: error > warning > info. | Must Have |
| FR-10 | `--dry-run` MUST print the file list matched per signal class, the approximate token count of the prompt that would be sent, and the full prompt text, then exit 0 without making any LLM API call. | Must Have |
| FR-11 | `tag ci review init` MUST detect the project type by checking for the presence of: `pyproject.toml` or `setup.py` (Python), `package.json` with `typescript` dep or `.tsx` files (TypeScript), `go.mod` (Go), `Cargo.toml` (Rust). First match wins. | Must Have |
| FR-12 | `tag ci review init` MUST write a valid `.tag-review.yaml` at the target path. If the file already exists, it MUST refuse and print an error unless `--force` is passed. | Must Have |
| FR-13 | `tag ci review init` MUST NOT overwrite an existing `.tag-review.yaml` without `--force`. | Must Have |
| FR-14 | All findings MUST be stored in the `review_findings` SQLite table (see Section 9.2) after each review run. | Must Have |
| FR-15 | `tag ci review history --pr <N>` MUST query `review_findings` and display findings grouped by review run, most recent first, filtered to the specified PR number. | Must Have |
| FR-16 | When `--post-comments` is active (or `review.post_comments: true` in config), all non-info findings MUST be posted as inline GitHub PR comments using the existing `post_pr_review_comments()` function from `ci.py`. | Must Have |
| FR-17 | `--json` MUST output a JSON object with keys `pr`, `repo`, `signals_active`, `findings` (array), `summary` (counts per severity per class), and `exit_code`. | Must Have |
| FR-18 | Signal class `severity` in config accepts `error`, `warning`, `advisory`. An `advisory` class's findings MUST appear in output but MUST NOT contribute to `--fail-on` threshold evaluation. | Must Have |
| FR-19 | A `focus` list in a signal class config MUST be appended to the class-specific system prompt as a bulleted list of what to specifically look for. If `focus` is empty or absent, a built-in default focus list for the class MUST be used. | Should Have |
| FR-20 | `tag ci review --pr <N> --signals security --model anthropic/claude-opus-4` MUST use the specified model for all LLM calls in this run, overriding both the config and the profile default. | Should Have |

---

## 9. Non-Functional Requirements

| ID | Requirement | Target |
|----|-------------|--------|
| NFR-01 | YAML config parsing MUST complete in < 10ms for any valid `.tag-review.yaml` of ≤ 500 lines. | Performance |
| NFR-02 | Path-glob matching against a PR diff of 50 files MUST complete in < 50ms. | Performance |
| NFR-03 | The `review_findings` SQLite insert for a 20-finding review result MUST complete in < 100ms using WAL mode via `open_db()`. | Performance |
| NFR-04 | When a signal class LLM call times out (> 60s), the command MUST print a warning and continue with the remaining signal classes rather than aborting the entire review. | Reliability |
| NFR-05 | Invalid `.tag-review.yaml` (missing required `version` key, unknown signal class name, invalid severity value) MUST produce a human-readable validation error describing which field is invalid, not a raw Python exception traceback. | Usability |
| NFR-06 | The `--dry-run` mode MUST never make any network call (LLM API, GitHub API). This is a firm contract, not best-effort. | Security |
| NFR-07 | Signal class names in `.tag-review.yaml` and on the CLI MUST be case-insensitive (`Security` and `SECURITY` are equivalent to `security`). | Usability |
| NFR-08 | The `.tag-review.yaml` schema MUST be documented inline (via comments in the init-scaffolded file) such that a developer can understand all options without external documentation. | Usability |
| NFR-09 | All SQLite operations MUST use `open_db()` from `controller.py` with WAL mode. No direct `sqlite3.connect()` calls in the new code. | Consistency |
| NFR-10 | The feature MUST add zero mandatory new Python package dependencies. `pyyaml` is already available in the TAG dependency set. `fnmatch` is stdlib. | Compatibility |
| NFR-11 | When `ANTHROPIC_API_KEY` is not set and no model override is provided, the command MUST fail with a clear error message referencing the missing env var, not an opaque HTTP 401. | Usability |
| NFR-12 | All findings stored in `review_findings` MUST include the git commit SHA of the PR head at review time, enabling deduplication of findings across re-reviews of the same commit. | Data Integrity |

---

## 10. Technical Design

### 10.1 New and Modified Files

| File | Change Type | Description |
|------|-------------|-------------|
| `src/tag/ci.py` | Modified | Add `ReviewConfig`, `SignalClassConfig`, `ReviewFinding` dataclasses; add `load_review_config()`, `filter_diff_by_paths()`, `build_signal_prompt()`, `parse_signal_findings()`, `run_signal_review()`, `scaffold_review_config()`; update `build_review_prompt()` to accept optional signal config |
| `src/tag/controller.py` | Modified | Add `cmd_ci_review()` with `--signals`, `--config`, `--fail-on`, `--post-comments`, `--dry-run`, `--json` flags; add `cmd_ci_review_init()` and `cmd_ci_review_history()` subcommand handlers; wire into existing `tag ci` command group |
| `~/.tag/runtime/tag.sqlite3` | Schema | Add `review_findings` table and `review_runs` table (see Section 9.2) |

### 10.2 SQLite DDL

```sql
-- review_runs: one row per invocation of tag ci review --pr
CREATE TABLE IF NOT EXISTS review_runs (
    id             TEXT    PRIMARY KEY,           -- UUID v4
    created_at     TEXT    NOT NULL               -- ISO-8601 with timezone
                           DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    repo           TEXT    NOT NULL,              -- owner/name
    pr_number      INTEGER NOT NULL,
    head_sha       TEXT,                          -- PR head commit SHA at review time
    signals_active TEXT    NOT NULL,              -- JSON array of active class names
    config_path    TEXT,                          -- path to .tag-review.yaml used, or NULL
    model          TEXT,                          -- model used for LLM calls
    total_findings INTEGER NOT NULL DEFAULT 0,
    error_count    INTEGER NOT NULL DEFAULT 0,
    warning_count  INTEGER NOT NULL DEFAULT 0,
    info_count     INTEGER NOT NULL DEFAULT 0,
    exit_code      INTEGER,
    duration_ms    INTEGER
);

CREATE INDEX IF NOT EXISTS idx_review_runs_pr
    ON review_runs (repo, pr_number, created_at DESC);

-- review_findings: one row per LLM-returned finding
CREATE TABLE IF NOT EXISTS review_findings (
    id             TEXT    PRIMARY KEY,           -- UUID v4
    run_id         TEXT    NOT NULL               -- FK to review_runs.id
                           REFERENCES review_runs (id) ON DELETE CASCADE,
    created_at     TEXT    NOT NULL
                           DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    repo           TEXT    NOT NULL,
    pr_number      INTEGER NOT NULL,
    head_sha       TEXT,
    signal_class   TEXT    NOT NULL,              -- security | coverage | style | correctness | performance | accessibility
    severity       TEXT    NOT NULL,              -- error | warning | info
    file_path      TEXT,                          -- relative path within repo
    line_number    INTEGER,                       -- NULL when not applicable
    message        TEXT    NOT NULL,
    suggestion     TEXT,
    fingerprint    TEXT,                          -- SHA-256 of (repo, signal_class, file_path, message_normalized)
    resolved       INTEGER NOT NULL DEFAULT 0     -- 0 = open, 1 = resolved (detected via re-review)
);

CREATE INDEX IF NOT EXISTS idx_review_findings_run
    ON review_findings (run_id);

CREATE INDEX IF NOT EXISTS idx_review_findings_pr
    ON review_findings (repo, pr_number, signal_class, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_review_findings_fingerprint
    ON review_findings (fingerprint);
```

### 10.3 Core Dataclasses

```python
from __future__ import annotations

import fnmatch
import hashlib
import json
from dataclasses import dataclass, field
from pathlib import Path
from typing import Literal

SignalClassName = Literal[
    "security", "coverage", "style", "correctness", "performance", "accessibility"
]
Severity = Literal["error", "warning", "advisory", "info"]


@dataclass
class SignalClassConfig:
    """Per-signal-class configuration loaded from .tag-review.yaml."""

    name: SignalClassName
    enabled: bool = True
    severity: Severity = "warning"
    paths: list[str] = field(default_factory=lambda: ["**/*"])
    focus: list[str] = field(default_factory=list)

    def matches_path(self, file_path: str) -> bool:
        """Return True if file_path matches any glob in self.paths."""
        return any(fnmatch.fnmatch(file_path, pat) for pat in self.paths)


@dataclass
class ReviewConfig:
    """Parsed representation of .tag-review.yaml."""

    version: str = "1"
    model: str | None = None
    max_diff_chars: int = 8000
    post_comments: bool = False
    fail_on: Severity | None = "error"
    signals: dict[str, SignalClassConfig] = field(default_factory=dict)

    @classmethod
    def default(cls) -> "ReviewConfig":
        """Return config with all six classes at default settings."""
        cfg = cls()
        for name in (
            "security", "coverage", "style",
            "correctness", "performance", "accessibility"
        ):
            cfg.signals[name] = SignalClassConfig(name=name)  # type: ignore[arg-type]
        return cfg

    def active_classes(self) -> list[SignalClassConfig]:
        """Return signal classes that are enabled."""
        return [c for c in self.signals.values() if c.enabled]


@dataclass
class ReviewFinding:
    """A single finding returned by the LLM for one signal class."""

    signal_class: SignalClassName
    severity: str                    # "error" | "warning" | "info"
    file: str | None
    line: int | None
    message: str
    suggestion: str | None = None

    @property
    def fingerprint(self) -> str:
        """Stable SHA-256 dedup key."""
        normalized = (
            f"{self.signal_class}:{self.file or ''}:"
            f"{self.message.strip().lower()[:120]}"
        )
        return hashlib.sha256(normalized.encode()).hexdigest()[:16]

    def to_dict(self) -> dict:
        return {
            "signal_class": self.signal_class,
            "severity": self.severity,
            "file": self.file,
            "line": self.line,
            "message": self.message,
            "suggestion": self.suggestion,
            "fingerprint": self.fingerprint,
        }
```

### 10.4 YAML Config Loader

```python
import yaml

# Canonical default focus lists per signal class
_DEFAULT_FOCUS: dict[str, list[str]] = {
    "security": [
        "injection vulnerabilities (SQL, shell, template, SSTI)",
        "authentication and authorisation bypass",
        "hardcoded secrets, API keys, or credentials",
        "unsafe deserialization or pickle usage",
        "path traversal and directory listing",
        "SSRF, open redirect, CSRF",
        "cryptographic weaknesses (MD5, SHA1 for security, ECB mode)",
    ],
    "coverage": [
        "new public functions or classes without corresponding tests",
        "edge cases and error paths not covered by existing tests",
        "missing test assertions (tests that cannot fail)",
    ],
    "style": [
        "naming convention violations",
        "excessive function length (> 50 lines) or cyclomatic complexity",
        "missing or incomplete docstrings on public API",
        "inconsistent formatting or unnecessary whitespace changes",
    ],
    "correctness": [
        "logic errors and off-by-one mistakes",
        "unchecked return values or error codes",
        "race conditions, shared mutable state without synchronization",
        "resource leaks (files, connections, locks not closed/released)",
        "incorrect type assumptions or implicit coercions",
    ],
    "performance": [
        "O(n²) or worse complexity in hot paths",
        "N+1 query patterns or repeated I/O inside loops",
        "unnecessary large data structure copies",
        "blocking I/O in async contexts",
    ],
    "accessibility": [
        "missing ARIA labels, roles, or landmark regions",
        "interactive elements not keyboard-navigable",
        "insufficient colour contrast (WCAG AA 4.5:1 minimum)",
        "images without meaningful alt text",
        "form inputs without associated labels",
    ],
}


def load_review_config(path: Path) -> ReviewConfig:
    """Parse .tag-review.yaml into a ReviewConfig.

    Raises
    ------
    ValueError
        For schema violations with a descriptive human-readable message.
    FileNotFoundError
        If the path does not exist.
    """
    raw = yaml.safe_load(path.read_text(encoding="utf-8"))
    if not isinstance(raw, dict):
        raise ValueError(f"{path}: expected a YAML mapping at the top level")

    version = raw.get("version", "1")
    review_block = raw.get("review", {})
    signals_block = raw.get("signals", {})

    valid_classes = {
        "security", "coverage", "style",
        "correctness", "performance", "accessibility",
    }
    valid_severities = {"error", "warning", "advisory", "info"}

    signals: dict[str, SignalClassConfig] = {}
    for cls_name, cls_raw in signals_block.items():
        cls_name_lower = cls_name.lower()
        if cls_name_lower not in valid_classes:
            raise ValueError(
                f"{path}: unknown signal class '{cls_name}'. "
                f"Valid classes: {sorted(valid_classes)}"
            )
        if not isinstance(cls_raw, dict):
            raise ValueError(
                f"{path}: signals.{cls_name} must be a mapping"
            )
        severity = cls_raw.get("severity", "warning").lower()
        if severity not in valid_severities:
            raise ValueError(
                f"{path}: signals.{cls_name}.severity '{severity}' is not "
                f"one of {sorted(valid_severities)}"
            )
        paths = cls_raw.get("paths", ["**/*"])
        if not isinstance(paths, list):
            raise ValueError(
                f"{path}: signals.{cls_name}.paths must be a list of globs"
            )
        focus = cls_raw.get("focus", _DEFAULT_FOCUS.get(cls_name_lower, []))
        signals[cls_name_lower] = SignalClassConfig(
            name=cls_name_lower,       # type: ignore[arg-type]
            enabled=cls_raw.get("enabled", True),
            severity=severity,         # type: ignore[arg-type]
            paths=paths,
            focus=focus,
        )

    # Fill in any classes not mentioned in YAML with defaults (disabled)
    for cls_name in valid_classes:
        if cls_name not in signals:
            signals[cls_name] = SignalClassConfig(
                name=cls_name,          # type: ignore[arg-type]
                enabled=False,
                severity="warning",
                paths=["**/*"],
                focus=_DEFAULT_FOCUS[cls_name],
            )

    fail_on_raw = review_block.get("fail_on", "error")
    if fail_on_raw is not None and fail_on_raw.lower() not in valid_severities:
        raise ValueError(
            f"{path}: review.fail_on '{fail_on_raw}' is not one of "
            f"{sorted(valid_severities)}"
        )

    return ReviewConfig(
        version=str(version),
        model=review_block.get("model"),
        max_diff_chars=int(review_block.get("max_diff_chars", 8000)),
        post_comments=bool(review_block.get("post_comments", False)),
        fail_on=fail_on_raw.lower() if fail_on_raw else None,   # type: ignore[union-attr]
        signals=signals,
    )
```

### 10.5 Diff Filtering by Path Globs

```python
import re


def filter_diff_by_paths(diff: str, path_globs: list[str]) -> str:
    """Extract diff hunks for files matching any of path_globs.

    Parses the unified diff format, collecting per-file sections. Each section
    begins at a ``diff --git`` line. Sections are included when the file path
    (b-side of the diff header) matches at least one glob pattern.

    Parameters
    ----------
    diff:
        Full unified diff text.
    path_globs:
        List of fnmatch-style glob patterns, e.g. ``["**/*.py", "**/*.yaml"]``.

    Returns
    -------
    str
        Filtered diff containing only matching file sections, or an empty
        string if no files match.
    """
    sections: list[tuple[str, str]] = []   # (file_path, section_text)
    current_path: str | None = None
    current_lines: list[str] = []

    header_re = re.compile(r'^diff --git a/.+ b/(.+)$')

    for line in diff.splitlines(keepends=True):
        m = header_re.match(line)
        if m:
            if current_path is not None:
                sections.append((current_path, "".join(current_lines)))
            current_path = m.group(1).strip()
            current_lines = [line]
        else:
            if current_path is not None:
                current_lines.append(line)

    if current_path is not None:
        sections.append((current_path, "".join(current_lines)))

    matched: list[str] = []
    for file_path, section_text in sections:
        if any(fnmatch.fnmatch(file_path, pat) for pat in path_globs):
            matched.append(section_text)

    return "".join(matched)
```

### 10.6 Signal-Specific Prompt Builder

```python
_SIGNAL_SYSTEM_PREAMBLE = """\
You are an expert code reviewer. You are reviewing a pull request diff.
Your task is SPECIFICALLY to check for {signal_class_upper} issues.
Focus ONLY on {signal_class_upper}. Do not comment on other aspects of the code.

Look specifically for:
{focus_bullets}

Return your findings as a JSON array. Each object must have exactly these keys:
  "signal_class": "{signal_class}" (string, always this value)
  "severity":     "error" | "warning" | "info"
  "file":         relative file path (string, or null if not file-specific)
  "line":         line number in the diff (integer, or null)
  "message":      concise description of the finding (string, ≤ 120 chars)
  "suggestion":   concrete remediation suggestion (string, or null)

If you find no issues, return an empty JSON array: []
Return ONLY the JSON array. Do not include any prose, markdown fences, or explanation.
"""

_SIGNAL_DIFF_TEMPLATE = """\
## Pull Request

- **Title**: {title}
- **Author**: {author}
- **Base → Head**: {base} → {head}

## Diff ({signal_class} — {file_count} file(s) matched)

```diff
{filtered_diff}
```
"""


def build_signal_prompt(
    signal_cfg: SignalClassConfig,
    filtered_diff: str,
    metadata: dict,
) -> str:
    """Build a focused LLM prompt for a single signal class.

    Parameters
    ----------
    signal_cfg:
        The signal class configuration (name, focus list, etc.).
    filtered_diff:
        Diff text pre-filtered to only the files matching this signal class.
    metadata:
        PR metadata dict from fetch_pr_metadata().

    Returns
    -------
    str
        Complete prompt string for the LLM call.
    """
    focus_bullets = "\n".join(f"- {item}" for item in signal_cfg.focus)
    system = _SIGNAL_SYSTEM_PREAMBLE.format(
        signal_class_upper=signal_cfg.name.upper(),
        signal_class=signal_cfg.name,
        focus_bullets=focus_bullets,
    )
    file_count = filtered_diff.count("\ndiff --git") + (
        1 if filtered_diff.startswith("diff --git") else 0
    )
    diff_block = _SIGNAL_DIFF_TEMPLATE.format(
        title=metadata.get("title", "(no title)"),
        author=(metadata.get("author") or {}).get("login", "unknown"),
        base=metadata.get("baseRefName", ""),
        head=metadata.get("headRefName", ""),
        signal_class=signal_cfg.name,
        file_count=file_count,
        filtered_diff=filtered_diff[:8000],  # guard; caller should pre-truncate
    )
    return system + "\n" + diff_block
```

### 10.7 Finding Parser

```python
def parse_signal_findings(
    raw_response: str,
    expected_class: str,
) -> list[ReviewFinding]:
    """Parse the LLM JSON response into ReviewFinding objects.

    Tolerant of common LLM formatting mistakes: strips markdown code fences,
    ignores trailing prose after the closing ``]``.

    Parameters
    ----------
    raw_response:
        Raw text returned by the LLM.
    expected_class:
        The signal class name; used to override whatever the LLM returned in
        ``signal_class`` (ensures data integrity regardless of LLM hallucination).

    Returns
    -------
    list[ReviewFinding]
        Parsed findings. Returns an empty list on any parse failure (logged at
        WARNING level), never raises.
    """
    import logging
    logger = logging.getLogger(__name__)

    # Strip markdown code fences if present
    text = raw_response.strip()
    if text.startswith("```"):
        lines = text.splitlines()
        text = "\n".join(
            line for line in lines
            if not line.startswith("```")
        ).strip()

    # Locate the JSON array
    start = text.find("[")
    end = text.rfind("]")
    if start == -1 or end == -1:
        logger.warning(
            "signal %s: LLM response contains no JSON array; treating as no findings",
            expected_class,
        )
        return []

    try:
        raw_findings = json.loads(text[start : end + 1])
    except json.JSONDecodeError as exc:
        logger.warning(
            "signal %s: JSON parse error: %s; treating as no findings",
            expected_class, exc,
        )
        return []

    findings: list[ReviewFinding] = []
    valid_severities = {"error", "warning", "info"}
    for item in raw_findings:
        if not isinstance(item, dict):
            continue
        severity = str(item.get("severity", "info")).lower()
        if severity not in valid_severities:
            severity = "info"
        findings.append(ReviewFinding(
            signal_class=expected_class,       # type: ignore[arg-type]
            severity=severity,
            file=item.get("file") or None,
            line=item.get("line") if isinstance(item.get("line"), int) else None,
            message=str(item.get("message", "")).strip()[:500],
            suggestion=str(item.get("suggestion", "")).strip() or None,
        ))
    return findings
```

### 10.8 Orchestrator: `run_signal_review()`

```python
import uuid
import time
import logging
from datetime import datetime, timezone

logger = logging.getLogger(__name__)


def run_signal_review(
    repo: str,
    pr_number: int,
    config: ReviewConfig,
    llm_call_fn,                # Callable[[str, str | None], str]
    signals_override: list[str] | None = None,
    dry_run: bool = False,
) -> tuple[list[ReviewFinding], dict]:
    """Orchestrate signal-class reviews for a PR.

    Parameters
    ----------
    repo:
        GitHub repository in ``owner/name`` format.
    pr_number:
        Pull-request number.
    config:
        Parsed ReviewConfig.
    llm_call_fn:
        Callable taking (prompt: str, model: str | None) -> str. Abstracts the
        LLM provider so this function is unit-testable without network access.
    signals_override:
        If provided, only these class names are activated (case-insensitive).
        Merges with config — classes in the override that are disabled in config
        are activated at their default settings.
    dry_run:
        If True, print prompts and return empty findings without calling llm_call_fn.

    Returns
    -------
    tuple[list[ReviewFinding], dict]
        (all_findings, summary_dict) where summary_dict contains counts per
        severity per class.
    """
    diff = fetch_pr_diff(repo, pr_number)
    metadata = fetch_pr_metadata(repo, pr_number)

    active_classes = config.active_classes()
    if signals_override:
        override_set = {s.lower() for s in signals_override}
        active_classes = [c for c in active_classes if c.name in override_set]
        # Add any override classes not in config (use defaults)
        existing_names = {c.name for c in active_classes}
        for cls_name in override_set:
            if cls_name not in existing_names and cls_name in config.signals:
                sig = config.signals[cls_name]
                sig.enabled = True
                active_classes.append(sig)

    if not active_classes:
        logger.warning("No active signal classes; falling back to monolithic review")
        return [], {}

    all_findings: list[ReviewFinding] = []
    summary: dict[str, dict[str, int]] = {}

    for sig_cfg in active_classes:
        sig_cfg.focus = sig_cfg.focus or _DEFAULT_FOCUS.get(sig_cfg.name, [])
        filtered = filter_diff_by_paths(diff, sig_cfg.paths)
        if not filtered:
            logger.info("signal %s: no files matched path globs; skipping", sig_cfg.name)
            continue

        prompt = build_signal_prompt(sig_cfg, filtered, metadata)

        if dry_run:
            print(f"\n[DRY RUN] Signal class: {sig_cfg.name}")
            print(f"[DRY RUN] Matched files:")
            for line in filtered.splitlines():
                if line.startswith("diff --git"):
                    print(f"  {line.split(' b/')[-1]}")
            print(f"[DRY RUN] Prompt ({len(prompt)} chars):\n---\n{prompt}\n---")
            continue

        t0 = time.monotonic()
        try:
            raw = llm_call_fn(prompt, config.model)
        except Exception as exc:
            logger.warning(
                "signal %s: LLM call failed (%s); skipping class", sig_cfg.name, exc
            )
            continue
        elapsed_ms = int((time.monotonic() - t0) * 1000)
        logger.debug("signal %s: LLM call took %dms", sig_cfg.name, elapsed_ms)

        findings = parse_signal_findings(raw, sig_cfg.name)
        all_findings.extend(findings)

        # Build summary
        class_summary: dict[str, int] = {"error": 0, "warning": 0, "info": 0}
        for f in findings:
            class_summary[f.severity] = class_summary.get(f.severity, 0) + 1
        summary[sig_cfg.name] = class_summary

    return all_findings, summary
```

### 10.9 Project-Type Detection for `init`

```python
def detect_project_type(root: Path) -> str:
    """Return a project type string for scaffold selection.

    Checks in priority order: python, typescript, go, rust, generic.

    Parameters
    ----------
    root:
        Directory to inspect (typically the current working directory).

    Returns
    -------
    str
        One of: ``"python"``, ``"typescript"``, ``"go"``, ``"rust"``, ``"generic"``
    """
    if (root / "pyproject.toml").exists() or (root / "setup.py").exists():
        return "python"

    pkg = root / "package.json"
    if pkg.exists():
        try:
            pkg_data = json.loads(pkg.read_text())
            deps = {
                **pkg_data.get("dependencies", {}),
                **pkg_data.get("devDependencies", {}),
            }
            if "typescript" in deps or any(
                (root / p).suffix == ".tsx"
                for p in (root / "src").glob("**/*") if (root / "src").exists()
            ):
                return "typescript"
        except (json.JSONDecodeError, OSError):
            pass
        return "typescript"   # package.json without TS dep → still JS/TS ecosystem

    if (root / "go.mod").exists():
        return "go"

    if (root / "Cargo.toml").exists():
        return "rust"

    return "generic"
```

### 10.10 Integration Points

| Integration Point | How It Is Used |
|------------------|----------------|
| `ci.py:fetch_pr_diff()` | Called by `run_signal_review()` to get the full diff before per-class filtering |
| `ci.py:fetch_pr_metadata()` | Called to populate PR title, author, base/head branch in each class prompt |
| `ci.py:post_pr_review_comments()` | Called after findings collection when `--post-comments` is active; findings are mapped to `{"path": f.file, "position": f.line, "body": ...}` dicts |
| `controller.py:open_db()` | Used in `cmd_ci_review()` to insert into `review_runs` and `review_findings` |
| `controller.py:_run_hermes()` (or equivalent LLM call path) | Abstracted via the `llm_call_fn` parameter in `run_signal_review()`; allows unit testing without live LLM |
| `tracing.py` | `run_signal_review()` creates one OTel span per signal class call, tagged with `signal.class`, `pr.number`, `findings.count` |
| `budget.py` | Each signal class LLM call is accounted against the active run's budget; if budget is exceeded mid-review, remaining classes are skipped |

---

## 11. Security Considerations

1. **No credential exposure in findings:** The `message` and `suggestion` fields written to SQLite and posted as PR comments must never include raw secret values even when the `security` signal class finds hardcoded credentials. The prompt instructs the LLM to reference the location and pattern name only; the controller must strip any sequence matching the secret-pattern library from `security.py` before storing a finding.

2. **YAML parsing is restricted to `yaml.safe_load()`:** The `.tag-review.yaml` loader uses `yaml.safe_load()` exclusively, never `yaml.load()`. This prevents arbitrary Python object instantiation via YAML tags in a user-controlled config file.

3. **Path glob injection:** The `paths` field in `.tag-review.yaml` is an fnmatch pattern, not a shell glob. `filter_diff_by_paths()` uses `fnmatch.fnmatch()`, which does not execute shell commands. However, excessively broad patterns (e.g., `**`) are accepted without restriction; this is by design — the user controls their own config.

4. **`--dry-run` must make zero network calls:** This is enforced structurally by returning before any `llm_call_fn()` invocation. The `--dry-run` path in `run_signal_review()` prints and returns without calling the LLM. A test verifies that a mock `llm_call_fn` is never called when `dry_run=True`.

5. **SQL injection prevention:** All SQLite operations use parameterised queries via the `?` placeholder syntax, never string interpolation. The fingerprint column is a deterministic hash, not user-supplied text.

6. **GitHub comment injection:** `message` and `suggestion` fields are written into GitHub PR comments. These fields are LLM-generated and could theoretically contain Markdown that creates misleading comment structure. The controller must strip HTML tags and limit comment body length to 65,535 characters (GitHub API limit) before posting.

7. **Config path traversal:** When `--config <path>` is provided, the path is resolved with `Path(path).resolve()` and validated to exist and be a regular file. Symlinks are followed but the resolved path must still be a file on the local filesystem. No URL or `file://` scheme is accepted.

8. **Model override trust boundary:** The `--model` CLI flag and `review.model` in config can specify any model ID. This is passed directly to the LLM call function. The controller validates that the model ID matches the pattern `provider/model-name` (no shell metacharacters, no URL encoding) before use.

---

## 12. Testing Strategy

### 12.1 Unit Tests

**File:** `tests/test_ci_signals.py`

| Test | What It Covers |
|------|---------------|
| `test_load_config_valid_python` | Loads a Python-project YAML fixture; asserts security enabled, coverage disabled, fail_on=error |
| `test_load_config_unknown_class` | Passes `signals: { hacking: ... }` — asserts ValueError with class name in message |
| `test_load_config_invalid_severity` | Passes `severity: critical` — asserts ValueError with field name in message |
| `test_load_config_missing_version` | Omits `version` key — asserts graceful default (version="1") |
| `test_filter_diff_python_only` | Passes mixed Python+CSS diff; filters to `**/*.py`; asserts CSS section absent |
| `test_filter_diff_no_match` | Passes Python diff; filters to `**/*.tsx`; asserts empty string returned |
| `test_filter_diff_double_star_glob` | `**/*.py` matches `src/deeply/nested/foo.py` |
| `test_parse_findings_valid_json` | LLM returns clean JSON array; asserts 3 FindingReview objects with correct fields |
| `test_parse_findings_markdown_fenced` | LLM wraps array in ```json...``` fences; asserts successful parse |
| `test_parse_findings_trailing_prose` | LLM appends a paragraph after the array; asserts array parsed and prose ignored |
| `test_parse_findings_invalid_json` | LLM returns prose only; asserts empty list returned (no exception) |
| `test_parse_findings_unknown_severity` | Finding has `severity: critical`; asserts normalised to `info` |
| `test_fingerprint_stability` | Same finding data produces identical fingerprint across two calls |
| `test_run_signal_review_dry_run` | `dry_run=True` — asserts `llm_call_fn` never called and output contains [DRY RUN] |
| `test_run_signal_review_class_skipped_no_match` | Signal class whose path globs match nothing — asserts LLM not called for that class |
| `test_run_signal_review_budget_exceeded` | `llm_call_fn` raises BudgetExceededError on second call — asserts first class findings preserved |
| `test_detect_project_type_python` | Temp dir with `pyproject.toml` — asserts "python" |
| `test_detect_project_type_typescript` | Temp dir with `package.json` containing `"typescript"` dep — asserts "typescript" |
| `test_detect_project_type_go` | Temp dir with `go.mod` — asserts "go" |
| `test_detect_project_type_rust` | Temp dir with `Cargo.toml` — asserts "rust" |
| `test_detect_project_type_generic` | Empty temp dir — asserts "generic" |

### 12.2 Integration Tests

**File:** `tests/test_ci_signals_integration.py`

These tests require `ANTHROPIC_API_KEY` and are skipped (`pytest.mark.skipif`) when the key is absent.

| Test | What It Covers |
|------|---------------|
| `test_real_security_review` | Runs `run_signal_review()` against a fixture diff containing a deliberate SQL injection pattern; asserts at least one `error` finding in the `security` class |
| `test_real_correctness_review` | Runs against a fixture diff with an unchecked return value; asserts at least one finding in `correctness` |
| `test_structured_json_output_rate` | Runs 10 security reviews against 10 fixture diffs; asserts `parse_signal_findings()` succeeds (non-empty list OR empty for clean diffs) for all 10 |
| `test_sqlite_persistence` | Full review run against fixture; opens `tag.sqlite3` and asserts `review_runs` row and `review_findings` rows inserted with correct `pr_number` and `signal_class` |
| `test_post_comments_integration` | With `GH_TOKEN` set, posts findings to a test-only GitHub repo PR; asserts `post_pr_review_comments()` returns True |

### 12.3 CLI End-to-End Tests

**File:** `tests/test_ci_review_cli.py`

Uses `click.testing.CliRunner` or subprocess to invoke the CLI.

| Test | Command | Assertion |
|------|---------|-----------|
| `test_init_python` | `tag ci review init --project-type python --output /tmp/test-review.yaml` | File written; valid YAML; `security.enabled: true` |
| `test_init_force` | Run init twice; second run with `--force` | Second run succeeds and overwrites |
| `test_init_no_force` | Run init twice without `--force` | Second run exits 1 with "already exists" message |
| `test_dry_run_no_api_call` | `tag ci review --pr 1 --signals security --dry-run` with mocked `gh` and no API key | Exits 0; output contains "[DRY RUN]" |
| `test_fail_on_exit_code` | Inject mock LLM returning error-severity findings; run with `--fail-on error` | Exit code 1 |
| `test_no_fail_advisory` | Inject mock LLM returning error-severity findings; class configured as `severity: advisory`; run with `--fail-on error` | Exit code 0 |
| `test_json_output_shape` | `--json` flag | stdout is valid JSON with keys `pr`, `repo`, `signals_active`, `findings`, `summary`, `exit_code` |
| `test_backward_compat` | No `.tag-review.yaml`, no `--signals`; mock LLM | Behavior identical to pre-PRD-061 `build_review_prompt()` output (asserts `_REVIEW_SYSTEM` preamble in prompt) |
| `test_history_subcommand` | Seed `review_findings` table; run `tag ci review history --pr 1` | Output contains finding message; most recent run listed first |

### 12.4 Performance Tests

| Test | Threshold |
|------|-----------|
| Parse `.tag-review.yaml` (500 lines) | < 10ms |
| `filter_diff_by_paths()` on 100-file diff | < 50ms |
| `review_findings` bulk insert (100 rows) | < 100ms |
| `detect_project_type()` on 10,000-file repo | < 200ms (uses `Path.exists()` only, not `glob`) |

---

## 13. Acceptance Criteria

| ID | Criterion | Verification |
|----|-----------|--------------|
| AC-01 | `tag ci review --pr 123 --signals security` exits 0 when no error-severity findings are returned and exits 1 when at least one error-severity security finding is returned (with default `--fail-on error`) | Integration test `test_real_security_review` + CLI test `test_fail_on_exit_code` |
| AC-02 | `tag ci review --pr 123 --signals security,correctness` runs exactly two LLM calls (one per class) and merges findings | Unit test with mock `llm_call_fn` counting invocations |
| AC-03 | A `.tag-review.yaml` with `signals.coverage.enabled: false` results in no LLM call for the `coverage` class | Unit test: assert `llm_call_fn` called only for enabled classes |
| AC-04 | `tag ci review --pr 123 --signals security --dry-run` makes zero network calls and exits 0 | CLI test `test_dry_run_no_api_call` with network mock |
| AC-05 | `tag ci review init` in a Python repo writes a `.tag-review.yaml` with `signals.security.enabled: true` and `signals.accessibility.enabled: false` | CLI test `test_init_python` |
| AC-06 | Running `tag ci review init` twice without `--force` prints an error containing "already exists" and exits 1 | CLI test `test_init_no_force` |
| AC-07 | After a review run, `review_runs` has exactly one new row and `review_findings` has one row per finding returned by the LLM | Integration test `test_sqlite_persistence` |
| AC-08 | `tag ci review history --pr 123` lists runs in descending `created_at` order | SQLite integration test asserting order |
| AC-09 | A finding in a class with `severity: advisory` does not trigger exit code 1 even with `--fail-on error` | CLI test `test_no_fail_advisory` |
| AC-10 | `tag ci review --pr 123 --signals unknown_class` exits 1 with a message listing valid class names | Unit test on CLI arg validation |
| AC-11 | `tag ci review --pr 123 --config nonexistent.yaml` exits 1 with a message referencing the missing file path | Unit test |
| AC-12 | When no `.tag-review.yaml` exists and `--signals` is not passed, `build_review_prompt()` receives the same arguments as before PRD-061 (backward compatibility) | Regression test comparing prompt strings |
| AC-13 | The `fingerprint` column in `review_findings` is a deterministic SHA-256 hash; two review runs that produce the same finding for the same file and message produce identical fingerprints | Unit test `test_fingerprint_stability` |
| AC-14 | `--json` output is valid JSON (parsed by `json.loads()`) and contains all required keys | CLI test `test_json_output_shape` |
| AC-15 | Diff hunks for `.css` files are excluded from the prompt when `--signals correctness` is active and `correctness.paths` defaults do not include CSS | Unit test `test_filter_diff_python_only` variant |

---

## 14. Dependencies

| Dependency | Type | Notes |
|------------|------|-------|
| PRD-020 (CI/CD Integration) | Hard | `ci.py` (`fetch_pr_diff`, `fetch_pr_metadata`, `post_pr_review_comments`, `build_review_prompt`) must exist; this PRD extends those functions |
| PRD-013 (Agent Tracing) | Soft | `tracing.py` OTel spans are emitted per-class if tracing is configured; feature works without tracing active |
| PRD-012 (Cost Tracking) | Soft | `budget.py` budget accounting applied per LLM call; feature degrades gracefully if budget module is unavailable |
| PRD-034 (Secret Scanning) | Soft | Pattern library from `security.py` used to redact potential secrets from finding messages before storage; feature works without it but secret values may appear in findings |
| `pyyaml` | Python package | Already in TAG dependencies; used for `.tag-review.yaml` parsing |
| `fnmatch` | Python stdlib | Used in `filter_diff_by_paths()`; no installation required |
| `uuid` | Python stdlib | Used for `review_runs.id` and `review_findings.id` generation |
| `gh` CLI | System binary | Required for `fetch_pr_diff()` and `post_pr_review_comments()`; must be authenticated |
| Anthropic API | External | Required for LLM calls; must have `ANTHROPIC_API_KEY` set |

---

## 15. Open Questions

| ID | Question | Owner | Target Resolution |
|----|----------|-------|-------------------|
| OQ-1 | Should signal class LLM calls be parallelised (e.g., `asyncio.gather` or `ThreadPoolExecutor`) to reduce total review latency? Parallel calls reduce wall time from O(n_classes) to O(1) at the cost of simultaneous API calls and potential rate-limit contention. | Engineering lead | Before implementation start |
| OQ-2 | Should `coverage` signal class integrate with actual coverage reports (Codecov API, `coverage.xml`) as additional context for the LLM, rather than relying solely on diff analysis? This would make coverage findings more accurate but adds a new optional dependency. | Product | Sprint planning |
| OQ-3 | Should `.tag-review.yaml` support a `profiles` block that maps branch name patterns to different signal configs (e.g., `main` gets strict security+correctness, `feat/*` gets advisory-only)? This mirrors Reviewpad's automations model. | Product | Phase 2 scope decision |
| OQ-4 | Should finding deduplication across re-reviews of the same commit (via `fingerprint` matching) automatically mark previous findings as `resolved` when they disappear in a new review? This requires a background reconciliation job or post-review pass. | Engineering lead | Phase 2 scope decision |
| OQ-5 | What is the right handling when the entire diff matches no path globs for any active signal class? Currently: warning logged, empty findings, exit 0. Should this be a warning exit code (e.g., exit 2) to alert CI operators that the review produced no signal? | Product | Before implementation |
| OQ-6 | Should `tag ci review init` offer an interactive wizard (`--interactive`) that asks the user which classes to enable, or is a best-guess scaffold with comments sufficient? The interactive mode would require a `questionary` or `rich.prompt` dependency. | UX | Phase 1 scope decision |
| OQ-7 | The `focus` list in each signal class is injected verbatim into the system prompt. Should there be a maximum length (e.g., 20 items, 2,000 chars) enforced at config-load time to prevent prompt bloat? | Engineering lead | Before implementation |

---

## 16. Complexity and Timeline

**Overall estimate:** S (3–5 days)

### Phase 1: Core Data Model and Config Loading (Day 1)

- Define `SignalClassConfig`, `ReviewConfig`, `ReviewFinding` dataclasses in `ci.py`
- Implement `load_review_config()` with full schema validation
- Implement `filter_diff_by_paths()` with fnmatch
- Implement `detect_project_type()` heuristics
- Write `test_load_config_*` and `test_filter_diff_*` unit tests
- Create SQLite DDL (`review_runs`, `review_findings` tables) with migration guard in `open_db()` call site

Deliverable: Config parsing, diff filtering, and schema are fully tested and merged.

### Phase 2: Signal Prompt Engine and LLM Orchestration (Day 2)

- Implement `build_signal_prompt()` with per-class system preamble and default focus lists
- Implement `parse_signal_findings()` with JSON tolerance logic
- Implement `run_signal_review()` orchestrator with timeout handling and budget accounting
- Write `test_parse_findings_*` and `test_run_signal_review_*` unit tests
- Add OTel spans in `run_signal_review()` via `tracing.py`

Deliverable: LLM orchestration is fully unit-tested against mock `llm_call_fn`.

### Phase 3: CLI Surface and Scaffold (Day 3)

- Add `--signals`, `--config`, `--fail-on`, `--post-comments`, `--dry-run`, `--json` to `cmd_ci_review()` in `controller.py`
- Implement `cmd_ci_review_init()` with project type detection and YAML template rendering
- Implement `cmd_ci_review_history()` backed by `review_findings` SQLite query
- Write CLI unit tests using `CliRunner`

Deliverable: All CLI surfaces work end-to-end with mocked GitHub and LLM.

### Phase 4: Integration Testing and Backward Compatibility (Day 4)

- Run integration tests against real Anthropic API with fixture diffs
- Verify backward compatibility regression test passes (no `.tag-review.yaml` behavior unchanged)
- Test `--post-comments` against a sandbox GitHub repo
- Profile SQLite insert performance

Deliverable: Integration test suite passing; CI green.

### Phase 5: Documentation and Polish (Day 5)

- Write inline docstrings for all new public functions in `ci.py`
- Verify `tag ci review init` YAML is fully self-documenting (comments on every field)
- Update `docs/prd/INDEX.md` to reference PRD-061
- Final code review and merge

Deliverable: PRD-061 shipped and documented.

