"""PRD-046: Per-span USD cost attribution.

Provides pricing lookup and cost computation for LLM model usage.
Pricing data is loaded from YAML files with user-override support.
"""

from __future__ import annotations

import fnmatch
from dataclasses import dataclass, field
from pathlib import Path
from typing import Optional

# ---------------------------------------------------------------------------
# Hardcoded fallback pricing (used when PyYAML is unavailable).
# Prices are USD per 1M tokens as of mid-2025.
# ---------------------------------------------------------------------------
_FALLBACK_PRICING: dict[str, dict] = {
    "claude-opus-4-5": {
        "input_usd_per_1m": 15.0,
        "output_usd_per_1m": 75.0,
        "cache_read_multiplier": 0.1,
        "batch_multiplier": 0.5,
    },
    "claude-sonnet-4-5": {
        "input_usd_per_1m": 3.0,
        "output_usd_per_1m": 15.0,
        "cache_read_multiplier": 0.1,
        "batch_multiplier": 0.5,
    },
    "claude-haiku-3-5": {
        "input_usd_per_1m": 0.8,
        "output_usd_per_1m": 4.0,
        "cache_read_multiplier": 0.1,
        "batch_multiplier": 0.5,
    },
    "claude-opus-4": {
        "input_usd_per_1m": 15.0,
        "output_usd_per_1m": 75.0,
        "cache_read_multiplier": 0.1,
        "batch_multiplier": 0.5,
    },
    "claude-sonnet-4": {
        "input_usd_per_1m": 3.0,
        "output_usd_per_1m": 15.0,
        "cache_read_multiplier": 0.1,
        "batch_multiplier": 0.5,
    },
    "claude-sonnet-4-6": {
        "input_usd_per_1m": 3.0,
        "output_usd_per_1m": 15.0,
        "cache_read_multiplier": 0.1,
        "batch_multiplier": 0.5,
    },
    "claude-haiku-3": {
        "input_usd_per_1m": 0.25,
        "output_usd_per_1m": 1.25,
        "cache_read_multiplier": 0.1,
        "batch_multiplier": 0.5,
    },
    "gpt-4o": {
        "input_usd_per_1m": 2.5,
        "output_usd_per_1m": 10.0,
        "cache_read_multiplier": 0.1,
        "batch_multiplier": 0.5,
    },
    "gpt-4o-mini": {
        "input_usd_per_1m": 0.15,
        "output_usd_per_1m": 0.6,
        "cache_read_multiplier": 0.1,
        "batch_multiplier": 0.5,
    },
    "gpt-4-turbo": {
        "input_usd_per_1m": 10.0,
        "output_usd_per_1m": 30.0,
        "cache_read_multiplier": 0.1,
        "batch_multiplier": 0.5,
    },
    "gpt-3.5-turbo": {
        "input_usd_per_1m": 0.5,
        "output_usd_per_1m": 1.5,
        "cache_read_multiplier": 0.1,
        "batch_multiplier": 0.5,
    },
    "gemini-1.5-pro": {
        "input_usd_per_1m": 3.5,
        "output_usd_per_1m": 10.5,
        "cache_read_multiplier": 0.1,
        "batch_multiplier": 0.5,
    },
    "gemini-1.5-flash": {
        "input_usd_per_1m": 0.075,
        "output_usd_per_1m": 0.3,
        "cache_read_multiplier": 0.1,
        "batch_multiplier": 0.5,
    },
}

# Module-level singleton for the loaded pricing table.
_pricing_table: Optional[dict[str, "PricingEntry"]] = None

# Bundled asset path.
_BUNDLED_PRICING_PATH = Path(__file__).parent / "assets" / "pricing.yaml"
_USER_PRICING_PATH = Path.home() / ".tag" / "pricing.yaml"


@dataclass
class PricingEntry:
    """Pricing configuration for a single model.

    Attributes:
        model_id: Canonical model identifier (may include glob wildcards,
            e.g. ``"claude-*"``).
        input_usd_per_1m: Cost in USD per 1 million input tokens.
        output_usd_per_1m: Cost in USD per 1 million output tokens.
        cache_read_multiplier: Multiplier applied to the input rate when
            tokens are served from the prompt cache (default 0.1 = 90% off).
        batch_multiplier: Multiplier applied to both rates when the request
            is submitted via the batch API (default 0.5 = 50% off).
    """

    model_id: str
    input_usd_per_1m: float
    output_usd_per_1m: float
    cache_read_multiplier: float = 0.1
    batch_multiplier: float = 0.5


# ---------------------------------------------------------------------------
# YAML loading helpers
# ---------------------------------------------------------------------------

def _try_load_yaml(path: Path) -> Optional[dict]:
    """Attempt to load a YAML file, returning None on any failure."""
    if not path.exists():
        return None
    try:
        import yaml  # type: ignore[import-untyped]
        with path.open("r", encoding="utf-8") as fh:
            data = yaml.safe_load(fh)
        if isinstance(data, dict):
            return data
        return None
    except ImportError:
        return None
    except Exception:  # noqa: BLE001
        return None


def _parse_pricing_dict(raw: dict) -> dict[str, PricingEntry]:
    """Convert a raw dict (from YAML or hardcoded) into PricingEntry objects.

    Supports two YAML structures:

    List format (preferred)::

        models:
          - model_id: claude-opus-4
            input_usd_per_1m: 15.0
            output_usd_per_1m: 75.0

    Dict format::

        models:
          claude-opus-4:
            input_usd_per_1m: 15.0
            output_usd_per_1m: 75.0

    Flat dicts (keyed directly by model_id) are also accepted.
    """
    entries: dict[str, PricingEntry] = {}
    models_section = raw.get("models", raw)

    # Handle list format: [{model_id: ..., input_usd_per_1m: ...}, ...]
    if isinstance(models_section, list):
        for item in models_section:
            if not isinstance(item, dict):
                continue
            model_id = item.get("model_id")
            if not model_id:
                continue
            try:
                entries[str(model_id)] = PricingEntry(
                    model_id=str(model_id),
                    input_usd_per_1m=float(item["input_usd_per_1m"]),
                    output_usd_per_1m=float(item["output_usd_per_1m"]),
                    cache_read_multiplier=float(
                        item.get("cache_read_multiplier", 0.1)
                    ),
                    batch_multiplier=float(item.get("batch_multiplier", 0.5)),
                )
            except (KeyError, ValueError, TypeError):
                continue
        return entries

    # Handle dict format: {model_id: {input_usd_per_1m: ...}, ...}
    if not isinstance(models_section, dict):
        return entries
    for model_id, cfg in models_section.items():
        if not isinstance(cfg, dict):
            continue
        try:
            entries[str(model_id)] = PricingEntry(
                model_id=str(model_id),
                input_usd_per_1m=float(cfg["input_usd_per_1m"]),
                output_usd_per_1m=float(cfg["output_usd_per_1m"]),
                cache_read_multiplier=float(
                    cfg.get("cache_read_multiplier", 0.1)
                ),
                batch_multiplier=float(cfg.get("batch_multiplier", 0.5)),
            )
        except (KeyError, ValueError, TypeError):
            # Skip malformed entries rather than crashing.
            continue
    return entries


# ---------------------------------------------------------------------------
# Public API
# ---------------------------------------------------------------------------

def load_pricing_table(path: Optional[str | Path] = None) -> dict[str, PricingEntry]:
    """Load the pricing table from YAML files and return it as a dict.

    Load order (later sources override earlier ones):

    1. Bundled ``src/tag/assets/pricing.yaml`` (shipped with the package).
    2. User override at ``~/.tag/pricing.yaml``.
    3. *path* argument (if provided).

    Glob prefix matching is supported: a ``model_id`` of ``"claude-*"`` will
    match any model whose name starts with ``"claude-"``.

    Args:
        path: Optional explicit path to an additional pricing YAML file whose
            entries are merged on top of the bundled + user tables.

    Returns:
        A dict mapping ``model_id`` strings to :class:`PricingEntry` objects.
    """
    # Start with hardcoded fallback so the module works without PyYAML.
    merged_raw: dict[str, dict] = dict(_FALLBACK_PRICING)

    # Layer 1: bundled asset (overrides hardcoded fallback).
    bundled = _try_load_yaml(_BUNDLED_PRICING_PATH)
    if bundled is not None:
        models_section = bundled.get("models", bundled)
        if isinstance(models_section, list):
            for item in models_section:
                if isinstance(item, dict) and item.get("model_id"):
                    mid = str(item["model_id"])
                    merged_raw[mid] = {k: v for k, v in item.items() if k != "model_id"}
        elif isinstance(models_section, dict):
            for k, v in models_section.items():
                merged_raw[str(k)] = v

    # Layer 2: user override (~/.tag/pricing.yaml).
    user = _try_load_yaml(_USER_PRICING_PATH)
    if user is not None:
        models_section = user.get("models", user)
        if isinstance(models_section, list):
            for item in models_section:
                if isinstance(item, dict) and item.get("model_id"):
                    mid = str(item["model_id"])
                    merged_raw[mid] = {k: v for k, v in item.items() if k != "model_id"}
        elif isinstance(models_section, dict):
            for k, v in models_section.items():
                merged_raw[str(k)] = v

    # Layer 3: explicit path argument.
    if path is not None:
        explicit = _try_load_yaml(Path(path))
        if explicit is not None:
            models_section = explicit.get("models", explicit)
            if isinstance(models_section, list):
                for item in models_section:
                    if isinstance(item, dict) and item.get("model_id"):
                        mid = str(item["model_id"])
                        merged_raw[mid] = {k: v for k, v in item.items() if k != "model_id"}
            elif isinstance(models_section, dict):
                for k, v in models_section.items():
                    merged_raw[str(k)] = v

    return _parse_pricing_dict(merged_raw)


def _get_table() -> dict[str, PricingEntry]:
    """Return the cached pricing table, loading it lazily on first access."""
    global _pricing_table
    if _pricing_table is None:
        _pricing_table = load_pricing_table()
    return _pricing_table


def _resolve_entry(model_id: str) -> Optional[PricingEntry]:
    """Look up a model in the pricing table, respecting glob patterns.

    Exact matches take priority over glob matches. Among glob patterns the
    longest (most specific) pattern wins.

    Args:
        model_id: The concrete model identifier to look up.

    Returns:
        The matching :class:`PricingEntry`, or ``None`` if no entry matches.
    """
    table = _get_table()

    # 1. Exact match — fastest path.
    if model_id in table:
        return table[model_id]

    # 2. Glob matching — pick the most specific (longest) matching pattern.
    best: Optional[PricingEntry] = None
    best_len = -1
    for pattern, entry in table.items():
        if fnmatch.fnmatch(model_id, pattern):
            if len(pattern) > best_len:
                best = entry
                best_len = len(pattern)

    return best


def compute_cost(
    model_id: str,
    input_tokens: int,
    output_tokens: int,
    *,
    cache_read: bool = False,
    batch: bool = False,
) -> Optional[float]:
    """Compute the USD cost for a single inference span.

    Returns ``None`` for unknown models rather than raising an exception so
    that callers can safely ignore pricing gaps without crashing.

    The cost formula is::

        input_rate  = entry.input_usd_per_1m
        output_rate = entry.output_usd_per_1m

        if cache_read:
            input_rate *= entry.cache_read_multiplier

        if batch:
            input_rate  *= entry.batch_multiplier
            output_rate *= entry.batch_multiplier

        cost = (input_tokens * input_rate / 1_000_000)
             + (output_tokens * output_rate / 1_000_000)

    Args:
        model_id: The model identifier (e.g. ``"claude-sonnet-4-6"``).
        input_tokens: Number of input tokens consumed.
        output_tokens: Number of output tokens generated.
        cache_read: When ``True``, the input tokens were served from the
            prompt cache and the reduced ``cache_read_multiplier`` rate is
            applied to input costs.
        batch: When ``True``, the request was submitted via the batch API
            and the ``batch_multiplier`` is applied to both rates.

    Returns:
        Estimated cost in USD, or ``None`` if the model is not in the
        pricing table.
    """
    entry = _resolve_entry(model_id)
    if entry is None:
        return None

    # Clamp negative token counts so cost is never negative (PRD-046 hardening).
    input_tokens = max(0, input_tokens)
    output_tokens = max(0, output_tokens)

    input_rate = entry.input_usd_per_1m
    output_rate = entry.output_usd_per_1m

    if cache_read:
        input_rate *= entry.cache_read_multiplier

    if batch:
        input_rate *= entry.batch_multiplier
        output_rate *= entry.batch_multiplier

    return (input_tokens * input_rate / 1_000_000) + (
        output_tokens * output_rate / 1_000_000
    )


def get_pricing_entry(model_id: str) -> Optional[PricingEntry]:
    """Return the :class:`PricingEntry` for *model_id*, or ``None``.

    Glob patterns in the pricing table are honoured; the most specific
    matching pattern wins.

    Args:
        model_id: The concrete model identifier to look up.

    Returns:
        The matching :class:`PricingEntry`, or ``None`` if not found.
    """
    return _resolve_entry(model_id)


def list_all_models() -> list[PricingEntry]:
    """Return all known pricing entries sorted alphabetically by model_id.

    Returns:
        A list of :class:`PricingEntry` objects in ascending ``model_id``
        order.
    """
    table = _get_table()
    return sorted(table.values(), key=lambda e: e.model_id)


def reload_pricing_table() -> None:
    """Clear the in-memory pricing singleton, forcing a reload on next call.

    Useful after modifying ``~/.tag/pricing.yaml`` at runtime or in tests
    that need a fresh state.
    """
    global _pricing_table
    _pricing_table = None
