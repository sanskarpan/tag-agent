# PRD-059: SAST Vulnerability Auto-Remediation from SARIF (`tag ci fix-vuln`)

**Status:** Proposed
**Priority:** P2
**Estimated Effort:** M (1-2 weeks)
**Category:** CI/CD & Agentic Dev Workflows
**Affects:** `ci.py + security.py`
**Depends on:** PRD-020 (CI/CD integration), PRD-021 (agentic loop), PRD-028 (sandbox), PRD-013 (agent tracing), PRD-034 (secret scanning), PRD-033 (dependency-aware task queue), PRD-042 (architect-editor agent split)
**Inspired by:** GitHub CodeQL, Semgrep, Snyk SAST + AI fix

---

## 1. Overview

Static Application Security Testing (SAST) tools produce findings every day in modern CI pipelines. GitHub CodeQL, Semgrep, and Bandit are now standard fixtures in Python, JavaScript, and Go projects. These tools reliably identify SQL injection sinks, deserialization flaws, path traversal bugs, use of weak cryptographic primitives, hard-coded credentials, and dozens of other vulnerability classes. The output they produce — increasingly standardized as SARIF 2.1.0 — is precise: a rule ID, a severity rating, a file path, a line range, and often a fix suggestion or a dataflow trace. Yet despite this precision, the last mile remains entirely manual: a developer must read the finding, understand the vulnerable code pattern, understand the fix, locate the relevant code, apply the change, re-run the scanner to confirm the fix did not introduce a regression, and open a pull request. For a repository with 50 findings, this process can consume multiple engineering days.

`tag ci fix-vuln` closes this loop. It ingests any SARIF 2.1.0 file — whether produced by CodeQL, Semgrep, Bandit, or any other tool — parses each finding into a structured representation, and spawns a bounded agentic loop per finding (or per batch) to understand the vulnerability, read the relevant code context, generate a targeted fix, run any available linter or test suite to validate the fix, and optionally open a GitHub pull request. The command integrates with TAG's existing agent infrastructure: the agent loop from PRD-021, the sandbox from PRD-028, budget controls from PRD-039, tracing from PRD-013, and the PR helpers already in `ci.py`.

The system is designed around the ACI (Agent-Computer Interface) pattern described in SWE-agent (Princeton NLP, NeurIPS 2024). Rather than giving the remediation agent raw bash access, it exposes a narrow, structured tool harness: a windowed file viewer showing the vulnerable region in context, a line-targeted edit operation that runs the linter after each change, a test runner that reports only pass/fail counts to keep the context window manageable, and a fingerprint check to avoid re-processing findings already fixed in a previous run. This bounded harness is why SWE-agent scores roughly 2x on SWE-bench compared to raw bash: the model never has to reason about tool noise or context overflow.

SARIF 2.1.0 is the universal interchange format for SAST findings. Its schema is well-defined: `tool.driver.rules[]` define the rule metadata including help text, fix descriptions, and CWE tags; `results[].locations[].physicalLocation.region` gives exact file path, start line, end line, start column, and end column; `results[].relatedLocations[]` carries dataflow nodes for taint-tracking findings; and `results[].fingerprints` provides stable deduplication keys that survive line number drift between commits. TAG uses these fields faithfully to construct the agent prompt, deduplicate across repeated runs, and build the pull request description.

The feature supports three primary workflows: consuming an existing SARIF file produced by a CI job (`--sarif`), running a supported scanner in-process and then auto-remediating the results (`--run-scanner`), and dry-run mode to preview what would be done without touching the repository or creating any PRs. Each workflow terminates within a configurable agent budget — maximum steps, maximum wall-clock seconds, and maximum cost in USD — satisfying the three stopping conditions required for safe agentic loops (success, failure, budget exhausted). All findings, fix attempts, and agent decisions are stored in TAG's SQLite database at `~/.tag/runtime/tag.sqlite3` for auditability and cost reporting.

---

## 2. Problem Statement

### 2.1 SAST findings rot in backlogs

Modern security tooling creates findings faster than teams can remediate them. A medium-sized Python monorepo running Bandit and Semgrep in CI will accumulate 20-100 actionable findings per quarter. GitHub's own research shows the median time-to-fix for a SAST finding is 58 days for high-severity issues and over 200 days for medium-severity. The bottleneck is not detection — it is the manual effort required to understand the pattern, apply the correct fix, and verify the fix is complete. Teams accept the backlog as a cost of doing business, and CVE-tagged vulnerabilities sit unremediated in main until they are exploited or a compliance audit forces priority.

### 2.2 Manual fix workflows are error-prone at scale

Even when developers prioritize a SAST finding, the fix pattern is repetitive and mechanical: identify the sink, understand the safe API, replace the unsafe call, check for related sinks in the same file or function, run linter + tests. A developer doing this for the 10th time is just as error-prone as for the 1st because the task requires careful attention to context — the code around the finding, the function signature, the data flow from source to sink — not creative judgment. This is exactly the class of task where LLM agents, given proper context framing, perform well: bounded, context-rich, mechanical transformation with a clear correctness criterion (scanner no longer flags the location).

### 2.3 PR overhead discourages incremental security work

Even when a developer fixes a finding, the PR overhead — branch creation, commit message, PR body describing the CVE and the fix, linking to the finding, requesting review — adds 10-15 minutes of toil per finding. For a batch of 20 findings across 15 files, this toil can exceed the time spent writing the fixes themselves. The result is that developers batch fixes into large "security cleanup" PRs that are harder to review, harder to bisect on regression, and harder to deploy safely. `tag ci fix-vuln --auto-pr` eliminates this toil: each finding (or configurable batch) gets its own branch, commit, and PR with a fully-formed description that cites the rule ID, CWE, file location, and fix rationale — generated by the same agent that wrote the fix.

---

## 3. Goals

| # | Goal |
|---|------|
| G1 | Parse any valid SARIF 2.1.0 file and extract all findings with their full location, rule metadata, severity, message, and fingerprints, without requiring scanner-specific adapters. |
| G2 | For each finding (or configurable batch), spawn a bounded agentic loop using TAG's existing loop infrastructure that reads the vulnerable code, generates a targeted fix, and validates the fix using available linters and tests. |
| G3 | Deduplicate findings across repeated runs using SARIF fingerprints and a local SQLite record, so that already-fixed findings are never re-processed. |
| G4 | Optionally create one GitHub pull request per finding (or per batch), with a machine-generated PR body citing rule ID, CWE, CVSS severity, affected file, fix summary, and scanner verification status. |
| G5 | Support severity filtering (`--severity high,critical`) so teams can address the most critical findings first without processing the full backlog. |
| G6 | Support running a bundled scanner (`--run-scanner bandit`, `--run-scanner semgrep`) to produce SARIF output in-process, then immediately proceed to remediation, enabling a single-command "scan and fix" workflow. |
| G7 | Enforce agent budget limits (max steps, max cost USD, max wall seconds) per finding, with three well-defined stopping conditions: success (scanner no longer flags the location), failure (unrecoverable error or patch rejected by linter), and budget exhausted. |
| G8 | Store every finding, fix attempt, agent decision, diff applied, PR URL, and budget consumption record in SQLite for auditability. |
| G9 | Support dry-run mode that previews all findings that would be processed, their severities, affected files, and estimated cost, without touching the repository or calling external APIs. |
| G10 | Emit OpenTelemetry spans for each remediation loop (start, each agent step, fix validation, PR creation, end) compatible with PRD-013 and PRD-041. |

## 3.1 Non-Goals

| # | Non-Goal |
|---|----------|
| NG1 | Replacing the SAST scanner. TAG does not implement its own static analysis engine; it consumes SARIF produced by existing tools. |
| NG2 | Guaranteeing that the generated fix is semantically correct for all possible code patterns. The agent produces a best-effort fix; PR review by a human is always recommended before merge. |
| NG3 | Supporting non-SARIF input formats (e.g., raw Bandit JSON v1, Checkmarx XML, Fortify FPR) without conversion. Tools that support SARIF output are supported natively; others require an adapter outside TAG's scope. |
| NG4 | Auto-merging pull requests. `tag ci fix-vuln` creates PRs; it never merges them. Merge requires human approval. |
| NG5 | Running arbitrary code in the fixed repository without the sandbox. All test/lint validation runs inside the existing TAG sandbox (PRD-028). |
| NG6 | Supporting GitLab merge requests in this version. The PR creation path targets GitHub via `gh` CLI. GitLab MR support is a follow-on (see Open Questions). |
| NG7 | Providing a web UI for reviewing or approving fix PRs. PR review happens in GitHub. |
| NG8 | Storing the full SARIF file in SQLite. Only per-finding metadata and TAG's processing state are stored; the raw SARIF file is referenced by path. |

---

## 4. Success Metrics

| Metric | Target | Measurement Method |
|--------|--------|--------------------|
| SARIF parse coverage | Correctly parses 100% of SARIF 2.1.0 files from CodeQL, Semgrep, and Bandit test suites | Unit test suite over 15 real SARIF fixtures |
| Fix success rate (high-severity) | Agent produces a diff that causes the scanner to no longer flag the location for ≥ 60% of high/critical findings on the TAG test corpus | Integration test: re-run scanner on patched file |
| Deduplication accuracy | Zero duplicate fix PRs created when `fix-vuln` is run twice on the same SARIF without code changes between runs | Integration test: run twice, assert PR count = N (not 2N) |
| Agent budget adherence | No agent loop exceeds its configured `max_cost_usd` or `max_steps` | Unit test: mock agent, assert loop exits on budget signal |
| Dry-run zero side-effects | `--dry-run` produces no git commits, no PRs, no SQLite writes beyond the dry_run session row | Integration test: assert git log unchanged, PR count 0, SQLite diff = 1 row |
| PR description quality | PR body contains rule ID, CWE (if available), severity, file:line, and fix summary for every auto-created PR | Assertion test on PR body string |
| Scanner-in-process | `--run-scanner bandit` produces a valid SARIF file and proceeds to remediation in a single invocation | Integration test on a repo with known Bandit findings |
| OTel spans | Every invocation emits at minimum `fix_vuln.start`, `fix_vuln.finding.remediate` (one per finding), and `fix_vuln.end` spans | Unit test with mock OTel exporter |
| End-to-end latency | Processing a 10-finding SARIF with `--profile coder` completes in under 5 minutes wall-clock on a 16-core machine | Benchmark test with mocked LLM responses |
| Fingerprint dedup stability | Same finding fingerprinted identically across two SARIF runs produced by the same scanner version on the same commit | Unit test: parse same SARIF twice, assert fingerprint equality |

---

## 5. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|-----------|----------|
| U1 | Security engineer | run `tag ci fix-vuln --sarif results.sarif --profile coder --auto-pr` | Every actionable finding in the SARIF is addressed with a concrete code fix and a linked PR, without me writing a single line of code or PR description |
| U2 | Platform engineer | run `tag ci fix-vuln --sarif results.sarif --severity high,critical --dry-run` | I can preview exactly which findings will be processed, their estimated cost, and which files will be modified, before approving the remediation run |
| U3 | Developer | run `tag ci fix-vuln --run-scanner bandit --profile coder` | I can fix Bandit findings in my project with a single command that handles both the scan and the remediation |
| U4 | DevOps engineer | add `tag ci fix-vuln --sarif $SARIF_FILE --severity critical --auto-pr` to my GitHub Actions workflow | Critical SAST findings are automatically addressed with PRs every time CI runs, keeping the security backlog near zero |
| U5 | Security lead | run `tag ci fix-vuln --sarif results.sarif --batch 5` | Related findings in the same file are fixed in a single PR, reducing review overhead while keeping PRs reviewable |
| U6 | Developer | run `tag ci fix-vuln --sarif results.sarif --finding-id B101` | I can target a single rule class across all its instances to understand and fix a specific vulnerability pattern |
| U7 | Platform engineer | run `tag ci fix-vuln history` | I can see a ledger of all past remediation runs, per-finding outcomes, cost, and PR URLs for audit purposes |
| U8 | Developer | run `tag ci fix-vuln --sarif results.sarif --no-pr` | I get the code fixes applied to my working tree as local commits without needing GitHub access, for offline or pre-push use |
| U9 | Security auditor | query `vuln_findings` in `tag.sqlite3` | I have a full record of every finding TAG processed, whether it was fixed, the diff applied, and whether the PR was merged |
| U10 | Developer | see per-finding cost and step counts after a run | I can tune `--max-cost-usd` and `--max-steps` budgets for my team's cost tolerance |

---

## 6. Proposed CLI Surface

All `fix-vuln` subcommands live under the `tag ci` namespace, consistent with the existing `ci.py` command family.

### 6.1 Primary remediation command

```
tag ci fix-vuln \
  --sarif <path>            # Path to SARIF 2.1.0 file. Required unless --run-scanner is set.
  --profile <name>          # TAG profile to use for the remediation agent. Default: "coder".
  [--severity <list>]       # Comma-separated severity filter: info,low,medium,high,critical.
                            # Default: high,critical
  [--finding-id <rule_id>]  # Process only findings matching this SARIF rule ID (e.g. B101, CWE-89).
                            # Can be specified multiple times.
  [--batch <n>]             # Group n findings per agent invocation / per PR. Default: 1 (one PR per finding).
  [--auto-pr]               # Create a GitHub PR for each fix batch. Requires gh CLI authenticated.
  [--no-pr]                 # Apply fixes as local commits only. Overrides --auto-pr.
  [--dry-run]               # Preview findings and estimated cost; do not modify code or create PRs.
  [--max-steps <n>]         # Maximum agent loop steps per finding/batch. Default: 20.
  [--max-cost-usd <f>]      # Maximum LLM spend per finding/batch in USD. Default: 0.50.
  [--max-wall-sec <n>]      # Maximum wall-clock seconds per finding/batch. Default: 120.
  [--branch-prefix <s>]     # Git branch name prefix for fix branches. Default: "tag/fix-vuln".
  [--base-branch <s>]       # Base branch for PRs. Default: current branch.
  [--pr-draft]              # Create PRs as drafts. Default: false.
  [--yes]                   # Skip cost confirmation prompt.
  [--json]                  # Output machine-readable JSON.
  [--output <path>]         # Write full remediation report JSON to this file.
  [--workers <n>]           # Number of findings to process in parallel. Default: 1.
  [--skip-verify]           # Skip post-fix scanner re-run verification step.
  [--verify-cmd <cmd>]      # Custom command to verify fix (exit 0 = fixed). Overrides re-run default.
```

### 6.2 Run-scanner-then-fix

```
tag ci fix-vuln \
  --run-scanner bandit      # One of: bandit, semgrep. Runs scanner, captures SARIF, then remediates.
  --profile coder \
  [--scanner-args "--skip B104,B105"]   # Raw args forwarded to the scanner.
  [--sarif-out <path>]      # Save the generated SARIF to disk (optional).
  [--severity high,critical] \
  [--auto-pr]
```

### 6.3 History and status

```
# List all past fix-vuln runs
tag ci fix-vuln history [--last 20] [--json]

# Show details of a specific run
tag ci fix-vuln show <run_id>

# Show status of a specific finding (by fingerprint or rule_id+location)
tag ci fix-vuln status --fingerprint <fp>
```

### 6.4 Full example output (TTY mode)

```
$ tag ci fix-vuln --sarif results.sarif --severity high,critical --profile coder --auto-pr

Parsed SARIF: semgrep/2.1.0 — 47 findings total, 12 matching severity=high,critical
Deduplication: 3 already fixed (fingerprints in DB), 9 new findings to process

Estimated cost: 9 findings × ~$0.50 max = $4.50 max (actual will be lower)
Proceed? [y/N]: y

Finding 1/9  B608 sql-injection  src/api/users.py:142  [HIGH]
  └─ Spawning agent (profile: coder, max_steps=20, max_cost=$0.50)...
  └─ Step 1: open src/api/users.py 135
  └─ Step 4: edit 142:142 — replaced f-string query with parameterized query
  └─ Step 7: run_linter — 0 errors
  └─ Step 8: verify — scanner no longer flags line 142 ✓
  └─ Fixed in 8 steps / $0.12 / 34s
  └─ Branch: tag/fix-vuln/B608-users-py-142
  └─ PR #47: https://github.com/org/repo/pull/47

Finding 2/9  semgrep.python.jwt-decode-without-verify  src/auth/jwt.py:88  [CRITICAL]
  └─ Spawning agent...
  └─ Step 1: open src/auth/jwt.py 82
  └─ Step 5: edit 88:90 — added options={"verify_signature": True}
  └─ Step 9: verify — scanner no longer flags line 88 ✓
  └─ Fixed in 9 steps / $0.18 / 41s
  └─ PR #48: https://github.com/org/repo/pull/48

...

Summary
  Processed:   9 findings
  Fixed:        7  (77.8%)
  Failed:       1  (budget exhausted after $0.50, 20 steps — B307 eval-usage)
  Skipped:      1  (--skip-verify, diff applied but not verified)
  PRs created:  7
  Total cost:   $1.43
  Total time:   6m 12s

Run ID: fvr_8a3c2e1b
```

### 6.5 Dry-run output

```
$ tag ci fix-vuln --sarif results.sarif --severity high,critical --dry-run

DRY RUN — no code will be modified, no PRs will be created

Parsed SARIF: semgrep/2.1.0 — 47 findings total, 12 matching severity=high,critical
Deduplication: 3 already fixed, 9 new

Findings that would be processed:
  #  Rule ID                              Severity  File                       Line
  1  B608 sql-injection                   HIGH      src/api/users.py           142
  2  semgrep.python.jwt-decode...         CRITICAL  src/auth/jwt.py            88
  3  B324 use-of-md5                      HIGH      src/utils/hashing.py       23
  4  B301 blacklist-pickle                HIGH      src/cache/serialize.py     67
  5  CWE-078 os-injection                 CRITICAL  src/jobs/runner.py         199
  6  B602 subprocess-popen-with-shell     HIGH      src/deploy/hooks.py        34
  7  B506 yaml-load                       HIGH      src/config/loader.py       89
  8  B105 hardcoded-password-string       HIGH      src/auth/defaults.py       12
  9  semgrep.python.path-traversal        CRITICAL  src/files/download.py      55

Estimated cost: 9 × $0.50 max = $4.50 max
```

---

## 7. Functional Requirements

| ID | Requirement |
|----|-------------|
| FR-01 | **SARIF 2.1.0 parsing:** `parse_sarif(path: Path) -> SarifReport` must handle SARIF files from CodeQL, Semgrep, and Bandit without scanner-specific branches. It must extract `tool.driver.name`, `tool.driver.version`, `tool.driver.rules[]` (for rule metadata including `helpUri`, `shortDescription`, `tags`), and `results[]` (location, message, severity level, fingerprints, relatedLocations). Malformed SARIF must raise `SarifParseError` with the offending field path. |
| FR-02 | **Severity normalization:** SARIF severities vary by scanner (Semgrep uses `ERROR/WARNING/INFO`; Bandit uses `HIGH/MEDIUM/LOW`; CodeQL uses `error/warning/note/none`). `normalize_severity(raw: str) -> Severity` maps all variants to the canonical enum `Severity(critical, high, medium, low, info)`. The mapping table is defined in `security.py`. |
| FR-03 | **Fingerprint deduplication:** Before spawning any agent, `check_dedup(conn, fingerprint: str) -> bool` queries the `vuln_findings` table. If a row exists with `status = 'fixed'` or `status = 'pr_open'` for the same fingerprint, the finding is skipped. If no fingerprint is available in the SARIF result, a deterministic fingerprint is computed as `sha256(rule_id + ":" + file_path + ":" + start_line)`. |
| FR-04 | **Severity filtering:** `--severity` flag accepts a comma-separated list of canonical severity names. Findings not matching the filter are parsed and stored in `vuln_findings` with `status = 'skipped_severity'` but not processed by any agent. |
| FR-05 | **ACI agent harness:** Each remediation agent is given a structured tool set (not raw bash) consisting of: `view_file(path, start_line, end_line)` (100-line windowed viewer), `edit_file(path, start_line, end_line, new_content)` (line-targeted replace, runs linter after edit, blocks if linter error), `search_file(path, pattern)` (grep-like, returns matching lines with context), `run_linter(path)` (returns structured lint result), `run_tests(pattern)` (returns pass/fail count), `verify_fix(sarif_path, rule_id, file_path, line)` (re-runs scanner on the single file and confirms the finding is gone). |
| FR-06 | **Agent stopping conditions:** The loop implementing each finding remediation must implement exactly three stopping conditions: (a) **success** — `verify_fix` returns no finding at the target location; (b) **failure** — `edit_file` returns a linter error that cannot be resolved within the remaining budget, or the agent emits a `give_up` signal; (c) **budget** — `steps >= max_steps` OR `cost_usd >= max_cost_usd` OR `wall_seconds >= max_wall_sec`. Any loop that lacks all three conditions is a defect. |
| FR-07 | **Prompt construction:** `build_remediation_prompt(finding: SarifFinding, code_context: str) -> str` must include: the rule ID, rule short description, rule help URI (if available), CWE tag (if available in `rule.tags`), CVSS severity, the exact file path and line range, the SARIF result message, the code context (100 lines centred on the finding), and the full dataflow trace from `relatedLocations` if present. The prompt must end with an explicit instruction not to change any code outside the affected function scope unless the finding's dataflow trace spans multiple functions. |
| FR-08 | **Batch mode:** When `--batch N` is specified, up to N findings from the same file are grouped into a single agent invocation. The agent is given all N findings simultaneously and asked to produce a unified diff. A single commit and a single PR are created for the batch. Findings from different files are never batched together unless they share the same root cause rule ID and both files are under 500 lines. |
| FR-09 | **Auto-PR creation:** When `--auto-pr` is set and the agent produces a valid diff that passes `verify_fix`, the command: (1) creates a new git branch named `{branch_prefix}/{rule_id}-{file_slug}-{start_line}`; (2) applies the diff and commits with message `fix({rule_id}): remediate {rule_short_desc} in {file_path}:{start_line}`; (3) opens a PR via `gh pr create` with a body that includes: finding rule ID, CWE, severity, SARIF tool name, file:line, fix summary, and a note that the fix was generated by TAG CI and should be reviewed before merge. The PR body must include a `<!-- tag:fix-vuln run_id={run_id} -->` HTML comment for traceability. |
| FR-10 | **No-PR local commit mode:** When `--no-pr` is set (or `--auto-pr` is absent), fixes are applied as local commits on the current branch. The commit message follows the same format as FR-09 but omits PR creation. The branch is not pushed. |
| FR-11 | **Run-scanner integration:** When `--run-scanner bandit` is specified, the command runs `bandit -r . -f sarif -o /tmp/tag-bandit-{ts}.sarif {scanner_args}` and uses the resulting SARIF file as input. When `--run-scanner semgrep` is specified, it runs `semgrep --config auto --sarif --output /tmp/tag-semgrep-{ts}.sarif {scanner_args}`. Both must confirm the tool is installed before attempting to run it. If the scanner exits non-zero, the command exits 1 with the scanner's stderr. |
| FR-12 | **SQLite persistence:** Every finding parsed from SARIF is inserted into `vuln_findings` before any agent work begins. Every agent step is recorded in `vuln_fix_steps`. Every PR created is recorded in `vuln_prs`. The schema is defined in Section 9.2. `open_db()` from the existing codebase is used for all connections. |
| FR-13 | **Parallel workers:** When `--workers N` is specified (N > 1), up to N findings are processed concurrently using `ThreadPoolExecutor`. Each worker uses an independent SQLite connection. The main thread waits for all workers and then prints the final summary. Budget limits apply per-finding, not across the pool. |
| FR-14 | **Dry-run mode:** When `--dry-run` is set: parse SARIF, run deduplication, apply severity filter, print the findings table, print cost estimate, write one row to `vuln_fix_runs` with `mode = 'dry_run'`. Do not spawn any agent, do not modify any file, do not create any git object, do not create any PR. Exit code 0 if parsing succeeds, 1 on parse error. |
| FR-15 | **History subcommand:** `tag ci fix-vuln history` queries `vuln_fix_runs` joined with `vuln_findings` and displays: `run_id`, `run_at`, SARIF tool name, findings processed, fixed count, failed count, total cost, PR URLs. `--last N` limits to the N most recent runs. |
| FR-16 | **OTel tracing:** Every invocation emits spans using the existing `tracing.py` infrastructure. Span names follow `fix_vuln.*` convention. Attributes on `fix_vuln.finding.remediate` include `finding.rule_id`, `finding.severity`, `finding.file`, `finding.line`, `agent.steps`, `agent.cost_usd`, `fix.status`. |
| FR-17 | **Cost confirmation gate:** Before beginning any agent work, compute total estimated cost as `N_findings × max_cost_usd`. If the estimate exceeds `0.0` (always), print the estimate. If the estimate exceeds `fix_vuln.cost_warn_threshold_usd` (configurable, default $5.00), prompt for confirmation with `y/N` unless `--yes` or `CI=true` is set. |
| FR-18 | **Exit codes:** `0` — all targeted findings were successfully fixed (or already fixed); `1` — internal error (bad SARIF, missing tool, unrecoverable scanner error); `2` — one or more findings failed remediation (budget exhausted or linter rejected all patches); `3` — partial success (some fixed, some failed). |
| FR-19 | **Verify-cmd support:** When `--verify-cmd <cmd>` is specified, the agent uses `subprocess.run(shlex.split(cmd), ...)` instead of re-running the scanner to verify the fix. Exit code 0 means fixed; non-zero means still broken. This enables custom verification (e.g., running the project's own test suite). The command runs inside the sandbox when `--sandbox` is set. |
| FR-20 | **Finding status transitions:** Each finding in `vuln_findings` must follow the state machine: `new` → `skipped_severity | skipped_dedup | in_progress` → `fixed | failed | budget_exhausted`. The `status` column is updated atomically with `UPDATE ... WHERE id = ? AND status = 'in_progress'` to prevent race conditions in parallel mode. |

---

## 8. Non-Functional Requirements

| ID | Requirement |
|----|-------------|
| NFR-01 | **ACI tool latency:** Each ACI tool call (view, edit, search, lint) must complete in under 2 seconds for files under 2000 lines. The windowed viewer must never load more than 200 lines into the agent context per call. |
| NFR-02 | **SARIF parse performance:** `parse_sarif` must parse a 10,000-finding SARIF file in under 3 seconds on a standard laptop. Use `orjson` if available, falling back to `json`. |
| NFR-03 | **No network calls in dry-run:** `--dry-run` must make zero outbound network calls. All estimation is computed locally from the SARIF file and config. Assert with a test that mocks `socket.socket` to raise on any connection attempt. |
| NFR-04 | **Sandbox isolation:** When `--sandbox` is set (default: on when `sandbox.enabled` in config), all linter and test runner invocations use the existing `sandbox.py` subprocess isolation. Scanner re-run verification always runs outside the sandbox (the scanner binary reads the modified file from the working tree). |
| NFR-05 | **Git safety:** The command never operates on a dirty working tree without confirmation. Before creating any fix branch, it checks `git status --porcelain`. If there are uncommitted changes, it aborts with an actionable error message unless `--force-dirty` is set. |
| NFR-06 | **Idempotency:** Running `tag ci fix-vuln` twice on the same SARIF without any code changes between runs must produce zero new PRs and zero new agent invocations, because all findings are already marked `fixed` or `pr_open` in the database. |
| NFR-07 | **Atomic branch creation:** Each fix branch is created from the same base commit regardless of how many findings are processed in parallel. All branches are created before any agent begins work, ensuring no branch sees partially-fixed code from another parallel agent. |
| NFR-08 | **Log level discipline:** The ACI tool harness emits tool call arguments at `DEBUG` level and results (truncated to 200 chars) at `DEBUG` level. No tool argument values (which may contain code) are emitted at `INFO` or above. |
| NFR-09 | **TTY vs. non-TTY output:** In TTY mode, use Rich progress bars per finding. When piped or in CI, use plain-text per-finding status lines compatible with CI log parsers. In `--json` mode, emit a single JSON object at the end (no streaming). |
| NFR-10 | **Partial SARIF tolerance:** If a SARIF result is missing a fingerprint, location, or severity, the finding is still parsed with the available fields and processed with a logged warning. Only completely unparseable results (no rule ID, no location) are silently skipped with a count reported at the end. |

---

## 9. Technical Design

### 9.1 New and modified files

| File | Change type | Description |
|------|-------------|-------------|
| `src/tag/ci.py` | Modified | Add `cmd_fix_vuln`, ACI tool harness functions, scanner runner, PR creation logic |
| `src/tag/security.py` | Modified | Add `parse_sarif`, `SarifFinding`, `SarifReport`, `SarifParseError`, `normalize_severity`, `Severity`, severity mapping table |
| `src/tag/controller.py` | Modified | Wire `tag ci fix-vuln` subcommand and all flags in the CLI dispatch table |
| `src/tag/integrations/fix_vuln_aci.py` | New | ACI tool harness: `ViewFileTool`, `EditFileTool`, `SearchFileTool`, `LintTool`, `TestRunnerTool`, `VerifyFixTool` |

### 9.2 SQLite DDL

The following tables are created with `CREATE TABLE IF NOT EXISTS` in `ensure_fix_vuln_schema(conn)`, called from `cmd_fix_vuln` on startup.

```sql
-- One row per fix-vuln invocation
CREATE TABLE IF NOT EXISTS vuln_fix_runs (
    id            TEXT    PRIMARY KEY,           -- e.g. fvr_8a3c2e1b
    sarif_path    TEXT    NOT NULL,
    sarif_tool    TEXT    NOT NULL,              -- tool.driver.name
    sarif_version TEXT    NOT NULL,              -- tool.driver.version
    profile       TEXT    NOT NULL,
    severity_filter TEXT  NOT NULL,             -- comma-separated canonical levels
    mode          TEXT    NOT NULL DEFAULT 'run', -- 'run' | 'dry_run'
    total_findings INTEGER NOT NULL DEFAULT 0,
    fixed_count   INTEGER NOT NULL DEFAULT 0,
    failed_count  INTEGER NOT NULL DEFAULT 0,
    skipped_count INTEGER NOT NULL DEFAULT 0,
    total_cost_usd REAL    NOT NULL DEFAULT 0.0,
    total_steps   INTEGER NOT NULL DEFAULT 0,
    wall_seconds  REAL    NOT NULL DEFAULT 0.0,
    started_at    TEXT    NOT NULL,              -- ISO 8601 UTC
    finished_at   TEXT,
    status        TEXT    NOT NULL DEFAULT 'running' -- 'running'|'done'|'failed'
);
CREATE INDEX IF NOT EXISTS idx_vfr_started ON vuln_fix_runs(started_at);

-- One row per SARIF result finding
CREATE TABLE IF NOT EXISTS vuln_findings (
    id            TEXT    PRIMARY KEY,           -- uuid4 hex[:12]
    run_id        TEXT    NOT NULL REFERENCES vuln_fix_runs(id),
    rule_id       TEXT    NOT NULL,              -- e.g. B608, CWE-89
    rule_name     TEXT,
    severity      TEXT    NOT NULL,             -- canonical: critical|high|medium|low|info
    file_path     TEXT    NOT NULL,
    start_line    INTEGER NOT NULL,
    end_line      INTEGER,
    start_col     INTEGER,
    end_col       INTEGER,
    message       TEXT,
    fingerprint   TEXT,                          -- from SARIF result.fingerprints or computed
    cwe_ids       TEXT,                          -- comma-separated, e.g. "CWE-89,CWE-943"
    help_uri      TEXT,
    dataflow_json TEXT,                          -- JSON: relatedLocations[] if present
    status        TEXT    NOT NULL DEFAULT 'new',
        -- new | skipped_severity | skipped_dedup | in_progress | fixed | failed | budget_exhausted
    fix_diff      TEXT,                          -- unified diff of the applied fix
    pr_url        TEXT,
    pr_number     INTEGER,
    agent_steps   INTEGER,
    agent_cost_usd REAL,
    agent_wall_sec REAL,
    verified      INTEGER NOT NULL DEFAULT 0,   -- 1 if scanner confirmed fix
    created_at    TEXT    NOT NULL,
    updated_at    TEXT    NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_vf_run ON vuln_findings(run_id);
CREATE INDEX IF NOT EXISTS idx_vf_fingerprint ON vuln_findings(fingerprint);
CREATE INDEX IF NOT EXISTS idx_vf_status ON vuln_findings(status);
CREATE INDEX IF NOT EXISTS idx_vf_rule ON vuln_findings(rule_id);

-- One row per agent step during a finding remediation
CREATE TABLE IF NOT EXISTS vuln_fix_steps (
    id            TEXT    PRIMARY KEY,
    finding_id    TEXT    NOT NULL REFERENCES vuln_findings(id),
    step_num      INTEGER NOT NULL,
    tool_name     TEXT    NOT NULL,             -- view_file|edit_file|search_file|run_linter|run_tests|verify_fix
    tool_input    TEXT,                          -- JSON of tool arguments (no file content, only paths+lines)
    tool_result   TEXT,                          -- JSON summary of result (truncated to 500 chars)
    cost_usd      REAL    NOT NULL DEFAULT 0.0,
    wall_sec      REAL    NOT NULL DEFAULT 0.0,
    created_at    TEXT    NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_vfs_finding ON vuln_fix_steps(finding_id);

-- One row per PR created
CREATE TABLE IF NOT EXISTS vuln_prs (
    id            TEXT    PRIMARY KEY,
    finding_id    TEXT    NOT NULL REFERENCES vuln_findings(id),
    run_id        TEXT    NOT NULL REFERENCES vuln_fix_runs(id),
    repo          TEXT    NOT NULL,
    branch        TEXT    NOT NULL,
    base_branch   TEXT    NOT NULL,
    pr_number     INTEGER,
    pr_url        TEXT,
    pr_body_hash  TEXT,                          -- sha256 of PR body for idempotency
    draft         INTEGER NOT NULL DEFAULT 0,
    created_at    TEXT    NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_vpr_branch ON vuln_prs(branch);
```

### 9.3 Core dataclasses

```python
# src/tag/security.py additions

from __future__ import annotations
import enum
import hashlib
import json
from dataclasses import dataclass, field
from pathlib import Path
from typing import Optional


class Severity(enum.Enum):
    CRITICAL = "critical"
    HIGH     = "high"
    MEDIUM   = "medium"
    LOW      = "low"
    INFO     = "info"


_SEVERITY_MAP: dict[str, Severity] = {
    # SARIF / CodeQL
    "error":   Severity.HIGH,
    "warning": Severity.MEDIUM,
    "note":    Severity.LOW,
    "none":    Severity.INFO,
    # Semgrep (maps to SARIF level)
    "ERROR":   Severity.HIGH,
    "WARNING": Severity.MEDIUM,
    "INFO":    Severity.INFO,
    # Bandit property bag
    "CRITICAL": Severity.CRITICAL,
    "HIGH":     Severity.HIGH,
    "MEDIUM":   Severity.MEDIUM,
    "LOW":      Severity.LOW,
    # Canonical pass-through
    "critical": Severity.CRITICAL,
    "high":     Severity.HIGH,
    "medium":   Severity.MEDIUM,
    "low":      Severity.LOW,
    "info":     Severity.INFO,
}


def normalize_severity(raw: str) -> Severity:
    """Map scanner-specific severity strings to the canonical Severity enum."""
    return _SEVERITY_MAP.get(raw.strip(), Severity.MEDIUM)


@dataclass
class SarifLocation:
    file_path: str
    start_line: int
    end_line: Optional[int] = None
    start_col: Optional[int] = None
    end_col: Optional[int] = None


@dataclass
class SarifFinding:
    rule_id: str
    rule_name: Optional[str]
    severity: Severity
    message: str
    location: SarifLocation
    fingerprint: str                          # stable dedup key
    cwe_ids: list[str] = field(default_factory=list)
    help_uri: Optional[str] = None
    related_locations: list[dict] = field(default_factory=list)  # dataflow nodes

    @staticmethod
    def compute_fingerprint(rule_id: str, file_path: str, start_line: int) -> str:
        raw = f"{rule_id}:{file_path}:{start_line}"
        return hashlib.sha256(raw.encode()).hexdigest()[:16]


@dataclass
class SarifRule:
    id: str
    name: Optional[str]
    short_description: Optional[str]
    help_uri: Optional[str]
    cwe_ids: list[str]
    tags: list[str]


@dataclass
class SarifReport:
    tool_name: str
    tool_version: str
    rules: dict[str, SarifRule]              # rule_id → SarifRule
    findings: list[SarifFinding]
    raw_path: Path


class SarifParseError(ValueError):
    """Raised when SARIF parsing fails; message includes the offending field path."""
    pass
```

### 9.4 SARIF parser

```python
# src/tag/security.py additions (continued)

def parse_sarif(path: Path) -> SarifReport:
    """
    Parse a SARIF 2.1.0 file into a SarifReport.

    Supports CodeQL, Semgrep, and Bandit SARIF output.
    Raises SarifParseError on structural violations.
    """
    try:
        try:
            import orjson
            data = orjson.loads(path.read_bytes())
        except ImportError:
            import json as _json
            data = _json.loads(path.read_text(encoding="utf-8"))
    except (OSError, ValueError) as exc:
        raise SarifParseError(f"Cannot read/parse SARIF file {path}: {exc}") from exc

    runs = data.get("runs")
    if not isinstance(runs, list) or not runs:
        raise SarifParseError("$.runs must be a non-empty array")

    # Use the first run (CodeQL / Semgrep produce one run per invocation)
    run = runs[0]

    # --- Tool metadata ---
    try:
        driver = run["tool"]["driver"]
        tool_name    = driver.get("name", "unknown")
        tool_version = driver.get("version") or driver.get("semanticVersion", "unknown")
    except (KeyError, TypeError) as exc:
        raise SarifParseError(f"$.runs[0].tool.driver missing or malformed: {exc}") from exc

    # --- Rules ---
    rules: dict[str, SarifRule] = {}
    for raw_rule in driver.get("rules", []):
        rule_id = raw_rule.get("id", "")
        if not rule_id:
            continue
        tags_raw: list[str] = []
        props = raw_rule.get("properties", {})
        if isinstance(props, dict):
            tags_raw = props.get("tags", [])
        cwe_ids = [t for t in tags_raw if t.startswith("CWE-")]
        rules[rule_id] = SarifRule(
            id=rule_id,
            name=raw_rule.get("name"),
            short_description=(raw_rule.get("shortDescription") or {}).get("text"),
            help_uri=raw_rule.get("helpUri"),
            cwe_ids=cwe_ids,
            tags=tags_raw,
        )

    # --- Results / findings ---
    findings: list[SarifFinding] = []
    for idx, result in enumerate(run.get("results", [])):
        result_rule_id = result.get("ruleId", "")
        if not result_rule_id:
            # Try ruleIndex lookup
            ri = result.get("ruleIndex")
            if ri is not None:
                rule_list = driver.get("rules", [])
                if 0 <= ri < len(rule_list):
                    result_rule_id = rule_list[ri].get("id", "")
        if not result_rule_id:
            continue  # unparseable, skip with count

        # Severity: SARIF uses "level" at the result level; property bags vary
        raw_level = result.get("level", "warning")
        # Bandit stores severity in properties.issue_severity
        props = result.get("properties", {})
        if isinstance(props, dict) and "issue_severity" in props:
            raw_level = props["issue_severity"]
        severity = normalize_severity(raw_level)

        message_text = (result.get("message") or {}).get("text", "")

        # Location
        locations_list = result.get("locations", [])
        if not locations_list:
            continue
        ploc = (locations_list[0]
                .get("physicalLocation", {}))
        artifact_uri = (ploc.get("artifactLocation", {})
                        .get("uri", ""))
        region = ploc.get("region", {})
        start_line = region.get("startLine", 1)
        end_line   = region.get("endLine")
        start_col  = region.get("startColumn")
        end_col    = region.get("endColumn")

        # Fingerprint: prefer SARIF fingerprint, fall back to computed
        fps = result.get("fingerprints", {})
        fp = next(iter(fps.values()), None) if fps else None
        if not fp:
            fp = SarifFinding.compute_fingerprint(result_rule_id, artifact_uri, start_line)

        rule_meta = rules.get(result_rule_id)
        findings.append(SarifFinding(
            rule_id=result_rule_id,
            rule_name=rule_meta.name if rule_meta else None,
            severity=severity,
            message=message_text,
            location=SarifLocation(
                file_path=artifact_uri,
                start_line=start_line,
                end_line=end_line,
                start_col=start_col,
                end_col=end_col,
            ),
            fingerprint=fp,
            cwe_ids=rule_meta.cwe_ids if rule_meta else [],
            help_uri=rule_meta.help_uri if rule_meta else None,
            related_locations=result.get("relatedLocations", []),
        ))

    return SarifReport(
        tool_name=tool_name,
        tool_version=tool_version,
        rules=rules,
        findings=findings,
        raw_path=path,
    )
```

### 9.5 ACI tool harness (key functions)

```python
# src/tag/integrations/fix_vuln_aci.py

from __future__ import annotations
import shlex
import subprocess
import textwrap
from pathlib import Path
from typing import NamedTuple


class ViewResult(NamedTuple):
    content: str    # 100-line windowed view with line numbers
    total_lines: int
    first_line: int
    last_line: int


def view_file(path: Path, center_line: int, window: int = 100) -> ViewResult:
    """
    ACI windowed file viewer. Shows [center_line - window//2, center_line + window//2].
    Never loads more than window+10 lines into the caller's context.
    Line numbers are prefixed so the model can use them in edit_file calls.
    """
    try:
        lines = path.read_text(encoding="utf-8", errors="replace").splitlines()
    except OSError as exc:
        raise FileNotFoundError(f"Cannot open {path}: {exc}") from exc

    half = window // 2
    first = max(0, center_line - half - 1)
    last  = min(len(lines), center_line + half)
    window_lines = lines[first:last]
    numbered = "\n".join(
        f"{first + i + 1:6d}\t{line}"
        for i, line in enumerate(window_lines)
    )
    return ViewResult(
        content=numbered,
        total_lines=len(lines),
        first_line=first + 1,
        last_line=first + len(window_lines),
    )


class EditResult(NamedTuple):
    success: bool
    lint_errors: list[str]
    diff: str           # unified diff of the edit
    new_content: str    # full file content after edit


def edit_file(
    path: Path,
    start_line: int,
    end_line: int,
    new_content: str,
    linter_cmd: str | None = None,
) -> EditResult:
    """
    ACI line-targeted editor. Replaces lines [start_line, end_line] (1-indexed, inclusive)
    with new_content. Runs linter after edit; reverts the edit if linter reports errors.

    Returns EditResult. If lint fails, success=False and the file is left unchanged.
    """
    import difflib, tempfile, os

    original = path.read_text(encoding="utf-8", errors="replace")
    lines = original.splitlines(keepends=True)

    # Bounds check
    if start_line < 1 or start_line > len(lines) + 1:
        return EditResult(success=False, lint_errors=[f"start_line {start_line} out of range"], diff="", new_content=original)

    replacement_lines = [l if l.endswith("\n") else l + "\n" for l in new_content.splitlines()]
    new_lines = lines[:start_line - 1] + replacement_lines + lines[end_line:]
    new_text = "".join(new_lines)

    # Write to temp, run linter, only commit if clean
    with tempfile.NamedTemporaryFile(mode="w", suffix=path.suffix, delete=False, encoding="utf-8") as tmp:
        tmp.write(new_text)
        tmp_path = tmp.name

    lint_errors: list[str] = []
    if linter_cmd:
        result = subprocess.run(
            shlex.split(linter_cmd) + [tmp_path],
            capture_output=True, text=True, timeout=15,
        )
        if result.returncode != 0:
            lint_errors = (result.stdout + result.stderr).splitlines()[:20]

    os.unlink(tmp_path)

    if lint_errors:
        return EditResult(success=False, lint_errors=lint_errors, diff="", new_content=original)

    diff_lines = list(difflib.unified_diff(
        original.splitlines(keepends=True),
        new_text.splitlines(keepends=True),
        fromfile=f"a/{path}",
        tofile=f"b/{path}",
    ))
    path.write_text(new_text, encoding="utf-8")
    return EditResult(success=True, lint_errors=[], diff="".join(diff_lines), new_content=new_text)


def verify_fix(
    sarif_path: Path,
    scanner_name: str,
    rule_id: str,
    file_path: str,
    start_line: int,
    scanner_args: list[str] | None = None,
) -> bool:
    """
    Re-run the scanner on the modified file and check whether the finding is gone.
    Returns True if the finding is no longer present at the target location.
    """
    import tempfile, os
    out_path = Path(tempfile.mktemp(suffix=".sarif"))
    try:
        if scanner_name == "bandit":
            cmd = ["bandit", file_path, "-f", "sarif", "-o", str(out_path)] + (scanner_args or [])
        elif scanner_name == "semgrep":
            cmd = ["semgrep", "--config", "auto", "--sarif", "--output", str(out_path), file_path] + (scanner_args or [])
        else:
            return False  # unknown scanner, cannot verify

        result = subprocess.run(cmd, capture_output=True, timeout=60)
        if not out_path.exists():
            # scanner found nothing — no findings means the fix worked
            return True
        from tag.security import parse_sarif
        report = parse_sarif(out_path)
        for finding in report.findings:
            if finding.rule_id == rule_id and finding.location.file_path == file_path:
                if finding.location.start_line == start_line:
                    return False  # still present
        return True
    finally:
        if out_path.exists():
            out_path.unlink()
```

### 9.6 Remediation prompt template

```python
# src/tag/ci.py additions

_REMEDIATION_SYSTEM = textwrap.dedent("""\
    You are an expert security engineer and code remediator. You have been given a
    SAST finding and the vulnerable code context. Your task is to:

    1. Understand the vulnerability — what is the unsafe pattern, why is it exploitable,
       what is the attack vector.
    2. Identify the minimal, correct fix — prefer library functions or parameterized APIs
       over manual sanitization. Do not introduce new logic unless necessary.
    3. Apply the fix using the available tools (edit_file, view_file, search_file).
    4. Validate the fix using run_linter and verify_fix.
    5. Do NOT modify code outside the function(s) identified in the finding unless the
       dataflow trace explicitly shows the vulnerability spans multiple functions.
    6. Do NOT introduce new imports unless they are part of the standard library or already
       present in the file's existing imports.

    Signal completion by calling verify_fix. If verify_fix returns True, you are done.
    If you cannot produce a correct fix within the available budget, call give_up with
    a brief explanation.
""")


def build_remediation_prompt(finding: "SarifFinding", code_context: str) -> str:
    cwe_str = ", ".join(finding.cwe_ids) if finding.cwe_ids else "N/A"
    dataflow_str = ""
    if finding.related_locations:
        nodes = [
            f"  [{i+1}] {loc.get('message', {}).get('text', '')} "
            f"@ {loc.get('physicalLocation', {}).get('artifactLocation', {}).get('uri', '')}:"
            f"{loc.get('physicalLocation', {}).get('region', {}).get('startLine', '?')}"
            for i, loc in enumerate(finding.related_locations[:10])
        ]
        dataflow_str = "\n\nDataflow Trace:\n" + "\n".join(nodes)

    return textwrap.dedent(f"""\
        {_REMEDIATION_SYSTEM}

        ## Finding

        - **Rule ID:** {finding.rule_id}
        - **Rule Name:** {finding.rule_name or 'N/A'}
        - **Severity:** {finding.severity.value.upper()}
        - **CWE:** {cwe_str}
        - **File:** {finding.location.file_path}
        - **Lines:** {finding.location.start_line}–{finding.location.end_line or finding.location.start_line}
        - **Message:** {finding.message}
        - **Help:** {finding.help_uri or 'N/A'}
        {dataflow_str}

        ## Vulnerable Code (with line numbers)

        ```
        {code_context}
        ```

        Begin remediation now. Call view_file first if you need more context.
    """)
```

### 9.7 Agentic loop integration

The main remediation loop in `cmd_fix_vuln` delegates to `loop_agent.py` (PRD-021) with a restricted tool set. The loop config is:

```python
@dataclass
class RemediationLoopConfig:
    max_steps: int       = 20
    max_cost_usd: float  = 0.50
    max_wall_sec: float  = 120.0
    profile: str         = "coder"
    scanner_name: str    = "bandit"  # for verify_fix
    scanner_args: list[str] = field(default_factory=list)
    linter_cmd: str | None = None    # e.g. "ruff check --fix"
    sandbox: bool        = True
```

The loop is called as:

```python
result = run_remediation_loop(
    cfg=loop_cfg,
    finding=finding,
    code_context=view_file(Path(finding.location.file_path),
                           finding.location.start_line).content,
    conn=conn,
)
```

`run_remediation_loop` returns a `RemediationResult(status, diff, steps, cost_usd, wall_sec, verified)`. Status is one of `"fixed"`, `"failed"`, `"budget_exhausted"`.

---

## 10. Security Considerations

1. **No shell injection in scanner invocation.** Both `bandit` and `semgrep` are invoked via `subprocess.run(list_form, ...)`, never through `shell=True`. User-supplied `--scanner-args` are passed through `shlex.split` and then appended as list elements to prevent argument injection.

2. **Sandbox for linter and test validation.** `edit_file`'s linter invocation and `run_tests` both route through the existing `sandbox.py` subprocess isolation (PRD-028) when `sandbox.enabled = true` in config. The scanner re-run for `verify_fix` runs outside the sandbox because it is a read-only inspection operation, not code execution.

3. **No LLM prompt injection via SARIF.** The `message` and `rule_name` fields from the SARIF file are inserted into the remediation prompt but are always placed in clearly-delimited Markdown sections, never in positions where a crafted SARIF message could masquerade as a system prompt. This is enforced by the fixed structure of `build_remediation_prompt`.

4. **No credential exposure in git commits.** Before committing a fix diff, the command runs `security.scan_text(diff, Path("fix.diff"))` (PRD-034 secret scanner) on the diff text. If any secret pattern or high-entropy string is found in the diff, the commit is aborted and the engineer is prompted to review.

5. **PR body content sanitization.** The auto-generated PR body is constructed from structured fields (rule ID, severity, file path, line number, fix summary) and never includes raw SARIF `message.text` without HTML escaping, since the message field can contain angle brackets or markdown syntax.

6. **SARIF file path traversal prevention.** `artifact_uri` values from the SARIF file are validated to be relative paths within the repository root using `Path(artifact_uri).resolve().is_relative_to(repo_root)` before any file operations. Absolute paths and `..` traversal are rejected.

7. **Budget cap hard ceiling.** `max_cost_usd` is read from the CLI flag and capped at `min(flag_value, config.fix_vuln.max_cost_usd_ceiling)` (default ceiling: $5.00 per finding). This prevents a malformed budget flag from authorizing runaway spend.

8. **No plaintext storage of scanner output.** The SARIF file is parsed in memory; its raw content is never written to SQLite. Only structured metadata fields (rule ID, severity, file, line, fingerprint) are persisted.

9. **Branch name sanitization.** Fix branch names are constructed as `f"{prefix}/{rule_id}-{file_slug}-{start_line}"` where `rule_id` and `file_slug` are sanitized with `re.sub(r'[^A-Za-z0-9_\-.]', '-', value)` to prevent git branch name injection.

10. **HMAC-free SARIF (local files only).** SARIF files are always consumed from the local filesystem by path. There is no webhook-style SARIF ingestion in this PRD. Remote SARIF fetch is an explicit non-goal to avoid SSRF and credential-in-URL risks.

---

## 11. Testing Strategy

### 11.1 Unit tests (`tests/test_prd_059_fix_vuln.py`)

| Test | Description |
|------|-------------|
| `test_parse_sarif_codeql` | Parse a 25-finding CodeQL SARIF fixture; assert tool name, rule count, finding count, severity distribution |
| `test_parse_sarif_semgrep` | Parse a 10-finding Semgrep SARIF fixture; assert ERROR maps to HIGH, WARNING to MEDIUM |
| `test_parse_sarif_bandit` | Parse a 15-finding Bandit SARIF fixture; assert `issue_severity` property bag is used for severity |
| `test_parse_sarif_missing_runs` | Assert `SarifParseError` raised with field path when `runs` is absent |
| `test_parse_sarif_missing_location` | Assert finding with no location is silently skipped, count reported |
| `test_normalize_severity_all_variants` | Exhaustive table test of all known severity strings across all three scanners |
| `test_compute_fingerprint_deterministic` | Same rule_id + file + line produces identical fingerprint across calls |
| `test_compute_fingerprint_differs` | Different line numbers produce different fingerprints |
| `test_view_file_window` | 200-line file, center_line=150, window=100: assert returned lines are 100–200, prefixed with numbers |
| `test_view_file_near_start` | center_line=5, window=100: assert window starts at line 1 (no negative indexing) |
| `test_edit_file_success` | Replace lines 10:10 with clean code; assert diff is non-empty, file is modified, linter=None skips lint |
| `test_edit_file_linter_rejects` | Replace with syntactically broken code; assert success=False, file unchanged |
| `test_edit_file_bounds_check` | start_line > file length: assert success=False without touching file |
| `test_severity_filter` | 5 findings: 2 HIGH, 2 MEDIUM, 1 LOW; `--severity high` passes only 2 |
| `test_dedup_skip` | Insert fingerprint with status='fixed'; assert `check_dedup` returns True |
| `test_dedup_new` | No matching row; assert `check_dedup` returns False |
| `test_budget_max_steps` | Mock agent takes 25 steps; assert loop exits at max_steps=20 with status='budget_exhausted' |
| `test_budget_max_cost` | Mock agent accumulates $0.60 in 3 steps; assert loop exits at max_cost_usd=0.50 |
| `test_stopping_success` | verify_fix returns True on step 8; assert loop exits with status='fixed' |
| `test_build_remediation_prompt_fields` | Assert prompt contains rule_id, CWE, severity, file:line, code_context |
| `test_build_remediation_prompt_no_cwe` | CWE list empty: assert "N/A" appears in prompt, no KeyError |
| `test_scanner_args_no_shell_injection` | `--scanner-args "$(rm -rf /)"`: assert subprocess receives literal string, no shell=True |
| `test_branch_name_sanitization` | rule_id with special chars: assert branch name matches `[A-Za-z0-9/_\-\.]+` |
| `test_pr_body_contains_fields` | Assert PR body contains rule_id, severity, file path, fix summary, HTML comment tag |
| `test_secret_scan_on_diff` | Diff containing `sk-ant-...` pattern: assert commit aborted with secret found error |
| `test_artifact_uri_traversal` | `artifact_uri = "../../../etc/passwd"`: assert rejected before any file I/O |

### 11.2 Integration tests (`tests/integration/test_fix_vuln_integration.py`)

Require: Python, bandit, semgrep (skipped with `pytest.mark.skipif` if not installed), SQLite.

| Test | Description |
|------|-------------|
| `test_end_to_end_bandit_fix` | Write a file with a known B608 SQL injection pattern; run `tag ci fix-vuln --run-scanner bandit --no-pr`; assert file modified, scanner no longer flags line, `vuln_findings.status = 'fixed'` in DB |
| `test_end_to_end_sarif_input` | Write a SARIF file referencing the same B608 file; run with `--sarif`; same assertions |
| `test_dry_run_no_side_effects` | Run `--dry-run` on a SARIF with 3 findings; assert 0 git commits, 0 PRs, 0 modified files, 1 `vuln_fix_runs` row with mode='dry_run' |
| `test_dedup_second_run` | Run twice on same SARIF; assert only N PRs (not 2N), second run `vuln_findings.status = 'skipped_dedup'` |
| `test_history_command` | Run twice; run `tag ci fix-vuln history --last 2 --json`; assert 2 run rows in output |
| `test_parallel_workers` | 4 findings, `--workers 2`; assert all 4 processed, no SQLite write conflicts |
| `test_severity_filter_integration` | SARIF with 2 HIGH + 3 LOW; `--severity high`; assert only 2 agents spawned |
| `test_auto_pr_body_format` | With mocked `gh pr create`; assert PR body contains all required fields |

### 11.3 Performance tests

| Test | Target |
|------|--------|
| `bench_parse_sarif_10k` | Parse a 10,000-finding SARIF in < 3 seconds |
| `bench_view_file_5k_lines` | `view_file` on a 5,000-line file at center_line=2500 in < 50ms |
| `bench_dedup_10k_findings` | `check_dedup` × 10,000 sequential calls in < 1 second (SQLite index hit) |

---

## 12. Acceptance Criteria

| ID | Criterion | Test Method |
|----|-----------|-------------|
| AC-01 | `parse_sarif` correctly parses all findings from CodeQL, Semgrep, and Bandit SARIF test fixtures with zero assertion failures | Unit: `test_parse_sarif_*` |
| AC-02 | `--dry-run` produces zero git commits, zero PRs, zero file modifications, and exits 0 | Integration: `test_dry_run_no_side_effects` |
| AC-03 | Running `fix-vuln` twice on the same SARIF without code changes produces no duplicate PRs and no duplicate agent invocations | Integration: `test_dedup_second_run` |
| AC-04 | Every remediation loop exits within its configured `max_steps`, `max_cost_usd`, and `max_wall_sec` limits | Unit: `test_budget_*` |
| AC-05 | A diff containing a known secret pattern (from PRD-034 pattern library) is rejected before commit | Unit: `test_secret_scan_on_diff` |
| AC-06 | `artifact_uri` values containing `..` or absolute paths are rejected before any file operation | Unit: `test_artifact_uri_traversal` |
| AC-07 | Scanner commands are never invoked with `shell=True`; user-supplied `--scanner-args` cannot achieve command injection | Unit: `test_scanner_args_no_shell_injection` |
| AC-08 | `--severity high,critical` filters out medium/low/info findings; filtered findings are stored in DB with `status = 'skipped_severity'` | Unit+Integration: severity filter tests |
| AC-09 | `--auto-pr` creates a PR with body containing rule_id, CWE (if available), severity, file:line, fix summary, and `<!-- tag:fix-vuln ... -->` traceability comment | Integration: `test_auto_pr_body_format` |
| AC-10 | `--workers 2` processes 4 independent findings in parallel without SQLite write conflicts or data corruption | Integration: `test_parallel_workers` |
| AC-11 | `tag ci fix-vuln history` returns a JSON array with `run_id`, `sarif_tool`, `fixed_count`, `failed_count`, `total_cost_usd` for each past run | Integration: `test_history_command` |
| AC-12 | Exit code is 0 when all targeted findings are fixed, 2 when any finding fails or exhausts budget, 3 on partial success | Integration: exit code assertions in end-to-end tests |
| AC-13 | A 10-finding SARIF with mocked LLM responses completes in under 60 seconds wall-clock with `--workers 1` | Performance: `bench_*` |
| AC-14 | OTel spans `fix_vuln.start`, `fix_vuln.finding.remediate`, and `fix_vuln.end` are emitted for every invocation | Unit: span name assertion with mock exporter |
| AC-15 | `--run-scanner bandit` invokes bandit via subprocess list form, captures SARIF output, and proceeds to parse + remediate | Integration: `test_end_to_end_bandit_fix` |

---

## 13. Dependencies

| Dependency | Type | Version | Notes |
|------------|------|---------|-------|
| `bandit` | Optional runtime | >= 1.7.0 | Required for `--run-scanner bandit`; checked at invocation, not import time |
| `semgrep` | Optional runtime | >= 1.0.0 | Required for `--run-scanner semgrep`; checked at invocation |
| `gh` CLI | Optional runtime | >= 2.0.0 | Required for `--auto-pr`; checked before PR creation attempt |
| `orjson` | Optional Python | >= 3.9.0 | Faster SARIF parsing; falls back to stdlib `json` if absent |
| `ruff` | Optional runtime | >= 0.4.0 | Default linter for Python files in `edit_file`; falls back to no-op if absent |
| PRD-021 | Internal | — | Agent loop infrastructure: `loop_agent.py` |
| PRD-028 | Internal | — | Sandbox isolation for linter and test runner |
| PRD-013 | Internal | — | OTel tracing: `tracing.py` span creation |
| PRD-034 | Internal | — | Secret scanning: `security.py` `scan_text` used on fix diffs |
| PRD-039 | Internal | — | Budget enforcement: `budget.py` cost tracking |
| PRD-033 | Internal | — | Dependency-aware task queue for `--workers N` ordering |
| PRD-042 | Internal | — | Architect-editor split pattern used by remediation agent harness |
| SQLite WAL | Infrastructure | — | `open_db()` from existing codebase; WAL mode required for `--workers N` |
| GitHub Issue #344 | Reference | — | Feature tracking issue |

---

## 14. Open Questions

| # | Question | Owner | Target Date |
|---|----------|-------|-------------|
| OQ-1 | Should `--batch N` be bounded by file (only findings in the same file are batched) or by rule class (all instances of the same rule ID)? File-bounded batches produce tighter diffs; rule-bounded batches may be more useful for education. | Engineering lead | Before implementation start |
| OQ-2 | What is the right default for `--max-cost-usd`? $0.50/finding allows ~3,000 tokens of context and ~15 model calls at Sonnet pricing. Is this sufficient for complex taint-tracking findings (CWE-89, CWE-79)? | Security team | After pilot run on internal codebase |
| OQ-3 | Should `verify_fix` re-run the full scanner on the repository or only on the modified file? Full-repo scan is more accurate (catches fix regressions elsewhere) but 10-100x slower. File-scoped is the default; full-repo as opt-in flag. | Engineering | Before FR-05 implementation |
| OQ-4 | GitLab MR support: the `gh pr create` path is GitHub-specific. Should GitLab MR creation be in scope for v1 or deferred? Uses `glab mr create`. PRD-020 already calls out GitLab; the CI lint API integration exists. | Product | Sprint planning |
| OQ-5 | Should a failing `verify_fix` (finding still present after agent edit) cause the commit to be reverted automatically, or left on the fix branch for human review? Current plan: leave it for human review with `[UNVERIFIED]` prefix in PR title. | Engineering | Before AC-09 is finalized |
| OQ-6 | How should `--batch N > 1` handle the case where one finding in the batch fails and others succeed? Options: (a) create PR only for the successfully-fixed subset; (b) fail the whole batch; (c) commit partial fixes with warning. | Engineering lead | Before FR-08 implementation |
| OQ-7 | Is `semgrep --config auto` the right default scan config, or should users be required to provide an explicit ruleset? `--config auto` requires network access to the Semgrep registry; an offline default (e.g., `--config p/python`) may be preferable for air-gapped environments. | DevOps | Before FR-11 implementation |
| OQ-8 | Should the ACI harness expose a `run_tests` tool by default, or make it opt-in via `--with-tests`? Running tests is high-latency (potentially minutes) and may be inappropriate for security-only remediation tasks. | Engineering | Before fix_vuln_aci.py implementation |

---

## 15. Complexity and Timeline

**Total estimated effort:** M (1–2 weeks, ~8–12 engineering days)

### Phase 1 — SARIF parsing and security.py extensions (Days 1–2)

- Implement `SarifFinding`, `SarifReport`, `SarifRule`, `SarifParseError`, `Severity`, `normalize_severity` dataclasses and enums in `security.py`
- Implement `parse_sarif(path)` with full CodeQL/Semgrep/Bandit coverage
- Implement `compute_fingerprint` and `check_dedup`
- Write unit tests: `test_parse_sarif_*`, `test_normalize_severity_*`, `test_compute_fingerprint_*`
- Deliverable: `parse_sarif` passes all fixture tests

### Phase 2 — SQLite schema and persistence layer (Day 3)

- Implement `ensure_fix_vuln_schema(conn)` with all four tables and indexes
- Implement insert helpers: `insert_run`, `insert_finding`, `insert_step`, `insert_pr`
- Implement `update_finding_status` with atomic WHERE clause
- Write unit tests: schema creation, dedup query, status transition
- Deliverable: All DB operations pass unit tests; schema created cleanly on fresh DB

### Phase 3 — ACI tool harness (Days 4–5)

- Implement `view_file`, `edit_file`, `search_file`, `run_linter`, `verify_fix` in `fix_vuln_aci.py`
- Integrate `sandbox.py` for linter invocations
- Implement scanner invocation for `--run-scanner bandit` and `--run-scanner semgrep`
- Implement `build_remediation_prompt`
- Write unit tests: all ACI tool unit tests, scanner arg injection test, branch sanitization test
- Deliverable: ACI tools pass unit tests; linter integration confirmed with ruff

### Phase 4 — Agentic loop integration and `cmd_fix_vuln` (Days 6–8)

- Implement `RemediationLoopConfig` and `run_remediation_loop` integrating `loop_agent.py`
- Implement `cmd_fix_vuln` in `ci.py`: arg parsing, severity filter, dedup, parallel worker dispatch, summary output, OTel spans
- Implement `--dry-run` mode
- Wire up `tag ci fix-vuln` in `controller.py`
- Implement `tag ci fix-vuln history` and `show` subcommands
- Write integration tests: `test_end_to_end_bandit_fix`, `test_dry_run_no_side_effects`, `test_dedup_second_run`
- Deliverable: Full CLI surface works end-to-end on a test repository

### Phase 5 — Auto-PR, secret scanning gate, and parallel workers (Days 9–10)

- Implement `create_fix_pr` using `gh pr create` with structured PR body
- Implement secret scan gate on fix diff (PRD-034 integration)
- Implement `artifact_uri` path traversal validation
- Implement `--workers N` parallel dispatch with `ThreadPoolExecutor`
- Implement `--batch N` finding grouping
- Write integration tests: `test_auto_pr_body_format`, `test_parallel_workers`, `test_secret_scan_on_diff`, `test_artifact_uri_traversal`
- Deliverable: Auto-PR creation confirmed with mocked `gh`; secret scan gate confirmed; parallel mode confirmed

### Phase 6 — Polish, performance tests, and documentation (Days 11–12)

- Rich TTY output: progress bar per finding, color-coded status
- JSON output mode (`--json`, `--output`)
- Performance tests: `bench_parse_sarif_10k`, `bench_view_file`, `bench_dedup`
- OTel span coverage verification
- Cost confirmation gate and `--yes` / `CI=true` bypass
- Final acceptance criteria pass
- Deliverable: All 15 AC rows pass; performance benchmarks meet targets

---

*GitHub Issue:* [#344](https://github.com/org/repo/issues/344)
*Document version:* 1.0 — 2026-06-17

