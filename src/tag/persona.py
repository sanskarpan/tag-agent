"""PRD-037: Agent Personas (tag persona).

Lightweight YAML style-injection layer on top of profiles. A persona adds
a style_prompt to a profile's system prompt without modifying stored YAML.

Persona YAML format:
    name: terse-engineer
    description: "Terse, senior-engineer communication style"
    style_prompt: |
      Communicate tersely. Prefer code over explanation. ...
    inject: prepend   # or "append"
    tags: [style, engineering]
"""
from __future__ import annotations

import json
import sqlite3
import uuid
from pathlib import Path
from typing import Any

import yaml

BUILTIN_PERSONAS_DIR = Path(__file__).parent / "config" / "personas"

BUILTIN_PERSONAS: dict[str, dict] = {
    "terse-engineer": {
        "name": "terse-engineer",
        "description": "Terse, senior-engineer style: prefer code, skip preamble",
        "inject": "prepend",
        "style_prompt": (
            "You communicate as a terse senior software engineer. "
            "Skip preamble, avoid filler phrases, and prefer code samples over prose. "
            "Never say 'Certainly!', 'Of course!', or 'Great question!'. "
            "Be direct and precise."
        ),
        "tags": ["style", "engineering"],
    },
    "verbose-explainer": {
        "name": "verbose-explainer",
        "description": "Detailed, tutorial-style explanations for learning contexts",
        "inject": "append",
        "style_prompt": (
            "Explain every concept in detail. Use analogies and examples. "
            "Break complex topics into numbered steps. Assume the reader is learning. "
            "Include 'why' explanations alongside 'how' steps."
        ),
        "tags": ["style", "education"],
    },
    "security-focused": {
        "name": "security-focused",
        "description": "Security-first lens: flag risks, OWASP references, secure defaults",
        "inject": "prepend",
        "style_prompt": (
            "Apply a security-first lens to every response. "
            "Flag OWASP Top 10 risks where relevant, recommend secure defaults, "
            "and always mention if a suggested approach has known CVEs or attack vectors. "
            "Prefer defense-in-depth recommendations."
        ),
        "tags": ["security", "domain"],
    },
    "data-scientist": {
        "name": "data-scientist",
        "description": "Data science domain conventions: pandas, sklearn, Jupyter idioms",
        "inject": "append",
        "style_prompt": (
            "You work within a data science context. Use pandas, numpy, and scikit-learn idioms. "
            "Prefer vectorized operations over loops. Always mention data leakage risks in ML pipelines. "
            "Suggest visualization with matplotlib or seaborn where appropriate."
        ),
        "tags": ["domain", "data"],
    },
    "teacher": {
        "name": "teacher",
        "description": "Socratic teaching style: ask guiding questions, scaffold understanding",
        "inject": "append",
        "style_prompt": (
            "Teach using the Socratic method. Ask guiding questions to help the user discover answers. "
            "Scaffold explanations from fundamentals upward. "
            "Provide worked examples before asking the user to try independently."
        ),
        "tags": ["style", "education"],
    },
}


# ---------------------------------------------------------------------------
# Schema
# ---------------------------------------------------------------------------

def ensure_schema(conn: sqlite3.Connection) -> None:
    conn.executescript("""
        CREATE TABLE IF NOT EXISTS personas (
          id           TEXT PRIMARY KEY,
          name         TEXT NOT NULL UNIQUE,
          description  TEXT NOT NULL DEFAULT '',
          style_prompt TEXT NOT NULL,
          inject       TEXT NOT NULL DEFAULT 'prepend',
          tags_json    TEXT NOT NULL DEFAULT '[]',
          source       TEXT NOT NULL DEFAULT 'builtin',
          created_at   TEXT NOT NULL
        );
        CREATE TABLE IF NOT EXISTS active_personas (
          profile      TEXT NOT NULL,
          persona_name TEXT NOT NULL,
          position     INTEGER NOT NULL DEFAULT 0,
          session_id   TEXT,
          created_at   TEXT NOT NULL,
          PRIMARY KEY(profile, persona_name)
        );
        CREATE INDEX IF NOT EXISTS idx_ap_profile ON active_personas(profile);
    """)
    conn.commit()


def _utc_now() -> str:
    import datetime
    return datetime.datetime.now(datetime.timezone.utc).isoformat()


# ---------------------------------------------------------------------------
# Persona CRUD
# ---------------------------------------------------------------------------

def _seed_builtins(conn: sqlite3.Connection) -> None:
    for p in BUILTIN_PERSONAS.values():
        conn.execute(
            """INSERT OR IGNORE INTO personas(id, name, description, style_prompt, inject, tags_json, source, created_at)
               VALUES(?,?,?,?,?,?,?,?)""",
            (uuid.uuid4().hex[:12], p["name"], p.get("description", ""),
             p["style_prompt"], p.get("inject", "prepend"),
             json.dumps(p.get("tags", [])), "builtin", _utc_now()),
        )
    conn.commit()


def load_persona_file(path: Path) -> dict:
    """Load and validate a persona YAML file."""
    if not path.exists():
        raise FileNotFoundError(f"Persona file not found: {path}")
    try:
        with path.open() as fh:
            data = yaml.safe_load(fh)
    except yaml.YAMLError as exc:
        # Surface a friendly validation message instead of the raw parser dump.
        reason = str(getattr(exc, "problem", None) or exc).strip().splitlines()[0]
        raise ValueError(f"Invalid persona file: {reason}") from exc
    if not isinstance(data, dict):
        raise ValueError("Persona must be a YAML mapping")
    if "style_prompt" not in data:
        raise ValueError("Persona must have a 'style_prompt' field")
    if "name" not in data:
        data["name"] = path.stem
    return data


def install_persona(conn: sqlite3.Connection, persona: dict, source: str = "user") -> str:
    """Insert or replace a persona. Returns the id.

    Built-in persona names are protected: installing over one would permanently
    shadow the builtin (``_seed_builtins`` uses INSERT OR IGNORE and never
    re-seeds), so we refuse rather than allow an override.
    """
    ensure_schema(conn)
    name = persona["name"]
    if source != "builtin" and name in BUILTIN_PERSONAS:
        raise ValueError(
            f"'{name}' is a built-in persona and cannot be overwritten; "
            "choose a different name."
        )
    # Guard against a builtin that has already been seeded into the table.
    existing = conn.execute(
        "SELECT source FROM personas WHERE name=?", (name,)
    ).fetchone()
    if source != "builtin" and existing and existing[0] == "builtin":
        raise ValueError(
            f"'{name}' is a built-in persona and cannot be overwritten; "
            "choose a different name."
        )
    persona_id = uuid.uuid4().hex[:12]
    conn.execute(
        """INSERT INTO personas(id, name, description, style_prompt, inject, tags_json, source, created_at)
           VALUES(?,?,?,?,?,?,?,?)
           ON CONFLICT(name) DO UPDATE SET
             description=excluded.description, style_prompt=excluded.style_prompt,
             inject=excluded.inject, tags_json=excluded.tags_json, source=excluded.source""",
        (persona_id, persona["name"], persona.get("description", ""),
         persona["style_prompt"], persona.get("inject", "prepend"),
         json.dumps(persona.get("tags", [])), source, _utc_now()),
    )
    conn.commit()
    row = conn.execute("SELECT id FROM personas WHERE name=?", (persona["name"],)).fetchone()
    return row[0]


def get_persona(conn: sqlite3.Connection, name: str) -> dict | None:
    ensure_schema(conn)
    _seed_builtins(conn)
    row = conn.execute(
        "SELECT id, name, description, style_prompt, inject, tags_json, source FROM personas WHERE name=?",
        (name,),
    ).fetchone()
    if not row:
        return None
    return {
        "id": row[0], "name": row[1], "description": row[2],
        "style_prompt": row[3], "inject": row[4],
        "tags": json.loads(row[5] or "[]"), "source": row[6],
    }


def list_personas(conn: sqlite3.Connection) -> list[dict]:
    ensure_schema(conn)
    _seed_builtins(conn)
    rows = conn.execute(
        "SELECT id, name, description, inject, tags_json, source FROM personas ORDER BY source, name"
    ).fetchall()
    return [
        {"id": r[0], "name": r[1], "description": r[2],
         "inject": r[3], "tags": json.loads(r[4] or "[]"), "source": r[5]}
        for r in rows
    ]


def remove_persona(conn: sqlite3.Connection, name: str) -> bool:
    ensure_schema(conn)
    cur = conn.execute("DELETE FROM personas WHERE name=? AND source!='builtin'", (name,))
    conn.commit()
    return cur.rowcount > 0


# ---------------------------------------------------------------------------
# Active persona management
# ---------------------------------------------------------------------------

def apply_persona(
    conn: sqlite3.Connection,
    profile: str,
    persona_name: str,
    session_id: str | None = None,
) -> None:
    """Activate a persona for a profile (session-scoped if session_id given)."""
    ensure_schema(conn)
    p = get_persona(conn, persona_name)
    if not p:
        raise ValueError(f"Persona not found: {persona_name!r}")
    # Get current max position
    row = conn.execute(
        "SELECT MAX(position) FROM active_personas WHERE profile=?", (profile,)
    ).fetchone()
    pos = (row[0] or -1) + 1
    conn.execute(
        """INSERT INTO active_personas(profile, persona_name, position, session_id, created_at)
           VALUES(?,?,?,?,?)
           ON CONFLICT(profile, persona_name) DO UPDATE SET
             position=excluded.position, session_id=excluded.session_id""",
        (profile, persona_name, pos, session_id, _utc_now()),
    )
    conn.commit()


def remove_active_persona(conn: sqlite3.Connection, profile: str, persona_name: str) -> bool:
    ensure_schema(conn)
    cur = conn.execute(
        "DELETE FROM active_personas WHERE profile=? AND persona_name=?", (profile, persona_name)
    )
    conn.commit()
    return cur.rowcount > 0


def get_active_personas(
    conn: sqlite3.Connection,
    profile: str,
    session_id: str | None = None,
) -> list[dict]:
    """Return active personas for *profile* ordered by position."""
    ensure_schema(conn)
    _seed_builtins(conn)
    if session_id:
        rows = conn.execute(
            "SELECT persona_name, position FROM active_personas "
            "WHERE profile=? AND (session_id=? OR session_id IS NULL) ORDER BY position",
            (profile, session_id),
        ).fetchall()
    else:
        rows = conn.execute(
            "SELECT persona_name, position FROM active_personas WHERE profile=? ORDER BY position",
            (profile,),
        ).fetchall()
    result = []
    for row in rows:
        p = get_persona(conn, row[0])
        if p:
            p["position"] = row[1]
            result.append(p)
    return result


def build_merged_prompt(
    base_system_prompt: str,
    personas: list[dict],
) -> str:
    """Merge persona style_prompts into *base_system_prompt*."""
    if not personas:
        return base_system_prompt

    prepend_parts: list[str] = []
    append_parts: list[str] = []

    for p in sorted(personas, key=lambda x: x.get("position", 0)):
        style = p["style_prompt"].strip()
        if p.get("inject", "prepend") == "append":
            append_parts.append(style)
        else:
            prepend_parts.append(style)

    parts = []
    if prepend_parts:
        parts.append("\n\n".join(prepend_parts))
    parts.append(base_system_prompt.strip())
    if append_parts:
        parts.append("\n\n".join(append_parts))

    return "\n\n".join(parts)

