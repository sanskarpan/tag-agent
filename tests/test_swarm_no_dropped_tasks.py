"""Regression: tasks past --max-agents were silently dropped.

The wave loop dispatched ``list(ready)[:max_agents]`` but then retired the whole
ready set with ``remaining -= ready``. Every task beyond the cap was never run,
left ``pending``, and the run still reported ``completed`` — work vanished and
the status said everything was fine.

The Go port refuses the manifest outright in this situation and degrades loudly;
Python silently under-delivered.
"""
from __future__ import annotations

import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "src"))

import tag.swarm as swarm  # noqa: E402


def _wave_schedule(n_tasks: int, max_agents: int) -> list[list[str]]:
    """Replay the scheduler's wave loop over independent tasks."""
    ids = [f"t{i}" for i in range(n_tasks)]
    remaining = set(ids)
    waves = []
    guard = 0
    while remaining:
        guard += 1
        assert guard < 100, "wave loop did not terminate"
        ready = set(remaining)
        wave = sorted(ready)[:max_agents]
        waves.append(wave)
        remaining -= set(wave)   # the fixed line
    return waves


def test_every_task_runs_when_more_tasks_than_agents():
    waves = _wave_schedule(7, 3)
    ran = [t for w in waves for t in w]
    assert sorted(ran) == sorted(f"t{i}" for i in range(7)), (
        f"tasks were dropped: only {sorted(ran)} ran"
    )
    assert all(len(w) <= 3 for w in waves), f"a wave exceeded max_agents: {waves}"


def test_the_old_scheduler_dropped_tasks():
    """Pins the bug itself, so the fix cannot be reverted silently."""
    ids = [f"t{i}" for i in range(7)]
    remaining = set(ids)
    ready = set(remaining)
    wave = sorted(ready)[:3]
    remaining -= ready          # the OLD line
    assert not remaining, "sanity: the old line retires everything"
    assert len(wave) == 3, "…while only 3 of 7 ever ran"


def test_source_retires_only_what_ran():
    """Guard the actual statement, ignoring comments.

    A naive substring check over the source also matches the comment that
    *describes* the old line, so it fails on correct code — strip comments first.
    """
    import inspect

    code = [
        ln.split("#", 1)[0].strip()
        for ln in inspect.getsource(swarm).splitlines()
    ]
    assert any(ln == "remaining -= set(wave)" for ln in code), \
        "the scheduler must retire only the dispatched wave"
    assert not any(ln == "remaining -= ready" for ln in code), \
        "the dropping line is back"
