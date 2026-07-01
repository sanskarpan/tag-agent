"""PRD-045 to PRD-072 cluster A/B/C feature commands."""
from __future__ import annotations

import argparse
import json
import os
import sys
import sqlite3
from pathlib import Path
from typing import Any

from tag.core.config import load_config, config_path
from tag.core.paths import runtime_db_path
from tag.core.utils import nonnegative_int, utc_now

try:
    from tag.tui_output import print_error, print_success, print_warning
except Exception:
    def print_error(msg): print(f"error: {msg}", file=sys.stderr)
    def print_success(msg): print(msg)
    def print_warning(msg): print(f"warning: {msg}", file=sys.stderr)


# ---------------------------------------------------------------------------
# Shared helpers used by cluster A/B/C command handlers
# ---------------------------------------------------------------------------

def _load_cfg_and_profile(args):
    cfg = load_config(config_path(getattr(args, "config", None)))
    profile = getattr(args, "profile", None) or cfg.get("defaults", {}).get("master_profile", "default")
    return cfg, profile

def _db_for_profile(profile, cfg):
    return runtime_db_path(cfg)


# ---------------------------------------------------------------------------
# PRD-046: pricing command handler
# ---------------------------------------------------------------------------
def cmd_pricing(args: argparse.Namespace) -> int:
    sub = getattr(args, "pricing_subcommand", None)
    try:
        from tag.cost_table import list_all_models, compute_cost, reload_pricing_table
        reload_pricing_table()
    except ImportError as e:
        print_error(f"cost_table not available: {e}")
        return 1
    if sub == "list" or sub is None:
        models = list_all_models()
        if getattr(args, "json", False):
            print(json.dumps([{"model_id": m.model_id, "input_usd_per_1m": m.input_usd_per_1m,
                               "output_usd_per_1m": m.output_usd_per_1m} for m in models], indent=2))
        else:
            print(f"{'Model':<45} {'Input $/1M':>12} {'Output $/1M':>12}")
            print("-" * 72)
            for m in models:
                print(f"{m.model_id:<45} {m.input_usd_per_1m:>12.4f} {m.output_usd_per_1m:>12.4f}")
        return 0
    if sub == "get":
        cost = compute_cost(args.model, args.input_tokens, args.output_tokens,
                            cache_read=getattr(args, "cache_read", False),
                            batch=getattr(args, "batch", False))
        if cost is None:
            print_error(f"Model not found: {args.model!r}")
            return 1
        print(f"${cost:.8f}")
        return 0
    print_error(f"Unknown pricing subcommand: {sub!r}")
    return 1


# ---------------------------------------------------------------------------
# PRD-045: eval judge command handler
# ---------------------------------------------------------------------------
def cmd_eval_judge(args: argparse.Namespace) -> int:
    sub = getattr(args, "judge_subcommand", None)
    try:
        import sqlite3 as _sq3
        from tag.eval_judge import ensure_schema, list_judge_runs, get_judge_results
    except ImportError as e:
        print_error(f"eval_judge not available: {e}")
        return 1
    cfg, profile = _load_cfg_and_profile(args)
    db_path = _db_for_profile(profile, cfg)
    conn = _sq3.connect(str(db_path))
    ensure_schema(conn)
    if sub == "list" or sub is None:
        run_id = getattr(args, "eval_run_id", None)
        runs = list_judge_runs(conn, eval_run_id=run_id, limit=getattr(args, "limit", 20))
        if getattr(args, "json", False):
            print(json.dumps([vars(r) if hasattr(r, "__dict__") else dict(r) for r in runs], indent=2, default=str))
        else:
            for r in runs:
                print(r)
        return 0
    if sub == "run":
        from tag.eval_judge import run_judge_on_eval
        result = run_judge_on_eval(conn, args.eval_run_id,
                                   judge_model=getattr(args, "judge_model", "claude-sonnet-4-6"),
                                   criteria=getattr(args, "criteria", None),
                                   cfg=cfg)
        if getattr(args, "json", False):
            print(json.dumps(
                result,
                default=lambda o: vars(o) if hasattr(o, "__dict__") else str(o),
                indent=2,
            ))
        else:
            print(result)
        return 0
    print_error(f"Unknown subcommand: {sub!r}")
    return 1


# ---------------------------------------------------------------------------
# PRD-049: eval dataset command handler
# ---------------------------------------------------------------------------
def cmd_eval_dataset(args: argparse.Namespace) -> int:
    sub = getattr(args, "dataset_subcommand", None)
    try:
        import sqlite3 as _sq3
        from tag.eval_datasets import ensure_schema, create_dataset, list_datasets, get_dataset, export_to_yaml, delete_dataset
    except ImportError as e:
        print_error(f"eval_datasets not available: {e}")
        return 1
    cfg, profile = _load_cfg_and_profile(args)
    db_path = _db_for_profile(profile, cfg)
    conn = _sq3.connect(str(db_path))
    ensure_schema(conn)
    if sub == "create":
        ds = create_dataset(conn, args.name, getattr(args, "description", ""))
        print(f"Created dataset '{ds.name}' (id={ds.id}, v{ds.version})")
        return 0
    if sub == "list" or sub is None:
        datasets = list_datasets(conn)
        if getattr(args, "json", False):
            print(json.dumps([vars(d) if hasattr(d, "__dict__") else dict(d) for d in datasets], indent=2, default=str))
        else:
            for d in datasets:
                print(f"{d.name:<40} v{d.version}")
        return 0
    if sub == "export":
        ds = get_dataset(conn, args.name)
        if ds is None:
            print_error(f"Dataset not found: {args.name!r}")
            return 1
        yaml_str = export_to_yaml(conn, ds.id)
        out = getattr(args, "out", None)
        if out:
            Path(out).write_text(yaml_str)
            print(f"Exported to {out}")
        else:
            print(yaml_str)
        return 0
    if sub == "delete":
        ds = get_dataset(conn, args.name)
        if ds is None:
            print_error(f"Dataset not found: {args.name!r}")
            return 1
        delete_dataset(conn, ds.id)
        print(f"Deleted dataset '{args.name}'")
        return 0
    print_error(f"Unknown subcommand: {sub!r}")
    return 1


# ---------------------------------------------------------------------------
# PRD-047: eval CI command handler
# ---------------------------------------------------------------------------
def cmd_eval_ci(args: argparse.Namespace) -> int:
    sub = getattr(args, "evci_subcommand", None)
    try:
        from tag.eval_ci import run_eval_ci, scaffold_github_action, install_github_action
    except ImportError as e:
        print_error(f"eval_ci not available: {e}")
        return 1
    if sub == "scaffold" or sub is None:
        wf_type = getattr(args, "type", "eval")
        yaml_str = scaffold_github_action(wf_type)
        out = getattr(args, "out", None)
        if out:
            Path(out).write_text(yaml_str)
            print(f"Wrote {out}")
        else:
            print(yaml_str)
        return 0
    if sub == "run":
        import sqlite3 as _sq3
        cfg, profile = _load_cfg_and_profile(args)
        db_path = _db_for_profile(profile, cfg)
        conn = _sq3.connect(str(db_path))
        result = run_eval_ci(conn, args.suite, profile, cfg,
                             threshold=getattr(args, "threshold", 0.8),
                             post_comment=getattr(args, "post_comment", False))
        print(result)
        return 0 if result.passed else 1
    print_error(f"Unknown subcommand: {sub!r}")
    return 1


# ---------------------------------------------------------------------------
# PRD-050: alert command handler
# ---------------------------------------------------------------------------
def cmd_alert(args: argparse.Namespace) -> int:
    sub = getattr(args, "alert_subcommand", None)
    try:
        import sqlite3 as _sq3
        from tag.alerts import (ensure_schema, create_rule, list_rules, delete_rule,
                                 check_alerts, compute_metric_snapshot, get_recent_firings)
    except ImportError as e:
        print_error(f"alerts not available: {e}")
        return 1
    cfg, profile = _load_cfg_and_profile(args)
    db_path = _db_for_profile(profile, cfg)
    conn = _sq3.connect(str(db_path))
    ensure_schema(conn)
    if sub == "create":
        rule = create_rule(conn, args.name, args.metric, args.condition,
                           args.threshold, args.severity,
                           profile=getattr(args, "profile", None))
        print(f"Created rule '{rule.name}' (id={rule.id})")
        return 0
    if sub == "list" or sub is None:
        rules = list_rules(conn)
        if getattr(args, "json", False):
            print(json.dumps([vars(r) if hasattr(r, "__dict__") else dict(r) for r in rules], indent=2, default=str))
        else:
            for r in rules:
                print(f"{r.id[:8]}  {r.name:<30} {r.metric} {r.condition} {r.threshold} [{r.severity}]")
        return 0
    if sub == "check":
        snapshot = compute_metric_snapshot(conn, profile=getattr(args, "profile", None))
        firings = check_alerts(conn, snapshot)
        if getattr(args, "json", False):
            print(json.dumps([vars(f) if hasattr(f, "__dict__") else dict(f) for f in firings], indent=2, default=str))
        else:
            if firings:
                for f in firings:
                    print(f"[{f.severity.upper()}] {f.rule_name}: {f.actual_value:.4f} {f.condition} {f.threshold}")
            else:
                print("No alerts firing")
        return 0
    if sub == "firings":
        firings = get_recent_firings(conn, limit=getattr(args, "limit", 20))
        if getattr(args, "json", False):
            print(json.dumps(
                [vars(f) if hasattr(f, "__dict__") else dict(f) for f in firings],
                indent=2, default=str,
            ))
            return 0
        for f in firings:
            print(f"[{f.severity}] {f.rule_name}: {f.actual_value:.4f} at {f.fired_at}")
        return 0
    if sub == "delete":
        ok = delete_rule(conn, args.rule_id)
        print("Deleted" if ok else "Not found")
        return 0 if ok else 1
    print_error(f"Unknown subcommand: {sub!r}")
    return 1


# ---------------------------------------------------------------------------
# PRD-051: annotation queue command handler
# ---------------------------------------------------------------------------
def cmd_annotate(args: argparse.Namespace) -> int:
    sub = getattr(args, "annotate_subcommand", None)
    try:
        import sqlite3 as _sq3
        from tag.annotation_queue import (ensure_schema, enqueue, dequeue,
                                           submit_label, skip_task, queue_stats, export_labeled)
    except ImportError as e:
        print_error(f"annotation_queue not available: {e}")
        return 1
    cfg, profile = _load_cfg_and_profile(args)
    db_path = _db_for_profile(profile, cfg)
    conn = _sq3.connect(str(db_path))
    ensure_schema(conn)
    if sub == "next" or sub is None:
        batch = getattr(args, "batch", 1)
        tasks = dequeue(conn, assigned_to=getattr(args, "assignee", None), limit=batch)
        if not tasks:
            print("Queue is empty")
        for t in tasks:
            print(f"[{t.id}] {t.task_type}:{t.source_id}\n  {t.question}\n  Content: {t.content[:200]}")
        return 0
    if sub == "label":
        ok = submit_label(conn, args.task_id, args.label, notes=getattr(args, "notes", None))
        print("Labeled" if ok else "Task not found")
        return 0 if ok else 1
    if sub == "stats":
        stats = queue_stats(conn)
        print(json.dumps(stats, indent=2))
        return 0
    if sub == "export":
        fmt = getattr(args, "format", "jsonl")
        data = export_labeled(conn, format=fmt)
        out = getattr(args, "out", None)
        if out:
            Path(out).write_text(data)
            print(f"Exported to {out}")
        else:
            print(data)
        return 0
    print_error(f"Unknown subcommand: {sub!r}")
    return 1


# ---------------------------------------------------------------------------
# PRD-052: prompt hub command handler
# ---------------------------------------------------------------------------
def cmd_prompt_hub(args: argparse.Namespace) -> int:
    sub = getattr(args, "prompt_subcommand", None)
    try:
        import sqlite3 as _sq3
        from tag.prompt_hub import (ensure_schema, save_prompt, get_prompt,
                                     list_prompts, list_versions, diff_versions)
    except ImportError as e:
        print_error(f"prompt_hub not available: {e}")
        return 1
    cfg, profile = _load_cfg_and_profile(args)
    db_path = _db_for_profile(profile, cfg)
    conn = _sq3.connect(str(db_path))
    ensure_schema(conn)
    if sub == "save":
        p = Path(args.file)
        if not p.exists():
            print_error(f"Prompt file not found: {args.file}")
            return 1
        content = p.read_text()
        pv = save_prompt(conn, args.name, content, message=getattr(args, "notes", None))
        print(f"Saved '{pv.name}' v{pv.version} (id={pv.id})")
        return 0
    if sub == "get":
        pv = get_prompt(conn, args.name, version=getattr(args, "version", None))
        if pv is None:
            print_error(f"Prompt not found: {args.name!r}")
            return 1
        print(pv.content)
        return 0
    if sub == "list" or sub is None:
        prompts = list_prompts(conn)
        if getattr(args, "json", False):
            print(json.dumps([vars(p) if hasattr(p, "__dict__") else dict(p) for p in prompts], indent=2, default=str))
        else:
            for p in prompts:
                print(f"{p.name:<40} v{p.version}")
        return 0
    if sub == "diff":
        diff = diff_versions(conn, args.name, args.v1, args.v2)
        print(diff)
        return 0
    print_error(f"Unknown subcommand: {sub!r}")
    return 1


# ---------------------------------------------------------------------------
# PRD-054: devui command handler
# ---------------------------------------------------------------------------
def cmd_devui(args: argparse.Namespace) -> int:
    try:
        from tag.devui import DevUIServer
    except ImportError as e:
        print_error(f"devui not available: {e}")
        return 1
    cfg, profile = _load_cfg_and_profile(args)
    db_path = _db_for_profile(profile, cfg)
    port = getattr(args, "port", 7777)
    host = getattr(args, "host", "127.0.0.1")
    server = DevUIServer(db_path=str(db_path), host=host, port=port)
    if getattr(args, "open_browser", False):
        import webbrowser
        webbrowser.open(f"http://{host}:{port}")
    print(f"DevUI running at http://{host}:{port} — Ctrl+C to stop")
    server.start()  # blocking
    return 0


# ---------------------------------------------------------------------------
# PRD-055: issue solver command handler
# ---------------------------------------------------------------------------
def cmd_issue_solve(args: argparse.Namespace) -> int:
    try:
        from tag.issue_solver import fetch_issue, solve_issue, IssuePlatform
    except ImportError as e:
        print_error(f"issue_solver not available: {e}")
        return 1
    cfg, profile = _load_cfg_and_profile(args)
    ref = args.issue_ref
    # Auto-detect platform from ref if not specified
    platform = getattr(args, "platform", None)
    if platform is None:
        if ref.startswith("github:") or "github.com" in ref:
            platform = "github"
        elif ref.startswith("linear:"):
            platform = "linear"
        else:
            platform = "github"
    issue = fetch_issue(platform, ref, repo=getattr(args, "repo", None), token=None)
    result = solve_issue(issue, profile, cfg,
                         auto_pr=getattr(args, "auto_pr", False),
                         dry_run=getattr(args, "dry_run", False))
    print(result)
    return 0


# ---------------------------------------------------------------------------
# PRD-056: webhook server command handler
# ---------------------------------------------------------------------------
def cmd_webhook_server(args: argparse.Namespace) -> int:
    sub = getattr(args, "hooks_subcommand", None)
    try:
        import sqlite3 as _sq3
        from tag.webhook_server import (ensure_schema, create_rule as wh_create_rule,
                                         list_rules as wh_list_rules, list_events, WebhookServer)
    except ImportError as e:
        print_error(f"webhook_server not available: {e}")
        return 1
    cfg, profile = _load_cfg_and_profile(args)
    db_path = _db_for_profile(profile, cfg)
    conn = _sq3.connect(str(db_path))
    ensure_schema(conn)
    if sub == "listen" or sub is None:
        port = getattr(args, "port", 8765)
        host = getattr(args, "host", "127.0.0.1")
        server = WebhookServer(conn=conn, host=host, port=port, cfg=cfg)
        print(f"Webhook server listening on {host}:{port} — Ctrl+C to stop")
        server.serve_forever()
        return 0
    if sub == "rule-add":
        rule = wh_create_rule(conn, args.platform, args.event, args.profile,
                              getattr(args, "action", "run"))
        print(f"Rule created: {rule.id}")
        return 0
    if sub == "rule-list":
        rules = wh_list_rules(conn)
        for r in rules:
            print(f"{r.id[:8]}  {r.platform:<10} {r.event:<30} {r.action}")
        return 0
    if sub == "events":
        events = list_events(conn, limit=getattr(args, "limit", 20))
        for e in events:
            print(e)
        return 0
    print_error(f"Unknown subcommand: {sub!r}")
    return 1


# ---------------------------------------------------------------------------
# PRD-057/058/059/060/061/062/063: ci extensions command handler
# ---------------------------------------------------------------------------
def cmd_ci_ext(args: argparse.Namespace) -> int:
    sub = getattr(args, "ci_subcommand", None)
    try:
        from tag.ci import (detect_test_framework, generate_tests,
                             scaffold_github_action as ci_scaffold,
                             install_github_action,
                             parse_sarif, fix_sarif_vulns,
                             parse_ci_failure, diagnose_and_fix,
                             review_pr_with_signals,
                             generate_gitlab_pipeline, write_gitlab_pipeline,
                             detect_flaky_tests, run_flaky_fix_session)
    except ImportError as e:
        print_error(f"ci not available: {e}")
        return 1
    cfg, profile = _load_cfg_and_profile(args)
    if sub == "test-gen":
        diff_arg = getattr(args, "diff", None)
        diff = ""
        if diff_arg:
            p = Path(diff_arg)
            diff = p.read_text() if p.exists() else diff_arg
        out = generate_tests(diff, profile, cfg, out=getattr(args, "out", None))
        print(out)
        return 0
    if sub == "install-action":
        wf_type = getattr(args, "type", "eval")
        out_dir = Path(getattr(args, "out_dir", ".github/workflows"))
        path = install_github_action(wf_type, out_dir)
        print(f"Installed {path}")
        return 0
    if sub == "fix-vuln":
        sarif_path = Path(args.sarif)
        vulns = parse_sarif(sarif_path)
        if not vulns:
            print("No vulnerabilities found")
            return 0
        result = fix_sarif_vulns(vulns, profile, cfg, dry_run=getattr(args, "dry_run", False))
        print(result)
        return 0
    if sub in ("diagnose", "ci-diagnose"):
        log_text = Path(args.log).read_text()
        failure = parse_ci_failure(log_text)
        result = diagnose_and_fix(failure, profile, cfg, dry_run=getattr(args, "dry_run", False))
        print(result)
        return 0
    if sub == "review":
        result = review_pr_with_signals(args.pr_ref, profile, cfg,
                                        signals=getattr(args, "signals", None))
        print(result)
        return 0
    if sub == "gen-pipeline":
        repo_path = Path(getattr(args, "repo", "."))
        out_path = Path(getattr(args, "out", ".gitlab-ci.yml"))
        write_gitlab_pipeline(repo_path, out_path, profile, cfg)
        print(f"Written {out_path}")
        return 0
    if sub == "flaky-fix":
        log_path = Path(args.log)
        result = run_flaky_fix_session(log_path, profile, cfg,
                                       dry_run=getattr(args, "dry_run", False))
        print(result)
        return 0
    print_error(f"Unknown ci subcommand: {sub!r}")
    return 1


# ---------------------------------------------------------------------------
# PRD-064: SWE-agent harness command handler
# ---------------------------------------------------------------------------
def cmd_swe_solve(args: argparse.Namespace) -> int:
    try:
        from tag.swe_harness import run_swe_session
    except ImportError as e:
        print_error(f"swe_harness not available: {e}")
        return 1
    cfg, profile = _load_cfg_and_profile(args)
    repo = getattr(args, "repo", ".")
    result = run_swe_session(args.task, profile, cfg,
                             working_dir=repo,
                             max_turns=getattr(args, "max_turns", 20),
                             dry_run=getattr(args, "dry_run", False))
    print(result)
    return 0


# ---------------------------------------------------------------------------
# PRD-065/068: memory extras command handler
# ---------------------------------------------------------------------------
def cmd_mem_ext(args: argparse.Namespace) -> int:
    sub = getattr(args, "mem_subcommand", None)
    cfg, profile = _load_cfg_and_profile(args)
    profile = getattr(args, "profile", None) or profile

    if sub == "gc":
        try:
            import sqlite3 as _sq3
            from tag.memory_gc import run_gc, run_gc_all_profiles, GCConfig
        except ImportError as e:
            print_error(f"memory_gc not available: {e}")
            return 1
        db_path = _db_for_profile(profile, cfg)
        conn = _sq3.connect(str(db_path))
        config = GCConfig(dry_run=getattr(args, "dry_run", False))
        if getattr(args, "all_profiles", False):
            results = run_gc_all_profiles(conn, config=config)
            for r in results:
                print(f"{r.profile}: evicted={r.evicted_count} merged={r.merged_count} promoted={r.promoted_count}")
        else:
            result = run_gc(conn, profile, config=config)
            print(f"GC done: evicted={result.evicted_count} merged={result.merged_count} promoted={result.promoted_count}")
        return 0

    if sub == "extract":
        try:
            import sqlite3 as _sq3
            from tag.memory_extractor import auto_extract_post_run
        except ImportError as e:
            print_error(f"memory_extractor not available: {e}")
            return 1
        db_path = _db_for_profile(profile, cfg)
        conn = _sq3.connect(str(db_path))
        # Read run output from the runs table
        row = conn.execute(
            "SELECT output FROM runs WHERE id=? LIMIT 1", (args.run_id,)
        ).fetchone()
        if not row:
            print_error(f"Run not found: {args.run_id!r}")
            return 1
        memories = auto_extract_post_run(conn, args.run_id, row[0], profile, cfg)
        print(f"Extracted {len(memories)} memories")
        return 0

    if sub == "tier":
        try:
            import sqlite3 as _sq3
            from tag.semantic_memory import ensure_schema, list_memories_by_tier, MEMORY_TIERS, ensure_tier_schema
        except ImportError as e:
            print_error(f"semantic_memory not available: {e}")
            return 1
        db_path = _db_for_profile(profile, cfg)
        conn = _sq3.connect(str(db_path))
        ensure_schema(conn)
        ensure_tier_schema(conn)
        tier_filter = getattr(args, "tier", None)
        tiers = [tier_filter] if tier_filter else list(MEMORY_TIERS.keys())
        for tier in tiers:
            memories = list_memories_by_tier(conn, profile, tier)
            print(f"\n=== {tier.upper()} ({len(memories)}) ===")
            for m in memories[:10]:
                print(f"  [{m.get('id','')[:8]}] {m.get('content','')[:80]}")
        return 0

    if sub == "fact":
        try:
            import sqlite3 as _sq3
            from tag.semantic_memory import (ensure_schema, ensure_temporal_schema,
                                              update_fact, get_fact_history, list_facts_at)
        except ImportError as e:
            print_error(f"semantic_memory temporal not available: {e}")
            return 1
        db_path = _db_for_profile(profile, cfg)
        conn = _sq3.connect(str(db_path))
        ensure_schema(conn)
        ensure_temporal_schema(conn)
        action = args.action
        if action == "update":
            if not getattr(args, "fact_id", None) or not getattr(args, "content", None):
                print_error("--id and --content required for fact update")
                return 1
            new_id = update_fact(conn, args.fact_id, args.content, profile=profile)
            print(f"Updated fact, new id={new_id}")
        elif action == "history":
            hist = get_fact_history(conn, args.fact_id)
            print(json.dumps(hist, indent=2, default=str))
        elif action == "list-at":
            facts = list_facts_at(conn, profile, at=getattr(args, "at", None))
            print(json.dumps(facts, indent=2, default=str))
        return 0

    if sub == "episode":
        try:
            import sqlite3 as _sq3
            from tag.semantic_memory import (ensure_schema, ensure_episode_schema,
                                              start_episode, end_episode,
                                              list_episodes, get_episode_memories)
        except ImportError as e:
            print_error(f"semantic_memory episodes not available: {e}")
            return 1
        db_path = _db_for_profile(profile, cfg)
        conn = _sq3.connect(str(db_path))
        ensure_schema(conn)
        ensure_episode_schema(conn)
        action = args.action
        if action == "start":
            ep_id = start_episode(conn, profile, "CLI session")
            print(f"Episode started: {ep_id}")
        elif action == "end":
            if not getattr(args, "episode_id", None):
                print_error("--id required")
                return 1
            end_episode(conn, args.episode_id, summary=getattr(args, "summary", None))
            print("Episode ended")
        elif action == "list":
            episodes = list_episodes(conn, profile)
            for ep in episodes:
                print(ep)
        elif action == "get":
            memories = get_episode_memories(conn, args.episode_id)
            print(json.dumps(memories, indent=2, default=str))
        return 0

    if sub == "store":
        try:
            import sqlite3 as _sq3
            from tag.semantic_memory import (ensure_schema, ensure_vector_schema,
                                              search_by_vector, rebuild_embeddings, store_embedding)
        except ImportError as e:
            print_error(f"semantic_memory vectors not available: {e}")
            return 1
        db_path = _db_for_profile(profile, cfg)
        conn = _sq3.connect(str(db_path))
        ensure_schema(conn)
        ensure_vector_schema(conn)
        action = args.action
        if action == "search":
            q = getattr(args, "query", None) or ""
            results = search_by_vector(conn, profile, q)
            print(json.dumps(results[:10], indent=2, default=str))
        elif action == "rebuild":
            rebuild_embeddings(conn, profile)
            print("Embeddings rebuilt")
        else:
            print_error(f"Unknown store action: {action!r}")
            return 1
        return 0

    print_error(f"Unknown mem subcommand: {sub!r}. Use: gc, extract, tier, fact, episode, store")
    return 1


# ---------------------------------------------------------------------------
# PRD-070: entity graph command handler
# ---------------------------------------------------------------------------
def cmd_entity_graph(args: argparse.Namespace) -> int:
    sub = getattr(args, "graph_subcommand", None)
    try:
        import sqlite3 as _sq3
        from tag.entity_graph import (ensure_schema, query_graph, format_graph_summary,
                                       get_entity_neighbors, extract_entities_from_memory,
                                       add_entity, detect_communities)
        from tag.semantic_memory import ensure_schema as sm_ensure, list_memories
    except ImportError as e:
        print_error(f"entity_graph not available: {e}")
        return 1
    cfg, profile = _load_cfg_and_profile(args)
    profile = getattr(args, "profile", None) or profile
    db_path = _db_for_profile(profile, cfg)
    conn = _sq3.connect(str(db_path))
    ensure_schema(conn)
    if sub == "show" or sub is None:
        summary = format_graph_summary(conn, profile)
        if getattr(args, "json", False):
            graph = query_graph(conn, profile)
            print(json.dumps(graph, indent=2, default=str))
        else:
            print(summary)
        return 0
    if sub == "query":
        # query_graph exposes entity_name/entity_type/limit (single-level
        # neighborhood); it has no depth-traversal parameter.
        result = query_graph(conn, profile, entity_name=args.entity)
        print(json.dumps(result, indent=2, default=str))
        return 0
    if sub == "build":
        sm_ensure(conn)
        memories = list_memories(conn, profile)
        count = 0
        for m in memories:
            entities = extract_entities_from_memory(m.get("content", ""), profile)
            for ent in entities:
                add_entity(conn, ent["name"], ent.get("type", "unknown"), profile)
                count += 1
        detect_communities(conn, profile)
        print(f"Built graph from {len(memories)} memories, extracted {count} entities")
        return 0
    print_error(f"Unknown graph subcommand: {sub!r}")
    return 1


# ---------------------------------------------------------------------------
# register(sub): attach all PRD-045 to PRD-072 subcommands to `sub`
# ---------------------------------------------------------------------------
def register(sub: argparse._SubParsersAction) -> None:  # noqa: SLF001
    # ── PRD-046/047: pricing ────────────────────────────────────────────────
    pricing_cmd = sub.add_parser("pricing", help="LLM pricing table (cost-per-span)")
    pricing_sub = pricing_cmd.add_subparsers(dest="pricing_subcommand")
    pr_list = pricing_sub.add_parser("list", help="List all known model prices")
    pr_list.add_argument("--json", action="store_true")
    pr_get = pricing_sub.add_parser("get", help="Get cost for a specific model + token count")
    pr_get.add_argument("model", metavar="MODEL")
    pr_get.add_argument("--input-tokens", type=int, default=1000)
    pr_get.add_argument("--output-tokens", type=int, default=500)
    pr_get.add_argument("--cache-read", action="store_true")
    pr_get.add_argument("--batch", action="store_true")
    for ap in [pricing_cmd, pr_list, pr_get]:
        ap.set_defaults(func=cmd_pricing)

    # ── PRD-045: eval judge ─────────────────────────────────────────────────
    judge_cmd = sub.add_parser("eval-judge", help="LLM-as-judge evaluation")
    judge_sub = judge_cmd.add_subparsers(dest="judge_subcommand")
    judge_run = judge_sub.add_parser("run", help="Run judge on an eval run")
    judge_run.add_argument("eval_run_id", metavar="EVAL_RUN_ID")
    judge_run.add_argument("--profile", default=None)
    judge_run.add_argument("--judge-model", default="claude-sonnet-4-6")
    judge_run.add_argument("--criteria", nargs="+", default=None)
    judge_run.add_argument("--json", action="store_true")
    judge_list = judge_sub.add_parser("list", help="List judge runs")
    judge_list.add_argument("--eval-run-id", default=None)
    judge_list.add_argument("--limit", type=int, default=20)
    judge_list.add_argument("--json", action="store_true")
    for ap in [judge_cmd, judge_run, judge_list]:
        ap.set_defaults(func=cmd_eval_judge)

    # ── PRD-049: eval datasets ──────────────────────────────────────────────
    dataset_cmd = sub.add_parser("eval-dataset", help="Versioned eval dataset management")
    dataset_sub = dataset_cmd.add_subparsers(dest="dataset_subcommand")
    ds_create = dataset_sub.add_parser("create", help="Create a new dataset")
    ds_create.add_argument("name", metavar="NAME")
    ds_create.add_argument("--description", default="")
    ds_list = dataset_sub.add_parser("list", help="List datasets")
    ds_list.add_argument("--json", action="store_true")
    ds_export = dataset_sub.add_parser("export", help="Export dataset to YAML")
    ds_export.add_argument("name", metavar="NAME")
    ds_export.add_argument("--out", default=None)
    ds_delete = dataset_sub.add_parser("delete", help="Delete a dataset")
    ds_delete.add_argument("name", metavar="NAME")
    for ap in [dataset_cmd, ds_create, ds_list, ds_export, ds_delete]:
        ap.set_defaults(func=cmd_eval_dataset)

    # ── PRD-047: eval CI gate ───────────────────────────────────────────────
    evci_cmd = sub.add_parser("eval-ci", help="Eval CI gate and GitHub Action scaffold")
    evci_sub = evci_cmd.add_subparsers(dest="evci_subcommand")
    evci_run = evci_sub.add_parser("run", help="Run eval CI gate")
    evci_run.add_argument("suite", metavar="SUITE_PATH")
    evci_run.add_argument("--profile", default=None)
    evci_run.add_argument("--threshold", type=float, default=0.8)
    evci_run.add_argument("--post-comment", action="store_true")
    evci_scaffold = evci_sub.add_parser("scaffold", help="Scaffold GitHub Action workflow")
    evci_scaffold.add_argument("--type", default="eval", choices=["eval", "review", "test-gen", "fix-vuln"])
    evci_scaffold.add_argument("--out", default=None)
    for ap in [evci_cmd, evci_run, evci_scaffold]:
        ap.set_defaults(func=cmd_eval_ci)

    # ── PRD-050: alert rules ────────────────────────────────────────────────
    alert_cmd = sub.add_parser("alert", help="Alert rules and firing management")
    alert_sub = alert_cmd.add_subparsers(dest="alert_subcommand")
    alert_create = alert_sub.add_parser("create", help="Create an alert rule")
    alert_create.add_argument("name", metavar="NAME")
    alert_create.add_argument("--metric", required=True)
    alert_create.add_argument("--condition", required=True, choices=["lt", "gt", "lte", "gte"])
    alert_create.add_argument("--threshold", type=float, required=True)
    alert_create.add_argument("--severity", default="warning", choices=["info", "warning", "critical"])
    alert_create.add_argument("--profile", default=None)
    alert_list = alert_sub.add_parser("list", help="List alert rules")
    alert_list.add_argument("--json", action="store_true")
    alert_check = alert_sub.add_parser("check", help="Check alerts against current metrics")
    alert_check.add_argument("--profile", default=None)
    alert_check.add_argument("--json", action="store_true")
    alert_firings = alert_sub.add_parser("firings", help="Show recent alert firings")
    alert_firings.add_argument("--limit", type=int, default=20)
    alert_firings.add_argument("--json", action="store_true")
    alert_delete = alert_sub.add_parser("delete", help="Delete an alert rule")
    alert_delete.add_argument("rule_id", metavar="RULE_ID")
    for ap in [alert_cmd, alert_create, alert_list, alert_check, alert_firings, alert_delete]:
        ap.set_defaults(func=cmd_alert)

    # ── PRD-051: annotation queue ───────────────────────────────────────────
    annot_cmd = sub.add_parser("annotate", help="Human annotation queue")
    annot_sub = annot_cmd.add_subparsers(dest="annotate_subcommand")
    annot_next = annot_sub.add_parser("next", help="Get next task to annotate")
    annot_next.add_argument("--assignee", default=None)
    annot_next.add_argument("--batch", type=int, default=1)
    annot_label = annot_sub.add_parser("label", help="Submit a label for a task")
    annot_label.add_argument("task_id", metavar="TASK_ID")
    annot_label.add_argument("label", metavar="LABEL")
    annot_label.add_argument("--notes", default=None)
    annot_stats = annot_sub.add_parser("stats", help="Show annotation queue statistics")
    annot_export = annot_sub.add_parser("export", help="Export labeled tasks")
    annot_export.add_argument("--format", default="jsonl", choices=["jsonl", "csv"])
    annot_export.add_argument("--out", default=None)
    for ap in [annot_cmd, annot_next, annot_label, annot_stats, annot_export]:
        ap.set_defaults(func=cmd_annotate)

    # ── PRD-052: prompt hub ─────────────────────────────────────────────────
    prompt_cmd = sub.add_parser("prompt", help="Prompt versioning hub")
    prompt_sub = prompt_cmd.add_subparsers(dest="prompt_subcommand")
    prompt_save = prompt_sub.add_parser("save", help="Save a new prompt version")
    prompt_save.add_argument("name", metavar="NAME")
    prompt_save.add_argument("file", metavar="FILE")
    prompt_save.add_argument("--notes", default=None)
    prompt_get = prompt_sub.add_parser("get", help="Get latest prompt version")
    prompt_get.add_argument("name", metavar="NAME")
    prompt_get.add_argument("--version", type=int, default=None)
    prompt_list = prompt_sub.add_parser("list", help="List saved prompts")
    prompt_list.add_argument("--json", action="store_true")
    prompt_diff = prompt_sub.add_parser("diff", help="Diff two versions of a prompt")
    prompt_diff.add_argument("name", metavar="NAME")
    prompt_diff.add_argument("v1", type=int, metavar="V1")
    prompt_diff.add_argument("v2", type=int, metavar="V2")
    for ap in [prompt_cmd, prompt_save, prompt_get, prompt_list, prompt_diff]:
        ap.set_defaults(func=cmd_prompt_hub)

    # ── PRD-054: devui ──────────────────────────────────────────────────────
    devui_p = sub.add_parser("devui", help="Local browser DevUI dashboard")
    devui_p.add_argument("--port", type=int, default=7777)
    devui_p.add_argument("--host", default="127.0.0.1")
    devui_p.add_argument("--open", action="store_true", dest="open_browser")
    devui_p.add_argument("--profile", default=None)
    devui_p.set_defaults(func=cmd_devui)

    # ── PRD-055: issue solver ───────────────────────────────────────────────
    issue_cmd = sub.add_parser("issue-solve", help="Agentic issue-to-PR solver")
    issue_cmd.add_argument("issue_ref", metavar="ISSUE_REF", help="e.g. github:owner/repo#42 or linear:PROJ-123")
    issue_cmd.add_argument("--platform", default=None, choices=["github", "linear"])
    issue_cmd.add_argument("--profile", default=None)
    issue_cmd.add_argument("--auto-pr", action="store_true")
    issue_cmd.add_argument("--dry-run", action="store_true")
    issue_cmd.add_argument("--repo", default=None)
    issue_cmd.set_defaults(func=cmd_issue_solve)

    # ── PRD-056: webhook server ─────────────────────────────────────────────
    wh_cmd = sub.add_parser("webhook", help="Webhook server for CI/CD automation (PRD-056)")
    wh_sub = wh_cmd.add_subparsers(dest="hooks_subcommand")
    wh_listen = wh_sub.add_parser("listen", help="Start webhook server")
    wh_listen.add_argument("--port", type=int, default=8765)
    wh_listen.add_argument("--host", default="127.0.0.1")
    wh_listen.add_argument("--profile", default=None)
    wh_rule_add = wh_sub.add_parser("rule-add", help="Add a trigger rule")
    wh_rule_add.add_argument("--platform", required=True, choices=["github", "linear", "slack"])
    wh_rule_add.add_argument("--event", required=True)
    wh_rule_add.add_argument("--profile", required=True)
    wh_rule_add.add_argument("--action", default="run")
    wh_rule_list = wh_sub.add_parser("rule-list", help="List trigger rules")
    wh_events = wh_sub.add_parser("events", help="List recent webhook events")
    wh_events.add_argument("--limit", type=int, default=20)
    for ap in [wh_cmd, wh_listen, wh_rule_add, wh_rule_list, wh_events]:
        ap.set_defaults(func=cmd_webhook_server)

    # ── PRD-057/058/059/061/062/063: agentic-ci extensions ─────────────────
    aci_cmd = sub.add_parser("agentic-ci", help="Agentic CI: test-gen, SARIF fix, PR review, pipelines")
    aci_sub = aci_cmd.add_subparsers(dest="ci_subcommand")
    aci_testgen = aci_sub.add_parser("test-gen", help="Generate tests from diff")
    aci_testgen.add_argument("--diff", default=None, help="Diff text or path to diff file")
    aci_testgen.add_argument("--profile", default=None)
    aci_testgen.add_argument("--out", default=None)
    aci_action = aci_sub.add_parser("install-action", help="Install GitHub Actions workflow")
    aci_action.add_argument("--type", default="eval", choices=["eval", "review", "test-gen", "fix-vuln"])
    aci_action.add_argument("--out-dir", default=".github/workflows")
    aci_sast = aci_sub.add_parser("fix-vuln", help="Auto-remediate SARIF vulnerabilities")
    aci_sast.add_argument("sarif", metavar="SARIF_FILE")
    aci_sast.add_argument("--profile", default=None)
    aci_sast.add_argument("--dry-run", action="store_true")
    aci_diag = aci_sub.add_parser("ci-diagnose", help="Diagnose and auto-fix CI failures")
    aci_diag.add_argument("log", metavar="LOG_FILE")
    aci_diag.add_argument("--profile", default=None)
    aci_diag.add_argument("--dry-run", action="store_true")
    aci_review = aci_sub.add_parser("review", help="PR review with signals")
    aci_review.add_argument("pr_ref", metavar="PR_REF", help="e.g. owner/repo#42")
    aci_review.add_argument("--profile", default=None)
    aci_review.add_argument("--signals", nargs="+", default=None)
    aci_pipeline = aci_sub.add_parser("gen-pipeline", help="Generate GitLab CI pipeline")
    aci_pipeline.add_argument("--repo", default=".", metavar="REPO_PATH")
    aci_pipeline.add_argument("--out", default=".gitlab-ci.yml")
    aci_flaky = aci_sub.add_parser("flaky-fix", help="Detect and fix flaky tests")
    aci_flaky.add_argument("log", metavar="LOG_FILE")
    aci_flaky.add_argument("--profile", default=None)
    aci_flaky.add_argument("--dry-run", action="store_true")
    for ap in [aci_cmd, aci_testgen, aci_action, aci_sast, aci_diag, aci_review, aci_pipeline, aci_flaky]:
        ap.set_defaults(func=cmd_ci_ext)

    # ── PRD-064: swe-agent harness ──────────────────────────────────────────
    swe_cmd = sub.add_parser("swe-solve", help="SWE-Agent style agentic task solver")
    swe_cmd.add_argument("task", metavar="TASK", help="Natural language task description")
    swe_cmd.add_argument("--profile", default=None)
    swe_cmd.add_argument("--repo", default=".", metavar="REPO_PATH")
    swe_cmd.add_argument("--max-turns", type=int, default=20)
    swe_cmd.add_argument("--dry-run", action="store_true")
    swe_cmd.set_defaults(func=cmd_swe_solve)

    # ── PRD-065/068: memory+ extensions ────────────────────────────────────
    # NOTE: the top-level `mem2` subparser (gc/extract/tier/fact/episode/store)
    # is registered by tag.cmd.memory.register(), which owns the memory surface.
    # Registering it here too raised `argparse.ArgumentError: conflicting
    # subparser: mem2` on Python 3.11–3.13 (the supported runtimes), crashing
    # build_parser() — and the duplicate aborted every command registered after
    # it (graph, …). The `cmd_mem_ext` handler in this module is retained for
    # backward-compatible imports.

    # ── PRD-070: entity graph ───────────────────────────────────────────────
    graph_cmd = sub.add_parser("graph", help="Entity knowledge graph")
    graph_sub = graph_cmd.add_subparsers(dest="graph_subcommand")
    graph_show = graph_sub.add_parser("show", help="Show entity graph summary")
    graph_show.add_argument("--profile", default=None)
    graph_show.add_argument("--json", action="store_true")
    graph_query = graph_sub.add_parser("query", help="Query graph by entity")
    graph_query.add_argument("entity", metavar="ENTITY")
    graph_query.add_argument("--profile", default=None)
    graph_query.add_argument("--depth", type=int, default=2)
    graph_build = graph_sub.add_parser("build", help="Build graph from existing memories")
    graph_build.add_argument("--profile", default=None)
    for ap in [graph_cmd, graph_show, graph_query, graph_build]:
        ap.set_defaults(func=cmd_entity_graph)
