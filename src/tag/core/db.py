"""Database connection and schema utilities for TAG CLI."""
from __future__ import annotations

import json
import os
import sqlite3
import subprocess
import sys
import time
import uuid
from pathlib import Path
from typing import Any

try:
    from tag.tui_output import print_error
except Exception:
    def print_error(msg: str) -> None:
        print(f"error: {msg}", file=sys.stderr)

try:
    from tag.core.paths import runtime_db_path, ensure_runtime_dirs
    from tag.core.config import config_path
except Exception:
    from tag.controller import (  # type: ignore[no-redef]
        runtime_db_path,
        ensure_runtime_dirs,
        config_path,
    )

try:
    from tag.core.utils import utc_now
except Exception:
    import datetime as _dt

    def utc_now() -> str:  # type: ignore[misc]
        return _dt.datetime.now(_dt.timezone.utc).isoformat()


# ---------------------------------------------------------------------------
# Database open / schema
# ---------------------------------------------------------------------------

def open_db(cfg: dict[str, Any]) -> sqlite3.Connection:
    ensure_runtime_dirs(cfg)
    db_path = runtime_db_path(cfg)
    try:
        conn = sqlite3.connect(db_path, timeout=5)
        conn.row_factory = sqlite3.Row
        conn.execute("PRAGMA busy_timeout = 5000")
        conn.execute("PRAGMA foreign_keys = ON")
    except sqlite3.DatabaseError as exc:
        print_error(f"Database error: {exc}. Try deleting {db_path} and re-running.")
        raise SystemExit(1)
    last_error: sqlite3.OperationalError | None = None
    for _ in range(20):
        try:
            conn.execute("PRAGMA journal_mode = WAL")
            conn.execute("PRAGMA synchronous = NORMAL")
            last_error = None
            break
        except sqlite3.DatabaseError as exc:
            if "locked" not in str(exc).lower():
                print_error(f"Database corrupt or unreadable ({exc}). Try deleting {db_path}.")
                raise SystemExit(1)
            last_error = sqlite3.OperationalError(str(exc))
            time.sleep(0.1)
    if last_error is not None:
        raise last_error
    conn.executescript(
        """
        CREATE TABLE IF NOT EXISTS runs (
          id TEXT PRIMARY KEY,
          created_at TEXT NOT NULL,
          kind TEXT NOT NULL,
          task_type TEXT NOT NULL,
          execution TEXT NOT NULL,
          master_profile TEXT NOT NULL,
          board TEXT NOT NULL,
          prompt TEXT NOT NULL,
          route_json TEXT NOT NULL,
          status TEXT NOT NULL,
          metadata_json TEXT NOT NULL
        );

        CREATE TABLE IF NOT EXISTS steps (
          id INTEGER PRIMARY KEY AUTOINCREMENT,
          run_id TEXT NOT NULL,
          role TEXT NOT NULL,
          profile TEXT NOT NULL,
          model_ref TEXT NOT NULL,
          prompt TEXT NOT NULL,
          output TEXT NOT NULL,
          status TEXT NOT NULL,
          started_at TEXT NOT NULL,
          finished_at TEXT NOT NULL,
          duration_ms INTEGER NOT NULL,
          extra_json TEXT NOT NULL,
          FOREIGN KEY(run_id) REFERENCES runs(id)
        );

        CREATE TABLE IF NOT EXISTS memory_journal (
          id          TEXT PRIMARY KEY,
          profile     TEXT NOT NULL,
          key         TEXT NOT NULL,
          value       TEXT NOT NULL,
          scope       TEXT NOT NULL DEFAULT 'profile',
          created_at  TEXT NOT NULL,
          expires_at  TEXT,
          UNIQUE(profile, key)
        );
        CREATE INDEX IF NOT EXISTS idx_mj_profile ON memory_journal(profile);

        CREATE TABLE IF NOT EXISTS queue_jobs (
          id          TEXT PRIMARY KEY,
          profile     TEXT NOT NULL,
          task        TEXT NOT NULL,
          task_type   TEXT NOT NULL DEFAULT 'mixed',
          status      TEXT NOT NULL DEFAULT 'queued',
          priority    INTEGER NOT NULL DEFAULT 5,
          created_at  TEXT NOT NULL,
          started_at  TEXT,
          finished_at TEXT,
          pid         INTEGER,
          result_path TEXT,
          exit_code   INTEGER,
          error       TEXT,
          notify      INTEGER NOT NULL DEFAULT 1
        );
        CREATE INDEX IF NOT EXISTS idx_qj_status ON queue_jobs(status, priority, created_at);

        CREATE TABLE IF NOT EXISTS spans (
          id                TEXT PRIMARY KEY,
          trace_id          TEXT NOT NULL,
          parent_id         TEXT,
          name              TEXT NOT NULL,
          profile           TEXT,
          model_id          TEXT,
          started_at        TEXT NOT NULL,
          finished_at       TEXT,
          duration_ms       INTEGER,
          status            TEXT NOT NULL DEFAULT 'ok',
          prompt_tokens     INTEGER NOT NULL DEFAULT 0,
          completion_tokens INTEGER NOT NULL DEFAULT 0,
          attributes        TEXT NOT NULL DEFAULT '{}',
          error_msg         TEXT
        );
        CREATE INDEX IF NOT EXISTS idx_spans_trace ON spans(trace_id, started_at);

        CREATE TABLE IF NOT EXISTS events (
          id          TEXT PRIMARY KEY,
          event_type  TEXT NOT NULL,
          profile     TEXT,
          run_id      TEXT,
          payload     TEXT NOT NULL DEFAULT '{}',
          created_at  TEXT NOT NULL
        );
        CREATE INDEX IF NOT EXISTS idx_events_type ON events(event_type, created_at);

        CREATE TABLE IF NOT EXISTS hook_log (
          id          TEXT PRIMARY KEY,
          hook_name   TEXT NOT NULL,
          event_id    TEXT NOT NULL,
          status      TEXT NOT NULL DEFAULT 'ok',
          response    TEXT,
          fired_at    TEXT NOT NULL
        );
        CREATE INDEX IF NOT EXISTS idx_hook_log_name ON hook_log(hook_name, fired_at);

        CREATE TABLE IF NOT EXISTS benchmark_comparisons (
          id          TEXT PRIMARY KEY,
          suite_path  TEXT NOT NULL,
          models      TEXT NOT NULL DEFAULT '[]',
          judge_model TEXT,
          created_at  TEXT NOT NULL,
          status      TEXT NOT NULL DEFAULT 'running'
        );

        CREATE TABLE IF NOT EXISTS benchmark_results (
          id                TEXT PRIMARY KEY,
          comparison_id     TEXT NOT NULL,
          model_id          TEXT NOT NULL,
          case_id           TEXT NOT NULL,
          output            TEXT,
          passed            INTEGER,
          quality_score     REAL,
          latency_ms        INTEGER,
          prompt_tokens     INTEGER,
          completion_tokens INTEGER,
          cost_usd          REAL,
          error             TEXT,
          created_at        TEXT NOT NULL
        );
        CREATE INDEX IF NOT EXISTS idx_br_comparison ON benchmark_results(comparison_id, model_id);
        """
    )
    _migrate_runs_cost_columns(conn)
    _migrate_prd_021_032_tables(conn)
    _migrate_prd_033_044_tables(conn)
    _migrate_swarm_tables(conn)
    _prune_old_spans(conn)
    return conn


def _migrate_swarm_tables(conn: sqlite3.Connection) -> None:
    """PRD-023: Context-centric swarm tables (idempotent)."""
    try:
        from tag.swarm import migrate_swarm_tables  # noqa: PLC0415
        migrate_swarm_tables(conn)
    except Exception:
        pass


def _migrate_prd_021_032_tables(conn: sqlite3.Connection) -> None:
    """Create tables for PRD-021 through PRD-032 features (idempotent)."""
    try:
        conn.executescript("""
            -- PRD-021: Agent Loop
            CREATE TABLE IF NOT EXISTS loop_runs (
              id           TEXT PRIMARY KEY,
              profile      TEXT NOT NULL,
              goal         TEXT NOT NULL,
              max_iters    INTEGER NOT NULL DEFAULT 10,
              current_iter INTEGER NOT NULL DEFAULT 0,
              status       TEXT NOT NULL DEFAULT 'running',
              approval     TEXT NOT NULL DEFAULT 'auto',
              created_at   TEXT NOT NULL,
              updated_at   TEXT NOT NULL,
              completed_at TEXT
            );
            CREATE INDEX IF NOT EXISTS idx_lr_status ON loop_runs(status, created_at);

            CREATE TABLE IF NOT EXISTS loop_iterations (
              id         TEXT PRIMARY KEY,
              loop_id    TEXT NOT NULL,
              iteration  INTEGER NOT NULL,
              input      TEXT NOT NULL,
              output     TEXT NOT NULL DEFAULT '',
              decision   TEXT NOT NULL DEFAULT 'continue',
              created_at TEXT NOT NULL,
              FOREIGN KEY(loop_id) REFERENCES loop_runs(id)
            );
            CREATE INDEX IF NOT EXISTS idx_li_loop ON loop_iterations(loop_id, iteration);

            -- PRD-022: Cron
            CREATE TABLE IF NOT EXISTS cron_jobs (
              id          TEXT PRIMARY KEY,
              name        TEXT NOT NULL,
              schedule    TEXT NOT NULL,
              profile     TEXT NOT NULL,
              task        TEXT NOT NULL,
              enabled     INTEGER NOT NULL DEFAULT 1,
              last_run_at TEXT,
              run_count   INTEGER NOT NULL DEFAULT 0,
              created_at  TEXT NOT NULL,
              updated_at  TEXT NOT NULL
            );
            CREATE INDEX IF NOT EXISTS idx_cj_enabled ON cron_jobs(enabled, name);

            -- PRD-024: Workspace
            CREATE TABLE IF NOT EXISTS workspace_files (
              path         TEXT PRIMARY KEY,
              content_hash TEXT NOT NULL,
              byte_size    INTEGER NOT NULL DEFAULT 0,
              token_count  INTEGER NOT NULL DEFAULT 0,
              rank         REAL NOT NULL DEFAULT 0.0,
              indexed_at   TEXT NOT NULL
            );
            CREATE INDEX IF NOT EXISTS idx_wf_rank ON workspace_files(rank DESC);

            -- PRD-025: Semantic Memory
            CREATE TABLE IF NOT EXISTS semantic_memories (
              id           TEXT PRIMARY KEY,
              profile      TEXT NOT NULL,
              content      TEXT NOT NULL,
              memory_type  TEXT NOT NULL DEFAULT 'fact',
              confidence   REAL NOT NULL DEFAULT 1.0,
              created_at   TEXT NOT NULL,
              accessed_at  TEXT NOT NULL,
              access_count INTEGER NOT NULL DEFAULT 0,
              source       TEXT NOT NULL DEFAULT 'manual'
            );
            CREATE INDEX IF NOT EXISTS idx_sm_profile ON semantic_memories(profile, memory_type);

            -- PRD-026: Profile Marketplace
            CREATE TABLE IF NOT EXISTS profile_cache (
              id           TEXT PRIMARY KEY,
              name         TEXT NOT NULL,
              source_url   TEXT NOT NULL,
              sha256       TEXT NOT NULL,
              local_path   TEXT NOT NULL,
              downloaded_at TEXT NOT NULL,
              UNIQUE(name)
            );

            -- PRD-027: Eval Framework
            CREATE TABLE IF NOT EXISTS eval_runs (
              id           TEXT PRIMARY KEY,
              suite_path   TEXT NOT NULL,
              profile      TEXT NOT NULL,
              suite_name   TEXT NOT NULL DEFAULT '',
              status       TEXT NOT NULL DEFAULT 'running',
              pass_count   INTEGER NOT NULL DEFAULT 0,
              fail_count   INTEGER NOT NULL DEFAULT 0,
              total_count  INTEGER NOT NULL DEFAULT 0,
              created_at   TEXT NOT NULL,
              completed_at TEXT
            );
            CREATE INDEX IF NOT EXISTS idx_er_created ON eval_runs(created_at DESC);

            CREATE TABLE IF NOT EXISTS eval_cases (
              id             TEXT PRIMARY KEY,
              eval_run_id    TEXT NOT NULL,
              case_id        TEXT NOT NULL,
              input          TEXT NOT NULL,
              output         TEXT NOT NULL DEFAULT '',
              passed         INTEGER NOT NULL DEFAULT 0,
              score          REAL NOT NULL DEFAULT 0.0,
              failure_reason TEXT,
              created_at     TEXT NOT NULL,
              FOREIGN KEY(eval_run_id) REFERENCES eval_runs(id)
            );
            CREATE INDEX IF NOT EXISTS idx_ec_run ON eval_cases(eval_run_id);

            -- PRD-028: Sandbox
            CREATE TABLE IF NOT EXISTS sandbox_runs (
              id           TEXT PRIMARY KEY,
              command      TEXT NOT NULL,
              backend      TEXT NOT NULL DEFAULT 'restricted',
              image        TEXT,
              status       TEXT NOT NULL DEFAULT 'running',
              exit_code    INTEGER,
              output       TEXT NOT NULL DEFAULT '',
              error        TEXT,
              created_at   TEXT NOT NULL,
              completed_at TEXT
            );
            CREATE INDEX IF NOT EXISTS idx_sr_created ON sandbox_runs(created_at DESC);

            -- PRD-031: Route Fallback Chains
            CREATE TABLE IF NOT EXISTS route_fallbacks (
              id             TEXT PRIMARY KEY,
              profile        TEXT NOT NULL,
              primary_model  TEXT NOT NULL,
              fallback_model TEXT NOT NULL,
              condition      TEXT NOT NULL DEFAULT 'context_overflow',
              priority       INTEGER NOT NULL DEFAULT 1,
              enabled        INTEGER NOT NULL DEFAULT 1,
              created_at     TEXT NOT NULL
            );
            CREATE INDEX IF NOT EXISTS idx_rf_profile ON route_fallbacks(profile, enabled);

            -- PRD-032: Trace Snapshots
            CREATE TABLE IF NOT EXISTS trace_snapshots (
              id           TEXT PRIMARY KEY,
              trace_id     TEXT NOT NULL,
              step_index   INTEGER NOT NULL DEFAULT 0,
              snapshot_json TEXT NOT NULL DEFAULT '{}',
              created_at   TEXT NOT NULL
            );
            CREATE INDEX IF NOT EXISTS idx_ts_trace ON trace_snapshots(trace_id, step_index);
        """)
        conn.commit()
    except sqlite3.OperationalError:
        pass

    # Migrate: add cache token columns to runs table (PRD-030)
    existing_cols = {row[1] for row in conn.execute("PRAGMA table_info(runs)")}
    for col, typedef in [
        ("cache_read_tokens", "INTEGER DEFAULT 0"),
        ("cache_creation_tokens", "INTEGER DEFAULT 0"),
    ]:
        if col not in existing_cols:
            try:
                conn.execute(f"ALTER TABLE runs ADD COLUMN {col} {typedef}")
            except sqlite3.OperationalError:
                pass
    conn.commit()


def _migrate_prd_033_044_tables(conn: sqlite3.Connection) -> None:
    """Create tables for PRD-033 through PRD-044 features (idempotent)."""
    try:
        conn.executescript("""
            -- PRD-033: DAG / dependency-aware queue
            CREATE TABLE IF NOT EXISTS queue_dags (
              id          TEXT PRIMARY KEY,
              name        TEXT NOT NULL UNIQUE,
              spec_json   TEXT NOT NULL,
              created_at  TEXT NOT NULL
            );
            CREATE TABLE IF NOT EXISTS _dag_init_sentinel (id INTEGER PRIMARY KEY);

            -- PRD-034: Secret Scanning
            CREATE TABLE IF NOT EXISTS security_scans (
              id          TEXT PRIMARY KEY,
              scanned_path TEXT NOT NULL,
              finding_count INTEGER NOT NULL DEFAULT 0,
              status      TEXT NOT NULL DEFAULT 'ok',
              created_at  TEXT NOT NULL
            );
            CREATE TABLE IF NOT EXISTS security_findings (
              id          TEXT PRIMARY KEY,
              scan_id     TEXT NOT NULL,
              file_path   TEXT NOT NULL,
              line_no     INTEGER NOT NULL,
              pattern_name TEXT NOT NULL,
              is_entropy  INTEGER NOT NULL DEFAULT 0,
              created_at  TEXT NOT NULL,
              FOREIGN KEY(scan_id) REFERENCES security_scans(id)
            );
            CREATE INDEX IF NOT EXISTS idx_sf_scan ON security_findings(scan_id);

            -- PRD-035: LSP sessions
            CREATE TABLE IF NOT EXISTS lsp_sessions (
              id          TEXT PRIMARY KEY,
              transport   TEXT NOT NULL DEFAULT 'stdio',
              port        INTEGER,
              pid         INTEGER,
              status      TEXT NOT NULL DEFAULT 'running',
              created_at  TEXT NOT NULL,
              stopped_at  TEXT
            );

            -- PRD-037: Personas
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

            -- PRD-039: Token Budgets
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

            -- PRD-040: Notification Hooks
            CREATE TABLE IF NOT EXISTS notification_hooks (
              id           TEXT PRIMARY KEY,
              profile      TEXT,
              event        TEXT NOT NULL,
              channel      TEXT NOT NULL,
              config_json  TEXT NOT NULL DEFAULT '{}',
              template     TEXT NOT NULL DEFAULT '',
              enabled      INTEGER NOT NULL DEFAULT 1,
              created_at   TEXT NOT NULL
            );
            CREATE INDEX IF NOT EXISTS idx_nh_event ON notification_hooks(event, enabled);
            CREATE TABLE IF NOT EXISTS notification_log (
              id           TEXT PRIMARY KEY,
              hook_id      TEXT NOT NULL,
              event        TEXT NOT NULL,
              channel      TEXT NOT NULL,
              outcome      TEXT NOT NULL,
              http_status  INTEGER,
              attempt      INTEGER NOT NULL DEFAULT 1,
              created_at   TEXT NOT NULL,
              FOREIGN KEY(hook_id) REFERENCES notification_hooks(id)
            );

            -- PRD-042: Architect/Editor Split
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

            -- PRD-043: Tool index metadata
            CREATE TABLE IF NOT EXISTS tool_index_meta (
              id          TEXT PRIMARY KEY DEFAULT 'singleton',
              registry_mtime REAL,
              tool_count  INTEGER NOT NULL DEFAULT 0,
              built_at    TEXT NOT NULL
            );

            -- PRD-044: AgentOps sessions
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
    except sqlite3.OperationalError:
        pass

    # Add deps_json to queue_jobs if not present (PRD-033)
    cols = {r[1] for r in conn.execute("PRAGMA table_info(queue_jobs)").fetchall()}
    if "deps_json" not in cols:
        try:
            conn.execute("ALTER TABLE queue_jobs ADD COLUMN deps_json TEXT DEFAULT '[]'")
            conn.commit()
        except sqlite3.OperationalError:
            pass


def _migrate_runs_cost_columns(conn: sqlite3.Connection) -> None:
    """Add cost/token columns to runs table if they don't exist yet."""
    cursor = conn.execute("PRAGMA table_info(runs)")
    existing = {row[1] for row in cursor.fetchall()}
    migrations = [
        ("prompt_tokens",     "INTEGER DEFAULT 0"),
        ("completion_tokens", "INTEGER DEFAULT 0"),
        ("total_tokens",      "INTEGER DEFAULT 0"),
        ("estimated_cost_usd","REAL"),
        ("model_id",          "TEXT"),
        ("provider",          "TEXT"),
    ]
    for col, typedef in migrations:
        if col not in existing:
            try:
                conn.execute(f"ALTER TABLE runs ADD COLUMN {col} {typedef}")
            except sqlite3.OperationalError:
                pass
    conn.commit()


def _prune_old_spans(conn: sqlite3.Connection, days: int = 30) -> None:
    """Remove spans older than `days` days to keep the DB size bounded."""
    import datetime as _dt
    cutoff = (_dt.datetime.now(_dt.timezone.utc) - _dt.timedelta(days=days)).isoformat()
    try:
        conn.execute("DELETE FROM spans WHERE started_at < ?", (cutoff,))
        conn.commit()
    except sqlite3.OperationalError:
        pass


# ---------------------------------------------------------------------------
# PRD-002: Memory journal helpers
# ---------------------------------------------------------------------------

def journal_save(
    db: sqlite3.Connection,
    profile: str,
    key: str,
    value: str,
    *,
    ttl_days: int | None = None,
) -> str:
    """Upsert a key→value fact. Returns the row id."""
    # Strip null bytes — they corrupt display and are never meaningful in text facts
    key = key.replace("\x00", "").strip()
    value = value.replace("\x00", "")
    if not key:
        raise ValueError("journal key must not be empty")
    entry_id = uuid.uuid4().hex[:12]
    now = utc_now()
    expires_at: str | None = None
    if ttl_days is not None:
        if ttl_days <= 0:
            raise ValueError(f"ttl_days must be positive, got {ttl_days}")
        import datetime
        expires_dt = datetime.datetime.now(datetime.timezone.utc) + datetime.timedelta(days=ttl_days)
        expires_at = expires_dt.isoformat()
    db.execute(
        """INSERT INTO memory_journal(id, profile, key, value, scope, created_at, expires_at)
           VALUES(?,?,?,?,'profile',?,?)
           ON CONFLICT(profile, key) DO UPDATE SET
             value=excluded.value, expires_at=excluded.expires_at, created_at=excluded.created_at""",
        (entry_id, profile, key, value, now, expires_at),
    )
    db.commit()
    existing = db.execute(
        "SELECT id FROM memory_journal WHERE profile=? AND key=?", (profile, key)
    ).fetchone()
    return existing[0] if existing else entry_id


def journal_list(
    db: sqlite3.Connection,
    profile: str,
    *,
    include_global: bool = True,
) -> list[dict[str, Any]]:
    """Return all non-expired entries for profile (plus global if include_global)."""
    now = utc_now()
    if include_global:
        rows = db.execute(
            """SELECT id, profile, key, value, scope, created_at, expires_at
               FROM memory_journal
               WHERE (profile=? OR profile='*')
                 AND (expires_at IS NULL OR expires_at > ?)
               ORDER BY created_at""",
            (profile, now),
        ).fetchall()
    else:
        rows = db.execute(
            """SELECT id, profile, key, value, scope, created_at, expires_at
               FROM memory_journal
               WHERE profile=?
                 AND (expires_at IS NULL OR expires_at > ?)
               ORDER BY created_at""",
            (profile, now),
        ).fetchall()
    return [dict(r) for r in rows]


def journal_forget(db: sqlite3.Connection, entry_id: str) -> bool:
    """Delete entry by id. Returns True if deleted."""
    cursor = db.execute("DELETE FROM memory_journal WHERE id=?", (entry_id,))
    db.commit()
    return cursor.rowcount > 0


def journal_clear(db: sqlite3.Connection, profile: str) -> int:
    """Delete all non-global entries for profile. Returns count deleted."""
    cursor = db.execute(
        "DELETE FROM memory_journal WHERE profile=? AND profile != '*'", (profile,)
    )
    db.commit()
    return cursor.rowcount


def journal_to_prompt_prefix(db: sqlite3.Connection, profile: str) -> str | None:
    """Format non-expired journal entries as a system-prompt injection block."""
    entries = journal_list(db, profile)
    if not entries:
        return None
    lines = ["## Persistent Context (TAG Memory Journal)"]
    for e in entries:
        lines.append(f"- {e['key']}: {e['value']}")
    return "\n".join(lines)


# ---------------------------------------------------------------------------
# PRD-008: Queue job helpers
# ---------------------------------------------------------------------------

def queue_insert_job(
    db: sqlite3.Connection,
    job_id: str,
    profile: str,
    task: str,
    *,
    task_type: str = "mixed",
    priority: int = 5,
    notify: bool = True,
) -> None:
    db.execute(
        """INSERT INTO queue_jobs(id, profile, task, task_type, status, priority, created_at, notify)
           VALUES(?,?,?,?,'queued',?,?,?)""",
        (job_id, profile, task, task_type, priority, utc_now(), 1 if notify else 0),
    )
    db.commit()


def queue_update_pid(db: sqlite3.Connection, job_id: str, pid: int) -> None:
    db.execute("UPDATE queue_jobs SET pid=? WHERE id=?", (pid, job_id))
    db.commit()


def queue_update_status(db: sqlite3.Connection, job_id: str, status: str) -> None:
    db.execute(
        "UPDATE queue_jobs SET status=?, finished_at=? WHERE id=?",
        (status, utc_now(), job_id),
    )
    db.commit()


def queue_get_job(db: sqlite3.Connection, job_id: str) -> dict[str, Any] | None:
    row = db.execute("SELECT * FROM queue_jobs WHERE id=?", (job_id,)).fetchone()
    return dict(row) if row else None


def queue_list_jobs(
    db: sqlite3.Connection,
    *,
    status: str | None = None,
    profile: str | None = None,
    limit: int = 50,
) -> list[dict[str, Any]]:
    query = "SELECT * FROM queue_jobs WHERE 1=1"
    params: list[Any] = []
    if status:
        query += " AND status=?"
        params.append(status)
    if profile:
        query += " AND profile=?"
        params.append(profile)
    query += " ORDER BY created_at DESC LIMIT ?"
    params.append(limit)
    return [dict(r) for r in db.execute(query, params).fetchall()]


def queue_clear_completed(db: sqlite3.Connection) -> int:
    cursor = db.execute(
        "DELETE FROM queue_jobs WHERE status IN ('done','failed','cancelled')"
    )
    db.commit()
    return cursor.rowcount


def launch_queue_worker(cfg: dict[str, Any], job_id: str) -> int:
    """Launch queue worker as a detached process. Returns PID."""
    python = sys.executable
    config_arg = str(config_path(None))
    db_arg = str(runtime_db_path(cfg))
    cmd = [
        python,
        "-m",
        "tag.queue_worker",
        "--job-id",
        job_id,
        "--config",
        config_arg,
        "--db",
        db_arg,
    ]
    proc = subprocess.Popen(
        cmd,
        start_new_session=True,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
        close_fds=True,
    )
    return proc.pid
