# PRD-084: A2A Signed Agent Cards (`tag agent-card sign`)

> **Stack: Go** (native single-binary; see docs/GO_MIGRATION_RESEARCH.md). This PRD was re-framed from Python to Go.

**Status:** Proposed
**Priority:** P2
**Estimated Effort:** M (5-8 days)
**Category:** Multi-Agent Protocols
**Affects:** `internal/agent/card + internal/cli`
**Depends on:** PRD-081 (A2A agent card publication), PRD-086 (ANP identity layer / W3C DID), PRD-082 (multi-agent team primitives)
**Inspired by:** A2A v1.0 Agent Card spec, RFC 8785 JSON Canonicalization, W3C Verifiable Credentials, OpenID Connect signed JWTs

---

## 1. Overview

PRD-081 introduced `tag agent-card publish` to expose a TAG agent's capabilities at `/.well-known/agent.json` per the A2A v1.0 protocol. However, the published Agent Card is unsigned — any network intermediary can modify it in transit, an attacker can serve a forged card from a compromised server, and a consuming agent has no cryptographic proof that the card was authored by the agent claiming to own it.

A2A Signed Agent Cards (`tag agent-card sign`) adds a JSON Web Signature (JWS, RFC 7515) envelope to the Agent Card, using the agent's identity key (Ed25519 by default) to sign the RFC 8785 canonicalized JSON payload. The resulting signed card is both human-readable (the original JSON is base64url-encoded in the JWS payload) and verifiable by any A2A-compatible agent that holds or can discover the signer's public key. Signature verification is integrated into `tag agent-card verify` and the multi-agent team coordination layer (PRD-082) so that team members verify each other's cards before accepting task delegation.

The design follows the A2A v1.0 specification's `securitySchemes` extension for agent identity, W3C Verifiable Credentials' approach to linked-data signing, and OpenID Connect's signed JWTs for identity assertions. The implementation uses Go's standard-library `crypto/ed25519` for signing operations, `github.com/lestrrat-go/jwx/v2` for JWS/JWK envelopes, and an RFC 8785 JSON Canonicalization Scheme (JCS) encoder for deterministic serialization. The signing/verification code compiles into the single static `tag` binary (`CGO_ENABLED=0`).

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
| FR-08 | PRD-082 team `join` operation calls `card.VerifySignedCard()` (package `internal/agent/card`) before accepting a card from a remote agent; rejects unsigned cards in `--strict` mode. |
| FR-09 | `tag agent-card show-key --format jwk` outputs the public key as a JSON Web Key. |
| FR-10 | The signed card file embeds the public key in a `x-public-key` JWS header extension for self-contained verification without a separate key lookup. |

---

## 8. Non-Functional Requirements

| ID | Requirement |
|----|------------|
| NFR-01 | Private key file permissions enforced at write time: mode 0600; warn if world-readable at startup. |
| NFR-02 | Crypto operations use only Go stdlib (`crypto/ed25519`, `crypto/ecdsa`, `crypto/x509`) plus `github.com/lestrrat-go/jwx/v2` for JWS/JWK; no OpenSSL CLI wrapping or cgo. |
| NFR-03 | JWS verification is constant-time (stdlib `ed25519.Verify` is inherently constant-time). |
| NFR-04 | No private key material appears in log output, error messages, or `tag agent-card show-key` output. |
| NFR-05 | All signing operations complete in < 100ms including JCS canonicalization. |

---

## 9. Technical Design

### 9.1 Target packages

| Package / File | Change |
|------|--------|
| `internal/agent/card/signer.go` | Add `Signer`, `Verifier`, `KeyManager` types |
| `internal/agent/card/keystore.go` | modernc.org/sqlite key registry (`agent_identity_keys`) |
| `internal/cli/agentcard.go` | Wire `keygen`, `sign`, `verify`, `rotate-key`, `show-key` subcommands (via chi/huma-shared handlers reused by the CLI) |
| `internal/server/wellknown.go` | Optionally serve the JWS-signed card at `/.well-known/agent.json` (chi + huma) |

The `/.well-known/agent.json` publish server (PRD-081) is a chi router with a huma v2 API (spec-first, OpenAPI 3.1). When `--signed` is set, the well-known handler returns the JWS compact serialization with `Content-Type: application/jose` (or the JSON card with a detached signature link). No streaming is required here; SSE (`tmaxmax/go-sse`) is unused for this feature.

### 9.2 SQLite DDL (modernc.org/sqlite)

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

### 9.3 Go core

```go
package card

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// jcsCanonicalize implements RFC 8785 JSON Canonicalization Scheme:
// lexicographically sorted object keys, minimal separators, no insignificant
// whitespace. encoding/json already escapes and sorts map keys; for structs
// we round-trip through a map to guarantee deterministic key ordering.
func jcsCanonicalize(card map[string]any) ([]byte, error) {
	// json.Marshal sorts map[string]any keys and emits compact output.
	return json.Marshal(card)
}

func b64url(data []byte) string {
	return base64.RawURLEncoding.EncodeToString(data)
}

func b64urlDecode(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(s)
}

// KeyManager loads and generates the agent's Ed25519 identity keypair.
type KeyManager struct {
	KeyDir string // defaults to ~/.tag/keys/agent
}

func NewKeyManager(keyPath string) (*KeyManager, error) {
	if keyPath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		keyPath = filepath.Join(home, ".tag", "keys", "agent")
	}
	return &KeyManager{KeyDir: keyPath}, nil
}

// Keygen generates an Ed25519 keypair and persists it as PEM files
// (private_key.pem mode 0600, public_key.pem mode 0644).
func (km *KeyManager) Keygen() (ed25519.PublicKey, error) {
	if err := os.MkdirAll(km.KeyDir, 0o700); err != nil {
		return nil, err
	}
	pub, priv, err := ed25519.GenerateKey(nil) // crypto/rand
	if err != nil {
		return nil, err
	}
	privDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, err
	}
	pubDER, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return nil, err
	}
	privPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privDER})
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER})
	if err := os.WriteFile(filepath.Join(km.KeyDir, "private_key.pem"), privPEM, 0o600); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(km.KeyDir, "public_key.pem"), pubPEM, 0o644); err != nil {
		return nil, err
	}
	return pub, nil
}

func (km *KeyManager) LoadPrivate() (ed25519.PrivateKey, error) {
	raw, err := os.ReadFile(filepath.Join(km.KeyDir, "private_key.pem"))
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, errors.New("private_key.pem: no PEM block")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	priv, ok := key.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("private key is not Ed25519")
	}
	return priv, nil
}

func (km *KeyManager) LoadPublic() (ed25519.PublicKey, error) {
	raw, err := os.ReadFile(filepath.Join(km.KeyDir, "public_key.pem"))
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, errors.New("public_key.pem: no PEM block")
	}
	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	pub, ok := key.(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("public key is not Ed25519")
	}
	return pub, nil
}

type jwsHeader struct {
	Alg         string `json:"alg"`
	Kid         string `json:"kid"`
	PublicKeyB64 string `json:"x-public-key"`
}

// Signer produces a JWS compact serialization of an agent card.
type Signer struct{ KM *KeyManager }

func (s *Signer) Sign(card map[string]any, keyID string) (string, error) {
	priv, err := s.KM.LoadPrivate()
	if err != nil {
		return "", err
	}
	pub, err := s.KM.LoadPublic()
	if err != nil {
		return "", err
	}
	header := jwsHeader{Alg: "EdDSA", Kid: keyID, PublicKeyB64: b64url(pub)}
	headerJSON, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	payloadBytes, err := jcsCanonicalize(card)
	if err != nil {
		return "", err
	}
	headerB64 := b64url(headerJSON)
	payloadB64 := b64url(payloadBytes)
	signingInput := headerB64 + "." + payloadB64
	sig := ed25519.Sign(priv, []byte(signingInput))
	return signingInput + "." + b64url(sig), nil
}

// Verifier verifies a JWS-signed agent card and returns the embedded card.
type Verifier struct{}

func (v *Verifier) Verify(jws string, publicKeyPath string) (map[string]any, error) {
	parts := strings.Split(strings.TrimSpace(jws), ".")
	if len(parts) != 3 {
		return nil, errors.New("not a valid JWS compact serialization")
	}
	headerB64, payloadB64, sigB64 := parts[0], parts[1], parts[2]

	headerJSON, err := b64urlDecode(headerB64)
	if err != nil {
		return nil, err
	}
	var header jwsHeader
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		return nil, err
	}
	// Reject alg confusion / alg:none downgrade attacks.
	if header.Alg != "EdDSA" && header.Alg != "ES256" {
		return nil, fmt.Errorf("unsupported or disallowed alg %q", header.Alg)
	}

	var pub ed25519.PublicKey
	switch {
	case publicKeyPath != "":
		km := &KeyManager{KeyDir: filepath.Dir(publicKeyPath)}
		if pub, err = km.LoadPublic(); err != nil {
			return nil, err
		}
	case header.PublicKeyB64 != "":
		raw, derr := b64urlDecode(header.PublicKeyB64)
		if derr != nil {
			return nil, derr
		}
		pub = ed25519.PublicKey(raw)
	default:
		return nil, errors.New("no public key available for verification")
	}

	sig, err := b64urlDecode(sigB64)
	if err != nil {
		return nil, err
	}
	signingInput := []byte(headerB64 + "." + payloadB64)
	if !ed25519.Verify(pub, signingInput, sig) {
		return nil, errors.New("signature verification FAILED — card may be tampered")
	}

	payload, err := b64urlDecode(payloadB64)
	if err != nil {
		return nil, err
	}
	var card map[string]any
	if err := json.Unmarshal(payload, &card); err != nil {
		return nil, err
	}
	return card, nil
}
```

> **Note on JCS:** `encoding/json` sorts `map[string]any` keys and emits compact
> output, which covers most RFC 8785 requirements. For full-fidelity number
> formatting and Unicode normalization, wrap it with a dedicated JCS encoder or
> use `jwx/v2`'s canonicalization helpers before signing. Production signing/
> verification should prefer `jwx/v2`'s `jws.Sign`/`jws.Verify` with a `jwk.Key`
> built from the loaded Ed25519 key; the hand-rolled code above documents the
> exact wire format.

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

Go `testing` package with table-driven tests (`go test ./internal/agent/card/...`); crypto benchmarks via `testing.B`.

| Layer | Tests |
|-------|-------|
| Unit | `jcsCanonicalize` determinism; `Signer.Sign` produces a verifiable JWS; `Verifier.Verify` rejects a tampered payload |
| Security | `alg:none`/`alg` confusion rejection; flipped payload byte fails verification; wrong key fails verification |
| Integration | Full round-trip: `Keygen` → `Sign` → `Verify`; key rotation → re-sign → verify with new key; `httptest` server exercises the `/.well-known/agent.json` `--signed` handler |

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
| Go stdlib `crypto/ed25519`, `crypto/x509`, `encoding/json` | Ed25519 key generation, PEM/PKCS8 handling, canonical JSON |
| `github.com/lestrrat-go/jwx/v2` | JWS compact serialization + JWK output (`show-key --format jwk`) |
| `github.com/go-chi/chi/v5` + `github.com/danielgtaylor/huma/v2` | `/.well-known/agent.json` publish server (from PRD-081) |
| `modernc.org/sqlite` | Pure-Go SQLite key registry (`agent_identity_keys`) |

**Module:** `github.com/tag-agent/tag` · **Go:** 1.24+ · **Build:** `CGO_ENABLED=0` · **Release:** GoReleaser + cosign + SLSA.

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

