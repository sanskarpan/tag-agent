# PRD-058: GitHub Actions Workflow Scaffold (`tag ci install-action`)

**Status:** Proposed
**Priority:** P2
**Estimated Effort:** XS (1-2 days)
**Category:** CI/CD & Agentic Dev Workflows
**Affects:** `ci.py + templates/`
**Depends on:** PRD-020 (CI/CD integration), PRD-027 (eval framework), PRD-028 (sandbox code execution), PRD-013 (agent tracing/observability), PRD-034 (secret scanning), PRD-016 (webhook event triggers)
**Inspired by:** Braintrust GitHub Action, LangSmith CI, various GH action generators

---

## 1. Overview

TAG ships with a powerful suite of CI-oriented commands — `tag ci diagnose`, `tag ci commit-lint`, `tag review-pr`, `tag eval run` — but adopting any of them inside a real GitHub Actions pipeline today requires engineers to hand-author YAML from scratch. They must know the correct runner image, the right secrets to expose, how to call `tag setup --skip-tui-build` in a headless environment, which exit codes map to gate failures, and how to wire together the various `gh` CLI calls the commands depend on. This friction is the primary reason TAG's CI capabilities remain underutilised on teams that otherwise already run TAG locally.

The `tag ci install-action` command eliminates that adoption barrier by generating production-ready GitHub Actions YAML files and writing them directly to `.github/workflows/`. Each generated file is a complete, runnable workflow targeting a specific TAG use case: an eval quality gate that fails the build when agent metrics drop below threshold, a security scan that uploads SARIF findings to GitHub Code Scanning, an issue-solve loop that triggers on label assignment and posts a patch as a PR, and an automated PR review that posts inline comments. All four templates are designed to work out of the box after a single secrets configuration step in the GitHub repository settings.

The generator is aware of TAG's runtime requirements: it injects the correct `pip install tag-agent` invocation, sets `CI=true` to suppress interactive prompts, exposes only the secrets each workflow actually needs, pins action versions (no floating `@latest` references), and validates the generated YAML against the official GitHub Actions JSON Schema before writing to disk. Engineers can customise the output through CLI flags — choosing the runner image, pinning a specific TAG version, selecting which eval suite to gate on, or adding extra environment variables — then commit the file without further modification.

From a broader ecosystem perspective, `tag ci install-action` is TAG's answer to a trend that Braintrust, LangSmith, and Inspect AI have all adopted: first-class CI scaffolding that makes agent evaluation and monitoring a default part of the software development lifecycle rather than an afterthought. The eval gate workflow in particular brings TAG's `tag eval run` (PRD-027) into the pull-request feedback loop, where regressions in agent quality are blocked before they reach the main branch — closing the loop between agent development and quality assurance that currently requires significant manual integration effort.

This command is intentionally low-complexity: it renders Jinja2 templates that live in `src/tag/templates/` into the local repository, with no network calls, no database writes, and no agent execution. The surface area is small enough that the entire feature can ship in one to two engineer-days, yet the impact on time-to-CI-adoption is measured in hours saved per team per onboarding event.

---

## 2. Problem Statement

### 2.1 Authoring GitHub Actions YAML for AI agents is bespoke and error-prone

Setting up TAG in CI currently requires a developer to write a GitHub Actions workflow from memory or by copying the incomplete example in the README. The workflow must correctly handle: installing `tag-agent` in a headless Python environment, suppressing TUI output (`--skip-tui-build`, `CI=true`), passing the right API keys as secrets, calling the correct `tag` subcommand for the desired use case, interpreting TAG's multi-value exit codes (0 = pass, 2 = threshold failure, 3 = regression), and conditioning the PR comment step on the review command's exit code. Missing any of these details produces a silently broken workflow that either always passes, always fails, or fails to post output. The iteration cycle — commit, push, wait for Actions, read error, edit — takes 5–15 minutes per attempt on a cold runner.

### 2.2 TAG's eval gate and security scan have CI-specific invocation patterns that are not obvious from `--help`

`tag eval run` must be called with `--yes` in CI (otherwise it prompts for cost confirmation). `tag ci security-scan` must be called with `--sarif-output results.sarif` and followed by the `github/codeql-action/upload-sarif` step to surface findings in the Security tab. The `issue-solve` workflow requires `GITHUB_TOKEN` with `issues: write` and `pull-requests: write` permissions and must parse the issue number from the `github.event.issue.number` context variable. None of these details are surfaced in `tag --help`; they are scattered across the documentation, the source of PRD-020, and the GitHub Actions documentation. Engineers who are not already expert in both TAG and Actions regularly get stuck and abandon the integration.

### 2.3 There is no canonical, version-controlled source of truth for TAG CI patterns

The single workflow file that currently ships in `src/tag/config/workflows/tag-review.yml` covers only the PR review use case, is not accessible via any CLI command, and has not been updated to reflect recent changes to `tag review-pr` (it still references the old `OPENROUTER_API_KEY` secret name). There is no equivalent file for eval gating, security scanning, or issue solving. When TAG releases a new version that changes a CLI flag or exit code, every team's hand-authored workflow breaks silently. A generator-based approach solves this: teams re-run `tag ci install-action --type eval` to regenerate from the latest template, and diffs between old and new versions are immediately visible in their git history.

---

## 3. Goals

| ID | Goal |
|----|------|
| G1 | `tag ci install-action --type <type>` writes a fully valid, immediately runnable GitHub Actions YAML file to `.github/workflows/tag-<type>.yml` in the current repository. |
| G2 | Four action types are supported at launch: `eval`, `security-scan`, `issue-solve`, and `pr-review`. |
| G3 | Generated YAML is validated against the GitHub Actions JSON Schema at `https://json.schemastore.org/github-workflow.json` before being written to disk; malformed output is rejected with a descriptive error. |
| G4 | `tag ci install-action --list` prints a table of available action types with name, description, trigger event, required secrets, and estimated runtime. |
| G5 | All generated workflows are parameterisable through CLI flags (TAG version pin, runner image, eval suite path, profile name, extra environment variables) with sensible defaults that work without any flags. |
| G6 | The command detects if `.github/workflows/tag-<type>.yml` already exists and prompts for confirmation before overwriting; `--force` skips the prompt. |
| G7 | Generated workflows pin all referenced GitHub Actions to a specific SHA-tagged version (not `@latest` or `@v1`), following security best practices. |
| G8 | A post-write summary prints the exact secrets the user needs to add in GitHub → Settings → Secrets and lists the permissions the workflow requires. |
| G9 | `--dry-run` prints the YAML to stdout without writing any file. |
| G10 | The entire feature is implemented with no network calls at generation time; all templates are bundled in the package at `src/tag/templates/`. |

---

## 4. Non-Goals

| ID | Non-Goal |
|----|----------|
| NG1 | GitLab CI YAML generation. This PRD targets GitHub Actions only. GitLab support is a separate future PRD. |
| NG2 | Automatically adding secrets to the GitHub repository via the GitHub API. The command lists what secrets are needed; the user adds them manually. |
| NG3 | Reusable workflow composition (the `workflow_call` trigger). Generated workflows are standalone; composability is a future enhancement. |
| NG4 | Validating generated YAML by running it (requires a real GitHub Actions environment). Schema validation is the extent of pre-write checking. |
| NG5 | Self-hosted runner configuration or runner group management. |
| NG6 | Generating workflows for CI systems other than GitHub Actions (CircleCI, Jenkins, Buildkite). |
| NG7 | Automatic PR creation to add the workflow to the repository. The file is written locally; the user commits and pushes. |
| NG8 | Ongoing synchronisation or auto-update of previously installed workflows. Users re-run the command to regenerate. |

---

## 5. Success Metrics

| Metric | Baseline (now) | Target (30 days post-ship) | Measurement Method |
|--------|---------------|---------------------------|--------------------|
| Time from `pip install tag-agent` to working Actions CI | ~90 min (manual) | < 10 min | User study / support ticket time logs |
| % of new TAG-adopting repos that add at least one Actions workflow | ~5% | > 30% | GitHub repo scan / telemetry in `tag doctor` |
| GitHub Actions schema validation pass rate on generated YAML | N/A | 100% | CI test suite assertion |
| Support tickets citing "CI setup confusion" | Baseline (track) | -50% | GitHub Issues labelled `ci-setup` |
| `tag ci install-action` command invocations per week | 0 | >50 | Opt-in telemetry counter |

---

## 6. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|-----------|----------|
| U1 | Backend engineer onboarding TAG | run `tag ci install-action --type pr-review` in my repo | I get a working PR review workflow without reading the Actions documentation |
| U2 | ML engineer who uses `tag eval run` locally | run `tag ci install-action --type eval --suite evals/coding.yaml` | every PR is automatically gated on agent quality and I get the same signal in CI as I get locally |
| U3 | Security engineer | run `tag ci install-action --type security-scan` | security findings from TAG are surfaced in the GitHub Security tab automatically on each push to main |
| U4 | Platform engineer setting up a monorepo | run `tag ci install-action --type issue-solve --profile coder --runner ubuntu-latest` | labeled GitHub issues trigger a TAG coding agent that opens a fix PR without manual intervention |
| U5 | Developer evaluating TAG for team adoption | run `tag ci install-action --list` | I can see all available workflow types and decide which ones are relevant for my team before installing any |
| U6 | Engineer updating TAG version | run `tag ci install-action --type eval --tag-version 0.4.0 --force` | the workflow is regenerated with the new TAG version pinned without manually editing YAML |
| U7 | Security-conscious engineer | run `tag ci install-action --type pr-review --dry-run` | I can inspect the generated YAML before writing to disk to verify it meets my organisation's security policies |
| U8 | Developer who already has a workflow | run `tag ci install-action --type pr-review` and be warned the file exists | I am not surprised by an overwrite; I can diff old and new before committing |

---

## 7. Proposed CLI Surface

All subcommands live under `tag ci`. `install-action` is a new subcommand alongside the existing `diagnose`, `commit-lint`, and `status`.

### 7.1 `tag ci install-action`

Install a GitHub Actions workflow template into `.github/workflows/`.

```
tag ci install-action \
  --type <eval|security-scan|issue-solve|pr-review> \
  [--suite evals/coding.yaml] \
  [--profile <profile-name>] \
  [--runner ubuntu-latest] \
  [--python-version 3.12] \
  [--tag-version <semver|latest>] \
  [--output-dir <path>] \
  [--extra-env KEY=VALUE ...] \
  [--force] \
  [--dry-run] \
  [--json]
```

**Options:**

- `--type`: Required. One of `eval`, `security-scan`, `issue-solve`, `pr-review`.
- `--suite`: Path to the eval suite YAML (only applicable with `--type eval`). Default: `evals/default.yaml`. Relative to cwd.
- `--profile`: TAG profile name to use in the workflow. Default: `orchestrator` for `issue-solve`; `reviewer` for `pr-review`; `evaluator` for `eval`; `security` for `security-scan`.
- `--runner`: GitHub Actions runner label. Default: `ubuntu-latest`. Must be one of the supported GitHub-hosted runner labels.
- `--python-version`: Python version string passed to `actions/setup-python`. Default: `3.12`.
- `--tag-version`: TAG package version to pin in `pip install tag-agent==<version>`. Default: the currently installed version of `tag-agent` (from `importlib.metadata`). Pass `latest` to omit the pin.
- `--output-dir`: Directory to write the YAML file into. Default: `.github/workflows/` relative to the nearest git root (detected via `git rev-parse --show-toplevel`).
- `--extra-env`: Additional environment variables to inject into the workflow job (can be repeated). Format: `KEY=VALUE`. Values that look like secrets (e.g. `KEY=secret_*`) are emitted as `${{ secrets.KEY }}` references instead of literal values.
- `--force`: Overwrite existing file without prompting.
- `--dry-run`: Print generated YAML to stdout; do not write to disk. Implies schema validation still runs.
- `--json`: Output a machine-readable JSON summary (file path, action type, secrets required, validation result).

**Exit codes:**

- `0` — file written (or printed with `--dry-run`) and schema validation passed.
- `1` — internal error (template render failure, schema validation error, git root not found).
- `2` — target file already exists and `--force` was not passed (user prompted; said no, or non-interactive).

**Example: install eval gate**

```
$ tag ci install-action --type eval --suite evals/coding.yaml --profile coder

Rendering template: eval
TAG version: 0.3.0
Suite: evals/coding.yaml
Profile: coder
Runner: ubuntu-latest
Python: 3.12

Validating against GitHub Actions JSON Schema... OK

Writing: .github/workflows/tag-eval.yml

Done. Next steps:
  1. Add the following secrets in GitHub → Settings → Secrets and variables → Actions:
       ANTHROPIC_API_KEY   (or OPENROUTER_API_KEY — set whichever TAG uses locally)
  2. Commit and push: git add .github/workflows/tag-eval.yml && git commit -m "ci: add TAG eval gate"
  3. Open a PR — the workflow triggers on pull_request and fails if eval score drops below threshold.

Required permissions (already set in the generated YAML):
  pull-requests: write   (to post eval summary as PR comment)
  contents: read
```

**Example: list available types**

```
$ tag ci install-action --list

┌─────────────────┬──────────────────────────────────────────────┬─────────────────────────────┬──────────────────────────────────────────┬──────────────┐
│ Type            │ Description                                  │ Trigger                     │ Required Secrets                         │ Est. Runtime │
├─────────────────┼──────────────────────────────────────────────┼─────────────────────────────┼──────────────────────────────────────────┼──────────────┤
│ eval            │ Eval quality gate — fails PR if score drops  │ pull_request                │ ANTHROPIC_API_KEY (or OPENROUTER_API_KEY)│ 2–5 min      │
│ security-scan   │ SAST scan, uploads SARIF to Code Scanning    │ push (main), pull_request   │ ANTHROPIC_API_KEY                        │ 1–3 min      │
│ issue-solve     │ Agent solves issue on label, opens fix PR    │ issues (labeled)            │ ANTHROPIC_API_KEY, GH_TOKEN (auto)       │ 3–10 min     │
│ pr-review       │ Inline AI code review on every PR            │ pull_request                │ ANTHROPIC_API_KEY (or OPENROUTER_API_KEY)│ 1–4 min      │
└─────────────────┴──────────────────────────────────────────────┴─────────────────────────────┴──────────────────────────────────────────┴──────────────┘

Run `tag ci install-action --type <type>` to install a workflow.
```

**Example: dry run**

```
$ tag ci install-action --type pr-review --dry-run

# --- DRY RUN: output below would be written to .github/workflows/tag-pr-review.yml ---

name: TAG PR Review
on:
  pull_request:
    types: [opened, synchronize, reopened]
...
```

### 7.2 `tag ci install-action --list` (alias)

```
tag ci install-action --list [--json]
```

With `--json`, outputs a JSON array of objects with keys: `type`, `description`, `trigger`, `required_secrets`, `output_file`, `estimated_runtime_seconds_min`, `estimated_runtime_seconds_max`.

---

## 8. Functional Requirements

| ID | Requirement |
|----|-------------|
| FR-01 | `cmd_ci` in `controller.py` must dispatch to a new `install-action` branch when `ci_subcommand == "install-action"`. The dispatch must call `workflow_scaffold.install_action(args, cfg)` from `src/tag/workflow_scaffold.py`. |
| FR-02 | `--type` is required when not using `--list`. Passing an unsupported type must print the list of valid types and exit 1. |
| FR-03 | Template files are stored at `src/tag/templates/github_actions/<type>.yml.j2` (Jinja2 format). All four template files (`eval.yml.j2`, `security-scan.yml.j2`, `issue-solve.yml.j2`, `pr-review.yml.j2`) must be present and parseable at package install time. |
| FR-04 | Template rendering uses `jinja2.Environment(undefined=jinja2.StrictUndefined)` so that any undefined variable in the template raises `UndefinedError` rather than silently emitting an empty string. |
| FR-05 | The `--tag-version` default is resolved by calling `importlib.metadata.version("tag-agent")` at render time. If the package is not found (editable install), the version defaults to `"latest"` and a warning is printed. |
| FR-06 | After rendering, the generated YAML string is validated against the GitHub Actions JSON Schema fetched from the bundled copy at `src/tag/templates/github_actions/github-workflow-schema.json`. Validation uses `jsonschema.validate`. If validation fails, the error message must include the JSON Schema path and a human-readable description. The YAML must not be written to disk on validation failure. |
| FR-07 | The GitHub Actions JSON Schema file at `src/tag/templates/github_actions/github-workflow-schema.json` must be bundled in the package (not fetched at runtime). It must be refreshed during the TAG release process, not at user install time. |
| FR-08 | If the target output file already exists, the command must print a warning and prompt `Overwrite? [y/N]` on stderr before proceeding. In non-interactive mode (no TTY on stdin), it must refuse to overwrite and exit 2. `--force` suppresses the prompt and always overwrites. |
| FR-09 | The output directory must be created with `Path.mkdir(parents=True, exist_ok=True)` if it does not exist. |
| FR-10 | All generated workflows must include a comment block at the top of the file identifying the generator: `# Generated by tag ci install-action v<TAG_VERSION> on <ISO_DATE>. Re-run to update.` |
| FR-11 | Generated workflows must pin all external action references to a specific commit SHA (not a mutable tag like `@v4`). The SHA pins are stored in `src/tag/templates/github_actions/action-pins.json` and are used during rendering. |
| FR-12 | The `--list` flag must work without `--type` and must not require a git repository to be present. |
| FR-13 | `--dry-run` must print the generated YAML to stdout (not stderr), pass schema validation, and exit 0 on success. No files are created. The phrase `DRY RUN` must appear in the stderr output. |
| FR-14 | The `--json` flag produces a JSON object on stdout with keys: `type`, `output_file` (absolute path), `rendered_yaml` (only in `--dry-run`), `schema_valid` (bool), `secrets_required` (list of strings), `permissions` (dict), `warnings` (list of strings). |
| FR-15 | Post-write output must list each required secret with a one-line description of what it is used for, and list each workflow-level permission set on the `permissions:` key in the generated YAML. |
| FR-16 | The `eval` workflow template must invoke `tag eval run` with `--yes` (to suppress cost confirmation) and must use the exit code to set the job status: exit 0 passes, exit 2 or 3 fails, exit 1 also fails with an error annotation. |
| FR-17 | The `security-scan` workflow template must call `tag ci security-scan --sarif-output results.sarif` and then use `github/codeql-action/upload-sarif` to submit findings to GitHub Code Scanning. |
| FR-18 | The `issue-solve` workflow template must trigger on `issues: [labeled]` and include a conditional step that checks `github.event.label.name == 'tag-solve'` to prevent triggering on all labels. The agent label name must be parameterisable via `--label <name>` (default: `tag-solve`). |
| FR-19 | The `pr-review` workflow template must include a `concurrency` key to cancel in-progress runs on the same PR when a new commit is pushed, preventing duplicate review comments. |
| FR-20 | `tag ci install-action --list` output is sorted alphabetically by type name. |

---

## 9. Non-Functional Requirements

| ID | Requirement |
|----|-------------|
| NFR-01 | **Zero network calls at generation time.** The command must work in air-gapped environments. All templates and the JSON Schema are bundled in the package. |
| NFR-02 | **Sub-second execution.** Template rendering plus schema validation must complete in under 1 second on any modern machine. No heavy imports at module load time (`jinja2` and `jsonschema` are imported lazily inside `install_action()`). |
| NFR-03 | **Idempotency.** Running `install-action --type X --force` twice in a row on a repo with no intervening changes produces an identical output file both times (deterministic rendering). |
| NFR-04 | **No side effects on `--dry-run`.** Absolutely no files are created, modified, or deleted. No directory creation. |
| NFR-05 | **Jinja2 as the only new dependency.** `jinja2` is already a transitive dependency of the `tag-agent` package. `jsonschema` is also already required. No new package dependencies are introduced. |
| NFR-06 | **Template maintainability.** Each template must be a standalone file that can be reviewed and modified without understanding the rendering logic. Templates must use only basic Jinja2 features (variable substitution, `if` blocks, `for` loops). No Jinja2 macros or `include` directives that obscure the final YAML structure. |
| NFR-07 | **Action SHA pins must be auditable.** `action-pins.json` is a plain JSON mapping of `owner/action@tag -> sha` that can be reviewed and updated in a single commit. The update process must be documented in `CONTRIBUTING.md`. |
| NFR-08 | **Windows compatibility.** File paths in the output directory resolution must use `pathlib.Path` throughout, not string concatenation. The command must work on Windows (for developers who run TAG on Windows). |
| NFR-09 | **No writes to `tag.sqlite3`.** This feature has no persistent state requirement. No database reads or writes are performed. |
| NFR-10 | **Graceful degradation if git root not found.** If `git rev-parse --show-toplevel` fails (not in a git repo), the command falls back to writing to `.github/workflows/` relative to the current working directory and prints a warning that git root detection failed. |

---

## 10. Technical Design

### 10.1 New files

| Path | Purpose |
|------|---------|
| `src/tag/workflow_scaffold.py` | Core module: `install_action()`, `list_actions()`, `render_template()`, `validate_yaml()`, `detect_git_root()`, `resolve_tag_version()`. All logic; no Click/argparse dependencies. |
| `src/tag/templates/github_actions/eval.yml.j2` | Jinja2 template for the eval gate workflow |
| `src/tag/templates/github_actions/security-scan.yml.j2` | Jinja2 template for the security scan + SARIF upload workflow |
| `src/tag/templates/github_actions/issue-solve.yml.j2` | Jinja2 template for the issue-solve agent workflow |
| `src/tag/templates/github_actions/pr-review.yml.j2` | Jinja2 template for the PR review workflow |
| `src/tag/templates/github_actions/github-workflow-schema.json` | Bundled copy of the GitHub Actions JSON Schema (updated at release time) |
| `src/tag/templates/github_actions/action-pins.json` | SHA pins for all external GitHub Actions used in templates |
| `tests/test_workflow_scaffold.py` | Unit and integration tests |

### 10.2 Core dataclasses

```python
# src/tag/workflow_scaffold.py
from __future__ import annotations

import importlib.metadata
import subprocess
from dataclasses import dataclass, field
from pathlib import Path
from typing import Optional


@dataclass
class ActionType:
    """Metadata for a single installable workflow type."""
    key: str                          # CLI slug: "eval", "security-scan", etc.
    display_name: str                 # Human-readable: "Eval Quality Gate"
    description: str                  # One-line description for --list output
    template_file: str                # Relative to templates/github_actions/
    output_file: str                  # Written to .github/workflows/<output_file>
    trigger: str                      # Human-readable trigger description
    required_secrets: list[str]       # List of secret names the workflow needs
    optional_secrets: list[str] = field(default_factory=list)
    permissions: dict[str, str] = field(default_factory=dict)
    est_runtime_min: int = 1          # Estimated minimum runtime in minutes
    est_runtime_max: int = 5          # Estimated maximum runtime in minutes


@dataclass
class RenderContext:
    """All variables available to Jinja2 templates."""
    action_type: str
    tag_version: str                  # e.g. "0.3.0" or "latest"
    runner: str                       # e.g. "ubuntu-latest"
    python_version: str               # e.g. "3.12"
    profile: str                      # TAG profile name
    suite_path: Optional[str]         # Path to eval suite (eval type only)
    label_name: str                   # Issue label to trigger on (issue-solve only)
    extra_env: dict[str, str]         # Additional env vars
    generator_version: str            # TAG version used to generate
    generator_date: str               # ISO 8601 date string
    action_pins: dict[str, str]       # owner/action@tag -> sha


@dataclass
class InstallResult:
    """Result of a single install-action invocation."""
    action_type: str
    output_file: Path
    rendered_yaml: str
    schema_valid: bool
    schema_errors: list[str]
    secrets_required: list[str]
    permissions: dict[str, str]
    warnings: list[str]
    dry_run: bool
    written: bool


KNOWN_ACTIONS: dict[str, ActionType] = {
    "eval": ActionType(
        key="eval",
        display_name="Eval Quality Gate",
        description="Fails the PR if TAG eval scores drop below threshold",
        template_file="eval.yml.j2",
        output_file="tag-eval.yml",
        trigger="pull_request",
        required_secrets=["ANTHROPIC_API_KEY"],
        optional_secrets=["OPENROUTER_API_KEY"],
        permissions={"pull-requests": "write", "contents": "read"},
        est_runtime_min=2,
        est_runtime_max=5,
    ),
    "security-scan": ActionType(
        key="security-scan",
        display_name="Security Scan (SARIF)",
        description="SAST scan with SARIF upload to GitHub Code Scanning",
        template_file="security-scan.yml.j2",
        output_file="tag-security-scan.yml",
        trigger="push (main), pull_request",
        required_secrets=["ANTHROPIC_API_KEY"],
        permissions={
            "security-events": "write",
            "contents": "read",
            "actions": "read",
        },
        est_runtime_min=1,
        est_runtime_max=3,
    ),
    "issue-solve": ActionType(
        key="issue-solve",
        display_name="Issue Solve Agent",
        description="TAG agent solves labeled issues and opens a fix PR",
        template_file="issue-solve.yml.j2",
        output_file="tag-issue-solve.yml",
        trigger="issues (labeled)",
        required_secrets=["ANTHROPIC_API_KEY"],
        permissions={
            "issues": "write",
            "pull-requests": "write",
            "contents": "write",
        },
        est_runtime_min=3,
        est_runtime_max=10,
    ),
    "pr-review": ActionType(
        key="pr-review",
        display_name="PR Review",
        description="Inline AI code review posted as PR comments",
        template_file="pr-review.yml.j2",
        output_file="tag-pr-review.yml",
        trigger="pull_request",
        required_secrets=["ANTHROPIC_API_KEY"],
        optional_secrets=["OPENROUTER_API_KEY"],
        permissions={"pull-requests": "write", "contents": "read"},
        est_runtime_min=1,
        est_runtime_max=4,
    ),
}
```

### 10.3 Core algorithm: `install_action()`

```python
def install_action(args: argparse.Namespace, cfg: dict) -> int:
    """Entry point called from cmd_ci in controller.py."""
    import jinja2
    import jsonschema
    import yaml as pyyaml
    from datetime import date

    action_type_key = getattr(args, "action_type", None)
    if not action_type_key:
        list_actions()
        return 0

    if action_type_key not in KNOWN_ACTIONS:
        print_error(
            f"Unknown action type: {action_type_key!r}. "
            f"Valid types: {', '.join(KNOWN_ACTIONS)}"
        )
        return 1

    action = KNOWN_ACTIONS[action_type_key]
    tag_version = resolve_tag_version(getattr(args, "tag_version", None))
    git_root = detect_git_root()
    output_dir = Path(getattr(args, "output_dir", None) or (
        git_root / ".github" / "workflows" if git_root else Path.cwd() / ".github" / "workflows"
    ))

    ctx = RenderContext(
        action_type=action_type_key,
        tag_version=tag_version,
        runner=getattr(args, "runner", "ubuntu-latest"),
        python_version=getattr(args, "python_version", "3.12"),
        profile=getattr(args, "profile", None) or _default_profile(action_type_key),
        suite_path=getattr(args, "suite", "evals/default.yaml"),
        label_name=getattr(args, "label", "tag-solve"),
        extra_env=_parse_extra_env(getattr(args, "extra_env", []) or []),
        generator_version=tag_version,
        generator_date=date.today().isoformat(),
        action_pins=_load_action_pins(),
    )

    rendered = render_template(action.template_file, ctx)
    schema_valid, schema_errors = validate_yaml(rendered)

    if not schema_valid:
        print_error("Generated YAML failed schema validation:")
        for err in schema_errors:
            print_error(f"  {err}")
        return 1

    dry_run = getattr(args, "dry_run", False)
    if dry_run:
        print(f"# --- DRY RUN: output below would be written to "
              f"{output_dir / action.output_file} ---\n")
        print(rendered)
        return 0

    output_file = output_dir / action.output_file
    if output_file.exists() and not getattr(args, "force", False):
        confirmed = _prompt_overwrite(output_file)
        if not confirmed:
            print("Aborted. Use --force to overwrite without prompting.")
            return 2

    output_dir.mkdir(parents=True, exist_ok=True)
    output_file.write_text(rendered, encoding="utf-8")

    _print_post_write_summary(action, output_file)
    return 0
```

### 10.4 Template rendering

Templates are loaded from the package using `importlib.resources`:

```python
def _templates_dir() -> Path:
    """Return the path to src/tag/templates/github_actions/."""
    import importlib.resources as ir
    # Python 3.9+: files() returns a Traversable
    pkg = ir.files("tag") / "templates" / "github_actions"
    return Path(str(pkg))


def render_template(template_file: str, ctx: RenderContext) -> str:
    import jinja2
    templates_dir = _templates_dir()
    env = jinja2.Environment(
        loader=jinja2.FileSystemLoader(str(templates_dir)),
        undefined=jinja2.StrictUndefined,
        keep_trailing_newline=True,
        autoescape=False,   # YAML, not HTML
    )
    template = env.get_template(template_file)
    return template.render(**vars(ctx))
```

### 10.5 Schema validation

```python
def validate_yaml(rendered: str) -> tuple[bool, list[str]]:
    """Validate rendered YAML against the bundled GitHub Actions JSON Schema."""
    import json
    import jsonschema
    import yaml as pyyaml

    schema_path = _templates_dir() / "github-workflow-schema.json"
    with schema_path.open(encoding="utf-8") as f:
        schema = json.load(f)

    try:
        instance = pyyaml.safe_load(rendered)
    except pyyaml.YAMLError as exc:
        return False, [f"YAML parse error: {exc}"]

    validator = jsonschema.Draft7Validator(schema)
    errors = sorted(validator.iter_errors(instance), key=lambda e: list(e.path))
    if errors:
        messages = [
            f"  At {' > '.join(str(p) for p in err.path) or 'root'}: {err.message}"
            for err in errors[:10]  # cap at 10 to avoid overwhelming output
        ]
        return False, messages
    return True, []
```

### 10.6 Sample template: `eval.yml.j2`

```jinja
# Generated by tag ci install-action v{{ generator_version }} on {{ generator_date }}.
# Re-run `tag ci install-action --type eval --force` to update.
#
# Required secrets:
#   ANTHROPIC_API_KEY  — your Anthropic API key (or set OPENROUTER_API_KEY instead)
#
# Required repository permissions: pull-requests:write, contents:read

name: TAG Eval Gate

on:
  pull_request:
    types: [opened, synchronize, reopened]

permissions:
  pull-requests: write
  contents: read

concurrency:
  group: tag-eval-${{ '{{' }} github.ref {{ '}}' }}
  cancel-in-progress: true

jobs:
  tag-eval:
    runs-on: {{ runner }}

    steps:
      - name: Checkout
        uses: actions/checkout@{{ action_pins['actions/checkout@v4'] }}

      - name: Set up Python {{ python_version }}
        uses: actions/setup-python@{{ action_pins['actions/setup-python@v5'] }}
        with:
          python-version: '{{ python_version }}'
          cache: pip

      - name: Install TAG{% if tag_version != 'latest' %} {{ tag_version }}{% endif %}

        run: pip install tag-agent{% if tag_version != 'latest' %}=={{ tag_version }}{% endif %}

      - name: TAG setup (CI mode)
        run: tag setup --skip-tui-build
        env:
          ANTHROPIC_API_KEY: ${{ '{{' }} secrets.ANTHROPIC_API_KEY {{ '}}' }}
          CI: "true"

      - name: Run eval suite
        id: eval
        run: |
          tag eval run \
            --suite {{ suite_path }} \
            --profile {{ profile }} \
            --yes \
            --json > eval-results.json
        env:
          ANTHROPIC_API_KEY: ${{ '{{' }} secrets.ANTHROPIC_API_KEY {{ '}}' }}
          CI: "true"
        continue-on-error: true
{% if extra_env %}
          # Extra environment variables
{% for key, value in extra_env.items() %}
          {{ key }}: {{ value }}
{% endfor %}
{% endif %}

      - name: Post eval summary as PR comment
        if: always()
        uses: actions/github-script@{{ action_pins['actions/github-script@v7'] }}
        with:
          script: |
            const fs = require('fs');
            let body = '### TAG Eval Results\n\n';
            try {
              const results = JSON.parse(fs.readFileSync('eval-results.json', 'utf8'));
              body += '| Case | Score | Threshold | Pass |\n|---|---|---|---|\n';
              for (const c of (results.cases || [])) {
                body += `| ${c.name} | ${c.score?.toFixed(3) ?? 'N/A'} | ${c.threshold} | ${c.passed ? '✓' : '✗'} |\n`;
              }
              body += `\n**Overall pass rate:** ${results.pass_rate ?? 'N/A'}`;
            } catch (e) {
              body += '_Eval results not available._';
            }
            github.rest.issues.createComment({
              issue_number: context.issue.number,
              owner: context.repo.owner,
              repo: context.repo.repo,
              body
            });

      - name: Fail if eval gate failed
        if: steps.eval.outcome == 'failure'
        run: exit 1
```

### 10.7 Sample template: `issue-solve.yml.j2`

```jinja
# Generated by tag ci install-action v{{ generator_version }} on {{ generator_date }}.
# Trigger: apply the label '{{ label_name }}' to any issue to start a TAG agent.

name: TAG Issue Solve

on:
  issues:
    types: [labeled]

permissions:
  issues: write
  pull-requests: write
  contents: write

jobs:
  tag-issue-solve:
    if: github.event.label.name == '{{ label_name }}'
    runs-on: {{ runner }}
    timeout-minutes: 15

    steps:
      - name: Checkout
        uses: actions/checkout@{{ action_pins['actions/checkout@v4'] }}

      - name: Set up Python {{ python_version }}
        uses: actions/setup-python@{{ action_pins['actions/setup-python@v5'] }}
        with:
          python-version: '{{ python_version }}'

      - name: Install TAG
        run: pip install tag-agent{% if tag_version != 'latest' %}=={{ tag_version }}{% endif %}

      - name: TAG setup (CI mode)
        run: tag setup --skip-tui-build
        env:
          ANTHROPIC_API_KEY: ${{ '{{' }} secrets.ANTHROPIC_API_KEY {{ '}}' }}
          CI: "true"

      - name: Solve issue
        run: |
          tag loop \
            --task "Solve GitHub issue #${{ '{{' }} github.event.issue.number {{ '}}' }}: ${{ '{{' }} github.event.issue.title {{ '}}' }}" \
            --profile {{ profile }} \
            --budget-usd 2.00 \
            --max-steps 30 \
            --create-pr \
            --pr-title "fix: resolve issue #${{ '{{' }} github.event.issue.number {{ '}}' }}"
        env:
          ANTHROPIC_API_KEY: ${{ '{{' }} secrets.ANTHROPIC_API_KEY {{ '}}' }}
          GH_TOKEN: ${{ '{{' }} secrets.GITHUB_TOKEN {{ '}}' }}
          CI: "true"
```

### 10.8 Action SHA pins format (`action-pins.json`)

```json
{
  "actions/checkout@v4": "11bd71901bbe5b1630ceea73d27597364c9af683",
  "actions/setup-python@v5": "0b93645e9fea7318ecaed2b359559ac225c90a2",
  "actions/upload-artifact@v4": "6f51ac03b9356f520e9adb1b1b7802705f340c2b",
  "github/codeql-action/upload-sarif@v3": "05963f47d870e2cb19a537396c1f668a348c7d8f",
  "actions/github-script@v7": "60a0d83039c74a4aee543508d2ffcb1c3799cdea"
}
```

Pins are resolved by running `gh api /repos/<owner>/<repo>/git/ref/tags/<tag>` and following to the commit SHA. The update script is at `scripts/update_action_pins.py`.

### 10.9 `pyproject.toml` package data entry

```toml
[tool.setuptools.package-data]
"tag" = [
    "templates/github_actions/*.j2",
    "templates/github_actions/*.json",
]
```

### 10.10 Controller integration

In `controller.py`, extend the existing `cmd_ci` function:

```python
if sub == "install-action":
    from tag.workflow_scaffold import install_action
    return install_action(args, cfg)
```

And in the argparse setup, add under the `ci` subparser:

```python
# Inside the section that builds ci subcommands
install_action_parser = ci_sub.add_parser(
    "install-action",
    help="Scaffold a GitHub Actions workflow for TAG CI tasks",
)
install_action_parser.add_argument(
    "--type",
    dest="action_type",
    choices=["eval", "security-scan", "issue-solve", "pr-review"],
    help="Workflow type to generate",
)
install_action_parser.add_argument("--list", action="store_true", dest="list_types")
install_action_parser.add_argument("--suite", default="evals/default.yaml")
install_action_parser.add_argument("--profile", default=None)
install_action_parser.add_argument("--runner", default="ubuntu-latest")
install_action_parser.add_argument("--python-version", default="3.12")
install_action_parser.add_argument("--tag-version", default=None)
install_action_parser.add_argument("--output-dir", default=None)
install_action_parser.add_argument("--label", default="tag-solve")
install_action_parser.add_argument(
    "--extra-env", nargs="*", dest="extra_env", metavar="KEY=VALUE"
)
install_action_parser.add_argument("--force", action="store_true")
install_action_parser.add_argument("--dry-run", action="store_true")
install_action_parser.add_argument("--json", action="store_true", dest="json_output")
```

---

## 11. Security Considerations

1. **No secrets in generated YAML literals.** All sensitive values (API keys, tokens) are always rendered as `${{ secrets.SECRET_NAME }}` references, never as literal values. The `--extra-env` parser inspects each value against a heuristic (contains `secret`, starts with `sk-`, longer than 20 characters of apparent entropy) and emits them as secret references with a warning if they appear sensitive.

2. **Action SHA pinning prevents supply-chain attacks.** Using commit SHAs instead of mutable tags (`@v4`) ensures that a compromised upstream action release cannot silently alter the generated workflow's behaviour. This follows the GitHub security hardening guide for Actions.

3. **Least-privilege permissions.** Each template declares only the `permissions` keys actually needed by that workflow. The `pr-review` and `eval` workflows do not request `contents: write`. The `issue-solve` workflow requests `contents: write` only because it needs to push a fix branch; this is documented in the post-write summary.

4. **No execution of generated YAML.** The command writes a file; it does not trigger any workflow run. There is no `gh workflow run` call. The user controls when the file is committed and pushed.

5. **Template injection prevention.** Template variables come from CLI arguments, not from external sources (issue bodies, PR titles, commit messages). The Jinja2 environment uses `autoescape=False` (appropriate for YAML output, not HTML), but all user-supplied strings that flow into templates are constrained by argparse `choices` or are file paths validated as existing paths. Free-form strings (profile names) are passed verbatim into YAML; the schema validator will catch any YAML structure violations caused by adversarial names.

6. **Bundled schema, not fetched at runtime.** The JSON Schema is bundled in the package, not fetched from `json.schemastore.org` at generation time. This prevents a SSRF or DNS-based attack and ensures the command works in air-gapped environments. The tradeoff is that the schema may be slightly stale between TAG releases; the release checklist includes a schema refresh step.

7. **`GITHUB_TOKEN` scope.** The `issue-solve` template uses `secrets.GITHUB_TOKEN` (the automatically provided token), not a personal access token, for all GitHub API operations. `GH_TOKEN` is set to `${{ secrets.GITHUB_TOKEN }}` in the generated YAML. No PAT is required.

8. **Budget guard in issue-solve.** The `tag loop` invocation in the `issue-solve` template includes `--budget-usd 2.00` and `--max-steps 30` to prevent runaway agentic loops from consuming unbounded API credits on a mislabeled issue.

---

## 12. Testing Strategy

### 12.1 Unit tests (`tests/test_workflow_scaffold.py`)

| Test | Assertion |
|------|-----------|
| `test_render_eval_template` | Renders `eval.yml.j2` with a `RenderContext`; asserts the output contains the profile name, suite path, and generator comment. |
| `test_render_security_scan_template` | Asserts `upload-sarif` action appears in the rendered YAML. |
| `test_render_issue_solve_template` | Asserts label condition `github.event.label.name == 'tag-solve'` appears verbatim. |
| `test_render_pr_review_template` | Asserts `concurrency` key appears in rendered YAML. |
| `test_schema_validation_passes` | Renders all four templates with default context; asserts `validate_yaml()` returns `(True, [])` for each. |
| `test_schema_validation_rejects_invalid` | Constructs deliberately invalid YAML (missing `on:` key); asserts `validate_yaml()` returns `(False, [...])`. |
| `test_unknown_action_type_exits_1` | Calls `install_action` with `action_type="nonexistent"`; asserts return code is 1. |
| `test_dry_run_no_file_written` | Calls with `dry_run=True`; asserts no file is written in a temp directory. |
| `test_force_overwrites_existing` | Creates a stub file at the output path; calls with `force=True`; asserts file is overwritten with new content. |
| `test_no_force_aborts_on_existing_noninteractive` | Creates a stub file; calls without `force` in non-interactive mode (mocked `sys.stdin.isatty() = False`); asserts return code is 2 and file is unchanged. |
| `test_resolve_tag_version_from_importlib` | Mocks `importlib.metadata.version` to return `"0.3.0"`; asserts `resolve_tag_version(None)` returns `"0.3.0"`. |
| `test_resolve_tag_version_explicit` | Calls `resolve_tag_version("0.2.5")`; asserts it returns `"0.2.5"`. |
| `test_action_pins_loaded` | Loads `action-pins.json`; asserts all keys referenced in templates have entries. |
| `test_extra_env_secret_heuristic` | Passes `extra_env=["MY_KEY=sk-abc123defgh"]`; asserts generated YAML references `${{ secrets.MY_KEY }}` not the literal value. |
| `test_jinja2_strict_undefined_raises` | Creates a template with `{{ undefined_var }}`; asserts `render_template` raises `jinja2.UndefinedError`. |
| `test_output_dir_created_if_missing` | Calls with `output_dir` pointing to a nonexistent path; asserts the directory is created. |
| `test_list_actions_output` | Calls `list_actions()`; asserts all four known action types appear in the output string. |
| `test_git_root_detection` | Mocks `subprocess.run` to return a known path; asserts `detect_git_root()` returns a `Path` to that directory. |
| `test_git_root_fallback_on_failure` | Mocks `subprocess.run` to exit non-zero; asserts `detect_git_root()` returns `None` and no exception is raised. |

### 12.2 Integration tests

Run with `pytest -m integration` against a real temporary git repository created with `git init`:

- `test_full_install_pr_review`: Runs `tag ci install-action --type pr-review --dry-run` in a temp git repo; asserts stdout contains valid YAML and stderr contains `DRY RUN`.
- `test_full_install_writes_file`: Runs `tag ci install-action --type eval` in a temp git repo; asserts `.github/workflows/tag-eval.yml` exists and is valid YAML.
- `test_all_types_schema_valid`: Loops over all four types, renders each with `--dry-run`, and asserts schema validation passes for every one.

### 12.3 Performance test

```python
def test_render_performance():
    import time
    start = time.monotonic()
    for action_type in KNOWN_ACTIONS:
        ctx = _default_render_context(action_type)
        rendered = render_template(KNOWN_ACTIONS[action_type].template_file, ctx)
        validate_yaml(rendered)
    elapsed = time.monotonic() - start
    assert elapsed < 1.0, f"All four types took {elapsed:.2f}s; expected < 1.0s"
```

---

## 13. Acceptance Criteria

| ID | Criterion | How to Verify |
|----|-----------|---------------|
| AC-01 | `tag ci install-action --type eval` writes `.github/workflows/tag-eval.yml` in a git repository | Run command in test repo; assert file exists |
| AC-02 | The generated `tag-eval.yml` passes `actionlint` (if installed) with zero errors | Run `actionlint .github/workflows/tag-eval.yml` |
| AC-03 | The generated `tag-eval.yml` passes internal JSON Schema validation (no `--external` call) | Asserted by `test_schema_validation_passes` |
| AC-04 | `tag ci install-action --type security-scan` generates YAML that includes `upload-sarif` step | Assert substring in rendered output |
| AC-05 | `tag ci install-action --type issue-solve` generates YAML with `on: issues: [labeled]` trigger | Assert YAML key in parsed output |
| AC-06 | `tag ci install-action --type pr-review` generates YAML with `concurrency:` key | Assert YAML key in parsed output |
| AC-07 | `tag ci install-action --list` prints a table containing all four type names | Assert stdout contains `eval`, `security-scan`, `issue-solve`, `pr-review` |
| AC-08 | `--dry-run` prints YAML to stdout and creates zero files | Assert no file at expected path after run |
| AC-09 | Without `--force`, the command refuses to overwrite an existing file in non-interactive mode and exits 2 | Assert exit code 2 and original file content unchanged |
| AC-10 | `--force` overwrites the existing file without prompting | Assert file content is updated |
| AC-11 | `--tag-version 0.2.5` generates YAML containing `pip install tag-agent==0.2.5` | Assert substring |
| AC-12 | `--tag-version latest` generates YAML containing `pip install tag-agent` without a version pin | Assert no `==` in the pip install line |
| AC-13 | All four generated workflows contain the generator comment at the top | Assert first lines of each file match expected pattern |
| AC-14 | All external action references in generated YAML use SHA pins, not mutable tags | Assert no `@v[0-9]` pattern appears in generated YAML (only `@<40-char hex>`) |
| AC-15 | `--extra-env MY_KEY=sk-abc123` renders as a `${{ secrets.MY_KEY }}` reference, not the literal value | Assert `sk-abc123` does not appear in generated YAML |
| AC-16 | `tag ci install-action --type unknown` exits 1 with a message listing valid types | Assert exit code and stderr content |
| AC-17 | `tag ci install-action --type eval --json` outputs valid JSON with `schema_valid: true` | Parse stdout as JSON; assert key |
| AC-18 | Post-write output lists all `required_secrets` for the chosen action type | Assert each secret name appears in stdout |
| AC-19 | The `issue-solve` template includes `--budget-usd` and `--max-steps` guards in the `tag loop` invocation | Assert substrings in rendered YAML |
| AC-20 | `tag ci install-action --type eval --suite evals/custom.yaml` generates YAML referencing `evals/custom.yaml` | Assert suite path in rendered YAML |

---

## 14. Dependencies

| Dependency | Type | Version | Notes |
|------------|------|---------|-------|
| `jinja2` | Python package | `>=3.1` | Already a transitive dependency of `tag-agent`; no new install required |
| `jsonschema` | Python package | `>=4.0` | Already required by `tag-agent`; used for schema validation |
| `pyyaml` | Python package | `>=6.0` | Already required; used to parse rendered YAML before schema validation |
| `importlib.resources` | stdlib | Python 3.9+ | Used to locate template files in installed package |
| `importlib.metadata` | stdlib | Python 3.8+ | Used to resolve installed TAG version for default `--tag-version` |
| PRD-020 | TAG feature | Implemented | `cmd_ci` dispatch point and `tag.ci` module already exist |
| PRD-027 | TAG feature | Proposed | The `eval` workflow template invokes `tag eval run`; PRD-027 must be implemented for the workflow to be functional (the generator itself does not depend on it) |
| PRD-034 | TAG feature | Proposed | The `security-scan` template invokes `tag ci security-scan`; the generator does not depend on it |
| `actionlint` | External tool | Any | Optional; used in AC-02 for extra validation. Not required at runtime. |
| `github-workflow-schema.json` | Bundled file | 2024-01 snapshot | Must be refreshed on each TAG release via `scripts/update_action_pins.py` |

---

## 15. Open Questions

| ID | Question | Owner | Resolution Needed By |
|----|---------|-------|---------------------|
| OQ-01 | Should the `security-scan` template also emit a `copilot-setup-steps.yml` file (per cluster research item 7) for Copilot-compatible environment setup? Or is that a separate `--type copilot-setup`? | @eng-lead | Before implementation |
| OQ-02 | Should `--extra-env` values that match the pattern `${{ secrets.FOO }}` be passed through verbatim, or should the parser strip the wrapping and re-emit them? | CLI team | Before implementation |
| OQ-03 | The JSON Schema bundling strategy means the schema can be stale between TAG releases. Should we add a `tag ci install-action --refresh-schema` command that fetches the latest schema from SchemaStore and saves it locally? | @infra | After ship |
| OQ-04 | For the `issue-solve` workflow, the default `--budget-usd 2.00` is a guess. Should this be a CLI flag (`--budget-usd`)? What is the right default for a cold start issue-solve? | Agent team | Before implementation |
| OQ-05 | Do we want to support a `--branch <name>` flag for the `issue-solve` template so the agent pushes its fix to a named branch instead of a generated name? | CLI team | After ship |
| OQ-06 | Should the generator validate that the `--suite` path exists on disk at generation time, or just embed it as a string in the YAML? (The path must exist at workflow run time in CI, not necessarily at generation time on the developer's machine.) | CLI team | Before implementation |
| OQ-07 | Should `action-pins.json` be auto-updated by a separate GitHub Actions workflow in the TAG repo itself (using Dependabot or a custom bot), rather than a manual `scripts/update_action_pins.py` script? | Platform | After ship |
| OQ-08 | The `eval` template posts a PR comment using `actions/github-script`. Should this be factored into a reusable composite action in a `tag-actions` repository to avoid duplicating this logic across templates? | Architecture | After ship |

---

## 16. Complexity and Timeline

**Total estimated effort: 1–2 engineer-days**

### Phase 1 — Core scaffold (Day 1, morning, ~4 hours)

- Create `src/tag/workflow_scaffold.py` with `ActionType`, `RenderContext`, `InstallResult` dataclasses and all core functions: `install_action()`, `list_actions()`, `render_template()`, `validate_yaml()`, `detect_git_root()`, `resolve_tag_version()`.
- Create `src/tag/templates/github_actions/` directory.
- Bundle `github-workflow-schema.json` (download from SchemaStore).
- Create `action-pins.json` with current SHA pins for all four referenced actions.
- Wire `cmd_ci` dispatch for `install-action` subcommand and add argparse config in `controller.py`.
- Add package data entries to `pyproject.toml`.

### Phase 2 — Templates (Day 1, afternoon, ~3 hours)

- Write `pr-review.yml.j2` (simplest; already have a prototype in `config/workflows/tag-review.yml`).
- Write `eval.yml.j2` (integrates with `tag eval run`; needs correct exit code handling).
- Write `security-scan.yml.j2` (needs SARIF upload step; model from cluster research item 4).
- Write `issue-solve.yml.j2` (needs label conditional and budget guard).
- Manual verification: render all four templates and inspect output.

### Phase 3 — Tests and polish (Day 2, morning, ~3 hours)

- Write unit tests covering all ACs (see Section 12.1).
- Write integration tests against a real temp git repo.
- Add performance test asserting < 1 second for all four renders.
- Run `actionlint` on all four generated files; fix any issues.
- Update `docs/prd/INDEX.md` with PRD-058 entry.
- Update `CHANGELOG.md` with feature entry.

### Phase 4 — Documentation and review (Day 2, afternoon, ~2 hours)

- Update `README.md` CI/CD section with `tag ci install-action` examples.
- Write `scripts/update_action_pins.py` for pin refresh workflow.
- Address any review feedback.
- Merge.

**Risk factors:**

- The GitHub Actions JSON Schema is large (~1 MB) and may have validation false positives for valid YAML patterns that the schema does not fully capture. If schema validation proves too strict, the fallback is to use `actionlint` (called as a subprocess if installed) instead of, or in addition to, `jsonschema`. This decision should be made during Phase 3 based on actual validation results.
- The `issue-solve` template's `tag loop --create-pr` flag assumes PRD-021 implements that flag. If it does not, the template will need to emit a manual `gh pr create` step instead.

