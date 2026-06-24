"""Command modules for TAG CLI. Each module exposes a register(sub) function."""
from __future__ import annotations

import importlib
from types import ModuleType
from typing import List

_MODULE_NAMES = [
    "system",
    "session",
    "import_",
    "routing",
    "memory",
    "queue_dag",
    "swarm",
    "observability",
    "workflow_mgmt",
    "ci_loop",
    "marketplace",
    "agent_tools",
    "prd_clusters",
]


def _load_module(name: str) -> ModuleType | None:
    try:
        return importlib.import_module(f"tag.cmd.{name}")
    except ImportError:
        return None


def get_command_modules() -> List[ModuleType]:
    """Return all available command modules (skipping any that failed to import)."""
    modules = []
    for name in _MODULE_NAMES:
        mod = _load_module(name)
        if mod is not None:
            modules.append(mod)
    return modules


# COMMAND_MODULES is a property-like accessor for backward compat
COMMAND_MODULES = get_command_modules()
