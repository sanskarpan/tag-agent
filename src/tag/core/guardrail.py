"""Runtime CONTENT guardrail processor and tripwire (PRD-123), Python port.

This mirrors tag-go/internal/guardrail. It is a different thing from the
permission engine:

    permission answers "is this SUBJECT (a path, a command name) allowed?"
    guardrail  answers "does this CONTENT violate policy?"

Fail-open vs fail-closed, stated per check (identical doctrine to the Go engine):

    load-time bad regex / bad config -> HARD ERROR (compile raises).
    args not serialisable for inspection -> FAIL CLOSED (block).
    tripwire counter store unreachable -> FAIL CLOSED (block).
    guardrail_events write failure -> FAIL OPEN (best effort) + a loud warning.
    no rules configured -> inert.
"""
from __future__ import annotations

import datetime as _dt
import fnmatch
import json
import re
import sqlite3
from dataclasses import dataclass, field
from typing import Any

# ---- stages ---------------------------------------------------------------

STAGE_TOOL_INPUT = "tool_input"
STAGE_TOOL_OUTPUT = "tool_output"
STAGE_MODEL_OUTPUT = "model_output"
STAGE_USER_INPUT = "user_input"


def stages() -> list[str]:
    return [STAGE_TOOL_INPUT, STAGE_TOOL_OUTPUT, STAGE_MODEL_OUTPUT, STAGE_USER_INPUT]


def parse_stage(s: str) -> str:
    v = (s or "").strip().lower()
    if v in stages():
        return v
    raise ValueError(f"invalid guardrail stage {s!r} (want one of {stages()})")


# ---- actions --------------------------------------------------------------

ACTION_BLOCK = "block"
ACTION_WARN = "warn"
ACTION_INTERRUPT = "interrupt"


def parse_action(s: str) -> str:
    v = (s or "").strip().lower()
    if v == "":
        return ACTION_BLOCK
    if v in (ACTION_BLOCK, ACTION_WARN, ACTION_INTERRUPT):
        return v
    raise ValueError(f"invalid guardrail action {s!r} (want block, warn, or interrupt)")


# ---- rule types -----------------------------------------------------------

TYPE_PATTERN = "pattern"
TYPE_TRIPWIRE = "tripwire"
TYPE_REQUIRE_APPROVAL = "require-approval"


@dataclass
class Rule:
    name: str = ""
    type: str = TYPE_PATTERN
    stage: str = ""  # empty = every stage
    tool: str = "*"
    builtin: str = ""
    pattern: str = ""
    threshold: int = 0
    window_seconds: float = 0.0  # 0 = no window
    window_text: str = ""  # the original duration string, for display
    action: str = ACTION_BLOCK
    message: str = ""
    source: str = ""

    _re: re.Pattern[str] | None = field(default=None, repr=False, compare=False)
    _tool_pat: str = field(default="", repr=False, compare=False)

    def describe(self, default: str) -> str:
        return self.message if self.message.strip() else default

    def evaluate(self, content: str) -> list["_Hit"]:
        if self.builtin:
            return builtin_scan(self.builtin, content)
        if self._re is None:
            return []
        out: list[_Hit] = []
        for m in _finditer_capped(self._re, content, 8):
            out.append(_Hit(detector="pattern:" + self.name,
                            excerpt=redact(m.group(0)), offset=m.start()))
        return out


@dataclass
class _Hit:
    detector: str
    excerpt: str
    offset: int


@dataclass
class Finding:
    rule: str
    type: str
    stage: str
    action: str
    detector: str = ""
    tool: str = ""
    excerpt: str = ""
    offset: int = 0
    count: int = 0
    threshold: int = 0
    message: str = ""

    def to_json(self) -> dict[str, Any]:
        m: dict[str, Any] = {
            "rule": self.rule, "type": self.type, "stage": self.stage,
            "detector": self.detector, "action": self.action,
            "excerpt": self.excerpt, "offset": self.offset, "message": self.message,
        }
        if self.tool:
            m["tool"] = self.tool
        if self.count:
            m["count"] = self.count
        if self.threshold:
            m["threshold"] = self.threshold
        return m


@dataclass
class Verdict:
    stage: str
    tool: str = ""
    findings: list[Finding] = field(default_factory=list)
    blocked: bool = False
    interrupted: bool = False
    warned: bool = False
    reason: str = ""
    undecidable: bool = False

    def fired(self) -> bool:
        return self.blocked or self.interrupted or self.warned

    def outcome(self) -> str:
        """Collapse to the single effective action: block > interrupt > warn > pass."""
        if self.blocked:
            return ACTION_BLOCK
        if self.interrupted:
            return ACTION_INTERRUPT
        if self.warned:
            return ACTION_WARN
        return ""

    def to_json(self) -> dict[str, Any]:
        m: dict[str, Any] = {
            "stage": self.stage,
            "findings": [f.to_json() for f in self.findings],
            "blocked": self.blocked,
            "warned": self.warned,
        }
        if self.tool:
            m["tool"] = self.tool
        if self.interrupted:
            m["interrupted"] = self.interrupted
        if self.reason:
            m["reason"] = self.reason
        if self.undecidable:
            m["undecidable"] = self.undecidable
        return m


def _finditer_capped(pat: re.Pattern[str], content: str, cap: int) -> list[re.Match[str]]:
    out: list[re.Match[str]] = []
    for m in pat.finditer(content):
        out.append(m)
        if len(out) >= cap:
            break
    return out


def redact(s: str) -> str:
    """Render a match safely for logs: only a short prefix and the length survive."""
    s = s.replace("\n", " ").replace("\r", " ").replace("\t", " ")
    # Mask the match ENTIRELY — do not echo any of the caught secret into stdout
    # or the persisted history log (#763 security). Length is not sensitive and
    # helps an operator recognise the hit. Matches the Go harness.
    if not s:
        return ""
    return f"[redacted, {len(s)} chars]"


def match_tool(pattern: str, tool: str) -> bool:
    if pattern == "" or pattern == "*":
        return True
    return fnmatch.fnmatchcase(tool, pattern)


# ---- duration parsing (Go time.ParseDuration subset) ----------------------

_DURATION_UNITS = {
    "ns": 1e-9, "us": 1e-6, "µs": 1e-6, "ms": 1e-3, "s": 1.0, "m": 60.0, "h": 3600.0,
}
_DURATION_RE = re.compile(r"(\d+(?:\.\d+)?)(ns|us|µs|ms|s|m|h)")


def parse_duration_seconds(text: str) -> float:
    """Parse a Go-style duration ("1h", "30m", "1h30m") into seconds."""
    s = (text or "").strip()
    if s in ("", "0"):
        return 0.0
    total = 0.0
    pos = 0
    matched_any = False
    for m in _DURATION_RE.finditer(s):
        if m.start() != pos:
            raise ValueError(f"invalid duration {text!r}")
        pos = m.end()
        total += float(m.group(1)) * _DURATION_UNITS[m.group(2)]
        matched_any = True
    if not matched_any or pos != len(s):
        raise ValueError(f"invalid duration {text!r} (want a value like 30m or 1h)")
    return total


def format_duration(seconds: float) -> str:
    """Format seconds the way Go's Duration.String would for whole seconds."""
    if seconds <= 0:
        return "0s"
    total = int(round(seconds))
    h, rem = divmod(total, 3600)
    m, s = divmod(rem, 60)
    out = ""
    if h:
        out += f"{h}h"
    if h or m:
        out += f"{m}m"
    out += f"{s}s"
    return out


# ---- config parsing -------------------------------------------------------

def parse_layer(block: dict[str, Any] | None, source: str) -> list[Rule]:
    """Read a `tripwire:` block out of a decoded YAML map. A malformed entry is a
    HARD ERROR (raises), mirroring the Go loader — a half-loaded policy is worse
    than one that refuses to start."""
    if not block:
        return []
    out: list[Rule] = []

    if "preset" in block:
        v = block["preset"]
        if not isinstance(v, str):
            raise ValueError(f"{source}: tripwire.preset must be a string")
        name = v.strip().lower()
        if name not in ("", "none"):
            presets = builtin_presets()
            if name not in presets:
                raise ValueError(
                    f"{source}: unknown tripwire.preset {name!r} (want one of {preset_names()})"
                )
            for r in presets[name]:
                r = _copy_rule(r)
                r.source = f"{source}:preset:{name}"
                out.append(r)

    if "rules" in block:
        v = block["rules"]
        if not isinstance(v, list):
            raise ValueError(f"{source}: tripwire.rules must be a list")
        for i, item in enumerate(v):
            if not isinstance(item, dict):
                raise ValueError(f"{source}: tripwire.rules[{i}] must be a mapping")
            r = _parse_rule(item, f"{source}: tripwire.rules[{i}]")
            r.source = f"{source}:rules"
            out.append(r)
    return compile_rules(out)


def _parse_rule(m: dict[str, Any], where: str) -> Rule:
    r = Rule()
    r.name = str(m.get("name", "") or "").strip()
    if not r.name:
        raise ValueError(f"{where}: name is required (it identifies the rule in findings and counters)")

    r.type = TYPE_PATTERN
    if "type" in m:
        s = str(m.get("type", "") or "").strip().lower()
        if s in (TYPE_PATTERN, ""):
            r.type = TYPE_PATTERN
        elif s == TYPE_TRIPWIRE:
            r.type = TYPE_TRIPWIRE
        elif s == TYPE_REQUIRE_APPROVAL:
            r.type = TYPE_REQUIRE_APPROVAL
        else:
            raise ValueError(f"{where}: type must be 'pattern', 'tripwire', or 'require-approval', got {s!r}")

    if "stage" in m:
        s = str(m.get("stage", "") or "").strip()
        if s:
            try:
                r.stage = parse_stage(s)
            except ValueError as exc:
                raise ValueError(f"{where}: {exc}")

    r.tool = str(m.get("tool", "") or "").strip() or "*"

    r.builtin = str(m.get("builtin", "") or "").strip()
    if r.builtin:
        validate_builtin(r.builtin)  # raises on unknown
        r.builtin = r.builtin
    r.pattern = str(m.get("pattern", "") or "")
    if r.builtin and r.pattern.strip():
        raise ValueError(f"{where}: set either 'builtin' or 'pattern', not both")

    if "threshold" in m:
        try:
            r.threshold = _as_int(m["threshold"])
        except ValueError as exc:
            raise ValueError(f"{where}: threshold must be an integer: {exc}")
    if "window" in m:
        s = str(m.get("window", "") or "").strip()
        try:
            r.window_seconds = parse_duration_seconds(s)
            r.window_text = s
        except ValueError as exc:
            raise ValueError(f"{where}: window must be a duration like 30m or 1h: {exc}")

    act = str(m.get("action", "") or "")
    r.action = parse_action(act)
    if r.type == TYPE_REQUIRE_APPROVAL and act.strip() == "":
        r.action = ACTION_INTERRUPT
    r.message = str(m.get("message", "") or "")
    return r


def _as_int(v: Any) -> int:
    if isinstance(v, bool):  # bool is an int subclass; reject it explicitly
        raise ValueError(f"got {type(v).__name__}")
    if isinstance(v, int):
        return v
    if isinstance(v, float):
        return int(v)
    raise ValueError(f"got {type(v).__name__}")


def _copy_rule(r: Rule) -> Rule:
    return Rule(
        name=r.name, type=r.type, stage=r.stage, tool=r.tool, builtin=r.builtin,
        pattern=r.pattern, threshold=r.threshold, window_seconds=r.window_seconds,
        window_text=r.window_text, action=r.action, message=r.message, source=r.source,
    )


def compile_rules(rules: list[Rule]) -> list[Rule]:
    """Validate and pre-compile a ruleset. Every failure here raises at load time,
    never a rule that quietly matches nothing."""
    out: list[Rule] = []
    seen: set[str] = set()
    for r in rules:
        if not r.name:
            raise ValueError("guardrail rule with no name")
        if r.name in seen:
            raise ValueError(f"duplicate guardrail rule name {r.name!r}")
        seen.add(r.name)

        if not r.tool:
            r.tool = "*"
        try:
            re.compile(fnmatch.translate(r.tool))
        except re.error as exc:
            raise ValueError(f"guardrail rule {r.name!r}: invalid tool glob {r.tool!r}: {exc}")
        r._tool_pat = r.tool

        if not r.type:
            r.type = TYPE_PATTERN
        if not r.action:
            r.action = ACTION_INTERRUPT if r.type == TYPE_REQUIRE_APPROVAL else ACTION_BLOCK
        if r.builtin:
            validate_builtin(r.builtin)
        p = r.pattern.strip()
        if p:
            expr = p if p.startswith("(?") else "(?i)" + p
            try:
                r._re = re.compile(expr)
            except re.error as exc:
                raise ValueError(f"guardrail rule {r.name!r}: invalid pattern {r.pattern!r}: {exc}")

        if r.type == TYPE_PATTERN:
            if r._re is None and not r.builtin:
                raise ValueError(f"guardrail rule {r.name!r}: a pattern rule needs 'pattern' or 'builtin'")
        elif r.type == TYPE_TRIPWIRE:
            if r.threshold <= 0:
                raise ValueError(f"guardrail rule {r.name!r}: a tripwire needs a positive 'threshold'")
        elif r.type == TYPE_REQUIRE_APPROVAL:
            if r._re is not None or r.builtin:
                raise ValueError(f"guardrail rule {r.name!r}: a require-approval rule matches every invocation and takes no 'pattern'/'builtin'")
            if r.threshold != 0:
                raise ValueError(f"guardrail rule {r.name!r}: a require-approval rule takes no 'threshold'")
        out.append(r)
    return out


# ---- built-in detectors ---------------------------------------------------

@dataclass
class _Detector:
    name: str
    re: re.Pattern[str]
    verify: Any = None  # optional callable(str) -> bool


_PLACEHOLDER_RE = re.compile(
    r"(?i)(x{8,}|\.{3,}|<[a-z_ -]+>|your[_\- ]?(api[_\- ]?)?key|"
    r"example|placeholder|redacted|changeme|dummy|s+a+m+p+l+e|\$\{[^}]+\}|\bnull\b|\bnone\b)"
)

_SECRET_DETECTORS = [
    _Detector("aws-access-key-id", re.compile(r"\b(?:AKIA|ASIA)[0-9A-Z]{16}\b")),
    _Detector("github-token", re.compile(r"\bgh[pousr]_[A-Za-z0-9]{36,255}\b")),
    _Detector("slack-token", re.compile(r"\bxox[baprs]-[A-Za-z0-9-]{10,}\b")),
    _Detector("google-api-key", re.compile(r"\bAIza[0-9A-Za-z_\-]{35}\b")),
    _Detector("anthropic-api-key", re.compile(r"\bsk-ant-[A-Za-z0-9_\-]{20,}\b")),
    _Detector("openai-api-key", re.compile(r"\bsk-(?:proj-)?[A-Za-z0-9_\-]{32,}\b")),
    _Detector("private-key-block", re.compile(r"-----BEGIN (?:RSA |EC |OPENSSH |PGP |DSA )?PRIVATE KEY-----")),
    _Detector("jwt", re.compile(r"\beyJ[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}\b")),
    _Detector(
        "generic-api-key-assignment",
        re.compile(
            r"(?i)\b(?:api[_\-]?key|secret[_\-]?key|access[_\-]?token|auth[_\-]?token|password)\b"
            r"\s*[:=]\s*[\"']?([A-Za-z0-9/+_\-]{16,})[\"']?"
        ),
        verify=lambda s: not _PLACEHOLDER_RE.search(s),
    ),
]

_DESTRUCTIVE_DETECTORS = [
    _Detector("rm-rf-root", re.compile(
        r"(?i)\brm\s+(?:-{1,2}[a-z][a-z-]*\s+)*-[a-z]*[rR][a-z]*[fF][a-z]*\s+(?:-{1,2}[a-z][a-z-]*\s+|--\s+)*[/~]\S*")),
    _Detector("rm-rf-root-alt", re.compile(
        r"(?i)\brm\s+(?:-{1,2}[a-z][a-z-]*\s+)*-[a-z]*[fF][a-z]*[rR][a-z]*\s+(?:-{1,2}[a-z][a-z-]*\s+|--\s+)*[/~]\S*")),
    _Detector("mkfs", re.compile(r"(?i)\bmkfs(?:\.[a-z0-9]+)?\s")),
    _Detector("dd-to-device", re.compile(r"(?i)\bdd\b[^\n]*\bof=/dev/")),
    _Detector("fork-bomb", re.compile(r":\(\)\s*\{\s*:\s*\|\s*:\s*&\s*\}\s*;\s*:")),
    _Detector("chmod-777-root", re.compile(r"(?i)\bchmod\s+(?:-[a-zA-Z]+\s+)*777\s+/(?:\s|$)")),
    _Detector("curl-pipe-shell", re.compile(r"(?i)\b(?:curl|wget)\b[^\n|]*\|\s*(?:sudo\s+)?(?:ba|z|k)?sh\b")),
    _Detector("history-wipe", re.compile(r"(?i)\b(?:history\s+-c|shred\s+|>\s*~?/?\.bash_history)")),
    _Detector("drop-database", re.compile(r"(?i)\bdrop\s+(?:database|schema|table)\b")),
    _Detector("git-force-push-main", re.compile(r"(?i)\bgit\s+push\b[^\n]*(?:--force|-f)\b[^\n]*\b(?:main|master)\b")),
]


def builtin_names() -> list[str]:
    return ["destructive", "secrets"]


def validate_builtin(name: str) -> None:
    if name not in builtin_names():
        raise ValueError(f"unknown builtin guardrail detector {name!r} (want one of {builtin_names()})")


def _detectors_for(name: str) -> list[_Detector]:
    if name == "secrets":
        return _SECRET_DETECTORS
    if name == "destructive":
        return _DESTRUCTIVE_DETECTORS
    return []


def builtin_scan(family: str, content: str) -> list[_Hit]:
    out: list[_Hit] = []
    for d in _detectors_for(family):
        for m in _finditer_capped(d.re, content, 8):
            raw = m.group(0)
            if d.verify is not None and not d.verify(raw):
                continue
            out.append(_Hit(detector=f"{family}:{d.name}", excerpt=redact(raw.strip()), offset=m.start()))
    out.sort(key=lambda h: (h.offset, h.detector))
    return out


def builtin_presets() -> dict[str, list[Rule]]:
    return {
        "standard": [
            Rule(name="builtin-destructive-command", type=TYPE_PATTERN, stage=STAGE_TOOL_INPUT,
                 tool="bash", builtin="destructive", action=ACTION_BLOCK,
                 message="destructive shell command", source="preset:standard"),
            Rule(name="builtin-secret-in-tool-input", type=TYPE_PATTERN, stage=STAGE_TOOL_INPUT,
                 tool="*", builtin="secrets", action=ACTION_BLOCK,
                 message="credential-shaped value in tool arguments", source="preset:standard"),
            Rule(name="builtin-secret-in-tool-output", type=TYPE_PATTERN, stage=STAGE_TOOL_OUTPUT,
                 tool="*", builtin="secrets", action=ACTION_BLOCK,
                 message="credential-shaped value in tool result", source="preset:standard"),
        ],
        "observe": [
            Rule(name="builtin-destructive-command", type=TYPE_PATTERN, stage=STAGE_TOOL_INPUT,
                 tool="bash", builtin="destructive", action=ACTION_WARN,
                 message="destructive shell command", source="preset:observe"),
            Rule(name="builtin-secret-in-tool-input", type=TYPE_PATTERN, stage=STAGE_TOOL_INPUT,
                 tool="*", builtin="secrets", action=ACTION_WARN,
                 message="credential-shaped value in tool arguments", source="preset:observe"),
            Rule(name="builtin-secret-in-tool-output", type=TYPE_PATTERN, stage=STAGE_TOOL_OUTPUT,
                 tool="*", builtin="secrets", action=ACTION_WARN,
                 message="credential-shaped value in tool result", source="preset:observe"),
        ],
    }


def preset_names() -> list[str]:
    return sorted(builtin_presets().keys())


# ---- persistence ----------------------------------------------------------

_SCHEMA = """
CREATE TABLE IF NOT EXISTS guardrail_events (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  created_at  TEXT NOT NULL,
  session_id  TEXT NOT NULL DEFAULT '',
  direction   TEXT NOT NULL DEFAULT 'runtime',
  stage       TEXT NOT NULL,
  tool        TEXT NOT NULL DEFAULT '',
  rule        TEXT NOT NULL DEFAULT '',
  action      TEXT NOT NULL DEFAULT '',
  blocked     INTEGER NOT NULL DEFAULT 0,
  undecidable INTEGER NOT NULL DEFAULT 0,
  detail      TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_guardrail_events_at ON guardrail_events(created_at DESC);

CREATE TABLE IF NOT EXISTS tripwire_counters (
  session_id   TEXT NOT NULL,
  rule         TEXT NOT NULL,
  count        INTEGER NOT NULL DEFAULT 0,
  window_start TEXT NOT NULL,
  updated_at   TEXT NOT NULL,
  PRIMARY KEY(session_id, rule)
);
"""


def ensure_schema(conn: sqlite3.Connection) -> None:
    if conn is None:
        raise ValueError("guardrail: no database handle")
    conn.executescript(_SCHEMA)


def _now_ts() -> str:
    return _dt.datetime.now(_dt.timezone.utc).replace(microsecond=0).strftime("%Y-%m-%dT%H:%M:%SZ")


@dataclass
class Event:
    id: int
    created_at: str
    direction: str
    stage: str
    rule: str
    action: str
    blocked: bool
    undecidable: bool
    session_id: str = ""
    tool: str = ""
    detail: str = ""

    def to_json(self) -> dict[str, Any]:
        m: dict[str, Any] = {
            "id": self.id, "created_at": self.created_at, "direction": self.direction,
            "stage": self.stage, "rule": self.rule, "action": self.action,
            "blocked": self.blocked, "undecidable": self.undecidable,
        }
        if self.session_id:
            m["session_id"] = self.session_id
        if self.tool:
            m["tool"] = self.tool
        if self.detail:
            m["detail"] = self.detail
        return m


def history(conn: sqlite3.Connection, limit: int) -> list[Event]:
    ensure_schema(conn)
    if limit <= 0:
        limit = 50
    rows = conn.execute(
        "SELECT id, created_at, session_id, direction, stage, tool, rule, action, "
        "blocked, undecidable, detail FROM guardrail_events ORDER BY id DESC LIMIT ?",
        (limit,),
    ).fetchall()
    out: list[Event] = []
    for r in rows:
        out.append(Event(
            id=r[0], created_at=r[1], session_id=r[2] or "", direction=r[3], stage=r[4],
            tool=r[5] or "", rule=r[6], action=r[7], blocked=bool(r[8]), undecidable=bool(r[9]),
            detail=r[10] or "",
        ))
    return out


def sort_rules(rules: list[Rule]) -> None:
    """Order rules deterministically for display: block first, then by name."""
    rules.sort(key=lambda r: (0 if r.action == ACTION_BLOCK else 1, r.name))


# ---- processor ------------------------------------------------------------

class Processor:
    """Evaluates rules against content. An empty ruleset is inert."""

    def __init__(self, rules: list[Rule] | None = None, conn: sqlite3.Connection | None = None,
                 session_id: str = "", log: Any = None, quiet_warn: bool = False):
        self.rules = rules or []
        self.conn = conn
        self.session_id = session_id
        self.log = log
        self.quiet_warn = quiet_warn

    def enabled(self) -> bool:
        return len(self.rules) > 0

    def has_stage(self, stage: str) -> bool:
        if not self.enabled():
            return False
        return any(r.stage == "" or r.stage == stage for r in self.rules)

    def scan_args(self, tool: str, args: dict[str, Any]) -> Verdict:
        """Screen tool arguments. Serialisation failure FAILS CLOSED (block)."""
        if not self.has_stage(STAGE_TOOL_INPUT):
            return Verdict(stage=STAGE_TOOL_INPUT, tool=tool, findings=[])
        try:
            b = json.dumps(args, default=str)
        except (TypeError, ValueError) as exc:
            v = Verdict(
                stage=STAGE_TOOL_INPUT, tool=tool, findings=[], blocked=True, undecidable=True,
                reason=(f"guardrail could not serialise the arguments of tool {tool!r} for "
                        f"inspection ({exc}); blocking rather than allowing uninspected content"),
            )
            self.record(v)
            return v
        return self.scan(STAGE_TOOL_INPUT, tool, b)

    def scan(self, stage: str, tool: str, content: str) -> Verdict:
        v = Verdict(stage=stage, tool=tool, findings=[])
        if not self.enabled():
            return v
        if tool == "":
            tool = "-"
        for r in self.rules:
            if r.stage != "" and r.stage != stage:
                continue
            if not match_tool(r._tool_pat, tool):
                continue
            if r.type == TYPE_REQUIRE_APPROVAL:
                v.findings.append(Finding(
                    rule=r.name, type=r.type, stage=stage, tool=tool, detector="require-approval",
                    action=r.action,
                    message=r.describe(f"tool {r.tool!r} requires approval before it runs"),
                ))
                self._apply(v, r)
                continue
            hits = r.evaluate(content)
            if r.type == TYPE_TRIPWIRE:
                inc = 1
                if r.pattern or r.builtin:
                    inc = len(hits)
                    if inc == 0:
                        continue
                try:
                    count = self._bump(r, inc)
                except Exception as exc:  # noqa: BLE001 — fail closed on any counter error
                    v.blocked = True
                    v.undecidable = True
                    v.reason = (f"tripwire {r.name!r} could not read its counter ({exc}); "
                                f"blocking rather than silently not enforcing the threshold")
                    self.record(v)
                    return v
                if count < r.threshold:
                    continue
                v.findings.append(Finding(
                    rule=r.name, type=r.type, stage=stage, tool=tool, detector="tripwire",
                    action=r.action, count=count, threshold=r.threshold,
                    message=r.describe(
                        f"tripwire threshold reached: {count}/{r.threshold} matching events "
                        f"for {r.tool!r} within {r.window_text or format_duration(r.window_seconds)}"),
                ))
                self._apply(v, r)
                continue
            for h in hits:
                v.findings.append(Finding(
                    rule=r.name, type=r.type, stage=stage, tool=tool, detector=h.detector,
                    action=r.action, excerpt=h.excerpt, offset=h.offset,
                    message=r.describe(h.detector + " matched"),
                ))
                self._apply(v, r)

        if v.blocked and not v.reason:
            v.reason = _summarise(v.findings, ACTION_BLOCK)
        if v.interrupted and not v.reason:
            v.reason = _summarise(v.findings, ACTION_INTERRUPT)
        if v.warned and not v.reason:
            v.reason = _summarise(v.findings, ACTION_WARN)
        if v.fired():
            self.record(v)
        if v.warned and not v.blocked and self.log is not None and not self.quiet_warn:
            print(f"  guardrail WARNING ({stage}): {v.reason}", file=self.log)
        return v

    def _apply(self, v: Verdict, r: Rule) -> None:
        if r.action == ACTION_BLOCK:
            v.blocked = True
        elif r.action == ACTION_INTERRUPT:
            v.interrupted = True
        else:
            v.warned = True

    def _bump(self, r: Rule, by: int) -> int:
        """Increment a tripwire counter within its window; return the new count.
        Any error propagates so the caller FAILS CLOSED."""
        if self.conn is None:
            raise RuntimeError("no store is configured for tripwire counters")
        ensure_schema(self.conn)
        session = self.session_id or "-"
        cur = self.conn.execute(
            "SELECT count, window_start FROM tripwire_counters WHERE session_id=? AND rule=?",
            (session, r.name),
        ).fetchone()
        if cur is None:
            count, window_start = 0, _now_ts()
        else:
            count, window_start = int(cur[0]), cur[1]
            if r.window_seconds > 0:
                started = _parse_ts(window_start)
                if started is None:
                    count, window_start = 0, _now_ts()
                else:
                    age = (_dt.datetime.now(_dt.timezone.utc) - started).total_seconds()
                    if age > r.window_seconds:
                        count, window_start = 0, _now_ts()
        count += by
        self.conn.execute(
            "INSERT INTO tripwire_counters(session_id, rule, count, window_start, updated_at) "
            "VALUES(?,?,?,?,?) ON CONFLICT(session_id, rule) DO UPDATE SET count=excluded.count, "
            "window_start=excluded.window_start, updated_at=excluded.updated_at",
            (session, r.name, count, window_start, _now_ts()),
        )
        self.conn.commit()
        return count

    def record(self, v: Verdict) -> None:
        """Append guardrail_events rows. Best-effort (telemetry must not decide),
        but never silent: a failure is reported on log."""
        if self.conn is None:
            return
        try:
            ensure_schema(self.conn)
        except sqlite3.Error as exc:
            self._warnf(f"guardrail: could not prepare the event log ({exc}); the decision above still stands")
            return
        rows = v.findings or [Finding(rule="-", type="", stage=v.stage, tool=v.tool,
                                      action="block", message=v.reason)]
        for f in rows:
            try:
                self.conn.execute(
                    "INSERT INTO guardrail_events (created_at, session_id, direction, stage, tool, "
                    "rule, action, blocked, undecidable, detail) VALUES(?,?,?,?,?,?,?,?,?,?)",
                    (_now_ts(), self.session_id, "runtime", v.stage, v.tool, f.rule, f.action,
                     1 if v.blocked else 0, 1 if v.undecidable else 0,
                     (f.message + " " + f.excerpt).strip()),
                )
                self.conn.commit()
            except sqlite3.Error as exc:
                self._warnf(f"guardrail: could not append to guardrail_events ({exc}); the decision above still stands")
                return

    def _warnf(self, msg: str) -> None:
        if self.log is not None:
            print("  " + msg, file=self.log)

    # ---- permission.Screener adapter --------------------------------------

    def screen_tool_input(self, tool: str, args: dict[str, Any]) -> tuple[bool, bool, str]:
        """Pre-hook: returns (blocked, interrupt, reason). A fail-closed verdict is
        Blocked, never Interrupt, so it can never be waved past an approval gate."""
        if not self.has_stage(STAGE_TOOL_INPUT):
            return False, False, ""
        v = self.scan_args(tool, args)
        oc = v.outcome()
        if oc == ACTION_BLOCK:
            return True, False, v.reason
        if oc == ACTION_INTERRUPT:
            return False, True, v.reason
        return False, False, ""

    def screen_tool_result(self, tool: str, result: str) -> tuple[bool, str]:
        """Post-hook: returns (blocked, reason)."""
        if not self.has_stage(STAGE_TOOL_OUTPUT):
            return False, ""
        v = self.scan(STAGE_TOOL_OUTPUT, tool, result)
        return v.blocked, v.reason


def _summarise(findings: list[Finding], want: str) -> str:
    names = [f"{f.rule} ({f.message})" for f in findings if f.action == want]
    if not names:
        return ""
    verb = {ACTION_WARN: "warned", ACTION_INTERRUPT: "requires approval"}.get(want, "blocked")
    return f"guardrail {verb}: {'; '.join(names)}"


def _parse_ts(s: str) -> _dt.datetime | None:
    for fmt in ("%Y-%m-%dT%H:%M:%SZ", "%Y-%m-%dT%H:%M:%S%z"):
        try:
            dt = _dt.datetime.strptime(s, fmt)
            if dt.tzinfo is None:
                dt = dt.replace(tzinfo=_dt.timezone.utc)
            return dt
        except ValueError:
            continue
    try:
        dt = _dt.datetime.fromisoformat(s.replace("Z", "+00:00"))
        if dt.tzinfo is None:
            dt = dt.replace(tzinfo=_dt.timezone.utc)
        return dt
    except ValueError:
        return None
