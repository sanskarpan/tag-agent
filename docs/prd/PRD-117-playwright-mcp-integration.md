# PRD-117: Playwright MCP Integration (`tag playwright`)

**Status:** Proposed
**Priority:** P1
**Estimated Effort:** M (5-8 days)
**Category:** Computer Use
**Affects:** `playwright_tools.py + controller.py`
**Depends on:** PRD-073 (live MCP registry sync), PRD-074 (MCP OAuth/PKCE), PRD-028 (sandbox code execution)
**Inspired by:** Playwright MCP server, Microsoft playwright-mcp, BrowserBase, Stagehand browser automation

---

## 1. Overview

Web browser automation is one of the most high-value capabilities for AI agents: scraping real-time data, filling forms, navigating authenticated apps, running end-to-end tests. The Playwright MCP server (open-source, 7k+ GitHub stars as of 2025) exposes browser control as MCP tools (`browser_navigate`, `browser_click`, `browser_screenshot`, `browser_fill`, `browser_evaluate`), making it the natural integration point for TAG's MCP ecosystem (PRD-073).

However, TAG currently has no first-class integration with the Playwright MCP server: there is no pre-configured server entry, no browser session management, no screenshot capture to the TAG span system, and no safe-mode for running browser automation in a sandbox (PRD-028). Engineers must manually configure the Playwright MCP server in their `cli-config.yaml` and manage browser sessions externally.

Playwright MCP Integration (`tag playwright`) adds a pre-packaged Playwright MCP server configuration to TAG, a `tag playwright` CLI for session management (launch, screenshot, close), automatic screenshot capture to the OTel span system (PRD-041), a safe sandbox execution mode, and a `--headless` default for CI/CD use.

---

## 2. Problem Statement

### 2.1 No out-of-box browser automation

Engineers wanting browser automation must: (1) install `playwright-mcp`, (2) configure the MCP server in YAML, (3) install browser binaries, (4) manage server lifecycle. This is 15+ minutes of setup for a basic capability.

### 2.2 No browser session management

The Playwright MCP server is stateless — it does not manage session state between TAG tool calls. Long browser workflows (navigate → fill form → wait for redirect → extract data) require manual session tracking.

### 2.3 Screenshots not captured in observability

When an agent uses browser tools, the screenshots taken during automation are not captured in the TAG span system (PRD-041). Debugging failed browser automation requires manual log inspection.

---

## 3. Goals

| ID | Goal |
|----|------|
| G1 | `tag playwright setup` installs Playwright, downloads browser binaries, and adds the MCP server to `cli-config.yaml`. |
| G2 | `tag playwright start [--headless] [--browser chromium|firefox|webkit]` starts a managed browser session. |
| G3 | `tag playwright screenshot [--url URL]` navigates to URL and captures a screenshot to disk and to the TAG span system. |
| G4 | `tag playwright close [--session-id ID]` closes a browser session gracefully. |
| G5 | All browser actions taken via MCP tools are logged to the TAG span system with screenshot attachments. |
| G6 | Sandbox mode: `tag playwright start --sandbox` runs the browser in a PRD-028 sandbox container (no local filesystem access). |
| G7 | Session persistence: a Playwright session can survive across multiple `tag run` invocations within a session ID. |

## 3.1 Non-Goals

| ID | Non-Goal |
|----|----------|
| NG1 | Replacing Playwright MCP server with a custom implementation. TAG wraps the existing server. |
| NG2 | Mobile browser automation (desktop browsers only). |
| NG3 | Video recording of browser sessions. |
| NG4 | Automated test generation from browser interactions. |

---

## 4. Success Metrics

| Metric | Target | Measurement |
|--------|--------|-------------|
| Setup time | `tag playwright setup` completes in < 3 minutes on a clean machine | Manual timing |
| Screenshot latency | `tag playwright screenshot` captures and saves screenshot in < 2s | Benchmark test |
| Session persistence | Browser session survives 5 `tag run` invocations without restart | Integration test |
| Sandbox isolation | Sandbox browser cannot write to host filesystem | Security test |

---

## 5. User Stories

| ID | As a... | I want to... | So that... |
|----|---------|-------------|------------|
| US1 | Developer | Set up browser automation with one command | I start automating without manual configuration |
| US2 | Agent | Use `browser_screenshot` and have it logged in the span system | I can debug browser actions from observability UI |
| US3 | Platform engineer | Run browser automation in a sandbox | I prevent agents from accessing the host filesystem |
| US4 | Developer | Start a headless browser session for CI automation | I use browser tools in automated pipelines |
| US5 | QA engineer | Capture screenshots at key workflow steps | I build a visual test record |

---

## 6. CLI Surface

```
tag playwright <subcommand> [options]

Subcommands:
  setup       Install Playwright and configure MCP server
  start       Start a managed browser session
  screenshot  Navigate to URL and capture screenshot
  close       Close a browser session
  list        List active browser sessions
  eval        Execute JavaScript in browser context

tag playwright setup \
  [--browser chromium|firefox|webkit] \
  [--headless / --headed]

tag playwright start \
  [--browser chromium|firefox|webkit] \
  [--headless] \
  [--sandbox] \
  [--session-id ID]

tag playwright screenshot \
  [--url URL] \
  [--session-id ID] \
  [--output PATH]

tag playwright close [--session-id ID | --all]

tag playwright list

tag playwright eval \
  --js "document.title" \
  [--session-id ID]

Options:
  --browser    Browser type (default: chromium)
  --headless   Run without visible browser window (default: true)
  --sandbox    Run in PRD-028 sandbox container
  --session-id Use or create a specific session ID
  --output     Screenshot output path (default: ~/.tag/screenshots/<timestamp>.png)
```

---

## 7. Functional Requirements

| ID | Requirement |
|----|------------|
| FR-01 | `tag playwright setup` runs `pip install playwright`, `playwright install chromium`, and writes a `playwright-mcp` server entry to `cli-config.yaml`. |
| FR-02 | `tag playwright start` launches the Playwright MCP server as a subprocess, stores the PID and WebSocket URL in `playwright_sessions` SQLite table. |
| FR-03 | `tag playwright screenshot --url URL` calls the MCP `browser_navigate` then `browser_screenshot` tool, saves the PNG to disk, and attaches it to the current OTel span. |
| FR-04 | Browser session PID and connection URL persisted in SQLite; subsequent `tag run` calls with the same session ID reuse the running session. |
| FR-05 | `tag playwright close` sends `browser_close` MCP tool call and removes the session row. |
| FR-06 | `tag playwright list` queries `playwright_sessions` and shows: session_id, browser, headless, url, pid, started_at. |
| FR-07 | `--sandbox` mode: launch browser within a PRD-028 sandbox; all filesystem writes go to the sandbox volume. |
| FR-08 | All MCP tool calls made via Playwright server are wrapped in a `PLAYWRIGHT` span type, with `browser.action` attribute and optional screenshot blob. |
| FR-09 | `tag playwright eval --js "..."` calls `browser_evaluate` MCP tool and returns the result as JSON. |
| FR-10 | Session health check: before using a stored session, verify the MCP server process is still running (PID check); restart if dead. |

---

## 8. Non-Functional Requirements

| ID | Requirement |
|----|------------|
| NFR-01 | Playwright MCP server subprocess supervised via `subprocess.Popen`; SIGTERM on `tag playwright close`. |
| NFR-02 | Screenshots stored in `~/.tag/screenshots/` with session-scoped subdirectory; auto-pruned after 30 days. |
| NFR-03 | Headless mode is the default; `--headed` requires user confirmation in non-interactive sessions. |
| NFR-04 | `playwright install` output suppressed unless `--verbose`; only final status shown. |

---

## 9. Technical Design

### 9.1 SQLite DDL

```sql
CREATE TABLE IF NOT EXISTS playwright_sessions (
  id          TEXT PRIMARY KEY,
  browser     TEXT NOT NULL DEFAULT 'chromium',
  headless    INTEGER NOT NULL DEFAULT 1,
  sandbox     INTEGER NOT NULL DEFAULT 0,
  ws_url      TEXT,
  pid         INTEGER,
  status      TEXT NOT NULL DEFAULT 'running',
  created_at  TEXT NOT NULL,
  last_used_at TEXT
);

CREATE TABLE IF NOT EXISTS playwright_screenshots (
  id          TEXT PRIMARY KEY,
  session_id  TEXT NOT NULL,
  url         TEXT,
  file_path   TEXT NOT NULL,
  span_id     TEXT,
  created_at  TEXT NOT NULL
);
```

### 9.2 Python core

```python
from __future__ import annotations
import sqlite3
import subprocess
import sys
import uuid
from pathlib import Path
from typing import Optional

class PlaywrightSession:
    def __init__(self, db_path: str) -> None:
        self.db_path = db_path

    def start(self, browser: str = "chromium", headless: bool = True,
              sandbox: bool = False) -> str:
        session_id = uuid.uuid4().hex[:8]
        cmd = [sys.executable, "-m", "playwright_mcp.server",
               f"--browser={browser}"]
        if headless:
            cmd.append("--headless")
        proc = subprocess.Popen(cmd, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        now = _utc_now()
        conn = sqlite3.connect(self.db_path)
        conn.execute(
            "INSERT INTO playwright_sessions(id,browser,headless,sandbox,pid,status,created_at,last_used_at) "
            "VALUES(?,?,?,?,?,?,?,?)",
            (session_id, browser, int(headless), int(sandbox), proc.pid, "running", now, now)
        )
        conn.commit()
        return session_id

    def close(self, session_id: str) -> None:
        conn = sqlite3.connect(self.db_path)
        row = conn.execute("SELECT pid FROM playwright_sessions WHERE id=?", (session_id,)).fetchone()
        if row and row[0]:
            import signal, os
            try:
                os.kill(row[0], signal.SIGTERM)
            except ProcessLookupError:
                pass
        conn.execute("UPDATE playwright_sessions SET status='closed' WHERE id=?", (session_id,))
        conn.commit()

    def is_alive(self, session_id: str) -> bool:
        conn = sqlite3.connect(self.db_path)
        row = conn.execute("SELECT pid FROM playwright_sessions WHERE id=?", (session_id,)).fetchone()
        if not row or not row[0]:
            return False
        import os
        try:
            os.kill(row[0], 0)
            return True
        except ProcessLookupError:
            return False

def _utc_now() -> str:
    from datetime import datetime, timezone
    return datetime.now(timezone.utc).isoformat()
```

---

## 10. Security Considerations

| Risk | Mitigation |
|------|-----------|
| Browser accessing local files | Sandbox mode routes browser through isolated container; headless default reduces attack surface |
| MCP server exposed on local port | Bind to `127.0.0.1` only; no external network exposure |
| Screenshot containing sensitive screen content | Screenshots stored in `~/.tag/screenshots/` (mode 0700); not transmitted externally |
| Malicious JS in `tag playwright eval` | Eval only in explicit user-initiated calls; not available as autonomous agent tool by default |

---

## 11. Testing Strategy

| Layer | Tests |
|-------|-------|
| Unit | Session start/close/is_alive; PID health check on dead process |
| Integration | Full flow: setup → start → navigate → screenshot → close |
| Security | Sandbox mode cannot write to host filesystem |

---

## 12. Acceptance Criteria

| ID | Criterion |
|----|----------|
| AC-01 | `tag playwright setup` completes and playwright-mcp appears in `cli-config.yaml` |
| AC-02 | `tag playwright start --headless` creates a running session in SQLite |
| AC-03 | `tag playwright screenshot --url https://example.com` produces a PNG file |
| AC-04 | Screenshot is attached to the current OTel span |
| AC-05 | `tag playwright close` terminates the browser process |

---

## 13. Dependencies

| Dependency | Reason |
|-----------|--------|
| `playwright-mcp` PyPI package | MCP server implementation |
| PRD-073 MCP registry sync | MCP server configuration infrastructure |
| PRD-028 sandbox execution | Sandbox browser mode |
| PRD-041 OTel cost attribution | Screenshot span attachment |

---

## 14. Open Questions

| ID | Question |
|----|---------|
| OQ-01 | Should `tag playwright start` be invoked automatically when a browser MCP tool is called without a running session? |
| OQ-02 | Should browser cookies/localStorage persist between sessions for authenticated workflows? |

---

## 15. Complexity & Timeline

**Complexity:** Medium (M)
**Estimated effort:** 5–8 engineer-days

| Phase | Work | Days |
|-------|------|------|
| 1 | `playwright setup`, session management SQLite DDL | 1 |
| 2 | `PlaywrightSession` start/close/health-check | 2 |
| 3 | Screenshot capture + span attachment | 1 |
| 4 | CLI commands, sandbox mode, integration tests | 2 |

