"""Agent infrastructure and tooling commands."""
from __future__ import annotations

import argparse
import json
import os
import sys
from pathlib import Path
from typing import Any

from tag.core.config import load_config, save_config, config_path
from tag.core.paths import runtime_db_path
from tag.core.db import open_db
from tag.core.utils import nonnegative_int

try:
    from tag.tui_output import print_error, print_success, print_warning
except Exception:
    def print_error(msg): print(f"error: {msg}", file=sys.stderr)
    def print_success(msg): print(msg)
    def print_warning(msg): print(f"warning: {msg}", file=sys.stderr)


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

_DEFAULT_TAG_HOME = Path("~/.tag").expanduser()


def _tag_home() -> Path:
    return Path(os.environ.get("TAG_HOME", str(_DEFAULT_TAG_HOME))).expanduser().resolve()


# ---------------------------------------------------------------------------
# PRD-034: Secret Scanning / Security Audit
# ---------------------------------------------------------------------------

def cmd_security(args: argparse.Namespace) -> int:
    """PRD-034: tag security scan/list."""
    from tag.security import scan_directory, scan_file, record_scan, ensure_schema as sec_ensure
    cfg = load_config(config_path(getattr(args, "config", None)))
    db = open_db(cfg)
    sec_ensure(db)
    sub = getattr(args, "security_subcommand", None)

    if sub == "scan" or sub is None:
        path_str = getattr(args, "path", ".") or "."
        scan_path = Path(path_str).resolve()
        max_files = getattr(args, "max_files", 2000) or 2000

        if not scan_path.exists():
            db.close()
            print_error(f"Path not found: {path_str}")
            return 1

        if scan_path.is_file():
            from tag.security import scan_file as sf
            findings = sf(scan_path)
        else:
            from tag.security import scan_directory as sd
            findings = list(sd(scan_path, max_files=max_files))

        record_scan(db, str(scan_path), findings)
        db.close()

        if getattr(args, "json", False):
            print(json.dumps([
                {"file": str(f.file), "line_no": f.line_no, "pattern": f.pattern_name,
                 "entropy": f.is_entropy}
                for f in findings
            ], indent=2))
            return 1 if findings else 0

        if not findings:
            print(f"✓ No secrets found in {scan_path}")
            return 0

        print(f"⚠ Found {len(findings)} potential secret(s) in {scan_path}:\n")
        for f in findings:
            tag = "[entropy]" if f.is_entropy else f"[{f.pattern_name}]"
            print(f"  {f.file}:{f.line_no}  {tag}")
        print("\nNOTE: Matched values are NOT displayed for security.")
        return 1

    if sub == "list":
        rows = db.execute(
            "SELECT id, scanned_path, finding_count, status, created_at FROM security_scans "
            "ORDER BY created_at DESC LIMIT 20"
        ).fetchall()
        db.close()
        if getattr(args, "json", False):
            data = [{"id": r[0], "path": r[1], "findings": r[2],
                     "status": r[3], "created_at": r[4]} for r in rows]
            print(json.dumps(data, indent=2))
            return 0
        if not rows:
            print("No security scans recorded.")
            return 0
        for r in rows:
            status_icon = "✓" if r[3] == "clean" else "⚠"
            print(f"{status_icon} {r[0][:8]}  {r[1][:60]:<60}  {r[2]} findings  {r[4][:19]}")
        return 0

    db.close()
    print_error(f"Unknown security subcommand: {sub!r}")
    return 1


# ---------------------------------------------------------------------------
# PRD-037: Agent Personas
# ---------------------------------------------------------------------------

def cmd_persona(args: argparse.Namespace) -> int:
    """PRD-037: tag persona list/show/apply/remove/stack."""
    from tag.persona import (
        list_personas, get_persona, apply_persona, remove_active_persona,
        get_active_personas, remove_persona, install_persona, load_persona_file,
        ensure_schema as persona_ensure, build_merged_prompt,
    )
    cfg = load_config(config_path(getattr(args, "config", None)))
    db = open_db(cfg)
    persona_ensure(db)
    sub = getattr(args, "persona_subcommand", None)

    if sub == "list" or sub is None:
        personas = list_personas(db)
        db.close()
        if getattr(args, "json", False):
            print(json.dumps(personas, indent=2))
            return 0
        if not personas:
            print("No personas available.")
            return 0
        for p in personas:
            print(f"{'[builtin]' if p['source'] == 'builtin' else '[user]   ':10} {p['name']:<30}  {p['description'][:50]}")
        return 0

    if sub == "show":
        name = getattr(args, "name", "")
        p = get_persona(db, name)
        db.close()
        if not p:
            print_error(f"Persona not found: {name!r}")
            return 1
        if getattr(args, "json", False):
            print(json.dumps(p, indent=2))
            return 0
        print(f"Name:        {p['name']}")
        print(f"Description: {p['description']}")
        print(f"Inject:      {p['inject']}")
        print(f"Tags:        {', '.join(p.get('tags', []))}")
        print(f"Source:      {p['source']}")
        print(f"\nStyle Prompt:\n{p['style_prompt']}")
        return 0

    if sub == "apply":
        name = getattr(args, "name", "")
        profile = getattr(args, "profile", None) or cfg["defaults"]["master_profile"]
        session_id = getattr(args, "session_id", None)
        try:
            apply_persona(db, profile, name, session_id=session_id)
        except ValueError as exc:
            print_error(str(exc))
            db.close()
            return 1
        db.close()
        print(f"Persona '{name}' applied to profile '{profile}'.")
        return 0

    if sub == "remove":
        name = getattr(args, "name", "")
        profile = getattr(args, "profile", None) or cfg["defaults"]["master_profile"]
        removed = remove_active_persona(db, profile, name)
        db.close()
        if removed:
            print(f"Persona '{name}' removed from profile '{profile}'.")
        else:
            print(f"Persona '{name}' was not active on profile '{profile}'.")
        return 0

    if sub == "stack":
        profile = getattr(args, "profile", None) or cfg["defaults"]["master_profile"]
        personas = get_active_personas(db, profile)
        db.close()
        if not personas:
            print(f"No active personas for profile '{profile}'.")
            return 0
        print(f"Active personas for '{profile}':")
        for p in personas:
            print(f"  [{p.get('position', 0)}] {p['name']} ({p['inject']})")
        return 0

    if sub == "install":
        path_str = getattr(args, "file", "")
        try:
            persona_data = load_persona_file(Path(path_str))
            pid = install_persona(db, persona_data, source="user")
        except (FileNotFoundError, ValueError) as exc:
            print_error(str(exc))
            db.close()
            return 1
        db.close()
        print(f"Persona '{persona_data['name']}' installed ({pid[:8]}).")
        return 0

    if sub == "preview":
        profile = getattr(args, "profile", None) or cfg["defaults"]["master_profile"]
        base_prompt = getattr(args, "base_prompt", "You are a helpful agent.")
        personas = get_active_personas(db, profile)
        db.close()
        merged = build_merged_prompt(base_prompt, personas)
        print(merged)
        return 0

    db.close()
    print_error(f"Unknown persona subcommand: {sub!r}")
    return 1


# ---------------------------------------------------------------------------
# PRD-038: Diff-Aware Context Injection
# ---------------------------------------------------------------------------

def cmd_diff_inject(args: argparse.Namespace) -> int:
    """PRD-038: tag context inject --git-diff / --pr / --staged."""
    from tag.diff_context import build_diff_context, pr_diff_context
    cfg = load_config(config_path(getattr(args, "config", None)))

    pr_num = getattr(args, "pr", None)
    ref = getattr(args, "ref", "HEAD") or "HEAD"
    staged = getattr(args, "staged", False)
    context_lines = getattr(args, "context_lines", 3) or 3
    max_files = getattr(args, "max_files", 10) or 10
    blocked = getattr(args, "blocked", []) or []
    output_only = getattr(args, "output_only", False)

    try:
        if pr_num:
            repo = getattr(args, "repo", None)
            result = pr_diff_context(
                pr_num, repo, context_lines=context_lines,
                max_files=max_files, blocked_patterns=blocked,
            )
        else:
            workdir = Path(getattr(args, "workdir", ".") or ".").resolve()
            result = build_diff_context(
                ref, staged=staged, context_lines=context_lines,
                max_files=max_files, blocked_patterns=blocked, workdir=workdir,
            )
    except RuntimeError as exc:
        if getattr(args, "json", False):
            print(json.dumps({"error": str(exc), "files": [], "content": "", "estimated_tokens": 0}))
        else:
            print_error(str(exc))
        return 1

    if result["warn"]:
        print(f"⚠ Warning: diff context is large ({result['estimated_tokens']:,} estimated tokens).", file=sys.stderr)

    if result["files_skipped"]:
        print(f"Skipped {len(result['files_skipped'])} file(s): {', '.join(result['files_skipped'][:5])}", file=sys.stderr)

    if not result["content"].strip():
        if getattr(args, "json", False):
            print(json.dumps({"files": [], "content": "", "estimated_tokens": 0, "warn": False, "files_included": [], "files_skipped": []}))
        else:
            print("No diff content to inject (no changed files in scope).")
        return 0

    _json = getattr(args, "json", False)
    print(f"Diff context: {len(result['files_included'])} file(s), ~{result['estimated_tokens']:,} tokens",
          file=sys.stderr if _json else sys.stdout)

    if output_only or _json:
        if _json:
            print(json.dumps(result, indent=2))
        else:
            print(result["content"])
        return 0

    # Store in context (writes to a context file that tag submit picks up)
    context_dir = runtime_db_path(cfg).parent / "context"
    context_dir.mkdir(parents=True, exist_ok=True)
    ctx_file = context_dir / "diff_context.md"
    ctx_file.write_text(result["content"])
    print(f"Diff context saved to {ctx_file}")
    return 0


# ---------------------------------------------------------------------------
# PRD-039: Token Budget Enforcement
# ---------------------------------------------------------------------------

def cmd_budget(args: argparse.Namespace) -> int:
    """PRD-039: tag budget set/get/list/remove/check."""
    from tag.budget import (
        set_budget, get_budget, list_budgets, remove_budget, check_budget,
        BudgetExceeded, ensure_schema as budget_ensure,
    )
    cfg = load_config(config_path(getattr(args, "config", None)))
    db = open_db(cfg)
    budget_ensure(db)
    sub = getattr(args, "budget_subcommand", None)

    if sub == "set":
        profile = getattr(args, "profile", None) or cfg["defaults"]["master_profile"]
        max_tokens = getattr(args, "max_tokens", 0)
        period = getattr(args, "period", "daily") or "daily"
        warn_pct = getattr(args, "warn_pct", 0.8)
        try:
            bid = set_budget(db, profile, max_tokens, period=period, warn_pct=warn_pct)
        except ValueError as exc:
            print_error(str(exc))
            db.close()
            return 1
        db.close()
        print(f"Budget set for '{profile}': {max_tokens:,} tokens/{period} (warn at {int(warn_pct*100)}%)")
        return 0

    if sub == "get":
        profile = getattr(args, "profile", None) or cfg["defaults"]["master_profile"]
        budget = get_budget(db, profile)
        db.close()
        if not budget:
            if getattr(args, "json", False):
                print(json.dumps({"profile": profile, "budget": None}))
            else:
                print(f"No budget set for profile '{profile}'.")
            return 0
        if getattr(args, "json", False):
            print(json.dumps(budget, indent=2))
        else:
            print(f"Profile:    {profile}")
            print(f"Period:     {budget['period']}")
            print(f"Max tokens: {budget['max_tokens']:,}")
            print(f"Warn at:    {int(budget['warn_pct']*100)}%")
            print(f"Enabled:    {budget['enabled']}")
        return 0

    if sub == "list":
        budgets = list_budgets(db)
        db.close()
        if getattr(args, "json", False):
            print(json.dumps(budgets, indent=2))
            return 0
        if not budgets:
            print("No token budgets configured.")
            return 0
        for b in budgets:
            status = "✓" if b["enabled"] else "✗"
            print(f"{status} {b['profile']:<30}  {b['max_tokens']:>10,} tokens/{b['period']}")
        return 0

    if sub == "remove":
        profile = getattr(args, "profile", None) or cfg["defaults"]["master_profile"]
        removed = remove_budget(db, profile)
        db.close()
        if removed:
            print(f"Budget removed for '{profile}'.")
        else:
            print(f"No budget found for '{profile}'.")
        return 0

    if sub == "check":
        profile = getattr(args, "profile", None) or cfg["defaults"]["master_profile"]
        try:
            result = check_budget(db, profile)
        except BudgetExceeded as exc:
            db.close()
            print_error(str(exc))
            return 1
        db.close()
        if result.get("budget") is None:
            if getattr(args, "json", False):
                print(json.dumps({"profile": profile, "budget": None, "unlimited": True}))
            else:
                print(f"No budget configured for '{profile}' — unlimited.")
            return 0
        if getattr(args, "json", False):
            print(json.dumps(result, indent=2))
        else:
            used = result.get("used", 0)
            limit = result.get("limit", 0)
            pct = result.get("pct", 0.0)
            warn = result.get("warn", False)
            warn_icon = "⚠" if warn else "✓"
            print(f"{warn_icon} {profile}: {used:,}/{limit:,} tokens ({pct}%) [{result.get('period')}]")
        return 0

    # Default: list
    budgets = list_budgets(db)
    db.close()
    if getattr(args, "json", False):
        print(json.dumps(budgets, indent=2))
        return 0
    if not budgets:
        print("No token budgets configured. Use 'tag budget set' to add one.")
        return 0
    for b in budgets:
        status = "✓" if b["enabled"] else "✗"
        print(f"{status} {b['profile']:<30}  {b['max_tokens']:>10,} tokens/{b['period']}")
    return 0


# ---------------------------------------------------------------------------
# PRD-040: Notification Hooks
# ---------------------------------------------------------------------------

def cmd_notify(args: argparse.Namespace) -> int:
    """PRD-040: tag notify add/list/test/remove/enable/disable."""
    from tag.notifications import (
        add_hook, list_hooks, remove_hook, set_hook_enabled, deliver,
        ensure_schema as notif_ensure, VALID_CHANNELS, VALID_EVENTS,
    )
    cfg = load_config(config_path(getattr(args, "config", None)))
    db = open_db(cfg)
    notif_ensure(db)
    sub = getattr(args, "notify_subcommand", None)

    if sub == "add":
        event = getattr(args, "event", "run.completed") or "run.completed"
        channel = getattr(args, "channel", "desktop") or "desktop"
        profile = getattr(args, "profile", None)
        config_str = getattr(args, "config_json", "{}") or "{}"
        template = getattr(args, "template", "") or ""
        try:
            config_data = json.loads(config_str)
        except json.JSONDecodeError as exc:
            print_error(f"Invalid config JSON: {exc}")
            db.close()
            return 1
        try:
            hook_id = add_hook(db, event, channel, config_data, profile=profile, template=template)
        except ValueError as exc:
            print_error(str(exc))
            db.close()
            return 1
        db.close()
        if getattr(args, "json", False):
            print(json.dumps({"id": hook_id, "channel": channel, "event": event}))
        else:
            print(f"Notification hook added: {hook_id}  ({channel} on {event})")
        return 0

    if sub == "list":
        profile = getattr(args, "profile", None)
        hooks = list_hooks(db, profile=profile)
        db.close()
        if getattr(args, "json", False):
            print(json.dumps(hooks, indent=2))
            return 0
        if not hooks:
            print("No notification hooks configured.")
            return 0
        for h in hooks:
            status = "✓" if h["enabled"] else "✗"
            print(f"{status} {h['id'][:8]}  {h['channel']:<10} {h['event']:<20} profile={h['profile'] or '*'}")
        return 0

    if sub == "test":
        hook_id = getattr(args, "hook_id", "")
        hooks = list_hooks(db)
        hook = next((h for h in hooks if h["id"].startswith(hook_id)), None)
        db.close()
        if not hook:
            print_error(f"Hook not found: {hook_id!r}")
            return 1
        ctx = {
            "run_id": "test-run-001", "profile": "test", "duration": "0s",
            "tokens_used": "0", "cost_usd": "0.00", "status": "completed",
            "error_message": "", "task": "Test notification", "event": "test",
        }
        ok, err = deliver(hook, "test", ctx)
        if ok:
            print(f"✓ Test notification sent via {hook['channel']}.")
        else:
            print_error(f"Delivery failed: {err}")
        return 0 if ok else 1

    if sub == "remove":
        hook_id = getattr(args, "hook_id", "")
        removed = remove_hook(db, hook_id)
        db.close()
        if removed:
            print(f"Hook {hook_id} removed.")
        else:
            print_error(f"Hook not found: {hook_id}")
            return 1
        return 0

    if sub == "enable":
        hook_id = getattr(args, "hook_id", "")
        ok = set_hook_enabled(db, hook_id, True)
        db.close()
        if not ok:
            print_error(f"Hook not found: {hook_id}")
            return 1
        print(f"Hook {hook_id} enabled.")
        return 0

    if sub == "disable":
        hook_id = getattr(args, "hook_id", "")
        ok = set_hook_enabled(db, hook_id, False)
        db.close()
        if not ok:
            print_error(f"Hook not found: {hook_id}")
            return 1
        print(f"Hook {hook_id} disabled.")
        return 0

    db.close()
    print_error(f"Unknown notify subcommand: {sub!r}")
    return 1


# ---------------------------------------------------------------------------
# PRD-042: Architect/Editor Agent Split
# ---------------------------------------------------------------------------

def cmd_split(args: argparse.Namespace) -> int:
    """PRD-042: tag split list/show/plan."""
    from tag.split_agent import (
        create_split_run, get_split_run, list_split_runs,
        save_spec, ChangeSpec, ARCHITECT_SYSTEM, EDITOR_SYSTEM,
        ensure_schema as split_ensure,
    )
    cfg = load_config(config_path(getattr(args, "config", None)))
    db = open_db(cfg)
    split_ensure(db)
    sub = getattr(args, "split_subcommand", None)

    if sub == "list" or sub is None:
        runs = list_split_runs(db)
        db.close()
        if getattr(args, "json", False):
            print(json.dumps(runs, indent=2))
            return 0
        if not runs:
            print("No architect/editor split runs.")
            return 0
        for r in runs:
            print(f"{r['id'][:12]}  {r['status']:<12}  {r['architect_model'][:20]} → {r['editor_model'][:20]}  {r['task'][:50]}")
        return 0

    if sub == "show":
        run_id = getattr(args, "run_id", "")
        run = get_split_run(db, run_id)
        db.close()
        if not run:
            print_error(f"Split run not found: {run_id!r}")
            return 1
        if getattr(args, "json", False):
            print(json.dumps(run, indent=2))
        else:
            print(f"Run:         {run['id']}")
            print(f"Task:        {run['task']}")
            print(f"Architect:   {run['architect_model']}")
            print(f"Editor:      {run['editor_model']}")
            print(f"Status:      {run['status']}")
            print(f"Items:       {run['items_done']}/{run['items_total']} done, {run['items_rejected']} rejected")
            if run.get("items"):
                print("\nItems:")
                for item in run["items"]:
                    icon = {"accepted": "✓", "rejected": "✗", "pending": "○"}.get(item["status"], "?")
                    print(f"  {icon} [{item['action']:8}] {item['file']:40}  {item['description'][:50]}")
        return 0

    if sub == "plan":
        task = (getattr(args, "task", "") or "").strip()
        if not task:
            db.close()
            print_error("task must not be empty")
            return 1
        architect = getattr(args, "architect", "claude-opus-4") or "claude-opus-4"
        editor = getattr(args, "editor", "claude-haiku-4-5") or "claude-haiku-4-5"
        profile = getattr(args, "profile", None) or cfg["defaults"]["master_profile"]
        spec_json_str = getattr(args, "spec_json", None)

        run_id = create_split_run(db, task, architect, editor, profile)

        if spec_json_str:
            try:
                spec = ChangeSpec.from_json(spec_json_str)
                save_spec(db, run_id, spec)
                db.close()
                print(f"Split run created: {run_id}  ({len(spec.items)} items from spec)")
            except (json.JSONDecodeError, KeyError) as exc:
                print_error(f"Invalid spec JSON: {exc}")
                db.close()
                return 1
        else:
            db.close()
            print(f"Split run created: {run_id}")
            print(f"Architect: {architect}  Editor: {editor}")
            print(f"\nArchitect system prompt:\n{ARCHITECT_SYSTEM}")
        return 0

    db.close()
    print_error(f"Unknown split subcommand: {sub!r}")
    return 1


# ---------------------------------------------------------------------------
# PRD-043: Vector-Based Tool Retrieval
# ---------------------------------------------------------------------------

def cmd_tool_retrieval(args: argparse.Namespace) -> int:
    """PRD-043: tag mcp-registry index/search."""
    from tag.tool_retrieval import (
        build_index, search_tools, is_available, keyword_search_tools,
        get_index_stats, ensure_schema as tr_ensure,
    )
    cfg = load_config(config_path(getattr(args, "config", None)))
    db = open_db(cfg)
    tr_ensure(db)
    sub = getattr(args, "tr_subcommand", None)

    persist_dir = runtime_db_path(cfg).parent / "tool_index"
    cache_dir = _tag_home() / ".cache" / "embeddings"

    if sub == "index" or sub is None:
        # Load tools from MCP registry YAML
        mcp_registry_path = _tag_home() / "mcp-registry.yaml"
        tools: list[dict] = []
        if mcp_registry_path.exists():
            import yaml
            try:
                reg = yaml.safe_load(mcp_registry_path.read_text())
                for server_name, server_cfg in (reg or {}).items():
                    for tool in (server_cfg or {}).get("tools", []):
                        tools.append({
                            "name": tool.get("name", ""),
                            "description": tool.get("description", ""),
                            "server": server_name,
                        })
            except Exception as exc:
                print(f"Warning: could not parse MCP registry: {exc}", file=sys.stderr)

        if not is_available():
            print("⚠ chromadb and sentence-transformers not installed.", file=sys.stderr)
            print("  Install with: pip install chromadb sentence-transformers")
            print(f"  Found {len(tools)} tool(s) in MCP registry — index not built.")
            db.close()
            return 0

        count = build_index(tools, persist_dir, cache_dir, conn=db)
        db.close()
        print(f"✓ Tool index built: {count} tools indexed")
        return 0

    if sub == "search":
        query = getattr(args, "query", "")
        top_k = getattr(args, "top_k", 8) or 8
        if not query.strip():
            print_error("Query must not be empty.")
            db.close()
            return 1

        if is_available():
            results = search_tools(query, persist_dir, cache_dir, top_k=top_k)
        else:
            # Fallback: load from registry and do keyword search
            mcp_registry_path = _tag_home() / "mcp-registry.yaml"
            all_tools: list[dict] = []
            if mcp_registry_path.exists():
                import yaml
                try:
                    reg = yaml.safe_load(mcp_registry_path.read_text())
                    for sname, scfg in (reg or {}).items():
                        for t in (scfg or {}).get("tools", []):
                            all_tools.append({"name": t.get("name", ""), "description": t.get("description", ""), "server": sname})
                except Exception:
                    pass
            results = keyword_search_tools(query, all_tools, top_k=top_k)

        db.close()

        if not results:
            if getattr(args, "json", False):
                print(json.dumps([]))
            else:
                print(f"No tools found for query: {query!r}")
            return 0

        if getattr(args, "json", False):
            print(json.dumps(results, indent=2))
        else:
            print(f"Top {len(results)} tools for: {query!r}\n")
            for i, t in enumerate(results, 1):
                print(f"  {i:2}. [{t.get('server', '?'):20}] {t.get('name', ''):<30}  {t.get('description', '')[:60]}")
        return 0

    if sub == "status":
        stats = get_index_stats(db)
        db.close()
        if getattr(args, "json", False):
            print(json.dumps(stats, indent=2))
            return 0
        if not stats.get("built"):
            print("Tool index not built. Run: tag tool-index index")
            return 0
        print(f"Index status:  {stats['tool_count']} tools")
        print(f"Built at:      {stats.get('built_at', 'unknown')}")
        print(f"Backend:       {'chromadb + sentence-transformers' if stats.get('available') else 'keyword fallback'}")
        return 0

    db.close()
    print_error(f"Unknown tool-index subcommand: {sub!r}")
    return 1


# ---------------------------------------------------------------------------
# Parser registration
# ---------------------------------------------------------------------------

def register(sub: argparse._SubParsersAction) -> None:  # type: ignore[type-arg]
    """Register agent infrastructure and tooling subcommands onto *sub*."""

    # ---- PRD-034: security ----
    sec_cmd = sub.add_parser("security", help="Secret scanning and security auditing")
    sec_cmd.add_argument("--json", action="store_true")
    sec_sub = sec_cmd.add_subparsers(dest="security_subcommand")
    sec_scan = sec_sub.add_parser("scan", help="Scan files for secrets")
    sec_scan.add_argument("path", nargs="?", default=".", metavar="PATH")
    sec_scan.add_argument("--max-files", type=int, default=2000)
    sec_scan.add_argument("--json", action="store_true")
    sec_list = sec_sub.add_parser("list", help="List past scan results")
    sec_list.add_argument("--json", action="store_true")
    for sp in [sec_cmd, sec_scan, sec_list]:
        sp.set_defaults(func=cmd_security)

    # ---- PRD-037: persona ----
    persona_cmd = sub.add_parser("persona", help="Agent persona management")
    persona_sub = persona_cmd.add_subparsers(dest="persona_subcommand")
    pa_list = persona_sub.add_parser("list", help="List available personas")
    pa_list.add_argument("--json", action="store_true")
    pa_show = persona_sub.add_parser("show", help="Show persona details")
    pa_show.add_argument("name", metavar="NAME")
    pa_show.add_argument("--json", action="store_true")
    pa_apply = persona_sub.add_parser("apply", help="Apply a persona to a profile")
    pa_apply.add_argument("name", metavar="NAME")
    pa_apply.add_argument("--profile")
    pa_apply.add_argument("--session-id")
    pa_remove = persona_sub.add_parser("remove", help="Remove an active persona from a profile")
    pa_remove.add_argument("name", metavar="NAME")
    pa_remove.add_argument("--profile")
    pa_stack = persona_sub.add_parser("stack", help="Show active persona stack for a profile")
    pa_stack.add_argument("--profile")
    pa_install = persona_sub.add_parser("install", help="Install a persona from a YAML file")
    pa_install.add_argument("file", metavar="FILE")
    pa_preview = persona_sub.add_parser("preview", help="Preview merged system prompt with active personas")
    pa_preview.add_argument("--profile")
    pa_preview.add_argument("--base-prompt", default="You are a helpful agent.")
    for pp in [persona_cmd, pa_list, pa_show, pa_apply, pa_remove, pa_stack, pa_install, pa_preview]:
        pp.set_defaults(func=cmd_persona)

    # ---- PRD-038: diff-context ----
    diff_cmd = sub.add_parser("diff-context", help="Inject git diff context for agent runs")
    diff_cmd.add_argument("--ref", default="HEAD", help="Git ref to diff against")
    diff_cmd.add_argument("--staged", action="store_true", help="Diff staged changes only")
    diff_cmd.add_argument("--pr", type=int, metavar="PR_NUMBER", help="GitHub PR number")
    diff_cmd.add_argument("--repo", help="GitHub repo (owner/repo) for --pr")
    diff_cmd.add_argument("--context-lines", type=int, default=3, dest="context_lines")
    diff_cmd.add_argument("--max-files", type=int, default=10)
    diff_cmd.add_argument("--blocked", action="append", metavar="PATTERN", help="Extra blocked patterns")
    diff_cmd.add_argument("--output-only", action="store_true", help="Print diff content without saving")
    diff_cmd.add_argument("--workdir", default=".")
    diff_cmd.add_argument("--json", action="store_true")
    diff_cmd.set_defaults(func=cmd_diff_inject)

    # ---- PRD-039: budget ----
    budget_cmd = sub.add_parser("budget", help="Per-profile token budget enforcement")
    budget_sub = budget_cmd.add_subparsers(dest="budget_subcommand")
    b_set = budget_sub.add_parser("set", help="Set token budget")
    b_set.add_argument("--profile")
    b_set.add_argument("--max-tokens", type=int, required=True, dest="max_tokens")
    b_set.add_argument("--period", choices=["daily", "weekly", "monthly"], default="daily")
    b_set.add_argument("--warn-pct", type=float, default=0.8, dest="warn_pct")
    b_get = budget_sub.add_parser("get", help="Get token budget for a profile")
    b_get.add_argument("--profile")
    b_get.add_argument("--json", action="store_true")
    b_list = budget_sub.add_parser("list", help="List all token budgets")
    b_list.add_argument("--json", action="store_true")
    b_remove = budget_sub.add_parser("remove", help="Remove token budget")
    b_remove.add_argument("--profile")
    b_check = budget_sub.add_parser("check", help="Check current usage against budget")
    b_check.add_argument("--profile")
    b_check.add_argument("--json", action="store_true")
    for bp in [budget_cmd, b_set, b_get, b_list, b_remove, b_check]:
        bp.set_defaults(func=cmd_budget)

    # ---- PRD-040: notify ----
    notify_cmd = sub.add_parser("notify", help="Notification hooks (Slack, email, desktop)")
    notify_sub = notify_cmd.add_subparsers(dest="notify_subcommand")
    n_add = notify_sub.add_parser("add", help="Add a notification hook")
    n_add.add_argument("--event", default="run.completed")
    n_add.add_argument("--channel", choices=["slack", "email", "desktop", "webhook"], default="desktop")
    n_add.add_argument("--profile")
    n_add.add_argument("--config-json", default="{}", dest="config_json")
    n_add.add_argument("--template", default="")
    n_add.add_argument("--json", action="store_true")
    n_list = notify_sub.add_parser("list", help="List notification hooks")
    n_list.add_argument("--profile")
    n_list.add_argument("--json", action="store_true")
    n_test = notify_sub.add_parser("test", help="Send test notification")
    n_test.add_argument("hook_id", metavar="HOOK_ID")
    n_remove = notify_sub.add_parser("remove", help="Remove a notification hook")
    n_remove.add_argument("hook_id", metavar="HOOK_ID")
    n_enable = notify_sub.add_parser("enable", help="Enable a hook")
    n_enable.add_argument("hook_id", metavar="HOOK_ID")
    n_disable = notify_sub.add_parser("disable", help="Disable a hook")
    n_disable.add_argument("hook_id", metavar="HOOK_ID")
    for np in [notify_cmd, n_add, n_list, n_test, n_remove, n_enable, n_disable]:
        np.set_defaults(func=cmd_notify)

    # ---- PRD-042: split ----
    split_cmd = sub.add_parser("split", help="Architect/Editor agent split execution")
    split_sub = split_cmd.add_subparsers(dest="split_subcommand")
    sp_list = split_sub.add_parser("list", help="List split runs")
    sp_list.add_argument("--json", action="store_true")
    sp_show = split_sub.add_parser("show", help="Show split run details")
    sp_show.add_argument("run_id", metavar="RUN_ID")
    sp_show.add_argument("--json", action="store_true")
    sp_plan = split_sub.add_parser("plan", help="Create a split run plan")
    sp_plan.add_argument("task", metavar="TASK")
    sp_plan.add_argument("--architect", default="claude-opus-4")
    sp_plan.add_argument("--editor", default="claude-haiku-4-5")
    sp_plan.add_argument("--profile")
    sp_plan.add_argument("--spec-json", dest="spec_json", help="Optional pre-built spec JSON")
    for ssp in [split_cmd, sp_list, sp_show, sp_plan]:
        ssp.set_defaults(func=cmd_split)

    # ---- PRD-043: tool-index ----
    tr_cmd = sub.add_parser("tool-index", help="Vector tool retrieval for MCP registry")
    tr_sub = tr_cmd.add_subparsers(dest="tr_subcommand")
    tr_index = tr_sub.add_parser("index", help="Build tool embedding index")
    tr_search = tr_sub.add_parser("search", help="Search tools by query")
    tr_search.add_argument("query", metavar="QUERY")
    tr_search.add_argument("--top-k", type=int, default=8, dest="top_k")
    tr_search.add_argument("--json", action="store_true")
    tr_status = tr_sub.add_parser("status", help="Show tool index status")
    tr_status.add_argument("--json", action="store_true")
    for tp in [tr_cmd, tr_index, tr_search, tr_status]:
        tp.set_defaults(func=cmd_tool_retrieval)
