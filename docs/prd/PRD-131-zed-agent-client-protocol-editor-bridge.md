# PRD-131: Zed **Agent *Client* Protocol** — Editor↔Agent Bridge (`tag editor serve`)

> **Stack: Go** (native single-binary; see docs/GO_MIGRATION_RESEARCH.md). This PRD is Go-native from the start — there is no Python precursor to re-frame.

> 🚨 **ACRONYM COLLISION — READ THIS FIRST.** Two unrelated protocols are both abbreviated **ACP**, and this repository already has a PRD for the other one.
>
> | | **PRD-087** — "ACP" | **PRD-131** — "ACP" (this document) |
> |---|---|---|
> | Full name | Agent **Communication** Protocol | Agent **Client** Protocol |
> | Owner | IBM / BeeAI, under Linux Foundation AGNTCY | Zed Industries, co-developed with JetBrains |
> | Topology | Agent ↔ agent, intra-cluster | **Editor ↔ agent**, local |
> | Transport | HTTP/REST — `POST /runs`, `GET /agents` | **JSON-RPC 2.0 over stdio** |
> | Analogy | A lightweight service mesh for agents | **"LSP, but for coding agents"** |
> | Who is the client | Another agent or an orchestrator | A human's editor: Zed, JetBrains, VS Code, Emacs, Neovim, marimo |
> | Command surface | `tag acp` (claimed by PRD-087) | **`tag editor`** (this PRD — deliberately avoids `acp`) |
>
> They are **different protocols that happen to share an acronym**. This PRD implements Zed's. It does not supersede, replace, extend or conflict with PRD-087, and the two can ship independently in either order. Anyone tempted to merge them should read this table again: one speaks HTTP to a cluster, the other speaks JSON-RPC over a pipe to a text editor.

**Status:** Proposed
**Priority:** P2
**Estimated Effort:** M (2-3 weeks)
**Category:** Editor Interoperability & Multi-Surface Reach
**Affects:** `internal/editoracp` (new package — deliberately *not* named `acp`, to keep the two protocols textually distinct in the import graph), `internal/mcp` (its JSON-RPC framing and read loop are the reusable prior art), `internal/agent` (streaming callbacks the loop does not currently expose), `internal/tool` (editor-delegated `fs/*` and `terminal/*` implementations), `internal/permission` (an ACP-backed `Prompter` — the composition that makes this valuable), `internal/cli` (new `editor` command group), `internal/store` (`editor_sessions` table), `internal/llm` (unchanged; the protocol is transport, not inference)
**Depends on:** PRD-021 (agent loop / autonomous mode — the loop being exposed), PRD-013 (distributed agent tracing & observability — editor-driven runs must appear in `tag trace` like any other), PRD-018 (context window & long-context management — `session/load` replays a stored session and must respect the window), PRD-035 (IDE bridge — LSP server & VS Code extension; the existing `internal/lsp` establishes the editor-facing surface and its stdio conventions — note the file `PRD-035-ide-bridge-lsp.md` carries a stale `# PRD-022:` heading), PRD-039 (token budget enforcement — an editor session must be cappable), PRD-078 (HITL tool approval — ACP's `session/request_permission` is the wire form of exactly this)
**Cross-reference, NOT a dependency:** PRD-087 (IBM ACP REST adapter). Different protocol; see the box above.
**Composes with:** the shipped tool-permission model in `internal/permission` (no PRD)
**Inspired by:** Zed Agent Client Protocol v1 + the ACP Registry (~45 agents, 13 client editors), the Language Server Protocol, OpenCode `acp`, Goose, Gemini CLI, OpenHands ACP delegation

---

## 1. Overview

The Agent Client Protocol is Zed Industries' standard for the connection between a code editor and a coding agent. It is JSON-RPC 2.0 over stdio: the editor spawns the agent as a subprocess and they exchange typed messages over the pipe. The editor owns the UI, the buffers, the file system and the human; the agent owns the reasoning and the tool loop. The framing is deliberate and explicit — it is LSP's architecture applied to agents, and it is being adopted the way LSP was, because the alternative is N×M integrations.

The July 2026 competitive audit calls this out as the standardization event of the year: ACP hit v1 with JetBrains co-authorship and gained a registry in January 2026 listing **~45 agents and 13 client editors** (Zed, JetBrains IDEs, VS Code, Emacs, Neovim, marimo and others). The audit's §2 says it plainly — *"ACP is the closest thing to an MCP-scale standardization moment in 2026, and it is happening on the client side"* — and notes that TAG participates in neither ACP nor A2A.

The strategic case for TAG specifically is unusually clean, and it turns on a decision the project already made. `tag-go/MIGRATION_STATUS.md` records multi-surface (desktop, IDE extension, web) as a **deliberate non-port**. The audit agrees that this is defensible for an automation control plane — and then identifies the cost: single-surface tools are now the minority, and it recommends ACP as "the cheap hedge — it buys editor reach without building or maintaining a surface." That is the entire argument. Implementing a protocol endpoint is a few thousand lines of Go in a package with no UI, no bundler, no extension marketplace submission, no VS Code API churn to track, and no second distribution channel. Implementing an IDE extension is a product. ACP gets TAG into thirteen editors for the cost of the former, and it does so **without weakening the headless-first stance** — an ACP server is a headless process that speaks a pipe, which is exactly what TAG already is.

The second reason this fits TAG particularly well is that ACP's hardest requirement is one TAG has already built. ACP defines `session/request_permission`: when the agent wants to do something consequential, it asks the client, the client renders a UI, the human answers, and the answer comes back over the wire. TAG's `internal/permission` package defines exactly this shape as a Go interface — `Prompter.Ask(ctx, Request) (Response, error)` — with three responses (`ResponseDeny`, `ResponseAllowOnce`, `ResponseAllowSession`) that map onto ACP's permission options essentially one-to-one, and with a `Guard` whose contract is already "a nil Prompter means non-interactive, and `ask` becomes deny." Adding an ACP `Prompter` is one small type. Every existing rule, every credential-path deny, every audit row in `permission_decisions`, and the entire `tag permissions show|log` surface work unchanged for editor-driven sessions. A harness without a permission model would have to invent one to implement ACP properly; TAG gets to implement ACP *because* it has one.

The third piece is `internal/mcp`. TAG already ships a JSON-RPC-over-stdio client *and* server for MCP — `frame.go`, `client.go`'s `readLoop`/`call`, `server.go`'s `handleLine`/`dispatch`. ACP is a different schema over the same transport shape. The framing, the pending-request map, the concurrent-safe write path and the error-envelope handling are all prior art in-repo, tested, and CGO-free.

---

## 2. Problem Statement

### 2.1 TAG is unreachable from every editor a developer actually uses

TAG's user-facing surfaces are the CLI, a TUI, and three partial local dashboards. A developer working in Zed, IntelliJ, VS Code, Emacs or Neovim cannot invoke TAG from where they are. They can shell out to `tag run`, which loses streaming, loses in-editor diffs, loses permission prompts rendered in the UI, and loses any notion of the editor's open buffers and unsaved state. Meanwhile the audit's parity matrix scores TAG-Go as `—` on "Desktop / IDE / web surfaces" against Claude Code's 8 and Cline's 4, and marks the row "**table stakes**".

### 2.2 The obvious fix is the expensive one, and it was already rejected for good reasons

Building an IDE extension means a TypeScript codebase, a bundler, a marketplace listing, a review process, a release cadence coupled to someone else's API, and — because there is more than one editor — repeating that N times. It contradicts the single-static-binary tenet, contradicts the documented non-port decision, and would consume more engineering than every feature in this PRD batch combined. The audit lists "Building an IDE extension or desktop app" under **explicitly not recommended**. The problem is real; the obvious solution is not the right one.

### 2.3 Without a protocol, TAG's differentiated capabilities stay invisible where developers work

This is the part that makes ACP more than a checkbox. TAG's genuinely category-leading assets — five interlocking memory subsystems, temporal facts with validity windows, a knowledge graph, the DAG/cron/webhook automation loop — are reachable only from a terminal. An ACP server changes what "TAG in your editor" means: not another chat sidebar, but a sidebar whose agent has durable cross-session memory of this repository and can enqueue a scheduled follow-up. The protocol is a distribution mechanism for the differentiators, not just for the loop.

### 2.4 A protocol without a permission model is a liability, and most implementations have one problem or the other

An editor-spawned agent runs with the developer's full privileges in their real repository. ACP's answer is `session/request_permission`. An agent that implements ACP's transport but not its consent semantics is strictly more dangerous than a CLI, because the human is further from the action. Conversely, an agent with a good permission model but no protocol is stuck in the terminal. TAG is currently in the second position, which is the recoverable one — but only if the ACP implementation routes through the existing gate rather than around it. Every tool call originating from an editor session must land in `Guard.Check` and be recorded in `permission_decisions`, or this PRD has quietly created a bypass of the project's best security asset.

### 2.5 The acronym collision is itself a documented project risk

This repository has a recorded history of cross-references drifting to the wrong document after renumbering, and PRD-087 already occupies the string "ACP" and the command `tag acp`. Two protocols sharing an acronym, in one PRD corpus, is a mis-merge waiting to happen. Hence the banner box, the distinct command surface (`tag editor`, not `tag acp`), the distinct package name (`internal/editoracp`, not `internal/acp` — which PRD-087 claims), and this subsection. The disambiguation is a deliverable of the PRD, not decoration on it.

---

## 3. Goals and Non-Goals

### Goals

| # | Goal |
|---|------|
| G1 | `tag editor serve` speaks Zed ACP v1 over stdio: `initialize`, `authenticate`, `session/new`, `session/load`, `session/prompt`, `session/cancel`, plus `session/update` notifications. |
| G2 | Streaming: agent text, reasoning, tool-call starts and tool-call results are emitted as `session/update` notifications as they happen, not batched at the end. |
| G3 | `session/request_permission` is backed by an `internal/permission.Prompter` implementation, so **every** editor-originated tool call passes through the same `Guard`, the same ruleset, and the same `permission_decisions` audit trail as a CLI run. |
| G4 | Editor-delegated filesystem access (`fs/read_text_file`, `fs/write_text_file`) is used when the client advertises the capability, so the agent sees **unsaved buffer state** rather than stale on-disk content — the concrete thing a CLI cannot do. |
| G5 | `terminal/*` methods are supported when the client advertises them, so shell output renders in the editor's own terminal UI. |
| G6 | Capability negotiation is honest: TAG advertises only what it implements, degrades to its own `internal/tool` implementations when the client lacks a capability, and never claims a capability it will fail on. |
| G7 | ACP sessions are first-class TAG runs — recorded in `runs`, traced via PRD-013, budgeted via PRD-039, visible in `tag runs`, `tag trace` and `tag costs`. |
| G8 | `session/load` replays a stored session so a developer can resume yesterday's conversation from the editor. |
| G9 | `tag editor doctor` diagnoses the integration (protocol version, negotiated capabilities, permission posture, provider reachability) and prints ready-to-paste editor configuration. |
| G10 | Cancellation is real: `session/cancel` propagates through `context.Context` into the in-flight provider request and running tools, and returns promptly. |
| G11 | The server runs offline against `echo`, so an editor integration can be developed and tested with no keys and no network. |
| G12 | The naming distinction from PRD-087 is unmissable in the title, the banner box, the command surface, the package name and the docs. |

### Non-Goals

| # | Non-Goal |
|---|----------|
| NG1 | Implementing IBM ACP (`POST /runs`, `GET /agents`, BeeAI/AGNTCY). That is PRD-087, a different protocol, and this PRD neither implements nor blocks it. |
| NG2 | Writing an editor extension for any editor. The entire point is that the *client* side already exists in 13 editors. TAG ships a server and configuration snippets, nothing more. |
| NG3 | Acting as an ACP *client* (driving other agents from TAG). Interesting — OpenHands does it — but it is a separate PRD with a different threat model. |
| NG4 | Publishing to the ACP Registry as a launch requirement. Worth doing; not engineering work; not a gate on shipping. |
| NG5 | Reversing the multi-surface non-port. This PRD is explicitly the *substitute* for that decision, not a step toward reopening it. No desktop app, no web IDE, no browser client. |
| NG6 | ACP extension methods beyond v1 core. Vendor extensions are tracked, not implemented, until they stabilize. |
| NG7 | Multi-tenant or network-exposed operation. ACP is stdio between a local editor and a local subprocess. There is no listening socket, and adding one would be a different, much larger security problem. |
| NG8 | Porting to the Python edition. Go-native. |

---

## 4. Success Metrics

| Metric | Target | Measurement |
|--------|--------|-------------|
| Protocol conformance | 100% of the v1 core method set implemented or explicitly declined via capability negotiation — zero methods that are advertised and then fail | Conformance suite against captured Zed/JetBrains transcripts in `testdata/` |
| Permission routing | 100% of editor-originated tool calls produce a `permission_decisions` row | Integration test counting rows per session |
| No bypass | Zero code paths reach a tool `Exec` without passing `Guard.Check` | Architecture test asserting every registered tool is `permission.Wrap`-ed |
| Streaming latency | First `session/update` within 200 ms of `session/prompt` for a streaming provider | Timed integration test with a stub SSE provider |
| Cancellation | `session/cancel` returns within 500 ms and the provider request is actually aborted | Integration test with a slow stub provider |
| Buffer freshness | With `fs/read_text_file` negotiated, the agent reads unsaved editor content in 100% of cases | Integration test with a mock client holding divergent content |
| Startup | `tag editor serve` reaches `initialize` response in < 100 ms, consistent with the 56-68 ms binary baseline | Timed test |
| Offline honesty | A full session completes against `echo` with egress blackholed | Offline CI job |
| Disambiguation | Zero references in the repo where PRD-131 and PRD-087 are conflated | Doc lint grepping for `acp` near both numbers |

---

## 5. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|-----------|----------|
| U1 | Zed user | add TAG as an ACP agent in `settings.json` | I use TAG from my editor without leaving it |
| U2 | JetBrains user | configure TAG as an external agent | one TAG install serves my IDE and my terminal identically |
| U3 | Neovim user | drive TAG through an ACP client plugin | I get an agent in a 19 MB binary with no Node runtime |
| U4 | Developer | see TAG's proposed edit as a diff in my editor's UI | I review changes where I already review changes |
| U5 | Developer | have the agent read my **unsaved** buffer | it reasons about what I am looking at, not what I last saved |
| U6 | Security-conscious developer | approve tool calls in an editor dialog backed by my TAG permission rules | the same policy governs both surfaces, with one audit trail |
| U7 | Security-conscious developer | run `tag permissions log` after an editor session | editor-driven actions are auditable exactly like CLI ones |
| U8 | Developer | resume yesterday's session via `session/load` | context survives closing the editor |
| U9 | Developer | benefit from TAG's cross-session memory inside the editor | the agent remembers this repository across days |
| U10 | Developer | cancel a runaway agent from the editor and have it stop | cancellation means cancellation |
| U11 | Platform engineer | run `tag editor doctor` and paste the emitted config | setup takes a minute, not an afternoon |
| U12 | Cost-conscious operator | see editor sessions in `tag costs` alongside CLI runs | one cost picture, not two |

---

## 6. Proposed CLI Surface

**Collision check.** `tag --help` on the built binary lists no `editor` command; the `e` neighbourhood is `env`, `eval`, `eval-ci`, `eval-dataset`, `eval-judge`. No existing PRD claims `tag editor`. **`tag acp` is deliberately not used** — PRD-087 claims it, and reusing it would create precisely the conflation this PRD exists to prevent. The package is `internal/editoracp` for the same reason: PRD-087 specifies `internal/acp`, and two packages differing only by import path would be a latent mis-edit.

### 6.1 `tag editor serve`

```bash
tag editor serve [--profile NAME] [--provider NAME] [--tools] [--max-steps N] \
                 [--allow-tool SPEC] [--deny-tool SPEC] [--ask-tool SPEC] \
                 [--log-file PATH] [--protocol-version N]
```

Speaks ACP on stdin/stdout. Not intended for direct human invocation — the editor spawns it. Because stdout carries the protocol, **all** diagnostics go to stderr or `--log-file`; a stray `fmt.Println` corrupts the stream, and a test asserts stdout contains only framed JSON-RPC.

Permission flags are accepted and resolve exactly as they do for `tag run` — `permFlags.policy()` is reused verbatim. The Prompter is the ACP one rather than the TTY one, which is the only difference.

### 6.2 `tag editor doctor`

```bash
tag editor doctor [--json]
```

```
TAG editor bridge (Zed Agent Client Protocol)
  NOTE: this is Zed's Agent CLIENT Protocol (editor↔agent, JSON-RPC/stdio).
        It is NOT IBM's Agent COMMUNICATION Protocol (`tag acp`, PRD-087).

  binary              /usr/local/bin/tag (0.9.0-go)
  protocol            ACP v1
  implemented         initialize, authenticate, session/new, session/load,
                      session/prompt, session/cancel, session/update
  client capabilities used when offered
                      fs/read_text_file, fs/write_text_file, terminal/*
  provider            anthropic (ANTHROPIC_API_KEY present)
  profile             coder → claude-sonnet-4-6
  permissions         12 rules; bash=ask, write_file=ask, credential paths denied
                      → `ask` will surface as an editor dialog via session/request_permission
  store               ~/.tag/runtime/tag.sqlite3 (writable)

Zed — add to settings.json:
  "agent_servers": { "TAG": { "command": "tag", "args": ["editor", "serve"] } }
```

### 6.3 `tag editor sessions`

```bash
tag editor sessions [list|show ID|rm ID] [--json]
```

Inspect ACP sessions from the terminal — the surface that makes an editor session debuggable without the editor.

### 6.4 Configuration

```yaml
editor:
  enabled: true
  profile: coder
  provider: anthropic
  tools: true
  max_steps: 24
  prefer_client_fs: true        # use fs/* when offered (unsaved-buffer awareness)
  prefer_client_terminal: true
  log_file: ~/.tag/logs/editor-acp.log
  session_retention_days: 30
```

---

## 7. Functional Requirements

| ID | Requirement | Acceptance Test |
|----|------------|-----------------|
| FR-01 | JSON-RPC 2.0 framing over stdio reuses the `internal/mcp` framing approach (`frame.go`), including partial-read handling and a bounded maximum message size. | Unit test with split/coalesced reads and an oversized message. |
| FR-02 | `initialize` negotiates protocol version and exchanges capabilities. A client protocol version TAG does not support produces a clean error response, never a panic or a silent mismatch. | Table-driven unit test over supported/unsupported versions. |
| FR-03 | TAG advertises only capabilities it implements. A capability it will fail on is never advertised. | Conformance test cross-checking the advertised set against the dispatch table. |
| FR-04 | `session/new` creates a session with a cwd and optional MCP server list, persists it to `editor_sessions`, and returns a session id. | Integration test. |
| FR-05 | `session/prompt` runs `agent.Loop` and streams `session/update` notifications for text, reasoning, tool-call start and tool-call result. | Integration test asserting notification ordering against a stub provider. |
| FR-06 | `session/cancel` cancels the session's `context.Context`, aborting the in-flight provider request and running tools, and responds within 500 ms. | Integration test with a slow stub provider. |
| FR-07 | `session/request_permission` is issued by an `acpPrompter` implementing `permission.Prompter`. ACP's permission options map to `ResponseDeny` / `ResponseAllowOnce` / `ResponseAllowSession`. | Unit test over the full option matrix. |
| FR-08 | The `Guard` for an ACP session is built by the **same** `permFlags.policy()` path as `tag run`, so config and flag rules resolve identically across surfaces. | Unit test comparing resolved rulesets between a CLI and an ACP invocation with identical inputs. |
| FR-09 | Every editor-originated tool call is routed through `permission.Wrap`. There is no ACP-specific tool registration path that bypasses the gate. | Architecture test asserting the ACP registry is built by `tool.Register` and by nothing else. |
| FR-10 | Decisions are recorded in `permission_decisions` with the session's run id, so `tag permissions log` covers editor sessions. | Integration test. |
| FR-11 | A cancelled or disconnected client resolves an outstanding `session/request_permission` as **deny**, never as allow, and never blocks. This mirrors `Guard`'s existing "no human ⇒ deny" contract. | Unit test closing the pipe mid-prompt. |
| FR-12 | When the client offers `fs/read_text_file`, `read_file` is served by the client (unsaved buffers). Otherwise TAG's own root-confined `read_file` is used. | Integration test with a mock client whose buffer content differs from disk. |
| FR-13 | Client-delegated `fs/*` paths are still subject to the permission gate — a `KindPath` request is constructed and checked before the client call is made, so credential-path denies apply to editor-delegated reads exactly as to local ones. | Integration test: `fs/read_text_file` on `~/.ssh/id_rsa` is denied before any client round-trip. |
| FR-14 | When the client offers `terminal/*`, `bash` is executed through the client; otherwise TAG's own bash tool runs it. Either way it is gated as `KindCommand`. | Integration test in both modes. |
| FR-15 | Client-delegated paths are normalized to absolute, lexically cleaned paths before rule matching, matching `Request.Subject`'s documented contract, so a `../..` from the client cannot dodge a rule. | Fuzz test over traversal shapes. |
| FR-16 | `session/load` restores a stored session's history within the context budget (PRD-018), truncating oldest-first with an explicit notification rather than silently. | Integration test with an over-budget session. |
| FR-17 | Each `session/prompt` is recorded as a `runs` row with kind `editor` and emits PRD-013 spans; token/cost accounting flows to `tag costs`. | Integration test querying `runs` and `spans`. |
| FR-18 | Budget caps (PRD-039) apply per session; exceeding one ends the turn with an explicit `session/update` and an error response, not a silent stop. | Unit test with a mock cost accumulator. |
| FR-19 | stdout carries **only** framed JSON-RPC. All logging goes to stderr or `--log-file`. | Test asserting stdout parses fully as framed JSON-RPC across a full session, including error paths. |
| FR-20 | Malformed JSON, unknown methods and bad params produce spec-conformant JSON-RPC error objects; the server never exits on a bad message. | Fuzz test over malformed input. |
| FR-21 | `tag editor doctor` reports protocol version, implemented methods, negotiated capabilities, permission posture and provider reachability, and prints editor config snippets. | Golden-file test. |
| FR-22 | `tag editor doctor` output and the package docs state the PRD-087 distinction explicitly. | String-match test. |
| FR-23 | The server runs fully against `--provider echo` with no keys and no network. | Offline CI job. |
| FR-24 | Session records are retained per `session_retention_days` and reaped by an existing maintenance path, not by a new daemon. | Integration test with a backdated session. |

---

## 8. Non-Functional Requirements

| ID | Requirement | Target |
|----|------------|--------|
| NFR-01 | `initialize` responds in < 100 ms cold. | Timed test |
| NFR-02 | First `session/update` within 200 ms of `session/prompt` for a streaming provider. | Timed test |
| NFR-03 | No new direct Go modules. JSON-RPC is `encoding/json` plus the framing already proven in `internal/mcp`. `CGO_ENABLED=0` unchanged. | `go mod graph` diff empty |
| NFR-04 | Concurrent sessions are safe: writes to stdout are serialized through a single mutex-guarded writer, as `internal/mcp`'s server already does. | Race-detector test with concurrent sessions |
| NFR-05 | Memory is bounded per session; message size is capped and streamed content is not accumulated beyond the context budget. | Memory profile over a long session |
| NFR-06 | `internal/editoracp` imports `internal/agent`, `internal/tool`, `internal/permission`, `internal/store` — never `internal/cli`. | Import-graph architecture test |
| NFR-07 | The package name and every doc comment make the Zed-vs-IBM distinction explicit, so a future maintainer cannot conflate them from the code alone. | Doc lint |
| NFR-08 | `go vet`, `golangci-lint`, `staticcheck` clean; `gofmt` clean. | CI gate |
| NFR-09 | Session writes honour the single-writer + WAL contract. | Concurrent integration test |
| NFR-10 | No listening socket is opened under any flag combination. | Test asserting zero `net.Listen` calls in the package |

---

## 9. Technical Design

### 9.1 New and Modified Files

| File | Change | Description |
|---|---|---|
| `internal/editoracp/protocol.go` | **New** | v1 wire types: `InitializeRequest/Response`, `ClientCapabilities`, `AgentCapabilities`, `SessionUpdate`, `ContentBlock`, `PermissionRequest/Response`, `ToolCallUpdate`. |
| `internal/editoracp/server.go` | **New** | Read loop, dispatch table, mutex-guarded writer, error envelopes. Structurally parallel to `internal/mcp/server.go`. |
| `internal/editoracp/session.go` | **New** | `Session` lifecycle, per-session `context.Context`, history, `session/load`. |
| `internal/editoracp/prompter.go` | **New** | `acpPrompter` implementing `permission.Prompter` over `session/request_permission`. |
| `internal/editoracp/clientfs.go` | **New** | `fs/read_text_file`, `fs/write_text_file`, `terminal/*` client-delegated tools, gated identically to the local ones. |
| `internal/editoracp/stream.go` | **New** | `agent.Loop` events → `session/update` notifications. |
| `internal/agent/loop.go` | **Extend** | `Options.OnEvent func(Event)` streaming callback. Today `Run` returns only a final `*Result`; ACP needs incremental delivery, and a callback is the smallest change that provides it without restructuring the loop. |
| `internal/cli/editor.go` | **New** | `tag editor` group. |
| `internal/store/schema` | **Extend** | `editor_sessions` table. |

### 9.2 SQLite DDL

```sql
CREATE TABLE IF NOT EXISTS editor_sessions (
  id            TEXT PRIMARY KEY,          -- ACP session id
  client_name   TEXT NOT NULL,             -- "Zed", "IntelliJ IDEA", …
  client_version TEXT,
  protocol_ver  INTEGER NOT NULL,
  cwd           TEXT NOT NULL,
  profile       TEXT NOT NULL,
  provider      TEXT NOT NULL,
  capabilities_json TEXT NOT NULL DEFAULT '{}',  -- negotiated client capabilities
  history_json  TEXT NOT NULL DEFAULT '[]',      -- llm.Message history for session/load
  turns         INTEGER NOT NULL DEFAULT 0,
  prompt_tokens INTEGER NOT NULL DEFAULT 0,
  completion_tokens INTEGER NOT NULL DEFAULT 0,
  cost_usd      REAL NOT NULL DEFAULT 0.0,
  status        TEXT NOT NULL DEFAULT 'active', -- active|ended
  created_at    TEXT NOT NULL,
  updated_at    TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_es_status ON editor_sessions(status, updated_at DESC);
```

### 9.3 The Permission Bridge — the load-bearing composition

```go
// internal/editoracp/prompter.go
//
// acpPrompter implements permission.Prompter over ACP's session/request_permission.
// This is the whole reason ACP is cheap for TAG: the Guard, the resolved ruleset,
// the credential-path denies and the permission_decisions audit trail are entirely
// unchanged. Only the transport for "ask a human" differs from the TTY prompter.
type acpPrompter struct {
	srv       *Server
	sessionID string
}

func (p *acpPrompter) Ask(ctx context.Context, req permission.Request) (permission.Response, error) {
	msg := PermissionRequest{
		SessionID: p.sessionID,
		ToolCall:  toolCallFrom(req),   // name, kind, subject, args summary
		Options: []PermissionOption{
			{ID: "allow-once",    Name: "Allow once",             Kind: "allow_once"},
			{ID: "allow-session", Name: "Allow for this session", Kind: "allow_always"},
			{ID: "deny",          Name: "Deny",                   Kind: "reject_once"},
		},
	}
	var out PermissionResponse
	if err := p.srv.request(ctx, p.sessionID, "session/request_permission", msg, &out); err != nil {
		// Client gone, cancelled, or timed out: DENY. This mirrors the Guard's
		// existing contract that an unanswerable `ask` is a deny, never an allow.
		return permission.ResponseDeny, nil
	}
	switch out.Outcome.OptionID {
	case "allow-once":
		return permission.ResponseAllowOnce, nil
	case "allow-session":
		return permission.ResponseAllowSession, nil
	default:
		return permission.ResponseDeny, nil
	}
}
```

The mapping is one-to-one because both designs answered the same question the same way. `Guard.grantSession` already implements per-tool session persistence for `ResponseAllowSession`, so ACP's `allow_always` needs no new state.

### 9.4 Client-Delegated Tools Are Gated Identically

The subtle risk in ACP is that `fs/read_text_file` looks like the *client's* operation and could plausibly be treated as outside TAG's policy. It must not be. The delegated tool constructs a `permission.Request` and checks it **before** the client round-trip:

```go
func clientReadFileTool(s *Server, sess *Session, g *permission.Guard) agent.Tool {
	t := agent.Tool{Def: readFileDef, Exec: func(ctx context.Context, in map[string]any) (string, error) {
		return s.clientReadTextFile(ctx, sess.ID, in["path"].(string))
	}}
	// Same wrapper, same subject extractor, same guard as the local tool.
	return permission.Wrap(g, t, pathSubjectAbs(sess.CWD, "path"))
}
```

`pathSubjectAbs` resolves to an absolute, lexically cleaned path against the session cwd, satisfying `permission.Request.Subject`'s documented contract. Consequently `CredentialRules()` — `*.env`, `~/.ssh/**`, `*.pem`, `credentials.json` and the rest — applies to editor-delegated reads. FR-13 tests that `fs/read_text_file` on `~/.ssh/id_rsa` is refused **before** any message is sent to the client, which also means the denial is recorded and visible in `tag permissions log`.

### 9.5 Streaming from the Agent Loop

`agent.Loop.Run` currently accumulates a `*Result` and returns it. ACP needs incremental delivery, so `Options` gains one field:

```go
// Options gains:
//   // OnEvent, when non-nil, receives loop events as they occur. Nil preserves
//   // today's batch behaviour exactly, so every existing caller is unaffected.
//   OnEvent func(Event)

type Event struct {
	Kind     string // "text" | "reasoning" | "tool_call" | "tool_result" | "step_finish"
	Text     string
	ToolName string
	ToolArgs map[string]any
	Result   string
	Err      string
	Usage    *llm.Usage
}
```

A callback rather than a channel keeps the loop's control flow unchanged and avoids introducing a second goroutine and its shutdown semantics into `internal/agent`. The ACP server's handler translates each `Event` into a `session/update` notification. `internal/cli`'s existing callers pass `nil` and are byte-for-byte unaffected — which is the property that makes this a safe change to a package every command depends on.

### 9.6 Capability Negotiation

| Client offers | TAG behaviour |
|---|---|
| `fs.readTextFile` | `read_file` delegated to the client (unsaved buffers), gated as `KindPath` |
| `fs.writeTextFile` | `write_file` delegated (edits appear in editor diff UI), gated as `KindPath` |
| `terminal` | `bash` delegated to the editor terminal, gated as `KindCommand` |
| *(none of the above)* | TAG's own root-confined `internal/tool` implementations, gated identically |

The negotiated set is persisted in `editor_sessions.capabilities_json` and shown by `tag editor sessions show`, so "why did the agent read stale content?" is answerable after the fact rather than by guesswork.

### 9.7 Integration Points

| Package | Integration |
|---|---|
| `internal/mcp` | Framing and read-loop prior art; concurrent-safe write pattern reused. |
| `internal/agent` | `Options.OnEvent` streaming callback (nil = unchanged). |
| `internal/tool` | Local tools used when the client lacks capabilities; delegated tools wrapped identically. |
| `internal/permission` | `acpPrompter`; policy resolved by the same `permFlags.policy()` path as `tag run`. **No changes to the package.** |
| `internal/store` | `editor_sessions`; `runs` rows with kind `editor`. |
| `internal/cli` | `tag editor` group; reuses `permFlags`. |
| PRD-013 | Per-turn spans. |
| PRD-018 | `session/load` truncation within the context budget. |
| PRD-035 (`internal/lsp`) | Sibling editor-facing surface; conventions shared, code not. LSP answers language questions; ACP drives an agent. Both may run against one editor simultaneously. |
| PRD-039 | Per-session budgets. |

---

## 10. Security Considerations

1. **An editor-spawned agent has the developer's full privileges.** The mitigation is that it runs under the *same* `Guard`, the *same* resolved ruleset (`bash` and `write_file` default to `ask`), the *same* always-on credential-path denies, and the *same* `permission_decisions` audit trail as the CLI. FR-08's ruleset-equality test is the assertion that these have not drifted apart.

2. **Client-delegated filesystem access must not become a policy hole.** `fs/read_text_file` is checked before the client round-trip (§9.4, FR-13). Treating it as "the editor's operation, therefore out of scope" would be the single most likely implementation mistake in this PRD, and is called out here, in the FRs and in the tests for that reason.

3. **Unanswerable permission requests deny.** A closed pipe, a cancelled context or a timeout resolves to `ResponseDeny` (FR-11), matching `Guard`'s existing non-interactive contract. There is no path where losing the client grants a permission.

4. **No network surface.** stdio only, no `net.Listen` under any flag (NG7, NFR-10, tested). This is the sharpest possible contrast with the audit's finding that OpenCode's `serve` is unauthenticated by default and, with `--mdns`, binds `0.0.0.0` while exposing shell-capable endpoints. TAG's editor bridge cannot be exposed to a network because there is nothing to expose.

5. **Prompt injection from repository content.** Unchanged and unimproved by this PRD: file contents entering context can carry injected instructions. The mitigation remains the permission gate on every consequential action — now rendered in an editor dialog, which is arguably better than a terminal prompt because there is more room to show what is being requested.

6. **Path traversal from a hostile or buggy client.** Delegated paths are normalized to absolute, cleaned form before rule matching (FR-15), so a client-supplied `../../..` is matched as the path it actually denotes.

7. **stdout discipline.** A stray write to stdout corrupts the protocol stream. All output goes to stderr or `--log-file`, enforced by a test that parses the entire stdout of a full session including error paths (FR-19).

8. **Session data at rest.** `editor_sessions.history_json` contains conversation content, which may include file excerpts. It lives in the existing `~/.tag/runtime/tag.sqlite3` under the existing permissions, is retained per `session_retention_days`, and is removable via `tag editor sessions rm`.

9. **Acronym confusion as a security concern, not merely a documentation one.** An operator who believes `tag editor serve` is an intra-cluster REST service (PRD-087) might attempt to expose it; one who believes `tag acp serve` is a local stdio bridge might under-secure a network listener. The banner box, distinct commands, distinct package names and `doctor` output exist to prevent both directions of that error.

---

## 11. Testing Strategy

### 11.1 Unit Tests (`internal/editoracp/*_test.go`)

- `TestFraming` — split reads, coalesced reads, oversized messages, trailing garbage.
- `TestInitializeVersionNegotiation` — supported, unsupported, missing.
- `TestCapabilityHonesty` — every advertised capability has a dispatch entry.
- `TestPermissionOptionMapping` — full ACP-option → `permission.Response` matrix.
- `TestPrompterDeniesOnClosedPipe` / `TestPrompterDeniesOnCancel`.
- `TestDelegatedPathNormalization` — fuzz over traversal shapes.
- `TestMalformedMessageProducesErrorNotExit`.
- `TestOnEventNilPreservesBatchBehaviour` — regression on `internal/agent`.

### 11.2 Integration Tests (`internal/cli/editor_e2e_test.go`)

A mock ACP client drives the server over pipes, following the `internal/mcp` client/server test pattern.

- `TestFullSessionEcho` — `initialize` → `session/new` → `session/prompt` → updates → completion.
- `TestStreamingOrder` — text, tool-call, tool-result notifications arrive in causal order.
- `TestPermissionDialogAllowOnce` / `TestPermissionDialogDeny` / `TestPermissionDialogAllowSession` (second call not re-prompted).
- `TestEveryToolCallAudited` — `permission_decisions` row count equals tool-call count.
- `TestRulesetIdenticalToCLI` — same flags and config produce byte-identical resolved rulesets on both surfaces.
- `TestCredentialDenyOnDelegatedRead` — `~/.ssh/id_rsa` denied before any client message.
- `TestUnsavedBufferRead` — mock client's buffer differs from disk; agent sees the buffer.
- `TestTerminalDelegation` — with and without the capability.
- `TestCancelAbortsProvider` — slow stub provider; `session/cancel` returns < 500 ms.
- `TestSessionLoadTruncatesWithinBudget`.
- `TestStdoutIsPureJSONRPC` — parse the whole stdout of a session including error paths.
- `TestNoListenerOpened` — assert no socket under every flag combination.
- `TestRunAndSpansRecorded` — `runs` kind `editor`, spans present, cost in `tag costs`.

### 11.3 Conformance (`internal/editoracp/conformance_test.go`)

Replay captured Zed and JetBrains transcripts from `testdata/` and assert responses validate against the v1 schema. This is the test that catches spec drift, and the transcripts are checked in so it runs offline.

### 11.4 Benchmarks

- `BenchmarkInitialize` — < 100 ms cold.
- `BenchmarkFirstUpdateLatency` — < 200 ms with a streaming stub.

---

## 12. Acceptance Criteria

| ID | Criterion | Verification |
|----|-----------|-------------|
| AC-01 | Zed configured with `{"command":"tag","args":["editor","serve"]}` completes a full prompt/response cycle. | Manual + mock-client integration test |
| AC-02 | Every editor-originated tool call appears in `tag permissions log`. | Integration test |
| AC-03 | A `bash` call in an editor session raises a `session/request_permission` dialog under the default policy. | Integration test |
| AC-04 | Denying in the editor returns an honest "permission denied" tool result and the loop continues. | Integration test |
| AC-05 | `fs/read_text_file` on `~/.ssh/id_rsa` is denied before any client round-trip. | Integration test |
| AC-06 | With `fs/read_text_file` negotiated, the agent reads unsaved buffer content. | Integration test |
| AC-07 | `session/cancel` returns within 500 ms and the provider request is aborted. | Timed integration test |
| AC-08 | stdout parses entirely as framed JSON-RPC across a full session including error paths. | Integration test |
| AC-09 | No socket is opened under any flag combination. | Architecture test |
| AC-10 | A full session completes against `--provider echo` with egress blackholed. | Offline CI job |
| AC-11 | `tag editor doctor` prints the PRD-087 distinction and a valid Zed config snippet. | Golden-file test |
| AC-12 | Editor sessions appear in `tag runs`, `tag trace` and `tag costs` alongside CLI runs. | Integration test |
| AC-13 | Resolved permission rulesets are byte-identical between an ACP session and a `tag run` with the same flags and config. | Integration test |
| AC-14 | With `OnEvent` nil, `agent.Loop.Run` behaviour is byte-identical to the pre-feature build. | Regression test |

---

## 13. Dependencies

| Dependency | Type | Justification |
|---|---|---|
| `encoding/json` | Stdlib | JSON-RPC 2.0 |
| `bufio`, `os` | Stdlib | stdio framing (pattern from `internal/mcp/frame.go`) |
| `modernc.org/sqlite` | Core (project driver) | `editor_sessions` |
| `github.com/spf13/cobra` | Core | `tag editor` group |
| `internal/permission` (shipped, no PRD) | Internal | **Hard prerequisite.** ACP requires a consent model; TAG has one and it maps one-to-one. Without it this PRD would be a security regression. |
| `internal/mcp` (shipped) | Internal | JSON-RPC-over-stdio prior art |
| PRD-021 (agent loop) | Internal | The loop being exposed; gains `OnEvent` |
| PRD-013 (tracing) | Internal | Per-turn spans |
| PRD-018 (context management) | Internal | `session/load` truncation |
| PRD-035 (IDE bridge / LSP) | Internal | Sibling editor-facing surface and stdio conventions |
| PRD-039 (token budget) | Internal | Per-session caps |
| PRD-078 (HITL tool approval) | Internal | `session/request_permission` is the wire form of this |
| PRD-087 (IBM ACP REST adapter) | **Not a dependency** | **Different protocol.** See the banner box. Neither blocks the other. |

---

## 14. Open Questions

| # | Question | Owner | Resolution Target |
|---|----------|-------|-------------------|
| OQ-1 | Is `tag editor` the right name, or should it be `tag zed-acp` / `tag agent-client`? `editor` is the clearest description of purpose and avoids "ACP" entirely, which is its main virtue; the counter-argument is that people searching for "ACP" will not find it — mitigated by an alias and by `doctor` output. | Product | Before implementation |
| OQ-2 | Should client-delegated `fs/*` be preferred by default? Unsaved-buffer awareness is the headline benefit, but it means TAG's root confinement is replaced by the client's notion of scope for those calls. The permission gate still applies (FR-13), but the *root* guarantee differs. Proposal: prefer by default, document the difference, offer `prefer_client_fs: false`. | Security | Before implementation |
| OQ-3 | Should ACP sessions have their own default permission profile (stricter than CLI, since the human is further from the action), or is identical policy the right invariant? Identical is proposed — one policy, one audit trail, no surface-specific surprises. | Security | Before implementation |
| OQ-4 | Should TAG's memory tools (`mem search`, `graph`) be exposed as agent tools inside editor sessions? This is where the differentiator becomes visible in the editor, but it widens the tool surface. Proposal: opt-in via config in v1. | Product | Before v1 |
| OQ-5 | Should `session/load` restore full history or a compacted summary (PRD-018)? Full within budget, then compact, is proposed. | Engineering | During implementation |
| OQ-6 | Does ACP v1 stability warrant pinning a protocol version in the binary, or should `--protocol-version` negotiate a range? Range negotiation is proposed. | Engineering | During implementation |
| OQ-7 | Should registry submission be part of the launch checklist? It is free reach but implies a support commitment across 13 clients TAG cannot test. | Product | After v1 soak |
| OQ-8 | Should `internal/lsp` (PRD-035) and `internal/editoracp` share a process when an editor uses both? Two subprocesses is simpler and matches how editors treat LSP servers. Proposal: separate. | Arch | Before implementation |

---

## 15. Complexity and Timeline

**Total Estimated Effort:** M (2-3 weeks, 1 engineer)

### Phase 1 — Transport and handshake (Days 1-4)
- `protocol.go` v1 wire types; `server.go` read loop, dispatch, mutex-guarded writer, error envelopes
- `initialize` version and capability negotiation; stdout-purity discipline
- Deliverable: a mock client completes `initialize`; framing and malformed-input fuzz tests pass

### Phase 2 — Sessions and streaming (Days 5-9)
- `session/new`, `session/prompt`, `session/cancel`; `editor_sessions` persistence
- `agent.Options.OnEvent` + `stream.go` translation to `session/update`
- Regression test proving nil `OnEvent` is unchanged
- Deliverable: full echo session with ordered streaming updates

### Phase 3 — Permission bridge (Days 10-13)
- `acpPrompter`; policy via the shared `permFlags.policy()` path
- Ruleset-equality test against `tag run`; audit-row coverage; deny-on-disconnect
- Deliverable: AC-02 through AC-04 and AC-13 pass

### Phase 4 — Client-delegated tools (Days 14-17)
- `fs/read_text_file`, `fs/write_text_file`, `terminal/*` with identical gating and path normalization
- Capability-driven fallback to local `internal/tool` implementations
- Deliverable: AC-05, AC-06 pass; delegated-path fuzz clean

### Phase 5 — Observability, doctor, conformance (Days 18-21)
- `runs`/spans/costs integration; `session/load` with budget truncation
- `tag editor doctor`, `tag editor sessions`; captured-transcript conformance suite
- Offline CI job; the PRD-087 disambiguation string tests
- Deliverable: all 14 AC items pass; benchmarks meet NFR targets

---

*PRD-131 authored for TAG. Status: Proposed — not built.*
*This PRD implements **Zed's Agent Client Protocol** (editor↔agent, JSON-RPC over stdio). It is **not** PRD-087, which implements **IBM's Agent Communication Protocol** (intra-cluster REST). Same acronym, different protocol, different command (`tag editor` vs `tag acp`), different package (`internal/editoracp` vs `internal/acp`).*
