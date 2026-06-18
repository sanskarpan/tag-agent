"""CI/CD integration helpers for TAG (PRD-020).

Provides utilities for interacting with GitHub PRs via the ``gh`` CLI,
reading CI log files, building LLM prompts for code-review and failure
diagnosis, and detecting the current git host.
"""

from __future__ import annotations

import json
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

