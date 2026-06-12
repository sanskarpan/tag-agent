# PRD-022: IDE Bridge — LSP Server & VS Code Extension

**Status:** Proposed  
**Priority:** P2  
**Estimated Effort:** XL (3–4 sprints, ~8–10 weeks)  
**Affects:** `controller.py` (new `cmd_lsp`), new `src/tag/lsp_server.py`, new `vscode/` directory (extension package)

---

## 1. Overview

TAG agents run at the terminal. The IDE is where developers spend most of their working time. This PRD closes that gap by exposing TAG profile commands as first-class IDE actions via two complementary delivery mechanisms:

1. **An LSP server** (`tag lsp`) — a [Language Server Protocol](https://microsoft.github.io/language-server-protocol/) server implemented in Python using `pygls`. Any LSP-capable editor (VS Code, Neovim, Helix, Zed, Emacs with `lsp-mode`) can connect to it and expose TAG's full profile catalogue as code actions and commands.

2. **A VS Code extension** (`vscode/`) — a thin TypeScript/JavaScript extension that activates the LSP server on workspace open, maps TAG profiles to VS Code code actions and command palette entries, and renders agent output in a dedicated webview panel.

The result: a developer selects a block of code, right-clicks, and sees "Ask TAG: reviewer", "Ask TAG: architect", "Ask TAG: security" — the same profiles they run from the terminal, without leaving their editor. Profile output streams back into a side panel in real time. The entire agent execution path (Hermes, model calls, tools) runs locally via the existing `tag` binary; the IDE extension is purely presentation.

The design deliberately mirrors [continuedev/continue](https://github.com/continuedev/continue)'s architecture: a language server handles protocol mechanics, and editor-specific extensions are thin wrappers. TAG-specific additions are the profile catalogue, selection context injection, and result streaming over LSP `window/logMessage` + `window/showDocument`.

---

## 2. Problem Statement

- TAG's power is locked behind a terminal. Developers who want to review a function, ask a security question, or run an eval must switch windows, paste code, and switch back.
- `tag shell` (PRD-019) improves the TUI experience but does not integrate with the editor's selection context, file position, or diagnostics.
- Competing tools (GitHub Copilot, Cursor, Continue) are embedded in the editor. TAG appears as a separate, manual workflow by comparison.
- The continuedev/continue codebase demonstrates that an LSP bridge is the right abstraction — it works across editors, requires no per-editor plugin rewrite, and keeps agent logic server-side.
- VS Code has 71 % market share among professional developers (Stack Overflow 2024). A VS Code extension is the highest-leverage first integration.

---

## 3. Goals

1. **LSP server implementation** — `tag lsp [--port 7878] [--stdio]` starts a `pygls`-based Language Server that speaks LSP 3.17 and handles the lifecycle methods, `textDocument/codeAction`, and `workspace/executeCommand`.
2. **VS Code extension** — a separate npm package in `vscode/` that auto-starts the LSP server, registers VS Code command palette commands, and provides a webview panel for streaming TAG output.
3. **Code action for "Ask TAG"** — any text selection in any file produces a code action list populated from the active TAG profiles. Selecting one sends the highlighted code + file context to the matching TAG profile.
4. **Inline context injection** — the LSP server reads the selected text range, the file URI, the document language ID, and surrounding lines (configurable window, default ±30 lines) and prepends them as structured context to the agent goal.
5. **Result display in editor panel** — agent output streams back to the editor via a VS Code webview panel with Markdown rendering, syntax-highlighted code blocks, and a copy-to-clipboard button.
6. **Profile switching from command palette** — "TAG: Switch Profile" opens a VS Code quick-pick populated from `tag profile list`. The active profile persists to `.vscode/settings.json` under `tag.defaultProfile`.
7. **`tag lsp status`** — prints the running LSP server's PID, transport mode, port (if TCP), and connected clients.
8. **`pygls` foundation** — use the same Python LSP framework used by `pylsp` and `ruff-lsp`; no custom protocol implementation.

---

## 4. Non-Goals

- **JetBrains plugin** — future. The LSP server works with JetBrains' built-in LSP client (available since 2023.2), but a native plugin with UI is deferred.
- **Neovim integration** — future. The LSP server is fully compatible with `nvim-lspconfig`; a config snippet will be documented, but no dedicated plugin is in scope.
- **Real-time inline suggestions (Copilot-style)** — TAG agents are deliberate, multi-step processes. Sub-200 ms inline completions require a different model tier and interaction model.
- **Remote LSP** — the LSP server runs on the developer's local machine. Remote SSH development (VS Code Remote) is out of scope for v1.
- **Extension marketplace publishing** — the extension will be buildable as a `.vsix` and installable locally. Marketplace submission is deferred pending security and policy review.
- **Streaming token-by-token diff rendering** — the first version shows output after agent completion. Streaming progress notifications are in scope; streaming partial Markdown render is not.
- **Multi-root workspace profiles** — a single TAG profile is active per VS Code workspace. Per-folder profile assignment is deferred.

---

## 5. User Stories

### US-01 — Code action on selected code
> "As a developer, I want to select a function body in VS Code, right-click, and see 'Ask TAG: reviewer' and 'Ask TAG: architect' in the code actions menu, so I can trigger a TAG review without leaving my editor."

**Acceptance:** Selecting any text range produces ≥1 TAG code action entry. Clicking one opens the TAG output panel and the agent begins execution within 2 seconds.

---

### US-02 — Profile switching from command palette
> "As a developer, I want to type 'TAG: Switch Profile' in the VS Code command palette and pick from my configured profiles, so I can change which TAG agent responds to my code actions without editing config files."

**Acceptance:** Command palette shows "TAG: Switch Profile". A quick-pick opens with all profiles from `tag profile list --json`. Selecting one writes `tag.defaultProfile` to `.vscode/settings.json` and confirms the switch with a status bar notification.

---

### US-03 — Streaming response in editor panel
> "As a developer, I want TAG's response to appear in a dedicated side panel with Markdown rendering and syntax-highlighted code blocks, so I can read the agent's output without leaving the editor."

**Acceptance:** A "TAG Output" webview panel opens to the right. Agent output streams into it progressively (progress notifications visible). Final output is fully Markdown-rendered with fenced code blocks syntax-highlighted. A "Copy" button copies the raw text.

---

### US-04 — Trigger a TAG eval from a test file
> "As a developer, I want to right-click inside a failing test function and trigger 'Ask TAG: debugger' to get root cause analysis, so I can diagnose test failures faster."

**Acceptance:** With a test file open, selecting the failing test function and invoking "Ask TAG: debugger" sends the function source + file path + language ID to the debugger profile. The TAG output panel shows the agent's diagnosis.

---

### US-05 — Set up TAG for a new workspace from the IDE
> "As a developer opening a new project, I want the TAG extension to detect that no TAG config exists and prompt me to run `tag setup`, so I can onboard without switching to a terminal."

**Acceptance:** On workspace open, if `~/.tag/config.yaml` does not exist, the extension shows an information notification: "TAG is not configured. Run 'TAG: Setup Workspace' to get started." Clicking it opens an embedded terminal and runs `tag setup`.

---

### US-06 — Inspect agent context before sending
> "As a security-conscious developer, I want to see exactly what text and context will be sent to the TAG agent before it executes, so I can ensure no secrets or sensitive data are included."

**Acceptance:** The "TAG: Preview Context" command shows a VS Code diff-style preview of the structured prompt (selection + surrounding context + file path) that will be sent to the agent. The user can cancel or proceed.

---

## 6. Proposed CLI Surface

### LSP server commands

```
tag lsp [--port 7878] [--stdio]
    Start the TAG LSP server.
    --port PORT     Listen on TCP localhost:PORT (default 7878).
    --stdio         Use stdin/stdout transport instead of TCP.
                    Required for VS Code extension auto-start.
    --log-level     debug|info|warning|error (default: info)
    --config PATH   Path to tag config.yaml (default: auto-discover)

tag lsp status
    Print running LSP server status: PID, transport, port, connected clients.
    Exit code 0 if server is running, 1 if not.
```

**Transport selection rationale:**
- `--stdio` is the standard transport for editor-managed language servers. The VS Code extension spawns `tag lsp --stdio` as a child process and speaks LSP over its stdin/stdout. No port management, no firewall rules.
- `--port` (TCP) is for users who want to run the server persistently in the background (e.g., as a systemd/launchd service) and connect multiple editor instances.

### VS Code extension commands (registered in `package.json`)

```
TAG: Run on Selection          (tag.runOnSelection)
TAG: Switch Profile            (tag.switchProfile)
TAG: Show Output Panel         (tag.showOutputPanel)
TAG: Preview Context           (tag.previewContext)
TAG: Setup Workspace           (tag.setupWorkspace)
TAG: Restart Language Server   (tag.restartServer)
TAG: Open Settings             (tag.openSettings)
```

### VS Code extension package location

```
vscode/
  package.json          # npm package: tag-vscode
  extension.js          # main activation entry point
  client.js             # LanguageClient setup (vscode-languageclient)
  panel.js              # WebviewPanel for TAG output
  README.md             # Extension documentation
  icons/
    tag-logo.png
  test/
    extension.test.js   # @vscode/test-electron test suite
```

---

## 7. Functional Requirements

### FR-01 — LSP server initialization
The `tag lsp` command MUST start a `pygls`-based Language Server that completes the LSP initialization handshake within 1 second of receiving the `initialize` request. The server MUST respond to `initialized`, `shutdown`, and `exit` lifecycle methods correctly.

### FR-02 — Capability advertisement
The server MUST advertise `codeActionProvider: true` and `executeCommandProvider` with the full list of `tag.run.<profile>` commands in the `InitializeResult.capabilities` object.

### FR-03 — `textDocument/codeAction` handler
On any non-empty selection range in any text document, the server MUST return a `CodeAction[]` where each entry has:
- `title`: `"Ask TAG: <profile_name>"` (e.g., `"Ask TAG: reviewer"`)
- `kind`: `CodeActionKind.RefactorRewrite` (or `source.organizeImports` for the security profile — configurable)
- `command`: `{ command: "tag.run.<profile_name>", arguments: [uri, range] }`

The profile list is read from the TAG config at server startup and re-read on `workspace/didChangeConfiguration`.

### FR-04 — `workspace/executeCommand` handler
When the client executes `tag.run.<profile>`, the server MUST:
1. Resolve the document URI to an absolute file path.
2. Extract the selected text from the range.
3. Read ±30 surrounding lines (configurable via `tag.contextLines` workspace setting).
4. Construct a structured goal string (see Technical Design, Section 8).
5. Invoke the TAG profile asynchronously: `tag shell --profile <P> --goal "<goal>"` (or the Python API equivalent).
6. Stream progress via `window/logMessage` notifications.
7. Send the final result via a custom `tag/executionResult` notification so the extension can update the webview panel.

The handler MUST be non-blocking. The server MUST remain responsive to new requests while an agent execution is in progress.

### FR-05 — Profile listing via LSP workspace configuration
The server MUST support `workspace/configuration` requests for the `tag` section. It MUST expose `tag.profiles` (array of profile name strings) and `tag.defaultProfile` (string). On `workspace/didChangeConfiguration`, the server MUST re-read the tag config and refresh the advertised commands if the profile list changed.

### FR-06 — Selection context size limit
The server MUST refuse to send selections larger than 32,000 characters (approximately 8,000 tokens at average tokenization). If the selection exceeds this limit, the server MUST return a `CodeAction` with a warning diagnostic and NOT invoke the agent. The limit MUST be configurable via `tag.maxSelectionChars` workspace setting.

### FR-07 — Progress notifications
During agent execution, the server MUST send `$/progress` notifications (LSP 3.15+) with a `WorkDoneProgressBegin` at start, periodic `WorkDoneProgressReport` messages with partial output, and `WorkDoneProgressEnd` on completion or error. This drives the VS Code spinning progress indicator in the status bar.

### FR-08 — VS Code extension activation events
The extension MUST declare the following activation events in `package.json`:
- `onStartupFinished` — activates on VS Code startup (after extensions are loaded, not blocking startup)
- `onCommand:tag.runOnSelection` — activates on first command invocation if not already active

The extension MUST NOT activate eagerly (no `*` activation event).

### FR-09 — Language Server auto-start
On activation, the extension MUST check whether the `tag` binary is on `$PATH`. If found, it MUST start `tag lsp --stdio` as a child process using `vscode-languageclient`'s `LanguageClient`. If not found, it MUST show a one-time notification: "TAG binary not found. Install with: pip install tag-agent".

### FR-10 — Command palette integration
All 7 extension commands (see Section 6) MUST appear in the VS Code command palette under the "TAG" category. Commands that require an active text selection (e.g., `tag.runOnSelection`, `tag.previewContext`) MUST use `when` clauses to be enabled only when `editorHasSelection` is true.

### FR-11 — Webview panel for TAG output
The extension MUST provide a `WebviewPanel` with:
- Title: "TAG — [profile name]"
- Markdown rendering via the `marked` library (bundled, no CDN)
- Syntax highlighting via `highlight.js` (bundled, no CDN)
- A "Copy" button that copies the raw agent output to the clipboard
- A "New Chat" button that clears the panel and allows a new query
- Persistent panel state: panel reopens to the last output on VS Code restart (using `retainContextWhenHidden`)

### FR-12 — `.vscode/settings.json` integration
The extension MUST write `tag.defaultProfile` to `.vscode/settings.json` when the user switches profiles via the command palette. It MUST read `tag.defaultProfile` from workspace settings on activation. The `tag.lsp.port` and `tag.lsp.transport` settings MUST control whether the extension uses TCP or stdio transport.

### FR-13 — `tag lsp status` command
`tag lsp status` MUST:
- Check for a running LSP server (by PID file at `~/.tag/lsp.pid` or TCP port probe).
- Print: server PID, transport mode, port (TCP only), number of connected clients (if available), uptime.
- Exit with code 0 if running, 1 if not running.

### FR-14 — Graceful shutdown
When VS Code closes or the extension is deactivated, the extension MUST send the LSP `shutdown` request followed by `exit` notification. The `tag lsp` process MUST exit cleanly within 5 seconds of receiving `shutdown`. Any in-progress agent execution MUST be terminated (SIGTERM to the hermes subprocess).

### FR-15 — Configuration schema in `package.json`
The extension MUST declare a JSON schema for all `tag.*` settings in `package.json`'s `contributes.configuration` section. This enables VS Code's settings UI to render dropdowns, descriptions, and validation for TAG settings without opening `settings.json` manually.

### FR-16 — Workspace setup detection
On activation, if `~/.tag/config.yaml` does not exist AND the workspace contains source code files, the extension MUST show a one-time information message: "TAG is not configured for this workspace. Run 'TAG: Setup Workspace' to get started." This message MUST be suppressible and MUST NOT repeat after dismissal (stored in `globalState`).

---

## 8. Non-Functional Requirements

### NFR-01 — LSP server startup time
`tag lsp --stdio` MUST be ready to respond to the LSP `initialize` request within **1 second** on a modern developer machine (MacBook M-series or Intel i7, SSD). The server MUST NOT import heavy dependencies (PyTorch, transformers, etc.) at startup.

### NFR-02 — Non-blocking agent execution
The LSP server MUST remain responsive to new `textDocument/codeAction` requests during an ongoing agent execution. Agent calls MUST run in a separate `asyncio` task or thread. A server that blocks on agent execution is a regression blocker.

### NFR-03 — Extension package size
The VS Code extension `.vsix` package MUST be under **5 MB**. All JS dependencies (marked, highlight.js) MUST be bundled at build time (webpack or esbuild). No runtime npm install.

### NFR-04 — Memory footprint
The idle `tag lsp` process MUST use less than **150 MB RSS** when no agent execution is in progress. The `pygls` event loop itself uses < 20 MB; the ceiling accounts for loaded TAG config and profile metadata.

### NFR-05 — Error resilience
If the TAG binary crashes or an agent execution returns a non-zero exit code, the LSP server MUST NOT crash. It MUST send a `window/showMessage` notification with the error text and continue serving requests.

### NFR-06 — Cross-platform support
The LSP server MUST run on macOS, Linux, and Windows (WSL2). The VS Code extension MUST activate on all three platforms. Path handling MUST use `pathlib.Path` (server) and `vscode.Uri` (extension) throughout — no hardcoded POSIX separators.

### NFR-07 — No network egress at startup
The LSP server and VS Code extension MUST NOT make any network calls during initialization. Profile listing reads local config only. No telemetry, no update checks on startup.

---

## 9. Technical Design

### 9.1 New files

| Path | Description |
|------|-------------|
| `src/tag/lsp_server.py` | `pygls`-based LSP server; all protocol handlers |
| `vscode/package.json` | VS Code extension manifest and npm package definition |
| `vscode/extension.js` | Extension activation entry point |
| `vscode/client.js` | `vscode-languageclient` `LanguageClient` setup |
| `vscode/panel.js` | `WebviewPanel` implementation for TAG output |
| `vscode/README.md` | Extension user documentation |
| `vscode/test/extension.test.js` | `@vscode/test-electron` test suite |

### 9.2 Changes to existing files

| Path | Change |
|------|--------|
| `src/tag/controller.py` | Add `cmd_lsp` and `cmd_lsp_status`; add `lsp` and `lsp status` subparsers |
| `pyproject.toml` | Add `pygls` to new `[lsp]` optional-dependency extra |

### 9.3 LSP server architecture (`src/tag/lsp_server.py`)

```python
# Sketch — not final implementation

from pygls.server import LanguageServer
from lsprotocol.types import (
    TEXT_DOCUMENT_CODE_ACTION,
    WORKSPACE_EXECUTE_COMMAND,
    CodeAction, CodeActionKind, CodeActionParams,
    ExecuteCommandParams,
    WorkDoneProgressBegin, WorkDoneProgressEnd,
)

TAG_SERVER_VERSION = "0.1.0"
server = LanguageServer("tag-lsp", TAG_SERVER_VERSION)

@server.feature(TEXT_DOCUMENT_CODE_ACTION)
def code_action(ls: LanguageServer, params: CodeActionParams) -> list[CodeAction]:
    """Return one CodeAction per TAG profile for any non-empty selection."""
    if _is_empty_range(params.range):
        return []
    profiles = _load_profiles(ls)  # reads ~/.tag/config.yaml
    return [
        CodeAction(
            title=f"Ask TAG: {p}",
            kind=CodeActionKind.RefactorRewrite,
            command=Command(
                title=f"Ask TAG: {p}",
                command=f"tag.run.{p}",
                arguments=[str(params.text_document.uri), params.range],
            ),
        )
        for p in profiles
    ]

@server.command(f"tag.run.*")  # registered dynamically per profile at startup
async def execute_tag(ls: LanguageServer, args: list) -> None:
    """Execute the TAG profile on the selected text."""
    uri, range_, profile = args[0], args[1], args[2]
    context = _build_context(uri, range_, ls)
    token = await ls.progress.create_async()
    await ls.progress.begin(token, WorkDoneProgressBegin(title=f"TAG: {profile}"))
    try:
        result = await _run_tag_async(profile, context)
        ls.send_notification("tag/executionResult", {"profile": profile, "result": result})
    finally:
        await ls.progress.end(token, WorkDoneProgressEnd())
```

**Context construction (`_build_context`):**

```python
def _build_context(uri: str, range_: Range, ls: LanguageServer) -> str:
    doc = ls.workspace.get_text_document(uri)
    selected = _extract_range(doc.source, range_)
    lines = doc.source.splitlines()
    ctx_start = max(0, range_.start.line - CONTEXT_LINES)
    ctx_end = min(len(lines), range_.end.line + CONTEXT_LINES + 1)
    surrounding = "\n".join(lines[ctx_start:ctx_end])
    lang = _detect_language(uri)
    return (
        f"File: {uri}\n"
        f"Language: {lang}\n"
        f"Selected range: lines {range_.start.line + 1}–{range_.end.line + 1}\n\n"
        f"Context:\n```{lang}\n{surrounding}\n```\n\n"
        f"Selected code:\n```{lang}\n{selected}\n```\n\n"
        f"Goal: {selected}"
    )
```

**Agent invocation (`_run_tag_async`):**

```python
async def _run_tag_async(profile: str, context: str) -> str:
    proc = await asyncio.create_subprocess_exec(
        tag_cli_bin(), "shell", "--profile", profile, "--goal", context,
        "--no-tui",  # suppress Rich TUI; output plain text
        stdout=asyncio.subprocess.PIPE,
        stderr=asyncio.subprocess.PIPE,
    )
    stdout, stderr = await proc.communicate()
    if proc.returncode != 0:
        raise RuntimeError(stderr.decode())
    return stdout.decode()
```

### 9.4 VS Code extension architecture

The extension follows the standard `vscode-languageclient` pattern used by all major VS Code language extensions:

```
extension.js (activate)
  └── client.js (LanguageClient)
        ├── ServerOptions: { command: "tag", args: ["lsp", "--stdio"] }
        └── ClientOptions: { documentSelector: [{ scheme: "file" }] }
              └── onNotification("tag/executionResult", handler)
                    └── panel.js (WebviewPanel.reveal / update)
```

**`vscode/package.json` key sections:**

```json
{
  "name": "tag-vscode",
  "displayName": "TAG — AI Agent Bridge",
  "publisher": "nous-research",
  "engines": { "vscode": "^1.85.0" },
  "activationEvents": ["onStartupFinished"],
  "contributes": {
    "commands": [
      { "command": "tag.runOnSelection", "title": "Run on Selection", "category": "TAG" },
      { "command": "tag.switchProfile",  "title": "Switch Profile",   "category": "TAG" },
      { "command": "tag.showOutputPanel","title": "Show Output Panel","category": "TAG" },
      { "command": "tag.previewContext", "title": "Preview Context",  "category": "TAG" },
      { "command": "tag.setupWorkspace", "title": "Setup Workspace",  "category": "TAG" },
      { "command": "tag.restartServer",  "title": "Restart Language Server", "category": "TAG" },
      { "command": "tag.openSettings",   "title": "Open Settings",   "category": "TAG" }
    ],
    "menus": {
      "editor/context": [
        {
          "when": "editorHasSelection",
          "command": "tag.runOnSelection",
          "group": "1_modification"
        }
      ]
    },
    "configuration": {
      "title": "TAG",
      "properties": {
        "tag.defaultProfile":    { "type": "string",  "default": "",    "description": "Active TAG profile name" },
        "tag.contextLines":      { "type": "integer", "default": 30,    "description": "Lines of context around selection" },
        "tag.maxSelectionChars": { "type": "integer", "default": 32000, "description": "Max selection size before warning" },
        "tag.lsp.transport":     { "type": "string",  "enum": ["stdio","tcp"], "default": "stdio" },
        "tag.lsp.port":          { "type": "integer", "default": 7878,  "description": "TCP port (transport=tcp only)" }
      }
    }
  },
  "dependencies": {
    "vscode-languageclient": "^9.0.1"
  },
  "devDependencies": {
    "@vscode/test-electron": "^2.4.0",
    "esbuild": "^0.23.0"
  }
}
```

### 9.5 Webview panel (`vscode/panel.js`)

The webview uses a self-contained HTML template with all assets inlined or loaded via `vscode.Uri.joinPath` (CSP-safe):

- **Markdown rendering:** `marked` v13 (bundled via esbuild, ~50 KB gzipped)
- **Syntax highlighting:** `highlight.js` core + common languages (bundled, ~120 KB gzipped)
- **Content Security Policy:** `default-src 'none'; style-src 'nonce-{nonce}'; script-src 'nonce-{nonce}'` — no external resources, nonce-based inline scripts
- **Streaming updates:** The panel listens for `tag/executionResult` notifications. As progress chunks arrive via `WorkDoneProgressReport`, the webview appends them to a `<pre>` buffer; on completion, it re-renders the full response as Markdown.

### 9.6 `tag lsp` CLI integration in `controller.py`

```python
def cmd_lsp(args: argparse.Namespace) -> int:
    sub = getattr(args, "lsp_subcommand", None)
    if sub == "status":
        return _lsp_status()
    # Start server
    from tag.lsp_server import start_server
    transport = "stdio" if args.stdio else "tcp"
    port = args.port if not args.stdio else None
    return start_server(transport=transport, port=port, log_level=args.log_level)

def _lsp_status() -> int:
    pid_file = tag_home() / "lsp.pid"
    if not pid_file.exists():
        print_error("TAG LSP server is not running.")
        return 1
    pid = int(pid_file.read_text(encoding="utf-8").strip())
    import psutil
    try:
        proc = psutil.Process(pid)
        print_success(f"TAG LSP server running: PID {pid}, started {proc.create_time():.0f}")
        return 0
    except psutil.NoSuchProcess:
        pid_file.unlink(missing_ok=True)
        print_error("TAG LSP server PID file exists but process is not running (stale PID).")
        return 1
```

### 9.7 `pygls` dependency

`pygls` is the established Python LSP server framework. It is used by:
- `python-lsp-server` (pylsp) — the reference Python language server
- `ruff-lsp` — Astral's LSP wrapper for the `ruff` linter
- `jedi-language-server` — Jedi-based Python completion server

Adding `pygls` as an optional dependency (not in core) follows the existing pattern in `pyproject.toml` where heavy optional features are gated behind extras:

```toml
[project.optional-dependencies]
lsp = ["pygls==2.0.0", "lsprotocol==2024.0.1"]
```

Users who want the LSP server install: `pip install tag-agent[lsp]`

The VS Code extension's `tag.setupWorkspace` command installs this extra automatically if missing.

---

## 10. Security Considerations

### SC-01 — Localhost-only binding
When started in TCP mode, the LSP server MUST bind to `127.0.0.1` (or `::1` for IPv6) only. It MUST NOT bind to `0.0.0.0` or any external interface. This prevents other machines on the local network from submitting code to the agent without authentication.

### SC-02 — No external code upload
All agent execution runs via the local `tag` binary and the locally-configured Hermes instance. The LSP server MUST NOT transmit selected code to any external service itself. Any model API calls (OpenAI, Anthropic, etc.) are made by Hermes using the user's own API keys configured in their TAG profile `.env` file — the same path used when running `tag` from the terminal.

### SC-03 — Selection context size limits
The 32,000-character selection cap (FR-06) prevents accidental inclusion of entire files, which might contain `.env` contents, private keys, or database credentials that the developer doesn't intend to share with the model. The limit MUST be enforced server-side (in `_build_context`) as well as surfaced in the VS Code extension UI.

### SC-04 — Context preview before send
The "TAG: Preview Context" command (FR-11 / US-06) allows developers to inspect the full structured prompt before it is sent to the agent. This is a human-in-the-loop control for sensitive codebases.

### SC-05 — Webview Content Security Policy
The TAG output webview MUST use a strict CSP (see Section 9.5) that disallows external script and style sources. All rendering assets are bundled. This prevents XSS via malicious agent output that includes JavaScript in Markdown code blocks.

### SC-06 — No persistent connection authentication
In `--stdio` mode (the default and recommended mode for VS Code), the LSP server is spawned as a child process by the VS Code extension and communicates only over its stdin/stdout pipe. There is no network socket to attack. In TCP mode, connections are accepted from localhost only (SC-01), which limits the attack surface to processes already running on the same machine.

### SC-07 — Extension marketplace review requirements
If the extension is submitted to the VS Code Marketplace, it MUST pass Microsoft's automated security scan. In particular:
- No `eval()` or `new Function()` in extension code.
- No dynamic `require()` of user-provided paths.
- The webview HTML content MUST use `vscode.Uri.joinPath` for all asset references (not string concatenation).
- The extension MUST declare `"publisher": "nous-research"` and be signed with a verified publisher account.

### SC-08 — PID file permissions
The `~/.tag/lsp.pid` file MUST be written with mode `0600` (owner read/write only) to prevent other local users from learning the LSP server's PID.

---

## 11. Testing Strategy

### 11.1 LSP protocol conformance tests (`tests/test_lsp_server.py`)

Use `pygls`'s built-in test client to exercise the server in-process:

```python
from pygls.protocol import JsonRPCProtocol
from tag.lsp_server import server

def test_initialize():
    """Server MUST complete initialization handshake."""
    result = client.send_request("initialize", {...})
    assert result.capabilities.code_action_provider is True

def test_code_action_empty_selection():
    """Empty selection MUST return no code actions."""
    result = client.send_request("textDocument/codeAction", {
        "range": {"start": {"line": 0, "character": 0},
                  "end":   {"line": 0, "character": 0}},
    })
    assert result == []

def test_code_action_nonempty_selection():
    """Non-empty selection MUST return one action per profile."""
    result = client.send_request("textDocument/codeAction", {
        "range": {"start": {"line": 0, "character": 0},
                  "end":   {"line": 5, "character": 20}},
    })
    titles = [a.title for a in result]
    assert any("Ask TAG:" in t for t in titles)

def test_selection_size_limit():
    """Selection over limit MUST NOT invoke agent."""
    large = "x" * 33000
    # Server should return a diagnostic-bearing action, not invoke tag
    result = client.send_request("textDocument/codeAction", ...)
    assert result[0].title == "TAG: Selection too large (limit: 32000 chars)"
```

### 11.2 VS Code extension unit tests (`vscode/test/extension.test.js`)

Use `@vscode/test-electron` to run tests in a real VS Code instance:

```javascript
suite('TAG Extension Test Suite', () => {
    test('Extension activates on startup', async () => {
        const ext = vscode.extensions.getExtension('nous-research.tag-vscode');
        assert.ok(ext);
        await ext.activate();
        assert.ok(ext.isActive);
    });

    test('Commands are registered', () => {
        const commands = await vscode.commands.getCommands(true);
        assert.ok(commands.includes('tag.runOnSelection'));
        assert.ok(commands.includes('tag.switchProfile'));
    });

    test('Language client starts when tag binary exists', async () => {
        // mock: process.env.PATH includes a fake 'tag' binary
        // verify: LanguageClient reaches 'running' state
    });
});
```

### 11.3 Integration tests

A pytest integration test (`tests/test_lsp_integration.py`, gated behind `@pytest.mark.integration`) starts the full LSP server via `tag lsp --port 9799`, opens a TCP connection, sends a real `initialize` + `textDocument/codeAction` exchange, and verifies the response without mocking the TAG binary.

### 11.4 End-to-end smoke test

A CI step in `.github/workflows/lsp.yml`:
1. `pip install tag-agent[lsp]`
2. `tag lsp --stdio &`
3. Send a minimal LSP `initialize` packet via a Python script
4. Assert the server returns `InitializeResult` within 2 seconds
5. Send `shutdown` + `exit`

### 11.5 Extension build verification

`npm run build` in `vscode/` MUST produce a `.vsix` under 5 MB. This is enforced as a CI check with `npx vsce ls --no-dependencies` (lists bundled files) and a `du -sh *.vsix` assertion.

---

## 12. Acceptance Criteria

| ID | Criterion | How to verify |
|----|-----------|---------------|
| AC-01 | `tag lsp --stdio` starts and responds to `initialize` within 1 second | Run LSP init script; assert response time < 1s |
| AC-02 | `tag lsp --stdio` exits cleanly within 5 seconds of `shutdown` + `exit` | Send shutdown sequence; assert process exits |
| AC-03 | Non-empty text selection in VS Code produces ≥1 "Ask TAG:" code action | Open any source file, select text, open code actions menu |
| AC-04 | Clicking "Ask TAG: reviewer" opens the TAG output panel within 2 seconds | Measure time from click to panel focus |
| AC-05 | Agent output appears in the TAG webview panel with Markdown rendering | Verify fenced code blocks render with syntax highlighting |
| AC-06 | "TAG: Switch Profile" command shows all profiles from `tag profile list` | Open command palette, invoke switch, verify list matches `tag profile list --json` |
| AC-07 | Switching profile writes `tag.defaultProfile` to `.vscode/settings.json` | Switch profile; open `.vscode/settings.json`; verify key is set |
| AC-08 | Selection of 33,001 characters does NOT invoke the agent | Create a large selection; trigger code action; verify warning message, no agent call |
| AC-09 | The VS Code extension `.vsix` is under 5 MB | Run `npm run build`; assert `du -sh *.vsix` < 5 MB |
| AC-10 | `tag lsp status` exits 0 when server is running, 1 when not | Start server; run status (expect 0); stop server; run status (expect 1) |
| AC-11 | LSP server remains responsive during an ongoing agent execution | Trigger a slow agent; immediately request code actions in a second file; verify response within 500ms |
| AC-12 | Server binds to 127.0.0.1 only in TCP mode | Run `lsof -nP -iTCP:7878`; assert listen address is 127.0.0.1, not 0.0.0.0 |
| AC-13 | "TAG: Preview Context" shows the exact prompt before agent invocation | Invoke preview; verify structured context matches what would be sent |
| AC-14 | Workspace setup notification shown when config missing | Delete `~/.tag/config.yaml`; open VS Code; verify notification appears exactly once |
| AC-15 | LSP server idle RSS < 150 MB | Start server; wait 10 seconds; `ps -o rss= -p <PID>`; assert < 153600 |

---

## 13. Dependencies

### Python (server-side)

| Package | Version | Role |
|---------|---------|------|
| `pygls` | `^2.0.0` | LSP server framework (asyncio-based, used by pylsp, ruff-lsp) |
| `lsprotocol` | `^2024.0.1` | LSP type definitions (co-maintained with pygls) |
| `psutil` | already in core | Process management for `lsp status` PID check |

These are added to a new `[lsp]` optional extra in `pyproject.toml`, NOT to the core `dependencies` list.

### Node.js (extension-side)

| Package | Version | Role |
|---------|---------|------|
| `vscode-languageclient` | `^9.0.1` | LSP client library for VS Code extensions |
| `@vscode/test-electron` | `^2.4.0` | VS Code extension test runner |
| `esbuild` | `^0.23.0` | Extension bundler (dev dependency) |
| `marked` | `^13.0.0` | Markdown rendering in webview (bundled) |
| `highlight.js` | `^11.10.0` | Syntax highlighting in webview (bundled) |

Node.js ≥ 18 required. VS Code engine ≥ 1.85.0 (December 2023).

### VS Code API surface used

- `vscode.languages.registerCodeActionsProvider` (fallback if LSP code actions need client-side filtering)
- `vscode.window.createWebviewPanel`
- `vscode.window.createTerminal`
- `vscode.workspace.getConfiguration`
- `vscode.commands.registerCommand`
- `vscode.LanguageClient` (from `vscode-languageclient`)

---

## 14. Open Questions

### OQ-01 — LSP vs. native VS Code extension API
`pygls` handles LSP protocol mechanics (framing, dispatch, lifecycle), but VS Code's built-in language client also supports server-side code action providers natively. The question is whether the LSP server needs to be a standalone process at all, or whether the extension could call the `tag` binary directly and skip LSP for the VS Code case.

**Current decision:** Use LSP for editor-agnostic reach (Neovim, Helix, Zed, Emacs are all future beneficiaries of a conformant LSP server). The VS Code extension uses `vscode-languageclient` as a thin wrapper. If the LSP approach proves too heavy, a VS Code–only "direct call" mode can be added as a fallback.

### OQ-02 — stdio vs TCP default
`--stdio` is simpler for editor-managed servers. `--port` is simpler for persistent background servers. Should the extension default to stdio (spawning a fresh server per VS Code instance) or TCP (connecting to a shared persistent server)?

**Current decision:** `--stdio` is the default for the extension. The `tag lsp --port` TCP mode is documented for power users who want a persistent server (e.g., shared across multiple VS Code workspaces or connected from Neovim simultaneously).

### OQ-03 — Extension marketplace publishing timeline
VS Code Marketplace submission requires a Microsoft publisher account, a verified publisher badge process (domain verification), and passing automated security scanning. How quickly can `nous-research` complete this?

**Placeholder decision:** First release ships as a `.vsix` file installable via `code --install-extension tag-agent-<version>.vsix`. Marketplace submission is a post-launch task.

### OQ-04 — `tag shell --goal` flag
The LSP server invokes `tag shell --profile P --goal "..."`. Does `tag shell` currently accept a `--goal` flag for non-interactive one-shot execution? If not, this flag needs to be added as part of this PRD's implementation.

**Dependency:** `src/tag/shell_mode.py` (PRD-019) must accept `--goal` for non-interactive mode. If PRD-019 is not complete, the LSP server will need its own invocation path (direct Python API call into `run_profile_hermes`).

### OQ-05 — Streaming partial output
The current design streams progress notifications to the webview via `WorkDoneProgressReport`. For long-running agents (30+ second reviews), should the webview render partial Markdown as it arrives, or wait for the full response?

**Current decision:** Wait for full response for v1 (simpler). A streaming token-by-token render path is a v2 enhancement. The progress bar provides user feedback during the wait.

### OQ-06 — Multiple concurrent agent executions
If the user triggers two code actions simultaneously, should the LSP server queue them, run them in parallel, or reject the second with a "busy" response?

**Current decision:** Run in parallel (separate asyncio tasks). Each execution has its own progress token and sends its result to the webview panel independently. The webview shows a tab per execution if multiple are in flight.

---

## 15. Complexity & Timeline

**Complexity:** XL — This is the most architecturally novel TAG feature to date. It introduces a new process (the LSP server), a new language (TypeScript/JavaScript for the extension), a new protocol (LSP 3.17), a new build system (npm/esbuild), and a new runtime environment (the VS Code Extension Host). None of these are prohibitively complex individually, but their combination spans multiple engineering domains.

### Sprint breakdown (2-week sprints)

**Sprint 1 — LSP server foundation**
- Scaffold `src/tag/lsp_server.py` with `pygls`
- Implement `initialize`, `shutdown`, `exit` lifecycle
- Implement `textDocument/codeAction` returning static profile list
- Implement `workspace/executeCommand` calling `tag shell --goal` subprocess
- Add `tag lsp [--port] [--stdio]` and `tag lsp status` CLI commands
- Write LSP conformance tests (`tests/test_lsp_server.py`)
- Add `pygls` to `pyproject.toml[lsp]` extra

**Sprint 2 — VS Code extension**
- Scaffold `vscode/` directory with `package.json`, `extension.js`, `client.js`
- Integrate `vscode-languageclient` with stdio transport
- Implement `WebviewPanel` with Markdown + syntax highlighting (`panel.js`)
- Implement `tag.switchProfile` command palette command
- Implement workspace setup detection and notification
- Write VS Code extension unit tests (`@vscode/test-electron`)
- Configure esbuild for bundling; verify `.vsix` < 5 MB

**Sprint 3 — Context injection, security, polish**
- Implement `_build_context` with ±30 surrounding lines
- Implement selection size limit (FR-06)
- Implement "TAG: Preview Context" command
- Implement `.vscode/settings.json` read/write for `tag.defaultProfile`
- Enforce CSP on webview (SC-05)
- Ensure localhost-only TCP binding (SC-01)
- Write integration tests (`tests/test_lsp_integration.py`)
- CI workflow (`.github/workflows/lsp.yml`)

**Sprint 4 — Hardening, documentation, acceptance testing**
- Run full acceptance criteria checklist (AC-01 through AC-15)
- Performance profiling: startup time, idle RSS, responsiveness under load
- Cross-platform testing: macOS, Linux (Ubuntu), Windows (WSL2)
- Write `vscode/README.md` (user documentation: install, configure, usage)
- Update `docs/prd/INDEX.md` to include PRD-022
- `.vsix` build and local install testing
- Final security review (SC-01 through SC-08)

---

## 16. Risks

| Risk | Likelihood | Impact | Mitigation |
|------|-----------|--------|------------|
| `tag shell --goal` non-interactive mode not yet implemented | High | High | Define `--goal` flag in `shell_mode.py` as sprint-1 prerequisite; fallback: direct `run_profile_hermes` Python call |
| `pygls` async model conflicts with TAG's subprocess calls | Medium | Medium | Use `asyncio.create_subprocess_exec` throughout; run blocking calls in `loop.run_in_executor` |
| VS Code extension activation too eager (slow startup) | Low | Medium | Use `onStartupFinished` activation event, not `*` |
| Webview XSS via agent output containing `<script>` in code blocks | Medium | High | CSP + `marked`'s default HTML sanitization; test with adversarial output |
| `.vsix` size blows past 5 MB due to bundled assets | Low | Low | Track size in CI; lazy-load `highlight.js` language packs that aren't needed |
| LSP server breaks when TAG config is missing | Medium | Medium | Gracefully return empty code action list when config parse fails; surface actionable error via `window/showMessage` |

---

*PRD-022 — IDE Bridge (LSP Server + VS Code Extension)*  
*Author: TAG Product Team*  
*Created: 2026-06-12*  
*Target release: TAG v0.8.0*
