"""Memory management commands."""
from __future__ import annotations

import argparse
import json
import sqlite3
import sys
from pathlib import Path
from typing import Any

from tag.core.config import load_config, config_path
from tag.core.paths import runtime_db_path, ensure_runtime_dirs
from tag.core.db import (
    open_db,
    journal_save,
    journal_list,
    journal_forget,
    journal_clear,
    journal_to_prompt_prefix,
)
from tag.core.utils import nonnegative_int

try:
    from tag.tui_output import print_error, print_success, print_warning
except Exception:
    def print_error(msg: str) -> None:  # type: ignore[misc]
        print(f"error: {msg}", file=sys.stderr)

    def print_success(msg: str) -> None:  # type: ignore[misc]
        print(msg)

    def print_warning(msg: str) -> None:  # type: ignore[misc]
        print(f"warning: {msg}", file=sys.stderr)


# ---------------------------------------------------------------------------
# Helpers shared by cmd_mem_ext
# ---------------------------------------------------------------------------

def _load_cfg_and_profile(args: argparse.Namespace) -> tuple[dict[str, Any], str]:
    """Load config and resolve active profile from args."""
    cfg = load_config(config_path(getattr(args, "config", None)))
    profile = getattr(args, "profile", None) or cfg.get("defaults", {}).get("master_profile", "default")
    return cfg, profile


def _db_for_profile(profile: str, cfg: dict[str, Any]) -> Path:
    """Return the runtime db path (all profiles share one runtime db)."""
    return runtime_db_path(cfg)


# ---------------------------------------------------------------------------
# memory — passthrough to hermes memory command
# ---------------------------------------------------------------------------

def cmd_memory(args: argparse.Namespace) -> int:
    from tag.controller import cmd_hermes_command
    return cmd_hermes_command(args, "memory")


# ---------------------------------------------------------------------------
# PRD-002: memory-journal
# ---------------------------------------------------------------------------

def cmd_memory_journal(args: argparse.Namespace) -> int:
    """Tag-native cross-session memory journal (key->value facts per profile)."""
    cfg = load_config(config_path(args.config))
    ensure_runtime_dirs(cfg)
    db = open_db(cfg)
    sub = getattr(args, "mj_subcommand", None) or "list"
    profile = getattr(args, "profile", None) or cfg["defaults"]["master_profile"]

    if sub == "save":
        entry_id = journal_save(db, profile, args.key, args.value, ttl_days=getattr(args, "ttl_days", None))
        db.close()
        if getattr(args, "json", False):
            print(json.dumps({"id": entry_id, "profile": profile, "key": args.key}))
        else:
            print(f"saved: {entry_id}")
        return 0

    if sub == "list":
        entries = journal_list(db, profile)
        db.close()
        if getattr(args, "json", False):
            print(json.dumps(entries, indent=2))
            return 0
        if not entries:
            print(f"No memory journal entries for profile '{profile}'.")
            return 0
        for e in entries:
            exp = f" (expires {e['expires_at'][:10]})" if e.get("expires_at") else ""
            print(f"  [{e['id']}] {e['key']}: {e['value']}{exp}")
        return 0

    if sub == "forget":
        deleted = journal_forget(db, args.entry_id)
        db.close()
        if getattr(args, "json", False):
            print(json.dumps({"deleted": deleted}))
        else:
            print("deleted" if deleted else "not found")
        return 0 if deleted else 1

    if sub == "clear":
        if not getattr(args, "confirm", False):
            print("Pass --confirm to clear all journal entries for this profile.")
            db.close()
            return 1
        count = journal_clear(db, profile)
        db.close()
        if getattr(args, "json", False):
            print(json.dumps({"cleared": count}))
        else:
            print(f"cleared {count} entries")
        return 0

    db.close()
    return 0


# ---------------------------------------------------------------------------
# PRD-025: mem — semantic memory with confidence decay
# ---------------------------------------------------------------------------

def cmd_memory_semantic(args: argparse.Namespace) -> int:
    """PRD-025: Semantic memory with confidence decay and FTS search."""
    cfg = load_config(config_path(getattr(args, "config", None)))
    ensure_runtime_dirs(cfg)
    db = open_db(cfg)
    profile = getattr(args, "profile", None) or cfg["defaults"]["master_profile"]
    sub = getattr(args, "mem_subcommand", "list")

    try:
        from tag.semantic_memory import (
            add_memory,
            search_memories,
            list_memories,
            forget_memory,
            memory_stats,
            ensure_schema,
        )
    except ImportError as exc:
        db.close()
        print_error(f"tag.semantic_memory not available: {exc}")
        return 1

    ensure_schema(db)

    if sub == "add":
        content = (getattr(args, "content", "") or "").strip()
        if not content:
            db.close()
            print_error("Memory content required (positional argument or --content)")
            return 1
        mtype = getattr(args, "memory_type", "fact") or "fact"
        confidence = getattr(args, "confidence", 1.0) or 1.0
        try:
            mem_id = add_memory(db, profile, content, memory_type=mtype, confidence=confidence)
        except ValueError as exc:
            db.close()
            print_error(str(exc))
            return 1
        db.close()
        if getattr(args, "json", False):
            print(json.dumps({"id": mem_id, "profile": profile}))
        else:
            print(f"Memory saved: {mem_id}")
        return 0

    if sub == "search":
        query = (getattr(args, "query", "") or "").strip()
        if not query:
            db.close()
            print_error("QUERY required")
            return 1
        limit = getattr(args, "limit", 10) or 10
        mtype = getattr(args, "memory_type", None)
        results = search_memories(db, profile, query, limit=limit, memory_type=mtype)
        db.close()
        if getattr(args, "json", False):
            print(json.dumps(results, indent=2))
            return 0
        if not results:
            print(f"No memories found for: {query!r}")
            return 0
        for r in results:
            conf = r["confidence"]
            print(f"[{r['id'][:8]}] ({r['memory_type']} conf={conf:.2f}) {r['content'][:80]}")
        return 0

    if sub == "list":
        limit = getattr(args, "limit", 20) or 20
        mtype = getattr(args, "memory_type", None)
        mems = list_memories(db, profile, memory_type=mtype, limit=limit)
        db.close()
        if getattr(args, "json", False):
            print(json.dumps(mems, indent=2))
            return 0
        if not mems:
            print(f"No memories for profile '{profile}'.")
            return 0
        for m in mems:
            print(f"[{m['id'][:8]}] ({m['memory_type']} conf={m['confidence']:.2f}) {m['content'][:80]}")
        return 0

    if sub == "forget":
        mem_id = getattr(args, "mem_id", None)
        if not mem_id:
            db.close()
            print_error("MEMORY_ID required")
            return 1
        deleted = forget_memory(db, mem_id, profile)
        db.close()
        if not deleted:
            print_error(f"Memory '{mem_id}' not found for profile '{profile}'")
            return 1
        print(f"forgotten: {mem_id}")
        return 0

    if sub == "stats":
        stats = memory_stats(db, profile)
        db.close()
        if getattr(args, "json", False):
            print(json.dumps(stats, indent=2))
            return 0
        print(f"Profile: {profile}  Total memories: {stats['total']}")
        for mtype, info in sorted(stats["by_type"].items()):
            print(f"  {mtype:<12}  count={info['count']}  avg_conf_base={info['avg_confidence_base']:.3f}")
        return 0

    db.close()
    return 0


# ---------------------------------------------------------------------------
# mem2 — advanced memory: gc, extract, tier, fact, episode, store (vectors)
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
                print(f"  [{m.get('id', '')[:8]}] {m.get('content', '')[:80]}")
        return 0

    if sub == "fact":
        try:
            import sqlite3 as _sq3
            from tag.semantic_memory import (
                ensure_schema,
                ensure_temporal_schema,
                update_fact,
                get_fact_history,
                list_facts_at,
            )
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
            from tag.semantic_memory import (
                ensure_schema,
                ensure_episode_schema,
                start_episode,
                end_episode,
                list_episodes,
                get_episode_memories,
            )
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
            from tag.semantic_memory import (
                ensure_schema,
                ensure_vector_schema,
                search_by_vector,
                rebuild_embeddings,
                store_embedding,
            )
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
# register(sub) — attach all four commands to the CLI subparsers
# ---------------------------------------------------------------------------

def register(sub: argparse.Action) -> None:
    # ---- memory (hermes passthrough) ----
    memory = sub.add_parser("memory", help="Run memory inside a TAG profile")
    memory.add_argument("--profile", default="orchestrator", help="TAG profile to use")
    memory.add_argument("hermes_args", nargs=argparse.REMAINDER, metavar="...")
    memory.set_defaults(func=cmd_memory)

    # ---- PRD-002: memory-journal ----
    mj = sub.add_parser("memory-journal", help="Manage TAG's cross-session memory journal")
    mj_sub = mj.add_subparsers(dest="mj_subcommand")

    mj_save = mj_sub.add_parser("save", help="Save a key->value fact")
    mj_save.add_argument("key", help="Fact key (e.g. 'project context')")
    mj_save.add_argument("value", help="Fact value")
    mj_save.add_argument("--profile", help="Profile (default: orchestrator)")
    mj_save.add_argument("--ttl-days", type=int, metavar="N", dest="ttl_days")
    mj_save.add_argument("--json", action="store_true")

    mj_list = mj_sub.add_parser("list", help="List journal entries")
    mj_list.add_argument("--profile", help="Profile (default: orchestrator)")
    mj_list.add_argument("--json", action="store_true")

    mj_forget = mj_sub.add_parser("forget", help="Delete a journal entry by ID")
    mj_forget.add_argument("entry_id", metavar="ID")
    mj_forget.add_argument("--json", action="store_true")

    mj_clear = mj_sub.add_parser("clear", help="Clear all journal entries for a profile")
    mj_clear.add_argument("--profile", help="Profile (default: orchestrator)")
    mj_clear.add_argument("--confirm", action="store_true")
    mj_clear.add_argument("--json", action="store_true")

    for mj_p in [mj, mj_save, mj_list, mj_forget, mj_clear]:
        if "config" not in {a.dest for a in mj_p._actions}:
            mj_p.add_argument("--config", help=argparse.SUPPRESS)
        mj_p.set_defaults(func=cmd_memory_journal)

    # ---- PRD-025: mem (semantic memory) ----
    mem_cmd = sub.add_parser("mem", help="Semantic memory with confidence decay (tag mem)")
    mem_sub = mem_cmd.add_subparsers(dest="mem_subcommand")

    mem_add = mem_sub.add_parser("add", help="Add a memory")
    mem_add.add_argument("content", metavar="CONTENT", help="Memory text")
    mem_add.add_argument(
        "--type",
        dest="memory_type",
        default="fact",
        choices=["fact", "convention", "decision", "gotcha", "other"],
    )
    mem_add.add_argument("--confidence", type=float, default=1.0)
    mem_add.add_argument("--profile")
    mem_add.add_argument("--json", action="store_true")

    mem_search = mem_sub.add_parser("search", help="Full-text search over memories")
    mem_search.add_argument("query", metavar="QUERY")
    mem_search.add_argument("--type", dest="memory_type")
    mem_search.add_argument("--limit", type=int, default=10)
    mem_search.add_argument("--profile")
    mem_search.add_argument("--json", action="store_true")

    mem_list = mem_sub.add_parser("list", help="List memories sorted by effective confidence")
    mem_list.add_argument("--type", dest="memory_type")
    mem_list.add_argument("--limit", type=int, default=20)
    mem_list.add_argument("--profile")
    mem_list.add_argument("--json", action="store_true")

    mem_forget = mem_sub.add_parser("forget", help="Delete a memory by ID")
    mem_forget.add_argument("mem_id", metavar="MEMORY_ID")
    mem_forget.add_argument("--profile")

    mem_stats = mem_sub.add_parser("stats", help="Show memory store statistics")
    mem_stats.add_argument("--profile")
    mem_stats.add_argument("--json", action="store_true")

    for mp in [mem_cmd, mem_add, mem_search, mem_list, mem_forget, mem_stats]:
        mp.set_defaults(func=cmd_memory_semantic)

    # ---- mem2 (advanced memory: gc, extract, tier, fact, episode, store) ----
    mem2_cmd = sub.add_parser("mem2", help="Advanced memory: gc, extract, tier, fact, episodes, vectors")
    mem2_sub = mem2_cmd.add_subparsers(dest="mem_subcommand")

    mem2_gc = mem2_sub.add_parser("gc", help="Run memory garbage collection")
    mem2_gc.add_argument("--profile", default=None)
    mem2_gc.add_argument("--all-profiles", action="store_true")
    mem2_gc.add_argument("--dry-run", action="store_true")

    mem2_extract = mem2_sub.add_parser("extract", help="Extract memories from last run output")
    mem2_extract.add_argument("run_id", metavar="RUN_ID")
    mem2_extract.add_argument("--profile", default=None)

    mem2_tier = mem2_sub.add_parser("tier", help="List memories by tier (core/recall/archival)")
    mem2_tier.add_argument("--profile", default=None)
    mem2_tier.add_argument("--tier", default=None, choices=["core", "recall", "archival"])

    mem2_fact = mem2_sub.add_parser("fact", help="Temporal fact versioning")
    mem2_fact.add_argument("action", choices=["update", "history", "list-at"])
    mem2_fact.add_argument("--id", default=None, dest="fact_id")
    mem2_fact.add_argument("--content", default=None)
    mem2_fact.add_argument("--at", default=None, help="ISO timestamp for list-at")
    mem2_fact.add_argument("--profile", default=None)

    mem2_episode = mem2_sub.add_parser("episode", help="Episodic memory sessions")
    mem2_episode.add_argument("action", choices=["start", "end", "list", "get"])
    mem2_episode.add_argument("--id", default=None, dest="episode_id")
    mem2_episode.add_argument("--summary", default=None)
    mem2_episode.add_argument("--profile", default=None)

    mem2_vector = mem2_sub.add_parser("store", help="Store or search vector embeddings")
    mem2_vector.add_argument("action", choices=["store", "search", "rebuild"])
    mem2_vector.add_argument("--query", default=None)
    mem2_vector.add_argument("--id", default=None, dest="memory_id")
    mem2_vector.add_argument("--profile", default=None)

    for ap in [mem2_cmd, mem2_gc, mem2_extract, mem2_tier, mem2_fact, mem2_episode, mem2_vector]:
        ap.set_defaults(func=cmd_mem_ext)
