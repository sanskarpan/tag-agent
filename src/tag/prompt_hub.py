"""PRD-052: Versioned prompt storage, diffing, and terminal playground.

Provides:
  - PromptVersion / PlaygroundRun dataclasses
  - SQLite-backed CRUD (ensure_schema, save_prompt, get_prompt, …)
  - diff_versions   — unified diff between two version ints
  - render_prompt   — {{variable}} substitution
  - promote_to_profile — write prompt into a profile's tag.yaml config key
  - record_playground_run / get_playground_history
  - format_version_table — plain-text table for terminal display
"""

from __future__ import annotations

import dataclasses
import datetime
import difflib
import hashlib
import json
import re
import sqlite3
import uuid
from pathlib import Path
from typing import Any


# ---------------------------------------------------------------------------
# Dataclasses
# ---------------------------------------------------------------------------

@dataclasses.dataclass
class PromptVersion:
    id: str
    name: str
    version: int
    content: str
    variables: list[str]
    tags: list[str]
    parent_version_id: str | None
    author: str | None
    message: str | None
    sha256: str
    created_at: str
    is_active: bool = True


@dataclasses.dataclass
class PlaygroundRun:
    id: str
    prompt_version_id: str
    profile: str
    variables_json: str
    rendered_prompt: str
    output: str
    tokens_prompt: int
    tokens_completion: int
    cost_usd: float | None
    score: float | None
    run_at: str


# ---------------------------------------------------------------------------
# Schema
# ---------------------------------------------------------------------------

def ensure_schema(conn: sqlite3.Connection) -> None:
    """Create prompt_versions and playground_runs tables if they don't exist."""
    conn.executescript(
        """
        CREATE TABLE IF NOT EXISTS prompt_versions (
            id                TEXT PRIMARY KEY,
            name              TEXT NOT NULL,
            version           INTEGER NOT NULL,
            content           TEXT NOT NULL,
            variables_json    TEXT NOT NULL DEFAULT '[]',
            tags_json         TEXT NOT NULL DEFAULT '[]',
            parent_version_id TEXT,
            author            TEXT,
            message           TEXT,
            sha256            TEXT NOT NULL,
            created_at        TEXT NOT NULL,
            is_active         INTEGER NOT NULL DEFAULT 1,
            UNIQUE(name, version)
        );

        CREATE INDEX IF NOT EXISTS idx_pv_name_version
            ON prompt_versions(name, version);

        CREATE TABLE IF NOT EXISTS playground_runs (
            id                TEXT PRIMARY KEY,
            prompt_version_id TEXT NOT NULL,
            profile           TEXT NOT NULL,
            variables_json    TEXT NOT NULL DEFAULT '{}',
            rendered_prompt   TEXT NOT NULL,
            output            TEXT NOT NULL,
            tokens_prompt     INTEGER NOT NULL DEFAULT 0,
            tokens_completion INTEGER NOT NULL DEFAULT 0,
            cost_usd          REAL,
            score             REAL,
            run_at            TEXT NOT NULL,
            FOREIGN KEY(prompt_version_id) REFERENCES prompt_versions(id)
        );

        CREATE INDEX IF NOT EXISTS idx_pr_version
            ON playground_runs(prompt_version_id);
        """
    )
    conn.commit()


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

def _now_iso() -> str:
    return datetime.datetime.utcnow().strftime("%Y-%m-%dT%H:%M:%SZ")


def _extract_variables(content: str) -> list[str]:
    """Return deduplicated list of {{variable}} placeholders in order of appearance."""
    seen: dict[str, None] = {}
    for match in re.finditer(r"\{\{(\w+)\}\}", content):
        seen[match.group(1)] = None
    return list(seen)


def _sha256(content: str) -> str:
    return hashlib.sha256(content.encode("utf-8")).hexdigest()


def _row_to_prompt_version(row: sqlite3.Row) -> PromptVersion:
    return PromptVersion(
        id=row["id"],
        name=row["name"],
        version=row["version"],
        content=row["content"],
        variables=json.loads(row["variables_json"]),
        tags=json.loads(row["tags_json"]),
        parent_version_id=row["parent_version_id"],
        author=row["author"],
        message=row["message"],
        sha256=row["sha256"],
        created_at=row["created_at"],
        is_active=bool(row["is_active"]),
    )


def _row_to_playground_run(row: sqlite3.Row) -> PlaygroundRun:
    return PlaygroundRun(
        id=row["id"],
        prompt_version_id=row["prompt_version_id"],
        profile=row["profile"],
        variables_json=row["variables_json"],
        rendered_prompt=row["rendered_prompt"],
        output=row["output"],
        tokens_prompt=row["tokens_prompt"],
        tokens_completion=row["tokens_completion"],
        cost_usd=row["cost_usd"],
        score=row["score"],
        run_at=row["run_at"],
    )


# ---------------------------------------------------------------------------
# CRUD — prompts
# ---------------------------------------------------------------------------

def save_prompt(
    conn: sqlite3.Connection,
    name: str,
    content: str,
    *,
    tags: list[str] | None = None,
    author: str | None = None,
    message: str | None = None,
    parent_version_id: str | None = None,
) -> PromptVersion:
    """Save a new version of a named prompt and return the PromptVersion."""
    conn.row_factory = sqlite3.Row
    cursor = conn.execute(
        "SELECT COALESCE(MAX(version), 0) FROM prompt_versions WHERE name = ?",
        (name,),
    )
    max_version: int = cursor.fetchone()[0]
    new_version = max_version + 1

    variables = _extract_variables(content)
    digest = _sha256(content)
    now = _now_iso()
    version_id = str(uuid.uuid4())

    conn.execute(
        """
        INSERT INTO prompt_versions
            (id, name, version, content, variables_json, tags_json,
             parent_version_id, author, message, sha256, created_at, is_active)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1)
        """,
        (
            version_id,
            name,
            new_version,
            content,
            json.dumps(variables),
            json.dumps(tags or []),
            parent_version_id,
            author,
            message,
            digest,
            now,
        ),
    )
    conn.commit()

    return PromptVersion(
        id=version_id,
        name=name,
        version=new_version,
        content=content,
        variables=variables,
        tags=tags or [],
        parent_version_id=parent_version_id,
        author=author,
        message=message,
        sha256=digest,
        created_at=now,
        is_active=True,
    )


def get_prompt(
    conn: sqlite3.Connection,
    name: str,
    *,
    version: int | None = None,
) -> PromptVersion | None:
    """Return a specific version (or latest active) of a named prompt."""
    conn.row_factory = sqlite3.Row
    if version is None:
        row = conn.execute(
            """
            SELECT * FROM prompt_versions
            WHERE name = ? AND is_active = 1
            ORDER BY version DESC
            LIMIT 1
            """,
            (name,),
        ).fetchone()
    else:
        row = conn.execute(
            "SELECT * FROM prompt_versions WHERE name = ? AND version = ?",
            (name, version),
        ).fetchone()

    if row is None:
        return None
    return _row_to_prompt_version(row)


def list_prompts(conn: sqlite3.Connection) -> list[dict[str, Any]]:
    """Return summary rows: [{name, latest_version, versions_count, created_at, tags}]."""
    conn.row_factory = sqlite3.Row
    rows = conn.execute(
        """
        SELECT
            name,
            MAX(version)  AS latest_version,
            COUNT(*)      AS versions_count,
            MIN(created_at) AS created_at,
            tags_json
        FROM prompt_versions
        GROUP BY name
        ORDER BY name
        """
    ).fetchall()

    result = []
    for row in rows:
        # tags_json from the latest version for display
        tags_row = conn.execute(
            "SELECT tags_json FROM prompt_versions WHERE name = ? ORDER BY version DESC LIMIT 1",
            (row["name"],),
        ).fetchone()
        tags = json.loads(tags_row["tags_json"]) if tags_row else []
        result.append(
            {
                "name": row["name"],
                "latest_version": row["latest_version"],
                "versions_count": row["versions_count"],
                "created_at": row["created_at"],
                "tags": tags,
            }
        )
    return result


def list_versions(conn: sqlite3.Connection, name: str) -> list[PromptVersion]:
    """Return all versions of a prompt ordered by version ascending."""
    conn.row_factory = sqlite3.Row
    rows = conn.execute(
        "SELECT * FROM prompt_versions WHERE name = ? ORDER BY version ASC",
        (name,),
    ).fetchall()
    return [_row_to_prompt_version(r) for r in rows]


# ---------------------------------------------------------------------------
# Diffing
# ---------------------------------------------------------------------------

def diff_versions(conn: sqlite3.Connection, name: str, v1: int, v2: int) -> str:
    """Return a unified diff string between two version numbers of a prompt."""
    pv1 = get_prompt(conn, name, version=v1)
    pv2 = get_prompt(conn, name, version=v2)

    if pv1 is None:
        raise ValueError(f"Prompt '{name}' version {v1} not found.")
    if pv2 is None:
        raise ValueError(f"Prompt '{name}' version {v2} not found.")

    lines1 = pv1.content.splitlines(keepends=True)
    lines2 = pv2.content.splitlines(keepends=True)

    diff = difflib.unified_diff(
        lines1,
        lines2,
        fromfile=f"{name} v{v1}",
        tofile=f"{name} v{v2}",
        lineterm="",
    )
    return "".join(diff)


# ---------------------------------------------------------------------------
# Rendering
# ---------------------------------------------------------------------------

def render_prompt(version: PromptVersion, variables: dict[str, str]) -> str:
    """Substitute {{key}} placeholders in the prompt content.

    Raises ValueError if any declared variable is missing from *variables*.
    """
    missing = [v for v in version.variables if v not in variables]
    if missing:
        raise ValueError(
            f"Missing variables for prompt '{version.name}': {', '.join(missing)}"
        )

    result = version.content
    for key, value in variables.items():
        result = result.replace("{{" + key + "}}", value)
    return result


# ---------------------------------------------------------------------------
# Profile promotion
# ---------------------------------------------------------------------------

def promote_to_profile(
    conn: sqlite3.Connection,
    prompt_version_id: str,
    profile: str,
    config_key: str = "system_prompt",
) -> bool:
    """Write the prompt content into the profile's config section in tag.yaml.

    Locates tag.yaml via the TAG_HOME environment variable (falls back to
    ~/.tag), then sets profiles.<profile>.config.<config_key> to the prompt
    content and saves the file.

    Returns True on success, False if the prompt version was not found.
    """
    import os

    import yaml  # type: ignore[import-untyped]

    conn.row_factory = sqlite3.Row
    row = conn.execute(
        "SELECT * FROM prompt_versions WHERE id = ?",
        (prompt_version_id,),
    ).fetchone()

    if row is None:
        return False

    pv = _row_to_prompt_version(row)

    # Locate tag.yaml using the same logic as controller.py
    tag_home = os.environ.get("TAG_HOME", str(Path.home() / ".tag"))
    config_file = Path(tag_home) / "config" / "tag.yaml"

    if config_file.exists():
        with config_file.open("r", encoding="utf-8-sig") as fh:
            data: dict[str, Any] = yaml.safe_load(fh) or {}
    else:
        data = {}

    profiles: dict[str, Any] = data.setdefault("profiles", {})
    profile_cfg: dict[str, Any] = profiles.setdefault(profile, {})
    profile_config: dict[str, Any] = profile_cfg.setdefault("config", {})
    profile_config[config_key] = pv.content

    config_file.parent.mkdir(parents=True, exist_ok=True)
    with config_file.open("w", encoding="utf-8") as fh:
        yaml.safe_dump(data, fh, sort_keys=False, allow_unicode=True)

    return True


# ---------------------------------------------------------------------------
# Playground runs
# ---------------------------------------------------------------------------

def record_playground_run(
    conn: sqlite3.Connection,
    prompt_version_id: str,
    profile: str,
    variables: dict[str, str],
    rendered: str,
    output: str,
    tokens_p: int,
    tokens_c: int,
    *,
    cost_usd: float | None = None,
    score: float | None = None,
) -> PlaygroundRun:
    """Persist a playground run and return the PlaygroundRun dataclass."""
    run_id = str(uuid.uuid4())
    now = _now_iso()
    variables_json = json.dumps(variables)

    conn.execute(
        """
        INSERT INTO playground_runs
            (id, prompt_version_id, profile, variables_json, rendered_prompt,
             output, tokens_prompt, tokens_completion, cost_usd, score, run_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
        """,
        (
            run_id,
            prompt_version_id,
            profile,
            variables_json,
            rendered,
            output,
            tokens_p,
            tokens_c,
            cost_usd,
            score,
            now,
        ),
    )
    conn.commit()

    return PlaygroundRun(
        id=run_id,
        prompt_version_id=prompt_version_id,
        profile=profile,
        variables_json=variables_json,
        rendered_prompt=rendered,
        output=output,
        tokens_prompt=tokens_p,
        tokens_completion=tokens_c,
        cost_usd=cost_usd,
        score=score,
        run_at=now,
    )


def get_playground_history(
    conn: sqlite3.Connection,
    name: str,
    *,
    limit: int = 20,
) -> list[PlaygroundRun]:
    """Return the most recent playground runs for all versions of *name*."""
    conn.row_factory = sqlite3.Row
    rows = conn.execute(
        """
        SELECT pr.*
        FROM playground_runs pr
        JOIN prompt_versions pv ON pv.id = pr.prompt_version_id
        WHERE pv.name = ?
        ORDER BY pr.run_at DESC
        LIMIT ?
        """,
        (name, limit),
    ).fetchall()
    return [_row_to_playground_run(r) for r in rows]


# ---------------------------------------------------------------------------
# Formatting
# ---------------------------------------------------------------------------

def format_version_table(versions: list[PromptVersion]) -> str:
    """Return a plain-text table summarising the given prompt versions."""
    if not versions:
        return "(no versions)"

    # Column headers
    headers = ["Ver", "ID (short)", "Author", "Tags", "SHA256 (short)", "Active", "Created", "Message"]

    def _short(s: str | None, n: int = 8) -> str:
        if not s:
            return "-"
        return s[:n]

    rows: list[list[str]] = [headers]
    for pv in versions:
        rows.append(
            [
                str(pv.version),
                _short(pv.id),
                pv.author or "-",
                ",".join(pv.tags) if pv.tags else "-",
                _short(pv.sha256),
                "yes" if pv.is_active else "no",
                pv.created_at[:19].replace("T", " "),
                (pv.message or "-")[:40],
            ]
        )

    # Compute column widths
    col_widths = [max(len(r[i]) for r in rows) for i in range(len(headers))]

    sep = "+-" + "-+-".join("-" * w for w in col_widths) + "-+"

    lines: list[str] = [sep]
    for idx, row in enumerate(rows):
        line = "| " + " | ".join(cell.ljust(col_widths[i]) for i, cell in enumerate(row)) + " |"
        lines.append(line)
        if idx == 0:
            lines.append(sep)
    lines.append(sep)
    return "\n".join(lines)
