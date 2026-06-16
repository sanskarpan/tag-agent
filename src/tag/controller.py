#!/usr/bin/env python3
"""TAG control-plane CLI built on top of Hermes."""

from __future__ import annotations

import argparse
import datetime as dt
import hashlib
import json
import os
import re
import shutil
import sqlite3
import subprocess
import sys
import tarfile
import textwrap
import time
import uuid
import urllib.error
import urllib.request
from concurrent.futures import ThreadPoolExecutor, as_completed
from pathlib import Path
from pathlib import PurePosixPath
from typing import Any, TextIO

import yaml

try:
    from tag import __version__
except Exception:  # pragma: no cover - fallback for direct file loading in tests
    __version__ = "0.1.0"

try:
    from tag.tui_output import (
        chat_spinner,
        get_console,
        make_benchmark_progress,
        make_submit_progress,
        print_doctor_report,
        print_error,
        print_success,
        print_warning,
        send_desktop_notification,
    )

    _TUI_OUTPUT_AVAILABLE = True
except Exception:  # pragma: no cover — tui_output not importable in all test environments
    _TUI_OUTPUT_AVAILABLE = False

    def get_console():  # type: ignore[misc]
        return None

    def print_error(msg: str) -> None:  # type: ignore[misc]
        print(f"error: {msg}", file=sys.stderr)

    def print_success(msg: str) -> None:  # type: ignore[misc]
        print(msg)

    def print_warning(msg: str) -> None:  # type: ignore[misc]
        print(f"warning: {msg}", file=sys.stderr)

    def print_doctor_report(groups: dict) -> None:  # type: ignore[misc]
        for group, checks in groups.items():
            print(f"\n{group.upper()}")
            for c in checks:
                st = c.get("status", "pass")
                icon = {"pass": "✓", "warn": "⚠", "fail": "✗"}.get(st, "?")
                print(f"  {icon} {c.get('name','?'):<28} {c.get('message','')}")

    def send_desktop_notification(title: str, message: str) -> None:  # type: ignore[misc]
        pass

    def chat_spinner(profile: str, model: str):  # type: ignore[misc]
        import contextlib
        return contextlib.nullcontext()

    def make_benchmark_progress():  # type: ignore[misc]
        return None

    def make_submit_progress():  # type: ignore[misc]
        return None

APP_NAME = "TAG"
CLI_LABEL = "tag"
DEFAULT_TAG_HOME = Path("~/.tag").expanduser()
DEFAULT_HERMES_CHECKOUT = "managed/hermes-agent-upstream"
MIN_PYTHON = (3, 11)
MAX_PYTHON_EXCLUSIVE = (3, 14)


def package_root() -> Path:
    return Path(__file__).resolve().parent


def resource_path(*parts: str) -> Path:
    return package_root().joinpath(*parts)


def bundled_hermes_archive() -> Path:
    return resource_path("vendor", "hermes-agent-upstream.tar.gz")


def python_runtime_supported(version_info: tuple[int, int]) -> bool:
    return MIN_PYTHON <= version_info < MAX_PYTHON_EXCLUSIVE


def hermes_checkout_kind(root: Path) -> str:
    if not root.exists():
        return "missing"
    if (root / ".git").exists():
        return "git"
    return "bundled"


def is_hermes_checkout(root: Path) -> bool:
    return root.exists() and (root / "pyproject.toml").exists() and (root / "ui-tui" / "package.json").exists()


def discover_local_hermes_checkout() -> Path | None:
    candidates: list[Path] = []
    cwd = Path.cwd().resolve()
    candidates.extend([cwd / "hermes-agent-upstream", cwd.parent / "hermes-agent-upstream"])
    package_candidates = [
        package_root().parents[2] / "hermes-agent-upstream",
        package_root().parents[3] / "hermes-agent-upstream" if len(package_root().parents) > 3 else None,
    ]
    candidates.extend(candidate for candidate in package_candidates if candidate is not None)
    hermes_exec = shutil.which("hermes")
    if hermes_exec:
        exec_path = Path(hermes_exec).resolve()
        if len(exec_path.parents) >= 3:
            candidates.append(exec_path.parents[2])
    seen: set[Path] = set()
    for candidate in candidates:
        resolved = candidate.resolve()
        if resolved in seen:
            continue
        seen.add(resolved)
        if is_hermes_checkout(resolved):
            return resolved
    return None


def tag_home() -> Path:
    return Path(os.environ.get("TAG_HOME", str(DEFAULT_TAG_HOME))).expanduser().resolve()


def tag_cli_label() -> str:
    return os.environ.get("TAG_CLI_LABEL", CLI_LABEL).strip() or CLI_LABEL


def tag_cli_bin() -> str:
    override = os.environ.get("TAG_BIN", "").strip()
    if override:
        return override
    argv0 = Path(sys.argv[0]).expanduser()
    if argv0.exists():
        return str(argv0.resolve())
    found = shutil.which(tag_cli_label())
    if found:
        return found
    return tag_cli_label()


def resolve_home_relative(value: str, *, base: Path | None = None) -> Path:
    raw = Path(value).expanduser()
    if raw.is_absolute():
        return raw.resolve()
    return ((base or tag_home()) / raw).resolve()


def ensure_default_file(target: Path, source: Path) -> Path:
    if target.exists():
        return target
    try:
        target.parent.mkdir(parents=True, exist_ok=True)
        shutil.copy2(source, target)
    except (PermissionError, NotADirectoryError) as exc:
        raise SystemExit(f"Cannot initialize TAG file {target}: {exc.strerror or exc}") from exc
    return target


def is_tty(stream: TextIO | None) -> bool:
    try:
        return bool(stream and stream.isatty())
    except Exception:
        return False


def can_launch_interactive_tui(
    stdin: TextIO | None = None,
    stdout: TextIO | None = None,
    stderr: TextIO | None = None,
) -> bool:
    return is_tty(stdin or sys.stdin) and (is_tty(stdout or sys.stdout) or is_tty(stderr or sys.stderr))


def config_root() -> Path:
    return tag_home() / "config"


def managed_root() -> Path:
    return tag_home() / "managed"


def hermes_root(cfg: dict[str, Any] | None = None) -> Path:
    override = os.environ.get("TAG_HERMES_ROOT", "").strip()
    if override:
        return Path(override).expanduser().resolve()
    configured = resolve_home_relative(
        str(cfg.get("upstream", {}).get("checkout_dir", DEFAULT_HERMES_CHECKOUT))
        if cfg is not None
        else DEFAULT_HERMES_CHECKOUT
    )
    if configured.exists():
        return configured
    discovered = discover_local_hermes_checkout()
    if discovered is not None:
        return discovered
    return configured


def hermes_bin(cfg: dict[str, Any] | None = None) -> Path:
    return hermes_root(cfg) / ".venv" / "bin" / "hermes"


def config_path(arg_value: str | None) -> Path:
    if arg_value:
        return Path(arg_value).expanduser().resolve()
    return ensure_default_file(config_root() / "tag.yaml", resource_path("config", "default.yaml"))


def benchmark_suite_path(arg_value: str | None) -> Path:
    if arg_value:
        return Path(arg_value).expanduser().resolve()
    return ensure_default_file(
        config_root() / "benchmark-suite.yaml",
        resource_path("config", "benchmark-suite.yaml"),
    )


def load_config(path: Path) -> dict[str, Any]:
    try:
        with path.open("r", encoding="utf-8-sig") as fh:
            data = yaml.safe_load(fh) or {}
    except FileNotFoundError:
        raise SystemExit(f"Config file not found: {path}")
    if not isinstance(data, dict):
        raise SystemExit(f"Config at {path} must be a YAML object.")
    return data


def save_config(path: Path, payload: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("w", encoding="utf-8") as fh:
        yaml.safe_dump(payload, fh, sort_keys=False)


def utc_now() -> str:
    return dt.datetime.now(dt.timezone.utc).isoformat()


def runtime_home(cfg: dict[str, Any]) -> Path:
    value = cfg.get("runtime", {}).get("home_dir", "runtime/home")
    return resolve_home_relative(str(value))


def runtime_codex_home(cfg: dict[str, Any]) -> Path:
    override = os.environ.get("TAG_CODEX_HOME")
    if override:
        return Path(override).expanduser().resolve()
    value = cfg.get("runtime", {}).get("codex_home", "runtime/home/.codex")
    return resolve_home_relative(str(value))


def runtime_db_path(cfg: dict[str, Any]) -> Path:
    value = cfg.get("runtime", {}).get("db_path", "runtime/tag.sqlite3")
    return resolve_home_relative(str(value))


def hermes_repo_url(cfg: dict[str, Any]) -> str:
    return str(
        os.environ.get(
            "TAG_HERMES_REPO",
            cfg.get("upstream", {}).get("repo", "https://github.com/NousResearch/Hermes-Agent.git"),
        )
    )


def hermes_ref(cfg: dict[str, Any]) -> str:
    return str(os.environ.get("TAG_HERMES_REF", cfg.get("upstream", {}).get("ref", "main")))


def hermes_env(cfg: dict[str, Any]) -> dict[str, str]:
    home_dir = runtime_home(cfg)
    hhome = Path(os.environ.get("TAG_HERMES_HOME", home_dir / ".hermes"))
    codex_home = runtime_codex_home(cfg)
    tui_dir = hermes_root(cfg) / "ui-tui"
    env = os.environ.copy()
    env["HOME"] = str(home_dir)
    env["HERMES_HOME"] = str(hhome)
    env["CODEX_HOME"] = str(codex_home)
    env["HERMES_BIN"] = tag_cli_bin()
    env["HERMES_BIN_LABEL"] = tag_cli_label()
    env["HERMES_ENV_LABEL"] = "the active TAG profile env file"
    env["HERMES_TUI_DIR"] = str(tui_dir)
    env["PATH"] = f"{hermes_root(cfg) / '.venv' / 'bin'}:{env.get('PATH', '')}"
    return env


def profile_exec_env(cfg: dict[str, Any], profile_name: str) -> dict[str, str]:
    env = hermes_env(cfg)
    real_home = os.environ.get("HOME", "")
    passthrough_profiles = {
        item.strip()
        for item in os.environ.get(
            "TAG_PASSTHROUGH_HOME_PROFILES", "codex-runtime-master"
        ).split(",")
        if item.strip()
    }
    if profile_name in passthrough_profiles:
        env["HOME"] = os.environ.get(
            "TAG_REAL_HOME", real_home or str(runtime_home(cfg))
        )
        env["CODEX_HOME"] = os.environ.get(
            "TAG_CODEX_HOME", str(Path(real_home).expanduser() / ".codex") if real_home else str(runtime_codex_home(cfg))
        )
    env["HERMES_HOME"] = str(profile_home(cfg, profile_name))
    for key, value in read_dotenv(profile_home(cfg, profile_name) / ".env").items():
        env[key] = value
    # PRD-002: inject memory journal as system prompt prefix when DB exists
    db_path = runtime_db_path(cfg)
    if db_path.exists():
        try:
            _db = sqlite3.connect(str(db_path), timeout=2)
            _db.row_factory = sqlite3.Row
            prefix = journal_to_prompt_prefix(_db, profile_name)
            _db.close()
            if prefix:
                env["HERMES_SYSTEM_INJECT"] = prefix
        except Exception:  # pragma: no cover — DB not yet initialised or locked
            pass
    return env


def ensure_runtime_dirs(cfg: dict[str, Any]) -> None:
    env = hermes_env(cfg)
    Path(env["HOME"]).mkdir(parents=True, exist_ok=True)
    Path(env["HERMES_HOME"]).mkdir(parents=True, exist_ok=True)
    Path(env["CODEX_HOME"]).mkdir(parents=True, exist_ok=True)
    runtime_db_path(cfg).parent.mkdir(parents=True, exist_ok=True)


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
    _prune_old_spans(conn)
    return conn


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
# PRD-002: Memory Journal helpers
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


def run_hermes(cfg: dict[str, Any], *args: str, check: bool = True) -> subprocess.CompletedProcess[str]:
    ensure_runtime_dirs(cfg)
    return subprocess.run(
        [str(hermes_bin(cfg)), *args],
        env=hermes_env(cfg),
        text=True,
        capture_output=True,
        check=check,
    )


def run_profile_hermes(
    cfg: dict[str, Any],
    profile_name: str,
    *args: str,
    check: bool = True,
    timeout: int | None = None,
) -> subprocess.CompletedProcess[str]:
    ensure_runtime_dirs(cfg)
    return subprocess.run(
        [str(hermes_bin(cfg)), *args],
        env=profile_exec_env(cfg, profile_name),
        text=True,
        capture_output=True,
        check=check,
        timeout=timeout,
    )


def profile_home(cfg: dict[str, Any], profile_name: str) -> Path:
    return Path(hermes_env(cfg)["HERMES_HOME"]) / "profiles" / profile_name


def read_dotenv(path: Path) -> dict[str, str]:
    if not path.exists():
        return {}
    values: dict[str, str] = {}
    for raw_line in path.read_text(encoding="utf-8").splitlines():
        line = raw_line.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        key, value = line.split("=", 1)
        values[key.strip()] = value.strip()
    return values


def _sanitize_env_value(value: str) -> str:
    """Strip characters that would break .env line format or enable injection.

    Newlines would create additional KEY=VALUE entries; null bytes corrupt
    the file on some platforms. Strip both. Callers should validate further
    if they expect a specific format (e.g. URL, token).
    """
    return value.replace("\r\n", " ").replace("\n", " ").replace("\r", " ").replace("\x00", "")


def _upsert_env_line(env_file: Path, key: str, value: str) -> None:
    """Write or replace KEY=VALUE in an .env file without disturbing other lines."""
    value = _sanitize_env_value(value)
    env_file.parent.mkdir(parents=True, exist_ok=True)
    lines = env_file.read_text(encoding="utf-8").splitlines() if env_file.exists() else []
    prefix = f"{key}="
    new_line = f"{key}={value}"
    replaced = False
    out = []
    for line in lines:
        stripped = line.strip()
        if stripped.startswith(prefix) or stripped.lstrip("# ").startswith(prefix):
            out.append(new_line)
            replaced = True
        else:
            out.append(line)
    if not replaced:
        out.append(new_line)
    env_file.write_text("\n".join(out) + "\n", encoding="utf-8")


def run_profile_python(
    cfg: dict[str, Any],
    profile_name: str,
    inline: str,
    *,
    check: bool = True,
) -> subprocess.CompletedProcess[str]:
    ensure_runtime_dirs(cfg)
    return subprocess.run(
        [str(hermes_root(cfg) / ".venv" / "bin" / "python"), "-c", inline],
        env=profile_exec_env(cfg, profile_name),
        text=True,
        capture_output=True,
        check=check,
    )


def write_yaml(path: Path, payload: dict[str, Any], force: bool) -> None:
    if path.exists() and not force:
        return
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("w", encoding="utf-8") as fh:
        yaml.safe_dump(payload, fh, sort_keys=False)


def write_text(path: Path, content: str, force: bool) -> None:
    if path.exists() and not force:
        return
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(content, encoding="utf-8")


def slugify(text: str) -> str:
    slug = re.sub(r"[^a-z0-9]+", "-", text.lower()).strip("-")
    return slug[:48] or "run"


def normalize_chat_output(output: str) -> str:
    cleaned: list[str] = []
    for line in output.splitlines():
        if line.strip().startswith("session_id:"):
            continue
        if "tirith security scanner enabled but not available" in line.lower():
            continue
        cleaned.append(line)
    return "\n".join(cleaned).strip()


def rewrite_cli_hints(text: str) -> str:
    if not text:
        return text
    label = tag_cli_label()

    def replace_inner(inner: str) -> str:
        return re.sub(r"\bhermes\b", label, inner, flags=re.IGNORECASE)

    rewritten = re.sub(
        r"`([^`\n]*\bhermes\b[^`\n]*)`",
        lambda match: f"`{replace_inner(match.group(1))}`",
        text,
        flags=re.IGNORECASE,
    )
    rewritten = re.sub(
        r"'([^'\n]*\bhermes\b[^'\n]*)'",
        lambda match: f"'{replace_inner(match.group(1))}'",
        rewritten,
        flags=re.IGNORECASE,
    )
    rewritten = re.sub(
        r"\bhermes (?=(auth|config|model|setup|update|gateway|sessions|doctor|tools|portal|status|plugins|skills|mcp|logs|memory|completion|prompt-size|chat|--resume|-c)\b)",
        f"{label} ",
        rewritten,
        flags=re.IGNORECASE,
    )
    rewritten = re.sub(r"\bHermes/tag\b", "TAG", rewritten, flags=re.IGNORECASE)
    rewritten = re.sub(r"\btag/tag\b", "tag", rewritten, flags=re.IGNORECASE)
    rewritten = re.sub(r"\bHermes Agent\b", "TAG", rewritten, flags=re.IGNORECASE)
    rewritten = re.sub(r"\bhermes-agent\b", "tag-agent", rewritten, flags=re.IGNORECASE)
    rewritten = re.sub(r"\bthis Hermes profile\b", "this TAG profile", rewritten, flags=re.IGNORECASE)
    rewritten = re.sub(r"\bActive Hermes profile\b", "Active TAG profile", rewritten, flags=re.IGNORECASE)
    rewritten = re.sub(r"\bHermes profile\b", "TAG profile", rewritten, flags=re.IGNORECASE)
    rewritten = rewritten.replace("~/.hermes/.env", "the active TAG profile env file")
    return rewritten


def infrastructure_failure_reason(output: str) -> str | None:
    normalized = normalize_chat_output(output)
    lowered = normalized.lower()
    known_failures = (
        "error: codex authentication failed",
        "login looks expired or invalid",
        "api call failed after",
        "no api keys or providers found",
        "it looks like the managed runtime isn't configured yet",
        "it looks like hermes isn't configured yet",
    )
    for marker in known_failures:
        if marker in lowered:
            return marker
    return None


def strip_json_fences(text: str) -> str:
    stripped = text.strip()
    if stripped.startswith("```") and stripped.endswith("```"):
        lines = stripped.splitlines()
        if len(lines) >= 3:
            return "\n".join(lines[1:-1]).strip()
    return stripped


def merged_env_example(cfg: dict[str, Any], profile_name: str) -> str:
    env_examples = cfg.get("env_examples", {})
    if env_examples is None:
        env_examples = {}
    if not isinstance(env_examples, dict):
        raise SystemExit("Config field 'env_examples' must be a YAML object.")
    shared = env_examples.get("shared", {})
    profiles = env_examples.get("profiles", {})
    if shared is None:
        shared = {}
    if profiles is None:
        profiles = {}
    if not isinstance(shared, dict):
        raise SystemExit("Config field 'env_examples.shared' must be a YAML object.")
    if not isinstance(profiles, dict):
        raise SystemExit("Config field 'env_examples.profiles' must be a YAML object.")
    per_profile = profiles.get(profile_name, {})
    if per_profile is None:
        per_profile = {}
    if not isinstance(per_profile, dict):
        raise SystemExit(
            f"Config field 'env_examples.profiles.{profile_name}' must be a YAML object."
        )
    lines: list[str] = []
    for key, value in {**shared, **per_profile}.items():
        lines.append(f"{key}={value}")
    if not lines:
        lines.append("# Add provider credentials here.")
    lines.append("")
    return "\n".join(lines)


def configured_skins(cfg: dict[str, Any]) -> list[dict[str, Any]]:
    skins = cfg.get("skins", {})
    if not isinstance(skins, dict):
        return []
    resolved: list[dict[str, Any]] = []
    for name, data in skins.items():
        source: str | None
        if isinstance(data, dict):
            source = str(data.get("source", "") or "").strip()
        else:
            source = str(data or "").strip()
        if not source:
            continue
        source_path = Path(source)
        if not source_path.is_absolute():
            candidate = resource_path(source)
            source_path = candidate.resolve() if candidate.exists() else resolve_home_relative(source)
        resolved.append({"name": str(name).strip(), "source": source_path})
    return resolved


def nonnegative_int(value: str) -> int:
    parsed = int(value)
    if parsed < 0:
        raise argparse.ArgumentTypeError("value must be >= 0")
    return parsed


def positive_int(value: str) -> int:
    parsed = int(value)
    if parsed <= 0:
        raise argparse.ArgumentTypeError("value must be > 0")
    return parsed


def install_profile_skins(cfg: dict[str, Any], profile_name: str, force: bool) -> list[str]:
    installed: list[str] = []
    skins = configured_skins(cfg)
    if not skins:
        return installed
    skin_dir = profile_home(cfg, profile_name) / "skins"
    skin_dir.mkdir(parents=True, exist_ok=True)
    for skin in skins:
        name = str(skin["name"])
        source = Path(skin["source"])
        if not source.exists():
            raise SystemExit(f"Skin asset not found: {source}")
        destination = skin_dir / f"{name}.yaml"
        if destination.exists() and not force:
            installed.append(str(destination))
            continue
        shutil.copy2(source, destination)
        installed.append(str(destination))
    return installed


def _deep_merge(base: dict[str, Any], override: dict[str, Any]) -> dict[str, Any]:
    """Recursively merge override into base. Used by render_profiles (PRD-010)."""
    result = dict(base)
    for k, v in override.items():
        if k in result and isinstance(result[k], dict) and isinstance(v, dict):
            result[k] = _deep_merge(result[k], v)
        else:
            result[k] = v
    return result


def _apply_memory_config(
    profile_cfg: dict[str, Any],
    env_file: Path,
    memory_section: dict[str, Any],
) -> None:
    """PRD-001: Write memory backend config keys into profile_cfg dict."""
    provider = memory_section.get("provider", "none")
    if not provider or provider == "none":
        return

    profile_cfg["memory"] = {"provider": provider}

    if provider == "supermemory":
        sm = memory_section.get("supermemory", {})
        if sm.get("session_ingest"):
            profile_cfg["memory"]["session_ingest"] = True
            _upsert_env_line(env_file, "SUPERMEMORY_SESSION_INGEST", "1")

    elif provider == "honcho":
        honcho = memory_section.get("honcho", {})
        base_url = honcho.get("base_url", "http://localhost:8001")
        app_name = honcho.get("app_name", "tag")
        profile_cfg["memory"]["base_url"] = base_url
        profile_cfg["memory"]["app_name"] = app_name

    elif provider == "local":
        pass  # hermes-local-memory plugin picks up {"provider": "local"}


def render_profiles(cfg: dict[str, Any], force: bool) -> list[dict[str, str]]:
    results: list[dict[str, str]] = []
    profiles = cfg.get("profiles", {})
    for name, profile in profiles.items():
        home = profile_home(cfg, name)
        config_file = home / "config.yaml"
        env_file = home / ".env"
        env_example = home / ".env.example"

        # PRD-010: deep-merge with existing config to preserve panel edits
        existing: dict[str, Any] = {}
        if config_file.exists() and not force:
            try:
                existing = yaml.safe_load(config_file.read_text()) or {}
            except yaml.YAMLError:
                existing = {}

        profile_cfg = dict(profile.get("config", {}))

        # PRD-001: apply memory backend config
        memory_section = profile_cfg.pop("memory", None)
        if memory_section:
            _apply_memory_config(profile_cfg, env_file, memory_section)

        # PRD-001: apply gateway config
        gateway_section = profile_cfg.pop("gateway", None)
        if gateway_section and gateway_section.get("enabled"):
            profile_cfg["gateway"] = {"use_gateway": True}
            if tools := gateway_section.get("tools"):
                profile_cfg["gateway"]["allowed_tools"] = tools

        # PRD-005: apply execution backend config
        exec_section = profile_cfg.pop("execution", None)
        if exec_section:
            backend = exec_section.get("backend", "local")
            if backend != "local":
                exec_out: dict[str, Any] = {"backend": backend}
                if backend == "docker":
                    docker_cfg = exec_section.get("docker", {})
                    exec_out["docker"] = {
                        "image": docker_cfg.get("image", "ubuntu:22.04"),
                        "auto_pull": docker_cfg.get("auto_pull", True),
                    }
                    if volumes := docker_cfg.get("extra_volumes"):
                        exec_out["docker"]["extra_volumes"] = volumes
                elif backend == "ssh":
                    ssh_cfg = exec_section.get("ssh", {})
                    exec_out["ssh"] = {
                        "host": ssh_cfg.get("host", ""),
                        "user": ssh_cfg.get("user", ""),
                        "port": ssh_cfg.get("port", 22),
                        "key_file": str(Path(ssh_cfg.get("key_file", "~/.ssh/id_rsa")).expanduser()),
                        "remote_work_dir": ssh_cfg.get("remote_work_dir", "/tmp/tag-agent"),
                    }
                elif backend == "modal":
                    modal_cfg = exec_section.get("modal", {})
                    exec_out["modal"] = {
                        "app_name": modal_cfg.get("app_name", f"tag-{name}"),
                        "gpu": modal_cfg.get("gpu", ""),
                    }
                elif backend == "daytona":
                    daytona_cfg = exec_section.get("daytona", {})
                    exec_out["daytona"] = {
                        "workspace_id": daytona_cfg.get("workspace_id", ""),
                    }
                profile_cfg["execution"] = exec_out

        merged_cfg = _deep_merge(existing, profile_cfg)
        write_yaml(config_file, merged_cfg, force=True)
        write_text(env_example, merged_env_example(cfg, name), force=force)
        installed_skins = install_profile_skins(cfg, name, force=force)
        results.append(
            {
                "profile": name,
                "config": str(config_file),
                "env_example": str(env_example),
                "skins": ", ".join(installed_skins),
            }
        )
    return results


def bootstrap_profiles(cfg: dict[str, Any]) -> list[dict[str, str]]:
    created: list[dict[str, str]] = []
    profiles = cfg.get("profiles", {})
    for name, profile in profiles.items():
        home = profile_home(cfg, name)
        if home.exists():
            created.append({"profile": name, "status": "existing"})
            continue
        cmd = ["profile", "create", name, "--no-alias"]
        description = str(profile.get("description", "")).strip()
        if description:
            cmd.extend(["--description", description])
        try:
            run_hermes(cfg, *cmd)
        except subprocess.CalledProcessError as exc:
            message = exc.stderr.strip() or exc.stdout.strip() or str(exc)
            raise SystemExit(f"Failed to create TAG-managed profile '{name}': {message}") from exc
        created.append({"profile": name, "status": "created"})
    return created


def resolve_route(cfg: dict[str, Any], task_type: str, master_override: str | None, worker_override: list[str]) -> dict[str, Any]:
    defaults = cfg.get("defaults", {})
    routing = cfg.get("routing", {}).get("task_types", {})
    route = dict(routing.get(task_type, {}))
    if not route:
        available = ", ".join(sorted(routing))
        raise SystemExit(f"Unknown task type '{task_type}'. Available: {available}")

    master = master_override or defaults.get("master_profile")
    workers = worker_override or route.get("workers", [])
    verifier = route.get("verifier")

    profiles = cfg.get("profiles", {})
    snapshot = {
        "master_profile": master,
        "board": defaults.get("board", "default"),
        "execution": route.get("execution", "kanban"),
        "workers": [],
        "verifier": None,
    }

    if master not in profiles:
        raise SystemExit(f"Master profile '{master}' is not defined in config.")

    for worker in workers:
        if worker not in profiles:
            raise SystemExit(f"Worker profile '{worker}' is not defined in config.")
        pdata = profiles[worker]
        snapshot["workers"].append(
            {
                "name": worker,
                "description": pdata.get("description", ""),
                "tags": pdata.get("tags", []),
                "model": pdata.get("config", {}).get("model", {}),
            }
        )

    if verifier:
        if verifier not in profiles:
            raise SystemExit(f"Verifier profile '{verifier}' is not defined in config.")
        vdata = profiles[verifier]
        snapshot["verifier"] = {
            "name": verifier,
            "description": vdata.get("description", ""),
            "tags": vdata.get("tags", []),
            "model": vdata.get("config", {}).get("model", {}),
        }

    master_data = profiles[master]
    snapshot["master"] = {
        "name": master,
        "description": master_data.get("description", ""),
        "tags": master_data.get("tags", []),
        "model": master_data.get("config", {}).get("model", {}),
        "delegation": master_data.get("config", {}).get("delegation", {}),
    }
    return snapshot


def parse_model_ref(value: str) -> tuple[str, str]:
    if any(ord(c) < 32 or ord(c) == 127 for c in value):
        raise SystemExit(
            f"Invalid model reference '{value}'. Provider and model must not contain control characters."
        )
    ref = value.strip()
    if "/" not in ref:
        raise SystemExit(
            f"Invalid model reference '{value}'. Use provider/model-id format."
        )
    provider, model = ref.split("/", 1)
    provider = provider.strip()
    model = model.strip()
    if not provider or not model:
        raise SystemExit(
            f"Invalid model reference '{value}'. Use provider/model-id format."
        )
    return provider, model


def format_model_ref(model_cfg: dict[str, Any]) -> str:
    provider = str(model_cfg.get("provider", "") or "").strip()
    model = str(model_cfg.get("default", model_cfg.get("name", "")) or "").strip()
    if provider and model:
        return f"{provider}/{model}"
    if model:
        return model
    return "-"


def collect_assignments(cfg: dict[str, Any]) -> list[dict[str, Any]]:
    rows: list[dict[str, Any]] = []
    for name, profile in (cfg.get("profiles") or {}).items():
        profile_cfg = profile.get("config", {})
        primary = profile_cfg.get("model", {}) if isinstance(profile_cfg, dict) else {}
        delegation = (
            profile_cfg.get("delegation", {}) if isinstance(profile_cfg, dict) else {}
        )
        row = {
            "profile": name,
            "description": profile.get("description", ""),
            "primary_model": format_model_ref(primary if isinstance(primary, dict) else {}),
            "delegation_model": "-",
            "openai_runtime": "",
        }
        if isinstance(delegation, dict) and delegation.get("provider") and delegation.get("model"):
            row["delegation_model"] = f"{delegation['provider']}/{delegation['model']}"
        if isinstance(primary, dict) and primary.get("openai_runtime"):
            row["openai_runtime"] = str(primary["openai_runtime"])
        rows.append(row)
    return rows


def load_model_inventory(cfg: dict[str, Any], profile_name: str) -> dict[str, Any]:
    inline = textwrap.dedent(
        """
        import json
        from hermes_cli.inventory import build_models_payload, load_picker_context

        payload = build_models_payload(
            load_picker_context(),
            include_unconfigured=True,
            picker_hints=True,
            canonical_order=True,
            pricing=True,
            capabilities=True,
            max_models=50,
        )
        print(json.dumps(payload))
        """
    ).strip()
    proc = run_profile_python(cfg, profile_name, inline)
    stdout = proc.stdout.strip()
    if not stdout:
        raise SystemExit(f"Failed to load model inventory for profile '{profile_name}'.")
    return json.loads(stdout)


def load_openrouter_catalog(cfg: dict[str, Any], profile_name: str) -> list[dict[str, Any]]:
    env = profile_exec_env(cfg, profile_name)
    api_key = env.get("OPENROUTER_API_KEY", "").strip()
    if not api_key:
        raise SystemExit(
            f"OPENROUTER_API_KEY is not set for profile '{profile_name}'."
        )
    req = urllib.request.Request(
        "https://openrouter.ai/api/v1/models",
        headers={"Authorization": f"Bearer {api_key}"},
    )
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            payload = json.loads(resp.read().decode("utf-8"))
    except urllib.error.HTTPError as exc:
        raise SystemExit(f"OpenRouter models request failed with HTTP {exc.code}.") from exc
    except urllib.error.URLError as exc:
        reason = exc.reason if exc.reason else "unknown network error"
        raise SystemExit(f"OpenRouter models request failed: {reason}") from exc
    except TimeoutError as exc:
        raise SystemExit("OpenRouter models request timed out.") from exc
    except json.JSONDecodeError as exc:
        raise SystemExit("OpenRouter models response was not valid JSON.") from exc
    rows = payload.get("data", [])
    if not isinstance(rows, list):
        raise SystemExit("Unexpected OpenRouter models payload.")
    return rows


def ensure_profile_exists(cfg: dict[str, Any], profile_name: str) -> None:
    profiles = cfg.get("profiles", {})
    if profile_name not in profiles:
        available = ", ".join(sorted(profiles))
        raise SystemExit(f"Unknown profile '{profile_name}'. Available: {available}")


def apply_route_model_overrides(
    route: dict[str, Any],
    *,
    master_model: str | None,
    verifier_model: str | None,
    worker_models: list[str],
) -> dict[str, Any]:
    if master_model:
        provider, model = parse_model_ref(master_model)
        route["master"]["model"] = {"provider": provider, "default": model}
    if verifier_model and route.get("verifier"):
        provider, model = parse_model_ref(verifier_model)
        route["verifier"]["model"] = {"provider": provider, "default": model}
    overrides: dict[str, tuple[str, str]] = {}
    for item in worker_models:
        if "=" not in item:
            raise SystemExit(
                f"Invalid worker override '{item}'. Use profile=provider/model-id."
            )
        worker_name, ref = item.split("=", 1)
        overrides[worker_name.strip()] = parse_model_ref(ref)
    for worker in route["workers"]:
        if worker["name"] not in overrides:
            continue
        provider, model = overrides[worker["name"]]
        worker["model"] = {"provider": provider, "default": model}
    return route


def run_chat_step(
    cfg: dict[str, Any],
    *,
    profile_name: str,
    prompt: str,
) -> dict[str, Any]:
    started = dt.datetime.now(dt.timezone.utc)
    proc = run_profile_hermes(
        cfg,
        profile_name,
        "chat",
        "-q",
        prompt,
        "-Q",
        check=False,
    )
    finished = dt.datetime.now(dt.timezone.utc)
    output = proc.stdout.strip()
    if proc.stderr.strip():
        output = f"{output}\n{proc.stderr.strip()}".strip()
    profiles = cfg.get("profiles", {})
    model_cfg = profiles.get(profile_name, {}).get("config", {}).get("model", {})
    failure_reason = infrastructure_failure_reason(output)
    return {
        "profile": profile_name,
        "status": "ok" if proc.returncode == 0 and not failure_reason else "error",
        "prompt": prompt,
        "output": output,
        "started_at": started.isoformat(),
        "finished_at": finished.isoformat(),
        "duration_ms": int((finished - started).total_seconds() * 1000),
        "returncode": proc.returncode,
        "model_ref": format_model_ref(model_cfg if isinstance(model_cfg, dict) else {}),
        "failure_reason": failure_reason or "",
    }


def load_benchmark_suite(path: Path) -> list[dict[str, Any]]:
    try:
        payload = load_config(path)
    except SystemExit:
        raise FileNotFoundError(path)
    cases = payload.get("cases", [])
    if not isinstance(cases, list):
        raise SystemExit(f"Benchmark suite at {path} must contain a 'cases' list.")
    return cases


def case_passed(case: dict[str, Any], output: str) -> tuple[bool, str]:
    normalized = normalize_chat_output(output)
    expected_exact = case.get("expected_exact")
    if isinstance(expected_exact, str):
        ok = normalized == expected_exact
        return ok, f"expected exact tail '{expected_exact}'"
    expected_regex = case.get("expected_regex")
    if isinstance(expected_regex, str):
        ok = re.search(expected_regex, normalized, re.MULTILINE) is not None
        return ok, f"expected regex '{expected_regex}'"
    expected_json = case.get("expected_json")
    if isinstance(expected_json, dict):
        try:
            parsed = json.loads(strip_json_fences(normalized))
        except Exception:
            return False, "expected valid JSON in final line"
        for key, value in expected_json.items():
            if parsed.get(key) != value:
                return False, f"expected JSON field {key}={value!r}"
        return True, "matched expected JSON fields"
    return False, "benchmark case missing expectation"


def create_temp_profile(
    cfg: dict[str, Any],
    *,
    base_profile: str,
    model_ref: str,
) -> str:
    ensure_profile_exists(cfg, base_profile)
    provider, model = parse_model_ref(model_ref)
    profile_name = f"bench-{base_profile}-{slugify(provider)}-{slugify(model)}"
    home = profile_home(cfg, profile_name)
    if not home.exists():
        run_hermes(cfg, "profile", "create", profile_name, "--no-alias")
    base_cfg = cfg.get("profiles", {}).get(base_profile, {})
    profile_cfg = json.loads(json.dumps(base_cfg))
    profile_cfg.setdefault("config", {}).setdefault("model", {})
    profile_cfg["config"]["model"]["provider"] = provider
    profile_cfg["config"]["model"]["default"] = model
    write_yaml(home / "config.yaml", profile_cfg.get("config", {}), force=True)
    base_env = profile_home(cfg, base_profile) / ".env"
    if base_env.exists():
        write_text(home / ".env", base_env.read_text(encoding="utf-8"), force=True)
    for name in ("auth.json", "auth.lock", "models_dev_cache.json", "provider_models_cache.json"):
        src = profile_home(cfg, base_profile) / name
        dst = home / name
        if src.exists() and not dst.exists():
            shutil.copy2(src, dst)
    return profile_name


def show_kanban_task(cfg: dict[str, Any], *, profile_name: str, board: str, task_id: str) -> dict[str, Any]:
    proc = run_profile_hermes(
        cfg,
        profile_name,
        "kanban",
        "--board",
        board,
        "show",
        task_id,
        "--json",
        check=False,
    )
    if proc.returncode != 0 or not proc.stdout.strip():
        raise SystemExit(proc.stderr.strip() or proc.stdout.strip() or f"Failed to show task {task_id}.")
    return json.loads(proc.stdout)


def insert_run(
    conn: sqlite3.Connection,
    *,
    run_id: str,
    kind: str,
    task_type: str,
    execution: str,
    master_profile: str,
    board: str,
    prompt: str,
    route: dict[str, Any],
    status: str,
    metadata: dict[str, Any],
) -> None:
    conn.execute(
        """
        INSERT INTO runs (
          id, created_at, kind, task_type, execution, master_profile, board,
          prompt, route_json, status, metadata_json
        ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
        """,
        (
            run_id,
            utc_now(),
            kind,
            task_type,
            execution,
            master_profile,
            board,
            prompt,
            json.dumps(route),
            status,
            json.dumps(metadata),
        ),
    )
    conn.commit()


def update_run_status(
    conn: sqlite3.Connection,
    *,
    run_id: str,
    status: str,
    metadata: dict[str, Any] | None = None,
) -> None:
    if metadata is None:
        conn.execute("UPDATE runs SET status = ? WHERE id = ?", (status, run_id))
    else:
        conn.execute(
            "UPDATE runs SET status = ?, metadata_json = ? WHERE id = ?",
            (status, json.dumps(metadata), run_id),
        )
    conn.commit()


def insert_step(
    conn: sqlite3.Connection,
    *,
    run_id: str,
    role: str,
    profile: str,
    model_ref: str,
    prompt: str,
    output: str,
    status: str,
    started_at: str,
    finished_at: str,
    duration_ms: int,
    extra: dict[str, Any],
) -> None:
    conn.execute(
        """
        INSERT INTO steps (
          run_id, role, profile, model_ref, prompt, output, status,
          started_at, finished_at, duration_ms, extra_json
        ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
        """,
        (
            run_id,
            role,
            profile,
            model_ref,
            prompt,
            output,
            status,
            started_at,
            finished_at,
            duration_ms,
            json.dumps(extra),
        ),
    )
    conn.commit()


def ensure_parent(path: Path) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)


def run_external(
    cmd: list[str],
    *,
    cwd: Path | None = None,
    env: dict[str, str] | None = None,
    check: bool = True,
    capture_output: bool = True,
) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        cmd,
        cwd=str(cwd) if cwd else None,
        env=env,
        text=True,
        capture_output=capture_output,
        check=check,
    )


def tool_path(name: str) -> str:
    return shutil.which(name) or ""


def tool_version(cmd: list[str]) -> str:
    proc = run_external(cmd, check=False)
    if proc.returncode != 0:
        return (proc.stderr or proc.stdout).strip()
    return (proc.stdout or proc.stderr).strip()


def patch_status(cfg: dict[str, Any]) -> str:
    root = hermes_root(cfg)
    patch = hermes_patch_path()
    if not root.exists():
        return "checkout-missing"
    reverse = run_external(
        ["git", "apply", "--reverse", "--check", str(patch)],
        cwd=root,
        check=False,
    )
    if reverse.returncode == 0:
        return "prepatched" if hermes_checkout_kind(root) == "bundled" else "applied"
    forward = run_external(
        ["git", "apply", "--check", str(patch)],
        cwd=root,
        check=False,
    )
    if forward.returncode == 0:
        return "not-applied"
    return "diverged"


def workspace_node_module_manifest(root: Path, package: str) -> Path:
    scoped_parts = package.split("/")
    candidates = (
        root / "node_modules" / Path(*scoped_parts) / "package.json",
        root / "ui-tui" / "node_modules" / Path(*scoped_parts) / "package.json",
    )
    for candidate in candidates:
        if candidate.exists():
            return candidate
    return candidates[0]


def doctor_prerequisites(cfg: dict[str, Any]) -> dict[str, Any]:
    git = tool_path("git")
    npm = tool_path("npm")
    python = sys.executable
    root = hermes_root(cfg)
    tui_dist = root / "ui-tui" / "dist" / "entry.js"
    tui_react = workspace_node_module_manifest(root, "react")
    tui_vitest = workspace_node_module_manifest(root, "vitest")
    python_bin = setup_python_bin(cfg)

    report: dict[str, Any] = {
        "git": {"found": bool(git), "path": git, "version": tool_version([git, "--version"]) if git else ""},
        "npm": {"found": bool(npm), "path": npm, "version": tool_version([npm, "--version"]) if npm else ""},
        "python": {"path": python, "version": tool_version([python, "--version"])},
        "python_runtime_supported": python_runtime_supported(sys.version_info[:2]),
        "hermes_checkout_exists": root.exists(),
        "hermes_checkout_kind": hermes_checkout_kind(root),
        "hermes_python_exists": python_bin.exists(),
        "bundled_hermes_available": bundled_hermes_archive().exists(),
        "patch_status": patch_status(cfg),
        "tui_dist_exists": tui_dist.exists(),
        "tui_react_installed": tui_react.exists(),
        "tui_vitest_installed": tui_vitest.exists(),
    }

    return report


def ensure_setup_prereqs(cfg: dict[str, Any], *, need_npm: bool, need_git: bool) -> None:
    prereqs = doctor_prerequisites(cfg)
    missing: list[str] = []

    if need_git and not prereqs["git"]["found"]:
        missing.append("git")
    if need_npm and not prereqs["npm"]["found"]:
        missing.append("npm")
    if not prereqs["python_runtime_supported"]:
        version = prereqs["python"]["version"]
        raise SystemExit(
            "TAG currently requires Python >=3.11 and <3.14 because the managed runtime does. "
            f"Current runtime: {version}."
        )

    if missing:
        names = ", ".join(missing)
        raise SystemExit(f"Missing required tools for TAG setup: {names}. Run `tag doctor` for details.")

def setup_python_bin(cfg: dict[str, Any]) -> Path:
    return hermes_root(cfg) / ".venv" / "bin" / "python"


def hermes_patch_path() -> Path:
    return resource_path("patches", "hermes-ui.patch")


def safe_extract_tar_gz(archive: Path, target: Path) -> None:
    target_real = target.resolve()
    try:
        with tarfile.open(archive, "r:gz") as tf:
            members = tf.getmembers()
            for member in members:
                member_name = member.name
                pure = PurePosixPath(member_name)
                if pure.is_absolute() or ".." in pure.parts:
                    raise SystemExit(f"Bundled Hermes archive contains an unsafe entry: {member_name}")
                if member.issym() or member.islnk():
                    raise SystemExit(f"Bundled Hermes archive contains an unsupported link entry: {member_name}")
                if not (member.isdir() or member.isfile()):
                    raise SystemExit(f"Bundled Hermes archive contains an unsupported entry type: {member_name}")
                dest = (target / member_name).resolve()
                if target_real != dest and target_real not in dest.parents:
                    raise SystemExit(f"Bundled Hermes archive contains a path traversal entry: {member_name}")
            for member in members:
                tf.extract(member, target)
    except (tarfile.TarError, OSError) as exc:
        raise SystemExit(f"Bundled Hermes archive could not be read: {archive}") from exc


def extract_bundled_hermes(root: Path) -> dict[str, Any]:
    archive = bundled_hermes_archive()
    if not archive.exists():
        raise SystemExit("Bundled Hermes snapshot is not available in this TAG build.")
    ensure_parent(root)
    if root.exists():
        shutil.rmtree(root)
    root.mkdir(parents=True, exist_ok=True)
    safe_extract_tar_gz(archive, root)
    return {"checkout": str(root), "status": "bundled", "archive": str(archive)}


def clone_or_update_hermes(cfg: dict[str, Any], *, refresh: bool) -> dict[str, Any]:
    override = os.environ.get("TAG_HERMES_ROOT", "").strip()
    root = (
        Path(override).expanduser().resolve()
        if override
        else resolve_home_relative(str(cfg.get("upstream", {}).get("checkout_dir", DEFAULT_HERMES_CHECKOUT)))
    )
    repo = hermes_repo_url(cfg)
    ref = hermes_ref(cfg)
    archive = bundled_hermes_archive()

    if root.exists():
        if refresh and not (root / ".git").exists() and archive.exists():
            return extract_bundled_hermes(root)
        if refresh:
            run_external(["git", "fetch", "--all", "--tags"], cwd=root)
            run_external(["git", "checkout", ref], cwd=root)
            run_external(["git", "pull", "--ff-only"], cwd=root, check=False)
            return {"checkout": str(root), "status": "updated", "ref": ref}
        return {"checkout": str(root), "status": "existing", "ref": ref}

    if archive.exists():
        return extract_bundled_hermes(root)

    ensure_parent(root)
    run_external(["git", "clone", "--depth", "1", "--branch", ref, repo, str(root)])
    return {"checkout": str(root), "status": "cloned", "ref": ref}


def ensure_venv(cfg: dict[str, Any]) -> dict[str, Any]:
    python_bin = setup_python_bin(cfg)
    if python_bin.exists():
        return {"venv": str(python_bin.parent.parent), "status": "existing"}
    run_external([sys.executable, "-m", "venv", str(python_bin.parent.parent)])
    return {"venv": str(python_bin.parent.parent), "status": "created"}


def install_hermes_python(cfg: dict[str, Any]) -> dict[str, Any]:
    python_bin = setup_python_bin(cfg)
    run_external([str(python_bin), "-m", "ensurepip", "--upgrade"], cwd=hermes_root(cfg), check=False)
    run_external([str(python_bin), "-m", "pip", "install", "--upgrade", "pip"], cwd=hermes_root(cfg))
    run_external(
        [str(python_bin), "-m", "pip", "install", "-e", ".[cli,web,mcp]"],
        cwd=hermes_root(cfg),
    )
    return {"python": str(python_bin), "status": "installed"}


def apply_hermes_patch(cfg: dict[str, Any]) -> dict[str, Any]:
    patch = hermes_patch_path()
    root = hermes_root(cfg)
    reverse = run_external(
        ["git", "apply", "--reverse", "--check", str(patch)],
        cwd=root,
        check=False,
    )
    if reverse.returncode == 0:
        status = "prepatched" if hermes_checkout_kind(root) == "bundled" else "already-applied"
        return {"patch": str(patch), "status": status}
    forward = run_external(
        ["git", "apply", "--check", str(patch)],
        cwd=root,
        check=False,
    )
    if forward.returncode != 0:
        message = forward.stderr.strip() or forward.stdout.strip() or "TAG patch check failed."
        raise SystemExit(message)
    run_external(["git", "apply", str(patch)], cwd=root)
    return {"patch": str(patch), "status": "applied"}


def install_tui_dependencies(cfg: dict[str, Any]) -> dict[str, Any]:
    root = hermes_root(cfg)
    run_external(
        [
            "npm",
            "install",
            "--workspace",
            "ui-tui",
            "--silent",
            "--no-fund",
            "--no-audit",
            "--progress=false",
        ],
        cwd=root,
    )
    run_external(["npm", "run", "build", "--workspace", "ui-tui"], cwd=root)
    return {"ui_tui": str(root / "ui-tui"), "status": "built"}


def import_codex_into_profile(
    cfg: dict[str, Any],
    *,
    profile_name: str,
    source_codex_home: Path,
) -> dict[str, Any]:
    ensure_runtime_dirs(cfg)
    target_home = profile_home(cfg, profile_name)
    if not target_home.exists():
        return {"profile": profile_name, "status": "profile-missing"}

    env = hermes_env(cfg)
    env["HERMES_HOME"] = str(target_home)
    env["CODEX_HOME"] = str(source_codex_home.expanduser().resolve())

    inline = textwrap.dedent(
        """
        import json
        from hermes_cli.auth import _import_codex_cli_tokens, _save_codex_tokens

        tokens = _import_codex_cli_tokens()
        if not tokens:
            raise SystemExit("No importable Codex CLI tokens found.")
        _save_codex_tokens(tokens)
        print(json.dumps({"imported": True}))
        """
    ).strip()

    proc = subprocess.run(
        [str(hermes_root(cfg) / ".venv" / "bin" / "python"), "-c", inline],
        env=env,
        text=True,
        capture_output=True,
    )
    if proc.returncode != 0:
        message = proc.stderr.strip() or proc.stdout.strip() or "Codex import failed."
        return {"profile": profile_name, "status": "failed", "message": message}
    return {"profile": profile_name, "status": "imported", "codex_home": str(env["CODEX_HOME"])}


def auto_import_codex_profiles(cfg: dict[str, Any]) -> list[dict[str, Any]]:
    source_home = Path(
        os.environ.get("TAG_IMPORT_CODEX_HOME", str(Path.home() / ".codex"))
    ).expanduser().resolve()
    if not (source_home / "auth.json").exists():
        return [
            {"profile": "orchestrator", "status": "skipped-no-auth"},
            {"profile": "codex-runtime-master", "status": "skipped-no-auth"},
        ]
    results = []
    for profile_name in ("orchestrator", "codex-runtime-master"):
        results.append(
            import_codex_into_profile(
                cfg,
                profile_name=profile_name,
                source_codex_home=source_home,
            )
        )
    return results


# ---------- Claude Code credential import ----------


def _detect_claude_code_credentials(
    source_home: Path | None = None,
) -> dict[str, Any]:
    """Detect available Claude Code credentials on the local machine.

    Checks, in order:
      1. ANTHROPIC_API_KEY env var (safest — no ToS risk)
      2. ~/.claude/.credentials.json  (OAuth, Linux/Windows/SSH)
      3. ~/.claude.json               (OAuth, macOS app-state fallback)

    Returns a dict with keys: api_key, oauth_token, oauth_expires_at, source.
    """
    result: dict[str, Any] = {
        "api_key": None,
        "oauth_token": None,
        "oauth_expires_at": None,
        "source": None,
    }

    api_key = os.environ.get("ANTHROPIC_API_KEY", "").strip()
    if api_key:
        result["api_key"] = api_key

    claude_home = source_home or (Path.home() / ".claude")

    creds_file = claude_home / ".credentials.json"
    if creds_file.exists():
        try:
            data = json.loads(creds_file.read_text(encoding="utf-8"))
            oauth = data.get("claudeAiOauth") or {}
            token = (oauth.get("accessToken") or "").strip()
            if token:
                result["oauth_token"] = token
                result["oauth_expires_at"] = oauth.get("expiresAt")
                result["source"] = str(creds_file)
        except (json.JSONDecodeError, OSError):
            pass

    if not result["oauth_token"]:
        dot_claude_json = Path.home() / ".claude.json"
        if dot_claude_json.exists():
            try:
                data = json.loads(dot_claude_json.read_text(encoding="utf-8"))
                oauth = data.get("claudeAiOauth") or data.get("oauthAccount") or {}
                token = (oauth.get("accessToken") or "").strip()
                if token:
                    result["oauth_token"] = token
                    result["oauth_expires_at"] = oauth.get("expiresAt")
                    result["source"] = str(dot_claude_json)
            except (json.JSONDecodeError, OSError):
                pass

    return result


def import_claude_into_profile(
    cfg: dict[str, Any],
    *,
    profile_name: str,
    source_claude_home: Path | None = None,
    use_oauth: bool = False,
) -> dict[str, Any]:
    """Import Claude Code / Anthropic credentials into a TAG-managed Hermes profile.

    API key (ANTHROPIC_API_KEY) is always preferred — zero ToS risk.
    OAuth token (CLAUDE_CODE_OAUTH_TOKEN) is only written when use_oauth=True;
    Anthropic prohibits use of claude auth login tokens in third-party tools
    as of early 2026 and actively suspends violating accounts.
    """
    ensure_runtime_dirs(cfg)
    target_home = profile_home(cfg, profile_name)
    if not target_home.exists():
        return {"profile": profile_name, "status": "profile-missing"}

    creds = _detect_claude_code_credentials(source_claude_home)

    if creds["api_key"]:
        _upsert_env_line(target_home / ".env", "ANTHROPIC_API_KEY", creds["api_key"])
        return {
            "profile": profile_name,
            "status": "imported",
            "mode": "api_key",
            "provider": "anthropic",
        }

    if use_oauth and creds["oauth_token"]:
        _upsert_env_line(target_home / ".env", "CLAUDE_CODE_OAUTH_TOKEN", creds["oauth_token"])
        return {
            "profile": profile_name,
            "status": "imported",
            "mode": "oauth",
            "provider": "anthropic",
            "source": creds["source"],
            "tos_warning": (
                "Anthropic prohibits use of claude auth login OAuth tokens in "
                "third-party tools. Set ANTHROPIC_API_KEY for ToS-compliant access."
            ),
        }

    return {"profile": profile_name, "status": "skipped-no-auth"}


def auto_import_claude_profiles(
    cfg: dict[str, Any],
    *,
    use_oauth: bool = False,
) -> list[dict[str, Any]]:
    """Auto-import Claude/Anthropic credentials into all non-Codex profiles during setup."""
    creds = _detect_claude_code_credentials()
    if not creds["api_key"] and not (use_oauth and creds["oauth_token"]):
        return [
            {"profile": p, "status": "skipped-no-auth"}
            for p in cfg.get("profiles", {})
            if p != "codex-runtime-master"
        ]
    return [
        import_claude_into_profile(cfg, profile_name=p, use_oauth=use_oauth)
        for p in cfg.get("profiles", {})
        if p != "codex-runtime-master"
    ]


# ---------- Gemini CLI credential import ----------


def _detect_gemini_credentials(
    source_home: Path | None = None,
) -> dict[str, Any]:
    """Detect available Gemini CLI credentials on the local machine.

    Checks, in order:
      1. GEMINI_API_KEY env var (safest — no ToS risk)
      2. ~/.gemini/.env          (GEMINI_API_KEY stored by gemini CLI)
      3. ~/.gemini/oauth_creds.json  (OAuth, use_oauth=True only)

    Returns a dict with keys:
      api_key, oauth_token, refresh_token, oauth_expiry_ms, source.
    """
    result: dict[str, Any] = {
        "api_key": None,
        "oauth_token": None,
        "refresh_token": None,
        "oauth_expiry_ms": None,
        "source": None,
    }

    api_key = os.environ.get("GEMINI_API_KEY", "").strip()
    if api_key:
        result["api_key"] = api_key

    gemini_home = source_home or (Path.home() / ".gemini")

    if not result["api_key"]:
        gemini_dotenv = gemini_home / ".env"
        key = read_dotenv(gemini_dotenv).get("GEMINI_API_KEY", "").strip()
        if key:
            result["api_key"] = key

    oauth_file = gemini_home / "oauth_creds.json"
    if oauth_file.exists():
        try:
            data = json.loads(oauth_file.read_text(encoding="utf-8"))
            token = (data.get("access_token") or "").strip()
            refresh = (data.get("refresh_token") or "").strip()
            if token or refresh:
                result["oauth_token"] = token or None
                result["refresh_token"] = refresh or None
                result["oauth_expiry_ms"] = data.get("expiry_date")
                result["source"] = str(oauth_file)
        except (json.JSONDecodeError, OSError):
            pass

    return result


def import_gemini_into_profile(
    cfg: dict[str, Any],
    *,
    profile_name: str,
    source_gemini_home: Path | None = None,
    use_oauth: bool = False,
) -> dict[str, Any]:
    """Import Gemini CLI / Google API credentials into a TAG-managed Hermes profile.

    API key (GEMINI_API_KEY) is always preferred — zero ToS risk.
    OAuth tokens from ~/.gemini/oauth_creds.json are written to Hermes's
    google_oauth.json store only when use_oauth=True. Google explicitly bans
    piggybacking on Gemini CLI OAuth (enforced with account suspensions since
    March 2026). Use GEMINI_API_KEY from https://aistudio.google.com/app/apikey
    for ToS-compliant access.
    """
    ensure_runtime_dirs(cfg)
    target_home = profile_home(cfg, profile_name)
    if not target_home.exists():
        return {"profile": profile_name, "status": "profile-missing"}

    creds = _detect_gemini_credentials(source_gemini_home)

    if creds["api_key"]:
        _upsert_env_line(target_home / ".env", "GEMINI_API_KEY", creds["api_key"])
        return {
            "profile": profile_name,
            "status": "imported",
            "mode": "api_key",
            "provider": "gemini",
        }

    if use_oauth and (creds["oauth_token"] or creds["refresh_token"]):
        google_oauth_dir = target_home / "auth"
        google_oauth_dir.mkdir(parents=True, exist_ok=True)
        google_oauth_file = google_oauth_dir / "google_oauth.json"
        existing: dict[str, Any] = {}
        if google_oauth_file.exists():
            try:
                existing = json.loads(google_oauth_file.read_text(encoding="utf-8"))
            except (json.JSONDecodeError, OSError):
                existing = {}
        existing.update({
            "access_token": creds["oauth_token"] or "",
            "refresh_token": creds["refresh_token"] or "",
            "expiry_date": creds["oauth_expiry_ms"],
            "source": "gemini-cli-import",
        })
        google_oauth_file.write_text(json.dumps(existing, indent=2), encoding="utf-8")
        return {
            "profile": profile_name,
            "status": "imported",
            "mode": "oauth",
            "provider": "google-gemini-cli",
            "source": creds["source"],
            "tos_warning": (
                "Google explicitly prohibits piggybacking on Gemini CLI OAuth tokens "
                "in third-party tools and began enforcing bans in March 2026. "
                "Use GEMINI_API_KEY from https://aistudio.google.com/app/apikey "
                "for ToS-compliant access."
            ),
        }

    return {"profile": profile_name, "status": "skipped-no-auth"}


def auto_import_gemini_profiles(
    cfg: dict[str, Any],
    *,
    use_oauth: bool = False,
) -> list[dict[str, Any]]:
    """Auto-import Gemini credentials into all profiles during setup."""
    creds = _detect_gemini_credentials()
    if not creds["api_key"] and not (use_oauth and (creds["oauth_token"] or creds["refresh_token"])):
        return [
            {"profile": p, "status": "skipped-no-auth"}
            for p in cfg.get("profiles", {})
        ]
    return [
        import_gemini_into_profile(cfg, profile_name=p, use_oauth=use_oauth)
        for p in cfg.get("profiles", {})
    ]


# ---------- Continue.dev credential import ----------

# Maps Continue provider slugs to the Hermes env var name
_CONTINUE_PROVIDER_ENV_MAP: dict[str, str] = {
    "openai": "OPENAI_API_KEY",
    "anthropic": "ANTHROPIC_API_KEY",
    "google": "GEMINI_API_KEY",
    "gemini": "GEMINI_API_KEY",
    "mistral": "MISTRAL_API_KEY",
    "deepseek": "DEEPSEEK_API_KEY",
    "xai": "XAI_API_KEY",
    "openrouter": "OPENROUTER_API_KEY",
    "huggingface": "HF_TOKEN",
    "nvidia": "NVIDIA_API_KEY",
    "groq": "GROQ_API_KEY",
    "together": "TOGETHER_API_KEY",
    "cohere": "COHERE_API_KEY",
    "fireworks": "FIREWORKS_API_KEY",
    "perplexity": "PERPLEXITY_API_KEY",
}


def _detect_continue_credentials(
    source_home: Path | None = None,
) -> dict[str, str]:
    """Detect API keys stored in a Continue.dev config file.

    Reads ~/.continue/config.yaml (new format) or ~/.continue/config.json
    (deprecated format) and extracts provider→apiKey mappings.

    Returns a dict mapping Hermes env var names to key values.
    Keys stored as 'localEnv:VAR_NAME' references are resolved from the
    current environment; entries without resolvable values are skipped.
    """
    continue_home = source_home or (Path.home() / ".continue")
    found: dict[str, str] = {}

    def _resolve_key(raw: str) -> str | None:
        raw = (raw or "").strip()
        if raw.startswith("localEnv:"):
            return os.environ.get(raw[len("localEnv:"):], "").strip() or None
        return raw or None

    def _extract_from_models(models: list[Any]) -> None:
        for model in models:
            if not isinstance(model, dict):
                continue
            provider = (model.get("provider") or "").strip().lower()
            api_key = _resolve_key(model.get("apiKey") or model.get("api_key") or "")
            env_var = _CONTINUE_PROVIDER_ENV_MAP.get(provider)
            if env_var and api_key and env_var not in found:
                found[env_var] = api_key

    yaml_cfg = continue_home / "config.yaml"
    json_cfg = continue_home / "config.json"

    if yaml_cfg.exists():
        try:
            data = yaml.safe_load(yaml_cfg.read_text(encoding="utf-8")) or {}
            _extract_from_models(data.get("models") or [])
        except Exception:
            pass

    if json_cfg.exists():
        try:
            data = json.loads(json_cfg.read_text(encoding="utf-8"))
            _extract_from_models(data.get("models") or [])
        except (json.JSONDecodeError, OSError):
            pass

    return found


def import_continue_into_profile(
    cfg: dict[str, Any],
    *,
    profile_name: str,
    source_continue_home: Path | None = None,
) -> dict[str, Any]:
    """Import API keys from a Continue.dev config into a TAG-managed Hermes profile.

    Reads ~/.continue/config.yaml (or config.json) and writes each discovered
    provider API key to the profile's .env file. Only env-var style keys are
    written — no OAuth tokens are involved, so there is no ToS risk.
    """
    ensure_runtime_dirs(cfg)
    target_home = profile_home(cfg, profile_name)
    if not target_home.exists():
        return {"profile": profile_name, "status": "profile-missing"}

    keys = _detect_continue_credentials(source_continue_home)
    if not keys:
        return {"profile": profile_name, "status": "skipped-no-auth"}

    env_file = target_home / ".env"
    for env_var, value in keys.items():
        _upsert_env_line(env_file, env_var, value)

    return {
        "profile": profile_name,
        "status": "imported",
        "mode": "api_keys",
        "providers_imported": list(keys.keys()),
    }


def auto_import_continue_profiles(cfg: dict[str, Any]) -> list[dict[str, Any]]:
    """Auto-import Continue.dev API keys into all profiles during setup."""
    keys = _detect_continue_credentials()
    if not keys:
        return [
            {"profile": p, "status": "skipped-no-auth"}
            for p in cfg.get("profiles", {})
        ]
    return [
        import_continue_into_profile(cfg, profile_name=p)
        for p in cfg.get("profiles", {})
    ]


# ---------- Mistral Vibe credential import ----------


def _detect_mistral_credentials(
    source_home: Path | None = None,
) -> dict[str, Any]:
    """Detect Mistral API key from Mistral Vibe CLI config.

    Checks, in order:
      1. MISTRAL_API_KEY env var
      2. ~/.vibe/.env (written by `mistral-vibe` CLI on first auth)
    """
    result: dict[str, Any] = {"api_key": None, "source": None}

    api_key = os.environ.get("MISTRAL_API_KEY", "").strip()
    if api_key:
        result["api_key"] = api_key
        return result

    vibe_home = source_home or (Path.home() / ".vibe")
    vibe_dotenv = vibe_home / ".env"
    if vibe_dotenv.exists():
        key = read_dotenv(vibe_dotenv).get("MISTRAL_API_KEY", "").strip()
        if key:
            result["api_key"] = key
            result["source"] = str(vibe_dotenv)

    return result


def import_mistral_into_profile(
    cfg: dict[str, Any],
    *,
    profile_name: str,
    source_vibe_home: Path | None = None,
) -> dict[str, Any]:
    """Import Mistral API key from the Mistral Vibe CLI into a TAG-managed profile."""
    ensure_runtime_dirs(cfg)
    target_home = profile_home(cfg, profile_name)
    if not target_home.exists():
        return {"profile": profile_name, "status": "profile-missing"}

    creds = _detect_mistral_credentials(source_vibe_home)
    if not creds["api_key"]:
        return {"profile": profile_name, "status": "skipped-no-auth"}

    _upsert_env_line(target_home / ".env", "MISTRAL_API_KEY", creds["api_key"])
    return {
        "profile": profile_name,
        "status": "imported",
        "mode": "api_key",
        "provider": "mistral",
        "source": creds.get("source"),
    }


def auto_import_mistral_profiles(cfg: dict[str, Any]) -> list[dict[str, Any]]:
    """Auto-import Mistral API key into all profiles during setup."""
    creds = _detect_mistral_credentials()
    if not creds["api_key"]:
        return [
            {"profile": p, "status": "skipped-no-auth"}
            for p in cfg.get("profiles", {})
        ]
    return [
        import_mistral_into_profile(cfg, profile_name=p)
        for p in cfg.get("profiles", {})
    ]


# ---------- opencode credential import ----------

_OPENCODE_PROVIDER_ENV_MAP: dict[str, str] = {
    "anthropic": "ANTHROPIC_API_KEY",
    "openai": "OPENAI_API_KEY",
    "google": "GEMINI_API_KEY",
    "google-vertex-ai": "GEMINI_API_KEY",
    "groq": "GROQ_API_KEY",
    "deepseek": "DEEPSEEK_API_KEY",
    "openrouter": "OPENROUTER_API_KEY",
    "xai": "XAI_API_KEY",
    "mistral": "MISTRAL_API_KEY",
    "fireworks": "FIREWORKS_API_KEY",
    "together": "TOGETHER_API_KEY",
    "perplexity": "PERPLEXITY_API_KEY",
    "cohere": "COHERE_API_KEY",
    "nvidia": "NVIDIA_API_KEY",
    "github": "GITHUB_TOKEN",
}


def _detect_opencode_credentials(
    source_data_dir: Path | None = None,
) -> dict[str, str]:
    """Detect API keys in ~/.local/share/opencode/auth.json.

    opencode writes: {"<provider-id>": {"type": "api", "key": "<key>"}}
    Returns a dict mapping env var names to key values.
    """
    data_dir = source_data_dir or (Path.home() / ".local" / "share" / "opencode")
    auth_file = data_dir / "auth.json"
    found: dict[str, str] = {}
    if not auth_file.exists():
        return found
    try:
        data = json.loads(auth_file.read_text(encoding="utf-8"))
    except (json.JSONDecodeError, OSError):
        return found
    for provider_id, cred in data.items():
        if not isinstance(cred, dict) or cred.get("type") != "api":
            continue
        key = (cred.get("key") or "").strip()
        env_var = _OPENCODE_PROVIDER_ENV_MAP.get(provider_id.lower())
        if key and env_var and env_var not in found:
            found[env_var] = key
    return found


def import_opencode_into_profile(
    cfg: dict[str, Any],
    *,
    profile_name: str,
    source_data_dir: Path | None = None,
) -> dict[str, Any]:
    """Import API keys from opencode auth.json into a TAG-managed Hermes profile."""
    ensure_runtime_dirs(cfg)
    target_home = profile_home(cfg, profile_name)
    if not target_home.exists():
        return {"profile": profile_name, "status": "profile-missing"}
    keys = _detect_opencode_credentials(source_data_dir)
    if not keys:
        return {"profile": profile_name, "status": "skipped-no-auth"}
    env_file = target_home / ".env"
    for env_var, value in keys.items():
        _upsert_env_line(env_file, env_var, value)
    return {
        "profile": profile_name,
        "status": "imported",
        "mode": "api_keys",
        "providers_imported": list(keys.keys()),
    }


def auto_import_opencode_profiles(cfg: dict[str, Any]) -> list[dict[str, Any]]:
    """Auto-import opencode API keys into all profiles during setup."""
    keys = _detect_opencode_credentials()
    if not keys:
        return [{"profile": p, "status": "skipped-no-auth"} for p in cfg.get("profiles", {})]
    return [import_opencode_into_profile(cfg, profile_name=p) for p in cfg.get("profiles", {})]


# ---------- Zed editor credential import ----------


def _detect_zed_credentials(
    source_zed_config: Path | None = None,
) -> dict[str, str]:
    """Detect API keys from Zed editor's settings.json.

    Zed stores most keys in the OS keychain, but users may set api_key under
    language_models.<provider> in settings.json for custom endpoints or older
    configs. Also checks standard env vars that Zed reads.
    Returns a dict mapping env var names to key values.
    """
    zed_settings = source_zed_config or (Path.home() / ".config" / "zed" / "settings.json")
    found: dict[str, str] = {}

    zed_provider_env_map = {
        "openai": "OPENAI_API_KEY",
        "anthropic": "ANTHROPIC_API_KEY",
        "google": "GEMINI_API_KEY",
        "mistral": "MISTRAL_API_KEY",
        "deepseek": "DEEPSEEK_API_KEY",
        "xai": "XAI_API_KEY",
        "groq": "GROQ_API_KEY",
        "ollama": None,
    }

    if zed_settings.exists():
        try:
            data = json.loads(zed_settings.read_text(encoding="utf-8"))
            lm = data.get("language_models") or {}
            for provider, cfg_block in lm.items():
                if not isinstance(cfg_block, dict):
                    continue
                key = (cfg_block.get("api_key") or "").strip()
                env_var = zed_provider_env_map.get(provider.lower())
                if key and env_var and env_var not in found:
                    found[env_var] = key
        except (json.JSONDecodeError, OSError):
            pass

    return found


def import_zed_into_profile(
    cfg: dict[str, Any],
    *,
    profile_name: str,
    source_zed_config: Path | None = None,
) -> dict[str, Any]:
    """Import API keys from Zed editor settings into a TAG-managed Hermes profile."""
    ensure_runtime_dirs(cfg)
    target_home = profile_home(cfg, profile_name)
    if not target_home.exists():
        return {"profile": profile_name, "status": "profile-missing"}
    keys = _detect_zed_credentials(source_zed_config)
    if not keys:
        return {"profile": profile_name, "status": "skipped-no-auth"}
    env_file = target_home / ".env"
    for env_var, value in keys.items():
        _upsert_env_line(env_file, env_var, value)
    return {
        "profile": profile_name,
        "status": "imported",
        "mode": "api_keys",
        "providers_imported": list(keys.keys()),
    }


def auto_import_zed_profiles(cfg: dict[str, Any]) -> list[dict[str, Any]]:
    """Auto-import Zed API keys into all profiles during setup."""
    keys = _detect_zed_credentials()
    if not keys:
        return [{"profile": p, "status": "skipped-no-auth"} for p in cfg.get("profiles", {})]
    return [import_zed_into_profile(cfg, profile_name=p) for p in cfg.get("profiles", {})]


# ---------- GitHub Copilot credential import ----------


def _detect_copilot_credentials(
    source_gh_config: Path | None = None,
) -> dict[str, Any]:
    """Detect GitHub OAuth token from the gh CLI credential store.

    Reads ~/.config/gh/hosts.yml, field github.com.oauth_token.
    Returns dict with keys: github_token, source.
    """
    result: dict[str, Any] = {"github_token": None, "source": None}

    gh_token = os.environ.get("GITHUB_TOKEN", "").strip() or os.environ.get("GH_TOKEN", "").strip()
    if gh_token:
        result["github_token"] = gh_token
        return result

    hosts_file = source_gh_config or (Path.home() / ".config" / "gh" / "hosts.yml")
    if hosts_file.exists():
        try:
            data = yaml.safe_load(hosts_file.read_text(encoding="utf-8")) or {}
            token = (
                (data.get("github.com") or {}).get("oauth_token", "")
                or (data.get("github.com") or {}).get("token", "")
            ).strip()
            if token:
                result["github_token"] = token
                result["source"] = str(hosts_file)
        except Exception:
            pass

    return result


def import_copilot_into_profile(
    cfg: dict[str, Any],
    *,
    profile_name: str,
    source_gh_config: Path | None = None,
) -> dict[str, Any]:
    """Import GitHub OAuth token from gh CLI into a TAG-managed Hermes profile.

    Writes GITHUB_TOKEN to the profile .env so Hermes can reach GitHub Copilot
    and GitHub Models APIs via OpenAI-compatible endpoints.
    """
    ensure_runtime_dirs(cfg)
    target_home = profile_home(cfg, profile_name)
    if not target_home.exists():
        return {"profile": profile_name, "status": "profile-missing"}
    creds = _detect_copilot_credentials(source_gh_config)
    if not creds["github_token"]:
        return {"profile": profile_name, "status": "skipped-no-auth"}
    _upsert_env_line(target_home / ".env", "GITHUB_TOKEN", creds["github_token"])
    return {
        "profile": profile_name,
        "status": "imported",
        "mode": "oauth_token",
        "provider": "github-copilot",
        "source": creds.get("source"),
    }


def auto_import_copilot_profiles(cfg: dict[str, Any]) -> list[dict[str, Any]]:
    """Auto-import GitHub Copilot token into all profiles during setup."""
    creds = _detect_copilot_credentials()
    if not creds["github_token"]:
        return [{"profile": p, "status": "skipped-no-auth"} for p in cfg.get("profiles", {})]
    return [import_copilot_into_profile(cfg, profile_name=p) for p in cfg.get("profiles", {})]


# ---------- Aider credential import ----------

_AIDER_YAML_KEY_MAP: dict[str, str] = {
    "openai-api-key": "OPENAI_API_KEY",
    "anthropic-api-key": "ANTHROPIC_API_KEY",
    "gemini-api-key": "GEMINI_API_KEY",
    "deepseek-api-key": "DEEPSEEK_API_KEY",
    "openrouter-api-key": "OPENROUTER_API_KEY",
    "mistral-api-key": "MISTRAL_API_KEY",
    "groq-api-key": "GROQ_API_KEY",
    "xai-api-key": "XAI_API_KEY",
    "cohere-api-key": "COHERE_API_KEY",
    "perplexity-api-key": "PERPLEXITY_API_KEY",
}

_AIDER_API_KEY_PREFIX_MAP: dict[str, str] = {
    "anthropic": "ANTHROPIC_API_KEY",
    "gemini": "GEMINI_API_KEY",
    "openrouter": "OPENROUTER_API_KEY",
    "mistral": "MISTRAL_API_KEY",
    "groq": "GROQ_API_KEY",
    "deepseek": "DEEPSEEK_API_KEY",
    "xai": "XAI_API_KEY",
    "cohere": "COHERE_API_KEY",
    "perplexity": "PERPLEXITY_API_KEY",
    "together": "TOGETHER_API_KEY",
    "fireworks": "FIREWORKS_API_KEY",
}


def _detect_aider_credentials(
    source_home: Path | None = None,
) -> dict[str, str]:
    """Detect API keys from Aider config files.

    Checks, in order:
      1. ~/.aider.conf.yml  (YAML, openai-api-key / anthropic-api-key / api-key list)
      2. ~/.env             (standard KEY=VALUE, Aider reads this by default)
      3. ~/.aider.env       (explicit Aider-only dotenv)

    Returns a dict mapping env var names to key values.
    """
    base = source_home or Path.home()
    found: dict[str, str] = {}

    aider_yaml = base / ".aider.conf.yml"
    if aider_yaml.exists():
        try:
            data = yaml.safe_load(aider_yaml.read_text(encoding="utf-8")) or {}
            for yaml_key, env_var in _AIDER_YAML_KEY_MAP.items():
                val = (str(data.get(yaml_key) or "")).strip()
                if val and env_var not in found:
                    found[env_var] = val
            api_key_list = data.get("api-key") or []
            if isinstance(api_key_list, list):
                for entry in api_key_list:
                    entry = (str(entry) or "").strip()
                    if "=" in entry:
                        prefix, _, val = entry.partition("=")
                        env_var = _AIDER_API_KEY_PREFIX_MAP.get(prefix.strip().lower())
                        if val.strip() and env_var and env_var not in found:
                            found[env_var] = val.strip()
        except Exception:
            pass

    for dotenv_path in (base / ".env", base / ".aider.env"):
        if dotenv_path.exists():
            for env_var in (
                "OPENAI_API_KEY", "ANTHROPIC_API_KEY", "GEMINI_API_KEY",
                "OPENROUTER_API_KEY", "MISTRAL_API_KEY", "GROQ_API_KEY",
                "DEEPSEEK_API_KEY", "XAI_API_KEY", "PERPLEXITY_API_KEY",
                "COHERE_API_KEY", "TOGETHER_API_KEY", "FIREWORKS_API_KEY",
            ):
                val = read_dotenv(dotenv_path).get(env_var, "").strip()
                if val and env_var not in found:
                    found[env_var] = val

    return found


def import_aider_into_profile(
    cfg: dict[str, Any],
    *,
    profile_name: str,
    source_home: Path | None = None,
) -> dict[str, Any]:
    """Import API keys from Aider config into a TAG-managed Hermes profile."""
    ensure_runtime_dirs(cfg)
    target_home = profile_home(cfg, profile_name)
    if not target_home.exists():
        return {"profile": profile_name, "status": "profile-missing"}
    keys = _detect_aider_credentials(source_home)
    if not keys:
        return {"profile": profile_name, "status": "skipped-no-auth"}
    env_file = target_home / ".env"
    for env_var, value in keys.items():
        _upsert_env_line(env_file, env_var, value)
    return {
        "profile": profile_name,
        "status": "imported",
        "mode": "api_keys",
        "providers_imported": list(keys.keys()),
    }


def auto_import_aider_profiles(cfg: dict[str, Any]) -> list[dict[str, Any]]:
    """Auto-import Aider API keys into all profiles during setup."""
    keys = _detect_aider_credentials()
    if not keys:
        return [{"profile": p, "status": "skipped-no-auth"} for p in cfg.get("profiles", {})]
    return [import_aider_into_profile(cfg, profile_name=p) for p in cfg.get("profiles", {})]


# ---------- AWS / Amazon Q credential import ----------


def _detect_aws_credentials(
    source_aws_dir: Path | None = None,
) -> dict[str, Any]:
    """Detect AWS credentials from ~/.aws/credentials and ~/.aws/config.

    Reads the [default] profile by default. Useful for Amazon Q Developer,
    Amazon Bedrock, and Kiro IDE which all use standard AWS credentials.
    Returns dict with: access_key_id, secret_access_key, session_token,
    region, source.
    """
    result: dict[str, Any] = {
        "access_key_id": None,
        "secret_access_key": None,
        "session_token": None,
        "region": None,
        "source": None,
    }

    result["access_key_id"] = os.environ.get("AWS_ACCESS_KEY_ID", "").strip() or None
    result["secret_access_key"] = os.environ.get("AWS_SECRET_ACCESS_KEY", "").strip() or None
    result["session_token"] = os.environ.get("AWS_SESSION_TOKEN", "").strip() or None
    result["region"] = os.environ.get("AWS_DEFAULT_REGION", "").strip() or None

    aws_dir = source_aws_dir or (Path.home() / ".aws")
    credentials_file = aws_dir / "credentials"
    config_file = aws_dir / "config"

    def _read_ini_section(path: Path, section: str) -> dict[str, str]:
        try:
            import configparser
            cp = configparser.ConfigParser()
            cp.read(str(path))
            if cp.has_section(section):
                return dict(cp[section])
        except Exception:
            pass
        return {}

    if credentials_file.exists() and not result["access_key_id"]:
        creds = _read_ini_section(credentials_file, "default")
        result["access_key_id"] = creds.get("aws_access_key_id", "").strip() or None
        result["secret_access_key"] = creds.get("aws_secret_access_key", "").strip() or None
        result["session_token"] = creds.get("aws_session_token", "").strip() or None
        if result["access_key_id"]:
            result["source"] = str(credentials_file)

    if config_file.exists() and not result["region"]:
        cfg_data = _read_ini_section(config_file, "default")
        result["region"] = cfg_data.get("region", "").strip() or None

    return result


def import_aws_into_profile(
    cfg: dict[str, Any],
    *,
    profile_name: str,
    source_aws_dir: Path | None = None,
    aws_profile: str = "default",
) -> dict[str, Any]:
    """Import AWS credentials into a TAG-managed Hermes profile.

    Writes AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY, and optionally
    AWS_SESSION_TOKEN and AWS_DEFAULT_REGION to the profile .env.
    These are required for Amazon Bedrock and Amazon Q Developer access.
    """
    ensure_runtime_dirs(cfg)
    target_home = profile_home(cfg, profile_name)
    if not target_home.exists():
        return {"profile": profile_name, "status": "profile-missing"}

    creds = _detect_aws_credentials(source_aws_dir)
    if not creds["access_key_id"] or not creds["secret_access_key"]:
        return {"profile": profile_name, "status": "skipped-no-auth"}

    env_file = target_home / ".env"
    _upsert_env_line(env_file, "AWS_ACCESS_KEY_ID", creds["access_key_id"])
    _upsert_env_line(env_file, "AWS_SECRET_ACCESS_KEY", creds["secret_access_key"])
    if creds["session_token"]:
        _upsert_env_line(env_file, "AWS_SESSION_TOKEN", creds["session_token"])
    if creds["region"]:
        _upsert_env_line(env_file, "AWS_DEFAULT_REGION", creds["region"])

    return {
        "profile": profile_name,
        "status": "imported",
        "mode": "access_key",
        "provider": "aws-bedrock",
        "source": creds.get("source"),
    }


def auto_import_aws_profiles(cfg: dict[str, Any]) -> list[dict[str, Any]]:
    """Auto-import AWS credentials into all profiles during setup."""
    creds = _detect_aws_credentials()
    if not creds["access_key_id"] or not creds["secret_access_key"]:
        return [{"profile": p, "status": "skipped-no-auth"} for p in cfg.get("profiles", {})]
    return [import_aws_into_profile(cfg, profile_name=p) for p in cfg.get("profiles", {})]


# ---------- Cursor IDE credential import ----------


def _detect_cursor_credentials(
    source_cursor_dir: Path | None = None,
) -> dict[str, str]:
    """Detect API keys stored in Cursor's globalStorage/state.vscdb SQLite database.

    Cursor stores user-entered BYOK (bring-your-own-key) API keys in a SQLite
    ItemTable. Known key patterns are searched; API key values are matched by
    their well-known prefixes (sk-, sk-ant-, AIza-).

    Returns a dict mapping env var names to key values.
    """
    import sqlite3

    found: dict[str, str] = {}

    if source_cursor_dir:
        db_candidates = [source_cursor_dir / "state.vscdb"]
    else:
        db_candidates = [
            Path.home() / "Library" / "Application Support" / "Cursor" / "User" / "globalStorage" / "state.vscdb",
            Path.home() / ".config" / "Cursor" / "User" / "globalStorage" / "state.vscdb",
        ]

    db_path = next((p for p in db_candidates if p.exists()), None)
    if not db_path:
        return found

    known_key_map = {
        "openai.apiKey": "OPENAI_API_KEY",
        "cursor.openaiApiKey": "OPENAI_API_KEY",
        "anthropic.apiKey": "ANTHROPIC_API_KEY",
        "cursor.anthropicApiKey": "ANTHROPIC_API_KEY",
        "gemini.apiKey": "GEMINI_API_KEY",
        "cursor.googleApiKey": "GEMINI_API_KEY",
    }

    api_key_value_patterns: list[tuple[str, str]] = [
        ("sk-ant-", "ANTHROPIC_API_KEY"),
        ("sk-or-", "OPENROUTER_API_KEY"),
        ("AIza", "GEMINI_API_KEY"),
    ]

    try:
        conn = sqlite3.connect(f"file:{db_path}?mode=ro", uri=True)
        try:
            rows = conn.execute("SELECT key, value FROM ItemTable").fetchall()
        finally:
            conn.close()
    except Exception:
        return found

    for db_key, db_value in rows:
        if not db_value or not isinstance(db_value, str):
            continue
        value = db_value.strip()
        env_var = known_key_map.get(db_key)
        if env_var and value and env_var not in found:
            found[env_var] = value
            continue
        for prefix, env_var in api_key_value_patterns:
            if value.startswith(prefix) and env_var not in found:
                found[env_var] = value
                break

    return found


def import_cursor_into_profile(
    cfg: dict[str, Any],
    *,
    profile_name: str,
    source_cursor_dir: Path | None = None,
) -> dict[str, Any]:
    """Import API keys from Cursor's local SQLite store into a TAG-managed profile."""
    ensure_runtime_dirs(cfg)
    target_home = profile_home(cfg, profile_name)
    if not target_home.exists():
        return {"profile": profile_name, "status": "profile-missing"}
    keys = _detect_cursor_credentials(source_cursor_dir)
    if not keys:
        return {"profile": profile_name, "status": "skipped-no-auth"}
    env_file = target_home / ".env"
    for env_var, value in keys.items():
        _upsert_env_line(env_file, env_var, value)
    return {
        "profile": profile_name,
        "status": "imported",
        "mode": "api_keys",
        "providers_imported": list(keys.keys()),
    }


def auto_import_cursor_profiles(cfg: dict[str, Any]) -> list[dict[str, Any]]:
    """Auto-import Cursor IDE API keys into all profiles during setup."""
    keys = _detect_cursor_credentials()
    if not keys:
        return [{"profile": p, "status": "skipped-no-auth"} for p in cfg.get("profiles", {})]
    return [import_cursor_into_profile(cfg, profile_name=p) for p in cfg.get("profiles", {})]


def ensure_hermes_ready(
    cfg: dict[str, Any],
    *,
    config_arg: str | None,
    need_tui: bool,
) -> None:
    if hermes_bin(cfg).exists():
        return
    setup_args = argparse.Namespace(
        config=config_arg,
        refresh=False,
        skip_python_install=False,
        skip_tui_build=not need_tui,
        json=False,
    )
    cmd_setup(setup_args)


def normalize_hermes_passthrough_args(args: list[str]) -> list[str]:
    normalized = list(args)
    if normalized[:1] == ["--"]:
        normalized = normalized[1:]
    if len(normalized) >= 2 and normalized[1] == "--":
        normalized = [normalized[0], *normalized[2:]]
    if not normalized:
        return ["--help"]
    return normalized


def cmd_setup(args: argparse.Namespace) -> int:
    cfg = load_config(config_path(args.config))
    benchmark_path = benchmark_suite_path(None)
    needs_git = bool(args.refresh or not bundled_hermes_archive().exists())
    ensure_setup_prereqs(cfg, need_npm=not args.skip_tui_build, need_git=needs_git)
    ensure_runtime_dirs(cfg)
    steps = {
        "config": {"config": str(config_path(args.config)), "benchmark_suite": str(benchmark_path)},
        "prerequisites": doctor_prerequisites(cfg),
        "clone": clone_or_update_hermes(cfg, refresh=args.refresh),
        "venv": ensure_venv(cfg),
    }
    if not args.skip_python_install:
        steps["python_install"] = install_hermes_python(cfg)
    steps["patch"] = apply_hermes_patch(cfg)
    if not args.skip_tui_build:
        steps["tui"] = install_tui_dependencies(cfg)
    if not hermes_bin(cfg).exists():
        raise SystemExit(
            "The managed runtime Python is not installed; cannot bootstrap profiles. "
            "Re-run `tag setup` without `--skip-python-install`."
        )
    steps["bootstrap"] = {
        "profiles": bootstrap_profiles(cfg),
        "rendered": render_profiles(cfg, force=False),
    }
    steps["codex_import"] = auto_import_codex_profiles(cfg)
    steps["claude_import"] = auto_import_claude_profiles(cfg)
    steps["gemini_import"] = auto_import_gemini_profiles(cfg)
    steps["continue_import"] = auto_import_continue_profiles(cfg)
    steps["mistral_import"] = auto_import_mistral_profiles(cfg)

    if args.json:
        print(json.dumps(steps, indent=2))
        return 0

    for name, payload in steps.items():
        print(f"{name}: {payload}")
    return 0


def cmd_hermes_passthrough(args: argparse.Namespace) -> int:
    cfg = load_config(config_path(args.config))
    ensure_hermes_ready(
        cfg,
        config_arg=args.config,
        need_tui="--tui" in args.hermes_args,
    )
    env = profile_exec_env(cfg, args.profile) if args.profile else hermes_env(cfg)
    raw_args = list(args.hermes_args)
    hermes_args = normalize_hermes_passthrough_args(raw_args)
    wants_help = any(arg in {"--help", "-h"} for arg in hermes_args)
    if getattr(args, "hermes_version", False):
        if not raw_args:
            hermes_args = ["--version"]
        else:
            hermes_args = ["--version", *hermes_args]
            wants_help = True
    interactive_passthrough = (
        "--tui" in hermes_args
        or (
            hermes_args[:1] in (["gateway"], ["dashboard"])
            and not wants_help
        )
        or (
            hermes_args[:1] == ["chat"]
            and "-q" not in hermes_args
            and "--query" not in hermes_args
            and not wants_help
        )
    )
    capture_output = not interactive_passthrough
    proc = subprocess.run(
        [str(hermes_bin(cfg)), *hermes_args],
        env=env,
        text=True,
        check=False,
        capture_output=capture_output,
    )
    if capture_output:
        stdout = getattr(proc, "stdout", "")
        stderr = getattr(proc, "stderr", "")
        if stdout:
            print(rewrite_cli_hints(stdout), end="")
        if stderr:
            print(rewrite_cli_hints(stderr), end="", file=sys.stderr)
    return int(proc.returncode)


def cmd_tui(args: argparse.Namespace) -> int:
    raw_args = list(args.hermes_args)
    normalized_args = normalize_hermes_passthrough_args(raw_args)
    if raw_args and normalized_args in (["--help"], ["-h"]):
        passthrough = argparse.Namespace(
            config=args.config,
            profile=args.profile,
            hermes_args=["--help"],
            hermes_version=False,
        )
        return cmd_hermes_passthrough(passthrough)
    if not can_launch_interactive_tui() and os.environ.get("TAG_FORCE_TUI", "").strip() not in {"1", "true", "yes"}:
        print(
            "TAG TUI requires an interactive terminal. Use `tag doctor`, `tag setup`, "
            "`tag submit ...`, or rerun in a real TTY. Set TAG_FORCE_TUI=1 to bypass this guard.",
            file=sys.stderr,
        )
        return 2
    forwarded = ["--tui", *args.hermes_args]
    passthrough = argparse.Namespace(
        config=args.config,
        profile=args.profile,
        hermes_args=forwarded,
        hermes_version=False,
    )
    return cmd_hermes_passthrough(passthrough)


def cmd_hermes_command(args: argparse.Namespace, command_name: str) -> int:
    forwarded = [command_name, *args.hermes_args]
    passthrough = argparse.Namespace(
        config=args.config,
        profile=args.profile,
        hermes_args=forwarded,
        hermes_version=False,
    )
    return cmd_hermes_passthrough(passthrough)


def cmd_chat(args: argparse.Namespace) -> int:
    return cmd_hermes_command(args, "chat")


def cmd_gateway(args: argparse.Namespace) -> int:
    return cmd_hermes_command(args, "gateway")


def cmd_kanban(args: argparse.Namespace) -> int:
    return cmd_hermes_command(args, "kanban")


def cmd_model(args: argparse.Namespace) -> int:
    return cmd_hermes_command(args, "model")


def cmd_profile(args: argparse.Namespace) -> int:
    return cmd_hermes_command(args, "profile")


def cmd_status(args: argparse.Namespace) -> int:
    return cmd_hermes_command(args, "status")


def cmd_config(args: argparse.Namespace) -> int:
    return cmd_hermes_command(args, "config")


def cmd_sessions(args: argparse.Namespace) -> int:
    return cmd_hermes_command(args, "sessions")


def cmd_skills(args: argparse.Namespace) -> int:
    return cmd_hermes_command(args, "skills")


def cmd_plugins(args: argparse.Namespace) -> int:
    return cmd_hermes_command(args, "plugins")


def cmd_tools(args: argparse.Namespace) -> int:
    return cmd_hermes_command(args, "tools")


def cmd_mcp(args: argparse.Namespace) -> int:
    return cmd_hermes_command(args, "mcp")


def cmd_logs(args: argparse.Namespace) -> int:
    return cmd_hermes_command(args, "logs")


def _dashboard_snapshot(cfg: dict[str, Any]) -> dict[str, Any]:
    """Read current TAG state for dashboard display — pure SQLite, no hermes."""
    snap: dict[str, Any] = {"runs": [], "queue": [], "journal_count": 0, "kanban": {}}
    try:
        db = open_db(cfg)
        # Recent runs (last 20)
        rows = db.execute(
            "SELECT id AS run_id, kind, task_type, master_profile, status, "
            "created_at FROM runs ORDER BY created_at DESC LIMIT 20"
        ).fetchall()
        snap["runs"] = [dict(r) for r in rows]
        # Queue jobs
        snap["queue"] = queue_list_jobs(db, status=None)
        # Journal entry count across all profiles
        snap["journal_count"] = db.execute(
            "SELECT COUNT(*) FROM memory_journal"
        ).fetchone()[0]
        db.close()
    except Exception:
        pass

    # Per-profile kanban summary (direct SQLite reads, no hermes)
    kanban_by_profile: dict[str, Any] = {}
    for pname in cfg.get("profiles", {}):
        try:
            kpath = _kanban.profile_kanban_db_path(cfg, pname)
            if not kpath.exists():
                continue
            kconn = _kanban.open_db(kpath)
            tasks = _kanban.list_tasks(kconn)
            kconn.close()
            by_status: dict[str, int] = {}
            for t in tasks:
                by_status[t["status"]] = by_status.get(t["status"], 0) + 1
            kanban_by_profile[pname] = {"total": len(tasks), "by_status": by_status}
        except Exception:
            pass
    snap["kanban"] = kanban_by_profile
    return snap


def _render_dashboard_plain(snap: dict[str, Any], profile: str) -> None:
    """Print a static dashboard snapshot (fallback when Rich unavailable)."""
    import datetime
    print(f"\n=== TAG Dashboard  (profile: {profile}) ===")

    runs = snap.get("runs", [])
    print(f"\nRuns ({len(runs)} recent):")
    if runs:
        print(f"  {'ID':<10} {'KIND':<10} {'PROFILE':<16} {'STATUS':<12} WHEN")
        for r in runs[:10]:
            try:
                ts = datetime.datetime.fromisoformat(r.get("created_at") or "").strftime("%H:%M")
            except Exception:
                ts = "?"
            print(f"  {r['run_id']:<10} {r['kind']:<10} {r['master_profile']:<16} "
                  f"{r['status']:<12} {ts}")
    else:
        print("  (none)")

    queue = snap.get("queue", [])
    print(f"\nQueue ({len(queue)} jobs):")
    if queue:
        for j in queue[:8]:
            print(f"  [{j['id']}] {j['status']:<10} {j.get('task','')[:40]}")
    else:
        print("  (empty)")

    print(f"\nMemory journal entries: {snap.get('journal_count', 0)}")

    kanban = snap.get("kanban", {})
    if kanban:
        print("\nKanban boards:")
        for pname, info in kanban.items():
            by_s = info.get("by_status", {})
            parts = ", ".join(f"{s}:{n}" for s, n in by_s.items())
            print(f"  {pname}: {info['total']} tasks  [{parts}]")


def cmd_dashboard(args: argparse.Namespace) -> int:
    """TAG-native live dashboard — reads directly from TAG's SQLite state (PRD-010).

    No hermes binary dependency. Shows runs, queue, journal, and kanban
    board status for all profiles. Refreshes every few seconds.
    Use --no-browser to suppress the browser open (legacy flag, kept for
    CLI compat; dashboard is terminal-only).
    """
    cfg = load_config(config_path(args.config))
    profile = getattr(args, "profile", None) or cfg["defaults"]["master_profile"]
    if profile not in cfg.get("profiles", {}):
        print(f"warning: unknown profile '{profile}'", file=sys.stderr)
    refresh_secs = getattr(args, "port", None) or 3  # --port reused as refresh interval
    # Note: --port is repurposed here as refresh_seconds for the live view.
    # A value ≥10 is assumed to be a port (legacy hermes mode); ≤9 is refresh rate.
    if isinstance(refresh_secs, int) and refresh_secs >= 10:
        refresh_secs = 3

    try:
        from rich.console import Console
        from rich.live import Live
        from rich.table import Table
        from rich.panel import Panel
        from rich import box
        import datetime

        console = Console()

        def make_layout() -> Panel:
            snap = _dashboard_snapshot(cfg)

            run_table = Table(box=box.SIMPLE, show_header=True, header_style="bold cyan",
                              expand=True, min_width=60)
            run_table.add_column("ID", width=10)
            run_table.add_column("Kind", width=10)
            run_table.add_column("Profile", width=16)
            run_table.add_column("Status", width=12)
            run_table.add_column("When", width=8)
            for r in snap.get("runs", [])[:8]:
                try:
                    ts = datetime.datetime.fromisoformat(r.get("created_at") or "").strftime("%H:%M")
                except Exception:
                    ts = "?"
                s = r["status"]
                style = "green" if s == "completed" else "red" if s == "failed" else "yellow"
                run_table.add_row(r["run_id"], r["kind"], r["master_profile"],
                                  f"[{style}]{s}[/{style}]", ts)

            q_table = Table(box=box.SIMPLE, show_header=True, header_style="bold cyan",
                            expand=True, min_width=60)
            q_table.add_column("Job", width=10)
            q_table.add_column("Status", width=12)
            q_table.add_column("Task", width=40)
            for j in snap.get("queue", [])[:6]:
                s = j["status"]
                style = "green" if s == "done" else "red" if s in ("failed","cancelled") else "yellow"
                q_table.add_row(j["id"], f"[{style}]{s}[/{style}]", (j.get("task") or "")[:40])

            kb_table = Table(box=box.SIMPLE, show_header=True, header_style="bold cyan",
                             expand=True, min_width=40)
            kb_table.add_column("Profile", width=16)
            kb_table.add_column("Total", width=6)
            kb_table.add_column("Ready/Running", width=14)
            kb_table.add_column("Done", width=6)
            for pname, info in snap.get("kanban", {}).items():
                by_s = info.get("by_status", {})
                active = by_s.get("ready", 0) + by_s.get("running", 0)
                done = by_s.get("done", 0)
                kb_table.add_row(pname, str(info["total"]), str(active), str(done))

            from rich.columns import Columns
            from rich.text import Text
            header = Text(
                f"TAG Dashboard  ·  profile: {profile}  ·  "
                f"journal entries: {snap.get('journal_count', 0)}  ·  "
                f"Press Ctrl+C to exit",
                style="bold",
            )
            from rich.layout import Layout
            layout = Layout()
            layout.split_column(
                Layout(header, size=1),
                Layout(Panel(run_table, title="[bold]Runs[/bold]"), name="runs"),
                Layout(
                    Columns([
                        Panel(q_table, title="[bold]Queue[/bold]"),
                        Panel(kb_table, title="[bold]Kanban[/bold]"),
                    ]),
                    name="bottom",
                    size=12,
                ),
            )
            return Panel(layout, title="[bold blue]TAG[/bold blue]", border_style="blue")

        with Live(make_layout(), console=console, refresh_per_second=0.5,
                  screen=True) as live:
            while True:
                time.sleep(refresh_secs)
                live.update(make_layout())

    except KeyboardInterrupt:
        pass
    except ImportError:
        # Rich not available — static snapshot
        snap = _dashboard_snapshot(cfg)
        _render_dashboard_plain(snap, profile)
    return 0


def cmd_memory(args: argparse.Namespace) -> int:
    return cmd_hermes_command(args, "memory")


# ---------------------------------------------------------------------------
# PRD-002: Memory Journal command
# ---------------------------------------------------------------------------

def cmd_memory_journal(args: argparse.Namespace) -> int:
    """Tag-native cross-session memory journal (key→value facts per profile)."""
    cfg = load_config(config_path(args.config))
    ensure_runtime_dirs(cfg)
    db = open_db(cfg)
    sub = getattr(args, "mj_subcommand", None) or "list"
    profile = getattr(args, "profile", None) or cfg["defaults"]["master_profile"]

    if sub == "save":
        entry_id = journal_save(db, profile, args.key, args.value, ttl_days=getattr(args, "ttl_days", None))
        db.close()
        if getattr(args, "json", False):
            print(json.dumps({"id": entry_id, "profile": profile, "key": args.key}))
        else:
            print(f"saved: {entry_id}")
        return 0

    if sub == "list":
        entries = journal_list(db, profile)
        db.close()
        if getattr(args, "json", False):
            print(json.dumps(entries, indent=2))
            return 0
        if not entries:
            print(f"No memory journal entries for profile '{profile}'.")
            return 0
        for e in entries:
            exp = f" (expires {e['expires_at'][:10]})" if e.get("expires_at") else ""
            print(f"  [{e['id']}] {e['key']}: {e['value']}{exp}")
        return 0

    if sub == "forget":
        deleted = journal_forget(db, args.entry_id)
        db.close()
        if getattr(args, "json", False):
            print(json.dumps({"deleted": deleted}))
        else:
            print("deleted" if deleted else "not found")
        return 0 if deleted else 1

    if sub == "clear":
        if not getattr(args, "confirm", False):
            print("Pass --confirm to clear all journal entries for this profile.")
            db.close()
            return 1
        count = journal_clear(db, profile)
        db.close()
        if getattr(args, "json", False):
            print(json.dumps({"cleared": count}))
        else:
            print(f"cleared {count} entries")
        return 0

    db.close()
    return 0


# ---------------------------------------------------------------------------
# PRD-008: Background task queue command
# ---------------------------------------------------------------------------

def cmd_queue(args: argparse.Namespace) -> int:
    """Background task queue — submit, list, result, cancel."""
    cfg = load_config(config_path(args.config))
    ensure_runtime_dirs(cfg)
    db = open_db(cfg)
    sub = getattr(args, "queue_subcommand", "list")

    if sub == "add":
        task_text = (args.task or "").replace("\x00", "").strip()
        if not task_text:
            db.close()
            print("error: task text must not be empty.", file=sys.stderr)
            return 1
        job_id = uuid.uuid4().hex[:8]
        profile = getattr(args, "profile", None) or cfg["defaults"]["master_profile"]
        task_type = getattr(args, "task_type", "mixed") or "mixed"
        priority_arg = getattr(args, "priority", None)
        priority = priority_arg if priority_arg is not None else 5
        if not (1 <= priority <= 10):
            db.close()
            print(f"error: --priority must be between 1 and 10, got {priority}.", file=sys.stderr)
            return 1
        notify = not getattr(args, "no_notify", False)
        queue_insert_job(db, job_id, profile, task_text, task_type=task_type, priority=priority, notify=notify)
        pid = launch_queue_worker(cfg, job_id)
        queue_update_pid(db, job_id, pid)
        db.close()
        if getattr(args, "json", False):
            print(json.dumps({"job_id": job_id, "pid": pid, "status": "queued"}))
        else:
            print(f"queued: {job_id}  (worker pid {pid})")
        return 0

    if sub == "list":
        status_filter = getattr(args, "status_filter", None)
        limit = getattr(args, "limit", 50) or 50
        jobs = queue_list_jobs(db, status=status_filter, limit=limit + 1)
        total_indicator = len(jobs) > limit
        if total_indicator:
            jobs = jobs[:limit]
        db.close()
        if getattr(args, "json", False):
            print(json.dumps(jobs, indent=2))
            return 0
        if not jobs:
            print("No jobs in queue.")
            return 0
        print(f"  {'ID':<10} {'STATUS':<12} {'PROFILE':<16} {'TASK'}")
        print("  " + "─" * 70)
        for j in jobs:
            task_short = (j.get("task") or "")[:40]
            print(f"  {j['id']:<10} {j['status']:<12} {j.get('profile','?'):<16} {task_short}")
        if total_indicator:
            print(f"  (showing {limit} of more — use --limit N to see more)")
        return 0

    if sub == "result":
        job = queue_get_job(db, args.job_id)
        db.close()
        if not job:
            print(f"Job '{args.job_id}' not found.", file=sys.stderr)
            return 1
        result_path = job.get("result_path")
        if result_path and Path(result_path).exists():
            print(Path(result_path).read_text())
        else:
            print(f"No result yet (status: {job['status']})")
        return 0

    if sub == "cancel":
        job = queue_get_job(db, args.job_id)
        if not job:
            db.close()
            print(f"Job '{args.job_id}' not found.", file=sys.stderr)
            return 1
        if job["status"] in ("done", "failed", "cancelled"):
            db.close()
            print(f"Job '{args.job_id}' is already {job['status']}.", file=sys.stderr)
            return 1
        pid = job.get("pid")
        if pid:
            import signal as _signal
            try:
                os.kill(pid, _signal.SIGTERM)
            except ProcessLookupError:
                pass
        queue_update_status(db, args.job_id, "cancelled")
        db.close()
        if getattr(args, "json", False):
            print(json.dumps({"job_id": args.job_id, "status": "cancelled"}))
        else:
            print(f"cancelled: {args.job_id}")
        return 0

    if sub == "clear":
        count = queue_clear_completed(db)
        db.close()
        if getattr(args, "json", False):
            print(json.dumps({"cleared": count}))
        else:
            print(f"cleared {count} completed/failed jobs")
        return 0

    db.close()
    return 0


# ---------------------------------------------------------------------------
# PRD-004: Kanban swarm topology helper (native, no hermes binary required)
# ---------------------------------------------------------------------------

import tag.kanban as _kanban


def _try_start_gateway(cfg: dict[str, Any], profile_name: str) -> None:
    """Best-effort: start hermes gateway so it can dispatch tasks we created.

    Management-plane operations (create/monitor tasks) don't need this.
    Execution-plane (AI agents running tasks) does. Fire-and-forget.
    """
    try:
        hbin = hermes_bin(cfg)
        if not hbin.exists():
            return
        env = profile_exec_env(cfg, profile_name)
        result = subprocess.run(
            [str(hbin), "gateway", "status"],
            env={**os.environ, **env},
            capture_output=True,
            timeout=5,
        )
        if result.returncode == 0:
            return
        subprocess.Popen(
            [str(hbin), "gateway", "start"],
            env={**os.environ, **env},
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )
        time.sleep(1)
    except Exception:
        pass


def cmd_swarm(args: argparse.Namespace) -> int:
    """Create a kanban swarm using TAG's native kanban layer (PRD-004).

    Management plane (task creation + monitoring) is pure SQLite — no hermes
    binary and no AI API key needed. Execution (agents running tasks) still
    goes through the hermes gateway, which needs a profile API key. That's
    expected: you need AI credentials to run AI.
    """
    cfg = load_config(config_path(args.config))

    task_type = getattr(args, "task_type", "mixed") or "mixed"
    board = getattr(args, "board", None) or cfg["defaults"].get("board", "default")
    profile = getattr(args, "profile", None) or cfg["defaults"]["master_profile"]
    task_text = args.task

    if profile not in cfg.get("profiles", {}):
        print(f"warning: unknown profile '{profile}' — not found in config", file=sys.stderr)

    try:
        route = resolve_route(cfg, task_type, profile, [])
    except SystemExit:
        route = {}

    workers_cfg = route.get("workers", [])
    verifier_cfg = route.get("verifier") or {}
    verifier_name = (
        verifier_cfg.get("name") if isinstance(verifier_cfg, dict) else str(verifier_cfg)
    ) or profile
    synthesizer_name = profile

    # Validate inputs early — before inserting any DB records
    task_text = task_text.replace("\x00", "").strip()
    if not task_text:
        print("error: task/goal text must not be empty.", file=sys.stderr)
        return 1

    try:
        kanban_path = _kanban.profile_kanban_db_path(cfg, profile, board)
    except ValueError as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 1

    worker_specs: list[tuple[str, str]] = [
        (
            (w.get("name") if isinstance(w, dict) else str(w)),
            task_text[:80],
        )
        for w in workers_cfg
    ]
    if not worker_specs:
        defaults = [p for p in ["researcher", "coder"] if p in cfg.get("profiles", {})]
        worker_specs = [(w, task_text[:80]) for w in (defaults or [profile])]

    db = open_db(cfg)
    run_id = str(uuid.uuid4())[:8]
    insert_run(
        db,
        run_id=run_id,
        kind="swarm",
        task_type=task_type,
        execution="kanban",
        master_profile=profile,
        board=board,
        prompt=task_text,
        route=route,
        status="running",
        metadata={},
    )

    print(f"Swarm run: {run_id}")
    print(f"Profile: {profile}  Board: {board}  Task: {task_text[:60]}")

    try:
        kconn = _kanban.open_db(kanban_path)
    except Exception as exc:
        print(f"kanban db error: {exc}", file=sys.stderr)
        update_run_status(db, run_id=run_id, status="failed")
        db.close()
        return 1

    idem_key = hashlib.sha256(f"{board}:{task_text}".encode()).hexdigest()[:16]
    try:
        topology = _kanban.create_swarm(
            kconn,
            goal=task_text,
            workers=worker_specs,
            verifier_assignee=verifier_name,
            synthesizer_assignee=synthesizer_name,
            idempotency_key=idem_key,
        )
    except Exception as exc:
        print(f"swarm creation failed: {exc}", file=sys.stderr)
        update_run_status(db, run_id=run_id, status="failed")
        kconn.close()
        db.close()
        return 1

    # Best-effort: nudge gateway to start picking up the tasks
    _try_start_gateway(cfg, profile)

    swarm_out = {
        "run_id": run_id,
        "status": "running",
        "swarm": topology,
        "kanban_db": str(kanban_path),
    }

    if getattr(args, "no_wait", False):
        update_run_status(db, run_id=run_id, status="running")
        kconn.close()
        db.close()
        if getattr(args, "json", False):
            print(json.dumps(swarm_out))
        else:
            print(f"Swarm created: root={topology['root_id']}  "
                  f"workers={topology['worker_ids']}")
            print(f"Kanban DB: {kanban_path}")
        return 0

    # Monitor via direct SQLite reads — no hermes binary, no API key
    poll_interval = cfg.get("swarm", {}).get("poll_interval_seconds", 5)
    max_wait = cfg.get("swarm", {}).get("max_wait_seconds", 3600)
    deadline = time.time() + max_wait
    all_task_ids = (
        topology["worker_ids"]
        + [topology["verifier_id"], topology["synthesizer_id"]]
    )

    try:
        while time.time() < deadline:
            if _kanban.tasks_are_terminal(kconn, all_task_ids):
                break
            snap = _kanban.swarm_status_summary(kconn, topology)
            print(f"\r  {snap['done']}/{snap['total']} tasks done", end="", flush=True)
            time.sleep(poll_interval)
    except KeyboardInterrupt:
        print()
        update_run_status(db, run_id=run_id, status="interrupted")
        kconn.close()
        db.close()
        sys.exit(130)

    print()
    final = _kanban.swarm_status_summary(kconn, topology)
    kconn.close()

    status = "completed" if final["complete"] else "timeout"
    update_run_status(db, run_id=run_id, status=status)
    db.close()

    if getattr(args, "json", False):
        print(json.dumps({**swarm_out, "status": status, "final": final}))
    else:
        print(f"Swarm {status}: {run_id}  ({final['done']}/{final['total']} tasks done)")
    return 0 if status == "completed" else 1


# ---------------------------------------------------------------------------
# PRD-007: Desktop Electron app launcher
# ---------------------------------------------------------------------------

def desktop_build_root(cfg: dict[str, Any]) -> Path:
    return runtime_home(cfg) / "desktop"


def desktop_app_path(cfg: dict[str, Any]) -> Path | None:
    """Return the built Electron app binary path, or None if not built."""
    import platform
    build_root = desktop_build_root(cfg)
    system = platform.system()

    if system == "Darwin":
        apps = list((build_root / "build").glob("*.app/Contents/MacOS/*")) if (build_root / "build").exists() else []
        return apps[0] if apps else None
    if system == "Linux":
        build_dir = build_root / "build"
        if build_dir.exists():
            appimages = list(build_dir.glob("*.AppImage"))
            unpacked = list((build_dir / "linux-unpacked").glob("*")) if (build_dir / "linux-unpacked").exists() else []
            candidates = appimages + [p for p in unpacked if p.is_file() and os.access(p, os.X_OK)]
            return candidates[0] if candidates else None
    if system == "Windows":
        build_dir = build_root / "build" / "win-unpacked"
        if build_dir.exists():
            exes = list(build_dir.glob("*.exe"))
            return exes[0] if exes else None
    return None


def build_desktop_app(cfg: dict[str, Any], *, force: bool = False) -> dict[str, Any]:
    """Build Electron desktop app from the Hermes vendor tarball."""
    hermes_checkout = hermes_root(cfg)
    desktop_src = hermes_checkout / "apps" / "desktop"

    if not desktop_src.exists():
        return {"status": "no_source", "message": "apps/desktop not found in hermes checkout"}

    build_root = desktop_build_root(cfg)
    build_root.mkdir(parents=True, exist_ok=True)

    if force or not (build_root / "package.json").exists():
        try:
            shutil.copytree(
                desktop_src,
                build_root,
                dirs_exist_ok=True,
                symlinks=True,
                ignore_dangling_symlinks=True,
            )
        except shutil.Error as _copy_err:
            # Non-fatal: broken symlinks in node_modules/.bin — npm install fixes them
            pass

    npm_bin = shutil.which("npm")
    if not npm_bin:
        return {"status": "error", "message": "npm not found"}

    install = subprocess.run([npm_bin, "install"], cwd=build_root, capture_output=True, text=True)
    if install.returncode != 0:
        return {"status": "install_failed", "stderr": install.stderr[-500:]}

    build = subprocess.run(
        [npm_bin, "run", "build"],
        cwd=build_root,
        env={**os.environ, "ELECTRON_BUILDER_COMPRESSION_LEVEL": "1"},
        capture_output=True,
        text=True,
    )
    if build.returncode != 0:
        return {"status": "build_failed", "stderr": build.stderr[-500:]}

    app = desktop_app_path(cfg)
    return {
        "status": "built" if app else "build_output_missing",
        "app_path": str(app) if app else None,
    }


def cmd_desktop(args: argparse.Namespace) -> int:
    """PRD-007: Build and launch the Electron desktop app."""
    cfg = load_config(config_path(args.config))
    ensure_hermes_ready(cfg, config_arg=args.config, need_tui=False)
    sub = getattr(args, "desktop_subcommand", "open")

    if sub == "build":
        print("Building Electron desktop app (this may take 2–3 minutes)…")
        result = build_desktop_app(cfg, force=getattr(args, "force", False))
        if getattr(args, "json", False):
            print(json.dumps(result, indent=2))
            return 0
        if result["status"] == "built":
            print(f"Built: {result['app_path']}")
            return 0
        print(f"Build failed ({result['status']}): {result.get('message', result.get('stderr', ''))}", file=sys.stderr)
        return 1

    # sub == "open"
    app = desktop_app_path(cfg)
    if not app:
        print("Desktop app not built. Run: tag desktop build", file=sys.stderr)
        return 1

    profile = getattr(args, "profile", None) or cfg["defaults"]["master_profile"]
    env = {**os.environ, **profile_exec_env(cfg, profile), "TAG_DESKTOP_PROFILE": profile}
    subprocess.Popen([str(app)], env=env)
    print(f"Launched desktop (profile: {profile})")
    return 0


def cmd_completion(args: argparse.Namespace) -> int:
    return cmd_hermes_command(args, "completion")


def cmd_prompt_size(args: argparse.Namespace) -> int:
    return cmd_hermes_command(args, "prompt-size")


def cmd_update(args: argparse.Namespace) -> int:
    cfg = load_config(config_path(args.config))
    root = hermes_root(cfg)
    if root.exists() and (root / ".git").exists():
        return cmd_hermes_command(args, "update")
    setup_args = argparse.Namespace(
        config=args.config,
        refresh=True,
        skip_python_install=False,
        skip_tui_build=False,
        json=getattr(args, "json", False),
    )
    return cmd_setup(setup_args)


def cmd_default(args: argparse.Namespace) -> int:
    if not can_launch_interactive_tui():
        print(
            "TAG detected a non-interactive shell, so it will not auto-launch the TUI.\n"
            "Run `tag doctor` to inspect the install, `tag setup` to bootstrap the managed runtime, "
            "or `tag submit ...` / `tag hermes ...` for non-interactive usage.",
            file=sys.stderr,
        )
        return 2
    cfg = load_config(config_path(args.config))
    if not hermes_bin(cfg).exists():
        setup_args = argparse.Namespace(
            config=args.config,
            refresh=False,
            skip_python_install=False,
            skip_tui_build=False,
            json=False,
        )
        cmd_setup(setup_args)
    else:
        bootstrap_profiles(cfg)
        render_profiles(cfg, force=False)
    tui_args = argparse.Namespace(config=args.config, profile="orchestrator", hermes_args=[])
    return cmd_tui(tui_args)


def _doctor_system_checks(cfg: dict[str, Any]) -> list[dict[str, Any]]:
    """PRD-009: System-level health checks."""
    checks: list[dict[str, Any]] = []
    prereqs = doctor_prerequisites(cfg)

    # Python version
    ok = prereqs.get("python_runtime_supported", False)
    checks.append({
        "name": "python_version",
        "status": "pass" if ok else "fail",
        "message": f"Python {sys.version_info[:3]} {'supported' if ok else 'not supported (need 3.11–3.13)'}",
        "fix_cmd": None if ok else "Install Python 3.11–3.13",
    })

    # Node.js
    npm_info = prereqs.get("npm", {})
    npm_ok = npm_info.get("found", False)
    checks.append({
        "name": "npm",
        "status": "pass" if npm_ok else "warn",
        "message": f"npm {npm_info.get('version', 'not found')}",
        "fix_cmd": None if npm_ok else "brew install node  (or equivalent)",
    })

    # git
    git_info = prereqs.get("git", {})
    git_ok = git_info.get("found", False)
    checks.append({
        "name": "git",
        "status": "pass" if git_ok else "warn",
        "message": f"git {git_info.get('version', 'not found')}",
        "fix_cmd": None if git_ok else "brew install git  (or equivalent)",
    })

    # Disk space: warn if < 1 GB free in tag_home parent
    try:
        import shutil as _shutil
        stat = _shutil.disk_usage(tag_home().parent)
        free_gb = stat.free / (1024 ** 3)
        disk_ok = free_gb >= 1.0
        checks.append({
            "name": "disk_space",
            "status": "pass" if disk_ok else "warn",
            "message": f"{free_gb:.1f} GB free in {tag_home().parent}",
            "fix_cmd": None if disk_ok else "Free up disk space",
        })
    except Exception:
        pass

    return checks


def _doctor_hermes_checks(cfg: dict[str, Any]) -> list[dict[str, Any]]:
    """PRD-009: Hermes runtime health checks."""
    checks: list[dict[str, Any]] = []
    prereqs = doctor_prerequisites(cfg)

    bin_exists = hermes_bin(cfg).exists()
    checks.append({
        "name": "hermes_binary",
        "status": "pass" if bin_exists else "fail",
        "message": str(hermes_bin(cfg)) if bin_exists else "not provisioned",
        "fix_cmd": None if bin_exists else "tag setup",
    })

    if bin_exists:
        try:
            v = run_hermes(cfg, "--version").stdout.strip()
            checks.append({"name": "hermes_version", "status": "pass", "message": v})
        except subprocess.CalledProcessError as exc:
            checks.append({
                "name": "hermes_version",
                "status": "fail",
                "message": exc.stderr.strip() or str(exc),
                "fix_cmd": "tag setup --refresh",
            })

    tui_ok = prereqs.get("tui_dist_exists", False)
    checks.append({
        "name": "tui_built",
        "status": "pass" if tui_ok else "warn",
        "message": "TUI built" if tui_ok else "TUI not built",
        "fix_cmd": None if tui_ok else "tag setup",
    })

    patch_st = prereqs.get("patch_status", "unknown")
    _patch_ok = patch_st in ("patched", "applied", "prepatched")
    checks.append({
        "name": "patch_applied",
        "status": "pass" if _patch_ok else "warn",
        "message": patch_st,
        "fix_cmd": None if _patch_ok else "tag setup",
    })

    return checks


def _doctor_profile_checks(cfg: dict[str, Any], profile_name: str) -> list[dict[str, Any]]:
    """PRD-009: Per-profile health checks."""
    checks: list[dict[str, Any]] = []
    ph = profile_home(cfg, profile_name)

    home_ok = ph.exists()
    checks.append({
        "name": "home",
        "status": "pass" if home_ok else "fail",
        "message": str(ph) if home_ok else "missing",
        "fix_cmd": None if home_ok else f"tag bootstrap",
    })
    if not home_ok:
        return checks

    config_file = ph / "config.yaml"
    cfg_ok = config_file.exists()
    checks.append({
        "name": "config.yaml",
        "status": "pass" if cfg_ok else "fail",
        "message": "present" if cfg_ok else "missing",
        "fix_cmd": None if cfg_ok else f"tag render",
    })

    env_file = ph / ".env"
    env_data = read_dotenv(env_file) if env_file.exists() else {}

    profile_cfg = cfg.get("profiles", {}).get(profile_name, {})
    provider = profile_cfg.get("config", {}).get("model", {}).get("provider", "openrouter")
    required_keys: dict[str, str] = {
        "openrouter": "OPENROUTER_API_KEY",
        "anthropic": "ANTHROPIC_API_KEY",
        "openai": "OPENAI_API_KEY",
        "google": "GEMINI_API_KEY",
        "openai-codex": "OPENAI_API_KEY",
    }
    if req_key := required_keys.get(provider):
        key_present = bool(env_data.get(req_key))
        import_cmd = {
            "openrouter": f"tag import-opencode --profile {profile_name}",
            "anthropic": f"tag import-claude --profile {profile_name}",
            "openai": f"tag import-cursor --profile {profile_name}",
            "google": f"tag import-gemini --profile {profile_name}",
            "openai-codex": f"tag import-codex --profile {profile_name}",
        }.get(provider)
        checks.append({
            "name": req_key,
            "status": "pass" if key_present else "warn",
            "message": "present" if key_present else "missing",
            "fix_cmd": None if key_present else import_cmd,
        })

    # Nous Portal gateway check
    if env_data.get("NOUS_PORTAL_API_KEY"):
        checks.append({
            "name": "nous_gateway",
            "status": "pass",
            "message": "API key present",
        })

    # PRD-005: execution backend health check
    exec_cfg = profile_cfg.get("config", {}).get("execution", {})
    backend = exec_cfg.get("backend", "local")
    if backend == "docker":
        import shutil as _shutil
        docker_ok = False
        if _shutil.which("docker"):
            try:
                p = subprocess.run(["docker", "info"], capture_output=True, timeout=5)
                docker_ok = p.returncode == 0
            except Exception:
                docker_ok = False
        checks.append({
            "name": "docker backend",
            "status": "pass" if docker_ok else "warn",
            "message": "daemon running" if docker_ok else "docker daemon not reachable",
            "fix_cmd": None if docker_ok else "sudo systemctl start docker  # or start Docker Desktop",
        })
    elif backend == "ssh":
        ssh_host = env_data.get("SSH_HOST") or exec_cfg.get("ssh", {}).get("host", "")
        if ssh_host:
            ssh_user = env_data.get("SSH_USER") or exec_cfg.get("ssh", {}).get("user", "")
            target = f"{ssh_user}@{ssh_host}" if ssh_user else ssh_host
            try:
                p = subprocess.run(
                    ["ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=3", target, "exit"],
                    capture_output=True,
                    timeout=8,
                )
                ssh_ok = p.returncode == 0
            except Exception:
                ssh_ok = False
            checks.append({
                "name": "ssh backend",
                "status": "pass" if ssh_ok else "warn",
                "message": f"reachable ({target})" if ssh_ok else f"cannot reach {target}",
                "fix_cmd": None if ssh_ok else f"tag import-ssh --profile {profile_name} --host {ssh_host}",
            })
        else:
            checks.append({
                "name": "ssh backend",
                "status": "warn",
                "message": "SSH_HOST not set",
                "fix_cmd": f"tag import-ssh --profile {profile_name} --host <YOUR_HOST>",
            })
    elif backend == "modal":
        modal_ok = bool(env_data.get("MODAL_TOKEN_ID") and env_data.get("MODAL_TOKEN_SECRET"))
        checks.append({
            "name": "modal backend",
            "status": "pass" if modal_ok else "warn",
            "message": "credentials present" if modal_ok else "MODAL_TOKEN_ID/SECRET not set",
            "fix_cmd": None if modal_ok else f"tag import-modal --profile {profile_name} --token-id ID --token-secret SECRET",
        })
    elif backend == "daytona":
        daytona_ok = bool(env_data.get("DAYTONA_WORKSPACE_ID"))
        checks.append({
            "name": "daytona backend",
            "status": "pass" if daytona_ok else "warn",
            "message": "workspace ID set" if daytona_ok else "DAYTONA_WORKSPACE_ID not set",
            "fix_cmd": None if daytona_ok else f"tag import-daytona --profile {profile_name} --workspace-id ID",
        })

    return checks


def cmd_doctor(args: argparse.Namespace) -> int:
    """PRD-009: Comprehensive health check with pass/warn/fail per component."""
    cfg = load_config(config_path(args.config))
    target_profile = getattr(args, "profile", None)

    if getattr(args, "json", False):
        # Legacy JSON mode: include full report + new per-profile checks
        env = hermes_env(cfg)
        report: dict[str, Any] = {
            "app_name": APP_NAME,
            "package_root": str(package_root()),
            "tag_home": str(tag_home()),
            "managed_root": str(managed_root()),
            "hermes_root": str(hermes_root(cfg)),
            "hermes_bin_exists": hermes_bin(cfg).exists(),
            "home": env["HOME"],
            "hermes_home": env["HERMES_HOME"],
            "codex_home": env["CODEX_HOME"],
            "config": str(config_path(args.config)),
            "benchmark_suite": str(benchmark_suite_path(None)),
            "prerequisites": doctor_prerequisites(cfg),
        }
        if hermes_bin(cfg).exists():
            try:
                report["hermes_version"] = run_hermes(cfg, "--version").stdout.strip()
            except subprocess.CalledProcessError as exc:
                report["hermes_version_error"] = exc.stderr.strip()
        else:
            report["hermes_version"] = "not provisioned yet"

        profiles_report: dict[str, Any] = {}
        profiles_to_check = (
            [target_profile] if target_profile
            else list(cfg.get("profiles", {}).keys())
        )
        for p in profiles_to_check:
            profiles_report[p] = _doctor_profile_checks(cfg, p)
        report["profiles"] = profiles_report
        print(json.dumps(report, indent=2))
        has_fail = any(
            c.get("status") == "fail"
            for checks_list in profiles_report.values()
            for c in checks_list
        )
        return 1 if has_fail else 0

    # Rich / plain-text grouped report
    groups: dict[str, list[dict[str, Any]]] = {}
    groups["system"] = _doctor_system_checks(cfg)
    groups["hermes runtime"] = _doctor_hermes_checks(cfg)

    profiles_to_check = (
        [target_profile] if target_profile
        else list(cfg.get("profiles", {}).keys())
    )
    for p in profiles_to_check:
        groups[f"profile: {p}"] = _doctor_profile_checks(cfg, p)

    print_doctor_report(groups)

    all_statuses = [c["status"] for checks in groups.values() for c in checks]
    if any(s == "fail" for s in all_statuses):
        return 1
    return 0


def cmd_bootstrap(args: argparse.Namespace) -> int:
    cfg = load_config(config_path(args.config))
    ensure_hermes_ready(cfg, config_arg=args.config, need_tui=False)
    created = bootstrap_profiles(cfg)
    rendered = render_profiles(cfg, force=args.force)
    result = {"profiles": created, "rendered": rendered}
    if args.json:
        print(json.dumps(result, indent=2))
        return 0
    print("Profiles:")
    for item in created:
        print(f"  {item['profile']}: {item['status']}")
    print("Rendered:")
    for item in rendered:
        print(f"  {item['profile']}: {item['config']}")
    return 0


def cmd_render(args: argparse.Namespace) -> int:
    cfg = load_config(config_path(args.config))
    rendered = render_profiles(cfg, force=args.force)
    if args.json:
        print(json.dumps(rendered, indent=2))
        return 0
    for item in rendered:
        print(f"{item['profile']}: {item['config']}")
    return 0


def cmd_route(args: argparse.Namespace) -> int:
    cfg = load_config(config_path(args.config))
    route = resolve_route(cfg, args.task_type, args.master_profile, args.worker_profile)
    route = apply_route_model_overrides(
        route,
        master_model=args.master_model,
        verifier_model=args.verifier_model,
        worker_models=args.worker_model_override,
    )
    if args.json:
        print(json.dumps(route, indent=2))
        return 0
    print(f"task_type: {args.task_type}")
    print(f"board: {route['board']}")
    print(f"execution: {route['execution']}")
    print(f"master: {route['master']['name']} -> {route['master']['model']}")
    for worker in route["workers"]:
        print(f"worker: {worker['name']} -> {worker['model']}")
    if route["verifier"]:
        print(f"verifier: {route['verifier']['name']} -> {route['verifier']['model']}")
    return 0


def cmd_env(args: argparse.Namespace) -> int:
    cfg = load_config(config_path(args.config))
    env = hermes_env(cfg)
    for key in ("HOME", "HERMES_HOME", "CODEX_HOME", "PATH"):
        print(f"{key}={env[key]}")
    return 0


def cmd_import_codex(args: argparse.Namespace) -> int:
    cfg = load_config(config_path(args.config))
    ensure_hermes_ready(cfg, config_arg=args.config, need_tui=False)
    profiles = cfg.get("profiles", {})
    if args.profile not in profiles:
        available = ", ".join(sorted(profiles))
        raise SystemExit(f"Unknown profile '{args.profile}'. Available: {available}")

    ensure_runtime_dirs(cfg)
    target_home = profile_home(cfg, args.profile)
    if not target_home.exists():
        raise SystemExit(
            f"Profile home does not exist for '{args.profile}'. Run bootstrap first."
        )

    source_home = (
        Path(args.codex_home).expanduser().resolve()
        if args.codex_home
        else Path(
            os.environ.get("TAG_IMPORT_CODEX_HOME", str(runtime_codex_home(cfg)))
        ).expanduser().resolve()
    )
    result = import_codex_into_profile(
        cfg,
        profile_name=args.profile,
        source_codex_home=source_home,
    )
    if result["status"] != "imported":
        raise SystemExit(str(result.get("message", "Codex import failed.")))

    if args.json:
        print(
            json.dumps(
                {
                    "profile": args.profile,
                    "codex_home": str(source_home),
                    "hermes_home": str(target_home),
                    "status": "imported",
                },
                indent=2,
            )
        )
        return 0

    print(f"Imported Codex credentials into profile '{args.profile}'.")
    return 0


def cmd_import_claude(args: argparse.Namespace) -> int:
    cfg = load_config(config_path(args.config))
    ensure_hermes_ready(cfg, config_arg=args.config, need_tui=False)
    profiles = cfg.get("profiles", {})
    if args.profile not in profiles:
        available = ", ".join(sorted(profiles))
        raise SystemExit(f"Unknown profile '{args.profile}'. Available: {available}")
    ensure_runtime_dirs(cfg)
    target_home = profile_home(cfg, args.profile)
    if not target_home.exists():
        raise SystemExit(
            f"Profile home does not exist for '{args.profile}'. Run `tag bootstrap` first."
        )
    source_home = (
        Path(args.claude_home).expanduser().resolve()
        if getattr(args, "claude_home", None)
        else None
    )
    result = import_claude_into_profile(
        cfg,
        profile_name=args.profile,
        source_claude_home=source_home,
        use_oauth=getattr(args, "use_oauth", False),
    )
    if result["status"] == "skipped-no-auth":
        raise SystemExit(
            "No Claude credentials found. Set ANTHROPIC_API_KEY or use "
            "`tag import-claude --use-oauth` to import from claude auth login."
        )
    if result["status"] == "profile-missing":
        raise SystemExit(f"Profile '{args.profile}' home does not exist. Run `tag bootstrap` first.")
    if args.json:
        print(json.dumps(result, indent=2))
        return 0
    mode = result.get("mode", "unknown")
    print(f"Imported Claude credentials into profile '{args.profile}' (mode: {mode}).")
    if "tos_warning" in result:
        print(f"WARNING: {result['tos_warning']}")
    return 0


def cmd_import_gemini(args: argparse.Namespace) -> int:
    cfg = load_config(config_path(args.config))
    ensure_hermes_ready(cfg, config_arg=args.config, need_tui=False)
    profiles = cfg.get("profiles", {})
    if args.profile not in profiles:
        available = ", ".join(sorted(profiles))
        raise SystemExit(f"Unknown profile '{args.profile}'. Available: {available}")
    ensure_runtime_dirs(cfg)
    target_home = profile_home(cfg, args.profile)
    if not target_home.exists():
        raise SystemExit(
            f"Profile home does not exist for '{args.profile}'. Run `tag bootstrap` first."
        )
    source_home = (
        Path(args.gemini_home).expanduser().resolve()
        if getattr(args, "gemini_home", None)
        else None
    )
    result = import_gemini_into_profile(
        cfg,
        profile_name=args.profile,
        source_gemini_home=source_home,
        use_oauth=getattr(args, "use_oauth", False),
    )
    if result["status"] == "skipped-no-auth":
        raise SystemExit(
            "No Gemini credentials found. Set GEMINI_API_KEY (from "
            "https://aistudio.google.com/app/apikey) or use "
            "`tag import-gemini --use-oauth` to import from ~/.gemini/oauth_creds.json."
        )
    if result["status"] == "profile-missing":
        raise SystemExit(f"Profile '{args.profile}' home does not exist. Run `tag bootstrap` first.")
    if args.json:
        print(json.dumps(result, indent=2))
        return 0
    mode = result.get("mode", "unknown")
    print(f"Imported Gemini credentials into profile '{args.profile}' (mode: {mode}).")
    if "tos_warning" in result:
        print(f"WARNING: {result['tos_warning']}")
    return 0


def cmd_import_continue(args: argparse.Namespace) -> int:
    cfg = load_config(config_path(args.config))
    ensure_hermes_ready(cfg, config_arg=args.config, need_tui=False)
    profiles = cfg.get("profiles", {})
    if args.profile not in profiles:
        available = ", ".join(sorted(profiles))
        raise SystemExit(f"Unknown profile '{args.profile}'. Available: {available}")
    ensure_runtime_dirs(cfg)
    target_home = profile_home(cfg, args.profile)
    if not target_home.exists():
        raise SystemExit(
            f"Profile home does not exist for '{args.profile}'. Run `tag bootstrap` first."
        )
    source_home = (
        Path(args.continue_home).expanduser().resolve()
        if getattr(args, "continue_home", None)
        else None
    )
    result = import_continue_into_profile(cfg, profile_name=args.profile, source_continue_home=source_home)
    if result["status"] == "skipped-no-auth":
        raise SystemExit(
            "No Continue.dev config found with API keys. "
            "Expected ~/.continue/config.yaml or ~/.continue/config.json."
        )
    if result["status"] == "profile-missing":
        raise SystemExit(f"Profile '{args.profile}' home does not exist. Run `tag bootstrap` first.")
    if args.json:
        print(json.dumps(result, indent=2))
        return 0
    providers = ", ".join(result.get("providers_imported") or [])
    print(f"Imported Continue.dev credentials into profile '{args.profile}' ({providers}).")
    return 0


def cmd_import_mistral(args: argparse.Namespace) -> int:
    cfg = load_config(config_path(args.config))
    ensure_hermes_ready(cfg, config_arg=args.config, need_tui=False)
    profiles = cfg.get("profiles", {})
    if args.profile not in profiles:
        available = ", ".join(sorted(profiles))
        raise SystemExit(f"Unknown profile '{args.profile}'. Available: {available}")
    ensure_runtime_dirs(cfg)
    target_home = profile_home(cfg, args.profile)
    if not target_home.exists():
        raise SystemExit(
            f"Profile home does not exist for '{args.profile}'. Run `tag bootstrap` first."
        )
    source_home = (
        Path(args.vibe_home).expanduser().resolve()
        if getattr(args, "vibe_home", None)
        else None
    )
    result = import_mistral_into_profile(cfg, profile_name=args.profile, source_vibe_home=source_home)
    if result["status"] == "skipped-no-auth":
        raise SystemExit(
            "No Mistral credentials found. Set MISTRAL_API_KEY or ensure "
            "`mistral-vibe` has written ~/.vibe/.env."
        )
    if result["status"] == "profile-missing":
        raise SystemExit(f"Profile '{args.profile}' home does not exist. Run `tag bootstrap` first.")
    if args.json:
        print(json.dumps(result, indent=2))
        return 0
    print(f"Imported Mistral credentials into profile '{args.profile}'.")
    return 0


def _cmd_import_generic(
    args: argparse.Namespace,
    *,
    import_fn: Any,
    no_auth_msg: str,
    source_path_attr: str | None,
    display_name: str,
    extra_kwargs: dict[str, Any] | None = None,
) -> int:
    cfg = load_config(config_path(args.config))
    ensure_hermes_ready(cfg, config_arg=args.config, need_tui=False)
    profiles = cfg.get("profiles", {})
    if args.profile not in profiles:
        available = ", ".join(sorted(profiles))
        raise SystemExit(f"Unknown profile '{args.profile}'. Available: {available}")
    ensure_runtime_dirs(cfg)
    target_home = profile_home(cfg, args.profile)
    if not target_home.exists():
        raise SystemExit(
            f"Profile home does not exist for '{args.profile}'. Run `tag bootstrap` first."
        )
    kwargs: dict[str, Any] = {"profile_name": args.profile}
    if source_path_attr and getattr(args, source_path_attr, None):
        raw = getattr(args, source_path_attr)
        try:
            kwargs[source_path_attr] = Path(raw).expanduser().resolve()
        except (OSError, RuntimeError) as exc:
            raise SystemExit(f"Cannot resolve path '{raw}': {exc}") from exc
    if extra_kwargs:
        kwargs.update(extra_kwargs)
    result = import_fn(cfg, **kwargs)
    if result["status"] == "skipped-no-auth":
        raise SystemExit(no_auth_msg)
    if result["status"] == "profile-missing":
        raise SystemExit(f"Profile '{args.profile}' home does not exist. Run `tag bootstrap` first.")
    if args.json:
        print(json.dumps(result, indent=2))
        return 0
    providers = result.get("providers_imported")
    if providers:
        print(f"Imported {display_name} credentials into profile '{args.profile}' ({', '.join(providers)}).")
    else:
        mode = result.get("mode", "")
        print(f"Imported {display_name} credentials into profile '{args.profile}' (mode: {mode}).")
    if "tos_warning" in result:
        print(f"WARNING: {result['tos_warning']}")
    return 0


def cmd_import_opencode(args: argparse.Namespace) -> int:
    return _cmd_import_generic(
        args,
        import_fn=import_opencode_into_profile,
        no_auth_msg="No opencode credentials found. Expected ~/.local/share/opencode/auth.json.",
        source_path_attr="opencode_data_dir",
        display_name="opencode",
    )


def cmd_import_zed(args: argparse.Namespace) -> int:
    return _cmd_import_generic(
        args,
        import_fn=import_zed_into_profile,
        no_auth_msg=(
            "No API keys found in Zed settings. Zed stores keys in the OS keychain by default; "
            "set keys via Zed's Agent Settings panel and ensure they are also exported as standard "
            "env vars (OPENAI_API_KEY, ANTHROPIC_API_KEY, etc.)."
        ),
        source_path_attr="zed_config",
        display_name="Zed",
    )


def cmd_import_copilot(args: argparse.Namespace) -> int:
    return _cmd_import_generic(
        args,
        import_fn=import_copilot_into_profile,
        no_auth_msg=(
            "No GitHub token found. Run `gh auth login` to authenticate the gh CLI, "
            "or set GITHUB_TOKEN in your environment."
        ),
        source_path_attr="gh_config",
        display_name="GitHub Copilot",
    )


def cmd_import_aider(args: argparse.Namespace) -> int:
    return _cmd_import_generic(
        args,
        import_fn=import_aider_into_profile,
        no_auth_msg=(
            "No Aider credentials found. Expected ~/.aider.conf.yml, ~/.env, or ~/.aider.env "
            "with at least one of: OPENAI_API_KEY, ANTHROPIC_API_KEY, GEMINI_API_KEY, etc."
        ),
        source_path_attr="aider_home",
        display_name="Aider",
    )


def cmd_import_aws(args: argparse.Namespace) -> int:
    return _cmd_import_generic(
        args,
        import_fn=import_aws_into_profile,
        no_auth_msg=(
            "No AWS credentials found. Run `aws configure` or set AWS_ACCESS_KEY_ID and "
            "AWS_SECRET_ACCESS_KEY in your environment."
        ),
        source_path_attr="aws_dir",
        display_name="AWS Bedrock",
    )


def cmd_import_cursor(args: argparse.Namespace) -> int:
    return _cmd_import_generic(
        args,
        import_fn=import_cursor_into_profile,
        no_auth_msg=(
            "No API keys found in Cursor's local storage. Add API keys via Cursor Settings → "
            "Models (BYOK) and ensure Cursor has been run at least once."
        ),
        source_path_attr="cursor_dir",
        display_name="Cursor",
    )


# ---------------------------------------------------------------------------
# PRD-001: Supermemory and Honcho credential import
# ---------------------------------------------------------------------------

def _detect_supermemory_credentials(
    source_config_dir: Path | None = None,
) -> dict[str, str]:
    """Read Supermemory API key from known config locations."""
    candidates: list[Path] = []
    if source_config_dir:
        candidates.append(source_config_dir / "config.json")
    candidates += [
        Path.home() / ".config" / "supermemory" / "config.json",
        Path.home() / ".supermemory" / "config.json",
    ]
    for path in candidates:
        if path.exists():
            try:
                data = json.loads(path.read_text())
                key = data.get("api_key") or data.get("token")
                if key:
                    return {"SUPERMEMORY_API_KEY": str(key)}
            except (json.JSONDecodeError, OSError):
                pass
    if key := os.environ.get("SUPERMEMORY_API_KEY", ""):
        return {"SUPERMEMORY_API_KEY": key}
    return {}


def import_supermemory_into_profile(
    cfg: dict[str, Any],
    profile_name: str,
    *,
    api_key: str | None = None,
    source_config_dir: Path | None = None,
    force: bool = False,
) -> dict[str, Any]:
    ph = profile_home(cfg, profile_name)
    if not ph.exists():
        return {"status": "profile-missing"}
    creds = {"SUPERMEMORY_API_KEY": api_key} if api_key else _detect_supermemory_credentials(source_config_dir)
    if not creds:
        return {"status": "skipped-no-auth"}
    env_file = ph / ".env"
    for key, value in creds.items():
        _upsert_env_line(env_file, key, value)
    _upsert_env_line(env_file, "SUPERMEMORY_SESSION_INGEST", "1")
    return {"status": "ok", "profile": profile_name, "providers_imported": ["supermemory"]}


def _detect_honcho_credentials(
    source_config: Path | None = None,
) -> dict[str, str]:
    """Read Honcho credentials from known config locations."""
    candidates: list[Path] = []
    if source_config:
        candidates.append(source_config)
    candidates += [
        Path.home() / ".honcho" / ".env",
        Path.home() / ".config" / "honcho" / "config.yaml",
    ]
    result: dict[str, str] = {}
    for path in candidates:
        if not path.exists():
            continue
        try:
            if path.suffix in (".yaml", ".yml"):
                data = yaml.safe_load(path.read_text()) or {}
                if k := data.get("api_key") or data.get("HONCHO_API_KEY"):
                    result["HONCHO_API_KEY"] = str(k)
                if u := data.get("base_url") or data.get("HONCHO_BASE_URL"):
                    result["HONCHO_BASE_URL"] = str(u)
            else:
                for line in path.read_text().splitlines():
                    line = line.strip()
                    if "=" in line and not line.startswith("#"):
                        k, _, v = line.partition("=")
                        if k in ("HONCHO_API_KEY", "HONCHO_BASE_URL"):
                            result[k] = v.strip()
        except (OSError, yaml.YAMLError):
            pass
    for key in ("HONCHO_API_KEY", "HONCHO_BASE_URL"):
        if key not in result and (val := os.environ.get(key, "")):
            result[key] = val
    return result


def import_honcho_into_profile(
    cfg: dict[str, Any],
    profile_name: str,
    *,
    source_config: Path | None = None,
    base_url: str | None = None,
    force: bool = False,
) -> dict[str, Any]:
    ph = profile_home(cfg, profile_name)
    if not ph.exists():
        return {"status": "profile-missing"}
    creds = _detect_honcho_credentials(source_config)
    if base_url:
        creds["HONCHO_BASE_URL"] = base_url
    if not creds:
        return {"status": "skipped-no-auth"}
    env_file = ph / ".env"
    for key, value in creds.items():
        _upsert_env_line(env_file, key, value)
    return {"status": "ok", "profile": profile_name, "providers_imported": list(creds.keys())}


def cmd_import_supermemory(args: argparse.Namespace) -> int:
    return _cmd_import_generic(
        args,
        import_fn=import_supermemory_into_profile,
        no_auth_msg=(
            "No Supermemory API key found. Pass --api-key or set SUPERMEMORY_API_KEY.\n"
            "Get a key at https://supermemory.ai/"
        ),
        source_path_attr="source_config_dir",
        display_name="Supermemory",
        extra_kwargs={"api_key": getattr(args, "api_key", None) or None},
    )


def cmd_import_honcho(args: argparse.Namespace) -> int:
    return _cmd_import_generic(
        args,
        import_fn=import_honcho_into_profile,
        no_auth_msg=(
            "No Honcho credentials found. Pass --base-url and set HONCHO_API_KEY.\n"
            "See https://honcho.dev/ for self-hosted setup."
        ),
        source_path_attr="source_config",
        display_name="Honcho",
        extra_kwargs={"base_url": getattr(args, "base_url", None) or None},
    )


# ---------------------------------------------------------------------------
# PRD-006: Nous Portal Tool Gateway
# ---------------------------------------------------------------------------

def _detect_nous_portal_credentials(
    source_config: Path | None = None,
) -> dict[str, str]:
    """Read Nous Portal API key from known config locations."""
    candidates: list[Path] = []
    if source_config:
        candidates.append(source_config)
    candidates += [
        Path.home() / ".config" / "nousresearch" / "portal.json",
        Path.home() / ".nousresearch" / "config.json",
        Path.home() / ".nousresearch" / "portal.json",
    ]
    for path in candidates:
        if path.exists():
            try:
                data = json.loads(path.read_text())
                key = data.get("api_key") or data.get("token") or data.get("key")
                if key:
                    return {"NOUS_PORTAL_API_KEY": str(key)}
            except (json.JSONDecodeError, OSError):
                pass
    if key := os.environ.get("NOUS_PORTAL_API_KEY", ""):
        return {"NOUS_PORTAL_API_KEY": key}
    return {}


def import_nous_portal_into_profile(
    cfg: dict[str, Any],
    profile_name: str,
    *,
    api_key: str | None = None,
    source_config: Path | None = None,
    force: bool = False,
) -> dict[str, Any]:
    """Write NOUS_PORTAL_API_KEY to profile .env and enable gateway in config."""
    ph = profile_home(cfg, profile_name)
    if not ph.exists():
        return {"status": "profile-missing", "profile": profile_name}

    creds = {"NOUS_PORTAL_API_KEY": api_key} if api_key else _detect_nous_portal_credentials(source_config)
    if not creds:
        return {"status": "skipped-no-auth", "profile": profile_name}

    env_file = ph / ".env"
    for key, value in creds.items():
        _upsert_env_line(env_file, key, value)

    # Enable use_gateway in profile's Hermes config.yaml
    profile_config_file = ph / "config.yaml"
    if profile_config_file.exists():
        try:
            pcfg = yaml.safe_load(profile_config_file.read_text()) or {}
            pcfg.setdefault("gateway", {})["use_gateway"] = True
            write_yaml(profile_config_file, pcfg, force=True)
        except Exception:
            pass

    return {
        "status": "ok",
        "profile": profile_name,
        "providers_imported": ["nous_portal"],
        "env_file": str(env_file),
    }


def cmd_import_nous_portal(args: argparse.Namespace) -> int:
    cfg = load_config(config_path(args.config))
    ensure_hermes_ready(cfg, config_arg=args.config, need_tui=False)
    profiles_cfg = cfg.get("profiles", {})

    if getattr(args, "all_profiles", False):
        profiles_to_update = list(profiles_cfg.keys())
    else:
        p = getattr(args, "profile", None) or cfg["defaults"]["master_profile"]
        if p not in profiles_cfg:
            available = ", ".join(sorted(profiles_cfg))
            raise SystemExit(f"Unknown profile '{p}'. Available: {available}")
        profiles_to_update = [p]

    api_key_arg = getattr(args, "api_key", None) or None
    if api_key_arg is not None and len(api_key_arg) < 20:
        raise SystemExit(
            f"API key too short ({len(api_key_arg)} chars); Nous Portal keys are at least 20 characters"
        )

    results = []
    for p in profiles_to_update:
        ensure_runtime_dirs(cfg)
        result = import_nous_portal_into_profile(
            cfg,
            p,
            api_key=api_key_arg,
            force=getattr(args, "force", False),
        )
        results.append(result)

    if getattr(args, "json", False):
        print(json.dumps(results, indent=2))
        return 0

    any_ok = False
    for r in results:
        profile_name = r.get("profile", "?")
        if r["status"] == "ok":
            print(f"  ✓ {profile_name}: Nous Portal gateway enabled")
            any_ok = True
        elif r["status"] == "skipped-no-auth":
            print(f"  – {profile_name}: no credentials found")
        else:
            print(f"  ✗ {profile_name}: {r['status']}")

    if not any_ok:
        print(
            "Hint: pass --api-key YOUR_KEY or set NOUS_PORTAL_API_KEY env var.\n"
            "Note: Requires an active Nous Portal subscription (https://portal.nousresearch.com/)."
        )
        return 1
    return 0


# ---------------------------------------------------------------------------
# PRD-005: Execution backend credential import helpers
# ---------------------------------------------------------------------------

_VALID_BACKENDS = ("local", "docker", "ssh", "modal", "daytona", "singularity")
_DOCKER_IMAGE_RE = re.compile(r'^[a-zA-Z0-9][a-zA-Z0-9_./:@-]*$')


def import_docker_into_profile(
    cfg: dict[str, Any],
    profile_name: str,
    *,
    image: str | None = None,
    force: bool = False,
) -> dict[str, Any]:
    """Write Docker backend settings to the profile's .env file."""
    if image and not _DOCKER_IMAGE_RE.match(image):
        raise SystemExit(f"Invalid Docker image name: {image!r}")
    env_file = profile_home(cfg, profile_name) / ".env"
    env_file.parent.mkdir(parents=True, exist_ok=True)

    result: dict[str, Any] = {"profile": profile_name, "status": "ok", "keys_written": []}
    default_image = image or "ubuntu:22.04"
    _upsert_env_line(env_file, "DOCKER_DEFAULT_IMAGE", default_image)
    result["keys_written"].append("DOCKER_DEFAULT_IMAGE")

    # Verify Docker is actually available (advisory only — we don't block on this)
    import shutil as _shutil
    if _shutil.which("docker"):
        try:
            proc = subprocess.run(
                ["docker", "info"],
                capture_output=True,
                timeout=5,
            )
            result["docker_available"] = proc.returncode == 0
        except Exception:
            result["docker_available"] = False
    else:
        result["docker_available"] = False
        result["warning"] = "docker binary not found — install Docker before using this backend"

    return result


_SSH_HOST_RE = re.compile(r'^[a-zA-Z0-9.\-_\[\]:]+$')


def import_ssh_into_profile(
    cfg: dict[str, Any],
    profile_name: str,
    *,
    host: str,
    user: str | None = None,
    key_file: str | None = None,
    port: int = 22,
    force: bool = False,
) -> dict[str, Any]:
    """Write SSH backend credentials to the profile's .env file."""
    if not host or not host.strip():
        raise SystemExit("--host is required for SSH backend import")
    if not _SSH_HOST_RE.match(host.strip()):
        raise SystemExit(
            f"Invalid SSH host '{host}': must contain only alphanumerics, dots, hyphens, "
            "underscores, brackets, and colons (no shell metacharacters)"
        )
    if not (1 <= port <= 65535):
        raise SystemExit(f"Invalid SSH port {port}: must be 1–65535")
    env_file = profile_home(cfg, profile_name) / ".env"
    env_file.parent.mkdir(parents=True, exist_ok=True)

    result: dict[str, Any] = {"profile": profile_name, "status": "ok", "keys_written": []}
    _upsert_env_line(env_file, "SSH_HOST", host)
    result["keys_written"].append("SSH_HOST")
    if user:
        _upsert_env_line(env_file, "SSH_USER", user)
        result["keys_written"].append("SSH_USER")
    if key_file:
        _upsert_env_line(env_file, "SSH_KEY_FILE", str(Path(key_file).expanduser()))
        result["keys_written"].append("SSH_KEY_FILE")
        if not Path(key_file).expanduser().exists():
            result["warning"] = f"Key file not found: {key_file}"
    if port != 22:
        _upsert_env_line(env_file, "SSH_PORT", str(port))
        result["keys_written"].append("SSH_PORT")
    return result


def import_modal_into_profile(
    cfg: dict[str, Any],
    profile_name: str,
    *,
    token_id: str,
    token_secret: str,
    force: bool = False,
) -> dict[str, Any]:
    """Write Modal backend credentials to the profile's .env file."""
    if not token_id or not token_id.strip() or not token_secret or not token_secret.strip():
        raise SystemExit("--token-id and --token-secret must not be empty or whitespace-only")
    env_file = profile_home(cfg, profile_name) / ".env"
    env_file.parent.mkdir(parents=True, exist_ok=True)

    _upsert_env_line(env_file, "MODAL_TOKEN_ID", token_id)
    _upsert_env_line(env_file, "MODAL_TOKEN_SECRET", token_secret)
    return {"profile": profile_name, "status": "ok", "keys_written": ["MODAL_TOKEN_ID", "MODAL_TOKEN_SECRET"]}


def import_daytona_into_profile(
    cfg: dict[str, Any],
    profile_name: str,
    *,
    workspace_id: str,
    api_key: str | None = None,
    force: bool = False,
) -> dict[str, Any]:
    """Write Daytona workspace ID to the profile's .env file."""
    if not workspace_id or not workspace_id.strip():
        raise SystemExit("--workspace-id must not be empty or whitespace-only")
    env_file = profile_home(cfg, profile_name) / ".env"
    env_file.parent.mkdir(parents=True, exist_ok=True)

    keys: list[str] = []
    _upsert_env_line(env_file, "DAYTONA_WORKSPACE_ID", workspace_id)
    keys.append("DAYTONA_WORKSPACE_ID")
    if api_key:
        _upsert_env_line(env_file, "DAYTONA_API_KEY", api_key)
        keys.append("DAYTONA_API_KEY")
    return {"profile": profile_name, "status": "ok", "keys_written": keys}


def cmd_import_docker(args: argparse.Namespace) -> int:
    cfg = load_config(config_path(args.config))
    ensure_hermes_ready(cfg, config_arg=args.config, need_tui=False)
    profile_name = getattr(args, "profile", None) or cfg["defaults"]["master_profile"]
    if profile_name not in cfg.get("profiles", {}):
        available = ", ".join(sorted(cfg.get("profiles", {})))
        raise SystemExit(f"Unknown profile '{profile_name}'. Available: {available}")
    ensure_runtime_dirs(cfg)
    result = import_docker_into_profile(
        cfg,
        profile_name,
        image=getattr(args, "image", None) or None,
        force=getattr(args, "force", False),
    )
    if getattr(args, "json", False):
        print(json.dumps(result, indent=2))
        return 0
    if result["status"] == "ok":
        print(f"✓ {profile_name}: Docker backend configured")
        if not result.get("docker_available"):
            print(f"  ⚠ Warning: {result.get('warning', 'Docker daemon not running')}")
    else:
        print(f"✗ {profile_name}: {result['status']}")
    return 0


def cmd_import_ssh(args: argparse.Namespace) -> int:
    cfg = load_config(config_path(args.config))
    ensure_hermes_ready(cfg, config_arg=args.config, need_tui=False)
    profile_name = getattr(args, "profile", None) or cfg["defaults"]["master_profile"]
    if profile_name not in cfg.get("profiles", {}):
        available = ", ".join(sorted(cfg.get("profiles", {})))
        raise SystemExit(f"Unknown profile '{profile_name}'. Available: {available}")
    ensure_runtime_dirs(cfg)
    port_arg = getattr(args, "port", None)
    result = import_ssh_into_profile(
        cfg,
        profile_name,
        host=args.host,
        user=getattr(args, "user", None) or None,
        key_file=getattr(args, "key_file", None) or None,
        port=port_arg if port_arg is not None else 22,
        force=getattr(args, "force", False),
    )
    if getattr(args, "json", False):
        print(json.dumps(result, indent=2))
        return 0
    if result["status"] == "ok":
        print(f"✓ {profile_name}: SSH backend configured (host: {args.host})")
    else:
        print(f"✗ {profile_name}: {result['status']}")
    return 0


def cmd_import_modal(args: argparse.Namespace) -> int:
    cfg = load_config(config_path(args.config))
    ensure_hermes_ready(cfg, config_arg=args.config, need_tui=False)
    profile_name = getattr(args, "profile", None) or cfg["defaults"]["master_profile"]
    if profile_name not in cfg.get("profiles", {}):
        available = ", ".join(sorted(cfg.get("profiles", {})))
        raise SystemExit(f"Unknown profile '{profile_name}'. Available: {available}")
    ensure_runtime_dirs(cfg)
    result = import_modal_into_profile(
        cfg,
        profile_name,
        token_id=args.token_id,
        token_secret=args.token_secret,
        force=getattr(args, "force", False),
    )
    if getattr(args, "json", False):
        print(json.dumps(result, indent=2))
        return 0
    print(f"✓ {profile_name}: Modal backend credentials written")
    return 0


def cmd_import_daytona(args: argparse.Namespace) -> int:
    cfg = load_config(config_path(args.config))
    ensure_hermes_ready(cfg, config_arg=args.config, need_tui=False)
    profile_name = getattr(args, "profile", None) or cfg["defaults"]["master_profile"]
    if profile_name not in cfg.get("profiles", {}):
        available = ", ".join(sorted(cfg.get("profiles", {})))
        raise SystemExit(f"Unknown profile '{profile_name}'. Available: {available}")
    ensure_runtime_dirs(cfg)
    result = import_daytona_into_profile(
        cfg,
        profile_name,
        workspace_id=args.workspace_id,
        api_key=getattr(args, "api_key", None) or None,
        force=getattr(args, "force", False),
    )
    if getattr(args, "json", False):
        print(json.dumps(result, indent=2))
        return 0
    print(f"✓ {profile_name}: Daytona backend configured (workspace: {args.workspace_id})")
    return 0


def cmd_assignments(args: argparse.Namespace) -> int:
    cfg = load_config(config_path(args.config))
    rows = collect_assignments(cfg)
    if args.json:
        print(json.dumps(rows, indent=2))
        return 0
    for row in rows:
        runtime = f" [{row['openai_runtime']}]" if row["openai_runtime"] else ""
        print(f"{row['profile']}: {row['primary_model']}{runtime}")
        if row["delegation_model"] != "-":
            print(f"  delegation: {row['delegation_model']}")
    return 0


def cmd_models(args: argparse.Namespace) -> int:
    cfg = load_config(config_path(args.config))
    ensure_profile_exists(cfg, args.profile)
    ensure_hermes_ready(cfg, config_arg=args.config, need_tui=False)
    payload = load_model_inventory(cfg, args.profile)
    providers = payload.get("providers", [])
    if args.provider:
        providers = [item for item in providers if item.get("slug") == args.provider]
    result = {
        "profile": args.profile,
        "current_provider": payload.get("provider", ""),
        "current_model": payload.get("model", ""),
        "providers": providers,
    }
    if args.json:
        print(json.dumps(result, indent=2))
        return 0
    print(f"profile: {args.profile}")
    current = (
        f"{result['current_provider']}/{result['current_model']}"
        if result["current_provider"] and result["current_model"]
        else "-"
    )
    print(f"current: {current}")
    for provider in providers:
        header = provider.get("slug", "")
        if provider.get("authenticated") is False:
            header = f"{header} (not configured)"
        print(header)
        for model in provider.get("models", [])[: args.limit]:
            print(f"  - {model}")
    return 0


def cmd_set_model(args: argparse.Namespace) -> int:
    path = config_path(args.config)
    cfg = load_config(path)
    ensure_profile_exists(cfg, args.profile)
    provider, model = parse_model_ref(args.ref)
    profile_cfg = cfg.setdefault("profiles", {}).setdefault(args.profile, {}).setdefault("config", {})

    if args.target == "primary":
        model_cfg = profile_cfg.setdefault("model", {})
        model_cfg["provider"] = provider
        model_cfg["default"] = model
        if args.openai_runtime:
            model_cfg["openai_runtime"] = args.openai_runtime
    else:
        delegation_cfg = profile_cfg.setdefault("delegation", {})
        delegation_cfg["provider"] = provider
        delegation_cfg["model"] = model

    save_config(path, cfg)
    render_profiles(cfg, force=True)

    result = {
        "profile": args.profile,
        "target": args.target,
        "ref": f"{provider}/{model}",
        "config": str(path),
    }
    if args.json:
        print(json.dumps(result, indent=2))
        return 0
    print(f"{args.profile} {args.target} model -> {provider}/{model}")
    return 0


def cmd_submit(args: argparse.Namespace) -> int:
    cfg_path = config_path(args.config)
    cfg = load_config(cfg_path)
    ensure_hermes_ready(cfg, config_arg=args.config, need_tui=False)
    prompt = args.prompt.strip()
    if not prompt:
        raise SystemExit("Prompt cannot be empty.")

    route = resolve_route(cfg, args.task_type, args.master_profile, args.worker_profile)
    route = apply_route_model_overrides(
        route,
        master_model=args.master_model,
        verifier_model=args.verifier_model,
        worker_models=args.worker_model_override,
    )
    execution = (
        args.execution
        if args.execution != "auto"
        else str(route.get("execution", "kanban"))
    )
    run_id = f"run-{slugify(args.task_type)}-{uuid.uuid4().hex[:10]}"
    conn = open_db(cfg)
    metadata = {
        "title": args.title or "",
        "source": args.source,
        "config": str(cfg_path),
    }
    insert_run(
        conn,
        run_id=run_id,
        kind="submit",
        task_type=args.task_type,
        execution=execution,
        master_profile=route["master"]["name"],
        board=route["board"],
        prompt=prompt,
        route=route,
        status="running",
        metadata=metadata,
    )

    result: dict[str, Any] = {
        "run_id": run_id,
        "execution": execution,
        "route": route,
        "steps": [],
    }

    if execution == "direct":
        futures = {}
        with ThreadPoolExecutor(max_workers=max(1, len(route["workers"]))) as pool:
            for worker in route["workers"]:
                worker_prompt = prompt
                futures[
                    pool.submit(
                        run_chat_step,
                        cfg,
                        profile_name=worker["name"],
                        prompt=worker_prompt,
                    )
                ] = worker
            for future in as_completed(futures):
                worker = futures[future]
                step = future.result()
                step["role"] = "worker"
                step["profile"] = worker["name"]
                step["model_ref"] = format_model_ref(worker["model"])
                result["steps"].append(step)
                insert_step(
                    conn,
                    run_id=run_id,
                    role="worker",
                    profile=worker["name"],
                    model_ref=step["model_ref"],
                    prompt=step["prompt"],
                    output=step["output"],
                    status=step["status"],
                    started_at=step["started_at"],
                    finished_at=step["finished_at"],
                    duration_ms=step["duration_ms"],
                    extra={"returncode": step["returncode"]},
                )

        if args.verify and route.get("verifier"):
            verifier_prompt = textwrap.dedent(
                f"""
                Task:
                {prompt}

                Worker outputs:
                {json.dumps([{k: v for k, v in step.items() if k in ('profile', 'status', 'output')} for step in result['steps']], indent=2)}

                Return compact JSON with keys status, verdict, notes.
                """
            ).strip()
            verify_step = run_chat_step(
                cfg,
                profile_name=route["verifier"]["name"],
                prompt=verifier_prompt,
            )
            verify_step["role"] = "verifier"
            verify_step["profile"] = route["verifier"]["name"]
            verify_step["model_ref"] = format_model_ref(route["verifier"]["model"])
            result["verifier"] = verify_step
            insert_step(
                conn,
                run_id=run_id,
                role="verifier",
                profile=verify_step["profile"],
                model_ref=verify_step["model_ref"],
                prompt=verify_step["prompt"],
                output=verify_step["output"],
                status=verify_step["status"],
                started_at=verify_step["started_at"],
                finished_at=verify_step["finished_at"],
                duration_ms=verify_step["duration_ms"],
                extra={"returncode": verify_step["returncode"]},
            )

        failures = [step for step in result["steps"] if step["status"] != "ok"]
        final_status = "ok" if not failures else "error"
        result["status"] = final_status
        update_run_status(conn, run_id=run_id, status=final_status, metadata=metadata)
    elif execution == "kanban":
        board = route["board"]
        title = args.title or f"{args.task_type}: {prompt[:80]}"
        create_cmd = [
            "kanban",
            "--board",
            board,
            "create",
            title,
            "--assignee",
        ]
        for worker in route["workers"]:
            worker_prompt = prompt
            proc = run_profile_hermes(
                cfg,
                route["master"]["name"],
                *create_cmd,
                worker["name"],
                "--body",
                worker_prompt,
                "--json",
                check=False,
            )
            output = (proc.stdout.strip() or proc.stderr.strip()).strip()
            step = {
                "role": "worker",
                "profile": worker["name"],
                "model_ref": format_model_ref(worker["model"]),
                "prompt": worker_prompt,
                "output": output,
                "status": "ok" if proc.returncode == 0 else "error",
                "task_id": "",
            }
            try:
                task_payload = json.loads(output) if output else {}
                step["task_id"] = str(task_payload.get("id", "") or "")
            except Exception:
                step["task_id"] = ""
            result["steps"].append(step)
            now = utc_now()
            insert_step(
                conn,
                run_id=run_id,
                role="worker",
                profile=worker["name"],
                model_ref=step["model_ref"],
                prompt=worker_prompt,
                output=output,
                status=step["status"],
                started_at=now,
                finished_at=now,
                duration_ms=0,
                extra={"kanban": True, "task_id": step["task_id"]},
            )
        final_status = "queued"
        if args.wait_seconds > 0:
            deadline = time.time() + args.wait_seconds
            pending = {step["task_id"]: step for step in result["steps"] if step.get("task_id")}
            while pending and time.time() < deadline:
                for task_id, step in list(pending.items()):
                    snapshot = show_kanban_task(
                        cfg,
                        profile_name=route["master"]["name"],
                        board=board,
                        task_id=task_id,
                    )
                    task = snapshot.get("task", {})
                    task_status = str(task.get("status", "") or "")
                    if task_status in {"done", "blocked", "archived"}:
                        step["task_status"] = task_status
                        step["latest_summary"] = snapshot.get("latest_summary")
                        pending.pop(task_id, None)
                if pending:
                    time.sleep(3)
            if pending:
                final_status = "queued"
            else:
                final_status = "ok"
        result["status"] = final_status
        update_run_status(conn, run_id=run_id, status=final_status, metadata=metadata)
    else:
        raise SystemExit(f"Unsupported execution mode '{execution}'.")

    if args.json:
        print(json.dumps(result, indent=2))
        return 0
    print(f"run_id: {run_id}")
    print(f"status: {result['status']}")
    for step in result["steps"]:
        print(f"{step['profile']}: {step['status']}")
    return 0


def cmd_benchmark(args: argparse.Namespace) -> int:
    cfg = load_config(config_path(args.config))
    ensure_hermes_ready(cfg, config_arg=args.config, need_tui=False)
    suite_path = benchmark_suite_path(args.suite)
    try:
        suite = load_benchmark_suite(suite_path)
    except FileNotFoundError as exc:
        raise SystemExit(f"Benchmark suite not found: {suite_path}") from exc
    if args.case:
        selected = set(args.case)
        suite = [case for case in suite if case.get("id") in selected]
    if not suite:
        raise SystemExit("No benchmark cases selected.")

    model_refs = args.model_ref or [
        collect_assignments(cfg)
        and next(
            row["primary_model"]
            for row in collect_assignments(cfg)
            if row["profile"] == args.profile
        )
    ]
    run_id = f"bench-{slugify(args.profile)}-{uuid.uuid4().hex[:10]}"
    conn = open_db(cfg)
    insert_run(
        conn,
        run_id=run_id,
        kind="benchmark",
        task_type="benchmark",
        execution="direct",
        master_profile=args.profile,
        board="-",
        prompt=f"benchmark suite: {benchmark_suite_path(args.suite)}",
        route={"profile": args.profile, "models": model_refs},
        status="running",
        metadata={"suite": str(benchmark_suite_path(args.suite))},
    )
    result = {"run_id": run_id, "profile": args.profile, "models": []}
    overall_ok = True

    for model_ref in model_refs:
        temp_profile = create_temp_profile(cfg, base_profile=args.profile, model_ref=model_ref)
        model_entry = {"model_ref": model_ref, "profile": temp_profile, "cases": []}
        for case in suite:
            step = run_chat_step(cfg, profile_name=temp_profile, prompt=str(case.get("prompt", "")))
            ok, reason = case_passed(case, step["output"])
            case_result = {
                "id": case.get("id"),
                "status": "ok" if ok and step["status"] == "ok" else "error",
                "reason": reason,
                "output": step["output"],
            }
            model_entry["cases"].append(case_result)
            overall_ok = overall_ok and case_result["status"] == "ok"
            insert_step(
                conn,
                run_id=run_id,
                role="benchmark",
                profile=temp_profile,
                model_ref=model_ref,
                prompt=step["prompt"],
                output=step["output"],
                status=case_result["status"],
                started_at=step["started_at"],
                finished_at=step["finished_at"],
                duration_ms=step["duration_ms"],
                extra={"case_id": case.get("id"), "reason": reason},
            )
        result["models"].append(model_entry)

    result["status"] = "ok" if overall_ok else "error"
    update_run_status(conn, run_id=run_id, status=result["status"], metadata={"suite": str(benchmark_suite_path(args.suite))})
    if args.json:
        print(json.dumps(result, indent=2))
        return 0
    print(f"run_id: {run_id}")
    print(f"status: {result['status']}")
    for model in result["models"]:
        failed = sum(1 for case in model["cases"] if case["status"] != "ok")
        print(f"{model['model_ref']}: {len(model['cases']) - failed}/{len(model['cases'])} passed")
    return 0


def cmd_runs(args: argparse.Namespace) -> int:
    cfg = load_config(config_path(args.config))
    conn = open_db(cfg)
    rows = conn.execute(
        "SELECT id, created_at, kind, task_type, execution, master_profile, status FROM runs ORDER BY created_at DESC LIMIT ?",
        (args.limit,),
    ).fetchall()
    payload = [dict(row) for row in rows]
    if args.json:
        print(json.dumps(payload, indent=2))
        return 0
    for row in payload:
        print(
            f"{row['id']} | {row['kind']} | {row['task_type']} | {row['execution']} | {row['master_profile']} | {row['status']}"
        )
    return 0


def cmd_openrouter_models(args: argparse.Namespace) -> int:
    cfg = load_config(config_path(args.config))
    ensure_profile_exists(cfg, args.profile)
    ensure_hermes_ready(cfg, config_arg=args.config, need_tui=False)
    rows = load_openrouter_catalog(cfg, args.profile)

    if args.search:
        needle = args.search.lower()
        rows = [
            row for row in rows
            if needle in str(row.get("id", "")).lower()
            or needle in str(row.get("name", "")).lower()
            or needle in str(row.get("description", "")).lower()
        ]

    def prompt_cost(row: dict[str, Any]) -> float:
        try:
            return float(row.get("pricing", {}).get("prompt", "0") or 0)
        except Exception:
            return 0.0

    def completion_cost(row: dict[str, Any]) -> float:
        try:
            return float(row.get("pricing", {}).get("completion", "0") or 0)
        except Exception:
            return 0.0

    if args.sort == "prompt":
        rows = sorted(rows, key=prompt_cost)
    elif args.sort == "completion":
        rows = sorted(rows, key=completion_cost)
    elif args.sort == "context":
        rows = sorted(rows, key=lambda row: int(row.get("context_length", 0) or 0), reverse=True)
    else:
        rows = sorted(rows, key=lambda row: str(row.get("id", "")))

    if args.limit == 0:
        rows = []
    elif args.limit > 0:
        rows = rows[: args.limit]

    if args.ids_only:
        for row in rows:
            print(f"openrouter/{row.get('id', '')}")
        return 0

    if args.json:
        print(json.dumps(rows, indent=2))
        return 0

    for row in rows:
        pricing = row.get("pricing", {}) or {}
        print(f"{row.get('id', '')}")
        print(
            f"  prompt={pricing.get('prompt', '?')} completion={pricing.get('completion', '?')} context={row.get('context_length', '?')}"
        )
    return 0


# ---------------------------------------------------------------------------
# PRD-011: Plugin System
# ---------------------------------------------------------------------------

def _safe_profile_path(base: Path, profile: str) -> Path:
    """Return ``base / profile`` only when the resolved path stays within *base*.

    Raises ``SystemExit`` if *profile* contains path-traversal components such
    as ``../`` that would escape the base directory.
    """
    resolved = (base / profile).resolve()
    base_resolved = base.resolve()
    try:
        resolved.relative_to(base_resolved)
    except ValueError:
        raise SystemExit(f"Invalid profile name (path traversal detected): {profile!r}")
    return base / profile


def _plugin_registry_path() -> Path:
    return Path(__file__).parent / "config" / "plugin-registry.yaml"


def _load_plugin_registry() -> dict[str, Any]:
    p = _plugin_registry_path()
    if not p.exists():
        return {}
    with p.open() as fh:
        return yaml.safe_load(fh) or {}


def _hermes_venv_pip(cfg: dict[str, Any], profile: str, *pip_args: str) -> subprocess.CompletedProcess:
    venv_pip = tag_home() / "venvs" / profile / "bin" / "pip"
    if not venv_pip.exists():
        venv_pip = hermes_bin(cfg).parent / "pip"
    return subprocess.run([str(venv_pip), *pip_args], capture_output=True, text=True)


def cmd_plugin(args: argparse.Namespace) -> int:
    cfg = load_config(config_path(getattr(args, "config", None)))
    registry = _load_plugin_registry()
    plugins_map: dict[str, Any] = registry.get("plugins", registry).get("registry", {})
    sub = getattr(args, "plugin_subcommand", None)

    if sub == "list" or sub is None:
        if not plugins_map:
            print("No plugins in registry.")
            return 0
        rows = []
        for name, info in plugins_map.items():
            rows.append({"name": name, "description": info.get("description", ""), "pypi": info.get("pypi", "")})
        if getattr(args, "json", False):
            print(json.dumps(rows, indent=2))
        else:
            for r in rows:
                print(f"  {r['name']:<35} {r['description']}")
        return 0

    if sub == "install":
        name = args.plugin_name
        info = plugins_map.get(name)
        if not info:
            print_error(f"Unknown plugin: {name}")
            return 1
        pypi = info.get("pypi", name)
        profile = getattr(args, "profile", None) or cfg.get("master_profile", "orchestrator")
        result = _hermes_venv_pip(cfg, profile, "install", pypi)
        if result.returncode != 0:
            print_error(f"pip install failed: {result.stderr.strip()}")
            return result.returncode
        print_success(f"Installed {name} ({pypi}) into profile '{profile}'")
        return 0

    if sub == "enable":
        name = args.plugin_name
        profile = getattr(args, "profile", None) or cfg.get("master_profile", "orchestrator")
        profile_dir = _safe_profile_path(tag_home() / "profiles", profile)
        env_file = profile_dir / ".env"
        # Normalise the plugin name to a valid env var identifier (replace any
        # non-alphanumeric characters with underscores, not just hyphens).
        env_key_suffix = re.sub(r"[^A-Z0-9]", "_", name.upper())
        line = f"TAG_PLUGIN_{env_key_suffix}_ENABLED=true\n"
        if env_file.exists():
            existing = env_file.read_text()
            if f"TAG_PLUGIN_{env_key_suffix}" in existing:
                env_file.write_text(re.sub(
                    rf"TAG_PLUGIN_{re.escape(env_key_suffix)}_ENABLED=.*\n",
                    line, existing,
                ))
            else:
                with env_file.open("a") as fh:
                    fh.write(line)
        else:
            env_file.parent.mkdir(parents=True, exist_ok=True)
            env_file.write_text(line)
        print_success(f"Enabled plugin '{name}' for profile '{profile}'")
        return 0

    if sub == "disable":
        name = args.plugin_name
        profile = getattr(args, "profile", None) or cfg.get("master_profile", "orchestrator")
        profile_dir = _safe_profile_path(tag_home() / "profiles", profile)
        env_file = profile_dir / ".env"
        if env_file.exists():
            env_key_suffix = re.sub(r"[^A-Z0-9]", "_", name.upper())
            key = f"TAG_PLUGIN_{env_key_suffix}_ENABLED"
            lines = [l for l in env_file.read_text().splitlines(keepends=True)
                     if not l.startswith(key)]
            env_file.write_text("".join(lines))
        print_success(f"Disabled plugin '{name}' for profile '{profile}'")
        return 0

    print_error(f"Unknown subcommand: {sub}")
    return 1


# ---------------------------------------------------------------------------
# PRD-012: Cost Tracking
# ---------------------------------------------------------------------------

_COST_TABLE: dict[str, dict[str, float]] = {
    "openai/gpt-4o": {"prompt": 0.005, "completion": 0.015},
    "openai/gpt-4o-mini": {"prompt": 0.00015, "completion": 0.0006},
    "openai/gpt-4-turbo": {"prompt": 0.01, "completion": 0.03},
    "openai/gpt-3.5-turbo": {"prompt": 0.0005, "completion": 0.0015},
    "anthropic/claude-sonnet-4-6": {"prompt": 0.003, "completion": 0.015},
    "anthropic/claude-opus-4-8": {"prompt": 0.015, "completion": 0.075},
    "anthropic/claude-haiku-4-5": {"prompt": 0.00025, "completion": 0.00125},
    "google/gemini-2.5-pro": {"prompt": 0.00125, "completion": 0.005},
    "google/gemini-2.5-flash": {"prompt": 0.000075, "completion": 0.0003},
    "meta-llama/llama-3.3-70b-instruct": {"prompt": 0.00059, "completion": 0.00079},
}


def _estimate_cost(prompt_tokens: int, completion_tokens: int, model_id: str) -> float:
    entry = _COST_TABLE.get(model_id, {"prompt": 0.001, "completion": 0.002})
    return (prompt_tokens / 1000 * entry["prompt"]) + (completion_tokens / 1000 * entry["completion"])


def cmd_costs(args: argparse.Namespace) -> int:
    cfg = load_config(config_path(getattr(args, "config", None)))
    db_path = runtime_db_path(cfg)
    if not db_path.exists():
        print("No runs database found.")
        return 0
    conn = sqlite3.connect(str(db_path))
    try:
        cols = {row[1] for row in conn.execute("PRAGMA table_info(runs)")}
        if "total_tokens" not in cols:
            print("No cost data recorded yet (run some tasks first).")
            conn.close()
            return 0
        limit = getattr(args, "limit", 20)
        profile_filter = getattr(args, "profile", None)
        where = "WHERE master_profile = ?" if profile_filter else ""
        params = (profile_filter,) if profile_filter else ()
        rows = conn.execute(
            f"SELECT id, master_profile, model_id, prompt_tokens, completion_tokens, total_tokens, "
            f"estimated_cost_usd, created_at FROM runs {where} ORDER BY created_at DESC LIMIT ?",
            (*params, limit),
        ).fetchall()
        agg = conn.execute(
            f"SELECT SUM(prompt_tokens), SUM(completion_tokens), SUM(total_tokens), "
            f"SUM(estimated_cost_usd) FROM runs {where}",
            params,
        ).fetchone()
    finally:
        conn.close()

    if getattr(args, "json", False):
        out = {
            "runs": [
                {"id": r[0], "profile": r[1], "model_id": r[2], "prompt_tokens": r[3],
                 "completion_tokens": r[4], "total_tokens": r[5],
                 "estimated_cost_usd": r[6], "created_at": r[7]}
                for r in rows
            ],
            "totals": {
                "prompt_tokens": agg[0] or 0,
                "completion_tokens": agg[1] or 0,
                "total_tokens": agg[2] or 0,
                "estimated_cost_usd": agg[3] or 0.0,
            },
        }
        print(json.dumps(out, indent=2))
        return 0

    print(f"{'Run ID':<24} {'Profile':<20} {'Model':<40} {'Tokens':>8} {'Cost':>10}")
    print("-" * 110)
    for r in rows:
        cost = f"${r[6]:.4f}" if r[6] is not None else "n/a"
        print(f"{r[0]:<24} {(r[1] or ''):<20} {(r[2] or ''):<40} {(r[5] or 0):>8} {cost:>10}")
    print("-" * 110)
    total_cost = f"${agg[3]:.4f}" if agg[3] is not None else "n/a"
    print(f"{'TOTAL':<85} {(agg[2] or 0):>8} {total_cost:>10}")
    return 0


# ---------------------------------------------------------------------------
# PRD-013: Distributed Tracing
# ---------------------------------------------------------------------------

def cmd_trace(args: argparse.Namespace) -> int:
    cfg = load_config(config_path(getattr(args, "config", None)))
    db_path = runtime_db_path(cfg)
    if not db_path.exists():
        print("No spans database found.")
        return 0

    conn = sqlite3.connect(str(db_path))
    try:
        sub = getattr(args, "trace_subcommand", None)

        if sub == "list" or sub is None:
            rows = conn.execute(
                "SELECT DISTINCT trace_id, MIN(started_at) as t, COUNT(*) as n FROM spans "
                "GROUP BY trace_id ORDER BY t DESC LIMIT ?",
                (getattr(args, "limit", 20),),
            ).fetchall()
            if getattr(args, "json", False):
                print(json.dumps([{"trace_id": r[0], "started_at": r[1], "span_count": r[2]} for r in rows], indent=2))
            else:
                print(f"{'Trace ID':<36} {'Started':<28} {'Spans':>6}")
                print("-" * 74)
                for r in rows:
                    print(f"{r[0]:<36} {r[1]:<28} {r[2]:>6}")
            return 0

        if sub == "show":
            trace_id = args.trace_id
            rows = conn.execute(
                "SELECT id, trace_id, parent_id, name, profile, model_id, started_at, "
                "finished_at, duration_ms, status, prompt_tokens, completion_tokens, "
                "attributes, error_msg FROM spans WHERE trace_id = ? ORDER BY started_at",
                (trace_id,),
            ).fetchall()
            if not rows:
                print(f"No spans found for trace {trace_id}")
                return 1
            if getattr(args, "json", False):
                col = ["id","trace_id","parent_id","name","profile","model_id","started_at",
                       "finished_at","duration_ms","status","prompt_tokens","completion_tokens",
                       "attributes","error_msg"]
                print(json.dumps([dict(zip(col, r)) for r in rows], indent=2))
                return 0
            try:
                from tag.tracing import Span, render_trace_terminal
                spans = []
                for r in rows:
                    s = Span(
                        id=r[0], trace_id=r[1], parent_id=r[2], name=r[3],
                        profile=r[4], model_id=r[5], started_at=r[6],
                        finished_at=r[7], duration_ms=r[8], status=r[9],
                        prompt_tokens=r[10], completion_tokens=r[11],
                        attributes=json.loads(r[12] or "{}"), error_msg=r[13],
                    )
                    spans.append(s)
                print(render_trace_terminal(spans))
            except ImportError:
                for r in rows:
                    print(f"  {r[3]:<40} {r[9]:<8} {r[8] or 0}ms")
            return 0

        if sub == "export":
            endpoint = args.endpoint
            profile = getattr(args, "profile", None) or cfg.get("master_profile", "orchestrator")
            trace_id = getattr(args, "trace_id", None)
            where = "WHERE trace_id = ?" if trace_id else ""
            params = (trace_id,) if trace_id else ()
            rows = conn.execute(
                f"SELECT id, trace_id, parent_id, name, profile, model_id, started_at, "
                f"finished_at, duration_ms, status, prompt_tokens, completion_tokens, "
                f"attributes, error_msg FROM spans {where} ORDER BY started_at",
                params,
            ).fetchall()
            try:
                from tag.tracing import export_spans_otlp
                ok = export_spans_otlp(rows, endpoint)
                if ok:
                    print_success(f"Exported {len(rows)} spans to {endpoint}")
                else:
                    print_error(f"OTLP export failed — check endpoint: {endpoint}")
                    return 1
            except ImportError:
                print_error("tag.tracing not available")
                return 1
            return 0

    finally:
        conn.close()

    # PRD-032 extension: replay, diff, checkpoint, snapshot
    if sub in ("replay", "diff", "checkpoint", "snapshot"):
        return cmd_trace_extended(args)

    print_error(f"Unknown subcommand: {sub}")
    return 1


# ---------------------------------------------------------------------------
# PRD-014: MCP Server Registry
# ---------------------------------------------------------------------------

def _load_mcp_registry() -> dict[str, Any]:
    p = Path(__file__).parent / "config" / "mcp-registry.yaml"
    if not p.exists():
        return {}
    with p.open() as fh:
        return yaml.safe_load(fh) or {}


def cmd_mcp_registry(args: argparse.Namespace) -> int:
    cfg = load_config(config_path(getattr(args, "config", None)))
    reg = _load_mcp_registry()
    servers: dict[str, Any] = reg.get("servers", {})
    sub = getattr(args, "mcp_reg_subcommand", None)

    if sub == "list" or sub is None:
        category_filter = getattr(args, "category", None)
        rows = []
        for name, info in servers.items():
            if category_filter and info.get("category") != category_filter:
                continue
            rows.append({
                "name": name,
                "description": info.get("description", ""),
                "category": info.get("category", ""),
                "requires_env": info.get("requires_env", []),
            })
        if getattr(args, "json", False):
            print(json.dumps(rows, indent=2))
        else:
            print(f"{'Name':<30} {'Category':<14} {'Description'}")
            print("-" * 80)
            for r in rows:
                env_note = f" [needs: {', '.join(r['requires_env'])}]" if r["requires_env"] else ""
                print(f"  {r['name']:<28} {r['category']:<14} {r['description']}{env_note}")
        return 0

    if sub == "install":
        name = args.server_name
        info = servers.get(name)
        if not info:
            print_error(f"Unknown MCP server: {name}")
            return 1
        install = info.get("install", {})
        pkg = install.get("package", name)
        itype = install.get("type", "npm")
        if itype == "npm":
            result = subprocess.run(["npm", "install", "-g", pkg], capture_output=True, text=True)
        elif itype == "pip":
            result = subprocess.run([sys.executable, "-m", "pip", "install", pkg], capture_output=True, text=True)
        else:
            print_error(f"Unknown install type: {itype}")
            return 1
        if result.returncode != 0:
            print_error(f"Install failed: {result.stderr.strip()}")
            return result.returncode
        print_success(f"Installed MCP server '{name}' ({pkg})")
        return 0

    if sub == "enable":
        name = args.server_name
        info = servers.get(name)
        if not info:
            print_error(f"Unknown MCP server: {name}")
            return 1
        profile = getattr(args, "profile", None) or cfg.get("master_profile", "orchestrator")
        cfg_block = info.get("config", {})
        profile_dir = _safe_profile_path(tag_home() / "profiles", profile)
        profile_cfg_path = profile_dir / "lab-config.yaml"
        if profile_cfg_path.exists():
            with profile_cfg_path.open() as fh:
                pcfg = yaml.safe_load(fh) or {}
        else:
            pcfg = {}
        mcp_list: list = pcfg.setdefault("mcp_servers", [])
        existing_names = [e.get("name") for e in mcp_list]
        if name not in existing_names:
            mcp_list.append({"name": name, **cfg_block})
            profile_cfg_path.parent.mkdir(parents=True, exist_ok=True)
            write_yaml(profile_cfg_path, pcfg, force=True)
            print_success(f"Enabled MCP server '{name}' for profile '{profile}'")
        else:
            print(f"MCP server '{name}' is already enabled for profile '{profile}'")
        return 0

    if sub == "disable":
        name = args.server_name
        profile = getattr(args, "profile", None) or cfg.get("master_profile", "orchestrator")
        profile_dir = _safe_profile_path(tag_home() / "profiles", profile)
        profile_cfg_path = profile_dir / "lab-config.yaml"
        if profile_cfg_path.exists():
            with profile_cfg_path.open() as fh:
                pcfg = yaml.safe_load(fh) or {}
            mcp_list = pcfg.get("mcp_servers", [])
            pcfg["mcp_servers"] = [e for e in mcp_list if e.get("name") != name]
            write_yaml(profile_cfg_path, pcfg, force=True)
            print_success(f"Disabled MCP server '{name}' for profile '{profile}'")
        else:
            print(f"No profile config found for '{profile}'")
        return 0

    print_error(f"Unknown subcommand: {sub}")
    return 1


# ---------------------------------------------------------------------------
# PRD-015: Profile Templates
# ---------------------------------------------------------------------------

_REDACT_PATTERNS = re.compile(
    r"(api[_-]?key|secret|token|password|credential|auth|url)",
    re.IGNORECASE,
)


def _redact_env(key: str, val: str) -> str:
    if _REDACT_PATTERNS.search(key):
        return f"<{key.upper()}>"
    return val


def cmd_template(args: argparse.Namespace) -> int:
    cfg = load_config(config_path(getattr(args, "config", None)))
    sub = getattr(args, "template_subcommand", None)

    if sub == "export" or sub is None:
        profile = getattr(args, "profile", None) or cfg.get("master_profile", "orchestrator")
        profile_dir = tag_home() / "profiles" / profile
        env_file = profile_dir / ".env"
        cfg_file = profile_dir / "lab-config.yaml"

        template: dict[str, Any] = {
            "name": profile,
            "version": "1",
            "description": f"TAG profile template for '{profile}'",
            "env": {},
            "config": {},
        }

        if env_file.exists():
            for line in env_file.read_text().splitlines():
                line = line.strip()
                if not line or line.startswith("#") or "=" not in line:
                    continue
                k, _, v = line.partition("=")
                template["env"][k.strip()] = _redact_env(k.strip(), v.strip())

        if cfg_file.exists():
            with cfg_file.open() as fh:
                template["config"] = yaml.safe_load(fh) or {}

        out_path = getattr(args, "output", None)
        yaml_text = yaml.dump(template, default_flow_style=False, sort_keys=False)
        if out_path:
            Path(out_path).write_text(yaml_text)
            print_success(f"Template exported to {out_path}")
        else:
            print(yaml_text)
        return 0

    if sub == "import":
        tmpl_path = args.template_file
        with open(tmpl_path) as fh:
            tmpl = yaml.safe_load(fh)
        if not isinstance(tmpl, dict):
            print_error(f"Template file '{tmpl_path}' does not contain a valid YAML mapping")
            return 1
        profile = getattr(args, "profile", None) or tmpl.get("name", "imported")
        profile_dir = tag_home() / "profiles" / profile
        profile_dir.mkdir(parents=True, exist_ok=True)

        env_data = tmpl.get("env", {})
        if env_data:
            env_file = profile_dir / ".env"
            lines = []
            for k, v in env_data.items():
                if str(v).startswith("<") and str(v).endswith(">"):
                    lines.append(f"# {k}=<fill in>")
                else:
                    lines.append(f"{k}={v}")
            env_file.write_text("\n".join(lines) + "\n")

        cfg_data = tmpl.get("config", {})
        if cfg_data:
            write_yaml(profile_dir / "lab-config.yaml", cfg_data, force=True)

        print_success(f"Template imported as profile '{profile}'")
        return 0

    if sub == "fetch":
        url = args.url
        try:
            with urllib.request.urlopen(url, timeout=15) as resp:
                tmpl_text = resp.read().decode()
        except urllib.error.URLError as exc:
            print_error(f"Failed to fetch template: {exc}")
            return 1
        print(tmpl_text)
        return 0

    print_error(f"Unknown subcommand: {sub}")
    return 1


# ---------------------------------------------------------------------------
# PRD-016: Webhook Event Hooks
# ---------------------------------------------------------------------------

def _interpolate(template: str, payload: dict[str, Any]) -> str:
    for k, v in payload.items():
        template = template.replace(f"{{{{{k}}}}}", str(v))
    return template


def _execute_hook(hook: dict[str, Any], payload: dict[str, Any]) -> bool:
    hook_type = hook.get("type", "shell")
    if hook_type == "shell":
        cmd_str = _interpolate(hook.get("command", ""), payload)
        try:
            result = subprocess.run(cmd_str, shell=True, capture_output=True, text=True, timeout=30)
            return result.returncode == 0
        except subprocess.TimeoutExpired:
            return False
    if hook_type == "webhook":
        url = hook.get("url", "")
        data = json.dumps(payload).encode()
        req = urllib.request.Request(
            url, data=data, method="POST",
            headers={"Content-Type": "application/json"},
        )
        try:
            with urllib.request.urlopen(req, timeout=10):
                return True
        except urllib.error.URLError:
            return False
    return False


def _fire_hooks(cfg: dict[str, Any], event_type: str, payload: dict[str, Any], db_path: Path | None = None) -> int:
    hooks: list[dict[str, Any]] = cfg.get("hooks", {}).get(event_type, [])
    if not hooks:
        return 0
    fired = 0
    for hook in hooks:
        exc_msg: str | None = None
        try:
            ok = _execute_hook(hook, payload)
        except Exception as exc:
            ok = False
            exc_msg = str(exc)
        if ok:
            fired += 1
        if db_path:
            try:
                conn = sqlite3.connect(str(db_path))
                conn.execute(
                    "INSERT INTO hook_log (id, hook_name, event_id, status, response, fired_at) "
                    "VALUES (?,?,?,?,?,datetime('now'))",
                    (uuid.uuid4().hex[:12], hook.get("name", ""), event_type,
                     "ok" if ok else "error", exc_msg),
                )
                conn.commit()
                conn.close()
            except Exception:
                pass
    return fired


def cmd_hooks(args: argparse.Namespace) -> int:
    cfg = load_config(config_path(getattr(args, "config", None)))
    sub = getattr(args, "hooks_subcommand", None)

    if sub == "list" or sub is None:
        hooks_cfg: dict[str, Any] = cfg.get("hooks", {})
        if not hooks_cfg:
            print("No hooks configured.")
            return 0
        if getattr(args, "json", False):
            print(json.dumps(hooks_cfg, indent=2))
            return 0
        for event_type, hook_list in hooks_cfg.items():
            print(f"\n  {event_type}:")
            for h in hook_list:
                print(f"    - {h.get('name', '(unnamed)')}: {h.get('type', 'shell')}")
        return 0

    if sub == "log":
        db_path = runtime_db_path(cfg)
        if not db_path.exists():
            print("No hook log found.")
            return 0
        conn = sqlite3.connect(str(db_path))
        rows = conn.execute(
            "SELECT id, hook_name, event_id, status, response, fired_at "
            "FROM hook_log ORDER BY fired_at DESC LIMIT ?",
            (getattr(args, "limit", 50),),
        ).fetchall()
        conn.close()
        if getattr(args, "json", False):
            print(json.dumps([
                {"id": r[0], "hook_name": r[1], "event_type": r[2],
                 "status": r[3], "response": r[4], "fired_at": r[5]}
                for r in rows
            ], indent=2))
            return 0
        print(f"{'ID':<14} {'Event':<20} {'Hook':<25} {'Status':<8} {'Time'}")
        print("-" * 90)
        for r in rows:
            print(f"  {r[0]:<12} {r[1]:<20} {r[2]:<25} {r[3]:<8} {r[5]}")
        return 0

    if sub == "test":
        event_type = args.event_type
        payload = {"event_type": event_type, "test": "true", "timestamp": str(dt.datetime.now(dt.timezone.utc))}
        fired = _fire_hooks(cfg, event_type, payload)
        print_success(f"Fired {fired} hook(s) for event '{event_type}'")
        return 0

    print_error(f"Unknown subcommand: {sub}")
    return 1


# ---------------------------------------------------------------------------
# PRD-017: Multi-Model Comparison
# ---------------------------------------------------------------------------

def cmd_compare(args: argparse.Namespace) -> int:
    cfg = load_config(config_path(getattr(args, "config", None)))
    db_path = runtime_db_path(cfg)
    sub = getattr(args, "compare_subcommand", None)

    if sub == "list" or sub is None:
        if not db_path.exists():
            print("No benchmark database found.")
            return 0
        conn = sqlite3.connect(str(db_path))
        rows = conn.execute(
            "SELECT id, suite_path, created_at, status, models FROM benchmark_comparisons "
            "ORDER BY created_at DESC LIMIT ?",
            (getattr(args, "limit", 20),),
        ).fetchall()
        conn.close()
        if getattr(args, "json", False):
            print(json.dumps([
                {"id": r[0], "suite_path": r[1], "created_at": r[2], "status": r[3], "models": r[4]}
                for r in rows
            ], indent=2))
            return 0
        print(f"{'ID':<14} {'Suite':<40} {'Status':<12} {'Created'}")
        print("-" * 90)
        for r in rows:
            print(f"  {r[0]:<12} {r[1]:<40} {r[3]:<12} {r[2]}")
        return 0

    if sub == "show":
        comparison_id = args.comparison_id
        if not db_path.exists():
            print("No benchmark database found.")
            return 0
        conn = sqlite3.connect(str(db_path))
        meta = conn.execute(
            "SELECT id, suite_path, created_at, status, models FROM benchmark_comparisons WHERE id = ?",
            (comparison_id,),
        ).fetchone()
        if not meta:
            print_error(f"Comparison '{comparison_id}' not found")
            conn.close()
            return 1
        results = conn.execute(
            "SELECT model_id, case_id, quality_score, latency_ms, prompt_tokens, completion_tokens, output "
            "FROM benchmark_results WHERE comparison_id = ? ORDER BY case_id, quality_score DESC",
            (comparison_id,),
        ).fetchall()
        conn.close()
        if getattr(args, "json", False):
            print(json.dumps({
                "id": meta[0], "suite_path": meta[1], "created_at": meta[2],
                "status": meta[3], "models": meta[4],
                "results": [
                    {"model_id": r[0], "case_id": r[1], "quality_score": r[2],
                     "latency_ms": r[3], "prompt_tokens": r[4], "completion_tokens": r[5]}
                    for r in results
                ],
            }, indent=2))
            return 0
        print(f"Comparison: {meta[1]} (id={meta[0]})")
        print(f"Status:     {meta[3]}  |  Created: {meta[2]}")
        print(f"Models:     {meta[4]}")
        print(f"\n{'Model':<40} {'Case':<25} {'Score':>6} {'Latency':>10}")
        print("-" * 90)
        for r in results:
            print(f"  {r[0]:<38} {r[1]:<25} {r[2] or '-':>6} {(str(r[3]) + 'ms') if r[3] else 'n/a':>10}")
        return 0

    if sub == "run":
        profile = getattr(args, "profile", None) or cfg.get("master_profile", "orchestrator")
        model_refs = getattr(args, "model_ref", [])
        suite_path = getattr(args, "suite", None)
        if not model_refs:
            print_error("Provide at least one --model-ref")
            return 1
        if not suite_path:
            print_error("Provide --suite <path>")
            return 1
        with open(suite_path) as fh:
            suite = yaml.safe_load(fh) or {}
        cases = suite.get("cases", [])
        if not cases:
            print_error("Suite has no cases")
            return 1

        comparison_id = uuid.uuid4().hex[:12]
        comparison_name = suite.get("name", Path(suite_path).stem)
        conn = open_db(cfg)
        conn.execute(
            "INSERT INTO benchmark_comparisons (id, suite_path, created_at, status, models) "
            "VALUES (?,?,datetime('now'),?,?)",
            (comparison_id, str(suite_path), "running", json.dumps(model_refs)),
        )
        conn.commit()

        for case in cases:
            case_name = case.get("name", "unnamed")
            prompt_text = case.get("prompt", "")
            for model_ref in model_refs:
                print(f"  Running case '{case_name}' with model '{model_ref}'...")
                env = profile_exec_env(cfg, profile)
                env["HERMES_MODEL"] = model_ref
                start = time.monotonic()
                try:
                    result = subprocess.run(
                        [str(hermes_bin(cfg)), "chat", "-q", prompt_text, "-Q"],
                        env=env, capture_output=True, text=True, timeout=120,
                    )
                    latency = int((time.monotonic() - start) * 1000)
                    output = result.stdout.strip()
                    score = None
                except subprocess.TimeoutExpired:
                    latency = 120000
                    output = "(timeout)"
                    score = 0
                result_id = uuid.uuid4().hex[:12]
                conn.execute(
                    "INSERT INTO benchmark_results (id, comparison_id, model_id, case_id, quality_score, "
                    "latency_ms, prompt_tokens, completion_tokens, output) VALUES (?,?,?,?,?,?,?,?,?)",
                    (result_id, comparison_id, model_ref, case_name, score, latency, 0, 0, output),
                )
                conn.commit()

        conn.close()
        print_success(f"Comparison '{comparison_name}' saved (id={comparison_id})")
        return 0

    print_error(f"Unknown subcommand: {sub}")
    return 1


# ---------------------------------------------------------------------------
# PRD-018: Context Window Management
# ---------------------------------------------------------------------------

def cmd_context(args: argparse.Namespace) -> int:
    cfg = load_config(config_path(getattr(args, "config", None)))
    profile = getattr(args, "profile", None) or cfg.get("master_profile", "orchestrator")
    sub = getattr(args, "context_subcommand", None)

    if sub == "show" or sub is None:
        result = subprocess.run(
            [str(hermes_bin(cfg)), "sessions", "list", "--json"],
            env=profile_exec_env(cfg, profile),
            capture_output=True, text=True,
        )
        if result.returncode != 0:
            print_error(f"Failed to list sessions: {result.stderr.strip()}")
            return 1
        try:
            sessions = json.loads(result.stdout)
        except json.JSONDecodeError:
            print(result.stdout)
            return 0
        if getattr(args, "json", False):
            print(json.dumps(sessions, indent=2))
            return 0
        if not sessions:
            print(f"No active sessions for profile '{profile}'")
            return 0
        print(f"Sessions for profile '{profile}':")
        for sess in sessions[:20]:
            sid = sess.get("id", sess.get("session_id", "?"))
            tokens = sess.get("token_count", sess.get("tokens", "?"))
            model = sess.get("model_id", sess.get("model", "?"))
            print(f"  {sid:<40} tokens={tokens:<10} model={model}")
        return 0

    if sub == "compress":
        session_id = getattr(args, "session_id", None)
        if not session_id:
            print_error("Provide --session-id")
            return 1
        cmd_args = [str(hermes_bin(cfg)), "sessions", "compress", session_id]
        result = subprocess.run(
            cmd_args, env=profile_exec_env(cfg, profile),
            capture_output=True, text=True,
        )
        if result.returncode != 0:
            print_error(f"Context compression failed: {result.stderr.strip()}")
            return 1
        print_success(f"Context compressed for session '{session_id}'")
        return 0

    if sub == "trim":
        session_id = getattr(args, "session_id", None)
        keep_last = getattr(args, "keep_last", 10)
        if not session_id:
            print_error("Provide --session-id")
            return 1
        result = subprocess.run(
            [str(hermes_bin(cfg)), "sessions", "trim", session_id, "--keep-last", str(keep_last)],
            env=profile_exec_env(cfg, profile),
            capture_output=True, text=True,
        )
        if result.returncode != 0:
            print_error(f"Context trim failed: {result.stderr.strip()}")
            return 1
        print_success(f"Trimmed session '{session_id}' to last {keep_last} turns")
        return 0

    print_error(f"Unknown subcommand: {sub}")
    return 1


# ---------------------------------------------------------------------------
# PRD-019: Natural Language Shell
# ---------------------------------------------------------------------------

def cmd_shell(args: argparse.Namespace) -> int:
    cfg = load_config(config_path(getattr(args, "config", None)))
    profile = getattr(args, "profile", None) or cfg.get("master_profile", "orchestrator")
    try:
        from tag.shell_mode import run_shell
        return run_shell(cfg, profile)
    except ImportError as exc:
        print_error(f"Shell mode not available: {exc}")
        return 1


# ---------------------------------------------------------------------------
# PRD-020: CI/CD Integration
# ---------------------------------------------------------------------------

def cmd_review_pr(args: argparse.Namespace) -> int:
    cfg = load_config(config_path(getattr(args, "config", None)))
    repo = getattr(args, "repo", None)
    pr_number = getattr(args, "pr", None)
    profile = getattr(args, "profile", None) or cfg.get("master_profile", "orchestrator")
    post_comments = getattr(args, "post_comments", False)

    try:
        from tag.ci import (
            fetch_pr_diff,
            fetch_pr_metadata,
            post_pr_comment,
            build_review_prompt,
        )
    except ImportError as exc:
        print_error(f"tag.ci not available: {exc}")
        return 1

    if not repo or not pr_number:
        print_error("Provide --repo owner/name and --pr NUMBER")
        return 1

    print(f"Fetching PR #{pr_number} from {repo}...")
    try:
        diff = fetch_pr_diff(repo, pr_number)
        metadata = fetch_pr_metadata(repo, pr_number)
    except (RuntimeError, ValueError) as exc:
        print_error(str(exc))
        return 1

    prompt_text = build_review_prompt(diff, metadata)

    print(f"Running code review with profile '{profile}'...")
    result = subprocess.run(
        [str(hermes_bin(cfg)), "chat", "-q", prompt_text, "-Q"],
        env=profile_exec_env(cfg, profile),
        capture_output=True, text=True,
    )
    review_text = result.stdout.strip()

    if not review_text:
        print_error("No review output received from agent")
        return 1

    if post_comments:
        body = f"## TAG Automated Code Review\n\n{review_text}\n\n---\n*Generated by tag-agent*"
        ok = post_pr_comment(repo, pr_number, body)
        if ok:
            print_success(f"Posted review comment to {repo}#{pr_number}")
        else:
            print_warning("Failed to post comment — printing review instead:")
            print(review_text)
    else:
        print(review_text)

    return 0


def cmd_ci(args: argparse.Namespace) -> int:
    cfg = load_config(config_path(getattr(args, "config", None)))
    sub = getattr(args, "ci_subcommand", None)
    profile = getattr(args, "profile", None) or cfg.get("master_profile", "orchestrator")

    try:
        from tag.ci import (
            build_diagnose_prompt,
            read_ci_log,
            detect_git_host,
            get_staged_diff,
        )
    except ImportError as exc:
        print_error(f"tag.ci not available: {exc}")
        return 1

    if sub == "diagnose":
        log_path = getattr(args, "log_file", None)
        if not log_path:
            print_error("Provide --log-file PATH")
            return 1
        try:
            log_content = read_ci_log(Path(log_path))
        except FileNotFoundError:
            print_error(f"Log file not found: {log_path}")
            return 1
        prompt_text = build_diagnose_prompt(log_content)
        result = subprocess.run(
            [str(hermes_bin(cfg)), "chat", "-q", prompt_text, "-Q"],
            env=profile_exec_env(cfg, profile),
            capture_output=True, text=True,
        )
        print(result.stdout.strip() or "(no output)")
        return result.returncode

    if sub == "commit-lint":
        diff = get_staged_diff()
        if not diff.strip():
            print("No staged changes. Stage your changes with `git add` first.")
            return 1
        prompt_text = textwrap.dedent(f"""\
            Review the following staged git diff and suggest a concise conventional commit message.
            Format: <type>(<scope>): <subject>
            Types: feat, fix, docs, style, refactor, perf, test, chore
            Keep subject under 72 characters.

            Diff:
            {diff[:4000]}
        """)
        result = subprocess.run(
            [str(hermes_bin(cfg)), "chat", "-q", prompt_text, "-Q"],
            env=profile_exec_env(cfg, profile),
            capture_output=True, text=True,
        )
        print(result.stdout.strip() or "(no output)")
        return result.returncode

    if sub == "status":
        host = detect_git_host()
        print(f"Git host: {host}")
        result = subprocess.run(["git", "status", "--short"], capture_output=True, text=True)
        print(result.stdout or "(clean)")
        return 0

    print_error(f"Unknown subcommand: {sub}")
    return 1


# ---------------------------------------------------------------------------
# PRD-021: Agent Loop / Autonomous Mode
# ---------------------------------------------------------------------------

def _launch_loop_worker(cfg: dict[str, Any], loop_id: str) -> int:
    """Spawn loop_agent as a detached background process. Returns PID."""
    db_path = runtime_db_path(cfg)
    proc = subprocess.Popen(
        [sys.executable, "-m", "tag.loop_agent",
         "--loop-id", loop_id,
         "--config", str(config_path(None)),
         "--db", str(db_path)],
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
        start_new_session=True,
    )
    return proc.pid


def cmd_loop(args: argparse.Namespace) -> int:
    """PRD-021: Autonomous agent loop with iteration cap and goal detection."""
    cfg = load_config(config_path(getattr(args, "config", None)))
    ensure_runtime_dirs(cfg)
    db = open_db(cfg)
    sub = getattr(args, "loop_subcommand", "list")

    if sub == "start":
        goal = (getattr(args, "goal", "") or "").strip()
        if not goal:
            db.close()
            print_error("--goal TEXT is required")
            return 1
        profile = getattr(args, "profile", None) or cfg["defaults"]["master_profile"]
        max_iters = getattr(args, "max_iters", 10)
        if max_iters is None:
            max_iters = 10
        if max_iters < 1:
            db.close()
            print_error("--max-iters must be >= 1")
            return 1
        approval = getattr(args, "approval", "auto") or "auto"
        if approval not in ("auto", "human"):
            db.close()
            print_error("--approval must be 'auto' or 'human'")
            return 1

        loop_id = uuid.uuid4().hex[:12]
        now = utc_now()
        db.execute(
            """INSERT INTO loop_runs(id, profile, goal, max_iters, current_iter,
               status, approval, created_at, updated_at)
               VALUES(?,?,?,?,0,'running',?,?,?)""",
            (loop_id, profile, goal, max_iters, approval, now, now),
        )
        db.commit()

        pid = _launch_loop_worker(cfg, loop_id)
        db.close()

        if getattr(args, "json", False):
            print(json.dumps({"loop_id": loop_id, "pid": pid, "status": "running"}))
        else:
            print(f"loop started: {loop_id}  (worker pid {pid})")
            print(f"goal: {goal[:80]}")
            print(f"max iterations: {max_iters}  approval: {approval}")
        return 0

    if sub == "list":
        rows = db.execute(
            "SELECT id, profile, status, current_iter, max_iters, created_at, goal "
            "FROM loop_runs ORDER BY created_at DESC LIMIT 20"
        ).fetchall()
        db.close()
        if getattr(args, "json", False):
            print(json.dumps([dict(r) for r in rows], indent=2))
            return 0
        if not rows:
            print("No loop runs.")
            return 0
        print(f"  {'ID':<14} {'PROFILE':<16} {'STATUS':<12} {'ITERS':<8} {'GOAL'}")
        print("  " + "─" * 76)
        for r in rows:
            goal_short = (r["goal"] or "")[:40]
            iters = f"{r['current_iter']}/{r['max_iters']}"
            print(f"  {r['id']:<14} {r['profile']:<16} {r['status']:<12} {iters:<8} {goal_short}")
        return 0

    if sub == "status":
        loop_id = getattr(args, "loop_id", None)
        if not loop_id:
            db.close()
            print_error("LOOP_ID required")
            return 1
        run = db.execute("SELECT * FROM loop_runs WHERE id=?", (loop_id,)).fetchone()
        if not run:
            db.close()
            print_error(f"Loop '{loop_id}' not found")
            return 1
        iters = db.execute(
            "SELECT iteration, decision, output FROM loop_iterations WHERE loop_id=? ORDER BY iteration",
            (loop_id,),
        ).fetchall()
        db.close()
        run_d = dict(run)
        if getattr(args, "json", False):
            run_d["iterations"] = [dict(i) for i in iters]
            print(json.dumps(run_d, indent=2))
            return 0
        print(f"Loop: {loop_id}")
        print(f"  Profile: {run_d['profile']}  Status: {run_d['status']}")
        print(f"  Goal: {run_d['goal'][:80]}")
        print(f"  Iterations: {run_d['current_iter']}/{run_d['max_iters']}")
        for it in iters:
            print(f"  [{it['iteration']}] {it['decision']}: {(it['output'] or '')[:80]}")
        return 0

    if sub == "abort":
        loop_id = getattr(args, "loop_id", None)
        if not loop_id:
            db.close()
            print_error("LOOP_ID required")
            return 1
        db.execute(
            "UPDATE loop_runs SET status='aborted', updated_at=? WHERE id=? AND status='running'",
            (utc_now(), loop_id),
        )
        db.commit()
        db.close()
        print(f"abort requested: {loop_id}")
        return 0

    db.close()
    return 0


# ---------------------------------------------------------------------------
# PRD-022: Cron / Scheduled Agents
# ---------------------------------------------------------------------------

def cmd_cron(args: argparse.Namespace) -> int:
    """PRD-022: Cron-style scheduled agent runs."""
    cfg = load_config(config_path(getattr(args, "config", None)))
    ensure_runtime_dirs(cfg)
    db = open_db(cfg)
    sub = getattr(args, "cron_subcommand", "list")

    if sub == "add":
        from tag.cron_scheduler import validate_cron_expression
        name = (getattr(args, "name", "") or "").strip()
        schedule = (getattr(args, "schedule", "") or "").strip()
        profile = getattr(args, "profile", None) or cfg["defaults"]["master_profile"]
        task = (getattr(args, "task", "") or "").strip()
        if not name or not schedule or not task:
            db.close()
            print_error("--name, --schedule and TASK are required")
            return 1
        try:
            validate_cron_expression(schedule)
        except ValueError as exc:
            db.close()
            print_error(str(exc))
            return 1
        existing = db.execute("SELECT id FROM cron_jobs WHERE name=?", (name,)).fetchone()
        if existing:
            db.close()
            print_error(f"A cron job named '{name}' already exists (names must be unique)")
            return 1
        job_id = uuid.uuid4().hex[:8]
        now = utc_now()
        db.execute(
            """INSERT INTO cron_jobs(id, name, schedule, profile, task, enabled,
               run_count, created_at, updated_at)
               VALUES(?,?,?,?,?,1,0,?,?)""",
            (job_id, name, schedule, profile, task, now, now),
        )
        db.commit()
        db.close()
        if getattr(args, "json", False):
            print(json.dumps({"id": job_id, "name": name, "schedule": schedule}))
        else:
            print(f"cron job added: {job_id}  '{name}'  [{schedule}]")
        return 0

    if sub == "list":
        rows = db.execute(
            "SELECT id, name, schedule, profile, enabled, last_run_at, run_count FROM cron_jobs ORDER BY name"
        ).fetchall()
        db.close()
        if getattr(args, "json", False):
            print(json.dumps([dict(r) for r in rows], indent=2))
            return 0
        if not rows:
            print("No cron jobs.")
            return 0
        print(f"  {'ID':<10} {'NAME':<20} {'SCHEDULE':<16} {'PROFILE':<14} {'EN':<4} {'RUNS':>5} {'LAST RUN'}")
        print("  " + "─" * 80)
        for r in rows:
            en = "✓" if r["enabled"] else "✗"
            last = (r["last_run_at"] or "never")[:19]
            print(f"  {r['id']:<10} {r['name']:<20} {r['schedule']:<16} {r['profile']:<14} {en:<4} {r['run_count']:>5} {last}")
        return 0

    if sub == "remove":
        job_id = getattr(args, "job_id", None)
        if not job_id:
            db.close()
            print_error("JOB_ID required")
            return 1
        cur = db.execute("DELETE FROM cron_jobs WHERE id=?", (job_id,))
        db.commit()
        db.close()
        if cur.rowcount == 0:
            print_error(f"Job '{job_id}' not found")
            return 1
        print(f"removed: {job_id}")
        return 0

    if sub in ("enable", "disable"):
        job_id = getattr(args, "job_id", None)
        if not job_id:
            db.close()
            print_error("JOB_ID required")
            return 1
        enabled = 1 if sub == "enable" else 0
        cur = db.execute(
            "UPDATE cron_jobs SET enabled=?, updated_at=? WHERE id=?",
            (enabled, utc_now(), job_id),
        )
        db.commit()
        db.close()
        if cur.rowcount == 0:
            print_error(f"Job '{job_id}' not found")
            return 1
        print(f"{sub}d: {job_id}")
        return 0

    if sub == "run":
        # Trigger a cron job immediately (one-shot, ignores schedule)
        job_id = getattr(args, "job_id", None)
        if not job_id:
            db.close()
            print_error("JOB_ID required")
            return 1
        job = db.execute("SELECT * FROM cron_jobs WHERE id=?", (job_id,)).fetchone()
        if not job:
            db.close()
            print_error(f"Job '{job_id}' not found")
            return 1
        q_id = uuid.uuid4().hex[:8]
        now = utc_now()
        db.execute(
            """INSERT INTO queue_jobs(id, profile, task, task_type, status, priority, created_at, notify)
               VALUES(?,?,?,?,?,?,?,?)""",
            (q_id, job["profile"], job["task"], "mixed", "queued", 5, now, 1),
        )
        db.execute(
            "UPDATE cron_jobs SET last_run_at=?, run_count=run_count+1, updated_at=? WHERE id=?",
            (now, now, job_id),
        )
        db.commit()
        pid = launch_queue_worker(cfg, q_id)
        queue_update_pid(db, q_id, pid)
        db.commit()
        db.close()
        print(f"triggered: cron job {job_id} → queue job {q_id} (pid {pid})")
        return 0

    if sub == "daemon":
        # Run the cron daemon in-process (blocking)
        db.close()
        from tag.cron_scheduler import run_daemon
        db_path = str(runtime_db_path(cfg))
        cfg_path = str(config_path(getattr(args, "config", None)) or "")
        print(f"TAG cron daemon starting (polling every 30s) — Ctrl+C to stop")
        try:
            run_daemon(db_path, cfg_path)
        except KeyboardInterrupt:
            print("\ncron daemon stopped.")
        return 0

    db.close()
    return 0


# ---------------------------------------------------------------------------
# PRD-024: Repo-Map / Workspace Context
# ---------------------------------------------------------------------------

def cmd_workspace(args: argparse.Namespace) -> int:
    """PRD-024: Workspace file indexing and token-efficient repo-map generation."""
    cfg = load_config(config_path(getattr(args, "config", None)))
    ensure_runtime_dirs(cfg)
    db = open_db(cfg)
    sub = getattr(args, "workspace_subcommand", "status")

    try:
        from tag.workspace import index_workspace, build_workspace_map, workspace_status
    except ImportError as exc:
        db.close()
        print_error(f"tag.workspace not available: {exc}")
        return 1

    root = Path(getattr(args, "path", None) or ".").resolve()

    if sub == "index":
        max_files = getattr(args, "max_files", 500) or 500
        result = index_workspace(db, root, max_files=max_files)
        db.close()
        if getattr(args, "json", False):
            print(json.dumps(result))
        else:
            print(f"Indexed {result['files_indexed']} files ({result['total_tokens']} tokens)")
            if result["max_rank_file"]:
                print(f"Top-ranked file: {result['max_rank_file']}")
        return 0

    if sub == "map":
        budget = getattr(args, "budget", 4000) or 4000
        ws_map = build_workspace_map(db, root, budget_tokens=budget)
        db.close()
        if getattr(args, "json", False):
            print(json.dumps({"map": ws_map}))
        else:
            print(ws_map)
        return 0

    if sub == "status":
        stats = workspace_status(db)
        db.close()
        if getattr(args, "json", False):
            print(json.dumps(stats))
        else:
            if stats["file_count"] == 0:
                print("Workspace not indexed. Run `tag workspace index` first.")
            else:
                print(f"Indexed: {stats['file_count']} files  {stats['total_tokens']} tokens")
                print(f"Last indexed: {stats['last_indexed'] or 'never'}")
        return 0

    if sub == "clear":
        db.execute("DELETE FROM workspace_files")
        db.commit()
        db.close()
        print("Workspace index cleared.")
        return 0

    db.close()
    return 0


# ---------------------------------------------------------------------------
# PRD-025: Semantic Memory with Confidence Decay
# ---------------------------------------------------------------------------

def cmd_memory_semantic(args: argparse.Namespace) -> int:
    """PRD-025: Semantic memory with confidence decay and FTS search."""
    cfg = load_config(config_path(getattr(args, "config", None)))
    ensure_runtime_dirs(cfg)
    db = open_db(cfg)
    profile = getattr(args, "profile", None) or cfg["defaults"]["master_profile"]
    sub = getattr(args, "mem_subcommand", "list")

    try:
        from tag.semantic_memory import (
            add_memory, search_memories, list_memories,
            forget_memory, memory_stats, ensure_schema,
        )
    except ImportError as exc:
        db.close()
        print_error(f"tag.semantic_memory not available: {exc}")
        return 1

    ensure_schema(db)

    if sub == "add":
        content = (getattr(args, "content", "") or "").strip()
        if not content:
            db.close()
            print_error("Memory content required (positional argument or --content)")
            return 1
        mtype = getattr(args, "memory_type", "fact") or "fact"
        confidence = getattr(args, "confidence", 1.0) or 1.0
        try:
            mem_id = add_memory(db, profile, content, memory_type=mtype, confidence=confidence)
        except ValueError as exc:
            db.close()
            print_error(str(exc))
            return 1
        db.close()
        if getattr(args, "json", False):
            print(json.dumps({"id": mem_id, "profile": profile}))
        else:
            print(f"Memory saved: {mem_id}")
        return 0

    if sub == "search":
        query = (getattr(args, "query", "") or "").strip()
        if not query:
            db.close()
            print_error("QUERY required")
            return 1
        limit = getattr(args, "limit", 10) or 10
        mtype = getattr(args, "memory_type", None)
        results = search_memories(db, profile, query, limit=limit, memory_type=mtype)
        db.close()
        if getattr(args, "json", False):
            print(json.dumps(results, indent=2))
            return 0
        if not results:
            print(f"No memories found for: {query!r}")
            return 0
        for r in results:
            conf = r["confidence"]
            print(f"[{r['id'][:8]}] ({r['memory_type']} conf={conf:.2f}) {r['content'][:80]}")
        return 0

    if sub == "list":
        limit = getattr(args, "limit", 20) or 20
        mtype = getattr(args, "memory_type", None)
        mems = list_memories(db, profile, memory_type=mtype, limit=limit)
        db.close()
        if getattr(args, "json", False):
            print(json.dumps(mems, indent=2))
            return 0
        if not mems:
            print(f"No memories for profile '{profile}'.")
            return 0
        for m in mems:
            print(f"[{m['id'][:8]}] ({m['memory_type']} conf={m['confidence']:.2f}) {m['content'][:80]}")
        return 0

    if sub == "forget":
        mem_id = getattr(args, "mem_id", None)
        if not mem_id:
            db.close()
            print_error("MEMORY_ID required")
            return 1
        deleted = forget_memory(db, mem_id, profile)
        db.close()
        if not deleted:
            print_error(f"Memory '{mem_id}' not found for profile '{profile}'")
            return 1
        print(f"forgotten: {mem_id}")
        return 0

    if sub == "stats":
        stats = memory_stats(db, profile)
        db.close()
        if getattr(args, "json", False):
            print(json.dumps(stats, indent=2))
            return 0
        print(f"Profile: {profile}  Total memories: {stats['total']}")
        for mtype, info in sorted(stats["by_type"].items()):
            print(f"  {mtype:<12}  count={info['count']}  avg_conf_base={info['avg_confidence_base']:.3f}")
        return 0

    db.close()
    return 0


# ---------------------------------------------------------------------------
# PRD-026: Profile Marketplace (pull/push)
# ---------------------------------------------------------------------------

def _profile_sha256(path: Path) -> str:
    import hashlib
    return hashlib.sha256(path.read_bytes()).hexdigest()


def cmd_profile_marketplace(args: argparse.Namespace) -> int:
    """PRD-026: Pull/push profiles from/to GitHub Gist or URL."""
    cfg = load_config(config_path(getattr(args, "config", None)))
    ensure_runtime_dirs(cfg)
    db = open_db(cfg)
    sub = getattr(args, "marketplace_subcommand", None)

    if sub == "pull":
        url = getattr(args, "url", "")
        if not url:
            db.close()
            print_error("URL required (e.g. https://raw.githubusercontent.com/user/repo/main/profile.yaml)")
            return 1
        name = getattr(args, "name", None) or Path(url).stem

        try:
            response = urllib.request.urlopen(url, timeout=15)  # noqa: S310
            content = response.read()
        except urllib.error.URLError as exc:
            db.close()
            print_error(f"Failed to fetch profile: {exc}")
            return 1

        # Basic YAML validation
        try:
            profile_data = yaml.safe_load(content)
            if not isinstance(profile_data, dict):
                raise ValueError("Profile must be a YAML mapping")
        except Exception as exc:
            db.close()
            print_error(f"Invalid profile YAML: {exc}")
            return 1

        sha = hashlib.sha256(content).hexdigest()
        profiles_dir = runtime_home(cfg) / "profiles"
        profiles_dir.mkdir(parents=True, exist_ok=True)
        local_path = profiles_dir / f"{name}.yaml"
        local_path.write_bytes(content)

        now = utc_now()
        db.execute(
            """INSERT INTO profile_cache(id, name, source_url, sha256, local_path, downloaded_at)
               VALUES(?,?,?,?,?,?)
               ON CONFLICT(name) DO UPDATE SET
                 source_url=excluded.source_url, sha256=excluded.sha256,
                 local_path=excluded.local_path, downloaded_at=excluded.downloaded_at""",
            (uuid.uuid4().hex[:12], name, url, sha, str(local_path), now),
        )
        db.commit()
        db.close()

        if getattr(args, "json", False):
            print(json.dumps({"name": name, "sha256": sha, "local_path": str(local_path)}))
        else:
            print(f"Pulled profile: {name}")
            print(f"  SHA256: {sha[:16]}...")
            print(f"  Saved to: {local_path}")
        return 0

    if sub == "push":
        profile_name = getattr(args, "profile_name", None)
        if not profile_name:
            db.close()
            print_error("profile name required")
            return 1
        # Find the profile file
        profiles_dir = runtime_home(cfg) / "profiles"
        candidates = list(profiles_dir.glob(f"{profile_name}.yaml"))
        if not candidates:
            db.close()
            print_error(f"Profile file not found: {profiles_dir}/{profile_name}.yaml")
            return 1
        pfile = candidates[0]
        sha = _profile_sha256(pfile)
        db.close()
        # For now, print info — actual GitHub Gist push requires auth token
        print(f"Profile: {profile_name}")
        print(f"  File: {pfile}")
        print(f"  SHA256: {sha}")
        print("  To push: gh gist create --public --filename profile.yaml " + str(pfile))
        return 0

    if sub == "list":
        rows = db.execute(
            "SELECT name, source_url, sha256, downloaded_at FROM profile_cache ORDER BY name"
        ).fetchall()
        db.close()
        if getattr(args, "json", False):
            print(json.dumps([{"name": r[0], "source_url": r[1], "sha256": r[2][:12], "downloaded_at": r[3]} for r in rows], indent=2))
            return 0
        if not rows:
            print("No cached profiles. Use `tag marketplace pull <url>` to add one.")
            return 0
        for r in rows:
            print(f"  {r[0]:<24} {r[3][:10]}  {r[1][:60]}")
        return 0

    db.close()
    return 0


# ---------------------------------------------------------------------------
# PRD-027: Eval Framework
# ---------------------------------------------------------------------------

def cmd_eval(args: argparse.Namespace) -> int:
    """PRD-027: Run eval suites against TAG profiles."""
    cfg = load_config(config_path(getattr(args, "config", None)))
    ensure_runtime_dirs(cfg)
    db = open_db(cfg)
    sub = getattr(args, "eval_subcommand", "list")

    try:
        from tag.eval_framework import (
            load_suite, score_case, create_eval_run,
            record_case_result, finalize_eval_run,
            list_eval_runs, get_eval_run_detail,
        )
    except ImportError as exc:
        db.close()
        print_error(f"tag.eval_framework not available: {exc}")
        return 1

    if sub == "run":
        suite_path_str = getattr(args, "suite", None)
        if not suite_path_str:
            db.close()
            print_error("--suite SUITE_PATH required")
            return 1
        suite_path = Path(suite_path_str)
        profile = getattr(args, "profile", None) or cfg["defaults"]["master_profile"]
        dry_run = getattr(args, "dry_run", False)

        try:
            suite = load_suite(suite_path)
        except (FileNotFoundError, ValueError) as exc:
            db.close()
            print_error(str(exc))
            return 1

        suite_name = suite.get("name", suite_path.stem)
        run_id = create_eval_run(db, str(suite_path), profile, suite_name)
        cases = suite.get("cases", [])

        if not dry_run:
            print(f"Eval run: {run_id}  suite: {suite_name}  profile: {profile}")
            print(f"Running {len(cases)} cases...")

        passed = 0
        failed = 0
        for case in cases:
            case_id = case.get("id", f"case_{cases.index(case)+1}")
            input_text = case.get("input", "")

            if dry_run:
                output = "(dry-run — no agent invocation)"
                ok, score, reason = True, 1.0, None
            else:
                # Run the case via hermes
                result = subprocess.run(
                    [sys.executable, "-m", "tag", "--config",
                     str(config_path(getattr(args, "config", None)) or ""),
                     "submit", "--task-type", "mixed", "--prompt", input_text,
                     "--master-profile", profile, "--source", "eval"],
                    capture_output=True, text=True, timeout=300,
                )
                output = result.stdout
                ok, score, reason = score_case(case, output)

            record_case_result(
                db, run_id, case_id, input_text, output,
                passed=ok, score=score, failure_reason=reason,
            )
            if ok:
                passed += 1
            else:
                failed += 1
            status_char = "✓" if ok else "✗"
            if not dry_run:
                print(f"  [{status_char}] {case_id}  score={score:.2f}" +
                      (f"  {reason}" if reason else ""))

        summary = finalize_eval_run(db, run_id)
        db.close()

        if getattr(args, "json", False):
            print(json.dumps(summary, indent=2))
        else:
            print(f"\nResults: {passed}/{len(cases)} passed")
        return 0 if failed == 0 else 1

    if sub == "list":
        runs = list_eval_runs(db)
        db.close()
        if getattr(args, "json", False):
            print(json.dumps(runs, indent=2))
            return 0
        if not runs:
            print("No eval runs yet.")
            return 0
        print(f"  {'ID':<18} {'SUITE':<24} {'PROFILE':<14} {'STATUS':<10} {'PASS':<6} {'FAIL':<6}")
        print("  " + "─" * 80)
        for r in runs:
            print(f"  {r['id']:<18} {r['suite_name'][:24]:<24} {r['profile']:<14} "
                  f"{r['status']:<10} {r['pass_count']:<6} {r['fail_count']:<6}")
        return 0

    if sub == "show":
        run_id = getattr(args, "run_id", None)
        if not run_id:
            db.close()
            print_error("RUN_ID required")
            return 1
        detail = get_eval_run_detail(db, run_id)
        db.close()
        if not detail:
            print_error(f"Eval run '{run_id}' not found")
            return 1
        if getattr(args, "json", False):
            print(json.dumps(detail, indent=2))
            return 0
        print(f"Eval run: {detail['id']}")
        print(f"  Suite: {detail['suite_name']}  Profile: {detail['profile']}")
        print(f"  Status: {detail['status']}  {detail['pass_count']}/{detail['total_count']} passed")
        for c in detail.get("cases", []):
            icon = "✓" if c["passed"] else "✗"
            reason = f"  — {c['failure_reason']}" if c.get("failure_reason") else ""
            print(f"  [{icon}] {c['case_id']}  score={c['score']:.2f}{reason}")
        return 0

    db.close()
    return 0


# ---------------------------------------------------------------------------
# PRD-028: Sandbox Code Execution
# ---------------------------------------------------------------------------

def cmd_sandbox(args: argparse.Namespace) -> int:
    """PRD-028: Isolated code execution via restricted subprocess or Docker."""
    cfg = load_config(config_path(getattr(args, "config", None)))
    ensure_runtime_dirs(cfg)
    db = open_db(cfg)
    sub = getattr(args, "sandbox_subcommand", "list")

    try:
        from tag.sandbox import run_in_sandbox, list_sandbox_runs, get_sandbox_run
    except ImportError as exc:
        db.close()
        print_error(f"tag.sandbox not available: {exc}")
        return 1

    if sub == "run":
        command = getattr(args, "command", "")
        if not command:
            db.close()
            print_error("COMMAND required")
            return 1
        backend = getattr(args, "backend", "restricted") or "restricted"
        image = getattr(args, "image", "python:3.12-slim") or "python:3.12-slim"
        timeout = getattr(args, "timeout", 60) or 60

        result = run_in_sandbox(db, command, backend=backend, image=image, timeout=timeout)
        db.close()

        if getattr(args, "json", False):
            out = {k: v for k, v in result.items() if k != "output"}
            out["output_preview"] = (result.get("output") or "")[:200]
            print(json.dumps(out, indent=2))
        else:
            print(f"Sandbox run: {result['id']}  exit={result['exit_code']}")
            if result.get("output"):
                print("--- output ---")
                print(result["output"][:2000])
        return 0 if result.get("exit_code") == 0 else 1

    if sub == "list":
        runs = list_sandbox_runs(db, limit=20)
        db.close()
        if getattr(args, "json", False):
            print(json.dumps(runs, indent=2))
            return 0
        if not runs:
            print("No sandbox runs.")
            return 0
        print(f"  {'ID':<14} {'BACKEND':<12} {'STATUS':<10} {'EXIT':<5} {'COMMAND'}")
        print("  " + "─" * 70)
        for r in runs:
            ec = str(r["exit_code"]) if r["exit_code"] is not None else "?"
            print(f"  {r['id']:<14} {r['backend']:<12} {r['status']:<10} {ec:<5} {r['command']}")
        return 0

    if sub == "result":
        run_id = getattr(args, "run_id", None)
        if not run_id:
            db.close()
            print_error("RUN_ID required")
            return 1
        run = get_sandbox_run(db, run_id)
        db.close()
        if not run:
            print_error(f"Sandbox run '{run_id}' not found")
            return 1
        if getattr(args, "json", False):
            print(json.dumps(run, indent=2))
        else:
            print(f"Sandbox run: {run['id']}  backend: {run['backend']}  exit: {run['exit_code']}")
            print(run.get("output") or "(no output)")
        return 0

    db.close()
    return 0


# ---------------------------------------------------------------------------
# PRD-029: Streaming TUI Dashboard (tag serve enhancement)
# ---------------------------------------------------------------------------

def cmd_serve(args: argparse.Namespace) -> int:
    """PRD-029: Start a local HTTP server serving the TAG dashboard as SSE stream."""
    cfg = load_config(config_path(getattr(args, "config", None)))
    ensure_runtime_dirs(cfg)
    port = getattr(args, "port", 7880) or 7880
    profile = getattr(args, "profile", None) or cfg["defaults"]["master_profile"]

    try:
        import http.server
        import threading

        class _Handler(http.server.BaseHTTPRequestHandler):
            def log_message(self, fmt, *a):
                pass  # Silence default access log

            def do_GET(self):
                if self.path == "/events":
                    self._serve_sse()
                elif self.path == "/" or self.path == "/index.html":
                    self._serve_html()
                else:
                    self.send_error(404)

            def _serve_html(self):
                html = _dashboard_html(profile)
                self.send_response(200)
                self.send_header("Content-Type", "text/html; charset=utf-8")
                self.send_header("Content-Length", str(len(html.encode())))
                self.end_headers()
                self.wfile.write(html.encode())

            def _serve_sse(self):
                self.send_response(200)
                self.send_header("Content-Type", "text/event-stream")
                self.send_header("Cache-Control", "no-cache")
                self.send_header("Access-Control-Allow-Origin", "*")
                self.end_headers()
                try:
                    while True:
                        snap = _dashboard_snapshot(cfg)
                        data = json.dumps(snap)
                        msg = f"data: {data}\n\n"
                        self.wfile.write(msg.encode())
                        self.wfile.flush()
                        time.sleep(3)
                except (BrokenPipeError, ConnectionResetError):
                    pass

        server = http.server.HTTPServer(("127.0.0.1", port), _Handler)
        url = f"http://127.0.0.1:{port}"
        print(f"TAG dashboard server: {url}  (Ctrl+C to stop)")

        # Try to open browser
        try:
            import webbrowser
            threading.Timer(0.5, lambda: webbrowser.open(url)).start()
        except Exception:
            pass

        server.serve_forever()
    except KeyboardInterrupt:
        print("\nServer stopped.")
    return 0


def _dashboard_html(profile: str) -> str:
    """Minimal HTML page that connects to the SSE stream and renders a live table."""
    return f"""<!DOCTYPE html>
<html lang="en">
<head><meta charset="utf-8"><title>TAG Dashboard — {profile}</title>
<style>
body{{font-family:monospace;background:#111;color:#eee;padding:16px}}
h1{{color:#7ec8e3}}table{{border-collapse:collapse;width:100%;margin:8px 0}}
th{{background:#222;color:#7ec8e3;padding:6px 10px;text-align:left}}
td{{padding:4px 10px;border-bottom:1px solid #333}}
.ok{{color:#5fbb5f}}.fail{{color:#e05252}}.run{{color:#e0c000}}
#ts{{float:right;color:#888;font-size:0.8em}}
</style></head>
<body>
<h1>TAG Dashboard <span id=ts></span></h1>
<h2>Recent Runs</h2><table id=runs><tr><th>ID</th><th>Profile</th><th>Status</th><th>When</th></tr></table>
<h2>Queue</h2><table id=queue><tr><th>Job</th><th>Status</th><th>Task</th></tr></table>
<script>
const es=new EventSource('/events');
es.onmessage=e=>{{
  const d=JSON.parse(e.data);
  document.getElementById('ts').textContent=new Date().toLocaleTimeString();
  const runs=document.getElementById('runs');
  runs.innerHTML='<tr><th>ID</th><th>Profile</th><th>Status</th><th>When</th></tr>';
  (d.runs||[]).slice(0,10).forEach(r=>{{
    const cls=r.status==='completed'?'ok':r.status==='failed'?'fail':'run';
    const when=(r.created_at||'').substring(11,16);
    runs.innerHTML+=`<tr><td>${{r.run_id}}</td><td>${{r.master_profile}}</td><td class="${{cls}}">${{r.status}}</td><td>${{when}}</td></tr>`;
  }});
  const q=document.getElementById('queue');
  q.innerHTML='<tr><th>Job</th><th>Status</th><th>Task</th></tr>';
  (d.queue||[]).slice(0,8).forEach(j=>{{
    const cls=j.status==='done'?'ok':j.status==='failed'?'fail':'run';
    q.innerHTML+=`<tr><td>${{j.id}}</td><td class="${{cls}}">${{j.status}}</td><td>${{(j.task||'').substring(0,60)}}</td></tr>`;
  }});
}};
</script></body></html>"""


# ---------------------------------------------------------------------------
# PRD-030: Prompt Cache Analytics
# ---------------------------------------------------------------------------

def cmd_cache(args: argparse.Namespace) -> int:
    """PRD-030: Prompt cache analytics — hit rate and cost savings per profile."""
    cfg = load_config(config_path(getattr(args, "config", None)))
    db_path = runtime_db_path(cfg)
    _json = getattr(args, "json", False)
    if not db_path.exists():
        print(json.dumps([]) if _json else "No runs database found.")
        return 0

    conn = sqlite3.connect(str(db_path))
    try:
        # Check columns exist
        cols = {row[1] for row in conn.execute("PRAGMA table_info(runs)")}
        has_cache = "cache_read_tokens" in cols and "cache_creation_tokens" in cols
        if not has_cache:
            print(json.dumps([]) if _json else "No cache data recorded yet (cache columns not present).")
            return 0

        profile_filter = getattr(args, "profile", None)
        where = "WHERE master_profile=?" if profile_filter else ""
        params = (profile_filter,) if profile_filter else ()

        rows = conn.execute(
            f"""SELECT master_profile, model_id,
                   SUM(prompt_tokens) as pt,
                   SUM(completion_tokens) as ct,
                   SUM(total_tokens) as tt,
                   SUM(COALESCE(cache_read_tokens, 0)) as crt,
                   SUM(COALESCE(cache_creation_tokens, 0)) as cct,
                   SUM(COALESCE(estimated_cost_usd, 0)) as cost,
                   COUNT(*) as runs
                FROM runs {where}
                GROUP BY master_profile, model_id
                ORDER BY tt DESC
                LIMIT 20""",
            params,
        ).fetchall()
    finally:
        conn.close()

    if not rows:
        print(json.dumps([]) if _json else "No run data found.")
        return 0

    if _json:
        out = []
        for r in rows:
            total_input = (r[2] or 0)
            cache_read = (r[5] or 0)
            hit_rate = cache_read / total_input if total_input > 0 else 0.0
            # Cache reads cost 0.1x; savings vs full price
            savings_tokens = cache_read * 0.9  # 90% discount on reads
            out.append({
                "profile": r[0], "model": r[1],
                "prompt_tokens": r[2], "completion_tokens": r[3],
                "cache_read_tokens": r[5], "cache_creation_tokens": r[6],
                "cache_hit_rate": round(hit_rate, 4),
                "estimated_savings_tokens": int(savings_tokens),
                "total_cost_usd": r[7],
                "runs": r[8],
            })
        print(json.dumps(out, indent=2))
        return 0

    print(f"{'Profile':<20} {'Model':<36} {'Prompt':>8} {'CacheRead':>10} {'HitRate':>8} {'Savings':>10}")
    print("-" * 100)
    for r in rows:
        profile, model, pt, ct, tt, crt, cct, cost, runs = r
        hit_rate = (crt / pt * 100) if pt else 0.0
        savings_tokens = (crt or 0) * 0.9
        print(f"{(profile or ''):<20} {(model or ''):<36} {(pt or 0):>8} "
              f"{(crt or 0):>10} {hit_rate:>7.1f}% {int(savings_tokens):>10}")
    return 0


# ---------------------------------------------------------------------------
# PRD-031: Model Fallback Chains
# ---------------------------------------------------------------------------

def cmd_route_fallback(args: argparse.Namespace) -> int:
    """PRD-031: Manage model fallback chains for automatic provider switching."""
    cfg = load_config(config_path(getattr(args, "config", None)))
    ensure_runtime_dirs(cfg)
    db = open_db(cfg)
    sub = getattr(args, "fallback_subcommand", "list")
    profile = getattr(args, "profile", None) or cfg["defaults"]["master_profile"]

    if sub == "add":
        primary = (getattr(args, "primary", "") or "").strip()
        fallback = (getattr(args, "fallback", "") or "").strip()
        if not primary or not fallback:
            db.close()
            print_error("--primary and --fallback model IDs required")
            return 1
        if primary == fallback:
            db.close()
            print_error("--primary and --fallback must be different models")
            return 1
        condition = getattr(args, "condition", "context_overflow") or "context_overflow"
        valid_conditions = {"context_overflow", "error", "timeout", "cost_limit", "any"}
        if condition not in valid_conditions:
            db.close()
            print_error(f"--condition must be one of: {', '.join(sorted(valid_conditions))}")
            return 1
        priority = getattr(args, "priority", 1) or 1
        fb_id = uuid.uuid4().hex[:8]
        db.execute(
            """INSERT INTO route_fallbacks(id, profile, primary_model, fallback_model,
               condition, priority, enabled, created_at)
               VALUES(?,?,?,?,?,?,1,?)""",
            (fb_id, profile, primary, fallback, condition, priority, utc_now()),
        )
        db.commit()
        db.close()
        if getattr(args, "json", False):
            print(json.dumps({"id": fb_id, "profile": profile, "primary": primary, "fallback": fallback}))
        else:
            print(f"Fallback added: {fb_id}")
            print(f"  {primary} → {fallback}  on: {condition}  priority: {priority}")
        return 0

    if sub == "list":
        rows = db.execute(
            """SELECT id, primary_model, fallback_model, condition, priority, enabled
               FROM route_fallbacks WHERE profile=? ORDER BY priority, created_at""",
            (profile,),
        ).fetchall()
        db.close()
        if getattr(args, "json", False):
            print(json.dumps([{
                "id": r[0], "primary": r[1], "fallback": r[2],
                "condition": r[3], "priority": r[4], "enabled": bool(r[5]),
            } for r in rows], indent=2))
            return 0
        if not rows:
            print(f"No fallback chains for profile '{profile}'.")
            return 0
        print(f"  {'ID':<10} {'PRIMARY':<36} {'FALLBACK':<36} {'CONDITION':<16} {'PRI':>4} {'EN':>4}")
        print("  " + "─" * 110)
        for r in rows:
            en = "✓" if r[5] else "✗"
            print(f"  {r[0]:<10} {r[1]:<36} {r[2]:<36} {r[3]:<16} {r[4]:>4} {en:>4}")
        return 0

    if sub == "remove":
        fb_id = getattr(args, "fb_id", None)
        if not fb_id:
            db.close()
            print_error("FALLBACK_ID required")
            return 1
        cur = db.execute("DELETE FROM route_fallbacks WHERE id=? AND profile=?", (fb_id, profile))
        db.commit()
        db.close()
        if cur.rowcount == 0:
            print_error(f"Fallback '{fb_id}' not found for profile '{profile}'")
            return 1
        print(f"removed: {fb_id}")
        return 0

    if sub == "resolve":
        primary = (getattr(args, "primary", "") or "").strip()
        condition = getattr(args, "condition", "context_overflow") or "context_overflow"
        if not primary:
            db.close()
            print_error("--primary required")
            return 1
        row = db.execute(
            """SELECT fallback_model FROM route_fallbacks
               WHERE profile=? AND primary_model=? AND condition=? AND enabled=1
               ORDER BY priority LIMIT 1""",
            (profile, primary, condition),
        ).fetchone()
        db.close()
        if not row:
            print(f"No fallback configured for {primary!r} on condition={condition!r}")
            return 1
        if getattr(args, "json", False):
            print(json.dumps({"primary": primary, "fallback": row[0], "condition": condition}))
        else:
            print(f"Fallback: {primary} → {row[0]}  (condition: {condition})")
        return 0

    db.close()
    return 0


# ---------------------------------------------------------------------------
# PRD-032: Agent Replay / Time-Travel Debugging (extends tag trace)
# ---------------------------------------------------------------------------

def _snapshot_trace(conn: sqlite3.Connection, trace_id: str) -> None:
    """Capture a full snapshot of the trace into trace_snapshots."""
    rows = conn.execute(
        """SELECT id, name, profile, model_id, started_at, finished_at,
               prompt_tokens, completion_tokens, status, attributes, error_msg
           FROM spans WHERE trace_id=? ORDER BY started_at""",
        (trace_id,),
    ).fetchall()
    if not rows:
        return

    import datetime
    now = datetime.datetime.now(datetime.timezone.utc).isoformat()
    snap_id = uuid.uuid4().hex[:16]
    snapshot = {
        "trace_id": trace_id,
        "captured_at": now,
        "spans": [
            {
                "id": r[0], "name": r[1], "profile": r[2], "model_id": r[3],
                "started_at": r[4], "finished_at": r[5],
                "prompt_tokens": r[6], "completion_tokens": r[7],
                "status": r[8],
                "attributes": json.loads(r[9] or "{}"),
                "error_msg": r[10],
            }
            for r in rows
        ],
    }
    conn.execute(
        """INSERT OR REPLACE INTO trace_snapshots(id, trace_id, step_index, snapshot_json, created_at)
           VALUES(?,?,0,?,?)""",
        (snap_id, trace_id, json.dumps(snapshot), now),
    )
    conn.commit()


def cmd_trace_extended(args: argparse.Namespace) -> int:
    """PRD-032: Extended trace commands including replay, diff, and snapshot."""
    cfg = load_config(config_path(getattr(args, "config", None)))
    db_path = runtime_db_path(cfg)
    if not db_path.exists():
        print("No spans database found.")
        return 0

    conn = sqlite3.connect(str(db_path))
    try:
        sub = getattr(args, "trace_subcommand", None)

        if sub == "snapshot":
            trace_id = getattr(args, "trace_id", None)
            if not trace_id:
                print_error("TRACE_ID required")
                return 1
            _snapshot_trace(conn, trace_id)
            print(f"Snapshot captured for trace: {trace_id}")
            return 0

        if sub == "replay":
            trace_id = getattr(args, "trace_id", None)
            if not trace_id:
                print_error("TRACE_ID required")
                return 1
            row = conn.execute(
                "SELECT snapshot_json FROM trace_snapshots WHERE trace_id=? ORDER BY created_at DESC LIMIT 1",
                (trace_id,),
            ).fetchone()
            if not row:
                # Try to build snapshot from live spans
                _snapshot_trace(conn, trace_id)
                row = conn.execute(
                    "SELECT snapshot_json FROM trace_snapshots WHERE trace_id=? ORDER BY created_at DESC LIMIT 1",
                    (trace_id,),
                ).fetchone()
            if not row:
                print_error(f"No snapshot found for trace {trace_id}")
                return 1

            snap = json.loads(row[0])
            spans = snap.get("spans", [])
            if getattr(args, "json", False):
                print(json.dumps(snap, indent=2))
                return 0

            print(f"Trace replay: {trace_id}")
            print(f"Captured: {snap.get('captured_at', '?')}")
            print(f"Spans: {len(spans)}")
            print()
            for i, s in enumerate(spans, 1):
                status = s.get("status", "?")
                dur = ""
                if s.get("started_at") and s.get("finished_at"):
                    try:
                        from datetime import datetime as _dt
                        start = _dt.fromisoformat(s["started_at"])
                        end = _dt.fromisoformat(s["finished_at"])
                        ms = int((end - start).total_seconds() * 1000)
                        dur = f"  {ms}ms"
                    except Exception:
                        pass
                pt = s.get("prompt_tokens", 0) or 0
                ct = s.get("completion_tokens", 0) or 0
                print(f"  [{i:02d}] {s['name']:<40} {status:<8} {pt+ct:>8} tokens{dur}")
                if s.get("error_msg"):
                    print(f"       error: {s['error_msg'][:80]}")
            return 0

        if sub == "diff":
            trace_a = getattr(args, "trace_a", None)
            trace_b = getattr(args, "trace_b", None)
            if not trace_a or not trace_b:
                print_error("Two trace IDs required: TRACE_A TRACE_B")
                return 1

            def _load_snap(tid):
                r = conn.execute(
                    "SELECT snapshot_json FROM trace_snapshots WHERE trace_id=? ORDER BY created_at DESC LIMIT 1",
                    (tid,),
                ).fetchone()
                if not r:
                    _snapshot_trace(conn, tid)
                    r = conn.execute(
                        "SELECT snapshot_json FROM trace_snapshots WHERE trace_id=? ORDER BY created_at DESC LIMIT 1",
                        (tid,),
                    ).fetchone()
                return json.loads(r[0]) if r else None

            snap_a = _load_snap(trace_a)
            snap_b = _load_snap(trace_b)
            if not snap_a:
                print_error(f"No snapshot for trace {trace_a}")
                return 1
            if not snap_b:
                print_error(f"No snapshot for trace {trace_b}")
                return 1

            spans_a = {s["name"]: s for s in snap_a.get("spans", [])}
            spans_b = {s["name"]: s for s in snap_b.get("spans", [])}
            all_names = sorted(set(spans_a) | set(spans_b))

            if getattr(args, "json", False):
                diff = []
                for name in all_names:
                    sa = spans_a.get(name)
                    sb = spans_b.get(name)
                    diff.append({"name": name, "a": sa, "b": sb})
                print(json.dumps(diff, indent=2))
                return 0

            print(f"Trace diff: {trace_a[:12]}  vs  {trace_b[:12]}")
            print(f"{'Span':<40} {'A tokens':>10} {'B tokens':>10} {'Δ tokens':>10} {'A status':<10} {'B status'}")
            print("-" * 100)
            for name in all_names:
                sa = spans_a.get(name)
                sb = spans_b.get(name)
                ta = ((sa or {}).get("prompt_tokens", 0) or 0) + ((sa or {}).get("completion_tokens", 0) or 0)
                tb = ((sb or {}).get("prompt_tokens", 0) or 0) + ((sb or {}).get("completion_tokens", 0) or 0)
                delta = tb - ta
                delta_str = f"+{delta}" if delta > 0 else str(delta)
                sta = (sa or {}).get("status", "—")
                stb = (sb or {}).get("status", "—")
                prefix = "+" if sa is None else ("-" if sb is None else " ")
                print(f"{prefix} {name:<38} {ta:>10} {tb:>10} {delta_str:>10} {sta:<10} {stb}")
            return 0

        if sub == "checkpoint":
            # snapshot sub-alias
            trace_id = getattr(args, "trace_id", None)
            if not trace_id:
                print_error("TRACE_ID required")
                return 1
            _snapshot_trace(conn, trace_id)
            snaps = conn.execute(
                "SELECT id, created_at FROM trace_snapshots WHERE trace_id=? ORDER BY created_at DESC",
                (trace_id,),
            ).fetchall()
            if getattr(args, "json", False):
                print(json.dumps([{"id": r[0], "created_at": r[1]} for r in snaps], indent=2))
            else:
                print(f"Checkpoints for trace {trace_id}:")
                for i, r in enumerate(snaps):
                    print(f"  [{i}] {r[0]}  {r[1]}")
            return 0

    finally:
        conn.close()

    return 0


# ---------------------------------------------------------------------------
# PRD-033: Dependency-Aware Task Queue
# ---------------------------------------------------------------------------

def cmd_dag(args: argparse.Namespace) -> int:
    """PRD-033: tag queue dag show/save/run/list."""
    from tag.dag import (
        ensure_schema as dag_ensure, show_dag, save_dag, run_dag,
        list_dags, DagSpec,
    )
    cfg = load_config(config_path(getattr(args, "config", None)))
    db = open_db(cfg)
    dag_ensure(db)
    sub = getattr(args, "dag_subcommand", None)

    if sub == "show" or sub is None:
        job_ids = getattr(args, "job_ids", None) or []
        from tag.dag import list_jobs_raw
        if getattr(args, "json", False):
            rows = list_jobs_raw(db, job_ids if job_ids else None)
            db.close()
            print(json.dumps(rows, indent=2))
            return 0
        print(show_dag(db, job_ids if job_ids else None))
        db.close()
        return 0

    if sub == "save":
        name = getattr(args, "name", "")
        steps_json = getattr(args, "steps", "[]")
        try:
            steps = json.loads(steps_json)
        except json.JSONDecodeError as exc:
            print_error(f"Invalid steps JSON: {exc}")
            db.close()
            return 1
        spec = DagSpec(name=name, steps=steps)
        dag_id = save_dag(db, spec)
        db.close()
        print(f"DAG saved: {name} ({dag_id})")
        return 0

    if sub == "run":
        name = getattr(args, "name", "")
        board = getattr(args, "board", "default")
        try:
            job_ids = run_dag(db, name, board=board)
        except ValueError as exc:
            print_error(str(exc))
            db.close()
            return 1
        db.close()
        print(f"DAG '{name}' submitted: {len(job_ids)} jobs")
        for jid in job_ids:
            print(f"  {jid}")
        return 0

    if sub == "list":
        dags = list_dags(db)
        db.close()
        if getattr(args, "json", False):
            print(json.dumps(dags, indent=2))
            return 0
        if not dags:
            print("No saved DAGs.")
            return 0
        for d in dags:
            print(f"{d['id'][:8]}  {d['name']:<30}  {d['step_count']} steps  {d['created_at'][:19]}")
        return 0

    db.close()
    print_error(f"Unknown dag subcommand: {sub!r}")
    return 1


def cmd_queue_extended(args: argparse.Namespace) -> int:
    """PRD-033: extended queue subcommands (depends-on support)."""
    from tag.dag import ensure_schema as dag_ensure, add_job, promote_ready_jobs
    cfg = load_config(config_path(getattr(args, "config", None)))
    db = open_db(cfg)
    dag_ensure(db)

    sub = getattr(args, "queue_ext_subcommand", None)

    if sub == "add":
        task = getattr(args, "task", "")
        profile = getattr(args, "profile", None)
        depends_on = getattr(args, "depends_on", []) or []
        if not task.strip():
            print_error("Task must not be empty.")
            db.close()
            return 1
        try:
            job_id = add_job(db, task, profile=profile, depends_on=depends_on)
        except ValueError as exc:
            print_error(str(exc))
            db.close()
            return 1
        db.close()
        if getattr(args, "json", False):
            print(json.dumps({"job_id": job_id, "status": "added", "depends_on": depends_on}))
        else:
            print(f"Queue job added: {job_id}")
        return 0

    if sub == "promote":
        promoted = promote_ready_jobs(db)
        db.close()
        if promoted:
            print(f"Promoted {len(promoted)} jobs to ready: {', '.join(promoted)}")
        else:
            print("No jobs promoted.")
        return 0

    db.close()
    return 0


# ---------------------------------------------------------------------------
# PRD-034: Secret Scanning
# ---------------------------------------------------------------------------

def cmd_security(args: argparse.Namespace) -> int:
    """PRD-034: tag security scan/list."""
    from tag.security import scan_directory, scan_file, record_scan, ensure_schema as sec_ensure
    cfg = load_config(config_path(getattr(args, "config", None)))
    db = open_db(cfg)
    sec_ensure(db)
    sub = getattr(args, "security_subcommand", None)

    if sub == "scan" or sub is None:
        path_str = getattr(args, "path", ".") or "."
        scan_path = Path(path_str).resolve()
        max_files = getattr(args, "max_files", 2000) or 2000

        if scan_path.is_file():
            from tag.security import scan_file as sf
            findings = sf(scan_path)
        else:
            from tag.security import scan_directory as sd
            findings = list(sd(scan_path, max_files=max_files))

        record_scan(db, str(scan_path), findings)
        db.close()

        if getattr(args, "json", False):
            print(json.dumps([
                {"file": str(f.file), "line_no": f.line_no, "pattern": f.pattern_name,
                 "entropy": f.is_entropy}
                for f in findings
            ], indent=2))
            return 1 if findings else 0

        if not findings:
            print(f"✓ No secrets found in {scan_path}")
            return 0

        print(f"⚠ Found {len(findings)} potential secret(s) in {scan_path}:\n")
        for f in findings:
            tag = "[entropy]" if f.is_entropy else f"[{f.pattern_name}]"
            print(f"  {f.file}:{f.line_no}  {tag}")
        print("\nNOTE: Matched values are NOT displayed for security.")
        return 1

    if sub == "list":
        rows = db.execute(
            "SELECT id, scanned_path, finding_count, status, created_at FROM security_scans "
            "ORDER BY created_at DESC LIMIT 20"
        ).fetchall()
        db.close()
        if getattr(args, "json", False):
            data = [{"id": r[0], "path": r[1], "findings": r[2],
                     "status": r[3], "created_at": r[4]} for r in rows]
            print(json.dumps(data, indent=2))
            return 0
        if not rows:
            print("No security scans recorded.")
            return 0
        for r in rows:
            status_icon = "✓" if r[3] == "clean" else "⚠"
            print(f"{status_icon} {r[0][:8]}  {r[1][:60]:<60}  {r[2]} findings  {r[4][:19]}")
        return 0

    db.close()
    print_error(f"Unknown security subcommand: {sub!r}")
    return 1


# ---------------------------------------------------------------------------
# PRD-035: IDE Bridge (LSP)
# ---------------------------------------------------------------------------

def cmd_lsp(args: argparse.Namespace) -> int:
    """PRD-035: tag lsp [--port PORT] [--stdio] [status]."""
    from tag.lsp_server import TagLspServer, get_lsp_status, ensure_schema as lsp_ensure
    cfg = load_config(config_path(getattr(args, "config", None)))
    db = open_db(cfg)
    lsp_ensure(db)
    sub = getattr(args, "lsp_subcommand", None)

    if sub == "status" or sub is None:
        sessions = get_lsp_status(db)
        db.close()
        if getattr(args, "json", False):
            print(json.dumps(sessions, indent=2))
            return 0
        if not sessions:
            print("No active LSP sessions.")
            return 0
        for s in sessions:
            tp = s["transport"]
            port_suffix = f":{s['port']}" if s.get("port") else ""
            print(f"{s['id'][:8]}  {tp}{port_suffix}  pid={s['pid']}  {s['created_at'][:19]}")
        return 0

    if sub == "start":
        # Collect profile names
        profiles_dir = tag_home() / "profiles"
        profiles: list[str] = []
        if profiles_dir.exists():
            profiles = [p.name for p in profiles_dir.iterdir() if p.is_dir()]
        if not profiles:
            profiles = ["orchestrator", "coder", "reviewer"]

        server_port = getattr(args, "port", 7878)
        use_stdio = getattr(args, "stdio", False)
        server = TagLspServer(profiles=profiles, conn=db)

        if use_stdio or server_port == 0:
            print("TAG LSP server starting on stdio ...", file=sys.stderr)
            server.run_stdio()
        else:
            server.run_tcp(host="127.0.0.1", port=server_port)

    db.close()
    return 0


# ---------------------------------------------------------------------------
# PRD-036: Web Dashboard
# ---------------------------------------------------------------------------

def cmd_web(args: argparse.Namespace) -> int:
    """PRD-036: tag web [--port 8787] [--host 127.0.0.1] [--no-browser]."""
    from tag.api import DashboardServer
    cfg = load_config(config_path(getattr(args, "config", None)))
    db_path = runtime_db_path(cfg)
    if not db_path.exists():
        # Ensure DB exists with base schema
        db = open_db(cfg)
        db.close()

    host = getattr(args, "host", "127.0.0.1") or "127.0.0.1"
    port = getattr(args, "port", 8787) or 8787
    no_browser = getattr(args, "no_browser", False)

    if host != "127.0.0.1":
        print(f"⚠ WARNING: Binding to {host} — dashboard will be accessible on your network.", file=sys.stderr)

    server = DashboardServer(db_path=db_path, host=host, port=port)
    server.start(open_browser=not no_browser)
    return 0


# ---------------------------------------------------------------------------
# PRD-037: Agent Personas
# ---------------------------------------------------------------------------

def cmd_persona(args: argparse.Namespace) -> int:
    """PRD-037: tag persona list/show/apply/remove/stack."""
    from tag.persona import (
        list_personas, get_persona, apply_persona, remove_active_persona,
        get_active_personas, remove_persona, install_persona, load_persona_file,
        ensure_schema as persona_ensure, build_merged_prompt,
    )
    cfg = load_config(config_path(getattr(args, "config", None)))
    db = open_db(cfg)
    persona_ensure(db)
    sub = getattr(args, "persona_subcommand", None)

    if sub == "list" or sub is None:
        personas = list_personas(db)
        db.close()
        if getattr(args, "json", False):
            print(json.dumps(personas, indent=2))
            return 0
        if not personas:
            print("No personas available.")
            return 0
        for p in personas:
            print(f"{'[builtin]' if p['source'] == 'builtin' else '[user]   ':10} {p['name']:<30}  {p['description'][:50]}")
        return 0

    if sub == "show":
        name = getattr(args, "name", "")
        p = get_persona(db, name)
        db.close()
        if not p:
            print_error(f"Persona not found: {name!r}")
            return 1
        if getattr(args, "json", False):
            print(json.dumps(p, indent=2))
            return 0
        print(f"Name:        {p['name']}")
        print(f"Description: {p['description']}")
        print(f"Inject:      {p['inject']}")
        print(f"Tags:        {', '.join(p.get('tags', []))}")
        print(f"Source:      {p['source']}")
        print(f"\nStyle Prompt:\n{p['style_prompt']}")
        return 0

    if sub == "apply":
        name = getattr(args, "name", "")
        profile = getattr(args, "profile", None) or cfg["defaults"]["master_profile"]
        session_id = getattr(args, "session_id", None)
        try:
            apply_persona(db, profile, name, session_id=session_id)
        except ValueError as exc:
            print_error(str(exc))
            db.close()
            return 1
        db.close()
        print(f"Persona '{name}' applied to profile '{profile}'.")
        return 0

    if sub == "remove":
        name = getattr(args, "name", "")
        profile = getattr(args, "profile", None) or cfg["defaults"]["master_profile"]
        removed = remove_active_persona(db, profile, name)
        db.close()
        if removed:
            print(f"Persona '{name}' removed from profile '{profile}'.")
        else:
            print(f"Persona '{name}' was not active on profile '{profile}'.")
        return 0

    if sub == "stack":
        profile = getattr(args, "profile", None) or cfg["defaults"]["master_profile"]
        personas = get_active_personas(db, profile)
        db.close()
        if not personas:
            print(f"No active personas for profile '{profile}'.")
            return 0
        print(f"Active personas for '{profile}':")
        for p in personas:
            print(f"  [{p.get('position', 0)}] {p['name']} ({p['inject']})")
        return 0

    if sub == "install":
        path_str = getattr(args, "file", "")
        try:
            persona_data = load_persona_file(Path(path_str))
            pid = install_persona(db, persona_data, source="user")
        except (FileNotFoundError, ValueError) as exc:
            print_error(str(exc))
            db.close()
            return 1
        db.close()
        print(f"Persona '{persona_data['name']}' installed ({pid[:8]}).")
        return 0

    if sub == "preview":
        profile = getattr(args, "profile", None) or cfg["defaults"]["master_profile"]
        base_prompt = getattr(args, "base_prompt", "You are a helpful agent.")
        personas = get_active_personas(db, profile)
        db.close()
        merged = build_merged_prompt(base_prompt, personas)
        print(merged)
        return 0

    db.close()
    print_error(f"Unknown persona subcommand: {sub!r}")
    return 1


# ---------------------------------------------------------------------------
# PRD-038: Diff-Aware Context Injection
# ---------------------------------------------------------------------------

def cmd_diff_inject(args: argparse.Namespace) -> int:
    """PRD-038: tag context inject --git-diff / --pr / --staged."""
    from tag.diff_context import build_diff_context, pr_diff_context
    cfg = load_config(config_path(getattr(args, "config", None)))

    pr_num = getattr(args, "pr", None)
    ref = getattr(args, "ref", "HEAD") or "HEAD"
    staged = getattr(args, "staged", False)
    context_lines = getattr(args, "context_lines", 3) or 3
    max_files = getattr(args, "max_files", 10) or 10
    blocked = getattr(args, "blocked", []) or []
    output_only = getattr(args, "output_only", False)

    try:
        if pr_num:
            repo = getattr(args, "repo", None)
            result = pr_diff_context(
                pr_num, repo, context_lines=context_lines,
                max_files=max_files, blocked_patterns=blocked,
            )
        else:
            workdir = Path(getattr(args, "workdir", ".") or ".").resolve()
            result = build_diff_context(
                ref, staged=staged, context_lines=context_lines,
                max_files=max_files, blocked_patterns=blocked, workdir=workdir,
            )
    except RuntimeError as exc:
        if getattr(args, "json", False):
            print(json.dumps({"error": str(exc), "files": [], "content": "", "estimated_tokens": 0}))
        else:
            print_error(str(exc))
        return 1

    if result["warn"]:
        print(f"⚠ Warning: diff context is large ({result['estimated_tokens']:,} estimated tokens).", file=sys.stderr)

    if result["files_skipped"]:
        print(f"Skipped {len(result['files_skipped'])} file(s): {', '.join(result['files_skipped'][:5])}", file=sys.stderr)

    if not result["content"].strip():
        if getattr(args, "json", False):
            print(json.dumps({"files": [], "content": "", "estimated_tokens": 0, "warn": False, "files_included": [], "files_skipped": []}))
        else:
            print("No diff content to inject (no changed files in scope).")
        return 0

    _json = getattr(args, "json", False)
    print(f"Diff context: {len(result['files_included'])} file(s), ~{result['estimated_tokens']:,} tokens",
          file=sys.stderr if _json else sys.stdout)

    if output_only or _json:
        if _json:
            print(json.dumps(result, indent=2))
        else:
            print(result["content"])
        return 0

    # Store in context (writes to a context file that tag submit picks up)
    context_dir = runtime_db_path(cfg).parent / "context"
    context_dir.mkdir(parents=True, exist_ok=True)
    ctx_file = context_dir / "diff_context.md"
    ctx_file.write_text(result["content"])
    print(f"Diff context saved to {ctx_file}")
    return 0


# ---------------------------------------------------------------------------
# PRD-039: Token Budget Enforcement
# ---------------------------------------------------------------------------

def cmd_budget(args: argparse.Namespace) -> int:
    """PRD-039: tag budget set/get/list/remove/check."""
    from tag.budget import (
        set_budget, get_budget, list_budgets, remove_budget, check_budget,
        BudgetExceeded, ensure_schema as budget_ensure,
    )
    cfg = load_config(config_path(getattr(args, "config", None)))
    db = open_db(cfg)
    budget_ensure(db)
    sub = getattr(args, "budget_subcommand", None)

    if sub == "set":
        profile = getattr(args, "profile", None) or cfg["defaults"]["master_profile"]
        max_tokens = getattr(args, "max_tokens", 0)
        period = getattr(args, "period", "daily") or "daily"
        warn_pct = getattr(args, "warn_pct", 0.8)
        try:
            bid = set_budget(db, profile, max_tokens, period=period, warn_pct=warn_pct)
        except ValueError as exc:
            print_error(str(exc))
            db.close()
            return 1
        db.close()
        print(f"Budget set for '{profile}': {max_tokens:,} tokens/{period} (warn at {int(warn_pct*100)}%)")
        return 0

    if sub == "get":
        profile = getattr(args, "profile", None) or cfg["defaults"]["master_profile"]
        budget = get_budget(db, profile)
        db.close()
        if not budget:
            print(f"No budget set for profile '{profile}'.")
            return 0
        if getattr(args, "json", False):
            print(json.dumps(budget, indent=2))
        else:
            print(f"Profile:    {profile}")
            print(f"Period:     {budget['period']}")
            print(f"Max tokens: {budget['max_tokens']:,}")
            print(f"Warn at:    {int(budget['warn_pct']*100)}%")
            print(f"Enabled:    {budget['enabled']}")
        return 0

    if sub == "list":
        budgets = list_budgets(db)
        db.close()
        if getattr(args, "json", False):
            print(json.dumps(budgets, indent=2))
            return 0
        if not budgets:
            print("No token budgets configured.")
            return 0
        for b in budgets:
            status = "✓" if b["enabled"] else "✗"
            print(f"{status} {b['profile']:<30}  {b['max_tokens']:>10,} tokens/{b['period']}")
        return 0

    if sub == "remove":
        profile = getattr(args, "profile", None) or cfg["defaults"]["master_profile"]
        removed = remove_budget(db, profile)
        db.close()
        if removed:
            print(f"Budget removed for '{profile}'.")
        else:
            print(f"No budget found for '{profile}'.")
        return 0

    if sub == "check":
        profile = getattr(args, "profile", None) or cfg["defaults"]["master_profile"]
        try:
            result = check_budget(db, profile)
        except BudgetExceeded as exc:
            db.close()
            print_error(str(exc))
            return 1
        db.close()
        if result.get("budget") is None:
            print(f"No budget configured for '{profile}' — unlimited.")
            return 0
        if getattr(args, "json", False):
            print(json.dumps(result, indent=2))
        else:
            used = result.get("used", 0)
            limit = result.get("limit", 0)
            pct = result.get("pct", 0.0)
            warn = result.get("warn", False)
            warn_icon = "⚠" if warn else "✓"
            print(f"{warn_icon} {profile}: {used:,}/{limit:,} tokens ({pct}%) [{result.get('period')}]")
        return 0

    # Default: list
    budgets = list_budgets(db)
    db.close()
    if getattr(args, "json", False):
        print(json.dumps(budgets, indent=2))
        return 0
    if not budgets:
        print("No token budgets configured. Use 'tag budget set' to add one.")
        return 0
    for b in budgets:
        status = "✓" if b["enabled"] else "✗"
        print(f"{status} {b['profile']:<30}  {b['max_tokens']:>10,} tokens/{b['period']}")
    return 0


# ---------------------------------------------------------------------------
# PRD-040: Notification Hooks
# ---------------------------------------------------------------------------

def cmd_notify(args: argparse.Namespace) -> int:
    """PRD-040: tag notify add/list/test/remove/enable/disable."""
    from tag.notifications import (
        add_hook, list_hooks, remove_hook, set_hook_enabled, deliver,
        ensure_schema as notif_ensure, VALID_CHANNELS, VALID_EVENTS,
    )
    cfg = load_config(config_path(getattr(args, "config", None)))
    db = open_db(cfg)
    notif_ensure(db)
    sub = getattr(args, "notify_subcommand", None)

    if sub == "add":
        event = getattr(args, "event", "run.completed") or "run.completed"
        channel = getattr(args, "channel", "desktop") or "desktop"
        profile = getattr(args, "profile", None)
        config_str = getattr(args, "config_json", "{}") or "{}"
        template = getattr(args, "template", "") or ""
        try:
            config_data = json.loads(config_str)
        except json.JSONDecodeError as exc:
            print_error(f"Invalid config JSON: {exc}")
            db.close()
            return 1
        try:
            hook_id = add_hook(db, event, channel, config_data, profile=profile, template=template)
        except ValueError as exc:
            print_error(str(exc))
            db.close()
            return 1
        db.close()
        if getattr(args, "json", False):
            print(json.dumps({"id": hook_id, "channel": channel, "event": event}))
        else:
            print(f"Notification hook added: {hook_id}  ({channel} on {event})")
        return 0

    if sub == "list":
        profile = getattr(args, "profile", None)
        hooks = list_hooks(db, profile=profile)
        db.close()
        if getattr(args, "json", False):
            print(json.dumps(hooks, indent=2))
            return 0
        if not hooks:
            print("No notification hooks configured.")
            return 0
        for h in hooks:
            status = "✓" if h["enabled"] else "✗"
            print(f"{status} {h['id'][:8]}  {h['channel']:<10} {h['event']:<20} profile={h['profile'] or '*'}")
        return 0

    if sub == "test":
        hook_id = getattr(args, "hook_id", "")
        hooks = list_hooks(db)
        hook = next((h for h in hooks if h["id"].startswith(hook_id)), None)
        db.close()
        if not hook:
            print_error(f"Hook not found: {hook_id!r}")
            return 1
        ctx = {
            "run_id": "test-run-001", "profile": "test", "duration": "0s",
            "tokens_used": "0", "cost_usd": "0.00", "status": "completed",
            "error_message": "", "task": "Test notification", "event": "test",
        }
        ok, err = deliver(hook, "test", ctx)
        if ok:
            print(f"✓ Test notification sent via {hook['channel']}.")
        else:
            print_error(f"Delivery failed: {err}")
        return 0 if ok else 1

    if sub == "remove":
        hook_id = getattr(args, "hook_id", "")
        removed = remove_hook(db, hook_id)
        db.close()
        if removed:
            print(f"Hook {hook_id} removed.")
        else:
            print_error(f"Hook not found: {hook_id}")
            return 1
        return 0

    if sub == "enable":
        hook_id = getattr(args, "hook_id", "")
        set_hook_enabled(db, hook_id, True)
        db.close()
        print(f"Hook {hook_id} enabled.")
        return 0

    if sub == "disable":
        hook_id = getattr(args, "hook_id", "")
        set_hook_enabled(db, hook_id, False)
        db.close()
        print(f"Hook {hook_id} disabled.")
        return 0

    db.close()
    print_error(f"Unknown notify subcommand: {sub!r}")
    return 1


# ---------------------------------------------------------------------------
# PRD-041: OTel GenAI Span Cost Attribution
# ---------------------------------------------------------------------------

def cmd_otel_export(args: argparse.Namespace) -> int:
    """PRD-041: tag trace export --otlp-endpoint ... --semconv."""
    from tag.otel_semconv import spans_to_otlp_json, SEMCONV_VERSION
    cfg = load_config(config_path(getattr(args, "config", None)))
    db = open_db(cfg)
    trace_id = getattr(args, "trace_id", None)
    endpoint = getattr(args, "endpoint", "") or ""
    include_metrics = not getattr(args, "no_metrics", False)
    semconv = getattr(args, "semconv", SEMCONV_VERSION) or SEMCONV_VERSION

    # Fetch spans
    if trace_id:
        rows = db.execute(
            "SELECT id, trace_id, parent_id, name, profile, model_id, started_at, "
            "finished_at, duration_ms, status, prompt_tokens, completion_tokens, attributes "
            "FROM spans WHERE trace_id=? ORDER BY started_at",
            (trace_id,),
        ).fetchall()
    else:
        rows = db.execute(
            "SELECT id, trace_id, parent_id, name, profile, model_id, started_at, "
            "finished_at, duration_ms, status, prompt_tokens, completion_tokens, attributes "
            "FROM spans ORDER BY started_at DESC LIMIT 100",
        ).fetchall()

    db.close()

    span_dicts = [
        {
            "id": r[0], "trace_id": r[1], "parent_id": r[2], "name": r[3],
            "profile": r[4], "model_id": r[5], "started_at": r[6],
            "finished_at": r[7], "duration_ms": r[8], "status": r[9],
            "prompt_tokens": r[10], "completion_tokens": r[11],
        }
        for r in rows
    ]

    payload = spans_to_otlp_json(span_dicts, include_metrics=include_metrics)

    if not endpoint:
        print(json.dumps(payload, indent=2))
        return 0

    # POST to OTLP endpoint
    import urllib.request, urllib.error
    body = json.dumps(payload).encode()
    req = urllib.request.Request(
        endpoint.rstrip("/") + "/v1/traces",
        data=body,
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            print(f"✓ Exported {len(span_dicts)} spans to {endpoint} (HTTP {resp.status})")
            print(f"  OTel GenAI semconv version: {semconv}")
        if include_metrics and any(s.get("prompt_tokens") for s in span_dicts):
            metrics_body = json.dumps({"resourceMetrics": payload.get("resourceMetrics", [])}).encode()
            metrics_req = urllib.request.Request(
                endpoint.rstrip("/") + "/v1/metrics",
                data=metrics_body,
                headers={"Content-Type": "application/json"},
                method="POST",
            )
            with urllib.request.urlopen(metrics_req, timeout=30) as resp:
                print(f"✓ Exported token usage metrics (HTTP {resp.status})")
    except urllib.error.URLError as exc:
        print_error(f"OTLP export failed: {exc}")
        return 1
    return 0


# ---------------------------------------------------------------------------
# PRD-042: Architect/Editor Agent Split
# ---------------------------------------------------------------------------

def cmd_split(args: argparse.Namespace) -> int:
    """PRD-042: tag split list/show/plan."""
    from tag.split_agent import (
        create_split_run, get_split_run, list_split_runs,
        save_spec, ChangeSpec, ARCHITECT_SYSTEM, EDITOR_SYSTEM,
        ensure_schema as split_ensure,
    )
    cfg = load_config(config_path(getattr(args, "config", None)))
    db = open_db(cfg)
    split_ensure(db)
    sub = getattr(args, "split_subcommand", None)

    if sub == "list" or sub is None:
        runs = list_split_runs(db)
        db.close()
        if getattr(args, "json", False):
            print(json.dumps(runs, indent=2))
            return 0
        if not runs:
            print("No architect/editor split runs.")
            return 0
        for r in runs:
            print(f"{r['id'][:12]}  {r['status']:<12}  {r['architect_model'][:20]} → {r['editor_model'][:20]}  {r['task'][:50]}")
        return 0

    if sub == "show":
        run_id = getattr(args, "run_id", "")
        run = get_split_run(db, run_id)
        db.close()
        if not run:
            print_error(f"Split run not found: {run_id!r}")
            return 1
        if getattr(args, "json", False):
            print(json.dumps(run, indent=2))
        else:
            print(f"Run:         {run['id']}")
            print(f"Task:        {run['task']}")
            print(f"Architect:   {run['architect_model']}")
            print(f"Editor:      {run['editor_model']}")
            print(f"Status:      {run['status']}")
            print(f"Items:       {run['items_done']}/{run['items_total']} done, {run['items_rejected']} rejected")
            if run.get("items"):
                print("\nItems:")
                for item in run["items"]:
                    icon = {"accepted": "✓", "rejected": "✗", "pending": "○"}.get(item["status"], "?")
                    print(f"  {icon} [{item['action']:8}] {item['file']:40}  {item['description'][:50]}")
        return 0

    if sub == "plan":
        task = (getattr(args, "task", "") or "").strip()
        if not task:
            db.close()
            print_error("task must not be empty")
            return 1
        architect = getattr(args, "architect", "claude-opus-4") or "claude-opus-4"
        editor = getattr(args, "editor", "claude-haiku-4-5") or "claude-haiku-4-5"
        profile = getattr(args, "profile", None) or cfg["defaults"]["master_profile"]
        spec_json_str = getattr(args, "spec_json", None)

        run_id = create_split_run(db, task, architect, editor, profile)

        if spec_json_str:
            try:
                spec = ChangeSpec.from_json(spec_json_str)
                save_spec(db, run_id, spec)
                db.close()
                print(f"Split run created: {run_id}  ({len(spec.items)} items from spec)")
            except (json.JSONDecodeError, KeyError) as exc:
                print_error(f"Invalid spec JSON: {exc}")
                db.close()
                return 1
        else:
            db.close()
            print(f"Split run created: {run_id}")
            print(f"Architect: {architect}  Editor: {editor}")
            print(f"\nArchitect system prompt:\n{ARCHITECT_SYSTEM}")
        return 0

    db.close()
    print_error(f"Unknown split subcommand: {sub!r}")
    return 1


# ---------------------------------------------------------------------------
# PRD-043: Vector-Based Tool Retrieval
# ---------------------------------------------------------------------------

def cmd_tool_retrieval(args: argparse.Namespace) -> int:
    """PRD-043: tag mcp-registry index/search."""
    from tag.tool_retrieval import (
        build_index, search_tools, is_available, keyword_search_tools,
        get_index_stats, ensure_schema as tr_ensure,
    )
    cfg = load_config(config_path(getattr(args, "config", None)))
    db = open_db(cfg)
    tr_ensure(db)
    sub = getattr(args, "tr_subcommand", None)

    persist_dir = runtime_db_path(cfg).parent / "tool_index"
    cache_dir = tag_home() / ".cache" / "embeddings"

    if sub == "index" or sub is None:
        # Load tools from MCP registry YAML
        mcp_registry_path = tag_home() / "mcp-registry.yaml"
        tools: list[dict] = []
        if mcp_registry_path.exists():
            import yaml
            try:
                reg = yaml.safe_load(mcp_registry_path.read_text())
                for server_name, server_cfg in (reg or {}).items():
                    for tool in (server_cfg or {}).get("tools", []):
                        tools.append({
                            "name": tool.get("name", ""),
                            "description": tool.get("description", ""),
                            "server": server_name,
                        })
            except Exception as exc:
                print(f"Warning: could not parse MCP registry: {exc}", file=sys.stderr)

        if not is_available():
            print("⚠ chromadb and sentence-transformers not installed.", file=sys.stderr)
            print("  Install with: pip install chromadb sentence-transformers")
            print(f"  Found {len(tools)} tool(s) in MCP registry — index not built.")
            db.close()
            return 0

        count = build_index(tools, persist_dir, cache_dir, conn=db)
        db.close()
        print(f"✓ Tool index built: {count} tools indexed")
        return 0

    if sub == "search":
        query = getattr(args, "query", "")
        top_k = getattr(args, "top_k", 8) or 8
        if not query.strip():
            print_error("Query must not be empty.")
            db.close()
            return 1

        if is_available():
            results = search_tools(query, persist_dir, cache_dir, top_k=top_k)
        else:
            # Fallback: load from registry and do keyword search
            mcp_registry_path = tag_home() / "mcp-registry.yaml"
            all_tools: list[dict] = []
            if mcp_registry_path.exists():
                import yaml
                try:
                    reg = yaml.safe_load(mcp_registry_path.read_text())
                    for sname, scfg in (reg or {}).items():
                        for t in (scfg or {}).get("tools", []):
                            all_tools.append({"name": t.get("name",""), "description": t.get("description",""), "server": sname})
                except Exception:
                    pass
            results = keyword_search_tools(query, all_tools, top_k=top_k)

        db.close()

        stats = get_index_stats(db) if False else {}  # already closed
        if not results:
            print(f"No tools found for query: {query!r}")
            return 0

        if getattr(args, "json", False):
            print(json.dumps(results, indent=2))
        else:
            print(f"Top {len(results)} tools for: {query!r}\n")
            for i, t in enumerate(results, 1):
                print(f"  {i:2}. [{t.get('server','?'):20}] {t.get('name',''):<30}  {t.get('description','')[:60]}")
        return 0

    if sub == "status":
        stats = get_index_stats(db)
        db.close()
        if getattr(args, "json", False):
            print(json.dumps(stats, indent=2))
            return 0
        if not stats.get("built"):
            print("Tool index not built. Run: tag tool-index index")
            return 0
        print(f"Index status:  {stats['tool_count']} tools")
        print(f"Built at:      {stats.get('built_at', 'unknown')}")
        print(f"Backend:       {'chromadb + sentence-transformers' if stats.get('available') else 'keyword fallback'}")
        return 0

    db.close()
    print_error(f"Unknown tool-index subcommand: {sub!r}")
    return 1


# ---------------------------------------------------------------------------
# PRD-044: AgentOps Session Observability
# ---------------------------------------------------------------------------

def cmd_agentops(args: argparse.Namespace) -> int:
    """PRD-044: tag agentops sessions/show."""
    from tag.integrations.agentops_bridge import (
        is_available, is_configured, list_sessions, get_session_for_run,
        mask_key, ensure_schema as ao_ensure,
    )
    cfg = load_config(config_path(getattr(args, "config", None)))
    db = open_db(cfg)
    ao_ensure(db)
    sub = getattr(args, "agentops_subcommand", None)

    if sub == "status":
        sdk_ok = is_available()
        cfg_ok = is_configured(cfg)
        db.close()
        if getattr(args, "json", False):
            import os
            key = cfg.get("agentops", {}).get("api_key", "") or os.environ.get("AGENTOPS_API_KEY", "")
            print(json.dumps({
                "sdk_installed": sdk_ok,
                "api_key_configured": cfg_ok,
                "api_key_masked": mask_key(key) if cfg_ok else None,
            }, indent=2))
            return 0
        print(f"AgentOps SDK installed: {'✓' if sdk_ok else '✗'}")
        print(f"API key configured:     {'✓' if cfg_ok else '✗ (run: tag config set agentops.api_key <key>)'}")
        if cfg_ok:
            import os
            key = cfg.get("agentops", {}).get("api_key", "") or os.environ.get("AGENTOPS_API_KEY", "")
            print(f"API key:               {mask_key(key)}")
        return 0

    if sub == "sessions" or sub is None:
        limit = getattr(args, "limit", 20) or 20
        sessions = list_sessions(db, limit=limit)
        db.close()
        if getattr(args, "json", False):
            print(json.dumps(sessions, indent=2))
            return 0
        if not sessions:
            print("No AgentOps sessions recorded.")
            return 0
        for s in sessions:
            print(f"{s['run_id'][:12]}  {s['status']:<12}  {s['session_id'] or '(no session)'}  {s['created_at'][:19]}")
        return 0

    if sub == "show":
        run_id = getattr(args, "run_id", "")
        session = get_session_for_run(db, run_id)
        db.close()
        if not session:
            print_error(f"No AgentOps session for run: {run_id}")
            return 1
        if getattr(args, "json", False):
            print(json.dumps(session, indent=2))
        else:
            print(f"Session ID:    {session['session_id']}")
            print(f"Dashboard URL: {session['dashboard_url']}")
            print(f"Status:        {session['status']}")
            print(f"Created at:    {session['created_at']}")
        return 0

    db.close()
    print_error(f"Unknown agentops subcommand: {sub!r}")
    return 1


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description="TAG orchestration CLI")
    parser.add_argument("--config", help="Path to lab config YAML")
    parser.add_argument("--version", action="version", version=f"%(prog)s {__version__}")
    sub = parser.add_subparsers(dest="command")

    setup = sub.add_parser("setup", help="Provision the managed runtime, apply TAG patches, build the TUI, and bootstrap profiles")
    setup.add_argument("--refresh", action="store_true", help="Fetch and update an existing managed runtime checkout")
    setup.add_argument("--skip-python-install", action="store_true")
    setup.add_argument("--skip-tui-build", action="store_true")
    setup.add_argument("--json", action="store_true")
    setup.set_defaults(func=cmd_setup)

    doctor = sub.add_parser("doctor", help="Validate local TAG paths and the managed runtime")
    doctor.add_argument("--json", action="store_true")
    doctor.add_argument("--profile", metavar="NAME", help="Check only this profile")
    doctor.set_defaults(func=cmd_doctor)

    bootstrap = sub.add_parser("bootstrap", help="Create profiles and render config")
    bootstrap.add_argument("--force", action="store_true", help="Overwrite rendered files")
    bootstrap.add_argument("--json", action="store_true")
    bootstrap.set_defaults(func=cmd_bootstrap)

    render = sub.add_parser("render", help="Render per-profile config only")
    render.add_argument("--force", action="store_true", help="Overwrite rendered files")
    render.add_argument("--json", action="store_true")
    render.set_defaults(func=cmd_render)

    route = sub.add_parser("route", help="Resolve task routing from lab policy")
    route.add_argument("--task-type", required=True)
    route.add_argument("--master-profile")
    route.add_argument("--worker-profile", action="append", default=[])
    route.add_argument("--master-model", help="Override master as provider/model-id")
    route.add_argument("--verifier-model", help="Override verifier as provider/model-id")
    route.add_argument(
        "--worker-model-override",
        action="append",
        default=[],
        help="Override worker as profile=provider/model-id",
    )
    route.add_argument("--json", action="store_true")
    route.set_defaults(func=cmd_route)

    env_cmd = sub.add_parser("env", help="Print the isolated environment values")
    env_cmd.set_defaults(func=cmd_env)

    assignments = sub.add_parser(
        "assignments", help="Show the current default model assignment per profile"
    )
    assignments.add_argument("--json", action="store_true")
    assignments.set_defaults(func=cmd_assignments)

    models = sub.add_parser(
        "models", help="List curated provider/model options for a profile"
    )
    models.add_argument("--profile", required=True)
    models.add_argument("--provider", help="Filter to one provider slug")
    models.add_argument("--limit", type=nonnegative_int, default=10)
    models.add_argument("--json", action="store_true")
    models.set_defaults(func=cmd_models)

    set_model = sub.add_parser(
        "set-model", help="Persist a profile's primary or delegation model"
    )
    set_model.add_argument("--profile", required=True)
    set_model.add_argument("--ref", required=True, help="provider/model-id")
    set_model.add_argument(
        "--target",
        choices=("primary", "delegation"),
        default="primary",
    )
    set_model.add_argument(
        "--openai-runtime",
        help="Optional runtime override when setting a primary OpenAI/Codex model",
    )
    set_model.add_argument("--json", action="store_true")
    set_model.set_defaults(func=cmd_set_model)

    submit = sub.add_parser(
        "submit", help="Resolve a route and execute it directly or through Kanban"
    )
    submit.add_argument("--task-type", required=True)
    submit.add_argument("--prompt", required=True)
    submit.add_argument("--title")
    submit.add_argument("--source", default="manual")
    submit.add_argument(
        "--execution",
        choices=("auto", "direct", "kanban"),
        default="auto",
    )
    submit.add_argument("--master-profile")
    submit.add_argument("--worker-profile", action="append", default=[])
    submit.add_argument("--master-model")
    submit.add_argument("--verifier-model")
    submit.add_argument("--worker-model-override", action="append", default=[])
    submit.add_argument("--verify", action="store_true")
    submit.add_argument(
        "--wait-seconds",
        type=nonnegative_int,
        default=0,
        help="For Kanban submits, poll spawned tasks until completion or timeout",
    )
    submit.add_argument("--json", action="store_true")
    submit.set_defaults(func=cmd_submit)

    benchmark = sub.add_parser(
        "benchmark", help="Run a prompt-contract benchmark across one or more models"
    )
    benchmark.add_argument("--profile", required=True)
    benchmark.add_argument("--suite", help="Path to benchmark suite YAML")
    benchmark.add_argument("--model-ref", action="append", default=[])
    benchmark.add_argument("--case", action="append", default=[])
    benchmark.add_argument("--json", action="store_true")
    benchmark.set_defaults(func=cmd_benchmark)

    runs = sub.add_parser("runs", help="Show recent submit and benchmark runs")
    runs.add_argument("--limit", type=positive_int, default=20)
    runs.add_argument("--json", action="store_true")
    runs.set_defaults(func=cmd_runs)

    openrouter_models = sub.add_parser(
        "openrouter-models",
        help="Query the full OpenRouter model catalog for a profile's API key",
    )
    openrouter_models.add_argument("--profile", required=True)
    openrouter_models.add_argument("--search")
    openrouter_models.add_argument(
        "--sort",
        choices=("id", "prompt", "completion", "context"),
        default="id",
    )
    openrouter_models.add_argument("--limit", type=nonnegative_int, default=20)
    openrouter_models.add_argument("--ids-only", action="store_true")
    openrouter_models.add_argument("--json", action="store_true")
    openrouter_models.set_defaults(func=cmd_openrouter_models)

    import_codex = sub.add_parser(
        "import-codex",
        help="Import existing Codex CLI credentials into a TAG-managed profile",
    )
    import_codex.add_argument("--profile", required=True)
    import_codex.add_argument("--codex-home", help="Path to the source CODEX_HOME")
    import_codex.add_argument("--json", action="store_true")
    import_codex.set_defaults(func=cmd_import_codex)

    import_claude = sub.add_parser(
        "import-claude",
        help="Import Claude Code / Anthropic API credentials into a TAG-managed profile",
    )
    import_claude.add_argument("--profile", required=True)
    import_claude.add_argument(
        "--claude-home",
        help="Path to source ~/.claude directory (default: ~/.claude)",
    )
    import_claude.add_argument(
        "--use-oauth",
        action="store_true",
        help=(
            "Import the OAuth session token from `claude auth login`. "
            "Anthropic prohibits this in third-party tools; ANTHROPIC_API_KEY is preferred."
        ),
    )
    import_claude.add_argument("--json", action="store_true")
    import_claude.set_defaults(func=cmd_import_claude)

    import_gemini = sub.add_parser(
        "import-gemini",
        help="Import Gemini CLI / Google API credentials into a TAG-managed profile",
    )
    import_gemini.add_argument("--profile", required=True)
    import_gemini.add_argument(
        "--gemini-home",
        help="Path to source ~/.gemini directory (default: ~/.gemini)",
    )
    import_gemini.add_argument(
        "--use-oauth",
        action="store_true",
        help=(
            "Import OAuth tokens from ~/.gemini/oauth_creds.json. "
            "Google prohibits this in third-party tools; GEMINI_API_KEY is preferred."
        ),
    )
    import_gemini.add_argument("--json", action="store_true")
    import_gemini.set_defaults(func=cmd_import_gemini)

    import_continue = sub.add_parser(
        "import-continue",
        help="Import API keys from a Continue.dev config into a TAG-managed profile",
    )
    import_continue.add_argument("--profile", required=True)
    import_continue.add_argument(
        "--continue-home",
        help="Path to source ~/.continue directory (default: ~/.continue)",
    )
    import_continue.add_argument("--json", action="store_true")
    import_continue.set_defaults(func=cmd_import_continue)

    import_mistral = sub.add_parser(
        "import-mistral",
        help="Import Mistral API key from the Mistral Vibe CLI into a TAG-managed profile",
    )
    import_mistral.add_argument("--profile", required=True)
    import_mistral.add_argument(
        "--vibe-home",
        help="Path to source ~/.vibe directory (default: ~/.vibe)",
    )
    import_mistral.add_argument("--json", action="store_true")
    import_mistral.set_defaults(func=cmd_import_mistral)

    import_opencode = sub.add_parser(
        "import-opencode",
        help="Import API keys from opencode (~/.local/share/opencode/auth.json) into a TAG-managed profile",
    )
    import_opencode.add_argument("--profile", required=True)
    import_opencode.add_argument(
        "--opencode-data-dir",
        help="Path to opencode data dir (default: ~/.local/share/opencode)",
    )
    import_opencode.add_argument("--json", action="store_true")
    import_opencode.set_defaults(func=cmd_import_opencode)

    import_zed = sub.add_parser(
        "import-zed",
        help="Import API keys from Zed editor settings.json into a TAG-managed profile",
    )
    import_zed.add_argument("--profile", required=True)
    import_zed.add_argument(
        "--zed-config",
        help="Path to Zed settings.json (default: ~/.config/zed/settings.json)",
    )
    import_zed.add_argument("--json", action="store_true")
    import_zed.set_defaults(func=cmd_import_zed)

    import_copilot = sub.add_parser(
        "import-copilot",
        help="Import GitHub OAuth token from gh CLI into a TAG-managed profile",
    )
    import_copilot.add_argument("--profile", required=True)
    import_copilot.add_argument(
        "--gh-config",
        help="Path to gh CLI hosts.yml (default: ~/.config/gh/hosts.yml)",
    )
    import_copilot.add_argument("--json", action="store_true")
    import_copilot.set_defaults(func=cmd_import_copilot)

    import_aider = sub.add_parser(
        "import-aider",
        help="Import API keys from Aider config (~/.aider.conf.yml or ~/.env) into a TAG-managed profile",
    )
    import_aider.add_argument("--profile", required=True)
    import_aider.add_argument(
        "--aider-home",
        help="Base directory for Aider config files (default: ~)",
    )
    import_aider.add_argument("--json", action="store_true")
    import_aider.set_defaults(func=cmd_import_aider)

    import_aws = sub.add_parser(
        "import-aws",
        help="Import AWS credentials (~/.aws/credentials) for Amazon Bedrock / Q Developer into a TAG-managed profile",
    )
    import_aws.add_argument("--profile", required=True)
    import_aws.add_argument(
        "--aws-dir",
        help="Path to AWS config directory (default: ~/.aws)",
    )
    import_aws.add_argument("--json", action="store_true")
    import_aws.set_defaults(func=cmd_import_aws)

    import_cursor = sub.add_parser(
        "import-cursor",
        help="Import BYOK API keys from Cursor IDE's local SQLite store into a TAG-managed profile",
    )
    import_cursor.add_argument("--profile", required=True)
    import_cursor.add_argument(
        "--cursor-dir",
        help="Path to Cursor globalStorage directory containing state.vscdb",
    )
    import_cursor.add_argument("--json", action="store_true")
    import_cursor.set_defaults(func=cmd_import_cursor)

    hermes_cmd = sub.add_parser("hermes", help="Pass raw arguments through to the managed runtime binary")
    hermes_cmd.add_argument("--profile", help="Run the managed runtime inside one TAG profile home")
    hermes_cmd.add_argument("--version", dest="hermes_version", action="store_true", help="Show the managed runtime version")
    hermes_cmd.add_argument("hermes_args", nargs=argparse.REMAINDER)
    hermes_cmd.set_defaults(func=cmd_hermes_passthrough)

    chat = sub.add_parser("chat", help="Run chat inside a TAG profile")
    chat.add_argument("--profile", default="orchestrator", help="TAG profile to use")
    chat.add_argument("hermes_args", nargs=argparse.REMAINDER)
    chat.set_defaults(func=cmd_chat)

    gateway = sub.add_parser("gateway", help="Run gateway commands inside a TAG profile")
    gateway.add_argument("--profile", default="orchestrator", help="TAG profile to use")
    gateway.add_argument("hermes_args", nargs=argparse.REMAINDER)
    gateway.set_defaults(func=cmd_gateway)

    kanban = sub.add_parser("kanban", help="Run Kanban commands inside a TAG profile")
    kanban.add_argument("--profile", default="orchestrator", help="TAG profile to use")
    kanban.add_argument("hermes_args", nargs=argparse.REMAINDER)
    kanban.set_defaults(func=cmd_kanban)

    model = sub.add_parser("model", help="Run model commands inside a TAG profile")
    model.add_argument("--profile", default="orchestrator", help="TAG profile to use")
    model.add_argument("hermes_args", nargs=argparse.REMAINDER)
    model.set_defaults(func=cmd_model)

    profile = sub.add_parser("profile", help="Run profile commands in the managed TAG environment")
    profile.add_argument("--profile", help="Optional active profile home override")
    profile.add_argument("hermes_args", nargs=argparse.REMAINDER)
    profile.set_defaults(func=cmd_profile)

    status = sub.add_parser("status", help="Run status inside a TAG profile")
    status.add_argument("--profile", default="orchestrator", help="TAG profile to use")
    status.add_argument("hermes_args", nargs=argparse.REMAINDER)
    status.set_defaults(func=cmd_status)

    config_cmd = sub.add_parser("config", help="Run config inside a TAG profile")
    config_cmd.add_argument("--profile", default="orchestrator", help="TAG profile to use")
    config_cmd.add_argument("hermes_args", nargs=argparse.REMAINDER)
    config_cmd.set_defaults(func=cmd_config)

    sessions = sub.add_parser("sessions", help="Run sessions inside a TAG profile")
    sessions.add_argument("--profile", default="orchestrator", help="TAG profile to use")
    sessions.add_argument("hermes_args", nargs=argparse.REMAINDER)
    sessions.set_defaults(func=cmd_sessions)

    skills = sub.add_parser("skills", help="Run skills inside a TAG profile")
    skills.add_argument("--profile", default="orchestrator", help="TAG profile to use")
    skills.add_argument("hermes_args", nargs=argparse.REMAINDER)
    skills.set_defaults(func=cmd_skills)

    plugins = sub.add_parser("plugins", help="Run plugins inside a TAG profile")
    plugins.add_argument("--profile", default="orchestrator", help="TAG profile to use")
    plugins.add_argument("hermes_args", nargs=argparse.REMAINDER)
    plugins.set_defaults(func=cmd_plugins)

    tools_cmd = sub.add_parser("tools", help="Run tools inside a TAG profile")
    tools_cmd.add_argument("--profile", default="orchestrator", help="TAG profile to use")
    tools_cmd.add_argument("hermes_args", nargs=argparse.REMAINDER)
    tools_cmd.set_defaults(func=cmd_tools)

    mcp = sub.add_parser("mcp", help="Run MCP commands inside a TAG profile")
    mcp.add_argument("--profile", default="orchestrator", help="TAG profile to use")
    mcp.add_argument("hermes_args", nargs=argparse.REMAINDER)
    mcp.set_defaults(func=cmd_mcp)

    logs = sub.add_parser("logs", help="Run logs inside a TAG profile")
    logs.add_argument("--profile", default="orchestrator", help="TAG profile to use")
    logs.add_argument("hermes_args", nargs=argparse.REMAINDER)
    logs.set_defaults(func=cmd_logs)

    dashboard = sub.add_parser("dashboard", help="Run dashboard inside a TAG profile")
    dashboard.add_argument("--profile", default="orchestrator", help="TAG profile to use")
    dashboard.add_argument("--port", type=int, metavar="N", help="Dashboard port (default: 3333)")
    dashboard.add_argument("--no-browser", action="store_false", dest="open_browser",
                           help="Print URL only; don't open browser tab")
    dashboard.add_argument("hermes_args", nargs=argparse.REMAINDER)
    dashboard.set_defaults(func=cmd_dashboard)

    memory = sub.add_parser("memory", help="Run memory inside a TAG profile")
    memory.add_argument("--profile", default="orchestrator", help="TAG profile to use")
    memory.add_argument("hermes_args", nargs=argparse.REMAINDER)
    memory.set_defaults(func=cmd_memory)

    completion = sub.add_parser("completion", help="Run completion inside a TAG profile")
    completion.add_argument("--profile", default="orchestrator", help="TAG profile to use")
    completion.add_argument("hermes_args", nargs=argparse.REMAINDER)
    completion.set_defaults(func=cmd_completion)

    prompt_size = sub.add_parser("prompt-size", help="Run prompt-size inside a TAG profile")
    prompt_size.add_argument("--profile", default="orchestrator", help="TAG profile to use")
    prompt_size.add_argument("hermes_args", nargs=argparse.REMAINDER)
    prompt_size.set_defaults(func=cmd_prompt_size)

    update = sub.add_parser("update", help="Run update inside a TAG profile")
    update.add_argument("--profile", default="orchestrator", help="TAG profile to use")
    update.add_argument("--json", action="store_true", help="When TAG manages the update locally, emit JSON")
    update.add_argument("hermes_args", nargs=argparse.REMAINDER)
    update.set_defaults(func=cmd_update)

    tui = sub.add_parser("tui", help="Launch the managed TUI through TAG")
    tui.add_argument("--profile", default="orchestrator", help="TAG profile to use")
    tui.add_argument("hermes_args", nargs=argparse.REMAINDER)
    tui.set_defaults(func=cmd_tui)

    # ---- PRD-002: memory-journal ----
    mj = sub.add_parser("memory-journal", help="Manage TAG's cross-session memory journal")
    mj_sub = mj.add_subparsers(dest="mj_subcommand")

    mj_save = mj_sub.add_parser("save", help="Save a key→value fact")
    mj_save.add_argument("key", help="Fact key (e.g. 'project context')")
    mj_save.add_argument("value", help="Fact value")
    mj_save.add_argument("--profile", help="Profile (default: master_profile)")
    mj_save.add_argument("--ttl-days", type=int, metavar="N", dest="ttl_days")
    mj_save.add_argument("--json", action="store_true")

    mj_list = mj_sub.add_parser("list", help="List journal entries")
    mj_list.add_argument("--profile", help="Profile (default: master_profile)")
    mj_list.add_argument("--json", action="store_true")

    mj_forget = mj_sub.add_parser("forget", help="Delete a journal entry by ID")
    mj_forget.add_argument("entry_id", metavar="ID")
    mj_forget.add_argument("--json", action="store_true")

    mj_clear = mj_sub.add_parser("clear", help="Clear all journal entries for a profile")
    mj_clear.add_argument("--profile", help="Profile (default: master_profile)")
    mj_clear.add_argument("--confirm", action="store_true")
    mj_clear.add_argument("--json", action="store_true")

    for mj_p in [mj, mj_save, mj_list, mj_forget, mj_clear]:
        mj_p.add_argument("--config", help=argparse.SUPPRESS) if "config" not in {a.dest for a in mj_p._actions} else None
        mj_p.set_defaults(func=cmd_memory_journal)

    # ---- PRD-004: swarm ----
    swarm = sub.add_parser("swarm", help="Launch a Hermes Kanban swarm using TAG's profile topology")
    swarm.add_argument("task", help="Task description for the swarm")
    swarm.add_argument("--profile", help="Orchestrator profile (default: master_profile)")
    swarm.add_argument("--type", dest="task_type", default="mixed", choices=("research", "implementation", "review", "mixed"))
    swarm.add_argument("--board", help="Kanban board name (default: from config)")
    swarm.add_argument("--no-wait", action="store_true", dest="no_wait",
                       help="Create swarm tasks but don't block waiting for completion")
    swarm.add_argument("--json", action="store_true")
    swarm.set_defaults(func=cmd_swarm)

    # ---- PRD-007: desktop ----
    desktop = sub.add_parser("desktop", help="Build and launch Electron desktop app")
    desktop_sub = desktop.add_subparsers(dest="desktop_subcommand")
    desktop_open = desktop_sub.add_parser("open", help="Launch the desktop app")
    desktop_open.add_argument("--profile", help="Profile to launch with")
    desktop_build = desktop_sub.add_parser("build", help="Build the desktop app (one-time, ~2-3 min)")
    desktop_build.add_argument("--force", action="store_true")
    desktop_build.add_argument("--json", action="store_true")
    for dp in [desktop, desktop_open, desktop_build]:
        dp.set_defaults(func=cmd_desktop)

    # ---- PRD-008: queue ----
    queue = sub.add_parser("queue", help="Background task queue")
    queue_sub = queue.add_subparsers(dest="queue_subcommand")

    q_add = queue_sub.add_parser("add", help="Queue a task to run in the background")
    q_add.add_argument("task", help="Task description")
    q_add.add_argument("--profile", help="Profile to use (default: master_profile)")
    q_add.add_argument("--type", dest="task_type", default="mixed")
    q_add.add_argument("--priority", type=int, default=5)
    q_add.add_argument("--no-notify", action="store_true")
    q_add.add_argument("--json", action="store_true")

    q_list = queue_sub.add_parser("list", help="List queued/running/done jobs")
    q_list.add_argument("--status", dest="status_filter", choices=("queued", "running", "done", "failed", "cancelled"))
    q_list.add_argument("--limit", type=int, default=50, metavar="N", help="Max jobs to show (default: 50)")
    q_list.add_argument("--json", action="store_true")

    q_result = queue_sub.add_parser("result", help="Show output of a completed job")
    q_result.add_argument("job_id")

    q_cancel = queue_sub.add_parser("cancel", help="Cancel a running job")
    q_cancel.add_argument("job_id")
    q_cancel.add_argument("--json", action="store_true")

    q_clear = queue_sub.add_parser("clear", help="Remove completed/failed jobs from list")
    q_clear.add_argument("--json", action="store_true")

    for qp in [queue, q_add, q_list, q_result, q_cancel, q_clear]:
        qp.set_defaults(func=cmd_queue)

    # ---- PRD-001: import-supermemory, import-honcho ----
    import_sm = sub.add_parser("import-supermemory", help="Import Supermemory API key into a TAG profile")
    import_sm.add_argument("--profile", required=True)
    import_sm.add_argument("--api-key", metavar="KEY", dest="api_key")
    import_sm.add_argument("--source-config-dir", metavar="PATH", dest="source_config_dir")
    import_sm.add_argument("--json", action="store_true")
    import_sm.set_defaults(func=cmd_import_supermemory)

    import_honcho = sub.add_parser("import-honcho", help="Import Honcho credentials into a TAG profile")
    import_honcho.add_argument("--profile", required=True)
    import_honcho.add_argument("--base-url", metavar="URL", dest="base_url")
    import_honcho.add_argument("--source-config", metavar="PATH", dest="source_config")
    import_honcho.add_argument("--json", action="store_true")
    import_honcho.set_defaults(func=cmd_import_honcho)

    # ---- PRD-006: import-nous-portal ----
    import_nous = sub.add_parser("import-nous-portal", help="Import Nous Portal API key (enables Tool Gateway)")
    import_nous.add_argument("--profile", metavar="NAME")
    import_nous.add_argument("--api-key", metavar="KEY", dest="api_key")
    import_nous.add_argument("--all-profiles", action="store_true", dest="all_profiles")
    import_nous.add_argument("--force", action="store_true")
    import_nous.add_argument("--json", action="store_true")
    import_nous.set_defaults(func=cmd_import_nous_portal)

    # ---- PRD-005: execution backend imports ----
    import_docker = sub.add_parser("import-docker", help="Configure Docker execution backend for a profile")
    import_docker.add_argument("--profile", metavar="NAME")
    import_docker.add_argument("--image", metavar="IMAGE", help="Docker image (default: ubuntu:22.04)")
    import_docker.add_argument("--force", action="store_true")
    import_docker.add_argument("--json", action="store_true")
    import_docker.set_defaults(func=cmd_import_docker)

    import_ssh_p = sub.add_parser("import-ssh", help="Configure SSH remote execution backend for a profile")
    import_ssh_p.add_argument("--profile", metavar="NAME")
    import_ssh_p.add_argument("--host", required=True, metavar="HOST")
    import_ssh_p.add_argument("--user", metavar="USER")
    import_ssh_p.add_argument("--key-file", metavar="PATH", dest="key_file")
    import_ssh_p.add_argument("--port", type=int, default=22)
    import_ssh_p.add_argument("--force", action="store_true")
    import_ssh_p.add_argument("--json", action="store_true")
    import_ssh_p.set_defaults(func=cmd_import_ssh)

    import_modal_p = sub.add_parser("import-modal", help="Configure Modal cloud execution backend for a profile")
    import_modal_p.add_argument("--profile", metavar="NAME")
    import_modal_p.add_argument("--token-id", required=True, metavar="ID", dest="token_id")
    import_modal_p.add_argument("--token-secret", required=True, metavar="SECRET", dest="token_secret")
    import_modal_p.add_argument("--force", action="store_true")
    import_modal_p.add_argument("--json", action="store_true")
    import_modal_p.set_defaults(func=cmd_import_modal)

    import_daytona_p = sub.add_parser("import-daytona", help="Configure Daytona workspace backend for a profile")
    import_daytona_p.add_argument("--profile", metavar="NAME")
    import_daytona_p.add_argument("--workspace-id", required=True, metavar="ID", dest="workspace_id")
    import_daytona_p.add_argument("--api-key", metavar="KEY", dest="api_key")
    import_daytona_p.add_argument("--force", action="store_true")
    import_daytona_p.add_argument("--json", action="store_true")
    import_daytona_p.set_defaults(func=cmd_import_daytona)

    # ---- PRD-011: plugin ----
    plugin = sub.add_parser("plugin", help="Manage TAG plugins")
    plugin_sub = plugin.add_subparsers(dest="plugin_subcommand")
    plugin_list = plugin_sub.add_parser("list", help="List available plugins")
    plugin_list.add_argument("--json", action="store_true")
    plugin_install = plugin_sub.add_parser("install", help="Install a plugin into a profile venv")
    plugin_install.add_argument("plugin_name", metavar="NAME")
    plugin_install.add_argument("--profile")
    plugin_install.add_argument("--json", action="store_true")
    plugin_enable = plugin_sub.add_parser("enable", help="Enable a plugin for a profile")
    plugin_enable.add_argument("plugin_name", metavar="NAME")
    plugin_enable.add_argument("--profile")
    plugin_disable = plugin_sub.add_parser("disable", help="Disable a plugin for a profile")
    plugin_disable.add_argument("plugin_name", metavar="NAME")
    plugin_disable.add_argument("--profile")
    for pp in [plugin, plugin_list, plugin_install, plugin_enable, plugin_disable]:
        pp.set_defaults(func=cmd_plugin)

    # ---- PRD-012: costs ----
    costs = sub.add_parser("costs", help="Show token usage and cost estimates for recent runs")
    costs.add_argument("--profile", help="Filter by profile")
    costs.add_argument("--limit", type=positive_int, default=20)
    costs.add_argument("--json", action="store_true")
    costs.set_defaults(func=cmd_costs)

    # ---- PRD-013: trace ----
    trace = sub.add_parser("trace", help="View and export distributed trace spans")
    trace_sub = trace.add_subparsers(dest="trace_subcommand")
    trace_list = trace_sub.add_parser("list", help="List recent traces")
    trace_list.add_argument("--limit", type=positive_int, default=20)
    trace_list.add_argument("--json", action="store_true")
    trace_show = trace_sub.add_parser("show", help="Show flamechart for a trace")
    trace_show.add_argument("trace_id", metavar="TRACE_ID")
    trace_show.add_argument("--json", action="store_true")
    trace_export = trace_sub.add_parser("export", help="Export spans to OTLP endpoint")
    trace_export.add_argument("endpoint", metavar="ENDPOINT")
    trace_export.add_argument("--trace-id", metavar="ID", dest="trace_id")
    trace_export.add_argument("--profile")
    # PRD-032: replay, diff, checkpoint, snapshot
    trace_replay = trace_sub.add_parser("replay", help="Replay a captured trace snapshot (PRD-032)")
    trace_replay.add_argument("trace_id", metavar="TRACE_ID")
    trace_replay.add_argument("--json", action="store_true")
    trace_diff = trace_sub.add_parser("diff", help="Diff two traces span-by-span (PRD-032)")
    trace_diff.add_argument("trace_a", metavar="TRACE_A")
    trace_diff.add_argument("trace_b", metavar="TRACE_B")
    trace_diff.add_argument("--json", action="store_true")
    trace_checkpoint = trace_sub.add_parser("checkpoint", help="List snapshots for a trace (PRD-032)")
    trace_checkpoint.add_argument("trace_id", metavar="TRACE_ID")
    trace_checkpoint.add_argument("--json", action="store_true")
    trace_snapshot = trace_sub.add_parser("snapshot", help="Capture a trace snapshot (PRD-032)")
    trace_snapshot.add_argument("trace_id", metavar="TRACE_ID")
    for tp in [trace, trace_list, trace_show, trace_export,
               trace_replay, trace_diff, trace_checkpoint, trace_snapshot]:
        tp.set_defaults(func=cmd_trace)

    # ---- PRD-014: mcp-registry ----
    mcp_reg = sub.add_parser("mcp-registry", help="Browse and install curated MCP servers")
    mcp_reg_sub = mcp_reg.add_subparsers(dest="mcp_reg_subcommand")
    mcp_list = mcp_reg_sub.add_parser("list", help="List available MCP servers")
    mcp_list.add_argument("--category", help="Filter by category")
    mcp_list.add_argument("--json", action="store_true")
    mcp_install = mcp_reg_sub.add_parser("install", help="Install an MCP server globally")
    mcp_install.add_argument("server_name", metavar="NAME")
    mcp_enable = mcp_reg_sub.add_parser("enable", help="Enable an MCP server for a profile")
    mcp_enable.add_argument("server_name", metavar="NAME")
    mcp_enable.add_argument("--profile")
    mcp_disable = mcp_reg_sub.add_parser("disable", help="Disable an MCP server for a profile")
    mcp_disable.add_argument("server_name", metavar="NAME")
    mcp_disable.add_argument("--profile")
    for mp in [mcp_reg, mcp_list, mcp_install, mcp_enable, mcp_disable]:
        mp.set_defaults(func=cmd_mcp_registry)

    # ---- PRD-015: template ----
    tmpl = sub.add_parser("template", help="Export/import/fetch profile config templates")
    tmpl_sub = tmpl.add_subparsers(dest="template_subcommand")
    tmpl_export = tmpl_sub.add_parser("export", help="Export a profile as a YAML template")
    tmpl_export.add_argument("--profile")
    tmpl_export.add_argument("--output", "-o", metavar="FILE", help="Write to file instead of stdout")
    tmpl_import = tmpl_sub.add_parser("import", help="Import a YAML template as a new profile")
    tmpl_import.add_argument("template_file", metavar="FILE")
    tmpl_import.add_argument("--profile", help="Override profile name from template")
    tmpl_fetch = tmpl_sub.add_parser("fetch", help="Fetch a template from a URL")
    tmpl_fetch.add_argument("url", metavar="URL")
    for tp in [tmpl, tmpl_export, tmpl_import, tmpl_fetch]:
        tp.set_defaults(func=cmd_template)

    # ---- PRD-016: hooks ----
    hooks_cmd = sub.add_parser("hooks", help="Manage and test TAG lifecycle event hooks")
    hooks_sub = hooks_cmd.add_subparsers(dest="hooks_subcommand")
    hooks_list = hooks_sub.add_parser("list", help="List configured hooks")
    hooks_list.add_argument("--json", action="store_true")
    hooks_log = hooks_sub.add_parser("log", help="Show recent hook execution log")
    hooks_log.add_argument("--limit", type=positive_int, default=50)
    hooks_log.add_argument("--json", action="store_true")
    hooks_test = hooks_sub.add_parser("test", help="Test-fire hooks for an event type")
    hooks_test.add_argument("event_type", metavar="EVENT")
    for hp in [hooks_cmd, hooks_list, hooks_log, hooks_test]:
        hp.set_defaults(func=cmd_hooks)

    # ---- PRD-017: compare ----
    compare = sub.add_parser("compare", help="Multi-model benchmark comparisons")
    compare_sub = compare.add_subparsers(dest="compare_subcommand")
    compare_list = compare_sub.add_parser("list", help="List saved comparisons")
    compare_list.add_argument("--limit", type=positive_int, default=20)
    compare_list.add_argument("--json", action="store_true")
    compare_show = compare_sub.add_parser("show", help="Show comparison results")
    compare_show.add_argument("comparison_id", metavar="ID")
    compare_show.add_argument("--json", action="store_true")
    compare_run = compare_sub.add_parser("run", help="Run a new multi-model comparison")
    compare_run.add_argument("--profile")
    compare_run.add_argument("--suite", required=True, help="Path to benchmark suite YAML")
    compare_run.add_argument("--model-ref", action="append", default=[], metavar="REF",
                             help="Model reference (provider/model-id); repeat for multiple")
    compare_run.add_argument("--json", action="store_true")
    for cp in [compare, compare_list, compare_show, compare_run]:
        cp.set_defaults(func=cmd_compare)

    # ---- PRD-018: context ----
    context_cmd = sub.add_parser("context", help="Manage agent context window size")
    context_sub = context_cmd.add_subparsers(dest="context_subcommand")
    ctx_show = context_sub.add_parser("show", help="List active sessions and their token counts")
    ctx_show.add_argument("--profile")
    ctx_show.add_argument("--json", action="store_true")
    ctx_compress = context_sub.add_parser("compress", help="Summarize and compress a session context")
    ctx_compress.add_argument("--profile")
    ctx_compress.add_argument("--session-id", required=True, dest="session_id")
    ctx_trim = context_sub.add_parser("trim", help="Trim a session to the last N turns")
    ctx_trim.add_argument("--profile")
    ctx_trim.add_argument("--session-id", required=True, dest="session_id")
    ctx_trim.add_argument("--keep-last", type=positive_int, default=10, dest="keep_last")
    for ctx_p in [context_cmd, ctx_show, ctx_compress, ctx_trim]:
        ctx_p.set_defaults(func=cmd_context)

    # ---- PRD-019: shell ----
    shell_cmd = sub.add_parser("shell", help="Open interactive natural-language TAG shell")
    shell_cmd.add_argument("--profile", help="Profile to use (default: master_profile)")
    shell_cmd.set_defaults(func=cmd_shell)

    # ---- PRD-020: review-pr / ci ----
    review_pr_cmd = sub.add_parser("review-pr", help="AI code review for a GitHub pull request")
    review_pr_cmd.add_argument("--repo", required=True, help="GitHub repository (owner/name)")
    review_pr_cmd.add_argument("--pr", required=True, type=int, metavar="NUMBER")
    review_pr_cmd.add_argument("--profile", help="Profile to run review with")
    review_pr_cmd.add_argument("--post-comments", action="store_true", dest="post_comments",
                               help="Post review as a PR comment via gh CLI")
    review_pr_cmd.set_defaults(func=cmd_review_pr)

    ci_cmd = sub.add_parser("ci", help="CI/CD integration utilities")
    ci_sub = ci_cmd.add_subparsers(dest="ci_subcommand")
    ci_diagnose = ci_sub.add_parser("diagnose", help="Diagnose a CI failure from a log file")
    ci_diagnose.add_argument("--log-file", required=True, dest="log_file", metavar="PATH")
    ci_diagnose.add_argument("--profile")
    ci_lint = ci_sub.add_parser("commit-lint", help="Suggest a conventional commit message for staged changes")
    ci_lint.add_argument("--profile")
    ci_status = ci_sub.add_parser("status", help="Show git host and working tree status")
    for cp in [ci_cmd, ci_diagnose, ci_lint, ci_status]:
        cp.set_defaults(func=cmd_ci)

    # ---- PRD-021: loop ----
    loop_cmd = sub.add_parser("loop", help="Autonomous agent loop with goal detection and iteration cap")
    loop_sub = loop_cmd.add_subparsers(dest="loop_subcommand")
    loop_start = loop_sub.add_parser("start", help="Start an autonomous loop")
    loop_start.add_argument("--goal", required=True, help="Goal text the loop works toward")
    loop_start.add_argument("--profile", help="Profile to run (default: master_profile)")
    loop_start.add_argument("--max-iters", type=int, default=10, dest="max_iters",
                            help="Maximum number of iterations (default: 10)")
    loop_start.add_argument("--approval", choices=["auto", "human"], default="auto",
                            help="Gate mode: auto continues automatically, human pauses for approval")
    loop_start.add_argument("--json", action="store_true")
    loop_list = loop_sub.add_parser("list", help="List all loop runs")
    loop_list.add_argument("--json", action="store_true")
    loop_status_p = loop_sub.add_parser("status", help="Show status and iterations of a loop")
    loop_status_p.add_argument("loop_id", metavar="LOOP_ID")
    loop_status_p.add_argument("--json", action="store_true")
    loop_abort = loop_sub.add_parser("abort", help="Abort a running loop")
    loop_abort.add_argument("loop_id", metavar="LOOP_ID")
    for lp in [loop_cmd, loop_start, loop_list, loop_status_p, loop_abort]:
        lp.set_defaults(func=cmd_loop)

    # ---- PRD-022: cron ----
    cron_cmd = sub.add_parser("cron", help="Cron-style scheduled agent runs")
    cron_sub = cron_cmd.add_subparsers(dest="cron_subcommand")
    cron_add = cron_sub.add_parser("add", help="Add a cron job")
    cron_add.add_argument("--name", required=True, help="Unique job name")
    cron_add.add_argument("--schedule", required=True, help="5-field cron expression (e.g. '0 9 * * 1-5')")
    cron_add.add_argument("--profile", help="Profile to run task with")
    cron_add.add_argument("task", metavar="TASK", help="Task text to submit")
    cron_add.add_argument("--json", action="store_true")
    cron_list = cron_sub.add_parser("list", help="List all cron jobs")
    cron_list.add_argument("--json", action="store_true")
    cron_remove = cron_sub.add_parser("remove", help="Remove a cron job")
    cron_remove.add_argument("job_id", metavar="JOB_ID")
    cron_enable = cron_sub.add_parser("enable", help="Enable a cron job")
    cron_enable.add_argument("job_id", metavar="JOB_ID")
    cron_disable = cron_sub.add_parser("disable", help="Disable a cron job")
    cron_disable.add_argument("job_id", metavar="JOB_ID")
    cron_run = cron_sub.add_parser("run", help="Trigger a cron job immediately (ignore schedule)")
    cron_run.add_argument("job_id", metavar="JOB_ID")
    cron_daemon = cron_sub.add_parser("daemon", help="Run the cron daemon in-process (blocking)")
    for cp in [cron_cmd, cron_add, cron_list, cron_remove, cron_enable, cron_disable, cron_run, cron_daemon]:
        cp.set_defaults(func=cmd_cron)

    # ---- PRD-024: workspace ----
    ws_cmd = sub.add_parser("workspace", help="Repo-map and workspace context indexing")
    ws_sub = ws_cmd.add_subparsers(dest="workspace_subcommand")
    ws_index = ws_sub.add_parser("index", help="Index workspace files")
    ws_index.add_argument("--path", default=".", help="Root path to index (default: .)")
    ws_index.add_argument("--max-files", type=int, default=500, dest="max_files")
    ws_index.add_argument("--json", action="store_true")
    ws_map = ws_sub.add_parser("map", help="Print token-efficient workspace map")
    ws_map.add_argument("--path", default=".", help="Root path")
    ws_map.add_argument("--budget", type=int, default=4000, help="Token budget (default: 4000)")
    ws_map.add_argument("--json", action="store_true")
    ws_status = ws_sub.add_parser("status", help="Show workspace index statistics")
    ws_status.add_argument("--json", action="store_true")
    ws_clear = ws_sub.add_parser("clear", help="Clear workspace index")
    for wp in [ws_cmd, ws_index, ws_map, ws_status, ws_clear]:
        wp.set_defaults(func=cmd_workspace)

    # ---- PRD-025: memory (semantic) ----
    mem_cmd = sub.add_parser("mem", help="Semantic memory with confidence decay (tag mem)")
    mem_sub = mem_cmd.add_subparsers(dest="mem_subcommand")
    mem_add = mem_sub.add_parser("add", help="Add a memory")
    mem_add.add_argument("content", metavar="CONTENT", help="Memory text")
    mem_add.add_argument("--type", dest="memory_type", default="fact",
                         choices=["fact", "convention", "decision", "gotcha", "other"])
    mem_add.add_argument("--confidence", type=float, default=1.0)
    mem_add.add_argument("--profile")
    mem_add.add_argument("--json", action="store_true")
    mem_search = mem_sub.add_parser("search", help="Full-text search over memories")
    mem_search.add_argument("query", metavar="QUERY")
    mem_search.add_argument("--type", dest="memory_type")
    mem_search.add_argument("--limit", type=int, default=10)
    mem_search.add_argument("--profile")
    mem_search.add_argument("--json", action="store_true")
    mem_list = mem_sub.add_parser("list", help="List memories sorted by effective confidence")
    mem_list.add_argument("--type", dest="memory_type")
    mem_list.add_argument("--limit", type=int, default=20)
    mem_list.add_argument("--profile")
    mem_list.add_argument("--json", action="store_true")
    mem_forget = mem_sub.add_parser("forget", help="Delete a memory by ID")
    mem_forget.add_argument("mem_id", metavar="MEMORY_ID")
    mem_forget.add_argument("--profile")
    mem_stats = mem_sub.add_parser("stats", help="Show memory store statistics")
    mem_stats.add_argument("--profile")
    mem_stats.add_argument("--json", action="store_true")
    for mp in [mem_cmd, mem_add, mem_search, mem_list, mem_forget, mem_stats]:
        mp.set_defaults(func=cmd_memory_semantic)

    # ---- PRD-026: marketplace ----
    mkt_cmd = sub.add_parser("marketplace", help="Profile marketplace: pull/push profiles")
    mkt_sub = mkt_cmd.add_subparsers(dest="marketplace_subcommand")
    mkt_pull = mkt_sub.add_parser("pull", help="Download a profile from a URL")
    mkt_pull.add_argument("url", metavar="URL")
    mkt_pull.add_argument("--name", help="Local name for the profile (default: filename)")
    mkt_pull.add_argument("--json", action="store_true")
    mkt_push = mkt_sub.add_parser("push", help="Show how to push a profile to GitHub Gist")
    mkt_push.add_argument("profile_name", metavar="PROFILE_NAME")
    mkt_list = mkt_sub.add_parser("list", help="List cached profiles")
    mkt_list.add_argument("--json", action="store_true")
    for mp in [mkt_cmd, mkt_pull, mkt_push, mkt_list]:
        mp.set_defaults(func=cmd_profile_marketplace)

    # ---- PRD-027: eval ----
    eval_cmd = sub.add_parser("eval", help="Run eval suites against TAG profiles")
    eval_sub = eval_cmd.add_subparsers(dest="eval_subcommand")
    eval_run = eval_sub.add_parser("run", help="Run an eval suite")
    eval_run.add_argument("--suite", required=True, metavar="SUITE_PATH", help="Path to YAML eval suite")
    eval_run.add_argument("--profile", help="Profile to evaluate")
    eval_run.add_argument("--dry-run", action="store_true", dest="dry_run",
                          help="Validate suite without running agent")
    eval_run.add_argument("--json", action="store_true")
    eval_list = eval_sub.add_parser("list", help="List eval runs")
    eval_list.add_argument("--json", action="store_true")
    eval_show = eval_sub.add_parser("show", help="Show eval run detail")
    eval_show.add_argument("run_id", metavar="RUN_ID")
    eval_show.add_argument("--json", action="store_true")
    for ep in [eval_cmd, eval_run, eval_list, eval_show]:
        ep.set_defaults(func=cmd_eval)

    # ---- PRD-028: sandbox ----
    sb_cmd = sub.add_parser("sandbox", help="Isolated code execution (restricted subprocess or Docker)")
    sb_sub = sb_cmd.add_subparsers(dest="sandbox_subcommand")
    sb_run = sb_sub.add_parser("run", help="Run a command in the sandbox")
    sb_run.add_argument("command", metavar="COMMAND", help="Shell command to run")
    sb_run.add_argument("--backend", choices=["restricted", "docker"], default="restricted")
    sb_run.add_argument("--image", default="python:3.12-slim", help="Docker image (for --backend docker)")
    sb_run.add_argument("--timeout", type=int, default=60, metavar="SECONDS")
    sb_run.add_argument("--json", action="store_true")
    sb_list = sb_sub.add_parser("list", help="List recent sandbox runs")
    sb_list.add_argument("--json", action="store_true")
    sb_result = sb_sub.add_parser("result", help="Show sandbox run output")
    sb_result.add_argument("run_id", metavar="RUN_ID")
    sb_result.add_argument("--json", action="store_true")
    for sp in [sb_cmd, sb_run, sb_list, sb_result]:
        sp.set_defaults(func=cmd_sandbox)

    # ---- PRD-029: serve ----
    serve_cmd = sub.add_parser("serve", help="Start local HTTP dashboard server with SSE streaming")
    serve_cmd.add_argument("--port", type=int, default=7880, help="Port to listen on (default: 7880)")
    serve_cmd.add_argument("--profile", help="Default profile for dashboard view")
    serve_cmd.set_defaults(func=cmd_serve)

    # ---- PRD-030: cache ----
    cache_cmd = sub.add_parser("cache", help="Prompt cache analytics")
    cache_sub = cache_cmd.add_subparsers(dest="cache_subcommand")
    cache_stats = cache_sub.add_parser("stats", help="Show cache hit rates and savings per profile")
    cache_stats.add_argument("--profile", help="Filter to a specific profile")
    cache_stats.add_argument("--json", action="store_true")
    for cp in [cache_cmd, cache_stats]:
        cp.set_defaults(func=cmd_cache)

    # ---- PRD-031: route fallback ----
    # Extend the existing 'route' command with a 'fallback' subgroup
    route_fallback_cmd = sub.add_parser("route-fallback", help="Manage model fallback chains (PRD-031)")
    rf_sub = route_fallback_cmd.add_subparsers(dest="fallback_subcommand")
    rf_add = rf_sub.add_parser("add", help="Add a fallback chain")
    rf_add.add_argument("--primary", required=True, help="Primary model ID")
    rf_add.add_argument("--fallback", required=True, help="Fallback model ID")
    rf_add.add_argument("--condition", default="context_overflow",
                        choices=["context_overflow", "error", "timeout", "cost_limit", "any"])
    rf_add.add_argument("--priority", type=int, default=1)
    rf_add.add_argument("--profile")
    rf_add.add_argument("--json", action="store_true")
    rf_list = rf_sub.add_parser("list", help="List fallback chains")
    rf_list.add_argument("--profile")
    rf_list.add_argument("--json", action="store_true")
    rf_remove = rf_sub.add_parser("remove", help="Remove a fallback chain")
    rf_remove.add_argument("fb_id", metavar="FALLBACK_ID")
    rf_remove.add_argument("--profile")
    rf_resolve = rf_sub.add_parser("resolve", help="Show which fallback would be used")
    rf_resolve.add_argument("--primary", required=True)
    rf_resolve.add_argument("--condition", default="context_overflow")
    rf_resolve.add_argument("--profile")
    rf_resolve.add_argument("--json", action="store_true")
    for rfp in [route_fallback_cmd, rf_add, rf_list, rf_remove, rf_resolve]:
        rfp.set_defaults(func=cmd_route_fallback)

    # ---- PRD-032: extend trace with replay/diff/checkpoint/snapshot ----
    # The existing 'trace' command now supports replay, diff, checkpoint, snapshot
    # Parser entries are added here as aliases on the existing trace_sub
    # (already registered above — we just need to add the new subcommands)

    # ---- PRD-033: dependency-aware task queue / DAG ----
    dag_cmd = sub.add_parser("dag", help="DAG workflow engine for queue jobs (PRD-033)")
    dag_sub = dag_cmd.add_subparsers(dest="dag_subcommand")
    dag_show = dag_sub.add_parser("show", help="Show job dependency graph")
    dag_show.add_argument("job_ids", nargs="*", metavar="JOB_ID", help="Job IDs to show (default: all)")
    dag_show.add_argument("--json", action="store_true")
    dag_save = dag_sub.add_parser("save", help="Save a named DAG spec")
    dag_save.add_argument("name", metavar="NAME")
    dag_save.add_argument("--steps", default="[]", help="JSON array of step objects")
    dag_run = dag_sub.add_parser("run", help="Submit a named DAG")
    dag_run.add_argument("name", metavar="NAME")
    dag_run.add_argument("--board", default="default")
    dag_list = dag_sub.add_parser("list", help="List saved DAGs")
    dag_list.add_argument("--json", action="store_true")
    for dp in [dag_cmd, dag_show, dag_save, dag_run, dag_list]:
        dp.set_defaults(func=cmd_dag)

    qext_cmd = sub.add_parser("queue-dep", help="Add queue job with dependencies (PRD-033)")
    qext_sub = qext_cmd.add_subparsers(dest="queue_ext_subcommand")
    qadd = qext_sub.add_parser("add", help="Add a queue job with --depends-on")
    qadd.add_argument("task", metavar="TASK")
    qadd.add_argument("--depends-on", dest="depends_on", action="append", metavar="JOB_ID",
                      help="Prerequisite job ID (can be repeated)")
    qadd.add_argument("--profile")
    qadd.add_argument("--json", action="store_true")
    qpromote = qext_sub.add_parser("promote", help="Promote ready pending jobs")
    for qp in [qext_cmd, qadd, qpromote]:
        qp.set_defaults(func=cmd_queue_extended)

    # ---- PRD-034: secret scanning ----
    sec_cmd = sub.add_parser("security", help="Secret scanning and security auditing (PRD-034)")
    sec_sub = sec_cmd.add_subparsers(dest="security_subcommand")
    sec_scan = sec_sub.add_parser("scan", help="Scan files for secrets")
    sec_scan.add_argument("path", nargs="?", default=".", metavar="PATH")
    sec_scan.add_argument("--max-files", type=int, default=2000)
    sec_scan.add_argument("--json", action="store_true")
    sec_list = sec_sub.add_parser("list", help="List past scan results")
    sec_list.add_argument("--json", action="store_true")
    for sp in [sec_cmd, sec_scan, sec_list]:
        sp.set_defaults(func=cmd_security)

    # ---- PRD-035: IDE Bridge / LSP ----
    lsp_cmd = sub.add_parser("lsp", help="TAG IDE Bridge / LSP server (PRD-035)")
    lsp_sub = lsp_cmd.add_subparsers(dest="lsp_subcommand")
    lsp_start = lsp_sub.add_parser("start", help="Start LSP server")
    lsp_start.add_argument("--port", type=int, default=7878, help="TCP port (0=stdio)")
    lsp_start.add_argument("--stdio", action="store_true", help="Use stdio transport")
    lsp_status = lsp_sub.add_parser("status", help="Show running LSP sessions")
    lsp_status.add_argument("--json", action="store_true")
    for lp in [lsp_cmd, lsp_start, lsp_status]:
        lp.set_defaults(func=cmd_lsp)

    # ---- PRD-036: Web Dashboard ----
    web_cmd = sub.add_parser("web", help="Local web dashboard (FastAPI+React) (PRD-036)")
    web_cmd.add_argument("--port", type=int, default=8787)
    web_cmd.add_argument("--host", default="127.0.0.1")
    web_cmd.add_argument("--no-browser", action="store_true")
    web_cmd.set_defaults(func=cmd_web)

    # ---- PRD-037: Agent Personas ----
    persona_cmd = sub.add_parser("persona", help="Agent persona management (PRD-037)")
    persona_sub = persona_cmd.add_subparsers(dest="persona_subcommand")
    pa_list = persona_sub.add_parser("list", help="List available personas")
    pa_list.add_argument("--json", action="store_true")
    pa_show = persona_sub.add_parser("show", help="Show persona details")
    pa_show.add_argument("name", metavar="NAME")
    pa_show.add_argument("--json", action="store_true")
    pa_apply = persona_sub.add_parser("apply", help="Apply a persona to a profile")
    pa_apply.add_argument("name", metavar="NAME")
    pa_apply.add_argument("--profile")
    pa_apply.add_argument("--session-id")
    pa_remove = persona_sub.add_parser("remove", help="Remove an active persona from a profile")
    pa_remove.add_argument("name", metavar="NAME")
    pa_remove.add_argument("--profile")
    pa_stack = persona_sub.add_parser("stack", help="Show active persona stack for a profile")
    pa_stack.add_argument("--profile")
    pa_install = persona_sub.add_parser("install", help="Install a persona from a YAML file")
    pa_install.add_argument("file", metavar="FILE")
    pa_preview = persona_sub.add_parser("preview", help="Preview merged system prompt with active personas")
    pa_preview.add_argument("--profile")
    pa_preview.add_argument("--base-prompt", default="You are a helpful agent.")
    for pp in [persona_cmd, pa_list, pa_show, pa_apply, pa_remove, pa_stack, pa_install, pa_preview]:
        pp.set_defaults(func=cmd_persona)

    # ---- PRD-038: Diff-Aware Context Injection ----
    diff_cmd = sub.add_parser("diff-context", help="Inject git diff context for agent runs (PRD-038)")
    diff_cmd.add_argument("--ref", default="HEAD", help="Git ref to diff against")
    diff_cmd.add_argument("--staged", action="store_true", help="Diff staged changes only")
    diff_cmd.add_argument("--pr", type=int, metavar="PR_NUMBER", help="GitHub PR number")
    diff_cmd.add_argument("--repo", help="GitHub repo (owner/repo) for --pr")
    diff_cmd.add_argument("--context-lines", type=int, default=3, dest="context_lines")
    diff_cmd.add_argument("--max-files", type=int, default=10)
    diff_cmd.add_argument("--blocked", action="append", metavar="PATTERN", help="Extra blocked patterns")
    diff_cmd.add_argument("--output-only", action="store_true", help="Print diff content without saving")
    diff_cmd.add_argument("--workdir", default=".")
    diff_cmd.add_argument("--json", action="store_true")
    diff_cmd.set_defaults(func=cmd_diff_inject)

    # ---- PRD-039: Token Budget Enforcement ----
    budget_cmd = sub.add_parser("budget", help="Per-profile token budget enforcement (PRD-039)")
    budget_sub = budget_cmd.add_subparsers(dest="budget_subcommand")
    b_set = budget_sub.add_parser("set", help="Set token budget")
    b_set.add_argument("--profile")
    b_set.add_argument("--max-tokens", type=int, required=True, dest="max_tokens")
    b_set.add_argument("--period", choices=["daily", "weekly", "monthly"], default="daily")
    b_set.add_argument("--warn-pct", type=float, default=0.8, dest="warn_pct")
    b_get = budget_sub.add_parser("get", help="Get token budget for a profile")
    b_get.add_argument("--profile")
    b_get.add_argument("--json", action="store_true")
    b_list = budget_sub.add_parser("list", help="List all token budgets")
    b_list.add_argument("--json", action="store_true")
    b_remove = budget_sub.add_parser("remove", help="Remove token budget")
    b_remove.add_argument("--profile")
    b_check = budget_sub.add_parser("check", help="Check current usage against budget")
    b_check.add_argument("--profile")
    b_check.add_argument("--json", action="store_true")
    for bp in [budget_cmd, b_set, b_get, b_list, b_remove, b_check]:
        bp.set_defaults(func=cmd_budget)

    # ---- PRD-040: Notification Hooks ----
    notify_cmd = sub.add_parser("notify", help="Notification hooks (Slack, email, desktop) (PRD-040)")
    notify_sub = notify_cmd.add_subparsers(dest="notify_subcommand")
    n_add = notify_sub.add_parser("add", help="Add a notification hook")
    n_add.add_argument("--event", default="run.completed")
    n_add.add_argument("--channel", choices=["slack", "email", "desktop", "webhook"], default="desktop")
    n_add.add_argument("--profile")
    n_add.add_argument("--config-json", default="{}", dest="config_json")
    n_add.add_argument("--template", default="")
    n_add.add_argument("--json", action="store_true")
    n_list = notify_sub.add_parser("list", help="List notification hooks")
    n_list.add_argument("--profile")
    n_list.add_argument("--json", action="store_true")
    n_test = notify_sub.add_parser("test", help="Send test notification")
    n_test.add_argument("hook_id", metavar="HOOK_ID")
    n_remove = notify_sub.add_parser("remove", help="Remove a notification hook")
    n_remove.add_argument("hook_id", metavar="HOOK_ID")
    n_enable = notify_sub.add_parser("enable", help="Enable a hook")
    n_enable.add_argument("hook_id", metavar="HOOK_ID")
    n_disable = notify_sub.add_parser("disable", help="Disable a hook")
    n_disable.add_argument("hook_id", metavar="HOOK_ID")
    for np in [notify_cmd, n_add, n_list, n_test, n_remove, n_enable, n_disable]:
        np.set_defaults(func=cmd_notify)

    # ---- PRD-041: OTel GenAI Span Cost Attribution ----
    otel_cmd = sub.add_parser("otel-export", help="Export spans with OTel GenAI semconv attributes (PRD-041)")
    otel_cmd.add_argument("--trace-id", dest="trace_id", metavar="TRACE_ID")
    otel_cmd.add_argument("--endpoint", help="OTLP HTTP endpoint (e.g. http://localhost:4318)")
    otel_cmd.add_argument("--semconv", default="1.28.0", help="Override OTel GenAI semconv version")
    otel_cmd.add_argument("--no-metrics", action="store_true", dest="no_metrics")
    otel_cmd.add_argument("--json", action="store_true")
    otel_cmd.set_defaults(func=cmd_otel_export)

    # ---- PRD-042: Architect/Editor Agent Split ----
    split_cmd = sub.add_parser("split", help="Architect/Editor agent split execution (PRD-042)")
    split_sub = split_cmd.add_subparsers(dest="split_subcommand")
    sp_list = split_sub.add_parser("list", help="List split runs")
    sp_list.add_argument("--json", action="store_true")
    sp_show = split_sub.add_parser("show", help="Show split run details")
    sp_show.add_argument("run_id", metavar="RUN_ID")
    sp_show.add_argument("--json", action="store_true")
    sp_plan = split_sub.add_parser("plan", help="Create a split run plan")
    sp_plan.add_argument("task", metavar="TASK")
    sp_plan.add_argument("--architect", default="claude-opus-4")
    sp_plan.add_argument("--editor", default="claude-haiku-4-5")
    sp_plan.add_argument("--profile")
    sp_plan.add_argument("--spec-json", dest="spec_json", help="Optional pre-built spec JSON")
    for ssp in [split_cmd, sp_list, sp_show, sp_plan]:
        ssp.set_defaults(func=cmd_split)

    # ---- PRD-043: Vector-Based Tool Retrieval ----
    tr_cmd = sub.add_parser("tool-index", help="Vector tool retrieval for MCP registry (PRD-043)")
    tr_sub = tr_cmd.add_subparsers(dest="tr_subcommand")
    tr_index = tr_sub.add_parser("index", help="Build tool embedding index")
    tr_search = tr_sub.add_parser("search", help="Search tools by query")
    tr_search.add_argument("query", metavar="QUERY")
    tr_search.add_argument("--top-k", type=int, default=8, dest="top_k")
    tr_search.add_argument("--json", action="store_true")
    tr_status = tr_sub.add_parser("status", help="Show tool index status")
    tr_status.add_argument("--json", action="store_true")
    for tp in [tr_cmd, tr_index, tr_search, tr_status]:
        tp.set_defaults(func=cmd_tool_retrieval)

    # ---- PRD-044: AgentOps Session Observability ----
    ao_cmd = sub.add_parser("agentops", help="AgentOps session observability (PRD-044)")
    ao_sub = ao_cmd.add_subparsers(dest="agentops_subcommand")
    ao_status = ao_sub.add_parser("status", help="Show AgentOps integration status")
    ao_status.add_argument("--json", action="store_true")
    ao_sessions = ao_sub.add_parser("sessions", help="List AgentOps sessions")
    ao_sessions.add_argument("--limit", type=int, default=20)
    ao_sessions.add_argument("--json", action="store_true")
    ao_show = ao_sub.add_parser("show", help="Show AgentOps session for a run")
    ao_show.add_argument("run_id", metavar="RUN_ID")
    ao_show.add_argument("--json", action="store_true")
    for ap in [ao_cmd, ao_status, ao_sessions, ao_show]:
        ap.set_defaults(func=cmd_agentops)

    return parser


def main() -> int:
    parser = build_parser()
    args = parser.parse_args()
    if not getattr(args, "command", None):
        return int(cmd_default(args))
    try:
        return int(args.func(args))
    except sqlite3.OperationalError as exc:
        msg = str(exc)
        if "readonly" in msg.lower():
            print_error(f"Database is read-only — check file permissions: {msg}")
        elif "locked" in msg.lower():
            print_error(f"Database is locked by another process: {msg}")
        else:
            print_error(f"Database error: {msg}")
        return 1


if __name__ == "__main__":
    sys.exit(main())
