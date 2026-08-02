"""Regression: the loop must not complete on a MENTION of GOAL_ACHIEVED.

`_is_goal_achieved` was a bare ``"GOAL_ACHIEVED" in output``. The sentinel is
also in the prompt, so any echoing provider completed the loop on iteration 1;
and a model that merely restates the instruction ("I will output GOAL_ACHIEVED
when done. Not finished yet.") completed it while saying the opposite.

Mirrors internal/loop/goalachieved_test.go in the Go harness.
"""
from __future__ import annotations

import sys
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "src"))

from tag.loop_agent import _is_goal_achieved  # noqa: E402

PROMPT_FIRST = (
    "Goal: fix the bug\n\n"
    "This is iteration 1 of an autonomous agent loop. "
    "Work toward achieving the goal. Output GOAL_ACHIEVED when done."
)
PROMPT_NEXT = (
    "Goal: fix the bug\n\n"
    "Previous iteration output:\nsomething\n\n"
    "This is iteration 2. Continue working toward the goal, "
    "or output GOAL_ACHIEVED if the goal has been met."
)


@pytest.mark.parametrize(
    "output",
    [
        "I am working on it. I will output GOAL_ACHIEVED when done. Not finished yet.",
        "Once the tests pass I'll say GOAL_ACHIEVED.",
        "The instruction says to output GOAL_ACHIEVED if the goal has been met; it has not.",
        "GOAL_ACHIEVED is what I will print later, but first I need to read the file.",
        PROMPT_FIRST,
        PROMPT_NEXT,
        "still thinking",
        "",
    ],
)
def test_mentions_do_not_complete_the_loop(output):
    assert not _is_goal_achieved(output), output


@pytest.mark.parametrize(
    "output",
    [
        "GOAL_ACHIEVED",
        "Fixed the off-by-one in parse().\n\nGOAL_ACHIEVED",
        "All tests pass now. GOAL_ACHIEVED",
        "Done. **GOAL_ACHIEVED**",
        "  GOAL_ACHIEVED  \n",
        "Patched and verified. GOAL_ACHIEVED!",
    ],
)
def test_declarations_complete_the_loop(output):
    assert _is_goal_achieved(output), output
