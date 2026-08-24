"""Runtime content-guardrail / tripwire commands (PRD-123), Python port.

Command surface (parity with the Go harness):

    tag tripwire list | check | test | history
    tag guardrail runtime list | check | test | history | add | remove

`tripwire` is the canonical spelling; `guardrail runtime` is a thin alias over
the SAME engine plus the config-editing add/remove verbs.

Exit codes: 0 clean (or --exit-zero) · 1 runtime failure · 2 usage · 3 fired.
"""
from __future__ import annotations

import argparse
import json
import sys
from typing import Any

from tag.core.config import config_path, load_config, update_config
from tag.core import guardrail as gr

try:
    from tag.tui_output import print_error
except Exception:  # pragma: no cover
    def print_error(msg: str) -> None:
        print(f"error: {msg}", file=sys.stderr)

EXIT_FINDINGS = 3


# ---------------------------------------------------------------------------
# helpers
# ---------------------------------------------------------------------------

def _cfg(args: argparse.Namespace) -> dict[str, Any]:
    return load_config(config_path(getattr(args, "config", None)))


def _master_profile(cfg: dict[str, Any]) -> str:
    defaults = cfg.get("defaults") or {}
    if isinstance(defaults, dict):
        p = defaults.get("master_profile")
        if isinstance(p, str) and p.strip():
            return p
    return "orchestrator"


def _profile(cfg: dict[str, Any], flag: str | None) -> str:
    return flag if flag else _master_profile(cfg)


def _tripwire_rules(cfg: dict[str, Any], profile: str | None) -> list[gr.Rule]:
    prof = _profile(cfg, profile)
    profs = cfg.get("profiles")
    if isinstance(profs, dict):
        pm = profs.get(prof)
        if isinstance(pm, dict):
            cfgm = pm.get("config")
            if isinstance(cfgm, dict) and isinstance(cfgm.get("tripwire"), dict):
                return gr.parse_layer(cfgm["tripwire"], f"config:profile:{prof}")
    block = cfg.get("tripwire")
    return gr.parse_layer(block if isinstance(block, dict) else None, "config")


def _open_conn(args: argparse.Namespace, cfg: dict[str, Any]):
    from tag.core.db import open_db
    try:
        return open_db(cfg)
    except Exception:  # noqa: BLE001 — persistence is best-effort for read/dry-run
        return None


def _build_processor(args: argparse.Namespace, cfg: dict[str, Any], profile: str | None,
                     session: str) -> gr.Processor | None:
    rules = _tripwire_rules(cfg, profile)
    if not rules:
        return None
    conn = _open_conn(args, cfg)
    # QuietWarn: these commands render every finding themselves.
    return gr.Processor(rules=rules, conn=conn, session_id=session, log=sys.stderr, quiet_warn=True)


def _read_content(text: str, file: str, use_stdin: bool) -> str:
    n = sum(1 for x in (text, file) if x) + (1 if use_stdin else 0)
    if n == 0:
        raise _Usage("supply the content with --text, --file or --stdin")
    if n > 1:
        raise _Usage("--text, --file and --stdin are mutually exclusive")
    if text:
        return text
    if file:
        try:
            with open(file, "r", encoding="utf-8") as fh:
                return fh.read()
        except OSError as exc:
            raise _Usage(f"cannot read --file {file}: {exc}")
    return sys.stdin.read()


class _Usage(Exception):
    """A usage error — maps to exit 2."""


def _json_out(args: argparse.Namespace) -> bool:
    return bool(getattr(args, "json", False))


def _emit_verdict(v: gr.Verdict, exit_zero: bool, json_out: bool) -> int:
    if json_out:
        print(json.dumps(v.to_json(), indent=2, ensure_ascii=False))
    else:
        if v.undecidable:
            print(f"UNDECIDABLE ({v.stage}): {v.reason}")
            print("  the guardrail could not evaluate this content and therefore BLOCKED it (fail-closed)")
        elif v.blocked:
            print(f"TRIPWIRE FIRED ({v.stage}): {v.reason}")
        elif v.interrupted:
            print(f"APPROVAL REQUIRED ({v.stage}): {v.reason}")
        elif v.warned:
            print(f"WARN ({v.stage}): {v.reason}")
        else:
            print(f"clean ({v.stage}): no guardrail rule matched")
        for f in v.findings:
            print(f"  - {f.rule:<24} {f.action:<8} {f.message}")
            if f.excerpt:
                print(f"      at offset {f.offset}: {f.excerpt}")
            if f.threshold > 0:
                print(f"      count {f.count} of {f.threshold}")
    if v.fired() and not exit_zero:
        return EXIT_FINDINGS
    return 0


def _rule_json(r: gr.Rule) -> dict[str, Any]:
    m: dict[str, Any] = {"name": r.name, "type": r.type, "tool": r.tool,
                         "action": r.action, "source": r.source}
    if r.stage:
        m["stage"] = r.stage
    if r.builtin:
        m["builtin"] = r.builtin
    if r.pattern:
        m["pattern"] = r.pattern
    if r.threshold > 0:
        m["threshold"] = r.threshold
    if r.window_seconds > 0:
        m["window"] = r.window_text or gr.format_duration(r.window_seconds)
    if r.message:
        m["message"] = r.message
    return m


# ---------------------------------------------------------------------------
# handlers
# ---------------------------------------------------------------------------

def cmd_tripwire_list(args: argparse.Namespace) -> int:
    try:
        rules = _tripwire_rules(_cfg(args), getattr(args, "profile", None))
    except ValueError as exc:
        print_error(str(exc))
        return 2
    gr.sort_rules(rules)
    if _json_out(args):
        print(json.dumps([_rule_json(r) for r in rules], indent=2, ensure_ascii=False))
        return 0
    if not rules:
        print("no guardrail rules configured")
        print("  add a `tripwire:` block to tag.yaml — the quickest start is `tripwire: {preset: standard}`")
        print(f"  presets: {gr.preset_names()}   builtin detectors: destructive, secrets")
        return 0
    print(f"{len(rules)} guardrail rule(s):")
    for i, r in enumerate(rules):
        det = r.builtin if r.builtin else f"/{r.pattern}/"
        print(f"  {i + 1:>2}. {r.name:<28} {r.action:<9} stage={(r.stage or 'any'):<13} "
              f"tool={r.tool:<10} {det}")
        if r.type == gr.TYPE_TRIPWIRE:
            print(f"      tripwire: threshold={r.threshold} window={r.window_text or gr.format_duration(r.window_seconds)}")
        print(f"      [{r.source}]")
    return 0


def cmd_tripwire_check(args: argparse.Namespace) -> int:
    try:
        st = gr.parse_stage(args.stage)
    except ValueError as exc:
        print_error(str(exc))
        return 2
    try:
        content = _read_content(args.text, args.file, args.stdin)
    except _Usage as exc:
        print_error(str(exc))
        return 2
    try:
        proc = _build_processor(args, _cfg(args), getattr(args, "profile", None), args.session or "")
    except ValueError as exc:
        print_error(str(exc))
        return 2
    if proc is None or not proc.enabled():
        print_error("no guardrail rules are configured, so nothing was checked — add a "
                    "`tripwire:` block to tag.yaml (see `tag tripwire list`) rather than treating this as a pass")
        return 2
    v = proc.scan(st, "", content)
    return _emit_verdict(v, args.exit_zero, _json_out(args))


def cmd_tripwire_test(args: argparse.Namespace) -> int:
    if not (args.tool or "").strip():
        print_error("--tool is required")
        return 2
    tool_args: dict[str, Any] = {}
    s = (args.args or "").strip()
    if s:
        try:
            tool_args = json.loads(s)
        except json.JSONDecodeError as exc:
            print_error(f"--args must be a JSON object: {exc}")
            return 2
        if not isinstance(tool_args, dict):
            print_error("--args must be a JSON object")
            return 2
    try:
        proc = _build_processor(args, _cfg(args), getattr(args, "profile", None), args.session or "")
    except ValueError as exc:
        print_error(str(exc))
        return 2
    if proc is None or not proc.enabled():
        print_error("no guardrail rules are configured, so nothing was checked — add a "
                    "`tripwire:` block to tag.yaml (see `tag tripwire list`)")
        return 2
    v = proc.scan_args(args.tool, tool_args)
    return _emit_verdict(v, args.exit_zero, _json_out(args))


def cmd_tripwire_history(args: argparse.Namespace) -> int:
    cfg = _cfg(args)
    conn = _open_conn(args, cfg)
    if conn is None:
        print_error("could not open the database")
        return 1
    try:
        rows = gr.history(conn, args.limit)
    except Exception as exc:  # noqa: BLE001
        print_error(str(exc))
        return 1
    if _json_out(args):
        print(json.dumps([e.to_json() for e in rows], indent=2, ensure_ascii=False))
        return 0
    if not rows:
        print("no guardrail events recorded yet")
        return 0
    for e in rows:
        verdict = e.action
        if e.blocked:
            verdict = "BLOCK"
        if e.undecidable:
            verdict = "UNDECIDABLE"
        print(f"{e.created_at}  {verdict:<12} {e.stage:<14} {e.rule:<24} {e.tool}")
        if e.detail:
            print(f"    {e.detail}")
    return 0


def cmd_guardrail_add(args: argparse.Namespace) -> int:
    name = (args.name or "").strip()
    if not name:
        print_error("--name is required (it identifies the rule in findings and counters)")
        return 2

    rule: dict[str, Any] = {"name": name}
    for key, val in (("tool", args.tool), ("type", args.type), ("stage", args.stage),
                     ("pattern", args.pattern), ("builtin", args.builtin),
                     ("action", args.action), ("message", args.message), ("window", args.window)):
        if val and str(val).strip():
            rule[key] = val
    if args.threshold and args.threshold > 0:
        rule["threshold"] = args.threshold

    scoped = bool((args.profile or "").strip())
    prof = ""
    path = config_path(getattr(args, "config", None))
    cur = load_config(path)
    if scoped:
        prof = _profile(cur, args.profile)
        try:
            from tag.core.profile import ensure_profile_exists
            ensure_profile_exists(cur, prof)
        except SystemExit as exc:
            print_error(str(exc))
            return 2

    existing = _tripwire_rule_list(cur, prof if scoped else "")
    for e in existing:
        if isinstance(e, dict) and e.get("name") == name:
            print_error(f"a guardrail rule named {name!r} already exists; remove it first or choose another name")
            return 2

    # Validate the WHOLE resulting block through the real loader before writing.
    proposed = list(existing) + [rule]
    try:
        gr.parse_layer({"rules": proposed}, "validate")
    except ValueError as exc:
        print_error(str(exc))
        return 2

    def _mutate(data: dict[str, Any]) -> None:
        block = _ensure_tripwire_block(data, prof, scoped)
        rules = block.get("rules")
        if not isinstance(rules, list):
            rules = []
        block["rules"] = rules + [rule]

    update_config(path, _mutate)
    if _json_out(args):
        print(json.dumps({"added": name, "effective": "next run"}, indent=2, ensure_ascii=False))
        return 0
    print(f"added guardrail rule {name!r} — effective on the NEXT run "
          f"(the ruleset is loaded once at process start, NFR-03)")
    return 0


def cmd_guardrail_remove(args: argparse.Namespace) -> int:
    name = (args.name or "").strip()
    if not name:
        print_error("--name is required")
        return 2
    scoped = bool((args.profile or "").strip())
    prof = ""
    path = config_path(getattr(args, "config", None))
    cur = load_config(path)
    if scoped:
        prof = _profile(cur, args.profile)
        try:
            from tag.core.profile import ensure_profile_exists
            ensure_profile_exists(cur, prof)
        except SystemExit as exc:
            print_error(str(exc))
            return 2

    removed = {"hit": False}

    def _mutate(data: dict[str, Any]) -> None:
        block = _lookup_tripwire_block(data, prof if scoped else "")
        if block is None:
            return
        rules = block.get("rules")
        if not isinstance(rules, list):
            return
        kept = []
        for r in rules:
            if isinstance(r, dict) and r.get("name") == name:
                removed["hit"] = True
                continue
            kept.append(r)
        block["rules"] = kept

    update_config(path, _mutate)
    if not removed["hit"]:
        print_error(f"no guardrail rule named {name!r} found")
        return 2
    if _json_out(args):
        print(json.dumps({"removed": name, "effective": "next run"}, indent=2, ensure_ascii=False))
        return 0
    print(f"removed guardrail rule {name!r} — effective on the NEXT run (NFR-03)")
    return 0


# ---- raw-config nav (mirrors the Go lookup/ensure helpers) ----------------

def _lookup_tripwire_block(data: dict[str, Any], prof: str) -> dict[str, Any] | None:
    if prof:
        profs = data.get("profiles")
        if isinstance(profs, dict):
            pm = profs.get(prof)
            if isinstance(pm, dict):
                cfgm = pm.get("config")
                if isinstance(cfgm, dict) and isinstance(cfgm.get("tripwire"), dict):
                    return cfgm["tripwire"]
        return None
    block = data.get("tripwire")
    return block if isinstance(block, dict) else None


def _tripwire_rule_list(data: dict[str, Any], prof: str) -> list[Any]:
    block = _lookup_tripwire_block(data, prof)
    if block is None:
        return []
    rules = block.get("rules")
    return rules if isinstance(rules, list) else []


def _child_map(parent: dict[str, Any], key: str) -> dict[str, Any]:
    child = parent.get(key)
    if not isinstance(child, dict):
        child = {}
        parent[key] = child
    return child


def _ensure_tripwire_block(data: dict[str, Any], prof: str, scoped: bool) -> dict[str, Any]:
    if scoped:
        profs = _child_map(data, "profiles")
        pm = _child_map(profs, prof)
        cfgm = _child_map(pm, "config")
        return _child_map(cfgm, "tripwire")
    return _child_map(data, "tripwire")


# ---------------------------------------------------------------------------
# parser registration
# ---------------------------------------------------------------------------

def _needs_subcommand(_args: argparse.Namespace) -> int:
    print_error("a subcommand is required (try --help)")
    return 2


def _add_common_subcommands(sub: argparse._SubParsersAction) -> None:  # type: ignore[type-arg]
    stages_help = f"which screening point to evaluate {gr.stages()}"

    lst = sub.add_parser("list", help="Print the resolved guardrail ruleset")
    lst.add_argument("--profile", help="profile whose tripwire block to resolve")
    lst.add_argument("--json", action="store_true")
    lst.set_defaults(func=cmd_tripwire_list)

    chk = sub.add_parser("check", help="Screen content against the ruleset (exit 3 if the tripwire fires)")
    chk.add_argument("--stage", default=gr.STAGE_MODEL_OUTPUT, help=stages_help)
    chk.add_argument("--text", default="", help="content to screen")
    chk.add_argument("--file", default="", help="read the content from a file")
    chk.add_argument("--stdin", action="store_true", help="read the content from stdin")
    chk.add_argument("--session", default="", help="session id for tripwire counters")
    chk.add_argument("--profile", help="profile whose tripwire block to resolve")
    chk.add_argument("--exit-zero", dest="exit_zero", action="store_true",
                     help="exit 0 even when the tripwire fires (advisory use)")
    chk.add_argument("--json", action="store_true")
    chk.set_defaults(func=cmd_tripwire_check)

    tst = sub.add_parser("test", help="Dry-run the tool-input guardrails against a mock tool call")
    tst.add_argument("--tool", default="", help="tool name to simulate (required)")
    tst.add_argument("--args", default="{}", help="tool arguments as a JSON object")
    tst.add_argument("--session", default="", help="session id for tripwire counters")
    tst.add_argument("--profile", help="profile whose tripwire block to resolve")
    tst.add_argument("--exit-zero", dest="exit_zero", action="store_true",
                     help="exit 0 even when the tripwire fires")
    tst.add_argument("--json", action="store_true")
    tst.set_defaults(func=cmd_tripwire_test)

    hist = sub.add_parser("history", help="Show recorded guardrail decisions")
    hist.add_argument("--limit", type=int, default=50, help="max rows")
    hist.add_argument("--json", action="store_true")
    hist.set_defaults(func=cmd_tripwire_history)


def _add_edit_subcommands(sub: argparse._SubParsersAction) -> None:  # type: ignore[type-arg]
    add = sub.add_parser("add", help="Add a guardrail rule to tag.yaml (effective NEXT run)")
    add.add_argument("--profile", default="", help="profile whose tripwire block to edit (default: top-level)")
    add.add_argument("--name", default="", help="rule name (required, unique)")
    add.add_argument("--tool", default="", help="tool-name glob the rule applies to (default *)")
    add.add_argument("--type", default="", help="rule type: pattern | tripwire | require-approval (default pattern)")
    add.add_argument("--pattern", default="", help="regex matched against content (pattern rules)")
    add.add_argument("--builtin", default="", help="built-in detector: secrets | destructive (pattern rules)")
    add.add_argument("--stage", default="", help=f"screening stage {gr.stages()} (default: every stage)")
    add.add_argument("--action", default="", help="action on match: block | warn | interrupt")
    add.add_argument("--threshold", type=int, default=0, help="tripwire threshold (tripwire rules)")
    add.add_argument("--window", default="", help="tripwire window duration, e.g. 1h (tripwire rules)")
    add.add_argument("--message", default="", help="message shown when the rule fires")
    add.add_argument("--json", action="store_true")
    add.set_defaults(func=cmd_guardrail_add)

    rem = sub.add_parser("remove", help="Remove a guardrail rule from tag.yaml by name (effective NEXT run)")
    rem.add_argument("--profile", default="", help="profile whose tripwire block to edit (default: top-level)")
    rem.add_argument("--name", default="", help="name of the rule to remove (required)")
    rem.add_argument("--json", action="store_true")
    rem.set_defaults(func=cmd_guardrail_remove)


# ---------------------------------------------------------------------------
# Content guardrails — `guardrail input` (PRD-122) and `guardrail output`
# (PRD-121), sharing the GuardrailResult type (PRD-124) and audit log.
# ---------------------------------------------------------------------------

def cmd_guardrail_content(args: argparse.Namespace) -> int:
    from tag.core import content_guardrail as cg  # noqa: PLC0415

    direction = getattr(args, "guardrail_command", None)  # 'input' | 'output'
    verb = getattr(args, "content_command", None)
    if direction not in ("input", "output"):
        print_error("expected `guardrail input ...` or `guardrail output ...`")
        return 2
    if verb is None:
        return _needs_subcommand(args)

    cfg = _cfg(args)
    profile = _profile(cfg, getattr(args, "profile", None))
    conn = _open_conn(args, cfg)
    if conn is None:
        print_error("could not open the database")
        return 1

    if verb == "add":
        return _content_add(cg, conn, args, direction, profile)
    if verb == "list":
        return _content_list(cg, conn, args, direction, profile)
    if verb == "remove":
        return _content_remove(cg, conn, args, direction)
    if verb == "test":
        return _content_test(cg, conn, args, direction, profile)
    if verb == "history":
        return _content_history(cg, conn, args, direction)
    return _needs_subcommand(args)


def _content_add(cg, conn, args, direction: str, profile: str) -> int:
    gtype = (args.type or "").strip()
    valid_types = cg.VALID_TYPES_INPUT if direction == "input" else cg.VALID_TYPES_OUTPUT
    if gtype not in valid_types:
        print_error(f"--type must be one of {sorted(valid_types)}")
        return 2
    default_action = cg.ACTION_BLOCK
    action = (args.action or "").strip() or default_action
    valid_actions = cg.VALID_ACTIONS_INPUT if direction == "input" else cg.VALID_ACTIONS_OUTPUT
    if action not in valid_actions:
        print_error(f"--action must be one of {sorted(valid_actions)} for {direction} guardrails")
        return 2

    config: dict[str, Any] = {}
    if gtype == "length-limit":
        config["max_length"] = int(getattr(args, "max_length", 4096) or 4096)
    if gtype == "topic-filter":
        topics = (getattr(args, "topics", "") or "").strip()
        if topics:
            config["topics"] = [t.strip() for t in topics.split(",") if t.strip()]
        if getattr(args, "threshold", None) is not None:
            config["threshold"] = args.threshold
    if gtype == "json-schema":
        schema_path = (getattr(args, "schema", "") or "").strip()
        if not schema_path:
            print_error("--schema PATH is required for a json-schema guardrail")
            return 2
        try:
            with open(schema_path, "r", encoding="utf-8") as fh:
                config["schema"] = json.load(fh)
        except (OSError, json.JSONDecodeError) as exc:
            print_error(f"cannot read --schema {schema_path}: {exc}")
            return 2
    if gtype == "profanity":
        words = (getattr(args, "words", "") or "").strip()
        if words:
            config["words"] = [w.strip() for w in words.split(",") if w.strip()]

    model = getattr(args, "classifier_model", None) if direction == "input" \
        else getattr(args, "remediation_model", None)
    severity = getattr(args, "severity", "high") or "high"
    cid = cg.add_config(conn, direction, profile=profile, gtype=gtype, action=action,
                        config=config, severity=severity, model=model)
    if _json_out(args):
        print(json.dumps({"id": cid, "profile": profile, "type": gtype,
                          "action": action, "direction": direction}, indent=2))
        return 0
    print(f"added {direction} guardrail {gtype!r} ({action}) for profile {profile!r} — id {cid}")
    return 0


def _content_list(cg, conn, args, direction: str, profile: str) -> int:
    configs = cg.list_configs(conn, direction, profile)
    if _json_out(args):
        print(json.dumps(configs, indent=2))
        return 0
    if not configs:
        print(f"No {direction} guardrails configured for profile {profile!r}.")
        print(f"  add one, e.g. `tag guardrail {direction} add --type "
              f"{'prompt-injection' if direction == 'input' else 'pii'} --action block`")
        return 0
    print(f"{len(configs)} {direction} guardrail(s) for {profile!r}:")
    for c in configs:
        extra = ""
        if c["config"]:
            extra = "  " + json.dumps(c["config"])
        print(f"  {c['id']}  {c['guardrail_type']:<16} {c['action']:<9} [{c['severity']}]{extra}")
    return 0


def _content_remove(cg, conn, args, direction: str) -> int:
    cid = (getattr(args, "id", "") or "").strip()
    if not cid:
        print_error("remove needs a guardrail id (see `guardrail %s list`)" % direction)
        return 2
    if not cg.remove_config(conn, direction, cid):
        print_error(f"no {direction} guardrail with id {cid!r}")
        return 2
    if _json_out(args):
        print(json.dumps({"removed": cid}, indent=2))
        return 0
    print(f"removed {direction} guardrail {cid}")
    return 0


def _content_test(cg, conn, args, direction: str, profile: str) -> int:
    text = _read_content(getattr(args, "input", "") or "", getattr(args, "file", "") or "",
                         getattr(args, "stdin", False))
    if not cg.list_configs(conn, direction, profile):
        print_error(f"no {direction} guardrails are configured for profile {profile!r}, so "
                    f"nothing was checked — add one with `tag guardrail {direction} add` "
                    f"rather than treating this as a pass")
        return 2
    verdict = cg.run_chain(conn, direction, profile, text, run_id="test", persist=False)
    if _json_out(args):
        print(json.dumps(verdict.to_dict(), indent=2, ensure_ascii=False))
    else:
        if verdict.final_action == cg.ACTION_PASS:
            print(f"clean ({direction}): no guardrail matched")
        else:
            print(f"{verdict.final_action.upper()} ({direction})")
        for r in verdict.results:
            line = f"  - {r.guardrail:<16} {r.action:<9} {r.reason}"
            print(line)
            if r.message:
                print(f"      note: {r.message}")
        if verdict.final_action == cg.ACTION_SANITIZE:
            print(f"  sanitized → {verdict.text}")
    if verdict.final_action != cg.ACTION_PASS and not getattr(args, "exit_zero", False):
        return EXIT_FINDINGS
    return 0


def _content_history(cg, conn, args, direction: str) -> int:
    limit = int(getattr(args, "limit", 50) or 50)
    cg.ensure_content_schema(conn, direction)
    rows = conn.execute(
        "SELECT created_at, action, rule, blocked, detail FROM guardrail_events "
        "WHERE direction=? ORDER BY id DESC LIMIT ?", (direction, limit)).fetchall()
    if _json_out(args):
        print(json.dumps([
            {"created_at": r[0], "action": r[1], "guardrail": r[2],
             "blocked": bool(r[3]), "detail": r[4]} for r in rows], indent=2))
        return 0
    if not rows:
        print(f"no {direction} guardrail events recorded yet")
        return 0
    for r in rows:
        verdict = "BLOCK" if r[3] else str(r[1]).upper()
        print(f"{r[0]}  {verdict:<10} {r[2]:<16} {r[4]}")
    return 0


def _add_content_subcommands(sub: argparse._SubParsersAction, direction: str) -> None:  # type: ignore[type-arg]
    types = "prompt-injection|pii|secret|topic-filter|length-limit|custom" if direction == "input" \
        else "pii|secret|json-schema|topic-filter|profanity|toxicity|custom"
    actions = "block|sanitize|warn" if direction == "input" else "block|rewrite|warn"

    add = sub.add_parser("add", help=f"Add an {direction} guardrail to a profile")
    add.add_argument("--profile", help="target profile (default: master profile)")
    add.add_argument("--type", default="", help=f"guardrail type: {types}")
    add.add_argument("--action", default="", help=f"on match: {actions} (default block)")
    add.add_argument("--severity", default="high", choices=["high", "medium", "low"])
    add.add_argument("--max-length", dest="max_length", type=int, default=4096,
                     help="length-limit type: max characters")
    add.add_argument("--topics", default="", help="topic-filter type: comma-separated forbidden topics")
    add.add_argument("--threshold", type=float, default=None, help="topic-filter similarity threshold")
    add.add_argument("--schema", default="", help="json-schema type: path to a JSON Schema file")
    add.add_argument("--words", default="", help="profanity type: comma-separated words (extends defaults)")
    if direction == "input":
        add.add_argument("--classifier-model", dest="classifier_model", default=None,
                         help="optional LLM classifier model (topic-filter/injection)")
    else:
        add.add_argument("--remediation-model", dest="remediation_model", default=None,
                         help="model for the rewrite action")
    add.add_argument("--json", action="store_true")
    add.set_defaults(func=cmd_guardrail_content)

    lst = sub.add_parser("list", help=f"List configured {direction} guardrails")
    lst.add_argument("--profile", help="profile (default: master profile)")
    lst.add_argument("--json", action="store_true")
    lst.set_defaults(func=cmd_guardrail_content)

    rem = sub.add_parser("remove", help=f"Remove an {direction} guardrail by id")
    rem.add_argument("id", metavar="ID", nargs="?", default="")
    rem.add_argument("--json", action="store_true")
    rem.set_defaults(func=cmd_guardrail_content)

    tst = sub.add_parser("test", help=f"Dry-run the {direction} guardrail chain against a string")
    tst.add_argument("--profile", help="profile (default: master profile)")
    tst.add_argument("--input", default="", help="content to screen")
    tst.add_argument("--file", default="", help="read content from a file")
    tst.add_argument("--stdin", action="store_true", help="read content from stdin")
    tst.add_argument("--exit-zero", dest="exit_zero", action="store_true",
                     help="exit 0 even when a guardrail fires")
    tst.add_argument("--json", action="store_true")
    tst.set_defaults(func=cmd_guardrail_content)

    hist = sub.add_parser("history", help=f"Show recent {direction} guardrail decisions")
    hist.add_argument("--limit", type=int, default=50)
    hist.add_argument("--json", action="store_true")
    hist.set_defaults(func=cmd_guardrail_content)


def register(sub: argparse._SubParsersAction) -> None:  # type: ignore[type-arg]
    # tripwire — the canonical spelling.
    tw = sub.add_parser(
        "tripwire",
        help="Content guardrails: screen model output and tool I/O, halt on policy violations",
    )
    tw.set_defaults(func=_needs_subcommand)
    tw_sub = tw.add_subparsers(dest="tripwire_command", metavar="SUBCOMMAND")
    _add_common_subcommands(tw_sub)

    # guardrail runtime — PRD-123 §6 alias over the same engine + edit verbs.
    g = sub.add_parser(
        "guardrail",
        help="Runtime guardrails (PRD-123): the same engine as `tag tripwire`, under the guardrail namespace",
    )
    g.set_defaults(func=_needs_subcommand)
    g_sub = g.add_subparsers(dest="guardrail_command", metavar="SUBCOMMAND")
    runtime = g_sub.add_parser(
        "runtime", help="Runtime content guardrails: list, check, test, inspect, and edit the ruleset")
    runtime.set_defaults(func=_needs_subcommand)
    rt_sub = runtime.add_subparsers(dest="runtime_command", metavar="SUBCOMMAND")
    _add_common_subcommands(rt_sub)
    _add_edit_subcommands(rt_sub)

    # guardrail input (PRD-122) — pre-model input validation.
    ginput = g_sub.add_parser(
        "input", help="Input guardrails (PRD-122): validate/sanitize user input before the model")
    ginput.set_defaults(func=cmd_guardrail_content)
    _add_content_subcommands(ginput.add_subparsers(dest="content_command", metavar="SUBCOMMAND"), "input")

    # guardrail output (PRD-121) — post-model output validation.
    goutput = g_sub.add_parser(
        "output", help="Output guardrails (PRD-121): screen model output (PII/secret/schema/…)")
    goutput.set_defaults(func=cmd_guardrail_content)
    _add_content_subcommands(goutput.add_subparsers(dest="content_command", metavar="SUBCOMMAND"), "output")
