# PRD-035: Profile Marketplace (tag profile pull/push)

**Status:** Proposed — BLOCKED on PRD-034 (Secret Scanning)
**Priority:** P1
**Estimated Effort:** M (1 sprint, ~2 weeks)
**Affects:** `controller.py` (new `cmd_profile_hub`), new `src/tag/profile_hub.py`
**Security Classification:** HIGH SECURITY RISK if implemented before PRD-034

> **WARNING:** This feature downloads YAML files containing system prompts from arbitrary
> GitHub repositories. System prompts can carry prompt injection payloads. Activation of any
> downloaded profile MUST be gated behind mandatory secret scanning (PRD-034) and an explicit
> `--trust` flag. Shipping this PRD without PRD-034 in place creates a supply-chain attack
> vector against every TAG installation.

---

## 1. Overview

TAG profiles encode significant operational knowledge: system prompts, model selection, tool
permissions, memory backends, routing rules, and budget limits. Today, sharing a profile
requires manually copying YAML files out of `~/.tag/` and sending them over a side channel.
There is no way to discover, distribute, or pin community profiles.

This PRD defines the **Profile Marketplace**: a GitHub-based distribution system for Hermes
profiles that lets users pull a profile from any GitHub repo or Gist, push their own profiles
as Gists or repo commits, browse available profiles from a repo tree, inspect diffs before
activation, and lock installed profiles to exact commit SHAs to prevent silent supply-chain
mutations.

The design draws directly on the precedent set by **simonw/llm** (PyPI-based plugin registry)
and the community template corpus in **codebyNJ/hermes-octo**. Unlike llm plugins, TAG
profiles are pure YAML data — no code execution — but the system prompts they contain are
treated as untrusted input until explicitly trusted by the user.

---

## 2. Goals

1. **GitHub-based profile distribution** — Any GitHub repo or Gist can serve as a profile
   source. No central TAG-operated registry server is required.
2. **SHA-pinned lock file** — Every installed profile is pinned to an exact Git commit SHA
   in `profiles.lock`, preventing silent upstream mutations.
3. **Mandatory security scan before activation** — Integration with PRD-034 (Secret Scanning)
   runs on every fetched profile before the user can activate it. Profiles containing secrets
   or high-confidence prompt injection patterns are blocked unless explicitly overridden.
4. **Versioning support** — Users can pin to a specific SHA, update to the latest commit, and
   roll back to a previously pinned version using the lock file.
5. **Local profile registry** — A structured `profiles.lock` in `TAG_HOME` tracks every
   installed marketplace profile: source URL, SHA, install timestamp, and trust decision.
6. **Diff-before-activate workflow** — `tag profile diff` shows a coloured YAML diff between
   the installed version and the remote HEAD before the user decides to update.
7. **Push to Gist/GitHub** — Users can publish a local profile (with secrets automatically
   stripped) as a public or private GitHub Gist or as a commit to a target repo.
8. **Offline mode** — When the network is unavailable, TAG falls back to the locked version
   on disk rather than failing.

---

## 3. Non-Goals

- **Centralized hub server** — TAG will not operate a profile registry API. GitHub is the
  registry.
- **Paid or gated profiles** — All marketplace profiles are fetched from public or
  user-accessible GitHub resources.
- **Profile execution sandboxing** — Isolating the runtime effects of a profile's system
  prompt is a separate concern addressed in a future PRD.
- **Automatic profile updates** — Updates are explicit user actions (`tag profile lock`).
  No background polling or auto-upgrade mechanism.
- **Profile signing with GPG** — SHA pinning provides integrity. Cryptographic author
  identity is a future enhancement.
- **Profile dependency resolution** — Profiles that depend on other profiles are out of
  scope. Dependencies on MCP servers are declared informatively only.

---

## 4. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|------------|----------|
| U1 | Developer | run `tag profile pull codebyNJ/hermes-octo/profiles/researcher.yaml` | I get a production-grade researcher profile without writing it from scratch |
| U2 | Developer | run `tag profile pull <owner>/<repo> --pin abc1234` | I lock my installation to a known-good commit and prevent silent upstream changes |
| U3 | Power user | run `tag profile push researcher --to gist --public` | I share my tuned researcher profile with the community via a public Gist |
| U4 | Developer | run `tag profile diff researcher --remote` | I review exactly what changed in the upstream profile before deciding to update |
| U5 | Team lead | run `tag profile list --remote codebyNJ/hermes-octo` | I browse all available profiles in a community repo before choosing one to install |
| U6 | Developer | run `tag profile lock` | I capture the current SHAs of all installed remote profiles into `profiles.lock` for reproducible deploys |
| U7 | Security engineer | receive a clear error when `tag profile pull` detects a secret in the fetched YAML | I am protected from accidentally activating a profile that exfiltrates credentials |
| U8 | Developer | run `tag profile verify researcher` | I re-run the security scan on an already-installed profile after updating my secret patterns |
| U9 | Developer | run `tag profile uninstall researcher` | I remove a marketplace profile and its lock entry cleanly |
| U10 | Developer | pull a community profile, fork and edit it locally, then push the fork as a new Gist | I customise an existing community profile and publish my variant |

---

## 5. Proposed CLI Surface

All subcommands are grouped under `tag profile`. Existing `tag profile` subcommands
(`create`, `edit`, `switch`, etc.) are unaffected.

### 5.1 pull

```
tag profile pull <owner>/<repo>[/<path/to/profile.yaml>] [--pin <sha>] [--trust]
```

- Resolves the GitHub raw URL for the specified path (default branch unless `--pin` supplied).
- Fetches raw YAML content over HTTPS.
- Runs PRD-034 secret scan. Blocks activation if secrets detected unless `--trust` passed.
- Shows a summary of the profile (name, description, system prompt preview).
- Requires explicit `--trust` flag to write the profile to `TAG_HOME/profiles/` and activate.
- Writes an entry to `profiles.lock`.
- Without `--trust`, saves the fetched YAML to a staging area and prints instructions.

### 5.2 push

```
tag profile push <profile-name> [--to gist|github] [--public|--private]
```

- Reads the named profile YAML from `TAG_HOME/profiles/`.
- Strips any values matching secret patterns (via PRD-034 scanner) before publishing.
- `--to gist` (default): creates a GitHub Gist via the GitHub API.
- `--to github`: commits to a branch in a user-specified repo (`--repo <owner>/<repo>`).
- `--public` / `--private` control Gist visibility (default: `--private`).
- Prints the resulting Gist URL or commit SHA.

### 5.3 list --remote

```
tag profile list --remote <owner>/<repo>
```

- Fetches the GitHub tree API for `<owner>/<repo>` at HEAD.
- Filters entries matching `*.yaml` under a `profiles/` directory (configurable via
  `hub.profiles_dir` in `cli-config.yaml`).
- Prints a table: profile filename, last-modified SHA, size.
- Respects GitHub API rate limits; prints remaining rate limit on `--verbose`.

### 5.4 diff

```
tag profile diff <profile-name> [--remote]
```

- Without `--remote`: diffs the installed profile against the staged (pre-trust) version.
- With `--remote`: fetches the current HEAD from the source recorded in `profiles.lock` and
  diffs against the installed version.
- Output is a unified diff with YAML context lines, printed to stdout.
- Exits with code 1 if differences exist (suitable for CI use).

### 5.5 lock

```
tag profile lock
```

- For every entry in `profiles.lock` that has a `source_url`, re-fetches the current HEAD
  SHA from the GitHub API (no content download) and updates the `sha` field.
- Does NOT fetch content or update installed files — only records the new SHA.
- Prints a summary of profiles where the recorded SHA differs from HEAD (indicating
  available updates).

### 5.6 verify

```
tag profile verify <profile-name>
```

- Reads the installed profile YAML from `TAG_HOME/profiles/`.
- Re-runs the PRD-034 security scan with current patterns.
- Prints PASS / WARN / FAIL per check category.
- Exits non-zero if any check fails.

### 5.7 uninstall

```
tag profile uninstall <profile-name>
```

- Removes the profile YAML from `TAG_HOME/profiles/`.
- Removes the entry from `profiles.lock`.
- Refuses to uninstall the currently active profile; prompts the user to switch first.

---

## 6. Functional Requirements

| ID | Requirement |
|----|-------------|
| FR-01 | The system MUST fetch profile YAML via `https://raw.githubusercontent.com/<owner>/<repo>/<ref>/<path>` or the GitHub Gist raw URL. Plain HTTP fetches MUST be rejected. |
| FR-02 | Every installed remote profile MUST have an entry in `profiles.lock` recording `source_url`, `sha` (full 40-char commit SHA), `installed_at` (ISO-8601 UTC), and `trusted_by` (git user email from `git config user.email` or `TAG_USER`). |
| FR-03 | Before any profile content is written to `TAG_HOME/profiles/`, the PRD-034 secret scanner MUST run on the raw YAML bytes. If the scanner returns severity `HIGH`, the install MUST be aborted unless `--trust` is passed with explicit acknowledgement. |
| FR-04 | The `--trust` flag MUST require the user to confirm they have reviewed the profile. In non-interactive mode (piped stdin), `--trust` alone is sufficient; in interactive mode a confirmation prompt is shown. |
| FR-05 | When `--pin <sha>` is provided, the fetch URL MUST use that exact SHA as the Git ref. The resolved SHA in `profiles.lock` MUST match the provided value; a mismatch MUST abort the install. |
| FR-06 | YAML format validation MUST be run on every fetched profile before the security scan. Malformed YAML MUST abort the install with a descriptive error. |
| FR-07 | `tag profile diff` MUST produce output compatible with standard `diff` tooling (unified diff format). The exit code MUST be 0 if no differences, 1 if differences exist. |
| FR-08 | `tag profile push` MUST run the PRD-034 scanner on the profile before uploading and MUST strip or mask any detected secret values. The user MUST be shown a summary of stripped fields before the push proceeds. |
| FR-09 | `tag profile list --remote` MUST use the GitHub Trees API (`GET /repos/{owner}/{repo}/git/trees/{sha}?recursive=1`) and filter for YAML files under the configured `profiles_dir`. |
| FR-10 | `tag profile lock` MUST only update SHA records in `profiles.lock` — it MUST NOT overwrite installed profile files. |
| FR-11 | All GitHub API calls MUST include a `User-Agent: tag-agent/<version>` header and MUST support `GITHUB_TOKEN` from the environment for authenticated requests (higher rate limits). |
| FR-12 | The fetch timeout MUST default to 10 seconds and MUST be configurable via `hub.fetch_timeout_seconds` in `cli-config.yaml`. |
| FR-13 | Retry logic: failed fetches MUST be retried up to 3 times with exponential backoff (1 s, 2 s, 4 s). 404 responses MUST NOT be retried. |
| FR-14 | Version tracking: `profiles.lock` MUST record the TAG version that performed the install in a `tag_version` field, enabling future migration logic. |
| FR-15 | Update check: `tag profile list` (local, no `--remote`) MUST compare installed SHAs against `profiles.lock` entries and flag any profile where the installed file's content SHA (SHA-256) does not match the expected value, indicating local tampering. |
| FR-16 | `tag profile uninstall` MUST refuse to remove the profile currently set as `defaults.master_profile` in `default.yaml` and MUST print a clear error directing the user to switch profiles first. |
| FR-17 | The `profiles.lock` file MUST be human-readable YAML (not JSON) and MUST be suitable for committing to version control. |
| FR-18 | Staging area: profiles fetched without `--trust` MUST be written to `TAG_HOME/profile-staging/<name>-<sha[:8]>.yaml` and MUST NOT be written to the active profiles directory. |

---

## 7. Non-Functional Requirements

| ID | Requirement |
|----|-------------|
| NFR-01 | **Offline mode** — If `profiles.lock` exists and `--offline` is passed (or network is unreachable), `tag profile pull` MUST use the locked version from `TAG_HOME/profiles/` and print a warning. It MUST NOT fail with an unhandled exception. |
| NFR-02 | **Fetch timeout** — All outbound HTTP requests MUST respect `hub.fetch_timeout_seconds` (default 10 s). |
| NFR-03 | **Retry logic** — Transient network failures (5xx, connection reset) MUST be retried up to 3 times with exponential backoff. |
| NFR-04 | **Rate limit awareness** — If the GitHub API returns `403 rate limit exceeded`, the CLI MUST print a human-readable message suggesting `GITHUB_TOKEN` authentication and MUST NOT crash. |
| NFR-05 | **Lock file atomicity** — Writes to `profiles.lock` MUST be atomic (write to temp file, then rename) to prevent corruption on interruption. |
| NFR-06 | **No new mandatory runtime dependencies** — `profile_hub.py` MUST use only `urllib.request`, `urllib.parse`, `hashlib`, `json`, `yaml` (already a TAG dependency), and the standard library. `requests` MAY be used if already present in the venv. |
| NFR-07 | **Profile size limit** — Fetched profiles exceeding 512 KB MUST be rejected with an error. This prevents zip-bomb-style YAML payloads. |

---

## 8. Technical Design

### 8.1 New files

| File | Purpose |
|------|---------|
| `src/tag/profile_hub.py` | GitHub fetcher, SHA validator, lock file manager, push logic |

### 8.2 `profiles.lock` format

```yaml
# profiles.lock — managed by `tag profile lock` — safe to commit to version control
# DO NOT edit manually unless you know what you are doing.
version: "1"
profiles:
  researcher:
    source_url: "https://raw.githubusercontent.com/codebyNJ/hermes-octo/abc1234def5678/profiles/researcher.yaml"
    sha: "abc1234def5678abc1234def5678abc1234def5678"
    content_sha256: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
    installed_at: "2026-06-12T10:00:00Z"
    trusted_by: "sanskar@noclick.com"
    tag_version: "0.3.0"
    pinned: false
  devops-pro:
    source_url: "https://gist.githubusercontent.com/octocat/abc123/raw/devops-pro.yaml"
    sha: "def5678abc1234def5678abc1234def5678abc123"
    content_sha256: "a3b4c5d6e7f8a3b4c5d6e7f8a3b4c5d6e7f8a3b4c5d6e7f8a3b4c5d6e7f8a3b4"
    installed_at: "2026-06-12T11:00:00Z"
    trusted_by: "sanskar@noclick.com"
    tag_version: "0.3.0"
    pinned: true
```

### 8.3 Fetch flow

```
tag profile pull <owner>/<repo>/<path> [--pin <sha>] [--trust]
        │
        ▼
1. Resolve GitHub raw URL
   • branch = --pin value OR "HEAD" → resolve to full SHA via Commits API
   • url = https://raw.githubusercontent.com/<owner>/<repo>/<sha>/<path>
        │
        ▼
2. Fetch raw YAML bytes (urllib.request, timeout=hub.fetch_timeout_seconds)
   • Retry up to 3× on transient errors
   • Reject if Content-Length > 512 KB
   • Reject if not HTTPS
        │
        ▼
3. YAML format validation
   • yaml.safe_load() — catch yaml.YAMLError
   • Reject if top-level type is not dict
        │
        ▼
4. PRD-034 security scan
   • Run scanner on raw bytes
   • If HIGH severity → abort (unless --trust)
   • Print scan report summary
        │
        ▼
5. Show profile summary (name, description, system prompt first 200 chars)
   • Prompt: "Review the full diff? [y/N]"
   • If y: print unified diff vs. installed version (or empty if new)
        │
        ▼
6. Require --trust
   • Without --trust: save to TAG_HOME/profile-staging/ and exit
   • With --trust (interactive): show confirmation prompt
   • With --trust (non-interactive): proceed
        │
        ▼
7. Write to TAG_HOME/profiles/<name>.yaml
   • Compute content SHA-256 of written bytes
        │
        ▼
8. Update profiles.lock (atomic write)
   • Record source_url, sha, content_sha256, installed_at, trusted_by, tag_version
```

### 8.4 Push flow

```
tag profile push <name> [--to gist|github] [--public|--private]
        │
        ▼
1. Read TAG_HOME/profiles/<name>.yaml
        │
        ▼
2. PRD-034 secret scan on local file
   • Strip/mask detected secret values
   • Show user a summary of stripped fields
   • Prompt confirmation before proceeding
        │
        ▼
3. Select target
   ├── --to gist (default)
   │   • POST /gists via GitHub API
   │   • Requires GITHUB_TOKEN in env
   │   • Returns Gist URL
   │
   └── --to github
       • Requires --repo <owner>/<repo> [--path profiles/<name>.yaml]
       • PUT /repos/{owner}/{repo}/contents/{path} via GitHub API
       • Returns commit SHA and URL
        │
        ▼
4. Print result URL
5. Offer to record push in profiles.lock under a "pushed_to" field
```

### 8.5 Core module: `src/tag/profile_hub.py`

Key public functions:

```python
def resolve_github_sha(owner: str, repo: str, ref: str, token: str | None) -> str:
    """Resolve a branch name or short SHA to a full 40-char commit SHA."""

def fetch_raw_profile(url: str, timeout: int) -> bytes:
    """Fetch raw profile bytes from a GitHub raw URL. Enforces HTTPS and size limit."""

def validate_yaml(raw: bytes) -> dict:
    """Parse and structurally validate profile YAML. Raises ValueError on failure."""

def compute_content_sha256(raw: bytes) -> str:
    """Return hex SHA-256 of raw bytes for lock file integrity tracking."""

def read_lock_file(tag_home: Path) -> dict:
    """Read profiles.lock, returning empty structure if absent."""

def write_lock_file(tag_home: Path, lock: dict) -> None:
    """Atomically write profiles.lock (write to .tmp, then rename)."""

def update_lock_entry(lock: dict, name: str, entry: dict) -> dict:
    """Upsert a profile entry into the lock dict and return updated dict."""

def stage_profile(tag_home: Path, name: str, sha: str, raw: bytes) -> Path:
    """Write raw bytes to TAG_HOME/profile-staging/<name>-<sha[:8]>.yaml."""

def install_profile(tag_home: Path, name: str, raw: bytes) -> Path:
    """Write raw bytes to TAG_HOME/profiles/<name>.yaml."""

def list_remote_profiles(owner: str, repo: str, profiles_dir: str, token: str | None) -> list[dict]:
    """List YAML files under profiles_dir in the repo tree via GitHub Trees API."""

def push_to_gist(name: str, content: str, public: bool, token: str) -> str:
    """Create a GitHub Gist and return the Gist URL."""

def push_to_repo(owner: str, repo: str, path: str, content: str, token: str) -> str:
    """Commit profile YAML to a GitHub repo and return the commit SHA."""

def diff_profiles(installed: str, remote: str) -> str:
    """Return unified diff string between installed and remote YAML content."""
```

### 8.6 `cmd_profile_hub` integration

`controller.py` gets a new `cmd_profile_hub` function that dispatches to the subcommands
above. The existing `cmd_profile` function handles local profile management; `cmd_profile_hub`
handles all marketplace operations (pull, push, list --remote, diff, lock, verify, uninstall).

The argparse subparser is registered as a subcommand of the existing `profile` parser:

```
tag profile pull ...
tag profile push ...
tag profile list [--remote <owner>/<repo>]
tag profile diff ...
tag profile lock
tag profile verify ...
tag profile uninstall ...
```

---

## 9. Security Considerations

| ID | Concern | Mitigation |
|----|---------|------------|
| SEC-01 | **Prompt injection in system prompts** — A malicious system prompt in a community profile could instruct Hermes to exfiltrate data or override safety rules. | PRD-034 security scan runs on every fetched profile. Patterns target known injection signatures. `--trust` required. |
| SEC-02 | **Supply-chain mutation** — A profile author could silently update a profile after a user has reviewed it. | SHA pinning in `profiles.lock`. `tag profile verify` detects content drift via `content_sha256`. |
| SEC-03 | **Secrets embedded in profiles** — A malicious publisher could embed `OPENROUTER_API_KEY = sk-...` directly in a profile's YAML. | PRD-034 scanner runs pre-activation and pre-push. High-confidence secret patterns block install. |
| SEC-04 | **SSRF via custom hub URLs** — If hub URLs are configurable, an attacker controlling `cli-config.yaml` could redirect fetches to internal network addresses. | URL validation MUST enforce `https://raw.githubusercontent.com/` or `https://gist.githubusercontent.com/` prefixes. Custom hub URLs from config MUST be validated against an allowlist. |
| SEC-05 | **YAML injection** — Malformed YAML using tags like `!!python/object` could execute code during parsing. | All profile YAML MUST be parsed with `yaml.safe_load()`, never `yaml.load()`. This is enforced in `validate_yaml()`. |
| SEC-06 | **GitHub API rate limits** — Unauthenticated requests are limited to 60/hour. A team CI system could exhaust limits. | `GITHUB_TOKEN` support for authenticated requests (5,000/hour). Rate limit error messages direct users to set the token. |
| SEC-07 | **Man-in-the-middle on fetch** — Network interception could substitute profile content. | HTTPS-only fetches enforced. SHA pinning ensures content integrity post-fetch. |
| SEC-08 | **Oversized payload** — A 100 MB YAML file could cause memory exhaustion. | 512 KB hard limit enforced on `Content-Length` header and actual bytes read. |
| SEC-09 | **Lock file tampering** — An attacker with filesystem access could modify `profiles.lock` to bypass SHA checks. | `profiles.lock` is verified on read by comparing recorded `content_sha256` against the installed file on disk. Mismatch triggers `tag profile verify` warning. |
| SEC-10 | **Push of profiles containing real credentials** — A user running `tag profile push` might accidentally publish secrets. | PRD-034 scanner runs on the local file before push. Detected secrets are stripped and the user is shown a confirmation summary. Push aborts if scanner returns `BLOCK`. |
| SEC-11 | **`--trust` flag abuse in scripts** — Automated scripts could pass `--trust` without human review. | `--trust` in non-interactive mode logs the decision with timestamp and user identity to `TAG_HOME/audit.log`. |
| SEC-12 | **Profile impersonation** — A profile could claim to be `researcher` while containing malicious content. | The profile `name` field in the YAML is informational only; the install name is derived from the CLI argument, not the YAML content. |

---

## 10. Testing Strategy

### 10.1 Unit tests (`tests/test_profile_hub.py`)

| Test | What it verifies |
|------|-----------------|
| `test_resolve_github_sha_branch` | Mock GitHub Commits API returns correct full SHA for `main` branch |
| `test_resolve_github_sha_pin_validates` | Providing `--pin abc123` with mismatching API response aborts install |
| `test_fetch_raw_profile_https_only` | HTTP URL raises `ValueError` before any network call |
| `test_fetch_raw_profile_size_limit` | Response body > 512 KB raises `ValueError` |
| `test_validate_yaml_safe_load_only` | Profile YAML with `!!python/object` tag raises `ValueError`, not executes code |
| `test_validate_yaml_malformed` | Malformed YAML raises `ValueError` with message |
| `test_compute_content_sha256` | Known input produces expected SHA-256 hex string |
| `test_read_lock_file_missing` | Returns empty structure when `profiles.lock` absent |
| `test_write_lock_file_atomic` | Verifies `.tmp` intermediate file is used and renamed |
| `test_stage_profile_path` | Staged file lands in `profile-staging/` with correct name |
| `test_install_profile_path` | Installed file lands in `profiles/` with correct name |
| `test_list_remote_profiles_filters_yaml` | GitHub Trees API mock returns mixed files; only YAML under `profiles/` returned |
| `test_diff_profiles_empty` | Identical content returns empty string, exit 0 |
| `test_diff_profiles_changed` | Modified content returns non-empty unified diff |

### 10.2 Security scan integration tests (`tests/test_profile_hub_security.py`)

| Test | What it verifies |
|------|-----------------|
| `test_malicious_profile_blocked_by_scanner` | Profile YAML containing `sk-proj-...` triggers PRD-034 scan FAIL and blocks install |
| `test_prompt_injection_pattern_detected` | System prompt containing `ignore all previous instructions` triggers WARN |
| `test_push_strips_secrets` | `push_to_gist` call strips detected `API_KEY` values before payload is sent |
| `test_trust_flag_required_for_high_severity` | HIGH severity scan result without `--trust` raises `SecurityBlockError` |
| `test_trust_flag_logs_to_audit` | `--trust` in non-interactive mode writes entry to `TAG_HOME/audit.log` |

### 10.3 Lock file tests (`tests/test_profiles_lock.py`)

| Test | What it verifies |
|------|-----------------|
| `test_lock_update_preserves_other_entries` | Updating one entry does not clobber others |
| `test_lock_detects_content_drift` | Modifying installed file changes SHA-256; `verify` command detects mismatch |
| `test_lock_atomic_write_interrupted` | Simulated interruption mid-write leaves old lock intact |
| `test_lock_command_updates_sha_not_content` | `tag profile lock` updates SHA field but does not modify installed YAML |

### 10.4 CLI integration tests (`tests/test_profile_hub_cli.py`)

| Test | What it verifies |
|------|-----------------|
| `test_pull_e2e_mock_github` | Full pull flow with mocked GitHub API and scanner writes profile and lock |
| `test_pull_without_trust_stages_only` | Pull without `--trust` writes to staging, not profiles |
| `test_pull_pin_sha_enforced` | Mismatch between `--pin` value and resolved SHA aborts install |
| `test_uninstall_refuses_active_profile` | Attempting to uninstall active profile returns error code 1 |
| `test_list_remote_table_output` | Mocked tree API produces expected tabular output |
| `test_diff_exit_code_on_changes` | `tag profile diff` exits 1 when remote differs from installed |
| `test_verify_fail_on_secret` | `tag profile verify` exits non-zero when installed file now contains a secret |

---

## 11. Acceptance Criteria

| ID | Criterion | How to verify |
|----|-----------|--------------|
| AC-01 | `tag profile pull codebyNJ/hermes-octo/profiles/researcher.yaml --trust` fetches the profile, runs the security scan, and writes it to `TAG_HOME/profiles/researcher.yaml` with a `profiles.lock` entry. | Manual + `test_pull_e2e_mock_github` |
| AC-02 | `tag profile pull` without `--trust` writes to `TAG_HOME/profile-staging/` and exits 0 without modifying `profiles/`. | `test_pull_without_trust_stages_only` |
| AC-03 | A profile YAML containing a high-confidence secret pattern causes `tag profile pull` to print a security error and exit non-zero, even with `--trust`. | `test_malicious_profile_blocked_by_scanner` |
| AC-04 | `profiles.lock` contains `source_url`, `sha` (40 chars), `content_sha256`, `installed_at`, `trusted_by`, and `tag_version` after a successful pull. | Inspect lock file after AC-01 |
| AC-05 | `tag profile pull --pin abc1234def5678abc1234def5678abc1234def56 --trust` uses the exact SHA in the fetch URL and records it in `profiles.lock`. | `test_pull_pin_sha_enforced` |
| AC-06 | `tag profile diff researcher --remote` prints a unified diff and exits 1 when upstream has changed, exits 0 when identical. | `test_diff_exit_code_on_changes` |
| AC-07 | `tag profile push researcher --to gist --public` creates a Gist, strips any secret values, prints the Gist URL, and exits 0. | Manual with `GITHUB_TOKEN` set |
| AC-08 | `tag profile list --remote codebyNJ/hermes-octo` prints a table of YAML profiles from the repo. | `test_list_remote_table_output` |
| AC-09 | `tag profile lock` updates `sha` fields in `profiles.lock` without modifying any installed profile YAML files. | `test_lock_command_updates_sha_not_content` |
| AC-10 | `tag profile verify researcher` exits non-zero if the installed profile's content SHA-256 does not match `profiles.lock`. | `test_lock_detects_content_drift` |
| AC-11 | `tag profile uninstall researcher` removes `TAG_HOME/profiles/researcher.yaml` and the lock entry, and refuses with exit 1 if `researcher` is the active profile. | `test_uninstall_refuses_active_profile` |
| AC-12 | All HTTP fetches use HTTPS. Passing an HTTP URL raises a clear error before any network request. | `test_fetch_raw_profile_https_only` |
| AC-13 | A profile YAML > 512 KB is rejected with a human-readable error. | `test_fetch_raw_profile_size_limit` |
| AC-14 | `GITHUB_TOKEN` env var is used for authenticated GitHub API calls when present; unauthenticated calls work when absent (subject to rate limits). | Manual + `test_list_remote_profiles_filters_yaml` |

---

## 12. Dependencies

| Dependency | Type | Notes |
|------------|------|-------|
| **PRD-034 — Secret Scanning** | Hard blocker — must ship first | The security scan integration (FR-03, FR-04, FR-08, SEC-01, SEC-03, SEC-10) calls into the scanner module defined by PRD-034. This PRD MUST NOT ship without PRD-034 in production. |
| `urllib.request` | stdlib | Used for HTTPS fetches. No new install required. |
| `urllib.parse` | stdlib | URL construction and validation. |
| `hashlib` | stdlib | SHA-256 content hashing for lock file. |
| `yaml` (PyYAML) | Already in TAG deps | Profile parsing — `yaml.safe_load()` only. |
| `difflib` | stdlib | Unified diff generation for `tag profile diff`. |
| `GITHUB_TOKEN` env var | Runtime (optional) | Enables authenticated GitHub API calls for higher rate limits and private repo access. |

---

## 13. Open Questions

| ID | Question | Impact | Owner |
|----|----------|--------|-------|
| OQ-01 | **Profile format compatibility with hermes-octo** — Do profiles in `codebyNJ/hermes-octo` use the same YAML schema as TAG's `default.yaml` profile entries, or do they use the `tag-template.yaml` schema from PRD-015? If different, a conversion step is needed. | Medium — affects `validate_yaml()` and import logic | Clarify with codebyNJ maintainer |
| OQ-02 | **Hub URL configuration** — Should users be able to configure alternative hub URLs (e.g., GitHub Enterprise, self-hosted Gitea) via `hub.base_url` in `cli-config.yaml`? The SSRF risk (SEC-04) requires an allowlist strategy if this is supported. | High — affects security design | Decide before implementation |
| OQ-03 | **Profile versioning scheme** — The current design uses Git commit SHAs as the version identifier. Should TAG also support semver tags (`v1.2.0`) as pin targets? This would require resolving tag refs via the GitHub Tags API. | Low-medium — additive feature | Can defer to post-launch |
| OQ-04 | **profiles.lock placement** — Should `profiles.lock` live in `TAG_HOME` (per-installation) or alongside `default.yaml` in the project directory (per-project)? The per-project option enables reproducible team installs via version control but requires a project-scoped TAG config. | Medium — affects UX and multi-project workflows | Decide before implementation |
| OQ-05 | **Gist update vs. create** — If the user runs `tag profile push researcher --to gist` twice, should it create a new Gist each time or update the existing one? Updating requires storing the Gist ID in `profiles.lock`. | Low — UX refinement | Can default to "create new" and add update in v2 |

---

## 14. Complexity & Timeline

**Complexity:** M (Medium)
**Timeline:** 1 sprint (~2 weeks), contingent on PRD-034 being complete

| Week | Tasks |
|------|-------|
| Week 1 | Implement `profile_hub.py` (fetch, validate, lock read/write, SHA resolution); implement `cmd_profile_hub` pull and list subcommands; unit tests for all hub functions |
| Week 2 | Implement push flow (Gist + repo); implement diff, lock, verify, uninstall subcommands; security scan integration tests; CLI integration tests; argparse registration in `controller.py` |

**Milestone gate:** PRD-034 secret scanning module must be merged and stable before Week 1
begins. Starting implementation without PRD-034 means shipping `tag profile pull` with no
security scan, which is the HIGH SECURITY RISK case described in the warning at the top of
this document.

---

## 15. Risk Register

| Risk | Likelihood | Impact | Mitigation |
|------|-----------|--------|------------|
| PRD-034 is delayed | Medium | High — blocks this PRD entirely | Do not ship any part of this PRD without PRD-034. Use the delay to finalize the lock file schema and CLI surface. |
| hermes-octo profile schema is incompatible | Medium | Medium — requires conversion layer | Clarify schema with maintainer (OQ-01) before implementation sprint. |
| GitHub rate limits block CI test suite | Low | Medium — tests fail intermittently | Mock all GitHub API calls in tests. Real API calls only in optional integration test suite gated by `GITHUB_TOKEN` env var. |
| Users bypass `--trust` in CI scripts | Medium | High — prompt injection reaches active profiles | Audit log for `--trust` in non-interactive mode (SEC-11). Consider adding `--trust-reason` argument for audit trail. |
| YAML safe_load bypass via future PyYAML CVE | Low | Critical | Pin PyYAML version in `pyproject.toml`; add a `tag doctor` check for PyYAML version. |
