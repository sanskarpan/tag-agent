# Overview

TAG (Terminal Agent Gateway) is a local-first control plane for AI-assisted software work. The repository contains two substantial implementations of that product:

- The Python distribution under `src/tag/` is the documented recommended installation. It provisions and wraps a bundled or fetched Hermes agent runtime, manages per-profile credentials and configuration, runs foreground and detached agents, and supplies orchestration, memory, evaluation, observability, sandboxing, webhook, dashboard, and developer-workflow features. `src/tag/controller.py`, `src/tag/core/run.py`, `src/tag/queue_worker.py`, `src/tag/cron_scheduler.py`, and `src/tag/swarm.py` are representative control and execution paths. The packaged `src/tag/vendor/hermes-agent-upstream.tar.gz` is executable runtime supply-chain input, not inert sample data.
- `tag-go/` is an actively deployed native Go harness and migration target, not merely an example. Its command tree in `tag-go/internal/cli/root.go` exposes local orchestration, provider, memory, CI, sandbox, marketplace, webhook, MCP, LSP, and OpenAI-compatible gateway surfaces. The native agent loop (`tag-go/internal/agent/loop.go`), tool layer (`tag-go/internal/tool/`), provider clients (`tag-go/internal/llm/`), SQLite store, and HTTP listeners are primary runtime code.

TAG is designed mainly for a single developer or operator and normally runs with that user's OS permissions. It is nevertheless security-sensitive because it combines valuable provider and developer credentials, private source repositories, durable prompt/run history, network access, and model-selected file or command tools. Queue, cron, loop, swarm, webhook, CI, issue-solving, and PR-review features turn a one-shot local command into persistent or externally triggered automation. A defect that crosses the model-to-tool boundary can therefore become host compromise even when the model itself is not trusted.

The `docs/`, `tests/`, migration reports, and the small `web/src/` tree help explain or verify the product but are not the center of the deployed trust model. Security review should prioritize the Python package, native Go runtime, embedded assets and registries, process-launching code, HTTP/stdio servers, credential importers, persistent store, and package/install flows.

The primary assets are:

- Provider API keys, OAuth/refresh tokens, GitHub/AWS/cloud credentials, webhook and gateway secrets, and profile `.env` files.
- The operator's source tree, Git worktree and history, local files reachable by the process, and the integrity of edits, commits, PRs, and CI actions produced by agents.
- The local SQLite control-plane store: prompts, outputs, memory, queue and cron jobs, tool decisions, traces, cost records, webhook payloads, evals, and approval state.
- Model budgets and accounts, because forged or looping work can create material spend or exhaust quotas.
- The integrity of permission rules, sandbox claims, approval/audit records, plugin/MCP registries, bundled Hermes runtime, and installed dependencies.
- Availability of long-lived workers, HTTP endpoints, local database, and the host on which they execute.

# Threat Model, Trust Boundaries, and Assumptions

## Actors and input ownership

Operator-controlled inputs include CLI arguments and stdin, trusted profile/configuration edits, explicit permission and auto-approval flags, selected workspace paths, installation choices, environment variables, and administrative creation of webhook rules, cron jobs, plugins, and MCP servers. These inputs can intentionally grant broad powers; the product should make such grants explicit, preserve their scope, and avoid turning a narrow grant into a global one.

Developer-controlled inputs include committed source and configuration maintained by the repository owner, embedded YAML registries, dependency versions, release artifacts, and the bundled Hermes archive. They are trusted only at build/release time. A compromised dependency, registry entry, release pipeline, or upstream runtime crosses directly into users' developer machines.

Attacker-controlled inputs include:

- Contents of a cloned or opened repository, Git diff, issue, PR, test output, build log, prompt file, memory entry, or document sent to a model. These can contain prompt injection even when they are valid source text.
- Model/provider responses, tool calls, streaming events, and model-selected arguments. The model is not an authorization principal; its output must remain untrusted.
- Signed webhook fields such as issue titles, bodies, labels, and URLs. HMAC proves the sender/platform, not that the referenced user content is benign.
- Requests to any exposed gateway, webhook, dashboard, DevUI, or other HTTP listener, including headers, JSON, SSE lifetime, body size, model names, and connection behavior.
- Responses and redirects from provider, search, marketplace, template, notification, OTel, and other outbound endpoints; DNS answers are also hostile at the SSRF boundary.
- Data returned by third-party plugins and MCP servers, and packages or executables installed for them.
- Local credential/configuration files imported from other tools when those files can be modified by another process or repository setup.

## Trust boundaries

1. **Human/operator to TAG.** CLI and configuration choices define profiles, budgets, permission rules, runtime homes, providers, and automation. TAG may assume the current OS user is authorized to make these choices, but malformed configuration must not silently weaken security. Dangerous modes such as `--auto-approve`, `--allow-unsigned`, `--allow-unauthenticated`, `--allow-unconfined`, and `--dangerously-allow-all` are explicit trust-boundary overrides.
2. **Untrusted content to the model.** Repository text, webhook payloads, issue/PR content, tool results, and recalled memory enter model context. No natural-language statement in that content may alter authorization, disclose secrets, or confer tool privileges. Prompt injection is a normal operating condition for an agent that reads arbitrary repositories.
3. **Model to tools and host.** `tag-go/internal/agent/loop.go` executes model-requested calls through registries. File reads/writes, shell commands, MCP tools, web search, Git, CI, and external automation cross from probabilistic output to deterministic side effects. This is the most important privilege boundary in the repository.
4. **Sandboxed child to host.** Sandboxed commands are attacker-influenced code. Filesystem, process, network, time, memory, environment, and working-directory isolation must match the claims returned to callers. The unrestricted Go `bash` tool is explicitly not a sandbox; its safety depends on the permission gate and operator policy.
5. **Local process to credentials and profiles.** Credential importers read other tools' sensitive local state and copy it into TAG profile homes. Profiles provide operational separation, but they run under one OS account and should not be treated as hostile-tenant isolation. A same-UID attacker can generally edit configuration or read process-accessible state outside TAG's control.
6. **TAG to external services.** Provider prompts, repository excerpts, notifications, traces, search queries, and marketplace/template fetches cross the machine/network boundary. Remote services can observe submitted data and return hostile, oversized, slow, redirected, or malformed responses.
7. **Network client to listener.** The Go OpenAI-compatible gateway (`tag-go/internal/gateway/gateway.go`), Python and Go webhook servers, and Python dashboard are network boundaries. Loopback is a useful default but is not authentication against every same-host process; a non-loopback bind changes the exposure materially. TLS and reverse-proxy policy are not implemented by these handlers and are deployment responsibilities.
8. **Runtime to durable SQLite state.** Jobs and automation survive process boundaries through the database. Stored prompts, webhook payloads, memory, or approval records remain untrusted when read later. Database write access is equivalent to substantial control-plane authority in this single-user design.
9. **Core runtime to plugins/MCP/upstream runtime.** Enabled MCP servers and plugins can be independently privileged processes. Python setup can unpack or fetch Hermes and install packages; the Go MCP registry can invoke package managers. Package selection, archive extraction, executable resolution, and environment forwarding are supply-chain boundaries.

## Security invariants

- Model output and retrieved content never authorize a side effect. Every tool invocation must pass the effective operator policy, and headless execution must fail closed when a human decision is required.
- Read/write tools remain within their configured root even through `..`, absolute paths, symlinks, non-existent descendants, or platform path quirks. Credential-shaped paths remain denied unless the operator grants a specific visible exception.
- Sandboxed execution must either enforce and accurately report the requested filesystem/network/resource isolation or refuse to run. Degraded isolation must never be represented as full confinement.
- Publicly reachable listeners require effective authentication unless the operator explicitly selects an insecure mode. Webhook signatures must cover the exact processed bytes, reject stale/replayed requests where supported, and be verified before parsing, persistence, or job creation.
- Remote or webhook-authenticated content remains data, not trusted instructions. It cannot select a more privileged profile, bypass budgets/permissions, or inject executable arguments outside the intended workflow.
- Credentials are not logged, rendered in templates, returned in API errors, placed in prompts, included in diff/workspace context, exposed to unrelated profiles, or written with permissive modes. Credential imports preserve owner-only permissions.
- Outbound fetches cannot reach loopback, private, link-local, metadata, reserved, or alternate-scheme targets through literals, DNS rebinding, redirects, proxies, or IPv4/IPv6 parsing tricks unless that access is an explicit feature.
- Body, output, message, tool-step, recursion, concurrency, and execution limits are enforced before large allocation or side effect; cancellation and timeout handling terminates process trees and producer goroutines.
- SQLite operations preserve job/approval state transitions atomically enough that a failed dispatch is not reported as successful, a job is not executed twice unintentionally, and attacker text is bound as data rather than SQL.
- Embedded runtime archives, registries, packages, and updates cannot escape their destination or substitute unexpected executable content without a deliberate trust decision.
- Observability, memory, dashboard, and export surfaces do not create a second unprotected channel for sensitive prompts, source, secrets, tool arguments, or results.

Assumptions are that the OS account and trusted local configuration are not already compromised; filesystem permissions and kernel/container primitives behave as documented; provider endpoints intentionally receive the prompts sent to them; and the default deployment is single-user rather than mutually distrusting multi-tenant hosting. Root compromise, malicious kernel/runtime behavior, physical access, and a hostile same-UID user are outside TAG's containment guarantees. Public deployment, shared machines, or processing repositories from untrusted contributors makes the permission, authentication, and sandbox controls mandatory rather than optional.

# Attack Surface, Mitigations, and Attacker Stories

## Agent execution and workspace access

The realistic high-value attack is indirect prompt injection: a malicious README, issue body, test log, or webhook prompt tells the model to read credentials, alter unrelated files, or run a shell command. The model may comply, so the enforcement point is the tool boundary. In Go, `tag-go/internal/tool/tools.go` root-confines file tools, resolves symlinked ancestors, caps reads, bounds shell duration, and routes built-ins through `tag-go/internal/permission/`. Default policy allows confined reads/listing, asks for writes and shell, denies credential paths, and converts `ask` to deny without a prompt. Unknown MCP/plugin tools also default to `ask`. Explicit auto-approval and allow-all modes intentionally weaken this boundary and must never become implicit in queue, cron, gateway, CI, or webhook paths.

The native shell tool executes `sh -c` with the user's host privileges and only uses the tool root as a working directory. Therefore command-pattern matching, content guardrails, human approval, timeouts, process-group cleanup, and clear unsafe-mode UX matter more than cwd confinement. MCP calls in `tag-go/internal/tool/mcp_bridge.go` are third-party side effects and should remain permission-gated even when the MCP server itself is curated.

Python similarly launches the Hermes runtime and numerous subprocess workflows through `src/tag/core/run.py`, controller/setup helpers, workers, loops, CI, and issue/PR commands. Detached queue and cron work has no interactive human by default. Security review should trace whether profile selection, approval semantics, budgets, environment composition, and workspace scope remain intact across these process boundaries.

## Sandboxes and code execution

`src/tag/sandbox.py` and `tag-go/internal/sandbox/` accept operator- or agent-influenced commands. The Go implementation documents platform-specific macOS sandbox profiles, Linux Landlock/network namespace/resource limits, Docker egress policy, broad-directory refusal, and fail-closed behavior when real isolation is unavailable. It also discloses degraded claims rather than silently asserting confinement. The Python restricted backend and cloud/container alternatives must receive equal scrutiny, especially for shell opt-ins, environment inheritance, working-directory symlinks, process descendants, network access, output caps, and timeouts.

A sandbox escape is realistic whenever an agent evaluates an untrusted repository or generated code. `--allow-unconfined` is acceptable only as an informed operator override; it must not be selected automatically after a platform failure.

## Inbound services and durable automation

The Go gateway defaults to loopback, can require a bearer key using constant-time comparison, refuses unauthenticated non-loopback binds unless explicitly overridden, caps chat bodies, and sets header/idle timeouts. Its direct provider surface can still be abused for account spend, data submission, model enumeration, or availability if exposed without authentication. Any future wiring of this endpoint to host tools would raise its impact sharply and must preserve the model-to-tool gate.

Python and Go webhook receivers accept attacker-authored issue/PR/event text and convert matching events into durable jobs. Existing controls include HMAC verification, explicit refusal without a secret unless unsigned mode is chosen, Slack timestamp checks, bounded request bodies, connection timeouts, parameterized persistence, and bounded delivery-ID replay caches. These prove provenance and improve availability, but do not sanitize prompt injection. Rules and profile selection must be operator-owned, and a verified event must receive the same tool/budget restrictions as any other untrusted task.

The Python dashboard in `src/tag/api.py` exposes local run, span, queue, and cost data without wildcard CORS and binds to loopback by default. Binding it to a LAN/public address is a confidentiality change because it has no application authentication. Similar DevUI/web surfaces should not inherit an assumption that loopback-only data is safe after rebinding.

## Credentials, persistence, and data egress

Python and Go importers read credentials from Codex, Claude, Gemini, GitHub, AWS, editors, and other local tools. The Go importer writes profile `.env` files with mode `0600` and sanitizes line breaks. The integrity and confidentiality of source files, destination paths, output/error rendering, and profile environment construction are critical. The importer must never echo secret values or follow attacker-controlled destinations outside the profile home.

SQLite is deliberately local and WAL-backed. Parameter binding is common and helps against injection, but the store contains sensitive prompts, webhook bodies, model outputs, memory, and audit data. Local dashboard, trace export, logs, templates, snapshots, and backup paths must treat all of it as confidential and untrusted for rendering. Memory poisoning is also a realistic persistence attack: malicious content stored as a durable fact can influence unrelated future runs.

Outbound notifications and marketplace/template fetches cross an SSRF and exfiltration boundary. `src/tag/notifications.py` and `tag-go/internal/marketplace/marketplace.go` contain public-address checks, redirect/connect-time validation, scheme restrictions, response caps, and timeouts. Equivalent protections must apply consistently to every URL-accepting feature, including OTel exporters, webhooks, provider base-URL overrides, local-provider configuration, and future integrations. Intentional local-provider access is a distinct operator-controlled feature and should not accidentally weaken unrelated fetchers.

## Supply chain and installation

The Python distribution embeds a Hermes tarball and can update or install runtime dependencies. `src/tag/controller.py` rejects absolute/traversal archive members, links, and unsupported file types before extraction. The Go registry can install MCP packages with npm/pip, while plugin and curated-MCP flows enable registry-specified components. A malicious registry change, dependency release, executable substitution in `PATH`, or compromised bundled archive can run code with developer privileges. Exact Python dependency pins and embedded Go modules reduce opportunistic drift, but release provenance, lockfile consistency, registry review, package-manager arguments, and artifact integrity remain important.

## Less relevant or out-of-scope stories

- Traditional cross-tenant authorization is less relevant in the documented single-user local deployment; profiles are not security tenants. It becomes high risk only if TAG is repurposed as a shared hosted service.
- SQL injection from remote text is generally lower likelihood where code consistently uses bound SQLite parameters. Direct local database modification by the same OS user is outside the hostile-tenant model, though defensive parsing still protects availability.
- Browser XSS is secondary for the current minimal local dashboard and React pages, but stored model/webhook text must remain encoded because a richer UI or non-loopback bind would make stored injection realistic.
- An attacker who already controls the TAG OS account, configuration, binary, or provider account can bypass application-layer controls and is outside the intended boundary.
- Provider confidentiality cannot be guaranteed for text intentionally sent to a selected provider. TAG must prevent accidental secret inclusion, but it cannot make a third-party inference service unable to observe submitted prompts.

# Severity Calibration (Critical, High, Medium, Low)

## Critical

Critical issues enable broad, low-friction compromise of a developer host or credentials from an untrusted/remote input, or compromise the distributed runtime supply chain. Examples include an unauthenticated network path that reaches unrestricted shell execution; a default sandbox escape that lets generated code read the user's home and execute outside confinement; archive/package installation traversal or registry compromise that executes arbitrary code for every installer; or bulk exfiltration of provider, cloud, and source-control credentials through a default workflow. Critical requires a demonstrated path across the relevant authentication, permission, or operator-choice boundary, not merely the presence of a command runner.

## High

High issues cross a major boundary with meaningful prerequisites or narrower scope. Examples include bypassing the Go permission gate to read `.env`/SSH/AWS material or write outside the workspace; webhook authentication/replay flaws that let an external attacker create privileged agent jobs; bearer-auth bypass on a non-loopback service with access to paid providers or sensitive prompts; SSRF reaching cloud metadata or an internal control service; cross-profile credential disclosure; or a reliable escape from a non-default but documented sandbox configuration. Persistent prompt/memory poisoning that predictably causes later privileged tool execution can also be high when the tool path is shown.

## Medium

Medium issues expose limited sensitive data, corrupt bounded workflow state, bypass a defense without reaching a high-impact sink, or cause material but recoverable resource loss. Examples include unauthenticated LAN exposure of run/trace/memory records after an operator bind choice; a body, SSE, worker, or process-tree flaw allowing bounded service exhaustion; duplicate signed events that trigger limited jobs or provider spend; budget/step-limit bypass confined to one profile; unsafe rendering of stored attacker text in a local UI; or workspace path escape that reaches non-secret files but not credentials or execution. Severity rises if the affected endpoint is public by default or the data includes credentials/source.

## Low

Low issues have small local impact, require strong operator/local access, or affect robustness without crossing a meaningful security boundary. Examples include leakage of non-sensitive model names or health metadata, a minor local-only port/thread leak, misleading but non-security-critical error detail, incomplete redaction of values that are demonstrably non-secret, or a crash on malformed input reachable only from the trusted local CLI and without persistent corruption. Pure documentation/test/example issues are low or not applicable unless they directly shape a deployed insecure default.

Repository: target_sha256_d47a7e615920ce18ed8cc35a01c1c5700717a06dba03d5af84a5f205a6c0bc02
Version: a578919dd91766b18ba9ec9492151cac5c50c754
