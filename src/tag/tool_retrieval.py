"""PRD-043: Vector-Based Tool Retrieval (tag mcp-registry index).

Dynamically selects relevant MCP tool subsets using local embeddings +
ChromaDB. Degrades gracefully when optional packages are absent.

Optional deps: chromadb, sentence-transformers
Install: pip install chromadb sentence-transformers
"""
from __future__ import annotations

import json
import sqlite3
from pathlib import Path
from typing import Any

_CHROMA_AVAILABLE = False
_ST_AVAILABLE = False

try:
    import chromadb
    _CHROMA_AVAILABLE = True
except ImportError:
    pass

try:
    from sentence_transformers import SentenceTransformer
    _ST_AVAILABLE = True
except ImportError:
    pass

EMBED_MODEL_NAME = "all-MiniLM-L6-v2"
COLLECTION_NAME = "tag_tools"
DEFAULT_TOP_K = 8
SEMCONV_VERSION = "1.28.0"


def is_available() -> bool:
    """Return True if both chromadb and sentence-transformers are installed."""
    return _CHROMA_AVAILABLE and _ST_AVAILABLE


def get_chroma_client(persist_dir: Path):
    """Return a ChromaDB PersistentClient pointed at *persist_dir*."""
    if not _CHROMA_AVAILABLE:
        raise ImportError("chromadb is required. Install with: pip install chromadb")
    persist_dir.mkdir(parents=True, exist_ok=True)
    return chromadb.PersistentClient(path=str(persist_dir))


def get_embed_model(cache_dir: Path | None = None):
    """Load sentence-transformers embedding model."""
    if not _ST_AVAILABLE:
        raise ImportError(
            "sentence-transformers is required. Install with: pip install sentence-transformers"
        )
    kwargs: dict = {}
    if cache_dir:
        kwargs["cache_folder"] = str(cache_dir)
    return SentenceTransformer(EMBED_MODEL_NAME, **kwargs)


# ---------------------------------------------------------------------------
# SQLite schema for index metadata
# ---------------------------------------------------------------------------

def ensure_schema(conn: sqlite3.Connection) -> None:
    conn.executescript("""
        CREATE TABLE IF NOT EXISTS tool_index_meta (
          id          TEXT PRIMARY KEY DEFAULT 'singleton',
          registry_mtime REAL,
          tool_count  INTEGER NOT NULL DEFAULT 0,
          built_at    TEXT NOT NULL
        );
    """)
    conn.commit()


def _utc_now() -> str:
    import datetime
    return datetime.datetime.now(datetime.timezone.utc).isoformat()


# ---------------------------------------------------------------------------
# Index operations
# ---------------------------------------------------------------------------

def build_index(
    tools: list[dict[str, Any]],
    persist_dir: Path,
    cache_dir: Path | None = None,
    *,
    registry_mtime: float = 0.0,
    conn: sqlite3.Connection | None = None,
) -> int:
    """Build (or rebuild) the tool index. Returns the number of tools indexed.

    *tools* is a list of dicts with at least "name" and "description" keys.
    """
    client = get_chroma_client(persist_dir)
    model = get_embed_model(cache_dir)

    # Delete and recreate collection
    try:
        client.delete_collection(COLLECTION_NAME)
    except Exception:
        pass
    collection = client.create_collection(
        COLLECTION_NAME,
        metadata={"hnsw:space": "cosine"},
    )

    if not tools:
        return 0

    ids = [f"tool_{i}" for i in range(len(tools))]
    documents = [f"{t.get('name', '')} {t.get('description', '')}" for t in tools]
    metadatas = [
        {"name": t.get("name", ""), "server": t.get("server", ""), "schema": json.dumps(t)}
        for t in tools
    ]
    embeddings = model.encode(documents).tolist()

    collection.add(ids=ids, documents=documents, metadatas=metadatas, embeddings=embeddings)

    if conn is not None:
        ensure_schema(conn)
        conn.execute(
            """INSERT INTO tool_index_meta(id, registry_mtime, tool_count, built_at)
               VALUES('singleton',?,?,?)
               ON CONFLICT(id) DO UPDATE SET registry_mtime=excluded.registry_mtime,
               tool_count=excluded.tool_count, built_at=excluded.built_at""",
            (registry_mtime, len(tools), _utc_now()),
        )
        conn.commit()

    return len(tools)


def search_tools(
    query: str,
    persist_dir: Path,
    cache_dir: Path | None = None,
    *,
    top_k: int = DEFAULT_TOP_K,
) -> list[dict[str, Any]]:
    """Return top-K tools relevant to *query*.

    Returns a list of tool dicts (deserialized from stored schema metadata).
    Falls back to an empty list if the index doesn't exist.
    """
    if not is_available():
        return []

    try:
        client = get_chroma_client(persist_dir)
        collection = client.get_collection(COLLECTION_NAME)
        model = get_embed_model(cache_dir)
        embedding = model.encode([query]).tolist()
        results = collection.query(
            query_embeddings=embedding,
            n_results=min(top_k, collection.count()),
            include=["metadatas", "distances"],
        )
        tools = []
        for meta in (results.get("metadatas") or [[]])[0]:
            schema_str = meta.get("schema", "{}")
            try:
                tool = json.loads(schema_str)
            except json.JSONDecodeError:
                tool = {"name": meta.get("name", ""), "description": ""}
            tools.append(tool)
        return tools
    except Exception:
        return []


def is_index_stale(
    conn: sqlite3.Connection,
    registry_mtime: float,
) -> bool:
    """Return True if the tool index needs rebuilding."""
    ensure_schema(conn)
    row = conn.execute(
        "SELECT registry_mtime FROM tool_index_meta WHERE id='singleton'"
    ).fetchone()
    if not row:
        return True  # Never built
    return abs(row[0] - registry_mtime) > 0.5  # 0.5s tolerance


def get_index_stats(conn: sqlite3.Connection) -> dict:
    ensure_schema(conn)
    row = conn.execute(
        "SELECT registry_mtime, tool_count, built_at FROM tool_index_meta WHERE id='singleton'"
    ).fetchone()
    if not row:
        return {"built": False, "tool_count": 0}
    return {
        "built": True,
        "registry_mtime": row[0],
        "tool_count": row[1],
        "built_at": row[2],
        "available": is_available(),
    }


# ---------------------------------------------------------------------------
# Simple fallback retrieval (keyword-based, no deps)
# ---------------------------------------------------------------------------

def keyword_search_tools(
    query: str,
    tools: list[dict[str, Any]],
    *,
    top_k: int = DEFAULT_TOP_K,
) -> list[dict[str, Any]]:
    """Keyword-based fallback when vector search is unavailable."""
    query_lower = query.lower()
    scored: list[tuple[int, dict]] = []
    for tool in tools:
        name = tool.get("name", "").lower()
        desc = tool.get("description", "").lower()
        text = f"{name} {desc}"
        score = sum(1 for word in query_lower.split() if word in text)
        if score > 0:
            scored.append((score, tool))
    scored.sort(key=lambda x: -x[0])
    return [t for _, t in scored[:top_k]]
