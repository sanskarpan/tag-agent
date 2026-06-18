# PRD-062: GitLab CI/CD Pipeline Auto-Generation (`tag ci gen-pipeline --platform gitlab`)

**Status:** Proposed
**Priority:** P3
**Estimated Effort:** M (1-2 weeks)
**Category:** CI/CD & Agentic Dev Workflows
**Affects:** `ci.py`
**Depends on:** PRD-027 (Eval Framework), PRD-028 (Sandbox Code Execution), PRD-013 (Agent Tracing & Observability), PRD-034 (Secret Scanning), PRD-016 (Webhook Event Triggers)
**Inspired by:** GitLab Duo pipeline generation, GitHub Copilot for Actions

---

## 1. Overview

Modern software teams operate CI/CD pipelines as first-class infrastructure, yet authoring and maintaining these pipelines remains a high-friction, error-prone task. `.gitlab-ci.yml` files carry complex syntax: `include:` directives, `extends:` inheritance chains, `rules:` conditional logic, `artifacts:` report paths, and Auto DevOps overrides — all of which must be coordinated correctly for a pipeline to pass lint validation and actually produce useful signals. Engineers routinely spend hours chasing YAML indentation errors, missing cache keys, or mis-configured Docker-in-Docker before arriving at a working pipeline. GitLab Duo's pipeline generation demonstrates that LLM-assisted scaffold generation drastically reduces time-to-first-green-pipeline for new repositories and significantly lowers the maintenance burden for existing ones.

`tag ci gen-pipeline` automates this process end-to-end. Given a repository root, the command statically analyzes the codebase — detecting language(s), package manager, test runner, build tool, containerization approach, and deployment target — then constructs a syntactically valid, lint-passing `.gitlab-ci.yml` (or `.github/workflows/ci.yml` for GitHub Actions) that encodes GitLab's own AutoDevOps stage topology: `build`, `test`, `security-scan`, `package`, `review`, `staging`, `production`. The generated pipeline includes proper job templates, caching strategies, JUnit test report artifacts (`artifacts.reports.junit`), SAST report artifacts (`artifacts.reports.sast`), and security stage ordering. Validation is performed via the GitLab CI Lint API (`POST /projects/:id/ci/lint`) before the file is written, catching errors that client-side YAML schema validation would miss because the API resolves `include:` directives and CI variable references in project context.

The `--detect` mode auto-selects the platform (GitLab vs. GitHub) by inspecting the git remote URL. When the remote is `gitlab.com` or a self-managed GitLab instance, it generates `.gitlab-ci.yml`; when the remote is `github.com`, it generates a GitHub Actions workflow under `.github/workflows/`. Both outputs are validated before write: GitLab via the Lint API, GitHub via `actionlint` (if present) with JSON Schema fallback. A `--dry-run` flag prints the generated YAML to stdout without touching the filesystem, enabling review-before-commit workflows.

The feature lives entirely within the existing `ci.py` module, adding a `gen_pipeline` subsystem alongside the existing GitHub PR and CI log helpers. No new mandatory dependencies are introduced: detection and template rendering use stdlib only; the GitLab API call requires `GITLAB_TOKEN` (optional — falls back to unauthenticated lint if missing, which still catches most syntax errors); `actionlint` for GitHub validation is probed via `shutil.which` and skipped gracefully when absent. A new `ci_pipeline_generations` table in `tag.sqlite3` stores each generation event for auditability and future `tag ci gen-pipeline --history` commands.

This feature closes GitHub issue #344.

---

## 2. Problem Statement

### 2.1 Manual Pipeline Authoring is High-Friction and Error-Prone

GitLab CI YAML is powerful but complex. A correct pipeline for a Python project with pytest, Docker image build, and deployment to Kubernetes requires: correct image selection, pip caching with `key: files: ["requirements*.txt"]`, pytest `--junitxml` output wired to `artifacts.reports.junit`, a `kaniko` or Docker-in-Docker build stage with proper `DOCKER_TLS_CERTDIR` configuration, `rules:` to skip deployment on feature branches, and `environment:` blocks for review apps. Getting all of this right from scratch typically requires reading GitLab docs for 1-2 hours and at least 2-3 failed pipeline runs to diagnose YAML lint errors and misconfigured artifact paths. For teams onboarding a new repository to CI, this represents a significant productivity sink.

### 2.2 Platform Detection is Error-Prone When Done Manually

Teams that work across both GitLab and GitHub routinely generate the wrong pipeline file for a repository, copy-paste templates from the internet that target a different CI system, or maintain parallel pipelines that drift out of sync. The `--detect` flag eliminates this ambiguity by reading the git remote URL, resolving the correct platform, and generating the appropriate file format with no user intervention. Without automated detection, the user must know both systems' YAML dialects well enough to choose correctly — a precondition that is often not met.

### 2.3 Validation Gaps Lead to Silent Pipeline Failures

Client-side YAML schema validation (e.g., IDE extensions, JSON Schema) validates structure but cannot resolve GitLab-specific constructs: `include: [template: ...]` directives that pull in Auto DevOps templates, CI/CD variables referenced as `$KUBECONFIG`, or `extends:` keys that refer to jobs defined in included files. The GitLab CI Lint API performs full server-side merging and resolution, making it the authoritative validator. Without using this API, generated pipelines that appear schema-valid can still fail when parsed by GitLab's runner coordination layer. This PRD mandates lint API validation as a pre-write gate.

---

## 3. Goals

| ID | Goal |
|----|------|
| G1 | Analyze a repository root and detect language, package manager, test runner, build tool, containerization approach, and deployment target with no user prompting. |
| G2 | Generate a syntactically valid, lint-passing `.gitlab-ci.yml` that follows GitLab AutoDevOps stage topology and includes JUnit + SAST artifact report wiring. |
| G3 | Generate a syntactically valid `.github/workflows/ci.yml` for GitHub Actions with the same detection pipeline, validated by `actionlint` or JSON Schema. |
| G4 | Auto-detect the target platform (GitLab vs. GitHub) from the git remote URL via `--detect` mode. |
| G5 | Validate generated YAML via the GitLab CI Lint API before writing to disk; report lint errors with line numbers and fix suggestions. |
| G6 | Support `--dry-run` to print generated YAML to stdout without writing to disk. |
| G7 | Persist each generation event to `ci_pipeline_generations` in `tag.sqlite3` for auditability. |
| G8 | Integrate with the existing `ci.py` module; no new mandatory Python dependencies. |
| G9 | Support GitLab AutoDevOps pattern overrides via `--template` flag (e.g., `--template auto-devops`, `--template minimal`, `--template security-scan`). |
| G10 | Emit OpenTelemetry spans for the detection, generation, validation, and write phases (consistent with PRD-013). |

---

## 4. Non-Goals

| ID | Non-Goal |
|-----|----------|
| NG1 | Running the generated pipeline. This feature generates and validates YAML only; it does not trigger pipeline execution via the GitLab or GitHub API. |
| NG2 | Maintaining or updating existing pipeline files. Idempotent update and merge of an existing `.gitlab-ci.yml` is deferred to a follow-on PRD. |
| NG3 | Full GitLab Auto DevOps deployment pipeline with Kubernetes Helm chart generation. The generated pipeline includes deployment stage stubs; chart values files are out of scope. |
| NG4 | Custom CI/CD variable management (creating, updating, or rotating CI variables in the GitLab or GitHub API). |
| NG5 | Multi-project pipeline orchestration (`trigger:` jobs that chain across repositories). |
| NG6 | Windows-specific CI runner configurations. Generated pipelines target `ubuntu-latest` / `image: alpine` runners. |
| NG7 | LLM-assisted pipeline generation. Detection and template rendering are fully deterministic and rule-based; no LLM call is made during `gen-pipeline`. This avoids latency, cost, and non-determinism. |
| NG8 | Bitbucket Pipelines or CircleCI support. Only GitLab CI and GitHub Actions are in scope for this PRD. |

---

## 5. Success Metrics

| Metric | Target | Measurement Method |
|--------|--------|--------------------|
| Time-to-first-valid-pipeline for a new Python repo | < 10 seconds | Stopwatch from `tag ci gen-pipeline` invocation to file write with lint pass |
| Lint API pass rate on first generation attempt | >= 90% across test repos | Integration test suite against 10+ language fixtures |
| Language detection accuracy | >= 95% for top-8 languages | Offline evaluation against 50 open-source repos with known stacks |
| Generated pipeline executes successfully in GitLab CI | >= 80% on first run (no edits) | Manual verification against 5 representative repos |
| `--detect` platform selection accuracy | 100% for `github.com` and `gitlab.com` remotes | Unit test coverage of remote URL parsing |
| SQLite generation event write latency | < 5 ms P99 | Performance test with WAL mode |
| Total command wall time (`--dry-run`) | < 3 seconds for any repo | Benchmark test against large monorepo fixtures |

---

## 6. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|-----------|----------|
| U1 | Backend engineer onboarding a new Python service to GitLab | run `tag ci gen-pipeline --platform gitlab` in the repo root | I get a complete, lint-passing `.gitlab-ci.yml` with pytest, Docker build, and staging deploy stages without reading GitLab docs for hours |
| U2 | DevOps engineer managing 20 microservice repos | run `tag ci gen-pipeline --detect --output .gitlab-ci.yml` in each repo | I get the correct pipeline format for each repo's git host automatically, without checking each remote manually |
| U3 | Security engineer enforcing SAST on all pipelines | run `tag ci gen-pipeline --template security-scan` | All new pipelines include `artifacts.reports.sast` and the GitLab SAST template include, satisfying the org security policy |
| U4 | Developer working offline or behind a corporate proxy | run `tag ci gen-pipeline --platform gitlab --skip-lint` | I get generated YAML without requiring GitLab API access; I accept that server-side validation is deferred to GitLab's CI runner |
| U5 | Platform engineer evaluating the generated output before committing | run `tag ci gen-pipeline --platform gitlab --dry-run` | I can review the YAML in the terminal and pipe it to a linter or editor before any file is touched |
| U6 | Node.js developer with a custom test script | run `tag ci gen-pipeline --platform github --test-cmd "npm run test:ci"` | The generated workflow uses my exact test command instead of a generic `npm test` default |
| U7 | Compliance officer auditing CI pipeline provenance | run `tag ci gen-pipeline --history` | I can see every pipeline file generated by TAG for this repository with timestamps, detected stack, and lint result |
| U8 | Senior engineer wanting a minimal pipeline scaffold | run `tag ci gen-pipeline --template minimal` | I get a 3-stage (test, build, deploy) pipeline with no optional security or review-app stages, which I can extend manually |
| U9 | Go developer using a private GitLab instance | run `tag ci gen-pipeline --platform gitlab --gitlab-url https://git.corp.example.com` | The lint validation uses the correct self-managed GitLab URL rather than `gitlab.com` |
| U10 | CI/CD architect generating pipelines for multiple languages in a monorepo | run `tag ci gen-pipeline --platform gitlab --mono` | Each detected language component gets its own pipeline job block, using `rules: changes:` to trigger only on relevant path changes |

---

## 7. Proposed CLI Surface

All pipeline generation subcommands live under `tag ci gen-pipeline`.

### 7.1 `tag ci gen-pipeline --platform gitlab`

Generate a `.gitlab-ci.yml` for the current repository.

```
tag ci gen-pipeline \
  --platform gitlab \
  [--output .gitlab-ci.yml] \
  [--template {auto-devops,minimal,security-scan,custom}] \
  [--gitlab-url https://gitlab.com] \
  [--project-id <id>] \
  [--test-cmd "pytest tests/"] \
  [--build-cmd "docker build ."] \
  [--deploy-env staging] \
  [--mono] \
  [--dry-run] \
  [--skip-lint] \
  [--force] \
  [--json]
```

**Flags:**

- `--platform {gitlab,github}` — Target CI platform. Required unless `--detect` is used.
- `--output PATH` — Destination file path. Default: `.gitlab-ci.yml` for GitLab, `.github/workflows/ci.yml` for GitHub. Parent directories are created with `mkdir -p` semantics.
- `--template {auto-devops,minimal,security-scan,custom}` — Pipeline template variant. Default: `auto-devops` for GitLab, `standard` for GitHub.
- `--gitlab-url URL` — Base URL of the GitLab instance for lint validation. Default: `https://gitlab.com`. Reads `GITLAB_URL` env var if not set.
- `--project-id ID` — GitLab numeric project ID for lint API calls. When omitted, lint runs without project context (unauthenticated anonymous lint). When provided and `GITLAB_TOKEN` is set, the API resolves project-level CI variables.
- `--test-cmd CMD` — Override the auto-detected test command (e.g., `"pytest tests/ --junitxml=report.xml"`).
- `--build-cmd CMD` — Override the auto-detected build command.
- `--deploy-env ENV` — Set the default deployment environment name. Default: `staging`.
- `--mono` — Enable monorepo mode: detect multiple language stacks and emit separate jobs per component with `rules: changes:` path filters.
- `--dry-run` — Print generated YAML to stdout without writing to disk. Lint validation still runs.
- `--skip-lint` — Skip GitLab CI Lint API validation. Useful for offline use or when `GITLAB_TOKEN` is unavailable.
- `--force` — Overwrite existing output file without prompting.
- `--json` — Emit machine-readable JSON result to stdout (includes detected stack, lint result, output path).

**Example output (terminal):**

```
tag ci gen-pipeline --platform gitlab

Analyzing repository...
  Language:      Python 3.11
  Package mgr:   pip (requirements.txt)
  Test runner:   pytest
  Build:         Docker (Dockerfile detected)
  Deploy target: Kubernetes (helm/ directory detected)

Generating .gitlab-ci.yml (template: auto-devops)...
Validating via GitLab CI Lint API... OK (12 jobs, 6 stages)

Wrote .gitlab-ci.yml (187 lines)

Next steps:
  git add .gitlab-ci.yml && git commit -m "ci: add GitLab pipeline"
  Set CI variable KUBE_CONFIG in GitLab project settings for deploy stage
```

---

### 7.2 `tag ci gen-pipeline --platform github`

Generate a GitHub Actions workflow.

```
tag ci gen-pipeline \
  --platform github \
  [--output .github/workflows/ci.yml] \
  [--template {standard,security,release}] \
  [--test-cmd "pytest tests/"] \
  [--dry-run] \
  [--skip-lint] \
  [--force] \
  [--json]
```

Validates via `actionlint` (if `shutil.which("actionlint")` succeeds) then falls back to JSON Schema validation against `https://json.schemastore.org/github-workflow.json` (fetched once and cached in `~/.tag/cache/schema/`).

---

### 7.3 `tag ci gen-pipeline --detect`

Auto-detect platform from the git remote URL and generate accordingly.

```
tag ci gen-pipeline \
  --detect \
  [--output PATH] \
  [--dry-run] \
  [--json]
```

Detection logic:
1. Run `git remote get-url origin`.
2. Parse the URL.
3. If host matches `gitlab.com` or is a known GitLab pattern (`gitlab.*`, `git.*` with `/api/v4/` available), use `--platform gitlab`.
4. If host matches `github.com`, use `--platform github`.
5. If ambiguous, prompt the user interactively (skipped in `--json` mode; exits 1 with error).

---

### 7.4 `tag ci gen-pipeline --history`

Show past generation events for the current repository.

```
tag ci gen-pipeline --history [--last N] [--json]
```

Reads `ci_pipeline_generations` from SQLite. Output table:

```
ID   Generated At           Platform  Template     Stack           Lint  Output Path
g-1  2026-06-12 14:32:01Z  gitlab    auto-devops  Python/pytest   PASS  .gitlab-ci.yml
g-2  2026-06-12 15:00:42Z  github    standard     Python/pytest   PASS  .github/workflows/ci.yml
```

---

### 7.5 `tag ci gen-pipeline --validate`

Validate an existing `.gitlab-ci.yml` or `.github/workflows/ci.yml` without generating a new one.

```
tag ci gen-pipeline --validate --platform gitlab [--file .gitlab-ci.yml] [--json]
```

Reads the specified file, posts its content to the GitLab CI Lint API (or runs `actionlint` for GitHub), and reports errors with line numbers.

---

## 8. Functional Requirements

| ID | Requirement |
|----|-------------|
| FR-01 | **Repository detection — language:** `gen_pipeline` must detect the primary language by scanning for: `*.py` + `requirements*.txt` / `pyproject.toml` / `setup.py` (Python); `package.json` (Node.js); `go.mod` (Go); `Cargo.toml` (Rust); `pom.xml` / `build.gradle` (Java/Kotlin); `Gemfile` (Ruby); `*.cs` + `*.csproj` (.NET); `composer.json` (PHP). When multiple are found, the language associated with the most root-level files wins. Tie-breaking follows alphabetical order for determinism. |
| FR-02 | **Repository detection — package manager:** For Python, detect `pip` (requirements.txt), `poetry` (pyproject.toml with `[tool.poetry]`), or `uv` (uv.lock). For Node.js, detect `npm` (package-lock.json), `yarn` (yarn.lock), or `pnpm` (pnpm-lock.yaml). For Java, detect `maven` (pom.xml) or `gradle` (build.gradle / gradlew). Detection results are stored in the `DetectedStack` dataclass. |
| FR-03 | **Repository detection — test runner:** Detect pytest (pytest.ini, conftest.py, `[tool.pytest]` in pyproject.toml), unittest (no additional marker needed for Python), Jest (`jest` key in package.json), Vitest (`vitest.config.*`), Go test (`go test`), Cargo test (no additional marker), JUnit / surefire (pom.xml with surefire plugin). |
| FR-04 | **Repository detection — containerization:** Check for `Dockerfile` (root or `docker/`), `docker-compose.yml`, `.dockerignore`. If detected, mark `containerized=True` in `DetectedStack` and include a Docker build job in the generated pipeline. |
| FR-05 | **Repository detection — deployment target:** Check for `helm/` or `charts/` directory (Kubernetes), `k8s/` or `manifests/` directory (Kubernetes without Helm), `serverless.yml` (Serverless Framework), `fly.toml` (Fly.io), `.platform/` (Platform.sh), `Procfile` (Heroku/generic). |
| FR-06 | **GitLab YAML generation — stages:** The generated `.gitlab-ci.yml` must include a `stages:` block with at minimum `[build, test]`. When containerization is detected, add `package`. When a deployment target is detected, add `staging` and `production`. When `--template security-scan` or `auto-devops`, add `security-scan`. Order: `build → test → security-scan → package → review → staging → production`. |
| FR-07 | **GitLab YAML generation — JUnit artifacts:** Any test job that uses pytest, Jest, Vitest, Go test, or Maven surefire must include `artifacts: reports: junit: <path>` pointing to the correct JUnit XML output path. For pytest, the job command must include `--junitxml=report.xml`. For Jest/Vitest, include `--reporter=junit --outputFile=report.xml`. |
| FR-08 | **GitLab YAML generation — SAST:** When `--template auto-devops` or `--template security-scan`, include `include: [template: Security/SAST.gitlab-ci.yml]` and add `artifacts: reports: sast: gl-sast-report.json` to the security-scan stage job. |
| FR-09 | **GitLab YAML generation — caching:** Include a `cache:` block with `key: files: [<lockfile>]` where `<lockfile>` is the detected package manager's lock file (requirements.txt, package-lock.json, go.sum, Cargo.lock, etc.). Use `paths:` matching the package manager's cache directory (`.venv`, `node_modules`, `~/.cache/go/`, etc.). |
| FR-10 | **GitLab YAML generation — rules:** Feature branch jobs (test, build) must use `rules: [{if: '$CI_PIPELINE_SOURCE == "merge_request_event"'}, {if: '$CI_COMMIT_BRANCH'}]`. Staging deploy must restrict to `$CI_COMMIT_BRANCH == $CI_DEFAULT_BRANCH`. Production deploy must use `when: manual` with the same branch rule. |
| FR-11 | **GitLab CI Lint API validation:** Unless `--skip-lint`, after YAML generation, the content must be sent to `POST {gitlab_url}/api/v4/ci/lint` (unauthenticated) or `POST {gitlab_url}/api/v4/projects/{project_id}/ci/lint` (authenticated with `GITLAB_TOKEN`). The request body must be `{"content": "<yaml_string>", "dry_run": false}`. A response with `"status": "valid"` is required to proceed. On `"status": "invalid"`, print each error in `response["errors"]` with line number context extracted by matching error messages to YAML line numbers. |
| FR-12 | **GitLab CI Lint API — error presentation:** Lint errors must be displayed as: `  Line N: <error message>` with the corresponding YAML line printed below for context. The user must be prompted to fix and retry, or accept the invalid file with `--force`. |
| FR-13 | **GitHub Actions validation:** For `--platform github`, after generation, run `actionlint` if available (`shutil.which("actionlint")`). If not available, download the JSON Schema from `https://json.schemastore.org/github-workflow.json` (cached at `~/.tag/cache/schema/github-workflow.json`; re-fetch if older than 7 days) and validate using `jsonschema.validate`. Schema validation failure is treated as a warning (not a blocking error) since the JSON Schema does not cover all semantic rules. |
| FR-14 | **Output file write:** The output file is written only after lint validation passes (or `--skip-lint` / `--force` is set). Parent directories are created with `Path.mkdir(parents=True, exist_ok=True)`. If the output file exists and `--force` is not set, prompt the user: `File .gitlab-ci.yml already exists. Overwrite? [y/N]`. In non-TTY environments (piped stdin), default to No and exit 1. |
| FR-15 | **Dry-run mode:** When `--dry-run` is set, generated YAML is printed to stdout and lint validation runs normally. No file is written. The exit code reflects lint validation result (0 = valid, 1 = lint error). |
| FR-16 | **SQLite persistence:** Every successful `gen-pipeline` invocation (file written or `--dry-run` completed) must write one row to `ci_pipeline_generations`. Rows are written using `open_db()`. Failed runs (lint error without `--force`) do not write a row. |
| FR-17 | **`--detect` remote URL parsing:** The command runs `git remote get-url origin` via `subprocess.run`. The URL is parsed with `urllib.parse.urlparse`. Host matching: `gitlab.com` -> gitlab; `github.com` -> github; self-managed instances require `--platform` to be set explicitly or `--gitlab-url` to override detection. |
| FR-18 | **`--history` subcommand:** Reads `ci_pipeline_generations` for the current repo (detected via `git rev-parse --show-toplevel`). Displays rows in reverse chronological order. Supports `--last N` (default: 10) and `--json`. |
| FR-19 | **`--validate` subcommand:** Reads an existing CI file and posts it to the lint API or `actionlint`. Does not modify the file. Reports pass/fail with error details. Exit code 0 = valid, 1 = invalid or API error. |
| FR-20 | **Monorepo mode:** When `--mono` is set, `detect_stack()` is called recursively for each subdirectory that contains a language marker file (e.g., `services/*/requirements.txt`). Each detected component generates its own set of jobs in the same `.gitlab-ci.yml`, prefixed with the component name and using `rules: changes: ["<component_dir>/**/*"]`. |
| FR-21 | **OpenTelemetry tracing:** The `gen_pipeline()` function must emit OTel spans for: `ci.detect_stack` (attributes: detected language, package manager, test runner), `ci.render_template` (attributes: platform, template), `ci.lint_api` (attributes: lint status, error count), `ci.write_output` (attributes: output path, file size bytes). Follows PRD-013 patterns using `tracing.py`. |
| FR-22 | **`--json` output format:** When `--json` is set, stdout is a single JSON object: `{"detected_stack": {...}, "platform": "gitlab", "template": "auto-devops", "output_path": ".gitlab-ci.yml", "lint_status": "valid", "lint_errors": [], "generation_id": "g-<uuid>", "dry_run": false}`. Stderr receives progress messages. |

---

## 9. Non-Functional Requirements

| ID | Requirement |
|----|-------------|
| NFR-01 | **No mandatory new dependencies:** The detection and YAML rendering code must use only Python stdlib (`pathlib`, `subprocess`, `urllib.request`, `json`, `yaml` — already in `pyproject.toml`). The optional `jsonschema` import for GitHub schema validation must be guarded with `try/except ImportError`. |
| NFR-02 | **Wall-clock time:** `tag ci gen-pipeline --platform gitlab --dry-run` (no lint API call) must complete in < 3 seconds for any repository with fewer than 10,000 files. File traversal must use `os.scandir` or `Path.iterdir` (not `os.walk` full traversal); detection stops at the first confirmed match per language. |
| NFR-03 | **Lint API timeout:** The HTTP request to the GitLab CI Lint API must enforce a 10-second timeout (`urllib.request.urlopen(..., timeout=10)`). On timeout, print a warning and fall back as if `--skip-lint` was passed; the warning is included in `--json` output. |
| NFR-04 | **Generated YAML quality:** Generated pipelines must: (a) pass `yamllint` default rules (no trailing spaces, consistent 2-space indentation, no tabs), (b) be idempotent given the same `DetectedStack` inputs, (c) include a header comment crediting TAG and recording the generation timestamp and TAG version. |
| NFR-05 | **SQLite write isolation:** All writes to `ci_pipeline_generations` use the `open_db()` context manager from `controller.py`. WAL mode is enabled by default; no additional locking is required for single-writer access from this command. |
| NFR-06 | **Security — no credentials in generated YAML:** The YAML renderer must never embed the value of `GITLAB_TOKEN`, `KUBE_CONFIG`, or any other secret directly in the file. Secrets are referenced as CI variable names only (e.g., `$KUBE_CONFIG`). The generated YAML is scanned via the existing `security.py` secret-pattern detector before write (FR-14). If a match is found, the write is aborted and an error is printed. |
| NFR-07 | **Determinism:** Given identical repository contents and the same `--template` flag, `gen_pipeline()` must produce identical byte-for-byte YAML output. Template rendering must not include timestamps or UUIDs in the pipeline YAML body (only in the header comment and SQLite row). |
| NFR-08 | **Graceful offline behavior:** When the GitLab Lint API is unreachable (network error, DNS failure), the error is caught, a warning is printed (`Warning: GitLab Lint API unreachable; skipping server-side validation`), and the flow continues as if `--skip-lint` was set. The `--json` output includes `"lint_status": "skipped"` and `"lint_skip_reason": "api_unreachable"`. |
| NFR-09 | **TTY vs. pipe:** Progress output (Analyzing repository..., Generating..., Validating...) is printed to stderr so that `--dry-run` stdout can be piped cleanly to a file or another tool. When stderr is not a TTY, progress lines are omitted. |
| NFR-10 | **Test coverage:** The `gen_pipeline` subsystem must have >= 90% line coverage in `tests/test_ci_gen_pipeline.py`. Detection logic, template rendering, lint API integration (mocked), and SQLite persistence must each have dedicated test classes. |

---

## 10. Technical Design

### 10.1 New and Modified Files

| File | Change Type | Description |
|------|-------------|-------------|
| `src/tag/ci.py` | Extend | Add `detect_stack()`, `render_gitlab_pipeline()`, `render_github_workflow()`, `lint_gitlab_yaml()`, `validate_github_yaml()`, `gen_pipeline()`, `cmd_ci_gen_pipeline()`. Add `DetectedStack`, `PipelineGenConfig`, `PipelineGenResult` dataclasses. |
| `src/tag/controller.py` | Extend | Register `tag ci gen-pipeline` subparser; wire to `cmd_ci_gen_pipeline()`. Add `ci_pipeline_generations` table creation to `_init_db()`. |
| `tests/test_ci_gen_pipeline.py` | New | Unit + integration tests for all FR-01 through FR-22. |
| `src/tag/templates/gitlab/` | New | Jinja2-style string templates (stdlib `string.Template`) for each pipeline variant. No new `jinja2` dependency. |
| `src/tag/templates/github/` | New | String templates for GitHub Actions workflow variants. |

### 10.2 SQLite DDL

```sql
-- Migration: add to _init_db() in controller.py
CREATE TABLE IF NOT EXISTS ci_pipeline_generations (
    id              TEXT        PRIMARY KEY,           -- "g-" + uuid4 hex
    repo_root       TEXT        NOT NULL,              -- git rev-parse --show-toplevel result
    generated_at    TEXT        NOT NULL,              -- ISO 8601 UTC: strftime('%Y-%m-%dT%H:%M:%SZ')
    platform        TEXT        NOT NULL,              -- 'gitlab' | 'github'
    template        TEXT        NOT NULL,              -- 'auto-devops' | 'minimal' | 'security-scan' | 'standard'
    detected_lang   TEXT        NOT NULL,              -- 'python' | 'nodejs' | 'go' | etc.
    detected_stack  TEXT        NOT NULL,              -- JSON blob: DetectedStack serialized
    lint_status     TEXT        NOT NULL,              -- 'valid' | 'invalid' | 'skipped'
    lint_errors     TEXT,                              -- JSON array of error strings, or NULL
    output_path     TEXT,                              -- absolute path to written file, NULL if --dry-run
    dry_run         INTEGER     NOT NULL DEFAULT 0,    -- 1 if --dry-run
    tag_version     TEXT        NOT NULL,              -- __version__
    CONSTRAINT ci_pipeline_platform_chk CHECK (platform IN ('gitlab', 'github')),
    CONSTRAINT ci_pipeline_lint_chk     CHECK (lint_status IN ('valid', 'invalid', 'skipped'))
);

CREATE INDEX IF NOT EXISTS idx_ci_pipeline_repo_generated
    ON ci_pipeline_generations (repo_root, generated_at DESC);
```

### 10.3 Core Dataclasses

```python
# src/tag/ci.py additions

from __future__ import annotations
import dataclasses
import json
from pathlib import Path
from typing import Optional


@dataclasses.dataclass
class DetectedStack:
    """Result of static repository analysis."""
    language: str                        # e.g. "python", "nodejs", "go"
    language_version: Optional[str]      # e.g. "3.11", "20", "1.22"
    package_manager: str                 # e.g. "pip", "npm", "go-modules"
    lockfile: Optional[str]              # e.g. "requirements.txt", "package-lock.json"
    test_runner: str                     # e.g. "pytest", "jest", "go-test"
    test_cmd: Optional[str]              # e.g. "pytest tests/ --junitxml=report.xml"
    junit_report_path: Optional[str]     # e.g. "report.xml"
    build_cmd: Optional[str]             # e.g. "docker build -t $CI_REGISTRY_IMAGE ."
    containerized: bool                  # True if Dockerfile detected
    deploy_target: Optional[str]         # "kubernetes-helm", "kubernetes", "fly", "heroku", None
    components: list[ComponentStack]     # populated in --mono mode; empty otherwise
    cache_paths: list[str]               # e.g. [".venv", "~/.cache/pip"]
    image: str                           # base Docker image for CI runner

    def to_json(self) -> str:
        return json.dumps(dataclasses.asdict(self))


@dataclasses.dataclass
class ComponentStack:
    """One language component in a monorepo."""
    name: str                            # e.g. "auth-service"
    path: str                            # relative path from repo root, e.g. "services/auth"
    stack: DetectedStack                 # nested detection result for this component


@dataclasses.dataclass
class PipelineGenConfig:
    """User-supplied configuration for pipeline generation."""
    platform: str                        # "gitlab" | "github"
    template: str                        # "auto-devops" | "minimal" | "security-scan" | "standard"
    output_path: Path
    gitlab_url: str                      # default: "https://gitlab.com"
    project_id: Optional[str]            # GitLab numeric project ID for authenticated lint
    test_cmd_override: Optional[str]
    build_cmd_override: Optional[str]
    deploy_env: str                      # default: "staging"
    mono: bool
    dry_run: bool
    skip_lint: bool
    force: bool
    json_output: bool
    repo_root: Path                      # git rev-parse --show-toplevel


@dataclasses.dataclass
class LintResult:
    """Result of GitLab CI Lint API call or actionlint."""
    status: str                          # "valid" | "invalid" | "skipped"
    errors: list[str]                    # empty list if valid
    warnings: list[str]
    skip_reason: Optional[str]           # populated when status == "skipped"


@dataclasses.dataclass
class PipelineGenResult:
    """Final result returned by gen_pipeline()."""
    generation_id: str                   # "g-" + uuid4.hex
    detected_stack: DetectedStack
    platform: str
    template: str
    yaml_content: str                    # the generated YAML string
    lint_result: LintResult
    output_path: Optional[Path]          # None if --dry-run
    dry_run: bool

    def to_json_dict(self) -> dict:
        return {
            "generation_id": self.generation_id,
            "detected_stack": dataclasses.asdict(self.detected_stack),
            "platform": self.platform,
            "template": self.template,
            "lint_status": self.lint_result.status,
            "lint_errors": self.lint_result.errors,
            "output_path": str(self.output_path) if self.output_path else None,
            "dry_run": self.dry_run,
        }
```

### 10.4 Detection Algorithm

```python
# src/tag/ci.py

import os
from pathlib import Path
from typing import Optional

# Language marker rules: (marker_files, language, default_image)
_LANG_RULES: list[tuple[list[str], str, str]] = [
    (["pyproject.toml", "setup.py", "requirements.txt"], "python", "python:3.11-slim"),
    (["package.json"],                                   "nodejs", "node:20-alpine"),
    (["go.mod"],                                         "go",     "golang:1.22-alpine"),
    (["Cargo.toml"],                                     "rust",   "rust:1.78-slim"),
    (["pom.xml"],                                        "java",   "maven:3.9-eclipse-temurin-21"),
    (["build.gradle", "build.gradle.kts"],               "java",   "gradle:8.7-jdk21"),
    (["Gemfile"],                                        "ruby",   "ruby:3.3-slim"),
    (["composer.json"],                                  "php",    "php:8.3-cli"),
    (["*.csproj", "*.sln"],                              "dotnet", "mcr.microsoft.com/dotnet/sdk:8.0"),
]

_PKG_MANAGERS: dict[str, list[tuple[str, str]]] = {
    "python": [
        ("uv.lock",           "uv"),
        ("poetry.lock",       "poetry"),
        ("requirements.txt",  "pip"),
        ("pyproject.toml",    "pip"),   # fallback if no other lockfile
    ],
    "nodejs": [
        ("pnpm-lock.yaml",    "pnpm"),
        ("yarn.lock",         "yarn"),
        ("package-lock.json", "npm"),
    ],
    "go":     [("go.sum",     "go-modules")],
    "rust":   [("Cargo.lock", "cargo")],
    "java":   [("pom.xml",    "maven"), ("build.gradle", "gradle")],
    "ruby":   [("Gemfile.lock","bundler")],
    "php":    [("composer.lock","composer")],
}

_TEST_RUNNER_MARKERS: dict[str, list[tuple[str, str, str, str]]] = {
    # language: [(marker_file, test_runner, test_cmd, junit_path), ...]
    "python": [
        ("pytest.ini",    "pytest", "pytest tests/ --junitxml=report.xml", "report.xml"),
        ("conftest.py",   "pytest", "pytest tests/ --junitxml=report.xml", "report.xml"),
        ("pyproject.toml","pytest", "pytest tests/ --junitxml=report.xml", "report.xml"),
    ],
    "nodejs": [
        ("jest.config.*", "jest",   "npx jest --reporters=jest-junit", "junit.xml"),
        ("vitest.config.*","vitest","npx vitest run --reporter=junit", "junit.xml"),
    ],
    "go":     [
        ("go.mod",        "go-test","go test ./... -v 2>&1 | go-junit-report > report.xml", "report.xml"),
    ],
    "java":   [
        ("pom.xml",       "maven",  "mvn test",  "target/surefire-reports/"),
        ("build.gradle",  "gradle", "gradle test","build/test-results/"),
    ],
}

_CACHE_PATHS: dict[str, dict[str, list[str]]] = {
    "python": {
        "pip":    [".venv", "~/.cache/pip"],
        "poetry": [".venv", "~/.cache/pypoetry"],
        "uv":     [".venv", "~/.cache/uv"],
    },
    "nodejs": {
        "npm":    ["node_modules", "~/.npm"],
        "yarn":   ["node_modules", "~/.yarn/cache"],
        "pnpm":   ["node_modules", "~/.pnpm-store"],
    },
    "go":     {"go-modules": ["~/.cache/go"]},
    "rust":   {"cargo":      ["~/.cargo/registry", "target/"]},
    "java":   {"maven":      ["~/.m2"], "gradle": ["~/.gradle"]},
}


def detect_stack(repo_root: Path) -> DetectedStack:
    """
    Statically analyze *repo_root* and return a DetectedStack.

    Scans only the top level of the repository (O(1) file checks per rule).
    Stops at the first language match for each marker set.
    """
    root_files = {f.name for f in repo_root.iterdir() if f.is_file()}
    # also capture glob patterns like *.csproj
    root_names_lower = {n.lower() for n in root_files}

    language = "unknown"
    image = "alpine:3.19"
    for markers, lang, img in _LANG_RULES:
        for marker in markers:
            if "*" in marker:
                ext = marker.lstrip("*")
                if any(n.endswith(ext) for n in root_names_lower):
                    language, image = lang, img
                    break
            elif marker in root_files:
                language, image = lang, img
                break
        if language != "unknown":
            break

    # Detect package manager
    pkg_manager = "unknown"
    lockfile = None
    for fname, pm in _PKG_MANAGERS.get(language, []):
        if fname in root_files:
            pkg_manager = pm
            lockfile = fname
            break

    # Detect test runner
    test_runner = "unknown"
    test_cmd = None
    junit_path = None
    for fname, runner, cmd, jpath in _TEST_RUNNER_MARKERS.get(language, []):
        if "*" in fname:
            ext = fname.split("*", 1)[1]
            if any(f.endswith(ext) for f in root_files):
                test_runner, test_cmd, junit_path = runner, cmd, jpath
                break
        elif fname in root_files:
            test_runner, test_cmd, junit_path = runner, cmd, jpath
            break

    # Containerization
    containerized = (
        "Dockerfile" in root_files
        or "docker-compose.yml" in root_files
        or "docker-compose.yaml" in root_files
    )

    # Deployment target
    deploy_target = None
    root_dirs = {d.name for d in repo_root.iterdir() if d.is_dir()}
    if "helm" in root_dirs or "charts" in root_dirs:
        deploy_target = "kubernetes-helm"
    elif "k8s" in root_dirs or "manifests" in root_dirs or "kubernetes" in root_dirs:
        deploy_target = "kubernetes"
    elif "fly.toml" in root_files:
        deploy_target = "fly"
    elif "Procfile" in root_files:
        deploy_target = "heroku"
    elif "serverless.yml" in root_files or "serverless.yaml" in root_files:
        deploy_target = "serverless"

    # Cache paths
    cache_paths = _CACHE_PATHS.get(language, {}).get(pkg_manager, [])

    # Language version sniffing (best-effort)
    language_version = _detect_language_version(repo_root, language, root_files)

    return DetectedStack(
        language=language,
        language_version=language_version,
        package_manager=pkg_manager,
        lockfile=lockfile,
        test_runner=test_runner,
        test_cmd=test_cmd,
        junit_report_path=junit_path,
        build_cmd="docker build -t $CI_REGISTRY_IMAGE:$CI_COMMIT_SHA ." if containerized else None,
        containerized=containerized,
        deploy_target=deploy_target,
        components=[],
        cache_paths=cache_paths,
        image=image,
    )


def _detect_language_version(
    repo_root: Path, language: str, root_files: set[str]
) -> Optional[str]:
    """Best-effort language version detection from common config files."""
    try:
        if language == "python":
            for fname in (".python-version", ".tool-versions"):
                p = repo_root / fname
                if p.exists():
                    content = p.read_text(errors="replace").strip()
                    if fname == ".python-version":
                        return content.split("\n")[0].strip()
                    for line in content.splitlines():
                        if line.startswith("python "):
                            return line.split()[1]
            # Try pyproject.toml requires-python
            pp = repo_root / "pyproject.toml"
            if pp.exists():
                import re
                text = pp.read_text(errors="replace")
                m = re.search(r'requires-python\s*=\s*"[>=~!^]*(\d+\.\d+)', text)
                if m:
                    return m.group(1)
        elif language == "nodejs":
            p = repo_root / ".nvmrc"
            if p.exists():
                return p.read_text(errors="replace").strip().lstrip("v")
            p = repo_root / ".node-version"
            if p.exists():
                return p.read_text(errors="replace").strip().lstrip("v")
        elif language == "go":
            p = repo_root / "go.mod"
            if p.exists():
                import re
                m = re.search(r'^go\s+(\d+\.\d+)', p.read_text(errors="replace"), re.M)
                if m:
                    return m.group(1)
    except Exception:
        pass
    return None
```

### 10.5 GitLab CI Lint API Integration

```python
# src/tag/ci.py

import urllib.request
import urllib.error
import json as _json


def lint_gitlab_yaml(
    yaml_content: str,
    gitlab_url: str = "https://gitlab.com",
    project_id: Optional[str] = None,
    token: Optional[str] = None,
    timeout: int = 10,
) -> LintResult:
    """
    POST yaml_content to the GitLab CI Lint API and return a LintResult.

    Uses project-scoped endpoint when project_id is provided (resolves
    include: directives and CI variables in project context).
    Falls back to anonymous endpoint otherwise.

    Reference: https://docs.gitlab.com/ee/api/lint.html
    """
    if project_id and token:
        url = f"{gitlab_url.rstrip('/')}/api/v4/projects/{project_id}/ci/lint"
    else:
        url = f"{gitlab_url.rstrip('/')}/api/v4/ci/lint"

    payload = _json.dumps({
        "content": yaml_content,
        "dry_run": False,
        "include_jobs": False,
        "ref": "main",
    }).encode()

    headers = {
        "Content-Type": "application/json",
        "Accept": "application/json",
    }
    if token:
        headers["PRIVATE-TOKEN"] = token

    try:
        req = urllib.request.Request(url, data=payload, headers=headers, method="POST")
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            body = _json.loads(resp.read().decode())
    except urllib.error.HTTPError as exc:
        return LintResult(
            status="skipped",
            errors=[f"HTTP {exc.code}: {exc.reason}"],
            warnings=[],
            skip_reason=f"http_error_{exc.code}",
        )
    except (urllib.error.URLError, OSError):
        return LintResult(
            status="skipped",
            errors=[],
            warnings=["GitLab Lint API unreachable"],
            skip_reason="api_unreachable",
        )

    status = "valid" if body.get("status") == "valid" or body.get("valid") is True else "invalid"
    errors = body.get("errors", [])
    warnings = body.get("warnings", [])

    return LintResult(
        status=status,
        errors=errors,
        warnings=warnings,
        skip_reason=None,
    )
```

### 10.6 Template Rendering

Templates are stored as Python string constants (or stdlib `string.Template` objects) in `src/tag/templates/`. No Jinja2 dependency is introduced.

```python
# Partial example: GitLab minimal template for Python/pytest

_GITLAB_MINIMAL_PYTHON = """\
# Generated by TAG {tag_version} on {generated_at}
# Stack: {language} / {package_manager} / {test_runner}
# Do not edit the generation header; edit jobs below as needed.

image: {image}

stages:
  - test
{build_stage}
{deploy_stages}

variables:
  PIP_CACHE_DIR: "$CI_PROJECT_DIR/.cache/pip"

cache:
  key:
    files:
      - {lockfile}
  paths:
{cache_paths}

test:
  stage: test
  script:
    - {install_cmd}
    - {test_cmd}
  artifacts:
    reports:
      junit: {junit_report_path}
    when: always
  rules:
    - if: '$CI_PIPELINE_SOURCE == "merge_request_event"'
    - if: '$CI_COMMIT_BRANCH'
"""
```

Template variants are selected by the `template` field in `PipelineGenConfig`. The `render_gitlab_pipeline()` function:
1. Picks the base template string for the requested variant.
2. Fills in all `{placeholder}` fields from `DetectedStack`.
3. Conditionally appends build, package, security-scan, and deploy job blocks.
4. Applies user overrides (`--test-cmd`, `--build-cmd`).
5. Returns the rendered YAML string.

### 10.7 `gen_pipeline()` Orchestration

```python
def gen_pipeline(config: PipelineGenConfig) -> PipelineGenResult:
    """
    Main orchestration function for CI pipeline generation.

    Phases (each emits an OTel span):
      1. detect_stack()       — analyze repository
      2. render_*_pipeline()  — generate YAML string
      3. lint_*_yaml()        — validate with API or actionlint
      4. write output / dry-run print
      5. persist to SQLite
    """
    import uuid
    from tag import __version__
    from tag.tracing import get_tracer

    generation_id = f"g-{uuid.uuid4().hex}"
    tracer = get_tracer("tag.ci.gen_pipeline")

    with tracer.start_as_current_span("ci.detect_stack") as span:
        stack = detect_stack(config.repo_root)
        # Apply user overrides
        if config.test_cmd_override:
            stack.test_cmd = config.test_cmd_override
        if config.build_cmd_override:
            stack.build_cmd = config.build_cmd_override
        span.set_attribute("detected.language", stack.language)
        span.set_attribute("detected.package_manager", stack.package_manager)
        span.set_attribute("detected.test_runner", stack.test_runner)

    with tracer.start_as_current_span("ci.render_template") as span:
        if config.platform == "gitlab":
            yaml_content = render_gitlab_pipeline(stack, config)
        else:
            yaml_content = render_github_workflow(stack, config)
        span.set_attribute("platform", config.platform)
        span.set_attribute("template", config.template)

    with tracer.start_as_current_span("ci.lint") as span:
        if config.skip_lint:
            lint_result = LintResult(status="skipped", errors=[], warnings=[], skip_reason="user_requested")
        elif config.platform == "gitlab":
            import os
            token = os.environ.get("GITLAB_TOKEN")
            lint_result = lint_gitlab_yaml(
                yaml_content,
                gitlab_url=config.gitlab_url,
                project_id=config.project_id,
                token=token,
            )
        else:
            lint_result = validate_github_yaml(yaml_content)
        span.set_attribute("lint.status", lint_result.status)
        span.set_attribute("lint.error_count", len(lint_result.errors))

    with tracer.start_as_current_span("ci.write_output") as span:
        output_path = None
        if not config.dry_run and lint_result.status != "invalid":
            _write_pipeline_file(yaml_content, config)
            output_path = config.output_path.resolve()
            span.set_attribute("output.path", str(output_path))
            span.set_attribute("output.bytes", len(yaml_content.encode()))

    _persist_generation(
        generation_id=generation_id,
        config=config,
        stack=stack,
        lint_result=lint_result,
        output_path=output_path,
    )

    return PipelineGenResult(
        generation_id=generation_id,
        detected_stack=stack,
        platform=config.platform,
        template=config.template,
        yaml_content=yaml_content,
        lint_result=lint_result,
        output_path=output_path,
        dry_run=config.dry_run,
    )
```

### 10.8 Remote URL Detection

```python
import subprocess
import urllib.parse

_GITLAB_HOSTS = frozenset(["gitlab.com"])
_GITHUB_HOSTS = frozenset(["github.com"])


def detect_platform_from_remote() -> Optional[str]:
    """
    Return 'gitlab', 'github', or None by inspecting the origin remote URL.

    Supports SSH URLs (git@gitlab.com:org/repo.git),
    HTTPS URLs (https://gitlab.com/org/repo.git),
    and SSH-with-port forms (ssh://git@gitlab.com:2222/org/repo.git).
    """
    try:
        result = subprocess.run(
            ["git", "remote", "get-url", "origin"],
            capture_output=True,
            text=True,
            timeout=5,
        )
        if result.returncode != 0:
            return None
        remote_url = result.stdout.strip()
    except (subprocess.TimeoutExpired, FileNotFoundError):
        return None

    # Normalize SSH shorthand: git@host:path -> https://host/path
    if remote_url.startswith("git@"):
        # git@gitlab.com:org/repo.git  ->  gitlab.com
        host_part = remote_url[4:].split(":")[0]
    else:
        parsed = urllib.parse.urlparse(remote_url)
        host_part = parsed.hostname or ""

    host_lower = host_part.lower()
    if host_lower in _GITLAB_HOSTS or "gitlab" in host_lower:
        return "gitlab"
    if host_lower in _GITHUB_HOSTS or "github" in host_lower:
        return "github"
    return None
```

### 10.9 SQLite Persistence

```python
def _persist_generation(
    generation_id: str,
    config: PipelineGenConfig,
    stack: DetectedStack,
    lint_result: LintResult,
    output_path: Optional[Path],
) -> None:
    """Write one row to ci_pipeline_generations using open_db()."""
    import datetime
    from tag import __version__
    # open_db() is imported from controller.py or a shared db module
    from tag.controller import open_db

    now = datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    with open_db() as conn:
        conn.execute(
            """
            INSERT INTO ci_pipeline_generations
                (id, repo_root, generated_at, platform, template,
                 detected_lang, detected_stack, lint_status, lint_errors,
                 output_path, dry_run, tag_version)
            VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
            """,
            (
                generation_id,
                str(config.repo_root),
                now,
                config.platform,
                config.template,
                stack.language,
                stack.to_json(),
                lint_result.status,
                json.dumps(lint_result.errors),
                str(output_path) if output_path else None,
                1 if config.dry_run else 0,
                __version__,
            ),
        )
        conn.commit()
```

### 10.10 Integration with `controller.py`

```python
# In controller.py: _init_db() — add after existing CREATE TABLE statements
conn.execute("""
    CREATE TABLE IF NOT EXISTS ci_pipeline_generations (
        id              TEXT PRIMARY KEY,
        repo_root       TEXT NOT NULL,
        generated_at    TEXT NOT NULL,
        platform        TEXT NOT NULL,
        template        TEXT NOT NULL,
        detected_lang   TEXT NOT NULL,
        detected_stack  TEXT NOT NULL,
        lint_status     TEXT NOT NULL,
        lint_errors     TEXT,
        output_path     TEXT,
        dry_run         INTEGER NOT NULL DEFAULT 0,
        tag_version     TEXT NOT NULL,
        CONSTRAINT ci_pipeline_platform_chk CHECK (platform IN ('gitlab', 'github')),
        CONSTRAINT ci_pipeline_lint_chk     CHECK (lint_status IN ('valid', 'invalid', 'skipped'))
    )
""")
conn.execute("""
    CREATE INDEX IF NOT EXISTS idx_ci_pipeline_repo_generated
        ON ci_pipeline_generations (repo_root, generated_at DESC)
""")

# In controller.py: subparser registration
def _register_ci_subparsers(ci_subparsers):
    gen = ci_subparsers.add_parser(
        "gen-pipeline",
        help="Generate a CI/CD pipeline for this repository.",
    )
    gen.add_argument("--platform", choices=["gitlab", "github"])
    gen.add_argument("--detect", action="store_true")
    gen.add_argument("--output", type=Path, default=None)
    gen.add_argument("--template", default=None,
                     choices=["auto-devops", "minimal", "security-scan", "standard", "release"])
    gen.add_argument("--gitlab-url", default="https://gitlab.com")
    gen.add_argument("--project-id", default=None)
    gen.add_argument("--test-cmd", default=None)
    gen.add_argument("--build-cmd", default=None)
    gen.add_argument("--deploy-env", default="staging")
    gen.add_argument("--mono", action="store_true")
    gen.add_argument("--dry-run", action="store_true")
    gen.add_argument("--skip-lint", action="store_true")
    gen.add_argument("--force", action="store_true")
    gen.add_argument("--json", dest="json_output", action="store_true")
    gen.add_argument("--history", action="store_true")
    gen.add_argument("--last", type=int, default=10)
    gen.add_argument("--validate", action="store_true")
    gen.add_argument("--file", type=Path, default=None)
    gen.set_defaults(func=cmd_ci_gen_pipeline)
```

---

## 11. Security Considerations

1. **No secrets in generated YAML.** Template rendering never interpolates environment variable _values_ — only CI variable _names_ (e.g., `$KUBE_CONFIG`, `$CI_REGISTRY_PASSWORD`). The rendered YAML is passed through `security.py`'s secret pattern scanner before write; any high-entropy string match aborts the write and prints an error. This prevents accidental credential leakage if a user passes a secret value via `--build-cmd`.

2. **`GITLAB_TOKEN` handling.** The token is read from the environment (`os.environ.get("GITLAB_TOKEN")`) and never logged, stored in SQLite, or printed in any output mode including `--json`. It is passed only as an HTTP request header to the GitLab Lint API and discarded after the request.

3. **Remote URL injection.** The value returned by `git remote get-url origin` is used only as input to `urllib.parse.urlparse` for host extraction. It is never passed to a shell or used as a template substitution that could execute arbitrary code. The `subprocess.run` call uses a list form (not `shell=True`).

4. **Path traversal in `--output`.** The `--output` path is resolved with `Path.resolve()` after construction. A check confirms the resolved path is within the repository root or a user-specified allowed prefix; paths that resolve outside the repo root emit a warning and require `--force` to proceed.

5. **GitLab Lint API SSRF.** The `--gitlab-url` parameter is parsed with `urllib.parse.urlparse`; only `http://` and `https://` schemes are accepted. Attempts to use `file://`, `ftp://`, or other schemes are rejected with a `ValueError` before any request is made.

6. **Template file injection.** The `string.Template` substitution uses `safe_substitute()` to avoid `KeyError` on undefined placeholders. User-supplied overrides (`--test-cmd`, `--build-cmd`) are inserted as YAML scalar strings; they are YAML-escaped (single-quoted) to prevent YAML injection if they contain `:`, `#`, or other special characters.

7. **Schema cache freshness.** The GitHub JSON Schema cached at `~/.tag/cache/schema/github-workflow.json` is re-fetched if its `mtime` is older than 7 days. The fetch uses `https://` only; the downloaded content is parsed as JSON before writing to disk to prevent storing a malformed file.

8. **Dependency on `actionlint`.** `actionlint` is invoked via `subprocess.run` with a fixed list argument (no shell interpolation). The generated YAML is passed via a temporary file written with `tempfile.NamedTemporaryFile(suffix=".yml", delete=False)` — not via stdin shell redirection.

---

## 12. Testing Strategy

### 12.1 Unit Tests (`tests/test_ci_gen_pipeline.py`)

**Detection tests:**

```python
class TestDetectStack:
    def test_python_pip(self, tmp_path):
        (tmp_path / "requirements.txt").write_text("flask\npytest\n")
        (tmp_path / "conftest.py").write_text("")
        stack = detect_stack(tmp_path)
        assert stack.language == "python"
        assert stack.package_manager == "pip"
        assert stack.test_runner == "pytest"
        assert "report.xml" in (stack.junit_report_path or "")

    def test_nodejs_pnpm(self, tmp_path):
        (tmp_path / "package.json").write_text('{"name":"app","scripts":{"test":"vitest"}}')
        (tmp_path / "pnpm-lock.yaml").write_text("")
        (tmp_path / "vitest.config.ts").write_text("")
        stack = detect_stack(tmp_path)
        assert stack.language == "nodejs"
        assert stack.package_manager == "pnpm"
        assert stack.test_runner == "vitest"

    def test_go_modules(self, tmp_path):
        (tmp_path / "go.mod").write_text("module example.com/app\n\ngo 1.22\n")
        stack = detect_stack(tmp_path)
        assert stack.language == "go"
        assert stack.language_version == "1.22"

    def test_containerized(self, tmp_path):
        (tmp_path / "requirements.txt").write_text("")
        (tmp_path / "Dockerfile").write_text("FROM python:3.11\n")
        stack = detect_stack(tmp_path)
        assert stack.containerized is True
        assert stack.build_cmd is not None

    def test_kubernetes_helm_deploy(self, tmp_path):
        (tmp_path / "requirements.txt").write_text("")
        (tmp_path / "helm").mkdir()
        stack = detect_stack(tmp_path)
        assert stack.deploy_target == "kubernetes-helm"

    def test_unknown_language(self, tmp_path):
        (tmp_path / "README.md").write_text("# Project\n")
        stack = detect_stack(tmp_path)
        assert stack.language == "unknown"
```

**Template rendering tests:**

```python
class TestRenderGitlabPipeline:
    def test_stages_auto_devops(self, python_stack):
        config = make_config(platform="gitlab", template="auto-devops")
        yaml_str = render_gitlab_pipeline(python_stack, config)
        doc = yaml.safe_load(yaml_str)
        assert "test" in doc["stages"]
        assert "security-scan" in doc["stages"]

    def test_junit_artifact(self, python_stack):
        config = make_config(platform="gitlab", template="minimal")
        yaml_str = render_gitlab_pipeline(python_stack, config)
        doc = yaml.safe_load(yaml_str)
        # Find a job with a junit report
        junit_jobs = [
            j for j in doc.values()
            if isinstance(j, dict)
            and j.get("artifacts", {}).get("reports", {}).get("junit")
        ]
        assert len(junit_jobs) >= 1

    def test_cache_key_lockfile(self, python_pip_stack):
        config = make_config(platform="gitlab", template="minimal")
        yaml_str = render_gitlab_pipeline(python_pip_stack, config)
        assert "requirements.txt" in yaml_str

    def test_no_secrets_in_output(self, python_stack):
        config = make_config(platform="gitlab", template="minimal")
        config.build_cmd_override = "docker build ."
        yaml_str = render_gitlab_pipeline(python_stack, config)
        # Must not contain raw token values
        assert "glpat-" not in yaml_str
        assert "ghp_" not in yaml_str

    def test_deterministic(self, python_stack):
        config = make_config(platform="gitlab", template="minimal")
        yaml1 = render_gitlab_pipeline(python_stack, config)
        yaml2 = render_gitlab_pipeline(python_stack, config)
        assert yaml1 == yaml2
```

**Lint API tests (mocked):**

```python
class TestLintGitlabYaml:
    def test_valid_response(self, requests_mock):
        requests_mock.post(
            "https://gitlab.com/api/v4/ci/lint",
            json={"status": "valid", "errors": [], "warnings": []},
        )
        result = lint_gitlab_yaml("stages:\n  - test\n")
        assert result.status == "valid"
        assert result.errors == []

    def test_invalid_response(self, requests_mock):
        requests_mock.post(
            "https://gitlab.com/api/v4/ci/lint",
            json={"status": "invalid", "errors": ["jobs:test config contains unknown keys: foo"]},
        )
        result = lint_gitlab_yaml("stages:\n  - test\njobs:\n  test:\n    foo: bar\n")
        assert result.status == "invalid"
        assert len(result.errors) == 1

    def test_api_unreachable(self):
        # Use a non-routable address to simulate unreachable API
        result = lint_gitlab_yaml("stages:\n  - test\n", gitlab_url="http://192.0.2.1", timeout=1)
        assert result.status == "skipped"
        assert result.skip_reason == "api_unreachable"
```

**SQLite persistence tests:**

```python
class TestPersistGeneration:
    def test_row_written(self, tmp_db):
        config = make_config(platform="gitlab", template="minimal")
        config.dry_run = True
        # ... run gen_pipeline() with mocked lint ...
        with open_db() as conn:
            rows = conn.execute("SELECT * FROM ci_pipeline_generations").fetchall()
        assert len(rows) == 1
        assert rows[0]["platform"] == "gitlab"
        assert rows[0]["lint_status"] in ("valid", "skipped")
```

### 12.2 Integration Tests

Integration tests run against a set of fixture repositories in `tests/fixtures/ci_gen/`:

| Fixture | Language | Package Mgr | Expected Stages |
|---------|----------|-------------|-----------------|
| `python-flask-docker` | Python | pip | build, test, package, staging |
| `nodejs-express` | Node.js | npm | build, test, staging |
| `go-api` | Go | go-modules | test, build |
| `java-spring` | Java | maven | test, package |
| `rust-cli` | Rust | cargo | test, build |
| `monorepo-python-node` | Python + Node | pip + npm | test (×2, path-filtered) |

Each fixture test:
1. Calls `detect_stack(fixture_path)` and asserts on key fields.
2. Calls `render_gitlab_pipeline()` and asserts on YAML structure.
3. Parses the rendered YAML with `yaml.safe_load()` to confirm it is valid YAML.
4. (Optionally, with `GITLAB_TOKEN` in env) calls `lint_gitlab_yaml()` and asserts `status == "valid"`.

### 12.3 Performance Tests

```python
class TestPerformance:
    def test_detect_stack_under_1s(self, large_repo_fixture):
        """detect_stack on a 5000-file repo must complete in < 1 second."""
        import time
        start = time.monotonic()
        detect_stack(large_repo_fixture)
        elapsed = time.monotonic() - start
        assert elapsed < 1.0

    def test_render_pipeline_under_100ms(self, python_stack):
        import time
        config = make_config(platform="gitlab", template="auto-devops")
        start = time.monotonic()
        render_gitlab_pipeline(python_stack, config)
        elapsed = time.monotonic() - start
        assert elapsed < 0.1
```

---

## 13. Acceptance Criteria

| ID | Criterion | Testable? |
|----|-----------|-----------|
| AC-01 | `tag ci gen-pipeline --platform gitlab --dry-run` in a Python/pip/pytest repo with `Dockerfile` and `helm/` directory prints a YAML string containing stages `[build, test, security-scan, package, staging, production]`. | Yes — automated |
| AC-02 | The generated YAML passes `yaml.safe_load()` without raising an exception for all 6 language fixtures. | Yes — automated |
| AC-03 | The generated `.gitlab-ci.yml` for the Python fixture contains a `test` job with `artifacts.reports.junit: report.xml` and `--junitxml=report.xml` in its script. | Yes — automated |
| AC-04 | The generated `.gitlab-ci.yml` for the Python/pip fixture contains a `cache.key.files: [requirements.txt]` block. | Yes — automated |
| AC-05 | When `GITLAB_TOKEN` is set and `--project-id` is provided, `lint_gitlab_yaml()` posts to the project-scoped endpoint and not the anonymous endpoint. | Yes — mock test |
| AC-06 | When the GitLab Lint API returns `"status": "invalid"`, `tag ci gen-pipeline` prints each error, does not write the output file, and exits with code 1. | Yes — automated |
| AC-07 | When `--force` is set and the lint status is `"invalid"`, the file is written anyway and exit code is 0. | Yes — automated |
| AC-08 | `tag ci gen-pipeline --detect` in a repo whose `origin` remote is `git@gitlab.com:org/repo.git` selects `platform=gitlab` automatically. | Yes — automated |
| AC-09 | `tag ci gen-pipeline --detect` in a repo whose `origin` remote is `https://github.com/org/repo` selects `platform=github` automatically. | Yes — automated |
| AC-10 | Running `gen_pipeline()` twice with identical `DetectedStack` and `PipelineGenConfig` produces identical `yaml_content` byte-for-byte (except the header comment timestamp). | Yes — automated |
| AC-11 | The `ci_pipeline_generations` table receives exactly one new row per successful `gen_pipeline()` call, including `--dry-run` calls. | Yes — automated |
| AC-12 | `tag ci gen-pipeline --history` outputs the correct row for the most recent generation in the current repo. | Yes — automated |
| AC-13 | `tag ci gen-pipeline --platform gitlab --dry-run` completes in < 3 seconds for the largest monorepo fixture (1,000 files). | Yes — performance test |
| AC-14 | The generated YAML does not contain any string matching the regex `r'glpat-[0-9a-zA-Z_-]{20}'` even if `GITLAB_TOKEN` is set to such a value. | Yes — automated |
| AC-15 | `tag ci gen-pipeline --platform gitlab --mono` in the `monorepo-python-node` fixture produces two test jobs each with `rules: changes:` pointing to their respective subdirectories. | Yes — automated |
| AC-16 | `tag ci gen-pipeline --validate --platform gitlab --file .gitlab-ci.yml` correctly reports `valid` for a known-good fixture file and `invalid` for a file with a deliberate syntax error. | Yes — automated |
| AC-17 | `tag ci gen-pipeline --platform github --output .github/workflows/ci.yml` creates the `.github/workflows/` directory if it does not exist and writes the workflow file. | Yes — automated |
| AC-18 | When `GITLAB_TOKEN` is not set and `--gitlab-url` points to a non-routable address, the lint step is skipped with `"lint_status": "skipped"` in `--json` output and the file is still written. | Yes — automated |

---

## 14. Dependencies

| Dependency | Type | Justification | Fallback |
|------------|------|---------------|----------|
| `yaml` (PyYAML) | Already present | Render and parse generated YAML, read pyproject.toml | None — already required by TAG |
| `urllib.request` (stdlib) | Stdlib | GitLab CI Lint API HTTP call | N/A |
| `subprocess` (stdlib) | Stdlib | `git remote get-url origin`, `actionlint` invocation | N/A |
| `string.Template` (stdlib) | Stdlib | Pipeline template rendering | N/A |
| `jsonschema` | Optional | GitHub Actions JSON Schema validation | Skip schema validation; warn user |
| `actionlint` | Optional binary | GitHub Actions semantic validation | Fall back to `jsonschema` |
| `tracing.py` | Internal | OTel span emission (PRD-013) | `contextlib.nullcontext()` stub |
| `security.py` | Internal | Secret pattern scan before file write (PRD-034) | Skip scan; emit warning |
| PRD-013 (Tracing) | Internal PRD | OTel span structure for `ci.*` spans | No OTel if tracer unavailable |
| PRD-034 (Secret Scanning) | Internal PRD | `scan_text()` function used in FR-14 / Security §1 | Skip scan with warning |
| PRD-016 (Webhook Triggers) | Internal PRD | Future: trigger `gen-pipeline` on repo creation webhook | N/A for this PRD |
| `GITLAB_TOKEN` env var | Environment | Authenticated GitLab Lint API calls | Anonymous lint (less accurate) |

---

## 15. Open Questions

| ID | Question | Owner | Deadline |
|----|----------|-------|----------|
| OQ-01 | Should `gen-pipeline` support updating an existing `.gitlab-ci.yml` (merge/diff mode) rather than only generating from scratch? This would make it useful for repos that already have a partial pipeline. Requires a YAML-merge algorithm. | CI team | Before v1 implementation start |
| OQ-02 | Should the GitLab Lint API call be cached per `(yaml_content_hash, project_id)` in SQLite to avoid repeated API calls during development iterations? Cache TTL would be 5 minutes. | Platform team | v1 design review |
| OQ-03 | Should template variants be user-extensible via YAML files in `~/.tag/ci-templates/`? This would allow orgs to maintain custom pipeline patterns. Requires a template discovery and validation layer. | Architecture | v2 planning |
| OQ-04 | Is `string.Template` sufficient for complex conditional job generation, or should we use Jinja2 (adding a new optional dependency)? Jinja2 supports `{% if %}` blocks cleanly; `string.Template` requires conditional construction in Python code. | CI team | Before implementation |
| OQ-05 | For `--mono` mode, what is the correct behaviour when a monorepo component's path changes or is deleted? Should the generated pipeline be idempotent w.r.t. component order? | CI team | v1 design review |
| OQ-06 | Should `tag ci gen-pipeline --platform github` also generate a `copilot-setup-steps.yml` file per PRD cluster research (cluster research item 7)? The constraints (exactly one job named `copilot-setup-steps`, Ubuntu/Windows x64 only, default branch) are well-defined. | Feature owner | v1 scope decision |
| OQ-07 | Should the command support a `--watch` mode that regenerates the pipeline whenever a language marker file changes (e.g., new dependencies added to `requirements.txt`)? Would use `watchdog` or `inotify`. | UX team | v2 planning |
| OQ-08 | GitLab CI Lint API v4 requires the `content` field; the newer `dry_run` field requires GitLab 15.1+. Should we detect the GitLab version before sending `dry_run: false`? Or is 15.1+ a safe baseline assumption? | CI team | Before API integration |
| OQ-09 | Should lint errors be correlated to line numbers in the rendered YAML (via difflib or a line-tracking template renderer) to produce `Line N: <error>` output as specified in FR-12? GitLab Lint API errors often include human-readable job-name context but not line numbers. | CI team | v1 design review |
| OQ-10 | Should the `ci_pipeline_generations` table store the full generated YAML content (for replay/diff) or only metadata? Full YAML storage enables `tag ci gen-pipeline --history --show <id>` but adds storage overhead. | Architecture | v1 schema freeze |

---

## 16. Complexity and Timeline

**Total estimated effort: 8–12 engineering days (M)**

### Phase 1: Detection Engine (Days 1–3)

- Implement `DetectedStack`, `ComponentStack`, `PipelineGenConfig`, `PipelineGenResult`, `LintResult` dataclasses in `ci.py`.
- Implement `detect_stack()` with all language, package manager, test runner, containerization, and deploy target rules.
- Implement `_detect_language_version()` for Python, Node.js, Go.
- Implement `detect_platform_from_remote()` for `--detect` mode.
- Write `TestDetectStack` unit tests with fixture directories covering all 8 languages.
- Write performance test asserting `detect_stack()` < 1 second on a 5000-file tree.

**Deliverable:** `detect_stack()` returning a correct `DetectedStack` for all fixture repos; 90%+ test coverage on detection code.

### Phase 2: Template Rendering (Days 4–6)

- Implement `render_gitlab_pipeline()` with `minimal`, `auto-devops`, and `security-scan` template variants.
- Implement `render_github_workflow()` with `standard` and `release` variants.
- Add YAML header comment with TAG version and timestamp.
- Implement `--mono` mode: recursive detection + per-component job blocks with `rules: changes:`.
- Write `TestRenderGitlabPipeline` and `TestRenderGithubWorkflow` unit tests.
- Assert generated YAML parses cleanly with `yaml.safe_load()`.
- Assert determinism (two calls produce identical output modulo timestamp header).

**Deliverable:** Template rendering for all variants producing parseable, deterministic YAML.

### Phase 3: Validation and Integration (Days 7–9)

- Implement `lint_gitlab_yaml()` with unauthenticated and authenticated endpoints, timeout handling, and graceful offline fallback.
- Implement `validate_github_yaml()` with `actionlint` probe and `jsonschema` fallback.
- Implement `_write_pipeline_file()` with path resolution, `--force` prompt, directory creation, and `security.py` scan gate.
- Implement `gen_pipeline()` orchestration function with OTel spans.
- Add `ci_pipeline_generations` table DDL to `controller.py:_init_db()`.
- Implement `_persist_generation()`.
- Write mock-based `TestLintGitlabYaml` tests.
- Write `TestPersistGeneration` tests.

**Deliverable:** End-to-end `gen_pipeline()` working locally with lint pass for Python fixture.

### Phase 4: CLI Wiring and Polish (Days 10–11)

- Register `tag ci gen-pipeline` subparser in `controller.py`.
- Implement `cmd_ci_gen_pipeline()` handler covering all subcommand paths (`--detect`, `--history`, `--validate`, default generation).
- Implement `--json` output serialization.
- Implement `--history` and `--validate` subcommand display logic.
- Integration test against all 6 language fixtures.
- End-to-end test: generate, validate, write, read history.
- Update `docs/prd/INDEX.md` to reference PRD-062.

**Deliverable:** `tag ci gen-pipeline` fully functional CLI with all flags.

### Phase 5: Security Review and Hardening (Day 12)

- Audit generated YAML for secret leakage paths (FR-14, Security §1).
- Confirm SSRF guard on `--gitlab-url` (Security §5).
- Confirm YAML injection guard on `--test-cmd` / `--build-cmd` (Security §6).
- Run `tag secret-scan` on test fixture outputs.
- Final test run: confirm >= 90% line coverage in `test_ci_gen_pipeline.py`.
- Confirm all 18 acceptance criteria pass.

**Deliverable:** PRD-062 ready for merge; all AC pass; security checklist signed off.

