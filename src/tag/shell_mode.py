"""PRD-019: Natural Language Shell Mode for TAG CLI.

Provides an interactive REPL that accepts plain-English instructions and
routes them to the appropriate Hermes profile via subprocess, with support
for slash-command shortcuts and automatic task-type classification.
"""

from __future__ import annotations

import json
import subprocess
import sys
from typing import Any

from prompt_toolkit.shortcuts import PromptSession

# ---------------------------------------------------------------------------
# Slash-command registry
# ---------------------------------------------------------------------------

SHELL_COMMANDS: dict[str, str] = {
    "/exit": "Exit the TAG shell",
    "/quit": "Exit the TAG shell (alias for /exit)",
    "/profile <name>": "Switch to a different profile for this session",
    "/status": "Show current profile, session cost, and turn count",
    "/cost": "Show estimated session cost in USD",
    "/history": "Print the conversation history for this session",
    "/clear": "Clear the conversation history",
    "/help": "Show available slash commands",
    "/profiles": "List all profiles defined in the TAG config",
}

# ---------------------------------------------------------------------------
# Task-type classifier
# ---------------------------------------------------------------------------

TASK_CLASSIFIER_PROMPT: str = """\
You are a task-type classifier for TAG (an AI agent orchestration tool).

Your job is to analyse a single user message and decide which category it
belongs to, then return a JSON object with exactly these fields:

  task_type  - one of: "research", "implementation", "review", "mixed", "direct_chat"
  profile    - the best profile slug to handle this task (or empty string if
               you cannot determine it from context)
  reasoning  - a one-sentence explanation of your classification

Category definitions
--------------------
research       : gathering information, explaining concepts, answering factual
                 questions, web lookups, documentation look-ups
implementation : writing, editing, or debugging code; creating files; running
                 commands; making things
review         : reviewing code, pull requests, diffs, or designs for quality,
                 correctness, or security issues
mixed          : the request spans multiple categories (e.g. "research X then
                 implement Y" or "review and fix this code")
direct_chat    : casual conversation, clarifying questions, greetings, or
                 anything that does not require agent tool-use

Return ONLY valid JSON — no markdown fences, no commentary outside the JSON
object itself. Example:

{"task_type": "implementation", "profile": "coder", "reasoning": "The user wants to write a Python script."}
"""

# ---------------------------------------------------------------------------
# Classifier helper
# ---------------------------------------------------------------------------


def classify_input(text: str, cfg: dict[str, Any], profile: str) -> dict[str, Any]:
    """Call hermes with the classifier prompt and return parsed JSON.

    Falls back to a safe default if the subprocess fails or returns
    malformed output.
    """
    default: dict[str, Any] = {
        "task_type": "direct_chat",
        "profile": profile,
        "reasoning": "Classification unavailable — defaulting to direct_chat.",
    }

    try:
        from tag.controller import (  # noqa: PLC0415
            ensure_runtime_dirs,
            hermes_bin,
            profile_exec_env,
        )
    except ImportError:
        return default

    try:
        ensure_runtime_dirs(cfg)
        env = profile_exec_env(cfg, profile)
        proc = subprocess.run(
            [
                str(hermes_bin(cfg)),
                "chat",
                "-q",
                f"{TASK_CLASSIFIER_PROMPT}\n\nUser message to classify:\n{text}",
                "-Q",
            ],
            env=env,
            text=True,
            capture_output=True,
            timeout=30,
        )
        output = proc.stdout.strip()
        if not output:
            return default
        # Strip markdown code fences if present.
        if output.startswith("```") and output.endswith("```"):
            lines = output.splitlines()
            if len(lines) >= 3:
                output = "\n".join(lines[1:-1]).strip()
        parsed: dict[str, Any] = json.loads(output)
        if "task_type" not in parsed:
            return default
        parsed.setdefault("profile", profile)
        parsed.setdefault("reasoning", "")
        return parsed
    except (subprocess.TimeoutExpired, subprocess.SubprocessError, json.JSONDecodeError, OSError):
        return default


# ---------------------------------------------------------------------------
# Session state
# ---------------------------------------------------------------------------


class ShellSession:
    """Tracks conversation history and cost for one TAG shell session."""

    def __init__(self, cfg: dict[str, Any], profile_name: str) -> None:
        self.cfg = cfg
        self.profile_name = profile_name
        self.history: list[dict[str, Any]] = []
        self.session_cost_usd: float = 0.0

    def add_turn(self, role: str, content: str) -> None:
        """Append a turn to the conversation history."""
        self.history.append({"role": role, "content": content})

    def get_history_text(self) -> str:
        """Return the conversation history as a readable string."""
        if not self.history:
            return "(no history)"
        lines: list[str] = []
        for i, turn in enumerate(self.history, start=1):
            role = turn.get("role", "?")
            content = turn.get("content", "")
            lines.append(f"[{i}] {role.upper()}: {content}")
        return "\n".join(lines)


# ---------------------------------------------------------------------------
# Internal dispatch helper
# ---------------------------------------------------------------------------


def _dispatch_to_hermes(
    session: ShellSession,
    text: str,
    *,
    target_profile: str,
) -> int:
    """Run hermes chat -q <text> -Q for the given profile, streaming output live."""
    try:
        from tag.controller import (  # noqa: PLC0415
            ensure_runtime_dirs,
            hermes_bin,
            profile_exec_env,
        )
    except ImportError:
        print("error: TAG controller not available.", file=sys.stderr)
        return 1

    try:
        ensure_runtime_dirs(session.cfg)
        env = profile_exec_env(session.cfg, target_profile)
        proc = subprocess.run(
            [str(hermes_bin(session.cfg)), "chat", "-q", text, "-Q"],
            env=env,
            text=True,
            check=False,
        )
        return int(proc.returncode)
    except (subprocess.SubprocessError, OSError) as exc:
        print(f"error: hermes subprocess failed: {exc}", file=sys.stderr)
        return 1


# ---------------------------------------------------------------------------
# Main REPL
# ---------------------------------------------------------------------------


def run_shell(cfg: dict[str, Any], profile_name: str) -> int:
    """Run the TAG natural-language shell (PRD-019).

    Presents an interactive prompt, classifies input, routes to the
    appropriate profile, and handles slash-command shortcuts.

    Returns 0 on clean exit (/exit, /quit, or EOF).
    """
    profiles: dict[str, Any] = cfg.get("profiles", {})

    if profile_name not in profiles:
        available = ", ".join(sorted(profiles)) or "(none)"
        print(
            f"error: profile '{profile_name}' not found in TAG config. "
            f"Available profiles: {available}",
            file=sys.stderr,
        )
        return 1

    session = ShellSession(cfg, profile_name)
    pt_session: PromptSession = PromptSession()

    print(
        f"TAG shell — profile: {session.profile_name}  "
        f"(type /help for commands, /exit to quit)"
    )

    while True:
        prompt_str = f"TAG shell [{session.profile_name}]> "

        try:
            raw: str = pt_session.prompt(prompt_str)
        except KeyboardInterrupt:
            # Ctrl-C clears the current line; continue without exiting.
            print("(interrupted — use /exit to quit)")
            continue
        except EOFError:
            # Ctrl-D — treat as clean exit.
            print()
            return 0

        text = raw.strip()
        if not text:
            continue

        # ------------------------------------------------------------------
        # Slash-command dispatch
        # ------------------------------------------------------------------
        lower = text.lower()

        if lower in ("/exit", "/quit"):
            print("Goodbye.")
            return 0

        if lower == "/help":
            print("Available commands:")
            for cmd, desc in SHELL_COMMANDS.items():
                print(f"  {cmd:<28} {desc}")
            continue

        if lower == "/status":
            turns = len(session.history)
            print(
                f"Profile : {session.profile_name}\n"
                f"Turns   : {turns}\n"
                f"Cost    : ${session.session_cost_usd:.4f} USD (estimated)"
            )
            continue

        if lower == "/cost":
            print(f"Estimated session cost: ${session.session_cost_usd:.4f} USD")
            continue

        if lower == "/history":
            print(session.get_history_text())
            continue

        if lower == "/clear":
            session.history.clear()
            session.session_cost_usd = 0.0
            print("History cleared.")
            continue

        if lower == "/profiles":
            all_profiles = sorted(cfg.get("profiles", {}).keys())
            if all_profiles:
                print("Configured profiles:")
                for p in all_profiles:
                    marker = " *" if p == session.profile_name else ""
                    print(f"  {p}{marker}")
            else:
                print("No profiles configured.")
            continue

        if lower.startswith("/profile "):
            parts = text.split(None, 1)
            if len(parts) < 2 or not parts[1].strip():
                print("Usage: /profile <name>")
                continue
            new_profile = parts[1].strip()
            if new_profile not in profiles:
                available = ", ".join(sorted(profiles))
                print(
                    f"error: profile '{new_profile}' not found. "
                    f"Available: {available}"
                )
                continue
            session.profile_name = new_profile
            print(f"Switched to profile: {new_profile}")
            continue

        if text.startswith("/"):
            print(
                f"Unknown command '{text.split()[0]}'. "
                "Type /help to see available commands."
            )
            continue

        # ------------------------------------------------------------------
        # Natural-language dispatch
        # ------------------------------------------------------------------
        session.add_turn("user", text)

        classification = classify_input(text, cfg, session.profile_name)
        target_profile = str(classification.get("profile") or session.profile_name).strip()
        if not target_profile or target_profile not in profiles:
            target_profile = session.profile_name

        _dispatch_to_hermes(session, text, target_profile=target_profile)
        session.add_turn("assistant", f"(dispatched to profile: {target_profile})")

    return 0

