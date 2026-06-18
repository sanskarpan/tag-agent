# PRD-119: Claude Computer-Use Screenshot Loop (`tag cu-loop`)

**Status:** Proposed
**Priority:** P1
**Estimated Effort:** M (5-8 days)
**Category:** Computer Use
**Affects:** `computer_use_loop.py + controller.py`
**Depends on:** PRD-118 (computer use CLI), PRD-028 (sandbox execution), PRD-041 (OTel span cost attribution)
**Inspired by:** Anthropic computer use demo, Claude 3.5 Sonnet computer_use_20241022 tool, CUA (Computer Use Agent), Screenshot-to-action loop

---

## 1. Overview

The Claude computer-use API (available in Claude 3.5 Sonnet and Claude 4 models) exposes a `computer_20241022` beta tool that enables screenshot capture, keyboard input, mouse control, and cursor position queries. The model requests these actions via structured tool calls; the client must implement the execution loop: take screenshot → send to model → model requests action → execute action → repeat.

This PRD specifies the Claude Computer-Use Screenshot Loop (`tag cu-loop`) — the core execution engine for all computer use sessions in TAG. It manages the full agentic loop: screenshot capture (via X11/Xvfb, VNC, or host display), encoding (PNG → base64), Claude API calls with the `computer_use_20241022` beta tool, action dispatch (`xdotool`, `pyautogui`, or VNC RFB protocol), loop termination detection, and safety constraints.

The implementation is modeled after Anthropic's open-source computer use demo (anthropic-quickstarts/computer-use-demo) and extended with TAG-specific features: OTel span attachment, cost tracking, action recording to SQLite, and sandboxed execution.

---

## 2. Problem Statement

### 2.1 Screenshot-action loop requires custom implementation

Each consumer of the Claude computer-use API must implement the same boilerplate: capture screenshot as PNG, encode base64, construct messages with image content blocks, parse tool_use responses, dispatch actions, loop. TAG should provide this as a reusable, tested module.

### 2.2 No error recovery in the loop

If a screenshot fails or an action raises an exception, the entire session crashes. Production loops need error recovery: retry failed screenshots, skip failed actions with an error message to the model, and enforce max-retry limits.

### 2.3 Loop termination is ambiguous

The model may declare "I'm done" in text, or it may run forever. A well-implemented loop needs: text-response detection (model finished), max action limit, idle detection (model takes no action), and timeout.

---

## 3. Goals

| ID | Goal |
|----|------|
| G1 | `ComputerUseLoop.run(goal, max_actions, allow_actions)` manages the full screenshot-action loop lifecycle. |
| G2 | Screenshot capture: support X11 (`scrot`/`xwd`), `pyautogui`, and VNC RFB protocol backends. |
| G3 | Claude API integration: send screenshot as base64 image + current action history; parse `computer_use_20241022` tool_use blocks. |
| G4 | Action dispatch: map Claude tool call arguments to system actions (`xdotool`, `pyautogui`, VNC RFB). |
| G5 | Loop termination: stop on text-only response (no tool_use), max actions, timeout, or explicit stop token. |
| G6 | Per-iteration cost tracking: extract input/output tokens from API response; accumulate in SQLite. |
| G7 | Error recovery: retry failed screenshots up to 3 times; return error message to model on action failure. |

## 3.1 Non-Goals

| ID | Non-Goal |
|----|----------|
| NG1 | Non-Claude model support (OpenAI computer use, etc.). Claude API only in this PRD. |
| NG2 | Custom action execution environments beyond X11/pyautogui/VNC. |
| NG3 | Streaming screenshots to an external observer. |
| NG4 | Parallel computer-use sessions. Each `ComputerUseLoop` manages one session. |

---

## 4. Success Metrics

| Metric | Target | Measurement |
|--------|--------|-------------|
| Loop iteration time | < 500ms per iteration (excluding API latency) | Benchmark test |
| Screenshot encoding time | PNG capture + base64 encode in < 200ms | Benchmark test |
| Error recovery | Failed screenshot retried 3 times before failing session | Unit test |
| Cost tracking accuracy | Token counts match Claude API response headers | Integration test |
| Loop termination | Text-only model response terminates loop in 100% of test cases | Unit test |

---

## 5. User Stories

| ID | As a... | I want to... | So that... |
|----|---------|-------------|------------|
| US1 | Developer | Have a correct, tested screenshot-action loop | I don't implement computer-use boilerplate myself |
| US2 | Developer | Have automatic cost tracking per computer-use session | I know how much each automation costs |
| US3 | Developer | Have error recovery on failed actions | Sessions survive transient failures |
| US4 | Developer | Use the loop with a VNC backend for sandbox isolation | I run computer use safely |

---

## 6. CLI Surface

```python
# Python API:
from tag.computer_use_loop import ComputerUseLoop, ScreenshotBackend

loop = ComputerUseLoop(
    model="claude-sonnet-4-6",
    screenshot_backend=ScreenshotBackend.PYAUTOGUI,
    display_width=1280,
    display_height=800,
    allow_actions={"screenshot", "left_click", "type", "key"},
    max_actions=50,
    timeout=300,
)

result = loop.run(
    goal="Open Firefox and navigate to example.com",
    session_id="my-session",
)
# result: {"status": "completed|max_actions|timeout", "action_count": N,
#          "final_screenshot": <base64>, "cost_usd": 0.12}
```

```
# CLI (thin wrapper, main interface is PRD-118):
tag cu-loop run --goal "..." --backend pyautogui|vnc|x11 [options]
```

---

## 7. Functional Requirements

| ID | Requirement |
|----|------------|
| FR-01 | `ComputerUseLoop.__init__` validates model supports computer use (checked against known model IDs). |
| FR-02 | Each iteration: capture screenshot via configured backend → encode PNG as base64 → construct messages → call Claude API. |
| FR-03 | Claude API call uses `anthropic-beta: computer-use-2024-10-22` header and includes `computer_20241022` tool in tools list. |
| FR-04 | Parse API response: if content contains `tool_use` block with `type: computer_20241022`, extract action; else, check for text-only response → terminate. |
| FR-05 | Action dispatch: map action type → platform action (see action map below). |
| FR-06 | Allow-list check before dispatch: if action not in `allow_actions`, add error message to messages and continue loop. |
| FR-07 | Per-iteration cost: extract `usage.input_tokens` and `usage.output_tokens` from API response; accumulate total cost. |
| FR-08 | Screenshot retry: if screenshot capture raises, retry up to 3 times with 0.5s delay; on 3rd failure, raise `ScreenshotError`. |
| FR-09 | Max actions: after `max_actions` iterations, set status `max_actions` and return. |
| FR-10 | Messages are truncated to keep context window under 90% capacity: drop oldest screenshot images first (keep text content). |

---

## 8. Non-Functional Requirements

| ID | Requirement |
|----|------------|
| NFR-01 | Screenshot size: scale down to max 1092×1092 pixels before base64 encoding (Anthropic recommendation). |
| NFR-02 | Base64-encoded screenshots stored as strings in the messages list; not written to disk by the loop itself. |
| NFR-03 | Loop is synchronous (not async); all API calls are blocking. |
| NFR-04 | Claude API `max_tokens` set to 4096 per computer-use call. |

---

## 9. Technical Design

### 9.1 Action dispatch map

```python
ACTION_HANDLERS = {
    "screenshot":     lambda a: take_screenshot(a),
    "left_click":     lambda a: dispatch_click(a["coordinate"], button="left"),
    "right_click":    lambda a: dispatch_click(a["coordinate"], button="right"),
    "double_click":   lambda a: dispatch_click(a["coordinate"], button="left", double=True),
    "type":           lambda a: dispatch_type(a["text"]),
    "key":            lambda a: dispatch_key(a["text"]),
    "scroll":         lambda a: dispatch_scroll(a["coordinate"], a["direction"], a["amount"]),
    "move":           lambda a: dispatch_move(a["coordinate"]),
    "cursor_position": lambda a: get_cursor_position(),
}
```

### 9.2 Python core (loop skeleton)

```python
from __future__ import annotations
import base64
import io
import time
from typing import List, Optional, Set

class ComputerUseLoop:
    COMPUTER_USE_BETA = "computer-use-2024-10-22"
    TOOL_NAME = "computer_20241022"

    def __init__(self, model: str, screenshot_backend: str = "pyautogui",
                 display_width: int = 1280, display_height: int = 800,
                 allow_actions: Optional[Set[str]] = None, max_actions: int = 50,
                 timeout: int = 300) -> None:
        self.model = model
        self.backend = screenshot_backend
        self.width, self.height = display_width, display_height
        self.allow_actions = allow_actions
        self.max_actions = max_actions
        self.timeout = timeout

    def _capture_screenshot(self) -> str:
        if self.backend == "pyautogui":
            import pyautogui
            img = pyautogui.screenshot()
            buf = io.BytesIO()
            img.resize((min(self.width, 1092), min(self.height, 1092))).save(buf, format="PNG")
            return base64.standard_b64encode(buf.getvalue()).decode()
        raise NotImplementedError(f"Backend {self.backend} not implemented")

    def run(self, goal: str, session_id: Optional[str] = None) -> dict:
        import anthropic
        client = anthropic.Anthropic()
        messages: List[dict] = [{"role": "user", "content": goal}]
        action_count = 0
        total_input_tokens = 0
        total_output_tokens = 0
        start_time = time.time()

        while action_count < self.max_actions:
            if time.time() - start_time > self.timeout:
                return {"status": "timeout", "action_count": action_count}
            # Capture and inject screenshot
            try:
                screenshot_b64 = self._capture_screenshot()
            except Exception:
                return {"status": "screenshot_error", "action_count": action_count}
            messages.append({"role": "user", "content": [
                {"type": "tool_result", "tool_use_id": "screenshot",
                 "content": [{"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": screenshot_b64}}]}
            ]}) if action_count > 0 else None
            # Call Claude
            response = client.beta.messages.create(
                model=self.model,
                max_tokens=4096,
                tools=[{"type": self.TOOL_NAME, "name": self.TOOL_NAME,
                        "display_width_px": self.width, "display_height_px": self.height,
                        "display_number": 1}],
                messages=messages,
                betas=[self.COMPUTER_USE_BETA],
            )
            total_input_tokens += response.usage.input_tokens
            total_output_tokens += response.usage.output_tokens
            # Check for stop
            tool_uses = [b for b in response.content if b.type == "tool_use"]
            if not tool_uses:
                return {"status": "completed", "action_count": action_count,
                        "cost_usd": self._cost(total_input_tokens, total_output_tokens)}
            for tool_use in tool_uses:
                if self.allow_actions and tool_use.input.get("action") not in self.allow_actions:
                    messages.append({"role": "user", "content": [
                        {"type": "tool_result", "tool_use_id": tool_use.id,
                         "content": f"Action '{tool_use.input.get('action')}' is not allowed."}
                    ]})
                    continue
                ACTION_HANDLERS.get(tool_use.input.get("action", ""), lambda a: None)(tool_use.input)
                action_count += 1
            messages.append({"role": "assistant", "content": response.content})

        return {"status": "max_actions", "action_count": action_count}

    def _cost(self, input_tokens: int, output_tokens: int) -> float:
        INPUT_RATE = 3.0 / 1_000_000  # claude-sonnet-4-6 input
        OUTPUT_RATE = 15.0 / 1_000_000
        return input_tokens * INPUT_RATE + output_tokens * OUTPUT_RATE

ACTION_HANDLERS: dict = {}  # Populated by platform-specific backends
```

---

## 10. Security Considerations

| Risk | Mitigation |
|------|-----------|
| Model executing arbitrary system actions | Allow-list enforced at dispatch |
| Screenshot leaking sensitive desktop content | Screenshots stored only in memory during loop; PRD-118 persists selectively |
| Infinite API loop consuming credits | `max_actions` and `timeout` hard stops |

---

## 11. Testing Strategy

| Layer | Tests |
|-------|-------|
| Unit | Action allow-list enforcement; loop termination on text-only response; cost calculation |
| Integration | Mock Claude API → loop iteration → action dispatch → screenshot → next iteration |
| Error handling | Screenshot retry; action failure recovery |

---

## 12. Acceptance Criteria

| ID | Criterion |
|----|----------|
| AC-01 | Text-only model response terminates loop with status `completed` |
| AC-02 | After `max_actions` iterations, loop returns status `max_actions` |
| AC-03 | Action not in allow-list sends error message to model and continues |
| AC-04 | Cost calculation matches `input_tokens * rate + output_tokens * rate` |
| AC-05 | Screenshot retry attempts 3 times before raising |

---

## 13. Dependencies

| Dependency | Reason |
|-----------|--------|
| `anthropic` SDK ≥ 0.40 | Computer use beta tool support |
| `pyautogui` | Desktop screenshot capture (optional backend) |
| PRD-118 computer use CLI | Session management wrapper |

---

## 14. Open Questions

| ID | Question |
|----|---------|
| OQ-01 | Should the loop support streaming responses for lower TTFB? |
| OQ-02 | Should screenshot resolution adapt based on Claude's recommended scaling? |

---

## 15. Complexity & Timeline

**Complexity:** Medium (M)
**Estimated effort:** 5–8 engineer-days

| Phase | Work | Days |
|-------|------|------|
| 1 | `ComputerUseLoop` core, screenshot capture, action dispatch | 2 |
| 2 | Claude API integration (beta headers, tool definition, response parsing) | 2 |
| 3 | Allow-list, error recovery, cost tracking | 1 |
| 4 | Integration tests, documentation | 2 |

