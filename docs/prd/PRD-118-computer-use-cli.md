# PRD-118: Computer Use CLI (`tag computer-use`)

> **Stack: Go** (native single-binary; see docs/GO_MIGRATION_RESEARCH.md). This PRD was re-framed from Python to Go.

**Status:** Proposed
**Priority:** P1
**Estimated Effort:** M (5-8 days)
**Category:** Computer Use
**Affects:** `internal/agent (computer-use loop) + internal/llm (vision) + internal/sandbox + internal/cli`
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
| FR-01 | `tag computer-use run` creates a `computer_use_sessions` SQLite row (`internal/store`) and launches the PRD-119 screenshot→vision→action loop in `internal/agent`. |
| FR-02 | Action allow-list: before dispatching any model-requested action, check the action type against the allow-list; reject with a tool error result returned to the model if not allowed. |
| FR-03 | Each screenshot is passed to the vision provider (`internal/llm`) as an image content block; the `internal/llm/anthropic` adapter over `anthropics/anthropic-sdk-go` v1.55.x sends it as a base64 image source alongside the `computer_use` tool result. |
| FR-04 | Each action dispatched is recorded in the `computer_use_actions` SQLite table with type, coordinates (JSON), text, and timestamp. |
| FR-05 | `--sandbox` flag: bring up a PRD-120 VNC sandbox desktop via the `internal/sandbox` ladder before the loop; all actions are dispatched to the VNC display through the desktop driver (`chromedp`/`playwright-go` for browser targets). |
| FR-06 | `--max-actions` hard stop: after N actions, stop the loop and report the current state. |
| FR-07 | `--timeout` hard stop: enforced with `context.WithTimeout`; if the session ctx expires, stop and report. |
| FR-08 | `tag computer-use replay <id>` queries `computer_use_screenshots` for the session and displays them in sequence with `--delay` between frames. |
| FR-09 | `tag computer-use show <id>` renders session metadata + action log (type, coordinates, text, timestamp). |
| FR-10 | Screenshots are also attached to the current OTel span (`go.opentelemetry.io/otel`, PRD-041) as binary blob attachments. |

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

### 9.1 Architecture

The session is a **screenshot → vision → action loop** driven by `internal/agent`, calling the Claude *computer use* tool through the provider-neutral vision interface in `internal/llm` (`internal/llm/anthropic` over `anthropics/anthropic-sdk-go` v1.55.x, using the `computer_use` tool + image content blocks). Each turn:

1. Capture a screenshot from the target display (VNC sandbox via `internal/sandbox`; browser targets driven by `github.com/chromedp/chromedp` or `github.com/playwright-community/playwright-go`).
2. Send the screenshot as an image block + prior tool results to the model; the model responds with a `computer_use` tool call (action + coordinates/text).
3. Gate the action through the allow-list and the `internal/tool` permission engine, then dispatch it to the driver.
4. Persist the action + screenshot to `internal/store` and attach the screenshot to the OTel span.

Loop control (`--max-actions`, `--timeout`, doom-loop/interrupt) reuses the bounded-loop primitives in `internal/agent`; `--timeout` is a `context.WithTimeout` on the loop ctx. Untrusted actions are gated through the `internal/sandbox` ladder (landlock+seccomp+nftables → docker → gVisor → firecracker on Linux; degrade off-Linux) plus the permission gate.

### 9.2 SQLite DDL (`internal/store`, modernc.org/sqlite)

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

### 9.3 Action allow-list enforcement

```go
package computeruse // internal/agent/computeruse

// validActions is the set of computer_use tool action types TAG understands.
var validActions = map[string]struct{}{
	"screenshot": {}, "key": {}, "type": {}, "left_click": {}, "right_click": {},
	"double_click": {}, "scroll": {}, "move": {}, "drag": {}, "cursor_position": {},
}

// checkAllowList reports whether an action type may be dispatched.
// A nil allow set means "all valid actions are allowed".
func checkAllowList(actionType string, allow map[string]struct{}) bool {
	if _, ok := validActions[actionType]; !ok {
		return false
	}
	if allow == nil {
		return true
	}
	_, ok := allow[actionType]
	return ok
}
```

### 9.4 Loop skeleton

```go
// Run drives one computer-use session until goal completion, max-actions,
// timeout, or interrupt. vision is the internal/llm provider; drv is the
// display/browser driver; store persists actions + screenshots.
func (s *Session) Run(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, s.timeout) // FR-07
	defer cancel()

	for n := 0; n < s.maxActions; n++ { // FR-06
		shot, err := s.drv.Screenshot(ctx)
		if err != nil {
			return err
		}
		s.store.SaveScreenshot(ctx, s.id, n, shot) // FR-04/FR-10 (+ OTel span attach)

		call, err := s.vision.NextAction(ctx, s.goal, shot) // anthropic-sdk-go computer_use tool
		if err != nil {
			return err
		}
		if call.Stop {
			return nil
		}
		if !checkAllowList(call.Action, s.allow) { // FR-02
			s.vision.RejectAction(call, "action not permitted")
			continue
		}
		if err := s.drv.Dispatch(ctx, call); err != nil {
			return err
		}
		s.store.SaveAction(ctx, s.id, n, call)
	}
	return errMaxActions
}
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
| Unit | Table-driven `go test` for `checkAllowList`; action recording; session state persistence across a simulated crash |
| Integration | Loop against a fake `internal/llm` vision provider + fake driver: screenshot → mocked tool call → action dispatch → assert recording |
| Benchmark | `testing.B` for loop-iteration and screenshot-capture latency (Success Metrics: cycle < 3s, capture < 500ms) |
| Security | Rejected action type never dispatched; `--max-actions` and `context` `--timeout` stop the loop |

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
| `github.com/anthropics/anthropic-sdk-go` v1.55.x (via `internal/llm/anthropic`) | Claude `computer_use` tool + image content blocks |
| `github.com/chromedp/chromedp` **or** `github.com/playwright-community/playwright-go` | Display/browser action driver |
| `modernc.org/sqlite` (via `internal/store`) | Session/action/screenshot persistence (pure-Go) |
| `github.com/google/uuid` | Session/action IDs |
| PRD-119 screenshot-action loop | Core execution loop (`internal/agent`) |
| PRD-120 VNC sandbox | Isolated desktop environment (`internal/sandbox`) |
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

