"""PRD-044: AgentOps Session Observability (tag config set agentops.api_key).

Optional integration with the AgentOps Python SDK. When agentops.api_key is
set, every tag run automatically starts an AgentOps session, emits LLM call
events, tool call events, and error events, and closes the session on
completion.

Zero overhead and zero imports when the key is not configured.
The agentops SDK is optional; TAG continues normally if it is not installed.
"""
from __future__ import annotations

import json
import sqlite3
from typing import Any

_AGENTOPS_AVAILABLE = False

try:
    import agentops  # type: ignore
    _AGENTOPS_AVAILABLE = True
except ImportError:
    pass


def is_available() -> bool:
    """Return True if the agentops SDK is installed."""
    return _AGENTOPS_AVAILABLE


def is_configured(cfg: dict) -> bool:
    """Return True if agentops.api_key is set in *cfg*."""
    return bool(_get_api_key(cfg))


def _get_api_key(cfg: dict) -> str:
    import os
    # Check config dict first, then env var
    key = cfg.get("agentops", {}).get("api_key", "") or os.environ.get("AGENTOPS_API_KEY", "")
    return key.strip()


def mask_key(key: str) -> str:
    """Show only the last 4 chars of the API key."""
    if not key or len(key) <= 4:
        return "****"
    return f"{'*' * (len(key) - 4)}{key[-4:]}"


# ---------------------------------------------------------------------------
# Schema
# ---------------------------------------------------------------------------

def ensure_schema(conn: sqlite3.Connection) -> None:
    conn.executescript("""
        CREATE TABLE IF NOT EXISTS agentops_sessions (
          id             TEXT PRIMARY KEY,
          run_id         TEXT NOT NULL UNIQUE,
          session_id     TEXT,
          dashboard_url  TEXT,
          status         TEXT NOT NULL DEFAULT 'pending',
          created_at     TEXT NOT NULL,
          closed_at      TEXT
        );
        CREATE INDEX IF NOT EXISTS idx_ao_run ON agentops_sessions(run_id);
    """)
    conn.commit()


def _utc_now() -> str:
    import datetime
    return datetime.datetime.now(datetime.timezone.utc).isoformat()


# ---------------------------------------------------------------------------
# Session lifecycle
# ---------------------------------------------------------------------------

class AgentOpsSession:
    """Thin wrapper around an AgentOps session (or a no-op stub)."""

    def __init__(
        self,
        run_id: str,
        profile: str,
        task: str,
        conn: sqlite3.Connection | None = None,
        cfg: dict | None = None,
    ):
        self._run_id = run_id
        self._profile = profile
        self._task = task
        self._conn = conn
        self._session = None
        self._session_id: str | None = None
        self._dashboard_url: str | None = None
        self._active = False

        # Lazy init
        if cfg and is_configured(cfg) and _AGENTOPS_AVAILABLE:
            try:
                api_key = _get_api_key(cfg)
                agentops.init(api_key=api_key, auto_start_session=False)
                self._session = agentops.start_session(tags=[f"profile:{profile}"])
                self._session_id = str(getattr(self._session, "session_id", ""))
                self._active = True
                if conn:
                    self._persist_session(conn, "active")
            except Exception:
                self._active = False

    def _persist_session(self, conn: sqlite3.Connection, status: str) -> None:
        ensure_schema(conn)
        conn.execute(
            """INSERT INTO agentops_sessions(id, run_id, session_id, dashboard_url, status, created_at)
               VALUES(?,?,?,?,?,?)
               ON CONFLICT(run_id) DO UPDATE SET session_id=excluded.session_id,
               status=excluded.status""",
            (
                self._run_id, self._run_id, self._session_id,
                self._dashboard_url, status, _utc_now(),
            ),
        )
        conn.commit()

    def record_llm_call(
        self,
        model: str,
        prompt_tokens: int,
        completion_tokens: int,
        cost_usd: float = 0.0,
    ) -> None:
        if not self._active or not _AGENTOPS_AVAILABLE:
            return
        try:
            agentops.record(agentops.LLMEvent(
                model=model,
                prompt_tokens=prompt_tokens,
                completion_tokens=completion_tokens,
                cost=cost_usd,
            ))
        except Exception:
            pass

    def record_tool_call(self, tool_name: str, inputs: dict, outputs: str) -> None:
        if not self._active or not _AGENTOPS_AVAILABLE:
            return
        try:
            agentops.record(agentops.ToolEvent(
                name=tool_name,
                logs={"inputs": inputs, "outputs": outputs[:500]},
            ))
        except Exception:
            pass

    def record_error(self, error: str) -> None:
        if not self._active or not _AGENTOPS_AVAILABLE:
            return
        try:
            agentops.record(agentops.ErrorEvent(
                trigger_event=None,
                exception=error,
            ))
        except Exception:
            pass

    def close(self, success: bool = True) -> None:
        if not self._active or not _AGENTOPS_AVAILABLE:
            return
        try:
            end_state = agentops.EndState.Success if success else agentops.EndState.Fail
            agentops.end_session(end_state=end_state)
            self._active = False
            if self._conn:
                ensure_schema(self._conn)
                self._conn.execute(
                    "UPDATE agentops_sessions SET status=?, closed_at=? WHERE run_id=?",
                    ("completed" if success else "failed", _utc_now(), self._run_id),
                )
                self._conn.commit()
        except Exception:
            pass

    @property
    def session_id(self) -> str | None:
        return self._session_id

    @property
    def dashboard_url(self) -> str | None:
        if self._session_id:
            return f"https://app.agentops.ai/sessions/{self._session_id}"
        return None


def get_session_for_run(conn: sqlite3.Connection, run_id: str) -> dict | None:
    """Look up AgentOps session metadata for a tag run."""
    ensure_schema(conn)
    row = conn.execute(
        "SELECT session_id, dashboard_url, status, created_at FROM agentops_sessions WHERE run_id=?",
        (run_id,),
    ).fetchone()
    if not row:
        return None
    session_id = row[0]
    dashboard_url = row[1] or (f"https://app.agentops.ai/sessions/{session_id}" if session_id else None)
    return {
        "session_id": session_id,
        "dashboard_url": dashboard_url,
        "status": row[2],
        "created_at": row[3],
    }


def list_sessions(conn: sqlite3.Connection, *, limit: int = 20) -> list[dict]:
    ensure_schema(conn)
    rows = conn.execute(
        "SELECT run_id, session_id, status, created_at FROM agentops_sessions ORDER BY created_at DESC LIMIT ?",
        (limit,),
    ).fetchall()
    return [
        {
            "run_id": r[0], "session_id": r[1],
            "dashboard_url": f"https://app.agentops.ai/sessions/{r[1]}" if r[1] else None,
            "status": r[2], "created_at": r[3],
        }
        for r in rows
    ]
