"""PRD-023: Context-centric multi-agent swarm orchestration.

Coordinator partitions a goal into context slices and routes each slice to an
isolated sub-agent subprocess. Results are aggregated via a SQLite context bus
with write-once, per-agent permission enforcement.
"""
from __future__ import annotations

import json
import os
import signal
import sqlite3
import subprocess
import sys
import tempfile
import time
import uuid
from concurrent.futures import ThreadPoolExecutor, as_completed
from pathlib import Path
from typing import Any

# Hard limit — non-negotiable
SWARM_MAX_AGENTS = 10

# ---------------------------------------------------------------------------
# JSON schema for coordinator manifest (inline, no jsonschema dep required)
# ---------------------------------------------------------------------------

_REQUIRED_MANIFEST_KEYS = {"swarm_id", "goal", "tasks"}
_REQUIRED_TASK_KEYS = {"task_id", "description", "context_slice", "profile"}
_VALID_SLICE_TYPES = {"file_paths", "directory", "url_list", "key_list", "free_text"}
_VALID_FAILURE_POLICIES = {"abort_on_any", "best_effort", "require_majority"}


class SwarmManifestError(Exception):
    """Raised when the coordinator output is not a valid task manifest."""


def _validate_manifest(manifest: dict, max_agents: int) -> None:
    if not isinstance(manifest, dict):
        raise SwarmManifestError("Manifest must be a JSON object")
    missing = _REQUIRED_MANIFEST_KEYS - manifest.keys()
    if missing:
        raise SwarmManifestError(f"Manifest missing required keys: {missing}")
    tasks = manifest["tasks"]
    if not isinstance(tasks, list) or not tasks:
        raise SwarmManifestError("Manifest 'tasks' must be a non-empty array")
    if len(tasks) > max_agents:
        raise SwarmManifestError(
            f"Manifest has {len(tasks)} tasks but max_agents={max_agents}"
        )
    seen_ids: set[str] = set()
    all_selectors: list[Any] = []
    for t in tasks:
        missing_t = _REQUIRED_TASK_KEYS - t.keys()
        if missing_t:
            raise SwarmManifestError(f"Task missing keys {missing_t}: {t}")
        tid = t["task_id"]
        if not isinstance(tid, str) or not tid.replace("_", "").replace("-", "").isalnum():
            raise SwarmManifestError(f"task_id must be alphanumeric+underscore+dash: {tid!r}")
        if tid in seen_ids:
            raise SwarmManifestError(f"Duplicate task_id: {tid}")
        seen_ids.add(tid)
        cs = t.get("context_slice", {})
        if cs.get("type") not in _VALID_SLICE_TYPES:
            raise SwarmManifestError(f"Invalid context_slice.type for task {tid}")
        sel = cs.get("selector", [])
        if isinstance(sel, list):
            for s in sel:
                if s in all_selectors:
                    raise SwarmManifestError(
                        f"Overlapping context_slice selector {s!r} in task {tid}"
                    )
                all_selectors.append(s)
    # validate dependency graph: referential integrity + acyclicity
    dep_map = {t["task_id"]: set(t.get("depends_on", [])) for t in tasks}
    for tid, deps in dep_map.items():
        unknown = deps - seen_ids
        if unknown:
            raise SwarmManifestError(
                f"Task {tid!r} depends_on unknown task_id(s): {sorted(unknown)}"
            )
    _assert_acyclic(dep_map)


def _assert_acyclic(dep_map: dict[str, set[str]]) -> None:
    visited: set[str] = set()
    in_stack: set[str] = set()

    def dfs(node: str) -> None:
        visited.add(node)
        in_stack.add(node)
        for dep in dep_map.get(node, set()):
            if dep in in_stack:
                raise SwarmManifestError(f"Dependency cycle detected involving task {node!r}")
            if dep not in visited:
                dfs(dep)
        in_stack.discard(node)

    for node in dep_map:
        if node not in visited:
            dfs(node)


# ---------------------------------------------------------------------------
# SwarmDB — thin SQLite wrapper for swarm tables
# ---------------------------------------------------------------------------

_SWARM_SCHEMA = """
CREATE TABLE IF NOT EXISTS swarm_runs (
    swarm_id               TEXT PRIMARY KEY,
    goal                   TEXT NOT NULL,
    coordinator_profile    TEXT NOT NULL,
    failure_policy         TEXT NOT NULL DEFAULT 'best_effort',
    status                 TEXT NOT NULL DEFAULT 'pending'
                           CHECK(status IN ('pending','running','completed','partial','failed','aborted')),
    max_agents             INTEGER NOT NULL DEFAULT 4,
    started_at             TEXT,
    completed_at           TEXT,
    total_tokens_prompt    INTEGER DEFAULT 0,
    total_tokens_completion INTEGER DEFAULT 0,
    total_cost_usd         REAL DEFAULT 0.0,
    task_count             INTEGER DEFAULT 0,
    final_output           TEXT,
    manifest_json          TEXT,
    created_at             TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
);

CREATE TABLE IF NOT EXISTS swarm_tasks (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    swarm_id            TEXT NOT NULL REFERENCES swarm_runs(swarm_id),
    task_id             TEXT NOT NULL,
    profile             TEXT NOT NULL,
    description         TEXT,
    context_slice_json  TEXT,
    status              TEXT NOT NULL DEFAULT 'pending'
                        CHECK(status IN ('pending','running','done','failed','timed_out','skipped','memory_limit_exceeded')),
    pid                 INTEGER,
    started_at          TEXT,
    completed_at        TEXT,
    tokens_prompt       INTEGER DEFAULT 0,
    tokens_completion   INTEGER DEFAULT 0,
    cost_usd            REAL DEFAULT 0.0,
    model               TEXT,
    output              TEXT,
    error_message       TEXT,
    artifacts_json      TEXT,
    UNIQUE(swarm_id, task_id)
);

CREATE TABLE IF NOT EXISTS swarm_context (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    swarm_id    TEXT NOT NULL REFERENCES swarm_runs(swarm_id),
    key         TEXT NOT NULL,
    value       TEXT NOT NULL,
    value_type  TEXT NOT NULL CHECK(value_type IN ('string','number','boolean','json_object','json_array')),
    written_by  TEXT NOT NULL,
    written_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now')),
    schema_hint TEXT,
    UNIQUE(swarm_id, key)
);

CREATE INDEX IF NOT EXISTS idx_swarm_tasks_swarm_id ON swarm_tasks(swarm_id);
CREATE INDEX IF NOT EXISTS idx_swarm_ctx_swarm_key ON swarm_context(swarm_id, key);
"""


def migrate_swarm_tables(conn: sqlite3.Connection) -> None:
    conn.executescript(_SWARM_SCHEMA)
    conn.commit()


def _now_iso() -> str:
    import datetime
    return datetime.datetime.now(datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%S.%f")[:-3] + "Z"


# ---------------------------------------------------------------------------
# ContextBus
# ---------------------------------------------------------------------------

_VALID_VALUE_TYPES = {"string", "number", "boolean", "json_object", "json_array"}


class ContextBus:
    def __init__(self, conn: sqlite3.Connection, swarm_id: str) -> None:
        self._conn = conn
        self._swarm_id = swarm_id

    def write(
        self,
        key: str,
        value: Any,
        value_type: str,
        written_by: str,
        permitted_keys: list[str],
        schema_hint: str | None = None,
    ) -> bool:
        if key not in permitted_keys:
            return False
        if value_type not in _VALID_VALUE_TYPES:
            return False
        # Validate value matches declared type
        try:
            encoded = json.dumps(value)
            decoded = json.loads(encoded)
            if value_type == "string" and not isinstance(decoded, str):
                return False
            if value_type == "number" and not isinstance(decoded, (int, float)):
                return False
            if value_type == "boolean" and not isinstance(decoded, bool):
                return False
            if value_type == "json_object" and not isinstance(decoded, dict):
                return False
            if value_type == "json_array" and not isinstance(decoded, list):
                return False
        except (TypeError, ValueError):
            return False

        # Write-once: check if key exists from a different writer
        existing = self._conn.execute(
            "SELECT written_by FROM swarm_context WHERE swarm_id=? AND key=?",
            (self._swarm_id, key),
        ).fetchone()
        if existing:
            if existing[0] != written_by:
                return False  # silently reject — different writer owns this key
            # Same writer can update their own key
        try:
            self._conn.execute(
                """INSERT INTO swarm_context(swarm_id, key, value, value_type, written_by, written_at, schema_hint)
                   VALUES(?,?,?,?,?,?,?)
                   ON CONFLICT(swarm_id, key) DO UPDATE SET
                     value=excluded.value, written_at=excluded.written_at
                   WHERE written_by=excluded.written_by""",
                (self._swarm_id, key, encoded, value_type, written_by, _now_iso(), schema_hint),
            )
            self._conn.commit()
            return True
        except sqlite3.Error:
            return False

    def read_snapshot(self, permitted_keys: list[str]) -> dict[str, dict]:
        if not permitted_keys:
            return {}
        placeholders = ",".join("?" * len(permitted_keys))
        rows = self._conn.execute(
            f"SELECT key, value, value_type, written_by FROM swarm_context "
            f"WHERE swarm_id=? AND key IN ({placeholders})",
            (self._swarm_id, *permitted_keys),
        ).fetchall()
        return {
            r[0]: {"value": json.loads(r[1]), "value_type": r[2], "written_by": r[3]}
            for r in rows
        }

    def full_audit(self) -> list[dict]:
        rows = self._conn.execute(
            "SELECT key, value, value_type, written_by, written_at FROM swarm_context "
            "WHERE swarm_id=? ORDER BY written_at",
            (self._swarm_id,),
        ).fetchall()
        return [
            {"key": r[0], "value": json.loads(r[1]), "value_type": r[2],
             "written_by": r[3], "written_at": r[4]}
            for r in rows
        ]


# ---------------------------------------------------------------------------
# SwarmCoordinator
# ---------------------------------------------------------------------------

_COORDINATOR_SYSTEM_PROMPT = """\
You are a task coordinator for a multi-agent system. Your sole output must be a
valid JSON object matching the schema below. Do not output any prose, markdown
fences, or explanatory text outside the JSON object.

Rules for context_slice assignment:
1. Each task must have a non-overlapping context slice. Two tasks cannot share
   file paths, directories, or URL domains in their selectors.
2. Assign tasks based on context ownership — which agent "owns" that domain of
   knowledge or files — not by task type alone.
3. Do not create more than {max_agents} tasks. Fewer is usually better.
4. context_bus_writes must be minimal — only keys downstream tasks genuinely need.
5. task_id values must be lowercase alphanumeric with underscores/dashes only.

Goal: {goal}
Available profiles: {profiles_list}
Swarm ID: {swarm_id}

Output exactly one JSON object with this shape:
{{
  "swarm_id": "{swarm_id}",
  "goal": "<the goal>",
  "tasks": [
    {{
      "task_id": "unique_snake_case_id",
      "description": "What this agent should do",
      "context_slice": {{
        "type": "file_paths|directory|url_list|key_list|free_text",
        "selector": ["path/or/url", ...] or "free text description"
      }},
      "profile": "profile_name_from_config",
      "depends_on": [],
      "context_bus_reads": [],
      "context_bus_writes": []
    }}
  ],
  "failure_policy": "best_effort",
  "synthesis_profile": "profile_name_or_null"
}}
"""


class SwarmCoordinator:
    def __init__(self, cfg: dict, profile: str) -> None:
        self._cfg = cfg
        self._profile = profile

    def produce_manifest(self, goal: str, swarm_id: str, max_agents: int) -> dict:
        profiles_list = ", ".join(self._cfg.get("profiles", {}).keys())
        system_prompt = _COORDINATOR_SYSTEM_PROMPT.format(
            max_agents=max_agents,
            goal=goal,
            profiles_list=profiles_list,
            swarm_id=swarm_id,
        )
        # Try to invoke the coordinator profile subprocess
        manifest_json = self._invoke_coordinator(system_prompt, goal)
        # Retry once on JSON parse failure
        if manifest_json is None:
            manifest_json = self._invoke_coordinator(system_prompt, goal)
        if manifest_json is None:
            raise SwarmManifestError("Coordinator produced no usable JSON output after 2 attempts")
        try:
            manifest = json.loads(manifest_json)
        except json.JSONDecodeError as exc:
            raise SwarmManifestError(f"Coordinator output is not valid JSON: {exc}") from exc
        _validate_manifest(manifest, max_agents)
        # Normalise: set swarm_id and default failure_policy
        manifest["swarm_id"] = swarm_id
        manifest.setdefault("failure_policy", "best_effort")
        return manifest

    def _invoke_coordinator(self, system_prompt: str, goal: str) -> str | None:
        try:
            from tag.controller import hermes_bin, profile_exec_env, ensure_runtime_dirs  # noqa: PLC0415
        except ImportError:
            return None
        try:
            ensure_runtime_dirs(self._cfg)
            env = profile_exec_env(self._cfg, self._profile)
            proc = subprocess.run(
                [str(hermes_bin(self._cfg)), "chat", "-q",
                 f"{system_prompt}\n\nUser: Decompose the goal into a task manifest now.", "-Q"],
                env=env,
                text=True,
                capture_output=True,
                timeout=120,
            )
            output = proc.stdout.strip()
        except (subprocess.TimeoutExpired, subprocess.SubprocessError, OSError):
            return None
        # Strip markdown fences
        if output.startswith("```"):
            lines = output.splitlines()
            if len(lines) >= 3:
                output = "\n".join(lines[1:-1]).strip()
        # Extract first {...} block
        start = output.find("{")
        end = output.rfind("}")
        if start != -1 and end != -1 and end > start:
            return output[start:end + 1]
        return None


# ---------------------------------------------------------------------------
# SwarmRunner
# ---------------------------------------------------------------------------

_SECURE_TMP_SUBDIR = ".tag/tmp"


def _secure_tmp_dir() -> Path:
    d = Path.home() / _SECURE_TMP_SUBDIR
    d.mkdir(parents=True, exist_ok=True)
    d.chmod(0o700)
    return d


def _build_agent_env(cfg: dict, profile: str) -> dict[str, str]:
    safe_keys = {"PATH", "HOME", "TMPDIR", "TEMP", "TMP", "LANG", "LC_ALL", "TERM"}
    env: dict[str, str] = {k: v for k, v in os.environ.items() if k in safe_keys}
    profiles = cfg.get("profiles", {})
    prof_cfg = profiles.get(profile, {})
    env_block = prof_cfg.get("env", {})
    for k, v in env_block.items():
        env[str(k)] = str(v)
    return env


class TaskResult:
    def __init__(
        self,
        task_id: str,
        status: str,
        output: str = "",
        error_message: str = "",
        tokens_prompt: int = 0,
        tokens_completion: int = 0,
        cost_usd: float = 0.0,
        model: str = "",
        artifacts: list | None = None,
    ) -> None:
        self.task_id = task_id
        self.status = status
        self.output = output
        self.error_message = error_message
        self.tokens_prompt = tokens_prompt
        self.tokens_completion = tokens_completion
        self.cost_usd = cost_usd
        self.model = model
        self.artifacts = artifacts or []


class SwarmRunner:
    def __init__(
        self,
        cfg: dict,
        manifest: dict,
        bus: ContextBus,
        conn: sqlite3.Connection,
        swarm_id: str,
        max_agents: int = 4,
        timeout_per_agent: int = 300,
        failure_policy: str = "best_effort",
        parallel: bool = True,
        approve: bool = False,
    ) -> None:
        self._cfg = cfg
        self._manifest = manifest
        self._bus = bus
        self._conn = conn
        self._swarm_id = swarm_id
        self._max_agents = min(max_agents, SWARM_MAX_AGENTS)
        self._timeout = timeout_per_agent
        self._failure_policy = failure_policy
        self._parallel = parallel
        self._approve = approve
        self._aborted = False

    def run(self) -> dict[str, Any]:
        tasks = self._manifest["tasks"]
        synthesis_profile = self._manifest.get("synthesis_profile") or self._manifest.get("coordinator_profile")

        # Build dependency map and topological order
        dep_map = {t["task_id"]: set(t.get("depends_on", [])) for t in tasks}
        task_by_id = {t["task_id"]: t for t in tasks}
        completed_ids: set[str] = set()
        failed_ids: set[str] = set()
        results: list[TaskResult] = []

        # Update swarm to running
        self._conn.execute(
            "UPDATE swarm_runs SET status='running', started_at=? WHERE swarm_id=?",
            (_now_iso(), self._swarm_id),
        )
        self._conn.commit()

        # Wave-based execution
        remaining = set(task_by_id.keys())
        while remaining and not self._aborted:
            ready = {
                tid for tid in remaining
                if dep_map[tid].issubset(completed_ids | failed_ids)
            }
            if not ready:
                # No task's dependencies can ever be satisfied (unknown dep,
                # cycle, or unmet dependency). Strand them visibly as 'skipped'
                # rather than silently dropping them so the run cannot report
                # 'completed' with missing tasks (B049).
                for tid in remaining:
                    self._set_task_status(tid, "skipped", "dependencies unsatisfiable")
                    results.append(TaskResult(
                        tid, "skipped",
                        error_message="dependencies unsatisfiable",
                    ))
                remaining = set()
                break
            wave = list(ready)[: self._max_agents]
            wave_results = self._run_wave(wave, task_by_id)
            for r in wave_results:
                results.append(r)
                if r.status in ("done", "partial"):
                    completed_ids.add(r.task_id)
                else:
                    failed_ids.add(r.task_id)
                    if self._failure_policy == "abort_on_any":
                        self._aborted = True
                        break
            remaining -= ready

        if self._aborted:
            self._conn.execute(
                "UPDATE swarm_runs SET status='failed', completed_at=? WHERE swarm_id=?",
                (_now_iso(), self._swarm_id),
            )
            self._conn.commit()
            return {"status": "failed", "results": [vars(r) for r in results]}

        # Apply failure policy
        successful = [r for r in results if r.status in ("done", "partial")]
        n_total = len(results)
        n_ok = len(successful)

        if self._failure_policy == "require_majority" and n_ok <= n_total / 2:
            self._conn.execute(
                "UPDATE swarm_runs SET status='failed', completed_at=? WHERE swarm_id=?",
                (_now_iso(), self._swarm_id),
            )
            self._conn.commit()
            return {"status": "failed", "results": [vars(r) for r in results]}

        # Synthesize
        final_output = self._synthesize(successful, synthesis_profile)
        final_status = "completed" if n_ok == n_total else "partial"

        total_prompt = sum(r.tokens_prompt for r in results)
        total_completion = sum(r.tokens_completion for r in results)
        total_cost = sum(r.cost_usd for r in results)

        self._conn.execute(
            """UPDATE swarm_runs SET status=?, completed_at=?, final_output=?,
               total_tokens_prompt=?, total_tokens_completion=?, total_cost_usd=?
               WHERE swarm_id=?""",
            (final_status, _now_iso(), final_output,
             total_prompt, total_completion, total_cost, self._swarm_id),
        )
        self._conn.commit()
        return {"status": final_status, "final_output": final_output, "results": [vars(r) for r in results]}

    def _run_wave(self, task_ids: list[str], task_by_id: dict) -> list[TaskResult]:
        if self._approve:
            approved = []
            for tid in task_ids:
                t = task_by_id[tid]
                print(f"\n[swarm] Task: {tid}")
                print(f"  Description: {t['description'][:120]}")
                print(f"  Profile:     {t['profile']}")
                print(f"  Context:     {t['context_slice'].get('type')} → {t['context_slice'].get('selector')}")
                ans = input("Dispatch? [y/N/skip] ").strip().lower()
                if ans == "y":
                    approved.append(tid)
                elif ans == "skip":
                    self._conn.execute(
                        "UPDATE swarm_tasks SET status='skipped' WHERE swarm_id=? AND task_id=?",
                        (self._swarm_id, tid),
                    )
                    self._conn.commit()
                else:
                    self._aborted = True
                    return []
            task_ids = approved

        if self._parallel:
            with ThreadPoolExecutor(max_workers=self._max_agents) as ex:
                futures = {ex.submit(self._run_task, task_by_id[tid]): tid for tid in task_ids}
                wave_results = []
                for f in as_completed(futures):
                    wave_results.append(f.result())
            return wave_results
        else:
            return [self._run_task(task_by_id[tid]) for tid in task_ids]

    def _run_task(self, task: dict) -> TaskResult:
        tid = task["task_id"]
        profile = task.get("profile", self._manifest.get("coordinator_profile", ""))
        tmp = _secure_tmp_dir()

        # Prepare context bus snapshot
        permitted_reads = task.get("context_bus_reads", [])
        snapshot = self._bus.read_snapshot(permitted_reads)

        # Write temp files
        ctx_out_path = tmp / f"swarm_{self._swarm_id}_{tid}_ctx_out.json"
        result_path = tmp / f"swarm_{self._swarm_id}_{tid}_result.json"
        input_data = {
            "task_id": tid,
            "swarm_id": self._swarm_id,
            "description": task["description"],
            "context_slice": task["context_slice"],
            "context_bus_snapshot": snapshot,
            "context_bus_output_path": str(ctx_out_path),
            "result_output_path": str(result_path),
        }
        input_path = tmp / f"swarm_{self._swarm_id}_{tid}_input.json"
        input_path.write_text(json.dumps(input_data))
        input_path.chmod(0o600)

        env = _build_agent_env(self._cfg, profile)
        env["TAG_SWARM_TASK_INPUT"] = str(input_path)
        env["TAG_CONTEXT_BUS_OUTPUT"] = str(ctx_out_path)
        env["TAG_SWARM_RESULT_OUTPUT"] = str(result_path)
        env["TAG_SWARM_PROFILE"] = profile
        env["TAG_SWARM_TIMEOUT"] = str(self._timeout)
        # Hand the resolved runtime binary to the sub-agent so it need not
        # re-load config (swarm_agent_entry reads TAG_HERMES_BIN) — B009.
        try:
            from tag.controller import hermes_bin as _hermes_bin_fn  # noqa: PLC0415
            env["TAG_HERMES_BIN"] = str(_hermes_bin_fn(self._cfg))
        except Exception:
            pass

        # Update DB: running
        self._conn.execute(
            """UPDATE swarm_tasks SET status='running', started_at=?, profile=?,
               description=?, context_slice_json=? WHERE swarm_id=? AND task_id=?""",
            (_now_iso(), profile, task["description"],
             json.dumps(task["context_slice"]), self._swarm_id, tid),
        )
        self._conn.commit()

        # Spawn subprocess
        try:
            from tag.controller import hermes_bin, ensure_runtime_dirs  # noqa: PLC0415
            ensure_runtime_dirs(self._cfg)
            cmd = [sys.executable, "-m", "tag.swarm_agent_entry"]
            proc = subprocess.Popen(
                cmd, env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                start_new_session=True,
            )
            self._conn.execute(
                "UPDATE swarm_tasks SET pid=? WHERE swarm_id=? AND task_id=?",
                (proc.pid, self._swarm_id, tid),
            )
            self._conn.commit()
        except Exception as exc:
            return self._fail_task(tid, str(exc))

        # Wait with timeout
        start = time.monotonic()
        while True:
            try:
                stdout, stderr = proc.communicate(timeout=min(5.0, self._timeout))
                break
            except subprocess.TimeoutExpired:
                elapsed = time.monotonic() - start
                if elapsed >= self._timeout:
                    self._kill_proc(proc)
                    self._set_task_status(tid, "timed_out",
                                          f"exceeded timeout of {self._timeout}s")
                    return TaskResult(tid, "failed",
                                      error_message=f"Timeout after {self._timeout}s")

        completed_at = _now_iso()

        # Parse result envelope
        if result_path.exists():
            try:
                envelope = json.loads(result_path.read_text())
                result = TaskResult(
                    task_id=tid,
                    status=envelope.get("status", "failure"),
                    output=envelope.get("output", ""),
                    error_message=envelope.get("error_message") or "",
                    tokens_prompt=int(envelope.get("tokens_prompt") or 0),
                    tokens_completion=int(envelope.get("tokens_completion") or 0),
                    cost_usd=float(envelope.get("cost_usd") or 0.0),
                    model=envelope.get("model") or "",
                    artifacts=envelope.get("artifacts") or [],
                )
                if result.status == "success":
                    result.status = "done"
            except Exception:
                result = TaskResult(tid, "failed",
                                    error_message=stderr.decode("utf-8", errors="replace")[:2000])
        else:
            result = TaskResult(tid, "failed",
                                error_message=stderr.decode("utf-8", errors="replace")[:2000])

        # Apply context bus outputs
        if ctx_out_path.exists():
            try:
                ctx_data = json.loads(ctx_out_path.read_text())
                permitted_writes = task.get("context_bus_writes", [])
                for k, v in (ctx_data.items() if isinstance(ctx_data, dict) else []):
                    vt = "string" if isinstance(v, str) else \
                         "number" if isinstance(v, (int, float)) and not isinstance(v, bool) else \
                         "boolean" if isinstance(v, bool) else \
                         "json_object" if isinstance(v, dict) else \
                         "json_array" if isinstance(v, list) else "string"
                    self._bus.write(k, v, vt, tid, permitted_writes)
            except Exception:
                pass

        # Update DB
        db_status = result.status if result.status in (
            "done", "failed", "timed_out", "skipped", "memory_limit_exceeded"
        ) else "failed"
        self._conn.execute(
            """UPDATE swarm_tasks SET status=?, completed_at=?, output=?, error_message=?,
               tokens_prompt=?, tokens_completion=?, cost_usd=?, model=?
               WHERE swarm_id=? AND task_id=?""",
            (db_status, completed_at, result.output[:10000], result.error_message[:2000],
             result.tokens_prompt, result.tokens_completion, result.cost_usd, result.model,
             self._swarm_id, tid),
        )
        # Update run totals
        self._conn.execute(
            """UPDATE swarm_runs SET
               total_tokens_prompt = total_tokens_prompt + ?,
               total_tokens_completion = total_tokens_completion + ?,
               total_cost_usd = total_cost_usd + ?
               WHERE swarm_id=?""",
            (result.tokens_prompt, result.tokens_completion, result.cost_usd, self._swarm_id),
        )
        self._conn.commit()

        # Cleanup temp files
        for p in (input_path, ctx_out_path, result_path):
            try:
                p.unlink(missing_ok=True)
            except Exception:
                pass

        return result

    def _fail_task(self, task_id: str, error: str) -> TaskResult:
        self._set_task_status(task_id, "failed", error)
        return TaskResult(task_id, "failed", error_message=error)

    def _set_task_status(self, task_id: str, status: str, error: str = "") -> None:
        self._conn.execute(
            "UPDATE swarm_tasks SET status=?, completed_at=?, error_message=? WHERE swarm_id=? AND task_id=?",
            (status, _now_iso(), error, self._swarm_id, task_id),
        )
        self._conn.commit()

    def _kill_proc(self, proc: subprocess.Popen) -> None:
        try:
            pgid = os.getpgid(proc.pid)
            os.killpg(pgid, signal.SIGTERM)
            time.sleep(5)
            try:
                os.killpg(pgid, signal.SIGKILL)
            except ProcessLookupError:
                pass
        except (ProcessLookupError, PermissionError, AttributeError):
            try:
                proc.kill()
            except Exception:
                pass

    def _synthesize(self, successful: list[TaskResult], synthesis_profile: str | None) -> str:
        if not successful:
            return ""
        if len(successful) == 1:
            return successful[0].output

        # Build synthesis prompt
        parts = [f"Goal: {self._manifest['goal']}\n\nAgent results:\n"]
        for r in successful:
            parts.append(f"--- {r.task_id} ---\n{r.output}\n")
        prompt = "\n".join(parts) + "\nSynthesize the above results into a comprehensive final answer."

        if not synthesis_profile:
            return prompt  # fallback: concatenate

        try:
            from tag.controller import hermes_bin, profile_exec_env, ensure_runtime_dirs  # noqa: PLC0415
            ensure_runtime_dirs(self._cfg)
            env = profile_exec_env(self._cfg, synthesis_profile)
            proc = subprocess.run(
                [str(hermes_bin(self._cfg)), "chat", "-q", prompt, "-Q"],
                env=env, text=True, capture_output=True, timeout=120,
            )
            return proc.stdout.strip() or "\n".join(r.output for r in successful)
        except Exception:
            return "\n\n---\n\n".join(r.output for r in successful)


# ---------------------------------------------------------------------------
# Public helpers used by controller.py
# ---------------------------------------------------------------------------

def create_swarm_run(
    conn: sqlite3.Connection,
    swarm_id: str,
    goal: str,
    coordinator_profile: str,
    failure_policy: str,
    max_agents: int,
) -> None:
    conn.execute(
        """INSERT INTO swarm_runs(swarm_id, goal, coordinator_profile, failure_policy, max_agents, created_at)
           VALUES(?,?,?,?,?,?)""",
        (swarm_id, goal, coordinator_profile, failure_policy, max_agents, _now_iso()),
    )
    conn.commit()


def insert_swarm_tasks(conn: sqlite3.Connection, swarm_id: str, tasks: list[dict]) -> None:
    for t in tasks:
        conn.execute(
            """INSERT OR IGNORE INTO swarm_tasks(swarm_id, task_id, profile, description, context_slice_json)
               VALUES(?,?,?,?,?)""",
            (swarm_id, t["task_id"], t.get("profile", ""),
             t.get("description", ""), json.dumps(t.get("context_slice", {}))),
        )
    conn.execute(
        "UPDATE swarm_runs SET task_count=? WHERE swarm_id=?",
        (len(tasks), swarm_id),
    )
    conn.commit()
