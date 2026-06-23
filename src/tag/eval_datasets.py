"""PRD-049: Versioned eval dataset management.

Provides persistent dataset creation, import from eval runs, and YAML export
compatible with eval_framework.load_suite().
"""
from __future__ import annotations

import json
import sqlite3
import uuid
from dataclasses import dataclass, field
from datetime import datetime, timezone
from pathlib import Path
from typing import Any


def _utc_now() -> str:
    return datetime.now(timezone.utc).isoformat()


@dataclass
class EvalDataset:
    id: str
    name: str
    description: str
    created_at: str
    version: int
    source_type: str
    case_count: int
    tags: list[str] = field(default_factory=list)


@dataclass
class EvalDatasetCase:
    id: str
    dataset_id: str
    case_id: str
    input: str
    expected_output: str | None
    reference_context: str | None
    metadata_json: str
    created_at: str


def ensure_schema(conn: sqlite3.Connection) -> None:
    conn.executescript("""
        CREATE TABLE IF NOT EXISTS eval_datasets (
            id           TEXT PRIMARY KEY,
            name         TEXT NOT NULL UNIQUE,
            description  TEXT NOT NULL DEFAULT '',
            created_at   TEXT NOT NULL,
            version      INTEGER NOT NULL DEFAULT 1,
            source_type  TEXT NOT NULL DEFAULT 'manual',
            case_count   INTEGER NOT NULL DEFAULT 0,
            tags_json    TEXT NOT NULL DEFAULT '[]'
        );

        CREATE TABLE IF NOT EXISTS eval_dataset_cases (
            id                 TEXT PRIMARY KEY,
            dataset_id         TEXT NOT NULL REFERENCES eval_datasets(id),
            case_id            TEXT NOT NULL,
            input              TEXT NOT NULL,
            expected_output    TEXT,
            reference_context  TEXT,
            metadata_json      TEXT NOT NULL DEFAULT '{}',
            created_at         TEXT NOT NULL
        );
        CREATE INDEX IF NOT EXISTS idx_edc_dataset ON eval_dataset_cases(dataset_id);
    """)
    conn.commit()


def _row_to_dataset(row: sqlite3.Row | tuple, keys: list[str] | None = None) -> EvalDataset:
    if isinstance(row, sqlite3.Row):
        d = dict(row)
    else:
        cols = keys or ["id","name","description","created_at","version","source_type","case_count","tags_json"]
        d = dict(zip(cols, row))
    return EvalDataset(
        id=d["id"],
        name=d["name"],
        description=d.get("description",""),
        created_at=d["created_at"],
        version=d.get("version",1),
        source_type=d.get("source_type","manual"),
        case_count=d.get("case_count",0),
        tags=json.loads(d.get("tags_json","[]") or "[]"),
    )


def create_dataset(
    conn: sqlite3.Connection,
    name: str,
    description: str = "",
    *,
    tags: list[str] | None = None,
    source_type: str = "manual",
) -> EvalDataset:
    ensure_schema(conn)
    ds_id = uuid.uuid4().hex[:12]
    now = _utc_now()
    tags_json = json.dumps(tags or [])
    conn.execute(
        """INSERT INTO eval_datasets(id,name,description,created_at,version,source_type,case_count,tags_json)
           VALUES (?,?,?,?,1,?,0,?)""",
        (ds_id, name, description, now, source_type, tags_json),
    )
    conn.commit()
    conn.row_factory = sqlite3.Row
    row = conn.execute("SELECT * FROM eval_datasets WHERE id=?", (ds_id,)).fetchone()
    conn.row_factory = None
    return _row_to_dataset(row)


def add_case(
    conn: sqlite3.Connection,
    dataset_id: str,
    case_id: str,
    input_text: str,
    *,
    expected_output: str | None = None,
    reference_context: str | None = None,
    metadata: dict | None = None,
) -> EvalDatasetCase:
    ensure_schema(conn)
    c_id = uuid.uuid4().hex[:12]
    now = _utc_now()
    meta_json = json.dumps(metadata or {})
    conn.execute(
        """INSERT INTO eval_dataset_cases(id,dataset_id,case_id,input,expected_output,
           reference_context,metadata_json,created_at) VALUES (?,?,?,?,?,?,?,?)""",
        (c_id, dataset_id, case_id, input_text, expected_output, reference_context, meta_json, now),
    )
    conn.execute(
        "UPDATE eval_datasets SET case_count=case_count+1 WHERE id=?", (dataset_id,)
    )
    conn.commit()
    return EvalDatasetCase(
        id=c_id, dataset_id=dataset_id, case_id=case_id, input=input_text,
        expected_output=expected_output, reference_context=reference_context,
        metadata_json=meta_json, created_at=now,
    )


def import_from_eval_runs(
    conn: sqlite3.Connection,
    dataset_name: str,
    *,
    since_days: int = 7,
    limit: int = 50,
    profile: str | None = None,
) -> EvalDataset:
    ensure_schema(conn)
    ds = create_dataset(conn, dataset_name, source_type="from_runs")
    try:
        profile_clause = "AND r.profile=?" if profile else ""
        profile_params: list[Any] = [profile] if profile else []
        rows = conn.execute(
            f"""SELECT c.id, c.input, c.expected_output, c.metadata_json
                FROM eval_cases c
                JOIN eval_runs r ON c.run_id=r.id
                WHERE r.created_at >= datetime('now', '-{int(since_days)} days')
                {profile_clause}
                AND c.status='pass'
                ORDER BY r.created_at DESC
                LIMIT ?""",
            profile_params + [limit],
        ).fetchall()
        for row in rows:
            add_case(conn, ds.id, row[0], row[1] or "",
                     expected_output=row[2],
                     metadata=json.loads(row[3] or "{}"))
    except Exception:
        pass
    # Return refreshed dataset
    conn.row_factory = sqlite3.Row
    row = conn.execute("SELECT * FROM eval_datasets WHERE id=?", (ds.id,)).fetchone()
    conn.row_factory = None
    return _row_to_dataset(row)


def export_to_yaml(conn: sqlite3.Connection, dataset_id: str) -> str:
    ensure_schema(conn)
    conn.row_factory = sqlite3.Row
    ds_row = conn.execute("SELECT * FROM eval_datasets WHERE id=?", (dataset_id,)).fetchone()
    if not ds_row:
        return ""
    ds = _row_to_dataset(ds_row)
    cases = conn.execute(
        "SELECT * FROM eval_dataset_cases WHERE dataset_id=? ORDER BY created_at",
        (dataset_id,),
    ).fetchall()
    conn.row_factory = None

    lines = [
        f"name: {ds.name}",
        f"description: {ds.description or ''}",
        "cases:",
    ]
    for c in cases:
        inp = str(c["input"]).replace('"', '\\"')
        lines.append(f'  - id: {c["case_id"]}')
        lines.append(f'    input: "{inp}"')
        if c["expected_output"]:
            exp = str(c["expected_output"]).replace('"', '\\"')
            lines.append(f'    expected_output: "{exp}"')
    return "\n".join(lines) + "\n"


def list_datasets(conn: sqlite3.Connection) -> list[EvalDataset]:
    ensure_schema(conn)
    conn.row_factory = sqlite3.Row
    rows = conn.execute("SELECT * FROM eval_datasets ORDER BY created_at DESC").fetchall()
    conn.row_factory = None
    return [_row_to_dataset(r) for r in rows]


def get_dataset(conn: sqlite3.Connection, name_or_id: str) -> EvalDataset | None:
    ensure_schema(conn)
    conn.row_factory = sqlite3.Row
    row = conn.execute(
        "SELECT * FROM eval_datasets WHERE id=? OR name=?", (name_or_id, name_or_id)
    ).fetchone()
    conn.row_factory = None
    return _row_to_dataset(row) if row else None


def delete_dataset(conn: sqlite3.Connection, dataset_id: str) -> bool:
    ensure_schema(conn)
    conn.execute("DELETE FROM eval_dataset_cases WHERE dataset_id=?", (dataset_id,))
    cur = conn.execute("DELETE FROM eval_datasets WHERE id=?", (dataset_id,))
    conn.commit()
    return cur.rowcount > 0
