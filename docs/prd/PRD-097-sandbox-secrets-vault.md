# PRD-097: Sandbox-Level Secrets Injection via Encrypted Vault (`tag sandbox secret`)

> **Stack: Go** (native single-binary; see docs/GO_MIGRATION_RESEARCH.md). This PRD was re-framed from Python to Go.

**Status:** Proposed
**Priority:** P3
**Estimated Effort:** M (1-2 weeks)
**Category:** Sandbox & Execution Environment
**Affects:** `internal/sandbox` + `internal/credentials/vault` (new)
**Depends on:** PRD-028 (Sandbox Code Execution), PRD-034 (Secret Scanning), PRD-013 (Agent Tracing / Observability), PRD-008 (Background Task Queue)
**GitHub Issue:** #348
**Inspired by:** Modal secrets, E2B environment variables, HashiCorp Vault

---

## 1. Overview

TAG's sandbox subsystem (PRD-028) provides process isolation via Docker, E2B, Modal, and restricted-subprocess backends. However, it has no first-class mechanism for delivering secrets to sandbox workloads. Today, users who need an `OPENAI_API_KEY` inside a sandboxed code execution have three bad options: embed the key in `--code` (leaks into `sandbox_runs.command`), pass it via `--env KEY=value` on the CLI (leaks into shell history and process lists), or hardcode it in the agent's system prompt (leaks into traces and profile exports). All three paths violate the security guarantees that the sandbox itself is meant to provide.

This PRD specifies `tag sandbox secret`: a local, encrypted secrets vault integrated tightly with the sandbox execution layer. Secrets are stored as AES-256-GCM ciphertext in BLOB columns of a pure-Go `modernc.org/sqlite` database (CGO_ENABLED=0) at `~/.tag/vault/vault.db`, separate from the main `tag.sqlite3` state store. At sandbox invocation time, named secrets are decrypted in memory, injected into the sandbox environment (as environment variables or ephemeral files) through the `internal/sandbox` Backend interface, and immediately zeroed after the process exits. The plaintext value never touches disk outside the vault, never appears in `sandbox_runs`, never appears in audit logs, and never appears in any TAG config file.

The design draws directly from three production systems. Modal Secrets stores key-value maps encrypted at rest, exposes them to function invocations via a `secrets=[Secret.from_name("my-secret")]` parameter, and never logs values. E2B environment variables are injected at sandbox creation time into the Firecracker microVM, so the host process never exposes them as CLI arguments. HashiCorp Vault's model of explicit `vault read secret/myapp/api-key` → in-process decryption → injected into the child process environment without writing to disk is the procedural template for the vault client in this PRD. TAG adopts all three patterns: named secrets, injection at creation time, and in-process-only decryption.

The vault key derivation is user-space and deterministic: a master key is derived from a user-supplied passphrase via Argon2id (`golang.org/x/crypto/argon2.IDKey`, the OWASP-recommended KDF for password-based key derivation) and stored in the OS keychain (macOS Keychain, Linux libsecret/Secret Service, Windows Credential Manager) via the `github.com/zalando/go-keyring` module. When `go-keyring` reports no usable backend, the derived key is cached in `~/.tag/vault/.key` with mode `0600`. Each secret ciphertext includes its own random 96-bit nonce and 128-bit authentication tag per AES-256-GCM (`crypto/aes` + `crypto/cipher` GCM), making the vault tamper-evident at the per-secret level.

Audit trails are written to a separate append-only `secret_audit` table in `tag.sqlite3`. Every access event — `add`, `get`, `list`, `delete`, `inject` — is recorded with timestamp, secret name (never value), operation, invoking sandbox run ID (if applicable), and the OS user. This gives platform engineers and security teams a complete access log without storing any plaintext material.

---

## 2. Problem Statement

### 2.1 Secrets Leak Through Sandbox Run Records

`internal/sandbox`'s schema bootstrap (`EnsureSchema`) creates a `sandbox_runs` table with a `command TEXT NOT NULL` column. When a user today passes credentials as environment variables via `--env OPENAI_KEY=sk-...`, those values are captured in the command string or in the process environment visible via `/proc/<pid>/environ` on Linux. The `sandbox_runs.output` column also captures full stdout, meaning any debug print of the environment inside sandboxed code permanently embeds the key in the database. PRD-034 (Secret Scanning) gates profile export but has no coverage of the `sandbox_runs` table.

### 2.2 No Secure Handoff Between Agent Loop and Sandbox

The agent loop in `internal/agent` generates code dynamically. If an agent needs to call an external API, it must either hardcode the key into the generated code (which then appears in `sandbox_runs.command`) or the user must pre-inject it via a mechanism that does not exist. The gap is structural: there is a secure vault in principle (the OS keychain, `.env` files) and there is a secure execution environment (the sandbox), but there is no bridge between them that maintains the security invariant across the boundary.

### 2.3 Audit-Free Secret Usage in Agent Workflows

TAG supports multi-step agent workflows (queue jobs, swarm agents, DAG pipelines). When a secret like `DATABASE_URL` is used in step 3 of a 10-step DAG, there is currently no record of which step used it, when, or under which run ID. If a key is later found to have been compromised, there is no forensic trail to determine which TAG jobs had access to it. This is a compliance gap in enterprise deployments.

---

## 3. Goals

| ID | Goal |
|----|------|
| G1 | Store named secrets in an AES-256-GCM encrypted local vault; plaintext never written to any TAG config file, `sandbox_runs`, or log. |
| G2 | Inject secrets into sandbox workloads as environment variables or ephemeral temp files at runtime, with zero-on-exit memory clearing for in-process buffers. |
| G3 | `tag sandbox secret list` shows only names and metadata; values are never displayed in any list or table output. |
| G4 | Derive the vault master key from a user passphrase via Argon2id; store derived key in OS keychain via `zalando/go-keyring`; fall back to `~/.tag/vault/.key` (mode 0600) when keychain is unavailable. |
| G5 | Write a tamper-evident audit record for every secret operation (add, delete, inject, list) to `secret_audit` in `tag.sqlite3`. |
| G6 | `tag sandbox run --secret NAME` syntax binds a named vault secret to the sandbox invocation; multiple `--secret` flags are supported. |
| G7 | `tag sandbox secret inject-file --name DB_CERT --mount /run/secrets/db.crt` delivers a secret as an ephemeral read-only file inside the sandbox (Docker and E2B backends). |
| G8 | Secrets survive across shell sessions; the vault persists at `~/.tag/vault/vault.db`. |
| G9 | `tag sandbox secret rotate --name OPENAI_KEY` re-encrypts a secret with a fresh nonce without exposing the plaintext value. |
| G10 | Zero mandatory new binary dependencies; AES-256-GCM and Argon2id come from the Go standard library and `golang.org/x/crypto` (no cgo, CGO_ENABLED=0 preserved); `zalando/go-keyring` is the only optional module. |

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
| Memory zero-on-exit | Decrypted secret bytes are zeroed before `RunSandbox()` returns | Unit test asserts the backing `[]byte` slice is all-zero after the call |

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
- `--value`: Plaintext value. Mutually exclusive with `--from-env` and `--from-file`. If none of the three are provided, the CLI prompts interactively using `golang.org/x/term.ReadPassword` so the value is never echoed.
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
| FR-03 | Vault master key is derived via Argon2id with at minimum: time=3, memory=65536 (64 MiB), threads=1, keyLen=32. | Unit test verifies `argon2.IDKey` is invoked with `time=3, memory=64*1024, threads=1, keyLen=32` |
| FR-04 | On macOS/Linux/Windows where `go-keyring` has a usable backend, the derived key is stored in and retrieved from the OS keychain under service name `tag-vault`. | Integration test: store key, restart process, retrieve without re-prompting passphrase |
| FR-05 | If `go-keyring` reports no backend (`keyring.ErrUnsupportedPlatform`/error), the derived key is written to `~/.tag/vault/.key` with mode 0600 and a warning printed. | Unit test injects a keyring stub returning an error; assert file created with correct mode |
| FR-06 | Passphrase can be supplied via `TAG_VAULT_PASSPHRASE` environment variable for CI/non-interactive use; when set, no passphrase prompt is issued. | Test: set env var, call `secret add` without a TTY; assert no prompt |
| FR-07 | `secret add` rejects names that do not match `[A-Z][A-Z0-9_]*`; error message cites the naming constraint. | `secret add --name my-key` exits 1 with pattern-mismatch message |
| FR-08 | `secret add` rejects values exceeding 65536 bytes; the backing `[]byte` is zeroed before the error is returned. | Test with 65537-byte value; assert exit 1 and no vault row created |
| FR-09 | `secret list` output never contains any substring that matches any stored ciphertext or plaintext value. | Store known value, capture `list` stdout, assert value not present |
| FR-10 | `secret delete` overwrites the `ciphertext` column with `NULL` or zero bytes before executing `DELETE`. | Read row after failed delete (simulate); assert ciphertext is zeroed in the UPDATE |
| FR-11 | `tag sandbox run --secret NAME` injects the secret into the sandbox process environment exclusively; the value does not appear in `sandbox_runs.command`, `sandbox_runs.output`, or `sandbox_runs.error`. | Integration test: print secret value inside sandbox; grep `sandbox_runs` for the known value |
| FR-12 | The Docker backend injects secrets via the `docker/moby` client `container.Config.Env` field on `ContainerCreate` (the request body, never argv), where the value is the decrypted string, never written to any intermediate file. | Inspect the `ContainerCreate` request struct in a fake moby client; assert no temp file written to disk |
| FR-13 | In file-injection mode, the secret is written to a `0400` temp file (`os.CreateTemp`), bind-mounted read-only into the container (`mount.Mount{Type: bind, ReadOnly: true, Source: <temp>, Target: /run/secrets/name}`), and the temp file is removed after the container run returns. | Assert temp file does not exist after run; assert file was read-only inside container |
| FR-14 | The E2B backend injects secrets via the `envs` field of its create-sandbox HTTP request (Go HTTP client), never via a `commands.run("export ...")` call. | Unit test with a stub E2B transport; assert `envs` field populated, no export command issued |
| FR-15 | The Modal backend injects secrets via the secret-dict field of its function-invoke HTTP request (Go HTTP client). | Unit test with a stub Modal transport; assert the secrets field populated |
| FR-16 | The restricted backend injects secrets by setting `exec.Cmd.Env` to a filtered slice built from `os.Environ()` with the secrets merged in (no inheritance of unrelated host env). | Assert `Cmd.Env` contains the secret value and excludes filtered host vars |
| FR-17 | Every secret operation writes a row to `secret_audit` in `tag.sqlite3` inside the same transaction as the main operation. If the audit write fails, the transaction is rolled back and an error is returned. | Inject a DB write failure; assert main operation not committed |
| FR-18 | `secret audit` filters by `--name`, `--op`, `--since`, and `--last N`; all filters combine with AND semantics. | Test each filter in isolation and in combination |
| FR-19 | `secret rotate` generates a new 96-bit nonce, re-encrypts with the same vault key, updates the row atomically in a single SQLite transaction; the old ciphertext is overwritten. | Assert new nonce != old nonce; assert row count unchanged; assert old ciphertext not retrievable |
| FR-20 | When `--secret NAME` references a secret that does not exist in the vault, `sandbox run` exits with code 1 and prints `Error: secret 'NAME' not found in vault.` before starting any sandbox process. | Test with non-existent secret name |
| FR-21 | Decrypted secret values are held in `[]byte` (never `string`) and explicitly zeroed after use via a `zeroBytes(b []byte)` helper (a `for i := range b { b[i] = 0 }` loop the compiler must not elide). | Unit test inspects the slice contents after vault read returns; assert all-zero |
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
| NFR-05 | Cross-platform support | `internal/credentials/vault` must run on macOS 12+, Ubuntu 20.04+, Windows 10+ (where Docker backend is available); `go-keyring` integration tested on macOS Keychain and Linux Secret Service |
| NFR-06 | Dependency minimalism | AES-256-GCM (`crypto/aes`+`crypto/cipher`) and Argon2id (`golang.org/x/crypto/argon2`) are the only crypto deps and require no cgo; `github.com/zalando/go-keyring` is an optional module; the SQLite store reuses the project-wide `modernc.org/sqlite` driver |
| NFR-07 | Concurrency safety | Vault operations are safe to call from multiple goroutines (WAL-mode SQLite with a serialized writer; key derivation is idempotent and cached behind a `sync.Mutex`) |
| NFR-08 | No secret in core dumps | On Linux, call `unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0)` (`golang.org/x/sys/unix`) before decryption if running as root; document that users should set `ulimit -c 0` for defense-in-depth |
| NFR-09 | Test coverage | `internal/credentials/vault` coverage >= 90% (`go test -cover`); integration tests cover all four sandbox backends |
| NFR-10 | Code auditability | `internal/credentials/vault` is a standalone package with no import of `internal/agent`; it imports only stdlib + `golang.org/x/crypto` + `modernc.org/sqlite` + optional `zalando/go-keyring`; target < 600 lines |

---

## 10. Technical Design

### 10.1 New Files

| File | Purpose |
|------|---------|
| `internal/credentials/vault/vault.go` | Vault client: key derivation, AES-256-GCM encrypt/decrypt, vault DB CRUD, audit writes |
| `internal/credentials/vault/schema.sql` | SQL DDL (`go:embed`ed) for vault.db and the `secret_audit` table migration |
| `internal/credentials/vault/vault_test.go` | Table-driven unit tests for the vault package |
| `internal/sandbox/secret_inject.go` | `InjectSecrets` bridge + `Backend` env/file wiring |
| `internal/sandbox/secret_inject_test.go` | Integration tests for the `--secret` flag across all backends |

The vault package lives under `internal/credentials` (the 18-source credential + execution-backend home) and is imported by `internal/sandbox` for injection and by `internal/cli` for the `tag sandbox secret` command tree (cobra).

### 10.2 SQLite DDL

#### `~/.tag/vault/vault.db` (separate database, mode 0600)

The DDL is stored as an embedded `schema.sql` and applied by `vault.EnsureSchema(db *sql.DB)` over the pure-Go `modernc.org/sqlite` driver.

```sql
-- vault.db: encrypted secrets store
-- Applied by vault.EnsureSchema() over modernc.org/sqlite
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

This table is added to the main store by `store.migratePRD097SecretAudit(db)`, registered in the `internal/store` migration chain. It is a `CREATE TABLE IF NOT EXISTS`, so it is idempotent; had it needed `ALTER TABLE ... ADD COLUMN`, the migration would swallow the `duplicate column name` error returned by the driver (the standard guarded-migration idiom used across `internal/store`).

```sql
-- Added by store.migratePRD097SecretAudit() in the internal/store migration chain
CREATE TABLE IF NOT EXISTS secret_audit (
  id          TEXT    PRIMARY KEY,   -- UUID4
  name        TEXT    NOT NULL,      -- secret name (never value)
  operation   TEXT    NOT NULL,      -- add|delete|inject|list|export|rotate
  run_id      TEXT,                  -- sandbox_runs.id if operation=inject, else NULL
  os_user     TEXT    NOT NULL,      -- os/user.Current().Username
  is_warning  INTEGER NOT NULL DEFAULT 0,   -- 1 for export operations
  created_at  TEXT    NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_sa_name       ON secret_audit(name, created_at);
CREATE INDEX IF NOT EXISTS idx_sa_run_id     ON secret_audit(run_id) WHERE run_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_sa_operation  ON secret_audit(operation, created_at);
```

### 10.3 Core Types

```go
// Package vault: internal/credentials/vault/vault.go
package vault

import (
	"database/sql"
	"path/filepath"
	"sync"
	"time"
)

// InjectMode is a typed string constant (replaces the Python enum-by-convention).
type InjectMode string

const (
	InjectEnv  InjectMode = "env"
	InjectFile InjectMode = "file"
)

// Operation is the audited operation kind.
type Operation string

const (
	OpAdd    Operation = "add"
	OpDelete Operation = "delete"
	OpInject Operation = "inject"
	OpList   Operation = "list"
	OpExport Operation = "export"
	OpRotate Operation = "rotate"
)

// Config holds paths and KDF parameters for the vault.
type Config struct {
	VaultDir        string // default: ~/.tag/vault
	DBName          string // default: "vault.db"
	KDFTime         uint32 // Argon2id time cost, default 3
	KDFMemory       uint32 // Argon2id memory in KiB, default 65536 (64 MiB)
	KDFParallelism  uint8  // default 1
	KDFKeyLen       uint32 // default 32 (256-bit AES key)
	KeyringService  string // default "tag-vault"
	KeyringUsername string // default "master-key"
}

func (c Config) DBPath() string      { return filepath.Join(c.VaultDir, c.DBName) }
func (c Config) KeyFallback() string { return filepath.Join(c.VaultDir, ".key") }

// DefaultConfig returns the vault defaults.
func DefaultConfig(home string) Config {
	return Config{
		VaultDir:        filepath.Join(home, ".tag", "vault"),
		DBName:          "vault.db",
		KDFTime:         3,
		KDFMemory:       64 * 1024,
		KDFParallelism:  1,
		KDFKeyLen:       32,
		KeyringService:  "tag-vault",
		KeyringUsername: "master-key",
	}
}

// SecretMeta is vault metadata for a single secret (never carries the plaintext).
type SecretMeta struct {
	Name          string            `json:"name"`
	Description   string            `json:"description"`
	Tags          map[string]string `json:"tags"`
	SizeBytes     int               `json:"size_bytes"`
	InjectMode    InjectMode        `json:"inject_mode"`
	FileMountPath string            `json:"file_mount_path,omitempty"`
	FileMode      uint32            `json:"file_mode"` // octal, e.g. 0o400
	CreatedAt     time.Time         `json:"created_at"`
	UpdatedAt     time.Time         `json:"updated_at"`
	LastUsedAt    *time.Time        `json:"last_used_at,omitempty"`
}

// AuditEvent is a single secret audit record.
type AuditEvent struct {
	ID        string    `json:"id"` // UUIDv4
	Name      string    `json:"name"`
	Operation Operation `json:"operation"`
	RunID     string    `json:"run_id,omitempty"`
	OSUser    string    `json:"os_user"`
	IsWarning bool      `json:"is_warning"`
	CreatedAt time.Time `json:"created_at"`
}

// Vault is the stateful client. The derived key is cached behind keyMu for the
// process lifetime (idempotent KDF); it is never logged or serialized.
type Vault struct {
	cfg    Config
	db     *sql.DB
	keyMu  sync.Mutex
	key    []byte // 32-byte AES-256 key, or nil until unlocked
}
```

### 10.4 Vault Client Core Algorithms

```go
import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"

	"golang.org/x/crypto/argon2"
)

// deriveKey performs Argon2id password-based derivation. Returns a 32-byte AES-256 key.
// The result is cached on the Vault for the process lifetime (see unlock()).
func (c Config) deriveKey(passphrase, salt []byte) []byte {
	return argon2.IDKey(passphrase, salt, c.KDFTime, c.KDFMemory, c.KDFParallelism, c.KDFKeyLen)
}

// encrypt performs AES-256-GCM sealing. Returns (ciphertext, nonce, authTag).
// The 16-byte GCM tag is split out to match the vault column layout; on read it
// is re-appended before Open.
func encrypt(plaintext, key []byte) (ciphertext, nonce, authTag []byte, err error) {
	block, err := aes.NewCipher(key) // key len 32 => AES-256
	if err != nil {
		return nil, nil, nil, err
	}
	gcm, err := cipher.NewGCM(block) // 12-byte nonce, 16-byte tag
	if err != nil {
		return nil, nil, nil, err
	}
	nonce = make([]byte, gcm.NonceSize()) // 96-bit random nonce
	if _, err = rand.Read(nonce); err != nil {
		return nil, nil, nil, err
	}
	sealed := gcm.Seal(nil, nonce, plaintext, nil) // ciphertext||tag
	tagLen := gcm.Overhead()                         // 16
	return sealed[:len(sealed)-tagLen], nonce, sealed[len(sealed)-tagLen:], nil
}

// decrypt performs AES-256-GCM opening. Returns a non-nil error on authentication
// failure (the Go equivalent of cryptography's InvalidTag). Caller zeroes plaintext.
func decrypt(ciphertext, nonce, authTag, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	sealed := append(append([]byte{}, ciphertext...), authTag...)
	pt, err := gcm.Open(nil, nonce, sealed, nil)
	if err != nil {
		return nil, errors.New("vault: authentication failed (tampered ciphertext or wrong key)")
	}
	return pt, nil
}

// zeroBytes overwrites b in place. Written as an explicit loop so the value is
// clobbered even though Go's GC may have copied the backing array elsewhere.
func zeroBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
```

> **Note on Fernet parity:** the Python design used `cryptography`'s Fernet/`AESGCM`. Fernet is a higher-level authenticated-encryption token format; the Go re-frame drops it in favor of `crypto/cipher` AES-256-GCM directly (authenticated encryption with a per-secret random nonce), which is equivalent in guarantees and avoids a third-party token format. `golang.org/x/crypto/nacl/secretbox` (XSalsa20-Poly1305) or `chacha20poly1305` are documented drop-in alternatives if a non-AES AEAD is preferred; AES-256-GCM is the default because it has hardware acceleration on all target CPUs.

### 10.5 Injection Bridge: `InjectSecrets`

This is the core bridge between the vault and the sandbox `Backend` interface. It lives in `internal/sandbox/secret_inject.go`:

```go
// FileMount pairs a host temp file with its in-sandbox target path (file-mode secrets).
type FileMount struct {
	HostPath      string // 0400 temp file the caller must remove after the run
	ContainerPath string // e.g. /run/secrets/tls.crt
	Mode          uint32 // e.g. 0o400
}

// InjectSecrets decrypts the named vault secrets and produces the environment
// additions (env-mode) and file mounts (file-mode) for a sandbox run.
//
// mergedEnv is the []string ("KEY=VALUE") slice to hand to the Backend.
// fileMounts must be removed by the caller after the sandbox process exits.
// An 'inject' audit row is written for each secret inside the store transaction.
// All decrypted []byte buffers are zeroed before InjectSecrets returns; only the
// string form required at the OS boundary escapes into mergedEnv.
func InjectSecrets(
	ctx context.Context,
	v *vault.Vault,
	names []string,
	baseEnv []string,
	store *sql.DB,
	runID string,
) (mergedEnv []string, fileMounts []FileMount, err error)
```

`InjectSecrets` returns an error before any process starts if a named secret is missing (FR-20), so the caller aborts the run without writing a `sandbox_runs` row.

### 10.6 Integration Points with `internal/sandbox`

Secret delivery is expressed through the `Backend` interface so each isolation tier
(process/restricted, docker/moby, gVisor, firecracker) injects secrets in its native
way. The `RunOpts` gains `SecretNames`, and `RunSandbox` wires injection in:

```go
// Backend is the isolation-ladder interface (process -> docker/moby -> gVisor -> firecracker).
type Backend interface {
	// Run executes cmd with the given environment and file mounts, honoring ctx
	// for cancellation/timeout. Env is []string of "KEY=VALUE"; Mounts are the
	// decrypted secret files (and any workspace mounts).
	Run(ctx context.Context, cmd []string, env []string, mounts []FileMount) (Result, error)
}

type RunOpts struct {
	Backend     string        // "restricted" | "docker" | "gvisor" | "firecracker" | "e2b" | "modal"
	Image       string
	Timeout     time.Duration
	Workdir     string
	Code        string
	SecretNames []string // NEW — resolved against the vault at run time
}

func RunSandbox(ctx context.Context, v *vault.Vault, store *sql.DB, opts RunOpts) (Result, error) {
	ctx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	env, mounts := safeBaseEnv(), []FileMount(nil)
	if len(opts.SecretNames) > 0 {
		var err error
		env, mounts, err = InjectSecrets(ctx, v, opts.SecretNames, safeBaseEnv(), store, runID)
		if err != nil {
			return Result{}, err // aborts before any process starts (FR-20)
		}
	}
	// Ensure temp secret files are removed regardless of outcome (FR-13).
	defer func() {
		for _, m := range mounts {
			_ = os.Remove(m.HostPath)
		}
	}()

	be := selectBackend(opts.Backend)
	return be.Run(ctx, buildCommand(opts), env, mounts)
}
```

Per-backend env injection (never on argv, never in `sandbox_runs.command`):

- **restricted / process** — `exec.CommandContext(ctx, ...)` with `cmd.Env = env`.
- **docker / gVisor** — `docker/moby` client: `container.Config{Env: env}` on `ContainerCreate`; file secrets as `mount.Mount{Type: mount.TypeBind, ReadOnly: true, ...}`.
- **firecracker** — `firecracker-go-sdk`: env delivered via kernel boot-args / MMDS (metadata service), file secrets staged into the guest rootfs image before boot.
- **e2b / modal** — Go HTTP client passes the secrets in the create/invoke request body (`envs` / secret-dict field).

### 10.7 CLI Integration (cobra)

`internal/cli` gains a `tag sandbox secret` command subtree (cobra), mirroring the `internal/cli/sandbox.go` group. Each leaf `RunE` calls the vault package:

```go
// internal/cli/sandbox_secret.go (abbreviated)
func newSandboxSecretCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "secret", Short: "Manage the encrypted sandbox secrets vault"}
	cmd.AddCommand(
		newSecretAddCmd(),        // --name --value --from-env --from-file --description --tags --overwrite
		newSecretListCmd(),       // --json --tags
		newSecretDeleteCmd(),     // <NAME> --yes
		newSecretRotateCmd(),     // --name
		newSecretExportCmd(),     // --name --to-env
		newSecretInjectFileCmd(), // --name --mount --mode
		newSecretAuditCmd(),      // --name --op --since --last --json
	)
	return cmd
}
```

The `--secret` flag is added to the existing `tag sandbox run` command (`StringArrayVar`, repeatable). The `store.migratePRD097SecretAudit()` migration is registered in the `internal/store` migration chain run at DB open.

### 10.8 Vault Unlock Flow

```
tag sandbox secret add --name FOO --value bar
  │
  ▼
1. Check TAG_VAULT_PASSPHRASE env var (os.Getenv)
   If set → use as passphrase (CI mode)
   If not set and stdin is a TTY (golang.org/x/term.IsTerminal) → term.ReadPassword("Vault passphrase: ")
   If not set and no TTY → exit 1 "Set TAG_VAULT_PASSPHRASE for non-interactive use"
  │
  ▼
2. Open vault.db via modernc.org/sqlite (create if not exists; EnsureSchema)
   If new vault: crypto/rand 16-byte salt, insert vault_meta row
   If existing vault: read salt from vault_meta
  │
  ▼
3. Check go-keyring for cached derived key (keyring.Get("tag-vault","master-key"))
   If found → use directly (skip KDF)
   If not found → run argon2.IDKey (~300ms)
                → keyring.Set(...) (or write .key file 0600 on keyring error)
   Cache on Vault.key under keyMu for the process lifetime
  │
  ▼
4. Execute operation (encrypt/decrypt/delete/list) in a store transaction
  │
  ▼
5. Insert audit event into tag.sqlite3 in the SAME transaction, then commit
  │
  ▼
6. zeroBytes() all decrypted []byte buffers
```

---

## 11. Security Considerations

1. **AES-256-GCM authentication tag verification.** Go's `cipher.AEAD.Open` returns a non-nil error (not the plaintext) if the ciphertext has been tampered with — the equivalent of `cryptography`'s `InvalidTag`. This error must propagate to the caller as a hard error, never discarded. Any authentication failure is logged as a security event (without the ciphertext value) and aborts the operation.

2. **Nonce uniqueness.** AES-GCM is catastrophically broken if a (key, nonce) pair is reused. The 96-bit nonce is generated via `crypto/rand.Read` into a `gcm.NonceSize()` (12-byte) buffer for every `add` and `rotate` operation. The probability of a nonce collision for a single key across 2^32 secrets is less than 2^-64 — negligible for local use cases. No counter-based nonce scheme is used.

3. **Vault key never in `sandbox_runs`.** `RunSandbox` writes the `command` string (the user-facing template) to `sandbox_runs.command` before calling `InjectSecrets`. The `[]string` env slice containing decrypted secrets is passed directly to `exec.Cmd.Env`, the moby client's `container.Config.Env`, or the E2B/Modal HTTP request body; it is never concatenated into a loggable command string.

4. **Process list exposure.** The Go Docker backend uses the `docker/moby` API client rather than shelling out to `docker run`: environment variables travel in the `ContainerCreate` request body over the daemon socket, never on any process `argv`. This structurally eliminates the `/proc/<pid>/cmdline` exposure window that the CLI (`docker run --env NAME=VALUE`) had — a strict improvement over the Python design. For the restricted backend, `exec.Cmd.Env` is likewise passed via the process environment (not argv). On macOS, process argument inspection is restricted to root by default.

5. **`PRAGMA secure_delete = ON`.** The vault DB uses SQLite's `secure_delete` pragma, which causes SQLite to overwrite deleted database pages with zeros before freeing them. This is defense-in-depth against recovery of deleted secret bytes from disk.

6. **OS keychain as secondary storage.** The derived key (not the passphrase, not any secret value) is stored in the OS keychain. The keychain entry is 32 bytes — not the secret itself. Even if the keychain is compromised, an attacker needs the vault DB to recover secrets. Even if the vault DB is copied, an attacker needs the keychain key or the passphrase to decrypt.

7. **Vault directory permissions.** `vault.ensureVaultDir()` creates `~/.tag/vault/` with mode `0700` (owner-only access) via `os.MkdirAll` + `os.Chmod`. The vault DB is created with mode `0600`. If the directory or file is found with broader permissions, a warning is printed and the operation is aborted.

8. **Memory zeroing is best-effort in Go.** Go `string` values are immutable and garbage-collected; once a secret is materialized as a `string`, the bytes cannot be reliably wiped because the runtime may have copied them. The vault API therefore carries decrypted values as `[]byte` throughout and calls `zeroBytes()` before returning; the zeroing loop is written so the compiler cannot elide it. Callers must not convert to `string` until the last possible moment. The final `KEY=VALUE` entries in the `[]string` env passed to `exec.Cmd.Env` / the moby API are unavoidably string-typed at the OS boundary; the GC may relocate them, so this residual exposure is documented, not eliminated (identical to the CPython constraint).

9. **Export operation is audited with warning flag.** `secret export` is the only operation that intentionally exposes a plaintext value outside the sandbox. Its `secret_audit` row has `is_warning=1`. A separate `tag sandbox secret audit --op export` query surfaces all historical export events for review.

10. **`TAG_VAULT_PASSPHRASE` in CI.** When the passphrase is taken from the environment variable, a notice is printed to stderr: `Note: unlocking vault from TAG_VAULT_PASSPHRASE. Ensure this variable is a masked CI secret.` This is informational only and does not block the operation.

11. **Vault integrity check on open.** On every `vault.db` open, `_check_vault_integrity()` runs `PRAGMA integrity_check` (fast mode, max 1 error). A non-OK result aborts with `Error: vault database integrity check failed. Do not use this vault.`

12. **Argon2id parameter enforcement.** The KDF parameters are stored in `vault_meta` at creation time. On subsequent opens, the stored parameters are read and compared against the hardcoded minimums (`time_cost >= 3`, `memory_cost >= 65536`). If the vault was created with weaker parameters (e.g., migration from a test environment), a warning is printed and re-derivation with current parameters is offered.

---

## 12. Testing Strategy

Tests use Go's `testing` package, table-driven cases where the inputs vary, and interfaces + dependency injection for mocking (a `Keyring` interface and a `Backend` interface, both stubbed in tests). No global monkeypatching; the OS-keychain and sandbox backends are injected.

### 12.1 Unit Tests (`internal/credentials/vault/vault_test.go`)

| Test | Description |
|------|-------------|
| `TestEncryptDecryptRoundtrip` | Encrypt a known plaintext, decrypt, assert equality; assert nonce is 12 bytes |
| `TestNonceUniqueness` | Call `encrypt` 1000 times; assert all nonces are distinct |
| `TestOpenRejectsTamperedCiphertext` | Corrupt 1 byte of ciphertext; assert `decrypt` returns a non-nil error |
| `TestArgon2idParams` | Assert `deriveKey` calls `argon2.IDKey` with `time=3, memory=64*1024, threads=1, keyLen=32` (verified via a known-answer vector) |
| `TestVaultDirMode0700` | Call `ensureVaultDir` on a temp path; assert directory mode via `os.Stat` |
| `TestVaultDBMode0600` | After first `AddSecret`, assert vault DB file mode |
| `TestNameValidationRejectsLowercase` | `AddSecret` with `lower_case`; assert error |
| `TestNameValidationAcceptsUppercase` | `AddSecret` with `VALID_KEY`; assert no error |
| `TestValueSizeLimit` | Pass 65537-byte value; assert error returned before DB write |
| `TestListContainsNoValues` | Add secret, call `ListSecrets`; assert returned `SecretMeta` has no value field |
| `TestDeleteZerosCiphertext` | After delete, assert the row's ciphertext column was overwritten before DELETE |
| `TestRotateNewNonce` | `RotateSecret`; assert new nonce != old nonce; assert decrypt still works |
| `TestZeroBytesAfterDecrypt` | Inspect the `[]byte` slice after `zeroBytes(buf)`; assert all zero |
| `TestAuditWrittenOnAdd` | Add secret; assert `secret_audit` row count == 1, operation == `add` |
| `TestAuditWrittenOnInject` | Call `InjectSecrets`; assert audit row with operation == `inject` |
| `TestAuditRollbackOnMainFailure` | Inject a store failure; assert the transaction rolls back and no audit row is written |
| `TestKeyringCacheHit` | Stub `Keyring`; assert `deriveKey` not called on second unlock (key cached under `keyMu`) |
| `TestEnvVarPassphrase` | Set `TAG_VAULT_PASSPHRASE`; call add with no TTY; assert no prompt |
| `TestSecureDeletePragma` | Open vault DB; `PRAGMA secure_delete`; assert `= 1` |
| `TestIntegrityCheckOnOpen` | Corrupt vault DB; assert `Open` returns an integrity error |

### 12.2 Integration Tests (`internal/sandbox/secret_inject_test.go`)

| Test | Description |
|------|-------------|
| `TestRestrictedBackendEnvInjection` | Add secret, run with restricted backend, assert value appears in `cmd.Env` via echo; assert `sandbox_runs.command` does not contain value |
| `TestDockerBackendEnvInjection` | (requires Docker; `t.Skip` otherwise) Add secret, run docker sandbox via moby client, assert env var accessible inside container |
| `TestDockerBackendFileInjection` | (requires Docker) Add secret with `InjectFile`, run, assert file readable at mount path inside container; assert temp file removed on host after run |
| `TestNonexistentSecretHardError` | Pass `--secret DOES_NOT_EXIST`; assert error before any backend `Run`; no `sandbox_runs` row created |
| `TestMultipleSecretsInjected` | Add 3 secrets, pass all 3; assert all 3 env vars accessible inside sandbox |
| `TestOutputDoesNotContainSecret` | Sandboxed code echoes secret; assert `sandbox_runs.command` column does NOT contain the known value |
| `TestAuditRunIDLinked` | After injection run, assert `secret_audit.run_id` matches `sandbox_runs.id` |

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
| `crypto/aes` + `crypto/cipher` | stdlib | — | AES-256-GCM authenticated encryption (`cipher.AEAD`) | Yes (Go stdlib) |
| `crypto/rand` | stdlib | — | CSPRNG for nonce and salt generation | Yes (Go stdlib) |
| `golang.org/x/crypto/argon2` | Hard (new) | latest | Argon2id KDF via `argon2.IDKey` | No (add to go.mod) |
| `github.com/zalando/go-keyring` | Soft (optional) | latest | OS keychain integration for derived key storage | No (add to go.mod) |
| `modernc.org/sqlite` | Hard | project-wide | Pure-Go SQLite driver for vault.db + audit table (CGO_ENABLED=0) | Yes (project driver) |
| `golang.org/x/term` | Hard | latest | Passphrase prompt without echo (`ReadPassword`) | Likely (CLI already uses it) |
| `golang.org/x/sys/unix` | Hard | latest | `Prctl(PR_SET_DUMPABLE, 0)` core-dump suppression (Linux) | Likely |
| PRD-028 (`internal/sandbox`) | Internal | — | `RunSandbox` / `Backend` interface to extend with `SecretNames` | Yes (implemented) |
| PRD-034 (`internal/obs` scanning) | Internal | — | Pattern library reference for audit log filtering | Yes (implemented) |
| PRD-013 (`internal/obs` tracing) | Internal | — | Audit event correlation with OTel spans | Yes (implemented) |
| PRD-008 (`internal/queue`) | Internal | — | Future: queue jobs referencing vault secrets | Yes (implemented) |

---

## 15. Open Questions

| ID | Question | Owner | Target Resolution |
|----|----------|-------|-------------------|
| OQ-01 | Should the vault passphrase be derivable from the user's system login password (PAM integration on Linux, Keychain login keychain on macOS) so users never need to set a separate passphrase? | Security lead | Before implementation kickoff |
| OQ-02 | Should `secret add` accept a `--ttl <duration>` so secrets auto-expire? Useful for short-lived CI tokens. | Product | v2 consideration; not blocking v1 |
| OQ-03 | The Go Docker backend uses the `docker/moby` API client (env travels in the `ContainerCreate` body, so cmdline exposure is already eliminated). Should we still support a `docker` CLI fallback for hosts without socket access, and if so, use `--env-file` via an `os.Pipe` rather than `--env NAME=VALUE`? | Engineering | During implementation |
| OQ-04 | Should `inject_secrets_into_env` scrub the `sandbox_runs.output` column post-run using the known secret values as a regex? This would prevent secrets echoed by careless code from persisting. The counterargument is that scrubbing may mask debugging information. | Security + Product | Design review |
| OQ-05 | Should the vault support named secret groups (analogous to Modal's `Secret` objects), where `tag sandbox run --secret-group MY_APP_SECRETS` injects all secrets tagged with that group? | Product | v2 |
| OQ-06 | Should `tag sandbox secret audit` be surfaced in the existing `tag trace` output (e.g., as child spans under a sandbox run span in PRD-013)? | Engineering | PRD-013 owner alignment |
| OQ-07 | Is Argon2id `memory_cost=65536` (64 MiB) acceptable on systems with < 256 MiB free RAM (e.g., embedded CI runners, Raspberry Pi)? Should we add a `--kdf-profile fast|standard|strong` flag? | Engineering | Benchmark on target CI hardware |
| OQ-08 | For E2B backend: E2B's `Sandbox.create(envs={...})` passes secrets to the Firecracker VM at creation time, but E2B logs sandbox creation events server-side. Do E2B's server-side logs capture the `envs` dict? Need to confirm with E2B support or SDK source. | Engineering | E2B support inquiry |

---

## 16. Complexity and Timeline

### Phase 1 — Vault Core (Days 1–4)

- Implement `internal/credentials/vault`: `Config`, `SecretMeta`, `AuditEvent`, `Vault`, and the `InjectMode`/`Operation` typed constants.
- Implement `ensureVaultDir()`, `EnsureSchema()` (embedded `schema.sql`), `Open()` over `modernc.org/sqlite`.
- Implement Argon2id KDF (`argon2.IDKey`) with `go-keyring` integration behind a `Keyring` interface and `.key` fallback.
- Implement `encrypt()`, `decrypt()`, `zeroBytes()`.
- Implement `AddSecret()`, `ListSecrets()`, `DeleteSecret()`, `RotateSecret()`.
- Table-driven unit tests for all crypto primitives and vault CRUD (AC-01, AC-03, AC-04, AC-06, AC-11, AC-12).

### Phase 2 — Audit Layer (Day 5)

- Register `store.migratePRD097SecretAudit()` in the `internal/store` migration chain.
- Implement `WriteAuditEvent()` in the vault package.
- Ensure audit writes share the main operation's `*sql.Tx` (FR-17).
- Tests: AC-05, AC-13.

### Phase 3 — Sandbox Integration (Days 6–8)

- Add `SecretNames` to `RunOpts`; wire `InjectSecrets` into `RunSandbox`.
- Implement per-`Backend` injection (restricted via `exec.Cmd.Env`, docker/gVisor via moby `container.Config.Env`, firecracker via MMDS/boot-args, e2b/modal via HTTP body).
- Implement file-injection mode with temp-file lifecycle via `defer os.Remove` (FR-13).
- Integration tests for restricted and Docker backends (AC-02, AC-09, AC-10, AC-14).
- CLI wiring: `tag sandbox secret` cobra subtree in `internal/cli`.

### Phase 4 — CLI Surface + Audit Query (Days 9–10)

- Implement all `tag sandbox secret` subcommands: `add`, `list`, `delete`, `rotate`, `export`, `inject-file`, `audit` (cobra).
- `--json` output for `list` and `audit` via `encoding/json` (AC-15).
- `tag sandbox run --secret` flag plumbing (`StringArrayVar`).
- End-to-end CLI tests matching AC-01 through AC-15.
- Documentation update in `docs/prd/INDEX.md`.

### Phase 5 — Hardening and Review (Days 11–14)

- Security review: verify `[]byte` zeroing, nonce uniqueness, permission checks, moby-vs-CLI process exposure.
- Benchmarks (`go test -bench`): KDF latency, inject latency, 10k-secret list.
- Address OQ-03 (moby client vs docker CLI `--env-file` fallback) if decided.
- Address OQ-04 (output scrubbing) if decided.
- Coverage gate: `internal/credentials/vault` >= 90% (`go test -cover`).
- PR review and merge.

**Total estimated effort: 10–14 engineering days (M size estimate confirmed).**

