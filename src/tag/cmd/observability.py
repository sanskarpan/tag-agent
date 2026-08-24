"""Observability, tracing, and cost monitoring commands."""
from __future__ import annotations

import argparse
import datetime
import hashlib
import json
import shutil
import sqlite3
import sys
from pathlib import Path
from typing import Any

try:
    from tag.tui_output import print_error, print_success, print_warning
except Exception:
    def print_error(msg): print(f"error: {msg}", file=sys.stderr)
    def print_success(msg): print(msg)
    def print_warning(msg): print(f"warning: {msg}", file=sys.stderr)


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

def _positive_int(value: str) -> int:
    parsed = int(value)
    if parsed <= 0:
        raise argparse.ArgumentTypeError("value must be > 0")
    return parsed


_COST_TABLE: dict[str, dict[str, float]] = {
    "openai/gpt-4o": {"prompt": 0.005, "completion": 0.015},
    "openai/gpt-4o-mini": {"prompt": 0.00015, "completion": 0.0006},
    "openai/gpt-4-turbo": {"prompt": 0.01, "completion": 0.03},
    "openai/gpt-3.5-turbo": {"prompt": 0.0005, "completion": 0.0015},
    "anthropic/claude-sonnet-4-6": {"prompt": 0.003, "completion": 0.015},
    "anthropic/claude-opus-4-8": {"prompt": 0.015, "completion": 0.075},
    "anthropic/claude-haiku-4-5": {"prompt": 0.00025, "completion": 0.00125},
    "google/gemini-2.5-pro": {"prompt": 0.00125, "completion": 0.005},
    "google/gemini-2.5-flash": {"prompt": 0.000075, "completion": 0.0003},
    "meta-llama/llama-3.3-70b-instruct": {"prompt": 0.00059, "completion": 0.00079},
}


def _cache_savings(
    cache_read_tokens: int, cache_creation_tokens: int, model_id: str
) -> tuple[float, float, float]:
    """Returns (savings_usd, write_premium_usd, net_savings_usd)."""
    # Clamp negative token counts so savings figures never go negative.
    cache_read_tokens = max(0, cache_read_tokens or 0)
    cache_creation_tokens = max(0, cache_creation_tokens or 0)
    entry = _COST_TABLE.get(model_id or "", {"prompt": 0.003, "completion": 0.015})
    input_rate = entry.get("prompt", 0.003)
    savings = (cache_read_tokens / 1_000) * input_rate * 0.9
    write_mult = 2.0 if "haiku" in (model_id or "").lower() else 1.25
    write_premium = (cache_creation_tokens / 1_000) * input_rate * (write_mult - 1.0)
    return savings, write_premium, savings - write_premium


def _parse_since_delta(since: str) -> datetime.timedelta:
    """Parse '7d', '2w', '1m' into a timedelta.

    Raises ValueError with a user-facing message on malformed input.
    """
    s = (since or "").strip().lower()
    if len(s) < 2 or not s[:-1].isdigit():
        raise ValueError("invalid --since value; expected e.g. 7d, 2w, 1m")
    unit = s[-1]
    n = int(s[:-1])
    if unit == "d":
        return datetime.timedelta(days=n)
    if unit == "w":
        return datetime.timedelta(weeks=n)
    if unit == "m":
        return datetime.timedelta(days=n * 30)
    raise ValueError("invalid --since value; expected e.g. 7d, 2w, 1m")


def _parse_since(since: str) -> str:
    """Convert '7d', '2w', '1m' to an ISO cutoff string."""
    cutoff = datetime.datetime.now(datetime.timezone.utc) - _parse_since_delta(since)
    return cutoff.strftime("%Y-%m-%dT%H:%M:%S")


def _build_snapshot(conn: sqlite3.Connection, trace_id: str) -> dict | None:
    """Build an in-memory snapshot of a trace from live spans (read-only).

    Returns the snapshot dict, or None if the trace has no spans. Does not
    write anything to the database.
    """
    rows = conn.execute(
        """SELECT id, name, profile, model_id, started_at, finished_at,
               prompt_tokens, completion_tokens, status, attributes, error_msg
           FROM spans WHERE trace_id=? ORDER BY started_at""",
        (trace_id,),
    ).fetchall()
    if not rows:
        return None

    now = datetime.datetime.now(datetime.timezone.utc).isoformat()
    return {
        "trace_id": trace_id,
        "captured_at": now,
        "spans": [
            {
                "id": r[0], "name": r[1], "profile": r[2], "model_id": r[3],
                "started_at": r[4], "finished_at": r[5],
                "prompt_tokens": r[6], "completion_tokens": r[7],
                "status": r[8],
                "attributes": json.loads(r[9] or "{}"),
                "error_msg": r[10],
            }
            for r in rows
        ],
    }


def _snapshot_trace(conn: sqlite3.Connection, trace_id: str) -> None:
    """Capture a full snapshot of the trace into trace_snapshots.

    The snapshot row PK is derived deterministically from the trace ID so
    repeated snapshots of the same trace de-duplicate (INSERT OR REPLACE
    updates the single existing row) instead of accumulating unbounded rows.
    """
    snapshot = _build_snapshot(conn, trace_id)
    if snapshot is None:
        return

    now = snapshot["captured_at"]
    snap_id = hashlib.sha256(trace_id.encode()).hexdigest()[:16]
    conn.execute(
        """INSERT OR REPLACE INTO trace_snapshots(id, trace_id, step_index, snapshot_json, created_at)
           VALUES(?,?,0,?,?)""",
        (snap_id, trace_id, json.dumps(snapshot), now),
    )
    conn.commit()


# ---------------------------------------------------------------------------
# cmd_costs — PRD-012
# ---------------------------------------------------------------------------

def _empty_source_summary(source: str) -> dict:
    """A zeroed per-source breakdown section (used when a table is absent)."""
    return {
        "source": source,
        "rows": 0,
        "prompt_tokens": 0,
        "completion_tokens": 0,
        "total_tokens": 0,
        "cost_usd": 0.0,
        "includes_estimated_rates": False,
    }


def _table_columns(conn: sqlite3.Connection, table: str) -> set:
    """Return the column names of *table*, or an empty set if it is absent."""
    try:
        return {row[1] for row in conn.execute(f"PRAGMA table_info({table})")}
    except sqlite3.Error:
        return set()


def _summarise_source(
    conn: sqlite3.Connection,
    *,
    source: str,
    table: str,
    cost_column: str,
    profile_column: str,
    profile_filter: str | None,
) -> dict:
    """Aggregate tokens + cost for one population (``runs`` or ``spans``).

    Rows whose stored cost column is NULL *or* 0 but which carry a ``model_id``
    and token counts are priced through :func:`tag.cost_table.compute_cost`; if
    such a fallback resolves to an entry flagged ``estimated``, the section's
    ``includes_estimated_rates`` is set. A missing table or missing columns
    yields a zeroed, ``rows: 0`` section rather than raising.

    A stored 0 must be treated as "no stored cost", not as a real $0.00: the
    Go engine bootstraps ``runs.estimated_cost_usd`` as ``NOT NULL DEFAULT 0``
    while the Python schema leaves it nullable, so the very same unpriced row
    reads back as 0 on a Go-created database and NULL on a Python-created one.
    Honouring only NULL here made Python report a confidently-wrong $0.00 for
    priced traffic and diverge from Go. Do not "simplify" this back to an
    ``is not None`` check.
    """
    summary = _empty_source_summary(source)
    cols = _table_columns(conn, table)
    if not cols:
        return summary
    if not {"prompt_tokens", "completion_tokens"} <= cols:
        return summary

    has_cost = cost_column in cols
    has_model = "model_id" in cols
    has_total = "total_tokens" in cols

    select_cost = cost_column if has_cost else "NULL"
    select_model = "model_id" if has_model else "NULL"
    select_total = "total_tokens" if has_total else "NULL"
    where = ""
    params: tuple = ()
    if profile_filter and profile_column in cols:
        where = f"WHERE {profile_column} = ?"
        params = (profile_filter,)

    try:
        rows = conn.execute(
            f"SELECT prompt_tokens, completion_tokens, {select_total}, "
            f"{select_cost}, {select_model} FROM {table} {where}",
            params,
        ).fetchall()
    except sqlite3.Error:
        return summary

    try:
        from tag.cost_table import compute_cost, resolve_pricing_entry
    except Exception:  # noqa: BLE001 - pricing is optional
        compute_cost = None  # type: ignore[assignment]
        resolve_pricing_entry = None  # type: ignore[assignment]

    for prompt_tokens, completion_tokens, total_tokens, cost, model_id in rows:
        prompt_tokens = int(prompt_tokens or 0)
        completion_tokens = int(completion_tokens or 0)
        total = int(total_tokens) if total_tokens is not None else prompt_tokens + completion_tokens
        summary["rows"] += 1
        summary["prompt_tokens"] += prompt_tokens
        summary["completion_tokens"] += completion_tokens
        summary["total_tokens"] += total
        try:
            stored = float(cost) if cost is not None else None
        except (TypeError, ValueError):
            stored = None
        if stored:  # non-None and non-zero
            summary["cost_usd"] += stored
            continue
        # Fall back to the pricing table for rows with tokens but no stored cost
        # (NULL on the Python schema, 0 on the Go NOT NULL DEFAULT 0 schema).
        if not model_id or compute_cost is None:
            continue
        if prompt_tokens == 0 and completion_tokens == 0:
            continue
        derived = compute_cost(str(model_id), prompt_tokens, completion_tokens)
        if derived is None:
            continue
        summary["cost_usd"] += float(derived)
        entry = resolve_pricing_entry(str(model_id)) if resolve_pricing_entry else None
        if entry is not None and getattr(entry, "estimated", False):
            summary["includes_estimated_rates"] = True

    return summary


def _build_cost_breakdown(
    conn: sqlite3.Connection | None, profile_filter: str | None
) -> tuple[dict, dict]:
    """Return ``(by_source, totals)`` for the runs + spans populations."""
    if conn is None:
        runs_summary = _empty_source_summary("runs")
        spans_summary = _empty_source_summary("spans")
    else:
        runs_summary = _summarise_source(
            conn, source="runs", table="runs", cost_column="estimated_cost_usd",
            profile_column="master_profile", profile_filter=profile_filter,
        )
        spans_summary = _summarise_source(
            conn, source="spans", table="spans", cost_column="cost_usd",
            profile_column="profile", profile_filter=profile_filter,
        )

    by_source = {"runs": runs_summary, "spans": spans_summary}
    populated = [s["source"] for s in (runs_summary, spans_summary) if s["rows"] > 0]
    cost = runs_summary["cost_usd"] + spans_summary["cost_usd"]
    totals = {
        "prompt_tokens": runs_summary["prompt_tokens"] + spans_summary["prompt_tokens"],
        "completion_tokens": runs_summary["completion_tokens"] + spans_summary["completion_tokens"],
        "total_tokens": runs_summary["total_tokens"] + spans_summary["total_tokens"],
        "cost_usd": cost,
        # Back-compat alias for consumers predating the dual-source contract.
        "estimated_cost_usd": cost,
        "sources": populated,
        "overlap_warning": len(populated) == 2,
        "includes_estimated_rates": (
            runs_summary["includes_estimated_rates"]
            or spans_summary["includes_estimated_rates"]
        ),
    }
    return by_source, totals


def _fetch_run_details(
    conn: sqlite3.Connection, profile_filter: str | None, limit: int
) -> list[dict]:
    """Return the per-run detail rows, tolerating partial ``runs`` schemas.

    The Go-created ``runs`` table has no ``total_tokens`` column; rather than
    bailing out of the detail listing (which produced an empty ``runs`` array
    and a misleading "no cost data" sentence on a database that plainly had
    rows), the total is derived as ``prompt_tokens + completion_tokens`` — the
    same thing the Go engine reports. Any other missing column selects as NULL
    so an alien schema degrades to partial rows instead of raising.
    """
    cols = _table_columns(conn, "runs")
    if not cols or "id" not in cols:
        return []

    def col(name: str) -> str:
        return name if name in cols else "NULL"

    has_total = "total_tokens" in cols
    select_total = "total_tokens" if has_total else "NULL"
    where = ""
    params: tuple = ()
    if profile_filter and "master_profile" in cols:
        where = "WHERE master_profile = ?"
        params = (profile_filter,)
    order = "ORDER BY created_at DESC " if "created_at" in cols else ""
    try:
        rows = conn.execute(
            f"SELECT id, {col('master_profile')}, {col('model_id')}, "
            f"{col('prompt_tokens')}, {col('completion_tokens')}, {select_total}, "
            f"{col('estimated_cost_usd')}, {col('created_at')} FROM runs "
            f"{where} {order}LIMIT ?",
            (*params, limit),
        ).fetchall()
    except sqlite3.Error:
        return []

    out: list[dict] = []
    for r in rows:
        prompt_tokens, completion_tokens = r[3], r[4]
        total = r[5]
        if total is None:
            total = int(prompt_tokens or 0) + int(completion_tokens or 0)
        out.append({
            "id": r[0], "profile": r[1], "model_id": r[2],
            "prompt_tokens": prompt_tokens, "completion_tokens": completion_tokens,
            "total_tokens": total, "estimated_cost_usd": r[6], "created_at": r[7],
        })
    return out


def _print_cost_breakdown(by_source: dict, totals: dict) -> None:
    """Print the labelled per-source breakdown + TOTAL line."""
    print()
    print(f"{'Source':<10} {'Rows':>6} {'Prompt':>10} {'Completion':>12} {'Tokens':>10} {'Cost':>12}")
    print("-" * 64)
    for key in ("runs", "spans"):
        s = by_source[key]
        marker = "  (includes estimated rates)" if s["includes_estimated_rates"] else ""
        print(
            f"{s['source']:<10} {s['rows']:>6} {s['prompt_tokens']:>10} "
            f"{s['completion_tokens']:>12} {s['total_tokens']:>10} "
            f"${s['cost_usd']:>11.4f}{marker}"
        )
    print("-" * 64)
    print(
        f"{'TOTAL':<10} {'':>6} {totals['prompt_tokens']:>10} "
        f"{totals['completion_tokens']:>12} {totals['total_tokens']:>10} "
        f"${totals['cost_usd']:>11.4f}"
    )
    if totals["overlap_warning"]:
        print("note: runs and spans are different populations (a run aggregates spans); "
              "the TOTAL may double-count.")
    if totals["includes_estimated_rates"]:
        print("note: total includes estimated rates that are not authoritative "
              "published prices.")


def cmd_costs(args: argparse.Namespace) -> int:
    from tag.controller import load_config, config_path, runtime_db_path
    cfg = load_config(config_path(getattr(args, "config", None)))
    db_path = runtime_db_path(cfg)
    profile_filter = getattr(args, "profile", None)
    if not db_path.exists():
        # Emit the full contract shape (zeroed) so a --json consumer can tell
        # "no data" from "different schema" instead of getting a stub.
        by_source, totals = _build_cost_breakdown(None, profile_filter)
        if getattr(args, "json", False):
            print(json.dumps({"runs": [], "by_source": by_source, "totals": totals}, indent=2))
        else:
            print("No runs database found.")
            _print_cost_breakdown(by_source, totals)
        return 0
    conn = sqlite3.connect(str(db_path))
    try:
        limit = getattr(args, "limit", 20)
        # A partial `runs` schema (e.g. the Go-created table, which has no
        # `total_tokens`) no longer short-circuits the detail listing: the rows
        # exist and must be shown, with the total derived from the token
        # columns. See _fetch_run_details.
        run_rows = _fetch_run_details(conn, profile_filter, limit)
        # `runs` and `spans` are written by different engines (Python fills
        # spans, Go fills runs), so both populations are reported separately.
        by_source, totals = _build_cost_breakdown(conn, profile_filter)
    finally:
        conn.close()

    if getattr(args, "json", False):
        print(json.dumps(
            {"runs": run_rows, "by_source": by_source, "totals": totals}, indent=2
        ))
        return 0

    # Only claim "no data" when both populations are genuinely empty — never
    # above a breakdown that is about to print non-zero rows.
    if not run_rows and not totals["sources"]:
        print("No cost data recorded yet (run some tasks first).")
        _print_cost_breakdown(by_source, totals)
        return 0

    print(f"{'Run ID':<24} {'Profile':<20} {'Model':<40} {'Tokens':>8} {'Cost':>10}")
    print("-" * 110)
    for r in run_rows:
        # A stored 0 means "not priced" (see _summarise_source), so render it
        # as n/a rather than printing $0.0000 directly above a breakdown that
        # reports this same run's derived cost as non-zero.
        stored = r["estimated_cost_usd"]
        cost = f"${float(stored):.4f}" if stored else "n/a"
        print(f"{str(r['id']):<24} {(r['profile'] or ''):<20} "
              f"{(r['model_id'] or ''):<40} {(r['total_tokens'] or 0):>8} {cost:>10}")
    print("-" * 110)
    _print_cost_breakdown(by_source, totals)
    return 0


# ---------------------------------------------------------------------------
# cmd_trace — PRD-013
# ---------------------------------------------------------------------------

def cmd_trace(args: argparse.Namespace) -> int:
    from tag.controller import load_config, config_path, runtime_db_path
    cfg = load_config(config_path(getattr(args, "config", None)))
    db_path = runtime_db_path(cfg)
    if not db_path.exists():
        # `show <id>` on a store with no spans is a NOT-FOUND (exit 1, shared
        # error shape), not a clean empty — matching the Go harness and the
        # in-DB no-rows path below (#763). `list` stays an empty result (exit 0).
        if getattr(args, "trace_subcommand", None) == "show":
            msg = f'no spans found for trace "{getattr(args, "trace_id", "")}"'
            if getattr(args, "json", False):
                print(json.dumps({"error": msg}))
            else:
                print_error(msg)
            return 1
        if getattr(args, "json", False):
            print(json.dumps([]))
        else:
            print("No spans database found.")
        return 0

    conn = sqlite3.connect(str(db_path))
    try:
        sub = getattr(args, "trace_subcommand", None)

        if sub == "list" or sub is None:
            rows = conn.execute(
                "SELECT DISTINCT trace_id, MIN(started_at) as t, COUNT(*) as n FROM spans "
                "GROUP BY trace_id ORDER BY t DESC LIMIT ?",
                (getattr(args, "limit", 20),),
            ).fetchall()
            if getattr(args, "json", False):
                print(json.dumps([{"trace_id": r[0], "started_at": r[1], "span_count": r[2]} for r in rows], indent=2))
            else:
                print(f"{'Trace ID':<36} {'Started':<28} {'Spans':>6}")
                print("-" * 74)
                for r in rows:
                    print(f"{r[0]:<36} {r[1]:<28} {r[2]:>6}")
            return 0

        if sub == "show":
            trace_id = args.trace_id
            rows = conn.execute(
                "SELECT id, trace_id, parent_id, name, profile, model_id, started_at, "
                "finished_at, duration_ms, status, prompt_tokens, completion_tokens, "
                "attributes, error_msg FROM spans WHERE trace_id = ? ORDER BY started_at",
                (trace_id,),
            ).fetchall()
            if not rows:
                # Shared not-found shape: {"error":...} on stdout under --json,
                # "error: ..." on stderr otherwise, exit 1 — matching the Go
                # harness and sibling detail commands (was "[]"/stdout) (#763).
                msg = f'no spans found for trace "{trace_id}"'
                if getattr(args, "json", False):
                    print(json.dumps({"error": msg}))
                else:
                    print_error(msg)
                return 1
            if getattr(args, "json", False):
                col = ["id","trace_id","parent_id","name","profile","model_id","started_at",
                       "finished_at","duration_ms","status","prompt_tokens","completion_tokens",
                       "attributes","error_msg"]
                print(json.dumps([dict(zip(col, r)) for r in rows], indent=2))
                return 0
            try:
                from tag.tracing import Span, render_trace_terminal
                spans = []
                for r in rows:
                    s = Span(
                        id=r[0], trace_id=r[1], parent_id=r[2], name=r[3],
                        profile=r[4], model_id=r[5], started_at=r[6],
                        finished_at=r[7], duration_ms=r[8], status=r[9],
                        prompt_tokens=r[10], completion_tokens=r[11],
                        attributes=json.loads(r[12] or "{}"), error_msg=r[13],
                    )
                    spans.append(s)
                print(render_trace_terminal(spans))
            except ImportError:
                for r in rows:
                    print(f"  {r[3]:<40} {r[9]:<8} {r[8] or 0}ms")
            return 0

        if sub == "export":
            endpoint = args.endpoint
            trace_id = getattr(args, "trace_id", None)
            where = "WHERE trace_id = ?" if trace_id else ""
            params = (trace_id,) if trace_id else ()
            rows = conn.execute(
                f"SELECT id, trace_id, parent_id, name, profile, model_id, started_at, "
                f"finished_at, duration_ms, status, prompt_tokens, completion_tokens, "
                f"attributes, error_msg FROM spans {where} ORDER BY started_at",
                params,
            ).fetchall()
            try:
                from tag.tracing import export_spans_otlp
                ok = export_spans_otlp(rows, endpoint)
                if ok:
                    print_success(f"Exported {len(rows)} spans to {endpoint}")
                else:
                    print_error(f"OTLP export failed — check endpoint: {endpoint}")
                    return 1
            except ImportError:
                print_error("tag.tracing not available")
                return 1
            return 0

    finally:
        conn.close()

    # PRD-032 extension: replay, diff, checkpoint, snapshot
    if sub in ("replay", "diff", "checkpoint", "snapshot"):
        return cmd_trace_extended(args)

    print_error(f"Unknown subcommand: {sub}")
    return 1


# ---------------------------------------------------------------------------
# cmd_trace_extended — PRD-032
# ---------------------------------------------------------------------------

def cmd_trace_extended(args: argparse.Namespace) -> int:
    """PRD-032: Extended trace commands including replay, diff, and snapshot."""
    from tag.controller import load_config, config_path, runtime_db_path
    cfg = load_config(config_path(getattr(args, "config", None)))
    db_path = runtime_db_path(cfg)
    if not db_path.exists():
        if getattr(args, "json", False):
            print(json.dumps([]))
        else:
            print("No spans database found.")
        return 0

    conn = sqlite3.connect(str(db_path))
    try:
        sub = getattr(args, "trace_subcommand", None)

        if sub == "snapshot":
            trace_id = getattr(args, "trace_id", None)
            if not trace_id:
                print_error("TRACE_ID required")
                return 1
            _snapshot_trace(conn, trace_id)
            print(f"Snapshot captured for trace: {trace_id}")
            return 0

        if sub == "replay":
            trace_id = getattr(args, "trace_id", None)
            if not trace_id:
                print_error("TRACE_ID required")
                return 1
            row = conn.execute(
                "SELECT snapshot_json FROM trace_snapshots WHERE trace_id=? ORDER BY created_at DESC LIMIT 1",
                (trace_id,),
            ).fetchone()
            if row:
                snap = json.loads(row[0])
            else:
                # Read-only: build a snapshot from live spans without persisting.
                snap = _build_snapshot(conn, trace_id)
            if not snap:
                print_error(f"No snapshot found for trace {trace_id}")
                return 1

            spans = snap.get("spans", [])
            if getattr(args, "json", False):
                print(json.dumps(snap, indent=2))
                return 0

            print(f"Trace replay: {trace_id}")
            print(f"Captured: {snap.get('captured_at', '?')}")
            print(f"Spans: {len(spans)}")
            print()
            for i, s in enumerate(spans, 1):
                status = s.get("status", "?")
                dur = ""
                if s.get("started_at") and s.get("finished_at"):
                    try:
                        from datetime import datetime as _dt
                        start = _dt.fromisoformat(s["started_at"])
                        end = _dt.fromisoformat(s["finished_at"])
                        ms = int((end - start).total_seconds() * 1000)
                        dur = f"  {ms}ms"
                    except Exception:
                        pass
                pt = s.get("prompt_tokens", 0) or 0
                ct = s.get("completion_tokens", 0) or 0
                print(f"  [{i:02d}] {s['name']:<40} {status:<8} {pt+ct:>8} tokens{dur}")
                if s.get("error_msg"):
                    print(f"       error: {s['error_msg'][:80]}")
            return 0

        if sub == "diff":
            trace_a = getattr(args, "trace_a", None)
            trace_b = getattr(args, "trace_b", None)
            if not trace_a or not trace_b:
                print_error("Two trace IDs required: TRACE_A TRACE_B")
                return 1

            def _load_snap(tid):
                r = conn.execute(
                    "SELECT snapshot_json FROM trace_snapshots WHERE trace_id=? ORDER BY created_at DESC LIMIT 1",
                    (tid,),
                ).fetchone()
                if r:
                    return json.loads(r[0])
                # Read-only: build from live spans without persisting.
                return _build_snapshot(conn, tid)

            snap_a = _load_snap(trace_a)
            snap_b = _load_snap(trace_b)
            if not snap_a:
                print_error(f"No snapshot for trace {trace_a}")
                return 1
            if not snap_b:
                print_error(f"No snapshot for trace {trace_b}")
                return 1

            # Key by (name, occurrence) rather than name alone. Multiple spans
            # sharing a name (several `llm.call`s in one trace) is the normal
            # case, and keying by name let the last one silently overwrite the
            # rest — so a trace with llm.call=400 and llm.call=900 reported only
            # 900 and printed a delta computed from a single span.
            def _index(spans):
                out, counts = {}, {}
                for s in spans:
                    name = s["name"]
                    counts[name] = counts.get(name, 0) + 1
                    out[(name, counts[name])] = s
                return out, counts

            spans_a, counts_a = _index(snap_a.get("spans", []))
            spans_b, counts_b = _index(snap_b.get("spans", []))
            all_keys = sorted(set(spans_a) | set(spans_b))

            def _tokens(s):
                return ((s or {}).get("prompt_tokens", 0) or 0) + ((s or {}).get("completion_tokens", 0) or 0)

            def _label(key):
                name, occurrence = key
                # Only disambiguate when the name actually repeats, so the
                # common single-span case keeps its original label.
                if max(counts_a.get(name, 0), counts_b.get(name, 0)) > 1:
                    return f"{name}#{occurrence}"
                return name

            if getattr(args, "json", False):
                diff = []
                for key in all_keys:
                    diff.append({
                        "name": key[0],
                        "occurrence": key[1],
                        "a": spans_a.get(key),
                        "b": spans_b.get(key),
                    })
                print(json.dumps(diff, indent=2))
                return 0

            print(f"Trace diff: {trace_a[:12]}  vs  {trace_b[:12]}")
            print(f"{'Span':<40} {'A tokens':>10} {'B tokens':>10} {'Δ tokens':>10} {'A status':<10} {'B status'}")
            print("-" * 100)
            total_a = total_b = 0
            for key in all_keys:
                sa = spans_a.get(key)
                sb = spans_b.get(key)
                ta = _tokens(sa)
                tb = _tokens(sb)
                total_a += ta
                total_b += tb
                delta = tb - ta
                delta_str = f"+{delta}" if delta > 0 else str(delta)
                sta = (sa or {}).get("status", "—")
                stb = (sb or {}).get("status", "—")
                prefix = "+" if sa is None else ("-" if sb is None else " ")
                print(f"{prefix} {_label(key):<38} {ta:>10} {tb:>10} {delta_str:>10} {sta:<10} {stb}")
            total_delta = total_b - total_a
            total_delta_str = f"+{total_delta}" if total_delta > 0 else str(total_delta)
            print(f"  {'TOTAL':<38} {total_a:>10} {total_b:>10} {total_delta_str:>10}")
            return 0

        if sub == "checkpoint":
            # snapshot sub-alias
            trace_id = getattr(args, "trace_id", None)
            if not trace_id:
                print_error("TRACE_ID required")
                return 1
            _snapshot_trace(conn, trace_id)
            snaps = conn.execute(
                "SELECT id, created_at FROM trace_snapshots WHERE trace_id=? ORDER BY created_at DESC",
                (trace_id,),
            ).fetchall()
            if getattr(args, "json", False):
                print(json.dumps([{"id": r[0], "created_at": r[1]} for r in snaps], indent=2))
            else:
                print(f"Checkpoints for trace {trace_id}:")
                for i, r in enumerate(snaps):
                    print(f"  [{i}] {r[0]}  {r[1]}")
            return 0

    finally:
        conn.close()

    return 0


# ---------------------------------------------------------------------------
# cmd_cache — PRD-030
# ---------------------------------------------------------------------------

def cmd_cache(args: argparse.Namespace) -> int:
    """PRD-030: Prompt cache analytics — stats/trend/tips subcommands."""
    sub = getattr(args, "cache_subcommand", None) or "stats"
    if sub == "stats":
        return _cmd_cache_stats(args)
    if sub == "trend":
        return _cmd_cache_trend(args)
    if sub == "tips":
        return _cmd_cache_tips(args)
    print("usage: tag cache stats|trend|tips [options]")
    return 0


def _cmd_cache_stats(args: argparse.Namespace) -> int:
    from tag.controller import load_config, config_path, runtime_db_path
    cfg = load_config(config_path(getattr(args, "config", None)))
    db_path = runtime_db_path(cfg)
    _json = getattr(args, "json", False)
    profile_filter = getattr(args, "profile", None)
    model_filter = getattr(args, "model", None)
    since = getattr(args, "since", "7d") or "7d"
    warn_threshold = getattr(args, "warn_threshold", None)

    try:
        cutoff = _parse_since(since)
    except ValueError as exc:
        if _json:
            print(json.dumps({"error": str(exc)}))
        else:
            print_error(str(exc))
        return 1

    if not db_path.exists():
        msg = {"error": "No runs database"} if _json else "No runs database found."
        print(json.dumps(msg) if _json else msg)
        return 0

    conn = sqlite3.connect(str(db_path))
    try:
        cols = {r[1] for r in conn.execute("PRAGMA table_info(runs)")}
        has_cache = "cache_read_tokens" in cols and "cache_creation_tokens" in cols

        where_parts = ["created_at >= ?"]
        params: list = [cutoff]
        if profile_filter:
            where_parts.append("master_profile=?"); params.append(profile_filter)
        if model_filter:
            where_parts.append("model_id=?"); params.append(model_filter)
        where = "WHERE " + " AND ".join(where_parts)

        if has_cache:
            rows = conn.execute(
                f"""SELECT master_profile, model_id,
                       SUM(prompt_tokens), SUM(completion_tokens),
                       SUM(COALESCE(cache_read_tokens,0)),
                       SUM(COALESCE(cache_creation_tokens,0)),
                       SUM(COALESCE(estimated_cost_usd,0)), COUNT(*)
                    FROM runs {where}
                    GROUP BY master_profile, model_id
                    ORDER BY SUM(COALESCE(cache_read_tokens,0)) DESC LIMIT 30""",
                params,
            ).fetchall()
        else:
            rows = conn.execute(
                f"""SELECT master_profile, model_id,
                       SUM(prompt_tokens), SUM(completion_tokens),
                       0, 0, SUM(COALESCE(estimated_cost_usd,0)), COUNT(*)
                    FROM runs {where}
                    GROUP BY master_profile, model_id
                    ORDER BY SUM(prompt_tokens) DESC LIMIT 30""",
                params,
            ).fetchall()
    finally:
        conn.close()

    if not rows:
        msg = [] if _json else "No run data found for the given filters."
        print(json.dumps(msg) if _json else msg)
        return 0

    warned = False
    if _json:
        out = []
        for r in rows:
            pt = r[2] or 0; crt = r[4] or 0; cct = r[5] or 0
            hit_rate = crt / pt if pt > 0 else None
            savings, write_prem, net = _cache_savings(crt, cct, r[1] or "")
            out.append({
                "profile": r[0], "model": r[1],
                "window_days": since, "runs_total": r[7],
                "prompt_tokens": pt, "completion_tokens": r[3] or 0,
                "cache_read_tokens": crt, "cache_creation_tokens": cct,
                "hit_rate": round(hit_rate, 4) if hit_rate is not None else None,
                "savings_usd": round(savings, 6), "write_premium_usd": round(write_prem, 6),
                "net_savings_usd": round(net, 6), "total_cost_usd": r[6] or 0,
            })
            if warn_threshold and hit_rate is not None and hit_rate < warn_threshold:
                warned = True
        print(json.dumps(out, indent=2))
        return 1 if warned else 0

    # Table output
    print(f"\nPrompt Cache Analytics — last {since}\n")
    for r in rows:
        profile, model, pt, ct, crt, cct, cost, runs = r
        pt = pt or 0; crt = crt or 0; cct = cct or 0
        hit_rate = crt / pt if pt > 0 else 0.0
        savings, write_prem, net = _cache_savings(crt, cct, model or "")
        if warn_threshold and pt > 0 and hit_rate < warn_threshold:
            warned = True
            print(f"  [WARN] {profile}: hit rate {hit_rate:.1%} below threshold {warn_threshold:.0%}")
        print(f"  Profile: {profile}  |  Model: {model}")
        print(f"  {'Runs':<22} {runs}")
        print(f"  {'Total input tokens':<22} {pt:,}")
        print(f"  {'Cache write tokens':<22} {cct:,}  ({cct/pt*100:.1f}%)" if pt else f"  {'Cache write tokens':<22} {cct:,}")
        print(f"  {'Cache read tokens':<22} {crt:,}  ({hit_rate:.1%} hit rate)")
        print(f"  {'Write premium':<22} ${write_prem:.4f}")
        print(f"  {'Read savings':<22} ${savings:.4f}")
        print(f"  {'Net savings':<22} ${net:.4f}")
        print()
    return 1 if warned else 0


def _cmd_cache_trend(args: argparse.Namespace) -> int:
    from tag.controller import load_config, config_path, runtime_db_path
    cfg = load_config(config_path(getattr(args, "config", None)))
    db_path = runtime_db_path(cfg)
    profile_filter = getattr(args, "profile", None)
    since = getattr(args, "since", "30d") or "30d"
    buckets = int(getattr(args, "buckets", 14) or 14)
    _json = getattr(args, "json", False)

    try:
        delta = _parse_since_delta(since)
    except ValueError as exc:
        if _json:
            print(json.dumps({"error": str(exc)}))
        else:
            print_error(str(exc))
        return 1
    days = max(1, delta.days)

    if not db_path.exists():
        if _json:
            print(json.dumps({"profile": profile_filter, "since": since, "buckets": []}))
        else:
            print("No runs database found.")
        return 0

    conn = sqlite3.connect(str(db_path))
    try:
        cols = {r[1] for r in conn.execute("PRAGMA table_info(runs)")}
        has_cache = "cache_read_tokens" in cols
        cutoff = (datetime.datetime.now(datetime.timezone.utc) - delta).strftime("%Y-%m-%dT%H:%M:%S")
        where = "WHERE created_at >= ?" + (" AND master_profile=?" if profile_filter else "")
        params = [cutoff] + ([profile_filter] if profile_filter else [])
        if has_cache:
            rows = conn.execute(
                f"""SELECT date(created_at) as day,
                       SUM(prompt_tokens), SUM(COALESCE(cache_read_tokens,0))
                    FROM runs {where}
                    GROUP BY day ORDER BY day""", params
            ).fetchall()
        else:
            rows = conn.execute(
                f"SELECT date(created_at) as day, SUM(prompt_tokens), 0 FROM runs {where} GROUP BY day ORDER BY day",
                params
            ).fetchall()
    finally:
        conn.close()

    data = {r[0]: (r[1] or 0, r[2] or 0) for r in rows}
    today = datetime.date.today()
    start = today - datetime.timedelta(days=days - 1)

    # Build a per-day series across the requested window.
    series = []
    for i in range(days):
        day = (start + datetime.timedelta(days=i)).isoformat()
        pt, crt = data.get(day, (0, 0))
        series.append((day, pt, crt))

    # Group days into at most `buckets` contiguous buckets.
    n_buckets = max(1, min(buckets, days))
    size = (days + n_buckets - 1) // n_buckets
    grouped = []
    for b in range(0, days, size):
        chunk = series[b:b + size]
        if not chunk:
            continue
        pt = sum(c[1] for c in chunk)
        crt = sum(c[2] for c in chunk)
        hit = crt / pt if pt > 0 else 0.0
        grouped.append({
            "start": chunk[0][0], "end": chunk[-1][0],
            "prompt_tokens": pt, "cache_read_tokens": crt,
            "hit_rate": round(hit, 4),
        })

    if _json:
        print(json.dumps({
            "profile": profile_filter,
            "since": since,
            "buckets": grouped,
        }, indent=2))
        return 0

    term_width = shutil.get_terminal_size((80, 24)).columns
    bar_width = max(10, term_width - 40)
    print(f"Cache hit rate — {profile_filter or 'all profiles'} — last {since}\n")
    for g in grouped:
        span_label = g["start"] if g["start"] == g["end"] else f"{g['start']}..{g['end']}"
        bar = "█" * int(g["hit_rate"] * bar_width)
        print(f"  {span_label:<24}  {bar:<{bar_width}}  {g['hit_rate']:.0%}")
    return 0


def _cmd_cache_tips(args: argparse.Namespace) -> int:
    from tag.controller import load_config, config_path, runtime_db_path
    cfg = load_config(config_path(getattr(args, "config", None)))
    db_path = runtime_db_path(cfg)
    profile = getattr(args, "profile", None)
    if not profile:
        print("error: --profile is required for cache tips", file=sys.stderr)
        return 1
    if not db_path.exists():
        print("No runs database found.")
        return 0

    conn = sqlite3.connect(str(db_path))
    try:
        cols = {r[1] for r in conn.execute("PRAGMA table_info(runs)")}
        has_cache = "cache_read_tokens" in cols
        if has_cache:
            rows = conn.execute(
                "SELECT prompt, cache_read_tokens, prompt_tokens, created_at FROM runs "
                "WHERE master_profile=? ORDER BY created_at DESC LIMIT 20",
                (profile,),
            ).fetchall()
        else:
            rows = conn.execute(
                "SELECT prompt, 0, prompt_tokens, created_at FROM runs "
                "WHERE master_profile=? ORDER BY created_at DESC LIMIT 20",
                (profile,),
            ).fetchall()
    finally:
        conn.close()

    print(f"Cache tips for profile: {profile}\n")
    if not rows:
        print("  No run history found for this profile.")
        return 0

    # SHA stability check
    shas = [hashlib.sha256((r[0] or "").encode()).hexdigest() for r in rows]
    stable_pairs = sum(a == b for a, b in zip(shas, shas[1:]))
    stability = stable_pairs / max(len(shas) - 1, 1)

    # Hit rate
    total_pt = sum(r[2] or 0 for r in rows)
    total_crt = sum(r[1] or 0 for r in rows)
    hit_rate = total_crt / total_pt if total_pt > 0 else 0.0

    # Prompt length
    recent_prompt = rows[0][0] or ""
    est_tokens = len(recent_prompt.split()) * 1.3

    if hit_rate < 0.3:
        print(f"  [WARN] Cache hit rate is {hit_rate:.0%} over the last {len(rows)} runs (threshold: 30%)")
    else:
        print(f"  [OK]   Cache hit rate is {hit_rate:.0%} over the last {len(rows)} runs")

    if est_tokens > 1024:
        print(f"  [INFO] System prompt is ~{int(est_tokens):,} tokens — large enough to benefit from caching")
    else:
        print(f"  [INFO] System prompt is ~{int(est_tokens):,} tokens — below 1024 token caching threshold")

    print("\nRecommendations:")
    n = 0
    if stability < 0.5:
        n += 1
        print(f"  {n}. System prompt SHA changed in {len(shas)-1-stable_pairs}/{len(shas)-1} consecutive runs.")
        print("     A volatile prompt prevents cache reuse. Move dynamic content to the user-turn message.")
    if hit_rate < 0.3 and est_tokens > 1024:
        n += 1
        print(f"  {n}. Add a cache_control breakpoint at the end of your static system prompt block:")
        print('     {"cache_control": {"type": "ephemeral"}} in your system message.')
    if hit_rate < 0.3 and not has_cache:
        n += 1
        print(f"  {n}. Cache token columns not present — upgrade to tag-agent 0.7.1+ to track cache metrics.")
    if n == 0:
        print("  No specific issues detected — cache appears healthy.")
    return 0


# ---------------------------------------------------------------------------
# cmd_otel_export — PRD-041
# ---------------------------------------------------------------------------

def cmd_otel_export(args: argparse.Namespace) -> int:
    """PRD-041: tag trace export --otlp-endpoint ... --semconv."""
    import urllib.request
    import urllib.error
    from tag.otel_semconv import spans_to_otlp_json, SEMCONV_VERSION
    from tag.controller import load_config, config_path, open_db
    cfg = load_config(config_path(getattr(args, "config", None)))
    db = open_db(cfg)
    trace_id = getattr(args, "trace_id", None)
    endpoint = getattr(args, "endpoint", "") or ""
    include_metrics = not getattr(args, "no_metrics", False)
    semconv = getattr(args, "semconv", SEMCONV_VERSION) or SEMCONV_VERSION
    _json = getattr(args, "json", False)

    # Fetch spans
    if trace_id:
        rows = db.execute(
            "SELECT id, trace_id, parent_id, name, profile, model_id, started_at, "
            "finished_at, duration_ms, status, prompt_tokens, completion_tokens, attributes "
            "FROM spans WHERE trace_id=? ORDER BY started_at",
            (trace_id,),
        ).fetchall()
    else:
        rows = db.execute(
            "SELECT id, trace_id, parent_id, name, profile, model_id, started_at, "
            "finished_at, duration_ms, status, prompt_tokens, completion_tokens, attributes "
            "FROM spans ORDER BY started_at DESC LIMIT 100",
        ).fetchall()

    db.close()

    span_dicts = [
        {
            "id": r[0], "trace_id": r[1], "parent_id": r[2], "name": r[3],
            "profile": r[4], "model_id": r[5], "started_at": r[6],
            "finished_at": r[7], "duration_ms": r[8], "status": r[9],
            "prompt_tokens": r[10], "completion_tokens": r[11],
        }
        for r in rows
    ]

    payload = spans_to_otlp_json(
        span_dicts, include_metrics=include_metrics, semconv_version=semconv
    )

    if not endpoint:
        # With no endpoint, emitting the OTLP JSON payload IS the export — this
        # is the command's primary output (consumers pipe it to a file/collector).
        print(json.dumps(payload, indent=2))
        return 0

    # POST to OTLP endpoint
    body = json.dumps(payload).encode()
    req = urllib.request.Request(
        endpoint.rstrip("/") + "/v1/traces",
        data=body,
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    metrics_status = None
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            traces_status = resp.status
        if include_metrics and any(s.get("prompt_tokens") for s in span_dicts):
            metrics_body = json.dumps({"resourceMetrics": payload.get("resourceMetrics", [])}).encode()
            metrics_req = urllib.request.Request(
                endpoint.rstrip("/") + "/v1/metrics",
                data=metrics_body,
                headers={"Content-Type": "application/json"},
                method="POST",
            )
            with urllib.request.urlopen(metrics_req, timeout=30) as resp:
                metrics_status = resp.status
    except urllib.error.URLError as exc:
        if _json:
            print(json.dumps({"error": str(exc), "endpoint": endpoint}))
        else:
            print_error(f"OTLP export failed: {exc}")
        return 1

    if _json:
        print(json.dumps({
            "exported_spans": len(span_dicts),
            "endpoint": endpoint,
            "semconv": semconv,
            "traces_status": traces_status,
            "metrics_status": metrics_status,
        }, indent=2))
    else:
        print(f"✓ Exported {len(span_dicts)} spans to {endpoint} (HTTP {traces_status})")
        print(f"  OTel GenAI semconv version: {semconv}")
        if metrics_status is not None:
            print(f"✓ Exported token usage metrics (HTTP {metrics_status})")
    return 0


# ---------------------------------------------------------------------------
# cmd_agentops — PRD-044
# ---------------------------------------------------------------------------

def cmd_agentops(args: argparse.Namespace) -> int:
    """PRD-044: tag agentops sessions/show."""
    from tag.integrations.agentops_bridge import (
        is_available, is_configured, list_sessions, get_session_for_run,
        mask_key, ensure_schema as ao_ensure,
    )
    from tag.controller import load_config, config_path, open_db
    cfg = load_config(config_path(getattr(args, "config", None)))
    db = open_db(cfg)
    ao_ensure(db)
    sub = getattr(args, "agentops_subcommand", None)

    if sub == "status":
        sdk_ok = is_available()
        cfg_ok = is_configured(cfg)
        if getattr(args, "json", False):
            import os
            key = cfg.get("agentops", {}).get("api_key", "") or os.environ.get("AGENTOPS_API_KEY", "")
            # Emit the union of both engines' fields. Python previously reported
            # only the SDK/credential block and Go only the run aggregates, so
            # `agentops status --json` had a completely disjoint schema between
            # them and no consumer could read both.
            agg = _agentops_run_aggregates(db)
            db.close()
            payload = {
                "sdk_installed": sdk_ok,
                "api_key_configured": cfg_ok,
                "api_key_masked": mask_key(key) if cfg_ok else None,
            }
            payload.update(agg)
            print(json.dumps(payload, indent=2))
            return 0
        db.close()
        print(f"AgentOps SDK installed: {'✓' if sdk_ok else '✗'}")
        print(f"API key configured:     {'✓' if cfg_ok else '✗ (run: tag config set agentops.api_key <key>)'}")
        if cfg_ok:
            import os
            key = cfg.get("agentops", {}).get("api_key", "") or os.environ.get("AGENTOPS_API_KEY", "")
            print(f"API key:               {mask_key(key)}")
        return 0

    if sub == "sessions" or sub is None:
        limit = getattr(args, "limit", 20)
        if limit is None:
            limit = 20
        sessions = list_sessions(db, limit=limit)
        db.close()
        if getattr(args, "json", False):
            print(json.dumps(sessions, indent=2))
            return 0
        if not sessions:
            print("No AgentOps sessions recorded.")
            return 0
        for s in sessions:
            print(f"{s['run_id'][:12]}  {s['status']:<12}  {s['session_id'] or '(no session)'}  {s['created_at'][:19]}")
        return 0

    if sub == "show":
        run_id = getattr(args, "run_id", "")
        session = get_session_for_run(db, run_id)
        db.close()
        if not session:
            if getattr(args, "json", False):
                print(json.dumps({"error": f"No AgentOps session for run: {run_id}"}))
            else:
                print_error(f"No AgentOps session for run: {run_id}")
            return 1
        if getattr(args, "json", False):
            print(json.dumps(session, indent=2))
        else:
            print(f"Session ID:    {session['session_id']}")
            print(f"Dashboard URL: {session['dashboard_url']}")
            print(f"Status:        {session['status']}")
            print(f"Created at:    {session['created_at']}")
        return 0

    db.close()
    print_error(f"Unknown agentops subcommand: {sub!r}")
    return 1


# ---------------------------------------------------------------------------
# Parser registration
# ---------------------------------------------------------------------------

def register(sub: argparse._SubParsersAction) -> None:  # type: ignore[type-arg]
    """Register all observability subcommands onto *sub*."""

    # ---- PRD-012: costs ----
    costs = sub.add_parser("costs", help="Show token usage and cost estimates for recent runs")
    costs.add_argument("--profile", help="Filter by profile")
    costs.add_argument("--limit", type=_positive_int, default=20)
    costs.add_argument("--json", action="store_true")
    costs.set_defaults(func=cmd_costs)

    # ---- PRD-013: trace ----
    trace = sub.add_parser("trace", help="View and export distributed trace spans")
    trace_sub = trace.add_subparsers(dest="trace_subcommand")
    trace_list = trace_sub.add_parser("list", help="List recent traces")
    trace_list.add_argument("--limit", type=_positive_int, default=20)
    trace_list.add_argument("--json", action="store_true")
    trace_show = trace_sub.add_parser("show", help="Show flamechart for a trace")
    trace_show.add_argument("trace_id", metavar="TRACE_ID")
    trace_show.add_argument("--json", action="store_true")
    trace_export = trace_sub.add_parser("export", help="Export spans to OTLP endpoint")
    trace_export.add_argument("endpoint", metavar="ENDPOINT")
    trace_export.add_argument("--trace-id", metavar="ID", dest="trace_id")
    trace_export.add_argument("--profile")
    # PRD-032: replay, diff, checkpoint, snapshot
    trace_replay = trace_sub.add_parser("replay", help="Replay a captured trace snapshot")
    trace_replay.add_argument("trace_id", metavar="TRACE_ID")
    trace_replay.add_argument("--json", action="store_true")
    trace_diff = trace_sub.add_parser("diff", help="Diff two traces span-by-span")
    trace_diff.add_argument("trace_a", metavar="TRACE_A")
    trace_diff.add_argument("trace_b", metavar="TRACE_B")
    trace_diff.add_argument("--json", action="store_true")
    trace_checkpoint = trace_sub.add_parser("checkpoint", help="List snapshots for a trace")
    trace_checkpoint.add_argument("trace_id", metavar="TRACE_ID")
    trace_checkpoint.add_argument("--json", action="store_true")
    trace_snapshot = trace_sub.add_parser("snapshot", help="Capture a trace snapshot")
    trace_snapshot.add_argument("trace_id", metavar="TRACE_ID")
    for tp in [trace, trace_list, trace_show, trace_export,
               trace_replay, trace_diff, trace_checkpoint, trace_snapshot]:
        tp.set_defaults(func=cmd_trace)

    # ---- PRD-030: cache ----
    cache_cmd = sub.add_parser("cache", help="Prompt cache analytics")
    cache_sub = cache_cmd.add_subparsers(dest="cache_subcommand")

    cache_stats = cache_sub.add_parser("stats", help="Show cache hit rates and savings per profile")
    cache_stats.add_argument("--profile", help="Filter to a specific profile")
    cache_stats.add_argument("--since", default="7d", help="Time window: 7d, 2w, 1m (default: 7d)")
    cache_stats.add_argument("--model", help="Filter by model ID")
    cache_stats.add_argument("--warn-threshold", dest="warn_threshold", type=float, default=None,
                             help="Opt-in: hit-rate below this fraction triggers a warning and "
                                  "nonzero exit (e.g. --warn-threshold 0.5)")
    cache_stats.add_argument("--json", action="store_true")

    cache_trend = cache_sub.add_parser("trend", help="Show cache hit-rate trend over time (ASCII chart)")
    cache_trend.add_argument("--profile", help="Filter to a specific profile")
    cache_trend.add_argument("--since", default="30d", help="Time window (default: 30d)")
    cache_trend.add_argument("--buckets", type=int, default=14, help="Number of time buckets (default: 14)")
    cache_trend.add_argument("--json", action="store_true")

    cache_tips = cache_sub.add_parser("tips", help="Show actionable recommendations to improve cache efficiency")
    cache_tips.add_argument("--profile", help="Filter to a specific profile")
    cache_tips.add_argument("--since", default="7d")

    for cp in [cache_cmd, cache_stats, cache_trend, cache_tips]:
        cp.set_defaults(func=cmd_cache)

    # ---- PRD-041: otel-export ----
    otel_cmd = sub.add_parser("otel-export", help="Export spans with OTel GenAI semconv attributes")
    otel_cmd.add_argument("--trace-id", dest="trace_id", metavar="TRACE_ID")
    otel_cmd.add_argument("--endpoint", help="OTLP HTTP endpoint (e.g. http://localhost:4318)")
    otel_cmd.add_argument("--semconv", default="1.28.0", help="Override OTel GenAI semconv version")
    otel_cmd.add_argument("--no-metrics", action="store_true", dest="no_metrics")
    otel_cmd.add_argument("--json", action="store_true")
    otel_cmd.set_defaults(func=cmd_otel_export)

    # ---- PRD-044: agentops ----
    ao_cmd = sub.add_parser("agentops", help="AgentOps session observability")
    ao_sub = ao_cmd.add_subparsers(dest="agentops_subcommand")
    ao_status = ao_sub.add_parser("status", help="Show AgentOps integration status")
    ao_status.add_argument("--json", action="store_true")
    ao_sessions = ao_sub.add_parser("sessions", help="List AgentOps sessions")
    ao_sessions.add_argument("--limit", type=int, default=20)
    ao_sessions.add_argument("--json", action="store_true")
    ao_show = ao_sub.add_parser("show", help="Show AgentOps session for a run")
    ao_show.add_argument("run_id", metavar="RUN_ID")
    ao_show.add_argument("--json", action="store_true")
    for ap in [ao_cmd, ao_status, ao_sessions, ao_show]:
        ap.set_defaults(func=cmd_agentops)


def _agentops_run_aggregates(db) -> dict:
    """Run-level aggregates matching tag-go/internal/cli/agentops.go's schema."""
    row = db.execute(
        "SELECT COUNT(*), COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(completion_tokens),0), "
        "COALESCE(SUM(estimated_cost_usd),0) FROM runs"
    ).fetchone()
    total_runs, pt, ct, cost = int(row[0]), int(row[1]), int(row[2]), float(row[3])
    statuses = {
        str(r[0]): int(r[1])
        for r in db.execute("SELECT status, COUNT(*) FROM runs GROUP BY status").fetchall()
    }
    # List-of-objects, matching Go's aopProfileStat so the `profiles` value has
    # the same shape in both engines, not just the same key name.
    profiles = []
    prows = db.execute(
        "SELECT master_profile, COUNT(*), COALESCE(SUM(prompt_tokens),0), "
        "COALESCE(SUM(completion_tokens),0), COALESCE(SUM(estimated_cost_usd),0) "
        "FROM runs GROUP BY master_profile ORDER BY master_profile"
    ).fetchall()
    for pr in prows:
        pname = str(pr[0])
        pstatuses = {
            str(r[0]): int(r[1])
            for r in db.execute(
                "SELECT status, COUNT(*) FROM runs WHERE master_profile=? GROUP BY status",
                (pname,),
            ).fetchall()
        }
        profiles.append({
            "profile": pname,
            "runs": int(pr[1]),
            "prompt_tokens": int(pr[2]),
            "completion_tokens": int(pr[3]),
            "total_tokens": int(pr[2]) + int(pr[3]),
            "estimated_cost_usd": float(pr[4]),
            "statuses": pstatuses,
        })
    return {
        "total_runs": total_runs,
        "prompt_tokens": pt,
        "completion_tokens": ct,
        "total_tokens": pt + ct,
        "estimated_cost_usd": cost,
        "statuses": statuses,
        "profiles": profiles,
    }
