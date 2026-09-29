"""Rendering tests for the bundled Python CLI (cli/pilldrops); no server needed.
Kept out of tests/contract, whose conftest skips everything when the published CLI binary is absent.
"""

import importlib.util
import io
import pathlib
import sys
from contextlib import redirect_stdout

import pytest

CLI_PATH = pathlib.Path(__file__).resolve().parents[2] / "cli" / "pilldrops"


@pytest.fixture(scope="module")
def cli():
    """Import cli/pilldrops; it has no .py suffix, so tell the loader it is source."""
    spec = importlib.util.spec_from_loader(
        "pilldrops_cli",
        importlib.machinery.SourceFileLoader("pilldrops_cli", str(CLI_PATH)),
    )
    mod = importlib.util.module_from_spec(spec)
    sys.modules["pilldrops_cli"] = mod
    spec.loader.exec_module(mod)
    return mod


def render_logs(cli, body, monkeypatch):
    monkeypatch.setattr(cli, "api", lambda *a, **k: body)
    args = type("Args", (), {"id": "sb-1"})()
    buf = io.StringIO()
    with redirect_stdout(buf):
        cli.cmd_sandbox_logs(args)
    return buf.getvalue()


# The logs endpoint returns {"lines": [...], "note": "..."}, not a list of exec records.
def test_logs_renders_lines_without_crashing(cli, monkeypatch):
    out = render_logs(cli, {"lines": ["APPLOG-1", "APPLOG-2"], "note": ""}, monkeypatch)
    assert "APPLOG-1" in out
    assert "APPLOG-2" in out


def test_logs_shows_the_note_when_there_are_no_lines(cli, monkeypatch):
    note = "no app log yet — it captures the server started via `--start`"
    out = render_logs(cli, {"lines": [], "note": note}, monkeypatch)
    assert "no app log yet" in out


def test_logs_falls_back_when_there_is_no_note(cli, monkeypatch):
    out = render_logs(cli, {"lines": [], "note": ""}, monkeypatch)
    assert "No logs" in out


def test_logs_tolerates_a_bare_list(cli, monkeypatch):
    # Older servers answer with a bare array.
    out = render_logs(cli, ["line-a", "line-b"], monkeypatch)
    assert "line-a" in out


def test_logs_tolerates_an_empty_body(cli, monkeypatch):
    # A paused sandbox answers with an empty body.
    assert render_logs(cli, None, monkeypatch) != ""
