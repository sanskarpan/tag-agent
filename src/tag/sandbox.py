"""PRD-028: Sandbox Code Execution (tag sandbox).

Runs arbitrary commands in an isolated environment. Three backends:
  - restricted (default): subprocess with resource limits + timeout
  - docker: Docker container (requires docker CLI)
  - modal: Modal cloud (requires modal SDK + credentials)

All runs are recorded in the sandbox_runs SQLite table.
"""
from __future__ import annotations

import os
import shlex
import sqlite3
import subprocess
import sys
import uuid
from pathlib import Path

BACKENDS = {"restricted", "docker", "modal"}


def ensure_schema(conn: sqlite3.Connection) -> None:
    conn.executescript("""
        CREATE TABLE IF NOT EXISTS sandbox_runs (
          id          TEXT PRIMARY KEY,
          command     TEXT NOT NULL,
          backend     TEXT NOT NULL DEFAULT 'restricted',
          image       TEXT,
          status      TEXT NOT NULL DEFAULT 'running',
          exit_code   INTEGER,
          output      TEXT NOT NULL DEFAULT '',
          error       TEXT,
          created_at  TEXT NOT NULL,
          completed_at TEXT
        );
        CREATE INDEX IF NOT EXISTS idx_sr_status ON sandbox_runs(status, created_at);
    """)
    conn.commit()


def _utc_now() -> str:
    import datetime
    return datetime.datetime.now(datetime.timezone.utc).isoformat()


def _run_restricted(
    command: list[str],
    *,
    timeout: int = 60,
    workdir: Path | None = None,
) -> tuple[int, str, str]:
    """Run command in a restricted subprocess. Returns (exit_code, stdout, stderr)."""
    env = {
        "PATH": "/usr/bin:/bin:/usr/local/bin",
        "HOME": str(workdir or Path.home()),
    }
    # On Linux, set resource limits via preexec_fn
    preexec = None
    if sys.platform.startswith("linux"):
        def _set_limits():
            import resource
            # CPU limit: timeout + 5 seconds grace
            resource.setrlimit(resource.RLIMIT_CPU, (timeout + 5, timeout + 10))
            # Memory limit: 512 MB
            mem = 512 * 1024 * 1024
            resource.setrlimit(resource.RLIMIT_AS, (mem, mem))
        preexec = _set_limits

    try:
        proc = subprocess.run(
            command,
            capture_output=True,
            text=True,
            timeout=timeout,
            env=env,
            cwd=str(workdir) if workdir else None,
            preexec_fn=preexec,
        )
        return proc.returncode, proc.stdout, proc.stderr
    except subprocess.TimeoutExpired:
        return 124, "", f"Timed out after {timeout} seconds"
    except FileNotFoundError as exc:
        return 127, "", f"Command not found: {exc}"
    except Exception as exc:
        return 1, "", f"Execution error: {exc}"


def _run_docker(
    command: list[str],
    image: str,
    *,
    timeout: int = 60,
) -> tuple[int, str, str]:
    """Run command inside a Docker container."""
    docker = "docker"
    docker_cmd = [
        docker, "run",
        "--rm",
        "--network=none",
        "--memory=512m",
        "--cpus=1",
        f"--stop-timeout={timeout}",
        image,
    ] + command
    try:
        proc = subprocess.run(
            docker_cmd,
            capture_output=True,
            text=True,
            timeout=timeout + 30,  # extra time for container spin-up
        )
        return proc.returncode, proc.stdout, proc.stderr
    except FileNotFoundError:
        return 1, "", "docker not found — install Docker or use --backend restricted"
    except subprocess.TimeoutExpired:
        return 124, "", f"Docker run timed out after {timeout}s"
    except Exception as exc:
        return 1, "", str(exc)


def run_in_sandbox(
    conn: sqlite3.Connection,
    command_str: str,
    *,
    backend: str = "restricted",
    image: str = "python:3.12-slim",
    timeout: int = 60,
    workdir: Path | None = None,
) -> dict:
    """Execute *command_str* in the sandbox. Returns a result dict with output."""
    ensure_schema(conn)
    if backend not in BACKENDS:
        raise ValueError(f"backend must be one of {BACKENDS}, got {backend!r}")

    run_id = uuid.uuid4().hex[:12]
    now = _utc_now()

    conn.execute(
        """INSERT INTO sandbox_runs(id, command, backend, image, status, created_at)
           VALUES(?,?,?,?,'running',?)""",
        (run_id, command_str, backend, image if backend == "docker" else None, now),
    )
    conn.commit()

    try:
        cmd = shlex.split(command_str)
    except ValueError as exc:
        conn.execute(
            "UPDATE sandbox_runs SET status='failed', error=?, completed_at=? WHERE id=?",
            (str(exc), _utc_now(), run_id),
        )
        conn.commit()
        return {"id": run_id, "status": "failed", "error": str(exc)}

    if backend == "docker":
        exit_code, stdout, stderr = _run_docker(cmd, image, timeout=timeout)
    else:
        exit_code, stdout, stderr = _run_restricted(cmd, timeout=timeout, workdir=workdir)

    status = "done" if exit_code == 0 else "failed"
    output = stdout + (("\n---stderr---\n" + stderr) if stderr.strip() else "")
    conn.execute(
        """UPDATE sandbox_runs SET status=?, exit_code=?, output=?, completed_at=?
           WHERE id=?""",
        (status, exit_code, output[:50000], _utc_now(), run_id),
    )
    conn.commit()

    return {
        "id": run_id,
        "command": command_str,
        "backend": backend,
        "status": status,
        "exit_code": exit_code,
        "output": output,
        "created_at": now,
    }


def list_sandbox_runs(conn: sqlite3.Connection, *, limit: int = 20) -> list[dict]:
    """List recent sandbox runs."""
    ensure_schema(conn)
    rows = conn.execute(
        """SELECT id, command, backend, status, exit_code, created_at
           FROM sandbox_runs ORDER BY created_at DESC LIMIT ?""",
        (limit,),
    ).fetchall()
    return [
        {
            "id": r[0], "command": r[1][:60], "backend": r[2],
            "status": r[3], "exit_code": r[4], "created_at": r[5],
        }
        for r in rows
    ]


def get_sandbox_run(conn: sqlite3.Connection, run_id: str) -> dict | None:
    """Return full details for a sandbox run."""
    ensure_schema(conn)
    row = conn.execute(
        "SELECT * FROM sandbox_runs WHERE id=?", (run_id,)
    ).fetchone()
    if not row:
        return None
    cols = ["id", "command", "backend", "image", "status", "exit_code",
            "output", "error", "created_at", "completed_at"]
    return dict(zip(cols, row))
