"""PRD-050: Alert rules on metric thresholds.

Configurable alert rules that fire when metrics cross thresholds. Rules are
persisted in SQLite. Firings are recorded and can be queried. A snapshot
helper queries live metric values from sibling tables.
"""
from __future__ import annotations

import sqlite3
import uuid
from dataclasses import dataclass, field
from datetime import datetime, timezone
from enum import Enum
from typing import Optional


# ---------------------------------------------------------------------------
# Enums
# ---------------------------------------------------------------------------

class AlertMetric(str, Enum):
    EVAL_PASS_RATE = "eval_pass_rate"
    EVAL_SCORE = "eval_score"
    SPAN_ERROR_RATE = "span_error_rate"
    P95_LATENCY_MS = "p95_latency_ms"
    COST_USD_PER_RUN = "cost_usd_per_run"
    CACHE_HIT_RATE = "cache_hit_rate"
    MEMORY_COUNT = "memory_count"


class AlertSeverity(str, Enum):
    INFO = "info"
    WARNING = "warning"
    CRITICAL = "critical"


# ---------------------------------------------------------------------------
# Dataclasses
# ---------------------------------------------------------------------------

@dataclass
class AlertRule:
    id: str
    name: str
    metric: str                      # AlertMetric value
    condition: str                   # "lt" | "gt" | "lte" | "gte"
    threshold: float
    severity: str                    # AlertSeverity value
    profile: Optional[str]
    suite: Optional[str]
    enabled: bool
    notify_channels: list[str]       # e.g. ["slack", "desktop"]
    created_at: str
    last_triggered_at: Optional[str] = None


@dataclass
class AlertFiring:
    id: str
    rule_id: str
    rule_name: str
    metric: str
    actual_value: float
    threshold: float
    severity: str
    fired_at: str
    message: str
    resolved_at: Optional[str] = None


# ---------------------------------------------------------------------------
# Internal helpers
# ---------------------------------------------------------------------------

def _utc_now() -> str:
    return datetime.now(timezone.utc).isoformat()


def _new_id() -> str:
    return uuid.uuid4().hex


def _channels_to_str(channels: list[str]) -> str:
    return ",".join(channels)


def _channels_from_str(raw: str) -> list[str]:
    if not raw:
        return []
    return [c for c in raw.split(",") if c]


def _row_to_rule(row: tuple) -> AlertRule:
    (
        rule_id, name, metric, condition, threshold, severity,
        profile, suite, enabled, notify_channels, created_at, last_triggered_at,
    ) = row
    return AlertRule(
        id=rule_id,
        name=name,
        metric=metric,
        condition=condition,
        threshold=float(threshold),
        severity=severity,
        profile=profile,
        suite=suite,
        enabled=bool(enabled),
        notify_channels=_channels_from_str(notify_channels or ""),
        created_at=created_at,
        last_triggered_at=last_triggered_at,
    )


def _row_to_firing(row: tuple) -> AlertFiring:
    (
        firing_id, rule_id, rule_name, metric, actual_value,
        threshold, severity, fired_at, resolved_at, message,
    ) = row
    return AlertFiring(
        id=firing_id,
        rule_id=rule_id,
        rule_name=rule_name,
        metric=metric,
        actual_value=float(actual_value),
        threshold=float(threshold),
        severity=severity,
        fired_at=fired_at,
        resolved_at=resolved_at,
        message=message,
    )


# ---------------------------------------------------------------------------
# Schema
# ---------------------------------------------------------------------------

def ensure_schema(conn: sqlite3.Connection) -> None:
    """Create alert_rules and alert_firings tables if they do not exist."""
    conn.executescript("""
        CREATE TABLE IF NOT EXISTS alert_rules (
            id                  TEXT PRIMARY KEY,
            name                TEXT NOT NULL,
            metric              TEXT NOT NULL,
            condition           TEXT NOT NULL,
            threshold           REAL NOT NULL,
            severity            TEXT NOT NULL,
            profile             TEXT,
            suite               TEXT,
            enabled             INTEGER NOT NULL DEFAULT 1,
            notify_channels     TEXT NOT NULL DEFAULT '',
            created_at          TEXT NOT NULL,
            last_triggered_at   TEXT
        );

        CREATE TABLE IF NOT EXISTS alert_firings (
            id              TEXT PRIMARY KEY,
            rule_id         TEXT NOT NULL,
            rule_name       TEXT NOT NULL,
            metric          TEXT NOT NULL,
            actual_value    REAL NOT NULL,
            threshold       REAL NOT NULL,
            severity        TEXT NOT NULL,
            fired_at        TEXT NOT NULL,
            resolved_at     TEXT,
            message         TEXT NOT NULL,
            FOREIGN KEY(rule_id) REFERENCES alert_rules(id)
        );

        CREATE INDEX IF NOT EXISTS idx_alert_firings_rule_fired
            ON alert_firings(rule_id, fired_at);
    """)
    conn.commit()


# ---------------------------------------------------------------------------
# Rule CRUD
# ---------------------------------------------------------------------------

def create_rule(
    conn: sqlite3.Connection,
    name: str,
    metric: str,
    condition: str,
    threshold: float,
    severity: str,
    *,
    profile: Optional[str] = None,
    suite: Optional[str] = None,
    notify_channels: Optional[list[str]] = None,
) -> AlertRule:
    """Persist a new AlertRule and return it."""
    if metric not in [m.value for m in AlertMetric]:
        raise ValueError(f"Unknown metric: {metric!r}")
    if condition not in ("lt", "gt", "lte", "gte"):
        raise ValueError(f"Unknown condition: {condition!r}; must be lt/gt/lte/gte")
    if severity not in [s.value for s in AlertSeverity]:
        raise ValueError(f"Unknown severity: {severity!r}")

    rule_id = _new_id()
    created_at = _utc_now()
    channels = notify_channels or []

    conn.execute(
        """
        INSERT INTO alert_rules
            (id, name, metric, condition, threshold, severity,
             profile, suite, enabled, notify_channels, created_at, last_triggered_at)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?, NULL)
        """,
        (
            rule_id, name, metric, condition, threshold, severity,
            profile, suite, _channels_to_str(channels), created_at,
        ),
    )
    conn.commit()

    return AlertRule(
        id=rule_id,
        name=name,
        metric=metric,
        condition=condition,
        threshold=threshold,
        severity=severity,
        profile=profile,
        suite=suite,
        enabled=True,
        notify_channels=channels,
        created_at=created_at,
        last_triggered_at=None,
    )


def list_rules(
    conn: sqlite3.Connection,
    *,
    enabled_only: bool = True,
) -> list[AlertRule]:
    """Return all (or only enabled) alert rules ordered by created_at."""
    if enabled_only:
        cur = conn.execute(
            "SELECT id, name, metric, condition, threshold, severity, "
            "profile, suite, enabled, notify_channels, created_at, last_triggered_at "
            "FROM alert_rules WHERE enabled = 1 ORDER BY created_at",
        )
    else:
        cur = conn.execute(
            "SELECT id, name, metric, condition, threshold, severity, "
            "profile, suite, enabled, notify_channels, created_at, last_triggered_at "
            "FROM alert_rules ORDER BY created_at",
        )
    return [_row_to_rule(row) for row in cur.fetchall()]


def delete_rule(conn: sqlite3.Connection, rule_id: str) -> bool:
    """Delete an alert rule by id. Returns True if a row was deleted."""
    cur = conn.execute("DELETE FROM alert_rules WHERE id = ?", (rule_id,))
    conn.commit()
    return cur.rowcount > 0


# ---------------------------------------------------------------------------
# Evaluation
# ---------------------------------------------------------------------------

_CONDITION_FNS = {
    "lt":  lambda actual, thresh: actual < thresh,
    "gt":  lambda actual, thresh: actual > thresh,
    "lte": lambda actual, thresh: actual <= thresh,
    "gte": lambda actual, thresh: actual >= thresh,
}


def evaluate_rule(rule: AlertRule, actual_value: float) -> bool:
    """Return True if *actual_value* satisfies the rule's condition vs its threshold."""
    fn = _CONDITION_FNS.get(rule.condition)
    if fn is None:
        return False
    return fn(actual_value, rule.threshold)


def _build_message(rule: AlertRule, actual_value: float) -> str:
    cond_labels = {
        "lt":  "<",
        "gt":  ">",
        "lte": "<=",
        "gte": ">=",
    }
    op = cond_labels.get(rule.condition, rule.condition)
    return (
        f"[{rule.severity.upper()}] {rule.name}: "
        f"{rule.metric} = {actual_value:.4g} {op} {rule.threshold:.4g}"
    )


def check_alerts(
    conn: sqlite3.Connection,
    metrics: dict[str, float],
) -> list[AlertFiring]:
    """
    Evaluate all enabled rules against *metrics*.

    For each rule that fires, an AlertFiring is persisted to alert_firings and
    the rule's last_triggered_at is updated.  Returns the list of new firings.
    """
    rules = list_rules(conn, enabled_only=True)
    fired: list[AlertFiring] = []
    now = _utc_now()

    for rule in rules:
        if rule.metric not in metrics:
            continue
        actual = metrics[rule.metric]
        if not evaluate_rule(rule, actual):
            continue

        firing_id = _new_id()
        message = _build_message(rule, actual)

        conn.execute(
            """
            INSERT INTO alert_firings
                (id, rule_id, rule_name, metric, actual_value,
                 threshold, severity, fired_at, resolved_at, message)
            VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULL, ?)
            """,
            (
                firing_id, rule.id, rule.name, rule.metric,
                actual, rule.threshold, rule.severity, now, message,
            ),
        )
        conn.execute(
            "UPDATE alert_rules SET last_triggered_at = ? WHERE id = ?",
            (now, rule.id),
        )

        firing = AlertFiring(
            id=firing_id,
            rule_id=rule.id,
            rule_name=rule.name,
            metric=rule.metric,
            actual_value=actual,
            threshold=rule.threshold,
            severity=rule.severity,
            fired_at=now,
            resolved_at=None,
            message=message,
        )
        fired.append(firing)

    if fired:
        conn.commit()

    return fired


# ---------------------------------------------------------------------------
# Query firings
# ---------------------------------------------------------------------------

def get_recent_firings(
    conn: sqlite3.Connection,
    *,
    limit: int = 50,
    severity: Optional[str] = None,
) -> list[AlertFiring]:
    """Return recent alert firings, newest first, optionally filtered by severity."""
    if severity is not None:
        cur = conn.execute(
            "SELECT id, rule_id, rule_name, metric, actual_value, "
            "threshold, severity, fired_at, resolved_at, message "
            "FROM alert_firings WHERE severity = ? "
            "ORDER BY fired_at DESC LIMIT ?",
            (severity, limit),
        )
    else:
        cur = conn.execute(
            "SELECT id, rule_id, rule_name, metric, actual_value, "
            "threshold, severity, fired_at, resolved_at, message "
            "FROM alert_firings ORDER BY fired_at DESC LIMIT ?",
            (limit,),
        )
    return [_row_to_firing(row) for row in cur.fetchall()]


# ---------------------------------------------------------------------------
# Formatting
# ---------------------------------------------------------------------------

def format_alert_table(firings: list[AlertFiring]) -> str:
    """Return a plain-text table of alert firings."""
    if not firings:
        return "No alert firings."

    headers = ["Severity", "Metric", "Actual", "Threshold", "Rule", "Fired At"]
    rows: list[list[str]] = []
    for f in firings:
        rows.append([
            f.severity.upper(),
            f.metric,
            f"{f.actual_value:.4g}",
            f"{f.threshold:.4g}",
            f.rule_name,
            f.fired_at[:19].replace("T", " "),
        ])

    col_widths = [len(h) for h in headers]
    for row in rows:
        for i, cell in enumerate(row):
            col_widths[i] = max(col_widths[i], len(cell))

    def fmt_row(cells: list[str]) -> str:
        return "  ".join(c.ljust(col_widths[i]) for i, c in enumerate(cells))

    sep = "  ".join("-" * w for w in col_widths)
    lines = [fmt_row(headers), sep] + [fmt_row(r) for r in rows]
    return "\n".join(lines)


# ---------------------------------------------------------------------------
# Metric snapshot
# ---------------------------------------------------------------------------

def _table_exists(conn: sqlite3.Connection, table: str) -> bool:
    cur = conn.execute(
        "SELECT 1 FROM sqlite_master WHERE type='table' AND name=?",
        (table,),
    )
    return cur.fetchone() is not None


def compute_metric_snapshot(
    conn: sqlite3.Connection,
    profile: Optional[str] = None,
) -> dict[str, float]:
    """
    Query live metric values from sibling tables and return a dict mapping
    each AlertMetric value to its current float value.

    Missing or empty tables are handled gracefully (returns 0.0).
    """
    snapshot: dict[str, float] = {m.value: 0.0 for m in AlertMetric}

    # ---- eval metrics (eval_runs + eval_cases) --------------------------------
    if _table_exists(conn, "eval_runs") and _table_exists(conn, "eval_cases"):
        try:
            if profile:
                run_cur = conn.execute(
                    "SELECT id FROM eval_runs WHERE profile = ? AND status = 'completed'",
                    (profile,),
                )
            else:
                run_cur = conn.execute(
                    "SELECT id FROM eval_runs WHERE status = 'completed'",
                )
            run_ids = [r[0] for r in run_cur.fetchall()]

            if run_ids:
                placeholders = ",".join("?" * len(run_ids))
                agg_cur = conn.execute(
                    f"SELECT SUM(passed), COUNT(*), AVG(score) "
                    f"FROM eval_cases WHERE eval_run_id IN ({placeholders})",
                    run_ids,
                )
                row = agg_cur.fetchone()
                if row and row[1]:
                    total_passed = row[0] or 0
                    total_cases = row[1] or 1
                    avg_score = row[2] or 0.0
                    snapshot[AlertMetric.EVAL_PASS_RATE.value] = (
                        total_passed / total_cases
                    )
                    snapshot[AlertMetric.EVAL_SCORE.value] = float(avg_score)
        except Exception:
            pass

    # ---- span metrics (spans table) ------------------------------------------
    if _table_exists(conn, "spans"):
        try:
            if profile:
                base_filter = "WHERE profile = ?"
                params_base: tuple = (profile,)
            else:
                base_filter = ""
                params_base = ()

            # error rate
            err_cur = conn.execute(
                f"SELECT "
                f"  SUM(CASE WHEN status = 'error' THEN 1 ELSE 0 END) * 1.0 / "
                f"  MAX(COUNT(*), 1) "
                f"FROM spans {base_filter}",
                params_base,
            )
            row = err_cur.fetchone()
            if row and row[0] is not None:
                snapshot[AlertMetric.SPAN_ERROR_RATE.value] = float(row[0])

            # p95 latency
            p95_cur = conn.execute(
                f"SELECT duration_ms FROM spans {base_filter} "
                f"ORDER BY duration_ms",
                params_base,
            )
            durations = [r[0] for r in p95_cur.fetchall() if r[0] is not None]
            if durations:
                idx = max(0, int(len(durations) * 0.95) - 1)
                snapshot[AlertMetric.P95_LATENCY_MS.value] = float(
                    sorted(durations)[idx]
                )

            # cost per run — average cost_usd grouped by trace_id
            cost_cur = conn.execute(
                f"SELECT trace_id, SUM(cost_usd) FROM spans "
                f"{base_filter} {'AND' if base_filter else 'WHERE'} "
                f"cost_usd IS NOT NULL GROUP BY trace_id",
                params_base,
            )
            trace_costs = [r[1] for r in cost_cur.fetchall() if r[1] is not None]
            if trace_costs:
                snapshot[AlertMetric.COST_USD_PER_RUN.value] = sum(trace_costs) / len(
                    trace_costs
                )
        except Exception:
            # Retry without profile if profile-specific query failed due to
            # missing column; gracefully leave defaults.
            try:
                err_cur2 = conn.execute(
                    "SELECT "
                    "  SUM(CASE WHEN status = 'error' THEN 1 ELSE 0 END) * 1.0 / "
                    "  MAX(COUNT(*), 1) "
                    "FROM spans",
                )
                row2 = err_cur2.fetchone()
                if row2 and row2[0] is not None:
                    snapshot[AlertMetric.SPAN_ERROR_RATE.value] = float(row2[0])
            except Exception:
                pass

    # ---- cache hit rate (from spans attributes) ------------------------------
    if _table_exists(conn, "spans"):
        try:
            if profile:
                cache_cur = conn.execute(
                    "SELECT attributes FROM spans WHERE profile = ?",
                    (profile,),
                )
            else:
                cache_cur = conn.execute("SELECT attributes FROM spans")

            import json as _json
            hits = 0
            total = 0
            for (attrs_raw,) in cache_cur.fetchall():
                if not attrs_raw:
                    continue
                try:
                    attrs = _json.loads(attrs_raw)
                except Exception:
                    continue
                if "cache_hit" in attrs:
                    total += 1
                    if attrs["cache_hit"]:
                        hits += 1
            if total > 0:
                snapshot[AlertMetric.CACHE_HIT_RATE.value] = hits / total
        except Exception:
            pass

    # ---- memory count --------------------------------------------------------
    if _table_exists(conn, "semantic_memories"):
        try:
            if profile:
                mc_cur = conn.execute(
                    "SELECT COUNT(*) FROM semantic_memories WHERE profile = ?",
                    (profile,),
                )
            else:
                mc_cur = conn.execute("SELECT COUNT(*) FROM semantic_memories")
            row = mc_cur.fetchone()
            if row:
                snapshot[AlertMetric.MEMORY_COUNT.value] = float(row[0])
        except Exception:
            pass

    return snapshot
