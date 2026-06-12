# PRD-021: Repo-Map / Workspace Context

**Status:** Proposed
**Priority:** P1
**Estimated Effort:** L (2 sprints, ~4 weeks)
**Affects:** new `src/tag/workspace.py`, `src/tag/controller.py` (new `cmd_workspace_*` commands, `hermes_env()` injection), `src/tag/config/default.yaml` (new `workspace:` config block)

---

## 1. Overview

Large codebases overflow the context window of any profile. TAG's `context.py` manages per-session token usage, but there is no mechanism to give an agent a navigable map of the repository it is working in. The agent either receives nothing (and hallucinates structure) or the user manually pastes file lists (brittle, stale).

This PRD introduces **workspace indexing**: a background-cacheable, PageRank-weighted map of the codebase that is automatically injected into profile context at the start of each `tag run` or `tag chat` step. The map is token-budget-constrained so it never displaces the task payload, and it is personalized to the most recent conversation turns and goal text so the highest-relevance files surface first.

The design is directly inspired by Aider's repo-map (Gauthier 2023), adapted for TAG's profile-centric multi-agent architecture:

- A file-dependency graph is built from import/call/inherit edges.
- NetworkX PageRank with a personalization vector biased toward recently-mentioned files produces a ranked list.
- The ranked list is rendered as a compact text block (`file: symbol1, symbol2, …`) and trimmed to a configurable `--map-tokens` budget.
- The map is cached as `TAG_HOME/workspace.json`; only files whose `mtime` has changed since last index require re-parsing.

---

## 2. Goals

1. **Token-budget-respecting map**: The workspace map consumes at most `workspace.map_tokens` tokens (default 2048) of the profile's context window, leaving the remainder for task history and goal text.
2. **Automatic relevance personalization**: PageRank personalization is seeded with files mentioned by name or symbol in the last 3 conversation turns and in the current goal/task text, so the map front-loads what the agent is most likely to need.
3. **Incremental mtime-based invalidation**: Re-indexing only re-parses files modified since the last run, keeping cold-start penalty below 30 s for a 10 k-file repository and warm re-index below 500 ms when fewer than 50 files changed.
4. **Language-agnostic symbol extraction**: tree-sitter grammars cover Python, JavaScript, TypeScript, Go, and Rust; a `ctags` subprocess fallback handles C, C++, Java, Ruby, and everything else; plain filename-only entries are emitted when neither tool is available so the map is never empty.
5. **Configurable per-profile injection**: Each profile can independently enable/disable map injection (`workspace.auto_inject: true/false`), override `map_tokens`, and add extra exclude patterns, without affecting other profiles.
6. **Blocked-patterns enforcement on map output**: The same `blocked_patterns` rules (`.env`, `*.key`, `*secret*`, etc.) that gate tool execution also filter the workspace map, so secrets-containing filenames and symbols are never injected into the LLM context.
7. **Zero-mandatory-dependency install path**: `networkx` is a soft dependency pulled in on first `tag workspace index`; tree-sitter and ctags are both optional. TAG must remain importable and all existing commands must remain functional when workspace deps are absent.

---

## 3. Non-Goals

- **Full file content indexing**: The map contains file paths and exported symbol names only — not source text, docstrings, or type signatures. Semantic/embedding-based code search is a separate feature.
- **Semantic code search / vector retrieval**: Nearest-neighbour lookup over embeddings is out of scope for this PRD (future: PRD-022).
- **IDE or LSP integration**: Language Server Protocol hover/go-to-definition is covered by a future desktop PRD (PRD-007 extension). This PRD targets CLI-only usage.
- **Remote repository indexing**: Only the local working tree (`cwd` or `workspace.root`) is indexed. Cloud-hosted repos (GitHub, GitLab) are not fetched.
- **Real-time file-watcher daemon**: The map is refreshed lazily at session start and on explicit `tag workspace index`. A persistent inotify/FSEvents watcher is not in scope.
- **Per-symbol call-graph depth > 2**: Import/call edges are tracked one level deep (A imports B). Full transitive closure of call chains is not computed.

---

## 4. User Stories

**US-01 — Auto-inject for coder profile**
As a developer using `tag run --profile coder "add pagination to the API"`, I want the coder agent to automatically receive a compact map of which files exist in the repo and what they export, so it can write correct import paths and avoid re-implementing existing utilities, without me having to paste a file listing.

**US-02 — Manual refresh after big refactor**
As a developer who just moved 40 files from `src/old/` to `src/new/` during a refactor, I want to run `tag workspace index --force` to rebuild the entire index from scratch rather than relying on a stale cache, and see a summary like "indexed 312 files, 4,821 symbols in 8.3 s".

**US-03 — Customise token budget per project**
As a developer working on a monorepo with 25 k files, I want to set `workspace.map_tokens: 4096` in my project-level `cli-config.yaml` so the coder profile gets a more complete map, while keeping the default 2048 for the researcher profile which doesn't need code structure.

**US-04 — Inspect the current map before a run**
As a developer debugging why the agent keeps opening the wrong file, I want to run `tag workspace show --profile coder` and see exactly the compact text block that would be injected into the coder's context for the current directory, so I can verify the most relevant files are ranked first.

**US-05 — Exclude generated directories**
As a developer whose repo contains a `dist/` folder with 8 k minified JS files, I want to run `tag workspace index --exclude 'dist,node_modules,.venv,__pycache__'` (or configure `workspace.exclude_patterns` in `cli-config.yaml`) so those directories are never indexed and do not pollute the map with noise.

**US-06 — Check index health in doctor**
As a developer running `tag doctor`, I want to see a workspace section showing whether the index exists, when it was last built, how many files it covers, and whether `networkx` is installed, so I can diagnose map-injection issues without reading logs.

**US-07 — Disable map injection for a research profile**
As a developer whose researcher profile does web searches and never touches local files, I want to set `workspace.auto_inject: false` under the `researcher` profile config so that 2048 tokens of irrelevant file listings are not wasted on every research step.

---

## 5. Proposed CLI Surface

All subcommands live under the new `tag workspace` group.

### 5.1 `tag workspace index`

```
tag workspace index [--map-tokens INT] [--exclude PATTERNS] [--force] [--root DIR]
```

Build or update the workspace index.

| Flag | Default | Description |
|---|---|---|
| `--map-tokens INT` | 2048 | Target token budget for the rendered map. Stored in cache metadata and used as default for all subsequent `show` calls unless overridden per call. |
| `--exclude PATTERNS` | `node_modules,.venv,__pycache__,dist,.git` | Comma-separated glob patterns. Directories matching any pattern are skipped entirely. Files matching any pattern are excluded from symbol extraction. |
| `--force` | false | Re-parse all files regardless of mtime. Equivalent to deleting the cache and reindexing. |
| `--root DIR` | `cwd` | Root of the workspace to index. Defaults to the current working directory. Stored in the cache. |

Output (Rich-formatted):

```
Indexing workspace: /Users/alice/myproject
  Parsing: [###########---------] 55% (1,234 / 2,241 files)
  Done: 2,241 files, 18,492 symbols in 12.4 s
  Cache: ~/.tag/workspace.json (updated)
```

Exit codes: `0` success, `1` networkx not installed (with install hint), `2` root not found.

### 5.2 `tag workspace show`

```
tag workspace show [--profile PROFILE] [--map-tokens INT] [--query TEXT]
```

Print the map block that would be injected into the given profile's context right now.

| Flag | Default | Description |
|---|---|---|
| `--profile PROFILE` | default profile | Which profile's personalisation context to use (reads last 3 turns of that profile's session for the personalization seed). |
| `--map-tokens INT` | from index metadata | Override token budget for this preview only. |
| `--query TEXT` | (none) | Treat TEXT as an additional personalization seed (simulates what happens if the task goal contains these words). |

Output:

```
Workspace map for profile 'coder' (847 / 2048 tokens used):

src/tag/controller.py: main, cmd_run, cmd_chat, hermes_env, tag_home
src/tag/workspace.py: build_index, load_index, render_map, GraphBuilder
src/tag/context.py: get_context_size, format_context_bar, summarize_context
src/tag/config/default.yaml  [config]
tests/test_workspace.py: test_pagerank, test_token_budget
… (14 more files, not shown — increase --map-tokens to include)
```

### 5.3 `tag workspace stats`

```
tag workspace stats
```

Show summary statistics about the current index.

Output:

```
Workspace index: /Users/alice/myproject
  Root:          /Users/alice/myproject
  Generated at:  2026-06-12T14:23:11Z  (47 minutes ago)
  Files indexed: 2,241
  Symbols:       18,492
  Excluded:      node_modules, .venv, __pycache__, dist
  Cache path:    ~/.tag/workspace.json
  networkx:      installed (3.3)
  tree-sitter:   installed (0.21.3)
  ctags:         found at /usr/bin/ctags
```

Exit code `1` with message if no index exists yet.

### 5.4 `tag workspace clear`

```
tag workspace clear [--yes]
```

Delete the workspace cache file. Prompts for confirmation unless `--yes` is passed.

Output: `Workspace index cleared.`

---

## 6. Functional Requirements

**FR-01 — tree-sitter language detection**
The indexer shall use `tree-sitter` grammars to extract exported symbol names from Python (`.py`), JavaScript (`.js`, `.mjs`), TypeScript (`.ts`, `.tsx`), Go (`.go`), and Rust (`.rs`) files. For Python: all top-level `def`, `class`, and `__all__` entries. For JS/TS: `export function`, `export class`, `export const`, `export default`. For Go: all exported identifiers (uppercase first letter at package scope). For Rust: all `pub fn`, `pub struct`, `pub enum`, `pub trait` in the crate root and `lib.rs`.

**FR-02 — ctags fallback**
When `tree-sitter` is unavailable or a file's extension is not covered by a bundled grammar, the indexer shall attempt `ctags --output-format=json -f - <file>` as a subprocess. If ctags is also absent, the file is recorded with an empty symbol list. A filename-only entry is always emitted so the file still appears in the map.

**FR-03 — File-dependency graph construction**
The indexer shall build a directed graph `G` where each node is a repository-relative file path. Edges are added as follows:

- **Python**: Parse `import X`, `from X import Y` statements; resolve X to a repository-relative path using the same logic as `importlib.util.find_spec` scoped to the workspace root. Add edge `(current_file, resolved_path)` weighted by number of import occurrences in the file.
- **JS/TS**: Parse `import … from 'X'` and `require('X')`; resolve relative paths relative to the importing file; skip `node_modules` paths.
- **Go**: Parse `import "X"` blocks; resolve module-path prefixes to workspace-relative directories using `go.mod` if present.
- **Rust**: Parse `mod X;` and `use X::Y;`; resolve to relative file paths under `src/`.
- **All other languages**: No edges are added (the file appears as an isolated node).

Edge weight = number of times the import appears in the importing file (default 1 if count is not extractable).

**FR-04 — PageRank calculation**
The indexer shall compute PageRank scores using `networkx.pagerank(G, alpha=0.85, weight='weight', max_iter=100, tol=1e-6)`. The result is a `{file_path: score}` dict stored in the cache under each file entry's `pagerank_score` field.

**FR-05 — Personalization vector construction**
At map-render time (not at index time), a personalization vector `p` is constructed:

1. Extract all tokens from: (a) the last 3 turns of the active profile's session history (via `hermes sessions export`), and (b) the current goal/task text if provided.
2. For each file in `G`, compute a raw affinity score = number of tokens from step 1 that match either the file's basename (without extension), any of its recorded symbol names, or any component of its directory path.
3. Normalise the raw affinity scores to sum to 1.0 to form the personalization dict `p`. If `p` is all-zeros (no matches), fall back to `None` (uniform PageRank, no personalization).
4. Compute personalised PageRank: `networkx.pagerank(G, alpha=0.85, weight='weight', personalization=p, max_iter=100, tol=1e-6)`.

The personalised scores are used only for sorting the rendered map; they are **not** written back to the cache (the cache always stores baseline PageRank).

**FR-06 — mtime-based incremental invalidation**
On each `tag workspace index` call (without `--force`), the indexer shall:

1. Load the existing `workspace.json` cache.
2. Walk the filesystem under `workspace.root` (respecting `exclude_patterns`).
3. For each file, compare `os.stat(file).st_mtime` against the cached `mtime`. Files whose mtime matches the cache are skipped (symbols and edges are reused). Files with a changed or absent mtime are re-parsed.
4. Files present in the cache but absent from the filesystem are removed from the graph.
5. After incremental update, PageRank is recomputed over the full graph (not just changed nodes) since edge weights may have changed.

**FR-07 — Token budget enforcement**
The map renderer shall enforce the `map_tokens` budget using the following trimming algorithm:

1. Sort files by personalised PageRank score descending.
2. For each file in rank order, format a candidate line: `{rel_path}: {sym1}, {sym2}, … ` (symbols sorted alphabetically, up to 8 symbols per file; if more exist, append `+N more`).
3. Estimate token count for the candidate line using `len(line.split()) * 1.3` (a conservative whitespace-tokenisation heuristic; replaced by `tiktoken` if installed).
4. Accumulate lines until adding the next line would exceed `map_tokens`. Stop.
5. Prepend a single header line: `# Workspace map ({n_included} of {n_total} files)\n`.
6. If zero files fit within the budget (e.g. `map_tokens` is very small), emit only the header with a note: `# (map_tokens too small to show any files — increase workspace.map_tokens)`.
7. The final map string is returned as-is for injection.

**FR-08 — blocked_patterns filtering**
Before any file is added to the map output (step 2 of FR-07), the indexer shall check its repository-relative path against the active profile's `blocked_patterns` list (the same list used by the tool gateway). Files matching any pattern are silently excluded from the map. Additionally, the following patterns are always blocked regardless of configuration: `.env`, `.env.*`, `*.key`, `*.pem`, `*.p12`, `*.pfx`, `*secret*`, `*credential*`, `*password*`, `*token*` (case-insensitive glob match on basename).

**FR-09 — Auto-injection into profile context**
When `workspace.auto_inject` is `true` for a profile (see FR-12), `cmd_run` and `cmd_chat` shall:

1. Call `workspace.render_map(profile=profile_name, goal=goal_text)` before launching the Hermes subprocess.
2. Write the map string to a temporary file `TAG_HOME/workspace_map_{profile}.txt`.
3. Pass the path via environment variable `TAG_WORKSPACE_MAP_FILE` to the Hermes subprocess. Hermes reads this file and prepends its contents to the system prompt before the first turn (implementation on Hermes side is outside this PRD's scope — this PRD defines the contract).
4. If `workspace.json` does not exist and `auto_inject` is `true`, log a one-time warning: `No workspace index found. Run 'tag workspace index' to enable workspace context injection.` and proceed without injection (not an error).

**FR-10 — workspace.auto_inject config option**
The `workspace` config block shall be supported at both the global level (under the `defaults:` key in `cli-config.yaml`) and the per-profile level (under `profiles.{name}.config.workspace:`). Per-profile settings take precedence over global settings. Fields:

```yaml
workspace:
  auto_inject: true          # bool, default false
  map_tokens: 2048           # int, default 2048
  exclude_patterns:          # list of globs, merged with CLI --exclude
    - node_modules
    - .venv
    - __pycache__
    - dist
  root: .                    # path relative to TAG_HOME or absolute; default cwd
```

**FR-11 — exclude patterns**
Exclude patterns are evaluated as glob patterns against each path component (not the full path). A directory is excluded if any component matches. A file is excluded if its basename matches or if any of its parent components matches. Patterns passed via `--exclude` on the CLI are merged with `workspace.exclude_patterns` from config; the union is applied.

**FR-12 — Cache storage format**
The workspace cache shall be stored at `TAG_HOME/workspace.json` with the following schema:

```json
{
  "schema_version": 1,
  "generated_at": "2026-06-12T14:23:11Z",
  "root": "/Users/alice/myproject",
  "map_tokens": 2048,
  "exclude_patterns": ["node_modules", ".venv"],
  "files": {
    "src/tag/controller.py": {
      "mtime": 1749734591.234,
      "symbols": ["main", "cmd_run", "cmd_chat", "hermes_env"],
      "pagerank_score": 0.004821,
      "edges_to": ["src/tag/context.py", "src/tag/tui_output.py"]
    }
  }
}
```

On schema version mismatch (future migrations), the cache is silently discarded and rebuilt from scratch.

**FR-13 — `tag doctor` integration**
`cmd_doctor` shall include a `workspace` section with the following checks:

| Check | Pass condition | Warn condition | Fail condition |
|---|---|---|---|
| Index exists | `workspace.json` present and < 24 h old | `workspace.json` present but > 24 h old | `workspace.json` absent |
| networkx installed | `import networkx` succeeds | — | ImportError |
| tree-sitter available | `import tree_sitter` succeeds | — | ImportError (not fail; ctags or filename-only fallback) |
| ctags available | `shutil.which('ctags')` returns a path | — | not found (not fail; filename-only fallback) |
| Blocked files clean | 0 files in index match hardcoded sensitive patterns | >0 files match | — |

**FR-14 — File count cap**
To bound memory and index time, the indexer shall skip files beyond a configurable `workspace.max_files` limit (default 50 000). When the cap is reached, a warning is emitted listing how many files were skipped and suggesting adding exclude patterns.

**FR-15 — Graceful degradation when networkx absent**
If `networkx` is not installed, `tag workspace index` shall print an error with install instructions and exit code 1. All other `tag` commands shall continue to work normally. `auto_inject` with a missing index shall silently skip injection (FR-09).

---

## 7. Non-Functional Requirements

**NFR-01 — Indexing time**: Full index build shall complete in under 30 s for a 10 000-file Python repository on a 2020-era laptop (M1 MacBook Pro equivalent). Measured from `tag workspace index --force` invocation to cache write completion.

**NFR-02 — Warm re-index time**: Incremental re-index (fewer than 50 changed files in a 10 000-file repo) shall complete in under 500 ms.

**NFR-03 — Map render time**: `render_map()` — including personalisation vector construction and PageRank computation — shall complete in under 500 ms for a 10 000-node graph.

**NFR-04 — Memory usage**: Peak RSS during full index of a 10 000-file repo shall not exceed 256 MB.

**NFR-05 — Cache file size**: `workspace.json` for a 10 000-file repo with an average of 8 symbols per file shall not exceed 5 MB.

**NFR-06 — No subprocess blocking on inject**: The `TAG_WORKSPACE_MAP_FILE` injection path in `cmd_run`/`cmd_chat` shall add no more than 50 ms of latency to session startup (map is pre-rendered and written to disk; the Hermes subprocess reads it on its own).

**NFR-07 — Idempotent indexing**: Running `tag workspace index` twice in a row with no file changes shall produce a byte-for-byte identical `workspace.json` (modulo `generated_at` timestamp).

**NFR-08 — Thread safety**: `GraphBuilder.build()` shall be safe to call from a ThreadPoolExecutor with up to 8 workers parsing files concurrently. The shared graph object is assembled only after all workers complete (no concurrent writes to the NetworkX graph).

---

## 8. Technical Design

### 8.1 New files

**`src/tag/workspace.py`** — primary module. Contains:

- `GraphBuilder` — walks the filesystem, dispatches to language-specific parsers, builds the NetworkX `DiGraph`.
- `SymbolExtractor` — thin abstraction over tree-sitter grammars and ctags fallback.
- `PageRankRanker` — wraps `networkx.pagerank`, constructs personalisation vectors from session text.
- `TokenBudgetTrimmer` — converts ranked file list to a token-bounded map string.
- `load_index(tag_home: Path) -> dict` — loads and validates `workspace.json`.
- `save_index(index: dict, tag_home: Path) -> None` — atomic write (write to `.tmp`, rename).
- `build_index(root: Path, tag_home: Path, *, force: bool, exclude: list[str], map_tokens: int) -> dict` — orchestrates the full pipeline.
- `render_map(tag_home: Path, *, profile: str | None, goal: str | None, map_tokens: int | None, hermes_bin: Path | None) -> str` — loads index, runs personalised PageRank, calls trimmer, returns map string.

### 8.2 Symbol extraction

```
SymbolExtractor.extract(file_path: Path) -> tuple[list[str], list[str]]
    # returns (symbols, import_targets)
```

Resolution order:
1. Check `_TREE_SITTER_GRAMMARS` dict keyed by file suffix. If grammar available, parse AST and walk to extract symbols and import strings.
2. Else if `shutil.which('ctags')` found, run `ctags --output-format=json --fields=+n -f - <file>`. Parse JSON lines, extract `name` field for `kind` in `{function, class, method, interface, type, struct, enum, trait, const, variable}` with `scope` == file-level.
3. Else return `([], [])` (no symbols; file still nodes in graph).

Tree-sitter grammar availability is checked once at module import time and cached in `_TREE_SITTER_AVAILABLE: bool`. The grammar objects for each language are loaded lazily on first use for that language and cached in a module-level dict.

### 8.3 Graph construction

```python
class GraphBuilder:
    def __init__(self, root: Path, exclude_patterns: list[str], max_files: int):
        self.root = root
        self.exclude = exclude_patterns
        self.max_files = max_files
        self.G = nx.DiGraph()
        self._extractor = SymbolExtractor()

    def build(self, existing_index: dict | None = None, *, force: bool = False) -> dict:
        """
        Walk root, parse files (incrementally if existing_index provided and not force),
        populate self.G, compute PageRank, return updated index dict.
        """
```

Walk algorithm:
1. `os.walk(root, topdown=True)` with `dirs[:] = [d for d in dirs if not _excluded(d)]` to prune excluded subtrees in-place (avoids descending into `node_modules`).
2. For each non-excluded file: check mtime against cache; skip if unchanged (reuse cached symbols and edges); else extract symbols and imports.
3. After walk: resolve import strings to file paths (best-effort: try `root / path_from_import_string`, try with `.py`/`/index.js`/etc. appended). Add `G.add_edge(source, target, weight=count)` for each resolved import.
4. Add all files as nodes even if they have no edges (`G.add_node(rel_path, symbols=[...], mtime=...)`).
5. Compute `nx.pagerank(G, alpha=0.85, weight='weight', max_iter=100, tol=1e-6)`. Store scores on nodes.

### 8.4 PageRank personalization algorithm

```python
class PageRankRanker:
    def rank(
        self,
        G: nx.DiGraph,
        session_turns: list[str],
        goal: str | None,
    ) -> dict[str, float]:
        # 1. Tokenize seed text
        seed_text = " ".join(session_turns[-3:])
        if goal:
            seed_text += " " + goal
        tokens = set(re.findall(r'\b\w{3,}\b', seed_text.lower()))

        # 2. Compute raw affinity per node
        affinity: dict[str, float] = {}
        for node in G.nodes:
            node_tokens: set[str] = set()
            # file basename without extension
            node_tokens.add(Path(node).stem.lower())
            # directory components
            node_tokens.update(p.lower() for p in Path(node).parts[:-1])
            # exported symbols
            node_tokens.update(s.lower() for s in G.nodes[node].get('symbols', []))
            affinity[node] = len(tokens & node_tokens)

        # 3. Normalise to personalization dict
        total = sum(affinity.values())
        if total == 0:
            return nx.pagerank(G, alpha=0.85, weight='weight', max_iter=100, tol=1e-6)
        p = {node: affinity[node] / total for node in G.nodes}

        # 4. Personalised PageRank
        return nx.pagerank(
            G, alpha=0.85, weight='weight',
            personalization=p, max_iter=100, tol=1e-6
        )
```

Notes:
- `alpha=0.85` matches Aider's original implementation and the seminal Brin/Page paper default.
- The personalisation vector is re-computed on every `render_map` call because session turns change per invocation.
- If the graph has no edges (isolated nodes only), PageRank degenerates to uniform distribution; personalisation still biases the sorted order via the affinity scores.

### 8.5 Token budget trimming algorithm

```python
class TokenBudgetTrimmer:
    MAX_SYMBOLS_PER_FILE = 8

    def __init__(self, map_tokens: int):
        self.map_tokens = map_tokens
        self._count = self._tiktoken_count if _TIKTOKEN_AVAILABLE else self._heuristic_count

    @staticmethod
    def _heuristic_count(text: str) -> int:
        # Conservative: count whitespace-separated tokens * 1.3
        return int(len(text.split()) * 1.3)

    @staticmethod
    def _tiktoken_count(text: str) -> int:
        import tiktoken
        enc = tiktoken.get_encoding("cl100k_base")
        return len(enc.encode(text))

    def trim(
        self,
        ranked_files: list[tuple[str, float]],  # (rel_path, score), sorted desc
        symbols_map: dict[str, list[str]],        # rel_path -> symbol list
    ) -> str:
        header = ""  # filled in at end
        lines: list[str] = []
        used_tokens = 0
        n_total = len(ranked_files)

        for rel_path, _score in ranked_files:
            syms = sorted(symbols_map.get(rel_path, []))
            if len(syms) > self.MAX_SYMBOLS_PER_FILE:
                extra = len(syms) - self.MAX_SYMBOLS_PER_FILE
                syms = syms[:self.MAX_SYMBOLS_PER_FILE] + [f"+{extra} more"]
            if syms:
                line = f"{rel_path}: {', '.join(syms)}\n"
            else:
                line = f"{rel_path}\n"

            cost = self._count(line)
            if used_tokens + cost > self.map_tokens:
                break
            lines.append(line)
            used_tokens += cost

        n_included = len(lines)
        header = f"# Workspace map ({n_included} of {n_total} files)\n"
        header_cost = self._count(header)

        # If header alone exceeds budget, still emit it (header is always shown)
        if not lines:
            return header + "# (map_tokens too small to show any files — increase workspace.map_tokens)\n"

        return header + "".join(lines)
```

Notes on the trimming algorithm:
- The header is added **after** the file loop so `n_included` is known.
- Header token cost is not subtracted from `map_tokens` to keep the budget simple (the header is always small, < 20 tokens).
- `tiktoken` is used when installed (accurate cl100k counts for GPT-4/o-series models); the `*1.3` heuristic errs on the side of slight over-estimation, meaning the actual injected token count may be slightly under budget — this is intentional (conservative, safe).
- The sorted order from `PageRankRanker.rank` is respected strictly; no secondary sort is applied within this function.

### 8.6 Integration with `cmd_run` / `cmd_chat`

In `controller.py`, the injection hook is added in `hermes_env()`:

```python
def hermes_env(profile_cfg: dict, tag_home: Path, ...) -> dict[str, str]:
    env = {... existing env vars ...}

    ws_cfg = profile_cfg.get("config", {}).get("workspace", {})
    if ws_cfg.get("auto_inject", False):
        try:
            from tag.workspace import render_map, load_index
            index = load_index(tag_home)
            if index:
                map_str = render_map(
                    tag_home=tag_home,
                    profile=profile_name,
                    goal=goal_text,
                    map_tokens=ws_cfg.get("map_tokens", 2048),
                    hermes_bin=hermes_bin_path,
                )
                map_file = tag_home / f"workspace_map_{profile_name}.txt"
                map_file.write_text(map_str, encoding="utf-8")
                env["TAG_WORKSPACE_MAP_FILE"] = str(map_file)
        except Exception as exc:
            # Never let workspace errors crash a session
            warnings.warn(f"workspace map injection failed: {exc}")

    return env
```

### 8.7 `cmd_workspace` dispatch

A new top-level argument group `workspace` is registered in the `argparse` setup inside `main()`:

```
tag workspace index   -> cmd_workspace_index(args)
tag workspace show    -> cmd_workspace_show(args)
tag workspace stats   -> cmd_workspace_stats(args)
tag workspace clear   -> cmd_workspace_clear(args)
```

Each `cmd_workspace_*` function lives in `controller.py` (consistent with existing command style) and calls into `workspace.py` for all heavy lifting.

### 8.8 Cache atomicity

`save_index` writes to `TAG_HOME/workspace.json.tmp` then calls `os.replace()` (atomic on POSIX, best-effort atomic on Windows). This prevents a partial write from corrupting the cache if the process is interrupted.

---

## 9. Security Considerations

**SEC-01 — blocked_patterns enforcement**: The `blocked_patterns` list from the active profile config is applied as a filter in `TokenBudgetTrimmer.trim()` before any file path or symbol is written to the map string. This ensures that even if a sensitive file (`.env`, `secrets.py`) is present in the workspace, it is never surfaced in the map injected into the LLM context.

**SEC-02 — Hardcoded sensitive pattern exclusions**: Regardless of user configuration, the following glob patterns are always excluded from the map output (checked against basename, case-insensitive): `.env`, `.env.*`, `*.key`, `*.pem`, `*.p12`, `*.pfx`, `*secret*`, `*credential*`, `*password*`, `*token*`, `*apikey*`, `.netrc`, `*.htpasswd`. These are non-overridable; there is no flag to disable them.

**SEC-03 — Local-only cache**: `workspace.json` is stored under `TAG_HOME` (default `~/.tag/`). It is never transmitted over the network, never included in `tag export`, and never shared via the profile template system (PRD-015). The `tag workspace clear` command provides a user-facing way to delete it.

**SEC-04 — Symbol content only, not file content**: The cache stores symbol names and file paths, not file content, docstrings, or any other potentially sensitive string extracted from source files. A compromised `workspace.json` leaks the structural shape of the codebase but not source text.

**SEC-05 — Subprocess sandboxing for ctags**: The ctags subprocess is invoked with `subprocess.run([...], timeout=10, capture_output=True)`. No shell expansion is used (list-form invocation). The timeout prevents a malformed file from hanging the indexer. ctags output is parsed as JSON lines; unexpected output is silently discarded.

**SEC-06 — No eval / exec on extracted content**: Symbol names are stored as raw strings. They are never passed to `eval()`, `exec()`, `importlib.import_module()`, or any dynamic code execution mechanism. They are treated as opaque text labels throughout the pipeline.

**SEC-07 — Map injection via file, not env var content**: The map string is written to a temp file and the path is passed via `TAG_WORKSPACE_MAP_FILE`. This avoids `ARG_MAX` limits on large maps and prevents map content from appearing in `ps aux` output on the parent process.

**SEC-08 — Cache permissions**: `save_index` creates `workspace.json` with mode `0o600` (owner read/write only) so other users on a shared machine cannot read the codebase structure.

---

## 10. Testing Strategy

### Unit tests (`tests/test_workspace.py`)

**Test group A — PageRank calculation**:
- `test_pagerank_uniform`: A graph of 5 isolated nodes should produce uniform scores (all ~0.2).
- `test_pagerank_hub_gets_higher_score`: A node imported by 4 others should have higher score than leaf nodes.
- `test_pagerank_personalization_seeds_toward_mentioned_file`: When session text contains "controller", `src/tag/controller.py` should rank first.
- `test_pagerank_personalization_fallback_on_no_match`: When session text has no file matches, result equals non-personalised PageRank.
- `test_pagerank_empty_graph`: Empty graph returns empty dict without raising.

**Test group B — Token budget enforcement**:
- `test_trimmer_respects_map_tokens`: A 2048-token budget with a 10 000-file ranked list should produce a map string whose token count is <= 2048.
- `test_trimmer_header_always_present`: Even with `map_tokens=1`, the header line is present.
- `test_trimmer_symbols_truncated_at_8`: A file with 20 symbols shows 8 + "+12 more".
- `test_trimmer_empty_symbols`: A file with no symbols shows just the path, no colon.
- `test_trimmer_heuristic_vs_tiktoken`: Both counting methods produce results within 10% of each other on a sample text (integration check).

**Test group C — mtime invalidation**:
- `test_incremental_skips_unchanged_files`: Build index, then call `build_index` again without modifying files; confirm 0 files re-parsed (tracked via mock).
- `test_incremental_reparsed_on_mtime_change`: Modify mtime of one file, confirm only that file is re-parsed.
- `test_incremental_removes_deleted_files`: Build index with 5 files, delete one, re-index; confirm deleted file absent from new index.
- `test_force_rebuild_ignores_cache`: With `force=True`, all files are re-parsed even if mtime unchanged.

**Test group D — blocked_patterns**:
- `test_sensitive_file_excluded_from_map`: A `.env` file in the repo must not appear in the rendered map string.
- `test_custom_blocked_pattern_respected`: A pattern `"*.secret"` in `blocked_patterns` must exclude `config.secret` from map.

**Test group E — cache atomicity**:
- `test_save_index_atomic`: Simulate SIGKILL mid-write by mocking `os.replace` to raise; confirm original `workspace.json` is unchanged.
- `test_schema_version_mismatch_rebuilds`: A cache with `schema_version: 0` must be discarded and rebuilt.

### Integration tests (`tests/test_workspace_integration.py`)

- `test_index_real_tag_repo`: Run `build_index(root=Path('src/tag'), ...)` on the actual TAG source tree; confirm `controller.py` scores higher than test files (it is imported by nothing but has many edges to helpers).
- `test_end_to_end_render_map`: Build index on TAG source tree, call `render_map()` with `goal="fix context window bug"`, confirm `context.py` appears in top 5 results.
- `test_exclude_pycache_not_in_map`: Build index on TAG source tree; confirm no `__pycache__` paths appear in `workspace.json`.

---

## 11. Acceptance Criteria

**AC-01**: `tag workspace index` on a fresh checkout of the TAG repo completes in under 30 s and writes a `workspace.json` to `~/.tag/`.

**AC-02**: `tag workspace show --profile coder` prints a map block that is non-empty and whose total token count (measured by `_heuristic_count`) does not exceed the configured `map_tokens` value.

**AC-03**: Running `tag workspace index` twice without modifying files produces the same `workspace.json` content (excluding `generated_at`).

**AC-04**: After modifying `src/tag/controller.py`, running `tag workspace index` (no `--force`) re-parses only `controller.py` and updates its entry in the cache; all other files retain their previous `mtime` and `symbols`.

**AC-05**: A file named `.env` present in the workspace root does NOT appear in the output of `tag workspace show`, regardless of `blocked_patterns` configuration.

**AC-06**: `tag workspace stats` reports the correct file count matching `wc -l` of the cache's `files` dict.

**AC-07**: When `workspace.auto_inject: true` is set for the `coder` profile, running `tag run --profile coder "..."` results in the environment variable `TAG_WORKSPACE_MAP_FILE` being set to a valid readable file path containing the map text.

**AC-08**: When `networkx` is not installed, `tag workspace index` exits with code 1 and a human-readable message suggesting `pip install networkx`; no other `tag` subcommand is affected.

**AC-09**: `tag workspace clear` removes `workspace.json` from `TAG_HOME`; subsequent `tag workspace stats` exits with code 1 and the message `No workspace index found`.

**AC-10**: With `--map-tokens 100`, the rendered map contains fewer files than with `--map-tokens 2048`, and the 2048-token map is a superset (same file ordering, more files appended).

**AC-11**: `tag workspace show --query "context window"` ranks `src/tag/context.py` higher than it would rank without the `--query` flag.

**AC-12**: `tag doctor` includes a `workspace` section and shows `pass` for `Index exists` when the cache is less than 24 h old.

---

## 12. Dependencies

### Required at runtime (when workspace feature is used)

| Package | Version constraint | Purpose |
|---|---|---|
| `networkx` | `>=3.0` | DiGraph construction, PageRank computation |

### Optional runtime dependencies

| Package | Version constraint | Purpose | Fallback |
|---|---|---|---|
| `tree-sitter` | `>=0.21` | AST-based symbol extraction for Python/JS/TS/Go/Rust | ctags subprocess |
| `tree-sitter-python` | `>=0.21` | Python grammar for tree-sitter | ctags / filename-only |
| `tree-sitter-javascript` | `>=0.21` | JS grammar | ctags / filename-only |
| `tree-sitter-typescript` | `>=0.21` | TS grammar | ctags / filename-only |
| `tree-sitter-go` | `>=0.21` | Go grammar | ctags / filename-only |
| `tree-sitter-rust` | `>=0.21` | Rust grammar | ctags / filename-only |
| `tiktoken` | `>=0.6` | Accurate token counting for GPT-4/o-series | heuristic `*1.3` counter |

### System tools (optional)

| Tool | Purpose | Fallback |
|---|---|---|
| `ctags` (Universal Ctags preferred) | Symbol extraction for all other languages | filename-only entries |

### Already-available in TAG environment

| Package | Used for |
|---|---|
| `pyyaml` | Parsing `cli-config.yaml` workspace config block |
| `rich` | Progress bar during indexing, styled output in `show`/`stats` |
| `pathlib` | All filesystem operations |

### Pyproject.toml changes

`networkx>=3.0` is added to `dependencies` as an optional extra:

```toml
[project.optional-dependencies]
workspace = [
    "networkx>=3.0",
    "tree-sitter>=0.21",
    "tiktoken>=0.6",
]
```

Users can install with `pip install tag-agent[workspace]`. The base `tag-agent` install continues to work without these packages.

---

## 13. Open Questions

**OQ-01 — Symbol extraction depth**: Should the indexer extract only top-level (module-scope) symbols, or also class methods? Methods add significant noise for large classes (a 50-method class would produce 50 symbols). The current spec caps at 8 symbols per file in the rendered map, but the cache stores all. Should there be a `max_symbols_per_file` config option for the cache as well (to bound cache size)?

**OQ-02 — Map format**: The current spec uses plain text (`file: sym1, sym2`). Alternatives considered:
- JSON: machine-readable, but significantly more tokens for the same information density.
- XML: similar verbosity problem as JSON.
- Aider-style: uses a more elaborate format with class/function nesting (`class Foo: def bar`). More readable for humans, but more complex to generate and higher token cost per file.
Recommendation: keep plain text for now; add a `workspace.map_format: text | aider` config option in a follow-up.

**OQ-03 — Personalization query source**: The current spec uses the last 3 session turns from `hermes sessions export`. This requires a subprocess call to Hermes before each `render_map`. For profiles with no active session (e.g. `tag workspace show` called cold), this returns nothing and personalisation is skipped. Should there be a persistent "recent queries" log in the workspace cache that is updated after each session, so personalisation works even in the first turn of a new session?

**OQ-04 — Cross-profile graph sharing**: Should all profiles share one `workspace.json`, or should each profile have its own `workspace_{profile}.json`? The current spec shares one graph (the dependency graph is objective) but computes personalised ranking per-profile at render time. This seems correct but means a `--map-tokens` set during `tag workspace index` applies globally; per-profile token budgets are applied only at render time. Is this the right split?

**OQ-05 — Monorepo with multiple languages**: In a repo with Python, Go, and TypeScript all present, import resolution across language boundaries is not attempted (FR-03 handles each language independently). For a monorepo with a Python API calling a Go service, the inter-language dependency edge is missing. Should there be a heuristic (e.g., shared directory names as proxy edges)?

**OQ-06 — `tiktoken` model selection**: The heuristic uses `cl100k_base` (GPT-4 tokenizer). TAG profiles can run on many different models (Qwen, DeepSeek, Llama) with different tokenizers. Should the token counter be aware of the active profile's model to pick an accurate tokenizer, or is `cl100k_base` a close-enough universal approximation?

---

## 14. Complexity and Timeline

**Overall complexity: L**

The feature requires a new 400–600 line module (`workspace.py`), changes to `controller.py` (hook injection, 4 new CLI commands), config schema additions, and a comprehensive test suite. NetworkX and tree-sitter are well-understood dependencies with stable APIs, but the incremental invalidation logic and the personalisation vector construction both have edge cases that require careful testing.

### Sprint 1 (2 weeks)

| Week | Tasks |
|---|---|
| Week 1 | Scaffold `src/tag/workspace.py`; implement `GraphBuilder` with Python import parsing only; implement `save_index` / `load_index` with schema validation; implement `cmd_workspace_index` and `cmd_workspace_stats`; unit tests for graph construction and mtime invalidation. |
| Week 2 | Implement `SymbolExtractor` with tree-sitter for Python + ctags fallback; implement `PageRankRanker` with personalisation; implement `TokenBudgetTrimmer`; implement `cmd_workspace_show`; unit tests for PageRank and token budget. |

### Sprint 2 (2 weeks)

| Week | Tasks |
|---|---|
| Week 3 | Add JS/TS/Go/Rust grammars to `SymbolExtractor`; implement `auto_inject` hook in `hermes_env()`; implement `cmd_workspace_clear`; `tag doctor` integration; `blocked_patterns` enforcement; `cli-config.yaml` schema extension. |
| Week 4 | Integration tests on real repos; performance profiling and optimisation to hit NFR-01/02/03; documentation in `docs/prd/`; optional-dependency packaging (`pip install tag-agent[workspace]`); final acceptance criteria verification. |

### Risk register

| Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|
| tree-sitter grammar API changes between 0.21 and 0.22+ | Medium | Medium | Pin grammar packages; use version check guard at import |
| PageRank non-convergence on cyclic graphs | Low | Low | `max_iter=100` + `tol=1e-6` ensures termination; NetworkX raises `nx.PowerIterationFailedConvergence` which we catch and fall back to uniform |
| ctags output format variation between Universal Ctags and Exuberant Ctags | Medium | Low | Detect via `ctags --version`; use `--output-format=json` only for Universal Ctags; for Exuberant, parse tag file format |
| Cache corruption on disk full during write | Low | Medium | Atomic write (write+rename) means old cache survives partial write |
| Personalisation subprocess call to Hermes adds latency | Medium | Medium | Cache last-3-turns text in `TAG_HOME/workspace_context_{profile}.txt` after each session; read from file instead of subprocess |
