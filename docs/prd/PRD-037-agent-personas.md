# PRD-036: Agent Personas (tag persona)

**Status:** Proposed
**Priority:** P1
**Estimated Effort:** M (1 sprint, ~2 weeks)
**Affects:** `controller.py` (new `cmd_persona_*` handlers), new `src/tag/persona.py`,
new `src/tag/config/personas/` (built-in persona files)
**Security Classification:** MEDIUM — personas are system prompt injections; same threat
model as profiles; marketplace personas gated behind PRD-034 (Secret Scanning)
**Depends on:** PRD-035 (Profile Marketplace infrastructure for `persona pull/push`)

---

## 1. Overview

TAG profiles today carry a single monolithic system prompt together with tool permissions,
model selection, memory config, and routing rules. When a user wants to change how the agent
communicates — tersely, verbosely, with domain conventions specific to security or data
science — they must either edit the profile YAML directly or maintain duplicate profiles
that differ only in their system prompt preamble. Neither approach scales.

This PRD defines **Agent Personas**: a personality and style injection layer that sits on top
of profiles. A persona is a lightweight YAML artifact containing a `style_prompt` — a concise
natural-language description of communication style, tone, verbosity level, and optional
domain-specific conventions. At dispatch time, TAG merges the active persona's `style_prompt`
into the profile's system prompt at a declared injection position (`prepend` or `append`)
without modifying the stored profile YAML on disk.

Personas are composable: any persona can be applied to any profile, multiple personas can be
stacked (concatenated in declaration order), and the active persona can be switched for the
duration of a terminal session without touching the underlying profile. Built-in personas
(`terse-engineer`, `verbose-explainer`, `security-focused`, `data-scientist`, `teacher`)
ship with TAG out of the box. Community personas can be pulled from any GitHub repo or Gist
using the same infrastructure established in PRD-035.

Personas are **not** profile templates, **not** per-message style injections, and **not** a
replacement for profiles. They are a separation-of-concerns layer: profiles describe what the
agent can do; personas describe how the agent communicates.

---

## 2. Goals

1. **Separation of concerns between persona and profile** — Communication style, tone, and
   domain conventions are fully decoupled from tool permissions, model selection, memory
   config, and budget limits. A profile change never forces a persona change and vice versa.

2. **Composable application** — Any persona can be applied to any profile with a single CLI
   command. The same `terse-engineer` persona should work identically whether the active
   profile is `coder`, `researcher`, or `sre`.

3. **Runtime session switching** — Users can switch personas for the duration of a terminal
   session via `tag persona apply --session` without writing back to any YAML file on disk.
   The session state is ephemeral and scoped to the originating terminal's `TAG_SESSION_ID`.

4. **Persona marketplace (community-shareable)** — Any GitHub repo or Gist can serve as a
   persona distribution source. `tag persona pull <owner>/<repo>/<persona>` installs a
   community persona using the same SHA-pinned lock file infrastructure from PRD-035.

5. **Style and domain personas** — TAG ships two classes of built-in persona: pure style
   personas (e.g., `terse-engineer`, `verbose-explainer`) that control tone and verbosity
   regardless of subject matter, and domain personas (e.g., `security-focused`,
   `data-scientist`) that inject domain-specific conventions and terminology on top of any
   style.

6. **Persona stacking** — Multiple personas can be declared active simultaneously. Their
   `style_prompt` values are concatenated in declaration order to produce the final injection
   string. Conflict detection warns when two active personas contain contradictory
   instructions (e.g., `terse-engineer` + `verbose-explainer`).

7. **Zero profile mutation** — Persona injection is a runtime operation only. The profile
   YAML stored in `TAG_HOME/profiles/` is never modified by persona application. The merged
   system prompt exists only in the in-memory `DispatchContext` object at dispatch time.

8. **Discoverability** — `tag persona list` renders a formatted table of all installed
   personas (built-in and user-created) with name, domain, and a one-line description. Users
   can filter by domain or tag.

---

## 3. Non-Goals

- **Replacing profiles** — Personas do not and cannot replace profiles. They have no
  knowledge of tool permissions, model names, memory backends, or budget limits. A persona
  without an active profile is inert.

- **Per-message style injection** — Personas are session-level artifacts. There is no API
  for injecting a different style on a per-message or per-turn basis. Callers who need
  per-turn style control should use the profile's system prompt directly.

- **Adversarial or jailbreak personas** — Personas that attempt to override TAG's safety
  instructions, impersonate other AI systems, or instruct the agent to ignore previous
  instructions are explicitly out of scope and are blocked by the persona validator.

- **Persona-scoped tool permissions** — A persona cannot grant or revoke tool permissions.
  Tool access is exclusively controlled by the active profile.

- **Persona-scoped model selection** — A persona cannot override the model declared in the
  active profile. Model selection remains a profile-level concern.

- **Automatic persona updates** — There is no background polling or auto-upgrade mechanism.
  Updates to marketplace personas are explicit user actions (`tag persona pull --update`).

- **Persona execution sandboxing** — Isolating runtime effects of injected style prompts is
  a future concern. Personas are treated as trusted after the security scan passes.

---

## 4. User Stories

| ID  | As a…            | I want to…                                                                                  | So that…                                                                                                  |
|-----|------------------|----------------------------------------------------------------------------------------------|-----------------------------------------------------------------------------------------------------------|
| U1  | Senior engineer  | run `tag persona apply terse-engineer --profile coder` and start a coding session            | the agent drops all filler prose and gives me dense, precise answers without changing any tool permissions |
| U2  | Team lead        | run `tag persona apply verbose-explainer --profile coder --session` during a pairing session | the agent produces step-by-step explanations suitable for a junior developer, reverting when the session ends |
| U3  | Platform engineer | run `tag persona create --name our-company-style --style style-guide.md`                    | I encode our internal communication conventions once and apply them to any profile across the team        |
| U4  | Any user         | run `tag persona list` and `tag persona list --domain security`                             | I can discover all available personas and filter them by domain before choosing one                       |
| U5  | Security engineer | run `tag persona apply security-focused --profile researcher`                               | the agent uses CVE references, threat-model framing, and security terminology when I am doing security research |
| U6  | Data scientist   | run `tag persona apply data-scientist --profile coder`                                       | the agent assumes familiarity with NumPy, pandas, and statistical notation without repeating basics        |
| U7  | Educator         | run `tag persona pull community/tag-personas/teacher`                                        | I install a community-maintained teaching persona without writing the style prompt from scratch            |
| U8  | Developer        | run `tag persona apply terse-engineer data-scientist --profile coder`                        | I stack two personas and the agent combines terse style with data-science conventions in a single session  |

---

## 5. Proposed CLI Surface

All subcommands are grouped under `tag persona`. Existing `tag profile` subcommands are
unaffected.

### 5.1 `tag persona list`

```
tag persona list [--domain <domain>] [--tag <tag>] [--json]
```

Lists all installed personas (built-in and user-created) in a formatted table. Columns:
`NAME`, `DOMAIN`, `INJECT`, `TAGS`, `DESCRIPTION`. `--domain` filters to a single domain
class (`security`, `code`, `data`, `writing`, `general`). `--tag` filters by arbitrary YAML
tag. `--json` emits a JSON array for scripting.

### 5.2 `tag persona show <name>`

```
tag persona show <name> [--json]
```

Displays the full persona YAML including the `style_prompt` field rendered as a formatted
block. `--json` emits raw YAML-parsed JSON. Exits with code 1 if `<name>` is not found.

### 5.3 `tag persona apply <name...> --profile <profile>`

```
tag persona apply <name> [<name>...] --profile <profile> [--session] [--position prepend|append]
```

Applies one or more personas to `<profile>`. Without `--session`, the association is written
to `TAG_HOME/personas.active` and persists across sessions until explicitly removed.
With `--session`, the association is written only to `TAG_HOME/session.json` under the
current `TAG_SESSION_ID` key and is discarded when the session exits.

Multiple `<name>` arguments stack personas in declaration order. `--position` overrides the
`inject_position` declared in the persona YAML (useful for one-off testing). Emits a warning
if two or more active personas are detected as conflicting (see FR-09).

### 5.4 `tag persona create --name <name> --style <file.md>`

```
tag persona create --name <name> --style <file.md> [--domain security|code|data|writing|general] [--tags <tag,...>] [--position prepend|append]
```

Creates a new user persona from a plain-text or Markdown style file. The file contents are
stored verbatim as the `style_prompt` field after size validation (see NFR-02). `--domain`
and `--tags` are stored as metadata for `list` filtering. `--position` defaults to `prepend`.
The resulting persona YAML is written to `TAG_HOME/personas/<name>.yaml`.

### 5.5 `tag persona edit <name>`

```
tag persona edit <name>
```

Opens the persona YAML for `<name>` in `$EDITOR`. Re-validates the file on save (size,
jailbreak pattern scan). Refuses to write back a file that fails validation.

### 5.6 `tag persona delete <name>`

```
tag persona delete <name> [--force]
```

Deletes the persona YAML from `TAG_HOME/personas/`. Refuses to delete built-in personas
(those installed to `src/tag/config/personas/`) unless `--force` is given, which copies
the built-in to the user directory first and then removes the copy. Removes any active
association for the deleted persona from `personas.active` and `session.json`.

### 5.7 `tag persona pull <owner>/<repo>/<persona>`

```
tag persona pull <owner>/<repo>/<persona> [--pin <sha>] [--trust]
```

Downloads a persona YAML from a GitHub repo path or Gist. Reuses the GitHub fetch, SHA
pinning, lock-file, and security scan infrastructure from PRD-035. The fetched persona is
written to `TAG_HOME/personas/<persona>.yaml` and its source SHA is recorded in
`TAG_HOME/personas.lock`. Requires `--trust` to activate after download (same as
`tag profile pull`). Blocked until PRD-034 (Secret Scanning) passes.

---

## 6. Functional Requirements

**FR-01 — Persona YAML schema**
Every persona is a YAML file conforming to the following schema:

```yaml
name: string          # unique identifier, kebab-case, max 64 chars
description: string   # one-line human description, max 120 chars
style_prompt: string  # natural-language style instructions, max 2048 chars
domain: enum          # one of: general | security | code | data | writing
inject_position: enum # one of: prepend | append  (default: prepend)
tags: [string]        # arbitrary list of searchable tags
version: string       # semver string, default "1.0.0"
```

All fields except `tags` and `version` are required. The schema is validated at creation,
edit, and pull time using a `PersonaSchema` Pydantic model in `src/tag/persona.py`.

**FR-02 — System prompt injection at dispatch time**
When a profile is dispatched with one or more active personas, `persona.py` reads the
profile's `system_prompt` string and the `style_prompt` of each active persona, then
produces a merged prompt string: if `inject_position` is `prepend`, the persona prompt is
placed before the profile prompt, separated by a blank line; if `append`, it is placed after.
This merged string is set on the in-memory `DispatchContext.system_prompt` field. The profile
YAML on disk is never modified.

**FR-03 — Session-scoped vs. persistent application**
`tag persona apply --session` writes the active persona list to
`TAG_HOME/session.json["personas"][TAG_SESSION_ID]`. This entry is cleared on clean session
exit. `tag persona apply` without `--session` writes to `TAG_HOME/personas.active` as a
persistent mapping of `profile_name -> [persona_name, ...]`. Both storage paths are read by
the dispatch layer; session-scoped entries take precedence over persistent entries for the
same profile.

**FR-04 — Persona stacking**
When multiple personas are declared active for a profile, their `style_prompt` fields are
concatenated in declaration order, each separated by a newline, before injection into the
profile system prompt. Stacking applies regardless of whether personas were applied together
in a single `apply` call or accumulated over multiple calls.

**FR-05 — Conflict detection**
Before writing a new persona application, `persona.py` checks the set of active personas for
the target profile against a built-in conflict matrix. Any pair in the conflict matrix (e.g.,
`terse-engineer` + `verbose-explainer`) emits a `WARNING: personas <A> and <B> contain
contradictory style instructions. Applying both may produce inconsistent agent behavior.`
to stderr. Application proceeds; the warning is not a hard block.

**FR-06 — Persona validation**
Persona validation runs at create, edit, and pull time. Validation checks: (1) schema
conformance (Pydantic), (2) `style_prompt` size <= 2048 bytes, (3) jailbreak pattern scan
(see Security §9), (4) secret pattern scan for marketplace pulls (PRD-034 integration). A
persona that fails any check is rejected with a clear error message and not written to disk.

**FR-07 — Built-in personas**
Five personas ship with TAG and are installed to `src/tag/config/personas/`:

| Name               | Domain   | Inject   | Description                                              |
|--------------------|----------|----------|----------------------------------------------------------|
| `terse-engineer`   | code     | prepend  | Dense, precise answers; no filler; bullet lists preferred|
| `verbose-explainer`| general  | prepend  | Step-by-step reasoning; analogies; suitable for learners |
| `security-focused` | security | prepend  | CVE references; threat-model framing; attack surface lens|
| `data-scientist`   | data     | prepend  | NumPy/pandas idioms; statistical terminology; plots first |
| `teacher`          | general  | prepend  | Socratic questioning; checks for understanding; patient  |

Built-in personas are read-only. `tag persona edit` on a built-in copies it to the user
persona directory first (like `tag profile edit` for read-only profiles).

**FR-08 — Integration with profile template export**
`tag profile export` (existing feature) exports a profile YAML without any persona
association embedded. Personas are exported separately via `tag persona export <name>` which
emits a standalone persona YAML. This preserves the separation-of-concerns principle in
exported artifacts.

**FR-09 — Persona search and filter**
`tag persona list --domain <domain>` filters the output table to personas with the matching
domain value. `tag persona list --tag <tag>` filters by presence of `<tag>` in the persona's
`tags` list. Both filters are composable (AND semantics). `tag persona list --json` emits a
JSON array for use in scripts and completions.

**FR-10 — Active persona introspection**
`tag persona status [--profile <profile>]` shows the currently active persona(s) for the
given profile (or all profiles if omitted), distinguishing between session-scoped and
persistent activations, and rendering the merged system prompt preview for verification.

**FR-11 — Persona deactivation**
`tag persona unapply [<name>] --profile <profile> [--session]` removes a named persona (or
all personas if no name is given) from the active set for `<profile>`. With `--session`,
only the session-scoped association is removed. Without `--session`, the persistent
association in `personas.active` is removed.

**FR-12 — Inject position override at apply time**
`tag persona apply <name> --profile <profile> --position append` overrides the
`inject_position` declared in the persona YAML for this application only. The override is
stored in the active association record alongside the persona name.

**FR-13 — Persona lock file for marketplace personas**
Marketplace personas installed via `tag persona pull` are recorded in
`TAG_HOME/personas.lock` with: `source_url`, `sha`, `install_timestamp`, `trusted` boolean,
and `local_path`. `tag persona lock` regenerates the lock file from currently installed
marketplace personas. `tag persona verify <name>` re-runs the security scan against the
currently locked SHA.

---

## 7. Non-Functional Requirements

**NFR-01 — Injection latency < 1 ms**
Persona injection is a pure in-memory string concatenation. It must add less than 1 ms to
dispatch time on any hardware capable of running TAG. No I/O is permitted during the inject
phase (persona YAML is loaded once at session startup and cached in memory).

**NFR-02 — Persona size limit: 2048 bytes for `style_prompt`**
The `style_prompt` field must not exceed 2048 bytes (UTF-8 encoded). This limit prevents
context window saturation from persona injection alone, and ensures that the combined
profile system prompt + persona injection stays within reasonable token budgets for all
supported models. Creation and edit commands enforce this limit with a clear error message
showing the byte count.

**NFR-03 — No disk write on dispatch**
The dispatch path must never write to `TAG_HOME/profiles/` or `TAG_HOME/personas/`. Profile
and persona YAML files are read-only during dispatch. Any attempt to modify them from the
dispatch path is a bug.

**NFR-04 — Backward compatibility**
The persona layer is purely additive. Existing profiles and all existing `tag profile`
subcommands continue to operate identically when no persona is active. A fresh TAG
installation with no `personas.active` file behaves exactly as TAG behaved before this PRD.

**NFR-05 — Shell completion support**
`tag persona apply`, `tag persona show`, `tag persona delete`, and `tag persona edit` all
support shell completion for persona names using the same completion infrastructure as
`tag profile switch`.

---

## 8. Technical Design

### 8.1 New Files

| Path                                    | Purpose                                                          |
|-----------------------------------------|------------------------------------------------------------------|
| `src/tag/persona.py`                    | `PersonaSchema`, `PersonaManager`, `inject_persona()`, `detect_conflicts()`, `scan_jailbreak()` |
| `src/tag/config/personas/terse-engineer.yaml`     | Built-in: terse-engineer persona                   |
| `src/tag/config/personas/verbose-explainer.yaml`  | Built-in: verbose-explainer persona                |
| `src/tag/config/personas/security-focused.yaml`   | Built-in: security-focused persona                 |
| `src/tag/config/personas/data-scientist.yaml`     | Built-in: data-scientist persona                   |
| `src/tag/config/personas/teacher.yaml`            | Built-in: teacher persona                          |

### 8.2 Modified Files

| Path                    | Change                                                             |
|-------------------------|--------------------------------------------------------------------|
| `src/tag/controller.py` | Add `cmd_persona_list`, `cmd_persona_show`, `cmd_persona_apply`, `cmd_persona_create`, `cmd_persona_edit`, `cmd_persona_delete`, `cmd_persona_pull`, `cmd_persona_unapply`, `cmd_persona_status`, `cmd_persona_lock`, `cmd_persona_verify` |
| `src/tag/dispatch.py`   | Call `inject_persona()` in dispatch pipeline before model call    |
| `TAG_HOME/session.json` | Add `personas` key: `{TAG_SESSION_ID: {profile_name: [persona_name, ...]}}` |
| `TAG_HOME/`             | New files: `personas.active`, `personas.lock`                      |

### 8.3 Persona YAML Schema (full example)

```yaml
name: terse-engineer
description: Dense, precise answers with no filler prose; bullet lists preferred
style_prompt: |
  Respond with maximum density. Omit introductory phrases, apologies, and affirmations.
  Prefer bullet lists over prose paragraphs. Use technical terminology without definition
  unless asked. Assume the reader is a senior engineer. If uncertain, say so in one
  sentence. Do not repeat the question back.
domain: code
inject_position: prepend
tags:
  - terse
  - engineering
  - senior
version: 1.0.0
```

### 8.4 Injection Algorithm

```python
def inject_persona(profile_system_prompt: str, active_personas: list[PersonaSchema]) -> str:
    if not active_personas:
        return profile_system_prompt
    prepend_parts = [p.style_prompt for p in active_personas if p.inject_position == "prepend"]
    append_parts  = [p.style_prompt for p in active_personas if p.inject_position == "append"]
    parts = []
    if prepend_parts:
        parts.append("\n".join(prepend_parts))
    parts.append(profile_system_prompt)
    if append_parts:
        parts.append("\n".join(append_parts))
    return "\n\n".join(parts)
```

This function is called in `dispatch.py` immediately before the system prompt is serialized
into the model API request. It is a pure function with no side effects.

### 8.5 Active Persona Storage

**Persistent (`TAG_HOME/personas.active`)** — YAML mapping:

```yaml
coder:
  - name: terse-engineer
    position_override: null
  - name: data-scientist
    position_override: null
researcher:
  - name: security-focused
    position_override: append
```

**Session-scoped (`TAG_HOME/session.json` additions)**:

```json
{
  "personas": {
    "abc-session-id-123": {
      "coder": [
        {"name": "verbose-explainer", "position_override": null}
      ]
    }
  }
}
```

Session-scoped entries take precedence over persistent entries for the same profile.
If both are present, the session-scoped list **replaces** (not extends) the persistent list
for the duration of the session.

### 8.6 Conflict Detection Matrix

Built-in conflict pairs (extensible via user configuration):

| Persona A          | Persona B           | Reason                              |
|--------------------|---------------------|-------------------------------------|
| `terse-engineer`   | `verbose-explainer` | Contradictory verbosity instructions|
| `teacher`          | `terse-engineer`    | Socratic patience vs. density       |

User-defined conflicts can be declared in `TAG_HOME/config.yaml` under
`persona_conflicts: [[a, b], ...]`.

### 8.7 Jailbreak Pattern Scan

`scan_jailbreak(style_prompt: str) -> list[str]` checks the persona's `style_prompt` for a
curated list of patterns indicative of jailbreak or adversarial injection attempts:

- Phrases containing "ignore previous instructions", "disregard", "forget your system prompt"
- Instructions to impersonate a named AI system other than TAG
- Instructions containing base64-encoded payloads (heuristic: long alphanum strings without
  spaces)
- Instructions to reveal the system prompt verbatim

The scan returns a list of matched patterns. Any non-empty result causes `persona validate`
to fail with `ERROR: persona contains potentially adversarial instructions: <pattern>`.

---

## 9. Security Considerations

**SC-01 — Personas are system prompt injections**
A persona's `style_prompt` is injected directly into the model system prompt. It carries the
same security risk as any profile system prompt: a malicious persona can attempt to redirect
the agent's behavior. The threat model and mitigations are identical to those documented in
PRD-035 §Security for profile system prompts.

**SC-02 — Marketplace personas require secret scan (PRD-034 dependency)**
`tag persona pull` is blocked until PRD-034 (Secret Scanning) is active and passes on the
fetched YAML. A persona containing embedded secrets (API keys, tokens) or high-confidence
prompt injection patterns must be rejected before being written to disk. Shipping `persona
pull` before PRD-034 creates the same supply-chain attack vector as PRD-035.

**SC-03 — Persona size limit prevents context overflow**
The 2048-byte cap on `style_prompt` (NFR-02) limits the maximum tokens a persona can inject.
This prevents a crafted large persona from consuming the entire context window and crowding
out the user's actual task instructions or the profile's safety guardrails.

**SC-04 — Jailbreak persona detection**
The `scan_jailbreak()` function (§8.7) runs at create, edit, and pull time. Personas that
contain instructions explicitly designed to override safety instructions, impersonate other
AI systems, or exfiltrate the system prompt are blocked at the validation gate.

**SC-05 — Built-in personas are read-only at the package level**
Built-in personas installed to `src/tag/config/personas/` are read-only. They cannot be
overwritten by `tag persona create` or `tag persona pull`. User customizations are always
written to `TAG_HOME/personas/`. This preserves a known-good baseline and prevents a supply
chain attack that overwrites a trusted built-in at install time.

**SC-06 — No credential fields in persona YAML**
The persona YAML schema has no fields that accept credentials, tokens, or API keys. The
`PersonaSchema` Pydantic model rejects any unrecognized field (strict mode). This eliminates
the risk of a persona YAML being used as a credential transport vehicle.

**SC-07 — SHA pinning for marketplace personas**
Marketplace personas installed via `tag persona pull` are recorded in `personas.lock` with
an exact Git commit SHA. Subsequent activations verify that the file on disk matches the
locked SHA before injection. A SHA mismatch triggers `ERROR: persona <name> has been
modified since installation. Run 'tag persona verify <name>' to re-scan and re-lock.`

**SC-08 — Explicit --trust flag required for marketplace personas**
Downloaded marketplace personas cannot be activated without an explicit `--trust` flag. The
flag is not stored; it must be re-supplied on the first `apply` after a `pull`. This creates
a deliberate human review checkpoint before any untrusted `style_prompt` is injected into a
production session.

---

## 10. Testing Strategy

**Unit tests (`tests/test_persona.py`)**

- `test_inject_prepend` — verifies that a single prepend persona places `style_prompt`
  before the profile system prompt with correct blank-line separator.
- `test_inject_append` — verifies that a single append persona places `style_prompt`
  after the profile system prompt.
- `test_inject_stack_order` — verifies that two prepend personas are concatenated in
  declaration order before the profile prompt.
- `test_inject_mixed_positions` — verifies correct ordering when stacking one prepend and
  one append persona together.
- `test_inject_empty_personas` — verifies that `inject_persona(prompt, [])` returns the
  original prompt unchanged.
- `test_inject_no_profile_prompt` — verifies that injection with an empty profile prompt
  returns only the persona prompt.

**Session scoping tests (`tests/test_persona_session.py`)**

- `test_session_scoped_overrides_persistent` — applies a persistent persona and a
  contradictory session-scoped persona; verifies session-scoped takes precedence.
- `test_session_clear_on_exit` — simulates session exit; verifies that the session entry
  in `session.json` is removed and the persistent persona is restored.
- `test_no_session_fallback_to_persistent` — verifies that when no session-scoped entry
  exists, persistent entry is used.

**Conflict detection tests (`tests/test_persona_conflicts.py`)**

- `test_conflict_warning_emitted` — stacks `terse-engineer` + `verbose-explainer`; captures
  stderr; asserts warning string is present.
- `test_no_conflict_for_compatible_personas` — stacks `terse-engineer` + `security-focused`;
  asserts no warning emitted.
- `test_custom_conflict_pair` — adds a custom conflict pair to config; asserts warning fires.

**Jailbreak scan tests (`tests/test_persona_security.py`)**

- `test_scan_detects_ignore_instructions` — persona containing "ignore previous instructions"
  is rejected.
- `test_scan_detects_impersonation` — persona instructing agent to "act as GPT-4" is
  rejected.
- `test_scan_detects_base64_payload` — persona containing a long base64-encoded string
  triggers the heuristic.
- `test_scan_passes_clean_persona` — well-formed terse-engineer style prompt passes scan.

**Marketplace pull security tests (`tests/test_persona_pull.py`)**

- `test_pull_blocked_without_prd034` — `tag persona pull` before PRD-034 activation returns
  error code 1 with message referencing the dependency.
- `test_pull_sha_mismatch_detected` — lock file SHA != file SHA triggers verification error.
- `test_pull_requires_trust_flag` — applying a pulled persona without `--trust` is rejected.

**Integration tests (`tests/test_persona_integration.py`)**

- `test_end_to_end_apply_and_dispatch` — creates a profile, applies `terse-engineer`, runs a
  mock dispatch, and asserts the injected system prompt contains the persona preamble.
- `test_end_to_end_stack_two_personas` — stacks `terse-engineer` + `security-focused` and
  validates the merged prompt structure.

---

## 11. Acceptance Criteria

**AC-01** — `tag persona list` outputs a formatted table containing at minimum the five
built-in personas (`terse-engineer`, `verbose-explainer`, `security-focused`,
`data-scientist`, `teacher`) with correct `DOMAIN` and `INJECT` columns populated.

**AC-02** — `tag persona apply terse-engineer --profile coder` followed by a mock dispatch
of the `coder` profile produces a system prompt that begins with the `terse-engineer`
`style_prompt` text, not the profile's original system prompt text.

**AC-03** — `tag persona apply verbose-explainer --profile coder --session` persists the
session association in `TAG_HOME/session.json` and does not modify `TAG_HOME/personas.active`.
After session exit, the `session.json` entry is absent.

**AC-04** — `tag persona apply terse-engineer verbose-explainer --profile coder` emits a
conflict warning to stderr containing both persona names and does not exit with a non-zero
code.

**AC-05** — `tag persona create --name test-persona --style /tmp/style.md` with a style file
exceeding 2048 bytes exits with code 1 and an error message stating the byte count.

**AC-06** — `tag persona create --name jailbreak-test --style /tmp/jailbreak.md` where
`jailbreak.md` contains "ignore previous instructions" exits with code 1 and names the
matched pattern.

**AC-07** — `tag persona delete terse-engineer` without `--force` exits with code 1 and
message `terse-engineer is a built-in persona; use --force to override`.

**AC-08** — `tag persona apply security-focused --profile coder --position append` causes
dispatch to produce a system prompt where the `security-focused` prompt appears after, not
before, the profile system prompt, overriding the persona's declared `prepend` position.

**AC-09** — `tag persona unapply --profile coder` removes all active personas for the `coder`
profile from `personas.active` and the resulting dispatch produces an unmodified profile
system prompt.

**AC-10** — `tag persona show terse-engineer --json` emits valid JSON containing at minimum
the keys `name`, `description`, `style_prompt`, `domain`, `inject_position`, `tags`,
`version`.

---

## 12. Dependencies

| PRD     | Title                        | Dependency type                                                  |
|---------|------------------------------|------------------------------------------------------------------|
| PRD-034 | Secret Scanning              | HARD BLOCK — `tag persona pull` must not ship before PRD-034    |
| PRD-035 | Profile Marketplace          | INFRASTRUCTURE — `persona pull/push/lock/verify` reuses GitHub fetch, SHA pin, lock file, and secret scan integration from PRD-035 |

No other PRDs are required. The core persona injection layer (create, apply, list, stack,
session scoping) has no external dependencies and can ship independently of PRD-034/035,
as long as `tag persona pull` is gated.

---

## 13. Open Questions

**OQ-01 — Persona vs. profile system prompt merging strategy for edge cases**
The current design places persona prompts before or after the profile system prompt. Some
profiles use structured system prompts with section headers (e.g., `## Tools`, `## Rules`).
Should TAG support injection at a named section marker (e.g., inject after `## Style:`)?
This would require a more complex merge strategy and a new `inject_position: after-section`
value. Decision needed before implementation.

**OQ-02 — Persona versioning and semver enforcement**
The schema includes a `version` field but this PRD does not define update semantics. When a
user runs `tag persona pull --update`, should TAG enforce that the remote version is strictly
greater than the installed version? Should it block downgrades? The versioning policy for
personas should mirror whatever policy PRD-035 adopts for profiles, but this has not yet been
decided in PRD-035 either.

**OQ-03 — Conflict resolution for contradictory personas: warn or block?**
The current design emits a warning but does not block application of contradictory personas.
Some teams may prefer a strict mode that blocks application of conflicting stacks. Should
`config.yaml` expose a `persona_conflict_mode: warn|block` setting? This affects UX
significantly and should be decided before the CLI surface is finalized.

**OQ-04 — Session identity for `--session` scoping**
`TAG_SESSION_ID` is not yet a formally defined concept in TAG. This PRD assumes it maps to
the shell session's PID or a UUID generated at shell startup and exported as an environment
variable. The exact mechanism for generating, persisting, and expiring session IDs needs a
design decision, potentially warranting a micro-PRD or an addition to this PRD.

**OQ-05 — Persona inheritance / extends field**
Should a persona be able to declare `extends: terse-engineer` to inherit all fields from
another persona and override only specific fields? This would enable users to create
`our-terse-engineer` without copy-pasting the base `style_prompt`. Inheritance adds
complexity to the validator and resolver. Deferred to a follow-up PRD unless the community
strongly requests it.

---

## 14. Complexity and Timeline

**Complexity:** M (Medium)

**Estimated timeline:** 1 sprint (~2 weeks)

| Week | Deliverable                                                                                         |
|------|-----------------------------------------------------------------------------------------------------|
| 1    | `src/tag/persona.py` (schema, manager, inject, conflict detection, jailbreak scan); built-in persona YAMLs; unit tests passing |
| 1    | `controller.py` handlers for `list`, `show`, `create`, `edit`, `delete`, `apply`, `unapply`, `status` |
| 2    | Session-scoped storage (`session.json` integration); persistent storage (`personas.active`); integration tests |
| 2    | `tag persona pull` gated behind PRD-034 check; `personas.lock` infrastructure reusing PRD-035; marketplace pull security tests; AC verification pass |

**Risk:** Low for the core injection and CLI surface. Medium for the `--session` scoping work
(depends on `TAG_SESSION_ID` design — see OQ-04). `tag persona pull` is blocked on PRD-034
availability and does not gate the rest of the sprint.
