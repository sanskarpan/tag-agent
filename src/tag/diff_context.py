"""PRD-038: Diff-Aware Context Injection (tag context inject --git-diff).

Scopes context injection to only the files changed in a git diff.
Injects as a user-message turn (never modifies system prompt) so the
system prompt stays eligible for prompt caching at 0.1x cost.
"""
from __future__ import annotations

import fnmatch
import subprocess
from pathlib import Path
from typing import Iterator

# Files always blocked from injection
DEFAULT_BLOCKED_PATTERNS = [
    ".env", "*.env", ".env.*",
    "*.key", "*.pem", "*.p12", "*.pfx",
    "*secret*", "*credential*", "*password*",
    "*.token",
]

_BINARY_EXTS = {
    ".png", ".jpg", ".jpeg", ".gif", ".ico", ".svg", ".webp",
    ".zip", ".tar", ".gz", ".bz2", ".xz", ".7z", ".rar",
    ".exe", ".dll", ".so", ".dylib", ".pyc",
    ".pdf", ".docx", ".xlsx", ".pptx",
    ".mp4", ".mov", ".mp3", ".wav",
    ".ttf", ".woff", ".woff2", ".eot",
}

WARN_TOKEN_THRESHOLD = 10_000


def _is_blocked(filename: str, blocked_patterns: list[str]) -> bool:
    name = Path(filename).name
    for pat in blocked_patterns:
        if fnmatch.fnmatch(name, pat) or fnmatch.fnmatch(filename, pat):
            return True
    return False


def _is_binary(filename: str) -> bool:
    return Path(filename).suffix.lower() in _BINARY_EXTS


def get_changed_files(
    ref: str = "HEAD",
    *,
    staged: bool = False,
    workdir: Path | None = None,
) -> list[str]:
    """Return list of changed file paths from git diff."""
    cmd = ["git", "diff", "--name-only"]
    if staged:
        cmd.append("--cached")
    else:
        cmd.append(ref)
    try:
        result = subprocess.run(
            cmd, capture_output=True, text=True,
            cwd=str(workdir or Path.cwd()),
        )
        if result.returncode != 0:
            raise RuntimeError(f"git diff failed: {result.stderr.strip()}")
        files = [f.strip() for f in result.stdout.splitlines() if f.strip()]
        return files
    except FileNotFoundError:
        raise RuntimeError("git not found in PATH")


def get_file_diff(
    filename: str,
    ref: str = "HEAD",
    *,
    context_lines: int = 3,
    staged: bool = False,
    workdir: Path | None = None,
) -> str:
    """Return unified diff for a single file."""
    cmd = ["git", "diff", f"-U{context_lines}"]
    if staged:
        cmd.append("--cached")
    else:
        cmd.append(ref)
    cmd.extend(["--", filename])

    try:
        result = subprocess.run(
            cmd, capture_output=True, text=True,
            cwd=str(workdir or Path.cwd()),
        )
        return result.stdout
    except Exception:
        return ""


def _estimate_tokens(text: str) -> int:
    """Rough token estimate: ~4 chars per token."""
    return max(1, len(text) // 4)


def build_diff_context(
    ref: str = "HEAD",
    *,
    staged: bool = False,
    context_lines: int = 3,
    max_files: int = 10,
    blocked_patterns: list[str] | None = None,
    workdir: Path | None = None,
) -> dict:
    """Build a diff context block for injection.

    Returns:
        {
            "content": str,          # the assembled diff text
            "files_included": list,
            "files_skipped": list,
            "estimated_tokens": int,
            "warn": bool,            # True if > WARN_TOKEN_THRESHOLD
        }
    """
    patterns = (blocked_patterns or []) + DEFAULT_BLOCKED_PATTERNS

    all_changed = get_changed_files(ref, staged=staged, workdir=workdir)
    included: list[str] = []
    skipped: list[str] = []

    for f in all_changed:
        if _is_blocked(f, patterns):
            skipped.append(f)
            continue
        if _is_binary(f):
            skipped.append(f)
            continue
        if len(included) >= max_files:
            skipped.append(f)
            continue
        included.append(f)

    diff_parts: list[str] = []
    for f in included:
        diff_text = get_file_diff(f, ref, context_lines=context_lines, staged=staged, workdir=workdir)
        if diff_text.strip():
            diff_parts.append(f"### {f}\n```diff\n{diff_text.rstrip()}\n```")

    content = "\n\n".join(diff_parts)
    tokens = _estimate_tokens(content)

    return {
        "content": content,
        "files_included": included,
        "files_skipped": skipped,
        "estimated_tokens": tokens,
        "warn": tokens > WARN_TOKEN_THRESHOLD,
    }


def pr_diff_context(
    pr_number: int | str,
    repo: str | None = None,
    *,
    context_lines: int = 3,
    max_files: int = 10,
    blocked_patterns: list[str] | None = None,
) -> dict:
    """Fetch and filter a GitHub PR diff via gh CLI."""
    patterns = (blocked_patterns or []) + DEFAULT_BLOCKED_PATTERNS
    cmd = ["gh", "pr", "diff", str(pr_number), "--patch"]
    if repo:
        cmd.extend(["--repo", repo])
    try:
        result = subprocess.run(cmd, capture_output=True, text=True)
        if result.returncode != 0:
            raise RuntimeError(f"gh pr diff failed: {result.stderr.strip()}")
        raw_diff = result.stdout
    except FileNotFoundError:
        raise RuntimeError("gh CLI not found. Install GitHub CLI.")

    # Parse filenames from diff headers
    included: list[str] = []
    skipped: list[str] = []
    current_file: str | None = None
    file_diffs: dict[str, list[str]] = {}

    for line in raw_diff.splitlines():
        if line.startswith("diff --git "):
            parts = line.split(" b/")
            if len(parts) >= 2:
                current_file = parts[-1].strip()
                if _is_blocked(current_file, patterns) or _is_binary(current_file):
                    skipped.append(current_file)
                    current_file = None
                elif len(included) >= max_files:
                    skipped.append(current_file)
                    current_file = None
                else:
                    included.append(current_file)
                    file_diffs[current_file] = [line]
        elif current_file and current_file in file_diffs:
            file_diffs[current_file].append(line)

    diff_parts = []
    for f in included:
        text = "\n".join(file_diffs.get(f, []))
        if text.strip():
            diff_parts.append(f"### {f}\n```diff\n{text}\n```")

    content = "\n\n".join(diff_parts)
    tokens = _estimate_tokens(content)
    return {
        "content": content,
        "files_included": included,
        "files_skipped": skipped,
        "estimated_tokens": tokens,
        "warn": tokens > WARN_TOKEN_THRESHOLD,
    }
