# PRD-097: Sandbox-Level Secrets Injection via Encrypted Vault (`tag sandbox secret`)

**Status:** Proposed
**Priority:** P3
**Estimated Effort:** M (1-2 weeks)
**Category:** Sandbox & Execution Environment
**Affects:** `sandbox.py + secrets_vault.py` (new)
**Depends on:** PRD-028 (Sandbox Code Execution), PRD-034 (Secret Scanning), PRD-013 (Agent Tracing / Observability), PRD-008 (Background Task Queue)
**GitHub Issue:** #348
**Inspired by:** Modal secrets, E2B environment variables, HashiCorp Vault

---

## 1. Overview

TAG's sandbox subsystem (PRD-028) provides process isolation via Docker, E2B, Modal, and restricted-subprocess backends. However, it has no first-class mechanism for delivering secrets to sandbox workloads. Today, users who need an `OPENAI_API_KEY` inside a sandboxed code execution have three bad options: embed the key in `--code` (leaks into `sandbox_runs.command`), pass it via `--env KEY=value` on the CLI (leaks into shell history and process lists), or hardcode it in the agent's system prompt (leaks into traces and profile exports). All three paths violate the security guarantees that the sandbox itself is meant to provide.

This PRD specifies `tag sandbox secret`: a local, encrypted secrets vault integrated tightly with the sandbox execution layer. Secrets are stored in an AES-256-GCM encrypted SQLite database at `~/.tag/vault/vault.db`, separate from the main `tag.sqlite3` state store. At sandbox invocation time, named secrets are decrypted in memory, injected into the sandbox environment (as environment variables or ephemeral files), and immediately zeroed after the process exits. The plaintext value never touches disk outside the vault, never appears in `sandbox_runs`, never appears in audit logs, and never appears in any TAG config file.

The design draws directly from three production systems. Modal Secrets stores key-value maps encrypted at rest, exposes them to function invocations via a `secrets=[Secret.from_name("my-secret")]` parameter, and never logs values. E2B environment variables are injected at sandbox creation time into the Firecracker microVM, so the host process never exposes them as CLI arguments. HashiCorp Vault's model of explicit `vault read secret/myapp/api-key` → in-process decryption → injected into `subprocess.env` without writing to disk is the procedural template for the vault client in this PRD. TAG adopts all three patterns: named secrets, injection at creation time, and in-process-only decryption.

The vault key derivation is user-space and deterministic: a master key is derived from a user-supplied passphrase via Argon2id (the OWASP-recommended KDF for password-based key derivation as of 2024) and stored in the OS keychain (macOS Keychain, Linux libsecret, Windows Credential Manager) via the `keyring` Python library. When `keyring` is unavailable, the derived key is cached in `~/.tag/vault/.key` with mode `0600`. Each secret ciphertext includes its own random 96-bit nonce and 128-bit authentication tag per AES-256-GCM, making the vault tamper-evident at the per-secret level.

Audit trails are written to a separate append-only `secret_audit` table in `tag.sqlite3`. Every access event — `add`, `get`, `list`, `delete`, `inject` — is recorded with timestamp, secret name (never value), operation, invoking sandbox run ID (if applicable), and the OS user. This gives platform engineers and security teams a complete access log without storing any plaintext material.

---

## 2. Problem Statement

### 2.1 Secrets Leak Through Sandbox Run Records

`sandbox.py:ensure_schema()` creates a `sandbox_runs` table with a `command TEXT NOT NULL` column. When a user today passes credentials as environment variables via `--env OPENAI_KEY=sk-...`, those values are captured in the command string or in the process environment visible via `/proc/<pid>/environ` on Linux. The `sandbox_runs.output` column also captures full stdout, meaning any debug print of `os.environ` inside sandboxed code permanently embeds the key in the database. PRD-034 (Secret Scanning) gates profile export but has no coverage of the `sandbox_runs` table.

### 2.2 No Secure Handoff Between Agent Loop and Sandbox

The agent loop in `controller.py` generates code dynamically. If an agent needs to call an external API, it must either hardcode the key into the generated code (which then appears in `sandbox_runs.command`) or the user must pre-inject it via a mechanism that does not exist. The gap is structural: there is a secure vault in principle (the OS keychain, `.env` files) and there is a secure execution environment (the sandbox), but there is no bridge between them that maintains the security invariant across the boundary.

### 2.3 Audit-Free Secret Usage in Agent Workflows

TAG supports multi-step agent workflows (queue jobs, swarm agents, DAG pipelines). When a secret like `DATABASE_URL` is used in step 3 of a 10-step DAG, there is currently no record of which step used it, when, or under which run ID. If a key is later found to have been compromised, there is no forensic trail to determine which TAG jobs had access to it. This is a compliance gap in enterprise deployments.

---

## 3. Goals

| ID | Goal |
|----|------|
| G1 | Store named secrets in an AES-256-GCM encrypted local vault; plaintext never written to any TAG config file, `sandbox_runs`, or log. |
| G2 | Inject secrets into sandbox workloads as environment variables or ephemeral temp files at runtime, with zero-on-exit memory clearing for in-process buffers. |
| G3 | `tag sandbox secret list` shows only names and metadata; values are never displayed in any list or table output. |
| G4 | Derive the vault master key from a user passphrase via Argon2id; store derived key in OS keychain via `keyring`; fall back to `~/.tag/vault/.key` (mode 0600) when keychain is unavailable. |
| G5 | Write a tamper-evident audit record for every secret operation (add, delete, inject, list) to `secret_audit` in `tag.sqlite3`. |
| G6 | `tag sandbox run --secret NAME` syntax binds a named vault secret to the sandbox invocation; multiple `--secret` flags are supported. |
| G7 | `tag sandbox secret inject-file --name DB_CERT --mount /run/secrets/db.crt` delivers a secret as an ephemeral read-only file inside the sandbox (Docker and E2B backends). |
| G8 | Secrets survive across shell sessions; the vault persists at `~/.tag/vault/vault.db`. |
| G9 | `tag sandbox secret rotate --name OPENAI_KEY` re-encrypts a secret with a fresh nonce without exposing the plaintext value. |
| G10 | Zero mandatory new binary dependencies; `cryptography` (already a transitive dependency of many Python packages) is the only new Python import; `keyring` is optional. |

---

## 4. Non-Goals

| ID | Non-Goal |
|-----|----------|
| NG1 | Integration with remote secret managers (HashiCorp Vault server, AWS Secrets Manager, GCP Secret Manager). The vault is local only. A future PRD may add remote backend adapters. |
| NG2 | Secret sharing between users or machines. The vault is single-user, single-machine. |
| NG3 | Multi-passphrase or role-based access control within the local vault. One passphrase unlocks all secrets. |
| NG4 | Automatic secret rotation from external providers (e.g., rotating an AWS key in the AWS console and pulling the new value). |
| NG5 | Secret versioning with rollback (store only the current value per name). |
| NG6 | Secrets injection into non-sandbox TAG commands (e.g., `tag run`, `tag agent`, `tag eval`). Vault secrets are sandbox-scoped in v1. |
| NG7 | A TUI or web UI for the vault. All management is CLI-only in v1. |
| NG8 | Cross-sandbox secret sharing (one sandbox reading another's injected secrets). |

---

## 5. Success Metrics

| Metric | Target | Measurement Method |
|--------|--------|--------------------|
| Zero plaintext in `sandbox_runs` | No secret value appears in `command`, `output`, or `error` columns after injection | Automated test: `grep` vault values against DB after 50 injection runs |
| Inject latency overhead | < 5 ms added to sandbox startup time per injected secret | Benchmark 100 `sandbox run --secret` invocations, measure delta vs baseline |
| Audit completeness | 100% of secret operations produce a `secret_audit` row | Integration test: perform add/list/delete/inject, verify row count == operation count |
| Vault size | 10,000 secrets stored and retrieved in < 50 ms | SQLite benchmark with 10k rows |
| Key derivation time | Argon2id KDF completes in 200–500 ms on reference hardware (M-series Mac, c5.xlarge) | Timed unit test |
| Keychain integration | On macOS, derived key is stored in and retrieved from Keychain without user prompt after first unlock | Manual verification test on macOS 14+ |
| Memory zero-on-exit | Decrypted secret bytes are zeroed before `run_sandbox()` returns | Unit test with `ctypes` memory inspection post-call |

---

## 6. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|-----------|----------|
| U1 | Developer | Run `tag sandbox secret add --name OPENAI_KEY --value sk-...` once and never type the key again | My key is stored securely and usable in any sandbox run without shell history exposure |
| U2 | Developer | Run `tag sandbox run --code "import openai; ..." --secret OPENAI_KEY` | The sandboxed code receives `OPENAI_KEY` as an env var without the key appearing in logs, shell history, or the SQLite audit table |
| U3 | Security engineer | Run `tag sandbox secret list` | I can see which secrets exist in the vault without any values being displayed |
| U4 | Developer | Run `tag sandbox secret delete OPENAI_KEY` | The secret is permanently removed from the vault and a deletion audit event is recorded |
| U5 | Platform engineer | Query `SELECT * FROM secret_audit WHERE name = 'DB_URL'` | I can see every sandbox run that received the `DB_URL` secret, with timestamps and run IDs, for compliance forensics |
| U6 | Developer | Run `tag sandbox secret inject-file --name TLS_CERT --mount /run/secrets/tls.crt` in a Docker sandbox | The cert is available at `/run/secrets/tls.crt` inside the container without being a mount of a host plaintext file |
| U7 | Developer | Run `tag sandbox secret rotate --name OPENAI_KEY` | My secret is re-encrypted with a fresh nonce; the plaintext value is never re-displayed |
| U8 | Developer | Run `tag sandbox secret export --name OPENAI_KEY --to-env` | The key is printed once to stdout for one-time use in a script, with a warning that the value is now visible |
| U9 | DevOps engineer | Set `TAG_VAULT_PASSPHRASE` in CI environment | Vault unlocks non-interactively in CI pipelines that need secrets injected into sandbox test runs |
| U10 | Developer | Run `tag sandbox secret add --name DB_URL --from-env DATABASE_URL` | The current value of `DATABASE_URL` from my shell environment is captured into the vault without me typing it into the terminal |

---

## 7. Proposed CLI Surface

All secret management subcommands live under the `tag sandbox secret` namespace. Sandbox run integration is via the `--secret` flag on `tag sandbox run`.

### 7.1 `tag sandbox secret add`

Store a new secret in the encrypted vault.

```
tag sandbox secret add \
  --name <NAME> \
  [--value <VALUE>] \
  [--from-env <ENV_VAR>] \
  [--from-file <PATH>] \
  [--description <TEXT>] \
  [--tags <k=v,...>]
```

- `--name`: Identifier used to reference the secret in `--secret` flags. Must match `[A-Z][A-Z0-9_]*` (POSIX env var convention). Required.
- `--value`: Plaintext value. Mutually exclusive with `--from-env` and `--from-file`. If none of the three are provided, the CLI prompts interactively using `getpass.getpass()` so the value is never echoed.
- `--from-env VAR`: Read the current value of environment variable `VAR` and store it. Useful in CI.
- `--from-file PATH`: Read the first 65536 bytes of `PATH` and store as the secret value. Useful for certificates, private keys, JSON service account files.
- `--description`: Human-readable description stored in the vault metadata (plaintext, not encrypted; do not put sensitive data here).
- `--tags`: Comma-separated `key=value` pairs stored as JSON in vault metadata for filtering.

**Example output:**
```
Vault unlocked.
Secret 'OPENAI_KEY' stored (32 bytes, AES-256-GCM, nonce: 9f3a...).
Audit event written: add @ 2026-06-17T09:14:33Z
```

**Error cases:**
- If `--name` already exists: `Error: secret 'OPENAI_KEY' already exists. Use --overwrite to replace.`
- If `--value` length exceeds 65536 bytes: `Error: secret value exceeds 64 KiB limit.`

### 7.2 `tag sandbox secret list`

List all secrets in the vault. Values are never shown.

```
tag sandbox secret list [--json] [--tags <k=v,...>]
```

**Example tabular output:**
```
NAME             DESCRIPTION              SIZE    CREATED              LAST_USED
OPENAI_KEY       OpenAI API key           32 B    2026-06-17 09:14     2026-06-17 11:02
DATABASE_URL     Staging DB connection    78 B    2026-06-15 14:30     2026-06-16 08:55
TLS_CERT         mTLS client cert         2.3 KB  2026-06-10 10:00     never
```

**JSON output** (`--json`):
```json
[
  {
    "name": "OPENAI_KEY",
    "description": "OpenAI API key",
    "size_bytes": 32,
    "created_at": "2026-06-17T09:14:33Z",
    "last_used_at": "2026-06-17T11:02:17Z",
    "tags": {}
  }
]
```

### 7.3 `tag sandbox secret delete`

Permanently delete a secret from the vault.

```
tag sandbox secret delete <NAME> [--yes]
```

- Prompts for confirmation unless `--yes` is passed.
- Writes a `delete` audit event.
- Overwrites the ciphertext storage row with zeros before deletion (defensive).

**Example:**
```
Delete secret 'OPENAI_KEY'? This cannot be undone. [y/N] y
Secret 'OPENAI_KEY' deleted. Audit event written.
```

### 7.4 `tag sandbox secret rotate`

Re-encrypt an existing secret with a fresh random nonce. The plaintext value is never displayed.

```
tag sandbox secret rotate --name <NAME>
```

Decrypts with the current nonce, re-encrypts with a new 96-bit random nonce, updates the vault row. Writes a `rotate` audit event.

**Example:**
```
Rotating 'OPENAI_KEY'...
Re-encrypted with new nonce (9a1b...). Audit event written.
```

### 7.5 `tag sandbox secret export`

One-time export of a secret value to stdout (use sparingly). Writes an `export` audit event with a warning marker.

```
tag sandbox secret export --name <NAME> [--to-env]
```

- Without `--to-env`: prints raw value to stdout.
- With `--to-env`: prints `export NAME=VALUE` suitable for `eval $(tag sandbox secret export --name FOO --to-env)`.

**Warning displayed on stderr:**
```
WARNING: Secret value printed to stdout. This appears in your terminal scrollback and may be captured by terminal logging.
Audit event: export (WARN) @ 2026-06-17T09:20:11Z
```

### 7.6 `tag sandbox secret inject-file`

Register a secret for file-mode injection. Associates a secret name with a target mount path inside the sandbox.

```
tag sandbox secret inject-file \
  --name <NAME> \
  --mount <CONTAINER_PATH> \
  [--mode <octal>]
```

This is a metadata operation; actual injection happens at `tag sandbox run` time when `--secret NAME` is also passed. The mount path and mode are stored in vault metadata.

**Example:**
```
Secret 'TLS_CERT' will be injected as /run/secrets/tls.crt (mode 0400) in Docker/E2B runs.
```

### 7.7 `tag sandbox run` (extended)

The existing `tag sandbox run` command gains the `--secret` flag:

```
tag sandbox run \
  --code "import os; print(os.environ['OPENAI_KEY'][:5])" \
  --secret OPENAI_KEY \
  [--secret DATABASE_URL] \
  [--runtime docker] \
  [--image python:3.12]
```

- `--secret NAME`: Decrypt the named vault secret and inject it as an environment variable into the sandbox. Repeatable. If the secret has a registered `inject-file` mount path, it is delivered as an ephemeral file instead of an env var.
- Multiple `--secret` flags are additive.
- Secret names that do not exist in the vault produce a hard error before sandbox startup.
- The `sandbox_runs.command` column stores the command template with `--secret NAME` tokens but never the resolved values.

**Example output:**
```
Vault unlocked.
Injecting 2 secret(s): OPENAI_KEY (env), TLS_CERT (file: /run/secrets/tls.crt)
Sandbox [docker] starting...
sk-an  ← (truncated output from the code)
Exit code: 0
Duration: 1.24s
Audit: 2 secrets injected into run sandbox-run-8f3a2c
```

### 7.8 `tag sandbox secret audit`

Display the audit trail for vault operations.

```
tag sandbox secret audit \
  [--name <NAME>] \
  [--op <add|delete|inject|list|export|rotate>] \
  [--since <ISO8601>] \
  [--last <N>] \
  [--json]
```

**Example tabular output:**
```
TIMESTAMP              OP       NAME         RUN_ID              USER
2026-06-17 11:02:17    inject   OPENAI_KEY   sandbox-run-8f3a2c  sanskar
2026-06-17 09:14:33    add      OPENAI_KEY   —                   sanskar
2026-06-16 08:55:01    inject   DATABASE_URL sandbox-run-2b1d9e  sanskar
2026-06-15 14:30:11    add      DATABASE_URL —                   sanskar
```

---

## 8. Functional Requirements

| ID | Requirement | Testable Condition |
|----|-------------|-------------------|
| FR-01 | Vault database created at `~/.tag/vault/vault.db` on first `secret add`, with file mode 0600. | `os.stat(vault_path).st_mode & 0o777 == 0o600` after first add |
| FR-02 | Each secret ciphertext uses AES-256-GCM with a unique 96-bit random nonce per secret per write. | Read two rows; assert nonces differ. Assert nonce length == 12 bytes. |
| FR-03 | Vault master key is derived via Argon2id with at minimum: time_cost=3, memory_cost=65536 (64 MiB), parallelism=1, hash_len=32. | Unit test verifies `argon2.low_level.hash_secret_raw` called with these parameters |
| FR-04 | On macOS/Linux/Windows with `keyring` installed, derived key is stored in and retrieved from the OS keychain under service name `tag-vault`. | Integration test: store key, restart process, retrieve without re-prompting passphrase |
| FR-05 | If `keyring` is unavailable, derived key is written to `~/.tag/vault/.key` with mode 0600 and a warning printed. | Unit test with mocked ImportError for keyring; assert file created with correct mode |
| FR-06 | Passphrase can be supplied via `TAG_VAULT_PASSPHRASE` environment variable for CI/non-interactive use; when set, no passphrase prompt is issued. | Test: set env var, call `secret add` without TTY; assert no prompt |
| FR-07 | `secret add` rejects names that do not match `[A-Z][A-Z0-9_]*`; error message cites the naming constraint. | `secret add --name my-key` exits 1 with pattern-mismatch message |
| FR-08 | `secret add` rejects values exceeding 65536 bytes; the value is zeroed from memory before the error is raised. | Test with 65537-byte value; assert exit 1 and no vault row created |
| FR-09 | `secret list` output never contains any substring that matches any stored ciphertext or plaintext value. | Store known value, capture `list` stdout, assert value not present |
| FR-10 | `secret delete` overwrites the `ciphertext` column with `NULL` or zero bytes before executing `DELETE`. | Read row after failed delete (simulate); assert ciphertext is zeroed in the UPDATE |
| FR-11 | `tag sandbox run --secret NAME` injects the secret into the sandbox process environment exclusively; the value does not appear in `sandbox_runs.command`, `sandbox_runs.output`, or `sandbox_runs.error`. | Integration test: print secret value inside sandbox; grep `sandbox_runs` for the known value |
| FR-12 | In Docker backend, secrets are injected via `docker run --env NAME=VALUE` where VALUE is the decrypted string, never written to any intermediate file. | Inspect subprocess call args; assert no temp file written to disk |
| FR-13 | In file-injection mode, the secret is written to a named temporary file, mounted read-only into the container (`--volume /tmp/tag-secret-XXXX:/run/secrets/name:ro`), and the temp file is unlinked after `docker run` returns. | Assert temp file does not exist after run; assert file was read-only inside container |
| FR-14 | E2B backend injects secrets via `Sandbox.create(envs={...})` parameter, never via `sandbox.commands.run("export ...")`. | Unit test with mocked E2B SDK; assert `envs` kwarg populated, no export command issued |
| FR-15 | Modal backend injects secrets via `modal.Secret.from_dict({...})` passed to the Modal function invocation. | Unit test with mocked Modal SDK; assert `secrets` param populated |
| FR-16 | Restricted subprocess backend injects secrets by passing a filtered `env` dict to `subprocess.run`; the dict is built from `os.environ` copy with secrets merged in. | Assert subprocess called with correct `env` containing secret value |
| FR-17 | Every secret operation writes a row to `secret_audit` in `tag.sqlite3` before the operation completes. If the audit write fails, the main operation is rolled back and an error is returned. | Mock DB write failure; assert main operation not committed |
| FR-18 | `secret audit` filters by `--name`, `--op`, `--since`, and `--last N`; all filters combine with AND semantics. | Test each filter in isolation and in combination |
| FR-19 | `secret rotate` generates a new 96-bit nonce, re-encrypts with the same vault key, updates the row atomically in a single SQLite transaction; the old ciphertext is overwritten. | Assert new nonce != old nonce; assert row count unchanged; assert old ciphertext not retrievable |
| FR-20 | When `--secret NAME` references a secret that does not exist in the vault, `sandbox run` exits with code 1 and prints `Error: secret 'NAME' not found in vault.` before starting any sandbox process. | Test with non-existent secret name |
| FR-21 | Decrypted secret byte arrays are explicitly zeroed after use: for `bytearray` buffers call `memset`; for `str` objects use `ctypes.memset`. | Unit test using `ctypes` to inspect memory after vault read returns |
| FR-22 | `secret add --from-file PATH` rejects symlinks and paths outside `~` to prevent path-traversal injection of system files. | Test with `/etc/passwd` symlink; assert rejection |
| FR-23 | The vault database uses `PRAGMA journal_mode=WAL` and `PRAGMA foreign_keys=ON` for consistency with the main `tag.sqlite3`. | Assert PRAGMA values after open |
| FR-24 | `tag sandbox secret list --json` output is valid JSON and contains no `value` or `plaintext` key at any nesting level. | Parse JSON output; recurse all keys; assert absence of sensitive keys |

---

## 9. Non-Functional Requirements

| ID | Requirement | Target |
|----|-------------|--------|
| NFR-01 | Vault unlock latency | Argon2id KDF completes in 200–500 ms; subsequent operations within same process reuse cached key and add < 1 ms overhead |
| NFR-02 | Secret inject latency | Decryption and env-var injection adds < 5 ms to sandbox startup for up to 10 secrets |
| NFR-03 | Vault DB size | 10,000 secrets with 64-byte values consume < 10 MB on disk (SQLite row overhead ~150 bytes per secret) |
| NFR-04 | Audit table growth | `secret_audit` rows are append-only; table must support 1M rows without query degradation (index on `name`, `created_at`) |
| NFR-05 | Cross-platform support | vault.py must run on macOS 12+, Ubuntu 20.04+, Windows 10+ (where Docker backend is available); `keyring` integration tested on macOS Keychain and Linux Secret Service |
| NFR-06 | Dependency minimalism | Only new hard dependency is `cryptography>=42.0`; `argon2-cffi>=23.0` is new hard dependency; `keyring>=25.0` is optional soft dependency |
| NFR-07 | Thread safety | Vault operations are safe to call from multiple threads (use per-connection SQLite with WAL; key derivation is idempotent and cached behind a `threading.Lock`) |
| NFR-08 | No secret in core dumps | On Linux, call `prctl(PR_SET_DUMPABLE, 0)` before decryption if running as root; document that users should set `ulimit -c 0` for defense-in-depth |
| NFR-09 | Test coverage | `secrets_vault.py` unit test coverage >= 90%; integration tests cover all four sandbox backends |
| NFR-10 | Code auditability | `secrets_vault.py` is a standalone module with no imports from `controller.py`; it imports only stdlib + `cryptography` + optional `keyring`; total file size target < 600 lines |

---

## 10. Technical Design

### 10.1 New Files

| File | Purpose |
|------|---------|
| `src/tag/secrets_vault.py` | Vault client: key derivation, AES-256-GCM encrypt/decrypt, vault DB CRUD, audit writes |
| `src/tag/vault_schema.sql` | SQL DDL for vault.db and the `secret_audit` table migration |
| `tests/test_secrets_vault.py` | Unit + integration tests for vault module |
| `tests/test_sandbox_secret_injection.py` | Integration tests for --secret flag across all backends |

### 10.2 SQLite DDL

#### `~/.tag/vault/vault.db` (separate database, mode 0600)

```sql
-- vault.db: encrypted secrets store
-- Created by secrets_vault.py:ensure_vault_schema()
PRAGMA journal_mode = WAL;
PRAGMA foreign_keys = ON;
PRAGMA secure_delete = ON;   -- overwrite freed pages with zeros

CREATE TABLE IF NOT EXISTS vault_meta (
  schema_version  INTEGER NOT NULL DEFAULT 1,
  kdf             TEXT    NOT NULL DEFAULT 'argon2id',
  kdf_time_cost   INTEGER NOT NULL DEFAULT 3,
  kdf_memory_cost INTEGER NOT NULL DEFAULT 65536,  -- KiB
  kdf_parallelism INTEGER NOT NULL DEFAULT 1,
  kdf_hash_len    INTEGER NOT NULL DEFAULT 32,
  kdf_salt        BLOB    NOT NULL,                -- 16 bytes random, generated once
  created_at      TEXT    NOT NULL
);

CREATE TABLE IF NOT EXISTS secrets (
  name            TEXT    PRIMARY KEY,             -- [A-Z][A-Z0-9_]*
  description     TEXT    NOT NULL DEFAULT '',
  tags_json       TEXT    NOT NULL DEFAULT '{}',   -- JSON k=v pairs
  ciphertext      BLOB    NOT NULL,                -- AES-256-GCM output
  nonce           BLOB    NOT NULL,                -- 12 bytes random
  auth_tag        BLOB    NOT NULL,                -- 16 bytes GCM tag
  size_bytes      INTEGER NOT NULL,                -- plaintext byte length
  inject_mode     TEXT    NOT NULL DEFAULT 'env',  -- 'env' | 'file'
  file_mount_path TEXT,                            -- NULL unless inject_mode='file'
  file_mode       INTEGER NOT NULL DEFAULT 256,    -- 0o400 default
  created_at      TEXT    NOT NULL,
  updated_at      TEXT    NOT NULL,
  last_used_at    TEXT
);

CREATE INDEX IF NOT EXISTS idx_secrets_created ON secrets(created_at);
```

#### `~/.tag/runtime/tag.sqlite3` audit table (appended by migration)

```sql
-- Added by _migrate_prd_097_secret_audit() in controller.py
CREATE TABLE IF NOT EXISTS secret_audit (
  id          TEXT    PRIMARY KEY,   -- UUID4
  name        TEXT    NOT NULL,      -- secret name (never value)
  operation   TEXT    NOT NULL,      -- add|delete|inject|list|export|rotate
  run_id      TEXT,                  -- sandbox_runs.id if operation=inject, else NULL
  os_user     TEXT    NOT NULL,      -- getpass.getuser()
  is_warning  INTEGER NOT NULL DEFAULT 0,   -- 1 for export operations
  created_at  TEXT    NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_sa_name       ON secret_audit(name, created_at);
CREATE INDEX IF NOT EXISTS idx_sa_run_id     ON secret_audit(run_id) WHERE run_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_sa_operation  ON secret_audit(operation, created_at);
```

### 10.3 Core Dataclasses

```python
# src/tag/secrets_vault.py
from __future__ import annotations

import os
import uuid
import sqlite3
import getpass
import secrets as _secrets
import threading
from dataclasses import dataclass, field
from pathlib import Path
from typing import Optional


@dataclass
class VaultConfig:
    """Paths and KDF parameters for the vault."""
    vault_dir: Path = field(
        default_factory=lambda: Path.home() / ".tag" / "vault"
    )
    db_name: str = "vault.db"
    kdf_time_cost: int = 3
    kdf_memory_cost: int = 65_536   # 64 MiB in KiB
    kdf_parallelism: int = 1
    kdf_hash_len: int = 32          # 256-bit AES key
    keyring_service: str = "tag-vault"
    keyring_username: str = "master-key"

    @property
    def db_path(self) -> Path:
        return self.vault_dir / self.db_name

    @property
    def key_fallback_path(self) -> Path:
        return self.vault_dir / ".key"


@dataclass
class SecretMeta:
    """Vault metadata for a single secret (no plaintext value)."""
    name: str
    description: str
    tags: dict[str, str]
    size_bytes: int
    inject_mode: str        # 'env' | 'file'
    file_mount_path: Optional[str]
    file_mode: int          # octal, e.g. 0o400
    created_at: str
    updated_at: str
    last_used_at: Optional[str]


@dataclass
class AuditEvent:
    """A single secret audit record."""
    id: str
    name: str
    operation: str          # add | delete | inject | list | export | rotate
    run_id: Optional[str]
    os_user: str
    is_warning: bool
    created_at: str
```

### 10.4 Vault Client Core Algorithms

```python
# Key derivation (called once per process, result cached)
_KEY_CACHE: dict[str, bytes] = {}
_KEY_LOCK = threading.Lock()


def _derive_key(passphrase: str, salt: bytes, cfg: VaultConfig) -> bytes:
    """Argon2id password-based key derivation. Result is 32 bytes (AES-256 key)."""
    from argon2.low_level import hash_secret_raw, Type
    return hash_secret_raw(
        secret=passphrase.encode("utf-8"),
        salt=salt,
        time_cost=cfg.kdf_time_cost,
        memory_cost=cfg.kdf_memory_cost,
        parallelism=cfg.kdf_parallelism,
        hash_len=cfg.kdf_hash_len,
        type=Type.ID,
    )


def _encrypt(plaintext: bytes, key: bytes) -> tuple[bytes, bytes, bytes]:
    """AES-256-GCM encrypt. Returns (ciphertext, nonce, auth_tag)."""
    from cryptography.hazmat.primitives.ciphers.aead import AESGCM
    nonce = _secrets.token_bytes(12)        # 96-bit random nonce
    aesgcm = AESGCM(key)
    ciphertext_with_tag = aesgcm.encrypt(nonce, plaintext, None)
    # cryptography library appends 16-byte GCM tag to ciphertext
    ciphertext = ciphertext_with_tag[:-16]
    auth_tag = ciphertext_with_tag[-16:]
    return ciphertext, nonce, auth_tag


def _decrypt(ciphertext: bytes, nonce: bytes, auth_tag: bytes, key: bytes) -> bytes:
    """AES-256-GCM decrypt. Raises InvalidTag on authentication failure."""
    from cryptography.hazmat.primitives.ciphers.aead import AESGCM
    aesgcm = AESGCM(key)
    return aesgcm.decrypt(nonce, ciphertext + auth_tag, None)


def _zero_bytes(buf: bytearray) -> None:
    """Overwrite a bytearray with zeros in-place."""
    import ctypes
    ctypes.memset((ctypes.c_char * len(buf)).from_buffer(buf), 0, len(buf))
```

### 10.5 Injection Bridge: `inject_secrets_into_env()`

This is the core bridge between the vault and sandbox backends:

```python
def inject_secrets_into_env(
    names: list[str],
    vault_cfg: VaultConfig,
    base_env: dict[str, str],
    *,
    main_db_conn: sqlite3.Connection,
    run_id: str,
) -> tuple[dict[str, str], list[tuple[str, str]]]:
    """
    Decrypt named secrets and merge into base_env for env-mode secrets.
    Returns:
      - merged_env: dict to pass to subprocess / Docker / E2B as environment
      - file_mounts: list of (host_temp_path, container_path) for file-mode secrets

    Writes an 'inject' audit event for each secret.
    Caller is responsible for unlinking host_temp_path after sandbox exits.
    Plaintext bytes are zeroed before this function returns.
    """
```

### 10.6 Integration Points with `sandbox.py`

The existing `run_sandbox()` function in `sandbox.py` gains a `secret_names: list[str]` parameter:

```python
def run_sandbox(
    command: list[str],
    *,
    backend: str = "restricted",
    image: str | None = None,
    timeout: int = 60,
    workdir: Path | None = None,
    code: str | None = None,
    secret_names: list[str] | None = None,   # NEW
    cfg: dict | None = None,                  # for open_db() and VaultConfig
) -> tuple[int, str, str]:
    ...
    # Before dispatching to backend:
    if secret_names:
        env, file_mounts = inject_secrets_into_env(
            names=secret_names,
            vault_cfg=VaultConfig(),
            base_env=_safe_base_env(),
            main_db_conn=conn,
            run_id=run_id,
        )
    else:
        env, file_mounts = _safe_base_env(), []
    ...
    # After sandbox exits, regardless of exit code:
    finally:
        for host_path, _ in file_mounts:
            Path(host_path).unlink(missing_ok=True)
```

### 10.7 Controller Integration

`controller.py` gains the following new command handler functions and CLI routes:

```
cmd_sandbox_secret_add(cfg, name, value, from_env, from_file, description, tags, overwrite)
cmd_sandbox_secret_list(cfg, json_output, tags_filter)
cmd_sandbox_secret_delete(cfg, name, yes)
cmd_sandbox_secret_rotate(cfg, name)
cmd_sandbox_secret_export(cfg, name, to_env)
cmd_sandbox_secret_inject_file(cfg, name, mount, mode)
cmd_sandbox_secret_audit(cfg, name, op, since, last, json_output)
```

And the `_migrate_prd_097_secret_audit()` migration is added to the existing migration chain called from `open_db()`.

### 10.8 Vault Unlock Flow

```
tag sandbox secret add --name FOO --value bar
  │
  ▼
1. Check TAG_VAULT_PASSPHRASE env var
   If set → use as passphrase (CI mode)
   If not set and TTY → getpass.getpass("Vault passphrase: ")
   If not set and no TTY → exit 1 "Set TAG_VAULT_PASSPHRASE for non-interactive use"
  │
  ▼
2. Open vault.db (create if not exists)
   If new vault: generate 16-byte salt, write vault_meta row
   If existing vault: read salt from vault_meta
  │
  ▼
3. Check keyring for cached derived key
   If found → use directly (skip KDF)
   If not found → run Argon2id KDF (~300ms)
                → store result in keyring (or .key file)
  │
  ▼
4. Execute operation (encrypt/decrypt/delete/list)
  │
  ▼
5. Write audit event to tag.sqlite3
  │
  ▼
6. Zero plaintext buffers
```

---

## 11. Security Considerations

1. **AES-256-GCM authentication tag verification.** The `cryptography` library's `AESGCM.decrypt()` raises `cryptography.exceptions.InvalidTag` if the ciphertext has been tampered with. This exception must propagate to the caller as a hard error, not silently ignored. Any `InvalidTag` exception should be logged as a security event (without the ciphertext value) and abort the operation.

2. **Nonce uniqueness.** AES-GCM is catastrophically broken if a (key, nonce) pair is reused. The 96-bit nonce is generated via `secrets.token_bytes(12)` (CSPRNG) for every `add` and `rotate` operation. The probability of a nonce collision for a single key across 2^32 secrets is less than 2^-64 — negligible for local use cases. No counter-based nonce scheme is used.

3. **Vault key never in `sandbox_runs`.** The `run_sandbox()` function writes the `command` string (the user-facing template) to `sandbox_runs.command` before calling `inject_secrets_into_env()`. The env dict containing decrypted secrets is passed directly to `subprocess.run(env=...)`, `docker run --env`, or the E2B/Modal SDK; it is never concatenated into a loggable command string.

4. **Process list exposure.** For the Docker backend, secrets are passed via `--env NAME=VALUE` in the `docker run` subprocess arguments. On Linux, `/proc/<pid>/cmdline` is readable by the local user during the brief window between `Popen` and container start. To mitigate: use `docker run --env-file <(echo KEY=VALUE)` with a process substitution (fd-based pipe) on Linux, so the value never appears in `cmdline`. On macOS, process argument inspection is restricted to root by default.

5. **`PRAGMA secure_delete = ON`.** The vault DB uses SQLite's `secure_delete` pragma, which causes SQLite to overwrite deleted database pages with zeros before freeing them. This is defense-in-depth against recovery of deleted secret bytes from disk.

6. **OS keychain as secondary storage.** The derived key (not the passphrase, not any secret value) is stored in the OS keychain. The keychain entry is 32 bytes — not the secret itself. Even if the keychain is compromised, an attacker needs the vault DB to recover secrets. Even if the vault DB is copied, an attacker needs the keychain key or the passphrase to decrypt.

7. **Vault directory permissions.** `secrets_vault.py:ensure_vault_dir()` creates `~/.tag/vault/` with mode `0700` (owner-only access). The vault DB is created with mode `0600`. If the directory or file is found with broader permissions, a warning is printed and the operation is aborted.

8. **Memory zeroing is best-effort in CPython.** CPython strings are immutable; once a secret is assigned to a `str`, the bytes cannot be reliably zeroed because the interpreter may hold internal references. The vault API uses `bytearray` for decrypted values throughout and calls `_zero_bytes()` before returning. Callers are responsible for not converting to `str` until the last possible moment (env dict construction for subprocess). The env dict passed to `subprocess.run` is a `{str: str}` dict as required by the Python API; the str conversion is unavoidable at the system boundary.

9. **Export operation is audited with warning flag.** `secret export` is the only operation that intentionally exposes a plaintext value outside the sandbox. Its `secret_audit` row has `is_warning=1`. A separate `tag sandbox secret audit --op export` query surfaces all historical export events for review.

10. **`TAG_VAULT_PASSPHRASE` in CI.** When the passphrase is taken from the environment variable, a notice is printed to stderr: `Note: unlocking vault from TAG_VAULT_PASSPHRASE. Ensure this variable is a masked CI secret.` This is informational only and does not block the operation.

11. **Vault integrity check on open.** On every `vault.db` open, `_check_vault_integrity()` runs `PRAGMA integrity_check` (fast mode, max 1 error). A non-OK result aborts with `Error: vault database integrity check failed. Do not use this vault.`

12. **Argon2id parameter enforcement.** The KDF parameters are stored in `vault_meta` at creation time. On subsequent opens, the stored parameters are read and compared against the hardcoded minimums (`time_cost >= 3`, `memory_cost >= 65536`). If the vault was created with weaker parameters (e.g., migration from a test environment), a warning is printed and re-derivation with current parameters is offered.

---

## 12. Testing Strategy

### 12.1 Unit Tests (`tests/test_secrets_vault.py`)

| Test | Description |
|------|-------------|
| `test_encrypt_decrypt_roundtrip` | Encrypt a known plaintext, decrypt, assert equality; assert nonce is 12 bytes |
| `test_nonce_uniqueness` | Call `_encrypt` 1000 times; assert all nonces are distinct |
| `test_invalid_tag_raises` | Corrupt 1 byte of ciphertext; assert `InvalidTag` raised on decrypt |
| `test_argon2id_params` | Mock `hash_secret_raw`; assert called with `time_cost=3, memory_cost=65536, type=Type.ID` |
| `test_vault_dir_created_mode_0700` | Call `ensure_vault_dir` on temp path; assert directory mode |
| `test_vault_db_mode_0600` | After first `secret add`, assert vault DB file mode |
| `test_name_validation_rejects_lowercase` | `secret add --name lower_case`; assert ValueError |
| `test_name_validation_accepts_uppercase` | `secret add --name VALID_KEY`; assert no error |
| `test_value_size_limit` | Pass 65537-byte value; assert VaultError raised before DB write |
| `test_list_contains_no_values` | Add secret, call `list_secrets()`; assert returned SecretMeta has no `value` field |
| `test_delete_zeros_ciphertext` | After delete, assert the row's ciphertext column was overwritten before DELETE |
| `test_rotate_new_nonce` | `rotate_secret`; assert new_nonce != old_nonce; assert decrypt still works |
| `test_memory_zero_after_decrypt` | Use `ctypes` to inspect buffer after `_zero_bytes(buf)`; assert all zeros |
| `test_audit_written_on_add` | Add secret; assert `secret_audit` row count == 1, operation == 'add' |
| `test_audit_written_on_inject` | Call `inject_secrets_into_env`; assert audit row with operation == 'inject' |
| `test_audit_rollback_on_main_failure` | Mock main operation to fail; assert audit row NOT written |
| `test_keyring_cache_hit` | Mock keyring; assert `_derive_key` not called on second vault unlock |
| `test_env_var_passphrase` | Set `TAG_VAULT_PASSPHRASE`; call add without TTY; assert no prompt |
| `test_secure_delete_pragma` | Open vault DB; `PRAGMA secure_delete`; assert `= on` |
| `test_integrity_check_on_open` | Corrupt vault DB; assert `VaultIntegrityError` raised on open |

### 12.2 Integration Tests (`tests/test_sandbox_secret_injection.py`)

| Test | Description |
|------|-------------|
| `test_restricted_backend_env_injection` | Add secret, run with restricted backend, assert secret value appears in subprocess env via echo; assert `sandbox_runs.command` does not contain value |
| `test_docker_backend_env_injection` | (requires Docker) Add secret, run docker sandbox, assert env var accessible inside container |
| `test_docker_backend_file_injection` | (requires Docker) Add secret with `inject_mode='file'`, run, assert file readable at mount path inside container; assert temp file deleted on host after run |
| `test_nonexistent_secret_hard_error` | Pass `--secret DOES_NOT_EXIST`; assert exit 1 before docker/subprocess started |
| `test_multiple_secrets_injected` | Add 3 secrets, pass all 3 via `--secret`; assert all 3 env vars accessible inside sandbox |
| `test_output_does_not_contain_secret` | Sandboxed code echoes secret; assert `sandbox_runs.output` column in DB does NOT contain the known secret value (scrubbing is post-hoc — this test validates that the command column is clean) |
| `test_audit_run_id_linked` | After `--secret` injection run, assert `secret_audit.run_id` matches `sandbox_runs.id` |

### 12.3 Performance Tests

| Test | Target |
|------|--------|
| KDF latency | Argon2id with production params: 200–500 ms on CI hardware |
| Cached key lookup | Second vault open (key in keyring): < 5 ms |
| Inject 10 secrets | `inject_secrets_into_env` with 10 secrets: < 50 ms total |
| 10,000-secret vault | `list_secrets()` with 10k rows: < 50 ms |

### 12.4 Security Regression Tests

- **Plaintext grep test:** After every test that runs `secret add` + `sandbox run`, assert that the known plaintext value does not appear anywhere in `~/.tag/runtime/tag.sqlite3` or `~/.tag/vault/vault.db` as a raw byte string.
- **Permission drift test:** Assert `~/.tag/vault/` and `~/.tag/vault/vault.db` retain mode `0700`/`0600` after concurrent writes.
- **Nonce collision test:** Insert 10,000 secrets; assert all `nonce` columns are distinct (probabilistic collision check).

---

## 13. Acceptance Criteria

| ID | Criterion | Pass Condition |
|----|-----------|----------------|
| AC-01 | Vault creation | `tag sandbox secret add --name TEST_KEY --value hello` creates `~/.tag/vault/vault.db` with mode 0600 | Stat file after command |
| AC-02 | Value never in runs table | After `tag sandbox run --code "import os; print(os.environ['TEST_KEY'])" --secret TEST_KEY`, query `SELECT command, output, error FROM sandbox_runs ORDER BY created_at DESC LIMIT 1`; assert 'hello' is not a substring of any column | SQLite query |
| AC-03 | List shows no values | `tag sandbox secret list` output does not contain 'hello' | String search on stdout |
| AC-04 | Delete removes row | After `tag sandbox secret delete TEST_KEY --yes`, `SELECT count(*) FROM secrets WHERE name='TEST_KEY'` == 0 in vault.db | SQLite query |
| AC-05 | Audit trail complete | After add + run + delete cycle, `SELECT count(*) FROM secret_audit WHERE name='TEST_KEY'` == 3 | SQLite query |
| AC-06 | Rotate preserves value | `tag sandbox secret rotate --name TEST_KEY`; subsequent sandbox run still receives correct value | Functional test |
| AC-07 | CI passphrase mode | With `TAG_VAULT_PASSPHRASE=mysecret` set, `secret add` completes without any stdin prompt even with TTY=false | Subprocess test with stdin=None |
| AC-08 | Invalid name rejected | `tag sandbox secret add --name bad-name --value x` exits 1 and prints pattern constraint message | Exit code + stdout check |
| AC-09 | Nonexistent secret blocks run | `tag sandbox run --code "pass" --secret NO_SUCH_KEY` exits 1 without starting the sandbox | Exit code; assert no `sandbox_runs` row created |
| AC-10 | File injection cleaned up | After Docker file-injection run, temp file at `/tmp/tag-secret-*` does not exist on host | `glob` check after run |
| AC-11 | Auth tag tamper detection | Corrupt 1 byte in `secrets.ciphertext`; subsequent sandbox run with that secret exits 1 with "vault integrity error" message | Hex-edit vault.db; run; check exit code |
| AC-12 | Vault dir permissions | `~/.tag/vault/` has mode 0700; vault.db has mode 0600 on creation | `os.stat` assertions |
| AC-13 | Export audit warning | `tag sandbox secret export --name TEST_KEY` produces `secret_audit` row with `is_warning=1` | SQLite query |
| AC-14 | Multiple secrets | `tag sandbox run --secret A --secret B --secret C` injects all three env vars visible inside sandbox | Three-env-var functional test |
| AC-15 | `--json` output valid | `tag sandbox secret list --json` produces valid JSON per `json.loads()` with no `value` key at any level | JSON parse + key recursion |

---

## 14. Dependencies

| Dependency | Type | Version | Purpose | Already Present? |
|------------|------|---------|---------|-----------------|
| `cryptography` | Hard (new) | >= 42.0 | AES-256-GCM via `AESGCM`; `InvalidTag` exception | No (add to pyproject.toml) |
| `argon2-cffi` | Hard (new) | >= 23.0 | Argon2id KDF via `argon2.low_level.hash_secret_raw` | No (add to pyproject.toml) |
| `keyring` | Soft (optional) | >= 25.0 | OS keychain integration for derived key storage | No (optional extras) |
| `sqlite3` | stdlib | — | Vault DB and audit table | Yes |
| `secrets` | stdlib | — | CSPRNG for nonce generation | Yes |
| `getpass` | stdlib | — | Passphrase prompt without echo | Yes |
| `ctypes` | stdlib | — | Memory zeroing of decrypted buffers | Yes |
| PRD-028 (sandbox.py) | Internal | — | `run_sandbox()` to extend with `secret_names` param | Yes (implemented) |
| PRD-034 (security.py) | Internal | — | Pattern library reference for audit log filtering | Yes (implemented) |
| PRD-013 (tracing.py) | Internal | — | Audit event correlation with trace spans | Yes (implemented) |
| PRD-008 (queue_worker.py) | Internal | — | Future: queue jobs referencing vault secrets | Yes (implemented) |

---

## 15. Open Questions

| ID | Question | Owner | Target Resolution |
|----|----------|-------|-------------------|
| OQ-01 | Should the vault passphrase be derivable from the user's system login password (PAM integration on Linux, Keychain login keychain on macOS) so users never need to set a separate passphrase? | Security lead | Before implementation kickoff |
| OQ-02 | Should `secret add` accept a `--ttl <duration>` so secrets auto-expire? Useful for short-lived CI tokens. | Product | v2 consideration; not blocking v1 |
| OQ-03 | For the Docker backend, should we use `--env-file /dev/stdin` (piped from Python) instead of `--env NAME=VALUE` to eliminate cmdline exposure? This requires Docker 20.10+ and Linux process substitution support. | Engineering | During implementation |
| OQ-04 | Should `inject_secrets_into_env` scrub the `sandbox_runs.output` column post-run using the known secret values as a regex? This would prevent secrets echoed by careless code from persisting. The counterargument is that scrubbing may mask debugging information. | Security + Product | Design review |
| OQ-05 | Should the vault support named secret groups (analogous to Modal's `Secret` objects), where `tag sandbox run --secret-group MY_APP_SECRETS` injects all secrets tagged with that group? | Product | v2 |
| OQ-06 | Should `tag sandbox secret audit` be surfaced in the existing `tag trace` output (e.g., as child spans under a sandbox run span in PRD-013)? | Engineering | PRD-013 owner alignment |
| OQ-07 | Is Argon2id `memory_cost=65536` (64 MiB) acceptable on systems with < 256 MiB free RAM (e.g., embedded CI runners, Raspberry Pi)? Should we add a `--kdf-profile fast|standard|strong` flag? | Engineering | Benchmark on target CI hardware |
| OQ-08 | For E2B backend: E2B's `Sandbox.create(envs={...})` passes secrets to the Firecracker VM at creation time, but E2B logs sandbox creation events server-side. Do E2B's server-side logs capture the `envs` dict? Need to confirm with E2B support or SDK source. | Engineering | E2B support inquiry |

---

## 16. Complexity and Timeline

### Phase 1 — Vault Core (Days 1–4)

- Implement `secrets_vault.py`: `VaultConfig`, `SecretMeta`, `AuditEvent` dataclasses.
- Implement `ensure_vault_dir()`, `ensure_vault_schema()`, `_open_vault_db()`.
- Implement Argon2id KDF with `keyring` integration and `.key` fallback.
- Implement `_encrypt()`, `_decrypt()`, `_zero_bytes()`.
- Implement `add_secret()`, `list_secrets()`, `delete_secret()`, `rotate_secret()`.
- Unit tests for all crypto primitives and vault CRUD (AC-01, AC-03, AC-04, AC-06, AC-11, AC-12).

### Phase 2 — Audit Layer (Day 5)

- Add `_migrate_prd_097_secret_audit()` to `controller.py` migration chain.
- Implement `write_audit_event()` in `secrets_vault.py`.
- Ensure audit writes are transactional with main operations (FR-17).
- Tests: AC-05, AC-13.

### Phase 3 — Sandbox Integration (Days 6–8)

- Extend `sandbox.py:run_sandbox()` with `secret_names` parameter.
- Implement `inject_secrets_into_env()` for all four backends (restricted, docker, e2b, modal).
- Implement file-injection mode with temp-file lifecycle management (FR-13).
- Integration tests for restricted and Docker backends (AC-02, AC-09, AC-10, AC-14).
- CLI wiring: `cmd_sandbox_secret_*` handlers in `controller.py`.

### Phase 4 — CLI Surface + Audit Query (Days 9–10)

- Implement all `tag sandbox secret` subcommands: `add`, `list`, `delete`, `rotate`, `export`, `inject-file`, `audit`.
- `--json` output for `list` and `audit` (AC-15).
- `tag sandbox run --secret` flag plumbing.
- End-to-end CLI tests matching AC-01 through AC-15.
- Documentation update in `docs/prd/INDEX.md`.

### Phase 5 — Hardening and Review (Days 11–14)

- Security review: verify memory zeroing, nonce uniqueness, permission checks, process-list exposure.
- Performance benchmarks: KDF latency, inject latency, 10k-secret list.
- Address OQ-03 (Docker `--env-file` via pipe) if decided.
- Address OQ-04 (output scrubbing) if decided.
- Code coverage gate: `secrets_vault.py` >= 90%.
- PR review and merge.

**Total estimated effort: 10–14 engineering days (M size estimate confirmed).**

