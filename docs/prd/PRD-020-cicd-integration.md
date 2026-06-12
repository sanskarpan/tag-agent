# PRD-020: CI/CD Integration & Automated Code Review

**Status:** Proposed  
**Priority:** P2  
**Estimated Effort:** L (3–4 weeks)  
**Affects:** `controller.py` (new `cmd_review_pr`, `cmd_ci`), new `tag/ci.py`, GitHub Actions YAML

---

## 1. Overview

The most common entry point for AI agent adoption in engineering teams is CI/CD: automated code review on pull requests, test failure diagnosis, and commit message improvements. This PRD adds native GitHub, GitLab, and Gitea integration to TAG, enabling it to run as a CI bot that posts agent-generated PR reviews as inline comments, diagnoses failing tests, and runs automatically on git events.

---

## 2. Problem Statement

- TAG agents can review code, but there is no mechanism to run them automatically on a pull request.
- There is no way to post agent output directly as a GitHub PR comment.
- Teams using TAG for code review must manually copy output into PR comments.
- Competing tools (CodeRabbit, Sweep, GitHub Copilot) are automatically triggered on PRs — TAG appears manual by comparison.
- CI/CD integration would unlock enterprise adoption.

---

## 3. Goals

1. `tag review-pr --repo owner/repo --pr 123` fetches PR diff and runs reviewer profile.
2. `tag review-pr --post-comments` posts findings as inline GitHub PR comments via `gh` CLI.
3. A GitHub Actions workflow template (`tag-review.yml`) lets teams add TAG to any repo in < 5 minutes.
4. `tag ci diagnose --log <file>` takes a CI failure log and runs root cause analysis.
5. `tag ci commit-lint` reviews staged changes and suggests an improved commit message.
6. Supports GitHub, GitLab (via `glab`), and local git repos.

---

## 4. Non-Goals

- Self-hosted CI runner management.
- Automatic code fixes (agents suggest; humans apply).
- Merging PRs or modifying branches.

---

## 5. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|-----------|----------|
| U1 | Developer | run `tag review-pr --pr 123` | I get AI review before merging |
| U2 | Team lead | add TAG to GitHub Actions | every PR gets automatic code review |
| U3 | Developer | run `tag ci diagnose --log pytest-failure.txt` | I understand why tests failed |
| U4 | Developer | run `tag ci commit-lint` | my commit message is clear and conventional |
| U5 | DevOps | post TAG review as inline PR comment | reviewers see feedback in the right context |

---

## 6. Technical Design

### 6.1 New module: `src/tag/ci.py`

```python
"""CI/CD integration helpers for TAG."""
from __future__ import annotations
import json, os, subprocess
from pathlib import Path
from typing import Any


def fetch_pr_diff(repo: str, pr_number: int) -> str:
    """Fetch PR diff using gh CLI."""
    result = subprocess.run(
        ["gh", "pr", "diff", str(pr_number), "--repo", repo],
        capture_output=True, text=True,
    )
    if result.returncode != 0:
        raise RuntimeError(f"Failed to fetch PR diff: {result.stderr}")
    return result.stdout


def fetch_pr_metadata(repo: str, pr_number: int) -> dict[str, Any]:
    """Fetch PR title, description, author via gh CLI."""
    result = subprocess.run(
        ["gh", "pr", "view", str(pr_number), "--repo", repo, "--json",
         "title,body,author,additions,deletions,changedFiles,baseRefName,headRefName"],
        capture_output=True, text=True,
    )
    return json.loads(result.stdout) if result.returncode == 0 else {}


CODE_REVIEW_PROMPT = """You are a senior software engineer performing a code review.

PR Title: {title}
PR Description: {description}
Author: {author}
Changes: +{additions} -{deletions} across {files} files

Diff:
```
{diff}
```

Please provide a thorough code review covering:
1. **Correctness**: Any bugs, logic errors, or incorrect behavior
2. **Security**: Potential vulnerabilities (SQL injection, XSS, secrets in code, etc.)
3. **Performance**: Bottlenecks, unnecessary allocations, N+1 queries
4. **Maintainability**: Code clarity, naming, structure
5. **Testing**: Missing test cases for edge conditions

Format your findings as:
- Line references where applicable (e.g., "Line 42: ...")
- Severity: [CRITICAL] [MAJOR] [MINOR] [SUGGESTION]
- Clear fix recommendations

End with a summary verdict: APPROVE, REQUEST_CHANGES, or COMMENT."""


def format_review_for_github(review_text: str, pr_metadata: dict) -> dict[str, Any]:
    """Format review as GitHub PR review body."""
    # Extract verdict from review
    verdict = "COMMENT"
    if "APPROVE" in review_text.upper()[-200:]:
        verdict = "APPROVE"
    elif "REQUEST_CHANGES" in review_text.upper()[-200:]:
        verdict = "REQUEST_CHANGES"
    
    return {
        "body": review_text,
        "event": verdict,
    }


def post_pr_review(repo: str, pr_number: int, review: dict[str, Any]) -> bool:
    """Post review to GitHub PR via gh CLI."""
    event_flag = {
        "APPROVE": "--approve",
        "REQUEST_CHANGES": "--request-changes",
        "COMMENT": "--comment",
    }.get(review["event"], "--comment")
    
    result = subprocess.run(
        ["gh", "pr", "review", str(pr_number), "--repo", repo,
         event_flag, "--body", review["body"]],
        capture_output=True, text=True,
    )
    return result.returncode == 0


def diagnose_ci_failure(log_text: str) -> str:
    """Return a prompt for diagnosing a CI failure log."""
    return f"""You are a CI/CD expert. Analyze this failure log and identify:
1. Root cause of the failure
2. Specific files/lines involved (if visible in the log)
3. Likely fix (code change, dependency update, config change, etc.)
4. Steps to reproduce locally

Failure log:
```
{log_text[:8000]}
```"""
```

### 6.2 `cmd_review_pr` command

```python
def cmd_review_pr(args: argparse.Namespace) -> int:
    cfg = load_config(config_path(args.config))
    ensure_hermes_ready(cfg, config_arg=args.config, need_tui=False)
    
    from tag.ci import fetch_pr_diff, fetch_pr_metadata, format_review_for_github, post_pr_review, CODE_REVIEW_PROMPT
    
    repo = args.repo
    pr_number = args.pr_number
    profile = getattr(args, "profile", "reviewer")
    
    print(f"Fetching PR #{pr_number} from {repo}…")
    diff = fetch_pr_diff(repo, pr_number)
    metadata = fetch_pr_metadata(repo, pr_number)
    
    if len(diff) > 20000:
        print(f"Large diff ({len(diff):,} chars) — truncating to first 20,000 chars")
        diff = diff[:20000] + "\n[...diff truncated...]"
    
    prompt = CODE_REVIEW_PROMPT.format(
        title=metadata.get("title", ""),
        description=metadata.get("body", "")[:500],
        author=metadata.get("author", {}).get("login", ""),
        additions=metadata.get("additions", 0),
        deletions=metadata.get("deletions", 0),
        files=metadata.get("changedFiles", 0),
        diff=diff,
    )
    
    print(f"Running reviewer profile…")
    from tag.tui_output import chat_spinner
    with chat_spinner(profile, ""):
        result = run_chat_step(cfg, profile, prompt)
    
    review_text = normalize_chat_output(result.get("output", ""))
    
    if getattr(args, "post_comments", False):
        review = format_review_for_github(review_text, metadata)
        success = post_pr_review(repo, pr_number, review)
        if success:
            print(f"✓ Review posted to PR #{pr_number}")
        else:
            print(f"✗ Failed to post review; printing instead:\n\n{review_text}")
    else:
        print(f"\n{review_text}\n")
        if not getattr(args, "no_post_hint", False):
            print(f"Hint: run with --post-comments to post this review to GitHub")
    
    return 0
```

### 6.3 `cmd_ci` command with subcommands

```
tag ci diagnose --log <file>        — diagnose CI failure log
tag ci diagnose --log -             — read log from stdin
tag ci commit-lint                   — review staged changes and suggest commit message
tag ci review-staged                 — run reviewer on current git diff (no PR needed)
```

```python
def cmd_ci(args: argparse.Namespace) -> int:
    cfg = load_config(config_path(args.config))
    sub = args.ci_subcommand
    
    if sub == "diagnose":
        from tag.ci import diagnose_ci_failure
        log_path = getattr(args, "log", None)
        if log_path == "-":
            log_text = sys.stdin.read()
        elif log_path:
            log_text = Path(log_path).read_text()
        else:
            print("Pass --log <file> or --log - to read from stdin", file=sys.stderr)
            return 1
        
        prompt = diagnose_ci_failure(log_text)
        result = run_chat_step(cfg, "reviewer", prompt)
        print(normalize_chat_output(result.get("output", "")))
    
    elif sub == "commit-lint":
        diff = subprocess.run(["git", "diff", "--staged"], capture_output=True, text=True).stdout
        if not diff:
            print("No staged changes. Stage files with git add first.")
            return 1
        
        prompt = f"""Review this staged git diff and suggest a conventional commit message.
Follow Conventional Commits format: type(scope): description

Types: feat, fix, docs, style, refactor, test, chore, perf, ci, build

Diff:
```
{diff[:5000]}
```

Suggest the best commit message (one line, max 72 chars)."""
        result = run_chat_step(cfg, "reviewer", prompt)
        print(normalize_chat_output(result.get("output", "")))
    
    elif sub == "review-staged":
        diff = subprocess.run(["git", "diff", "--staged"], capture_output=True, text=True).stdout
        if not diff:
            diff = subprocess.run(["git", "diff"], capture_output=True, text=True).stdout
        prompt = CODE_REVIEW_PROMPT.format(
            title="Staged changes", description="", author="", 
            additions=0, deletions=0, files=0, diff=diff[:20000]
        )
        result = run_chat_step(cfg, "reviewer", prompt)
        print(normalize_chat_output(result.get("output", "")))
    
    return 0
```

### 6.4 GitHub Actions workflow template

Create `src/tag/config/workflows/tag-review.yml`:
```yaml
name: TAG Code Review

on:
  pull_request:
    types: [opened, synchronize, reopened]

jobs:
  tag-review:
    runs-on: ubuntu-latest
    permissions:
      pull-requests: write
      contents: read
    
    steps:
      - uses: actions/checkout@v4
      
      - name: Set up Python
        uses: actions/setup-python@v5
        with:
          python-version: '3.12'
      
      - name: Install TAG
        run: pip install tag-agent
      
      - name: Setup TAG
        run: tag setup --skip-tui-build
        env:
          OPENROUTER_API_KEY: ${{ secrets.OPENROUTER_API_KEY }}
      
      - name: Run Code Review
        run: |
          tag review-pr \
            --repo ${{ github.repository }} \
            --pr ${{ github.event.pull_request.number }} \
            --post-comments
        env:
          GH_TOKEN: ${{ secrets.GITHUB_TOKEN }}
          OPENROUTER_API_KEY: ${{ secrets.OPENROUTER_API_KEY }}
```

### 6.5 `tag ci install-workflow` helper

```python
def cmd_ci_install_workflow(args: argparse.Namespace) -> int:
    """Copy GitHub Actions workflow template to .github/workflows/."""
    workflow_src = resource_path("config", "workflows", "tag-review.yml")
    workflow_dir = Path(".github") / "workflows"
    workflow_dir.mkdir(parents=True, exist_ok=True)
    
    target = workflow_dir / "tag-review.yml"
    shutil.copy(workflow_src, target)
    print(f"Installed: {target}")
    print("Add OPENROUTER_API_KEY to your GitHub repository secrets.")
    return 0
```

---

## 7. Implementation Plan

| Step | Task |
|------|------|
| 1 | Create `src/tag/ci.py` with PR fetch, format, post functions |
| 2 | Implement `cmd_review_pr` |
| 3 | Implement `cmd_ci` with diagnose, commit-lint, review-staged subcommands |
| 4 | Create `tag-review.yml` GitHub Actions template |
| 5 | Implement `cmd_ci_install_workflow` helper |
| 6 | Register `review-pr` and `ci` parsers |
| 7 | Tests: `test_review_pr_formats_prompt_correctly`, `test_ci_diagnose_reads_stdin` |
| 8 | Update README with CI/CD section |

---

## 8. Success Metrics

- `tag review-pr --repo owner/repo --pr 1` fetches diff and outputs review.
- `tag review-pr --post-comments` posts to GitHub without error.
- `tag ci diagnose --log pytest.log` returns actionable diagnosis.
- `tag ci install-workflow` creates a valid GitHub Actions YAML.

---

## 9. Risks & Mitigations

| Risk | Mitigation |
|------|-----------|
| `gh` CLI not installed | Check with `shutil.which("gh")`; provide install instructions |
| GitHub token permissions insufficient | Document required permissions in workflow template; test with minimal permission set |
| Large PRs exceed context window | Auto-truncate diff at 20,000 chars with clear message |
| Agent posts low-quality reviews | Add `--dry-run` flag (default); require explicit `--post-comments` to post |
| Rate limiting by GitHub API | `gh` handles rate limiting; add retry logic for 429 errors |
