"""#737: maintenance_state must use the shared (key, value) shape so a
Python-created TAG_HOME stays usable by the Go harness (which declares the
table as (key, value) and whose migrator cannot ALTER TABLE ADD a PRIMARY KEY
column onto Python's legacy (name, last_run) shape).

Imports are function-local to keep the suite order-independent (see the note in
test_arg_parity.py about controller's dual-load).
"""

from __future__ import annotations

import os
import sqlite3
from pathlib import Path
from unittest.mock import patch


def _columns(conn: sqlite3.Connection) -> set[str]:
    return {row[1] for row in conn.execute("PRAGMA table_info(maintenance_state)").fetchall()}


def test_fresh_home_uses_key_value_shape(tmp_path):
    """A freshly provisioned home must create maintenance_state as (key, value)."""
    from tag.core.config import load_config
    from tag.core.db import open_db
    from tag.core.paths import package_root

    with patch.dict(os.environ, {"TAG_HOME": str(tmp_path / "home")}):
        cfg = load_config(package_root() / "config" / "default.yaml")
        conn = open_db(cfg)
    try:
        assert _columns(conn) == {"key", "value"}
    finally:
        conn.close()


def test_legacy_name_last_run_table_is_migrated(tmp_path):
    """A legacy (name, last_run) table is rebuilt to (key, value), row preserved."""
    from tag.core.db import _migrate_maintenance_state

    db = tmp_path / "legacy.sqlite3"
    conn = sqlite3.connect(db)
    conn.execute("CREATE TABLE maintenance_state (name TEXT PRIMARY KEY, last_run TEXT NOT NULL)")
    conn.execute(
        "INSERT INTO maintenance_state(name, last_run) VALUES('prune_spans', '2026-01-01T00:00:00+00:00')"
    )
    conn.commit()

    _migrate_maintenance_state(conn)

    assert _columns(conn) == {"key", "value"}
    row = conn.execute("SELECT key, value FROM maintenance_state WHERE key='prune_spans'").fetchone()
    assert row == ("prune_spans", "2026-01-01T00:00:00+00:00")
    conn.close()


def test_migration_is_idempotent(tmp_path):
    """Running the migration on an already-(key, value) table is a no-op, not an error."""
    from tag.core.db import _migrate_maintenance_state

    db = tmp_path / "canonical.sqlite3"
    conn = sqlite3.connect(db)
    conn.execute("CREATE TABLE maintenance_state (key TEXT PRIMARY KEY, value TEXT NOT NULL)")
    conn.execute("INSERT INTO maintenance_state(key, value) VALUES('prune_spans', 'ts')")
    conn.commit()

    _migrate_maintenance_state(conn)
    _migrate_maintenance_state(conn)

    assert _columns(conn) == {"key", "value"}
    assert conn.execute("SELECT value FROM maintenance_state WHERE key='prune_spans'").fetchone() == ("ts",)
    conn.close()


def test_prune_bookkeeping_roundtrips_through_key_value(tmp_path):
    """The prune path reads/writes via key/value without raising."""
    from tag.core.config import load_config
    from tag.core.db import _prune_old_spans, open_db
    from tag.core.paths import package_root

    with patch.dict(os.environ, {"TAG_HOME": str(tmp_path / "home")}):
        cfg = load_config(package_root() / "config" / "default.yaml")
        conn = open_db(cfg)
    try:
        # force a prune regardless of the interval, then confirm the row landed
        _prune_old_spans(conn, days=0, min_interval_hours=0)
        row = conn.execute("SELECT value FROM maintenance_state WHERE key='prune_spans'").fetchone()
        assert row is not None and row[0]
    finally:
        conn.close()
