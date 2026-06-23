"""CI/CD integration helpers for TAG (PRD-020, PRD-057–PRD-063).

Provides utilities for interacting with GitHub PRs via the ``gh`` CLI,
reading CI log files, building LLM prompts for code-review and failure
diagnosis, detecting the current git host, generating tests, scaffolding
GitHub Actions, parsing SARIF vulnerability reports, root-cause analysis,
signal-scoped PR reviews, GitLab CI pipeline generation, and flaky-test
detection and remediation.
"""

from __future__ import annotations

import json
import os
import re
import subprocess
import textwrap
from pathlib import Path


# ---------------------------------------------------------------------------
# GitHub PR helpers
# ---------------------------------------------------------------------------


def fetch_pr_diff(repo: str, pr_number: int) -> str:
    """Return the unified diff for *pr_number* in *repo*.

    Parameters
    ----------
    repo:
        GitHub repository in ``owner/name`` format.
    pr_number:
        Pull-request number.

    Returns
    -------
    str
        Raw diff text.

    Raises
    ------
    RuntimeError
        If ``gh pr diff`` exits with a non-zero status.
    """
    result = subprocess.run(
        ["gh", "pr", "diff", str(pr_number), "--repo", repo],
        capture_output=True,
        text=True,
    )
    if result.returncode != 0:
        raise RuntimeError(
            f"gh pr diff failed for {repo}#{pr_number}: {result.stderr.strip()}"
        )
    return result.stdout


def fetch_pr_metadata(repo: str, pr_number: int) -> dict:
    """Return a dict of metadata for *pr_number* in *repo*.

    Fetches common fields: ``title``, ``body``, ``author``, ``state``,
    ``baseRefName``, ``headRefName``, ``labels``, ``reviews``,
    ``reviewRequests``, ``files``.

    Parameters
    ----------
    repo:
        GitHub repository in ``owner/name`` format.
    pr_number:
        Pull-request number.

    Returns
    -------
    dict
        Parsed JSON response from ``gh pr view --json``.

    Raises
    ------
    RuntimeError
        If ``gh pr view`` fails.
    ValueError
        If the output cannot be parsed as JSON.
    """
    fields = ",".join(
        [
            "title",
            "body",
            "author",
            "state",
            "baseRefName",
            "headRefName",
            "labels",
            "reviews",
            "reviewRequests",
            "files",
        ]
    )
    result = subprocess.run(
        ["gh", "pr", "view", str(pr_number), "--repo", repo, "--json", fields],
        capture_output=True,
        text=True,
    )
    if result.returncode != 0:
        raise RuntimeError(
            f"gh pr view failed for {repo}#{pr_number}: {result.stderr.strip()}"
        )
    try:
        return json.loads(result.stdout)
    except json.JSONDecodeError as exc:
        raise ValueError(
            f"Could not parse JSON from gh pr view: {exc}"
        ) from exc


def post_pr_comment(repo: str, pr_number: int, body: str) -> bool:
    """Post a top-level comment on a pull request.

    Parameters
    ----------
    repo:
        GitHub repository in ``owner/name`` format.
    pr_number:
        Pull-request number.
    body:
        Markdown comment body.

    Returns
    -------
    bool
        ``True`` if the comment was posted successfully, ``False`` otherwise.
    """
    result = subprocess.run(
        [
            "gh",
            "pr",
            "comment",
            str(pr_number),
            "--repo",
            repo,
            "--body",
            body,
        ],
        capture_output=True,
        text=True,
    )
    return result.returncode == 0


def post_pr_review_comments(
    repo: str, pr_number: int, comments: list[dict]
) -> bool:
    """Post inline review comments on a pull request via the GitHub API.

    Each entry in *comments* should be a dict with at minimum the keys
    expected by the GitHub ``POST /repos/{owner}/{repo}/pulls/{pull_number}/reviews``
    endpoint, e.g.::

        {
            "path": "src/foo.py",
            "position": 5,          # or "line" for multi-line diff
            "body": "Consider using a list comprehension here.",
        }

    All comments are submitted as a single PENDING review that is
    immediately submitted with event ``COMMENT``.

    Parameters
    ----------
    repo:
        GitHub repository in ``owner/name`` format, e.g. ``"owner/repo"``.
    pr_number:
        Pull-request number.
    comments:
        List of inline comment dicts.

    Returns
    -------
    bool
        ``True`` if the API call succeeded, ``False`` otherwise.
    """
    owner, name = repo.split("/", 1)
    payload = json.dumps(
        {
            "event": "COMMENT",
            "comments": comments,
        }
    )
    result = subprocess.run(
        [
            "gh",
            "api",
            f"repos/{owner}/{name}/pulls/{pr_number}/reviews",
            "--method",
            "POST",
            "--input",
            "-",
        ],
        input=payload,
        capture_output=True,
        text=True,
    )
    return result.returncode == 0


# ---------------------------------------------------------------------------
# CI log helpers
# ---------------------------------------------------------------------------


def read_ci_log(log_path: Path) -> str:
    """Read a CI log file, truncating to the last 100 lines when it is large.

    If the file contains more than 200 lines only the final 100 lines are
    returned, prefixed with a notice so consumers know lines were omitted.

    Parameters
    ----------
    log_path:
        Absolute or relative path to the log file.

    Returns
    -------
    str
        Log content (possibly truncated).

    Raises
    ------
    FileNotFoundError
        If *log_path* does not exist.
    """
    log_path = Path(log_path)
    text = log_path.read_text(encoding="utf-8", errors="replace")
    lines = text.splitlines()
    if len(lines) > 200:
        kept = lines[-100:]
        omitted = len(lines) - 100
        header = f"[... {omitted} earlier lines omitted ...]\n"
        return header + "\n".join(kept)
    return text


# ---------------------------------------------------------------------------
# Prompt builders
# ---------------------------------------------------------------------------

_REVIEW_SYSTEM = textwrap.dedent(
    """\
    You are an expert code reviewer. Review the following pull request diff and
    metadata carefully. Provide:
    1. A concise summary of the changes.
    2. Potential bugs or correctness issues (if any).
    3. Style, maintainability, and performance suggestions.
    4. An overall recommendation: APPROVE, REQUEST_CHANGES, or COMMENT.

    Be constructive, precise, and reference specific line numbers where possible.
    """
)

_DIAGNOSE_SYSTEM = textwrap.dedent(
    """\
    You are an expert DevOps engineer performing root-cause analysis on a
    failing CI/CD pipeline. Analyse the log below and provide:
    1. The primary error or failure reason.
    2. The most likely root cause.
    3. Concrete remediation steps the developer should take.

    Be concise and actionable.
    """
)


def build_review_prompt(
    diff: str,
    metadata: dict,
    max_diff_chars: int = 8000,
) -> str:
    """Build an LLM prompt for reviewing a pull request.

    Parameters
    ----------
    diff:
        Unified diff text (will be truncated to *max_diff_chars* if needed).
    metadata:
        PR metadata dict as returned by :func:`fetch_pr_metadata`.
    max_diff_chars:
        Maximum number of characters to include from the diff.

    Returns
    -------
    str
        Fully formatted prompt string.
    """
    if len(diff) > max_diff_chars:
        diff = diff[:max_diff_chars] + f"\n\n[... diff truncated at {max_diff_chars} chars ...]"

    title = metadata.get("title", "(no title)")
    body = metadata.get("body") or "(no description)"
    author = metadata.get("author", {})
    author_login = author.get("login", "unknown") if isinstance(author, dict) else str(author)
    base = metadata.get("baseRefName", "")
    head = metadata.get("headRefName", "")
    labels = ", ".join(
        lbl.get("name", "") if isinstance(lbl, dict) else str(lbl)
        for lbl in metadata.get("labels", [])
    ) or "(none)"

    prompt = textwrap.dedent(
        f"""\
        {_REVIEW_SYSTEM}

        ## Pull Request Metadata

        - **Title**: {title}
        - **Author**: {author_login}
        - **Base → Head**: {base} → {head}
        - **Labels**: {labels}

        ### Description

        {body}

        ## Diff

        ```diff
        {diff}
        ```
        """
    )
    return prompt


def build_diagnose_prompt(log_content: str) -> str:
    """Build an LLM prompt for diagnosing a CI/CD failure.

    Parameters
    ----------
    log_content:
        Raw (possibly pre-truncated) CI log text.

    Returns
    -------
    str
        Fully formatted prompt string.
    """
    prompt = textwrap.dedent(
        f"""\
        {_DIAGNOSE_SYSTEM}

        ## CI/CD Log

        ```
        {log_content}
        ```
        """
    )
    return prompt


# ---------------------------------------------------------------------------
# Git / host detection
# ---------------------------------------------------------------------------


def detect_git_host() -> str:
    """Detect the git hosting provider from the current repository's remote URL.

    Inspects the ``origin`` remote URL (falls back to the first remote found).

    Returns
    -------
    str
        One of ``'github'``, ``'gitlab'``, or ``'local'``.
    """
    result = subprocess.run(
        ["git", "remote", "-v"],
        capture_output=True,
        text=True,
    )
    if result.returncode != 0 or not result.stdout.strip():
        return "local"

    url_lower = result.stdout.lower()
    if "github.com" in url_lower:
        return "github"
    if "gitlab.com" in url_lower or "gitlab." in url_lower:
        return "gitlab"
    return "local"


def get_staged_diff() -> str:
    """Return the unified diff of staged (index) changes.

    Returns
    -------
    str
        Output of ``git diff --staged``, or an empty string if there are no
        staged changes or git is unavailable.
    """
    result = subprocess.run(
        ["git", "diff", "--staged"],
        capture_output=True,
        text=True,
    )
    if result.returncode != 0:
        return ""
    return result.stdout


# ---------------------------------------------------------------------------
# PRD-057: Automated test generation
# ---------------------------------------------------------------------------

_TEST_GEN_SYSTEM = textwrap.dedent(
    """\
    You are an expert software engineer specialising in test-driven development.
    Given a code diff, generate a comprehensive test suite that covers:
    1. The happy path for every new or modified function.
    2. Edge cases: empty inputs, boundary values, type errors.
    3. Error / exception paths.

    Output ONLY runnable test code — no prose, no markdown fences.
    """
)

# Mapping of framework to file-glob patterns used for detection.
_FRAMEWORK_MARKERS: dict[str, list[str]] = {
    "pytest": ["pytest.ini", "pyproject.toml", "setup.cfg", "conftest.py", "tox.ini"],
    "jest": ["jest.config.js", "jest.config.ts", "jest.config.mjs", "jest.config.cjs"],
    "mocha": [".mocharc.yml", ".mocharc.js", ".mocharc.json", "mocha.opts"],
    "cargo": ["Cargo.toml"],
    "go-test": ["go.mod"],
    "rspec": [".rspec", "spec/spec_helper.rb", "Gemfile"],
}


def detect_test_framework(repo_root: Path | None = None) -> str:
    """Detect the test framework used in *repo_root*.

    Walks up from *repo_root* (defaults to the current working directory) and
    checks for well-known configuration files.  Returns the first match among
    ``pytest``, ``jest``, ``mocha``, ``cargo``, ``go-test``, or ``rspec``.
    Falls back to ``"pytest"`` when nothing is found.

    Parameters
    ----------
    repo_root:
        Repository root to inspect.  Defaults to ``Path.cwd()``.

    Returns
    -------
    str
        Framework name.
    """
    root = Path(repo_root) if repo_root else Path.cwd()

    # Walk from root, checking marker files.
    for framework, markers in _FRAMEWORK_MARKERS.items():
        for marker in markers:
            candidate = root / marker
            if candidate.exists():
                # For pyproject.toml, confirm [tool.pytest.ini_options] or pytest dep.
                if marker == "pyproject.toml":
                    try:
                        content = candidate.read_text(encoding="utf-8", errors="replace")
                        if "pytest" in content:
                            return "pytest"
                        # Might be a Rust project with Cargo.toml sibling.
                        continue
                    except OSError:
                        continue
                return framework

    # Fallback: inspect package.json for jest/mocha scripts.
    pkg = root / "package.json"
    if pkg.exists():
        try:
            data = json.loads(pkg.read_text(encoding="utf-8"))
            scripts = data.get("scripts", {})
            devdeps = {**data.get("devDependencies", {}), **data.get("dependencies", {})}
            if "jest" in devdeps or "jest" in str(scripts):
                return "jest"
            if "mocha" in devdeps or "mocha" in str(scripts):
                return "mocha"
        except (json.JSONDecodeError, OSError):
            pass

    return "pytest"


def generate_tests(
    diff: str,
    profile: str,
    cfg: dict,
    *,
    framework: str = "pytest",
    output_path: Path | None = None,
) -> str:
    """Build a prompt and invoke the TAG runtime to generate tests for changed code.

    Parameters
    ----------
    diff:
        Unified diff of the code changes to generate tests for.
    profile:
        TAG profile name to use for the LLM invocation.
    cfg:
        TAG configuration dict (as returned by ``load_config``).
    framework:
        Target test framework.  Defaults to ``"pytest"``.
    output_path:
        If provided, the generated test code is written to this path.

    Returns
    -------
    str
        Generated test code as a string.

    Raises
    ------
    RuntimeError
        If the TAG runtime invocation fails.
    """
    # Import lazily to avoid circular imports at module level.
    from tag.controller import hermes_bin, profile_exec_env  # type: ignore[import]

    prompt = textwrap.dedent(
        f"""\
        {_TEST_GEN_SYSTEM}

        ## Target Framework

        {framework}

        ## Code Diff

        ```diff
        {diff[:8000]}
        ```

        Generate tests now.
        """
    )

    result = subprocess.run(
        [str(hermes_bin(cfg)), "chat", "-q", prompt, "-Q"],
        env=profile_exec_env(cfg, profile),
        capture_output=True,
        text=True,
    )
    if result.returncode != 0:
        raise RuntimeError(
            f"TAG runtime failed during test generation: {result.stderr.strip()}"
        )

    generated = result.stdout.strip()

    if output_path is not None:
        output_path = Path(output_path)
        output_path.parent.mkdir(parents=True, exist_ok=True)
        output_path.write_text(generated, encoding="utf-8")

    return generated


# ---------------------------------------------------------------------------
# PRD-058: GitHub Actions workflow scaffold
# ---------------------------------------------------------------------------

_GH_ACTION_TEMPLATES: dict[str, str] = {
    "eval": textwrap.dedent(
        """\
        name: TAG Eval

        on:
          push:
            branches: ["main", "master"]
          pull_request:
            branches: ["main", "master"]
          workflow_dispatch:

        permissions:
          contents: read

        jobs:
          tag-eval:
            runs-on: ubuntu-latest
            steps:
              - name: Checkout
                uses: actions/checkout@v4

              - name: Set up Python
                uses: actions/setup-python@v5
                with:
                  python-version: "3.12"

              - name: Install tag-agent
                run: pip install tag-agent

              - name: Run TAG eval
                env:
                  TAG_PROFILE: {profile}
                  TAG_THRESHOLD: "{threshold}"
                run: |
                  tag eval --profile "$TAG_PROFILE" --threshold "$TAG_THRESHOLD"
        """
    ),
    "review": textwrap.dedent(
        """\
        name: TAG PR Review

        on:
          pull_request:
            types: [opened, synchronize, reopened]

        permissions:
          contents: read
          pull-requests: write

        jobs:
          tag-review:
            runs-on: ubuntu-latest
            steps:
              - name: Checkout
                uses: actions/checkout@v4

              - name: Set up Python
                uses: actions/setup-python@v5
                with:
                  python-version: "3.12"

              - name: Install tag-agent
                run: pip install tag-agent

              - name: Run TAG PR review
                env:
                  GH_TOKEN: ${{{{ secrets.GITHUB_TOKEN }}}}
                  TAG_PROFILE: {profile}
                run: |
                  tag ci review \\
                    --repo "${{{{ github.repository }}}}" \\
                    --pr "${{{{ github.event.pull_request.number }}}}" \\
                    --profile "$TAG_PROFILE" \\
                    --post-comment
        """
    ),
    "test-gen": textwrap.dedent(
        """\
        name: TAG Test Generation

        on:
          pull_request:
            types: [opened, synchronize, reopened]

        permissions:
          contents: write
          pull-requests: write

        jobs:
          tag-test-gen:
            runs-on: ubuntu-latest
            steps:
              - name: Checkout
                uses: actions/checkout@v4
                with:
                  fetch-depth: 0

              - name: Set up Python
                uses: actions/setup-python@v5
                with:
                  python-version: "3.12"

              - name: Install tag-agent
                run: pip install tag-agent

              - name: Generate tests for PR diff
                env:
                  GH_TOKEN: ${{{{ secrets.GITHUB_TOKEN }}}}
                  TAG_PROFILE: {profile}
                run: |
                  tag ci test-gen \\
                    --repo "${{{{ github.repository }}}}" \\
                    --pr "${{{{ github.event.pull_request.number }}}}" \\
                    --profile "$TAG_PROFILE"
        """
    ),
    "fix-vuln": textwrap.dedent(
        """\
        name: TAG Vulnerability Auto-Fix

        on:
          workflow_dispatch:
            inputs:
              sarif_path:
                description: "Path to SARIF file"
                required: false
                default: "results.sarif"
          schedule:
            - cron: "0 3 * * 1"

        permissions:
          contents: write
          pull-requests: write
          security-events: read

        jobs:
          tag-fix-vuln:
            runs-on: ubuntu-latest
            steps:
              - name: Checkout
                uses: actions/checkout@v4

              - name: Set up Python
                uses: actions/setup-python@v5
                with:
                  python-version: "3.12"

              - name: Install tag-agent
                run: pip install tag-agent

              - name: Fix vulnerabilities from SARIF
                env:
                  GH_TOKEN: ${{{{ secrets.GITHUB_TOKEN }}}}
                  TAG_PROFILE: {profile}
                run: |
                  SARIF="${{{{ github.event.inputs.sarif_path || 'results.sarif' }}}}"
                  tag ci fix-vuln \\
                    --sarif "$SARIF" \\
                    --profile "$TAG_PROFILE" \\
                    --auto-commit \\
                    --create-pr
        """
    ),
}


def scaffold_github_action(
    workflow_type: str,
    *,
    profile: str = "reviewer",
    threshold: float = 0.85,
) -> str:
    """Return YAML content for a ``.github/workflows/tag-<type>.yml`` file.

    Parameters
    ----------
    workflow_type:
        One of ``"eval"``, ``"review"``, ``"test-gen"``, or ``"fix-vuln"``.
    profile:
        TAG profile name to embed in the workflow.
    threshold:
        Numeric quality threshold (used by the ``"eval"`` workflow).

    Returns
    -------
    str
        Complete GitHub Actions YAML content.

    Raises
    ------
    ValueError
        If *workflow_type* is not recognised.
    """
    if workflow_type not in _GH_ACTION_TEMPLATES:
        valid = ", ".join(sorted(_GH_ACTION_TEMPLATES))
        raise ValueError(
            f"Unknown workflow_type {workflow_type!r}. Valid types: {valid}"
        )
    template = _GH_ACTION_TEMPLATES[workflow_type]
    return template.format(profile=profile, threshold=threshold)


def install_github_action(
    workflow_type: str,
    output_dir: Path | None = None,
) -> Path:
    """Write the scaffolded workflow to ``.github/workflows/`` and return the path.

    Parameters
    ----------
    workflow_type:
        Workflow type passed to :func:`scaffold_github_action`.
    output_dir:
        Directory to write the file into.  Defaults to
        ``.github/workflows/`` under the current working directory.

    Returns
    -------
    Path
        Absolute path to the written YAML file.
    """
    if output_dir is None:
        output_dir = Path.cwd() / ".github" / "workflows"
    output_dir = Path(output_dir)
    output_dir.mkdir(parents=True, exist_ok=True)

    content = scaffold_github_action(workflow_type)
    dest = output_dir / f"tag-{workflow_type}.yml"
    dest.write_text(content, encoding="utf-8")
    return dest.resolve()


# ---------------------------------------------------------------------------
# PRD-059: SAST vulnerability auto-remediation
# ---------------------------------------------------------------------------

def parse_sarif(sarif_path: Path) -> list[dict]:
    """Parse a SARIF file and return a flat list of findings.

    Each item in the returned list is a dict with the keys:

    ``rule_id``
        SARIF rule identifier string.
    ``message``
        Human-readable finding message.
    ``path``
        Source file path relative to the repository root.
    ``start_line``
        1-based line number where the issue starts (``0`` if unknown).
    ``severity``
        Normalised severity string: ``"error"``, ``"warning"``, ``"note"``,
        or ``"none"``.

    Parameters
    ----------
    sarif_path:
        Path to the SARIF JSON file.

    Returns
    -------
    list[dict]
        List of vulnerability dicts.

    Raises
    ------
    FileNotFoundError
        If *sarif_path* does not exist.
    ValueError
        If the file cannot be parsed as valid SARIF JSON.
    """
    sarif_path = Path(sarif_path)
    if not sarif_path.exists():
        raise FileNotFoundError(f"SARIF file not found: {sarif_path}")

    try:
        data = json.loads(sarif_path.read_text(encoding="utf-8", errors="replace"))
    except json.JSONDecodeError as exc:
        raise ValueError(f"Cannot parse SARIF file {sarif_path}: {exc}") from exc

    findings: list[dict] = []
    for run in data.get("runs", []):
        # Build rule-id → severity lookup from the run's tool rules.
        rule_severity: dict[str, str] = {}
        rules = (
            run.get("tool", {}).get("driver", {}).get("rules", [])
            or run.get("tool", {}).get("extensions", [{}])[0].get("rules", [])
        )
        for rule in rules:
            rid = rule.get("id", "")
            level = (
                rule.get("defaultConfiguration", {}).get("level", "warning")
            )
            rule_severity[rid] = level

        for result in run.get("results", []):
            rule_id = result.get("ruleId", result.get("rule", {}).get("id", "unknown"))
            message = (
                result.get("message", {}).get("text", "")
                or result.get("message", {}).get("markdown", "")
            )
            severity = result.get("level") or rule_severity.get(rule_id, "warning")

            for location in result.get("locations", [{}]):
                physical = location.get("physicalLocation", {})
                artifact = physical.get("artifactLocation", {})
                region = physical.get("region", {})
                path = artifact.get("uri", "")
                # Strip common URI prefixes.
                if path.startswith("file:///"):
                    path = path[8:]
                elif path.startswith("file://"):
                    path = path[7:]
                start_line = region.get("startLine", 0)

                findings.append(
                    {
                        "rule_id": rule_id,
                        "message": message,
                        "path": path,
                        "start_line": int(start_line),
                        "severity": severity,
                    }
                )

            # Result with no locations still gets an entry.
            if not result.get("locations"):
                findings.append(
                    {
                        "rule_id": rule_id,
                        "message": message,
                        "path": "",
                        "start_line": 0,
                        "severity": severity,
                    }
                )

    return findings


def build_vuln_fix_prompt(vuln: dict, file_content: str) -> str:
    """Build an LLM prompt to fix a specific vulnerability.

    Parameters
    ----------
    vuln:
        Vulnerability dict as returned by :func:`parse_sarif`.
    file_content:
        Full source text of the affected file.

    Returns
    -------
    str
        Formatted prompt string.
    """
    rule_id = vuln.get("rule_id", "unknown")
    message = vuln.get("message", "")
    path = vuln.get("path", "unknown file")
    start_line = vuln.get("start_line", 0)
    severity = vuln.get("severity", "warning")

    context_lines = file_content.splitlines()
    lo = max(0, start_line - 10)
    hi = min(len(context_lines), start_line + 10)
    snippet = "\n".join(
        f"{i + 1 + lo}: {line}" for i, line in enumerate(context_lines[lo:hi])
    )

    return textwrap.dedent(
        f"""\
        You are a security engineer performing automated vulnerability remediation.

        ## Vulnerability Details

        - **Rule**: {rule_id}
        - **Severity**: {severity}
        - **File**: {path}
        - **Line**: {start_line}
        - **Finding**: {message}

        ## Relevant Code (lines {lo + 1}–{hi})

        ```
        {snippet}
        ```

        ## Full File Content

        ```
        {file_content[:6000]}
        ```

        ## Task

        Provide a corrected version of the FULL file that fixes the vulnerability
        described above.  Output ONLY the complete corrected file content —
        no explanations, no markdown fences.
        """
    )


def fix_sarif_vulns(
    sarif_path: Path,
    profile: str,
    cfg: dict,
    *,
    auto_commit: bool = False,
    dry_run: bool = False,
) -> list[dict]:
    """Fix every vulnerability found in a SARIF file using the TAG runtime.

    For each finding the function:

    1. Reads the affected source file.
    2. Builds a fix prompt via :func:`build_vuln_fix_prompt`.
    3. Invokes the TAG runtime to produce corrected file content.
    4. Writes the corrected content back (unless *dry_run* is ``True``).
    5. Optionally commits the fix with ``git commit``.

    Parameters
    ----------
    sarif_path:
        Path to the SARIF file.
    profile:
        TAG profile name.
    cfg:
        TAG configuration dict.
    auto_commit:
        If ``True`` and not *dry_run*, commit each fixed file with git.
    dry_run:
        If ``True``, compute fixes but do not write files or commit.

    Returns
    -------
    list[dict]
        One dict per vulnerability with keys ``vuln``, ``fix_applied``
        (bool), and ``commit_sha`` (str or ``None``).
    """
    from tag.controller import hermes_bin, profile_exec_env  # type: ignore[import]

    vulns = parse_sarif(sarif_path)
    results: list[dict] = []

    for vuln in vulns:
        file_path = vuln.get("path", "")
        record: dict = {"vuln": vuln, "fix_applied": False, "commit_sha": None}

        if not file_path or not Path(file_path).exists():
            results.append(record)
            continue

        try:
            file_content = Path(file_path).read_text(encoding="utf-8", errors="replace")
        except OSError:
            results.append(record)
            continue

        prompt = build_vuln_fix_prompt(vuln, file_content)

        proc = subprocess.run(
            [str(hermes_bin(cfg)), "chat", "-q", prompt, "-Q"],
            env=profile_exec_env(cfg, profile),
            capture_output=True,
            text=True,
        )

        if proc.returncode != 0 or not proc.stdout.strip():
            results.append(record)
            continue

        fixed_content = proc.stdout.strip()

        if not dry_run:
            Path(file_path).write_text(fixed_content, encoding="utf-8")
            record["fix_applied"] = True

            if auto_commit:
                subprocess.run(["git", "add", file_path], capture_output=True)
                commit_result = subprocess.run(
                    [
                        "git",
                        "commit",
                        "-m",
                        f"fix({vuln['rule_id']}): auto-remediate {Path(file_path).name} "
                        f"line {vuln['start_line']}",
                    ],
                    capture_output=True,
                    text=True,
                )
                if commit_result.returncode == 0:
                    sha_proc = subprocess.run(
                        ["git", "rev-parse", "--short", "HEAD"],
                        capture_output=True,
                        text=True,
                    )
                    record["commit_sha"] = sha_proc.stdout.strip() or None
        else:
            # dry_run: record that we *would* apply the fix.
            record["fix_applied"] = True

        results.append(record)

    return results


# ---------------------------------------------------------------------------
# PRD-060: CI failure root-cause + auto-fix
# ---------------------------------------------------------------------------

# Ordered list of (pattern, group_map) for extracting structured data from
# common CI failure log formats.
_CI_FAILURE_PATTERNS: list[tuple[re.Pattern, dict]] = [
    # Python traceback — captures file and line.
    (
        re.compile(
            r'File "(?P<file_path>[^"]+)", line (?P<line_number>\d+)',
            re.MULTILINE,
        ),
        {"file_path": "file_path", "line_number": "line_number"},
    ),
    # pytest FAILED line — captures test name.
    (
        re.compile(r"FAILED (?P<test_name>\S+) - (?P<error_message>.+)$", re.MULTILINE),
        {"test_name": "test_name", "error_message": "error_message"},
    ),
    # pytest ERROR line.
    (
        re.compile(r"ERROR (?P<test_name>\S+)", re.MULTILINE),
        {"test_name": "test_name"},
    ),
    # AssertionError / standard exception line.
    (
        re.compile(r"(?P<error_type>\w*Error|\w*Exception): (?P<error_message>.+)$", re.MULTILINE),
        {"error_type": "error_type", "error_message": "error_message"},
    ),
    # npm / Jest failure.
    (
        re.compile(r"● (?P<test_name>.+)$", re.MULTILINE),
        {"test_name": "test_name"},
    ),
    # Go test failure.
    (
        re.compile(r"--- FAIL: (?P<test_name>\S+)", re.MULTILINE),
        {"test_name": "test_name"},
    ),
    # Rust test failure.
    (
        re.compile(r"test (?P<test_name>\S+) \.\.\. FAILED", re.MULTILINE),
        {"test_name": "test_name"},
    ),
]


def parse_ci_failure(log_content: str) -> dict:
    """Extract structured data from a CI log string.

    Attempts to identify:

    - ``error_type``: exception class or generic error category.
    - ``error_message``: human-readable error description.
    - ``file_path``: source file implicated in the failure.
    - ``line_number``: line number (as int) within *file_path*.
    - ``test_name``: name of the failing test case or job step.

    Returns a dict with all five keys; unknown fields are ``None``.

    Parameters
    ----------
    log_content:
        Raw CI log text.

    Returns
    -------
    dict
        Structured failure information.
    """
    info: dict = {
        "error_type": None,
        "error_message": None,
        "file_path": None,
        "line_number": None,
        "test_name": None,
    }

    for pattern, group_map in _CI_FAILURE_PATTERNS:
        # Find *all* matches; prefer the last occurrence (closest to failure).
        matches = list(pattern.finditer(log_content))
        if not matches:
            continue
        m = matches[-1]
        for result_key, group_name in group_map.items():
            try:
                value = m.group(group_name)
                if value and info.get(result_key) is None:
                    info[result_key] = value
            except IndexError:
                pass

    if info["line_number"] is not None:
        try:
            info["line_number"] = int(info["line_number"])
        except (TypeError, ValueError):
            info["line_number"] = None

    # Derive a generic error_type when none was found.
    if info["error_type"] is None and info["error_message"]:
        info["error_type"] = "UnknownError"

    return info


def diagnose_and_fix(
    log_path: Path,
    profile: str,
    cfg: dict,
    *,
    auto_fix: bool = False,
    create_pr: bool = False,
    repo: str | None = None,
) -> dict:
    """Full root-cause analysis and optional auto-fix flow for a CI failure.

    Steps
    -----
    1. Read the log file and parse the failure with :func:`parse_ci_failure`.
    2. Build a diagnosis prompt and invoke the TAG runtime.
    3. If *auto_fix* is ``True``: attempt to apply the suggested edits and
       run the test suite.
    4. If *create_pr* is ``True`` and *repo* is provided: push fixes to a
       branch and open a GitHub PR via ``gh pr create``.

    Parameters
    ----------
    log_path:
        Path to the CI log file.
    profile:
        TAG profile name.
    cfg:
        TAG configuration dict.
    auto_fix:
        Attempt to apply LLM-suggested edits automatically.
    create_pr:
        Open a pull request with the applied fixes.
    repo:
        GitHub repository in ``owner/name`` format (required when
        *create_pr* is ``True``).

    Returns
    -------
    dict
        Keys: ``diagnosis`` (str), ``fix_applied`` (bool), ``pr_url`` (str
        or ``None``).
    """
    from tag.controller import hermes_bin, profile_exec_env  # type: ignore[import]

    log_content = read_ci_log(log_path)
    failure = parse_ci_failure(log_content)
    prompt = build_diagnose_prompt(log_content)

    proc = subprocess.run(
        [str(hermes_bin(cfg)), "chat", "-q", prompt, "-Q"],
        env=profile_exec_env(cfg, profile),
        capture_output=True,
        text=True,
    )
    diagnosis = proc.stdout.strip()

    result: dict = {"diagnosis": diagnosis, "fix_applied": False, "pr_url": None}

    if not auto_fix:
        return result

    # --- Build an auto-fix prompt that incorporates the diagnosis. ---
    file_path = failure.get("file_path")
    file_content = ""
    if file_path and Path(file_path).exists():
        try:
            file_content = Path(file_path).read_text(encoding="utf-8", errors="replace")
        except OSError:
            pass

    fix_prompt = textwrap.dedent(
        f"""\
        You are an expert software engineer fixing a CI failure.

        ## Diagnosis

        {diagnosis}

        ## Failure Details

        - Error type: {failure.get('error_type', 'unknown')}
        - Error message: {failure.get('error_message', 'unknown')}
        - File: {failure.get('file_path', 'unknown')}
        - Line: {failure.get('line_number', 'unknown')}
        - Test: {failure.get('test_name', 'unknown')}

        ## Affected File Content

        ```
        {file_content[:5000]}
        ```

        Output ONLY the corrected complete file content. No explanations.
        """
    )

    fix_proc = subprocess.run(
        [str(hermes_bin(cfg)), "chat", "-q", fix_prompt, "-Q"],
        env=profile_exec_env(cfg, profile),
        capture_output=True,
        text=True,
    )

    if fix_proc.returncode == 0 and fix_proc.stdout.strip() and file_path:
        fixed_content = fix_proc.stdout.strip()
        try:
            Path(file_path).write_text(fixed_content, encoding="utf-8")
            result["fix_applied"] = True
        except OSError:
            pass

    if not result["fix_applied"]:
        return result

    if create_pr and repo:
        branch_name = "tag/ci-fix-" + _short_hash(log_content)
        subprocess.run(["git", "checkout", "-b", branch_name], capture_output=True)
        if file_path:
            subprocess.run(["git", "add", file_path], capture_output=True)
        subprocess.run(
            ["git", "commit", "-m", "fix(ci): auto-fix CI failure via TAG"],
            capture_output=True,
        )
        subprocess.run(
            ["git", "push", "--set-upstream", "origin", branch_name],
            capture_output=True,
        )
        pr_result = subprocess.run(
            [
                "gh",
                "pr",
                "create",
                "--repo",
                repo,
                "--title",
                "fix(ci): auto-fix CI failure",
                "--body",
                f"## TAG Auto-Fix\n\n{diagnosis}\n\n---\n*Generated by tag-agent*",
                "--head",
                branch_name,
            ],
            capture_output=True,
            text=True,
        )
        if pr_result.returncode == 0:
            result["pr_url"] = pr_result.stdout.strip()

    return result


def _short_hash(text: str, length: int = 8) -> str:
    """Return a short hex hash of *text* for use in branch names."""
    import hashlib

    return hashlib.sha256(text.encode("utf-8", errors="replace")).hexdigest()[:length]


# ---------------------------------------------------------------------------
# PRD-061: Configurable PR review signal classes
# ---------------------------------------------------------------------------

SIGNAL_CLASSES: dict[str, str] = {
    "security": "security vulnerabilities, injection, auth issues",
    "correctness": "logic errors, off-by-one, null pointer, race conditions",
    "coverage": "untested code paths, missing edge cases",
    "style": "naming conventions, code style, readability",
    "performance": "N+1 queries, unnecessary allocations, blocking I/O",
    "documentation": "missing docstrings, unclear variable names",
}


def build_review_prompt_with_signals(
    diff: str,
    metadata: dict,
    signals: list[str],
    *,
    max_diff_chars: int = 8000,
) -> str:
    """Build a PR review prompt scoped to the requested signal classes.

    Like :func:`build_review_prompt` but restricts the review focus to
    specific concern areas described in :data:`SIGNAL_CLASSES`.

    Parameters
    ----------
    diff:
        Unified diff text.
    metadata:
        PR metadata dict as returned by :func:`fetch_pr_metadata`.
    signals:
        List of signal class keys from :data:`SIGNAL_CLASSES`.  Passing
        an empty list falls back to reviewing all signal classes.
    max_diff_chars:
        Maximum diff characters to include in the prompt.

    Returns
    -------
    str
        Fully formatted prompt string.

    Raises
    ------
    ValueError
        If any element of *signals* is not a key in :data:`SIGNAL_CLASSES`.
    """
    unknown = [s for s in signals if s not in SIGNAL_CLASSES]
    if unknown:
        valid = ", ".join(sorted(SIGNAL_CLASSES))
        raise ValueError(
            f"Unknown signal class(es): {unknown}. Valid: {valid}"
        )

    active_signals = signals if signals else list(SIGNAL_CLASSES)

    signal_lines = "\n".join(
        f"- **{s}**: {SIGNAL_CLASSES[s]}" for s in active_signals
    )

    if len(diff) > max_diff_chars:
        diff = diff[:max_diff_chars] + f"\n\n[... diff truncated at {max_diff_chars} chars ...]"

    title = metadata.get("title", "(no title)")
    body = metadata.get("body") or "(no description)"
    author = metadata.get("author", {})
    author_login = (
        author.get("login", "unknown") if isinstance(author, dict) else str(author)
    )
    base = metadata.get("baseRefName", "")
    head = metadata.get("headRefName", "")
    labels = ", ".join(
        lbl.get("name", "") if isinstance(lbl, dict) else str(lbl)
        for lbl in metadata.get("labels", [])
    ) or "(none)"

    return textwrap.dedent(
        f"""\
        You are an expert code reviewer. Review the following pull request diff
        focusing ONLY on the signal classes listed below.  For each signal class
        present in your findings, prefix the section with the class name in bold.
        Ignore concerns outside the listed signal classes entirely.

        ## Review Focus Areas

        {signal_lines}

        ## Pull Request Metadata

        - **Title**: {title}
        - **Author**: {author_login}
        - **Base → Head**: {base} → {head}
        - **Labels**: {labels}

        ### Description

        {body}

        ## Diff

        ```diff
        {diff}
        ```

        For each signal class above, list specific findings with file and line
        references.  If no issues are found for a class write "None".
        End with an overall recommendation: APPROVE, REQUEST_CHANGES, or COMMENT.
        """
    )


def review_pr_with_signals(
    repo: str,
    pr_number: int,
    profile: str,
    cfg: dict,
    signals: list[str],
    *,
    post_comment: bool = False,
) -> dict:
    """Run a full PR review scoped to specific signal classes.

    Parameters
    ----------
    repo:
        GitHub repository in ``owner/name`` format.
    pr_number:
        Pull-request number.
    profile:
        TAG profile name.
    cfg:
        TAG configuration dict.
    signals:
        List of signal class keys from :data:`SIGNAL_CLASSES`.
    post_comment:
        If ``True``, post the review as a PR comment via :func:`post_pr_comment`.

    Returns
    -------
    dict
        Keys: ``review_text`` (str), ``signals_found`` (list of str found in
        output), ``posted`` (bool).
    """
    from tag.controller import hermes_bin, profile_exec_env  # type: ignore[import]

    diff = fetch_pr_diff(repo, pr_number)
    metadata = fetch_pr_metadata(repo, pr_number)
    prompt = build_review_prompt_with_signals(diff, metadata, signals)

    proc = subprocess.run(
        [str(hermes_bin(cfg)), "chat", "-q", prompt, "-Q"],
        env=profile_exec_env(cfg, profile),
        capture_output=True,
        text=True,
    )
    review_text = proc.stdout.strip()

    # Detect which signal classes appear in the output.
    signals_found = [s for s in SIGNAL_CLASSES if s.lower() in review_text.lower()]

    posted = False
    if post_comment and review_text:
        active_labels = ", ".join(signals) if signals else "all"
        body = (
            f"## TAG Code Review (signals: {active_labels})\n\n"
            f"{review_text}\n\n---\n*Generated by tag-agent*"
        )
        posted = post_pr_comment(repo, pr_number, body)

    return {"review_text": review_text, "signals_found": signals_found, "posted": posted}


# ---------------------------------------------------------------------------
# PRD-062: GitLab CI pipeline auto-generation
# ---------------------------------------------------------------------------

# Maps detected stack identifiers to detection heuristics.
_STACK_DETECTION_RULES: list[tuple[str, list[str]]] = [
    ("python", ["pyproject.toml", "setup.py", "setup.cfg", "requirements.txt", "Pipfile"]),
    ("node", ["package.json", "yarn.lock", "pnpm-lock.yaml", "bun.lockb"]),
    ("rust", ["Cargo.toml"]),
    ("go", ["go.mod"]),
    ("java", ["pom.xml", "build.gradle", "build.gradle.kts", "gradlew"]),
    ("ruby", ["Gemfile", ".ruby-version"]),
    ("docker", ["Dockerfile", "docker-compose.yml", "docker-compose.yaml"]),
    ("k8s", ["k8s/", "kubernetes/", "helm/", "Chart.yaml"]),
]

# Per-stack GitLab CI job snippets.
_STACK_JOB_SNIPPETS: dict[str, str] = {
    "python": textwrap.dedent(
        """\
        python-lint:
          stage: test
          image: python:3.12-slim
          script:
            - pip install --upgrade pip
            - pip install -e ".[dev]" || pip install -r requirements.txt || true
            - python -m pytest --tb=short -q || true

        python-type-check:
          stage: test
          image: python:3.12-slim
          script:
            - pip install mypy || true
            - mypy . --ignore-missing-imports || true
        """
    ),
    "node": textwrap.dedent(
        """\
        node-install:
          stage: build
          image: node:20-alpine
          script:
            - npm ci || yarn install --frozen-lockfile || true
          cache:
            paths:
              - node_modules/

        node-test:
          stage: test
          image: node:20-alpine
          script:
            - npm test || yarn test || true
          needs: [node-install]
        """
    ),
    "rust": textwrap.dedent(
        """\
        rust-build:
          stage: build
          image: rust:latest
          script:
            - cargo build --release

        rust-test:
          stage: test
          image: rust:latest
          script:
            - cargo test
          needs: [rust-build]
        """
    ),
    "go": textwrap.dedent(
        """\
        go-build:
          stage: build
          image: golang:1.22-alpine
          script:
            - go build ./...

        go-test:
          stage: test
          image: golang:1.22-alpine
          script:
            - go test ./...
          needs: [go-build]
        """
    ),
    "java": textwrap.dedent(
        """\
        java-build:
          stage: build
          image: eclipse-temurin:21-jdk
          script:
            - ./mvnw package -DskipTests || ./gradlew build -x test || true

        java-test:
          stage: test
          image: eclipse-temurin:21-jdk
          script:
            - ./mvnw test || ./gradlew test || true
          needs: [java-build]
        """
    ),
    "ruby": textwrap.dedent(
        """\
        ruby-test:
          stage: test
          image: ruby:3.3-slim
          script:
            - bundle install
            - bundle exec rspec || true
        """
    ),
    "docker": textwrap.dedent(
        """\
        docker-build:
          stage: build
          image: docker:24
          services:
            - docker:24-dind
          variables:
            DOCKER_TLS_CERTDIR: "/certs"
          script:
            - docker build -t $CI_REGISTRY_IMAGE:$CI_COMMIT_SHORT_SHA .
            - docker push $CI_REGISTRY_IMAGE:$CI_COMMIT_SHORT_SHA || true
        """
    ),
    "k8s": textwrap.dedent(
        """\
        k8s-lint:
          stage: test
          image: bitnami/kubectl:latest
          script:
            - kubectl apply --dry-run=client -f k8s/ || kubectl apply --dry-run=client -f kubernetes/ || true
        """
    ),
}

_DEPLOY_SNIPPET = textwrap.dedent(
    """\
    deploy-staging:
      stage: deploy
      environment:
        name: staging
        url: https://staging.example.com
      script:
        - echo "Deploy to staging — customise this step."
      only:
        - main
        - master
      when: manual
    """
)


def detect_stack(repo_root: Path | None = None) -> list[str]:
    """Detect the technology stack present in *repo_root*.

    Inspects the file tree for well-known marker files and directories.

    Parameters
    ----------
    repo_root:
        Repository root directory.  Defaults to ``Path.cwd()``.

    Returns
    -------
    list[str]
        Ordered list of detected stack identifiers, e.g.
        ``["python", "docker"]``.
    """
    root = Path(repo_root) if repo_root else Path.cwd()
    detected: list[str] = []

    for stack, markers in _STACK_DETECTION_RULES:
        for marker in markers:
            # Support directory markers ending with "/".
            if marker.endswith("/"):
                candidate = root / marker.rstrip("/")
                if candidate.is_dir():
                    detected.append(stack)
                    break
            else:
                if (root / marker).exists():
                    detected.append(stack)
                    break

    return detected


def generate_gitlab_pipeline(
    stack: list[str],
    *,
    stages: list[str] | None = None,
    include_deploy: bool = False,
) -> str:
    """Generate ``.gitlab-ci.yml`` content based on the detected stack.

    Parameters
    ----------
    stack:
        List of stack identifiers (as returned by :func:`detect_stack`).
    stages:
        Explicit list of CI stages.  Defaults to
        ``["build", "test", "deploy"]``.
    include_deploy:
        If ``True``, append a deploy-to-staging job.

    Returns
    -------
    str
        Complete ``.gitlab-ci.yml`` YAML string.
    """
    if stages is None:
        stages = ["build", "test", "deploy"]

    header = textwrap.dedent(
        f"""\
        # Generated by tag-agent — customise as needed.
        # https://docs.gitlab.com/ee/ci/yaml/

        default:
          retry:
            max: 1
            when:
              - runner_system_failure
              - stuck_or_timeout_failure

        stages:
          - {chr(10 + ord(" ") * 0).join(f"- {s}" for s in stages)}
        """
    )
    # Build stages block properly.
    stages_block = "stages:\n" + "\n".join(f"  - {s}" for s in stages)

    preamble = textwrap.dedent(
        """\
        # Generated by tag-agent — customise as needed.
        # https://docs.gitlab.com/ee/ci/yaml/

        default:
          retry:
            max: 1
            when:
              - runner_system_failure
              - stuck_or_timeout_failure

        """
    )

    parts = [preamble, stages_block, "\n"]

    for identifier in stack:
        snippet = _STACK_JOB_SNIPPETS.get(identifier)
        if snippet:
            parts.append(f"# --- {identifier} ---\n")
            parts.append(snippet)
            parts.append("\n")

    if not stack:
        parts.append(textwrap.dedent(
            """\
            # No stack detected — add your jobs below.
            placeholder:
              stage: test
              script:
                - echo "Add your CI steps here."
            """
        ))

    if include_deploy:
        parts.append("# --- deploy ---\n")
        parts.append(_DEPLOY_SNIPPET)

    return "".join(parts)


def write_gitlab_pipeline(
    repo_root: Path | None = None,
    *,
    force: bool = False,
) -> Path:
    """Write a generated ``.gitlab-ci.yml`` to *repo_root*.

    Parameters
    ----------
    repo_root:
        Repository root.  Defaults to ``Path.cwd()``.
    force:
        If ``False`` and ``.gitlab-ci.yml`` already exists, raise
        ``FileExistsError``.

    Returns
    -------
    Path
        Absolute path to the written file.

    Raises
    ------
    FileExistsError
        If ``.gitlab-ci.yml`` already exists and *force* is ``False``.
    """
    root = Path(repo_root) if repo_root else Path.cwd()
    dest = root / ".gitlab-ci.yml"

    if dest.exists() and not force:
        raise FileExistsError(
            f"{dest} already exists. Pass force=True to overwrite."
        )

    stack = detect_stack(root)
    content = generate_gitlab_pipeline(stack)
    dest.write_text(content, encoding="utf-8")
    return dest.resolve()


# ---------------------------------------------------------------------------
# PRD-063: Self-healing flaky test detection
# ---------------------------------------------------------------------------

def detect_flaky_tests(
    test_log_path: Path,
    *,
    runs: int = 3,
) -> list[dict]:
    """Parse a test log to identify tests with mixed pass/fail results.

    Supports log formats produced by pytest, Jest, Go test, Rust ``cargo
    test``, and RSpec.  The log is expected to contain output from multiple
    test runs separated by a run-delimiter line (e.g. ``=== RUN START ===``
    or a pytest session header) or interleaved with mixed PASSED/FAILED
    lines for each test.

    Parameters
    ----------
    test_log_path:
        Path to the test log file (may contain multiple run outputs
        concatenated).
    runs:
        Expected number of test runs in the log.  Used to normalise the
        flakiness score when run boundaries cannot be detected automatically.

    Returns
    -------
    list[dict]
        Each item has keys: ``test_name`` (str), ``pass_count`` (int),
        ``fail_count`` (int), ``flakiness_score`` (float 0–1).  Only tests
        with at least one pass AND one fail are returned.
    """
    test_log_path = Path(test_log_path)
    content = test_log_path.read_text(encoding="utf-8", errors="replace")

    # Accumulate pass/fail counts per test name.
    pass_counts: dict[str, int] = {}
    fail_counts: dict[str, int] = {}

    # pytest: "PASSED tests/foo.py::test_bar" or "FAILED tests/foo.py::test_bar"
    for m in re.finditer(r"(PASSED|FAILED)\s+([\w/.\-:]+::\w+)", content):
        outcome, name = m.group(1), m.group(2)
        if outcome == "PASSED":
            pass_counts[name] = pass_counts.get(name, 0) + 1
        else:
            fail_counts[name] = fail_counts.get(name, 0) + 1

    # Jest: "✓ test description" / "✗ test description" or "× test description"
    for m in re.finditer(r"([✓✗×])\s+(.+)$", content, re.MULTILINE):
        symbol, name = m.group(1), m.group(2).strip()
        name = re.sub(r"\s+\(\d+\s*ms\)$", "", name)
        if symbol == "✓":
            pass_counts[name] = pass_counts.get(name, 0) + 1
        else:
            fail_counts[name] = fail_counts.get(name, 0) + 1

    # Go test: "--- PASS: TestFoo" / "--- FAIL: TestFoo"
    for m in re.finditer(r"--- (PASS|FAIL): (\S+)", content):
        outcome, name = m.group(1), m.group(2)
        if outcome == "PASS":
            pass_counts[name] = pass_counts.get(name, 0) + 1
        else:
            fail_counts[name] = fail_counts.get(name, 0) + 1

    # Rust: "test foo ... ok" / "test foo ... FAILED"
    for m in re.finditer(r"test (\S+) \.\.\. (ok|FAILED)", content):
        name, outcome = m.group(1), m.group(2)
        if outcome == "ok":
            pass_counts[name] = pass_counts.get(name, 0) + 1
        else:
            fail_counts[name] = fail_counts.get(name, 0) + 1

    # RSpec: "  1) ExampleGroup#method" then later "0 examples, 0 failures"
    for m in re.finditer(r"(Finished|examples|failures).*$", content, re.MULTILINE):
        pass  # RSpec aggregate parsing is complex; skip per-test tracking here.

    # Build flaky list: only tests with both passes and failures.
    flaky: list[dict] = []
    all_tests = set(pass_counts) | set(fail_counts)
    for name in all_tests:
        pc = pass_counts.get(name, 0)
        fc = fail_counts.get(name, 0)
        if pc > 0 and fc > 0:
            total = pc + fc
            flakiness_score = round(1.0 - abs(pc - fc) / total, 4)
            flaky.append(
                {
                    "test_name": name,
                    "pass_count": pc,
                    "fail_count": fc,
                    "flakiness_score": flakiness_score,
                }
            )

    # Sort by flakiness descending (most flaky first).
    flaky.sort(key=lambda x: x["flakiness_score"], reverse=True)
    return flaky


def fix_flaky_test(
    test_name: str,
    test_file: Path,
    profile: str,
    cfg: dict,
    *,
    dry_run: bool = False,
) -> dict:
    """Invoke the TAG runtime to analyse and fix a flaky test.

    The function reads the full test file, builds a targeted prompt, and
    asks the LLM to produce a corrected version of the file that eliminates
    the flakiness.

    Parameters
    ----------
    test_name:
        Fully-qualified test name (as returned by :func:`detect_flaky_tests`).
    test_file:
        Path to the file containing the flaky test.
    profile:
        TAG profile name.
    cfg:
        TAG configuration dict.
    dry_run:
        If ``True``, return the proposed fix without writing the file.

    Returns
    -------
    dict
        Keys: ``original_code`` (str), ``fixed_code`` (str), ``fix_applied``
        (bool), ``explanation`` (str).
    """
    from tag.controller import hermes_bin, profile_exec_env  # type: ignore[import]

    test_file = Path(test_file)
    original_code = test_file.read_text(encoding="utf-8", errors="replace")

    prompt = textwrap.dedent(
        f"""\
        You are an expert software engineer eliminating a flaky (intermittently
        failing) test.

        ## Flaky Test

        Test name: {test_name}
        File: {test_file}

        ## Test File Content

        ```
        {original_code[:7000]}
        ```

        ## Task

        1. Identify the root cause of the flakiness (race condition, time
           dependency, random seed, shared mutable state, network call, etc.).
        2. Rewrite the test so it passes deterministically every time.
        3. Output the COMPLETE corrected test file content followed by a line
           that starts with "EXPLANATION:" and a brief explanation.

        Output format (no markdown fences):
        <corrected file content>
        EXPLANATION: <one-sentence explanation>
        """
    )

    proc = subprocess.run(
        [str(hermes_bin(cfg)), "chat", "-q", prompt, "-Q"],
        env=profile_exec_env(cfg, profile),
        capture_output=True,
        text=True,
    )

    raw_output = proc.stdout.strip()

    # Split out the explanation line.
    explanation = ""
    fixed_code = raw_output
    if "EXPLANATION:" in raw_output:
        idx = raw_output.rfind("EXPLANATION:")
        explanation = raw_output[idx + len("EXPLANATION:"):].strip()
        fixed_code = raw_output[:idx].strip()

    fix_applied = False
    if not dry_run and fixed_code and proc.returncode == 0:
        test_file.write_text(fixed_code, encoding="utf-8")
        fix_applied = True

    return {
        "original_code": original_code,
        "fixed_code": fixed_code,
        "fix_applied": fix_applied,
        "explanation": explanation,
    }


def run_flaky_fix_session(
    test_log_path: Path,
    profile: str,
    cfg: dict,
    *,
    dry_run: bool = False,
    max_fixes: int = 5,
) -> list[dict]:
    """Detect flaky tests in a log and attempt to fix each one.

    Parameters
    ----------
    test_log_path:
        Path to the test log file (passed to :func:`detect_flaky_tests`).
    profile:
        TAG profile name.
    cfg:
        TAG configuration dict.
    dry_run:
        If ``True``, compute fixes but do not write files.
    max_fixes:
        Maximum number of flaky tests to attempt to fix in one session.

    Returns
    -------
    list[dict]
        Each item merges the flaky-test record from
        :func:`detect_flaky_tests` with the fix result from
        :func:`fix_flaky_test`.  An extra key ``error`` (str or ``None``)
        records any exception encountered while fixing.
    """
    flaky = detect_flaky_tests(test_log_path)
    session_results: list[dict] = []

    for entry in flaky[:max_fixes]:
        test_name = entry["test_name"]

        # Attempt to resolve the test file from the test name.
        test_file = _resolve_test_file(test_name)
        if test_file is None or not test_file.exists():
            session_results.append(
                {
                    **entry,
                    "original_code": None,
                    "fixed_code": None,
                    "fix_applied": False,
                    "explanation": "",
                    "error": f"Could not locate test file for {test_name!r}",
                }
            )
            continue

        try:
            fix_result = fix_flaky_test(
                test_name,
                test_file,
                profile,
                cfg,
                dry_run=dry_run,
            )
            session_results.append({**entry, **fix_result, "error": None})
        except Exception as exc:  # noqa: BLE001
            session_results.append(
                {
                    **entry,
                    "original_code": None,
                    "fixed_code": None,
                    "fix_applied": False,
                    "explanation": "",
                    "error": str(exc),
                }
            )

    return session_results


def _resolve_test_file(test_name: str) -> Path | None:
    """Best-effort resolution of a test name to a source file path.

    Handles common conventions:

    - pytest: ``tests/foo.py::TestClass::test_method`` → ``tests/foo.py``
    - Go: ``TestFoo`` or ``pkg.TestFoo`` — search ``*_test.go`` files.
    - Rust: ``crate::module::test_name`` — search ``*.rs`` files.
    - Jest / Mocha: plain name, search ``*.test.{js,ts}`` files.

    Returns ``None`` when resolution fails.

    Parameters
    ----------
    test_name:
        Test identifier string.

    Returns
    -------
    Path | None
        Resolved path or ``None``.
    """
    # pytest: path::name separator.
    if "::" in test_name:
        file_part = test_name.split("::")[0]
        candidate = Path(file_part)
        if candidate.exists():
            return candidate.resolve()

    # Go test: look for the function name in *_test.go files.
    if re.match(r"^[A-Z][a-zA-Z0-9_]*$", test_name):
        for p in Path(".").rglob("*_test.go"):
            try:
                if f"func {test_name}(" in p.read_text(encoding="utf-8", errors="replace"):
                    return p.resolve()
            except OSError:
                continue

    # Rust: test name with "::" separators — look in src/**/*.rs.
    if "::" in test_name:
        fn_name = test_name.split("::")[-1]
        for p in Path(".").rglob("*.rs"):
            try:
                if f"fn {fn_name}(" in p.read_text(encoding="utf-8", errors="replace"):
                    return p.resolve()
            except OSError:
                continue

    # Jest / Mocha: search *.test.{js,ts,jsx,tsx} for matching describe/it/test.
    for pattern in ("*.test.js", "*.test.ts", "*.spec.js", "*.spec.ts"):
        for p in Path(".").rglob(pattern):
            try:
                content = p.read_text(encoding="utf-8", errors="replace")
                if test_name in content:
                    return p.resolve()
            except OSError:
                continue

    return None
