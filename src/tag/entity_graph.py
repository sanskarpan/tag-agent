"""PRD-070: Entity-relationship graph with community detection.

Builds a lightweight entity-relation graph from agent memories using local
heuristics (no LLM call required). Communities are detected via union-find
on connected components.
"""
from __future__ import annotations

import re
import sqlite3
import uuid
from dataclasses import dataclass, field
from datetime import datetime, timezone


# ---------------------------------------------------------------------------
# Tech keyword catalogue for local entity extraction
# ---------------------------------------------------------------------------
_TECH_KEYWORDS: set[str] = {
    "python", "javascript", "typescript", "rust", "go", "java", "ruby", "c++",
    "redis", "postgres", "postgresql", "mysql", "sqlite", "mongodb", "kafka",
    "docker", "kubernetes", "k8s", "terraform", "ansible", "nginx", "fastapi",
    "django", "flask", "react", "vue", "angular", "node", "nodejs", "graphql",
    "grpc", "rest", "openai", "anthropic", "claude", "gpt", "llm", "ai", "ml",
    "github", "gitlab", "bitbucket", "jenkins", "circleci", "github actions",
    "aws", "gcp", "azure", "lambda", "s3", "ec2", "ecs", "gke", "aks",
    "linear", "jira", "slack", "notion", "figma",
}

_ENTITY_TYPES = {
    "person": "person",
    "organization": "organization",
    "concept": "concept",
    "technology": "technology",
    "place": "place",
    "event": "event",
    "other": "other",
}

_RELATION_TYPES = [
    "is_a", "has", "uses", "depends_on", "works_at",
    "related_to", "causes", "contradicts",
]


def _utc_now() -> str:
    return datetime.now(timezone.utc).isoformat()


# ---------------------------------------------------------------------------
# Dataclasses
# ---------------------------------------------------------------------------

@dataclass
class Entity:
    id: str
    name: str
    entity_type: str
    description: str
    confidence: float
    profile: str
    created_at: str
    mention_count: int = 1


@dataclass
class Relation:
    id: str
    source_entity_id: str
    target_entity_id: str
    relation_type: str
    confidence: float
    source_memory_id: str | None
    created_at: str


@dataclass
class Community:
    id: str
    member_entity_ids: list[str]
    label: str
    cohesion_score: float


# ---------------------------------------------------------------------------
# Schema
# ---------------------------------------------------------------------------

def ensure_schema(conn: sqlite3.Connection) -> None:
    conn.executescript("""
        CREATE TABLE IF NOT EXISTS entities (
            id            TEXT PRIMARY KEY,
            name          TEXT NOT NULL,
            entity_type   TEXT NOT NULL DEFAULT 'other',
            description   TEXT NOT NULL DEFAULT '',
            confidence    REAL NOT NULL DEFAULT 1.0,
            profile       TEXT NOT NULL,
            created_at    TEXT NOT NULL,
            mention_count INTEGER NOT NULL DEFAULT 1
        );
        CREATE INDEX IF NOT EXISTS idx_ent_profile ON entities(profile, entity_type);
        CREATE INDEX IF NOT EXISTS idx_ent_name ON entities(profile, name COLLATE NOCASE);

        CREATE TABLE IF NOT EXISTS relations (
            id                TEXT PRIMARY KEY,
            source_entity_id  TEXT NOT NULL,
            target_entity_id  TEXT NOT NULL,
            relation_type     TEXT NOT NULL DEFAULT 'related_to',
            confidence        REAL NOT NULL DEFAULT 1.0,
            source_memory_id  TEXT,
            created_at        TEXT NOT NULL
        );
        CREATE INDEX IF NOT EXISTS idx_rel_source ON relations(source_entity_id);
        CREATE INDEX IF NOT EXISTS idx_rel_target ON relations(target_entity_id);

        CREATE TABLE IF NOT EXISTS entity_communities (
            id                TEXT PRIMARY KEY,
            member_ids_json   TEXT NOT NULL,
            label             TEXT NOT NULL,
            cohesion_score    REAL NOT NULL DEFAULT 0.5,
            profile           TEXT NOT NULL,
            computed_at       TEXT NOT NULL
        );
    """)
    conn.commit()


# ---------------------------------------------------------------------------
# Entity extraction (local, no LLM)
# ---------------------------------------------------------------------------

def extract_entities_from_memory(
    memory_content: str, profile: str
) -> list[dict]:
    found: list[dict] = []

    # Tech keywords (case-insensitive)
    lower = memory_content.lower()
    for kw in _TECH_KEYWORDS:
        if kw in lower:
            found.append({
                "name": kw.title() if not kw[0].isupper() else kw,
                "entity_type": "technology",
                "confidence": 0.8,
            })

    # Capitalized multi-word phrases (likely proper nouns / organizations / people)
    cap_phrases = re.findall(r'\b[A-Z][a-zA-Z]+(?:\s+[A-Z][a-zA-Z]+)*\b', memory_content)
    for phrase in cap_phrases:
        if len(phrase) < 3:
            continue
        # Skip if it's a tech keyword already captured
        if phrase.lower() in _TECH_KEYWORDS:
            continue
        # Heuristic type classification
        if any(suf in phrase for suf in ("Inc", "Corp", "Ltd", "LLC", "GmbH")):
            etype = "organization"
        elif re.match(r'^[A-Z][a-z]+ [A-Z][a-z]+$', phrase):
            etype = "person"
        else:
            etype = "other"
        found.append({"name": phrase, "entity_type": etype, "confidence": 0.6})

    # Deduplicate by name (case-insensitive)
    seen: set[str] = set()
    deduped: list[dict] = []
    for e in found:
        key = e["name"].lower()
        if key not in seen:
            seen.add(key)
            deduped.append(e)

    return deduped


# ---------------------------------------------------------------------------
# CRUD
# ---------------------------------------------------------------------------

def _row_to_entity(row: sqlite3.Row) -> Entity:
    return Entity(
        id=row["id"], name=row["name"], entity_type=row["entity_type"],
        description=row["description"], confidence=row["confidence"],
        profile=row["profile"], created_at=row["created_at"],
        mention_count=row["mention_count"],
    )


def add_entity(
    conn: sqlite3.Connection,
    name: str,
    entity_type: str,
    profile: str,
    *,
    description: str = "",
    confidence: float = 1.0,
) -> Entity:
    ensure_schema(conn)
    conn.row_factory = sqlite3.Row
    # Check for existing (case-insensitive)
    existing = conn.execute(
        "SELECT * FROM entities WHERE profile=? AND name=? COLLATE NOCASE",
        (profile, name),
    ).fetchone()
    if existing:
        new_count = existing["mention_count"] + 1
        new_conf = max(existing["confidence"], confidence)
        conn.execute(
            "UPDATE entities SET mention_count=?, confidence=? WHERE id=?",
            (new_count, new_conf, existing["id"]),
        )
        conn.commit()
        ent = conn.execute("SELECT * FROM entities WHERE id=?", (existing["id"],)).fetchone()
        conn.row_factory = None
        return _row_to_entity(ent)

    ent_id = uuid.uuid4().hex[:12]
    now = _utc_now()
    conn.execute(
        """INSERT INTO entities(id,name,entity_type,description,confidence,profile,created_at,mention_count)
           VALUES(?,?,?,?,?,?,?,1)""",
        (ent_id, name, entity_type, description, confidence, profile, now),
    )
    conn.commit()
    ent = conn.execute("SELECT * FROM entities WHERE id=?", (ent_id,)).fetchone()
    conn.row_factory = None
    return _row_to_entity(ent)


def add_relation(
    conn: sqlite3.Connection,
    source_id: str,
    target_id: str,
    relation_type: str,
    *,
    confidence: float = 1.0,
    source_memory_id: str | None = None,
) -> Relation:
    ensure_schema(conn)
    rel_id = uuid.uuid4().hex[:12]
    now = _utc_now()
    conn.execute(
        """INSERT INTO relations(id,source_entity_id,target_entity_id,relation_type,
           confidence,source_memory_id,created_at) VALUES(?,?,?,?,?,?,?)""",
        (rel_id, source_id, target_id, relation_type, confidence, source_memory_id, now),
    )
    conn.commit()
    return Relation(
        id=rel_id, source_entity_id=source_id, target_entity_id=target_id,
        relation_type=relation_type, confidence=confidence,
        source_memory_id=source_memory_id, created_at=now,
    )


def extract_and_store_from_memory(
    conn: sqlite3.Connection,
    memory_id: str,
    memory_content: str,
    profile: str,
) -> tuple[int, int]:
    entities_raw = extract_entities_from_memory(memory_content, profile)
    entity_ids: list[str] = []
    for e in entities_raw:
        ent = add_entity(conn, e["name"], e["entity_type"], profile,
                         confidence=e["confidence"])
        entity_ids.append(ent.id)

    # Create "related_to" relations between co-occurring entities
    rels_added = 0
    for i in range(len(entity_ids)):
        for j in range(i + 1, len(entity_ids)):
            try:
                add_relation(conn, entity_ids[i], entity_ids[j], "related_to",
                             confidence=0.5, source_memory_id=memory_id)
                rels_added += 1
            except Exception:
                pass

    return len(entity_ids), rels_added


# ---------------------------------------------------------------------------
# Community detection (union-find)
# ---------------------------------------------------------------------------

def _union_find(nodes: list[str], edges: list[tuple[str, str]]) -> dict[str, str]:
    parent = {n: n for n in nodes}

    def find(x: str) -> str:
        while parent[x] != x:
            parent[x] = parent[parent[x]]
            x = parent[x]
        return x

    def union(a: str, b: str) -> None:
        ra, rb = find(a), find(b)
        if ra != rb:
            parent[ra] = rb

    for a, b in edges:
        if a in parent and b in parent:
            union(a, b)

    return {n: find(n) for n in nodes}


def detect_communities(conn: sqlite3.Connection, profile: str) -> list[Community]:
    ensure_schema(conn)
    entities = conn.execute(
        "SELECT id, name, mention_count FROM entities WHERE profile=?", (profile,)
    ).fetchall()
    relations = conn.execute(
        """SELECT r.source_entity_id, r.target_entity_id
           FROM relations r
           JOIN entities e1 ON r.source_entity_id=e1.id
           JOIN entities e2 ON r.target_entity_id=e2.id
           WHERE e1.profile=? AND e2.profile=?""",
        (profile, profile),
    ).fetchall()

    node_ids = [e[0] for e in entities]
    edges = [(r[0], r[1]) for r in relations]
    name_map = {e[0]: e[1] for e in entities}
    count_map = {e[0]: e[2] for e in entities}

    if not node_ids:
        return []

    membership = _union_find(node_ids, edges)

    # Group nodes by root
    groups: dict[str, list[str]] = {}
    for node_id, root in membership.items():
        groups.setdefault(root, []).append(node_id)

    communities: list[Community] = []
    for root, members in groups.items():
        # Label with the most-mentioned entity
        label = max(members, key=lambda m: count_map.get(m, 1))
        label_name = name_map.get(label, label)
        cohesion = min(1.0, len(members) / max(1, len(node_ids)))
        communities.append(Community(
            id=uuid.uuid4().hex[:12],
            member_entity_ids=members,
            label=label_name,
            cohesion_score=cohesion,
        ))

    return sorted(communities, key=lambda c: -len(c.member_entity_ids))


def query_graph(
    conn: sqlite3.Connection,
    profile: str,
    *,
    entity_name: str | None = None,
    entity_type: str | None = None,
    limit: int = 50,
) -> dict:
    ensure_schema(conn)
    conn.row_factory = sqlite3.Row
    where_parts = ["profile=?"]
    params: list = [profile]
    if entity_name:
        where_parts.append("name LIKE ? COLLATE NOCASE")
        params.append(f"%{entity_name}%")
    if entity_type:
        where_parts.append("entity_type=?")
        params.append(entity_type)
    where = " AND ".join(where_parts)
    entities = conn.execute(
        f"SELECT * FROM entities WHERE {where} ORDER BY mention_count DESC LIMIT ?",
        params + [limit],
    ).fetchall()
    entity_ids = [e["id"] for e in entities]

    relations = []
    if entity_ids:
        placeholders = ",".join("?" * len(entity_ids))
        relations = conn.execute(
            f"""SELECT * FROM relations
                WHERE source_entity_id IN ({placeholders})
                   OR target_entity_id IN ({placeholders})""",
            entity_ids + entity_ids,
        ).fetchall()

    conn.row_factory = None
    return {
        "entities": [dict(e) for e in entities],
        "relations": [dict(r) for r in relations],
    }


def get_entity_neighbors(
    conn: sqlite3.Connection,
    entity_id: str,
    *,
    max_depth: int = 2,
) -> dict:
    ensure_schema(conn)
    visited: set[str] = set()
    frontier = {entity_id}
    all_entities: list[dict] = []
    all_relations: list[dict] = []

    conn.row_factory = sqlite3.Row
    for _ in range(max_depth):
        if not frontier:
            break
        new_frontier: set[str] = set()
        for eid in frontier:
            if eid in visited:
                continue
            visited.add(eid)
            ent = conn.execute("SELECT * FROM entities WHERE id=?", (eid,)).fetchone()
            if ent:
                all_entities.append(dict(ent))
            rels = conn.execute(
                "SELECT * FROM relations WHERE source_entity_id=? OR target_entity_id=?",
                (eid, eid),
            ).fetchall()
            for r in rels:
                all_relations.append(dict(r))
                other = r["target_entity_id"] if r["source_entity_id"] == eid else r["source_entity_id"]
                if other not in visited:
                    new_frontier.add(other)
        frontier = new_frontier

    conn.row_factory = None
    return {"entities": all_entities, "relations": all_relations}


def format_graph_summary(conn: sqlite3.Connection, profile: str) -> str:
    ensure_schema(conn)
    n_entities = conn.execute(
        "SELECT COUNT(*) FROM entities WHERE profile=?", (profile,)
    ).fetchone()[0]
    n_relations = conn.execute(
        """SELECT COUNT(*) FROM relations r
           JOIN entities e ON r.source_entity_id=e.id WHERE e.profile=?""",
        (profile,),
    ).fetchone()[0]
    communities = detect_communities(conn, profile)
    return (
        f"{n_entities} entities, {n_relations} relations, "
        f"{len(communities)} communities"
    )
