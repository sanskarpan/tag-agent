# PRD-086: ANP Identity Layer: W3C DID-Based Decentralized Agent Identity (`tag identity`)

**Status:** Proposed
**Priority:** P3
**Estimated Effort:** XL (4-8 weeks)
**Category:** Multi-Agent Interoperability Protocols
**Affects:** `anp_identity.py`
**Depends on:** PRD-028 (Sandbox Code Execution), PRD-034 (Secret Scanning), PRD-013 (Agent Tracing / Observability), PRD-027 (Eval Framework), PRD-074 (MCP OAuth PKCE / Device Flow), PRD-078 (HITL Tool Approval Audit Trail)
**GitHub Issue:** #347
**Inspired by:** ANP (Agent Network Protocol), W3C DID Core spec (W3C Recommendation 2022), ION on Bitcoin, did:wba HTTP Message Signatures (RFC 9421)

---

## 1. Overview

Modern multi-agent systems that coordinate across organizational boundaries require a trustworthy, forgery-resistant way to identify agents — not just within a single platform, but across arbitrary networks, providers, and execution environments. Today, TAG agents have no stable cryptographic identity. When a TAG `coder` profile initiates a task via A2A or ANP and reaches a remote agent, that remote agent has no mechanism to verify it is genuinely talking to the TAG coder agent and not an impersonator. Shared secrets, bearer tokens, and platform-issued certificates all require trusting a central authority. For truly open, federated multi-agent networks, a decentralized alternative is essential.

The W3C Decentralized Identifier (DID) specification (https://www.w3.org/TR/did-core/) defines a URI scheme for self-sovereign identifiers backed by cryptographic key pairs. A DID like `did:web:example.com:agents:coder` or `did:key:z6MkhaXgBZDvotDkL5257faiztiGiC2QtKLGpbnnEGta2doK` resolves to a DID Document — a JSON-LD document that contains the agent's public verification keys, service endpoints, and capability declarations. Because the identifier is derived from the key material (did:key) or from a URL under the agent operator's control (did:web), no third-party registry or certificate authority is required. The identity is self-issued and cryptographically verifiable by anyone who can resolve the DID.

The Agent Network Protocol (ANP) uses DID as the foundational trust layer for agent-to-agent authentication. ANP's did:wba (DID Web-Based Authentication) method extends did:web with RFC 9421 HTTP Message Signatures — allowing an agent to prove control of its DID private key by signing HTTP requests, and then exchange that proof for a short-lived JWT Bearer token for subsequent API interactions. This approach provides strong authentication without requiring agents to share secrets in advance and without centralizing identity management.

This PRD specifies `anp_identity.py` — a new TAG module that gives each TAG profile its own W3C DID, manages the associated Ed25519 key pairs in the TAG keystore, publishes DID Documents for did:web profiles, enables local did:key generation for offline use, implements the ANP did:wba HTTP Message Signature authentication handshake, and provides the `tag identity` CLI surface for all identity lifecycle operations. The module is the cryptographic foundation for all future ANP interoperability work in TAG (task delegation, federated agent discovery, cross-network trust chains).

This feature is rated Difficulty 5/5 because it requires careful implementation of W3C DID Core, RFC 8785 JSON Canonicalization, RFC 9421 HTTP Message Signatures, and Ed25519 key management — all of which have subtle correctness requirements. It is rated Impact 2/5 because ANP adoption in the broader ecosystem is nascent as of mid-2026, and the immediate user-facing value is primarily infrastructure that unblocks future high-impact protocol integrations. Teams building federated multi-agent pipelines will find it essential; single-user deployments will not need it.

---

## 2. Problem Statement

### 2.1 No Cryptographic Agent Identity Across Trust Boundaries

TAG profiles are identified only by name within the local TAG installation. When a TAG agent communicates with an external agent via A2A, ACP, or ANP, there is no mechanism for the receiving agent to verify that the sender is who it claims to be. The only available signals are transport-level credentials (OAuth tokens, API keys), which are issued by centralized providers and do not carry intrinsic semantic meaning about the agent's identity, capabilities, or operator. A remote agent receiving an A2A task request from a `coder` TAG profile cannot distinguish it from a request fabricated by a malicious actor who obtained the same OAuth token. This is not a theoretical risk: in open multi-agent networks where agents from different organizations communicate, identity impersonation is a concrete attack vector.

### 2.2 No Standard Resolution Path for Agent Descriptions

The A2A, ACP, ANP, and MCP protocols all define different discovery patterns. ANP specifically uses DID resolution as the entry point: given a DID, a resolver fetches the DID Document from `/.well-known/did.json` (for did:web) or derives it from the DID string itself (for did:key), and the DID Document's `service` section points to the agent's description document, capability endpoint, and communication channels. Without a DID, TAG agents cannot participate in ANP's discovery flow: remote agents cannot find TAG agents by DID, cannot resolve their service endpoints, and cannot verify their capability declarations are authentic (signed by the agent's own key). TAG is invisible to the growing ecosystem of ANP-aware agents.

### 2.3 Key Material Is Currently Unmanaged

TAG already generates Ed25519 signing keys for some security operations (secret scanning, sandbox attestations). However, these keys are ephemeral, per-operation, and not associated with a persistent profile identity. There is no keystore, no key rotation policy, no revocation mechanism, and no standard way to export a public key for external verification. Any future cryptographic protocol (ANP did:wba, Verifiable Credentials, signed agent cards) requires a principled key management layer. Building this layer piecemeal inside individual protocol modules would create divergence and security inconsistencies. `anp_identity.py` establishes that layer once, correctly, and shares it across all consumers.

---

## 3. Goals

| ID | Goal |
|----|------|
| G1 | Generate a W3C-conformant DID Document for each TAG profile using either `did:web` (for agents with a public-facing HTTPS endpoint) or `did:key` (for local/offline agents). |
| G2 | Manage Ed25519 key pairs in a TAG keystore backed by the existing SQLite database; support key rotation with version tracking and old-key revocation. |
| G3 | Publish did:web DID Documents at the correct well-known URL (`/.well-known/did.json` served by the TAG API server) without requiring external hosting. |
| G4 | Implement the ANP did:wba authentication handshake: sign outbound HTTP requests with RFC 9421 HTTP Message Signatures and exchange the proof for a JWT Bearer token. |
| G5 | Implement DID Document signing using RFC 8785 JSON Canonicalization Scheme (JCS) and Ed25519, with the `DataIntegrityProof` proof type. |
| G6 | Provide `tag identity create`, `show`, `resolve`, and `verify` CLI commands covering the full DID lifecycle. |
| G7 | Cache resolved external DID Documents in SQLite with configurable TTL to avoid repeated network fetches. |
| G8 | Integrate DID verification into the ANP outbound request pipeline so that `tag` agents can authenticate to remote ANP endpoints without manual key configuration. |
| G9 | Expose DID Document as a service endpoint in any A2A Agent Card published by TAG (forward compatibility). |

---

## 4. Non-Goals

| ID | Non-Goal |
|----|----------|
| NG1 | Full ANP agent-to-agent task delegation protocol — that is a separate PRD. This PRD provides only the identity layer that such a protocol will depend on. |
| NG2 | did:ion (Bitcoin-anchored DID method) — ION is listed as inspiration but production ION requires Bitcoin transaction fees and Sidetree infrastructure. Out of scope for v1; noted as a future upgrade path. |
| NG3 | did:peer, did:ethr, did:sov, or other DID methods beyond did:web and did:key. |
| NG4 | Verifiable Credentials issuance or presentation — DIDs are the identity layer; VCs are an application layer on top. Future PRD. |
| NG5 | Universal DID Resolver integration (uniresolver.io) — resolving non-did:web/did:key DIDs from external parties is out of scope for v1. |
| NG6 | Hardware security module (HSM) or OS keychain integration — key material is protected at rest by the SQLite database encryption layer (existing security.py AES-256-GCM). HSM is a future upgrade. |
| NG7 | Multi-key ceremony or threshold signing — single Ed25519 key per profile identity is sufficient for v1. |
| NG8 | Automatic DID Document publication to external did:web hosts (GitHub Pages, Cloudflare) — users who want external hosting must export and deploy manually. |

---

## 5. Success Metrics

| Metric | Target | Measurement |
|--------|--------|-------------|
| DID creation latency | < 100 ms for did:key, < 500 ms for did:web | `time tag identity create --profile coder --method did:key` |
| DID Document schema conformance | 100% of generated documents pass W3C DID Core JSON-LD validation | CI test using `did-spec-registries` validator |
| JCS canonicalization correctness | 100% match against RFC 8785 test vectors | Unit tests against all 42 published test vectors |
| HTTP Message Signature correctness | Passes ANP did:wba conformance test suite (20 cases) | Integration test against conformance harness |
| DID resolution cache hit rate | > 80% for repeated resolutions of same DID within TTL window | SQLite query on `anp_did_cache` hit/miss counters |
| Key rotation correctness | Old key proofs rejected, new key proofs accepted after rotation | Integration test: sign with old key post-rotation, verify = FAIL |
| CLI command p95 latency (local ops) | < 200 ms | pytest-benchmark suite |
| `tag identity verify` false positive rate | 0% on tampered documents | Fuzz test: 1000 random bit-flip mutations of valid signed docs |

---

## 6. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|-----------|----------|
| U1 | Platform engineer | run `tag identity create --profile coder --method did:web --domain agents.mycompany.com` | My `coder` agent has a stable, globally resolvable DID that remote ANP agents can verify |
| U2 | Developer | run `tag identity create --profile local-dev --method did:key` | I can test ANP authentication locally without needing an HTTPS endpoint |
| U3 | Security engineer | run `tag identity show --profile coder --format json` | I can inspect the full DID Document and verify the key material and service endpoints are correct before publishing |
| U4 | Platform engineer | run `tag identity resolve did:web:agents.mycompany.com:coder` | I can verify that the published DID Document at the well-known URL matches what TAG generated, before relying on it for authentication |
| U5 | Developer | run `tag identity verify --did did:web:agents.partner.com:researcher` | I can confirm a partner agent's DID Document has a valid self-signature before trusting its capability declarations |
| U6 | Security engineer | run `tag identity rotate --profile coder` | I can replace a compromised or expired key while the DID remains stable, so existing relationships are not broken |
| U7 | Platform engineer | inspect `~/.tag/runtime/tag.sqlite3` | I can see all DID-related state (keys, documents, cache) alongside other TAG state for unified backup and audit |
| U8 | Developer | integrate `tag identity` DID into an A2A Agent Card | Remote A2A agents can find and verify my TAG agent's DID endpoint alongside its A2A capability declaration |
| U9 | Operator | run `tag identity export --profile coder --output did-document.json` | I can deploy the DID Document to an external host (Nginx, GitHub Pages) for did:web resolution when TAG API server is not publicly accessible |
| U10 | Developer | use `tag identity sign --profile coder --input payload.json` | I can produce a JCS-canonicalized, Ed25519-signed document for use in ANP agent description publishing |

---

## 7. Proposed CLI Surface

All identity subcommands live under the `tag identity` namespace.

### 7.1 `tag identity create`

Create a new W3C DID and key pair for a TAG profile.

```
tag identity create \
  --profile <name> \
  --method {did:web|did:key} \
  [--domain <hostname>] \
  [--path <url-path-component>] \
  [--key-type {ed25519}] \
  [--service-endpoint <url>] \
  [--force] \
  [--json]
```

**Options:**
- `--profile`: TAG profile name (required). Must exist in `~/.tag/profiles/`.
- `--method`: DID method (required). `did:web` requires `--domain`. `did:key` derives the DID from the public key.
- `--domain`: Hostname for did:web (e.g., `agents.mycompany.com`). Ignored for did:key.
- `--path`: Additional URL path components for did:web (e.g., `users:alice`). Encoded per did:web spec: colons represent `/` separators. Defaults to `agents:<profile-name>`.
- `--key-type`: Signing key algorithm. Only `ed25519` is supported in v1.
- `--service-endpoint`: URL of the agent's ANP service. If omitted and the TAG API server is running, defaults to `http://localhost:<port>/anp/<profile>`.
- `--force`: Overwrite an existing DID for this profile. Requires explicit confirmation unless `--yes`.
- `--json`: Output the created DID Document as JSON.

**Example output:**

```
Creating DID for profile "coder" (method: did:web) ...

  DID:         did:web:agents.mycompany.com:agents:coder
  Method:      did:web
  Key type:    Ed25519
  Key ID:      did:web:agents.mycompany.com:agents:coder#key-1
  Public key:  z6MkhaXgBZDvotDkL5257faiztiGiC2QtKLGpbnnEGta2doK  (base58btc multibase)
  Created at:  2026-06-17T10:23:41Z

  DID Document stored in: tag.sqlite3 (anp_did_registry)
  Publish at:  https://agents.mycompany.com/.well-known/did.json
               (if running: tag api serve --profile coder --public)

  Next steps:
    tag identity show --profile coder
    tag identity export --profile coder --output did-document.json
```

### 7.2 `tag identity show`

Display the DID Document for a profile.

```
tag identity show \
  --profile <name> \
  [--format {text|json|json-ld}] \
  [--include-private-key-fingerprint]
```

**Example output (text):**

```
DID Document: did:web:agents.mycompany.com:agents:coder
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
  id:          did:web:agents.mycompany.com:agents:coder
  created:     2026-06-17T10:23:41Z
  updated:     2026-06-17T10:23:41Z
  key version: 1

  Verification Methods:
    #key-1   Ed25519VerificationKey2020
             Public key (base58btc): z6MkhaXgBZDvot...

  Verification Relationships:
    authentication:       [#key-1]
    assertionMethod:      [#key-1]
    capabilityInvocation: [#key-1]

  Services:
    #anp-endpoint   ANPEndpoint
                    serviceEndpoint: https://agents.mycompany.com/anp/coder

  Proof:
    type:               DataIntegrityProof
    cryptosuite:        eddsa-jcs-2022
    created:            2026-06-17T10:23:41Z
    verificationMethod: did:web:agents.mycompany.com:agents:coder#key-1
    proofValue:         z2FphhniH...  (Ed25519 signature over JCS-canonical document)
```

### 7.3 `tag identity resolve`

Resolve an arbitrary DID string to its DID Document (with caching).

```
tag identity resolve <did> \
  [--no-cache] \
  [--timeout <seconds>] \
  [--json]
```

**Example:**

```
tag identity resolve did:web:agents.mycompany.com:agents:coder
```

**Output:**

```
Resolving did:web:agents.mycompany.com:agents:coder ...
  Method:   did:web
  URL:      https://agents.mycompany.com/.well-known/did.json
  Source:   network (cached for 3600s)
  Status:   resolved

{
  "@context": ["https://www.w3.org/ns/did/v1", ...],
  "id": "did:web:agents.mycompany.com:agents:coder",
  ...
}
```

For did:key DIDs, resolution is fully local (key material is encoded in the DID string itself — no network fetch required):

```
tag identity resolve did:key:z6MkhaXgBZDvotDkL5257faiztiGiC2QtKLGpbnnEGta2doK
  Method:   did:key
  Source:   local derivation (no network)
  Status:   resolved
```

### 7.4 `tag identity verify`

Verify the integrity of a DID Document's self-signature (DataIntegrityProof).

```
tag identity verify \
  --did <did-string> \
  [--document <path-to-json>] \
  [--no-cache] \
  [--json]
```

If `--document` is omitted, the document is resolved via `tag identity resolve`. Verification:
1. Extract the `proof` field from the document.
2. Remove the `proof` field from the document copy.
3. JCS-canonicalize the proof-stripped document.
4. Resolve `proof.verificationMethod` to a public key.
5. Verify the Ed25519 signature in `proof.proofValue` over the canonical bytes.

**Example output:**

```
Verifying DID Document: did:web:agents.partner.com:researcher ...

  Document resolved:   YES (from cache, age: 142s)
  Proof type:          DataIntegrityProof (eddsa-jcs-2022)
  Verification method: did:web:agents.partner.com:researcher#key-1
  Public key:          z6Mkf5rGMoatr...
  JCS canonical hash:  sha256:a4f3e2...
  Signature:           VALID

  Result: VERIFIED
```

On failure:

```
  Signature:  INVALID — document may have been tampered with
  Result: VERIFICATION FAILED (exit code 2)
```

### 7.5 `tag identity rotate`

Generate a new key pair, update the DID Document, and retire the old key.

```
tag identity rotate \
  --profile <name> \
  [--keep-old-for <seconds>] \
  [--json]
```

The old key remains listed in the DID Document's `verificationMethod` array (with a `revoked` timestamp in `extra_json`) for `--keep-old-for` seconds (default: 3600), allowing in-flight sessions to drain. After the window, `tag identity rotate --profile coder --prune` removes the old key from the document.

### 7.6 `tag identity export`

Export the DID Document to a file for external hosting.

```
tag identity export \
  --profile <name> \
  --output <path> \
  [--format {json|json-ld}]
```

Exports to `did-document.json` (or `did.json`) for deployment to an external web server. Includes deployment instructions for Nginx and Caddy as a comment block.

### 7.7 `tag identity sign`

Sign an arbitrary JSON document using the profile's identity key (JCS + Ed25519).

```
tag identity sign \
  --profile <name> \
  --input <path> \
  [--output <path>] \
  [--purpose {authentication|assertionMethod|capabilityInvocation}]
```

Embeds a `DataIntegrityProof` in the document. Used by agent description publishing (ANP agent description spec) and signed A2A agent cards.

### 7.8 `tag identity list`

List all profiles with associated DIDs.

```
tag identity list [--json]
```

```
Profile    DID                                              Method    Key Version  Created
coder      did:web:agents.mycompany.com:agents:coder       did:web   1            2026-06-17
local-dev  did:key:z6MkhaXgBZDvotDkL5257faiztiGiC2Qt...  did:key   1            2026-06-10
```

---

## 8. Functional Requirements

| ID | Requirement |
|----|-------------|
| FR-01 | `tag identity create --method did:key` MUST derive the DID string from the Ed25519 public key using the multicodec prefix `0xed01`, multibase encoding `z` (base58btc), and the did:key specification. No network access is required. |
| FR-02 | `tag identity create --method did:web --domain <hostname>` MUST construct the DID as `did:web:<hostname>:<path-components>` where each URL path segment is represented as a colon-separated component per the did:web specification. |
| FR-03 | Generated DID Documents MUST include `@context` with `https://www.w3.org/ns/did/v1` and `https://w3id.org/security/suites/ed25519-2020/v1` as the first two context entries. |
| FR-04 | Each DID Document MUST include at minimum: `id`, `@context`, `verificationMethod` (one entry), `authentication` (referencing the verification method), `assertionMethod` (referencing the verification method), and `service` (ANP endpoint). |
| FR-05 | Verification method entries MUST use type `Ed25519VerificationKey2020` with `publicKeyMultibase` encoding (multibase prefix `z`, base58btc-encoded 32-byte raw public key). |
| FR-06 | Every locally-owned DID Document MUST be self-signed using a `DataIntegrityProof` with cryptosuite `eddsa-jcs-2022`. The signing process MUST: (1) remove the `proof` field from the document, (2) JCS-canonicalize the document per RFC 8785, (3) sign the canonical bytes with Ed25519, (4) encode the signature as base58btc with multibase prefix `z`, (5) attach as `proof.proofValue`. |
| FR-07 | JCS canonicalization MUST sort JSON object keys by UTF-16 code unit comparison (RFC 8785 Section 3.2.3), NOT by simple byte comparison. Numbers MUST be serialized per IEEE 754 / ECMAScript rules (no trailing `.0`, no unnecessary exponents). The `rfc8785` package (Trail of Bits, zero dependencies) is the canonical implementation. |
| FR-08 | Private keys MUST be stored in `anp_identity_keys` table in `tag.sqlite3`, encrypted at rest using AES-256-GCM via `security.py`'s existing encryption helpers. The plaintext private key MUST NOT appear in any log, trace span, or SQLite row outside the `key_ciphertext` column. |
| FR-09 | `tag identity resolve did:web:<domain>:<path>` MUST construct the resolution URL as `https://<domain>/<path-components>/did.json` where colon separators are converted to `/`. If path is absent, the URL is `https://<domain>/.well-known/did.json`. |
| FR-10 | Resolved DID Documents MUST be cached in the `anp_did_cache` SQLite table with a TTL (default: 3600 seconds). Cache entries MUST be keyed by the full DID string. `--no-cache` forces a fresh network fetch and updates the cache entry. |
| FR-11 | `tag identity verify` MUST return exit code 0 on valid signature, exit code 2 on invalid signature, exit code 1 on resolution failure or missing proof. It MUST NOT emit the raw document bytes to stdout on failure to prevent using `verify` as an accidental resolver proxy. |
| FR-12 | `tag identity rotate --profile <name>` MUST generate a new Ed25519 key pair, increment the `key_version` in `anp_did_registry`, and update the DID Document with the new verification method. The old key MUST be retained in the database with `retired_at` timestamp and MUST be listed in the DID Document with `revoked` metadata for the duration of `--keep-old-for`. |
| FR-13 | The TAG API server (existing `api.py`) MUST serve the did:web DID Document at `GET /.well-known/did.json?profile=<name>` and at `GET /agents/<name>/did.json`. The response MUST set `Content-Type: application/did+json` and `Cache-Control: max-age=3600`. |
| FR-14 | `tag identity sign --purpose authentication` MUST produce a `DataIntegrityProof` with `proofPurpose: "authentication"`. The proof MUST include `challenge` and `domain` fields if the document being signed contains a `challenge` or `domain` field, for replay-attack resistance. |
| FR-15 | The ANP did:wba outbound request signing MUST use RFC 9421 HTTP Message Signatures. The `Signature-Input` header MUST include the full DID URL (including fragment) as the `keyid` parameter: e.g., `keyid="did:web:example.com:agents:coder#key-1"`. The signed components MUST include at minimum: `@method`, `@target-uri`, `@authority`, `content-type`, `content-digest`, and `date`. |
| FR-16 | `tag identity list` MUST query `anp_did_registry` and display all profiles with DID records. Output MUST include DID string, method, key version, and creation timestamp. |
| FR-17 | All `tag identity` subcommands MUST support `--json` for machine-readable output. JSON output schema MUST be stable across patch releases. |
| FR-18 | `tag identity export --profile <name> --output <path>` MUST write a valid JSON-LD DID Document to the specified path. If the file exists, the command MUST prompt for overwrite confirmation unless `--force` is set. |
| FR-19 | When a DID is created, a structured trace span MUST be emitted via `tracing.py` (PRD-013) with span name `anp_identity.create`, attributes `did.method`, `did.profile`, `did.key_version`, and `did.created_at`. No private key material MUST appear in trace attributes. |
| FR-20 | `tag identity create` MUST be idempotent within the same profile+method combination if `--force` is NOT set: re-running with the same inputs produces an error message and exit code 1 rather than overwriting the existing identity. |

---

## 9. Non-Functional Requirements

| ID | Requirement |
|----|-------------|
| NFR-01 | **Cryptographic correctness:** Ed25519 operations MUST use the `cryptography` library (PyCA, v43+), not any other Python Ed25519 implementation. Test vectors from RFC 8032 MUST be verified in CI. |
| NFR-02 | **Key material never in logs:** The module MUST be audited to ensure no Ed25519 private key bytes pass through Python `logging`, `print`, or the OpenTelemetry span attribute layer. This is enforced by a static analysis rule in CI (grep for `private_key` near log calls). |
| NFR-03 | **W3C DID Core conformance:** Generated DID Documents MUST pass the W3C DID Core conformance test suite at the "Core Properties" level. CI MUST include a conformance check using the `did-resolver` npm package or equivalent Python validator. |
| NFR-04 | **RFC 8785 test vector pass rate:** The JCS implementation MUST pass all 42 normative test vectors published at https://www.rfc-editor.org/rfc/rfc8785#appendix-B. This is verified by a dedicated unit test file `tests/test_jcs_vectors.py`. |
| NFR-05 | **Resolution timeout:** `tag identity resolve` MUST timeout network fetches after 10 seconds (configurable via `identity.resolve_timeout_seconds` in `cli-config.yaml`). A timeout MUST return exit code 1 with a descriptive error, not a Python stack trace. |
| NFR-06 | **SQLite WAL compatibility:** All reads and writes to `anp_did_registry`, `anp_identity_keys`, and `anp_did_cache` MUST use the existing `open_db()` helper and MUST respect WAL mode. No module-level connection caching that bypasses WAL checkpointing. |
| NFR-07 | **did:key offline operation:** Creating and resolving did:key DIDs MUST require no network access whatsoever. The entire operation MUST be completable in an air-gapped environment. |
| NFR-08 | **Backward compatibility of DID Documents:** Once a DID Document is published for a profile, the `id` field MUST NOT change across key rotations. Key rotation updates only the `verificationMethod`, `authentication`, and `assertionMethod` fields. |
| NFR-09 | **CLI startup latency:** Importing `anp_identity` MUST not add more than 50 ms to `tag` CLI startup time on a cold Python process. Heavy imports (`cryptography`, `rfc8785`) MUST be lazy-loaded inside function bodies, not at module level. |
| NFR-10 | **Secure deletion of old key material:** When `tag identity rotate --prune` removes an old key, the `key_ciphertext` column for that row MUST be overwritten with null or a sentinel value before the row is deleted, to prevent SQLite page-level recovery of the ciphertext. |

---

## 10. Technical Design

### 10.1 New Files

- **`src/tag/anp_identity.py`** — All DID and ANP identity logic. Exposed to `controller.py` via `cmd_identity()`. Zero circular imports with other TAG modules.
- **`tests/test_anp_identity.py`** — Unit and integration tests.
- **`tests/test_jcs_vectors.py`** — RFC 8785 test vector verification (standalone, no TAG dependencies).

### 10.2 SQLite DDL

```sql
-- Stores the authoritative DID Document and key metadata for each profile
CREATE TABLE IF NOT EXISTS anp_did_registry (
    id              TEXT PRIMARY KEY,         -- uuid4 row id
    profile         TEXT NOT NULL UNIQUE,     -- TAG profile name (FK to profiles)
    did             TEXT NOT NULL UNIQUE,     -- Full DID string e.g. did:web:example.com:agents:coder
    method          TEXT NOT NULL,            -- 'did:web' or 'did:key'
    domain          TEXT,                     -- hostname for did:web; NULL for did:key
    did_path        TEXT,                     -- colon-separated path for did:web; NULL for did:key
    key_version     INTEGER NOT NULL DEFAULT 1,
    did_document    TEXT NOT NULL,            -- JSON-serialized DID Document (with proof)
    service_endpoint TEXT,                   -- ANP service endpoint URL
    created_at      TEXT NOT NULL,            -- ISO-8601 UTC
    updated_at      TEXT NOT NULL             -- ISO-8601 UTC (updated on key rotation)
);

CREATE INDEX IF NOT EXISTS idx_adr_did ON anp_did_registry(did);
CREATE INDEX IF NOT EXISTS idx_adr_profile ON anp_did_registry(profile);

-- Stores Ed25519 key pairs (private key encrypted at rest)
CREATE TABLE IF NOT EXISTS anp_identity_keys (
    id              TEXT PRIMARY KEY,         -- uuid4
    profile         TEXT NOT NULL,            -- TAG profile name
    did             TEXT NOT NULL,            -- Parent DID string
    key_id          TEXT NOT NULL,            -- DID URL with fragment e.g. did:...:coder#key-1
    key_version     INTEGER NOT NULL,         -- Monotonically increasing per profile
    public_key_multibase TEXT NOT NULL,       -- Base58btc multibase-encoded 32-byte public key
    key_ciphertext  TEXT NOT NULL,            -- AES-256-GCM encrypted Ed25519 private key seed (base64)
    key_nonce       TEXT NOT NULL,            -- AES-GCM nonce (base64)
    algorithm       TEXT NOT NULL DEFAULT 'Ed25519',
    created_at      TEXT NOT NULL,
    retired_at      TEXT,                     -- NULL if active; ISO-8601 UTC if rotated out
    revoked         INTEGER NOT NULL DEFAULT 0 -- 1 if explicitly revoked
);

CREATE INDEX IF NOT EXISTS idx_aik_profile ON anp_identity_keys(profile, key_version);
CREATE INDEX IF NOT EXISTS idx_aik_key_id ON anp_identity_keys(key_id);

-- Cache for externally resolved DID Documents (TTL-based)
CREATE TABLE IF NOT EXISTS anp_did_cache (
    did             TEXT PRIMARY KEY,         -- Full DID string (cache key)
    did_document    TEXT NOT NULL,            -- JSON-serialized DID Document
    resolved_at     TEXT NOT NULL,            -- ISO-8601 UTC of last resolution
    ttl_seconds     INTEGER NOT NULL DEFAULT 3600,
    source_url      TEXT,                     -- URL used for resolution (did:web only)
    hit_count       INTEGER NOT NULL DEFAULT 0  -- total cache hits for this entry
);

-- ANP did:wba authentication token cache (JWT Bearer tokens issued by remote ANP servers)
CREATE TABLE IF NOT EXISTS anp_auth_tokens (
    id              TEXT PRIMARY KEY,         -- uuid4
    profile         TEXT NOT NULL,            -- Local TAG profile (the authenticating agent)
    remote_did      TEXT NOT NULL,            -- DID of the remote ANP server
    remote_endpoint TEXT NOT NULL,            -- The specific endpoint URL
    token           TEXT NOT NULL,            -- JWT Bearer token (stored encrypted in future; plaintext in v1)
    issued_at       TEXT NOT NULL,            -- ISO-8601 UTC
    expires_at      TEXT NOT NULL,            -- ISO-8601 UTC
    UNIQUE(profile, remote_did, remote_endpoint)
);

CREATE INDEX IF NOT EXISTS idx_aat_profile ON anp_auth_tokens(profile, remote_did);
```

### 10.3 Core Python Dataclasses

```python
# src/tag/anp_identity.py

from __future__ import annotations
from dataclasses import dataclass, field
from typing import Literal, Optional
import datetime


DIDMethod = Literal["did:web", "did:key"]
ProofPurpose = Literal["authentication", "assertionMethod", "capabilityInvocation"]


@dataclass
class VerificationMethod:
    id: str                          # Full DID URL with fragment, e.g. did:web:...:coder#key-1
    type: str                        # "Ed25519VerificationKey2020"
    controller: str                  # The DID that controls this key
    public_key_multibase: str        # Base58btc multibase, prefix 'z'


@dataclass
class DIDService:
    id: str                          # Fragment identifier, e.g. did:...:coder#anp-endpoint
    type: str                        # "ANPEndpoint" | "A2AEndpoint" | "LinkedDomains"
    service_endpoint: str            # HTTPS URL


@dataclass
class DataIntegrityProof:
    type: str = "DataIntegrityProof"
    cryptosuite: str = "eddsa-jcs-2022"
    created: str = ""                # ISO-8601 UTC
    verification_method: str = ""    # Full DID URL to the signing key
    proof_purpose: ProofPurpose = "assertionMethod"
    proof_value: str = ""            # Base58btc multibase Ed25519 signature
    challenge: Optional[str] = None  # For authentication proofs
    domain: Optional[str] = None     # For authentication proofs


@dataclass
class DIDDocument:
    context: list[str] = field(default_factory=lambda: [
        "https://www.w3.org/ns/did/v1",
        "https://w3id.org/security/suites/ed25519-2020/v1",
    ])
    id: str = ""                     # The full DID string
    controller: Optional[str] = None # Defaults to self (same as id)
    verification_method: list[VerificationMethod] = field(default_factory=list)
    authentication: list[str] = field(default_factory=list)     # DID URLs
    assertion_method: list[str] = field(default_factory=list)   # DID URLs
    capability_invocation: list[str] = field(default_factory=list)
    service: list[DIDService] = field(default_factory=list)
    proof: Optional[DataIntegrityProof] = None
    created: str = ""
    updated: str = ""

    def to_json_ld(self) -> dict:
        """Serialize to JSON-LD dict suitable for JCS canonicalization."""
        ...

    @classmethod
    def from_json_ld(cls, data: dict) -> "DIDDocument":
        """Deserialize from JSON-LD dict."""
        ...


@dataclass
class DIDIdentity:
    """Complete identity record stored in the database."""
    row_id: str
    profile: str
    did: str
    method: DIDMethod
    domain: Optional[str]
    did_path: Optional[str]
    key_version: int
    did_document: DIDDocument
    service_endpoint: Optional[str]
    created_at: datetime.datetime
    updated_at: datetime.datetime


@dataclass
class KeyRecord:
    """Key pair record (private key available only after decryption)."""
    row_id: str
    profile: str
    did: str
    key_id: str
    key_version: int
    public_key_multibase: str
    # Private key: decrypted on demand, never stored as attribute
    algorithm: str
    created_at: datetime.datetime
    retired_at: Optional[datetime.datetime]
    revoked: bool


@dataclass
class ResolutionResult:
    did: str
    did_document: DIDDocument
    did_document_metadata: dict
    resolution_metadata: dict     # includes "contentType", "duration_ms", "from_cache"
```

### 10.4 Core Algorithms

#### 10.4.1 did:key Generation

```python
# src/tag/anp_identity.py

import base64
import hashlib
from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey
from cryptography.hazmat.primitives.serialization import (
    Encoding, PublicFormat, PrivateFormat, NoEncryption,
)

# Multicodec prefix for Ed25519 public key: 0xed01 (varint-encoded)
ED25519_MULTICODEC_PREFIX = bytes([0xed, 0x01])
MULTIBASE_BASE58BTC_PREFIX = "z"


def _base58_encode(data: bytes) -> str:
    """Base58btc encoding (Bitcoin alphabet)."""
    alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"
    n = int.from_bytes(data, "big")
    result = []
    while n > 0:
        n, remainder = divmod(n, 58)
        result.append(alphabet[remainder])
    # Leading zero bytes
    for byte in data:
        if byte == 0:
            result.append(alphabet[0])
        else:
            break
    return "".join(reversed(result))


def generate_ed25519_keypair() -> tuple[Ed25519PrivateKey, bytes]:
    """
    Generate Ed25519 key pair.
    Returns (private_key_object, raw_32_byte_public_key).
    """
    private_key = Ed25519PrivateKey.generate()
    public_key_bytes = private_key.public_key().public_bytes(
        Encoding.Raw, PublicFormat.Raw
    )  # 32 bytes
    return private_key, public_key_bytes


def public_key_to_multibase(public_key_bytes: bytes) -> str:
    """
    Encode a 32-byte Ed25519 public key as multibase base58btc string.
    Format: 'z' + base58btc(0xed 0x01 + public_key_bytes)
    """
    prefixed = ED25519_MULTICODEC_PREFIX + public_key_bytes
    return MULTIBASE_BASE58BTC_PREFIX + _base58_encode(prefixed)


def derive_did_key(public_key_bytes: bytes) -> str:
    """Derive a did:key DID from a 32-byte Ed25519 public key."""
    multibase_key = public_key_to_multibase(public_key_bytes)
    return f"did:key:{multibase_key}"


def build_did_web(domain: str, path_components: list[str]) -> str:
    """
    Construct a did:web DID.
    domain: 'agents.mycompany.com'
    path_components: ['agents', 'coder']  -> did:web:agents.mycompany.com:agents:coder
    """
    parts = [domain] + [p.replace("/", ":") for p in path_components]
    return "did:web:" + ":".join(parts)
```

#### 10.4.2 DID Document Signing (JCS + Ed25519)

```python
# src/tag/anp_identity.py

from rfc8785 import dumps as jcs_dumps  # Trail of Bits rfc8785 package


def sign_did_document(
    doc: DIDDocument,
    private_key: Ed25519PrivateKey,
    verification_method_id: str,
    proof_purpose: ProofPurpose = "assertionMethod",
    challenge: str | None = None,
    domain: str | None = None,
) -> DIDDocument:
    """
    Apply DataIntegrityProof (eddsa-jcs-2022) to a DID Document.

    Algorithm (per W3C Data Integrity 1.0 + eddsa-jcs-2022 cryptosuite):
    1. Build the proof options object (without proofValue).
    2. JCS-canonicalize the proof options -> proof_options_canonical.
    3. Remove existing proof field from doc; JCS-canonicalize doc -> doc_canonical.
    4. Compute hash_to_sign = sha256(proof_options_canonical) + sha256(doc_canonical).
       (The eddsa-jcs-2022 cryptosuite concatenates the two SHA-256 digests.)
    5. Sign hash_to_sign with Ed25519.
    6. Attach proof with proofValue = multibase_base58btc(signature).
    """
    import datetime
    now = datetime.datetime.now(datetime.timezone.utc).isoformat(timespec="seconds")

    proof_options = {
        "type": "DataIntegrityProof",
        "cryptosuite": "eddsa-jcs-2022",
        "created": now,
        "verificationMethod": verification_method_id,
        "proofPurpose": proof_purpose,
    }
    if challenge:
        proof_options["challenge"] = challenge
    if domain:
        proof_options["domain"] = domain

    # JCS-canonicalize proof options
    proof_options_canonical = jcs_dumps(proof_options)

    # Build document dict without proof
    doc_dict = doc.to_json_ld()
    doc_dict.pop("proof", None)

    # JCS-canonicalize the document
    doc_canonical = jcs_dumps(doc_dict)

    # Concatenate SHA-256 hashes (eddsa-jcs-2022 spec)
    hash_input = (
        hashlib.sha256(proof_options_canonical).digest()
        + hashlib.sha256(doc_canonical).digest()
    )

    # Sign
    signature_bytes = private_key.sign(hash_input)

    # Encode as multibase base58btc
    proof_value = MULTIBASE_BASE58BTC_PREFIX + _base58_encode(signature_bytes)

    proof = DataIntegrityProof(
        created=now,
        verification_method=verification_method_id,
        proof_purpose=proof_purpose,
        proof_value=proof_value,
        challenge=challenge,
        domain=domain,
    )
    doc.proof = proof
    return doc


def verify_did_document_proof(doc_dict: dict) -> bool:
    """
    Verify a DataIntegrityProof on a DID Document dict.
    Returns True if valid, False if invalid.
    Raises ValueError on missing or malformed proof.
    """
    from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PublicKey
    from cryptography.exceptions import InvalidSignature

    proof = doc_dict.get("proof")
    if not proof:
        raise ValueError("DID Document has no 'proof' field")
    if proof.get("cryptosuite") != "eddsa-jcs-2022":
        raise ValueError(f"Unsupported cryptosuite: {proof.get('cryptosuite')}")

    proof_value_multibase = proof.get("proofValue", "")
    if not proof_value_multibase.startswith(MULTIBASE_BASE58BTC_PREFIX):
        raise ValueError("proofValue must be multibase base58btc (prefix 'z')")

    # Decode signature
    signature_bytes = _base58_decode(proof_value_multibase[1:])

    # Reconstruct proof options (without proofValue)
    proof_options = {k: v for k, v in proof.items() if k != "proofValue"}

    # Reconstruct document without proof
    doc_no_proof = {k: v for k, v in doc_dict.items() if k != "proof"}

    proof_options_canonical = jcs_dumps(proof_options)
    doc_canonical = jcs_dumps(doc_no_proof)

    hash_input = (
        hashlib.sha256(proof_options_canonical).digest()
        + hashlib.sha256(doc_canonical).digest()
    )

    # Resolve the verification method's public key
    vm_id = proof.get("verificationMethod", "")
    public_key_bytes = _resolve_public_key_from_did_doc(doc_dict, vm_id)

    public_key = Ed25519PublicKey.from_public_bytes(public_key_bytes)
    try:
        public_key.verify(signature_bytes, hash_input)
        return True
    except InvalidSignature:
        return False
```

#### 10.4.3 RFC 9421 HTTP Message Signatures (ANP did:wba)

```python
# src/tag/anp_identity.py

import base64
import email.utils
import hashlib
import time
from urllib.parse import urlparse


def build_anp_signed_request(
    method: str,
    url: str,
    body: bytes,
    content_type: str,
    profile_did: str,
    key_id: str,               # Full DID URL with fragment
    private_key: Ed25519PrivateKey,
) -> dict[str, str]:
    """
    Build HTTP headers for an ANP did:wba authenticated request.

    Implements RFC 9421 HTTP Message Signatures with the following
    signed components: @method, @target-uri, @authority,
    content-type, content-digest, date.

    Returns a dict of headers to merge into the outgoing request.
    """
    # Content-Digest: sha-256=:<base64>=
    digest = base64.b64encode(hashlib.sha256(body).digest()).decode()
    content_digest = f'sha-256=:{digest}:'

    # Date: RFC 7231 HTTP-date
    date_str = email.utils.formatdate(usegmt=True)

    parsed = urlparse(url)
    authority = parsed.netloc
    target_uri = url

    # Signature-Input components (RFC 9421 Section 2)
    sig_params = (
        f'("@method" "@target-uri" "@authority" '
        f'"content-type" "content-digest" "date")'
        f';keyid="{key_id}";alg="ed25519";created={int(time.time())}'
    )
    signature_input = f'sig1={sig_params}'

    # Build the signature base string (RFC 9421 Section 2.5)
    sig_base_lines = [
        f'"@method": {method.upper()}',
        f'"@target-uri": {target_uri}',
        f'"@authority": {authority}',
        f'"content-type": {content_type}',
        f'"content-digest": {content_digest}',
        f'"date": {date_str}',
        f'"@signature-params": {sig_params}',
    ]
    sig_base = "\n".join(sig_base_lines)

    # Sign
    sig_bytes = private_key.sign(sig_base.encode("utf-8"))
    sig_b64 = base64.b64encode(sig_bytes).decode()
    signature_header = f'sig1=:{sig_b64}:'

    return {
        "Content-Type": content_type,
        "Content-Digest": content_digest,
        "Date": date_str,
        "Signature-Input": signature_input,
        "Signature": signature_header,
    }


async def anp_wba_authenticate(
    profile: str,
    remote_did: str,
    remote_token_endpoint: str,
    private_key: Ed25519PrivateKey,
    key_id: str,
) -> str:
    """
    Execute the ANP did:wba authentication handshake.

    1. Build a signed HTTP POST to remote_token_endpoint.
    2. The body is a JSON object: {"did": profile_did, "nonce": <uuid4>}.
    3. Send with RFC 9421 signature headers.
    4. Parse the JWT Bearer token from the 200 response.
    5. Cache the token in anp_auth_tokens.

    Returns the JWT Bearer token string.
    """
    import httpx, json, uuid

    body_data = {"did": _get_profile_did(profile), "nonce": str(uuid.uuid4())}
    body = json.dumps(body_data, separators=(",", ":")).encode()
    content_type = "application/json"

    headers = build_anp_signed_request(
        method="POST",
        url=remote_token_endpoint,
        body=body,
        content_type=content_type,
        profile_did=body_data["did"],
        key_id=key_id,
        private_key=private_key,
    )

    async with httpx.AsyncClient(timeout=10.0) as client:
        resp = await client.post(remote_token_endpoint, content=body, headers=headers)
        resp.raise_for_status()
        data = resp.json()
        token = data["access_token"]

    _cache_auth_token(profile, remote_did, remote_token_endpoint, token, data.get("expires_in", 3600))
    return token
```

### 10.5 did:web Resolution Algorithm

```python
def resolve_did_web(did: str, timeout: float = 10.0) -> dict:
    """
    Resolve a did:web DID to its DID Document.

    did:web resolution rules (did:web spec Section 3.1):
      1. Remove 'did:web:' prefix.
      2. Split remaining string on ':'.
      3. First component is the domain (percent-decode it).
      4. Remaining components are URL path segments.
      5. If no path: URL = https://<domain>/.well-known/did.json
      6. If path present: URL = https://<domain>/<path>/did.json
         where each ':' in path becomes '/'
    """
    import httpx
    from urllib.parse import unquote

    assert did.startswith("did:web:"), f"Not a did:web DID: {did}"
    remainder = did[len("did:web:"):]
    parts = remainder.split(":")
    domain = unquote(parts[0])

    if len(parts) == 1:
        url = f"https://{domain}/.well-known/did.json"
    else:
        path = "/".join(unquote(p) for p in parts[1:])
        url = f"https://{domain}/{path}/did.json"

    response = httpx.get(url, timeout=timeout, headers={"Accept": "application/did+json, application/json"})
    response.raise_for_status()
    doc = response.json()

    # Validate that document 'id' matches the resolved DID
    if doc.get("id") != did:
        raise ValueError(
            f"DID Document 'id' mismatch: expected '{did}', got '{doc.get('id')}'"
        )
    return doc
```

### 10.6 Key Encryption / Decryption (Delegated to security.py)

```python
# In anp_identity.py — delegates to existing security.py helpers

from tag.security import encrypt_value, decrypt_value  # AES-256-GCM, existing API


def _store_private_key(
    conn,
    profile: str,
    did: str,
    key_id: str,
    key_version: int,
    public_key_multibase: str,
    private_key: Ed25519PrivateKey,
) -> str:
    """Encrypt and persist a private key. Returns the row uuid."""
    import uuid, base64
    from cryptography.hazmat.primitives.serialization import Encoding, PrivateFormat, NoEncryption

    raw_seed = private_key.private_bytes(Encoding.Raw, PrivateFormat.Raw, NoEncryption())
    ciphertext_b64, nonce_b64 = encrypt_value(raw_seed)  # existing security.py helper

    row_id = str(uuid.uuid4())
    conn.execute(
        """INSERT INTO anp_identity_keys
           (id, profile, did, key_id, key_version, public_key_multibase,
            key_ciphertext, key_nonce, algorithm, created_at, retired_at, revoked)
           VALUES (?,?,?,?,?,?,?,?,'Ed25519',?,NULL,0)""",
        (row_id, profile, did, key_id, key_version, public_key_multibase,
         ciphertext_b64, nonce_b64,
         datetime.datetime.now(datetime.timezone.utc).isoformat(timespec="seconds")),
    )
    conn.commit()
    return row_id


def _load_private_key(conn, profile: str, key_version: int | None = None) -> Ed25519PrivateKey:
    """Load and decrypt the active private key for a profile."""
    from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PrivateKey as _Ed25519

    query = """SELECT key_ciphertext, key_nonce FROM anp_identity_keys
               WHERE profile = ? AND retired_at IS NULL AND revoked = 0
               ORDER BY key_version DESC LIMIT 1"""
    if key_version is not None:
        query = query.replace("retired_at IS NULL AND revoked = 0",
                              f"key_version = {int(key_version)}")

    row = conn.execute(query, (profile,)).fetchone()
    if not row:
        raise KeyError(f"No active key found for profile '{profile}'")

    raw_seed = decrypt_value(row[0], row[1])  # existing security.py helper
    return _Ed25519.from_private_bytes(raw_seed)
```

### 10.7 Integration with Existing TAG API Server (api.py)

Add two routes to the existing `api.py` FastAPI application:

```python
# In api.py — additions only

@app.get("/.well-known/did.json")
async def well_known_did(profile: str = Query(...)):
    """Serve DID Document for did:web resolution."""
    from tag.anp_identity import get_did_document_json
    doc = get_did_document_json(profile)
    if not doc:
        raise HTTPException(404, detail=f"No DID registered for profile '{profile}'")
    return Response(
        content=doc,
        media_type="application/did+json",
        headers={"Cache-Control": "max-age=3600"},
    )


@app.get("/agents/{profile}/did.json")
async def agent_did(profile: str):
    """Alternate resolution URL for did:web."""
    return await well_known_did(profile=profile)
```

### 10.8 Controller Integration (cmd_identity)

```python
# In controller.py — new cmd_identity() dispatch function

def cmd_identity(args):
    """Dispatch handler for `tag identity` subcommands."""
    from tag import anp_identity

    subcmd = args.subcommand  # create | show | resolve | verify | rotate | export | sign | list

    if subcmd == "create":
        return anp_identity.cmd_identity_create(args)
    elif subcmd == "show":
        return anp_identity.cmd_identity_show(args)
    elif subcmd == "resolve":
        return anp_identity.cmd_identity_resolve(args)
    elif subcmd == "verify":
        return anp_identity.cmd_identity_verify(args)
    elif subcmd == "rotate":
        return anp_identity.cmd_identity_rotate(args)
    elif subcmd == "export":
        return anp_identity.cmd_identity_export(args)
    elif subcmd == "sign":
        return anp_identity.cmd_identity_sign(args)
    elif subcmd == "list":
        return anp_identity.cmd_identity_list(args)
    else:
        raise SystemExit(f"Unknown identity subcommand: {subcmd}")
```

---

## 11. Security Considerations

1. **Private key never in plaintext outside memory:** The 32-byte Ed25519 private key seed MUST exist in plaintext only within the Python process heap during active operations. It MUST be encrypted with AES-256-GCM (delegated to `security.py`) before any SQLite write. Logging, tracing, and error message handlers MUST be audited to ensure no accidental serialization of `Ed25519PrivateKey` objects or raw seed bytes.

2. **Key material zeroing after use:** After decrypting and using the private key, the raw seed bytes variable MUST be overwritten with zeros using `ctypes.memset` or equivalent before going out of scope. Python's GC does not guarantee timely memory reclamation. Use `bytearray` (mutable) for the seed to enable explicit zeroing.

3. **Replay attack resistance in proofs:** Authentication proofs for ANP did:wba MUST include a `challenge` (server-issued nonce) and `domain` (intended recipient domain) in the `DataIntegrityProof` to prevent replay attacks. The challenge MUST be single-use: once a proof with a given challenge has been accepted by the remote server, it cannot be reused.

4. **DID Document `id` binding:** On resolution, the `id` field of the fetched DID Document MUST match the DID that was requested (FR-09 enforces this). A mismatch indicates either a misconfigured server or a man-in-the-middle attack attempting to substitute a different identity. This check MUST be performed before returning any document to callers.

5. **HTTPS-only for did:web resolution:** `tag identity resolve` MUST reject `http://` URLs for did:web resolution. The did:web specification requires HTTPS for production deployments to prevent DNS hijacking + HTTP interception attacks. Only `http://localhost` is permitted as an exception for local testing.

6. **Cache poisoning protection:** Cached DID Documents are sourced exclusively from HTTPS responses. Cache entries include the `resolved_at` timestamp and TTL; stale entries MUST NOT be served after TTL expiry even if the network is unavailable (fail-closed, not fail-open). An attacker who gains write access to the SQLite database can inject malicious cached documents — this is in the threat model for database compromise (the same attacker can also access encrypted key material). Defense-in-depth: the SQLite file MUST have mode `0600`.

7. **Verification method binding:** During `verify`, the `verificationMethod` field in the proof MUST reference a key listed in the DID Document's own `verificationMethod` array. Cross-document key references (where the proof points to a key in a different DID Document) MUST be rejected to prevent key confusion attacks.

8. **Profile name injection in DID path:** The `--profile` value is used to construct DID path components (`did:web:domain:agents:<profile>`). Profile names MUST be validated against the pattern `^[a-z0-9][a-z0-9_-]{0,62}[a-z0-9]$` before use in DID construction to prevent injection of `:` characters that would malform the DID or its resolution URL.

9. **JWT token storage:** ANP did:wba JWT Bearer tokens cached in `anp_auth_tokens` are stored as plaintext in v1. These tokens have limited lifetimes (typically 3600 seconds). In v2, these MUST also be AES-256-GCM encrypted at rest. The `anp_auth_tokens` table MUST have `mode 0600` enforced at the file level (existing SQLite file protection applies).

10. **Signing oracle prevention:** The `tag identity sign` command MUST require explicit user confirmation (or `--yes` flag) before signing arbitrary documents. Without this gate, any process with access to the TAG CLI could use the identity key as a signing oracle for arbitrary data, enabling impersonation attacks if the CLI is exposed via automation.

---

## 12. Testing Strategy

### 12.1 Unit Tests (`tests/test_anp_identity.py`)

- **did:key derivation:** Verify that `derive_did_key()` for a known 32-byte public key produces the exact expected DID string. Use the test vector from the did:key specification Section 3.1 (ed25519-x25519 example).
- **did:web construction:** Parameterized tests for `build_did_web()`:
  - `("example.com", [])` → `"did:web:example.com"` (resolution: `/.well-known/did.json`)
  - `("example.com", ["users", "alice"])` → `"did:web:example.com:users:alice"`
  - `("example.com%3A8080", [])` → percent-encoded port in domain
- **DID Document serialization round-trip:** Create a `DIDDocument`, call `to_json_ld()`, call `from_json_ld()`, assert all fields equal the original.
- **DataIntegrityProof sign/verify:** Generate key pair, create minimal DID Document, sign, verify. Assert `verify_did_document_proof()` returns `True`. Flip one byte in `proof.proofValue`, assert returns `False`.
- **Tamper detection:** Modify a field in the document body after signing (e.g., change `id`). Assert `verify_did_document_proof()` returns `False`.
- **JCS field ordering:** Build a dict with keys in non-alphabetical order, assert `jcs_dumps()` output has keys in UTF-16 code unit order. Use the RFC 8785 Appendix B.1 test vector.
- **Key encryption round-trip:** Call `_store_private_key()` and `_load_private_key()` on a test in-memory SQLite DB. Assert the recovered key produces identical signatures as the original.
- **Key rotation:** Create key v1, rotate to v2, assert `_load_private_key()` returns v2 key. Assert v1 key row has non-null `retired_at`. Assert v1 key still loadable by explicit `key_version=1` for the overlap window.
- **Profile name validation:** Assert that profile names containing `:`, `/`, or `..` raise `ValueError` during DID construction.

### 12.2 RFC 8785 Test Vectors (`tests/test_jcs_vectors.py`)

- Load all 42 normative test vectors from RFC 8785 Appendix B as a pytest parametrize fixture.
- For each vector: call `rfc8785.dumps(input_dict)`, assert output bytes match the expected bytes exactly.
- This test file has zero dependencies on TAG internals and runs in under 1 second.

### 12.3 Integration Tests

- **did:web resolution (mocked HTTP):** Use `respx` to mock `https://example.com/.well-known/did.json`. Call `resolve_did_web("did:web:example.com")`. Assert the returned document matches the mock response. Assert `id` mismatch raises `ValueError`.
- **did:web resolution cache:** Resolve the same DID twice. Assert the second call reads from `anp_did_cache` (mock HTTP is not called the second time). Assert `--no-cache` bypasses cache.
- **TTL expiry:** Insert a cache entry with `ttl_seconds=1`, wait 2 seconds, assert next resolve fetches fresh from network.
- **ANP did:wba handshake (mocked):** Mock the remote token endpoint. Call `anp_wba_authenticate()`. Assert the outbound request has valid `Signature-Input` and `Signature` headers. Assert the token is persisted to `anp_auth_tokens`. Assert that a second call within TTL returns the cached token without a new network request.
- **RFC 9421 signature verification (interop):** Use the `http-message-signatures` Python reference implementation to verify the headers produced by `build_anp_signed_request()`.
- **API server DID endpoint:** Start a test FastAPI instance with `httpx.AsyncClient`. Call `GET /.well-known/did.json?profile=test-profile`. Assert `Content-Type: application/did+json` and body matches stored DID Document.

### 12.4 Performance Tests

- **did:key creation:** Benchmark `create_did_key(profile)` including SQLite write. Target: p99 < 100 ms.
- **JCS canonicalization throughput:** Benchmark `jcs_dumps()` on a 100-field document. Target: > 10,000 calls/second.
- **Verification throughput:** Benchmark `verify_did_document_proof()` on a pre-signed document. Target: > 1,000 verifications/second (Ed25519 batch verification is not required in v1).

### 12.5 Security Tests

- **Bit-flip fuzz:** Generate 1,000 random single-bit flips in a valid signed DID Document (targeting the document body, not the proof value). Assert all produce `verify = False`. Target: 0 false positives.
- **Proof value corruption:** Replace `proofValue` with a random 64-byte Ed25519 signature over different data. Assert `verify = False`.
- **Key oracle gate:** Assert that `tag identity sign` without `--yes` prompts for confirmation on a non-TTY by checking exit code and stderr.

---

## 13. Acceptance Criteria

| ID | Criterion |
|----|-----------|
| AC-01 | `tag identity create --profile local-dev --method did:key` completes in under 100 ms, produces a DID of the form `did:key:z6Mk...`, and writes one row each to `anp_did_registry` and `anp_identity_keys`. |
| AC-02 | `tag identity create --profile coder --method did:web --domain agents.example.com` produces a DID of the form `did:web:agents.example.com:agents:coder` and a DID Document with a valid `DataIntegrityProof` of cryptosuite `eddsa-jcs-2022`. |
| AC-03 | `tag identity show --profile coder --format json` outputs valid JSON that passes W3C DID Core JSON-LD schema validation (no `@context` errors, all required fields present). |
| AC-04 | `tag identity verify --did did:web:agents.example.com:agents:coder` (using the document from AC-02, served locally) exits 0 and prints `Result: VERIFIED`. |
| AC-05 | Modifying any field in the DID Document body and rerunning `tag identity verify` exits 2 and prints `Result: VERIFICATION FAILED`. |
| AC-06 | `tag identity resolve did:key:z6MkhaXgBZDvotDkL5257faiztiGiC2QtKLGpbnnEGta2doK` completes without network access and produces a correct DID Document derived from the embedded public key. |
| AC-07 | `tag identity rotate --profile coder` generates a new key, increments `key_version` to 2, produces an updated DID Document signed with the new key, and sets `retired_at` on the old key row in `anp_identity_keys`. |
| AC-08 | After rotation, a signature produced with the old key fails `verify_did_document_proof()` when the DID Document has been updated to reference only the new key. |
| AC-09 | `tag identity export --profile coder --output /tmp/did.json` writes a file that, when placed at `https://agents.example.com/.well-known/did.json`, allows `tag identity resolve did:web:agents.example.com:agents:coder` to succeed and pass `verify`. |
| AC-10 | `GET /.well-known/did.json?profile=coder` on the TAG API server returns `Content-Type: application/did+json`, `Cache-Control: max-age=3600`, and a body identical to the document returned by `tag identity show --profile coder --format json`. |
| AC-11 | `tag identity list` displays all profiles with DIDs in tabular form, with correct DID string, method, key version, and created date for each. |
| AC-12 | All 42 RFC 8785 test vectors pass in `tests/test_jcs_vectors.py` with zero failures. |
| AC-13 | `build_anp_signed_request()` output passes verification by the `http-message-signatures` reference library for all test cases in the ANP did:wba conformance suite. |
| AC-14 | `tag identity resolve <did>` called twice for the same DID within TTL window makes exactly one network request (confirmed by `respx` call count assertion). |
| AC-15 | `tag identity create --profile coder --method did:web` run a second time without `--force` exits 1 with error message `"DID already exists for profile 'coder'. Use --force to overwrite."` |
| AC-16 | Importing `anp_identity` in a fresh Python process adds less than 50 ms to startup time (verified by `python -X importtime` and `time python -c "import tag.anp_identity"`). |
| AC-17 | `tag identity sign --profile coder --input payload.json` without `--yes` in a non-interactive context exits 1 with a message prompting for explicit confirmation. |
| AC-18 | The `anp_did_registry`, `anp_identity_keys`, `anp_did_cache`, and `anp_auth_tokens` tables are created automatically by `open_db()` on first use, with no manual migration step required. |

---

## 14. Dependencies

| Dependency | Type | Version | Notes |
|------------|------|---------|-------|
| `cryptography` | Python package (existing) | >= 43.0.0 | Ed25519 key generation, signing, verification. Already in TAG dependencies for `security.py`. |
| `rfc8785` | Python package (new) | >= 0.1.2 | Trail of Bits JCS canonicalization. Zero dependencies. **Do not use `jcs` (Anders Rundgren) package** — it has a known bug with Number serialization under Python 3.12+. |
| `httpx` | Python package (existing) | >= 0.27.0 | HTTP client for did:web resolution and ANP handshake. Already used by `api.py` and `hermes_bridge.py`. |
| `base58` | Python package (new) | >= 2.1.1 | Base58btc encoding for multibase. Alternative: inline the 20-line alphabet implementation (preferred to avoid dependency for such a small function). |
| `respx` | Python package (dev/test) | >= 0.21.0 | HTTP mocking in tests. Already used by existing test suite. |
| PRD-013 | Internal PRD | — | Agent Tracing / Observability: span emission for identity operations. |
| PRD-028 | Internal PRD | — | Sandbox: key operations run outside sandbox; document this explicitly in `anp_identity.py` module docstring. |
| PRD-034 | Internal PRD | — | Secret Scanning: ensure `anp_identity_keys.key_ciphertext` column is never included in profile exports scanned by secret scanner. Add exclusion rule in `security.py`. |
| PRD-074 | Internal PRD | — | MCP OAuth / PKCE: ANP did:wba is an alternative auth method to OAuth; both must coexist in outbound request pipeline. Coordination needed on `Authorization` header management. |
| W3C DID Core | External spec | 2022-07-19 | https://www.w3.org/TR/did-core/ — normative conformance target for DID Document structure. |
| did:web spec | External spec | 2023-02-14 | https://w3c-ccg.github.io/did-method-web/ — normative conformance target for did:web resolution. |
| did:key spec | External spec | 2023-02-10 | https://w3c-ccg.github.io/did-method-key/ — normative conformance target for did:key derivation. |
| RFC 8785 | External spec | 2020-06 | https://datatracker.ietf.org/doc/html/rfc8785 — JCS canonicalization, normative for proof signing. |
| RFC 9421 | External spec | 2024-02 | https://datatracker.ietf.org/doc/html/rfc9421 — HTTP Message Signatures, normative for ANP did:wba. |
| ANP DID spec | External spec | community | https://agent-network-protocol.com/specs/did-method.html — ANP-specific DID method rules. |

---

## 15. Open Questions

| ID | Question | Owner | Target Resolution |
|----|----------|-------|------------------|
| OQ-01 | Should `tag identity create --method did:web` automatically start the TAG API server on a public port, or should we document a manual Nginx/Caddy config? The UX of requiring users to self-host HTTPS is a significant barrier. | Platform team | Before Phase 1 complete |
| OQ-02 | The ANP did:wba spec requires the remote server to issue a `challenge` nonce before the client signs. The current `anp_wba_authenticate()` design assumes a single-roundtrip flow. Does the ANP conformance test suite require the two-step challenge-response flow? | Protocol research | Before Phase 2 start |
| OQ-03 | Should `tag identity rotate --keep-old-for` default to 3600 seconds or to 0 (immediate revocation)? Keeping old keys available is friendly to long-running sessions but creates a window where both old and new key proofs are valid. | Security team | Phase 2 design review |
| OQ-04 | The `rfc8785` package (Trail of Bits) is preferred over `jcs` (Anders Rundgren) due to Python 3.12 number serialization behavior. Has this been verified against the full 42-vector test suite in Python 3.13 (the planned minimum for TAG v1.0)? | Engineering | Before PR merge |
| OQ-05 | Did:key documents are derived from the public key and are stateless — there is no "update" operation. This means key rotation is incompatible with did:key (the DID would change). Should `tag identity rotate` be blocked for did:key profiles, or should it silently create a new DID? | Protocol design | Phase 1 design review |
| OQ-06 | The W3C DID Core spec defines `created` and `updated` as DID Document metadata (in `didDocumentMetadata`), not as fields inside the document itself. Some implementations embed them directly in the document. Which approach does the ANP ecosystem expect? | Protocol research | Before Phase 1 complete |
| OQ-07 | ANP is described as "community (not yet under Linux Foundation as of June 2026)." Is the spec stable enough to build production implementations against, or should this PRD be marked as experimental pending spec stabilization? | Product | Immediate |
| OQ-08 | Should `anp_auth_tokens` JWT Bearer tokens be encrypted at rest in v1, or is the existing SQLite file-level protection (mode 0600, AES-256 full-disk-encryption assumed) sufficient for initial release? | Security team | Phase 2 security review |

---

## 16. Complexity and Timeline

**Total estimate: 5-7 weeks** (Difficulty 5/5 — requires mastery of W3C DID, RFC 8785, RFC 9421, Ed25519 key management, and careful security review)

### Phase 1: Foundation (Days 1-10)

- [ ] SQLite DDL migration: create `anp_did_registry`, `anp_identity_keys`, `anp_did_cache`, `anp_auth_tokens` tables in `open_db()` bootstrap sequence.
- [ ] Core cryptography: `generate_ed25519_keypair()`, `public_key_to_multibase()`, `derive_did_key()`, `build_did_web()`.
- [ ] Key storage: `_store_private_key()`, `_load_private_key()` using existing `security.py` AES-256-GCM helpers.
- [ ] DID Document dataclass: `DIDDocument`, `VerificationMethod`, `DIDService`, `DataIntegrityProof`, `to_json_ld()`, `from_json_ld()`.
- [ ] `tag identity create` for did:key and did:web.
- [ ] `tag identity show` for text and JSON formats.
- [ ] `tag identity list`.
- [ ] Unit tests for key derivation, DID construction, document serialization.
- [ ] RFC 8785 test vector suite (42 vectors, all passing).

### Phase 2: Signing, Verification, Resolution (Days 11-22)

- [ ] JCS signing: `sign_did_document()` with `eddsa-jcs-2022` cryptosuite, SHA-256 hash concatenation per spec.
- [ ] Proof verification: `verify_did_document_proof()`.
- [ ] `tag identity verify` CLI command with exit code semantics.
- [ ] did:web HTTP resolution: `resolve_did_web()` with HTTPS enforcement, `id` binding check.
- [ ] did:key local resolution: derive DID Document from encoded public key.
- [ ] `tag identity resolve` CLI command with caching.
- [ ] Cache TTL logic in `anp_did_cache`.
- [ ] Integration tests (mocked HTTP, cache hit/miss, TTL expiry).
- [ ] Bit-flip fuzz test (1,000 mutations, zero false positives).
- [ ] Security audit: confirm no key material in logs, traces, error messages.

### Phase 3: ANP did:wba and API Integration (Days 23-32)

- [ ] RFC 9421 HTTP Message Signature builder: `build_anp_signed_request()`.
- [ ] ANP did:wba authentication handshake: `anp_wba_authenticate()` with token caching.
- [ ] RFC 9421 interop test with `http-message-signatures` reference library.
- [ ] TAG API server routes: `/.well-known/did.json` and `/agents/<profile>/did.json`.
- [ ] `tag identity export` command.
- [ ] `tag identity sign` command (with confirmation gate).
- [ ] Key rotation: `tag identity rotate`, `--keep-old-for`, `--prune`.
- [ ] Tracing integration: span emission for all identity operations (PRD-013).
- [ ] Integration with A2A Agent Card service endpoint field (forward-compat, non-blocking).

### Phase 4: Hardening, Documentation, Performance (Days 33-38)

- [ ] Performance benchmarks: p99 targets for all operations.
- [ ] Startup time profiling: lazy import validation, target < 50 ms import overhead.
- [ ] Private key zeroing: `bytearray` + `ctypes.memset` after use.
- [ ] SQLite file mode enforcement: assert `stat(tag.sqlite3).st_mode & 0o777 == 0o600` in `open_db()` bootstrap.
- [ ] CLI help text, man page entries, `tag identity --help` coverage.
- [ ] Resolve open questions OQ-01 through OQ-08.
- [ ] Final security review with PRD-034 (secret scanning) exclusion rules.
- [ ] Acceptance criteria walkthrough: all 18 ACs verified against implementation.

### Milestones

| Date (relative) | Milestone |
|-----------------|-----------|
| Day 10 | Phase 1 complete — did:key and did:web creation working, RFC 8785 vectors passing |
| Day 22 | Phase 2 complete — signing, verification, resolution working; security audit passed |
| Day 32 | Phase 3 complete — ANP did:wba handshake working; API server routes live |
| Day 38 | Phase 4 complete — all ACs verified; ready for PR and code review |

