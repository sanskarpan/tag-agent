# PRD-050: Alert Rules on Metric Thresholds (`tag alert`)

**Status:** Proposed
**Priority:** P2
**Estimated Effort:** M (5-8 days)
**Category:** Evaluation & Observability
**Affects:** `alerts.py + notifications.py + controller.py`
**Depends on:** PRD-041 (OTel GenAI span cost attribution — metric emission), PRD-027 (eval framework — eval_runs table), PRD-044 (AgentOps session observability — sessions table), PRD-040 (notification hooks — delivery infrastructure)
**Inspired by:** Grafana alerting, PagerDuty alert rules, Datadog monitors, LangSmith alert conditions

---

## 1. Overview

TAG emits detailed per-span cost, latency, and token metrics via its OTel-compatible tracing infrastructure (PRD-041, PRD-044). Yet there is no mechanism to notify engineers when these metrics breach operational thresholds — a runaway agent spending $50 in a single run, an eval score dropping below 0.7, or P95 latency exceeding 30 seconds goes undetected until someone manually queries the database. This forces teams to instrument their own monitoring or accept that regressions will be discovered only after damage is done.

Alert Rules on Metric Thresholds (`tag alert`) introduces a first-class alerting layer on top of TAG's existing metrics infrastructure. Engineers define alert rules that watch specific metric fields — `run.cost_usd`, `eval_run.score`, `step.duration_ms`, `session.token_count` — and fire notification actions when threshold conditions are met. Rules are stored in SQLite, evaluated by a lightweight polling daemon, and deliver notifications through the PRD-040 notification hook system (Slack webhook, email, desktop notification). The alert system is entirely local-first, requiring no cloud dependencies.

The design is inspired by Grafana's alert rule model (condition expression, evaluation interval, pending period before firing, re-fire suppression), Datadog monitors (multi-window aggregation, anomaly detection mode), and LangSmith's alert conditions (eval score thresholds on datasets). TAG's implementation focuses on simplicity: threshold comparisons over SQL aggregation windows (last N runs, rolling 1h/24h), with an optional LLM-judge mode that uses a judgment model to classify whether a run's output constitutes an alert condition.

---

## 2. Problem Statement

### 2.1 No automated cost anomaly detection

TAG tracks per-run and per-span USD cost (PRD-041), but teams must manually query `tag runs cost --since 1h` to spot overruns. A single misbehaving agent loop can accumulate hundreds of dollars of API spend before anyone notices. There is no way to receive a Slack message when a run exceeds a cost threshold or when rolling hourly spend exceeds budget.

### 2.2 Eval score regressions discovered too late

PRD-027 eval runs generate scores, and PRD-047 gates CI PRs on score thresholds. But outside of CI, there is no continuous monitoring of eval scores. A score that drifts from 0.85 to 0.60 over two weeks is not detected until the next manual eval run review.

### 2.3 Latency and reliability blindspot

Step durations, tool call latencies, and retry counts are stored in spans but there are no operational alerts. If the embedding model starts returning responses 10× slower due to an upstream change, TAG silently continues at degraded performance.

---

## 3. Goals

| ID | Goal |
|----|------|
| G1 | Define named alert rules with a metric source (SQL expression over `runs`/`steps`/`eval_runs`/`sessions` tables), comparison operator, threshold value, and evaluation window. |
| G2 | Evaluate rules on a configurable interval (default 60s) via a polling daemon; respect `pending_periods` before firing to suppress transient spikes. |
| G3 | Fire notifications through PRD-040 hooks (Slack, email, desktop) with a templated message including metric value, threshold, and a direct link to the offending run. |
| G4 | Support re-fire suppression (`resolve_after`) so a firing alert does not spam on every evaluation cycle. |
| G5 | Provide `tag alert create`, `tag alert list`, `tag alert show`, `tag alert delete`, `tag alert test`, `tag alert history` subcommands. |
| G6 | Persist alert rule state (last evaluation, current state — OK/PENDING/FIRING/RESOLVED) in SQLite for auditability. |
| G7 | Support a simple LLM-judge alert mode where the condition is a natural-language description evaluated against the latest run's output by a cheap classifier model. |

## 3.1 Non-Goals

| ID | Non-Goal |
|----|----------|
| NG1 | Distributed/cloud alerting or PagerDuty/OpsGenie integration. All delivery is through PRD-040 local hooks. |
| NG2 | Anomaly detection, forecasting, or ML-based threshold learning. Only static threshold comparisons in this PRD. |
| NG3 | Multi-dimensional alert correlations or composite conditions (A AND B). Single metric per rule only. |
| NG4 | Real-time streaming evaluation. Rules are polled, not event-driven. |
| NG5 | Alert silencing windows / maintenance modes. |

---

## 4. Success Metrics

| Metric | Target | Measurement |
|--------|--------|-------------|
| Rule evaluation latency | Rule evaluated and notification dispatched in < 2s after polling interval fires | Integration test with mock notifier |
| False-positive suppression | Zero duplicate notifications sent within `resolve_after` window | Unit test |
| SQLite write overhead | Alert daemon adds < 5ms overhead per evaluation cycle on 10-rule ruleset | Benchmark test |
| CLI usability | Engineer creates a cost alert with `tag alert create --metric run.cost_usd --gt 5.0 --window 1h` in < 30s | Manual test |
| LLM-judge alert accuracy | Judge mode achieves > 80% precision on curated 20-case test set | Eval test |

---

## 5. User Stories

| ID | As a... | I want to... | So that... |
|----|---------|-------------|------------|
| US1 | Platform engineer | Receive a Slack alert when any run costs more than $5 | I catch runaway agent loops before they drain budget |
| US2 | ML engineer | Be notified when my eval suite's average score drops below 0.70 | I detect model regressions before they reach production |
| US3 | On-call engineer | Get a desktop notification when P95 step latency exceeds 30s | I can investigate upstream API slowdowns immediately |
| US4 | QA engineer | Define an LLM-judge alert that fires when a run output contains PII | I enforce data-handling policies automatically |
| US5 | Team lead | See the history of all fired alerts for the past 7 days | I understand system reliability trends |

---

## 6. CLI Surface

```
tag alert <subcommand> [options]

Subcommands:
  create     Define a new alert rule
  list       List all alert rules and their current state
  show       Show full details of a rule including fire history
  delete     Remove a rule
  test       Evaluate a rule once immediately against current data
  history    Show alert fire/resolve events over a time window
  daemon     Run the alert evaluation daemon in the foreground

tag alert create \
  --name "high-cost-run" \
  --metric "run.cost_usd" \
  --op gt \
  --threshold 5.0 \
  --window 1h \
  --profile default \
  --notify slack \
  --message "Run {{run_id}} cost ${{value}} (threshold: ${{threshold}})" \
  [--pending-periods 2] \
  [--resolve-after 300]

tag alert create \
  --name "eval-score-drop" \
  --metric "eval_run.score" \
  --op lt \
  --threshold 0.70 \
  --window 24h \
  --notify slack,email

tag alert create \
  --name "pii-in-output" \
  --mode llm-judge \
  --judge-prompt "Does this output contain any PII (names, emails, phone numbers)?" \
  --judge-model claude-haiku-4-5 \
  --profile default \
  --notify slack

tag alert list [--state firing|ok|pending] [--profile PROFILE]
tag alert show <name>
tag alert delete <name>
tag alert test <name>
tag alert history [--since 7d] [--name NAME]
tag alert daemon [--interval 60] [--db PATH] [--config PATH]

Options:
  --name NAME               Unique rule name
  --metric FIELD            Dot-notation metric: run.cost_usd, eval_run.score,
                            step.duration_ms, session.token_count
  --op lt|gt|lte|gte|eq     Comparison operator
  --threshold FLOAT         Numeric threshold value
  --window DURATION         Aggregation window: 1h, 24h, 7d, or last:N
  --profile PROFILE         Scope rule to a specific TAG profile
  --notify CHANNELS         Comma-separated: slack,email,desktop
  --message TEMPLATE        Go-style template with {{run_id}}, {{value}}, {{threshold}}
  --pending-periods N       Require N consecutive threshold breaches before firing (default: 1)
  --resolve-after SECONDS   Suppress re-fire for N seconds after firing (default: 300)
  --mode threshold|llm-judge  Alert evaluation mode (default: threshold)
  --judge-prompt TEXT       Natural language condition for llm-judge mode
  --judge-model MODEL       Model for llm-judge evaluation (default: claude-haiku-4-5)
  --enabled / --disabled    Enable or disable the rule
```

---

## 7. Functional Requirements

| ID | Requirement |
|----|------------|
| FR-01 | `tag alert create` writes a rule record to `alert_rules` SQLite table with all specified parameters; validation rejects unknown metric fields. |
| FR-02 | Alert daemon polls at the configured interval, evaluating each enabled rule's SQL expression against the current DB state. |
| FR-03 | For `threshold` mode: compute the aggregated metric value for the window, compare against threshold using the specified operator, transition state machine (OK → PENDING → FIRING → RESOLVED). |
| FR-04 | For `llm-judge` mode: fetch the most recent run output for the profile, call the judge model with the judge-prompt, parse yes/no response to determine firing. |
| FR-05 | When a rule transitions to FIRING: dispatch notification through all configured PRD-040 channels with the templated message. |
| FR-06 | Re-fire suppression: do not dispatch a second notification within `resolve_after` seconds of the first FIRING event. |
| FR-07 | `pending_periods` logic: only transition to FIRING after N consecutive evaluation cycles show the threshold breached; reset counter on any OK evaluation. |
| FR-08 | `tag alert history` queries the `alert_events` table and renders a table of (timestamp, rule, state, value, message). |
| FR-09 | `tag alert test` evaluates the rule's SQL expression once against current DB and prints the computed value, whether it would fire, and the notification payload — without writing events or sending notifications. |
| FR-10 | `tag alert daemon` runs an infinite poll loop; supports graceful shutdown via SIGTERM; logs evaluation results at DEBUG level. |

---

## 8. Non-Functional Requirements

| ID | Requirement |
|----|------------|
| NFR-01 | Daemon must not block the main TAG process; runs as a subprocess or systemd unit. |
| NFR-02 | SQLite writes from the daemon use WAL mode with `busy_timeout = 5000ms` to avoid blocking concurrent tag commands. |
| NFR-03 | LLM-judge mode must cache the last judge response for `resolve_after` seconds to avoid repeated model calls on every polling cycle. |
| NFR-04 | All SQLite queries in rule evaluation must complete in < 100ms on a 1M-row `runs` table (indexed on `profile`, `created_at`, `cost_usd`). |
| NFR-05 | No network calls from the daemon except notification delivery and llm-judge API calls; rule evaluation is purely local SQL. |

---

## 9. Technical Design

### 9.1 Target files

| File | Change |
|------|--------|
| `src/tag/alerts.py` | New module: `AlertRule`, `AlertState`, `AlertDaemon`, SQL evaluation logic |
| `src/tag/notifications.py` | New module: notification dispatchers (Slack webhook, SMTP, desktop), wrapping PRD-040 hooks |
| `src/tag/controller.py` | Add `cmd_alert` entrypoint; register `alert` subparser with all subcommands |

### 9.2 SQLite DDL

```sql
CREATE TABLE IF NOT EXISTS alert_rules (
  id            TEXT PRIMARY KEY,
  name          TEXT NOT NULL UNIQUE,
  profile       TEXT,
  mode          TEXT NOT NULL DEFAULT 'threshold',  -- 'threshold' | 'llm-judge'
  metric        TEXT,                               -- e.g. 'run.cost_usd'
  op            TEXT,                               -- 'lt'|'gt'|'lte'|'gte'|'eq'
  threshold     REAL,
  window        TEXT NOT NULL DEFAULT '1h',
  notify        TEXT NOT NULL DEFAULT 'desktop',    -- comma-separated channels
  message_tmpl  TEXT,
  pending_periods INTEGER NOT NULL DEFAULT 1,
  resolve_after INTEGER NOT NULL DEFAULT 300,
  judge_prompt  TEXT,
  judge_model   TEXT,
  enabled       INTEGER NOT NULL DEFAULT 1,
  state         TEXT NOT NULL DEFAULT 'ok',         -- 'ok'|'pending'|'firing'|'resolved'
  pending_count INTEGER NOT NULL DEFAULT 0,
  last_value    REAL,
  last_eval_at  TEXT,
  last_fired_at TEXT,
  created_at    TEXT NOT NULL,
  updated_at    TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS alert_events (
  id          TEXT PRIMARY KEY,
  rule_id     TEXT NOT NULL REFERENCES alert_rules(id),
  rule_name   TEXT NOT NULL,
  event_type  TEXT NOT NULL,  -- 'firing'|'resolved'|'pending'|'ok'
  value       REAL,
  threshold   REAL,
  message     TEXT,
  run_id      TEXT,
  created_at  TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_alert_events_rule_time
  ON alert_events(rule_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_runs_profile_cost
  ON runs(profile, created_at DESC, cost_usd);
```

### 9.3 Python core — AlertDaemon

```python
from __future__ import annotations
import dataclasses
import re
import sqlite3
import time
from datetime import datetime, timezone, timedelta
from typing import Optional

METRIC_SQL = {
    "run.cost_usd": "SELECT SUM(cost_usd) FROM runs WHERE {where}",
    "run.duration_ms": "SELECT AVG(duration_ms) FROM runs WHERE {where}",
    "eval_run.score": "SELECT AVG(score) FROM eval_runs WHERE {where}",
    "step.duration_ms": "SELECT AVG(duration_ms) FROM steps WHERE {where}",
    "session.token_count": "SELECT SUM(total_tokens) FROM sessions WHERE {where}",
}

WINDOW_SECONDS = {
    "1h": 3600, "24h": 86400, "7d": 604800, "30d": 2592000,
}

@dataclasses.dataclass
class AlertRule:
    id: str
    name: str
    profile: Optional[str]
    mode: str
    metric: Optional[str]
    op: Optional[str]
    threshold: Optional[float]
    window: str
    notify: str
    message_tmpl: Optional[str]
    pending_periods: int
    resolve_after: int
    judge_prompt: Optional[str]
    judge_model: Optional[str]
    enabled: bool
    state: str
    pending_count: int
    last_value: Optional[float]
    last_fired_at: Optional[str]

class AlertDaemon:
    def __init__(self, db_path: str, interval: int = 60) -> None:
        self.db_path = db_path
        self.interval = interval

    def _conn(self) -> sqlite3.Connection:
        conn = sqlite3.connect(self.db_path, timeout=10)
        conn.row_factory = sqlite3.Row
        conn.execute("PRAGMA journal_mode=WAL")
        conn.execute("PRAGMA busy_timeout=5000")
        return conn

    def _eval_threshold(self, conn: sqlite3.Connection, rule: AlertRule) -> Optional[float]:
        sql_tmpl = METRIC_SQL.get(rule.metric or "")
        if not sql_tmpl:
            return None
        window_s = WINDOW_SECONDS.get(rule.window, 3600)
        cutoff = (datetime.now(timezone.utc) - timedelta(seconds=window_s)).isoformat()
        where_parts = [f"created_at >= '{cutoff}'"]
        if rule.profile:
            where_parts.append(f"profile = '{rule.profile}'")
        where = " AND ".join(where_parts)
        sql = sql_tmpl.format(where=where)
        row = conn.execute(sql).fetchone()
        return float(row[0]) if row and row[0] is not None else 0.0

    def _check_threshold(self, value: float, op: str, threshold: float) -> bool:
        return {
            "gt": value > threshold, "lt": value < threshold,
            "gte": value >= threshold, "lte": value <= threshold,
            "eq": abs(value - threshold) < 1e-9,
        }.get(op, False)

    def _render_message(self, tmpl: str, run_id: str, value: float, threshold: float) -> str:
        return tmpl.replace("{{run_id}}", run_id or "").replace(
            "{{value}}", f"{value:.4f}").replace("{{threshold}}", f"{threshold:.4f}")

    def run_once(self, conn: sqlite3.Connection) -> None:
        now = datetime.now(timezone.utc).isoformat()
        rules = conn.execute(
            "SELECT * FROM alert_rules WHERE enabled=1"
        ).fetchall()
        for row in rules:
            rule = AlertRule(**{k: row[k] for k in row.keys()})
            try:
                value = self._eval_threshold(conn, rule)
                breaching = value is not None and self._check_threshold(
                    value, rule.op or "gt", rule.threshold or 0.0
                )
                new_state, new_pending = rule.state, rule.pending_count
                event_type = None
                if breaching:
                    if rule.state in ("ok", "resolved"):
                        new_pending = 1
                        new_state = "pending"
                    elif rule.state == "pending":
                        new_pending += 1
                        if new_pending >= rule.pending_periods:
                            new_state = "firing"
                            event_type = "firing"
                    # Already firing — check re-fire suppression
                    elif rule.state == "firing":
                        last_fired = rule.last_fired_at
                        if last_fired:
                            elapsed = (datetime.now(timezone.utc) -
                                       datetime.fromisoformat(last_fired)).total_seconds()
                            if elapsed < rule.resolve_after:
                                pass  # suppress
                            else:
                                event_type = "firing"
                else:
                    if rule.state != "ok":
                        new_state = "ok"
                        new_pending = 0
                        event_type = "resolved"

                update_fields: dict = {
                    "state": new_state, "pending_count": new_pending,
                    "last_value": value, "last_eval_at": now, "updated_at": now,
                }
                if event_type == "firing":
                    update_fields["last_fired_at"] = now
                conn.execute(
                    "UPDATE alert_rules SET state=:state, pending_count=:pending_count, "
                    "last_value=:last_value, last_eval_at=:last_eval_at, "
                    "last_fired_at=COALESCE(:last_fired_at, last_fired_at), updated_at=:updated_at "
                    "WHERE id=:id",
                    {**update_fields, "last_fired_at": update_fields.get("last_fired_at"), "id": rule.id}
                )
                if event_type:
                    import uuid
                    conn.execute(
                        "INSERT INTO alert_events(id,rule_id,rule_name,event_type,value,threshold,created_at) "
                        "VALUES(?,?,?,?,?,?,?)",
                        (uuid.uuid4().hex[:8], rule.id, rule.name, event_type,
                         value, rule.threshold, now)
                    )
                conn.commit()
                if event_type == "firing":
                    self._dispatch(rule, value)
            except Exception:
                pass

    def _dispatch(self, rule: AlertRule, value: float) -> None:
        from tag.notifications import dispatch_notification
        msg = self._render_message(
            rule.message_tmpl or f"Alert '{rule.name}' firing: {{{{value}}}} {rule.op} {{{{threshold}}}}",
            "", value, rule.threshold or 0.0
        )
        for channel in rule.notify.split(","):
            dispatch_notification(channel.strip(), rule.name, msg)

    def run(self) -> None:
        conn = self._conn()
        while True:
            self.run_once(conn)
            time.sleep(self.interval)
```

---

## 10. Security Considerations

| Risk | Mitigation |
|------|-----------|
| SQL injection via metric field names | Validate `metric` field against allowlist `METRIC_SQL.keys()` before constructing query |
| Notification webhook credential exposure | Webhook URLs stored in `~/.tag/config.yaml` with file mode 0600; not logged |
| LLM-judge prompt injection via run output | Truncate run output to 2000 chars; wrap in system-prompt boundary markers |
| Alert rule name containing shell metacharacters | Validate name matches `[a-z0-9_-]{1,64}` at creation time |

---

## 11. Testing Strategy

| Layer | Tests |
|-------|-------|
| Unit | `cron_matches`-style evaluation of threshold expressions; state machine transitions (OK→PENDING→FIRING→RESOLVED) |
| Integration | End-to-end: seed `runs` table, create alert rule, run daemon once, assert `alert_events` row and notification dispatch |
| Security | Attempt SQL injection via metric name; assert rejection |
| Performance | Benchmark 10-rule evaluation on 1M-row `runs` table; assert < 500ms total |

---

## 12. Acceptance Criteria

| ID | Criterion |
|----|----------|
| AC-01 | `tag alert create --name cost-watch --metric run.cost_usd --op gt --threshold 5.0 --window 1h --notify desktop` succeeds and persists rule to SQLite |
| AC-02 | After seeding the `runs` table with a $6.00 run, `tag alert test cost-watch` prints "WOULD FIRE: value=6.0 > threshold=5.0" |
| AC-03 | Running the daemon with interval=1s fires a notification within 2 seconds of a threshold breach |
| AC-04 | A second notification is NOT sent within `resolve_after` seconds |
| AC-05 | `tag alert history --since 1h` lists the fired event with timestamp, value, and rule name |
| AC-06 | `tag alert delete cost-watch` removes the rule and all events |

---

## 13. Dependencies

| Dependency | Reason |
|-----------|--------|
| PRD-040 notification hooks | Delivery channels for alert notifications |
| PRD-041 OTel cost attribution | `run.cost_usd` metric source in `runs` table |
| PRD-027 eval framework | `eval_run.score` metric source in `eval_runs` table |

---

## 14. Open Questions

| ID | Question |
|----|---------|
| OQ-01 | Should alert rules be profile-scoped or global? Current design supports both (NULL profile = global). |
| OQ-02 | Should there be a rate-limit on LLM-judge evaluations to prevent unexpected API costs? |
| OQ-03 | Should the daemon be a systemd/launchd unit or a subprocess of the main TAG process? |

---

## 15. Complexity & Timeline

**Complexity:** Medium (M)
**Estimated effort:** 5–8 engineer-days

| Phase | Work | Days |
|-------|------|------|
| 1 | SQLite DDL, `AlertRule` dataclass, state machine unit tests | 2 |
| 2 | `AlertDaemon` SQL evaluation, pending/resolve logic | 2 |
| 3 | Notification dispatchers (Slack, email, desktop) | 1 |
| 4 | CLI commands (`create`, `list`, `show`, `delete`, `test`, `history`, `daemon`) | 2 |
| 5 | LLM-judge mode, integration tests | 1 |
