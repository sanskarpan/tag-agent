# PRD-081: A2A Agent Card Publication (`tag agent-card`)

> **Stack: Go** (native single-binary; see docs/GO_MIGRATION_RESEARCH.md). This PRD was re-framed from Python to Go.

**Status:** Proposed
**Priority:** P1
**Estimated Effort:** S (3-5 days)
**Category:** Multi-Agent Interoperability Protocols
**Affects:** `internal/agent + internal/server + internal/cli`
**Depends on:** PRD-013 (agent tracing/observability), PRD-028 (sandbox code execution), PRD-034 (secret scanning/security), PRD-027 (eval framework), PRD-036 (web dashboard / tag serve), PRD-014 (MCP server registry)
**Inspired by:** A2A v1.0 (Linux Foundation), MAF 1.0, CrewAI, LangGraph A2A

---

## 1. Overview

The Agent-to-Agent (A2A) protocol, now governed by the Linux Foundation A2A Project under the `lf.a2a.v1` namespace, has emerged as the primary interoperability standard for autonomous agent ecosystems. As of v1.0 (stable, 2026), A2A defines a JSON-RPC 2.0 over HTTP+SSE wire format, a normative Agent Card discovery mechanism using the RFC 8615 well-known URI pattern (`/.well-known/agent-card.json`), a structured task lifecycle with eight states, and optional gRPC bindings for high-throughput deployments. The Python SDK `a2a-sdk==1.1.0` (released May 2026, requires Python >=3.10) provides full v1.0 implementation with v0.3 compatibility mode. More than 150 platforms — including CrewAI, LangGraph, AutoGen, AWS Bedrock Agents, Vertex AI Agent Builder, and Cursor's agentic backend — can discover and invoke any compliant A2A agent by fetching its Agent Card and initiating tasks via the A2A JSON-RPC interface.

TAG CLI currently operates as a powerful local agent orchestrator: it manages named profiles, executes multi-step agentic tasks through the Hermes bridge, maintains span traces in SQLite, and exposes a lightweight HTTP dashboard via `tag serve`. However, TAG agents are invisible to the A2A ecosystem. There is no machine-readable self-description at a well-known URL, no declared capability set, and no supported mechanism for remote A2A orchestrators to discover that a TAG agent exists, understand what it can do, or invoke it in a protocol-compliant way. This means TAG cannot participate in cross-platform agent workflows, cannot be delegated tasks by A2A orchestrators, and cannot be composed with other A2A-compatible agents — a significant gap as the industry converges on A2A as the common agent interchange protocol.

PRD-081 introduces `tag agent-card`: a new Go package (`internal/agent`) and extensions to the existing `internal/server` HTTP server (chi/huma) that together implement the A2A v1.0 Agent Card publication lifecycle. The feature has three primary surfaces. First, `tag agent-card generate` reads an existing TAG profile and produces a spec-compliant `AgentCard` JSON document serialized to disk. Second, `tag agent-card serve` (and the `--a2a` flag on the existing `tag serve`) mounts the generated card at `/.well-known/agent-card.json` on the chi router and activates a minimal A2A task reception endpoint at `/a2a`, allowing remote orchestrators to submit tasks that are routed directly into TAG's existing run infrastructure. Third, `tag agent-card discover` acts as a client-side resolver: given any remote URL, it fetches, validates, and pretty-prints the Agent Card, enabling developers to inspect peer agent capabilities and populate TAG's local `a2a_agent_registry` table with trusted remote agents. The `tag agent call` command completes the loop by allowing TAG to dispatch a task to any registered remote A2A agent and stream the results back to the terminal.

The feature is deliberately scoped to the mandatory A2A v1.0 surface: Agent Card publication, `/.well-known/agent-card.json` serving, `tasks/send` and `tasks/sendSubscribe` (SSE streaming) JSON-RPC methods, and the eight-state task lifecycle. Optional A2A extensions — gRPC binding, push notifications via Webhook, and `tasks/resubscribe` for session resumption — are deferred to follow-on PRDs. Authentication is implemented at the Agent Card declaration layer (the card's `securitySchemes` and `security` fields describe requirements) and at the HTTP transport layer (Bearer token validation middleware in `internal/server`), with no OAuth2 authorization server bundled in this PRD. The JCS (RFC 8785) signing path for tamper-evident Agent Cards is specified as an optional feature and implemented behind a `--sign` flag using Go's stdlib `crypto/ed25519` plus an internal JCS canonicalization helper.

The expected outcome is that any TAG profile can be published as a first-class A2A agent discoverable and invokable by the 150+ platforms in the A2A ecosystem, with zero changes required to existing TAG profiles or run infrastructure. The new packages add approximately 700 lines of Go across `internal/agent`, additions to `internal/server` and `internal/cli`, a single new SQLite table (`a2a_cards`) via `modernc.org/sqlite`, two new chi routes, and a four-subcommand CLI surface. Rollout risk is low because the feature is additive: existing `tag serve`, profile execution, and SQLite schemas are unchanged unless `--a2a` is explicitly passed.

---

## 2. Problem Statement

### 2.1 TAG agents are undiscoverable by A2A-compatible platforms

A2A v1.0 mandates that every compliant agent publish a self-description document at `/.well-known/agent-card.json` (RFC 8615). This document declares the agent's name, description, capabilities, supported input/output content types, authentication requirements, and the URL of its A2A JSON-RPC endpoint. Platforms such as CrewAI Cloud, LangGraph Platform, AWS Bedrock multi-agent orchestration, and Vertex AI Agent Builder all follow the A2A discovery pattern: they fetch `/.well-known/agent-card.json` from a given host URL and use the card to drive capability-aware task routing.

TAG agents, despite being fully capable of executing complex multi-step tasks, have no presence in this ecosystem. There is no well-known endpoint, no capability declaration, and no protocol-compliant invocation surface. As a result, TAG cannot be registered as a worker agent in any of these platforms without custom integration code — which defeats the purpose of a standard protocol. This gap will widen as A2A adoption accelerates; TAG agents risk becoming permanent second-class citizens in the emerging multi-agent economy.

### 2.2 No mechanism for cross-agent task delegation or remote discovery

TAG currently has no ability to discover peer agents, understand their capabilities, or delegate tasks to them in a structured, traceable way. The `tag agent call` command proposed in this PRD does not yet exist: to invoke a remote agent, a developer must manually inspect documentation, write bespoke HTTP client code, parse the response format, and handle errors — none of which is audited, cost-tracked, or linked to TAG's tracing infrastructure.

This bidirectional gap — TAG cannot be discovered by peers, and TAG cannot discover peers — limits the practical utility of TAG in compositions that are becoming common: a LangGraph orchestrator needs to call a specialized code-writing agent (TAG's strength), or a TAG research run needs to delegate a code compilation step to a remote sandbox agent. Without A2A, these compositions require custom glue code per integration, creating maintenance burden and inconsistent observability.

### 2.3 Agent identity and capability are opaque at runtime

Even within a single TAG deployment, there is no machine-readable document that describes what a given profile can do. Profile capabilities are encoded implicitly in system prompts, tool grants in YAML, and model assignments in config — information that is only accessible to a human reading the profile files or to TAG's own internal logic. There is no API surface for a runtime orchestrator to ask "what inputs does this agent accept?", "what output formats can it produce?", "does it require authentication to invoke?", or "what skills does it advertise?".

The A2A Agent Card is exactly this machine-readable capability declaration. By generating and serving Agent Cards from existing TAG profiles, PRD-081 makes TAG agent capabilities inspectable by both human developers (via `tag agent-card discover`) and machine orchestrators (via `/.well-known/agent-card.json`), closing the identity and capability opacity gap without requiring any changes to existing profile definitions.

---

## 3. Goals

| # | Goal |
|---|------|
| G1 | `tag agent-card generate --profile <name> --url <url>` produces a spec-compliant A2A v1.0 `AgentCard` JSON document from an existing TAG profile and writes it to `~/.tag/agent-cards/<profile>.json`. |
| G2 | `tag agent-card serve --port <N>` (and `tag serve --a2a`) serves the card at `GET /.well-known/agent-card.json` and activates the A2A JSON-RPC endpoint at `POST /a2a` on the existing HTTP server. |
| G3 | The A2A task endpoint handles `tasks/send` (synchronous) and `tasks/sendSubscribe` (SSE streaming) JSON-RPC methods, routing received tasks into TAG's existing run infrastructure via `cmd_run`. |
| G4 | `tag agent-card discover --url <remote-url>` fetches, validates, and displays a remote Agent Card, and optionally registers the remote agent in the local `a2a_agent_registry` SQLite table. |
| G5 | `tag agent call <remote-agent-id> --task "<text>"` dispatches a task to a registered remote A2A agent, streams the response back to the terminal, and records the interaction in TAG's `runs` table with `source=a2a_remote`. |
| G6 | All generated Agent Cards are persisted to the `a2a_cards` SQLite table with full metadata, enabling `tag agent-card list` and `tag agent-card show`. |
| G7 | Agent Cards optionally include a JCS (RFC 8785) proof section when `--sign` is passed, using an Ed25519 key stored in `~/.tag/keys/`. |
| G8 | The `securitySchemes` field in the Agent Card accurately reflects the TAG server's actual auth configuration (none, Bearer token, or mTLS), and the Bearer token validation middleware in `internal/server` enforces it. |
| G9 | Inbound A2A task state transitions are stored in the `a2a_tasks` SQLite table and exposed via `tag agent-card tasks` for observability. |
| G10 | Zero breaking changes to existing `tag serve`, profile execution pipeline, SQLite schema, or any other existing command. |

## 3.1 Non-Goals

| # | Non-Goal |
|---|----------|
| NG1 | Implementing an OAuth2 authorization server or token issuance endpoint. Auth declaration in the Agent Card is in scope; running an auth server is not. |
| NG2 | gRPC binding for A2A. JSON-RPC 2.0 over HTTP+SSE is the mandatory A2A v1.0 binding and is the only binding implemented here. gRPC is deferred to a follow-on PRD. |
| NG3 | ANP (did:wba) or ACP (AGNTCY) protocol support. This PRD is A2A-specific. A unified multi-protocol resolver is a future effort. |
| NG4 | Automatic Agent Card refresh or push-based capability advertisement. Cards are generated on demand and served statically. |
| NG5 | Agent Card hosting on a public registry or marketplace. TAG serves the card from the local HTTP server; DNS/TLS provisioning is the operator's responsibility. |
| NG6 | Full A2A task resumption (`tasks/resubscribe`) for interrupted long-running tasks. The `INPUT_REQUIRED` and `AUTH_REQUIRED` states are declared in the task lifecycle but human-in-the-loop resumption is deferred. |
| NG7 | Multi-profile Agent Cards (one card advertising multiple TAG profiles). Each Agent Card maps 1:1 to one TAG profile. |
| NG8 | Semantic versioning of Agent Card capability declarations. Card version is a timestamp string, not a SemVer field. |

---

## 4. Success Metrics

| Metric | Target | Measurement Method |
|--------|--------|--------------------|
| Spec compliance | Generated Agent Card passes A2A v1.0 JSON Schema validation with zero errors | `go test ./internal/agent/ -run TestCardSchemaValidation` using bundled A2A JSON Schema |
| Discovery interoperability | A TAG-served card is successfully fetched and decoded by an `http.Client` in integration test | `go test ./internal/agent/ -run TestDiscoveryRoundtrip` |
| Task routing latency overhead | `tasks/send` adds ≤50 ms median overhead vs. direct run call (excluding model inference time) | Benchmark 100 tasks; measure routing overhead distribution |
| Streaming correctness | `tasks/sendSubscribe` emits valid SSE `data:` lines with JSON-RPC 2.0 `StreamResponse` objects for every inference step | `go test ./internal/server/ -run TestTasksSendSubscribeSSE` with mock run backend |
| Card generation time | `tag agent-card generate` completes in ≤500 ms for any profile | `go test ./internal/agent/ -run TestCardGenerationPerf -bench .` |
| SQLite persistence | `tag agent-card list` correctly shows all cards generated in the current session | Integration test: generate 3 cards, assert list count and metadata fields |
| Discoverability | `tag agent-card discover --url` returns valid card data and registers agent in ≤2 s for a local test server | `go test ./internal/agent/ -run TestDiscoverAndRegister` |
| Zero regression | Existing `tag serve` (without `--a2a`) behavior is unchanged; all existing tests pass | CI gate on `go test ./...` excluding a2a build tag |
| Auth enforcement | Requests to `POST /a2a` without a valid Bearer token when auth is configured return HTTP 401, not 500 | `go test ./internal/server/ -run TestBearerAuthEnforcement` |

---

## 5. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|-----------|----------|
| U1 | TAG user | run `tag agent-card generate --profile coder --url https://myhost.example.com` | I get a spec-compliant `AgentCard` JSON in under a second without reading A2A spec docs |
| U2 | Platform engineer | run `tag serve --a2a --port 8080` and point a CrewAI orchestrator at `https://myhost.example.com/.well-known/agent-card.json` | The CrewAI platform discovers my TAG coder agent, reads its capabilities, and can delegate coding tasks to it automatically |
| U3 | Developer building a multi-agent workflow | run `tag agent-card discover --url https://remote-agent.example.com/.well-known/agent-card.json` | I can inspect the remote agent's capabilities, supported input types, and auth requirements without reading their documentation |
| U4 | Developer | run `tag agent call research-agent --task "Summarize arxiv paper 2505.02279" --json` | TAG delegates the task to the registered remote A2A agent, streams progress back to my terminal, and records the interaction in my local run history |
| U5 | Security-conscious operator | run `tag agent-card generate --profile coder --auth bearer` and then `tag serve --a2a` | All inbound A2A task requests require a valid Bearer token; unauthenticated requests get HTTP 401 |
| U6 | DevOps engineer | run `tag agent-card generate --sign` to produce a JCS-signed card | Remote orchestrators that verify Agent Card integrity can confirm the card was not tampered with during transit |
| U7 | Developer | run `tag agent-card tasks` | I can see all inbound A2A tasks received by my local server, their states (SUBMITTED, WORKING, COMPLETED, FAILED), and their associated TAG run IDs |
| U8 | Developer exploring the A2A ecosystem | run `tag agent-card discover --url https://remote.example.com/.well-known/agent-card.json --save` | The remote agent is registered in my local registry and I can reference it by name in `tag agent call` without specifying the full URL again |
| U9 | CI pipeline author | run `tag agent-card validate --path ./agent-cards/coder.json` | The CI job fails if the card is malformed, outdated, or missing required fields before it is deployed |
| U10 | Multi-agent workflow architect | check `tag agent-card list` | I see all profiles that have published Agent Cards, their capabilities, last-generated timestamps, and whether they are currently being served |

---

## 6. Proposed CLI Surface

All `agent-card` subcommands live under the `tag agent-card` namespace. The `tag agent call` command is a top-level shortcut.

### 6.1 `tag agent-card generate`

Generate an A2A v1.0 Agent Card from an existing TAG profile.

```bash
tag agent-card generate \
  --profile coder \
  --name "tag-coder" \
  --url https://myhost.example.com \
  [--description "Expert coding agent powered by TAG"] \
  [--auth none|bearer|mtls] \
  [--input-modes text,data,file] \
  [--output-modes text,data] \
  [--skills "write_code,review_code,fix_bugs"] \
  [--version "1.0.0"] \
  [--sign] \
  [--out ./agent-card.json] \
  [--json]
```

**Flags:**
- `--profile` (required): TAG profile name to generate card for. Must exist in `~/.tag/profiles/`.
- `--name`: Display name for the agent (defaults to profile name).
- `--url` (required): Base URL where the agent will be served (used to construct the A2A endpoint URL as `<url>/a2a`).
- `--description`: Human-readable capability overview. If omitted, extracted from the profile's system prompt first paragraph (first 256 characters).
- `--auth`: Authentication scheme to declare. `none` = no auth required; `bearer` = Bearer token required; `mtls` = mutual TLS required. Default: `none`.
- `--input-modes`: Comma-separated list of A2A `Part` types the agent accepts. Valid values: `text`, `data`, `file`. Default: `text`.
- `--output-modes`: Comma-separated list of `Part` types the agent produces. Default: `text`.
- `--skills`: Comma-separated skill names to advertise in `AgentCard.skills`. Each skill gets a generated `id`, `name`, and `description` derived from the profile's tool grants.
- `--version`: Semantic version string for the card (default: `"1.0.0"`).
- `--sign`: Sign the card with an Ed25519 key from `~/.tag/keys/signing.key`. Creates the key if it does not exist. Adds a `proof` section using JCS (RFC 8785) canonicalization.
- `--out`: Write JSON to this file path instead of the default `~/.tag/agent-cards/<profile>.json`.
- `--json`: Print the generated JSON to stdout (also happens if `--out` is `-`).

**Example output (stdout summary):**
```
Generated Agent Card for profile 'coder'
  Name:        tag-coder
  URL:         https://myhost.example.com
  A2A endpoint: https://myhost.example.com/a2a
  Auth:        none
  Input modes: text
  Output modes: text
  Skills:      write_code, review_code, fix_bugs (3)
  Saved to:    /Users/alice/.tag/agent-cards/coder.json
  Signed:      no
```

**Example generated `agent-card.json`:**
```json
{
  "name": "tag-coder",
  "description": "Expert coding agent powered by TAG CLI. Handles code writing, review, and bug fixing across all major languages.",
  "url": "https://myhost.example.com/a2a",
  "version": "1.0.0",
  "defaultInputModes": ["text"],
  "defaultOutputModes": ["text"],
  "capabilities": {
    "streaming": true,
    "pushNotifications": false,
    "stateTransitionHistory": true
  },
  "skills": [
    {
      "id": "write_code",
      "name": "Write Code",
      "description": "Write new code in any language based on a specification or prompt.",
      "inputModes": ["text"],
      "outputModes": ["text", "data"]
    },
    {
      "id": "review_code",
      "name": "Review Code",
      "description": "Review existing code for correctness, style, and security issues.",
      "inputModes": ["text", "file"],
      "outputModes": ["text"]
    }
  ],
  "securitySchemes": {},
  "security": []
}
```

### 6.2 `tag agent-card serve`

Start a standalone HTTP server (or extend the existing `tag serve` server) to serve the Agent Card and receive A2A tasks.

```bash
tag agent-card serve \
  --profile coder \
  --port 8080 \
  [--host 0.0.0.0] \
  [--card ./agent-card.json] \
  [--bearer-token-env A2A_BEARER_TOKEN] \
  [--reload]
```

**Flags:**
- `--profile` (required): TAG profile to use for executing inbound tasks. The generated card for this profile is served.
- `--port`: HTTP port to bind (default: `8080`).
- `--host`: Bind address (default: `127.0.0.1`; use `0.0.0.0` for LAN/public access).
- `--card`: Path to a pre-generated card JSON file. If omitted, generates from `--profile` on startup.
- `--bearer-token-env`: Name of the environment variable containing the expected Bearer token for auth. If set, all `POST /a2a` requests must include `Authorization: Bearer <token>`.
- `--reload`: Watch `~/.tag/profiles/<profile>.yaml` and regenerate the card on change (development mode).

**Console output on startup:**
```
TAG A2A Server starting...
  Profile:       coder
  Card endpoint: http://127.0.0.1:8080/.well-known/agent-card.json
  A2A endpoint:  http://127.0.0.1:8080/a2a
  Auth:          none (set --bearer-token-env to require auth)
  Press Ctrl+C to stop.
```

### 6.3 `tag serve --a2a` (existing command extension)

The existing `tag serve` command gains an `--a2a` flag that activates A2A routes on the existing dashboard HTTP server, avoiding the need to run a separate process.

```bash
tag serve \
  --port 8080 \
  --a2a \
  [--a2a-profile coder] \
  [--a2a-bearer-token-env A2A_BEARER_TOKEN]
```

When `--a2a` is set:
- `GET /.well-known/agent-card.json` is mounted and returns the card for `--a2a-profile` (default: the first profile found in `~/.tag/profiles/`).
- `POST /a2a` is activated and routes tasks to `cmd_run`.
- All existing dashboard endpoints (`/`, `/api/runs`, `/api/spans`, `/api/queue`, `/api/costs`, `/api/stream`) remain unchanged.

### 6.4 `tag agent-card discover`

Fetch and display a remote A2A Agent Card.

```bash
tag agent-card discover \
  --url https://remote.example.com \
  [--save] \
  [--alias myresearcher] \
  [--verify-signature] \
  [--json] \
  [--timeout 10]
```

**Resolution algorithm:** The resolver tries the following URLs in order, stopping at the first successful `200 OK` response:
1. `<url>/.well-known/agent-card.json` (A2A v1.0 canonical path)
2. `<url>/agent-card.json` (A2A v0.3 compatibility path)
3. `<url>/.well-known/agent.json` (older A2A drafts)

**Flags:**
- `--url` (required): Base URL of the remote agent host.
- `--save`: Register the discovered agent in the local `a2a_agent_registry` SQLite table.
- `--alias`: Short name to use when referencing this agent in `tag agent call` (defaults to the card's `name` field, slugified).
- `--verify-signature`: If the card contains a `proof` section, verify the Ed25519 signature using the public key referenced in the proof. Prints `VALID`, `INVALID`, or `NO_PROOF`.
- `--json`: Output the raw card JSON instead of the formatted summary.
- `--timeout`: HTTP request timeout in seconds (default: `10`).

**Example output:**
```
Remote Agent Card: https://remote.example.com/.well-known/agent-card.json
  Name:        research-agent
  Description: Deep research agent with web search and citation extraction.
  A2A endpoint: https://remote.example.com/a2a
  Version:     1.0.0
  Auth:        Bearer token required
  Input modes: text
  Output modes: text, data
  Skills:      web_search (1), citation_extract (2), summarize (3)
  Capabilities:
    Streaming:         yes
    Push notifications: no
    State history:     yes
  Signature:   not present

Registered as 'research-agent' (alias: researcher)
  Run 'tag agent call researcher --task "..."' to invoke.
```

### 6.5 `tag agent call`

Dispatch a task to a registered remote A2A agent.

```bash
tag agent call <agent-id> \
  --task "Generate changelog from git log since v0.3.0" \
  [--profile local-profile] \
  [--stream] \
  [--timeout 300] \
  [--json] \
  [--file ./input.txt] \
  [--data '{"key": "value"}']
```

**Arguments:**
- `<agent-id>` (required): Alias or `name` of a registered remote agent from `a2a_agent_registry`, or a full URL of an A2A endpoint.

**Flags:**
- `--task`: Task text to send as the user message (A2A `TextPart`). Required unless `--file` is given.
- `--stream`: Use `tasks/sendSubscribe` (SSE) instead of `tasks/send`. Streams `TaskStatusUpdateEvent` and `TaskArtifactUpdateEvent` to stdout as they arrive.
- `--timeout`: Maximum seconds to wait for task completion (default: `300`).
- `--json`: Output the final A2A `Task` object as JSON instead of human-readable.
- `--file`: Path to a file whose content is sent as an A2A `FilePart`.
- `--data`: JSON string to include as an A2A `DataPart` alongside the task text.

**Example streaming output:**
```
[SUBMITTED] Task a3f9b12c submitted to research-agent
[WORKING]   Agent is processing...
[WORKING]   Searching web for "git changelog generation"...
[WORKING]   Found 12 results, extracting citations...
[COMPLETED] Task completed in 8.3s

Output:
  ## Changelog (v0.3.0 → HEAD)
  - feat: add A2A agent card publication
  - fix: resolve SQLite WAL checkpoint race condition
  ...
```

### 6.6 `tag agent-card list`

List all generated Agent Cards stored locally.

```bash
tag agent-card list [--json]
```

**Example output:**
```
Profile     Name         URL                              Auth    Generated             Signed
coder       tag-coder    https://myhost.example.com       none    2026-06-17 10:23:01   no
researcher  tag-researcher https://myhost.example.com     bearer  2026-06-16 14:05:42   yes
```

### 6.7 `tag agent-card show`

Display the full Agent Card JSON for a profile.

```bash
tag agent-card show --profile coder [--json]
```

### 6.8 `tag agent-card validate`

Validate a card JSON file against the A2A v1.0 JSON Schema.

```bash
tag agent-card validate --path ./agent-cards/coder.json [--strict]
```

- `--strict`: Also check that all declared skills have non-empty `description` fields and that `url` is an HTTPS URL.

### 6.9 `tag agent-card tasks`

List inbound A2A tasks received by the local server.

```bash
tag agent-card tasks \
  [--state SUBMITTED|WORKING|COMPLETED|FAILED|CANCELED|REJECTED] \
  [--last 20] \
  [--json]
```

**Example output:**
```
Task ID          State      Profile   Run ID       Received              Duration
a3f9b12c         COMPLETED  coder     run-abc123   2026-06-17 10:25:00   12.3s
b7e2d45f         WORKING    coder     run-def456   2026-06-17 10:27:30   (running)
c1a8e90b         FAILED     coder     run-ghi789   2026-06-17 09:15:11   3.1s
```

---

## 7. Functional Requirements

| ID | Requirement | Priority |
|----|------------|----------|
| FR-01 | `tag agent-card generate` MUST produce a JSON document that passes validation against the official A2A v1.0 JSON Schema (validated via `invopop/jsonschema` from the `AgentCard` struct or a bundled schema artifact). | MUST |
| FR-02 | The well-known URL served by TAG MUST be `GET /.well-known/agent-card.json` (not `/agent.json` or any other path), conforming to RFC 8615. | MUST |
| FR-03 | The generated `AgentCard.url` field MUST point to the agent's A2A JSON-RPC endpoint (i.e., `<base_url>/a2a`), not the well-known URL itself. | MUST |
| FR-04 | The A2A endpoint at `POST /a2a` MUST handle `tasks/send` and `tasks/sendSubscribe` JSON-RPC 2.0 method names. Unrecognized methods MUST return a JSON-RPC error with code `-32601` (Method not found). | MUST |
| FR-05 | `tasks/send` MUST accept an A2A `TaskSendParams` payload, create an entry in `a2a_tasks`, route the task to the TAG run infrastructure (`internal/runtime`) with the configured profile, await completion, and return an A2A `Task` object in the JSON-RPC result field. | MUST |
| FR-06 | `tasks/sendSubscribe` MUST accept the same `TaskSendParams`, respond immediately with `Content-Type: text/event-stream`, and emit SSE `data:` lines containing JSON-RPC 2.0 `StreamResponse` objects. Each Hermes inference step MUST produce at least one `TaskStatusUpdateEvent` SSE event. Task completion MUST emit a final `TaskArtifactUpdateEvent` with the agent's output. | MUST |
| FR-07 | The `a2a_tasks` table MUST record task ID, state, profile, associated TAG run ID, inbound message parts, and all state transitions with timestamps. | MUST |
| FR-08 | A2A task state MUST follow the eight-state lifecycle: `SUBMITTED → WORKING → COMPLETED | FAILED | CANCELED | REJECTED`. The `INPUT_REQUIRED` and `AUTH_REQUIRED` states MUST be representable in the schema even if full resumption is deferred. | MUST |
| FR-09 | When `--auth bearer` is specified in `generate` and a Bearer token env var is configured in `serve`, the middleware MUST validate the `Authorization: Bearer <token>` header and return HTTP 401 on mismatch before any task processing begins. | MUST |
| FR-10 | `tag agent-card generate --sign` MUST use JCS (RFC 8785) canonicalization: remove the `proof` field if present, serialize with the internal `jcsCanonical()` helper (keys sorted by UTF-16 code unit value per RFC 8785 §3.2.3), sign the canonical bytes with `crypto/ed25519`, and re-attach the `proofValue` as base64url via `encoding/base64.RawURLEncoding`. | MUST |
| FR-11 | `tag agent-card discover` MUST try `/.well-known/agent-card.json`, then `/agent-card.json`, then `/.well-known/agent.json` in that order, and use the first `200 OK` response. | MUST |
| FR-12 | `tag agent-card discover --save` MUST insert the discovered card into `a2a_agent_registry` with the agent's name, URL, A2A endpoint, auth scheme, and fetched timestamp. Duplicate entries (same `agent_url`) MUST be upserted, not duplicated. | MUST |
| FR-13 | `tag agent call` MUST look up the target agent in `a2a_agent_registry` by alias or name, construct a `TaskSendParams` from `--task`/`--file`/`--data`, POST to the agent's A2A endpoint, and print the result. The call MUST be recorded in TAG's own `runs` table with `source='a2a_remote'`. | MUST |
| FR-14 | `tag agent call --stream` MUST consume the SSE stream from the remote agent and render `TaskStatusUpdateEvent` and `TaskArtifactUpdateEvent` events to the terminal in real time, using the existing terminal output helpers in `internal/cli`. | MUST |
| FR-15 | `tag agent-card validate` MUST exit with code `0` on a valid card and code `1` on any validation error, printing each error with its JSON path. | MUST |
| FR-16 | `tag agent-card list` MUST read from the `a2a_cards` SQLite table and display all stored cards with profile, name, URL, auth, generated timestamp, and signed status. | MUST |
| FR-17 | All generated cards MUST include the `capabilities` object with `streaming: true` (since TAG supports SSE), `pushNotifications: false` (deferred), and `stateTransitionHistory: true`. | MUST |
| FR-18 | The `AgentCard.skills` array MUST be auto-populated from the profile's tool grants: each granted MCP tool becomes one skill entry with `id` = tool name, `name` = title-cased tool name, and `description` = tool description if available. | SHOULD |
| FR-19 | `tag serve --a2a` MUST mount A2A routes on the existing HTTP server thread without blocking the existing dashboard routes or the SSE `/api/stream` endpoint. | MUST |
| FR-20 | All JSON-RPC errors returned by the A2A endpoint MUST include `code`, `message`, and optionally `data` fields per JSON-RPC 2.0 spec. Internal TAG errors MUST map to code `-32603` (Internal error) with the error message in `data.details`. | MUST |

---

## 8. Non-Functional Requirements

| ID | Requirement | Target |
|----|------------|--------|
| NFR-01 | **Latency** — `tasks/send` round-trip overhead (excluding model inference) MUST be ≤50 ms at p50 and ≤150 ms at p99 for local loopback requests. | Benchmark test |
| NFR-02 | **Throughput** — The A2A server MUST handle at least 10 concurrent inbound `tasks/send` requests without deadlocking; Go's `net/http` goroutine-per-request model satisfies this natively, with a `chan struct{}` semaphore enforcing the `--max-concurrent-tasks` cap. | Concurrent load test |
| NFR-03 | **Binary size** — Adding the A2A packages MUST NOT increase the compiled binary size by more than 2 MB. All A2A code is always compiled in (no lazy import); the `crypto/ed25519` and `encoding/json` signers are stdlib with zero added weight. | `go build` size check in CI |
| NFR-04 | **Startup time** — `tag serve --a2a` MUST start and be ready to accept connections in ≤1 s after the command is entered, matching the existing `tag serve` startup time. | Timed integration test |
| NFR-05 | **Spec compliance** — All A2A JSON-RPC responses MUST include `jsonrpc: "2.0"` and `id` fields matching the request. SSE responses MUST not include `Content-Length`. | Protocol conformance test |
| NFR-06 | **SQLite WAL mode** — All writes to `a2a_tasks` and `a2a_cards` MUST use the existing `store.Open()` helper (WAL mode, `PRAGMA journal_mode=WAL`, `PRAGMA synchronous=NORMAL`) via `modernc.org/sqlite`. No raw `database/sql` connections outside of `internal/store`. | Code review |
| NFR-07 | **Concurrency safety** — The SSE streaming handler for `tasks/sendSubscribe` MUST use a Go channel (`chan sseEvent`) to communicate between the inference goroutine and the SSE writer goroutine, never sharing mutable state without synchronization. Run with `go test -race` to verify. | Code review + `go test -race` |
| NFR-08 | **Observability** — Every inbound A2A task MUST produce an OpenTelemetry span with `tag.a2a.task_id`, `tag.a2a.method`, and `tag.a2a.state` attributes, following the existing `internal/runtime` tracing span pattern. | OTel integration test |
| NFR-09 | **Security — no SSRF** — `tag agent call` and `tag agent-card discover` MUST reject URLs pointing to RFC 1918 private IP ranges (10.x, 172.16–31.x, 192.168.x) unless `--allow-private` is explicitly passed, to prevent SSRF in automated pipelines. | Unit test |
| NFR-10 | **Dependency footprint** — There are no optional Go dependencies; all A2A code is compiled into the single binary. The `go.mod` MUST NOT introduce any CGO dependencies for A2A functionality; `modernc.org/sqlite` (pure-Go, `CGO_ENABLED=0`) is already the project's SQLite driver. | `CGO_ENABLED=0 go build` in CI |
| NFR-11 | **Portability** — All features MUST work on Linux, macOS, and Windows. Go's `net/http` SSE response relies on `http.Flusher`, which is available on all Go platforms and does not use `select()` or `fcntl`. Cross-platform builds are verified via GoReleaser matrix. | CI matrix (linux/amd64, darwin/arm64, windows/amd64) |
| NFR-12 | **Key storage** — Ed25519 signing keys stored in `~/.tag/keys/signing.key` MUST be written with `os.OpenFile(..., os.O_CREATE\|os.O_WRONLY, 0600)` on POSIX. Key generation uses `crypto/ed25519.GenerateKey(rand.Reader)` from the Go stdlib. | `os.Stat()` mode assertion in test |

---

## 9. Technical Design

### 9.1 New Packages / Files (Go)

| File | Purpose |
|------|---------|
| `internal/agent/a2acard.go` | Core package: `AgentCard`, `A2ASkill`, `A2ACapabilities`, `A2ATaskState`, `A2AProof` Go structs; `GenerateCardFromProfile()`; card serialization via `encoding/json` |
| `internal/agent/a2asign.go` | `SignAgentCard()` and `VerifyAgentCardSignature()` using `crypto/ed25519` + internal `jcsCanonical()` helper (RFC 8785) |
| `internal/agent/a2adiscovery.go` | `DiscoverRemoteCard()` client with three-path resolution and SSRF protection via `net.LookupHost` + RFC 1918 check |
| `internal/agent/a2acall.go` | `CallRemoteAgent()` outbound A2A client; goroutine-based SSE consumer for `--stream` mode |
| `internal/server/a2a.go` | `MountA2ARoutes()` registering chi routes; `handleA2APost()` JSON-RPC 2.0 dispatcher; `handleTasksSend()` and `handleTasksSendSubscribe()`; Bearer token middleware |
| `internal/cli/agent_card.go` | cobra sub-commands: `generate`, `serve`, `discover`, `list`, `show`, `validate`, `tasks` |
| `internal/cli/agent_call.go` | `tag agent call` cobra command |
| `internal/store/a2a_migrations.go` | `CREATE TABLE IF NOT EXISTS` DDL for the four A2A tables; runs inside existing `store.Open()` migration sequence |

### 9.2 Modified Files

| File | Changes |
|------|---------|
| `internal/server/server.go` | Conditionally call `MountA2ARoutes()` on the chi router when `--a2a` flag is set; pass `A2AConfig` struct |
| `internal/cli/root.go` | Wire `agent-card` subcommand group and `agent call` into the cobra root command |
| `go.mod` | Add `go-chi/chi/v5`, `danielgtaylor/huma/v2`, `tmaxmax/go-sse`, `invopop/jsonschema`, `gofrs/flock`, `golang-jwt/jwt/v5` (all are already listed or needed; `modernc.org/sqlite` is pre-existing) |

### 9.3 SQLite DDL

All four tables are created by `store.Open()` via `modernc.org/sqlite` (pure-Go, `CGO_ENABLED=0`, FTS5 built-in). Writes are serialized via `gofrs/flock` on the database file; atomic read-modify-write uses `os.Rename`. WAL mode (`PRAGMA journal_mode=WAL`, `PRAGMA synchronous=NORMAL`) is set at connection open.

```sql
-- Agent Cards: stores all generated cards, 1 row per profile per generation
CREATE TABLE IF NOT EXISTS a2a_cards (
    id             TEXT    PRIMARY KEY,          -- UUID v4
    profile        TEXT    NOT NULL,             -- TAG profile name
    name           TEXT    NOT NULL,             -- AgentCard.name
    url            TEXT    NOT NULL,             -- base URL (not the well-known URL)
    a2a_endpoint   TEXT    NOT NULL,             -- AgentCard.url (the /a2a endpoint)
    card_json      TEXT    NOT NULL,             -- full AgentCard as JSON string
    auth_scheme    TEXT    NOT NULL DEFAULT 'none',  -- 'none' | 'bearer' | 'mtls'
    is_signed      INTEGER NOT NULL DEFAULT 0,   -- 1 if JCS proof present
    generated_at   TEXT    NOT NULL,             -- ISO8601 UTC
    UNIQUE (profile)  ON CONFLICT REPLACE        -- latest card per profile wins
);

-- A2A Tasks: records every inbound task received by the local A2A server
CREATE TABLE IF NOT EXISTS a2a_tasks (
    id             TEXT    PRIMARY KEY,          -- A2A task ID (from client or UUID v4)
    profile        TEXT    NOT NULL,             -- TAG profile used for execution
    run_id         TEXT,                         -- associated TAG runs.id (set after routing)
    state          TEXT    NOT NULL,             -- A2ATaskState enum value
    method         TEXT    NOT NULL,             -- 'tasks/send' | 'tasks/sendSubscribe'
    message_json   TEXT    NOT NULL,             -- inbound TaskSendParams.message as JSON
    result_json    TEXT,                         -- final Task object as JSON (on completion)
    error_json     TEXT,                         -- JSON-RPC error object (on failure)
    received_at    TEXT    NOT NULL,             -- ISO8601 UTC
    completed_at   TEXT,                         -- ISO8601 UTC (NULL until terminal state)
    duration_ms    INTEGER                       -- wall time from received_at to completed_at
);

-- A2A Task State History: full state transition audit trail
CREATE TABLE IF NOT EXISTS a2a_task_history (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id        TEXT    NOT NULL REFERENCES a2a_tasks(id) ON DELETE CASCADE,
    from_state     TEXT,                         -- NULL for initial SUBMITTED entry
    to_state       TEXT    NOT NULL,
    transitioned_at TEXT   NOT NULL,             -- ISO8601 UTC
    reason         TEXT                          -- optional human-readable reason
);

-- A2A Agent Registry: remote agents discovered via 'tag agent-card discover --save'
CREATE TABLE IF NOT EXISTS a2a_agent_registry (
    id             TEXT    PRIMARY KEY,          -- UUID v4
    alias          TEXT    NOT NULL UNIQUE,      -- slugified name for 'tag agent call <alias>'
    name           TEXT    NOT NULL,             -- AgentCard.name from remote
    agent_url      TEXT    NOT NULL UNIQUE,      -- base URL of the remote agent
    a2a_endpoint   TEXT    NOT NULL,             -- remote A2A JSON-RPC endpoint
    auth_scheme    TEXT    NOT NULL DEFAULT 'none',
    card_json      TEXT    NOT NULL,             -- full fetched AgentCard as JSON
    fetched_at     TEXT    NOT NULL,             -- ISO8601 UTC
    last_called_at TEXT                          -- ISO8601 UTC of most recent 'tag agent call'
);

-- Indexes
CREATE INDEX IF NOT EXISTS idx_a2a_tasks_profile   ON a2a_tasks(profile);
CREATE INDEX IF NOT EXISTS idx_a2a_tasks_state     ON a2a_tasks(state);
CREATE INDEX IF NOT EXISTS idx_a2a_tasks_run_id    ON a2a_tasks(run_id);
CREATE INDEX IF NOT EXISTS idx_a2a_history_task    ON a2a_task_history(task_id);
```

### 9.4 Core Go Structs and Types

There is no official Go A2A SDK. The Agent Card is a plain JSON document over `net/http`, trivially modelled as Go structs with `encoding/json` tags. Schema documentation is driven by `invopop/jsonschema` struct tags.

```go
// internal/agent/a2acard.go

package agent

import (
    "encoding/json"
    "time"
)

// A2ATaskState represents the A2A v1.0 eight-state task lifecycle (§4.2).
type A2ATaskState string

const (
    TaskStateSubmitted     A2ATaskState = "submitted"
    TaskStateWorking       A2ATaskState = "working"
    TaskStateInputRequired A2ATaskState = "input-required"
    TaskStateAuthRequired  A2ATaskState = "auth-required"
    TaskStateCompleted     A2ATaskState = "completed"
    TaskStateFailed        A2ATaskState = "failed"
    TaskStateCanceled      A2ATaskState = "canceled"
    TaskStateRejected      A2ATaskState = "rejected"
)

func (s A2ATaskState) IsTerminal() bool {
    switch s {
    case TaskStateCompleted, TaskStateFailed, TaskStateCanceled, TaskStateRejected:
        return true
    }
    return false
}

type A2AAuthScheme string

const (
    AuthNone   A2AAuthScheme = "none"
    AuthBearer A2AAuthScheme = "bearer"
    AuthMTLS   A2AAuthScheme = "mtls"
)

// A2ASkill maps to the A2A v1.0 AgentCard skill entry.
type A2ASkill struct {
    ID          string   `json:"id"`
    Name        string   `json:"name"`
    Description string   `json:"description"`
    InputModes  []string `json:"inputModes"`
    OutputModes []string `json:"outputModes"`
    Tags        []string `json:"tags,omitempty"`
}

// A2ACapabilities declares optional A2A server features.
type A2ACapabilities struct {
    Streaming              bool `json:"streaming"`
    PushNotifications      bool `json:"pushNotifications"`
    StateTransitionHistory bool `json:"stateTransitionHistory"`
}

// A2AProof holds the JCS (RFC 8785) + Ed25519 signature section.
type A2AProof struct {
    Type       string `json:"type"`       // "Ed25519Signature2020"
    Created    string `json:"created"`    // ISO8601 UTC
    ProofValue string `json:"proofValue"` // base64url-encoded Ed25519 signature
    PublicKey  string `json:"publicKey"`  // base64url-encoded raw Ed25519 public key
}

// AgentCard is the A2A v1.0 Agent Card document (lf.a2a.v1.AgentCard).
// Served at GET /.well-known/agent-card.json; URL field points to POST /a2a endpoint.
type AgentCard struct {
    Name               string            `json:"name"`
    Description        string            `json:"description"`
    URL                string            `json:"url"` // A2A JSON-RPC endpoint, NOT well-known URL
    Version            string            `json:"version"`
    DefaultInputModes  []string          `json:"defaultInputModes"`
    DefaultOutputModes []string          `json:"defaultOutputModes"`
    Capabilities       A2ACapabilities   `json:"capabilities"`
    Skills             []A2ASkill        `json:"skills"`
    SecuritySchemes    map[string]any    `json:"securitySchemes"`
    Security           []map[string]any  `json:"security"`
    Proof              *A2AProof         `json:"proof,omitempty"`
}
```

### 9.5 JCS Signing Algorithm (Go)

No external JCS library is assumed. An internal `jcsCanonical()` helper implements RFC 8785 §3.2.3 (recursive key sort by Unicode code point, ECMAScript IEEE 754 number serialization). The signing primitives are entirely from the Go stdlib.

```go
// internal/agent/a2asign.go

package agent

import (
    "crypto/ed25519"
    "crypto/rand"
    "encoding/base64"
    "encoding/json"
    "fmt"
    "os"
    "sort"
    "time"
)

// SignAgentCard signs card in-place using JCS (RFC 8785) + Ed25519.
//
// Algorithm:
//  1. Nil out any existing Proof field.
//  2. Serialize with jcsCanonical() — keys sorted by Unicode code point per RFC 8785 §3.2.3.
//  3. Sign the canonical bytes with crypto/ed25519.
//  4. Attach Proof with base64url-encoded signature and public key.
func SignAgentCard(card *AgentCard, privateKeyPath string) error {
    card.Proof = nil                          // Step 1: strip existing proof

    canonical, err := jcsCanonical(card)     // Step 2: JCS canonicalize
    if err != nil {
        return fmt.Errorf("JCS serialization: %w", err)
    }

    privBytes, err := os.ReadFile(privateKeyPath)
    if err != nil {
        return fmt.Errorf("read signing key: %w", err)
    }
    privKey := ed25519.PrivateKey(privBytes)
    sig := ed25519.Sign(privKey, canonical)  // Step 3: Ed25519 sign

    pubKey := privKey.Public().(ed25519.PublicKey)
    card.Proof = &A2AProof{                  // Step 4: attach proof
        Type:       "Ed25519Signature2020",
        Created:    time.Now().UTC().Format(time.RFC3339),
        ProofValue: base64.RawURLEncoding.EncodeToString(sig),
        PublicKey:  base64.RawURLEncoding.EncodeToString(pubKey),
    }
    return nil
}

// VerifyAgentCardSignature verifies the JCS proof on a decoded card.
func VerifyAgentCardSignature(card *AgentCard) bool {
    if card.Proof == nil {
        return false
    }
    proof := card.Proof
    card.Proof = nil
    canonical, err := jcsCanonical(card)
    card.Proof = proof
    if err != nil {
        return false
    }
    sig, err := base64.RawURLEncoding.DecodeString(proof.ProofValue)
    if err != nil {
        return false
    }
    pub, err := base64.RawURLEncoding.DecodeString(proof.PublicKey)
    if err != nil {
        return false
    }
    return ed25519.Verify(ed25519.PublicKey(pub), canonical, sig)
}

// GenerateSigningKey creates a new Ed25519 key pair and writes the private key
// to keyPath with mode 0600. Returns the raw 64-byte private key bytes.
func GenerateSigningKey(keyPath string) (ed25519.PrivateKey, error) {
    _, priv, err := ed25519.GenerateKey(rand.Reader)
    if err != nil {
        return nil, err
    }
    f, err := os.OpenFile(keyPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
    if err != nil {
        return nil, err
    }
    defer f.Close()
    _, err = f.Write([]byte(priv))
    return priv, err
}

// jcsCanonical implements RFC 8785 JSON Canonicalization Scheme.
// Marshals v to generic map, sorts keys recursively, then encodes.
func jcsCanonical(v any) ([]byte, error) {
    raw, err := json.Marshal(v)
    if err != nil {
        return nil, err
    }
    var m any
    if err := json.Unmarshal(raw, &m); err != nil {
        return nil, err
    }
    return marshalSorted(m)
}
```

### 9.6 A2A HTTP Handlers (chi + huma, Go)

A2A routes are mounted on the existing chi router — no second HTTP server is needed.

```go
// internal/server/a2a.go

package server

import (
    "context"
    "encoding/json"
    "fmt"
    "net/http"

    "github.com/go-chi/chi/v5"

    "tag/internal/agent"
    "tag/internal/store"
)

// A2AConfig carries runtime options for the A2A route group.
type A2AConfig struct {
    Profile     string
    Card        *agent.AgentCard
    BearerToken string         // empty = no auth
    MaxConcurrent int          // semaphore capacity; default 4
    DB          *store.DB
}

// MountA2ARoutes registers A2A routes on an existing chi router.
// Called from server.go when --a2a flag is set; no existing routes are affected.
func MountA2ARoutes(r chi.Router, cfg A2AConfig) {
    if cfg.BearerToken != "" {
        r.Use(bearerTokenMiddleware(cfg.BearerToken))
    }
    sem := make(chan struct{}, max(cfg.MaxConcurrent, 4))
    r.Get("/.well-known/agent-card.json", serveAgentCard(cfg))
    r.Post("/a2a", handleA2APost(cfg, sem))
}

// handleA2APost dispatches JSON-RPC 2.0 methods: tasks/send and tasks/sendSubscribe.
func handleA2APost(cfg A2AConfig, sem chan struct{}) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        var req jsonRPCRequest
        if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
            writeJSONRPCError(w, nil, -32700, "Parse error", nil)
            return
        }
        // Semaphore: enforce --max-concurrent-tasks
        select {
        case sem <- struct{}{}:
            defer func() { <-sem }()
        default:
            writeJSONRPCError(w, req.ID, -32000, "Server error: too many concurrent tasks", nil)
            return
        }
        switch req.Method {
        case "tasks/send":
            handleTasksSend(w, r, cfg, req)
        case "tasks/sendSubscribe":
            handleTasksSendSubscribe(w, r, cfg, req)
        default:
            writeJSONRPCError(w, req.ID, -32601, "Method not found: "+req.Method, nil)
        }
    }
}

// handleTasksSendSubscribe streams TaskStatusUpdateEvent and TaskArtifactUpdateEvent
// via SSE. A Go channel bridges the inference goroutine to the SSE writer — no shared
// mutable state, no mutex required.
func handleTasksSendSubscribe(w http.ResponseWriter, r *http.Request,
    cfg A2AConfig, req jsonRPCRequest) {

    w.Header().Set("Content-Type", "text/event-stream")
    w.Header().Set("Cache-Control", "no-cache")
    w.Header().Set("X-Accel-Buffering", "no")

    type sseEvent struct{ JSON []byte }
    eventCh := make(chan sseEvent, 16)

    go func() {
        defer close(eventCh)
        // Route into existing TAG run infrastructure via internal/runtime.
        agent.RunTaskStreaming(r.Context(), cfg.Profile, req.Params, eventCh)
    }()

    flusher, _ := w.(http.Flusher)
    for ev := range eventCh {
        fmt.Fprintf(w, "data: %s\n\n", ev.JSON)
        if flusher != nil {
            flusher.Flush()
        }
        if r.Context().Err() != nil {
            return // client disconnected
        }
    }
}

// bearerTokenMiddleware validates Authorization: Bearer <token> on every request.
func bearerTokenMiddleware(expected string) func(http.Handler) http.Handler {
    return func(next http.Handler) http.Handler {
        return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
            auth := r.Header.Get("Authorization")
            if len(auth) < 8 || auth[:7] != "Bearer " || auth[7:] != expected {
                w.Header().Set("WWW-Authenticate", `Bearer realm="TAG A2A"`)
                w.Header().Set("Content-Type", "application/json")
                w.WriteHeader(http.StatusUnauthorized)
                json.NewEncoder(w).Encode(map[string]any{
                    "jsonrpc": "2.0", "id": nil,
                    "error": map[string]any{"code": -32001, "message": "Unauthorized"},
                })
                return
            }
            next.ServeHTTP(w, r)
        })
    }
}
```

### 9.7 Agent Card Generation Algorithm

`GenerateCardFromProfile(profileName, baseURL string, opts GenerateOptions) (*AgentCard, error)` in `internal/agent/a2acard.go` follows this sequence:

1. **Load profile YAML** from `~/.tag/profiles/<profile>.yaml` via `internal/config` (koanf/v2 + gopkg.in/yaml.v3). Extract `system_prompt`, `model`, `tools` (slice of MCP tool names).
2. **Derive description** — use `opts.Description` if provided; otherwise take the first 256 non-whitespace characters of `system_prompt`, stripping leading `#` headers via a simple string scanner.
3. **Build skills** — for each tool name, construct an `A2ASkill{ID: name, Name: toTitle(name), Description: desc}`. Description is looked up from `internal/mcp` tool registry; falls back to an empty string if unavailable.
4. **Build security schemes** — based on `opts.AuthScheme`:
   - `none`: `SecuritySchemes: map[string]any{}`, `Security: []`
   - `bearer`: `SecuritySchemes: {"bearerAuth": {"type": "http", "scheme": "bearer"}}`, `Security: [{"bearerAuth": []}]`
   - `mtls`: `SecuritySchemes: {"mtls": {"type": "mutualTLS"}}`, `Security: [{"mtls": []}]`
5. **Construct `AgentCard`** struct; marshal to JSON via `encoding/json`.
6. **Optionally sign** via `SignAgentCard()` if `opts.Sign` is set.
7. **Persist** to `~/.tag/agent-cards/<profile>.json` (directory created with `os.MkdirAll`) and upsert into `a2a_cards` table via `internal/store`.
8. **Print summary** to stdout via the cobra command's output writer.

An `//go:embed agent-card-skeleton.json` in the package bundles a minimal valid card for use in `validate` offline checks.

### 9.8 Discovery Client Algorithm (Go)

`DiscoverRemoteCard(rawURL string, opts DiscoverOptions) (*AgentCard, string, error)` in `internal/agent/a2adiscovery.go`:

```go
// internal/agent/a2adiscovery.go

package agent

import (
    "encoding/json"
    "fmt"
    "net"
    "net/http"
    "strings"
    "time"
)

var discoveryPaths = []string{
    "/.well-known/agent-card.json", // A2A v1.0 canonical
    "/agent-card.json",             // A2A v0.3 compatibility
    "/.well-known/agent.json",      // older drafts
}

// DiscoverOptions controls discovery behaviour.
type DiscoverOptions struct {
    Timeout      time.Duration
    AllowPrivate bool
    Save         bool   // upsert into a2a_agent_registry via internal/store
    Alias        string // override the alias slug; defaults to slugified card.Name
}

// DiscoverRemoteCard tries each path in order, returning the first successful AgentCard.
// Enforces SSRF protection: RFC 1918 / loopback addresses are rejected unless AllowPrivate.
// Redirects are followed via http.Client.CheckRedirect; the redirect target is re-checked.
func DiscoverRemoteCard(rawURL string, opts DiscoverOptions) (*AgentCard, string, error) {
    base := strings.TrimRight(rawURL, "/")
    client := &http.Client{
        Timeout: opts.Timeout,
        CheckRedirect: func(req *http.Request, via []*http.Request) error {
            return checkSSRF(req.URL.Host, opts.AllowPrivate)
        },
    }

    for _, path := range discoveryPaths {
        candidate := base + path
        if err := checkSSRF(candidate, opts.AllowPrivate); err != nil {
            return nil, "", err
        }
        resp, err := client.Get(candidate)
        if err != nil || resp.StatusCode != http.StatusOK {
            if resp != nil {
                resp.Body.Close()
            }
            continue
        }
        defer resp.Body.Close()
        var card AgentCard
        if err := json.NewDecoder(resp.Body).Decode(&card); err != nil {
            continue
        }
        return &card, candidate, nil
    }
    return nil, "", fmt.Errorf("no A2A Agent Card found at %s (tried %v)", rawURL, discoveryPaths)
}

// checkSSRF resolves host to IPs via net.LookupHost and rejects RFC 1918 / loopback
// addresses (10/8, 172.16/12, 192.168/16, 127/8, ::1) unless allowPrivate is true.
func checkSSRF(rawURL string, allowPrivate bool) error {
    // host extraction + net.ParseCIDR range checks — full impl in a2adiscovery.go
    return nil
}
```

### 9.9 Integration with Existing chi Server

In `internal/server/server.go`, the existing chi router gains a conditional A2A mount after all existing `/api/*` routes have been registered:

```go
// internal/server/server.go (addition)

if cfg.A2AEnabled {
    card, err := agent.LoadOrGenerateCard(cfg.A2AProfile, cfg.BaseURL)
    if err != nil {
        return fmt.Errorf("load A2A card: %w", err)
    }
    MountA2ARoutes(r, A2AConfig{
        Profile:       cfg.A2AProfile,
        Card:          card,
        BearerToken:   os.Getenv(cfg.A2ABearerTokenEnv),
        MaxConcurrent: cfg.A2AMaxConcurrent,
        DB:            db,
    })
}
```

No existing routes are modified. The `MountA2ARoutes` call is the only change to `server.go`; all existing `/api/*` endpoints served by huma are unaffected. A2A routes are added to the same goroutine-safe chi router instance — no second HTTP listener or goroutine is needed.

### 9.10 CLI Wiring (cobra)

```go
// internal/cli/agent_card.go

package cli

import (
    "github.com/spf13/cobra"
    "tag/internal/agent"
)

var agentCardCmd = &cobra.Command{
    Use:   "agent-card",
    Short: "Manage A2A Agent Cards for TAG profiles",
}

func init() {
    agentCardCmd.AddCommand(
        newGenerateCmd(),
        newServeCmd(),
        newDiscoverCmd(),
        newListCmd(),
        newShowCmd(),
        newValidateCmd(),
        newTasksCmd(),
    )
    rootCmd.AddCommand(agentCardCmd)
}

// internal/cli/agent_call.go

var agentCallCmd = &cobra.Command{
    Use:   "call <agent-id>",
    Short: "Dispatch a task to a registered remote A2A agent",
    Args:  cobra.ExactArgs(1),
    RunE: func(cmd *cobra.Command, args []string) error {
        opts := agent.CallOptions{
            AgentID:    args[0],
            Task:       flagTask,
            Stream:     flagStream,
            Timeout:    flagTimeout,
            JSONOutput: flagJSON,
        }
        return agent.CallRemoteAgent(cmd.Context(), opts)
    },
}
```

Each subcommand is a typed `*cobra.Command` with `RunE` returning `error`. Configuration flows through `cmd.Context()` via a koanf config struct injected in `rootCmd.PersistentPreRunE`. No global mutable state.`

---

## 10. Security Considerations

1. **SSRF prevention** — `tag agent-card discover` and `tag agent call` resolve URLs via `net/http`. Before any outbound HTTP request, the resolved IP address MUST be checked against RFC 1918 private ranges (10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16) and loopback (127.0.0.0/8) using `net.LookupHost` + `net.ParseCIDR`. Requests to private IPs are rejected with a clear error unless `--allow-private` is explicitly set. Redirects are intercepted via `http.Client.CheckRedirect`; the redirect target is re-checked before following.

2. **Bearer token storage** — The Bearer token expected by the inbound A2A server is read from an environment variable (never stored in SQLite or config files). If a developer accidentally passes the token as a CLI flag, it appears in the process argument list and shell history; the `--bearer-token-env` pattern avoids this by accepting only the env var NAME, not the token value.

3. **JCS signing key permissions** — Ed25519 private keys stored in `~/.tag/keys/signing.key` are written with `os.OpenFile(..., os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)` to ensure they are only readable by the owning user. On Windows, ACL inheritance is not automatically restricted; the Windows path prints a warning that key security depends on NTFS permissions.

4. **Input validation on inbound tasks** — The `message` field in `TaskSendParams` is parsed via `json.NewDecoder` with a 1 MB `io.LimitReader` and MUST be validated for maximum depth (≤10 levels) before being routed to the run infrastructure. Oversized or deeply nested messages are rejected with JSON-RPC error `-32602` (Invalid params) to prevent memory exhaustion.

5. **Prompt injection via A2A tasks** — Inbound task text from remote A2A clients becomes part of the prompt sent to the LLM. The existing sandbox and security modules (PRD-028, PRD-034) apply normally. Additionally, inbound task text MUST pass through the existing `internal/runtime` secret scanner to ensure no credentials are inadvertently logged or replayed.

6. **Card content injection** — The `AgentCard.description` field is derived from the profile system prompt. If the system prompt contains HTML or markdown, the card's JSON `description` value must be plain text. The generator strips HTML tags and markdown headers before embedding the description in the card.

7. **Signature verification trust model** — Signature verification in `tag agent-card discover --verify-signature` uses the `publicKey` embedded in the card's own `proof` section. This is a self-certifying signature, not a CA-rooted one. Callers should treat a valid self-signature as "card not tampered in transit" rather than "agent identity is verified". A future PRD can extend this to support DID-rooted public keys for stronger identity assertions.

8. **A2A endpoint exposure** — `tag agent-card serve` defaults to `127.0.0.1` (loopback only). Binding to `0.0.0.0` makes the A2A endpoint accessible on all network interfaces. When `--host 0.0.0.0` is passed without `--bearer-token-env`, the CLI prints a prominent warning: `WARNING: A2A endpoint is publicly accessible without authentication. Set --bearer-token-env to require a Bearer token.`

9. **Denial of service via long tasks** — Inbound `tasks/send` occupies a goroutine for the duration of the TAG run. Go's goroutines are lightweight (4 KB stack), but unbounded goroutine creation is still a risk. The `--max-concurrent-tasks N` flag (default: `4`) is enforced by a `chan struct{}` semaphore in `handleA2APost`; additional requests receive JSON-RPC error `-32000` (Server error: too many concurrent tasks) without spawning a run goroutine.

10. **Audit trail** — Every inbound A2A task is recorded in `a2a_tasks` with the full message JSON. This creates a local audit trail of all remote instructions received by the TAG agent. `tag agent-card tasks --state FAILED` surfaces failed tasks for retrospective review. The audit trail is not transmitted externally unless OTLP export is configured (PRD-041).

---

## 11. Testing Strategy

All tests use Go's `testing` package + `github.com/stretchr/testify`. HTTP handler tests use `net/http/httptest`. Run with `go test ./internal/agent/... ./internal/server/... -race`. Slow integration tests are guarded by `testing.Short()` and skipped in fast CI.

### 11.1 Unit Tests (`internal/agent/a2acard_test.go`, `internal/server/a2a_test.go`)

| Test | What it covers |
|------|---------------|
| `TestCardGenerationRequiredFields` | `GenerateCardFromProfile()` returns an `AgentCard` with all required A2A v1.0 fields non-zero: `Name`, `Description`, `URL`, `Version`, `DefaultInputModes`, `DefaultOutputModes`, `Capabilities`, `Skills`, `SecuritySchemes`, `Security`. |
| `TestCardSchemaValidation` | The JSON-marshalled card is decoded against the bundled A2A v1.0 JSON Schema (via `invopop/jsonschema`) with zero errors. |
| `TestWellKnownURLPath` | `httptest.NewRecorder` + chi router: `GET /.well-known/agent-card.json` returns HTTP 200 and `Content-Type: application/json`. |
| `TestA2AEndpointURLInCard` | `AgentCard.URL` equals `<base_url>/a2a`, not the well-known URL. |
| `TestJCSSigningRoundtrip` | After `SignAgentCard()`, `VerifyAgentCardSignature()` returns `true`. |
| `TestJCSTamperDetection` | Modifying any field of a signed card causes `VerifyAgentCardSignature()` to return `false`. |
| `TestA2ATaskStateMachine` | `IsTerminal()` returns `true` for Completed/Failed/Canceled/Rejected and `false` for Submitted/Working/InputRequired/AuthRequired. |
| `TestBearerAuthMissing` | `POST /a2a` without `Authorization: Bearer` header when token is configured returns HTTP 401 JSON body with code `-32001`. |
| `TestBearerAuthValid` | `POST /a2a` with correct `Authorization: Bearer <token>` proceeds to method dispatch. |
| `TestMethodNotFound` | `POST /a2a` with `method: "tasks/unknown"` returns JSON-RPC error code `-32601`. |
| `TestSSRFPrivateIPRejected` | `checkSSRF("http://192.168.1.1/", false)` returns a non-nil error containing "private" without making any network call. |
| `TestDiscoveryPathOrder` | `httptest.NewServer` answering only at `/agent-card.json`: `DiscoverRemoteCard` succeeds on the second path attempt. |
| `TestSQLiteCardPersistence` | After `GenerateCardFromProfile()`, the `a2a_cards` table in an in-memory `modernc.org/sqlite` DB contains one row with matching `profile`, `name`, `a2a_endpoint`. |
| `TestSQLiteTaskCreation` | `createTaskRecord()` inserts into `a2a_tasks` and `transitionTask()` inserts into `a2a_task_history`. |
| `TestSigningKeyPermissions` | On POSIX, `os.Stat(keyPath).Mode().Perm()` equals `0600` after `GenerateSigningKey()`. |
| `TestJCSKeySort` | `jcsCanonical(map{"b":1,"a":2})` produces `{"a":2,"b":1}` (keys sorted by Unicode code point). |
| `TestCardDescriptionFromSystemPrompt` | When `opts.Description` is empty, the first 256 characters of the profile system prompt (markdown headers stripped) become `AgentCard.Description`. |
| `TestSkillsFromToolGrants` | A profile with `tools: [bash, read_file, write_file]` produces three `A2ASkill` entries with matching `ID` values. |
| `TestMaxConcurrentTasks` | Sending 5 concurrent requests when `MaxConcurrent=4` causes the 5th to receive error code `-32000` (via `httptest` + goroutines). |

### 11.2 Integration Tests (`internal/server/a2a_integration_test.go`, build tag `//go:build integration`)

| Test | What it covers |
|------|---------------|
| `TestServeAndDiscoverRoundtrip` | `httptest.NewServer` serving a chi router with A2A routes mounted; `DiscoverRemoteCard` successfully fetches and decodes the card. |
| `TestTasksSendE2E` | Submit a `tasks/send` JSON-RPC request to an `httptest.Server` with a mocked run backend; verify `result.status.state == "completed"` and a non-empty artifact. |
| `TestTasksSendSubscribeSSEEvents` | Submit `tasks/sendSubscribe`; consume SSE stream via `bufio.Scanner`; assert at least one `TaskStatusUpdateEvent` with state `working` and a final `TaskArtifactUpdateEvent`. |
| `TestTagServeA2AFlag` | Start the full chi router (including existing `/api/runs` huma endpoint) with A2A enabled; assert both `/.well-known/agent-card.json` and `/api/runs` respond correctly without conflict. |
| `TestAgentCallRoundtrip` | `DiscoverRemoteCard(..., DiscoverOptions{Save: true})` followed by `CallRemoteAgent` completes without error against an `httptest.Server`. |
| `TestDiscoverSavesToRegistry` | After discovery with `Save: true`, an in-memory DB `a2a_agent_registry` contains one row with the correct `alias` and `a2a_endpoint`. |
| `TestA2ATasksVisibleInList` | After routing one task, the `a2a_tasks` table row has correct `state` and non-empty `run_id`. |

### 11.3 Performance / Benchmark Tests (`internal/agent/a2acard_bench_test.go`)

| Test | Target |
|------|--------|
| `BenchmarkTasksSendRoutingOverhead` | Mean overhead of 100 `tasks/send` requests (mock run backend, no LLM) ≤50 ms each. |
| `BenchmarkCardGenerationTime` | `GenerateCardFromProfile()` for a profile with 10 tools completes in ≤500 ms. |
| `BenchmarkSSEEventThroughput` | 1000 SSE events written via `http.Flusher` over loopback in ≤1 s (tests write path, not LLM). |

### 11.4 Race Detection

All tests are routinely run with `go test -race`. The goroutine-channel design of `handleTasksSendSubscribe` guarantees no data races; the race detector validates this in CI on every push to `main`.

---

## 12. Acceptance Criteria

| ID | Criterion | Verification |
|----|-----------|-------------|
| AC-01 | `tag agent-card generate --profile coder --url https://example.com` produces a JSON file at `~/.tag/agent-cards/coder.json` that is valid against the bundled A2A v1.0 JSON Schema. | `go test ./internal/agent/ -run TestCardSchemaValidation` |
| AC-02 | `tag serve --a2a --port 8080` responds to `GET http://127.0.0.1:8080/.well-known/agent-card.json` with HTTP 200 and `Content-Type: application/json`. | `curl -s -o /dev/null -w "%{http_code}" http://127.0.0.1:8080/.well-known/agent-card.json` outputs `200` |
| AC-03 | `POST /a2a` with `{"jsonrpc":"2.0","id":1,"method":"tasks/send","params":{"message":{"parts":[{"type":"text","text":"hello"}]}}}` returns a JSON-RPC 2.0 response with `result.status.state == "completed"`. | `go test ./internal/server/ -run TestTasksSendE2E` |
| AC-04 | `POST /a2a` with `method: "tasks/sendSubscribe"` returns `Content-Type: text/event-stream` and at least two SSE `data:` lines: one with `state: "working"` and one with a `TaskArtifactUpdateEvent`. | `go test ./internal/server/ -run TestTasksSendSubscribeSSEEvents` |
| AC-05 | `tag agent-card discover --url http://127.0.0.1:8080` outputs the agent name, A2A endpoint URL, and capabilities without error. | `go test ./internal/server/ -run TestServeAndDiscoverRoundtrip` |
| AC-06 | `tag agent-card generate --sign` produces a card with a `proof` field containing `type: "Ed25519Signature2020"` and a non-empty `proofValue`. | `go test ./internal/agent/ -run TestJCSSigningRoundtrip` |
| AC-07 | A signed card where any JSON field is modified causes `tag agent-card discover --verify-signature` to print `INVALID`. | `go test ./internal/agent/ -run TestJCSTamperDetection` |
| AC-08 | `tag serve --a2a` with `--bearer-token-env A2A_TOKEN` (env var set) rejects unauthenticated `POST /a2a` with HTTP 401. | `go test ./internal/server/ -run TestBearerAuthMissing` |
| AC-09 | `tag agent-card discover --url http://192.168.1.1` prints an error containing "private" and exits with code 1 without making any HTTP connection. | `go test ./internal/agent/ -run TestSSRFPrivateIPRejected` |
| AC-10 | `tag agent-card list` shows a row for the `coder` profile after `tag agent-card generate --profile coder`. | `go test ./internal/agent/ -run TestSQLiteCardPersistence` |
| AC-11 | `tag agent-card validate --path ~/.tag/agent-cards/coder.json` exits 0 for a valid card and exits 1 with error details for a card missing the `url` field. | `go test ./internal/agent/ -run TestCardValidationPass` + `TestCardValidationFail` |
| AC-12 | All existing `tag serve` tests pass without modification when `--a2a` is not specified. | `go test ./... -short` green (excludes integration build tag) |
| AC-13 | `tag agent-card tasks` shows an entry for every inbound task that was routed to a TAG run, with correct `state` and `run_id`. | `go test ./internal/server/ -run TestA2ATasksVisibleInList` |
| AC-14 | `tag agent call <alias> --task "..." --stream` prints status updates in real time and exits 0 on task completion. | `go test ./internal/server/ -run TestAgentCallRoundtrip` |
| AC-15 | The `a2a_cards`, `a2a_tasks`, `a2a_task_history`, and `a2a_agent_registry` tables are created by `store.Open()` (via `CREATE TABLE IF NOT EXISTS`) and do not require a separate migration step. | Fresh in-memory `modernc.org/sqlite` integration test |
| AC-16 | `POST /a2a` with unknown method returns `{"error": {"code": -32601, "message": "Method not found: tasks/unknown"}}`. | `go test ./internal/server/ -run TestMethodNotFound` |
| AC-17 | `CGO_ENABLED=0 go build ./...` succeeds on Linux, macOS, and Windows — the feature has zero CGO dependencies. | GoReleaser matrix CI gate |

---

## 13. Dependencies

There is no official Go A2A SDK; the Agent Card is plain JSON over `net/http`, which requires no external A2A library. All signing is done with Go stdlib. New `go.mod` entries are minimal.

| Dependency | Type | Version | Justification |
|-----------|------|---------|---------------|
| `go-chi/chi/v5` | Go module | `v5.x` | HTTP router; already used by the existing `internal/server`. A2A routes are mounted on the existing chi router instance — no new dep. |
| `danielgtaylor/huma/v2` | Go module | `v2.x` | Spec-first API layer for existing `/api/*` endpoints. A2A routes bypass huma and use raw `http.Handler` for JSON-RPC 2.0 compliance (huma is OpenAPI-centric, not JSON-RPC). No new dep. |
| `tmaxmax/go-sse` | Go module | latest | SSE writer for `tasks/sendSubscribe`; already used by `internal/server` for the `/api/stream` endpoint. No new dep. |
| `modernc.org/sqlite` | Go module | `v1.x` | Pure-Go SQLite driver (`CGO_ENABLED=0`, FTS5 built-in); already the project's SQLite driver. Four new tables added via `store.Open()` migration. No new dep. |
| `invopop/jsonschema` | Go module | `v0.x` | Derive JSON Schema from `AgentCard` struct tags for offline `validate` sub-command. New dep. |
| `gofrs/flock` | Go module | `v0.x` | File-level locking for single-writer SQLite; already used by `internal/store`. No new dep. |
| `golang-jwt/jwt/v5` | Go module | `v5.x` | JWS signing path if future PRD extends to JWT-wrapped proof sections. Listed as potential dep; not required by this PRD. |
| `crypto/ed25519` | Go stdlib | — | Ed25519 key generation and signing. Zero third-party dep for signing. |
| `encoding/json` | Go stdlib | — | All JSON serialization. No external JSON library needed. |
| `net/http` | Go stdlib | — | Outbound HTTP client for `DiscoverRemoteCard` and `CallRemoteAgent`. |
| `PRD-013` (`internal/runtime`) | Internal | — | OTel spans emitted for every A2A task via existing tracing helpers. |
| `PRD-036` (`internal/server`) | Internal | — | A2A routes are added to the existing chi server; `store.Open()` is shared. |
| `PRD-034` (`internal/runtime`) | Internal | — | Inbound task text passes through secret scanner before being logged or replayed. |
| `PRD-026` (`internal/mcp`) | Internal | Soft | Tool descriptions looked up from MCP registry for `AgentCard.Skills` auto-population; falls back to title-casing if unavailable. |

---

## 14. Open Questions

| # | Question | Owner | Resolution Needed By |
|---|----------|-------|---------------------|
| OQ-1 | Should `tag agent-card serve` and `tag serve --a2a` share the same HTTP listener, or should A2A be served on a separate port to isolate dashboard traffic from A2A task execution? The current design shares one chi router and one `net/http` listener. Because Go's `net/http` is goroutine-per-request, long A2A tasks do not block dashboard routes — the original concern about a blocking server thread does not apply to Go. Sharing a single listener is therefore the preferred default. | Engineering | Before Phase 1 implementation |
| OQ-2 | The A2A spec allows the well-known URL to return a `307 Temporary Redirect` to a different host. Should `tag agent-card discover` follow cross-host redirects? If yes, the SSRF check must apply to the final redirect target, not just the initial URL. | Security | Before Phase 1 implementation |
| OQ-3 | Should `tag agent call` record outbound A2A calls as child spans in the TAG run that initiated the call, or as independent top-level runs? The current design creates an independent run with `source='a2a_remote'`. | Architecture | Phase 2 |
| OQ-4 | `AgentCard.skills` is auto-populated from MCP tool grants. Should skills also be declared manually via a `~/.tag/agent-cards/<profile>.skills.yaml` override file, enabling richer skill descriptions than what can be derived from tool names? | Product | Phase 2 |
| OQ-5 | The A2A spec does not publish a stable standalone JSON Schema artifact. Should `tag agent-card validate` bundle a pinned copy of the schema (embedded via `//go:embed` in `internal/agent`) or generate it at runtime from the `AgentCard` Go struct via `invopop/jsonschema`? The struct-derived approach is self-maintaining; the bundled approach ensures exact spec fidelity even if the struct drifts. | Engineering | Phase 1 |
| OQ-6 | A2A v1.0 defines `tasks/cancel` and `tasks/get` methods in addition to `tasks/send` and `tasks/sendSubscribe`. Should these be implemented in this PRD or deferred? `tasks/get` is useful for polling-based clients; `tasks/cancel` requires cancelling the run goroutine via `context.CancelFunc`. | Engineering | Phase 1 scoping |
| OQ-7 | For multi-agent deployments where multiple TAG instances run on different ports on the same host, should each instance have its own signing key, or should a fleet-level key be supported? | Security | Phase 3 |

---

## 15. Complexity and Timeline

### Phase 1 — Core Agent Card Generation and Serving (Days 1–2)

- Implement `AgentCard`, `A2ASkill`, `A2ACapabilities`, `A2ATaskState`, `A2AProof` Go structs in `internal/agent/a2acard.go` (~150 lines).
- Implement `GenerateCardFromProfile()` with profile YAML loading via `internal/config`, description extraction, skills auto-population, and SQLite persistence to `a2a_cards` via `internal/store` (~100 lines).
- Implement `newGenerateCmd`, `newListCmd`, `newShowCmd`, `newValidateCmd` cobra commands in `internal/cli/agent_card.go`.
- Mount `GET /.well-known/agent-card.json` on the chi router in `internal/server/a2a.go`, behind `A2AEnabled` config flag.
- Add A2A DDL (`CREATE TABLE IF NOT EXISTS`) to `internal/store/a2a_migrations.go`; run inside `store.Open()`.
- Write `TestCardGenerationRequiredFields`, `TestCardSchemaValidation`, `TestWellKnownURLPath`, `TestSQLiteCardPersistence` (~8 unit tests using `httptest` + in-memory sqlite).

**Deliverable:** `tag agent-card generate` and `GET /.well-known/agent-card.json` working end-to-end.

### Phase 2 — A2A Task Reception Endpoint (Days 3–4)

- Implement `handleTasksSend()` in `internal/server/a2a.go` including `createTaskRecord()`, `transitionTask()`, JSON-RPC 2.0 response helpers (~120 lines).
- Implement `handleTasksSendSubscribe()` SSE handler with Go channel bridging inference goroutine to writer goroutine (~100 lines).
- Implement `bearerTokenMiddleware()` and `chan struct{}` semaphore for `--max-concurrent-tasks`.
- Implement `newServeCmd` and wire `--a2a` flag into `internal/cli/root.go`.
- Implement `newTasksCmd` cobra command.
- Write integration tests: `TestTasksSendE2E`, `TestTasksSendSubscribeSSEEvents`, `TestBearerAuthMissing`, `TestBearerAuthValid`, `TestMethodNotFound`.
- Write security tests: `TestSSRFPrivateIPRejected`, `TestMaxConcurrentTasks`.
- Run all tests with `go test -race`.

**Deliverable:** Full inbound A2A task reception, SSE streaming, and auth enforcement.

### Phase 3 — Discovery Client, Outbound Calls, and Signing (Day 5)

- Implement `DiscoverRemoteCard()` with three-path resolution and SSRF protection via `net.LookupHost` in `internal/agent/a2adiscovery.go` (~80 lines).
- Implement `newDiscoverCmd` with `--save` (upserts into `a2a_agent_registry`) and `--verify-signature`.
- Implement `CallRemoteAgent()` in `internal/agent/a2acall.go` for `tag agent call`, including synchronous and goroutine-based SSE streaming modes (~100 lines).
- Implement `SignAgentCard()`, `VerifyAgentCardSignature()`, `GenerateSigningKey()`, and `jcsCanonical()` in `internal/agent/a2asign.go` using `crypto/ed25519` (~80 lines).
- No `go.mod` extras required — `invopop/jsonschema` is the only new dependency.
- Write integration tests: `TestServeAndDiscoverRoundtrip`, `TestAgentCallRoundtrip`, `TestDiscoverSavesToRegistry`.
- Write signing tests: `TestJCSSigningRoundtrip`, `TestJCSTamperDetection`, `TestJCSKeySort`, `TestSigningKeyPermissions`.

**Deliverable:** Full bidirectional A2A interoperability — TAG can be discovered by remote orchestrators AND TAG can discover and call remote A2A agents.

### Total Estimated Effort

**3–5 days** for a single engineer, with a target of approximately 700 lines of new Go across `internal/agent` (a2acard.go, a2asign.go, a2adiscovery.go, a2acall.go), `internal/server/a2a.go`, `internal/cli/agent_card.go`, and `internal/store/a2a_migrations.go`, plus 35 test functions. The implementation is classified **Difficulty 2/5** because it builds on existing infrastructure (`internal/server` chi router, `store.Open()`, `internal/runtime` tracing, cobra CLI dispatch) and introduces no new architectural patterns: the A2A endpoint is a new route on the existing chi server, the task router calls existing run infrastructure, and the discovery client uses stdlib `net/http`.

The **Impact 4/5** rating reflects that this single feature makes TAG visible to the entire A2A ecosystem (150+ platforms) with no required changes to existing profiles, enabling a new class of multi-agent workflows that TAG could not participate in before.

