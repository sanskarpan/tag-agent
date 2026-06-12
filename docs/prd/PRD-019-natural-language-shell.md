# PRD-019: Natural Language Shell Mode (`tag shell`)

**Status:** Proposed  
**Priority:** P2  
**Estimated Effort:** M (2 weeks)  
**Affects:** `controller.py` (new `cmd_shell`), new `tag/shell_mode.py`

---

## 1. Overview

TAG's current interface is a structured CLI: users must know `tag submit --task-type mixed "..."` syntax. This creates a barrier for users who want to interact conversationally with the agent system. This PRD adds `tag shell` — a persistent REPL that accepts natural language input, automatically routes requests to the right profiles, and maintains session state across turns. It blurs the line between CLI tool and AI chat interface, making TAG accessible without learning its command syntax.

---

## 2. Problem Statement

- New users face a steep learning curve: profiles, task types, routing, import commands.
- Experienced users want to type "research concurrency in Python then implement a cache" and have the system figure out orchestration.
- There is no persistent session across multiple `tag chat` invocations.
- Competing tools (Claude Code, GitHub Copilot Chat, Cursor Chat) all offer conversational entry points.
- `tag tui` exists but requires launching a full TUI; `tag shell` would be lighter.

---

## 3. Goals

1. `tag shell` opens a persistent REPL that accepts free-text input.
2. Orchestrator profile auto-routes requests to the appropriate profile(s).
3. Session state persists across turns in the shell (user can reference previous outputs).
4. Rich streaming output (from PRD-003) shows which profile is responding.
5. Built-in shell commands: `/profile`, `/status`, `/cost`, `/history`, `/exit`.
6. `tag shell --profile researcher` starts a direct single-profile chat session.

---

## 4. Non-Goals

- Replacing `tag tui` (the full TUI with kanban, sessions, etc.).
- Voice input.
- Streaming multi-agent visualization (too complex for v1).

---

## 5. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|-----------|----------|
| U1 | New user | type `tag shell` and start chatting | I don't need to learn TAG syntax |
| U2 | Developer | type "implement a Redis cache for our API" | coder profile picks it up automatically |
| U3 | Developer | type "research best Redis patterns then implement" | orchestrator routes research → coder |
| U4 | Developer | type `/profile coder` in the shell | I switch to direct coder mode |
| U5 | Developer | type `/cost` | I see how much this shell session has cost |

---

## 6. Technical Design

### 6.1 New module: `src/tag/shell_mode.py`

```python
"""Interactive REPL for TAG — natural language shell mode."""
from __future__ import annotations
import sys, readline, re
from typing import Any

SHELL_COMMANDS = {
    "/exit": "Exit the shell",
    "/quit": "Exit the shell",
    "/profile <name>": "Switch to a specific profile",
    "/status": "Show current profile, model, and context usage",
    "/cost": "Show session cost so far",
    "/history": "Show conversation history",
    "/clear": "Clear conversation context",
    "/help": "Show this help",
    "/profiles": "List available profiles",
}

TASK_CLASSIFIER_PROMPT = """You are a routing assistant for TAG (an AI agent orchestration system).
Based on the user's request, determine:
1. The best task_type: "research", "implementation", "review", or "mixed"
2. Whether it should use orchestrator routing (multi-profile) or direct single-profile

User request: {request}

Respond with JSON only: {{"task_type": "...", "routing": "orchestrator|direct", "profile": "..."}}"""


class ShellSession:
    def __init__(self, cfg: dict[str, Any], profile: str | None = None):
        self.cfg = cfg
        self.current_profile = profile or cfg["defaults"]["master_profile"]
        self.session_cost = 0.0
        self.turn_count = 0
        self.history: list[dict[str, str]] = []
    
    def classify_request(self, text: str) -> dict[str, Any]:
        """Ask orchestrator to classify the request type."""
        try:
            import json
            from tag.controller import run_chat_step, normalize_chat_output
            result = run_chat_step(
                self.cfg,
                self.cfg["defaults"]["master_profile"],
                TASK_CLASSIFIER_PROMPT.format(request=text),
            )
            output = normalize_chat_output(result.get("output", ""))
            # Extract JSON from output
            match = re.search(r'\{[^}]+\}', output)
            if match:
                return json.loads(match.group())
        except Exception:
            pass
        return {"task_type": "mixed", "routing": "direct", "profile": self.current_profile}
    
    def handle_shell_command(self, cmd: str) -> bool:
        """Handle /command. Returns True if handled."""
        parts = cmd.strip().split(None, 1)
        command = parts[0].lower()
        arg = parts[1] if len(parts) > 1 else ""
        
        if command in ("/exit", "/quit"):
            print("Goodbye!")
            sys.exit(0)
        
        elif command == "/profile":
            if arg:
                profiles = list(self.cfg.get("profiles", {}).keys())
                if arg in profiles:
                    self.current_profile = arg
                    print(f"Switched to profile: {arg}")
                else:
                    print(f"Unknown profile: {arg}. Available: {', '.join(profiles)}")
            else:
                print(f"Current profile: {self.current_profile}")
        
        elif command == "/profiles":
            for p, pdata in self.cfg.get("profiles", {}).items():
                desc = pdata.get("description", "")[:60]
                marker = "●" if p == self.current_profile else " "
                print(f"  {marker} {p}: {desc}")
        
        elif command == "/status":
            from tag.controller import get_context_size
            size = get_context_size(self.cfg, self.current_profile)
            print(f"Profile: {self.current_profile}")
            print(f"Context: {size.get('used_tokens', 0):,} / {size.get('max_tokens', 128000):,} tokens ({size.get('pct', 0):.0f}%)")
            print(f"Session cost: ${self.session_cost:.4f}")
            print(f"Turns: {self.turn_count}")
        
        elif command == "/cost":
            print(f"Session cost: ${self.session_cost:.4f}")
        
        elif command == "/clear":
            from tag.controller import run_profile_hermes
            run_profile_hermes(self.cfg, self.current_profile, "sessions", "clear", check=False)
            self.history.clear()
            print("Context cleared.")
        
        elif command == "/history":
            for i, turn in enumerate(self.history[-10:], 1):
                print(f"[{i}] {turn['role']}: {turn['content'][:100]}")
        
        elif command == "/help":
            print("\nShell commands:")
            for cmd, desc in SHELL_COMMANDS.items():
                print(f"  {cmd:<25} {desc}")
            print()
        
        else:
            return False
        
        return True
    
    def run_turn(self, user_input: str) -> None:
        """Process one user turn."""
        from tag.controller import run_chat_step, normalize_chat_output
        from tag.tui_output import chat_spinner, stream_output, print_status_bar
        import time
        
        self.turn_count += 1
        self.history.append({"role": "user", "content": user_input})
        
        # Auto-route if using orchestrator
        profile_to_use = self.current_profile
        if profile_to_use == self.cfg["defaults"]["master_profile"]:
            routing = self.classify_request(user_input)
            if routing.get("routing") == "direct" and routing.get("profile"):
                profile_to_use = routing["profile"]
        
        # Show which profile is handling
        if profile_to_use != self.current_profile:
            print(f"\n[routing → {profile_to_use}]")
        
        start = time.monotonic()
        with chat_spinner(profile_to_use, ""):
            result = run_chat_step(self.cfg, profile_to_use, user_input)
        elapsed = time.monotonic() - start
        
        output = normalize_chat_output(result.get("output", ""))
        print(f"\n{output}\n")
        
        print_status_bar(
            profile_to_use, "",
            result.get("prompt_tokens", 0),
            result.get("completion_tokens", 0),
            elapsed,
        )
        
        self.history.append({"role": "assistant", "content": output})
```

### 6.2 `cmd_shell` function

```python
def cmd_shell(args: argparse.Namespace) -> int:
    cfg = load_config(config_path(args.config))
    ensure_hermes_ready(cfg, config_arg=args.config, need_tui=False)
    
    profile = getattr(args, "profile", None)
    
    from tag.shell_mode import ShellSession
    from tag.tui_output import get_console
    
    session = ShellSession(cfg, profile)
    console = get_console()
    
    if console:
        console.print(f"\n[bold cyan]TAG Shell[/bold cyan] [dim]v{__version__}[/dim]")
        console.print(f"[dim]Profile: {session.current_profile} · Type /help for commands · Ctrl+C to exit[/dim]\n")
    else:
        print(f"\nTAG Shell v{__version__}")
        print(f"Profile: {session.current_profile} | Type /help for commands | Ctrl+C to exit\n")
    
    while True:
        try:
            user_input = input("you> ").strip()
        except (EOFError, KeyboardInterrupt):
            print("\nGoodbye!")
            break
        
        if not user_input:
            continue
        
        if user_input.startswith("/"):
            if not session.handle_shell_command(user_input):
                print(f"Unknown command: {user_input}. Try /help")
            continue
        
        session.run_turn(user_input)
    
    return 0
```

### 6.3 readline history

```python
# In cmd_shell, after session init:
import atexit
history_file = tag_home() / "shell_history"
try:
    readline.read_history_file(str(history_file))
except FileNotFoundError:
    pass
atexit.register(readline.write_history_file, str(history_file))
readline.set_history_length(1000)
```

### 6.4 Parser registration

```python
p_shell = subparsers.add_parser("shell", help="Interactive natural language shell")
p_shell.add_argument("--profile", metavar="NAME", help="Start with specific profile")
p_shell.add_argument("--no-route", action="store_true", help="Disable auto-routing")
p_shell.set_defaults(func=cmd_shell)
```

---

## 7. Implementation Plan

| Step | Task |
|------|------|
| 1 | Create `src/tag/shell_mode.py` with `ShellSession` class |
| 2 | Implement `handle_shell_command` for all `/commands` |
| 3 | Implement `run_turn` with Rich output |
| 4 | Implement `classify_request` routing |
| 5 | Add readline history persistence |
| 6 | Implement `cmd_shell` |
| 7 | Register `shell` parser |
| 8 | Tests: `test_shell_command_profile_switch`, `test_shell_command_clear` |
| 9 | Update README with shell quickstart |

---

## 8. Success Metrics

- `tag shell` opens and accepts input without error.
- `/profile coder` switches current profile.
- Natural language "write a function to sort strings" routes to coder profile.
- Shell history persists across invocations.
- Ctrl+C exits cleanly.

---

## 9. Risks & Mitigations

| Risk | Mitigation |
|------|-----------|
| Auto-classification LLM call adds latency to every turn | Cache classification for similar inputs; add `--no-route` flag |
| readline not available on Windows | Try/except with fallback to `input()` |
| Shell session leaks Hermes gateway processes | Cleanup via `atexit.register` to call `hermes gateway stop` |
