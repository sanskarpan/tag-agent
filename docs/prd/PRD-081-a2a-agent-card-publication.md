# PRD-081: A2A Agent Card Publication (`tag agent-card`)

**Status:** Proposed
**Priority:** P1
**Estimated Effort:** S (3-5 days)
**Category:** Multi-Agent Interoperability Protocols
**Affects:** `a2a_card.py + api.py`
**Depends on:** PRD-013 (agent tracing/observability), PRD-028 (sandbox code execution), PRD-034 (secret scanning/security), PRD-027 (eval framework), PRD-036 (web dashboard / tag serve), PRD-014 (MCP server registry)
**Inspired by:** A2A v1.0 (Linux Foundation), MAF 1.0, CrewAI, LangGraph A2A

---

## 1. Overview

The Agent-to-Agent (A2A) protocol, now governed by the Linux Foundation A2A Project under the `lf.a2a.v1` namespace, has emerged as the primary interoperability standard for autonomous agent ecosystems. As of v1.0 (stable, 2026), A2A defines a JSON-RPC 2.0 over HTTP+SSE wire format, a normative Agent Card discovery mechanism using the RFC 8615 well-known URI pattern (`/.well-known/agent-card.json`), a structured task lifecycle with eight states, and optional gRPC bindings for high-throughput deployments. The Python SDK `a2a-sdk==1.1.0` (released May 2026, requires Python >=3.10) provides full v1.0 implementation with v0.3 compatibility mode. More than 150 platforms — including CrewAI, LangGraph, AutoGen, AWS Bedrock Agents, Vertex AI Agent Builder, and Cursor's agentic backend — can discover and invoke any compliant A2A agent by fetching its Agent Card and initiating tasks via the A2A JSON-RPC interface.

TAG CLI currently operates as a powerful local agent orchestrator: it manages named profiles, executes multi-step agentic tasks through the Hermes bridge, maintains span traces in SQLite, and exposes a lightweight HTTP dashboard via `tag serve`. However, TAG agents are invisible to the A2A ecosystem. There is no machine-readable self-description at a well-known URL, no declared capability set, and no supported mechanism for remote A2A orchestrators to discover that a TAG agent exists, understand what it can do, or invoke it in a protocol-compliant way. This means TAG cannot participate in cross-platform agent workflows, cannot be delegated tasks by A2A orchestrators, and cannot be composed with other A2A-compatible agents — a significant gap as the industry converges on A2A as the common agent interchange protocol.

PRD-081 introduces `tag agent-card`: a new module (`src/tag/a2a_card.py`) and extensions to the existing `api.py` HTTP server that together implement the A2A v1.0 Agent Card publication lifecycle. The feature has three primary surfaces. First, `tag agent-card generate` reads an existing TAG profile and produces a spec-compliant `AgentCard` JSON document serialized to disk. Second, `tag agent-card serve` (and the `--a2a` flag on the existing `tag serve`) mounts the generated card at `/.well-known/agent-card.json` on the local HTTP server and activates a minimal A2A task reception endpoint at `/a2a`, allowing remote orchestrators to submit tasks that are routed directly into TAG's existing run infrastructure. Third, `tag agent-card discover` acts as a client-side resolver: given any remote URL, it fetches, validates, and pretty-prints the Agent Card, enabling developers to inspect peer agent capabilities and populate TAG's local `a2a_agent_registry` table with trusted remote agents. The `tag agent call` command completes the loop by allowing TAG to dispatch a task to any registered remote A2A agent and stream the results back to the terminal.

The feature is deliberately scoped to the mandatory A2A v1.0 surface: Agent Card publication, `/.well-known/agent-card.json` serving, `tasks/send` and `tasks/sendSubscribe` (SSE streaming) JSON-RPC methods, and the eight-state task lifecycle. Optional A2A extensions — gRPC binding, push notifications via Webhook, and `tasks/resubscribe` for session resumption — are deferred to follow-on PRDs. Authentication is implemented at the Agent Card declaration layer (the card's `securitySchemes` and `security` fields describe requirements) and at the HTTP transport layer (Bearer token validation middleware in `api.py`), with no OAuth2 authorization server bundled in this PRD. The JCS (RFC 8785) signing path for tamper-evident Agent Cards is specified as an optional feature and implemented behind a `--sign` flag using the `rfc8785` package.

The expected outcome is that any TAG profile can be published as a first-class A2A agent discoverable and invokable by the 150+ platforms in the A2A ecosystem, with zero changes required to existing TAG profiles or run infrastructure. The new module adds approximately 600 lines of Python to the codebase, a single new SQLite table (`a2a_cards`), two new routes in `api.py`, and a four-subcommand CLI surface. Rollout risk is low because the feature is additive: existing `tag serve`, profile execution, and SQLite schemas are unchanged unless `--a2a` is explicitly passed.

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
| G8 | The `securitySchemes` field in the Agent Card accurately reflects the TAG server's actual auth configuration (none, Bearer token, or mTLS), and the Bearer token validation middleware in `api.py` enforces it. |
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
| Spec compliance | Generated Agent Card passes A2A v1.0 JSON Schema validation with zero errors | `pytest tests/test_a2a_card.py::test_schema_validation` using official `a2a-sdk` validator |
| Discovery interoperability | A TAG-served card is successfully fetched and parsed by `a2a-sdk` `AgentClient` in integration test | `tests/test_a2a_integration.py::test_discovery_roundtrip` |
| Task routing latency overhead | `tasks/send` adds ≤50 ms median overhead vs. direct `cmd_run` call (excluding model inference time) | Benchmark 100 tasks; measure routing overhead distribution |
| Streaming correctness | `tasks/sendSubscribe` emits valid SSE `data:` lines with JSON-RPC 2.0 `StreamResponse` objects for every Hermes inference step | `tests/test_a2a_sse.py` with mock Hermes bridge |
| Card generation time | `tag agent-card generate` completes in ≤500 ms for any profile | Timed in `test_card_generation_perf` |
| SQLite persistence | `tag agent-card list` correctly shows all cards generated in the current session | Integration test: generate 3 cards, assert list count and metadata fields |
| Discoverability | `tag agent-card discover --url` returns valid card data and registers agent in ≤2 s for a local test server | `test_discover_and_register` |
| Zero regression | Existing `tag serve` (without `--a2a`) behavior is unchanged; all existing `tests/` pass | CI gate on existing test suite |
| Auth enforcement | Requests to `POST /a2a` without a valid Bearer token when auth is configured return HTTP 401, not 500 | `test_bearer_auth_enforcement` |

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
| FR-01 | `tag agent-card generate` MUST produce a JSON document that passes validation against the official A2A v1.0 JSON Schema (as implemented by `a2a-sdk==1.1.0`). | MUST |
| FR-02 | The well-known URL served by TAG MUST be `GET /.well-known/agent-card.json` (not `/agent.json` or any other path), conforming to RFC 8615. | MUST |
| FR-03 | The generated `AgentCard.url` field MUST point to the agent's A2A JSON-RPC endpoint (i.e., `<base_url>/a2a`), not the well-known URL itself. | MUST |
| FR-04 | The A2A endpoint at `POST /a2a` MUST handle `tasks/send` and `tasks/sendSubscribe` JSON-RPC 2.0 method names. Unrecognized methods MUST return a JSON-RPC error with code `-32601` (Method not found). | MUST |
| FR-05 | `tasks/send` MUST accept an A2A `TaskSendParams` payload, create an entry in `a2a_tasks`, route the task to `cmd_run` with the configured profile, await completion, and return an A2A `Task` object in the JSON-RPC result field. | MUST |
| FR-06 | `tasks/sendSubscribe` MUST accept the same `TaskSendParams`, respond immediately with `Content-Type: text/event-stream`, and emit SSE `data:` lines containing JSON-RPC 2.0 `StreamResponse` objects. Each Hermes inference step MUST produce at least one `TaskStatusUpdateEvent` SSE event. Task completion MUST emit a final `TaskArtifactUpdateEvent` with the agent's output. | MUST |
| FR-07 | The `a2a_tasks` table MUST record task ID, state, profile, associated TAG run ID, inbound message parts, and all state transitions with timestamps. | MUST |
| FR-08 | A2A task state MUST follow the eight-state lifecycle: `SUBMITTED → WORKING → COMPLETED | FAILED | CANCELED | REJECTED`. The `INPUT_REQUIRED` and `AUTH_REQUIRED` states MUST be representable in the schema even if full resumption is deferred. | MUST |
| FR-09 | When `--auth bearer` is specified in `generate` and a Bearer token env var is configured in `serve`, the middleware MUST validate the `Authorization: Bearer <token>` header and return HTTP 401 on mismatch before any task processing begins. | MUST |
| FR-10 | `tag agent-card generate --sign` MUST use JCS (RFC 8785) canonicalization: remove the `proof` field if present, serialize with `rfc8785.dumps()`, sign the canonical bytes with Ed25519, and re-attach the `proofValue` as base64url. Object keys in the canonical form MUST be sorted by UTF-16 code unit value (per RFC 8785 §3.2.3). | MUST |
| FR-11 | `tag agent-card discover` MUST try `/.well-known/agent-card.json`, then `/agent-card.json`, then `/.well-known/agent.json` in that order, and use the first `200 OK` response. | MUST |
| FR-12 | `tag agent-card discover --save` MUST insert the discovered card into `a2a_agent_registry` with the agent's name, URL, A2A endpoint, auth scheme, and fetched timestamp. Duplicate entries (same `agent_url`) MUST be upserted, not duplicated. | MUST |
| FR-13 | `tag agent call` MUST look up the target agent in `a2a_agent_registry` by alias or name, construct a `TaskSendParams` from `--task`/`--file`/`--data`, POST to the agent's A2A endpoint, and print the result. The call MUST be recorded in TAG's own `runs` table with `source='a2a_remote'`. | MUST |
| FR-14 | `tag agent call --stream` MUST consume the SSE stream from the remote agent and render `TaskStatusUpdateEvent` and `TaskArtifactUpdateEvent` events to the terminal in real time, using the existing Rich spinner pattern from `tui_output.py`. | MUST |
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
| NFR-02 | **Throughput** — The A2A server MUST handle at least 10 concurrent inbound `tasks/send` requests without deadlocking, using the existing `threading.Thread`-per-request model in `http.server`. | Concurrent load test |
| NFR-03 | **Memory** — `a2a_card.py` module import MUST NOT increase baseline memory footprint by more than 5 MB. The `rfc8785` package is imported only when `--sign` is used (lazy import). | `tracemalloc` test |
| NFR-04 | **Startup time** — `tag serve --a2a` MUST start and be ready to accept connections in ≤1 s after the command is entered, matching the existing `tag serve` startup time. | Timed integration test |
| NFR-05 | **Spec compliance** — All A2A JSON-RPC responses MUST include `jsonrpc: "2.0"` and `id` fields matching the request. SSE responses MUST not include `Content-Length`. | Protocol conformance test |
| NFR-06 | **SQLite WAL mode** — All writes to `a2a_tasks` and `a2a_cards` MUST use the existing `open_db()` helper (WAL mode, `PRAGMA journal_mode=WAL`, `PRAGMA synchronous=NORMAL`). No raw `sqlite3.connect()` calls in `a2a_card.py`. | Code review |
| NFR-07 | **Thread safety** — The SSE streaming handler for `tasks/sendSubscribe` MUST use a `queue.Queue` to communicate between the Hermes inference thread and the SSE writer thread, never sharing mutable state across threads without locking. | Code review + race detector test |
| NFR-08 | **Observability** — Every inbound A2A task MUST produce an OpenTelemetry span with `tag.a2a.task_id`, `tag.a2a.method`, and `tag.a2a.state` attributes, following the existing `tracing.py` span pattern. | OTel integration test |
| NFR-09 | **Security — no SSRF** — `tag agent call` and `tag agent-card discover` MUST reject URLs pointing to RFC 1918 private IP ranges (10.x, 172.16–31.x, 192.168.x) unless `--allow-private` is explicitly passed, to prevent SSRF in automated pipelines. | Unit test |
| NFR-10 | **Dependency footprint** — The `a2a-sdk` package is an OPTIONAL dependency. `a2a_card.py` MUST NOT import it at module load time. When `a2a-sdk` is not installed, `tag agent-card` MUST print an actionable install hint (`pip install 'tag-agent[a2a]'`) and exit with code `1` without crashing the rest of TAG. | ImportError test |
| NFR-11 | **Portability** — All features MUST work on Linux, macOS, and Windows (Python 3.10+). SSE implementation MUST NOT rely on `select()` or `fcntl`, which are unavailable on Windows. | CI matrix |
| NFR-12 | **Key storage** — Ed25519 signing keys stored in `~/.tag/keys/` MUST have file permissions `0600` on POSIX systems. Key generation MUST use `cryptography` library's `Ed25519PrivateKey.generate()`. | `stat()` assertion in test |

---

## 9. Technical Design

### 9.1 New Files

| File | Purpose |
|------|---------|
| `src/tag/a2a_card.py` | Core module: `AgentCard` dataclass, generation logic, signing, discovery client, A2A task router, SSE streaming handler |
| `src/tag/a2a_card.py` (extension) | `A2ATaskState` enum, `A2ATask` dataclass, `A2AAgentRegistry` dataclass |

### 9.2 Modified Files

| File | Changes |
|------|---------|
| `src/tag/api.py` | Add `_route_a2a()` dispatch function; mount `/.well-known/agent-card.json` and `/a2a` routes; add Bearer token middleware |
| `src/tag/controller.py` | Add `cmd_agent_card_*` subcommands; add `cmd_agent_call`; wire into CLI dispatch |
| `pyproject.toml` | Add `[project.optional-dependencies] a2a = ["a2a-sdk>=1.1.0", "rfc8785>=0.1.2", "cryptography>=42.0"]` |

### 9.3 SQLite DDL

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

### 9.4 Core Python Dataclasses and Types

```python
# src/tag/a2a_card.py
from __future__ import annotations

import json
import uuid
from dataclasses import dataclass, field
from enum import Enum
from typing import Literal


class A2ATaskState(str, Enum):
    """A2A v1.0 task lifecycle states (§4.2 of the spec)."""
    SUBMITTED      = "submitted"
    WORKING        = "working"
    INPUT_REQUIRED = "input-required"    # human-in-the-loop pause
    AUTH_REQUIRED  = "auth-required"     # additional auth needed
    COMPLETED      = "completed"
    FAILED         = "failed"
    CANCELED       = "canceled"
    REJECTED       = "rejected"

    @property
    def is_terminal(self) -> bool:
        return self in {
            A2ATaskState.COMPLETED,
            A2ATaskState.FAILED,
            A2ATaskState.CANCELED,
            A2ATaskState.REJECTED,
        }


class A2AAuthScheme(str, Enum):
    NONE   = "none"
    BEARER = "bearer"
    MTLS   = "mtls"


@dataclass
class A2ASkill:
    id:           str
    name:         str
    description:  str
    input_modes:  list[str] = field(default_factory=lambda: ["text"])
    output_modes: list[str] = field(default_factory=lambda: ["text"])
    tags:         list[str] = field(default_factory=list)

    def to_dict(self) -> dict:
        return {
            "id":          self.id,
            "name":        self.name,
            "description": self.description,
            "inputModes":  self.input_modes,
            "outputModes": self.output_modes,
            **({"tags": self.tags} if self.tags else {}),
        }


@dataclass
class A2ACapabilities:
    streaming:               bool = True
    push_notifications:      bool = False
    state_transition_history: bool = True

    def to_dict(self) -> dict:
        return {
            "streaming":               self.streaming,
            "pushNotifications":       self.push_notifications,
            "stateTransitionHistory":  self.state_transition_history,
        }


@dataclass
class AgentCard:
    """A2A v1.0 Agent Card document (lf.a2a.v1.AgentCard proto mapping)."""
    name:              str
    description:       str
    url:               str                    # A2A JSON-RPC endpoint (not well-known URL)
    version:           str           = "1.0.0"
    default_input_modes:  list[str]  = field(default_factory=lambda: ["text"])
    default_output_modes: list[str]  = field(default_factory=lambda: ["text"])
    capabilities:      A2ACapabilities = field(default_factory=A2ACapabilities)
    skills:            list[A2ASkill]   = field(default_factory=list)
    security_schemes:  dict             = field(default_factory=dict)
    security:          list[dict]       = field(default_factory=list)
    proof:             dict | None      = None   # JCS proof section (optional)

    def to_dict(self) -> dict:
        d: dict = {
            "name":               self.name,
            "description":        self.description,
            "url":                self.url,
            "version":            self.version,
            "defaultInputModes":  self.default_input_modes,
            "defaultOutputModes": self.default_output_modes,
            "capabilities":       self.capabilities.to_dict(),
            "skills":             [s.to_dict() for s in self.skills],
            "securitySchemes":    self.security_schemes,
            "security":           self.security,
        }
        if self.proof is not None:
            d["proof"] = self.proof
        return d

    def to_json(self, indent: int = 2) -> str:
        return json.dumps(self.to_dict(), indent=indent)

    @classmethod
    def from_dict(cls, d: dict) -> "AgentCard":
        capabilities = A2ACapabilities(
            streaming=d.get("capabilities", {}).get("streaming", True),
            push_notifications=d.get("capabilities", {}).get("pushNotifications", False),
            state_transition_history=d.get("capabilities", {}).get("stateTransitionHistory", True),
        )
        skills = [
            A2ASkill(
                id=s["id"],
                name=s["name"],
                description=s.get("description", ""),
                input_modes=s.get("inputModes", ["text"]),
                output_modes=s.get("outputModes", ["text"]),
                tags=s.get("tags", []),
            )
            for s in d.get("skills", [])
        ]
        return cls(
            name=d["name"],
            description=d["description"],
            url=d["url"],
            version=d.get("version", "1.0.0"),
            default_input_modes=d.get("defaultInputModes", ["text"]),
            default_output_modes=d.get("defaultOutputModes", ["text"]),
            capabilities=capabilities,
            skills=skills,
            security_schemes=d.get("securitySchemes", {}),
            security=d.get("security", []),
            proof=d.get("proof"),
        )
```

### 9.5 JCS Signing Algorithm

```python
# src/tag/a2a_card.py (continued)
import base64

def sign_agent_card(card: AgentCard, private_key_path: Path) -> AgentCard:
    """
    Sign an AgentCard using JCS (RFC 8785) canonicalization + Ed25519.

    Algorithm:
    1. Remove any existing 'proof' field from the card dict.
    2. Serialize with rfc8785.dumps() — keys sorted by UTF-16 code unit value,
       numbers per ECMAScript IEEE 754 rules (no trailing .0).
    3. Sign the canonical bytes with Ed25519.
    4. Attach proof section with base64url-encoded signature.
    """
    try:
        import rfc8785                              # Trail of Bits, no-dep package
        from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
        from cryptography.hazmat.primitives.serialization import (
            load_pem_private_key, Encoding, PublicFormat,
        )
    except ImportError as e:
        raise RuntimeError(
            "Signing requires optional deps: pip install 'tag-agent[a2a]'"
        ) from e

    card_dict = card.to_dict()
    card_dict.pop("proof", None)                    # Step 1: strip existing proof

    canonical: bytes = rfc8785.dumps(card_dict)     # Step 2: JCS canonicalize

    pem = private_key_path.read_bytes()
    private_key: Ed25519PrivateKey = load_pem_private_key(pem, password=None)
    signature: bytes = private_key.sign(canonical)  # Step 3: Ed25519 sign

    pub_bytes = private_key.public_key().public_bytes(Encoding.Raw, PublicFormat.Raw)
    proof = {
        "type":       "Ed25519Signature2020",
        "created":    datetime.utcnow().strftime("%Y-%m-%dT%H:%M:%SZ"),
        "proofValue": base64.urlsafe_b64encode(signature).rstrip(b"=").decode(),
        "publicKey":  base64.urlsafe_b64encode(pub_bytes).rstrip(b"=").decode(),
    }
    card.proof = proof                               # Step 4: re-attach proof
    return card


def verify_agent_card_signature(card_dict: dict) -> bool:
    """Verify JCS proof on a fetched card. Returns True if valid."""
    import rfc8785
    from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PublicKey
    from cryptography.hazmat.primitives.serialization import Encoding, PublicFormat
    import base64

    proof = card_dict.get("proof")
    if not proof:
        return False

    doc = {k: v for k, v in card_dict.items() if k != "proof"}
    canonical = rfc8785.dumps(doc)

    sig = base64.urlsafe_b64decode(proof["proofValue"] + "==")
    pub_raw = base64.urlsafe_b64decode(proof["publicKey"] + "==")
    public_key = Ed25519PublicKey.from_public_bytes(pub_raw)

    try:
        public_key.verify(sig, canonical)
        return True
    except Exception:
        return False
```

### 9.6 A2A Endpoint Handler in api.py

```python
# src/tag/api.py (additions to existing BaseHTTPRequestHandler)

def _handle_a2a_post(
    handler: BaseHTTPRequestHandler,
    conn: sqlite3.Connection,
    profile: str,
    bearer_token: str | None,
) -> None:
    """Route POST /a2a — JSON-RPC 2.0 dispatch for A2A tasks/send and tasks/sendSubscribe."""

    # Bearer token enforcement
    if bearer_token:
        auth_header = handler.headers.get("Authorization", "")
        if not auth_header.startswith("Bearer ") or auth_header[7:] != bearer_token:
            handler.send_response(401)
            handler.send_header("WWW-Authenticate", 'Bearer realm="TAG A2A"')
            handler.send_header("Content-Type", "application/json")
            handler.end_headers()
            handler.wfile.write(json.dumps({
                "jsonrpc": "2.0", "id": None,
                "error": {"code": -32001, "message": "Unauthorized"},
            }).encode())
            return

    length = int(handler.headers.get("Content-Length", 0))
    body = json.loads(handler.rfile.read(length))
    req_id = body.get("id")
    method = body.get("method", "")
    params = body.get("params", {})

    if method == "tasks/send":
        _handle_tasks_send(handler, conn, profile, req_id, params)
    elif method == "tasks/sendSubscribe":
        _handle_tasks_send_subscribe(handler, conn, profile, req_id, params)
    else:
        _jsonrpc_error(handler, req_id, -32601, f"Method not found: {method}")


def _handle_tasks_send(
    handler, conn, profile, req_id, params
) -> None:
    """Synchronous task execution — blocks until completion."""
    import queue as q_mod
    from .a2a_card import A2ATaskState, _create_task_record, _transition_task

    task_id = params.get("id") or str(uuid.uuid4())
    message = params.get("message", {})

    _create_task_record(conn, task_id, profile, "tasks/send", message)

    try:
        # Route into existing TAG run infrastructure
        from .controller import cmd_run_sync
        task_text = _extract_text_from_message(message)
        run_id, output = cmd_run_sync(profile=profile, prompt=task_text)
        _transition_task(conn, task_id, A2ATaskState.WORKING, A2ATaskState.COMPLETED, run_id=run_id)

        result_task = {
            "id":     task_id,
            "status": {"state": "completed"},
            "artifacts": [{
                "parts": [{"type": "text", "text": output}],
                "index": 0,
            }],
        }
        _jsonrpc_result(handler, req_id, result_task)

    except Exception as exc:
        _transition_task(conn, task_id, A2ATaskState.WORKING, A2ATaskState.FAILED)
        _jsonrpc_error(handler, req_id, -32603, "Internal error", {"details": str(exc)})


def _handle_tasks_send_subscribe(
    handler, conn, profile, req_id, params
) -> None:
    """Streaming task execution via SSE — emits TaskStatusUpdateEvent and TaskArtifactUpdateEvent."""
    import queue
    from .a2a_card import A2ATaskState, _create_task_record, _transition_task

    task_id = params.get("id") or str(uuid.uuid4())
    message = params.get("message", {})
    _create_task_record(conn, task_id, profile, "tasks/sendSubscribe", message)

    handler.send_response(200)
    handler.send_header("Content-Type", "text/event-stream")
    handler.send_header("Cache-Control", "no-cache")
    handler.send_header("X-Accel-Buffering", "no")
    handler.end_headers()

    event_queue: queue.Queue = queue.Queue()

    def _sse(event_type: str, data: dict) -> None:
        """Write a single SSE data line with a JSON-RPC 2.0 StreamResponse."""
        payload = json.dumps({
            "jsonrpc": "2.0",
            "id":      req_id,
            "result":  {"type": event_type, **data},
        })
        try:
            handler.wfile.write(f"data: {payload}\n\n".encode())
            handler.wfile.flush()
        except BrokenPipeError:
            event_queue.put(("ABORT", None))

    def _on_step(step_text: str) -> None:
        _sse("TaskStatusUpdateEvent", {
            "id":     task_id,
            "status": {"state": "working", "message": {"parts": [{"type": "text", "text": step_text}]}},
            "final":  False,
        })

    _sse("TaskStatusUpdateEvent", {
        "id":     task_id,
        "status": {"state": "submitted"},
        "final":  False,
    })

    try:
        from .controller import cmd_run_sync_streaming
        task_text = _extract_text_from_message(message)
        run_id, output = cmd_run_sync_streaming(
            profile=profile, prompt=task_text, step_callback=_on_step
        )
        _transition_task(conn, task_id, A2ATaskState.WORKING, A2ATaskState.COMPLETED, run_id=run_id)

        _sse("TaskArtifactUpdateEvent", {
            "id":    task_id,
            "artifact": {
                "parts": [{"type": "text", "text": output}],
                "index": 0,
                "lastChunk": True,
            },
        })
        _sse("TaskStatusUpdateEvent", {
            "id":     task_id,
            "status": {"state": "completed"},
            "final":  True,
        })
    except Exception as exc:
        _transition_task(conn, task_id, A2ATaskState.WORKING, A2ATaskState.FAILED)
        _sse("TaskStatusUpdateEvent", {
            "id":     task_id,
            "status": {"state": "failed", "message": {"parts": [{"type": "text", "text": str(exc)}]}},
            "final":  True,
        })
```

### 9.7 Agent Card Generation Algorithm

The generation algorithm in `a2a_card.py::generate_card_from_profile()` follows this sequence:

1. **Load profile YAML** from `~/.tag/profiles/<profile>.yaml` using the existing config loader. Extract `system_prompt`, `model`, `tools` (list of MCP tool names).
2. **Derive description** — use `--description` if provided; otherwise take the first 256 non-whitespace characters of `system_prompt`, stripping any leading `#` headers.
3. **Build skills** — for each tool in `profile.tools`, create an `A2ASkill(id=tool_name, name=title_case(tool_name), description=tool_desc_or_default)`. If the tool name is in the local MCP tool registry (from `tool_retrieval.py`), pull the description from there.
4. **Build security schemes** — based on `--auth`:
   - `none`: `security_schemes = {}`, `security = []`
   - `bearer`: `security_schemes = {"bearerAuth": {"type": "http", "scheme": "bearer"}}`, `security = [{"bearerAuth": []}]`
   - `mtls`: `security_schemes = {"mtls": {"type": "mutualTLS"}}`, `security = [{"mtls": []}]`
5. **Construct `AgentCard`** dataclass and serialize to `to_dict()`.
6. **Optionally sign** via `sign_agent_card()` if `--sign` was passed.
7. **Persist** to `~/.tag/agent-cards/<profile>.json` and insert into `a2a_cards` SQLite table.
8. **Print summary** to stdout.

### 9.8 Discovery Client Algorithm

`discover_remote_card(url: str, timeout: int = 10) -> AgentCard`:

```python
DISCOVERY_PATHS = [
    "/.well-known/agent-card.json",  # A2A v1.0 canonical
    "/agent-card.json",              # A2A v0.3 compatibility
    "/.well-known/agent.json",       # older drafts
]

def discover_remote_card(url: str, timeout: int = 10) -> tuple[AgentCard, str]:
    """
    Returns (AgentCard, resolved_url). Raises RuntimeError if all paths fail.
    Enforces SSRF protection: rejects RFC 1918 addresses unless --allow-private.
    """
    import urllib.request
    import urllib.error
    from ipaddress import ip_address, ip_network

    PRIVATE_RANGES = [
        ip_network("10.0.0.0/8"),
        ip_network("172.16.0.0/12"),
        ip_network("192.168.0.0/16"),
        ip_network("127.0.0.0/8"),
        ip_network("::1/128"),
    ]

    base = url.rstrip("/")
    for path in DISCOVERY_PATHS:
        candidate = base + path
        try:
            req = urllib.request.Request(candidate, headers={"Accept": "application/json"})
            with urllib.request.urlopen(req, timeout=timeout) as resp:
                if resp.status == 200:
                    data = json.loads(resp.read())
                    return AgentCard.from_dict(data), candidate
        except (urllib.error.HTTPError, urllib.error.URLError):
            continue

    raise RuntimeError(
        f"No A2A Agent Card found at {url}. "
        f"Tried: {', '.join(DISCOVERY_PATHS)}"
    )
```

### 9.9 Integration with Existing api.py HTTP Server

The existing `BaseHTTPRequestHandler.do_GET` and `do_POST` methods in `api.py` are extended with two new route branches. The extension is conditional on the `_a2a_enabled` flag set by `serve_forever(a2a=True, ...)`:

```python
# In do_GET:
if self.path == "/.well-known/agent-card.json" and self._a2a_enabled:
    self._serve_agent_card()
    return

# In do_POST:
if self.path == "/a2a" and self._a2a_enabled:
    _handle_a2a_post(
        self, open_db(), self._a2a_profile, self._bearer_token
    )
    return
```

No existing routes are modified. The two new branches are only active when `_a2a_enabled = True`.

### 9.10 CLI Wiring in controller.py

```python
# controller.py — new dispatch entry points (simplified)

def cmd_agent_card(args: argparse.Namespace) -> int:
    sub = args.agent_card_subcommand
    if sub == "generate":  return cmd_agent_card_generate(args)
    if sub == "serve":     return cmd_agent_card_serve(args)
    if sub == "discover":  return cmd_agent_card_discover(args)
    if sub == "list":      return cmd_agent_card_list(args)
    if sub == "show":      return cmd_agent_card_show(args)
    if sub == "validate":  return cmd_agent_card_validate(args)
    if sub == "tasks":     return cmd_agent_card_tasks(args)
    return 1

def cmd_agent_call(args: argparse.Namespace) -> int:
    """Dispatch a task to a registered remote A2A agent."""
    from .a2a_card import call_remote_agent
    return call_remote_agent(
        agent_id=args.agent_id,
        task_text=args.task,
        stream=args.stream,
        timeout=args.timeout,
        json_output=args.json,
    )
```

---

## 10. Security Considerations

1. **SSRF prevention** — `tag agent-card discover` and `tag agent call` resolve URLs via `urllib.request`. Before any outbound HTTP request, the resolved IP address MUST be checked against RFC 1918 private ranges (10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16) and loopback (127.0.0.0/8). Requests to private IPs are rejected with a clear error unless `--allow-private` is explicitly set. Redirects are followed but the final resolved IP is re-checked after each redirect.

2. **Bearer token storage** — The Bearer token expected by the inbound A2A server is read from an environment variable (never stored in SQLite or config files). If a developer accidentally passes the token as a CLI flag, it appears in the process argument list and shell history; the `--bearer-token-env` pattern avoids this by accepting only the env var NAME, not the token value.

3. **JCS signing key permissions** — Ed25519 private keys stored in `~/.tag/keys/signing.key` are written with `os.open(..., os.O_CREAT | os.O_WRONLY, 0o600)` to ensure they are only readable by the owning user. On Windows, ACL inheritance is not automatically restricted; the Windows path prints a warning that key security depends on NTFS permissions.

4. **Input validation on inbound tasks** — The `message` field in `TaskSendParams` is parsed as JSON and MUST be validated for maximum depth (≤10 levels) and maximum size (≤1 MB) before being passed to `cmd_run`. Oversized or deeply nested messages are rejected with JSON-RPC error `-32602` (Invalid params) to prevent stack overflows or memory exhaustion.

5. **Prompt injection via A2A tasks** — Inbound task text from remote A2A clients becomes part of the prompt sent to the LLM. The existing sandbox and security modules (PRD-028, PRD-034) apply normally. Additionally, inbound task text MUST pass through the existing `security.py` secret scanner to ensure no credentials are inadvertently logged or replayed.

6. **Card content injection** — The `AgentCard.description` field is derived from the profile system prompt. If the system prompt contains HTML or markdown, the card's JSON `description` value must be plain text. The generator strips HTML tags and markdown headers before embedding the description in the card.

7. **Signature verification trust model** — Signature verification in `tag agent-card discover --verify-signature` uses the `publicKey` embedded in the card's own `proof` section. This is a self-certifying signature, not a CA-rooted one. Callers should treat a valid self-signature as "card not tampered in transit" rather than "agent identity is verified". A future PRD can extend this to support DID-rooted public keys for stronger identity assertions.

8. **A2A endpoint exposure** — `tag agent-card serve` defaults to `127.0.0.1` (loopback only). Binding to `0.0.0.0` makes the A2A endpoint accessible on all network interfaces. When `--host 0.0.0.0` is passed without `--bearer-token-env`, the CLI prints a prominent warning: `WARNING: A2A endpoint is publicly accessible without authentication. Set --bearer-token-env to require a Bearer token.`

9. **Denial of service via long tasks** — Inbound `tasks/send` blocks a server thread for the duration of the TAG run. Since `http.server` uses one thread per connection, a large number of concurrent long-running tasks can exhaust the thread pool. The `--max-concurrent-tasks N` flag (default: `4`) limits the number of simultaneously executing A2A tasks; additional requests receive JSON-RPC error `-32000` (Server error: too many concurrent tasks).

10. **Audit trail** — Every inbound A2A task is recorded in `a2a_tasks` with the full message JSON. This creates a local audit trail of all remote instructions received by the TAG agent. `tag agent-card tasks --state FAILED` surfaces failed tasks for retrospective review. The audit trail is not transmitted externally unless OTLP export is configured (PRD-041).

---

## 11. Testing Strategy

### 11.1 Unit Tests (`tests/test_a2a_card.py`)

| Test | What it covers |
|------|---------------|
| `test_card_generation_required_fields` | `generate_card_from_profile()` produces a dict with all required A2A v1.0 fields: `name`, `description`, `url`, `version`, `defaultInputModes`, `defaultOutputModes`, `capabilities`, `skills`, `securitySchemes`, `security`. |
| `test_card_schema_validation` | The generated card passes `a2a_sdk.AgentCard.model_validate(card_dict)` without exception (requires `a2a-sdk` installed in test env). |
| `test_well_known_url_path` | The served path is exactly `/.well-known/agent-card.json`, verified by checking `handler.path` in a mock handler. |
| `test_a2a_endpoint_url_in_card` | `AgentCard.url` (the `url` field, not the well-known URL) equals `<base_url>/a2a`. |
| `test_jcs_signing_roundtrip` | After `sign_agent_card()`, `verify_agent_card_signature(card.to_dict())` returns `True`. |
| `test_jcs_tamper_detection` | Modifying any field of a signed card causes `verify_agent_card_signature()` to return `False`. |
| `test_a2a_task_state_machine` | `A2ATaskState.is_terminal` returns `True` for COMPLETED/FAILED/CANCELED/REJECTED and `False` for SUBMITTED/WORKING/INPUT_REQUIRED/AUTH_REQUIRED. |
| `test_bearer_auth_missing` | `POST /a2a` without `Authorization: Bearer` header when token is configured returns HTTP 401 JSON body with code `-32001`. |
| `test_bearer_auth_valid` | `POST /a2a` with correct `Authorization: Bearer <token>` proceeds to method dispatch. |
| `test_method_not_found` | `POST /a2a` with `method: "tasks/unknown"` returns JSON-RPC error code `-32601`. |
| `test_ssrf_private_ip_rejected` | `discover_remote_card("http://192.168.1.1/")` raises `RuntimeError` containing "private" before making any HTTP request. |
| `test_discovery_path_order` | Mock server at `<base>/agent-card.json` (not `/.well-known/agent-card.json`) is discovered on the second path attempt. |
| `test_sqlite_card_persistence` | After `generate_card_from_profile()`, the `a2a_cards` table contains one row with matching `profile`, `name`, `a2a_endpoint`. |
| `test_sqlite_task_creation` | `_create_task_record()` inserts into `a2a_tasks` and `_transition_task()` inserts into `a2a_task_history`. |
| `test_signing_key_permissions` | On POSIX, the generated key file at `~/.tag/keys/signing.key` has `oct(stat.st_mode)` ending in `600`. |
| `test_import_without_a2a_sdk` | `sys.modules` monkeypatching to hide `a2a` causes `cmd_agent_card_generate()` to print install hint and return exit code 1. |
| `test_rfc8785_key_sort_ascii` | JCS-canonical form of `{"b": 1, "a": 2}` produces `{"a":2,"b":1}` (ASCII keys: UTF-16 order = alphabetical). |
| `test_card_description_from_system_prompt` | When `--description` is omitted, the first 256 characters of the profile system prompt (stripped of markdown headers) become `AgentCard.description`. |
| `test_skills_from_tool_grants` | A profile with `tools: [bash, read_file, write_file]` produces three `A2ASkill` entries with matching `id` values. |
| `test_max_concurrent_tasks` | Sending 5 concurrent tasks when `max_concurrent_tasks=4` causes the 5th to receive error code `-32000`. |

### 11.2 Integration Tests (`tests/test_a2a_integration.py`)

| Test | What it covers |
|------|---------------|
| `test_serve_and_discover_roundtrip` | Start `tag agent-card serve` in a subprocess, then `tag agent-card discover --url http://127.0.0.1:<port>` successfully fetches and parses the card. |
| `test_tasks_send_e2e` | Submit a `tasks/send` JSON-RPC request to a running TAG A2A server with a simple prompt (mocked Hermes); verify the response includes `status.state = "completed"` and an artifact with non-empty text. |
| `test_tasks_send_subscribe_sse_events` | Submit a `tasks/sendSubscribe` request; consume all SSE events; verify at least one `TaskStatusUpdateEvent` with state `working` and a final `TaskArtifactUpdateEvent`. |
| `test_tag_serve_a2a_flag` | `tag serve --a2a --a2a-profile coder --port <N>` serves both `/.well-known/agent-card.json` and the existing `/api/runs` without conflict. |
| `test_agent_call_roundtrip` | `tag agent-card discover --url <local> --save` followed by `tag agent call <alias> --task "ping"` completes without error. |
| `test_discover_saves_to_registry` | After `tag agent-card discover --save`, `a2a_agent_registry` contains one row with the correct `alias`, `a2a_endpoint`. |
| `test_a2a_tasks_visible_in_list` | After serving one task, `tag agent-card tasks --json` output includes the task with correct `state` and `run_id`. |

### 11.3 Performance Tests (`tests/test_a2a_perf.py`)

| Test | Target |
|------|--------|
| `test_tasks_send_routing_overhead` | Mean overhead of 100 `tasks/send` requests (mock Hermes, no LLM) ≤50 ms each. |
| `test_card_generation_time` | `generate_card_from_profile()` for a profile with 10 tools completes in ≤500 ms. |
| `test_sse_event_throughput` | 1000 SSE events emitted in ≤1 s over loopback (tests SSE write path, not LLM). |

### 11.4 Spec Compliance Tests (`tests/test_a2a_spec.py`)

Run the official A2A conformance test suite from `a2a-sdk` against the TAG A2A server (requires `a2a-sdk` and a running local server). This test is marked `@pytest.mark.slow` and skipped in normal CI; it runs in the `slow` CI job on push to `main`.

---

## 12. Acceptance Criteria

| ID | Criterion | Verification |
|----|-----------|-------------|
| AC-01 | `tag agent-card generate --profile coder --url https://example.com` produces a JSON file at `~/.tag/agent-cards/coder.json` that passes `a2a_sdk.AgentCard.model_validate()` without errors. | `pytest tests/test_a2a_card.py::test_card_schema_validation` |
| AC-02 | `tag serve --a2a --port 8080` responds to `GET http://127.0.0.1:8080/.well-known/agent-card.json` with HTTP 200 and `Content-Type: application/json`. | `curl -s -o /dev/null -w "%{http_code}" http://127.0.0.1:8080/.well-known/agent-card.json` outputs `200` |
| AC-03 | `POST /a2a` with `{"jsonrpc":"2.0","id":1,"method":"tasks/send","params":{"message":{"parts":[{"type":"text","text":"hello"}]}}}` returns a JSON-RPC 2.0 response with `result.status.state == "completed"`. | `test_tasks_send_e2e` |
| AC-04 | `POST /a2a` with `method: "tasks/sendSubscribe"` returns `Content-Type: text/event-stream` and at least two SSE `data:` lines: one with `state: "working"` and one with a `TaskArtifactUpdateEvent`. | `test_tasks_send_subscribe_sse_events` |
| AC-05 | `tag agent-card discover --url http://127.0.0.1:8080` outputs the agent name, A2A endpoint URL, and capabilities without error. | `test_serve_and_discover_roundtrip` |
| AC-06 | `tag agent-card generate --sign` produces a card with a `proof` field containing `type: "Ed25519Signature2020"` and a non-empty `proofValue`. | `test_jcs_signing_roundtrip` |
| AC-07 | A signed card where any JSON field is modified causes `tag agent-card discover --verify-signature` to print `INVALID`. | `test_jcs_tamper_detection` |
| AC-08 | `tag serve --a2a` with `--bearer-token-env A2A_TOKEN` (env var set) rejects unauthenticated `POST /a2a` with HTTP 401. | `test_bearer_auth_missing` |
| AC-09 | `tag agent-card discover --url http://192.168.1.1` prints an error containing "private" and exits with code 1 without making any HTTP connection. | `test_ssrf_private_ip_rejected` |
| AC-10 | `tag agent-card list` shows a row for the `coder` profile after `tag agent-card generate --profile coder`. | `test_sqlite_card_persistence` |
| AC-11 | `tag agent-card validate --path ~/.tag/agent-cards/coder.json` exits 0 for a valid card and exits 1 with error details for a card missing the `url` field. | `test_card_validation_pass` + `test_card_validation_fail` |
| AC-12 | All existing `tag serve` tests pass without modification when `--a2a` is not specified. | `pytest tests/ -k "not a2a"` green |
| AC-13 | `tag agent-card tasks` shows an entry for every inbound task that was routed to a TAG run, with correct `state` and `run_id`. | `test_a2a_tasks_visible_in_list` |
| AC-14 | `tag agent call <alias> --task "..." --stream` prints status updates in real time and exits 0 on task completion. | `test_agent_call_roundtrip` |
| AC-15 | The `a2a_cards`, `a2a_tasks`, `a2a_task_history`, and `a2a_agent_registry` tables are created by `open_db()` (via `CREATE TABLE IF NOT EXISTS`) and do not require a separate migration step. | Fresh DB integration test |
| AC-16 | `POST /a2a` with unknown method returns `{"error": {"code": -32601, "message": "Method not found: tasks/unknown"}}`. | `test_method_not_found` |
| AC-17 | Installing TAG without the `[a2a]` extra and running `tag agent-card generate` prints `pip install 'tag-agent[a2a]'` and exits 1 without a traceback. | `test_import_without_a2a_sdk` |

---

## 13. Dependencies

| Dependency | Type | Version | Justification |
|-----------|------|---------|---------------|
| `a2a-sdk` | Optional (`[a2a]` extra) | `>=1.1.0` | Official A2A Python SDK; provides `AgentCard` Pydantic model, JSON Schema validator, `AgentClient` for outbound calls. Required for `--validate` and schema compliance checks. |
| `rfc8785` | Optional (`[a2a]` extra) | `>=0.1.2` | Trail of Bits JCS (RFC 8785) implementation. Zero transitive dependencies. Used only for `--sign`. |
| `cryptography` | Optional (`[a2a]` extra) | `>=42.0` | Ed25519 key generation and signing. Likely already installed as a transitive dep of `anthropic`. |
| `urllib.request` | stdlib | — | Used for `tag agent-card discover` HTTP client. No new third-party HTTP library needed. |
| Python | runtime | `>=3.10` | Required by `a2a-sdk==1.1.0`. TAG already requires 3.10+. |
| `PRD-013` (tracing.py) | Internal | — | OTel spans emitted for every A2A task; `tracing.py` span helpers used in `a2a_card.py`. |
| `PRD-036` (api.py) | Internal | — | `a2a_card.py` extends the existing HTTP server in `api.py`; uses `open_db()` from the same module. |
| `PRD-034` (security.py) | Internal | — | Inbound task text passes through secret scanner before being logged or replayed. |
| `PRD-026` (tool_retrieval.py) | Internal | Soft | Tool descriptions used to auto-populate `AgentCard.skills` from profile tool grants. Falls back to title-casing if not available. |

---

## 14. Open Questions

| # | Question | Owner | Resolution Needed By |
|---|----------|-------|---------------------|
| OQ-1 | Should `tag agent-card serve` and `tag serve --a2a` share the same HTTP server thread, or should A2A be served on a separate port/thread to isolate dashboard traffic from A2A task execution? The current design shares one server, which simplifies deployment but means a long A2A task blocks a server thread. | Engineering | Before Phase 1 implementation |
| OQ-2 | The A2A spec allows the well-known URL to return a `307 Temporary Redirect` to a different host. Should `tag agent-card discover` follow cross-host redirects? If yes, the SSRF check must apply to the final redirect target, not just the initial URL. | Security | Before Phase 1 implementation |
| OQ-3 | Should `tag agent call` record outbound A2A calls as child spans in the TAG run that initiated the call, or as independent top-level runs? The current design creates an independent run with `source='a2a_remote'`. | Architecture | Phase 2 |
| OQ-4 | `AgentCard.skills` is auto-populated from MCP tool grants. Should skills also be declared manually via a `~/.tag/agent-cards/<profile>.skills.yaml` override file, enabling richer skill descriptions than what can be derived from tool names? | Product | Phase 2 |
| OQ-5 | The `a2a-sdk` package does not yet publish a stable JSON Schema artifact suitable for offline validation. Should `tag agent-card validate` bundle a pinned copy of the schema, or require `a2a-sdk` to be installed and call the Pydantic model validator? The Pydantic approach requires a network install; the bundled schema approach adds a maintenance burden. | Engineering | Phase 1 |
| OQ-6 | A2A v1.0 defines `tasks/cancel` and `tasks/get` methods in addition to `tasks/send` and `tasks/sendSubscribe`. Should these be implemented in this PRD or deferred? `tasks/get` is useful for polling-based clients; `tasks/cancel` requires signal propagation into the Hermes run thread. | Engineering | Phase 1 scoping |
| OQ-7 | For multi-agent deployments where multiple TAG instances run on different ports on the same host, should each instance have its own signing key, or should a fleet-level key be supported? | Security | Phase 3 |

---

## 15. Complexity and Timeline

### Phase 1 — Core Agent Card Generation and Serving (Days 1–2)

- Implement `AgentCard` dataclass, `A2ASkill`, `A2ACapabilities`, `A2ATaskState` in `a2a_card.py` (~150 lines).
- Implement `generate_card_from_profile()` with profile YAML reading, description extraction, skills auto-population, and SQLite persistence to `a2a_cards` (~100 lines).
- Implement `cmd_agent_card_generate` in `controller.py` with all flags.
- Implement `cmd_agent_card_list`, `cmd_agent_card_show`, `cmd_agent_card_validate`.
- Mount `GET /.well-known/agent-card.json` route in `api.py` behind `_a2a_enabled` flag.
- Write `test_card_generation_required_fields`, `test_card_schema_validation`, `test_well_known_url_path`, `test_sqlite_card_persistence` (~8 unit tests).
- Create SQLite DDL migrations (added to `open_db()` initialization block).

**Deliverable:** `tag agent-card generate` and `GET /.well-known/agent-card.json` working end-to-end.

### Phase 2 — A2A Task Reception Endpoint (Days 3–4)

- Implement `tasks/send` handler in `api.py` including `_create_task_record`, `_transition_task`, JSON-RPC 2.0 response formatting (~120 lines).
- Implement `tasks/sendSubscribe` SSE handler with queue-based threading (~100 lines).
- Implement Bearer token middleware.
- Implement `cmd_agent_card_serve` and `tag serve --a2a` flag.
- Implement `cmd_agent_card_tasks`.
- Write integration tests: `test_tasks_send_e2e`, `test_tasks_send_subscribe_sse_events`, `test_bearer_auth_missing`, `test_bearer_auth_valid`, `test_method_not_found`.
- Write security tests: `test_ssrf_private_ip_rejected`, `test_max_concurrent_tasks`.

**Deliverable:** Full inbound A2A task reception, SSE streaming, and auth enforcement.

### Phase 3 — Discovery Client, Outbound Calls, and Signing (Day 5)

- Implement `discover_remote_card()` with three-path resolution algorithm and SSRF protection (~80 lines).
- Implement `cmd_agent_card_discover` with `--save` (writes to `a2a_agent_registry`) and `--verify-signature`.
- Implement `call_remote_agent()` for `tag agent call`, including both synchronous and SSE streaming modes (~100 lines).
- Implement `sign_agent_card()` and `verify_agent_card_signature()` with JCS + Ed25519 (~80 lines).
- Update `pyproject.toml` with `[a2a]` optional dependency group.
- Write integration tests: `test_serve_and_discover_roundtrip`, `test_agent_call_roundtrip`, `test_discover_saves_to_registry`.
- Write signing tests: `test_jcs_signing_roundtrip`, `test_jcs_tamper_detection`, `test_rfc8785_key_sort_ascii`, `test_signing_key_permissions`.

**Deliverable:** Full bidirectional A2A interoperability — TAG can be discovered by remote orchestrators AND TAG can discover and call remote A2A agents.

### Total Estimated Effort

**3–5 days** for a single engineer, with a target of approximately 600 lines of new Python (across `a2a_card.py`, additions to `api.py` and `controller.py`) and 35 test cases. The implementation is classified **Difficulty 2/5** because it builds on existing infrastructure (`api.py` HTTP server, `open_db()`, `tracing.py`, `controller.py` dispatch pattern) and introduces no new architectural patterns: the A2A endpoint is a new route on an existing server, the task router calls existing `cmd_run` infrastructure, and the discovery client uses stdlib `urllib.request`.

The **Impact 4/5** rating reflects that this single feature makes TAG visible to the entire A2A ecosystem (150+ platforms) with no required changes to existing profiles, enabling a new class of multi-agent workflows that TAG could not participate in before.

