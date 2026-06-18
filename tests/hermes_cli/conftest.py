"""Fixtures for hermes_cli integration tests.

These tests require the `hermes_cli` package and its dependencies to be
installed. When they are absent the entire suite is skipped automatically.
"""
import os
import pytest


def pytest_configure(config):
    """Skip all tests in this directory if hermes packages are absent."""
    try:
        import hermes_constants  # noqa: F401
    except ImportError:
        pass  # individual tests will skip via the fixture


@pytest.fixture
def _isolate_hermes_home(tmp_path, monkeypatch):
    """Set HERMES_HOME to a throw-away temp directory for test isolation."""
    try:
        import hermes_constants
    except ImportError:
        pytest.skip("hermes_constants not installed — hermes_cli tests require the hermes package")

    hermes_home = tmp_path / "hermes_home"
    hermes_home.mkdir()
    monkeypatch.setenv("HERMES_HOME", str(hermes_home))
    # Also patch the module-level constant if present
    if hasattr(hermes_constants, "HERMES_HOME"):
        monkeypatch.setattr(hermes_constants, "HERMES_HOME", hermes_home)
    yield hermes_home

