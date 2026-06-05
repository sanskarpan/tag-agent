from __future__ import annotations

import importlib.util
from copy import deepcopy
import io
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
MODULE_PATH = ROOT / "src" / "tag" / "controller.py"
SPEC = importlib.util.spec_from_file_location("tag_controller", MODULE_PATH)
assert SPEC and SPEC.loader
TAG = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(TAG)


def load_cfg():
    return TAG.load_config(ROOT / "src" / "tag" / "config" / "default.yaml")


def test_parse_model_ref():
    provider, model = TAG.parse_model_ref("openrouter/deepseek/deepseek-v4-flash")
    assert provider == "openrouter"
    assert model == "deepseek/deepseek-v4-flash"


def test_resolve_route_mixed():
    route = TAG.resolve_route(load_cfg(), "mixed", None, [])
    assert route["master"]["name"] == "orchestrator"
    assert [worker["name"] for worker in route["workers"]] == ["researcher", "coder"]
    assert route["verifier"]["name"] == "reviewer"


def test_apply_route_model_overrides():
    route = TAG.resolve_route(load_cfg(), "mixed", None, [])
    updated = TAG.apply_route_model_overrides(
        route,
        master_model="openai-codex/gpt-5.4",
        verifier_model="openrouter/deepseek/deepseek-v4-pro",
        worker_models=[
            "researcher=openrouter/deepseek/deepseek-v4-flash",
            "coder=openrouter/qwen/qwen3-coder",
        ],
    )
    assert updated["master"]["model"]["default"] == "gpt-5.4"
    assert updated["verifier"]["model"]["default"] == "deepseek/deepseek-v4-pro"
    assert updated["workers"][0]["model"]["default"] == "deepseek/deepseek-v4-flash"
    assert updated["workers"][1]["model"]["default"] == "qwen/qwen3-coder"


def test_case_passed_exact():
    ok, reason = TAG.case_passed({"expected_exact": "bench-ok"}, "bench-ok")
    assert ok is True
    assert "bench-ok" in reason


def test_case_passed_json():
    ok, _ = TAG.case_passed(
        {"expected_json": {"status": "ok", "sum": 42}},
        '{"status":"ok","sum":42}',
    )
    assert ok is True


def test_normalize_chat_output_strips_runtime_noise():
    output = (
        "⚠ tirith security scanner enabled but not available — command scanning will use pattern matching only\n"
        "bench-ok\n"
        "session_id: 123"
    )
    assert TAG.normalize_chat_output(output) == "bench-ok"


def test_case_passed_json_fenced():
    ok, _ = TAG.case_passed(
        {"expected_json": {"status": "ok", "sum": 42}},
        '```json\n{"status":"ok","sum":42}\n```',
    )
    assert ok is True


def test_render_profiles_installs_skin_assets(tmp_path, monkeypatch):
    monkeypatch.setenv("TAG_HOME", str(tmp_path / "tag-home"))
    cfg = deepcopy(load_cfg())
    rendered = TAG.render_profiles(cfg, force=True)
    assert rendered

    researcher_home = (
        Path(TAG.runtime_home(cfg)).resolve() / ".hermes" / "profiles" / "researcher"
    )
    skin_file = researcher_home / "skins" / "tag-control.yaml"
    config_file = researcher_home / "config.yaml"

    assert skin_file.exists()
    assert config_file.exists()
    rendered_cfg = TAG.load_config(config_file)
    assert rendered_cfg["display"]["skin"] == "tag-control"


class FakeTty(io.StringIO):
    def __init__(self, is_tty: bool):
        super().__init__()
        self._is_tty = is_tty

    def isatty(self):
        return self._is_tty


def test_can_launch_interactive_tui_requires_tty():
    assert TAG.can_launch_interactive_tui(FakeTty(True), FakeTty(True), FakeTty(True)) is True
    assert TAG.can_launch_interactive_tui(FakeTty(False), FakeTty(True), FakeTty(True)) is False
    assert TAG.can_launch_interactive_tui(FakeTty(True), FakeTty(False), FakeTty(False)) is False


def test_doctor_prerequisites_reports_missing_checkout(tmp_path, monkeypatch):
    monkeypatch.setenv("TAG_HOME", str(tmp_path / "tag-home"))
    monkeypatch.setenv("TAG_HERMES_ROOT", str(tmp_path / "missing-hermes"))
    report = TAG.doctor_prerequisites(load_cfg())
    assert report["hermes_checkout_exists"] is False
    assert report["bundled_hermes_available"] is True
    assert report["patch_status"] == "checkout-missing"
    assert report["tui_react_installed"] is False
    assert report["tui_vitest_installed"] is False


def test_bundled_hermes_archive_exists():
    assert TAG.bundled_hermes_archive().exists() is True


def test_python_runtime_supported_range():
    assert TAG.python_runtime_supported((3, 11)) is True
    assert TAG.python_runtime_supported((3, 13)) is True
    assert TAG.python_runtime_supported((3, 10)) is False
    assert TAG.python_runtime_supported((3, 14)) is False


def test_hermes_checkout_kind(tmp_path):
    missing = tmp_path / "missing"
    assert TAG.hermes_checkout_kind(missing) == "missing"

    bundled = tmp_path / "bundled"
    bundled.mkdir()
    assert TAG.hermes_checkout_kind(bundled) == "bundled"

    git_root = tmp_path / "git-root"
    (git_root / ".git").mkdir(parents=True)
    assert TAG.hermes_checkout_kind(git_root) == "git"


def test_build_parser_exposes_extended_hermes_surface():
    parser = TAG.build_parser()
    help_text = parser.format_help()
    for command in (
        "status",
        "config",
        "sessions",
        "skills",
        "plugins",
        "tools",
        "mcp",
        "logs",
        "dashboard",
        "memory",
        "completion",
        "prompt-size",
        "update",
    ):
        assert command in help_text
