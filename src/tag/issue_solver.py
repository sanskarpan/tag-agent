"""PRD-055: Issue-to-PR autonomous loop.

Fetches issues from GitHub/Linear, creates a branch, invokes TAG to solve,
runs tests, and optionally opens a pull request.
"""
from __future__ import annotations

import json
import re
import subprocess
import time
from dataclasses import dataclass, field
from pathlib import Path


class IssuePlatform:
    GITHUB = "github"
    LINEAR = "linear"
    JIRA = "jira"
    AUTO = "auto"


@dataclass
class Issue:
    id: str
    platform: str
    title: str
    body: str
    url: str
    labels: list[str] = field(default_factory=list)
    assignee: str | None = None
    repo: str | None = None
    number: int | None = None


@dataclass
class SolverResult:
    issue: Issue
    branch_name: str
    commits: list[str] = field(default_factory=list)
    pr_url: str | None = None
    pr_number: int | None = None
    status: str = "dry_run"
    plan: str = ""
    changes_summary: str = ""
    test_results: str | None = None
    cost_usd: float | None = None
    duration_seconds: float = 0.0


def _branch_slug(title: str) -> str:
    slug = re.sub(r'[^a-zA-Z0-9 ]', '', title.lower())
    slug = re.sub(r'\s+', '-', slug.strip())
    return slug[:40].rstrip('-')


def _detect_test_command(repo_root: Path | None = None) -> str | None:
    root = repo_root or Path.cwd()
    if (root / "pyproject.toml").exists() or (root / "setup.py").exists():
        return "python -m pytest --tb=short -q"
    if (root / "package.json").exists():
        return "npm test -- --passWithNoTests"
    if (root / "Cargo.toml").exists():
        return "cargo test"
    if (root / "go.mod").exists():
        return "go test ./..."
    return None


def fetch_issue(
    platform: str,
    issue_ref: str,
    *,
    repo: str | None = None,
    token: str | None = None,
) -> Issue:
    if platform == IssuePlatform.AUTO:
        platform = _detect_platform(issue_ref)

    if platform == IssuePlatform.GITHUB:
        return _fetch_github_issue(issue_ref, repo=repo)
    if platform == IssuePlatform.LINEAR:
        return _fetch_linear_issue(issue_ref, token=token)
    raise NotImplementedError(f"Platform {platform!r} not yet supported")


def _detect_platform(issue_ref: str) -> str:
    if re.match(r'^https://github\.com/', issue_ref):
        return IssuePlatform.GITHUB
    if re.match(r'^[A-Z]+-\d+$', issue_ref):
        return IssuePlatform.LINEAR
    if issue_ref.isdigit():
        return IssuePlatform.GITHUB
    return IssuePlatform.GITHUB


def _fetch_github_issue(issue_ref: str, *, repo: str | None = None) -> Issue:
    # Extract number from URL or plain number
    m = re.search(r'/issues/(\d+)', issue_ref)
    number = int(m.group(1)) if m else int(issue_ref) if issue_ref.isdigit() else None
    if number is None:
        raise ValueError(f"Cannot parse GitHub issue number from: {issue_ref!r}")

    cmd = ["gh", "issue", "view", str(number), "--json",
           "title,body,url,labels,assignees,number"]
    if repo:
        cmd += ["--repo", repo]
    result = subprocess.run(cmd, capture_output=True, text=True)
    if result.returncode != 0:
        raise RuntimeError(f"gh issue view failed: {result.stderr.strip()}")
    data = json.loads(result.stdout)
    return Issue(
        id=f"gh-{data['number']}",
        platform=IssuePlatform.GITHUB,
        title=data.get("title", ""),
        body=data.get("body", ""),
        url=data.get("url", ""),
        labels=[lbl.get("name","") for lbl in (data.get("labels") or [])],
        assignee=(data.get("assignees") or [{}])[0].get("login") if data.get("assignees") else None,
        repo=repo,
        number=data.get("number"),
    )


def _fetch_linear_issue(issue_ref: str, *, token: str | None = None) -> Issue:
    import os
    api_key = token or os.environ.get("LINEAR_API_KEY", "")
    if not api_key:
        raise RuntimeError("LINEAR_API_KEY not set")
    import urllib.request
    # Use a GraphQL variables map instead of interpolating the issue ref into the
    # query string — a quote in issue_ref would otherwise break the JSON/GraphQL
    # body or inject fields.
    graphql = (
        "query($id: String!) { issue(id: $id) { id title description url "
        "labels { nodes { name } } assignee { name } } }"
    )
    body = json.dumps({"query": graphql, "variables": {"id": issue_ref}})
    req = urllib.request.Request(
        "https://api.linear.app/graphql",
        data=body.encode(),
        headers={"Content-Type": "application/json", "Authorization": api_key},
        method="POST",
    )
    with urllib.request.urlopen(req, timeout=15) as resp:
        data = json.loads(resp.read())
    issue_data = data.get("data", {}).get("issue", {})
    return Issue(
        id=issue_ref,
        platform=IssuePlatform.LINEAR,
        title=issue_data.get("title", ""),
        body=issue_data.get("description", ""),
        url=issue_data.get("url", ""),
        labels=[n.get("name","") for n in (issue_data.get("labels",{}).get("nodes") or [])],
        assignee=(issue_data.get("assignee") or {}).get("name"),
    )


def _create_pr(
    issue: Issue, branch: str, plan: str, changes: str
) -> tuple[str, int] | None:
    body = f"""## Summary
{changes[:500]}

## Plan
{plan[:500]}

## Related Issue
{issue.url or f'#{issue.number}'}

---
*Auto-generated by TAG issue-solve*"""
    cmd = [
        "gh", "pr", "create",
        "--title", f"fix: {issue.title[:70]}",
        "--body", body,
        "--head", branch,
    ]
    if issue.repo:
        cmd += ["--repo", issue.repo]
    result = subprocess.run(cmd, capture_output=True, text=True)
    if result.returncode != 0:
        return None
    pr_url = result.stdout.strip()
    m = re.search(r'/pull/(\d+)', pr_url)
    pr_num = int(m.group(1)) if m else None
    return pr_url, pr_num


def _find_tag_bin() -> str:
    import shutil
    for name in ("tag", "tag-agent"):
        found = shutil.which(name)
        if found:
            return found
    return "tag"


def solve_issue(
    issue: Issue,
    profile: str,
    cfg: dict,
    *,
    auto_pr: bool = False,
    dry_run: bool = False,
    sandbox: str | None = None,
    branch_prefix: str = "fix/",
    max_iterations: int = 5,
) -> SolverResult:
    t0 = time.monotonic()
    branch_name = f"{branch_prefix}{issue.id}-{_branch_slug(issue.title)}"
    result = SolverResult(issue=issue, branch_name=branch_name)

    if not dry_run:
        # Create branch
        subprocess.run(["git", "checkout", "-b", branch_name], check=False)

    # Build planning prompt
    prompt = (
        f"You are solving GitHub issue: {issue.title}\n\n"
        f"Issue body:\n{issue.body[:2000]}\n\n"
        "Please analyze the issue and provide:\n"
        "1. A clear plan to fix it\n"
        "2. The specific files and changes needed\n"
        "3. Any edge cases to consider\n"
        "Respond with the plan and then implement the fix.\n"
    )

    tag_bin = _find_tag_bin()
    try:
        cmd = [tag_bin, "-q", prompt, "-p", profile]
        r = subprocess.run(cmd, capture_output=True, text=True, timeout=300)
        output = r.stdout
    except Exception as e:
        output = f"Error invoking TAG: {e}"

    result.plan = output[:1000]
    result.changes_summary = f"TAG output: {len(output)} chars"

    # Run tests if not dry_run
    if not dry_run:
        test_cmd = _detect_test_command()
        if test_cmd:
            try:
                tr = subprocess.run(test_cmd, shell=True, capture_output=True, text=True, timeout=120)
                result.test_results = (tr.stdout + tr.stderr)[:500]
            except Exception:
                result.test_results = "test run timed out"

        # Commit changes
        commit_r = subprocess.run(
            ["git", "add", "-A"], capture_output=True, text=True
        )
        commit_r2 = subprocess.run(
            ["git", "commit", "-m", f"fix: {issue.title[:60]}"],
            capture_output=True, text=True,
        )
        if commit_r2.returncode == 0:
            result.commits.append(commit_r2.stdout.strip())

        if auto_pr:
            pr_result = _create_pr(issue, branch_name, result.plan, result.changes_summary)
            if pr_result:
                result.pr_url, result.pr_number = pr_result
            result.status = "opened" if result.pr_url else "draft"
        else:
            result.status = "draft"
    else:
        result.status = "dry_run"

    result.duration_seconds = time.monotonic() - t0
    return result
