"""PRD-024: Repo-Map / Workspace Context.

Builds a token-efficient map of the workspace for injection into agent context.
Uses git ls-files for file discovery, computes a lightweight importance score
(file size + recency), and renders a compact ASCII tree.
"""
from __future__ import annotations

import hashlib
import os
import sqlite3
import subprocess
from pathlib import Path

# Approximate tokens per byte for plain text (rough heuristic)
_BYTES_PER_TOKEN = 4.0

# Extensions that should be included in the workspace map
_INCLUDE_EXTS = {
    ".py", ".js", ".ts", ".jsx", ".tsx", ".go", ".rs", ".java", ".c", ".cpp",
    ".h", ".hpp", ".rb", ".php", ".swift", ".kt", ".scala", ".cs", ".sh",
    ".bash", ".zsh", ".fish", ".yaml", ".yml", ".toml", ".json", ".md",
    ".rst", ".txt", ".sql", ".html", ".css", ".scss",
}

# Directories to always skip
_SKIP_DIRS = {
    ".git", ".hg", ".svn", "node_modules", "__pycache__", ".venv", "venv",
    "env", ".env", "dist", "build", ".tox", ".mypy_cache", ".pytest_cache",
    ".ruff_cache", "coverage", ".coverage", "htmlcov", "target",
}


def _ensure_schema(conn: sqlite3.Connection) -> None:
    conn.executescript("""
        CREATE TABLE IF NOT EXISTS workspace_files (
          path         TEXT PRIMARY KEY,
          content_hash TEXT NOT NULL,
          byte_size    INTEGER NOT NULL DEFAULT 0,
          token_count  INTEGER NOT NULL DEFAULT 0,
          rank         REAL NOT NULL DEFAULT 0.0,
          indexed_at   TEXT NOT NULL
        );
        CREATE INDEX IF NOT EXISTS idx_wf_rank ON workspace_files(rank DESC);
    """)
    conn.commit()


def _files_from_git(root: Path) -> list[Path]:
    """Return tracked files via git ls-files. Falls back to os.walk on error."""
    try:
        result = subprocess.run(
            ["git", "ls-files", "--cached", "--others", "--exclude-standard"],
            cwd=root,
            capture_output=True,
            text=True,
            timeout=15,
        )
        if result.returncode == 0:
            return [root / p for p in result.stdout.splitlines() if p]
    except Exception:
        pass

    # Fallback: walk directory tree
    files = []
    for dirpath, dirnames, filenames in os.walk(root):
        dirnames[:] = [d for d in dirnames if d not in _SKIP_DIRS]
        for fn in filenames:
            fp = Path(dirpath) / fn
            files.append(fp)
    return files


def index_workspace(conn: sqlite3.Connection, root: Path, *, max_files: int = 500) -> dict:
    """Scan the workspace and update the workspace_files table.

    Returns a summary dict with counts.
    """
    import datetime
    _ensure_schema(conn)

    all_files = _files_from_git(root)
    # Filter by extension and existence
    candidates = [
        f for f in all_files
        if f.suffix in _INCLUDE_EXTS and f.is_file()
        and not any(part in _SKIP_DIRS for part in f.parts)
    ]

    # Score: smaller files and recently modified files rank higher
    scored = []
    for fp in candidates:
        try:
            stat = fp.stat()
            size = stat.st_size
            mtime = stat.st_mtime
            # Rank: recency weight + inverse-size weight (smaller = easier to include)
            age_days = (datetime.datetime.now().timestamp() - mtime) / 86400
            rank = 1.0 / (1.0 + age_days * 0.1) + 1.0 / (1.0 + size / 10000)
            scored.append((fp, size, rank))
        except OSError:
            continue

    scored.sort(key=lambda t: -t[2])
    selected = scored[:max_files]

    now_iso = datetime.datetime.now(datetime.timezone.utc).isoformat()
    indexed = 0
    for fp, size, rank in selected:
        try:
            content = fp.read_bytes()
            h = hashlib.sha256(content[:4096]).hexdigest()[:16]
            tokens = int(size / _BYTES_PER_TOKEN)
            rel_path = str(fp.relative_to(root))
            conn.execute(
                """INSERT INTO workspace_files(path, content_hash, byte_size, token_count, rank, indexed_at)
                   VALUES(?,?,?,?,?,?)
                   ON CONFLICT(path) DO UPDATE SET
                     content_hash=excluded.content_hash, byte_size=excluded.byte_size,
                     token_count=excluded.token_count, rank=excluded.rank, indexed_at=excluded.indexed_at""",
                (rel_path, h, size, tokens, rank, now_iso),
            )
            indexed += 1
        except Exception:
            continue

    conn.commit()
    total_tokens = conn.execute("SELECT SUM(token_count) FROM workspace_files").fetchone()[0] or 0
    return {
        "files_indexed": indexed,
        "total_files": len(all_files),
        "total_tokens": total_tokens,
        "max_rank_file": selected[0][0].name if selected else None,
    }


def build_workspace_map(conn: sqlite3.Connection, root: Path, *, budget_tokens: int = 4000) -> str:
    """Return a token-efficient ASCII tree map of the top-ranked workspace files.

    Respects *budget_tokens* by including only the highest-ranked files that fit.
    """
    _ensure_schema(conn)

    rows = conn.execute(
        "SELECT path, token_count, rank FROM workspace_files ORDER BY rank DESC LIMIT 200"
    ).fetchall()

    if not rows:
        return "(workspace not indexed — run `tag workspace index` first)"

    # Build file tree
    tree: dict = {}
    included_tokens = 0
    for row in rows:
        path_str, tokens, _ = row
        if included_tokens + tokens > budget_tokens:
            continue
        included_tokens += tokens
        parts = Path(path_str).parts
        node = tree
        for part in parts[:-1]:
            node = node.setdefault(part, {})
        node[parts[-1]] = None  # leaf

    lines = [f"Workspace: {root.name}/  ({included_tokens} tokens)"]
    _render_tree(tree, lines, prefix="")
    return "\n".join(lines)


def _render_tree(node: dict, lines: list[str], prefix: str) -> None:
    items = sorted(node.items(), key=lambda x: (x[1] is not None, x[0]))
    for i, (name, child) in enumerate(items):
        connector = "└── " if i == len(items) - 1 else "├── "
        lines.append(prefix + connector + name)
        if isinstance(child, dict):
            ext = "    " if i == len(items) - 1 else "│   "
            _render_tree(child, lines, prefix + ext)


def workspace_status(conn: sqlite3.Connection) -> dict:
    """Return statistics about the indexed workspace."""
    _ensure_schema(conn)
    row = conn.execute(
        "SELECT COUNT(*), SUM(token_count), MAX(indexed_at) FROM workspace_files"
    ).fetchone()
    return {
        "file_count": row[0] or 0,
        "total_tokens": row[1] or 0,
        "last_indexed": row[2],
    }

