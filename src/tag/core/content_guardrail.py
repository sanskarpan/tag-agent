"""Input/output content guardrails (PRD-121 output, PRD-122 input) built on the
shared GuardrailResult type (PRD-124).

This is a different layer from the runtime tripwire engine (PRD-123, in
``tag.core.guardrail``): that one screens tool I/O by *stage* with pattern /
tripwire / require-approval rules; this one screens the model's INPUT and OUTPUT
content with typed detectors (prompt-injection, PII, secret, json-schema,
length-limit, profanity, topic-filter, toxicity) that each return a
``GuardrailResult`` and can block / warn / sanitize / rewrite.

Config lives in the ``input_guardrail_configs`` / ``output_guardrail_configs``
tables; every decision is appended to the shared ``guardrail_events`` audit log.
"""
from __future__ import annotations

import datetime as _dt
import json
import re
import sqlite3
import uuid
from dataclasses import dataclass, field
from typing import Any, Callable

# ---------------------------------------------------------------------------
# PRD-124: shared GuardrailResult value type
# ---------------------------------------------------------------------------

ACTION_PASS = "pass"
ACTION_BLOCK = "block"
ACTION_SANITIZE = "sanitize"
ACTION_WARN = "warn"
ACTION_INTERRUPT = "interrupt"
ACTION_REWRITE = "rewrite"  # PRD-121 output-only remediation action

_ALL_ACTIONS = {ACTION_PASS, ACTION_BLOCK, ACTION_SANITIZE, ACTION_WARN,
                ACTION_INTERRUPT, ACTION_REWRITE}


@dataclass
class GuardrailResult:
    """Standardized return type for every content-guardrail check (PRD-124).

    Passed and returned by value; guardrails never share a mutable result.
    """
    action: str
    guardrail: str
    reason: str = ""
    sanitized_text: str | None = None
    message: str | None = None
    metadata: dict[str, Any] = field(default_factory=dict)

    def is_blocking(self) -> bool:
        """True when downstream processing should stop (block or interrupt)."""
        return self.action in (ACTION_BLOCK, ACTION_INTERRUPT)

    def should_sanitize(self) -> bool:
        return self.action == ACTION_SANITIZE and self.sanitized_text is not None

    def fired(self) -> bool:
        return self.action != ACTION_PASS

    def to_dict(self) -> dict[str, Any]:
        out: dict[str, Any] = {
            "action": self.action, "reason": self.reason, "guardrail": self.guardrail,
            "sanitized_text": self.sanitized_text, "message": self.message,
        }
        if self.metadata:
            out["metadata"] = self.metadata
        return out

    def __str__(self) -> str:  # log-friendly
        return (f"GuardrailResult(action={self.action}, reason={self.reason!r}, "
                f"guardrail={self.guardrail!r})")


def result_pass(guardrail: str) -> GuardrailResult:
    return GuardrailResult(action=ACTION_PASS, guardrail=guardrail)


def result_block(reason: str, guardrail: str, message: str | None = None) -> GuardrailResult:
    return GuardrailResult(action=ACTION_BLOCK, reason=reason, guardrail=guardrail, message=message)


def result_sanitize(sanitized: str, reason: str, guardrail: str) -> GuardrailResult:
    return GuardrailResult(action=ACTION_SANITIZE, reason=reason, guardrail=guardrail,
                           sanitized_text=sanitized)


def result_warn(reason: str, guardrail: str) -> GuardrailResult:
    return GuardrailResult(action=ACTION_WARN, reason=reason, guardrail=guardrail)


# ---------------------------------------------------------------------------
# Detectors
# ---------------------------------------------------------------------------

_RE_EMAIL = re.compile(r"[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}")
_RE_SSN = re.compile(r"\b\d{3}-\d{2}-\d{4}\b")
_RE_PHONE = re.compile(r"\b(?:\+?1[-.\s]?)?\(?\d{3}\)?[-.\s]\d{3}[-.\s]\d{4}\b")
_RE_CC = re.compile(r"\b(?:\d[ -]?){13,16}\b")

# Default prompt-injection patterns (RE2-safe: no backrefs/lookaround), matched
# case-insensitively. Operators can override via the rule's config.
DEFAULT_INJECTION_PATTERNS = [
    r"ignore (all )?(previous|prior|above) (instructions|prompts|context)",
    r"disregard (all )?(previous|prior) (instructions|prompts)",
    r"you are now (a|an) (dan|jailbreak|unrestricted)",
    r"output your (system prompt|instructions|context)",
    r"reveal your (system prompt|instructions)",
    r"act as if you have no (restrictions|guidelines|safety)",
    r"pretend (you are|to be) (an ai without|a model without) (restrictions|safety)",
    r"\bjailbreak\b",
    r"\[system\]",
]

# A small, conservative profanity seed list (word-boundary matched). Operators
# extend it via the rule config's "words" list.
DEFAULT_PROFANITY = ["damn", "shit", "fuck", "bitch", "asshole", "bastard"]


def _luhn_ok(digits: str) -> bool:
    """Luhn check to reduce credit-card false positives."""
    nums = [int(c) for c in digits if c.isdigit()]
    if len(nums) < 13:
        return False
    total = 0
    for i, n in enumerate(reversed(nums)):
        if i % 2 == 1:
            n *= 2
            if n > 9:
                n -= 9
        total += n
    return total % 10 == 0


def detect_pii(text: str) -> list[tuple[str, str]]:
    """Return a list of (kind, match) PII hits."""
    hits: list[tuple[str, str]] = []
    for m in _RE_EMAIL.finditer(text):
        hits.append(("email", m.group(0)))
    for m in _RE_SSN.finditer(text):
        hits.append(("SSN", m.group(0)))
    for m in _RE_PHONE.finditer(text):
        hits.append(("phone", m.group(0)))
    for m in _RE_CC.finditer(text):
        if _luhn_ok(m.group(0)):
            hits.append(("credit-card", m.group(0)))
    return hits


def sanitize_pii(text: str) -> str:
    """Replace PII with deterministic placeholders (PRD-122 sanitize)."""
    text = _RE_EMAIL.sub("[REDACTED_EMAIL]", text)
    text = _RE_SSN.sub("[REDACTED_SSN]", text)
    text = _RE_PHONE.sub("[REDACTED_PHONE]", text)
    # Credit cards: only redact Luhn-valid runs so we don't clobber ordinary numbers.
    def _cc(m: "re.Match[str]") -> str:
        return "[REDACTED_CC]" if _luhn_ok(m.group(0)) else m.group(0)
    text = _RE_CC.sub(_cc, text)
    return text


def detect_secrets(text: str) -> list[tuple[str, str]]:
    """Reuse the PRD-123 secret detectors so input/output share one scanner."""
    from tag.core.guardrail import builtin_scan  # noqa: PLC0415
    hits = builtin_scan("secrets", text)
    return [(h.detector.split(":", 1)[-1], h.excerpt) for h in hits]


def compile_injection_patterns(patterns: list[str]) -> list[re.Pattern[str]]:
    out: list[re.Pattern[str]] = []
    for p in patterns:
        try:
            out.append(re.compile(p if p.startswith("(?") else "(?i)" + p))
        except re.error as exc:
            raise ValueError(f"injection pattern {p!r} is invalid: {exc}")
    return out


# ---------------------------------------------------------------------------
# Guardrail evaluation — one function per type
# ---------------------------------------------------------------------------

def _redact(s: str) -> str:
    from tag.core.guardrail import redact  # noqa: PLC0415
    return redact(s)


def eval_guardrail(gtype: str, action: str, text: str, config: dict[str, Any]) -> GuardrailResult:
    """Evaluate one content guardrail against *text*. Returns a GuardrailResult.

    Types: pii, secret, prompt-injection, length-limit, json-schema, profanity,
    topic-filter, toxicity. Unknown/LLM-only types that cannot run offline pass
    with an explanatory note rather than failing closed here (the CLI reports it).
    """
    name = gtype

    if gtype == "pii":
        hits = detect_pii(text)
        if not hits:
            return result_pass(name)
        if action == ACTION_SANITIZE:
            return result_sanitize(sanitize_pii(text), "PII_SANITIZED", name)
        kinds = ",".join(sorted({k for k, _ in hits}))
        return GuardrailResult(action=action, guardrail=name,
                               reason=f"PII_DETECTED:{kinds}")

    if gtype == "secret":
        hits = detect_secrets(text)
        if not hits:
            return result_pass(name)
        return GuardrailResult(action=action, guardrail=name,
                               reason=f"SECRET_DETECTED:{hits[0][0]}")

    if gtype == "prompt-injection":
        pats = config.get("patterns") or DEFAULT_INJECTION_PATTERNS
        for i, rx in enumerate(compile_injection_patterns(pats)):
            if rx.search(text):
                return GuardrailResult(action=action, guardrail=name,
                                       reason=f"PROMPT_INJECTION:pattern_{i}")
        return result_pass(name)

    if gtype == "length-limit":
        max_len = int(config.get("max_length", 4096))
        if len(text) > max_len:
            return GuardrailResult(action=action, guardrail=name,
                                   reason=f"INPUT_TOO_LONG:{len(text)}>{max_len}")
        return result_pass(name)

    if gtype == "json-schema":
        try:
            obj = json.loads(text)
        except json.JSONDecodeError as exc:
            return GuardrailResult(action=action, guardrail=name,
                                   reason=f"SCHEMA_INVALID:not JSON: {str(exc)[:80]}")
        schema = config.get("schema")
        if schema:
            err = _validate_json_schema(obj, schema)
            if err:
                return GuardrailResult(action=action, guardrail=name,
                                       reason=f"SCHEMA_INVALID:{err[:100]}")
        return result_pass(name)

    if gtype == "profanity":
        words = [w.lower() for w in (config.get("words") or DEFAULT_PROFANITY)]
        low = text.lower()
        for w in words:
            if re.search(r"\b" + re.escape(w) + r"\b", low):
                return GuardrailResult(action=action, guardrail=name,
                                       reason="PROFANITY_DETECTED")
        return result_pass(name)

    if gtype in ("topic-filter", "toxicity"):
        # These require an embedding/classification model. They pass (with a
        # note) when no model is configured, rather than blocking blindly — the
        # LLM path is opt-in (PRD-122 NFR-02). A configured model is honoured by
        # the runtime integration; the CLI dry-run reports the degradation.
        return GuardrailResult(action=ACTION_PASS, guardrail=name,
                               reason="", message="requires a classifier model (not run offline)",
                               metadata={"llm_required": True})

    # Unknown type: pass with a note rather than crash.
    return GuardrailResult(action=ACTION_PASS, guardrail=name,
                           reason="", message=f"unknown guardrail type {gtype!r}")


def _validate_json_schema(obj: Any, schema: dict[str, Any]) -> str | None:
    """Minimal JSON-schema validation (type/required/properties). Returns an
    error string or None. Uses ``jsonschema`` if installed, else a small
    built-in checker covering the common cases the PRD lists."""
    try:
        import jsonschema  # noqa: PLC0415
        try:
            jsonschema.validate(obj, schema)
            return None
        except jsonschema.ValidationError as exc:  # type: ignore[attr-defined]
            return str(exc.message)
    except ImportError:
        return _validate_json_schema_builtin(obj, schema)


_JSON_TYPES: dict[str, tuple[type, ...]] = {
    "object": (dict,), "array": (list,), "string": (str,),
    "number": (int, float), "integer": (int,), "boolean": (bool,), "null": (type(None),),
}


def _validate_json_schema_builtin(obj: Any, schema: dict[str, Any]) -> str | None:
    t = schema.get("type")
    if t:
        expected = _JSON_TYPES.get(t)
        if expected:
            # bool is a subclass of int — exclude it from integer/number.
            if t in ("integer", "number") and isinstance(obj, bool):
                return f"expected {t}, got boolean"
            if not isinstance(obj, expected):
                return f"expected {t}, got {type(obj).__name__}"
    if t == "object" or (t is None and isinstance(obj, dict)):
        if isinstance(obj, dict):
            for req in schema.get("required", []):
                if req not in obj:
                    return f"missing required property {req!r}"
            props = schema.get("properties", {})
            for key, subschema in props.items():
                if key in obj:
                    err = _validate_json_schema_builtin(obj[key], subschema)
                    if err:
                        return f"{key}: {err}"
    if t == "array" and isinstance(obj, list):
        item_schema = schema.get("items")
        if item_schema:
            for i, item in enumerate(obj):
                err = _validate_json_schema_builtin(item, item_schema)
                if err:
                    return f"[{i}]: {err}"
    return None


# ---------------------------------------------------------------------------
# Config persistence + audit log
# ---------------------------------------------------------------------------

_CONFIG_SCHEMA = """
CREATE TABLE IF NOT EXISTS {table} (
  id                TEXT PRIMARY KEY,
  profile           TEXT NOT NULL,
  guardrail_type    TEXT NOT NULL,
  action            TEXT NOT NULL DEFAULT 'block',
  config_json       TEXT,
  severity          TEXT NOT NULL DEFAULT 'high',
  enabled           INTEGER NOT NULL DEFAULT 1,
  {extra}
  created_at        TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_{table}_profile ON {table}(profile);
"""

_SEVERITY_RANK = {"high": 0, "medium": 1, "low": 2}
VALID_ACTIONS_INPUT = {ACTION_BLOCK, ACTION_SANITIZE, ACTION_WARN}
VALID_ACTIONS_OUTPUT = {ACTION_BLOCK, ACTION_REWRITE, ACTION_WARN}
VALID_TYPES_INPUT = {"prompt-injection", "pii", "secret", "topic-filter", "length-limit", "custom"}
VALID_TYPES_OUTPUT = {"pii", "secret", "json-schema", "topic-filter", "profanity", "toxicity", "custom"}


def _table(direction: str) -> str:
    return "input_guardrail_configs" if direction == "input" else "output_guardrail_configs"


def _extra_col(direction: str) -> str:
    # input uses classifier_model, output uses remediation_model (PRD-121/122 DDL)
    return ("classifier_model  TEXT," if direction == "input"
            else "remediation_model TEXT,")


def ensure_content_schema(conn: sqlite3.Connection, direction: str) -> None:
    conn.executescript(_CONFIG_SCHEMA.format(table=_table(direction), extra=_extra_col(direction)))
    # Ensure the shared audit log exists too (shared with PRD-123).
    from tag.core.guardrail import ensure_schema as _ensure_events  # noqa: PLC0415
    _ensure_events(conn)


def _now() -> str:
    return _dt.datetime.now(_dt.timezone.utc).replace(microsecond=0).strftime("%Y-%m-%dT%H:%M:%SZ")


def add_config(conn: sqlite3.Connection, direction: str, *, profile: str, gtype: str,
               action: str, config: dict[str, Any], severity: str = "high",
               model: str | None = None) -> str:
    ensure_content_schema(conn, direction)
    cid = uuid.uuid4().hex[:12]
    model_col = "classifier_model" if direction == "input" else "remediation_model"
    conn.execute(
        f"INSERT INTO {_table(direction)} "
        f"(id, profile, guardrail_type, action, config_json, severity, enabled, {model_col}, created_at) "
        f"VALUES (?,?,?,?,?,?,1,?,?)",
        (cid, profile, gtype, action, json.dumps(config), severity, model, _now()),
    )
    conn.commit()
    return cid


def list_configs(conn: sqlite3.Connection, direction: str, profile: str | None = None) -> list[dict[str, Any]]:
    ensure_content_schema(conn, direction)
    q = f"SELECT id, profile, guardrail_type, action, config_json, severity, enabled FROM {_table(direction)}"
    args: tuple = ()
    if profile:
        q += " WHERE profile=?"
        args = (profile,)
    q += " ORDER BY CASE severity WHEN 'high' THEN 0 WHEN 'medium' THEN 1 ELSE 2 END, created_at"
    rows = conn.execute(q, args).fetchall()
    out = []
    for r in rows:
        out.append({
            "id": r[0], "profile": r[1], "guardrail_type": r[2], "action": r[3],
            "config": json.loads(r[4] or "{}"), "severity": r[5], "enabled": bool(r[6]),
        })
    return out


def remove_config(conn: sqlite3.Connection, direction: str, config_id: str) -> bool:
    ensure_content_schema(conn, direction)
    cur = conn.execute(f"DELETE FROM {_table(direction)} WHERE id=?", (config_id,))
    conn.commit()
    return cur.rowcount > 0


def _append_event(conn: sqlite3.Connection, direction: str, profile: str,
                  run_id: str, res: GuardrailResult) -> None:
    """Append to the shared guardrail_events audit log (best-effort)."""
    try:
        from tag.core.guardrail import ensure_schema as _ensure  # noqa: PLC0415
        _ensure(conn)
        conn.execute(
            "INSERT INTO guardrail_events "
            "(created_at, session_id, direction, stage, tool, rule, action, blocked, undecidable, detail) "
            "VALUES (?,?,?,?,?,?,?,?,0,?)",
            (_now(), run_id, direction, res.guardrail, profile, res.guardrail,
             res.action, 1 if res.is_blocking() else 0,
             (res.reason + (" " + _redact(res.sanitized_text) if res.sanitized_text else "")).strip()),
        )
        conn.commit()
    except sqlite3.Error:
        pass


@dataclass
class ChainVerdict:
    """The collapsed result of running a content-guardrail chain."""
    direction: str
    results: list[GuardrailResult] = field(default_factory=list)
    final_action: str = ACTION_PASS
    text: str = ""  # possibly-sanitized text after the chain

    def to_dict(self) -> dict[str, Any]:
        return {
            "direction": self.direction, "final_action": self.final_action,
            "text": self.text, "results": [r.to_dict() for r in self.results],
        }


def run_chain(conn: sqlite3.Connection, direction: str, profile: str, text: str,
              *, run_id: str = "", persist: bool = True) -> ChainVerdict:
    """Run every enabled guardrail for *profile* in severity order.

    - block/interrupt short-circuits (PRD-121 FR-01 / PRD-122 FR-01).
    - sanitize (input) threads the rewritten text to later guardrails and the
      final output (PRD-122 FR-07).
    - warn/rewrite are recorded but do not stop the chain.
    Every decision is appended to guardrail_events when *persist* is set.
    """
    cfgs = [c for c in list_configs(conn, direction, profile) if c["enabled"]]
    verdict = ChainVerdict(direction=direction, text=text)
    current = text
    for c in cfgs:
        res = eval_guardrail(c["guardrail_type"], c["action"], current, c["config"])
        verdict.results.append(res)
        if persist:
            _append_event(conn, direction, profile, run_id, res)
        if res.is_blocking():
            verdict.final_action = res.action
            verdict.text = current
            return verdict
        if res.should_sanitize():
            current = res.sanitized_text or current
            if verdict.final_action == ACTION_PASS:
                verdict.final_action = ACTION_SANITIZE
        elif res.action in (ACTION_WARN, ACTION_REWRITE) and verdict.final_action == ACTION_PASS:
            verdict.final_action = res.action
    verdict.text = current
    return verdict
