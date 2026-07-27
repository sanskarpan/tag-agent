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
import shutil
import sqlite3
import subprocess
import sys
import uuid
from pathlib import Path

BACKENDS = {"restricted", "docker", "modal"}

# ---------------------------------------------------------------------------
# Shell-metacharacter handling
# ---------------------------------------------------------------------------
# `tag sandbox run` takes a COMMAND that is tokenised with shlex and exec'd
# directly — there is no shell in the loop. Pipes, redirections, globs, command
# substitution and `;`/`&&` chains therefore used to be passed through as
# *literal argv words* (e.g. `echo hi > /tmp/x` printed "hi > /tmp/x" and never
# created the file), which silently did something other than what the help text
# ("Shell command to run") promised.
#
# Design decision: REJECT unquoted shell metacharacters instead of spawning
# `/bin/sh -c`. Running a shell would be the more featureful option, but it
# weakens the isolation this backend provides:
#   * on Linux the only confinement is the rlimits installed by preexec_fn on
#     the direct child, and a shell turns one supervised process into an
#     arbitrary process tree with redirections that can write anywhere the
#     user can write — outside the intended workdir;
#   * with --backend docker the chosen image is not guaranteed to contain a
#     shell at all, so behaviour would diverge per image;
#   * exit codes / "command not found" reporting get masked by the shell.
# Rejecting is explicit and preserves the existing (working) isolation. Users
# who need shell features can run `sh -c '<script>'` themselves, which is then
# confined by exactly the same jail as any other command.
_SHELL_METACHARS = "|&;<>()`$*?[]{}!\n\r"


def find_shell_metacharacters(command_str: str) -> list[str]:
    """Return the distinct shell metacharacters that appear *unquoted*.

    Characters inside single or double quotes, or backslash-escaped, are not
    reported: they survive shlex tokenisation as literal text and are handed to
    the program unchanged, which is exactly what a real shell would do too.
    """
    found: list[str] = []
    quote: str | None = None
    escaped = False
    for ch in command_str:
        if escaped:
            escaped = False
            continue
        if quote:
            if ch == quote:
                quote = None
            elif ch == "\\" and quote == '"':
                escaped = True
            continue
        if ch == "\\":
            escaped = True
            continue
        if ch in ("'", '"'):
            quote = ch
            continue
        if ch in _SHELL_METACHARS and ch not in found:
            found.append(ch)
    return found


class ShellMetacharacterError(ValueError):
    """Raised when a command contains unquoted shell metacharacters."""


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
    if timeout <= 0:
        # A non-positive timeout would be passed straight to subprocess.run:
        # 0/negative causes an immediate/way-past-deadline TimeoutExpired, so
        # the command "always times out". Reject it instead.
        return 1, "", f"Invalid timeout {timeout}: must be > 0 seconds"

    env = {
        "PATH": "/usr/bin:/bin:/usr/local/bin",
        "HOME": str(workdir or Path.home()),
    }
    run_dir = str((workdir or Path.cwd()).resolve())

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

    # On macOS there are no rlimit-based namespaces available here, and without
    # any confinement `restricted` was just an unsandboxed host command runner
    # (it could read /etc/passwd and open network sockets). Wrap the command in
    # sandbox-exec with a profile that blocks all network egress and denies
    # read/write of sensitive system locations while still allowing the process
    # to load system libraries and work under its run directory + tmp.
    if sys.platform == "darwin":
        sandbox_exec = shutil.which("sandbox-exec")
        if sandbox_exec:
            # Deny reads of the user's home tree (secrets like ~/.ssh, ~/.aws,
            # browser cookies, keychains) while still allowing the process to
            # read/write its own run directory. The run_dir subpath is allowed
            # AFTER the home denial so a scratch dir under $HOME still works.
            home = str(Path.home())
            sensitive_home = [
                f'{home}/.ssh', f'{home}/.aws', f'{home}/.gnupg',
                f'{home}/.config', f'{home}/.gcloud', f'{home}/.kube',
                f'{home}/.docker', f'{home}/Library/Keychains',
            ]
            deny_home = " ".join(f'(subpath "{p}")' for p in sensitive_home)
            profile = (
                "(version 1)\n"
                "(allow default)\n"
                "(deny network*)\n"
                '(deny file-read* file-write*'
                ' (subpath "/etc") (subpath "/private/etc")'
                ' (subpath "/var/db") (subpath "/private/var/db")'
                ' (literal "/etc/master.passwd") (literal "/private/etc/master.passwd"))\n'
                # Deny reading the user's home tree by default (protects secrets).
                f'(deny file-read* (subpath "{home}"))\n'
                # Explicitly deny sensitive credential dirs for reads AND writes.
                f'(deny file-read* file-write* {deny_home})\n'
                # Re-allow read/write access to the sandbox run directory so the
                # command can operate on its own scratch/working files.
                f'(allow file-read* file-write* (subpath "{run_dir}"))\n'
                '(deny file-write*'
                ' (subpath "/usr") (subpath "/bin") (subpath "/sbin")'
                ' (subpath "/System") (subpath "/Library"))\n'
            )
            command = [sandbox_exec, "-p", profile] + list(command)
        else:
            return (
                127,
                "",
                "sandbox-exec not available: cannot isolate on this platform. "
                "Use --backend docker for isolated execution.",
            )

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
    if timeout <= 0:
        raise ValueError(f"timeout must be > 0 seconds, got {timeout}")

    # Reject shell syntax up-front (see _SHELL_METACHARS above) rather than
    # passing `>`/`|`/`*` to the program as literal arguments. Checked before
    # the audit row is written so a rejected command is not recorded as a run.
    meta = find_shell_metacharacters(command_str)
    if meta:
        raise ShellMetacharacterError(
            "command contains unquoted shell metacharacter(s): "
            + " ".join(repr(m) for m in meta)
            + ". `tag sandbox run` executes a single program directly (no shell), "
            "so pipes, redirections, globs and command substitution are not "
            "interpreted. Quote them to pass them literally, or run them "
            "explicitly via a shell inside the sandbox, e.g.: "
            "tag sandbox run \"sh -c 'echo hi > out.txt'\""
        )

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
        # exit_code is always present so callers can format the result without
        # a KeyError on this early-failure path.
        return {"id": run_id, "command": command_str, "backend": backend,
                "status": "failed", "exit_code": 1, "output": "",
                "error": str(exc)}

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

