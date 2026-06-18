# PRD-118: Computer Use CLI (`tag computer-use`)

**Status:** Proposed
**Priority:** P1
**Estimated Effort:** M (5-8 days)
**Category:** Computer Use
**Affects:** `computer_use.py + controller.py`
**Depends on:** PRD-119 (Claude computer-use screenshot loop), PRD-028 (sandbox execution), PRD-089 (sandbox streaming stdout/stderr)
**Inspired by:** Anthropic Claude computer use, OpenAI computer use preview, Playwright CUA, SWE-agent bash harness

---

## 1. Overview

Anthropic's Claude 3.5 Sonnet and Claude 4 models support computer use via a specialized tool API: the model can request screenshot captures, mouse clicks, keyboard input, and cursor position — enabling it to control a computer GUI as a human would. TAG currently has no first-class CLI surface for computer use workflows: there is no `tag computer-use` command, no integration with Claude's `computer_20241022` tool type, and no way to configure the screenshot/action loop parameters.

Computer Use CLI (`tag computer-use`) introduces a first-class `tag computer-use` command that launches a controlled computer-use session using Claude's computer use API. The session manages the screenshot → model → action loop (PRD-119), runs in a sandboxed virtual desktop (PRD-120 VNC), and captures all screenshots and actions as OTel spans for observability. Engineers can configure the desktop dimensions, model, max actions, and action allow-list to constrain what the model can do.

---

## 2. Problem Statement

### 2.1 No CLI for computer use workflows

Claude supports computer use via the API, but TAG has no CLI wrapper. Engineers must write custom Python scripts to implement the screenshot-action loop, handle screenshot encoding, manage action dispatch, and implement safety constraints.

### 2.2 No safety constraints on computer use actions

Unbounded computer use (allowing all click/type/scroll actions) is dangerous for production environments. There is no built-in allow-list for permitted action types or domains.

### 2.3 No observability for computer use sessions

Screenshots, action sequences, and outcomes are not captured in the TAG span system — making it impossible to replay or debug computer use sessions after the fact.

---

## 3. Goals

| ID | Goal |
|----|------|
| G1 | `tag computer-use run --goal GOAL` launches a computer-use session with the specified natural language goal. |
| G2 | Configure screen dimensions, model, max actions, action type allow-list. |
| G3 | All screenshots captured as base64-encoded image attachments in OTel spans. |
| G4 | Action allow-list: `--allow-actions screenshot,key,type,left_click` restricts which computer_use tool actions the model can request. |
| G5 | Integration with sandbox VNC (PRD-120) for isolated desktop environment. |
| G6 | `tag computer-use replay <session-id>` replays the screenshot sequence from a past session. |
| G7 | Session metadata (goal, model, action count, final screenshot) persisted to SQLite. |

## 3.1 Non-Goals

| ID | Non-Goal |
|----|----------|
| NG1 | Cross-platform action dispatch (Windows/macOS/Linux host control). Sandbox VNC only. |
| NG2 | Video recording of computer use sessions. |
| NG3 | Multi-model computer use (single model per session). |
| NG4 | Real-time streaming of screenshots to external observer. |

---

## 4. Success Metrics

| Metric | Target | Measurement |
|--------|--------|-------------|
| Loop iteration latency | Screenshot → model → action cycle in < 3s (excluding model API latency) | Benchmark test |
| Allow-list enforcement | Rejected action types are never dispatched in 100% of test cases | Unit test |
| Screenshot capture | All screenshots saved to span system in < 500ms | Benchmark test |
| Session replay | `tag computer-use replay` renders all screenshots in sequence | Integration test |

---

## 5. User Stories

| ID | As a... | I want to... | So that... |
|----|---------|-------------|------------|
| US1 | Developer | Run a computer use task with `tag computer-use run --goal "..."` | I automate GUI workflows with one command |
| US2 | Security engineer | Restrict allowed actions to `screenshot,type` only | I prevent the model from clicking arbitrary UI elements |
| US3 | Developer | Replay a past computer use session's screenshots | I debug what the model did |
| US4 | Developer | Run computer use in a VNC sandbox | I prevent the agent from accessing my actual desktop |

---

## 6. CLI Surface

```
tag computer-use run \
  --goal "Navigate to example.com and extract the page title" \
  [--model claude-sonnet-4-6] \
  [--max-actions 50] \
  [--allow-actions screenshot,key,type,left_click,scroll] \
  [--display-width 1280] \
  [--display-height 800] \
  [--sandbox] \
  [--timeout 300]

tag computer-use replay <session-id> [--delay 0.5]
tag computer-use list [--since DURATION]
tag computer-use show <session-id>
tag computer-use stop <session-id>

Options:
  --goal TEXT           Natural language goal for the computer-use session
  --model MODEL         Claude model with computer use support (default: claude-sonnet-4-6)
  --max-actions N       Maximum number of actions before stopping (default: 50)
  --allow-actions LIST  Comma-separated action types allowed (default: all)
  --display-width N     Virtual display width in pixels (default: 1280)
  --display-height N    Virtual display height in pixels (default: 800)
  --sandbox             Run in a sandboxed VNC virtual desktop (PRD-120)
  --timeout N           Session timeout in seconds (default: 300)
  --delay FLOAT         Delay between actions in seconds (default: 0.5)
```

---

## 7. Functional Requirements

| ID | Requirement |
|----|------------|
| FR-01 | `tag computer-use run` creates a `computer_use_sessions` SQLite row and launches the PRD-119 screenshot-action loop. |
| FR-02 | Action allow-list: before dispatching any model-requested action, check action type against allow-list; reject with error response if not allowed. |
| FR-03 | Each screenshot is base64-encoded and passed to the Claude API as an `image` content block with `type: base64`. |
| FR-04 | Each action dispatched is recorded in `computer_use_actions` SQLite table with type, coordinates, text, and timestamp. |
| FR-05 | `--sandbox` flag: start a PRD-120 VNC sandbox desktop before the loop; all actions dispatched to the VNC display. |
| FR-06 | `--max-actions` hard stop: after N actions, stop the loop and report the current state. |
| FR-07 | `--timeout` hard stop: if the session runs longer than N seconds, stop and report. |
| FR-08 | `tag computer-use replay <id>` queries `computer_use_screenshots` for the session and displays them in sequence with `--delay` between frames. |
| FR-09 | `tag computer-use show <id>` renders session metadata + action log (type, coordinates, text, timestamp). |
| FR-10 | Screenshots are also attached to the current OTel span (PRD-041) as binary blob attachments. |

---

## 8. Non-Functional Requirements

| ID | Requirement |
|----|------------|
| NFR-01 | Screenshot images stored as PNG blobs in `computer_use_screenshots` SQLite table (not as files) for portability. |
| NFR-02 | Action dispatch must complete in < 200ms (screenshot capture time excluded). |
| NFR-03 | Session state updated after every action; a crash during the loop does not lose prior action history. |
| NFR-04 | `--allow-actions` validation at session start; reject unknown action types immediately. |

---

## 9. Technical Design

### 9.1 SQLite DDL

```sql
CREATE TABLE IF NOT EXISTS computer_use_sessions (
  id              TEXT PRIMARY KEY,
  goal            TEXT NOT NULL,
  model           TEXT NOT NULL,
  status          TEXT NOT NULL DEFAULT 'running',
  action_count    INTEGER NOT NULL DEFAULT 0,
  max_actions     INTEGER NOT NULL DEFAULT 50,
  allow_actions   TEXT,
  display_width   INTEGER NOT NULL DEFAULT 1280,
  display_height  INTEGER NOT NULL DEFAULT 800,
  sandbox         INTEGER NOT NULL DEFAULT 0,
  created_at      TEXT NOT NULL,
  completed_at    TEXT
);

CREATE TABLE IF NOT EXISTS computer_use_actions (
  id          TEXT PRIMARY KEY,
  session_id  TEXT NOT NULL,
  action_num  INTEGER NOT NULL,
  action_type TEXT NOT NULL,
  coordinates TEXT,  -- JSON {"x": 100, "y": 200}
  text        TEXT,
  created_at  TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS computer_use_screenshots (
  id          TEXT PRIMARY KEY,
  session_id  TEXT NOT NULL,
  action_num  INTEGER NOT NULL,
  image_blob  BLOB NOT NULL,
  created_at  TEXT NOT NULL
);
```

### 9.2 Action allow-list enforcement

```python
VALID_ACTIONS = frozenset({
    "screenshot", "key", "type", "left_click", "right_click",
    "double_click", "scroll", "move", "drag", "cursor_position"
})

def check_allow_list(action_type: str, allow_list: Optional[set]) -> bool:
    if action_type not in VALID_ACTIONS:
        return False
    if allow_list is None:
        return True
    return action_type in allow_list
```

---

## 10. Security Considerations

| Risk | Mitigation |
|------|-----------|
| Model requesting arbitrary keyboard/mouse actions | Allow-list enforced before every action dispatch |
| Computer use on production desktop | `--sandbox` strongly recommended; warn if not used |
| Screenshot containing sensitive screen content | Screenshots stored only in local SQLite; not transmitted externally |
| Infinite loop via computer use | `--max-actions` and `--timeout` hard stops |

---

## 11. Testing Strategy

| Layer | Tests |
|-------|-------|
| Unit | Allow-list enforcement; action recording; session state persistence |
| Integration | Mock computer-use loop: screenshot → model mock → action dispatch → assert recording |
| Security | Rejected action type never dispatched; `--max-actions` stops loop |

---

## 12. Acceptance Criteria

| ID | Criterion |
|----|----------|
| AC-01 | `tag computer-use run --goal "test" --max-actions 3` completes after exactly 3 actions |
| AC-02 | Action type not in allow-list returns error response to model |
| AC-03 | All actions recorded in `computer_use_actions` |
| AC-04 | `tag computer-use replay <id>` displays screenshots in sequence |
| AC-05 | `--timeout 10` stops session after 10 seconds |

---

## 13. Dependencies

| Dependency | Reason |
|-----------|--------|
| PRD-119 screenshot-action loop | Core execution loop |
| PRD-120 VNC sandbox | Isolated desktop environment |
| PRD-041 OTel span system | Screenshot attachment |
| Claude claude-sonnet-4-6 | Computer-use capable model |

---

## 14. Open Questions

| ID | Question |
|----|---------|
| OQ-01 | Should computer use sessions support pause/resume (PRD-109 interrupt)? |
| OQ-02 | Should the allow-list be profile-configurable (not just per-session)? |

---

## 15. Complexity & Timeline

**Complexity:** Medium (M)
**Estimated effort:** 5–8 engineer-days

| Phase | Work | Days |
|-------|------|------|
| 1 | SQLite DDL, session management, allow-list enforcement | 2 |
| 2 | Screenshot capture, action dispatch, OTel attachment | 2 |
| 3 | CLI commands, replay, sandbox integration | 2 |
| 4 | Integration tests, security tests | 1 |

