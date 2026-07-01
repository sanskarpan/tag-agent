"""PRD-064: SWE-Agent-style structured Bash+Editor harness.

Provides a controlled tool-use environment for LLM agents to perform
software engineering tasks via structured XML action tags.
"""
from __future__ import annotations

import re
import shlex
import sqlite3
import subprocess
import textwrap
import time
import uuid
from dataclasses import dataclass, field
from datetime import datetime, timezone
from pathlib import Path
from typing import Any


_BLOCKED_PATTERNS = [
    re.compile(r'rm\s+-rf\s+/'),
    re.compile(r'\bsudo\b'),
    re.compile(r'mkfs\b'),
    re.compile(r'dd\s+.*of=/dev/'),
    re.compile(r':(){.*}'),  # fork bomb
]

_EXTERNAL_CURL = re.compile(r'(curl|wget)\s+.*https?://')

# Real egress control: any common network tool / URL scheme / raw-socket path,
# not just a two-token curl|wget regex that a python one-liner trivially bypasses.
_EXTERNAL_NET = re.compile(
    r'\b(curl|wget|nc|ncat|netcat|telnet|ssh|scp|sftp|ftp|rsync|socat)\b'
    r'|https?://|ftp://'
    r'|/dev/(tcp|udp)/'
    r'|\b(urllib|urllib2|requests|httpx|http\.client|httplib|socket|smtplib|'
    r'ftplib|aiohttp|websocket|urlopen)\b'
)

# Absolute paths that are safe to reference (interpreters, libraries, system
# binaries) even though they sit outside the working dir. User data locations
# like /etc, /tmp, /var and $HOME are deliberately excluded.
_SAFE_EXEC_PREFIXES = (
    "/usr/bin", "/usr/local/bin", "/usr/sbin", "/bin", "/sbin",
    "/usr/lib", "/usr/libexec", "/System", "/Library", "/opt",
)


@dataclass
class HarnessAction:
    action: str
    args: dict
    result: str | None = None
    error: str | None = None
    exit_code: int | None = None


@dataclass
class HarnessState:
    session_id: str
    task: str
    working_dir: str
    action_history: list[HarnessAction] = field(default_factory=list)
    iteration: int = 0
    max_iterations: int = 30
    done: bool = False
    success: bool = False
    final_answer: str = ""


class SWEHarness:
    def __init__(
        self,
        task: str,
        working_dir: str = ".",
        *,
        max_iterations: int = 30,
        timeout_seconds: int = 30,
        allowed_dirs: list[str] | None = None,
    ) -> None:
        self.task = task
        self.working_dir = Path(working_dir).resolve()
        self.max_iterations = max_iterations
        self.timeout_seconds = timeout_seconds
        self.allowed_dirs = [Path(d).resolve() for d in (allowed_dirs or [])]
        self.state = HarnessState(
            session_id=uuid.uuid4().hex[:12],
            task=task,
            working_dir=str(self.working_dir),
            max_iterations=max_iterations,
        )

    def _is_safe_path(self, path: Path) -> bool:
        try:
            resolved = path.resolve()
        except OSError:
            return False
        # Use true path containment, NOT string-prefix matching: the latter let a
        # sibling directory sharing a name prefix (e.g. /tmp/work-evil vs the
        # working dir /tmp/work) escape the sandbox.
        for base in [self.working_dir, *self.allowed_dirs]:
            try:
                resolved.relative_to(base)
                return True
            except ValueError:
                continue
        return False

    def _is_safe_bash(self, cmd: str) -> bool:
        for pat in _BLOCKED_PATTERNS:
            if pat.search(cmd):
                return False
        # Block outbound network egress via any common tool/library or raw
        # socket path (defense against exfiltration / fetching untrusted
        # payloads) — not just literal `curl http://…`.
        if _EXTERNAL_NET.search(cmd):
            return False
        # Path containment, mirroring view/edit/create: any absolute path or
        # parent-traversal token must resolve inside the working dir (or a
        # known system binary/library location). This rejects reads like
        # `cat /etc/passwd` or `cat /tmp/outside/secret`.
        try:
            tokens = shlex.split(cmd)
        except ValueError:
            return False
        for tok in tokens:
            cand = tok.lstrip('0123456789<>|&')
            if not cand:
                continue
            looks_pathy = (
                cand.startswith('/')
                or cand.startswith('~')
                or '..' in cand.split('/')
            )
            if not looks_pathy:
                continue
            expanded = Path(cand).expanduser()
            if not expanded.is_absolute():
                expanded = self.working_dir / expanded
            if self._is_safe_path(expanded):
                continue
            try:
                resolved = str(expanded.resolve())
            except OSError:
                return False
            if any(
                resolved == p or resolved.startswith(p + "/")
                for p in _SAFE_EXEC_PREFIXES
            ):
                continue
            return False
        return True

    def execute_action(self, action_name: str, args: dict) -> HarnessAction:
        act = HarnessAction(action=action_name, args=args)
        try:
            if action_name == "bash":
                act = self._exec_bash(args, act)
            elif action_name == "view":
                act = self._exec_view(args, act)
            elif action_name == "edit":
                act = self._exec_edit(args, act)
            elif action_name == "create":
                act = self._exec_create(args, act)
            elif action_name == "search":
                act = self._exec_search(args, act)
            elif action_name == "done":
                self.state.done = True
                self.state.success = True
                self.state.final_answer = args.get("answer", "")
                act.result = f"Task marked complete: {self.state.final_answer[:100]}"
                act.exit_code = 0
            else:
                act.error = f"Unknown action: {action_name!r}"
                act.exit_code = 1
        except Exception as e:
            act.error = str(e)
            act.exit_code = 1
        return act

    def _exec_bash(self, args: dict, act: HarnessAction) -> HarnessAction:
        cmd = args.get("command", "")
        if not self._is_safe_bash(cmd):
            act.error = "Command blocked by safety policy"
            act.exit_code = 1
            return act
        try:
            r = subprocess.run(
                cmd, shell=True, capture_output=True, text=True,
                cwd=str(self.working_dir), timeout=self.timeout_seconds,
            )
            output = (r.stdout + r.stderr)[:2000]
            act.result = output or "(no output)"
            act.exit_code = r.returncode
        except subprocess.TimeoutExpired:
            act.error = f"Command timed out after {self.timeout_seconds}s"
            act.exit_code = 124
        return act

    def _exec_view(self, args: dict, act: HarnessAction) -> HarnessAction:
        path = self.working_dir / args.get("path", "")
        if not self._is_safe_path(path):
            act.error = f"Path outside working directory: {path}"
            act.exit_code = 1
            return act
        start = int(args.get("start", 1))
        if start < 1:
            act.error = f"start must be >= 1 (got {start})"
            act.exit_code = 1
            return act
        end = int(args.get("end", start + 99))
        try:
            lines = path.read_text(encoding="utf-8", errors="replace").splitlines()
            selected = lines[start - 1 : end]
            numbered = [f"{i+start:4d}  {ln}" for i, ln in enumerate(selected)]
            act.result = "\n".join(numbered)
            act.exit_code = 0
        except FileNotFoundError:
            act.error = f"File not found: {path}"
            act.exit_code = 1
        return act

    def _exec_edit(self, args: dict, act: HarnessAction) -> HarnessAction:
        path = self.working_dir / args.get("path", "")
        if not self._is_safe_path(path):
            act.error = f"Path outside working directory: {path}"
            act.exit_code = 1
            return act
        start = int(args.get("start", 1))
        if start < 1:
            act.error = f"start must be >= 1 (got {start})"
            act.exit_code = 1
            return act
        end = int(args.get("end", start))
        new_content = args.get("content", "")
        try:
            lines = path.read_text(encoding="utf-8", errors="replace").splitlines(keepends=True)
            lines[start - 1 : end] = [new_content + ("\n" if not new_content.endswith("\n") else "")]
            path.write_text("".join(lines), encoding="utf-8")
            act.result = f"Edited {path.name} lines {start}-{end}"
            act.exit_code = 0
        except Exception as e:
            act.error = str(e)
            act.exit_code = 1
        return act

    def _exec_create(self, args: dict, act: HarnessAction) -> HarnessAction:
        path = self.working_dir / args.get("path", "")
        if not self._is_safe_path(path):
            act.error = f"Path outside working directory: {path}"
            act.exit_code = 1
            return act
        content = args.get("content", "")
        try:
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(content, encoding="utf-8")
            act.result = f"Created {path}"
            act.exit_code = 0
        except Exception as e:
            act.error = str(e)
            act.exit_code = 1
        return act

    def _exec_search(self, args: dict, act: HarnessAction) -> HarnessAction:
        pattern = args.get("pattern", "")
        search_dir = self.working_dir / args.get("dir", ".")
        try:
            r = subprocess.run(
                ["grep", "-r", "--include=*.py", "--include=*.js", "--include=*.ts",
                 "-n", pattern, str(search_dir)],
                capture_output=True, text=True, timeout=15,
            )
            act.result = (r.stdout or "(no matches)")[:2000]
            act.exit_code = r.returncode
        except Exception as e:
            act.error = str(e)
            act.exit_code = 1
        return act

    def build_system_prompt(self) -> str:
        return textwrap.dedent(f"""\
            You are a software engineering agent. Solve the task below by using the available tools.

            Working directory: {self.working_dir}

            # Task
            {self.task}

            # Available Actions (use XML tags)

            <bash>command here</bash>
              Run a shell command. Output truncated to 2000 chars.

            <view path="file.py" start="1" end="50"/>
              View file lines (default: first 100 lines).

            <edit path="file.py" start="5" end="10">
            new content to replace lines 5-10
            </edit>

            <create path="new_file.py">
            file content here
            </create>

            <search pattern="def my_func" dir="src/"/>
              Grep for pattern recursively.

            <done>
            Your final answer or summary of changes made.
            </done>

            After each action, you will receive the result. Continue until done.
        """)

    def parse_agent_response(self, response: str) -> list[tuple[str, dict]]:
        actions: list[tuple[str, dict]] = []

        # <bash>...</bash>
        for m in re.finditer(r'<bash>(.*?)</bash>', response, re.DOTALL):
            actions.append(("bash", {"command": m.group(1).strip()}))

        # <view path="..." start="N" end="N"/>
        for m in re.finditer(r'<view\s+([^/]+)/>', response, re.DOTALL):
            attrs = _parse_attrs(m.group(1))
            actions.append(("view", attrs))

        # <view path="...">...</view>
        for m in re.finditer(r'<view\s+([^>]+)>(.*?)</view>', response, re.DOTALL):
            attrs = _parse_attrs(m.group(1))
            actions.append(("view", attrs))

        # <edit path="..." start="N" end="N">...</edit>
        for m in re.finditer(r'<edit\s+([^>]+)>(.*?)</edit>', response, re.DOTALL):
            attrs = _parse_attrs(m.group(1))
            attrs["content"] = m.group(2)
            actions.append(("edit", attrs))

        # <create path="...">...</create>
        for m in re.finditer(r'<create\s+([^>]+)>(.*?)</create>', response, re.DOTALL):
            attrs = _parse_attrs(m.group(1))
            attrs["content"] = m.group(2)
            actions.append(("create", attrs))

        # <search pattern="..." dir="..."/>
        for m in re.finditer(r'<search\s+([^/]+)/>', response, re.DOTALL):
            attrs = _parse_attrs(m.group(1))
            actions.append(("search", attrs))

        # <done>...</done>
        for m in re.finditer(r'<done>(.*?)</done>', response, re.DOTALL):
            actions.append(("done", {"answer": m.group(1).strip()}))

        return actions

    def run_iteration(self, agent_output: str) -> list[HarnessAction]:
        parsed = self.parse_agent_response(agent_output)
        results: list[HarnessAction] = []
        for action_name, args in parsed:
            act = self.execute_action(action_name, args)
            self.state.action_history.append(act)
            results.append(act)
            if self.state.done:
                break
        return results

    def build_context_for_next(self, last_actions: list[HarnessAction]) -> str:
        lines = ["# Action Results"]
        for act in last_actions:
            lines.append(f"\n## {act.action.upper()}")
            if act.result:
                lines.append(f"```\n{act.result[:1000]}\n```")
            if act.error:
                lines.append(f"ERROR: {act.error}")
            if act.exit_code is not None and act.exit_code != 0:
                lines.append(f"Exit code: {act.exit_code}")
        return "\n".join(lines)

    def format_history(self, *, max_iterations: int = 5) -> str:
        recent = self.state.action_history[-max_iterations * 3:]
        lines = []
        for act in recent:
            lines.append(f"[{act.action}] {json_safe(act.args)}")
            if act.result:
                lines.append(f"  -> {act.result[:200]}")
        return "\n".join(lines)


def _parse_attrs(attrs_str: str) -> dict:
    result: dict = {}
    for m in re.finditer(r'(\w+)=["\']([^"\']*)["\']', attrs_str):
        result[m.group(1)] = m.group(2)
    return result


def json_safe(obj: Any) -> str:
    import json
    try:
        return json.dumps(obj)[:100]
    except Exception:
        return str(obj)[:100]


def ensure_schema(conn: sqlite3.Connection) -> None:
    conn.executescript("""
        CREATE TABLE IF NOT EXISTS swe_sessions (
            id              TEXT PRIMARY KEY,
            task            TEXT NOT NULL,
            working_dir     TEXT NOT NULL,
            status          TEXT NOT NULL DEFAULT 'running',
            iterations_used INTEGER NOT NULL DEFAULT 0,
            final_answer    TEXT,
            cost_usd        REAL,
            created_at      TEXT NOT NULL,
            completed_at    TEXT
        );
    """)
    conn.commit()


def run_swe_session(
    task: str,
    profile: str,
    cfg: dict,
    working_dir: str = ".",
    *,
    max_iterations: int = 30,
    conn: sqlite3.Connection | None = None,
) -> HarnessState:
    import shutil
    harness = SWEHarness(task, working_dir, max_iterations=max_iterations)
    session_id = harness.state.session_id
    now = datetime.now(timezone.utc).isoformat()

    if conn:
        ensure_schema(conn)
        conn.execute(
            "INSERT INTO swe_sessions(id,task,working_dir,status,created_at) VALUES(?,?,?,?,?)",
            (session_id, task, working_dir, "running", now),
        )
        conn.commit()

    tag_bin = shutil.which("tag") or shutil.which("tag-agent") or "tag"
    system_prompt = harness.build_system_prompt()
    current_prompt = system_prompt

    for i in range(max_iterations):
        harness.state.iteration = i + 1
        try:
            r = subprocess.run(
                [tag_bin, "-q", current_prompt, "-p", profile],
                capture_output=True, text=True, timeout=120,
            )
            agent_output = r.stdout
        except Exception as e:
            harness.state.final_answer = f"Error: {e}"
            harness.state.done = True
            break

        actions_done = harness.run_iteration(agent_output)
        if harness.state.done:
            break

        context = harness.build_context_for_next(actions_done)
        current_prompt = f"{system_prompt}\n\n{context}\n\nContinue solving the task."

    if conn:
        completed = datetime.now(timezone.utc).isoformat()
        conn.execute(
            """UPDATE swe_sessions SET status=?,iterations_used=?,final_answer=?,completed_at=?
               WHERE id=?""",
            ("done" if harness.state.success else "incomplete",
             harness.state.iteration, harness.state.final_answer, completed, session_id),
        )
        conn.commit()

    return harness.state
