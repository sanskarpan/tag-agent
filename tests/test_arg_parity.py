"""#755/#761: Python set-model/models accept the Go-style positional form.

Parse-only tests (no command execution) so there are no config/render side
effects. The `tag.controller` import is deliberately done *inside* each test
rather than at module scope: test_controller.py re-loads controller.py as a
separate module via spec_from_file_location, and importing the canonical
`tag.controller` at collection time perturbs that dual-load. A function-local
import defers it past collection and keeps the suite order-independent.
"""

from __future__ import annotations


def test_set_model_accepts_positional_and_flags():
    from tag.controller import build_parser

    parser = build_parser()

    # Go-style positional form.
    ns = parser.parse_args(["set-model", "coder", "openrouter/pos-model"])
    assert ns.profile_pos == "coder"
    assert ns.ref_pos == "openrouter/pos-model"

    # Historical flag form still parses.
    ns = parser.parse_args(["set-model", "--profile", "coder", "--ref", "openrouter/flag"])
    assert ns.profile == "coder"
    assert ns.ref == "openrouter/flag"


def test_set_model_no_args_parses_without_argparse_error():
    from tag.controller import build_parser

    # With optional positionals/flags, no-args must PARSE (the usage error is
    # raised in cmd_set_model, not by argparse) rather than exit 2 at parse time.
    ns = build_parser().parse_args(["set-model"])
    assert ns.profile is None and ns.profile_pos is None


def test_models_accepts_positional():
    from tag.controller import build_parser

    ns = build_parser().parse_args(["models", "coder"])
    assert ns.profile_pos == "coder"
    ns = build_parser().parse_args(["models", "--profile", "coder"])
    assert ns.profile == "coder"


def test_route_accepts_positional_task_type():
    from tag.controller import build_parser

    # Go-style positional form.
    ns = build_parser().parse_args(["route", "implementation"])
    assert ns.task_type_pos == "implementation"
    # Historical flag form still parses.
    ns = build_parser().parse_args(["route", "--task-type", "implementation"])
    assert ns.task_type == "implementation"


def test_route_worker_model_flag_alias():
    from tag.controller import build_parser

    # Go spells it --worker-model; Python historically --worker-model-override.
    # Both must land in the same dest so scripts written for either distro work.
    ns = build_parser().parse_args(["route", "impl", "--worker-model", "coder=openrouter/x"])
    assert ns.worker_model_override == ["coder=openrouter/x"]
    ns = build_parser().parse_args(["route", "impl", "--worker-model-override", "coder=openrouter/y"])
    assert ns.worker_model_override == ["coder=openrouter/y"]
