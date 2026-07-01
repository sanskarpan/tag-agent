"""PRD-033: Dependency-Aware Task Queue (tag queue dag).

Adds a topological ordering / DAG layer on top of the existing queue_jobs
table. Jobs declare --depends-on parent job IDs; the dispatcher promotes a
job from 'pending' to 'ready' only when all parents have reached 'done'.
"""
from __future__ import annotations

import json
import sqlite3
import uuid
from pathlib import Path


# ---------------------------------------------------------------------------
# Schema migration — adds deps_json column and queue_dags table
# ---------------------------------------------------------------------------

def ensure_schema(conn: sqlite3.Connection) -> None:
    conn.executescript("""
        -- deps_json column: JSON array of prerequisite job IDs
        -- Adds column only if it does not already exist (SQLite-safe pattern)
        CREATE TABLE IF NOT EXISTS _dag_init_sentinel (id INTEGER PRIMARY KEY);
    """)

    # Add deps_json to queue_jobs if not present
    cols = {r[1] for r in conn.execute("PRAGMA table_info(queue_jobs)").fetchall()}
    if "deps_json" not in cols:
        conn.execute("ALTER TABLE queue_jobs ADD COLUMN deps_json TEXT DEFAULT '[]'")

    conn.executescript("""
        CREATE TABLE IF NOT EXISTS queue_dags (
          id          TEXT PRIMARY KEY,
          name        TEXT NOT NULL UNIQUE,
          spec_json   TEXT NOT NULL,
          created_at  TEXT NOT NULL
        );
    """)
    conn.commit()


def _utc_now() -> str:
    import datetime
    return datetime.datetime.now(datetime.timezone.utc).isoformat()


# ---------------------------------------------------------------------------
# Dependency resolution
# ---------------------------------------------------------------------------

def _get_job_status(conn: sqlite3.Connection, job_id: str) -> str | None:
    row = conn.execute("SELECT status FROM queue_jobs WHERE id=?", (job_id,)).fetchone()
    return row[0] if row else None


def all_deps_satisfied(conn: sqlite3.Connection, job_id: str) -> bool:
    """Return True if all declared dependencies of *job_id* have status='done'."""
    row = conn.execute("SELECT deps_json FROM queue_jobs WHERE id=?", (job_id,)).fetchone()
    if not row or not row[0]:
        return True
    deps: list[str] = json.loads(row[0] or "[]")
    for dep_id in deps:
        status = _get_job_status(conn, dep_id)
        if status != "done":
            return False
    return True


# Terminal statuses that mean a dependency can never succeed. A dependent of
# any of these must be cascade-failed rather than left pending forever (B046).
_FAILED_DEP_STATUSES = {"failed", "cancelled", "timed_out"}


def has_failed_dep(conn: sqlite3.Connection, job_id: str) -> str | None:
    """Return the first terminally-failed dependency ID, or None.

    Covers 'failed', 'cancelled', and 'timed_out' — any of which mean the
    dependency will never reach 'done', so the dependent should be failed too.
    """
    row = conn.execute("SELECT deps_json FROM queue_jobs WHERE id=?", (job_id,)).fetchone()
    if not row or not row[0]:
        return None
    deps: list[str] = json.loads(row[0] or "[]")
    for dep_id in deps:
        status = _get_job_status(conn, dep_id)
        if status in _FAILED_DEP_STATUSES:
            return dep_id
    return None


def promote_ready_jobs(conn: sqlite3.Connection) -> list[str]:
    """Promote pending jobs whose dependencies are all satisfied.

    Returns list of job IDs that were promoted from 'pending' to 'ready'.
    """
    # Ensure queue_jobs has deps_json column
    try:
        ensure_schema(conn)
    except Exception:
        pass

    pending_rows = conn.execute(
        "SELECT id FROM queue_jobs WHERE status='pending'"
    ).fetchall()
    promoted: list[str] = []
    for row in pending_rows:
        job_id = row[0]
        # Check for failed deps — if any dep failed, cascade-fail this job
        failed_dep = has_failed_dep(conn, job_id)
        if failed_dep:
            dep_status = _get_job_status(conn, failed_dep) or "failed"
            conn.execute(
                "UPDATE queue_jobs SET status='failed', error=? WHERE id=?",
                (f"dependency {failed_dep} {dep_status}", job_id),
            )
            continue
        if all_deps_satisfied(conn, job_id):
            cursor = conn.execute(
                "UPDATE queue_jobs SET status='ready' WHERE id=? AND status='pending'",
                (job_id,),
            )
            # Only count jobs actually updated (rowcount=0 means another thread beat us)
            if cursor.rowcount > 0:
                promoted.append(job_id)
    conn.commit()
    return promoted


# ---------------------------------------------------------------------------
# Job submission with dependency declaration
# ---------------------------------------------------------------------------

def add_job(
    conn: sqlite3.Connection,
    task: str,
    profile: str | None = None,
    *,
    depends_on: list[str] | None = None,
    task_type: str = "mixed",
    board: str = "default",
) -> str:
    """Insert a new job with optional dependency list. Returns the new job ID."""
    ensure_schema(conn)
    # Strip null bytes for parity with `queue add` and kanban._clean_text (B095):
    # SQLite/display truncate at an embedded NUL, silently corrupting the task.
    task = (task or "").replace("\x00", "")
    job_id = uuid.uuid4().hex[:16]
    now = _utc_now()
    deps = depends_on or []
    # Validate that dependencies exist
    for dep_id in deps:
        row = conn.execute("SELECT id FROM queue_jobs WHERE id=?", (dep_id,)).fetchone()
        if not row:
            raise ValueError(f"Dependency job not found: {dep_id!r}")

    # If no deps, start ready; otherwise pending until promoted
    status = "ready" if not deps else "pending"

    # Use only columns guaranteed to exist in queue_jobs (see controller.py schema)
    conn.execute(
        """INSERT INTO queue_jobs(id, profile, task, task_type, status, deps_json, created_at)
           VALUES(?,?,?,?,?,?,?)""",
        (job_id, profile or "default", task, task_type, status, json.dumps(deps), now),
    )
    conn.commit()
    return job_id


# ---------------------------------------------------------------------------
# DAG visualization
# ---------------------------------------------------------------------------

def list_jobs_raw(conn: sqlite3.Connection, job_ids: list[str] | None = None) -> list[dict]:
    """Return job records as dicts for JSON output."""
    ensure_schema(conn)
    if job_ids:
        rows = conn.execute(
            f"SELECT id, task, task_type, profile, status, deps_json, created_at FROM queue_jobs"
            f" WHERE id IN ({','.join('?'*len(job_ids))})",
            job_ids,
        ).fetchall()
    else:
        rows = conn.execute(
            "SELECT id, task, task_type, profile, status, deps_json, created_at FROM queue_jobs ORDER BY created_at LIMIT 50"
        ).fetchall()
    return [
        {
            "id": r[0], "task": r[1], "task_type": r[2], "profile": r[3],
            "status": r[4], "deps": json.loads(r[5] or "[]"), "created_at": r[6],
        }
        for r in rows
    ]


def show_dag(conn: sqlite3.Connection, job_ids: list[str] | None = None) -> str:
    """Return an ASCII representation of the job dependency graph."""
    ensure_schema(conn)

    if job_ids:
        rows = conn.execute(
            f"SELECT id, task, status, deps_json FROM queue_jobs WHERE id IN ({','.join('?'*len(job_ids))})",
            job_ids,
        ).fetchall()
    else:
        rows = conn.execute(
            "SELECT id, task, status, deps_json FROM queue_jobs ORDER BY created_at LIMIT 50"
        ).fetchall()

    if not rows:
        return "No jobs found."

    # Cover both the DAG vocabulary (pending/ready/done/failed) and the queue
    # vocabulary (queued/running/done/failed/cancelled) since both share the
    # queue_jobs table (B097).
    _STATUS_ICON = {
        "ready": "⏳", "running": "▶", "done": "✓", "failed": "✗",
        "pending": "○", "cancelled": "⊘", "queued": "•",
        "timed_out": "⌛", "skipped": "–",
    }

    lines = ["Job Dependency Graph", "=" * 40]
    for row in rows:
        job_id, task, status, deps_json = row
        icon = _STATUS_ICON.get(status, "?")
        task_short = (task or "")[:50]
        deps = json.loads(deps_json or "[]")
        dep_str = f" ← [{', '.join(d[:8] for d in deps)}]" if deps else ""
        lines.append(f"{icon} {job_id[:12]} [{status:8}] {task_short}{dep_str}")

    return "\n".join(lines)


# ---------------------------------------------------------------------------
# Named DAG storage
# ---------------------------------------------------------------------------

class DagSpec:
    """A named DAG: ordered list of steps with dependency declarations."""

    def __init__(self, name: str, steps: list[dict]):
        """
        steps: list of {task, profile, depends_on (optional step indices)}
        Example:
          steps = [
              {"task": "Generate tests", "profile": "coder"},
              {"task": "Run tests", "profile": "tester", "depends_on": [0]},
              {"task": "Review output", "profile": "reviewer", "depends_on": [1]},
          ]
        """
        self.name = name
        self.steps = steps

    def to_dict(self) -> dict:
        return {"name": self.name, "steps": self.steps}

    def to_json(self) -> str:
        return json.dumps(self.to_dict(), indent=2)

    @classmethod
    def from_dict(cls, d: dict) -> "DagSpec":
        return cls(name=d["name"], steps=d.get("steps", []))

    @classmethod
    def from_json(cls, s: str) -> "DagSpec":
        return cls.from_dict(json.loads(s))


def validate_dag_spec(spec: DagSpec) -> None:
    """Validate a DAG spec at save time. Raises ValueError on bad input (B098)."""
    if not spec.name or not str(spec.name).strip():
        raise ValueError("DAG name must not be empty.")
    steps = spec.steps
    if not isinstance(steps, list):
        raise ValueError(
            f"DAG steps must be a JSON array of step objects, got "
            f"{type(steps).__name__}."
        )
    # Keys the engine actually reads (see run_dag). Unknown keys — especially
    # dependency aliases like 'deps'/'needs' — would be silently ignored,
    # producing an edge-free DAG with wrong ordering, so reject them (C032).
    _RECOGNIZED_STEP_KEYS = {"name", "task", "depends_on", "profile"}
    _DEP_ALIASES = {"deps", "depends", "needs", "dependencies", "requires", "after"}
    for i, step in enumerate(steps):
        if not isinstance(step, dict):
            raise ValueError(f"DAG step {i} must be an object, got {type(step).__name__}.")
        if not str(step.get("task", "")).strip():
            raise ValueError(f"DAG step {i} is missing required non-empty 'task'.")
        unknown = set(step) - _RECOGNIZED_STEP_KEYS
        if unknown:
            alias = sorted(unknown & _DEP_ALIASES)
            if alias:
                raise ValueError(
                    f"DAG step {i} uses unrecognized dependency key(s) "
                    f"{alias}; use 'depends_on' instead."
                )
            raise ValueError(
                f"DAG step {i} has unrecognized key(s) {sorted(unknown)}; "
                f"allowed keys are {sorted(_RECOGNIZED_STEP_KEYS)}."
            )


def save_dag(conn: sqlite3.Connection, spec: DagSpec) -> str:
    ensure_schema(conn)
    validate_dag_spec(spec)
    dag_id = uuid.uuid4().hex[:12]
    conn.execute(
        """INSERT INTO queue_dags(id, name, spec_json, created_at) VALUES(?,?,?,?)
           ON CONFLICT(name) DO UPDATE SET spec_json=excluded.spec_json""",
        (dag_id, spec.name, spec.to_json(), _utc_now()),
    )
    conn.commit()
    row = conn.execute("SELECT id FROM queue_dags WHERE name=?", (spec.name,)).fetchone()
    return row[0]


def run_dag(conn: sqlite3.Connection, dag_name: str, board: str = "default") -> list[str]:
    """Submit all steps of a named DAG in topological order. Returns job IDs."""
    row = conn.execute("SELECT spec_json FROM queue_dags WHERE name=?", (dag_name,)).fetchone()
    if not row:
        raise ValueError(f"DAG not found: {dag_name!r}")
    spec = DagSpec.from_json(row[0])
    steps = spec.steps
    if not isinstance(steps, list):
        raise ValueError(
            f"DAG {dag_name!r} has malformed steps (expected a list, got "
            f"{type(steps).__name__})."
        )

    # Pre-index step names so dependencies can be given as either integer
    # indices or step names.
    name_to_index: dict[str, int] = {}
    for i, step in enumerate(steps):
        if not isinstance(step, dict):
            raise ValueError(f"DAG {dag_name!r} step {i} is not an object.")
        if "task" not in step:
            raise ValueError(f"DAG {dag_name!r} step {i} is missing required 'task'.")
        nm = step.get("name")
        if nm:
            name_to_index[str(nm)] = i

    submitted: list[str] = []  # index → job_id
    for i, step in enumerate(steps):
        dep_refs = step.get("depends_on", []) or []
        if not isinstance(dep_refs, list):
            raise ValueError(f"DAG {dag_name!r} step {i} 'depends_on' must be a list.")
        dep_job_ids: list[str] = []
        for ref in dep_refs:
            if isinstance(ref, bool):
                raise ValueError(f"DAG {dag_name!r} step {i} has an invalid dependency {ref!r}.")
            if isinstance(ref, int):
                idx = ref
            elif isinstance(ref, str):
                if ref not in name_to_index:
                    raise ValueError(
                        f"DAG {dag_name!r} step {i} depends on unknown step {ref!r}."
                    )
                idx = name_to_index[ref]
            else:
                raise ValueError(f"DAG {dag_name!r} step {i} has an invalid dependency {ref!r}.")
            if idx == i:
                raise ValueError(f"DAG {dag_name!r} step {i} cannot depend on itself.")
            if idx < 0 or idx >= i:
                # A dependency must reference an earlier (already-submitted) step.
                # Forward references and out-of-range indices indicate a cycle or
                # an ordering error — fail loudly instead of silently dropping it.
                raise ValueError(
                    f"DAG {dag_name!r} step {i} depends on step {ref!r}, which is not an "
                    f"earlier step (cycles and forward references are not allowed)."
                )
            dep_job_ids.append(submitted[idx])
        job_id = add_job(
            conn, step["task"],
            profile=step.get("profile"),
            depends_on=dep_job_ids,
            board=board,
        )
        submitted.append(job_id)

    return submitted


def list_dags(conn: sqlite3.Connection) -> list[dict]:
    ensure_schema(conn)
    rows = conn.execute(
        "SELECT id, name, spec_json, created_at FROM queue_dags ORDER BY name"
    ).fetchall()
    return [
        {
            "id": r[0], "name": r[1],
            "step_count": len(json.loads(r[2] or "{}").get("steps", [])),
            "created_at": r[3],
        }
        for r in rows
    ]

