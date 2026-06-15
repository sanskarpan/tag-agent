"""PRD-039: Token Budget Enforcement (tag budget).

Per-profile hard limits on token consumption over rolling daily/weekly/monthly
windows. Pre-run gate rejects invocations when the hard cap is hit; emits a
warning at 80% utilization.
"""
from __future__ import annotations

import datetime
import sqlite3
import uuid
from typing import Literal

Period = Literal["daily", "weekly", "monthly"]

_PERIOD_DAYS: dict[str, int] = {
    "daily": 1,
    "weekly": 7,
    "monthly": 30,
}


def _utc_now() -> datetime.datetime:
    return datetime.datetime.now(datetime.timezone.utc)


def _window_start(period: str) -> str:
    """ISO timestamp for the start of the current budget window."""
    days = _PERIOD_DAYS.get(period, 1)
    return (_utc_now() - datetime.timedelta(days=days)).isoformat()


def ensure_schema(conn: sqlite3.Connection) -> None:
    conn.executescript("""
        CREATE TABLE IF NOT EXISTS token_budgets (
          id          TEXT PRIMARY KEY,
          profile     TEXT NOT NULL UNIQUE,
          period      TEXT NOT NULL DEFAULT 'daily',
          max_tokens  INTEGER NOT NULL,
          warn_pct    REAL NOT NULL DEFAULT 0.8,
          enabled     INTEGER NOT NULL DEFAULT 1,
          created_at  TEXT NOT NULL,
          updated_at  TEXT NOT NULL
        );
        CREATE INDEX IF NOT EXISTS idx_tb_profile ON token_budgets(profile);
    """)
    conn.commit()


# ---------------------------------------------------------------------------
# CRUD
# ---------------------------------------------------------------------------

def set_budget(
    conn: sqlite3.Connection,
    profile: str,
    max_tokens: int,
    period: Period = "daily",
    warn_pct: float = 0.8,
) -> str:
    """Create or replace a token budget for *profile*."""
    ensure_schema(conn)
    if max_tokens <= 0:
        raise ValueError("max_tokens must be > 0")
    if period not in _PERIOD_DAYS:
        raise ValueError(f"period must be one of {list(_PERIOD_DAYS)}, got {period!r}")
    if not (0.0 < warn_pct < 1.0):
        raise ValueError("warn_pct must be in (0, 1)")

    budget_id = uuid.uuid4().hex[:12]
    now = _utc_now().isoformat()
    conn.execute(
        """INSERT INTO token_budgets(id, profile, period, max_tokens, warn_pct, enabled, created_at, updated_at)
           VALUES(?,?,?,?,?,1,?,?)
           ON CONFLICT(profile) DO UPDATE SET
             period=excluded.period, max_tokens=excluded.max_tokens,
             warn_pct=excluded.warn_pct, enabled=1, updated_at=excluded.updated_at""",
        (budget_id, profile, period, max_tokens, warn_pct, now, now),
    )
    conn.commit()
    # Return the actual id (may be the pre-existing one on upsert)
    row = conn.execute("SELECT id FROM token_budgets WHERE profile=?", (profile,)).fetchone()
    return row[0]


def remove_budget(conn: sqlite3.Connection, profile: str) -> bool:
    ensure_schema(conn)
    cur = conn.execute("DELETE FROM token_budgets WHERE profile=?", (profile,))
    conn.commit()
    return cur.rowcount > 0


def get_budget(conn: sqlite3.Connection, profile: str) -> dict | None:
    ensure_schema(conn)
    row = conn.execute(
        "SELECT id, profile, period, max_tokens, warn_pct, enabled FROM token_budgets WHERE profile=?",
        (profile,),
    ).fetchone()
    if not row:
        return None
    return {
        "id": row[0], "profile": row[1], "period": row[2],
        "max_tokens": row[3], "warn_pct": row[4], "enabled": bool(row[5]),
    }


def list_budgets(conn: sqlite3.Connection) -> list[dict]:
    ensure_schema(conn)
    rows = conn.execute(
        "SELECT id, profile, period, max_tokens, warn_pct, enabled FROM token_budgets ORDER BY profile"
    ).fetchall()
    return [
        {"id": r[0], "profile": r[1], "period": r[2],
         "max_tokens": r[3], "warn_pct": r[4], "enabled": bool(r[5])}
        for r in rows
    ]


# ---------------------------------------------------------------------------
# Usage calculation
# ---------------------------------------------------------------------------

def used_tokens(conn: sqlite3.Connection, profile: str, period: str = "daily") -> int:
    """Total prompt+completion tokens consumed by *profile* within current window."""
    window_start = _window_start(period)
    row = conn.execute(
        """SELECT COALESCE(SUM(prompt_tokens + completion_tokens), 0)
           FROM runs
           WHERE master_profile=? AND created_at >= ? AND status='completed'""",
        (profile, window_start),
    ).fetchone()
    return int(row[0]) if row else 0


# ---------------------------------------------------------------------------
# Pre-run gate
# ---------------------------------------------------------------------------

class BudgetExceeded(Exception):
    """Raised when a profile has exhausted its token budget."""
    def __init__(self, profile: str, used: int, limit: int, period: str):
        super().__init__(
            f"Token budget exceeded for profile '{profile}': "
            f"{used:,} / {limit:,} tokens used ({period})"
        )
        self.profile = profile
        self.used = used
        self.limit = limit
        self.period = period


class BudgetWarning(UserWarning):
    pass


def check_budget(conn: sqlite3.Connection, profile: str) -> dict:
    """Check whether *profile* can run. Returns a status dict.

    Raises BudgetExceeded if the hard cap is reached.
    Emits a BudgetWarning (import warnings; use warnings.warn) near threshold.
    """
    ensure_schema(conn)
    budget = get_budget(conn, profile)
    if budget is None or not budget["enabled"]:
        return {"allowed": True, "budget": None}

    used = used_tokens(conn, profile, budget["period"])
    limit = budget["max_tokens"]
    pct = used / limit if limit > 0 else 0.0

    result = {
        "allowed": True,
        "profile": profile,
        "used": used,
        "limit": limit,
        "period": budget["period"],
        "pct": round(pct * 100, 1),
        "warn": False,
    }

    if pct >= 1.0:
        raise BudgetExceeded(profile, used, limit, budget["period"])

    if pct >= budget["warn_pct"]:
        result["warn"] = True

    return result
