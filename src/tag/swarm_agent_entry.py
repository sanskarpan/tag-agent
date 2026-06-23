"""Swarm sub-agent entrypoint for PRD-023 context-centric multi-agent swarms.

Invoked by SwarmRunner as: python -m tag.swarm_agent_entry
Reads TAG_SWARM_TASK_INPUT (path to JSON), executes the assigned subtask via the
TAG runtime, then writes a result envelope to TAG_SWARM_RESULT_OUTPUT.
"""

from __future__ import annotations

import json
import os
import subprocess
import sys
import time
from pathlib import Path


def _read_input() -> dict:
    input_path = os.environ.get("TAG_SWARM_TASK_INPUT")
    if not input_path:
        _fail_exit("TAG_SWARM_TASK_INPUT not set")
    try:
        return json.loads(Path(input_path).read_text())
    except Exception as exc:
        _fail_exit(f"Cannot read input file: {exc}")


def _write_result(result_path: str, envelope: dict) -> None:
    try:
        p = Path(result_path)
        p.write_text(json.dumps(envelope))
        p.chmod(0o600)
    except Exception:
        pass


def _fail_exit(message: str) -> None:
    result_path = os.environ.get("TAG_SWARM_RESULT_OUTPUT", "")
    envelope = {"status": "failure", "error_message": message, "output": ""}
    if result_path:
        _write_result(result_path, envelope)
    sys.exit(1)


def _build_prompt(task: dict) -> str:
    description = task.get("description", "")
    context_slice = task.get("context_slice") or {}
    snapshot = task.get("context_bus_snapshot") or {}

    parts = [description]

    if context_slice:
        parts.append("\n\n--- Context provided for this subtask ---")
        for k, v in context_slice.items():
            parts.append(f"{k}: {json.dumps(v) if not isinstance(v, str) else v}")

    if snapshot:
        parts.append("\n\n--- Shared context bus (read-only snapshot) ---")
        for key, entry in snapshot.items():
            val = entry.get("value", "") if isinstance(entry, dict) else str(entry)
            parts.append(f"{key}: {val}")

    parts.append(
        "\n\n--- Output instructions ---\n"
        "Complete the subtask described above. Be thorough and precise.\n"
        "Your final output will be recorded as the subtask result."
    )
    return "\n".join(parts)


def main() -> None:
    task = _read_input()
    result_path = task.get("result_output_path") or os.environ.get("TAG_SWARM_RESULT_OUTPUT", "")
    ctx_out_path = task.get("context_bus_output_path") or os.environ.get("TAG_CONTEXT_BUS_OUTPUT", "")

    if not result_path:
        _fail_exit("result_output_path not specified")

    prompt = _build_prompt(task)

    # Resolve the TAG runtime binary
    try:
        from tag.controller import hermes_bin, _load_cfg  # noqa: PLC0415
        cfg = _load_cfg()
        binary = str(hermes_bin(cfg))
    except Exception:
        binary = os.environ.get("TAG_HERMES_BIN", "")
        if not binary:
            _fail_exit("Cannot locate TAG runtime binary")

    profile = os.environ.get("TAG_SWARM_PROFILE", "")
    start = time.monotonic()

    try:
        result = subprocess.run(
            [binary, "chat", "-q", prompt, "-Q"],
            text=True,
            capture_output=True,
            timeout=int(os.environ.get("TAG_SWARM_TIMEOUT", "300")),
        )
        elapsed = time.monotonic() - start
        output = result.stdout.strip()
        stderr = result.stderr.strip()

        # Write any context bus outputs
        if ctx_out_path and output:
            task_id = task.get("task_id", "unknown")
            ctx_payload = {
                "task_id": task_id,
                "output_key": f"result_{task_id}",
                "value": output,
                "value_type": "string",
            }
            try:
                p = Path(ctx_out_path)
                p.write_text(json.dumps(ctx_payload))
                p.chmod(0o600)
            except Exception:
                pass

        envelope = {
            "status": "success" if result.returncode == 0 else "failure",
            "output": output,
            "error_message": stderr if result.returncode != 0 else "",
            "tokens_prompt": 0,
            "tokens_completion": 0,
            "cost_usd": 0.0,
            "model": profile,
            "elapsed_seconds": round(elapsed, 2),
        }
    except subprocess.TimeoutExpired:
        envelope = {
            "status": "timed_out",
            "output": "",
            "error_message": "Subprocess timed out",
            "tokens_prompt": 0,
            "tokens_completion": 0,
            "cost_usd": 0.0,
            "model": profile,
        }
    except Exception as exc:
        envelope = {
            "status": "failure",
            "output": "",
            "error_message": str(exc),
            "tokens_prompt": 0,
            "tokens_completion": 0,
            "cost_usd": 0.0,
            "model": profile,
        }

    _write_result(result_path, envelope)
    sys.exit(0 if envelope["status"] in ("success",) else 1)


if __name__ == "__main__":
    main()
