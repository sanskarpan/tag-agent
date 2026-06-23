"""PRD-065: Automatic post-run memory extraction.

Invokes an LLM to extract structured memories from agent run outputs and
persists them to the semantic memory store (with deduplication).
"""
from __future__ import annotations

import json
import re
import shutil
import sqlite3
import subprocess
from dataclasses import dataclass, field
from datetime import datetime, timezone
from typing import Any


EXTRACTION_PROMPT = """\
You are a memory extraction assistant. Given a block of text from an AI agent session,
extract the most important reusable facts, decisions, gotchas, or conventions.

Return ONLY a JSON array (no prose, no markdown fences) like:
[
  {"content": "...", "memory_type": "fact", "confidence": 0.9},
  {"content": "...", "memory_type": "decision", "confidence": 0.8}
]

Rules:
- memory_type must be one of: fact, decision, gotcha, convention, other
- confidence must be a float 0.0-1.0
- content must be a standalone, self-contained statement (no dangling pronouns or implicit context)
- Only extract high-value, reusable knowledge; skip procedural chatter and ephemeral details
- Return [] if nothing significant is found
"""


@dataclass
class ExtractionConfig:
    enabled: bool = False
    profile: str = ""
    min_confidence: float = 0.7
    max_memories_per_run: int = 5
    memory_types: list[str] = field(default_factory=lambda: ["fact", "decision", "gotcha"])
    dedup_similarity_threshold: float = 0.8


@dataclass
class ExtractedMemory:
    content: str
    memory_type: str
    confidence: float
    source_run_id: str
    extracted_at: str


def get_extraction_config(profile: str, cfg: dict) -> ExtractionConfig:
    """Read auto-extract config from cfg["profiles"][profile]["config"]["memory"]["auto_extract"]."""
    try:
        mem_cfg: dict = (
            cfg.get("profiles", {})
            .get(profile, {})
            .get("config", {})
            .get("memory", {})
            .get("auto_extract", {})
        )
        return ExtractionConfig(
            enabled=bool(mem_cfg.get("enabled", False)),
            profile=profile,
            min_confidence=float(mem_cfg.get("min_confidence", 0.7)),
            max_memories_per_run=int(mem_cfg.get("max_memories_per_run", 5)),
            memory_types=list(mem_cfg.get("memory_types", ["fact", "decision", "gotcha"])),
            dedup_similarity_threshold=float(mem_cfg.get("dedup_similarity_threshold", 0.8)),
        )
    except Exception:
        return ExtractionConfig(enabled=False, profile=profile)


def _jaccard_similarity(a: str, b: str) -> float:
    """Compute Jaccard similarity between two strings based on word sets."""
    words_a = set(a.lower().split())
    words_b = set(b.lower().split())
    if not words_a or not words_b:
        return 0.0
    intersection = len(words_a & words_b)
    union = len(words_a | words_b)
    return intersection / union if union else 0.0


def _is_duplicate(content: str, existing_memories: list[dict], threshold: float) -> bool:
    """Return True if any existing memory shares > threshold Jaccard similarity with content."""
    for mem in existing_memories:
        if _jaccard_similarity(content, mem.get("content", "")) >= threshold:
            return True
    return False


def _find_tag_bin() -> str | None:
    """Locate the tag or tag-agent binary on PATH."""
    for name in ("tag", "tag-agent"):
        found = shutil.which(name)
        if found:
            return found
    return None


def _parse_extraction_response(text: str) -> list[dict]:
    """Extract a JSON array from the LLM response text."""
    match = re.search(r'\[.*\]', text, re.DOTALL)
    if not match:
        return []
    try:
        data = json.loads(match.group())
        if isinstance(data, list):
            return data
    except Exception:
        pass
    return []


def extract_from_output(
    output: str,
    run_id: str,
    profile: str,
    cfg: dict,
    *,
    config: ExtractionConfig | None = None,
) -> list[ExtractedMemory]:
    """Build extraction prompt, invoke TAG runtime, parse and filter results.

    Returns up to config.max_memories_per_run ExtractedMemory objects that
    pass the min_confidence and memory_types filters.
    """
    if config is None:
        config = get_extraction_config(profile, cfg)

    prompt = (
        f"{EXTRACTION_PROMPT}\n\n"
        f"<session_output>\n{output[:4000]}\n</session_output>"
    )

    tag_bin = _find_tag_bin()
    if not tag_bin:
        return []

    try:
        result = subprocess.run(
            [tag_bin, "-q", prompt, "-Q"],
            capture_output=True,
            text=True,
            timeout=60,
        )
        text = result.stdout.strip()
    except Exception:
        return []

    raw_memories = _parse_extraction_response(text)

    now = datetime.now(timezone.utc).isoformat()
    extracted: list[ExtractedMemory] = []
    for m in raw_memories:
        try:
            mtype = str(m.get("memory_type", "fact"))
            conf = float(m.get("confidence", 0.5))
            content = str(m.get("content", "")).strip()
            if not content:
                continue
            if conf < config.min_confidence:
                continue
            if mtype not in config.memory_types:
                continue
            extracted.append(ExtractedMemory(
                content=content,
                memory_type=mtype,
                confidence=conf,
                source_run_id=run_id,
                extracted_at=now,
            ))
            if len(extracted) >= config.max_memories_per_run:
                break
        except Exception:
            continue

    return extracted


def save_extracted_memories(
    conn: sqlite3.Connection,
    memories: list[ExtractedMemory],
    *,
    profile: str = "",
    dedup_threshold: float = 0.8,
) -> int:
    """Persist memories to the semantic memory store with deduplication.

    For each memory: checks for duplicates among existing memories for the
    same profile (Jaccard similarity), then calls semantic_memory.add_memory().
    Returns count of actually saved memories.
    """
    from tag import semantic_memory  # local import to avoid circular deps

    semantic_memory.ensure_schema(conn)
    ensure_schema(conn)

    # Load existing memories for dedup check
    existing: list[dict] = []
    try:
        rows = conn.execute(
            "SELECT content FROM semantic_memories WHERE profile=?",
            (profile,),
        ).fetchall()
        existing = [{"content": r[0]} for r in rows]
    except Exception:
        pass

    saved = 0
    for mem in memories:
        if _is_duplicate(mem.content, existing, dedup_threshold):
            continue
        try:
            semantic_memory.add_memory(
                conn,
                profile,
                mem.content,
                memory_type=mem.memory_type,
                confidence=mem.confidence,
                source=f"auto:{mem.source_run_id[:8]}",
            )
            # Update source_run_id column if it exists
            try:
                conn.execute(
                    """UPDATE semantic_memories SET source_run_id=?
                       WHERE profile=? AND content=? AND source_run_id IS NULL""",
                    (mem.source_run_id, profile, mem.content),
                )
                conn.commit()
            except Exception:
                pass
            existing.append({"content": mem.content})
            saved += 1
        except Exception:
            continue

    return saved


def auto_extract_post_run(
    conn: sqlite3.Connection,
    run_id: str,
    output: str,
    profile: str,
    cfg: dict,
) -> int:
    """Check if auto-extract is enabled for profile; if so, extract and save.

    Returns count of saved memories (0 if disabled or nothing extracted).
    """
    config = get_extraction_config(profile, cfg)
    if not config.enabled:
        return 0
    memories = extract_from_output(output, run_id, profile, cfg, config=config)
    return save_extracted_memories(
        conn,
        memories,
        profile=profile,
        dedup_threshold=config.dedup_similarity_threshold,
    )


def ensure_schema(conn: sqlite3.Connection) -> None:
    """ALTER TABLE semantic_memories ADD COLUMN source_run_id TEXT if not exists."""
    try:
        conn.execute("ALTER TABLE semantic_memories ADD COLUMN source_run_id TEXT")
        conn.commit()
    except Exception:
        # Column already exists or table doesn't exist yet — both are fine
        pass
