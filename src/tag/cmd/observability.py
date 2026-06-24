"""Observability, tracing, and cost monitoring commands."""
from __future__ import annotations

import argparse
import datetime
import hashlib
import json
import shutil
import sqlite3
import sys
import uuid
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
    entry = _COST_TABLE.get(model_id or "", {"prompt": 0.003, "completion": 0.015})
    input_rate = entry.get("prompt", 0.003)
    savings = (cache_read_tokens / 1_000) * input_rate * 0.9
    write_mult = 2.0 if "haiku" in (model_id or "").lower() else 1.25
    write_premium = (cache_creation_tokens / 1_000) * input_rate * (write_mult - 1.0)
    return savings, write_premium, savings - write_premium


def _parse_since(since: str) -> str:
    """Convert '7d', '2w', '1m' to an ISO cutoff string."""
    unit = since[-1].lower()
    n = int(since[:-1])
    delta = {"d": datetime.timedelta(days=n), "w": datetime.timedelta(weeks=n),
             "m": datetime.timedelta(days=n * 30)}.get(unit, datetime.timedelta(days=n))
    cutoff = datetime.datetime.now(datetime.timezone.utc) - delta
    return cutoff.strftime("%Y-%m-%dT%H:%M:%S")


def _snapshot_trace(conn: sqlite3.Connection, trace_id: str) -> None:
    """Capture a full snapshot of the trace into trace_snapshots."""
    rows = conn.execute(
        """SELECT id, name, profile, model_id, started_at, finished_at,
               prompt_tokens, completion_tokens, status, attributes, error_msg
           FROM spans WHERE trace_id=? ORDER BY started_at""",
        (trace_id,),
    ).fetchall()
    if not rows:
        return

    now = datetime.datetime.now(datetime.timezone.utc).isoformat()
    snap_id = uuid.uuid4().hex[:16]
    snapshot = {
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
    conn.execute(
        """INSERT OR REPLACE INTO trace_snapshots(id, trace_id, step_index, snapshot_json, created_at)
           VALUES(?,?,0,?,?)""",
        (snap_id, trace_id, json.dumps(snapshot), now),
    )
    conn.commit()


# ---------------------------------------------------------------------------
# cmd_costs — PRD-012
# ---------------------------------------------------------------------------

def cmd_costs(args: argparse.Namespace) -> int:
    from tag.controller import load_config, config_path, runtime_db_path
    cfg = load_config(config_path(getattr(args, "config", None)))
    db_path = runtime_db_path(cfg)
    if not db_path.exists():
        print("No runs database found.")
        return 0
    conn = sqlite3.connect(str(db_path))
    try:
        cols = {row[1] for row in conn.execute("PRAGMA table_info(runs)")}
        if "total_tokens" not in cols:
            print("No cost data recorded yet (run some tasks first).")
            conn.close()
            return 0
        limit = getattr(args, "limit", 20)
        profile_filter = getattr(args, "profile", None)
        where = "WHERE master_profile = ?" if profile_filter else ""
        params = (profile_filter,) if profile_filter else ()
        rows = conn.execute(
            f"SELECT id, master_profile, model_id, prompt_tokens, completion_tokens, total_tokens, "
            f"estimated_cost_usd, created_at FROM runs {where} ORDER BY created_at DESC LIMIT ?",
            (*params, limit),
        ).fetchall()
        agg = conn.execute(
            f"SELECT SUM(prompt_tokens), SUM(completion_tokens), SUM(total_tokens), "
            f"SUM(estimated_cost_usd) FROM runs {where}",
            params,
        ).fetchone()
    finally:
        conn.close()

    if getattr(args, "json", False):
        out = {
            "runs": [
                {"id": r[0], "profile": r[1], "model_id": r[2], "prompt_tokens": r[3],
                 "completion_tokens": r[4], "total_tokens": r[5],
                 "estimated_cost_usd": r[6], "created_at": r[7]}
                for r in rows
            ],
            "totals": {
                "prompt_tokens": agg[0] or 0,
                "completion_tokens": agg[1] or 0,
                "total_tokens": agg[2] or 0,
                "estimated_cost_usd": agg[3] or 0.0,
            },
        }
        print(json.dumps(out, indent=2))
        return 0

    print(f"{'Run ID':<24} {'Profile':<20} {'Model':<40} {'Tokens':>8} {'Cost':>10}")
    print("-" * 110)
    for r in rows:
        cost = f"${r[6]:.4f}" if r[6] is not None else "n/a"
        print(f"{r[0]:<24} {(r[1] or ''):<20} {(r[2] or ''):<40} {(r[5] or 0):>8} {cost:>10}")
    print("-" * 110)
    total_cost = f"${agg[3]:.4f}" if agg[3] is not None else "n/a"
    print(f"{'TOTAL':<85} {(agg[2] or 0):>8} {total_cost:>10}")
    return 0


# ---------------------------------------------------------------------------
# cmd_trace — PRD-013
# ---------------------------------------------------------------------------

def cmd_trace(args: argparse.Namespace) -> int:
    from tag.controller import load_config, config_path, runtime_db_path
    cfg = load_config(config_path(getattr(args, "config", None)))
    db_path = runtime_db_path(cfg)
    if not db_path.exists():
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
                print(f"No spans found for trace {trace_id}")
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
            profile = getattr(args, "profile", None) or cfg.get("master_profile", "orchestrator")
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
            if not row:
                # Try to build snapshot from live spans
                _snapshot_trace(conn, trace_id)
                row = conn.execute(
                    "SELECT snapshot_json FROM trace_snapshots WHERE trace_id=? ORDER BY created_at DESC LIMIT 1",
                    (trace_id,),
                ).fetchone()
            if not row:
                print_error(f"No snapshot found for trace {trace_id}")
                return 1

            snap = json.loads(row[0])
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
                if not r:
                    _snapshot_trace(conn, tid)
                    r = conn.execute(
                        "SELECT snapshot_json FROM trace_snapshots WHERE trace_id=? ORDER BY created_at DESC LIMIT 1",
                        (tid,),
                    ).fetchone()
                return json.loads(r[0]) if r else None

            snap_a = _load_snap(trace_a)
            snap_b = _load_snap(trace_b)
            if not snap_a:
                print_error(f"No snapshot for trace {trace_a}")
                return 1
            if not snap_b:
                print_error(f"No snapshot for trace {trace_b}")
                return 1

            spans_a = {s["name"]: s for s in snap_a.get("spans", [])}
            spans_b = {s["name"]: s for s in snap_b.get("spans", [])}
            all_names = sorted(set(spans_a) | set(spans_b))

            if getattr(args, "json", False):
                diff = []
                for name in all_names:
                    sa = spans_a.get(name)
                    sb = spans_b.get(name)
                    diff.append({"name": name, "a": sa, "b": sb})
                print(json.dumps(diff, indent=2))
                return 0

            print(f"Trace diff: {trace_a[:12]}  vs  {trace_b[:12]}")
            print(f"{'Span':<40} {'A tokens':>10} {'B tokens':>10} {'Δ tokens':>10} {'A status':<10} {'B status'}")
            print("-" * 100)
            for name in all_names:
                sa = spans_a.get(name)
                sb = spans_b.get(name)
                ta = ((sa or {}).get("prompt_tokens", 0) or 0) + ((sa or {}).get("completion_tokens", 0) or 0)
                tb = ((sb or {}).get("prompt_tokens", 0) or 0) + ((sb or {}).get("completion_tokens", 0) or 0)
                delta = tb - ta
                delta_str = f"+{delta}" if delta > 0 else str(delta)
                sta = (sa or {}).get("status", "—")
                stb = (sb or {}).get("status", "—")
                prefix = "+" if sa is None else ("-" if sb is None else " ")
                print(f"{prefix} {name:<38} {ta:>10} {tb:>10} {delta_str:>10} {sta:<10} {stb}")
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

    if not db_path.exists():
        msg = {"error": "No runs database"} if _json else "No runs database found."
        print(json.dumps(msg) if _json else msg)
        return 0

    conn = sqlite3.connect(str(db_path))
    try:
        cols = {r[1] for r in conn.execute("PRAGMA table_info(runs)")}
        has_cache = "cache_read_tokens" in cols and "cache_creation_tokens" in cols

        cutoff = _parse_since(since)
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
    days = int(getattr(args, "days", 30) or 30)

    if not db_path.exists():
        print("No runs database found.")
        return 0

    conn = sqlite3.connect(str(db_path))
    try:
        cols = {r[1] for r in conn.execute("PRAGMA table_info(runs)")}
        has_cache = "cache_read_tokens" in cols
        cutoff = (datetime.datetime.now(datetime.timezone.utc) - datetime.timedelta(days=days)).strftime("%Y-%m-%dT%H:%M:%S")
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
    term_width = shutil.get_terminal_size((80, 24)).columns
    bar_width = max(10, term_width - 40)

    label = f"Cache hit rate — {profile_filter or 'all profiles'} — last {days} days\n"
    print(label)
    for i in range(days):
        day = (start + datetime.timedelta(days=i)).isoformat()
        if day not in data:
            print(f"  {day}  (no data)")
            continue
        pt, crt = data[day]
        hit = crt / pt if pt > 0 else 0.0
        bar = "█" * int(hit * bar_width)
        print(f"  {day}  {bar:<{bar_width}}  {hit:.0%}")
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

    payload = spans_to_otlp_json(span_dicts, include_metrics=include_metrics)

    if not endpoint:
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
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            print(f"✓ Exported {len(span_dicts)} spans to {endpoint} (HTTP {resp.status})")
            print(f"  OTel GenAI semconv version: {semconv}")
        if include_metrics and any(s.get("prompt_tokens") for s in span_dicts):
            metrics_body = json.dumps({"resourceMetrics": payload.get("resourceMetrics", [])}).encode()
            metrics_req = urllib.request.Request(
                endpoint.rstrip("/") + "/v1/metrics",
                data=metrics_body,
                headers={"Content-Type": "application/json"},
                method="POST",
            )
            with urllib.request.urlopen(metrics_req, timeout=30) as resp:
                print(f"✓ Exported token usage metrics (HTTP {resp.status})")
    except urllib.error.URLError as exc:
        print_error(f"OTLP export failed: {exc}")
        return 1
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
        db.close()
        if getattr(args, "json", False):
            import os
            key = cfg.get("agentops", {}).get("api_key", "") or os.environ.get("AGENTOPS_API_KEY", "")
            print(json.dumps({
                "sdk_installed": sdk_ok,
                "api_key_configured": cfg_ok,
                "api_key_masked": mask_key(key) if cfg_ok else None,
            }, indent=2))
            return 0
        print(f"AgentOps SDK installed: {'✓' if sdk_ok else '✗'}")
        print(f"API key configured:     {'✓' if cfg_ok else '✗ (run: tag config set agentops.api_key <key>)'}")
        if cfg_ok:
            import os
            key = cfg.get("agentops", {}).get("api_key", "") or os.environ.get("AGENTOPS_API_KEY", "")
            print(f"API key:               {mask_key(key)}")
        return 0

    if sub == "sessions" or sub is None:
        limit = getattr(args, "limit", 20) or 20
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
    cache_stats.add_argument("--warn-threshold", dest="warn_threshold", type=float, default=0.5,
                             help="Hit-rate below this fraction triggers a warning (default: 0.50)")
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
