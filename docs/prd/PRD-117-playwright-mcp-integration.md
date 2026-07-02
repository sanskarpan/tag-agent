# PRD-117: Playwright MCP Integration (`tag playwright`)

> **Stack: Go** (native single-binary; see docs/GO_MIGRATION_RESEARCH.md). This PRD was re-framed from Python to Go.

**Status:** Proposed
**Priority:** P1
**Estimated Effort:** M (5-8 days)
**Category:** Computer Use
**Affects:** `internal/mcp (Playwright MCP client facade) + internal/tool + internal/cli`
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
| FR-01 | `tag playwright setup` ensures the Playwright MCP server binary is resolvable (default `npx -y @playwright/mcp@latest`), triggers browser-binary download, and writes a `playwright-mcp` server entry into the embedded MCP registry (`internal/mcp/registry`, `go:embed registry.yaml`) / `cli-config.yaml`. No Python/pip is involved: the Go binary shells out to the external `npx` launcher via `os/exec.CommandContext`. |
| FR-02 | `tag playwright start` launches the Playwright MCP server as an out-of-process child via `os/exec.CommandContext` with `SysProcAttr{Setpgid: true}`, connects to it as an MCP client through the `internal/mcp` go-sdk facade (`CommandTransport`/stdio, or `StreamableHTTP`/SSE for a remote server), and persists the PID and transport endpoint in the `playwright_sessions` SQLite table (`internal/store`, modernc.org/sqlite). |
| FR-03 | `tag playwright screenshot --url URL` calls the MCP `browser_navigate` then `browser_screenshot` tool via the go-sdk `ClientSession.CallTool`, saves the returned PNG to disk, and attaches it to the current OTel span. |
| FR-04 | Browser session PID and transport endpoint persisted in SQLite; subsequent `tag run` calls with the same session ID reuse the running session (reconnect the go-sdk client to the existing endpoint). |
| FR-05 | `tag playwright close` issues the `browser_close` MCP tool call, closes the `ClientSession`, and removes the session row. |
| FR-06 | `tag playwright list` queries `playwright_sessions` and shows: session_id, browser, headless, endpoint, pid, started_at. |
| FR-07 | `--sandbox` mode: launch the MCP server / browser through the `internal/sandbox` ladder (landlock+seccomp+nftables → docker → gVisor → firecracker on Linux; degrade to sandbox-exec/Docker Desktop off-Linux); all filesystem writes go to the sandbox volume. |
| FR-08 | Every tool call routed through the Playwright server is dispatched behind the unified `internal/tool` interface (`Info()`/`Run(ctx, ToolCall)`/`ProviderOptions()`), wrapped in a `PLAYWRIGHT` span type, with a `browser.action` attribute and optional screenshot blob. |
| FR-09 | `tag playwright eval --js "..."` calls the `browser_evaluate` MCP tool and returns the result marshaled with `encoding/json`. |
| FR-10 | Session health check: before reusing a stored session, verify the MCP server process group is still alive (`syscall.Kill(pid, 0)` on Unix); restart via FR-02 if dead. |

---

## 8. Non-Functional Requirements

| ID | Requirement |
|----|------------|
| NFR-01 | Playwright MCP server child supervised via `os/exec.CommandContext` with `Setpgid: true`; on `tag playwright close` (or ctx cancel) the whole process group is signalled (`syscall.Kill(-pgid, SIGTERM)`) so `npx`-spawned node children are not orphaned. |
| NFR-02 | Screenshots stored in `~/.tag/screenshots/` with session-scoped subdirectory (mode 0700); auto-pruned after 30 days. |
| NFR-03 | Headless mode is the default; `--headed` requires user confirmation in non-interactive sessions. |
| NFR-04 | Browser-binary download output suppressed unless `--verbose`; only final status shown. |

---

## 9. Technical Design

### 9.1 Architecture

TAG does **not** implement a browser driver. The Playwright MCP server is an existing out-of-process MCP server (`@playwright/mcp`, launched via `npx`). TAG connects to it **as an MCP client** through the TAG-owned facade in `internal/mcp`, which wraps `github.com/modelcontextprotocol/go-sdk` v1.6.1 (GA; client + server; stdio `CommandTransport` and `StreamableHTTP`/SSE). The MCP protocol version is pinned as a single const in `internal/mcp` (`ProtocolVersion = "2025-11-25"`).

Each Playwright MCP tool (`browser_navigate`, `browser_click`, `browser_screenshot`, `browser_evaluate`, …) is surfaced to the agent through the unified `internal/tool` interface (`Info()`/`Run(ctx, ToolCall)`/`ProviderOptions()`), namespaced `mcp_playwright_<tool>`, with every `Run()` gated by the `internal/tool` permission engine.

If a future variant needs an **in-process, no-Node** path, the direct-driver alternative is `github.com/playwright-community/playwright-go` (bundled driver) or `github.com/chromedp/chromedp` (CDP). The MCP-client path is primary; the direct driver is an out-of-scope fallback noted for the extensibility discussion below.

> **Extensibility note (Plan decision #6):** the dynamic Python-plugin style ("drop a `playwright_tools.py` module") does **not** map to the static Go binary. The only supported extension surfaces are MCP servers (this PRD) plus shell/HTTP hooks. Custom browser tools are added by registering another MCP server, not by loading Go plugins at runtime.

### 9.2 SQLite DDL (`internal/store`, modernc.org/sqlite)

```sql
CREATE TABLE IF NOT EXISTS playwright_sessions (
  id          TEXT PRIMARY KEY,
  browser     TEXT NOT NULL DEFAULT 'chromium',
  headless    INTEGER NOT NULL DEFAULT 1,
  sandbox     INTEGER NOT NULL DEFAULT 0,
  endpoint    TEXT,            -- stdio marker or StreamableHTTP URL
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

### 9.3 Go core

```go
package playwright // internal/mcp/playwright

import (
	"context"
	"database/sql"
	"os/exec"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// isoUTC matches the timestamp format used across the store for byte-parity:
// microsecond precision with a "+00:00" offset.
func isoUTC() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000000+00:00") }

// Manager owns Playwright MCP client sessions and their persistence.
type Manager struct {
	db  *sql.DB       // internal/store handle (single writer, flock + atomic RMW)
	mcp *mcp.Client   // TAG-owned go-sdk client facade (internal/mcp)
}

type Session struct {
	ID       string
	Browser  string
	Headless bool
	Sandbox  bool
	PID      int
	cmd      *exec.Cmd
	client   *mcp.ClientSession
}

// Start launches the out-of-process Playwright MCP server and connects to it.
func (m *Manager) Start(ctx context.Context, browser string, headless, sandbox bool) (*Session, error) {
	id := uuid.NewString()[:8]

	args := []string{"-y", "@playwright/mcp@latest", "--browser=" + browser}
	if headless {
		args = append(args, "--headless")
	}
	cmd := exec.CommandContext(ctx, "npx", args...)
	// Own the process group so npx-spawned node children die with us (NFR-01).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// go-sdk stdio transport over the child's stdin/stdout.
	transport := &mcp.CommandTransport{Command: cmd}
	cs, err := m.mcp.Connect(ctx, transport, nil)
	if err != nil {
		return nil, err
	}

	now := isoUTC()
	if _, err := m.db.ExecContext(ctx,
		`INSERT INTO playwright_sessions
		   (id,browser,headless,sandbox,pid,status,created_at,last_used_at)
		 VALUES (?,?,?,?,?,?,?,?)`,
		id, browser, b2i(headless), b2i(sandbox), cmd.Process.Pid, "running", now, now,
	); err != nil {
		return nil, err
	}
	return &Session{ID: id, Browser: browser, Headless: headless, Sandbox: sandbox,
		PID: cmd.Process.Pid, cmd: cmd, client: cs}, nil
}

// Close issues browser_close, drops the client, and kills the process group.
func (m *Manager) Close(ctx context.Context, id string) error {
	var pid int
	if err := m.db.QueryRowContext(ctx,
		`SELECT pid FROM playwright_sessions WHERE id=?`, id).Scan(&pid); err != nil {
		return err
	}
	if pid > 0 {
		_ = syscall.Kill(-pid, syscall.SIGTERM) // whole group; ignore ESRCH
	}
	_, err := m.db.ExecContext(ctx,
		`UPDATE playwright_sessions SET status='closed' WHERE id=?`, id)
	return err
}

// IsAlive checks whether the MCP server process group is still running.
func IsAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil // ESRCH => dead
}

func b2i(b bool) int { if b { return 1 }; return 0 }
```

`Screenshot` navigates and captures via the go-sdk client, then persists + attaches to the OTel span:

```go
func (s *Session) Screenshot(ctx context.Context, url string) (*mcp.CallToolResult, error) {
	if _, err := s.client.CallTool(ctx, &mcp.CallToolParams{
		Name: "browser_navigate", Arguments: map[string]any{"url": url},
	}); err != nil {
		return nil, err
	}
	return s.client.CallTool(ctx, &mcp.CallToolParams{Name: "browser_screenshot"})
}
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
| Unit | Table-driven `go test` for session start/close/IsAlive; PID/process-group health check on a dead process; against a fake in-process MCP server (go-sdk `mcp.Server` over an in-memory transport) so no real browser is needed |
| Integration | Full flow: setup → start → navigate → screenshot → close, asserting process-group teardown leaves no orphaned node children |
| Benchmark | `testing.B` for screenshot capture latency (Success Metrics target < 2s) |
| Security | Sandbox mode (`internal/sandbox` ladder) cannot write to host filesystem |

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
| `@playwright/mcp` (external, launched via `npx`) | Out-of-process Playwright MCP server TAG connects to as a client |
| `github.com/modelcontextprotocol/go-sdk` v1.6.1 | GA MCP client (stdio `CommandTransport` + StreamableHTTP/SSE); protocol pin `2025-11-25` |
| `github.com/google/uuid` | Session IDs |
| `modernc.org/sqlite` (via `internal/store`) | Session + screenshot persistence (pure-Go, no CGO) |
| `github.com/playwright-community/playwright-go` **or** `github.com/chromedp/chromedp` | Optional in-process/no-Node direct-driver fallback (out of scope for v1) |
| PRD-073 MCP registry sync | MCP server configuration infrastructure (`internal/mcp/registry`) |
| PRD-028 sandbox execution | Sandbox browser mode (`internal/sandbox` ladder) |
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
| 1 | `playwright setup`, session management SQLite DDL (`internal/store`) | 1 |
| 2 | `internal/mcp/playwright` `Manager` start/close/health-check over the go-sdk client facade | 2 |
| 3 | Screenshot capture + span attachment | 1 |
| 4 | CLI commands, sandbox mode, integration tests | 2 |

