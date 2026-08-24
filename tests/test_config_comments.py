"""#746/#754: config edits preserve comments (ISSUE-018)."""

from __future__ import annotations

from tag.core.config import update_config


def test_update_config_preserves_comments(tmp_path):
    p = tmp_path / "tag.yaml"
    p.write_text(
        "# top-of-file docs\n"
        "profiles:\n"
        "  # the coder profile\n"
        "  coder:\n"
        "    config:\n"
        "      model:\n"
        "        provider: openrouter\n"
        "        default: qwen/qwen3-coder  # inline\n"
        "defaults:\n"
        "  master_profile: coder\n"
    )

    def mutate(cfg):
        cfg["profiles"]["coder"]["config"]["model"]["default"] = "openrouter/new-x"

    update_config(p, mutate)
    out = p.read_text()
    for want in ("# top-of-file docs", "# the coder profile", "# inline"):
        assert want in out, f"comment {want!r} was stripped:\n{out}"
    assert "openrouter/new-x" in out, f"edit not applied:\n{out}"
