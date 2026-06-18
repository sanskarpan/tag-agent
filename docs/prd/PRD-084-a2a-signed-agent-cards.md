# PRD-084: A2A Signed Agent Cards (`tag agent-card sign`)

**Status:** Proposed
**Priority:** P2
**Estimated Effort:** M (5-8 days)
**Category:** Multi-Agent Protocols
**Affects:** `agent_card.py + controller.py`
**Depends on:** PRD-081 (A2A agent card publication), PRD-086 (ANP identity layer / W3C DID), PRD-082 (multi-agent team primitives)
**Inspired by:** A2A v1.0 Agent Card spec, RFC 8785 JSON Canonicalization, W3C Verifiable Credentials, OpenID Connect signed JWTs

---

## 1. Overview

PRD-081 introduced `tag agent-card publish` to expose a TAG agent's capabilities at `/.well-known/agent.json` per the A2A v1.0 protocol. However, the published Agent Card is unsigned — any network intermediary can modify it in transit, an attacker can serve a forged card from a compromised server, and a consuming agent has no cryptographic proof that the card was authored by the agent claiming to own it.

A2A Signed Agent Cards (`tag agent-card sign`) adds a JSON Web Signature (JWS, RFC 7515) envelope to the Agent Card, using the agent's identity key (Ed25519 by default) to sign the RFC 8785 canonicalized JSON payload. The resulting signed card is both human-readable (the original JSON is base64url-encoded in the JWS payload) and verifiable by any A2A-compatible agent that holds or can discover the signer's public key. Signature verification is integrated into `tag agent-card verify` and the multi-agent team coordination layer (PRD-082) so that team members verify each other's cards before accepting task delegation.

The design follows the A2A v1.0 specification's `securitySchemes` extension for agent identity, W3C Verifiable Credentials' approach to linked-data signing, and OpenID Connect's signed JWTs for identity assertions. The implementation uses Python's `cryptography` library for Ed25519 operations and RFC 8785's `jcs` (JSON Canonicalization Scheme) for deterministic serialization.

---

## 2. Problem Statement

### 2.1 Agent Cards are unauthenticated

A consuming agent that fetches `/.well-known/agent.json` has no way to verify that the card was authored by the legitimate agent. A MITM attacker can modify the card's capabilities list, insert malicious tool entries, or replace the contact endpoint with a phishing address.

### 2.2 No non-repudiation for capability assertions

When an orchestrating agent delegates a task to a sub-agent based on its card's declared capabilities, there is no cryptographic binding between the capability claim and the key held by the agent. A rogue agent can claim capabilities it does not possess.

### 2.3 Multi-agent team security gap

PRD-082 team primitives assume that agent cards received from discovered agents are authentic. Without signing, a compromised team member can inject a forged card to escalate privileges or redirect task output.

---

## 3. Goals

| ID | Goal |
|----|------|
| G1 | `tag agent-card sign` produces a JWS compact serialization of the agent card signed with the agent's Ed25519 identity key. |
| G2 | `tag agent-card verify <card-file>` verifies the JWS signature against the agent's public key (from local key store or DID document). |
| G3 | The signed card is backward-compatible: the original unsigned JSON is embedded in the JWS payload and can be extracted without verification. |
| G4 | Keys are managed via `tag agent-card keygen` (generates Ed25519 keypair, stores in `~/.tag/keys/agent/`). |
| G5 | Integration with PRD-082 team coordination: team member card verification runs automatically before accepting task delegation. |
| G6 | Support key rotation via `tag agent-card rotate-key`, producing a new keypair and re-signing the card. |
| G7 | The `/.well-known/agent.json` endpoint optionally serves the JWS-signed card when `--signed` is passed to the publish server. |

## 3.1 Non-Goals

| ID | Non-Goal |
|----|----------|
| NG1 | Certificate authority integration or X.509 PKI. Ed25519 self-signed only in this PRD. |
| NG2 | Key escrow or hardware security module (HSM) support. |
| NG3 | Card revocation lists. Key rotation is the mechanism for invalidating old cards. |
| NG4 | Mutual TLS between agents. JWS signing is at the application layer. |
| NG5 | Automatic key distribution via keyserver. Public keys are embedded in the signed card's `kid` header. |

---

## 4. Success Metrics

| Metric | Target | Measurement |
|--------|--------|-------------|
| Sign latency | `tag agent-card sign` completes in < 100ms | Benchmark test |
| Verify latency | `tag agent-card verify` completes in < 50ms | Benchmark test |
| Tamper detection | Modified card body fails verification in 100% of test cases | Unit test |
| Key rotation | New keypair + re-signed card produced in < 500ms | Unit test |
| Backward compatibility | Unsigned cards continue to parse without error | Regression test |

---

## 5. User Stories

| ID | As a... | I want to... | So that... |
|----|---------|-------------|------------|
| US1 | Agent developer | Sign my agent's card with my identity key | Consuming agents can verify my identity |
| US2 | Agent developer | Verify a received agent card's signature | I trust the card's capabilities before delegating tasks |
| US3 | Platform engineer | Have team member cards verified automatically | Team coordination is secure by default |
| US4 | Security engineer | Rotate the agent's identity key | I can respond to key compromise incidents |
| US5 | Agent developer | Serve a signed card from `/.well-known/agent.json` | Clients get an authenticated card |

---

## 6. CLI Surface

```
tag agent-card <subcommand> [options]

Subcommands (new in this PRD):
  keygen      Generate an Ed25519 identity keypair for this agent
  sign        Sign an agent card with the identity key
  verify      Verify a signed agent card's JWS signature
  rotate-key  Generate a new keypair and re-sign the current card
  show-key    Show the public key in PEM and base64url formats

tag agent-card keygen \
  [--key-id NAME]              # defaults to agent name from card
  [--key-path PATH]            # defaults to ~/.tag/keys/agent/

tag agent-card sign \
  [--card-file PATH]           # defaults to ~/.well-known/agent.json
  [--key-path PATH]            # defaults to ~/.tag/keys/agent/
  [--output PATH]              # defaults to <card-file>.jws
  [--algorithm ed25519|es256]

tag agent-card verify \
  <signed-card-file>           # .jws file or JWS compact string
  [--public-key-path PATH]     # override public key location
  [--extract]                  # extract and print the unsigned JSON payload

tag agent-card rotate-key \
  [--card-file PATH]
  [--key-path PATH]
  [--backup / --no-backup]     # backup old keypair (default: backup)

tag agent-card show-key [--format pem|base64url|jwk]

Options:
  --algorithm       Signing algorithm (default: ed25519)
  --key-id NAME     JWS 'kid' header value (default: agent name)
  --card-file PATH  Path to unsigned agent card JSON
  --key-path PATH   Directory containing private_key.pem and public_key.pem
  --output PATH     Output path for signed card
```

---

## 7. Functional Requirements

| ID | Requirement |
|----|------------|
| FR-01 | `tag agent-card keygen` generates an Ed25519 keypair, saves `private_key.pem` and `public_key.pem` to `~/.tag/keys/agent/`, mode 0600/0644 respectively. |
| FR-02 | `tag agent-card sign` reads the agent card JSON, applies RFC 8785 JCS canonicalization, signs the canonical bytes with the private key, and produces a JWS compact serialization (`header.payload.signature`). |
| FR-03 | The JWS header includes `{"alg": "EdDSA", "kid": "<key-id>", "crit": ["b64"], "b64": false}` for detached-payload mode, or standard base64url payload encoding. |
| FR-04 | The JWS payload is the base64url-encoded RFC 8785 canonical JSON of the agent card. |
| FR-05 | `tag agent-card verify` decodes the JWS, extracts the public key (from `kid` header or `--public-key-path`), verifies the Ed25519 signature, and prints OK or INVALID. |
| FR-06 | Verification failure exits with code 1 and prints the error reason (tampered payload / unknown key / expired). |
| FR-07 | `tag agent-card rotate-key` backs up the current keypair to `~/.tag/keys/agent/backup/TIMESTAMP/`, generates a new keypair, and re-signs the current card. |
| FR-08 | PRD-082 team `join` operation calls `agent_card.verify_signed_card()` before accepting a card from a remote agent; rejects unsigned cards in `--strict` mode. |
| FR-09 | `tag agent-card show-key --format jwk` outputs the public key as a JSON Web Key. |
| FR-10 | The signed card file embeds the public key in a `x-public-key` JWS header extension for self-contained verification without a separate key lookup. |

---

## 8. Non-Functional Requirements

| ID | Requirement |
|----|------------|
| NFR-01 | Private key file permissions enforced at write time: mode 0600; warn if world-readable at startup. |
| NFR-02 | `cryptography` library (≥ 41.0) is the only dependency for crypto operations; no OpenSSL CLI wrapping. |
| NFR-03 | JWS verification is constant-time (use `cryptography`'s `Ed25519PublicKey.verify` which is inherently constant-time). |
| NFR-04 | No private key material appears in log output, error messages, or `tag agent-card show-key` output. |
| NFR-05 | All signing operations complete in < 100ms including JCS canonicalization. |

---

## 9. Technical Design

### 9.1 Target files

| File | Change |
|------|--------|
| `src/tag/agent_card.py` | Add `AgentCardSigner`, `AgentCardVerifier`, `KeyManager` classes |
| `src/tag/controller.py` | Add `keygen`, `sign`, `verify`, `rotate-key`, `show-key` to `cmd_agent_card` |

### 9.2 SQLite DDL

```sql
CREATE TABLE IF NOT EXISTS agent_identity_keys (
  id            TEXT PRIMARY KEY,
  key_id        TEXT NOT NULL UNIQUE,
  algorithm     TEXT NOT NULL DEFAULT 'ed25519',
  public_key_b64  TEXT NOT NULL,
  key_path      TEXT NOT NULL,
  is_active     INTEGER NOT NULL DEFAULT 1,
  created_at    TEXT NOT NULL,
  rotated_at    TEXT
);
```

### 9.3 Python core

```python
from __future__ import annotations
import base64
import json
from pathlib import Path
from typing import Optional

try:
    from cryptography.hazmat.primitives.asymmetric.ed25519 import (
        Ed25519PrivateKey, Ed25519PublicKey
    )
    from cryptography.hazmat.primitives.serialization import (
        Encoding, PublicFormat, PrivateFormat, NoEncryption, load_pem_private_key, load_pem_public_key
    )
    from cryptography.exceptions import InvalidSignature
    HAS_CRYPTO = True
except ImportError:
    HAS_CRYPTO = False

def _jcs_canonicalize(obj: dict) -> bytes:
    """RFC 8785 JSON Canonicalization Scheme — sorted keys, no extra whitespace."""
    return json.dumps(obj, sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode("utf-8")

def _b64url(data: bytes) -> str:
    return base64.urlsafe_b64encode(data).rstrip(b"=").decode()

def _b64url_decode(s: str) -> bytes:
    pad = 4 - len(s) % 4
    return base64.urlsafe_b64decode(s + "=" * (pad % 4))

class KeyManager:
    def __init__(self, key_path: Optional[str] = None) -> None:
        self.key_dir = Path(key_path or "~/.tag/keys/agent/").expanduser()

    def keygen(self, key_id: str = "default") -> Ed25519PublicKey:
        if not HAS_CRYPTO:
            raise RuntimeError("pip install cryptography to use agent card signing")
        self.key_dir.mkdir(parents=True, exist_ok=True, mode=0o700)
        priv = Ed25519PrivateKey.generate()
        priv_pem = priv.private_bytes(Encoding.PEM, PrivateFormat.PKCS8, NoEncryption())
        pub_pem = priv.public_key().public_bytes(Encoding.PEM, PublicFormat.SubjectPublicKeyInfo)
        priv_path = self.key_dir / "private_key.pem"
        pub_path = self.key_dir / "public_key.pem"
        priv_path.write_bytes(priv_pem)
        priv_path.chmod(0o600)
        pub_path.write_bytes(pub_pem)
        pub_path.chmod(0o644)
        return priv.public_key()

    def load_private(self) -> Ed25519PrivateKey:
        return load_pem_private_key((self.key_dir / "private_key.pem").read_bytes(), password=None)

    def load_public(self) -> Ed25519PublicKey:
        return load_pem_public_key((self.key_dir / "public_key.pem").read_bytes())

class AgentCardSigner:
    def __init__(self, key_manager: KeyManager) -> None:
        self.km = key_manager

    def sign(self, card: dict, key_id: str = "default") -> str:
        if not HAS_CRYPTO:
            raise RuntimeError("pip install cryptography to use agent card signing")
        priv = self.km.load_private()
        pub = self.km.load_public()
        pub_b64 = _b64url(pub.public_bytes(Encoding.Raw, PublicFormat.Raw))
        header = {"alg": "EdDSA", "kid": key_id, "x-public-key": pub_b64}
        header_b64 = _b64url(json.dumps(header, separators=(",", ":")).encode())
        payload_bytes = _jcs_canonicalize(card)
        payload_b64 = _b64url(payload_bytes)
        signing_input = f"{header_b64}.{payload_b64}".encode()
        signature = priv.sign(signing_input)
        sig_b64 = _b64url(signature)
        return f"{header_b64}.{payload_b64}.{sig_b64}"

class AgentCardVerifier:
    def verify(self, jws: str, public_key_path: Optional[str] = None) -> dict:
        if not HAS_CRYPTO:
            raise RuntimeError("pip install cryptography to use agent card signing")
        parts = jws.strip().split(".")
        if len(parts) != 3:
            raise ValueError("Not a valid JWS compact serialization")
        header_b64, payload_b64, sig_b64 = parts
        header = json.loads(_b64url_decode(header_b64))
        pub_b64 = header.get("x-public-key", "")
        if public_key_path:
            pub = load_pem_public_key(Path(public_key_path).read_bytes())
        elif pub_b64:
            from cryptography.hazmat.primitives.asymmetric.ed25519 import Ed25519PublicKey as _K
            pub = Ed25519PublicKey.from_public_bytes(_b64url_decode(pub_b64))
        else:
            raise ValueError("No public key available for verification")
        signing_input = f"{header_b64}.{payload_b64}".encode()
        try:
            pub.verify(_b64url_decode(sig_b64), signing_input)
        except InvalidSignature:
            raise ValueError("Signature verification FAILED — card may be tampered")
        return json.loads(_b64url_decode(payload_b64))
```

---

## 10. Security Considerations

| Risk | Mitigation |
|------|-----------|
| Private key leakage | Private key stored only in `~/.tag/keys/agent/private_key.pem` mode 0600; never logged |
| JWS algorithm confusion (alg:none attack) | Verify only accepts `alg: EdDSA` or `alg: ES256`; rejects `alg: none` |
| Replay attack (old signed card reused) | Include `iat` (issued-at) timestamp in card JSON; optional `exp` (expiry); consumers enforce freshness |
| Key material in error messages | All exception handlers strip key material before raising |
| MITM on public key exchange | Recommend serving agent cards over HTTPS (TLS); JWS adds application-layer integrity |

---

## 11. Testing Strategy

| Layer | Tests |
|-------|-------|
| Unit | `_jcs_canonicalize` determinism; `AgentCardSigner.sign` produces verifiable JWS; `AgentCardVerifier.verify` rejects tampered payload |
| Security | `alg:none` rejection; modified payload bytes fail verification; wrong key fails verification |
| Integration | Full round-trip: keygen → sign → verify; key rotation → re-sign → verify with new key |

---

## 12. Acceptance Criteria

| ID | Criterion |
|----|----------|
| AC-01 | `tag agent-card keygen` creates `private_key.pem` (mode 0600) and `public_key.pem` (mode 0644) |
| AC-02 | `tag agent-card sign --card-file agent.json` produces a JWS file with three base64url-separated parts |
| AC-03 | `tag agent-card verify agent.json.jws` prints "Signature OK" |
| AC-04 | Modifying one byte of the card JSON and re-verifying prints "INVALID" and exits 1 |
| AC-05 | `tag agent-card rotate-key` backs up old keypair and produces a new signed card |
| AC-06 | `alg: none` in the JWS header is rejected with an error |

---

## 13. Dependencies

| Dependency | Reason |
|-----------|--------|
| PRD-081 A2A agent card publication | Base Agent Card format being signed |
| PRD-082 multi-agent team primitives | Card verification integration point |
| `cryptography` (≥ 41.0) | Ed25519 key generation and JWS signing |

---

## 14. Open Questions

| ID | Question |
|----|---------|
| OQ-01 | Should key IDs be DID:key format for interoperability with PRD-086 DID layer? |
| OQ-02 | Should card expiry (`exp`) be mandatory or optional? |
| OQ-03 | Should the signed card be a detached-payload JWS (card readable without verification) or embedded-payload? |

---

## 15. Complexity & Timeline

**Complexity:** Medium (M)
**Estimated effort:** 5–8 engineer-days

| Phase | Work | Days |
|-------|------|------|
| 1 | `KeyManager` (keygen, load), Ed25519 JCS+JWS signing, unit tests | 2 |
| 2 | `AgentCardVerifier` with tamper detection, security tests | 2 |
| 3 | Key rotation, CLI commands, SQLite key registry | 2 |
| 4 | PRD-082 team integration, documentation | 2 |

