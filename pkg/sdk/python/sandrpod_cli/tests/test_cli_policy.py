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


def test_merge_egress_flags_build_a_policy():
    from sandrpod_cli.main import merge_egress

    assert merge_egress(None, ("a.example",), ()) == {"allowed_domains": ["a.example"]}
    assert merge_egress(None, (), ("b.example",)) == {"denied_domains": ["b.example"]}


def test_merge_egress_composes_with_a_file():
    from sandrpod_cli.main import merge_egress

    base = {"allowed_domains": ["a.example"], "denied_domain_reasons": {"x": "because"}}
    out = merge_egress(base, ("b.example", "a.example"), ("c.example",))
    assert out["allowed_domains"] == ["a.example", "b.example"]  # no duplicate
    assert out["denied_domains"] == ["c.example"]
    assert out["denied_domain_reasons"] == {"x": "because"}      # untouched
    assert base["allowed_domains"] == ["a.example"]              # input not mutated


def test_merge_egress_none_stays_none():
    from sandrpod_cli.main import merge_egress

    # No flags and no file means "no policy" — not an empty one.
    assert merge_egress(None, (), ()) is None


def test_egress_set_requires_something():
    from click.testing import CliRunner
    from sandrpod_cli.main import cli

    out = CliRunner().invoke(cli, ["egress", "set", "s1"])
    assert out.exit_code == 1
    assert "at least one" in out.output
