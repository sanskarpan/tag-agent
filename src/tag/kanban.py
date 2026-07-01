"""
TAG-native kanban layer.

Implements the same SQLite schema as hermes kanban_db.py (MIT licensed,
https://github.com/hermes-project/hermes-agent) so tasks we create here
are picked up by any hermes gateway pointed at the same DB file.

Management-plane operations (create task, list tasks, monitor status)
are pure SQLite — zero dependency on the hermes binary or an AI API key.
Execution-plane operations (actually running an AI agent on a task) still
go through hermes; that naturally requires the profile API key.
"""

from __future__ import annotations

import contextlib
import json
import re
import secrets
import sqlite3
import time
from pathlib import Path
from typing import Any, Iterable, Optional

# Board slug: lowercase alphanumeric + hyphens/underscores, no path separators or dots.
_BOARD_SLUG_RE = re.compile(r'^[a-z0-9][a-z0-9\-_]{0,63}$')

# ---------------------------------------------------------------------------
# Schema (compatible with hermes kanban_db.py)
# ---------------------------------------------------------------------------

SCHEMA_SQL = """
CREATE TABLE IF NOT EXISTS tasks (
    id                   TEXT PRIMARY KEY,
    title                TEXT NOT NULL,
    body                 TEXT,
    assignee             TEXT,
    status               TEXT NOT NULL,
    priority             INTEGER DEFAULT 0,
    created_by           TEXT,
    created_at           INTEGER NOT NULL,
    started_at           INTEGER,
    completed_at         INTEGER,
    workspace_kind       TEXT NOT NULL DEFAULT 'scratch',
    workspace_path       TEXT,
    branch_name          TEXT,
    claim_lock           TEXT,
    claim_expires        INTEGER,
    tenant               TEXT,
    result               TEXT,
    idempotency_key      TEXT,
    consecutive_failures INTEGER NOT NULL DEFAULT 0,
    worker_pid           INTEGER,
    last_failure_error   TEXT,
    max_runtime_seconds  INTEGER,
    last_heartbeat_at    INTEGER,
    current_run_id       INTEGER,
    workflow_template_id TEXT,
    current_step_key     TEXT,
    skills               TEXT,
    model_override       TEXT,
    max_retries          INTEGER,
    goal_mode            INTEGER NOT NULL DEFAULT 0,
    goal_max_turns       INTEGER,
    session_id           TEXT
);

CREATE TABLE IF NOT EXISTS task_links (
    parent_id  TEXT NOT NULL,
    child_id   TEXT NOT NULL,
    PRIMARY KEY (parent_id, child_id)
);

CREATE TABLE IF NOT EXISTS task_comments (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id    TEXT NOT NULL,
    author     TEXT NOT NULL,
    body       TEXT NOT NULL,
    created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS task_events (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id    TEXT NOT NULL,
    run_id     INTEGER,
    kind       TEXT NOT NULL,
    payload    TEXT,
    created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS task_runs (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id             TEXT NOT NULL,
    profile             TEXT,
    step_key            TEXT,
    status              TEXT NOT NULL,
    claim_lock          TEXT,
    claim_expires       INTEGER,
    worker_pid          INTEGER,
    max_runtime_seconds INTEGER,
    last_heartbeat_at   INTEGER,
    started_at          INTEGER NOT NULL,
    ended_at            INTEGER,
    outcome             TEXT,
    summary             TEXT,
    metadata            TEXT,
    error               TEXT
);

CREATE TABLE IF NOT EXISTS task_attachments (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id      TEXT NOT NULL,
    filename     TEXT NOT NULL,
    stored_path  TEXT NOT NULL,
    content_type TEXT,
    size         INTEGER NOT NULL DEFAULT 0,
    uploaded_by  TEXT,
    created_at   INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS kanban_notify_subs (
    task_id          TEXT NOT NULL,
    platform         TEXT NOT NULL,
    chat_id          TEXT NOT NULL,
    thread_id        TEXT NOT NULL DEFAULT '',
    user_id          TEXT,
    notifier_profile TEXT,
    created_at       INTEGER NOT NULL,
    last_event_id    INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (task_id, platform, chat_id, thread_id)
);

CREATE INDEX IF NOT EXISTS idx_tasks_assignee_status ON tasks(assignee, status);
CREATE INDEX IF NOT EXISTS idx_tasks_status          ON tasks(status);
CREATE INDEX IF NOT EXISTS idx_links_child           ON task_links(child_id);
CREATE INDEX IF NOT EXISTS idx_links_parent          ON task_links(parent_id);
CREATE INDEX IF NOT EXISTS idx_comments_task         ON task_comments(task_id, created_at);
CREATE INDEX IF NOT EXISTS idx_events_task           ON task_events(task_id, created_at);
CREATE INDEX IF NOT EXISTS idx_runs_task             ON task_runs(task_id, started_at);
CREATE INDEX IF NOT EXISTS idx_runs_status           ON task_runs(status);
CREATE INDEX IF NOT EXISTS idx_attachments_task      ON task_attachments(task_id, created_at);
CREATE INDEX IF NOT EXISTS idx_notify_task           ON kanban_notify_subs(task_id);
"""

VALID_STATUSES = {
    "triage", "todo", "scheduled", "ready", "running",
    "blocked", "review", "done", "archived",
}
TERMINAL_STATUSES = {"done", "archived"}

# ---------------------------------------------------------------------------
# Path helpers
# ---------------------------------------------------------------------------

def _validate_board_slug(board: str) -> str:
    """Normalise and validate a board slug. Raises ValueError on bad input."""
    slug = board.lower().strip()
    # Reject anything containing path separators, dots, or null bytes before
    # regex check — these could escape the boards directory via traversal.
    if any(c in slug for c in ('/', '\\', '\x00', '..')) or '..' in slug:
        raise ValueError(
            f"Invalid board name {board!r}: must not contain path separators or '..'"
        )
    if not _BOARD_SLUG_RE.match(slug):
        raise ValueError(
            f"Invalid board name {board!r}: use lowercase letters, digits, "
            "hyphens and underscores only (1-64 chars, must start with alphanumeric)."
        )
    return slug


def kanban_db_path(hermes_home: Path, board: str = "default") -> Path:
    """Return the kanban.db path for a given profile hermes_home."""
    if board == "default":
        return hermes_home / "kanban.db"
    slug = _validate_board_slug(board)
    # Extra containment: resolve and assert the result stays under hermes_home.
    candidate = hermes_home / "kanban" / "boards" / slug / "kanban.db"
    try:
        resolved = candidate.resolve()
        home_resolved = hermes_home.resolve()
        if not str(resolved).startswith(str(home_resolved)):
            raise ValueError(
                f"Board path {resolved} escapes hermes home {home_resolved}"
            )
    except (OSError, ValueError):
        raise ValueError(f"Invalid board path for {board!r}")
    return candidate


def profile_kanban_db_path(cfg: dict[str, Any], profile_name: str, board: str = "default") -> Path:
    """Resolve kanban DB path for a TAG profile's hermes home."""
    from tag.controller import profile_home
    return kanban_db_path(profile_home(cfg, profile_name), board)

# ---------------------------------------------------------------------------
# Connection
# ---------------------------------------------------------------------------

def open_db(path: Path) -> sqlite3.Connection:
    """Open (and init if needed) a kanban.db at path."""
    path.parent.mkdir(parents=True, exist_ok=True)
    conn = sqlite3.connect(str(path), timeout=30)
    conn.row_factory = sqlite3.Row
    conn.execute("PRAGMA busy_timeout = 30000")
    conn.execute("PRAGMA journal_mode = WAL")
    conn.execute("PRAGMA synchronous = NORMAL")
    conn.execute("PRAGMA foreign_keys = ON")
    conn.executescript(SCHEMA_SQL)
    return conn


@contextlib.contextmanager
def open_db_ctx(path: Path):
    conn = open_db(path)
    try:
        yield conn
    finally:
        conn.close()

# ---------------------------------------------------------------------------
# Internal helpers
# ---------------------------------------------------------------------------

def _clean_text(s: Optional[str]) -> Optional[str]:
    """Strip null bytes from text before writing to SQLite."""
    if s is None:
        return None
    return s.replace("\x00", "")


def _new_task_id() -> str:
    return "t_" + secrets.token_hex(4)


@contextlib.contextmanager
def _write_txn(conn: sqlite3.Connection):
    conn.execute("BEGIN IMMEDIATE")
    try:
        yield conn
        conn.execute("COMMIT")
    except Exception:
        conn.execute("ROLLBACK")
        raise


def _append_event(conn: sqlite3.Connection, task_id: str, kind: str, payload: Any = None) -> None:
    conn.execute(
        "INSERT INTO task_events (task_id, kind, payload, created_at) VALUES (?, ?, ?, ?)",
        (task_id, kind, json.dumps(payload) if payload is not None else None, int(time.time())),
    )

# ---------------------------------------------------------------------------
# Task CRUD
# ---------------------------------------------------------------------------

def create_task(
    conn: sqlite3.Connection,
    *,
    title: str,
    body: Optional[str] = None,
    assignee: Optional[str] = None,
    created_by: Optional[str] = None,
    parents: Iterable[str] = (),
    workspace_kind: str = "scratch",
    workspace_path: Optional[str] = None,
    tenant: Optional[str] = None,
    priority: int = 0,
    idempotency_key: Optional[str] = None,
    max_runtime_seconds: Optional[int] = None,
    skills: Optional[Iterable[str]] = None,
    goal_mode: bool = False,
    goal_max_turns: Optional[int] = None,
) -> str:
    """Create a task; return its id. Status is 'ready' unless parents are pending."""
    title = _clean_text(title) or ""
    body = _clean_text(body)
    if not title or not title.strip():
        raise ValueError("title is required")
    parent_ids = [p for p in parents if p]
    skills_json = json.dumps(list(skills)) if skills else None

    if idempotency_key:
        row = conn.execute(
            "SELECT id FROM tasks WHERE idempotency_key = ? AND status != 'archived' "
            "ORDER BY created_at DESC LIMIT 1",
            (idempotency_key,),
        ).fetchone()
        if row:
            return row["id"]

    now = int(time.time())
    for _ in range(2):
        task_id = _new_task_id()
        try:
            with _write_txn(conn):
                # Determine initial status
                if parent_ids:
                    rows = conn.execute(
                        "SELECT status FROM tasks WHERE id IN ({})".format(
                            ",".join("?" * len(parent_ids))
                        ),
                        parent_ids,
                    ).fetchall()
                    status = "todo" if any(r["status"] not in TERMINAL_STATUSES for r in rows) else "ready"
                else:
                    status = "ready"

                conn.execute(
                    """
                    INSERT INTO tasks (
                        id, title, body, assignee, status, priority,
                        created_by, created_at, workspace_kind, workspace_path,
                        tenant, idempotency_key, max_runtime_seconds,
                        skills, goal_mode, goal_max_turns
                    ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
                    """,
                    (
                        task_id, title.strip(), body, assignee, status, priority,
                        created_by, now, workspace_kind, workspace_path,
                        tenant, idempotency_key,
                        int(max_runtime_seconds) if max_runtime_seconds is not None else None,
                        skills_json, 1 if goal_mode else 0,
                        int(goal_max_turns) if goal_max_turns is not None else None,
                    ),
                )
                for pid in parent_ids:
                    conn.execute(
                        "INSERT OR IGNORE INTO task_links (parent_id, child_id) VALUES (?, ?)",
                        (pid, task_id),
                    )
                _append_event(conn, task_id, "created", {
                    "assignee": assignee, "status": status, "parents": parent_ids,
                })
            return task_id
        except sqlite3.IntegrityError:
            continue
    raise RuntimeError("task id collision after 2 attempts")


def get_task(conn: sqlite3.Connection, task_id: str) -> Optional[dict[str, Any]]:
    row = conn.execute("SELECT * FROM tasks WHERE id = ?", (task_id,)).fetchone()
    return dict(row) if row else None


def list_tasks(
    conn: sqlite3.Connection,
    *,
    assignee: Optional[str] = None,
    status: Optional[str] = None,
    parent_id: Optional[str] = None,
    include_archived: bool = False,
    limit: Optional[int] = None,
) -> list[dict[str, Any]]:
    """List tasks with optional filters."""
    if parent_id:
        # tasks that are children of parent_id
        query = (
            "SELECT t.* FROM tasks t "
            "JOIN task_links l ON l.child_id = t.id "
            "WHERE l.parent_id = ?"
        )
        params: list[Any] = [parent_id]
    else:
        query = "SELECT * FROM tasks WHERE 1=1"
        params = []

    if assignee is not None:
        query += " AND t.assignee = ?" if parent_id else " AND assignee = ?"
        params.append(assignee)
    if status is not None:
        query += " AND t.status = ?" if parent_id else " AND status = ?"
        params.append(status)
    if not include_archived:
        query += " AND t.status != 'archived'" if parent_id else " AND status != 'archived'"
    query += " ORDER BY priority DESC, created_at ASC"
    # Respect an explicit limit=0 (return zero rows) — `if limit:` treated 0 as
    # falsy and dropped the LIMIT clause, returning every row (C038).
    if limit is not None:
        query += f" LIMIT {int(limit)}"
    rows = conn.execute(query, params).fetchall()
    return [dict(r) for r in rows]


def complete_task(
    conn: sqlite3.Connection,
    task_id: str,
    *,
    result: Optional[str] = None,
    summary: Optional[str] = None,
    metadata: Optional[dict] = None,
) -> bool:
    now = int(time.time())
    with _write_txn(conn):
        cur = conn.execute(
            """
            UPDATE tasks
               SET status = 'done', result = ?, completed_at = ?,
                   claim_lock = NULL, claim_expires = NULL, worker_pid = NULL
             WHERE id = ? AND status IN ('running', 'ready', 'blocked', 'todo')
            """,
            (result, now, task_id),
        )
        if cur.rowcount:
            _append_event(conn, task_id, "completed", {
                "summary": summary, "result_preview": (result or "")[:200],
                "metadata": metadata,
            })
            # Unblock children that were waiting only on this task
            _maybe_unblock_children(conn, task_id)
    return bool(cur.rowcount)


def _maybe_unblock_children(conn: sqlite3.Connection, parent_id: str) -> None:
    """Promote todo→ready for children whose remaining parents are all done."""
    children = conn.execute(
        "SELECT child_id FROM task_links WHERE parent_id = ?", (parent_id,)
    ).fetchall()
    for row in children:
        cid = row["child_id"]
        pending = conn.execute(
            "SELECT 1 FROM task_links l JOIN tasks p ON p.id = l.parent_id "
            "WHERE l.child_id = ? AND p.status NOT IN ('done','archived') LIMIT 1",
            (cid,),
        ).fetchone()
        if not pending:
            conn.execute(
                "UPDATE tasks SET status='ready' WHERE id=? AND status='todo'", (cid,)
            )


# ---------------------------------------------------------------------------
# Comments (used as swarm blackboard)
# ---------------------------------------------------------------------------

def add_comment(
    conn: sqlite3.Connection,
    task_id: str,
    *,
    author: str,
    body: str,
) -> int:
    cur = conn.execute(
        "INSERT INTO task_comments (task_id, author, body, created_at) VALUES (?, ?, ?, ?)",
        (task_id, author, body, int(time.time())),
    )
    return cur.lastrowid  # type: ignore[return-value]


def list_comments(conn: sqlite3.Connection, task_id: str) -> list[dict[str, Any]]:
    rows = conn.execute(
        "SELECT * FROM task_comments WHERE task_id = ? ORDER BY created_at ASC",
        (task_id,),
    ).fetchall()
    return [dict(r) for r in rows]

# ---------------------------------------------------------------------------
# Swarm topology (ported from hermes kanban_swarm.py, MIT licensed)
# ---------------------------------------------------------------------------

_SWARM_CONTEXT_TMPL = """

## Swarm protocol
- Swarm root / shared blackboard: `{root_id}`.
- Read sibling/parent handoffs from Kanban context before working.
- Put machine-readable facts in completion metadata.
- Put cross-worker notes on the root task using structured comments.
- Goal: {goal}
"""

_BLACKBOARD_PREFIX = "[swarm:blackboard] "


def create_swarm(
    conn: sqlite3.Connection,
    *,
    goal: str,
    workers: Iterable[tuple[str, str]],  # (profile, title)
    verifier_assignee: str,
    synthesizer_assignee: str,
    tenant: Optional[str] = None,
    created_by: str = "tag-swarm-orchestrator",
    priority: int = 0,
    idempotency_key: Optional[str] = None,
) -> dict[str, Any]:
    """Create a kanban swarm topology.

    workers: list of (profile_name, task_title) tuples.
    Returns dict: {root_id, worker_ids, verifier_id, synthesizer_id}
    """
    if not goal.strip():
        raise ValueError("goal is required")
    worker_specs = list(workers)
    if not worker_specs:
        raise ValueError("at least one worker required")

    root_id = create_task(
        conn,
        title=f"Swarm: {goal.splitlines()[0][:80]}",
        body=(
            "Kanban Swarm v1 planning/root card. Completed immediately; "
            "remains the shared blackboard and audit anchor.\n\nGoal:\n" + goal
        ),
        assignee=created_by,
        created_by=created_by,
        tenant=tenant,
        priority=priority,
        idempotency_key=idempotency_key,
        skills=["kanban-orchestrator"],
    )

    # Check idempotency: if we recovered an existing root, return its topology
    existing = latest_blackboard(conn, root_id).get("topology")
    if isinstance(existing, dict):
        wids = [str(x) for x in existing.get("worker_ids", []) if x]
        vid = existing.get("verifier_id")
        sid = existing.get("synthesizer_id")
        if wids and vid and sid:
            return {"root_id": root_id, "worker_ids": wids,
                    "verifier_id": str(vid), "synthesizer_id": str(sid)}

    complete_task(conn, root_id,
                  summary="Swarm topology planned.",
                  metadata={"kind": "kanban_swarm_v1", "goal": goal,
                            "worker_count": len(worker_specs)})

    ctx = _SWARM_CONTEXT_TMPL.format(root_id=root_id, goal=goal)
    worker_ids: list[str] = []
    for profile, title in worker_specs:
        wid = create_task(
            conn,
            title=title,
            body=(f"Work assigned to {profile} as part of swarm." + ctx),
            assignee=profile,
            created_by=created_by,
            parents=[root_id],
            tenant=tenant,
            priority=priority,
        )
        worker_ids.append(wid)

    verifier_id = create_task(
        conn,
        title="Verify swarm outputs",
        body=(
            "Review every worker handoff and blackboard update. "
            "Gate the swarm: complete only with metadata {\"gate\": \"pass\"} "
            "when evidence is sufficient; otherwise block with exact missing work."
            + ctx
        ),
        assignee=verifier_assignee,
        created_by=created_by,
        parents=worker_ids,
        tenant=tenant,
        priority=priority,
        skills=["requesting-code-review"],
    )

    synthesizer_id = create_task(
        conn,
        title="Synthesize swarm outputs",
        body=(
            "Synthesize the verified worker outputs into the final deliverable. "
            "Do not start until the verifier has passed the gate." + ctx
        ),
        assignee=synthesizer_assignee,
        created_by=created_by,
        parents=[verifier_id],
        tenant=tenant,
        priority=priority,
        skills=["humanizer"],
    )

    topology = {
        "root_id": root_id,
        "worker_ids": worker_ids,
        "verifier_id": verifier_id,
        "synthesizer_id": synthesizer_id,
    }
    post_blackboard_update(conn, root_id, author=created_by,
                           key="topology", value=topology | {"goal": goal})
    return topology


def post_blackboard_update(
    conn: sqlite3.Connection,
    root_id: str,
    *,
    author: str,
    key: str,
    value: Any,
) -> None:
    payload = json.dumps({"key": key, "value": value})
    add_comment(conn, root_id, author=author, body=_BLACKBOARD_PREFIX + payload)


def latest_blackboard(conn: sqlite3.Connection, root_id: str) -> dict[str, Any]:
    """Merge blackboard comments into a single dict (last-write-wins)."""
    merged: dict[str, Any] = {}
    for comment in list_comments(conn, root_id):
        body = comment.get("body", "")
        if not body.startswith(_BLACKBOARD_PREFIX):
            continue
        try:
            payload = json.loads(body[len(_BLACKBOARD_PREFIX):])
            k = payload.get("key")
            if isinstance(k, str) and k:
                merged[k] = payload.get("value")
        except (json.JSONDecodeError, AttributeError):
            pass
    return merged


# ---------------------------------------------------------------------------
# Status helpers
# ---------------------------------------------------------------------------

def tasks_are_terminal(conn: sqlite3.Connection, task_ids: list[str]) -> bool:
    """True if all given task IDs have reached done or archived."""
    if not task_ids:
        return True
    rows = conn.execute(
        "SELECT status FROM tasks WHERE id IN ({})".format(
            ",".join("?" * len(task_ids))
        ),
        task_ids,
    ).fetchall()
    return all(r["status"] in TERMINAL_STATUSES for r in rows)


def swarm_status_summary(conn: sqlite3.Connection, topology: dict[str, Any]) -> dict[str, Any]:
    """Return a status snapshot for all tasks in a swarm topology."""
    all_ids = (
        [topology["root_id"]]
        + topology.get("worker_ids", [])
        + [topology.get("verifier_id"), topology.get("synthesizer_id")]
    )
    all_ids = [i for i in all_ids if i]
    tasks = {t["id"]: t["status"] for t in [get_task(conn, i) for i in all_ids] if t}
    done_count = sum(1 for s in tasks.values() if s in TERMINAL_STATUSES)
    return {
        "tasks": tasks,
        "total": len(all_ids),
        "done": done_count,
        "complete": done_count == len(all_ids),
    }

