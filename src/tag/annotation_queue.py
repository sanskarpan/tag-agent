"""PRD-051: Human Annotation and Labeling Queue.

Implements a human-in-the-loop annotation queue for labeling agent outputs.
Supports multiple label schema types (scale, choice, freetext) and is suitable
for fine-tuning dataset creation via JSONL export.

Usage:
    import sqlite3
    from tag.annotation_queue import ensure_schema, enqueue, dequeue, submit_label

    conn = sqlite3.connect("tag.db")
    ensure_schema(conn)
    task = enqueue(conn, "run_output", "run-123", "The sky is blue.", "Is this correct?",
                   {"type": "choice", "options": ["yes", "no"]})
    tasks = dequeue(conn, assigned_to="alice@example.com")
    submit_label(conn, tasks[0].id, "yes", notes="Clearly correct.")
"""
from __future__ import annotations

import json
import sqlite3
import uuid
from dataclasses import dataclass, field
from datetime import datetime, timezone
from typing import Any


# ---------------------------------------------------------------------------
# Status constants
# ---------------------------------------------------------------------------

class AnnotationStatus:
    PENDING = "pending"
    IN_PROGRESS = "in_progress"
    COMPLETED = "completed"
    SKIPPED = "skipped"


# ---------------------------------------------------------------------------
# Dataclass
# ---------------------------------------------------------------------------

@dataclass
class AnnotationTask:
    id: str
    source_type: str          # "eval_case", "span", "run_output", "manual"
    source_id: str
    content: str              # text to annotate
    question: str             # what the annotator should answer
    label_schema: dict        # e.g. {"type": "scale", "min": 1, "max": 5}
    status: str = AnnotationStatus.PENDING
    assigned_to: str | None = None
    label: str | None = None
    notes: str | None = None
    created_at: str = field(default_factory=lambda: _now_iso())
    completed_at: str | None = None
    priority: int = 0         # higher = more urgent
    tags: list[str] = field(default_factory=list)


# ---------------------------------------------------------------------------
# Internal helpers
# ---------------------------------------------------------------------------

def _now_iso() -> str:
    return datetime.now(timezone.utc).isoformat()


def _row_to_task(row: sqlite3.Row) -> AnnotationTask:
    tags_raw = row["tags"]
    tags: list[str] = json.loads(tags_raw) if tags_raw else []
    schema_raw = row["label_schema"]
    label_schema: dict = json.loads(schema_raw) if schema_raw else {}
    return AnnotationTask(
        id=row["id"],
        source_type=row["source_type"],
        source_id=row["source_id"],
        content=row["content"],
        question=row["question"],
        label_schema=label_schema,
        status=row["status"],
        assigned_to=row["assigned_to"],
        label=row["label"],
        notes=row["notes"],
        created_at=row["created_at"],
        completed_at=row["completed_at"],
        priority=row["priority"],
        tags=tags,
    )


# ---------------------------------------------------------------------------
# Schema
# ---------------------------------------------------------------------------

def ensure_schema(conn: sqlite3.Connection) -> None:
    """Create annotation_tasks table and indexes if they do not exist."""
    conn.executescript("""
        CREATE TABLE IF NOT EXISTS annotation_tasks (
          id            TEXT PRIMARY KEY,
          source_type   TEXT NOT NULL,
          source_id     TEXT NOT NULL,
          content       TEXT NOT NULL,
          question      TEXT NOT NULL,
          label_schema  TEXT NOT NULL DEFAULT '{}',
          status        TEXT NOT NULL DEFAULT 'pending',
          assigned_to   TEXT,
          label         TEXT,
          notes         TEXT,
          created_at    TEXT NOT NULL,
          completed_at  TEXT,
          priority      INTEGER NOT NULL DEFAULT 0,
          tags          TEXT NOT NULL DEFAULT '[]'
        );
        CREATE INDEX IF NOT EXISTS idx_at_status_priority
          ON annotation_tasks(status, priority DESC, created_at);
        CREATE INDEX IF NOT EXISTS idx_at_assigned
          ON annotation_tasks(assigned_to, status);
    """)
    conn.commit()


# ---------------------------------------------------------------------------
# Enqueue
# ---------------------------------------------------------------------------

def enqueue(
    conn: sqlite3.Connection,
    source_type: str,
    source_id: str,
    content: str,
    question: str,
    label_schema: dict,
    *,
    priority: int = 0,
    tags: list[str] | None = None,
) -> AnnotationTask:
    """Add a new annotation task to the queue with PENDING status.

    Returns the created AnnotationTask.
    """
    task = AnnotationTask(
        id=str(uuid.uuid4()),
        source_type=source_type,
        source_id=source_id,
        content=content,
        question=question,
        label_schema=label_schema,
        status=AnnotationStatus.PENDING,
        priority=priority,
        tags=tags or [],
        created_at=_now_iso(),
    )
    conn.execute(
        """
        INSERT INTO annotation_tasks
          (id, source_type, source_id, content, question, label_schema,
           status, assigned_to, label, notes, created_at, completed_at, priority, tags)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
        """,
        (
            task.id,
            task.source_type,
            task.source_id,
            task.content,
            task.question,
            json.dumps(task.label_schema),
            task.status,
            task.assigned_to,
            task.label,
            task.notes,
            task.created_at,
            task.completed_at,
            task.priority,
            json.dumps(task.tags),
        ),
    )
    conn.commit()
    return task


# ---------------------------------------------------------------------------
# Dequeue
# ---------------------------------------------------------------------------

def dequeue(
    conn: sqlite3.Connection,
    *,
    assigned_to: str | None = None,
    limit: int = 1,
) -> list[AnnotationTask]:
    """Fetch the highest-priority PENDING tasks and mark them IN_PROGRESS.

    Optionally assigns them to *assigned_to*. Returns the claimed tasks.
    """
    conn.row_factory = sqlite3.Row
    # Claim atomically: a single UPDATE...RETURNING runs under SQLite's write
    # lock, so two concurrent workers cannot SELECT-then-UPDATE the same PENDING
    # row (no lost-update window). The subquery selects the highest-priority
    # pending rows; RETURNING yields the post-update (IN_PROGRESS) rows.
    rows = conn.execute(
        """
        UPDATE annotation_tasks
        SET status = ?, assigned_to = ?
        WHERE id IN (
            SELECT id FROM annotation_tasks
            WHERE status = ?
            ORDER BY priority DESC, created_at ASC
            LIMIT ?
        )
        RETURNING *
        """,
        (AnnotationStatus.IN_PROGRESS, assigned_to, AnnotationStatus.PENDING, limit),
    ).fetchall()
    conn.commit()

    if not rows:
        return []

    return [_row_to_task(row) for row in rows]


# ---------------------------------------------------------------------------
# Submit label
# ---------------------------------------------------------------------------

def submit_label(
    conn: sqlite3.Connection,
    task_id: str,
    label: str,
    *,
    notes: str | None = None,
) -> bool:
    """Mark a task as COMPLETED with the given label.

    Returns True if a row was updated, False if the task was not found.
    """
    now = _now_iso()
    cursor = conn.execute(
        """
        UPDATE annotation_tasks
        SET status = ?, label = ?, notes = ?, completed_at = ?
        WHERE id = ?
        """,
        (AnnotationStatus.COMPLETED, label, notes, now, task_id),
    )
    conn.commit()
    return cursor.rowcount > 0


# ---------------------------------------------------------------------------
# Skip task
# ---------------------------------------------------------------------------

def skip_task(conn: sqlite3.Connection, task_id: str) -> bool:
    """Mark a task as SKIPPED.

    Returns True if a row was updated, False if the task was not found.
    """
    cursor = conn.execute(
        "UPDATE annotation_tasks SET status = ? WHERE id = ?",
        (AnnotationStatus.SKIPPED, task_id),
    )
    conn.commit()
    return cursor.rowcount > 0


# ---------------------------------------------------------------------------
# List tasks
# ---------------------------------------------------------------------------

def list_tasks(
    conn: sqlite3.Connection,
    *,
    status: str | None = None,
    assigned_to: str | None = None,
    limit: int = 50,
) -> list[AnnotationTask]:
    """Return annotation tasks filtered by status and/or assigned_to."""
    conn.row_factory = sqlite3.Row
    clauses: list[str] = []
    params: list[Any] = []

    if status is not None:
        clauses.append("status = ?")
        params.append(status)
    if assigned_to is not None:
        clauses.append("assigned_to = ?")
        params.append(assigned_to)

    where = ("WHERE " + " AND ".join(clauses)) if clauses else ""
    params.append(limit)

    rows = conn.execute(
        f"""
        SELECT * FROM annotation_tasks
        {where}
        ORDER BY priority DESC, created_at ASC
        LIMIT ?
        """,
        params,
    ).fetchall()

    return [_row_to_task(r) for r in rows]


# ---------------------------------------------------------------------------
# Queue stats
# ---------------------------------------------------------------------------

def queue_stats(conn: sqlite3.Connection) -> dict:
    """Return aggregate counts and average completion latency.

    Keys: pending, in_progress, completed, skipped, total, avg_latency_hours.
    avg_latency_hours is computed over COMPLETED tasks that have both
    created_at and completed_at populated; None when no completed tasks exist.
    """
    conn.row_factory = None  # use plain tuples for these queries
    counts: dict[str, int] = {
        AnnotationStatus.PENDING: 0,
        AnnotationStatus.IN_PROGRESS: 0,
        AnnotationStatus.COMPLETED: 0,
        AnnotationStatus.SKIPPED: 0,
    }
    for status_val, cnt in conn.execute(
        "SELECT status, COUNT(*) FROM annotation_tasks GROUP BY status"
    ).fetchall():
        if status_val in counts:
            counts[status_val] = cnt

    # Average latency in hours for completed tasks
    row = conn.execute(
        """
        SELECT AVG(
            (julianday(completed_at) - julianday(created_at)) * 24.0
        )
        FROM annotation_tasks
        WHERE status = 'completed'
          AND completed_at IS NOT NULL
          AND created_at IS NOT NULL
        """
    ).fetchone()
    avg_latency: float | None = row[0] if row else None

    total = sum(counts.values())
    return {
        "pending": counts[AnnotationStatus.PENDING],
        "in_progress": counts[AnnotationStatus.IN_PROGRESS],
        "completed": counts[AnnotationStatus.COMPLETED],
        "skipped": counts[AnnotationStatus.SKIPPED],
        "total": total,
        "avg_latency_hours": avg_latency,
    }


# ---------------------------------------------------------------------------
# Export labeled
# ---------------------------------------------------------------------------

def export_labeled(conn: sqlite3.Connection, *, format: str = "jsonl") -> str:
    """Return all COMPLETED tasks with their labels serialized for export.

    Each record is suitable for fine-tuning dataset creation. Supported
    *format* values are ``"jsonl"`` (one JSON object per line) and ``"csv"``.
    """
    if format not in ("jsonl", "csv"):
        raise ValueError(
            f"Unsupported export format: {format!r}. Supported: 'jsonl', 'csv'."
        )

    conn.row_factory = sqlite3.Row
    rows = conn.execute(
        """
        SELECT * FROM annotation_tasks
        WHERE status = ?
        ORDER BY completed_at ASC
        """,
        (AnnotationStatus.COMPLETED,),
    ).fetchall()

    records: list[dict] = []
    for row in rows:
        task = _row_to_task(row)
        records.append({
            "id": task.id,
            "source_type": task.source_type,
            "source_id": task.source_id,
            "content": task.content,
            "question": task.question,
            "label_schema": task.label_schema,
            "label": task.label,
            "notes": task.notes,
            "assigned_to": task.assigned_to,
            "created_at": task.created_at,
            "completed_at": task.completed_at,
            "priority": task.priority,
            "tags": task.tags,
        })

    if format == "csv":
        import csv
        import io

        fieldnames = [
            "id", "source_type", "source_id", "content", "question",
            "label_schema", "label", "notes", "assigned_to",
            "created_at", "completed_at", "priority", "tags",
        ]
        buf = io.StringIO()
        writer = csv.DictWriter(buf, fieldnames=fieldnames)
        writer.writeheader()
        for record in records:
            row_out = dict(record)
            # Serialize nested structures so the CSV cells stay well-formed.
            row_out["label_schema"] = json.dumps(record["label_schema"], ensure_ascii=False)
            row_out["tags"] = json.dumps(record["tags"], ensure_ascii=False)
            writer.writerow(row_out)
        return buf.getvalue()

    return "\n".join(json.dumps(r, ensure_ascii=False) for r in records)


# ---------------------------------------------------------------------------
# Import from eval run
# ---------------------------------------------------------------------------

def import_from_eval_run(
    conn: sqlite3.Connection,
    eval_run_id: str,
    question: str = "Rate the quality of this output (1-5)",
) -> int:
    """Load eval cases from the eval_cases table and enqueue each output.

    Creates one annotation task per eval case using a 1-5 quality scale.
    Returns the number of tasks enqueued.

    Requires the eval_framework schema to be present in the same *conn*
    database (i.e. ensure_schema from eval_framework must have been called).
    """
    conn.row_factory = sqlite3.Row
    rows = conn.execute(
        """
        SELECT id, case_id, input, output
        FROM eval_cases
        WHERE eval_run_id = ?
        ORDER BY rowid ASC
        """,
        (eval_run_id,),
    ).fetchall()

    label_schema: dict = {"type": "scale", "min": 1, "max": 5}
    count = 0
    for row in rows:
        content = (
            f"Input:\n{row['input']}\n\nOutput:\n{row['output']}"
        )
        enqueue(
            conn,
            source_type="eval_case",
            source_id=row["id"],
            content=content,
            question=question,
            label_schema=label_schema,
            tags=[f"eval_run:{eval_run_id}", f"case:{row['case_id']}"],
        )
        count += 1

    return count
