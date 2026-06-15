"""PRD-034: Secret Scanning (tag security scan).

Combines Shannon entropy detection (>4.5 bits over 20-char windows) with
a named-pattern library of ~15 known credential formats. NEVER logs
matched plaintext values — only file path, line number, and pattern name.
"""
from __future__ import annotations

import math
import re
import sqlite3
from pathlib import Path
from typing import Iterator

# ---------------------------------------------------------------------------
# Pattern library — name → compiled regex (no capturing groups on value)
# ---------------------------------------------------------------------------
_PATTERNS: list[tuple[str, re.Pattern]] = [
    ("anthropic_api_key",      re.compile(r'sk-ant-api0[0-9]-[A-Za-z0-9_\-]{80,}')),
    ("openai_api_key",         re.compile(r'sk-[A-Za-z0-9]{48}')),
    ("openai_org",             re.compile(r'org-[A-Za-z0-9]{24}')),
    ("aws_access_key",         re.compile(r'AKIA[0-9A-Z]{16}')),
    ("aws_secret_key",         re.compile(r'(?i)aws.{0,20}secret.{0,20}["\']?([A-Za-z0-9/+]{40})')),
    ("github_pat_classic",     re.compile(r'ghp_[A-Za-z0-9]{36}')),
    ("github_pat_fine",        re.compile(r'github_pat_[A-Za-z0-9_]{82}')),
    ("github_oauth",           re.compile(r'gho_[A-Za-z0-9]{36}')),
    ("npm_access_token",       re.compile(r'npm_[A-Za-z0-9]{36}')),
    ("stripe_secret",          re.compile(r'sk_live_[A-Za-z0-9]{24,}')),
    ("stripe_restricted",      re.compile(r'rk_live_[A-Za-z0-9]{24,}')),
    ("twilio_account_sid",     re.compile(r'AC[a-f0-9]{32}')),
    ("twilio_auth_token",      re.compile(r'SK[a-f0-9]{32}')),
    ("google_api_key",         re.compile(r'AIza[0-9A-Za-z\-_]{35}')),
    ("slack_token",            re.compile(r'xox[baprs]-[0-9A-Za-z\-]+')),
    ("heroku_api_key",         re.compile(r'[hH]eroku.{0,20}[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}')),
    ("generic_private_key",    re.compile(r'-----BEGIN (?:RSA |EC |DSA |OPENSSH )?PRIVATE KEY')),
    ("jwt_token",              re.compile(r'eyJ[A-Za-z0-9_\-]+\.eyJ[A-Za-z0-9_\-]+\.[A-Za-z0-9_\-]+')),
]

# Files/dirs that are always skipped
_SKIP_DIRS = {".git", "node_modules", "__pycache__", ".venv", ".qa-venv312",
              "venv", ".mypy_cache", ".pytest_cache", "dist", "build"}
_SKIP_EXTS = {".png", ".jpg", ".jpeg", ".gif", ".ico", ".svg", ".woff",
              ".ttf", ".eot", ".mp4", ".mov", ".zip", ".tar", ".gz",
              ".pyc", ".pyo", ".so", ".dylib", ".dll", ".exe"}
_MAX_FILE_BYTES = 1_000_000  # 1 MB


def _shannon_entropy(s: str) -> float:
    """Bits-per-character Shannon entropy of *s*."""
    if not s:
        return 0.0
    freq: dict[str, int] = {}
    for ch in s:
        freq[ch] = freq.get(ch, 0) + 1
    n = len(s)
    return -sum((c / n) * math.log2(c / n) for c in freq.values())


def _high_entropy_windows(line: str, window: int = 20, threshold: float = 4.5) -> list[str]:
    """Return distinct high-entropy substrings in *line*."""
    hits: list[str] = []
    for i in range(len(line) - window + 1):
        chunk = line[i:i + window]
        if _shannon_entropy(chunk) > threshold:
            hits.append(chunk)
    # Deduplicate overlapping windows by content prefix
    seen: set[str] = set()
    result: list[str] = []
    for h in hits:
        key = h[:8]
        if key not in seen:
            seen.add(key)
            result.append(h)
    return result


# ---------------------------------------------------------------------------
# Public finding dataclass
# ---------------------------------------------------------------------------

class Finding:
    __slots__ = ("file", "line_no", "pattern_name", "is_entropy")

    def __init__(self, file: Path, line_no: int, pattern_name: str, is_entropy: bool = False):
        self.file = file
        self.line_no = line_no
        self.pattern_name = pattern_name
        self.is_entropy = is_entropy

    def __repr__(self) -> str:
        tag = "[entropy]" if self.is_entropy else f"[{self.pattern_name}]"
        return f"{self.file}:{self.line_no} {tag}"


def scan_text(content: str, file: Path) -> list[Finding]:
    """Scan *content* for secrets. Returns Finding list; never logs values."""
    findings: list[Finding] = []
    for line_no, line in enumerate(content.splitlines(), 1):
        # Named patterns
        for name, pattern in _PATTERNS:
            if pattern.search(line):
                findings.append(Finding(file, line_no, name))
                break  # one finding per line for named patterns

        # Entropy detection (only if no named pattern matched already)
        if not findings or findings[-1].line_no != line_no:
            if _high_entropy_windows(line):
                findings.append(Finding(file, line_no, "high_entropy", is_entropy=True))
    return findings


def scan_file(path: Path) -> list[Finding]:
    """Scan a single file. Skips binary, oversized, or unreadable files."""
    if path.suffix.lower() in _SKIP_EXTS:
        return []
    try:
        size = path.stat().st_size
    except OSError:
        return []
    if size > _MAX_FILE_BYTES:
        return []
    try:
        content = path.read_text(encoding="utf-8", errors="replace")
    except OSError:
        return []
    return scan_text(content, path)


def scan_directory(root: Path, *, max_files: int = 2000) -> Iterator[Finding]:
    """Walk *root* recursively, yielding findings. Skips git, node_modules etc."""
    count = 0
    for dirpath, dirnames, filenames in (root).walk() if hasattr(root, "walk") else _walk(root):
        # Prune skip dirs in-place
        dirnames[:] = [d for d in dirnames if d not in _SKIP_DIRS]
        for fname in filenames:
            if count >= max_files:
                return
            fpath = Path(dirpath) / fname
            for finding in scan_file(fpath):
                yield finding
            count += 1


def _walk(root: Path):
    """Fallback os.walk for Python < 3.12."""
    import os
    for dp, dns, fns in os.walk(root):
        yield Path(dp), dns, fns


# ---------------------------------------------------------------------------
# SQLite audit log (optional — does NOT store matched values)
# ---------------------------------------------------------------------------

def ensure_schema(conn: sqlite3.Connection) -> None:
    conn.executescript("""
        CREATE TABLE IF NOT EXISTS security_scans (
          id          TEXT PRIMARY KEY,
          scanned_path TEXT NOT NULL,
          finding_count INTEGER NOT NULL DEFAULT 0,
          status      TEXT NOT NULL DEFAULT 'ok',
          created_at  TEXT NOT NULL
        );
        CREATE TABLE IF NOT EXISTS security_findings (
          id          TEXT PRIMARY KEY,
          scan_id     TEXT NOT NULL,
          file_path   TEXT NOT NULL,
          line_no     INTEGER NOT NULL,
          pattern_name TEXT NOT NULL,
          is_entropy  INTEGER NOT NULL DEFAULT 0,
          created_at  TEXT NOT NULL,
          FOREIGN KEY(scan_id) REFERENCES security_scans(id)
        );
        CREATE INDEX IF NOT EXISTS idx_sf_scan ON security_findings(scan_id);
    """)
    conn.commit()


def record_scan(conn: sqlite3.Connection, scanned_path: str, findings: list[Finding]) -> str:
    import uuid, datetime
    ensure_schema(conn)
    scan_id = uuid.uuid4().hex[:12]
    now = datetime.datetime.now(datetime.timezone.utc).isoformat()
    status = "clean" if not findings else "secrets_found"
    conn.execute(
        "INSERT INTO security_scans(id, scanned_path, finding_count, status, created_at) VALUES(?,?,?,?,?)",
        (scan_id, scanned_path, len(findings), status, now),
    )
    for f in findings:
        conn.execute(
            "INSERT INTO security_findings(id, scan_id, file_path, line_no, pattern_name, is_entropy, created_at) "
            "VALUES(?,?,?,?,?,?,?)",
            (uuid.uuid4().hex[:12], scan_id, str(f.file), f.line_no, f.pattern_name, int(f.is_entropy), now),
        )
    conn.commit()
    return scan_id
