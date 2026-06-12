# PRD-034: Secret Scanning (`tag security scan`)

**Status:** Proposed
**PRD Number:** 034
**Category:** Security
**Priority:** P0 Critical
**Estimated Effort:** S (3–5 days)
**Affects:** new `src/tag/security.py`, `controller.py` (new `security/scan` subcommands), profile import/export flow
**Unblocks:** PRD-026 (Profile Marketplace)
**Dependencies:** None (standalone gate module)
**Security Classification:** CRITICAL — module MUST never log plaintext matched values

---

## 1. Overview

TAG profiles contain `.env` files, context blocks, system prompts, and arbitrary YAML that users compose by hand or import from third-party sources. Any of these surfaces can contain live credentials — Anthropic API keys, OpenAI keys, AWS access keys, GitHub personal access tokens, npm automation tokens, and similar secrets. When a profile is shared (manually today; via marketplace after PRD-026), a single overlooked `.env` file silently publishes a live credential to whoever receives the profile.

This PRD specifies a secret-scanning gate that combines **Shannon entropy detection** over sliding windows with a **named-pattern library** of ~15 known secret formats. The scanner runs as a pre-flight check before every `tag profile push`, before every profile import from a remote source, and as an explicit `tag security scan` command. Detected findings are reported by file path and line number; matched values are **never** emitted to stdout, stderr, or any log sink.

---

## 2. Problem Statement

### 2.1 The Specific Failure Mode

A user creates a profile for their `coder` agent. They add an `.env` file inside `~/.tag/profiles/coder/` containing:

```
ANTHROPIC_API_KEY=sk-ant-api03-XXXXXXXXXXXXXXXXXXXXXXXX
OPENAI_API_KEY=sk-XXXXXXXXXXXXXXXXXXXXXXXX
```

They run `tag profile push coder` (PRD-026) to share the profile on a public GitHub Gist. The `.env` file ships with the profile. The key is live. The cost and access damage is immediate.

The same failure can occur with:
- Context blocks inside profile YAML that paste API keys for documentation
- Model routing config that embeds AWS credentials for Bedrock access
- Agent-generated `config.json` files placed in the profile directory during a run

### 2.2 Why Existing Defenses Are Insufficient

| Existing defense | Gap |
|-----------------|-----|
| `.gitignore` patterns | TAG profiles don't use git; push is a tarball + Gist upload |
| User discipline | Insufficient at scale; one mistake is catastrophic |
| PRD-026's `--trust` flag | Protects consumers, not producers of profiles |
| No current scan step | There is literally no check today |

### 2.3 Impact Scope

- **PRD-026 (Profile Marketplace)** is explicitly marked BLOCKED on this PRD because shipping profile distribution without a producer-side scan creates a supply-chain attack vector against every TAG installation. PRD-026 cannot ship first.
- Autonomous mode (PRD-021) can write files into profile directories that the user never reads — making accidental secret injection possible without any user action.

---

## 3. Goals

1. **Detect known credential formats** using a pattern library of ~15 named regexes covering major API key namespaces.
2. **Detect unknown high-entropy secrets** using Shannon entropy over sliding 20-character windows, flagging windows with entropy > 4.5 bits per character.
3. **Block profile export/push** when findings exist, with a clear human-readable report.
4. **Gate profile import** (from URL, Gist, or marketplace) with the same scan before activation.
5. **Provide `tag security scan`** as a standalone command users can run anytime.
6. **Provide `tag security audit`** to scan all profiles in TAG_HOME at once.
7. **Never log matched values** — findings report file path, line number, pattern name, and entropy score only.
8. **Provide `--fix` mode** that redacts confirmed findings in-place (replaces matched value with `[REDACTED]`) after explicit user confirmation.

---

## 4. Non-Goals

1. Full static analysis / SAST scanning beyond credential patterns.
2. Scanning agent-generated code artifacts outside the profile directory.
3. Integrating with secret management vaults (HashiCorp Vault, AWS Secrets Manager) — that belongs to a future PRD.
4. Git history scanning (outside profile directories).
5. Automatic key rotation or revocation — the scanner detects; it does not act on secrets beyond redaction.
6. Scanning binary files — only UTF-8 decodable text files are in scope.

---

## 5. Success Metrics

| Metric | Target |
|--------|--------|
| True-positive rate on known formats | 100% on test vector suite |
| False-positive rate (base64 non-secrets > 20 chars) | < 5% in representative profile corpus |
| `tag security scan` cold start (single profile) | < 200 ms |
| `tag security audit` (50 profiles) | < 5 s |
| Zero occurrences of matched value in any log | Pass on audit |
| PRD-026 marketplace push blocked when secrets present | 100% gate fidelity |

---

## 6. User Stories

### US-01: Developer Exports a Profile with an .env File
**As a** developer about to run `tag profile push my-coder`,
**I want** TAG to automatically scan my profile for secrets before the push,
**so that** I never accidentally publish a live API key to a public Gist.

**Acceptance Criteria:**
- `tag profile push` runs the scanner as a pre-flight step with no extra flags required.
- If `ANTHROPIC_API_KEY=sk-ant-api03-...` is present in any file under the profile directory, the push is aborted.
- The error message shows: file path, line number, pattern name (`anthropic-api-key`), and a reminder to use `[REDACTED]` or environment variable substitution.
- The actual key value does not appear in the error message.

### US-02: Developer Runs a Standalone Scan
**As a** developer who wants to audit a profile before sharing it manually,
**I want** to run `tag security scan --profile coder`,
**so that** I get a report of any secrets found without triggering a push.

**Acceptance Criteria:**
- `tag security scan --profile coder` exits 0 if clean, exits 1 if findings present.
- Output follows the format: `FINDING [pattern-name] file.ext:42 entropy=5.12`.
- Running with `--json` outputs a machine-readable JSON array of findings.

### US-03: Developer Fixes Detected Secrets
**As a** developer who ran `tag security scan` and got findings,
**I want** to run `tag security scan --fix` to have the matched values automatically redacted,
**so that** I don't have to hunt through files manually.

**Acceptance Criteria:**
- `--fix` shows a preview of each replacement before writing.
- Each confirmed replacement writes `[REDACTED]` in place of the matched value.
- Original files are backed up to `<file>.scan-backup` before modification.
- After `--fix`, re-running the scan returns clean (no findings for redacted lines).

### US-04: Organization Lead Audits All Profiles
**As a** team lead managing a shared TAG install,
**I want** to run `tag security audit`,
**so that** I get a single report covering all profiles in TAG_HOME.

**Acceptance Criteria:**
- `tag security audit` scans every profile directory under `~/.tag/profiles/`.
- Summary table shows: profile name, finding count, highest-risk finding.
- Exits 1 if any profile has findings; exits 0 if all profiles are clean.

### US-05: Profile Import is Gated
**As a** developer running `tag profile pull github:user/repo/coder.yaml`,
**I want** the imported profile to be scanned before it is written to disk,
**so that** a malicious or misconfigured community profile can't inject credentials into my environment.

**Acceptance Criteria:**
- The scan runs on the raw downloaded bytes before the profile is written to `~/.tag/profiles/`.
- If findings are detected, the import fails with a report and no files are written.
- `--force-import` bypasses the scan with an explicit warning printed to stderr.

---

## 7. Technical Design

### 7.1 New Module: `src/tag/security.py`

The entire scanning logic lives in a single, importable, side-effect-free module. It has no dependencies beyond the Python standard library (re, math, pathlib, dataclasses) so it can be imported before any optional dependency is available.

#### 7.1.1 Data Structures

```python
from dataclasses import dataclass, field
from pathlib import Path

@dataclass
class SecretFinding:
    file_path: Path
    line_number: int          # 1-indexed
    pattern_name: str         # e.g. "anthropic-api-key"
    entropy: float            # Shannon entropy of the matched window
    # NOTE: matched_value is intentionally ABSENT — never stored
    context_before: str = ""  # up to 40 chars before match, secret stripped
    context_after: str = ""   # up to 40 chars after match, secret stripped

@dataclass
class ScanResult:
    scanned_files: int
    findings: list[SecretFinding] = field(default_factory=list)
    errors: list[str] = field(default_factory=list)  # file read errors

    @property
    def clean(self) -> bool:
        return len(self.findings) == 0
```

#### 7.1.2 Named Pattern Library

Each pattern is a compiled regex. The value capture group (group 1) is used exclusively to compute entropy and determine redaction range — it is never included in logs, displayed output, or exception messages.

```python
import re

# Format: (pattern_name, compiled_regex_with_value_in_group_1)
SECRET_PATTERNS: list[tuple[str, re.Pattern]] = [
    # Anthropic
    ("anthropic-api-key",
     re.compile(r'(sk-ant-(?:api03-|admin01-)[A-Za-z0-9_\-]{93,})', re.ASCII)),

    # OpenAI / OpenAI-compatible
    ("openai-api-key",
     re.compile(r'(sk-[A-Za-z0-9]{48,})', re.ASCII)),
    ("openai-project-key",
     re.compile(r'(sk-proj-[A-Za-z0-9_\-]{48,})', re.ASCII)),

    # GitHub
    ("github-pat-classic",
     re.compile(r'(ghp_[A-Za-z0-9]{36,})', re.ASCII)),
    ("github-pat-fine-grained",
     re.compile(r'(github_pat_[A-Za-z0-9_]{82,})', re.ASCII)),
    ("github-oauth-token",
     re.compile(r'(gho_[A-Za-z0-9]{36,})', re.ASCII)),
    ("github-actions-token",
     re.compile(r'(ghs_[A-Za-z0-9]{36,})', re.ASCII)),

    # AWS
    ("aws-access-key-id",
     re.compile(r'(AKIA[0-9A-Z]{16})', re.ASCII)),
    ("aws-secret-access-key",
     re.compile(r'(?i)aws[_\-\s]?secret[_\-\s]?(?:access[_\-\s]?)?key[\s]*[=:]\s*([A-Za-z0-9/+]{40})', re.ASCII)),

    # npm
    ("npm-token",
     re.compile(r'(npm_[A-Za-z0-9]{36,})', re.ASCII)),

    # OpenRouter
    ("openrouter-api-key",
     re.compile(r'(sk-or-v1-[A-Za-z0-9]{64,})', re.ASCII)),

    # Generic bearer tokens in .env assignment context
    ("generic-bearer-token",
     re.compile(r'(?i)(?:api[_\-]?key|token|secret|password|passwd|auth)[_\-\s]*[=:]\s*["\']?([A-Za-z0-9_\-\.]{32,})["\']?', re.ASCII)),

    # Private RSA/EC key headers
    ("private-key-header",
     re.compile(r'(-----BEGIN (?:RSA |EC |OPENSSH )?PRIVATE KEY-----)', re.ASCII)),

    # Stripe
    ("stripe-secret-key",
     re.compile(r'(sk_live_[A-Za-z0-9]{24,})', re.ASCII)),
    ("stripe-restricted-key",
     re.compile(r'(rk_live_[A-Za-z0-9]{24,})', re.ASCII)),
]
```

**Pattern authorship notes:**
- `anthropic-api-key`: matches `sk-ant-api03-` (standard) and `sk-ant-admin01-` (admin key) prefixes; minimum suffix length 93 chars as per Anthropic's current key format.
- `openai-api-key`: the `sk-` prefix alone is kept short because project keys (`sk-proj-`) share the prefix; the generic pattern intentionally overlaps with the project pattern so both fire.
- `aws-access-key-id`: `AKIA` is the canonical access key prefix; all 20 chars including prefix.
- `generic-bearer-token`: acts as a catch-all for `.env` assignment patterns; minimum 32-char value to reduce false positives from short config strings.
- `private-key-header`: any embedded private key block header is an unconditional finding regardless of entropy.

#### 7.1.3 Shannon Entropy Algorithm

```python
import math

def _shannon_entropy(s: str) -> float:
    """Compute Shannon entropy in bits per character for string s."""
    if not s:
        return 0.0
    freq: dict[str, int] = {}
    for ch in s:
        freq[ch] = freq.get(ch, 0) + 1
    n = len(s)
    return -sum((c / n) * math.log2(c / n) for c in freq.values())


ENTROPY_WINDOW_SIZE = 20       # sliding window length in characters
ENTROPY_THRESHOLD = 4.5        # bits per character; flag windows >= this value
ENTROPY_MIN_ALPHA_RATIO = 0.5  # window must be >= 50% alphanumeric to avoid false positives on binary-looking data
```

The entropy scan operates as a fallback pass over lines that did not match any named pattern. For each line:
1. Strip the key name from any `KEY=value` style assignment (everything before and including `=` or `:`).
2. Slide a window of `ENTROPY_WINDOW_SIZE` characters with step 1.
3. For each window, compute `_shannon_entropy(window)`.
4. If entropy >= `ENTROPY_THRESHOLD` AND alphanumeric ratio >= `ENTROPY_MIN_ALPHA_RATIO`, record a finding with `pattern_name = "high-entropy"`.
5. Advance the window past the end of the highest-scoring substring to avoid duplicate findings on the same value.

The 4.5-bit threshold is drawn from empirical analysis in the Detect-Secrets project: random 20-char alphanumeric strings average ~4.7 bits; human-readable strings average ~3.5 bits; UUID v4 averages ~3.6 bits (hyphen-separated segments reduce entropy).

#### 7.1.4 Files Scanned

```python
SCANNABLE_EXTENSIONS = {
    ".env", ".yaml", ".yml", ".json", ".toml", ".ini", ".cfg",
    ".conf", ".properties", ".txt", ".md", ".sh", ".bash", ".zsh",
    ""  # extensionless files (e.g. Makefile, Dockerfile)
}

MAX_FILE_SIZE_BYTES = 1 * 1024 * 1024  # 1 MiB — skip larger files, log a warning
```

Binary files (detected by `b'\x00'` presence in the first 8192 bytes) are skipped silently.

#### 7.1.5 Scan Flow

```
scan_profile(profile_dir: Path) -> ScanResult:
  for each file in profile_dir (recursive, up to MAX_FILE_SIZE_BYTES):
    if file is binary: skip
    for each line in file:
      for each (pattern_name, regex) in SECRET_PATTERNS:
        m = regex.search(line)
        if m:
          entropy = _shannon_entropy(m.group(1))
          append SecretFinding(file, lineno, pattern_name, entropy)
          # do NOT include m.group(1) anywhere
          break  # one finding per line per position is enough
      else:
        # no named pattern matched — run entropy scan on value portion
        value_portion = extract_value_portion(line)
        for window in sliding_windows(value_portion, ENTROPY_WINDOW_SIZE):
          if _shannon_entropy(window) >= ENTROPY_THRESHOLD and alpha_ratio(window) >= ENTROPY_MIN_ALPHA_RATIO:
            append SecretFinding(file, lineno, "high-entropy", entropy)
            break
```

#### 7.1.6 Output Format

Human-readable (default):

```
SECURITY SCAN — coder
─────────────────────────────────────────────────────────
FINDING  anthropic-api-key   .env:3          entropy=5.81
FINDING  high-entropy        context.yaml:17 entropy=4.72
─────────────────────────────────────────────────────────
2 findings in 8 files scanned.
Resolve findings before pushing. Run `tag security scan --fix` to redact in place.
```

JSON (with `--json`):

```json
{
  "profile": "coder",
  "scanned_files": 8,
  "findings": [
    {
      "file": ".env",
      "line": 3,
      "pattern": "anthropic-api-key",
      "entropy": 5.81
    },
    {
      "file": "context.yaml",
      "line": 17,
      "pattern": "high-entropy",
      "entropy": 4.72
    }
  ]
}
```

Exit codes:
- `0` — clean (no findings)
- `1` — findings present
- `2` — scan error (file read failure, permission denied)

### 7.2 CLI Surface

All security commands are grouped under the `security` subcommand in `controller.py`.

#### `tag security scan`

```
tag security scan [--profile NAME] [--path PATH] [--json] [--fix] [--quiet]

Options:
  --profile NAME   Scan a named profile in TAG_HOME/profiles/
  --path PATH      Scan an arbitrary directory or file (for import pre-checks)
  --json           Emit findings as JSON to stdout
  --fix            After showing findings, prompt to redact each in place
  --quiet          Suppress all output; use exit code only (for CI)
```

At least one of `--profile` or `--path` must be provided. If neither is given, scan the current working directory with a warning.

#### `tag security audit`

```
tag security audit [--json] [--quiet]

Scans all profiles in TAG_HOME/profiles/. Reports a summary table.
```

### 7.3 Pre-flight Integration Points

#### 7.3.1 `tag profile push` (PRD-026)

In `cmd_profile_hub` (PRD-026), before constructing the Gist/GitHub payload:

```python
from tag.security import scan_profile, ScanResult

result: ScanResult = scan_profile(profile_dir)
if not result.clean:
    _print_scan_findings(result)
    raise SystemExit(
        "Aborting push: secrets detected. "
        "Run `tag security scan --fix` to redact, then retry."
    )
```

#### 7.3.2 Profile Import from Remote

In the profile download path (PRD-026 `cmd_profile_pull`), after writing to a temp directory but before moving to `~/.tag/profiles/`:

```python
result = scan_profile(tmp_dir)
if not result.clean and not args.force_import:
    _print_scan_findings(result)
    shutil.rmtree(tmp_dir)
    raise SystemExit(
        "Import blocked: secrets detected in remote profile. "
        "Use --force-import to override (not recommended)."
    )
```

### 7.4 Logging Safety Contract

The following rules apply throughout `security.py` and all call sites:

1. `SecretFinding` does NOT have a `matched_value` field. Any attempt to add one must fail code review.
2. `str(finding)` and `repr(finding)` MUST NOT include the matched value.
3. No `print`, `logging.debug`, `rich.print`, or exception message may include `m.group(1)` or any slice of the matched text.
4. The `.scan-backup` files written by `--fix` preserve the original content — this is acceptable because the user explicitly requested backup before redaction. The backup files MUST be documented as containing secrets.

### 7.5 `--fix` Redaction Flow

```
for finding in result.findings:
    print(f"  {finding.file_path}:{finding.line_number} [{finding.pattern_name}]")
    confirm = input("  Redact this finding? [y/N] ")
    if confirm.lower() == 'y':
        # read file, replace matched span with [REDACTED], write back
        # matched span is re-located by re-running the regex on the line
        # backup written first
```

`[REDACTED]` is chosen (not `***` or empty string) because:
- It is a recognizable sentinel that differentiates intentional removal from an empty/missing value.
- Downstream tools that parse `.env` will still see a key assignment, preventing silent config errors.

---

## 8. Implementation Plan

### Phase 1 — Core Scanner (Day 1–2)

| Task | File | Notes |
|------|------|-------|
| Create `src/tag/security.py` | new | `SecretFinding`, `ScanResult`, `SECRET_PATTERNS`, `_shannon_entropy`, `scan_profile` |
| Write unit tests | `tests/test_security.py` | Test vector for each pattern; entropy edge cases; binary file skip |
| Add `tag security scan` command | `controller.py` | `cmd_security_scan`, route through `argparse` |

### Phase 2 — Audit + Fix (Day 2–3)

| Task | File | Notes |
|------|------|-------|
| `tag security audit` command | `controller.py` | Iterates all profiles, calls `scan_profile` per profile |
| `--fix` redaction mode | `security.py` | Backup + in-place redaction with confirmation prompt |
| JSON output mode | `security.py` / `controller.py` | `--json` flag wired through |

### Phase 3 — Integration Gates (Day 3–4)

| Task | File | Notes |
|------|------|-------|
| Pre-flight hook in `cmd_profile_push` | `controller.py` | Abort if not clean; error message with no values |
| Pre-flight hook in profile import | `controller.py` | Gate on temp-dir scan before move |
| `--force-import` escape hatch | `controller.py` | Logged warning, not silenced |

### Phase 4 — Hardening + Docs (Day 4–5)

| Task | File | Notes |
|------|------|-------|
| Integration tests with real profile fixtures | `tests/` | Profile dirs with intentional findings and clean profiles |
| Performance benchmark (50-profile audit) | `tests/` | Assert under 5 s |
| Add to `tag doctor` checks | `controller.py` | Warn if any profile has never been scanned |
| Update README / help text | `README.md`, `controller.py` | Document `tag security` subcommand group |

---

## 9. Risks and Mitigations

| Risk | Likelihood | Impact | Mitigation |
|------|-----------|--------|------------|
| False positives: base64-encoded non-secrets flagged by entropy scan | Medium | Low (user friction, no data loss) | Raise `ENTROPY_MIN_ALPHA_RATIO` to 0.6; allow per-file `# tag:noscan` suppression comment |
| False negatives: user obfuscates key with `base64.b64encode` | Low | High (key leaks) | Document that `--fix` redacts only detected values; encourage explicit `.env` exclusion in profile manifest |
| Matched value leaks via exception traceback | Medium | High | Wrap regex match code in try/except that re-raises a sanitized exception |
| `--fix` corrupts files with non-UTF-8 encoding | Low | Medium | Detect encoding before writing; fall back to `latin-1` with a warning |
| Scan adds unacceptable latency to `tag profile push` | Low | Low | Benchmark shows < 200 ms for typical profiles; async scan possible if needed |
| Pattern library becomes stale as providers change key formats | Medium | Medium | Add `tag security update-patterns` as future PRD; open-source the pattern list for community updates |

### 9.1 Known False-Positive Sources

The following classes of strings are known to trigger the entropy scanner without being secrets:

- Long UUIDs (v4) with hyphens stripped: entropy ~4.55, just above threshold
- Short JWT payloads (alg header only)
- Long hexadecimal digests (SHA-256/512)

**Mitigation patterns already embedded in the design:**
- `ENTROPY_MIN_ALPHA_RATIO = 0.5` excludes pure hex strings (0–9, a–f only → ratio 1.0 but character diversity low)
- A `# tag:noscan-next-line` suppression comment will be parsed before scanning the following line
- Hex-only strings (matching `^[0-9a-fA-F]+$`) are explicitly excluded from entropy findings in `scan_profile`

---

## 10. Security Considerations

### 10.1 The Scanner Must Not Become a Secret Exfiltration Vector

The scan result object intentionally omits matched values. This is enforced at the type level. Any future contributor who adds a `matched_value: str` field to `SecretFinding` must justify it in a security review. The field should never be added.

### 10.2 Backup Files Created by `--fix`

The `.scan-backup` files contain the original file content including the secret. They are created in the same directory as the original file. Users must be warned:
- The backup files contain the live secret.
- They should be deleted after verifying the redaction is correct.
- They should be added to `.gitignore` / `.tagignore`.

### 10.3 `--force-import` Audit Trail

When a user bypasses the import gate with `--force-import`, this event is logged to `~/.tag/security-audit.log` with timestamp, profile name, and finding count — but not the findings themselves. This creates an audit trail for incidents without logging secrets.

### 10.4 Transport Security

This PRD does not handle transport security for the marketplace (that is PRD-026's concern). The scanner operates on already-downloaded bytes; it cannot protect against MITM attacks on the download itself.

---

## 11. Open Questions

| # | Question | Owner | Target resolution |
|---|----------|-------|-------------------|
| OQ-1 | Should `# tag:noscan` suppression comments be supported in `.env` files? (dotenv spec does not support comments uniformly) | @sanskarpan | Before Phase 4 |
| OQ-2 | Should the entropy threshold be user-configurable via `cli-config.yaml`? Risk: users lower it to 0 to silence alerts | @sanskarpan | Before Phase 1 |
| OQ-3 | Should `tag security audit` write a signed report to disk for compliance purposes? | TBD | Post-v1 |
| OQ-4 | Do we want to scan memory journal files (PRD-002) and loop run journals (PRD-021) for accidentally logged secrets? | @sanskarpan | Separate PRD |
| OQ-5 | Should `--fix` support `--dry-run` to show what would be redacted without writing? | @sanskarpan | Nice-to-have in Phase 2 |

---

## 12. Acceptance Criteria Summary

- [ ] `tag security scan --profile <name>` exits 0 when profile is clean, exits 1 when findings present
- [ ] `tag security scan` output never contains matched secret value
- [ ] Pattern library covers all 15 named formats listed in Section 7.1.2
- [ ] Shannon entropy algorithm matches reference implementation in tests
- [ ] `tag profile push` is blocked (exit 1 with human-readable error) when findings present
- [ ] Profile import is blocked before files are written to `~/.tag/profiles/`
- [ ] `--fix` creates `.scan-backup` before modifying any file
- [ ] `tag security audit` scans all profiles and exits 1 if any finding present
- [ ] `--json` output is valid JSON matching the schema in Section 7.1.6
- [ ] All 15 pattern regexes have at least one positive test vector and one negative test vector in `tests/test_security.py`
- [ ] Performance: `tag security audit` on 50 profiles completes in < 5 s
- [ ] `src/tag/security.py` has no imports outside Python standard library
