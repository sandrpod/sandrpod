"""load_egress_policy: file, stdin, inline JSON, and the failure it must not hide."""

import json

import pytest
from click.testing import CliRunner

from sandrpod_cli.main import load_egress_policy


def test_file(tmp_path):
    p = tmp_path / "policy.json"
    p.write_text(json.dumps({"allowed_domains": ["example.com"]}))
    assert load_egress_policy(str(p)) == {"allowed_domains": ["example.com"]}


def test_inline_json():
    assert load_egress_policy('{"allowed_domains":["a.example"]}') == {"allowed_domains": ["a.example"]}


def test_stdin(monkeypatch):
    monkeypatch.setattr("sys.stdin", __import__("io").StringIO('{"strict_allowlist": true}'))
    assert load_egress_policy("-") == {"strict_allowlist": True}


def test_none_means_unconfigured():
    # None (no flag) is "no policy"; {} is a policy that allows nothing.
    assert load_egress_policy(None) is None


def test_bad_json_exits_nonzero():
    with pytest.raises(SystemExit) as exc:
        load_egress_policy("{not json")
    assert exc.value.code == 1


def test_non_object_exits_nonzero(tmp_path):
    p = tmp_path / "policy.json"
    p.write_text('["example.com"]')
    with pytest.raises(SystemExit) as exc:
        load_egress_policy(str(p))
    assert exc.value.code == 1


def test_cli_is_exposed():
    from sandrpod_cli.main import cli

    out = CliRunner().invoke(cli, ["egress", "--help"]).output
    for cmd in ("get", "set", "clear"):
        assert cmd in out
