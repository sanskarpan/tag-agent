# PRD-037: Notification Hooks (`tag hooks notify`)

**Status:** Proposed
**Priority:** P1
**Estimated Effort:** M (1 sprint, ~1 week)
**Affects:** `src/tag/notifications.py` (new), `src/tag/controller.py` (extend `_fire_hooks`, `cmd_hooks`, `cmd_hooks_notify` subcommand tree), `tag.sqlite3` (new `notification_log` table)
**Depends on:** PRD-016 (Webhook Event Triggers — implemented)

---

## 1. Overview

TAG's event hook system (PRD-016) fires shell commands and raw HTTP webhooks when lifecycle events occur (`run.completed`, `run.failed`, `budget.warning`, etc.). This is powerful but low-level: sending a Slack message requires the user to write a curl command; sending email requires a shell one-liner that embeds SMTP credentials. Neither approach is ergonomic, auditable, or safe.

Notification hooks add **first-class, structured notification delivery** as a distinct channel abstraction layered on top of the existing hook system. Users declare a notification hook once, credentials live in their profile `.env`, and TAG handles delivery, retry, logging, and templating. The result is that "notify me on Slack when my overnight benchmark completes" becomes a single CLI command rather than a shell script.

Four delivery channels are supported in this version:

- **Slack** — incoming webhook POST to a Slack App webhook URL
- **Email** — SMTP with STARTTLS/SSL, targeting a single recipient address
- **Desktop** — macOS `osascript` alert or Linux `notify-send` toast
- **Webhook** — generic HTTP POST with a JSON body (generalisation of PRD-016's existing webhook action)

Notification hooks integrate with the existing `_fire_hooks()` engine: a `notify` action type is added alongside the existing `webhook`, `run_command`, and `tag_submit` types. The new `tag hooks notify` subcommand tree lets users add, list, test, remove, enable, and disable notification hooks without hand-editing YAML.

---

## 2. Goals

1. **Slack delivery** — POST a rendered message to any Slack incoming webhook URL; no Slack SDK dependency.
2. **SMTP email delivery** — send email via Python stdlib `smtplib` with STARTTLS or SSL; support Gmail, Outlook, and self-hosted SMTP relays.
3. **macOS and Linux desktop notifications** — use `osascript` on macOS and `notify-send` on Linux; graceful no-op on unsupported platforms.
4. **Generic webhook** — POST a JSON payload to any URL, extending the existing webhook action with per-hook auth headers, retry, and delivery logging.
5. **Per-channel credential isolation** — webhook URLs, SMTP passwords, and API tokens are stored as environment variable references in the profile's `.env` file, never in the YAML config; the config YAML stores only the variable name.
6. **Message template engine** — `{{variable}}` substitution with a defined allowlist of template variables (`run_id`, `profile`, `duration`, `tokens_used`, `cost_usd`, `status`, `error_message`, `task`); no Jinja2 dependency.
7. **Test command** — `tag hooks notify test --channel slack --webhook-url <url>` fires a synthetic test notification so users can verify configuration before relying on it in production.
8. **Per-profile scoping** — notification hooks can be scoped to a specific profile with `--profile <name>`; hooks with no profile filter fire for all profiles.
9. **Delivery retry** — failed deliveries are retried up to 3 times with exponential backoff (1 s, 2 s, 4 s); final failure is logged to SQLite.
10. **Delivery log** — every delivery attempt is written to the `notification_log` table with outcome, HTTP response code, attempt number, and timestamp; message content is never stored in the log.

---

## 3. Non-Goals

- **PagerDuty / OpsGenie / SMS** — out of scope; a generic webhook can reach these if needed.
- **Notification aggregation and deduplication** — if 100 loop turns fire 100 `run.completed` events, 100 notifications are delivered; batching is tracked as an open question (Section 13).
- **Two-way notifications** — replying to a Slack message or email to approve a loop turn is out of scope; tracked as open question (Section 13).
- **Push notifications to mobile apps** — no APNs/FCM integration.
- **Notification scheduling or snooze** — no time-based suppression windows.
- **Rich HTML email** — plaintext email only; no HTML templating or attachments.
- **OAuth flows for Slack or email providers** — incoming webhook URLs and app passwords are used; no OAuth PKCE dance.

---

## 4. User Stories

| ID | As a… | I want to… | So that… |
|----|-------|-----------|----------|
| U1 | Developer running overnight benchmarks | receive a Slack message in my `#agents` channel when `tag submit` completes on the `benchmark` profile | I wake up to results rather than polling `tag runs list` |
| U2 | Team lead with a budget cap | get an email when any profile's spend hits the `budget.warning` threshold | I can intervene before hitting the hard cap and blocking the team |
| U3 | Developer with a long-running loop | receive a macOS desktop notification when an autonomous loop completes after 50+ turns | I know to check the output without keeping a terminal visible |
| U4 | Developer setting up a new Slack integration | run `tag hooks notify test --channel slack --webhook-url $SLACK_WEBHOOK_URL` before committing the hook to config | I confirm the webhook URL is valid and messages are formatted correctly |
| U5 | Developer managing multiple profiles | disable email notifications on the `dev` profile but keep them on the `coder` profile | I avoid alert fatigue during rapid iteration while preserving production alerting |
| U6 | DevOps engineer integrating TAG into CI/CD | configure a generic webhook notification on `run.failed` events pointing to an internal alerting endpoint | my incident management system receives structured JSON payloads without custom shell wrappers |
| U7 | Developer debugging a flaky task | see the delivery log for a specific notification hook | I confirm whether the notification was delivered and what the Slack/HTTP response was |

---

## 5. Proposed CLI Surface

All notification hook management lives under `tag hooks notify`. The parent `tag hooks` subcommand tree (list, log, test from PRD-016) is unchanged.

### 5.1 Add a notification hook

```
tag hooks notify add \
  --event <event_type> \
  --channel <slack|email|desktop|webhook> \
  [--profile <profile_name>] \
  [--message-template "Run {{run_id}} finished in {{duration}}s — cost ${{cost_usd}}"] \
  [--name <hook_name>]

# Slack-specific options
  --webhook-url <url_or_env_var_ref>         # e.g. "$SLACK_WEBHOOK_URL"

# Email-specific options
  --smtp-host <host>                          # e.g. smtp.gmail.com
  --smtp-port <port>                          # default: 587
  --smtp-user <username_or_env_var_ref>       # e.g. "$SMTP_USER"
  --smtp-password-env <env_var_name>          # always an env var name, never a literal password
  --to <recipient@example.com>
  --from <sender@example.com>
  --subject-template "TAG: {{event_type}} on {{profile}}"

# Desktop — no extra options required
# Webhook-specific options
  --url <url_or_env_var_ref>
  --header "Authorization: Bearer $TOKEN"     # repeatable; values may be env var refs
```

**Examples:**

```bash
# Slack: notify when any run completes on the coder profile
tag hooks notify add \
  --event run.complete \
  --channel slack \
  --webhook-url "$SLACK_WEBHOOK_URL" \
  --profile coder \
  --message-template "Run {{run_id}} done in {{duration}}s (cost \${{cost_usd}})"

# Email: notify when a loop fails
tag hooks notify add \
  --event run.failed \
  --channel email \
  --smtp-host smtp.gmail.com \
  --smtp-user me@company.com \
  --smtp-password-env SMTP_APP_PASSWORD \
  --to oncall@company.com \
  --message-template "TAG run {{run_id}} failed on profile {{profile}}: {{error_message}}"

# Desktop: notify on budget warning
tag hooks notify add \
  --event budget.warning \
  --channel desktop \
  --message-template "TAG budget warning: {{profile}} at {{cost_usd}} USD"

# Generic webhook
tag hooks notify add \
  --event run.failed \
  --channel webhook \
  --url "$ALERTMANAGER_WEBHOOK_URL" \
  --header "X-Tag-Source: tag-agent"
```

### 5.2 List notification hooks

```
tag hooks notify list [--json]
```

Output (tabular):

```
ID          Name                  Event          Channel   Profile   Status
----------  --------------------  -------------  --------  --------  --------
a1b2c3d4    slack-coder-complete  run.complete   slack     coder     enabled
e5f6a7b8    budget-email          budget.warning email     (all)     enabled
c9d0e1f2    desktop-loop          loop.complete  desktop   (all)     enabled
```

### 5.3 Test a notification channel

```
tag hooks notify test \
  --channel slack \
  --webhook-url <url>

tag hooks notify test \
  --channel email \
  --smtp-host smtp.gmail.com \
  --smtp-user me@company.com \
  --smtp-password-env SMTP_APP_PASSWORD \
  --to me@company.com

tag hooks notify test --channel desktop

tag hooks notify test --channel webhook --url <url>
```

Fires a synthetic notification with a fixed test payload and reports success/failure to stdout. Does not require a hook to be registered first.

### 5.4 Remove a notification hook

```
tag hooks notify remove <hook-id>
```

Removes the hook from config. Delivery log entries for the hook are retained.

### 5.5 Enable / Disable a notification hook

```
tag hooks notify disable <hook-id>
tag hooks notify enable <hook-id>
```

Sets `enabled: false` / `enabled: true` in the hook config. Disabled hooks are skipped by `_fire_hooks()` but remain in the config and log.

### 5.6 Show delivery log

```
tag hooks notify log [--hook <hook-id>] [--limit <n>] [--json]
```

Shows entries from the `notification_log` table. Does not show message content (per security requirement S8).

---

## 6. Functional Requirements

### 6.1 Slack channel

**FR-01** — When a notification hook with `channel: slack` fires, `SlackNotifier` SHALL POST a JSON body `{"text": "<rendered_message>"}` to the configured webhook URL using an HTTP POST request with `Content-Type: application/json`.

**FR-02** — The Slack webhook URL SHALL be accepted as a literal URL on the CLI (`--webhook-url https://hooks.slack.com/...`) or as an environment variable reference (`--webhook-url "$SLACK_WEBHOOK_URL"`). In both cases, the stored config YAML SHALL contain only the environment variable reference (e.g. `webhook_url_env: SLACK_WEBHOOK_URL`); a literal URL passed on the CLI SHALL be auto-stored in the profile `.env` under a generated variable name and replaced with the reference.

**FR-03** — If the Slack POST returns a non-2xx HTTP status, the delivery SHALL be marked as failed and retried (see FR-15).

### 6.2 Email channel

**FR-04** — When a notification hook with `channel: email` fires, `EmailNotifier` SHALL send email via Python stdlib `smtplib.SMTP` with STARTTLS (default port 587) or `smtplib.SMTP_SSL` (port 465) depending on `--smtp-port`.

**FR-05** — SMTP credentials (username, password) SHALL be read exclusively from environment variables at delivery time; the config stores only the env var name (e.g. `smtp_password_env: SMTP_APP_PASSWORD`). Credentials SHALL never be written to the config YAML or the delivery log.

**FR-06** — The email subject and body SHALL be rendered from configurable templates (see FR-10); the default subject template is `"TAG: {{event_type}} — {{profile}}"` and the default body template is `"Run {{run_id}} — Status: {{status}}\n\nProfile: {{profile}}\nDuration: {{duration}}s\nCost: ${{cost_usd}}\n\nError: {{error_message}}"`.

**FR-07** — Email delivery SHALL timeout after 15 seconds to avoid blocking the hook-firing thread.

### 6.3 Desktop channel

**FR-08** — On macOS, `DesktopNotifier` SHALL deliver notifications by calling `osascript -e 'display notification "<message>" with title "<title>"'` as a subprocess. On Linux, it SHALL call `notify-send "<title>" "<message>"`. On other platforms (Windows, CI environments where neither binary is available), it SHALL log a warning and return without error.

**FR-09** — Desktop notification delivery SHALL be best-effort; failure to deliver (missing `osascript`/`notify-send`, subprocess timeout) SHALL be logged but SHALL NOT cause the hook to be retried — desktop notifications are inherently ephemeral.

### 6.4 Generic webhook channel

**FR-10-webhook** — When a notification hook with `channel: webhook` fires, `WebhookNotifier` SHALL POST the full event payload as JSON to the configured URL. Custom headers defined with `--header` SHALL be included in the request; header values that begin with `$` are resolved from the environment at delivery time.

### 6.5 Template engine

**FR-10** — All message templates use `{{variable}}` syntax. The template engine SHALL perform simple string substitution — no loops, conditionals, or filters. This avoids a Jinja2 dependency.

**FR-11** — The following template variables SHALL be available for all channels and all events:

| Variable | Source | Description |
|----------|--------|-------------|
| `run_id` | `runs` table or event payload | Short run identifier |
| `profile` | event payload | Profile name that fired the event |
| `duration` | computed from `started_at` / `completed_at` | Wall-clock seconds as integer |
| `tokens_used` | event payload | Total tokens consumed by the run |
| `cost_usd` | event payload | Cost in USD, formatted to 4 decimal places |
| `status` | event payload | `completed`, `failed`, `warning`, etc. |
| `error_message` | event payload | First 500 characters of the error message, if any |
| `task` | event payload | The task string submitted to the agent |
| `event_type` | always available | The lifecycle event type string |
| `timestamp` | generated at delivery time | ISO-8601 UTC timestamp |

**FR-12** — Template variables with no value in the payload SHALL be replaced with an empty string, not left as `{{variable}}` literals.

**FR-13** — Template variable names outside the allowlist in FR-11 SHALL be silently ignored (replaced with empty string); arbitrary payload keys SHALL NOT be interpolated to prevent accidental leakage of sensitive run data.

### 6.6 Per-profile filtering

**FR-14** — A notification hook configured with `--profile <name>` SHALL only fire when the event payload's `profile` field matches `<name>`. Hooks with no `--profile` flag fire for all profiles.

### 6.7 Retry and delivery log

**FR-15** — Failed deliveries for Slack, email, and webhook channels SHALL be retried up to 3 times with exponential backoff: wait 1 s before retry 1, 2 s before retry 2, 4 s before retry 3. Desktop notifications SHALL NOT be retried (FR-09).

**FR-16** — Every delivery attempt (success or failure) SHALL be written to the `notification_log` table with: `hook_id`, `event_type`, `channel`, `status` (`delivered` | `failed` | `retrying`), `attempt` (1–3), `response_code` (HTTP status code or `null` for non-HTTP channels), `error_detail` (truncated exception message, max 200 chars), and `delivered_at` (UTC timestamp). Message content SHALL NOT be stored.

**FR-17** — The `notification_log` table SHALL retain entries for 90 days. A cleanup job SHALL run at startup (in `open_db()`) to delete entries older than 90 days.

### 6.8 Async delivery

**FR-18** — Notification delivery SHALL be non-blocking. `_fire_hooks()` SHALL dispatch notification hooks to a daemon `threading.Thread`; the caller returns immediately.

**FR-19** — On dry-run (`--dry-run` flag or `DRY_RUN=1` env var), notification hooks SHALL NOT fire. A log line `[dry-run] would deliver <channel> notification for <event_type>` SHALL be written to stderr instead.

---

## 7. Non-Functional Requirements

**NFR-01 — Delivery latency** — Notification delivery (from event emission to HTTP/SMTP request completion) SHALL complete within 5 seconds under normal network conditions, excluding retry attempts.

**NFR-02 — Non-blocking** — Notification hook execution SHALL never block the main CLI process. All delivery happens in daemon threads; even if the Slack API is unreachable, `tag submit` completes normally.

**NFR-03 — Retry backoff** — Retry delays SHALL use exponential backoff (1 s, 2 s, 4 s). The total worst-case delivery time for a hook that ultimately fails after 3 retries is 7 seconds of wait time plus network I/O.

**NFR-04 — Delivery log retention** — Entries in `notification_log` are retained for 90 days then deleted on next `open_db()` call.

**NFR-05 — No additional required runtime dependencies** — `smtplib` is stdlib; HTTP requests use `urllib.request` (already used in controller.py). If `httpx` or `requests` is available as an optional dependency it MAY be used for connection pooling, but the feature MUST work with stdlib only.

**NFR-06 — No-op on dry-run** — Dry-run mode MUST suppress all outbound network calls and subprocess invocations from notification hooks.

**NFR-07 — Credential safety** — At no point SHALL a webhook URL, SMTP password, or auth token appear in the `notification_log` table, in log output to stderr, or in the output of `tag hooks notify list`.

**NFR-08 — Graceful degradation** — If a channel dependency is unavailable (`notify-send` not installed, SMTP server refused, Slack returns 429), TAG SHALL log the failure and continue; it SHALL NOT raise an unhandled exception to the CLI layer.

---

## 8. Technical Design

### 8.1 New module: `src/tag/notifications.py`

```
src/tag/notifications.py
```

Contains:
- `BaseNotifier` — abstract base class defining `deliver(event_type, payload, template) -> DeliveryResult`
- `SlackNotifier(BaseNotifier)` — Slack incoming webhook implementation
- `EmailNotifier(BaseNotifier)` — smtplib SMTP/SMTP_SSL implementation
- `DesktopNotifier(BaseNotifier)` — osascript/notify-send subprocess implementation
- `WebhookNotifier(BaseNotifier)` — generic HTTP POST implementation
- `render_template(template: str, payload: dict) -> str` — allowlist-filtered `{{variable}}` substitution
- `DeliveryManager` — loads notifiers from config, dispatches to daemon threads, handles retry, writes `notification_log`
- `NotificationHook` — dataclass representing one configured notification hook

`DeliveryManager.fire(event_type, payload, db_path)` is called from `_fire_hooks()` in `controller.py` when a hook with `type: notify` is encountered.

### 8.2 BaseNotifier interface

```python
from __future__ import annotations
import abc
from dataclasses import dataclass
from typing import Any

@dataclass
class DeliveryResult:
    success: bool
    response_code: int | None   # HTTP status or None
    error_detail: str | None    # truncated exception message or None

class BaseNotifier(abc.ABC):
    @abc.abstractmethod
    def deliver(
        self,
        rendered_message: str,
        subject: str | None = None,
    ) -> DeliveryResult:
        """Deliver one notification. Returns DeliveryResult."""
```

### 8.3 Channel implementations

**SlackNotifier**

```python
class SlackNotifier(BaseNotifier):
    def __init__(self, webhook_url: str):
        self._url = webhook_url  # resolved from env at construction time

    def deliver(self, rendered_message: str, subject: str | None = None) -> DeliveryResult:
        payload = {"text": rendered_message}
        data = json.dumps(payload).encode()
        req = urllib.request.Request(
            self._url, data=data, method="POST",
            headers={"Content-Type": "application/json"},
        )
        try:
            with urllib.request.urlopen(req, timeout=10) as resp:
                return DeliveryResult(success=True, response_code=resp.status, error_detail=None)
        except urllib.error.HTTPError as e:
            return DeliveryResult(success=False, response_code=e.code, error_detail=str(e))
        except Exception as e:
            return DeliveryResult(success=False, response_code=None, error_detail=str(e)[:200])
```

**EmailNotifier**

```python
class EmailNotifier(BaseNotifier):
    def __init__(self, smtp_host: str, smtp_port: int, smtp_user: str,
                 smtp_password: str, to_addr: str, from_addr: str):
        # All resolved from env at construction time; never stored in config YAML
        ...

    def deliver(self, rendered_message: str, subject: str | None = None) -> DeliveryResult:
        # Uses smtplib.SMTP (port 587, STARTTLS) or smtplib.SMTP_SSL (port 465)
        # 15-second socket timeout
        ...
```

**DesktopNotifier**

```python
class DesktopNotifier(BaseNotifier):
    def deliver(self, rendered_message: str, subject: str | None = None) -> DeliveryResult:
        import platform, shutil, subprocess
        sys_platform = platform.system()
        if sys_platform == "Darwin":
            script = f'display notification "{rendered_message}" with title "TAG"'
            subprocess.run(["osascript", "-e", script], timeout=5, capture_output=True)
            return DeliveryResult(success=True, response_code=None, error_detail=None)
        elif sys_platform == "Linux" and shutil.which("notify-send"):
            subprocess.run(["notify-send", "TAG", rendered_message], timeout=5, capture_output=True)
            return DeliveryResult(success=True, response_code=None, error_detail=None)
        else:
            return DeliveryResult(success=False, response_code=None,
                                  error_detail="no desktop notification support on this platform")
```

**WebhookNotifier**

```python
class WebhookNotifier(BaseNotifier):
    def __init__(self, url: str, headers: dict[str, str]):
        # Header values beginning with $ are resolved from env
        ...

    def deliver(self, rendered_message: str, subject: str | None = None) -> DeliveryResult:
        # POSTs full event payload JSON; rendered_message is included as "message" field
        ...
```

### 8.4 Template engine

```python
_ALLOWED_VARS = frozenset({
    "run_id", "profile", "duration", "tokens_used", "cost_usd",
    "status", "error_message", "task", "event_type", "timestamp",
})

def render_template(template: str, payload: dict[str, Any]) -> str:
    result = template
    for var in _ALLOWED_VARS:
        value = str(payload.get(var, ""))
        result = result.replace("{{" + var + "}}", value)
    # Strip any remaining {{...}} patterns that are not in the allowlist
    import re
    result = re.sub(r"\{\{[^}]+\}\}", "", result)
    return result
```

### 8.5 DeliveryManager and retry

```python
class DeliveryManager:
    MAX_RETRIES = 3
    BACKOFF_BASE = 1  # seconds; retry waits: 1s, 2s, 4s

    def fire_async(self, hook: NotificationHook, event_type: str,
                   payload: dict, db_path: Path) -> None:
        import threading
        t = threading.Thread(
            target=self._deliver_with_retry,
            args=(hook, event_type, payload, db_path),
            daemon=True,
        )
        t.start()

    def _deliver_with_retry(self, hook: NotificationHook, event_type: str,
                             payload: dict, db_path: Path) -> None:
        notifier = self._build_notifier(hook)
        rendered = render_template(hook.message_template, payload)
        subject = render_template(hook.subject_template or "", payload) or None
        max_attempts = 1 if hook.channel == "desktop" else self.MAX_RETRIES + 1
        for attempt in range(1, max_attempts + 1):
            result = notifier.deliver(rendered, subject)
            self._log(db_path, hook, event_type, result, attempt)
            if result.success:
                break
            if attempt < max_attempts:
                import time
                time.sleep(self.BACKOFF_BASE * (2 ** (attempt - 1)))
```

### 8.6 SQLite schema: `notification_log` table

```sql
CREATE TABLE IF NOT EXISTS notification_log (
    id            TEXT PRIMARY KEY,
    hook_id       TEXT NOT NULL,       -- references the hook's name/id in config
    event_type    TEXT NOT NULL,       -- e.g. "run.complete"
    channel       TEXT NOT NULL,       -- "slack" | "email" | "desktop" | "webhook"
    status        TEXT NOT NULL,       -- "delivered" | "failed" | "retrying"
    attempt       INTEGER NOT NULL,    -- 1, 2, or 3
    response_code INTEGER,             -- HTTP status code or NULL
    error_detail  TEXT,                -- truncated error message or NULL
    delivered_at  TEXT NOT NULL        -- UTC ISO-8601 timestamp
    -- NOTE: message content is intentionally not stored
);
CREATE INDEX IF NOT EXISTS idx_notification_log_hook_id ON notification_log(hook_id);
CREATE INDEX IF NOT EXISTS idx_notification_log_delivered_at ON notification_log(delivered_at);
```

The `notification_log` table is created in `open_db()` alongside the existing `hook_log`, `events`, and `runs` tables.

### 8.7 Config YAML schema (per-hook)

Notification hooks are stored in the profile YAML under the `hooks` key alongside existing hook types. The distinguishing marker is `type: notify`:

```yaml
hooks:
  run.complete:
    - name: slack-coder-complete
      type: notify
      channel: slack
      profile_filter: coder               # optional; omit to match all profiles
      webhook_url_env: SLACK_WEBHOOK_URL  # env var name, not the URL itself
      enabled: true
      message_template: "Run {{run_id}} done in {{duration}}s — cost ${{cost_usd}}"

  run.failed:
    - name: email-oncall-on-failure
      type: notify
      channel: email
      smtp_host: smtp.gmail.com
      smtp_port: 587
      smtp_user_env: SMTP_USER
      smtp_password_env: SMTP_APP_PASSWORD
      to: oncall@company.com
      from: tag-agent@company.com
      subject_template: "TAG run failed on {{profile}}"
      message_template: "Run {{run_id}} failed.\nError: {{error_message}}\nCost so far: ${{cost_usd}}"
      enabled: true

  budget.warning:
    - name: desktop-budget-alert
      type: notify
      channel: desktop
      enabled: true
      message_template: "Budget warning on {{profile}}: ${{cost_usd}} spent"
```

### 8.8 Integration with `_fire_hooks()`

`_fire_hooks()` in `controller.py` is extended to detect hooks with `type: notify` and delegate to `DeliveryManager`:

```python
def _fire_hooks(cfg, event_type, payload, db_path=None):
    hooks = cfg.get("hooks", {}).get(event_type, [])
    ...
    for hook in hooks:
        if not hook.get("enabled", True):
            continue
        hook_type = hook.get("type", "shell")
        if hook_type == "notify":
            from tag.notifications import DeliveryManager, NotificationHook
            dm = DeliveryManager()
            nh = NotificationHook.from_dict(hook)
            # profile filter check
            if nh.profile_filter and payload.get("profile") != nh.profile_filter:
                continue
            # dry-run guard
            if os.environ.get("DRY_RUN") == "1":
                print(f"[dry-run] would deliver {nh.channel} notification for {event_type}",
                      file=sys.stderr)
                continue
            dm.fire_async(nh, event_type, payload, db_path)
        else:
            # existing shell/webhook/tag_submit handling
            ...
```

### 8.9 New CLI command tree: `tag hooks notify`

The `tag hooks notify` subcommand is added to the existing `hooks` parser in `controller.py`. The subcommands are: `add`, `list`, `test`, `remove`, `disable`, `enable`, `log`. Each subcommand calls a new `cmd_hooks_notify(args)` function.

`cmd_hooks_notify` reads and writes the config YAML for `add`/`remove`/`disable`/`enable`. For `test`, it constructs a notifier directly without touching config and delivers a fixed test payload.

---

## 9. Security Considerations

**S1 — Webhook URL exposure** — Slack webhook URLs and generic webhook URLs SHALL never be stored as literal values in the YAML config file. The CLI converts a literal URL to a profile `.env` variable on first use and stores only the env var name in YAML. This prevents the secret from appearing in version-controlled config files.

**S2 — SMTP credentials** — SMTP passwords SHALL only be passed via `--smtp-password-env <VAR_NAME>` (pointing to an environment variable), never as CLI flag literal values. The controller SHALL refuse `--smtp-password` as a CLI flag; only `--smtp-password-env` is accepted.

**S3 — Slack webhook URL rotation** — Slack webhook URLs are stateless secrets that cannot be revoked without regenerating them in the Slack App settings. Users should be informed (via `tag hooks notify add` output) that the URL is stored in their profile `.env` file and should be treated as a secret.

**S4 — Template variable allowlist** — The template engine (FR-13) only substitutes variables from a fixed allowlist. Arbitrary keys from the event payload (which may include tool output, file paths, or partial code) are NOT interpolated, preventing sensitive run data from appearing in Slack messages or email bodies.

**S5 — Delivery log content suppression** — The `notification_log` table stores outcome metadata only (hook ID, channel, status, attempt, response code, error detail). The rendered message body is intentionally not stored, preventing long-term retention of templated content that may contain run data.

**S6 — No-op on dry-run** — When `DRY_RUN=1` is set or `--dry-run` is passed, all outbound network calls and subprocess invocations from notification hooks are suppressed. This prevents accidental notification delivery during testing or CI runs.

**S7 — Auth header values** — Webhook `--header` values that begin with `$` are resolved from environment at delivery time and are never stored literally in YAML. This protects bearer tokens used in webhook auth headers.

**S8 — Error detail truncation** — Exception messages stored in `error_detail` are truncated to 200 characters. This prevents SMTP server error messages (which may echo back SMTP commands including credentials) from being stored in full.

**S9 — SMTP TLS enforcement** — `EmailNotifier` SHALL enforce STARTTLS negotiation on port 587 and SHALL NOT allow falling back to plaintext SMTP. Connection to port 465 uses `SMTP_SSL`. Connections to port 25 are not supported.

**S10 — URL masking in list output** — `tag hooks notify list` SHALL display only the environment variable name (e.g. `$SLACK_WEBHOOK_URL`) where a URL is configured, never the resolved URL value.

---

## 10. Testing Strategy

### Unit tests (`tests/test_notifications.py`)

**Slack/webhook HTTP mocking**

```python
from unittest.mock import patch, MagicMock

def test_slack_notifier_delivers_message():
    with patch("urllib.request.urlopen") as mock_open:
        mock_resp = MagicMock()
        mock_resp.status = 200
        mock_resp.__enter__ = lambda s: mock_resp
        mock_resp.__exit__ = MagicMock(return_value=False)
        mock_open.return_value = mock_resp
        notifier = SlackNotifier(webhook_url="https://hooks.slack.com/test")
        result = notifier.deliver("test message")
        assert result.success is True
        assert result.response_code == 200
        call_args = mock_open.call_args
        assert b'"text": "test message"' in call_args[0][0].data

def test_slack_notifier_retries_on_http_error():
    # Return 429 twice then 200; verify 3 calls
    ...

def test_webhook_notifier_includes_custom_headers():
    ...
```

**SMTP mocking**

```python
from unittest.mock import patch, MagicMock

def test_email_notifier_sends_with_starttls():
    with patch("smtplib.SMTP") as mock_smtp_cls:
        mock_smtp = MagicMock()
        mock_smtp_cls.return_value.__enter__ = lambda s: mock_smtp
        mock_smtp_cls.return_value.__exit__ = MagicMock(return_value=False)
        notifier = EmailNotifier(
            smtp_host="smtp.gmail.com", smtp_port=587,
            smtp_user="user@gmail.com", smtp_password="password",
            to_addr="to@example.com", from_addr="from@example.com",
        )
        result = notifier.deliver("body text", subject="TAG Test")
        mock_smtp.starttls.assert_called_once()
        mock_smtp.login.assert_called_once_with("user@gmail.com", "password")
        assert result.success is True
```

**Desktop notification mocking**

```python
from unittest.mock import patch

def test_desktop_notifier_macos():
    with patch("platform.system", return_value="Darwin"), \
         patch("subprocess.run") as mock_run:
        mock_run.return_value.returncode = 0
        notifier = DesktopNotifier()
        result = notifier.deliver("test message")
        assert result.success is True
        args = mock_run.call_args[0][0]
        assert "osascript" in args
        assert "test message" in " ".join(args)

def test_desktop_notifier_unsupported_platform():
    with patch("platform.system", return_value="Windows"):
        notifier = DesktopNotifier()
        result = notifier.deliver("test message")
        assert result.success is False
        assert "no desktop notification support" in result.error_detail
```

**Template rendering tests**

```python
def test_render_template_basic_substitution():
    payload = {"run_id": "abc123", "profile": "coder", "duration": "42",
               "cost_usd": "0.0123", "status": "completed", "tokens_used": "1500",
               "error_message": "", "task": "write tests", "event_type": "run.complete",
               "timestamp": "2026-06-12T00:00:00Z"}
    result = render_template("Run {{run_id}} on {{profile}} done in {{duration}}s", payload)
    assert result == "Run abc123 on coder done in 42s"

def test_render_template_unknown_variable_stripped():
    payload = {"run_id": "abc123"}
    result = render_template("{{run_id}} secret={{aws_secret_key}}", payload)
    assert "aws_secret_key" not in result
    assert result == "abc123 secret="

def test_render_template_missing_allowed_variable_replaced_with_empty():
    result = render_template("{{run_id}} {{error_message}}", {"run_id": "x"})
    assert result == "x "
```

**Retry backoff tests**

```python
def test_delivery_manager_retries_on_failure():
    # Mock notifier that fails twice then succeeds
    # Verify 3 deliver() calls and correct sleep intervals
    with patch("time.sleep") as mock_sleep:
        ...
        sleep_calls = [c[0][0] for c in mock_sleep.call_args_list]
        assert sleep_calls == [1, 2]  # backoff before retry 2 and 3

def test_desktop_notifier_not_retried():
    # Desktop failures should result in exactly 1 deliver() call
    ...
```

**Delivery log tests**

```python
def test_delivery_manager_writes_notification_log(tmp_path):
    db_path = tmp_path / "tag.sqlite3"
    # Set up DB, fire a notification, check notification_log table
    ...
    rows = conn.execute("SELECT status, attempt, channel FROM notification_log").fetchall()
    assert rows[0] == ("delivered", 1, "slack")

def test_delivery_log_does_not_store_message_content(tmp_path):
    # Verify no column named "message" or "body" exists in notification_log
    ...
```

**Profile filter tests**

```python
def test_notification_hook_fires_only_for_matching_profile():
    hook = NotificationHook(..., profile_filter="coder")
    # payload with profile="research" should not fire
    # payload with profile="coder" should fire
    ...
```

---

## 11. Acceptance Criteria

**AC-01** — `tag hooks notify add --event run.complete --channel slack --webhook-url "$SLACK_WEBHOOK_URL" --profile coder` adds a hook entry to the profile YAML with `type: notify`, `channel: slack`, `profile_filter: coder`, and `webhook_url_env: SLACK_WEBHOOK_URL`; the literal URL is not present in YAML.

**AC-02** — When a `run.complete` event fires for the `coder` profile, a POST request reaches the Slack webhook URL within 5 seconds; the body is `{"text": "<rendered message>"}`.

**AC-03** — When a `run.complete` event fires for a non-`coder` profile (e.g. `research`), the Slack notification hook from AC-01 does NOT fire; no POST request is made.

**AC-04** — `tag hooks notify test --channel slack --webhook-url <url>` successfully POSTs a test message and prints `Notification delivered successfully (slack)` to stdout without requiring a registered hook.

**AC-05** — `tag hooks notify test --channel email --smtp-host smtp.gmail.com --smtp-user test@gmail.com --smtp-password-env SMTP_APP_PASSWORD --to to@example.com` sends an email and prints success; if the env var is not set, it prints a clear error `SMTP_APP_PASSWORD is not set`.

**AC-06** — On macOS, `tag hooks notify test --channel desktop` displays a system notification titled "TAG" with a test message body; on Linux with `notify-send`, equivalent behavior; on unsupported platforms, it exits 0 with a warning message.

**AC-07** — `tag hooks notify list` displays all configured notification hooks in tabular form, shows the env var name (not URL) for credential fields, and shows `enabled` / `disabled` status.

**AC-08** — `tag hooks notify disable <hook-id>` sets `enabled: false`; subsequent event emissions do NOT fire the hook. `tag hooks notify enable <hook-id>` re-enables it.

**AC-09** — `tag hooks notify remove <hook-id>` removes the hook from config; `tag hooks notify list` no longer shows it; delivery log entries are retained.

**AC-10** — When the Slack API returns a non-2xx status on the first attempt, the delivery is retried up to 3 times; all 3 attempts and their outcomes are visible in `tag hooks notify log`.

**AC-11** — The `notification_log` table contains no column that stores rendered message content; `SELECT * FROM notification_log` returns only metadata fields.

**AC-12** — With `DRY_RUN=1` set, `tag submit "test task"` completes normally and prints `[dry-run] would deliver slack notification for run.complete` to stderr; no HTTP request is made.

**AC-13** — A `budget.warning` notification hook with `channel: desktop` fires when the cost threshold is crossed, completing synchronously (best-effort) without blocking `tag submit` or the budget enforcement path.

**AC-14** — Notification log entries older than 90 days are deleted on the next `open_db()` call; `tag hooks notify log` only returns entries within the retention window.

**AC-15** — Running `tag hooks notify add --event run.failed --channel email --smtp-password LITERAL_PASSWORD` (literal password via wrong flag) is rejected with an error: `use --smtp-password-env to specify an environment variable name for the SMTP password`.

---

## 12. Dependencies

| Dependency | Type | Justification |
|------------|------|---------------|
| `smtplib` | Python stdlib | SMTP email delivery; no new package required |
| `email.mime.text`, `email.mime.multipart` | Python stdlib | Build MIME email messages |
| `urllib.request` | Python stdlib | HTTP POST for Slack and generic webhook; already used in `controller.py` |
| `threading` | Python stdlib | Async/non-blocking delivery |
| `sqlite3` | Python stdlib | `notification_log` table; already used throughout |
| `subprocess` | Python stdlib | Desktop notification via `osascript`/`notify-send`; already used |
| `platform`, `shutil` | Python stdlib | Platform detection and binary existence check for desktop channel |
| `requests` / `httpx` | Optional runtime dependency | If already installed (e.g. as a transitive dep), MAY be used for connection pooling and cleaner timeout handling; NOT a required new dependency |

**No new required dependencies are added.** Jinja2 is explicitly excluded; the custom `render_template()` function covers the templating requirement with stdlib `str.replace()` and `re.sub()`.

---

## 13. Open Questions

**OQ-1 — Notification batching for high-frequency loops**

An autonomous loop running 100 turns fires 100 `run.complete` events and, without batching, sends 100 Slack messages. Possible mitigations: (a) a `--batch-window 60s` option that collects events over a time window and sends one summary notification; (b) a `--max-per-hour 5` rate limit per hook; (c) a special `loop.complete` event that fires only once at the end of a loop rather than per turn. The cleanest solution is (c): emit `loop.complete` from the loop controller with an aggregate payload, and recommend users hook on `loop.complete` rather than `run.complete`. This is left open pending the loop controller design (PRD-021).

**OQ-2 — PagerDuty and OpsGenie support**

Both services expose HTTP endpoints compatible with the `webhook` channel. A future `channel: pagerduty` could wrap the Events API v2 with severity mapping. Not in scope for this sprint; the generic webhook channel is sufficient as a workaround.

**OQ-3 — Two-way Slack approval**

A powerful future capability: TAG pauses a loop turn, sends a Slack message with "Approve / Reject" buttons, and waits for a reply before continuing. This requires a Slack App with Interactive Components (not just an incoming webhook), an HTTP endpoint reachable from Slack's servers, and a blocking wait in the loop controller. This is architecturally significant and is tracked as a separate PRD concept. It is explicitly out of scope here (Section 3).

**OQ-4 — Credential storage location**

Currently spec'd as profile `.env` files. An alternative is a system keychain (`keyring` library). The `.env` approach requires no new dependency and is consistent with how Hermes handles API keys, but exposes credentials as plaintext files on disk. If the TAG user base includes shared systems or multi-user deployments, keychain storage should be revisited.

**OQ-5 — Notification hook IDs**

The CLI uses hook names (from `--name`) as identifiers for `remove`/`disable`/`enable`. If the user does not provide `--name`, a UUID is auto-generated. Names must be unique within a profile config. Collision handling (error vs. auto-suffix) is an implementation detail left to the implementer.

---

## 14. Complexity and Timeline

**Complexity:** M

**Estimate:** 1 sprint (~5 developer-days)

| Day | Task |
|-----|------|
| 1 | Create `src/tag/notifications.py`: `BaseNotifier`, `SlackNotifier`, `WebhookNotifier`, `render_template`, `_ALLOWED_VARS` |
| 2 | `EmailNotifier` (smtplib + STARTTLS + SMTP_SSL), `DesktopNotifier` (osascript + notify-send), `DeliveryResult` dataclass |
| 3 | `DeliveryManager` with retry loop and `notification_log` writes; `open_db()` migration for `notification_log` table; integrate into `_fire_hooks()` |
| 4 | `cmd_hooks_notify` subcommand tree in `controller.py`: `add`, `list`, `test`, `remove`, `disable`, `enable`, `log`; YAML read/write helpers; credential env var auto-promotion |
| 5 | Unit tests (all channels, template engine, retry, profile filter, dry-run, delivery log), integration smoke test, update INDEX.md |

**Risk:** Low. All delivery is async; failure modes are handled and logged. The only novel surface is SMTP, which is exercised with smtplib mocks in tests.
