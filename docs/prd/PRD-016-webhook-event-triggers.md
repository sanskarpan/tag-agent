# PRD-016: Webhook Event Triggers & Automation

**Status:** Proposed  
**Priority:** P2  
**Estimated Effort:** L (3–4 weeks)  
**Affects:** `controller.py` (new `cmd_hooks`), new `tag/events.py`, `tag.sqlite3`

---

## 1. Overview

TAG currently operates in a fully synchronous, user-initiated model. There is no way to react to events: no notification when a run fails, no Slack message when coder completes a PR, no auto-retry when memory usage exceeds a threshold. This PRD adds an event emission system and a conditional automation layer — YAML-defined rules that trigger agent actions or outbound webhooks when lifecycle events occur.

---

## 2. Problem Statement

- Long-running agent tasks complete silently with no outbound notification.
- There is no integration point for CI/CD systems to receive agent results.
- Budget overruns, tool failures, and agent errors happen silently.
- Users who want "on task complete → post to Slack" must write custom shell scripts.
- Hermes v0.16.0 added a webhook creator in its admin panel but TAG doesn't expose it.

---

## 3. Goals

1. TAG emits typed events at lifecycle points: `run.started`, `run.completed`, `run.failed`, `step.completed`, `budget.warning`, `budget.exceeded`, `tool.failed`, `memory.saved`.
2. Users define hooks in `default.yaml` that fire on matching events.
3. Built-in actions: `webhook` (HTTP POST), `notify` (desktop notification), `slack`, `run_command` (shell), `tag_submit` (submit new task).
4. `tag hooks list` shows configured hooks and their last trigger status.
5. `tag hooks test <name>` fires the hook manually with sample data.
6. Hook execution is async (non-blocking) with retries and dead-letter logging.

---

## 4. Non-Goals

- Real-time event streaming (batch processing with sub-10s latency is acceptable).
- Building a message broker (hooks fire via simple HTTP/subprocess).
- Event-sourced architecture changes.

---

## 5. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|-----------|----------|
| U1 | Developer | get a Slack message when coder finishes | I review output without polling |
| U2 | DevOps | POST run results to my webhook URL | CI receives pass/fail signal |
| U3 | Developer | auto-retry a failed run once | transient errors self-heal |
| U4 | Manager | get a notification when monthly budget hits 80% | I can adjust before hitting the cap |
| U5 | Developer | fire `tag submit "run tests"` when coder completes | pipeline continues automatically |

---

## 6. Technical Design

### 6.1 Schema: `events` and `hook_log` tables

```sql
CREATE TABLE IF NOT EXISTS events (
    id          TEXT PRIMARY KEY,
    event_type  TEXT NOT NULL,   -- 'run.completed', 'budget.warning', etc.
    profile     TEXT,
    run_id      TEXT,
    payload     TEXT NOT NULL,   -- JSON
    created_at  TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS hook_log (
    id          TEXT PRIMARY KEY,
    hook_name   TEXT NOT NULL,
    event_id    TEXT NOT NULL,
    status      TEXT NOT NULL,   -- 'ok' | 'failed' | 'retrying'
    response    TEXT,            -- HTTP response or command output
    fired_at    TEXT NOT NULL
);
```

### 6.2 Event types

```python
class EventType:
    RUN_STARTED = "run.started"
    RUN_COMPLETED = "run.completed"
    RUN_FAILED = "run.failed"
    STEP_COMPLETED = "step.completed"
    BUDGET_WARNING = "budget.warning"
    BUDGET_EXCEEDED = "budget.exceeded"
    TOOL_FAILED = "tool.failed"
    MEMORY_SAVED = "memory.saved"
    QUEUE_JOB_COMPLETED = "queue.job.completed"
```

### 6.3 New module: `src/tag/events.py`

```python
"""Event emission and hook firing for TAG lifecycle events."""
from __future__ import annotations
import json, sqlite3, threading, time, uuid
from typing import Any, Callable

def emit_event(
    db: sqlite3.Connection,
    event_type: str,
    payload: dict[str, Any],
    *,
    profile: str | None = None,
    run_id: str | None = None,
) -> str:
    """Record an event and fire matching hooks asynchronously."""
    event_id = uuid.uuid4().hex[:16]
    db.execute("""
        INSERT INTO events (id, event_type, profile, run_id, payload, created_at)
        VALUES (?, ?, ?, ?, ?, ?)
    """, (event_id, event_type, profile, run_id, json.dumps(payload), utc_now()))
    db.commit()
    
    # Fire hooks in background thread
    threading.Thread(
        target=_fire_hooks,
        args=(db, event_type, payload, event_id),
        daemon=True,
    ).start()
    
    return event_id


def _fire_hooks(
    db: sqlite3.Connection,
    event_type: str,
    payload: dict[str, Any],
    event_id: str,
) -> None:
    """Fire all hooks matching the event type."""
    cfg = load_config(config_path(None))
    hooks = cfg.get("hooks", [])
    
    for hook in hooks:
        if not _hook_matches(hook, event_type, payload):
            continue
        _execute_hook(db, hook, payload, event_id)


def _hook_matches(hook: dict, event_type: str, payload: dict) -> bool:
    """Check if hook's event filter matches."""
    pattern = hook.get("on", "")
    if pattern == "*":
        return True
    if pattern == event_type:
        return True
    # Glob-style: "run.*" matches "run.completed", "run.failed"
    if pattern.endswith(".*"):
        prefix = pattern[:-2]
        return event_type.startswith(prefix + ".")
    # Conditional: "run.failed AND profile == coder"
    # Simple eval-free implementation
    return False


def _execute_hook(
    db: sqlite3.Connection,
    hook: dict,
    payload: dict[str, Any],
    event_id: str,
) -> None:
    """Execute a single hook action."""
    action_type = hook.get("action", {}).get("type")
    action = hook.get("action", {})
    status = "ok"
    response = ""
    
    try:
        if action_type == "webhook":
            _fire_webhook(action["url"], payload)
            response = "200"
        
        elif action_type == "slack":
            _fire_slack(action["webhook_url"], action.get("message", ""), payload)
        
        elif action_type == "notify":
            from tag.tui_output import _send_notification
            msg = action.get("message", f"TAG: {payload.get('event_type', '')}")
            _send_notification("TAG", _interpolate(msg, payload))
        
        elif action_type == "run_command":
            cmd = _interpolate(action["command"], payload)
            result = subprocess.run(cmd, shell=True, capture_output=True, text=True, timeout=30)
            response = result.stdout[-200:]
            if result.returncode != 0:
                status = "failed"
        
        elif action_type == "tag_submit":
            task = _interpolate(action["task"], payload)
            subprocess.Popen([sys.executable, "-m", "tag", "submit", task])
    
    except Exception as exc:
        status = "failed"
        response = str(exc)
    
    db.execute("""
        INSERT INTO hook_log (id, hook_name, event_id, status, response, fired_at)
        VALUES (?, ?, ?, ?, ?, ?)
    """, (uuid.uuid4().hex[:16], hook.get("name", "unnamed"), event_id, status, response, utc_now()))
    db.commit()


def _interpolate(template: str, payload: dict) -> str:
    """Simple {{key}} substitution from payload."""
    for key, value in payload.items():
        template = template.replace(f"{{{{{key}}}}}", str(value))
    return template


def _fire_webhook(url: str, payload: dict) -> None:
    data = json.dumps(payload).encode()
    req = urllib.request.Request(url, data=data, method="POST",
                                  headers={"Content-Type": "application/json"})
    urllib.request.urlopen(req, timeout=10)


def _fire_slack(webhook_url: str, message_template: str, payload: dict) -> None:
    message = _interpolate(message_template or "TAG event: {{event_type}}", payload)
    _fire_webhook(webhook_url, {"text": message})
```

### 6.4 default.yaml schema extension

```yaml
hooks:
  - name: notify-on-complete
    on: run.completed
    action:
      type: notify
      message: "{{profile}} completed: {{task}}"
  
  - name: slack-on-failure
    on: run.failed
    action:
      type: slack
      webhook_url: "${SLACK_WEBHOOK_URL}"
      message: ":x: TAG agent failed: {{profile}} — {{error}}"
  
  - name: ci-webhook
    on: "run.*"
    action:
      type: webhook
      url: "${CI_WEBHOOK_URL}"
  
  - name: auto-retry
    on: run.failed
    conditions:
      max_retries: 1
    action:
      type: tag_submit
      task: "{{task}}"
```

### 6.5 `cmd_hooks` command

```
tag hooks list                — show all hooks and last status
tag hooks test <name>         — fire hook with sample payload
tag hooks log [--hook NAME]   — show hook fire history
tag hooks enable/disable NAME — toggle a hook
```

---

## 7. Implementation Plan

| Step | Task |
|------|------|
| 1 | Add `events` and `hook_log` tables to `open_db()` |
| 2 | Create `src/tag/events.py` with `emit_event`, `_fire_hooks`, action executors |
| 3 | Add `emit_event` calls to `insert_run`, `update_run_status`, `insert_step` |
| 4 | Add `hooks` schema to `default.yaml` |
| 5 | Implement `cmd_hooks` with list/test/log/enable/disable |
| 6 | Register `hooks` parser |
| 7 | Tests: `test_hook_fires_on_run_completed`, `test_webhook_action_posts_json`, `test_hook_matches_glob` |
| 8 | Document hooks in README |

---

## 8. Success Metrics

- `run.completed` event emitted after every successful `tag submit`.
- `notify` action fires desktop notification on macOS.
- `webhook` action POSTs correct JSON to webhook URL.
- `tag hooks log` shows fire history.

---

## 9. Risks & Mitigations

| Risk | Mitigation |
|------|-----------|
| Hook fires block main thread | All hook execution is in background daemon threads |
| Webhook URL in config leaks in logs | Mask URL in hook_log; show only domain |
| Auto-retry loop causes infinite retries | `max_retries: N` condition enforced per event_id |
| Hook executor crashes silently | All exceptions caught, logged to hook_log with `status: failed` |
