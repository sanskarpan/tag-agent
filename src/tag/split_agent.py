"""PRD-042: Architect/Editor Agent Split (tag run --architect ... --editor ...).

Two-role execution: an architect model produces a structured JSON change spec,
then an editor model executes each spec item one at a time with restricted
file-only tool grants. The architect reviews each diff before it is applied.
"""
from __future__ import annotations

import json
import sqlite3
import uuid
from pathlib import Path
from typing import Any


# ---------------------------------------------------------------------------
# Change spec schema
# ---------------------------------------------------------------------------

class ChangeItem:
    """One item in the architect's change specification."""
    __slots__ = ("id", "file", "description", "action", "priority", "context")

    def __init__(
        self,
        id: str,
        file: str,
        description: str,
        action: str = "modify",
        priority: int = 0,
        context: str = "",
    ):
        self.id = id
        self.file = file
        self.description = description
        self.action = action   # "modify" | "create" | "delete"
        self.priority = priority
        self.context = context

    def to_dict(self) -> dict:
        return {
            "id": self.id,
            "file": self.file,
            "description": self.description,
            "action": self.action,
            "priority": self.priority,
            "context": self.context,
        }

    @classmethod
    def from_dict(cls, d: dict) -> "ChangeItem":
        if not isinstance(d, dict):
            raise ValueError(
                f"each change item must be a JSON object, got {type(d).__name__}"
            )
        if "file" not in d or "description" not in d:
            raise ValueError("each change item requires 'file' and 'description' fields")
        return cls(
            id=d.get("id", uuid.uuid4().hex[:8]),
            file=d["file"],
            description=d["description"],
            action=d.get("action", "modify"),
            priority=d.get("priority", 0),
            context=d.get("context", ""),
        )


class ChangeSpec:
    """Full architect change specification."""

    def __init__(self, task: str, items: list[ChangeItem], rationale: str = ""):
        self.task = task
        self.items = items
        self.rationale = rationale

    def to_dict(self) -> dict:
        return {
            "task": self.task,
            "rationale": self.rationale,
            "items": [i.to_dict() for i in self.items],
        }

    def to_json(self, indent: int = 2) -> str:
        return json.dumps(self.to_dict(), indent=indent)

    @classmethod
    def from_json(cls, text: str) -> "ChangeSpec":
        return cls.from_dict(json.loads(text))

    @classmethod
    def from_dict(cls, d: dict) -> "ChangeSpec":
        if not isinstance(d, dict):
            raise ValueError(f"change spec must be a JSON object, got {type(d).__name__}")
        raw_items = d.get("items", [])
        if not isinstance(raw_items, list):
            raise ValueError(f"'items' must be a list, got {type(raw_items).__name__}")
        items = [ChangeItem.from_dict(i) for i in raw_items]
        return cls(task=d.get("task", ""), items=items, rationale=d.get("rationale", ""))


# ---------------------------------------------------------------------------
# Schema
# ---------------------------------------------------------------------------

def ensure_schema(conn: sqlite3.Connection) -> None:
    conn.executescript("""
        CREATE TABLE IF NOT EXISTS split_runs (
          id               TEXT PRIMARY KEY,
          task             TEXT NOT NULL,
          architect_model  TEXT NOT NULL,
          editor_model     TEXT NOT NULL,
          profile          TEXT NOT NULL,
          spec_json        TEXT,
          status           TEXT NOT NULL DEFAULT 'pending',
          items_total      INTEGER NOT NULL DEFAULT 0,
          items_done       INTEGER NOT NULL DEFAULT 0,
          items_rejected   INTEGER NOT NULL DEFAULT 0,
          created_at       TEXT NOT NULL,
          updated_at       TEXT NOT NULL
        );
        CREATE TABLE IF NOT EXISTS split_items (
          id          TEXT PRIMARY KEY,
          run_id      TEXT NOT NULL,
          item_id     TEXT NOT NULL,
          file        TEXT NOT NULL,
          description TEXT NOT NULL,
          action      TEXT NOT NULL DEFAULT 'modify',
          status      TEXT NOT NULL DEFAULT 'pending',
          diff        TEXT,
          verdict     TEXT,
          retry_count INTEGER NOT NULL DEFAULT 0,
          created_at  TEXT NOT NULL,
          FOREIGN KEY(run_id) REFERENCES split_runs(id)
        );
        CREATE INDEX IF NOT EXISTS idx_si_run ON split_items(run_id, status);
    """)
    conn.commit()


def _utc_now() -> str:
    import datetime
    return datetime.datetime.now(datetime.timezone.utc).isoformat()


# ---------------------------------------------------------------------------
# CRUD helpers
# ---------------------------------------------------------------------------

def create_split_run(
    conn: sqlite3.Connection,
    task: str,
    architect_model: str,
    editor_model: str,
    profile: str,
) -> str:
    ensure_schema(conn)
    run_id = uuid.uuid4().hex[:16]
    now = _utc_now()
    conn.execute(
        """INSERT INTO split_runs(id, task, architect_model, editor_model, profile,
           status, items_total, items_done, items_rejected, created_at, updated_at)
           VALUES(?,?,?,?,?,'pending',0,0,0,?,?)""",
        (run_id, task, architect_model, editor_model, profile, now, now),
    )
    conn.commit()
    return run_id


def save_spec(conn: sqlite3.Connection, run_id: str, spec: ChangeSpec) -> None:
    ensure_schema(conn)
    now = _utc_now()
    conn.execute(
        "UPDATE split_runs SET spec_json=?, items_total=?, status='planning', updated_at=? WHERE id=?",
        (spec.to_json(), len(spec.items), now, run_id),
    )
    for item in spec.items:
        conn.execute(
            """INSERT INTO split_items(id, run_id, item_id, file, description, action,
               status, created_at) VALUES(?,?,?,?,?,?,'pending',?)""",
            (uuid.uuid4().hex[:12], run_id, item.id, item.file,
             item.description, item.action, now),
        )
    conn.commit()


def update_item_status(
    conn: sqlite3.Connection,
    run_id: str,
    item_id: str,
    status: str,
    diff: str | None = None,
    verdict: str | None = None,
) -> None:
    conn.execute(
        "UPDATE split_items SET status=?, diff=?, verdict=? WHERE run_id=? AND item_id=?",
        (status, diff, verdict, run_id, item_id),
    )
    # Update run counters
    done = conn.execute(
        "SELECT COUNT(*) FROM split_items WHERE run_id=? AND status='accepted'", (run_id,)
    ).fetchone()[0]
    rejected = conn.execute(
        "SELECT COUNT(*) FROM split_items WHERE run_id=? AND status='rejected'", (run_id,)
    ).fetchone()[0]
    conn.execute(
        "UPDATE split_runs SET items_done=?, items_rejected=?, updated_at=? WHERE id=?",
        (done, rejected, _utc_now(), run_id),
    )
    conn.commit()


def get_split_run(conn: sqlite3.Connection, run_id: str) -> dict | None:
    ensure_schema(conn)
    row = conn.execute("SELECT * FROM split_runs WHERE id=?", (run_id,)).fetchone()
    if not row:
        return None
    cols = ["id", "task", "architect_model", "editor_model", "profile",
            "spec_json", "status", "items_total", "items_done", "items_rejected",
            "created_at", "updated_at"]
    d = dict(zip(cols, row))
    d["spec"] = json.loads(d["spec_json"]) if d["spec_json"] else None
    items = conn.execute(
        "SELECT item_id, file, description, action, status, verdict FROM split_items WHERE run_id=? ORDER BY rowid",
        (run_id,),
    ).fetchall()
    d["items"] = [
        {"item_id": r[0], "file": r[1], "description": r[2],
         "action": r[3], "status": r[4], "verdict": r[5]}
        for r in items
    ]
    return d


def list_split_runs(conn: sqlite3.Connection, *, limit: int = 20) -> list[dict]:
    ensure_schema(conn)
    rows = conn.execute(
        """SELECT id, task, architect_model, editor_model, profile, status,
               items_total, items_done, created_at
           FROM split_runs ORDER BY created_at DESC LIMIT ?""",
        (limit,),
    ).fetchall()
    return [
        {
            "id": r[0], "task": r[1][:60], "architect_model": r[2],
            "editor_model": r[3], "profile": r[4], "status": r[5],
            "items_total": r[6], "items_done": r[7], "created_at": r[8],
        }
        for r in rows
    ]


# ---------------------------------------------------------------------------
# Prompt helpers (used by controller to build architect / editor prompts)
# ---------------------------------------------------------------------------

ARCHITECT_SYSTEM = """\
You are the Architect agent. Your ONLY job is to analyse the task and produce a
JSON change specification. You do NOT write code or modify files.

Output EXACTLY this JSON schema (no other text):
{
  "task": "<restate the task>",
  "rationale": "<brief high-level reasoning>",
  "items": [
    {
      "id": "<short unique id>",
      "file": "<relative file path>",
      "description": "<what to change and why>",
      "action": "modify|create|delete",
      "priority": <integer, 0=highest>,
      "context": "<optional extra context for the editor>"
    }
  ]
}
"""

EDITOR_SYSTEM = """\
You are the Editor agent. You implement EXACTLY the change described in the spec
item below. You may ONLY use file-read and file-write operations. Do NOT run
shell commands. Return the final file content only — do not explain.
"""


def build_architect_prompt(task: str, repo_map: str = "") -> str:
    context = f"\n\nREPO MAP:\n{repo_map}" if repo_map else ""
    return f"TASK: {task}{context}"


def build_editor_prompt(item: ChangeItem, file_content: str) -> str:
    return (
        f"SPEC ITEM:\nFile: {item.file}\nAction: {item.action}\n"
        f"Description: {item.description}\n"
        f"Context: {item.context or '(none)'}\n\n"
        f"CURRENT FILE CONTENT:\n```\n{file_content}\n```\n\n"
        f"Produce the complete updated file content."
    )

