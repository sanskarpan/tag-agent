# PRD-022: Diff-Aware Context Injection

**Status:** Proposed
**Priority:** P1
**Estimated Effort:** S (2–3 days)
**Affects:** `src/tag/context.py` (new `git_diff_filter`, `inject_diff` functions), `src/tag/controller.py` (extend `cmd_context` with `inject` subcommand and new flags), `src/tag/ci.py` (reuse `fetch_pr_diff` for `--pr` flag)

---

## 1. Overview

TAG's `context.py` manages session token budgets and compression, but provides no mechanism for scoping injected context to only the files changed in a git diff. When a developer is reviewing a PR, running a post-edit loop, or diagnosing a CI failure, the vast majority of workspace files are irrelevant — injecting the full workspace wastes tokens, inflates cost, and destroys prompt cache hit rates by mutating the user-turn content on every run.

This PRD introduces `tag context inject --git-diff`, a diff-aware variant of context injection that:

1. Shells out to `git diff` to enumerate only changed files.
2. Filters the changed file list against `blocked_patterns` (`.env`, `*.key`, `*.pem`, and user-configured patterns) before reading any content.
3. Injects the resulting diff blocks as a **user message turn** (never as a system prompt modification), so that the static system prompt remains unchanged across runs and qualifies for prompt caching at 0.1x input token cost.
4. Reports the token count of the injected diff before injection, warning if it exceeds 10k tokens.

The design is directly inspired by Aider's approach of personalizing context to the active diff using PageRank (PRD-021), but targets the simpler and higher-impact case of filtering raw diff output — no graph construction required, no soft dependencies, 2–3 days to ship.

---

## 2. Goals

1. **Git diff-based file filtering**: Parse `git diff --name-only` output to determine the set of changed files and limit context injection strictly to those files, ignoring all others in the working tree.
2. **Static system prompt preservation for caching**: Inject diff content as a user message turn so the system prompt bytes remain identical across runs, keeping them eligible for prompt cache hits at 0.1x cost.
3. **`blocked_patterns` filtering applied before content is read**: Match each changed filename against the configured `blocked_patterns` list (`.env`, `*.key`, `*.pem`, `*secret*`, `*credential*`, and any user-configured globs) before invoking `git diff` for that file's content. A file that matches a blocked pattern is never read and never appears in the injected context.
4. **Configurable context depth (`--context-lines`)**: Pass `-U<N>` to `git diff` to control how many lines of surrounding context are included around each change hunk, defaulting to 3 (standard unified diff) and supporting up to 50.
5. **CI integration via `--pr` flag**: Fetch a GitHub PR diff using the existing `ci.py` `fetch_pr_diff` function, apply the same filtering pipeline, and inject the result — enabling `tag context inject --pr 42` before `tag ci diagnose` in CI pipelines.
6. **`--staged` flag for pre-commit workflows**: When `--staged` is passed, diff against the index (`git diff --cached`) instead of `HEAD`, letting developers inject exactly the changes they are about to commit.
7. **`--max-files` cap to prevent runaway injection**: Refuse to inject more than `--max-files` changed files (default 10) in a single invocation, printing a warning listing the skipped files, so large refactoring commits do not silently flood the context window.
8. **Token counting and warning**: Count tokens in the assembled diff block before injection and print the count to stdout. Emit a warning if the count exceeds 10,000 tokens, prompting the user to reduce `--context-lines` or lower `--max-files`.

---

## 3. Non-Goals

- **Full file content injection** (existing behavior): Injecting complete file contents — rather than diff hunks — is the current default for other context workflows and is intentionally out of scope for this feature.
- **Semantic understanding of changes**: Ranking or filtering diff hunks by semantic relevance (e.g., PageRank over a call graph) is covered by PRD-021 and is not part of this PRD.
- **Merge conflict resolution**: Detecting or resolving `<<<<<<<` merge conflict markers in diffs is out of scope.
- **Binary file diff rendering**: Rendering meaningful context for binary file changes (images, compiled artifacts, archives) is not supported; binary files are silently skipped with a warning.
- **Cross-repo or remote-only diff fetching**: Only local git repositories and GitHub PRs via `gh` CLI are supported. GitLab, Bitbucket, and raw remote URLs are out of scope.
- **Automatic injection at session start**: This feature is an explicit CLI invocation, not an automatic hook. Auto-injection at `tag run` or `tag chat` start is a follow-on feature.

---

## 4. User Stories

**US-01 — Diff context in `tag review-pr`**
As a developer running `tag review-pr --repo owner/repo --pr 42`, I want to run `tag context inject --pr 42` beforehand so the reviewing agent sees only the changed files rather than a stale or full workspace snapshot, giving a more focused and accurate review.

**US-02 — Injecting only changed files before a loop run**
As a developer who just edited three files to fix a bug, I want to run `tag context inject --git-diff HEAD~1` before `tag run --profile coder "verify the fix"` so the agent receives only the relevant diff instead of the entire repo, cutting token usage by 90% and getting a cache hit on the system prompt.

**US-03 — Setting `--context-lines` for wider change context**
As a developer whose fix spans multiple functions separated by boilerplate, I want to run `tag context inject --git-diff HEAD~1 --context-lines 20` to include 20 lines of surrounding context around each hunk so the agent understands the full function signatures without me manually expanding the diff.

**US-04 — Excluding generated files from diff context**
As a developer working on a project that auto-generates `*.pb.go` and `*.generated.ts` files, I want to run `tag context inject --git-diff --exclude '*.generated.*' --exclude '*.pb.go'` so those noisy machine-generated files are never injected and do not consume my token budget.

**US-05 — Diff context in CI before `tag ci diagnose`**
As a CI pipeline engineer, I want to add `tag context inject --staged` to the pre-commit step and `tag context inject --git-diff origin/main` before `tag ci diagnose --log-file ci.log` in the pipeline so the diagnosing agent knows exactly which files changed in this push, leading to more precise failure attributions without injecting the entire repository.

**US-06 — Staged-only context for pre-commit review**
As a developer running a pre-commit hook, I want to run `tag context inject --staged` so only the files I have `git add`ed are included in the agent's context, keeping the review focused on what will actually be committed.

**US-07 — Token budget awareness before a long agent run**
As a developer about to start a multi-hour agent loop, I want `tag context inject --git-diff` to print the token count of the injected diff block so I can decide whether to reduce `--context-lines` or `--max-files` before committing to a costly run.

---

## 5. Proposed CLI Surface

The following flags are added to the existing `tag context inject` subcommand (which is introduced as part of this PRD alongside the existing `show`, `compress`, and `trim` subcommands):

```
tag context inject --git-diff [HEAD~1 | <commit-sha> | <branch>]
                   [--context-lines 10]
                   [--max-files 10]
                   [--exclude '*.generated.*']
                   [--format inline|block]

tag context inject --staged
                   [--context-lines 3]
                   [--max-files 10]
                   [--exclude <glob>]
                   [--format inline|block]

tag context inject --pr <number>
                   [--repo owner/name]
                   [--context-lines 3]
                   [--max-files 10]
                   [--exclude <glob>]
                   [--format inline|block]
```

**Flag reference:**

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--git-diff [ref]` | str, optional | `HEAD` | Diff target. Omitting the ref diffs working tree against HEAD. Provide a commit SHA, `HEAD~N`, or branch name to diff against that ref. |
| `--staged` | bool flag | false | Diff staged (index) changes only (`git diff --cached`). Mutually exclusive with `--git-diff` and `--pr`. |
| `--pr <number>` | int | — | Fetch PR diff from GitHub via `gh pr diff`. Requires `--repo` or `GH_REPO` env var. Mutually exclusive with `--git-diff` and `--staged`. |
| `--repo <owner/name>` | str | `GH_REPO` env | GitHub repository for `--pr`. |
| `--context-lines <N>` | int | `3` | Lines of surrounding context per hunk (`git diff -U<N>`). Accepted range: 0–50. |
| `--max-files <N>` | int | `10` | Maximum number of changed files to inject. Files beyond the cap are listed as skipped. |
| `--exclude <glob>` | str, repeatable | — | Additional glob patterns to exclude on top of `blocked_patterns`. May be specified multiple times. |
| `--format inline\|block` | str | `inline` | `inline`: inject raw unified diff. `block`: inject a human-readable summary with per-file change counts and the diff in a fenced code block. |
| `--profile <name>` | str | `master_profile` | Profile whose session receives the injection. |
| `--dry-run` | bool flag | false | Parse and filter the diff, print token count and file list, but do not inject. |

**Mutual exclusion:** `--git-diff`, `--staged`, and `--pr` are mutually exclusive. Providing more than one returns exit code 2 with a usage error.

---

## 6. Functional Requirements

**FR-01 — `git diff --name-only` parsing**
`git_diff_filter` must shell out to `git diff --name-only <base_ref>` (or `git diff --cached --name-only` for `--staged`) to enumerate changed files. The output is split on newlines, empty strings are discarded, and the resulting list is sorted lexicographically before further processing.

**FR-02 — `blocked_patterns` filtering applied before content is read**
Before any file content or per-file diff is fetched, each filename in the changed-file list must be matched against the union of the global `blocked_patterns` config list and any `--exclude` globs provided at the CLI. A file that matches any pattern is removed from the list silently. Matching uses `fnmatch.fnmatch` against the bare filename and the full relative path. This filtering step runs before `git diff -U<N> -- <file>` is ever invoked for that file.

**FR-03 — `.env` diff exposure prevention**
`.env`, `.env.*`, `*.env`, and any file whose basename is exactly `.env` must be unconditionally excluded regardless of `blocked_patterns` configuration. This rule is hardcoded and cannot be overridden by `--exclude` or config.

**FR-04 — Per-file diff retrieval with configurable context lines**
For each file that passes filtering, `git_diff_filter` must invoke `git diff -U<N> <base_ref> -- <file>` where `N` is the value of `--context-lines` (default 3). The output is captured and stored as a `(filename, diff_block)` tuple.

**FR-05 — `--max-files` cap enforcement**
After filtering, if the number of remaining files exceeds `--max-files`, the list is truncated to the first `--max-files` files (sorted lexicographically). A warning is printed listing all skipped filenames: `warning: skipped N files beyond --max-files cap: [file1, file2, ...]`.

**FR-06 — Inject as user message turn (not system prompt)**
`inject_diff` must format the diff content as a user message turn and pass it to Hermes via `hermes message add --role user`, not via any system-prompt modification path. This ensures the system prompt bytes remain static across runs, preserving cache eligibility for the system prompt at 0.1x input token cost.

**FR-07 — `--staged` flag for pre-commit context**
When `--staged` is passed, `git_diff_filter` must use `git diff --cached` instead of `git diff <base_ref>`. All other filtering, blocking, and injection logic applies identically.

**FR-08 — `--pr` flag via GitHub API integration**
When `--pr <number>` is passed, `inject_diff` must call `ci.fetch_pr_diff(repo, pr_number)` from `src/tag/ci.py` to obtain the raw unified diff string. This string is then parsed as a standard unified diff to extract per-file diff blocks, which are subjected to the same `blocked_patterns` filtering, `--max-files` cap, and format rendering as local diffs.

**FR-09 — Format options: `inline` vs `block`**
- `inline` (default): inject the raw unified diff text as-is, wrapped in a single fenced code block labeled `diff`.
- `block`: inject a structured summary. For each file: print a header line with the filename and `+N/-M` change counts, followed by the diff in a fenced code block. Prepend a one-line summary: `Changed files (K): file1, file2, …`.

**FR-10 — Token counting before injection**
Before calling `inject_diff`, `cmd_context_inject` must estimate the token count of the assembled diff string using a character-based approximation (`len(text) // 4`). Print the count to stdout: `diff context: ~<N> tokens across <K> files`. If the count exceeds 10,000 tokens, print a warning: `warning: diff context exceeds 10k tokens — consider --context-lines 0 or --max-files <lower>`.

**FR-11 — `--dry-run` mode**
When `--dry-run` is passed, perform all parsing, filtering, and token counting steps, print the file list and token count, but do not call `inject_diff` or invoke any Hermes command. Exit with code 0.

**FR-12 — Binary file detection and skip**
If a per-file `git diff` output begins with `Binary files` (the standard git binary diff marker), the file is silently skipped and added to a `skipped_binary` list. At the end of the run, print: `warning: skipped N binary file(s): [file1, file2, …]`.

**FR-13 — Exit codes**
- `0`: success, diff injected (or `--dry-run` completed).
- `1`: fatal error (git not found, Hermes injection failed, GitHub API error).
- `2`: usage error (mutually exclusive flags, invalid `--context-lines` range).
- `3`: nothing to inject (no changed files after filtering); print `info: no changed files to inject`.

---

## 7. Non-Functional Requirements

**NFR-01 — Diff parsing latency under 200ms**
The complete pipeline from `git diff --name-only` invocation through per-file diff retrieval and blocked-pattern filtering must complete in under 200ms for repositories with up to 500 changed files (measured on a cold filesystem cache). This excludes network latency for `--pr` mode.

**NFR-02 — `blocked_patterns` applied before any I/O on file content**
The filtering step (FR-02, FR-03) must execute and discard blocked filenames before any subprocess is spawned to read file content. There must be no code path where a blocked file's diff content is fetched and then discarded after the fact.

**NFR-03 — Token count reported to user on every injection**
Every non-dry-run invocation of `tag context inject --git-diff|--staged|--pr` must print the token count estimate and file list to stdout before returning. This is a user-facing contract, not an optional debug output.

**NFR-04 — No new mandatory runtime dependencies**
The feature must ship using only Python standard library modules (`subprocess`, `fnmatch`, `re`, `pathlib`) and existing TAG dependencies (`ci.py` for `--pr`). No new PyPI packages may be added as required dependencies.

**NFR-05 — Idempotent injection**
Calling `tag context inject --git-diff` twice with the same diff state must produce a single injection, not duplicate the diff block. `inject_diff` must check for a session marker (a comment header line `<!-- tag:diff-inject:<sha> -->`) and skip re-injection if the same base ref hash is already present in the session.

---

## 8. Technical Design

### 8.1 Changed Files

| File | Change |
|------|--------|
| `src/tag/context.py` | Add `git_diff_filter()` and `inject_diff()` functions |
| `src/tag/controller.py` | Add `inject` subparser to `tag context`; add `cmd_context_inject()` handler; wire to `cmd_context` dispatcher |

### 8.2 `git_diff_filter` Function Signature

```python
def git_diff_filter(
    base_ref: str,                  # e.g. "HEAD~1", a commit SHA, or a branch name
    *,
    staged: bool = False,           # use --cached instead of base_ref
    context_lines: int = 3,         # -U<N> for git diff
    max_files: int = 10,
    blocked_patterns: list[str],    # from config + --exclude CLI args
    cwd: Path | None = None,        # repo root; defaults to Path.cwd()
) -> tuple[list[tuple[str, str]], list[str], list[str]]:
    """Return ([(filename, diff_block)], skipped_blocked, skipped_binary)."""
```

**Implementation steps:**

1. Build the name-only command: `["git", "diff", "--name-only"] + (["--cached"] if staged else [base_ref])`.
2. Run via `subprocess.run(..., capture_output=True, text=True, timeout=30, cwd=cwd)`. Raise `RuntimeError` on non-zero exit.
3. Split stdout on `\n`, discard empty strings. Sort lexicographically.
4. For each filename:
   a. Check against hardcoded `.env` rule (FR-03). If matched, add to `skipped_blocked`, continue.
   b. Check against `blocked_patterns` using `fnmatch.fnmatch(filename, pat)` and `fnmatch.fnmatch(Path(filename).name, pat)`. If any match, add to `skipped_blocked`, continue.
   c. If `len(result_list) >= max_files`, add to `skipped_capped` and continue.
   d. Run `git diff -U<context_lines> [--cached | <base_ref>] -- <filename>`. If output starts with `Binary files`, add to `skipped_binary`, continue.
   e. Append `(filename, diff_output)` to result list.
5. Return `(result_list, skipped_blocked + skipped_capped, skipped_binary)`.

### 8.3 `inject_diff` Function Signature

```python
def inject_diff(
    hermes_bin_path: Path,
    profile_home: Path,
    diff_blocks: list[tuple[str, str]],
    base_ref: str,
    fmt: str = "inline",            # "inline" | "block"
) -> bool:
    """Inject diff as a user message turn. Returns True on success."""
```

**Implementation steps:**

1. Compute a deduplication key: `sha256(base_ref + "".join(f for f, _ in diff_blocks))[:12]`.
2. Build the message body:
   - `inline`: `f"<!-- tag:diff-inject:{key} -->\n```diff\n{combined_diff}\n```"` where `combined_diff` is the concatenation of all diff blocks separated by `\n`.
   - `block`: Build a structured message per FR-09.
3. Call `hermes message add --role user --content <body>` via `subprocess.run`.
4. Return `True` on exit code 0, `False` otherwise.

The key principle: the message is added as a **user turn**, not by modifying the system prompt file. The system prompt file on disk remains byte-for-byte identical between runs, preserving its cache eligibility.

### 8.4 `--pr` Flag Integration

When `--pr <number>` is passed:

1. Call `ci.fetch_pr_diff(repo, pr_number)` to obtain the raw unified diff string.
2. Parse the unified diff to extract per-file sections using a regex on `^diff --git a/(.*) b/` lines.
3. For each extracted `(filename, diff_block)` pair, apply the same `blocked_patterns` filter (FR-02, FR-03), binary check (FR-12), and `--max-files` cap (FR-05).
4. Pass the filtered list to `inject_diff`.

This reuses `fetch_pr_diff` from `ci.py` without modification.

### 8.5 Token Counting

Use a simple character-based approximation: `estimated_tokens = len(assembled_text) // 4`. This is consistent with the approximation used elsewhere in TAG and avoids importing a tokenizer library. Print the estimate; do not hard-block injection if the count exceeds the threshold — only warn.

### 8.6 Controller Integration

In `cmd_context` (controller.py), add a new branch:

```python
if sub == "inject":
    return cmd_context_inject(args)
```

`cmd_context_inject` orchestrates: load config, resolve `blocked_patterns`, call `git_diff_filter` or `ci.fetch_pr_diff`, print token count, call `inject_diff`, return exit code.

The `inject` subparser is added to the `context_sub` subparsers group alongside `show`, `compress`, and `trim`.

---

## 9. Security Considerations

**SC-01 — `blocked_patterns` applied before any file content is read**
The filtering step must be a pre-read gate, not a post-read filter. A file that matches a blocked pattern must never have `git diff -- <file>` invoked on it. This prevents secrets from ever entering process memory or being passed to a subprocess, even transiently.

**SC-02 — Hardcoded `.env` exclusion**
`.env` and its variants (`.env.local`, `.env.production`, `.env.*`, `*.env`) are unconditionally excluded by hardcoded logic that runs before config-derived `blocked_patterns` are evaluated. This rule cannot be disabled by user configuration.

**SC-03 — Entropy scan on diff content**
After per-file diff retrieval and before injection, run a simple entropy scan on each diff block to detect high-entropy strings (potential secrets in adjacent context lines that were not caught by filename filtering). Use a sliding window entropy check: any 40-character token with Shannon entropy > 4.5 bits/char triggers a warning: `warning: possible secret detected in diff of <filename> — review before injecting`. The user is not blocked but is warned. This catches cases where a context line adjacent to a real change happens to include an API key that was already in the file.

**SC-04 — No shell interpolation in subprocess calls**
All `subprocess.run` calls in `git_diff_filter` and `inject_diff` must use list-form arguments, never `shell=True`. The `base_ref` argument must be validated against `^[a-zA-Z0-9_.~^/:-]{1,256}$` before being passed to git to prevent argument injection.

**SC-05 — `--pr` diff trust boundary**
Diffs fetched via `--pr` are treated as untrusted external content. The same `blocked_patterns` filter and entropy scan (SC-03) are applied to PR diffs. The `fetch_pr_diff` function already uses `gh pr diff` (subprocess list-form), so no additional shell-injection surface is introduced.

**SC-06 — Deduplication key does not expose content**
The SHA-256 deduplication key stored in the injected comment header (SC-02 of section 8.3) is truncated to 12 hex characters, which provides collision resistance without exposing any content hash that could be used to fingerprint the diff.

---

## 10. Testing Strategy

**Unit tests in `tests/test_diff_context.py`:**

- **`test_git_diff_filter_name_only_parsing`**: Mock `subprocess.run` to return a known `--name-only` output; assert the returned filename list matches exactly.
- **`test_blocked_patterns_applied_before_content_read`**: Pass a filename matching a blocked pattern; assert that the per-file `git diff` subprocess is never called (use `unittest.mock.call_args_list`).
- **`test_env_file_unconditionally_blocked`**: Pass `.env`, `.env.local`, `.env.production` as changed files; assert all are in `skipped_blocked` regardless of `blocked_patterns` config.
- **`test_max_files_cap`**: Return 15 changed files from the mock; assert only 10 are in the result list and 5 are in `skipped_capped`.
- **`test_token_counting_prints_estimate`**: Capture stdout; assert the printed token estimate equals `len(assembled_text) // 4`.
- **`test_format_inline`**: Assert the injected message body contains a fenced `diff` code block.
- **`test_format_block`**: Assert the injected message body contains per-file `+N/-M` headers and a fenced code block per file.
- **`test_binary_file_skipped`**: Mock per-file `git diff` to return `Binary files a/foo.png and b/foo.png differ`; assert `foo.png` is in `skipped_binary` and not in result list.
- **`test_pr_flag_uses_fetch_pr_diff`**: Mock `ci.fetch_pr_diff`; assert it is called with the correct `repo` and `pr_number` arguments.
- **`test_staged_flag_uses_cached`**: Assert that `--staged` results in `["git", "diff", "--cached", "--name-only"]` being called (not `["git", "diff", "--name-only", "HEAD"]`).
- **`test_inject_as_user_turn`**: Assert that `inject_diff` calls `hermes message add --role user`, not any system-prompt modification command.
- **`test_entropy_scan_warning`**: Pass a diff block containing a high-entropy 40-character string; assert a warning is printed to stderr.
- **`test_dry_run_does_not_inject`**: Pass `--dry-run`; assert `inject_diff` is never called.
- **`test_base_ref_validation`**: Pass a `base_ref` containing shell metacharacters (e.g., `; rm -rf /`); assert `ValueError` is raised before any subprocess call.

---

## 11. Acceptance Criteria

**AC-01** — `tag context inject --git-diff HEAD~1` on a repository where 3 files changed produces an injection containing exactly those 3 files' diffs, and no other file content.

**AC-02** — A file named `.env` present in the `git diff --name-only` output is never passed to `git diff -- .env`; it appears in the CLI's warning output as skipped, and the process exits with code 0.

**AC-03** — `tag context inject --git-diff HEAD~1 --max-files 5` on a diff with 12 changed files injects exactly 5 files and prints a warning listing the 7 skipped files.

**AC-04** — `tag context inject --git-diff HEAD~1 --context-lines 20` results in `git diff -U20` being called for each changed file (verifiable via `--dry-run` output or test mock).

**AC-05** — The injected content is added as a `--role user` message turn; the system prompt file on disk is not modified by this command.

**AC-06** — `tag context inject --pr 42 --repo owner/repo` calls `ci.fetch_pr_diff("owner/repo", 42)` and applies the same blocked-patterns filter to the resulting diff.

**AC-07** — Running `tag context inject --git-diff` when there are no changed files prints `info: no changed files to inject` and exits with code 3.

**AC-08** — `tag context inject --staged` on a repository with 2 staged files and 5 unstaged files injects only the 2 staged files.

**AC-09** — Providing both `--git-diff` and `--staged` exits with code 2 and a usage error message without invoking any git subprocess.

**AC-10** — A diff containing a 40-character high-entropy string triggers a warning on stderr but does not block injection (exits with code 0).

---

## 12. Dependencies

| Dependency | Type | Notes |
|------------|------|-------|
| `git` (system binary) | Required | Must be on `PATH`. Verified at startup via `shutil.which("git")`; error if absent. |
| `ci.py` (`fetch_pr_diff`) | Internal | Reused for `--pr` flag. No new external code path. |
| `gh` CLI (system binary) | Required for `--pr` only | Already required by `tag review-pr` and `tag ci`. Not required for `--git-diff` or `--staged`. |
| `fnmatch` (stdlib) | Required | Used for blocked-pattern matching. |
| `hashlib` (stdlib) | Required | Used for deduplication key computation. |

No new PyPI packages are introduced.

---

## 13. Open Questions

**OQ-01 — Binary file diff handling**
The current design silently skips binary files. An alternative is to inject a one-line stub: `[binary file changed: foo.png, +12.4 KB]`. This gives the agent awareness that a binary changed without exposing its content. Decision needed before implementation.

**OQ-02 — Diff format for non-text changes**
Git diff for file renames (`git diff --name-only` shows the new name; the diff block shows `similarity index 100%`). Should rename-only diffs be injected as-is (they contain no content lines), or should they be reduced to a summary line `[renamed: old/path -> new/path]`?

**OQ-03 — Context injection order relative to workspace map**
PRD-021 introduces a workspace map injected at session start. If both `tag context inject --git-diff` and the workspace map are active, which appears first in the conversation? The diff context is likely more specific and should appear closer to the task prompt, suggesting it should be injected after the workspace map. Ordering policy needs to be defined and documented.

**OQ-04 — Idempotency across base-ref changes**
The deduplication key (section 8.3) is based on `base_ref` and filenames. If the user calls `--git-diff HEAD~1` twice on the same diff, injection is skipped the second time. But if the working tree changes between calls (a new file is added), the key changes and the diff is re-injected in full. Is this the right behavior, or should re-injection be a diff of diffs?

**OQ-05 — `--context-lines` upper bound**
The current upper bound is 50. For files with small total line counts, `--context-lines 50` may produce a full-file diff. Should a per-file line-count check cap the effective context lines to avoid duplicating full-file injection (which is the non-goal in section 3)?

---

## 14. Complexity and Timeline

**Complexity:** S (Small)

**Estimated timeline:** 2–3 days

| Day | Work |
|-----|------|
| Day 1 | Implement `git_diff_filter` in `context.py`; unit tests for parsing, blocked-patterns, max-files, binary skip |
| Day 2 | Implement `inject_diff` in `context.py`; wire `cmd_context_inject` in `controller.py`; add `inject` subparser; tests for format options, token counting, dry-run, staged flag |
| Day 3 | Implement `--pr` flag integration with `ci.py`; entropy scan; end-to-end CLI tests; documentation update |
